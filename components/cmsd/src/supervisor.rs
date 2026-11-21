//! 守护与对账（T1 启动对账、T2 只读收编、T4 自愈观测）。
//!
//! 纪律：
//! - 启动对账不信状态库，先核现实（systemd 单元、引擎目录、进程）；
//! - 野生进程只登记不接管（D2）；绝不 kill 未登记进程（红线 4）；
//! - 明确托管对象的恢复由 recovery 模块统一预算；下面保留迁移前的
//!   v1后端策略对账路径与外部资源的只读发现。

use crate::app::{log_line, App};
use crate::capacity;
use crate::engine;
use crate::ops;
use crate::store::{EngineInfo, ModelStatus};
use crate::timeutil::now_epoch;
use std::sync::Arc;

/// 启动对账：状态库当线索，现实为准。返回对账到的摘要（启动日志用）。
pub fn reconcile_once(app: &Arc<App>) -> String {
    let mut notes: Vec<String> = Vec::new();

    // 0. 孤儿任务收割：管理面重启时仍在 running 的任务，其工作线程已随旧进程
    //    消失（install_child/cancel 登记都是内存态）。不收割的话，state.json 里
    //    的 running install 会把全局安装互斥永久锁死（真机实测：install 进行中
    //    重启 cmsd，任务永远卡 running、后续 install 一律 409）。
    //    标 failed 并写清复核引导：启动/切换类的 systemd 侧可能已独立完成；
    //    下载类的已下载部分保留，重发从断点续传。
    let reaped = {
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        if guard.state.active_model.as_ref().is_some_and(|card| {
            guard
                .state
                .models
                .get(card)
                .is_some_and(|model| !model.lifecycle_owned)
        }) {
            guard.state.active_model = None;
            guard.mark_dirty();
        }
        let mut n = 0usize;
        for t in guard.state.tasks.values_mut() {
            if !t.status.is_terminal() && !(t.kind == "install" && t.store_run_id.is_some()) {
                t.status = crate::store::TaskStatus::Failed;
                t.finished_at = Some(now_epoch());
                t.error = Some(
                    "cmsd 重启导致任务中断（管理面进程重启时任务仍在进行）。启动/切换类：systemd 侧可能已独立完成，请用 /v1/models 复核实况；下载类：已下载部分保留，重新安装会从断点继续。"
                        .to_string(),
                );
                n += 1;
            }
        }
        if n > 0 {
            guard.mark_dirty();
        }
        n
    };
    if reaped > 0 {
        notes.push(format!("孤儿任务收割 {reaped} 个"));
    }

    // 1. 引擎目录 → 注册表（内容寻址，目录名即二进制 sha 前 8 位）
    let engine_dirs: Vec<(String, String)> = std::fs::read_dir(&app.config.engine_root)
        .map(|entries| {
            entries
                .flatten()
                .filter(|e| e.path().is_dir())
                .map(|e| {
                    (
                        e.file_name().to_string_lossy().into_owned(),
                        e.path().display().to_string(),
                    )
                })
                .collect()
        })
        .unwrap_or_default();
    {
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let now = now_epoch();
        for (sha8, path) in &engine_dirs {
            let entry = guard
                .state
                .engines
                .entry(sha8.clone())
                .or_insert_with(|| EngineInfo {
                    sha8: sha8.clone(),
                    engine_type: "native".to_string(),
                    path: path.clone(),
                    first_seen_at: now,
                    last_seen_at: now,
                });
            entry.last_seen_at = now;
            entry.path = path.clone();
        }
        guard.mark_dirty();
    }
    notes.push(format!("引擎目录 {} 个", engine_dirs.len()));

    // 2. systemd 单元现实 → 模型状态对账
    let units = app.systemd.list_model_units().unwrap_or_else(|e| {
        log_line(&format!("对账：列 systemd 单元失败（{e}），按无单元处理"));
        Vec::new()
    });
    {
        let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
        live.units.clear();
        for unit in &units {
            let us = app.systemd.show(unit).unwrap_or_default();
            live.units.insert(unit.clone(), us);
        }
    }
    notes.push(format!("megacube 单元 {} 个", units.len()));
    let known_cards: Vec<String> = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .models
            .values()
            .filter(|m| m.engine_type != "container")
            .map(|m| m.card.clone())
            .collect()
    };
    for card in known_cards {
        let unit = app.systemd.unit_name_for(&card);
        match crate::store_adoption::native_state(app, &card)
            .and_then(|state| state.map(Ok).unwrap_or_else(|| app.systemd.show(&unit)))
        {
            Ok(us) => reconcile_model(app, &card, &us, false),
            Err(error) => log_line(&format!("单元 {unit} 查询失败，保留原事实：{error}")),
        }
    }
    // 单元存在但状态库没有记录的卡：补登记（状态库丢失/重建场景）
    for unit in &units {
        if let Some(card) = app.systemd.card_from_unit(unit) {
            let exists = {
                let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
                guard.state.models.contains_key(&card)
            };
            if !exists {
                let us = app.systemd.show(unit).unwrap_or_default();
                ops::register_external_managed(app, &card, &us);
                reconcile_model(app, &card, &us, false);
            }
        }
    }

    // 3. 进程扫描 → 野生收编
    let procs = engine::scan_engine_procs(&app.config.proc_root, &app.config.engine_root);
    let managed_pids = managed_pids(app);
    let mut wild = 0;
    for proc in &procs {
        if !managed_pids.contains(&proc.pid) {
            ops::register_external(app, proc);
            wild += 1;
        }
    }
    notes.push(format!("驭光进程 {} 个（野生 {wild}）", procs.len()));
    {
        let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
        live.procs = procs;
    }

    // 4. W6 容器对账（收编在跑容器 + 手动停/异常死标记）
    notes.push(reconcile_containers(app));
    refresh_gpu_attribution(app);

    app.save_store();
    notes.join("，")
}

/// 后台守护：2s  tick 观测单元与进程，10s 级健康探测。
pub fn run(app: Arc<App>) {
    let mut tick: u64 = 0;
    loop {
        std::thread::sleep(std::time::Duration::from_secs(2));
        tick += 1;
        let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            supervise_tick(app.clone(), tick);
        }));
    }
}

