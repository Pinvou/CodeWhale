use std::collections::{HashMap, HashSet};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex as StdMutex, RwLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use base64::Engine as _;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tauri::Manager;
use tokio::sync::{Mutex, mpsc};

use crate::platform::telemetry::{TELEMETRY_DEFAULT_BASE_URL, TELEMETRY_ENROLLMENT_TOKEN};
use crate::platform::{filesystem, paths};

const STATE_VERSION: u8 = 6;
const DEFAULT_BEHAVIOR_EVENTS_URL: &str = "https://pinvou.com/pinvou3/stats/api/behavior/events";
const CONTROLLED_PROVIDER_KEYS: &[&str] = &[
    "openai",
    "anthropic",
    "deepseek",
    "gemini",
    "qwen",
    "kimi",
    "minimax",
    "glm",
    "mimo",
    "doubao",
    "xai",
    "local",
    "custom",
    "unknown",
];
const CONTROLLED_MODEL_KEYS: &[&str] = &[
    "claude_opus",
    "claude_sonnet",
    "claude_haiku",
    "deepseek_chat",
    "deepseek_reasoner",
    "deepseek",
    "gpt_5",
    "gpt_4",
    "o3",
    "gemini",
    "qwen",
    "kimi",
    "minimax",
    "glm",
    "mimo",
    "doubao",
    "grok",
    "llama",
    "mistral",
    "custom",
    "unknown",
];
const CONTROLLED_TOOL_KEYS: &[&str] = &[
    "shell",
    "read_file",
    "edit_file",
    "search_files",
    "search",
    "web_search",
    "view_image",
    "image_generation",
    "load_skill",
    "tool_search",
    "request_user_input",
    "update_plan",
    "feishu",
    "wecom",
    "dingtalk",
    "tmeet",
    "weather",
    "iwencai",
    "ima",
    "tencent_docs",
    "wecom_bot",
    "eip",
    "zhidao",
    "qcc",
    "patsnap_search",
    "canva",
    "obsidian",
    "yuandian",
    "gongwen",
    "pptx",
    "mcp_custom",
    "custom",
    "unknown",
];
const EVENT_WAKE_CAPACITY: usize = 1;
const CONTROL_WAKE_CAPACITY: usize = 1;
const EVENT_FLUSH_MAX: usize = 50;
const EVENT_BATCH_MAX_BYTES: usize = 400 * 1024;
const EVENT_FLUSH_DEBOUNCE: Duration = Duration::from_millis(1500);
// Local retry storage expires sooner than the server-side linkable-data policy.
const OUTBOX_RETENTION_MS: u64 = 30 * 24 * 60 * 60 * 1000;
const OUTBOX_MAX_EVENTS: usize = 5_000;
const OUTBOX_MAX_BYTES: usize = 64 * 1024 * 1024;
// Exposed in settings as the service retention ceiling, not the local outbox TTL.
const SERVER_LINKABLE_RETENTION_DAYS: u8 = 35;
const RETRY_MAX_DELAY: Duration = Duration::from_secs(60);

fn client_event_field_allowed(field: &str) -> bool {
    matches!(
        field,
        "session_id"
            | "attempt_id"
            | "turn_id"
            | "operation_id"
            | "attempt_index"
            | "view_id"
            | "entry_point"
            | "mode"
            | "initiator_type"
            | "input_type"
            | "status"
            | "duration_ms"
            | "queue_duration_ms"
            | "error_category"
            | "stage"
            | "success"
            | "eligibility_source"
            | "install_source"
            | "previous_version_present"
            | "launch_type"
            | "startup_duration_ms"
            | "provider_configured"
            | "provider_key"
            | "model_key"
            | "from_model_key"
            | "to_model_key"
            | "tool_key"
            | "tool_name"
            | "step"
            | "readiness_source"
            | "has_attachment"
            | "output_type"
            | "source_turn_id"
            | "retry_reason"
            | "voice_permission_status"
            | "recognizer_type"
            | "length_bucket"
            | "tool_type"
            | "position_bucket"
            | "scene_l1"
            | "scene_l2"
            | "selection_scope"
            | "selection_source"
            | "attachment_id"
            | "file_type_bucket"
            | "size_bucket"
            | "source_type"
            | "item_count_bucket"
            | "artifact_id"
            | "artifact_type"
            | "export_format"
            | "scheduled_task_id"
            | "schedule_type"
            | "task_type"
            | "member_count_bucket"
            | "feedback_id"
            | "feedback_action"
            | "rating"
            | "reason_code"
            | "content_type"
            | "remote_stage"
            | "remote_outcome"
            | "remote_reason"
            | "remote_latency_bucket"
            | "remote_model_source"
            | "remote_size_bucket"
            | "remote_permission_step"
    )
}

fn validate_client_event_fields(event: &BehaviorEvent) -> Result<()> {
    if event.event_name == "remote_control_event" {
        return remote_control::validate_event_fields(event);
    }
    for field in event.fields.keys() {
        if field.starts_with("remote_") {
            anyhow::bail!("remote-control event field is not allowed on another event");
        }
        if !client_event_field_allowed(field) {
            anyhow::bail!("behavior event field is not allowlisted: {field}");
        }
    }
    Ok(())
}

