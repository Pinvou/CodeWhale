//! Narrow adapter from engine observers to the authoritative remote task.
//! The remote ledger owns terminal status; generic turn observers own model
//! selection and first output. Collection leases never cross installations.
use super::*;

#[derive(Clone, Serialize, Deserialize)]
pub(super) struct RemoteExecution {
    task_id: String,
    mode: String,
    started_at: u64,
    turn_id: Option<String>,
    first_output: bool,
    lease: CollectionLease,
}

#[derive(Clone, Serialize, Deserialize)]
struct RemoteOrigin {
    mode: Option<String>,
    started_at: u64,
    lease: CollectionLease,
}

#[derive(Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct RemoteTelemetryState {
    executions: HashMap<String, RemoteExecution>,
    origins: HashMap<String, RemoteOrigin>,
}

fn context_path() -> PathBuf {
    paths::pinvou3_home().join("behavior-telemetry-remote-tasks.json")
}

fn persist(state: &RemoteTelemetryState) -> Result<()> {
    filesystem::atomic_write_private(&context_path(), &serde_json::to_vec(state)?)?;
    Ok(())
}

pub(super) fn load_state() -> RemoteTelemetryState {
    load_state_at(&context_path())
}

fn load_state_at(path: &Path) -> RemoteTelemetryState {
    if !std::fs::metadata(path).is_ok_and(|meta| meta.len() <= 8 * 1024 * 1024) {
        return RemoteTelemetryState::default();
    }
    let mut state: RemoteTelemetryState = std::fs::read(path)
        .ok()
        .and_then(|bytes| serde_json::from_slice(&bytes).ok())
        .unwrap_or_default();
    prune(&mut state);
    state
}

fn prune(state: &mut RemoteTelemetryState) {
    let cutoff = now_ms().saturating_sub(OUTBOX_RETENTION_MS);
    state.executions.retain(|_, e| e.started_at >= cutoff);
    state.origins.retain(|_, e| e.started_at >= cutoff);
    if state.executions.len() > OUTBOX_MAX_EVENTS || state.origins.len() > OUTBOX_MAX_EVENTS {
        *state = RemoteTelemetryState::default();
    }
}

pub(super) fn clear(telemetry: &BehaviorTelemetry) -> Result<()> {
    let mut contexts = telemetry
        .remote_executions
        .lock()
        .map_err(|_| anyhow::anyhow!("remote telemetry lock poisoned"))?;
    *contexts = RemoteTelemetryState::default();
    persist(&contexts)
}

/// Credential renewal preserves consent generation, but replaces the server
/// collection period. Never migrate work from a withdrawn authorization.
pub(super) fn rebind(telemetry: &BehaviorTelemetry, generation: u64, period: &str) -> Result<()> {
    let mut contexts = telemetry
        .remote_executions
        .lock()
        .map_err(|_| anyhow::anyhow!("remote telemetry lock poisoned"))?;
    for execution in contexts.executions.values_mut() {
        if execution.lease.generation == generation {
            execution.lease.collection_period_id = period.to_owned();
        }
    }
    for origin in contexts.origins.values_mut() {
        if origin.lease.generation == generation {
            origin.lease.collection_period_id = period.to_owned();
        }
    }
    persist(&contexts)
}

pub(crate) fn register_remote_origin<R: tauri::Runtime>(
    app: &tauri::AppHandle<R>,
    task: &str,
    mode: Option<&str>,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let Some(lease) = telemetry.current_lease() else {
        return;
    };
    if let Ok(mut contexts) = telemetry.remote_executions.lock() {
        prune(&mut contexts);
        if contexts.origins.len() >= OUTBOX_MAX_EVENTS {
            return;
        }
        // A retry cannot refresh an old task's collection authorization.
        contexts
            .origins
            .entry(task.to_owned())
            .or_insert_with(|| RemoteOrigin {
                mode: mode.map(str::to_owned),
                started_at: now_ms(),
                lease,
            });
        let _ = persist(&contexts);
    }
}

pub(crate) fn track_remote_result_received(app: &tauri::AppHandle, task: &str, status: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let origin = telemetry
        .remote_executions
        .lock()
        .ok()
        .and_then(|contexts| contexts.origins.get(task).cloned());
    let Some(origin) = origin else {
        return;
    };
    telemetry.track_for_lease(
        RemoteControlEvent::new(RemoteStage::ResultReceived, RemoteOutcome::Ok)
            .task(task, false, origin.mode.as_deref())
            .status(status)
            .duration_ms(now_ms().saturating_sub(origin.started_at))
            .into_behavior(),
        &origin.lease,
    );
}

