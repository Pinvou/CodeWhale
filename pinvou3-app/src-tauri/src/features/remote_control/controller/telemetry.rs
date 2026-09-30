//! Client-side remote-control telemetry and leak gates (DEV-24).
//!
//! Only the enumerated stages/outcomes/reasons below may leave the client, and
//! every record is aggregated before it reaches the existing behaviour
//! telemetry pipeline. The module also exposes the sentinel scanner used by the
//! security tests that prove no code, key, token or host path reaches a log,
//! a persisted file or a crash report.

use std::collections::HashMap;

use parking_lot::Mutex;
use serde_json::{Value, json};

pub(crate) const STAGES: &[&str] = &[
    "register",
    "claim",
    "handshake",
    "lease",
    "liveness",
    "resume",
    "task",
    "permission",
    "attachment",
    "artifact",
];
pub(crate) const OUTCOMES: &[&str] = &[
    "ok",
    "rejected",
    "timeout",
    "busy",
    "expired",
    "unsupported",
    "internal_error",
];
pub(crate) const REASONS: &[&str] = &[
    "none",
    "not_enrolled",
    "code_invalid",
    "slot_conflict",
    "handshake_failed",
    "receipt_mismatch",
    "liveness_missing",
    "watchdog_expired",
    "resume_failed",
    "task_conflict",
    "permission_denied",
    "transfer_budget",
    "artifact_missing",
    "rate_limited",
    "unknown",
];
pub(crate) const LATENCY_BUCKETS: &[&str] =
    &["lt_50ms", "lt_250ms", "lt_1s", "lt_5s", "lt_30s", "gte_30s"];
pub(crate) const MAX_COUNT: u64 = 100_000;

/// Fields that must never appear in a telemetry record. Matched as substrings
/// on the lower-cased key so `endpoint_id`, `resume_token` and `api_key` all
/// fail closed.
const FORBIDDEN_KEYS: &[&str] = &[
    "code",
    "endpoint",
    "lease",
    "token",
    "key",
    "secret",
    "credential",
    "ip",
    "device",
    "prompt",
    "path",
    "file",
    "url",
    "content",
    "addr",
    "host",
];

pub(crate) const SENSITIVE_SENTINELS: &[&str] = &[
    "sk-live-",
    "Bearer ",
    "Authorization:",
    "-----BEGIN",
    "vault://",
    "password=",
];

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct TelemetryError {
    pub(crate) code: &'static str,
}

impl TelemetryError {
    fn new(code: &'static str) -> Self {
        Self { code }
    }
}

type Result<T> = std::result::Result<T, TelemetryError>;

fn error(code: &'static str) -> TelemetryError {
    TelemetryError::new(code)
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct TelemetryRecord {
    pub(crate) stage: &'static str,
    pub(crate) outcome: &'static str,
    pub(crate) reason: &'static str,
    pub(crate) latency_bucket: &'static str,
    pub(crate) count: u64,
}

pub(crate) fn bucket_latency(milliseconds: u64) -> &'static str {
    match milliseconds {
        0..=49 => "lt_50ms",
        50..=249 => "lt_250ms",
        250..=999 => "lt_1s",
        1000..=4999 => "lt_5s",
        5000..=29999 => "lt_30s",
        _ => "gte_30s",
    }
}

