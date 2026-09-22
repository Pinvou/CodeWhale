//! Windows 静态 OTA：latest.json 平台清单 → 直接下载已签名 NSIS/MSI
//! （SHA-256）→ UAC 启动安装器 → 当前进程退出 → 安装成功后启动新版。
//!
//! Windows 正式安装包仍由独立签名服务器构建并签署；本模块不执行签名，也不做
//! Authenticode 二次阻断校验。更新器只信任固定更新源中的平台清单，并以清单内的
//! SHA-256 校验传输完整性。

use std::ffi::OsString;
use std::io::{Read, Write};
use std::path::{Component, Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use serde::Deserialize;
use sha2::{Digest, Sha256};
use tauri::{AppHandle, Emitter};
use tokio::time::timeout;

use crate::platform::paths;

const UPDATE_MANIFEST_URL: &str = "https://pinvou.com/pinvou3/latest.json";
const MAX_DOWNLOAD_BYTES: u64 = 4 * 1024 * 1024 * 1024;

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

pub fn cleanup_stale_backup() {}

fn manifest_url() -> String {
    std::env::var("PINVOU3_UPDATE_URL").unwrap_or_else(|_| UPDATE_MANIFEST_URL.to_string())
}

fn allow_insecure_asset_url() -> bool {
    std::env::var("PINVOU3_UPDATE_URL")
        .ok()
        .is_some_and(|value| value.trim().to_ascii_lowercase().starts_with("http://"))
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
) -> Result<super::super::UpdateInfo, String> {
    check_update_platform_support()?;
    let manifest: LatestManifest = client
        .get(manifest_url())
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
    let scheme_allowed =
        parsed.scheme() == "https" || (allow_insecure_http && parsed.scheme() == "http");
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

fn validate_download_info(info: &super::super::UpdateInfo) -> Result<WindowsInstallerKind, String> {
    if info.platform != "windows" {
        return Err("拒绝下载:更新信息不是 Windows 平台资产".to_string());
    }
    if !is_newer_version(&info.latest_version, &info.current_version) {
        return Err(format!(
            "拒绝下载非新版资产:当前 {}，目标 {}",
            info.current_version, info.latest_version
        ));
    }
    let asset = super::super::PlatformAsset {
        url: info.url.clone(),
        format: info.update_type.clone(),
        sha256: info.sha256.clone(),
        size: info.size,
        restart_after_install: false,
        notes: info.notes.clone(),
        pub_date: info.pub_date.clone(),
        version: info.latest_version.clone(),
    };
    validate_asset(&asset, allow_insecure_asset_url())
}

pub async fn download_update_package(
    info: &super::super::UpdateInfo,
    app: AppHandle,
    cancel: &AtomicBool,
    stall_timeout: Duration,
) -> Result<super::super::DownloadUpdateResult, String> {
    check_update_platform_support()?;
    let kind = validate_download_info(info)?;
    let updates_dir = ensure_updates_dir()?;
    let destination = updates_dir.join(kind.output_name(&info.latest_version));
    let expected_sha256 = info.sha256.to_ascii_lowercase();

    if destination.exists()
        && destination
            .metadata()
            .is_ok_and(|metadata| metadata.len() == info.size)
        && file_sha256(&destination).as_deref() == Some(expected_sha256.as_str())
    {
        return Ok(prepared_result(&destination, &info.latest_version));
    }

    if destination.exists() {
        std::fs::remove_file(&destination)
            .map_err(|e| format!("清理旧 Windows 更新文件失败: {e}"))?;
    }
    cleanup_stale_parts(&updates_dir);

    let temp_path = updates_dir.join(format!(
        ".{}.part.{}",
        kind.output_name(&info.latest_version),
        std::process::id()
    ));
    let _ = std::fs::remove_file(&temp_path);
    let mut temp_guard = TempDownload::new(temp_path.clone());

    let client = reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("HTTP client 构建失败: {e}"))?;
    let mut response = client
        .get(&info.url)
        .send()
        .await
        .map_err(|e| format!("下载请求失败: {e}"))?
        .error_for_status()
        .map_err(|e| format!("下载响应异常: {e}"))?;

    if let Some(content_length) = response.content_length() {
        if content_length != info.size {
            return Err(format!(
                "下载文件大小与清单不一致:清单 {} bytes，响应 {} bytes",
                info.size, content_length
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
    cancel.store(false, Ordering::SeqCst);

    loop {
        if cancel.load(Ordering::SeqCst) {
            return Err("已取消下载".to_string());
        }
        let chunk = match timeout(stall_timeout, response.chunk()).await {
            Err(_) => {
                return Err(format!(
                    "下载停滞:超过 {}s 无数据，已中断",
                    stall_timeout.as_secs()
                ));
            }
            Ok(Err(e)) => return Err(format!("下载中断: {e}")),
            Ok(Ok(None)) => break,
            Ok(Ok(Some(chunk))) => chunk,
        };
        let next = downloaded.saturating_add(chunk.len() as u64);
        if next > info.size || next > MAX_DOWNLOAD_BYTES {
            return Err("下载体积超过清单声明或安全上限，已中断".to_string());
        }
        file.write_all(&chunk)
            .map_err(|e| format!("写盘失败: {e}"))?;
        hasher.update(&chunk);
        downloaded = next;
        if downloaded - last_emit >= 262_144 || downloaded == info.size {
            last_emit = downloaded;
            let _ = app.emit(
                "update:progress",
                serde_json::json!({ "downloaded": downloaded, "total": info.size }),
            );
        }
    }
    drop(file);

    if downloaded != info.size {
        return Err(format!(
            "下载文件大小与清单不一致:清单 {} bytes，实际 {} bytes",
            info.size, downloaded
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
    Ok(prepared_result(&destination, &info.latest_version))
}

fn prepared_result(path: &Path, version: &str) -> super::super::DownloadUpdateResult {
    let path = windows_tool_path(path).to_string_lossy().into_owned();
    super::super::DownloadUpdateResult::Prepared(super::super::PreparedUpdate {
        package_path: path.clone(),
        installer_path: path,
        latest_version: version.to_string(),
    })
}

fn cleanup_stale_parts(updates_dir: &Path) {
    let Ok(entries) = std::fs::read_dir(updates_dir) else {
        return;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        if name.to_string_lossy().contains(".part.") {
            let _ = std::fs::remove_file(entry.path());
        }
    }
}

pub fn install_downloaded_update(
    _deb_path: Option<String>,
    installer_path: Option<String>,
    info: Option<super::super::UpdateInfo>,
) -> Result<bool, String> {
    let installer_path = installer_path.ok_or_else(|| "缺少 Windows 安装文件路径".to_string())?;
    let info = info.ok_or_else(|| "缺少 Windows 更新信息".to_string())?;
    let expected_kind = validate_download_info(&info)?;
    let updates_dir = ensure_updates_dir()?;
    let installer = canonical_inside(Path::new(&installer_path), &updates_dir, "Windows 安装文件")?;
    let actual_kind = installer_kind(&installer)?;
    if actual_kind != expected_kind {
        return Err("安装文件类型与更新清单不一致".to_string());
    }
    let metadata = installer
        .metadata()
        .map_err(|e| format!("读取 Windows 安装文件失败: {e}"))?;
    if metadata.len() != info.size {
        return Err(format!(
            "安装前文件大小复验失败:清单 {} bytes，实际 {} bytes",
            info.size,
            metadata.len()
        ));
    }
    let actual_sha256 = file_sha256(&installer)
        .ok_or_else(|| "安装前无法计算 Windows 安装文件 SHA-256".to_string())?;
    if !actual_sha256.eq_ignore_ascii_case(&info.sha256) {
        let _ = std::fs::remove_file(&installer);
        return Err("安装前 SHA-256 复验失败，已删除可疑文件".to_string());
    }

    install_update_package(&installer)?;
    Ok(true)
}

pub fn install_update_package(path: &Path) -> Result<(), String> {
    let updates_dir = ensure_updates_dir()?;
    let installer = canonical_inside(path, &updates_dir, "Windows 安装文件")?;
    let kind = installer_kind(&installer)?;
    let installer_arg = windows_tool_path(&installer);
    crate::platform::process::HiddenCommand::new("powershell.exe")
        .args(update_installer_launcher_args(kind, &installer_arg))
        .spawn()
        .map_err(|e| format!("Windows 安装器提权启动失败: {e}"))?;
    Ok(())
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
    vec![OsString::from("/P"), OsString::from("/UPDATE")]
}

fn update_installer_launcher_args(kind: WindowsInstallerKind, installer: &Path) -> Vec<OsString> {
    let (file_path, install_args) = match kind {
        WindowsInstallerKind::Msi => (OsString::from("msiexec.exe"), msi_install_args(installer)),
        WindowsInstallerKind::NsisExe => {
            (installer.as_os_str().to_os_string(), nsis_install_args())
        }
    };
    let file_path = powershell_single_quoted(&file_path.to_string_lossy());
    let argument_list = install_args
        .iter()
        .map(|argument| powershell_single_quoted(&argument.to_string_lossy()))
        .collect::<Vec<_>>()
        .join(", ");
    let mut script = format!(
        "$ErrorActionPreference = 'Stop'; $p = Start-Process -FilePath {file_path} -ArgumentList @({argument_list}) -Verb RunAs -Wait -PassThru; "
    );
    script.push_str("$code = $p.ExitCode; if ($code -eq 0 -or $code -eq 3010) { ");
    script.push_str("$installDir = $null; ");
    script.push_str("try { $installDir = (Get-ItemProperty -Path 'HKCU:\\Software\\pinvou\\pinvou3' -Name 'InstallDir' -ErrorAction SilentlyContinue).InstallDir } catch {} ");
    script.push_str("if ([string]::IsNullOrWhiteSpace($installDir)) { $installDir = Join-Path $env:ProgramFiles 'pinvou3' } ");
    script.push_str("$exe = Join-Path $installDir 'pinvou3-tauri.exe'; ");
    script.push_str("if (Test-Path -LiteralPath $exe) { Start-Process -FilePath $exe } ");
    script.push('}');
    vec![
        OsString::from("-NoProfile"),
        OsString::from("-ExecutionPolicy"),
        OsString::from("Bypass"),
        OsString::from("-WindowStyle"),
        OsString::from("Hidden"),
        OsString::from("-Command"),
        OsString::from(script),
    ]
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
        assert_eq!(args, vec!["/P", "/UPDATE"]);

        let msi: Vec<String> = msi_install_args(Path::new(r"C:\pinvou3.msi"))
            .into_iter()
            .map(|argument| argument.to_string_lossy().into_owned())
            .collect();
        assert!(msi.contains(&"/passive".to_string()));
        assert!(msi.contains(&"/norestart".to_string()));
        assert!(!msi.contains(&"REINSTALL=ALL".to_string()));
    }

    #[test]
    fn launcher_uses_uac_waits_and_restarts_new_binary() {
        let rendered = update_installer_launcher_args(
            WindowsInstallerKind::NsisExe,
            Path::new(r"C:\updates\pinvou3's-setup.exe"),
        )
        .into_iter()
        .map(|argument| argument.to_string_lossy().into_owned())
        .collect::<Vec<_>>()
        .join(" ");
        assert!(rendered.contains("-WindowStyle Hidden"));
        assert!(rendered.contains("-Verb RunAs"));
        assert!(rendered.contains("-Wait"));
        assert!(rendered.contains("'/P'"));
        assert!(rendered.contains("'/UPDATE'"));
        assert!(rendered.contains("pinvou3-tauri.exe"));
        assert!(rendered.contains("pinvou3''s-setup.exe"));
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
