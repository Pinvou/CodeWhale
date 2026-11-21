//! Explicitly owned model recovery goes through the same serialized lifecycle
//! admission and memory budget as an interactive enable. Discovery never arms it.
use crate::app::{api_err, log_line, ApiErr, App};
use crate::json::Value;
use crate::store::{ModelState, ModelStatus};
use std::io::Write;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::PathBuf;
use std::sync::Arc;

fn path(app: &App, card: &str) -> Result<PathBuf, ApiErr> {
    if card.is_empty()
        || card.len() > 128
        || card == "."
        || card == ".."
        || !card
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_.".contains(&b))
    {
        return Err(api_err(400, "invalid_card", "模型标识不合法"));
    }
    Ok(app
        .config
        .state_dir
        .join("recovery")
        .join(format!("{card}.json")))
}
pub fn managed(app: &App, card: &str) -> bool {
    path(app, card).is_ok_and(|p| !matches!(std::fs::symlink_metadata(p), Err(e) if e.kind() == std::io::ErrorKind::NotFound))
}

pub fn owns(app: &App, card: &str) -> bool {
    let store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    store
        .state
        .models
        .get(card)
        .is_some_and(|m| m.lifecycle_owned)
        && managed(app, card)
}
fn read(app: &App, card: &str) -> Result<Option<Value>, ApiErr> {
    let path = path(app, card)?;
    let Some(bytes) = crate::private_storage::read_control(&path, 16 * 1024)
        .map_err(|message| api_err(409, "recovery_unreadable", &message))?
    else {
        return Ok(None);
    };
    let text = std::str::from_utf8(&bytes)
        .map_err(|_| api_err(409, "recovery_corrupt", "自动恢复记录UTF-8损坏"))?;
    let record =
        Value::parse(text).map_err(|_| api_err(409, "recovery_corrupt", "自动恢复记录损坏"))?;
    if record.get("card").and_then(Value::as_str) != Some(card)
        || record
            .get("desired_running")
            .and_then(Value::as_bool)
            .is_none()
        || record.get("attempts").and_then(Value::as_arr).is_none()
        || record
            .get("boot_enabled")
            .is_some_and(|v| v.as_bool().is_none())
        || !matches!(
            record.get("phase").and_then(Value::as_str),
            Some("fencing" | "ready" | "archiving" | "archived")
        )
        || record
            .get("archived_previous_boot")
            .is_some_and(|v| v.as_bool().is_none())
        || (matches!(
            record.get("phase").and_then(Value::as_str),
            Some("archiving" | "archived")
        ) && (record.get("desired_running").and_then(Value::as_bool) != Some(false)
            || boot_enabled(&record)))
    {
        return Err(api_err(409, "recovery_corrupt", "自动恢复记录字段不合法"));
    }
    Ok(Some(record))
}
fn write(app: &App, card: &str, record: &Value) -> Result<(), ApiErr> {
    // A component rollback must not silently turn a disabled boot preference
    // back into the old combined running/boot intent.
    if record.get("boot_enabled").is_some() {
        crate::private_storage::secure_state(&app.config.state_dir)
            .map_err(|e| api_err(500, "recovery_persist_failed", &e))?;
        let marker = app.config.state_dir.join("model-boot-policy-v1");
        const FORMAT: &[u8] = b"megacube-model-boot-policy-v1\n";
        match crate::private_storage::read(&marker)
            .map_err(|e| api_err(409, "recovery_corrupt", &e))?
        {
            Some(bytes) if bytes == FORMAT => {}
            Some(_) => return Err(api_err(409, "recovery_corrupt", "模型自启策略标记不合法")),
            None => crate::private_storage::publish(&marker, FORMAT, false)
                .map_err(|e| api_err(500, "recovery_persist_failed", &e))?,
        }
    }
    let path = path(app, card)?;
    let parent = path.parent().unwrap();
    std::fs::create_dir_all(parent)
        .map_err(|e| api_err(500, "recovery_persist_failed", &e.to_string()))?;
    if std::fs::symlink_metadata(parent)
        .map_err(|e| api_err(500, "recovery_persist_failed", &e.to_string()))?
        .file_type()
        .is_symlink()
    {
        return Err(api_err(409, "recovery_corrupt", "恢复目录不能是符号链接"));
    }
    std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| api_err(500, "recovery_persist_failed", &e.to_string()))?;
    let stamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let temp = parent.join(format!(".recovery-{stamp}.tmp"));
    let result = (|| -> std::io::Result<()> {
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&temp)?;
        file.write_all(record.to_string().as_bytes())?;
        file.sync_all()?;
        std::fs::rename(&temp, &path)?;
        std::fs::File::open(parent)?.sync_all()
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(temp);
    }
    result.map_err(|e| api_err(500, "recovery_persist_failed", &e.to_string()))
}
fn initial(card: &str, desired: bool) -> Value {
    let mut record = Value::obj();
    record.set("card", card.into());
    record.set("desired_running", desired.into());
    record.set("boot_enabled", desired.into());
    record.set("phase", "fencing".into());
    record.set("attempts", Value::Arr(Vec::new()));
    record.set("boot_id", crate::peer::boot_id().into());
    record.set("next_attempt_at", 0u64.into());
    record.set("circuit_open", false.into());
    record
}

/// Caller holds store mutex. Disable intent is durable before external stop.
pub fn disarm(app: &App, card: &str) -> Result<(), ApiErr> {
    if let Some(mut record) = read(app, card)? {
        record.set("desired_running", false.into());
        record.set("boot_enabled", false.into());
        write(app, card, &record)?;
    }
    Ok(())
}

/// Legacy records used the running intent for both crash and boot recovery.
pub fn boot_enabled(record: &Value) -> bool {
    record
        .get("boot_enabled")
        .and_then(Value::as_bool)
        .unwrap_or_else(|| record.get("desired_running").and_then(Value::as_bool) == Some(true))
}