fn supervise_tick(app: Arc<App>, tick: u64) {
    if app
        .store_adoption_requested
        .swap(false, std::sync::atomic::Ordering::AcqRel)
        || tick == 1
        || tick.is_multiple_of(15)
    {
        crate::store_adoption::reconcile(&app);
    }
    // ---- 单元观测：重启计数、熔断识别、状态收敛（W6：容器模型不走 systemd 面）----
    let cards: Vec<String> = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .models
            .values()
            .filter(|m| m.engine_type != "container")
            .map(|m| m.card.clone())
            .collect()
    };
    for card in &cards {
        let unit = app.systemd.unit_name_for(card);
        let us = match crate::store_adoption::native_state(&app, card)
            .and_then(|state| state.map(Ok).unwrap_or_else(|| app.systemd.show(&unit)))
        {
            Ok(us) => us,
            Err(error) => {
                log_line(&format!("单元 {unit} 查询失败，本轮不收敛：{error}"));
                continue;
            }
        };
        {
            let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
            live.units.insert(unit.clone(), us.clone());
        }
        reconcile_model(&app, card, &us, true);
    }

    // ---- 进程扫描与野生收编（每 tick）----
    let procs = engine::scan_engine_procs(&app.config.proc_root, &app.config.engine_root);
    let managed = managed_pids(&app);
    for proc in &procs {
        if !managed.contains(&proc.pid) {
            ops::register_external(&app, proc);
        }
    }
    // 清理已消失 10 分钟以上的野生 sighting（留痕但不无限膨胀）
    {
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let now = now_epoch();
        let live_pids: std::collections::BTreeSet<u32> = procs.iter().map(|p| p.pid).collect();
        let before = guard.state.externals.len();
        guard
            .state
            .externals
            .retain(|_, s| live_pids.contains(&s.pid) || now.saturating_sub(s.last_seen_at) < 600);
        if guard.state.externals.len() != before {
            guard.mark_dirty();
        }
    }
    {
        let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
        live.procs = procs.clone();
    }

    // ---- 健康探测（每 5 tick ≈ 10s 一轮；超时最坏 3s 在容忍带内）----
    if tick.is_multiple_of(5) {
        let mut ports: Vec<(u16, Option<String>, Option<u32>)> = Vec::new();
        {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            for m in guard.state.models.values() {
                if matches!(m.status, ModelStatus::Running | ModelStatus::Starting) && m.port > 0 {
                    ports.push((
                        m.port,
                        Some(m.model_id.clone()),
                        (m.engine_type == "native").then_some(m.main_pid).flatten(),
                    ));
                }
            }
            for s in guard.state.externals.values() {
                if let Some(p) = s.port {
                    // Sightings are retained for diagnostics after exit. They
                    // must not erase a current managed instance's probe key,
                    // or be applied to a reused PID/port after daemon restart.
                    if !ports.iter().any(|(port, _, _)| *port == p)
                        && crate::store_adoption::runtime::instance_key(&app, s.pid)
                            == Some((s.pid, s.start_time))
                    {
                        ports.push((p, s.model_id.clone(), Some(s.pid)));
                    }
                }
            }
        }
        // W6：未关联容器也探测（/v1/engines 展示健康）；已探端口不去重无所谓，少一次是一次
        {
            let live = app.live.lock().unwrap_or_else(|e| e.into_inner());
            for c in live.containers.values() {
                if c.running {
                    if let Some(p) = c.host_port {
                        if !ports.iter().any(|(port, _, _)| *port == p) {
                            ports.push((p, None, None));
                        }
                    }
                }
            }
        }
        let mut model_id_updates: Vec<(String, String)> = Vec::new();
        for (port, expect, pid) in ports {
            let before =
                pid.and_then(|pid| crate::store_adoption::runtime::instance_key(&app, pid));
            let health = engine::check_health(port, expect.as_deref());
            // W6：容器卡收编时可能还不知道真名，健康探测问到了就补登记
            if let Some(id) = &health.model_id {
                if health.ok {
                    model_id_updates.push((port.to_string(), id.clone()));
                }
            }
            let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
            let after = pid.and_then(|pid| crate::store_adoption::runtime::instance_key(&app, pid));
            if let Some(key) = after.filter(|key| Some(*key) == before) {
                live.health_instances.insert(port, key);
            } else {
                live.health_instances.remove(&port);
            }
            live.health.insert(port, health);
        }
        if !model_id_updates.is_empty() {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            let mut dirty = false;
            for (port, id) in model_id_updates {
                let Ok(port) = port.parse::<u16>() else {
                    continue;
                };
                if let Some(m) = guard.state.models.values_mut().find(|m| m.port == port) {
                    if m.model_id.is_empty() {
                        m.model_id = id;
                        dirty = true;
                    }
                }
            }
            if dirty {
                guard.mark_dirty();
            }
        }
    }

    // ---- W6 容器对账（每 15 tick ≈ 30s）与显存归属刷新（每 30 tick ≈ 60s）----
    if tick.is_multiple_of(15) {
        let note = reconcile_containers(&app);
        if !note.is_empty() {
            log_line(&note);
        }
    }
    if tick.is_multiple_of(30) {
        refresh_gpu_attribution(&app);
    }
    if tick.is_multiple_of(5) {
        crate::recovery::tick(&app);
    }

    app.save_store();
}

