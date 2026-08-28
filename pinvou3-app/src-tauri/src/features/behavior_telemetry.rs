use std::collections::{HashMap, HashSet};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex as StdMutex, RwLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use base64::Engine as _;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use tauri::Manager;
use tokio::sync::{mpsc, Mutex};

use crate::platform::{filesystem, paths};

const STATE_VERSION: u8 = 6;
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
const OUTBOX_RETENTION_MS: u64 = 30 * 24 * 60 * 60 * 1000;
const OUTBOX_MAX_EVENTS: usize = 5_000;
const OUTBOX_MAX_BYTES: usize = 64 * 1024 * 1024;
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
    )
}

fn validate_client_event_fields(event: &BehaviorEvent) -> Result<()> {
    for field in event.fields.keys() {
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
        let mut terminal = Some(terminal);
        {
            let mut pending = self.lock_pending();
            if authorization_generation.load(Ordering::Acquire) != generation
                || !renewal_in_progress.load(Ordering::Acquire)
            {
                drop(pending);
                if authorization_generation.load(Ordering::Acquire) == generation {
                    let terminal = terminal.as_ref().expect("terminal remains available");
                    let _ = publish(&terminal.event, generation);
                }
                return;
            }
            let terminal = terminal.take().expect("terminal inserted once");
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
            .tool(self.tool_key, self.tool_key, self.tool_type)
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
        _name: impl Into<String>,
        tool_type: impl Into<String>,
    ) -> Self {
        self.string("tool_key", key).string("tool_type", tool_type)
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
        filesystem::atomic_write(&self.path, &data)
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

impl BehaviorTelemetry {
    pub fn new() -> Self {
        let runtime = Arc::new(RuntimeConfig::from_env());
        let (mut persisted, mut state_healthy) = match load_state() {
            Ok(state) => (state, true),
            Err(error) => {
                log::error!(
                    "[pinvou3][behavior] telemetry state is unreadable; collection is disabled to preserve identity and pending controls: {error:#}"
                );
                (None, false)
            }
        };
        if !runtime.enabled {
            if let Some(state) = persisted.as_mut() {
                if prepare_operational_stop(state) {
                    if let Err(error) = persist_state(state) {
                        log::error!(
                            "[pinvou3][behavior] failed to persist operational telemetry stop: {error:#}"
                        );
                        state_healthy = false;
                    }
                }
            }
        }
        let active_period = persisted.as_ref().and_then(|state| {
            (runtime.enabled
                && state.telemetry_base_url == runtime.telemetry_base_url
                && state.behavior_events_url == runtime.behavior_events_url
                && state.control.enabled
                && !state.control.collection_blocked)
                .then(|| state.control.collection_period_id.clone())
                .flatten()
        });
        let active_generation = persisted
            .as_ref()
            .map_or(0, |state| state.control.generation);
        let collecting = Arc::new(AtomicBool::new(
            runtime.enabled && state_healthy && active_period.is_some(),
        ));
        let collection_period_id = Arc::new(RwLock::new(active_period.clone()));
        let mut loaded_outbox = Outbox::load(outbox_path()).unwrap_or_else(|error| {
            log::debug!("[pinvou3][behavior] failed to load outbox: {error:#}");
            Outbox::empty(outbox_path())
        });
        if let Err(error) = loaded_outbox.retain_period(active_period.as_deref(), active_generation)
        {
            log::debug!("[pinvou3][behavior] failed to sanitize outbox period: {error:#}");
        }
        let outbox = Arc::new(StdMutex::new(loaded_outbox));
        let recovered_tasks = load_task_attempts()
            .into_iter()
            .filter(|(_, attempt)| {
                active_period.as_deref() == Some(attempt.collection_period_id.as_str())
                    && attempt.generation == active_generation
            })
            .collect::<HashMap<_, _>>();
        let tasks = Arc::new(StdMutex::new(recovered_tasks.clone()));
        let state = Arc::new(Mutex::new(persisted));
        let state_healthy = Arc::new(AtomicBool::new(state_healthy));
        let credential_gate = Arc::new(Mutex::new(()));
        let control_gate = Arc::new(Mutex::new(()));
        let (event_tx, event_rx) = if runtime.enabled {
            let (tx, rx) = mpsc::channel(EVENT_WAKE_CAPACITY);
            (Some(tx), Some(rx))
        } else {
            (None, None)
        };
        let (control_tx, control_rx) = if state_healthy.load(Ordering::Acquire) {
            let (tx, rx) = mpsc::channel(CONTROL_WAKE_CAPACITY);
            (Some(tx), Some(rx))
        } else {
            (None, None)
        };
        let telemetry = Self {
            client: reqwest::Client::builder()
                .connect_timeout(Duration::from_secs(5))
                .timeout(Duration::from_secs(12))
                .build()
                .unwrap_or_else(|_| reqwest::Client::new()),
            runtime,
            state,
            state_healthy,
            credential_gate,
            control_gate,
            collecting,
            authorization_generation: Arc::new(AtomicU64::new(active_generation)),
            credential_renewal_in_progress: Arc::new(AtomicBool::new(false)),
            collection_period_id,
            outbox,
            tasks,
            team_sessions: Arc::new(StdMutex::new(HashMap::new())),
            tool_calls: Arc::new(StdMutex::new(HashMap::new())),
            voice_submissions: Arc::new(StdMutex::new(HashMap::new())),
            artifacts: Arc::new(StdMutex::new(HashMap::new())),
            latest_turns: Arc::new(StdMutex::new(HashMap::new())),
            tool_install_terminals: ToolInstallTerminalCoordinator::default(),
            event_tx,
            control_tx,
        };
        if let Some(rx) = event_rx {
            tauri::async_runtime::spawn(Self::run_worker(rx, telemetry.worker_clone()));
            if telemetry
                .outbox
                .lock()
                .is_ok_and(|outbox| !outbox.records.is_empty())
            {
                if let Some(tx) = &telemetry.event_tx {
                    let _ = tx.try_send(());
                }
            }
        }
        if let Some(rx) = control_rx {
            let mut control_worker = telemetry.worker_clone();
            control_worker.control_tx = None;
            tauri::async_runtime::spawn(Self::run_control_worker(rx, control_worker));
            if telemetry.has_pending_control_sync() {
                telemetry.wake_control_worker();
            }
        }
        if telemetry.runtime.enabled && telemetry.event_tx.is_none() {
            log::debug!("[pinvou3][behavior] telemetry disabled: queue initialization failed");
        }
        if telemetry.collecting.load(Ordering::Acquire) {
            telemetry.track(
                BehaviorEvent::new("app_opened")
                    .string("launch_type", "cold")
                    .integer(
                        "startup_duration_ms",
                        crate::platform::startup::elapsed_ms().min(600_000),
                    ),
            );
        }
        for (session_id, attempt) in recovered_tasks {
            if let Some(turn_id) = attempt.turn_id.as_deref() {
                telemetry.track(
                    task_fields(BehaviorEvent::new("task_finished"), &session_id, &attempt)
                        .turn(turn_id)
                        .status("interrupted")
                        .stage("shutdown")
                        .integer("duration_ms", now_ms().saturating_sub(attempt.submitted_at))
                        .string("error_category", "internal_error"),
                );
            } else {
                telemetry.track(
                    task_fields(BehaviorEvent::new("task_rejected"), &session_id, &attempt)
                        .string("error_category", "internal_error")
                        .stage("shutdown"),
                );
            }
        }
        if let Ok(mut tasks) = telemetry.tasks.lock() {
            tasks.clear();
            let _ = persist_task_attempts(&tasks);
        }
        telemetry
    }

    pub fn start_default_collection(&self) {
        if !self.runtime.enabled || self.collecting.load(Ordering::Acquire) {
            return;
        }
        let telemetry = self.clone();
        tauri::async_runtime::spawn(async move {
            let mut retry_delay = Duration::from_secs(1);
            loop {
                if !telemetry.state_healthy.load(Ordering::Acquire)
                    || telemetry.collection_is_blocked().await
                {
                    return;
                }
                match telemetry.set_collection_enabled(true).await {
                    Ok(_) => {
                        telemetry.track(
                            BehaviorEvent::new("app_opened")
                                .string("launch_type", "cold")
                                .integer(
                                    "startup_duration_ms",
                                    crate::platform::startup::elapsed_ms().min(600_000),
                                ),
                        );
                        return;
                    }
                    Err(error) => {
                        log::debug!(
                            "[pinvou3][behavior] default collection start deferred: {error:#}"
                        );
                        if !telemetry.state_healthy.load(Ordering::Acquire)
                            || telemetry.collection_is_blocked().await
                        {
                            return;
                        }
                        let jitter = Duration::from_millis(now_ms() % 251);
                        tokio::time::sleep(retry_delay.saturating_add(jitter)).await;
                        retry_delay = retry_delay.saturating_mul(2).min(RETRY_MAX_DELAY);
                    }
                }
            }
        });
    }

    async fn collection_is_blocked(&self) -> bool {
        self.state
            .lock()
            .await
            .as_ref()
            .is_some_and(|state| state.control.collection_blocked)
    }

    pub fn track(&self, event: BehaviorEvent) {
        let Some(lease) = self.current_lease() else {
            return;
        };
        self.track_for_lease(event, &lease);
    }

    fn track_for_lease(&self, event: BehaviorEvent, lease: &CollectionLease) {
        let _ = self.try_track_for_lease(event, lease);
    }

    fn try_track_for_lease(&self, event: BehaviorEvent, lease: &CollectionLease) -> bool {
        let record = match self.prepare_record(event, lease) {
            Ok(record) => record,
            Err(error) => {
                log::debug!("[pinvou3][behavior] event serialization skipped: {error:#}");
                return false;
            }
        };
        let enqueued = self
            .outbox
            .lock()
            .map_err(|_| anyhow::anyhow!("behavior outbox lock poisoned"))
            .and_then(|mut outbox| {
                if !self.lease_is_current(lease) {
                    return Ok(false);
                }
                outbox.enqueue(record)?;
                Ok(true)
            });
        match enqueued {
            Ok(true) => {}
            Ok(false) => return false,
            Err(error) => {
                log::debug!("[pinvou3][behavior] persistent enqueue skipped: {error:#}");
                return false;
            }
        }
        if let Some(tx) = &self.event_tx {
            let _ = tx.try_send(());
        }
        true
    }

    fn current_lease(&self) -> Option<CollectionLease> {
        let generation = self.authorization_generation.load(Ordering::Acquire);
        if !self.collecting.load(Ordering::Acquire) {
            return None;
        }
        let collection_period_id = self.collection_period_id.read().ok()?.clone()?;
        let lease = CollectionLease {
            generation,
            collection_period_id,
        };
        self.lease_is_current(&lease).then_some(lease)
    }

    fn lease_is_current(&self, lease: &CollectionLease) -> bool {
        lease_matches_authorization(
            &self.collecting,
            &self.authorization_generation,
            &self.collection_period_id,
            lease,
        )
    }

    fn invalidate_generation_above(&self, floor: u64) -> Result<u64> {
        let mut observed = self.authorization_generation.load(Ordering::Acquire);
        loop {
            let generation = observed
                .max(floor)
                .checked_add(1)
                .context("behavior telemetry generation is exhausted")?;
            match self.authorization_generation.compare_exchange_weak(
                observed,
                generation,
                Ordering::AcqRel,
                Ordering::Acquire,
            ) {
                Ok(_) => {
                    self.tool_install_terminals.discard_stale(generation);
                    return Ok(generation);
                }
                Err(actual) => observed = actual,
            }
        }
    }

    fn enter_generation_exhausted_fail_close(&self) {
        self.authorization_generation
            .store(u64::MAX, Ordering::Release);
        self.collecting.store(false, Ordering::Release);
        self.credential_renewal_in_progress
            .store(false, Ordering::Release);
        self.tool_install_terminals.clear_all();
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
    }

    fn finish_generation_exhausted<T>(
        &self,
        context: &str,
        mut failures: Vec<String>,
    ) -> Result<T> {
        failures.extend(self.clear_local_transient_state());
        self.wake_control_worker();
        if failures.is_empty() {
            anyhow::bail!(
                "{context}: behavior telemetry generation exhausted; collection permanently blocked"
            );
        }
        anyhow::bail!(
            "{context}: behavior telemetry generation exhausted; fail-close cleanup incomplete: {}",
            failures.join("; ")
        )
    }

    fn complete_tool_install_terminal(
        &self,
        operation_id: String,
        generation: u64,
        event: BehaviorEvent,
    ) {
        self.tool_install_terminals.submit(
            DeferredToolInstallTerminal {
                operation_id,
                generation,
                event,
            },
            &self.authorization_generation,
            &self.credential_renewal_in_progress,
            |event, expected_generation| {
                let Some(lease) = lease_for_generation(self.current_lease(), expected_generation)
                else {
                    return false;
                };
                self.try_track_for_lease(event.clone(), &lease)
            },
        );
    }

    fn flush_tool_install_terminals(&self, generation: u64) {
        self.tool_install_terminals.flush(
            &self.authorization_generation,
            &self.credential_renewal_in_progress,
            generation,
            |event, expected_generation| {
                let Some(lease) = lease_for_generation(self.current_lease(), expected_generation)
                else {
                    return false;
                };
                self.try_track_for_lease(event.clone(), &lease)
            },
        );
    }

    fn rebind_lifecycle_state(&self, generation: u64, period_id: &str) -> Result<()> {
        let rebind = |lease: &mut CollectionLease| {
            if lease.generation == generation {
                lease.collection_period_id = period_id.to_string();
            }
        };
        let mut tasks = self
            .tasks
            .lock()
            .map_err(|_| anyhow::anyhow!("behavior task state lock poisoned"))?;
        for attempt in tasks.values_mut() {
            if attempt.generation == generation {
                attempt.collection_period_id = period_id.to_string();
            }
        }
        persist_task_attempts(&tasks)?;
        drop(tasks);
        if let Ok(mut sessions) = self.team_sessions.lock() {
            sessions.values_mut().for_each(rebind);
        }
        if let Ok(mut calls) = self.tool_calls.lock() {
            calls
                .values_mut()
                .for_each(|operation| rebind(&mut operation.lease));
        }
        if let Ok(mut submissions) = self.voice_submissions.lock() {
            submissions.values_mut().for_each(rebind);
        }
        if let Ok(mut artifacts) = self.artifacts.lock() {
            artifacts
                .values_mut()
                .for_each(|record| rebind(&mut record.lease));
        }
        if let Ok(mut turns) = self.latest_turns.lock() {
            turns
                .values_mut()
                .for_each(|record| rebind(&mut record.lease));
        }
        Ok(())
    }

    fn prepare_record(
        &self,
        event: BehaviorEvent,
        lease: &CollectionLease,
    ) -> Result<OutboxRecord> {
        validate_client_event_fields(&event)?;
        let event_id = event.event_id.clone();
        let mut value = serde_json::to_value(event).context("serialize behavior event")?;
        let object = value
            .as_object_mut()
            .context("behavior event must serialize as an object")?;
        object.insert("schema_version".to_string(), json!(1));
        object.insert("event_source".to_string(), json!("client"));
        object.insert(
            "collection_period_id".to_string(),
            json!(lease.collection_period_id.clone()),
        );
        object.insert("producer".to_string(), json!("pinvou_agent"));
        object.insert(
            "producer_version".to_string(),
            json!(self.runtime.app_version.clone()),
        );
        object.insert(
            "app_version".to_string(),
            json!(self.runtime.app_version.clone()),
        );
        object.insert("platform".to_string(), json!(self.runtime.platform.clone()));
        object.insert("arch".to_string(), json!(self.runtime.arch.clone()));
        object.insert("channel".to_string(), json!(self.runtime.channel.clone()));
        object.insert("locale".to_string(), json!(self.runtime.locale.clone()));
        Ok(OutboxRecord {
            event_id,
            collection_period_id: lease.collection_period_id.clone(),
            generation: lease.generation,
            enqueued_at: now_ms(),
            delivery_attempt_count: 0,
            last_reason_code: None,
            event: value,
        })
    }

    fn worker_clone(&self) -> Self {
        Self {
            client: reqwest::Client::builder()
                .connect_timeout(Duration::from_secs(5))
                .timeout(Duration::from_secs(12))
                .build()
                .unwrap_or_else(|_| reqwest::Client::new()),
            runtime: self.runtime.clone(),
            state: self.state.clone(),
            state_healthy: self.state_healthy.clone(),
            credential_gate: self.credential_gate.clone(),
            control_gate: self.control_gate.clone(),
            collecting: self.collecting.clone(),
            authorization_generation: self.authorization_generation.clone(),
            credential_renewal_in_progress: self.credential_renewal_in_progress.clone(),
            collection_period_id: self.collection_period_id.clone(),
            outbox: self.outbox.clone(),
            tasks: self.tasks.clone(),
            team_sessions: self.team_sessions.clone(),
            tool_calls: self.tool_calls.clone(),
            voice_submissions: self.voice_submissions.clone(),
            artifacts: self.artifacts.clone(),
            latest_turns: self.latest_turns.clone(),
            tool_install_terminals: self.tool_install_terminals.clone(),
            event_tx: None,
            control_tx: self.control_tx.clone(),
        }
    }

    fn has_pending_control_sync(&self) -> bool {
        self.state
            .try_lock()
            .ok()
            .and_then(|state| state.as_ref().map(PersistedState::has_pending_control))
            .unwrap_or(false)
    }

    async fn has_pending_control(&self) -> bool {
        self.state
            .lock()
            .await
            .as_ref()
            .is_some_and(PersistedState::has_pending_control)
    }

    fn wake_control_worker(&self) {
        if let Some(tx) = &self.control_tx {
            let _ = tx.try_send(());
        }
    }

    async fn run_control_worker(mut rx: mpsc::Receiver<()>, telemetry: Self) {
        while rx.recv().await.is_some() {
            let mut retry_delay = Duration::from_secs(1);
            while telemetry.has_pending_control().await {
                match telemetry.retry_pending_control().await {
                    Ok(()) => retry_delay = Duration::from_secs(1),
                    Err(error) => {
                        log::debug!(
                            "[pinvou3][behavior] pending control retry deferred: {error:#}"
                        );
                        let jitter = Duration::from_millis(now_ms() % 251);
                        let wait = retry_delay.saturating_add(jitter);
                        retry_delay = retry_delay.saturating_mul(2).min(RETRY_MAX_DELAY);
                        tokio::select! {
                            _ = tokio::time::sleep(wait) => {}
                            signal = rx.recv() => {
                                if signal.is_none() {
                                    return;
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    async fn run_worker(mut rx: mpsc::Receiver<()>, telemetry: Self) {
        let mut retry_delay = Duration::from_secs(1);
        loop {
            let has_events = telemetry
                .outbox
                .lock()
                .is_ok_and(|outbox| !outbox.records.is_empty());
            if !has_events || !telemetry.collecting.load(Ordering::Acquire) {
                if rx.recv().await.is_none() {
                    break;
                }
                let _ = tokio::time::timeout(EVENT_FLUSH_DEBOUNCE, rx.recv()).await;
                continue;
            }
            let batch = telemetry
                .outbox
                .lock()
                .map(|outbox| outbox.batch())
                .unwrap_or_default();
            if batch.is_empty() {
                continue;
            }
            match telemetry.send_events(&batch).await {
                Ok(results) => {
                    let retryable_remains = telemetry.outbox.lock().map_or(true, |mut outbox| {
                        outbox
                            .apply_results(&batch, &results)
                            .unwrap_or_else(|error| {
                                log::debug!(
                                "[pinvou3][behavior] persist delivery results failed: {error:#}"
                            );
                                true
                            })
                    });
                    if retryable_remains {
                        let jitter = Duration::from_millis(now_ms() % 251);
                        tokio::time::sleep(retry_delay.saturating_add(jitter)).await;
                        retry_delay = retry_delay.saturating_mul(2).min(RETRY_MAX_DELAY);
                    } else {
                        retry_delay = Duration::from_secs(1);
                    }
                }
                Err(error) => {
                    log::debug!("[pinvou3][behavior] upload deferred: {error:#}");
                    if let Ok(mut outbox) = telemetry.outbox.lock() {
                        let _ = outbox.mark_retry(&batch, "network_unavailable");
                    }
                    let jitter = Duration::from_millis(now_ms() % 251);
                    tokio::time::sleep(retry_delay.saturating_add(jitter)).await;
                    retry_delay = retry_delay.saturating_mul(2).min(RETRY_MAX_DELAY);
                }
            }
        }
    }

    async fn send_events(
        &self,
        records: &[OutboxRecord],
    ) -> Result<HashMap<String, (DeliveryStatus, Option<String>)>> {
        let events: Vec<&Value> = records.iter().map(|record| &record.event).collect();
        if events.is_empty() {
            return Ok(HashMap::new());
        }
        let credentials = self.ensure_credentials().await?;
        let body = json!({
            "batch_id": token("batch_", 18),
            "device_id": credentials.device_id.clone(),
            "events": events,
        });
        let response = self
            .client
            .post(&credentials.behavior_events_url)
            .bearer_auth(&credentials.device_token)
            .json(&body)
            .send()
            .await
            .context("send behavior events")?;
        if response.status() == reqwest::StatusCode::GONE {
            let error_code = telemetry_error_code(response).await;
            if error_code == "device_credentials_expired" {
                let _ = self.recover_expired_credentials(&credentials).await?;
                anyhow::bail!("behavior telemetry credentials expired and were renewed");
            }
            if error_code == "device_data_deleted" {
                let _ = self.handle_authoritative_deletion(&credentials).await?;
                anyhow::bail!("behavior telemetry data was deleted; local collection disabled");
            }
            anyhow::bail!("behavior telemetry credentials unavailable: {error_code}");
        }
        if !response.status().is_success() {
            anyhow::bail!("behavior events endpoint returned {}", response.status());
        }
        let response: EventUploadResponse = response
            .json()
            .await
            .context("parse behavior event delivery response")?;
        Ok(delivery_results(records, response))
    }

    async fn recover_expired_credentials(&self, expired: &Credentials) -> Result<bool> {
        let _control_guard = self.control_gate.lock().await;
        self.recover_expired_credentials_locked(expired).await
    }

    async fn handle_authoritative_deletion(&self, deleted: &Credentials) -> Result<bool> {
        let _control_guard = self.control_gate.lock().await;
        self.handle_authoritative_deletion_locked(deleted).await
    }

    async fn handle_authoritative_deletion_locked(&self, deleted: &Credentials) -> Result<bool> {
        let mut guard = self.state.lock().await;
        let Some(state) = guard.as_ref() else {
            return Ok(false);
        };
        if !deleted_credentials_match(state, deleted) {
            return Ok(false);
        }

        let (generation, generation_exhausted) =
            match self.invalidate_generation_above(state.control.generation) {
                Ok(generation) => (generation, false),
                Err(_) => {
                    self.enter_generation_exhausted_fail_close();
                    (u64::MAX, true)
                }
            };
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
        let fresh = state_after_authoritative_deletion(state, &self.runtime, deleted, generation);

        let mut failures = Vec::new();
        if let Err(error) = persist_state(&fresh) {
            self.state_healthy.store(false, Ordering::Release);
            failures.push(format!("persist disabled telemetry state: {error:#}"));
        }
        *guard = Some(fresh);
        drop(guard);

        if generation_exhausted {
            return self.finish_generation_exhausted("authoritative device deletion", failures);
        }
        failures.extend(self.clear_local_transient_state());
        self.wake_control_worker();

        if failures.is_empty() {
            Ok(true)
        } else {
            anyhow::bail!(
                "authoritative deletion cleanup incomplete: {}",
                failures.join("; ")
            )
        }
    }

    async fn handle_authoritative_installation_deletion_locked(
        &self,
        expected: &PersistedState,
    ) -> Result<bool> {
        let mut guard = self.state.lock().await;
        let Some(current) = guard.as_ref() else {
            return Ok(false);
        };
        if current.installation_id != expected.installation_id
            || current.registration_secret != expected.registration_secret
        {
            return Ok(false);
        }

        let (generation, generation_exhausted) =
            match self.invalidate_generation_above(current.control.generation) {
                Ok(generation) => (generation, false),
                Err(_) => {
                    self.enter_generation_exhausted_fail_close();
                    (u64::MAX, true)
                }
            };
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
        let blocked = state_after_authoritative_installation_deletion(
            current,
            expected,
            &self.runtime,
            generation,
        );
        let mut failures = Vec::new();
        if let Err(error) = persist_state(&blocked) {
            self.state_healthy.store(false, Ordering::Release);
            failures.push(format!(
                "persist authoritative installation deletion: {error:#}"
            ));
        }
        *guard = Some(blocked);
        drop(guard);

        if generation_exhausted {
            return self
                .finish_generation_exhausted("authoritative installation deletion", failures);
        }
        failures.extend(self.clear_local_transient_state());
        self.wake_control_worker();
        if failures.is_empty() {
            Ok(true)
        } else {
            anyhow::bail!(
                "authoritative installation deletion cleanup incomplete: {}",
                failures.join("; ")
            )
        }
    }

    fn clear_local_transient_state(&self) -> Vec<String> {
        let mut failures = Vec::new();
        match self.outbox.lock() {
            Ok(mut outbox) => {
                if let Err(error) = outbox.clear() {
                    failures.push(format!("clear outbox: {error:#}"));
                }
            }
            Err(_) => failures.push("clear outbox: lock poisoned".to_string()),
        }
        match self.tasks.lock() {
            Ok(mut tasks) => {
                tasks.clear();
                if let Err(error) = persist_task_attempts(&tasks) {
                    failures.push(format!("clear task attempts: {error:#}"));
                }
            }
            Err(_) => failures.push("clear task attempts: lock poisoned".to_string()),
        }
        if let Ok(mut sessions) = self.team_sessions.lock() {
            sessions.clear();
        }
        if let Ok(mut calls) = self.tool_calls.lock() {
            calls.clear();
        }
        if let Ok(mut submissions) = self.voice_submissions.lock() {
            submissions.clear();
        }
        if let Ok(mut artifacts) = self.artifacts.lock() {
            artifacts.clear();
        }
        if let Ok(mut latest_turns) = self.latest_turns.lock() {
            latest_turns.clear();
        }
        self.tool_install_terminals.clear_all();
        failures
    }

    async fn recover_expired_credentials_locked(&self, expired: &Credentials) -> Result<bool> {
        let snapshot = self
            .state
            .lock()
            .await
            .clone()
            .context("behavior telemetry state missing during credential renewal")?;
        if !endpoint_matches_runtime(&snapshot, &self.runtime)
            || snapshot.has_pending_control()
            || snapshot.device_id.as_deref() != Some(expired.device_id.as_str())
            || snapshot.device_token.as_deref() != Some(expired.device_token.as_str())
        {
            return Ok(false);
        }
        let enrollment_token = self
            .runtime
            .enrollment_token
            .as_deref()
            .context("PINVOU_TELEMETRY_ENROLLMENT_TOKEN is not configured")?;
        let response = self
            .client
            .post(format!(
                "{}/v1/register",
                snapshot.telemetry_base_url.trim_end_matches('/')
            ))
            .json(&json!({
                "enrollment_token": enrollment_token,
                "hardware_claim": format!("pinvou3-installation:{}", snapshot.installation_id),
                "registration_secret": snapshot.registration_secret,
                "hardware_source": "installation_id",
                "identity_quality": "installation_only",
                "app_version": self.runtime.app_version,
                "platform": self.runtime.platform,
                "arch": self.runtime.arch,
            }))
            .send()
            .await
            .context("renew expired behavior telemetry registration")?;
        if response.status() == reqwest::StatusCode::GONE {
            let error_code = telemetry_error_code(response).await;
            if error_code == "installation_data_deleted" {
                let handled = self
                    .handle_authoritative_installation_deletion_locked(&snapshot)
                    .await?;
                if !handled {
                    self.fail_close_unmatched_authoritative_deletion();
                }
                anyhow::bail!(
                    "behavior telemetry installation data was deleted; local collection disabled"
                );
            }
            anyhow::bail!("telemetry registration renewal unavailable: {error_code}");
        }
        if !response.status().is_success() {
            anyhow::bail!(
                "telemetry registration renewal returned {}",
                response.status()
            );
        }
        let registered: RegistrationResponse = response
            .json()
            .await
            .context("parse renewed telemetry registration response")?;
        let renewed = Credentials {
            device_id: registered.device_id,
            device_token: registered.device_token,
            behavior_events_url: self.runtime.behavior_events_url.clone(),
        };
        let request_id = token("control_", 18);
        let started_response = self
            .request_control(
                &renewed,
                "collection/start",
                json!({ "request_id": request_id }),
            )
            .await?;
        if started_response.status() == reqwest::StatusCode::GONE {
            match parse_control_gone(started_response, "renewed collection/start").await? {
                GoneReason::DeviceCredentialsExpired => {
                    anyhow::bail!("renewed behavior credentials expired before collection start");
                }
                GoneReason::DeviceDataDeleted => {
                    let handled = self
                        .handle_authoritative_installation_deletion_locked(&snapshot)
                        .await?;
                    if !handled {
                        self.fail_close_unmatched_authoritative_deletion();
                    }
                    anyhow::bail!(
                        "renewed behavior device data was deleted before collection start"
                    );
                }
            }
        }
        if !started_response.status().is_success() {
            anyhow::bail!(
                "renewed behavior collection start returned {}",
                started_response.status()
            );
        }
        let started: CollectionStartResponse = started_response
            .json()
            .await
            .context("parse renewed behavior collection response")?;

        let mut guard = self.state.lock().await;
        let state = guard
            .as_mut()
            .context("behavior telemetry state disappeared during credential renewal")?;
        if state.device_id.as_deref() != Some(expired.device_id.as_str())
            || state.device_token.as_deref() != Some(expired.device_token.as_str())
            || state.control.generation != snapshot.control.generation
            || state.control.collection_blocked
            || state.has_pending_control()
        {
            return Ok(false);
        }
        let generation = self.authorization_generation.load(Ordering::Acquire);
        let renewal_transition = CredentialRenewalTransition::begin(
            &self.credential_renewal_in_progress,
            &self.tool_install_terminals,
            generation,
        );
        self.collecting.store(false, Ordering::Release);
        self.outbox
            .lock()
            .map_err(|_| anyhow::anyhow!("behavior outbox lock poisoned"))?
            .rebind_period(&started.collection_period_id, generation)?;
        self.rebind_lifecycle_state(generation, &started.collection_period_id)?;
        state.device_id = Some(renewed.device_id);
        state.device_token = Some(renewed.device_token);
        state.control.enabled = true;
        state.control.collection_period_id = Some(started.collection_period_id.clone());
        state.control.generation = generation;
        state.control.pending_request = None;
        persist_state(state)?;
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = Some(started.collection_period_id);
        }
        self.collecting.store(true, Ordering::Release);
        renewal_transition.complete();
        self.flush_tool_install_terminals(generation);
        Ok(true)
    }

    pub async fn status(&self) -> BehaviorTelemetryStatus {
        let state = self.state.lock().await;
        let control = state.as_ref().map(|value| &value.control);
        let (pending_event_count, dropped_event_count) = self
            .outbox
            .lock()
            .map(|outbox| (outbox.records.len(), outbox.dropped_event_count))
            .unwrap_or_default();
        BehaviorTelemetryStatus {
            configured: self.runtime.enabled && self.state_healthy.load(Ordering::Acquire),
            enabled: self.collecting.load(Ordering::Acquire),
            collection_period_id: control.and_then(|value| value.collection_period_id.clone()),
            withdrawal_pending: control.is_some_and(|value| {
                matches!(
                    value.pending_request.as_ref().map(|request| &request.kind),
                    Some(PendingControlKind::End)
                )
            }),
            deletion_pending: control.is_some_and(|value| {
                matches!(
                    value.pending_request.as_ref().map(|request| &request.kind),
                    Some(PendingControlKind::Delete)
                ) || !value.pending_deletions.is_empty()
            }),
            last_deletion_request_id: control
                .and_then(|value| value.last_deletion_request_id.clone()),
            retention_days: 35,
            pending_event_count,
            dropped_event_count,
        }
    }

    pub async fn set_collection_enabled(&self, enabled: bool) -> Result<BehaviorTelemetryStatus> {
        let _control_guard = self.control_gate.lock().await;
        if !self.state_healthy.load(Ordering::Acquire) {
            anyhow::bail!(
                "behavior telemetry state is unreadable; repair or delete the local state file before continuing"
            );
        }
        if enabled {
            if !self.runtime.enabled {
                anyhow::bail!("behavior telemetry service is not configured");
            }
            if self.collecting.load(Ordering::Acquire) {
                return Ok(self.status().await);
            }
            if self.collection_is_blocked().await {
                anyhow::bail!(
                    "behavior telemetry collection is blocked after authoritative deletion"
                );
            }
            while self
                .state
                .lock()
                .await
                .as_ref()
                .is_some_and(PersistedState::has_pending_control)
            {
                self.retry_pending_control_locked().await?;
            }
            if self.collection_is_blocked().await {
                anyhow::bail!(
                    "behavior telemetry collection is blocked after authoritative deletion"
                );
            }
            self.migrate_endpoint_locked().await?;
            if self.collection_is_blocked().await {
                anyhow::bail!(
                    "behavior telemetry collection is blocked after authoritative deletion"
                );
            }
            let credentials = self.ensure_credentials().await?;
            let request_id = token("control_", 18);
            let response = self
                .start_collection_with_renewal_locked(&credentials, request_id)
                .await?;
            let mut guard = self.state.lock().await;
            let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
            let emit_cohort = !state.control.cohort_started;
            let generation = match self.invalidate_generation_above(state.control.generation) {
                Ok(generation) => generation,
                Err(_) => {
                    state.control.collection_period_id = Some(response.collection_period_id);
                    state.control.pending_request = None;
                    converge_generation_exhausted(state);
                    self.enter_generation_exhausted_fail_close();
                    let mut failures = Vec::new();
                    if let Err(error) = persist_state(state) {
                        self.state_healthy.store(false, Ordering::Release);
                        failures.push(format!(
                            "persist generation-exhausted collection start: {error:#}"
                        ));
                    }
                    drop(guard);
                    return self.finish_generation_exhausted("start behavior collection", failures);
                }
            };
            state.control.enabled = true;
            state.control.collection_period_id = Some(response.collection_period_id);
            state.control.generation = generation;
            state.control.pending_request = None;
            state.control.cohort_started = true;
            persist_state(state)?;
            if let Ok(mut period) = self.collection_period_id.write() {
                *period = state.control.collection_period_id.clone();
            }
            self.collecting.store(true, Ordering::Release);
            drop(guard);
            if emit_cohort {
                let previous_version_present =
                    paths::pinvou3_home().join("settings.json").is_file();
                self.track(
                    BehaviorEvent::new("analytics_cohort_started")
                        .string("eligibility_source", "first_open")
                        .string(
                            "install_source",
                            if previous_version_present {
                                "upgrade"
                            } else {
                                "direct"
                            },
                        )
                        .boolean("previous_version_present", previous_version_present),
                );
            }
        } else {
            let mut generation_exhausted = false;
            let mut exhaustion_failures = Vec::new();
            {
                let mut guard = self.state.lock().await;
                let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
                match self.invalidate_generation_above(state.control.generation) {
                    Ok(generation) => prepare_disabled_state(state, generation),
                    Err(_) => {
                        generation_exhausted = true;
                        converge_generation_exhausted(state);
                        self.enter_generation_exhausted_fail_close();
                    }
                }
                if let Err(error) = persist_state(state) {
                    self.state_healthy.store(false, Ordering::Release);
                    if generation_exhausted {
                        exhaustion_failures
                            .push(format!("persist generation-exhausted stop: {error:#}"));
                    } else {
                        return Err(error.context("persist disabled behavior telemetry state"));
                    }
                }
            }
            self.collecting.store(false, Ordering::Release);
            if let Ok(mut period) = self.collection_period_id.write() {
                *period = None;
            }
            if generation_exhausted {
                return self
                    .finish_generation_exhausted("stop behavior collection", exhaustion_failures);
            }
            self.wake_control_worker();
            let failures = self.clear_local_transient_state();
            if !failures.is_empty() {
                anyhow::bail!(
                    "stop behavior collection cleanup incomplete: {}",
                    failures.join("; ")
                );
            }
        }
        Ok(self.status().await)
    }

    pub async fn delete_local_data(&self) -> Result<BehaviorTelemetryStatus> {
        self.outbox
            .lock()
            .map_err(|_| anyhow::anyhow!("behavior outbox lock poisoned"))?
            .clear()?;
        if let Ok(mut tasks) = self.tasks.lock() {
            tasks.clear();
            persist_task_attempts(&tasks)?;
        }
        if let Ok(mut submissions) = self.voice_submissions.lock() {
            submissions.clear();
        }
        if let Ok(mut artifacts) = self.artifacts.lock() {
            artifacts.clear();
        }
        if let Ok(mut latest_turns) = self.latest_turns.lock() {
            latest_turns.clear();
        }
        Ok(self.status().await)
    }

    pub async fn delete_remote_data(&self) -> Result<BehaviorTelemetryStatus> {
        let _control_guard = self.control_gate.lock().await;
        let mut generation_exhausted = false;
        let mut failures = Vec::new();
        {
            let mut guard = self.state.lock().await;
            let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
            let generation = match self.invalidate_generation_above(state.control.generation) {
                Ok(generation) => generation,
                Err(_) => {
                    generation_exhausted = true;
                    self.enter_generation_exhausted_fail_close();
                    u64::MAX
                }
            };
            let mut deletion_targets = state.control.pending_deletions.clone();
            deletion_targets.extend(state.deletion_targets());
            state.control.enabled = false;
            state.control.collection_period_id = None;
            state.control.generation = generation;
            state.control.collection_blocked = true;
            state.control.pending_request = None;
            state.control.pending_deletions = deletion_targets;
            enqueue_retired_endpoint_deletions(state);
            if let Err(error) = persist_state(state) {
                self.state_healthy.store(false, Ordering::Release);
                if generation_exhausted {
                    failures.push(format!(
                        "persist generation-exhausted remote deletion: {error:#}"
                    ));
                } else {
                    return Err(error.context("persist remote behavior deletion"));
                }
            }
        }
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
        if generation_exhausted {
            return self.finish_generation_exhausted("delete remote behavior data", failures);
        }
        if self.has_pending_control().await {
            self.wake_control_worker();
        }
        self.delete_local_data().await
    }

    async fn retry_pending_control(&self) -> Result<()> {
        let _control_guard = self.control_gate.lock().await;
        self.retry_pending_control_locked().await
    }

    async fn retry_pending_control_locked(&self) -> Result<()> {
        let snapshot = self.state.lock().await.clone();
        let Some(snapshot) = snapshot else {
            return Ok(());
        };
        if let Some(pending) = snapshot.control.pending_request.clone() {
            let credentials = self
                .credentials_for_pending(&pending)
                .await
                .context("credentials unavailable for pending behavior control")?;
            return self.send_pending_control(&credentials, pending).await;
        }
        let Some(pending) = snapshot.control.pending_deletions.first().cloned() else {
            return Ok(());
        };
        self.send_pending_deletion(pending).await
    }

    async fn send_pending_deletion(&self, pending: PendingEndpointDeletion) -> Result<()> {
        let credentials = Credentials {
            device_id: pending.device_id.clone(),
            device_token: pending.device_token.clone(),
            behavior_events_url: pending.behavior_events_url.clone(),
        };
        let response = self
            .request_control(
                &credentials,
                "deletion",
                json!({ "request_id": pending.request_id.clone() }),
            )
            .await?;
        let (deletion_request_id, authoritative_deleted) =
            if response.status() == reqwest::StatusCode::GONE {
                match parse_control_gone(response, "deletion").await? {
                    GoneReason::DeviceCredentialsExpired => (None, false),
                    GoneReason::DeviceDataDeleted => (None, true),
                }
            } else {
                if !response.status().is_success() {
                    anyhow::bail!("behavior control deletion returned {}", response.status());
                }
                (
                    Some(
                        response
                            .json::<DeletionResponse>()
                            .await
                            .context("parse behavior control deletion")?
                            .deletion_request_id,
                    ),
                    false,
                )
            };
        let mut guard = self.state.lock().await;
        let Some(state) = guard.as_mut() else {
            return Ok(());
        };
        if !state
            .control
            .pending_deletions
            .first()
            .is_some_and(|current| current.same_request(&pending))
        {
            return Ok(());
        }
        let mut generation_exhausted = false;
        let completion_generation = if authoritative_deleted
            || !state.control.collection_blocked
            || state.control.enabled
            || state.control.collection_period_id.is_some()
        {
            match self.invalidate_generation_above(state.control.generation) {
                Ok(generation) => generation,
                Err(_) => {
                    generation_exhausted = true;
                    self.enter_generation_exhausted_fail_close();
                    u64::MAX
                }
            }
        } else {
            self.authorization_generation.load(Ordering::Acquire)
        };
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
        state.control.pending_deletions.remove(0);
        state.control.enabled = false;
        state.control.collection_period_id = None;
        state.control.pending_request = None;
        state.control.collection_blocked = true;
        state.control.generation = state.control.generation.max(completion_generation);
        if deletion_request_id.is_some() {
            state.control.last_deletion_request_id = deletion_request_id;
        }
        state.retired_endpoints.retain(|retired| {
            retired.behavior_events_url != pending.behavior_events_url
                || retired.device_id != pending.device_id
                || retired.device_token != pending.device_token
        });
        if state.behavior_events_url == pending.behavior_events_url
            && state.device_id.as_deref() == Some(pending.device_id.as_str())
            && state.device_token.as_deref() == Some(pending.device_token.as_str())
        {
            state.device_id = None;
            state.device_token = None;
        }
        let mut failures = Vec::new();
        if let Err(error) = persist_state(state) {
            self.state_healthy.store(false, Ordering::Release);
            failures.push(format!("persist endpoint deletion: {error:#}"));
        }
        let has_more_deletions = !state.control.pending_deletions.is_empty();
        drop(guard);
        if has_more_deletions {
            self.wake_control_worker();
        }
        if generation_exhausted {
            return self.finish_generation_exhausted("endpoint deletion", failures);
        }
        failures.extend(self.clear_local_transient_state());
        if !failures.is_empty() {
            anyhow::bail!(
                "endpoint deletion cleanup incomplete: {}",
                failures.join("; ")
            );
        }
        Ok(())
    }

    async fn send_pending_control(
        &self,
        credentials: &Credentials,
        pending: PendingControlRequest,
    ) -> Result<()> {
        match pending.kind {
            PendingControlKind::End => {
                let response = self
                    .request_control(
                        credentials,
                        "collection/end",
                        json!({
                            "request_id": pending.request_id.clone(),
                            "collection_period_id": pending.collection_period_id.clone(),
                        }),
                    )
                    .await?;
                if response.status() == reqwest::StatusCode::GONE {
                    match parse_control_gone(response, "collection/end").await? {
                        GoneReason::DeviceCredentialsExpired => {}
                        GoneReason::DeviceDataDeleted => {
                            let _ = self
                                .handle_authoritative_deletion_locked(credentials)
                                .await?;
                            anyhow::bail!(
                                "behavior telemetry data was deleted while ending collection"
                            );
                        }
                    }
                } else {
                    if !response.status().is_success() {
                        anyhow::bail!(
                            "behavior control collection/end returned {}",
                            response.status()
                        );
                    }
                    let _: CollectionEndResponse = response
                        .json()
                        .await
                        .context("parse behavior control collection/end")?;
                }
                let mut guard = self.state.lock().await;
                if let Some(state) = guard.as_mut() {
                    if !pending_request_matches(state, &pending) {
                        return Ok(());
                    }
                    state.control.collection_period_id = None;
                    state.control.pending_request = None;
                    persist_state(state)?;
                }
            }
            PendingControlKind::Delete => {
                let response = self
                    .request_control(
                        credentials,
                        "deletion",
                        json!({ "request_id": pending.request_id.clone() }),
                    )
                    .await?;
                let (deletion_request_id, authoritative_deleted) = if response.status()
                    == reqwest::StatusCode::GONE
                {
                    match parse_control_gone(response, "legacy deletion").await? {
                        GoneReason::DeviceCredentialsExpired => (None, false),
                        GoneReason::DeviceDataDeleted => (None, true),
                    }
                } else {
                    if !response.status().is_success() {
                        anyhow::bail!("behavior control deletion returned {}", response.status());
                    }
                    (
                        Some(
                            response
                                .json::<DeletionResponse>()
                                .await
                                .context("parse behavior control deletion")?
                                .deletion_request_id,
                        ),
                        false,
                    )
                };
                let mut guard = self.state.lock().await;
                if !guard
                    .as_ref()
                    .is_some_and(|state| pending_request_matches(state, &pending))
                {
                    return Ok(());
                }
                let state = guard
                    .as_mut()
                    .expect("legacy deletion state remains available");
                let mut generation_exhausted = false;
                let generation = if authoritative_deleted {
                    match self.invalidate_generation_above(state.control.generation) {
                        Ok(generation) => generation,
                        Err(_) => {
                            generation_exhausted = true;
                            self.enter_generation_exhausted_fail_close();
                            u64::MAX
                        }
                    }
                } else {
                    self.authorization_generation.load(Ordering::Acquire)
                };
                converge_legacy_deletion(state, credentials, deletion_request_id, generation);
                self.collecting.store(false, Ordering::Release);
                if let Ok(mut period) = self.collection_period_id.write() {
                    *period = None;
                }
                let mut failures = Vec::new();
                if let Err(error) = persist_state(state) {
                    self.state_healthy.store(false, Ordering::Release);
                    failures.push(format!("persist legacy behavior deletion: {error:#}"));
                }
                let has_more_deletions = !state.control.pending_deletions.is_empty();
                drop(guard);
                if generation_exhausted {
                    return self.finish_generation_exhausted("legacy behavior deletion", failures);
                }
                failures.extend(self.clear_local_transient_state());
                if has_more_deletions {
                    self.wake_control_worker();
                }
                if !failures.is_empty() {
                    anyhow::bail!(
                        "legacy behavior deletion cleanup incomplete: {}",
                        failures.join("; ")
                    );
                }
            }
        }
        Ok(())
    }

    async fn migrate_endpoint_locked(&self) -> Result<()> {
        let snapshot = self.state.lock().await.clone();
        let Some(snapshot) = snapshot else {
            return Ok(());
        };
        if endpoint_matches_runtime(&snapshot, &self.runtime) {
            return Ok(());
        }
        if snapshot.has_pending_control() {
            anyhow::bail!("pending behavior control must finish before endpoint migration");
        }
        let Some(period_id) = snapshot.control.collection_period_id.clone() else {
            return Ok(());
        };
        if persisted_credentials_from_state(&snapshot).is_none() {
            anyhow::bail!(
                "cannot end the previous behavior collection period because its credentials are unavailable"
            );
        }
        {
            let mut guard = self.state.lock().await;
            let state = guard
                .as_mut()
                .context("behavior telemetry state disappeared during endpoint migration")?;
            if endpoint_matches_runtime(state, &self.runtime) {
                return Ok(());
            }
            state.control.enabled = false;
            state.control.pending_request = Some(PendingControlRequest {
                kind: PendingControlKind::End,
                request_id: token("control_", 18),
                collection_period_id: Some(period_id),
                behavior_events_url: state.behavior_events_url.clone(),
            });
            persist_state(state)?;
        }
        self.retry_pending_control_locked().await
    }

    async fn start_collection_with_renewal_locked(
        &self,
        credentials: &Credentials,
        request_id: String,
    ) -> Result<CollectionStartResponse> {
        let response = self
            .request_control(
                credentials,
                "collection/start",
                json!({ "request_id": request_id }),
            )
            .await?;
        if response.status() == reqwest::StatusCode::GONE {
            match parse_control_gone(response, "collection/start").await? {
                GoneReason::DeviceCredentialsExpired => {
                    if self.recover_expired_credentials_locked(credentials).await? {
                        let collection_period_id = self
                            .state
                            .lock()
                            .await
                            .as_ref()
                            .and_then(|state| state.control.collection_period_id.clone())
                            .context("renewed behavior collection period is unavailable")?;
                        return Ok(CollectionStartResponse {
                            collection_period_id,
                        });
                    }
                    anyhow::bail!("behavior collection credentials expired");
                }
                GoneReason::DeviceDataDeleted => {
                    let _ = self
                        .handle_authoritative_deletion_locked(credentials)
                        .await?;
                    anyhow::bail!("behavior telemetry data was deleted; local collection disabled");
                }
            }
        }
        if !response.status().is_success() {
            anyhow::bail!(
                "behavior control collection/start returned {}",
                response.status()
            );
        }
        response
            .json()
            .await
            .context("parse behavior control collection/start")
    }

    async fn request_control(
        &self,
        credentials: &Credentials,
        path: &str,
        mut body: Value,
    ) -> Result<reqwest::Response> {
        if let Value::Object(ref mut object) = body {
            object.insert(
                "device_id".to_string(),
                Value::String(credentials.device_id.clone()),
            );
        }
        let base = credentials
            .behavior_events_url
            .trim_end_matches('/')
            .strip_suffix("/events")
            .unwrap_or(credentials.behavior_events_url.trim_end_matches('/'));
        self.client
            .post(format!("{base}/{path}"))
            .bearer_auth(&credentials.device_token)
            .json(&body)
            .send()
            .await
            .with_context(|| format!("send behavior control {path}"))
    }

    async fn ensure_credentials(&self) -> Result<Credentials> {
        if !self.state_healthy.load(Ordering::Acquire) {
            anyhow::bail!(
                "behavior telemetry state is unreadable; refusing to replace the persisted installation identity"
            );
        }
        if let Some(credentials) = self.current_credentials().await {
            return Ok(credentials);
        }
        let _credential_guard = self.credential_gate.lock().await;
        if let Some(credentials) = self.current_credentials().await {
            return Ok(credentials);
        }
        let enrollment_token = self
            .runtime
            .enrollment_token
            .as_deref()
            .context("PINVOU_TELEMETRY_ENROLLMENT_TOKEN is not configured")?;
        let mut state = {
            let mut guard = self.state.lock().await;
            let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
            state.clone()
        };
        if !endpoint_matches_runtime(&state, &self.runtime) {
            if state.has_pending_control() || state.control.collection_period_id.is_some() {
                anyhow::bail!(
                    "previous behavior endpoint control state must be resolved before registration"
                );
            }
            state = state.rebind_endpoint(&self.runtime);
        }
        let body = json!({
            "enrollment_token": enrollment_token,
            "hardware_claim": format!("pinvou3-installation:{}", state.installation_id),
            "registration_secret": state.registration_secret,
            "hardware_source": "installation_id",
            "identity_quality": "installation_only",
            "app_version": self.runtime.app_version,
            "platform": self.runtime.platform,
            "arch": self.runtime.arch,
        });
        let response = self
            .client
            .post(format!(
                "{}/v1/register",
                state.telemetry_base_url.trim_end_matches('/')
            ))
            .json(&body)
            .send()
            .await
            .context("register behavior telemetry device")?;
        if response.status() == reqwest::StatusCode::GONE {
            let error_code = telemetry_error_code(response).await;
            if error_code == "installation_data_deleted" {
                let handled = self
                    .handle_authoritative_installation_deletion_locked(&state)
                    .await?;
                if !handled {
                    self.fail_close_unmatched_authoritative_deletion();
                }
                anyhow::bail!(
                    "behavior telemetry installation data was deleted; local collection disabled"
                );
            }
            anyhow::bail!("telemetry registration unavailable: {error_code}");
        }
        if !response.status().is_success() {
            anyhow::bail!("telemetry registration returned {}", response.status());
        }
        let registered: RegistrationResponse = response
            .json()
            .await
            .context("parse telemetry registration response")?;
        state.device_id = Some(registered.device_id);
        state.device_token = Some(registered.device_token);
        let mut guard = self.state.lock().await;
        let current = guard
            .as_ref()
            .context("behavior telemetry state disappeared during registration")?;
        if current.installation_id != state.installation_id
            || current.registration_secret != state.registration_secret
            || current.control.generation != state.control.generation
            || current.control.collection_blocked
            || current.has_pending_control()
        {
            anyhow::bail!("behavior telemetry state changed while registration was in flight");
        }
        persist_state(&state)?;
        *guard = Some(state);
        drop(guard);
        self.current_credentials()
            .await
            .context("telemetry credentials missing after registration")
    }

    fn fail_close_unmatched_authoritative_deletion(&self) {
        self.state_healthy.store(false, Ordering::Release);
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
    }

    async fn current_credentials(&self) -> Option<Credentials> {
        let state = self.state.lock().await.clone()?;
        if !endpoint_matches_runtime(&state, &self.runtime) {
            return None;
        }
        persisted_credentials_from_state(&state)
    }

    async fn credentials_for_pending(
        &self,
        pending: &PendingControlRequest,
    ) -> Option<Credentials> {
        let state = self.state.lock().await.clone()?;
        if !pending_request_matches(&state, pending) {
            return None;
        }
        let mut credentials = persisted_credentials_from_state(&state)?;
        if !pending.behavior_events_url.is_empty() {
            credentials.behavior_events_url = pending.behavior_events_url.clone();
        }
        Some(credentials)
    }
}

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
            .unwrap_or_default()
            .trim_end_matches('/')
            .to_string();
        let behavior_events_url = std::env::var("PINVOU_BEHAVIOR_EVENTS_URL").unwrap_or_default();
        let enrollment_token = std::env::var("PINVOU_TELEMETRY_ENROLLMENT_TOKEN")
            .ok()
            .filter(|value| value.len() >= 24);
        let enabled =
            requested_enabled && !telemetry_base_url.is_empty() && !behavior_events_url.is_empty();
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
    filesystem::atomic_write(path, &data).with_context(|| format!("write {}", path.display()))?;
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
    filesystem::atomic_write(&task_attempts_path(), &data).context("persist behavior task attempts")
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

pub fn track(app: &tauri::AppHandle, event: BehaviorEvent) {
    if let Some(telemetry) = app.try_state::<BehaviorTelemetry>() {
        telemetry.track(event);
    }
}

pub fn register_team_session(app: &tauri::AppHandle, session_id: &str) {
    if let Some(telemetry) = app.try_state::<BehaviorTelemetry>() {
        let Some(lease) = telemetry.current_lease() else {
            return;
        };
        if let Ok(mut sessions) = telemetry.team_sessions.lock() {
            if telemetry.lease_is_current(&lease) {
                sessions.insert(session_id.to_string(), lease);
            }
        }
    }
}

pub fn is_team_session(app: &tauri::AppHandle, session_id: &str) -> bool {
    app.try_state::<BehaviorTelemetry>()
        .is_some_and(|telemetry| {
            telemetry.team_sessions.lock().is_ok_and(|sessions| {
                sessions
                    .get(session_id)
                    .is_some_and(|lease| telemetry.lease_is_current(lease))
            })
        })
}

fn task_fields(event: BehaviorEvent, session_id: &str, attempt: &TaskAttempt) -> BehaviorEvent {
    event
        .session(session_id)
        .string("attempt_id", &attempt.attempt_id)
        .string("mode", &attempt.mode)
        .string("initiator_type", &attempt.initiator_type)
        .input_type(&attempt.input_type)
        .boolean("has_attachment", attempt.has_attachment)
}

pub fn begin_task_submission(
    app: &tauri::AppHandle,
    session_id: &str,
    mode: &str,
    initiator_type: &str,
    input_type: &str,
    has_attachment: bool,
) -> TaskSubmissionGuard {
    let telemetry = app.try_state::<BehaviorTelemetry>();
    let lease = telemetry
        .as_ref()
        .and_then(|telemetry| telemetry.current_lease());
    let attempt = TaskAttempt {
        attempt_id: token("attempt_", 16),
        submitted_at: now_ms(),
        admitted_at: None,
        turn_id: None,
        mode: mode.to_string(),
        initiator_type: initiator_type.to_string(),
        input_type: input_type.to_string(),
        has_attachment,
        first_output_emitted: false,
        generation: lease.as_ref().map_or(0, |lease| lease.generation),
        collection_period_id: lease
            .as_ref()
            .map_or_else(String::new, |lease| lease.collection_period_id.clone()),
    };
    if let (Some(telemetry), Some(lease)) = (telemetry, lease) {
        let inserted = if let Ok(mut tasks) = telemetry.tasks.lock() {
            if telemetry.lease_is_current(&lease) {
                tasks.insert(session_id.to_string(), attempt.clone());
                let _ = persist_task_attempts(&tasks);
                true
            } else {
                false
            }
        } else {
            false
        };
        if inserted {
            telemetry.track_for_lease(
                task_fields(BehaviorEvent::new("task_submitted"), session_id, &attempt),
                &lease,
            );
        }
    }
    TaskSubmissionGuard {
        app: app.clone(),
        session_id: session_id.to_string(),
        attempt_id: attempt.attempt_id,
        reject_on_drop: true,
    }
}

pub fn begin_task_retry(
    app: &tauri::AppHandle,
    session_id: &str,
    mode: &str,
    input_type: &str,
    has_attachment: bool,
    retry_reason: &str,
) -> TaskSubmissionGuard {
    let source_turn = app.try_state::<BehaviorTelemetry>().and_then(|telemetry| {
        let record = telemetry
            .latest_turns
            .lock()
            .ok()?
            .get(session_id)
            .cloned()?;
        telemetry.lease_is_current(&record.lease).then_some(record)
    });
    let guard = begin_task_submission(app, session_id, mode, "user", input_type, has_attachment);
    let Some(source_turn) = source_turn else {
        return guard;
    };
    if let Some(telemetry) = app.try_state::<BehaviorTelemetry>() {
        let attempt = telemetry.tasks.lock().ok().and_then(|tasks| {
            tasks.get(session_id).cloned().filter(|attempt| {
                attempt.generation == source_turn.lease.generation
                    && attempt.collection_period_id == source_turn.lease.collection_period_id
                    && telemetry.lease_is_current(&attempt.lease())
            })
        });
        if let Some(attempt) = attempt {
            telemetry.track_for_lease(
                BehaviorEvent::new("task_retry_requested")
                    .session(session_id)
                    .string("attempt_id", &attempt.attempt_id)
                    .string("source_turn_id", source_turn.turn_id)
                    .string("retry_reason", retry_reason_key(retry_reason)),
                &attempt.lease(),
            );
        }
    }
    guard
}

/// Associates a voice operation with the task attempt that was actually
/// admitted by the chat command. The frontend never manufactures attempt IDs.
pub fn track_voice_message_submitted(
    app: &tauri::AppHandle,
    session_id: &str,
    operation_id: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let attempt = telemetry
        .tasks
        .lock()
        .map_err(|_| "behavior task state is unavailable".to_string())?
        .get(session_id)
        .cloned()
        .ok_or_else(|| "no active task attempt for voice submission".to_string())?;
    let lease = attempt.lease();
    if !telemetry.lease_is_current(&lease) {
        return Ok(());
    }
    let dedup_key = format!("{operation_id}:{}", attempt.attempt_id);
    let mut submissions = telemetry
        .voice_submissions
        .lock()
        .map_err(|_| "voice submission state is unavailable".to_string())?;
    if submissions.len() >= OUTBOX_MAX_EVENTS {
        submissions.clear();
    }
    if !telemetry.lease_is_current(&lease) || submissions.contains_key(&dedup_key) {
        return Ok(());
    }
    submissions.insert(dedup_key, lease.clone());
    drop(submissions);
    telemetry.track_for_lease(
        BehaviorEvent::new("voice_message_submitted")
            .session(session_id)
            .string("operation_id", operation_id)
            .string("attempt_id", attempt.attempt_id),
        &lease,
    );
    Ok(())
}

pub fn scene_registry_pair(scene_l1: &str, scene_l2: &str) -> (&'static str, &'static str) {
    match (scene_l1, scene_l2) {
        ("work", "personal-workbench") => ("work", "personal-workbench"),
        ("work", "document-writing") => ("work", "document-writing"),
        ("design", "poster") => ("design", "poster"),
        ("design", "data-visualization") => ("design", "data-visualization"),
        ("design", "ppt") => ("design", "ppt"),
        _ => ("unknown", "unknown"),
    }
}

/// Associates the selected scene with the attempt created by the authoritative
/// chat command. The frontend supplies only controlled scene keys and never an
/// attempt identifier.
pub fn track_scene_submitted(
    app: &tauri::AppHandle,
    session_id: &str,
    scene_l1: &str,
    scene_l2: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let attempt = telemetry
        .tasks
        .lock()
        .map_err(|_| "behavior task state is unavailable".to_string())?
        .get(session_id)
        .cloned()
        .ok_or_else(|| "no active task attempt for scene submission".to_string())?;
    let lease = attempt.lease();
    if !telemetry.lease_is_current(&lease) {
        return Ok(());
    }
    let dedup_key = format!("scene:{}", attempt.attempt_id);
    let mut submissions = telemetry
        .voice_submissions
        .lock()
        .map_err(|_| "scene submission state is unavailable".to_string())?;
    if submissions.len() >= OUTBOX_MAX_EVENTS {
        submissions.clear();
    }
    if !telemetry.lease_is_current(&lease) || submissions.contains_key(&dedup_key) {
        return Ok(());
    }
    submissions.insert(dedup_key, lease.clone());
    drop(submissions);
    let (scene_l1, scene_l2) = scene_registry_pair(scene_l1, scene_l2);
    telemetry.track_for_lease(
        BehaviorEvent::new("scene_submitted")
            .session(session_id)
            .string("attempt_id", attempt.attempt_id)
            .scene(scene_l1, scene_l2),
        &lease,
    );
    Ok(())
}

fn artifact_type_key(value: &str) -> &'static str {
    match value {
        "document" => "document",
        "spreadsheet" => "spreadsheet",
        "slides" => "slides",
        "pdf" => "pdf",
        "image" => "image",
        "code" => "code",
        "html" => "html",
        _ => "other",
    }
}

fn export_format_key(value: &str) -> &'static str {
    match value {
        "docx" => "docx",
        "xlsx" => "xlsx",
        "pptx" => "pptx",
        "pdf" => "pdf",
        "png" => "png",
        "html" => "html",
        "markdown" => "markdown",
        _ => "other",
    }
}

fn scheduled_subject_id(
    lease: &CollectionLease,
    namespace: &str,
    source_id: &str,
) -> Result<String, String> {
    let mut digest = Sha256::new();
    digest.update(b"pinvou-behavior-v1\0");
    digest.update(namespace.as_bytes());
    digest.update(b"\0");
    digest.update(lease.collection_period_id.as_bytes());
    digest.update(b"\0");
    digest.update(source_id.as_bytes());
    let hex = crate::platform::encoding::hex_lower(&digest.finalize());
    Ok(format!("{namespace}_{}", &hex[..24]))
}

fn feedback_action_key(value: &str) -> Result<&'static str, String> {
    match value {
        "set" => Ok("set"),
        "clear" => Ok("clear"),
        _ => Err("unsupported feedback action".to_string()),
    }
}

fn feedback_rating_key(value: Option<&str>) -> Result<Option<&'static str>, String> {
    match value {
        Some("positive") => Ok(Some("positive")),
        Some("negative") => Ok(Some("negative")),
        None => Ok(None),
        _ => Err("unsupported feedback rating".to_string()),
    }
}

fn feedback_reason_key(value: Option<&str>) -> Result<Option<&'static str>, String> {
    match value {
        Some("accurate") => Ok(Some("accurate")),
        Some("helpful") => Ok(Some("helpful")),
        Some("fast") => Ok(Some("fast")),
        Some("incorrect") => Ok(Some("incorrect")),
        Some("incomplete") => Ok(Some("incomplete")),
        Some("unsafe") => Ok(Some("unsafe")),
        Some("too_slow") => Ok(Some("too_slow")),
        Some("other") => Ok(Some("other")),
        None => Ok(None),
        _ => Err("unsupported feedback reason".to_string()),
    }
}

fn response_content_type_key(value: &str) -> &'static str {
    match value {
        "text" => "text",
        "code" => "code",
        "table" => "table",
        _ => "other",
    }
}

fn retry_reason_key(value: &str) -> &'static str {
    match value {
        "manual_regenerate" => "manual_regenerate",
        "previous_failed" => "previous_failed",
        "input_changed" => "input_changed",
        _ => "unknown",
    }
}

pub fn track_response_feedback_changed(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    action: &str,
    rating: Option<&str>,
    reason: Option<&str>,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let Some(lease) = telemetry.current_lease() else {
        return Ok(());
    };
    let action = feedback_action_key(action)?;
    let rating = feedback_rating_key(rating)?;
    let reason = feedback_reason_key(reason)?;
    if (action == "set") != rating.is_some() || (action == "clear" && reason.is_some()) {
        return Err("feedback action and state do not match".to_string());
    }
    let source = format!("{session_id}\u{1f}{turn_id}");
    let feedback_id = scheduled_subject_id(&lease, "feedback", &source)?;
    let mut event = BehaviorEvent::new("response_feedback_changed")
        .session(session_id)
        .turn(turn_id)
        .string("feedback_id", feedback_id)
        .string("feedback_action", action);
    if let Some(rating) = rating {
        event = event.string("rating", rating);
    }
    if let Some(reason) = reason {
        event = event.string("reason_code", reason);
    }
    telemetry.track_for_lease(event, &lease);
    Ok(())
}

pub fn track_response_copied(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    content_type: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let Some(lease) = telemetry.current_lease() else {
        return Ok(());
    };
    telemetry.track_for_lease(
        BehaviorEvent::new("response_copied")
            .session(session_id)
            .turn(turn_id)
            .string("content_type", response_content_type_key(content_type)),
        &lease,
    );
    Ok(())
}

fn scheduled_schedule_type(rrule: &str) -> &'static str {
    let rule = rrule.trim().to_ascii_uppercase();
    if rule.contains("FREQ=MONTHLY") {
        "monthly"
    } else if rule.contains("FREQ=WEEKLY") {
        let every_day = ["MO", "TU", "WE", "TH", "FR", "SA", "SU"]
            .iter()
            .all(|day| rule.contains(day));
        if every_day {
            "daily"
        } else {
            "weekly"
        }
    } else if rule.contains("FREQ=DAILY") {
        "daily"
    } else if rule.contains("FREQ=HOURLY") || rule.contains("FREQ=") {
        "cron"
    } else {
        "once"
    }
}

fn scheduled_task_type(value: &str) -> &'static str {
    match value.trim().to_ascii_lowercase().as_str() {
        "daily-brief" | "daily_brief" => "daily_brief",
        "follow-up-monitor" | "follow_up_monitor" => "follow_up_monitor",
        "weekly-review" | "weekly_review" => "weekly_review",
        "custom" => "custom",
        _ => "unknown",
    }
}

fn scheduled_status(value: &str) -> Result<&'static str, String> {
    match value {
        "completed" | "success" => Ok("success"),
        "failed" => Ok("failed"),
        "canceled" | "cancelled" => Ok("cancelled"),
        "interrupted" => Ok("interrupted"),
        _ => Err("scheduled run is not terminal".to_string()),
    }
}

