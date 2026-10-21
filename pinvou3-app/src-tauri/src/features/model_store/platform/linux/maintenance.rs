//! Durable cross-process ownership of a managed model's uninstall transaction.
//! Public summaries contain no maintenance credential; recovery never reruns a script.
use std::fs::{self, File};
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::execution_fence::{ExecutionFence, open_lock, private_write};
use super::{config, script_runner::RunControl};
use crate::features::megacube::{Client, delegate, socket_path};

#[derive(Serialize, Deserialize)]
struct Record {
    card_id: String,
    #[serde(default)]
    runtime_card_id: Option<String>,
    card_name: String,
    transaction_id: String,
    owner: String,
    socket: PathBuf,
    run_id: Option<String>,
    purge_weights: bool,
    #[serde(default)]
    reservation_protocol: bool,
    #[serde(default)]
    reservation_only: bool,
    phase: String,
    stage: String,
    tasks: Vec<String>,
    fence: Option<PathBuf>,
    created_at: u64,
    updated_at: u64,
    error: Option<String>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Summary {
    pub card_id: String,
    pub card_name: String,
    pub phase: String,
    pub stage: String,
    pub recovery_required: bool,
    pub worker_active: bool,
    pub purge_weights: bool,
    pub reservation_only: bool,
    pub error: Option<String>,
    pub error_truncated: bool,
}

fn root() -> PathBuf {
    config::data_dir().join("maintenance")
}
fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}
fn path(card: &str) -> Result<PathBuf, String> {
    Ok(root().join(format!("{}.json", config::safe_path_component(card)?)))
}
fn lock(card: &str) -> Result<File, String> {
    let file = open_lock(&root().join(format!("{}.lock", config::safe_path_component(card)?)))?;
    file.try_lock()
        .map_err(|_| "maintenance_worker_active: 原卸载或恢复进程仍在工作".to_string())?;
    Ok(file)
}
fn load(card: &str) -> Result<Option<Record>, String> {
    let path = path(card)?;
    let Some(bytes) = super::control_file::read_optional(&path, 16 * 1024 * 1024)
        .map_err(|error| format!("maintenance_record_unsafe: {error}"))?
    else {
        return Ok(None);
    };
    serde_json::from_slice(&bytes)
        .map(Some)
        .map_err(|_| "maintenance_record_corrupt: 维护记录损坏，请从备份恢复，不自动解锁".into())
}
/// Terminal proof ignores the unbounded immutable log array, but still reads
/// the original journal through one trusted, non-blocking regular descriptor.
fn original_task_finished(path: &Path) -> Result<bool, String> {
    #[derive(Deserialize)]
    struct OriginalRun {
        outcome: Option<Value>,
    }
    let file = super::control_file::open_optional(path, None)
        .map_err(|error| format!("maintenance_original_task_unknown: {error}"))?
        .ok_or("maintenance_original_task_unknown")?;
    let original: OriginalRun = serde_json::from_reader(std::io::BufReader::new(file))
        .map_err(|_| "maintenance_original_task_unknown")?;
    Ok(original.outcome.is_some())
}

fn save(record: &mut Record) -> Result<(), String> {
    record.updated_at = now();
    private_write(
        &path(&record.card_id)?,
        &serde_json::to_vec(record).map_err(|e| e.to_string())?,
    )
}
fn checked(value: Value) -> Result<Value, String> {
    if let Some(error) = value.get("error").filter(|error| !error.is_null()) {
        return Err(error
            .get("message")
            .and_then(Value::as_str)
            .or_else(|| error.as_str())
            .unwrap_or("设备维护操作失败")
            .to_string());
    }
    if value
        .get("http_status")
        .and_then(Value::as_u64)
        .is_some_and(|s| s >= 400)
    {
        return Err("maintenance_invalid_response".into());
    }
    Ok(value)
}

fn task_blocks_release(task: &Value, card: &str, current_store_run: Option<&str>) -> bool {
    if task.get("card").and_then(Value::as_str) != Some(card) {
        return false;
    }
    if matches!(
        task.get("status").and_then(Value::as_str),
        Some("succeeded" | "failed" | "cancelled" | "interrupted")
    ) {
        return false;
    }
    // cmsd merges its device tasks with the store's tasks. This call is itself
    // a running store task until release returns; exempt only that exact run.
    !(task.get("source").and_then(Value::as_str) == Some("store")
        && current_store_run.is_some()
        && task.get("id").and_then(Value::as_str) == current_store_run
        && matches!(
            task.get("status").and_then(Value::as_str),
            Some("running" | "queued" | "cancelling")
        ))
}

pub fn ensure_clear(card: &str) -> Result<(), String> {
    if load(card)?.is_some_and(|r| r.phase != "completed") {
        return Err(
            "maintenance_recovery_required: 卸载事务未结束，请先在模型商店核实并恢复维护".into(),
        );
    }
    Ok(())
}

pub struct Transaction {
    client: Client,
    record: Record,
    _lock: File,
}

impl Record {
    fn resource_card(&self) -> &str {
        self.runtime_card_id.as_deref().unwrap_or(&self.card_id)
    }

    fn task_blocks_release(&self, task: &Value, current_store_run: Option<&str>) -> bool {
        // Device tasks use the physical identity; store tasks retain the logical
        // card. Either can keep this same resource busy during recovery.
        task_blocks_release(task, self.resource_card(), current_store_run)
            || task_blocks_release(task, &self.card_id, current_store_run)
    }
}

