//! B-side authoritative remote task owner.
//!
//! This module owns the remote-session mapping, the `task_id + client_request_id`
//! idempotency ledger, the per-session turn lock and the single dispatch
//! boundary. It never becomes a second source of truth: the authoritative
//! transcript keeps living in `SessionStore`, and this ledger only records what
//! the controlled device already committed to.
//!
//! Invariants enforced here:
//!
//! - every admitted task is bound to the exact live
//!   `connection_id + controller_lease_id + lease_epoch + relay_epoch + remote_control_generation`;
//! - one remote session admits at most one task at a time;
//! - a repeated `client_request_id` with identical parameters replays the
//!   existing record, and with different parameters fails closed;
//! - every task ends in exactly one terminal state, and a terminal record is
//!   never resurrected;
//! - a task whose dispatch was already committed but whose outcome cannot be
//!   proven reports the wire status `result_unknown` while still committing the
//!   authoritative terminal state `interrupted`.
//!
//! Prompts, workspace paths, credentials and Relay identities never enter the
//! persisted ledger: only opaque IDs, digests and bounded reason codes do.

use std::collections::{HashMap, HashSet};
use std::fmt;
use std::path::{Path, PathBuf};
use std::sync::Arc;

pub(crate) use super::transfer::upload::PublishedAttachment;
use async_trait::async_trait;
use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use super::journal::JournalKind;
use super::protocol::LeaseContext;

pub(crate) const TASK_MODE_CHAT: &str = "chat";
pub(crate) const TASK_MODE_CODE: &str = "code";
pub(crate) const TASK_MODEL_SOURCE_CONTROLLED: &str = "controlled_device";
pub(crate) const TASK_MODEL_SOURCE_TEMPORARY: &str = "controller_temporary";

pub(crate) const TASK_STATUS_ADMITTED: &str = "admitted";
pub(crate) const TASK_STATUS_RUNNING: &str = "running";
pub(crate) const TASK_STATUS_WAITING_PERMISSION: &str = "waiting_permission";
pub(crate) const TASK_STATUS_SUCCEEDED: &str = "succeeded";
pub(crate) const TASK_STATUS_FAILED: &str = "failed";
pub(crate) const TASK_STATUS_CANCELLED: &str = "cancelled";
pub(crate) const TASK_STATUS_INTERRUPTED: &str = "interrupted";
pub(crate) const TASK_STATUS_RESULT_UNKNOWN: &str = "result_unknown";

/// Executors return this bounded code when a turn ended because the caller
/// cancelled it. Anything else is reported as a plain failure.
pub(crate) const TASK_CANCELLED_CODE: &str = "task_cancelled";

const LEDGER_FILE_VERSION: u8 = 1;
const MAX_TRACKED_TASKS: usize = 256;
const MAX_LEDGER_FILE_BYTES: u64 = 2 * 1024 * 1024;
const MAX_LIVE_TASKS: usize = 64;
const MAX_ID_CHARS: usize = 128;
const MAX_REASON_CHARS: usize = 64;
const MAX_RESULT_DIGEST_CHARS: usize = 64;
const MAX_SEQUENCE_DIGITS: usize = 20;
const FINGERPRINT_DOMAIN: &[u8] = b"pinvou3/remote-task-request/v1\0";
const TASK_INPUT_DOMAIN: &[u8] = b"pinvou-controller-task-input-v1";

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct RemoteTaskError {
    pub(crate) code: &'static str,
}

impl RemoteTaskError {
    fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl fmt::Display for RemoteTaskError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for RemoteTaskError {}

type Result<T> = std::result::Result<T, RemoteTaskError>;

fn error(code: &'static str) -> RemoteTaskError {
    RemoteTaskError::new(code)
}

/// Frozen extension boundary between the controller transport and task
/// execution (declared by DEV-07, consumed by the DEV-13 adapters).
pub(crate) trait ControllerTaskEventSink: Send + Sync {
    fn emit(&self, event: Value);

    /// E2E-05：执行器产出的正文类事件（用户消息 / assistant 回复 / 推理等）。
    /// 默认忽略；命令层实现为「写权威 journal + 经加密通道回传 A」。
    fn emit_journal(
        &self,
        _kind: super::journal::JournalKind,
        _session_id: &str,
        _task_id: Option<&str>,
        _payload: Value,
    ) {
    }
}

#[async_trait]
pub(crate) trait ControllerTaskExecutor: Send + Sync {
    async fn execute(
        &self,
        request: Value,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String>;

    /// The default cannot silently ignore a published B-local attachment.
    /// Host executors opt in only after installing it into their real turn.
    async fn execute_with_attachments(
        &self,
        request: Value,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String> {
        if !attachments.is_empty() {
            return Err("task_attachments_unavailable".to_owned());
        }
        self.execute(request, events, cancel).await
    }
}

/// Serializable projection of the live lease context. The wire `LeaseContext`
/// stays owned by the protocol module; the ledger only stores this copy.
#[derive(Debug, Clone, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub(crate) struct TaskLeaseBinding {
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: String,
    pub(crate) relay_epoch: String,
    pub(crate) remote_control_generation: String,
}

impl TaskLeaseBinding {
    pub(crate) fn from_context(context: &LeaseContext) -> Self {
        Self {
            connection_id: context.connection_id.clone(),
            controller_lease_id: context.controller_lease_id.clone(),
            lease_epoch: context.lease_epoch.clone(),
            relay_epoch: context.relay_epoch.clone(),
            remote_control_generation: context.remote_control_generation.clone(),
        }
    }

    fn validate(&self) -> Result<()> {
        validate_identifier(&self.connection_id)?;
        validate_identifier(&self.controller_lease_id)?;
        validate_identifier(&self.lease_epoch)?;
        validate_identifier(&self.relay_epoch)?;
        validate_identifier(&self.remote_control_generation)?;
        Ok(())
    }
}

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum RemoteTaskState {
    Admitted,
    Running,
    WaitingPermission,
    Succeeded,
    Failed,
    Cancelled,
    Interrupted,
}

impl RemoteTaskState {
    pub(crate) fn wire_status(self) -> &'static str {
        match self {
            Self::Admitted => TASK_STATUS_ADMITTED,
            Self::Running => TASK_STATUS_RUNNING,
            Self::WaitingPermission => TASK_STATUS_WAITING_PERMISSION,
            Self::Succeeded => TASK_STATUS_SUCCEEDED,
            Self::Failed => TASK_STATUS_FAILED,
            Self::Cancelled => TASK_STATUS_CANCELLED,
            Self::Interrupted => TASK_STATUS_INTERRUPTED,
        }
    }

    pub(crate) fn is_terminal(self) -> bool {
        matches!(
            self,
            Self::Succeeded | Self::Failed | Self::Cancelled | Self::Interrupted
        )
    }
}

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum RemoteTaskTerminal {
    Succeeded,
    Failed,
    Cancelled,
    Interrupted,
}

impl RemoteTaskTerminal {
    pub(crate) fn state(self) -> RemoteTaskState {
        match self {
            Self::Succeeded => RemoteTaskState::Succeeded,
            Self::Failed => RemoteTaskState::Failed,
            Self::Cancelled => RemoteTaskState::Cancelled,
            Self::Interrupted => RemoteTaskState::Interrupted,
        }
    }

    pub(crate) fn reason(self) -> &'static str {
        match self {
            Self::Succeeded => "completed",
            Self::Failed => "executor_error",
            Self::Cancelled => "cancelled",
            Self::Interrupted => "interrupted",
        }
    }
}

/// Admission request derived from a validated `task_submit` payload.
///
/// `input` carries the prompt for dispatch and is deliberately kept out of the
/// persisted record and out of `Debug` output.
#[derive(Clone)]
pub(crate) struct RemoteTaskRequest {
    pub(crate) lease: TaskLeaseBinding,
    pub(crate) task_id: String,
    pub(crate) client_request_id: String,
    pub(crate) session_id: String,
    pub(crate) capability_revision: String,
    pub(crate) mode: String,
    pub(crate) workspace_id: Option<String>,
    pub(crate) model_source: String,
    pub(crate) input_digest: String,
    pub(crate) input: Value,
}

impl fmt::Debug for RemoteTaskRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RemoteTaskRequest")
            .field("task_id", &self.task_id)
            .field("session_id", &self.session_id)
            .field("mode", &self.mode)
            .field("model_source", &self.model_source)
            .finish()
    }
}

impl RemoteTaskRequest {
    pub(crate) fn from_payload(payload: &Value) -> Result<Self> {
        let object = payload
            .as_object()
            .ok_or_else(|| error("task_request_invalid"))?;
        let text = |field: &'static str| -> Result<String> {
            object
                .get(field)
                .and_then(Value::as_str)
                .map(str::to_owned)
                .ok_or_else(|| error("task_request_invalid"))
        };
        let input = object
            .get("input")
            .cloned()
            .ok_or_else(|| error("task_request_invalid"))?;
        let workspace_id = input
            .get("workspace_id")
            .and_then(Value::as_str)
            .filter(|value| !value.is_empty())
            .map(str::to_owned);
        let request = Self {
            lease: TaskLeaseBinding {
                connection_id: text("connection_id")?,
                controller_lease_id: text("controller_lease_id")?,
                lease_epoch: text("lease_epoch")?,
                relay_epoch: text("relay_epoch")?,
                remote_control_generation: text("remote_control_generation")?,
            },
            task_id: text("task_id")?,
            client_request_id: text("client_request_id")?,
            session_id: text("session_id")?,
            capability_revision: text("capability_revision")?,
            mode: text("mode")?,
            workspace_id,
            model_source: text("model_source")?,
            input_digest: text("input_digest")?,
            input,
        };
        request.validate()?;
        Ok(request)
    }

    pub(crate) fn validate(&self) -> Result<()> {
        self.validate_identity()?;
        if task_input_digest_for_value(&self.input)? != self.input_digest {
            return Err(error("task_input_digest_invalid"));
        }
        self.validate_execution_parameters()
    }

    fn validate_identity(&self) -> Result<()> {
        self.lease.validate()?;
        validate_identifier(&self.task_id)?;
        validate_identifier(&self.client_request_id)?;
        validate_identifier(&self.session_id)?;
        if self.capability_revision.parse::<u64>().is_err()
            || self.capability_revision.len() > MAX_SEQUENCE_DIGITS
        {
            return Err(error("task_request_invalid"));
        }
        validate_digest(&self.input_digest)?;
        Ok(())
    }

    fn validate_execution_parameters(&self) -> Result<()> {
        match self.mode.as_str() {
            TASK_MODE_CHAT => {
                if self.workspace_id.is_some() {
                    return Err(error("task_workspace_unexpected"));
                }
            }
            TASK_MODE_CODE => match self.workspace_id.as_deref() {
                None | Some("") => return Err(error("task_workspace_required")),
                Some(workspace_id) => validate_identifier(workspace_id)?,
            },
            _ => return Err(error("task_mode_invalid")),
        }
        if !matches!(
            self.model_source.as_str(),
            TASK_MODEL_SOURCE_CONTROLLED | TASK_MODEL_SOURCE_TEMPORARY
        ) {
            return Err(error("task_model_source_invalid"));
        }
        Ok(())
    }

    /// Durable records contain only bounded identity, parameters and digests;
    /// never restore or manufacture a plaintext input to recompute its hash.
    /// Incoming admissions still use `validate` with the actual input above.
    fn validate_persisted_metadata(&self) -> Result<()> {
        self.validate_identity()?;
        self.validate_execution_parameters()
    }

    pub(crate) fn skill_ids(&self) -> Result<Vec<String>> {
        extract_skill_ids_from_input(&self.input).map_err(|_| error("task_skill_invalid"))
    }

    /// Stable fingerprint of every request-determining parameter. The A-side
    /// request ID is *not* part of it: the same ID with different parameters
    /// must be rejected instead of silently replayed.
    pub(crate) fn fingerprint(&self) -> String {
        let mut hasher = Sha256::new();
        hasher.update(FINGERPRINT_DOMAIN);
        for part in [
            self.lease.connection_id.as_str(),
            self.lease.controller_lease_id.as_str(),
            self.lease.lease_epoch.as_str(),
            self.lease.relay_epoch.as_str(),
            self.lease.remote_control_generation.as_str(),
            self.task_id.as_str(),
            self.client_request_id.as_str(),
            self.session_id.as_str(),
            self.capability_revision.as_str(),
            self.mode.as_str(),
            self.workspace_id.as_deref().unwrap_or(""),
            self.model_source.as_str(),
            self.input_digest.as_str(),
        ] {
            hasher.update(part.as_bytes());
            hasher.update([0u8]);
        }
        hex_lower(&hasher.finalize())
    }
}

#[derive(Debug, Clone, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub(crate) struct RemoteTaskRecord {
    pub(crate) sequence: u64,
    pub(crate) transition_sequence: u64,
    pub(crate) lease: TaskLeaseBinding,
    pub(crate) task_id: String,
    pub(crate) client_request_id: String,
    pub(crate) session_id: String,
    pub(crate) capability_revision: String,
    pub(crate) mode: String,
    pub(crate) workspace_id: Option<String>,
    pub(crate) model_source: String,
    pub(crate) input_digest: String,
    pub(crate) fingerprint: String,
    pub(crate) state: RemoteTaskState,
    pub(crate) dispatch_committed: bool,
    pub(crate) outcome_unknown: bool,
    pub(crate) cancel_requested: bool,
    pub(crate) terminal_reason: Option<String>,
    pub(crate) result_digest: Option<String>,
    pub(crate) admitted_at_ms: u64,
    pub(crate) updated_at_ms: u64,
}

impl RemoteTaskRecord {
    pub(crate) fn is_terminal(&self) -> bool {
        self.state.is_terminal()
    }

    /// Wire status reported to A. A committed-but-unprovable outcome is
    /// reported as `result_unknown` while the ledger keeps the authoritative
    /// terminal state `interrupted`.
    pub(crate) fn wire_status(&self) -> &'static str {
        if self.is_terminal() && self.outcome_unknown {
            TASK_STATUS_RESULT_UNKNOWN
        } else {
            self.state.wire_status()
        }
    }
}

/// Durable ledger sink. `persist` runs inside the ledger critical section and
/// must be atomic: a failure must leave the previous durable state intact.
pub(crate) trait RemoteTaskLedgerStore: Send + Sync {
    fn persist(&self, records: &[RemoteTaskRecord]) -> std::result::Result<(), String>;
}

#[derive(Default)]
pub(crate) struct InMemoryTaskLedgerStore;

impl RemoteTaskLedgerStore for InMemoryTaskLedgerStore {
    fn persist(&self, _records: &[RemoteTaskRecord]) -> std::result::Result<(), String> {
        Ok(())
    }
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct LedgerFile {
    version: u8,
    tasks: Vec<RemoteTaskRecord>,
}

/// Atomic JSON ledger file. Each write replaces the file with a complete
/// snapshot of the tracked tasks, so a crash can only lose the newest
/// transition and never produce a torn record.
pub(crate) struct JsonTaskLedgerStore {
    path: PathBuf,
}

impl JsonTaskLedgerStore {
    pub(crate) fn new(path: PathBuf) -> Self {
        Self { path }
    }

    pub(crate) fn load(path: &Path) -> std::result::Result<Vec<RemoteTaskRecord>, String> {
        use std::io::Read as _;
        let parent = path.parent().ok_or("task_ledger_unreadable")?;
        let directory =
            match crate::platform::filesystem::open_existing_private_file_directory(parent) {
                Ok(Some(directory)) => directory,
                Ok(None) => return Ok(Vec::new()),
                Err(_) => return Err("task_ledger_unreadable".to_owned()),
            };
        let file =
            match directory.open_plain_file(path.file_name().ok_or("task_ledger_unreadable")?) {
                Ok(Some(file)) => file,
                Ok(None) => return Ok(Vec::new()),
                Err(_) => return Err("task_ledger_unreadable".to_owned()),
            };
        let mut bytes = Vec::new();
        file.take(MAX_LEDGER_FILE_BYTES + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| "task_ledger_unreadable".to_owned())?;
        if bytes.len() as u64 > MAX_LEDGER_FILE_BYTES {
            return Err("task_ledger_capacity".to_owned());
        }
        let file: LedgerFile =
            serde_json::from_slice(&bytes).map_err(|_| "task_ledger_corrupt".to_owned())?;
        if file.version != LEDGER_FILE_VERSION {
            return Err("task_ledger_version_unsupported".to_owned());
        }
        Ok(file.tasks)
    }
}

impl RemoteTaskLedgerStore for JsonTaskLedgerStore {
    fn persist(&self, records: &[RemoteTaskRecord]) -> std::result::Result<(), String> {
        if records.len() > MAX_TRACKED_TASKS {
            return Err("task_ledger_capacity".to_owned());
        }
        let file = LedgerFile {
            version: LEDGER_FILE_VERSION,
            tasks: records.to_vec(),
        };
        let payload = serde_json::to_vec_pretty(&file)
            .map_err(|_| "task_ledger_serialize_failed".to_owned())?;
        if payload.len() as u64 > MAX_LEDGER_FILE_BYTES {
            return Err("task_ledger_capacity".to_owned());
        }
        crate::platform::filesystem::atomic_write_private_anchored(&self.path, &payload)
            .map_err(|_| "task_ledger_write_failed".to_owned())
    }
}

#[derive(Default, Clone)]
struct LedgerInner {
    tasks: HashMap<String, RemoteTaskRecord>,
    by_client_request: HashMap<String, String>,
    turn_locks: HashMap<String, String>,
    order: Vec<String>,
    sequence: u64,
}

pub(crate) struct RemoteTaskTicket {
    pub(crate) record: RemoteTaskRecord,
    pub(crate) input: Value,
}

impl fmt::Debug for RemoteTaskTicket {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RemoteTaskTicket")
            .field("record", &self.record)
            .finish_non_exhaustive()
    }
}

#[derive(Debug)]
pub(crate) enum AdmissionOutcome {
    /// The task was admitted and the caller owns the only dispatch ticket.
    Admitted(RemoteTaskTicket),
    /// The exact same request was already admitted; nothing else happens.
    Replay(RemoteTaskRecord),
}

/// Callback-free durable decision, finalized after the lifecycle lock is
/// released. Installation failures are already terminal in the ledger.
pub(crate) enum PreparedTaskAdmission {
    Accepted(AdmissionOutcome),
    InstallationFailed(RemoteTaskRecord, &'static str),
}

#[derive(Debug)]
pub(crate) enum CancelOutcome {
    /// Dispatch had not been committed, so the operation is rejected outright.
    CancelledBeforeDispatch(RemoteTaskRecord),
    /// Dispatch was already committed; the executor must confirm the terminal.
    CancellationRequested(RemoteTaskRecord),
    AlreadyTerminal(RemoteTaskRecord),
}

pub(crate) struct RemoteTaskLedger {
    inner: Mutex<LedgerInner>,
    store: Arc<dyn RemoteTaskLedgerStore>,
}

impl RemoteTaskLedger {
    pub(crate) fn new(store: Arc<dyn RemoteTaskLedgerStore>) -> Self {
        Self {
            inner: Mutex::new(LedgerInner::default()),
            store,
        }
    }

    pub(crate) fn in_memory() -> Self {
        Self::new(Arc::new(InMemoryTaskLedgerStore))
    }

    /// Open a fresh process owner and durably close every interrupted task
    /// before exposing the runtime. Never replace a corrupt/unwritable ledger
    /// with volatile state. The transcript remains owned by SessionStore.
    pub(crate) fn open_recovered(path: PathBuf, now_b: u64) -> Result<Self> {
        let records = JsonTaskLedgerStore::load(&path).map_err(|code| {
            error(match code.as_str() {
                "task_ledger_corrupt" => "task_ledger_corrupt",
                "task_ledger_capacity" => "task_ledger_capacity",
                "task_ledger_version_unsupported" => "task_ledger_version_unsupported",
                _ => "task_ledger_unreadable",
            })
        })?;
        let ledger = Self::restore(Arc::new(JsonTaskLedgerStore::new(path)), records)?;
        ledger.recover_durable(now_b)?;
        Ok(ledger)
    }

    fn recover_durable(&self, now_b: u64) -> Result<()> {
        {
            let mut inner = self.inner.lock();
            let mut recovered = inner.clone();
            for record in recovered
                .tasks
                .values_mut()
                .filter(|record| !record.is_terminal())
            {
                record.transition_sequence = record
                    .transition_sequence
                    .checked_add(1)
                    .ok_or_else(|| error("task_ledger_capacity"))?;
                record.state = RemoteTaskState::Interrupted;
                record.terminal_reason = Some("process_restart".to_owned());
                record.outcome_unknown = record.dispatch_committed;
                record.updated_at_ms = now_b;
            }
            recovered.turn_locks.clear();
            self.persist_inner(&recovered)?;
            *inner = recovered;
        }
        Ok(())
    }

    pub(crate) fn restore(
        store: Arc<dyn RemoteTaskLedgerStore>,
        mut records: Vec<RemoteTaskRecord>,
    ) -> Result<Self> {
        let mut inner = LedgerInner::default();
        if records.len() > MAX_TRACKED_TASKS {
            return Err(error("task_ledger_capacity"));
        }
        records.sort_by_key(|record| record.sequence);
        let mut sequences = HashSet::new();
        for record in records {
            let request = RemoteTaskRequest {
                lease: record.lease.clone(),
                task_id: record.task_id.clone(),
                client_request_id: record.client_request_id.clone(),
                session_id: record.session_id.clone(),
                capability_revision: record.capability_revision.clone(),
                mode: record.mode.clone(),
                workspace_id: record.workspace_id.clone(),
                model_source: record.model_source.clone(),
                input_digest: record.input_digest.clone(),
                input: Value::Null,
            };
            if request.validate_persisted_metadata().is_err()
                || request.fingerprint() != record.fingerprint
                || record.sequence == 0
                || record.transition_sequence == 0
                || !sequences.insert(record.sequence)
                || inner.tasks.contains_key(&record.task_id)
                || inner
                    .by_client_request
                    .contains_key(&record.client_request_id)
                || record
                    .result_digest
                    .as_deref()
                    .is_some_and(|digest| validate_digest(digest).is_err())
                || record
                    .terminal_reason
                    .as_deref()
                    .is_some_and(|reason| bounded_reason(reason) != reason)
                || (record.outcome_unknown && (!record.is_terminal() || !record.dispatch_committed))
                || (record.outcome_unknown && record.state != RemoteTaskState::Interrupted)
                || (!record.is_terminal()
                    && (record.terminal_reason.is_some() || record.result_digest.is_some()))
                || (record.is_terminal() && record.terminal_reason.is_none())
                || (matches!(
                    record.state,
                    RemoteTaskState::Running | RemoteTaskState::WaitingPermission
                ) && !record.dispatch_committed)
                || (record.state == RemoteTaskState::Admitted && record.dispatch_committed)
            {
                return Err(error("task_ledger_corrupt"));
            }
            inner.sequence = inner.sequence.max(record.sequence);
            inner
                .by_client_request
                .insert(record.client_request_id.clone(), record.task_id.clone());
            if !record.is_terminal()
                && inner
                    .turn_locks
                    .insert(record.session_id.clone(), record.task_id.clone())
                    .is_some()
            {
                return Err(error("task_ledger_corrupt"));
            }
            inner.order.push(record.task_id.clone());
            inner.tasks.insert(record.task_id.clone(), record);
        }
        if live_task_count(&inner) > MAX_LIVE_TASKS {
            return Err(error("task_ledger_capacity"));
        }
        Ok(Self {
            inner: Mutex::new(inner),
            store,
        })
    }

