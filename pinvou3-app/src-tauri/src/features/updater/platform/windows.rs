//! Windows 静态 OTA：latest.json 平台清单 → 直接下载已签名 NSIS/MSI
//! （SHA-256）→ UAC 启动安装器 → 当前进程退出。NSIS 通过 `/R` 由安装器自身
//! 的 RunAsUser 逻辑启动新版；MSI 暂不自动重启应用。
//!
//! Windows 正式安装包仍由独立签名服务器构建并签署；本模块不执行签名，也不做
//! Authenticode 二次阻断校验。更新器只信任固定更新源中的平台清单，并以清单内的
//! SHA-256 校验传输完整性。

use std::ffi::OsString;
use std::io::{Read, Write};
use std::path::{Component, Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::AtomicBool;
use std::sync::{Mutex, MutexGuard};
use std::time::{Duration, Instant, SystemTime};

use base64::Engine as _;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use tauri::{AppHandle, Emitter};

use crate::platform::paths;

const MAX_DOWNLOAD_BYTES: u64 = 4 * 1024 * 1024 * 1024;
const OWNED_PART_PREFIX: &str = ".pinvou3-update-";
const OWNED_PART_SUFFIX: &str = ".part";
const OWNED_INSTALL_PREFIX: &str = ".pinvou3-install-";
const STALE_PART_AGE: Duration = Duration::from_secs(24 * 60 * 60);
const INSTALL_START_TIMEOUT: Duration = Duration::from_secs(120);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum WindowsInstallerKind {
    Msi,
    NsisExe,
}

impl WindowsInstallerKind {
    fn extension(self) -> &'static str {
        match self {
            Self::Msi => "msi",
            Self::NsisExe => "exe",
        }
    }

    fn manifest_format(self) -> &'static str {
        match self {
            Self::Msi => "msi",
            Self::NsisExe => "nsis",
        }
    }

    fn output_name(self, version: &str) -> String {
        match self {
            Self::Msi => format!("pinvou3_{}_x64.msi", safe_version(version)),
            Self::NsisExe => format!("pinvou3_{}_x64-setup.exe", safe_version(version)),
        }
    }
}

#[derive(Debug, Deserialize)]
struct LatestManifest {
    #[serde(default)]
    platforms: std::collections::HashMap<String, super::super::PlatformAsset>,
}

#[derive(Debug, Clone)]
struct ApprovedAsset {
    receipt: String,
    current_version: String,
    version: String,
    url: String,
    sha256: String,
    size: u64,
    kind: WindowsInstallerKind,
    downloaded: bool,
}

impl ApprovedAsset {
    fn same_manifest_asset(
        &self,
        current_version: &str,
        asset: &super::super::PlatformAsset,
        kind: WindowsInstallerKind,
    ) -> bool {
        self.current_version == current_version
            && self.version == asset.version
            && self.url == asset.url
            && self.sha256.eq_ignore_ascii_case(&asset.sha256)
            && self.size == asset.size
            && self.kind == kind
    }

    fn matches_ipc(&self, info: &super::super::UpdateInfo) -> bool {
        !self.receipt.is_empty()
            && self.receipt == info.approval_receipt
            && self.current_version == info.current_version
            && self.version == info.latest_version
            && self.url == info.url
            && self.sha256.eq_ignore_ascii_case(&info.sha256)
            && self.size == info.size
            && self.kind.manifest_format() == info.update_type
            && info.platform == "windows"
    }
}

static APPROVED_ASSET: Mutex<Option<ApprovedAsset>> = Mutex::new(None);

fn approved_asset_state() -> MutexGuard<'static, Option<ApprovedAsset>> {
    APPROVED_ASSET
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

fn approve_manifest_asset(
    current_version: &str,
    asset: &super::super::PlatformAsset,
    kind: WindowsInstallerKind,
) -> ApprovedAsset {
    let mut state = approved_asset_state();
    if let Some(existing) = state.as_ref() {
        if existing.same_manifest_asset(current_version, asset, kind) {
            return existing.clone();
        }
    }
    let approved = ApprovedAsset {
        receipt: uuid::Uuid::new_v4().to_string(),
        current_version: current_version.to_string(),
        version: asset.version.clone(),
        url: asset.url.clone(),
        sha256: asset.sha256.to_ascii_lowercase(),
        size: asset.size,
        kind,
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
        .ok_or_else(|| "Windows 更新批准收据无效或清单资产已变更，请重新检查更新".to_string())?;
    if require_downloaded && !approved.downloaded {
        return Err("Windows 更新资产尚未由本进程完成下载校验".to_string());
    }
    Ok(approved.clone())
}

fn mark_approved_downloaded(receipt: &str) -> Result<(), String> {
    let mut state = approved_asset_state();
    let approved = state
        .as_mut()
        .filter(|approved| approved.receipt == receipt)
        .ok_or_else(|| "Windows 更新批准状态已失效，请重新检查更新".to_string())?;
    approved.downloaded = true;
    Ok(())
}

fn mark_approved_download_pending(receipt: &str) -> Result<(), String> {
    let mut state = approved_asset_state();
    let approved = state
        .as_mut()
        .filter(|approved| approved.receipt == receipt)
        .ok_or_else(|| "Windows 更新批准状态已失效，请重新检查更新".to_string())?;
    approved.downloaded = false;
    Ok(())
}

struct TempDownload {
    path: PathBuf,
    committed: bool,
}

impl TempDownload {
    fn new(path: PathBuf) -> Self {
        Self {
            path,
            committed: false,
        }
    }

    fn commit(&mut self) {
        self.committed = true;
    }
}

impl Drop for TempDownload {
    fn drop(&mut self) {
        if !self.committed {
            let _ = std::fs::remove_file(&self.path);
        }
    }
}

struct HandshakeFiles {
    nonce: String,
    started: PathBuf,
    error: PathBuf,
    pending: PathBuf,
    claimed: PathBuf,
    aborted: PathBuf,
}

impl Drop for HandshakeFiles {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.started);
        let _ = std::fs::remove_file(&self.error);
        let _ = std::fs::remove_dir(&self.pending);
        let _ = std::fs::remove_dir(&self.claimed);
        let _ = std::fs::remove_dir(&self.aborted);
    }
}