#[derive(Clone)]
pub struct BehaviorTelemetry {
    client: reqwest::Client,
    runtime: Arc<RuntimeConfig>,
    state: Arc<Mutex<Option<PersistedState>>>,
    state_healthy: Arc<AtomicBool>,
    credential_gate: Arc<Mutex<()>>,
    control_gate: Arc<Mutex<()>>,
    collecting: Arc<AtomicBool>,
    authorization_generation: Arc<AtomicU64>,
    credential_renewal_in_progress: Arc<AtomicBool>,
    collection_period_id: Arc<RwLock<Option<String>>>,
    outbox: Arc<StdMutex<Outbox>>,
    tasks: Arc<StdMutex<HashMap<String, TaskAttempt>>>,
    team_sessions: Arc<StdMutex<HashMap<String, CollectionLease>>>,
    tool_calls: Arc<StdMutex<HashMap<String, ToolOperation>>>,
    voice_submissions: Arc<StdMutex<HashMap<String, CollectionLease>>>,
    artifacts: Arc<StdMutex<HashMap<String, ArtifactTelemetryRecord>>>,
    latest_turns: Arc<StdMutex<HashMap<String, TurnTelemetryRecord>>>,
    tool_install_terminals: ToolInstallTerminalCoordinator,
    event_tx: Option<mpsc::Sender<()>>,
    control_tx: Option<mpsc::Sender<()>>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct TaskAttempt {
    attempt_id: String,
    submitted_at: u64,
    admitted_at: Option<u64>,
    turn_id: Option<String>,
    mode: String,
    initiator_type: String,
    input_type: String,
    has_attachment: bool,
    first_output_emitted: bool,
    #[serde(default)]
    generation: u64,
    #[serde(default)]
    collection_period_id: String,
}

impl TaskAttempt {
    fn lease(&self) -> CollectionLease {
        CollectionLease {
            generation: self.generation,
            collection_period_id: self.collection_period_id.clone(),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct CollectionLease {
    generation: u64,
    collection_period_id: String,
}

#[derive(Debug, Clone)]
struct TurnTelemetryRecord {
    turn_id: String,
    lease: CollectionLease,
}

#[derive(Debug, Clone)]
struct ArtifactTelemetryRecord {
    session_id: String,
    turn_id: String,
    artifact_type: String,
    lease: CollectionLease,
}

#[derive(Debug, Clone)]
struct ToolOperation {
    operation_id: String,
    started_at: u64,
    turn_id: String,
    tool_key: &'static str,
    tool_name: String,
    tool_type: &'static str,
    lease: CollectionLease,
}

#[derive(Debug, Clone)]
struct DeferredToolInstallTerminal {
    operation_id: String,
    generation: u64,
    event: BehaviorEvent,
}

#[derive(Debug, Clone, Default)]
struct ToolInstallTerminalCoordinator {
    pending: Arc<StdMutex<HashMap<String, DeferredToolInstallTerminal>>>,
}

impl ToolInstallTerminalCoordinator {
    fn lock_pending(
        &self,
    ) -> std::sync::MutexGuard<'_, HashMap<String, DeferredToolInstallTerminal>> {
        self.pending
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    fn submit<F>(
        &self,
        terminal: DeferredToolInstallTerminal,
        authorization_generation: &AtomicU64,
        renewal_in_progress: &AtomicBool,
        mut publish: F,
    ) where
        F: FnMut(&BehaviorEvent, u64) -> bool,
    {
        let generation = terminal.generation;
        if authorization_generation.load(Ordering::Acquire) != generation {
            return;
        }
        let renewal_was_in_progress = renewal_in_progress.load(Ordering::Acquire);
        if publish(&terminal.event, generation) {
            return;
        }
        if !renewal_was_in_progress && !renewal_in_progress.load(Ordering::Acquire) {
            return;
        }
        if !renewal_in_progress.load(Ordering::Acquire) {
            let _ = publish(&terminal.event, generation);
            return;
        }
        {
            let mut pending = self.lock_pending();
            if authorization_generation.load(Ordering::Acquire) != generation
                || !renewal_in_progress.load(Ordering::Acquire)
            {
                drop(pending);
                if authorization_generation.load(Ordering::Acquire) == generation {
                    let _ = publish(&terminal.event, generation);
                }
                return;
            }
            pending
                .entry(terminal.operation_id.clone())
                .or_insert(terminal);
        }
        // Close the race where renewal publishes its new lease after the first
        // attempt but before the terminal is inserted into the deferred queue.
        self.flush_inner(
            authorization_generation,
            renewal_in_progress,
            generation,
            &mut publish,
        );
    }

    fn flush<F>(
        &self,
        authorization_generation: &AtomicU64,
        renewal_in_progress: &AtomicBool,
        generation: u64,
        mut publish: F,
    ) where
        F: FnMut(&BehaviorEvent, u64) -> bool,
    {
        self.flush_inner(
            authorization_generation,
            renewal_in_progress,
            generation,
            &mut publish,
        );
    }

    fn flush_inner<F>(
        &self,
        authorization_generation: &AtomicU64,
        renewal_in_progress: &AtomicBool,
        generation: u64,
        publish: &mut F,
    ) where
        F: FnMut(&BehaviorEvent, u64) -> bool,
    {
        self.flush_inner_with_reinsert_hook(
            authorization_generation,
            renewal_in_progress,
            generation,
            publish,
            &mut || {},
        );
    }

    fn flush_inner_with_reinsert_hook<F, H>(
        &self,
        authorization_generation: &AtomicU64,
        renewal_in_progress: &AtomicBool,
        generation: u64,
        publish: &mut F,
        before_reinsert_lock: &mut H,
    ) where
        F: FnMut(&BehaviorEvent, u64) -> bool,
        H: FnMut(),
    {
        let current_generation = authorization_generation.load(Ordering::Acquire);
        let renewal_was_in_progress = renewal_in_progress.load(Ordering::Acquire);
        let terminals = {
            let mut pending = self.lock_pending();
            pending.retain(|_, terminal| terminal.generation == current_generation);
            if current_generation != generation {
                return;
            }
            let operation_ids = pending
                .iter()
                .filter_map(|(operation_id, terminal)| {
                    (terminal.generation == generation).then(|| operation_id.clone())
                })
                .collect::<Vec<_>>();
            operation_ids
                .into_iter()
                .filter_map(|operation_id| pending.remove(&operation_id))
                .collect::<Vec<_>>()
        };

        for terminal in terminals {
            if authorization_generation.load(Ordering::Acquire) != generation {
                continue;
            }
            if !publish(&terminal.event, generation)
                && authorization_generation.load(Ordering::Acquire) == generation
            {
                if renewal_in_progress.load(Ordering::Acquire) {
                    before_reinsert_lock();
                    let mut pending = self.lock_pending();
                    if authorization_generation.load(Ordering::Acquire) == generation
                        && renewal_in_progress.load(Ordering::Acquire)
                    {
                        pending
                            .entry(terminal.operation_id.clone())
                            .or_insert(terminal);
                    }
                } else if renewal_was_in_progress {
                    let _ = publish(&terminal.event, generation);
                }
            }
        }
    }

    #[cfg(test)]
    fn flush_with_reinsert_hook<F, H>(
        &self,
        authorization_generation: &AtomicU64,
        renewal_in_progress: &AtomicBool,
        generation: u64,
        mut publish: F,
        mut before_reinsert_lock: H,
    ) where
        F: FnMut(&BehaviorEvent, u64) -> bool,
        H: FnMut(),
    {
        self.flush_inner_with_reinsert_hook(
            authorization_generation,
            renewal_in_progress,
            generation,
            &mut publish,
            &mut before_reinsert_lock,
        );
    }

    fn discard_stale(&self, generation: u64) {
        self.lock_pending()
            .retain(|_, terminal| terminal.generation == generation);
    }

    fn discard_generation(&self, generation: u64) {
        self.lock_pending()
            .retain(|_, terminal| terminal.generation != generation);
    }

    fn clear_all(&self) {
        self.lock_pending().clear();
    }

    #[cfg(test)]
    fn pending_len(&self) -> usize {
        self.lock_pending().len()
    }
}

struct CredentialRenewalTransition<'a> {
    in_progress: &'a AtomicBool,
    terminals: &'a ToolInstallTerminalCoordinator,
    generation: u64,
    completed: bool,
}

impl<'a> CredentialRenewalTransition<'a> {
    fn begin(
        in_progress: &'a AtomicBool,
        terminals: &'a ToolInstallTerminalCoordinator,
        generation: u64,
    ) -> Self {
        in_progress.store(true, Ordering::Release);
        Self {
            in_progress,
            terminals,
            generation,
            completed: false,
        }
    }

    fn complete(mut self) {
        self.in_progress.store(false, Ordering::Release);
        self.completed = true;
    }
}

impl Drop for CredentialRenewalTransition<'_> {
    fn drop(&mut self) {
        self.in_progress.store(false, Ordering::Release);
        if !self.completed {
            self.terminals.discard_generation(self.generation);
        }
    }
}

pub struct ToolInstallGuard {
    app: tauri::AppHandle,
    operation_id: String,
    started_at: u64,
    tool_key: &'static str,
    tool_name: String,
    tool_type: &'static str,
    authorization_generation: Option<u64>,
    completed: bool,
}

impl ToolInstallGuard {
    pub fn success(mut self) {
        self.complete(true, None);
    }

