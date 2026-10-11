//! Windows remote history and journal commands (DEV-19).
//!
//! The journal is published by the actual task owner. A07 has no renderer
//! persistent/raw B-history write ingress: authenticated history projection
//! is an A08 boundary. A-local controller history has a separate adapter.

use std::sync::Arc;

use super::desktop_remote_state::DesktopRemoteState as State;
use serde::Serialize;

use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::history::RemoteHistoryStore;
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
    ) -> Result<Arc<Self>, String> {
        Ok(Self::new(
            Arc::new(RemoteHistoryStore::persistent(path).map_err(|error| error.code.to_owned())?),
            Arc::new(RemoteJournal::new(epoch)),
        ))
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

    #[test]
    fn production_runtime_constructor_propagates_history_load_failure() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("remote_history.json");
        let bytes = b"{invalid fixture";
        std::fs::write(&path, bytes).unwrap();
        assert_eq!(
            DesktopRemoteHistoryRuntime::with_persistent_history(path.clone(), "epoch-1")
                .err()
                .unwrap(),
            "history_corrupt"
        );
        assert_eq!(std::fs::read(path).unwrap(), bytes);
    }

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
            .append_authenticated(&id, &runtime.journal().snapshot().events[0], 1_100)
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
