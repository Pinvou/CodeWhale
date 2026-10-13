//! 应用内升级：
//! - Linux：检查 latest.json → 下载 deb（sha256 校验）→ pkexec apt 安装 → 重启。
//! - macOS：检查平台清单 → 下载 dmg（sha256 校验）→ 原子替换 app → 重启。
//! - Windows：检查平台清单 → 下载已签名 NSIS/MSI（sha256 校验）→ UAC 安装；
//!   NSIS 由安装器自身的 `/R`/RunAsUser 启动新版，MSI 不自动重启应用。
//!
//! 设计要点：
//! - **更新源是静态 HTTPS**：服务器只托管 `latest.json` + 各平台安装包，零服务端逻辑。
//! - **URL 不进 settings.json**：更新源是基础设施不是用户偏好，可改会成攻击面。
//!   仅 debug/test 构建允许 `PINVOU3_UPDATE_URL` 指向 loopback e2e server；
//!   release 构建始终使用固定 HTTPS 地址。
//! - **下载目录用 `~/.pinvou3/updates/`** 而非 /tmp：tmpfs 受内存限制且重启清空。
//! - **安装走 pkexec**（弹系统密码框），与 super_permission.rs 同套路；但这里用
//!   `.output()` 捕 stderr 透传 apt 真实报错（lock 占用 / 磁盘不足等）。
//! - **inode 语义**：apt 替换 `/usr/bin/pinvou3` 后老进程持旧 inode 继续跑无害，
//!   `app.restart()` 按路径 exec 才换到新版。所以装完不强制重启，前端给按钮。

use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tauri::AppHandle;

mod health;
mod platform;

pub use health::UpdateReadyCommitResult;

pub(crate) fn observe_pending_update_health() {
    health::observe_pending_update(env!("CARGO_PKG_VERSION"), std::env::consts::OS);
}

/// 下载停滞看门狗阈值：连上后单次等待数据超过此时长即判定挂死。
/// 用「单 chunk 间隔」而非「总耗时」做超时——慢网持续小流量不会被误杀，
/// 只有真正长时间收不到任何字节（更新源挂起 / 半开连接）才中断。
const DOWNLOAD_STALL_TIMEOUT: Duration = Duration::from_secs(30);

const UPDATE_MANIFEST_URL: &str = "https://pinvou.com/pinvou3/latest.json";

fn manifest_url() -> String {
    #[cfg(any(debug_assertions, test))]
    if let Ok(value) = std::env::var("PINVOU3_UPDATE_URL") {
        if is_loopback_update_url(&value) {
            return value;
        }
    }
    UPDATE_MANIFEST_URL.to_string()
}

fn allow_loopback_test_urls() -> bool {
    cfg!(any(debug_assertions, test))
}

fn is_loopback_update_url(value: &str) -> bool {
    let Ok(url) = reqwest::Url::parse(value.trim()) else {
        return false;
    };
    is_loopback_http_url(&url)
}