/// 单元状态 → 模型状态收敛（对账与守护共用的核心判定）。
/// count_restarts=false 用于启动对账：刚开机/重启后 pid 必然不同，
/// 不能把"系统重启后的新进程"误记为引擎崩溃重启（V10 前自查发现）；
/// 运行期 tick 才按 pid 变化累计重启次数。
/// W6 边界规则（驭光侧，10-14 事故教训）：
/// - failed 非熔断 = 异常死：degraded_reason 写明「引擎异常退出（非管理面停止）」，
///   自启标记保留（systemd 仍按策略重拉；恢复后标记自愈清除）；
/// - 状态说 Running 但单元干净退出（inactive）= 绕开管理面停（systemctl stop）
///   ⇒ 视为手动关闭：状态落 Stopped + 撤自启登记。
fn reconcile_model(
    app: &Arc<App>,
    card: &str,
    us: &crate::systemd::UnitState,
    count_restarts: bool,
) {
    if crate::recovery::owns(app, card) {
        return;
    }
    // r11 P1-1：start_limit_hit 在 failed 态会跑 journalctl 子进程（上限
    // 10s）——它只依赖单元名与 UnitState，不依赖锁内状态，必须在取锁前
    // 算好，否则任一受管单元 failed 时每 2s tick 持全局 store 互斥阻塞
    // 全部 API/网关/install。
    let start_limit_hit = app
        .systemd
        .start_limit_hit(&app.systemd.unit_name_for(card), us);
    // 绕开管理面停（手动关闭）的判定要在持锁改状态之前算好（disable 要在锁外做）
    let mut manual_stop = false;
    // 刚转 degraded 时锁外补一句预算复核（恢复集超内存的可见性，语义边界 2）
    let mut just_degraded = false;
    {
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let Some(m) = guard.state.models.get_mut(card) else {
            return;
        };
        if !m.lifecycle_owned {
            // r10 P2-C：观察路径先 diff 再标脏——状态与 PID 均未变化时
            // 不做每 2s tick 的全量落盘。
            let next_status = if us.is_active() {
                ModelStatus::Running
            } else {
                ModelStatus::Stopped
            };
            let next_pid = Some(us.main_pid).filter(|pid| *pid > 0);
            if m.status != next_status || m.main_pid != next_pid {
                m.status = next_status;
                m.main_pid = next_pid;
                guard.mark_dirty();
            }
            return;
        }
        let prev_status = m.status;
        let prev_restarts = m.restart_count;
        let prev_pid = m.main_pid;

        // 熔断识别优先（D4：转 degraded 不再拉起，留痕）
        if start_limit_hit {
            if m.status != ModelStatus::Degraded {
                m.status = ModelStatus::Degraded;
                m.degraded_reason = Some(format!(
                    "5 分钟内启动失败 3 次，systemd 已熔断（{}），不再自动拉起。修复后用 enable 恢复",
                    crate::timeutil::now_iso8601()
                ));
                just_degraded = true;
                log_line(&format!("模型 {card} 转 degraded：start-limit 熔断"));
            }
        } else if us.is_active() {
            // 重启检测：MainPID 变了 = 引擎被重新拉起（NRestarts 会被 reset-failed
            // 清零，不可做权威计数——V3 实证踩过）。cmsd 宕机期间的崩溃重启在下次
            // 对账时也能以 pid 差异捕获（时间戳为对账时刻，属近似留痕）。
            if us.main_pid > 0 {
                if count_restarts {
                    if let Some(old) = m.main_pid {
                        if old != us.main_pid {
                            m.restart_count += 1;
                            m.last_restart_at = Some(now_epoch());
                            log_line(&format!(
                                "模型 {card} 引擎重启：pid {old} → {}（累计 {} 次）",
                                us.main_pid, m.restart_count
                            ));
                        }
                    }
                }
                m.main_pid = Some(us.main_pid);
            }
            if m.status != ModelStatus::Running && m.status != ModelStatus::Starting {
                // 对账时把 starting 之外的一刀切成 running；健康与否由探测逐步确认
                m.status = ModelStatus::Running;
            }
            // 活过来了：清掉上一轮异常退出的可见标记（自愈闭环）
            if m.degraded_reason
                .as_deref()
                .map(|r| r.contains("异常退出"))
                .unwrap_or(false)
            {
                m.degraded_reason = None;
            }
        } else if us.active_state == "inactive" || us.active_state == "failed" || us.not_found {
            if us.active_state == "failed" && us.result != "success" {
                // 非熔断的 failed = 异常死（如 OOM kill 后未达限频）：
                // 可见标记 + 自启标记保留，systemd 仍会按 Restart=on-failure 尝试
                if m.status != ModelStatus::Degraded {
                    m.degraded_reason = Some(format!(
                        "引擎异常退出（非管理面停止，结果 {}）；自启标记保留，systemd 正按策略重拉（反复失败会熔断转 degraded）",
                        us.result
                    ));
                }
                if prev_status == ModelStatus::Starting {
                    m.status = ModelStatus::Stopped;
                }
            } else if prev_status == ModelStatus::Running {
                // 状态说 Running 但单元干净退出/消失：cmsd 的停都会先把状态落成
                // Stopped，能撞见 Running 就是绕开管理面停的 ⇒ 手动关闭
                manual_stop = true;
                m.status = ModelStatus::Stopped;
                m.main_pid = None;
            } else if matches!(m.status, ModelStatus::Starting) {
                m.status = ModelStatus::Stopped;
            }
        }
        if m.status != prev_status || m.restart_count != prev_restarts || m.main_pid != prev_pid {
            guard.mark_dirty();
        }
    }
    if manual_stop {
        // 手动关闭 = 清标记（锁外做 systemd 调用；幂等，失败只留日志）
        log_line(&format!(
            "对账：{card} 状态为 Running 但单元已停，按绕开管理面的手动关闭处理：撤开机自启登记"
        ));
        let _ = app.systemd.disable(&app.systemd.unit_name_for(card));
    }
    if just_degraded {
        // 恢复集超内存的可见性：熔断原因后补一句当前预算复核（不阻塞、不改判定）
        if let Some(note) = budget_note_if_insufficient(app, card) {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(m) = guard.state.models.get_mut(card) {
                if let Some(r) = m.degraded_reason.as_mut() {
                    r.push_str(&format!("；当前预算复核：{note}"));
                }
                guard.mark_dirty();
            }
        }
    }
}

/// 预算不足时的中文复核句（degraded 原因的可视化补充；卡片扫不到/内存够给 None）。
fn budget_note_if_insufficient(app: &Arc<App>, card_name: &str) -> Option<String> {
    let card = crate::cards::scan_cards(&app.config.cards_dir)
        .into_iter()
        .find(|c| c.name == card_name)?;
    let mem = capacity::read_meminfo(&app.config.meminfo_path).ok()?;
    let measured = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .models
            .get(card_name)
            .and_then(|m| m.measured_footprint_gib)
    };
    let (required, basis, note) = ops::required_gib_for(app, &card, mem.total_gib(), measured);
    let budget = capacity::evaluate_with_required(
        &mem,
        required,
        basis,
        &note,
        app.config.debug_available_mib,
    );
    if budget.ok {
        None
    } else {
        Some(budget.verdict)
    }
}

fn managed_pids(app: &Arc<App>) -> std::collections::BTreeSet<u32> {
    let live = app.live.lock().unwrap_or_else(|e| e.into_inner());
    live.units
        .values()
        .filter(|u| u.is_active() && u.main_pid > 0)
        .map(|u| u.main_pid)
        .collect()
}

// ---------------------------------------------------------------------------
// W6 容器对账：发现 → 收编 → 状态收敛（语义规格边界规则的落点）。
// 纪律：docker scan 失败 = 「没数据」，整段跳过（绝不能拿空列表当「没容器」收敛）；
// 卡片任务在跑时不动它的状态（对账不与编排打架）。
// ---------------------------------------------------------------------------

