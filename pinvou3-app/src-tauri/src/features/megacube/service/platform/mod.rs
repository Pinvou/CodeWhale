#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "linux")]
pub use linux::{bundled_package, inspect_package, logs, run_worker, status, submit};

#[cfg(not(target_os = "linux"))]
mod unsupported {
    use serde_json::{Value, json};
    pub fn bundled_package() -> Result<Value, String> {
        Ok(json!({"available":false,"reason":"unsupported_platform"}))
    }
    pub fn status() -> Result<Value, String> {
        Ok(json!({"supported":false,"installed":false,"reason":"unsupported_platform"}))
    }
    pub fn inspect_package(_: &str) -> Result<Value, String> {
        Err("unsupported_platform".into())
    }
    pub fn submit(_: &str, _: Option<String>, _: Option<String>) -> Result<Value, String> {
        Err("unsupported_platform".into())
    }
    pub fn run_worker(_: &str) -> Result<(), String> {
        Err("unsupported_platform".into())
    }
    pub fn logs() -> Result<serde_json::Value, String> {
        Err("unsupported_platform".into())
    }
}
#[cfg(not(target_os = "linux"))]
pub use unsupported::{bundled_package, inspect_package, logs, run_worker, status, submit};
