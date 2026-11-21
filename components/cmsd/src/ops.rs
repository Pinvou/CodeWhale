//! 模型操作：enable / disable / purge（T3、T5 预算、T9）。
//!
//! 长操作一律任务化：API 返回任务句柄，工作线程推进并写事件（T7），
//! SSE 订阅者实时看到进度。enable 幂等：已在跑且健康 → 直接成功，不重启
//! （权重 22GB，重启代价分钟级）。

use crate::app::{api_err, log_line, ApiErr, App};
use crate::capacity;
use crate::cards::{scan_cards, Card};
use crate::engine;
use crate::redact::redact_text;
use crate::store::{ExternalSighting, ModelState, ModelStatus};
use crate::systemd::env_file_content;
use crate::timeutil::now_epoch;
use std::path::{Path, PathBuf};
use std::sync::Arc;
#[cfg(test)]
mod native_handoff_tests;
mod native_profile;
mod purge_target;
#[cfg(test)]
mod purge_tests;
mod script_output;
#[cfg(test)]
mod store_root_tests;

const MAX_CONTEXT: u32 = 32768;

fn arm_recovery(app: &Arc<App>, task: &str, card: &str) -> Result<(), String> {
    let automatic = app
        .store
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .state
        .tasks
        .get(task)
        .is_some_and(|t| t.kind == "recover");
    crate::recovery::arm(app, card, automatic)
        .map_err(|(_, code, message)| format!("{code}: {message}"))
}
fn disarm_recovery(app: &Arc<App>, card: &str) -> Result<(), String> {
    let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    crate::recovery::disarm(app, card).map_err(|(_, code, message)| format!("{code}: {message}"))
}

fn require_lifecycle_owner(model: &ModelState) -> Result<(), ApiErr> {
    if model.lifecycle_owned {
        return Ok(());
    }
    Err(api_err(409, "ownership_required", "此资源仅被发现，尚未授予 cmsd 启停或默认切换权。请核实标准卡后显式启用并接管；不要回退旧脚本。"))
}

pub(crate) fn find_card(app: &App, card_name: &str) -> Result<Card, ApiErr> {
    find_card_raw(app, card_name)
}

/// Internal physical lookup. Public mutations must first hold a selection guard.
pub(crate) fn find_card_raw(app: &App, card_name: &str) -> Result<Card, ApiErr> {
    scan_cards(&app.config.cards_dir)
        .into_iter()
        .find(|c| c.name == card_name)
        .ok_or_else(|| {
            api_err(
                404,
                "unknown_card",
                &format!(
                    "不认识卡片 {card_name}（在 {} 下没找到，或不是原生驭光卡）",
                    app.config.cards_dir.display()
                ),
            )
        })
}

/// POST /v1/models/{card}/enable
pub fn enable(app: &Arc<App>, card_name: &str) -> Result<String, ApiErr> {
    enable_with_kind(app, card_name, "enable")
}

pub fn enable_automatic(app: &Arc<App>, card_name: &str) -> Result<String, ApiErr> {
    enable_with_kind(app, card_name, "recover")
}

fn enable_with_kind(app: &Arc<App>, card_name: &str, kind: &str) -> Result<String, ApiErr> {
    let selected = crate::store_adoption::selection::admit(app, card_name)?;
    let card_name = selected.card.as_str();
    let card = find_card(app, card_name)?;
    // W6：容器卡走容器生命周期（docker 托管，不碰 systemd）
    if card.engine_kind == crate::cards::EngineKind::Container {
        return enable_container(app, card, kind, selected);
    }
    let deploy = card.deploy.clone().ok_or_else(|| {
        api_err(
            409,
            "not_deployed",
            &format!("卡片 {card_name} 还没有部署过（缺 .deploy-env），请先在品悟商店完成「部署」"),
        )
    })?;
    let engine_bin = card.engine_bin().ok_or_else(|| {
        api_err(
            409,
            "engine_missing",
            &format!(
                "引擎未就绪，请先完成「安装引擎」（缺 {}）",
                card.engine_dir()
                    .map(|p| p.display().to_string())
                    .unwrap_or_default()
            ),
        )
    })?;
    if !engine_bin.exists() {
        return Err(api_err(
            409,
            "engine_missing",
            &format!("引擎未就绪：{} 不存在", engine_bin.display()),
        ));
    }
    if !card.weights_complete() {
        return Err(api_err(
            409,
            "weights_incomplete",
            &format!(
                "权重不完整：{}（期望 {} 字节），请先完成「下载」",
                card.weights_path()
                    .map(|p| p.display().to_string())
                    .unwrap_or_default(),
                card.total_bytes
            ),
        ));
    }
    if let Some(tid) = app.task_running_for(card_name) {
        return Err(api_err(
            409,
            "task_in_progress",
            &format!("卡片 {card_name} 已有进行中的任务 {tid}，请等它结束或查 /v1/tasks/{tid}"),
        ));
    }

    let task_id = app.task_create_exclusive(kind, card_name)?;
    selected.pin_task(app, &task_id)?;
    let app2 = Arc::clone(app);
    let tid = task_id.clone();
    std::thread::spawn(move || enable_worker(&app2, &tid, &card, &deploy, selected));
    Ok(task_id)
}

fn enable_worker(
    app: &Arc<App>,
    task_id: &str,
    card: &Card,
    deploy: &crate::cards::DeployEnv,
    selected: crate::store_adoption::selection::Guard,
) {
    let outcome = (|| {
        selected.verify(app)?;
        selected.retire_related(app, task_id)?;
        let message = enable_inner(app, task_id, card, deploy)?;
        selected.inherit_boot(app)?;
        Ok(message)
    })();
    match &outcome {
        Ok(msg) => log_line(&format!("enable {} 成功：{msg}", card.name)),
        Err(msg) => log_line(&format!("enable {} 失败：{msg}", card.name)),
    }
    app.task_finish(task_id, outcome);
    app.save_store();
}

/// 把就绪/健康结论写入 supervisor 的健康缓存（`live.health`——/v1/models 的
/// healthy 字段来源）。就绪判定刚通过时周期探测器往往还没跑到这个端口，
/// 不 priming 的话，任务终结后立刻来查的调用方会拿到 healthy=null
/// （真机实证 2026-10-15：换胎 14:04 就绪，首个缓存记录 14:29 才出现，
/// 品悟的换胎后自动接入因 healthy≠true 静默放弃）。
fn prime_health_cache(
    app: &Arc<App>,
    port: u16,
    health: crate::engine::Health,
    pid: Option<u32>,
) -> Result<(), String> {
    let key = pid
        .map(|pid| {
            crate::store_adoption::runtime::instance_key(app, pid)
                .ok_or("native_instance_generation_unverified")
        })
        .transpose()?;
    let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
    live.health.insert(port, health);
    if let Some(key) = key {
        live.health_instances.insert(port, key);
    } else {
        live.health_instances.remove(&port);
    }
    Ok(())
}

/// Publish health together with the verified container generation so the API
/// can distinguish a healthy endpoint from a matching current deployment.
fn prime_container_health(
    app: &Arc<App>,
    card: &Card,
    port: u16,
    health: crate::engine::Health,
) -> Result<crate::docker::ContainerInfo, String> {
    let container = crate::container_runtime::inspect(app, card, None)?
        .ok_or("runtime_missing: 容器已不存在")?;
    if !container.running || container.pid == 0 {
        return Err("configuration_changed: 容器当前未运行".into());
    }
    prime_health_cache(app, port, health, None)?;
    app.live
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .containers
        .insert(container.name.clone(), container.clone());
    Ok(container)
}

/// Prove any existing activity before a request writes control state. Managed
/// units may use a previously allocated port instead of the card's preference;
/// keep their cgroup/binding proof and actual registered port. New unbound
/// candidates use the independent read-only store discovery proof.
fn verify_native_runtime(
    app: &Arc<App>,
    card: &Card,
    profile: &native_profile::Profile,
) -> Result<Option<crate::store_adoption::runtime::Binding>, String> {
    let adopted = crate::store_adoption::native_state(app, &card.name)?;
    let unit = app.systemd.show(&app.systemd.unit_name_for(&card.name))?;
    let port = current_port(app, &card.name)
        .filter(|port| *port > 0)
        .or_else(|| card.deploy.as_ref().map(|deploy| deploy.port))
        .ok_or("native_instance_mismatch: 缺少原生端口")?;
    let mut active = false;
    for state in std::iter::once(unit).chain(adopted) {
        if state.is_active() || state.main_pid > 0 {
            active = true;
            if !state.is_active()
                || !native_profile::matches(app, card, port, state.main_pid, profile)
            {
                return Err(
                    "native_instance_mismatch: 现有原生实例的完整部署身份未匹配，未获取控制权"
                        .into(),
                );
            }
        }
    }
    if !active {
        return crate::store_adoption::runtime::discover(app, card);
    }
    Ok(None)
}

fn enable_inner(
    app: &Arc<App>,
    task_id: &str,
    card: &Card,
    deploy: &crate::cards::DeployEnv,
) -> Result<String, String> {
    crate::store_adoption::selection::ensure_current(app, &card.name)?;
    let profile = native_profile::resolve(app, card)?;
    // A same-named active unit is not an ownership grant. Validate existing
    // bindings/candidates without writing units, recovery policy or ownership.
    let native_binding = verify_native_runtime(app, card, &profile)?;
    if !native_profile::recorded(app, card, &profile) {
        upsert_model_state(app, &card.name, |model| {
            model.measured_footprint_gib = None;
            model.engine_rss_gib = None;
        });
    }
    if app
        .systemd
        .write_template()
        .map_err(|error| error.to_string())?
    {
        app.systemd.daemon_reload()?;
    }
    let unit = app.systemd.unit_name_for(&card.name);
    let started = now_epoch();
    let event = |stage: &str, msg: &str| app.task_event(task_id, stage, msg);

    // Discovery is read-only. Only the authorized mutation publishes its exact
    // binding, before recovery can claim policy or start a second instance.
    if let Some(binding) = native_binding {
        crate::store_adoption::runtime::save(app, &card.name, &binding)?;
        crate::store_adoption::runtime::fence(app, &card.name)?;
    }
    // Explicit local enable also adopts a previously observation-only native unit.
    upsert_model_state(app, &card.name, |m| m.lifecycle_owned = true);
    arm_recovery(app, task_id, &card.name)?;

    if let Some(message) = crate::store_adoption::try_running_native(app, card)? {
        native_profile::record(app, card, &profile)?;
        set_active_if_free(app, &card.name);
        return Ok(message);
    }

    // ---- 幂等优先：已在跑且健康 → 直接成功，不重启（已在跑就不再需要新内存）----
    let unit_state = app.systemd.show(&unit).unwrap_or_default();
    if unit_state.is_active() {
        let port = current_port(app, &card.name).unwrap_or(deploy.port);
        if !native_profile::matches(app, card, port, unit_state.main_pid, &profile) {
            return Err("native_instance_mismatch: 实际引擎、权重或启动参数与当前部署不一致；未按同名端口认定成功，请核对并重新部署".into());
        }
        let health = engine::check_health(port, Some(&deploy.model_id));
        if health.ok {
            native_profile::record(app, card, &profile)?;
            prime_health_cache(app, port, health, Some(unit_state.main_pid))?;
            upsert_model_state(app, &card.name, |m| {
                m.status = ModelStatus::Running;
                m.port = port;
                m.main_pid = Some(unit_state.main_pid).filter(|p| *p > 0);
            });
            set_active_if_free(app, &card.name);
            event(
                "idempotent",
                "服务本来就在跑且健康，幂等成功，不重启（重启要重载 22GB 权重）",
            );
            return Ok(format!(
                "服务已在运行：http://127.0.0.1:{port}/v1（模型标识 {}）",
                deploy.model_id
            ));
        }
        event("restart", "服务在跑但不健康，按卡片范式先停掉再重新拉起");
        let _ = app.systemd.stop(&unit);
        wait_unit_inactive(app, &unit, 130);
    }

    // ---- T5 预算检查：先看内存，再谈启动（红线 2：真实内存数据核预算）----
    let measured = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .models
            .get(&card.name)
            .and_then(|m| m.measured_footprint_gib)
    }
    .filter(|_| native_profile::recorded(app, card, &profile));
    let mem = capacity::read_meminfo(&app.config.meminfo_path)
        .map_err(|e| format!("读 {} 失败：{e}", app.config.meminfo_path.display()))?;
    let budget = budget_for(app, card, &mem, measured);
    event("capacity", &budget.verdict);
    if !budget.ok {
        return Err(budget.verdict);
    }

    // ---- degraded 恢复：显式 enable 是唯一出口 ----
    let was_degraded = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        matches!(
            guard.state.models.get(&card.name).map(|m| m.status),
            Some(ModelStatus::Degraded)
        )
    };
    if was_degraded {
        event(
            "recover",
            "模型处于 degraded，按显式指令清除熔断记录并重新尝试",
        );
        let _ = app.systemd.reset_failed(&unit);
        upsert_model_state(app, &card.name, |m| {
            m.degraded_reason = None;
        });
    }

    // ---- D5 端口分配：优先卡片部署端口，撞了才从池里发 ----
    let port = allocate_port(app, &card.name, deploy.port)?;
    event(
        "port",
        &format!(
            "端口 {port}（原生池 {}-{}）",
            app.config.pool_start, app.config.pool_end
        ),
    );

    // ---- 基线内存（V2 标定：就绪后测差值）----
    let baseline_available_gib = capacity::read_meminfo(&app.config.meminfo_path)
        .map(|m| m.available_gib())
        .unwrap_or(0.0);

    // ---- 生成 env 文件 + 模板单元，登记自启并拉起 ----
    let env_dir = app.config.env_dir();
    std::fs::create_dir_all(&env_dir)
        .map_err(|e| format!("创建 {} 失败：{e}", env_dir.display()))?;
    let env_path = env_dir.join(format!("{}.env", card.name));
    let extra_flags = profile.extras.clone();
    let mut effective_deploy = deploy.clone();
    effective_deploy.max_concurrency = profile.concurrency;
    let mut environment = env_file_content(&effective_deploy, port, profile.context, &extra_flags);
    environment.push_str(&format!(
        "DEVICE_STATE_SLOTS={}\nHOST_STATE_SLOTS={}\n",
        profile.device_slots, profile.host_slots
    ));
    if !native_profile::recorded(app, card, &profile) {
        upsert_model_state(app, &card.name, |model| {
            model.measured_footprint_gib = None;
            model.engine_rss_gib = None;
        });
        app.live
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .health
            .remove(&port);
    }
    native_profile::record(app, card, &profile)?;
    for (key, value) in crate::cards::native_environment(&app.config.cards_dir.join(&card.name))? {
        environment.push_str(&format!("{key}={value}\n"));
    }
    std::fs::write(&env_path, environment)
        .map_err(|e| format!("写 {} 失败：{e}", env_path.display()))?;
    // r9 P2-1：extra_flags 可含 --api-key 形态值（卡片脚本提取或管理员覆盖
    // 文件），进持久事件与 SSE 前过脱敏——redact.rs 对该形态有掩码能力。
    event(
        "flags",
        &format!(
            "serve 旗标：{}",
            if extra_flags.is_empty() {
                "（引擎默认）".to_string()
            } else {
                redact_text(&extra_flags)
            }
        ),
    );
    if app
        .systemd
        .write_template()
        .map_err(|e| format!("生成模板单元失败：{e}"))?
    {
        app.systemd.daemon_reload()?;
        event("systemd", "模板单元已生成/更新");
    }
    app.systemd.fence_recovery(&unit)?;
    let _ = app.systemd.reset_failed(&unit); // 清掉历史失败计数，给本次启动干净起点
    crate::store_adoption::runtime::record_native_unit(app, &card.name, Some(&unit))?;
    app.systemd
        .start(&unit)
        .map_err(|e| format!("systemd 拉起失败：{e}"))?;
    event(
        "systemd",
        &format!("已生成 {unit} 并拉起（cmsd统一预算与崩溃恢复）"),
    );

    upsert_model_state(app, &card.name, |m| {
        m.status = ModelStatus::Starting;
        m.port = port;
        m.enabled_at = m.enabled_at.or(Some(now_epoch()));
        m.degraded_reason = None;
    });

    // ---- 就绪等待：25 分钟上限，心跳复用引擎日志（卡片范式）----
    let deadline = started + app.config.ready_timeout_secs;
    let mut last_heartbeat = 0u64;
    loop {
        let now = now_epoch();
        if now >= deadline {
            let logs = app.systemd.journal_tail(&unit, 30).unwrap_or_default();
            let _ = app.systemd.stop(&unit);
            upsert_model_state(app, &card.name, |m| {
                m.status = ModelStatus::Degraded;
                m.degraded_reason = Some("25 分钟内没有就绪，已停止等待并停掉引擎".to_string());
            });
            return Err(format!(
                "25 分钟内服务没有就绪，已停止等待并停掉引擎。最后日志：\n{}",
                redact_text(&tail_lines(&logs, 30))
            ));
        }
        let us = app.systemd.show(&unit).unwrap_or_default();
        if us.active_state == "failed" || (us.active_state == "inactive" && now > started + 5) {
            let logs = app.systemd.journal_tail(&unit, 30).unwrap_or_default();
            let limit_hit = app.systemd.start_limit_hit(&unit, &us);
            let reason = if limit_hit {
                format!(
                    "启动反复失败，systemd 已熔断（5 分钟内 3 次，不再自动拉起；修复后用 enable 恢复）。最后日志：\n{}",
                    redact_text(&tail_lines(&logs, 30))
                )
            } else {
                format!(
                    "引擎进程启动后退出（结果 {}）。最后日志：\n{}",
                    us.result,
                    redact_text(&tail_lines(&logs, 30))
                )
            };
            upsert_model_state(app, &card.name, |m| {
                m.status = ModelStatus::Degraded;
                m.degraded_reason = Some(if limit_hit {
                    "5 分钟内启动失败 3 次，systemd 已熔断，不再自动拉起。修复后用 enable 恢复"
                        .to_string()
                } else {
                    degrade_reason_text(&us)
                });
            });
            return Err(reason);
        }
        let health = engine::check_health(port, Some(&deploy.model_id));
        if health.ok
            && us.is_active()
            && native_profile::matches(app, card, port, us.main_pid, &profile)
        {
            prime_health_cache(app, port, health, Some(us.main_pid))?;
            break;
        }
        if now >= last_heartbeat + 15 {
            last_heartbeat = now;
            let log_hint = app
                .systemd
                .journal_last_line(&unit)
                .map(|l| format!("（引擎最近日志：{}）", redact_text(&l)))
                .unwrap_or_default();
            event(
                "wait",
                &format!(
                    "等待服务就绪…已用时 {}s{log_hint}",
                    now.saturating_sub(started)
                ),
            );
        }
        std::thread::sleep(std::time::Duration::from_secs(5));
    }

    // ---- 就绪：模型 id 已由健康判定核对；登记状态、活跃指针、实测标定 ----
    let after_available_gib = capacity::read_meminfo(&app.config.meminfo_path)
        .map(|m| m.available_gib())
        .unwrap_or(0.0);
    let footprint = (baseline_available_gib - after_available_gib).max(0.0);
    let us = app.systemd.show(&unit).unwrap_or_default();
    let rss = if us.main_pid > 0 {
        engine::proc_rss_gib(&app.config.proc_root, us.main_pid)
    } else {
        None
    };
    upsert_model_state(app, &card.name, |m| {
        m.status = ModelStatus::Running;
        m.port = port;
        m.main_pid = Some(us.main_pid).filter(|p| *p > 0);
        m.last_ready_at = Some(now_epoch());
        m.measured_footprint_gib = Some((footprint * 10.0).round() / 10.0);
        m.engine_rss_gib = rss.map(|r| (r * 10.0).round() / 10.0);
    });
    set_active_if_free(app, &card.name);
    event(
        "ready",
        &format!(
            "服务已就绪：http://127.0.0.1:{port}/v1（模型标识 {}）",
            deploy.model_id
        ),
    );
    event(
        "calibrate",
        &format!(
            "实测标定：常驻占用约 {footprint:.1} GiB（就绪前后系统可用内存差），引擎 RSS {:.1} GiB",
            rss.unwrap_or(0.0)
        ),
    );
    Ok(format!(
        "服务已就绪：http://127.0.0.1:{port}/v1（模型标识 {}），用时 {}s",
        deploy.model_id,
        now_epoch().saturating_sub(started)
    ))
}