/// Validates an untrusted key/value record before it can be recorded. Keys are
/// checked against the forbidden list first so a caller cannot smuggle a
/// high-cardinality identifier into an "unknown" field.
pub(crate) fn normalize_record(record: &Value) -> Result<TelemetryRecord> {
    let object = record
        .as_object()
        .ok_or_else(|| error("controller_telemetry_record_invalid"))?;
    for key in object.keys() {
        let lowered = key.to_ascii_lowercase();
        if FORBIDDEN_KEYS
            .iter()
            .any(|forbidden| lowered.contains(forbidden))
        {
            return Err(error("controller_telemetry_field_forbidden"));
        }
        if !matches!(
            lowered.as_str(),
            "stage" | "outcome" | "reason" | "latency_bucket" | "count"
        ) {
            return Err(error("controller_telemetry_field_unknown"));
        }
    }
    let stage = object
        .get("stage")
        .and_then(Value::as_str)
        .filter(|value| STAGES.contains(value))
        .ok_or_else(|| error("controller_telemetry_stage_unknown"))?;
    let outcome = object
        .get("outcome")
        .and_then(Value::as_str)
        .filter(|value| OUTCOMES.contains(value))
        .ok_or_else(|| error("controller_telemetry_outcome_unknown"))?;
    let reason = object
        .get("reason")
        .and_then(Value::as_str)
        .unwrap_or("none");
    if !REASONS.contains(&reason) {
        return Err(error("controller_telemetry_reason_unknown"));
    }
    let latency_bucket = object
        .get("latency_bucket")
        .and_then(Value::as_str)
        .unwrap_or("lt_50ms");
    if !LATENCY_BUCKETS.contains(&latency_bucket) {
        return Err(error("controller_telemetry_latency_unknown"));
    }
    let count = object.get("count").and_then(Value::as_u64).unwrap_or(1);
    if count > MAX_COUNT {
        return Err(error("controller_telemetry_count_invalid"));
    }
    // The enum checks above guarantee these lookups succeed; the fallbacks keep
    // the function total without ever widening the accepted set.
    Ok(TelemetryRecord {
        stage: STAGES
            .iter()
            .find(|candidate| **candidate == stage)
            .copied()
            .unwrap_or("task"),
        outcome: OUTCOMES
            .iter()
            .find(|candidate| **candidate == outcome)
            .copied()
            .unwrap_or("internal_error"),
        reason: REASONS
            .iter()
            .find(|candidate| **candidate == reason)
            .copied()
            .unwrap_or("unknown"),
        latency_bucket: LATENCY_BUCKETS
            .iter()
            .find(|candidate| **candidate == latency_bucket)
            .copied()
            .unwrap_or("gte_30s"),
        count,
    })
}

/// Anonymous aggregation store. Nothing here can reconstruct a user, a device
/// or a pairing instance.
#[derive(Default)]
pub(crate) struct RemoteControlTelemetry {
    counters: Mutex<HashMap<String, u64>>,
    dedupe: Mutex<HashMap<String, ()>>,
}

impl RemoteControlTelemetry {
    pub(crate) fn record(
        &self,
        record: &Value,
        dedupe_key: Option<&str>,
    ) -> Result<TelemetryRecord> {
        let normalized = normalize_record(record)?;
        if let Some(key) = dedupe_key {
            let mut dedupe = self.dedupe.lock();
            if dedupe.contains_key(key) {
                return Err(error("controller_telemetry_duplicate"));
            }
            if dedupe.len() >= 4096 {
                dedupe.clear();
            }
            dedupe.insert(key.to_owned(), ());
        }
        let key = format!(
            "{}|{}|{}|{}",
            normalized.stage, normalized.outcome, normalized.reason, normalized.latency_bucket
        );
        let mut counters = self.counters.lock();
        let entry = counters.entry(key).or_insert(0);
        *entry = entry.saturating_add(normalized.count);
        Ok(normalized)
    }

    pub(crate) fn snapshot(&self) -> Value {
        let counters = self.counters.lock();
        let mut object = serde_json::Map::new();
        let mut total = 0u64;
        for (key, value) in counters.iter() {
            object.insert(key.clone(), json!(value));
            total = total.saturating_add(*value);
        }
        json!({ "counters": Value::Object(object), "total": total })
    }

    pub(crate) fn reset(&self) {
        self.counters.lock().clear();
        self.dedupe.lock().clear();
    }
}

