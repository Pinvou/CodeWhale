//! install 委托门面（2026-10-14 架构拍板：品悟模型商店是唯一下载执行者）。
//!
//! `POST /v1/models/{card}/install` 保留路径，语义改委托：
//! - 卡片路由：cards_dir/<card>/ 五个入口脚本齐 → install_local（MCP 造卡/
//!   已物化卡）；否则 → install（商店服务器取卡）；
//! - 商店 socket 缺失/连接失败 → 409 store_offline（品悟未在运行）；
//! - 委托成功：拿 run_id → 建 cmsd 任务（kind=install，登记 store run_id）→
//!   镜像 worker 轮询商店任务，把日志行/进度镜像进 cmsd 任务事件与 progress
//!   字段（MCP cube_get_task 看到的形状与直接执行时代一致）；终态映射
//!   succeeded/failed/cancelled；
//! - 全局单并发与每卡互斥的 409 语义保留（委托下发前 task_create_install
//!   持锁检查；撞商店侧互斥时透传商店中文消息）。
//!
//! W1/W7 的直接执行编排（引擎探测/实装/下载/deploy 阶段、脚本运行器、
//! humanize、engine_name_env、deploy_complete、sanitize_compose_env）已退役；
//! enable.sh 首装的运行器迁到 ops.rs（那是它唯一的剩余消费者）。

use crate::app::{api_err, log_line, ApiErr, App};
use crate::json::Value;
use crate::shop::{StoreClient, StoreErr};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;
pub mod retry;

const CANCEL_RESULT: &str = "已取消，已下载部分保留，重新安装会从断点继续";
const STORE_OFFLINE_MSG: &str =
    "store_offline: 模型商店后台服务不可达，请打开品悟恢复后台服务后重试";

/// 本地已物化判据：五个入口脚本齐（install_engine/download_model/deploy/
/// enable/stop）。common.sh 各代卡有无不一；uninstall.sh 真机卡普遍缺，
/// 不纳入判据——纳了会把已物化的真机卡误路由到商店端。
const LOCAL_SCRIPT_SET: [&str; 5] = [
    "install_engine.sh",
    "download_model.sh",
    "deploy.sh",
    "enable.sh",
    "stop.sh",
];

fn has_full_scripts(card_dir: &Path) -> bool {
    LOCAL_SCRIPT_SET.iter().all(|f| card_dir.join(f).is_file())
}

/// POST /v1/models/{card}/install：互斥检查 → 路由 → 委托 → 建任务 → 镜像。
pub fn install(app: &Arc<App>, card_name: &str) -> Result<String, ApiErr> {
    // 全局单并发与每卡互斥（持锁检查+落记录，并发请求不会都放行）。
    let card_dir = app.config.cards_dir.join(card_name);
    let local = has_full_scripts(&card_dir);
    let (card_name, local) =
        crate::store_adoption::selection::installation_route(app, card_name, local)?;
    let card_name = card_name.as_str();
    let context = crate::store::StoreContext {
        kind: if local { "install_local" } else { "download" }.into(),
        retry: None,
        cancel_requested: false,
    };
    let task_id = app.task_create_install_context(card_name, Some(context))?;
    let client = StoreClient::new(&app.config.store_socket_path);
    // Persist the correlation before sending: a lost POST response is not
    // evidence that the store did not accept the request.
    let expected_run = format!("cmsd-{task_id}");
    let routed = client.install_request(card_name, &task_id, local);
    let run_id = match routed {
        Ok(id) => id,
        Err(e) => {
            let uncertain = matches!(e, StoreErr::Offline(_) | StoreErr::Protocol(_));
            let err = map_store_err(e, local);
            if uncertain {
                app.task_event(
                    &task_id,
                    "reconnect",
                    "安装请求结果待核实，保留原任务关联与互斥，不自动重发",
                );
                let app = Arc::clone(app);
                let id = task_id.clone();
                let card = card_name.to_string();
                std::thread::spawn(move || mirror_worker(&app, &id, &card, &expected_run));
                return Err(api_err(
                    409,
                    "store_unconfirmed",
                    &format!(
                        "{}；任务 {task_id} 将核实原商店请求，请查询该任务，不要重复安装",
                        err.2
                    ),
                ));
            }
            app.task_finish(&task_id, Err(err.2.clone()));
            return Err(err);
        }
    };
    // Legacy stores may return their own run identifier. Keep compatibility,
    // but never build a task-query path from an invalid identifier.
    if !crate::store::safe_store_id(&run_id) {
        app.task_event(
            &task_id,
            "reconnect",
            "商店返回的任务关联不合法，保留预分配关联等待核实",
        );
        let app = Arc::clone(app);
        let tid = task_id.clone();
        let card = card_name.to_string();
        std::thread::spawn(move || mirror_worker(&app, &tid, &card, &expected_run));
        return Ok(task_id);
    }
    if let Err(error) = app.task_set_store_run(&task_id, &run_id) {
        // The store already accepted: retain in-memory association and query it
        // rather than releasing exclusion or leaving no live reconciliation.
        app.task_event(
            &task_id,
            "reconnect",
            &format!("关联保存待恢复：{}", error.2),
        );
    }
    app.task_event(
        &task_id,
        "store",
        &format!(
            "已委托品悟模型商店代办（{}，run_id={run_id}），镜像进度中",
            if local {
                "本地已物化卡"
            } else {
                "商店服务器取卡"
            }
        ),
    );
    let app2 = Arc::clone(app);
    let tid = task_id.clone();
    let card = card_name.to_string();
    std::thread::spawn(move || mirror_worker(&app2, &tid, &card, &run_id));
    Ok(task_id)
}

