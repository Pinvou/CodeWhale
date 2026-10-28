#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "macos")]
mod macos;
#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
mod unsupported;
#[cfg(target_os = "windows")]
mod windows;

#[cfg(target_os = "linux")]
pub use linux::{
    check_for_update_info, cleanup_stale_backup, download_update_package,
    install_downloaded_update, report_pending_update_result_info,
};
#[cfg(target_os = "macos")]
pub use macos::{
    check_for_update_info, cleanup_stale_backup, download_update_package,
    install_downloaded_update, report_pending_update_result_info,
};
#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
pub use unsupported::{
    check_for_update_info, cleanup_stale_backup, download_update_package,
    install_downloaded_update, report_pending_update_result_info,
};
#[cfg(target_os = "windows")]
pub use windows::{
    check_for_update_info, cleanup_stale_backup, download_update_package,
    report_pending_update_result_info,
};

#[cfg(target_os = "windows")]
pub fn install_update_for_operation(
    deb_path: Option<String>,
    installer_path: Option<String>,
    info: Option<super::UpdateInfo>,
    owner: &str,
) -> Result<bool, String> {
    windows::install_downloaded_update(deb_path, installer_path, info, owner)
}

#[cfg(not(target_os = "windows"))]
pub fn install_update_for_operation(
    deb_path: Option<String>,
    installer_path: Option<String>,
    info: Option<super::UpdateInfo>,
    _owner: &str,
) -> Result<bool, String> {
    install_downloaded_update(deb_path, installer_path, info)
}

#[cfg(target_os = "windows")]
pub use windows::{cancel_install_handoff, commit_install_handoff};

#[cfg(not(target_os = "windows"))]
pub fn commit_install_handoff(_owner: &str) -> Result<(), String> {
    Ok(())
}

#[cfg(not(target_os = "windows"))]
pub fn cancel_install_handoff(_owner: &str) {}
