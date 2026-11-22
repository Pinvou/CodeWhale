//! A durable per-card transaction gate spanning cmsd and store scripts. A
//! timeout or disconnected client cannot prove that destructive work stopped.
//! Admission and every gate mutation run under App.store's mutex.
use crate::app::{api_err, ApiErr, App};
use crate::json::Value;
use crate::peer::{self, LocalPeer};
use std::io::Write;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::PathBuf;
mod runtime;

/// A fresh backend observation may be used only with the exact state/gate
/// captured before I/O. Verification runs under Store together with admission.
pub(crate) struct Observation {
    revision: Option<u64>,
    model_digest: Option<String>,
    record: Option<Value>,
}
impl Observation {
    fn model_digest(model: &crate::store::ModelState) -> Result<String, String> {
        let fields = serde_json::json!({"card":model.card,"model_id":model.model_id,"port":model.port,
            "engine_type":model.engine_type,"engine_sha8":model.engine_sha8,"container_name":model.container_name,
            "image":model.image,"main_pid":model.main_pid,"managed":model.lifecycle_owned});
        let bytes = serde_jcs::to_vec(&fields).map_err(|_| "model identity encoding failed")?;
        Ok(ring::digest::digest(&ring::digest::SHA256, &bytes)
            .as_ref()
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect())
    }

    fn capture(
        state: &crate::store::State,
        card: &str,
        record: Option<Value>,
    ) -> Result<Self, ApiErr> {
        Ok(Self {
            revision: state.lifecycle_revision,
            model_digest: state
                .models
                .get(card)
                .map(Self::model_digest)
                .transpose()
                .map_err(|_| api_err(409, "state_unknown", "模型关联无法核实"))?,
            record,
        })
    }
    pub(crate) fn verify(
        &self,
        app: &App,
        state: &crate::store::State,
        card: &str,
    ) -> Result<(), ApiErr> {
        let current = Self::capture(state, card, read(app, card)?)?;
        if current.revision != self.revision
            || current.model_digest != self.model_digest
            || current.record != self.record
        {
            return Err(api_err(
                409,
                "precondition_changed",
                "核实运行时期间设备、模型关联或维护事务变化，请刷新并重新核实",
            ));
        }
        Ok(())
    }
}

fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id != "."
        && id != ".."
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_.".contains(&b))
}

fn path(app: &App, card: &str) -> Result<PathBuf, ApiErr> {
    if !valid_id(card) {
        return Err(api_err(400, "invalid_card", "模型标识不合法"));
    }
    Ok(app
        .config
        .state_dir
        .join("model-maintenance")
        .join(format!("{card}.json")))
}

fn read(app: &App, card: &str) -> Result<Option<Value>, ApiErr> {
    let path = path(app, card)?;
    let Some(bytes) = crate::private_storage::read_control(&path, 16 * 1024)
        .map_err(|message| api_err(409, "maintenance_unreadable", &message))?
    else {
        return Ok(None);
    };
    let text = std::str::from_utf8(&bytes)
        .map_err(|_| api_err(409, "maintenance_corrupt", "模型维护记录UTF-8损坏"))?;
    let record =
        Value::parse(text).map_err(|_| api_err(409, "maintenance_corrupt", "模型维护记录损坏"))?;
    if record.get("card").and_then(Value::as_str) != Some(card)
        || record.get("owner").and_then(Value::as_str).is_none()
        || record.get("uid").and_then(Value::as_u64).is_none()
        || !matches!(
            record.get("phase").and_then(Value::as_str),
            Some("active" | "recovery_required")
        )
    {
        return Err(api_err(
            409,
            "maintenance_corrupt",
            "模型维护记录字段不完整",
        ));
    }
    Ok(Some(record))
}

fn holder_alive(record: &Value) -> bool {
    let pid = record
        .get("pid")
        .and_then(Value::as_u64)
        .and_then(|n| u32::try_from(n).ok());
    record.get("boot_id").and_then(Value::as_str) == Some(peer::boot_id().as_str())
        && pid.and_then(peer::start_ticks).as_deref()
            == record.get("start_ticks").and_then(Value::as_str)
        && record.get("start_ticks").and_then(Value::as_str).is_some()
}

fn write(app: &App, card: &str, record: &Value) -> Result<(), ApiErr> {
    let path = path(app, card)?;
    let parent = path.parent().unwrap();
    std::fs::create_dir_all(parent)
        .map_err(|_| api_err(500, "maintenance_persist_failed", "不能建立模型维护目录"))?;
    if std::fs::symlink_metadata(parent)
        .map_err(|e| api_err(500, "maintenance_persist_failed", &e.to_string()))?
        .file_type()
        .is_symlink()
    {
        return Err(api_err(
            409,
            "maintenance_corrupt",
            "维护目录不能是符号链接",
        ));
    }
    std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o700))
        .map_err(|_| api_err(500, "maintenance_persist_failed", "不能保护模型维护目录"))?;
    let stamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let temp = parent.join(format!(".model-{stamp}.tmp"));
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
    result.map_err(|_| api_err(500, "maintenance_persist_failed", "模型维护记录未能持久化"))
}

pub fn check_admission(
    app: &App,
    kind: &str,
    card: &str,
    owner: Option<&str>,
) -> Result<(), ApiErr> {
    if let Some(record) = read(app, card)? {
        if ["disable", "purge"].contains(&kind)
            && owner.is_some()
            && owner == record.get("owner").and_then(Value::as_str)
        {
            return Ok(());
        }
        return Err(api_err(
            409,
            "model_maintenance",
            "模型卸载或恢复事务尚未结束，不能启动、安装或改变默认；请从品悟恢复原事务",
        ));
    }
    if owner.is_some() {
        return Err(api_err(
            409,
            "maintenance_missing",
            "原模型维护事务不存在，不能按旧事务执行",
        ));
    }
    Ok(())
}

/// API private token use is also bound to the authenticated OS uid.
pub fn authorize_owner(
    app: &App,
    card: &str,
    owner: &str,
    caller: LocalPeer,
) -> Result<(), ApiErr> {
    let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let record =
        read(app, card)?.ok_or_else(|| api_err(409, "maintenance_missing", "原维护事务不存在"))?;
    if record.get("owner").and_then(Value::as_str) != Some(owner)
        || record.get("uid").and_then(Value::as_u64) != Some(caller.uid as u64)
    {
        return Err(api_err(
            409,
            "maintenance_owned",
            "维护事务与系统调用方不匹配",
        ));
    }
    Ok(())
}

