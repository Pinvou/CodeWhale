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

/// Store operations can wait for a background host and its IPC response. Keep
/// every invocation off the WebView/runtime thread and dispatch it only once.
async fn dispatch_store<F>(job: F) -> Result<Value, String>
where
    F: FnOnce() -> Result<Value, String> + Send + 'static,
{
    tokio::task::spawn_blocking(job)
        .await
        .map_err(|error| error.to_string())?
}

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
pub async fn model_store_cards() -> Result<Value, String> {
    model_store::list_cards().await
}

#[tauri::command]
pub async fn model_store_library(
    service: State<'_, Arc<ModelStoreService>>,
) -> Result<Value, String> {
    model_store::library(Arc::clone(&service)).await
}

#[tauri::command]
pub async fn model_store_preview_incomplete(
    service: State<'_, Arc<ModelStoreService>>,
    run_id: String,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    model_store::preview_incomplete(&service, run_id).await
}

#[tauri::command]
pub async fn model_store_retry_incomplete(
    service: State<'_, Arc<ModelStoreService>>,
    run_id: String,
    expected_sha256: String,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    dispatch_store(move || model_store::retry_incomplete(&service, run_id, expected_sha256)).await
}

#[tauri::command]
pub async fn model_store_dismiss_incomplete(
    service: State<'_, Arc<ModelStoreService>>,
    run_id: String,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    model_store::dismiss_incomplete(&service, run_id).await
}

#[tauri::command]
pub async fn model_store_scan_orphans(
    service: State<'_, Arc<ModelStoreService>>,
) -> Result<Value, String> {
    model_store::scan_orphans(&service).await
}

#[tauri::command]
pub async fn model_store_remove_orphan(
    service: State<'_, Arc<ModelStoreService>>,
    path: String,
) -> Result<Value, String> {
    model_store::remove_orphan(&service, path).await
}

#[tauri::command]
pub async fn model_store_model_detail(card_id: String) -> Result<Value, String> {
    model_store::model_detail(card_id).await
}

#[tauri::command]
pub async fn model_store_installed_engines() -> Result<Value, String> {
    model_store::installed_engines().await
}

#[tauri::command]
pub async fn model_store_local_resources() -> Result<Value, String> {
    model_store::local_resources().await
}

#[tauri::command]
pub async fn model_store_memory_admission(
    card_id: String,
    version_id: Option<String>,
) -> Result<Value, String> {
    model_store::memory_admission(card_id, version_id).await
}

#[tauri::command]
pub async fn model_store_delete_engine(
    service: State<'_, Arc<ModelStoreService>>,
    engine_name: String,
    engine_version: Option<String>,
) -> Result<Value, String> {
    model_store::delete_engine(&service, engine_name, engine_version).await
}

#[tauri::command]
pub async fn model_store_follow_deployment_context(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    model_id: String,
) -> Result<Value, String> {
    model_store::follow_deployment_context(Arc::clone(&service), card_id, model_id).await
}

#[tauri::command]
pub async fn model_store_check_card_update(card_id: String) -> Result<Value, String> {
    model_store::check_card_update(card_id).await
}

#[tauri::command]
pub async fn model_store_startup_update_check(
    service: State<'_, Arc<ModelStoreService>>,
) -> Result<Value, String> {
    model_store::startup_update_check(Arc::clone(service.inner())).await
}

/// `measure=true` 时逐行重测目录大小并登记（评审第 7 轮：展示与配额读登记
/// 值，不每次全树遍历；显式测量是失效策略的用户入口）。
#[tauri::command]
pub async fn model_store_versions(card_id: String, measure: Option<bool>) -> Result<Value, String> {
    model_store::versions(card_id, measure).await
}
#[tauri::command]
pub async fn model_store_start_version_action(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    action: String,
    version_id: String,
    memory_confirmed: bool,
) -> Result<Value, String> {
    // r43 P2：switch 的内存准入带 Docker/进程探测（prepare_model_start 内联
    // reconcile+采样），同步命令会把 UI 线程卡住数秒——改走与 start_deploy
    // 相同的 spawn_blocking 形态（本文件统一走 dispatch_store 包装）。
    let service = Arc::clone(&service);
    dispatch_store(move || {
        model_store::start_version_action(&service, card_id, action, version_id, memory_confirmed)
    })
    .await
}

#[tauri::command]
pub async fn model_store_start_card_update(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    revision: String,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    dispatch_store(move || model_store::start_card_update(&service, card_id, revision)).await
}

// ---------------------------------------------------------------- 长操作

#[tauri::command]
pub async fn model_store_start_card_scripts(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    confirmed: Option<bool>,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    dispatch_store(move || {
        model_store::start_card_scripts(&service, card_id, confirmed.unwrap_or(false))
    })
    .await
}