/// POST /v1/models/{card}/disable
pub fn disable(app: &Arc<App>, card_name: &str) -> Result<String, ApiErr> {
    disable_with_maintenance(app, card_name, None)
}

pub fn disable_with_maintenance(
    app: &Arc<App>,
    card_name: &str,
    owner: Option<&str>,
) -> Result<String, ApiErr> {
    if let Some(owner) = owner {
        if let Some(task) =
            crate::model_maintenance::scoped_action(app, card_name, owner, "disable")?
        {
            return Ok(task);
        }
    }
    // A trusted maintenance owner already pins a physical instance. Its store
    // writer holds the exclusive resource lock; never acquire it recursively.
    let selected = if owner.is_none() {
        Some(crate::store_adoption::selection::admit_stop(
            app, card_name,
        )?)
    } else {
        None
    };
    let card_name = selected
        .as_ref()
        .map(|guard| guard.card.as_str())
        .unwrap_or(card_name);
    let engine_type = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let Some(m) = guard.state.models.get(card_name) else {
            return Err(api_err(
                404,
                "unknown_model",
                &format!("{card_name} 不在 cmsd 登记册里（从未 enable 过或已 purge）"),
            ));
        };
        require_lifecycle_owner(m)?;
        m.engine_type.clone()
    };
    if let Some(tid) = app.task_running_for(card_name) {
        return Err(api_err(
            409,
            "task_in_progress",
            &format!("已有进行中的任务 {tid}"),
        ));
    }
    let task_id = app.task_create_exclusive_with_maintenance("disable", card_name, owner)?;
    let app2 = Arc::clone(app);
    let tid = task_id.clone();
    let card = card_name.to_string();
    std::thread::spawn(move || {
        let outcome = (|| {
            if let Some(selected) = selected.as_ref() {
                selected.disarm_boot(&app2)?;
                selected.retire_related(&app2, &tid)?;
            }
            // W6：按引擎形态分发（容器 = docker stop + 撤 restart 策略）
            match engine_type.as_str() {
                "container" => disable_container_inner(&app2, &tid, &card),
                _ => disable_inner(&app2, &tid, &card),
            }
        })();
        match &outcome {
            Ok(msg) => log_line(&format!("disable {card} 成功：{msg}")),
            Err(msg) => log_line(&format!("disable {card} 失败：{msg}")),
        }
        app2.task_finish(&tid, outcome);
        app2.save_store();
    });
    Ok(task_id)
}

fn disable_inner(app: &Arc<App>, task_id: &str, card_name: &str) -> Result<String, String> {
    app.systemd.assert_template_owner()?;
    disarm_recovery(app, card_name)?;
    crate::store_adoption::stop_native(app, card_name)?;
    let unit = app.systemd.unit_name_for(card_name);
    app.task_event(
        task_id,
        "stop",
        &format!("停止 {unit}（引擎有 120s 体面退出窗口）"),
    );
    let _ = app.systemd.stop(&unit);
    if !wait_unit_inactive(app, &unit, 130) {
        return Err("130s 内引擎没有停下来，请检查 systemctl status".to_string());
    }
    let _ = app.systemd.disable(&unit);
    let _ = app.systemd.reset_failed(&unit);
    upsert_model_state(app, card_name, |m| {
        m.status = ModelStatus::Stopped;
        m.main_pid = None;
    });
    {
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        if guard.state.active_model.as_deref() == Some(card_name) {
            guard.state.active_model = None;
            guard.mark_dirty();
        }
    }
    app.task_event(task_id, "stopped", "引擎已停止，开机自启登记已撤销");
    Ok("已停止".to_string())
}

// ---------------------------------------------------------------------------
// W6 容器卡生命周期：enable/disable 的 docker 面。
// 明确托管对象由cmsd持久恢复意图与统一预算管理，后端独立重启关闭。
// 只读发现保留外部策略；启停、换胎和回滚复用同一准入。
// ---------------------------------------------------------------------------

/// POST /v1/models/{card}/enable 的容器卡入口（enable() 顶部分发过来）。
fn enable_container(
    app: &Arc<App>,
    card: Card,
    kind: &str,
    selected: crate::store_adoption::selection::Guard,
) -> Result<String, ApiErr> {
    if card.docker_deploy.is_none() {
        return Err(api_err(
            409,
            "not_deployed",
            &format!(
                "卡片 {} 还没有部署过（缺 .deploy-env），请先在品悟商店完成「部署」",
                card.name
            ),
        ));
    }
    if let Some(tid) = app.task_running_for(&card.name) {
        return Err(api_err(
            409,
            "task_in_progress",
            &format!(
                "卡片 {} 已有进行中的任务 {tid}，请等它结束或查 /v1/tasks/{tid}",
                card.name
            ),
        ));
    }
    let task_id = app.task_create_exclusive(kind, &card.name)?;
    selected.pin_task(app, &task_id)?;
    let app2 = Arc::clone(app);
    let tid = task_id.clone();
    std::thread::spawn(move || {
        let outcome: Result<String, String> = (|| {
            selected.verify(&app2)?;
            selected.retire_related(&app2, &tid)?;
            let message = enable_container_inner(&app2, &tid, &card)?;
            selected.inherit_boot(&app2)?;
            Ok(message)
        })();
        match &outcome {
            Ok(msg) => log_line(&format!("enable {} 成功：{msg}", card.name)),
            Err(msg) => log_line(&format!("enable {} 失败：{msg}", card.name)),
        }
        if let Err(reason) = &outcome {
            upsert_model_state(&app2, &card.name, |m| {
                if m.status == ModelStatus::Starting {
                    m.status = ModelStatus::Degraded;
                    m.degraded_reason = Some(reason.clone());
                }
            });
        }
        app2.task_finish(&tid, outcome);
        app2.save_store();
    });
    Ok(task_id)
}

/// docker start reuses the immutable old container configuration. Refuse known
/// drift before changing restart policy or claiming an idempotent start.
pub(crate) fn validate_container_config(
    deploy: &crate::cards::DockerDeploy,
    container: &crate::docker::ContainerInfo,
) -> Result<(), String> {
    let invalid = || {
        "configuration_changed: 容器镜像、端口、上下文或运行参数与部署记录不一致。请在模型商店核实并重新部署；不会直接启动旧配置或删除原容器".to_owned()
    };
    let scalar =
        |name: &str| crate::docker::unique_scalar(&container.args, name).map_err(|_| invalid());
    let port = scalar("--port")?
        .map(|value| {
            value
                .parse::<u16>()
                .ok()
                .filter(|port| *port > 0)
                .ok_or_else(invalid)
        })
        .transpose()?;
    let fraction = scalar("--mem-fraction-static")?
        .map(|value| {
            value
                .parse::<f64>()
                .ok()
                .filter(|value| value.is_finite() && *value > 0.0 && *value <= 1.0)
                .ok_or_else(invalid)
        })
        .transpose()?;
    let mut drift = container.image != deploy.image
        || (deploy.host_bind.is_some() && port != Some(deploy.port))
        || deploy.mem_fraction_static.is_some_and(|expected| {
            !expected.is_finite()
                || expected <= 0.0
                || expected > 1.0
                || fraction.is_none_or(|actual| (actual - expected).abs() > 0.00001)
        });
    for (flag, expected) in &deploy.runtime_flags {
        let numeric = matches!(
            flag.as_str(),
            "--context-length" | "--max-model-len" | "--max-running-requests"
        );
        drift |= expected.is_empty()
            || (numeric && expected.parse::<u32>().ok().is_none_or(|value| value == 0))
            || scalar(flag)?.as_deref() != Some(expected.as_str());
    }
    if drift {
        return Err(invalid());
    }
    Ok(())
}