pub fn track_scheduled_task_created(
    app: &tauri::AppHandle,
    source_task_id: &str,
    rrule: &str,
    task_type: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let Some(lease) = telemetry.current_lease() else {
        return Ok(());
    };
    let task_id = scheduled_subject_id(&lease, "scheduled", source_task_id)?;
    let mut event = BehaviorEvent::new("scheduled_task_created")
        .string("scheduled_task_id", &task_id)
        .string("schedule_type", scheduled_schedule_type(rrule))
        .string("task_type", scheduled_task_type(task_type));
    event.event_id = scheduled_subject_id(&lease, "scheduled_created", source_task_id)?;
    telemetry.track_for_lease(event, &lease);
    Ok(())
}

pub fn track_scheduled_run_finished(
    app: &tauri::AppHandle,
    source_task_id: &str,
    source_run_id: &str,
    status: &str,
    duration_ms: u64,
    category: Option<&str>,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let Some(lease) = telemetry.current_lease() else {
        return Ok(());
    };
    let status = scheduled_status(status)?;
    let task_id = scheduled_subject_id(&lease, "scheduled", source_task_id)?;
    let operation_id = scheduled_subject_id(&lease, "scheduled_run", source_run_id)?;
    let mut event = BehaviorEvent::new("scheduled_task_run_finished")
        .string("scheduled_task_id", task_id)
        .string("operation_id", operation_id)
        .integer("attempt_index", 1)
        .status(status)
        .integer("duration_ms", duration_ms.min(86_400_000));
    if status != "success" {
        event = event.string(
            "error_category",
            if status == "cancelled" {
                "user_cancelled"
            } else {
                error_category(category)
            },
        );
    }
    event.event_id = scheduled_subject_id(&lease, "scheduled_finished", source_run_id)?;
    telemetry.track_for_lease(event, &lease);
    Ok(())
}

