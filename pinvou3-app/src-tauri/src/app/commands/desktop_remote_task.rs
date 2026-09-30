//! Windows remote task commands (DEV-13).
//!
//! The command layer is a thin adapter: it re-validates the wire payload
//! against the live lease and the negotiated capabilities, resolves the opaque
//! workspace reference through the DEV-12 snapshot, and hands the request to
//! the B task owner. DEV-20 owns final handler registration.

use std::sync::Arc;
use std::time::Instant;

use deepseek_tui::models::{ContentBlock, Message, Role};
use serde_json::Value;
use tauri::{AppHandle, Emitter, Manager as _, State};
use tokio_util::sync::CancellationToken;

use crate::features::assistant::engine_pool::EnginePool;
use crate::features::assistant::remote_turn::{EnginePoolChatEngine, EnginePoolChatTurnPort};
use crate::features::codex_acp::AcpPool;
use crate::features::codex_acp::remote_turn::{AcpPoolCodeTurnPort, RemoteWorkspaceAuthorizations};
use crate::features::remote_control::controller::capabilities::CapabilitySnapshotManager;
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::controlling_session::task_input_digest;
use crate::features::remote_control::controller::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
};
#[cfg(test)]
use crate::features::remote_control::controller::task::RemoteTaskEvent;
use crate::features::remote_control::controller::task::{
    AdmissionOutcome, CancelOutcome, ChatTurnExecutor, CodeTurnExecutor, ControllerTaskEventSink,
    RemoteSessionProvisioner, RemoteTaskHooks, RemoteTaskLedger, RemoteTaskService, TASK_MODE_CHAT,
    TASK_MODE_CODE, TASK_MODEL_SOURCE_CONTROLLED, TASK_MODEL_SOURCE_TEMPORARY,
};
use crate::features::remote_control::controller::temporary_model::{
    SharedTemporaryModelStore, TemporaryModelPackageV1,
};
use crate::features::sessions::{SessionStore, transcript_revision};

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
const TASK_EVENT: &str = "desktop-remote-task-status";
const TASK_STATUS_EVENT: &str = "desktop-remote-task-event";
pub(crate) const CONTROLLED_SESSION_EVENT: &str = "desktop-remote-controlled-session";
const CONTROLLED_JOURNAL_EVENT: &str = "desktop-remote-controlled-journal";
const TASK_CONTENT_EVENT: &str = "desktop-remote-task-content";
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
        // B owns this conversation too. Project accepted user/assistant/error
        // text into B's local chat independently of encrypted B -> A delivery.
        // The journal event id is also the frontend idempotency key.
        let role = match kind {
            crate::features::remote_control::controller::journal::JournalKind::UserMessage => {
                Some("user")
            }
            crate::features::remote_control::controller::journal::JournalKind::AssistantDelta => {
                Some("assistant")
            }
            crate::features::remote_control::controller::journal::JournalKind::Error => {
                Some("error")
            }
            _ => None,
        };
        let local_content = role.zip(task_id).and_then(|(role, task_id)| {
            payload
                .get("text")
                .or_else(|| payload.get("message"))
                .and_then(Value::as_str)
                .map(|text| (role, task_id.to_owned(), text.to_owned()))
        });
        let history = self
            .app
            .state::<Arc<super::desktop_remote_history::DesktopRemoteHistoryRuntime>>();
        let event_id = format!("journal-{}", uuid::Uuid::new_v4().simple());
        match history
            .journal()
            .append(&event_id, kind, session_id, task_id, payload)
        {
            Ok(event) => {
                // B owns the authoritative event, so project it to B's UI as
                // well as forwarding the session-ref-rewritten copy to A.
                let _ = self.app.emit(
                    CONTROLLED_JOURNAL_EVENT,
                    serde_json::json!({
                        "historyId": Value::Null,
                        "event": &event,
                    }),
                );
                if let Some((role, task_id, text)) = local_content {
                    let _ = self.app.emit(
                        TASK_CONTENT_EVENT,
                        serde_json::json!({
                            "eventId": &event.event_id,
                            "taskId": task_id,
                            "sessionId": session_id,
                            "role": role,
                            "text": text,
                        }),
                    );
                }
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "journal_event_projected",
                    &event.kind,
                );
                super::desktop_remote_history::publish_authoritative_event(&self.app, &event);
            }
            Err(error) => {
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "journal_event_not_recorded",
                    error.code,
                );
            }
        }
    }
}

fn message_text(message: &Message) -> String {
    message
        .content
        .iter()
        .filter_map(|block| match block {
            ContentBlock::Text { text, .. } => Some(text.as_str()),
            _ => None,
        })
        .collect()
}