fn enable_container_inner(app: &Arc<App>, task_id: &str, card: &Card) -> Result<String, String> {
    crate::store_adoption::selection::ensure_current(app, &card.name)?;
    let mut started = now_epoch();
    let event = |stage: &str, msg: &str| app.task_event(task_id, stage, msg);
    let deploy = card
        .docker_deploy
        .clone()
        .ok_or_else(|| "卡片还没有部署过（缺 .deploy-env）".to_string())?;
    let name = card
        .container_name
        .clone()
        .unwrap_or_else(|| deploy.container_name.clone());
    let port = deploy.port;
    let recorded = app
        .store
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .state
        .models
        .get(&card.name)
        .cloned();
    let inspected = crate::container_runtime::inspect(app, card, recorded.as_ref())?;
    // Resolve before acquiring ownership or changing recovery policy. An old
    // receipt without its root cannot redirect first creation to new settings.
    let creation_environment = if inspected.is_none() {
        crate::store_adoption::selection::creation_models_root(app, &card.name)?
            .map(|root| vec![("LMM_MODELS_DIR", root.to_string_lossy().into_owned())])
            .unwrap_or_default()
    } else {
        Vec::new()
    };
    upsert_model_state(app, &card.name, |m| {
        m.engine_type = "container".to_string();
        m.container_name = Some(name.clone());
        m.image = Some(deploy.image.clone());
        m.port = port;
    });
    // An explicit enable is the local opt-in to manage this standard card.
    // Merely discovering the matching container never reaches this point.
    upsert_model_state(app, &card.name, |m| m.lifecycle_owned = true);
    arm_recovery(app, task_id, &card.name)?;

    // ---- 幂等：在跑且健康 → 确认自启标记后直接成功，不重启（语义 1：启用=挂标记）----
    if let Some(c) = &inspected {
        if c.running {
            ensure_autostart(app, task_id, card)?;
            // 幂等判定的健康探测给几次机会：瞬时连接失败不该把「在跑且健康」
            // 误降级成完整的启动等待路径（并行高负载下实测偶发）
            let mut health = engine::check_health(port, None);
            for _ in 0..3 {
                if health.ok {
                    break;
                }
                std::thread::sleep(std::time::Duration::from_secs(2));
                health = engine::check_health(port, None);
            }
            if health.ok {
                let c = prime_container_health(app, card, port, health.clone())?;
                let model_id = health
                    .model_id
                    .clone()
                    .or_else(|| c.served_model_name())
                    .unwrap_or_default();
                upsert_model_state(app, &card.name, |m| {
                    m.status = ModelStatus::Running;
                    m.main_pid = Some(c.pid).filter(|p| *p > 0);
                    if !model_id.is_empty() {
                        m.model_id = model_id.clone();
                    }
                    m.degraded_reason = None;
                });
                set_active_if_free(app, &card.name);
                event(
                    "idempotent",
                    "容器本来就在跑且健康，自启标记已确认，幂等成功（不重启）",
                );
                return Ok(format!(
                    "服务已在运行：http://127.0.0.1:{port}/v1（模型标识 {model_id}）"
                ));
            }
            // 在跑但服务未就绪：多半还在加载（GB10 上首启约 5 分钟），不重启，等它
            event(
                "wait",
                "容器在跑但服务未就绪（可能仍在加载权重/CUDA graph），继续等待",
            );
            let baseline = capacity::read_meminfo(&app.config.meminfo_path)
                .map(|m| m.available_gib())
                .unwrap_or(0.0);
            return wait_container_ready(app, task_id, card, &name, port, started, baseline);
        }
    }

    // ---- 预算（容器口径链：--mem-fraction-static × 总量 > 实测标定 > 权重+开销）----
    let mem = capacity::read_meminfo(&app.config.meminfo_path)
        .map_err(|e| format!("读 {} 失败：{e}", app.config.meminfo_path.display()))?;
    let measured = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .models
            .get(&card.name)
            .and_then(|m| m.measured_footprint_gib)
    };
    let (required, basis, note) =
        required_gib_container(card, inspected.as_ref(), mem.total_gib(), measured);
    let budget = capacity::evaluate_with_required(
        &mem,
        required,
        basis,
        &note,
        app.config.debug_available_mib,
    );
    event("capacity", &budget.verdict);
    if !budget.ok {
        return Err(budget.verdict);
    }
    let baseline_available = capacity::read_meminfo(&app.config.meminfo_path)
        .map(|m| m.available_gib())
        .unwrap_or(0.0);

    upsert_model_state(app, &card.name, |m| {
        m.status = ModelStatus::Starting;
        m.degraded_reason = None;
    });
    app.save_store();

    // ---- 拉起：容器在 → 挂标记 + docker start；不在 → 卡片 enable.sh 首装 ----
    match &inspected {
        Some(_) => {
            // 先挂标记再拉起（语义 1）：拉起失败标记也在——崩溃 ≠ 手动关闭，
            // docker 与下次开机都会再试；用户不想要了走 disable 撤标记。
            ensure_autostart(app, task_id, card)?;
            app.docker
                .start(&name)
                .map_err(|e| format!("docker start 失败：{e}"))?;
            event(
                "start",
                &format!("容器 {name} 已拉起，等服务就绪（首次约 5 分钟：加载权重 + CUDA graph）"),
            );
        }
        None => {
            let script = app.config.cards_dir.join(&card.name).join("enable.sh");
            if !script.is_file() {
                return Err(format!(
                    "容器 {name} 不存在，卡片也没有 enable.sh 可首装（请用品悟商店完成首次启用）"
                ));
            }
            event(
                "first_install",
                &format!("容器 {name} 不存在，跑卡片 enable.sh 首装（建容器并等就绪，适配器在创建时撤独立重启）"),
            );
            let run = run_card_script(app, task_id, &script, "enable", &creation_environment);
            match run.outcome {
                RunOutcome::Ok => {}
                RunOutcome::Failed(m) => return Err(m),
                RunOutcome::Cancelled => return Err("已取消".to_string()),
            }
            ensure_autostart(app, task_id, card)?;
            // enable.sh 自己等到 /health；cmsd 仍统一再过一遍等就绪+登记（口径一致）
            // r9 P1-3：首装脚本（镜像拉取+依赖安装，硬上限 6h）的耗时不得计入
            // 就绪预算——以脚本结束时刻重置等待起点，慢网首装成功不再被误判
            // 「25 分钟内服务没有就绪」并连带 Degraded/不登记。
            started = now_epoch();
        }
    }
    wait_container_ready(app, task_id, card, &name, port, started, baseline_available)
}

/// 等容器服务就绪 + 就绪登记（口径同 native：host 端口 /v1/models、25 分钟上限、
/// 心跳带容器日志末行、实测标定 = 就绪前后可用内存差）。
fn wait_container_ready(
    app: &Arc<App>,
    task_id: &str,
    card: &Card,
    name: &str,
    port: u16,
    started: u64,
    baseline_available_gib: f64,
) -> Result<String, String> {
    let event = |stage: &str, msg: &str| app.task_event(task_id, stage, msg);
    let deadline = started + app.config.ready_timeout_secs;
    let mut last_heartbeat = 0u64;
    // 就绪判定的那次成功探测直接带出给登记用（不再二次探测——多一次探测只是
    // 给「服务在但瞬时连接失败」开一扇误留空名的窗）
    let ready_health: crate::engine::Health = loop {
        let now = now_epoch();
        if now >= deadline {
            let logs = app.docker.logs_tail(name, 30).unwrap_or_default();
            return Err(format!(
                "25 分钟内服务没有就绪（容器保留，可 docker logs {name} 排查）。最后日志：\n{}",
                redact_text(&tail_lines(&logs, 30))
            ));
        }
        match app.docker.inspect(name) {
            Ok(Some(c)) if c.running => {
                crate::container_runtime::check(card, None, &c)?;
            }
            Ok(Some(c)) => {
                let logs = app.docker.logs_tail(name, 30).unwrap_or_default();
                return Err(format!(
                    "容器启动后退出（退出码 {}{}）。最后日志：\n{}",
                    c.exit_code,
                    if c.oom_killed {
                        "，驱动报 OOMKilled"
                    } else {
                        ""
                    },
                    redact_text(&tail_lines(&logs, 30))
                ));
            }
            Ok(None) => {
                return Err(format!(
                    "容器 {name} 在等待就绪期间消失了（可能被外部删除）"
                ))
            }
            Err(e) => return Err(format!("docker 不可用：{e}")),
        }
        let health = engine::check_health(port, None);
        if health.ok {
            break health;
        }
        if now >= last_heartbeat + 15 {
            last_heartbeat = now;
            let log_hint = app
                .docker
                .logs_tail(name, 1)
                .ok()
                .and_then(|t| t.lines().last().map(|l| l.trim().to_string()))
                .filter(|l| !l.is_empty())
                .map(|l| format!("（容器最近日志：{}）", clip(&redact_text(&l), 120)))
                .unwrap_or_default();
            event(
                "wait",
                &format!(
                    "等待服务就绪…已用时 {}s{log_hint}",
                    now.saturating_sub(started)
                ),
            );
        }
        std::thread::sleep(std::time::Duration::from_secs(5));
    };

    // 就绪结论落入健康缓存：/v1/models 读的是 supervisor 周期探测缓存，
    // 就绪后那几分钟里该端口还没有记录（真机实证：换胎 14:04 就绪，首个缓存
    // 记录 14:29 才出现），调用方（品悟换胎后自动接入）会拿到 healthy=null
    // 而静默放弃建行——三条就绪路径（native/container 幂等与首启）都 priming。
    let c = Some(prime_container_health(
        app,
        card,
        port,
        ready_health.clone(),
    )?);

    // ---- 就绪：登记状态、活跃指针、实测标定（与 native 同口径）----
    let after_available = capacity::read_meminfo(&app.config.meminfo_path)
        .map(|m| m.available_gib())
        .unwrap_or(0.0);
    let footprint = (baseline_available_gib - after_available).max(0.0);
    let pid = c.as_ref().map(|c| c.pid).filter(|p| *p > 0);
    let model_id = ready_health
        .model_id
        .or_else(|| c.as_ref().and_then(|c| c.served_model_name()))
        .unwrap_or_default();
    upsert_model_state(app, &card.name, |m| {
        m.status = ModelStatus::Running;
        m.port = port;
        m.main_pid = pid;
        if !model_id.is_empty() {
            m.model_id = model_id.clone();
        }
        m.last_ready_at = Some(now_epoch());
        m.enabled_at = m.enabled_at.or(Some(now_epoch()));
        m.measured_footprint_gib = Some((footprint * 10.0).round() / 10.0);
        m.degraded_reason = None;
    });
    set_active_if_free(app, &card.name);
    event(
        "ready",
        &format!("服务已就绪：http://127.0.0.1:{port}/v1（模型标识 {model_id}）"),
    );
    event(
        "calibrate",
        &format!("实测标定：常驻占用约 {footprint:.1} GiB（就绪前后系统可用内存差）"),
    );
    Ok(format!(
        "服务已就绪：http://127.0.0.1:{port}/v1（模型标识 {model_id}），用时 {}s",
        now_epoch().saturating_sub(started)
    ))
}

/// Remove independent Docker recovery; persisted cmsd intent is authoritative.
fn ensure_autostart(app: &Arc<App>, task_id: &str, card: &Card) -> Result<(), String> {
    let container = crate::container_runtime::inspect(app, card, None)?
        .ok_or("runtime_missing: 容器尚不存在")?;
    if container.restart_policy == "no" {
        return Ok(());
    }
    app.docker.set_restart_policy(&container.name, "no")?;
    app.task_event(
        task_id,
        "autostart",
        &format!(
            "自动恢复已交由cmsd统一预算（restart=no，原 {}）",
            container.restart_policy
        ),
    );
    Ok(())
}

/// 容器卡需求量口径链（W6 拍板）：
/// 1. 启动命令/卡片默认里的 --mem-fraction-static → 分数 × 内存总量（静态预留）；
/// 2. 实测标定（V2 就绪差值）；3. 权重 + 3.5 GiB 兜底。
///
/// 返回 (需求量 GiB, 口径标识, 中文口径说明)。
fn required_gib_container(
    card: &Card,
    container: Option<&crate::docker::ContainerInfo>,
    total_gib: f64,
    measured: Option<f64>,
) -> (f64, &'static str, String) {
    let floor = capacity::required(card.total_bytes, capacity::DEFAULT_OVERHEAD_GIB, measured);
    let fraction = container.and_then(|c| c.mem_fraction_static()).or_else(|| {
        card.docker_deploy
            .as_ref()
            .and_then(|d| d.mem_fraction_static)
    });
    if let Some(f) = fraction {
        if !f.is_finite() || f <= 0.0 || f > 1.0 {
            return (
                f64::INFINITY,
                "invalid",
                "mem-fraction-static必须大于0且不超过1".into(),
            );
        }
        let required = (f * total_gib).max(floor.0);
        return (
            required,
            "static",
            format!("静态预留 {required:.1}（--mem-fraction-static {f} × 总量 {total_gib:.1}，并保留权重/运行时下限）"),
        );
    }
    floor
}

/// 任意卡的需求量（switch 预检/释放量估算/capacity 视图/degraded 复核共用）：
/// 容器走容器口径链，原生走实测优先 + 权重+开销。
pub fn required_gib_for(
    app: &Arc<App>,
    card: &Card,
    total_gib: f64,
    measured: Option<f64>,
) -> (f64, &'static str, String) {
    match card.engine_kind {
        crate::cards::EngineKind::Container => {
            let inspected = card
                .container_name
                .as_deref()
                .and_then(|n| app.docker.inspect(n).ok().flatten());
            required_gib_container(card, inspected.as_ref(), total_gib, measured)
        }
        crate::cards::EngineKind::Native => {
            let measured = measured.filter(|_| {
                native_profile::resolve(app, card)
                    .is_ok_and(|profile| native_profile::recorded(app, card, &profile))
            });
            capacity::required_native(card.total_bytes, measured, card.native_reserved_gib)
        }
    }
}

/// Capacity display and actual native enable use exactly the same admission.
pub fn budget_for(
    app: &Arc<App>,
    card: &Card,
    mem: &capacity::MemInfo,
    measured: Option<f64>,
) -> capacity::Budget {
    let measured = if card.engine_kind == crate::cards::EngineKind::Native {
        measured.filter(|_| {
            native_profile::resolve(app, card)
                .is_ok_and(|profile| native_profile::recorded(app, card, &profile))
        })
    } else {
        measured
    };
    if card.engine_kind == crate::cards::EngineKind::Native && card.native_reserved_gib.is_none() {
        return capacity::evaluate(
            mem,
            card.total_bytes,
            capacity::DEFAULT_OVERHEAD_GIB,
            measured,
            app.config.debug_available_mib,
        );
    }
    let (required, basis, note) = required_gib_for(app, card, mem.total_gib(), measured);
    let mut budget = capacity::evaluate_with_required(
        mem,
        required,
        basis,
        &note,
        app.config.debug_available_mib,
    );
    if card.engine_kind == crate::cards::EngineKind::Native {
        budget.weights_gib = card.total_bytes as f64 / 1073741824.0;
        budget.overhead_gib = capacity::DEFAULT_OVERHEAD_GIB;
    }
    budget
}

/// disable 的容器面：docker stop → 撤自启标记（restart=no）。语义 2：手动关闭 = 不再拉起。
fn disable_container_inner(
    app: &Arc<App>,
    task_id: &str,
    card_name: &str,
) -> Result<String, String> {
    disarm_recovery(app, card_name)?;
    let name = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .models
            .get(card_name)
            .and_then(|m| m.container_name.clone())
            .or_else(|| {
                scan_cards(&app.config.cards_dir)
                    .into_iter()
                    .find(|c| c.name == card_name)
                    .and_then(|c| c.container_name)
            })
            .ok_or_else(|| format!("{card_name} 缺容器名（状态库与卡片都没有），无法停止"))?
    };
    let exists = app
        .docker
        .inspect(&name)
        .map_err(|e| format!("docker 不可用：{e}"))?;
    match exists {
        Some(c) => {
            let recorded = app
                .store
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .state
                .models
                .get(card_name)
                .cloned()
                .ok_or("state_unknown: 缺托管引用")?;
            crate::container_runtime::saved_reference(&recorded, &c)?;
            if c.running {
                app.task_event(
                    task_id,
                    "stop",
                    &format!("停止容器 {name}（120s 体面退出窗口）"),
                );
                app.docker.stop(&name)?;
            } else {
                app.task_event(task_id, "stop", &format!("容器 {name} 本来就没在跑"));
            }
            app.docker.set_restart_policy(&name, "no")?;
        }
        None => {
            app.task_event(
                task_id,
                "stop",
                &format!("容器 {name} 已不存在（可能已被外部删除）"),
            );
        }
    }
    upsert_model_state(app, card_name, |m| {
        m.status = ModelStatus::Stopped;
        m.main_pid = None;
    });
    {
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        if guard.state.active_model.as_deref() == Some(card_name) {
            guard.state.active_model = None;
            guard.mark_dirty();
        }
    }
    app.task_event(task_id, "stopped", "容器已停止，自启标记已撤（restart=no）");
    Ok("已停止".to_string())
}

