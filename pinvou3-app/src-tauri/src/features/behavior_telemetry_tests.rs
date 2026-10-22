use super::*;
use std::ffi::OsString;
use std::sync::MutexGuard;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

struct TestHome {
    _lock: MutexGuard<'static, ()>,
    previous: Option<OsString>,
    _directory: tempfile::TempDir,
}

impl TestHome {
    fn new() -> Self {
        let lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let directory = tempfile::tempdir().unwrap();
        let previous = std::env::var_os("PINVOU3_HOME");
        // SAFETY: ENV_LOCK serializes every environment mutation in this test module.
        unsafe { std::env::set_var("PINVOU3_HOME", directory.path()) };
        Self {
            _lock: lock,
            previous,
            _directory: directory,
        }
    }
}

impl Drop for TestHome {
    fn drop(&mut self) {
        match self.previous.take() {
            // SAFETY: TestHome retains ENV_LOCK until after this Drop completes.
            Some(value) => unsafe { std::env::set_var("PINVOU3_HOME", value) },
            // SAFETY: TestHome retains ENV_LOCK until after this Drop completes.
            None => unsafe { std::env::remove_var("PINVOU3_HOME") },
        }
    }
}

struct EnvRestore<'a> {
    _home: &'a TestHome,
    values: Vec<(&'static str, Option<OsString>)>,
}

impl<'a> EnvRestore<'a> {
    fn set(home: &'a TestHome, values: &[(&'static str, &str)]) -> Self {
        let previous = values
            .iter()
            .map(|(key, value)| {
                let old = std::env::var_os(key);
                // SAFETY: `home` proves this test owns ENV_LOCK for the restore lifetime.
                unsafe { std::env::set_var(key, value) };
                (*key, old)
            })
            .collect();
        Self {
            _home: home,
            values: previous,
        }
    }

    fn unset(home: &'a TestHome, keys: &[&'static str]) -> Self {
        let previous = keys
            .iter()
            .map(|key| {
                let old = std::env::var_os(key);
                // SAFETY: `home` proves this test owns ENV_LOCK for the restore lifetime.
                unsafe { std::env::remove_var(key) };
                (*key, old)
            })
            .collect();
        Self {
            _home: home,
            values: previous,
        }
    }
}

impl Drop for EnvRestore<'_> {
    fn drop(&mut self) {
        for (key, value) in self.values.drain(..).rev() {
            match value {
                // SAFETY: `_home` keeps TestHome (and ENV_LOCK) alive through this Drop.
                Some(value) => unsafe { std::env::set_var(key, value) },
                // SAFETY: `_home` keeps TestHome (and ENV_LOCK) alive through this Drop.
                None => unsafe { std::env::remove_var(key) },
            }
        }
    }
}

fn test_runtime(base_url: &str) -> RuntimeConfig {
    RuntimeConfig {
        enabled: true,
        telemetry_base_url: base_url.to_string(),
        behavior_events_url: format!("{base_url}/events"),
        enrollment_token: Some("test-enrollment-token-1234567890".to_string()),
        app_version: "0.0.0-test".to_string(),
        platform: "test".to_string(),
        arch: "test".to_string(),
        channel: "dev".to_string(),
        locale: "en".to_string(),
    }
}

fn test_telemetry(runtime: RuntimeConfig, state: PersistedState) -> BehaviorTelemetry {
    let active_period = state.control.collection_period_id.clone();
    let generation = state.control.generation;
    BehaviorTelemetry {
        client: reqwest::Client::builder()
            .timeout(Duration::from_secs(3))
            .build()
            .unwrap(),
        runtime: Arc::new(runtime),
        state: Arc::new(Mutex::new(Some(state))),
        state_healthy: Arc::new(AtomicBool::new(true)),
        credential_gate: Arc::new(Mutex::new(())),
        control_gate: Arc::new(Mutex::new(())),
        collecting: Arc::new(AtomicBool::new(active_period.is_some())),
        authorization_generation: Arc::new(AtomicU64::new(generation)),
        credential_renewal_in_progress: Arc::new(AtomicBool::new(false)),
        collection_period_id: Arc::new(RwLock::new(active_period)),
        outbox: Arc::new(StdMutex::new(Outbox::empty(outbox_path()))),
        tasks: Arc::new(StdMutex::new(HashMap::new())),
        diagnostics: Arc::new(StdMutex::new(Default::default())),
        team_sessions: Arc::new(StdMutex::new(HashMap::new())),
        tool_calls: Arc::new(StdMutex::new(HashMap::new())),
        voice_submissions: Arc::new(StdMutex::new(HashMap::new())),
        artifacts: Arc::new(StdMutex::new(HashMap::new())),
        latest_turns: Arc::new(StdMutex::new(HashMap::new())),
        tool_install_terminals: ToolInstallTerminalCoordinator::default(),
        event_tx: None,
        control_tx: None,
        heartbeat_tx: None,
        report_tx: None,
    }
}

#[test]
fn client_privacy_allowlist_keeps_diagnostic_delegation_bounded() {
    let schema: Value = serde_json::from_str(include_str!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../docs/schemas/behavior-telemetry-v1.schema.json"
    )))
    .unwrap();
    for field in schema["x-client-optional-fields"].as_array().unwrap() {
        let field = field.as_str().unwrap();
        assert!(client_event_field_allowed(field), "{field}");
        assert_eq!(
            diagnostics::known_field(field),
            field.starts_with("diagnostic_")
        );
    }
    for field in schema["x-server-injected-fields"].as_array().unwrap() {
        assert!(!client_event_field_allowed(field.as_str().unwrap()));
    }
    for field in [
        "message",
        "content",
        "file_path",
        "raw_error",
        "diagnostic_raw_error",
    ] {
        assert!(!client_event_field_allowed(field), "{field}");
        assert!(
            validate_client_event_fields(
                &BehaviorEvent::new("task_finished").string(field, "private fixture")
            )
            .is_err()
        );
    }
    // Known field names alone do not authorize orphaned or untyped evidence.
    assert!(
        validate_client_event_fields(
            &BehaviorEvent::new("task_finished").string("diagnostic_steps", "private fixture")
        )
        .is_err()
    );
}

fn diagnostic_failure(session: &str, turn: &str) -> BehaviorEvent {
    BehaviorEvent::new("task_finished")
        .session(session)
        .turn(turn)
        .string("attempt_id", "attempt_diagnostic_test")
        .string("mode", "assistant")
        .string("initiator_type", "user")
        .input_type("text")
        .boolean("has_attachment", false)
        .status("failed")
        .integer("duration_ms", 1000)
        .string("error_category", "model_error")
        .diagnostic_error(
            "HTTP 429 private-user-prompt api_key=not-a-real-key",
            "assistant_error",
        )
}

#[test]
fn diagnostic_metadata_sampling_and_original_metrics_are_independent() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    for index in 0..7 {
        let session = format!("session_diagnostic_{index}");
        telemetry.track(
            BehaviorEvent::new("model_used")
                .session(&session)
                .turn("turn_diagnostic")
                .string("attempt_id", "attempt_diagnostic")
                .model("openai", "gpt_5")
                .string("selection_source", "session"),
        );
        telemetry.track(diagnostic_failure(&session, "turn_diagnostic"));
    }
    let outbox = telemetry.outbox.lock().unwrap();
    let failures: Vec<_> = outbox
        .records
        .iter()
        .filter(|r| r.event["event_name"] == "task_finished")
        .collect();
    assert_eq!(failures.len(), 7);
    assert_eq!(
        failures
            .iter()
            .filter(|r| r.event["diagnostic_sampled"] == true)
            .count(),
        5
    );
    assert_eq!(
        failures[0].event["diagnostic_environment"]["model_key"],
        "gpt_5"
    );
    assert_eq!(failures[0].event["diagnostic_code"], "rate_limited");
    assert_eq!(failures[0].event["diagnostic_http_status"], 429);
    assert!(failures[6].event.get("diagnostic_steps").is_none());
    let serialized = serde_json::to_string(&failures[0].event).unwrap();
    assert!(!serialized.contains("private-user-prompt"));
    assert!(!serialized.contains("not-a-real-key"));
    assert!(
        !std::fs::read_to_string(paths::pinvou3_home().join("failure-diagnostics.json"))
            .unwrap()
            .contains("private-user-prompt")
    );
    drop(outbox);
    let lease = telemetry.current_lease().unwrap();
    telemetry.collecting.store(false, Ordering::Release);
    assert!(!telemetry.try_track_for_lease(diagnostic_failure("old", "old"), &lease));
    assert_eq!(
        diagnostics::diagnostic_incidents(&telemetry).unwrap()["incidents"],
        json!([])
    );
}

#[test]
fn diagnostic_preview_requires_exact_current_authorization_and_survives_restart_once_confirmed() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let failure = diagnostic_failure("session_diagnostic", "turn_diagnostic");
    let id = failure.event_id.clone();
    telemetry.track(failure);
    let preview = |notes: &str| {
        diagnostics::preview_diagnostic(
            &telemetry,
            DiagnosticPreviewRequest {
                incident_id: id.clone(),
                notes: notes.into(),
                attachments: vec![],
            },
        )
        .unwrap()
    };
    let first = preview("Authorization: Bearer fake-secret\nreproduce empty response");
    assert!(
        !first["report"]["notes"]
            .as_str()
            .unwrap()
            .contains("fake-secret")
    );
    assert!(
        diagnostics::confirm_diagnostic(
            &telemetry,
            first["preview_token"].as_str().unwrap(),
            false
        )
        .is_err()
    );
    let second = preview("new reproduction");
    assert!(
        diagnostics::confirm_diagnostic(&telemetry, first["preview_token"].as_str().unwrap(), true)
            .is_err()
    );
    let confirmation = diagnostics::confirm_diagnostic(
        &telemetry,
        second["preview_token"].as_str().unwrap(),
        true,
    )
    .unwrap();
    assert_eq!(
        confirmation,
        diagnostics::confirm_diagnostic(
            &telemetry,
            second["preview_token"].as_str().unwrap(),
            true
        )
        .unwrap()
    );
    *telemetry.diagnostics.lock().unwrap() = diagnostics::load();
    assert_eq!(
        diagnostics::diagnostic_incidents(&telemetry).unwrap()["reports"]
            .as_array()
            .unwrap()
            .len(),
        1
    );
    let third = preview("after restart");
    diagnostics::rebind(&telemetry, 0, "renewed_collection_period").unwrap();
    assert!(
        diagnostics::confirm_diagnostic(&telemetry, third["preview_token"].as_str().unwrap(), true)
            .is_err()
    );
    diagnostics::clear(&telemetry).unwrap();
    *telemetry.diagnostics.lock().unwrap() = diagnostics::load();
    assert_eq!(
        diagnostics::diagnostic_incidents(&telemetry).unwrap()["reports"],
        json!([])
    );
}