/// Reattach delegated work after daemon restart; never submit the installation twice.
pub fn recover(app: &Arc<App>) {
    let pending: Vec<_> = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .tasks
            .values()
            .filter(|t| t.kind == "install" && !t.status.is_terminal())
            .filter_map(|t| {
                t.store_run_id
                    .as_ref()
                    .map(|r| (t.id.clone(), t.card.clone(), r.clone()))
            })
            .collect()
    };
    for (id, card, run) in pending {
        app.task_event(
            &id,
            "reconnect",
            "cmsd 已重启，按原商店任务关联恢复进度查询，不重复安装",
        );
        let app = Arc::clone(app);
        std::thread::spawn(move || mirror_worker(&app, &id, &card, &run));
    }
}

/// 商店错误 → cmsd API 错误：socket 不在 = store_offline 409；商店结构化
/// 错误（404/409 互斥等）原样透传（商店的中文消息）。
pub(crate) fn map_store_err(e: StoreErr, _local: bool) -> ApiErr {
    match e {
        StoreErr::NotSent(_) | StoreErr::Offline(_) => {
            api_err(409, "store_offline", STORE_OFFLINE_MSG)
        }
        StoreErr::Http(status, code, message) => api_err(status, &code, &message),
        StoreErr::Protocol(m) => api_err(
            500,
            "store_protocol",
            &format!("商店服务应答无法解析（对端可能不是品悟商店服务）：{m}"),
        ),
    }
}