/// 容器对账（启动对账与 30s tick 共用）。返回摘要（空 = 无事发生）。
pub fn reconcile_containers(app: &Arc<App>) -> String {
    let scan = match app.docker.scan() {
        Ok(v) => v,
        Err(e) => {
            log_line(&format!("容器扫描失败（{e}），本轮跳过容器对账"));
            return String::new();
        }
    };
    let total = scan.len();
    {
        let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
        live.containers = scan.into_iter().map(|c| (c.name.clone(), c)).collect();
        live.containers_scanned_at = now_epoch();
    }
    let cards = crate::cards::scan_cards(&app.config.cards_dir);
    let tasks_running: std::collections::BTreeSet<String> = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .tasks
            .values()
            .filter(|t| t.status == crate::store::TaskStatus::Running)
            .map(|t| t.card.clone())
            .collect()
    };
    let mut adopted = 0;
    for card in cards
        .iter()
        .filter(|c| c.engine_kind == crate::cards::EngineKind::Container)
    {
        let Some(cname) = &card.container_name else {
            continue;
        };
        let container = {
            let live = app.live.lock().unwrap_or_else(|e| e.into_inner());
            live.containers.get(cname).cloned()
        };
        let registered = {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard.state.models.contains_key(&card.name)
        };
        if !registered {
            // W6a 收编（D2 只读语义）：命中卡的容器补登记，不动容器
            if let Some(c) = &container {
                ops::register_container_managed(app, card, c);
                adopted += 1;
            }
            continue;
        }
        if tasks_running.contains(&card.name) {
            continue;
        }
        reconcile_container_model(app, &card.name, container.as_ref());
    }
    // 活跃指针自动认领（2026-10-16 真机实证：旧版品悟/docker 绕开管理面把
    // 唯一模型拉起来，对账收编了 running 却没认 active——网关 no_active_model，
    // 用户看着「服务在跑」却不可用）。只在「活跃指针空缺 + 恰好一张容器卡在跑
    // **且健康探测通过、无异常标记**」时认领——容器 Up ≠ 引擎可用（权重加载
    // 数分钟），未就绪认领会把「明确的无活跃模型」变成「连接失败」；崩溃抖动
    // 中的卡同样不认。多卡在跑不替用户做选择（与 enable 只拉起不挪默认的
    // 防劫持纪律一致）；指针指向在跑卡（含崩溃自愈等待中）不动。
    let adoptable: Vec<String> = {
        let candidates: Vec<(String, u16)> = {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            if guard.state.active_model.is_some() {
                Vec::new()
            } else {
                guard
                    .state
                    .models
                    .iter()
                    .filter(|(_, m)| {
                        m.engine_type == "container"
                            && m.status == ModelStatus::Running
                            && m.degraded_reason.is_none()
                    })
                    .map(|(k, m)| (k.clone(), m.port))
                    .collect()
            }
        };
        let live = app.live.lock().unwrap_or_else(|e| e.into_inner());
        candidates
            .into_iter()
            .filter(|(_, port)| live.health.get(port).map(|h| h.ok).unwrap_or(false))
            .map(|(k, _)| k)
            .collect()
    };
    if let [only] = adoptable.as_slice() {
        let model = app
            .store
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .state
            .models
            .get(only)
            .cloned();
        // The runtime proof performs backend I/O outside both state locks.
        if !model
            .as_ref()
            .is_some_and(|model| crate::container_runtime::routable(app, model))
        {
            return format!("容器 {total} 个（收编 {adopted}）");
        }
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        // 取锁期间可能已被 enable/switch 任务写上，二次确认再落笔
        if guard.state.active_model.is_none()
            && guard
                .state
                .models
                .get(only)
                .is_some_and(|m| m.lifecycle_owned)
        {
            let only = only.clone();
            guard.state.active_model = Some(only.clone());
            guard.mark_dirty();
            drop(guard);
            log_line(&format!(
                "对账：活跃指针空缺，唯一在跑且健康的 {only} 认领为活跃"
            ));
        }
    }
    format!("容器 {total} 个（收编 {adopted}）")
}

/// 显存归属刷新（启动对账与 60s tick 共用）：nvidia-smi 进程清单 +
/// 在跑容器的宿主 PID 表（docker top），两者交集即容器显存占用。
fn refresh_gpu_attribution(app: &Arc<App>) {
    let gpu = app.docker.gpu_procs_mib();
    let running: Vec<String> = {
        let live = app.live.lock().unwrap_or_else(|e| e.into_inner());
        live.containers
            .values()
            .filter(|c| c.running)
            .map(|c| c.name.clone())
            .collect()
    };
    let mut pids_map = std::collections::BTreeMap::new();
    for name in running {
        if let Ok(pids) = app.docker.top_pids(&name) {
            pids_map.insert(name, pids);
        }
    }
    let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
    live.gpu_procs = gpu;
    live.container_pids = pids_map;
}

/// 已登记容器模型的状态收敛（边界规则）：
/// - 在跑 → Running，异常退出标记自愈清除，手动停驻留计时清零；
/// - 状态说 Running 但容器 exited 且自启策略在：
///   退出码 0/143（SIGTERM 落地）⇒ 显式 docker stop 的标志性形状（崩溃几乎
///   不会以此落地，且策略在的崩溃会被 docker 立即重拉）⇒ 立即按手动关闭；
///   其它退出码/OOMKilled ⇒ 先按异常死标记（标记保留，docker 按策略重拉），
///   驻留 120s 仍 exited（崩溃重拉早该发生）⇒ 改判手动关闭清标记；
/// - 策略本就是 no：0/143 落 Stopped（无标记），其余按异常死标记；
/// - 容器整个消失 ⇒ 事实 Stopped + 可见标记（enable 会走首装重建）。
const MANUAL_STOP_DWELL_SECS: u64 = 120;

