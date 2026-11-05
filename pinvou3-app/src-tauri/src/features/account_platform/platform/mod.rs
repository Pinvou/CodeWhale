//! Platform capability boundary for desktop OIDC.

#[cfg(any(target_os = "windows", target_os = "macos", target_os = "linux"))]
pub const fn desktop_oidc_supported() -> bool {
    true
}

#[cfg(not(any(target_os = "windows", target_os = "macos", target_os = "linux")))]
pub const fn desktop_oidc_supported() -> bool {
    false
}
