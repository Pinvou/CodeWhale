use super::*;
use serde_json::json;

fn runtime() -> RuntimeValidationContext {
    let mut capabilities = CONTROLLER_CAPABILITIES
        .iter()
        .map(|x| (*x).to_owned())
        .collect::<Vec<_>>();
    capabilities.sort();
    RuntimeValidationContext {
        lease: Some(LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "7".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }),
        negotiated_capabilities: capabilities,
    }
}

fn business() -> Value {
    json!({"v":1,"type":"encrypted_frame","connection_id":"connection-0001","controller_lease_id":"lease-0001","lease_epoch":"7","relay_epoch":"relay-epoch-0001","remote_control_generation":"generation-0001","frame_sequence":"1","direction":"a_to_b","payload_kind":"task_submit","ciphertext_b64":"AQID","ciphertext_bytes":"3"})
}

#[test]
fn parsed_runtime_requires_strict_wire_context_role_and_negotiated_capability() {
    let context = runtime();
    let wire = business().to_string();
    let parsed = ParsedControllerMessage::parse(&wire).unwrap();
    assert_eq!(
        parsed
            .validate_runtime(
                SenderRole::ControllerA,
                context.lease.as_ref(),
                &context.negotiated_capabilities
            )
            .unwrap()
            .value(),
        &business()
    );
    assert_eq!(
        validate_controller_runtime_message_json(&wire, SenderRole::ControllerA, Some(&context))
            .unwrap(),
        business()
    );
    assert!(
        parsed
            .validate_runtime(
                SenderRole::ControlledB,
                context.lease.as_ref(),
                &context.negotiated_capabilities
            )
            .is_err()
    );
    assert!(
        parsed
            .validate_runtime(
                SenderRole::ControllerA,
                None,
                &context.negotiated_capabilities
            )
            .is_err()
    );
    let mut wrong = context.lease.clone().unwrap();
    wrong.lease_epoch = "8".into();
    assert!(
        parsed
            .validate_runtime(
                SenderRole::ControllerA,
                Some(&wrong),
                &context.negotiated_capabilities
            )
            .is_err()
    );
    let caps = context
        .negotiated_capabilities
        .iter()
        .filter(|x| x.as_str() != "task_v1")
        .cloned()
        .collect::<Vec<_>>();
    assert!(
        parsed
            .validate_runtime(SenderRole::ControllerA, context.lease.as_ref(), &caps)
            .is_err()
    );
    let duplicate = wire.replacen("\"v\":1", "\"v\":1,\"v\":1", 1);
    assert!(ParsedControllerMessage::parse(&duplicate).is_err());
    assert!(
        validate_controller_runtime_message_json(
            &duplicate,
            SenderRole::ControllerA,
            Some(&context)
        )
        .is_err()
    );
    let mut extra = business();
    extra["unknown"] = json!(true);
    assert!(
        ParsedControllerMessage::parse(&extra.to_string())
            .unwrap()
            .validate_runtime(
                SenderRole::ControllerA,
                context.lease.as_ref(),
                &context.negotiated_capabilities
            )
            .is_err()
    );
}

#[test]
fn parsed_control_preserves_original_wire_size_and_runtime_error_order() {
    let mut wire=json!({"v":1,"type":"error","request_id":"request-0001","code":"controller_busy","message":"busy"}).to_string();
    wire.extend(std::iter::repeat_n(' ', MAX_CONTROL_MESSAGE_BYTES));
    let parsed = ParsedControllerMessage::parse(&wire).unwrap();
    assert!(parsed.validate(SenderRole::Relay, None).is_err());
    let invalid_context = RuntimeValidationContext {
        lease: None,
        negotiated_capabilities: vec!["unsupported_v1".into()],
    };
    let error = validate_controller_runtime_message_json(
        "invalid JSON",
        SenderRole::ControllerA,
        Some(&invalid_context),
    )
    .unwrap_err();
    assert_eq!(error.field, "negotiated_capabilities");
    let oversized = " ".repeat(MAX_ENCRYPTED_WIRE_BYTES + 1);
    assert!(ParsedControllerMessage::parse(&oversized).is_err());
}