fn reconcile_container_model(
    app: &Arc<App>,
    card_name: &str,
    c: Option<&crate::docker::ContainerInfo>,
) {
    if crate::recovery::owns(app, card_name) {
        return;
    }
    enum Verdict {
        Running,
        ManualStop(String), // 绕开管理面停 ⇒ 手动关闭（清标记）
        CrashDead(String),  // 异常死（标记保留，docker 按策略拉起）
        Nothing,
    }
    let verdict = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let Some(m) = guard.state.models.get(card_name) else {
            return;
        };
        if m.engine_type != "container" {
            return;
        }
        if !m.lifecycle_owned {
            // Observation cannot mutate restart policy or choose the gateway default.
            drop(guard);
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(model) = guard.state.models.get_mut(card_name) {
                // r18 #2：与下方原生孪生分支同款 diff 守卫——未归属容器卡的
                // 30s 观察在稳态（status/main_pid 未变）不得置脏（原为每轮
                // 全量落盘 2,880 saves/日）；重取锁后复检 ownership，避免并发
                // enable 完成时被旧观察覆盖落盘错误快照。
                if model.lifecycle_owned {
                    return;
                }
                let next_status = if c.is_some_and(|c| c.running) {
                    ModelStatus::Running
                } else {
                    ModelStatus::Stopped
                };
                let next_pid = c.filter(|c| c.running && c.pid > 0).map(|c| c.pid);
                if model.status != next_status || model.main_pid != next_pid {
                    model.status = next_status;
                    model.main_pid = next_pid;
                    guard.mark_dirty();
                }
            }
            return;
        }
        match c {
            Some(c) if c.running => Verdict::Running,
            Some(c) => {
                if !c.is_autostart() {
                    // 策略本来就是 no：没有「docker 会拉起」的前提，按退出码分
                    if m.status != ModelStatus::Running {
                        Verdict::Nothing
                    } else if !c.oom_killed && (c.exit_code == 0 || c.exit_code == 143) {
                        Verdict::ManualStop(format!(
                            "容器 {} 已停止（退出码 {}）",
                            c.name, c.exit_code
                        ))
                    } else {
                        Verdict::CrashDead(format!(
                            "引擎异常退出（非管理面停止，退出码 {}{}）",
                            c.exit_code,
                            if c.oom_killed {
                                "，驱动 OOMKilled"
                            } else {
                                ""
                            }
                        ))
                    }
                } else if !c.oom_killed && (c.exit_code == 0 || c.exit_code == 143) {
                    // 策略在却是干净退出码：docker 不可能让它停在 exited（崩溃会
                    // 被自动拉起）⇒ 显式 docker stop
                    if m.status != ModelStatus::Running {
                        Verdict::Nothing
                    } else {
                        Verdict::ManualStop(format!(
                            "容器 {} 被绕开管理面停掉（docker stop，退出码 {}）",
                            c.name, c.exit_code
                        ))
                    }
                } else {
                    // 非零退出 + 策略在：Running（初见按异常死）或 Stopped-with-marker
                    // （续见等驻留）都进驻留判定；120s 仍 exited ⇒ docker 的崩溃
                    // 重拉早该发生 ⇒ 改判手动关闭
                    let dwell_hit = {
                        let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
                        let first = live
                            .container_stop_candidates
                            .entry(card_name.to_string())
                            .or_insert_with(now_epoch);
                        now_epoch().saturating_sub(*first) >= MANUAL_STOP_DWELL_SECS
                    };
                    if dwell_hit {
                        Verdict::ManualStop(format!(
                            "容器 {} 持续退出超过 120s（疑似外部停止或反复崩溃，退出码 {}{}）",
                            c.name,
                            c.exit_code,
                            if c.oom_killed {
                                "，驱动 OOMKilled"
                            } else {
                                ""
                            }
                        ))
                    } else if m.status == ModelStatus::Running {
                        Verdict::CrashDead(format!(
                            "引擎异常退出（非管理面停止，退出码 {}{}）；自启标记保留（restart={}），docker 会按策略拉起",
                            c.exit_code,
                            if c.oom_killed { "，驱动 OOMKilled" } else { "" },
                            c.restart_policy
                        ))
                    } else {
                        Verdict::Nothing // 已落 Stopped+异常标记：安静等驻留
                    }
                }
            }
            None => {
                if matches!(m.status, ModelStatus::Running | ModelStatus::Starting) {
                    Verdict::CrashDead(
                        "容器已不存在（可能被外部 docker rm）；重新 enable 会走首装重建"
                            .to_string(),
                    )
                } else {
                    Verdict::Nothing
                }
            }
        }
    };
    match verdict {
        Verdict::Nothing => {}
        Verdict::Running => {
            let Some(c) = c else { return };
            let gpu_mib = {
                let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
                live.container_stop_candidates.remove(card_name);
                live.gpu_mib_for_container(&c.name)
            };
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(m) = guard.state.models.get_mut(card_name) {
                let prev = m.status;
                m.status = ModelStatus::Running;
                if c.pid > 0 {
                    m.main_pid = Some(c.pid);
                }
                if let Some(p) = c.host_port {
                    m.port = p;
                }
                // 实测常驻回填（nvidia-smi∩docker top 归属）：容量/模型接口与
                // MCP 汇总都用它说话；0 = 归属还没刷新出来，不覆盖旧值
                if gpu_mib > 0 {
                    m.measured_footprint_gib =
                        Some((gpu_mib as f64 / 1024.0 * 10.0).round() / 10.0);
                }
                // 活过来了：清掉上一轮异常退出的可见标记（自愈闭环）
                if m.degraded_reason
                    .as_deref()
                    .map(|r| r.contains("异常退出"))
                    .unwrap_or(false)
                {
                    m.degraded_reason = None;
                }
                if prev != ModelStatus::Running {
                    guard.mark_dirty();
                }
            }
        }
        Verdict::ManualStop(note) => {
            {
                let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
                if let Some(m) = guard.state.models.get_mut(card_name) {
                    m.status = ModelStatus::Stopped;
                    m.main_pid = None;
                    // 改判路径此前可能留过异常退出标记：手动关闭落定后清掉
                    m.degraded_reason = None;
                    guard.mark_dirty();
                }
                // 与 disable 同语义：绕开管理面的手动关闭也要清活跃指针，
                // 否则面板/MCP 把已停模型继续报成「活跃」（2026-10-16 真机
                // 实证：旧版品悟 stop.sh 停 mtp 后指针滞留）。
                if guard.state.active_model.as_deref() == Some(card_name) {
                    guard.state.active_model = None;
                }
            }
            // 手动关闭 = 清标记（语义边界 3：对账按事实对齐，视为手动关闭）
            if let Some(c) = c {
                let model = app
                    .store
                    .lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .state
                    .models
                    .get(card_name)
                    .cloned();
                if model.as_ref().is_some_and(|model| {
                    crate::container_runtime::saved_reference(model, c).is_ok()
                }) {
                    let _ = app.docker.set_restart_policy(&c.name, "no");
                }
            }
            {
                let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
                live.container_stop_candidates.remove(card_name);
            }
            log_line(&format!("对账：{note}，按手动关闭处理：已清自启标记"));
        }
        Verdict::CrashDead(reason) => {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(m) = guard.state.models.get_mut(card_name) {
                m.status = ModelStatus::Stopped;
                m.main_pid = None;
                m.degraded_reason = Some(reason.clone());
                guard.mark_dirty();
            }
            log_line(&format!("对账：{card_name} {reason}"));
        }
    }
    // 残留收敛：对账之前状态就已是 Stopped（旧版品悟 stop.sh / docker stop
    // 的历史现场），而活跃指针还指着这张卡、容器又不会自拉（策略已 no 或
    // 容器不在）⇒ 指针一并清掉，与 disable 语义对齐。
    // 崩溃但自启策略在的不动：docker 按策略重拉后指针仍有效（自愈闭环）。
    {
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        if guard.state.active_model.as_deref() == Some(card_name) {
            let stale = matches!(guard.state.models.get(card_name), Some(m) if m.status == ModelStatus::Stopped)
                && c.map(|c| !c.is_autostart()).unwrap_or(true);
            if stale {
                guard.state.active_model = None;
                guard.mark_dirty();
                log_line(&format!(
                    "对账：{card_name} 已停且不会自拉，清掉滞留的活跃指针"
                ));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::docker::fixtures as fx;
    use crate::store::TaskStatus;

    /// Legacy snapshot before recovery migration. These compatibility cases
    /// deliberately reconstruct the old Docker policy; new recovery is tested
    /// separately in recovery::tests and the real process fault harness.
    fn enabled_fixture(tag: &str) -> (std::path::PathBuf, Arc<App>, u16) {
        let root = fx::tmpdir(tag);
        let app = fx::make_app(&root, 128, 100);
        let port = fx::serve_models_json("Fixture-Model");
        fx::write_docker_card(
            &root.join("cards"),
            "fixture-c",
            "fixture-c-ctr",
            port,
            Some("0.01"),
        );
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            false,
            "no",
            0,
            false,
            0,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );
        let tid = ops::enable(&app, "fixture-c").unwrap();
        {
            let t = fx::wait_terminal(&app, &tid, 30);
            assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        }
        std::fs::remove_file(app.config.state_dir.join("recovery/fixture-c.json")).unwrap();
        app.docker
            .set_restart_policy("fixture-c-ctr", "unless-stopped")
            .unwrap();
        (root, app, port)
    }

    /// 孤儿任务收割：重启时仍在 running 的任务（工作线程随旧进程消失）必须
    /// 被标 failed 并释放安装互斥——否则后续 install 永远 409（真机实测：
    /// install 进行中重启 cmsd，任务卡 running、新 install 被拒 install_in_progress）。
    #[test]
    fn reconcile_reaps_orphan_running_tasks() {
        let root = fx::tmpdir("reap");
        let app = fx::make_app(&root, 128, 100);
        // 塞一个 running 的 install 任务（模拟重启前未终结的现场）
        {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard.state.tasks.insert(
                "t-orphan".to_string(),
                crate::store::TaskRecord {
                    id: "t-orphan".to_string(),
                    kind: "install".to_string(),
                    card: "fixture-c".to_string(),
                    status: TaskStatus::Running,
                    created_at: now_epoch(),
                    finished_at: None,
                    error: None,
                    result: None,
                    events: vec![],
                    progress_done_bytes: None,
                    progress_total_bytes: None,
                    store_run_id: None,
                    store_context: None,
                    selection: None,
                },
            );
        }
        let note = reconcile_once(&app);
        assert!(note.contains("孤儿任务收割 1"), "{note}");
        {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            let t = &guard.state.tasks["t-orphan"];
            assert_eq!(t.status, TaskStatus::Failed);
            assert!(t.error.as_deref().unwrap_or("").contains("cmsd 重启"));
        }
        // 互斥随之释放（task_create_install 的检查看 state.tasks 里的 running install）
        assert!(app.task_create_install("fixture-c").is_ok());
        let _ = std::fs::remove_dir_all(&root);
    }

    /// W6a 收编：在跑容器命中容器卡但状态库无记录 → 补登记（只读，不动策略）。
    #[test]
    fn reconcile_adopts_running_container() {
        let root = fx::tmpdir("adopt");
        let app = fx::make_app(&root, 128, 100);
        let port = fx::serve_models_json("Fixture-Model");
        fx::write_docker_card(
            &root.join("cards"),
            "fixture-c",
            "fixture-c-ctr",
            port,
            Some("0.01"),
        );
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            true,
            "unless-stopped",
            0,
            false,
            4321,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );

        let note = reconcile_containers(&app);
        assert!(note.contains("收编 1"), "{note}");
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let m = guard.state.models.get("fixture-c").unwrap();
        assert_eq!(m.engine_type, "container");
        assert_eq!(m.status, ModelStatus::Running);
        assert_eq!(m.container_name.as_deref(), Some("fixture-c-ctr"));
        assert_eq!(m.image.as_deref(), Some("fixture/img:1.0"));
        assert_eq!(m.port, port);
        assert_eq!(m.main_pid, Some(4321));
        assert_eq!(m.model_id, "Fixture-Model");
        assert!(!m.lifecycle_owned, "discovery cannot grant write authority");
        assert!(guard.state.active_model.is_none());
        drop(guard);
        assert_eq!(
            crate::ops::disable(&app, "fixture-c").unwrap_err().1,
            "ownership_required"
        );
        assert_eq!(
            crate::ops::set_default(&app, "fixture-c").unwrap_err().1,
            "ownership_required"
        );
        assert_eq!(
            crate::ops::purge(&app, "fixture-c").unwrap_err().1,
            "ownership_required"
        );
        // A second observation of an external stop must not change its policy.
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            false,
            "unless-stopped",
            143,
            false,
            0,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );
        reconcile_containers(&app);
        // 只读收编：restart 策略原样（没发 update 调用）
        let line = fx::state_line(&root, "fixture-c-ctr").unwrap();
        assert!(line.contains("^unless-stopped^"), "{line}");
        app.save_store();
        let saved = crate::store::StateStore::load(&app.config.state_file());
        assert!(
            !saved.state.models["fixture-c"].lifecycle_owned,
            "read-only survives restart"
        );
        let tid = crate::ops::enable(&app, "fixture-c").unwrap();
        assert_eq!(
            fx::wait_terminal(&app, &tid, 10).status,
            crate::store::TaskStatus::Succeeded
        );
        assert!(
            app.store.lock().unwrap().state.models["fixture-c"].lifecycle_owned,
            "explicit enable owns the lifecycle"
        );
        std::fs::remove_dir_all(&root).ok();
    }

    /// 边界规则：Running 的容器被 docker stop（退出码 143、策略仍在）
    /// ⇒ 绕开管理面的手动关闭 ⇒ 状态落 Stopped + 清自启标记。
    #[test]
    fn reconcile_manual_stop_clears_flag() {
        let (root, app, port) = enabled_fixture("manualstop");
        // 外部 docker stop：容器 exited 143，策略还是 unless-stopped
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            false,
            "unless-stopped",
            143,
            false,
            0,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );

        reconcile_containers(&app);
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let m = guard.state.models.get("fixture-c").unwrap();
        assert_eq!(m.status, ModelStatus::Stopped);
        assert!(m.main_pid.is_none());
        assert!(
            m.degraded_reason.is_none(),
            "手动关闭不该留异常标记：{:?}",
            m.degraded_reason
        );
        assert_eq!(
            guard.state.active_model, None,
            "手动关闭要清活跃指针（与 disable 同语义）"
        );
        drop(guard);
        let line = fx::state_line(&root, "fixture-c-ctr").unwrap();
        assert!(line.contains("^no^"), "标记应已清：{line}");
        std::fs::remove_dir_all(&root).ok();
    }

    /// 残留收敛：对账之前状态就已是 Stopped（旧版品悟 stop.sh / docker stop
    /// 的历史现场），活跃指针还滞留且容器不会自拉（策略 no）⇒ 清掉指针。
    /// r18 #2：非归属容器卡的观察在稳态（status/main_pid 未变）不得置脏
    /// store——原为每轮无条件赋值+mark（2,880 saves/日）。
    #[test]
    fn steady_unowned_container_observation_does_not_remark_the_store_dirty() {
        let (root, app, _port) = enabled_fixture("unowned-steady");
        {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            let model = guard.state.models.get_mut("fixture-c").unwrap();
            model.lifecycle_owned = false;
            model.status = ModelStatus::Stopped;
            model.main_pid = None;
        }
        // 第一次观察对齐运行态（容器已停）并落盘清脏。
        reconcile_containers(&app);
        assert!(app
            .store
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .is_dirty());
        app.store
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .save_if_dirty()
            .unwrap();
        // 稳态复测：同一观察不再置脏。
        for _ in 0..3 {
            reconcile_containers(&app);
        }
        assert!(
            !app.store
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .is_dirty(),
            "稳态非归属容器观察不得再次置脏 store（r18 #2）"
        );
        drop(app);
        std::fs::remove_dir_all(&root).ok();
    }

    #[test]
    fn reconcile_clears_stale_active_pointer_when_not_autostart() {
        let (root, app, port) = enabled_fixture("staleactive");
        // 历史现场：状态已 Stopped（此前的对账已收敛过状态与自启标记），
        // 只有指针还滞留
        {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard.state.models.get_mut("fixture-c").unwrap().status = ModelStatus::Stopped;
        }
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            false,
            "no",
            0,
            false,
            0,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );

        reconcile_containers(&app);
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        assert_eq!(
            guard.state.active_model, None,
            "已停且不会自拉：滞留的活跃指针应清掉"
        );
        drop(guard);
        std::fs::remove_dir_all(&root).ok();
    }

    /// 自动认领：活跃指针空缺 + 恰好一张容器卡在跑且健康 ⇒ 认领为活跃
    /// （旧版品悟 / docker 绕开管理面拉起唯一模型的场景）；指针已在的不动，
    /// 多卡在跑不替选，容器 Up 但引擎未就绪不认（防「连接失败」比
    /// 「无活跃模型」更像坏了）。
    #[test]
    fn reconcile_adopts_sole_running_container_as_active() {
        fn mark_healthy(app: &Arc<App>, port: u16, ok: bool) {
            app.live
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .health
                .insert(
                    port,
                    crate::engine::Health {
                        ok,
                        model_id: None,
                        model_len: None,
                        checked_at: 1,
                        detail: "probe".into(),
                    },
                );
        }
        // 正向：指针空缺 + 唯一在跑且健康 ⇒ 认领
        let (root, app, port) = enabled_fixture("adoptsole");
        {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard.state.active_model = None;
        }
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            true,
            "unless-stopped",
            0,
            false,
            4321,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );
        mark_healthy(&app, port, true);
        reconcile_containers(&app);
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        assert_eq!(
            guard.state.active_model.as_deref(),
            Some("fixture-c"),
            "指针空缺时唯一在跑且健康的卡应被认领为活跃"
        );
        drop(guard);
        std::fs::remove_dir_all(&root).ok();

        // 反向 A：第二张卡也在跑且健康 ⇒ 不替用户做选择
        let (root, app, port) = enabled_fixture("adoptmulti");
        {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard.state.active_model = None;
        }
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            true,
            "unless-stopped",
            0,
            false,
            4321,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );
        fx::write_docker_card(
            &root.join("cards"),
            "fixture-d",
            "fixture-d-ctr",
            port,
            Some("0.01"),
        );
        fx::set_container(
            &root,
            "fixture-d-ctr",
            "fixture/img:1.0",
            true,
            "unless-stopped",
            0,
            false,
            4322,
            port,
            8001,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );
        mark_healthy(&app, port, true);
        mark_healthy(&app, 8001, true);
        reconcile_containers(&app);
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        assert_eq!(
            guard.state.active_model, None,
            "多卡在跑：不替用户选择，指针保持空缺"
        );
        drop(guard);
        std::fs::remove_dir_all(&root).ok();

        // 反向 B：容器 Up 但引擎未就绪（健康探测未过）⇒ 不认领
        let (root, app, port) = enabled_fixture("adoptunready");
        {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard.state.active_model = None;
        }
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            true,
            "unless-stopped",
            0,
            false,
            4321,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );
        mark_healthy(&app, port, false);
        reconcile_containers(&app);
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        assert_eq!(
            guard.state.active_model, None,
            "引擎未就绪：不认领（保持诚实的无活跃模型）"
        );
        drop(guard);
        std::fs::remove_dir_all(&root).ok();
    }

    /// 边界规则：异常死（非零退出 / OOMKilled）⇒ 标记保留 + 可见原因。
    #[test]
    fn reconcile_crash_keeps_flag_marks_reason() {
        for (exit_code, oom, hint) in [(1, false, "异常退出"), (137, true, "OOMKilled")] {
            let (root, app, port) = enabled_fixture("crash");
            fx::set_container(
                &root,
                "fixture-c-ctr",
                "fixture/img:1.0",
                false,
                "unless-stopped",
                exit_code,
                oom,
                0,
                port,
                8000,
                "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
            );

            reconcile_containers(&app);
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            let m = guard.state.models.get("fixture-c").unwrap();
            assert_eq!(m.status, ModelStatus::Stopped, "exit={exit_code}");
            assert_eq!(
                guard.state.active_model.as_deref(),
                Some("fixture-c"),
                "崩溃但策略在：保留活跃指针等 docker 重拉自愈"
            );
            let reason = m.degraded_reason.clone().unwrap_or_default();
            assert!(reason.contains("异常退出"), "{reason}");
            assert!(reason.contains("非管理面停止"), "{reason}");
            if oom {
                assert!(reason.contains(hint), "{reason}");
            }
            drop(guard);
            // 自启标记保留（docker 会按策略拉起）
            let line = fx::state_line(&root, "fixture-c-ctr").unwrap();
            assert!(line.contains("^unless-stopped^"), "{line}");
            std::fs::remove_dir_all(&root).ok();
        }
    }

    /// 容器消失（docker rm）：事实 Stopped + 可见标记。
    #[test]
    fn reconcile_container_gone_marks() {
        let (root, app, _port) = enabled_fixture("gone");
        // 状态文件清空 = 容器没了
        std::fs::write(root.join("docker.state"), "").unwrap();

        reconcile_containers(&app);
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let m = guard.state.models.get("fixture-c").unwrap();
        assert_eq!(m.status, ModelStatus::Stopped);
        assert!(
            m.degraded_reason
                .as_deref()
                .unwrap_or("")
                .contains("不存在"),
            "{:?}",
            m.degraded_reason
        );
        std::fs::remove_dir_all(&root).ok();
    }

    /// docker daemon 查不到（scan 失败）：整段跳过，绝不拿「没数据」收敛状态。
    #[test]
    fn reconcile_skips_when_scan_fails() {
        let (root, app, _port) = enabled_fixture("scanfail");
        // 把 fake docker 换成永远失败的脚本（tmp+rename：直接覆写执行中的脚本
        // 偶发 ETXTBSY，与 write_fake_docker 同款防护）
        std::fs::write(root.join("docker.tmp"), "#!/bin/sh\nexit 1\n").unwrap();
        std::fs::rename(root.join("docker.tmp"), root.join("docker")).unwrap();

        let note = reconcile_containers(&app);
        assert!(note.is_empty(), "{note}");
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let m = guard.state.models.get("fixture-c").unwrap();
        assert_eq!(m.status, ModelStatus::Running, "scan 失败不得动状态");
        std::fs::remove_dir_all(&root).ok();
    }
}