impl Transaction {
    pub fn begin(
        card: &str,
        name: &str,
        purge: bool,
        control: &RunControl,
    ) -> Result<Option<Self>, String> {
        if control.cancelled() {
            return Err("已取消，未开始维护事务".into());
        }
        let guard = lock(card)?;
        ensure_clear(card)?;
        let Some(client) = delegate::maintenance_client(card)? else {
            return Ok(None);
        };
        let mut record = Record {
            card_id: card.to_string(),
            runtime_card_id: client
                .model_fact(card)?
                .and_then(|m| m["card"].as_str().map(str::to_owned)),
            card_name: name.to_string(),
            transaction_id: uuid::Uuid::new_v4().simple().to_string(),
            owner: uuid::Uuid::new_v4().simple().to_string(),
            socket: socket_path(),
            run_id: control.run_id.clone(),
            purge_weights: purge,
            reservation_protocol: true,
            reservation_only: false,
            phase: "active".into(),
            stage: "acquiring".into(),
            tasks: Vec::new(),
            fence: None,
            created_at: now(),
            updated_at: now(),
            error: None,
        };
        // Persist the private credential before the first remote mutation.
        save(&mut record)?;
        let mut transaction = Self {
            client,
            record,
            _lock: guard,
        };
        let value = transaction.client.model_maintenance(
            transaction.record.resource_card(),
            "begin",
            &transaction.record.owner,
        )?;
        if value.get("error").is_some() {
            let status = value
                .get("http_status")
                .and_then(Value::as_u64)
                .unwrap_or(500);
            if [400, 404, 409].contains(&status) {
                transaction.record.phase = "completed".into();
                transaction.record.stage = "not_started".into();
                save(&mut transaction.record)?;
            }
            if status == 404 {
                return Err(
                    "maintenance_unsupported: 设备服务不支持维护事务，请先更新；未执行卸载脚本"
                        .into(),
                );
            }
            return Err(checked(value)
                .err()
                .unwrap_or_else(|| "maintenance_rejected".into()));
        }
        if value.get("maintenance").and_then(Value::as_bool) != Some(true) {
            return Err("maintenance_unsupported: 设备服务未确认维护门，未执行卸载脚本".into());
        }
        transaction.record.reservation_only = value
            .get("reservation_only")
            .and_then(Value::as_bool)
            .ok_or("maintenance_mode_unknown: 无法核实清理预留模式，保留事务待恢复")?;
        transaction.stage("held")?;
        Ok(Some(transaction))
    }

    pub fn stage(&mut self, stage: &str) -> Result<(), String> {
        self.record.stage = stage.to_string();
        save(&mut self.record)
    }

    fn check_protocol(&self) -> Result<(), String> {
        if self.record.reservation_protocol
            && !self.client.supports("model.maintenance.reservation")?
        {
            return Err("maintenance_unsupported: 原事务需要清理预留协议，请先恢复兼容设备服务，不能回退旧脚本".into());
        }
        Ok(())
    }

    pub fn action(
        &mut self,
        action: &str,
        control: &RunControl,
        log: &mut dyn FnMut(&str),
    ) -> Result<(), String> {
        self.check_protocol()?;
        if control.cancelled() {
            return Err("已取消，维护保留待恢复".into());
        }
        self.stage(if action == "disable" {
            "stopping"
        } else {
            "purging"
        })?;
        let value = checked(self.client.maintained_action(
            self.record.resource_card(),
            action,
            &self.record.owner,
        )?)?;
        let task = value
            .get("task_id")
            .and_then(Value::as_str)
            .ok_or("maintenance_missing_task_id")?
            .to_string();
        self.record.tasks.push(task.clone());
        save(&mut self.record)?;
        loop {
            let status = checked(self.client.task(&task)?)?;
            match status.get("status").and_then(Value::as_str) {
                Some("succeeded") => break,
                Some("failed" | "cancelled" | "interrupted") => {
                    return Err(status
                        .get("error")
                        .and_then(Value::as_str)
                        .unwrap_or("设备维护任务未成功")
                        .to_string());
                }
                Some("running" | "queued" | "cancelling") => {
                    if control.cancelled() {
                        return Err("已取消等待；设备任务仍需核实，维护保留".into());
                    }
                    log("[cmsd] 等待维护事务中的设备任务结束…");
                    std::thread::sleep(Duration::from_millis(250));
                }
                _ => return Err("maintenance_task_unknown: 无法核实设备任务状态".into()),
            }
        }
        self.stage(if action == "disable" {
            "stopped"
        } else {
            "purged"
        })
    }

    pub fn fence(&mut self) -> Result<ExecutionFence, String> {
        let directory = root().join("executions").join(&self.record.transaction_id);
        self.record.fence = Some(directory.clone());
        self.stage("preparing_script")?;
        let fence = ExecutionFence::prepare(directory)?;
        self.stage("uninstalling")?;
        Ok(fence)
    }

