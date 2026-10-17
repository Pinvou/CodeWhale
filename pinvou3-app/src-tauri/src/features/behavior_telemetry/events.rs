use super::*;

pub fn track<R: tauri::Runtime>(app: &tauri::AppHandle<R>, event: BehaviorEvent) {
    if let Some(telemetry) = app.try_state::<BehaviorTelemetry>() {
        telemetry.track(event);
    }
}

pub fn register_team_session(app: &tauri::AppHandle, session_id: &str) {
    if let Some(telemetry) = app.try_state::<BehaviorTelemetry>() {
        let Some(lease) = telemetry.current_lease() else {
            return;
        };
        if let Ok(mut sessions) = telemetry.team_sessions.lock() {
            if telemetry.lease_is_current(&lease) {
                sessions.insert(session_id.to_string(), lease);
            }
        }
    }
}

pub fn is_team_session(app: &tauri::AppHandle, session_id: &str) -> bool {
    app.try_state::<BehaviorTelemetry>()
        .is_some_and(|telemetry| {
            telemetry.team_sessions.lock().is_ok_and(|sessions| {
                sessions
                    .get(session_id)
                    .is_some_and(|lease| telemetry.lease_is_current(lease))
            })
        })
}

pub(super) fn task_fields(
    event: BehaviorEvent,
    session_id: &str,
    attempt: &TaskAttempt,
) -> BehaviorEvent {
    event
        .session(session_id)
        .string("attempt_id", &attempt.attempt_id)
        .string("mode", &attempt.mode)
        .string("initiator_type", &attempt.initiator_type)
        .input_type(&attempt.input_type)
        .boolean("has_attachment", attempt.has_attachment)
}

pub fn begin_task_submission(
    app: &tauri::AppHandle,
    session_id: &str,
    mode: &str,
    initiator_type: &str,
    input_type: &str,
    has_attachment: bool,
) -> TaskSubmissionGuard {
    let telemetry = app.try_state::<BehaviorTelemetry>();
    let lease = telemetry
        .as_ref()
        .and_then(|telemetry| telemetry.current_lease());
    let attempt = TaskAttempt {
        attempt_id: token("attempt_", 16),
        submitted_at: now_ms(),
        admitted_at: None,
        turn_id: None,
        mode: mode.to_string(),
        initiator_type: initiator_type.to_string(),
        input_type: input_type.to_string(),
        has_attachment,
        first_output_emitted: false,
        generation: lease.as_ref().map_or(0, |lease| lease.generation),
        collection_period_id: lease
            .as_ref()
            .map_or_else(String::new, |lease| lease.collection_period_id.clone()),
    };
    if let (Some(telemetry), Some(lease)) = (telemetry, lease) {
        let inserted = if let Ok(mut tasks) = telemetry.tasks.lock() {
            if telemetry.lease_is_current(&lease) {
                tasks.insert(session_id.to_string(), attempt.clone());
                let _ = persist_task_attempts(&tasks);
                true
            } else {
                false
            }
        } else {
            false
        };
        if inserted {
            telemetry.track_for_lease(
                task_fields(BehaviorEvent::new("task_submitted"), session_id, &attempt),
                &lease,
            );
        }
    }
    TaskSubmissionGuard {
        app: app.clone(),
        session_id: session_id.to_string(),
        attempt_id: attempt.attempt_id,
        reject_on_drop: true,
    }
}

pub fn begin_task_retry(
    app: &tauri::AppHandle,
    session_id: &str,
    mode: &str,
    input_type: &str,
    has_attachment: bool,
    retry_reason: &str,
) -> TaskSubmissionGuard {
    let source_turn = app.try_state::<BehaviorTelemetry>().and_then(|telemetry| {
        let record = telemetry
            .latest_turns
            .lock()
            .ok()?
            .get(session_id)
            .cloned()?;
        telemetry.lease_is_current(&record.lease).then_some(record)
    });
    let guard = begin_task_submission(app, session_id, mode, "user", input_type, has_attachment);
    let Some(source_turn) = source_turn else {
        return guard;
    };
    if let Some(telemetry) = app.try_state::<BehaviorTelemetry>() {
        let attempt = telemetry.tasks.lock().ok().and_then(|tasks| {
            tasks.get(session_id).cloned().filter(|attempt| {
                attempt.generation == source_turn.lease.generation
                    && attempt.collection_period_id == source_turn.lease.collection_period_id
                    && telemetry.lease_is_current(&attempt.lease())
            })
        });
        if let Some(attempt) = attempt {
            telemetry.track_for_lease(
                BehaviorEvent::new("task_retry_requested")
                    .session(session_id)
                    .string("attempt_id", &attempt.attempt_id)
                    .string("source_turn_id", source_turn.turn_id)
                    .string("retry_reason", retry_reason_key(retry_reason)),
                &attempt.lease(),
            );
        }
    }
    guard
}