#[cfg(test)]
mod dwell_tests {
    use super::*;
    use crate::docker::fixtures as fx;
    use crate::store::TaskStatus;

    /// 驻留改判：非零退出（如 docker stop 被 SIGKILL 落地的 137）先按异常死标记，
    /// 120s 后仍 exited ⇒ docker 的崩溃重拉早该发生 ⇒ 改判手动关闭清标记。
    #[test]
    fn reconcile_dwell_converts_to_manual_stop() {
        let root = fx::tmpdir("dwell");
        let app = fx::make_app(&root, 128, 100);
        let port = fx::serve_models_json("Fixture-Model");
        fx::write_docker_card(
            &root.join("cards"),
            "fixture-c",
            "fixture-c-ctr",
            port,
            Some("0.01"),
        );
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            false,
            "no",
            0,
            false,
            0,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );
        let tid = ops::enable(&app, "fixture-c").unwrap();
        {
            let t = fx::wait_terminal(&app, &tid, 30);
            assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        }

        // Reconstruct the old persisted policy for pre-migration dwell handling.
        std::fs::remove_file(app.config.state_dir.join("recovery/fixture-c.json")).unwrap();
        // 外部停容器但落得 137（SIGKILL 落地——exit 143 判据接不住的形状）
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            false,
            "unless-stopped",
            137,
            false,
            0,
            port,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );
        // 第一次见到：异常死标记，标记保留
        reconcile_containers(&app);
        {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            let m = guard.state.models.get("fixture-c").unwrap();
            assert!(m
                .degraded_reason
                .as_deref()
                .unwrap_or("")
                .contains("异常退出"));
        }
        assert!(fx::state_line(&root, "fixture-c-ctr")
            .unwrap()
            .contains("^unless-stopped^"));
        // 驻留计时拨过 120s：改判手动关闭，清标记、清异常标记
        {
            let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
            live.container_stop_candidates
                .insert("fixture-c".to_string(), now_epoch() - 121);
        }
        reconcile_containers(&app);
        {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            let m = guard.state.models.get("fixture-c").unwrap();
            assert_eq!(m.status, ModelStatus::Stopped);
            assert!(
                m.degraded_reason.is_none(),
                "手动关闭落定后不该留异常标记：{:?}",
                m.degraded_reason
            );
        }
        let line = fx::state_line(&root, "fixture-c-ctr").unwrap();
        assert!(line.contains("^no^"), "{line}");
        std::fs::remove_dir_all(&root).ok();
    }
}

