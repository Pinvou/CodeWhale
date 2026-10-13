use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::AtomicBool;
use std::sync::{Mutex, MutexGuard};
use std::time::Duration;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tauri::{AppHandle, Emitter};

use crate::platform::paths;

const APT_INDEX_REFRESH_RETRY_MARKER: &str = "PINVOU3_APT_INDEX_REFRESH_RETRY";
const MAX_DOWNLOAD_BYTES: u64 = 4 * 1024 * 1024 * 1024;
const OWNED_PART_PREFIX: &str = ".pinvou3-linux-update-";
const OWNED_PART_SUFFIX: &str = ".part";

/// OTA 覆盖安装只处理 Pinvou 本体及其必需依赖，避免 LibreOffice 等推荐包把
/// 非核心系统依赖带入升级关键路径。首次安装仍由 deb 的 Recommends 保持原行为。
///
/// 软件源索引过期时，apt 常按旧索引请求已被镜像淘汰的具体版本并返回 404。
/// 仅在明确的仓库下载错误下刷新索引并重试一次；不运行 upgrade/dist-upgrade，
/// 也不自动修复整机 broken packages，避免扩大到 Pinvou 之外的软件包。
/// deb 路径通过 sh 的位置参数传入，不拼进脚本文本。
const APT_INSTALL_SCRIPT: &str = r#"
export DEBIAN_FRONTEND=noninteractive
export LC_ALL=C

stderr_file="$(mktemp)" || exit 1
root_deb="$(mktemp /var/tmp/pinvou3-update.XXXXXX.deb)" || exit 1
cleanup() {
  rm -f "$stderr_file"
  rm -f "$root_deb"
}
trap cleanup EXIT HUP INT TERM

source_deb="$1"
expected_size="$2"
expected_sha256="$3"
if [ ! -f "$source_deb" ] || [ -L "$source_deb" ]; then
  echo 'update source is not a regular non-symlink file' >&2
  exit 1
fi
chmod 0600 "$root_deb" || exit 1
cp -- "$source_deb" "$root_deb" || exit 1
actual_size="$(wc -c < "$root_deb" | tr -d '[:space:]')"
actual_sha256="$(sha256sum -- "$root_deb" | awk '{print $1}')"
if [ "$actual_size" != "$expected_size" ] || [ "$actual_sha256" != "$expected_sha256" ]; then
  echo 'root copy does not match approved update asset' >&2
  exit 1
fi

install_deb() {
  apt-get \
    -o Acquire::Retries=3 \
    -o DPkg::Lock::Timeout=60 \
    install -y --reinstall --no-install-recommends -- "$1" \
    2>"$stderr_file"
}

install_deb "$root_deb"
install_status=$?
if [ "$install_status" -eq 0 ]; then
  cat "$stderr_file" >&2
  exit 0
fi

cat "$stderr_file" >&2
if ! grep -Eiq \
  '404[[:space:]]+Not Found|Failed to fetch|Unable to fetch some archives|Hash Sum mismatch|File has unexpected size' \
  "$stderr_file"
then
  exit "$install_status"
fi

echo "PINVOU3_APT_INDEX_REFRESH_RETRY" >&2
apt-get -o Acquire::Retries=3 update || true

: >"$stderr_file"
install_deb "$root_deb"
retry_status=$?
cat "$stderr_file" >&2
exit "$retry_status"
"#;