/// 容器收编（W6a，D2 只读语义对齐驭光野生收编）：在跑/在册容器命中容器卡但
/// 状态库无记录 → 补登记。不动容器任何东西（restart 策略原样保留）。
pub fn register_container_managed(app: &Arc<App>, card: &Card, c: &crate::docker::ContainerInfo) {
    upsert_model_state(app, &card.name, |m| {
        m.lifecycle_owned = false;
        m.engine_type = "container".to_string();
        m.container_name = Some(c.name.clone());
        m.image = Some(c.image.clone());
        if let Some(p) = c.host_port {
            m.port = p;
        }
        if m.model_id.is_empty() {
            m.model_id = c.served_model_name().unwrap_or_default();
        }
        if c.running {
            m.status = ModelStatus::Running;
            if c.pid > 0 {
                m.main_pid = Some(c.pid);
            }
        }
    });
    log_line(&format!(
        "对账：容器 {}（卡 {}）补登记进模型状态（只读收编，restart 策略 {} 不动）",
        c.name, card.name, c.restart_policy
    ));
}

/// 引擎形态判定：状态库记录优先，卡片扫描兜底（收编前的卡状态库里还没有）。
pub fn engine_kind_of(app: &Arc<App>, card_name: &str) -> crate::cards::EngineKind {
    let recorded = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .models
            .get(card_name)
            .map(|m| m.engine_type.clone())
    };
    match recorded.as_deref() {
        Some("container") => return crate::cards::EngineKind::Container,
        Some("native") => return crate::cards::EngineKind::Native,
        _ => {}
    }
    scan_cards(&app.config.cards_dir)
        .into_iter()
        .find(|c| c.name == card_name)
        .map(|c| c.engine_kind)
        .unwrap_or(crate::cards::EngineKind::Native)
}

/// POST /v1/models/{card}/purge（T9：停服务→删权重→清状态→报释放量；引擎目录保留）
pub fn purge(app: &Arc<App>, card_name: &str) -> Result<String, ApiErr> {
    purge_with_maintenance(app, card_name, None)
}

pub fn purge_with_maintenance(
    app: &Arc<App>,
    card_name: &str,
    owner: Option<&str>,
) -> Result<String, ApiErr> {
    if let Some(owner) = owner {
        if let Some(task) = crate::model_maintenance::scoped_action(app, card_name, owner, "purge")?
        {
            return Ok(task);
        }
    }
    let selected = crate::store_adoption::selection::admit(app, card_name)?;
    let card_name = selected.card.as_str();
    let weights_dir = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let Some(m) = guard.state.models.get(card_name) else {
            return Err(api_err(
                404,
                "unknown_model",
                &format!("{card_name} 不在 cmsd 登记册里。红线：purge 只清 cmsd 登记管理的模型"),
            ));
        };
        require_lifecycle_owner(m)?;
        if m.engine_type == "container" {
            // 容器卡（2026-10-15 用户实测驱动）：purge 对容器是「注销」语义——
            // 停服务 + 撤出登记，**不碰**权重目录/镜像/容器实体（那些归品悟
            // 模型商店的卸载流程管；商店删除模型时会同步调本端点做传播）。
            None
        } else {
            let dir = m
                .card_weights_dir(app)
                .ok_or_else(|| api_err(500, "no_weights_path", "状态里缺权重路径"))?;
            Some(dir)
        }
    };
    if let Some(tid) = app.task_running_for(card_name) {
        return Err(api_err(
            409,
            "task_in_progress",
            &format!("已有进行中的任务 {tid}"),
        ));
    }
    let target = weights_dir
        .map(|path| purge_target::Target::capture(app, card_name, path))
        .transpose()
        .map_err(|message| api_err(409, "unsafe_purge_target", &message))?;
    let task_id = app.task_create_exclusive_with_maintenance("purge", card_name, owner)?;
    let app2 = Arc::clone(app);
    let tid = task_id.clone();
    let card = card_name.to_string();
    std::thread::spawn(move || {
        let _selected = selected;
        let outcome = match target {
            Some(target) => purge_inner(&app2, &tid, &card, target),
            None => purge_container_inner(&app2, &tid, &card),
        };
        match &outcome {
            Ok(msg) => log_line(&format!("purge {card} 成功：{msg}")),
            Err(msg) => log_line(&format!("purge {card} 失败：{msg}")),
        }
        app2.task_finish(&tid, outcome);
        app2.save_store();
    });
    Ok(task_id)
}

/// 容器卡的 purge（注销语义）：停服务（disable 含 restart=no 撤自启）+ 撤出
/// 登记册。权重目录/镜像/容器实体一律不动——归品悟模型商店的卸载流程管。
fn purge_container_inner(app: &Arc<App>, task_id: &str, card_name: &str) -> Result<String, String> {
    // Cached status is not a quiescence proof. Use the recorded Docker
    // reference even when the supervisor has not observed an external start.
    crate::store_adoption::selection::disarm_current_boot(app, card_name)?;
    disable_container_inner(app, task_id, card_name)?;
    crate::model_maintenance::require_runtime_quiescent(app, card_name)
        .map_err(|(_, code, message)| format!("{code}: {message}"))?;
    crate::store_adoption::runtime::retire_native_unit(app, card_name)?;
    app.task_event(
        task_id,
        "deregister",
        "撤出 cmsd 登记册（权重与容器实体不动，归模型商店清理）",
    );
    {
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard.state.models.remove(card_name);
        if guard.state.active_model.as_deref() == Some(card_name) {
            guard.state.active_model = None;
        }
        guard.mark_dirty();
    }
    app.task_event(
        task_id,
        "deregister",
        &format!("已从管理面注销：{card_name}。如需删除权重/容器实体，请在品悟模型商店执行卸载"),
    );
    Ok(format!(
        "已注销 {card_name}（停服务+撤登记；权重与容器实体未动，归模型商店卸载流程清理）"
    ))
}

fn purge_inner(
    app: &Arc<App>,
    task_id: &str,
    card_name: &str,
    target: purge_target::Target,
) -> Result<String, String> {
    target.verify(app, card_name)?;
    app.systemd.assert_template_owner()?;
    crate::store_adoption::selection::disarm_current_boot(app, card_name)?;
    // Neither a stopped cache nor a successful stop reply proves quiescence.
    // Stop the owned runtime, then use the same full proof as store uninstall
    // before removing any weight or registration.
    disable_inner(app, task_id, card_name)?;
    crate::model_maintenance::require_runtime_quiescent(app, card_name)
        .map_err(|(_, code, message)| format!("{code}: {message}"))?;
    // Recheck the exact path/object after stop and backend observation. A new
    // directory, altered alias or redeployment does not belong to this request.
    target.verify(app, card_name)?;
    let weights_dir = target.path();
    let freed = if target.is_link() {
        0
    } else {
        dir_size(weights_dir)
    };
    app.task_event(
        task_id,
        "delete",
        &format!(
            "删除权重目录 {}（{:.1} GiB）",
            weights_dir.display(),
            freed as f64 / 1073741824.0
        ),
    );
    // Size accounting and event persistence may take time; do not delete a
    // replacement that appeared after the earlier safe observation.
    target.verify(app, card_name)?;
    if target.exists() {
        std::fs::remove_dir_all(weights_dir)
            .map_err(|e| format!("删除 {} 失败：{e}", weights_dir.display()))?;
    }
    // Retire diagnostics while the stopped lifecycle record still exists; a
    // failed cleanup must remain explicitly recoverable rather than report success.
    crate::store_adoption::runtime::retire_native_unit(app, card_name)?;
    // 清状态：env 文件、自启登记、状态库条目、端口记录、活跃指针
    let env_path = app.config.env_dir().join(format!("{card_name}.env"));
    let _ = std::fs::remove_file(&env_path);
    let unit = app.systemd.unit_name_for(card_name);
    let _ = app.systemd.disable(&unit);
    let _ = app.systemd.reset_failed(&unit);
    let _ = app.systemd.daemon_reload();
    {
        let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard.state.models.remove(card_name);
        guard.state.ports.retain(|_, c| c != card_name);
        if guard.state.active_model.as_deref() == Some(card_name) {
            guard.state.active_model = None;
        }
        guard.mark_dirty();
    }
    let msg = format!(
        "已清除 {card_name}：权重释放 {:.1} GiB，引擎目录保留",
        freed as f64 / 1073741824.0
    );
    app.task_event(task_id, "done", &msg);
    Ok(msg)
}

/// 目录体积（字节，跟随文件大小，不跟随符号链接）。
fn dir_size(path: &Path) -> u64 {
    let mut total = 0;
    let mut stack = vec![path.to_path_buf()];
    while let Some(dir) = stack.pop() {
        if let Ok(entries) = std::fs::read_dir(&dir) {
            for entry in entries.flatten() {
                let Ok(meta) = entry.metadata() else {
                    continue;
                };
                if meta.is_dir() {
                    stack.push(entry.path());
                } else if meta.is_file() {
                    total += meta.len();
                }
            }
        }
    }
    total
}

/// 端口分配：优先卡片部署端口；被别的卡占或实际被监听才从池里发新的。
fn allocate_port(app: &Arc<App>, card_name: &str, preferred: u16) -> Result<u16, String> {
    let port_free = |port: u16| {
        std::net::TcpStream::connect_timeout(
            &format!("127.0.0.1:{port}").parse().unwrap_or_else(|_| {
                std::net::SocketAddr::new(std::net::IpAddr::V4(std::net::Ipv4Addr::LOCALHOST), 1)
            }),
            std::time::Duration::from_millis(300),
        )
        .is_err()
    };
    let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let owned_by_other = |port: u16| {
        guard
            .state
            .ports
            .get(&port)
            .map(|owner| owner != card_name)
            .unwrap_or(false)
    };
    if !owned_by_other(preferred) && port_free(preferred) {
        guard.state.ports.insert(preferred, card_name.to_string());
        guard.mark_dirty();
        return Ok(preferred);
    }
    for port in app.config.pool_start..=app.config.pool_end {
        if !owned_by_other(port) && port_free(port) {
            guard.state.ports.insert(port, card_name.to_string());
            guard.mark_dirty();
            return Ok(port);
        }
    }
    Err(format!(
        "原生端口池 {}-{} 分完了，请先 disable 不用的模型",
        app.config.pool_start, app.config.pool_end
    ))
}

fn current_port(app: &Arc<App>, card_name: &str) -> Option<u16> {
    let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    guard.state.models.get(card_name).map(|m| m.port)
}

fn set_active(app: &Arc<App>, card_name: &str) {
    let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    guard.state.active_model = Some(card_name.to_string());
    guard.mark_dirty();
}

/// 别名归属纪律（2026-10-13 用户拍板）：enable 只「拉起」——当前已有活跃模型时
/// 不挪默认；没有活跃模型才顺手设上。挪默认只有两条显式路径：switch（换胎）与
/// set-default（仅切换）。防止「多拉一个模型备用」劫持当前默认。
fn set_active_if_free(app: &Arc<App>, card_name: &str) {
    let has_active = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .active_model
            .as_ref()
            .and_then(|cur| guard.state.models.get(cur))
            .map(|m| m.status == ModelStatus::Running)
            .unwrap_or(false)
    };
    if !has_active {
        set_active(app, card_name);
    }
}

/// POST /v1/models/{card}/set-default：仅挪默认（目标必须在跑且健康，否则拒）。
/// 立即返回；留一条瞬结任务供监听方（品悟选择跟随）与台账看到这次切换。
pub fn set_default(app: &Arc<App>, card_name: &str) -> Result<String, ApiErr> {
    let _selected = crate::store_adoption::selection::admit(app, card_name)?;
    let card_name = _selected.card.as_str();
    let (running, port, model_id) = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        match guard.state.models.get(card_name) {
            Some(m) => {
                require_lifecycle_owner(m)?;
                (m.status == ModelStatus::Running, m.port, m.model_id.clone())
            }
            None => {
                return Err(api_err(
                    404,
                    "unknown_model",
                    &format!("{card_name} 不在 cmsd 登记册里"),
                ))
            }
        }
    };
    if !running {
        return Err(api_err(
            409,
            "not_running",
            &format!("{card_name} 没有在跑。先启动它（仅启动用 enable，换胎用 switch）"),
        ));
    }
    let card = find_card_raw(app, card_name)?;
    let runtime_matches = if card.engine_kind == crate::cards::EngineKind::Container {
        let model = app
            .store
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .state
            .models
            .get(card_name)
            .cloned();
        model.is_some_and(|model| crate::container_runtime::routable(app, &model))
    } else {
        let pid = app
            .store
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .state
            .models
            .get(card_name)
            .and_then(|m| m.main_pid)
            .unwrap_or(0);
        native_instance_matches(app, &card, port, pid)
    };
    if !runtime_matches || !engine::check_health(port, Some(&model_id)).ok {
        return Err(api_err(
            409,
            "not_healthy",
            &format!("{card_name} 在跑但不健康，等它就绪后再设默认"),
        ));
    }
    let task_id = app.task_create_exclusive("set_default", card_name)?;
    _selected.pin_task(app, &task_id)?;
    set_active(app, card_name);
    app.save_store();
    app.task_event(
        &task_id,
        "done",
        &format!("默认模型已切到 {card_name}（仅切换，未动任何进程）"),
    );
    app.task_finish(&task_id, Ok(format!("默认模型已切到 {card_name}")));
    Ok(task_id)
}