struct TrustedSystemTools {
    powershell: PathBuf,
    msiexec: PathBuf,
}

pub fn cleanup_stale_backup() -> Result<(), String> {
    Ok(())
}

fn allow_insecure_asset_url() -> bool {
    let manifest = super::super::manifest_url();
    manifest.starts_with("http://") && super::super::is_loopback_update_url(&manifest)
}

pub fn check_update_platform_support() -> Result<(), String> {
    if std::env::consts::ARCH != "x86_64" {
        return Err(format!(
            "Windows 静态更新当前只支持 x86_64，当前架构为 {}",
            std::env::consts::ARCH
        ));
    }
    Ok(())
}

pub async fn check_for_update_info(
    client: &reqwest::Client,
    current_version: &str,
    manifest_url: &str,
) -> Result<super::super::UpdateInfo, String> {
    check_update_platform_support()?;
    let manifest: LatestManifest = client
        .get(manifest_url)
        .send()
        .await
        .map_err(|e| format!("更新源连接失败: {e}"))?
        .error_for_status()
        .map_err(|e| format!("更新源响应异常: {e}"))?
        .json()
        .await
        .map_err(|e| format!("latest.json 解析失败: {e}"))?;

    let key = super::super::build_platform_key();
    let asset = manifest
        .platforms
        .get(&key)
        .ok_or_else(|| format!("latest.json 尚未发布 {key} 更新资产"))?;
    let kind = validate_asset(asset, allow_insecure_asset_url())?;
    let approved = approve_manifest_asset(current_version, asset, kind);

    Ok(super::super::UpdateInfo {
        available: is_newer_version(&asset.version, current_version),
        current_version: current_version.to_string(),
        latest_version: asset.version.clone(),
        notes: asset.notes.clone(),
        pub_date: asset.pub_date.clone(),
        url: asset.url.clone(),
        sha256: asset.sha256.to_ascii_lowercase(),
        size: asset.size,
        package_md5: String::new(),
        software_id: String::new(),
        sn: String::new(),
        update_type: kind.manifest_format().to_string(),
        platform: "windows".to_string(),
        ota_host: String::new(),
        approval_receipt: approved.receipt,
        platforms: manifest.platforms,
    })
}

fn validate_asset(
    asset: &super::super::PlatformAsset,
    allow_insecure_http: bool,
) -> Result<WindowsInstallerKind, String> {
    if parse_version_parts(&asset.version).is_none() {
        return Err(format!("Windows 更新版本号无效: {}", asset.version.trim()));
    }
    if !is_valid_sha256(&asset.sha256) {
        return Err("Windows 更新资产 sha256 必须是 64 位十六进制".to_string());
    }
    if asset.size == 0 || asset.size > MAX_DOWNLOAD_BYTES {
        return Err(format!("Windows 更新资产大小无效: {} bytes", asset.size));
    }

    let parsed = reqwest::Url::parse(asset.url.trim())
        .map_err(|_| "Windows 更新资产 URL 无效".to_string())?;
    let scheme_allowed = parsed.scheme() == "https"
        || (allow_insecure_http
            && parsed.scheme() == "http"
            && super::super::is_loopback_update_url(parsed.as_str()));
    if !scheme_allowed || parsed.host_str().is_none() {
        return Err("Windows 更新资产必须使用 HTTPS；HTTP 仅允许测试更新源".to_string());
    }

    let kind = installer_kind_from_format(&asset.format)?;
    let extension = Path::new(parsed.path())
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or_default();
    if !extension.eq_ignore_ascii_case(kind.extension()) {
        return Err(format!(
            "Windows 更新资产格式与 URL 扩展名不一致: format={} url={}",
            asset.format, asset.url
        ));
    }
    Ok(kind)
}

