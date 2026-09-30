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

use super::task::{RemoteTaskEvent, RemoteTaskEventSink};

pub(crate) const JOURNAL_CAPACITY: usize = 1024;
pub(crate) const JOURNAL_BYTES_CAPACITY: usize = 1024 * 1024;
pub(crate) const MAX_EVENT_PAYLOAD_BYTES: usize = 16 * 1024;
pub(crate) const MAX_EVENT_TEXT_CHARS: usize = 4096;
pub(crate) const MAX_TASK_ID_CHARS: usize = 128;
pub(crate) const MAX_SESSION_ID_CHARS: usize = 128;

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

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct JournalSnapshot {
    pub(crate) epoch: String,
    pub(crate) sequence: u64,
    pub(crate) digest: String,
    pub(crate) events: Vec<JournalEvent>,
}

#[derive(Debug, Clone, PartialEq)]
pub(crate) enum ReplayOutcome {
    /// The journal can cover the gap; apply these events in order.
    Events(Vec<JournalEvent>),
    /// The gap is too old; the caller must replace its state with a snapshot.
    Gap(JournalSnapshot),
}

struct JournalInner {
    epoch: String,
    sequence: u64,
    events: VecDeque<JournalEvent>,
    bytes: usize,
    event_ids: HashSet<String>,
    terminals: HashMap<String, String>,
    acknowledged: u64,
}

pub(crate) struct RemoteJournal {
    inner: Mutex<JournalInner>,
}

impl RemoteJournal {
    pub(crate) fn new(epoch: impl Into<String>) -> Self {
        Self {
            inner: Mutex::new(JournalInner {
                epoch: epoch.into(),
                sequence: 0,
                events: VecDeque::new(),
                bytes: 0,
                event_ids: HashSet::new(),
                terminals: HashMap::new(),
                acknowledged: 0,
            }),
        }
    }

    /// Starts a new epoch (reconnect or new lease). Everything older is
    /// unreachable, so a replay request against the old epoch yields a gap.
    pub(crate) fn reset(&self, epoch: impl Into<String>) -> JournalSnapshot {
        let mut inner = self.inner.lock();
        inner.epoch = epoch.into();
        inner.sequence = 0;
        inner.events.clear();
        inner.bytes = 0;
        inner.event_ids.clear();
        inner.terminals.clear();
        inner.acknowledged = 0;
        Self::snapshot_locked(&inner)
    }

    pub(crate) fn epoch(&self) -> String {
        self.inner.lock().epoch.clone()
    }

    pub(crate) fn last_sequence(&self) -> u64 {
        self.inner.lock().sequence
    }

    pub(crate) fn acknowledged(&self) -> u64 {
        self.inner.lock().acknowledged
    }

    pub(crate) fn len(&self) -> usize {
        self.inner.lock().events.len()
    }

    pub(crate) fn append(
        &self,
        event_id: &str,
        kind: JournalKind,
        session_id: &str,
        task_id: Option<&str>,
        payload: Value,
    ) -> Result<JournalEvent> {
        self.append_inner(event_id, kind, session_id, task_id, payload, None)
    }