pub fn cleanup_stale_backup() -> Result<(), String> {
    Ok(())
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct LatestManifest {
    pub version: String,
    #[serde(default)]
    pub notes: String,
    #[serde(default)]
    pub pub_date: String,
    pub url: String,
    pub sha256: String,
    #[serde(default)]
    pub size: u64,
}

#[derive(Debug, Clone)]
struct ApprovedAsset {
    receipt: String,
    current_version: String,
    version: String,
    url: String,
    sha256: String,
    size: u64,
    downloaded: bool,
}

impl ApprovedAsset {
    fn matches_ipc(&self, info: &super::super::UpdateInfo) -> bool {
        !self.receipt.is_empty()
            && self.receipt == info.approval_receipt
            && self.current_version == info.current_version
            && self.version == info.latest_version
            && self.url == info.url
            && self.sha256.eq_ignore_ascii_case(&info.sha256)
            && self.size == info.size
            && info.platform == "linux"
    }

    fn output_name(&self) -> String {
        format!("pinvou3_{}_{}.deb", self.version, deb_arch())
    }
}

static APPROVED_ASSET: Mutex<Option<ApprovedAsset>> = Mutex::new(None);

struct TempDownload {
    path: PathBuf,
    committed: bool,
}

impl Drop for TempDownload {
    fn drop(&mut self) {
        if !self.committed {
            let _ = std::fs::remove_file(&self.path);
        }
    }
}

fn approved_asset_state() -> MutexGuard<'static, Option<ApprovedAsset>> {
    APPROVED_ASSET
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

fn clear_approved_asset() {
    *approved_asset_state() = None;
}

fn approve_manifest_asset(current_version: &str, manifest: &LatestManifest) -> ApprovedAsset {
    let mut state = approved_asset_state();
    if let Some(existing) = state.as_ref() {
        if existing.current_version == current_version
            && existing.version == manifest.version
            && existing.url == manifest.url
            && existing.sha256.eq_ignore_ascii_case(&manifest.sha256)
            && existing.size == manifest.size
        {
            return existing.clone();
        }
    }
    let approved = ApprovedAsset {
        receipt: uuid::Uuid::new_v4().to_string(),
        current_version: current_version.to_string(),
        version: manifest.version.clone(),
        url: manifest.url.clone(),
        sha256: manifest.sha256.to_ascii_lowercase(),
        size: manifest.size,
        downloaded: false,
    };
    *state = Some(approved.clone());
    approved
}

fn approved_asset_from_ipc(
    info: &super::super::UpdateInfo,
    require_downloaded: bool,
) -> Result<ApprovedAsset, String> {
    let state = approved_asset_state();
    let approved = state
        .as_ref()
        .filter(|approved| approved.matches_ipc(info))
        .ok_or_else(|| "Linux 更新批准收据无效或清单资产已变更，请重新检查更新".to_string())?;
    if require_downloaded && !approved.downloaded {
        return Err("Linux 更新资产尚未由本进程完成下载校验".to_string());
    }
    Ok(approved.clone())
}

fn set_downloaded(receipt: &str, downloaded: bool) -> Result<(), String> {
    let mut state = approved_asset_state();
    let approved = state
        .as_mut()
        .filter(|approved| approved.receipt == receipt)
        .ok_or_else(|| "Linux 更新批准状态已失效，请重新检查更新".to_string())?;
    approved.downloaded = downloaded;
    Ok(())
}

pub fn check_update_platform_support() -> Result<(), String> {
    Ok(())
}

pub async fn check_for_update_info(
    client: &reqwest::Client,
    current_version: &str,
    manifest_url: &str,
) -> Result<super::super::UpdateInfo, String> {
    // A new check invalidates the prior browser-visible receipt. Failed,
    // cancelled, and no-update checks must not leave an old asset installable.
    clear_approved_asset();
    let m: LatestManifest = client
        .get(manifest_url)
        .send()
        .await
        .map_err(|e| format!("更新源连接失败: {e}"))?
        .error_for_status()
        .map_err(|e| format!("更新源响应异常: {e}"))?
        .json()
        .await
        .map_err(|e| format!("latest.json 解析失败: {e}"))?;
    validate_manifest_asset(&m)?;
    let available = is_newer(&m.version, current_version);
    let approved = available.then(|| approve_manifest_asset(current_version, &m));
    Ok(super::super::UpdateInfo {
        // 仅严格大于才提示 → 服务器版本 ≤ 本地时天然降级保护
        available,
        current_version: current_version.to_string(),
        latest_version: m.version,
        notes: m.notes,
        pub_date: m.pub_date,
        url: m.url,
        sha256: m.sha256.to_ascii_lowercase(),
        size: m.size,
        package_md5: String::new(),
        software_id: String::new(),
        sn: String::new(),
        update_type: String::new(),
        platform: "linux".to_string(),
        ota_host: String::new(),
        approval_receipt: approved.map_or_else(String::new, |asset| asset.receipt),
        platforms: std::collections::HashMap::new(),
    })
}

fn validate_manifest_asset(manifest: &LatestManifest) -> Result<(), String> {
    if !is_release_version(&manifest.version) {
        return Err("Linux 更新版本号必须是三段数字".to_string());
    }
    if manifest.sha256.len() != 64 || !manifest.sha256.bytes().all(|byte| byte.is_ascii_hexdigit())
    {
        return Err("Linux 更新资产 sha256 必须是 64 位十六进制".to_string());
    }
    if manifest.size == 0 || manifest.size > MAX_DOWNLOAD_BYTES {
        return Err(format!("Linux 更新资产大小无效: {} bytes", manifest.size));
    }
    // Building through the shared factory validates the initial URL and binds
    // its redirect policy; the download path creates the client it will use.
    let _validated_builder = super::super::update_client_builder(&manifest.url)?;
    Ok(())
}

fn is_release_version(version: &str) -> bool {
    let parts = version.split('.').collect::<Vec<_>>();
    parts.len() == 3
        && parts
            .iter()
            .all(|part| !part.is_empty() && part.bytes().all(|byte| byte.is_ascii_digit()))
}

/// Debian 架构名（deb 包文件名后缀），与 dpkg --print-architecture 一致。
fn deb_arch() -> &'static str {
    match std::env::consts::ARCH {
        "aarch64" => "arm64",
        "x86_64" => "amd64",
        other => other,
    }
}