/// Associates a voice operation with the task attempt that was actually
/// admitted by the chat command. The frontend never manufactures attempt IDs.
pub fn track_voice_message_submitted(
    app: &tauri::AppHandle,
    session_id: &str,
    operation_id: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let attempt = telemetry
        .tasks
        .lock()
        .map_err(|_| "behavior task state is unavailable".to_string())?
        .get(session_id)
        .cloned()
        .ok_or_else(|| "no active task attempt for voice submission".to_string())?;
    let lease = attempt.lease();
    if !telemetry.lease_is_current(&lease) {
        return Ok(());
    }
    let dedup_key = format!("{operation_id}:{}", attempt.attempt_id);
    let mut submissions = telemetry
        .voice_submissions
        .lock()
        .map_err(|_| "voice submission state is unavailable".to_string())?;
    if submissions.len() >= OUTBOX_MAX_EVENTS {
        submissions.clear();
    }
    if !telemetry.lease_is_current(&lease) || submissions.contains_key(&dedup_key) {
        return Ok(());
    }
    submissions.insert(dedup_key, lease.clone());
    drop(submissions);
    telemetry.track_for_lease(
        BehaviorEvent::new("voice_message_submitted")
            .session(session_id)
            .string("operation_id", operation_id)
            .string("attempt_id", attempt.attempt_id),
        &lease,
    );
    Ok(())
}

pub fn scene_registry_pair(scene_l1: &str, scene_l2: &str) -> (&'static str, &'static str) {
    match (scene_l1, scene_l2) {
        ("work", "personal-workbench") => ("work", "personal-workbench"),
        ("work", "document-writing") => ("work", "document-writing"),
        ("design", "poster") => ("design", "poster"),
        ("design", "data-visualization") => ("design", "data-visualization"),
        ("design", "ppt") => ("design", "ppt"),
        _ => ("unknown", "unknown"),
    }
}

/// Associates the selected scene with the attempt created by the authoritative
/// chat command. The frontend supplies only controlled scene keys and never an
/// attempt identifier.
pub fn track_scene_submitted(
    app: &tauri::AppHandle,
    session_id: &str,
    scene_l1: &str,
    scene_l2: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let attempt = telemetry
        .tasks
        .lock()
        .map_err(|_| "behavior task state is unavailable".to_string())?
        .get(session_id)
        .cloned()
        .ok_or_else(|| "no active task attempt for scene submission".to_string())?;
    let lease = attempt.lease();
    if !telemetry.lease_is_current(&lease) {
        return Ok(());
    }
    let dedup_key = format!("scene:{}", attempt.attempt_id);
    let mut submissions = telemetry
        .voice_submissions
        .lock()
        .map_err(|_| "scene submission state is unavailable".to_string())?;
    if submissions.len() >= OUTBOX_MAX_EVENTS {
        submissions.clear();
    }
    if !telemetry.lease_is_current(&lease) || submissions.contains_key(&dedup_key) {
        return Ok(());
    }
    submissions.insert(dedup_key, lease.clone());
    drop(submissions);
    let (scene_l1, scene_l2) = scene_registry_pair(scene_l1, scene_l2);
    telemetry.track_for_lease(
        BehaviorEvent::new("scene_submitted")
            .session(session_id)
            .string("attempt_id", attempt.attempt_id)
            .scene(scene_l1, scene_l2),
        &lease,
    );
    Ok(())
}

fn artifact_type_key(value: &str) -> &'static str {
    match value {
        "document" => "document",
        "spreadsheet" => "spreadsheet",
        "slides" => "slides",
        "pdf" => "pdf",
        "image" => "image",
        "code" => "code",
        "html" => "html",
        _ => "other",
    }
}

fn export_format_key(value: &str) -> &'static str {
    match value {
        "docx" => "docx",
        "xlsx" => "xlsx",
        "pptx" => "pptx",
        "pdf" => "pdf",
        "png" => "png",
        "html" => "html",
        "markdown" => "markdown",
        _ => "other",
    }
}