fn is_loopback_http_url(url: &reqwest::Url) -> bool {
    if !matches!(url.scheme(), "http" | "https")
        || !url.username().is_empty()
        || url.password().is_some()
    {
        return false;
    }
    url.host_str().is_some_and(|host| {
        host.eq_ignore_ascii_case("localhost")
            || host
                .strip_prefix('[')
                .and_then(|value| value.strip_suffix(']'))
                .unwrap_or(host)
                .parse::<std::net::IpAddr>()
                .is_ok_and(|address| address.is_loopback())
    })
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum UpdateRedirectBoundary {
    HttpsOnly,
    LoopbackOnly,
}

fn redirect_boundary_for_start(
    initial_url: &str,
    allow_loopback: bool,
) -> Result<UpdateRedirectBoundary, String> {
    let url = reqwest::Url::parse(initial_url.trim())
        .map_err(|_| "更新 URL 无效，已拒绝请求".to_string())?;
    if allow_loopback && is_loopback_http_url(&url) {
        return Ok(UpdateRedirectBoundary::LoopbackOnly);
    }
    if url.scheme() == "https" && url.host_str().is_some() {
        return Ok(UpdateRedirectBoundary::HttpsOnly);
    }
    Err("更新 URL 必须使用 HTTPS；HTTP 仅允许 debug/test loopback 源".to_string())
}

fn redirect_chain_allowed(
    boundary: UpdateRedirectBoundary,
    previous: &[reqwest::Url],
    next: &reqwest::Url,
) -> bool {
    previous
        .iter()
        .chain(std::iter::once(next))
        .all(|url| match boundary {
            UpdateRedirectBoundary::HttpsOnly => {
                url.scheme() == "https" && url.host_str().is_some()
            }
            UpdateRedirectBoundary::LoopbackOnly => is_loopback_http_url(url),
        })
}

fn update_redirect_policy(boundary: UpdateRedirectBoundary) -> reqwest::redirect::Policy {
    reqwest::redirect::Policy::custom(move |attempt| {
        if attempt.previous().len() > 10 {
            attempt.error("更新请求重定向次数超过上限")
        } else if redirect_chain_allowed(boundary, attempt.previous(), attempt.url()) {
            attempt.follow()
        } else {
            attempt.error("更新请求重定向越过了允许的 HTTPS/loopback 边界")
        }
    })
}

fn update_client_builder(initial_url: &str) -> Result<reqwest::ClientBuilder, String> {
    let boundary = redirect_boundary_for_start(initial_url, allow_loopback_test_urls())?;
    Ok(reqwest::Client::builder().redirect(update_redirect_policy(boundary)))
}

#[derive(Debug, PartialEq, Eq)]
enum DownloadWaitError<E> {
    Cancelled,
    Stalled,
    Request(E),
}

async fn wait_download_step<F, T, E>(
    cancel: &AtomicBool,
    stall_timeout: Duration,
    operation: F,
) -> Result<T, DownloadWaitError<E>>
where
    F: std::future::Future<Output = Result<T, E>>,
{
    let result = tokio::select! {
        biased;
        () = wait_for_download_cancel(cancel) => return Err(DownloadWaitError::Cancelled),
        result = tokio::time::timeout(stall_timeout, operation) => result,
    };
    if cancel.load(Ordering::SeqCst) {
        return Err(DownloadWaitError::Cancelled);
    }
    match result {
        Err(_) => Err(DownloadWaitError::Stalled),
        Ok(Err(error)) => Err(DownloadWaitError::Request(error)),
        Ok(Ok(value)) => Ok(value),
    }
}

async fn wait_for_download_cancel(cancel: &AtomicBool) {
    while !cancel.load(Ordering::SeqCst) {
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
}

/// 下载取消标志。前端 `cancel_download` 置位；等待响应头及每个响应块时都会检查。
/// 进程级单例：同一时刻只允许一个下载在跑（前端 updateDownloading gate 保证）。
static DOWNLOAD_CANCEL: AtomicBool = AtomicBool::new(false);

/// 后端更新操作门禁。前端状态只能改善交互，不能作为并发正确性的依据；下载和安装
/// 共用一个 single-flight，避免两个窗口或重复 invoke 同时改写同一个确定性安装包。
static UPDATE_OPERATION: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

/// check_for_update 的返回值；前端原样传回 download_update。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UpdateInfo {
    pub available: bool,
    pub current_version: String,
    pub latest_version: String,
    pub notes: String,
    pub pub_date: String,
    pub url: String,
    pub sha256: String,
    pub size: u64,
    #[serde(default)]
    pub package_md5: String,
    #[serde(default)]
    pub software_id: String,
    #[serde(default)]
    pub sn: String,
    #[serde(default)]
    pub update_type: String,
    #[serde(default)]
    pub platform: String,
    #[serde(default)]
    pub ota_host: String,
    /// Windows 静态更新由后端为已批准 manifest 资产生成的随机收据。调用方只能原样
    /// 回传；版本、URL、哈希、大小等字段仍由后端批准记录决定。
    #[serde(default)]
    pub approval_receipt: String,
    /// 多平台清单:`{ "macos-universal": PlatformAsset, "linux-arm64": ..., ... }`。
    /// 旧版 latest.json 没这字段 → 空 map → 调用方回退到顶层 url/sha256/size。
    #[serde(default)]
    pub platforms: std::collections::HashMap<String, PlatformAsset>,
}

/// 多平台更新清单的单平台资产。`latest.json` 的 `platforms` map 每个值用这个类型。
/// 客户端按平台 key（如 `macos-universal`、`linux-arm64`）选自己平台的资产;缺失则回退到顶层 url/sha256/size。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct PlatformAsset {
    pub url: String,
    #[serde(default)]
    pub format: String,
    #[serde(default)]
    pub sha256: String,
    #[serde(default)]
    pub size: u64,
    #[serde(default)]
    pub restart_after_install: bool,
    #[serde(default)]
    pub notes: String,
    #[serde(default)]
    pub pub_date: String,
    /// 本平台资产自己的版本号。各平台独立发版(Mac 先发 0.7.0、Linux 还在 0.6.3)时,
    /// 客户端用**本平台**版本判要不要升级,而不是读顶层 .version —— 后者代表「最近一次
    /// Linux 发版」,Mac 客户端读它会被误导。旧 manifest / 旧 platform 条目无此字段 → 空
    /// 串 → 调用方回退到顶层 version(向后兼容)。
    #[serde(default)]
    pub version: String,
}

