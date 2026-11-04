use super::*;

pub(super) fn guard(telemetry: &BehaviorTelemetry, tool_name: &str) -> ToolInstallGuard {
    ToolInstallGuard {
        telemetry: Some(telemetry.clone()),
        operation_id: token("install_", 16),
        started_at: now_ms(),
        tool_key: tool_registry_key(tool_name),
        tool_name: tool_name.into(),
        tool_type: "cli",
        authorization_generation: telemetry.current_lease().map(|lease| lease.generation),
        completed: false,
    }
}

#[test]
fn tool_install_failure_preserves_result_and_only_persists_classified_evidence() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    for (index, (tool, error, code)) in [
        (
            "feishu",
            "connection timeout PRIVATE_INSTALL_DETAILS",
            "request_timeout",
        ),
        (
            "wecom",
            "permission denied PRIVATE_INSTALL_DETAILS",
            "permission_denied",
        ),
        (
            "dingtalk",
            "DNS lookup failed PRIVATE_INSTALL_DETAILS",
            "dns_failed",
        ),
        ("tmeet", "HTTP 429 PRIVATE_INSTALL_DETAILS", "rate_limited"),
        (
            "custom",
            "certificate invalid PRIVATE_INSTALL_DETAILS",
            "tls_failed",
        ),
    ]
    .into_iter()
    .enumerate()
    {
        let install = guard(&telemetry, tool);
        let operation_id = install.operation_id.clone();
        let returned = install.finish(Err::<Value, _>(error.to_string()));
        assert_eq!(returned, Err(error.to_string()));
        let outbox = telemetry.outbox.lock().unwrap();
        // Consuming finish must not also emit the Drop fallback.
        assert_eq!(outbox.records.len(), index + 1);
        let event = &outbox.records[index].event;
        assert_eq!(event["event_name"], "tool_install_completed");
        assert_eq!(event["operation_id"], operation_id);
        assert_eq!(event["success"], false);
        assert_eq!(event["error_category"], error_category(Some(error)));
        assert_eq!(event["diagnostic_code"], code);
        assert_eq!(event["diagnostic_site"], "lifecycle");
        if tool == "tmeet" {
            assert_eq!(event["diagnostic_http_status"], 429);
        }
        assert!(!event.to_string().contains("PRIVATE_INSTALL_DETAILS"));
    }
    let persisted =
        std::fs::read_to_string(paths::pinvou3_home().join("failure-diagnostics.json")).unwrap();
    assert!(!persisted.contains("PRIVATE_INSTALL_DETAILS"));
}

#[test]
fn tool_install_success_preserves_value_and_emits_one_terminal() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    for already in [false, true] {
        let result = json!({"ok":true, "already":already});
        assert_eq!(
            guard(&telemetry, "feishu").finish(Ok(result.clone())),
            Ok(result)
        );
    }
    let outbox = telemetry.outbox.lock().unwrap();
    assert_eq!(outbox.records.len(), 2);
    for record in &outbox.records {
        assert_eq!(record.event["success"], true);
        assert!(record.event.get("error_category").is_none());
        assert!(record.event.get("diagnostic_id").is_none());
    }
}

#[test]
fn tool_install_abandoned_guard_keeps_internal_error_fallback() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    drop(guard(&telemetry, "feishu"));
    let outbox = telemetry.outbox.lock().unwrap();
    assert_eq!(outbox.records.len(), 1);
    assert_eq!(outbox.records[0].event["success"], false);
    assert_eq!(outbox.records[0].event["diagnostic_code"], "internal_error");
}

#[test]
fn tool_install_unclassified_failure_does_not_invent_a_root_cause() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let error = "opaque failure PRIVATE_INSTALL_DETAILS";
    assert_eq!(
        guard(&telemetry, "feishu").finish(Err::<(), _>(error.into())),
        Err(error.into())
    );
    let outbox = telemetry.outbox.lock().unwrap();
    assert_eq!(outbox.records.len(), 1);
    assert_eq!(outbox.records[0].event["diagnostic_code"], "unknown");
    assert!(
        !outbox.records[0]
            .event
            .to_string()
            .contains("PRIVATE_INSTALL_DETAILS")
    );
}

#[test]
fn tool_install_without_initial_collection_cannot_attach_to_a_later_authorization() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    telemetry.collecting.store(false, Ordering::Release);
    let install = guard(&telemetry, "feishu");
    assert!(install.authorization_generation.is_none());
    telemetry.collecting.store(true, Ordering::Release);
    assert!(
        install
            .finish(Err::<(), _>("connection timeout".into()))
            .is_err()
    );
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 0);
}

#[test]
fn tool_install_failure_survives_renewal_but_cannot_cross_authorization_generations() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let install = guard(&telemetry, "feishu");
    let generation = install.authorization_generation.unwrap();
    telemetry
        .credential_renewal_in_progress
        .store(true, Ordering::Release);
    telemetry.collecting.store(false, Ordering::Release);
    assert!(
        install
            .finish(Err::<(), _>("connection timeout".into()))
            .is_err()
    );
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 1);
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    *telemetry.collection_period_id.write().unwrap() = Some("renewed_install_period".into());
    telemetry.collecting.store(true, Ordering::Release);
    telemetry
        .credential_renewal_in_progress
        .store(false, Ordering::Release);
    telemetry.flush_tool_install_terminals(generation);
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 0);
    {
        let outbox = telemetry.outbox.lock().unwrap();
        assert_eq!(outbox.records.len(), 1);
        assert_eq!(
            outbox.records[0].event["diagnostic_code"],
            "request_timeout"
        );
        assert_eq!(
            outbox.records[0].event["collection_period_id"],
            "renewed_install_period"
        );
    }
    let stale = guard(&telemetry, "feishu");
    telemetry.invalidate_generation_above(generation).unwrap();
    assert!(
        stale
            .finish(Err::<(), _>("permission denied".into()))
            .is_err()
    );
    assert_eq!(telemetry.outbox.lock().unwrap().records.len(), 1);
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 0);
}