pub fn track_artifact_created(
    app: &tauri::AppHandle,
    session_id: &str,
    artifact_id: &str,
    artifact_type: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let turn = telemetry
        .tasks
        .lock()
        .map_err(|_| "behavior task state is unavailable".to_string())?
        .get(session_id)
        .and_then(|attempt| {
            let turn_id = attempt.turn_id.clone()?;
            telemetry
                .lease_is_current(&attempt.lease())
                .then(|| TurnTelemetryRecord {
                    turn_id,
                    lease: attempt.lease(),
                })
        });
    let turn = turn
        .or_else(|| {
            telemetry
                .latest_turns
                .lock()
                .ok()
                .and_then(|turns| turns.get(session_id).cloned())
                .filter(|record| telemetry.lease_is_current(&record.lease))
        })
        .ok_or_else(|| "no admitted turn for artifact creation".to_string())?;
    let artifact_type = artifact_type_key(artifact_type);
    let record = ArtifactTelemetryRecord {
        session_id: session_id.to_string(),
        turn_id: turn.turn_id.clone(),
        artifact_type: artifact_type.to_string(),
        lease: turn.lease.clone(),
    };
    let mut artifacts = telemetry
        .artifacts
        .lock()
        .map_err(|_| "artifact telemetry state is unavailable".to_string())?;
    if !telemetry.lease_is_current(&turn.lease) || artifacts.contains_key(artifact_id) {
        return Ok(());
    }
    if artifacts.len() >= OUTBOX_MAX_EVENTS {
        artifacts.clear();
    }
    artifacts.insert(artifact_id.to_string(), record);
    drop(artifacts);
    telemetry.track_for_lease(
        BehaviorEvent::new("artifact_created")
            .session(session_id)
            .turn(turn.turn_id)
            .string("artifact_id", artifact_id)
            .string("artifact_type", artifact_type),
        &turn.lease,
    );
    Ok(())
}