/// Change next-boot policy without starting/stopping the current model. All
/// automatic starts still enter the normal lifecycle admission and budget path.
pub fn set_boot(app: &Arc<App>, card: &str, enabled: bool) -> Result<Value, ApiErr> {
    let selection = if enabled {
        crate::store_adoption::selection::admit(app, card)?
    } else {
        crate::store_adoption::selection::admit_stop(app, card)?
    };
    let card = selection.card.as_str();
    if enabled {
        let model = app
            .store
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .state
            .models
            .get(card)
            .cloned();
        if let Some(model) =
            model.filter(|model| model.lifecycle_owned && model.engine_type == "container")
        {
            crate::container_runtime::inspect_recorded(app, &model)
                .map_err(|e| api_err(409, "configuration_changed", &e))?;
        }
    }
    let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    crate::component::check_admission(app)?;
    crate::model_maintenance::check_admission(app, "autostart", card, None)?;
    if !store
        .state
        .models
        .get(card)
        .is_some_and(|m| m.lifecycle_owned)
    {
        return Err(api_err(
            409,
            "ownership_required",
            "仅明确托管模型可设置自启",
        ));
    }
    if store
        .state
        .tasks
        .values()
        .any(|t| !t.status.is_terminal() && (t.card == card || t.kind != "install"))
    {
        return Err(api_err(409, "task_in_progress", "设备变更任务仍在进行"));
    }
    let mut record =
        read(app, card)?.ok_or_else(|| api_err(409, "recovery_missing", "请先明确启用托管模型"))?;
    if enabled && record.get("source").and_then(Value::as_str) == Some("model-store") {
        let declaration = crate::ops::find_card(app, card)?;
        if declaration.deploy.is_none() && declaration.docker_deploy.is_none() {
            return Err(api_err(409, "not_deployed", "请先在模型商店完成部署"));
        }
    }
    if record.get("migration_pending").and_then(Value::as_bool) == Some(true)
        || record.get("phase").and_then(Value::as_str) != Some("ready")
    {
        return Err(api_err(
            409,
            "migration_unconfirmed",
            "恢复策略尚未完成核实",
        ));
    }
    if boot_enabled(&record) != enabled || record.get("boot_enabled").is_none() {
        crate::lifecycle_revision::reserve(app, &mut store)?;
        record.set("boot_enabled", enabled.into());
        write(app, card, &record)?;
        store
            .save_if_dirty()
            .map_err(|e| api_err(500, "recovery_persist_failed", &e.to_string()))?;
    }
    let mut result = Value::obj();
    if let Some(logical) = selection.logical.as_deref() {
        crate::store_adoption::selection::save_boot(app, logical, enabled)
            .map_err(|message| api_err(500, "boot_policy_persist_failed", &message))?;
    }
    result.set("card", card.into());
    result.set("boot_enabled", enabled.into());
    Ok(result)
}

fn reconcile_boot(record: &mut Value, boot_id: &str) -> bool {
    if record.get("boot_id").and_then(Value::as_str) == Some(boot_id) {
        return false;
    }
    record.set("desired_running", boot_enabled(record).into());
    record.set("boot_id", boot_id.into());
    true
}

/// Caller holds store mutex and has passed every task/gate conflict check.
/// Attempts survive daemon restart; no restart-loop escape by restarting cmsd.
pub fn reserve_attempt(app: &App, card: &str) -> Result<(), ApiErr> {
    let mut record =
        read(app, card)?.ok_or_else(|| api_err(409, "recovery_missing", "未授予自动恢复"))?;
    if record.get("desired_running").and_then(Value::as_bool) != Some(true)
        || record.get("phase").and_then(Value::as_str) != Some("ready")
    {
        return Err(api_err(
            409,
            "recovery_disarmed",
            "自动恢复未启用或后端隔离尚未完成",
        ));
    }
    let now = crate::timeutil::now_epoch();
    if record.get("circuit_open").and_then(Value::as_bool) == Some(true) {
        return Err(api_err(
            409,
            "recovery_circuit_open",
            "自动恢复已熔断，请核查后显式启用",
        ));
    }
    let mut attempts: Vec<Value> = record
        .get("attempts")
        .and_then(Value::as_arr)
        .unwrap()
        .iter()
        .filter(|v| v.as_u64().is_some_and(|t| now.saturating_sub(t) < 300))
        .cloned()
        .collect();
    if attempts.len() >= 3 {
        record.set("circuit_open", true.into());
        write(app, card, &record)?;
        return Err(api_err(
            409,
            "recovery_circuit_open",
            "5分钟内已自动恢复3次，已熔断；请核查日志、资源和部署后显式启用",
        ));
    }
    if record
        .get("next_attempt_at")
        .and_then(Value::as_u64)
        .unwrap_or(0)
        > now
    {
        return Err(api_err(409, "recovery_backoff", "自动恢复退避中"));
    }
    attempts.push(now.into());
    let backoff = 5u64 << (attempts.len() - 1);
    record.set("attempts", Value::Arr(attempts));
    record.set("next_attempt_at", (now + backoff).into());
    record.set("boot_id", crate::peer::boot_id().into());
    write(app, card, &record)
}

fn fence(app: &App, model: &ModelState) -> Result<(), ApiErr> {
    if model.engine_type == "container" {
        let name = model
            .container_name
            .as_deref()
            .ok_or_else(|| api_err(409, "state_unknown", "缺少容器引用"))?;
        if let Some(container) = crate::container_runtime::inspect_recorded(app, model)
            .map_err(|e| api_err(409, "recovery_fence_failed", &e))?
        {
            if container.restart_policy != "no" {
                app.docker
                    .set_restart_policy(name, "no")
                    .map_err(|e| api_err(409, "recovery_fence_failed", &e))?;
            }
        }
    } else {
        crate::store_adoption::runtime::fence(app, &model.card)
            .map_err(|e| api_err(409, "recovery_fence_failed", &e))?;
        app.systemd
            .fence_recovery(&app.systemd.unit_name_for(&model.card))
            .map_err(|e| api_err(409, "recovery_fence_failed", &e))?;
    }
    Ok(())
}