fn scheduled_subject_id(
    lease: &CollectionLease,
    namespace: &str,
    source_id: &str,
) -> Result<String, String> {
    let mut digest = Sha256::new();
    digest.update(b"pinvou-behavior-v1\0");
    digest.update(namespace.as_bytes());
    digest.update(b"\0");
    digest.update(lease.collection_period_id.as_bytes());
    digest.update(b"\0");
    digest.update(source_id.as_bytes());
    let hex = crate::platform::encoding::hex_lower(&digest.finalize());
    Ok(format!("{namespace}_{}", &hex[..24]))
}

fn feedback_action_key(value: &str) -> Result<&'static str, String> {
    match value {
        "set" => Ok("set"),
        "clear" => Ok("clear"),
        _ => Err("unsupported feedback action".to_string()),
    }
}

fn feedback_rating_key(value: Option<&str>) -> Result<Option<&'static str>, String> {
    match value {
        Some("positive") => Ok(Some("positive")),
        Some("negative") => Ok(Some("negative")),
        None => Ok(None),
        _ => Err("unsupported feedback rating".to_string()),
    }
}

fn feedback_reason_key(value: Option<&str>) -> Result<Option<&'static str>, String> {
    match value {
        Some("accurate") => Ok(Some("accurate")),
        Some("helpful") => Ok(Some("helpful")),
        Some("fast") => Ok(Some("fast")),
        Some("incorrect") => Ok(Some("incorrect")),
        Some("incomplete") => Ok(Some("incomplete")),
        Some("unsafe") => Ok(Some("unsafe")),
        Some("too_slow") => Ok(Some("too_slow")),
        Some("other") => Ok(Some("other")),
        None => Ok(None),
        _ => Err("unsupported feedback reason".to_string()),
    }
}

fn response_content_type_key(value: &str) -> &'static str {
    match value {
        "text" => "text",
        "code" => "code",
        "table" => "table",
        _ => "other",
    }
}

fn retry_reason_key(value: &str) -> &'static str {
    match value {
        "manual_regenerate" => "manual_regenerate",
        "previous_failed" => "previous_failed",
        "input_changed" => "input_changed",
        _ => "unknown",
    }
}

pub fn track_response_feedback_changed(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    action: &str,
    rating: Option<&str>,
    reason: Option<&str>,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let Some(lease) = telemetry.current_lease() else {
        return Ok(());
    };
    let action = feedback_action_key(action)?;
    let rating = feedback_rating_key(rating)?;
    let reason = feedback_reason_key(reason)?;
    if (action == "set") != rating.is_some() || (action == "clear" && reason.is_some()) {
        return Err("feedback action and state do not match".to_string());
    }
    let source = format!("{session_id}\u{1f}{turn_id}");
    let feedback_id = scheduled_subject_id(&lease, "feedback", &source)?;
    let mut event = BehaviorEvent::new("response_feedback_changed")
        .session(session_id)
        .turn(turn_id)
        .string("feedback_id", feedback_id)
        .string("feedback_action", action);
    if let Some(rating) = rating {
        event = event.string("rating", rating);
    }
    if let Some(reason) = reason {
        event = event.string("reason_code", reason);
    }
    telemetry.track_for_lease(event, &lease);
    Ok(())
}

pub fn track_response_copied(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    content_type: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let Some(lease) = telemetry.current_lease() else {
        return Ok(());
    };
    telemetry.track_for_lease(
        BehaviorEvent::new("response_copied")
            .session(session_id)
            .turn(turn_id)
            .string("content_type", response_content_type_key(content_type)),
        &lease,
    );
    Ok(())
}

fn scheduled_schedule_type(rrule: &str) -> &'static str {
    let rule = rrule.trim().to_ascii_uppercase();
    if rule.contains("FREQ=MONTHLY") {
        "monthly"
    } else if rule.contains("FREQ=WEEKLY") {
        let every_day = ["MO", "TU", "WE", "TH", "FR", "SA", "SU"]
            .iter()
            .all(|day| rule.contains(day));
        if every_day { "daily" } else { "weekly" }
    } else if rule.contains("FREQ=DAILY") {
        "daily"
    } else if rule.contains("FREQ=HOURLY") || rule.contains("FREQ=") {
        "cron"
    } else {
        "once"
    }
}

fn scheduled_task_type(value: &str) -> &'static str {
    match value.trim().to_ascii_lowercase().as_str() {
        "daily-brief" | "daily_brief" => "daily_brief",
        "follow-up-monitor" | "follow_up_monitor" => "follow_up_monitor",
        "weekly-review" | "weekly_review" => "weekly_review",
        "custom" => "custom",
        _ => "unknown",
    }
}