pub fn track_artifact_exported(
    app: &tauri::AppHandle,
    artifact_id: &str,
    export_format: &str,
) -> Result<(), String> {
    let telemetry = app
        .try_state::<BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())?;
    let record = telemetry
        .artifacts
        .lock()
        .map_err(|_| "artifact telemetry state is unavailable".to_string())?
        .get(artifact_id)
        .cloned()
        .ok_or_else(|| "unknown artifact telemetry id".to_string())?;
    if !telemetry.lease_is_current(&record.lease) {
        return Ok(());
    }
    telemetry.track_for_lease(
        BehaviorEvent::new("artifact_exported")
            .session(record.session_id)
            .turn(record.turn_id)
            .string("artifact_id", artifact_id)
            .string("artifact_type", record.artifact_type)
            .string("export_format", export_format_key(export_format)),
        &record.lease,
    );
    Ok(())
}

pub fn reject_task_submission(
    app: &tauri::AppHandle,
    session_id: &str,
    attempt_id: &str,
    error_category: &str,
    stage: &str,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let attempt = telemetry.tasks.lock().ok().and_then(|mut tasks| {
        let removed = if tasks.get(session_id).is_some_and(|attempt| {
            attempt.attempt_id == attempt_id
                && attempt.admitted_at.is_none()
                && telemetry.lease_is_current(&attempt.lease())
        }) {
            tasks.remove(session_id)
        } else {
            None
        };
        if removed.is_some() {
            let _ = persist_task_attempts(&tasks);
        }
        removed
    });
    if let Some(attempt) = attempt {
        telemetry.track_for_lease(
            task_fields(BehaviorEvent::new("task_rejected"), session_id, &attempt)
                .string("error_category", error_category)
                .stage(stage),
            &attempt.lease(),
        );
    }
}