#[derive(Debug, Clone, Serialize)]
#[serde(untagged)]
pub enum DownloadUpdateResult {
    #[allow(dead_code)]
    Path(String),
    Prepared(PreparedUpdate),
}

#[derive(Debug, Clone, Serialize)]
pub struct PreparedUpdate {
    pub package_path: String,
    pub installer_path: String,
    pub latest_version: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct PendingUpdateReportResult {
    pub had_pending: bool,
    pub reported: bool,
    pub result: String,
    pub message: String,
}
pub fn get_app_version() -> String {
    env!("CARGO_PKG_VERSION").to_string()
}

/// 拼 OTA 平台 key,与 `latest.json` 的 `platforms` map key 对齐。
/// - macOS 固定返回 `"macos-universal"`:Mac 走 Universal 二进制,单包同时覆盖 arm64 / x86_64,
///   manifest 里不再按 arch 分条目。
/// - 其它平台仍按 `"{os}-{arch}"`,arch 归一:aarch64→arm64、x86_64→x86_64、其它原样。
pub fn build_platform_key() -> String {
    let os = std::env::consts::OS;
    match os {
        "macos" => "macos-universal".to_string(),
        _ => {
            let arch = match std::env::consts::ARCH {
                "aarch64" => "arm64",
                "x86_64" => "x86_64",
                other => other,
            };
            format!("{}-{}", os, arch)
        }
    }
}

/// 拉 latest.json 与当前版本比较。网络失败返回 Err——启动静默检查由前端吞掉，
/// 手动检查才展示错误。失败提示是给用户的短文案，原始原因在此记入日志
/// (开发期 stdout,打包构建进 app log),诊断轨迹走日志。
pub async fn check_for_update() -> Result<UpdateInfo, String> {
    let _operation = UPDATE_OPERATION
        .try_lock()
        .map_err(|_| "已有更新检查、下载或安装正在进行".to_string())?;
    let current = env!("CARGO_PKG_VERSION");
    let manifest_url = manifest_url();
    let client = update_client_builder(&manifest_url)?
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| {
            log::warn!("[updater] update check failed: {e}");
            format!("HTTP client 构建失败: {e}")
        })?;
    platform::check_for_update_info(&client, current, &manifest_url)
        .await
        .inspect_err(|e| log::warn!("[updater] update check failed: {e}"))
}

/// 下载更新包到 `~/.pinvou3/updates/`，流式写盘 + 校验，进度走
/// `update:progress` 事件。Linux 返回 deb 路径字符串；Windows 返回
/// 已校验的 NSIS/MSI 安装器信息对象。
pub async fn download_update(
    info: UpdateInfo,
    app: AppHandle,
) -> Result<DownloadUpdateResult, String> {
    let _operation = UPDATE_OPERATION
        .try_lock()
        .map_err(|_| "已有更新下载或安装正在进行".to_string())?;
    // 必须在拿到 single-flight 后、发起请求前重置。下载实现不得在响应到达后再次
    // 清零，否则会让等待响应期间发生的取消被晚到响应覆盖。
    DOWNLOAD_CANCEL.store(false, Ordering::SeqCst);
    platform::download_update_package(&info, app, &DOWNLOAD_CANCEL, DOWNLOAD_STALL_TIMEOUT)
        .await
        .inspect_err(|e| log::warn!("[updater] download failed: {e}"))
}

