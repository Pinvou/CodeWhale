//! 连接器按需安装的平台适配：目标 lock、可执行文件命名和权限。
//!
//! lock 表与可执行文件命名已下沉为跨功能原语 `crate::platform::connector_lock`
//! （marketplace 首启导入也要对照 lock 表验存量二进制）；此处保留委托，
//! 既有调用方（native_installer）零改动。Official 的 H3C 内部 CLI 路径适配
//! 继续保留在功能内的平台模块中。

use std::path::Path;

#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "macos")]
mod macos;
#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
mod unsupported;
#[cfg(target_os = "windows")]
mod windows;

#[cfg(target_os = "linux")]
pub(super) use linux::{eip_bin_path, zhidao_bin_path};
#[cfg(target_os = "macos")]
pub(super) use macos::{eip_bin_path, zhidao_bin_path};
#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
pub(super) use unsupported::{eip_bin_path, zhidao_bin_path};
#[cfg(target_os = "windows")]
pub(super) use windows::{eip_bin_path, zhidao_bin_path};
pub fn lock_json() -> &'static str {
    crate::platform::connector_lock::lock_json()
}

pub fn executable_name(name: &str) -> String {
    crate::platform::connector_lock::executable_name(name)
}

pub fn archive_member(name: &str) -> &'static str {
    match name {
        "dws" => {
            if cfg!(windows) {
                "dws.exe"
            } else {
                "dws"
            }
        }
        "lark-cli" => {
            if cfg!(windows) {
                "lark-cli.exe"
            } else {
                "lark-cli"
            }
        }
        "wecom-cli" => {
            if cfg!(windows) {
                "package/bin/wecom-cli.exe"
            } else {
                "package/bin/wecom-cli"
            }
        }
        _ => "",
    }
}

pub fn set_executable_permissions(path: &Path) -> std::io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755))?;
    }
    #[cfg(not(unix))]
    let _ = path;
    Ok(())
}
