//! Anonymous remote-control events in the existing behavior telemetry pipeline.
//! Callers can provide only fixed categories and bounded buckets; identifiers,
//! text, paths, credentials and raw errors have no slot in this event.

use super::{BehaviorEvent, Result};

macro_rules! controlled_enum {
    ($name:ident { $($variant:ident => $value:literal),+ $(,)? }) => {
        #[derive(Clone, Copy, Debug, Eq, PartialEq)]
        pub(crate) enum $name { $($variant),+ }

        impl $name {
            pub(crate) const fn as_str(self) -> &'static str {
                match self { $(Self::$variant => $value),+ }
            }

            fn accepts(value: &str) -> bool {
                matches!(value, $($value)|+)
            }
        }
    };
}

controlled_enum!(RemoteStage {
    Start => "start",
    Stop => "stop",
    Refresh => "refresh",
    Claim => "claim",
    Handshake => "handshake",
    Connection => "connection",
    Disconnection => "disconnection",
    Resume => "resume",
    TaskSubmit => "task_submit",
    TaskAdmit => "task_admit",
    TaskTerminal => "task_terminal",
    Permission => "permission",
    Attachment => "attachment",
    Artifact => "artifact",
});

controlled_enum!(RemoteOutcome {
    Ok => "ok",
    Rejected => "rejected",
    Timeout => "timeout",
    Busy => "busy",
    Expired => "expired",
    Unsupported => "unsupported",
    InternalError => "internal_error",
});

controlled_enum!(RemoteReason {
    None => "none",
    NotEnrolled => "not_enrolled",
    CodeInvalid => "code_invalid",
    SlotConflict => "slot_conflict",
    HandshakeFailed => "handshake_failed",
    ReceiptMismatch => "receipt_mismatch",
    LivenessMissing => "liveness_missing",
    WatchdogExpired => "watchdog_expired",
    ResumeFailed => "resume_failed",
    TaskConflict => "task_conflict",
    PermissionDenied => "permission_denied",
    PermissionTimeout => "permission_timeout",
    TransferBudget => "transfer_budget",
    ArtifactMissing => "artifact_missing",
    RateLimited => "rate_limited",
    Unknown => "unknown",
});

controlled_enum!(RemoteModelSource {
    Controlled => "controlled",
    ControllerTemporary => "controller_temporary",
});

controlled_enum!(RemotePermissionStep {
    Request => "request",
    Allowed => "allowed",
    Denied => "denied",
    Expired => "expired",
});

controlled_enum!(RemoteSizeBucket {
    ZeroToOneMiB => "0_1mib",
    OneToTenMiB => "1_10mib",
    TenToHundredMiB => "10_100mib",
    HundredMiBPlus => "100mib_plus",
});

fn latency_bucket(milliseconds: u64) -> &'static str {
    match milliseconds {
        0..=49 => "lt_50ms",
        50..=249 => "lt_250ms",
        250..=999 => "lt_1s",
        1_000..=4_999 => "lt_5s",
        5_000..=29_999 => "lt_30s",
        _ => "gte_30s",
    }
}

fn size_bucket(bytes: u64) -> RemoteSizeBucket {
    const MIB: u64 = 1024 * 1024;
    if bytes < MIB {
        RemoteSizeBucket::ZeroToOneMiB
    } else if bytes < 10 * MIB {
        RemoteSizeBucket::OneToTenMiB
    } else if bytes < 100 * MIB {
        RemoteSizeBucket::TenToHundredMiB
    } else {
        RemoteSizeBucket::HundredMiBPlus
    }
}

#[derive(Clone, Copy, Debug)]
pub(crate) struct RemoteControlEvent {
    stage: RemoteStage,
    outcome: RemoteOutcome,
    reason: RemoteReason,
    latency_ms: u64,
    model_source: Option<RemoteModelSource>,
    size_bucket: Option<RemoteSizeBucket>,
    permission_step: Option<RemotePermissionStep>,
}

impl RemoteControlEvent {
    pub(crate) fn new(stage: RemoteStage, outcome: RemoteOutcome) -> Self {
        Self {
            stage,
            outcome,
            reason: RemoteReason::None,
            latency_ms: 0,
            model_source: None,
            size_bucket: None,
            permission_step: None,
        }
    }

    pub(crate) fn reason(mut self, reason: RemoteReason) -> Self {
        self.reason = reason;
        self
    }

    pub(crate) fn latency_ms(mut self, latency_ms: u64) -> Self {
        self.latency_ms = latency_ms;
        self
    }

    pub(crate) fn model_source(mut self, source: RemoteModelSource) -> Self {
        self.model_source = Some(source);
        self
    }

    pub(crate) fn size_bytes(mut self, bytes: u64) -> Self {
        self.size_bucket = Some(size_bucket(bytes));
        self
    }

    pub(crate) fn permission_step(mut self, step: RemotePermissionStep) -> Self {
        self.permission_step = Some(step);
        self
    }