fn scheduled_status(value: &str) -> Result<&'static str, String> {
    match value {
        "completed" | "success" => Ok("success"),
        "failed" => Ok("failed"),
        "canceled" | "cancelled" => Ok("cancelled"),
        "interrupted" => Ok("interrupted"),
        _ => Err("scheduled run is not terminal".to_string()),
    }
}

pub fn track_scheduled_task_created(
    app: &tauri::AppHandle,
    source_task_id: &str,
    rrule: &str,
    task_type: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let Some(lease) = telemetry.current_lease() else {
        return Ok(());
    };
    let task_id = scheduled_subject_id(&lease, "scheduled", source_task_id)?;
    let mut event = BehaviorEvent::new("scheduled_task_created")
        .string("scheduled_task_id", &task_id)
        .string("schedule_type", scheduled_schedule_type(rrule))
        .string("task_type", scheduled_task_type(task_type));
    event.event_id = scheduled_subject_id(&lease, "scheduled_created", source_task_id)?;
    telemetry.track_for_lease(event, &lease);
    Ok(())
}

pub fn track_scheduled_run_finished(
    app: &tauri::AppHandle,
    source_task_id: &str,
    source_run_id: &str,
    status: &str,
    duration_ms: u64,
    category: Option<&str>,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let Some(lease) = telemetry.current_lease() else {
        return Ok(());
    };
    let status = scheduled_status(status)?;
    let task_id = scheduled_subject_id(&lease, "scheduled", source_task_id)?;
    let operation_id = scheduled_subject_id(&lease, "scheduled_run", source_run_id)?;
    let mut event = BehaviorEvent::new("scheduled_task_run_finished")
        .string("scheduled_task_id", task_id)
        .string("operation_id", operation_id)
        .integer("attempt_index", 1)
        .status(status)
        .integer("duration_ms", duration_ms.min(86_400_000));
    if status != "success" {
        event = event.string(
            "error_category",
            if status == "cancelled" {
                "user_cancelled"
            } else {
                error_category(category)
            },
        );
    }
    event.event_id = scheduled_subject_id(&lease, "scheduled_finished", source_run_id)?;
    telemetry.track_for_lease(event, &lease);
    Ok(())
}

pub fn track_artifact_created(
    app: &tauri::AppHandle,
    session_id: &str,
    artifact_id: &str,
    artifact_type: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let turn = telemetry
        .tasks
        .lock()
        .map_err(|_| "behavior task state is unavailable".to_string())?
        .get(session_id)
        .and_then(|attempt| {
            let turn_id = attempt.turn_id.clone()?;
            telemetry
                .lease_is_current(&attempt.lease())
                .then(|| TurnTelemetryRecord {
                    turn_id,
                    lease: attempt.lease(),
                })
        });
    let turn = turn
        .or_else(|| {
            telemetry
                .latest_turns
                .lock()
                .ok()
                .and_then(|turns| turns.get(session_id).cloned())
                .filter(|record| telemetry.lease_is_current(&record.lease))
        })
        .ok_or_else(|| "no admitted turn for artifact creation".to_string())?;
    let artifact_type = artifact_type_key(artifact_type);
    let record = ArtifactTelemetryRecord {
        session_id: session_id.to_string(),
        turn_id: turn.turn_id.clone(),
        artifact_type: artifact_type.to_string(),
        lease: turn.lease.clone(),
    };
    let mut artifacts = telemetry
        .artifacts
        .lock()
        .map_err(|_| "artifact telemetry state is unavailable".to_string())?;
    if !telemetry.lease_is_current(&turn.lease) || artifacts.contains_key(artifact_id) {
        return Ok(());
    }
    if artifacts.len() >= OUTBOX_MAX_EVENTS {
        artifacts.clear();
    }
    artifacts.insert(artifact_id.to_string(), record);
    drop(artifacts);
    telemetry.track_for_lease(
        BehaviorEvent::new("artifact_created")
            .session(session_id)
            .turn(turn.turn_id)
            .string("artifact_id", artifact_id)
            .string("artifact_type", artifact_type),
        &turn.lease,
    );
    Ok(())
}

