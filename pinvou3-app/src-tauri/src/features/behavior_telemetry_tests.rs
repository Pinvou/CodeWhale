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
        team_sessions: Arc::new(StdMutex::new(HashMap::new())),
        tool_calls: Arc::new(StdMutex::new(HashMap::new())),
        voice_submissions: Arc::new(StdMutex::new(HashMap::new())),
        artifacts: Arc::new(StdMutex::new(HashMap::new())),
        latest_turns: Arc::new(StdMutex::new(HashMap::new())),
        tool_install_terminals: ToolInstallTerminalCoordinator::default(),
        event_tx: None,
        control_tx: None,
    }
}

async fn mock_http(
    responses: Vec<(u16, &'static str)>,
) -> (String, tokio::task::JoinHandle<Vec<String>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
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
        }
        requests
    });
    (format!("http://{address}"), handle)
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
    outbox.rebind_period("period_renewed", 2).unwrap();
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
        let event = BehaviorEvent::new("tool_call_started")
            .tool("mcp_custom", raw_name, "mcp");
        assert_eq!(event.fields.get("tool_name"), Some(&json!(expected_name)));
    }
}