pub fn any_active(app: &App) -> Result<bool, ApiErr> {
    let directory = app.config.state_dir.join("model-maintenance");
    if !crate::private_storage::control_directory(&directory)
        .map_err(|message| api_err(409, "maintenance_unreadable", &message))?
    {
        return Ok(false);
    }
    match std::fs::read_dir(directory) {
        Ok(entries) => Ok(entries.into_iter().any(|entry| match entry {
            Ok(entry) => {
                let name = entry.file_name();
                let name = name.to_string_lossy();
                !(name.starts_with(".model-") && name.ends_with(".tmp"))
            }
            Err(_) => true,
        })),
        Err(_) => Err(api_err(
            409,
            "maintenance_unreadable",
            "不能核实模型维护事务，暂不维护组件",
        )),
    }
}

pub fn view(app: &App, card: &str) -> Result<Value, ApiErr> {
    let store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let record = read(app, card)?;
    let alive = record.as_ref().is_some_and(holder_alive);
    let recovery = record.as_ref().is_some_and(|r| {
        !alive || r.get("phase").and_then(Value::as_str) == Some("recovery_required")
    });
    let mut out = Value::obj();
    out.set("card", card.into());
    out.set("maintenance", record.is_some().into());
    out.set(
        "reservation_only",
        record
            .as_ref()
            .and_then(|r| r.get("reservation_only"))
            .and_then(Value::as_bool)
            .unwrap_or(false)
            .into(),
    );
    out.set(
        "model_present",
        store.state.models.contains_key(card).into(),
    );
    out.set("holder_alive", alive.into());
    out.set("recovery_required", recovery.into());
    out.set(
        "phase",
        if recovery {
            "recovery_required"
        } else if record.is_some() {
            "active"
        } else {
            "none"
        }
        .into(),
    );
    if let Some(record) = &record {
        let task_id = record.get("last_task_id").and_then(Value::as_str);
        out.set(
            "last_task_id",
            task_id.map(Value::from).unwrap_or(Value::Null),
        );
        out.set(
            "last_action",
            record.get("last_action").cloned().unwrap_or(Value::Null),
        );
        out.set(
            "last_task_status",
            task_id
                .and_then(|id| store.state.tasks.get(id))
                .map(|task| Value::from(task.status.as_str()))
                .or_else(|| record.get("last_task_status").cloned())
                .unwrap_or(Value::Null),
        );
    }
    Ok(out)
}

/// Persist the task receipt before returning admission and starting a worker.
/// Model purge does not remove this journal or the associated task record.
pub fn record_task(
    app: &App,
    card: &str,
    owner: &str,
    task: &str,
    action: &str,
) -> Result<(), ApiErr> {
    let mut record =
        read(app, card)?.ok_or_else(|| api_err(409, "maintenance_missing", "原维护事务不存在"))?;
    if record.get("owner").and_then(Value::as_str) != Some(owner) {
        return Err(api_err(409, "maintenance_owned", "维护事务不匹配"));
    }
    record.set("last_task_id", task.into());
    record.set("last_action", action.into());
    record.set("last_task_status", "running".into());
    write(app, card, &record)
}

/// Retain the latest task outcome independently of the bounded task history.
/// Caller holds App.store; terminal only means cmsd work ended, not uninstall.
pub fn finish_task(
    app: &App,
    card: &str,
    task: &str,
    status: &str,
    recovery: bool,
) -> Result<(), ApiErr> {
    if let Some(mut record) = read(app, card)? {
        if record.get("last_task_id").and_then(Value::as_str) == Some(task) {
            record.set("last_task_status", status.into());
            if recovery {
                record.set("phase", "recovery_required".into());
            }
            write(app, card, &record)?;
        }
    }
    Ok(())
}

/// Only at daemon startup, after orphan tasks were reconciled. A receipt with
/// no state record could not have started a worker, or its terminal history was
/// pruned. An ambiguous running receipt remains fenced and requires recovery.
pub fn recover_receipts(app: &App) -> Result<(), ApiErr> {
    let store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let directory = app.config.state_dir.join("model-maintenance");
    if !crate::private_storage::control_directory(&directory)
        .map_err(|message| api_err(409, "maintenance_unreadable", &message))?
    {
        return Ok(());
    }
    let entries = std::fs::read_dir(directory)
        .map_err(|_| api_err(409, "maintenance_unreadable", "维护目录不可读"))?;
    for entry in entries {
        let entry = entry.map_err(|_| api_err(409, "maintenance_unreadable", "维护记录不可读"))?;
        let name = entry.file_name();
        let Some(card) = name.to_str().and_then(|s| s.strip_suffix(".json")) else {
            continue;
        };
        let Some(record) = read(app, card)? else {
            continue;
        };
        if let Some(id) = record.get("last_task_id").and_then(Value::as_str) {
            if let Some(task) = store.state.tasks.get(id) {
                finish_task(
                    app,
                    card,
                    id,
                    task.status.as_str(),
                    !task.status.is_terminal() || task.status == crate::store::TaskStatus::Failed,
                )?;
            } else if record.get("last_task_status").and_then(Value::as_str) == Some("running") {
                finish_task(app, card, id, "failed", true)?;
            }
        }
    }
    Ok(())
}

/// Reuse uninstall's backend, process and listener proof before ordinary purge
/// removes weights or forgets ownership. Status caches cannot release resources.
pub(crate) fn require_runtime_quiescent(app: &App, card: &str) -> Result<(), ApiErr> {
    let reference = {
        let store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let model = store
            .state
            .models
            .get(card)
            .ok_or_else(|| api_err(409, "state_unknown", "原模型登记缺失，保留资源以便核实"))?;
        runtime::reference(app, card, Some(model))?
    };
    runtime::stopped(app, &reference, true)
}