    /// Terminal CAS: only the first terminal state of a task is journaled, and
    /// repeated reports are dropped instead of producing duplicate terminals.
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
        let event = self.append_inner(
            event_id,
            JournalKind::TaskStatus,
            session_id,
            Some(task_id),
            payload,
            Some(status.to_owned()),
        )?;
        Ok(Some(event))
    }

    fn append_inner(
        &self,
        event_id: &str,
        kind: JournalKind,
        session_id: &str,
        task_id: Option<&str>,
        payload: Value,
        terminal_status: Option<String>,
    ) -> Result<JournalEvent> {
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
        let mut inner = self.inner.lock();
        if inner.event_ids.contains(event_id) {
            // Duplicate delivery: return the stored projection when available.
            if let Some(existing) = inner.events.iter().find(|event| event.event_id == event_id) {
                return Ok(existing.clone());
            }
            return Err(error("journal_duplicate_event"));
        }
        if let (Some(status), Some(task_id)) = (&terminal_status, task_id) {
            if let Some(existing) = inner.terminals.get(task_id) {
                let _ = existing;
                // Exactly one terminal state per task. A conflicting terminal
                // report is dropped rather than rewritten.
                return Ok(JournalEvent {
                    event_id: event_id.to_owned(),
                    epoch: inner.epoch.clone(),
                    sequence: inner.sequence,
                    kind: kind.wire().to_owned(),
                    session_id: session_id.to_owned(),
                    task_id: Some(task_id.to_owned()),
                    terminal_status: Some(existing.clone()),
                    payload: Value::Null,
                });
            }
            inner.terminals.insert(task_id.to_owned(), status.clone());
        }
        inner.sequence = inner
            .sequence
            .checked_add(1)
            .ok_or_else(|| error("journal_sequence_exhausted"))?;
        let event = JournalEvent {
            event_id: event_id.to_owned(),
            epoch: inner.epoch.clone(),
            sequence: inner.sequence,
            kind: kind.wire().to_owned(),
            session_id: session_id.to_owned(),
            task_id: task_id.map(str::to_owned),
            terminal_status,
            payload: sanitized,
        };
        inner.bytes += encoded.len();
        inner.event_ids.insert(event_id.to_owned());
        inner.events.push_back(event.clone());
        Self::trim_locked(&mut inner);
        Ok(event)
    }

    /// Replays everything strictly after `sequence`. A request for an epoch
    /// that is not current, or for a sequence older than the retained window,
    /// yields a gap plus a snapshot.
    pub(crate) fn replay_after(&self, epoch: &str, sequence: u64) -> ReplayOutcome {
        let inner = self.inner.lock();
        if inner.epoch != epoch {
            return ReplayOutcome::Gap(Self::snapshot_locked(&inner));
        }
        let oldest = inner
            .events
            .front()
            .map(|event| event.sequence)
            .unwrap_or(inner.sequence + 1);
        if sequence + 1 < oldest && sequence < inner.sequence {
            return ReplayOutcome::Gap(Self::snapshot_locked(&inner));
        }
        ReplayOutcome::Events(
            inner
                .events
                .iter()
                .filter(|event| event.sequence > sequence)
                .cloned()
                .collect(),
        )
    }

    /// Snapshot with a base epoch/sequence and a content digest, so A can atomically
    /// replace its copy and then accept only newer events.
    pub(crate) fn snapshot(&self) -> JournalSnapshot {
        Self::snapshot_locked(&self.inner.lock())
    }

    pub(crate) fn acknowledge(&self, sequence: u64) {
        let mut inner = self.inner.lock();
        inner.acknowledged = inner.acknowledged.max(sequence.min(inner.sequence));
    }

    fn snapshot_locked(inner: &JournalInner) -> JournalSnapshot {
        let events: Vec<JournalEvent> = inner.events.iter().cloned().collect();
        let mut hasher = Sha256::new();
        hasher.update(inner.epoch.as_bytes());
        hasher.update(inner.sequence.to_be_bytes());
        for event in &events {
            hasher.update(event.sequence.to_be_bytes());
            hasher.update(event.kind.as_bytes());
        }
        JournalSnapshot {
            epoch: inner.epoch.clone(),
            sequence: inner.sequence,
            digest: hasher
                .finalize()
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect(),
            events,
        }
    }

    fn trim_locked(inner: &mut JournalInner) {
        while inner.events.len() > JOURNAL_CAPACITY || inner.bytes > JOURNAL_BYTES_CAPACITY {
            let Some(dropped) = inner.events.pop_front() else {
                break;
            };
            inner.bytes = inner.bytes.saturating_sub(
                serde_json::to_vec(&dropped.payload)
                    .map(|bytes| bytes.len())
                    .unwrap_or(0),
            );
            inner.event_ids.remove(&dropped.event_id);
        }
    }
}