pub(crate) fn upsert_model_state(app: &Arc<App>, card_name: &str, f: impl FnOnce(&mut ModelState)) {
    let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let card = scan_cards(&app.config.cards_dir)
        .into_iter()
        .find(|c| c.name == card_name);
    let entry = guard
        .state
        .models
        .entry(card_name.to_string())
        .or_insert_with(|| ModelState {
            lifecycle_owned: false,
            card: card_name.to_string(),
            model_id: card
                .as_ref()
                .map(|c| c.model_id.clone())
                .unwrap_or_default(),
            engine_sha8: card
                .as_ref()
                .map(|c| c.engine_sha8.clone())
                .unwrap_or_default(),
            port: card
                .as_ref()
                .map(|c| match c.engine_kind {
                    crate::cards::EngineKind::Container => {
                        c.docker_deploy.as_ref().map(|d| d.port).unwrap_or(0)
                    }
                    crate::cards::EngineKind::Native => {
                        c.deploy.as_ref().map(|d| d.port).unwrap_or(0)
                    }
                })
                .unwrap_or(0),
            status: ModelStatus::Stopped,
            degraded_reason: None,
            restart_count: 0,
            last_restart_at: None,
            last_ready_at: None,
            enabled_at: None,
            weights_bytes: card.as_ref().map(|c| c.total_bytes).unwrap_or(0),
            measured_footprint_gib: None,
            engine_rss_gib: None,
            main_pid: None,
            engine_type: card
                .as_ref()
                .map(|c| c.engine_kind.as_str().to_string())
                .unwrap_or_else(|| "native".to_string()),
            container_name: card.as_ref().and_then(|c| c.container_name.clone()),
            image: card.as_ref().and_then(|c| c.image.clone()),
        });
    // 已有记录也按最新扫描同步引擎字段（W6 升级后补登记；扫描不到卡时不乱动）
    if let Some(c) = &card.filter(|_| !entry.lifecycle_owned) {
        entry.engine_type = c.engine_kind.as_str().to_string();
        if c.container_name.is_some() {
            entry.container_name = c.container_name.clone();
        }
        if c.image.is_some() {
            entry.image = c.image.clone();
        }
    }
    f(entry);
    guard.mark_dirty();
}

fn wait_unit_inactive(app: &Arc<App>, unit: &str, timeout_secs: u64) -> bool {
    let deadline = now_epoch() + timeout_secs;
    loop {
        let us = app.systemd.show(unit).unwrap_or_default();
        if us.not_found || us.active_state == "inactive" || us.active_state == "failed" {
            return true;
        }
        if now_epoch() >= deadline {
            return false;
        }
        std::thread::sleep(std::time::Duration::from_secs(1));
    }
}

fn degrade_reason_text(us: &crate::systemd::UnitState) -> String {
    if us.start_limit_hit() {
        "5 分钟内启动失败 3 次，systemd 已熔断，不再自动拉起。修复后用 enable 恢复".to_string()
    } else {
        format!("引擎启动失败（结果 {}）", us.result)
    }
}

fn tail_lines(text: &str, n: usize) -> String {
    let lines: Vec<&str> = text.lines().collect();
    let start = lines.len().saturating_sub(n);
    lines[start..].join("\n")
}

/// ModelState 扩展：从卡片部署信息拿权重目录（purge 用）。
impl ModelState {
    pub fn card_weights_dir(&self, app: &Arc<App>) -> Option<std::path::PathBuf> {
        scan_cards(&app.config.cards_dir)
            .into_iter()
            .find(|c| c.name == self.card)
            .and_then(|c| c.deploy.map(|d| PathBuf::from(d.model_dir)))
    }
}

/// serve 旗标解析顺序：state_dir/serve-flags/<card>.txt 覆盖 > 卡片 enable.sh
/// 提取 > 空（引擎默认，保守）。覆盖文件是管理员显式配置，一行一条命令的旗标串。
fn resolve_serve_flags(app: &App, card: &Card) -> String {
    let override_path = app
        .config
        .state_dir
        .join("serve-flags")
        .join(format!("{}.txt", card.name));
    if let Ok(text) = std::fs::read_to_string(&override_path) {
        let flags = text.trim().to_string();
        if !flags.is_empty() {
            return flags;
        }
    }
    card.serve_flags.clone().unwrap_or_default()
}

pub(crate) fn native_instance_matches(app: &App, card: &Card, port: u16, pid: u32) -> bool {
    native_profile::resolve(app, card)
        .is_ok_and(|profile| native_profile::matches(app, card, port, pid, &profile))
}

/// Deployment identity without an already-persisted adoption binding. The
/// caller independently verifies this candidate's unit/account provenance.
pub(crate) fn native_deployment_matches(app: &App, card: &Card, port: u16, pid: u32) -> bool {
    native_profile::resolve(app, card)
        .is_ok_and(|profile| native_profile::matches_deployment(app, card, port, pid, &profile))
}

/// 状态库丢失/重建场景：systemd 里存在 megacube-model@<card> 单元但状态库
/// 没有记录时，以现实为准补登记（对账器职责：状态库可从现实重建）。
pub fn register_external_managed(app: &Arc<App>, card_name: &str, us: &crate::systemd::UnitState) {
    upsert_model_state(app, card_name, |m| {
        m.lifecycle_owned = false;
        if us.is_active() {
            m.status = ModelStatus::Running;
        }
        if us.main_pid > 0 {
            m.main_pid = Some(us.main_pid);
        }
    });
    log_line(&format!(
        "对账：单元 megacube-model@{card_name} 在状态库无记录，已按现实补登记"
    ));
}

/// 野生进程登记（D2）：只读收编，不拥有生命周期。
/// 同 pid 但 /proc start_time 变了 = pid 复用（孤儿检测思想），按新进程轮转登记。
/// r18 #1：sighting 存活判定由 supervisor 的 live_pids 承担（retain 对活进程
/// 不依赖 last_seen），持久化时间戳只需在接近保留窗口时刷新——逐 tick 落盘
/// 是纯写放大（每 sighting 43,200 saves/日）。
const EXTERNAL_LAST_SEEN_REFRESH_SECS: u64 = 600;

pub fn register_external(app: &Arc<App>, proc: &engine::ProcInfo) {
    let key = ExternalSighting::key(&proc.sha8, proc.pid);
    let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let now = now_epoch();
    let reused = guard
        .state
        .externals
        .get(&key)
        .map(|s| s.start_time != proc.start_time)
        .unwrap_or(false);
    if reused {
        guard.state.externals.remove(&key);
    }
    let existed = guard.state.externals.contains_key(&key);
    let entry = guard
        .state
        .externals
        .entry(key)
        .or_insert_with(|| ExternalSighting {
            sha8: proc.sha8.clone(),
            pid: proc.pid,
            start_time: proc.start_time,
            port: proc.port,
            model_id: proc.model_id.clone(),
            first_seen_at: now,
            last_seen_at: now,
        });
    // r18 #1：仅当持久化字段真实变化（新 sighting/start_time 轮换/port 或
    // model_id 补全）或时间戳接近保留窗口时才置脏。
    let mut changed = !existed;
    if now.saturating_sub(entry.last_seen_at) >= EXTERNAL_LAST_SEEN_REFRESH_SECS {
        entry.last_seen_at = now;
        changed = true;
    }
    if entry.port.is_none() && proc.port.is_some() {
        entry.port = proc.port;
        changed = true;
    }
    if entry.model_id.is_none() && proc.model_id.is_some() {
        entry.model_id = proc.model_id.clone();
        changed = true;
    }
    if changed {
        guard.mark_dirty();
    }
}

// ---------------------------------------------------------------------------
// 串行换胎（switch）：设计 §3.2——任何时刻只有一个模型常驻；事务在 cmsd 内
// （停旧 → 启新 → 失败自动回滚），客户端/MCP 中途断开不影响设备落态。
// ---------------------------------------------------------------------------

/// POST /v1/switch 的实现。幂等：目标已是活跃且健康 → 直接成功。
pub fn switch_to(app: &Arc<App>, target_card: &str) -> Result<String, ApiErr> {
    let selected = crate::store_adoption::selection::admit(app, target_card)?;
    let target_card = selected.card.as_str();
    let card = find_card(app, target_card)?;
    // W6：跨引擎——部署判据按引擎形态（原生卡根 .deploy-env / 容器 vendor .deploy-env）
    let deployed = match card.engine_kind {
        crate::cards::EngineKind::Native => card.deploy.is_some(),
        crate::cards::EngineKind::Container => card.docker_deploy.is_some(),
    };
    if !deployed {
        return Err(api_err(
            409,
            "not_deployed",
            &format!(
                "卡片 {target_card} 还没有部署过（缺 .deploy-env），请先在品悟商店完成「部署」"
            ),
        ));
    }
    if let Some(tid) = app.task_running_for(target_card) {
        return Err(api_err(
            409,
            "task_in_progress",
            &format!("目标卡已有进行中的任务 {tid}"),
        ));
    }
    let current = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard.state.active_model.clone()
    };
    if let Some(cur) = current.as_deref() {
        if cur == target_card {
            // 幂等：目标已在跑且健康 → 直接成功
            let healthy = {
                let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
                guard
                    .state
                    .models
                    .get(cur)
                    .map(|m| m.status == ModelStatus::Running && m.port > 0)
                    .unwrap_or(false)
            };
            if healthy {
                let port = {
                    let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
                    guard.state.models.get(cur).map(|m| m.port).unwrap_or(0)
                };
                let runtime_matches = if card.engine_kind == crate::cards::EngineKind::Container {
                    let model = app
                        .store
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .state
                        .models
                        .get(cur)
                        .cloned();
                    model.is_some_and(|model| crate::container_runtime::routable(app, &model))
                } else {
                    let pid = app
                        .store
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .state
                        .models
                        .get(cur)
                        .and_then(|m| m.main_pid)
                        .unwrap_or(0);
                    native_instance_matches(app, &card, port, pid)
                };
                if runtime_matches && engine::check_health(port, None).ok {
                    let task_id = app.task_create_switch(target_card, current.as_deref())?;
                    selected.pin_task(app, &task_id)?;
                    app.task_event(&task_id, "idempotent", "目标模型已是活跃且健康，无需切换");
                    app.task_finish(&task_id, Ok(format!("{target_card} 本来就在跑")));
                    return Ok(task_id);
                }
            }
        }
        if let Some(tid) = app.task_running_for(cur) {
            return Err(api_err(
                409,
                "task_in_progress",
                &format!("当前活跃模型 {cur} 有进行中的任务 {tid}，等它结束再切换"),
            ));
        }
    }

    // 预算预检（不碰任何东西）：可用 + 将释放的旧模型常驻 ≥ 目标需求 + 安全边际。
    // 需求量按目标引擎形态取口径（容器：静态预留 > 实测 > 权重+开销）。
    // 真正的 enable 预算在停旧之后还会再核一次（双保险），这里是避免「先停了旧的
    // 才发现新的装不下」的第一道防线。
    let mem = capacity::read_meminfo(&app.config.meminfo_path)
        .map_err(|e| api_err(500, "meminfo_unreadable", &format!("读内存信息失败：{e}")))?;
    let target_measured = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .models
            .get(target_card)
            .and_then(|m| m.measured_footprint_gib)
    };
    let (required, _basis, note) = required_gib_for(app, &card, mem.total_gib(), target_measured);
    let freed_gib = current.as_deref().and_then(|cur| {
        let measured = {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard
                .state
                .models
                .get(cur)
                .and_then(|m| m.measured_footprint_gib)
        };
        if measured.is_some() {
            return measured;
        }
        find_card(app, cur)
            .ok()
            .map(|old| required_gib_for(app, &old, mem.total_gib(), None).0)
    });
    let budget = capacity::evaluate_switch(
        &mem,
        required,
        &note,
        freed_gib,
        app.config.debug_available_mib,
    );
    if !budget.ok {
        return Err(api_err(409, "insufficient_memory", &budget.verdict));
    }

    let task_id = app.task_create_switch(target_card, current.as_deref())?;
    selected.pin_task(app, &task_id)?;
    let app2 = Arc::clone(app);
    let tid = task_id.clone();
    let target = target_card.to_string();
    let previous = current.clone();
    std::thread::spawn(move || {
        let outcome = (|| {
            selected.verify(&app2)?;
            selected.retire_related(&app2, &tid)?;
            let message = switch_inner(&app2, &tid, &target, previous.as_deref())?;
            selected.inherit_boot(&app2)?;
            Ok(message)
        })();
        match &outcome {
            Ok(msg) => log_line(&format!("switch → {target} 成功：{msg}")),
            Err(msg) => log_line(&format!("switch → {target} 失败：{msg}")),
        }
        app2.task_finish(&tid, outcome);
        app2.save_store();
    });
    Ok(task_id)
}

/// 停模型（跨引擎分发）：停法与撤标记都按引擎形态（disable 的语义 2 实现）。
fn stop_model_inner(app: &Arc<App>, task_id: &str, card_name: &str) -> Result<(), String> {
    match engine_kind_of(app, card_name) {
        crate::cards::EngineKind::Native => disable_inner(app, task_id, card_name).map(|_| ()),
        crate::cards::EngineKind::Container => {
            disable_container_inner(app, task_id, card_name).map(|_| ())
        }
    }
}

pub(crate) fn stop_archived(app: &Arc<App>, task_id: &str, physical: &str) -> Result<(), String> {
    stop_model_inner(app, task_id, physical)
}

/// 启模型（跨引擎分发）：拉起与挂标记都按引擎形态（enable 的语义 1 实现）。
fn start_model_inner(app: &Arc<App>, task_id: &str, card: &Card) -> Result<String, String> {
    match card.engine_kind {
        crate::cards::EngineKind::Native => {
            let deploy = card
                .deploy
                .clone()
                .ok_or_else(|| format!("卡片 {} 未部署", card.name))?;
            enable_inner(app, task_id, card, &deploy)
        }
        crate::cards::EngineKind::Container => enable_container_inner(app, task_id, card),
    }
}

/// 撤自启标记（回滚专用，尽力而为不留事件）：没起来/被换下的模型不该占着开机拉起位。
fn clear_autostart_quietly(app: &Arc<App>, card: &Card) {
    // A rejected start may never have acquired this runtime. Cleanup cannot
    // turn that rejection into authority over a colliding foreign deployment.
    let owned = app
        .store
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .state
        .models
        .get(&card.name)
        .filter(|model| model.lifecycle_owned && model.engine_type == card.engine_kind.as_str())
        .cloned();
    let Some(owned) = owned else {
        return;
    };
    let safe = match card.engine_kind {
        crate::cards::EngineKind::Native => native_profile::resolve(app, card)
            .is_ok_and(|profile| verify_native_runtime(app, card, &profile).is_ok()),
        crate::cards::EngineKind::Container => card.docker_deploy.as_ref().is_some_and(|deploy| {
            owned.container_name.as_deref() == Some(deploy.container_name.as_str())
                && owned.image.as_deref() == Some(deploy.image.as_str())
                && crate::container_runtime::inspect(app, card, Some(&owned)).is_ok()
        }),
    };
    if !safe {
        return;
    }
    if let Err(error) = crate::store_adoption::selection::disarm_current_boot(app, &card.name) {
        log_line(&format!("保存停用后的逻辑自启偏好失败：{error}"));
    }
    if card.engine_kind == crate::cards::EngineKind::Native
        && app.systemd.assert_template_owner().is_err()
    {
        return;
    }
    if disarm_recovery(app, &card.name).is_err() {
        return;
    }
    match card.engine_kind {
        crate::cards::EngineKind::Container => {
            if let Some(name) = &card.container_name {
                let _ = app.docker.set_restart_policy(name, "no");
            }
        }
        crate::cards::EngineKind::Native => {
            let _ = app.systemd.disable(&app.systemd.unit_name_for(&card.name));
        }
    }
}

