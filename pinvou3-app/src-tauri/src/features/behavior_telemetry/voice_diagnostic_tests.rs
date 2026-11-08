use super::*;

fn voice_event(name: &'static str, session: &str, operation: &str, at: u64) -> BehaviorEvent {
    let mut event = BehaviorEvent::new(name)
        .session(session)
        .string("operation_id", operation)
        .input_type("voice");
    event.occurred_at = at;
    event
}

fn preflight_failure(session: &str, operation: &str, at: u64) -> BehaviorEvent {
    voice_event("voice_recognition_failed", session, operation, at)
        .integer("duration_ms", 42)
        .string("error_category", "configuration_missing")
        .string("step", "asr_missing_model")
}

#[test]
fn voice_preflight_does_not_inherit_or_replace_chat_diagnostics() {
    let _home = TestHome::new();
    let mut runtime = test_runtime("http://localhost:1");
    // Produce a real wire envelope accepted by the receiver, without rewriting
    // fields after capture. All identities and faults in this test are synthetic.
    runtime.platform = "windows".into();
    runtime.arch = "x86_64".into();
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let at = now_ms();
    let mut submitted = BehaviorEvent::new("task_submitted")
        .session("voice_shared_chat")
        .string("attempt_id", "attempt_voice_chat")
        .string("mode", "assistant")
        .string("initiator_type", "user")
        .input_type("text")
        .boolean("has_attachment", false);
    submitted.occurred_at = at - 300_000;
    telemetry.track(submitted);
    let mut admitted = BehaviorEvent::new("task_admitted")
        .session("voice_shared_chat")
        .turn("turn_voice_chat")
        .string("attempt_id", "attempt_voice_chat")
        .string("mode", "assistant")
        .string("initiator_type", "user")
        .input_type("text")
        .boolean("has_attachment", false)
        .integer("queue_duration_ms", 1);
    admitted.occurred_at = at - 299_999;
    telemetry.track(admitted);
    let tool_failure = BehaviorEvent::new("tool_call_completed")
        .session("voice_shared_chat")
        .turn("turn_voice_chat")
        .string("attempt_id", "attempt_voice_chat")
        .string("operation_id", "toolop_voice_chat")
        .tool("custom", "custom", "custom")
        .model("deepseek", "deepseek")
        .success(false)
        .integer("duration_ms", 100)
        .string("error_category", "permission_denied");
    telemetry.track(tool_failure);
    telemetry.track(
        voice_event(
            "voice_entry_clicked",
            "voice_shared_chat",
            "voiceop_preflight",
            at - 42,
        )
        .string("entry_point", "composer"),
    );
    // Legacy clients omitted stage; configuration_missing has a unique known
    // producer at ASR dependency preflight and must not become recognition.
    telemetry.track(preflight_failure(
        "voice_shared_chat",
        "voiceop_preflight",
        at,
    ));
    telemetry.track(diagnostic_failure("voice_shared_chat", "turn_voice_chat"));

    let outbox = telemetry.outbox.lock().unwrap();
    let event = |name| {
        &outbox
            .records
            .iter()
            .find(|r| r.event["event_name"] == name)
            .unwrap()
            .event
    };
    let voice = event("voice_recognition_failed");
    let task = event("task_finished");
    let tool = event("tool_call_completed");
    assert_ne!(voice["diagnostic_trace_id"], tool["diagnostic_trace_id"]);
    assert_eq!(task["diagnostic_trace_id"], tool["diagnostic_trace_id"]);
    assert_eq!(
        voice["diagnostic_steps"],
        json!([{ "stage": "submission", "offset_ms": 0 }])
    );
    assert_eq!(voice["stage"], "submission");
    assert_eq!(voice["step"], "asr_missing_model");
    assert_eq!(voice["diagnostic_code"], "configuration_missing");
    assert_eq!(voice["diagnostic_component"], "voice");
    assert_eq!(voice["diagnostic_site"], "lifecycle");
    assert_eq!(
        voice["diagnostic_environment"],
        json!({ "routing": "local", "input_type": "voice" })
    );
    assert_eq!(task["diagnostic_environment"]["provider_key"], "deepseek");
    assert_eq!(task["diagnostic_environment"]["input_type"], "text");
    assert!(voice.get("diagnostic_http_status").is_none());
    let persisted: Value = serde_json::from_slice(
        &std::fs::read(paths::pinvou3_home().join("failure-diagnostics.json")).unwrap(),
    )
    .unwrap();
    let stored = persisted["incidents"]
        .as_array()
        .unwrap()
        .iter()
        .find(|i| i["event"]["diagnostic_id"] == voice["diagnostic_id"])
        .unwrap();
    assert_eq!(
        stored["event"]["diagnostic_environment"],
        voice["diagnostic_environment"]
    );
    assert_eq!(
        stored["event"]["diagnostic_steps"],
        voice["diagnostic_steps"]
    );
    println!("VOICE_DIAGNOSTIC_FIXTURE={}", voice);
}