/// Private-owner purge only retires registration; file deletion belongs to the
/// store's explicit keep/delete choice. Reservation-only disable proves absence
/// or quiescence and never calls an external lifecycle mutation.
pub fn scoped_action(
    app: &App,
    card: &str,
    owner: &str,
    kind: &str,
) -> Result<Option<String>, ApiErr> {
    let (observation, reference) = {
        let store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let record = read(app, card)?
            .ok_or_else(|| api_err(409, "maintenance_missing", "原维护事务不存在"))?;
        if record.get("owner").and_then(Value::as_str) != Some(owner) {
            return Err(api_err(409, "maintenance_owned", "维护事务不匹配"));
        }
        let reservation_only =
            record.get("reservation_only").and_then(Value::as_bool) == Some(true);
        if kind != "purge" && !reservation_only {
            return Ok(None);
        }
        if !["purge", "disable"].contains(&kind) {
            return Err(api_err(400, "invalid_action", "维护动作不支持"));
        }
        let reference = if let Some(reference) = record.get("runtime") {
            reference.clone()
        } else {
            runtime::reference(app, card, store.state.models.get(card))?
        };
        (
            Observation::capture(&store.state, card, Some(record))?,
            reference,
        )
    };
    runtime::stopped(app, &reference, true)?;
    let id = app.task_create_maintenance(kind, card, owner, &observation)?;
    if kind == "purge" {
        if let Err(message) = crate::store_adoption::runtime::retire_native_unit(app, card) {
            app.task_finish(&id, Err(message.clone()));
            return Err(api_err(500, "native_journal_retire_failed", &message));
        }
        let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        store.state.models.remove(card);
        store.state.ports.retain(|_, value| value != card);
        if store.state.active_model.as_deref() == Some(card) {
            store.state.active_model = None;
        }
        store.mark_dirty();
    }
    app.task_finish(
        &id,
        Ok(if kind == "purge" {
            "已撤管理记录；未删除权重或运行时，文件由模型商店按用户选择处理"
        } else {
            "已核实未托管运行时停止或不存在，未执行启停"
        }
        .into()),
    );
    Ok(Some(id))
}

