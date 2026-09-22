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

pub(crate) fn available() -> bool {
    false
}

pub(crate) fn unavailable_reason() -> Option<String> {
    Some(UNSUPPORTED_REASON.to_string())
}

fn nope<T>() -> Result<T, String> {
    Err(UNSUPPORTED_REASON.to_string())
}

pub async fn list_cards(_test_mode: bool) -> Result<Value, String> {
    nope()
}

pub async fn library() -> Result<Value, String> {
    nope()
}

pub async fn model_detail(_card_id: String) -> Result<Value, String> {
    nope()
}

pub fn start_download(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
    _test_mode: bool,
) -> Result<Value, String> {
    nope()
}

pub fn start_deploy(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
    _profile_id: Option<String>,
) -> Result<Value, String> {
    nope()
}

pub fn start_lifecycle(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
    _action: String,
    _purge_weights: bool,
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

pub fn test_store_state() -> Result<Value, String> {
    nope()
}

pub fn unlock_test_store(_password: String) -> Result<Value, String> {
    nope()
}

pub fn disable_test_store() -> Result<Value, String> {
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

pub fn set_storage_settings(
    _models_dir: Option<String>,
    _import_scan_dirs: Option<Vec<String>>,
) -> Result<Value, String> {
    nope()
}

/// 非 Linux 上没有脚本进程可收，直接返回。**不报错**：这是启动路径，
/// 一个平台不支持的功能不该让应用启动时冒出一句错误。
pub fn run_startup_cleanup(_service: &Arc<ModelStoreService>) {}

/// 非 Linux 上没有内置卡片库可刷，直接返回。同样不报错——这是启动路径。
pub fn run_startup_refresh() {}

pub fn take_startup_cleanup(_service: &Arc<ModelStoreService>) -> Result<Value, String> {
    nope()
}