    pub fn failed(mut self, error: &str) {
        self.complete(false, Some(error_category(Some(error))));
    }

    fn complete(&mut self, success: bool, error_category: Option<&str>) {
        let mut event = BehaviorEvent::new("tool_install_completed")
            .tool(self.tool_key, &self.tool_name, self.tool_type)
            .string("operation_id", &self.operation_id)
            .integer("attempt_index", 1)
            .success(success)
            .integer("duration_ms", now_ms().saturating_sub(self.started_at));
        if let Some(category) = error_category {
            event = event.string("error_category", category);
        }
        if let (Some(telemetry), Some(generation)) = (
            self.app.try_state::<BehaviorTelemetry>(),
            self.authorization_generation,
        ) {
            telemetry.complete_tool_install_terminal(self.operation_id.clone(), generation, event);
        }
        self.completed = true;
    }
}

impl Drop for ToolInstallGuard {
    fn drop(&mut self) {
        if !self.completed {
            self.complete(false, Some("internal_error"));
        }
    }
}

pub struct TaskSubmissionGuard {
    app: tauri::AppHandle,
    session_id: String,
    attempt_id: String,
    reject_on_drop: bool,
}

impl TaskSubmissionGuard {
    pub fn sent(mut self) {
        self.reject_on_drop = false;
    }
}

impl Drop for TaskSubmissionGuard {
    fn drop(&mut self) {
        if self.reject_on_drop {
            reject_task_submission(
                &self.app,
                &self.session_id,
                &self.attempt_id,
                "validation_failed",
                "submission",
            );
        }
    }
}

#[derive(Debug, Clone)]
struct RuntimeConfig {
    enabled: bool,
    telemetry_base_url: String,
    behavior_events_url: String,
    enrollment_token: Option<String>,
    app_version: String,
    platform: String,
    arch: String,
    channel: String,
    locale: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct PersistedState {
    version: u8,
    telemetry_base_url: String,
    behavior_events_url: String,
    installation_id: String,
    registration_secret: String,
    device_id: Option<String>,
    device_token: Option<String>,
    #[serde(default)]
    retired_endpoints: Vec<RetiredEndpointCredentials>,
    #[serde(default)]
    control: CollectionControl,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct RetiredEndpointCredentials {
    behavior_events_url: String,
    device_id: String,
    device_token: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
struct CollectionControl {
    enabled: bool,
    /// `None` keeps the internal-preview default (collection enabled).
    /// `Some` records the user's explicit, persistent settings choice.
    user_enabled: Option<bool>,
    collection_period_id: Option<String>,
    generation: u64,
    cohort_started: bool,
    #[serde(default)]
    collection_blocked: bool,
    pending_request: Option<PendingControlRequest>,
    pending_deletions: Vec<PendingEndpointDeletion>,
    last_deletion_request_id: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct PendingEndpointDeletion {
    request_id: String,
    behavior_events_url: String,
    device_id: String,
    device_token: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
enum PendingControlKind {
    End,
    Delete,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct PendingControlRequest {
    kind: PendingControlKind,
    request_id: String,
    collection_period_id: Option<String>,
    #[serde(default)]
    behavior_events_url: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct BehaviorTelemetryStatus {
    pub configured: bool,
    pub enabled: bool,
    pub user_enabled: Option<bool>,
    pub collection_period_id: Option<String>,
    pub withdrawal_pending: bool,
    pub deletion_pending: bool,
    pub last_deletion_request_id: Option<String>,
    pub retention_days: u8,
    pub pending_event_count: usize,
    pub dropped_event_count: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct OutboxRecord {
    event_id: String,
    collection_period_id: String,
    #[serde(default)]
    generation: u64,
    enqueued_at: u64,
    #[serde(default)]
    delivery_attempt_count: u32,
    #[serde(default)]
    last_reason_code: Option<String>,
    event: Value,
}

#[derive(Debug)]
struct Outbox {
    path: PathBuf,
    records: Vec<OutboxRecord>,
    serialized_bytes: usize,
    dropped_event_count: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DeliveryStatus {
    Accepted,
    Duplicate,
    Retryable,
    Rejected,
}

#[derive(Debug, Deserialize)]
struct EventDeliveryResult {
    event_id: String,
    status: String,
    reason_code: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
struct EventUploadResponse {
    #[serde(default)]
    results: Vec<EventDeliveryResult>,
    #[serde(default)]
    accepted: Vec<String>,
    #[serde(default)]
    duplicates: Vec<String>,
    #[serde(default)]
    rejected: Vec<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct BehaviorEvent {
    pub event_id: String,
    pub event_name: &'static str,
    pub occurred_at: u64,
    #[serde(flatten)]
    fields: serde_json::Map<String, Value>,
}

impl BehaviorEvent {
    pub fn new(event_name: &'static str) -> Self {
        Self {
            event_id: event_id(),
            event_name,
            occurred_at: now_ms(),
            fields: serde_json::Map::new(),
        }
    }

    pub fn string(mut self, key: &'static str, value: impl Into<String>) -> Self {
        if let Some(value) = nonempty(value.into()) {
            self.fields.insert(key.to_string(), Value::String(value));
        }
        self
    }

    pub fn boolean(mut self, key: &'static str, value: bool) -> Self {
        self.fields.insert(key.to_string(), Value::Bool(value));
        self
    }

    pub fn integer(mut self, key: &'static str, value: u64) -> Self {
        self.fields.insert(key.to_string(), json!(value));
        self
    }

    pub fn session(mut self, session_id: impl Into<String>) -> Self {
        if let Some(value) = nonempty(session_id.into()) {
            self.fields
                .insert("session_id".to_string(), Value::String(value));
        }
        self
    }

    pub fn turn(self, turn_id: impl Into<String>) -> Self {
        self.string("turn_id", turn_id)
    }

    pub fn input_type(self, input_type: impl Into<String>) -> Self {
        self.string("input_type", input_type)
    }

    pub fn status(self, status: impl Into<String>) -> Self {
        self.string("status", status)
    }

    pub fn stage(self, stage: impl Into<String>) -> Self {
        self.string("stage", stage)
    }

    pub fn tool(
        self,
        key: impl Into<String>,
        name: impl Into<String>,
        tool_type: impl Into<String>,
    ) -> Self {
        let key = key.into();
        let name = events::normalized_tool_name(&name.into(), &key);
        self.string("tool_key", key)
            .string("tool_name", name)
            .string("tool_type", tool_type)
    }

    pub fn success(self, success: bool) -> Self {
        self.boolean("success", success)
    }

    pub fn scene(self, l1: impl Into<String>, l2: impl Into<String>) -> Self {
        self.string("scene_l1", l1).string("scene_l2", l2)
    }

    pub fn model(self, provider_key: impl Into<String>, model_key: impl Into<String>) -> Self {
        self.string("provider_key", provider_key)
            .string("model_key", model_key)
    }
}

impl Outbox {
    fn empty(path: PathBuf) -> Self {
        Self {
            path,
            records: Vec::new(),
            serialized_bytes: 0,
            dropped_event_count: 0,
        }
    }

    fn load(path: PathBuf) -> Result<Self> {
        let mut outbox = Self::empty(path.clone());
        if path.is_file() {
            let contents = std::fs::read_to_string(&path)
                .with_context(|| format!("read {}", path.display()))?;
            for line in contents.lines().filter(|line| !line.trim().is_empty()) {
                match serde_json::from_str::<OutboxRecord>(line) {
                    Ok(record) => {
                        outbox.serialized_bytes = outbox
                            .serialized_bytes
                            .saturating_add(Self::record_bytes(&record));
                        outbox.records.push(record);
                    }
                    Err(_) => outbox.dropped_event_count += 1,
                }
            }
        }
        let changed = outbox.enforce_bounds(now_ms());
        if changed {
            outbox.rewrite()?;
        }
        Ok(outbox)
    }

    fn enqueue(&mut self, record: OutboxRecord) -> Result<()> {
        let changed = self.enforce_bounds(now_ms());
        if changed {
            self.rewrite()?;
        }
        let parent = self
            .path
            .parent()
            .with_context(|| format!("invalid outbox path: {}", self.path.display()))?;
        std::fs::create_dir_all(parent).with_context(|| format!("create {}", parent.display()))?;
        let line = serde_json::to_vec(&record).context("serialize outbox record")?;
        let mut file = filesystem::open_private_append_file(&self.path)
            .with_context(|| format!("append {}", self.path.display()))?;
        file.write_all(&line)?;
        file.write_all(b"\n")?;
        file.sync_data()?;
        self.serialized_bytes = self.serialized_bytes.saturating_add(line.len() + 1);
        self.records.push(record);
        if self.enforce_bounds(now_ms()) {
            self.rewrite()?;
        }
        Ok(())
    }

    fn enforce_bounds(&mut self, now: u64) -> bool {
        let before = self.records.len();
        let cutoff = now.saturating_sub(OUTBOX_RETENTION_MS);
        self.records.retain(|record| record.enqueued_at >= cutoff);
        if self.records.len() != before {
            self.recalculate_bytes();
        }
        while self.records.len() > OUTBOX_MAX_EVENTS || self.serialized_bytes > OUTBOX_MAX_BYTES {
            self.serialized_bytes = self
                .serialized_bytes
                .saturating_sub(Self::record_bytes(&self.records[0]));
            self.records.remove(0);
        }
        let removed = before.saturating_sub(self.records.len());
        if removed > 0 {
            self.dropped_event_count = self.dropped_event_count.saturating_add(removed as u64);
        }
        removed > 0
    }

    fn record_bytes(record: &OutboxRecord) -> usize {
        serde_json::to_vec(record).map_or(0, |value| value.len() + 1)
    }

    fn recalculate_bytes(&mut self) {
        self.serialized_bytes = self.records.iter().map(Self::record_bytes).sum();
    }

    fn batch(&self) -> Vec<OutboxRecord> {
        let mut bytes = 0;
        self.records
            .iter()
            .take_while(|record| {
                let size = serde_json::to_vec(&record.event).map_or(0, |value| value.len());
                if bytes + size > EVENT_BATCH_MAX_BYTES {
                    return false;
                }
                bytes += size;
                true
            })
            .take(EVENT_FLUSH_MAX)
            .cloned()
            .collect()
    }

    fn drop_oversized_front(&mut self) -> Result<bool> {
        let oversized = self.records.first().is_some_and(|record| {
            serde_json::to_vec(&record.event)
                .map_or(true, |value| value.len() > EVENT_BATCH_MAX_BYTES)
        });
        if !oversized {
            return Ok(false);
        }
        self.records.remove(0);
        self.dropped_event_count = self.dropped_event_count.saturating_add(1);
        self.recalculate_bytes();
        self.rewrite()?;
        Ok(true)
    }

    fn apply_results(
        &mut self,
        batch: &[OutboxRecord],
        results: &HashMap<String, (DeliveryStatus, Option<String>)>,
    ) -> Result<bool> {
        let batch_ids: std::collections::HashSet<&str> = batch
            .iter()
            .map(|record| record.event_id.as_str())
            .collect();
        let mut retryable_remains = false;
        self.records.retain_mut(|record| {
            if !batch_ids.contains(record.event_id.as_str()) {
                return true;
            }
            let (status, reason) = results.get(&record.event_id).cloned().unwrap_or((
                DeliveryStatus::Retryable,
                Some("missing_result".to_string()),
            ));
            match status {
                DeliveryStatus::Accepted | DeliveryStatus::Duplicate | DeliveryStatus::Rejected => {
                    false
                }
                DeliveryStatus::Retryable => {
                    record.delivery_attempt_count = record.delivery_attempt_count.saturating_add(1);
                    record.last_reason_code = reason;
                    retryable_remains = true;
                    true
                }
            }
        });
        self.recalculate_bytes();
        self.rewrite()?;
        Ok(retryable_remains)
    }

    fn mark_retry(&mut self, batch: &[OutboxRecord], reason: &str) -> Result<()> {
        let batch_ids: std::collections::HashSet<&str> = batch
            .iter()
            .map(|record| record.event_id.as_str())
            .collect();
        for record in &mut self.records {
            if batch_ids.contains(record.event_id.as_str()) {
                record.delivery_attempt_count = record.delivery_attempt_count.saturating_add(1);
                record.last_reason_code = Some(reason.to_string());
            }
        }
        self.recalculate_bytes();
        self.rewrite()
    }

    fn clear(&mut self) -> Result<()> {
        self.records.clear();
        self.serialized_bytes = 0;
        self.rewrite()
    }

    fn retain_period(&mut self, period_id: Option<&str>, generation: u64) -> Result<()> {
        let before = self.records.len();
        self.records.retain(|record| {
            period_id.is_some_and(|period_id| {
                record.collection_period_id == period_id && record.generation == generation
            })
        });
        if self.records.len() != before {
            self.recalculate_bytes();
            self.dropped_event_count = self
                .dropped_event_count
                .saturating_add(before.saturating_sub(self.records.len()) as u64);
            self.rewrite()?;
        }
        Ok(())
    }

    fn rebind_period(&mut self, period_id: &str, generation: u64) -> Result<()> {
        for record in &mut self.records {
            record.collection_period_id = period_id.to_string();
            record.generation = generation;
            if let Some(event) = record.event.as_object_mut() {
                event.insert(
                    "collection_period_id".to_string(),
                    Value::String(period_id.to_string()),
                );
            }
        }
        self.recalculate_bytes();
        self.rewrite()
    }

    fn rewrite(&self) -> Result<()> {
        let parent = self
            .path
            .parent()
            .with_context(|| format!("invalid outbox path: {}", self.path.display()))?;
        std::fs::create_dir_all(parent).with_context(|| format!("create {}", parent.display()))?;
        let mut data = Vec::new();
        for record in &self.records {
            serde_json::to_writer(&mut data, record).context("serialize outbox record")?;
            data.push(b'\n');
        }
        filesystem::atomic_write_private(&self.path, &data)
            .with_context(|| format!("rewrite {}", self.path.display()))
    }
}

fn delivery_results(
    records: &[OutboxRecord],
    response: EventUploadResponse,
) -> HashMap<String, (DeliveryStatus, Option<String>)> {
    let mut results = HashMap::new();
    for result in response.results {
        let status = match result.status.as_str() {
            "accepted" => DeliveryStatus::Accepted,
            "duplicate" => DeliveryStatus::Duplicate,
            "rejected" => DeliveryStatus::Rejected,
            _ => DeliveryStatus::Retryable,
        };
        results.insert(result.event_id, (status, result.reason_code));
    }
    for event_id in response.accepted {
        results.insert(event_id, (DeliveryStatus::Accepted, None));
    }
    for event_id in response.duplicates {
        results.insert(event_id, (DeliveryStatus::Duplicate, None));
    }
    for event_id in response.rejected {
        results.insert(
            event_id,
            (DeliveryStatus::Rejected, Some("rejected".to_string())),
        );
    }
    for record in records {
        results.entry(record.event_id.clone()).or_insert_with(|| {
            (
                DeliveryStatus::Retryable,
                Some("missing_result".to_string()),
            )
        });
    }
    results
}

#[path = "behavior_telemetry/runtime.rs"]
mod runtime;
fn lease_matches_authorization(
    collecting: &AtomicBool,
    generation: &AtomicU64,
    period: &RwLock<Option<String>>,
    lease: &CollectionLease,
) -> bool {
    collecting.load(Ordering::Acquire)
        && generation.load(Ordering::Acquire) == lease.generation
        && period
            .read()
            .is_ok_and(|current| current.as_deref() == Some(lease.collection_period_id.as_str()))
        && collecting.load(Ordering::Acquire)
        && generation.load(Ordering::Acquire) == lease.generation
}

#[derive(Debug, Deserialize)]
struct RegistrationResponse {
    device_id: String,
    device_token: String,
}

#[derive(Debug, Deserialize)]
struct CollectionStartResponse {
    collection_period_id: String,
}

#[derive(Debug, Deserialize)]
struct CollectionEndResponse {
    #[allow(dead_code)]
    collection_period_id: String,
}

#[derive(Debug, Deserialize)]
struct DeletionResponse {
    deletion_request_id: String,
}

#[derive(Debug, Deserialize)]
struct TelemetryErrorResponse {
    error: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum GoneReason {
    DeviceCredentialsExpired,
    DeviceDataDeleted,
}

#[derive(Debug)]
struct Credentials {
    device_id: String,
    device_token: String,
    behavior_events_url: String,
}

async fn telemetry_error_code(response: reqwest::Response) -> String {
    response
        .json::<TelemetryErrorResponse>()
        .await
        .map(|body| body.error)
        .unwrap_or_else(|_| "unknown_credential_error".to_string())
}

async fn parse_control_gone(response: reqwest::Response, operation: &str) -> Result<GoneReason> {
    let body = response
        .json::<TelemetryErrorResponse>()
        .await
        .with_context(|| format!("parse behavior control {operation} 410 response"))?;
    match body.error.as_str() {
        "device_credentials_expired" => Ok(GoneReason::DeviceCredentialsExpired),
        "device_data_deleted" => Ok(GoneReason::DeviceDataDeleted),
        error => anyhow::bail!("unsupported behavior control {operation} 410 error: {error}"),
    }
}

fn deleted_credentials_match(state: &PersistedState, deleted: &Credentials) -> bool {
    state.behavior_events_url == deleted.behavior_events_url
        && state.device_id.as_deref() == Some(deleted.device_id.as_str())
        && state.device_token.as_deref() == Some(deleted.device_token.as_str())
}

fn state_after_authoritative_deletion(
    state: &PersistedState,
    runtime: &RuntimeConfig,
    deleted: &Credentials,
    generation: u64,
) -> PersistedState {
    debug_assert!(generation >= state.control.generation);
    let mut fresh = PersistedState::fresh(runtime);
    fresh.control.generation = generation;
    fresh.control.collection_blocked = true;
    fresh.control.last_deletion_request_id = state.control.last_deletion_request_id.clone();
    fresh.retired_endpoints = state
        .retired_endpoints
        .iter()
        .filter(|retired| !retired_credentials_match(retired, deleted))
        .cloned()
        .collect();
    fresh.control.pending_deletions = state
        .control
        .pending_deletions
        .iter()
        .filter(|pending| !pending_deletion_matches(pending, deleted))
        .cloned()
        .collect();
    enqueue_retired_endpoint_deletions(&mut fresh);
    fresh
}

fn state_after_authoritative_installation_deletion(
    current: &PersistedState,
    expected: &PersistedState,
    runtime: &RuntimeConfig,
    generation: u64,
) -> PersistedState {
    debug_assert!(generation >= current.control.generation);
    let mut blocked = current.clone();

    for state in [current, expected] {
        if let Some(credentials) = persisted_credentials_from_state(state) {
            if credentials.behavior_events_url != runtime.behavior_events_url
                || state.control.pending_request.is_some()
            {
                remember_retired_credentials(&mut blocked, credentials);
            }
        }
        for retired in &state.retired_endpoints {
            remember_retired_endpoint(&mut blocked, retired.clone());
        }
        blocked
            .control
            .pending_deletions
            .extend(state.control.pending_deletions.iter().cloned());
    }

    blocked.version = STATE_VERSION;
    blocked.control.enabled = false;
    blocked.control.collection_period_id = None;
    blocked.control.generation = generation;
    blocked.control.collection_blocked = true;
    if blocked.control.pending_request.is_none() {
        blocked.telemetry_base_url = runtime.telemetry_base_url.clone();
        blocked.behavior_events_url = runtime.behavior_events_url.clone();
        blocked.device_id = None;
        blocked.device_token = None;
    }
    enqueue_retired_endpoint_deletions(&mut blocked);
    blocked
}

fn remember_retired_credentials(state: &mut PersistedState, credentials: Credentials) {
    remember_retired_endpoint(
        state,
        RetiredEndpointCredentials {
            behavior_events_url: credentials.behavior_events_url,
            device_id: credentials.device_id,
            device_token: credentials.device_token,
        },
    );
}

fn remember_retired_endpoint(state: &mut PersistedState, retired: RetiredEndpointCredentials) {
    if !state.retired_endpoints.iter().any(|existing| {
        existing.behavior_events_url == retired.behavior_events_url
            && existing.device_id == retired.device_id
            && existing.device_token == retired.device_token
    }) {
        state.retired_endpoints.push(retired);
    }
}

fn converge_legacy_deletion(
    state: &mut PersistedState,
    deleted: &Credentials,
    deletion_request_id: Option<String>,
    generation: u64,
) {
    state.control.enabled = false;
    state.control.collection_period_id = None;
    state.control.generation = state.control.generation.max(generation);
    state.control.collection_blocked = true;
    state.control.pending_request = None;
    if deletion_request_id.is_some() {
        state.control.last_deletion_request_id = deletion_request_id;
    }
    state
        .retired_endpoints
        .retain(|retired| !retired_credentials_match(retired, deleted));
    state
        .control
        .pending_deletions
        .retain(|pending| !pending_deletion_matches(pending, deleted));
    if deleted_credentials_match(state, deleted) {
        state.device_id = None;
        state.device_token = None;
    }
    enqueue_retired_endpoint_deletions(state);
}

fn enqueue_retired_endpoint_deletions(state: &mut PersistedState) {
    let mut seen = HashSet::new();
    state.control.pending_deletions.retain(|pending| {
        seen.insert((
            pending.behavior_events_url.clone(),
            pending.device_id.clone(),
            pending.device_token.clone(),
        ))
    });
    for retired in &state.retired_endpoints {
        if state
            .control
            .pending_deletions
            .iter()
            .any(|pending| pending_matches_retired(pending, retired))
        {
            continue;
        }
        state
            .control
            .pending_deletions
            .push(PendingEndpointDeletion {
                request_id: token("control_", 18),
                behavior_events_url: retired.behavior_events_url.clone(),
                device_id: retired.device_id.clone(),
                device_token: retired.device_token.clone(),
            });
    }
}

fn retired_credentials_match(retired: &RetiredEndpointCredentials, deleted: &Credentials) -> bool {
    retired.behavior_events_url == deleted.behavior_events_url
        && retired.device_id == deleted.device_id
        && retired.device_token == deleted.device_token
}

fn pending_deletion_matches(pending: &PendingEndpointDeletion, deleted: &Credentials) -> bool {
    pending.behavior_events_url == deleted.behavior_events_url
        && pending.device_id == deleted.device_id
        && pending.device_token == deleted.device_token
}

fn pending_matches_retired(
    pending: &PendingEndpointDeletion,
    retired: &RetiredEndpointCredentials,
) -> bool {
    pending.behavior_events_url == retired.behavior_events_url
        && pending.device_id == retired.device_id
        && pending.device_token == retired.device_token
}

fn lease_for_generation(
    lease: Option<CollectionLease>,
    generation: u64,
) -> Option<CollectionLease> {
    lease.filter(|current| current.generation == generation)
}

fn endpoint_matches_runtime(state: &PersistedState, runtime: &RuntimeConfig) -> bool {
    state.telemetry_base_url == runtime.telemetry_base_url
        && state.behavior_events_url == runtime.behavior_events_url
}

fn persisted_credentials_from_state(state: &PersistedState) -> Option<Credentials> {
    Some(Credentials {
        device_id: state.device_id.clone()?,
        device_token: state.device_token.clone()?,
        behavior_events_url: state.behavior_events_url.clone(),
    })
}

fn pending_request_matches(state: &PersistedState, pending: &PendingControlRequest) -> bool {
    state
        .control
        .pending_request
        .as_ref()
        .is_some_and(|current| {
            current.kind == pending.kind
                && current.request_id == pending.request_id
                && (current.behavior_events_url.is_empty()
                    || current.behavior_events_url == pending.behavior_events_url)
        })
}

fn prepare_disabled_state(state: &mut PersistedState, generation: u64) {
    state.control.enabled = false;
    state.control.generation = generation;
    if state.control.pending_request.is_none() {
        if let Some(period_id) = state.control.collection_period_id.clone() {
            state.control.pending_request = Some(PendingControlRequest {
                kind: PendingControlKind::End,
                request_id: token("control_", 18),
                collection_period_id: Some(period_id),
                behavior_events_url: state.behavior_events_url.clone(),
            });
        }
    }
}

fn converge_generation_exhausted(state: &mut PersistedState) {
    if state.control.pending_request.is_none() {
        if let (Some(period_id), Some(_)) = (
            state.control.collection_period_id.clone(),
            persisted_credentials_from_state(state),
        ) {
            state.control.pending_request = Some(PendingControlRequest {
                kind: PendingControlKind::End,
                request_id: token("control_", 18),
                collection_period_id: Some(period_id),
                behavior_events_url: state.behavior_events_url.clone(),
            });
        }
    }
    state.control.enabled = false;
    state.control.collection_period_id = None;
    state.control.generation = u64::MAX;
    state.control.collection_blocked = true;
    enqueue_retired_endpoint_deletions(state);
}

fn prepare_operational_stop(state: &mut PersistedState) -> bool {
    if !state.control.enabled
        && (state.control.collection_period_id.is_none() || state.control.pending_request.is_some())
    {
        return false;
    }
    if let Some(generation) = state.control.generation.checked_add(1) {
        prepare_disabled_state(state, generation);
    } else {
        converge_generation_exhausted(state);
    }
    true
}

impl RuntimeConfig {
    fn from_env() -> Self {
        let requested_enabled = behavior_telemetry_enabled_by_config(
            std::env::var("PINVOU_BEHAVIOR_TELEMETRY_ENABLED")
                .ok()
                .as_deref(),
        );
        let telemetry_base_url = std::env::var("PINVOU_TELEMETRY_BASE_URL")
            .unwrap_or_else(|_| TELEMETRY_DEFAULT_BASE_URL.to_string())
            .trim_end_matches('/')
            .to_string();
        let behavior_events_url = std::env::var("PINVOU_BEHAVIOR_EVENTS_URL")
            .unwrap_or_else(|_| DEFAULT_BEHAVIOR_EVENTS_URL.to_string())
            .trim_end_matches('/')
            .to_string();
        let enrollment_token = match std::env::var("PINVOU_TELEMETRY_ENROLLMENT_TOKEN") {
            Ok(value) => (value.len() >= 24).then_some(value),
            Err(_) => Some(TELEMETRY_ENROLLMENT_TOKEN.to_string()),
        };
        let enabled = requested_enabled
            && !telemetry_base_url.is_empty()
            && !behavior_events_url.is_empty()
            && enrollment_token.is_some();
        Self {
            enabled,
            telemetry_base_url,
            behavior_events_url,
            enrollment_token,
            app_version: env!("CARGO_PKG_VERSION").to_string(),
            platform: std::env::consts::OS.to_string(),
            arch: match std::env::consts::ARCH {
                "x86_64" => "x86_64",
                "aarch64" => "aarch64",
                _ => "unknown",
            }
            .to_string(),
            channel: behavior_release_channel(),
            locale: behavior_locale(),
        }
    }
}

fn behavior_telemetry_enabled_by_config(value: Option<&str>) -> bool {
    !value.is_some_and(|value| {
        matches!(
            value.trim().to_ascii_lowercase().as_str(),
            "0" | "false" | "no" | "off"
        )
    })
}

fn behavior_release_channel() -> String {
    match std::env::var("PINVOU_RELEASE_CHANNEL")
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase()
        .as_str()
    {
        "stable" => "stable",
        "beta" => "beta",
        "dev" => "dev",
        _ if cfg!(debug_assertions) => "dev",
        _ => "unknown",
    }
    .to_string()
}

fn behavior_locale() -> String {
    let language = std::fs::read_to_string(paths::settings_path())
        .ok()
        .and_then(|contents| serde_json::from_str::<Value>(&contents).ok())
        .and_then(|settings| settings.get("language")?.as_str().map(str::to_string));
    match language.as_deref() {
        Some("en") => "en",
        Some("ja") => "ja",
        _ => "zh-CN",
    }
    .to_string()
}

impl PersistedState {
    fn fresh(runtime: &RuntimeConfig) -> Self {
        Self {
            version: STATE_VERSION,
            telemetry_base_url: runtime.telemetry_base_url.clone(),
            behavior_events_url: runtime.behavior_events_url.clone(),
            installation_id: token("inst_", 18),
            registration_secret: token("rs_", 32),
            device_id: None,
            device_token: None,
            retired_endpoints: Vec::new(),
            control: CollectionControl::default(),
        }
    }

    fn has_pending_control(&self) -> bool {
        self.control.pending_request.is_some() || !self.control.pending_deletions.is_empty()
    }

    fn rebind_endpoint(mut self, runtime: &RuntimeConfig) -> Self {
        if let (Some(device_id), Some(device_token)) =
            (self.device_id.take(), self.device_token.take())
        {
            let retired = RetiredEndpointCredentials {
                behavior_events_url: self.behavior_events_url.clone(),
                device_id,
                device_token,
            };
            if !self.retired_endpoints.iter().any(|existing| {
                existing.behavior_events_url == retired.behavior_events_url
                    && existing.device_id == retired.device_id
                    && existing.device_token == retired.device_token
            }) {
                self.retired_endpoints.push(retired);
            }
        }
        self.version = STATE_VERSION;
        self.telemetry_base_url = runtime.telemetry_base_url.clone();
        self.behavior_events_url = runtime.behavior_events_url.clone();
        self.control.enabled = false;
        self.control.collection_period_id = None;
        self.control.pending_request = None;
        self.control.pending_deletions.clear();
        self
    }

    fn deletion_targets(&self) -> Vec<PendingEndpointDeletion> {
        let mut targets = Vec::new();
        if let Some(credentials) = persisted_credentials_from_state(self) {
            targets.push(PendingEndpointDeletion::from_credentials(credentials));
        }
        for retired in &self.retired_endpoints {
            if !targets
                .iter()
                .any(|target| pending_matches_retired(target, retired))
            {
                targets.push(PendingEndpointDeletion {
                    request_id: token("control_", 18),
                    behavior_events_url: retired.behavior_events_url.clone(),
                    device_id: retired.device_id.clone(),
                    device_token: retired.device_token.clone(),
                });
            }
        }
        targets
    }
}

impl PendingEndpointDeletion {
    fn from_credentials(credentials: Credentials) -> Self {
        Self {
            request_id: token("control_", 18),
            behavior_events_url: credentials.behavior_events_url,
            device_id: credentials.device_id,
            device_token: credentials.device_token,
        }
    }

    fn same_request(&self, other: &Self) -> bool {
        self.request_id == other.request_id
            && self.behavior_events_url == other.behavior_events_url
            && self.device_id == other.device_id
            && self.device_token == other.device_token
    }
}

fn load_state() -> Result<Option<PersistedState>> {
    let path = state_path();
    load_state_at(&path)
}

fn load_state_at(path: &Path) -> Result<Option<PersistedState>> {
    if !path.is_file() {
        return Ok(None);
    }
    let data = std::fs::read(path).with_context(|| format!("read {}", path.display()))?;
    let mut state: PersistedState =
        serde_json::from_slice(&data).with_context(|| format!("parse {}", path.display()))?;
    if state.version > STATE_VERSION {
        anyhow::bail!(
            "unsupported behavior telemetry state version {} (maximum supported {})",
            state.version,
            STATE_VERSION
        );
    }
    state.version = STATE_VERSION;
    if state.control.generation == u64::MAX {
        converge_generation_exhausted(&mut state);
        persist_state_at(path, &state)
            .context("persist generation-exhausted behavior telemetry fail-close state")?;
    }
    Ok(Some(state))
}

fn persist_state(state: &PersistedState) -> Result<()> {
    let path = state_path();
    persist_state_at(&path, state)
}

fn persist_state_at(path: &Path, state: &PersistedState) -> Result<()> {
    let parent = path
        .parent()
        .with_context(|| format!("invalid behavior telemetry path: {}", path.display()))?;
    std::fs::create_dir_all(parent).with_context(|| format!("create {}", parent.display()))?;
    let data = serde_json::to_vec_pretty(state).context("serialize behavior telemetry state")?;
    filesystem::atomic_write_private(path, &data)
        .with_context(|| format!("write {}", path.display()))?;
    Ok(())
}

fn state_path() -> PathBuf {
    paths::pinvou3_home().join("behavior-telemetry.json")
}

fn outbox_path() -> PathBuf {
    paths::pinvou3_home().join("behavior-telemetry-outbox.jsonl")
}

fn task_attempts_path() -> PathBuf {
    paths::pinvou3_home().join("behavior-telemetry-tasks.json")
}

fn load_task_attempts() -> HashMap<String, TaskAttempt> {
    std::fs::read(task_attempts_path())
        .ok()
        .and_then(|data| serde_json::from_slice(&data).ok())
        .unwrap_or_default()
}

fn persist_task_attempts(tasks: &HashMap<String, TaskAttempt>) -> Result<()> {
    let data = serde_json::to_vec(tasks).context("serialize behavior task attempts")?;
    filesystem::atomic_write_private(&task_attempts_path(), &data)
        .context("persist behavior task attempts")
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

fn event_id() -> String {
    token("beh_", 18)
}

fn token(prefix: &str, bytes: usize) -> String {
    let mut raw = vec![0_u8; bytes];
    use rand::Rng as _;
    rand::rng().fill_bytes(&mut raw);
    format!(
        "{}{}",
        prefix,
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(raw)
    )
}

fn nonempty(value: String) -> Option<String> {
    let trimmed = value.trim();
    (!trimmed.is_empty()).then(|| trimmed.chars().take(128).collect())
}

#[path = "behavior_telemetry/events.rs"]
mod events;

#[path = "behavior_telemetry/remote_control.rs"]
mod remote_control;

pub use events::*;
pub(crate) use remote_control::{
    RemoteControlEvent, RemoteModelSource, RemoteOutcome, RemotePermissionStep, RemoteReason,
    RemoteStage, track_remote_control, track_remote_control_with_event_id,
};

#[cfg(all(test, feature = "behavior-telemetry-test"))]
mod tests {
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/src/features/behavior_telemetry_tests.rs"
    ));
}