fn switch_inner(
    app: &Arc<App>,
    task_id: &str,
    target_card: &str,
    previous: Option<&str>,
) -> Result<String, String> {
    let started = now_epoch();
    let event = |stage: &str, msg: &str| app.task_event(task_id, stage, msg);
    let target = find_card(app, target_card).map_err(|e| e.2)?;
    let previous_boot = previous
        .map(|card| {
            crate::recovery::view(app, card)
                .map(|record| record.map(|record| crate::recovery::boot_enabled(&record)))
        })
        .transpose()
        .map_err(|e| e.2)?
        .flatten();

    // 停旧（按其引擎类型走对应停法 + 撤旧标记——语义 3：标记迁移）
    if let Some(prev) = previous {
        if prev != target_card {
            event("plan", &format!("切换计划：停 {prev} → 启 {target_card}"));
            event("stop_old", &format!("停止当前模型 {prev}"));
            crate::store_adoption::selection::disarm_current_boot(app, prev)?;
            stop_model_inner(app, task_id, prev)?;
            event(
                "stop_old",
                &format!("{prev} 已停止（自启标记已随停用撤下）"),
            );
        }
    }

    // 启新（enable 自带预算复检——此刻旧模型内存已真实释放；挂新标记）
    event(
        "start_new",
        &format!("启动 {target_card}（权重加载分钟级）"),
    );
    match start_model_inner(app, task_id, &target) {
        Ok(msg) => {
            let elapsed = now_epoch().saturating_sub(started);
            let done = format!("已切换到 {target_card}（{msg}），全程 {elapsed}s");
            event("done", &done);
            Ok(done)
        }
        Err(e) => {
            // 失败自动回滚：先撤掉新模型的自启标记（它没起来，不该占开机拉起位），
            // 再尽力把旧的拉回来（启旧 = 恢复旧标记——回滚恢复各自自启标记）。
            event(
                "rollback",
                &format!("{target_card} 启动失败：{e}；撤新模型自启标记，尝试回滚到 {previous:?}"),
            );
            clear_autostart_quietly(app, &target);
            let rollback_note = match previous {
                Some(prev)
                    if prev != target_card
                        && crate::store_adoption::selection::ensure_current(app, prev).is_ok() =>
                {
                    let prev_card = find_card(app, prev).map_err(|e2| e2.2)?;
                    match start_model_inner(app, task_id, &prev_card) {
                        Ok(_) => {
                            if let Some(enabled) = previous_boot {
                                crate::store_adoption::selection::restore_current_boot(
                                    app, prev, enabled,
                                )?;
                            }
                            format!("已回滚：{prev} 恢复运行（自启标记已恢复）")
                        }
                        Err(e2) => format!("回滚也失败了：{e2}。两个模型都没起来，请人工处理"),
                    }
                }
                _ => "未恢复历史版本；请修复商店当前部署，或选择另一张当前可用模型卡".to_string(),
            };
            event("rollback", &rollback_note);
            Err(format!("切换到 {target_card} 失败：{e}。{rollback_note}"))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn steady_external_sighting_does_not_remark_the_store_dirty() {
        // r18 #1：supervisor 每 2s 对每个野生引擎调用 register_external；
        // 稳态重复 sighting（start_time/port/model_id 不变且时间戳未到刷新
        // 窗口）不得置脏——原为每 tick 全量落盘（43,200 saves/日）。
        let root = crate::docker::fixtures::tmpdir("external-sighting-steady");
        let app = crate::docker::fixtures::make_app(&root, 128, 100);
        let proc = crate::engine::ProcInfo {
            pid: 4242,
            sha8: "ab12cd34".into(),
            port: Some(18000),
            model_id: Some("Model-X".into()),
            start_time: 100,
        };
        register_external(&app, &proc);
        {
            let mut store = app.store.lock().unwrap();
            assert!(store.is_dirty());
            assert_eq!(store.state.externals.len(), 1);
            store.save_if_dirty().unwrap();
        }
        // 稳态复测：同一 sighting 连续到达不再置脏。
        register_external(&app, &proc);
        assert!(
            !app.store.lock().unwrap().is_dirty(),
            "稳态 sighting 不得再次置脏 store（r18 #1）"
        );
        // 时间戳到达刷新窗口（600s）后允许一次刷新落盘。
        {
            let mut store = app.store.lock().unwrap();
            let sighting = store.state.externals.values_mut().next().unwrap();
            sighting.last_seen_at = sighting
                .last_seen_at
                .saturating_sub(EXTERNAL_LAST_SEEN_REFRESH_SECS);
            store.save_if_dirty().unwrap();
        }
        register_external(&app, &proc);
        assert!(
            app.store.lock().unwrap().is_dirty(),
            "陈旧 last_seen 刷新应落盘"
        );
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn changed_deployment_never_reuses_old_container() {
        let deploy = crate::cards::DockerDeploy {
            image: "sglang:fixed".into(),
            container_name: "test".into(),
            port: 30000,
            model_dir: "/models".into(),
            mem_fraction_static: Some(0.7),
            vendor_dir: None,
            host_bind: Some("127.0.0.1".into()),
            runtime_flags: [("--context-length".into(), "262144".into())].into(),
        };
        let mut container = crate::docker::ContainerInfo {
            image: deploy.image.clone(),
            args: [
                "--context-length=65536",
                "--port=30000",
                "--mem-fraction-static=0.70",
            ]
            .map(String::from)
            .into(),
            ..Default::default()
        };
        assert!(validate_container_config(&deploy, &container)
            .unwrap_err()
            .contains("configuration_changed"));
        container.args[0] = "--context-length=262144".into();
        assert!(validate_container_config(&deploy, &container).is_ok());
        container.args[1] = "--port=30001".into();
        assert!(validate_container_config(&deploy, &container).is_err());
        container.args.remove(0);
        assert!(
            validate_container_config(&deploy, &container).is_err(),
            "missing declared flag is drift too"
        );
        container.args = ["--context-length=262144", "--port=30000"]
            .map(String::from)
            .into();
        assert!(
            validate_container_config(&deploy, &container).is_err(),
            "missing declared memory fraction cannot use engine defaults"
        );
        for value in ["invalid", "NaN", "inf", "0", "1.1", "0.8"] {
            container.args.truncate(2);
            container
                .args
                .push(format!("--mem-fraction-static={value}"));
            assert!(
                validate_container_config(&deploy, &container).is_err(),
                "{value}"
            );
        }
        container.args.truncate(2);
        container.args.push("--mem-fraction-static=0.70".into());
        assert!(validate_container_config(&deploy, &container).is_ok());
        for tail in [
            "--context-length 262144 --context-length=32768 --port=30000 --mem-fraction-static=0.7",
            "--context-length=262144 --port 30000 --port=30001 --mem-fraction-static=0.7",
            "--context-length=262144 --port=30000 --mem-fraction-static 0.7 --mem-fraction-static=0.9",
            "--context-length --port=30000 --mem-fraction-static=0.7",
            "--context-length= --port=30000 --mem-fraction-static=0.7",
            "--context-length=262144 --port --mem-fraction-static=0.7",
            "--context-length=262144 --port=invalid --mem-fraction-static=0.7",
            "--context-length=262144 --port=30000 --mem-fraction-static",
        ] {
            container.args = tail.split_whitespace().map(str::to_owned).collect();
            assert!(validate_container_config(&deploy, &container).is_err(), "{tail}");
        }
        container.args = "--context-length 262144 --port 30000 --mem-fraction-static 0.7 --stop-token A --stop-token B".split_whitespace().map(str::to_owned).collect();
        assert!(
            validate_container_config(&deploy, &container).is_ok(),
            "unrelated repeatable flags are not interpreted"
        );
    }
    use crate::docker::fixtures as fx;
    use crate::store::TaskStatus;

    /// 启用已存在的容器：挂标记（update）→ start → 就绪登记（语义 1 全链）。
    #[test]
    fn enable_container_existing_happy() {
        let root = fx::tmpdir("enable");
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
            "--mem-fraction-static,0.01,--served-model-name,Fixture-Model",
        );

        let tid = enable(&app, "fixture-c").unwrap();
        let t = fx::wait_terminal(&app, &tid, 30);
        assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        let result = t.result.unwrap();
        assert!(result.contains("已就绪"), "{result}");
        assert!(result.contains(&port.to_string()), "{result}");

        // Existing restart=no needs no update. Startup remains serialized and
        // cmsd owns the durable desired state.
        let calls = fx::docker_calls(&root);
        assert!(calls.iter().any(|c| c == "start fixture-c-ctr"));
        assert!(!calls.iter().any(|c| c.contains("--restart=unless-stopped")));
        assert_eq!(
            crate::recovery::view(&app, "fixture-c")
                .unwrap()
                .unwrap()
                .get("desired_running")
                .and_then(crate::json::Value::as_bool),
            Some(true)
        );

        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let m = guard.state.models.get("fixture-c").unwrap();
        assert_eq!(m.engine_type, "container");
        assert_eq!(m.container_name.as_deref(), Some("fixture-c-ctr"));
        assert_eq!(m.status, ModelStatus::Running);
        assert_eq!(m.port, port);
        assert_eq!(m.main_pid, Some(4321));
        assert_eq!(m.model_id, "Fixture-Model");
        assert!(m.measured_footprint_gib.is_some());
        assert_eq!(guard.state.active_model.as_deref(), Some("fixture-c"));
        drop(guard);
        // 自启标记落进「唯一事实源」（状态文件的 restart 策略）
        let line = fx::state_line(&root, "fixture-c-ctr").unwrap();
        assert!(line.contains("^no^"), "{line}");
        std::fs::remove_dir_all(&root).ok();
    }

    /// 幂等：容器在跑且健康 → 确认标记后直接成功，不发 start。
    #[test]
    fn enable_container_idempotent_running() {
        let root = fx::tmpdir("idem");
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

        let tid = enable(&app, "fixture-c").unwrap();
        let t = fx::wait_terminal(&app, &tid, 30);
        assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        assert!(t.result.unwrap().contains("已在运行"));
        assert!(t.events.iter().any(|e| e.stage == "idempotent"));
        let calls = fx::docker_calls(&root);
        assert!(!calls.iter().any(|c| c.starts_with("start ")), "{calls:?}");
        std::fs::remove_dir_all(&root).ok();
    }

    /// 预算拒绝：静态预留 0.60×128=76.8 GiB 装不进 20 GiB 可用（10-14 事故场景），
    /// 任务失败且不发 start。
    #[test]
    fn enable_container_budget_refused() {
        let root = fx::tmpdir("budget");
        let app = fx::make_app(&root, 128, 20);
        let port = fx::serve_models_json("Fixture-Model");
        fx::write_docker_card(
            &root.join("cards"),
            "fixture-c",
            "fixture-c-ctr",
            port,
            Some("0.60"),
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
            "--mem-fraction-static,0.60",
        );

        let tid = enable(&app, "fixture-c").unwrap();
        let t = fx::wait_terminal(&app, &tid, 30);
        assert_eq!(t.status, TaskStatus::Failed);
        let err = t.error.unwrap();
        assert!(err.contains("内存不够"), "{err}");
        assert!(err.contains("静态预留"), "{err}");
        let calls = fx::docker_calls(&root);
        assert!(!calls.iter().any(|c| c.starts_with("start ")), "{calls:?}");
        std::fs::remove_dir_all(&root).ok();
    }

    /// 预算口径链：无 --mem-fraction-static 时用实测标定（事件里见口径说明）。
    #[test]
    fn enable_container_measured_basis() {
        let root = fx::tmpdir("measured");
        let app = fx::make_app(&root, 128, 100);
        let port = fx::serve_models_json("Fixture-Model");
        fx::write_docker_card(
            &root.join("cards"),
            "fixture-c",
            "fixture-c-ctr",
            port,
            None,
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
            "sleep,inf",
        );
        // 预置实测标定 5.0 GiB
        upsert_model_state(&app, "fixture-c", |m| {
            m.measured_footprint_gib = Some(5.0);
        });

        let tid = enable(&app, "fixture-c").unwrap();
        let t = fx::wait_terminal(&app, &tid, 30);
        assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        assert!(
            t.events.iter().any(|e| e.message.contains("实测常驻 5.0")),
            "{:?}",
            t.events.iter().map(|e| &e.message).collect::<Vec<_>>()
        );
        std::fs::remove_dir_all(&root).ok();
    }

    /// 停用容器：stop → 撤标记（restart=no），状态落 Stopped（语义 2）。
    #[test]
    fn disable_container_happy() {
        let root = fx::tmpdir("disable");
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
            "--mem-fraction-static,0.01",
        );
        let tid = enable(&app, "fixture-c").unwrap();
        assert_eq!(
            fx::wait_terminal(&app, &tid, 30).status,
            TaskStatus::Succeeded
        );

        let tid = disable(&app, "fixture-c").unwrap();
        let t = fx::wait_terminal(&app, &tid, 30);
        assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        let calls = fx::docker_calls(&root);
        let pos_stop = calls
            .iter()
            .position(|c| c == "stop -t 120 fixture-c-ctr")
            .unwrap_or(usize::MAX);
        let pos_no = calls
            .iter()
            .position(|c| c == "update --restart=no fixture-c-ctr")
            .unwrap_or(usize::MAX);
        assert!(pos_stop < pos_no, "{calls:?}");
        let line = fx::state_line(&root, "fixture-c-ctr").unwrap();
        assert!(line.contains("^no^"), "{line}");
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let m = guard.state.models.get("fixture-c").unwrap();
        assert_eq!(m.status, ModelStatus::Stopped);
        assert!(guard.state.active_model.is_none());
        std::fs::remove_dir_all(&root).ok();
    }

    /// 跨引擎 switch（容器 A → 容器 B）：停旧撤标记 → 启新挂标记 → 挪活跃指针（语义 3）。
    #[test]
    fn switch_container_to_container() {
        let root = fx::tmpdir("switch");
        let app = fx::make_app(&root, 128, 100);
        let port_a = fx::serve_models_json("Model-A");
        let port_b = fx::serve_models_json("Model-B");
        fx::write_docker_card(&root.join("cards"), "card-a", "ctr-a", port_a, Some("0.01"));
        fx::write_docker_card(&root.join("cards"), "card-b", "ctr-b", port_b, Some("0.01"));
        fx::set_container(
            &root,
            "ctr-a",
            "fixture/img:1.0",
            true,
            "unless-stopped",
            0,
            false,
            4321,
            port_a,
            8000,
            "--served-model-name,Model-A,--mem-fraction-static,0.01",
        );
        fx::set_container(
            &root,
            "ctr-b",
            "fixture/img:1.0",
            false,
            "no",
            0,
            false,
            0,
            port_b,
            8000,
            "--served-model-name,Model-B,--mem-fraction-static,0.01",
        );
        // A 登记在跑且为活跃模型
        upsert_model_state(&app, "card-a", |m| {
            m.status = ModelStatus::Running;
            m.port = port_a;
        });
        {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard.state.active_model = Some("card-a".to_string());
            guard.mark_dirty();
        }

        let tid = switch_to(&app, "card-b").unwrap();
        let t = fx::wait_terminal(&app, &tid, 30);
        assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        // 停旧在启新之前；旧标记撤、新标记挂
        let calls = fx::docker_calls(&root);
        let pos_stop = calls
            .iter()
            .position(|c| c == "stop -t 120 ctr-a")
            .unwrap_or(usize::MAX);
        let pos_start = calls
            .iter()
            .position(|c| c == "start ctr-b")
            .unwrap_or(usize::MAX);
        assert!(pos_stop < pos_start, "{calls:?}");
        assert!(fx::state_line(&root, "ctr-a").unwrap().contains("^no^"));
        assert!(fx::state_line(&root, "ctr-b").unwrap().contains("^no^"));
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        assert_eq!(guard.state.active_model.as_deref(), Some("card-b"));
        assert_eq!(
            guard.state.models.get("card-a").unwrap().status,
            ModelStatus::Stopped
        );
        assert_eq!(
            guard.state.models.get("card-b").unwrap().status,
            ModelStatus::Running
        );
        std::fs::remove_dir_all(&root).ok();
    }

    /// purge 对容器卡是「注销」语义（2026-10-15 用户实测驱动：商店删掉模型
    /// 后 LLM 仍报已下载——商店记录与管理面登记各自独立，删除必须传播）：
    /// 停服务+撤登记，不动权重/容器实体（归商店卸载流程）。
    #[test]
    fn purge_container_deregisters_without_touching_weights() {
        let root = fx::tmpdir("purge-ctr");
        let app = fx::make_app(&root, 128, 100);
        fx::write_docker_card(
            &root.join("cards"),
            "fixture-c",
            "fixture-c-ctr",
            30001,
            None,
        );
        upsert_model_state(&app, "fixture-c", |m| {
            m.lifecycle_owned = true;
            m.engine_type = "container".to_string();
            m.container_name = Some("fixture-c-ctr".into());
            m.status = ModelStatus::Stopped;
        });
        {
            let mut guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard.state.active_model = Some("fixture-c".to_string());
        }
        let tid = purge(&app, "fixture-c").unwrap();
        let t = crate::docker::fixtures::wait_terminal(&app, &tid, 30);
        assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        assert!(t.result.unwrap().contains("已注销"));
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        assert!(
            !guard.state.models.contains_key("fixture-c"),
            "登记册必须撤出（LLM/商店才不会再把已删模型报成已下载）"
        );
        assert_eq!(
            guard.state.active_model, None,
            "活跃指针若指向被注销卡要清掉"
        );
        std::fs::remove_dir_all(&root).ok();
    }

    #[test]
    fn failed_enable_cannot_claim_or_migrate_an_incompatible_late_container() {
        let root = fx::tmpdir("late-container-drift");
        let app = fx::make_app(&root, 128, 100);
        fx::write_docker_card(
            &app.config.cards_dir,
            "fixture-c",
            "fixture-c-ctr",
            18081,
            None,
        );
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "different/image",
            true,
            "always",
            0,
            false,
            54321,
            18081,
            8000,
            "",
        );
        let task = enable(&app, "fixture-c").unwrap();
        assert_eq!(
            fx::wait_terminal(&app, &task, 10).status,
            TaskStatus::Failed
        );
        assert!(!app.store.lock().unwrap().state.models["fixture-c"].lifecycle_owned);
        crate::recovery::migrate(&app);
        assert!(!crate::recovery::managed(&app, "fixture-c"));
        assert!(fx::state_line(&root, "fixture-c-ctr")
            .unwrap()
            .contains("^always^"));
        assert!(!fx::docker_calls(&root)
            .iter()
            .any(|c| c.starts_with("update ") || c.starts_with("stop ")));
    }

    #[test]
    fn changed_card_cannot_orphan_the_previous_owned_container() {
        let root = fx::tmpdir("owned-reference-drift");
        let app = fx::make_app(&root, 128, 100);
        fx::write_docker_card(&app.config.cards_dir, "fixture-c", "new-ctr", 18081, None);
        upsert_model_state(&app, "fixture-c", |m| {
            m.lifecycle_owned = true;
            m.container_name = Some("previous-ctr".into());
            m.image = Some("previous/image".into());
        });
        let task = enable(&app, "fixture-c").unwrap();
        assert_eq!(
            fx::wait_terminal(&app, &task, 10).status,
            TaskStatus::Failed
        );
        assert_eq!(
            app.store.lock().unwrap().state.models["fixture-c"]
                .container_name
                .as_deref(),
            Some("previous-ctr")
        );
        assert!(fx::docker_calls(&root).is_empty());
    }
}