pub(crate) fn track_remote_origin_event<R: tauri::Runtime>(
    app: &tauri::AppHandle<R>,
    task: &str,
    event: RemoteControlEvent,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let lease = telemetry
        .remote_executions
        .lock()
        .ok()
        .and_then(|contexts| {
            contexts
                .origins
                .get(task)
                .map(|origin| origin.lease.clone())
        });
    if let Some(lease) = lease {
        telemetry.track_for_lease(event.into_behavior(), &lease);
    }
}

pub(crate) fn register_remote_execution(
    app: &tauri::AppHandle,
    session: &str,
    task: &str,
    mode: &str,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let Some(lease) = telemetry.current_lease() else {
        return;
    };
    if let Ok(mut contexts) = telemetry.remote_executions.lock() {
        prune(&mut contexts);
        let executions = &mut contexts.executions;
        executions.retain(|_, execution| telemetry.lease_is_current(&execution.lease));
        if executions.len() >= OUTBOX_MAX_EVENTS
            || executions.get(session).is_some_and(|e| e.task_id == task)
        {
            return;
        }
        executions.insert(
            session.to_owned(),
            RemoteExecution {
                task_id: task.to_owned(),
                mode: mode.to_owned(),
                started_at: now_ms(),
                turn_id: None,
                first_output: false,
                lease,
            },
        );
        let _ = persist(&contexts);
    }
}

pub(crate) fn unregister_remote_execution(app: &tauri::AppHandle, session: &str, task: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    if let Ok(mut contexts) = telemetry.remote_executions.lock() {
        let executions = &mut contexts.executions;
        if executions.get(session).is_some_and(|e| e.task_id == task) {
            executions.remove(session);
        }
        let _ = persist(&contexts);
    }
}

pub(crate) fn track_remote_execution_event(
    app: &tauri::AppHandle,
    session: &str,
    task: &str,
    event: RemoteControlEvent,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let lease = telemetry
        .remote_executions
        .lock()
        .ok()
        .and_then(|contexts| {
            contexts
                .executions
                .get(session)
                .filter(|e| e.task_id == task)
                .map(|e| e.lease.clone())
        });
    if let Some(lease) = lease {
        telemetry.track_for_lease(event.into_behavior(), &lease);
    }
}

/// An executor's task-bound stream is also a first-output witness, including
/// during reconnect. A late chunk cannot attach to a new task in the session.
pub(crate) fn track_remote_task_output(app: &tauri::AppHandle, session: &str, task: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let execution = telemetry
        .remote_executions
        .lock()
        .ok()
        .and_then(|mut contexts| {
            let execution = contexts.executions.get_mut(session)?;
            if execution.task_id != task
                || execution.first_output
                || !telemetry.lease_is_current(&execution.lease)
            {
                return None;
            }
            execution.first_output = true;
            let execution = execution.clone();
            let _ = persist(&contexts);
            Some(execution)
        });
    if let Some(execution) = execution {
        telemetry.track_for_lease(
            RemoteControlEvent::new(RemoteStage::FirstOutput, RemoteOutcome::Ok)
                .task(task, true, Some(&execution.mode))
                .duration_ms(now_ms().saturating_sub(execution.started_at))
                .into_behavior(),
            &execution.lease,
        );
    }
}

pub(super) fn tool_task_key(
    telemetry: &BehaviorTelemetry,
    session: &str,
    turn: &str,
) -> Option<String> {
    let contexts = telemetry.remote_executions.lock().ok()?;
    let execution = contexts.executions.get(session)?;
    if execution.turn_id.as_deref() != Some(turn) || !telemetry.lease_is_current(&execution.lease) {
        return None;
    }
    let event = RemoteControlEvent::new(RemoteStage::TaskRunning, RemoteOutcome::Ok)
        .task(&execution.task_id, true, Some(&execution.mode))
        .into_behavior();
    event
        .fields
        .get("remote_task_key")?
        .as_str()
        .map(str::to_owned)
}

