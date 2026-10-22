//! Content-free failure witnesses and explicitly previewed support material.
//! This adapter observes existing lifecycle events; it never owns execution.
use super::*;
use anyhow::Context as _;
use sha2::{Digest, Sha256};

const NOTICE: &str = "failure-diagnostics-v1";
const RETENTION: u64 = 14 * 24 * 60 * 60 * 1000;
const MAX_STATE: usize = 16 * 1024 * 1024;
const MAX_REPORT_BYTES: usize = 2300 * 1024;

fn report_request_body(device_id: &str, period: &str, payload: &Value) -> Result<Value> {
    let body = json!({"device_id":device_id,"collection_period_id":period,"report":payload});
    anyhow::ensure!(
        serde_json::to_vec(&body)?.len() <= MAX_REPORT_BYTES,
        "diagnostic_report_too_large"
    );
    Ok(body)
}

fn sanitize_environment(environment: &mut serde_json::Map<String, Value>) {
    environment.retain(|key, value| {
        let allowed = |choices: &[&str]| value.as_str().is_some_and(|v| choices.contains(&v));
        match key.as_str() {
            "provider_key" => allowed(CONTROLLED_PROVIDER_KEYS),
            "model_key" => allowed(CONTROLLED_MODEL_KEYS),
            "tool_key" => allowed(CONTROLLED_TOOL_KEYS),
            "routing" => allowed(&["local", "remote_controller", "remote_controlled"]),
            "mode" => allowed(&["assistant", "code", "scheduled", "team", "system"]),
            "input_type" => allowed(&["text", "voice", "attachment", "mixed", "system"]),
            "has_attachment" => value.is_boolean(),
            _ => false,
        }
    });
}

#[derive(Debug, Clone)]
pub(super) struct DiagnosticHint {
    pub(super) code: String,
    pub(super) http: Option<u16>,
    pub(super) site: &'static str,
}
impl BehaviorEvent {
    pub(super) fn diagnostic_cancellation(mut self, cancelled: bool) -> Self {
        self.diagnostic_cancelled = cancelled;
        self
    }
    pub(crate) fn diagnostic_error(mut self, error: &str, site: &'static str) -> Self {
        let (code, http) = classify(error);
        self.diagnostic_hint = Some(DiagnosticHint {
            code: code.into(),
            http,
            site,
        });
        self
    }
}

pub(super) fn known_field(field: &str) -> bool {
    matches!(
        field,
        "diagnostic_id"
            | "diagnostic_origin_period_id"
            | "diagnostic_trace_id"
            | "diagnostic_code"
            | "diagnostic_component"
            | "diagnostic_site"
            | "diagnostic_inferred"
            | "diagnostic_build"
            | "diagnostic_http_status"
            | "diagnostic_exit_code"
            | "diagnostic_retryable"
            | "diagnostic_steps"
            | "diagnostic_environment"
            | "diagnostic_sampled"
    )
}
pub(super) fn validate(event: &BehaviorEvent) -> Result<()> {
    let Some(id) = event.fields.get("diagnostic_id") else {
        anyhow::ensure!(
            !event.fields.keys().any(|k| k.starts_with("diagnostic_")),
            "orphan diagnostic fields"
        );
        return Ok(());
    };
    anyhow::ensure!(
        failure(event) && id.as_str() == Some(&event.event_id),
        "invalid diagnostic identity"
    );
    static SCHEMA: std::sync::OnceLock<std::result::Result<Value, serde_json::Error>> =
        std::sync::OnceLock::new();
    let schema = SCHEMA
        .get_or_init(|| {
            serde_json::from_str(include_str!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../docs/schemas/behavior-telemetry-v1.schema.json"
            )))
        })
        .as_ref()
        .map_err(|error| anyhow::anyhow!("parse diagnostic schema: {error}"))?;
    for required in [
        "diagnostic_trace_id",
        "diagnostic_code",
        "diagnostic_component",
        "diagnostic_site",
    ] {
        anyhow::ensure!(
            event.fields.contains_key(required),
            "missing diagnostic field"
        );
    }
    for (key, value) in &event.fields {
        if !key.starts_with("diagnostic_") {
            continue;
        }
        anyhow::ensure!(known_field(key), "unknown diagnostic field");
        let definition = &schema["properties"][key];
        if let Some(values) = definition["enum"].as_array() {
            anyhow::ensure!(values.contains(value), "invalid diagnostic enum");
        }
        match key.as_str() {
            "diagnostic_origin_period_id" => anyhow::ensure!(
                value.as_str().is_some_and(|s| (12..=128).contains(&s.len())
                    && s.bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b))),
                "invalid diagnostic origin"
            ),
            "diagnostic_trace_id" => anyhow::ensure!(
                value.as_str().is_some_and(|s| s.len() == 64
                    && s.bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))),
                "invalid diagnostic trace"
            ),
            "diagnostic_build" => anyhow::ensure!(
                value.as_str().is_some_and(|s| !s.is_empty()
                    && s.len() <= 64
                    && s.bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b"._+-".contains(&b))),
                "invalid diagnostic build"
            ),
            "diagnostic_inferred" | "diagnostic_retryable" | "diagnostic_sampled" => {
                anyhow::ensure!(value.is_boolean(), "invalid diagnostic boolean")
            }
            "diagnostic_http_status" => anyhow::ensure!(
                value.as_u64().is_some_and(|n| (100..=599).contains(&n)),
                "invalid HTTP status"
            ),
            "diagnostic_exit_code" => anyhow::ensure!(
                value.as_i64().is_some_and(|n| i32::try_from(n).is_ok()),
                "invalid process code"
            ),
            "diagnostic_steps" => {
                let steps = value.as_array().context("invalid diagnostic steps")?;
                anyhow::ensure!(steps.len() <= 24, "too many diagnostic steps");
                for step in steps {
                    let fields = step.as_object().context("invalid diagnostic step")?;
                    anyhow::ensure!(
                        fields.len() == 2
                            && fields.contains_key("stage")
                            && fields.contains_key("offset_ms")
                            && schema["properties"]["stage"]["enum"]
                                .as_array()
                                .is_some_and(|stages| stages.contains(&step["stage"]))
                            && step["offset_ms"].as_u64().is_some_and(|n| n <= 604800000),
                        "invalid diagnostic step"
                    );
                }
            }
            "diagnostic_environment" => {
                let environment = value
                    .as_object()
                    .context("invalid diagnostic environment")?;
                for (name, value) in environment {
                    let definition = &definition["properties"][name];
                    anyhow::ensure!(!definition.is_null(), "unknown environment field");
                    if let Some(choices) = definition["enum"].as_array() {
                        anyhow::ensure!(choices.contains(value), "invalid environment enum");
                    } else {
                        anyhow::ensure!(
                            name == "has_attachment" && value.is_boolean(),
                            "invalid environment field"
                        );
                    }
                }
            }
            _ => {}
        }
    }
    Ok(())
}