pub fn track_artifact_exported(
    app: &tauri::AppHandle,
    artifact_id: &str,
    export_format: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let record = telemetry
        .artifacts
        .lock()
        .map_err(|_| "artifact telemetry state is unavailable".to_string())?
        .get(artifact_id)
        .cloned()
        .ok_or_else(|| "unknown artifact telemetry id".to_string())?;
    if !telemetry.lease_is_current(&record.lease) {
        return Ok(());
    }
    telemetry.track_for_lease(
        BehaviorEvent::new("artifact_exported")
            .session(record.session_id)
            .turn(record.turn_id)
            .string("artifact_id", artifact_id)
            .string("artifact_type", record.artifact_type)
            .string("export_format", export_format_key(export_format)),
        &record.lease,
    );
    Ok(())
}

pub fn reject_task_submission(
    app: &tauri::AppHandle,
    session_id: &str,
    attempt_id: &str,
    error_category: &str,
    stage: &str,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let attempt = telemetry.tasks.lock().ok().and_then(|mut tasks| {
        let removed = if tasks.get(session_id).is_some_and(|attempt| {
            attempt.attempt_id == attempt_id
                && attempt.admitted_at.is_none()
                && telemetry.lease_is_current(&attempt.lease())
        }) {
            tasks.remove(session_id)
        } else {
            None
        };
        if removed.is_some() {
            let _ = persist_task_attempts(&tasks);
        }
        removed
    });
    if let Some(attempt) = attempt {
        telemetry.track_for_lease(
            task_fields(BehaviorEvent::new("task_rejected"), session_id, &attempt)
                .string("error_category", error_category)
                .stage(stage),
            &attempt.lease(),
        );
    }
}

pub fn admit_task(app: &tauri::AppHandle, session_id: &str, turn_id: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let admitted_at = now_ms();
    let attempt = telemetry.tasks.lock().ok().and_then(|mut tasks| {
        let attempt = tasks.get_mut(session_id)?;
        if attempt.admitted_at.is_some() || !telemetry.lease_is_current(&attempt.lease()) {
            return None;
        }
        attempt.admitted_at = Some(admitted_at);
        attempt.turn_id = Some(turn_id.to_string());
        let result = attempt.clone();
        let _ = persist_task_attempts(&tasks);
        Some(result)
    });
    if let Some(attempt) = attempt {
        let lease = attempt.lease();
        if let Ok(mut turns) = telemetry.latest_turns.lock() {
            if !telemetry.lease_is_current(&lease) {
                return;
            }
            if turns.len() >= OUTBOX_MAX_EVENTS && !turns.contains_key(session_id) {
                turns.clear();
            }
            turns.insert(
                session_id.to_string(),
                TurnTelemetryRecord {
                    turn_id: turn_id.to_string(),
                    lease: lease.clone(),
                },
            );
        }
        telemetry.track_for_lease(
            task_fields(BehaviorEvent::new("task_admitted"), session_id, &attempt)
                .turn(turn_id)
                .integer(
                    "queue_duration_ms",
                    admitted_at.saturating_sub(attempt.submitted_at),
                ),
            &lease,
        );
    }
}

pub fn task_first_output(app: &tauri::AppHandle, session_id: &str, turn_id: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let now = now_ms();
    let attempt = telemetry.tasks.lock().ok().and_then(|mut tasks| {
        let attempt = tasks.get_mut(session_id)?;
        if attempt.turn_id.as_deref() != Some(turn_id)
            || attempt.first_output_emitted
            || !telemetry.lease_is_current(&attempt.lease())
        {
            return None;
        }
        attempt.first_output_emitted = true;
        let result = attempt.clone();
        let _ = persist_task_attempts(&tasks);
        Some(result)
    });
    if let Some(attempt) = attempt {
        telemetry.track_for_lease(
            task_fields(
                BehaviorEvent::new("task_first_output"),
                session_id,
                &attempt,
            )
            .turn(turn_id)
            .string("output_type", "text")
            .integer("duration_ms", now.saturating_sub(attempt.submitted_at)),
            &attempt.lease(),
        );
    }
}

pub fn request_task_cancel(app: &tauri::AppHandle, session_id: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let attempt = telemetry
        .tasks
        .lock()
        .ok()
        .and_then(|tasks| tasks.get(session_id).cloned());
    if let Some(attempt) = attempt.filter(|attempt| telemetry.lease_is_current(&attempt.lease())) {
        if let Some(turn_id) = attempt.turn_id.as_deref() {
            telemetry.track_for_lease(
                BehaviorEvent::new("task_cancel_requested")
                    .session(session_id)
                    .string("attempt_id", &attempt.attempt_id)
                    .turn(turn_id)
                    .stage(if attempt.first_output_emitted {
                        "output"
                    } else {
                        "model"
                    }),
                &attempt.lease(),
            );
        }
    }
}