fn text_message(role: Role, text: impl Into<String>) -> Message {
    Message {
        role,
        content: vec![ContentBlock::Text {
            text: text.into(),
            cache_control: None,
        }],
    }
}

fn append_remote_failure_messages(
    messages: &mut Vec<Message>,
    baseline_messages: usize,
    prompt: &str,
    reason: &str,
) {
    let tail = &messages[baseline_messages.min(messages.len())..];
    let prompt_persisted = tail
        .iter()
        .any(|message| message.role == Role::User && message_text(message) == prompt);
    if !prompt_persisted {
        messages.push(text_message(Role::User, prompt.to_owned()));
    }
    messages.push(text_message(Role::Assistant, reason.to_owned()));
}

/// A failed remote turn must remain part of B's authoritative transcript.
/// The engine normally persists the user turn itself; failures before engine
/// admission do not, so use the pre-dispatch message count to fill only the
/// missing prompt and then append the bounded failure reason.
fn persist_remote_task_failure(
    store: &SessionStore,
    session_id: &str,
    baseline_messages: usize,
    prompt: &str,
    reason: &str,
) -> std::result::Result<(), String> {
    for _ in 0..4 {
        let session = store
            .load(session_id)
            .map_err(|_| "task_failure_persist_failed".to_owned())?;
        let revision = transcript_revision(&session.messages)
            .map_err(|_| "task_failure_persist_failed".to_owned())?;
        let mut messages = session.messages;
        append_remote_failure_messages(&mut messages, baseline_messages, prompt, reason);
        match store.compare_and_swap_messages(session_id, &revision, messages) {
            Ok(_) => return Ok(()),
            Err(error) if error.to_string().contains("session_revision_conflict") => continue,
            Err(_) => return Err("task_failure_persist_failed".to_owned()),
        }
    }
    Err("task_failure_persist_conflict".to_owned())
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
        temporary_models: SharedTemporaryModelStore,
    ) -> Arc<Self> {
        let service = Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        )));
        let authorizations = Arc::new(RemoteWorkspaceAuthorizations::default());
        service.set_session_provisioner(Arc::new(DesktopRemoteSessionProvisioner::new(
            store.clone(),
            pool.clone(),
        )));
        service.register_executor(
            TASK_MODE_CHAT,
            Arc::new(ChatTurnExecutor::new(Arc::new(
                EnginePoolChatTurnPort::new(Arc::new(EnginePoolChatEngine::new(
                    app,
                    Arc::new(pool),
                    temporary_models,
                ))),
            ))),
        );
        service.register_executor(
            TASK_MODE_CODE,
            Arc::new(CodeTurnExecutor::new(Arc::new(AcpPoolCodeTurnPort::new(
                Arc::new(acp_pool),
                Arc::clone(&authorizations),
            )))),
        );
        // The composition root installs DesktopTaskEventSink with the shared
        // authoritative journal before recovery or task admission.
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
    store
        .contains_task(task_id)
        .then_some(())
        .ok_or_else(|| "model_package_missing".to_owned())
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

/// Ensure the A-side session reference already owns a B-authoritative session
/// during capability preparation. This lets either endpoint be first to type.
pub(crate) fn ensure_controlled_session(
    app: &AppHandle,
    remote_session_ref: &str,
    title: &str,
    code_mode: bool,
    generation: &str,
) -> Result<String, String> {
    let controlled = app.state::<ControlledDeviceManager>();
    let is_current = controlled
        .live_runtime_context()
        .is_ok_and(|(lease, _)| lease.remote_control_generation == generation);
    if !is_current {
        return Err("task_session_generation_stale".to_owned());
    }
    let runtime = app.state::<Arc<DesktopRemoteTaskRuntime>>();
    let session_id = runtime
        .service()
        .ensure_remote_session(remote_session_ref, title, code_mode, generation)
        .map_err(|error| error.code.to_owned())?;
    let remains_current = controlled
        .live_runtime_context()
        .is_ok_and(|(lease, _)| lease.remote_control_generation == generation);
    if !remains_current {
        runtime.service().clear_generation_sessions(generation);
        return Err("task_session_generation_stale".to_owned());
    }
    Ok(session_id)
}

pub(crate) fn project_controlled_session(
    app: &AppHandle,
    remote_session_ref: &str,
    session_id: &str,
    lease: &LeaseContext,
) -> Result<(), String> {
    app.emit(
        CONTROLLED_SESSION_EVENT,
        serde_json::json!({
            "sessionId": session_id,
            "sessionRef": remote_session_ref,
            "generation": lease.remote_control_generation,
        }),
    )
    .map_err(|_| "controlled_session_projection_failed".to_owned())
}

