use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::AtomicBool;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tauri::{AppHandle, Emitter};

use crate::platform::paths;

const APT_INDEX_REFRESH_RETRY_MARKER: &str = "PINVOU3_APT_INDEX_REFRESH_RETRY";

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
cleanup() {
  rm -f "$stderr_file"
}
trap cleanup EXIT HUP INT TERM

install_deb() {
  apt-get \
    -o Acquire::Retries=3 \
    -o DPkg::Lock::Timeout=60 \
    install -y --reinstall --no-install-recommends -- "$1" \
    2>"$stderr_file"
}

install_deb "$1"
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
install_deb "$1"
retry_status=$?
cat "$stderr_file" >&2
exit "$retry_status"
"#;

pub fn cleanup_stale_backup() {}

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

pub fn check_update_platform_support() -> Result<(), String> {
    Ok(())
}

pub async fn check_for_update_info(
    client: &reqwest::Client,
    current_version: &str,
    manifest_url: &str,
) -> Result<super::super::UpdateInfo, String> {
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
    Ok(super::super::UpdateInfo {
        // 仅严格大于才提示 → 服务器版本 ≤ 本地时天然降级保护
        available: is_newer(&m.version, current_version),
        current_version: current_version.to_string(),
        latest_version: m.version,
        notes: m.notes,
        pub_date: m.pub_date,
        url: m.url,
        sha256: m.sha256.to_lowercase(),
        size: m.size,
        package_md5: String::new(),
        software_id: String::new(),
        sn: String::new(),
        update_type: String::new(),
        platform: "linux".to_string(),
        ota_host: String::new(),
        approval_receipt: String::new(),
        platforms: std::collections::HashMap::new(),
    })
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
    let dir = paths::updates_dir();
    std::fs::create_dir_all(&dir).map_err(|e| format!("创建下载目录失败: {e}"))?;
    let dest = dir.join(format!(
        "pinvou3_{}_{}.deb",
        info.latest_version,
        deb_arch()
    ));
    let expected = info.sha256.to_lowercase();

    if dest.exists() && file_sha256(&dest).as_deref() == Some(expected.as_str()) {
        return Ok(super::super::DownloadUpdateResult::Path(
            dest.to_string_lossy().into_owned(),
        ));
    }

    if let Ok(entries) = std::fs::read_dir(&dir) {
        for e in entries.flatten() {
            if e.path().extension().is_some_and(|x| x == "deb") {
                let _ = std::fs::remove_file(e.path());
            }
        }
    }

    if info.size > 0 {
        if let Some(avail) = available_bytes(&dir) {
            let need = info.size.saturating_add(64 * 1024 * 1024);
            if avail < need {
                return Err(format!(
                    "磁盘空间不足：需约 {} MB，当前可用 {} MB",
                    need / 1_048_576,
                    avail / 1_048_576
                ));
            }
        }
    }

    let client = super::super::update_client_builder(&info.url)?
        .connect_timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("HTTP client 构建失败: {e}"))?;
    let response =
        super::super::wait_download_step(cancel, stall_timeout, client.get(&info.url).send()).await;
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

    let total = if info.size > 0 {
        info.size
    } else {
        resp.content_length().unwrap_or(0)
    };
    let mut file = std::fs::File::create(&dest).map_err(|e| format!("创建文件失败: {e}"))?;
    let mut hasher = Sha256::new();
    let mut downloaded: u64 = 0;
    let mut last_emit: u64 = 0;
    loop {
        let chunk =
            match super::super::wait_download_step(cancel, stall_timeout, resp.chunk()).await {
                Err(super::super::DownloadWaitError::Cancelled) => {
                    drop(file);
                    let _ = std::fs::remove_file(&dest);
                    return Err("已取消下载".to_string());
                }
                Err(super::super::DownloadWaitError::Stalled) => {
                    drop(file);
                    let _ = std::fs::remove_file(&dest);
                    return Err(format!(
                        "下载停滞：超过 {}s 无数据，已中断（网络异常或更新源无响应）",
                        stall_timeout.as_secs()
                    ));
                }
                Err(super::super::DownloadWaitError::Request(error)) => {
                    drop(file);
                    let _ = std::fs::remove_file(&dest);
                    return Err(format!("下载中断: {error}"));
                }
                Ok(None) => break,
                Ok(Some(chunk)) => chunk,
            };
        file.write_all(&chunk)
            .map_err(|e| format!("写盘失败: {e}"))?;
        hasher.update(&chunk);
        downloaded += chunk.len() as u64;
        if downloaded - last_emit >= 262_144 || downloaded == total {
            last_emit = downloaded;
            let _ = app.emit(
                "update:progress",
                serde_json::json!({ "downloaded": downloaded, "total": total }),
            );
        }
    }
    drop(file);

    let actual = crate::platform::encoding::hex_lower(&hasher.finalize());
    if actual != expected {
        let _ = std::fs::remove_file(&dest);
        return Err(format!(
            "sha256 校验失败（期望 {expected} 实际 {actual}），已删除下载文件"
        ));
    }
    Ok(super::super::DownloadUpdateResult::Path(
        dest.to_string_lossy().into_owned(),
    ))
}

pub fn install_update_package(path: &Path) -> Result<(), String> {
    let canon = validate_deb_path(path)?;
    let output = Command::new("pkexec")
        .args(["sh", "-c", APT_INSTALL_SCRIPT, "pinvou3-updater"])
        .arg(&canon)
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
    _info: Option<super::super::UpdateInfo>,
) -> Result<bool, String> {
    let deb_path = deb_path.ok_or_else(|| "缺少 deb 安装包路径".to_string())?;
    install_update_package(Path::new(&deb_path))?;
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

fn file_sha256(path: &Path) -> Option<String> {
    let bytes = std::fs::read(path).ok()?;
    Some(crate::platform::encoding::hex_lower(&Sha256::digest(
        &bytes,
    )))
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
        std::fs::create_dir_all(&bin_dir).unwrap();
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
            .arg("/tmp/pinvou3 package's regression.deb")
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
        assert!(calls[0].contains("/tmp/pinvou3 package's regression.deb"));
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