pub fn finish_task(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    status: &str,
    error_category: Option<&str>,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let dangling_tools = telemetry
        .tool_calls
        .lock()
        .ok()
        .map(|mut calls| {
            let keys = calls
                .iter()
                .filter(|(key, operation)| {
                    key.starts_with(&format!("{session_id}\u{1f}")) && operation.turn_id == turn_id
                })
                .map(|(key, _)| key.clone())
                .collect::<Vec<_>>();
            keys.into_iter()
                .filter_map(|key| calls.remove(&key))
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    for operation in dangling_tools {
        if !telemetry.lease_is_current(&operation.lease) {
            continue;
        }
        telemetry.track_for_lease(
            BehaviorEvent::new("tool_call_completed")
                .session(session_id)
                .turn(&operation.turn_id)
                .string("operation_id", operation.operation_id)
                .integer("attempt_index", 1)
                .tool(
                    operation.tool_key,
                    &operation.tool_name,
                    operation.tool_type,
                )
                .success(false)
                .integer("duration_ms", now_ms().saturating_sub(operation.started_at))
                .string(
                    "error_category",
                    if matches!(status, "cancelled" | "interrupted") {
                        "user_cancelled"
                    } else {
                        "tool_error"
                    },
                ),
            &operation.lease,
        );
    }
    let attempt = telemetry.tasks.lock().ok().and_then(|mut tasks| {
        let removed = if tasks
            .get(session_id)
            .is_some_and(|attempt| attempt.turn_id.as_deref() == Some(turn_id))
            && tasks
                .get(session_id)
                .is_some_and(|attempt| telemetry.lease_is_current(&attempt.lease()))
        {
            tasks.remove(session_id)
        } else {
            None
        };
        if removed.is_some() {
            let _ = persist_task_attempts(&tasks);
        }
        removed
    });
    if let Some(attempt) = attempt {
        let mut event = task_fields(BehaviorEvent::new("task_finished"), session_id, &attempt)
            .turn(turn_id)
            .status(status)
            .integer("duration_ms", now_ms().saturating_sub(attempt.submitted_at));
        if status != "success" {
            event = event.string("error_category", error_category.unwrap_or("unknown"));
        }
        telemetry.track_for_lease(event, &attempt.lease());
    }
}

pub fn error_category(error: Option<&str>) -> &'static str {
    let error = error.unwrap_or_default().to_ascii_lowercase();
    if error.contains("cancel") || error.contains("interrupt") {
        "user_cancelled"
    } else if error.contains("permission") || error.contains("denied") {
        "permission_denied"
    } else if error.contains("timeout") {
        "timeout"
    } else if error.contains("auth") || error.contains("401") || error.contains("403") {
        "authentication_failed"
    } else if error.contains("network") || error.contains("connect") {
        "network_unavailable"
    } else if error.contains("rate limit") || error.contains("429") {
        "rate_limited"
    } else if error.contains("config") || error.contains("missing") {
        "configuration_missing"
    } else if error.contains("model") || error.contains("provider") {
        "model_error"
    } else {
        "internal_error"
    }
}

fn tool_operation_key(session_id: &str, call_id: &str) -> String {
    format!("{session_id}\u{1f}{call_id}")
}

pub fn track_tool_call_started(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    call_id: &str,
    tool_name: &str,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let Some(lease) = telemetry.current_lease() else {
        return;
    };
    let tool_type = classify_tool(tool_name);
    let tool_key = tool_registry_key(tool_name);
    let operation = ToolOperation {
        operation_id: token("toolop_", 16),
        started_at: now_ms(),
        turn_id: turn_id.to_string(),
        tool_key,
        tool_name: normalized_tool_name(tool_name, tool_key),
        tool_type,
        lease: lease.clone(),
    };
    if let Ok(mut calls) = telemetry.tool_calls.lock() {
        let key = tool_operation_key(session_id, call_id);
        if !telemetry.lease_is_current(&lease) || calls.contains_key(&key) {
            return;
        }
        calls.insert(key, operation.clone());
    } else {
        return;
    }
    telemetry.track_for_lease(
        BehaviorEvent::new("tool_call_started")
            .session(session_id)
            .turn(turn_id)
            .string("operation_id", &operation.operation_id)
            .integer("attempt_index", 1)
            .tool(operation.tool_key, &operation.tool_name, tool_type),
        &lease,
    );
}

pub fn track_tool_call_completed(
    app: &tauri::AppHandle,
    session_id: &str,
    _turn_id: &str,
    call_id: &str,
    _tool_name: &str,
    success: bool,
    error_category: Option<&str>,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let operation = telemetry
        .tool_calls
        .lock()
        .ok()
        .and_then(|mut calls| calls.remove(&tool_operation_key(session_id, call_id)));
    let Some(operation) = operation else {
        return;
    };
    if !telemetry.lease_is_current(&operation.lease) {
        return;
    }
    let mut event = BehaviorEvent::new("tool_call_completed")
        .session(session_id)
        .turn(operation.turn_id.clone())
        .string("operation_id", operation.operation_id)
        .integer("attempt_index", 1)
        .tool(
            operation.tool_key,
            &operation.tool_name,
            operation.tool_type,
        )
        .success(success)
        .integer("duration_ms", now_ms().saturating_sub(operation.started_at));
    if !success {
        event = event.string("error_category", error_category.unwrap_or("tool_error"));
    }
    telemetry.track_for_lease(event, &operation.lease);
}

pub fn begin_tool_install(
    app: &tauri::AppHandle,
    tool_name: &str,
    tool_type: &'static str,
) -> ToolInstallGuard {
    let started_at = now_ms();
    let operation_id = token("install_", 16);
    let tool_key = tool_registry_key(tool_name);
    let tool_name = normalized_tool_name(tool_name, tool_key);
    let lease = app
        .try_state::<BehaviorTelemetry>()
        .and_then(|telemetry| telemetry.current_lease());
    if let (Some(telemetry), Some(lease)) = (app.try_state::<BehaviorTelemetry>(), lease.as_ref()) {
        telemetry.track_for_lease(
            BehaviorEvent::new("tool_install_started")
                .tool(tool_key, &tool_name, tool_type)
                .string("operation_id", &operation_id)
                .integer("attempt_index", 1),
            lease,
        );
    }
    ToolInstallGuard {
        app: app.clone(),
        operation_id,
        started_at,
        tool_key,
        tool_name,
        tool_type,
        authorization_generation: lease.map(|current| current.generation),
        completed: false,
    }
}

pub fn track_model_used_keys(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    provider: &str,
    model: &str,
    selection_source: &str,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let attempt = telemetry.tasks.lock().ok().and_then(|tasks| {
        tasks
            .get(session_id)
            .filter(|attempt| {
                attempt.turn_id.as_deref() == Some(turn_id)
                    && telemetry.lease_is_current(&attempt.lease())
            })
            .cloned()
    });
    if let Some(attempt) = attempt {
        telemetry.track_for_lease(
            BehaviorEvent::new("model_used")
                .session(session_id)
                .string("attempt_id", &attempt.attempt_id)
                .turn(turn_id)
                .model(provider_registry_key(provider), model_registry_key(model))
                .string("selection_source", selection_source),
            &attempt.lease(),
        );
    }
}

pub fn track_model_selected(
    app: &tauri::AppHandle,
    provider: &str,
    model: &str,
    selection_scope: &str,
    entry_point: &str,
) {
    track(
        app,
        BehaviorEvent::new("model_selected")
            .model(provider_registry_key(provider), model_registry_key(model))
            .string("selection_scope", selection_scope)
            .string("entry_point", entry_point_registry_key(entry_point)),
    );
}

pub fn entry_point_registry_key(entry_point: &str) -> &'static str {
    match entry_point.trim().to_ascii_lowercase().as_str() {
        "onboarding" => "onboarding",
        "composer" => "composer",
        "sidebar" => "sidebar",
        "settings" => "settings",
        "marketplace" => "marketplace",
        "scene_gallery" => "scene_gallery",
        "context_menu" => "context_menu",
        "notification" => "notification",
        "deep_link" => "deep_link",
        _ => "unknown",
    }
}