/// 镜像 worker：轮询商店单任务，日志行/进度镜像进 cmsd 任务，终态映射。
/// Query failure never proves absence. Keep the exclusion until a correctly
/// correlated terminal receipt is observed. Retry replay uses only the durable
/// three-field idempotency contract, never a fresh ordinary installation.
fn mirror_worker(app: &Arc<App>, task_id: &str, card: &str, run_id: &str) {
    let client = StoreClient::new(&app.config.store_socket_path);
    let mut seen = 0usize;
    let mut offline_streak = 0u32;
    let mut next_replay = std::time::Instant::now();
    let mut replay_delay = 1u64;
    loop {
        let context = {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            let Some(task) = guard.state.tasks.get(task_id) else {
                return;
            };
            if task.status.is_terminal() {
                return;
            }
            task.store_context.clone()
        };
        match client.task(run_id) {
            Ok(detail) => {
                if detail.task.run_id != run_id
                    || detail.task.card_id != card
                    || context.as_ref().is_some_and(|c| c.kind != detail.task.kind)
                {
                    if offline_streak == 0 {
                        app.task_event(task_id,"reconnect","商店任务关联或类型不一致，保留互斥等待正确回执；不会按其它任务解除管理权");
                    }
                    offline_streak = offline_streak.saturating_add(1);
                    std::thread::sleep(Duration::from_millis(app.config.store_poll_ms.max(50)));
                    continue;
                }
                offline_streak = 0;
                for line in &detail.lines[seen
                    .saturating_sub(detail.line_offset)
                    .min(detail.lines.len())..]
                {
                    app.task_event(task_id, "store", line);
                }
                seen = seen.max(detail.line_offset + detail.lines.len());
                if let Some((done, total)) = detail.task.progress {
                    app.task_progress(task_id, done, total);
                }
                match detail.task.status.as_str() {
                    "succeeded" => {
                        let msg = if detail.task.kind == "download" {
                            format!("下载完成（由品悟模型商店代办）：{card}，权重已校验，尚未部署或启用")
                        } else if detail.task.kind == "install_local" {
                            format!("本地部署完成（由品悟模型商店代办）：{card}，尚未启用，可调用 /v1/models/{card}/enable")
                        } else {
                            format!(
                                "商店任务完成：{card}（{}），请复核实际部署状态",
                                detail.task.kind
                            )
                        };
                        log_line(&format!("install {card} 成功（商店代办）：{msg}"));
                        app.task_event(task_id, "done", &msg);
                        app.task_finish(task_id, Ok(msg));
                        return;
                    }
                    "failed" => {
                        let msg = if detail.task.error.is_empty() {
                            "商店代办下载失败（原因见商店侧日志）".to_string()
                        } else {
                            detail.task.error
                        };
                        log_line(&format!("install {card} 失败（商店代办）：{msg}"));
                        app.task_finish(task_id, Err(msg));
                        return;
                    }
                    "cancelled" => {
                        log_line(&format!("install {card} 已取消（商店代办）"));
                        app.task_cancel(task_id, CANCEL_RESULT);
                        return;
                    }
                    _ => {
                        if context.as_ref().is_some_and(|c| c.cancel_requested) {
                            // Persisted cancellation survives UI / cmsd restart.
                            // A running receiver may already be completing; the
                            // final receipt remains the authority for outcome.
                            let _ = client.cancel(run_id);
                        }
                    }
                }
            }
            Err(StoreErr::NotSent(e)) | Err(StoreErr::Offline(e)) => {
                offline_streak = offline_streak.saturating_add(1);
                if offline_streak == 1 {
                    app.task_event(
                        task_id,
                        "store",
                        &format!("商店连接中断（{e}），等待重连（商店侧任务仍在）…"),
                    );
                }
                if offline_streak.is_multiple_of(150) {
                    app.task_event(
                        task_id,
                        "reconnect",
                        "商店持续失联，保留任务及互斥，等待原任务恢复；不会重新下发安装",
                    );
                }
            }
            Err(StoreErr::Http(404, _, _)) => {
                if offline_streak == 0 {
                    app.task_event(
                        task_id,
                        "reconnect",
                        "商店暂未返回对应任务，安装状态待核实，继续保留原关联及互斥",
                    );
                }
                offline_streak = offline_streak.saturating_add(1);
                if let Some(context) = &context {
                    if let Some(retry) = &context.retry {
                        if std::time::Instant::now() >= next_replay {
                            match client.retry_install(retry, run_id) {
                                Ok(receipt)
                                    if retry::matches_receipt(&receipt, run_id, card, context) => {}
                                Err(StoreErr::Http(status, _, message))
                                    if (400..500).contains(&status)
                                        && (replay_delay == 1 || replay_delay == 30) =>
                                {
                                    // The first POST may still be queued or its
                                    // accepted record may be unavailable. A
                                    // later rejection cannot certify absence.
                                    app.task_event(task_id,"reconnect",&format!("原关联仍未确认，商店核实请求被拒绝；继续保留互斥：{message}"));
                                }
                                _ => {} // Unknown is still unknown; never release exclusion.
                            }
                            next_replay =
                                std::time::Instant::now() + Duration::from_secs(replay_delay);
                            replay_delay = (replay_delay * 2).min(30);
                        }
                    }
                }
            }
            Err(StoreErr::Http(_, _, m)) => {
                offline_streak = offline_streak.saturating_add(1);
                if offline_streak == 1 || offline_streak.is_multiple_of(150) {
                    app.task_event(
                        task_id,
                        "reconnect",
                        &format!("商店查询异常，原任务仍待核实：{m}"),
                    );
                }
            }
            Err(StoreErr::Protocol(m)) => {
                if offline_streak == 0 {
                    app.task_event(
                        task_id,
                        "reconnect",
                        &format!("商店任务协议异常，保留原关联及互斥：{m}"),
                    );
                }
                offline_streak = offline_streak.saturating_add(1);
            }
        }
        std::thread::sleep(Duration::from_millis(app.config.store_poll_ms.max(50)));
    }
}