#[test]
fn voice_operations_remain_distinct_during_chat_reset_and_next_attempt() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let at = now_ms();
    telemetry.track(voice_event(
        "voice_entry_clicked",
        "same_voice_chat",
        "voiceop_first",
        at - 500,
    ));
    telemetry.track(voice_event(
        "voice_started",
        "same_voice_chat",
        "voiceop_first",
        at - 400,
    ));
    telemetry.track(voice_event(
        "voice_entry_clicked",
        "same_voice_chat",
        "voiceop_second",
        at - 42,
    ));
    telemetry.track(
        BehaviorEvent::new("task_submitted")
            .session("same_voice_chat")
            .input_type("text"),
    );
    telemetry.track(preflight_failure("same_voice_chat", "voiceop_second", at));
    telemetry.track(
        voice_event(
            "voice_recognition_failed",
            "same_voice_chat",
            "voiceop_first",
            at + 1,
        )
        .string("error_category", "tool_error")
        .integer("duration_ms", 501),
    );
    telemetry.track(voice_event(
        "voice_entry_clicked",
        "same_voice_chat",
        "voiceop_next",
        at + 2,
    ));
    telemetry.track(preflight_failure(
        "same_voice_chat",
        "voiceop_next",
        at + 44,
    ));
    let outbox = telemetry.outbox.lock().unwrap();
    let failures: Vec<_> = outbox
        .records
        .iter()
        .filter(|r| r.event["event_name"] == "voice_recognition_failed")
        .map(|r| &r.event)
        .collect();
    assert_eq!(failures.len(), 3);
    let traces: std::collections::HashSet<_> = failures
        .iter()
        .map(|e| e["diagnostic_trace_id"].as_str().unwrap())
        .collect();
    assert_eq!(traces.len(), 3);
    assert_eq!(
        failures[0]["diagnostic_steps"],
        json!([{ "stage": "submission", "offset_ms": 0 }])
    );
    assert_eq!(
        failures[1]["diagnostic_steps"],
        json!([
            { "stage": "submission", "offset_ms": 0 },
            { "stage": "recording", "offset_ms": 100 },
            { "stage": "recognition", "offset_ms": 501 }
        ])
    );
    assert_eq!(
        failures[2]["diagnostic_steps"],
        json!([{ "stage": "submission", "offset_ms": 0 }])
    );
    assert_eq!(failures[1]["diagnostic_code"], "tool_failed");
}

#[test]
fn malformed_voice_without_operation_never_reuses_a_chat_context() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    telemetry.track(diagnostic_failure("legacy_voice_chat", "legacy_voice_turn"));
    for _ in 0..2 {
        // Missing operation_id is not a valid server event. The local observer
        // must still isolate it, rather than poisoning a legitimate task trace.
        telemetry.track(
            BehaviorEvent::new("voice_recognition_failed")
                .session("legacy_voice_chat")
                .string("error_category", "configuration_missing"),
        );
    }
    let outbox = telemetry.outbox.lock().unwrap();
    let failures: Vec<_> = outbox
        .records
        .iter()
        .filter(|r| r.event["event_name"] == "voice_recognition_failed")
        .map(|r| &r.event)
        .collect();
    assert_eq!(failures.len(), 2);
    assert_ne!(
        failures[0]["diagnostic_trace_id"],
        failures[1]["diagnostic_trace_id"]
    );
    for event in failures {
        assert_eq!(event["diagnostic_code"], "configuration_missing");
        assert_eq!(
            event["diagnostic_environment"],
            json!({ "routing": "local" })
        );
        assert_eq!(
            event["diagnostic_steps"],
            json!([{ "stage": "submission", "offset_ms": 0 }])
        );
    }
}

