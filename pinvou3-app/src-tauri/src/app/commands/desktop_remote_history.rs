//! Windows remote history and journal commands (DEV-19).
//!
//! A consumes authenticated journal events or a snapshot; only then does the
//! independent history store accept them. Deleting a history entry never
//! touches B's session, and the list projection stays event-free.

use std::sync::Arc;

use serde::Serialize;
use serde_json::Value;
use tauri::State;

use crate::app::commands::desktop_remote_task::DesktopRemoteTaskRuntime;
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::history::{
    RemoteHistoryRecord, RemoteHistoryStore,
};
#[cfg(test)]
use crate::features::remote_control::controller::journal::ReplayOutcome;
use crate::features::remote_control::controller::journal::{
    JournalEvent, JournalSnapshot, RemoteJournal,
};
use crate::features::remote_control::controller::protocol::LeaseContext;
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

/// Send every B-side lifecycle transition to A, including tasks submitted
/// locally from B. The initial A-originated admitted response may duplicate
/// this frame; A deduplicates by `(task_id, status)`.
pub(crate) fn publish_authoritative_task_status(app: &tauri::AppHandle, event: &RemoteTaskEvent) {
    use tauri::Manager as _;
    let device = app.state::<ControlledDeviceManager>();
    let Ok((lease, capabilities)) = device.live_runtime_context() else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "task_status_not_sent",
            "lease_unavailable",
        );
        return;
    };
    let runtime = app.state::<Arc<DesktopRemoteTaskRuntime>>();
    let Some(session_ref) = runtime.service().remote_ref_for(&event.session_id) else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "task_status_not_sent",
            "session_mapping_missing",
        );
        return;
    };
    if runtime
        .service()
        .session_for(&session_ref, &lease.remote_control_generation)
        .as_deref()
        != Some(event.session_id.as_str())
    {
        return;
    }
    let Ok(mut payload) = runtime.service().status_payload(&event.task_id) else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "task_status_not_sent",
            "status_unavailable",
        );
        return;
    };
    let Some(object) = payload.as_object_mut() else {
        return;
    };
    object.insert("session_id".to_owned(), Value::String(session_ref));
    let outbound = app.state::<
        crate::features::remote_control::controller::relay_runtime::ControlledOutboundState,
    >();
    let Some(sender) = outbound.sender() else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "task_status_not_sent",
            "outbound_unavailable",
        );
        return;
    };
    let lane = app
        .state::<crate::features::remote_control::controller::encrypted_lane::ControlledLaneState>(
        );
    let Ok(frame) = lane.seal("task_status", &payload, &capabilities) else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "task_status_not_sent",
            "seal_failed",
        );
        return;
    };
    let result = sender.try_send(frame);
    crate::features::remote_control::controller::relay_runtime::log_controlled_event(
        if result.is_ok() { "task_status_sent" } else { "task_status_not_sent" },
        if result.is_ok() { event.status.as_str() } else { "queue_failed" },
    );
}

impl RemoteTaskEventSink for DesktopTaskEventSink {
    fn publish_task_event(&self, event: &RemoteTaskEvent) {
        use tauri::Emitter as _;
        let _ = self.app.emit(TASK_STATUS_EVENT, event);
        publish_authoritative_task_status(&self.app, event);
        // Only the current authenticated B session/generation can allocate a
        // visible journal sequence. Ordinary local sessions remain local.
        use tauri::Manager as _;
        let Some((lease, _)) = self
            .app
            .state::<ControlledDeviceManager>()
            .live_runtime_context()
            .ok()
        else {
            return;
        };
        let runtime = self.app.state::<Arc<DesktopRemoteTaskRuntime>>();
        let Some(remote_ref) = runtime.service().remote_ref_for(&event.session_id) else {
            return;
        };
        if runtime
            .service()
            .session_for(&remote_ref, &lease.remote_control_generation)
            .as_deref()
            != Some(event.session_id.as_str())
        {
            return;
        }
        let Ok(record) = runtime.service().status(&event.task_id) else {
            return;
        };
        if record.lease.remote_control_generation != lease.remote_control_generation {
            return;
        }
        if self
            .journal
            .bind_generation(&lease.remote_control_generation)
            .is_err()
        {
            return;
        }
        // E2E-05：先落 B 权威 journal，再经同代加密通道回传 A。
        if let Some(stored) = self
            .journal
            .record_task_event_for_generation(&lease.remote_control_generation, event)
        {
            publish_authoritative_event(&self.app, &lease.remote_control_generation, &stored);
        }
    }
}

/// 把一条权威 journal 事件回传当前主控端（E2E-05）：会话身份改写回 A 的引用，
/// 用 B 的加密通道封装，经同一代 endpoint lane 发送。未连接、未协商
/// `snapshot_journal_v1`、非远程会话或通道未就位时静默跳过（本地 B 事件照常）。
fn publish_if_current<T>(
    journal: &RemoteJournal,
    appended_generation: &str,
    lease: &LeaseContext,
    event: &JournalEvent,
    mapped_session: Option<&str>,
    send: impl FnOnce() -> T,
) -> Option<T> {
    if lease.remote_control_generation != appended_generation
        || mapped_session != Some(event.session_id.as_str())
        || journal
            .require_current_event_for_generation(appended_generation, event)
            .is_err()
    {
        return None;
    }
    Some(send())
}