pub fn provider_registry_key(provider: &str) -> &'static str {
    let key = match provider.trim().to_ascii_lowercase().as_str() {
        "deepseek" => "deepseek",
        "kimi" | "kimi_acp" => "kimi",
        "qwen" => "qwen",
        "doubao" => "doubao",
        "minimax" => "minimax",
        "glm" => "glm",
        "mimo" => "mimo",
        "openai" | "codex" | "codex_acp" => "openai",
        "anthropic" | "claude" | "claude_acp" => "anthropic",
        "gemini" => "gemini",
        "xai" => "xai",
        "local_vllm" => "local",
        "openai_compatible" => "custom",
        _ => "unknown",
    };
    controlled_registry_key(key, CONTROLLED_PROVIDER_KEYS, "unknown")
}

pub fn model_registry_key(model: &str) -> &'static str {
    let model = model.trim().to_ascii_lowercase();
    if model.is_empty() {
        return "unknown";
    }
    let key = if model.contains("deepseek") {
        "deepseek"
    } else if model.contains("claude") && model.contains("opus") {
        "claude_opus"
    } else if model.contains("claude") && model.contains("haiku") {
        "claude_haiku"
    } else if model.contains("claude") {
        "claude_sonnet"
    } else if model.contains("gpt-5") || model.contains("gpt_5") {
        "gpt_5"
    } else if model.contains("gpt-4") || model.contains("gpt_4") {
        "gpt_4"
    } else if model == "o3" || model.starts_with("o3-") {
        "o3"
    } else if model.contains("gemini") {
        "gemini"
    } else if model.contains("qwen") {
        "qwen"
    } else if model.contains("kimi") || model.contains("moonshot") {
        "kimi"
    } else if model.contains("minimax") {
        "minimax"
    } else if model.contains("glm") {
        "glm"
    } else if model.contains("mimo") {
        "mimo"
    } else if model.contains("doubao") {
        "doubao"
    } else if model.contains("grok") {
        "grok"
    } else if model.contains("llama") {
        "llama"
    } else if model.contains("mistral") {
        "mistral"
    } else {
        "custom"
    };
    controlled_registry_key(key, CONTROLLED_MODEL_KEYS, "unknown")
}

