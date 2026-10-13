use super::*;

#[test]
fn model_output_truncated_classifies_as_invalid_input_not_tool() {
    // The turn-level "Model output truncated" error is a provider/model
    // condition, not a tool failure: it must land in the same bucket as
    // `LlmError::ModelError` so the exec termination classifier reduces it
    // to `RunTerminationReason::ModelError` (never Resolved).
    assert_eq!(
        classify_error_message(
            "Model output truncated: provider stop reason `max_output_tokens`; no complete response or tool call was accepted."
        ),
        ErrorCategory::InvalidInput
    );
    assert_eq!(
        classify_error_message(
            "Model output truncated: provider stop reason `max_tokens`; no complete response or tool call was accepted."
        ),
        ErrorCategory::InvalidInput
    );
    assert_eq!(
        classify_error_message(
            "Model response incomplete: provider stop reason `content_filter`; no complete response or tool call was accepted."
        ),
        ErrorCategory::InvalidInput
    );
}

#[test]
fn raw_rate_and_quota_phrases_remain_coarse_rate_limit_diagnostics() {
    for message in [
        "Rate limit reached for gpt-4",
        "Too Many Requests",
        "HTTP 429 from upstream",
        "Your quota has been exceeded",
        "Authorization failed: You've reached your usage limit for this billing cycle",
    ] {
        assert_eq!(classify_error_message(message), ErrorCategory::RateLimit);
    }
}

#[test]
fn typed_llm_quota_envelope_is_non_recoverable_and_distinct_from_rate_limit() {
    let envelope = ErrorEnvelope::from(LlmError::from_http_response(
        429,
        r#"{"error":{"code":"insufficient_quota"}}"#,
    ));
    assert_eq!(envelope.category, ErrorCategory::RateLimit);
    assert_eq!(envelope.severity, ErrorSeverity::Error);
    assert!(!envelope.recoverable);
    assert_eq!(envelope.code, "llm_quota_exhausted");
}

#[test]
fn llm_auth_error_envelope_classifies_401_and_carries_body_verbatim() {
    // The envelope is a classifier, not a redactor: a 401 must map to a
    // critical, non-recoverable authentication error, and the body must be
    // carried verbatim — redacting secrets from provider bodies is the
    // caller's job before the envelope is built (see the
    // `redact_model_bound_text` scenario pins in client.rs).
    let envelope = ErrorEnvelope::from(LlmError::from_http_response(
        401,
        "Invalid API Key: tp-secret-token-plan-value",
    ));
    assert_eq!(envelope.category, ErrorCategory::Authentication);
    assert_eq!(envelope.severity, ErrorSeverity::Critical);
    assert!(!envelope.recoverable);
    assert!(envelope.message.contains("Invalid API Key"));
    assert!(envelope.message.contains("tp-secret-token-plan-value"));
}

#[test]
fn model_not_exist_rejection_is_a_terminal_invalid_input_error() {
    // The reported incident: a provider switch left GLM-5.3 selected on a
    // Model Studio route; the next message failed with
    // `Model error: Model not exist.` and the transcript rendered it as a
    // dismissable *warning* because the stringified error fell through to
    // `Internal` + `recoverable`. A wrong-model rejection is terminal for
    // the request: it must classify as InvalidInput so it renders at Error
    // severity and never as recoverable noise.
    for message in [
        "Model error: Model not exist.",
        "Model not exist",
        "Model not found: glm-5.3 on this endpoint",
        "Unknown model identifier",
        "Invalid model: qwen-flash",
    ] {
        let envelope = ErrorEnvelope::classify(message.to_string(), true);
        assert_eq!(
            envelope.category,
            ErrorCategory::InvalidInput,
            "message must classify as InvalidInput: {message}"
        );
        assert_eq!(
            envelope.severity,
            ErrorSeverity::Error,
            "wrong-model rejections must render at Error severity: {message}"
        );
        // `recoverable` governs offline-mode semantics, not transcript
        // severity: a wrong-model rejection keeps the session online so the
        // operator can repair the route.
        assert!(envelope.recoverable, "session stays online: {message}");
    }
}

#[test]
fn typed_llm_error_preserves_terminal_severity_across_boundary() {
    // Even where the typed error survives to the boundary (the turn loop's
    // stream-initiation and mid-stream paths), `envelope_for_llm_error`
    // must keep the typed contract instead of re-classifying the string
    // with `recoverable = true`.
    let typed: anyhow::Error =
        crate::llm_client::LlmError::ModelError("Model not exist.".to_string()).into();
    let envelope = envelope_for_llm_error(typed, "Model error: Model not exist.".to_string());
    assert_eq!(envelope.category, ErrorCategory::InvalidInput);
    assert_eq!(envelope.severity, ErrorSeverity::Error);
    assert_eq!(envelope.code, "llm_model_error");
    assert_eq!(envelope.message, "Model error: Model not exist.");

    // Untyped errors keep the legacy string fallback.
    let untyped: anyhow::Error = anyhow::anyhow!("stream read error: connection reset");
    let envelope = envelope_for_llm_error(untyped, "stream read error: connection reset".into());
    assert_eq!(envelope.category, ErrorCategory::Network);
    assert!(envelope.recoverable);
}