/// 安装下载好的更新包。Linux 走 pkexec apt；Windows 启动 MSI/NSIS 安装器，成功启动后退出进程。
pub async fn install_update(
    deb_path: Option<String>,
    installer_path: Option<String>,
    info: Option<UpdateInfo>,
    app: AppHandle,
) -> Result<(), String> {
    let _operation = UPDATE_OPERATION
        .try_lock()
        .map_err(|_| "已有更新下载或安装正在进行".to_string())?;
    let pending_target = info.as_ref().map(|value| value.latest_version.clone());
    let exit_after_start = tokio::task::spawn_blocking(move || {
        let result = platform::install_downloaded_update(deb_path, installer_path, info);
        if result.is_ok()
            && let Some(target_version) = pending_target
            && let Err(error) = health::record_pending_update(
                env!("CARGO_PKG_VERSION"),
                &target_version,
                std::env::consts::OS,
            )
        {
            // Installation has already crossed the platform boundary. Reporting
            // failure here would leave the old process running against replaced
            // files, so preserve the successful install result and surface only
            // the diagnostic. First-commit cleanup remains safe without a marker.
            log::warn!("[updater] installed update but could not persist health marker: {error}");
        }
        result
    })
    .await
    .map_err(|e| {
        log::warn!("[updater] install task failed: {e}");
        format!("install task failed: {e}")
    })?
    .inspect_err(|e| log::warn!("[updater] install failed: {e}"))?;
    if exit_after_start {
        app.exit(0);
    }
    Ok(())
}

pub async fn commit_pending_update_ready(
    window: tauri::WebviewWindow,
) -> Result<UpdateReadyCommitResult, String> {
    if window.label() != "main" {
        return Ok(UpdateReadyCommitResult::ignored_window());
    }
    tokio::task::spawn_blocking(|| {
        health::commit_ready(
            env!("CARGO_PKG_VERSION"),
            std::env::consts::OS,
            platform::cleanup_stale_backup,
        )
    })
    .await
    .map_err(|error| format!("更新 ready 提交任务失败: {error}"))?
}

/// 重启应用使新版本生效（exec 新 inode）。restart 跳过 RunEvent::Exit，
/// Synchronously close the browser host and reap ACP/connector child processes first so
/// native surfaces and orphan processes do not remain resident.
pub async fn restart_app(app: AppHandle) -> Result<(), String> {
    crate::prepare_app_restart(&app).await;
    app.restart();
}

/// 置位取消标志，让正在跑的 `download_update` 循环下一轮自行退出并清理半成品。
/// 仅对下载阶段有效；install 阶段(pkexec/apt)已交给系统，不在此中断。
pub fn cancel_download() {
    DOWNLOAD_CANCEL.store(true, Ordering::SeqCst);
}