pub fn classify_tool(tool_name: &str) -> &'static str {
    if tool_name.starts_with("mcp_") || tool_name.contains("-mcp") {
        "mcp"
    } else if matches!(tool_name, "Bash" | "exec_shell" | "task_shell_start") {
        "cli"
    } else if matches!(tool_name, "load_skill" | "tool_search") {
        "skill"
    } else {
        "unknown"
    }
}

pub fn tool_registry_key(tool_name: &str) -> &'static str {
    let key = match tool_name {
        "Bash" | "exec_shell" | "task_shell_start" => "shell",
        "read_file" => "read_file",
        "write_file" | "apply_patch" => "edit_file",
        "glob" | "rg" | "search_files" => "search_files",
        "web_search" | "search_query" => "web_search",
        "view_image" => "view_image",
        "image_gen" | "imagegen" => "image_generation",
        "load_skill" => "load_skill",
        "tool_search" => "tool_search",
        "request_user_input" => "request_user_input",
        "update_plan" | "checklist_write" | "todo_write" => "update_plan",
        "feishu" => "feishu",
        "wecom" => "wecom",
        "dingtalk" => "dingtalk",
        "tmeet" => "tmeet",
        "weather" => "weather",
        "iwencai" => "iwencai",
        "ima" => "ima",
        "tencent-docs" => "tencent_docs",
        "wecom-bot" => "wecom_bot",
        "eip" => "eip",
        "zhidao" => "zhidao",
        "qcc" => "qcc",
        "patsnap-search" => "patsnap_search",
        "canva-mcp" => "canva",
        "obsidian" => "obsidian",
        "yuandian-mcp" => "yuandian",
        "gongwen" => "gongwen",
        "pptx" => "pptx",
        name if name.starts_with("mcp_") || name.contains("-mcp") => "mcp_custom",
        _ => "custom",
    };
    controlled_registry_key(key, CONTROLLED_TOOL_KEYS, "unknown")
}

pub fn normalized_tool_name(tool_name: &str, fallback: &str) -> String {
    let name = tool_name.trim().to_ascii_lowercase();
    let valid = !name.is_empty()
        && name.len() <= 96
        && name.bytes().enumerate().all(|(index, byte)| {
            byte.is_ascii_lowercase()
                || byte.is_ascii_digit()
                || (index > 0 && matches!(byte, b'_' | b'.' | b'-'))
        });
    if valid { name } else { fallback.to_string() }
}

fn controlled_registry_key(
    candidate: &'static str,
    registry: &[&str],
    fallback: &'static str,
) -> &'static str {
    registry
        .contains(&candidate)
        .then_some(candidate)
        .unwrap_or(fallback)
}