#[cfg(test)]
mod compose_tests {
    use super::*;
    use crate::docker::fixtures as fx;
    use crate::store::TaskStatus;

    const SHA: &str = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc";

    /// compose 一代卡的 enable 全链：容器 host 网络（无端口映射，端口来自 .env），
    /// 启动参数是 --flag=value 连写（mem-fraction 预算口径从 args 连写形解析）。
    #[test]
    fn enable_compose_card_happy() {
        let root = fx::tmpdir("compose-enable");
        let app = fx::make_app(&root, 128, 110);
        let port = fx::serve_models_json("Qwen3.8-27B-NVFP4");
        let vendor = root.join("vendor");
        fx::write_compose_card(
            &root.join("cards"),
            "mtp",
            "fixture-mtp",
            &vendor,
            &root.join("models"),
            port,
            SHA,
        );
        // deploy 已跑过：.env 在（PORT = 应答器端口）
        std::fs::create_dir_all(&vendor).unwrap();
        std::fs::write(
            vendor.join(".env"),
            format!(
                "IMAGE=fixture/sglang:v0.5.16\nCONTAINER_NAME=fixture-mtp\nMODEL_PATH={}\nCACHE_PATH=/x\nHOST_BIND=127.0.0.1\nPORT={port}\nMEM_FRACTION_STATIC=0.70\n", root.join("models").display()
            ),
        )
        .unwrap();
        // host 网络容器：PortBindings 空（host_port None），args 是 compose 渲染的连写形
        fx::set_container(
            &root,
            "fixture-mtp",
            "fixture/sglang:v0.5.16",
            false,
            "no",
            0,
            false,
            0,
            0,
            0,
            &format!("python3,-m,sglang.launch_server,--model-path=/models/target,--host=127.0.0.1,--port={port},--mem-fraction-static=0.70"),
        );

        let tid = enable(&app, "mtp").unwrap();
        let t = fx::wait_terminal(&app, &tid, 30);
        assert_eq!(t.status, TaskStatus::Succeeded, "error={:?}", t.error);
        // 预算口径：args 连写形解析出 0.70 → 静态预留 0.70×128 = 89.6 GiB
        assert!(
            t.events.iter().any(|e| e.message.contains("静态预留 89.6")
                && e.message.contains("--mem-fraction-static 0.7")),
            "{:?}",
            t.events.iter().map(|e| &e.message).collect::<Vec<_>>()
        );
        // Already fenced Docker policy; cmsd owns recovery and reuses host port.
        let calls = fx::docker_calls(&root);
        assert!(calls.iter().any(|c| c == "start fixture-mtp"));
        assert!(!calls.iter().any(|c| c.contains("--restart=unless-stopped")));
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let m = guard.state.models.get("mtp").unwrap();
        assert_eq!(m.engine_type, "container");
        assert_eq!(m.container_name.as_deref(), Some("fixture-mtp"));
        assert_eq!(m.status, ModelStatus::Running);
        assert_eq!(m.port, port, "host 网络卡的服务端口来自 .env");
        assert_eq!(m.model_id, "Qwen3.8-27B-NVFP4");
        std::fs::remove_dir_all(&root).ok();
    }
}

// ---------------------------------------------------------------------------
// 卡片脚本运行器（enable.sh 首装专用；W1/W7 install 直接执行编排 2026-10-14
// 退役为商店委托门面后，这是本机器的唯一消费者，从 install.rs 迁入）。
// 语义：setsid 包裹出独立进程组（取消时整组收）；stdout/stderr 逐行进事件
// （stderr 前缀标注）；idle 600s 无输出行判死（脚本心跳会重置），单阶段
// 6 小时绝对上限；取消兜底 CancelEscalation 每行/每秒自查补枪。
// ---------------------------------------------------------------------------

/// 单个脚本阶段的运行结果（失败文案已带末尾输出）。
pub enum RunOutcome {
    Ok,
    Failed(String),
    Cancelled,
}

pub struct ScriptRun {
    pub outcome: RunOutcome,
}

/// 无任何输出行判死（卡片脚本自带 15s 心跳，10 分钟无声 = 卡死）。
const SCRIPT_IDLE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(600);
/// 绝对上限（22GB 下载在百兆带宽约 30 分钟，6 小时已是慢速网络的兜底）。
const SCRIPT_HARD_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(6 * 3600);
/// 失败摘要取的末尾行数（stdout/stderr 合并流式留存）。
const SCRIPT_TAIL_LINES: usize = 12;

fn run_card_script(
    app: &Arc<App>,
    task_id: &str,
    script: &std::path::Path,
    stage: &str,
    extra_env: &[(&str, String)],
) -> ScriptRun {
    run_card_script_with_limits(
        app,
        task_id,
        script,
        stage,
        extra_env,
        SCRIPT_IDLE_TIMEOUT,
        SCRIPT_HARD_TIMEOUT,
    )
}

fn run_card_script_with_limits(
    app: &Arc<App>,
    task_id: &str,
    script: &std::path::Path,
    stage: &str,
    extra_env: &[(&str, String)],
    idle_timeout: std::time::Duration,
    hard_timeout: std::time::Duration,
) -> ScriptRun {
    use std::io::BufReader;
    use std::process::{Command, Stdio};
    use std::sync::mpsc;
    use std::time::{Duration, Instant};

    if app.cancel_requested(task_id) {
        return ScriptRun {
            outcome: RunOutcome::Cancelled,
        };
    }
    let card_dir = script
        .parent()
        .map(std::path::Path::to_path_buf)
        .unwrap_or_default();
    let mut cmd = Command::new("setsid");
    cmd.arg(script)
        .current_dir(&card_dir)
        // 品悟约定注入（config.rs 三个配置项 → 脚本环境）
        .env("LMM_MODELS_DIR", &app.config.models_dir)
        .env("LMM_STAGING_DIR", &app.config.staging_dir)
        .env("LMM_HF_ENDPOINT", &app.config.hf_endpoint)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    for (k, v) in extra_env {
        cmd.env(k, v);
    }
    let container = app
        .store
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .state
        .models
        .get(
            &script
                .parent()
                .unwrap_or_else(|| std::path::Path::new(""))
                .file_name()
                .unwrap_or_default()
                .to_string_lossy()
                .into_owned(),
        )
        .and_then(|m| m.container_name.clone());
    if let Some(name) = container {
        if let Err(message) = crate::docker_guard::prepare(&mut cmd, app, task_id, &name) {
            return ScriptRun {
                outcome: RunOutcome::Failed(message),
            };
        }
    }
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            return ScriptRun {
                outcome: RunOutcome::Failed(format!("启动脚本 {} 失败：{e}", script.display())),
            };
        }
    };
    // setsid 后子进程自成进程组组长：pid 即 pgid，取消时按组收尸
    let pgid = child.id();

    // stdout/stderr 各一个泵线程经同一通道汇过来（避免两管互堵）。
    let (tx, rx) = mpsc::sync_channel::<Result<(bool, String), String>>(128);
    if let Some(stdout) = child.stdout.take() {
        let tx2 = tx.clone();
        std::thread::spawn(move || {
            pump_script(BufReader::new(stdout), tx2, false);
        });
    }
    if let Some(stderr) = child.stderr.take() {
        let tx2 = tx.clone();
        std::thread::spawn(move || {
            pump_script(BufReader::new(stderr), tx2, true);
        });
    }
    drop(tx);

    let mut tail: Vec<String> = Vec::new();
    let mut forced_fail: Option<String> = None;
    let mut last_line_at = Instant::now();
    let started = Instant::now();
    let mut exited_at: Option<Instant> = None;
    let mut output_closed = false;
    // 取消兜底：cancel 端点的 TERM 可能与登记竞态错过，这里每行/每秒自查补枪
    let mut term_sent = false;
    let mut kill_sent = false;
    let mut term_at: Option<Instant> = None;
    loop {
        if started.elapsed() >= hard_timeout {
            forced_fail = Some("脚本超过6小时上限，已终止，请核查任务与容器实况".into());
            kill_group(pgid, "-KILL");
            break;
        }
        let received = if output_closed {
            // EOF is a transport fact, not proof of process exit. Keep the
            // cancellation/idle/absolute checks active without a busy loop.
            std::thread::sleep(Duration::from_millis(100));
            Err(mpsc::RecvTimeoutError::Timeout)
        } else {
            rx.recv_timeout(Duration::from_secs(1))
        };
        match received {
            Ok(Err(message)) => {
                forced_fail = Some(message);
                kill_group(pgid, "-KILL");
                break;
            }
            Ok(Ok((is_err, line))) => {
                // 取消自查（有行时走这里）
                if app.cancel_requested(task_id) {
                    if !term_sent {
                        kill_group(pgid, "-TERM");
                        term_sent = true;
                        term_at = Some(Instant::now());
                    } else if !kill_sent
                        && term_at
                            .map(|t| t.elapsed() >= Duration::from_secs(3))
                            .unwrap_or(false)
                    {
                        kill_group(pgid, "-KILL");
                        kill_sent = true;
                    }
                }
                last_line_at = Instant::now();
                tail.push(line.clone());
                if tail.len() > SCRIPT_TAIL_LINES {
                    tail.remove(0);
                }
                // r8 P1-2：脚本 stdout/stderr 可含凭据行（env 注入 LMM_HF_TOKEN），
                // 进事件流（持久 + SSE）前过脱敏，与等待路径同口径。
                let line = redact_text(&line);
                if is_err {
                    app.task_event(task_id, stage, &format!("stderr: {line}"));
                } else {
                    app.task_event(task_id, stage, &line);
                }
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {
                // 取消自查（无行时走这里）
                if app.cancel_requested(task_id) {
                    if !term_sent {
                        kill_group(pgid, "-TERM");
                        term_sent = true;
                        term_at = Some(Instant::now());
                    } else if !kill_sent
                        && term_at
                            .map(|t| t.elapsed() >= Duration::from_secs(3))
                            .unwrap_or(false)
                    {
                        kill_group(pgid, "-KILL");
                        kill_sent = true;
                    }
                }
                if let Some(t) = exited_at {
                    // 子进程已退但管道被孙进程占着：宽限 5s 收残行，之后不等了
                    if t.elapsed() > Duration::from_secs(5) {
                        kill_group(pgid, "-KILL");
                        break;
                    }
                    continue;
                }
                if matches!(child.try_wait(), Ok(Some(_))) {
                    if output_closed {
                        break;
                    }
                    exited_at = Some(Instant::now());
                    continue;
                }
                if last_line_at.elapsed() >= idle_timeout {
                    forced_fail = Some(
                        "脚本超过 10 分钟没有任何输出，判定卡死，已终止（脚本自带心跳，正常不会这么久无声）"
                            .to_string(),
                    );
                    kill_group(pgid, "-KILL");
                    break;
                }
                if started.elapsed() >= hard_timeout {
                    forced_fail =
                        Some("脚本超过 6 小时绝对上限，已终止。请检查网络后重试（已下载部分保留，会断点续传）".to_string());
                    kill_group(pgid, "-KILL");
                    break;
                }
            }
            Err(mpsc::RecvTimeoutError::Disconnected) => {
                output_closed = true;
                if matches!(child.try_wait(), Ok(Some(_))) {
                    break;
                }
            }
        }
    }
    let status = child.wait();

    // 取消优先：用户已明确意图，无论脚本退出码如何都落 cancelled
    if app.cancel_requested(task_id) {
        return ScriptRun {
            outcome: RunOutcome::Cancelled,
        };
    }
    if let Some(msg) = forced_fail {
        return ScriptRun {
            outcome: RunOutcome::Failed(msg),
        };
    }
    match status {
        Ok(s) if s.success() => ScriptRun {
            outcome: RunOutcome::Ok,
        },
        Ok(s) => {
            let code_note = s
                .code()
                .map(|c| format!("（退出码 {c}）"))
                .unwrap_or_default();
            ScriptRun {
                outcome: RunOutcome::Failed(format!(
                    "脚本失败{code_note}。最后输出：\n{}",
                    redact_text(&tail.join("\n"))
                )),
            }
        }
        Err(e) => ScriptRun {
            outcome: RunOutcome::Failed(format!("等待脚本 {} 退出失败：{e}", script.display())),
        },
    }
}

