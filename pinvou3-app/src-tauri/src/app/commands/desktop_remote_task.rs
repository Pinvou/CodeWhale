//! Windows remote task commands (DEV-13).
//!
//! The command layer is a thin adapter: it re-validates the wire payload
//! against the live lease and the negotiated capabilities, resolves the opaque
//! workspace reference through the DEV-12 snapshot, and hands the request to
//! the B task owner. DEV-20 owns final handler registration.

use std::sync::Arc;
use std::time::Instant;

use serde_json::Value;
use tauri::{AppHandle, Emitter, State};
use tokio_util::sync::CancellationToken;

use crate::features::assistant::engine_pool::EnginePool;
use crate::features::assistant::remote_turn::{EnginePoolChatEngine, EnginePoolChatTurnPort};
use crate::features::codex_acp::AcpPool;
use crate::features::codex_acp::remote_turn::{AcpPoolCodeTurnPort, RemoteWorkspaceAuthorizations};
use crate::features::remote_control::controller::capabilities::CapabilitySnapshotManager;
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
};
use crate::features::remote_control::controller::task::{
    AdmissionOutcome, CancelOutcome, ChatTurnExecutor, CodeTurnExecutor, ControllerTaskEventSink,
    RemoteSessionProvisioner, RemoteTaskEvent, RemoteTaskEventSink, RemoteTaskHooks,
    RemoteTaskLedger, RemoteTaskService, TASK_MODE_CHAT, TASK_MODE_CODE,
};
use crate::features::sessions::SessionStore;

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
const TASK_EVENT: &str = "desktop-remote-task-status";
const TASK_STATUS_EVENT: &str = "desktop-remote-task-event";
const NEGOTIATED_CAPABILITIES: &[&str] = &[
    "pairing_v1",
    "encrypted_frames_v1",
    "finished_receipt_v1",
    "role_liveness_v1",
    "resume_v1",
    "task_v1",
];

fn monotonic_millis() -> u64 {
    use std::sync::OnceLock;
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

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
        let metadata = super::sessions::create_session_record(false, &self.store, &self.pool)?;
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
    ) -> Arc<Self> {
        let service = Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        )));
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

fn live_context(controlled: &ControlledDeviceManager) -> Result<LeaseContext, String> {
    controlled
        .live_lease()
        .map_err(|error| error.code.to_owned())
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

fn validated_task_payload(payload: &Value, lease: &LeaseContext) -> Result<Value, String> {
    let encoded = serde_json::to_string(payload).map_err(|_| "task_request_invalid".to_owned())?;
    let context = RuntimeValidationContext {
        lease: Some(lease.clone()),
        negotiated_capabilities: NEGOTIATED_CAPABILITIES
            .iter()
            .map(|value| (*value).to_owned())
            .collect(),
    };
    let validated =
        validate_controller_runtime_payload_json(&encoded, SenderRole::ControllerA, Some(&context))
            .map_err(|error| format!("task_payload_{}", error.code))?;
    if validated.get("type").and_then(Value::as_str) != Some("task_submit") {
        return Err("task_request_invalid".to_owned());
    }
    Ok(validated)
}

#[tauri::command]
pub fn desktop_remote_task_open_session(
    remote_session_ref: String,
    title: String,
    code_mode: bool,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<String, String> {
    ensure_supported(&controlled)?;
    runtime
        .service()
        .open_remote_session(&remote_session_ref, &title, code_mode)
        .map_err(|error| error.code.to_owned())
}

#[tauri::command]
pub fn desktop_remote_task_switch_session(
    remote_session_ref: String,
    session_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<String, String> {
    ensure_supported(&controlled)?;
    runtime
        .service()
        .switch_remote_session(&remote_session_ref, &session_id)
        .map_err(|error| error.code.to_owned())
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
pub fn desktop_remote_task_submit(
    app: AppHandle,
    payload: Value,
    controlled: State<'_, ControlledDeviceManager>,
    capabilities: State<'_, CapabilitySnapshotManager>,
    runtime: RuntimeState<'_>,
) -> Result<Value, String> {
    ensure_supported(&controlled)?;
    let lease = live_context(&controlled)?;
    let validated = validated_task_payload(&payload, &lease)?;
    let now_b = monotonic_millis();
    let session_id = validated
        .get("session_id")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?
        .to_owned();
    if runtime.service().is_remote_session(&session_id) {
        // The remote session map is the only authority for "this B session is
        // currently owned by a remote controller".
    } else {
        return Err("task_session_not_found".to_owned());
    }
    let mode = validated
        .get("mode")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?
        .to_owned();
    if mode == TASK_MODE_CODE {
        let workspace_id = validated
            .get("input")
            .and_then(|input| input.get("workspace_id"))
            .and_then(Value::as_str)
            .ok_or_else(|| "task_workspace_required".to_owned())?
            .to_owned();
        let revision = validated
            .get("capability_revision")
            .and_then(Value::as_str)
            .ok_or_else(|| "task_request_invalid".to_owned())?;
        // The snapshot is a hint; admitting the task still rechecks the live B
        // policy through the bounded ACP authorization registry below.
        capabilities
            .workspace_for_task(revision, &lease, &workspace_id, now_b)
            .map_err(|error| error.code.to_owned())?;
        if !runtime
            .authorizations()
            .authorize(&session_id, &workspace_id)
        {
            return Err("task_workspace_capacity".to_owned());
        }
    }

    let service = runtime.service();
    let outcome = service
        .submit(&lease, &validated, now_b)
        .map_err(|error| error.code.to_owned())?;
    match outcome {
        AdmissionOutcome::Replay(record) => service
            .status_payload(&record.task_id)
            .map_err(|error| error.code.to_owned()),
        AdmissionOutcome::Admitted(ticket) => {
            let task_id = ticket.record.task_id.clone();
            let status = service
                .status_payload(&task_id)
                .map_err(|error| error.code.to_owned())?;
            let events: Arc<dyn ControllerTaskEventSink> =
                Arc::new(TaskEventBridge { app: app.clone() });
            let dispatch_service = Arc::clone(&service);
            tauri::async_runtime::spawn(async move {
                if let Err(error) = dispatch_service
                    .dispatch(ticket, events, CancellationToken::new(), monotonic_millis())
                    .await
                {
                    eprintln!(
                        "[remote-control] remote task dispatch rejected: {}",
                        error.code
                    );
                }
            });
            let _ = app.emit(TASK_EVENT, &status);
            Ok(status)
        }
    }
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
    let outcome = service
        .cancel(&task_id, monotonic_millis())
        .map_err(|error| error.code.to_owned())?;
    let payload = service
        .status_payload(&task_id)
        .map_err(|error| error.code.to_owned())?;
    if let CancelOutcome::CancellationRequested(_) = outcome {
        let _ = app.emit(
            TASK_STATUS_EVENT,
            serde_json::json!({ "task_id": task_id, "status": "cancel_requested" }),
        );
    }
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
        .status_payload(&task_id)
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
        assert!(NEGOTIATED_CAPABILITIES.contains(&"task_v1"));
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