/// Windows OTA 安装后反馈升级结果；其他平台无待反馈记录时静默成功。
pub async fn report_pending_update_result() -> Result<PendingUpdateReportResult, String> {
    let client = update_client_builder(UPDATE_MANIFEST_URL)?
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("failed to build the HTTP client: {e}"))?;
    platform::report_pending_update_result_info(&client, env!("CARGO_PKG_VERSION")).await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn build_platform_key_matches_expected() {
        let key = build_platform_key();
        if std::env::consts::OS == "linux" {
            assert!(
                key == "linux-arm64" || key == "linux-x86_64",
                "unexpected linux platform key: {key}"
            );
        }
        if std::env::consts::OS == "macos" {
            assert!(
                key == "macos-universal",
                "unexpected macos platform key: {key}"
            );
        }
        if std::env::consts::OS == "windows" {
            assert!(
                key == "windows-x86_64" || key == "windows-arm64",
                "unexpected windows platform key: {key}"
            );
        }
    }

    #[test]
    fn update_info_platforms_defaults_empty() {
        // 旧版 latest.json(无 platforms 字段)反序列化应得空 map,不报错。
        let json = r#"{
            "available": true,
            "current_version": "0.1.0",
            "latest_version": "0.2.0",
            "notes": "",
            "pub_date": "",
            "url": "https://example.com/p.pkg",
            "sha256": "abc",
            "size": 0
        }"#;
        let info: UpdateInfo = serde_json::from_str(json).unwrap();
        assert!(info.platforms.is_empty());
    }

    #[test]
    fn platform_asset_roundtrip() {
        let asset = PlatformAsset {
            url: "https://example.com/m.dmg".to_string(),
            format: "dmg".to_string(),
            sha256: "deadbeef".to_string(),
            size: 1024,
            restart_after_install: false,
            notes: "mac only".to_string(),
            pub_date: "2026-09-22T08:00:00Z".to_string(),
            version: "0.7.0".to_string(),
        };
        let json = serde_json::to_string(&asset).unwrap();
        let back: PlatformAsset = serde_json::from_str(&json).unwrap();
        assert_eq!(back.url, asset.url);
        assert_eq!(back.size, 1024);
        assert_eq!(back.format, "dmg");
        assert_eq!(back.version, "0.7.0");
    }

    #[test]
    fn update_url_override_only_accepts_loopback_urls() {
        assert!(is_loopback_update_url("http://127.0.0.1:8787/latest.json"));
        assert!(is_loopback_update_url("http://[::1]:8787/latest.json"));
        assert!(is_loopback_update_url("https://localhost/latest.json"));
        assert!(!is_loopback_update_url(
            "http://[2001:db8::1]:8787/latest.json"
        ));
        assert!(!is_loopback_update_url(
            "https://localhost.evil/latest.json"
        ));
        assert!(!is_loopback_update_url("http://pinvou.com/latest.json"));
        assert!(!is_loopback_update_url("file:///tmp/latest.json"));
        assert_eq!(
            UPDATE_MANIFEST_URL,
            "https://pinvou.com/pinvou3/latest.json"
        );
    }

    #[test]
    fn release_redirect_boundary_rejects_https_downgrade_and_mixed_chain() {
        let boundary =
            redirect_boundary_for_start("https://pinvou.com/pinvou3/latest.json", false).unwrap();
        let https_start = reqwest::Url::parse("https://pinvou.com/pinvou3/latest.json").unwrap();
        let http_hop = reqwest::Url::parse("http://cdn.example.com/update").unwrap();
        let https_end = reqwest::Url::parse("https://cdn.example.com/update").unwrap();

        assert!(!redirect_chain_allowed(
            boundary,
            std::slice::from_ref(&https_start),
            &http_hop,
        ));
        assert!(!redirect_chain_allowed(
            boundary,
            &[https_start, http_hop],
            &https_end,
        ));
        assert!(redirect_boundary_for_start("http://pinvou.com/latest.json", false).is_err());
    }

    #[test]
    fn loopback_redirect_boundary_allows_only_loopback_http_or_https() {
        let boundary =
            redirect_boundary_for_start("http://127.0.0.1:8787/latest.json", true).unwrap();
        assert_eq!(boundary, UpdateRedirectBoundary::LoopbackOnly);
        let http_start = reqwest::Url::parse("http://127.0.0.1:8787/latest.json").unwrap();
        let https_hop = reqwest::Url::parse("https://localhost:9443/asset.exe").unwrap();
        let http_end = reqwest::Url::parse("http://[::1]:8787/asset.exe").unwrap();
        assert!(redirect_chain_allowed(
            boundary,
            &[http_start.clone(), https_hop],
            &http_end,
        ));

        let external = reqwest::Url::parse("https://example.com/asset.exe").unwrap();
        assert!(!redirect_chain_allowed(
            boundary,
            std::slice::from_ref(&http_start),
            &external,
        ));
        assert!(!redirect_chain_allowed(
            boundary,
            &[
                http_start,
                reqwest::Url::parse("http://example.com/intermediate").unwrap(),
            ],
            &http_end,
        ));
    }

    #[tokio::test]
    async fn download_wait_can_cancel_while_response_headers_are_pending() {
        let cancel = AtomicBool::new(false);
        let wait = wait_download_step(
            &cancel,
            Duration::from_secs(5),
            std::future::pending::<Result<(), ()>>(),
        );
        let trigger = async {
            tokio::time::sleep(Duration::from_millis(10)).await;
            cancel.store(true, Ordering::SeqCst);
        };
        let (result, ()) = tokio::join!(wait, trigger);
        assert_eq!(result, Err(DownloadWaitError::Cancelled));
    }

    #[tokio::test]
    async fn download_wait_rejects_response_that_arrives_after_cancel() {
        let cancel = AtomicBool::new(false);
        let result = wait_download_step(&cancel, Duration::from_secs(1), async {
            cancel.store(true, Ordering::SeqCst);
            Ok::<_, ()>(())
        })
        .await;
        assert_eq!(result, Err(DownloadWaitError::Cancelled));
    }
}
