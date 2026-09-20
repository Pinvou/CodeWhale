//! 模型商店的 Tauri 命令层。
//!
//! 这一层**只做搬运**：取 managed state、转发参数、把结果原样回给前端。所有判断
//! 都在 `features::model_store` 里，平台差异收在它的 `platform/`——所以这里没有
//! 一个 `#[cfg]`。
//!
//! 命名一律 `model_store_` 前缀，与前端 `model-store-api.js` 一一对应。
//!
//! # 为什么长操作是「立刻回 run_id + 长轮询日志」而不是事件推送
//!
//! 游标在**调用方**手里（`model_store_run_log(run_id, from, wait)`），所以界面切走
//! 再切回来、甚至应用重启之后重新挂上同一个 run_id，都不丢行也不重复。事件推送
//! 做不到这件事：订阅者晚到一步，前面的行就永远错过了——而出错原因往往正在前半段。
//!
//! `wait` 让服务端在没有新行时最多挂 25 秒，所以前端的轮询循环不是忙等。

use std::sync::Arc;

use serde_json::Value;
use tauri::State;

use crate::features::model_store::{self, ModelStoreService, StoreAvailability};

/// 这台机器上有没有模型商店。前端拿它决定侧栏入口渲不渲染。
///
/// 形状刻意跟其余命令保持一致（`async` + `Result`），尽管这个查询既不会失败也不
/// 需要异步。原因是排查过一个现象：它曾是全组唯一一个「同步 + 非 Result」的命令，
/// 而它的名字是这一组里**唯一一个没有出现在编译产物里**的——十五个都在，只少它。
/// 源码、注册、宏属性逐项核对都没问题。与其留着一个解释不清的差异，不如把它抹平。
#[tauri::command]
pub async fn model_store_availability() -> Result<StoreAvailability, String> {
    Ok(model_store::availability())
}

// ---------------------------------------------------------------- 只读查询

#[tauri::command]
pub async fn model_store_cards(test_mode: Option<bool>) -> Result<Value, String> {
    model_store::list_cards(test_mode.unwrap_or(false)).await
}

#[tauri::command]
pub async fn model_store_library() -> Result<Value, String> {
    model_store::library().await
}

#[tauri::command]
pub async fn model_store_model_detail(card_id: String) -> Result<Value, String> {
    model_store::model_detail(card_id).await
}

// ---------------------------------------------------------------- 长操作

#[tauri::command]
pub fn model_store_start_download(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    test_mode: Option<bool>,
) -> Result<Value, String> {
    model_store::start_download(&service, card_id, test_mode.unwrap_or(false))
}

#[tauri::command]
pub fn model_store_start_deploy(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    profile_id: Option<String>,
) -> Result<Value, String> {
    model_store::start_deploy(&service, card_id, profile_id)
}

/// 启用 / 停用 / 卸载。
///
/// `purge_weights` 只对卸载有意义，而且**缺省是保留**——不可逆的那一侧绝不能靠
/// 参数缺席命中。「卸载」保留权重并把卡片退回「已下载」，「删除」才连权重一起清掉
/// 并把卡片移出模型库。
#[tauri::command]
pub fn model_store_start_lifecycle(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    action: String,
    purge_weights: Option<bool>,
) -> Result<Value, String> {
    model_store::start_lifecycle(&service, card_id, action, purge_weights.unwrap_or(false))
}

#[tauri::command]
pub fn model_store_force_remove(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    confirm: Option<bool>,
) -> Result<Value, String> {
    model_store::force_remove(&service, card_id, confirm.unwrap_or(false))
}

// ---------------------------------------------------------------- 运行日志

#[tauri::command]
pub async fn model_store_run_log(
    service: State<'_, Arc<ModelStoreService>>,
    run_id: String,
    from: usize,
    wait: Option<u64>,
) -> Result<Value, String> {
    // 克隆 Arc 再进 await：State 的借用不能跨过 await 点。
    let service = Arc::clone(&service);
    model_store::run_log(service, run_id, from, wait).await
}

#[tauri::command]
pub fn model_store_cancel_run(
    service: State<'_, Arc<ModelStoreService>>,
    run_id: String,
) -> Result<Value, String> {
    model_store::cancel_run(&service, run_id)
}

#[tauri::command]
pub fn model_store_answer_run(
    service: State<'_, Arc<ModelStoreService>>,
    run_id: String,
    answer: Option<String>,
) -> Result<Value, String> {
    model_store::answer_run(&service, run_id, answer)
}

// ---------------------------------------------------------------- API Key

#[tauri::command]
pub fn model_store_api_keys(card_id: String) -> Result<Value, String> {
    model_store::api_keys(card_id)
}

#[tauri::command]
pub fn model_store_create_api_key(
    card_id: String,
    label: Option<String>,
) -> Result<Value, String> {
    model_store::create_api_key(card_id, label)
}

// ---------------------------------------------------------------- 测试商店

#[tauri::command]
pub fn model_store_test_store_state() -> Result<Value, String> {
    model_store::test_store_state()
}

/// 口令错**不是**错误：回 `{"ok": false}`，让前端能区分「输错了」和「保存失败」。
/// 都压成 Err 的话这两件事在界面上长得一模一样。
#[tauri::command]
pub fn model_store_unlock_test_store(password: String) -> Result<Value, String> {
    model_store::unlock_test_store(password)
}

#[tauri::command]
pub fn model_store_disable_test_store() -> Result<Value, String> {
    model_store::disable_test_store()
}

// ------------------------------------------------------ 权重目录与导入扫描

/// 权重目录、用户补充的扫描目录，以及**实际会扫的完整目录列表**。
///
/// 最后一项是重点：用户问的是"下载前到底找了哪些地方"，只回一个可编辑的补充
/// 列表答不了——内置位置和当前挂着的 U 盘都不在那个列表里。
#[tauri::command]
pub fn model_store_storage_settings() -> Result<Value, String> {
    model_store::storage_settings()
}

/// 传 `null` 的那项不动。改目录**不搬运已下载的权重**，界面上要讲清楚。
#[tauri::command]
pub fn model_store_set_storage_settings(
    models_dir: Option<String>,
    import_scan_dirs: Option<Vec<String>>,
) -> Result<Value, String> {
    model_store::set_storage_settings(models_dir, import_scan_dirs)
}