    fn verify_idle(&self, current_store_run: Option<&str>) -> Result<(), String> {
        let receipt = checked(
            self.client
                .model_maintenance_state(self.record.resource_card())?,
        )?;
        if receipt
            .get("last_task_id")
            .and_then(Value::as_str)
            .is_some()
            && !matches!(
                receipt.get("last_task_status").and_then(Value::as_str),
                Some("succeeded" | "failed" | "cancelled" | "interrupted")
            )
        {
            return Err("maintenance_task_active: 原事务任务尚未确认终结".into());
        }
        let tasks = checked(self.client.tasks()?)?;
        let tasks = tasks
            .get("tasks")
            .and_then(Value::as_array)
            .ok_or("maintenance_tasks_unknown")?;
        for task in tasks {
            if self.record.task_blocks_release(task, current_store_run) {
                return Err("maintenance_task_active: 该模型仍有其他未终结任务，不能恢复".into());
            }
        }
        // A reservation is not a lifecycle grant. Declared rows may still be
        // downloaded/not_deployed. The scoped end rechecks the saved runtime
        // reference for stopped/absent and no independent restart policy.
        if self.record.reservation_only {
            return Ok(());
        }
        let models = checked(self.client.models()?)?;
        let models = models
            .get("models")
            .and_then(Value::as_array)
            .ok_or("maintenance_models_unknown")?;
        if let Some(model) = models
            .iter()
            .find(|m| m.get("card").and_then(Value::as_str) == Some(self.record.resource_card()))
        {
            if model.get("status").and_then(Value::as_str) != Some("stopped") {
                return Err(
                    "maintenance_model_not_stopped: 服务尚未确认停用，请先在此恢复入口停用".into(),
                );
            }
        } else {
            let state = checked(
                self.client
                    .model_maintenance_state(self.record.resource_card())?,
            )?;
            if state.get("model_present").and_then(Value::as_bool) != Some(false) {
                return Err("maintenance_model_unknown: 无法核实模型已注销".into());
            }
        }
        Ok(())
    }

    pub fn finish(&mut self) -> Result<(), String> {
        let current = self.record.run_id.clone();
        self.finish_for_run(current.as_deref())
    }

    fn finish_for_run(&mut self, current_store_run: Option<&str>) -> Result<(), String> {
        self.check_protocol()?;
        let _execution = self
            .record
            .fence
            .as_ref()
            .map(|directory| {
                ExecutionFence {
                    directory: directory.clone(),
                }
                .quiesce()
            })
            .transpose()?;
        self.verify_idle(current_store_run)?;
        self.stage("releasing")?;
        let value = checked(self.client.model_maintenance(
            self.record.resource_card(),
            "end",
            &self.record.owner,
        )?)?;
        if value.get("maintenance").and_then(Value::as_bool) != Some(false) {
            return Err("maintenance_release_unknown".into());
        }
        self.record.phase = "completed".into();
        self.record.error = None;
        self.stage("completed")
    }

    pub fn failed(&mut self, error: &str) {
        self.record.phase = "recovery_required".into();
        self.record.error = Some(error.replace(&self.record.owner, "[private]"));
        let _ = save(&mut self.record);
        let _ = self.client.model_maintenance(
            self.record.resource_card(),
            "failed",
            &self.record.owner,
        );
    }
}

impl Drop for Transaction {
    fn drop(&mut self) {
        if self.record.phase == "active" {
            self.failed("maintenance_interrupted: 卸载未完整结束，需核实维护状态");
        }
    }
}

pub fn summaries() -> Result<Vec<Summary>, String> {
    let mut summaries = Vec::new();
    let Some(metadata) = super::control_file::metadata_optional(&root())
        .map_err(|error| format!("maintenance_index_unknown: {error}"))?
    else {
        return Ok(summaries);
    };
    if !metadata.is_dir() {
        return Err("maintenance_index_unknown: not a directory".into());
    }
    for entry in fs::read_dir(root()).map_err(|e| e.to_string())? {
        let path = entry.map_err(|e| e.to_string())?.path();
        if path.extension().and_then(|s| s.to_str()) != Some("json") {
            continue;
        }
        let Some(card) = path.file_stem().and_then(|s| s.to_str()) else {
            continue;
        };
        let record = match load(card) {
            Ok(record) => record,
            Err(_) => {
                summaries.push(Summary {
                    card_id: card.to_string(),
                    card_name: card.to_string(),
                    phase: "record_error".into(),
                    stage: "unknown".into(),
                    recovery_required: true,
                    worker_active: false,
                    purge_weights: false,
                    reservation_only: false,
                    error_truncated: false,
                    error: Some(
                        "maintenance_record_corrupt: 维护记录无法核实，请从备份恢复，不自动解锁"
                            .into(),
                    ),
                });
                continue;
            }
        };
        let Some(record) = record.filter(|r| r.phase != "completed") else {
            continue;
        };
        let worker_active = lock(card).is_err();
        let (error, error_truncated) = match record.error.as_deref() {
            Some(text) => {
                let (text, truncated) = super::log_page::summary(text);
                (Some(text), truncated)
            }
            None => (None, false),
        };
        summaries.push(Summary {
            card_id: record.card_id,
            card_name: record.card_name,
            phase: record.phase,
            stage: record.stage,
            recovery_required: !worker_active,
            worker_active,
            purge_weights: record.purge_weights,
            reservation_only: record.reservation_only,
            error,
            error_truncated,
        });
    }
    Ok(summaries)
}

