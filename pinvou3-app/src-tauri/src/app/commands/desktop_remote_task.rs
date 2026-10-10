//! B-local remote task status and stop commands, plus private task owners.
//!
//! A07 deliberately exposes no renderer task/session admission. A08 must
//! authenticate the sender and full binding before connecting the private
//! engine/session kernels; a renderer-supplied payload cannot assert A's role.

use std::sync::Arc;

use super::desktop_remote_state::DesktopRemoteState as State;
use serde_json::Value;
use tauri::{AppHandle, Emitter};

use crate::features::assistant::engine_pool::EnginePool;
use crate::features::assistant::remote_turn::{EnginePoolChatEngine, EnginePoolChatTurnPort};
use crate::features::codex_acp::AcpPool;
use crate::features::codex_acp::remote_turn::{AcpPoolCodeTurnPort, RemoteWorkspaceAuthorizations};
pub(crate) use crate::features::remote_control::controller::clock::monotonic_millis;
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::task::{
    ChatTurnExecutor, CodeTurnExecutor, ControllerTaskEventSink, RemoteSessionProvisioner,
    RemoteTaskEvent, RemoteTaskEventSink, RemoteTaskHooks, RemoteTaskLedger, RemoteTaskService,
    TASK_MODE_CHAT, TASK_MODE_CODE,
};
use crate::features::sessions::SessionStore;

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
const TASK_EVENT: &str = "desktop-remote-task-status";
const TASK_STATUS_EVENT: &str = "desktop-remote-task-event";

/// Creates B-authoritative sessions for remote tasks. Only the local session
/// identity, title and mode lane are persisted; nothing about A crosses this
/// boundary.
pub(crate) struct DesktopRemoteSessionProvisioner {
    store: SessionStore,
    pool: EnginePool,
}

impl DesktopRemoteSessionProvisioner {
    pub(crate) fn new(store: SessionStore, pool: EnginePool) -> Self {
        Self { store, pool }
    }
}

impl RemoteSessionProvisioner for DesktopRemoteSessionProvisioner {
    fn create_authoritative_session(
        &self,
        title: &str,
        code_mode: bool,
    ) -> std::result::Result<String, String> {
        let metadata =
            super::sessions::create_session_record(false, &self.store, &self.pool, None)?;
        let session_id = metadata.id.clone();
        let bounded: String = title.trim().chars().take(120).collect();
        if !bounded.is_empty() {
            self.store
                .set_title(&session_id, bounded)
                .map_err(|_| "task_session_create_failed".to_owned())?;
        }
        if code_mode {
            self.store
                .set_mode(&session_id, crate::core::mode_state::SerializableMode::Yolo)
                .map_err(|_| "task_session_create_failed".to_owned())?;
        }
        Ok(session_id)
    }

    fn session_exists(&self, session_id: &str) -> std::result::Result<bool, String> {
        match self.store.session_kind(session_id) {
            Ok(crate::features::sessions::SessionKind::ScheduledRun) => Ok(false),
            Ok(crate::features::sessions::SessionKind::Chat) => Ok(true),
            Err(_) => Ok(false),
        }
    }
}

struct TaskEventBridge {
    app: AppHandle,
}

impl ControllerTaskEventSink for TaskEventBridge {
    fn emit(&self, event: Value) {
        let _ = self.app.emit(TASK_STATUS_EVENT, event);
    }
}

impl RemoteTaskEventSink for TaskEventBridge {
    fn publish_task_event(&self, event: &RemoteTaskEvent) {
        let _ = self.app.emit(TASK_STATUS_EVENT, event);
    }
}

/// Managed state for the remote task owner: ledger, session map, engine ports
/// and the bounded workspace authorization registry.
pub(crate) struct DesktopRemoteTaskRuntime {
    service: Arc<RemoteTaskService>,
    authorizations: Arc<RemoteWorkspaceAuthorizations>,
}

impl DesktopRemoteTaskRuntime {
    pub(crate) fn new(
        app: AppHandle,
        store: SessionStore,
        pool: EnginePool,
        acp_pool: AcpPool,
        ledger: Arc<RemoteTaskLedger>,
    ) -> Arc<Self> {
        let service = Arc::new(RemoteTaskService::new(ledger));
        let authorizations = Arc::new(RemoteWorkspaceAuthorizations::default());
        service.set_session_provisioner(Arc::new(DesktopRemoteSessionProvisioner::new(
            store,
            pool.clone(),
        )));
        service.register_executor(
            TASK_MODE_CHAT,
            Arc::new(ChatTurnExecutor::new(Arc::new(
                EnginePoolChatTurnPort::new(Arc::new(EnginePoolChatEngine::new(Arc::new(pool)))),
            ))),
        );
        service.register_executor(
            TASK_MODE_CODE,
            Arc::new(CodeTurnExecutor::new(Arc::new(AcpPoolCodeTurnPort::new(
                Arc::new(acp_pool),
                Arc::clone(&authorizations),
            )))),
        );
        service
            .hooks()
            .set_journal(Arc::new(TaskEventBridge { app }));
        Arc::new(Self {
            service,
            authorizations,
        })
    }