pub fn change(app: &App, card: &str, body: &[u8], caller: LocalPeer) -> Result<Value, ApiErr> {
    if body.len() > 8192 {
        return Err(api_err(400, "invalid_json", "维护请求过大"));
    }
    let request = Value::parse(
        std::str::from_utf8(body).map_err(|_| api_err(400, "invalid_json", "维护请求不是UTF-8"))?,
    )
    .map_err(|_| api_err(400, "invalid_json", "维护请求格式不合法"))?;
    let owner = request.get("owner").and_then(Value::as_str).unwrap_or("");
    if owner.is_empty()
        || owner.len() > 128
        || !owner
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b))
    {
        return Err(api_err(
            400,
            "invalid_maintenance_owner",
            "维护事务标识不合法",
        ));
    }
    let action = request.get("action").and_then(Value::as_str).unwrap_or("");
    if !["begin", "end", "failed"].contains(&action) {
        return Err(api_err(400, "invalid_action", "模型维护动作不支持"));
    }
    let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let existing = read(app, card)?;
    if let Some(record) = &existing {
        if record.get("owner").and_then(Value::as_str) != Some(owner)
            || record.get("uid").and_then(Value::as_u64) != Some(caller.uid as u64)
        {
            return Err(api_err(
                409,
                "maintenance_owned",
                "模型维护事务属于其他调用方，请恢复原事务",
            ));
        }
    }
    if action == "begin" && existing.is_none() {
        crate::component::check_admission(app)?;
        let model = store.state.models.get(card).cloned();
        let observation = Observation::capture(&store.state, card, None)?;
        // The caller holds its store writer lock; do not recursively invoke
        // the reader/shared lock here. This cached positive current receipt is
        // rechecked with the same state observation before publishing the gate.
        let cleanup_only = crate::store_adoption::selection::cached(app)
            .and_then(|selection| selection.maintenance_target(card))
            .map_err(|error| api_err(409, "store_cleanup_unconfirmed", &error))?;
        let reservation_only = cleanup_only || !model.as_ref().is_some_and(|m| m.lifecycle_owned);
        drop(store);
        let reference = runtime::reference(app, card, model.as_ref())?;
        if reservation_only {
            runtime::stopped(app, &reference, true)?;
        }
        store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        observation.verify(app, &store.state, card)?;
        let still_cleanup_only = crate::store_adoption::selection::cached(app)
            .and_then(|selection| selection.maintenance_target(card))
            .map_err(|error| api_err(409, "store_cleanup_unconfirmed", &error))?;
        if still_cleanup_only != cleanup_only {
            return Err(api_err(
                409,
                "precondition_changed",
                "当前清理身份已变化，维护门未发布",
            ));
        }
        crate::component::check_admission(app)?;
        if store
            .state
            .tasks
            .values()
            .any(|t| !t.status.is_terminal() && (t.card == card || t.kind != "install"))
        {
            return Err(api_err(
                409,
                "task_in_progress",
                "模型或设备变更任务仍在进行",
            ));
        }
        let mut record = Value::obj();
        record.set(
            "generation",
            std::fs::read_to_string("/proc/sys/kernel/random/uuid")
                .map_err(|_| api_err(500, "maintenance_persist_failed", "不能分配维护代际"))?
                .trim()
                .into(),
        );
        record.set("reservation_only", reservation_only.into());
        record.set("runtime", reference);
        record.set("owner", owner.into());
        record.set("card", card.into());
        record.set("phase", "active".into());
        record.set("uid", (caller.uid as u64).into());
        record.set("pid", (caller.pid as u64).into());
        record.set("boot_id", peer::boot_id().into());
        record.set(
            "start_ticks",
            peer::start_ticks(caller.pid).unwrap_or_default().into(),
        );
        record.set("created_at", crate::timeutil::now_epoch().into());
        write(app, card, &record)?;
    } else if action == "failed" {
        let mut record =
            existing.ok_or_else(|| api_err(409, "maintenance_missing", "原维护事务不存在"))?;
        record.set("phase", "recovery_required".into());
        write(app, card, &record)?;
    } else if action == "end" && existing.is_some() {
        if store
            .state
            .tasks
            .values()
            .any(|t| t.card == card && !t.status.is_terminal())
        {
            return Err(api_err(
                409,
                "task_in_progress",
                "原模型任务还未终结，不能结束维护",
            ));
        }
        let needs_recovery = existing.as_ref().is_some_and(|r| {
            !holder_alive(r) || r.get("phase").and_then(Value::as_str) == Some("recovery_required")
        });
        let observation = Observation::capture(&store.state, card, existing.clone())?;
        let model = store.state.models.get(card).cloned();
        drop(store);
        if let Some(reference) = existing.as_ref().and_then(|r| r.get("runtime")) {
            runtime::stopped(app, reference, true)?;
        }
        if needs_recovery {
            if let Some(model) = &model {
                let current = runtime::reference(app, card, Some(model))?;
                runtime::stopped(app, &current, true)?;
            }
        }
        store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        observation.verify(app, &store.state, card)?;
        store.save_if_dirty().map_err(|e| {
            api_err(
                500,
                "maintenance_release_failed",
                &format!("状态尚未持久化，维护门保留：{e}"),
            )
        })?;
        let path = path(app, card)?;
        std::fs::remove_file(&path)
            .and_then(|_| std::fs::File::open(path.parent().unwrap())?.sync_all())
            .map_err(|_| api_err(500, "maintenance_release_failed", "模型维护门未能持久解除"))?;
    }
    drop(store);
    view(app, card)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::docker::fixtures as fx;
    use std::sync::{Arc, Barrier};

    #[test]
    fn unknown_maintenance_directories_never_become_empty_reservations() {
        for kind in ["dangling", "link", "fifo", "file", "writable"] {
            let root = fx::tmpdir(&format!("maintenance-directory-{kind}"));
            let app = fx::make_app(&root, 128, 100);
            crate::private_storage::secure_state(&app.config.state_dir).unwrap();
            let directory = app.config.state_dir.join("model-maintenance");
            let outside = root.join("outside");
            std::fs::create_dir(&outside).unwrap();
            match kind {
                "dangling" => std::os::unix::fs::symlink(root.join("missing"), &directory).unwrap(),
                "link" => std::os::unix::fs::symlink(&outside, &directory).unwrap(),
                "fifo" => {
                    use std::os::unix::ffi::OsStrExt;
                    let name = std::ffi::CString::new(directory.as_os_str().as_bytes()).unwrap();
                    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
                }
                "writable" => {
                    std::fs::create_dir(&directory).unwrap();
                    std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o777))
                        .unwrap();
                }
                _ => std::fs::write(&directory, b"preserve").unwrap(),
            }
            assert!(read(&app, "card-a").is_err(), "{kind}");
            assert!(any_active(&app).is_err(), "{kind}");
            assert!(recover_receipts(&app).is_err(), "{kind}");
            assert!(
                crate::component::change(&app, &request("begin", "component-a")).is_err(),
                "{kind}"
            );
            assert!(app.store.try_lock().is_ok());
            assert!(std::fs::symlink_metadata(directory).is_ok());
            assert!(std::fs::read_dir(outside).unwrap().next().is_none());
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn special_or_linked_reservation_receipts_stay_fenced_without_blocking_store() {
        for kind in ["fifo", "dangling"] {
            let root = fx::tmpdir(&format!("maintenance-receipt-{kind}"));
            let app = fx::make_app(&root, 128, 100);
            let directory = app.config.state_dir.join("model-maintenance");
            crate::private_storage::directory(&directory, true).unwrap();
            let path = directory.join("card-a.json");
            if kind == "fifo" {
                use std::os::unix::ffi::OsStrExt;
                let name = std::ffi::CString::new(path.as_os_str().as_bytes()).unwrap();
                assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
            } else {
                std::os::unix::fs::symlink(root.join("missing"), &path).unwrap();
            }
            assert!(view(&app, "card-a").is_err());
            assert!(check_admission(&app, "enable", "card-a", None).is_err());
            assert!(recover_receipts(&app).is_err());
            assert!(any_active(&app).unwrap());
            assert!(app.store.try_lock().is_ok());
            assert!(std::fs::symlink_metadata(path).is_ok());
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn backend_proof_is_invalidated_by_revision_binding_or_gate_generation() {
        let root = fx::tmpdir("maintenance-observation");
        let app = fx::make_app(&root, 128, 100);
        let observation = {
            let store = app.store.lock().unwrap();
            Observation::capture(&store.state, "card-a", None).unwrap()
        };
        {
            let mut store = app.store.lock().unwrap();
            assert!(observation.verify(&app, &store.state, "card-a").is_ok());
            store.state.lifecycle_revision = Some(1);
            assert_eq!(
                observation
                    .verify(&app, &store.state, "card-a")
                    .unwrap_err()
                    .1,
                "precondition_changed"
            );
            store.state.lifecycle_revision = None;
        }
        let mut record = Value::obj();
        record.set("owner", "private-owner".into());
        record.set("card", "card-a".into());
        record.set("phase", "active".into());
        record.set("uid", (LocalPeer::current().uid as u64).into());
        record.set("generation", "generation-a".into());
        write(&app, "card-a", &record).unwrap();
        let snapshot = {
            let store = app.store.lock().unwrap();
            Observation::capture(&store.state, "card-a", Some(record.clone())).unwrap()
        };
        record.set("generation", "generation-b".into());
        write(&app, "card-a", &record).unwrap();
        let store = app.store.lock().unwrap();
        assert_eq!(
            snapshot.verify(&app, &store.state, "card-a").unwrap_err().1,
            "precondition_changed"
        );
        drop(store);
        std::fs::remove_dir_all(root).unwrap();
        let (root, app) = fixture("maintenance-model-binding");
        let mut store = app.store.lock().unwrap();
        let observation = Observation::capture(&store.state, "card-a", None).unwrap();
        store.state.models.get_mut("card-a").unwrap().container_name =
            Some("different-runtime".into());
        assert_eq!(
            observation
                .verify(&app, &store.state, "card-a")
                .unwrap_err()
                .1,
            "precondition_changed"
        );
        drop(store);
        std::fs::remove_dir_all(root).unwrap();
    }

    fn fixture(tag: &str) -> (PathBuf, Arc<App>) {
        let root = fx::tmpdir(tag);
        let app = fx::make_app(&root, 128, 100);
        fx::write_docker_card(&app.config.cards_dir, "card-a", "qa-box", 18081, None);
        fx::set_container(
            &root,
            "qa-box",
            "fixture/img:1.0",
            false,
            "no",
            0,
            false,
            0,
            18081,
            8000,
            "",
        );
        let card = crate::ops::find_card(&app, "card-a").unwrap();
        let container = app.docker.inspect("qa-box").unwrap().unwrap();
        crate::ops::register_container_managed(&app, &card, &container);
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("card-a")
            .unwrap()
            .lifecycle_owned = true;
        (root, app)
    }
    fn request(action: &str, owner: &str) -> Vec<u8> {
        format!("{{\"action\":\"{action}\",\"owner\":\"{owner}\"}}").into_bytes()
    }
    fn change_now(app: &App, action: &str, owner: &str) -> Result<Value, ApiErr> {
        change(app, "card-a", &request(action, owner), LocalPeer::current())
    }

    #[test]
    fn durable_receipt_survives_bounded_history_and_restart() {
        let (_root, app) = fixture("maintenance-receipt");
        change_now(&app, "begin", "owner-a").unwrap();
        let task = app
            .task_create_exclusive_with_maintenance("disable", "card-a", Some("owner-a"))
            .unwrap();
        app.task_finish(&task, Ok("stopped".into()));
        {
            let mut store = app.store.lock().unwrap();
            store.state.tasks.get_mut(&task).unwrap().created_at = 0;
        }
        for _ in 0..110 {
            let id = app.task_create("test", "other-card");
            app.task_finish(&id, Ok("done".into()));
        }
        assert!(!app.store.lock().unwrap().state.tasks.contains_key(&task));
        let systemd = crate::systemd::Systemd::new(
            app.config.systemd_mode,
            app.config.unit_dir.clone(),
            app.config.env_dir(),
            "test".into(),
        );
        let recovered = App::new(
            app.config.clone(),
            crate::store::StateStore::try_load(&app.config.state_file()).unwrap(),
            systemd,
        );
        recover_receipts(&recovered).unwrap();
        assert_eq!(
            view(&recovered, "card-a")
                .unwrap()
                .get("last_task_status")
                .and_then(Value::as_str),
            Some("succeeded")
        );
        change_now(&recovered, "end", "owner-a").unwrap();
    }

    #[test]
    fn persistence_failure_does_not_leave_an_unstarted_running_task() {
        let (_root, app) = fixture("maintenance-state-write-failed");
        crate::lifecycle_revision::initialize(&app).unwrap();
        change_now(&app, "begin", "owner-a").unwrap();
        std::fs::remove_file(app.config.state_file()).unwrap();
        std::fs::create_dir(app.config.state_file()).unwrap();
        assert_eq!(
            app.task_create_exclusive_with_maintenance("disable", "card-a", Some("owner-a"))
                .unwrap_err()
                .1,
            "task_persist_failed"
        );
        assert!(app.store.lock().unwrap().state.tasks.is_empty());
        let status = view(&app, "card-a").unwrap();
        assert_eq!(
            status.get("last_task_status").and_then(Value::as_str),
            Some("failed")
        );
        assert_eq!(
            status.get("recovery_required").and_then(Value::as_bool),
            Some(true)
        );
        assert_eq!(
            change_now(&app, "end", "owner-a").unwrap_err().1,
            "maintenance_release_failed"
        );
        assert_eq!(
            view(&app, "card-a")
                .unwrap()
                .get("maintenance")
                .and_then(Value::as_bool),
            Some(true)
        );
        std::fs::remove_dir(app.config.state_file()).unwrap();
        change_now(&app, "end", "owner-a").unwrap();
    }

    #[test]
    fn private_receipt_requires_matching_os_uid() {
        let (_root, app) = fixture("maintenance-uid");
        change_now(&app, "begin", "owner-a").unwrap();
        let mut other = LocalPeer::current();
        other.uid = other.uid.wrapping_add(1);
        assert_eq!(
            authorize_owner(&app, "card-a", "owner-a", other)
                .unwrap_err()
                .1,
            "maintenance_owned"
        );
    }

    #[test]
    fn switch_cannot_use_a_previous_default_that_changed_during_preview() {
        let (_root, app) = fixture("switch-preview-generation");
        app.store.lock().unwrap().state.active_model = Some("previous-a".into());
        let previous = app.store.lock().unwrap().state.active_model.clone();
        app.store.lock().unwrap().state.active_model = Some("previous-b".into());
        assert_eq!(
            app.task_create_switch("target", previous.as_deref())
                .unwrap_err()
                .1,
            "precondition_changed"
        );
        assert!(app.store.lock().unwrap().state.tasks.is_empty());
    }

    #[test]
    fn never_enabled_card_can_reserve_uninstall_without_lifecycle_authority() {
        let (root, app) = fixture("reservation-never-enabled");
        app.store.lock().unwrap().state.models.clear();
        let began = change_now(&app, "begin", "private-owner").unwrap();
        assert_eq!(
            began.get("reservation_only").and_then(Value::as_bool),
            Some(true)
        );
        assert!(app.store.lock().unwrap().state.models.is_empty());
        assert!(crate::ops::enable(&app, "card-a").is_err());
        assert!(app.task_create_install("card-a").is_err());
        let stopped =
            crate::ops::disable_with_maintenance(&app, "card-a", Some("private-owner")).unwrap();
        assert_eq!(
            app.store.lock().unwrap().state.tasks[&stopped].status,
            crate::store::TaskStatus::Succeeded
        );
        let purged =
            crate::ops::purge_with_maintenance(&app, "card-a", Some("private-owner")).unwrap();
        assert_eq!(
            app.store.lock().unwrap().state.tasks[&purged].status,
            crate::store::TaskStatus::Succeeded
        );
        std::fs::remove_dir_all(app.config.cards_dir.join("card-a")).unwrap();
        change_now(&app, "end", "private-owner").unwrap();
        assert!(!fx::docker_calls(&root)
            .iter()
            .any(|line| ["start ", "stop ", "update ", "rm "]
                .iter()
                .any(|prefix| line.starts_with(prefix))));
    }

    #[test]
    fn scoped_purge_retires_corrupt_private_diagnostic_without_internal_repair() {
        for raw in [
            b"broken".to_vec(),
            vec![b'x'; 4097],
            br#"{"format":"cmsd-native-journal-v1","card":"other","unit":"ssh.service"}"#.to_vec(),
        ] {
            let (root, app) = fixture("maintenance-journal-retire");
            change_now(&app, "begin", "private-owner").unwrap();
            let unit = "megacube-model@card-a.service";
            crate::store_adoption::runtime::record_native_unit(&app, "card-a", Some(unit)).unwrap();
            let source = app.config.state_dir.join("native-journals/card-a.json");
            std::fs::write(&source, raw).unwrap();
            assert!(crate::store_adoption::runtime::native_unit_name(&app, "card-a").is_err());
            let id =
                crate::ops::purge_with_maintenance(&app, "card-a", Some("private-owner")).unwrap();
            assert_eq!(
                app.store.lock().unwrap().state.tasks[&id].status,
                crate::store::TaskStatus::Succeeded
            );
            assert!(!app
                .store
                .lock()
                .unwrap()
                .state
                .models
                .contains_key("card-a"));
            assert!(!source.exists());
            change_now(&app, "end", "private-owner").unwrap();
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn reservation_rejects_external_activity_autostart_and_unknown_backend() {
        let (root, app) = fixture("reservation-external");
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("card-a")
            .unwrap()
            .lifecycle_owned = false;
        for (running, policy) in [(true, "no"), (false, "always"), (false, "on-failure")] {
            fx::set_container(
                &root,
                "qa-box",
                "fixture/img:1.0",
                running,
                policy,
                1,
                false,
                if running { 9981 } else { 0 },
                18081,
                8000,
                "",
            );
            assert_eq!(
                change_now(&app, "begin", "private-owner").unwrap_err().1,
                "runtime_not_quiescent"
            );
            assert!(read(&app, "card-a").unwrap().is_none());
        }
        std::fs::rename(root.join("docker"), root.join("docker.unavailable")).unwrap();
        assert_eq!(
            change_now(&app, "begin", "private-owner").unwrap_err().1,
            "state_unknown"
        );
        assert!(read(&app, "card-a").unwrap().is_none());
    }

    #[test]
    fn external_start_during_reservation_never_gains_stop_permission() {
        let (root, app) = fixture("reservation-external-start");
        app.store.lock().unwrap().state.models.clear();
        change_now(&app, "begin", "private-owner").unwrap();
        fx::set_container(
            &root,
            "qa-box",
            "fixture/img:1.0",
            true,
            "no",
            0,
            false,
            9981,
            18081,
            8000,
            "",
        );
        assert!(
            crate::ops::disable_with_maintenance(&app, "card-a", Some("private-owner")).is_err()
        );
        assert!(crate::ops::purge_with_maintenance(&app, "card-a", Some("private-owner")).is_err());
        assert!(change_now(&app, "end", "private-owner").is_err());
        assert!(read(&app, "card-a").unwrap().is_some());
        assert!(!fx::docker_calls(&root)
            .iter()
            .any(|line| line.starts_with("stop ")));
    }

    #[test]
    fn scoped_native_purge_preserves_real_files_and_owned_metadata() {
        let root = fx::tmpdir("maintenance-native-keep");
        let base = fx::make_app(&root, 128, 100);
        let port = std::net::TcpListener::bind("127.0.0.1:0")
            .unwrap()
            .local_addr()
            .unwrap()
            .port();
        let card = base.config.cards_dir.join("card-a");
        std::fs::create_dir_all(&card).unwrap();
        let weights = root.join("weights");
        std::fs::create_dir_all(&weights).unwrap();
        std::fs::write(weights.join("model.tachyon"), b"real-file-to-preserve").unwrap();
        std::fs::write(card.join("common.sh"), "ENGINE_BIN_SHA256=0123456789\nMODEL_ID=native-test\nARTIFACT=model.tachyon\nTOTAL_BYTES=21\n").unwrap();
        std::fs::write(card.join(".deploy-env"), format!("PORT={port}\nHOST=127.0.0.1\nMODEL_DIR={}\nENGINE_DIR=/fixture-engine\nARTIFACT=model.tachyon\nMODEL_ID=native-test\nMAX_CONCURRENCY=1\n", weights.display())).unwrap();
        let cli = root.join("systemctl-test");
        std::fs::write(&cli, "#!/bin/sh\ncase \"$*\" in *is-enabled*) echo disabled; exit 1;; *show*) printf 'ActiveState=inactive\\nSubState=dead\\nMainPID=0\\nNRestarts=0\\nResult=success\\nLoadState=loaded\\n';; *) exit 0;; esac\n").unwrap();
        std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
        let mut cfg = base.config.clone();
        cfg.proc_root = root.join("proc");
        std::fs::create_dir_all(&cfg.proc_root).unwrap();
        let backend = crate::systemd::Systemd::new(
            cfg.systemd_mode,
            cfg.unit_dir.clone(),
            cfg.env_dir(),
            "test".into(),
        )
        .with_test_cli(&cli);
        let app = Arc::new(App::new(
            cfg.clone(),
            crate::store::StateStore::load(&cfg.state_file()),
            backend,
        ));
        crate::ops::register_external_managed(
            &app,
            "card-a",
            &crate::systemd::UnitState::default(),
        );
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("card-a")
            .unwrap()
            .lifecycle_owned = true;
        change_now(&app, "begin", "private-owner").unwrap();
        let id = crate::ops::purge_with_maintenance(&app, "card-a", Some("private-owner")).unwrap();
        assert_eq!(
            app.store.lock().unwrap().state.tasks[&id].status,
            crate::store::TaskStatus::Succeeded
        );
        assert!(!app
            .store
            .lock()
            .unwrap()
            .state
            .models
            .contains_key("card-a"));
        assert_eq!(
            std::fs::read(weights.join("model.tachyon")).unwrap(),
            b"real-file-to-preserve"
        );
        change_now(&app, "end", "private-owner").unwrap();
        assert!(!crate::store::StateStore::try_load(&cfg.state_file())
            .unwrap()
            .state
            .models
            .contains_key("card-a"));
    }

    #[test]
    fn gate_excludes_other_writes_and_protects_both_switch_sides() {
        let (_root, app) = fixture("model-maintenance-exclusive");
        change_now(&app, "begin", "owner-a").unwrap();
        for kind in ["enable", "disable", "purge", "set_default", "switch"] {
            assert_eq!(
                app.task_create_exclusive(kind, "card-a").unwrap_err().1,
                "model_maintenance"
            );
        }
        assert!(app.task_create_install("card-a").is_err());
        app.store.lock().unwrap().state.active_model = Some("card-a".into());
        assert!(app.task_create_exclusive("switch", "another-card").is_err());
        assert!(app
            .task_create_exclusive_with_maintenance("disable", "card-a", Some("wrong"))
            .is_err());
        let task = app
            .task_create_exclusive_with_maintenance("disable", "card-a", Some("owner-a"))
            .unwrap();
        assert!(change_now(&app, "begin", "owner-a").is_ok());
        assert_eq!(
            change_now(&app, "end", "owner-a").unwrap_err().1,
            "task_in_progress"
        );
        let public = view(&app, "card-a").unwrap().to_string();
        assert!(!public.contains("owner-a"));
        assert!(public.contains(&task));
        app.task_finish(&task, Ok("stopped".into()));
        app.store.lock().unwrap().state.models.remove("card-a");
        assert!(change_now(&app, "begin", "owner-a").is_ok());
        assert!(change_now(&app, "failed", "owner-a").is_ok());
        change_now(&app, "end", "owner-a").unwrap();
        change_now(&app, "end", "owner-a").unwrap();
    }

    #[test]
    fn gate_and_component_maintenance_are_mutually_exclusive() {
        let (_root, app) = fixture("model-maintenance-component");
        change_now(&app, "begin", "owner-a").unwrap();
        assert_eq!(
            crate::component::change(&app, &request("begin", "component-a"))
                .unwrap_err()
                .1,
            "model_maintenance"
        );
        change_now(&app, "end", "owner-a").unwrap();
        crate::component::change(&app, &request("begin", "component-a")).unwrap();
        assert_eq!(
            change_now(&app, "begin", "owner-a").unwrap_err().1,
            "component_maintenance"
        );
    }

    #[test]
    fn wrong_owner_and_running_service_cannot_release_recovery() {
        let (root, app) = fixture("model-maintenance-recovery");
        change_now(&app, "begin", "private-owner").unwrap();
        assert_eq!(
            std::fs::metadata(path(&app, "card-a").unwrap())
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
        assert_eq!(
            change_now(&app, "end", "someone-else").unwrap_err().1,
            "maintenance_owned"
        );
        change_now(&app, "failed", "private-owner").unwrap();
        fx::set_container(
            &root,
            "qa-box",
            "fixture/img:1.0",
            true,
            "no",
            0,
            false,
            123,
            18081,
            8000,
            "",
        );
        assert_eq!(
            change_now(&app, "end", "private-owner").unwrap_err().1,
            "runtime_not_quiescent"
        );
        fx::set_container(
            &root,
            "qa-box",
            "fixture/img:1.0",
            false,
            "no",
            0,
            false,
            0,
            18081,
            8000,
            "",
        );
        change_now(&app, "end", "private-owner").unwrap();
        std::fs::write(path(&app, "card-a").unwrap(), "corrupt").unwrap();
        assert!(app.task_create_exclusive("enable", "card-a").is_err());
        assert!(change_now(&app, "end", "private-owner").is_err());
    }

    #[test]
    fn concurrent_owners_cannot_both_acquire_maintenance() {
        let (_root, app) = fixture("model-maintenance-race");
        let barrier = Arc::new(Barrier::new(32));
        let winners = std::thread::scope(|scope| {
            let handles: Vec<_> = (0..32)
                .map(|i| {
                    let app = app.clone();
                    let barrier = barrier.clone();
                    scope.spawn(move || {
                        barrier.wait();
                        change_now(&app, "begin", &format!("owner-{i}")).is_ok()
                    })
                })
                .collect();
            handles
                .into_iter()
                .map(|h| h.join().unwrap())
                .filter(|won| *won)
                .count()
        });
        assert_eq!(winners, 1);
    }

    #[test]
    fn public_status_detects_dead_holder_without_releasing_its_gate() {
        let (_root, app) = fixture("model-maintenance-dead-holder");
        change(
            &app,
            "card-a",
            &request("begin", "owner-a"),
            LocalPeer {
                pid: u32::MAX,
                uid: LocalPeer::current().uid,
            },
        )
        .unwrap();
        let public = view(&app, "card-a").unwrap();
        assert_eq!(
            public.get("recovery_required").and_then(Value::as_bool),
            Some(true)
        );
        assert!(check_admission(&app, "enable", "card-a", None).is_err());
    }

    #[test]
    fn cleanup_current_owned_download_uses_only_reservation_and_scoped_actions() {
        let (root, app) = fixture("cleanup-current-owned-download");
        let inventory = serde_json::json!({"selection_policy":"latest-installed-v1", "selections":[{"logical_card":"logical","card":"card-a","version_id":"current"}], "models":[], "archived_models":[{"logical_card":"logical","card":"old-card","version_id":"old"}], "skipped":[{"card":"logical","reason":"current_not_deployed"}]});
        crate::store_adoption::selection::publish(&app, &inventory).unwrap();
        assert!(crate::ops::enable(&app, "card-a").is_err());
        assert!(crate::ops::set_default(&app, "card-a").is_err());
        assert!(crate::recovery::set_boot(&app, "card-a", true).is_err());
        let began = change_now(&app, "begin", "cleanup-owner").unwrap();
        assert_eq!(
            began.get("reservation_only").and_then(Value::as_bool),
            Some(true)
        );
        let id =
            crate::ops::disable_with_maintenance(&app, "card-a", Some("cleanup-owner")).unwrap();
        assert_eq!(
            app.store.lock().unwrap().state.tasks[&id].status,
            crate::store::TaskStatus::Succeeded
        );
        let id = crate::ops::purge_with_maintenance(&app, "card-a", Some("cleanup-owner")).unwrap();
        assert_eq!(
            app.store.lock().unwrap().state.tasks[&id].status,
            crate::store::TaskStatus::Succeeded
        );
        change_now(&app, "end", "cleanup-owner").unwrap();
        assert!(!path(&app, "card-a").unwrap().exists());
        assert!(!fx::docker_calls(&root)
            .iter()
            .any(|line| ["start ", "stop ", "update ", "rm "]
                .iter()
                .any(|action| line.starts_with(action))));
    }

    #[test]
    fn cleanup_current_unknown_or_archived_download_never_gets_a_gate() {
        for reason in [
            "store_deletion_incomplete",
            "store_inventory_current_version_mismatch",
        ] {
            let (_root, app) = fixture(&format!("cleanup-current-{reason}"));
            let inventory = serde_json::json!({"selection_policy":"latest-installed-v1", "selections":[{"logical_card":"logical","card":"card-a","version_id":"current"}], "models":[], "archived_models":[], "skipped":[{"card":"logical","reason":reason}]});
            crate::store_adoption::selection::publish(&app, &inventory).unwrap();
            assert!(change_now(&app, "begin", "cleanup-owner").is_err());
            assert!(!path(&app, "card-a").unwrap().exists());
        }
        let (_root, app) = fixture("cleanup-current-archived-download");
        let inventory = serde_json::json!({"selection_policy":"latest-installed-v1", "selections":[], "models":[], "archived_models":[{"logical_card":"logical","card":"card-a","version_id":"old"}]});
        crate::store_adoption::selection::publish(&app, &inventory).unwrap();
        assert!(change_now(&app, "begin", "cleanup-owner").is_err());
        assert!(!path(&app, "card-a").unwrap().exists());
    }

    #[test]
    fn cleanup_current_active_owned_runtime_can_stop_before_reserving() {
        use std::os::unix::fs::PermissionsExt;
        let (root, mut app) = fixture("cleanup-current-active-owned");
        let mut inventory = serde_json::json!({"format":"pinvou-installed-models-v1", "cards_root":app.config.cards_dir, "selection_policy":"latest-installed-v1", "selections":[{"logical_card":"logical","card":"card-a","version_id":"current"}], "models":[], "archived_models":[], "skipped":[{"card":"logical","reason":"current_not_deployed"}]});
        let profile = root.join("profile");
        std::fs::create_dir_all(profile.join("model-store")).unwrap();
        let program = root.join("reader");
        std::fs::write(
            &program,
            format!("#!/bin/sh\ncat <<'INVENTORY'\n{inventory}\nINVENTORY\n"),
        )
        .unwrap();
        std::fs::set_permissions(&program, std::fs::Permissions::from_mode(0o700)).unwrap();
        let config = &mut Arc::get_mut(&mut app).unwrap().config;
        config.store_inventory_program = Some(program);
        config.store_inventory_profile = Some(profile);
        crate::store_adoption::selection::publish(&app, &inventory).unwrap();
        fx::set_container(
            &root,
            "qa-box",
            "fixture/img:1.0",
            true,
            "no",
            0,
            false,
            9981,
            18081,
            8000,
            "",
        );
        let card = crate::ops::find_card(&app, "card-a").unwrap();
        let container = app.docker.inspect("qa-box").unwrap().unwrap();
        crate::ops::register_container_managed(&app, &card, &container);
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("card-a")
            .unwrap()
            .lifecycle_owned = true;
        assert_eq!(
            change_now(&app, "begin", "cleanup-owner").unwrap_err().1,
            "runtime_not_quiescent"
        );
        assert!(read(&app, "card-a").unwrap().is_none());
        let id = crate::ops::disable(&app, "card-a").unwrap();
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        loop {
            let status = app.store.lock().unwrap().state.tasks[&id].status;
            if status.is_terminal() {
                assert_eq!(status, crate::store::TaskStatus::Succeeded);
                break;
            }
            assert!(std::time::Instant::now() < deadline);
            std::thread::sleep(std::time::Duration::from_millis(10));
        }
        assert_eq!(
            change_now(&app, "begin", "cleanup-owner")
                .unwrap()
                .get("reservation_only")
                .and_then(Value::as_bool),
            Some(true)
        );
        change_now(&app, "end", "cleanup-owner").unwrap();
        app.store
            .lock()
            .unwrap()
            .state
            .models
            .get_mut("card-a")
            .unwrap()
            .lifecycle_owned = false;
        assert!(
            crate::ops::disable(&app, "card-a").is_err(),
            "cleanup identity never grants unowned stop permission"
        );
        inventory["selections"] = serde_json::json!([]);
        inventory["skipped"] = serde_json::json!([]);
        inventory["archived_models"] =
            serde_json::json!([{"logical_card":"logical","card":"card-a","version_id":"old"}]);
        crate::store_adoption::selection::publish(&app, &inventory).unwrap();
        assert!(crate::store_adoption::selection::cached(&app)
            .unwrap()
            .resolve("card-a")
            .is_err());
    }
    #[test]
    fn ordinary_public_purge_cleans_corrupt_private_journal_after_public_disable() {
        for raw in [
            b"broken".to_vec(),
            vec![b'x'; 4097],
            br#"{"format":"cmsd-native-journal-v1","card":"other","unit":"ssh.service"}"#.to_vec(),
        ] {
            let (root, app) = fixture("ordinary-purge-private-journal");
            crate::store_adoption::runtime::record_native_unit(
                &app,
                "card-a",
                Some("megacube-model@card-a.service"),
            )
            .unwrap();
            let source = app.config.state_dir.join("native-journals/card-a.json");
            std::fs::write(&source, raw).unwrap();
            assert!(crate::store_adoption::runtime::native_unit_name(&app, "card-a").is_err());
            let disable = crate::ops::disable(&app, "card-a").unwrap();
            assert_eq!(
                fx::wait_terminal(&app, &disable, 5).status,
                crate::store::TaskStatus::Succeeded
            );
            let purge = crate::ops::purge(&app, "card-a").unwrap();
            let purged = fx::wait_terminal(&app, &purge, 5);
            assert_eq!(
                purged.status,
                crate::store::TaskStatus::Succeeded,
                "{:?}",
                purged.error
            );
            assert!(!app
                .store
                .lock()
                .unwrap()
                .state
                .models
                .contains_key("card-a"));
            assert!(!source.exists());
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn scoped_purge_preserves_registration_and_target_for_untrusted_diagnostic_paths() {
        for linked in [true, false] {
            let (root, app) = fixture("scoped-purge-untrusted-journal");
            change_now(&app, "begin", "private-owner").unwrap();
            crate::store_adoption::runtime::record_native_unit(&app, "card-a", None).unwrap();
            let source = app.config.state_dir.join("native-journals/card-a.json");
            let target = root.join("original");
            std::fs::write(&target, b"preserve").unwrap();
            if linked {
                std::fs::remove_file(&source).unwrap();
                std::os::unix::fs::symlink(&target, &source).unwrap();
            } else {
                std::fs::set_permissions(&source, std::fs::Permissions::from_mode(0o644)).unwrap();
            }
            let error = crate::ops::purge_with_maintenance(&app, "card-a", Some("private-owner"))
                .unwrap_err();
            assert_eq!(error.1, "native_journal_retire_failed");
            assert!(app
                .store
                .lock()
                .unwrap()
                .state
                .models
                .contains_key("card-a"));
            assert_eq!(std::fs::read(&target).unwrap(), b"preserve");
            assert!(std::fs::symlink_metadata(&source).is_ok());
            assert_eq!(
                view(&app, "card-a")
                    .unwrap()
                    .get("last_task_status")
                    .and_then(Value::as_str),
                Some("failed")
            );
            std::fs::remove_dir_all(root).unwrap();
        }
    }
}
