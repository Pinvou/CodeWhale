//! B authoritative event journal with bounded replay (DEV-19).
//!
//! Every synchronized event carries an epoch plus a monotonic sequence. A
//! reconnect asks for everything after the last sequence it accepted: when the
//! journal can still cover the gap the exact suffix is replayed, otherwise the
//! caller receives a gap signal and must replace its state with a snapshot.
//!
//! The journal is deliberately lossy for presentation only: text, thinking and
//! tool status are bounded and sanitized, task terminal states are recorded
//! exactly once, and no path, credential or token can be appended.

use std::collections::{HashMap, HashSet, VecDeque};
use std::fmt;

use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use sha2::{Digest, Sha256};

use super::task::RemoteTaskEvent;
#[cfg(test)]
use super::task::RemoteTaskEventSink;

/// Must not exceed SessionStore.RemoteProxy's authenticated projection window.
pub(crate) const JOURNAL_CAPACITY: usize = 512;
// Count complete serialized events plus the maximum B-session -> A-ref
// expansion. A 256 KiB window remains below the 512 KiB plaintext ceiling
// after snapshot JSON and base64 framing.
pub(crate) const JOURNAL_BYTES_CAPACITY: usize = 256 * 1024;
pub(crate) const MAX_EVENT_PAYLOAD_BYTES: usize = 16 * 1024;
pub(crate) const MAX_EVENT_TEXT_CHARS: usize = 4096;
pub(crate) const MAX_TASK_ID_CHARS: usize = 128;
pub(crate) const MAX_SESSION_ID_CHARS: usize = 128;
const MAX_JOURNAL_SESSIONS: usize = 256;
const MAX_JOURNAL_EVENT_IDS_PER_SESSION: usize = 8192;
const MAX_RETIRED_GENERATIONS: usize = 64;

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct JournalError {
    pub(crate) code: &'static str,
}

