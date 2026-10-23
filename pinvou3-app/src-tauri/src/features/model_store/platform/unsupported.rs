//! 模型商店在非 Linux 平台的桩。
//!
//! 与 `linux/` 下的实现**同名同签名**，一律回「本平台不支持」。这样命令层不必
//! 分支、前端 invoke 也不用改：Windows / macOS 上 `available()` 回 false，侧栏
//! 入口据此整个不渲染。
//!
//! 不做成「编译期就没有这些命令」的原因：那样前端 invoke 会拿到一个
//! "command not found" 的底层错误，界面只能显示一句莫名其妙的话；而
//! 「这个功能只在 Linux 设备上有」是一句用户能看懂的解释。

use std::sync::Arc;

use serde_json::Value;

use super::super::UNSUPPORTED_REASON;

/// 占位的服务状态：非 Linux 上没有任何东西要管，但命令层要能一致地注册它。
#[derive(Default)]
pub struct ModelStoreService;

impl ModelStoreService {
    pub fn new() -> Self {
        Self
    }
}

pub(crate) fn available() -> bool {
    false
}

pub(crate) fn unavailable_reason() -> Option<String> {
    Some(UNSUPPORTED_REASON.to_string())
}

fn nope<T>() -> Result<T, String> {
    Err(UNSUPPORTED_REASON.to_string())
}

pub async fn list_cards() -> Result<Value, String> {
    nope()
}

/// 打开运行日志目录。非 Linux 上压根没有运行过任何卡片脚本，所以也没有这个目录。
pub fn reveal_run_logs_dir() -> Result<Value, String> {
    nope()
}

pub fn export_run_log(_run_id: String) -> Result<Value, String> {
    nope()
}

pub async fn library() -> Result<Value, String> {
    nope()
}

pub async fn model_detail(_card_id: String) -> Result<Value, String> {
    nope()
}

pub async fn installed_engines() -> Result<Value, String> {
    nope()
}

pub async fn local_resources() -> Result<Value, String> {
    nope()
}

pub async fn memory_admission(_card_id: String) -> Result<Value, String> {
    nope()
}

pub async fn delete_engine(
    _service: &Arc<ModelStoreService>,
    _engine_name: String,
    _engine_version: Option<String>,
) -> Result<Value, String> {
    nope()
}

pub fn start_card_scripts(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
    _confirmed: bool,
) -> Result<Value, String> {
    nope()
}

pub fn start_download(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
) -> Result<Value, String> {
    nope()
}

pub async fn start_deploy(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
    _profile_id: Option<String>,
    _memory_confirmed: bool,
) -> Result<Value, String> {
    nope()
}

pub async fn start_lifecycle(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
    _action: String,
    _purge_weights: bool,
    _memory_confirmed: bool,
) -> Result<Value, String> {
    nope()
}

pub async fn run_log(
    _service: Arc<ModelStoreService>,
    _run_id: String,
    _from: usize,
    _wait: Option<u64>,
) -> Result<Value, String> {
    nope()
}

pub fn cancel_run(_service: &Arc<ModelStoreService>, _run_id: String) -> Result<Value, String> {
    nope()
}

pub fn active_runs(
    _service: &Arc<ModelStoreService>,
    _card_id: Option<String>,
) -> Result<Value, String> {
    nope()
}

pub fn answer_run(
    _service: &Arc<ModelStoreService>,
    _run_id: String,
    _answer: Option<String>,
) -> Result<Value, String> {
    nope()
}

pub fn api_keys(_card_id: String) -> Result<Value, String> {
    nope()
}

pub fn create_api_key(_card_id: String, _label: Option<String>) -> Result<Value, String> {
    nope()
}

pub async fn lan_status(_service: &Arc<ModelStoreService>, _card: String) -> Result<Value, String> {
    nope()
}
pub async fn set_lan_sharing(
    _service: &Arc<ModelStoreService>,
    _card: String,
    _enabled: bool,
    _address: Option<String>,
    _port: u16,
) -> Result<Value, String> {
    nope()
}
pub fn revoke_api_key(_id: String) -> Result<Value, String> {
    nope()
}

pub fn force_remove(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
    _confirm: bool,
) -> Result<Value, String> {
    nope()
}

pub fn storage_settings() -> Result<Value, String> {
    nope()
}

/// 模块商店在非 Linux 平台没有 config 模块；顶层桥接函数需要一个路径出口。
pub mod config {
    pub fn data_dir() -> std::path::PathBuf {
        std::path::PathBuf::new()
    }
}

pub fn set_auto_start(_card_id: String, _enabled: bool) -> Result<Value, String> {
    nope()
}

pub async fn autostart_pending(_service: std::sync::Arc<ModelStoreService>) -> Value {
    Value::Null
}

pub fn take_autostart_report() -> Result<Value, String> {
    nope()
}

pub fn set_storage_settings(
    _models_dir: Option<String>,
    _import_scan_dirs: Option<Vec<String>>,
) -> Result<Value, String> {
    nope()
}

/// 非 Linux 上没有脚本进程可收，直接返回。**不报错**：这是启动路径，
/// 一个平台不支持的功能不该让应用启动时冒出一句错误。
pub fn run_startup_cleanup(_service: &Arc<ModelStoreService>) {}

pub fn take_startup_cleanup(_service: &Arc<ModelStoreService>) -> Result<Value, String> {
    nope()
}