pub(crate) fn publish_authoritative_event(
    app: &tauri::AppHandle,
    appended_generation: &str,
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
    let mapped_session = runtime
        .service()
        .session_for(&session_ref, appended_generation);
    let history = app.state::<Arc<DesktopRemoteHistoryRuntime>>();
    if publish_if_current(
        history.journal(),
        appended_generation,
        &lease,
        event,
        mapped_session.as_deref(),
        || {
            publish_current_authoritative_event(app, &lease, &capabilities, &session_ref, event);
        },
    )
    .is_none() {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "journal_event_not_sent",
            "generation_or_binding_stale",
        );
        return;
    }
}

fn publish_current_authoritative_event(
    app: &tauri::AppHandle,
    lease: &LeaseContext,
    capabilities: &[String],
    session_ref: &str,
    event: &JournalEvent,
) {
    use tauri::Manager as _;
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
    if sender.generation() != lease.remote_control_generation {
        return;
    }
    let Some(payload) =
        crate::features::remote_control::controller::relay_runtime::build_journal_event_payload(
            lease,
            session_ref,
            event,
        )
    else {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "journal_event_not_sent",
            "payload_not_exportable",
        );
        return;
    };
    let lane = app
        .state::<crate::features::remote_control::controller::encrypted_lane::ControlledLaneState>(
        );
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
pub(crate) struct DesktopControllerJournalSink {}

impl DesktopControllerJournalSink {
    pub(crate) fn new(_app: tauri::AppHandle, _runtime: Arc<DesktopRemoteHistoryRuntime>) -> Self {
        Self {}
    }
}

impl crate::features::remote_control::controller::controlling_service::ControllingJournalSink
    for DesktopControllerJournalSink
{
    fn publish(&self, _event: &JournalEvent) -> Result<(), String> {
        // This legacy history store is an offline compatibility copy, not the
        // new authenticated RemoteProxy authority. DEV-20 replaces this sink
        // with the DEV-30 projection adapter and durable cursor confirmation.
        Err("remote_projection_not_configured".to_owned())
    }

    /// E2E-05 缺口恢复：把权威快照原子应用到对应历史（未绑定历史时只广播）。
    fn publish_snapshot(
        &self,
        _snapshot: &crate::features::remote_control::controller::journal::JournalSnapshot,
    ) -> Result<(), String> {
        Err("remote_projection_not_configured".to_owned())
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
        .create(
            &title,
            &device_display_name,
            &session_ref,
            monotonic_millis(),
        )
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
    _history_id: String,
    _event: Value,
    controlled: State<'_, ControlledDeviceManager>,
    _runtime: HistoryState<'_>,
) -> Result<bool, String> {
    ensure_supported(&controlled)?;
    Err("history_authentication_required".to_owned())
}

/// Applies an authoritative snapshot atomically.
#[tauri::command]
pub fn desktop_remote_history_apply_snapshot(
    _history_id: String,
    _snapshot: Value,
    controlled: State<'_, ControlledDeviceManager>,
    _runtime: HistoryState<'_>,
) -> Result<usize, String> {
    ensure_supported(&controlled)?;
    Err("history_authentication_required".to_owned())
}

#[tauri::command]
pub fn desktop_remote_journal_replay(
    _epoch: String,
    _sequence: u64,
    controlled: State<'_, ControlledDeviceManager>,
    _runtime: HistoryState<'_>,
) -> Result<DesktopRemoteJournalReplay, String> {
    ensure_supported(&controlled)?;
    // Compatibility command has no authenticated session scope. It must not
    // select a journal implicitly; DEV-20 will expose a scoped bridge.
    Err("journal_scope_required".to_owned())
}

#[tauri::command]
pub fn desktop_remote_journal_snapshot(
    controlled: State<'_, ControlledDeviceManager>,
    _runtime: HistoryState<'_>,
) -> Result<JournalSnapshot, String> {
    ensure_supported(&controlled)?;
    Err("journal_scope_required".to_owned())
}

#[tauri::command]
pub fn desktop_remote_journal_acknowledge(
    _sequence: u64,
    controlled: State<'_, ControlledDeviceManager>,
    _runtime: HistoryState<'_>,
) -> Result<u64, String> {
    ensure_supported(&controlled)?;
    Err("journal_scope_required".to_owned())
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
    fn old_generation_publish_cannot_send_after_same_b_session_rebind() {
        use std::cell::Cell;

        let journal = RemoteJournal::new("boot");
        journal.bind_generation("generation-1").unwrap();
        let old = journal
            .append_for_generation(
                "generation-1",
                "event-old",
                JournalKind::UserMessage,
                "b-session",
                None,
                json!({"text":"old"}),
            )
            .unwrap();
        let lease = |generation: &str| LeaseContext {
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-1".into(),
            remote_control_generation: generation.into(),
        };
        let sent = Cell::new(0);
        let old_owner = lease("generation-1");
        journal.bind_generation("generation-2").unwrap();
        let new_owner = lease("generation-2");
        // The new ref maps to the same B session. Neither a captured old
        // lease nor reinterpreting the old event under the new lease may send.
        assert!(publish_if_current(
            &journal, "generation-1", &new_owner, &old, Some("b-session"),
            || sent.set(sent.get() + 1),
        ).is_none());
        assert!(publish_if_current(
            &journal, "generation-1", &old_owner, &old, Some("b-session"),
            || sent.set(sent.get() + 1),
        ).is_none());
        assert_eq!(sent.get(), 0);

        let current = journal
            .append_for_generation(
                "generation-2", "event-current", JournalKind::UserMessage,
                "b-session", None, json!({"text":"new"}),
            )
            .unwrap();
        assert!(publish_if_current(
            &journal, "generation-2", &new_owner, &current, Some("b-session"),
            || sent.set(sent.get() + 1),
        ).is_some());
        assert_eq!(sent.get(), 1);
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