impl JournalError {
    fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl fmt::Display for JournalError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for JournalError {}

type Result<T> = std::result::Result<T, JournalError>;

fn error(code: &'static str) -> JournalError {
    JournalError::new(code)
}

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum JournalKind {
    UserMessage,
    AssistantDelta,
    Reasoning,
    Tool,
    Permission,
    Error,
    Cancel,
    TaskStatus,
    Attachment,
    Artifact,
    Snapshot,
}

impl JournalKind {
    pub(crate) fn wire(self) -> &'static str {
        match self {
            Self::UserMessage => "user_message",
            Self::AssistantDelta => "assistant_delta",
            Self::Reasoning => "reasoning",
            Self::Tool => "tool",
            Self::Permission => "permission",
            Self::Error => "error",
            Self::Cancel => "cancel",
            Self::TaskStatus => "task_status",
            Self::Attachment => "attachment",
            Self::Artifact => "artifact",
            Self::Snapshot => "snapshot",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct JournalEvent {
    pub(crate) event_id: String,
    pub(crate) epoch: String,
    pub(crate) sequence: u64,
    pub(crate) kind: String,
    pub(crate) session_id: String,
    pub(crate) task_id: Option<String>,
    pub(crate) terminal_status: Option<String>,
    pub(crate) payload: Value,
}

impl JournalEvent {
    pub(crate) fn wire_kind(&self) -> Option<&'static str> {
        match self.kind.as_str() {
            "user_message" => Some("user_message"),
            "assistant_delta" => Some("assistant_delta"),
            "reasoning" => Some("reasoning"),
            "tool" => Some("tool"),
            "permission" => Some("permission"),
            "error" => Some("error"),
            "cancel" => Some("cancel"),
            "artifact" => Some("artifact"),
            "task_status" if self.terminal_status.is_some() => Some("terminal"),
            _ => None,
        }
    }

    pub(crate) fn validate_exportable(&self) -> Result<()> {
        validate_id(&self.event_id, MAX_TASK_ID_CHARS)?;
        validate_id(&self.epoch, MAX_SESSION_ID_CHARS)?;
        validate_id(&self.session_id, MAX_SESSION_ID_CHARS)?;
        if self.sequence == 0 || self.wire_kind().is_none() {
            return Err(error("journal_kind_not_exportable"));
        }
        if let Some(task_id) = &self.task_id {
            validate_id(task_id, MAX_TASK_ID_CHARS)?;
        }
        if self.kind == "task_status" {
            if self.task_id.is_none()
                || !matches!(
                    self.terminal_status.as_deref(),
                    Some("succeeded" | "failed" | "cancelled" | "interrupted" | "result_unknown")
                )
            {
                return Err(error("journal_status_invalid"));
            }
        } else if self.terminal_status.is_some() {
            return Err(error("journal_status_invalid"));
        }
        if self.payload != sanitize_event_payload(&self.payload)
            || serde_json::to_vec(self)
                .map_err(|_| error("journal_payload_invalid"))?
                .len()
                > MAX_EVENT_PAYLOAD_BYTES
        {
            return Err(error("journal_payload_invalid"));
        }
        Ok(())
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct JournalSnapshot {
    pub(crate) epoch: String,
    pub(crate) session_id: String,
    pub(crate) sequence: u64,
    pub(crate) digest: String,
    pub(crate) events: Vec<JournalEvent>,
}

impl JournalSnapshot {
    /// Domain-separated canonical digest of the entire visible window and
    /// its base. Replacing any event field, payload key/value or session
    /// identity necessarily changes this digest.
    pub(crate) fn compute_digest(&self) -> String {
        let value = json!({
            "epoch": self.epoch,
            "session_id": self.session_id,
            "base_sequence": self.sequence,
            "events": self.events,
        });
        let mut hasher = Sha256::new();
        hasher.update(b"pinvou-journal-snapshot-v1\0");
        hasher.update(
            serde_json::to_vec(&canonical_json(&value)).expect("serializable journal snapshot"),
        );
        crate::platform::encoding::hex_lower(&hasher.finalize())
    }

    pub(crate) fn verify_digest(&self) -> bool {
        self.digest == self.compute_digest()
            && (self.events.is_empty() == (self.sequence == 0))
            && self
                .events
                .last()
                .is_none_or(|event| event.sequence == self.sequence)
            && self.events.first().is_none_or(|event| event.sequence > 0)
            && self
                .events
                .windows(2)
                .all(|pair| pair[0].sequence.checked_add(1) == Some(pair[1].sequence))
            && self.events.iter().all(|event| {
                event.epoch == self.epoch
                    && event.session_id == self.session_id
                    && event.sequence <= self.sequence
                    && event.validate_exportable().is_ok()
            })
    }

    pub(crate) fn remap_session(&self, session_ref: &str) -> Result<Self> {
        validate_id(session_ref, MAX_SESSION_ID_CHARS)?;
        if !self.verify_digest() {
            return Err(error("journal_snapshot_digest_invalid"));
        }
        let mut mapped = self.clone();
        mapped.session_id = session_ref.to_owned();
        for event in &mut mapped.events {
            event.session_id = session_ref.to_owned();
        }
        mapped.digest = mapped.compute_digest();
        Ok(mapped)
    }
}

fn canonical_json(value: &Value) -> Value {
    match value {
        Value::Object(object) => {
            let mut keys: Vec<&String> = object.keys().collect();
            keys.sort_unstable();
            let mut canonical = Map::new();
            for key in keys {
                canonical.insert(key.clone(), canonical_json(&object[key]));
            }
            Value::Object(canonical)
        }
        Value::Array(items) => Value::Array(items.iter().map(canonical_json).collect()),
        other => other.clone(),
    }
}

#[derive(Debug, Clone, PartialEq)]
pub(crate) enum ReplayOutcome {
    /// The journal can cover the gap; apply these events in order.
    Events(Vec<JournalEvent>),
    /// The gap is too old; the caller must replace its state with a snapshot.
    Gap(JournalSnapshot),
}

struct JournalInner {
    sequence: u64,
    events: VecDeque<JournalEvent>,
    bytes: usize,
    // Never evict accepted IDs: a trimmed event must not be admitted again
    // under a new sequence. Capacity exhaustion rejects new events closed.
    event_ids: HashMap<String, [u8; 32]>,
    terminals: HashMap<String, String>,
    acknowledged: u64,
}

struct JournalState {
    epoch_seed: String,
    epoch: String,
    generation: Option<String>,
    retired_generations: HashSet<String>,
    sessions: HashMap<String, JournalInner>,
}

pub(crate) struct RemoteJournal {
    inner: Mutex<JournalState>,
}

impl RemoteJournal {
    pub(crate) fn new(epoch: impl Into<String>) -> Self {
        let epoch = epoch.into();
        Self {
            inner: Mutex::new(JournalState {
                epoch_seed: epoch.clone(),
                epoch,
                generation: None,
                retired_generations: HashSet::new(),
                sessions: HashMap::new(),
            }),
        }
    }

    fn new_stream() -> JournalInner {
        JournalInner {
            sequence: 0,
            events: VecDeque::new(),
            bytes: 0,
            event_ids: HashMap::new(),
            terminals: HashMap::new(),
            acknowledged: 0,
        }
    }

    /// Starts a new epoch (reconnect or new lease). Everything older is
    /// unreachable, so a replay request against the old epoch yields a gap.
    pub(crate) fn reset(&self, epoch: impl Into<String>) {
        let mut inner = self.inner.lock();
        inner.epoch = epoch.into();
        inner.epoch_seed = inner.epoch.clone();
        inner.generation = None;
        inner.retired_generations.clear();
        inner.sessions.clear();
    }

    /// The live B owner calls this before recording or reading a generation.
    /// A new lease generation retires the entire prior per-session epoch; it
    /// can never replay events admitted under the previous controller lease.
    pub(crate) fn bind_generation(&self, generation: &str) -> Result<String> {
        validate_id(generation, MAX_SESSION_ID_CHARS)?;
        let mut state = self.inner.lock();
        if state.generation.as_deref() != Some(generation) {
            if state.retired_generations.contains(generation)
                || (state.generation.is_some()
                    && state.retired_generations.len() >= MAX_RETIRED_GENERATIONS)
            {
                return Err(error("journal_generation_retired"));
            }
            if let Some(previous) = state.generation.take() {
                state.retired_generations.insert(previous);
            }
            let mut hasher = Sha256::new();
            hasher.update(b"pinvou-journal-generation-v1\0");
            hasher.update(state.epoch_seed.as_bytes());
            hasher.update([0]);
            hasher.update(generation.as_bytes());
            state.epoch = format!(
                "journal-{}",
                crate::platform::encoding::hex_lower(&hasher.finalize())
            );
            state.sessions.clear();
            state.generation = Some(generation.to_owned());
        }
        Ok(state.epoch.clone())
    }

    fn require_generation_locked(state: &JournalState, generation: &str) -> Result<()> {
        validate_id(generation, MAX_SESSION_ID_CHARS)?;
        if state.generation.as_deref() != Some(generation) {
            return Err(error("journal_generation_stale"));
        }
        Ok(())
    }

    /// Publication after append must not reinterpret an old event under a new
    /// live lease/ref. This exact-stream proof is checked under the same
    /// journal mutex as generation and epoch.
    pub(crate) fn require_current_event_for_generation(
        &self,
        generation: &str,
        event: &JournalEvent,
    ) -> Result<()> {
        let state = self.inner.lock();
        Self::require_generation_locked(&state, generation)?;
        if state.epoch != event.epoch
            || !state
                .sessions
                .get(&event.session_id)
                .is_some_and(|stream| stream.events.iter().any(|stored| stored == event))
        {
            return Err(error("journal_event_not_current"));
        }
        Ok(())
    }

    pub(crate) fn epoch(&self) -> String {
        self.inner.lock().epoch.clone()
    }

    pub(crate) fn last_sequence_for(&self, session_id: &str) -> u64 {
        self.inner
            .lock()
            .sessions
            .get(session_id)
            .map_or(0, |stream| stream.sequence)
    }

    pub(crate) fn acknowledged_for(&self, session_id: &str) -> u64 {
        self.inner
            .lock()
            .sessions
            .get(session_id)
            .map_or(0, |stream| stream.acknowledged)
    }

    pub(crate) fn len_for(&self, session_id: &str) -> usize {
        self.inner
            .lock()
            .sessions
            .get(session_id)
            .map_or(0, |stream| stream.events.len())
    }

    #[cfg(test)]
    pub(crate) fn append(
        &self,
        event_id: &str,
        kind: JournalKind,
        session_id: &str,
        task_id: Option<&str>,
        payload: Value,
    ) -> Result<JournalEvent> {
        self.append_inner(None, event_id, kind, session_id, task_id, payload, None)?
            .ok_or_else(|| error("journal_kind_not_exportable"))
    }

    pub(crate) fn append_for_generation(
        &self,
        generation: &str,
        event_id: &str,
        kind: JournalKind,
        session_id: &str,
        task_id: Option<&str>,
        payload: Value,
    ) -> Result<JournalEvent> {
        self.append_inner(
            Some(generation),
            event_id,
            kind,
            session_id,
            task_id,
            payload,
            None,
        )?
        .ok_or_else(|| error("journal_kind_not_exportable"))
    }

    /// Terminal CAS: only the first terminal state of a task is journaled, and
    /// repeated reports are dropped instead of producing duplicate terminals.
    #[cfg(test)]
    pub(crate) fn append_terminal(
        &self,
        event_id: &str,
        session_id: &str,
        task_id: &str,
        status: &str,
        payload: Value,
    ) -> Result<Option<JournalEvent>> {
        if !matches!(
            status,
            "succeeded" | "failed" | "cancelled" | "interrupted" | "result_unknown"
        ) {
            return Err(error("journal_status_invalid"));
        }
        self.append_inner(
            None,
            event_id,
            JournalKind::TaskStatus,
            session_id,
            Some(task_id),
            payload,
            Some(status.to_owned()),
        )
    }

    pub(crate) fn append_terminal_for_generation(
        &self,
        generation: &str,
        event_id: &str,
        session_id: &str,
        task_id: &str,
        status: &str,
        payload: Value,
    ) -> Result<Option<JournalEvent>> {
        if !matches!(
            status,
            "succeeded" | "failed" | "cancelled" | "interrupted" | "result_unknown"
        ) {
            return Err(error("journal_status_invalid"));
        }
        self.append_inner(
            Some(generation),
            event_id,
            JournalKind::TaskStatus,
            session_id,
            Some(task_id),
            payload,
            Some(status.to_owned()),
        )
    }

    fn append_inner(
        &self,
        generation: Option<&str>,
        event_id: &str,
        kind: JournalKind,
        session_id: &str,
        task_id: Option<&str>,
        payload: Value,
        terminal_status: Option<String>,
    ) -> Result<Option<JournalEvent>> {
        // Only entries representable by the frozen wire event-kind closed set
        // may allocate a visible sequence. In-progress task state has its own
        // task_status frame and must never create an unfillable journal gap.
        if matches!(kind, JournalKind::Attachment | JournalKind::Snapshot)
            || (kind == JournalKind::TaskStatus && terminal_status.is_none())
        {
            return Err(error("journal_kind_not_exportable"));
        }
        validate_id(event_id, MAX_TASK_ID_CHARS)?;
        validate_id(session_id, MAX_SESSION_ID_CHARS)?;
        if let Some(task_id) = task_id {
            validate_id(task_id, MAX_TASK_ID_CHARS)?;
        }
        let sanitized = sanitize_event_payload(&payload);
        let encoded =
            serde_json::to_vec(&sanitized).map_err(|_| error("journal_payload_invalid"))?;
        if encoded.len() > MAX_EVENT_PAYLOAD_BYTES {
            return Err(error("journal_payload_too_large"));
        }
        let identity = canonical_json(&json!({
            "kind": kind.wire(), "session_id": session_id, "task_id": task_id,
            "terminal_status": &terminal_status, "payload": &sanitized,
        }));
        let fingerprint: [u8; 32] = Sha256::digest(
            serde_json::to_vec(&identity).map_err(|_| error("journal_payload_invalid"))?,
        )
        .into();
        let mut state = self.inner.lock();
        if let Some(generation) = generation {
            Self::require_generation_locked(&state, generation)?;
        }
        let epoch = state.epoch.clone();
        if !state.sessions.contains_key(session_id) && state.sessions.len() >= MAX_JOURNAL_SESSIONS
        {
            return Err(error("journal_session_capacity"));
        }
        let inner = state
            .sessions
            .entry(session_id.to_owned())
            .or_insert_with(Self::new_stream);
        if let Some(accepted) = inner.event_ids.get(event_id) {
            if accepted != &fingerprint {
                return Err(error("journal_event_conflict"));
            }
            // Duplicate delivery: return the stored projection when available.
            if let Some(existing) = inner.events.iter().find(|event| event.event_id == event_id) {
                return Ok(Some(existing.clone()));
            }
            return Err(error("journal_duplicate_expired"));
        }
        if inner.event_ids.len() >= MAX_JOURNAL_EVENT_IDS_PER_SESSION {
            return Err(error("journal_event_id_capacity"));
        }
        let next_sequence = inner
            .sequence
            .checked_add(1)
            .ok_or_else(|| error("journal_sequence_exhausted"))?;
        if let (Some(_status), Some(task_id)) = (&terminal_status, task_id) {
            if inner.terminals.contains_key(task_id) {
                // Exactly one terminal state per task. A conflicting terminal
                // report is dropped rather than rewritten.
                return Ok(None);
            }
        }
        let event = JournalEvent {
            event_id: event_id.to_owned(),
            epoch,
            sequence: next_sequence,
            kind: kind.wire().to_owned(),
            session_id: session_id.to_owned(),
            task_id: task_id.map(str::to_owned),
            terminal_status,
            payload: sanitized,
        };
        event.validate_exportable()?;
        // A's opaque remote ref may be up to 128 bytes longer than B's local
        // ID. Reserve that headroom before consuming a visible sequence.
        let event_bytes =
            serde_json::to_vec(&event).map_err(|_| error("journal_payload_invalid"))?;
        if event_bytes.len() > MAX_EVENT_PAYLOAD_BYTES - MAX_SESSION_ID_CHARS {
            return Err(error("journal_payload_too_large"));
        }
        inner.sequence = next_sequence;
        if let (Some(status), Some(task_id)) = (&event.terminal_status, task_id) {
            inner.terminals.insert(task_id.to_owned(), status.clone());
        }
        inner.bytes += event_bytes.len() + MAX_SESSION_ID_CHARS;
        inner.event_ids.insert(event_id.to_owned(), fingerprint);
        inner.events.push_back(event.clone());
        Self::trim_locked(inner);
        Ok(Some(event))
    }

    /// Replays everything strictly after `sequence`. A request for an epoch
    /// that is not current, or for a sequence older than the retained window,
    /// yields a gap plus a snapshot.
    #[cfg(test)]
    pub(crate) fn replay_after_for(
        &self,
        session_id: &str,
        epoch: &str,
        sequence: u64,
    ) -> Result<ReplayOutcome> {
        self.replay_after_for_inner(None, session_id, epoch, sequence)
    }

    pub(crate) fn replay_after_for_generation(
        &self,
        generation: &str,
        session_id: &str,
        epoch: &str,
        sequence: u64,
    ) -> Result<ReplayOutcome> {
        self.replay_after_for_inner(Some(generation), session_id, epoch, sequence)
    }

    fn replay_after_for_inner(
        &self,
        generation: Option<&str>,
        session_id: &str,
        epoch: &str,
        sequence: u64,
    ) -> Result<ReplayOutcome> {
        validate_id(session_id, MAX_SESSION_ID_CHARS)?;
        let state = self.inner.lock();
        if let Some(generation) = generation {
            Self::require_generation_locked(&state, generation)?;
        }
        let stream = state.sessions.get(session_id);
        if state.epoch != epoch {
            return Ok(ReplayOutcome::Gap(Self::snapshot_locked(
                &state, session_id,
            )));
        }
        let Some(stream) = stream else {
            return Ok(ReplayOutcome::Events(Vec::new()));
        };
        let oldest = stream
            .events
            .front()
            .map(|event| event.sequence)
            .unwrap_or(stream.sequence.saturating_add(1));
        if sequence.saturating_add(1) < oldest && sequence < stream.sequence {
            return Ok(ReplayOutcome::Gap(Self::snapshot_locked(
                &state, session_id,
            )));
        }
        Ok(ReplayOutcome::Events(
            stream
                .events
                .iter()
                .filter(|event| event.sequence > sequence)
                .cloned()
                .collect(),
        ))
    }

    /// Snapshot with a base epoch/sequence and a content digest, so A can atomically
    /// replace its copy and then accept only newer events.
    #[cfg(test)]
    pub(crate) fn snapshot_for(&self, session_id: &str) -> Result<JournalSnapshot> {
        self.snapshot_for_inner(None, session_id)
    }

    pub(crate) fn snapshot_for_generation(
        &self,
        generation: &str,
        session_id: &str,
    ) -> Result<JournalSnapshot> {
        self.snapshot_for_inner(Some(generation), session_id)
    }

    fn snapshot_for_inner(
        &self,
        generation: Option<&str>,
        session_id: &str,
    ) -> Result<JournalSnapshot> {
        validate_id(session_id, MAX_SESSION_ID_CHARS)?;
        let state = self.inner.lock();
        if let Some(generation) = generation {
            Self::require_generation_locked(&state, generation)?;
        }
        Ok(Self::snapshot_locked(&state, session_id))
    }

    #[cfg(test)]
    pub(crate) fn acknowledge_for(
        &self,
        session_id: &str,
        epoch: &str,
        sequence: u64,
    ) -> Result<u64> {
        self.acknowledge_for_inner(None, session_id, epoch, sequence)
    }

    pub(crate) fn acknowledge_for_generation(
        &self,
        generation: &str,
        session_id: &str,
        epoch: &str,
        sequence: u64,
    ) -> Result<u64> {
        self.acknowledge_for_inner(Some(generation), session_id, epoch, sequence)
    }

    fn acknowledge_for_inner(
        &self,
        generation: Option<&str>,
        session_id: &str,
        epoch: &str,
        sequence: u64,
    ) -> Result<u64> {
        validate_id(session_id, MAX_SESSION_ID_CHARS)?;
        let mut state = self.inner.lock();
        if let Some(generation) = generation {
            Self::require_generation_locked(&state, generation)?;
        }
        if state.epoch != epoch {
            return Err(error("journal_epoch_stale"));
        }
        let Some(stream) = state.sessions.get_mut(session_id) else {
            return Ok(0);
        };
        stream.acknowledged = stream.acknowledged.max(sequence.min(stream.sequence));
        Ok(stream.acknowledged)
    }

    fn snapshot_locked(state: &JournalState, session_id: &str) -> JournalSnapshot {
        let stream = state.sessions.get(session_id);
        let mut snapshot = JournalSnapshot {
            epoch: state.epoch.clone(),
            session_id: session_id.to_owned(),
            sequence: stream.map_or(0, |stream| stream.sequence),
            digest: String::new(),
            events: stream.map_or_else(Vec::new, |stream| stream.events.iter().cloned().collect()),
        };
        snapshot.digest = snapshot.compute_digest();
        snapshot
    }

    fn trim_locked(inner: &mut JournalInner) {
        while inner.events.len() > JOURNAL_CAPACITY || inner.bytes > JOURNAL_BYTES_CAPACITY {
            let Some(dropped) = inner.events.pop_front() else {
                break;
            };
            inner.bytes = inner.bytes.saturating_sub(
                serde_json::to_vec(&dropped)
                    .map(|bytes| bytes.len() + MAX_SESSION_ID_CHARS)
                    .unwrap_or(0),
            );
            // Keep the event-id fingerprint tombstone across window trim.
        }
    }
}

#[cfg(test)]
mod dev19_contract_tests {
    use super::*;

    #[test]
    fn b_sessions_have_isolated_sequences_windows_and_acknowledgements() {
        let journal = RemoteJournal::new("epoch-1");
        for session in ["session-a", "session-b"] {
            let event = journal
                .append(
                    &format!("event-{session}"),
                    JournalKind::UserMessage,
                    session,
                    None,
                    json!({"text": session}),
                )
                .unwrap();
            assert_eq!(event.sequence, 1);
        }
        journal
            .append(
                "event-a-2",
                JournalKind::AssistantDelta,
                "session-a",
                None,
                json!({"text":"second"}),
            )
            .unwrap();
        let a = journal.snapshot_for("session-a").unwrap();
        let b = journal.snapshot_for("session-b").unwrap();
        assert_eq!(
            (a.sequence, a.events.len(), a.session_id.as_str()),
            (2, 2, "session-a")
        );
        assert_eq!(
            (b.sequence, b.events.len(), b.session_id.as_str()),
            (1, 1, "session-b")
        );
        assert!(a.events.iter().all(|event| event.session_id == "session-a"));
        assert_eq!(
            journal.acknowledge_for("session-a", "epoch-1", 2).unwrap(),
            2
        );
        assert_eq!(journal.acknowledged_for("session-b"), 0);
        assert!(
            matches!(journal.replay_after_for("session-b", "epoch-1", 0).unwrap(),
            ReplayOutcome::Events(events) if events.len() == 1 && events[0].session_id == "session-b")
        );
    }

    #[test]
    fn every_exportable_wire_kind_has_contiguous_journal_sequence() {
        let journal = RemoteJournal::new("epoch-1");
        for (index, kind) in [
            JournalKind::UserMessage,
            JournalKind::AssistantDelta,
            JournalKind::Reasoning,
            JournalKind::Tool,
            JournalKind::Permission,
            JournalKind::Error,
            JournalKind::Cancel,
            JournalKind::Artifact,
        ]
        .into_iter()
        .enumerate()
        {
            let event = journal
                .append(
                    &format!("event-{index}"),
                    kind,
                    "session-a",
                    None,
                    json!({"text":"safe", "message":"safe"}),
                )
                .unwrap();
            assert_eq!(event.sequence, (index + 1) as u64);
            assert!(event.wire_kind().is_some());
        }
        let terminal = journal
            .append_terminal(
                "terminal-1",
                "session-a",
                "task-1",
                "cancelled",
                json!({"status":"cancelled"}),
            )
            .unwrap()
            .unwrap();
        assert_eq!(terminal.sequence, 9);
        assert_eq!(terminal.wire_kind(), Some("terminal"));
        assert!(journal.snapshot_for("session-a").unwrap().verify_digest());
    }

    #[test]
    fn snapshot_digest_covers_all_fields_and_remapped_identity() {
        let journal = RemoteJournal::new("epoch-1");
        journal
            .append_terminal(
                "event-1",
                "session-b",
                "task-1",
                "succeeded",
                json!({"status":"succeeded", "message":"safe"}),
            )
            .unwrap();
        let original = journal.snapshot_for("session-b").unwrap();
        assert!(original.verify_digest());
        let mapped = original.remap_session("remote-a").unwrap();
        assert_ne!(original.digest, mapped.digest);
        assert!(mapped.verify_digest());
        for change in 0..8 {
            let mut tampered = mapped.clone();
            match change {
                0 => tampered.epoch = "other-epoch".into(),
                1 => tampered.session_id = "other-session".into(),
                2 => tampered.sequence += 1,
                3 => tampered.events[0].event_id = "other-event".into(),
                4 => tampered.events[0].session_id = "other-session".into(),
                5 => tampered.events[0].task_id = Some("other-task".into()),
                6 => tampered.events[0].terminal_status = Some("failed".into()),
                _ => tampered.events[0].payload["message"] = json!("changed"),
            }
            assert!(!tampered.verify_digest(), "tampering variant {change}");
        }
    }

    #[test]
    fn invisible_kinds_never_consume_sequence_and_trimmed_ids_cannot_reappear() {
        let journal = RemoteJournal::new("epoch-1");
        for kind in [
            JournalKind::Attachment,
            JournalKind::Snapshot,
            JournalKind::TaskStatus,
        ] {
            assert_eq!(
                journal
                    .append("hidden", kind, "session-a", None, json!({}))
                    .unwrap_err()
                    .code,
                "journal_kind_not_exportable"
            );
        }
        assert_eq!(journal.last_sequence_for("session-a"), 0);
        for index in 0..=JOURNAL_CAPACITY {
            journal
                .append(
                    &format!("event-{index}"),
                    JournalKind::UserMessage,
                    "session-a",
                    None,
                    json!({"text":"safe"}),
                )
                .unwrap();
        }
        let bounded = journal.snapshot_for("session-a").unwrap();
        assert_eq!(JOURNAL_CAPACITY, 512);
        assert_eq!(bounded.events.len(), 512);
        assert_eq!(bounded.events.first().unwrap().sequence, 2);
        assert_eq!(bounded.events.last().unwrap().sequence, 513);
        assert_eq!(bounded.sequence, 513);
        assert!(bounded.verify_digest());
        assert!(
            matches!(journal.replay_after_for("session-a", "epoch-1", 0).unwrap(),
            ReplayOutcome::Gap(snapshot) if snapshot.verify_digest())
        );
        assert_eq!(
            journal
                .append(
                    "event-0",
                    JournalKind::UserMessage,
                    "session-a",
                    None,
                    json!({"text":"safe"})
                )
                .unwrap_err()
                .code,
            "journal_duplicate_expired"
        );
        assert_eq!(
            journal
                .append(
                    "event-0",
                    JournalKind::UserMessage,
                    "session-a",
                    None,
                    json!({"text":"changed"})
                )
                .unwrap_err()
                .code,
            "journal_event_conflict"
        );
        assert_eq!(
            journal.last_sequence_for("session-a"),
            (JOURNAL_CAPACITY + 1) as u64
        );
    }

    #[test]
    fn generation_rotation_rejects_old_ack_and_clears_old_window() {
        let journal = RemoteJournal::new("boot-1");
        let first = journal.bind_generation("generation-1").unwrap();
        journal
            .append(
                "event-1",
                JournalKind::UserMessage,
                "session-a",
                None,
                json!({"text":"old"}),
            )
            .unwrap();
        let second = journal.bind_generation("generation-2").unwrap();
        assert_ne!(first, second);
        assert!(journal.snapshot_for("session-a").unwrap().events.is_empty());
        assert_eq!(
            journal
                .acknowledge_for("session-a", &first, 1)
                .unwrap_err()
                .code,
            "journal_epoch_stale"
        );
        assert_eq!(
            journal.bind_generation("generation-1").unwrap_err().code,
            "journal_generation_retired"
        );
    }

    #[test]
    fn generation_check_and_journal_operation_share_one_lock() {
        use std::sync::{Arc, Barrier};

        let journal = Arc::new(RemoteJournal::new("boot"));
        let first_epoch = journal.bind_generation("generation-1").unwrap();
        journal
            .append_for_generation(
                "generation-1",
                "event-old",
                JournalKind::UserMessage,
                "session-a",
                None,
                json!({"text":"old"}),
            )
            .unwrap();
        let ready = Arc::new(Barrier::new(2));
        let switched = Arc::new(Barrier::new(2));
        let old_writer = {
            let journal = journal.clone();
            let ready = ready.clone();
            let switched = switched.clone();
            std::thread::spawn(move || {
                // Caller has captured a previously valid generation. A new
                // lease switches before its append enters the journal lock.
                ready.wait();
                switched.wait();
                journal.append_for_generation(
                    "generation-1",
                    "event-late",
                    JournalKind::UserMessage,
                    "session-a",
                    None,
                    json!({"text":"must-not-land"}),
                )
            })
        };
        ready.wait();
        let second_epoch = journal.bind_generation("generation-2").unwrap();
        switched.wait();
        assert_eq!(
            old_writer.join().unwrap().unwrap_err().code,
            "journal_generation_stale"
        );
        assert_ne!(first_epoch, second_epoch);
        for operation in [
            journal
                .replay_after_for_generation("generation-1", "session-a", &second_epoch, 0)
                .map(|_| ()),
            journal
                .snapshot_for_generation("generation-1", "session-a")
                .map(|_| ()),
            journal
                .acknowledge_for_generation("generation-1", "session-a", &second_epoch, 1)
                .map(|_| ()),
        ] {
            assert_eq!(operation.unwrap_err().code, "journal_generation_stale");
        }
        let new = journal
            .append_for_generation(
                "generation-2",
                "event-new",
                JournalKind::UserMessage,
                "session-a",
                None,
                json!({"text":"new"}),
            )
            .unwrap();
        assert_eq!(new.sequence, 1);
        let snapshot = journal
            .snapshot_for_generation("generation-2", "session-a")
            .unwrap();
        assert_eq!(snapshot.events, vec![new]);
    }
}

#[cfg(test)]
impl RemoteTaskEventSink for RemoteJournal {
    fn publish_task_event(&self, event: &RemoteTaskEvent) {
        let _ = self.record_task_event(event);
    }
}

impl RemoteJournal {
    /// E2E-05：把一条任务状态事件写进权威 journal 并返回落库后的事件，供 B 端
    /// 通过 wire `journal_event` 回传 A。重复投递按同一 event_id 幂等。
    #[cfg(test)]
    pub(crate) fn record_task_event(&self, event: &RemoteTaskEvent) -> Option<JournalEvent> {
        self.record_task_event_inner(None, event)
    }

    pub(crate) fn record_task_event_for_generation(
        &self,
        generation: &str,
        event: &RemoteTaskEvent,
    ) -> Option<JournalEvent> {
        self.record_task_event_inner(Some(generation), event)
    }

    fn record_task_event_inner(
        &self,
        generation: Option<&str>,
        event: &RemoteTaskEvent,
    ) -> Option<JournalEvent> {
        let terminal = matches!(
            event.status.as_str(),
            "succeeded" | "failed" | "cancelled" | "interrupted" | "result_unknown"
        );
        if !terminal {
            // The independent task_status frame carries in-progress states.
            return None;
        }
        let payload = json!({
            "status": event.status,
            "outcome_unknown": event.outcome_unknown,
            "terminal_reason": event.terminal_reason,
            "result_digest": event.result_digest,
            "sequence": event.sequence,
            "transition_sequence": event.transition_sequence,
        });
        // `RemoteTaskRecord::sequence` identifies task admission order and is
        // intentionally stable across admitted/running/terminal transitions.
        // Non-terminal states never allocate a journal sequence.
        let event_identity = format!(
            "{}\0{}\0{}",
            event.task_id, event.sequence, event.transition_sequence
        );
        let event_id = format!(
            "task-{}",
            crate::platform::encoding::hex_lower(&Sha256::digest(event_identity.as_bytes()))
        );
        let result = match generation {
            Some(generation) => self.append_terminal_for_generation(
                generation,
                &event_id,
                &event.session_id,
                &event.task_id,
                &event.status,
                payload,
            ),
            #[cfg(test)]
            None => self.append_terminal(
                &event_id,
                &event.session_id,
                &event.task_id,
                &event.status,
                payload,
            ),
            #[cfg(not(test))]
            None => return None,
        };
        match result {
            Ok(stored) => stored,
            Err(error) => {
                eprintln!("[remote-control] journal append rejected: {}", error.code);
                None
            }
        }
    }
}

fn validate_id(value: &str, max_chars: usize) -> Result<()> {
    if value.is_empty()
        || value.len() > max_chars
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b':'))
    {
        return Err(error("journal_identifier_invalid"));
    }
    Ok(())
}

/// Allowlists the keys that may reach the journal and redacts anything that
/// looks like a credential or a host path.
pub(crate) fn sanitize_event_payload(payload: &Value) -> Value {
    match payload {
        Value::Object(object) => {
            let mut sanitized = Map::new();
            for (key, value) in object {
                if !ALLOWED_KEYS.contains(&key.as_str()) {
                    continue;
                }
                sanitized.insert(key.clone(), sanitize_value(key, value));
            }
            Value::Object(sanitized)
        }
        other => sanitize_value("payload", other),
    }
}

const ALLOWED_KEYS: &[&str] = &[
    "status",
    "outcome_unknown",
    "terminal_reason",
    "result_digest",
    "sequence",
    "text",
    "delta",
    "thinking",
    "tool_id",
    "tool_name",
    "tool_state",
    "operation_type",
    "risk_level",
    "target_scope",
    "decision",
    "code",
    "message",
    "attachment_id",
    "artifact_id",
    "display_name",
    "mime_type",
    "total_bytes",
    "sha256",
    "preview_kind",
    "truncated",
    "count",
];

fn sanitize_value(key: &str, value: &Value) -> Value {
    match value {
        Value::String(text) => Value::String(sanitize_text(text)),
        Value::Array(items) => Value::Array(
            items
                .iter()
                .take(64)
                .map(|item| sanitize_value(key, item))
                .collect(),
        ),
        Value::Object(_) => json!(null),
        other => other.clone(),
    }
}

pub(crate) fn sanitize_text(text: &str) -> String {
    let mut sanitizer = StreamingTextSanitizer::default();
    let mut out = sanitizer.push(text);
    out.push_str(&sanitizer.finish());
    out.chars().take(MAX_EVENT_TEXT_CHARS).collect()
}

/// Stateful sanitizer for streamed model output. An incomplete ASCII token is
/// retained until a safe boundary, so splitting `sk-secret`, a host path, or a
/// signed URL across provider deltas cannot bypass journal redaction.
#[derive(Default)]
pub(crate) struct StreamingTextSanitizer {
    token: String,
    redact_next: bool,
    redact_line: bool,
}

impl StreamingTextSanitizer {
    pub(crate) fn push(&mut self, text: &str) -> String {
        let mut out = String::with_capacity(text.len());
        for ch in text.chars() {
            if ch.is_whitespace() {
                self.flush_token(&mut out);
                out.push(ch);
                if matches!(ch, '\n' | '\r') {
                    self.redact_next = false;
                    self.redact_line = false;
                }
            } else if ch.is_ascii() {
                self.token.push(ch);
            } else {
                // Credential/path syntax is ASCII. Treating a non-ASCII code
                // point as a boundary keeps CJK reasoning visibly streaming
                // without weakening protection for split ASCII secrets.
                self.flush_token(&mut out);
                out.push(ch);
            }
        }
        out
    }

