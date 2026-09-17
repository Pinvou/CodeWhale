//! Windows remote history and journal commands (DEV-19).
//!
//! A consumes authenticated journal events or a snapshot; only then does the
//! independent history store accept them. Deleting a history entry never
//! touches B's session, and the list projection stays event-free.

use std::sync::Arc;

use serde::Serialize;
use serde_json::Value;
use tauri::State;

use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::history::{
    RemoteHistoryRecord, RemoteHistoryStore,
};
use crate::features::remote_control::controller::journal::{
    JournalEvent, JournalSnapshot, RemoteJournal, ReplayOutcome,
};
use crate::features::remote_control::controller::task::{RemoteTaskEvent, RemoteTaskEventSink};

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

#[derive(Debug, Clone, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct DesktopRemoteJournalReplay {
    kind: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    events: Option<Vec<JournalEvent>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    snapshot: Option<JournalSnapshot>,
}

/// Managed state for the A-side history plus the current journal.
pub(crate) struct DesktopRemoteHistoryRuntime {
    store: Arc<RemoteHistoryStore>,
    journal: Arc<RemoteJournal>,
}

/// The task owner's terminal/status events go to both the frontend (live UI)
/// and the authoritative journal this runtime owns.
pub(crate) struct DesktopTaskEventSink {
    app: tauri::AppHandle,
    journal: Arc<RemoteJournal>,
}

impl DesktopTaskEventSink {
    pub(crate) fn new(app: tauri::AppHandle, journal: Arc<RemoteJournal>) -> Self {
        Self { app, journal }
    }
}

const TASK_STATUS_EVENT: &str = "desktop-remote-task-event";

impl RemoteTaskEventSink for DesktopTaskEventSink {
    fn publish_task_event(&self, event: &RemoteTaskEvent) {
        use tauri::Emitter as _;
        let _ = self.app.emit(TASK_STATUS_EVENT, event);
        RemoteTaskEventSink::publish_task_event(self.journal.as_ref(), event);
    }
}

impl DesktopRemoteHistoryRuntime {
    pub(crate) fn new(store: Arc<RemoteHistoryStore>, journal: Arc<RemoteJournal>) -> Arc<Self> {
        Arc::new(Self { store, journal })
    }

    pub(crate) fn with_persistent_history(
        path: std::path::PathBuf,
        epoch: impl Into<String>,
    ) -> Arc<Self> {
        Self::new(
            Arc::new(RemoteHistoryStore::persistent(path)),
            Arc::new(RemoteJournal::new(epoch)),
        )
    }

    pub(crate) fn store(&self) -> &Arc<RemoteHistoryStore> {
        &self.store
    }

    pub(crate) fn journal(&self) -> &Arc<RemoteJournal> {
        &self.journal
    }
}

type HistoryState<'a> = State<'a, Arc<DesktopRemoteHistoryRuntime>>;

fn ensure_supported(controlled: &ControlledDeviceManager) -> Result<(), String> {
    if controlled.is_supported() {
        Ok(())
    } else {
        Err(UNSUPPORTED.to_owned())
    }
}