/// Archives keep facts for diagnostics, but never keep recovery/start authority.
pub(crate) fn archive(app: &Arc<App>, model: &ModelState) -> Result<(), ApiErr> {
    {
        let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let mut record = read(app, &model.card)?.unwrap_or_else(|| initial(&model.card, false));
        if record.get("phase").and_then(Value::as_str) == Some("archived") {
            return Ok(());
        }
        let old_boot = record
            .get("archived_previous_boot")
            .and_then(Value::as_bool)
            .unwrap_or_else(|| boot_enabled(&record));
        record.set("archived_previous_boot", old_boot.into());
        record.set("desired_running", false.into());
        record.set("boot_enabled", false.into());
        record.set("migration_pending", false.into());
        record.set("phase", "archiving".into());
        write(app, &model.card, &record)?;
    }
    fence(app, model)?;
    let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let mut record =
        read(app, &model.card)?.ok_or_else(|| api_err(409, "recovery_missing", "归档策略丢失"))?;
    record.set("phase", "archived".into());
    write(app, &model.card, &record)
}

/// Preserve the logical model's explicit next-boot preference across upgrades.
pub(crate) fn inherit_boot(app: &Arc<App>, card: &str, enabled: bool) -> Result<(), ApiErr> {
    let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    if !store
        .state
        .models
        .get(card)
        .is_some_and(|model| model.lifecycle_owned)
    {
        return Ok(());
    }
    let Some(mut record) = read(app, card)? else {
        return Ok(());
    };
    let reselected = record.get("phase").and_then(Value::as_str) == Some("archived")
        && crate::store_adoption::selection::cached(app)
            .is_ok_and(|snapshot| snapshot.metadata(card).is_some());
    if reselected {
        // Only the store's verified current deployment can reuse an archived
        // physical version. Restoring next-boot intent does not start it now.
        crate::store_adoption::selection::ensure_current(app, card)
            .map_err(|e| api_err(409, "store_selection_changed", &e))?;
    } else if record.get("phase").and_then(Value::as_str) != Some("ready") {
        return Ok(());
    }
    if reselected || boot_enabled(&record) != enabled {
        crate::lifecycle_revision::reserve(app, &mut store)?;
        record.set("boot_enabled", enabled.into());
        if reselected {
            record.set("phase", "ready".into());
        }
        write(app, card, &record)?;
        store
            .save_if_dirty()
            .map_err(|error| api_err(500, "recovery_persist_failed", &error.to_string()))?;
    }
    Ok(())
}

/// Interactive enable persists intent, then removes independent restart paths.
pub fn arm(app: &App, card: &str, automatic: bool) -> Result<(), ApiErr> {
    let model = {
        let store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let model = store
            .state
            .models
            .get(card)
            .filter(|m| m.lifecycle_owned)
            .ok_or_else(|| api_err(409, "ownership_required", "仅明确托管对象可启用自动恢复"))?;
        let mut record = read(app, card)?.unwrap_or_else(|| initial(card, true));
        // Preserve an explicit next-boot preference across interactive starts.
        record.set("boot_enabled", boot_enabled(&record).into());
        record.set("desired_running", true.into());
        record.set("migration_pending", false.into());
        record.set("phase", "fencing".into());
        if !automatic {
            record.set("attempts", Value::Arr(Vec::new()));
            record.set("next_attempt_at", 0u64.into());
            record.set("circuit_open", false.into());
        }
        write(app, card, &record)?;
        model.clone()
    };
    fence(app, &model)?;
    let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let mut record =
        read(app, card)?.ok_or_else(|| api_err(409, "recovery_missing", "恢复记录丢失"))?;
    record.set("phase", "ready".into());
    write(app, card, &record)
}

/// Adopt existing store inventory, preserving observed running and boot intent.
/// This never starts a model or changes the default. Existing policy wins.
pub fn adopt_store(app: &App, model: &ModelState, running: bool, boot: bool) -> Result<(), ApiErr> {
    let existing = read(app, &model.card)?;
    let mut record = existing
        .clone()
        .unwrap_or_else(|| initial(&model.card, running));
    if existing.is_none() {
        record.set("boot_enabled", boot.into());
    }
    record.set("source", "model-store".into());
    record.set("phase", "fencing".into());
    write(app, &model.card, &record)?;
    let card = crate::ops::find_card(app, &model.card)?;
    if card.deploy.is_some() || card.docker_deploy.is_some() {
        fence(app, model)?;
    }
    record.set("phase", "ready".into());
    write(app, &model.card, &record)
}

/// Startup only: migrate explicitly owned legacy models from fresh backend
/// facts; never infer intent from a stale 'running' cache on its own.
pub fn migrate(app: &Arc<App>) {
    if crate::component::check_admission(app).is_err() {
        return;
    }
    if app
        .lifecycle_revision_error
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .is_some()
    {
        return;
    }
    let models: Vec<_> = app
        .store
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .state
        .models
        .values()
        .filter(|m| {
            m.lifecycle_owned
                && crate::store_adoption::selection::cached(app)
                    .is_ok_and(|selection| selection.visible(&m.card))
        })
        .cloned()
        .collect();
    for model in models {
        if crate::model_maintenance::check_admission(app, "recover", &model.card, None).is_err() {
            continue;
        }
        let result = (|| -> Result<(), ApiErr> {
            let existing = read(app, &model.card)?;
            if existing.is_none()
                || existing
                    .as_ref()
                    .and_then(|r| r.get("migration_pending"))
                    .and_then(Value::as_bool)
                    == Some(true)
            {
                {
                    let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
                    let mut pending = initial(&model.card, false);
                    pending.set("migration_pending", true.into());
                    write(app, &model.card, &pending)?;
                }
                let desired = if model.engine_type == "container" {
                    let name = model
                        .container_name
                        .as_deref()
                        .ok_or_else(|| api_err(409, "state_unknown", "缺少容器引用"))?;
                    app.docker
                        .inspect(name)
                        .map_err(|e| api_err(409, "backend_unavailable", &e))?
                        .is_some_and(|c| c.running || c.is_autostart())
                } else {
                    let unit = app.systemd.unit_name_for(&model.card);
                    app.systemd
                        .show(&unit)
                        .map_err(|e| api_err(409, "backend_unavailable", &e))?
                        .is_active()
                        || app
                            .systemd
                            .enabled_checked(&unit)
                            .map_err(|e| api_err(409, "backend_unavailable", &e))?
                };
                let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
                write(app, &model.card, &initial(&model.card, desired))?;
            }
            fence(app, &model)?;
            let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
            let mut record = read(app, &model.card)?.unwrap();
            record.set("phase", "ready".into());
            write(app, &model.card, &record)
        })();
        if let Err((_, code, message)) = result {
            log_line(&format!(
                "模型 {} 恢复迁移待核查：{code}: {message}",
                model.card
            ));
        }
    }
}