#[derive(Clone, Serialize, Deserialize)]
struct Context {
    trace: String,
    lease: CollectionLease,
    started: u64,
    #[serde(default)]
    last_used: u64,
    turn: Option<String>,
    steps: Vec<Value>,
    code: Option<String>,
    site: String,
    http: Option<u16>,
    #[serde(default)]
    environment: serde_json::Map<String, Value>,
    #[serde(default)]
    cancel_requested: bool,
}
#[derive(Clone, Serialize, Deserialize)]
struct Incident {
    event: Value,
    lease: CollectionLease,
    at: u64,
}
#[derive(Clone, Serialize, Deserialize)]
struct Pending {
    payload: Value,
    lease: CollectionLease,
    at: u64,
    #[serde(default)]
    status: String,
    #[serde(default)]
    reason: String,
    #[serde(default)]
    next_attempt_at: u64,
    #[serde(default)]
    attempts: u16,
}
#[derive(Clone)]
struct Preview {
    token: String,
    payload: Value,
    lease: CollectionLease,
    expires: u64,
}
#[derive(Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct DiagnosticState {
    contexts: HashMap<String, Context>,
    incidents: Vec<Incident>,
    #[serde(skip)]
    pending: Vec<Pending>,
    #[serde(skip)]
    previews: Vec<Preview>,
    #[serde(default)]
    sampling_day: u64,
    #[serde(default)]
    sampling_total: u16,
    #[serde(default)]
    sampling: HashMap<String, u8>,
}
fn path() -> PathBuf {
    paths::pinvou3_home().join("failure-diagnostics.json")
}
fn reports_path() -> PathBuf {
    paths::pinvou3_home().join("failure-diagnostic-reports.json")
}
fn persist_reports(state: &DiagnosticState) -> Result<()> {
    let bytes = serde_json::to_vec(&state.pending)?;
    anyhow::ensure!(bytes.len() <= MAX_STATE, "diagnostic report capacity");
    filesystem::atomic_write_private(&reports_path(), &bytes)?;
    Ok(())
}
fn persist(state: &DiagnosticState) -> Result<()> {
    let bytes = serde_json::to_vec(state)?;
    anyhow::ensure!(bytes.len() <= MAX_STATE, "diagnostic local capacity");
    filesystem::atomic_write_private(&path(), &bytes)?;
    Ok(())
}
fn prune(state: &mut DiagnosticState) {
    let previous = (
        state.contexts.len(),
        state.incidents.len(),
        state.pending.len(),
    );
    let cutoff = now_ms().saturating_sub(RETENTION);
    state.contexts.retain(|_, c| c.started >= cutoff);
    while state.contexts.len() > 128 {
        evict_oldest_context(state);
    }
    state.incidents.retain(|i| i.at >= cutoff);
    if state.incidents.len() > 50 {
        state.incidents.drain(..state.incidents.len() - 50);
    }
    state.pending.retain(|p| p.at >= cutoff);
    while state.pending.len() > 50 {
        if let Some(index) = state.pending.iter().position(|p| p.status != "queued") {
            state.pending.remove(index);
        } else {
            break;
        }
    }
    state.previews.retain(|p| p.expires > now_ms());
    if previous.0 != state.contexts.len() || previous.1 != state.incidents.len() {
        let _ = persist(state);
    }
    if previous.2 != state.pending.len() {
        let _ = persist_reports(state);
    }
}
fn evict_oldest_context(state: &mut DiagnosticState) {
    let oldest = state
        .contexts
        .iter()
        .min_by_key(|(key, context)| (context.last_used.max(context.started), *key))
        .map(|(key, _)| key.clone());
    if let Some(key) = oldest {
        state.contexts.remove(&key);
    }
}
pub(super) fn load() -> DiagnosticState {
    let mut state: DiagnosticState = std::fs::symlink_metadata(path())
        .ok()
        .filter(|m| m.is_file() && !m.file_type().is_symlink() && m.len() <= MAX_STATE as u64)
        .and_then(|_| {
            filesystem::read_private_data_file_bytes_within(&path(), MAX_STATE as u64).ok()
        })
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default();
    state.pending = std::fs::symlink_metadata(reports_path())
        .ok()
        .filter(|m| m.is_file() && !m.file_type().is_symlink() && m.len() <= MAX_STATE as u64)
        .and_then(|_| {
            filesystem::read_private_data_file_bytes_within(&reports_path(), MAX_STATE as u64).ok()
        })
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default();
    prune(&mut state);
    state
}
pub(super) fn clear(telemetry: &BehaviorTelemetry) -> Result<()> {
    let mut state = telemetry
        .diagnostics
        .lock()
        .map_err(|_| anyhow::anyhow!("diagnostic lock poisoned"))?;
    *state = DiagnosticState::default();
    persist(&state)?;
    persist_reports(&state)
}
pub(super) fn sanitize_authorization(telemetry: &BehaviorTelemetry) {
    if let Ok(mut state) = telemetry.diagnostics.lock() {
        state
            .contexts
            .retain(|_, context| telemetry.lease_is_current(&context.lease));
        state
            .incidents
            .retain(|incident| telemetry.lease_is_current(&incident.lease));
        state
            .pending
            .retain(|report| telemetry.lease_is_current(&report.lease));
        let _ = persist(&state);
        let _ = persist_reports(&state);
    }
}
pub(super) fn rebind(telemetry: &BehaviorTelemetry, generation: u64, period: &str) -> Result<()> {
    let mut state = telemetry
        .diagnostics
        .lock()
        .map_err(|_| anyhow::anyhow!("diagnostic lock poisoned"))?;
    let renew = |lease: &mut CollectionLease| {
        if lease.generation == generation {
            lease.collection_period_id = period.to_owned();
        }
    };
    for context in state.contexts.values_mut() {
        renew(&mut context.lease);
    }
    for incident in &mut state.incidents {
        renew(&mut incident.lease);
    }
    for report in &mut state.pending {
        renew(&mut report.lease);
    }
    // A preview is an exact-content authorization; renewing invalidates it.
    state.previews.clear();
    persist(&state)?;
    persist_reports(&state)
}
pub(super) fn origin_periods(
    telemetry: &BehaviorTelemetry,
    generation: u64,
) -> Result<HashMap<String, String>> {
    let state = telemetry
        .diagnostics
        .lock()
        .map_err(|_| anyhow::anyhow!("diagnostic lock poisoned"))?;
    let mut origins = HashMap::new();
    // The incident snapshot and confirmed report preserve the original period
    // even if an older client already rebound its queued event. Never infer it
    // from another generation or silently choose between conflicting evidence.
    let incidents = state
        .incidents
        .iter()
        .filter(|i| i.lease.generation == generation)
        .map(|i| (&i.event["diagnostic_id"], &i.event["collection_period_id"]));
    let reports = state
        .pending
        .iter()
        .filter(|p| p.lease.generation == generation)
        .map(|p| {
            (
                &p.payload["incident_id"],
                p.payload
                    .get("incident_period_id")
                    .unwrap_or(&p.payload["collection_period_id"]),
            )
        });
    for (id, period) in incidents.chain(reports) {
        if let (Some(id), Some(period)) = (id.as_str(), period.as_str()) {
            if let Some(prior) = origins.insert(id.to_owned(), period.to_owned()) {
                anyhow::ensure!(prior == period, "diagnostic_origin_conflict");
            }
        }
    }
    Ok(origins)
}
fn digest(value: &str) -> String {
    crate::platform::encoding::hex_lower(&Sha256::digest(value.as_bytes()))
}
fn field<'a>(event: &'a BehaviorEvent, key: &str) -> Option<&'a str> {
    event.fields.get(key)?.as_str()
}
fn event_stage(event: &BehaviorEvent) -> &'static str {
    match field(event, "stage") {
        Some("submission") => "submission",
        Some("queue") => "queue",
        Some("permission") => "permission",
        Some("recording") => "recording",
        Some("recognition") => "recognition",
        Some("model") => "model",
        Some("tool") => "tool",
        Some("output") => "output",
        Some("shutdown") => "shutdown",
        _ => match event.event_name {
            "task_submitted" | "task_rejected" => "submission",
            "task_admitted" => "queue",
            "model_used" | "model_fallback_occurred" => "model",
            "tool_call_started" | "tool_call_completed" => "tool",
            "task_first_output" | "artifact_created" => "output",
            "voice_recognition_failed" => "recognition",
            _ => "unknown",
        },
    }
}
fn failure(event: &BehaviorEvent) -> bool {
    if event.diagnostic_cancelled || field(event, "status") == Some("cancelled") {
        return false;
    }
    // Error-text classification is not evidence that a user cancelled.
    matches!(field(event, "status"), Some("failed" | "interrupted"))
        || event.fields.get("success") == Some(&json!(false))
        || matches!(
            event.event_name,
            "task_rejected" | "provider_setup_failed" | "voice_recognition_failed"
        )
        || (event.event_name == "remote_control_event"
            && field(event, "remote_outcome").is_some_and(|s| s != "ok"))
        || (event.event_name == "response_feedback_changed"
            && field(event, "rating") == Some("negative")
            && field(event, "feedback_action") == Some("set"))
}
/// All outputs are registered constants. Never retain an error or its hash.
pub(super) fn classify(error: &str) -> (&'static str, Option<u16>) {
    let lower = error
        .chars()
        .take(8192)
        .collect::<String>()
        .to_ascii_lowercase();
    for (needle, code) in [
        ("insufficient_quota", "quota_exhausted"),
        ("context_length", "context_length_exceeded"),
        ("model_not_found", "model_not_found"),
        ("unexpected eof", "stream_unexpected_eof"),
        ("unexpected end", "stream_unexpected_eof"),
        ("connection reset", "connection_reset"),
        ("dns", "dns_failed"),
        ("certificate", "tls_failed"),
        ("task_permission_mode_required", "permission_required"),
        ("permission_changed", "permission_changed"),
        ("capability_snapshot", "capability_stale"),
    ] {
        if lower.contains(needle) {
            return (code, None);
        }
    }
    for status in [400, 401, 403, 404, 408, 413, 429, 500, 502, 503, 504] {
        if [
            format!("http {status}"),
            format!("status: {status}"),
            format!("status {status}"),
        ]
        .iter()
        .any(|n| {
            lower.match_indices(n).any(|(at, _)| {
                !lower
                    .as_bytes()
                    .get(at + n.len())
                    .is_some_and(u8::is_ascii_digit)
            })
        }) {
            return (
                match status {
                    401 | 403 => "authentication_failed",
                    408 | 504 => "request_timeout",
                    429 => "rate_limited",
                    500 | 502 | 503 => "provider_unavailable",
                    _ => "response_invalid",
                },
                Some(status),
            );
        }
    }
    (
        match super::events::error_category(Some(error)) {
            "permission_denied" => "permission_denied",
            "authentication_failed" => "authentication_failed",
            "configuration_missing" => "configuration_missing",
            "network_unavailable" => "network_unavailable",
            "timeout" => "request_timeout",
            "rate_limited" => "rate_limited",
            _ => "unknown",
        },
        None,
    )
}
pub(crate) fn capture_failure_context(
    app: &tauri::AppHandle,
    session: &str,
    error: &str,
    acp: bool,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let Ok(mut state) = telemetry.diagnostics.lock() else {
        return;
    };
    let Some(context) = state.contexts.get_mut(session) else {
        return;
    };
    if !telemetry.lease_is_current(&context.lease) {
        return;
    }
    let (code, http) = classify(error);
    context.code = Some(code.into());
    context.http = http;
    context.site = if acp { "acp_error" } else { "assistant_error" }.into();
}
pub(super) fn enrich(
    telemetry: &BehaviorTelemetry,
    event: &mut BehaviorEvent,
    lease: &CollectionLease,
) {
    if !telemetry.lease_is_current(lease) {
        return;
    }
    // Stateless observations have no lifecycle to correlate. Failures still
    // receive their own bounded witness, but unrelated settings/install events
    // must not evict active task traces.
    if event.diagnostic_remote.is_none()
        && field(event, "session_id").is_none()
        && field(event, "operation_id").is_none()
        && !failure(event)
    {
        return;
    }
    let mut key = event
        .diagnostic_remote
        .as_ref()
        .map(|(trace, _)| trace.as_str())
        .or_else(|| field(event, "session_id"))
        .or_else(|| field(event, "operation_id"))
        .unwrap_or(&event.event_id)
        .to_owned();
    if event.event_name == "response_feedback_changed" {
        key = format!(
            "quality|{key}|{}",
            field(event, "turn_id").unwrap_or("unknown")
        );
    }
    let Ok(mut state) = telemetry.diagnostics.lock() else {
        return;
    };
    prune(&mut state);
    if event.event_name == "task_submitted" || !state.contexts.contains_key(&key) {
        let source = (event.event_name == "response_feedback_changed")
            .then(|| {
                field(event, "session_id")
                    .and_then(|session| state.contexts.get(session))
                    .filter(|context| {
                        context.lease == *lease
                            && context.turn.as_deref() == field(event, "turn_id")
                    })
                    .cloned()
            })
            .flatten();
        if state.contexts.len() >= 128 && !state.contexts.contains_key(&key) {
            evict_oldest_context(&mut state);
        }
        state.contexts.insert(
            key.clone(),
            source.unwrap_or_else(|| Context {
                trace: digest(&format!("failure-trace-v1|{}", event.event_id)),
                lease: lease.clone(),
                started: event.occurred_at,
                last_used: event.occurred_at,
                turn: None,
                steps: Vec::new(),
                code: None,
                site: "lifecycle".into(),
                http: None,
                environment: Default::default(),
                cancel_requested: false,
            }),
        );
    }
    let Some(context) = state.contexts.get_mut(&key) else {
        // Enrichment is best effort; a missing observer context must not panic
        // or interfere with the original business event.
        return;
    };
    if context.lease != *lease {
        return;
    }
    // Loaded JSON is not proof that cached categories still satisfy the current
    // registries. Extension corruption must not drop the base terminal event.
    sanitize_environment(&mut context.environment);
    context.last_used = context.last_used.max(event.occurred_at);
    if let Some(turn) = field(event, "turn_id") {
        if context.turn.as_deref().is_some_and(|t| t != turn) {
            return;
        }
        context.turn = Some(turn.to_owned());
    }
    let stage = event_stage(event);
    if let Some((trace, controlled)) = &event.diagnostic_remote {
        context.trace = trace.clone();
        context.environment.insert(
            "routing".into(),
            json!(if *controlled {
                "remote_controlled"
            } else {
                "remote_controller"
            }),
        );
    } else {
        context
            .environment
            .entry("routing")
            .or_insert(json!("local"));
    }
    // Only controlled categories are copied; never copy custom model/tool names.
    for (key, registry) in [
        ("provider_key", CONTROLLED_PROVIDER_KEYS),
        ("model_key", CONTROLLED_MODEL_KEYS),
        ("tool_key", CONTROLLED_TOOL_KEYS),
    ] {
        if let Some(value) = field(event, key).filter(|value| registry.contains(value)) {
            context.environment.insert(key.into(), json!(value));
        }
    }
    for (key, choices) in [
        (
            "mode",
            &["assistant", "code", "scheduled", "team", "system"][..],
        ),
        (
            "input_type",
            &["text", "voice", "attachment", "mixed", "system"][..],
        ),
    ] {
        if context
            .environment
            .get(key)
            .is_some_and(|value| !value.as_str().is_some_and(|value| choices.contains(&value)))
        {
            context.environment.remove(key);
        }
        if let Some(value) = event.fields.get(key) {
            if value.as_str().is_some_and(|value| choices.contains(&value)) {
                context.environment.insert(key.into(), value.clone());
            } else {
                context.environment.remove(key);
            }
        }
    }
    if context
        .environment
        .get("has_attachment")
        .is_some_and(|value| !value.is_boolean())
    {
        context.environment.remove("has_attachment");
    }
    if let Some(value) = event
        .fields
        .get("has_attachment")
        .filter(|value| value.is_boolean())
    {
        context
            .environment
            .insert("has_attachment".into(), value.clone());
    }
    if context.steps.last().and_then(|s| s["stage"].as_str()) != Some(stage) {
        context.steps.push(json!({"stage": stage, "offset_ms": event.occurred_at.saturating_sub(context.started).min(604800000)}));
        if context.steps.len() > 24 {
            context.steps.remove(0);
        }
    }
    if event.event_name == "task_cancel_requested" {
        context.cancel_requested = true;
    }
    let cancelled_observation = context.cancel_requested
        && (field(event, "status") == Some("interrupted")
            || event.event_name == "tool_call_completed");
    if failure(event) && !cancelled_observation {
        let fallback =
            match field(event, "error_category").or_else(|| field(event, "remote_reason")) {
                Some("timeout") => "request_timeout",
                Some("tool_error") => "tool_failed",
                Some("model_error" | "provider_error") => "provider_unavailable",
                Some("permission_required") => "permission_required",
                Some("permission_changed") => "permission_changed",
                Some("permission_denied") => "permission_denied",
                Some("authentication_failed") => "authentication_failed",
                Some("configuration_missing") => "configuration_missing",
                Some("network_unavailable") => "network_unavailable",
                Some("rate_limited") => "rate_limited",
                Some("unsupported") => "unsupported",
                Some("validation_failed") => "validation_failed",
                Some("artifact_missing") => "artifact_missing",
                Some("resume_failed" | "watchdog_expired" | "liveness_missing") => {
                    "remote_disconnected"
                }
                Some("permission_timeout") => "request_timeout",
                Some("internal_error") => "internal_error",
                _ => "unknown",
            };
        let inferred = stage == "shutdown";
        let hint = event.diagnostic_hint.as_ref();
        let code = if event.event_name == "response_feedback_changed" {
            "user_reported_quality"
        } else if inferred {
            "shutdown_unconfirmed"
        } else if stage == "tool" {
            hint.map(|h| h.code.as_str())
                .filter(|c| *c != "unknown")
                .unwrap_or("tool_failed")
        } else {
            hint.map(|h| h.code.as_str())
                .or(context.code.as_deref())
                .unwrap_or(fallback)
        };
        let component = if event.event_name == "remote_control_event" {
            "remote_control"
        } else if event.event_name.starts_with("voice_") {
            "voice"
        } else if event.event_name.starts_with("provider_setup_") {
            "configuration"
        } else if stage == "tool" {
            "tool"
        } else if context.site == "acp_error" {
            "acp"
        } else if context.site == "assistant_error" {
            "assistant"
        } else if stage == "model" {
            "model"
        } else {
            "app"
        };
        event
            .fields
            .insert("diagnostic_id".into(), json!(event.event_id));
        event.fields.insert(
            "diagnostic_origin_period_id".into(),
            json!(lease.collection_period_id),
        );
        event
            .fields
            .insert("diagnostic_trace_id".into(), json!(context.trace));
        event.fields.insert("diagnostic_code".into(), json!(code));
        event
            .fields
            .insert("diagnostic_component".into(), json!(component));
        event.fields.insert(
            "diagnostic_site".into(),
            json!(if inferred {
                "recovery"
            } else if stage == "tool" {
                "tool_result"
            } else {
                hint.map_or(context.site.as_str(), |h| h.site)
            }),
        );
        event
            .fields
            .insert("diagnostic_inferred".into(), json!(inferred));
        event
            .fields
            .insert("diagnostic_build".into(), json!(env!("CARGO_PKG_VERSION")));
        event
            .fields
            .insert("diagnostic_steps".into(), json!(context.steps));
        event
            .fields
            .insert("diagnostic_environment".into(), json!(context.environment));
        event.fields.insert(
            "diagnostic_retryable".into(),
            json!(matches!(
                code,
                "request_timeout" | "rate_limited" | "connection_reset" | "provider_unavailable"
            )),
        );
        if let Some(http) = hint
            .and_then(|h| h.http)
            .or_else(|| (stage != "tool").then_some(context.http).flatten())
        {
            event
                .fields
                .insert("diagnostic_http_status".into(), json!(http));
        }
        if event.event_name != "remote_control_event" {
            event.fields.entry("stage").or_insert(json!(stage));
        }
        if !state
            .incidents
            .iter()
            .any(|i| i.event["diagnostic_id"] == event.event_id)
        {
            if let Ok(mut value) = serde_json::to_value(&event) {
                value["collection_period_id"] = json!(lease.collection_period_id);
                state.incidents.push(Incident {
                    event: value,
                    lease: lease.clone(),
                    at: event.occurred_at,
                });
            }
        }
        // Always retain the fault identity/count. Only expanded breadcrumbs
        // are sampled, independently from task-success metrics and reports.
        let day = now_ms() / 86400000;
        if state.sampling_day != day {
            state.sampling_day = day;
            state.sampling_total = 0;
            state.sampling.clear();
        }
        let sample_key = format!(
            "{}|{}|{}",
            event.fields["diagnostic_component"], stage, event.fields["diagnostic_code"]
        );
        let count = state.sampling.get(&sample_key).copied().unwrap_or(0);
        let sampled = count < 5 && state.sampling_total < 50;
        if sampled {
            state.sampling.insert(sample_key, count + 1);
            state.sampling_total += 1;
        }
        event
            .fields
            .insert("diagnostic_sampled".into(), json!(sampled));
        if !sampled {
            event.fields.remove("diagnostic_steps");
        }
    }
    if matches!(
        event.event_name,
        "task_submitted" | "task_admitted" | "task_finished" | "task_rejected"
    ) || failure(event)
    {
        prune(&mut state);
        let _ = persist(&state);
    }
}