pub async fn download_update_package(
    info: &super::super::UpdateInfo,
    app: AppHandle,
    cancel: &AtomicBool,
    stall_timeout: Duration,
) -> Result<super::super::DownloadUpdateResult, String> {
    check_update_platform_support()?;
    let approved = approved_asset_from_ipc(info, false)?;
    set_downloaded(&approved.receipt, false)?;
    let dir = paths::updates_dir();
    std::fs::create_dir_all(&dir).map_err(|e| format!("创建下载目录失败: {e}"))?;
    let dest = dir.join(approved.output_name());

    if dest
        .try_exists()
        .map_err(|e| format!("检查既有更新文件失败: {e}"))?
    {
        if verified_regular_file(&dest, approved.size, &approved.sha256)? {
            set_downloaded(&approved.receipt, true)?;
            return Ok(super::super::DownloadUpdateResult::Path(
                dest.to_string_lossy().into_owned(),
            ));
        }
        std::fs::remove_file(&dest).map_err(|e| format!("清理损坏的既有更新文件失败: {e}"))?;
    }

    if let Ok(entries) = std::fs::read_dir(&dir) {
        for entry in entries.flatten() {
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if name.starts_with(OWNED_PART_PREFIX) && name.ends_with(OWNED_PART_SUFFIX) {
                let _ = std::fs::remove_file(entry.path());
            }
        }
    }

    if let Some(avail) = available_bytes(&dir) {
        let need = approved.size.saturating_add(64 * 1024 * 1024);
        if avail < need {
            return Err(format!(
                "磁盘空间不足：需约 {} MB，当前可用 {} MB",
                need / 1_048_576,
                avail / 1_048_576
            ));
        }
    }

    let client = super::super::update_client_builder(&approved.url)?
        .connect_timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("HTTP client 构建失败: {e}"))?;
    let response =
        super::super::wait_download_step(cancel, stall_timeout, client.get(&approved.url).send())
            .await;
    let mut resp = match response {
        Ok(response) => response,
        Err(super::super::DownloadWaitError::Cancelled) => return Err("已取消下载".to_string()),
        Err(super::super::DownloadWaitError::Stalled) => {
            return Err(format!(
                "下载响应超时：超过 {}s 未收到响应头",
                stall_timeout.as_secs()
            ));
        }
        Err(super::super::DownloadWaitError::Request(error)) => {
            return Err(format!("下载请求失败: {error}"));
        }
    }
    .error_for_status()
    .map_err(|e| format!("下载响应异常: {e}"))?;

    if let Some(content_length) = resp.content_length() {
        if content_length != approved.size {
            return Err(format!(
                "下载响应大小与清单不一致：期望 {} 实际 {content_length}",
                approved.size
            ));
        }
    }
    let temp_path = dir.join(format!(
        "{OWNED_PART_PREFIX}{}{OWNED_PART_SUFFIX}",
        uuid::Uuid::new_v4().simple()
    ));
    let mut temporary = TempDownload {
        path: temp_path.clone(),
        committed: false,
    };
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temp_path)
        .map_err(|e| format!("创建下载临时文件失败: {e}"))?;
    let mut hasher = Sha256::new();
    let mut downloaded: u64 = 0;
    let mut last_emit: u64 = 0;
    loop {
        let chunk =
            match super::super::wait_download_step(cancel, stall_timeout, resp.chunk()).await {
                Err(super::super::DownloadWaitError::Cancelled) => {
                    return Err("已取消下载".to_string());
                }
                Err(super::super::DownloadWaitError::Stalled) => {
                    return Err(format!(
                        "下载停滞：超过 {}s 无数据，已中断（网络异常或更新源无响应）",
                        stall_timeout.as_secs()
                    ));
                }
                Err(super::super::DownloadWaitError::Request(error)) => {
                    return Err(format!("下载中断: {error}"));
                }
                Ok(None) => break,
                Ok(Some(chunk)) => chunk,
            };
        let next = downloaded.saturating_add(chunk.len() as u64);
        if next > approved.size || next > MAX_DOWNLOAD_BYTES {
            return Err("下载体积超过清单声明或安全上限，已中断".to_string());
        }
        file.write_all(&chunk)
            .map_err(|e| format!("写盘失败: {e}"))?;
        hasher.update(&chunk);
        downloaded = next;
        if downloaded - last_emit >= 262_144 || downloaded == approved.size {
            last_emit = downloaded;
            let _ = app.emit(
                "update:progress",
                serde_json::json!({ "downloaded": downloaded, "total": approved.size }),
            );
        }
    }
    file.sync_all()
        .map_err(|e| format!("同步下载文件失败: {e}"))?;
    drop(file);

    let actual = crate::platform::encoding::hex_lower(&hasher.finalize());
    if downloaded != approved.size {
        return Err(format!(
            "下载大小校验失败：期望 {} 实际 {downloaded}",
            approved.size
        ));
    }
    if actual != approved.sha256 {
        return Err(format!(
            "sha256 校验失败（期望 {} 实际 {actual}），已删除下载文件",
            approved.sha256
        ));
    }
    if dest
        .try_exists()
        .map_err(|e| format!("检查目标更新文件失败: {e}"))?
    {
        if verified_regular_file(&dest, approved.size, &approved.sha256)? {
            return Err("目标更新文件在下载期间已出现，请重新检查更新".to_string());
        }
        return Err("目标更新路径已被非预期文件占用，拒绝覆盖".to_string());
    }
    rename_no_replace(&temp_path, &dest).map_err(|e| format!("提交下载文件失败: {e}"))?;
    temporary.committed = true;
    set_downloaded(&approved.receipt, true)?;
    Ok(super::super::DownloadUpdateResult::Path(
        dest.to_string_lossy().into_owned(),
    ))
}