fn installer_kind_from_format(format: &str) -> Result<WindowsInstallerKind, String> {
    match format.trim().to_ascii_lowercase().as_str() {
        "msi" => Ok(WindowsInstallerKind::Msi),
        "nsis" | "exe" => Ok(WindowsInstallerKind::NsisExe),
        _ => Err("Windows 更新资产 format 只支持 nsis 或 msi".to_string()),
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
    if !is_newer_version(&approved.version, &approved.current_version) {
        return Err(format!(
            "拒绝下载非新版资产:当前 {}，目标 {}",
            approved.current_version, approved.version
        ));
    }
    let kind = approved.kind;
    let updates_dir = ensure_updates_dir()?;
    cleanup_stale_artifacts(&updates_dir);
    let destination = updates_dir.join(kind.output_name(&approved.version));
    let expected_sha256 = approved.sha256.clone();

    if destination.exists()
        && destination
            .metadata()
            .is_ok_and(|metadata| metadata.len() == approved.size)
        && file_sha256(&destination).as_deref() == Some(expected_sha256.as_str())
    {
        mark_approved_downloaded(&approved.receipt)?;
        return Ok(prepared_result(&destination, &approved.version));
    }

    // 一旦需要重新下载，先撤销先前的 downloaded 状态；失败或取消不会留下可安装批准。
    mark_approved_download_pending(&approved.receipt)?;

    if destination.exists() {
        std::fs::remove_file(&destination)
            .map_err(|e| format!("清理旧 Windows 更新文件失败: {e}"))?;
    }
    let temp_path = updates_dir.join(format!(
        "{OWNED_PART_PREFIX}{}-{}-{}{OWNED_PART_SUFFIX}",
        safe_version(&approved.version),
        std::process::id(),
        uuid::Uuid::new_v4()
    ));
    let mut temp_guard = TempDownload::new(temp_path.clone());

    let client = super::super::update_client_builder(&approved.url)?
        .connect_timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("HTTP client 构建失败: {e}"))?;
    let response = match super::super::wait_download_step(
        cancel,
        stall_timeout,
        client.get(&approved.url).send(),
    )
    .await
    {
        Ok(response) => response,
        Err(super::super::DownloadWaitError::Cancelled) => return Err("已取消下载".to_string()),
        Err(super::super::DownloadWaitError::Stalled) => {
            return Err(format!(
                "下载响应超时:超过 {}s 未收到响应头",
                stall_timeout.as_secs()
            ));
        }
        Err(super::super::DownloadWaitError::Request(error)) => {
            return Err(format!("下载请求失败: {error}"));
        }
    };
    let mut response = response
        .error_for_status()
        .map_err(|e| format!("下载响应异常: {e}"))?;

    if let Some(content_length) = response.content_length() {
        if content_length != approved.size {
            return Err(format!(
                "下载文件大小与清单不一致:清单 {} bytes，响应 {} bytes",
                approved.size, content_length
            ));
        }
    }

    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temp_path)
        .map_err(|e| format!("创建下载临时文件失败: {e}"))?;
    let mut hasher = Sha256::new();
    let mut downloaded = 0_u64;
    let mut last_emit = 0_u64;
    loop {
        let chunk = match super::super::wait_download_step(cancel, stall_timeout, response.chunk())
            .await
        {
            Err(super::super::DownloadWaitError::Cancelled) => return Err("已取消下载".to_string()),
            Err(super::super::DownloadWaitError::Stalled) => {
                return Err(format!(
                    "下载停滞:超过 {}s 无数据，已中断",
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
    drop(file);

    if downloaded != approved.size {
        return Err(format!(
            "下载文件大小与清单不一致:清单 {} bytes，实际 {} bytes",
            approved.size, downloaded
        ));
    }
    let actual_sha256 = crate::platform::encoding::hex_lower(&hasher.finalize());
    if actual_sha256 != expected_sha256 {
        return Err(format!(
            "SHA-256 校验失败(期望 {expected_sha256} 实际 {actual_sha256})，已删除下载文件"
        ));
    }

    std::fs::rename(&temp_path, &destination)
        .map_err(|e| format!("提交 Windows 更新文件失败: {e}"))?;
    temp_guard.commit();
    mark_approved_downloaded(&approved.receipt)?;
    Ok(prepared_result(&destination, &approved.version))
}

fn prepared_result(path: &Path, version: &str) -> super::super::DownloadUpdateResult {
    let path = windows_tool_path(path).to_string_lossy().into_owned();
    super::super::DownloadUpdateResult::Prepared(super::super::PreparedUpdate {
        package_path: path.clone(),
        installer_path: path,
        latest_version: version.to_string(),
    })
}

fn cleanup_stale_artifacts(updates_dir: &Path) {
    let Ok(entries) = std::fs::read_dir(updates_dir) else {
        return;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        let Ok(metadata) = entry.metadata() else {
            continue;
        };
        if is_owned_stale_artifact(
            &name.to_string_lossy(),
            metadata.modified().ok(),
            SystemTime::now(),
        ) {
            if metadata.is_file() {
                let _ = std::fs::remove_file(entry.path());
            } else if metadata.is_dir() {
                let _ = std::fs::remove_dir(entry.path());
            }
        }
    }
}

fn is_owned_stale_artifact(name: &str, modified: Option<SystemTime>, now: SystemTime) -> bool {
    let owned_part = name.starts_with(OWNED_PART_PREFIX) && name.ends_with(OWNED_PART_SUFFIX);
    let owned_install_marker = name.starts_with(OWNED_INSTALL_PREFIX)
        && (name.ends_with(".started")
            || name.ends_with(".error")
            || name.ends_with(".pending")
            || name.ends_with(".claimed")
            || name.ends_with(".aborted"));
    if !owned_part && !owned_install_marker {
        return false;
    }
    modified
        .and_then(|value| now.duration_since(value).ok())
        .is_some_and(|age| age >= STALE_PART_AGE)
}

pub fn install_downloaded_update(
    _deb_path: Option<String>,
    _installer_path: Option<String>,
    info: Option<super::super::UpdateInfo>,
) -> Result<bool, String> {
    let info = info.ok_or_else(|| "缺少 Windows 更新信息".to_string())?;
    let approved = approved_asset_from_ipc(&info, true)?;
    let expected_kind = approved.kind;
    let updates_dir = ensure_updates_dir()?;
    cleanup_stale_artifacts(&updates_dir);
    // installer_path 来自前端回传，不能作为安全边界。下载端和安装端都根据清单中的
    // 版本与格式独立推导同一个确定性文件名。
    let expected_name = expected_kind.output_name(&approved.version);
    let installer = canonical_inside(
        &updates_dir.join(&expected_name),
        &updates_dir,
        "Windows 安装文件",
    )?;
    if installer.file_name() != Some(std::ffi::OsStr::new(&expected_name)) {
        return Err("Windows 安装文件名与清单不一致".to_string());
    }
    let actual_kind = installer_kind(&installer)?;
    if actual_kind != expected_kind {
        return Err("安装文件类型与更新清单不一致".to_string());
    }
    let metadata = installer
        .metadata()
        .map_err(|e| format!("读取 Windows 安装文件失败: {e}"))?;
    if metadata.len() != approved.size {
        return Err(format!(
            "安装前文件大小复验失败:清单 {} bytes，实际 {} bytes",
            approved.size,
            metadata.len()
        ));
    }
    let actual_sha256 = file_sha256(&installer)
        .ok_or_else(|| "安装前无法计算 Windows 安装文件 SHA-256".to_string())?;
    if !actual_sha256.eq_ignore_ascii_case(&approved.sha256) {
        let _ = std::fs::remove_file(&installer);
        return Err("安装前 SHA-256 复验失败，已删除可疑文件".to_string());
    }

    install_update_package(
        &installer,
        &updates_dir,
        expected_kind,
        &expected_name,
        approved.size,
        &approved.sha256,
    )?;
    Ok(true)
}

fn install_update_package(
    installer: &Path,
    updates_dir: &Path,
    kind: WindowsInstallerKind,
    expected_name: &str,
    expected_size: u64,
    expected_sha256: &str,
) -> Result<(), String> {
    let trusted_tools = trusted_system_tools()?;
    let nonce = uuid::Uuid::new_v4().simple().to_string();
    let markers = HandshakeFiles {
        nonce: nonce.clone(),
        started: updates_dir.join(format!(".pinvou3-install-{nonce}.started")),
        error: updates_dir.join(format!(".pinvou3-install-{nonce}.error")),
        pending: updates_dir.join(format!(".pinvou3-install-{nonce}.pending")),
        claimed: updates_dir.join(format!(".pinvou3-install-{nonce}.claimed")),
        aborted: updates_dir.join(format!(".pinvou3-install-{nonce}.aborted")),
    };
    create_empty_marker(&markers.started)?;
    create_empty_marker(&markers.error)?;
    std::fs::create_dir(&markers.pending)
        .map_err(|error| format!("创建安装启动状态失败: {error}"))?;
    let expires_at = SystemTime::now()
        .checked_add(INSTALL_START_TIMEOUT)
        .and_then(|value| value.duration_since(SystemTime::UNIX_EPOCH).ok())
        .map(|value| value.as_secs())
        .ok_or_else(|| "无法计算安装启动批准有效期".to_string())?;

    let elevated_script = elevated_installer_script(
        kind,
        &windows_tool_path(&trusted_tools.msiexec),
        &windows_tool_path(installer),
        &windows_tool_path(updates_dir),
        expected_name,
        expected_size,
        expected_sha256,
        &windows_tool_path(&markers.started),
        &windows_tool_path(&markers.error),
        &windows_tool_path(&markers.pending),
        &windows_tool_path(&markers.claimed),
        &windows_tool_path(&markers.aborted),
        &markers.nonce,
        expires_at,
    );
    let encoded_script = powershell_encoded_command(&elevated_script);
    let broker_script = elevation_broker_script(
        &windows_tool_path(&trusted_tools.powershell),
        &encoded_script,
        &windows_tool_path(&markers.error),
        &markers.nonce,
    );
    let mut child = crate::platform::process::HiddenCommand::new(&trusted_tools.powershell)
        .args([
            "-NoProfile",
            "-NonInteractive",
            "-ExecutionPolicy",
            "Bypass",
            "-WindowStyle",
            "Hidden",
            "-Command",
            &broker_script,
        ])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|e| format!("Windows 安装器提权代理启动失败: {e}"))?;

    let started_at = Instant::now();
    let mut launch_claimed = false;
    loop {
        if marker_payload(&markers.started, &markers.nonce)
            .and_then(|value| value.parse::<u32>().ok())
            .is_some_and(|pid| pid > 0)
        {
            let _ = child.try_wait();
            return Ok(());
        }
        if let Some(error) = marker_payload(&markers.error, &markers.nonce) {
            let _ = child.wait();
            return Err(format!("Windows 安装器未启动: {error}"));
        }
        launch_claimed |= markers.claimed.is_dir();
        if !launch_claimed && started_at.elapsed() >= INSTALL_START_TIMEOUT {
            match std::fs::rename(&markers.pending, &markers.aborted) {
                Ok(()) => {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(format!(
                        "等待 Windows 安装器提权启动超过 {} 秒，批准已原子撤销；应用将继续运行",
                        INSTALL_START_TIMEOUT.as_secs()
                    ));
                }
                Err(_) if markers.claimed.is_dir() => launch_claimed = true,
                Err(_) if markers.aborted.is_dir() => {
                    return Err("Windows 安装器启动批准已撤销；应用将继续运行".to_string());
                }
                // 无法证明 abort 原子提交成功时不能向调用方宣称已取消；停止超时
                // 路径并等待 elevated helper 给出 started/error 明确终态。
                Err(_) => launch_claimed = true,
            }
        }
        let _ = child.try_wait();
        std::thread::sleep(Duration::from_millis(50));
    }
}

pub async fn report_pending_update_result_info(
    _client: &reqwest::Client,
    _current_version: &str,
) -> Result<super::super::PendingUpdateReportResult, String> {
    Ok(super::super::PendingUpdateReportResult {
        had_pending: false,
        reported: false,
        result: String::new(),
        message: "静态 Windows 更新没有服务端反馈记录".to_string(),
    })
}

pub fn is_newer_version(latest: &str, current: &str) -> bool {
    compare_versions(latest, current).is_some_and(|ordering| ordering.is_gt())
}

fn compare_versions(a: &str, b: &str) -> Option<std::cmp::Ordering> {
    let left = parse_version_parts(a)?;
    let right = parse_version_parts(b)?;
    let width = left.len().max(right.len());
    for index in 0..width {
        match left
            .get(index)
            .copied()
            .unwrap_or(0)
            .cmp(&right.get(index).copied().unwrap_or(0))
        {
            std::cmp::Ordering::Equal => {}
            other => return Some(other),
        }
    }
    Some(std::cmp::Ordering::Equal)
}

fn parse_version_parts(value: &str) -> Option<Vec<u64>> {
    let value = value.trim().trim_start_matches('v');
    if value.is_empty() {
        return None;
    }
    let parts: Option<Vec<u64>> = value.split('.').map(|part| part.parse().ok()).collect();
    let parts = parts?;
    if (3..=4).contains(&parts.len()) {
        Some(parts)
    } else {
        None
    }
}

fn is_valid_sha256(value: &str) -> bool {
    value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn safe_version(value: &str) -> String {
    value
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || matches!(character, '.' | '-' | '_') {
                character
            } else {
                '_'
            }
        })
        .collect()
}

fn ensure_updates_dir() -> Result<PathBuf, String> {
    let directory = paths::updates_dir();
    std::fs::create_dir_all(&directory).map_err(|e| format!("创建更新目录失败: {e}"))?;
    directory
        .canonicalize()
        .map_err(|e| format!("更新目录不可访问: {e}"))
}

fn canonical_inside(path: &Path, root: &Path, label: &str) -> Result<PathBuf, String> {
    let canonical = path
        .canonicalize()
        .map_err(|e| format!("{label}不存在或不可访问: {e}"))?;
    let canonical_root = root
        .canonicalize()
        .map_err(|e| format!("更新目录不可访问: {e}"))?;
    if !canonical.starts_with(&canonical_root) {
        return Err(format!("非法路径:{label}必须位于更新目录内"));
    }
    Ok(canonical)
}

fn installer_kind(installer: &Path) -> Result<WindowsInstallerKind, String> {
    match installer
        .extension()
        .and_then(|value| value.to_str())
        .map(|value| value.to_ascii_lowercase())
        .as_deref()
    {
        Some("msi") => Ok(WindowsInstallerKind::Msi),
        Some("exe") => Ok(WindowsInstallerKind::NsisExe),
        _ => Err("非法路径:只接受 .msi 或 .exe Windows 安装文件".to_string()),
    }
}

fn file_sha256(path: &Path) -> Option<String> {
    let mut file = std::fs::File::open(path).ok()?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let read = file.read(&mut buffer).ok()?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Some(crate::platform::encoding::hex_lower(&hasher.finalize()))
}

fn windows_tool_path(path: &Path) -> PathBuf {
    #[cfg(windows)]
    {
        use std::path::Prefix;

        let mut components = path.components();
        if let Some(Component::Prefix(prefix)) = components.next() {
            match prefix.kind() {
                Prefix::VerbatimDisk(disk) => {
                    let mut output = PathBuf::from(format!("{}:\\", disk as char));
                    output.extend(components);
                    return output;
                }
                Prefix::VerbatimUNC(server, share) => {
                    let mut output = PathBuf::from(format!(
                        "\\\\{}\\{}",
                        server.to_string_lossy(),
                        share.to_string_lossy()
                    ));
                    output.extend(components);
                    return output;
                }
                _ => {}
            }
        }
    }
    path.to_path_buf()
}

fn trusted_system_tools() -> Result<TrustedSystemTools, String> {
    use std::os::windows::ffi::OsStringExt as _;
    use windows_sys::Win32::System::SystemInformation::GetSystemDirectoryW;

    let mut buffer = vec![0_u16; 32_768];
    // SAFETY: buffer is writable for its advertised length; the API returns the number of
    // UTF-16 code units written (excluding NUL), or a required size larger than the buffer.
    let length = unsafe { GetSystemDirectoryW(buffer.as_mut_ptr(), buffer.len() as u32) } as usize;
    if length == 0 || length >= buffer.len() {
        return Err("无法从 Windows 获取可信系统目录".to_string());
    }
    buffer.truncate(length);
    let directory = PathBuf::from(OsString::from_wide(&buffer));
    let tools = trusted_system_tools_from_directory(&directory)?;
    if !tools.powershell.is_file() || !tools.msiexec.is_file() {
        return Err("Windows 系统 PowerShell 或 msiexec 不存在".to_string());
    }
    Ok(tools)
}

fn trusted_system_tools_from_directory(directory: &Path) -> Result<TrustedSystemTools, String> {
    if !directory.is_absolute() {
        return Err("Windows 系统目录不是绝对路径".to_string());
    }
    Ok(TrustedSystemTools {
        powershell: directory
            .join("WindowsPowerShell")
            .join("v1.0")
            .join("powershell.exe"),
        msiexec: directory.join("msiexec.exe"),
    })
}

fn msi_install_args(installer: &Path) -> Vec<OsString> {
    vec![
        OsString::from("/i"),
        installer.as_os_str().to_os_string(),
        OsString::from("REINSTALLMODE=vamus"),
        OsString::from("/passive"),
        OsString::from("/norestart"),
    ]
}

fn nsis_install_args() -> Vec<OsString> {
    vec![
        OsString::from("/P"),
        OsString::from("/UPDATE"),
        OsString::from("/R"),
    ]
}

fn create_empty_marker(path: &Path) -> Result<(), String> {
    std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(path)
        .map(|_| ())
        .map_err(|error| format!("创建安装启动握手文件失败: {error}"))
}

fn marker_payload(path: &Path, nonce: &str) -> Option<String> {
    std::fs::read_to_string(path)
        .ok()
        .and_then(|value| {
            value
                .trim()
                .strip_prefix(&format!("{nonce}:"))
                .map(str::to_string)
        })
        .filter(|value| !value.is_empty())
}

fn powershell_encoded_command(script: &str) -> String {
    let utf16le = script
        .encode_utf16()
        .flat_map(u16::to_le_bytes)
        .collect::<Vec<_>>();
    base64::engine::general_purpose::STANDARD.encode(utf16le)
}

fn elevation_broker_script(
    system_powershell: &Path,
    encoded_script: &str,
    error_marker: &Path,
    nonce: &str,
) -> String {
    let system_powershell = powershell_single_quoted(&system_powershell.to_string_lossy());
    let error_marker = powershell_single_quoted(&error_marker.to_string_lossy());
    let error_prefix = powershell_single_quoted(&format!("{nonce}:"));
    format!(
        "$ErrorActionPreference = 'Stop'; try {{ Start-Process -FilePath {system_powershell} -ArgumentList @('-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-WindowStyle','Hidden','-EncodedCommand','{encoded_script}') -Verb RunAs | Out-Null }} catch {{ [IO.File]::WriteAllText({error_marker}, {error_prefix} + $_.Exception.Message, [Text.UTF8Encoding]::new($false)) }}"
    )
}

#[allow(clippy::too_many_arguments)]
fn elevated_installer_script(
    kind: WindowsInstallerKind,
    system_msiexec: &Path,
    installer: &Path,
    updates_dir: &Path,
    expected_name: &str,
    expected_size: u64,
    expected_sha256: &str,
    started_marker: &Path,
    error_marker: &Path,
    pending_state: &Path,
    claimed_state: &Path,
    aborted_state: &Path,
    nonce: &str,
    expires_at: u64,
) -> String {
    let system_msiexec = powershell_single_quoted(&system_msiexec.to_string_lossy());
    let installer = powershell_single_quoted(&installer.to_string_lossy());
    let updates_dir = powershell_single_quoted(&updates_dir.to_string_lossy());
    let expected_name = powershell_single_quoted(expected_name);
    let expected_sha256 = powershell_single_quoted(&expected_sha256.to_ascii_lowercase());
    let started_marker = powershell_single_quoted(&started_marker.to_string_lossy());
    let error_marker = powershell_single_quoted(&error_marker.to_string_lossy());
    let pending_state = powershell_single_quoted(&pending_state.to_string_lossy());
    let claimed_state = powershell_single_quoted(&claimed_state.to_string_lossy());
    let aborted_state = powershell_single_quoted(&aborted_state.to_string_lossy());
    let nonce_prefix = powershell_single_quoted(&format!("{nonce}:"));
    let start_installer = match kind {
        WindowsInstallerKind::NsisExe => {
            "$p = Start-Process -FilePath $installer -ArgumentList @('/P','/UPDATE','/R') -PassThru; "
                .to_string()
        }
        WindowsInstallerKind::Msi => format!(
            "$msiArguments = '/i \"' + $installer + '\" REINSTALLMODE=vamus /passive /norestart'; $p = Start-Process -FilePath {system_msiexec} -ArgumentList $msiArguments -PassThru; "
        ),
    };

    format!(
        concat!(
            "$ErrorActionPreference = 'Stop'; $installerStarted = $false; ",
            "try {{ ",
            "$installer = [IO.Path]::GetFullPath({installer}); ",
            "$root = [IO.Path]::GetFullPath({updates_dir}); ",
            "$rootPrefix = $root.TrimEnd([char[]]@('\\','/')) + [IO.Path]::DirectorySeparatorChar; ",
            "if (-not $installer.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) {{ throw 'installer escaped controlled updates directory' }}; ",
            "if (-not [IO.Path]::GetFileName($installer).Equals({expected_name}, [StringComparison]::OrdinalIgnoreCase)) {{ throw 'installer filename does not match manifest' }}; ",
            "$stream = [IO.File]::Open($installer, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read); ",
            "try {{ ",
            "if ([uint64]$stream.Length -ne [uint64]{expected_size}) {{ throw 'installer size does not match manifest' }}; ",
            "$sha = [Security.Cryptography.SHA256]::Create(); ",
            "try {{ $actual = ([BitConverter]::ToString($sha.ComputeHash($stream))).Replace('-','').ToLowerInvariant() }} finally {{ $sha.Dispose() }}; ",
            "if ($actual -cne {expected_sha256}) {{ throw 'installer SHA-256 does not match manifest' }}; ",
            "if (Test-Path -LiteralPath {aborted_state}) {{ throw 'installer launch approval was revoked' }}; ",
            "if ([DateTimeOffset]::UtcNow.ToUnixTimeSeconds() -gt [int64]{expires_at}) {{ throw 'installer launch approval expired' }}; ",
            "try {{ Move-Item -LiteralPath {pending_state} -Destination {claimed_state} -ErrorAction Stop }} catch {{ if (Test-Path -LiteralPath {aborted_state}) {{ throw 'installer launch approval was revoked' }}; throw }}; ",
            "{start_installer}",
            "if ($null -eq $p -or $p.Id -le 0) {{ throw 'installer process was not created' }}; ",
            "[IO.File]::WriteAllText({started_marker}, {nonce_prefix} + [string]$p.Id, [Text.UTF8Encoding]::new($false)); ",
            "$installerStarted = $true; ",
            "$p.WaitForExit(); ",
            "}} finally {{ $stream.Dispose() }}; ",
            "}} catch {{ if (-not $installerStarted) {{ [IO.File]::WriteAllText({error_marker}, {nonce_prefix} + $_.Exception.Message, [Text.UTF8Encoding]::new($false)) }}; exit 1 }}"
        ),
        installer = installer,
        updates_dir = updates_dir,
        expected_name = expected_name,
        expected_size = expected_size,
        expected_sha256 = expected_sha256,
        start_installer = start_installer,
        started_marker = started_marker,
        pending_state = pending_state,
        claimed_state = claimed_state,
        aborted_state = aborted_state,
        expires_at = expires_at,
        nonce_prefix = nonce_prefix,
        error_marker = error_marker,
    )
}

fn powershell_single_quoted(value: &str) -> String {
    format!("'{}'", value.replace('\'', "''"))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn asset(format: &str, url: &str) -> super::super::super::PlatformAsset {
        super::super::super::PlatformAsset {
            url: url.to_string(),
            format: format.to_string(),
            sha256: "a".repeat(64),
            size: 1024,
            restart_after_install: false,
            notes: "Windows notes".to_string(),
            pub_date: "2026-09-22T08:00:00Z".to_string(),
            version: "0.9.0".to_string(),
        }
    }

    #[test]
    fn static_asset_accepts_signed_installer_formats() {
        assert_eq!(
            validate_asset(
                &asset("nsis", "https://example.com/pinvou3_0.9.0_x64-setup.exe"),
                false
            )
            .unwrap(),
            WindowsInstallerKind::NsisExe
        );
        assert_eq!(
            validate_asset(
                &asset("msi", "https://example.com/pinvou3_0.9.0_x64.msi"),
                false
            )
            .unwrap(),
            WindowsInstallerKind::Msi
        );
    }

    #[test]
    fn static_asset_rejects_insecure_or_mismatched_entries() {
        assert!(
            validate_asset(
                &asset("nsis", "http://example.com/pinvou3_0.9.0_x64-setup.exe"),
                false
            )
            .is_err()
        );
        assert!(
            validate_asset(
                &asset("msi", "https://example.com/pinvou3_0.9.0_x64-setup.exe"),
                false
            )
            .is_err()
        );
        let mut missing_hash = asset("nsis", "https://example.com/setup.exe");
        missing_hash.sha256.clear();
        assert!(validate_asset(&missing_hash, false).is_err());
    }

    #[test]
    fn approved_asset_requires_receipt_and_all_security_fields_to_match() {
        let approved = ApprovedAsset {
            receipt: "receipt-secret".to_string(),
            current_version: "0.8.0".to_string(),
            version: "0.9.0".to_string(),
            url: "https://example.com/pinvou3_0.9.0_x64-setup.exe".to_string(),
            sha256: "a".repeat(64),
            size: 1024,
            kind: WindowsInstallerKind::NsisExe,
            downloaded: false,
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
            update_type: approved.kind.manifest_format().to_string(),
            platform: "windows".to_string(),
            ota_host: String::new(),
            approval_receipt: approved.receipt.clone(),
            platforms: std::collections::HashMap::new(),
        };
        assert!(approved.matches_ipc(&info));
        info.platforms.insert(
            "windows-x86_64".to_string(),
            asset("msi", "https://attacker.invalid/fake.msi"),
        );
        assert!(approved.matches_ipc(&info), "platforms 副本不参与信任决策");
        info.size += 1;
        assert!(!approved.matches_ipc(&info));
        info.size = approved.size;
        info.approval_receipt = "forged".to_string();
        assert!(!approved.matches_ipc(&info));
    }

    #[test]
    fn four_part_versions_compare_without_downgrade() {
        assert!(is_newer_version("0.9.0.1", "0.9.0"));
        assert!(is_newer_version("0.10.0", "0.9.99.9"));
        assert!(!is_newer_version("0.9.0.0", "0.9.0"));
        assert!(!is_newer_version("0.8.9", "0.9.0"));
        assert!(!is_newer_version("invalid", "0.9.0"));
    }

    #[test]
    fn installer_arguments_keep_visible_passive_upgrade_mode() {
        let args: Vec<String> = nsis_install_args()
            .into_iter()
            .map(|argument| argument.to_string_lossy().into_owned())
            .collect();
        assert_eq!(args, vec!["/P", "/UPDATE", "/R"]);

        let msi: Vec<String> = msi_install_args(Path::new(r"C:\pinvou3.msi"))
            .into_iter()
            .map(|argument| argument.to_string_lossy().into_owned())
            .collect();
        assert!(msi.contains(&"/passive".to_string()));
        assert!(msi.contains(&"/norestart".to_string()));
        assert!(!msi.contains(&"REINSTALL=ALL".to_string()));
    }

    #[test]
    fn elevated_launcher_revalidates_and_delegates_restart_to_nsis() {
        let rendered = elevated_installer_script(
            WindowsInstallerKind::NsisExe,
            Path::new(r"C:\Windows\System32\msiexec.exe"),
            Path::new(r"C:\updates\pinvou3's-setup.exe"),
            Path::new(r"C:\updates"),
            "pinvou3_0.9.0_x64-setup.exe",
            1024,
            &"a".repeat(64),
            Path::new(r"C:\updates\.started"),
            Path::new(r"C:\updates\.error"),
            Path::new(r"C:\updates\.pending"),
            Path::new(r"C:\updates\.claimed"),
            Path::new(r"C:\updates\.aborted"),
            "test-nonce",
            4_102_444_800,
        );
        assert!(rendered.contains("[IO.FileShare]::Read"));
        assert!(rendered.contains("SHA-256 does not match manifest"));
        assert!(rendered.contains("installer process was not created"));
        assert!(rendered.contains("Move-Item -LiteralPath 'C:\\updates\\.pending'"));
        assert!(rendered.contains("'test-nonce:' + [string]$p.Id"));
        assert!(rendered.contains("@('/P','/UPDATE','/R')"));
        assert!(!rendered.contains("Microsoft.Win32.Registry"));
        assert!(!rendered.contains("CurrentUser"));
        assert!(!rendered.contains("LocalMachine"));
        assert!(!rendered.contains("InstallLocation"));
        assert!(!rendered.contains("ProgramFiles"));
        assert!(!rendered.contains("pinvou3-tauri.exe"));
        assert!(rendered.contains("pinvou3''s-setup.exe"));
        assert!(
            rendered.find("$p.WaitForExit()").unwrap()
                < rendered.find("$stream.Dispose()").unwrap()
        );
    }

    #[test]
    fn msi_launcher_keeps_space_and_apostrophe_path_as_one_quoted_argument() {
        let rendered = elevated_installer_script(
            WindowsInstallerKind::Msi,
            Path::new(r"C:\Windows\System32\msiexec.exe"),
            Path::new(r"C:\updates with space\pinvou3's package.msi"),
            Path::new(r"C:\updates with space"),
            "pinvou3_0.9.0_x64.msi",
            2048,
            &"b".repeat(64),
            Path::new(r"C:\updates with space\.started"),
            Path::new(r"C:\updates with space\.error"),
            Path::new(r"C:\updates with space\.pending"),
            Path::new(r"C:\updates with space\.claimed"),
            Path::new(r"C:\updates with space\.aborted"),
            "test-nonce",
            4_102_444_800,
        );
        assert!(rendered.contains(
            "$installer = [IO.Path]::GetFullPath('C:\\updates with space\\pinvou3''s package.msi')"
        ));
        assert!(rendered.contains(
            "$msiArguments = '/i \"' + $installer + '\" REINSTALLMODE=vamus /passive /norestart'"
        ));
        assert!(rendered.contains(
            "Start-Process -FilePath 'C:\\Windows\\System32\\msiexec.exe' -ArgumentList $msiArguments -PassThru"
        ));
        assert!(!rendered.contains("pinvou3-tauri.exe"));
        assert!(!rendered.contains("Microsoft.Win32.Registry"));
    }

    #[test]
    fn broker_only_reports_success_after_elevated_helper_writes_handshake() {
        let script = elevation_broker_script(
            Path::new(r"C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe"),
            "encoded-payload",
            Path::new(r"C:\u\failure.txt"),
            "test-nonce",
        );
        assert!(script.contains("-Verb RunAs"));
        assert!(script.contains(
            "-FilePath 'C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe'"
        ));
        assert!(script.contains("-EncodedCommand','encoded-payload'"));
        assert!(script.contains("WriteAllText('C:\\u\\failure.txt', 'test-nonce:'"));
    }

    #[test]
    fn trusted_tools_are_derived_from_absolute_system_directory() {
        let tools = trusted_system_tools_from_directory(Path::new(r"C:\Windows\System32")).unwrap();
        assert_eq!(
            tools.powershell,
            PathBuf::from(r"C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe")
        );
        assert_eq!(
            tools.msiexec,
            PathBuf::from(r"C:\Windows\System32\msiexec.exe")
        );
        assert!(trusted_system_tools_from_directory(Path::new("System32")).is_err());
    }

    #[test]
    fn stale_cleanup_only_owns_aged_random_part_files() {
        let now = SystemTime::UNIX_EPOCH + Duration::from_secs(3 * 24 * 60 * 60);
        let old = Some(now - STALE_PART_AGE);
        let fresh = Some(now - Duration::from_secs(60));
        assert!(is_owned_stale_artifact(
            ".pinvou3-update-0.9.0-42-random.part",
            old,
            now
        ));
        assert!(!is_owned_stale_artifact(
            ".pinvou3-update-0.9.0-42-random.part",
            fresh,
            now
        ));
        assert!(is_owned_stale_artifact(
            ".pinvou3-install-random.started",
            old,
            now
        ));
        assert!(!is_owned_stale_artifact("customer-file.part", old, now));
        assert!(!is_owned_stale_artifact(
            ".pinvou3-update-0.9.0-42-random.tmp",
            old,
            now
        ));
    }

    #[test]
    fn output_name_is_not_controlled_by_manifest_url() {
        assert_eq!(
            WindowsInstallerKind::NsisExe.output_name("0.9.0"),
            "pinvou3_0.9.0_x64-setup.exe"
        );
        assert_eq!(safe_version("../0.9.0"), ".._0.9.0");
    }
}
