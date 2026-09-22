//! Windows remote history and journal commands (DEV-19).
//!
//! A consumes authenticated journal events or a snapshot; only then does the
//! independent history store accept them. Deleting a history entry never
//! touches B's session, and the list projection stays event-free.

use std::sync::Arc;

use serde::Serialize;
use serde_json::Value;
use tauri::{Emitter as _, State};

use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::history::{
    RemoteHistoryRecord, RemoteHistoryStore,
};
use crate::features::remote_control::controller::journal::{
    JournalEvent, JournalSnapshot, RemoteJournal, ReplayOutcome,
};
use crate::features::remote_control::controller::task::{RemoteTaskEvent, RemoteTaskEventSink};
use crate::app::commands::desktop_remote_task::DesktopRemoteTaskRuntime;

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
        // E2E-05：先落权威 journal，再把落库后的事件经加密通道回传 A。
        if let Some(stored) = self.journal.record_task_event(event) {
            publish_authoritative_event(&self.app, &stored);
        }
    }
}

/// 把一条权威 journal 事件回传当前主控端（E2E-05）：会话身份改写回 A 的引用，
/// 用 B 的加密通道封装，经同一代 endpoint lane 发送。未连接、未协商
/// `snapshot_journal_v1`、非远程会话或通道未就位时静默跳过（本地 B 事件照常）。
pub(crate) fn publish_authoritative_event(
    app: &tauri::AppHandle,
    event: &JournalEvent,
) {
    use tauri::Manager as _;
    let device = app.state::<ControlledDeviceManager>();
    let Ok((lease, capabilities)) = device.live_runtime_context() else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "journal_event_not_sent",
            "lease_unavailable",
        );
        return;
    };
    let runtime = app.state::<Arc<DesktopRemoteTaskRuntime>>();
    let Some(session_ref) = runtime.service().remote_ref_for(&event.session_id) else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "journal_event_not_sent",
            "session_mapping_missing",
        );
        return;
    };
    let outbound = app.state::<
        crate::features::remote_control::controller::relay_runtime::ControlledOutboundState,
    >();
    let Some(sender) = outbound.sender() else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "journal_event_not_sent",
            "outbound_unavailable",
        );
        return;
    };
    let Some(payload) =
        crate::features::remote_control::controller::relay_runtime::build_journal_event_payload(
            &lease,
            &session_ref,
            event,
        )
    else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "journal_event_not_sent",
            "payload_not_exportable",
        );
        return;
    };
    let lane = app.state::<
        crate::features::remote_control::controller::encrypted_lane::ControlledLaneState,
    >();
    let Ok(frame) = lane.seal("journal_event", &payload, &capabilities) else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "journal_event_not_sent",
            "seal_failed",
        );
        return;
    };
    match sender.try_send(frame) {
        Ok(()) => crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "journal_event_sent",
            &event.kind,
        ),
        Err(_) => crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "journal_event_not_sent",
            "queue_failed",
        ),
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

/// E2E-05：A 端 journal 落库端口。把解密校验后的权威事件写进对应的本地历史
/// （按会话引用定位；未绑定时只投影事件），并广播给前端渲染正文。
pub(crate) struct DesktopControllerJournalSink {
    app: tauri::AppHandle,
    runtime: Arc<DesktopRemoteHistoryRuntime>,
}

impl DesktopControllerJournalSink {
    pub(crate) fn new(
        app: tauri::AppHandle,
        runtime: Arc<DesktopRemoteHistoryRuntime>,
    ) -> Self {
        Self { app, runtime }
    }
}

impl crate::features::remote_control::controller::controlling_service::ControllingJournalSink
    for DesktopControllerJournalSink
{
    fn publish(&self, event: &JournalEvent) {
        let history_id = self
            .runtime
            .store()
            .find_by_session_reference(&event.session_id);
        if let Some(history_id) = &history_id {
            let _ = self.runtime.store().append_authenticated(
                history_id,
                event,
                monotonic_millis(),
            );
        }
        let _ = self.app.emit(
            crate::features::remote_control::controller::controlling_service::CONTROLLER_JOURNAL_EVENT,
            serde_json::json!({
                "historyId": history_id,
                "event": event,
            }),
        );
    }

    /// E2E-05 缺口恢复：把权威快照原子应用到对应历史（未绑定历史时只广播）。
    fn publish_snapshot(
        &self,
        snapshot: &crate::features::remote_control::controller::journal::JournalSnapshot,
    ) {
        let session_ref = snapshot
            .events
            .first()
            .map(|event| event.session_id.clone());
        let history_id = session_ref
            .as_deref()
            .and_then(|session| self.runtime.store().find_by_session_reference(session));
        if let Some(history_id) = &history_id {
            let _ = self.runtime.store().apply_snapshot(
                history_id,
                snapshot,
                monotonic_millis(),
            );
        }
        let _ = self.app.emit(
            crate::features::remote_control::controller::controlling_service::CONTROLLER_JOURNAL_EVENT,
            serde_json::json!({
                "historyId": history_id,
                "snapshot": snapshot,
            }),
        );
    }
}

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
