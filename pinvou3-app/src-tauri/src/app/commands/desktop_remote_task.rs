//! Windows remote task commands (DEV-13).
//!
//! The command layer is a thin adapter: it re-validates the wire payload
//! against the live lease and the negotiated capabilities, resolves the opaque
//! workspace reference through the DEV-12 snapshot, and hands the request to
//! the B task owner. DEV-20 owns final handler registration.

use std::sync::Arc;
use std::time::Instant;

use serde_json::Value;
use tauri::{AppHandle, Emitter, Manager as _, State};
use tokio_util::sync::CancellationToken;

use crate::features::assistant::engine_pool::EnginePool;
use crate::features::assistant::remote_turn::{
    EnginePoolChatEngine, EnginePoolChatTurnPort, StoreChatTranscript,
};
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
    TASK_MODEL_SOURCE_TEMPORARY,
};
use crate::features::remote_control::controller::temporary_model::SharedTemporaryModelStore;
use crate::features::sessions::SessionStore;

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
const TASK_EVENT: &str = "desktop-remote-task-status";
const TASK_STATUS_EVENT: &str = "desktop-remote-task-event";
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

    /// E2E-05：执行器产出的正文事件先写权威 journal，再经加密通道回传 A。
    fn emit_journal(
        &self,
        kind: crate::features::remote_control::controller::journal::JournalKind,
        session_id: &str,
        task_id: Option<&str>,
        payload: Value,
    ) {
        use tauri::Manager as _;
        let history = self
            .app
            .state::<Arc<super::desktop_remote_history::DesktopRemoteHistoryRuntime>>();
        let event_id = format!("journal-{}", uuid::Uuid::new_v4().simple());
        if let Ok(event) = history
            .journal()
            .append(&event_id, kind, session_id, task_id, payload)
        {
            super::desktop_remote_history::publish_authoritative_event(&self.app, &event);
        }
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
            store.clone(),
            pool.clone(),
        )));
        service.register_executor(
            TASK_MODE_CHAT,
            Arc::new(ChatTurnExecutor::new(Arc::new(
                EnginePoolChatTurnPort::new(
                    Arc::new(EnginePoolChatEngine::new(Arc::new(pool))),
                    Arc::new(StoreChatTranscript::new(store)),
                ),
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

/// DEV-15 审核修复：A 选择「我的模型」时，任务提交前必须存在与之绑定的
/// 临时模型包；否则失败关闭，绝不回退到 B 的本机模型（RC-M-009）。
fn temporary_model_gate(
    model_source: &str,
    task_id: &str,
    store: &crate::features::remote_control::controller::temporary_model::TemporaryModelStore,
) -> Result<(), String> {
    if model_source != TASK_MODEL_SOURCE_TEMPORARY {
        return Ok(());
    }
    match store.active_task() {
        Some(active) if active == task_id => Ok(()),
        _ => Err("model_package_missing".to_owned()),
    }
}

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

fn validated_task_payload(
    payload: &Value,
    lease: &LeaseContext,
    negotiated_capabilities: &[String],
) -> Result<Value, String> {
    let encoded = serde_json::to_string(payload).map_err(|_| "task_request_invalid".to_owned())?;
    let context = RuntimeValidationContext {
        lease: Some(lease.clone()),
        negotiated_capabilities: negotiated_capabilities.to_vec(),
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
    models: State<'_, SharedTemporaryModelStore>,
    runtime: RuntimeState<'_>,
) -> Result<Value, String> {
    ensure_supported(&controlled)?;
    let (lease, negotiated_capabilities) = controlled
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    let validated = validated_task_payload(&payload, &lease, &negotiated_capabilities)?;
    let session_id = validated
        .get("session_id")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?
        .to_owned();
    // 命令入口只接受已经本地绑定过的远程会话（权威映射存在与否由服务判断）。
    if !runtime.service().is_remote_session(&session_id) {
        return Err("task_session_not_found".to_owned());
    }
    admit_and_dispatch(
        &app,
        &validated,
        &lease,
        &session_id,
        &capabilities,
        &models,
        runtime.service(),
        monotonic_millis(),
    )
}

/// wire 入口（E2E-04）：A 的会话引用首次出现时创建 B 权威会话并建立映射，
/// 随后与命令入口共用同一套接纳/派发逻辑。
pub(crate) fn submit_decrypted_task(
    app: &AppHandle,
    session_ref: &str,
    payload: &Value,
) -> Result<Value, String> {
    let controlled = app.state::<ControlledDeviceManager>();
    ensure_supported(&controlled)?;
    let (lease, negotiated_capabilities) = controlled
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    let validated = validated_task_payload(payload, &lease, &negotiated_capabilities)?;
    let mode = validated
        .get("mode")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?
        .to_owned();
    let runtime = app.state::<Arc<DesktopRemoteTaskRuntime>>();
    let service = runtime.service();
    let session_id = if service.is_remote_session(session_ref) {
        session_ref.to_owned()
    } else {
        service
            .open_remote_session(session_ref, "", mode == TASK_MODE_CODE)
            .map_err(|error| error.code.to_owned())?
    };
    let mut request = validated;
    if let Some(object) = request.as_object_mut() {
        object.insert("session_id".to_owned(), Value::String(session_id.clone()));
    }
    let capabilities = app.state::<CapabilitySnapshotManager>();
    let models = app.state::<SharedTemporaryModelStore>();
    admit_and_dispatch(
        app,
        &request,
        &lease,
        &session_id,
        &capabilities,
        &models,
        service,
        monotonic_millis(),
    )
}

/// 接纳并派发一条已通过冻结校验、且会话已在远程映射中的 `task_submit`。
/// 命令入口与 wire 入口共用，避免两条实现漂移。
#[allow(clippy::too_many_arguments)]
fn admit_and_dispatch(
    app: &AppHandle,
    validated: &Value,
    lease: &LeaseContext,
    session_id: &str,
    capabilities: &CapabilitySnapshotManager,
    models: &SharedTemporaryModelStore,
    service: Arc<RemoteTaskService>,
    now_b: u64,
) -> Result<Value, String> {
    let mode = validated
        .get("mode")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?
        .to_owned();
    let model_source = validated
        .get("model_source")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?
        .to_owned();
    let task_id = validated
        .get("task_id")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?
        .to_owned();
    temporary_model_gate(&model_source, &task_id, models)?;
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
            .workspace_for_task(revision, lease, &workspace_id, now_b)
            .map_err(|error| error.code.to_owned())?;
        if !app
            .state::<Arc<DesktopRemoteTaskRuntime>>()
            .authorizations()
            .authorize(session_id, &workspace_id)
        {
            return Err("task_workspace_capacity".to_owned());
        }
    }
    let outcome = service
        .submit(lease, validated, now_b)
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
    cancel_and_project(&app, &service, &task_id)
}

/// wire 入口（E2E-04）：取消一条任务并回传终态/请求态载荷。
pub(crate) fn cancel_decrypted_task(app: &AppHandle, task_id: &str) -> Result<Value, String> {
    let controlled = app.state::<ControlledDeviceManager>();
    ensure_supported(&controlled)?;
    let runtime = app.state::<Arc<DesktopRemoteTaskRuntime>>();
    cancel_and_project(app, &runtime.service(), task_id)
}

/// 取消并产出回包（命令入口与 wire 入口共用）。
fn cancel_and_project(
    app: &AppHandle,
    service: &Arc<RemoteTaskService>,
    task_id: &str,
) -> Result<Value, String> {
    let outcome = service
        .cancel(task_id, monotonic_millis())
        .map_err(|error| error.code.to_owned())?;
    let payload = service
        .status_payload(task_id)
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
        // 任务校验的能力集来自活租约的协商结果（live_runtime_context），
        // 不再使用本文件内的常量表。
        assert!(!TASK_MODE_CHAT.is_empty());
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

    #[test]
    fn controller_temporary_tasks_require_an_installed_package() {
        use crate::features::remote_control::controller::crypto::SensitiveString;
        use crate::features::remote_control::controller::protocol::LeaseContext;
        use crate::features::remote_control::controller::temporary_model::{
            ExportRoute, TemporaryModelBinding, TemporaryModelPackageV1, TemporaryModelStore,
        };

        let store = TemporaryModelStore::default();
        // B-side model tasks never need a package.
        assert!(temporary_model_gate("controlled_device", "task-0001", &store).is_ok());
        // A-side model tasks fail closed while no package is installed.
        assert_eq!(
            temporary_model_gate("controller_temporary", "task-0001", &store).unwrap_err(),
            "model_package_missing"
        );

        let lease = LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        };
        let package = TemporaryModelPackageV1 {
            package_id: "package-0001".into(),
            binding: TemporaryModelBinding {
                connection_id: lease.connection_id.clone(),
                controller_lease_id: lease.controller_lease_id.clone(),
                lease_epoch: 1,
                task_id: "task-0001".into(),
            },
            primary: ExportRoute {
                display_name: "My DeepSeek".into(),
                preset: "deepseek".into(),
                wire_api: "openai_chat_completions".into(),
                base_url: "https://api.deepseek.com/v1".into(),
                model: "deepseek-chat".into(),
                provider_kind: Some("official_api".into()),
                vendor: Some("deepseek".into()),
                endpoint_mode: None,
                context_window_tokens: None,
                max_output_tokens: None,
                reasoning_effort: None,
                image_capability_override: "pinvou".into(),
                secret: SensitiveString::new("sk-test-value").unwrap(),
            },
            vision_fallback: None,
        };
        store.install(package, &lease, "task-0001", 1_000).unwrap();
        assert!(temporary_model_gate("controller_temporary", "task-0001", &store).is_ok());
        // A package bound to one task never authorizes another task.
        assert_eq!(
            temporary_model_gate("controller_temporary", "task-0002", &store).unwrap_err(),
            "model_package_missing"
        );
    }
}
