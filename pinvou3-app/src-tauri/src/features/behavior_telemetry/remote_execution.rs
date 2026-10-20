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
    // In-flight admission snapshots cannot survive an explicit local deletion.
    // Tokens never cross process boundaries, so this epoch is not persisted.
    #[serde(skip)]
    registration_epoch: Arc<()>,
}

pub(crate) struct RemoteExecutionLease {
    lease: CollectionLease,
    registration_epoch: Arc<()>,
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

fn retain_newest<T>(rows: &mut HashMap<String, T>, limit: usize, started_at: impl Fn(&T) -> u64) {
    if rows.len() <= limit {
        return;
    }
    if rows.len() == limit + 1 {
        let oldest = rows
            .iter()
            .min_by_key(|(key, row)| (started_at(row), *key))
            .map(|(key, _)| key.clone());
        if let Some(key) = oldest {
            rows.remove(&key);
        }
        return;
    }
    let mut oldest: Vec<_> = rows
        .iter()
        .map(|(key, row)| (started_at(row), key.clone()))
        .collect();
    oldest.sort_unstable();
    for (_, key) in oldest.into_iter().take(rows.len() - limit) {
        rows.remove(&key);
    }
}

fn prune(state: &mut RemoteTelemetryState) -> bool {
    let before = (state.executions.len(), state.origins.len());
    let cutoff = now_ms().saturating_sub(OUTBOX_RETENTION_MS);
    state.executions.retain(|_, e| e.started_at >= cutoff);
    state.origins.retain(|_, e| e.started_at >= cutoff);
    retain_newest(&mut state.executions, OUTBOX_MAX_EVENTS, |e| e.started_at);
    retain_newest(&mut state.origins, OUTBOX_MAX_EVENTS, |e| e.started_at);
    before != (state.executions.len(), state.origins.len())
}

fn insert_origin(
    contexts: &mut RemoteTelemetryState,
    task: &str,
    mode: Option<&str>,
    lease: CollectionLease,
) -> bool {
    let changed = prune(contexts);
    // A retry must neither refresh authorization nor evict another task.
    if contexts.origins.contains_key(task) {
        return changed;
    }
    retain_newest(&mut contexts.origins, OUTBOX_MAX_EVENTS - 1, |e| {
        e.started_at
    });
    contexts.origins.insert(
        task.to_owned(),
        RemoteOrigin {
            mode: mode.map(str::to_owned),
            started_at: now_ms(),
            lease,
        },
    );
    true
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
    let mut changed = false;
    for execution in contexts.executions.values_mut() {
        if execution.lease.generation == generation
            && execution.lease.collection_period_id != period
        {
            execution.lease.collection_period_id = period.to_owned();
            changed = true;
        }
    }
    for origin in contexts.origins.values_mut() {
        if origin.lease.generation == generation && origin.lease.collection_period_id != period {
            origin.lease.collection_period_id = period.to_owned();
            changed = true;
        }
    }
    if changed {
        persist(&contexts)?;
    }
    Ok(())
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
        if !telemetry.lease_is_current(&lease) {
            return;
        }
        if insert_origin(&mut contexts, task, mode, lease) {
            let _ = persist(&contexts);
        }
    }
}

pub(crate) fn track_remote_result_received(app: &tauri::AppHandle, task: &str, status: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let Ok(contexts) = telemetry.remote_executions.lock() else {
        return;
    };
    let Some(origin) = contexts.origins.get(task) else {
        return;
    };
    // Hold the context lock through enqueue. Deletion clears contexts before
    // the outbox, so an already-running observer cannot enqueue after deletion.
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
    if let Ok(contexts) = telemetry.remote_executions.lock() {
        if let Some(origin) = contexts.origins.get(task) {
            telemetry.track_for_lease(event.into_behavior(), &origin.lease);
        }
    }
}

pub(crate) fn capture_remote_execution_lease(
    app: &tauri::AppHandle,
) -> Option<RemoteExecutionLease> {
    let telemetry = app.try_state::<BehaviorTelemetry>()?;
    capture_execution_lease(&telemetry)
}

pub(super) fn capture_execution_lease(
    telemetry: &BehaviorTelemetry,
) -> Option<RemoteExecutionLease> {
    let contexts = telemetry.remote_executions.lock().ok()?;
    Some(RemoteExecutionLease {
        lease: telemetry.current_lease()?,
        registration_epoch: contexts.registration_epoch.clone(),
    })
}