pub fn install_update_package(
    path: &Path,
    expected_size: u64,
    expected_sha256: &str,
) -> Result<(), String> {
    let canon = validate_deb_path(path)?;
    if !verified_regular_file(&canon, expected_size, expected_sha256)? {
        return Err("Linux 更新安装包大小或 SHA-256 与批准清单不一致".to_string());
    }
    let output = Command::new("pkexec")
        .args(["sh", "-c", APT_INSTALL_SCRIPT, "pinvou3-updater"])
        .arg(&canon)
        .arg(expected_size.to_string())
        .arg(expected_sha256)
        .output()
        .map_err(|e| format!("pkexec 启动失败: {e}"))?;

    if output.status.success() {
        return Ok(());
    }
    let code = output.status.code().unwrap_or(-1);
    Err(match code {
        126 => "用户取消授权".to_string(),
        127 => "未授权或 pkexec 不可用".to_string(),
        _ => {
            let stderr = String::from_utf8_lossy(&output.stderr);
            let tail: Vec<&str> = stderr
                .lines()
                .filter(|line| *line != APT_INDEX_REFRESH_RETRY_MARKER)
                .rev()
                .take(4)
                .collect();
            let tail: Vec<&str> = tail.into_iter().rev().collect();
            format!("安装失败 (exit {code}): {}", tail.join(" / "))
        }
    })
}