fn monotonic_millis() -> u64 {
    use std::sync::OnceLock;
    use std::time::Instant;
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

#[tauri::command]
pub fn desktop_remote_history_list(
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<Vec<RemoteHistoryRecord>, String> {
    ensure_supported(&controlled)?;
    Ok(runtime.store().list())
}

#[tauri::command]
pub fn desktop_remote_history_load(
    history_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<RemoteHistoryRecord, String> {
    ensure_supported(&controlled)?;
    runtime
        .store()
        .load(&history_id)
        .map_err(|error| error.code.to_owned())
}

#[tauri::command]
pub fn desktop_remote_history_delete(
    history_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<bool, String> {
    ensure_supported(&controlled)?;
    runtime
        .store()
        .delete(&history_id)
        .map_err(|error| error.code.to_owned())
}

#[tauri::command]
pub fn desktop_remote_history_create(
    title: String,
    device_display_name: String,
    session_ref: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<String, String> {
    ensure_supported(&controlled)?;
    runtime
        .store()
        .create(&title, &device_display_name, &session_ref, monotonic_millis())
        .map_err(|error| error.code.to_owned())
}

#[tauri::command]
pub fn desktop_remote_history_mark_offline(
    history_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<RemoteHistoryRecord, String> {
    ensure_supported(&controlled)?;
    runtime
        .store()
        .mark_offline(&history_id, monotonic_millis())
        .map_err(|error| error.code.to_owned())?;
    runtime
        .store()
        .load(&history_id)
        .map_err(|error| error.code.to_owned())
}

/// Applies one authenticated event to the A copy.
#[tauri::command]
pub fn desktop_remote_history_apply_event(
    history_id: String,
    event: Value,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<bool, String> {
    ensure_supported(&controlled)?;
    let event: JournalEvent =
        serde_json::from_value(event).map_err(|_| "history_event_invalid".to_owned())?;
    runtime
        .store()
        .append_authenticated(&history_id, &event, monotonic_millis())
        .map_err(|error| error.code.to_owned())
}

/// Applies an authoritative snapshot atomically.
#[tauri::command]
pub fn desktop_remote_history_apply_snapshot(
    history_id: String,
    snapshot: Value,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<usize, String> {
    ensure_supported(&controlled)?;
    let snapshot: JournalSnapshot =
        serde_json::from_value(snapshot).map_err(|_| "history_snapshot_invalid".to_owned())?;
    runtime
        .store()
        .apply_snapshot(&history_id, &snapshot, monotonic_millis())
        .map_err(|error| error.code.to_owned())
}

#[tauri::command]
pub fn desktop_remote_journal_replay(
    epoch: String,
    sequence: u64,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<DesktopRemoteJournalReplay, String> {
    ensure_supported(&controlled)?;
    Ok(match runtime.journal().replay_after(&epoch, sequence) {
        ReplayOutcome::Events(events) => DesktopRemoteJournalReplay {
            kind: "events".into(),
            events: Some(events),
            snapshot: None,
        },
        ReplayOutcome::Gap(snapshot) => DesktopRemoteJournalReplay {
            kind: "snapshot".into(),
            events: None,
            snapshot: Some(snapshot),
        },
    })
}

#[tauri::command]
pub fn desktop_remote_journal_snapshot(
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<JournalSnapshot, String> {
    ensure_supported(&controlled)?;
    Ok(runtime.journal().snapshot())
}

#[tauri::command]
pub fn desktop_remote_journal_acknowledge(
    sequence: u64,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: HistoryState<'_>,
) -> Result<u64, String> {
    ensure_supported(&controlled)?;
    runtime.journal().acknowledge(sequence);
    Ok(runtime.journal().acknowledged())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::history::RemoteHistoryState;
    use crate::features::remote_control::controller::journal::JournalKind;
    use serde_json::json;

    fn runtime() -> Arc<DesktopRemoteHistoryRuntime> {
        DesktopRemoteHistoryRuntime::new(
            Arc::new(RemoteHistoryStore::in_memory()),
            Arc::new(RemoteJournal::new("epoch-1")),
        )
    }

    #[test]
    fn list_projection_is_offline_safe_and_event_free() {
        let runtime = runtime();
        let id = runtime
            .store()
            .create("Remote task", "Office PC", "session-0001", 1_000)
            .unwrap();
        runtime
            .journal()
            .append(
                "event-1",
                JournalKind::UserMessage,
                "session-0001",
                None,
                json!({ "text": "hello" }),
            )
            .unwrap();
        runtime
            .store()
            .append_authenticated(
                &id,
                &runtime.journal().snapshot().events[0],
                1_100,
            )
            .unwrap();
        runtime.store().mark_offline(&id, 1_200).unwrap();
        let listed = runtime.store().list();
        assert_eq!(listed.len(), 1);
        assert!(listed[0].events.is_empty());
        assert_eq!(listed[0].state, RemoteHistoryState::OfflineCopy);
        let loaded = runtime.store().load(&id).unwrap();
        assert_eq!(loaded.events.len(), 1);
    }

    #[test]
    fn replay_gap_returns_a_snapshot_instead_of_a_partial_prefix() {
        let runtime = runtime();
        runtime
            .journal()
            .append(
                "event-1",
                JournalKind::AssistantDelta,
                "session-0001",
                None,
                json!({ "delta": "hello" }),
            )
            .unwrap();
        let replay = match runtime.journal().replay_after("epoch-2", 0) {
            ReplayOutcome::Gap(snapshot) => snapshot,
            ReplayOutcome::Events(_) => panic!("a foreign epoch must produce a snapshot"),
        };
        assert_eq!(replay.epoch, "epoch-1");
        assert_eq!(replay.sequence, 1);
    }

    #[test]
    fn deleting_the_local_copy_leaves_the_journal_untouched() {
        let runtime = runtime();
        let id = runtime
            .store()
            .create("Remote task", "Office PC", "session-0001", 1_000)
            .unwrap();
        assert!(runtime.store().delete(&id).unwrap());
        assert_eq!(runtime.journal().len(), 0);
        assert!(runtime.store().list().is_empty());
    }
}