pub fn admit_task(app: &tauri::AppHandle, session_id: &str, turn_id: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let admitted_at = now_ms();
    let attempt = telemetry.tasks.lock().ok().and_then(|mut tasks| {
        let attempt = tasks.get_mut(session_id)?;
        if attempt.admitted_at.is_some() || !telemetry.lease_is_current(&attempt.lease()) {
            return None;
        }
        attempt.admitted_at = Some(admitted_at);
        attempt.turn_id = Some(turn_id.to_string());
        let result = attempt.clone();
        let _ = persist_task_attempts(&tasks);
        Some(result)
    });
    if let Some(attempt) = attempt {
        let lease = attempt.lease();
        if let Ok(mut turns) = telemetry.latest_turns.lock() {
            if !telemetry.lease_is_current(&lease) {
                return;
            }
            if turns.len() >= OUTBOX_MAX_EVENTS && !turns.contains_key(session_id) {
                turns.clear();
            }
            turns.insert(
                session_id.to_string(),
                TurnTelemetryRecord {
                    turn_id: turn_id.to_string(),
                    lease: lease.clone(),
                },
            );
        }
        telemetry.track_for_lease(
            task_fields(BehaviorEvent::new("task_admitted"), session_id, &attempt)
                .turn(turn_id)
                .integer(
                    "queue_duration_ms",
                    admitted_at.saturating_sub(attempt.submitted_at),
                ),
            &lease,
        );
    }
}