pub fn install_downloaded_update(
    deb_path: Option<String>,
    _installer_path: Option<String>,
    info: Option<super::super::UpdateInfo>,
) -> Result<bool, String> {
    let info = info.ok_or_else(|| "缺少 Linux 更新批准信息".to_string())?;
    let approved = approved_asset_from_ipc(&info, true)?;
    let expected_path = paths::updates_dir().join(approved.output_name());
    let supplied_path = deb_path.ok_or_else(|| "缺少 deb 安装包路径".to_string())?;
    if Path::new(&supplied_path) != expected_path {
        return Err("Linux 更新安装路径与后端批准记录不一致".to_string());
    }
    install_update_package(&expected_path, approved.size, &approved.sha256)?;
    Ok(false)
}

pub async fn report_pending_update_result_info(
    _client: &reqwest::Client,
    _current_version: &str,
) -> Result<super::super::PendingUpdateReportResult, String> {
    Ok(super::super::PendingUpdateReportResult {
        had_pending: false,
        reported: false,
        result: String::new(),
        message: "当前平台没有待反馈升级结果".to_string(),
    })
}

fn verified_regular_file(
    path: &Path,
    expected_size: u64,
    expected_sha256: &str,
) -> Result<bool, String> {
    let metadata = std::fs::symlink_metadata(path)
        .map_err(|error| format!("读取 Linux 更新文件元数据失败: {error}"))?;
    if metadata.file_type().is_symlink() || !metadata.file_type().is_file() {
        return Err("Linux 更新路径必须是普通非符号链接文件".to_string());
    }
    if metadata.len() != expected_size {
        return Ok(false);
    }
    let mut file =
        std::fs::File::open(path).map_err(|error| format!("打开 Linux 更新文件失败: {error}"))?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let read = file
            .read(&mut buffer)
            .map_err(|error| format!("读取 Linux 更新文件失败: {error}"))?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(crate::platform::encoding::hex_lower(&hasher.finalize()) == expected_sha256)
}

fn rename_no_replace(source: &Path, destination: &Path) -> std::io::Result<()> {
    use std::ffi::CString;
    use std::os::unix::ffi::OsStrExt;

    let source = CString::new(source.as_os_str().as_bytes())
        .map_err(|_| std::io::Error::from(std::io::ErrorKind::InvalidInput))?;
    let destination = CString::new(destination.as_os_str().as_bytes())
        .map_err(|_| std::io::Error::from(std::io::ErrorKind::InvalidInput))?;
    // SAFETY: both paths are NUL-terminated C strings kept alive for the syscall.
    let result = unsafe {
        libc::renameat2(
            libc::AT_FDCWD,
            source.as_ptr(),
            libc::AT_FDCWD,
            destination.as_ptr(),
            libc::RENAME_NOREPLACE,
        )
    };
    if result == 0 {
        Ok(())
    } else {
        Err(std::io::Error::last_os_error())
    }
}

fn available_bytes(dir: &Path) -> Option<u64> {
    let out = Command::new("df")
        .args(["--output=avail", "-B1"])
        .arg(dir)
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    String::from_utf8_lossy(&out.stdout)
        .lines()
        .nth(1)?
        .trim()
        .parse()
        .ok()
}

fn parse_semver(v: &str) -> Option<(u64, u64, u64)> {
    let mut it = v.trim().trim_start_matches('v').splitn(3, '.');
    let major = it.next()?.parse().ok()?;
    let minor = it.next()?.parse().ok()?;
    let patch = it.next()?.parse().ok()?;
    Some((major, minor, patch))
}