pub fn view(app: &App, card: &str) -> Result<Option<Value>, ApiErr> {
    read(app, card)
}

pub fn tick(app: &Arc<App>) {
    if app
        .lifecycle_revision_error
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .is_some()
    {
        return;
    }
    let models: Vec<_> = app
        .store
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .state
        .models
        .values()
        .filter(|m| {
            m.lifecycle_owned
                && managed(app, &m.card)
                && crate::store_adoption::selection::cached(app)
                    .is_ok_and(|selection| selection.visible(&m.card))
        })
        .cloned()
        .collect();
    for model in models {
        if let Err((_, code, message)) = observe(app, &model) {
            if code == "task_in_progress"
                || code == "model_maintenance"
                || code == "component_maintenance"
                || code == "recovery_backoff"
            {
                continue;
            }
            let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
            apply_observe_error(&mut store, &model.card, &code, &message);
        }
    }
}

/// r14 R2：观测错误路径的 diff-before-mark——恢复循环每 10s 一跳，同一
/// 错误码与消息重复出现时不重写 degraded_reason/status，也就不置脏
/// store（避免 supervisor 的 2s save_store 反复全量落盘的写放大）。
fn apply_observe_error(
    store: &mut crate::store::StateStore,
    card: &str,
    code: &str,
    message: &str,
) {
    let Some(current) = store.state.models.get_mut(card) else {
        return;
    };
    let mut changed = false;
    if code == "recovery_circuit_open" && current.status != ModelStatus::Degraded {
        current.status = ModelStatus::Degraded;
        changed = true;
    }
    let reason = format!("{code}: {message}");
    if current.degraded_reason.as_deref() != Some(reason.as_str()) {
        current.degraded_reason = Some(reason);
        changed = true;
    }
    if changed {
        store.mark_dirty();
    }
}