#[test]
fn diagnostic_renewal_freezes_confirmed_bytes_and_restores_legacy_event_origin() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let failure = diagnostic_failure("renew_session", "renew_turn");
    let id = failure.event_id.clone();
    telemetry.track(failure);
    let preview = diagnostics::preview_diagnostic(
        &telemetry,
        DiagnosticPreviewRequest {
            incident_id: id.clone(),
            notes: "reproduce empty response".into(),
            attachments: vec![],
        },
    )
    .unwrap();
    diagnostics::confirm_diagnostic(&telemetry, preview["preview_token"].as_str().unwrap(), true)
        .unwrap();
    let original = preview["report"].clone();
    // Simulate an earlier client's defective rebind: the queued wire period
    // already moved, but the local incident and confirmed bytes are intact.
    {
        let mut outbox = telemetry.outbox.lock().unwrap();
        let event = outbox.records[0].event.as_object_mut().unwrap();
        event.remove("diagnostic_origin_period_id");
        event.insert(
            "collection_period_id".into(),
            json!("already_renewed_period"),
        );
    }
    let lease = telemetry.current_lease().unwrap();
    let origins = diagnostics::origin_periods(&telemetry, lease.generation).unwrap();
    telemetry
        .outbox
        .lock()
        .unwrap()
        .rebind_period("second_renewed_period", lease.generation, &origins)
        .unwrap();
    diagnostics::rebind(&telemetry, lease.generation, "second_renewed_period").unwrap();
    let event = telemetry.outbox.lock().unwrap().records[0].event.clone();
    assert_eq!(event["collection_period_id"], "second_renewed_period");
    assert_eq!(
        event["diagnostic_origin_period_id"],
        original["incident_period_id"]
    );
    let reports: Value = serde_json::from_slice(
        &std::fs::read(paths::pinvou3_home().join("failure-diagnostic-reports.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(reports[0]["payload"], original);
    assert_eq!(
        reports[0]["lease"]["collection_period_id"],
        "second_renewed_period"
    );
    assert!(
        diagnostics::origin_periods(&telemetry, lease.generation + 1)
            .unwrap()
            .is_empty()
    );
    let mut conflicting = origins;
    conflicting.insert(id, "conflicting_origin_period".into());
    let mut outbox = telemetry.outbox.lock().unwrap();
    assert!(
        outbox
            .rebind_period("third_renewed_period", lease.generation, &conflicting)
            .is_err()
    );
    assert_eq!(outbox.records[0].event, event);
}

#[tokio::test(flavor = "current_thread")]
async fn diagnostic_renewal_checkpoint_survives_each_interrupted_write_on_real_startup() {
    for stage in 0..4 {
        let home = TestHome::new();
        let (base, heartbeat_server) = mock_http(vec![(200, "{}")]).await;
        let (events_base, _events_seen, release_events, events_server) =
            controlled_http(503, "{}").await;
        let events_url = format!("{events_base}/events");
        let _env = EnvRestore::set(
            &home,
            &[
                ("PINVOU_BEHAVIOR_TELEMETRY_ENABLED", "true"),
                ("PINVOU_TELEMETRY_BASE_URL", &base),
                ("PINVOU_BEHAVIOR_EVENTS_URL", &events_url),
                (
                    "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
                    "test-enrollment-token-1234567890",
                ),
            ],
        );
        let runtime = RuntimeConfig::from_env();
        let mut state = heartbeat_state(&runtime);
        let telemetry = test_telemetry(runtime, state.clone());
        let failure = diagnostic_failure("checkpoint_session", "checkpoint_turn");
        let incident_id = failure.event_id.clone();
        telemetry.track(failure);
        let preview = diagnostics::preview_diagnostic(
            &telemetry,
            DiagnosticPreviewRequest {
                incident_id,
                notes: "reproduce empty response".into(),
                attachments: vec![],
            },
        )
        .unwrap();
        diagnostics::confirm_diagnostic(
            &telemetry,
            preview["preview_token"].as_str().unwrap(),
            true,
        )
        .unwrap();
        let frozen = preview["report"].clone();
        state.device_token = Some("renewed_checkpoint_token".into());
        state.control.collection_period_id = Some("renewed_checkpoint_period".into());
        state.credential_renewal_checkpoint = Some(CollectionLease {
            generation: state.control.generation,
            collection_period_id: "renewed_checkpoint_period".into(),
        });
        persist_state(&state).unwrap();
        if stage >= 1 {
            let origins =
                diagnostics::origin_periods(&telemetry, state.control.generation).unwrap();
            telemetry
                .outbox
                .lock()
                .unwrap()
                .rebind_period(
                    "renewed_checkpoint_period",
                    state.control.generation,
                    &origins,
                )
                .unwrap();
        }
        if stage >= 2 {
            telemetry
                .rebind_lifecycle_state(state.control.generation, "renewed_checkpoint_period")
                .unwrap();
        }
        if stage == 3 {
            // Inject failure of the final state commit after every child file
            // was rewritten. Restore the original durable checkpoint for reboot.
            let backup = state_path().with_extension("checkpoint-backup");
            std::fs::rename(state_path(), &backup).unwrap();
            std::fs::create_dir(state_path()).unwrap();
            assert!(
                telemetry
                    .finish_credential_renewal_checkpoint(&mut state)
                    .is_err()
            );
            std::fs::remove_dir(state_path()).unwrap();
            std::fs::rename(backup, state_path()).unwrap();
        }
        drop(telemetry);
        // Exercise the actual startup ordering: redo before outbox retention
        // and diagnostic authorization sanitization, with real persisted files.
        let restarted = BehaviorTelemetry::new();
        let workers = Arc::downgrade(&restarted.state);
        let reports: Value = serde_json::from_slice(
            &std::fs::read(paths::pinvou3_home().join("failure-diagnostic-reports.json")).unwrap(),
        )
        .unwrap();
        let report_preserved = reports.as_array().is_some_and(|rows| rows.len() == 1)
            && reports[0]["payload"] == frozen
            && reports[0]["lease"]["collection_period_id"] == "renewed_checkpoint_period";
        let committed = load_state().unwrap().unwrap();
        let events = Outbox::load(outbox_path()).unwrap();
        let event_preserved = events.records.iter().any(|row| {
            row.event["diagnostic_id"] == frozen["incident_id"]
                && row.event["diagnostic_origin_period_id"] == frozen["incident_period_id"]
                && row.collection_period_id == "renewed_checkpoint_period"
        });
        restarted.collecting.store(false, Ordering::Release);
        drop(restarted);
        let _ = release_events.send(());
        events_server.abort();
        let _ = events_server.await;
        heartbeat_server.abort();
        let _ = heartbeat_server.await;
        tokio::time::timeout(Duration::from_secs(5), async {
            while workers.upgrade().is_some() {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("drain workers before restoring the synthetic home");
        assert!(
            report_preserved,
            "confirmed report must survive interruption stage {stage}"
        );
        assert!(
            event_preserved,
            "original incident must survive interruption stage {stage}"
        );
        assert!(committed.credential_renewal_checkpoint.is_none());
    }
}

#[test]
fn diagnostic_checkpoint_cannot_restore_withdrawn_or_different_authorization() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    for boundary in ["disabled", "deleted", "generation", "endpoint"] {
        let mut state = heartbeat_state(&runtime);
        state.credential_renewal_checkpoint = Some(CollectionLease {
            generation: state.control.generation,
            collection_period_id: "installation_period".into(),
        });
        match boundary {
            "disabled" => state.control.enabled = false,
            "deleted" => state.control.collection_blocked = true,
            "generation" => state.control.generation += 1,
            "endpoint" => state.behavior_events_url = "http://localhost:2/events".into(),
            _ => unreachable!(),
        }
        let telemetry = test_telemetry(runtime.clone(), state.clone());
        assert!(!state.renewal_checkpoint_is_current(&runtime), "{boundary}");
        assert!(
            telemetry
                .finish_credential_renewal_checkpoint(&mut state)
                .is_err(),
            "{boundary}"
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn credential_checkpoint_recovers_in_background_without_queued_reports() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let mut state = heartbeat_state(&runtime);
    state.control.collection_period_id = Some("renewed_empty_queue_period".into());
    state.credential_renewal_checkpoint = Some(CollectionLease {
        generation: state.control.generation,
        collection_period_id: "renewed_empty_queue_period".into(),
    });
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state.clone());
    assert_eq!(
        diagnostics::diagnostic_incidents(&telemetry).unwrap()["reports"],
        json!([])
    );
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    telemetry.collecting.store(false, Ordering::Release);
    let backup = state_path().with_extension("checkpoint-backup");
    std::fs::rename(state_path(), &backup).unwrap();
    std::fs::create_dir(state_path()).unwrap();
    assert!(
        telemetry
            .finish_credential_renewal_checkpoint(&mut state)
            .is_err()
    );
    telemetry.state_healthy.store(false, Ordering::Release);
    let (tx, rx) = mpsc::channel(1);
    // Only the control worker runs: no report, event or restart can rescue it.
    let worker = tokio::spawn(BehaviorTelemetry::run_control_worker(
        rx,
        telemetry.worker_clone(),
    ));
    tx.send(()).await.unwrap();
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(!telemetry.collecting.load(Ordering::Acquire));
    std::fs::remove_dir(state_path()).unwrap();
    std::fs::rename(backup, state_path()).unwrap();
    let recovered = tokio::time::timeout(Duration::from_secs(5), async {
        while !telemetry.collecting.load(Ordering::Acquire) {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await;
    drop(tx);
    worker.await.unwrap();
    recovered.expect("disk recovery resumes collection without a queued report or restart");
    assert!(telemetry.state_healthy.load(Ordering::Acquire));
    assert!(
        load_state()
            .unwrap()
            .unwrap()
            .credential_renewal_checkpoint
            .is_none()
    );
    assert_eq!(
        telemetry.current_lease().unwrap().collection_period_id,
        "renewed_empty_queue_period"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn diagnostic_worker_settles_a_stale_lease_without_a_renewal_checkpoint() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let failure = diagnostic_failure("stale_session", "stale_turn");
    let id = failure.event_id.clone();
    telemetry.track(failure);
    let preview = diagnostics::preview_diagnostic(
        &telemetry,
        DiagnosticPreviewRequest {
            incident_id: id,
            notes: "reproduce empty response".into(),
            attachments: vec![],
        },
    )
    .unwrap();
    diagnostics::confirm_diagnostic(&telemetry, preview["preview_token"].as_str().unwrap(), true)
        .unwrap();
    *telemetry.collection_period_id.write().unwrap() = Some("superseding_period".into());
    let (tx, rx) = mpsc::channel(1);
    let worker = tokio::spawn(diagnostics::run_report_worker(rx, telemetry.worker_clone()));
    tx.send(()).await.unwrap();
    let result = tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let snapshot = diagnostics::diagnostic_incidents(&telemetry).unwrap();
            if snapshot["reports"][0]["status"] == "rejected" {
                break snapshot;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await;
    drop(tx);
    worker.await.unwrap();
    let snapshot =
        result.expect("stale lease must settle without network or a five-second polling cycle");
    assert_eq!(snapshot["reports"][0]["reason"], "authorization_changed");
}

#[test]
fn diagnostic_environment_discards_invalid_current_and_persisted_values() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let lease = telemetry.current_lease().unwrap();
    let mut first = diagnostic_failure("environment_session", "environment_turn")
        .string("mode", "private-invalid-mode")
        .string("input_type", "private-invalid-input");
    diagnostics::enrich(&telemetry, &mut first, &lease);
    assert!(diagnostics::validate(&first).is_ok());
    let environment = first.fields["diagnostic_environment"].as_object().unwrap();
    assert!(!environment.contains_key("mode"));
    assert!(!environment.contains_key("input_type"));
    // Upgrading must also recover a context written by the old implementation.
    let mut persisted = serde_json::to_value(&*telemetry.diagnostics.lock().unwrap()).unwrap();
    persisted["contexts"]["environment_session"]["environment"]["input_type"] =
        json!("private-stored-input");
    *telemetry.diagnostics.lock().unwrap() = serde_json::from_value(persisted).unwrap();
    let mut next = diagnostic_failure("environment_session", "environment_turn");
    next.fields.remove("input_type");
    diagnostics::enrich(&telemetry, &mut next, &lease);
    assert!(diagnostics::validate(&next).is_ok());
    assert!(
        next.fields["diagnostic_environment"]
            .get("input_type")
            .is_none()
    );
}

#[tokio::test(flavor = "current_thread")]
async fn diagnostic_rebound_report_sends_frozen_preview_with_current_delivery_authorization() {
    let _home = TestHome::new();
    for (http, conflict, expected, reason) in [
        (200, "", "received", ""),
        (
            409,
            "diagnostic_incident_period_mismatch",
            "rejected",
            "incident_period_mismatch",
        ),
        (
            409,
            "collection_inactive",
            "rejected",
            "collection_inactive",
        ),
        (409, "diagnostic_report_conflict", "rejected", ""),
    ] {
        let runtime = test_runtime("http://localhost:1");
        let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
        let failure = diagnostic_failure("delivery_session", "delivery_turn");
        let id = failure.event_id.clone();
        telemetry.track(failure);
        let preview = diagnostics::preview_diagnostic(
            &telemetry,
            DiagnosticPreviewRequest {
                incident_id: id,
                notes: "reproduce empty response".into(),
                attachments: vec![],
            },
        )
        .unwrap();
        diagnostics::confirm_diagnostic(
            &telemetry,
            preview["preview_token"].as_str().unwrap(),
            true,
        )
        .unwrap();
        let lease = telemetry.current_lease().unwrap();
        let origins = diagnostics::origin_periods(&telemetry, lease.generation).unwrap();
        telemetry
            .outbox
            .lock()
            .unwrap()
            .rebind_period("current_delivery_period", lease.generation, &origins)
            .unwrap();
        diagnostics::rebind(&telemetry, lease.generation, "current_delivery_period").unwrap();
        *telemetry.collection_period_id.write().unwrap() = Some("current_delivery_period".into());
        let receipt: &'static str = Box::leak(
            (if http == 200 {
                json!({"report_id":preview["report"]["report_id"],"status":"received"})
            } else {
                json!({"error":conflict})
            })
            .to_string()
            .into_boxed_str(),
        );
        let (base, server) = mock_http(vec![(http, receipt)]).await;
        {
            let mut state = telemetry.state.lock().await;
            let state = state.as_mut().unwrap();
            state.behavior_events_url = format!("{base}/behavior/events");
            state.control.collection_period_id = Some("current_delivery_period".into());
        }
        let mut worker_telemetry = telemetry.worker_clone();
        let mut runtime = (*worker_telemetry.runtime).clone();
        runtime.behavior_events_url = format!("{base}/behavior/events");
        worker_telemetry.runtime = Arc::new(runtime);
        let (tx, rx) = mpsc::channel(1);
        let worker = tokio::spawn(diagnostics::run_report_worker(rx, worker_telemetry));
        let requests = tokio::time::timeout(Duration::from_secs(3), server)
            .await
            .unwrap()
            .unwrap();
        drop(tx);
        worker.await.unwrap();
        let body: Value =
            serde_json::from_str(requests[0].split_once("\r\n\r\n").unwrap().1).unwrap();
        assert_eq!(body["collection_period_id"], "current_delivery_period");
        assert_eq!(body["report"], preview["report"]);
        for reload in [false, true] {
            if reload {
                *telemetry.diagnostics.lock().unwrap() = diagnostics::load();
            }
            let reports = diagnostics::diagnostic_incidents(&telemetry).unwrap();
            assert_eq!(reports["reports"][0]["status"], expected);
            assert_eq!(reports["reports"][0]["reason"], reason);
        }
    }
}

#[test]
fn diagnostic_context_capacity_evicts_one_cold_trace_without_stateless_pollution() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let lease = telemetry.current_lease().unwrap();
    let started = now_ms().saturating_sub(10000);
    for index in 0..128 {
        let mut event = BehaviorEvent::new("model_used").session(&format!("session_{index}"));
        event.occurred_at = started + index;
        diagnostics::enrich(&telemetry, &mut event, &lease);
    }
    let mut active = BehaviorEvent::new("task_first_output").session("session_0");
    diagnostics::enrich(&telemetry, &mut active, &lease);
    for _ in 0..140 {
        diagnostics::enrich(
            &telemetry,
            &mut BehaviorEvent::new("model_selected"),
            &lease,
        );
    }
    let mut added = BehaviorEvent::new("model_used").session("new_session");
    diagnostics::enrich(&telemetry, &mut added, &lease);
    let state = serde_json::to_value(&*telemetry.diagnostics.lock().unwrap()).unwrap();
    let contexts = state["contexts"].as_object().unwrap();
    assert_eq!(contexts.len(), 128);
    assert!(contexts.contains_key("session_0"));
    assert!(contexts.contains_key("new_session"));
    assert!(!contexts.contains_key("session_1"));
    assert_eq!(contexts["session_0"]["steps"].as_array().unwrap().len(), 2);
}

#[test]
fn diagnostic_preview_correlates_notes_and_attachments_without_linking_separate_reports() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let failure = diagnostic_failure("session_scope_fixture", "turn_scope_fixture");
    let id = failure.event_id.clone();
    telemetry.track(failure);
    let attachment = paths::pinvou3_home().join("scope-fixture.log");
    filesystem::atomic_write_private(
        &attachment,
        b"request_id=PRIVATE_SCOPE_FIXTURE error_code=EACCES op=write",
    )
    .unwrap();
    let preview = || {
        diagnostics::preview_diagnostic(
            &telemetry,
            DiagnosticPreviewRequest {
                incident_id: id.clone(),
                notes: "request_id=PRIVATE_SCOPE_FIXTURE http_status=403".into(),
                attachments: vec![attachment.to_string_lossy().into_owned()],
            },
        )
        .unwrap()
    };
    let first = preview();
    let notes: Value = serde_json::from_str(first["report"]["notes"].as_str().unwrap()).unwrap();
    let log: Value = serde_json::from_str(
        first["report"]["attachments"][0]["content"]
            .as_str()
            .unwrap(),
    )
    .unwrap();
    assert_eq!(notes["request_id"], log["request_id"]);
    assert_eq!(notes["http_status"], 403);
    assert_eq!(log["error_code"], "EACCES");
    assert!(!first.to_string().contains("PRIVATE_SCOPE_FIXTURE"));
    assert!(!first.to_string().contains("redaction_"));
    let second = preview();
    let next: Value = serde_json::from_str(second["report"]["notes"].as_str().unwrap()).unwrap();
    assert_ne!(notes["request_id"], next["request_id"]);
}

#[test]
fn diagnostic_queue_rolls_back_a_failed_private_write_before_retrying_confirmation() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let failure = diagnostic_failure("diagnostic_storage", "diagnostic_storage");
    let id = failure.event_id.clone();
    telemetry.track(failure);
    let preview = diagnostics::preview_diagnostic(
        &telemetry,
        DiagnosticPreviewRequest {
            incident_id: id,
            notes: "storage retry".into(),
            attachments: vec![],
        },
    )
    .unwrap();
    let report_path = paths::pinvou3_home().join("failure-diagnostic-reports.json");
    std::fs::create_dir(&report_path).unwrap();
    assert!(
        diagnostics::confirm_diagnostic(
            &telemetry,
            preview["preview_token"].as_str().unwrap(),
            true
        )
        .is_err()
    );
    assert_eq!(
        diagnostics::diagnostic_incidents(&telemetry).unwrap()["reports"],
        json!([])
    );
    std::fs::remove_dir(&report_path).unwrap();
    let confirmed = diagnostics::confirm_diagnostic(
        &telemetry,
        preview["preview_token"].as_str().unwrap(),
        true,
    )
    .unwrap();
    assert_eq!(
        confirmed,
        diagnostics::confirm_diagnostic(
            &telemetry,
            preview["preview_token"].as_str().unwrap(),
            true
        )
        .unwrap()
    );
    assert_eq!(
        diagnostics::diagnostic_incidents(&telemetry).unwrap()["reports"]
            .as_array()
            .unwrap()
            .len(),
        1
    );
}

#[test]
fn diagnostic_cancel_and_quality_observations_do_not_rewrite_task_outcomes() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    telemetry.track(
        diagnostic_failure("cancel", "cancel")
            .status("cancelled")
            .string("error_category", "user_cancelled"),
    );
    telemetry.track(
        BehaviorEvent::new("response_feedback_changed")
            .session("old_session")
            .turn("old_turn")
            .string("feedback_id", "feedback_diagnostic_test")
            .string("feedback_action", "set")
            .string("rating", "negative"),
    );
    let events = telemetry.outbox.lock().unwrap();
    assert!(events.records[0].event.get("diagnostic_id").is_none());
    assert_eq!(
        events.records[1].event["diagnostic_code"],
        "user_reported_quality"
    );
    assert!(events.records[1].event.get("status").is_none());
}

#[test]
fn diagnostic_failed_stream_is_not_discarded_as_a_user_cancellation() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let error = "stream interrupted: connection reset by peer";
    telemetry.track(
        diagnostic_failure("failed_stream", "failed_stream")
            .string("error_category", error_category(Some(error)))
            .diagnostic_error(error, "assistant_error"),
    );
    assert_eq!(
        telemetry.outbox.lock().unwrap().records[0].event["diagnostic_code"],
        "connection_reset"
    );
    telemetry.track(
        BehaviorEvent::new("tool_call_completed")
            .session("tool_session")
            .turn("tool_turn")
            .string("operation_id", "tool_operation_fixture")
            .integer("attempt_index", 1)
            .tool("shell", "shell", "cli")
            .success(false)
            .integer("duration_ms", 1)
            .string("error_category", error_category(Some(error)))
            .diagnostic_error(error, "tool_result"),
    );
    assert_eq!(
        telemetry.outbox.lock().unwrap().records[1].event["diagnostic_code"],
        "connection_reset"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn diagnostic_reply_limit_applies_to_chunked_bodies_without_content_length() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let server = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut request = [0u8; 1024];
        let _ = socket.read(&mut request).await;
        let _=socket.write_all(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n1001\r\n").await;
        let _ = socket.write_all(&vec![b'x'; 4097]).await;
        // No terminating chunk: a bounded reader must reject before waiting for EOF.
        tokio::time::sleep(Duration::from_secs(2)).await;
    });
    let mut response = reqwest::get(format!("http://{address}/receipt"))
        .await
        .unwrap();
    let result = tokio::time::timeout(
        Duration::from_secs(1),
        diagnostics::bounded_reply(&mut response),
    )
    .await
    .unwrap();
    assert!(result.is_err());
    server.abort();
    let _ = server.await;
}

#[tokio::test(flavor = "current_thread")]
async fn diagnostic_old_collector_fallback_preserves_id_and_business_event() {
    let _home = TestHome::new();
    let failure = diagnostic_failure("session_diagnostic", "turn_diagnostic");
    let id = failure.event_id.clone();
    let first: &'static str = Box::leak(
        json!({"results":[{"event_id":id,"status":"rejected","reason_code":"invalid_field"}]})
            .to_string()
            .into_boxed_str(),
    );
    let second: &'static str = Box::leak(
        json!({"results":[{"event_id":id,"status":"accepted","reason_code":null}]})
            .to_string()
            .into_boxed_str(),
    );
    let (base, server) = mock_http(vec![(200, first), (200, second)]).await;
    let runtime = test_runtime(&base);
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    telemetry.track(failure);
    let batch = telemetry.outbox.lock().unwrap().batch();
    let results = telemetry.send_events(&batch).await.unwrap();
    assert_eq!(results[&id].0, DeliveryStatus::Accepted);
    let requests = server.await.unwrap();
    let packet = |request: &str| {
        serde_json::from_str::<Value>(request.split_once("\r\n\r\n").unwrap().1).unwrap()
    };
    assert!(
        packet(&requests[0])["events"][0]
            .get("diagnostic_code")
            .is_some()
    );
    assert!(
        packet(&requests[1])["events"][0]
            .get("diagnostic_code")
            .is_none()
    );
    assert_eq!(
        packet(&requests[0])["events"][0]["event_id"],
        packet(&requests[1])["events"][0]["event_id"]
    );
}

#[tokio::test(flavor = "current_thread")]
async fn diagnostic_delivery_is_fair_requires_matching_receipt_and_clears_delivered_material() {
    let _home = TestHome::new();
    let (base, invalid_server) = mock_http(vec![(
        200,
        "{\"report_id\":\"wrong_report_id\",\"status\":\"received\"}",
    )])
    .await;
    let mut runtime = test_runtime(&base);
    runtime.behavior_events_url = format!("{base}/behavior/events");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let failure = diagnostic_failure("diagnostic", "diagnostic");
    let id = failure.event_id.clone();
    telemetry.track(failure);
    let preview = diagnostics::preview_diagnostic(
        &telemetry,
        DiagnosticPreviewRequest {
            incident_id: id,
            notes: "first pending reproduction".into(),
            attachments: vec![],
        },
    )
    .unwrap();
    diagnostics::confirm_diagnostic(&telemetry, preview["preview_token"].as_str().unwrap(), true)
        .unwrap();
    let (tx, rx) = mpsc::channel(1);
    tx.send(()).await.unwrap();
    let worker = tokio::spawn(diagnostics::run_report_worker(rx, telemetry.worker_clone()));
    let requests = tokio::time::timeout(Duration::from_secs(3), invalid_server)
        .await
        .unwrap()
        .unwrap();
    assert!(requests[0].starts_with("POST /diagnostics/report "));
    drop(tx);
    worker.await.unwrap();
    assert_eq!(
        diagnostics::diagnostic_incidents(&telemetry).unwrap()["reports"][0]["status"],
        "queued"
    );
    let second_failure = diagnostic_failure("second_diagnostic", "second_diagnostic");
    let second_id = second_failure.event_id.clone();
    telemetry.track(second_failure);
    let preview = diagnostics::preview_diagnostic(
        &telemetry,
        DiagnosticPreviewRequest {
            incident_id: second_id,
            notes: "reviewed reproduction".into(),
            attachments: vec![],
        },
    )
    .unwrap();
    let queued = diagnostics::confirm_diagnostic(
        &telemetry,
        preview["preview_token"].as_str().unwrap(),
        true,
    )
    .unwrap();
    let receipt: &'static str = Box::leak(
        json!({"report_id":queued["report_id"],"status":"received"})
            .to_string()
            .into_boxed_str(),
    );
    let (base, valid_server) = mock_http(vec![(200, receipt)]).await;
    {
        let mut persisted = telemetry.state.lock().await;
        persisted.as_mut().unwrap().behavior_events_url = format!("{base}/behavior/events");
    }
    let mut worker_telemetry = telemetry.worker_clone();
    let mut runtime = (*worker_telemetry.runtime).clone();
    runtime.behavior_events_url = format!("{base}/behavior/events");
    worker_telemetry.runtime = Arc::new(runtime);
    let (tx, rx) = mpsc::channel(1);
    tx.send(()).await.unwrap();
    let worker = tokio::spawn(diagnostics::run_report_worker(rx, worker_telemetry));
    tokio::time::timeout(Duration::from_secs(3), valid_server)
        .await
        .unwrap()
        .unwrap();
    drop(tx);
    worker.await.unwrap();
    assert_eq!(
        diagnostics::diagnostic_incidents(&telemetry).unwrap()["reports"][1]["status"],
        "received"
    );
    assert_eq!(
        diagnostics::diagnostic_incidents(&telemetry).unwrap()["reports"][0]["status"],
        "queued"
    );
    assert!(
        !std::fs::read_to_string(paths::pinvou3_home().join("failure-diagnostic-reports.json"))
            .unwrap()
            .contains("reviewed reproduction")
    );
}