#[cfg(test)]
mod compose_adopt_tests {
    use super::*;
    use crate::docker::fixtures as fx;

    const SHA: &str = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd";

    /// compose 一代卡的收编：host 网络容器（无 PortBindings）时端口回落到 .env PORT。
    #[test]
    fn reconcile_adopt_compose_host_network() {
        let root = fx::tmpdir("adopt-compose");
        let app = fx::make_app(&root, 128, 100);
        let vendor = root.join("vendor");
        fx::write_compose_card(
            &root.join("cards"),
            "mtp",
            "fixture-mtp",
            &vendor,
            &root.join("models"),
            30000,
            SHA,
        );
        std::fs::create_dir_all(&vendor).unwrap();
        std::fs::write(
            vendor.join(".env"),
            "IMAGE=fixture/sglang:v0.5.16\nCONTAINER_NAME=fixture-mtp\nMODEL_PATH=/x\nHOST_BIND=127.0.0.1\nPORT=30000\nMEM_FRACTION_STATIC=0.70\n",
        )
        .unwrap();
        // host 网络：state 行 hostport 字段空 → PortBindings {} → host_port None
        fx::set_container(
            &root,
            "fixture-mtp",
            "fixture/sglang:v0.5.16",
            true,
            "unless-stopped",
            0,
            false,
            4321,
            0,
            0,
            "python3,-m,sglang.launch_server,--port=30000,--mem-fraction-static=0.70",
        );

        let note = reconcile_containers(&app);
        assert!(note.contains("收编 1"), "{note}");
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let m = guard.state.models.get("mtp").unwrap();
        assert_eq!(m.engine_type, "container");
        assert_eq!(m.status, ModelStatus::Running);
        assert_eq!(m.port, 30000, "host 网络卡端口回落到 .env PORT");
        assert_eq!(m.main_pid, Some(4321));
        // compose 渲染的连写参数里没有 --served-model-name：真名留空等健康探测回填
        assert!(m.model_id.is_empty(), "{}", m.model_id);
        std::fs::remove_dir_all(&root).ok();
    }
}