/// POST /v1/tasks/{id}/cancel：install 类转发商店 cancel；其余类型维持原拒绝语义。
pub fn cancel(app: &Arc<App>, task_id: &str) -> Result<Value, ApiErr> {
    let local = app
        .store
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .state
        .tasks
        .contains_key(task_id);
    if !local {
        let client = StoreClient::new(&app.config.store_socket_path);
        let task = client
            .task(task_id)
            .map_err(|error| map_store_err(error, false))?;
        if task.task.status != "running" {
            return Err(api_err(409, "task_finished", "商店任务已结束"));
        }
        client
            .cancel(task_id)
            .map_err(|e| map_store_err(e, false))?;
        let mut value = Value::obj();
        value.set("task_id", task_id.into());
        value.set("status", "cancelling".into());
        return Ok(value);
    }
    let (store_run_id, cancelled_now) = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let Some(t) = guard.state.tasks.get(task_id) else {
            return Err(api_err(404, "unknown_task", &format!("没有任务 {task_id}")));
        };
        if t.status.is_terminal() {
            return Err(api_err(
                409,
                "task_finished",
                &format!("任务已结束（{}），不能取消", t.status.as_str()),
            ));
        }
        if t.kind != "install" {
            return Err(api_err(
                409,
                "kind_not_cancellable",
                &format!(
                    "该任务类型暂不支持取消（{}），目前只有 install 可取消",
                    t.kind
                ),
            ));
        }
        (
            t.store_run_id.clone(),
            t.status == crate::store::TaskStatus::Cancelled,
        )
    };
    let _ = cancelled_now;
    match store_run_id {
        Some(run_id) => {
            app.task_request_store_cancel(task_id)?;
            let client = StoreClient::new(&app.config.store_socket_path);
            match client.cancel(&run_id) {
                Ok(()) => {
                    let mut v = Value::obj();
                    v.set("task_id", task_id.into());
                    v.set("status", "cancelling".into());
                    v.set(
                        "message",
                        format!("已向品悟商店转发取消。{CANCEL_RESULT}").into(),
                    );
                    Ok(v)
                }
                Err(StoreErr::Http(404, _, _)) => {
                    let mut value = Value::obj();
                    value.set("task_id", task_id.into());
                    value.set("status", "cancelling".into());
                    value.set(
                        "message",
                        "取消意图已保存，正在核实原商店任务；尚未确认取消完成".into(),
                    );
                    Ok(value)
                }
                Err(e) => Err(map_store_err(e, false)),
            }
        }
        None => {
            // 委托没落上的遗留任务（理论上只存在于委托失败与落库之间的窗口
            // 或旧版直接执行时代的任务）：本地直接落 cancelled。
            app.cancel_mark(task_id);
            app.task_cancel(task_id, CANCEL_RESULT);
            let mut v = Value::obj();
            v.set("task_id", task_id.into());
            v.set("status", "cancelled".into());
            v.set("message", CANCEL_RESULT.into());
            Ok(v)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{Config, SystemdMode};
    use crate::shop::fixtures as sfx;
    use crate::store::{StateStore, TaskStatus};
    use crate::timeutil::now_epoch;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU64, Ordering};

    static SEQ: AtomicU64 = AtomicU64::new(0);

    fn tmpdir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "cmsd-facade-{tag}-{}-{}",
            std::process::id(),
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn make_app(root: &Path) -> Arc<App> {
        let cfg = Config {
            socket_path: root.join("cmsd.sock"),
            state_dir: root.join("state"),
            gateway_listen: "127.0.0.1:0".to_string(),
            systemd_mode: SystemdMode::User,
            native_unit_namespace: "legacy".into(),
            cards_dir: root.join("cards"),
            engine_root: root.join("engines"),
            unit_dir: root.join("units"),
            pool_start: 18080,
            pool_end: 18099,
            proc_root: PathBuf::from("/proc"),
            meminfo_path: PathBuf::from("/proc/meminfo"),
            debug_available_mib: None,
            ready_timeout_secs: 1500,
            models_dir: root.join("models"),
            staging_dir: root.join("staging"),
            hf_endpoint: "https://hf-mirror.example".to_string(),
            docker_cli: "docker".to_string(),
            nvidia_smi_cli: "nvidia-smi".to_string(),
            store_socket_path: root.join("store.sock"),
            store_poll_ms: 50,
            store_inventory_program: None,
            store_inventory_profile: None,
        };
        let store = StateStore::load(&cfg.state_file());
        let systemd = crate::systemd::Systemd::new(
            cfg.systemd_mode,
            cfg.unit_dir.clone(),
            cfg.env_dir(),
            "test".to_string(),
        );
        Arc::new(App::new(cfg, store, systemd))
    }

    #[test]
    fn lifecycle_admission_is_atomic_and_releases_after_finish() {
        let root = tmpdir("admission");
        let app = make_app(&root);
        let threads: Vec<_> = (0..16)
            .map(|i| {
                let app = Arc::clone(&app);
                std::thread::spawn(move || app.task_create_exclusive("enable", &format!("c-{i}")))
            })
            .collect();
        let accepted: Vec<_> = threads
            .into_iter()
            .filter_map(|t| t.join().unwrap().ok())
            .collect();
        assert_eq!(accepted.len(), 1);
        app.task_finish(&accepted[0], Ok("done".into()));
        assert!(app.task_create_exclusive("enable", "next").is_ok());
    }

    #[test]
    fn delegated_install_reconnects_to_original_run_after_reload() {
        let root = tmpdir("recover");
        let store = sfx::FakeStore::start(&root, &[], &["card-a"]);
        store.script_for(
            "card-a",
            vec![sfx::Step::Progress(2, 3), sfx::Step::Succeed],
        );
        let app = make_app(&root);
        let id = app.task_create_install("card-a").unwrap();
        let run = StoreClient::new(&root.join("store.sock"))
            .install_local("card-a")
            .unwrap();
        app.task_set_store_run(&id, &run).unwrap();
        drop(app);
        let restored = make_app(&root);
        recover(&restored);
        let task = wait_terminal(&restored, &id, 5);
        assert_eq!(task.status, TaskStatus::Succeeded);
        assert_eq!(task.store_run_id.as_deref(), Some(run.as_str()));
        assert_eq!(store.dispatched("card-a").len(), 1);
    }

    /// 写五个入口脚本（内容随便，判据只看存在）。
    fn write_full_scripts(root: &Path, card: &str) {
        let dir = root.join("cards").join(card);
        std::fs::create_dir_all(&dir).unwrap();
        for f in LOCAL_SCRIPT_SET {
            std::fs::write(dir.join(f), "#!/bin/sh\nexit 0\n").unwrap();
        }
    }

    fn wait_terminal(app: &Arc<App>, task_id: &str, timeout_secs: u64) -> crate::store::TaskRecord {
        let deadline = now_epoch() + timeout_secs;
        loop {
            {
                let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
                if let Some(t) = guard.state.tasks.get(task_id) {
                    if t.status.is_terminal() {
                        return t.clone();
                    }
                }
            }
            assert!(
                now_epoch() < deadline,
                "任务 {task_id} 在 {timeout_secs}s 内没有终结"
            );
            std::thread::sleep(Duration::from_millis(50));
        }
    }

    /// 本地卡走 install_local：委托→镜像（日志行/进度字段）→终态映射 succeeded。
    #[test]
    fn facade_local_card_full_mirror() {
        let root = tmpdir("local");
        let app = make_app(&root);
        write_full_scripts(&root, "fixture-local");
        let store = sfx::FakeStore::start(&root, &[], &["fixture-local"]);
        store.script_for(
            "fixture-local",
            vec![
                sfx::Step::Progress(1024, 4096),
                sfx::Step::Line("LMM_PROGRESS_BYTES=1024/4096"),
                sfx::Step::Progress(4096, 4096),
                sfx::Step::Line("权重下载完成并校验通过"),
                sfx::Step::Succeed,
            ],
        );
        let tid = install(&app, "fixture-local").unwrap();
        let t = wait_terminal(&app, &tid, 15);
        assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        assert!(
            t.result.as_deref().unwrap().contains("品悟模型商店代办"),
            "{:?}",
            t.result
        );
        assert_eq!(t.store_run_id.as_deref(), Some("r-1"));
        assert_eq!(t.progress_done_bytes, Some(4096));
        assert_eq!(t.progress_total_bytes, Some(4096));
        assert!(t
            .events
            .iter()
            .any(|e| e.message.contains("权重下载完成并校验通过")));
        assert_eq!(
            store.dispatched("fixture-local"),
            vec!["install_local".to_string()]
        );
        std::fs::remove_dir_all(&root).ok();
    }

    /// 脚本不齐的卡走 install（商店服务器取卡）。
    #[test]
    fn facade_partial_card_routes_to_install() {
        let root = tmpdir("partial");
        let app = make_app(&root);
        let dir = root.join("cards").join("fixture-partial");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("download_model.sh"), "#!/bin/sh\n").unwrap(); // 只有一个脚本
        let store = sfx::FakeStore::start(&root, &["fixture-partial"], &[]);
        store.script_for("fixture-partial", vec![sfx::Step::Succeed]);
        let tid = install(&app, "fixture-partial").unwrap();
        let t = wait_terminal(&app, &tid, 15);
        assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        // 命中的是 install 而非 install_local（locals 为空，走 install 才没 404）
        assert_eq!(
            store.dispatched("fixture-partial"),
            vec!["download".to_string()]
        );
        std::fs::remove_dir_all(&root).ok();
    }

    /// 商店 socket 缺失 → 409 store_offline（文案钉死），任务落 failed。
    #[test]
    fn facade_store_offline_409() {
        let root = tmpdir("offline");
        let app = make_app(&root);
        write_full_scripts(&root, "fixture-local");
        let e = install(&app, "fixture-local").unwrap_err();
        assert_eq!(e.0, 409);
        assert_eq!(e.1, "store_offline");
        assert_eq!(e.2, STORE_OFFLINE_MSG);
        let guard = app.store.lock().unwrap_or_else(|x| x.into_inner());
        let t = guard.state.tasks.values().next().unwrap();
        assert_eq!(t.status, TaskStatus::Failed);
        assert_eq!(t.error.as_deref(), Some(STORE_OFFLINE_MSG));
        std::fs::remove_dir_all(&root).ok();
    }

    /// 商店侧 404/409 透传（商店的中文消息原样给调用方）。
    #[test]
    fn facade_store_error_passthrough() {
        let root = tmpdir("pass404");
        let app = make_app(&root);
        write_full_scripts(&root, "no-such-local");
        let _store = sfx::FakeStore::start(&root, &["some-card"], &["some-local"]);
        let e = install(&app, "no-such-local").unwrap_err();
        assert_eq!(e.0, 404);
        assert_eq!(e.1, "unknown_local_card");
        assert!(e.2.contains("商店目录里没有卡片"), "{}", e.2);
        std::fs::remove_dir_all(&root).ok();
    }

    /// 商店侧互斥：已有一个下载在跑时，新委托撞 409（透传商店文案）。
    #[test]
    fn facade_store_side_mutex_passthrough() {
        let root = tmpdir("storemutex");
        let app = make_app(&root);
        write_full_scripts(&root, "card-a");
        write_full_scripts(&root, "card-b");
        let store = sfx::FakeStore::start(&root, &[], &["card-a", "card-b"]);
        // card-a 的剧本永远跑不完（无推进步骤，status 恒 running）
        store.script_for("card-a", vec![sfx::Step::Line("下载中…")]);
        let tid = install(&app, "card-a").unwrap();
        // 等商店侧任务真的落 running
        for _ in 0..100 {
            if store.running_count() == 1 {
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        // cmsd 侧互斥先拦（卡 b 撞「当前有安装任务进行中（card-a）」）
        let e = install(&app, "card-b").unwrap_err();
        assert_eq!(e.0, 409);
        assert!(e.2.contains("当前有安装任务进行中（card-a）"), "{}", e.2);
        // 取消 card-a：镜像看到 cancelled 后互斥释放
        cancel(&app, &tid).unwrap();
        let t = wait_terminal(&app, &tid, 15);
        assert_eq!(t.status, TaskStatus::Cancelled);
        assert_eq!(t.result.as_deref(), Some(CANCEL_RESULT));
        assert_eq!(
            store.dispatched("card-a"),
            vec!["install_local".to_string()]
        );
        std::fs::remove_dir_all(&root).ok();
    }

    /// 终态映射：商店 failed → cmsd failed（错误透传）；商店 cancelled → cancelled。
    #[test]
    fn facade_terminal_mapping() {
        let root = tmpdir("terminals");
        let app = make_app(&root);
        write_full_scripts(&root, "card-fail");
        write_full_scripts(&root, "card-cancel");
        let store = sfx::FakeStore::start(&root, &[], &["card-fail", "card-cancel"]);
        store.script_for(
            "card-fail",
            vec![
                sfx::Step::Line("下载中断，已下好的部分保留"),
                sfx::Step::Fail("网络问题：连接失败，已下载部分保留"),
            ],
        );
        store.script_for("card-cancel", vec![]);
        let tid_f = install(&app, "card-fail").unwrap();
        let t_f = wait_terminal(&app, &tid_f, 15);
        assert_eq!(t_f.status, TaskStatus::Failed);
        assert!(
            t_f.error.as_deref().unwrap().contains("网络问题"),
            "{:?}",
            t_f.error
        );
        // failed 是终态，互斥随之释放
        let tid_c = install(&app, "card-cancel").unwrap();
        // 商店侧无推进步骤：一直 running，从 cmsd 侧取消 → 商店 cancelled → 镜像落 cancelled
        let v = cancel(&app, &tid_c).unwrap();
        assert_eq!(v.get("status").and_then(|s| s.as_str()), Some("cancelling"));
        let t_c = wait_terminal(&app, &tid_c, 15);
        assert_eq!(t_c.status, TaskStatus::Cancelled);
        assert_eq!(t_c.result.as_deref(), Some(CANCEL_RESULT));
        assert!(store.dispatched("card-cancel").len() == 1);
        std::fs::remove_dir_all(&root).ok();
    }

    /// 取消的既有拒绝面不变：未知 404 / 已终结 409 / 非 install 409。
    #[test]
    fn cancel_rejects_unknown_finished_and_wrong_kind() {
        let root = tmpdir("cancelrej");
        let app = make_app(&root);
        let _store = sfx::FakeStore::start(&root, &[], &[]);
        assert_eq!(cancel(&app, "t-nope").unwrap_err().0, 404);
        let tid = app.task_create("enable", "c");
        app.task_finish(&tid, Ok("ok".to_string()));
        let e = cancel(&app, &tid).unwrap_err();
        assert_eq!(e.0, 409);
        assert!(e.2.contains("已结束"), "{}", e.2);
        let tid2 = app.task_create("enable", "c");
        let e2 = cancel(&app, &tid2).unwrap_err();
        assert_eq!(e2.0, 409);
        assert!(e2.2.contains("该任务类型暂不支持取消"), "{}", e2.2);
        std::fs::remove_dir_all(&root).ok();
    }

    /// 取消离线：商店 socket 没了 → 409 store_offline（委托任务不受本地强杀）。
    #[test]
    fn cancel_offline_returns_409() {
        let root = tmpdir("canceloff");
        let app = make_app(&root);
        write_full_scripts(&root, "card-a");
        let store = sfx::FakeStore::start(&root, &[], &["card-a"]);
        store.script_for("card-a", vec![]);
        let tid = install(&app, "card-a").unwrap();
        drop(store); // 商店进程消失（socket 文件还在但没人 accept → 连接即挂/超时）
        std::fs::remove_file(root.join("store.sock")).ok();
        let e = cancel(&app, &tid).unwrap_err();
        assert_eq!(e.0, 409);
        assert_eq!(e.1, "store_offline");
        std::fs::remove_dir_all(&root).ok();
    }
}