    /// Classifies a command result without copying its raw error into telemetry.
    /// Unknown errors collapse to one fixed category.
    pub(crate) fn from_result<T>(
        stage: RemoteStage,
        result: &std::result::Result<T, String>,
        latency_ms: u64,
    ) -> Self {
        let (outcome, reason) = match result {
            Ok(_) => (RemoteOutcome::Ok, RemoteReason::None),
            Err(error) => classify_error(error),
        };
        Self::new(stage, outcome)
            .reason(reason)
            .latency_ms(latency_ms)
    }

    fn into_behavior(self) -> BehaviorEvent {
        let mut event = BehaviorEvent::new("remote_control_event")
            .string("remote_stage", self.stage.as_str())
            .string("remote_outcome", self.outcome.as_str())
            .string("remote_reason", self.reason.as_str())
            .string("remote_latency_bucket", latency_bucket(self.latency_ms));
        if let Some(source) = self.model_source {
            event = event.string("remote_model_source", source.as_str());
        }
        if let Some(bucket) = self.size_bucket {
            event = event.string("remote_size_bucket", bucket.as_str());
        }
        if let Some(step) = self.permission_step {
            event = event.string("remote_permission_step", step.as_str());
        }
        event
    }
}

fn classify_error(error: &str) -> (RemoteOutcome, RemoteReason) {
    match error {
        "desktop_remote_control_unsupported" => (RemoteOutcome::Unsupported, RemoteReason::None),
        "desktop_remote_control_busy" | "controller_busy" => {
            (RemoteOutcome::Busy, RemoteReason::Unknown)
        }
        "task_busy" | "task_already_running" => (RemoteOutcome::Busy, RemoteReason::TaskConflict),
        "pairing_code_invalid" | "invalid_pairing_code" | "code_invalid" => {
            (RemoteOutcome::Rejected, RemoteReason::CodeInvalid)
        }
        "controller_slot_busy" | "slot_conflict" => {
            (RemoteOutcome::Busy, RemoteReason::SlotConflict)
        }
        "not_enrolled" | "endpoint_not_enrolled" => {
            (RemoteOutcome::Rejected, RemoteReason::NotEnrolled)
        }
        "handshake_failed" => (RemoteOutcome::Rejected, RemoteReason::HandshakeFailed),
        "receipt_mismatch" => (RemoteOutcome::Rejected, RemoteReason::ReceiptMismatch),
        "resume_failed" => (RemoteOutcome::Rejected, RemoteReason::ResumeFailed),
        "permission_denied" => (RemoteOutcome::Rejected, RemoteReason::PermissionDenied),
        "permission_expired" => (RemoteOutcome::Expired, RemoteReason::PermissionTimeout),
        "transfer_budget_exceeded" => (RemoteOutcome::Rejected, RemoteReason::TransferBudget),
        "artifact_not_found" => (RemoteOutcome::Rejected, RemoteReason::ArtifactMissing),
        "rate_limited" => (RemoteOutcome::Rejected, RemoteReason::RateLimited),
        "liveness_timeout" | "watchdog_expired" => {
            (RemoteOutcome::Timeout, RemoteReason::WatchdogExpired)
        }
        _ => (RemoteOutcome::InternalError, RemoteReason::Unknown),
    }
}

pub(crate) fn track_remote_control(app: &tauri::AppHandle, event: RemoteControlEvent) {
    super::events::track(app, event.into_behavior());
}

/// Track a lifecycle transition with a caller-derived anonymous, stable ID.
/// This is reserved for authoritative remote-control state machines whose
/// replay window is bounded: retransmission after local eviction reaches the
/// existing behavior outbox/server dedupe with the same identity.
pub(crate) fn track_remote_control_with_event_id(
    app: &tauri::AppHandle,
    event: RemoteControlEvent,
    event_id: String,
) {
    let mut behavior = event.into_behavior();
    behavior.event_id = event_id;
    super::events::track(app, behavior);
}