    pub(crate) fn admit(
        &self,
        live: &LeaseContext,
        request: &RemoteTaskRequest,
        now_b: u64,
    ) -> Result<AdmissionOutcome> {
        request.validate()?;
        if request.lease != TaskLeaseBinding::from_context(live) {
            return Err(error("task_binding_stale"));
        }
        let fingerprint = request.fingerprint();
        let mut inner = self.inner.lock();
        if let Some(existing) = inner.by_client_request.get(&request.client_request_id) {
            let record = inner
                .tasks
                .get(existing)
                .cloned()
                .ok_or_else(|| error("task_ledger_corrupt"))?;
            if record.fingerprint != fingerprint {
                return Err(error("task_request_conflict"));
            }
            return Ok(AdmissionOutcome::Replay(record));
        }
        if inner.tasks.contains_key(&request.task_id) {
            return Err(error("task_request_conflict"));
        }
        if inner.turn_locks.contains_key(&request.session_id) {
            return Err(error("task_session_busy"));
        }
        let mut candidate = inner.clone();
        prune_terminal(&mut candidate);
        if candidate.tasks.len() >= MAX_TRACKED_TASKS
            || live_task_count(&candidate) >= MAX_LIVE_TASKS
        {
            return Err(error("task_ledger_capacity"));
        }
        candidate.sequence = candidate
            .sequence
            .checked_add(1)
            .ok_or_else(|| error("task_ledger_capacity"))?;
        let record = RemoteTaskRecord {
            sequence: candidate.sequence,
            transition_sequence: 1,
            lease: request.lease.clone(),
            task_id: request.task_id.clone(),
            client_request_id: request.client_request_id.clone(),
            session_id: request.session_id.clone(),
            capability_revision: request.capability_revision.clone(),
            mode: request.mode.clone(),
            workspace_id: request.workspace_id.clone(),
            model_source: request.model_source.clone(),
            input_digest: request.input_digest.clone(),
            fingerprint,
            state: RemoteTaskState::Admitted,
            dispatch_committed: false,
            outcome_unknown: false,
            cancel_requested: false,
            terminal_reason: None,
            result_digest: None,
            admitted_at_ms: now_b,
            updated_at_ms: now_b,
        };
        candidate
            .turn_locks
            .insert(record.session_id.clone(), record.task_id.clone());
        candidate
            .by_client_request
            .insert(record.client_request_id.clone(), record.task_id.clone());
        candidate.order.push(record.task_id.clone());
        candidate
            .tasks
            .insert(record.task_id.clone(), record.clone());
        self.persist_inner(&candidate)?;
        *inner = candidate;
        Ok(AdmissionOutcome::Admitted(RemoteTaskTicket {
            record,
            input: request.input.clone(),
        }))
    }

    /// Commit the dispatch boundary. After this succeeds, B "stop"/"cancel" may
    /// no longer claim that the operation never ran.
    pub(crate) fn begin_dispatch(&self, task_id: &str, now_b: u64) -> Result<RemoteTaskRecord> {
        self.transition(task_id, RemoteTaskState::Running, now_b)
    }

    pub(crate) fn transition(
        &self,
        task_id: &str,
        next: RemoteTaskState,
        now_b: u64,
    ) -> Result<RemoteTaskRecord> {
        let mut inner = self.inner.lock();
        let previous = self.record_locked(&inner, task_id)?;
        if previous.is_terminal() {
            return Err(error("task_state_conflict"));
        }
        let allowed = matches!(
            (previous.state, next),
            (RemoteTaskState::Admitted, RemoteTaskState::Running)
                | (RemoteTaskState::Running, RemoteTaskState::WaitingPermission)
                | (RemoteTaskState::WaitingPermission, RemoteTaskState::Running)
        );
        if !allowed {
            return Err(error("task_state_conflict"));
        }
        let mut updated = previous.clone();
        updated.state = next;
        updated.updated_at_ms = now_b;
        if next == RemoteTaskState::Running && previous.state == RemoteTaskState::Admitted {
            updated.dispatch_committed = true;
        }
        self.commit(&mut inner, updated)
    }

    pub(crate) fn complete(
        &self,
        task_id: &str,
        terminal: RemoteTaskTerminal,
        result_digest: Option<String>,
        now_b: u64,
    ) -> Result<RemoteTaskRecord> {
        self.complete_with_reason(task_id, terminal, None, result_digest, now_b)
    }

    fn complete_with_reason(
        &self,
        task_id: &str,
        terminal: RemoteTaskTerminal,
        terminal_reason: Option<String>,
        result_digest: Option<String>,
        now_b: u64,
    ) -> Result<RemoteTaskRecord> {
        if let Some(digest) = result_digest.as_deref() {
            if digest.len() > MAX_RESULT_DIGEST_CHARS || !is_lower_hex(digest) {
                return Err(error("task_result_digest_invalid"));
            }
        }
        let mut inner = self.inner.lock();
        let previous = self.record_locked(&inner, task_id)?;
        if previous.is_terminal() {
            // Exactly one terminal state: repeated terminal reports are
            // idempotent and never overwrite the committed outcome.
            return Ok(previous);
        }
        let mut updated = previous.clone();
        updated.state = terminal.state();
        updated.terminal_reason = Some(
            terminal_reason
                .map(bounded_reason_owned)
                .unwrap_or_else(|| terminal.reason().to_owned()),
        );
        updated.result_digest = result_digest;
        updated.outcome_unknown =
            terminal == RemoteTaskTerminal::Interrupted && previous.outcome_unknown;
        updated.updated_at_ms = now_b;
        let updated = self.commit(&mut inner, updated)?;
        inner.turn_locks.remove(&updated.session_id);
        Ok(updated)
    }

    #[cfg(test)]
    pub(crate) fn request_cancel(&self, task_id: &str, now_b: u64) -> Result<CancelOutcome> {
        self.request_cancel_matching(task_id, now_b, None)
    }

    pub(crate) fn request_cancel_bound(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        now_b: u64,
    ) -> Result<CancelOutcome> {
        self.request_cancel_matching(task_id, now_b, Some((lease, session_id)))
    }

    fn request_cancel_matching(
        &self,
        task_id: &str,
        now_b: u64,
        binding: Option<(&LeaseContext, &str)>,
    ) -> Result<CancelOutcome> {
        let mut inner = self.inner.lock();
        let previous = self.record_locked(&inner, task_id)?;
        if binding.is_some_and(|(lease, session_id)| {
            previous.lease != TaskLeaseBinding::from_context(lease)
                || previous.session_id != session_id
        }) {
            return Err(error("task_binding_stale"));
        }
        if previous.is_terminal() {
            return Ok(CancelOutcome::AlreadyTerminal(previous));
        }
        if !previous.dispatch_committed {
            let mut updated = previous.clone();
            updated.state = RemoteTaskState::Cancelled;
            updated.terminal_reason = Some(RemoteTaskTerminal::Cancelled.reason().to_owned());
            updated.cancel_requested = true;
            updated.updated_at_ms = now_b;
            let updated = self.commit(&mut inner, updated)?;
            inner.turn_locks.remove(&updated.session_id);
            return Ok(CancelOutcome::CancelledBeforeDispatch(updated));
        }
        let mut updated = previous.clone();
        updated.cancel_requested = true;
        updated.updated_at_ms = now_b;
        let updated = self.commit(&mut inner, updated)?;
        Ok(CancelOutcome::CancellationRequested(updated))
    }

    /// Interrupt every non-terminal task of one `remote_control_generation`.
    /// Tasks whose dispatch was already committed keep `dispatch_committed` and
    /// report `result_unknown` instead of pretending the operation never ran.
    pub(crate) fn interrupt_generation(
        &self,
        generation: &str,
        reason: &str,
        now_b: u64,
    ) -> Vec<RemoteTaskRecord> {
        self.interrupt_where(
            |record| record.lease.remote_control_generation == generation,
            reason,
            now_b,
        )
    }

    pub(crate) fn interrupt_all(&self, reason: &str, now_b: u64) -> Vec<RemoteTaskRecord> {
        self.interrupt_where(|_| true, reason, now_b)
    }

    /// Called once after a process restart. Every non-terminal record is a task
    /// whose executor is gone, so it must reach a provable terminal state.
    pub(crate) fn recover_after_restart(&self, now_b: u64) -> Vec<RemoteTaskRecord> {
        let mut inner = self.inner.lock();
        let ids = self.non_terminal_ids(&inner, |_| true);
        let mut recovered = Vec::new();
        for id in ids {
            let Some(previous) = inner.tasks.get(&id).cloned() else {
                continue;
            };
            let mut updated = previous.clone();
            updated.state = RemoteTaskState::Interrupted;
            updated.terminal_reason = Some("process_restart".to_owned());
            updated.outcome_unknown = previous.dispatch_committed;
            updated.updated_at_ms = now_b;
            if let Ok(updated) = self.commit(&mut inner, updated) {
                inner.turn_locks.remove(&updated.session_id);
                recovered.push(updated);
            }
        }
        recovered
    }

    pub(crate) fn record(&self, task_id: &str) -> Result<RemoteTaskRecord> {
        let inner = self.inner.lock();
        self.record_locked(&inner, task_id)
    }

    pub(crate) fn records(&self) -> Vec<RemoteTaskRecord> {
        let inner = self.inner.lock();
        let mut records: Vec<RemoteTaskRecord> = inner.tasks.values().cloned().collect();
        records.sort_by_key(|record| record.sequence);
        records
    }

    pub(crate) fn active_for_session(&self, session_id: &str) -> Option<RemoteTaskRecord> {
        let inner = self.inner.lock();
        inner
            .turn_locks
            .get(session_id)
            .and_then(|task_id| inner.tasks.get(task_id))
            .cloned()
    }

    pub(crate) fn live_task_count(&self) -> usize {
        let inner = self.inner.lock();
        live_task_count(&inner)
    }

    fn non_terminal_ids(
        &self,
        inner: &LedgerInner,
        predicate: impl Fn(&RemoteTaskRecord) -> bool,
    ) -> Vec<String> {
        inner
            .order
            .iter()
            .filter(|id| {
                inner
                    .tasks
                    .get(*id)
                    .is_some_and(|record| !record.is_terminal() && predicate(record))
            })
            .cloned()
            .collect()
    }

    fn interrupt_where(
        &self,
        predicate: impl Fn(&RemoteTaskRecord) -> bool,
        reason: &str,
        now_b: u64,
    ) -> Vec<RemoteTaskRecord> {
        let mut inner = self.inner.lock();
        let ids = self.non_terminal_ids(&inner, predicate);
        let mut interrupted = Vec::new();
        for id in ids {
            let Some(previous) = inner.tasks.get(&id).cloned() else {
                continue;
            };
            let mut updated = previous.clone();
            updated.state = RemoteTaskState::Interrupted;
            updated.terminal_reason = Some(bounded_reason(reason));
            updated.outcome_unknown = previous.dispatch_committed;
            updated.updated_at_ms = now_b;
            if let Ok(updated) = self.commit(&mut inner, updated) {
                inner.turn_locks.remove(&updated.session_id);
                interrupted.push(updated);
            }
        }
        interrupted
    }

    fn record_locked(&self, inner: &LedgerInner, task_id: &str) -> Result<RemoteTaskRecord> {
        inner
            .tasks
            .get(task_id)
            .cloned()
            .ok_or_else(|| error("task_not_found"))
    }

    fn commit(
        &self,
        inner: &mut LedgerInner,
        mut updated: RemoteTaskRecord,
    ) -> Result<RemoteTaskRecord> {
        if let Some(previous) = inner.tasks.get(&updated.task_id) {
            updated.transition_sequence = previous
                .transition_sequence
                .checked_add(1)
                .ok_or_else(|| error("task_ledger_capacity"))?;
        }
        let mut candidate = inner.clone();
        candidate
            .tasks
            .insert(updated.task_id.clone(), updated.clone());
        self.persist_inner(&candidate)?;
        *inner = candidate;
        Ok(updated)
    }

    fn persist_inner(&self, inner: &LedgerInner) -> Result<()> {
        let mut records: Vec<_> = inner.tasks.values().cloned().collect();
        records.sort_by_key(|record| record.sequence);
        self.store
            .persist(&records)
            .map_err(|_| error("task_ledger_persist_failed"))
    }
}

fn live_task_count(inner: &LedgerInner) -> usize {
    inner
        .tasks
        .values()
        .filter(|record| !record.is_terminal())
        .count()
}

fn prune_terminal(inner: &mut LedgerInner) {
    if inner.tasks.len() < MAX_TRACKED_TASKS {
        return;
    }
    let mut removed = Vec::new();
    for id in inner.order.iter() {
        if inner.tasks.len() - removed.len() < MAX_TRACKED_TASKS {
            break;
        }
        if inner
            .tasks
            .get(id)
            .is_some_and(RemoteTaskRecord::is_terminal)
        {
            removed.push(id.clone());
        }
    }
    for id in removed {
        if let Some(record) = inner.tasks.remove(&id) {
            if inner
                .by_client_request
                .get(&record.client_request_id)
                .is_some_and(|mapped| mapped == &record.task_id)
            {
                inner.by_client_request.remove(&record.client_request_id);
            }
            inner.order.retain(|candidate| candidate != &id);
        }
    }
}

/// Opaque mapping from A's remote session reference to B's authoritative
/// session. The map is per connection and never persisted.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum RemoteSessionLane {
    Chat,
    Code,
    /// A switch to an existing B session has no trusted lane in the public
    /// command. Only a subsequent trusted ensure can claim it once.
    Unknown,
}

impl RemoteSessionLane {
    fn from_code_mode(code_mode: bool) -> Self {
        if code_mode { Self::Code } else { Self::Chat }
    }
}

#[derive(Default)]
pub(crate) struct RemoteSessionMap {
    forward: Mutex<HashMap<String, (String, String, RemoteSessionLane)>>,
    reverse: Mutex<HashMap<String, (String, String, RemoteSessionLane)>>,
}

impl RemoteSessionMap {
    pub(crate) fn bind(
        &self,
        remote_ref: &str,
        session_id: &str,
        generation: &str,
        lane: RemoteSessionLane,
    ) -> Result<String> {
        validate_identifier(remote_ref)?;
        validate_identifier(session_id)?;
        validate_identifier(generation)?;
        let mut forward = self.forward.lock();
        let mut reverse = self.reverse.lock();
        if let Some(existing) = reverse.get(session_id) {
            if existing.0 != remote_ref || existing.1 != generation {
                return Err(error("task_session_map_conflict"));
            }
        }
        if forward
            .get(remote_ref)
            .is_some_and(|existing| existing.1 != generation)
        {
            return Err(error("task_session_generation_conflict"));
        }
        if let Some(existing) = forward
            .get(remote_ref)
            .filter(|existing| existing.0 == session_id)
        {
            reverse
                .get(session_id)
                .filter(|reverse| {
                    reverse.0 == remote_ref && reverse.1 == generation && reverse.2 == existing.2
                })
                .ok_or_else(|| error("task_session_map_conflict"))?;
            match (existing.2, lane) {
                (RemoteSessionLane::Unknown, RemoteSessionLane::Chat | RemoteSessionLane::Code) => {
                    // Only trusted ensure_lane may claim an unknown binding.
                    return Err(error("task_session_lane_conflict"));
                }
                (RemoteSessionLane::Chat, RemoteSessionLane::Code)
                | (RemoteSessionLane::Code, RemoteSessionLane::Chat) => {
                    return Err(error("task_session_lane_conflict"));
                }
                _ => return Ok(session_id.to_owned()),
            }
        }
        let binding = (session_id.to_owned(), generation.to_owned(), lane);
        if let Some(previous) = forward.insert(remote_ref.to_owned(), binding) {
            if previous.0 != session_id {
                reverse.remove(&previous.0);
            }
        }
        reverse.insert(
            session_id.to_owned(),
            (remote_ref.to_owned(), generation.to_owned(), lane),
        );
        Ok(session_id.to_owned())
    }

    fn ensure_lane(
        &self,
        remote_ref: &str,
        generation: &str,
        requested: RemoteSessionLane,
        allow_unknown_claim: bool,
    ) -> Result<Option<String>> {
        let mut forward = self.forward.lock();
        let mut reverse = self.reverse.lock();
        let Some(binding) = forward.get_mut(remote_ref) else {
            return Ok(None);
        };
        if binding.1 != generation {
            return Ok(None);
        }
        let reverse_binding = reverse
            .get_mut(&binding.0)
            .filter(|reverse| {
                reverse.0 == remote_ref && reverse.1 == generation && reverse.2 == binding.2
            })
            .ok_or_else(|| error("task_session_map_conflict"))?;
        match binding.2 {
            lane if lane == requested => {}
            RemoteSessionLane::Unknown => {
                if !allow_unknown_claim {
                    return Err(error("task_session_lane_unknown"));
                }
                binding.2 = requested;
                reverse_binding.2 = requested;
            }
            _ => return Err(error("task_session_lane_conflict")),
        }
        Ok(Some(binding.0.clone()))
    }

    pub(crate) fn session_for(&self, remote_ref: &str, generation: &str) -> Option<String> {
        self.forward
            .lock()
            .get(remote_ref)
            .filter(|binding| binding.1 == generation)
            .map(|binding| binding.0.clone())
    }

    pub(crate) fn is_remote_session(&self, session_id: &str) -> bool {
        self.reverse.lock().contains_key(session_id)
    }

    pub(crate) fn is_remote_session_for(&self, session_id: &str, generation: &str) -> bool {
        self.reverse
            .lock()
            .get(session_id)
            .is_some_and(|binding| binding.1 == generation)
    }

    /// Read-only authority for a local B turn. Unmapped/wrong-generation
    /// sessions return None; Unknown is explicit and must not be treated as Chat.
    pub(crate) fn lane_for_session(
        &self,
        session_id: &str,
        generation: &str,
    ) -> Option<RemoteSessionLane> {
        self.reverse
            .lock()
            .get(session_id)
            .filter(|binding| binding.1 == generation)
            .map(|binding| binding.2)
    }

    fn binding_for_session(
        &self,
        session_id: &str,
        generation: &str,
    ) -> Option<(String, RemoteSessionLane)> {
        self.reverse
            .lock()
            .get(session_id)
            .filter(|binding| binding.1 == generation)
            .map(|binding| (binding.0.clone(), binding.2))
    }

    /// E2E-05：按 B 权威会话 id 反查 A 的会话引用（wire 回传统一用 A 的 ref）。
    pub(crate) fn remote_ref_for(&self, session_id: &str) -> Option<String> {
        self.reverse
            .lock()
            .get(session_id)
            .map(|binding| binding.0.clone())
    }

    pub(crate) fn unbind(&self, remote_ref: &str, generation: &str) -> Option<String> {
        let mut forward = self.forward.lock();
        let binding = forward.get(remote_ref)?;
        if binding.1 != generation {
            return None;
        }
        let (session_id, _, _) = forward.remove(remote_ref)?;
        let mut reverse = self.reverse.lock();
        if reverse
            .get(&session_id)
            .is_some_and(|candidate| candidate.0 == remote_ref && candidate.1 == generation)
        {
            reverse.remove(&session_id);
        }
        Some(session_id)
    }

    pub(crate) fn clear(&self) {
        self.forward.lock().clear();
        self.reverse.lock().clear();
    }

    pub(crate) fn clear_generation(&self, generation: &str) {
        let mut forward = self.forward.lock();
        let mut reverse = self.reverse.lock();
        let stale: Vec<(String, String)> = forward
            .iter()
            .filter(|(_, binding)| binding.1 == generation)
            .map(|(remote_ref, binding)| (remote_ref.clone(), binding.0.clone()))
            .collect();
        for (remote_ref, session_id) in stale {
            forward.remove(&remote_ref);
            reverse.remove(&session_id);
        }
    }

    pub(crate) fn len(&self) -> usize {
        self.forward.lock().len()
    }
}

/// Narrow B-side session provisioning port. The command layer implements it on
/// top of `SessionStore`/`EnginePool`; tests use a deterministic double.
pub(crate) trait RemoteSessionProvisioner: Send + Sync {
    fn create_authoritative_session(
        &self,
        title: &str,
        code_mode: bool,
    ) -> std::result::Result<String, String>;

    fn session_exists(&self, session_id: &str) -> std::result::Result<bool, String>;

    /// Resolve B-owned workspace authority before taking the session lock.
    /// Implementations may consult the controlled-device owner here, but must
    /// never re-enter that owner from `claim_execution_lane` below.
    #[cfg(not(test))]
    fn resolve_code_workspace(&self, workspace_id: &str) -> std::result::Result<PathBuf, String>;

    #[cfg(test)]
    fn resolve_code_workspace(&self, workspace_id: &str) -> std::result::Result<PathBuf, String> {
        Ok(PathBuf::from(workspace_id))
    }

    /// Bind an already-created authoritative Session to the execution backend
    /// selected by the first authenticated task. For code this resolves the
    /// opaque workspace capability entirely on B and persists the ACP owner
    /// before the logical lane becomes visible.
    #[cfg(not(test))]
    fn claim_execution_lane(
        &self,
        session_id: &str,
        code_mode: bool,
        workspace: Option<&Path>,
    ) -> std::result::Result<(), String>;