fn is_newer(latest: &str, current: &str) -> bool {
    match (parse_semver(latest), parse_semver(current)) {
        (Some(l), Some(c)) => l > c,
        _ => false,
    }
}

fn validate_deb_path(path: &Path) -> Result<PathBuf, String> {
    let metadata = std::fs::symlink_metadata(path).map_err(|e| format!("deb 文件不存在: {e}"))?;
    if metadata.file_type().is_symlink() || !metadata.file_type().is_file() {
        return Err("非法路径：deb 必须是普通非符号链接文件".to_string());
    }
    let canon = path
        .canonicalize()
        .map_err(|e| format!("deb 文件不存在: {e}"))?;
    let dir = crate::platform::paths::updates_dir()
        .canonicalize()
        .map_err(|e| format!("更新目录不存在: {e}"))?;
    if !canon.starts_with(&dir) {
        return Err("非法路径：deb 必须在更新下载目录内".to_string());
    }
    if canon.extension().is_none_or(|x| x != "deb") {
        return Err("非法路径：只接受 .deb 文件".to_string());
    }
    Ok(canon)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[cfg(unix)]
    fn run_fake_apt_scenario(scenario: &str) -> (std::process::Output, String) {
        use std::os::unix::fs::PermissionsExt;

        let root = std::env::temp_dir().join(format!(
            "pinvou3-apt-recovery-{scenario}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let bin_dir = root.join("bin");
        let fake_apt = bin_dir.join("apt-get");
        let log = root.join("apt.log");
        let state = root.join("apt.state");
        let source_deb = root.join("pinvou3 package's regression.deb");
        let source_bytes = b"approved deb fixture";
        std::fs::create_dir_all(&bin_dir).unwrap();
        std::fs::write(&source_deb, source_bytes).unwrap();
        std::fs::write(
            &fake_apt,
            r#"#!/bin/sh
action=""
for arg in "$@"; do
  case "$arg" in
    install|update)
      action="$arg"
      break
      ;;
  esac
done
printf '%s|%s\n' "$action" "$*" >>"$PINVOU_TEST_APT_LOG"

if [ "$action" = "update" ]; then
  exit 0
fi

attempt=0
if [ -f "$PINVOU_TEST_APT_STATE" ]; then
  attempt="$(cat "$PINVOU_TEST_APT_STATE")"
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" >"$PINVOU_TEST_APT_STATE"

case "$PINVOU_TEST_APT_SCENARIO" in
  success)
    exit 0
    ;;
  stale_then_success)
    if [ "$attempt" -eq 1 ]; then
      echo 'E: Failed to fetch package.deb  404 Not Found' >&2
      exit 100
    fi
    exit 0
    ;;
  non_repository_failure)
    echo 'E: dpkg was interrupted' >&2
    exit 100
    ;;
  retry_failure)
    echo 'E: Failed to fetch package.deb  404 Not Found' >&2
    exit 100
    ;;
esac