pub(crate) fn register_remote_execution_with_lease(
    app: &tauri::AppHandle,
    session: &str,
    task: &str,
    mode: &str,
    lease: &RemoteExecutionLease,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    register_execution(&telemetry, session, task, mode, lease);
}

pub(super) fn register_execution(
    telemetry: &BehaviorTelemetry,
    session: &str,
    task: &str,
    mode: &str,
    token: &RemoteExecutionLease,
) {
    if let Ok(mut contexts) = telemetry.remote_executions.lock() {
        if !Arc::ptr_eq(&contexts.registration_epoch, &token.registration_epoch)
            || !telemetry.lease_is_current(&token.lease)
        {
            return;
        }
        let mut changed = prune(&mut contexts);
        let executions = &mut contexts.executions;
        let before = executions.len();
        executions.retain(|_, execution| telemetry.lease_is_current(&execution.lease));
        changed |= before != executions.len();
        if executions.get(session).is_some_and(|e| e.task_id == task) {
            if changed {
                let _ = persist(&contexts);
            }
            return;
        }
        if !executions.contains_key(session) {
            retain_newest(executions, OUTBOX_MAX_EVENTS - 1, |e| e.started_at);
        }
        executions.insert(
            session.to_owned(),
            RemoteExecution {
                task_id: task.to_owned(),
                mode: mode.to_owned(),
                started_at: now_ms(),
                turn_id: None,
                first_output: false,
                lease: token.lease.clone(),
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
            let _ = persist(&contexts);
        }
    }
}

/// Called by the task owner before engine observers run, never by a generic
/// session event. Local turns in an admitted task's session cannot claim it.
pub(crate) fn bind_remote_execution_turn(
    app: &tauri::AppHandle,
    session: &str,
    task: &str,
    turn: &str,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    bind_execution_turn(&telemetry, session, task, turn);
}

pub(super) fn bind_execution_turn(
    telemetry: &BehaviorTelemetry,
    session: &str,
    task: &str,
    turn: &str,
) {
    if turn.is_empty() {
        return;
    }
    if let Ok(mut contexts) = telemetry.remote_executions.lock() {
        let Some(execution) = contexts.executions.get_mut(session) else {
            return;
        };
        if execution.task_id != task
            || execution.turn_id.is_some()
            || !telemetry.lease_is_current(&execution.lease)
        {
            return;
        }
        execution.turn_id = Some(turn.to_owned());
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
    track_execution_event(&telemetry, session, task, event);
}

pub(super) fn track_execution_event(
    telemetry: &BehaviorTelemetry,
    session: &str,
    task: &str,
    event: RemoteControlEvent,
) {
    if let Ok(contexts) = telemetry.remote_executions.lock() {
        if let Some(execution) = contexts
            .executions
            .get(session)
            .filter(|e| e.task_id == task)
        {
            telemetry.track_for_lease(event.into_behavior(), &execution.lease);
        }
    }
}

/// An executor's task-bound stream is also a first-output witness, including
/// during reconnect. A late chunk cannot attach to a new task in the session.
pub(crate) fn track_remote_task_output(app: &tauri::AppHandle, session: &str, task: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let Ok(mut contexts) = telemetry.remote_executions.lock() else {
        return;
    };
    let Some(execution) = contexts.executions.get_mut(session) else {
        return;
    };
    if execution.task_id != task
        || execution.first_output
        || !telemetry.lease_is_current(&execution.lease)
    {
        return;
    }
    execution.first_output = true;
    let execution = execution.clone();
    let _ = persist(&contexts);
    telemetry.track_for_lease(
        RemoteControlEvent::new(RemoteStage::FirstOutput, RemoteOutcome::Ok)
            .task(task, true, Some(&execution.mode))
            .duration_ms(now_ms().saturating_sub(execution.started_at))
            .into_behavior(),
        &execution.lease,
    );
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
    observe_execution(&telemetry, session, turn, stage, model)
}

pub(super) fn observe_execution(
    telemetry: &BehaviorTelemetry,
    session: &str,
    turn: &str,
    stage: RemoteStage,
    model: Option<(&str, &str)>,
) -> bool {
    let Ok(mut contexts) = telemetry.remote_executions.lock() else {
        return false;
    };
    let Some(execution) = contexts.executions.get_mut(session) else {
        return false;
    };
    if execution.turn_id.as_deref() != Some(turn) || !telemetry.lease_is_current(&execution.lease) {
        // Let genuine local turns use the ordinary task/model observer path.
        return false;
    }
    if stage == RemoteStage::FirstOutput && execution.first_output {
        return true;
    }
    if stage == RemoteStage::FirstOutput {
        execution.first_output = true;
    }
    let execution = execution.clone();
    if stage == RemoteStage::FirstOutput {
        let _ = persist(&contexts);
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
    const _: fn(&tauri::AppHandle, &str, &str, &str) = super::super::bind_remote_execution_turn;
    const _: fn(&tauri::AppHandle) -> Option<RemoteExecutionLease> =
        super::super::capture_remote_execution_lease;
    const _: fn(&tauri::AppHandle, &str, &str, &str, &RemoteExecutionLease) =
        super::super::register_remote_execution_with_lease;
    const _: fn(&tauri::AppHandle, &str, Option<&str>) = super::super::register_remote_origin;
    const _: fn(&tauri::AppHandle, &str, RemoteControlEvent) =
        super::super::track_remote_origin_event;
    const _: fn(&tauri::AppHandle, &str, &str, RemoteControlEvent) =
        super::super::track_remote_execution_event;
    const _: fn(&tauri::AppHandle, &str, &str) = super::super::track_remote_result_received;
    const _: fn(&tauri::AppHandle, &str, &str) = super::super::unregister_remote_execution;
    const _: fn(&tauri::AppHandle, &str, &str) = super::super::track_remote_task_output;

    #[test]
    fn full_origin_history_accepts_new_work_without_refreshing_retry_authorization() {
        let now = now_ms();
        let lease = CollectionLease {
            generation: 7,
            collection_period_id: "original".into(),
        };
        let mut state = RemoteTelemetryState::default();
        for index in 0..OUTBOX_MAX_EVENTS {
            state.origins.insert(
                format!("task-{index}"),
                RemoteOrigin {
                    mode: None,
                    started_at: now - (OUTBOX_MAX_EVENTS - index) as u64,
                    lease: lease.clone(),
                },
            );
        }
        let original = serde_json::to_value(&state).unwrap();
        let next_lease = CollectionLease {
            generation: 8,
            collection_period_id: "next-consent".into(),
        };
        assert!(!insert_origin(
            &mut state,
            "task-0",
            Some("code"),
            next_lease
        ));
        assert_eq!(serde_json::to_value(&state).unwrap(), original);
        assert!(insert_origin(
            &mut state,
            "new-task",
            Some("chat"),
            lease.clone()
        ));
        assert_eq!(state.origins.len(), OUTBOX_MAX_EVENTS);
        assert!(!state.origins.contains_key("task-0"));
        assert!(state.origins.contains_key("task-1"));
        assert_eq!(state.origins["new-task"].lease, lease);
    }

    #[test]
    fn oversized_contexts_evict_only_oldest_rows_in_each_independent_table() {
        let now = now_ms();
        let lease = CollectionLease {
            generation: 1,
            collection_period_id: "period".into(),
        };
        let mut state = RemoteTelemetryState::default();
        for index in 0..OUTBOX_MAX_EVENTS + 2 {
            state.origins.insert(
                format!("task-{index}"),
                RemoteOrigin {
                    mode: None,
                    started_at: now - (OUTBOX_MAX_EVENTS + 2 - index) as u64,
                    lease: lease.clone(),
                },
            );
        }
        state.executions.insert(
            "running-session".into(),
            RemoteExecution {
                task_id: "running-task".into(),
                mode: "assistant".into(),
                started_at: now,
                turn_id: Some("owned-turn".into()),
                first_output: false,
                lease,
            },
        );
        assert!(prune(&mut state));
        assert_eq!(state.origins.len(), OUTBOX_MAX_EVENTS);
        assert!(!state.origins.contains_key("task-0"));
        assert!(!state.origins.contains_key("task-1"));
        assert!(state.origins.contains_key("task-2"));
        assert_eq!(state.executions.len(), 1);
        assert_eq!(
            state.executions["running-session"].turn_id.as_deref(),
            Some("owned-turn")
        );
        assert!(!prune(&mut state));
    }

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