fn observe(app: &Arc<App>, model: &ModelState) -> Result<(), ApiErr> {
    if read(app, &model.card)?.and_then(|r| r.get("migration_pending").and_then(Value::as_bool))
        == Some(true)
    {
        return Err(api_err(
            409,
            "migration_unconfirmed",
            "旧恢复策略尚未核实，请核查后显式启用或重启cmsd重试迁移",
        ));
    }
    let epoch = {
        let store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        crate::component::check_admission(app)?;
        crate::model_maintenance::check_admission(app, "recover", &model.card, None)?;
        if store
            .state
            .tasks
            .values()
            .any(|t| !t.status.is_terminal() && (t.card == model.card || t.kind != "install"))
        {
            return Err(api_err(409, "task_in_progress", "设备变更进行中"));
        }
        app.lifecycle_epoch
            .load(std::sync::atomic::Ordering::Acquire)
    };
    fence(app, model)?;
    let (running, pid, clean, missing) = if model.engine_type == "container" {
        match crate::container_runtime::inspect_recorded(app, model)
            .map_err(|e| api_err(409, "configuration_changed", &e))?
        {
            Some(c) => (
                c.running,
                c.pid,
                !c.oom_killed && [0, 143].contains(&c.exit_code),
                false,
            ),
            None => (false, 0, false, true),
        }
    } else {
        let s = crate::store_adoption::native_state(app, &model.card)
            .and_then(|state| {
                state
                    .map(Ok)
                    .unwrap_or_else(|| app.systemd.show(&app.systemd.unit_name_for(&model.card)))
            })
            .map_err(|e| api_err(409, "backend_unavailable", &e))?;
        if ["activating", "deactivating"].contains(&s.active_state.as_str()) {
            return Ok(());
        }
        (
            s.is_active(),
            s.main_pid,
            s.result == "success",
            s.not_found,
        )
    };
    let (desired, record) = {
        let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        if epoch
            != app
                .lifecycle_epoch
                .load(std::sync::atomic::Ordering::Acquire)
        {
            return Err(api_err(
                409,
                "task_in_progress",
                "查询期间设备变更，等待下轮新事实",
            ));
        }
        let mut record = read(app, &model.card)?
            .ok_or_else(|| api_err(409, "recovery_missing", "恢复记录缺失"))?;
        let new_boot = reconcile_boot(&mut record, &crate::peer::boot_id());
        let phase_changed = record.get("phase").and_then(Value::as_str) != Some("ready");
        if phase_changed {
            record.set("phase", "ready".into());
        }
        let desired = record.get("desired_running").and_then(Value::as_bool) == Some(true);
        // Recheck after backend I/O. A disable/maintenance may have been admitted.
        crate::component::check_admission(app)?;
        crate::model_maintenance::check_admission(app, "recover", &model.card, None)?;
        if store
            .state
            .tasks
            .values()
            .any(|t| !t.status.is_terminal() && (t.card == model.card || t.kind != "install"))
        {
            return Err(api_err(409, "task_in_progress", "设备变更进行中"));
        }
        let Some(current) = store.state.models.get_mut(&model.card) else {
            return Ok(());
        };
        if !current.lifecycle_owned {
            return Ok(());
        }
        let was_running = current.status == ModelStatus::Running;
        // r14 R2：观测值未变化时不 mark_dirty——恢复循环每 10s 对每个受管
        // 模型走到这里，无条件覆写会让 supervisor 的 2s save_store 每次都
        // 全量序列化+双 fsync（单板机 eMMC 写放大与持锁抖动）。与
        // supervisor.rs 的 diff-before-mark 纪律对齐。
        let next_status = if running {
            ModelStatus::Running
        } else {
            ModelStatus::Stopped
        };
        let next_pid = Some(pid).filter(|p| *p > 0);
        let mut changed = current.status != next_status || current.main_pid != next_pid;
        current.status = next_status;
        current.main_pid = next_pid;
        if running {
            if model.main_pid.is_some_and(|old| old != pid) {
                current.restart_count += 1;
                current.last_restart_at = Some(crate::timeutil::now_epoch());
            }
            if current.degraded_reason.take().is_some() {
                changed = true;
            }
        } else if current.degraded_reason.as_deref().is_some_and(|reason| {
            // These checks succeeded in this observation. A deliberately stopped
            // model need not be started just to clear a repaired control error.
            // Keep crash/circuit/deployment diagnostics until their own recovery.
            reason.starts_with("recovery_fence_failed:")
                || reason.starts_with("backend_unavailable:")
                || (!missing && reason.starts_with("runtime_missing:"))
        }) {
            current.degraded_reason = None;
            changed = true;
        }
        if !running
            && clean
            && !new_boot
            && was_running
            && record.get("boot_id").and_then(Value::as_str)
                == Some(crate::peer::boot_id().as_str())
        {
            crate::store_adoption::selection::disarm_current_boot_locked(app, &model.card)
                .map_err(|message| api_err(500, "boot_policy_persist_failed", &message))?;
            disarm(app, &model.card)?;
            current.degraded_reason = Some("检测到管理面外的正常停止，已撤自动恢复意图".into());
            if store.state.active_model.as_deref() == Some(&model.card) {
                store.state.active_model = None;
            }
            store.mark_dirty();
            return Ok(());
        }
        if changed {
            store.mark_dirty();
        }
        if new_boot {
            // Persist the fresh runtime state before stamping the new boot.
            // Otherwise a crash here could mistake a prior-boot running cache
            // for an external clean stop and revoke next-boot intent.
            store
                .save_if_dirty()
                .map_err(|e| api_err(500, "recovery_persist_failed", &e.to_string()))?;
        }
        if new_boot || phase_changed {
            write(app, &model.card, &record)?;
        }
        (desired, record)
    };
    if running {
        if record.get("boot_id").and_then(Value::as_str) != Some(crate::peer::boot_id().as_str()) {
            let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
            let mut record = read(app, &model.card)?
                .ok_or_else(|| api_err(409, "recovery_missing", "恢复记录缺失"))?;
            record.set("boot_id", crate::peer::boot_id().into());
            write(app, &model.card, &record)?;
        }
        return Ok(());
    }
    if desired {
        if missing {
            return Err(api_err(
                409,
                "runtime_missing",
                "托管运行时已不存在，请在模型商店修复部署后显式启用；不会自动重跑安装脚本",
            ));
        }
        let task = crate::ops::enable_automatic(app, &model.card)?;
        log_line(&format!("模型 {} 进入统一预算恢复任务 {task}", model.card));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::docker::fixtures as fx;
    #[test]
    fn unknown_recovery_receipts_and_directories_never_disarm_or_default() {
        for kind in ["fifo", "dangling", "directory-link"] {
            let root = fx::tmpdir(&format!("recovery-control-{kind}"));
            let app = fx::make_app(&root, 128, 100);
            crate::private_storage::secure_state(&app.config.state_dir).unwrap();
            let directory = app.config.state_dir.join("recovery");
            let record = directory.join("card-a.json");
            if kind == "directory-link" {
                std::os::unix::fs::symlink(root.join("missing"), &directory).unwrap();
            } else {
                crate::private_storage::directory(&directory, true).unwrap();
                if kind == "fifo" {
                    use std::os::unix::ffi::OsStrExt;
                    let name = std::ffi::CString::new(record.as_os_str().as_bytes()).unwrap();
                    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
                } else {
                    std::os::unix::fs::symlink(root.join("missing"), &record).unwrap();
                }
            }
            assert!(view(&app, "card-a").is_err(), "{kind}");
            assert!(disarm(&app, "card-a").is_err(), "{kind}");
            assert!(std::fs::symlink_metadata(if kind == "directory-link" {
                directory
            } else {
                record
            })
            .is_ok());
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn steady_observation_does_not_remark_the_store_dirty() {
        let (root, app, _) = fixture("steady-observe-diff-guard", 100);
        arm(&app, "qa-recover", false).unwrap();
        disarm(&app, "qa-recover").unwrap();
        let model = {
            let mut store = app.store.lock().unwrap();
            let model = store.state.models.get_mut("qa-recover").unwrap();
            model.status = ModelStatus::Stopped;
            model.degraded_reason = None;
            model.clone()
        };
        // 第一次观测把缓存对齐运行态；随后落盘清脏。
        observe(&app, &model).unwrap();
        {
            let mut store = app.store.lock().unwrap();
            store.save_if_dirty().unwrap();
            assert!(!store.is_dirty());
        }
        // 稳态复测（r14 R2）：status/pid/reason 均未变化时不得再次置脏。
        let model = app.store.lock().unwrap().state.models["qa-recover"].clone();
        observe(&app, &model).unwrap();
        let store = app.store.lock().unwrap();
        assert!(
            !store.is_dirty(),
            "稳态观测不得再次置脏 store（r14 R2 写放大）"
        );
        assert_eq!(
            store.state.models["qa-recover"].status,
            ModelStatus::Stopped
        );
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn repeated_observe_error_does_not_remark_the_store_dirty() {
        let (root, app, _) = fixture("steady-error-diff-guard", 100);
        arm(&app, "qa-recover", false).unwrap();
        disarm(&app, "qa-recover").unwrap();
        let mut store = app.store.lock().unwrap();
        store.state.models.get_mut("qa-recover").unwrap().status = ModelStatus::Stopped;
        apply_observe_error(
            &mut store,
            "qa-recover",
            "runtime_missing",
            "container was recreated",
        );
        assert!(store.is_dirty());
        assert_eq!(
            store.state.models["qa-recover"].degraded_reason.as_deref(),
            Some("runtime_missing: container was recreated")
        );
        store.save_if_dirty().unwrap();
        // 同一错误码+消息重复出现（r14 R2）：不得再次置脏。
        apply_observe_error(
            &mut store,
            "qa-recover",
            "runtime_missing",
            "container was recreated",
        );
        assert!(!store.is_dirty(), "重复错误不得再次置脏 store");
        // 新消息或熔断仍要如实落账。
        apply_observe_error(
            &mut store,
            "qa-recover",
            "recovery_circuit_open",
            "crash loop",
        );
        assert!(store.is_dirty());
        assert_eq!(
            store.state.models["qa-recover"].status,
            ModelStatus::Degraded
        );
        std::fs::remove_dir_all(root).unwrap();
    }

    fn fixture(tag: &str, available: u64) -> (PathBuf, Arc<App>, u16) {
        let root = fx::tmpdir(tag);
        let app = fx::make_app(&root, 128, available);
        let port = fx::serve_models_json("Recovery-Model");
        fx::write_docker_card(
            &app.config.cards_dir,
            "qa-recover",
            "qa-recover-ctr",
            port,
            Some("0.01"),
        );
        fx::set_container(
            &root,
            "qa-recover-ctr",
            "fixture/img:1.0",
            false,
            "no",
            0,
            false,
            0,
            port,
            8000,
            "--served-model-name,Recovery-Model,--mem-fraction-static,0.01",
        );
        let card = crate::ops::find_card(&app, "qa-recover").unwrap();
        let c = app.docker.inspect("qa-recover-ctr").unwrap().unwrap();
        crate::ops::register_container_managed(&app, &card, &c);
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("qa-recover")
            .unwrap()
            .lifecycle_owned = true;
        (root, app, port)
    }
    #[test]
    fn explicit_store_rollback_restores_logical_boot_without_replaying_running_intent() {
        let (_root, app, _port) = fixture("reselected-store-version", 100);
        arm(&app, "qa-recover", false).unwrap();
        let model = app.store.lock().unwrap().state.models["qa-recover"].clone();
        archive(&app, &model).unwrap();
        // A retained directory or an empty selection cannot unarchive policy.
        inherit_boot(&app, "qa-recover", true).unwrap();
        assert_eq!(
            view(&app, "qa-recover")
                .unwrap()
                .unwrap()
                .get("phase")
                .and_then(Value::as_str),
            Some("archived")
        );
        let files: serde_json::Map<String, serde_json::Value> = ["common.sh", "vendor/.deploy-env"]
            .into_iter()
            .map(|name| {
                let bytes =
                    std::fs::read(app.config.cards_dir.join("qa-recover").join(name)).unwrap();
                (
                    name.into(),
                    ring::digest::digest(&ring::digest::SHA256, &bytes)
                        .as_ref()
                        .iter()
                        .map(|b| format!("{b:02x}"))
                        .collect::<String>()
                        .into(),
                )
            })
            .collect();
        let inventory = serde_json::json!({"selection_policy":"latest-installed-v1",
            "selections":[{"logical_card":"qa-recover","card":"qa-recover","version_id":"restored"}],
            "archived_models":[], "models":[{"logical_card":"qa-recover","card":"qa-recover","version_id":"restored",
                "deployment_revision":"restored-profile","origin":"store","store_status":"deployed","downloaded_at":"today","files":files}]});
        let snapshot = crate::store_adoption::selection::publish(&app, &inventory).unwrap();
        crate::store_adoption::selection::save_boot(&app, "qa-recover", true).unwrap();
        crate::store_adoption::selection::fence_archived(&app, &snapshot).unwrap();
        let restored = view(&app, "qa-recover").unwrap().unwrap();
        assert_eq!(restored.get("phase").and_then(Value::as_str), Some("ready"));
        assert!(boot_enabled(&restored));
        assert_eq!(
            restored.get("desired_running").and_then(Value::as_bool),
            Some(false)
        );
        assert!(crate::store_adoption::selection::ensure_current(&app, "qa-recover").is_ok());
    }

    #[test]
    fn archived_policy_is_durable_resumable_and_never_recovered_as_ready() {
        let (_root, app, _) = fixture("archive-durable", 100);
        arm(&app, "qa-recover", false).unwrap();
        let mut interrupted = read(&app, "qa-recover").unwrap().unwrap();
        interrupted.set("phase", "archiving".into());
        interrupted.set("desired_running", false.into());
        interrupted.set("boot_enabled", false.into());
        interrupted.set("archived_previous_boot", true.into());
        write(&app, "qa-recover", &interrupted).unwrap();
        let model = app.store.lock().unwrap().state.models["qa-recover"].clone();
        archive(&app, &model).unwrap();
        archive(&app, &model).unwrap();
        let archived = read(&app, "qa-recover").unwrap().unwrap();
        assert_eq!(
            archived.get("phase").and_then(Value::as_str),
            Some("archived")
        );
        assert_eq!(
            archived
                .get("archived_previous_boot")
                .and_then(Value::as_bool),
            Some(true)
        );
        crate::store_adoption::selection::publish(&app,&serde_json::json!({"selection_policy":"latest-installed-v1","selections":[],"models":[],"archived_models":[{"logical_card":"qa-recover","card":"qa-recover","version_id":"retired"}]})).unwrap();
        let before = app.store.lock().unwrap().state.tasks.len();
        migrate(&app);
        tick(&app);
        assert_eq!(app.store.lock().unwrap().state.tasks.len(), before);
        assert_eq!(
            read(&app, "qa-recover")
                .unwrap()
                .unwrap()
                .get("phase")
                .and_then(Value::as_str),
            Some("archived")
        );
        let mut unsafe_record = archived;
        unsafe_record.set("desired_running", true.into());
        write(&app, "qa-recover", &unsafe_record).unwrap();
        assert!(read(&app, "qa-recover").is_err());
    }

    #[test]
    fn stopped_model_clears_only_errors_verified_resolved_without_starting() {
        let (root, app, _) = fixture("stopped-repaired-control", 100);
        arm(&app, "qa-recover", false).unwrap();
        disarm(&app, "qa-recover").unwrap();
        for reason in [
            "recovery_fence_failed: model_unit_namespace_conflict",
            "backend_unavailable: docker temporarily unavailable",
            "runtime_missing: container was recreated",
        ] {
            let model = {
                let mut store = app.store.lock().unwrap();
                let model = store.state.models.get_mut("qa-recover").unwrap();
                model.status = ModelStatus::Stopped;
                model.degraded_reason = Some(reason.into());
                model.clone()
            };
            observe(&app, &model).unwrap();
            let store = app.store.lock().unwrap();
            assert_eq!(
                store.state.models["qa-recover"].status,
                ModelStatus::Stopped
            );
            assert!(store.state.models["qa-recover"].degraded_reason.is_none());
            assert!(store.state.tasks.is_empty());
        }
        assert!(!fx::docker_calls(&root)
            .iter()
            .any(|call| call.starts_with("start ")));
        assert_eq!(
            read(&app, "qa-recover")
                .unwrap()
                .unwrap()
                .get("desired_running")
                .and_then(Value::as_bool),
            Some(false)
        );
    }

    #[test]
    fn stopped_model_keeps_unresolved_runtime_and_other_fault_diagnostics() {
        let (root, app, _) = fixture("stopped-unresolved-control", 100);
        arm(&app, "qa-recover", false).unwrap();
        disarm(&app, "qa-recover").unwrap();
        std::fs::write(root.join("docker.state"), "").unwrap();
        for reason in [
            "runtime_missing: container still missing",
            "recovery_circuit_open: repeated crashes",
            "engine exited: OOM",
        ] {
            let model = {
                let mut store = app.store.lock().unwrap();
                let model = store.state.models.get_mut("qa-recover").unwrap();
                model.status = ModelStatus::Stopped;
                model.degraded_reason = Some(reason.into());
                model.clone()
            };
            observe(&app, &model).unwrap();
            let store = app.store.lock().unwrap();
            assert_eq!(
                store.state.models["qa-recover"].degraded_reason.as_deref(),
                Some(reason)
            );
            assert!(store.state.tasks.is_empty());
        }
        arm(&app, "qa-recover", false).unwrap();
        let model = app.store.lock().unwrap().state.models["qa-recover"].clone();
        assert_eq!(observe(&app, &model).unwrap_err().1, "runtime_missing");
    }

    #[test]
    fn boot_policy_is_independent_idempotent_durable_and_fenced() {
        let (root, app, _) = fixture("boot-policy", 100);
        arm(&app, "qa-recover", false).unwrap();
        let before = fx::docker_calls(&root);
        set_boot(&app, "qa-recover", false).unwrap();
        let revision = app.store.lock().unwrap().state.lifecycle_revision;
        set_boot(&app, "qa-recover", false).unwrap();
        assert_eq!(app.store.lock().unwrap().state.lifecycle_revision, revision);
        let record = read(&app, "qa-recover").unwrap().unwrap();
        assert!(!boot_enabled(&record));
        assert_eq!(
            record.get("desired_running").and_then(Value::as_bool),
            Some(true)
        );
        assert_eq!(fx::docker_calls(&root), before);
        migrate(&app);
        assert!(!boot_enabled(&read(&app, "qa-recover").unwrap().unwrap()));
        arm(&app, "qa-recover", false).unwrap();
        assert!(!boot_enabled(&read(&app, "qa-recover").unwrap().unwrap()));
        crate::model_maintenance::change(
            &app,
            "qa-recover",
            br#"{"action":"begin","owner":"qa-boot"}"#,
            crate::peer::LocalPeer::current(),
        )
        .unwrap();
        assert_eq!(
            set_boot(&app, "qa-recover", true).unwrap_err().1,
            "model_maintenance"
        );
    }

    #[test]
    fn stopped_model_boot_enable_does_not_start_now_and_new_boot_applies_policy() {
        let (root, app, _) = fixture("boot-stopped", 100);
        arm(&app, "qa-recover", false).unwrap();
        disarm(&app, "qa-recover").unwrap();
        let before = fx::docker_calls(&root);
        set_boot(&app, "qa-recover", true).unwrap();
        tick(&app);
        assert!(app.store.lock().unwrap().state.tasks.is_empty());
        assert!(!fx::docker_calls(&root)[before.len()..]
            .iter()
            .any(|c| c.starts_with("start ")));
        let mut record = read(&app, "qa-recover").unwrap().unwrap();
        assert_eq!(
            record.get("desired_running").and_then(Value::as_bool),
            Some(false)
        );
        assert!(reconcile_boot(&mut record, "different-boot"));
        assert_eq!(
            record.get("desired_running").and_then(Value::as_bool),
            Some(true)
        );
        assert!(!reconcile_boot(&mut record, "different-boot"));
        record.set("boot_enabled", false.into());
        assert!(reconcile_boot(&mut record, "next-boot"));
        assert_eq!(
            record.get("desired_running").and_then(Value::as_bool),
            Some(false)
        );
    }

    #[test]
    fn external_missing_and_corrupt_boot_policies_fail_closed() {
        let (_root, app, _) = fixture("boot-guards", 100);
        assert_eq!(
            set_boot(&app, "qa-recover", true).unwrap_err().1,
            "recovery_missing"
        );
        arm(&app, "qa-recover", false).unwrap();
        let mut record = read(&app, "qa-recover").unwrap().unwrap();
        record.set("boot_enabled", "yes".into());
        write(&app, "qa-recover", &record).unwrap();
        assert_eq!(
            set_boot(&app, "qa-recover", false).unwrap_err().1,
            "recovery_corrupt"
        );
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("qa-recover")
            .unwrap()
            .lifecycle_owned = false;
        assert_eq!(
            set_boot(&app, "qa-recover", true).unwrap_err().1,
            "ownership_required"
        );
    }

    #[test]
    fn crash_recovery_rechecks_budget_and_never_spends_low_memory() {
        let (root, app, port) = fixture("recover-budget", 1);
        arm(&app, "qa-recover", false).unwrap();
        fx::set_container(
            &root,
            "qa-recover-ctr",
            "fixture/img:1.0",
            false,
            "no",
            137,
            true,
            0,
            port,
            8000,
            "--served-model-name,Recovery-Model,--mem-fraction-static,0.01",
        );
        tick(&app);
        let id = app
            .store
            .lock()
            .unwrap()
            .state
            .tasks
            .values()
            .find(|t| t.kind == "recover")
            .unwrap()
            .id
            .clone();
        let task = fx::wait_terminal(&app, &id, 10);
        assert_eq!(task.status, crate::store::TaskStatus::Failed);
        assert!(!fx::docker_calls(&root)
            .iter()
            .any(|c| c.starts_with("start ")));
        assert!(view(&app, "qa-recover")
            .unwrap()
            .unwrap()
            .get("desired_running")
            .unwrap()
            .as_bool()
            .unwrap());
        assert_eq!(
            view(&app, "qa-recover")
                .unwrap()
                .unwrap()
                .get("attempts")
                .unwrap()
                .as_arr()
                .unwrap()
                .len(),
            1
        );
    }
    #[test]
    fn recovery_respects_uninstall_gate_and_manual_stop() {
        let (root, app, port) = fixture("recover-gate", 100);
        arm(&app, "qa-recover", false).unwrap();
        crate::model_maintenance::change(
            &app,
            "qa-recover",
            br#"{"action":"begin","owner":"qa-owner"}"#,
            crate::peer::LocalPeer::current(),
        )
        .unwrap();
        tick(&app);
        assert!(app.store.lock().unwrap().state.tasks.is_empty());
        crate::model_maintenance::change(
            &app,
            "qa-recover",
            br#"{"action":"end","owner":"qa-owner"}"#,
            crate::peer::LocalPeer::current(),
        )
        .unwrap();
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("qa-recover")
            .unwrap()
            .status = ModelStatus::Running;
        fx::set_container(
            &root,
            "qa-recover-ctr",
            "fixture/img:1.0",
            false,
            "no",
            143,
            false,
            0,
            port,
            8000,
            "--served-model-name,Recovery-Model,--mem-fraction-static,0.01",
        );
        tick(&app);
        assert_eq!(
            view(&app, "qa-recover")
                .unwrap()
                .unwrap()
                .get("desired_running")
                .and_then(Value::as_bool),
            Some(false)
        );
        assert!(app.store.lock().unwrap().state.tasks.is_empty());
    }
    #[test]
    fn circuit_and_attempts_are_persistent_until_explicit_enable() {
        let (_root, app, _) = fixture("recover-circuit", 100);
        arm(&app, "qa-recover", false).unwrap();
        for _ in 0..3 {
            let _store = app.store.lock().unwrap();
            let mut r = read(&app, "qa-recover").unwrap().unwrap();
            r.set("next_attempt_at", 0u64.into());
            write(&app, "qa-recover", &r).unwrap();
            reserve_attempt(&app, "qa-recover").unwrap();
        }
        assert_eq!(
            reserve_attempt(&app, "qa-recover").unwrap_err().1,
            "recovery_circuit_open"
        );
        let mut r = read(&app, "qa-recover").unwrap().unwrap();
        r.set("attempts", Value::Arr(Vec::new()));
        write(&app, "qa-recover", &r).unwrap();
        assert_eq!(
            reserve_attempt(&app, "qa-recover").unwrap_err().1,
            "recovery_circuit_open"
        );
        arm(&app, "qa-recover", false).unwrap();
        reserve_attempt(&app, "qa-recover").unwrap();
    }
    #[test]
    fn legacy_migration_preserves_intent_and_never_claims_external_resources() {
        let (root, app, port) = fixture("recover-migrate", 100);
        fx::set_container(
            &root,
            "qa-recover-ctr",
            "fixture/img:1.0",
            false,
            "unless-stopped",
            1,
            false,
            0,
            port,
            8000,
            "--mem-fraction-static,0.01",
        );
        migrate(&app);
        assert!(fx::state_line(&root, "qa-recover-ctr")
            .unwrap()
            .contains("^no^"));
        assert_eq!(
            view(&app, "qa-recover")
                .unwrap()
                .unwrap()
                .get("desired_running")
                .and_then(Value::as_bool),
            Some(true)
        );
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("qa-recover")
            .unwrap()
            .lifecycle_owned = false;
        std::fs::remove_file(path(&app, "qa-recover").unwrap()).unwrap();
        fx::set_container(
            &root,
            "qa-recover-ctr",
            "fixture/img:1.0",
            true,
            "always",
            0,
            false,
            5001,
            port,
            8000,
            "",
        );
        migrate(&app);
        tick(&app);
        assert!(!managed(&app, "qa-recover"));
        assert!(fx::state_line(&root, "qa-recover-ctr")
            .unwrap()
            .contains("^always^"));
    }
    #[test]
    fn explicit_disable_fences_before_worker_and_survives_restart() {
        let (_root, app, _) = fixture("recover-disable", 100);
        arm(&app, "qa-recover", false).unwrap();
        let _task = app.task_create_exclusive("disable", "qa-recover").unwrap();
        assert_eq!(
            view(&app, "qa-recover")
                .unwrap()
                .unwrap()
                .get("desired_running")
                .and_then(Value::as_bool),
            Some(false)
        );
        migrate(&app);
        assert_eq!(
            view(&app, "qa-recover")
                .unwrap()
                .unwrap()
                .get("desired_running")
                .and_then(Value::as_bool),
            Some(false)
        );
    }
    #[test]
    fn external_stop_clears_logical_boot_across_periodic_reconciliation() {
        let (root, app, port) = fixture("review-external-stop", 100);
        arm(&app, "qa-recover", false).unwrap();
        let inventory = serde_json::json!({"selection_policy":"latest-installed-v1",
          "selections":[{"logical_card":"qa-recover","card":"qa-recover","version_id":"current"}],
          "archived_models":[],"models":[{"logical_card":"qa-recover","card":"qa-recover","version_id":"current"}]});
        let snapshot = crate::store_adoption::selection::publish(&app, &inventory).unwrap();
        crate::store_adoption::selection::save_boot(&app, "qa-recover", true).unwrap();
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("qa-recover")
            .unwrap()
            .status = ModelStatus::Running;
        fx::set_container(
            &root,
            "qa-recover-ctr",
            "fixture/img:1.0",
            false,
            "no",
            143,
            false,
            0,
            port,
            8000,
            "--served-model-name,Recovery-Model,--mem-fraction-static,0.01",
        );
        tick(&app);
        assert!(!boot_enabled(&read(&app, "qa-recover").unwrap().unwrap()));
        crate::store_adoption::selection::fence_archived(&app, &snapshot).unwrap();
        assert!(
            !boot_enabled(&read(&app, "qa-recover").unwrap().unwrap()),
            "periodic reconciliation re-enabled externally stopped model's boot intent"
        );
    }
}