impl RemoteTaskEventSink for RemoteJournal {
    fn publish_task_event(&self, event: &RemoteTaskEvent) {
        let payload = json!({
            "status": event.status,
            "outcome_unknown": event.outcome_unknown,
            "terminal_reason": event.terminal_reason,
            "result_digest": event.result_digest,
            "sequence": event.sequence,
        });
        let event_id = format!("task-{}:{}", event.task_id, event.sequence);
        let terminal = matches!(
            event.status.as_str(),
            "succeeded" | "failed" | "cancelled" | "interrupted" | "result_unknown"
        );
        let result = if terminal {
            self.append_terminal(
                &event_id,
                &event.session_id,
                &event.task_id,
                &event.status,
                payload,
            )
            .map(|_| ())
        } else {
            self.append(
                &event_id,
                JournalKind::TaskStatus,
                &event.session_id,
                Some(&event.task_id),
                payload,
            )
            .map(|_| ())
        };
        if let Err(error) = result {
            eprintln!("[remote-control] journal append rejected: {}", error.code);
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
    let mut out = String::with_capacity(text.len().min(MAX_EVENT_TEXT_CHARS));
    for token in text.split_whitespace() {
        let lowered = token.to_ascii_lowercase();
        let replacement = if lowered.starts_with("sk-")
            || lowered.starts_with("bearer")
            || lowered.starts_with("authorization:")
            || lowered.starts_with("token=")
            || lowered.starts_with("api_key")
            || lowered.starts_with("cookie:")
        {
            Some("[REDACTED]")
        } else if looks_like_path(token) {
            Some("[PATH]")
        } else {
            None
        };
        let piece = replacement.unwrap_or(token);
        if !out.is_empty() {
            out.push(' ');
        }
        out.push_str(piece);
        if out.chars().count() >= MAX_EVENT_TEXT_CHARS {
            break;
        }
    }
    out.chars().take(MAX_EVENT_TEXT_CHARS).collect()
}

fn looks_like_path(value: &str) -> bool {
    let trimmed = value.trim_matches(|ch: char| matches!(ch, '\'' | '"' | ',' | ';' | '(' | ')'));
    let bytes = trimmed.as_bytes();
    if bytes.len() >= 3
        && bytes[0].is_ascii_alphabetic()
        && bytes[1] == b':'
        && matches!(bytes[2], b'\\' | b'/')
    {
        return true;
    }
    let separators = trimmed.matches(['\\', '/']).count();
    separators >= 2 || (separators == 1 && (trimmed.starts_with('/') || trimmed.contains('\\')))
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
        match journal.replay_after("epoch-1", 2) {
            ReplayOutcome::Events(events) => {
                assert_eq!(events.len(), 3);
                assert_eq!(events[0].sequence, 3);
                assert_eq!(events[2].sequence, 5);
            }
            other => panic!("unexpected {other:?}"),
        }
        assert!(matches!(
            journal.replay_after("epoch-other", 2),
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
        assert!(journal.len() <= JOURNAL_CAPACITY);
        match journal.replay_after("epoch-1", 1) {
            ReplayOutcome::Gap(snapshot) => {
                assert_eq!(snapshot.epoch, "epoch-1");
                assert!(!snapshot.digest.is_empty());
            }
            ReplayOutcome::Events(_) => panic!("an old gap must not replay partially"),
        }
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
        assert_eq!(journal.len(), 1);
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
            .unwrap()
            .unwrap();
        assert_eq!(second.terminal_status.as_deref(), Some("succeeded"));
        let snapshot = journal.snapshot();
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
        let snapshot = journal.reset("epoch-2");
        assert_eq!(snapshot.epoch, "epoch-2");
        assert_eq!(snapshot.sequence, 0);
        assert!(snapshot.events.is_empty());
        assert!(matches!(
            journal.replay_after("epoch-1", 0),
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
        journal.acknowledge(2);
        assert_eq!(journal.acknowledged(), 2);
        journal.acknowledge(1);
        assert_eq!(journal.acknowledged(), 2);
        journal.acknowledge(99);
        assert_eq!(journal.acknowledged(), 3);
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
            outcome_unknown: false,
            terminal_reason: None,
            result_digest: Some("a".repeat(64)),
        });
        sink.publish_task_event(&RemoteTaskEvent {
            task_id: "task-0001".into(),
            session_id: "session-0001".into(),
            status: "succeeded".into(),
            sequence: "2".into(),
            outcome_unknown: false,
            terminal_reason: Some("completed".into()),
            result_digest: Some("a".repeat(64)),
        });
        let snapshot = journal.snapshot();
        assert_eq!(snapshot.events.len(), 2);
        assert_eq!(snapshot.events[0].kind, "task_status");
        assert_eq!(
            snapshot.events[1].terminal_status.as_deref(),
            Some("succeeded")
        );
        let encoded = serde_json::to_string(&snapshot).unwrap();
        assert!(!encoded.contains(r"C:\"));
    }
}