async fn mock_http(
    responses: Vec<(u16, &'static str)>,
) -> (String, tokio::task::JoinHandle<Vec<String>>) {
    let (base, server, _) = mock_http_observed(responses).await;
    (base, server)
}

async fn mock_http_observed(
    responses: Vec<(u16, &'static str)>,
) -> (
    String,
    tokio::task::JoinHandle<Vec<String>>,
    mpsc::Receiver<()>,
) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (observed_tx, observed_rx) = mpsc::channel(responses.len().max(1));
    let handle = tokio::spawn(async move {
        let mut requests = Vec::new();
        for (status, body) in responses {
            let (mut socket, _) = listener.accept().await.unwrap();
            let mut bytes = Vec::new();
            let mut buffer = [0_u8; 4096];
            loop {
                let read = socket.read(&mut buffer).await.unwrap();
                if read == 0 {
                    break;
                }
                bytes.extend_from_slice(&buffer[..read]);
                let Some(header_end) = bytes.windows(4).position(|part| part == b"\r\n\r\n") else {
                    continue;
                };
                let headers = String::from_utf8_lossy(&bytes[..header_end]);
                let content_length = headers
                    .lines()
                    .find_map(|line| {
                        let (name, value) = line.split_once(':')?;
                        name.eq_ignore_ascii_case("content-length")
                            .then(|| value.trim().parse::<usize>().ok())
                            .flatten()
                    })
                    .unwrap_or(0);
                if bytes.len() >= header_end + 4 + content_length {
                    break;
                }
            }
            requests.push(String::from_utf8_lossy(&bytes).into_owned());
            let reason = if status == 200 { "OK" } else { "Gone" };
            let response = format!(
                "HTTP/1.1 {status} {reason}\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                body.len()
            );
            socket.write_all(response.as_bytes()).await.unwrap();
            socket.shutdown().await.unwrap();
            let _ = observed_tx.try_send(());
        }
        requests
    });
    (format!("http://{address}"), handle, observed_rx)
}

fn heartbeat_state(runtime: &RuntimeConfig) -> PersistedState {
    let mut state = PersistedState::fresh(runtime);
    state.device_id = Some("installation_device".to_string());
    state.device_token = Some("installation_token".to_string());
    state.control.enabled = true;
    state.control.collection_period_id = Some("installation_period".to_string());
    state
}

#[tokio::test(flavor = "current_thread")]
async fn installation_heartbeat_constructor_starts_persisted_installation_immediately() {
    let home = TestHome::new();
    let (base, heartbeat_server) = mock_http(vec![(200, "{}")]).await;
    let (events_base, events_seen, release_events, events_server) =
        controlled_http(503, "{}").await;
    let events_url = format!("{events_base}/events");
    let _env = EnvRestore::set(
        &home,
        &[
            ("PINVOU_BEHAVIOR_TELEMETRY_ENABLED", "true"),
            ("PINVOU_TELEMETRY_BASE_URL", &base),
            ("PINVOU_BEHAVIOR_EVENTS_URL", &events_url),
            (
                "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
                "test-enrollment-token-1234567890",
            ),
        ],
    );
    let runtime = RuntimeConfig::from_env();
    let mut state = heartbeat_state(&runtime);
    state.control.user_enabled = Some(true);
    state.control.cohort_started = true;
    persist_state(&state).unwrap();

    // Exercise the production constructor's channels, gating and spawn; never
    // inject a heartbeat sender or manually spawn the heartbeat worker here.
    let telemetry = BehaviorTelemetry::new();
    let channel_created = telemetry.heartbeat_tx.is_some();
    let worker_state = Arc::downgrade(&telemetry.state);
    let heartbeat = tokio::time::timeout(Duration::from_secs(3), heartbeat_server).await;
    let event_upload = tokio::time::timeout(Duration::from_secs(3), events_seen).await;

    // Drain all constructor workers before TestHome restores the process-wide
    // environment, even when the heartbeat observation failed in a mutation run.
    telemetry.collecting.store(false, Ordering::Release);
    drop(telemetry);
    let _ = release_events.send(());
    events_server.abort();
    let _ = events_server.await;
    tokio::time::timeout(Duration::from_secs(5), async {
        while worker_state.upgrade().is_some() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("constructor workers should stop before restoring the test environment");

    assert!(
        channel_created,
        "constructor must retain the heartbeat wake channel"
    );
    event_upload
        .expect("startup event upload should begin")
        .unwrap();
    let requests = heartbeat
        .expect("constructor must send its first heartbeat without waiting 120 seconds")
        .unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /v1/heartbeat "));
    assert!(requests[0].contains("Bearer installation_token"));
    let body: Value = serde_json::from_str(requests[0].split_once("\r\n\r\n").unwrap().1).unwrap();
    assert_eq!(
        body,
        json!({
            "device_id": "installation_device", "app_version": runtime.app_version,
            "platform": runtime.platform, "arch": runtime.arch, "state": "online",
        })
    );
}

#[tokio::test(flavor = "current_thread")]
async fn installation_heartbeat_does_not_wait_for_behavior_event_upload() {
    let _home = TestHome::new();
    let (base, heartbeat_server) = mock_http(vec![(200, "{}")]).await;
    let (events_base, seen, release, events_server) = controlled_http(503, "{}").await;
    let mut runtime = test_runtime(&base);
    runtime.behavior_events_url = format!("{events_base}/events");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    telemetry.track(BehaviorEvent::new("app_opened").string("launch_type", "cold"));
    let pending = telemetry.outbox.lock().unwrap().batch();
    assert_eq!(pending.len(), 1);
    let uploader = telemetry.worker_clone();
    let upload = tokio::spawn(async move { uploader.send_events(&pending).await });
    tokio::time::timeout(Duration::from_secs(3), seen)
        .await
        .unwrap()
        .unwrap();
    tokio::time::timeout(Duration::from_secs(1), telemetry.send_heartbeat())
        .await
        .unwrap()
        .unwrap();
    assert!(!upload.is_finished());
    assert_eq!(telemetry.outbox.lock().unwrap().records.len(), 1);
    release.send(()).unwrap();
    assert!(upload.await.unwrap().is_err());
    assert!(events_server.await.unwrap().starts_with("POST /events "));
    assert!(heartbeat_server.await.unwrap()[0].starts_with("POST /v1/heartbeat "));
}

#[tokio::test(flavor = "current_thread")]
async fn installation_heartbeat_progresses_for_six_idle_minutes_and_stops_with_owner() {
    let _home = TestHome::new();
    // The second heartbeat fails; subsequent ticks must still run without events.
    let (base, server, mut observed) =
        mock_http_observed(vec![(200, "{}"), (503, "{}"), (200, "{}"), (200, "{}")]).await;
    let runtime = test_runtime(&base);
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let (tx, rx) = mpsc::channel(1);
    let worker = tokio::spawn(BehaviorTelemetry::run_heartbeat_worker(
        rx,
        telemetry.worker_clone(),
    ));
    for tick in 0..4 {
        if tick > 0 {
            // Pause only the idle timer; HTTP runs against a real loopback socket.
            tokio::time::pause();
            tokio::time::advance(Duration::from_secs(119)).await;
            tokio::time::resume();
            assert!(
                tokio::time::timeout(Duration::from_millis(20), observed.recv())
                    .await
                    .is_err()
            );
            tokio::time::pause();
            tokio::time::advance(Duration::from_secs(1)).await;
            tokio::time::resume();
        }
        assert_eq!(
            tokio::time::timeout(Duration::from_secs(3), observed.recv())
                .await
                .unwrap(),
            Some(())
        );
        // Wait until the worker has consumed the response, before advancing time.
        drop(telemetry.control_gate.lock().await);
        assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    }
    drop(tx);
    tokio::time::timeout(Duration::from_secs(3), worker)
        .await
        .unwrap()
        .unwrap();
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 4);
    for request in requests {
        assert!(request.starts_with("POST /v1/heartbeat "));
        assert!(request.contains("Bearer installation_token"));
        let body: Value = serde_json::from_str(request.split_once("\r\n\r\n").unwrap().1).unwrap();
        assert_eq!(
            body,
            json!({
                "device_id": "installation_device", "app_version": "0.0.0-test",
                "platform": "test", "arch": "test", "state": "online",
            })
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn installation_heartbeat_enable_wakes_without_waiting_for_interval() {
    let _home = TestHome::new();
    let (base, server, mut observed) = mock_http_observed(vec![
        (200, r#"{"collection_period_id":"installation_period"}"#),
        (200, "{}"),
    ])
    .await;
    let runtime = test_runtime(&base);
    let mut state = heartbeat_state(&runtime);
    state.control.enabled = false;
    state.control.collection_period_id = None;
    state.control.cohort_started = true;
    let mut telemetry = test_telemetry(runtime, state);
    let (tx, rx) = mpsc::channel(1);
    telemetry.heartbeat_tx = Some(tx);
    let worker = tokio::spawn(BehaviorTelemetry::run_heartbeat_worker(
        rx,
        telemetry.worker_clone(),
    ));
    assert!(
        tokio::time::timeout(Duration::from_millis(30), observed.recv())
            .await
            .is_err()
    );
    assert!(
        telemetry
            .set_user_collection_enabled(true)
            .await
            .unwrap()
            .enabled
    );
    // The first response is collection/start; the next is the immediate heartbeat.
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(3), observed.recv())
            .await
            .expect("collection/start response should be observed"),
        Some(())
    );
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(3), observed.recv())
            .await
            .unwrap(),
        Some(())
    );
    drop(telemetry);
    tokio::time::timeout(Duration::from_secs(3), worker)
        .await
        .unwrap()
        .unwrap();
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 2);
    assert!(requests[0].starts_with("POST /collection/start "));
    assert!(requests[1].starts_with("POST /v1/heartbeat "));
}

#[tokio::test(flavor = "current_thread")]
async fn installation_heartbeat_skips_disabled_pending_mismatched_or_unhealthy_state() {
    let _home = TestHome::new();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let runtime = test_runtime(&format!("http://{}", listener.local_addr().unwrap()));
    for case in 0..12 {
        let mut config = runtime.clone();
        let mut state = heartbeat_state(&config);
        match case {
            0 => config.enabled = false,
            1 => state.control.enabled = false,
            2 => state.control.user_enabled = Some(false),
            3 => state.control.collection_blocked = true,
            4 => state.control.collection_period_id = None,
            5 => state.device_token = None,
            6 => state.telemetry_base_url.push_str("/old"),
            11 => state.behavior_events_url.push_str("/old"),
            7 => {
                state.control.pending_request = Some(PendingControlRequest {
                    kind: PendingControlKind::End,
                    request_id: "pending_end".into(),
                    collection_period_id: state.control.collection_period_id.clone(),
                    behavior_events_url: config.behavior_events_url.clone(),
                })
            }
            8 => state
                .control
                .pending_deletions
                .push(PendingEndpointDeletion {
                    request_id: "pending_delete".into(),
                    behavior_events_url: config.behavior_events_url.clone(),
                    device_id: "retired_device".into(),
                    device_token: "retired_token".into(),
                }),
            _ => {}
        }
        let telemetry = test_telemetry(config, state);
        if case == 9 {
            telemetry.state_healthy.store(false, Ordering::Release);
        }
        if case == 10 {
            telemetry
                .authorization_generation
                .store(1, Ordering::Release);
        }
        tokio::time::timeout(Duration::from_secs(1), telemetry.send_heartbeat())
            .await
            .unwrap()
            .unwrap();
    }
    assert!(
        tokio::time::timeout(Duration::from_millis(30), listener.accept())
            .await
            .is_err()
    );
}

#[tokio::test(flavor = "current_thread")]
async fn installation_heartbeat_remote_deletion_stops_collection_without_registering() {
    let _home = TestHome::new();
    let (base, server) = mock_http(vec![(410, r#"{"error":"device_data_deleted"}"#)]).await;
    let runtime = test_runtime(&base);
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    telemetry.send_heartbeat().await.unwrap();
    assert!(!telemetry.status().await.enabled);
    assert!(
        telemetry
            .state
            .lock()
            .await
            .as_ref()
            .unwrap()
            .control
            .collection_blocked
    );
    telemetry.send_heartbeat().await.unwrap();
    assert_eq!(server.await.unwrap().len(), 1);
}

#[tokio::test(flavor = "current_thread")]
async fn installation_heartbeat_expired_credentials_renew_same_installation() {
    let _home = TestHome::new();
    let (base, server) = mock_http(vec![
        (410, r#"{"error":"device_credentials_expired"}"#),
        (
            200,
            r#"{"device_id":"renewed_device","device_token":"renewed_token"}"#,
        ),
        (200, r#"{"collection_period_id":"renewed_period"}"#),
        (200, "{}"),
    ])
    .await;
    let runtime = test_runtime(&base);
    let state = heartbeat_state(&runtime);
    let installation_id = state.installation_id.clone();
    let telemetry = test_telemetry(runtime, state);
    telemetry.send_heartbeat().await.unwrap();
    telemetry.send_heartbeat().await.unwrap();
    let current = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(current.installation_id, installation_id);
    assert_eq!(current.device_id.as_deref(), Some("renewed_device"));
    assert!(telemetry.status().await.enabled);
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 4);
    assert!(requests[1].starts_with("POST /v1/register "));
    assert!(requests[1].contains(&format!("pinvou3-installation:{installation_id}")));
    assert!(requests[2].starts_with("POST /collection/start "));
    assert!(requests[3].starts_with("POST /v1/heartbeat "));
    assert!(requests[3].contains("Bearer renewed_token"));
}

#[tokio::test(flavor = "current_thread")]
async fn installation_heartbeat_unknown_errors_preserve_identity_and_allow_retry() {
    let _home = TestHome::new();
    let (base, server) = mock_http(vec![
        (401, "{}"),
        (410, r#"{"error":"future_reason"}"#),
        (410, "not-json"),
        (503, "{}"),
        (200, "{}"),
    ])
    .await;
    let runtime = test_runtime(&base);
    let state = heartbeat_state(&runtime);
    let installation_id = state.installation_id.clone();
    let telemetry = test_telemetry(runtime, state);
    for _ in 0..4 {
        assert!(telemetry.send_heartbeat().await.is_err());
    }
    telemetry.send_heartbeat().await.unwrap();
    let current = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(current.installation_id, installation_id);
    assert_eq!(current.device_id.as_deref(), Some("installation_device"));
    assert!(telemetry.status().await.enabled);
    assert!(
        server
            .await
            .unwrap()
            .iter()
            .all(|request| request.starts_with("POST /v1/heartbeat "))
    );
}

#[tokio::test(flavor = "current_thread")]
async fn installation_heartbeat_queued_behind_stop_rechecks_consent() {
    let _home = TestHome::new();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let runtime = test_runtime(&format!("http://{}", listener.local_addr().unwrap()));
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let gate = telemetry.control_gate.lock().await;
    let mut stop = Box::pin(telemetry.set_user_collection_enabled(false));
    let mut heartbeat = Box::pin(telemetry.send_heartbeat());
    // Queue both futures while collection is still enabled, with stop first in
    // the FIFO control gate. Eligibility must be checked after acquiring it.
    std::future::poll_fn(|cx| {
        assert!(std::future::Future::poll(stop.as_mut(), cx).is_pending());
        assert!(std::future::Future::poll(heartbeat.as_mut(), cx).is_pending());
        std::task::Poll::Ready(())
    })
    .await;
    drop(gate);
    let (stopped, heartbeat) = tokio::time::timeout(Duration::from_secs(3), async {
        tokio::join!(stop, heartbeat)
    })
    .await
    .unwrap();
    let stopped = stopped.unwrap();
    assert!(!stopped.enabled);
    assert!(stopped.withdrawal_pending);
    heartbeat.unwrap();
    assert!(
        tokio::time::timeout(Duration::from_millis(30), listener.accept())
            .await
            .is_err()
    );
}

async fn controlled_http(
    status: u16,
    body: &'static str,
) -> (
    String,
    tokio::sync::oneshot::Receiver<()>,
    tokio::sync::oneshot::Sender<()>,
    tokio::task::JoinHandle<String>,
) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (seen_tx, seen_rx) = tokio::sync::oneshot::channel();
    let (release_tx, release_rx) = tokio::sync::oneshot::channel();
    let handle = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut bytes = Vec::new();
        let mut buffer = [0_u8; 4096];
        loop {
            let read = socket.read(&mut buffer).await.unwrap();
            if read == 0 {
                break;
            }
            bytes.extend_from_slice(&buffer[..read]);
            let Some(header_end) = bytes.windows(4).position(|part| part == b"\r\n\r\n") else {
                continue;
            };
            let headers = String::from_utf8_lossy(&bytes[..header_end]);
            let content_length = headers
                .lines()
                .find_map(|line| {
                    let (name, value) = line.split_once(':')?;
                    name.eq_ignore_ascii_case("content-length")
                        .then(|| value.trim().parse::<usize>().ok())
                        .flatten()
                })
                .unwrap_or(0);
            if bytes.len() >= header_end + 4 + content_length {
                break;
            }
        }
        let _ = seen_tx.send(());
        let _ = release_rx.await;
        let reason = if status == 200 { "OK" } else { "Gone" };
        let response = format!(
            "HTTP/1.1 {status} {reason}\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
            body.len()
        );
        socket.write_all(response.as_bytes()).await.unwrap();
        socket.shutdown().await.unwrap();
        String::from_utf8_lossy(&bytes).into_owned()
    });
    (format!("http://{address}"), seen_rx, release_tx, handle)
}

fn outbox_record(id: &str, period: &str, enqueued_at: u64) -> OutboxRecord {
    OutboxRecord {
        event_id: id.to_string(),
        collection_period_id: period.to_string(),
        generation: 1,
        enqueued_at,
        delivery_attempt_count: 0,
        last_reason_code: None,
        event: json!({ "event_id": id, "collection_period_id": period }),
    }
}

#[test]
fn behavior_event_serializes_sparse_fields() {
    let event = BehaviorEvent::new("tool_call_completed")
        .session("s1")
        .turn("t1")
        .tool(
            "mcp_weather_get_weather",
            "mcp_weather_get_weather",
            classify_tool("mcp_weather_get_weather"),
        )
        .success(false);
    let value = serde_json::to_value(event).unwrap();
    assert_eq!(value["event_name"], "tool_call_completed");
    assert_eq!(value["session_id"], "s1");
    assert_eq!(value["turn_id"], "t1");
    assert_eq!(value["tool_type"], "mcp");
    assert_eq!(value["success"], false);
    assert!(value.get("app_version").is_none());
    assert!(value.get("scene_l1").is_none());
}

#[test]
fn client_event_fields_reject_payload_paths_arguments_and_raw_errors() {
    for forbidden in [
        "message",
        "content",
        "file_path",
        "tool_arguments",
        "raw_error",
    ] {
        let event = BehaviorEvent::new("task_submitted").string(forbidden, "private-marker");
        assert!(
            validate_client_event_fields(&event).is_err(),
            "accepted {forbidden}"
        );
    }
    let allowed = BehaviorEvent::new("task_submitted")
        .session("session_test")
        .string("attempt_id", "attempt_test")
        .string("mode", "assistant")
        .input_type("text")
        .boolean("has_attachment", false);
    validate_client_event_fields(&allowed).unwrap();
    assert!(
        !serde_json::to_string(&allowed)
            .unwrap()
            .contains("private-marker")
    );
}

#[test]
fn normalized_tool_name_passes_client_gateway_and_enters_outbox() {
    let _home = TestHome::new();
    let runtime = test_runtime("https://example.test");
    let mut state = PersistedState::fresh(&runtime);
    state.control.enabled = true;
    state.control.generation = 1;
    state.control.collection_period_id = Some("tool_name_period".to_string());
    let telemetry = test_telemetry(runtime, state);
    let lease = telemetry.current_lease().unwrap();

    assert!(
        telemetry.try_track_for_lease(
            BehaviorEvent::new("tool_call_completed")
                .tool("mcp_custom", "MCP__Private_CRM__Lookup", "mcp")
                .success(true),
            &lease,
        )
    );

    let outbox = telemetry.outbox.lock().unwrap();
    assert_eq!(outbox.records.len(), 1);
    assert_eq!(
        outbox.records[0].event["tool_name"],
        "mcp__private_crm__lookup"
    );
}

#[test]
fn model_registry_never_returns_user_supplied_identifiers() {
    assert_eq!(provider_registry_key("openai_compatible"), "custom");
    assert_eq!(
        provider_registry_key("https://private.example/v1"),
        "unknown"
    );
    assert_eq!(model_registry_key("customer-secret-alias"), "custom");
    assert_eq!(model_registry_key("claude-4-sonnet"), "claude_sonnet");
    assert_eq!(model_registry_key("gpt-5.2-codex"), "gpt_5");
}

#[test]
fn client_registries_match_the_shared_machine_readable_contract() {
    let registry: Value = serde_json::from_str(include_str!(
        "../../../../docs/schemas/behavior-telemetry-registries.json"
    ))
    .unwrap();
    for (name, expected) in [
        ("providers", CONTROLLED_PROVIDER_KEYS),
        ("models", CONTROLLED_MODEL_KEYS),
        ("tools", CONTROLLED_TOOL_KEYS),
    ] {
        let actual = registry[name]
            .as_array()
            .unwrap()
            .iter()
            .map(|value| value.as_str().unwrap())
            .collect::<Vec<_>>();
        assert_eq!(actual, expected, "{name} registry drifted");
    }
    assert_eq!(provider_registry_key("xai"), "xai");
    assert_eq!(provider_registry_key("local_vllm"), "local");
}

#[test]
fn generated_registration_secret_meets_server_minimum() {
    let runtime = RuntimeConfig {
        enabled: true,
        telemetry_base_url: "https://example.test/pinvou3/telemetry".to_string(),
        behavior_events_url: "https://example.test/pinvou3/stats/api/behavior/events".to_string(),
        enrollment_token: None,
        app_version: "0.0.0".to_string(),
        platform: "test".to_string(),
        arch: "test".to_string(),
        channel: "dev".to_string(),
        locale: "en".to_string(),
    };
    let state = PersistedState::fresh(&runtime);
    assert!(state.registration_secret.len() >= 32);
    assert!(state.installation_id.starts_with("inst_"));
    assert!(!state.control.enabled);
}

#[test]
fn fresh_state_awaits_a_server_confirmed_default_collection_period() {
    let runtime = RuntimeConfig {
        enabled: true,
        telemetry_base_url: "https://example.test/pinvou3/telemetry".to_string(),
        behavior_events_url: "https://example.test/pinvou3/stats/api/behavior/events".to_string(),
        enrollment_token: None,
        app_version: "0.0.0".to_string(),
        platform: "test".to_string(),
        arch: "test".to_string(),
        channel: "dev".to_string(),
        locale: "en".to_string(),
    };
    let state = PersistedState::fresh(&runtime);
    assert!(!state.control.enabled);
    assert!(state.control.collection_period_id.is_none());
    assert!(state.control.pending_request.is_none());
    assert_eq!(state.control.user_enabled, None);
}

#[test]
fn explicit_collection_choice_round_trips_separately_from_runtime_state() {
    let runtime = RuntimeConfig {
        enabled: true,
        telemetry_base_url: "https://example.test/pinvou3/telemetry".to_string(),
        behavior_events_url: "https://example.test/pinvou3/stats/api/behavior/events".to_string(),
        enrollment_token: None,
        app_version: "0.0.0".to_string(),
        platform: "test".to_string(),
        arch: "test".to_string(),
        channel: "dev".to_string(),
        locale: "en".to_string(),
    };
    let mut state = PersistedState::fresh(&runtime);
    state.control.user_enabled = Some(false);

    let restored: PersistedState =
        serde_json::from_slice(&serde_json::to_vec(&state).unwrap()).unwrap();
    assert_eq!(restored.control.user_enabled, Some(false));
    assert!(!restored.control.enabled);
}

#[test]
fn internal_collection_is_enabled_by_default_with_an_operational_kill_switch() {
    assert!(behavior_telemetry_enabled_by_config(None));
    assert!(behavior_telemetry_enabled_by_config(Some("true")));
    assert!(behavior_telemetry_enabled_by_config(Some("1")));
    assert!(!behavior_telemetry_enabled_by_config(Some("false")));
    assert!(!behavior_telemetry_enabled_by_config(Some("off")));
}

#[test]
fn packaged_runtime_uses_the_deployed_telemetry_service_without_launcher_environment() {
    let home = TestHome::new();
    let _env = EnvRestore::unset(
        &home,
        &[
            "PINVOU_BEHAVIOR_TELEMETRY_ENABLED",
            "PINVOU_TELEMETRY_BASE_URL",
            "PINVOU_BEHAVIOR_EVENTS_URL",
            "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
        ],
    );

    let runtime = RuntimeConfig::from_env();

    assert!(runtime.enabled);
    // 控制面 URL 由事件地址 strip_suffix("/events") 推导，默认值必须保持该后缀。
    assert!(DEFAULT_BEHAVIOR_EVENTS_URL.ends_with("/events"));
    assert!(TELEMETRY_ENROLLMENT_TOKEN.len() >= 24);
    assert_eq!(runtime.telemetry_base_url, TELEMETRY_DEFAULT_BASE_URL);
    assert_eq!(runtime.behavior_events_url, DEFAULT_BEHAVIOR_EVENTS_URL);
    assert_eq!(
        runtime.enrollment_token.as_deref(),
        Some(TELEMETRY_ENROLLMENT_TOKEN)
    );
}

#[test]
fn explicit_complete_runtime_override_is_honored_and_normalized() {
    let home = TestHome::new();
    let _env = EnvRestore::set(
        &home,
        &[
            ("PINVOU_BEHAVIOR_TELEMETRY_ENABLED", "true"),
            (
                "PINVOU_TELEMETRY_BASE_URL",
                "https://telemetry.example.internal/pinvou3/telemetry/",
            ),
            (
                "PINVOU_BEHAVIOR_EVENTS_URL",
                "https://telemetry.example.internal/pinvou3/stats/api/behavior/events/",
            ),
            (
                "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
                "pinvou_tel_enroll_v1_override_token_ok24",
            ),
        ],
    );

    let runtime = RuntimeConfig::from_env();

    assert!(runtime.enabled);
    assert_eq!(
        runtime.telemetry_base_url,
        "https://telemetry.example.internal/pinvou3/telemetry"
    );
    assert_eq!(
        runtime.behavior_events_url,
        "https://telemetry.example.internal/pinvou3/stats/api/behavior/events"
    );
    assert_eq!(
        runtime.enrollment_token.as_deref(),
        Some("pinvou_tel_enroll_v1_override_token_ok24")
    );
}

#[test]
fn explicit_incomplete_runtime_override_fails_closed() {
    let home = TestHome::new();
    let _env = EnvRestore::set(
        &home,
        &[
            ("PINVOU_BEHAVIOR_TELEMETRY_ENABLED", "true"),
            ("PINVOU_TELEMETRY_BASE_URL", ""),
            ("PINVOU_BEHAVIOR_EVENTS_URL", ""),
            ("PINVOU_TELEMETRY_ENROLLMENT_TOKEN", "short"),
        ],
    );

    assert!(!RuntimeConfig::from_env().enabled);
}

#[test]
fn persistent_outbox_recovers_and_applies_per_event_results() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("outbox.jsonl");
    let now = now_ms();
    let mut outbox = Outbox::empty(path.clone());
    outbox
        .enqueue(outbox_record("event_accept", "period_one", now))
        .unwrap();
    outbox
        .enqueue(outbox_record("event_retry", "period_one", now))
        .unwrap();
    outbox
        .enqueue(outbox_record("event_reject", "period_one", now))
        .unwrap();

    let mut recovered = Outbox::load(path).unwrap();
    assert_eq!(recovered.records.len(), 3);
    let batch = recovered.batch();
    let results = HashMap::from([
        ("event_accept".to_string(), (DeliveryStatus::Accepted, None)),
        (
            "event_retry".to_string(),
            (DeliveryStatus::Retryable, Some("rate_limited".to_string())),
        ),
        (
            "event_reject".to_string(),
            (DeliveryStatus::Rejected, Some("invalid_field".to_string())),
        ),
    ]);
    assert!(recovered.apply_results(&batch, &results).unwrap());
    assert_eq!(recovered.records.len(), 1);
    assert_eq!(recovered.records[0].event_id, "event_retry");
    assert_eq!(recovered.records[0].delivery_attempt_count, 1);
    assert_eq!(
        recovered.records[0].last_reason_code.as_deref(),
        Some("rate_limited")
    );
}

#[test]
fn outbox_rebinds_pending_events_after_server_identity_expiry() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("outbox.jsonl");
    let mut outbox = Outbox::empty(path.clone());
    outbox
        .enqueue(outbox_record("event_rebind", "period_expired", now_ms()))
        .unwrap();
    outbox
        .rebind_period("period_renewed", 2, &HashMap::new())
        .unwrap();
    assert_eq!(outbox.records[0].collection_period_id, "period_renewed");
    assert_eq!(outbox.records[0].generation, 2);
    assert_eq!(
        outbox.records[0].event["collection_period_id"],
        "period_renewed"
    );
    let reloaded = Outbox::load(path).unwrap();
    assert_eq!(reloaded.records[0].collection_period_id, "period_renewed");
}

#[test]
fn persistent_outbox_enqueue_meets_latency_and_representative_memory_budget() {
    let directory = tempfile::tempdir().unwrap();
    let mut outbox = Outbox::empty(directory.path().join("outbox.jsonl"));
    let mut samples = Vec::with_capacity(200);
    for index in 0..200 {
        let started = std::time::Instant::now();
        outbox
            .enqueue(outbox_record(
                &format!("latency_event_{index:04}"),
                "period_performance",
                now_ms(),
            ))
            .unwrap();
        samples.push(started.elapsed());
    }
    samples.sort_unstable();
    let p95 = samples[(samples.len() * 95).div_ceil(100) - 1];
    let p99 = samples[(samples.len() * 99).div_ceil(100) - 1];
    assert!(p95 <= Duration::from_millis(10), "enqueue P95 {p95:?}");
    assert!(p99 <= Duration::from_millis(50), "enqueue P99 {p99:?}");

    let representative_bytes: usize = (0..OUTBOX_MAX_EVENTS)
        .map(|index| {
            Outbox::record_bytes(&outbox_record(
                &format!("memory_event_{index:04}"),
                "period_performance",
                now_ms(),
            ))
        })
        .sum();
    assert!(representative_bytes <= 16 * 1024 * 1024);
}

#[test]
fn outbox_enforces_period_age_count_and_batch_limits() {
    let now = now_ms();
    let directory = tempfile::tempdir().unwrap();
    let mut outbox = Outbox::empty(directory.path().join("outbox.jsonl"));
    outbox.records.push(outbox_record(
        "expired",
        "old_period",
        now - OUTBOX_RETENTION_MS - 1,
    ));
    for index in 0..=OUTBOX_MAX_EVENTS {
        outbox.records.push(outbox_record(
            &format!("event_{index}"),
            "period_current",
            now,
        ));
    }
    assert!(outbox.enforce_bounds(now));
    assert_eq!(outbox.records.len(), OUTBOX_MAX_EVENTS);
    assert!(
        outbox
            .records
            .iter()
            .all(|record| record.event_id != "expired")
    );
    assert!(outbox.dropped_event_count >= 2);

    outbox.retain_period(Some("different_period"), 1).unwrap();
    assert!(outbox.records.is_empty());
}

#[test]
fn oversized_front_event_is_dropped_instead_of_spinning_the_worker() {
    let directory = tempfile::tempdir().unwrap();
    let mut outbox = Outbox::empty(directory.path().join("outbox.jsonl"));
    let mut oversized = outbox_record("oversized", "period_one", now_ms());
    oversized.event = json!({ "payload": "x".repeat(EVENT_BATCH_MAX_BYTES + 1) });
    outbox.records.push(oversized);
    outbox
        .records
        .push(outbox_record("deliverable", "period_one", now_ms()));

    assert!(outbox.batch().is_empty());
    assert!(outbox.drop_oversized_front().unwrap());
    assert_eq!(outbox.dropped_event_count, 1);
    assert_eq!(outbox.batch()[0].event_id, "deliverable");
}

#[test]
fn upload_response_supports_v1_results_and_legacy_arrays() {
    let records = vec![
        outbox_record("event_a", "period_one", now_ms()),
        outbox_record("event_b", "period_one", now_ms()),
    ];
    let v1 = delivery_results(
        &records,
        EventUploadResponse {
            results: vec![EventDeliveryResult {
                event_id: "event_a".to_string(),
                status: "duplicate".to_string(),
                reason_code: None,
            }],
            ..EventUploadResponse::default()
        },
    );
    assert_eq!(v1["event_a"].0, DeliveryStatus::Duplicate);
    assert_eq!(v1["event_b"].0, DeliveryStatus::Retryable);

    let legacy = delivery_results(
        &records,
        EventUploadResponse {
            accepted: vec!["event_a".to_string()],
            rejected: vec!["event_b".to_string()],
            ..EventUploadResponse::default()
        },
    );
    assert_eq!(legacy["event_a"].0, DeliveryStatus::Accepted);
    assert_eq!(legacy["event_b"].0, DeliveryStatus::Rejected);
}

#[test]
fn accepted_delivery_exits_retry_mode_while_retryable_delivery_keeps_backoff() {
    let directory = tempfile::tempdir().unwrap();
    let now = now_ms();
    let mut outbox = Outbox::empty(directory.path().join("outbox.jsonl"));
    outbox
        .enqueue(outbox_record("event_accept", "period_one", now))
        .unwrap();
    let accepted_batch = outbox.batch();
    assert!(
        !outbox
            .apply_results(
                &accepted_batch,
                &HashMap::from([("event_accept".to_string(), (DeliveryStatus::Accepted, None),)]),
            )
            .unwrap()
    );

    outbox
        .enqueue(outbox_record("event_retry", "period_one", now))
        .unwrap();
    let retry_batch = outbox.batch();
    assert!(
        outbox
            .apply_results(
                &retry_batch,
                &HashMap::from([(
                    "event_retry".to_string(),
                    (DeliveryStatus::Retryable, Some("rate_limited".to_string())),
                )]),
            )
            .unwrap()
    );
    assert_eq!(outbox.records[0].delivery_attempt_count, 1);
}

#[test]
fn atomic_state_write_preserves_pending_delete_when_a_partial_sibling_exists() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("behavior-telemetry.json");
    let runtime = RuntimeConfig {
        enabled: true,
        telemetry_base_url: "https://example.test/pinvou3/telemetry".to_string(),
        behavior_events_url: "https://example.test/pinvou3/stats/api/behavior/events".to_string(),
        enrollment_token: None,
        app_version: "0.0.0".to_string(),
        platform: "test".to_string(),
        arch: "test".to_string(),
        channel: "dev".to_string(),
        locale: "en".to_string(),
    };
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("device_test".to_string());
    state.device_token = Some("token_test".to_string());
    state.control.pending_request = Some(PendingControlRequest {
        kind: PendingControlKind::Delete,
        request_id: "control_delete".to_string(),
        collection_period_id: Some("period_test".to_string()),
        behavior_events_url: state.behavior_events_url.clone(),
    });
    persist_state_at(&path, &state).unwrap();

    std::fs::write(
        directory
            .path()
            .join("behavior-telemetry.json.tmp.interrupted"),
        b"{\"version\":",
    )
    .unwrap();
    let recovered = load_state_at(&path).unwrap().unwrap();
    assert_eq!(recovered.device_id.as_deref(), Some("device_test"));
    let pending = recovered.control.pending_request.unwrap();
    assert!(matches!(pending.kind, PendingControlKind::Delete));
    assert_eq!(pending.request_id, "control_delete");
}

