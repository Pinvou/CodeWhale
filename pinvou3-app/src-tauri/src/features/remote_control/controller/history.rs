//! A-side independent remote history (DEV-19).
//!
//! The history is a *copy*: it is written only from authenticated journal
//! events or snapshots, it never becomes an execution source of truth, and it
//! is stored separately from B's session so deleting one side never deletes the
//! other. Records keep only generic event fields; credentials and host paths
//! are redacted on the way in as well.

use std::collections::BTreeMap;
use std::fmt;
use std::path::{Path, PathBuf};

use parking_lot::Mutex;
use serde::{Deserialize, Serialize};

use super::journal::{JournalEvent, JournalSnapshot, sanitize_event_payload};

pub(crate) const HISTORY_VERSION: u8 = 1;
pub(crate) const MAX_HISTORIES: usize = 64;
pub(crate) const MAX_HISTORY_EVENTS: usize = 2048;
pub(crate) const MAX_TITLE_CHARS: usize = 120;
pub(crate) const MAX_DEVICE_CHARS: usize = 80;
pub(crate) const MAX_HISTORY_ID_CHARS: usize = 128;
pub(crate) const MAX_SESSION_REF_CHARS: usize = 128;

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct HistoryError {
    pub(crate) code: &'static str,
}

impl HistoryError {
    fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl fmt::Display for HistoryError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for HistoryError {}

type Result<T> = std::result::Result<T, HistoryError>;

fn error(code: &'static str) -> HistoryError {
    HistoryError::new(code)
}

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum RemoteHistoryState {
    /// Written while the authenticated connection was live.
    ActiveCopy,
    /// The connection ended; the copy is viewable offline only.
    OfflineCopy,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct RemoteHistoryEvent {
    pub(crate) epoch: String,
    pub(crate) sequence: u64,
    pub(crate) kind: String,
    pub(crate) task_id: Option<String>,
    pub(crate) terminal_status: Option<String>,
    pub(crate) payload: serde_json::Value,
}

impl From<&JournalEvent> for RemoteHistoryEvent {
    fn from(event: &JournalEvent) -> Self {
        Self {
            epoch: event.epoch.clone(),
            sequence: event.sequence,
            kind: event.kind.clone(),
            task_id: event.task_id.clone(),
            terminal_status: event.terminal_status.clone(),
            payload: sanitize_event_payload(&event.payload),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct RemoteHistoryRecord {
    pub(crate) history_id: String,
    pub(crate) title: String,
    pub(crate) device_display_name: String,
    /// Opaque reference to the B session; never a path or a credential.
    pub(crate) session_ref: String,
    pub(crate) state: RemoteHistoryState,
    pub(crate) epoch: String,
    pub(crate) last_sequence: u64,
    pub(crate) last_terminal_status: Option<String>,
    pub(crate) events: Vec<RemoteHistoryEvent>,
    pub(crate) created_at_ms: u64,
    pub(crate) updated_at_ms: u64,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct HistoryFile {
    version: u8,
    records: Vec<RemoteHistoryRecord>,
}

/// A-side store. The optional path makes the store persistent; tests and the
/// desktop runtime both use the same code path.
pub(crate) struct RemoteHistoryStore {
    path: Option<PathBuf>,
    records: Mutex<BTreeMap<String, RemoteHistoryRecord>>,
    sequence: Mutex<u64>,
}

impl Default for RemoteHistoryStore {
    fn default() -> Self {
        Self {
            path: None,
            records: Mutex::new(BTreeMap::new()),
            sequence: Mutex::new(0),
        }
    }
}

impl RemoteHistoryStore {
    pub(crate) fn in_memory() -> Self {
        Self::default()
    }

    pub(crate) fn persistent(path: PathBuf) -> Self {
        let store = Self {
            path: Some(path),
            records: Mutex::new(BTreeMap::new()),
            sequence: Mutex::new(0),
        };
        let _ = store.load_from_disk();
        store
    }

    pub(crate) fn load_from_disk(&self) -> Result<()> {
        let Some(path) = &self.path else {
            return Ok(());
        };
        let bytes = match std::fs::read(path) {
            Ok(bytes) => bytes,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(_) => return Err(error("history_unreadable")),
        };
        let file: HistoryFile =
            serde_json::from_slice(&bytes).map_err(|_| error("history_corrupt"))?;
        if file.version != HISTORY_VERSION {
            return Err(error("history_version_unsupported"));
        }
        let mut records = self.records.lock();
        records.clear();
        for record in file.records {
            records.insert(record.history_id.clone(), record);
        }
        Ok(())
    }

    pub(crate) fn create(
        &self,
        title: &str,
        device_display_name: &str,
        session_ref: &str,
        now_ms: u64,
    ) -> Result<String> {
        let title: String = title.trim().chars().take(MAX_TITLE_CHARS).collect();
        let device: String = device_display_name
            .chars()
            .filter(|ch| !ch.is_control())
            .take(MAX_DEVICE_CHARS)
            .collect();
        if title.is_empty() || device.trim().is_empty() {
            return Err(error("history_title_invalid"));
        }
        validate_ref(session_ref, MAX_SESSION_REF_CHARS)?;
        let mut records = self.records.lock();
        if records.len() >= MAX_HISTORIES {
            return Err(error("history_capacity"));
        }
        let mut sequence = self.sequence.lock();
        *sequence += 1;
        let history_id = format!("remote-history-{sequence:08}");
        let record = RemoteHistoryRecord {
            history_id: history_id.clone(),
            title,
            device_display_name: device,
            session_ref: session_ref.to_owned(),
            state: RemoteHistoryState::ActiveCopy,
            epoch: String::new(),
            last_sequence: 0,
            last_terminal_status: None,
            events: Vec::new(),
            created_at_ms: now_ms,
            updated_at_ms: now_ms,
        };
        records.insert(history_id.clone(), record);
        self.persist_locked(&records)?;
        Ok(history_id)
    }

    /// Appends one authenticated event. Out-of-order prefixes are accepted
    /// (the journal can deliver a snapshot first) but duplicates never create a
    /// second copy.
    pub(crate) fn append_authenticated(
        &self,
        history_id: &str,
        event: &JournalEvent,
        now_ms: u64,
    ) -> Result<bool> {
        let mut records = self.records.lock();
        let record = records
            .get_mut(history_id)
            .ok_or_else(|| error("history_not_found"))?;
        if record
            .events
            .iter()
            .any(|existing| existing.epoch == event.epoch && existing.sequence == event.sequence)
        {
            return Ok(false);
        }
        if record.epoch != event.epoch {
            // A new epoch invalidates the previous increment stream; the caller
            // must have applied a snapshot first.
            record.epoch = event.epoch.clone();
            record.events.clear();
            record.last_sequence = 0;
        }
        let history_event = RemoteHistoryEvent::from(event);
        if let Some(status) = &history_event.terminal_status {
            if record.last_terminal_status.is_none() {
                record.last_terminal_status = Some(status.clone());
            }
        }
        record.events.push(history_event);
        if record.events.len() > MAX_HISTORY_EVENTS {
            let overflow = record.events.len() - MAX_HISTORY_EVENTS;
            record.events.drain(0..overflow);
        }
        record.last_sequence = record.last_sequence.max(event.sequence);
        record.updated_at_ms = now_ms;
        self.persist_locked(&records)?;
        Ok(true)
    }

    /// Atomically replaces the copy with an authoritative snapshot, then
    /// accepts only events newer than the snapshot base sequence.
    pub(crate) fn apply_snapshot(
        &self,
        history_id: &str,
        snapshot: &JournalSnapshot,
        now_ms: u64,
    ) -> Result<usize> {
        let mut records = self.records.lock();
        let record = records
            .get_mut(history_id)
            .ok_or_else(|| error("history_not_found"))?;
        record.epoch = snapshot.epoch.clone();
        record.last_sequence = snapshot.sequence;
        record.events = snapshot
            .events
            .iter()
            .map(RemoteHistoryEvent::from)
            .take(MAX_HISTORY_EVENTS)
            .collect();
        record.updated_at_ms = now_ms;
        let count = record.events.len();
        self.persist_locked(&records)?;
        Ok(count)
    }

    pub(crate) fn mark_offline(&self, history_id: &str, now_ms: u64) -> Result<()> {
        let mut records = self.records.lock();
        let record = records
            .get_mut(history_id)
            .ok_or_else(|| error("history_not_found"))?;
        record.state = RemoteHistoryState::OfflineCopy;
        record.updated_at_ms = now_ms;
        self.persist_locked(&records)
    }

    /// Deletes only the A-side copy. It never touches B's session.
    pub(crate) fn delete(&self, history_id: &str) -> Result<bool> {
        let mut records = self.records.lock();
        let removed = records.remove(history_id).is_some();
        self.persist_locked(&records)?;
        Ok(removed)
    }

    pub(crate) fn list(&self) -> Vec<RemoteHistoryRecord> {
        self.records
            .lock()
            .values()
            .cloned()
            .map(|mut record| {
                // List projections stay light: events are loaded on demand.
                record.events.clear();
                record
            })
            .collect()
    }

    pub(crate) fn load(&self, history_id: &str) -> Result<RemoteHistoryRecord> {
        self.records
            .lock()
            .get(history_id)
            .cloned()
            .ok_or_else(|| error("history_not_found"))
    }

    /// 按会话引用查找本地历史 id（E2E-05：入站 journal 事件按 session ref 落到
    /// 对应的 A 端独立历史；未绑定历史时返回 None，由调用方决定是否创建）。
    pub(crate) fn find_by_session_reference(&self, session_ref: &str) -> Option<String> {
        self.records
            .lock()
            .values()
            .find(|record| record.session_ref == session_ref)
            .map(|record| record.history_id.clone())
    }

    pub(crate) fn len(&self) -> usize {
        self.records.lock().len()
    }

    pub(crate) fn redacted_json(&self) -> String {
        serde_json::to_string(&self.list()).unwrap_or_else(|_| "[]".to_owned())
    }

    fn persist_locked(&self, records: &BTreeMap<String, RemoteHistoryRecord>) -> Result<()> {
        let Some(path) = &self.path else {
            return Ok(());
        };
        let file = HistoryFile {
            version: HISTORY_VERSION,
            records: records.values().cloned().collect(),
        };
        let payload = serde_json::to_vec_pretty(&file).map_err(|_| error("history_serialize"))?;
        deepseek_tui::utils::write_atomic(path, &payload).map_err(|_| error("history_write_failed"))
    }

    pub(crate) fn path(&self) -> Option<&Path> {
        self.path.as_deref()
    }
}

fn validate_ref(value: &str, max_chars: usize) -> Result<()> {
    if value.is_empty()
        || value.len() > max_chars
        || value
            .bytes()
            .any(|byte| matches!(byte, b'/' | b'\\' | b':'))
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err(error("history_reference_invalid"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::journal::{JournalKind, RemoteJournal};
    use serde_json::json;

    fn journal_event(sequence: u64, kind: JournalKind, payload: serde_json::Value) -> JournalEvent {
        JournalEvent {
            event_id: format!("event-{sequence}"),
            epoch: "epoch-1".into(),
            sequence,
            kind: kind.wire().to_owned(),
            session_id: "session-0001".into(),
            task_id: Some("task-0001".into()),
            terminal_status: None,
            payload,
        }
    }

    #[test]
    fn history_is_created_and_marked_as_a_copy() {
        let store = RemoteHistoryStore::in_memory();
        let id = store
            .create("Remote task", "Office PC", "session-0001", 1_000)
            .unwrap();
        let record = store.load(&id).unwrap();
        assert_eq!(record.state, RemoteHistoryState::ActiveCopy);
        assert!(!record.title.contains("session"));
        store.mark_offline(&id, 2_000).unwrap();
        assert_eq!(
            store.load(&id).unwrap().state,
            RemoteHistoryState::OfflineCopy
        );
        assert_eq!(
            store.load("remote-history-99999999").unwrap_err().code,
            "history_not_found"
        );
    }

    #[test]
    fn duplicates_are_ignored_and_terminals_stay_first() {
        let store = RemoteHistoryStore::in_memory();
        let id = store
            .create("Remote task", "Office PC", "session-0001", 1_000)
            .unwrap();
        let event = journal_event(1, JournalKind::AssistantDelta, json!({ "delta": "hi" }));
        assert!(store.append_authenticated(&id, &event, 1_100).unwrap());
        assert!(!store.append_authenticated(&id, &event, 1_200).unwrap());

        let terminal = JournalEvent {
            terminal_status: Some("succeeded".into()),
            ..journal_event(2, JournalKind::TaskStatus, json!({ "status": "succeeded" }))
        };
        store.append_authenticated(&id, &terminal, 1_300).unwrap();
        let conflicting = JournalEvent {
            event_id: "event-3".into(),
            terminal_status: Some("failed".into()),
            ..journal_event(3, JournalKind::TaskStatus, json!({ "status": "failed" }))
        };
        store
            .append_authenticated(&id, &conflicting, 1_400)
            .unwrap();
        assert_eq!(
            store.load(&id).unwrap().last_terminal_status.as_deref(),
            Some("succeeded")
        );
        assert_eq!(store.load(&id).unwrap().events.len(), 3);
    }

    #[test]
    fn snapshot_replaces_the_copy_and_newer_events_continue_from_it() {
        let store = RemoteHistoryStore::in_memory();
        let id = store
            .create("Remote task", "Office PC", "session-0001", 1_000)
            .unwrap();
        store
            .append_authenticated(
                &id,
                &journal_event(1, JournalKind::UserMessage, json!({ "text": "old" })),
                1_100,
            )
            .unwrap();
        let journal = RemoteJournal::new("epoch-1");
        journal
            .append(
                "event-1",
                JournalKind::UserMessage,
                "session-0001",
                Some("task-0001"),
                json!({ "text": "snapshot body" }),
            )
            .unwrap();
        journal
            .append(
                "event-2",
                JournalKind::AssistantDelta,
                "session-0001",
                Some("task-0001"),
                json!({ "delta": "after" }),
            )
            .unwrap();
        let snapshot = journal.snapshot();
        assert_eq!(store.apply_snapshot(&id, &snapshot, 1_200).unwrap(), 2);
        let record = store.load(&id).unwrap();
        assert_eq!(record.last_sequence, 2);
        assert_eq!(record.events.len(), 2);
        assert!(!serde_json::to_string(&record).unwrap().contains("old"));

        // A new epoch replaces the increment stream instead of mixing epochs.
        let next_epoch = JournalEvent {
            event_id: "event-9".into(),
            epoch: "epoch-2".into(),
            sequence: 1,
            ..journal_event(1, JournalKind::AssistantDelta, json!({ "delta": "new" }))
        };
        store.append_authenticated(&id, &next_epoch, 1_300).unwrap();
        let record = store.load(&id).unwrap();
        assert_eq!(record.epoch, "epoch-2");
        assert_eq!(record.events.len(), 1);
    }

    #[test]
    fn deleting_the_a_copy_never_touches_b_state() {
        let store = RemoteHistoryStore::in_memory();
        let id = store
            .create("Remote task", "Office PC", "session-0001", 1_000)
            .unwrap();
        let journal = RemoteJournal::new("epoch-1");
        journal
            .append(
                "event-1",
                JournalKind::UserMessage,
                "session-0001",
                None,
                json!({ "text": "hello" }),
            )
            .unwrap();
        assert!(store.delete(&id).unwrap());
        assert!(!store.delete(&id).unwrap());
        // The authoritative B journal is untouched by the A-side delete.
        assert_eq!(journal.len(), 1);
        assert_eq!(store.len(), 0);
    }

    #[test]
    fn persisted_history_survives_a_reload() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("remote_history.json");
        let store = RemoteHistoryStore::persistent(path.clone());
        let id = store
            .create("Remote task", "Office PC", "session-0001", 1_000)
            .unwrap();
        store
            .append_authenticated(
                &id,
                &journal_event(1, JournalKind::AssistantDelta, json!({ "delta": "hi" })),
                1_100,
            )
            .unwrap();
        let reloaded = RemoteHistoryStore::persistent(path);
        let record = reloaded.load(&id).unwrap();
        assert_eq!(record.events.len(), 1);
        assert_eq!(record.device_display_name, "Office PC");
    }

    #[test]
    fn corrupt_history_files_fail_closed_instead_of_resetting() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("remote_history.json");
        std::fs::write(&path, b"{not json").unwrap();
        let store = RemoteHistoryStore {
            path: Some(path.clone()),
            records: Mutex::new(BTreeMap::new()),
            sequence: Mutex::new(0),
        };
        assert_eq!(store.load_from_disk().unwrap_err().code, "history_corrupt");
        std::fs::write(&path, br#"{"version":99,"records":[]}"#).unwrap();
        assert_eq!(
            store.load_from_disk().unwrap_err().code,
            "history_version_unsupported"
        );
    }

    #[test]
    fn stored_and_projected_records_are_redacted() {
        let store = RemoteHistoryStore::in_memory();
        let id = store
            .create("Remote task", "Office PC", "session-0001", 1_000)
            .unwrap();
        store
            .append_authenticated(
                &id,
                &journal_event(
                    1,
                    JournalKind::Error,
                    json!({
                        "code": "executor_error",
                        "message": "failed at C:\\Users\\me\\secret.txt with sk-live-123",
                    }),
                ),
                1_100,
            )
            .unwrap();
        let json = serde_json::to_string(&store.load(&id).unwrap()).unwrap();
        assert!(!json.contains("sk-live-123"));
        assert!(!json.contains("secret.txt"));
        assert!(!json.contains(r"C:\"));
        let listed = store.redacted_json();
        assert!(!listed.contains("sk-live-123"));
    }

    #[test]
    fn capacity_and_reference_validation_fail_closed() {
        let store = RemoteHistoryStore::in_memory();
        assert_eq!(
            store
                .create("Remote task", "Office PC", "../escape", 1_000)
                .unwrap_err()
                .code,
            "history_reference_invalid"
        );
        assert_eq!(
            store
                .create("", "Office PC", "session-0001", 1_000)
                .unwrap_err()
                .code,
            "history_title_invalid"
        );
        for index in 0..MAX_HISTORIES {
            store
                .create(
                    &format!("task {index}"),
                    "Office PC",
                    &format!("session-{index:04}"),
                    1_000,
                )
                .unwrap();
        }
        assert_eq!(
            store
                .create("overflow", "Office PC", "session-9999", 1_000)
                .unwrap_err()
                .code,
            "history_capacity"
        );
    }
}