/// Security gate used by the leak tests: scans an arbitrary text (log line,
/// persisted file, crash note) for credential-like sentinels.
pub(crate) fn find_sensitive_sentinels(text: &str) -> Vec<String> {
    let mut found = Vec::new();
    for sentinel in SENSITIVE_SENTINELS {
        if text.contains(sentinel) {
            found.push((*sentinel).to_owned());
        }
    }
    // Any `sk-` plus at least eight identifier characters is treated as a key
    // even when the provider prefix is not enumerated above.
    let bytes = text.as_bytes();
    let mut index = 0;
    while index + 3 <= bytes.len() {
        if &bytes[index..index + 3] == b"sk-" {
            let tail = &text[index + 3..];
            let length = tail
                .chars()
                .take_while(|ch| ch.is_ascii_alphanumeric() || *ch == '-' || *ch == '_')
                .count();
            if length >= 8 {
                found.push("sk-***".to_owned());
                break;
            }
        }
        index += 1;
    }
    found
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_allowlisted_enums_are_accepted() {
        assert_eq!(
            normalize_record(&json!({ "stage": "task", "outcome": "ok" }))
                .unwrap()
                .stage,
            "task"
        );
        assert_eq!(
            normalize_record(&json!({ "stage": "pairing", "outcome": "ok" }))
                .unwrap_err()
                .code,
            "controller_telemetry_stage_unknown"
        );
        assert_eq!(
            normalize_record(&json!({ "stage": "task", "outcome": "maybe" }))
                .unwrap_err()
                .code,
            "controller_telemetry_outcome_unknown"
        );
        assert_eq!(
            normalize_record(&json!({ "stage": "task", "outcome": "ok", "reason": "because" }))
                .unwrap_err()
                .code,
            "controller_telemetry_reason_unknown"
        );
        assert_eq!(
            normalize_record(&json!({
                "stage": "task",
                "outcome": "ok",
                "latency_bucket": "instant"
            }))
            .unwrap_err()
            .code,
            "controller_telemetry_latency_unknown"
        );
        assert_eq!(
            normalize_record(&json!({
                "stage": "task",
                "outcome": "ok",
                "count": MAX_COUNT + 1
            }))
            .unwrap_err()
            .code,
            "controller_telemetry_count_invalid"
        );
    }

    #[test]
    fn identity_and_content_fields_fail_closed() {
        for field in [
            "endpoint_id",
            "lease_id",
            "resume_token",
            "pairing_code",
            "api_key",
            "device_name",
            "prompt",
            "file_path",
            "remote_addr",
        ] {
            let mut record = serde_json::Map::new();
            record.insert("stage".into(), json!("task"));
            record.insert("outcome".into(), json!("ok"));
            record.insert(field.into(), json!("x"));
            assert_eq!(
                normalize_record(&Value::Object(record)).unwrap_err().code,
                "controller_telemetry_field_forbidden",
                "{field} must be rejected"
            );
        }
        assert_eq!(
            normalize_record(&json!({ "stage": "task", "outcome": "ok", "note": "x" }))
                .unwrap_err()
                .code,
            "controller_telemetry_field_unknown"
        );
    }

    #[test]
    fn latency_buckets_are_bounded() {
        assert_eq!(bucket_latency(0), "lt_50ms");
        assert_eq!(bucket_latency(49), "lt_50ms");
        assert_eq!(bucket_latency(50), "lt_250ms");
        assert_eq!(bucket_latency(4999), "lt_5s");
        assert_eq!(bucket_latency(30_000), "gte_30s");
        assert_eq!(bucket_latency(u64::MAX), "gte_30s");
    }

    #[test]
    fn counters_aggregate_and_dedupe() {
        let telemetry = RemoteControlTelemetry::default();
        telemetry
            .record(
                &json!({ "stage": "task", "outcome": "ok", "latency_bucket": "lt_1s" }),
                Some("event-1"),
            )
            .unwrap();
        telemetry
            .record(
                &json!({
                    "stage": "task",
                    "outcome": "ok",
                    "latency_bucket": "lt_1s",
                    "count": 2
                }),
                Some("event-2"),
            )
            .unwrap();
        assert_eq!(
            telemetry
                .record(
                    &json!({ "stage": "task", "outcome": "ok" }),
                    Some("event-1")
                )
                .unwrap_err()
                .code,
            "controller_telemetry_duplicate"
        );
        let snapshot = telemetry.snapshot();
        assert_eq!(snapshot["total"], json!(3));
        let encoded = snapshot.to_string();
        assert!(!encoded.contains("endpoint"));
        assert!(!encoded.contains("lease"));
        telemetry.reset();
        assert_eq!(telemetry.snapshot()["total"], json!(0));
    }

    #[test]
    fn sentinel_scanner_catches_keys_tokens_and_envelopes() {
        assert!(find_sensitive_sentinels("api key sk-live-1234567890 leaked").len() >= 2);
        assert!(
            find_sensitive_sentinels("Authorization: Bearer abc")
                .contains(&"Authorization:".to_owned())
        );
        assert!(find_sensitive_sentinels("vault://secret").contains(&"vault://".to_owned()));
        assert!(
            find_sensitive_sentinels("-----BEGIN PRIVATE KEY-----")
                .contains(&"-----BEGIN".to_owned())
        );
        assert!(find_sensitive_sentinels("harmless").is_empty());
        assert!(find_sensitive_sentinels("sk-short").is_empty());
    }
}