exit 2
"#,
        )
        .unwrap();
        std::fs::set_permissions(&fake_apt, std::fs::Permissions::from_mode(0o755)).unwrap();

        let path = format!(
            "{}:{}",
            bin_dir.display(),
            std::env::var("PATH").unwrap_or_default()
        );
        let output = Command::new("sh")
            .args(["-c", APT_INSTALL_SCRIPT, "pinvou3-updater"])
            .arg(&source_deb)
            .arg(source_bytes.len().to_string())
            .arg(crate::platform::encoding::hex_lower(&Sha256::digest(
                source_bytes,
            )))
            .env("PATH", path)
            .env("PINVOU_TEST_APT_SCENARIO", scenario)
            .env("PINVOU_TEST_APT_LOG", &log)
            .env("PINVOU_TEST_APT_STATE", &state)
            .output()
            .unwrap();
        let log_text = std::fs::read_to_string(&log).unwrap();
        std::fs::remove_dir_all(&root).unwrap();
        (output, log_text)
    }

    #[test]
    fn semver_numeric_not_lexicographic() {
        assert!(is_newer("0.10.0", "0.2.0"));
        assert!(is_newer("1.0.0", "0.99.99"));
        assert!(is_newer("0.2.1", "0.2.0"));
    }

    #[test]
    fn semver_equal_or_lower_not_newer() {
        assert!(!is_newer("0.2.0", "0.2.0"));
        assert!(!is_newer("0.1.9", "0.2.0"));
    }

    #[test]
    fn semver_malformed_not_newer() {
        assert!(!is_newer("abc", "0.2.0"));
        assert!(!is_newer("1.2", "0.2.0"));
        assert!(!is_newer("9.9.9", "garbage"));
    }

    #[test]
    fn semver_tolerates_v_prefix_and_spaces() {
        assert!(is_newer("v0.3.0", "0.2.0"));
        assert!(is_newer(" 0.3.0 ", "0.2.0"));
    }

    #[test]
    fn manifest_serde_roundtrip() {
        let json = r#"{
            "version": "0.3.0",
            "notes": "测试",
            "pub_date": "2026-06-10T08:00:00Z",
            "url": "https://pinvou.com/pinvou3/pinvou3_0.3.0_amd64.deb",
            "sha256": "abc123",
            "size": 1024
        }"#;
        let m: LatestManifest = serde_json::from_str(json).unwrap();
        assert_eq!(m.version, "0.3.0");
        assert_eq!(m.size, 1024);
        let back = serde_json::to_string(&m).unwrap();
        let m2: LatestManifest = serde_json::from_str(&back).unwrap();
        assert_eq!(m2.url, m.url);
    }

    #[test]
    fn manifest_optional_fields_default() {
        let m: LatestManifest =
            serde_json::from_str(r#"{"version":"0.3.0","url":"u","sha256":"s"}"#).unwrap();
        assert_eq!(m.notes, "");
        assert_eq!(m.size, 0);
    }

    #[test]
    fn approved_asset_rejects_tampered_ipc_metadata() {
        let approved = ApprovedAsset {
            receipt: "random-receipt".to_string(),
            current_version: "0.10.4".to_string(),
            version: "0.10.5".to_string(),
            url: "https://pinvou.com/pinvou3/pinvou3_0.10.5_amd64.deb".to_string(),
            sha256: "a".repeat(64),
            size: 1234,
            downloaded: true,
        };
        let mut info = super::super::super::UpdateInfo {
            available: true,
            current_version: approved.current_version.clone(),
            latest_version: approved.version.clone(),
            notes: String::new(),
            pub_date: String::new(),
            url: approved.url.clone(),
            sha256: approved.sha256.clone(),
            size: approved.size,
            package_md5: String::new(),
            software_id: String::new(),
            sn: String::new(),
            update_type: String::new(),
            platform: "linux".to_string(),
            ota_host: String::new(),
            approval_receipt: approved.receipt.clone(),
            platforms: std::collections::HashMap::new(),
        };
        assert!(approved.matches_ipc(&info));
        assert_eq!(
            approved.output_name(),
            format!("pinvou3_0.10.5_{}.deb", deb_arch())
        );
        info.url = "https://attacker.invalid/fake.deb".to_string();
        assert!(!approved.matches_ipc(&info));
        info.url = approved.url.clone();
        info.approval_receipt = "forged".to_string();
        assert!(!approved.matches_ipc(&info));
    }

    #[cfg(unix)]
    #[test]
    fn verification_rejects_symlinks_and_checks_exact_size_and_hash() {
        use std::os::unix::fs::symlink;

        let root = std::env::temp_dir().join(format!(
            "pinvou3-linux-verify-{}-{}",
            std::process::id(),
            uuid::Uuid::new_v4().simple()
        ));
        std::fs::create_dir_all(&root).unwrap();
        let regular = root.join("approved.deb");
        let link = root.join("linked.deb");
        let bytes = b"approved package";
        std::fs::write(&regular, bytes).unwrap();
        symlink(&regular, &link).unwrap();
        let sha256 = crate::platform::encoding::hex_lower(&Sha256::digest(bytes));
        assert!(verified_regular_file(&regular, bytes.len() as u64, &sha256).unwrap());
        assert!(!verified_regular_file(&regular, bytes.len() as u64 + 1, &sha256).unwrap());
        assert!(verified_regular_file(&link, bytes.len() as u64, &sha256).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn apt_install_skips_recommends_without_refresh_on_success() {
        let (output, log) = run_fake_apt_scenario("success");
        assert!(output.status.success());
        let calls: Vec<&str> = log.lines().collect();
        assert_eq!(calls.len(), 1);
        assert!(calls[0].starts_with("install|"));
        assert!(calls[0].contains("Acquire::Retries=3"));
        assert!(calls[0].contains("DPkg::Lock::Timeout=60"));
        assert!(calls[0].contains("--no-install-recommends"));
        assert!(calls[0].contains("/var/tmp/pinvou3-update."));
    }

    #[cfg(unix)]
    #[test]
    fn apt_install_refreshes_index_and_retries_once_for_404() {
        let (output, log) = run_fake_apt_scenario("stale_then_success");
        assert!(output.status.success());
        let calls: Vec<&str> = log.lines().collect();
        assert_eq!(calls.len(), 3);
        assert!(calls[0].starts_with("install|"));
        assert!(calls[1].starts_with("update|"));
        assert!(calls[2].starts_with("install|"));
        assert!(String::from_utf8_lossy(&output.stderr).contains(APT_INDEX_REFRESH_RETRY_MARKER));
    }

    #[cfg(unix)]
    #[test]
    fn apt_install_does_not_refresh_for_unrelated_dpkg_failure() {
        let (output, log) = run_fake_apt_scenario("non_repository_failure");
        assert_eq!(output.status.code(), Some(100));
        assert_eq!(log.lines().count(), 1);
        assert!(!String::from_utf8_lossy(&output.stderr).contains(APT_INDEX_REFRESH_RETRY_MARKER));
    }

    #[cfg(unix)]
    #[test]
    fn apt_install_stops_after_one_repository_retry() {
        let (output, log) = run_fake_apt_scenario("retry_failure");
        assert_eq!(output.status.code(), Some(100));
        let calls: Vec<&str> = log.lines().collect();
        assert_eq!(calls.len(), 3);
        assert!(calls[0].starts_with("install|"));
        assert!(calls[1].starts_with("update|"));
        assert!(calls[2].starts_with("install|"));
        assert!(String::from_utf8_lossy(&output.stderr).contains(APT_INDEX_REFRESH_RETRY_MARKER));
    }

    #[test]
    fn validate_deb_path_whitelist() {
        let _g = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let prev = std::env::var("PINVOU3_HOME").ok();
        let root = std::env::temp_dir().join("pinvou3-updater-test");
        // SAFETY: the crate-wide ENV_LOCK serializes this test's process-env mutation.
        unsafe { std::env::set_var("PINVOU3_HOME", &root) };

        let updates = crate::platform::paths::updates_dir();
        std::fs::create_dir_all(&updates).unwrap();
        let good = updates.join("pinvou3_9.9.9_amd64.deb");
        std::fs::write(&good, b"fake").unwrap();
        assert!(validate_deb_path(&good).is_ok());

        let quoted = updates.join("pinvou3_o'clock_amd64.deb");
        std::fs::write(&quoted, b"fake").unwrap();
        assert!(validate_deb_path(&quoted).is_ok());

        let outside = root.join("evil.deb");
        std::fs::write(&outside, b"fake").unwrap();
        assert!(validate_deb_path(&outside).is_err());

        let txt = updates.join("note.txt");
        std::fs::write(&txt, b"x").unwrap();
        assert!(validate_deb_path(&txt).is_err());

        assert!(validate_deb_path(&updates.join("ghost.deb")).is_err());

        let traversal = updates.join("../evil.deb");
        assert!(validate_deb_path(&traversal).is_err());

        let _ = std::fs::remove_dir_all(&root);
        match prev {
            // SAFETY: ENV_LOCK remains held while the original value is restored.
            Some(v) => unsafe { std::env::set_var("PINVOU3_HOME", v) },
            // SAFETY: the same lock serializes restoration by removal.
            None => unsafe { std::env::remove_var("PINVOU3_HOME") },
        }
    }
}