    #[cfg(test)]
    fn claim_execution_lane(
        &self,
        _session_id: &str,
        _code_mode: bool,
        _workspace: Option<&Path>,
    ) -> std::result::Result<(), String> {
        Ok(())
    }
}

/// Narrow engine port for plain-mode turns.
#[async_trait]
pub(crate) trait RemoteChatTurnPort: Send + Sync {
    /// Runs to the turn terminal state. `Err(TASK_CANCELLED_CODE)` marks an
    /// acknowledged cancellation; any other error is a bounded failure code.
    /// `Ok(Some(text))` 是该轮次产出的 assistant 正文（E2E-05，用于权威 journal）。
    async fn run_chat_turn(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        skill_ids: Vec<String>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<Option<String>, String>;

    async fn run_chat_turn_with_attachments(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        skill_ids: Vec<String>,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<Option<String>, String> {
        if !attachments.is_empty() {
            return Err("task_attachments_unavailable".to_owned());
        }
        self.run_chat_turn(
            session_id,
            task_id,
            model_source,
            lease,
            content,
            skill_ids,
            events,
            cancel,
        )
        .await
    }
}

/// Narrow engine port for code-mode turns.
#[async_trait]
pub(crate) trait RemoteCodeTurnPort: Send + Sync {
    async fn run_code_turn_with_attachments(
        &self,
        session_id: &str,
        task_id: &str,
        lease: &LeaseContext,
        workspace_id: &str,
        content: String,
        skill_ids: Vec<String>,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String>;
}

pub(crate) struct ChatTurnExecutor {
    port: Arc<dyn RemoteChatTurnPort>,
}

impl ChatTurnExecutor {
    pub(crate) fn new(port: Arc<dyn RemoteChatTurnPort>) -> Self {
        Self { port }
    }

    async fn execute_turn(
        &self,
        request: Value,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String> {
        let (session_id, content) = extract_turn(&request, TASK_MODE_CHAT)?;
        let task_id = request
            .get("task_id")
            .and_then(Value::as_str)
            .filter(|value| !value.is_empty())
            .ok_or_else(|| "task_request_invalid".to_owned())?;
        let model_source = request
            .get("model_source")
            .and_then(Value::as_str)
            .filter(|value| {
                matches!(
                    *value,
                    TASK_MODEL_SOURCE_CONTROLLED | TASK_MODEL_SOURCE_TEMPORARY
                )
            })
            .ok_or_else(|| "task_model_source_invalid".to_owned())?;
        let lease_text = |field: &str| {
            request
                .get(field)
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty())
                .map(str::to_owned)
                .ok_or_else(|| "task_request_invalid".to_owned())
        };
        let lease = LeaseContext {
            connection_id: lease_text("connection_id")?,
            controller_lease_id: lease_text("controller_lease_id")?,
            lease_epoch: lease_text("lease_epoch")?,
            relay_epoch: lease_text("relay_epoch")?,
            remote_control_generation: lease_text("remote_control_generation")?,
        };
        let skill_ids = extract_skill_ids(&request)?;
        // E2E-05：用户消息与回复正文都写进权威 journal（再经 wire 回传 A）。
        events.emit_journal(
            JournalKind::UserMessage,
            &session_id,
            Some(task_id),
            json!({ "text": content }),
        );
        let reply = self
            .port
            .run_chat_turn_with_attachments(
                &session_id,
                task_id,
                model_source,
                &lease,
                content,
                skill_ids,
                attachments,
                Arc::clone(&events),
                cancel,
            )
            .await?;
        if let Some(reply) = reply.filter(|text| !text.trim().is_empty()) {
            events.emit_journal(
                JournalKind::AssistantDelta,
                &session_id,
                Some(task_id),
                json!({ "text": reply }),
            );
        }
        Ok(())
    }
}

#[async_trait]
impl ControllerTaskExecutor for ChatTurnExecutor {
    async fn execute(
        &self,
        request: Value,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String> {
        self.execute_turn(request, Vec::new(), events, cancel).await
    }

    async fn execute_with_attachments(
        &self,
        request: Value,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String> {
        self.execute_turn(request, attachments, events, cancel)
            .await
    }
}

pub(crate) struct CodeTurnExecutor {
    port: Arc<dyn RemoteCodeTurnPort>,
}

impl CodeTurnExecutor {
    pub(crate) fn new(port: Arc<dyn RemoteCodeTurnPort>) -> Self {
        Self { port }
    }

    async fn execute_turn(
        &self,
        request: Value,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String> {
        let (session_id, content) = extract_turn(&request, TASK_MODE_CODE)?;
        let task_id = request
            .get("task_id")
            .and_then(Value::as_str)
            .filter(|value| !value.is_empty())
            .ok_or_else(|| "task_request_invalid".to_owned())?;
        let lease_text = |field: &str| {
            request
                .get(field)
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty())
                .map(str::to_owned)
                .ok_or_else(|| "task_request_invalid".to_owned())
        };
        let lease = LeaseContext {
            connection_id: lease_text("connection_id")?,
            controller_lease_id: lease_text("controller_lease_id")?,
            lease_epoch: lease_text("lease_epoch")?,
            relay_epoch: lease_text("relay_epoch")?,
            remote_control_generation: lease_text("remote_control_generation")?,
        };
        let workspace_id = request
            .get("input")
            .and_then(|input| input.get("workspace_id"))
            .and_then(Value::as_str)
            .ok_or_else(|| "task_workspace_required".to_owned())?;
        let skill_ids = extract_skill_ids(&request)?;
        events.emit_journal(
            JournalKind::UserMessage,
            &session_id,
            Some(task_id),
            json!({ "text": content }),
        );
        self.port
            .run_code_turn_with_attachments(
                &session_id,
                task_id,
                &lease,
                workspace_id,
                content,
                skill_ids,
                attachments,
                events,
                cancel,
            )
            .await
    }
}

#[async_trait]
impl ControllerTaskExecutor for CodeTurnExecutor {
    async fn execute(
        &self,
        request: Value,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String> {
        self.execute_turn(request, Vec::new(), events, cancel).await
    }

    async fn execute_with_attachments(
        &self,
        request: Value,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String> {
        self.execute_turn(request, attachments, events, cancel)
            .await
    }
}

fn extract_turn(
    request: &Value,
    expected_mode: &str,
) -> std::result::Result<(String, String), String> {
    let mode = request
        .get("mode")
        .and_then(Value::as_str)
        .unwrap_or_default();
    if mode != expected_mode {
        return Err("task_mode_mismatch".to_owned());
    }
    let session_id = request
        .get("session_id")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?;
    let content = request
        .get("input")
        .and_then(|input| input.get("text"))
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?;
    Ok((session_id.to_owned(), content.to_owned()))
}

fn extract_skill_ids(request: &Value) -> std::result::Result<Vec<String>, String> {
    request
        .get("input")
        .ok_or_else(|| "task_skills_invalid".to_owned())
        .and_then(extract_skill_ids_from_input)
}

fn extract_skill_ids_from_input(input: &Value) -> std::result::Result<Vec<String>, String> {
    input
        .get("skill_ids")
        .and_then(Value::as_array)
        .ok_or_else(|| "task_skills_invalid".to_owned())?
        .iter()
        .map(|value| {
            value
                .as_str()
                .filter(|value| !value.is_empty())
                .map(str::to_owned)
                .ok_or_else(|| "task_skills_invalid".to_owned())
        })
        .collect()
}

/// Extension surface frozen for DEV-15 (temporary model packages).
pub(crate) trait RemoteTaskModelScope: Send + Sync {
    fn release_task_credentials(&self, task_id: &str);
}

/// Extension surface frozen for DEV-16 (A-only approval authority).
pub(crate) trait RemoteTaskPermissionScope: Send + Sync {
    fn deny_pending_for_task(&self, task_id: &str) -> usize;
}

/// Extension surface frozen for DEV-17/DEV-18 (attachment and artifact
/// transfers).
pub(crate) trait RemoteTaskTransferScope: Send + Sync {
    fn abort_transfers_for_task(&self, task_id: &str) -> usize;
}

/// Two-phase artifact publication boundary. Executors may stage private
/// copies while a task is still running, but only this terminal hook may make
/// them visible. Implementations must consume staged state idempotently.
pub(crate) trait RemoteTaskArtifactScope: Send + Sync {
    fn commit_task_artifacts(&self, record: &RemoteTaskRecord);
    fn discard_task_artifacts(&self, task_id: &str);
}

/// Extension surface frozen for DEV-19 (authoritative event journal).
pub(crate) trait RemoteTaskEventSink: Send + Sync {
    fn publish_task_event(&self, event: &RemoteTaskEvent);
}

#[derive(Debug, Clone, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct RemoteTaskEvent {
    pub(crate) task_id: String,
    pub(crate) session_id: String,
    pub(crate) generation: String,
    pub(crate) model_source: RemoteTaskModelSource,
    pub(crate) status: String,
    pub(crate) sequence: String,
    pub(crate) transition_sequence: u64,
    pub(crate) outcome_unknown: bool,
    pub(crate) terminal_reason: Option<String>,
    pub(crate) result_digest: Option<String>,
}

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum RemoteTaskModelSource {
    ControlledDevice,
    ControllerTemporary,
}

impl RemoteTaskModelSource {
    pub(crate) fn from_record(record: &RemoteTaskRecord) -> Option<Self> {
        match record.model_source.as_str() {
            TASK_MODEL_SOURCE_CONTROLLED => Some(Self::ControlledDevice),
            TASK_MODEL_SOURCE_TEMPORARY => Some(Self::ControllerTemporary),
            _ => None,
        }
    }

    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::ControlledDevice => TASK_MODEL_SOURCE_CONTROLLED,
            Self::ControllerTemporary => TASK_MODEL_SOURCE_TEMPORARY,
        }
    }
}

#[derive(Default)]
pub(crate) struct RemoteTaskHooks {
    model: Mutex<Option<Arc<dyn RemoteTaskModelScope>>>,
    permission: Mutex<Option<Arc<dyn RemoteTaskPermissionScope>>>,
    transfer: Mutex<Option<Arc<dyn RemoteTaskTransferScope>>>,
    artifact: Mutex<Option<Arc<dyn RemoteTaskArtifactScope>>>,
    journal: Mutex<Option<Arc<dyn RemoteTaskEventSink>>>,
}

impl RemoteTaskHooks {
    pub(crate) fn set_model_scope(&self, scope: Arc<dyn RemoteTaskModelScope>) {
        *self.model.lock() = Some(scope);
    }

    pub(crate) fn set_permission_scope(&self, scope: Arc<dyn RemoteTaskPermissionScope>) {
        *self.permission.lock() = Some(scope);
    }

    pub(crate) fn set_transfer_scope(&self, scope: Arc<dyn RemoteTaskTransferScope>) {
        *self.transfer.lock() = Some(scope);
    }

    pub(crate) fn set_artifact_scope(&self, scope: Arc<dyn RemoteTaskArtifactScope>) {
        *self.artifact.lock() = Some(scope);
    }

    pub(crate) fn set_journal(&self, journal: Arc<dyn RemoteTaskEventSink>) {
        *self.journal.lock() = Some(journal);
    }
}

/// Single facade used by the Tauri command layer. It is the only place that
/// turns an admission into a dispatch and that runs the terminal cleanup.
/// Validated, path-resolved preparation has no Session/ACP side effects.
/// The app claims it only while holding the exact controlled lease owner.
pub(crate) struct PreparedTaskSession {
    request: RemoteTaskRequest,
    title: String,
    workspace: Option<PathBuf>,
    provisioner: Arc<dyn RemoteSessionProvisioner>,
}

pub(crate) struct RemoteTaskService {
    ledger: Arc<RemoteTaskLedger>,
    sessions: RemoteSessionMap,
    /// A retried capability request and the first task can provision at once.
    /// Serialize lookup/create/bind so one A reference owns one B session.
    session_open_lock: Mutex<()>,
    provisioner: Mutex<Option<Arc<dyn RemoteSessionProvisioner>>>,
    executors: Mutex<HashMap<String, Arc<dyn ControllerTaskExecutor>>>,
    /// DEV-13 审核修复：取消必须真正中断执行器。dispatch 注册当前任务的取消令牌，
    /// cancel() 命中 CancellationRequested 时取消该令牌，执行器据此收尾为终态。
    cancel_tokens: Mutex<HashMap<String, CancellationToken>>,
    hooks: RemoteTaskHooks,
}

impl RemoteTaskService {
    pub(crate) fn new(ledger: Arc<RemoteTaskLedger>) -> Self {
        Self {
            ledger,
            sessions: RemoteSessionMap::default(),
            session_open_lock: Mutex::new(()),
            provisioner: Mutex::new(None),
            executors: Mutex::new(HashMap::new()),
            cancel_tokens: Mutex::new(HashMap::new()),
            hooks: RemoteTaskHooks::default(),
        }
    }

    pub(crate) fn ledger(&self) -> Arc<RemoteTaskLedger> {
        Arc::clone(&self.ledger)
    }

    pub(crate) fn hooks(&self) -> &RemoteTaskHooks {
        &self.hooks
    }

    pub(crate) fn set_session_provisioner(&self, provisioner: Arc<dyn RemoteSessionProvisioner>) {
        *self.provisioner.lock() = Some(provisioner);
    }

    pub(crate) fn register_executor(&self, mode: &str, executor: Arc<dyn ControllerTaskExecutor>) {
        self.executors.lock().insert(mode.to_owned(), executor);
    }

    pub(crate) fn open_remote_session(
        &self,
        remote_ref: &str,
        title: &str,
        code_mode: bool,
        generation: &str,
    ) -> Result<String> {
        let provisioner = self
            .provisioner
            .lock()
            .clone()
            .ok_or_else(|| error("task_session_provisioner_unavailable"))?;
        let session_id = provisioner
            .create_authoritative_session(title, code_mode)
            .map_err(|_| error("task_session_create_failed"))?;
        self.sessions.bind(
            remote_ref,
            &session_id,
            generation,
            RemoteSessionLane::from_code_mode(code_mode),
        )
    }

    /// Return the B-authoritative session already bound to `remote_ref`, or
    /// create it once before either endpoint is allowed to submit.
    pub(crate) fn ensure_remote_session(
        &self,
        remote_ref: &str,
        title: &str,
        code_mode: bool,
        generation: &str,
    ) -> Result<String> {
        self.ensure_remote_session_for_lane(remote_ref, title, code_mode, generation, false)
    }

    /// Capability negotiation must expose the B-authoritative session ID before
    /// A chooses chat or code. Keep the mapping unclaimed until the first
    /// authenticated task (or B-local chat input) fixes the lane exactly once.
    pub(crate) fn ensure_unclaimed_remote_session(
        &self,
        remote_ref: &str,
        title: &str,
        generation: &str,
    ) -> Result<String> {
        let _guard = self.session_open_lock.lock();
        if let Some(session_id) = self.sessions.session_for(remote_ref, generation) {
            return Ok(session_id);
        }
        let provisioner = self
            .provisioner
            .lock()
            .clone()
            .ok_or_else(|| error("task_session_provisioner_unavailable"))?;
        // SessionStore chat records are the common transcript owner. The first
        // trusted task chooses the execution lane; code execution remains ACP.
        let session_id = provisioner
            .create_authoritative_session(title, false)
            .map_err(|_| error("task_session_create_failed"))?;
        self.sessions.bind(
            remote_ref,
            &session_id,
            generation,
            RemoteSessionLane::Unknown,
        )
    }

    /// Only an already authenticated controller task whose complete DEV-13
    /// shape has been checked may claim an Unknown mapping. The live lease is
    /// rechecked here, before the mapping transition, rather than trusting a
    /// UI-provided code_mode flag or session reference.
    #[cfg(test)]
    pub(crate) fn ensure_authenticated_task_session(
        &self,
        request: &RemoteTaskRequest,
        live: &LeaseContext,
        title: &str,
    ) -> Result<String> {
        let prepared = self.prepare_authenticated_task_session(request, live, title)?;
        self.claim_prepared_task_session(prepared, live)
    }

    pub(crate) fn prepare_authenticated_task_session(
        &self,
        request: &RemoteTaskRequest,
        live: &LeaseContext,
        title: &str,
    ) -> Result<PreparedTaskSession> {
        request.validate()?;
        if request.lease != TaskLeaseBinding::from_context(live) {
            return Err(error("task_lease_mismatch"));
        }
        let provisioner = self
            .provisioner
            .lock()
            .clone()
            .ok_or_else(|| error("task_session_provisioner_unavailable"))?;
        let workspace = request
            .workspace_id
            .as_deref()
            .filter(|_| request.mode == TASK_MODE_CODE)
            .map(|id| {
                provisioner
                    .resolve_code_workspace(id)
                    .map_err(|_| error("task_workspace_not_authorized"))
            })
            .transpose()?;
        Ok(PreparedTaskSession {
            request: request.clone(),
            title: title.to_owned(),
            workspace,
            provisioner,
        })
    }

    /// Caller holds with_current_business_lease through all Session/ACP mutations.
    /// This consumes resolved paths and never re-enters the controlled owner.
    pub(crate) fn claim_prepared_task_session(
        &self,
        prepared: PreparedTaskSession,
        live: &LeaseContext,
    ) -> Result<String> {
        if prepared.request.lease != TaskLeaseBinding::from_context(live) {
            return Err(error("task_lease_mismatch"));
        }
        self.claim_remote_session_for_lane(
            &prepared.request.session_id,
            &prepared.title,
            prepared.request.mode == TASK_MODE_CODE,
            prepared.workspace.as_deref(),
            &live.remote_control_generation,
            true,
            &prepared.provisioner,
        )
    }

    fn ensure_remote_session_for_lane(
        &self,
        remote_ref: &str,
        title: &str,
        code_mode: bool,
        generation: &str,
        allow_unknown_claim: bool,
    ) -> Result<String> {
        self.ensure_remote_session_for_lane_with_workspace(
            remote_ref,
            title,
            code_mode,
            None,
            generation,
            allow_unknown_claim,
        )
    }

    fn ensure_remote_session_for_lane_with_workspace(
        &self,
        remote_ref: &str,
        title: &str,
        code_mode: bool,
        workspace_id: Option<&str>,
        generation: &str,
        allow_unknown_claim: bool,
    ) -> Result<String> {
        let provisioner = self
            .provisioner
            .lock()
            .clone()
            .ok_or_else(|| error("task_session_provisioner_unavailable"))?;
        // Lock order is owner -> session -> ledger. Resolve before session,
        // including for an Unknown mapping claimed by its first code task.
        let workspace = if let Some(id) = workspace_id.filter(|_| code_mode) {
            Some(
                provisioner
                    .resolve_code_workspace(id)
                    .map_err(|_| error("task_workspace_not_authorized"))?,
            )
        } else {
            None
        };
        self.claim_remote_session_for_lane(
            remote_ref,
            title,
            code_mode,
            workspace.as_deref(),
            generation,
            allow_unknown_claim,
            &provisioner,
        )
    }

    fn claim_remote_session_for_lane(
        &self,
        remote_ref: &str,
        title: &str,
        code_mode: bool,
        workspace: Option<&std::path::Path>,
        generation: &str,
        allow_unknown_claim: bool,
        provisioner: &Arc<dyn RemoteSessionProvisioner>,
    ) -> Result<String> {
        let _guard = self.session_open_lock.lock();
        let requested = RemoteSessionLane::from_code_mode(code_mode);
        if let Some(session_id) = self.sessions.session_for(remote_ref, generation) {
            let current = self
                .sessions
                .lane_for_session(&session_id, generation)
                .ok_or_else(|| error("task_session_not_found"))?;
            if current == RemoteSessionLane::Unknown && allow_unknown_claim {
                provisioner
                    .claim_execution_lane(&session_id, code_mode, workspace)
                    .map_err(|_| error("task_session_backend_bind_failed"))?;
            }
            let claimed = self.sessions.ensure_lane(
                remote_ref,
                generation,
                requested,
                allow_unknown_claim,
            )?;
            if claimed.as_deref() != Some(session_id.as_str()) {
                return Err(error("task_session_not_found"));
            }
            return Ok(session_id);
        }
        let session_id = provisioner
            .create_authoritative_session(title, code_mode)
            .map_err(|_| error("task_session_create_failed"))?;
        provisioner
            .claim_execution_lane(&session_id, code_mode, workspace)
            .map_err(|_| error("task_session_backend_bind_failed"))?;
        self.sessions
            .bind(remote_ref, &session_id, generation, requested)
    }

    pub(crate) fn switch_remote_session(
        &self,
        remote_ref: &str,
        session_id: &str,
        generation: &str,
    ) -> Result<String> {
        let _guard = self.session_open_lock.lock();
        let provisioner = self
            .provisioner
            .lock()
            .clone()
            .ok_or_else(|| error("task_session_provisioner_unavailable"))?;
        match provisioner.session_exists(session_id) {
            Ok(true) => {}
            Ok(false) => return Err(error("task_session_not_found")),
            Err(_) => return Err(error("task_session_lookup_failed")),
        }
        self.sessions.bind(
            remote_ref,
            session_id,
            generation,
            RemoteSessionLane::Unknown,
        )
    }

    /// A B-local composer is also an authenticated owner of the current
    /// generation. It may claim an as-yet unselected mapping as chat, but can
    /// never change a mapping already claimed by code.
    pub(crate) fn claim_local_session(
        &self,
        session_id: &str,
        generation: &str,
    ) -> Result<(String, RemoteSessionLane)> {
        let _guard = self.session_open_lock.lock();
        let remote_ref = self
            .sessions
            .remote_ref_for(session_id)
            .ok_or_else(|| error("task_session_not_found"))?;
        let current = self
            .sessions
            .lane_for_session(session_id, generation)
            .ok_or_else(|| error("task_session_not_found"))?;
        let requested = match current {
            RemoteSessionLane::Unknown => RemoteSessionLane::Chat,
            RemoteSessionLane::Chat => RemoteSessionLane::Chat,
            RemoteSessionLane::Code => RemoteSessionLane::Code,
        };
        if current == RemoteSessionLane::Unknown {
            let provisioner = self
                .provisioner
                .lock()
                .clone()
                .ok_or_else(|| error("task_session_provisioner_unavailable"))?;
            provisioner
                .claim_execution_lane(session_id, false, None)
                .map_err(|_| error("task_session_backend_bind_failed"))?;
        }
        let claimed = self
            .sessions
            .ensure_lane(&remote_ref, generation, requested, true)?;
        if claimed.as_deref() != Some(session_id) {
            return Err(error("task_session_not_found"));
        }
        Ok((remote_ref, requested))
    }

    pub(crate) fn close_remote_session(
        &self,
        remote_ref: &str,
        generation: &str,
    ) -> Result<Option<String>> {
        let _guard = self.session_open_lock.lock();
        if let Some(session_id) = self.sessions.session_for(remote_ref, generation) {
            if self.ledger.active_for_session(&session_id).is_some() {
                return Err(error("task_session_busy"));
            }
        }
        Ok(self.sessions.unbind(remote_ref, generation))
    }

    pub(crate) fn session_for(&self, remote_ref: &str, generation: &str) -> Option<String> {
        self.sessions.session_for(remote_ref, generation)
    }

    pub(crate) fn is_remote_session(&self, session_id: &str) -> bool {
        self.sessions.is_remote_session(session_id)
    }

    pub(crate) fn is_remote_session_for(&self, session_id: &str, generation: &str) -> bool {
        self.sessions.is_remote_session_for(session_id, generation)
    }

    /// DEV-28 consumes only a trusted Chat lane for local B submissions.
    /// Code and Unknown are both non-Chat, including after a public switch.
    pub(crate) fn lane_for_session(
        &self,
        session_id: &str,
        generation: &str,
    ) -> Option<RemoteSessionLane> {
        self.sessions.lane_for_session(session_id, generation)
    }

    /// E2E-05：B 权威会话 id → A 的会话引用（未绑定或本地会话返回 None）。
    pub(crate) fn remote_ref_for(&self, session_id: &str) -> Option<String> {
        self.sessions.remote_ref_for(session_id)
    }

    pub(crate) fn clear_sessions(&self) {
        let _guard = self.session_open_lock.lock();
        self.sessions.clear();
    }

    pub(crate) fn clear_generation_sessions(&self, generation: &str) {
        let _guard = self.session_open_lock.lock();
        self.sessions.clear_generation(generation);
    }

    pub(crate) fn submit(
        &self,
        live: &LeaseContext,
        payload: &Value,
        now_b: u64,
    ) -> Result<AdmissionOutcome> {
        let request = RemoteTaskRequest::from_payload(payload)?;
        let outcome = self.ledger.admit(live, &request, now_b)?;
        if let AdmissionOutcome::Admitted(ticket) = &outcome {
            self.publish(&ticket.record);
        }
        Ok(outcome)
    }

    /// Admit only while the submitted B-authoritative session is still bound
    /// to the live controlled-device lifecycle. Cleanup takes the same lock,
    /// so a send racing stop/refresh is either admitted before cleanup (and
    /// then interrupted) or rejected after the mapping is cleared.
    pub(crate) fn submit_bound(
        &self,
        live: &LeaseContext,
        payload: &Value,
        now_b: u64,
    ) -> Result<AdmissionOutcome> {
        self.submit_bound_with_admission(live, payload, now_b, |_| Ok(()))
    }

    /// Admit a bound task and finish its task-scoped resource installation in
    /// the same lifecycle transaction. Stop/refresh takes the same lock, so it
    /// cannot run cleanup between ledger admission and resource installation.
    /// If installation fails, the admitted record is made terminal before the
    /// lock is released and the normal cleanup hooks run immediately.
    pub(crate) fn submit_bound_with_admission(
        &self,
        live: &LeaseContext,
        payload: &Value,
        now_b: u64,
        install: impl FnOnce(&RemoteTaskTicket) -> std::result::Result<(), &'static str>,
    ) -> Result<AdmissionOutcome> {
        self.submit_bound_with_expected_ref(live, payload, now_b, None, install)
    }

    /// The B-local path must keep the expected A reference authoritative up
    /// to the exact admission point. A preflight lookup is not sufficient:
    /// switch/close/rebind can change the reverse binding before ledger admit.
    pub(crate) fn submit_bound_for_ref_with_admission(
        &self,
        live: &LeaseContext,
        expected_remote_ref: &str,
        payload: &Value,
        now_b: u64,
        install: impl FnOnce(&RemoteTaskTicket) -> std::result::Result<(), &'static str>,
    ) -> Result<AdmissionOutcome> {
        validate_identifier(expected_remote_ref)?;
        self.submit_bound_with_expected_ref(
            live,
            payload,
            now_b,
            Some(expected_remote_ref),
            install,
        )
    }

    fn submit_bound_with_expected_ref(
        &self,
        live: &LeaseContext,
        payload: &Value,
        now_b: u64,
        expected_remote_ref: Option<&str>,
        install: impl FnOnce(&RemoteTaskTicket) -> std::result::Result<(), &'static str>,
    ) -> Result<AdmissionOutcome> {
        let prepared =
            self.prepare_bound_admission(live, payload, now_b, expected_remote_ref, install)?;
        self.finish_bound_admission(prepared)
    }

    /// Called under the controlled-device owner lock. Only mutate the ledger
    /// and install task resources here; journal/cleanup hooks may re-enter the
    /// owner and therefore belong to `finish_bound_admission` after unlock.
    pub(crate) fn prepare_bound_admission(
        &self,
        live: &LeaseContext,
        payload: &Value,
        now_b: u64,
        expected_remote_ref: Option<&str>,
        install: impl FnOnce(&RemoteTaskTicket) -> std::result::Result<(), &'static str>,
    ) -> Result<PreparedTaskAdmission> {
        if let Some(remote_ref) = expected_remote_ref {
            validate_identifier(remote_ref)?;
        }
        let request = RemoteTaskRequest::from_payload(payload)?;
        let _guard = self.session_open_lock.lock();
        let binding = self
            .sessions
            .binding_for_session(&request.session_id, &live.remote_control_generation)
            .ok_or_else(|| {
                error(if expected_remote_ref.is_some() {
                    "task_session_binding_mismatch"
                } else {
                    "task_session_not_found"
                })
            })?;
        if expected_remote_ref.is_some_and(|expected| binding.0 != expected) {
            return Err(error("task_session_binding_mismatch"));
        }
        let requested_lane = if request.mode == TASK_MODE_CHAT {
            RemoteSessionLane::Chat
        } else {
            RemoteSessionLane::Code
        };
        if binding.1 != requested_lane {
            return Err(error("task_session_lane_mismatch"));
        }
        let outcome = self.ledger.admit(live, &request, now_b)?;
        if let AdmissionOutcome::Admitted(ticket) = &outcome {
            if let Err(code) = install(ticket) {
                let failed = self.ledger.complete_with_reason(
                    &ticket.record.task_id,
                    RemoteTaskTerminal::Failed,
                    Some(code.to_owned()),
                    None,
                    now_b,
                )?;
                return Ok(PreparedTaskAdmission::InstallationFailed(failed, code));
            }
        }
        Ok(PreparedTaskAdmission::Accepted(outcome))
    }

    pub(crate) fn finish_bound_admission(
        &self,
        prepared: PreparedTaskAdmission,
    ) -> Result<AdmissionOutcome> {
        match prepared {
            PreparedTaskAdmission::Accepted(outcome) => {
                if let AdmissionOutcome::Admitted(ticket) = &outcome {
                    // Cleanup may have won immediately after admission. Never
                    // republish its older admitted state over a newer terminal.
                    if let Ok(current) = self.ledger.record(&ticket.record.task_id) {
                        self.publish(&current);
                    }
                }
                Ok(outcome)
            }
            PreparedTaskAdmission::InstallationFailed(record, code) => {
                self.cleanup(&record, code.to_owned());
                Err(error(code))
            }
        }
    }

    pub(crate) async fn dispatch(
        &self,
        ticket: RemoteTaskTicket,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
        now_b: u64,
    ) -> Result<RemoteTaskRecord> {
        self.dispatch_with_attachments(ticket, Vec::new(), events, cancel, now_b)
            .await
    }

    /// B-local published files are passed out of band, never by path on wire.
    /// A nonempty wire attachment list without exact consumed descriptors
    /// becomes a terminal failure rather than a text-only execution.
    pub(crate) async fn dispatch_with_attachments(
        &self,
        ticket: RemoteTaskTicket,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
        now_b: u64,
    ) -> Result<RemoteTaskRecord> {
        let task_id = ticket.record.task_id.clone();
        let requested = ticket.input.get("attachment_ids").and_then(Value::as_array);
        let exact = requested.is_some_and(|ids| {
            ids.len() == attachments.len()
                && ids
                    .iter()
                    .zip(&attachments)
                    .all(|(id, attachment)| id.as_str() == Some(attachment.handle_id.as_str()))
        });
        if !exact {
            events.emit_journal(
                JournalKind::Error,
                &ticket.record.session_id,
                Some(&task_id),
                json!({ "code": "task_attachments_invalid" }),
            );
            return self.finish(
                &task_id,
                RemoteTaskTerminal::Failed,
                Some("task_attachments_invalid".to_owned()),
                now_b,
            );
        }
        self.cancel_tokens
            .lock()
            .insert(task_id.clone(), cancel.clone());
        let running = match self.ledger.begin_dispatch(&task_id, now_b) {
            Ok(running) => running,
            Err(error) => {
                self.cancel_tokens.lock().remove(&task_id);
                return Err(error);
            }
        };
        self.publish(&running);
        let executor = self.executors.lock().get(&running.mode).cloned();
        let Some(executor) = executor else {
            self.cancel_tokens.lock().remove(&task_id);
            events.emit_journal(
                JournalKind::Error,
                &running.session_id,
                Some(&task_id),
                json!({ "code": "executor_unavailable" }),
            );
            return self.finish(
                &task_id,
                RemoteTaskTerminal::Failed,
                Some("executor_unavailable".to_owned()),
                now_b,
            );
        };
        // Executors consume the complete execution envelope. The ledger keeps
        // prompt input separate from durable task metadata so secrets never
        // enter the status record; rebuild the narrow envelope here instead
        // of passing only `input` (which makes `extract_turn` fail before the
        // engine ever sees the accepted prompt).
        let execution_request = json!({
            "task_id": running.task_id,
            "session_id": running.session_id,
            "mode": running.mode,
            "model_source": running.model_source,
            "connection_id": running.lease.connection_id,
            "controller_lease_id": running.lease.controller_lease_id,
            "lease_epoch": running.lease.lease_epoch,
            "relay_epoch": running.lease.relay_epoch,
            "remote_control_generation": running.lease.remote_control_generation,
            "input": ticket.input,
        });
        let result = executor
            .execute_with_attachments(execution_request, attachments, Arc::clone(&events), cancel)
            .await;
        self.cancel_tokens.lock().remove(&task_id);
        let cancel_won = self
            .ledger
            .record(&task_id)
            .is_ok_and(|record| record.cancel_requested && !record.is_terminal());
        let (terminal, reason) = match result {
            Ok(()) if cancel_won => {
                events.emit_journal(
                    JournalKind::Cancel,
                    &running.session_id,
                    Some(&task_id),
                    json!({ "status": TASK_STATUS_CANCELLED }),
                );
                (RemoteTaskTerminal::Cancelled, None)
            }
            Ok(()) => (RemoteTaskTerminal::Succeeded, None),
            Err(code) if code == TASK_CANCELLED_CODE => {
                events.emit_journal(
                    JournalKind::Cancel,
                    &running.session_id,
                    Some(&task_id),
                    json!({ "status": TASK_STATUS_CANCELLED }),
                );
                (RemoteTaskTerminal::Cancelled, None)
            }
            Err(code) => {
                events.emit_journal(
                    JournalKind::Error,
                    &running.session_id,
                    Some(&task_id),
                    json!({ "code": code }),
                );
                (RemoteTaskTerminal::Failed, Some(code))
            }
        };
        self.finish(&task_id, terminal, reason, now_b)
    }

    pub(crate) fn complete(
        &self,
        task_id: &str,
        terminal: RemoteTaskTerminal,
        result_digest: Option<String>,
        now_b: u64,
    ) -> Result<RemoteTaskRecord> {
        let record = match self
            .ledger
            .complete(task_id, terminal, result_digest, now_b)
        {
            Ok(record) => record,
            Err(error) => {
                if let Some(scope) = self.hooks.artifact.lock().clone() {
                    scope.discard_task_artifacts(task_id);
                }
                return Err(error);
            }
        };
        self.cleanup(
            &record,
            record
                .terminal_reason
                .clone()
                .unwrap_or_else(|| terminal.reason().to_owned()),
        );
        Ok(record)
    }

    /// A task admitted durably but rejected before executor dispatch must
    /// still reach a terminal state and release its turn/resources. This is
    /// intentionally unavailable once dispatch has started.
    pub(crate) fn fail_admitted(
        &self,
        task_id: &str,
        reason: &'static str,
        now_b: u64,
    ) -> Result<RemoteTaskRecord> {
        let current = self.ledger.record(task_id)?;
        if current.state != RemoteTaskState::Admitted {
            return Err(error("task_state_invalid"));
        }
        self.finish(
            task_id,
            RemoteTaskTerminal::Failed,
            Some(reason.to_owned()),
            now_b,
        )
    }

    pub(crate) fn enter_waiting_permission(
        &self,
        task_id: &str,
        now_b: u64,
    ) -> Result<RemoteTaskRecord> {
        let record = self
            .ledger
            .transition(task_id, RemoteTaskState::WaitingPermission, now_b)?;
        self.publish(&record);
        Ok(record)
    }

    pub(crate) fn resume_running(&self, task_id: &str, now_b: u64) -> Result<RemoteTaskRecord> {
        let record = self
            .ledger
            .transition(task_id, RemoteTaskState::Running, now_b)?;
        self.publish(&record);
        Ok(record)
    }

    #[cfg(test)]
    pub(crate) fn cancel(&self, task_id: &str, now_b: u64) -> Result<CancelOutcome> {
        let outcome = self.ledger.request_cancel(task_id, now_b)?;
        self.finish_cancel_outcome(&outcome);
        Ok(outcome)
    }

    /// B-local stop admission. The caller holds the controlled-device owner
    /// lock; this method only mutates the authoritative ledger and must not
    /// publish until the owner lock is released.
    pub(crate) fn prepare_local_cancel(
        &self,
        lease: &LeaseContext,
        task_id: &str,
        now_b: u64,
    ) -> Result<CancelOutcome> {
        let _guard = self.session_open_lock.lock();
        let record = self.ledger.record(task_id)?;
        if record.lease != TaskLeaseBinding::from_context(lease) {
            return Err(error("task_binding_stale"));
        }
        let Some(remote_ref) = self.sessions.remote_ref_for(&record.session_id) else {
            return Err(error("task_binding_stale"));
        };
        if self
            .sessions
            .session_for(&remote_ref, &lease.remote_control_generation)
            .as_deref()
            != Some(record.session_id.as_str())
        {
            return Err(error("task_binding_stale"));
        }
        self.ledger
            .request_cancel_bound(lease, &record.session_id, task_id, now_b)
    }

    /// Remote cancellation pins the authenticated reference mapping and checks
    /// the exact task lease/session atomically with the ledger mutation. No
    /// callbacks are emitted while the caller holds the B lifecycle lock.
    pub(crate) fn prepare_remote_cancel(
        &self,
        lease: &LeaseContext,
        remote_ref: &str,
        task_id: &str,
        now_b: u64,
    ) -> Result<CancelOutcome> {
        let _guard = self.session_open_lock.lock();
        let session_id = self
            .sessions
            .session_for(remote_ref, &lease.remote_control_generation)
            .ok_or_else(|| error("task_binding_stale"))?;
        self.ledger
            .request_cancel_bound(lease, &session_id, task_id, now_b)
    }

    pub(crate) fn finish_cancel_outcome(&self, outcome: &CancelOutcome) {
        match outcome {
            CancelOutcome::CancelledBeforeDispatch(record)
            | CancelOutcome::CancellationRequested(record)
            | CancelOutcome::AlreadyTerminal(record) => self.publish(record),
        }
        if let CancelOutcome::CancelledBeforeDispatch(record) = outcome {
            self.cleanup(record, "cancelled".to_owned());
        }
        if let CancelOutcome::CancellationRequested(record) = outcome {
            if let Some(token) = self.cancel_tokens.lock().get(&record.task_id) {
                token.cancel();
            }
        }
    }

    pub(crate) fn interrupt_generation(
        &self,
        generation: &str,
        reason: &str,
        now_b: u64,
    ) -> Vec<RemoteTaskRecord> {
        let records = self.ledger.interrupt_generation(generation, reason, now_b);
        self.finish_interrupted(records)
    }

    /// End one controlled-device lifecycle and invalidate every ephemeral
    /// A-ref/B-session mapping atomically with respect to bound admission.
    pub(crate) fn interrupt_generation_and_clear_sessions(
        &self,
        generation: &str,
        reason: &str,
        now_b: u64,
    ) -> Vec<RemoteTaskRecord> {
        let _guard = self.session_open_lock.lock();
        let records = self.ledger.interrupt_generation(generation, reason, now_b);
        self.sessions.clear_generation(generation);
        drop(_guard);
        self.finish_interrupted(records)
    }

    pub(crate) fn interrupt_all(&self, reason: &str, now_b: u64) -> Vec<RemoteTaskRecord> {
        let records = self.ledger.interrupt_all(reason, now_b);
        self.finish_interrupted(records)
    }

    pub(crate) fn recover_after_restart(&self, now_b: u64) -> Vec<RemoteTaskRecord> {
        let recovered = self.ledger.recover_after_restart(now_b);
        self.sessions.clear();
        self.finish_interrupted(recovered)
    }

    pub(crate) fn status(&self, task_id: &str) -> Result<RemoteTaskRecord> {
        self.ledger.record(task_id)
    }

    /// Protocol-shaped `task_status` payload for the wire.
    pub(crate) fn status_payload(&self, task_id: &str) -> Result<Value> {
        let record = self.ledger.record(task_id)?;
        let mut payload = json!({
            "v": 1,
            "type": "task_status",
            "connection_id": record.lease.connection_id,
            "controller_lease_id": record.lease.controller_lease_id,
            "lease_epoch": record.lease.lease_epoch,
            "relay_epoch": record.lease.relay_epoch,
            "remote_control_generation": record.lease.remote_control_generation,
            "request_id": format!("status-{}", record.sequence),
            "task_id": record.task_id,
            "session_id": record.session_id,
            "task_status": record.wire_status(),
        });
        if let Some(digest) = record.result_digest {
            payload["result_digest"] = Value::String(digest);
        }
        Ok(payload)
    }

    fn finish(
        &self,
        task_id: &str,
        terminal: RemoteTaskTerminal,
        reason: Option<String>,
        now_b: u64,
    ) -> Result<RemoteTaskRecord> {
        let record = match self
            .ledger
            .complete_with_reason(task_id, terminal, reason, None, now_b)
        {
            Ok(record) => record,
            Err(error) => {
                if let Some(scope) = self.hooks.artifact.lock().clone() {
                    scope.discard_task_artifacts(task_id);
                }
                return Err(error);
            }
        };
        let cleanup_reason = record
            .terminal_reason
            .clone()
            .unwrap_or_else(|| terminal.reason().to_owned());
        self.cleanup(&record, cleanup_reason);
        Ok(record)
    }

    fn finish_interrupted(&self, records: Vec<RemoteTaskRecord>) -> Vec<RemoteTaskRecord> {
        for record in &records {
            if let Some(token) = self.cancel_tokens.lock().remove(&record.task_id) {
                token.cancel();
            }
            self.cleanup(
                record,
                record
                    .terminal_reason
                    .clone()
                    .unwrap_or_else(|| RemoteTaskTerminal::Interrupted.reason().to_owned()),
            );
        }
        records
    }

    fn cleanup(&self, record: &RemoteTaskRecord, reason: String) {
        if let Some(scope) = self.hooks.artifact.lock().clone() {
            if record.state == RemoteTaskState::Succeeded && !record.outcome_unknown {
                scope.commit_task_artifacts(record);
            } else {
                scope.discard_task_artifacts(&record.task_id);
            }
        }
        if let Some(scope) = self.hooks.model.lock().clone() {
            scope.release_task_credentials(&record.task_id);
        }
        if let Some(scope) = self.hooks.permission.lock().clone() {
            scope.deny_pending_for_task(&record.task_id);
        }
        if let Some(scope) = self.hooks.transfer.lock().clone() {
            scope.abort_transfers_for_task(&record.task_id);
        }
        self.publish_with_reason(record, reason);
    }

    fn publish(&self, record: &RemoteTaskRecord) {
        let reason = record
            .terminal_reason
            .clone()
            .unwrap_or_else(|| record.wire_status().to_owned());
        self.publish_with_reason(record, reason);
    }

    fn publish_with_reason(&self, record: &RemoteTaskRecord, reason: String) {
        let journal = self.hooks.journal.lock().clone();
        if let (Some(journal), Some(model_source)) =
            (journal, RemoteTaskModelSource::from_record(record))
        {
            journal.publish_task_event(&RemoteTaskEvent {
                task_id: record.task_id.clone(),
                session_id: record.session_id.clone(),
                generation: record.lease.remote_control_generation.clone(),
                model_source,
                status: record.wire_status().to_owned(),
                sequence: record.sequence.to_string(),
                transition_sequence: record.transition_sequence,
                outcome_unknown: record.outcome_unknown,
                terminal_reason: Some(bounded_reason_owned(reason)),
                result_digest: record.result_digest.clone(),
            });
        }
    }
}

fn validate_identifier(value: &str) -> Result<()> {
    if value.is_empty()
        || value.len() > MAX_ID_CHARS
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b':'))
    {
        return Err(error("task_request_invalid"));
    }
    Ok(())
}

fn validate_digest(value: &str) -> Result<()> {
    if value.len() != 64 || !is_lower_hex(value) {
        return Err(error("task_input_digest_invalid"));
    }
    Ok(())
}

/// Recompute the authenticated task input digest on B. The digest binds the
/// actual text and the ordered attachment/skill references; accepting only a
/// well-shaped caller-provided digest would let idempotency describe different
/// execution input than the executor receives.
pub(crate) fn task_input_digest_for_value(input: &Value) -> Result<String> {
    let object = input
        .as_object()
        .ok_or_else(|| error("task_input_digest_invalid"))?;
    let text = object
        .get("text")
        .and_then(Value::as_str)
        .ok_or_else(|| error("task_input_digest_invalid"))?;
    let list = |field: &str| -> Result<Vec<&str>> {
        let values = object
            .get(field)
            .and_then(Value::as_array)
            .filter(|values| values.len() <= 32)
            .ok_or_else(|| error("task_input_digest_invalid"))?;
        values
            .iter()
            .map(|value| {
                value
                    .as_str()
                    .filter(|value| validate_identifier(value).is_ok())
                    .ok_or_else(|| error("task_input_digest_invalid"))
            })
            .collect()
    };
    let attachments = list("attachment_ids")?;
    let skills = list("skill_ids")?;
    let mut hasher = Sha256::new();
    hasher.update(TASK_INPUT_DOMAIN);
    for field in [
        text.as_bytes().to_vec(),
        attachments.join("\0").into_bytes(),
        skills.join("\0").into_bytes(),
    ] {
        hasher.update((field.len() as u64).to_be_bytes());
        hasher.update(field);
    }
    Ok(hex_lower(&hasher.finalize()))
}

fn is_lower_hex(value: &str) -> bool {
    value
        .bytes()
        .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn hex_lower(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        out.push(HEX[(byte >> 4) as usize] as char);
        out.push(HEX[(byte & 0x0f) as usize] as char);
    }
    out
}

fn bounded_reason(reason: &str) -> String {
    bounded_reason_owned(reason.to_owned())
}

fn bounded_reason_owned(reason: String) -> String {
    let sanitized: String = reason
        .chars()
        .filter(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-'))
        .take(MAX_REASON_CHARS)
        .collect();
    if sanitized.is_empty() {
        "unspecified".to_owned()
    } else {
        sanitized
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::protocol::SenderRole;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    fn context(generation: &str) -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: generation.into(),
        }
    }

    fn digest(seed: u8) -> String {
        hex_lower(&[seed; 32])
    }

    fn payload(
        session_id: &str,
        task_id: &str,
        client_request_id: &str,
        input_digest: &str,
    ) -> Value {
        let mut value = json!({
            "v": 1,
            "type": "task_submit",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "1",
            "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": "generation-0001",
            "request_id": client_request_id,
            "task_id": task_id,
            "client_request_id": client_request_id,
            "session_id": session_id,
            "capability_revision": "7",
            "model_source": "controlled_device",
            "mode": "chat",
            "input_digest": input_digest,
            "input": { "text": "run the report", "attachment_ids": [], "skill_ids": [] }
        });
        if input_digest != "not-a-digest" {
            value["input_digest"] = json!(task_input_digest_for_value(&value["input"]).unwrap());
        }
        value
    }

    fn code_payload(
        session_id: &str,
        task_id: &str,
        client_request_id: &str,
        workspace: &str,
    ) -> Value {
        let mut value = payload(session_id, task_id, client_request_id, &digest(2));
        value["mode"] = json!("code");
        value["input"]["workspace_id"] = json!(workspace);
        value
    }

    fn request(body: &Value) -> RemoteTaskRequest {
        RemoteTaskRequest::from_payload(body).unwrap()
    }

    #[test]
    fn task_input_digest_matches_the_frozen_cross_boundary_vector() {
        let input = json!({
            "text": "hello",
            "attachment_ids": ["attachment-1"],
            "skill_ids": ["skill-1"],
        });
        assert_eq!(
            task_input_digest_for_value(&input).unwrap(),
            "edc6e22106c7785a93b3c6f7482a294fe5bea7748b31380728101e07b87a8065"
        );
    }

    struct RecordingExecutor {
        calls: AtomicUsize,
        fail: AtomicBool,
        seen_mode: Mutex<Option<String>>,
        seen_request: Mutex<Option<Value>>,
    }

    impl RecordingExecutor {
        fn ok() -> Arc<Self> {
            Arc::new(Self {
                calls: AtomicUsize::new(0),
                fail: AtomicBool::new(false),
                seen_mode: Mutex::new(None),
                seen_request: Mutex::new(None),
            })
        }

        fn failing() -> Arc<Self> {
            Arc::new(Self {
                calls: AtomicUsize::new(0),
                fail: AtomicBool::new(true),
                seen_mode: Mutex::new(None),
                seen_request: Mutex::new(None),
            })
        }
    }

    #[async_trait]
    impl ControllerTaskExecutor for RecordingExecutor {
        async fn execute(
            &self,
            request: Value,
            _events: Arc<dyn ControllerTaskEventSink>,
            _cancel: CancellationToken,
        ) -> std::result::Result<(), String> {
            *self.seen_request.lock() = Some(request.clone());
            *self.seen_mode.lock() = request
                .get("mode")
                .and_then(Value::as_str)
                .map(str::to_owned);
            self.calls.fetch_add(1, Ordering::SeqCst);
            if self.fail.load(Ordering::SeqCst) {
                Err("executor_error".to_owned())
            } else {
                Ok(())
            }
        }
    }

    #[derive(Default)]
    struct CountingHooks {
        model: AtomicUsize,
        permission: AtomicUsize,
        transfer: AtomicUsize,
        events: Mutex<Vec<RemoteTaskEvent>>,
    }

    impl RemoteTaskModelScope for CountingHooks {
        fn release_task_credentials(&self, _task_id: &str) {
            self.model.fetch_add(1, Ordering::SeqCst);
        }
    }

    impl RemoteTaskPermissionScope for CountingHooks {
        fn deny_pending_for_task(&self, _task_id: &str) -> usize {
            self.permission.fetch_add(1, Ordering::SeqCst);
            1
        }
    }

    impl RemoteTaskTransferScope for CountingHooks {
        fn abort_transfers_for_task(&self, _task_id: &str) -> usize {
            self.transfer.fetch_add(1, Ordering::SeqCst);
            1
        }
    }

    impl RemoteTaskEventSink for CountingHooks {
        fn publish_task_event(&self, event: &RemoteTaskEvent) {
            self.events.lock().push(event.clone());
        }
    }

    #[derive(Default)]
    struct FakeSessions {
        created: Mutex<Vec<String>>,
        existing: Mutex<Vec<String>>,
        claims: Mutex<Vec<(String, bool, Option<String>)>>,
        fail_claim: AtomicBool,
    }

    impl RemoteSessionProvisioner for FakeSessions {
        fn create_authoritative_session(
            &self,
            _title: &str,
            _code_mode: bool,
        ) -> std::result::Result<String, String> {
            let id = format!("session-{:04}", self.created.lock().len() + 1);
            self.created.lock().push(id.clone());
            Ok(id)
        }

        fn session_exists(&self, session_id: &str) -> std::result::Result<bool, String> {
            Ok(self.existing.lock().iter().any(|id| id == session_id))
        }

        fn claim_execution_lane(
            &self,
            session_id: &str,
            code_mode: bool,
            workspace: Option<&Path>,
        ) -> std::result::Result<(), String> {
            self.claims.lock().push((
                session_id.to_owned(),
                code_mode,
                workspace.map(|path| path.to_string_lossy().into_owned()),
            ));
            if self.fail_claim.load(Ordering::SeqCst) {
                Err("backend_bind_failed".to_owned())
            } else {
                Ok(())
            }
        }
    }

    struct NoopSink;

    impl ControllerTaskEventSink for NoopSink {
        fn emit(&self, _event: Value) {}
    }

    #[derive(Default)]
    struct RecordingJournalSink(Mutex<Vec<(JournalKind, Value)>>);

    impl ControllerTaskEventSink for RecordingJournalSink {
        fn emit(&self, _event: Value) {}

        fn emit_journal(
            &self,
            kind: JournalKind,
            _session_id: &str,
            _task_id: Option<&str>,
            payload: Value,
        ) {
            self.0.lock().push((kind, payload));
        }
    }

    fn service(executor: Arc<RecordingExecutor>) -> (RemoteTaskService, Arc<CountingHooks>) {
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.register_executor(TASK_MODE_CHAT, executor);
        let hooks = Arc::new(CountingHooks::default());
        service.hooks().set_model_scope(hooks.clone());
        service.hooks().set_permission_scope(hooks.clone());
        service.hooks().set_transfer_scope(hooks.clone());
        service.hooks().set_journal(hooks.clone());
        (service, hooks)
    }

    fn runtime() -> tokio::runtime::Runtime {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
    }

    fn admitted(service: &RemoteTaskService, body: &Value, now_b: u64) -> RemoteTaskTicket {
        match service
            .submit(&context("generation-0001"), body, now_b)
            .unwrap()
        {
            AdmissionOutcome::Admitted(ticket) => ticket,
            other => panic!("expected admission, got {other:?}"),
        }
    }

    #[test]
    fn executor_failure_emits_one_error_before_task_completion() {
        let (service, hooks) = service(RecordingExecutor::failing());
        let body = payload("session-0001", "task-0001", "request-0001", &digest(1));
        let ticket = admitted(&service, &body, 1_000);
        let sink = Arc::new(RecordingJournalSink::default());
        let record = runtime()
            .block_on(service.dispatch(ticket, sink.clone(), CancellationToken::new(), 1_100))
            .unwrap();
        assert_eq!(record.state, RemoteTaskState::Failed);
        assert_eq!(
            sink.0.lock().as_slice(),
            &[(
                JournalKind::Error,
                json!({
                    "code": "executor_error"
                })
            )]
        );
        assert_eq!(
            hooks.events.lock().last().unwrap().status,
            TASK_STATUS_FAILED
        );
    }

    #[test]
    fn executor_cancellation_emits_one_cancel_before_task_completion() {
        struct CancelExecutor;
        #[async_trait]
        impl ControllerTaskExecutor for CancelExecutor {
            async fn execute(
                &self,
                _request: Value,
                _events: Arc<dyn ControllerTaskEventSink>,
                _cancel: CancellationToken,
            ) -> std::result::Result<(), String> {
                Err(TASK_CANCELLED_CODE.to_owned())
            }
        }

        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.register_executor(TASK_MODE_CHAT, Arc::new(CancelExecutor));
        let hooks = Arc::new(CountingHooks::default());
        service.hooks().set_journal(hooks.clone());
        let body = payload("session-0001", "task-0001", "request-0001", &digest(1));
        let ticket = admitted(&service, &body, 1_000);
        let sink = Arc::new(RecordingJournalSink::default());
        let record = runtime()
            .block_on(service.dispatch(ticket, sink.clone(), CancellationToken::new(), 1_100))
            .unwrap();
        assert_eq!(record.state, RemoteTaskState::Cancelled);
        assert_eq!(
            sink.0.lock().as_slice(),
            &[(
                JournalKind::Cancel,
                json!({
                    "status": TASK_STATUS_CANCELLED
                })
            )]
        );
        assert_eq!(
            hooks.events.lock().last().unwrap().status,
            TASK_STATUS_CANCELLED
        );
    }

    #[test]
    fn admission_requires_the_exact_live_lease_binding() {
        let ledger = RemoteTaskLedger::in_memory();
        let body = payload("session-0001", "task-0001", "client-0001", &digest(1));
        assert_eq!(
            ledger
                .admit(&context("generation-other"), &request(&body), 1_000)
                .unwrap_err()
                .code,
            "task_binding_stale"
        );
        assert!(
            ledger
                .admit(&context("generation-0001"), &request(&body), 1_000)
                .is_ok()
        );
    }

    #[test]
    fn mode_shape_and_input_digest_fail_closed_before_admission() {
        let mut code = payload("session-0001", "task-0001", "client-0001", &digest(1));
        code["mode"] = json!("code");
        assert_eq!(
            RemoteTaskRequest::from_payload(&code).unwrap_err().code,
            "task_workspace_required"
        );

        let mut chat = payload("session-0001", "task-0001", "client-0001", &digest(1));
        chat["input"]["workspace_id"] = json!("workspace-0001");
        assert_eq!(
            RemoteTaskRequest::from_payload(&chat).unwrap_err().code,
            "task_workspace_unexpected"
        );

        let bad_digest = payload("session-0001", "task-0001", "client-0001", "not-a-digest");
        assert_eq!(
            RemoteTaskRequest::from_payload(&bad_digest)
                .unwrap_err()
                .code,
            "task_input_digest_invalid"
        );

        let mut mismatched = payload("session-0001", "task-0001", "client-0001", &digest(1));
        mismatched["input_digest"] = json!(digest(9));
        assert_eq!(
            RemoteTaskRequest::from_payload(&mismatched)
                .unwrap_err()
                .code,
            "task_input_digest_invalid"
        );

        let mut bad_mode = payload("session-0001", "task-0001", "client-0001", &digest(1));
        bad_mode["mode"] = json!("root");
        assert_eq!(
            RemoteTaskRequest::from_payload(&bad_mode).unwrap_err().code,
            "task_mode_invalid"
        );

        let mut bad_source = payload("session-0001", "task-0001", "client-0001", &digest(1));
        bad_source["model_source"] = json!("controller_oauth");
        assert_eq!(
            RemoteTaskRequest::from_payload(&bad_source)
                .unwrap_err()
                .code,
            "task_model_source_invalid"
        );
    }

    #[test]
    fn repeated_client_request_replays_and_conflicting_reuse_is_rejected() {
        let ledger = RemoteTaskLedger::in_memory();
        let live = context("generation-0001");
        let body = payload("session-0001", "task-0001", "client-0001", &digest(1));
        assert!(matches!(
            ledger.admit(&live, &request(&body), 1_000).unwrap(),
            AdmissionOutcome::Admitted(_)
        ));
        match ledger.admit(&live, &request(&body), 1_100).unwrap() {
            AdmissionOutcome::Replay(record) => {
                assert_eq!(record.task_id, "task-0001");
                assert_eq!(record.sequence, 1);
            }
            other => panic!("expected replay, got {other:?}"),
        }

        let mut conflicting = body.clone();
        conflicting["input"]["text"] = json!("run a different report");
        conflicting["input_digest"] =
            json!(task_input_digest_for_value(&conflicting["input"]).unwrap());
        assert_eq!(
            ledger
                .admit(&live, &request(&conflicting), 1_200)
                .unwrap_err()
                .code,
            "task_request_conflict"
        );

        // A different client request ID with the same task ID is also a
        // conflict instead of silently replacing the admitted task.
        let mut other_id = body.clone();
        other_id["client_request_id"] = json!("client-0002");
        assert_eq!(
            ledger
                .admit(&live, &request(&other_id), 1_300)
                .unwrap_err()
                .code,
            "task_request_conflict"
        );
    }

    #[test]
    fn turn_lock_admits_one_task_per_session_and_releases_on_terminal() {
        let ledger = RemoteTaskLedger::in_memory();
        let live = context("generation-0001");
        let first = request(&payload(
            "session-0001",
            "task-0001",
            "client-0001",
            &digest(1),
        ));
        let second = request(&payload(
            "session-0001",
            "task-0002",
            "client-0002",
            &digest(2),
        ));
        ledger.admit(&live, &first, 1_000).unwrap();
        assert_eq!(
            ledger.admit(&live, &second, 1_100).unwrap_err().code,
            "task_session_busy"
        );
        ledger
            .complete("task-0001", RemoteTaskTerminal::Succeeded, None, 1_200)
            .unwrap();
        assert!(ledger.admit(&live, &second, 1_300).is_ok());
        assert_eq!(ledger.live_task_count(), 1);
    }

    #[test]
    fn exactly_one_terminal_state_is_committed() {
        let ledger = RemoteTaskLedger::in_memory();
        let live = context("generation-0001");
        ledger
            .admit(
                &live,
                &request(&payload(
                    "session-0001",
                    "task-0001",
                    "client-0001",
                    &digest(1),
                )),
                1_000,
            )
            .unwrap();
        ledger.begin_dispatch("task-0001", 1_100).unwrap();
        let succeeded = ledger
            .complete(
                "task-0001",
                RemoteTaskTerminal::Succeeded,
                Some(digest(3)),
                1_200,
            )
            .unwrap();
        assert_eq!(succeeded.state, RemoteTaskState::Succeeded);
        let repeated = ledger
            .complete("task-0001", RemoteTaskTerminal::Failed, None, 1_300)
            .unwrap();
        assert_eq!(repeated.state, RemoteTaskState::Succeeded);
        assert_eq!(repeated.result_digest.as_deref(), Some(digest(3).as_str()));
        assert_eq!(repeated.wire_status(), TASK_STATUS_SUCCEEDED);
        assert_eq!(ledger.live_task_count(), 0);
    }

    #[test]
    fn cancel_before_dispatch_rejects_and_after_dispatch_only_requests_it() {
        let ledger = RemoteTaskLedger::in_memory();
        let live = context("generation-0001");
        ledger
            .admit(
                &live,
                &request(&payload(
                    "session-0001",
                    "task-0001",
                    "client-0001",
                    &digest(1),
                )),
                1_000,
            )
            .unwrap();
        match ledger.request_cancel("task-0001", 1_100).unwrap() {
            CancelOutcome::CancelledBeforeDispatch(record) => {
                assert_eq!(record.state, RemoteTaskState::Cancelled);
                assert!(!record.dispatch_committed);
            }
            other => panic!("unexpected {other:?}"),
        }
        match ledger.request_cancel("task-0001", 1_200).unwrap() {
            CancelOutcome::AlreadyTerminal(record) => {
                assert_eq!(record.state, RemoteTaskState::Cancelled);
            }
            other => panic!("unexpected {other:?}"),
        }

        ledger
            .admit(
                &live,
                &request(&payload(
                    "session-0001",
                    "task-0002",
                    "client-0002",
                    &digest(2),
                )),
                1_300,
            )
            .unwrap();
        ledger.begin_dispatch("task-0002", 1_400).unwrap();
        match ledger.request_cancel("task-0002", 1_500).unwrap() {
            CancelOutcome::CancellationRequested(record) => {
                assert!(record.cancel_requested);
                assert!(record.dispatch_committed);
                assert_eq!(record.state, RemoteTaskState::Running);
            }
            other => panic!("unexpected {other:?}"),
        }
    }

    #[test]
    fn interrupt_marks_committed_tasks_result_unknown_and_releases_the_lock() {
        let ledger = RemoteTaskLedger::in_memory();
        let live = context("generation-0001");
        for (task, client, session) in [
            ("task-0001", "client-0001", "session-0001"),
            ("task-0002", "client-0002", "session-0002"),
        ] {
            ledger
                .admit(
                    &live,
                    &request(&payload(session, task, client, &digest(1))),
                    1_000,
                )
                .unwrap();
        }
        ledger.begin_dispatch("task-0001", 1_100).unwrap();
        let interrupted = ledger.interrupt_generation("generation-0001", "stop", 1_200);
        assert_eq!(interrupted.len(), 2);
        let dispatched = interrupted
            .iter()
            .find(|record| record.task_id == "task-0001")
            .unwrap();
        assert_eq!(dispatched.state, RemoteTaskState::Interrupted);
        assert!(dispatched.outcome_unknown);
        assert_eq!(dispatched.wire_status(), TASK_STATUS_RESULT_UNKNOWN);
        let not_dispatched = interrupted
            .iter()
            .find(|record| record.task_id == "task-0002")
            .unwrap();
        assert!(!not_dispatched.outcome_unknown);
        assert_eq!(not_dispatched.wire_status(), TASK_STATUS_INTERRUPTED);
        assert!(
            ledger
                .admit(
                    &live,
                    &request(&payload(
                        "session-0001",
                        "task-0003",
                        "client-0003",
                        &digest(3),
                    )),
                    1_300,
                )
                .is_ok()
        );
    }

    #[test]
    fn generation_scoped_interrupt_leaves_other_generations_alone() {
        let ledger = RemoteTaskLedger::in_memory();
        ledger
            .admit(
                &context("generation-0001"),
                &request(&payload(
                    "session-0001",
                    "task-0001",
                    "client-0001",
                    &digest(1),
                )),
                1_000,
            )
            .unwrap();
        let mut second_body = payload("session-0002", "task-0002", "client-0002", &digest(2));
        second_body["remote_control_generation"] = json!("generation-0002");
        let mut second_context = context("generation-0002");
        second_context.remote_control_generation = "generation-0002".into();
        ledger
            .admit(&second_context, &request(&second_body), 1_000)
            .unwrap();
        let interrupted = ledger.interrupt_generation("generation-0001", "refresh", 1_100);
        assert_eq!(interrupted.len(), 1);
        assert_eq!(
            ledger.record("task-0002").unwrap().state,
            RemoteTaskState::Admitted
        );
    }

    #[test]
    fn durable_restore_does_not_require_or_persist_the_validated_plaintext_input() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("task-ledger.json");
        let mut body = payload("session-1", "task-1", "client-1", &digest(1));
        let sentinel = "private-input-not-in-durable-task-ledger";
        body["input"]["text"] = json!(sentinel);
        body["input_digest"] = json!(task_input_digest_for_value(&body["input"]).unwrap());
        let validated = request(&body);
        {
            let ledger = RemoteTaskLedger::open_recovered(path.clone(), 0).unwrap();
            ledger
                .admit(&context("generation-0001"), &validated, 100)
                .unwrap();
        }
        assert!(!std::fs::read_to_string(&path).unwrap().contains(sentinel));
        let restored = RemoteTaskLedger::open_recovered(path, 0).unwrap();
        let record = restored.record("task-1").unwrap();
        assert_eq!(record.state, RemoteTaskState::Interrupted);
        assert_eq!(record.input_digest, validated.input_digest);
        assert_eq!(record.fingerprint, validated.fingerprint());
        assert_eq!(record.transition_sequence, 2);
        let mut tampered = validated;
        tampered.input["text"] = json!("different input");
        assert_eq!(
            tampered.validate().unwrap_err().code,
            "task_input_digest_invalid"
        );
    }

    #[test]
    fn durable_open_uses_fresh_process_owners_and_retains_terminal_history() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("task-ledger.json");
        {
            let ledger = RemoteTaskLedger::open_recovered(path.clone(), 0).unwrap();
            for index in 1..=3 {
                let body = payload(
                    &format!("session-{index}"),
                    &format!("task-{index}"),
                    &format!("client-{index}"),
                    &digest(index),
                );
                ledger
                    .admit(&context("generation-0001"), &request(&body), 100)
                    .unwrap();
            }
            ledger.begin_dispatch("task-1", 110).unwrap();
            ledger
                .complete("task-1", RemoteTaskTerminal::Succeeded, None, 120)
                .unwrap();
            ledger.begin_dispatch("task-2", 110).unwrap();
        }
        let restored = RemoteTaskLedger::open_recovered(path.clone(), 0).unwrap();
        assert_eq!(restored.records().len(), 3);
        assert_eq!(restored.record("task-1").unwrap().transition_sequence, 3);
        assert_eq!(restored.record("task-2").unwrap().transition_sequence, 3);
        assert_eq!(restored.record("task-3").unwrap().transition_sequence, 2);
        assert_eq!(
            restored.record("task-1").unwrap().state,
            RemoteTaskState::Succeeded
        );
        assert_eq!(
            restored.record("task-2").unwrap().wire_status(),
            TASK_STATUS_RESULT_UNKNOWN
        );
        assert_eq!(
            restored.record("task-3").unwrap().wire_status(),
            TASK_STATUS_INTERRUPTED
        );
        assert_eq!(restored.live_task_count(), 0);
        let body = payload("session-4", "task-4", "client-4", &digest(4));
        restored
            .admit(&context("generation-0001"), &request(&body), 10)
            .unwrap();
        restored.begin_dispatch("task-4", 11).unwrap();
        drop(restored);
        let second = RemoteTaskLedger::open_recovered(path.clone(), 0).unwrap();
        assert_eq!(
            second.records().len(),
            4,
            "a new store must not overwrite prior terminal records"
        );
        assert_eq!(
            second.record("task-1").unwrap().state,
            RemoteTaskState::Succeeded
        );
        assert_eq!(
            second.record("task-4").unwrap().wire_status(),
            TASK_STATUS_RESULT_UNKNOWN
        );
        assert_eq!(JsonTaskLedgerStore::load(&path).unwrap(), second.records());
    }

