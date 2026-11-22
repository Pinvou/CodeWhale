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

pub(crate) fn matches_current_model_route(_card_id: &str, _base_url: &str, _model: &str) -> bool {
    false
}

impl ModelStoreService {
    pub fn new() -> Self {
        Self
    }

    pub fn set_models_changed_handler(&self, _handler: super::super::ModelsChangedHandler) {}
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

pub async fn library(_service: Arc<ModelStoreService>) -> Result<Value, String> {
    nope()
}

pub async fn preview_incomplete(
    _service: &Arc<ModelStoreService>,
    _run_id: String,
) -> Result<Value, String> {
    nope()
}
pub fn retry_incomplete(
    _service: &Arc<ModelStoreService>,
    _run_id: String,
    _expected_sha256: String,
) -> Result<Value, String> {
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

pub async fn memory_admission(
    _card_id: String,
    _version_id: Option<String>,
) -> Result<Value, String> {
    nope()
}

pub async fn delete_engine(
    _service: &Arc<ModelStoreService>,
    _engine_name: String,
    _engine_version: Option<String>,
) -> Result<Value, String> {
    nope()
}

pub async fn follow_deployment_context(
    _service: Arc<ModelStoreService>,
    _card_id: String,
    _model_id: String,
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

pub async fn check_card_update(_card_id: String) -> Result<Value, String> {
    nope()
}

pub async fn startup_update_check(_service: Arc<ModelStoreService>) -> Result<Value, String> {
    Ok(serde_json::json!({"checked":0,"skipped":0,"updates":[]}))
}

pub async fn versions(_card_id: String, _measure: Option<bool>) -> Result<Value, String> {
    nope()
}
pub fn start_version_action(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
    _action: String,
    _version_id: String,
    _memory_confirmed: bool,
) -> Result<Value, String> {
    nope()
}

pub fn start_card_update(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
    _revision: String,
) -> Result<Value, String> {
    nope()
}

pub fn start_download(
    _service: &Arc<ModelStoreService>,
    _card_id: String,
) -> Result<Value, String> {
    nope()
}

/// 本地卡片安装只在 Linux 上有；桩返回同样的「不支持」。
pub fn start_install_local(
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
    _from_byte: Option<usize>,
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

pub fn set_storage_settings(
    _models_dir: Option<String>,
    _import_scan_dirs: Option<Vec<String>>,
    _startup_update_check_enabled: Option<bool>,
) -> Result<Value, String> {
    nope()
}

/// 非 Linux 上没有脚本进程可收，直接返回。**不报错**：这是启动路径，
/// 一个平台不支持的功能不该让应用启动时冒出一句错误。
pub fn run_startup_cleanup(_service: &Arc<ModelStoreService>) {}

/// 非 Linux 无 linked-models 可对账：no-op 而非报错（同上，启动路径不冒错）。
pub fn run_startup_linked_sync(_service: &Arc<ModelStoreService>) {}

pub fn take_startup_cleanup(_service: &Arc<ModelStoreService>) -> Result<Value, String> {
    nope()
}

impl ModelStoreService {
    pub fn for_desktop() -> Self {
        Self::default()
    }
    pub fn is_remote(&self) -> bool {
        false
    }
}
// ── 商店边界（与 linux/boundary.rs 同名同签名，见 platform/mod.rs 的转发）──

/// 非 Linux 没有商店，恒为未命中（宽松版，登记端用）。
pub(crate) fn store_model_name(_model_name: &str) -> Option<String> {
    None
}

/// 非 Linux 没有商店：查询"成功且未命中"。移除路径的 fail-closed 语义在
/// Linux 侧由 Err 表达；这里没有查询可言，Ok(None) 即可。
pub(crate) fn store_model_name_checked(
    _model_name: &str,
    _case_insensitive: bool,
) -> Result<Option<String>, String> {
    Ok(None)
}

/// 非 Linux 没有商店；保留数据根作为兜底边界（外部模型守卫统一走多根检查）。
pub(crate) fn store_weights_roots() -> Vec<std::path::PathBuf> {
    vec![crate::platform::paths::pinvou3_home().join("model-store")]
}

/// 非 Linux 无 settings 可读失败：恒 Ok（与上方宽松/可失败两版同根集合）。
pub(crate) fn store_weights_roots_checked() -> Result<Vec<std::path::PathBuf>, String> {
    Ok(store_weights_roots())
}

/// There are no store download/stage writers on this backend, hence no store
/// resources to exclude. Preserve the device feature's existing platform guards
/// and development override: this does not enable the unsupported model store,
/// nor borrow a Linux lock implementation for another host.
pub(crate) fn with_weight_removal_boundary<T>(
    operation: impl FnOnce() -> Result<T, String>,
) -> Result<T, String> {
    operation()
}

#[cfg(test)]
mod weight_removal_boundary_tests {
    #[test]
    fn absent_store_writers_do_not_disable_existing_device_operations() {
        let called = std::cell::Cell::new(0);
        let result = super::with_weight_removal_boundary(|| {
            called.set(called.get() + 1);
            Ok("device operation result")
        })
        .unwrap();
        assert_eq!(result, "device operation result");
        assert_eq!(called.get(), 1);
    }

    #[test]
    fn device_guard_errors_remain_authoritative_without_store_writers() {
        let result = super::with_weight_removal_boundary(|| {
            Err::<(), _>("original device path/identity guard failure".to_owned())
        });
        assert_eq!(
            result.unwrap_err(),
            "original device path/identity guard failure"
        );
    }
}

/// 非 Linux 没有商店：空引用集合（设备守卫的该臂恒不命中）。
pub(crate) fn installed_weight_paths_checked() -> Result<Vec<std::path::PathBuf>, String> {
    Ok(Vec::new())
}

pub(crate) fn installed_weight_paths_for_removal_checked(
    _include_persisted_roots: bool,
) -> Result<Vec<std::path::PathBuf>, String> {
    installed_weight_paths_checked()
}

/// 非 Linux 没有商店：空约定名集合（设备守卫的零痕迹旧根名字臂恒不命中）。
pub(crate) fn installed_weight_candidate_names_checked() -> Result<Vec<String>, String> {
    Ok(Vec::new())
}

/// 非 Linux 没有商店：空标记集合（设备守卫的就地导入原目录标记臂恒不命中）。
pub(crate) fn installed_import_markers_checked() -> Result<Vec<String>, String> {
    Ok(Vec::new())
}

/// 非 Linux 没有商店：空权重文件名集合（设备守卫的第四恢复臂恒不命中）。
pub(crate) fn installed_weight_filenames_checked() -> Result<Vec<String>, String> {
    Ok(Vec::new())
}

/// 非 Linux 没有商店：不存在在册卡片，归属恒可确认（设备守卫的第五臂恒不命中）。
pub(crate) fn installed_ownership_unconfirmable_checked() -> Result<bool, String> {
    Ok(false)
}

pub(crate) fn ollama_base_url() -> String {
    "http://127.0.0.1:11434".to_string()
}

/// 非 Linux 无 settings 可读失败：恒 Ok 默认地址（与可失败版同口径）。
pub(crate) fn ollama_base_url_checked() -> Result<String, String> {
    Ok(ollama_base_url())
}