#[tauri::command]
pub fn desktop_remote_task_open_session(
    app: AppHandle,
    remote_session_ref: String,
    title: String,
    code_mode: bool,
    controlled: State<'_, ControlledDeviceManager>,
    _runtime: RuntimeState<'_>,
) -> Result<String, String> {
    ensure_supported(&controlled)?;
    let (lease, _) = controlled
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    ensure_controlled_session(
        &app,
        &remote_session_ref,
        &title,
        code_mode,
        &lease.remote_control_generation,
    )
}

#[tauri::command]
pub fn desktop_remote_task_switch_session(
    remote_session_ref: String,
    session_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<String, String> {
    ensure_supported(&controlled)?;
    let (lease, _) = controlled
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    runtime
        .service()
        .switch_remote_session(
            &remote_session_ref,
            &session_id,
            &lease.remote_control_generation,
        )
        .map_err(|error| error.code.to_owned())
        .and_then(|session_id| {
            let remains_current = controlled.live_runtime_context().is_ok_and(|(current, _)| {
                current.remote_control_generation == lease.remote_control_generation
            });
            if remains_current {
                Ok(session_id)
            } else {
                runtime
                    .service()
                    .clear_generation_sessions(&lease.remote_control_generation);
                Err("task_session_generation_stale".to_owned())
            }
        })
}

#[tauri::command]
pub fn desktop_remote_task_close_session(
    remote_session_ref: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<Option<String>, String> {
    ensure_supported(&controlled)?;
    let (lease, _) = controlled
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    runtime
        .service()
        .close_remote_session(&remote_session_ref, &lease.remote_control_generation)
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
    // 命令入口只接受当前远控 generation 已绑定的权威会话。
    if !runtime
        .service()
        .is_remote_session_for(&session_id, &lease.remote_control_generation)
    {
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

/// B-side composer submission. Rust constructs the frozen payload and routes
/// it through the same admission, executor and journal as an A-originated task.
#[tauri::command]
pub fn desktop_remote_controlled_task_send(
    app: AppHandle,
    session_id: String,
    text: String,
    controlled: State<'_, ControlledDeviceManager>,
    capabilities: State<'_, CapabilitySnapshotManager>,
    models: State<'_, SharedTemporaryModelStore>,
    runtime: RuntimeState<'_>,
) -> Result<Value, String> {
    ensure_supported(&controlled)?;
    let text = text.trim();
    if text.is_empty() {
        return Err("task_input_invalid".to_owned());
    }
    let (lease, negotiated_capabilities) = controlled
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    if !runtime
        .service()
        .is_remote_session_for(&session_id, &lease.remote_control_generation)
    {
        return Err("task_session_not_found".to_owned());
    }
    let now_b = monotonic_millis();
    let snapshot = capabilities
        .current_for_context(&lease, now_b)
        .map_err(|error| error.code.to_owned())?;
    let revision_payload = capabilities.wire_capability_snapshot(&snapshot, "local-revision");
    let revision = revision_payload
        .get("capability_revision")
        .and_then(Value::as_str)
        .ok_or_else(|| "capability_snapshot_missing".to_owned())?;
    let unique = uuid::Uuid::new_v4().simple().to_string();
    let payload = serde_json::json!({
        "v": 1,
        "type": "task_submit",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "request_id": format!("local-request-{unique}"),
        "task_id": format!("local-task-{unique}"),
        "client_request_id": format!("local-client-{unique}"),
        "session_id": session_id.clone(),
        "capability_revision": revision,
        "model_source": TASK_MODEL_SOURCE_CONTROLLED,
        "mode": TASK_MODE_CHAT,
        "input_digest": task_input_digest(text),
        "input": {
            "text": text,
            "attachment_ids": Vec::<String>::new(),
            "skill_ids": Vec::<String>::new(),
        },
    });
    let validated = validated_task_payload(&payload, &lease, &negotiated_capabilities)?;
    admit_and_dispatch(
        &app,
        &validated,
        &lease,
        &session_id,
        &capabilities,
        &models,
        runtime.service(),
        now_b,
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
    let session_id = ensure_controlled_session(
        app,
        session_ref,
        "",
        mode == TASK_MODE_CODE,
        &lease.remote_control_generation,
    )?;
    project_controlled_session(app, session_ref, &session_id, &lease)?;
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
    if mode == TASK_MODE_CODE && model_source == TASK_MODEL_SOURCE_TEMPORARY {
        return Err("task_model_source_unsupported".to_owned());
    }
    crate::features::remote_control::controller::relay_runtime::log_controlled_event(
        "task_model_source_received",
        &model_source,
    );
    let temporary_package = if model_source == TASK_MODEL_SOURCE_TEMPORARY {
        let package = validated
            .get("temporary_model_package")
            .ok_or_else(|| "model_package_missing".to_owned())?;
        let package = TemporaryModelPackageV1::from_wire_value(package)
            .map_err(|error| error.code.to_owned())?;
        package
            .validate_for_task(lease, &task_id)
            .map_err(|error| error.code.to_owned())?;
        Some(package)
    } else {
        None
    };
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
        .submit_bound_with_admission(lease, validated, now_b, |ticket| {
            if let Some(package) = temporary_package {
                models
                    .install(package, lease, &ticket.record.task_id, now_b)
                    .map_err(|error| error.code)?;
            }
            Ok(())
        })
        .map_err(|error| error.code.to_owned())?;
    match outcome {
        AdmissionOutcome::Replay(record) => service
            .status_payload(&record.task_id)
            .map_err(|error| error.code.to_owned()),
        AdmissionOutcome::Admitted(ticket) => {
            let task_id = ticket.record.task_id.clone();
            if model_source == TASK_MODEL_SOURCE_TEMPORARY {
                temporary_model_gate(&model_source, &task_id, models)?;
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "task_temporary_model_installed",
                    "ready",
                );
            }
            let task_session_id = ticket.record.session_id.clone();
            let prompt = validated
                .get("input")
                .and_then(|input| input.get("text"))
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned();
            let session_store = app.state::<SessionStore>().inner().clone();
            let baseline_messages = session_store
                .load(&task_session_id)
                .map(|session| session.messages.len())
                .unwrap_or(0);
            let status = service
                .status_payload(&task_id)
                .map_err(|error| error.code.to_owned())?;
            // The persisted ledger intentionally excludes prompt text. Project
            // it once to B's local UI only after admission so the controlled
            // device can visibly confirm what it is executing.
            if !prompt.is_empty() {
                let _ = app.emit(
                    TASK_STATUS_EVENT,
                    serde_json::json!({
                        "taskId": &task_id,
                        "sessionId": session_id,
                        "status": "admitted",
                        "text": &prompt,
                        "messageIndex": baseline_messages,
                    }),
                );
            }
            let events: Arc<dyn ControllerTaskEventSink> =
                Arc::new(TaskEventBridge { app: app.clone() });
            let dispatch_service = Arc::clone(&service);
            crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                "task_execution_scheduled",
                "admitted",
            );
            // Publish every admitted projection before dispatch can synchronously
            // advance the ledger to running on the worker thread.
            let _ = app.emit(TASK_EVENT, &status);
            tauri::async_runtime::spawn(async move {
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "task_execution_started",
                    "running",
                );
                match dispatch_service
                    .dispatch(ticket, events, CancellationToken::new(), monotonic_millis())
                    .await
                {
                    Ok(record) => {
                        if record.wire_status() != "succeeded" {
                            let reason = record
                                .terminal_reason
                                .as_deref()
                                .unwrap_or_else(|| record.wire_status());
                            if let Err(error) = persist_remote_task_failure(
                                &session_store,
                                &task_session_id,
                                baseline_messages,
                                &prompt,
                                reason,
                            ) {
                                eprintln!(
                                    "[remote-control] failed to persist terminal task content: {error}"
                                );
                            }
                        }
                        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                            "task_execution_finished",
                            record.wire_status(),
                        );
                    }
                    Err(error) => {
                        if let Err(persist_error) = persist_remote_task_failure(
                            &session_store,
                            &task_session_id,
                            baseline_messages,
                            &prompt,
                            error.code,
                        ) {
                            eprintln!(
                                "[remote-control] failed to persist rejected task content: {persist_error}"
                            );
                        }
                        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                            "task_execution_failed",
                            error.code,
                        );
                        eprintln!(
                            "[remote-control] remote task dispatch rejected: {}",
                            error.code
                        );
                    }
                }
            });
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
            transition_sequence: 2,
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
    fn remote_failure_fills_a_missing_prompt_and_preserves_partial_output() {
        let mut missing_prompt = Vec::new();
        append_remote_failure_messages(
            &mut missing_prompt,
            0,
            "today's weather",
            "task_engine_unavailable",
        );
        assert_eq!(missing_prompt.len(), 2);
        assert_eq!(missing_prompt[0].role, Role::User);
        assert_eq!(message_text(&missing_prompt[0]), "today's weather");
        assert_eq!(missing_prompt[1].role, Role::Assistant);
        assert_eq!(message_text(&missing_prompt[1]), "task_engine_unavailable");

        let mut partial = vec![
            text_message(Role::User, "today's weather"),
            text_message(Role::Assistant, "partly cloudy"),
        ];
        append_remote_failure_messages(&mut partial, 0, "today's weather", "task_turn_failed");
        assert_eq!(partial.len(), 3);
        assert_eq!(message_text(&partial[1]), "partly cloudy");
        assert_eq!(message_text(&partial[2]), "task_turn_failed");
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
