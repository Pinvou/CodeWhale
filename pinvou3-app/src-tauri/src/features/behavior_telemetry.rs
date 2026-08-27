use std::collections::{HashMap, HashSet};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex as StdMutex, RwLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use base64::Engine as _;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use tauri::Manager;
use tokio::sync::{mpsc, Mutex};

use crate::platform::{filesystem, paths};

const STATE_VERSION: u8 = 2;
const EVENT_WAKE_CAPACITY: usize = 1;
const EVENT_FLUSH_MAX: usize = 50;
const EVENT_BATCH_MAX_BYTES: usize = 400 * 1024;
const EVENT_FLUSH_DEBOUNCE: Duration = Duration::from_millis(1500);
const OUTBOX_RETENTION_MS: u64 = 30 * 24 * 60 * 60 * 1000;
const OUTBOX_MAX_EVENTS: usize = 5_000;
const OUTBOX_MAX_BYTES: usize = 64 * 1024 * 1024;
const RETRY_MAX_DELAY: Duration = Duration::from_secs(60);

#[derive(Clone)]
pub struct BehaviorTelemetry {
    client: reqwest::Client,
    runtime: Arc<RuntimeConfig>,
    state: Arc<Mutex<Option<PersistedState>>>,
    credential_gate: Arc<Mutex<()>>,
    control_gate: Arc<Mutex<()>>,
    collecting: Arc<AtomicBool>,
    collection_period_id: Arc<RwLock<Option<String>>>,
    outbox: Arc<StdMutex<Outbox>>,
    tasks: Arc<StdMutex<HashMap<String, TaskAttempt>>>,
    team_sessions: Arc<StdMutex<HashSet<String>>>,
    event_tx: Option<mpsc::Sender<()>>,
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

#[derive(Debug)]
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
    control: CollectionControl,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
struct CollectionControl {
    enabled: bool,
    collection_period_id: Option<String>,
    cohort_started: bool,
    pending_request: Option<PendingControlRequest>,
    last_deletion_request_id: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
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