pub fn task_first_output(app: &tauri::AppHandle, session_id: &str, turn_id: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let now = now_ms();
    let attempt = telemetry.tasks.lock().ok().and_then(|mut tasks| {
        let attempt = tasks.get_mut(session_id)?;
        if attempt.turn_id.as_deref() != Some(turn_id)
            || attempt.first_output_emitted
            || !telemetry.lease_is_current(&attempt.lease())
        {
            return None;
        }
        attempt.first_output_emitted = true;
        let result = attempt.clone();
        let _ = persist_task_attempts(&tasks);
        Some(result)
    });
    if let Some(attempt) = attempt {
        telemetry.track_for_lease(
            task_fields(
                BehaviorEvent::new("task_first_output"),
                session_id,
                &attempt,
            )
            .turn(turn_id)
            .string("output_type", "text")
            .integer("duration_ms", now.saturating_sub(attempt.submitted_at)),
            &attempt.lease(),
        );
    }
}

pub fn request_task_cancel(app: &tauri::AppHandle, session_id: &str) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let attempt = telemetry
        .tasks
        .lock()
        .ok()
        .and_then(|tasks| tasks.get(session_id).cloned());
    if let Some(attempt) = attempt.filter(|attempt| telemetry.lease_is_current(&attempt.lease())) {
        if let Some(turn_id) = attempt.turn_id.as_deref() {
            telemetry.track_for_lease(
                BehaviorEvent::new("task_cancel_requested")
                    .session(session_id)
                    .string("attempt_id", &attempt.attempt_id)
                    .turn(turn_id)
                    .stage(if attempt.first_output_emitted {
                        "output"
                    } else {
                        "model"
                    }),
                &attempt.lease(),
            );
        }
    }
}