/// Explicit desktop recovery: never rerun uninstall or purge. It can stop the
/// maintained service using the private owner, then release after positive checks.
pub fn recover(
    card: &str,
    control: &RunControl,
    log: &mut dyn FnMut(&str),
) -> Result<Value, String> {
    let guard = lock(card)?;
    let record = load(card)?.ok_or("maintenance_record_missing")?;
    if record.phase == "completed" {
        return Ok(serde_json::json!({"recovered":true,"alreadyCompleted":true}));
    }
    if let Some(run) = record.run_id.as_ref() {
        let path = config::data_dir()
            .join("runs")
            .join(format!("{}.json", config::safe_path_component(run)?));
        if !original_task_finished(&path)? {
            return Err(
                "maintenance_original_task_active: 原任务尚未终结，请先恢复后台任务记录".into(),
            );
        }
    }
    let client = Client::at_socket(record.socket.clone())?;
    let mut transaction = Transaction {
        client,
        record,
        _lock: guard,
    };
    transaction.check_protocol()?;
    let _execution = transaction
        .record
        .fence
        .as_ref()
        .map(|directory| {
            ExecutionFence {
                directory: directory.clone(),
            }
            .quiesce()
        })
        .transpose()?;
    let state = checked(
        transaction
            .client
            .model_maintenance_state(transaction.record.resource_card())?,
    )?;
    if state.get("maintenance").and_then(Value::as_bool) == Some(false) {
        let never_started = transaction.record.stage == "acquiring"
            && transaction.record.tasks.is_empty()
            && transaction.record.fence.is_none();
        if transaction.record.stage != "releasing" && !never_started {
            return Err("maintenance_state_changed: 远端维护记录缺失，请先核实设备状态".into());
        }
        transaction.record.phase = "completed".into();
        transaction.stage("completed")?;
        return Ok(serde_json::json!({"recovered":true,"releasePreviouslyCompleted":true}));
    }
    checked(transaction.client.model_maintenance(
        transaction.record.resource_card(),
        "failed",
        &transaction.record.owner,
    )?)?;
    if transaction.record.reservation_protocol {
        transaction.record.reservation_only = state
            .get("reservation_only")
            .and_then(Value::as_bool)
            .ok_or("maintenance_mode_unknown: 设备未确认原清理预留模式，保留事务")?;
        save(&mut transaction.record)?;
    }
    let tasks = checked(transaction.client.tasks()?)?;
    if tasks
        .get("tasks")
        .and_then(Value::as_array)
        .ok_or("maintenance_tasks_unknown")?
        .iter()
        .any(|task| {
            transaction
                .record
                .task_blocks_release(task, control.run_id.as_deref())
        })
    {
        return Err("maintenance_task_active: 原设备任务仍在执行，保留维护".into());
    }
    if state.get("model_present").and_then(Value::as_bool) == Some(true) {
        log("[cmsd] 恢复维护：先经原事务停用模型，不重跑卸载脚本");
        transaction.action("disable", control, log)?;
    }
    if super::db::get_store_model(card)
        .map_err(|e| e.to_string())?
        .is_some()
    {
        super::db::set_running(card, false).map_err(|e| e.to_string())?;
    } else if transaction.record.purge_weights {
        delegate::forget_ownership(card)?;
    }
    drop(_execution);
    transaction.finish_for_run(control.run_id.as_deref())?;
    Ok(serde_json::json!({"recovered":true,"uninstallResumed":false,"filesPreserved":true}))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;
    use std::io::{Read, Write};
    use std::os::unix::fs::PermissionsExt;
    use std::os::unix::net::{UnixListener, UnixStream};
    use std::sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    };

    #[test]
    fn special_and_unknown_maintenance_records_never_clear_admission() {
        use std::os::unix::fs::{FileTypeExt, symlink};
        let _home = config::TempHome::new();
        fs::create_dir_all(root()).unwrap();
        let record = path("record-io").unwrap();
        let name = std::ffi::CString::new(record.as_os_str().as_encoded_bytes()).unwrap();
        // SAFETY: name is a terminated path under this test's temporary home.
        assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
        assert!(ensure_clear("record-io").is_err());
        assert!(record.symlink_metadata().unwrap().file_type().is_fifo());
        fs::remove_file(&record).unwrap();
        symlink("missing", &record).unwrap();
        assert!(ensure_clear("record-io").is_err());
        fs::remove_file(&record).unwrap();
        fs::remove_dir(root()).unwrap();
        symlink("missing", root()).unwrap();
        assert!(ensure_clear("record-io").is_err());
        assert!(summaries().is_err());
        fs::remove_file(root()).unwrap();
        assert!(ensure_clear("record-io").is_ok());
        assert!(summaries().unwrap().is_empty());
    }

    #[test]
    fn original_terminal_proof_streams_large_logs_and_refuses_special_evidence() {
        use std::io::Write;
        let _home = config::TempHome::new();
        let path = config::data_dir().join("large-original.json");
        let mut file = std::fs::File::create(&path).unwrap();
        file.set_permissions(fs::Permissions::from_mode(0o600))
            .unwrap();
        file.write_all(b"{\"lines\":[\"").unwrap();
        for _ in 0..17 {
            file.write_all(&vec![b'x'; 1024 * 1024]).unwrap();
        }
        file.write_all(b"\"],\"outcome\":{\"Ok\":null}}").unwrap();
        drop(file);
        assert!(original_task_finished(&path).unwrap());
        fs::remove_file(&path).unwrap();
        let name = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
        // SAFETY: name is a terminated path owned by the temporary test home.
        assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
        assert!(original_task_finished(&path).is_err());
        fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink("missing", &path).unwrap();
        assert!(original_task_finished(&path).is_err());
    }

    #[derive(Default)]
    struct State {
        logical_card: Option<String>,
        owner: Option<String>,
        present: bool,
        owned: bool,
        adoptable: bool,
        reservation_only: bool,
        status_override: Option<String>,
        running: bool,
        active: bool,
        store_running: Option<String>,
        old: bool,
        reservation_cap: bool,
        pause_enable: bool,
        drop_begin: bool,
        drop_end: bool,
        tasks: BTreeMap<String, Value>,
        last_action: Option<String>,
        requests: Vec<String>,
    }

    fn handle(state: &mut State, method: &str, path: &str, body: Value) -> Option<(u16, Value)> {
        use serde_json::json;
        state.requests.push(format!("{method} {path}"));
        let gate = "/v1/models/model-a/maintenance";
        if path == gate && state.old {
            return Some((404, json!({"error":{"message":"unsupported"}})));
        }
        let result = match (method, path) {
            ("GET", "/v1/device") => {
                let mut capabilities = if state.old {
                    vec![]
                } else {
                    vec!["model.maintenance", "model.explicit_adoption"]
                };
                if state.reservation_cap && !state.old {
                    capabilities.push("model.maintenance.reservation");
                }
                json!({"capabilities":capabilities})
            }
            ("GET", "/v1/models") => {
                let bindings: BTreeMap<_, _> = state
                    .logical_card
                    .as_ref()
                    .map(|logical| BTreeMap::from([(logical.as_str(), "model-a")]))
                    .unwrap_or_default();
                json!({"store_bindings":bindings,"models":if state.present { vec![json!({"card":"model-a","managed":state.owned,"can_adopt":state.adoptable,"can_reserve_uninstall":true,"status":if state.running {"running"} else {state.status_override.as_deref().unwrap_or("stopped")}})] } else {vec![]}})
            }
            ("GET", "/v1/tasks") => {
                let mut tasks: Vec<_> = state.tasks.values().cloned().collect();
                if state.active {
                    tasks.push(json!({"id":"other-active","card":"model-a","status":"running"}));
                }
                if let Some(id) = &state.store_running {
                    tasks.push(json!({"id":id,"card":state.logical_card.as_deref().unwrap_or("model-a"),"status":"running","source":"store","error":null}));
                }
                json!({"tasks":tasks})
            }
            ("GET", path) if path == gate => {
                json!({"maintenance":state.owner.is_some(),"reservation_only":state.reservation_only,"card":"model-a","model_present":state.present,
                "last_task_id":state.tasks.keys().last(),"last_task_status":if state.tasks.is_empty(){Value::Null}else{json!("succeeded")}})
            }
            ("GET", path) if path.starts_with("/v1/tasks/") => state
                .tasks
                .get(&path[10..])
                .cloned()
                .unwrap_or(json!({"status":"failed"})),
            ("POST", path) if path == gate => {
                let owner = body["owner"].as_str().unwrap_or("");
                match body["action"].as_str().unwrap_or("") {
                    "begin" => {
                        if state.owner.as_deref().is_some_and(|value| value != owner) {
                            return Some((409, json!({"error":{"message":"other owner"}})));
                        }
                        state.reservation_only = !state.owned;
                        if state.reservation_only && state.running {
                            return Some((
                                409,
                                json!({"error":{"message":"external runtime running"}}),
                            ));
                        }
                        state.owner = Some(owner.to_string());
                        if std::mem::take(&mut state.drop_begin) {
                            return None;
                        }
                        json!({"maintenance":true,"reservation_only":state.reservation_only,"card":"model-a"})
                    }
                    "failed" if state.owner.as_deref() == Some(owner) => {
                        json!({"maintenance":true})
                    }
                    "end"
                        if state.owner.as_deref() == Some(owner)
                            && !state.active
                            && !state.running =>
                    {
                        state.owner = None;
                        if std::mem::take(&mut state.drop_end) {
                            return None;
                        }
                        json!({"maintenance":false})
                    }
                    _ => return Some((409, json!({"error":{"message":"maintenance conflict"}}))),
                }
            }
            ("POST", "/v1/models/model-a/enable") => {
                if state.owner.is_some() {
                    return Some((409, json!({"error":{"message":"maintenance held"}})));
                }
                state.owned = true;
                state.running = true;
                let id = format!("task-{}", state.tasks.len() + 1);
                state.tasks.insert(
                    id.clone(),
                    json!({"id":id,"card":"model-a","status":if state.pause_enable {"running"} else {"succeeded"},"error":null}),
                );
                json!({"task_id":id})
            }
            ("POST", path) if path.ends_with("/disable") || path.ends_with("/purge") => {
                if state.owner.as_deref() != body["maintenance_owner"].as_str() {
                    return Some((409, json!({"error":{"message":"owner required"}})));
                }
                if state.reservation_only && state.running {
                    return Some((409, json!({"error":{"message":"external runtime running"}})));
                }
                state.running = false;
                if path.ends_with("/purge") {
                    state.present = false;
                }
                let id = format!("task-{}", state.tasks.len() + 1);
                state.tasks.insert(
                    id.clone(),
                    json!({"id":id,"card":"model-a","status":"succeeded","error":null}),
                );
                state.last_action = Some(path.to_string());
                json!({"task_id":id})
            }
            _ => json!({"service":"cmsd"}),
        };
        Some((200, result))
    }

    struct Fixture {
        _home: config::TempHome,
        state: Arc<Mutex<State>>,
        stop: Arc<AtomicBool>,
        thread: Option<std::thread::JoinHandle<()>>,
        previous: Option<std::ffi::OsString>,
        _environment: std::sync::MutexGuard<'static, ()>,
    }

    impl Fixture {
        fn new() -> Self {
            let environment = crate::platform::paths::tests::ENV_LOCK
                .lock()
                .unwrap_or_else(|e| e.into_inner());
            let home = config::TempHome::new();
            super::super::db::init_db().unwrap();
            let socket = home.path().join("cmsd.sock");
            let listener = UnixListener::bind(&socket).unwrap();
            listener.set_nonblocking(true).unwrap();
            let previous = std::env::var_os("CMSD_SOCKET");
            unsafe {
                std::env::set_var("CMSD_SOCKET", &socket);
            }
            let state = Arc::new(Mutex::new(State {
                present: true,
                owned: true,
                adoptable: true,
                reservation_cap: true,
                ..Default::default()
            }));
            let stop = Arc::new(AtomicBool::new(false));
            let cloned = Arc::clone(&state);
            let stopping = Arc::clone(&stop);
            let thread = std::thread::spawn(move || {
                while !stopping.load(Ordering::SeqCst) {
                    let mut stream = match listener.accept() {
                        Ok((stream, _)) => stream,
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                            std::thread::sleep(Duration::from_millis(5));
                            continue;
                        }
                        Err(_) => break,
                    };
                    respond(&mut stream, &cloned);
                }
            });
            Self {
                _home: home,
                state,
                stop,
                thread: Some(thread),
                previous,
                _environment: environment,
            }
        }
    }

    fn respond(stream: &mut UnixStream, state: &Arc<Mutex<State>>) {
        stream
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        let mut bytes = Vec::new();
        let (head, split, size) = loop {
            let mut chunk = [0; 4096];
            let Ok(n) = stream.read(&mut chunk) else {
                return;
            };
            if n == 0 {
                return;
            }
            bytes.extend_from_slice(&chunk[..n]);
            if let Some(split) = bytes.windows(4).position(|v| v == b"\r\n\r\n") {
                let head = String::from_utf8_lossy(&bytes[..split]).to_string();
                let size = head
                    .lines()
                    .find_map(|line| {
                        line.to_lowercase()
                            .strip_prefix("content-length:")
                            .and_then(|v| v.trim().parse::<usize>().ok())
                    })
                    .unwrap_or(0);
                break (head, split + 4, size);
            }
        };
        while bytes.len() < split + size {
            let mut chunk = [0; 4096];
            let Ok(n) = stream.read(&mut chunk) else {
                return;
            };
            if n == 0 {
                return;
            }
            bytes.extend_from_slice(&chunk[..n]);
        }
        let mut request = head.split_whitespace();
        let method = request.next().unwrap();
        let path = request.next().unwrap();
        let body = serde_json::from_slice(&bytes[split..split + size]).unwrap_or(Value::Null);
        if let Some((status, body)) = handle(&mut state.lock().unwrap(), method, path, body) {
            let body = serde_json::to_vec(&body).unwrap();
            let head = format!(
                "HTTP/1.1 {status} Result\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            );
            stream.write_all(head.as_bytes()).unwrap();
            stream.write_all(&body).unwrap();
        }
    }

    impl Drop for Fixture {
        fn drop(&mut self) {
            self.stop.store(true, Ordering::SeqCst);
            if let Some(thread) = self.thread.take() {
                thread.join().unwrap();
            }
            unsafe {
                match self.previous.as_ref() {
                    Some(value) => std::env::set_var("CMSD_SOCKET", value),
                    None => std::env::remove_var("CMSD_SOCKET"),
                }
            }
        }
    }

    #[test]
    fn declared_unenabled_card_reserves_cleanup_without_lifecycle_authority() {
        let fixture = Fixture::new();
        {
            let mut state = fixture.state.lock().unwrap();
            state.owned = false;
            state.adoptable = false;
            state.status_override = Some("not_deployed".into());
        }
        let mut transaction =
            Transaction::begin("model-a", "Partial install", false, &Default::default())
                .unwrap()
                .unwrap();
        assert!(transaction.record.reservation_only);
        assert!(
            serde_json::to_value(summaries().unwrap()).unwrap()[0]["reservationOnly"]
                .as_bool()
                .unwrap()
        );
        assert!(Client::connect().unwrap().manages_card("model-a").is_err());
        transaction
            .action("disable", &Default::default(), &mut |_| {})
            .unwrap();
        assert!(!fixture.state.lock().unwrap().owned);
        assert!(
            !crate::features::megacube::installation_records_dir()
                .join("ownership/model-a")
                .exists()
        );
        transaction.finish().unwrap();
        assert!(fixture.state.lock().unwrap().owner.is_none());
    }

    #[test]
    fn explicit_enable_adopts_but_discovery_does_not() {
        let fixture = Fixture::new();
        fixture.state.lock().unwrap().owned = false;
        assert!(Client::connect().unwrap().manages_card("model-a").is_err());
        assert!(
            !crate::features::megacube::installation_records_dir()
                .join("ownership/model-a")
                .exists()
        );
        let result = delegate::enable("model-a", &mut |_| {}, &|| false).unwrap();
        assert!(matches!(result, delegate::DelegateOutcome::Enabled { .. }));
        assert!(fixture.state.lock().unwrap().owned);
        assert!(Client::connect().unwrap().manages_card("model-a").unwrap());
        assert!(
            crate::features::megacube::installation_records_dir()
                .join("ownership/model-a")
                .exists()
        );
    }

    #[test]
    fn accepted_adoption_without_a_terminal_reply_never_falls_back_when_socket_disappears() {
        let fixture = Fixture::new();
        {
            let mut state = fixture.state.lock().unwrap();
            state.owned = false;
            state.pause_enable = true;
        }
        assert!(delegate::enable("model-a", &mut |_| {}, &|| true).is_err());
        assert!(fixture.state.lock().unwrap().owned);
        assert!(
            !crate::features::megacube::installation_records_dir()
                .join("ownership/model-a")
                .exists()
        );
        fs::remove_file(socket_path()).unwrap();
        assert!(
            delegate::stop("model-a", &mut |_| {}, &|| false)
                .err()
                .unwrap()
                .contains("cmsd_unavailable")
        );
    }

    #[test]
    fn protocol_downgrade_blocks_new_recovery_but_legacy_owned_records_can_recover() {
        let fixture = Fixture::new();
        drop(
            Transaction::begin("model-a", "Fixture", false, &Default::default())
                .unwrap()
                .unwrap(),
        );
        fixture.state.lock().unwrap().reservation_cap = false;
        let count = fixture.state.lock().unwrap().tasks.len();
        assert!(
            recover("model-a", &Default::default(), &mut |_| {})
                .unwrap_err()
                .contains("maintenance_unsupported")
        );
        assert_eq!(fixture.state.lock().unwrap().tasks.len(), count);
        let saved = path("model-a").unwrap();
        let mut old: Value = serde_json::from_slice(&fs::read(&saved).unwrap()).unwrap();
        old.as_object_mut().unwrap().remove("reservation_protocol");
        old.as_object_mut().unwrap().remove("reservation_only");
        private_write(&saved, &serde_json::to_vec(&old).unwrap()).unwrap();
        recover("model-a", &Default::default(), &mut |_| {}).unwrap();
        assert!(fixture.state.lock().unwrap().owner.is_none());
    }

    #[test]
    fn only_the_current_store_run_is_exempt_from_release_contention() {
        let mut task =
            serde_json::json!({"card":"model-a","id":"self","source":"store","status":"running"});
        assert!(!task_blocks_release(&task, "model-a", Some("self")));
        assert!(task_blocks_release(&task, "model-a", None));
        assert!(task_blocks_release(&task, "model-a", Some("another")));
        task["source"] = serde_json::json!("cmsd");
        assert!(task_blocks_release(&task, "model-a", Some("self")));
        task["source"] = serde_json::json!("store");
        task["status"] = Value::Null;
        assert!(task_blocks_release(&task, "model-a", Some("self")));
    }

    #[test]
    fn complete_transaction_keeps_credential_private_and_releases_after_purge() {
        let fixture = Fixture::new();
        fixture.state.lock().unwrap().store_running = Some("store-self".into());
        let control = RunControl {
            run_id: Some("store-self".into()),
            ..Default::default()
        };
        let mut transaction = Transaction::begin("model-a", "Fixture", true, &control)
            .unwrap()
            .unwrap();
        let record = load("model-a").unwrap().unwrap();
        assert_eq!(
            fs::metadata(path("model-a").unwrap())
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
        assert_eq!(
            fs::metadata(root()).unwrap().permissions().mode() & 0o777,
            0o700
        );
        let public = serde_json::to_string(&summaries().unwrap()).unwrap();
        assert!(!public.contains(&record.owner));
        transaction
            .action("disable", &Default::default(), &mut |_| {})
            .unwrap();
        transaction.fence().unwrap();
        transaction
            .action("purge", &Default::default(), &mut |_| {})
            .unwrap();
        transaction.stage("committed").unwrap();
        transaction.finish().unwrap();
        assert!(fixture.state.lock().unwrap().owner.is_none());
        assert!(summaries().unwrap().is_empty());
    }

    #[test]
    fn unsupported_daemon_never_starts_a_maintenance_transaction() {
        let fixture = Fixture::new();
        fixture.state.lock().unwrap().old = true;
        let error = Transaction::begin("model-a", "Fixture", false, &Default::default())
            .err()
            .unwrap();
        assert!(error.contains("maintenance_unsupported"));
        assert!(fixture.state.lock().unwrap().owner.is_none());
        assert!(summaries().unwrap().is_empty());
    }

    #[test]
    fn lost_begin_response_keeps_owner_and_can_recover_without_rerunning_scripts() {
        let fixture = Fixture::new();
        fixture.state.lock().unwrap().drop_begin = true;
        assert!(Transaction::begin("model-a", "Fixture", false, &Default::default()).is_err());
        assert_eq!(load("model-a").unwrap().unwrap().phase, "recovery_required");
        let result = recover("model-a", &Default::default(), &mut |_| {}).unwrap();
        assert_eq!(result["uninstallResumed"], false);
        assert!(fixture.state.lock().unwrap().owner.is_none());
    }

    #[test]
    fn failed_transaction_cannot_release_while_a_device_task_is_running() {
        let fixture = Fixture::new();
        let mut transaction = Transaction::begin("model-a", "Fixture", false, &Default::default())
            .unwrap()
            .unwrap();
        transaction.failed("cancelled");
        drop(transaction);
        fixture.state.lock().unwrap().active = true;
        assert!(
            recover("model-a", &Default::default(), &mut |_| {})
                .unwrap_err()
                .contains("maintenance_task_active")
        );
        assert!(fixture.state.lock().unwrap().owner.is_some());
        fixture.state.lock().unwrap().active = false;
        fixture.state.lock().unwrap().store_running = Some("other-store".into());
        assert!(recover("model-a", &Default::default(), &mut |_| {}).is_err());
        fixture.state.lock().unwrap().store_running = Some("recovery-self".into());
        let control = RunControl {
            run_id: Some("recovery-self".into()),
            ..Default::default()
        };
        recover("model-a", &control, &mut |_| {}).unwrap();
        assert!(fixture.state.lock().unwrap().owner.is_none());
    }

    #[test]
    fn lost_end_response_is_reconciled_without_another_stop_or_delete() {
        let fixture = Fixture::new();
        let mut transaction = Transaction::begin("model-a", "Fixture", false, &Default::default())
            .unwrap()
            .unwrap();
        transaction
            .action("disable", &Default::default(), &mut |_| {})
            .unwrap();
        fixture.state.lock().unwrap().drop_end = true;
        assert!(transaction.finish().is_err());
        drop(transaction);
        let count = fixture.state.lock().unwrap().tasks.len();
        let result = recover("model-a", &Default::default(), &mut |_| {}).unwrap();
        assert_eq!(result["releasePreviouslyCompleted"], true);
        assert_eq!(fixture.state.lock().unwrap().tasks.len(), count);
    }

    #[test]
    fn versioned_maintenance_recovery_uses_saved_runtime_and_checks_both_task_identities() {
        let fixture = Fixture::new();
        fixture.state.lock().unwrap().logical_card = Some("logical-a".into());
        let mut transaction =
            Transaction::begin("logical-a", "Versioned", false, &Default::default())
                .unwrap()
                .unwrap();
        assert_eq!(transaction.record.resource_card(), "model-a");
        transaction.failed("interrupted");
        drop(transaction);
        fixture.state.lock().unwrap().active = true;
        assert!(
            recover("logical-a", &Default::default(), &mut |_| {})
                .unwrap_err()
                .contains("maintenance_task_active")
        );
        fixture.state.lock().unwrap().active = false;
        fixture.state.lock().unwrap().store_running = Some("other-logical-task".into());
        assert!(
            recover("logical-a", &Default::default(), &mut |_| {})
                .unwrap_err()
                .contains("maintenance_task_active")
        );
        fixture.state.lock().unwrap().store_running = Some("recovery-task".into());
        let control = RunControl {
            run_id: Some("recovery-task".into()),
            ..Default::default()
        };
        recover("logical-a", &control, &mut |_| {}).unwrap();
        assert!(fixture.state.lock().unwrap().owner.is_none());
        assert_eq!(load("logical-a").unwrap().unwrap().phase, "completed");
        assert!(
            !fixture
                .state
                .lock()
                .unwrap()
                .requests
                .iter()
                .any(|path| path.contains("/logical-a/maintenance"))
        );
    }

    #[test]
    fn versioned_maintenance_reconciles_a_lost_release_without_querying_logical_gate() {
        let fixture = Fixture::new();
        fixture.state.lock().unwrap().logical_card = Some("logical-a".into());
        let mut transaction =
            Transaction::begin("logical-a", "Versioned", false, &Default::default())
                .unwrap()
                .unwrap();
        transaction
            .action("disable", &Default::default(), &mut |_| {})
            .unwrap();
        fixture.state.lock().unwrap().drop_end = true;
        assert!(transaction.finish().is_err());
        drop(transaction);
        let result = recover("logical-a", &Default::default(), &mut |_| {}).unwrap();
        assert_eq!(result["releasePreviouslyCompleted"], true);
        assert!(fixture.state.lock().unwrap().owner.is_none());
        assert!(
            !fixture
                .state
                .lock()
                .unwrap()
                .requests
                .iter()
                .any(|path| path.contains("/logical-a/maintenance"))
        );
    }

    #[test]
    fn original_store_task_must_be_terminal_before_recovery() {
        let _fixture = Fixture::new();
        let control = RunControl {
            run_id: Some("run-original".into()),
            ..Default::default()
        };
        drop(
            Transaction::begin("model-a", "Fixture", false, &control)
                .unwrap()
                .unwrap(),
        );
        let saved = config::data_dir().join("runs/run-original.json");
        private_write(&saved, br#"{"outcome":null}"#).unwrap();
        assert!(
            recover("model-a", &Default::default(), &mut |_| {})
                .unwrap_err()
                .contains("original_task_active")
        );
        private_write(&saved, br#"{"outcome":{"Err":"interrupted"}}"#).unwrap();
        recover("model-a", &Default::default(), &mut |_| {}).unwrap();
    }
}