    #[test]
    fn durable_snapshot_prunes_on_disk_together_with_memory() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("task-ledger.json");
        let ledger = RemoteTaskLedger::open_recovered(path.clone(), 0).unwrap();
        for index in 0..MAX_TRACKED_TASKS + 16 {
            let body = payload(
                "session-0001",
                &format!("task-{index}"),
                &format!("client-{index}"),
                &digest(1),
            );
            ledger
                .admit(&context("generation-0001"), &request(&body), 10)
                .unwrap();
            ledger.begin_dispatch(&format!("task-{index}"), 11).unwrap();
            ledger
                .complete(
                    &format!("task-{index}"),
                    RemoteTaskTerminal::Succeeded,
                    None,
                    12,
                )
                .unwrap();
            let disk = JsonTaskLedgerStore::load(&path).unwrap();
            assert_eq!(disk, ledger.records());
            assert!(disk.len() <= MAX_TRACKED_TASKS);
        }
        drop(ledger);
        let restored = RemoteTaskLedger::open_recovered(path.clone(), 0).unwrap();
        assert_eq!(restored.records().len(), MAX_TRACKED_TASKS);
        assert_eq!(
            restored.record("task-0").err().unwrap().code,
            "task_not_found"
        );
    }

    #[test]
    fn durable_open_rejects_corrupt_oversized_and_duplicate_records_without_overwrite() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("task-ledger.json");
        let ledger = RemoteTaskLedger::in_memory();
        ledger
            .admit(
                &context("generation-0001"),
                &request(&payload(
                    "session-0001",
                    "task-0001",
                    "client-0001",
                    &digest(1),
                )),
                10,
            )
            .unwrap();
        let record = ledger.record("task-0001").unwrap();
        let mut cases = Vec::new();
        let mut bad = record.clone();
        bad.fingerprint = digest(2);
        cases.push(vec![bad]);
        let mut bad = record.clone();
        bad.lease.relay_epoch = "bad/path".into();
        cases.push(vec![bad]);
        let mut bad = record.clone();
        bad.sequence = 0;
        cases.push(vec![bad]);
        let mut bad = record.clone();
        bad.transition_sequence = 0;
        cases.push(vec![bad]);
        let mut bad = record.clone();
        bad.state = RemoteTaskState::Running;
        cases.push(vec![bad]);
        let mut bad = record.clone();
        bad.result_digest = Some("wrong".into());
        cases.push(vec![bad]);
        let mut bad = record.clone();
        bad.terminal_reason = Some("secret/path".into());
        cases.push(vec![bad]);
        let mut bad = record.clone();
        bad.terminal_reason = Some("completed".into());
        cases.push(vec![bad]);
        let mut bad = record.clone();
        bad.result_digest = Some(digest(1));
        cases.push(vec![bad]);
        let mut bad = record.clone();
        bad.state = RemoteTaskState::Succeeded;
        cases.push(vec![bad]);
        cases.push(vec![record.clone(), record.clone()]);
        let other = RemoteTaskLedger::in_memory();
        other
            .admit(
                &context("generation-0001"),
                &request(&payload(
                    "session-0002",
                    "task-0002",
                    "client-0001",
                    &digest(1),
                )),
                10,
            )
            .unwrap();
        let mut duplicate_client = other.record("task-0002").unwrap();
        duplicate_client.sequence = 2;
        cases.push(vec![record.clone(), duplicate_client]);
        for records in cases {
            let bytes = serde_json::to_vec(&LedgerFile {
                version: LEDGER_FILE_VERSION,
                tasks: records,
            })
            .unwrap();
            std::fs::write(&path, &bytes).unwrap();
            assert_eq!(
                RemoteTaskLedger::open_recovered(path.clone(), 0)
                    .err()
                    .unwrap()
                    .code,
                "task_ledger_corrupt"
            );
            assert_eq!(std::fs::read(&path).unwrap(), bytes);
        }
        let bytes = vec![b' '; MAX_LEDGER_FILE_BYTES as usize + 1];
        std::fs::write(&path, &bytes).unwrap();
        assert_eq!(
            RemoteTaskLedger::open_recovered(path.clone(), 0)
                .err()
                .unwrap()
                .code,
            "task_ledger_capacity"
        );
        assert_eq!(std::fs::read(&path).unwrap(), bytes);
        assert!(
            JsonTaskLedgerStore::new(path.clone())
                .persist(&vec![record; MAX_TRACKED_TASKS + 1])
                .is_err()
        );
        assert_eq!(
            std::fs::read(&path).unwrap(),
            bytes,
            "oversized snapshots never write"
        );
        let mut huge = ledger.record("task-0001").unwrap();
        huge.terminal_reason = Some("x".repeat(MAX_LEDGER_FILE_BYTES as usize));
        assert!(
            JsonTaskLedgerStore::new(path.clone())
                .persist(&[huge])
                .is_err()
        );
        assert_eq!(
            std::fs::read(&path).unwrap(),
            bytes,
            "serialized byte cap is symmetric"
        );
        let blocked_parent = directory.path().join("not-a-directory");
        std::fs::write(&blocked_parent, b"untouched").unwrap();
        assert!(RemoteTaskLedger::open_recovered(blocked_parent.join("ledger.json"), 0).is_err());
        assert_eq!(std::fs::read(&blocked_parent).unwrap(), b"untouched");
    }

    #[test]
    fn restart_snapshot_failure_preserves_all_records_and_locks() {
        struct RejectSnapshot;
        impl RemoteTaskLedgerStore for RejectSnapshot {
            fn persist(&self, _records: &[RemoteTaskRecord]) -> std::result::Result<(), String> {
                Err("disk".into())
            }
        }
        let original = RemoteTaskLedger::in_memory();
        for index in 1..=2 {
            original
                .admit(
                    &context("generation-0001"),
                    &request(&payload(
                        &format!("session-{index}"),
                        &format!("task-{index}"),
                        &format!("client-{index}"),
                        &digest(1),
                    )),
                    100,
                )
                .unwrap();
        }
        original.begin_dispatch("task-1", 110).unwrap();
        let records = original.records();
        let restored =
            RemoteTaskLedger::restore(Arc::new(RejectSnapshot), records.clone()).unwrap();
        assert_eq!(
            restored.recover_durable(0).unwrap_err().code,
            "task_ledger_persist_failed"
        );
        assert_eq!(restored.records(), records);
        assert_eq!(restored.live_task_count(), 2);
        assert!(restored.active_for_session("session-1").is_some());
        assert!(restored.active_for_session("session-2").is_some());
    }

    #[test]
    fn transition_sequence_exhaustion_cannot_publish_or_partially_recover() {
        let source = RemoteTaskLedger::in_memory();
        source
            .admit(
                &context("generation-0001"),
                &request(&payload("session-1", "task-1", "client-1", &digest(1))),
                100,
            )
            .unwrap();
        let mut record = source.record("task-1").unwrap();
        record.transition_sequence = u64::MAX;
        let ledger =
            RemoteTaskLedger::restore(Arc::new(InMemoryTaskLedgerStore), vec![record.clone()])
                .unwrap();
        assert_eq!(
            ledger.begin_dispatch("task-1", 110).unwrap_err().code,
            "task_ledger_capacity"
        );
        assert_eq!(ledger.record("task-1").unwrap(), record);
        assert_eq!(
            ledger.recover_durable(0).unwrap_err().code,
            "task_ledger_capacity"
        );
        assert_eq!(ledger.record("task-1").unwrap(), record);
        assert!(ledger.active_for_session("session-1").is_some());
    }

    #[test]
    fn restore_rejects_duplicate_session_locks_sequences_and_live_capacity() {
        let mut records = Vec::new();
        for index in 0..=MAX_LIVE_TASKS {
            let source = RemoteTaskLedger::in_memory();
            source
                .admit(
                    &context("generation-0001"),
                    &request(&payload(
                        &format!("session-{index}"),
                        &format!("task-{index}"),
                        &format!("client-{index}"),
                        &digest(1),
                    )),
                    100,
                )
                .unwrap();
            let mut record = source.records().pop().unwrap();
            record.sequence = index as u64 + 1;
            records.push(record);
        }
        assert_eq!(
            RemoteTaskLedger::restore(Arc::new(InMemoryTaskLedgerStore), records.clone())
                .err()
                .unwrap()
                .code,
            "task_ledger_capacity"
        );
        records.truncate(2);
        records[1].sequence = records[0].sequence;
        assert_eq!(
            RemoteTaskLedger::restore(Arc::new(InMemoryTaskLedgerStore), records)
                .err()
                .unwrap()
                .code,
            "task_ledger_corrupt"
        );
        let mut records = Vec::new();
        for index in 1..=2 {
            let source = RemoteTaskLedger::in_memory();
            source
                .admit(
                    &context("generation-0001"),
                    &request(&payload(
                        "same-session",
                        &format!("task-{index}"),
                        &format!("client-{index}"),
                        &digest(1),
                    )),
                    100,
                )
                .unwrap();
            let mut record = source.records().pop().unwrap();
            record.sequence = index;
            records.push(record);
        }
        assert_eq!(
            RemoteTaskLedger::restore(Arc::new(InMemoryTaskLedgerStore), records)
                .err()
                .unwrap()
                .code,
            "task_ledger_corrupt"
        );
    }

    #[test]
    fn restart_recovery_terminates_every_persisted_non_terminal_task() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("task-ledger.json");
        let store: Arc<dyn RemoteTaskLedgerStore> =
            Arc::new(JsonTaskLedgerStore::new(path.clone()));
        let ledger = RemoteTaskLedger::new(store.clone());
        ledger
            .admit(
                &context("generation-0001"),
                &request(&payload(
                    "session-0001",
                    "task-0001",
                    "client-0001",
                    &digest(1),
                )),
                1_000,
            )
            .unwrap();
        ledger.begin_dispatch("task-0001", 1_100).unwrap();
        let records = JsonTaskLedgerStore::load(&path).unwrap();
        assert_eq!(records.len(), 1);
        let restored = RemoteTaskLedger::restore(store, records).unwrap();
        assert_eq!(
            restored
                .active_for_session("session-0001")
                .map(|record| record.task_id),
            Some("task-0001".to_owned())
        );
        let recovered = restored.recover_after_restart(2_000);
        assert_eq!(recovered.len(), 1);
        assert_eq!(recovered[0].state, RemoteTaskState::Interrupted);
        assert_eq!(recovered[0].wire_status(), TASK_STATUS_RESULT_UNKNOWN);
        assert_eq!(
            recovered[0].terminal_reason.as_deref(),
            Some("process_restart")
        );
        assert!(restored.active_for_session("session-0001").is_none());
    }

    #[test]
    fn persist_failure_fails_closed_for_admission_and_terminal_transitions() {
        struct FailingStore;
        impl RemoteTaskLedgerStore for FailingStore {
            fn persist(&self, _records: &[RemoteTaskRecord]) -> std::result::Result<(), String> {
                Err("disk".to_owned())
            }
        }
        let ledger = RemoteTaskLedger::new(Arc::new(FailingStore));
        let live = context("generation-0001");
        let body = payload("session-0001", "task-0001", "client-0001", &digest(1));
        assert_eq!(
            ledger
                .admit(&live, &request(&body), 1_000)
                .unwrap_err()
                .code,
            "task_ledger_persist_failed"
        );
        assert_eq!(ledger.live_task_count(), 0);
    }

    #[test]
    fn lifecycle_and_cleanup_hooks_run_for_every_terminal_path() {
        let executor = RecordingExecutor::ok();
        let (task_service, hooks) = service(executor.clone());
        let ticket = admitted(
            &task_service,
            &payload("session-0001", "task-0001", "client-0001", &digest(1)),
            1_000,
        );
        let record = runtime()
            .block_on(task_service.dispatch(
                ticket,
                Arc::new(NoopSink),
                CancellationToken::new(),
                1_100,
            ))
            .unwrap();
        assert_eq!(record.state, RemoteTaskState::Succeeded);
        assert_eq!(executor.calls.load(Ordering::SeqCst), 1);
        let seen = executor.seen_request.lock();
        let seen = seen.as_ref().expect("complete executor request");
        assert_eq!(seen["task_id"], "task-0001");
        assert_eq!(seen["session_id"], "session-0001");
        assert_eq!(seen["mode"], TASK_MODE_CHAT);
        assert_eq!(seen["model_source"], TASK_MODEL_SOURCE_CONTROLLED);
        assert_eq!(seen["connection_id"], "connection-0001");
        assert_eq!(seen["controller_lease_id"], "lease-0001");
        assert_eq!(seen["input"]["text"], "run the report");
        assert_eq!(hooks.model.load(Ordering::SeqCst), 1);
        assert_eq!(hooks.permission.load(Ordering::SeqCst), 1);
        assert_eq!(hooks.transfer.load(Ordering::SeqCst), 1);
        let statuses: Vec<String> = hooks
            .events
            .lock()
            .iter()
            .map(|event| event.status.clone())
            .collect();
        assert_eq!(
            statuses,
            vec![
                TASK_STATUS_ADMITTED.to_owned(),
                TASK_STATUS_RUNNING.to_owned(),
                TASK_STATUS_SUCCEEDED.to_owned()
            ]
        );

        // A failing executor, including a temporary-model preparation/DNS
        // failure before the first model call, still follows the observable
        // admitted -> running -> failed sequence and reaches cleanup.
        let failing_executor = RecordingExecutor::failing();
        let (failing_service, failing_hooks) = service(failing_executor.clone());
        let ticket = admitted(
            &failing_service,
            &payload("session-0002", "task-0002", "client-0002", &digest(2)),
            2_000,
        );
        let record = runtime()
            .block_on(failing_service.dispatch(
                ticket,
                Arc::new(NoopSink),
                CancellationToken::new(),
                2_100,
            ))
            .unwrap();
        assert_eq!(record.state, RemoteTaskState::Failed);
        assert_eq!(record.terminal_reason.as_deref(), Some("executor_error"));
        let failing_statuses: Vec<String> = failing_hooks
            .events
            .lock()
            .iter()
            .map(|event| event.status.clone())
            .collect();
        assert_eq!(
            failing_statuses,
            vec![
                TASK_STATUS_ADMITTED.to_owned(),
                TASK_STATUS_RUNNING.to_owned(),
                TASK_STATUS_FAILED.to_owned()
            ]
        );
        assert_eq!(
            failing_service
                .ledger()
                .record("task-0002")
                .unwrap()
                .terminal_reason
                .as_deref(),
            Some("executor_error")
        );
        assert_eq!(failing_hooks.model.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn published_attachments_reach_the_executor_only_when_ids_match_exactly() {
        struct AttachmentExecutor(Mutex<Vec<super::super::transfer::upload::PublishedAttachment>>);

        #[async_trait]
        impl ControllerTaskExecutor for AttachmentExecutor {
            async fn execute(
                &self,
                _request: Value,
                _events: Arc<dyn ControllerTaskEventSink>,
                _cancel: CancellationToken,
            ) -> std::result::Result<(), String> {
                Err("task_attachments_unavailable".to_owned())
            }

            async fn execute_with_attachments(
                &self,
                _request: Value,
                attachments: Vec<super::super::transfer::upload::PublishedAttachment>,
                _events: Arc<dyn ControllerTaskEventSink>,
                _cancel: CancellationToken,
            ) -> std::result::Result<(), String> {
                *self.0.lock() = attachments;
                Ok(())
            }
        }

        let executor = Arc::new(AttachmentExecutor(Mutex::new(Vec::new())));
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.register_executor(TASK_MODE_CHAT, executor.clone());
        let mut body = payload("session-0001", "task-0001", "client-0001", &digest(1));
        body["input"]["attachment_ids"] = json!(["ctr-up-00000001-1"]);
        body["input_digest"] = json!(task_input_digest_for_value(&body["input"]).unwrap());
        let ticket = admitted(&service, &body, 1_000);
        let published = super::super::transfer::upload::PublishedAttachment {
            handle_id: "ctr-up-00000001-1".into(),
            path: std::env::temp_dir().join("remote-attachment.txt"),
            display_name: "remote-attachment.txt".into(),
            mime_type: "text/plain".into(),
            bytes: 5,
            sha256: digest(3),
            read_owner: None,
        };
        let record = runtime()
            .block_on(service.dispatch_with_attachments(
                ticket,
                vec![published.clone()],
                Arc::new(NoopSink),
                CancellationToken::new(),
                1_100,
            ))
            .unwrap();
        assert_eq!(record.state, RemoteTaskState::Succeeded);
        assert_eq!(executor.0.lock().as_slice(), &[published]);

        let mut second = payload("session-0002", "task-0002", "client-0002", &digest(2));
        second["input"]["attachment_ids"] = json!(["ctr-up-00000002-2"]);
        second["input_digest"] = json!(task_input_digest_for_value(&second["input"]).unwrap());
        let ticket = admitted(&service, &second, 1_200);
        let rejected = runtime()
            .block_on(service.dispatch(ticket, Arc::new(NoopSink), CancellationToken::new(), 1_300))
            .unwrap();
        assert_eq!(rejected.state, RemoteTaskState::Failed);
        assert_eq!(
            rejected.terminal_reason.as_deref(),
            Some("task_attachments_invalid")
        );
        assert_eq!(
            executor.0.lock().len(),
            1,
            "the rejected turn never reached execution"
        );
    }

    #[test]
    fn admitted_setup_failure_is_terminal_and_releases_task_resources_and_turn_lock() {
        let executor = RecordingExecutor::ok();
        let (service, hooks) = service(executor.clone());
        let _ticket = admitted(
            &service,
            &payload("session-0001", "task-0001", "client-0001", &digest(1)),
            1_000,
        );
        let failed = service
            .fail_admitted("task-0001", "task_attachment_unconfirmed", 1_100)
            .unwrap();
        assert_eq!(failed.state, RemoteTaskState::Failed);
        assert_eq!(
            failed.terminal_reason.as_deref(),
            Some("task_attachment_unconfirmed")
        );
        assert_eq!(executor.calls.load(Ordering::SeqCst), 0);
        assert_eq!(hooks.model.load(Ordering::SeqCst), 1);
        assert_eq!(hooks.permission.load(Ordering::SeqCst), 1);
        assert_eq!(hooks.transfer.load(Ordering::SeqCst), 1);
        assert_eq!(
            service
                .fail_admitted("task-0001", "again", 1_200)
                .unwrap_err()
                .code,
            "task_state_invalid"
        );
        let next = admitted(
            &service,
            &payload("session-0001", "task-0002", "client-0002", &digest(2)),
            1_300,
        );
        assert_eq!(next.record.task_id, "task-0002");
    }

    #[test]
    fn chat_turn_port_receives_b_local_published_file_not_just_wire_handle() {
        struct AttachmentPort(Mutex<Vec<super::super::transfer::upload::PublishedAttachment>>);

        #[async_trait]
        impl RemoteChatTurnPort for AttachmentPort {
            async fn run_chat_turn(
                &self,
                _session_id: &str,
                _task_id: &str,
                _model_source: &str,
                _lease: &LeaseContext,
                _content: String,
                _skill_ids: Vec<String>,
                _events: Arc<dyn ControllerTaskEventSink>,
                _cancel: CancellationToken,
            ) -> std::result::Result<Option<String>, String> {
                panic!("attachment execution must use the attachment-aware port")
            }

            async fn run_chat_turn_with_attachments(
                &self,
                session_id: &str,
                task_id: &str,
                _model_source: &str,
                _lease: &LeaseContext,
                content: String,
                skill_ids: Vec<String>,
                attachments: Vec<super::super::transfer::upload::PublishedAttachment>,
                _events: Arc<dyn ControllerTaskEventSink>,
                _cancel: CancellationToken,
            ) -> std::result::Result<Option<String>, String> {
                assert_eq!(session_id, "session-0001");
                assert_eq!(task_id, "task-0001");
                assert_eq!(content, "run the report");
                assert_eq!(skill_ids, ["skill-1"]);
                *self.0.lock() = attachments;
                Ok(None)
            }
        }

        let port = Arc::new(AttachmentPort(Mutex::new(Vec::new())));
        let executor = ChatTurnExecutor::new(port.clone());
        let mut request = payload("session-0001", "task-0001", "client-0001", &digest(1));
        request["input"]["skill_ids"] = json!(["skill-1"]);
        request["input_digest"] = json!(task_input_digest_for_value(&request["input"]).unwrap());
        let descriptor = super::super::transfer::upload::PublishedAttachment {
            handle_id: "ctr-up-00000001-1".into(),
            path: std::env::temp_dir().join("remote-attachment.txt"),
            display_name: "remote-attachment.txt".into(),
            mime_type: "text/plain".into(),
            bytes: 5,
            sha256: digest(3),
            read_owner: None,
        };
        runtime()
            .block_on(executor.execute_with_attachments(
                request,
                vec![descriptor.clone()],
                Arc::new(NoopSink),
                CancellationToken::new(),
            ))
            .unwrap();
        assert_eq!(port.0.lock().as_slice(), &[descriptor]);
    }

    #[test]
    fn duplicate_client_request_runs_the_executor_once() {
        let executor = RecordingExecutor::ok();
        let (service, _hooks) = service(executor.clone());
        let body = payload("session-0001", "task-0001", "client-0001", &digest(1));
        let ticket = admitted(&service, &body, 1_000);
        runtime()
            .block_on(service.dispatch(ticket, Arc::new(NoopSink), CancellationToken::new(), 1_100))
            .unwrap();
        match service
            .submit(&context("generation-0001"), &body, 1_200)
            .unwrap()
        {
            AdmissionOutcome::Replay(record) => {
                assert_eq!(record.state, RemoteTaskState::Succeeded);
            }
            other => panic!("unexpected {other:?}"),
        }
        assert_eq!(executor.calls.load(Ordering::SeqCst), 1);
    }

    struct HangingExecutor {
        started: Arc<tokio::sync::Notify>,
        observed_cancel: AtomicBool,
    }

    #[async_trait]
    impl ControllerTaskExecutor for HangingExecutor {
        async fn execute(
            &self,
            _request: Value,
            _events: Arc<dyn ControllerTaskEventSink>,
            cancel: CancellationToken,
        ) -> std::result::Result<(), String> {
            self.started.notify_one();
            cancel.cancelled().await;
            self.observed_cancel.store(true, Ordering::SeqCst);
            Err(TASK_CANCELLED_CODE.to_owned())
        }
    }

    #[test]
    fn cancel_actually_interrupts_the_running_executor() {
        let executor = Arc::new(HangingExecutor {
            started: Arc::new(tokio::sync::Notify::new()),
            observed_cancel: AtomicBool::new(false),
        });
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.register_executor(TASK_MODE_CHAT, executor.clone());
        let ticket = admitted(
            &service,
            &payload("session-0001", "task-0001", "client-0001", &digest(1)),
            1_000,
        );
        let record = runtime().block_on(async {
            let dispatch = service.dispatch(
                ticket,
                Arc::new(NoopSink),
                CancellationToken::new(),
                1_100,
            );
            tokio::pin!(dispatch);
            tokio::select! {
                _ = executor.started.notified() => {}
                outcome = &mut dispatch => panic!("dispatch finished before cancellation: {outcome:?}"),
            }
            match service.cancel("task-0001", 1_200).unwrap() {
                CancelOutcome::CancellationRequested(_) => {}
                other => panic!("unexpected cancel outcome: {other:?}"),
            }
            dispatch.await.unwrap()
        });
        assert_eq!(record.state, RemoteTaskState::Cancelled);
        assert!(executor.observed_cancel.load(Ordering::SeqCst));
        assert_eq!(
            record.terminal_reason.as_deref(),
            Some("cancelled"),
            "the executor-confirmed terminal must be recorded"
        );
    }

    #[test]
    fn remote_cancel_rejects_foreign_session_and_every_stale_lease_field_atomically() {
        let (service, hooks) = service(RecordingExecutor::ok());
        service.set_session_provisioner(Arc::new(FakeSessions::default()));
        let session = service
            .ensure_remote_session("ref-chat", "Chat", false, "generation-0001")
            .unwrap();
        service
            .sessions
            .bind(
                "ref-other",
                "session-0002",
                "generation-0001",
                RemoteSessionLane::Chat,
            )
            .unwrap();
        admitted(
            &service,
            &payload(&session, "task-0001", "client-0001", &digest(1)),
            1_000,
        );
        let before = serde_json::to_value(service.status("task-0001").unwrap()).unwrap();
        let events_before = hooks.events.lock().len();
        let live = context("generation-0001");
        for remote_ref in ["ref-other", "ref-unknown"] {
            assert_eq!(
                service
                    .prepare_remote_cancel(&live, remote_ref, "task-0001", 1_100)
                    .unwrap_err()
                    .code,
                "task_binding_stale"
            );
        }
        for field in 0..5 {
            let mut stale = live.clone();
            match field {
                0 => stale.connection_id = "connection-other".into(),
                1 => stale.controller_lease_id = "lease-other".into(),
                2 => stale.lease_epoch = "2".into(),
                3 => stale.relay_epoch = "relay-other".into(),
                _ => stale.remote_control_generation = "generation-other".into(),
            }
            assert_eq!(
                service
                    .prepare_remote_cancel(&stale, "ref-chat", "task-0001", 1_100)
                    .unwrap_err()
                    .code,
                "task_binding_stale"
            );
            assert_eq!(
                serde_json::to_value(service.status("task-0001").unwrap()).unwrap(),
                before
            );
        }
        assert_eq!(hooks.events.lock().len(), events_before);
        let outcome = service
            .prepare_remote_cancel(&live, "ref-chat", "task-0001", 1_200)
            .unwrap();
        assert!(matches!(outcome, CancelOutcome::CancelledBeforeDispatch(_)));
        service.finish_cancel_outcome(&outcome);
        assert_eq!(
            service.status("task-0001").unwrap().wire_status(),
            "cancelled"
        );
        // Terminal retries still need authority; an AlreadyTerminal response
        // must not disclose another session's task.
        assert!(
            service
                .prepare_remote_cancel(&live, "ref-other", "task-0001", 1_300)
                .is_err()
        );
        assert!(matches!(
            service
                .prepare_remote_cancel(&live, "ref-chat", "task-0001", 1_300)
                .unwrap(),
            CancelOutcome::AlreadyTerminal(_)
        ));
    }

    #[test]
    fn local_stop_requires_current_lease_and_bound_b_session() {
        let (service, hooks) = service(RecordingExecutor::ok());
        service.set_session_provisioner(Arc::new(FakeSessions::default()));
        let session_id = service
            .ensure_remote_session("ref-chat", "Chat", false, "generation-0001")
            .unwrap();
        assert_eq!(session_id, "session-0001");
        let body = payload(&session_id, "task-0001", "client-0001", &digest(1));
        admitted(&service, &body, 1_000);
        assert_eq!(
            service
                .prepare_local_cancel(&context("generation-0002"), "task-0001", 1_100)
                .unwrap_err()
                .code,
            "task_binding_stale"
        );
        assert_eq!(
            service.status("task-0001").unwrap().wire_status(),
            "admitted"
        );
        let outcome = service
            .prepare_local_cancel(&context("generation-0001"), "task-0001", 1_200)
            .unwrap();
        assert!(matches!(outcome, CancelOutcome::CancelledBeforeDispatch(_)));
        service.finish_cancel_outcome(&outcome);
        assert_eq!(
            service.status("task-0001").unwrap().wire_status(),
            "cancelled"
        );
        let projected = hooks.events.lock();
        let event = projected.last().unwrap();
        assert_eq!(event.generation, "generation-0001");
        assert_eq!(event.model_source, RemoteTaskModelSource::ControlledDevice);
        assert_eq!(
            serde_json::to_value(event).unwrap()["modelSource"],
            "controlled_device"
        );
        drop(projected);
        service.clear_generation_sessions("generation-0001");
        assert_eq!(
            service
                .prepare_local_cancel(&context("generation-0001"), "task-0001", 1_300)
                .unwrap_err()
                .code,
            "task_binding_stale"
        );
    }

    #[test]
    fn lifecycle_cleanup_cancels_the_running_executor() {
        let executor = Arc::new(HangingExecutor {
            started: Arc::new(tokio::sync::Notify::new()),
            observed_cancel: AtomicBool::new(false),
        });
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.register_executor(TASK_MODE_CHAT, executor.clone());
        let ticket = admitted(
            &service,
            &payload("session-0001", "task-0001", "client-0001", &digest(1)),
            1_000,
        );

        let record = runtime().block_on(async {
            let dispatch =
                service.dispatch(ticket, Arc::new(NoopSink), CancellationToken::new(), 1_100);
            tokio::pin!(dispatch);
            tokio::select! {
                _ = executor.started.notified() => {}
                outcome = &mut dispatch => panic!("dispatch finished before cleanup: {outcome:?}"),
            }
            let interrupted = service.interrupt_generation_and_clear_sessions(
                "generation-0001",
                "remote_control_stopped",
                1_200,
            );
            assert_eq!(interrupted.len(), 1);
            dispatch.await.unwrap()
        });

        assert_eq!(record.state, RemoteTaskState::Interrupted);
        assert!(executor.observed_cancel.load(Ordering::SeqCst));
        assert_eq!(
            record.terminal_reason.as_deref(),
            Some("remote_control_stopped")
        );
    }

    /// E2E-05：chat 执行器把用户消息与 assistant 回复都交给正文事件端口
    /// （命令层据此写权威 journal 并回传 A）。
    #[test]
    fn chat_executor_journals_the_user_message_and_the_reply() {
        struct Port;

        #[async_trait]
        impl RemoteChatTurnPort for Port {
            async fn run_chat_turn(
                &self,
                _session_id: &str,
                _task_id: &str,
                _model_source: &str,
                _lease: &LeaseContext,
                content: String,
                _skill_ids: Vec<String>,
                _events: Arc<dyn ControllerTaskEventSink>,
                _cancel: CancellationToken,
            ) -> std::result::Result<Option<String>, String> {
                assert_eq!(content, "run the report");
                Ok(Some("hello from B".to_owned()))
            }
        }

        #[derive(Default)]
        struct Recording(Mutex<Vec<(String, Option<String>, Value)>>);

        impl ControllerTaskEventSink for Recording {
            fn emit(&self, _event: Value) {}

            fn emit_journal(
                &self,
                kind: JournalKind,
                _session_id: &str,
                task_id: Option<&str>,
                payload: Value,
            ) {
                self.0
                    .lock()
                    .push((kind.wire().to_owned(), task_id.map(str::to_owned), payload));
            }
        }

        let sink = Arc::new(Recording::default());
        let executor = ChatTurnExecutor::new(Arc::new(Port));
        let mut request = payload("session-0001", "task-0001", "client-0001", &digest(1));
        request["input"]["skill_ids"] = json!(["skill-1"]);
        request["input_digest"] = json!(task_input_digest_for_value(&request["input"]).unwrap());
        runtime()
            .block_on(executor.execute(request, sink.clone(), CancellationToken::new()))
            .unwrap();
        let events = sink.0.lock();
        assert_eq!(events.len(), 2, "用户消息与回复各一条正文事件");
        assert_eq!(events[0].0, "user_message");
        assert_eq!(events[0].1.as_deref(), Some("task-0001"));
        assert_eq!(events[0].2["text"], "run the report");
        assert_eq!(events[1].0, "assistant_delta");
        assert_eq!(events[1].1.as_deref(), Some("task-0001"));
        assert_eq!(events[1].2["text"], "hello from B");
    }

    #[test]
    fn code_executor_journals_the_user_message_before_acp_submission() {
        struct Port(Arc<AtomicBool>);
        #[async_trait]
        impl RemoteCodeTurnPort for Port {
            async fn run_code_turn_with_attachments(
                &self,
                _session_id: &str,
                _task_id: &str,
                _lease: &LeaseContext,
                _workspace_id: &str,
                content: String,
                skill_ids: Vec<String>,
                _attachments: Vec<PublishedAttachment>,
                _events: Arc<dyn ControllerTaskEventSink>,
                _cancel: CancellationToken,
            ) -> std::result::Result<(), String> {
                assert!(self.0.load(Ordering::SeqCst));
                assert_eq!(content, "run the report");
                assert_eq!(skill_ids, ["code-skill"]);
                Ok(())
            }
        }

        struct Recording {
            recorded: Arc<AtomicBool>,
            events: Mutex<Vec<(JournalKind, Value)>>,
        }
        impl ControllerTaskEventSink for Recording {
            fn emit(&self, _event: Value) {}
            fn emit_journal(
                &self,
                kind: JournalKind,
                _session_id: &str,
                _task_id: Option<&str>,
                payload: Value,
            ) {
                self.recorded.store(true, Ordering::SeqCst);
                self.events.lock().push((kind, payload));
            }
        }

        let recorded = Arc::new(AtomicBool::new(false));
        let sink = Arc::new(Recording {
            recorded: recorded.clone(),
            events: Mutex::new(Vec::new()),
        });
        let executor = CodeTurnExecutor::new(Arc::new(Port(recorded)));
        let mut request =
            code_payload("session-0001", "task-0001", "client-0001", "workspace-0001");
        request["input"]["skill_ids"] = json!(["code-skill"]);
        request["input_digest"] = json!(task_input_digest_for_value(&request["input"]).unwrap());
        runtime()
            .block_on(executor.execute(request, sink.clone(), CancellationToken::new()))
            .unwrap();
        assert_eq!(
            sink.events.lock().as_slice(),
            &[(
                JournalKind::UserMessage,
                json!({ "text": "run the report" })
            )]
        );
    }

    #[test]
    fn missing_executor_for_a_mode_fails_the_task_instead_of_hanging() {
        let (service, _hooks) = service(RecordingExecutor::ok());
        let code = code_payload("session-0001", "task-0001", "client-0001", "workspace-0001");
        let ticket = admitted(&service, &code, 1_000);
        let record = runtime()
            .block_on(service.dispatch(ticket, Arc::new(NoopSink), CancellationToken::new(), 1_100))
            .unwrap();
        assert_eq!(record.state, RemoteTaskState::Failed);
        assert_eq!(
            record.terminal_reason.as_deref(),
            Some("executor_unavailable")
        );
    }

    #[test]
    fn adapters_reject_a_mismatched_mode_before_touching_the_engine() {
        struct PanickingPort;

        #[async_trait]
        impl RemoteChatTurnPort for PanickingPort {
            async fn run_chat_turn(
                &self,
                _session_id: &str,
                _task_id: &str,
                _model_source: &str,
                _lease: &LeaseContext,
                _content: String,
                _skill_ids: Vec<String>,
                _events: Arc<dyn ControllerTaskEventSink>,
                _cancel: CancellationToken,
            ) -> std::result::Result<Option<String>, String> {
                panic!("the chat port must not be reached for a code payload");
            }
        }

        let executor = ChatTurnExecutor::new(Arc::new(PanickingPort));
        let result = runtime().block_on(executor.execute(
            code_payload("session-0001", "task-0001", "client-0001", "workspace-0001"),
            Arc::new(NoopSink),
            CancellationToken::new(),
        ));
        assert_eq!(result.unwrap_err(), "task_mode_mismatch");
    }

    #[test]
    fn ensure_remote_session_is_idempotent_under_concurrent_preparation() {
        let service = Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        )));
        let sessions = Arc::new(FakeSessions::default());
        service.set_session_provisioner(sessions.clone());
        let barrier = Arc::new(std::sync::Barrier::new(8));
        let handles: Vec<_> = (0..8)
            .map(|_| {
                let service = service.clone();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    barrier.wait();
                    service
                        .ensure_remote_session(
                            "remote-session-shared",
                            "Remote",
                            false,
                            "generation-0001",
                        )
                        .unwrap()
                })
            })
            .collect();
        let ids: Vec<_> = handles
            .into_iter()
            .map(|handle| handle.join().unwrap())
            .collect();

        assert!(ids.iter().all(|id| id == "session-0001"));
        assert_eq!(sessions.created.lock().len(), 1);
        assert_eq!(
            service.lane_for_session("session-0001", "generation-0001"),
            Some(RemoteSessionLane::Chat)
        );
        assert_eq!(
            service
                .session_for("remote-session-shared", "generation-0001")
                .as_deref(),
            Some("session-0001")
        );
    }

    #[test]
    fn known_lane_is_idempotent_but_conflicting_ensure_fails_closed() {
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        let sessions = Arc::new(FakeSessions::default());
        service.set_session_provisioner(sessions.clone());
        assert_eq!(
            service
                .ensure_remote_session("ref-chat", "Remote", false, "generation-0001")
                .unwrap(),
            "session-0001"
        );
        assert_eq!(
            service
                .ensure_remote_session("ref-chat", "Remote", false, "generation-0001")
                .unwrap(),
            "session-0001"
        );
        assert_eq!(
            service
                .ensure_remote_session("ref-chat", "Remote", true, "generation-0001")
                .unwrap_err()
                .code,
            "task_session_lane_conflict"
        );
        assert_eq!(sessions.created.lock().len(), 1);
        assert_eq!(
            service.lane_for_session("session-0001", "generation-0001"),
            Some(RemoteSessionLane::Chat)
        );
        sessions.existing.lock().push("session-0001".into());
        service
            .switch_remote_session("ref-chat", "session-0001", "generation-0001")
            .unwrap();
        assert_eq!(
            service.lane_for_session("session-0001", "generation-0001"),
            Some(RemoteSessionLane::Chat),
            "switching to the same binding must preserve its trusted lane"
        );
        assert_eq!(
            service
                .ensure_remote_session("ref-chat", "Remote", true, "generation-0001")
                .unwrap_err()
                .code,
            "task_session_lane_conflict"
        );
    }

    #[test]
    fn code_mode_open_and_unknown_code_claim_are_never_chat() {
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        let sessions = Arc::new(FakeSessions::default());
        sessions.existing.lock().push("session-existing".into());
        service.set_session_provisioner(sessions);
        let code_id = service
            .open_remote_session("ref-code", "Code", true, "generation-0001")
            .unwrap();
        assert_eq!(
            service.lane_for_session(&code_id, "generation-0001"),
            Some(RemoteSessionLane::Code)
        );
        service
            .switch_remote_session("ref-existing", "session-existing", "generation-0001")
            .unwrap();
        assert_eq!(
            service.lane_for_session("session-existing", "generation-0001"),
            Some(RemoteSessionLane::Unknown)
        );
        assert_eq!(
            service
                .ensure_remote_session("ref-existing", "Code", true, "generation-0001")
                .unwrap_err()
                .code,
            "task_session_lane_unknown"
        );
        let authenticated = request(&code_payload(
            "ref-existing",
            "task-claim-code",
            "request-claim-code",
            "workspace-0001",
        ));
        assert_eq!(
            service
                .ensure_authenticated_task_session(
                    &authenticated,
                    &context("generation-0002"),
                    "Code"
                )
                .unwrap_err()
                .code,
            "task_lease_mismatch"
        );
        assert_eq!(
            service.lane_for_session("session-existing", "generation-0001"),
            Some(RemoteSessionLane::Unknown)
        );
        assert_eq!(
            service
                .ensure_authenticated_task_session(
                    &authenticated,
                    &context("generation-0001"),
                    "Code"
                )
                .unwrap(),
            "session-existing"
        );
        assert_eq!(
            service.lane_for_session("session-existing", "generation-0001"),
            Some(RemoteSessionLane::Code)
        );
        assert_eq!(
            service
                .ensure_authenticated_task_session(
                    &request(&payload(
                        "ref-existing",
                        "task-conflicting-chat",
                        "request-conflicting-chat",
                        &digest(1)
                    )),
                    &context("generation-0001"),
                    "Chat"
                )
                .unwrap_err()
                .code,
            "task_session_lane_conflict"
        );
    }

    #[test]
    fn authenticated_code_claim_binds_backend_before_lane_and_local_input_reuses_it() {
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        let sessions = Arc::new(FakeSessions::default());
        sessions.existing.lock().push("session-existing".into());
        service.set_session_provisioner(sessions.clone());
        service
            .switch_remote_session("ref-existing", "session-existing", "generation-0001")
            .unwrap();

        let authenticated = request(&code_payload(
            "ref-existing",
            "task-claim-code",
            "request-claim-code",
            "workspace-0001",
        ));
        assert_eq!(
            service
                .ensure_authenticated_task_session(
                    &authenticated,
                    &context("generation-0001"),
                    "Code",
                )
                .unwrap(),
            "session-existing"
        );
        assert_eq!(
            sessions.claims.lock().as_slice(),
            &[(
                "session-existing".to_owned(),
                true,
                Some("workspace-0001".to_owned()),
            )]
        );
        assert_eq!(
            service
                .claim_local_session("session-existing", "generation-0001")
                .unwrap(),
            ("ref-existing".to_owned(), RemoteSessionLane::Code)
        );
        assert_eq!(
            sessions.claims.lock().len(),
            1,
            "the B-local turn reuses the established ACP binding"
        );
    }

    #[test]
    fn backend_bind_failure_keeps_the_switched_session_lane_unknown() {
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        let sessions = Arc::new(FakeSessions::default());
        sessions.existing.lock().push("session-existing".into());
        sessions.fail_claim.store(true, Ordering::SeqCst);
        service.set_session_provisioner(sessions.clone());
        service
            .switch_remote_session("ref-existing", "session-existing", "generation-0001")
            .unwrap();

        let authenticated = request(&code_payload(
            "ref-existing",
            "task-claim-code",
            "request-claim-code",
            "workspace-0001",
        ));
        assert_eq!(
            service
                .ensure_authenticated_task_session(
                    &authenticated,
                    &context("generation-0001"),
                    "Code",
                )
                .unwrap_err()
                .code,
            "task_session_backend_bind_failed"
        );
        assert_eq!(
            service.lane_for_session("session-existing", "generation-0001"),
            Some(RemoteSessionLane::Unknown),
            "the lane cannot become visible until the backend bind succeeds"
        );
        assert_eq!(
            sessions.claims.lock().as_slice(),
            &[(
                "session-existing".to_owned(),
                true,
                Some("workspace-0001".to_owned()),
            )]
        );
    }

    #[test]
    fn switched_unknown_lane_is_claimed_once_under_concurrent_ensure() {
        let service = Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        )));
        let sessions = Arc::new(FakeSessions::default());
        sessions.existing.lock().push("session-existing".into());
        service.set_session_provisioner(sessions.clone());
        service
            .switch_remote_session("ref-existing", "session-existing", "generation-0001")
            .unwrap();
        assert_eq!(
            service.lane_for_session("session-existing", "generation-0001"),
            Some(RemoteSessionLane::Unknown)
        );
        let barrier = Arc::new(std::sync::Barrier::new(8));
        let handles: Vec<_> = (0..8)
            .map(|index| {
                let service = service.clone();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    barrier.wait();
                    let body = if index % 2 == 1 {
                        code_payload(
                            "ref-existing",
                            "task-claim-code",
                            "request-claim-code",
                            "workspace-0001",
                        )
                    } else {
                        payload(
                            "ref-existing",
                            "task-claim-chat",
                            "request-claim-chat",
                            &digest(1),
                        )
                    };
                    service.ensure_authenticated_task_session(
                        &request(&body),
                        &context("generation-0001"),
                        "Remote",
                    )
                })
            })
            .collect();
        let results: Vec<_> = handles
            .into_iter()
            .map(|handle| handle.join().unwrap())
            .collect();
        assert_eq!(results.iter().filter(|result| result.is_ok()).count(), 4);
        assert_eq!(
            results
                .iter()
                .filter(|result| result
                    .as_ref()
                    .err()
                    .is_some_and(|error| error.code == "task_session_lane_conflict"))
                .count(),
            4
        );
        assert_eq!(sessions.created.lock().len(), 0);
        assert!(matches!(
            service.lane_for_session("session-existing", "generation-0001"),
            Some(RemoteSessionLane::Chat | RemoteSessionLane::Code)
        ));
        assert_eq!(
            service.lane_for_session("session-existing", "generation-0002"),
            None
        );
        service.clear_generation_sessions("generation-0001");
        assert_eq!(
            service.lane_for_session("session-existing", "generation-0001"),
            None
        );
    }

    #[test]
    fn lane_reverse_lookup_respects_binding_conflicts_and_cleanup() {
        let sessions = RemoteSessionMap::default();
        sessions
            .bind(
                "ref-one",
                "session-one",
                "generation-one",
                RemoteSessionLane::Code,
            )
            .unwrap();
        assert_eq!(
            sessions.lane_for_session("session-one", "generation-one"),
            Some(RemoteSessionLane::Code)
        );
        sessions
            .bind(
                "ref-one",
                "session-one",
                "generation-one",
                RemoteSessionLane::Code,
            )
            .unwrap();
        sessions
            .bind(
                "ref-one",
                "session-one",
                "generation-one",
                RemoteSessionLane::Unknown,
            )
            .unwrap();
        assert_eq!(
            sessions.lane_for_session("session-one", "generation-one"),
            Some(RemoteSessionLane::Code)
        );
        assert_eq!(
            sessions
                .bind(
                    "ref-one",
                    "session-one",
                    "generation-one",
                    RemoteSessionLane::Chat,
                )
                .unwrap_err()
                .code,
            "task_session_lane_conflict"
        );
        assert_eq!(
            sessions.lane_for_session("session-one", "generation-two"),
            None
        );
        assert_eq!(
            sessions
                .bind(
                    "ref-two",
                    "session-one",
                    "generation-one",
                    RemoteSessionLane::Chat
                )
                .unwrap_err()
                .code,
            "task_session_map_conflict"
        );
        assert_eq!(
            sessions
                .bind(
                    "ref-one",
                    "session-two",
                    "generation-two",
                    RemoteSessionLane::Chat
                )
                .unwrap_err()
                .code,
            "task_session_generation_conflict"
        );
        sessions
            .bind(
                "ref-one",
                "session-two",
                "generation-one",
                RemoteSessionLane::Unknown,
            )
            .unwrap();
        assert_eq!(
            sessions.lane_for_session("session-one", "generation-one"),
            None
        );
        assert_eq!(
            sessions.lane_for_session("session-two", "generation-one"),
            Some(RemoteSessionLane::Unknown)
        );
        sessions
            .bind(
                "ref-one",
                "session-two",
                "generation-one",
                RemoteSessionLane::Unknown,
            )
            .unwrap();
        assert_eq!(
            sessions
                .bind(
                    "ref-one",
                    "session-two",
                    "generation-one",
                    RemoteSessionLane::Code,
                )
                .unwrap_err()
                .code,
            "task_session_lane_conflict"
        );
        sessions.clear_generation("generation-one");
        assert_eq!(
            sessions.lane_for_session("session-two", "generation-one"),
            None
        );
        assert_eq!(sessions.len(), 0);
    }

    #[test]
    fn bound_admission_rejects_wrong_or_unknown_lane_without_ledger_entry() {
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        let sessions = Arc::new(FakeSessions::default());
        sessions.existing.lock().push("session-existing".into());
        service.set_session_provisioner(sessions);
        let code_id = service
            .ensure_remote_session("ref-code", "Code", true, "generation-0001")
            .unwrap();
        let wrong = payload(&code_id, "task-wrong-lane", "client-wrong-lane", &digest(1));
        assert_eq!(
            service
                .submit_bound(&context("generation-0001"), &wrong, 1_000)
                .unwrap_err()
                .code,
            "task_session_lane_mismatch"
        );
        assert_eq!(
            service.status("task-wrong-lane").unwrap_err().code,
            "task_not_found"
        );
        let code = code_payload(
            &code_id,
            "task-code-lane",
            "client-code-lane",
            "workspace-0001",
        );
        assert!(matches!(
            service.submit_bound(&context("generation-0001"), &code, 1_000),
            Ok(AdmissionOutcome::Admitted(_))
        ));
        service
            .switch_remote_session("ref-existing", "session-existing", "generation-0001")
            .unwrap();
        let unknown = payload(
            "session-existing",
            "task-unknown-lane",
            "client-unknown-lane",
            &digest(2),
        );
        assert_eq!(
            service
                .submit_bound(&context("generation-0001"), &unknown, 1_001)
                .unwrap_err()
                .code,
            "task_session_lane_mismatch"
        );
        assert_eq!(
            service.status("task-unknown-lane").unwrap_err().code,
            "task_not_found"
        );
    }

    #[test]
    fn expected_ref_admission_rejects_wrong_closed_and_rebound_refs_without_ledger_entry() {
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        let sessions = Arc::new(FakeSessions::default());
        sessions.existing.lock().push("session-0001".into());
        service.set_session_provisioner(sessions);
        service
            .ensure_remote_session("ref-original", "Chat", false, "generation-0001")
            .unwrap();

        let wrong = payload(
            "session-0001",
            "task-wrong-ref",
            "request-wrong-ref",
            &digest(1),
        );
        assert_eq!(
            service
                .submit_bound_for_ref_with_admission(
                    &context("generation-0001"),
                    "ref-other",
                    &wrong,
                    1_000,
                    |_| Ok(())
                )
                .unwrap_err()
                .code,
            "task_session_binding_mismatch"
        );
        assert_eq!(
            service.status("task-wrong-ref").unwrap_err().code,
            "task_not_found"
        );
        assert_eq!(
            service
                .submit_bound_for_ref_with_admission(
                    &context("generation-0001"),
                    "not a valid ref",
                    &wrong,
                    1_000,
                    |_| Ok(())
                )
                .unwrap_err()
                .code,
            "task_request_invalid"
        );

        service
            .close_remote_session("ref-original", "generation-0001")
            .unwrap();
        let closed = payload(
            "session-0001",
            "task-closed-ref",
            "request-closed-ref",
            &digest(2),
        );
        assert_eq!(
            service
                .submit_bound_for_ref_with_admission(
                    &context("generation-0001"),
                    "ref-original",
                    &closed,
                    1_001,
                    |_| Ok(())
                )
                .unwrap_err()
                .code,
            "task_session_binding_mismatch"
        );
        assert_eq!(
            service.status("task-closed-ref").unwrap_err().code,
            "task_not_found"
        );

        service
            .switch_remote_session("ref-new", "session-0001", "generation-0001")
            .unwrap();
        service
            .ensure_authenticated_task_session(
                &request(&payload(
                    "ref-new",
                    "task-claim-rebound",
                    "request-claim-rebound",
                    &digest(3),
                )),
                &context("generation-0001"),
                "Chat",
            )
            .unwrap();
        let rebound = payload(
            "session-0001",
            "task-rebound-ref",
            "request-rebound-ref",
            &digest(4),
        );
        assert_eq!(
            service
                .submit_bound_for_ref_with_admission(
                    &context("generation-0001"),
                    "ref-original",
                    &rebound,
                    1_002,
                    |_| Ok(())
                )
                .unwrap_err()
                .code,
            "task_session_binding_mismatch"
        );
        assert_eq!(
            service.status("task-rebound-ref").unwrap_err().code,
            "task_not_found"
        );
        assert!(matches!(
            service.submit_bound_for_ref_with_admission(
                &context("generation-0001"),
                "ref-new",
                &rebound,
                1_003,
                |_| Ok(())
            ),
            Ok(AdmissionOutcome::Admitted(_))
        ));
    }

    #[test]
    fn bound_admission_and_lane_switch_serialize_on_session_open_lock() {
        let service = Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        )));
        let sessions = Arc::new(FakeSessions::default());
        sessions.existing.lock().push("session-0002".into());
        service.set_session_provisioner(sessions);
        service
            .ensure_remote_session("ref-chat", "Chat", false, "generation-0001")
            .unwrap();
        let first = payload(
            "session-0001",
            "task-before-switch",
            "client-before-switch",
            &digest(1),
        );
        let (entered_tx, entered_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let submit_service = service.clone();
        let submit = std::thread::spawn(move || {
            submit_service.submit_bound_for_ref_with_admission(
                &context("generation-0001"),
                "ref-chat",
                &first,
                1_000,
                |_| {
                    entered_tx.send(()).unwrap();
                    release_rx.recv().unwrap();
                    Ok(())
                },
            )
        });
        entered_rx.recv().unwrap();
        let switch_service = service.clone();
        let switch = std::thread::spawn(move || {
            switch_service.switch_remote_session("ref-chat", "session-0002", "generation-0001")
        });
        release_tx.send(()).unwrap();
        assert!(matches!(
            submit.join().unwrap(),
            Ok(AdmissionOutcome::Admitted(_))
        ));
        switch.join().unwrap().unwrap();
        assert_eq!(
            service.lane_for_session("session-0001", "generation-0001"),
            None
        );
        assert_eq!(
            service.lane_for_session("session-0002", "generation-0001"),
            Some(RemoteSessionLane::Unknown)
        );
        let late = payload(
            "session-0001",
            "task-after-switch",
            "client-after-switch",
            &digest(2),
        );
        assert_eq!(
            service
                .submit_bound_for_ref_with_admission(
                    &context("generation-0001"),
                    "ref-chat",
                    &late,
                    1_100,
                    |_| Ok(())
                )
                .unwrap_err()
                .code,
            "task_session_binding_mismatch"
        );
        assert_eq!(
            service.status("task-after-switch").unwrap_err().code,
            "task_not_found"
        );
    }

    #[test]
    fn workspace_owner_resolution_precedes_session_lock_and_lane_claim_uses_only_path() {
        struct Resolver {
            service: std::sync::Weak<RemoteTaskService>,
            sessions: FakeSessions,
        }
        impl RemoteSessionProvisioner for Resolver {
            fn create_authoritative_session(
                &self,
                title: &str,
                code: bool,
            ) -> std::result::Result<String, String> {
                self.sessions.create_authoritative_session(title, code)
            }
            fn session_exists(&self, id: &str) -> std::result::Result<bool, String> {
                self.sessions.session_exists(id)
            }
            fn resolve_code_workspace(&self, id: &str) -> std::result::Result<PathBuf, String> {
                assert!(
                    self.service
                        .upgrade()
                        .unwrap()
                        .session_open_lock
                        .try_lock()
                        .is_some(),
                    "workspace authority must not run under the session lock"
                );
                Ok(PathBuf::from(id))
            }
            fn claim_execution_lane(
                &self,
                session: &str,
                code: bool,
                workspace: Option<&Path>,
            ) -> std::result::Result<(), String> {
                self.sessions.claim_execution_lane(session, code, workspace)
            }
        }
        let service = Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        )));
        let resolver = Arc::new(Resolver {
            service: Arc::downgrade(&service),
            sessions: FakeSessions::default(),
        });
        service.set_session_provisioner(resolver.clone());
        service
            .ensure_unclaimed_remote_session("ref-workspace-order", "Unknown", "generation-0001")
            .unwrap();
        let task = request(&code_payload(
            "ref-workspace-order",
            "task-workspace-order",
            "client-workspace-order",
            "workspace-0001",
        ));
        service
            .ensure_authenticated_task_session(&task, &context("generation-0001"), "Code")
            .unwrap();
        assert_eq!(
            resolver.sessions.claims.lock().as_slice(),
            &[("session-0001".into(), true, Some("workspace-0001".into()))]
        );
    }

    #[test]
    fn expected_ref_admission_and_close_serialize_without_losing_the_binding() {
        let service = Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        )));
        service.set_session_provisioner(Arc::new(FakeSessions::default()));
        service
            .ensure_remote_session("ref-chat", "Chat", false, "generation-0001")
            .unwrap();
        let first = payload(
            "session-0001",
            "task-before-close",
            "request-before-close",
            &digest(1),
        );
        let (entered_tx, entered_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let submit_service = service.clone();
        let submit = std::thread::spawn(move || {
            submit_service.submit_bound_for_ref_with_admission(
                &context("generation-0001"),
                "ref-chat",
                &first,
                1_000,
                |_| {
                    entered_tx.send(()).unwrap();
                    release_rx.recv().unwrap();
                    Ok(())
                },
            )
        });
        entered_rx.recv().unwrap();
        let close_service = service.clone();
        let close = std::thread::spawn(move || {
            close_service.close_remote_session("ref-chat", "generation-0001")
        });
        release_tx.send(()).unwrap();
        assert!(matches!(
            submit.join().unwrap(),
            Ok(AdmissionOutcome::Admitted(_))
        ));
        assert_eq!(close.join().unwrap().unwrap_err().code, "task_session_busy");
        assert_eq!(
            service
                .session_for("ref-chat", "generation-0001")
                .as_deref(),
            Some("session-0001")
        );

        service
            .complete(
                "task-before-close",
                RemoteTaskTerminal::Succeeded,
                None,
                1_100,
            )
            .unwrap();
        service
            .close_remote_session("ref-chat", "generation-0001")
            .unwrap();
        let after = payload(
            "session-0001",
            "task-after-close",
            "request-after-close",
            &digest(2),
        );
        assert_eq!(
            service
                .submit_bound_for_ref_with_admission(
                    &context("generation-0001"),
                    "ref-chat",
                    &after,
                    1_200,
                    |_| Ok(())
                )
                .unwrap_err()
                .code,
            "task_session_binding_mismatch"
        );
        assert_eq!(
            service.status("task-after-close").unwrap_err().code,
            "task_not_found"
        );
    }

    #[test]
    fn lifecycle_cleanup_invalidates_old_session_before_next_generation_can_submit() {
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        let sessions = Arc::new(FakeSessions::default());
        service.set_session_provisioner(sessions.clone());
        assert_eq!(
            service
                .ensure_remote_session("remote-session-shared", "Remote", false, "generation-0001",)
                .unwrap(),
            "session-0001"
        );

        service.interrupt_generation_and_clear_sessions(
            "generation-0001",
            "remote_control_stopped",
            1_100,
        );

        assert_eq!(
            service
                .ensure_remote_session("remote-session-shared", "Remote", false, "generation-0001",)
                .unwrap(),
            "session-0002",
            "a late old-generation prepare may create a session but must stay generation-bound"
        );

        let mut stale = payload(
            "session-0002",
            "task-generation-0002",
            "client-generation-0002",
            &digest(2),
        );
        stale["remote_control_generation"] = json!("generation-0002");
        assert_eq!(
            service
                .submit_bound(&context("generation-0002"), &stale, 1_200)
                .unwrap_err()
                .code,
            "task_session_not_found"
        );
        service.clear_generation_sessions("generation-0001");
        assert_eq!(
            service
                .ensure_remote_session("remote-session-shared", "Remote", false, "generation-0002",)
                .unwrap(),
            "session-0003"
        );
        assert_eq!(sessions.created.lock().len(), 3);
    }

    #[test]
    fn bound_admission_installs_resources_before_racing_lifecycle_cleanup() {
        #[derive(Default)]
        struct InstallationOrderScope {
            installed: AtomicBool,
            cleanup_saw_installed: AtomicBool,
        }

        impl RemoteTaskModelScope for InstallationOrderScope {
            fn release_task_credentials(&self, _task_id: &str) {
                self.cleanup_saw_installed
                    .store(self.installed.load(Ordering::SeqCst), Ordering::SeqCst);
            }
        }

        let service = Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        )));
        service.set_session_provisioner(Arc::new(FakeSessions::default()));
        assert_eq!(
            service
                .ensure_remote_session("remote-session-race", "Remote", false, "generation-0001",)
                .unwrap(),
            "session-0001"
        );
        let scope = Arc::new(InstallationOrderScope::default());
        service.hooks().set_model_scope(scope.clone());
        let body = payload(
            "session-0001",
            "task-install-race",
            "client-install-race",
            &digest(9),
        );
        let (entered_tx, entered_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let submit_service = service.clone();
        let install_scope = scope.clone();
        let submit = std::thread::spawn(move || {
            submit_service.submit_bound_with_admission(
                &context("generation-0001"),
                &body,
                1_000,
                |_| {
                    entered_tx.send(()).unwrap();
                    release_rx.recv().unwrap();
                    install_scope.installed.store(true, Ordering::SeqCst);
                    Ok(())
                },
            )
        });
        entered_rx.recv().unwrap();

        let (cleanup_started_tx, cleanup_started_rx) = std::sync::mpsc::channel();
        let cleanup_service = service.clone();
        let cleanup = std::thread::spawn(move || {
            cleanup_started_tx.send(()).unwrap();
            cleanup_service.interrupt_generation_and_clear_sessions(
                "generation-0001",
                "remote_control_stopped",
                1_001,
            )
        });
        cleanup_started_rx.recv().unwrap();
        std::thread::yield_now();
        release_tx.send(()).unwrap();

        assert!(matches!(
            submit.join().unwrap(),
            Ok(AdmissionOutcome::Admitted(_))
        ));
        assert_eq!(cleanup.join().unwrap().len(), 1);
        assert!(scope.cleanup_saw_installed.load(Ordering::SeqCst));
        assert_eq!(
            service.status("task-install-race").unwrap().state,
            RemoteTaskState::Interrupted
        );
    }

    #[test]
    fn old_generation_cleanup_preserves_a_new_generation_mapping() {
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.set_session_provisioner(Arc::new(FakeSessions::default()));
        assert_eq!(
            service
                .ensure_remote_session(
                    "remote-session-generation-1",
                    "Remote 1",
                    false,
                    "generation-0001",
                )
                .unwrap(),
            "session-0001"
        );
        assert_eq!(
            service
                .ensure_remote_session(
                    "remote-session-generation-2",
                    "Remote 2",
                    false,
                    "generation-0002",
                )
                .unwrap(),
            "session-0002"
        );

        service.interrupt_generation_and_clear_sessions(
            "generation-0001",
            "remote_control_stopped",
            1_100,
        );

        assert_eq!(
            service.session_for("remote-session-generation-1", "generation-0001"),
            None
        );
        assert_eq!(
            service
                .session_for("remote-session-generation-2", "generation-0002")
                .as_deref(),
            Some("session-0002")
        );
    }

    #[test]
    fn session_map_is_opaque_and_blocks_closing_a_busy_session() {
        let (service, _hooks) = service(RecordingExecutor::ok());
        service.set_session_provisioner(Arc::new(FakeSessions::default()));
        assert_eq!(
            service
                .open_remote_session("remote-session-0001", "Remote", false, "generation-0001",)
                .unwrap(),
            "session-0001"
        );
        assert!(service.is_remote_session("session-0001"));
        assert!(!service.is_remote_session("session-0002"));
        let _ticket = admitted(
            &service,
            &payload("session-0001", "task-0001", "client-0001", &digest(1)),
            1_000,
        );
        assert_eq!(
            service
                .close_remote_session("remote-session-0001", "generation-0001")
                .unwrap_err()
                .code,
            "task_session_busy"
        );
        service
            .complete("task-0001", RemoteTaskTerminal::Succeeded, None, 1_100)
            .unwrap();
        assert_eq!(
            service
                .close_remote_session("remote-session-0001", "generation-0001")
                .unwrap(),
            Some("session-0001".to_owned())
        );
        assert!(!service.is_remote_session("session-0001"));
    }

    #[test]
    fn status_payload_matches_the_frozen_protocol_shape() {
        let (service, _hooks) = service(RecordingExecutor::ok());
        let _ticket = admitted(
            &service,
            &payload("session-0001", "task-0001", "client-0001", &digest(1)),
            1_000,
        );
        let payload = service.status_payload("task-0001").unwrap();
        let encoded = serde_json::to_string(&payload).unwrap();
        let validated = crate::features::remote_control::controller::protocol::
            validate_controller_payload_json(&encoded, SenderRole::ControlledB)
            .unwrap();
        assert_eq!(validated["type"], json!("task_status"));
        assert_eq!(validated["task_status"], json!("admitted"));
        assert_eq!(
            service.status("task-9999").unwrap_err().code,
            "task_not_found"
        );
    }

    #[test]
    fn persisted_records_never_contain_the_prompt_or_a_host_path() {
        let (service, _hooks) = service(RecordingExecutor::ok());
        let mut body = code_payload("session-0001", "task-0001", "client-0001", "workspace-0001");
        body["input"]["text"] = json!(r"C:\Users\someone\secret\plan.md");
        body["input_digest"] = json!(task_input_digest_for_value(&body["input"]).unwrap());
        let _ticket = admitted(&service, &body, 1_000);
        let record = service.status("task-0001").unwrap();
        let encoded = serde_json::to_string(&record).unwrap();
        assert!(!encoded.contains("secret"));
        assert!(!encoded.contains("plan.md"));
        assert!(!encoded.contains(r"C:\"));
        assert!(!format!("{record:?}").contains("secret"));
        let ticket_debug = format!(
            "{:?}",
            admitted(
                &service,
                &payload("session-0002", "task-0002", "client-0002", &digest(2)),
                2_000
            )
        );
        assert!(!ticket_debug.contains("run the report"));
    }

    #[test]
    fn ledger_capacity_prunes_only_terminal_records() {
        let ledger = RemoteTaskLedger::in_memory();
        let live = context("generation-0001");
        for index in 0..MAX_TRACKED_TASKS {
            let task_id = format!("task-{index:04}");
            let client = format!("client-{index:04}");
            let session = format!("session-{index:04}");
            ledger
                .admit(
                    &live,
                    &request(&payload(&session, &task_id, &client, &digest(1))),
                    1_000,
                )
                .unwrap();
            ledger
                .complete(&task_id, RemoteTaskTerminal::Succeeded, None, 1_100)
                .unwrap();
        }
        assert!(
            ledger
                .admit(
                    &live,
                    &request(&payload(
                        "session-9999",
                        "task-9999",
                        "client-9999",
                        &digest(2),
                    )),
                    1_200,
                )
                .is_ok()
        );
        assert!(ledger.records().len() <= MAX_TRACKED_TASKS);
    }
}