#[test]
fn future_state_version_fails_closed_instead_of_creating_a_new_identity() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("behavior-telemetry.json");
    let runtime = test_runtime("https://future.example.test");
    let mut state = PersistedState::fresh(&runtime);
    state.version = STATE_VERSION.saturating_add(1);
    state.installation_id = "future_installation_identity".to_string();
    std::fs::write(&path, serde_json::to_vec(&state).unwrap()).unwrap();

    let error = load_state_at(&path).unwrap_err();
    assert!(
        error
            .to_string()
            .contains("unsupported behavior telemetry state version")
    );
    let unchanged: PersistedState = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    assert_eq!(unchanged.installation_id, "future_installation_identity");
    assert_eq!(unchanged.version, STATE_VERSION + 1);
}

#[tokio::test(flavor = "current_thread")]
async fn completed_legacy_deletion_remains_blocked_across_restart() {
    let _home = TestHome::new();
    let (base_url, server) = mock_http(vec![(
        200,
        r#"{"deletion_request_id":"deletion_complete"}"#,
    )])
    .await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("legacy_device".to_string());
    state.device_token = Some("legacy_token".to_string());
    state.control.generation = 11;
    state.control.collection_blocked = true;
    let pending = PendingEndpointDeletion {
        request_id: "legacy_delete".to_string(),
        behavior_events_url: runtime.behavior_events_url.clone(),
        device_id: "legacy_device".to_string(),
        device_token: "legacy_token".to_string(),
    };
    state.control.pending_deletions.push(pending.clone());
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);

    telemetry.send_pending_deletion(pending).await.unwrap();
    let after = telemetry.state.lock().await.clone().unwrap();
    assert!(after.control.collection_blocked);
    assert_eq!(after.control.generation, 11);
    assert!(after.control.pending_deletions.is_empty());
    assert!(after.device_id.is_none());
    assert!(telemetry.set_collection_enabled(true).await.is_err());

    let recovered = load_state().unwrap().unwrap();
    assert!(recovered.control.collection_blocked);
    assert_eq!(recovered.control.generation, 11);
    assert!(recovered.device_id.is_none());
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /deletion "));
}

