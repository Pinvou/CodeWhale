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
        let _ = self.record_task_event(event);
    }
}

impl RemoteJournal {
    /// E2E-05：把一条任务状态事件写进权威 journal 并返回落库后的事件，供 B 端
    /// 通过 wire `journal_event` 回传 A。重复投递按同一 event_id 幂等。
    pub(crate) fn record_task_event(&self, event: &RemoteTaskEvent) -> Option<JournalEvent> {
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
        // Include status so those distinct events do not collapse into the
        // first non-exportable admitted event in the journal.
        let event_identity = format!(
            "{}\0{}\0{}",
            event.task_id, event.sequence, event.transition_sequence
        );
        let event_id = format!(
            "task-{}",
            crate::platform::encoding::hex_lower(&Sha256::digest(event_identity.as_bytes()))
        );
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
        match result {
            Ok(()) => self
                .inner
                .lock()
                .events
                .iter()
                .rev()
                .find(|stored| stored.event_id == event_id)
                .cloned(),
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
        let stored = journal.record_task_event(&event).unwrap();
        assert_eq!(stored.kind, "task_status");
        assert_eq!(stored.sequence, 1);
        assert_eq!(stored.session_id, "session-0001");
        assert_eq!(stored.payload["status"], "running");
        // 重复投递（同 task + sequence）幂等：不新增事件，仍返回同一投影。
        let again = journal.record_task_event(&event).unwrap();
        assert_eq!(again, stored);
        assert_eq!(journal.len(), 1);
        let waiting = RemoteTaskEvent {
            status: "waiting_permission".into(),
            transition_sequence: 3,
            ..event.clone()
        };
        journal.record_task_event(&waiting).unwrap();
        let resumed = RemoteTaskEvent {
            transition_sequence: 4,
            ..event.clone()
        };
        journal.record_task_event(&resumed).unwrap();
        assert_eq!(journal.len(), 3);
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
        assert_eq!(stored.sequence, 4);

        let long_id_event = RemoteTaskEvent {
            task_id: "t".repeat(MAX_TASK_ID_CHARS),
            ..event
        };
        let long_id_journal = RemoteJournal::new("epoch-long-id");
        let stored = long_id_journal.record_task_event(&long_id_event).unwrap();
        assert!(stored.event_id.len() <= MAX_TASK_ID_CHARS);
        assert!(stored.event_id.starts_with("task-"));
    }
}