#[tauri::command]
pub async fn model_store_start_download(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    dispatch_store(move || model_store::start_download(&service, card_id)).await
}

#[tauri::command]
pub async fn model_store_start_deploy(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    profile_id: Option<String>,
    memory_confirmed: Option<bool>,
) -> Result<Value, String> {
    model_store::start_deploy(
        &service,
        card_id,
        profile_id,
        memory_confirmed.unwrap_or(false),
    )
    .await
}

/// 启用 / 停用 / 卸载。
///
/// `purge_weights` 只对卸载有意义，而且**缺省是保留**——不可逆的那一侧绝不能靠
/// 参数缺席命中。「卸载」保留权重并把卡片退回「已下载」，「删除」才连权重一起清掉
/// 并把卡片移出模型库。
#[tauri::command]
pub async fn model_store_start_lifecycle(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    action: String,
    purge_weights: Option<bool>,
    memory_confirmed: Option<bool>,
) -> Result<Value, String> {
    model_store::start_lifecycle(
        &service,
        card_id,
        action,
        purge_weights.unwrap_or(false),
        memory_confirmed.unwrap_or(false),
    )
    .await
}

#[tauri::command]
pub async fn model_store_force_remove(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    confirm: Option<bool>,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    dispatch_store(move || model_store::force_remove(&service, card_id, confirm.unwrap_or(false)))
        .await
}

// ---------------------------------------------------------------- 运行日志

#[tauri::command]
pub async fn model_store_run_log(
    service: State<'_, Arc<ModelStoreService>>,
    run_id: String,
    from: usize,
    wait: Option<u64>,
    from_byte: Option<usize>,
) -> Result<Value, String> {
    // 克隆 Arc 再进 await：State 的借用不能跨过 await 点。
    let service = Arc::clone(&service);
    model_store::run_log(service, run_id, from, wait, from_byte).await
}

/// 在文件管理器里打开模型商店的运行日志目录（`~/.pinvou3/model-store/logs/`）。
///
/// 只"打开目录"、不"把日志读回前端"：那几 MB 文本走 IPC 再塞进一个浮动任务卡没有
/// 任何意义，用户真正要做的是把文件发给别人。
#[tauri::command]
pub fn model_store_reveal_run_logs_dir() -> Result<Value, String> {
    model_store::reveal_run_logs_dir()
}

/// 导出一次运行的日志。导出那份比落盘那份多过一道脱敏（内网地址 + 全仓通用的
/// 凭证尺子）——它会离开这台机器。
/// r26 P2-4：4MiB 上限全文件逐行脱敏 + snapshot IPC 回传从主线程挪到
/// spawn_blocking——同步版最坏 ~0.3-1.5s UI 冻结（files.rs:269 先例同款）。
#[tauri::command]
pub async fn model_store_export_run_log(run_id: String) -> Result<Value, String> {
    tokio::task::spawn_blocking(move || model_store::export_run_log(run_id))
        .await
        .map_err(|e| e.to_string())?
}

#[tauri::command]
pub async fn model_store_cancel_run(
    service: State<'_, Arc<ModelStoreService>>,
    run_id: String,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    dispatch_store(move || model_store::cancel_run(&service, run_id)).await
}

#[tauri::command]
pub async fn model_store_answer_run(
    service: State<'_, Arc<ModelStoreService>>,
    run_id: String,
    answer: Option<String>,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    dispatch_store(move || model_store::answer_run(&service, run_id, answer)).await
}

/// 还在跑的长操作。给了 `card_id` 就只看那一张卡。
///
/// 界面进入模型商店时用它把任务卡接回去。模型商店的视图是条件挂载的，切一下视图
/// 整棵树就卸载、run_id 索引跟着一起没；而一次下载要跑几十分钟到十几小时。没有
/// 这条路，切走再回来就等于把这次运行丢了：看不到进度、点不了取消、脚本请求输入
/// 也没人答（600 秒后被判死），再点一次下载还会撞上卡片互斥，被渲染成一张说不清
/// 原因的失败卡。
///
/// 回的只是登记表快照；日志照常走 `model_store_run_log`，游标在调用方手里，
/// 接回来时前半段一行都不会丢。
#[tauri::command]
pub async fn model_store_active_runs(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: Option<String>,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    dispatch_store(move || model_store::active_runs(&service, card_id)).await
}

// ---------------------------------------------------------------- API Key

#[tauri::command]
pub fn model_store_api_keys(card_id: String) -> Result<Value, String> {
    model_store::api_keys(card_id)
}