pub(crate) fn diagnostic_incidents(telemetry: &BehaviorTelemetry) -> Result<Value> {
    let mut state = telemetry
        .diagnostics
        .lock()
        .map_err(|_| anyhow::anyhow!("diagnostic lock poisoned"))?;
    prune(&mut state);
    Ok(
        json!({"enabled": telemetry.current_lease().is_some(), "notice_version": NOTICE,
        "incidents": state.incidents.iter().rev().filter(|i| telemetry.lease_is_current(&i.lease)).map(|i| &i.event).collect::<Vec<_>>(),
        "reports": state.pending.iter().map(|p| json!({"report_id":p.payload["report_id"],"incident_id":p.payload["incident_id"],"status":p.status,"reason":p.reason})).collect::<Vec<_>>() }),
    )
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct DiagnosticPreviewRequest {
    pub incident_id: String,
    pub notes: String,
    #[serde(default)]
    pub attachments: Vec<String>,
}
// Public command errors remain a closed vocabulary: never expose a path,
// decoder message or arbitrary storage error through the IPC/UI boundary.
pub(crate) fn diagnostic_error_code(error: &anyhow::Error, fallback: &'static str) -> String {
    for cause in error.chain() {
        let text = cause.to_string();
        if matches!(
            text.as_str(),
            "diagnostic_collection_disabled"
                | "diagnostic_incident_unavailable"
                | "diagnostic_preview_expired"
                | "diagnostic_authorization_changed"
                | "diagnostic_queue_full"
                | "diagnostic_confirmation_required"
                | "diagnostic_input_too_large"
                | "diagnostic_attachment_invalid"
                | "diagnostic_attachment_too_large"
                | "diagnostic_text_attachment_too_large"
                | "diagnostic_image_invalid"
                | "diagnostic_image_too_large"
                | "diagnostic_report_too_large"
        ) {
            return text;
        }
    }
    fallback.to_owned()
}
fn clean_image(bytes: &[u8], format: xcap::image::ImageFormat) -> Result<Vec<u8>> {
    use xcap::image::{ImageReader, Limits};
    let mut limits = Limits::default();
    limits.max_image_width = Some(4096);
    limits.max_image_height = Some(4096);
    limits.max_alloc = Some(64 * 1024 * 1024);
    let mut reader = ImageReader::with_format(std::io::Cursor::new(bytes), format);
    reader.limits(limits);
    let image = reader.decode().context("diagnostic_image_invalid")?;
    let mut output = std::io::Cursor::new(Vec::new());
    // Decode/re-encode pixels to remove EXIF, filenames and other hidden metadata.
    image.write_to(&mut output, format)?;
    anyhow::ensure!(
        output.get_ref().len() <= 512 * 1024,
        "diagnostic_image_too_large"
    );
    Ok(output.into_inner())
}
pub(crate) fn preview_diagnostic(
    telemetry: &BehaviorTelemetry,
    request: DiagnosticPreviewRequest,
) -> Result<Value> {
    anyhow::ensure!(
        request.notes.chars().count() <= 6000 && request.attachments.len() <= 3,
        "diagnostic_input_too_large"
    );
    let lease = telemetry
        .current_lease()
        .context("diagnostic_collection_disabled")?;
    // One private, non-uploaded random scope across all notes and text attachments.
    let redaction_scope = token("redaction_", 32);
    let redactor = super::diagnostic_redaction::Redactor::new(&redaction_scope);
    let mut attachments = Vec::new();
    for selected in request.attachments {
        let metadata = std::fs::symlink_metadata(&selected)?;
        anyhow::ensure!(
            metadata.is_file()
                && !metadata.file_type().is_symlink()
                && metadata.len() <= 512 * 1024,
            "diagnostic_attachment_invalid"
        );
        let bytes =
            filesystem::read_private_data_file_bytes_within(Path::new(&selected), 512 * 1024)?;
        anyhow::ensure!(bytes.len() <= 512 * 1024, "diagnostic_attachment_too_large");
        use base64::Engine as _;
        let (kind, content) = if bytes.starts_with(b"\x89PNG\r\n\x1a\n") {
            (
                "image/png",
                base64::engine::general_purpose::STANDARD
                    .encode(clean_image(&bytes, xcap::image::ImageFormat::Png)?),
            )
        } else if bytes.starts_with(b"\xff\xd8\xff") {
            (
                "image/jpeg",
                base64::engine::general_purpose::STANDARD
                    .encode(clean_image(&bytes, xcap::image::ImageFormat::Jpeg)?),
            )
        } else {
            anyhow::ensure!(bytes.len() <= 65536, "diagnostic_text_attachment_too_large");
            (
                "text",
                redactor.bounded(std::str::from_utf8(&bytes)?, 65536, false),
            )
        };
        attachments.push(json!({"kind": kind, "content": content}));
    }
    let mut state = telemetry
        .diagnostics
        .lock()
        .map_err(|_| anyhow::anyhow!("diagnostic lock poisoned"))?;
    prune(&mut state);
    let incident = state
        .incidents
        .iter()
        .find(|i| i.event["diagnostic_id"] == request.incident_id && i.lease == lease)
        .context("diagnostic_incident_unavailable")?;
    let incident_period = incident.event["collection_period_id"].clone();
    let preview_token = token("preview_", 24);
    let payload = json!({"report_id": token("report_",24), "incident_id": request.incident_id,
        "collection_period_id": lease.collection_period_id, "incident_period_id": incident_period, "notice_version": NOTICE,
        "confirmed_at": now_ms(), "notes": redactor.bounded(&request.notes,6000,true), "attachments": attachments});
    // Reserve the full wire envelope, including the longest accepted identity
    // and delivery-period fields; credentials can renew after this preview.
    report_request_body(&"x".repeat(128), &"x".repeat(128), &payload)?;
    state.previews.clear();
    state.previews.push(Preview {
        token: preview_token.clone(),
        payload: payload.clone(),
        lease,
        expires: now_ms() + 600000,
    });
    Ok(json!({"preview_token": preview_token, "report":payload}))
}
pub(crate) fn confirm_diagnostic(
    telemetry: &BehaviorTelemetry,
    preview_token: &str,
    confirmed: bool,
) -> Result<Value> {
    anyhow::ensure!(confirmed, "diagnostic_confirmation_required");
    let mut state = telemetry
        .diagnostics
        .lock()
        .map_err(|_| anyhow::anyhow!("diagnostic lock poisoned"))?;
    prune(&mut state);
    let preview = state
        .previews
        .iter()
        .find(|p| p.token == preview_token)
        .cloned()
        .context("diagnostic_preview_expired")?;
    anyhow::ensure!(
        telemetry.lease_is_current(&preview.lease),
        "diagnostic_authorization_changed"
    );
    let id = preview.payload["report_id"].clone();
    // Keep the preview usable on storage failure, and make duplicate confirms
    // idempotent if the first IPC reply was lost.
    if state.pending.iter().any(|p| p.payload["report_id"] == id) {
        return Ok(json!({"report_id":id,"status":"queued"}));
    }
    anyhow::ensure!(
        state
            .pending
            .iter()
            .filter(|p| p.status == "queued")
            .count()
            < 4,
        "diagnostic_queue_full"
    );
    state.pending.push(Pending {
        payload: preview.payload,
        lease: preview.lease,
        at: now_ms(),
        status: "queued".into(),
        reason: String::new(),
        next_attempt_at: 0,
        attempts: 0,
    });
    if let Err(error) = persist_reports(&state) {
        state.pending.pop();
        return Err(error);
    }
    if let Some(tx) = &telemetry.report_tx {
        let _ = tx.try_send(());
    }
    Ok(json!({"report_id":id,"status":"queued"}))
}

pub(super) async fn bounded_reply(response: &mut reqwest::Response) -> Result<Value> {
    anyhow::ensure!(
        response.content_length().unwrap_or(0) <= 4096,
        "diagnostic_invalid_receipt"
    );
    let mut bytes = Vec::new();
    while let Some(chunk) = response.chunk().await? {
        anyhow::ensure!(
            bytes.len().saturating_add(chunk.len()) <= 4096,
            "diagnostic_invalid_receipt"
        );
        bytes.extend_from_slice(&chunk);
    }
    Ok(serde_json::from_slice(&bytes)?)
}
pub(super) async fn run_report_worker(mut rx: mpsc::Receiver<()>, telemetry: BehaviorTelemetry) {
    loop {
        let wait = telemetry.diagnostics.lock().ok().and_then(|mut state| {
            prune(&mut state);
            state
                .pending
                .iter()
                .filter(|p| p.status == "queued")
                .map(|p| p.next_attempt_at.saturating_sub(now_ms()).max(1))
                .min()
                .map(Duration::from_millis)
        });
        if let Some(wait) = wait {
            tokio::select! {
                signal = rx.recv() => if signal.is_none() { break; },
                _ = tokio::time::sleep(wait) => {},
            }
        } else if rx.recv().await.is_none() {
            break;
        }
        let report = telemetry.diagnostics.lock().ok().and_then(|mut s| {
            prune(&mut s);
            s.pending
                .iter()
                .filter(|p| p.status == "queued" && p.next_attempt_at <= now_ms())
                .min_by_key(|p| (p.next_attempt_at, p.at))
                .cloned()
        });
        let Some(report) = report else {
            continue;
        };
        if !telemetry.lease_is_current(&report.lease) {
            // Wait for an in-flight control operation before distinguishing a
            // durable interrupted renewal from genuinely superseded consent.
            let _gate = telemetry.control_gate.lock().await;
            if telemetry.lease_is_current(&report.lease) {
                continue;
            }
            let recovery = {
                let mut guard = telemetry.state.lock().await;
                guard
                    .as_mut()
                    .filter(|state| state.renewal_checkpoint_is_current(&telemetry.runtime))
                    .map(|state| telemetry.finish_credential_renewal_checkpoint(state))
            };
            if matches!(recovery, Some(Ok(()))) {
                if let Some(tx) = &telemetry.event_tx {
                    let _ = tx.try_send(());
                }
                telemetry.flush_tool_install_terminals(
                    telemetry.authorization_generation.load(Ordering::Acquire),
                );
                continue;
            }
            if let Ok(mut state) = telemetry.diagnostics.lock() {
                if let Some(pending) = state
                    .pending
                    .iter_mut()
                    .find(|p| p.payload["report_id"] == report.payload["report_id"])
                {
                    if recovery.is_some() {
                        telemetry.state_healthy.store(false, Ordering::Release);
                        telemetry.wake_control_worker();
                        pending.reason = "retry_pending".into();
                        pending.attempts = pending.attempts.saturating_add(1);
                        pending.next_attempt_at = now_ms()
                            + (5u64.saturating_mul(1u64 << pending.attempts.min(6))).min(300)
                                * 1000;
                    } else {
                        pending.status = "rejected".into();
                        pending.reason = "authorization_changed".into();
                        pending.payload["notes"] = json!("");
                        pending.payload["attachments"] = json!([]);
                    }
                }
                let _ = persist_reports(&state);
            }
            continue;
        }
        let result: Result<(bool, bool, u16, &'static str)> = async {
            let credentials = telemetry.ensure_credentials().await?;
            let gate = telemetry.control_gate.lock().await;
            anyhow::ensure!(
                telemetry.lease_is_current(&report.lease),
                "diagnostic_authorization_changed"
            );
            let endpoint = credentials
                .behavior_events_url
                .strip_suffix("behavior/events")
                .context("diagnostic_endpoint_unsupported")?
                .to_owned()
                + "diagnostics/report";
            let body = report_request_body(
                &credentials.device_id,
                &report.lease.collection_period_id,
                &report.payload,
            )?;
            let mut response = telemetry
                .client
                .post(endpoint)
                .bearer_auth(&credentials.device_token)
                .json(&body)
                .send()
                .await?;
            let status = response.status();
            if status == reqwest::StatusCode::GONE {
                let reply = bounded_reply(&mut response).await?;
                let code = reply["error"].as_str().unwrap_or("unknown");
                drop(gate);
                if code == "device_credentials_expired" {
                    let _ = telemetry.recover_expired_credentials(&credentials).await?;
                } else if code == "device_data_deleted" {
                    let _ = telemetry
                        .handle_authoritative_deletion(&credentials)
                        .await?;
                }
                anyhow::bail!("diagnostic credentials unavailable");
            }
            let received = if status.is_success() {
                // A proxy's HTML 200 or an unrelated JSON reply is not a receipt.
                let receipt = bounded_reply(&mut response).await?;
                receipt["report_id"] == report.payload["report_id"]
                    && receipt["status"] == "received"
            } else {
                false
            };
            let conflict_code = if status == reqwest::StatusCode::CONFLICT {
                let reply = bounded_reply(&mut response).await?;
                reply["error"].as_str().unwrap_or("unknown").to_owned()
            } else {
                String::new()
            };
            Ok((
                received,
                matches!(
                    conflict_code.as_str(),
                    "diagnostic_report_conflict"
                        | "collection_inactive"
                        | "diagnostic_incident_period_mismatch"
                ) || matches!(status.as_u16(), 400 | 403 | 413 | 422),
                status.as_u16(),
                match conflict_code.as_str() {
                    "collection_inactive" => "collection_inactive",
                    "diagnostic_incident_period_mismatch" => "incident_period_mismatch",
                    _ => "",
                },
            ))
        }
        .await;
        match result {
            Ok((success, permanent, _, reason)) if success || permanent => {
                if let Ok(mut state) = telemetry.diagnostics.lock() {
                    if let Some(pending) = state
                        .pending
                        .iter_mut()
                        .find(|p| p.payload["report_id"] == report.payload["report_id"])
                    {
                        pending.status = if success { "received" } else { "rejected" }.into();
                        pending.reason = if success { "" } else { reason }.into();
                        // Keep only the receipt locally once delivery is settled.
                        pending.payload["notes"] = json!("");
                        pending.payload["attachments"] = json!([]);
                    }
                    let _ = persist_reports(&state);
                }
            }
            retry => {
                if let Ok(mut state) = telemetry.diagnostics.lock() {
                    if let Some(pending) = state
                        .pending
                        .iter_mut()
                        .find(|p| p.payload["report_id"] == report.payload["report_id"])
                    {
                        pending.reason = match retry {
                            Ok((_, _, 404, _)) => "server_upgrade_required",
                            Ok((_, _, 409, _)) => "incident_pending",
                            _ => "retry_pending",
                        }
                        .into();
                        pending.attempts = pending.attempts.saturating_add(1);
                        pending.next_attempt_at = now_ms()
                            + (5u64.saturating_mul(1u64 << pending.attempts.min(6))).min(300)
                                * 1000;
                    }
                    let _ = persist_reports(&state);
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn report_size_limit_includes_the_actual_wire_envelope() {
        let empty = report_request_body("device", "period", &json!({"notes":""})).unwrap();
        let overhead = serde_json::to_vec(&empty).unwrap().len();
        let exact = json!({"notes":"x".repeat(MAX_REPORT_BYTES - overhead)});
        assert_eq!(
            serde_json::to_vec(&report_request_body("device", "period", &exact).unwrap())
                .unwrap()
                .len(),
            MAX_REPORT_BYTES
        );
        assert!(
            report_request_body(
                "device",
                "period",
                &json!({"notes":"x".repeat(MAX_REPORT_BYTES - overhead + 1)})
            )
            .is_err()
        );
        // A bare payload at the transport cap can never be a valid envelope.
        assert!(
            report_request_body(
                "device",
                "period",
                &json!({"notes":"x".repeat(MAX_REPORT_BYTES)})
            )
            .is_err()
        );
    }
    #[test]
    fn classification_and_redaction_never_retain_raw_errors() {
        assert_eq!(
            classify("HTTP 429 user-secret"),
            ("rate_limited", Some(429))
        );
        assert_eq!(
            classify("context_length_exceeded private prompt"),
            ("context_length_exceeded", None)
        );
        assert_eq!(classify("status 5001 fixture"), ("unknown", None));
        assert_eq!(
            classify("HTTP 500 fixture"),
            ("provider_unavailable", Some(500))
        );
        let cleaned = super::super::diagnostic_redaction::Redactor::new("test-only-scope").redact(
            "Authorization: Bearer secret\napi_key=secret\nC:\\Users\\private\\a.txt https://private.invalid/a\nreproduce with empty response",
        );
        assert!(!cleaned.contains("secret"));
        assert!(!cleaned.contains("private"));
        assert!(!cleaned.contains("empty response")); // Unframed multiline secrets fail closed.
        assert!(
            super::super::diagnostic_redaction::Redactor::new("test-only-scope")
                .redact("reproduce with empty response")
                .contains("empty response")
        );
    }
    #[test]
    fn public_errors_preserve_actionable_codes_without_exposing_private_causes() {
        let error = anyhow::anyhow!("diagnostic_preview_expired").context("private fixture");
        assert_eq!(
            diagnostic_error_code(&error, "diagnostic_preview_failed"),
            "diagnostic_preview_expired"
        );
        assert_eq!(
            diagnostic_error_code(
                &anyhow::anyhow!("private path or decoder detail"),
                "diagnostic_preview_failed"
            ),
            "diagnostic_preview_failed"
        );
    }
    #[test]
    fn screenshot_encoding_removes_hidden_trailing_material() {
        use xcap::image::{DynamicImage, ImageFormat, RgbaImage};
        let mut output = std::io::Cursor::new(Vec::new());
        DynamicImage::ImageRgba8(RgbaImage::new(2, 2))
            .write_to(&mut output, ImageFormat::Png)
            .unwrap();
        let mut input = output.into_inner();
        input.extend_from_slice(b"hidden-private-fixture");
        let cleaned = clean_image(&input, ImageFormat::Png).unwrap();
        assert!(
            !cleaned
                .windows(22)
                .any(|bytes| bytes == b"hidden-private-fixture")
        );
        assert!(clean_image(b"\x89PNG\r\n\x1a\nnot-an-image", ImageFormat::Png).is_err());
        let mut jpeg = std::io::Cursor::new(Vec::new());
        DynamicImage::new_rgb8(2, 2)
            .write_to(&mut jpeg, ImageFormat::Jpeg)
            .unwrap();
        let mut jpeg = jpeg.into_inner();
        jpeg.extend_from_slice(b"hidden-private-fixture");
        let cleaned = clean_image(&jpeg, ImageFormat::Jpeg).unwrap();
        assert!(cleaned.ends_with(b"\xff\xd9"));
        assert!(
            !cleaned
                .windows(22)
                .any(|bytes| bytes == b"hidden-private-fixture")
        );
    }
}