#[test]
fn reserved_voice_key_does_not_reuse_live_or_legacy_nonvoice_contexts() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let at = now_ms();
    let provider_failure = |session| {
        BehaviorEvent::new("provider_setup_failed")
            .session(session)
            .string("provider_key", "deepseek")
            .string("error_category", "configuration_missing")
    };

    // An older snapshot can contain a nonvoice context under a reserved raw ID.
    telemetry.track(provider_failure("ordinary_provider_session"));
    let path = paths::pinvou3_home().join("failure-diagnostics.json");
    let mut persisted: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    let contexts = persisted["contexts"].as_object_mut().unwrap();
    let ordinary = contexts.remove("ordinary_provider_session").unwrap();
    contexts.insert("voice|op|voiceop_legacy".into(), ordinary);
    std::fs::write(&path, serde_json::to_vec(&persisted).unwrap()).unwrap();
    *telemetry.diagnostics.lock().unwrap() = diagnostics::load();

    // Even a legacy context without a voice marker must stay outside the feedback lane.
    let mut feedback = BehaviorEvent::new("response_feedback_changed")
        .session("voice|op|voiceop_legacy")
        .string("feedback_action", "set")
        .string("rating", "negative");
    diagnostics::enrich(
        &telemetry,
        &mut feedback,
        &telemetry.current_lease().unwrap(),
    );
    assert!(
        feedback.fields["diagnostic_environment"]
            .get("provider_key")
            .is_none()
    );

    telemetry.track(preflight_failure("voice_chat", "voiceop_legacy", at));

    telemetry.track(voice_event(
        "voice_entry_clicked",
        "voice_chat",
        "voiceop_live",
        at,
    ));
    // The desktop telemetry command accepts this raw ID before wire normalization.
    telemetry.track(provider_failure("voice|op|voiceop_live"));
    telemetry.track(preflight_failure("voice_chat", "voiceop_live", at + 1));

    let outbox = telemetry.outbox.lock().unwrap();
    let failures: Vec<_> = outbox
        .records
        .iter()
        .filter(|record| record.event["event_name"] == "voice_recognition_failed")
        .collect();
    assert_eq!(failures.len(), 2);
    for failure in failures {
        assert_eq!(
            failure.event["diagnostic_environment"],
            json!({ "routing": "local", "input_type": "voice" })
        );
    }
}

#[test]
fn fallback_event_id_cannot_seed_an_explicit_voice_operation() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let malformed = BehaviorEvent::new("voice_recognition_failed")
        .session("voice_chat")
        .string("provider_key", "deepseek")
        .string("error_category", "configuration_missing");
    let operation = malformed.event_id.clone();
    telemetry.track(malformed);
    telemetry.track(preflight_failure("voice_chat", &operation, now_ms()));

    let outbox = telemetry.outbox.lock().unwrap();
    let failures: Vec<_> = outbox
        .records
        .iter()
        .filter(|record| record.event["event_name"] == "voice_recognition_failed")
        .collect();
    assert_eq!(failures.len(), 2);
    assert_eq!(
        failures[0].event["diagnostic_environment"]["provider_key"],
        "deepseek"
    );
    assert_ne!(
        failures[0].event["diagnostic_trace_id"],
        failures[1].event["diagnostic_trace_id"]
    );
    assert_eq!(
        failures[1].event["diagnostic_environment"],
        json!({ "routing": "local", "input_type": "voice" })
    );
}