pub(super) fn validate_event_fields(event: &BehaviorEvent) -> Result<()> {
    const REQUIRED: [&str; 4] = [
        "remote_stage",
        "remote_outcome",
        "remote_reason",
        "remote_latency_bucket",
    ];
    for field in event.fields.keys() {
        if !REQUIRED.contains(&field.as_str())
            && !matches!(
                field.as_str(),
                "remote_model_source" | "remote_size_bucket" | "remote_permission_step"
            )
        {
            anyhow::bail!("remote-control event field is not allowlisted");
        }
    }
    for field in REQUIRED {
        if !event
            .fields
            .get(field)
            .is_some_and(|value| value.is_string())
        {
            anyhow::bail!("remote-control event field is missing or invalid");
        }
    }
    let field = |name| event.fields.get(name).and_then(serde_json::Value::as_str);
    if !field("remote_stage").is_some_and(RemoteStage::accepts)
        || !field("remote_outcome").is_some_and(RemoteOutcome::accepts)
        || !field("remote_reason").is_some_and(RemoteReason::accepts)
        || !field("remote_latency_bucket").is_some_and(|value| {
            matches!(
                value,
                "lt_50ms" | "lt_250ms" | "lt_1s" | "lt_5s" | "lt_30s" | "gte_30s"
            )
        })
        || event.fields.contains_key("remote_model_source")
            && !field("remote_model_source").is_some_and(RemoteModelSource::accepts)
        || event.fields.contains_key("remote_size_bucket")
            && !field("remote_size_bucket").is_some_and(RemoteSizeBucket::accepts)
        || event.fields.contains_key("remote_permission_step")
            && !field("remote_permission_step").is_some_and(RemotePermissionStep::accepts)
    {
        anyhow::bail!("remote-control event enum is not allowlisted");
    }
    if field("remote_stage") == Some("permission")
        && !event.fields.contains_key("remote_permission_step")
        || field("remote_stage") != Some("permission")
            && event.fields.contains_key("remote_permission_step")
    {
        anyhow::bail!("permission step is required only for permission events");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn controlled_record_accepts_only_fixed_categories_and_buckets() {
        let event = RemoteControlEvent::new(RemoteStage::TaskAdmit, RemoteOutcome::Ok)
            .model_source(RemoteModelSource::ControllerTemporary)
            .latency_ms(1_000)
            .size_bytes(10 * 1024 * 1024)
            .into_behavior();
        validate_event_fields(&event).unwrap();
        let value = serde_json::to_value(&event).unwrap();
        assert_eq!(value["remote_latency_bucket"], "lt_5s");
        assert_eq!(value["remote_size_bucket"], "10_100mib");
        assert!(value.get("session_id").is_none());
        assert!(value.get("device_id").is_none());
    }

    #[test]
    fn arbitrary_fields_values_and_cross_event_reuse_fail_closed() {
        let ordinary = BehaviorEvent::new("task_submitted").string("remote_stage", "task_submit");
        assert!(super::super::validate_client_event_fields(&ordinary).is_err());
        let baseline = RemoteControlEvent::new(RemoteStage::Claim, RemoteOutcome::Rejected)
            .reason(RemoteReason::CodeInvalid)
            .into_behavior();
        for (field, value) in [
            ("endpoint", json!("vault://secret")),
            ("session_id", json!("private-session")),
            ("prompt", json!("private-content")),
            ("remote_stage", json!("private-stage")),
            ("remote_outcome", json!("maybe")),
            ("remote_reason", json!("private-reason")),
            ("remote_latency_bucket", json!("999ms")),
            ("remote_model_source", json!("my-model-id")),
            ("remote_size_bucket", json!("1234567")),
        ] {
            let mut event = baseline.clone();
            event.fields.insert(field.into(), value);
            assert!(
                super::super::validate_client_event_fields(&event).is_err(),
                "{field} accepted"
            );
        }
        let mut missing = baseline;
        missing.fields.remove("remote_reason");
        assert!(super::super::validate_client_event_fields(&missing).is_err());
    }

    #[test]
    fn serialized_record_has_no_sensitive_application_values() {
        let event = RemoteControlEvent::new(RemoteStage::Attachment, RemoteOutcome::Ok)
            .size_bytes(u64::MAX)
            .into_behavior();
        let serialized = serde_json::to_string(&event).unwrap();
        for sensitive in [
            "vault://",
            "secret",
            "Authorization",
            "C:\\private",
            "sk-live-",
        ] {
            assert!(!serialized.contains(sensitive));
        }
        assert_eq!(size_bucket(1024 * 1024), RemoteSizeBucket::OneToTenMiB);
    }

    #[test]
    fn permission_step_is_required_and_expiry_has_a_fixed_reason() {
        let expired = RemoteControlEvent::new(RemoteStage::Permission, RemoteOutcome::Expired)
            .reason(RemoteReason::PermissionTimeout)
            .permission_step(RemotePermissionStep::Expired)
            .into_behavior();
        super::super::validate_client_event_fields(&expired).unwrap();
        let value = serde_json::to_value(&expired).unwrap();
        assert_eq!(value["remote_permission_step"], "expired");
        assert_eq!(value["remote_reason"], "permission_timeout");

        let missing =
            RemoteControlEvent::new(RemoteStage::Permission, RemoteOutcome::Ok).into_behavior();
        assert!(super::super::validate_client_event_fields(&missing).is_err());
        let wrong_stage = RemoteControlEvent::new(RemoteStage::TaskSubmit, RemoteOutcome::Ok)
            .permission_step(RemotePermissionStep::Request)
            .into_behavior();
        assert!(super::super::validate_client_event_fields(&wrong_stage).is_err());
        assert_eq!(
            classify_error("permission_expired"),
            (RemoteOutcome::Expired, RemoteReason::PermissionTimeout)
        );
    }

    #[test]
    fn raw_error_never_enters_a_classified_event() {
        let result: std::result::Result<(), String> =
            Err("vault://private/device?token=sk-live-123456789".into());
        let event =
            RemoteControlEvent::from_result(RemoteStage::Connection, &result, 300).into_behavior();
        let serialized = serde_json::to_string(&event).unwrap();
        assert_eq!(
            serde_json::to_value(&event).unwrap()["remote_reason"],
            "unknown"
        );
        assert!(!serialized.contains("vault://"));
        assert!(!serialized.contains("sk-live-"));
        assert!(!serialized.contains("token"));
    }
}