    pub(crate) fn finish(&mut self) -> String {
        let mut out = String::new();
        self.flush_token(&mut out);
        self.redact_next = false;
        self.redact_line = false;
        out
    }

    fn flush_token(&mut self, out: &mut String) {
        if self.token.is_empty() {
            return;
        }
        let lowered = self.token.to_ascii_lowercase();
        let authorization_marker = lowered
            .trim_matches(|ch: char| !ch.is_ascii_alphanumeric() && ch != ':' && ch != '-')
            .starts_with("authorization:");
        let cookie_marker = lowered
            .trim_matches(|ch: char| !ch.is_ascii_alphanumeric() && ch != ':' && ch != '-')
            .starts_with("cookie:");
        let bearer_marker = lowered
            .trim_matches(|ch: char| !ch.is_ascii_alphanumeric() && ch != '-')
            .eq("bearer");
        let forced_redaction = self.redact_line || self.redact_next;
        self.redact_next = bearer_marker;
        self.redact_line |= authorization_marker || cookie_marker;

        if forced_redaction
            || authorization_marker
            || cookie_marker
            || bearer_marker
            || looks_like_secret(&lowered)
        {
            out.push_str("[REDACTED]");
        } else if let Some(url) = sanitize_url_token(&self.token) {
            out.push_str(&url);
        } else if looks_like_path(&self.token) {
            out.push_str("[PATH]");
        } else {
            out.push_str(&self.token);
        }
        self.token.clear();
    }
}

fn looks_like_secret(lowered: &str) -> bool {
    [
        "sk-",
        "token=",
        "access_token=",
        "access-token=",
        "api_key",
        "api-key",
        "secret=",
        "signature=",
        "x-amz-signature=",
        "sig=",
    ]
    .iter()
    .any(|marker| lowered.contains(marker))
}

/// Preserve a public link's Markdown shape while removing query/fragment data.
/// User-info cannot be made safely presentational and therefore fails closed.
fn sanitize_url_token(token: &str) -> Option<String> {
    let lowered = token.to_ascii_lowercase();
    let Some(start) = lowered.find("https://").or_else(|| lowered.find("http://")) else {
        return None;
    };
    let url = &token[start..];
    let lowered_url = &lowered[start..];
    let scheme_bytes = if lowered_url.starts_with("https://") {
        8
    } else {
        7
    };
    let after_scheme = &lowered_url[scheme_bytes..];
    let authority_end = after_scheme
        .find(['/', '?', '#', ')', ']'])
        .unwrap_or(after_scheme.len());
    if after_scheme[..authority_end].contains('@') {
        return Some("[REDACTED]".to_owned());
    }
    let sensitive_start = url.find(['?', '#'])?;
    let suffix_start = url[sensitive_start..]
        .find([')', ']', '>', ',', ';'])
        .map(|offset| sensitive_start + offset)
        .unwrap_or(url.len());
    Some(format!(
        "{}{}{}",
        &token[..start],
        &url[..sensitive_start],
        &url[suffix_start..]
    ))
}

fn looks_like_path(value: &str) -> bool {
    let trimmed = value.trim_matches(|ch: char| matches!(ch, '\'' | '"' | ',' | ';' | '(' | ')'));
    let lowered = trimmed.to_ascii_lowercase();
    if lowered.contains("http://") || lowered.contains("https://") {
        return false;
    }
    let bytes = trimmed.as_bytes();
    if bytes.len() >= 3
        && bytes[0].is_ascii_alphabetic()
        && bytes[1] == b':'
        && matches!(bytes[2], b'\\' | b'/')
    {
        return true;
    }
    let separators = trimmed.matches(['\\', '/']).count();
    separators >= 2
        || (separators == 1
            && trimmed.len() > 1
            && (trimmed.starts_with('/') || trimmed.contains('\\')))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn journal() -> RemoteJournal {
        RemoteJournal::new("epoch-1")
    }

    #[test]
    fn replay_returns_the_exact_suffix_and_detects_gaps() {
        let journal = journal();
        for index in 0..5 {
            journal
                .append(
                    &format!("event-{index}"),
                    JournalKind::AssistantDelta,
                    "session-0001",
                    Some("task-0001"),
                    json!({ "delta": format!("chunk {index}") }),
                )
                .unwrap();
        }
        match journal
            .replay_after_for("session-0001", "epoch-1", 2)
            .unwrap()
        {
            ReplayOutcome::Events(events) => {
                assert_eq!(events.len(), 3);
                assert_eq!(events[0].sequence, 3);
                assert_eq!(events[2].sequence, 5);
            }
            other => panic!("unexpected {other:?}"),
        }
        assert!(matches!(
            journal
                .replay_after_for("session-0001", "epoch-other", 2)
                .unwrap(),
            ReplayOutcome::Gap(_)
        ));
    }

    #[test]
    fn journal_trimming_produces_a_gap_instead_of_a_partial_replay() {
        let journal = journal();
        for index in 0..(JOURNAL_CAPACITY + 10) {
            journal
                .append(
                    &format!("event-{index}"),
                    JournalKind::AssistantDelta,
                    "session-0001",
                    None,
                    json!({ "delta": "x" }),
                )
                .unwrap();
        }
        assert!(journal.len_for("session-0001") <= JOURNAL_CAPACITY);
        match journal
            .replay_after_for("session-0001", "epoch-1", 1)
            .unwrap()
        {
            ReplayOutcome::Gap(snapshot) => {
                assert_eq!(snapshot.epoch, "epoch-1");
                assert!(!snapshot.digest.is_empty());
            }
            ReplayOutcome::Events(_) => panic!("an old gap must not replay partially"),
        }
    }

    #[test]
    fn byte_budget_produces_valid_shorter_snapshot_at_same_cursor() {
        let journal = RemoteJournal::new("epoch-1");
        // Exercise the byte window after the 4096-character sanitizer, not a
        // raw ASCII input that is truncated below the window before append.
        let body = "界".repeat(MAX_EVENT_TEXT_CHARS);
        for sequence in 1..=40 {
            journal
                .append(
                    &format!("event-{sequence}"),
                    JournalKind::AssistantDelta,
                    "session-a",
                    Some("task-1"),
                    json!({"text": body}),
                )
                .unwrap();
        }
        let snapshot = journal.snapshot_for("session-a").unwrap();
        assert!(snapshot.events.len() < 40);
        assert!(snapshot.events.iter().all(|event| {
            event.payload["text"].as_str().unwrap().chars().count() == MAX_EVENT_TEXT_CHARS
        }));
        assert_eq!(snapshot.sequence, 40);
        assert_eq!(snapshot.events.last().unwrap().sequence, 40);
        assert!(snapshot.verify_digest());
    }

    #[test]
    fn duplicate_delivery_does_not_duplicate_events() {
        let journal = journal();
        let first = journal
            .append(
                "event-1",
                JournalKind::Tool,
                "session-0001",
                Some("task-0001"),
                json!({ "tool_id": "browser", "tool_state": "running" }),
            )
            .unwrap();
        let replay = journal
            .append(
                "event-1",
                JournalKind::Tool,
                "session-0001",
                Some("task-0001"),
                json!({ "tool_id": "browser", "tool_state": "running" }),
            )
            .unwrap();
        assert_eq!(first.sequence, replay.sequence);
        assert_eq!(journal.len_for("session-0001"), 1);
    }

    #[test]
    fn only_one_terminal_state_is_journaled_per_task() {
        let journal = journal();
        journal
            .append_terminal(
                "terminal-1",
                "session-0001",
                "task-0001",
                "succeeded",
                json!({ "status": "succeeded" }),
            )
            .unwrap();
        let second = journal
            .append_terminal(
                "terminal-2",
                "session-0001",
                "task-0001",
                "failed",
                json!({ "status": "failed" }),
            )
            .unwrap();
        assert!(second.is_none());
        let snapshot = journal.snapshot_for("session-0001").unwrap();
        let terminals: Vec<&str> = snapshot
            .events
            .iter()
            .filter_map(|event| event.terminal_status.as_deref())
            .collect();
        assert_eq!(terminals, vec!["succeeded"]);
    }

    #[test]
    fn payloads_are_allowlisted_and_redacted() {
        let journal = journal();
        let event = journal
            .append(
                "event-1",
                JournalKind::Error,
                "session-0001",
                Some("task-0001"),
                json!({
                    "code": "executor_error",
                    "message": "failed reading C:\\Users\\me\\secret.txt with sk-live-123",
                    "credential_ref": "vault://secret",
                    "api_key": "sk-nope",
                }),
            )
            .unwrap();
        let encoded = serde_json::to_string(&event).unwrap();
        assert!(!encoded.contains("vault://secret"));
        assert!(!encoded.contains("sk-nope"));
        assert!(!encoded.contains("sk-live-123"));
        assert!(!encoded.contains("secret.txt"));
        assert!(encoded.contains("[REDACTED]"));
        assert!(encoded.contains("[PATH]"));
        assert!(event.payload.get("credential_ref").is_none());
    }

    #[test]
    fn sanitizing_text_preserves_markdown_layout_and_public_links() {
        let markdown =
            "# Title\n\n| A | B |\n|---|---|\n| 33℃ / 24℃ | [source](https://example.com/a/b) |";
        assert_eq!(sanitize_text(markdown), markdown);

        let redacted = sanitize_text("before\nC:\\Users\\me\\secret.txt\nafter sk-secret");
        assert_eq!(redacted, "before\n[PATH]\nafter [REDACTED]");
        assert_eq!(
            sanitize_text("[private](https://example.com/a?token=secret)"),
            "[REDACTED]"
        );
        assert_eq!(
            sanitize_text("[signed](https://example.com/a?X-Amz-Signature=secret)"),
            "[REDACTED]"
        );
        assert_eq!(
            sanitize_text("https://user:secret@example.com/a"),
            "[REDACTED]"
        );
        assert_eq!(
            sanitize_text("[docs](https://example.com/a?page=1#top)"),
            "[docs](https://example.com/a)"
        );
        assert_eq!(
            sanitize_text("Bearer secret-value\nCookie: session=secret other=value"),
            "[REDACTED] [REDACTED]\n[REDACTED] [REDACTED] [REDACTED]"
        );
    }

    #[test]
    fn streaming_sanitizer_keeps_sensitive_tokens_across_delta_boundaries() {
        let mut sanitizer = StreamingTextSanitizer::default();
        let mut output = sanitizer.push("prefix sk");
        output.push_str(&sanitizer.push("-split-secret and C:\\Users"));
        output.push_str(&sanitizer.push("\\me\\secret.txt then "));
        output.push_str(&sanitizer.push("https://example.com/file?access_"));
        output.push_str(&sanitizer.push("token=secret"));
        output.push_str(&sanitizer.finish());
        assert_eq!(output, "prefix [REDACTED] and [PATH] then [REDACTED]");
    }

    #[test]
    fn oversized_and_unknown_payload_shapes_fail_closed() {
        let journal = journal();
        // Even after sanitization an allowlisted array can exceed the payload
        // budget, and the journal refuses it instead of truncating silently.
        let wide: Vec<String> = (0..64).map(|_| "x".repeat(MAX_EVENT_TEXT_CHARS)).collect();
        assert_eq!(
            journal
                .append(
                    "event-1",
                    JournalKind::AssistantDelta,
                    "session-0001",
                    None,
                    json!({ "delta": wide }),
                )
                .unwrap_err()
                .code,
            "journal_payload_too_large"
        );
        assert_eq!(
            journal
                .append(
                    "bad id!",
                    JournalKind::AssistantDelta,
                    "session-0001",
                    None,
                    json!({ "delta": "x" }),
                )
                .unwrap_err()
                .code,
            "journal_identifier_invalid"
        );
    }

    #[test]
    fn reset_starts_a_new_epoch_and_snapshot_carries_the_base_sequence() {
        let journal = journal();
        journal
            .append(
                "event-1",
                JournalKind::UserMessage,
                "session-0001",
                None,
                json!({ "text": "hello" }),
            )
            .unwrap();
        journal.reset("epoch-2");
        let snapshot = journal.snapshot_for("session-0001").unwrap();
        assert_eq!(snapshot.epoch, "epoch-2");
        assert_eq!(snapshot.sequence, 0);
        assert!(snapshot.events.is_empty());
        assert!(matches!(
            journal
                .replay_after_for("session-0001", "epoch-1", 0)
                .unwrap(),
            ReplayOutcome::Gap(_)
        ));
    }

    #[test]
    fn acknowledgement_is_monotonic_and_bounded_by_the_sequence() {
        let journal = journal();
        for index in 0..3 {
            journal
                .append(
                    &format!("event-{index}"),
                    JournalKind::AssistantDelta,
                    "session-0001",
                    None,
                    json!({ "delta": "x" }),
                )
                .unwrap();
        }
        journal
            .acknowledge_for("session-0001", "epoch-1", 2)
            .unwrap();
        assert_eq!(journal.acknowledged_for("session-0001"), 2);
        journal
            .acknowledge_for("session-0001", "epoch-1", 1)
            .unwrap();
        assert_eq!(journal.acknowledged_for("session-0001"), 2);
        journal
            .acknowledge_for("session-0001", "epoch-1", 99)
            .unwrap();
        assert_eq!(journal.acknowledged_for("session-0001"), 3);
    }

    #[test]
    fn task_sink_appends_bounded_status_events_only() {
        let journal = journal();
        let sink: &dyn RemoteTaskEventSink = &journal;
        sink.publish_task_event(&RemoteTaskEvent {
            task_id: "task-0001".into(),
            session_id: "session-0001".into(),
            status: "running".into(),
            sequence: "1".into(),
            transition_sequence: 2,
            outcome_unknown: false,
            terminal_reason: None,
            result_digest: Some("a".repeat(64)),
        });
        sink.publish_task_event(&RemoteTaskEvent {
            task_id: "task-0001".into(),
            session_id: "session-0001".into(),
            status: "succeeded".into(),
            sequence: "2".into(),
            transition_sequence: 3,
            outcome_unknown: false,
            terminal_reason: Some("completed".into()),
            result_digest: Some("a".repeat(64)),
        });
        let snapshot = journal.snapshot_for("session-0001").unwrap();
        assert_eq!(snapshot.events.len(), 1);
        assert_eq!(
            snapshot.events[0].terminal_status.as_deref(),
            Some("succeeded")
        );
        let encoded = serde_json::to_string(&snapshot).unwrap();
        assert!(!encoded.contains(r"C:\"));
    }

    /// E2E-05：任务状态事件落库后必须能被回传层拿到（含终态），重复投递幂等。
    #[test]
    fn recorded_task_events_are_returned_for_wire_delivery() {
        let journal = RemoteJournal::new("epoch-1");
        let event = RemoteTaskEvent {
            task_id: "task-0001".into(),
            session_id: "session-0001".into(),
            status: "running".into(),
            sequence: "1".into(),
            transition_sequence: 2,
            outcome_unknown: false,
            terminal_reason: None,
            result_digest: None,
        };
        assert!(journal.record_task_event(&event).is_none());
        assert_eq!(journal.len_for("session-0001"), 0);
        let waiting = RemoteTaskEvent {
            status: "waiting_permission".into(),
            transition_sequence: 3,
            ..event.clone()
        };
        assert!(journal.record_task_event(&waiting).is_none());
        let resumed = RemoteTaskEvent {
            transition_sequence: 4,
            ..event.clone()
        };
        assert!(journal.record_task_event(&resumed).is_none());
        assert_eq!(journal.len_for("session-0001"), 0);
        // 终态只落一次，冲突的终态不会改写已落库结果。
        let terminal = RemoteTaskEvent {
            status: "succeeded".into(),
            // Task sequence is the admission order, so transitions retain it.
            sequence: "1".into(),
            transition_sequence: 5,
            terminal_reason: Some("completed".into()),
            ..event.clone()
        };
        let stored = journal.record_task_event(&terminal).unwrap();
        assert_eq!(stored.terminal_status.as_deref(), Some("succeeded"));
        assert_eq!(stored.sequence, 1);
        let again = journal.record_task_event(&terminal).unwrap();
        assert_eq!(again, stored);

        let long_id_event = RemoteTaskEvent {
            task_id: "t".repeat(MAX_TASK_ID_CHARS),
            ..terminal
        };
        let long_id_journal = RemoteJournal::new("epoch-long-id");
        let stored = long_id_journal.record_task_event(&long_id_event).unwrap();
        assert!(stored.event_id.len() <= MAX_TASK_ID_CHARS);
        assert!(stored.event_id.starts_with("task-"));
    }
}