    pub(crate) fn service(&self) -> Arc<RemoteTaskService> {
        Arc::clone(&self.service)
    }

    pub(crate) fn ledger(&self) -> Arc<RemoteTaskLedger> {
        self.service.ledger()
    }

    pub(crate) fn authorizations(&self) -> Arc<RemoteWorkspaceAuthorizations> {
        Arc::clone(&self.authorizations)
    }

    /// Extension scopes for DEV-15/16/17/18 are registered here.
    pub(crate) fn hooks(&self) -> &RemoteTaskHooks {
        self.service.hooks()
    }

    pub(crate) fn recover_after_restart(&self) -> usize {
        self.service.recover_after_restart(monotonic_millis()).len()
    }
}

type RuntimeState<'a> = State<'a, Arc<DesktopRemoteTaskRuntime>>;

fn ensure_supported(controlled: &ControlledDeviceManager) -> Result<(), String> {
    if controlled.is_supported() {
        Ok(())
    } else {
        Err(UNSUPPORTED.to_owned())
    }
}

fn sanitized_reason(reason: &str) -> String {
    let sanitized: String = reason
        .chars()
        .filter(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-'))
        .take(64)
        .collect();
    if sanitized.is_empty() {
        "unspecified".to_owned()
    } else {
        sanitized
    }
}

#[tauri::command]
pub fn desktop_remote_task_close_session(
    remote_session_ref: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<Option<String>, String> {
    ensure_supported(&controlled)?;
    runtime
        .service()
        .close_remote_session(&remote_session_ref)
        .map_err(|error| error.code.to_owned())
}

#[tauri::command]
pub fn desktop_remote_task_cancel(
    app: AppHandle,
    task_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<Value, String> {
    ensure_supported(&controlled)?;
    let service = runtime.service();
    service
        .cancel(&task_id, monotonic_millis())
        .map_err(|error| error.code.to_owned())?;
    let payload = service
        .status_view_payload(&task_id)
        .map_err(|error| error.code.to_owned())?;
    let _ = app.emit(TASK_EVENT, &payload);
    Ok(payload)
}

#[tauri::command]
pub fn desktop_remote_task_status(
    task_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<Value, String> {
    ensure_supported(&controlled)?;
    runtime
        .service()
        .status_view_payload(&task_id)
        .map_err(|error| error.code.to_owned())
}

#[tauri::command]
pub fn desktop_remote_task_interrupt_all(
    reason: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<Vec<String>, String> {
    ensure_supported(&controlled)?;
    let reason = sanitized_reason(&reason);
    let records = runtime.service().interrupt_all(&reason, monotonic_millis());
    Ok(records.into_iter().map(|record| record.task_id).collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn command_surface_names_are_stable() {
        assert_eq!(UNSUPPORTED, "desktop_remote_control_unsupported");
        assert_eq!(TASK_EVENT, "desktop-remote-task-status");
        assert_eq!(TASK_STATUS_EVENT, "desktop-remote-task-event");
    }

    #[test]
    fn task_event_serializes_only_the_bounded_projection() {
        let event = RemoteTaskEvent {
            task_id: "task-0001".into(),
            session_id: "session-0001".into(),
            status: "succeeded".into(),
            sequence: "1".into(),
            outcome_unknown: false,
            terminal_reason: Some("completed".into()),
            result_digest: None,
        };
        let encoded = serde_json::to_string(&event).unwrap();
        assert!(!encoded.contains(r"C:\"));
        assert!(!encoded.contains("api_key"));
        assert!(encoded.contains("task-0001"));
    }

    #[test]
    fn interrupt_reasons_are_sanitized_and_bounded() {
        assert_eq!(sanitized_reason("stop now!"), "stopnow");
        assert_eq!(sanitized_reason(""), "unspecified");
        assert!(sanitized_reason(&"a".repeat(200)).len() <= 64);
    }

    #[test]
    fn monotonic_clock_never_moves_backwards() {
        let first = monotonic_millis();
        let second = monotonic_millis();
        assert!(second >= first);
    }
}