pub(super) fn observe(
    app: &tauri::AppHandle,
    session: &str,
    turn: &str,
    stage: RemoteStage,
    model: Option<(&str, &str)>,
) -> bool {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return false;
    };
    let execution = telemetry
        .remote_executions
        .lock()
        .ok()
        .and_then(|mut contexts| {
            let execution = contexts.executions.get_mut(session)?;
            if !telemetry.lease_is_current(&execution.lease) {
                return None;
            }
            if stage == RemoteStage::TaskRunning && execution.turn_id.is_none() {
                execution.turn_id = Some(turn.to_owned());
                let execution = execution.clone();
                let _ = persist(&contexts);
                return Some(execution);
            }
            let execution = execution.clone();
            Some(execution)
        });
    let Some(execution) = execution else {
        return false;
    };
    // A late event for an older turn cannot attach to the next remote task.
    if execution.turn_id.as_deref() != Some(turn) {
        return true;
    }
    if stage == RemoteStage::FirstOutput {
        if let Ok(mut contexts) = telemetry.remote_executions.lock() {
            let Some(current) = contexts.executions.get_mut(session) else {
                return true;
            };
            if current.task_id != execution.task_id || current.first_output {
                return true;
            }
            current.first_output = true;
            let _ = persist(&contexts);
        } else {
            return true;
        }
    }
    let mut event = RemoteControlEvent::new(stage, RemoteOutcome::Ok)
        .task(&execution.task_id, true, Some(&execution.mode))
        .duration_ms(now_ms().saturating_sub(execution.started_at));
    if let Some((provider, model)) = model {
        event = event.model(provider, model);
    }
    telemetry.track_for_lease(event.into_behavior(), &execution.lease);
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    const _: fn(&tauri::AppHandle, &str, &str, &str) = super::super::register_remote_execution;
    const _: fn(&tauri::AppHandle, &str, Option<&str>) = super::super::register_remote_origin;
    const _: fn(&tauri::AppHandle, &str, RemoteControlEvent) =
        super::super::track_remote_origin_event;
    const _: fn(&tauri::AppHandle, &str, &str, RemoteControlEvent) =
        super::super::track_remote_execution_event;
    const _: fn(&tauri::AppHandle, &str, &str) = super::super::track_remote_result_received;
    const _: fn(&tauri::AppHandle, &str, &str) = super::super::unregister_remote_execution;
    const _: fn(&tauri::AppHandle, &str, &str) = super::super::track_remote_task_output;

    #[test]
    fn restart_keeps_original_collection_lease_and_first_output_marker() {
        let temp = tempfile::tempdir().unwrap();
        let file = temp.path().join("remote.json");
        let lease = CollectionLease {
            generation: 7,
            collection_period_id: "original-period".into(),
        };
        let mut state = RemoteTelemetryState::default();
        state.executions.insert(
            "local-session".into(),
            RemoteExecution {
                task_id: "random-task".into(),
                mode: "code".into(),
                started_at: now_ms(),
                turn_id: Some("original-turn".into()),
                first_output: true,
                lease: lease.clone(),
            },
        );
        state.origins.insert(
            "random-task".into(),
            RemoteOrigin {
                mode: Some("code".into()),
                started_at: now_ms(),
                lease: lease.clone(),
            },
        );
        filesystem::atomic_write_private(&file, &serde_json::to_vec(&state).unwrap()).unwrap();
        let restored = load_state_at(&file);
        assert_eq!(restored.executions["local-session"].lease, lease);
        assert!(restored.executions["local-session"].first_output);
        assert_eq!(restored.origins["random-task"].lease.generation, 7);
        assert_ne!(
            restored.origins["random-task"].lease,
            CollectionLease {
                generation: 8,
                collection_period_id: "new-period".into()
            }
        );
    }

    #[test]
    fn corrupt_expired_and_oversized_contexts_do_not_resurrect_tasks() {
        let temp = tempfile::tempdir().unwrap();
        let file = temp.path().join("remote.json");
        std::fs::write(&file, b"broken").unwrap();
        assert!(load_state_at(&file).origins.is_empty());
        let mut state = RemoteTelemetryState::default();
        state.origins.insert(
            "old-task".into(),
            RemoteOrigin {
                mode: None,
                started_at: now_ms().saturating_sub(OUTBOX_RETENTION_MS + 1),
                lease: CollectionLease {
                    generation: 1,
                    collection_period_id: "old".into(),
                },
            },
        );
        std::fs::write(&file, serde_json::to_vec(&state).unwrap()).unwrap();
        assert!(load_state_at(&file).origins.is_empty());
        std::fs::File::create(&file)
            .unwrap()
            .set_len(8 * 1024 * 1024 + 1)
            .unwrap();
        assert!(load_state_at(&file).executions.is_empty());
    }
}