fn pump_script<R: std::io::Read>(
    reader: std::io::BufReader<R>,
    sender: std::sync::mpsc::SyncSender<Result<(bool, String), String>>,
    stderr: bool,
) {
    let mut records = script_output::Records::new(reader);
    loop {
        match records.next() {
            Ok(None) => break,
            Ok(Some(line)) => {
                if sender
                    .send(Ok((stderr, line.trim_end_matches(['\r', '\n']).into())))
                    .is_err()
                {
                    break;
                }
            }
            Err(error) => {
                let _ = sender.send(Err(format!("脚本输出读取失败或单行超过8KiB：{error}")));
                break;
            }
        }
    }
}

/// Only signal the isolated script process group.
fn kill_group(pgid: u32, sig: &str) -> bool {
    let signal = match sig {
        "-TERM" => libc::SIGTERM,
        "-KILL" => libc::SIGKILL,
        _ => return false,
    };
    pgid > 1 && unsafe { libc::kill(-(pgid as i32), signal) == 0 }
}

/// 截断到 n 个字符（按 char 走，不切半个汉字）。
fn clip(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        s.to_string()
    } else {
        format!("{}…", s.chars().take(n).collect::<String>())
    }
}

#[cfg(test)]
mod script_boundary_tests {
    use super::*;
    use std::io::{BufReader, Cursor};
    use std::os::unix::fs::PermissionsExt;
    use std::sync::mpsc;
    use std::time::{Duration, Instant};

    #[test]
    fn carriage_return_progress_is_bounded_per_record_instead_of_whole_stream() {
        let (tx, rx) = mpsc::sync_channel(128);
        let text = (0..20)
            .map(|i| format!("progress {i}: {}\r", "x".repeat(500)))
            .collect::<String>();
        assert!(text.len() > 8192);
        pump_script(BufReader::new(Cursor::new(text)), tx, true);
        let records = rx.into_iter().collect::<Vec<_>>();
        assert_eq!(records.len(), 20);
        assert!(records.iter().all(|r| r
            .as_ref()
            .is_ok_and(|(err, line)| *err && line.starts_with("progress "))));
    }

    #[test]
    fn closing_both_pipes_does_not_bypass_cancellation_or_enter_blocking_wait() {
        let root = crate::docker::fixtures::tmpdir("script-closed-pipes");
        let app = crate::docker::fixtures::make_app(&root, 128, 100);
        let task = app.task_create("enable", "qa-script");
        let script = root.join("closed.sh");
        std::fs::write(
            &script,
            "#!/bin/sh\nprintf '%s' $$ > own-pgid\nexec 1>&- 2>&-\nsleep 3\n",
        )
        .unwrap();
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o700)).unwrap();
        let app2 = app.clone();
        let task2 = task.clone();
        let worker =
            std::thread::spawn(move || run_card_script(&app2, &task2, &script, "test", &[]));
        let deadline = Instant::now() + Duration::from_secs(2);
        while !root.join("own-pgid").exists() {
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(5));
        }
        // Let both real pipe readers observe EOF before marking cancellation.
        std::thread::sleep(Duration::from_millis(100));
        let started = Instant::now();
        app.cancel_mark(&task);
        let outcome = worker.join().unwrap().outcome;
        let elapsed = started.elapsed();
        std::fs::remove_dir_all(root).unwrap();
        assert!(matches!(outcome, RunOutcome::Cancelled));
        assert!(
            elapsed < Duration::from_secs(2),
            "closed pipe cancellation took {elapsed:?}"
        );
    }

    #[test]
    fn closed_output_still_obeys_an_absolute_script_budget() {
        let root = crate::docker::fixtures::tmpdir("script-closed-budget");
        let app = crate::docker::fixtures::make_app(&root, 128, 100);
        let task = app.task_create("enable", "qa-script");
        let script = root.join("closed.sh");
        std::fs::write(&script, "#!/bin/sh\nexec 1>&- 2>&-\nsleep 30\n").unwrap();
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o700)).unwrap();
        let started = Instant::now();
        let result = run_card_script_with_limits(
            &app,
            &task,
            &script,
            "test",
            &[],
            Duration::from_secs(10),
            Duration::from_millis(200),
        );
        assert!(matches!(result.outcome, RunOutcome::Failed(_)));
        assert!(started.elapsed() < Duration::from_secs(2));
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn exited_root_does_not_leave_its_pipe_holding_descendant_alive() {
        let root = crate::docker::fixtures::tmpdir("script-inherited-pipes");
        let app = crate::docker::fixtures::make_app(&root, 128, 100);
        let task = app.task_create("enable", "qa-script");
        let script = root.join("inherited.sh");
        std::fs::write(
            &script,
            "#!/bin/sh\nsleep 30 &\nprintf '%s' $! > own-child\nexit 0\n",
        )
        .unwrap();
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o700)).unwrap();
        let started = Instant::now();
        let result = run_card_script(&app, &task, &script, "test", &[]);
        assert!(matches!(result.outcome, RunOutcome::Ok));
        let pid: u32 = std::fs::read_to_string(root.join("own-child"))
            .unwrap()
            .parse()
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(1);
        loop {
            let state = std::fs::read_to_string(format!("/proc/{pid}/status"));
            if state.as_ref().ok().is_none_or(|text| {
                text.lines()
                    .any(|line| line.starts_with("State:") && line.contains('Z'))
            }) {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "pipe-holding descendant still alive"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        assert!(started.elapsed() < Duration::from_secs(9));
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[cfg(test)]
mod container_purge_tests {
    use super::*;
    use crate::docker::fixtures as fx;
    use crate::json::Value;
    use crate::store::TaskStatus;
    use std::os::unix::fs::PermissionsExt;

    fn fixture(tag: &str, cached: ModelStatus) -> (PathBuf, Arc<App>, PathBuf) {
        let root = fx::tmpdir(tag);
        let app = fx::make_app(&root, 128, 100);
        let card = fx::write_docker_card(
            &app.config.cards_dir,
            "fixture-c",
            "fixture-c-ctr",
            30001,
            None,
        );
        let weights = root.join("weights");
        std::fs::create_dir(&weights).unwrap();
        std::fs::write(weights.join("model.safetensors"), b"preserved").unwrap();
        let deploy = card.join("vendor/.deploy-env");
        std::fs::write(
            &deploy,
            std::fs::read_to_string(&deploy)
                .unwrap()
                .replace("/tmp/none", weights.to_str().unwrap()),
        )
        .unwrap();
        fx::set_container(
            &root,
            "fixture-c-ctr",
            "fixture/img:1.0",
            true,
            "always",
            0,
            false,
            4321,
            30001,
            8000,
            "--served-model-name,Fixture-Model,--mem-fraction-static,0.01",
        );
        upsert_model_state(&app, "fixture-c", |m| {
            m.lifecycle_owned = true;
            m.engine_type = "container".into();
            m.container_name = Some("fixture-c-ctr".into());
            m.status = cached;
            m.main_pid = (cached == ModelStatus::Running).then_some(4321);
        });
        let dir = app.config.state_dir.join("recovery");
        std::fs::create_dir_all(&dir).unwrap();
        let record = serde_json::json!({"card":"fixture-c","desired_running":true,"boot_enabled":true,"phase":"ready","attempts":[]});
        std::fs::write(dir.join("fixture-c.json"), record.to_string()).unwrap();
        (root, app, weights)
    }

    #[test]
    fn container_purge_stops_fresh_runtime_even_if_cached_stopped_and_preserves_files() {
        for cached in [ModelStatus::Running, ModelStatus::Stopped] {
            let (root, app, weights) = fixture("r04-purge-running", cached);
            let tid = purge(&app, "fixture-c").unwrap();
            let task = fx::wait_terminal(&app, &tid, 30);
            assert_eq!(task.status, TaskStatus::Succeeded, "{:?}", task.error);
            let actual = app.docker.inspect("fixture-c-ctr").unwrap().unwrap();
            assert!(!actual.running);
            assert_eq!(actual.pid, 0);
            assert_eq!(actual.restart_policy, "no");
            assert!(!app
                .store
                .lock()
                .unwrap()
                .state
                .models
                .contains_key("fixture-c"));
            let policy = crate::recovery::view(&app, "fixture-c").unwrap().unwrap();
            assert!(!crate::recovery::boot_enabled(&policy));
            assert_eq!(policy.get("desired_running"), Some(&Value::Bool(false)));
            assert_eq!(
                std::fs::read(weights.join("model.safetensors")).unwrap(),
                b"preserved"
            );
            assert!(!fx::docker_calls(&root).iter().any(|c| c.starts_with("rm ")));
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn container_purge_keeps_ownership_when_stop_fails_or_claims_success_without_quiescence() {
        for stop_result in [0, 7] {
            let (root, mut app, weights) = fixture("r04-purge-refused", ModelStatus::Running);
            let wrapper = root.join("docker-stop-override");
            std::fs::write(&wrapper, format!("#!/bin/sh\nif [ \"$1\" = stop ]; then exit {stop_result}; fi\nexec '{}' \"$@\"\n", root.join("docker").display())).unwrap();
            std::fs::set_permissions(&wrapper, std::fs::Permissions::from_mode(0o700)).unwrap();
            Arc::get_mut(&mut app).unwrap().docker =
                crate::docker::Docker::new(wrapper.to_str().unwrap(), "nvidia-smi");
            let tid = purge(&app, "fixture-c").unwrap();
            let task = fx::wait_terminal(&app, &tid, 30);
            assert_eq!(task.status, TaskStatus::Failed);
            assert!(app.store.lock().unwrap().state.models["fixture-c"].lifecycle_owned);
            assert!(
                app.docker
                    .inspect("fixture-c-ctr")
                    .unwrap()
                    .unwrap()
                    .running
            );
            assert_eq!(
                std::fs::read(weights.join("model.safetensors")).unwrap(),
                b"preserved"
            );
            assert!(!crate::recovery::boot_enabled(
                &crate::recovery::view(&app, "fixture-c").unwrap().unwrap()
            ));
            assert!(!fx::docker_calls(&root).iter().any(|c| c.starts_with("rm ")));
            std::fs::remove_dir_all(root).unwrap();
        }
    }
}

#[cfg(test)]
mod switch_rejection_tests {
    use super::*;
    use crate::docker::fixtures as fx;
    #[test]
    fn owned_card_reference_drift_preserves_the_new_foreign_container() {
        let root = fx::tmpdir("switch-owned-reference-drift");
        let app = fx::make_app(&root, 128, 100);
        fx::write_docker_card(
            &app.config.cards_dir,
            "current",
            "foreign-new-container",
            18199,
            None,
        );
        upsert_model_state(&app, "current", |model| {
            model.lifecycle_owned = true;
            model.container_name = Some("previous-owned-container".into());
            model.image = Some("fixture/img:1.0".into());
        });
        fx::set_container(
            &root,
            "foreign-new-container",
            "foreign/image",
            true,
            "always",
            0,
            false,
            412,
            18199,
            8000,
            "",
        );
        let task = app.task_create("switch", "current");
        assert!(switch_inner(&app, &task, "current", None)
            .unwrap_err()
            .contains("configuration_changed"));
        assert!(fx::state_line(&root, "foreign-new-container")
            .unwrap()
            .contains("^always^"));
        assert!(!fx::docker_calls(&root)
            .iter()
            .any(|call| call.starts_with("update ")));
        assert!(!crate::recovery::managed(&app, "current"));
    }
    #[test]
    fn rejected_switch_preserves_unowned_container_restart_policy() {
        let root = fx::tmpdir("switch-foreign-cleanup");
        let app = fx::make_app(&root, 128, 100);
        fx::write_docker_card(
            &app.config.cards_dir,
            "current",
            "foreign-container",
            18199,
            None,
        );
        fx::set_container(
            &root,
            "foreign-container",
            "foreign/image",
            true,
            "always",
            0,
            false,
            412,
            18199,
            8000,
            "",
        );
        let task = app.task_create("switch", "current");
        let result = switch_inner(&app, &task, "current", None);
        assert!(result.unwrap_err().contains("configuration_changed"));
        assert!(!app
            .store
            .lock()
            .unwrap()
            .state
            .models
            .get("current")
            .is_some_and(|model| model.lifecycle_owned));
        let state = fx::state_line(&root, "foreign-container").unwrap();
        assert!(state.contains("^always^"), "{state}");
        let calls = fx::docker_calls(&root);
        assert!(
            !calls.iter().any(|call| call.starts_with("update ")),
            "{calls:?}"
        );
    }
}