pub fn finish_task(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    status: &str,
    error_category: Option<&str>,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let dangling_tools = telemetry
        .tool_calls
        .lock()
        .ok()
        .map(|mut calls| {
            let keys = calls
                .iter()
                .filter(|(key, operation)| {
                    key.starts_with(&format!("{session_id}\u{1f}")) && operation.turn_id == turn_id
                })
                .map(|(key, _)| key.clone())
                .collect::<Vec<_>>();
            keys.into_iter()
                .filter_map(|key| calls.remove(&key))
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    for operation in dangling_tools {
        if !telemetry.lease_is_current(&operation.lease) {
            continue;
        }
        telemetry.track_for_lease(
            BehaviorEvent::new("tool_call_completed")
                .session(session_id)
                .turn(&operation.turn_id)
                .string("operation_id", operation.operation_id)
                .integer("attempt_index", 1)
                .tool(operation.tool_key, operation.tool_key, operation.tool_type)
                .success(false)
                .integer("duration_ms", now_ms().saturating_sub(operation.started_at))
                .string(
                    "error_category",
                    if matches!(status, "cancelled" | "interrupted") {
                        "user_cancelled"
                    } else {
                        "tool_error"
                    },
                ),
            &operation.lease,
        );
    }
    let attempt = telemetry.tasks.lock().ok().and_then(|mut tasks| {
        let removed = if tasks
            .get(session_id)
            .is_some_and(|attempt| attempt.turn_id.as_deref() == Some(turn_id))
            && tasks
                .get(session_id)
                .is_some_and(|attempt| telemetry.lease_is_current(&attempt.lease()))
        {
            tasks.remove(session_id)
        } else {
            None
        };
        if removed.is_some() {
            let _ = persist_task_attempts(&tasks);
        }
        removed
    });
    if let Some(attempt) = attempt {
        let mut event = task_fields(BehaviorEvent::new("task_finished"), session_id, &attempt)
            .turn(turn_id)
            .status(status)
            .integer("duration_ms", now_ms().saturating_sub(attempt.submitted_at));
        if status != "success" {
            event = event.string("error_category", error_category.unwrap_or("unknown"));
        }
        telemetry.track_for_lease(event, &attempt.lease());
    }
}

pub fn error_category(error: Option<&str>) -> &'static str {
    let error = error.unwrap_or_default().to_ascii_lowercase();
    if error.contains("cancel") || error.contains("interrupt") {
        "user_cancelled"
    } else if error.contains("permission") || error.contains("denied") {
        "permission_denied"
    } else if error.contains("timeout") {
        "timeout"
    } else if error.contains("auth") || error.contains("401") || error.contains("403") {
        "authentication_failed"
    } else if error.contains("network") || error.contains("connect") {
        "network_unavailable"
    } else if error.contains("rate limit") || error.contains("429") {
        "rate_limited"
    } else if error.contains("config") || error.contains("missing") {
        "configuration_missing"
    } else if error.contains("model") || error.contains("provider") {
        "model_error"
    } else {
        "internal_error"
    }
}

fn tool_operation_key(session_id: &str, call_id: &str) -> String {
    format!("{session_id}\u{1f}{call_id}")
}

pub fn track_tool_call_started(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    call_id: &str,
    tool_name: &str,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let Some(lease) = telemetry.current_lease() else {
        return;
    };
    let tool_type = classify_tool(tool_name);
    let operation = ToolOperation {
        operation_id: token("toolop_", 16),
        started_at: now_ms(),
        turn_id: turn_id.to_string(),
        tool_key: tool_registry_key(tool_name),
        tool_type,
        lease: lease.clone(),
    };
    if let Ok(mut calls) = telemetry.tool_calls.lock() {
        let key = tool_operation_key(session_id, call_id);
        if !telemetry.lease_is_current(&lease) || calls.contains_key(&key) {
            return;
        }
        calls.insert(key, operation.clone());
    } else {
        return;
    }
    telemetry.track_for_lease(
        BehaviorEvent::new("tool_call_started")
            .session(session_id)
            .turn(turn_id)
            .string("operation_id", &operation.operation_id)
            .integer("attempt_index", 1)
            .tool(operation.tool_key, operation.tool_key, tool_type),
        &lease,
    );
}

pub fn track_tool_call_completed(
    app: &tauri::AppHandle,
    session_id: &str,
    _turn_id: &str,
    call_id: &str,
    _tool_name: &str,
    success: bool,
    error_category: Option<&str>,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let operation = telemetry
        .tool_calls
        .lock()
        .ok()
        .and_then(|mut calls| calls.remove(&tool_operation_key(session_id, call_id)));
    let Some(operation) = operation else {
        return;
    };
    if !telemetry.lease_is_current(&operation.lease) {
        return;
    }
    let mut event = BehaviorEvent::new("tool_call_completed")
        .session(session_id)
        .turn(operation.turn_id.clone())
        .string("operation_id", operation.operation_id)
        .integer("attempt_index", 1)
        .tool(operation.tool_key, operation.tool_key, operation.tool_type)
        .success(success)
        .integer("duration_ms", now_ms().saturating_sub(operation.started_at));
    if !success {
        event = event.string("error_category", error_category.unwrap_or("tool_error"));
    }
    telemetry.track_for_lease(event, &operation.lease);
}

pub fn begin_tool_install(
    app: &tauri::AppHandle,
    tool_name: &str,
    tool_type: &'static str,
) -> ToolInstallGuard {
    let started_at = now_ms();
    let operation_id = token("install_", 16);
    let tool_key = tool_registry_key(tool_name);
    let lease = app
        .try_state::<BehaviorTelemetry>()
        .and_then(|telemetry| telemetry.current_lease());
    if let (Some(telemetry), Some(lease)) = (app.try_state::<BehaviorTelemetry>(), lease.as_ref()) {
        telemetry.track_for_lease(
            BehaviorEvent::new("tool_install_started")
                .tool(tool_key, tool_key, tool_type)
                .string("operation_id", &operation_id)
                .integer("attempt_index", 1),
            lease,
        );
    }
    ToolInstallGuard {
        app: app.clone(),
        operation_id,
        started_at,
        tool_key,
        tool_type,
        authorization_generation: lease.map(|current| current.generation),
        completed: false,
    }
}

pub fn track_model_used(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    bridge: &crate::features::assistant::platform::bridge::Pinvou3Bridge,
) {
    let Some(model) = bridge.effective_model_owned() else {
        return;
    };
    track_model_used_keys(
        app,
        session_id,
        turn_id,
        model.preset.as_str(),
        &model.model,
        if bridge.session_model.is_some() {
            "session_default"
        } else {
            "app_default"
        },
    );
}

pub fn track_model_used_keys(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: &str,
    provider: &str,
    model: &str,
    selection_source: &str,
) {
    let Some(telemetry) = app.try_state::<BehaviorTelemetry>() else {
        return;
    };
    let attempt = telemetry.tasks.lock().ok().and_then(|tasks| {
        tasks
            .get(session_id)
            .filter(|attempt| {
                attempt.turn_id.as_deref() == Some(turn_id)
                    && telemetry.lease_is_current(&attempt.lease())
            })
            .cloned()
    });
    if let Some(attempt) = attempt {
        telemetry.track_for_lease(
            BehaviorEvent::new("model_used")
                .session(session_id)
                .string("attempt_id", &attempt.attempt_id)
                .turn(turn_id)
                .model(provider_registry_key(provider), model_registry_key(model))
                .string("selection_source", selection_source),
            &attempt.lease(),
        );
    }
}

pub fn track_model_selected(
    app: &tauri::AppHandle,
    provider: &str,
    model: &str,
    selection_scope: &str,
    entry_point: &str,
) {
    track(
        app,
        BehaviorEvent::new("model_selected")
            .model(provider_registry_key(provider), model_registry_key(model))
            .string("selection_scope", selection_scope)
            .string("entry_point", entry_point),
    );
}

pub fn provider_registry_key(provider: &str) -> &'static str {
    let key = match provider.trim().to_ascii_lowercase().as_str() {
        "deepseek" => "deepseek",
        "kimi" | "kimi_acp" => "kimi",
        "qwen" => "qwen",
        "doubao" => "doubao",
        "minimax" => "minimax",
        "glm" => "glm",
        "mimo" => "mimo",
        "openai" | "codex" | "codex_acp" => "openai",
        "anthropic" | "claude" | "claude_acp" => "anthropic",
        "gemini" => "gemini",
        "xai" => "xai",
        "local_vllm" => "local",
        "openai_compatible" => "custom",
        _ => "unknown",
    };
    controlled_registry_key(key, CONTROLLED_PROVIDER_KEYS, "unknown")
}