    pub fn model(
        self,
        provider_key: impl Into<String>,
        provider_name: impl Into<String>,
        model_id: impl Into<String>,
        model_name: impl Into<String>,
    ) -> Self {
        self.string("provider_key", provider_key)
            .string("provider_name", provider_name)
            .string("model_id", model_id)
            .string("model_name", model_name)
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
    ) -> Result<()> {
        let batch_ids: std::collections::HashSet<&str> = batch
            .iter()
            .map(|record| record.event_id.as_str())
            .collect();
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
                    true
                }
            }
        });
        self.recalculate_bytes();
        self.rewrite()
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

    fn retain_period(&mut self, period_id: Option<&str>) -> Result<()> {
        let before = self.records.len();
        self.records.retain(|record| {
            period_id.is_some_and(|period_id| record.collection_period_id == period_id)
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
        let persisted = load_state().ok().flatten();
        let active_period = persisted.as_ref().and_then(|state| {
            state
                .control
                .enabled
                .then(|| state.control.collection_period_id.clone())
                .flatten()
        });
        let collecting = Arc::new(AtomicBool::new(runtime.enabled && active_period.is_some()));
        let collection_period_id = Arc::new(RwLock::new(active_period.clone()));
        let mut loaded_outbox = Outbox::load(outbox_path()).unwrap_or_else(|error| {
            log::debug!("[pinvou3][behavior] failed to load outbox: {error:#}");
            Outbox::empty(outbox_path())
        });
        if let Err(error) = loaded_outbox.retain_period(active_period.as_deref()) {
            log::debug!("[pinvou3][behavior] failed to sanitize outbox period: {error:#}");
        }
        let outbox = Arc::new(StdMutex::new(loaded_outbox));
        let recovered_tasks = load_task_attempts();
        let tasks = Arc::new(StdMutex::new(recovered_tasks.clone()));
        let state = Arc::new(Mutex::new(persisted));
        let credential_gate = Arc::new(Mutex::new(()));
        let control_gate = Arc::new(Mutex::new(()));
        let (event_tx, event_rx) = if runtime.enabled {
            let (tx, rx) = mpsc::channel(EVENT_WAKE_CAPACITY);
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
            credential_gate,
            control_gate,
            collecting,
            collection_period_id,
            outbox,
            tasks,
            team_sessions: Arc::new(StdMutex::new(HashSet::new())),
            event_tx,
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
            let retry = telemetry.worker_clone();
            tauri::async_runtime::spawn(async move {
                if let Err(error) = retry.retry_pending_control().await {
                    log::debug!("[pinvou3][behavior] pending control retry deferred: {error:#}");
                }
            });
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

    pub fn track(&self, event: BehaviorEvent) {
        if !self.collecting.load(Ordering::Acquire) {
            return;
        }
        let Some(tx) = &self.event_tx else {
            return;
        };
        let period_id = self
            .collection_period_id
            .read()
            .ok()
            .and_then(|value| value.clone());
        let Some(period_id) = period_id else {
            return;
        };
        let record = match self.prepare_record(event, period_id) {
            Ok(record) => record,
            Err(error) => {
                log::debug!("[pinvou3][behavior] event serialization skipped: {error:#}");
                return;
            }
        };
        let enqueued = self
            .outbox
            .lock()
            .map_err(|_| anyhow::anyhow!("behavior outbox lock poisoned"))
            .and_then(|mut outbox| outbox.enqueue(record));
        if let Err(error) = enqueued {
            log::debug!("[pinvou3][behavior] persistent enqueue skipped: {error:#}");
            return;
        }
        let _ = tx.try_send(());
    }

    fn prepare_record(&self, event: BehaviorEvent, period_id: String) -> Result<OutboxRecord> {
        let event_id = event.event_id.clone();
        let mut value = serde_json::to_value(event).context("serialize behavior event")?;
        let object = value
            .as_object_mut()
            .context("behavior event must serialize as an object")?;
        object.insert("schema_version".to_string(), json!(1));
        object.insert("event_source".to_string(), json!("client"));
        object.insert("collection_period_id".to_string(), json!(period_id.clone()));
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
            collection_period_id: period_id,
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
            credential_gate: self.credential_gate.clone(),
            control_gate: self.control_gate.clone(),
            collecting: self.collecting.clone(),
            collection_period_id: self.collection_period_id.clone(),
            outbox: self.outbox.clone(),
            tasks: self.tasks.clone(),
            team_sessions: self.team_sessions.clone(),
            event_tx: None,
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
                    if let Ok(mut outbox) = telemetry.outbox.lock() {
                        if let Err(error) = outbox.apply_results(&batch, &results) {
                            log::debug!(
                                "[pinvou3][behavior] persist delivery results failed: {error:#}"
                            );
                        }
                    }
                    retry_delay = Duration::from_secs(1);
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
            "device_id": credentials.device_id,
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
        if !response.status().is_success() {
            anyhow::bail!("behavior events endpoint returned {}", response.status());
        }
        let response: EventUploadResponse = response
            .json()
            .await
            .context("parse behavior event delivery response")?;
        Ok(delivery_results(records, response))
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
            configured: self.runtime.enabled,
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
                )
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
        if enabled {
            if !self.runtime.enabled {
                anyhow::bail!("behavior telemetry service is not configured");
            }
            if self.collecting.load(Ordering::Acquire) {
                return Ok(self.status().await);
            }
            if self
                .state
                .lock()
                .await
                .as_ref()
                .is_some_and(|state| state.control.pending_request.is_some())
            {
                self.retry_pending_control().await?;
            }
            let credentials = self.ensure_credentials().await?;
            let request_id = token("control_", 18);
            let response: CollectionStartResponse = self
                .post_control(
                    &credentials,
                    "collection/start",
                    json!({ "request_id": request_id }),
                )
                .await?;
            let mut guard = self.state.lock().await;
            let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
            let emit_cohort = !state.control.cohort_started;
            state.control.enabled = true;
            state.control.collection_period_id = Some(response.collection_period_id);
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
                        .string("eligibility_source", "later_opt_in")
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
            self.collecting.store(false, Ordering::Release);
            if let Ok(mut period) = self.collection_period_id.write() {
                *period = None;
            }
            if let Ok(mut outbox) = self.outbox.lock() {
                outbox.clear()?;
            }
            if let Ok(mut tasks) = self.tasks.lock() {
                tasks.clear();
                persist_task_attempts(&tasks)?;
            }
            let mut guard = self.state.lock().await;
            if let Some(state) = guard.as_mut() {
                state.control.enabled = false;
                if let Some(period_id) = state.control.collection_period_id.clone() {
                    state.control.pending_request = Some(PendingControlRequest {
                        kind: PendingControlKind::End,
                        request_id: token("control_", 18),
                        collection_period_id: Some(period_id),
                    });
                }
                persist_state(state)?;
            }
            drop(guard);
            if let Err(error) = self.retry_pending_control().await {
                log::debug!("[pinvou3][behavior] collection withdrawal pending: {error:#}");
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
        Ok(self.status().await)
    }

    pub async fn delete_remote_data(&self) -> Result<BehaviorTelemetryStatus> {
        let _control_guard = self.control_gate.lock().await;
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
        let Some(credentials) = self.current_credentials().await else {
            let mut guard = self.state.lock().await;
            if let Some(state) = guard.as_mut() {
                state.control.enabled = false;
                state.control.collection_period_id = None;
                state.control.pending_request = None;
                persist_state(state)?;
            }
            drop(guard);
            return self.delete_local_data().await;
        };
        {
            let mut guard = self.state.lock().await;
            let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
            state.control.enabled = false;
            state.control.pending_request = Some(PendingControlRequest {
                kind: PendingControlKind::Delete,
                request_id: token("control_", 18),
                collection_period_id: state.control.collection_period_id.clone(),
            });
            persist_state(state)?;
        }
        if let Err(error) = self.send_pending_control(&credentials).await {
            log::debug!("[pinvou3][behavior] remote deletion pending: {error:#}");
        }
        self.delete_local_data().await
    }

    async fn retry_pending_control(&self) -> Result<()> {
        let pending = self
            .state
            .lock()
            .await
            .as_ref()
            .and_then(|state| state.control.pending_request.clone());
        if pending.is_none() {
            return Ok(());
        }
        let credentials = self
            .current_credentials()
            .await
            .context("credentials unavailable for pending behavior control")?;
        self.send_pending_control(&credentials).await
    }

    async fn send_pending_control(&self, credentials: &Credentials) -> Result<()> {
        let pending = self
            .state
            .lock()
            .await
            .as_ref()
            .and_then(|state| state.control.pending_request.clone())
            .context("no pending behavior control request")?;
        match pending.kind {
            PendingControlKind::End => {
                let _: CollectionEndResponse = self
                    .post_control(
                        credentials,
                        "collection/end",
                        json!({
                            "request_id": pending.request_id,
                            "collection_period_id": pending.collection_period_id,
                        }),
                    )
                    .await?;
                let mut guard = self.state.lock().await;
                if let Some(state) = guard.as_mut() {
                    state.control.collection_period_id = None;
                    state.control.pending_request = None;
                    persist_state(state)?;
                }
            }
            PendingControlKind::Delete => {
                let response: DeletionResponse = self
                    .post_control(
                        credentials,
                        "deletion",
                        json!({ "request_id": pending.request_id }),
                    )
                    .await?;
                let fresh = PersistedState::fresh(&self.runtime);
                let mut guard = self.state.lock().await;
                *guard = Some(PersistedState {
                    control: CollectionControl {
                        last_deletion_request_id: Some(response.deletion_request_id),
                        ..CollectionControl::default()
                    },
                    ..fresh
                });
                if let Some(state) = guard.as_ref() {
                    persist_state(state)?;
                }
                if let Ok(mut period) = self.collection_period_id.write() {
                    *period = None;
                }
            }
        }
        Ok(())
    }

    async fn post_control<T: for<'de> Deserialize<'de>>(
        &self,
        credentials: &Credentials,
        path: &str,
        mut body: Value,
    ) -> Result<T> {
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
        let response = self
            .client
            .post(format!("{base}/{path}"))
            .bearer_auth(&credentials.device_token)
            .json(&body)
            .send()
            .await
            .with_context(|| format!("send behavior control {path}"))?;
        if !response.status().is_success() {
            anyhow::bail!("behavior control {path} returned {}", response.status());
        }
        response
            .json()
            .await
            .with_context(|| format!("parse behavior control {path}"))
    }

    async fn ensure_credentials(&self) -> Result<Credentials> {
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
        if state.telemetry_base_url != self.runtime.telemetry_base_url
            || state.behavior_events_url != self.runtime.behavior_events_url
        {
            state = PersistedState::fresh(&self.runtime);
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
        if !response.status().is_success() {
            anyhow::bail!("telemetry registration returned {}", response.status());
        }
        let registered: RegistrationResponse = response
            .json()
            .await
            .context("parse telemetry registration response")?;
        state.device_id = Some(registered.device_id);
        state.device_token = Some(registered.device_token);
        persist_state(&state)?;
        let mut guard = self.state.lock().await;
        *guard = Some(state);
        drop(guard);
        self.current_credentials()
            .await
            .context("telemetry credentials missing after registration")
    }

    async fn current_credentials(&self) -> Option<Credentials> {
        let state = self.state.lock().await.clone()?;
        if state.telemetry_base_url != self.runtime.telemetry_base_url
            || state.behavior_events_url != self.runtime.behavior_events_url
        {
            return None;
        }
        let device_id = state.device_id?;
        let device_token = state.device_token?;
        Some(Credentials {
            device_id,
            device_token,
            behavior_events_url: state.behavior_events_url,
        })
    }
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

#[derive(Debug)]
struct Credentials {
    device_id: String,
    device_token: String,
    behavior_events_url: String,
}

impl RuntimeConfig {
    fn from_env() -> Self {
        let requested_enabled = std::env::var("PINVOU_BEHAVIOR_TELEMETRY_ENABLED")
            .ok()
            .is_some_and(|value| {
                matches!(
                    value.trim().to_ascii_lowercase().as_str(),
                    "1" | "true" | "yes" | "on"
                )
            });
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
            control: CollectionControl::default(),
        }
    }
}

fn load_state() -> Result<Option<PersistedState>> {
    let path = state_path();
    if !path.is_file() {
        return Ok(None);
    }
    let data = std::fs::read(&path).with_context(|| format!("read {}", path.display()))?;
    let mut state: PersistedState =
        serde_json::from_slice(&data).with_context(|| format!("parse {}", path.display()))?;
    if state.version > STATE_VERSION {
        return Ok(None);
    }
    state.version = STATE_VERSION;
    Ok(Some(state))
}

fn persist_state(state: &PersistedState) -> Result<()> {
    let path = state_path();
    let parent = path
        .parent()
        .with_context(|| format!("invalid behavior telemetry path: {}", path.display()))?;
    std::fs::create_dir_all(parent).with_context(|| format!("create {}", parent.display()))?;
    let data = serde_json::to_vec_pretty(state).context("serialize behavior telemetry state")?;
    write_private_file(&path, &data).with_context(|| format!("write {}", path.display()))?;
    Ok(())
}

fn write_private_file(path: &Path, data: &[u8]) -> std::io::Result<()> {
    let mut file = filesystem::create_secret_file(path)?;
    file.write_all(data)?;
    file.sync_all()
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
        if let Ok(mut sessions) = telemetry.team_sessions.lock() {
            sessions.insert(session_id.to_string());
        }
    }
}

pub fn is_team_session(app: &tauri::AppHandle, session_id: &str) -> bool {
    app.try_state::<BehaviorTelemetry>()
        .is_some_and(|telemetry| {
            telemetry
                .team_sessions
                .lock()
                .is_ok_and(|sessions| sessions.contains(session_id))
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
    };
    if let Some(telemetry) = app.try_state::<BehaviorTelemetry>() {
        if telemetry.collecting.load(Ordering::Acquire) {
            if let Ok(mut tasks) = telemetry.tasks.lock() {
                tasks.insert(session_id.to_string(), attempt.clone());
                let _ = persist_task_attempts(&tasks);
            }
            telemetry.track(task_fields(
                BehaviorEvent::new("task_submitted"),
                session_id,
                &attempt,
            ));
        }
    }
    TaskSubmissionGuard {
        app: app.clone(),
        session_id: session_id.to_string(),
        attempt_id: attempt.attempt_id,
        reject_on_drop: true,
    }
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
            attempt.attempt_id == attempt_id && attempt.admitted_at.is_none()
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
        telemetry.track(
            task_fields(BehaviorEvent::new("task_rejected"), session_id, &attempt)
                .string("error_category", error_category)
                .stage(stage),
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
        if attempt.admitted_at.is_some() {
            return None;
        }
        attempt.admitted_at = Some(admitted_at);
        attempt.turn_id = Some(turn_id.to_string());
        let result = attempt.clone();
        let _ = persist_task_attempts(&tasks);
        Some(result)
    });
    if let Some(attempt) = attempt {
        telemetry.track(
            task_fields(BehaviorEvent::new("task_admitted"), session_id, &attempt)
                .turn(turn_id)
                .integer(
                    "queue_duration_ms",
                    admitted_at.saturating_sub(attempt.submitted_at),
                ),
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
        if attempt.turn_id.as_deref() != Some(turn_id) || attempt.first_output_emitted {
            return None;
        }
        attempt.first_output_emitted = true;
        let result = attempt.clone();
        let _ = persist_task_attempts(&tasks);
        Some(result)
    });
    if let Some(attempt) = attempt {
        telemetry.track(
            task_fields(
                BehaviorEvent::new("task_first_output"),
                session_id,
                &attempt,
            )
            .turn(turn_id)
            .string("output_type", "text")
            .integer("duration_ms", now.saturating_sub(attempt.submitted_at)),
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
    if let Some(attempt) = attempt {
        if let Some(turn_id) = attempt.turn_id.as_deref() {
            telemetry.track(
                BehaviorEvent::new("task_cancel_requested")
                    .session(session_id)
                    .string("attempt_id", &attempt.attempt_id)
                    .turn(turn_id)
                    .stage(if attempt.first_output_emitted {
                        "output"
                    } else {
                        "model"
                    }),
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
    let attempt = telemetry.tasks.lock().ok().and_then(|mut tasks| {
        let removed = if tasks
            .get(session_id)
            .is_some_and(|attempt| attempt.turn_id.as_deref() == Some(turn_id))
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
        telemetry.track(event);
    }
}

pub fn error_category(error: Option<&str>) -> &'static str {
    let error = error.unwrap_or_default().to_ascii_lowercase();
    if error.contains("cancel") || error.contains("interrupt") {
        "user_cancelled"
    } else if error.contains("timeout") {
        "timeout"
    } else if error.contains("auth") || error.contains("401") || error.contains("403") {
        "authentication_failed"
    } else if error.contains("network") || error.contains("connect") {
        "network_unavailable"
    } else if error.contains("model") || error.contains("provider") {
        "model_error"
    } else {
        "internal_error"
    }
}

pub fn track_tool_call(
    app: &tauri::AppHandle,
    session_id: &str,
    turn_id: Option<&str>,
    tool_name: &str,
    success: bool,
) {
    let mut event = BehaviorEvent::new("tool_call_completed")
        .session(session_id)
        .tool(tool_name, tool_name, classify_tool(tool_name))
        .success(success);
    if let Some(turn_id) = turn_id {
        event = event.turn(turn_id);
    }
    track(app, event);
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
    let provider_key = model
        .vendor
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| model.preset.as_str())
        .to_ascii_lowercase();
    let provider_name = provider_key.clone();
    let model_id = model.model.trim();
    if model_id.is_empty() {
        return;
    }
    track(
        app,
        BehaviorEvent::new("model_used")
            .session(session_id)
            .turn(turn_id)
            .model(provider_key, provider_name, model_id, model_id),
    );
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

#[cfg(test)]
mod tests {
    use super::*;

    fn outbox_record(id: &str, period: &str, enqueued_at: u64) -> OutboxRecord {
        OutboxRecord {
            event_id: id.to_string(),
            collection_period_id: period.to_string(),
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
    fn public_builds_do_not_collect_without_explicit_control_state() {
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
        recovered.apply_results(&batch, &results).unwrap();
        assert_eq!(recovered.records.len(), 1);
        assert_eq!(recovered.records[0].event_id, "event_retry");
        assert_eq!(recovered.records[0].delivery_attempt_count, 1);
        assert_eq!(
            recovered.records[0].last_reason_code.as_deref(),
            Some("rate_limited")
        );
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

        outbox.retain_period(Some("different_period")).unwrap();
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
}