#[tokio::test(flavor = "current_thread")]
async fn endpoint_migration_end_410_deletion_blocks_and_queues_all_retired_endpoints() {
    let _home = TestHome::new();
    let (old_base, server) = mock_http(vec![
        (410, r#"{"error":"device_data_deleted"}"#),
        (200, r#"{"deletion_request_id":"delete_retired_a"}"#),
        (200, r#"{"deletion_request_id":"delete_retired_b"}"#),
    ])
    .await;
    let old_runtime = test_runtime(&old_base);
    let new_runtime = test_runtime("https://new.example.test");
    let mut state = PersistedState::fresh(&old_runtime);
    state.device_id = Some("deleted_device".to_string());
    state.device_token = Some("deleted_token".to_string());
    state.control.enabled = false;
    state.control.collection_period_id = Some("old_period".to_string());
    state.control.generation = 4;
    let pending = PendingControlRequest {
        kind: PendingControlKind::End,
        request_id: "end_old_endpoint".to_string(),
        collection_period_id: Some("old_period".to_string()),
        behavior_events_url: old_runtime.behavior_events_url.clone(),
    };
    state.control.pending_request = Some(pending.clone());
    state.retired_endpoints = vec![
        RetiredEndpointCredentials {
            behavior_events_url: old_runtime.behavior_events_url.clone(),
            device_id: "retired_a".to_string(),
            device_token: "token_a".to_string(),
        },
        RetiredEndpointCredentials {
            behavior_events_url: old_runtime.behavior_events_url.clone(),
            device_id: "retired_b".to_string(),
            device_token: "token_b".to_string(),
        },
    ];
    state
        .control
        .pending_deletions
        .push(PendingEndpointDeletion {
            request_id: "existing_retired_a_delete".to_string(),
            behavior_events_url: old_runtime.behavior_events_url.clone(),
            device_id: "retired_a".to_string(),
            device_token: "token_a".to_string(),
        });
    state
        .control
        .pending_deletions
        .push(PendingEndpointDeletion {
            request_id: "duplicate_retired_a_delete".to_string(),
            behavior_events_url: old_runtime.behavior_events_url.clone(),
            device_id: "retired_a".to_string(),
            device_token: "token_a".to_string(),
        });
    persist_state(&state).unwrap();
    let credentials = persisted_credentials_from_state(&state).unwrap();
    let mut telemetry = test_telemetry(new_runtime.clone(), state);
    let (control_tx, mut control_rx) = mpsc::channel(1);
    telemetry.control_tx = Some(control_tx);

    let error = telemetry
        .send_pending_control(&credentials, pending)
        .await
        .unwrap_err();
    assert!(error.to_string().contains("deleted while ending"));
    let after = telemetry.state.lock().await.clone().unwrap();
    assert!(after.control.collection_blocked);
    assert_eq!(after.control.generation, 5);
    assert_eq!(after.telemetry_base_url, new_runtime.telemetry_base_url);
    assert_eq!(after.control.pending_deletions.len(), 2);
    assert_eq!(
        after
            .control
            .pending_deletions
            .iter()
            .filter(|target| target.device_id == "retired_a")
            .count(),
        1
    );
    assert!(
        after
            .control
            .pending_deletions
            .iter()
            .any(|target| target.device_id == "retired_b")
    );
    assert!(control_rx.try_recv().is_ok());
    telemetry.retry_pending_control().await.unwrap();
    telemetry.retry_pending_control().await.unwrap();
    let after_retired_cleanup = telemetry.state.lock().await.clone().unwrap();
    assert!(after_retired_cleanup.control.collection_blocked);
    assert_eq!(after_retired_cleanup.control.generation, 5);
    assert!(after_retired_cleanup.control.pending_deletions.is_empty());
    assert!(after_retired_cleanup.retired_endpoints.is_empty());
    assert!(telemetry.set_collection_enabled(true).await.is_err());
    let persisted = load_state().unwrap().unwrap();
    assert!(persisted.control.collection_blocked);
    assert!(persisted.control.pending_deletions.is_empty());

    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 3);
    assert!(requests[0].starts_with("POST /collection/end "));
    assert!(requests[1].starts_with("POST /deletion "));
    assert!(requests[2].starts_with("POST /deletion "));
}

#[tokio::test(flavor = "current_thread")]
async fn unknown_json_410_keeps_pending_end_for_a_future_retry() {
    let _home = TestHome::new();
    let (base_url, server) = mock_http(vec![(410, r#"{"error":"future_reason"}"#)]).await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("device_unknown_410".to_string());
    state.device_token = Some("token_unknown_410".to_string());
    state.control.collection_period_id = Some("period_unknown_410".to_string());
    state.control.generation = 6;
    let pending = PendingControlRequest {
        kind: PendingControlKind::End,
        request_id: "end_unknown_410".to_string(),
        collection_period_id: Some("period_unknown_410".to_string()),
        behavior_events_url: runtime.behavior_events_url.clone(),
    };
    state.control.pending_request = Some(pending.clone());
    persist_state(&state).unwrap();
    let credentials = persisted_credentials_from_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);

    let error = telemetry
        .send_pending_control(&credentials, pending)
        .await
        .unwrap_err();
    assert!(error.to_string().contains("future_reason"));
    let after = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(after.control.generation, 6);
    assert!(after.control.pending_request.is_some());
    assert_eq!(
        after.control.collection_period_id.as_deref(),
        Some("period_unknown_410")
    );
    assert!(!after.control.collection_blocked);
    let persisted = load_state().unwrap().unwrap();
    assert!(persisted.control.pending_request.is_some());
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /collection/end "));
}

#[tokio::test(flavor = "current_thread")]
async fn non_json_410_keeps_pending_endpoint_deletion_unchanged() {
    let _home = TestHome::new();
    let (base_url, server) = mock_http(vec![(410, "not-json")]).await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("device_non_json".to_string());
    state.device_token = Some("token_non_json".to_string());
    state.control.collection_blocked = true;
    state.control.generation = 9;
    let pending = PendingEndpointDeletion {
        request_id: "delete_non_json".to_string(),
        behavior_events_url: runtime.behavior_events_url.clone(),
        device_id: "device_non_json".to_string(),
        device_token: "token_non_json".to_string(),
    };
    state.control.pending_deletions.push(pending.clone());
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);

    let error = telemetry
        .send_pending_deletion(pending.clone())
        .await
        .unwrap_err();
    assert!(error.to_string().contains("410 response"));
    let after = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(after.control.generation, 9);
    assert_eq!(after.control.pending_deletions.len(), 1);
    assert!(after.control.pending_deletions[0].same_request(&pending));
    assert_eq!(after.device_id.as_deref(), Some("device_non_json"));
    let persisted = load_state().unwrap().unwrap();
    assert_eq!(persisted.control.pending_deletions.len(), 1);
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /deletion "));
}

#[tokio::test(flavor = "current_thread")]
async fn legacy_delete_converges_then_drains_retired_endpoint_without_losing_state() {
    let _home = TestHome::new();
    let (base_url, server) = mock_http(vec![
        (200, r#"{"deletion_request_id":"delete_current"}"#),
        (200, r#"{"deletion_request_id":"delete_retired"}"#),
    ])
    .await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("current_device".to_string());
    state.device_token = Some("current_token".to_string());
    state.control.generation = 12;
    state.control.collection_period_id = Some("legacy_period".to_string());
    state.control.pending_request = Some(PendingControlRequest {
        kind: PendingControlKind::Delete,
        request_id: "legacy_delete_current".to_string(),
        collection_period_id: Some("legacy_period".to_string()),
        behavior_events_url: runtime.behavior_events_url.clone(),
    });
    state.retired_endpoints.push(RetiredEndpointCredentials {
        behavior_events_url: runtime.behavior_events_url.clone(),
        device_id: "retired_device".to_string(),
        device_token: "retired_token".to_string(),
    });
    state
        .control
        .pending_deletions
        .push(PendingEndpointDeletion {
            request_id: "existing_retired_delete".to_string(),
            behavior_events_url: runtime.behavior_events_url.clone(),
            device_id: "retired_device".to_string(),
            device_token: "retired_token".to_string(),
        });
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);

    telemetry.retry_pending_control().await.unwrap();
    let converged = telemetry.state.lock().await.clone().unwrap();
    assert!(converged.control.collection_blocked);
    assert_eq!(converged.control.generation, 12);
    assert!(converged.control.pending_request.is_none());
    assert!(converged.device_id.is_none());
    assert_eq!(converged.retired_endpoints.len(), 1);
    assert_eq!(converged.control.pending_deletions.len(), 1);
    assert_eq!(
        converged.control.last_deletion_request_id.as_deref(),
        Some("delete_current")
    );

    telemetry.retry_pending_control().await.unwrap();
    let drained = telemetry.state.lock().await.clone().unwrap();
    assert!(drained.control.collection_blocked);
    assert_eq!(drained.control.generation, 12);
    assert!(drained.control.pending_deletions.is_empty());
    assert!(drained.retired_endpoints.is_empty());
    let recovered = load_state().unwrap().unwrap();
    assert!(recovered.control.collection_blocked);
    assert!(recovered.control.pending_deletions.is_empty());
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 2);
    assert!(requests[0].starts_with("POST /deletion "));
    assert!(requests[1].starts_with("POST /deletion "));
}

#[tokio::test(flavor = "current_thread")]
async fn registration_installation_tombstone_persists_block_and_clears_transient_data() {
    let _home = TestHome::new();
    let (base_url, server) =
        mock_http(vec![(410, r#"{"error":"installation_data_deleted"}"#)]).await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    let installation_id = state.installation_id.clone();
    state.retired_endpoints.push(RetiredEndpointCredentials {
        behavior_events_url: "https://retired.example.test/events".to_string(),
        device_id: "retired_device".to_string(),
        device_token: "retired_token".to_string(),
    });
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);
    telemetry
        .outbox
        .lock()
        .unwrap()
        .enqueue(outbox_record(
            "pending_before_tombstone",
            "old_period",
            now_ms(),
        ))
        .unwrap();
    telemetry.tasks.lock().unwrap().insert(
        "session_before_tombstone".to_string(),
        TaskAttempt {
            attempt_id: "attempt_before_tombstone".to_string(),
            submitted_at: now_ms(),
            admitted_at: None,
            turn_id: None,
            mode: "assistant".to_string(),
            initiator_type: "user".to_string(),
            input_type: "text".to_string(),
            has_attachment: false,
            first_output_emitted: false,
            generation: 0,
            collection_period_id: "old_period".to_string(),
        },
    );

    let error = telemetry.ensure_credentials().await.unwrap_err();
    assert!(error.to_string().contains("installation data was deleted"));
    let blocked = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(blocked.installation_id, installation_id);
    assert!(blocked.control.collection_blocked);
    assert_eq!(blocked.control.generation, 1);
    assert!(blocked.device_id.is_none());
    assert!(blocked.device_token.is_none());
    assert_eq!(blocked.control.pending_deletions.len(), 1);
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    assert!(telemetry.tasks.lock().unwrap().is_empty());
    assert!(telemetry.set_collection_enabled(true).await.is_err());

    let recovered = load_state().unwrap().unwrap();
    assert!(recovered.control.collection_blocked);
    assert_eq!(recovered.installation_id, installation_id);
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /v1/register "));
}

#[tokio::test(flavor = "current_thread")]
async fn renewal_registration_installation_tombstone_also_fails_closed() {
    let _home = TestHome::new();
    let (base_url, server) =
        mock_http(vec![(410, r#"{"error":"installation_data_deleted"}"#)]).await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("expired_device".to_string());
    state.device_token = Some("expired_token".to_string());
    state.control.enabled = true;
    state.control.collection_period_id = Some("expired_period".to_string());
    state.control.generation = 3;
    persist_state(&state).unwrap();
    let expired = persisted_credentials_from_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);

    let error = telemetry
        .recover_expired_credentials_locked(&expired)
        .await
        .unwrap_err();
    assert!(error.to_string().contains("installation data was deleted"));
    let blocked = telemetry.state.lock().await.clone().unwrap();
    assert!(blocked.control.collection_blocked);
    assert!(!blocked.control.enabled);
    assert!(blocked.control.collection_period_id.is_none());
    assert_eq!(blocked.control.generation, 4);
    assert!(blocked.device_id.is_none());
    assert!(blocked.device_token.is_none());
    assert!(telemetry.set_collection_enabled(true).await.is_err());
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /v1/register "));
}

#[tokio::test(flavor = "current_thread")]
async fn initial_registration_tombstone_at_max_generation_persists_fail_close() {
    let _home = TestHome::new();
    let (base_url, server) =
        mock_http(vec![(410, r#"{"error":"installation_data_deleted"}"#)]).await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.control.generation = u64::MAX;
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);
    telemetry
        .outbox
        .lock()
        .unwrap()
        .enqueue(outbox_record(
            "max_initial_outbox",
            "stale_period",
            now_ms(),
        ))
        .unwrap();

    let error = telemetry.ensure_credentials().await.unwrap_err();
    assert!(error.to_string().contains("generation exhausted"));
    assert!(!telemetry.collecting.load(Ordering::Acquire));
    assert_eq!(
        telemetry.authorization_generation.load(Ordering::Acquire),
        u64::MAX
    );
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    let blocked = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(blocked.control.generation, u64::MAX);
    assert!(blocked.control.collection_blocked);
    assert!(!blocked.control.enabled);
    assert!(blocked.control.collection_period_id.is_none());
    assert!(blocked.device_id.is_none());
    let persisted = load_state().unwrap().unwrap();
    assert_eq!(persisted.control.generation, u64::MAX);
    assert!(persisted.control.collection_blocked);

    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /v1/register "));
}

#[tokio::test(flavor = "current_thread")]
async fn renewal_registration_tombstone_at_max_generation_cleans_active_state() {
    let _home = TestHome::new();
    let (base_url, server) =
        mock_http(vec![(410, r#"{"error":"installation_data_deleted"}"#)]).await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("max_expired_device".to_string());
    state.device_token = Some("max_expired_token".to_string());
    state.control.enabled = true;
    state.control.collection_period_id = Some("max_expired_period".to_string());
    state.control.generation = u64::MAX;
    persist_state(&state).unwrap();
    let expired = persisted_credentials_from_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);
    telemetry
        .outbox
        .lock()
        .unwrap()
        .enqueue(outbox_record(
            "max_renewal_outbox",
            "max_expired_period",
            now_ms(),
        ))
        .unwrap();

    let error = telemetry
        .recover_expired_credentials_locked(&expired)
        .await
        .unwrap_err();
    assert!(error.to_string().contains("generation exhausted"));
    let blocked = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(blocked.control.generation, u64::MAX);
    assert!(blocked.control.collection_blocked);
    assert!(!blocked.control.enabled);
    assert!(blocked.control.collection_period_id.is_none());
    assert!(blocked.device_id.is_none());
    assert!(blocked.device_token.is_none());
    assert!(!telemetry.collecting.load(Ordering::Acquire));
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    let persisted = load_state().unwrap().unwrap();
    assert_eq!(persisted.control.generation, u64::MAX);
    assert!(persisted.control.collection_blocked);

    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /v1/register "));
}

#[tokio::test(flavor = "current_thread")]
async fn authoritative_device_deletion_at_max_generation_never_wraps() {
    let _home = TestHome::new();
    let (base_url, server) = mock_http(vec![(410, r#"{"error":"device_data_deleted"}"#)]).await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("max_deleted_device".to_string());
    state.device_token = Some("max_deleted_token".to_string());
    state.control.enabled = true;
    state.control.collection_period_id = Some("max_deleted_period".to_string());
    state.control.generation = u64::MAX;
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);
    telemetry
        .credential_renewal_in_progress
        .store(true, Ordering::Release);
    telemetry.collecting.store(false, Ordering::Release);
    telemetry.complete_tool_install_terminal(
        "max_deleted_install".to_string(),
        u64::MAX,
        BehaviorEvent::new("tool_install_completed")
            .string("operation_id", "max_deleted_install")
            .success(true),
    );
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 1);
    telemetry.collecting.store(true, Ordering::Release);
    let record = outbox_record("max_deleted_event", "max_deleted_period", now_ms());
    telemetry
        .outbox
        .lock()
        .unwrap()
        .enqueue(record.clone())
        .unwrap();

    let error = telemetry.send_events(&[record]).await.unwrap_err();
    assert!(error.to_string().contains("generation exhausted"));
    let blocked = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(blocked.control.generation, u64::MAX);
    assert!(blocked.control.collection_blocked);
    assert!(!blocked.control.enabled);
    assert!(blocked.device_id.is_none());
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 0);
    assert!(
        !telemetry
            .credential_renewal_in_progress
            .load(Ordering::Acquire)
    );
    telemetry.flush_tool_install_terminals(u64::MAX);
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    assert!(!telemetry.collecting.load(Ordering::Acquire));
    assert_eq!(
        telemetry.authorization_generation.load(Ordering::Acquire),
        u64::MAX
    );

    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /events "));
}

#[tokio::test(flavor = "current_thread")]
async fn registration_tombstone_merges_concurrent_control_state_before_worker_cleanup() {
    let _home = TestHome::new();
    let (deletion_base_url, deletion_server) = mock_http(vec![
        (200, r#"{"collection_period_id":"concurrent_period"}"#),
        (200, r#"{"deletion_request_id":"delete_existing"}"#),
        (200, r#"{"deletion_request_id":"delete_retired"}"#),
        (200, r#"{"deletion_request_id":"delete_migrated"}"#),
    ])
    .await;
    let (registration_base_url, request_seen, release_response, registration_server) =
        controlled_http(410, r#"{"error":"installation_data_deleted"}"#).await;
    let old_runtime = test_runtime(&deletion_base_url);
    let new_runtime = test_runtime(&registration_base_url);
    let mut state = PersistedState::fresh(&old_runtime);
    state.device_id = Some("migrated_device".to_string());
    state.device_token = Some("migrated_token".to_string());
    state.control.generation = 7;
    persist_state(&state).unwrap();

    let mut telemetry = test_telemetry(new_runtime, state);
    let (control_tx, control_rx) = mpsc::channel(1);
    telemetry.control_tx = Some(control_tx);
    let registration = {
        let telemetry = telemetry.clone();
        tokio::spawn(async move { telemetry.ensure_credentials().await })
    };
    request_seen.await.unwrap();

    let concurrent_end = PendingControlRequest {
        kind: PendingControlKind::End,
        request_id: "concurrent_end".to_string(),
        collection_period_id: Some("concurrent_period".to_string()),
        behavior_events_url: old_runtime.behavior_events_url.clone(),
    };
    {
        let mut guard = telemetry.state.lock().await;
        let current = guard.as_mut().unwrap();
        current.control.enabled = false;
        current.control.collection_period_id = Some("concurrent_period".to_string());
        current.control.generation = 40;
        current.control.collection_blocked = true;
        current.control.pending_request = Some(concurrent_end.clone());
        current.retired_endpoints.push(RetiredEndpointCredentials {
            behavior_events_url: old_runtime.behavior_events_url.clone(),
            device_id: "retired_device".to_string(),
            device_token: "retired_token".to_string(),
        });
        current
            .control
            .pending_deletions
            .push(PendingEndpointDeletion {
                request_id: "existing_delete".to_string(),
                behavior_events_url: old_runtime.behavior_events_url.clone(),
                device_id: "existing_pending_device".to_string(),
                device_token: "existing_pending_token".to_string(),
            });
        persist_state(current).unwrap();
    }
    telemetry
        .authorization_generation
        .store(50, Ordering::Release);
    release_response.send(()).unwrap();

    let error = registration.await.unwrap().unwrap_err();
    assert!(error.to_string().contains("installation data was deleted"));
    let converged = telemetry.state.lock().await.clone().unwrap();
    assert!(converged.control.collection_blocked);
    assert!(!converged.control.enabled);
    assert_eq!(converged.control.generation, 51);
    assert_eq!(
        telemetry.authorization_generation.load(Ordering::Acquire),
        51
    );
    assert!(pending_request_matches(&converged, &concurrent_end));
    assert_eq!(converged.device_id.as_deref(), Some("migrated_device"));
    assert_eq!(converged.retired_endpoints.len(), 2);
    assert!(
        converged
            .retired_endpoints
            .iter()
            .any(|retired| retired.device_id == "retired_device")
    );
    assert!(
        converged
            .retired_endpoints
            .iter()
            .any(|retired| retired.device_id == "migrated_device")
    );
    assert_eq!(converged.control.pending_deletions.len(), 3);
    assert!(
        converged
            .control
            .pending_deletions
            .iter()
            .any(|pending| pending.request_id == "existing_delete")
    );
    for device_id in [
        "existing_pending_device",
        "retired_device",
        "migrated_device",
    ] {
        assert_eq!(
            converged
                .control
                .pending_deletions
                .iter()
                .filter(|pending| pending.device_id == device_id)
                .count(),
            1,
            "deletion responsibility for {device_id} must be preserved exactly once"
        );
    }
    let persisted = load_state().unwrap().unwrap();
    assert_eq!(persisted.control.generation, 51);
    assert!(pending_request_matches(&persisted, &concurrent_end));
    assert_eq!(persisted.control.pending_deletions.len(), 3);

    let mut control_worker = telemetry.worker_clone();
    control_worker.control_tx = None;
    drop(telemetry.control_tx.take());
    tokio::time::timeout(
        Duration::from_secs(3),
        BehaviorTelemetry::run_control_worker(control_rx, control_worker),
    )
    .await
    .expect("control worker should drain merged installation deletion duties");

    let cleaned = telemetry.state.lock().await.clone().unwrap();
    assert!(cleaned.control.collection_blocked);
    assert_eq!(cleaned.control.generation, 51);
    assert!(cleaned.control.pending_request.is_none());
    assert!(cleaned.control.pending_deletions.is_empty());
    assert!(cleaned.retired_endpoints.is_empty());
    assert!(cleaned.device_id.is_none());
    assert!(cleaned.device_token.is_none());
    let persisted = load_state().unwrap().unwrap();
    assert!(persisted.control.collection_blocked);
    assert_eq!(persisted.control.generation, 51);
    assert!(persisted.control.pending_deletions.is_empty());

    let registration_request = registration_server.await.unwrap();
    assert!(registration_request.starts_with("POST /v1/register "));
    let deletion_requests = deletion_server.await.unwrap();
    assert_eq!(deletion_requests.len(), 4);
    assert!(deletion_requests[0].starts_with("POST /collection/end "));
    assert!(
        deletion_requests[1..]
            .iter()
            .all(|request| request.starts_with("POST /deletion "))
    );
}

#[tokio::test(flavor = "current_thread")]
async fn registration_success_cannot_overwrite_concurrent_stop_or_delete_state() {
    let _home = TestHome::new();
    for authoritative_delete in [false, true] {
        let (base_url, request_seen, release_response, server) = controlled_http(
            200,
            r#"{"device_id":"registered_device","device_token":"registered_token"}"#,
        )
        .await;
        let runtime = test_runtime(&base_url);
        let state = PersistedState::fresh(&runtime);
        persist_state(&state).unwrap();
        let telemetry = test_telemetry(runtime, state);
        let registration = {
            let telemetry = telemetry.clone();
            tokio::spawn(async move { telemetry.ensure_credentials().await })
        };
        request_seen.await.unwrap();
        {
            let mut guard = telemetry.state.lock().await;
            let state = guard.as_mut().unwrap();
            state.control.generation = 1;
            state.control.enabled = false;
            state.control.collection_blocked = authoritative_delete;
            if authoritative_delete {
                state
                    .control
                    .pending_deletions
                    .push(PendingEndpointDeletion {
                        request_id: "concurrent_delete".to_string(),
                        behavior_events_url: state.behavior_events_url.clone(),
                        device_id: "concurrent_device".to_string(),
                        device_token: "concurrent_token".to_string(),
                    });
            }
            persist_state(state).unwrap();
        }
        release_response.send(()).unwrap();
        let error = registration.await.unwrap().unwrap_err();
        assert!(error.to_string().contains("state changed"));
        let after = telemetry.state.lock().await.clone().unwrap();
        assert_eq!(after.control.generation, 1);
        assert_eq!(after.control.collection_blocked, authoritative_delete);
        assert!(after.device_id.is_none());
        assert!(after.device_token.is_none());
        if authoritative_delete {
            assert_eq!(after.control.pending_deletions.len(), 1);
        }
        let persisted = load_state().unwrap().unwrap();
        assert_eq!(persisted.control.generation, 1);
        assert_eq!(persisted.control.collection_blocked, authoritative_delete);
        assert!(persisted.device_id.is_none());
        let request = server.await.unwrap();
        assert!(request.starts_with("POST /v1/register "));
    }
}

#[tokio::test(flavor = "current_thread")]
async fn operational_stop_discards_old_outbox_and_restore_starts_a_new_period() {
    let _home = TestHome::new();
    let (base_url, server) = mock_http(vec![
        (200, r#"{"collection_period_id":"old_period"}"#),
        (200, r#"{"collection_period_id":"new_period"}"#),
    ])
    .await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("active_device".to_string());
    state.device_token = Some("active_token".to_string());
    state.control.enabled = true;
    state.control.collection_period_id = Some("old_period".to_string());
    state.control.generation = 7;
    persist_state(&state).unwrap();
    let mut outbox = Outbox::empty(outbox_path());
    outbox
        .enqueue(outbox_record("old_pending_event", "old_period", now_ms()))
        .unwrap();
    let _env = EnvRestore::set(
        &_home,
        &[
            ("PINVOU_BEHAVIOR_TELEMETRY_ENABLED", "false"),
            ("PINVOU_TELEMETRY_BASE_URL", &runtime.telemetry_base_url),
            ("PINVOU_BEHAVIOR_EVENTS_URL", &runtime.behavior_events_url),
            (
                "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
                "test-enrollment-token-1234567890",
            ),
        ],
    );

    let stopped = BehaviorTelemetry::new();
    assert!(!stopped.collecting.load(Ordering::Acquire));
    assert!(stopped.outbox.lock().unwrap().records.is_empty());
    let stopped_state = tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let snapshot = stopped.state.lock().await.clone().unwrap();
            if snapshot.control.pending_request.is_none()
                && snapshot.control.collection_period_id.is_none()
            {
                break snapshot;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("kill-switch control worker should finish the pending End");
    assert!(!stopped_state.control.enabled);
    assert_eq!(stopped_state.control.generation, 8);
    assert!(stopped_state.control.pending_request.is_none());
    assert!(stopped_state.control.collection_period_id.is_none());
    let mut repeated_stop = stopped_state.clone();
    assert!(!prepare_operational_stop(&mut repeated_stop));
    assert_eq!(repeated_stop.control.generation, 8);
    assert!(repeated_stop.control.pending_request.is_none());
    drop(stopped);

    let restored = test_telemetry(runtime, stopped_state);
    restored.set_collection_enabled(true).await.unwrap();
    let restored_state = restored.state.lock().await.clone().unwrap();
    assert!(restored_state.control.enabled);
    assert_eq!(restored_state.control.generation, 9);
    assert_eq!(
        restored_state.control.collection_period_id.as_deref(),
        Some("new_period")
    );
    let restored_outbox = restored.outbox.lock().unwrap();
    assert!(
        restored_outbox
            .records
            .iter()
            .all(|record| record.event_id != "old_pending_event"
                && record.collection_period_id == "new_period"
                && record.generation == 9)
    );
    assert!(restored_outbox.records.iter().any(|record| {
        record.event.get("event_name").and_then(Value::as_str) == Some("analytics_cohort_started")
    }));
    drop(restored_outbox);
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 2);
    assert!(requests[0].starts_with("POST /collection/end "));
    assert!(requests[1].starts_with("POST /collection/start "));
}

#[tokio::test(flavor = "current_thread")]
async fn max_generation_stop_persists_block_and_restart_discards_old_outbox() {
    let _home = TestHome::new();
    let (base_url, server) =
        mock_http(vec![(200, r#"{"collection_period_id":"max_period"}"#)]).await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("max_active_device".to_string());
    state.device_token = Some("max_active_token".to_string());
    state.control.enabled = true;
    state.control.collection_period_id = Some("max_period".to_string());
    state.control.generation = u64::MAX;
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime.clone(), state);
    telemetry
        .credential_renewal_in_progress
        .store(true, Ordering::Release);
    telemetry.collecting.store(false, Ordering::Release);
    telemetry.complete_tool_install_terminal(
        "max_stop_install".to_string(),
        u64::MAX,
        BehaviorEvent::new("tool_install_completed")
            .string("operation_id", "max_stop_install")
            .success(true),
    );
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 1);
    telemetry.collecting.store(true, Ordering::Release);
    telemetry
        .outbox
        .lock()
        .unwrap()
        .enqueue(outbox_record("max_old_event", "max_period", now_ms()))
        .unwrap();

    let error = telemetry.set_collection_enabled(false).await.unwrap_err();
    assert!(error.to_string().contains("generation exhausted"));
    assert!(!telemetry.collecting.load(Ordering::Acquire));
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 0);
    assert!(
        !telemetry
            .credential_renewal_in_progress
            .load(Ordering::Acquire)
    );
    telemetry.flush_tool_install_terminals(u64::MAX);
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    let stopped = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(stopped.control.generation, u64::MAX);
    assert!(stopped.control.collection_blocked);
    assert!(stopped.control.collection_period_id.is_none());
    assert!(matches!(
        stopped
            .control
            .pending_request
            .as_ref()
            .map(|pending| pending.kind),
        Some(PendingControlKind::End)
    ));
    drop(telemetry);

    let _env = EnvRestore::set(
        &_home,
        &[
            ("PINVOU_BEHAVIOR_TELEMETRY_ENABLED", "true"),
            ("PINVOU_TELEMETRY_BASE_URL", &runtime.telemetry_base_url),
            ("PINVOU_BEHAVIOR_EVENTS_URL", &runtime.behavior_events_url),
            (
                "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
                "test-enrollment-token-1234567890",
            ),
        ],
    );
    let restarted = BehaviorTelemetry::new();
    assert!(!restarted.collecting.load(Ordering::Acquire));
    assert_eq!(
        restarted.authorization_generation.load(Ordering::Acquire),
        u64::MAX
    );
    assert!(restarted.outbox.lock().unwrap().records.is_empty());
    let restarted_state = tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let snapshot = restarted.state.lock().await.clone().unwrap();
            if snapshot.control.pending_request.is_none() {
                break snapshot;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("restart control worker should finish the MAX generation End");
    assert_eq!(restarted_state.control.generation, u64::MAX);
    assert!(restarted_state.control.collection_blocked);
    assert!(!restarted_state.control.enabled);
    assert!(restarted_state.control.collection_period_id.is_none());
    assert!(!restarted.collecting.load(Ordering::Acquire));

    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /collection/end "));
}

#[tokio::test(flavor = "current_thread")]
async fn max_generation_start_ends_new_remote_period_and_stays_blocked() {
    let _home = TestHome::new();
    let (base_url, server) = mock_http(vec![
        (200, r#"{"collection_period_id":"started_at_max"}"#),
        (200, r#"{"collection_period_id":"started_at_max"}"#),
    ])
    .await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("max_start_device".to_string());
    state.device_token = Some("max_start_token".to_string());
    state.control.generation = u64::MAX;
    persist_state(&state).unwrap();
    let mut telemetry = test_telemetry(runtime, state);
    let (control_tx, control_rx) = mpsc::channel(1);
    telemetry.control_tx = Some(control_tx);

    let error = telemetry.set_collection_enabled(true).await.unwrap_err();
    assert!(error.to_string().contains("generation exhausted"));

    // The failed Start must have queued a control wake-up. Close the sender and
    // drive the real worker future directly so this assertion does not depend on
    // when a detached task happens to be scheduled under the full parallel suite.
    drop(telemetry.control_tx.take());
    let mut control_worker = telemetry.worker_clone();
    control_worker.control_tx = None;
    tokio::time::timeout(
        Duration::from_secs(3),
        BehaviorTelemetry::run_control_worker(control_rx, control_worker),
    )
    .await
    .expect("control worker should drain the queued End and stop");

    let blocked = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(blocked.control.generation, u64::MAX);
    assert!(blocked.control.collection_blocked);
    assert!(!blocked.control.enabled);
    assert!(blocked.control.collection_period_id.is_none());
    assert!(blocked.control.pending_request.is_none());
    assert!(!telemetry.collecting.load(Ordering::Acquire));
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());

    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 2);
    assert!(requests[0].starts_with("POST /collection/start "));
    assert!(requests[1].starts_with("POST /collection/end "));
    assert!(
        requests
            .iter()
            .all(|request| !request.starts_with("POST /v1/register ")
                && !request.starts_with("POST /events "))
    );
}

#[tokio::test(flavor = "current_thread")]
async fn max_generation_remote_delete_drains_without_wraparound() {
    let _home = TestHome::new();
    let (base_url, server) = mock_http(vec![(
        200,
        r#"{"deletion_request_id":"max_delete_complete"}"#,
    )])
    .await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("max_delete_device".to_string());
    state.device_token = Some("max_delete_token".to_string());
    state.control.generation = u64::MAX;
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);

    let error = telemetry.delete_remote_data().await.unwrap_err();
    assert!(error.to_string().contains("generation exhausted"));
    let pending = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(pending.control.generation, u64::MAX);
    assert!(pending.control.collection_blocked);
    assert_eq!(pending.control.pending_deletions.len(), 1);

    telemetry.retry_pending_control().await.unwrap();
    let cleaned = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(cleaned.control.generation, u64::MAX);
    assert!(cleaned.control.collection_blocked);
    assert!(cleaned.control.pending_deletions.is_empty());
    assert!(cleaned.device_id.is_none());
    assert_eq!(
        telemetry.authorization_generation.load(Ordering::Acquire),
        u64::MAX
    );

    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].starts_with("POST /deletion "));
}

#[tokio::test(flavor = "current_thread")]
async fn max_generation_legacy_delete_preserves_followup_queue() {
    let _home = TestHome::new();
    let (base_url, server) = mock_http(vec![
        (410, r#"{"error":"device_data_deleted"}"#),
        (200, r#"{"deletion_request_id":"retired_complete"}"#),
    ])
    .await;
    let runtime = test_runtime(&base_url);
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("max_legacy_device".to_string());
    state.device_token = Some("max_legacy_token".to_string());
    state.control.generation = u64::MAX;
    state.control.pending_request = Some(PendingControlRequest {
        kind: PendingControlKind::Delete,
        request_id: "max_legacy_delete".to_string(),
        collection_period_id: None,
        behavior_events_url: runtime.behavior_events_url.clone(),
    });
    state.retired_endpoints.push(RetiredEndpointCredentials {
        behavior_events_url: runtime.behavior_events_url.clone(),
        device_id: "max_retired_device".to_string(),
        device_token: "max_retired_token".to_string(),
    });
    persist_state(&state).unwrap();
    let telemetry = test_telemetry(runtime, state);

    let error = telemetry.retry_pending_control().await.unwrap_err();
    assert!(error.to_string().contains("generation exhausted"));
    let converged = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(converged.control.generation, u64::MAX);
    assert!(converged.control.collection_blocked);
    assert!(converged.control.pending_request.is_none());
    assert_eq!(converged.control.pending_deletions.len(), 1);
    assert_eq!(
        converged.control.pending_deletions[0].device_id,
        "max_retired_device"
    );

    telemetry.retry_pending_control().await.unwrap();
    let cleaned = telemetry.state.lock().await.clone().unwrap();
    assert_eq!(cleaned.control.generation, u64::MAX);
    assert!(cleaned.control.collection_blocked);
    assert!(cleaned.control.pending_deletions.is_empty());
    assert!(cleaned.retired_endpoints.is_empty());

    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 2);
    assert!(
        requests
            .iter()
            .all(|request| request.starts_with("POST /deletion "))
    );
}

#[test]
fn endpoint_rotation_keeps_pending_control_bound_to_the_previous_endpoint() {
    let runtime_a = RuntimeConfig {
        enabled: true,
        telemetry_base_url: "https://a.example/telemetry".to_string(),
        behavior_events_url: "https://a.example/stats/behavior/events".to_string(),
        enrollment_token: None,
        app_version: "0.0.0".to_string(),
        platform: "test".to_string(),
        arch: "test".to_string(),
        channel: "dev".to_string(),
        locale: "en".to_string(),
    };
    let mut runtime_b = runtime_a.clone();
    runtime_b.telemetry_base_url = "https://b.example/telemetry".to_string();
    runtime_b.behavior_events_url = "https://b.example/stats/behavior/events".to_string();
    let mut old_state = PersistedState::fresh(&runtime_a);
    old_state.device_id = Some("device_a".to_string());
    old_state.device_token = Some("token_a".to_string());
    old_state.control.collection_period_id = Some("period_a".to_string());
    let pending = PendingControlRequest {
        kind: PendingControlKind::End,
        request_id: "end_a".to_string(),
        collection_period_id: Some("period_a".to_string()),
        behavior_events_url: old_state.behavior_events_url.clone(),
    };
    old_state.control.pending_request = Some(pending.clone());

    assert!(!endpoint_matches_runtime(&old_state, &runtime_b));
    let credentials = persisted_credentials_from_state(&old_state).unwrap();
    assert_eq!(
        credentials.behavior_events_url,
        runtime_a.behavior_events_url
    );
    assert_eq!(pending.behavior_events_url, runtime_a.behavior_events_url);
    assert!(pending_request_matches(&old_state, &pending));

    old_state.control.pending_request = None;
    old_state.control.collection_period_id = None;
    old_state.control.cohort_started = true;
    let installation_id = old_state.installation_id.clone();
    let registration_secret = old_state.registration_secret.clone();
    let mut new_state = old_state.rebind_endpoint(&runtime_b);
    new_state.control.enabled = true;
    new_state.control.collection_period_id = Some("period_b".to_string());
    assert!(!pending_request_matches(&new_state, &pending));
    assert_eq!(new_state.installation_id, installation_id);
    assert_eq!(new_state.registration_secret, registration_secret);
    assert!(new_state.control.cohort_started);
    assert_eq!(new_state.retired_endpoints.len(), 1);
    assert_eq!(
        new_state.retired_endpoints[0].behavior_events_url,
        runtime_a.behavior_events_url
    );
    assert_eq!(new_state.retired_endpoints[0].device_id, "device_a");
    assert_eq!(
        new_state.control.collection_period_id.as_deref(),
        Some("period_b")
    );

    new_state.device_id = Some("device_b".to_string());
    new_state.device_token = Some("token_b".to_string());
    let deletion_targets = new_state.deletion_targets();
    assert_eq!(deletion_targets.len(), 2);
    assert!(deletion_targets.iter().any(|target| {
        target.behavior_events_url == runtime_a.behavior_events_url
            && target.device_id == "device_a"
    }));
    assert!(deletion_targets.iter().any(|target| {
        target.behavior_events_url == runtime_b.behavior_events_url
            && target.device_id == "device_b"
    }));

    let mut same_backend_state = PersistedState::fresh(&runtime_b);
    same_backend_state.device_id = Some("shared_device".to_string());
    same_backend_state.device_token = Some("shared_token".to_string());
    same_backend_state
        .retired_endpoints
        .push(RetiredEndpointCredentials {
            behavior_events_url: runtime_a.behavior_events_url.clone(),
            device_id: "shared_device".to_string(),
            device_token: "shared_token".to_string(),
        });
    let same_backend_targets = same_backend_state.deletion_targets();
    assert_eq!(same_backend_targets.len(), 2);
    assert!(
        same_backend_targets
            .iter()
            .any(|target| target.behavior_events_url == runtime_b.behavior_events_url)
    );
    assert!(
        same_backend_targets
            .iter()
            .any(|target| target.behavior_events_url == runtime_a.behavior_events_url)
    );
}

#[test]
fn authoritative_deletion_blocks_default_restart_for_the_matching_credential() {
    let runtime_a = RuntimeConfig {
        enabled: true,
        telemetry_base_url: "https://a.example.test/pinvou3/telemetry".to_string(),
        behavior_events_url: "https://a.example.test/pinvou3/stats/api/behavior/events".to_string(),
        enrollment_token: None,
        app_version: "0.0.0".to_string(),
        platform: "test".to_string(),
        arch: "test".to_string(),
        channel: "dev".to_string(),
        locale: "en".to_string(),
    };
    let mut runtime_b = runtime_a.clone();
    runtime_b.telemetry_base_url = "https://b.example.test/pinvou3/telemetry".to_string();
    runtime_b.behavior_events_url =
        "https://b.example.test/pinvou3/stats/api/behavior/events".to_string();

    let mut state = PersistedState::fresh(&runtime_a);
    state.device_id = Some("device_a".to_string());
    state.device_token = Some("token_a".to_string());
    state = state.rebind_endpoint(&runtime_b);
    let previous_installation_id = state.installation_id.clone();
    let previous_registration_secret = state.registration_secret.clone();
    state.device_id = Some("device_b".to_string());
    state.device_token = Some("token_b".to_string());
    state.control.enabled = true;
    state.control.collection_period_id = Some("period_deleted".to_string());
    state.control.generation = 7;
    state.control.pending_deletions = vec![
        PendingEndpointDeletion {
            request_id: "delete_a".to_string(),
            behavior_events_url: runtime_a.behavior_events_url.clone(),
            device_id: "device_a".to_string(),
            device_token: "token_a".to_string(),
        },
        PendingEndpointDeletion {
            request_id: "delete_b".to_string(),
            behavior_events_url: runtime_b.behavior_events_url.clone(),
            device_id: "device_b".to_string(),
            device_token: "token_b".to_string(),
        },
    ];
    let deleted = Credentials {
        device_id: "device_b".to_string(),
        device_token: "token_b".to_string(),
        behavior_events_url: runtime_b.behavior_events_url.clone(),
    };

    assert!(deleted_credentials_match(&state, &deleted));
    let reset = state_after_authoritative_deletion(&state, &runtime_b, &deleted, 8);
    assert!(!reset.control.enabled);
    assert!(reset.control.collection_blocked);
    assert!(reset.control.collection_period_id.is_none());
    assert!(reset.device_id.is_none());
    assert!(reset.device_token.is_none());
    assert_eq!(reset.control.generation, 8);
    assert_ne!(reset.installation_id, previous_installation_id);
    assert_ne!(reset.registration_secret, previous_registration_secret);
    assert_eq!(reset.retired_endpoints.len(), 1);
    assert_eq!(reset.retired_endpoints[0].device_id, "device_a");
    assert_eq!(reset.control.pending_deletions.len(), 1);
    assert_eq!(reset.control.pending_deletions[0].device_id, "device_a");
    let future_deletion_targets = reset.deletion_targets();
    assert_eq!(future_deletion_targets.len(), 1);
    assert_eq!(future_deletion_targets[0].device_id, "device_a");
    assert_eq!(
        future_deletion_targets[0].behavior_events_url,
        runtime_a.behavior_events_url
    );

    let mut newer = state;
    newer.device_token = Some("token_newer".to_string());
    assert!(!deleted_credentials_match(&newer, &deleted));
}

#[test]
fn disabled_state_survives_restart_before_fallible_cleanup() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("behavior-telemetry.json");
    let runtime = RuntimeConfig {
        enabled: true,
        telemetry_base_url: "https://example.test/pinvou3/telemetry".to_string(),
        behavior_events_url: "https://example.test/pinvou3/stats/api/behavior/events".to_string(),
        enrollment_token: None,
        app_version: "0.0.0".to_string(),
        platform: "test".to_string(),
        arch: "test".to_string(),
        channel: "dev".to_string(),
        locale: "en".to_string(),
    };
    let mut state = PersistedState::fresh(&runtime);
    state.device_id = Some("device_active".to_string());
    state.device_token = Some("token_active".to_string());
    state.control.enabled = true;
    state.control.collection_period_id = Some("period_active".to_string());
    state.control.generation = 4;

    prepare_disabled_state(&mut state, 5);
    persist_state_at(&path, &state).unwrap();

    // Simulate termination before any fallible outbox/task cleanup runs.
    let recovered = load_state_at(&path).unwrap().unwrap();
    assert!(!recovered.control.enabled);
    assert_eq!(recovered.control.generation, 5);
    let pending = recovered.control.pending_request.unwrap();
    assert_eq!(pending.kind, PendingControlKind::End);
    assert_eq!(
        pending.collection_period_id.as_deref(),
        Some("period_active")
    );
    assert_eq!(pending.behavior_events_url, runtime.behavior_events_url);
}

#[test]
fn tool_install_terminal_waits_for_renewal_and_drops_after_reauthorization() {
    use std::sync::Barrier;
    use std::thread;

    fn publish(
        collecting: &AtomicBool,
        generation: &AtomicU64,
        period: &RwLock<Option<String>>,
        delivered: &StdMutex<Vec<(String, String)>>,
        event: &BehaviorEvent,
        expected_generation: u64,
    ) -> bool {
        if !collecting.load(Ordering::Acquire)
            || generation.load(Ordering::Acquire) != expected_generation
        {
            return false;
        }
        let Some(period_id) = period.read().unwrap().clone() else {
            return false;
        };
        delivered
            .lock()
            .unwrap()
            .push((event.event_id.clone(), period_id));
        true
    }

    let coordinator = ToolInstallTerminalCoordinator::default();
    let collecting = Arc::new(AtomicBool::new(true));
    let generation = Arc::new(AtomicU64::new(12));
    let renewal_in_progress = Arc::new(AtomicBool::new(false));
    let period = Arc::new(RwLock::new(Some("period_old".to_string())));
    let delivered = Arc::new(StdMutex::new(Vec::<(String, String)>::new()));
    let renewal_paused = Arc::new(Barrier::new(2));
    let renewal_resume = Arc::new(Barrier::new(2));

    let renewal = {
        let coordinator = coordinator.clone();
        let collecting = collecting.clone();
        let generation = generation.clone();
        let renewal_in_progress = renewal_in_progress.clone();
        let period = period.clone();
        let delivered = delivered.clone();
        let renewal_paused = renewal_paused.clone();
        let renewal_resume = renewal_resume.clone();
        thread::spawn(move || {
            renewal_in_progress.store(true, Ordering::Release);
            collecting.store(false, Ordering::Release);
            renewal_paused.wait();
            renewal_resume.wait();
            *period.write().unwrap() = Some("period_renewed".to_string());
            collecting.store(true, Ordering::Release);
            renewal_in_progress.store(false, Ordering::Release);
            coordinator.flush(
                &generation,
                &renewal_in_progress,
                12,
                |event, expected_generation| {
                    publish(
                        &collecting,
                        &generation,
                        &period,
                        &delivered,
                        event,
                        expected_generation,
                    )
                },
            );
        })
    };

    renewal_paused.wait();
    let terminal = DeferredToolInstallTerminal {
        operation_id: "install_during_renewal".to_string(),
        generation: 12,
        event: BehaviorEvent::new("tool_install_completed")
            .string("operation_id", "install_during_renewal")
            .success(true),
    };
    coordinator.submit(
        terminal,
        &generation,
        &renewal_in_progress,
        |event, expected_generation| {
            publish(
                &collecting,
                &generation,
                &period,
                &delivered,
                event,
                expected_generation,
            )
        },
    );
    assert_eq!(coordinator.pending_len(), 1);
    assert!(delivered.lock().unwrap().is_empty());

    renewal_resume.wait();
    renewal.join().unwrap();
    assert_eq!(coordinator.pending_len(), 0);
    let delivered_after_renewal = delivered.lock().unwrap().clone();
    assert_eq!(delivered_after_renewal.len(), 1);
    assert_eq!(delivered_after_renewal[0].1, "period_renewed");
    coordinator.flush(
        &generation,
        &renewal_in_progress,
        12,
        |event, expected_generation| {
            publish(
                &collecting,
                &generation,
                &period,
                &delivered,
                event,
                expected_generation,
            )
        },
    );
    assert_eq!(delivered.lock().unwrap().len(), 1);

    renewal_in_progress.store(true, Ordering::Release);
    collecting.store(false, Ordering::Release);
    coordinator.submit(
        DeferredToolInstallTerminal {
            operation_id: "install_before_delete".to_string(),
            generation: 12,
            event: BehaviorEvent::new("tool_install_completed")
                .string("operation_id", "install_before_delete")
                .success(true),
        },
        &generation,
        &renewal_in_progress,
        |event, expected_generation| {
            publish(
                &collecting,
                &generation,
                &period,
                &delivered,
                event,
                expected_generation,
            )
        },
    );
    assert_eq!(coordinator.pending_len(), 1);
    generation.store(13, Ordering::Release);
    renewal_in_progress.store(false, Ordering::Release);
    coordinator.discard_stale(13);
    *period.write().unwrap() = Some("period_after_delete".to_string());
    collecting.store(true, Ordering::Release);
    coordinator.flush(
        &generation,
        &renewal_in_progress,
        13,
        |event, expected_generation| {
            publish(
                &collecting,
                &generation,
                &period,
                &delivered,
                event,
                expected_generation,
            )
        },
    );
    assert_eq!(coordinator.pending_len(), 0);
    assert_eq!(delivered.lock().unwrap().len(), 1);
}

#[test]
fn tool_install_flush_cleanup_is_linearized_before_reinsert() {
    use std::sync::Barrier;
    use std::thread;

    #[derive(Clone, Copy, Debug)]
    enum Cleanup {
        FailClose,
        GenerationAdvance,
        RenewalDrop,
    }

    fn run_interleaving(cleanup: Cleanup) {
        let coordinator = ToolInstallTerminalCoordinator::default();
        let generation = Arc::new(AtomicU64::new(77));
        let renewal_in_progress = Arc::new(AtomicBool::new(true));
        coordinator.submit(
            DeferredToolInstallTerminal {
                operation_id: format!("linearized_{cleanup:?}"),
                generation: 77,
                event: BehaviorEvent::new("tool_install_completed")
                    .string("operation_id", format!("linearized_{cleanup:?}")),
            },
            &generation,
            &renewal_in_progress,
            |_, _| false,
        );
        assert_eq!(coordinator.pending_len(), 1);

        let before_reinsert = Arc::new(Barrier::new(2));
        let resume_reinsert = Arc::new(Barrier::new(2));
        let flush = {
            let coordinator = coordinator.clone();
            let generation = generation.clone();
            let renewal_in_progress = renewal_in_progress.clone();
            let before_reinsert = before_reinsert.clone();
            let resume_reinsert = resume_reinsert.clone();
            thread::spawn(move || {
                coordinator.flush_with_reinsert_hook(
                    &generation,
                    &renewal_in_progress,
                    77,
                    |_, _| false,
                    || {
                        before_reinsert.wait();
                        resume_reinsert.wait();
                    },
                );
            })
        };

        // The terminal has been removed for publication, publication failed,
        // and flush observed renewal=true before pausing outside pending's lock.
        before_reinsert.wait();
        match cleanup {
            Cleanup::FailClose => {
                renewal_in_progress.store(false, Ordering::Release);
                coordinator.clear_all();
            }
            Cleanup::GenerationAdvance => {
                generation.store(78, Ordering::Release);
                coordinator.discard_stale(78);
            }
            Cleanup::RenewalDrop => {
                let transition =
                    CredentialRenewalTransition::begin(&renewal_in_progress, &coordinator, 77);
                drop(transition);
            }
        }
        resume_reinsert.wait();
        flush.join().unwrap();

        assert_eq!(
            coordinator.pending_len(),
            0,
            "cleanup {cleanup:?} must prevent a terminal from being reinserted"
        );
        renewal_in_progress.store(false, Ordering::Release);
        let simulated_outbox = StdMutex::new(Vec::<String>::new());
        coordinator.flush(
            &generation,
            &renewal_in_progress,
            generation.load(Ordering::Acquire),
            |event, _| {
                simulated_outbox
                    .lock()
                    .unwrap()
                    .push(event.event_id.clone());
                true
            },
        );
        assert!(
            simulated_outbox.lock().unwrap().is_empty(),
            "cleanup {cleanup:?} must not allow delayed publication"
        );
    }

    for cleanup in [
        Cleanup::FailClose,
        Cleanup::GenerationAdvance,
        Cleanup::RenewalDrop,
    ] {
        run_interleaving(cleanup);
    }
}

#[test]
fn tool_install_persistence_failures_do_not_accumulate_and_recovery_converges() {
    let coordinator = ToolInstallTerminalCoordinator::default();
    let generation = AtomicU64::new(21);
    let renewal_in_progress = AtomicBool::new(false);
    let storage_writable = AtomicBool::new(false);
    let delivered = StdMutex::new(Vec::<String>::new());

    for index in 0..(OUTBOX_MAX_EVENTS + 1) {
        let operation_id = format!("failed_install_{index}");
        coordinator.submit(
            DeferredToolInstallTerminal {
                operation_id: operation_id.clone(),
                generation: 21,
                event: BehaviorEvent::new("tool_install_completed")
                    .string("operation_id", operation_id),
            },
            &generation,
            &renewal_in_progress,
            |event, _| {
                if !storage_writable.load(Ordering::Acquire) {
                    return false;
                }
                delivered.lock().unwrap().push(event.event_id.clone());
                true
            },
        );
    }
    assert_eq!(coordinator.pending_len(), 0);
    assert!(delivered.lock().unwrap().is_empty());

    storage_writable.store(true, Ordering::Release);
    coordinator.submit(
        DeferredToolInstallTerminal {
            operation_id: "recovered_install".to_string(),
            generation: 21,
            event: BehaviorEvent::new("tool_install_completed")
                .string("operation_id", "recovered_install"),
        },
        &generation,
        &renewal_in_progress,
        |event, _| {
            if !storage_writable.load(Ordering::Acquire) {
                return false;
            }
            delivered.lock().unwrap().push(event.event_id.clone());
            true
        },
    );
    assert_eq!(coordinator.pending_len(), 0);
    assert_eq!(delivered.lock().unwrap().len(), 1);

    let failed_transition =
        CredentialRenewalTransition::begin(&renewal_in_progress, &coordinator, 21);
    coordinator.submit(
        DeferredToolInstallTerminal {
            operation_id: "failed_renewal_install".to_string(),
            generation: 21,
            event: BehaviorEvent::new("tool_install_completed")
                .string("operation_id", "failed_renewal_install"),
        },
        &generation,
        &renewal_in_progress,
        |_, _| false,
    );
    assert_eq!(coordinator.pending_len(), 1);
    drop(failed_transition);
    assert!(!renewal_in_progress.load(Ordering::Acquire));
    assert_eq!(coordinator.pending_len(), 0);
}

#[test]
fn stale_producer_cannot_write_after_delete_and_reauthorization_aba() {
    use std::sync::Barrier;
    use std::thread;

    let collecting = Arc::new(AtomicBool::new(true));
    let generation = Arc::new(AtomicU64::new(11));
    let period = Arc::new(RwLock::new(Some("period_old".to_string())));
    let tasks = Arc::new(StdMutex::new(HashMap::<String, CollectionLease>::new()));
    let outbox = Arc::new(StdMutex::new(Vec::<CollectionLease>::new()));
    let old_lease = CollectionLease {
        generation: 11,
        collection_period_id: "period_old".to_string(),
    };
    let captured = Arc::new(Barrier::new(2));
    let resume = Arc::new(Barrier::new(2));

    let producer = {
        let collecting = collecting.clone();
        let generation = generation.clone();
        let period = period.clone();
        let tasks = tasks.clone();
        let outbox = outbox.clone();
        let captured = captured.clone();
        let resume = resume.clone();
        let old_lease = old_lease.clone();
        thread::spawn(move || {
            captured.wait();
            resume.wait();
            let mut tasks = tasks.lock().unwrap();
            if lease_matches_authorization(&collecting, &generation, &period, &old_lease) {
                tasks.insert("stale-session".to_string(), old_lease.clone());
            }
            drop(tasks);
            let mut outbox = outbox.lock().unwrap();
            if lease_matches_authorization(&collecting, &generation, &period, &old_lease) {
                outbox.push(old_lease);
            }
        })
    };

    captured.wait();
    generation.store(12, Ordering::Release);
    collecting.store(false, Ordering::Release);
    *period.write().unwrap() = None;
    tasks.lock().unwrap().clear();

    generation.store(13, Ordering::Release);
    *period.write().unwrap() = Some("period_new".to_string());
    collecting.store(true, Ordering::Release);
    resume.wait();
    producer.join().unwrap();

    assert!(tasks.lock().unwrap().is_empty());
    assert!(outbox.lock().unwrap().is_empty());
    assert!(!lease_matches_authorization(
        &collecting,
        &generation,
        &period,
        &old_lease,
    ));
    assert!(lease_matches_authorization(
        &collecting,
        &generation,
        &period,
        &CollectionLease {
            generation: 13,
            collection_period_id: "period_new".to_string(),
        },
    ));
}
#[test]
fn tool_names_are_normalized_at_the_event_boundary() {
    for (raw_name, expected_name) in [
        ("MCP__Private_CRM__Lookup", "mcp__private_crm__lookup"),
        ("https://private.example/tool", "mcp_custom"),
        (r"C:\private\tool", "mcp_custom"),
        ("tool --secret=value", "mcp_custom"),
    ] {
        let event = BehaviorEvent::new("tool_call_started").tool("mcp_custom", raw_name, "mcp");
        assert_eq!(event.fields.get("tool_name"), Some(&json!(expected_name)));
    }
}