pub fn model_registry_key(model: &str) -> &'static str {
    let model = model.trim().to_ascii_lowercase();
    if model.is_empty() {
        return "unknown";
    }
    let key = if model.contains("deepseek") {
        "deepseek"
    } else if model.contains("claude") && model.contains("opus") {
        "claude_opus"
    } else if model.contains("claude") && model.contains("haiku") {
        "claude_haiku"
    } else if model.contains("claude") {
        "claude_sonnet"
    } else if model.contains("gpt-5") || model.contains("gpt_5") {
        "gpt_5"
    } else if model.contains("gpt-4") || model.contains("gpt_4") {
        "gpt_4"
    } else if model == "o3" || model.starts_with("o3-") {
        "o3"
    } else if model.contains("gemini") {
        "gemini"
    } else if model.contains("qwen") {
        "qwen"
    } else if model.contains("kimi") || model.contains("moonshot") {
        "kimi"
    } else if model.contains("minimax") {
        "minimax"
    } else if model.contains("glm") {
        "glm"
    } else if model.contains("mimo") {
        "mimo"
    } else if model.contains("doubao") {
        "doubao"
    } else if model.contains("grok") {
        "grok"
    } else if model.contains("llama") {
        "llama"
    } else if model.contains("mistral") {
        "mistral"
    } else {
        "custom"
    };
    controlled_registry_key(key, CONTROLLED_MODEL_KEYS, "unknown")
}

pub fn classify_tool(tool_name: &str) -> &'static str {
    if tool_name.starts_with("mcp_") || tool_name.contains("-mcp") {
        "mcp"
    } else if matches!(tool_name, "Bash" | "exec_shell" | "task_shell_start") {
        "cli"
    } else if matches!(tool_name, "load_skill" | "tool_search") {
        "skill"
    } else {
        "unknown"
    }
}

pub fn tool_registry_key(tool_name: &str) -> &'static str {
    let key = match tool_name {
        "Bash" | "exec_shell" | "task_shell_start" => "shell",
        "read_file" => "read_file",
        "write_file" | "apply_patch" => "edit_file",
        "glob" | "rg" | "search_files" => "search_files",
        "web_search" | "search_query" => "web_search",
        "view_image" => "view_image",
        "image_gen" | "imagegen" => "image_generation",
        "load_skill" => "load_skill",
        "tool_search" => "tool_search",
        "request_user_input" => "request_user_input",
        "update_plan" | "checklist_write" | "todo_write" => "update_plan",
        "feishu" => "feishu",
        "wecom" => "wecom",
        "dingtalk" => "dingtalk",
        "tmeet" => "tmeet",
        "weather" => "weather",
        "iwencai" => "iwencai",
        "ima" => "ima",
        "tencent-docs" => "tencent_docs",
        "wecom-bot" => "wecom_bot",
        "eip" => "eip",
        "zhidao" => "zhidao",
        "qcc" => "qcc",
        "patsnap-search" => "patsnap_search",
        "canva-mcp" => "canva",
        "obsidian" => "obsidian",
        "yuandian-mcp" => "yuandian",
        "gongwen" => "gongwen",
        "pptx" => "pptx",
        name if name.starts_with("mcp_") || name.contains("-mcp") => "mcp_custom",
        _ => "custom",
    };
    controlled_registry_key(key, CONTROLLED_TOOL_KEYS, "unknown")
}

fn controlled_registry_key(
    candidate: &'static str,
    registry: &[&str],
    fallback: &'static str,
) -> &'static str {
    registry
        .contains(&candidate)
        .then_some(candidate)
        .unwrap_or(fallback)
}

#[cfg(test)]
mod tests {
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
            std::env::set_var("PINVOU3_HOME", directory.path());
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
                Some(value) => std::env::set_var("PINVOU3_HOME", value),
                None => std::env::remove_var("PINVOU3_HOME"),
            }
        }
    }

    struct EnvRestore {
        values: Vec<(&'static str, Option<OsString>)>,
    }

    impl EnvRestore {
        fn set(values: &[(&'static str, &str)]) -> Self {
            let previous = values
                .iter()
                .map(|(key, value)| {
                    let old = std::env::var_os(key);
                    std::env::set_var(key, value);
                    (*key, old)
                })
                .collect();
            Self { values: previous }
        }
    }

    impl Drop for EnvRestore {
        fn drop(&mut self) {
            for (key, value) in self.values.drain(..).rev() {
                match value {
                    Some(value) => std::env::set_var(key, value),
                    None => std::env::remove_var(key),
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
                    let Some(header_end) = bytes.windows(4).position(|part| part == b"\r\n\r\n")
                    else {
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
        assert!(!serde_json::to_string(&allowed)
            .unwrap()
            .contains("private-marker"));
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
            behavior_events_url: "https://example.test/pinvou3/stats/api/behavior/events"
                .to_string(),
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
            behavior_events_url: "https://example.test/pinvou3/stats/api/behavior/events"
                .to_string(),
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
        assert!(outbox
            .records
            .iter()
            .all(|record| record.event_id != "expired"));
        assert!(outbox.dropped_event_count >= 2);

        outbox.retain_period(Some("different_period"), 1).unwrap();
        assert!(outbox.records.is_empty());
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
        assert!(!outbox
            .apply_results(
                &accepted_batch,
                &HashMap::from([("event_accept".to_string(), (DeliveryStatus::Accepted, None),)]),
            )
            .unwrap());

        outbox
            .enqueue(outbox_record("event_retry", "period_one", now))
            .unwrap();
        let retry_batch = outbox.batch();
        assert!(outbox
            .apply_results(
                &retry_batch,
                &HashMap::from([(
                    "event_retry".to_string(),
                    (DeliveryStatus::Retryable, Some("rate_limited".to_string())),
                )]),
            )
            .unwrap());
        assert_eq!(outbox.records[0].delivery_attempt_count, 1);
    }

    #[test]
    fn atomic_state_write_preserves_pending_delete_when_a_partial_sibling_exists() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("behavior-telemetry.json");
        let runtime = RuntimeConfig {
            enabled: true,
            telemetry_base_url: "https://example.test/pinvou3/telemetry".to_string(),
            behavior_events_url: "https://example.test/pinvou3/stats/api/behavior/events"
                .to_string(),
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
        assert!(error
            .to_string()
            .contains("unsupported behavior telemetry state version"));
        let unchanged: PersistedState =
            serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
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
        assert!(after
            .control
            .pending_deletions
            .iter()
            .any(|target| target.device_id == "retired_b"));
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
        assert!(!telemetry
            .credential_renewal_in_progress
            .load(Ordering::Acquire));
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
        assert!(converged
            .retired_endpoints
            .iter()
            .any(|retired| retired.device_id == "retired_device"));
        assert!(converged
            .retired_endpoints
            .iter()
            .any(|retired| retired.device_id == "migrated_device"));
        assert_eq!(converged.control.pending_deletions.len(), 3);
        assert!(converged
            .control
            .pending_deletions
            .iter()
            .any(|pending| pending.request_id == "existing_delete"));
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
        assert!(deletion_requests[1..]
            .iter()
            .all(|request| request.starts_with("POST /deletion ")));
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
        let _env = EnvRestore::set(&[
            ("PINVOU_BEHAVIOR_TELEMETRY_ENABLED", "false"),
            ("PINVOU_TELEMETRY_BASE_URL", &runtime.telemetry_base_url),
            ("PINVOU_BEHAVIOR_EVENTS_URL", &runtime.behavior_events_url),
            (
                "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
                "test-enrollment-token-1234567890",
            ),
        ]);

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
        assert!(restored.outbox.lock().unwrap().records.is_empty());
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
        assert!(!telemetry
            .credential_renewal_in_progress
            .load(Ordering::Acquire));
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

        let _env = EnvRestore::set(&[
            ("PINVOU_BEHAVIOR_TELEMETRY_ENABLED", "true"),
            ("PINVOU_TELEMETRY_BASE_URL", &runtime.telemetry_base_url),
            ("PINVOU_BEHAVIOR_EVENTS_URL", &runtime.behavior_events_url),
            (
                "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
                "test-enrollment-token-1234567890",
            ),
        ]);
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
            (200, r#"{"ended":true}"#),
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
        let mut control_worker = telemetry.worker_clone();
        control_worker.control_tx = None;
        let worker = tokio::spawn(BehaviorTelemetry::run_control_worker(
            control_rx,
            control_worker,
        ));

        let error = telemetry.set_collection_enabled(true).await.unwrap_err();
        assert!(error.to_string().contains("generation exhausted"));
        let blocked = tokio::time::timeout(Duration::from_secs(3), async {
            loop {
                let snapshot = telemetry.state.lock().await.clone().unwrap();
                if snapshot.control.pending_request.is_none() {
                    break snapshot;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("control worker should end the period created before generation exhaustion");
        assert_eq!(blocked.control.generation, u64::MAX);
        assert!(blocked.control.collection_blocked);
        assert!(!blocked.control.enabled);
        assert!(blocked.control.collection_period_id.is_none());
        assert!(blocked.control.pending_request.is_none());
        assert!(!telemetry.collecting.load(Ordering::Acquire));
        assert!(telemetry.outbox.lock().unwrap().records.is_empty());

        drop(telemetry.control_tx.take());
        tokio::time::timeout(Duration::from_secs(3), worker)
            .await
            .expect("control worker should stop when its sender is dropped")
            .unwrap();

        let requests = server.await.unwrap();
        assert_eq!(requests.len(), 2);
        assert!(requests[0].starts_with("POST /collection/start "));
        assert!(requests[1].starts_with("POST /collection/end "));
        assert!(requests
            .iter()
            .all(|request| !request.starts_with("POST /v1/register ")
                && !request.starts_with("POST /events ")));
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
        assert!(requests
            .iter()
            .all(|request| request.starts_with("POST /deletion ")));
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
        assert!(same_backend_targets
            .iter()
            .any(|target| target.behavior_events_url == runtime_b.behavior_events_url));
        assert!(same_backend_targets
            .iter()
            .any(|target| target.behavior_events_url == runtime_a.behavior_events_url));
    }

    #[test]
    fn authoritative_deletion_blocks_default_restart_for_the_matching_credential() {
        let runtime_a = RuntimeConfig {
            enabled: true,
            telemetry_base_url: "https://a.example.test/pinvou3/telemetry".to_string(),
            behavior_events_url: "https://a.example.test/pinvou3/stats/api/behavior/events"
                .to_string(),
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
            behavior_events_url: "https://example.test/pinvou3/stats/api/behavior/events"
                .to_string(),
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
}