#[tauri::command]
pub fn model_store_create_api_key(card_id: String, label: Option<String>) -> Result<Value, String> {
    model_store::create_api_key(card_id, label)
}

#[tauri::command]
pub async fn model_store_lan_status(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
) -> Result<Value, String> {
    model_store::lan_status(&service, card_id).await
}
#[tauri::command]
pub async fn model_store_set_lan_sharing(
    service: State<'_, Arc<ModelStoreService>>,
    card_id: String,
    enabled: bool,
    address: Option<String>,
    port: u16,
) -> Result<Value, String> {
    model_store::set_lan_sharing(&service, card_id, enabled, address, port).await
}
#[tauri::command]
pub fn model_store_revoke_api_key(key_id: String) -> Result<Value, String> {
    model_store::revoke_api_key(key_id)
}

// 这里原来有 `model_store_test_store_state` / `model_store_unlock_test_store` /
// `model_store_disable_test_store` 三个命令，服务的是内置测试商店那个隐藏开关。
// 内置测试商店 2026-09-30 整体拿掉，三个命令连同 lib.rs 里的注册、前端
// `model-store-api.js` 里的调用方一起删干净了（非 Linux 那边的同名桩也删了，
// 不留孤儿）。

// ------------------------------------------------------ 启动时回收残留进程

/// 取走启动清理的报告（取一次就没了）。
///
/// 应用启动时会收掉上次没收干净的脚本进程（见 lib.rs setup），收掉了什么攒在
/// 服务里等界面起来取。**取一次就没了**——否则用户每切一次视图就被同一条提示
/// 弹一遍。没有残留时回的是两个空数组，不是错误。
#[tauri::command]
pub async fn model_store_take_startup_cleanup(
    service: State<'_, Arc<ModelStoreService>>,
) -> Result<Value, String> {
    let service = Arc::clone(&service);
    dispatch_store(move || model_store::take_startup_cleanup(&service)).await
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
    startup_update_check_enabled: Option<bool>,
) -> Result<Value, String> {
    model_store::set_storage_settings(models_dir, import_scan_dirs, startup_update_check_enabled)
}

#[cfg(test)]
mod blocking_dispatch_tests {
    use super::dispatch_store;
    use serde_json::json;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::Duration;

    #[tokio::test(flavor = "current_thread")]
    async fn concurrent_dispatches_leave_the_invoke_thread_free() {
        let caller = std::thread::current().id();
        let calls = Arc::new(AtomicUsize::new(0));
        let mut pending = Vec::new();
        let mut started = Vec::new();
        let mut release = Vec::new();
        for id in 0..2 {
            let (started_tx, started_rx) = tokio::sync::oneshot::channel();
            let (release_tx, release_rx) = std::sync::mpsc::channel();
            let calls = Arc::clone(&calls);
            pending.push(tokio::spawn(dispatch_store(move || {
                calls.fetch_add(1, Ordering::SeqCst);
                let _ = started_tx.send(std::thread::current().id());
                release_rx
                    .recv_timeout(Duration::from_secs(2))
                    .map_err(|error| error.to_string())?;
                Ok(json!({"run_id": id}))
            })));
            started.push(started_rx);
            release.push(release_tx);
        }
        // Both blocking jobs must start before either is released. If dispatch
        // runs inline, its bounded wait finishes the job before this can run.
        for worker in started {
            let worker = tokio::time::timeout(Duration::from_secs(2), worker)
                .await
                .expect("worker did not start")
                .expect("worker dropped its start signal");
            assert_ne!(worker, caller);
        }
        assert!(pending.iter().all(|job| !job.is_finished()));
        assert_eq!(calls.load(Ordering::SeqCst), 2);
        for release in release {
            release.send(()).unwrap();
        }
        for (id, job) in pending.into_iter().enumerate() {
            assert_eq!(job.await.unwrap().unwrap(), json!({"run_id": id}));
        }
        assert_eq!(calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn backend_error_is_returned_without_replaying_the_operation() {
        let calls = Arc::new(AtomicUsize::new(0));
        let probe = Arc::clone(&calls);
        let result = dispatch_store(move || {
            probe.fetch_add(1, Ordering::SeqCst);
            Err("store_offline: fixture".into())
        })
        .await;
        assert_eq!(result.unwrap_err(), "store_offline: fixture");
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn join_error_is_returned_without_replaying_the_operation() {
        let calls = Arc::new(AtomicUsize::new(0));
        let probe = Arc::clone(&calls);
        let result = dispatch_store(move || {
            probe.fetch_add(1, Ordering::SeqCst);
            panic!("model store dispatch fixture panic");
        })
        .await;
        assert!(
            result
                .unwrap_err()
                .contains("model store dispatch fixture panic")
        );
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }
}
