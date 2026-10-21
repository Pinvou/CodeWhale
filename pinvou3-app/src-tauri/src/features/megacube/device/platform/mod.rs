#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "linux")]
pub use linux::status;
#[cfg(not(target_os = "linux"))]
pub fn status(_: bool) -> Result<serde_json::Value, String> {
    Ok(serde_json::json!({"supported":false,"detected":false,"reason":"unsupported_platform"}))
}
