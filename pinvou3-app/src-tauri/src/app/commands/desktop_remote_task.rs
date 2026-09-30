//! Windows remote task commands (DEV-13).
//!
//! The command layer is a thin adapter: it re-validates the wire payload
//! against the live lease and the negotiated capabilities, resolves the opaque
//! workspace reference through the DEV-12 snapshot, and hands the request to
//! the B task owner. DEV-20 owns final handler registration.

use std::sync::Arc;
use std::time::Instant;

use deepseek_tui::models::{ContentBlock, Message, Role};
use serde::Deserialize;
use serde_json::{Value, json};
use tauri::{AppHandle, Emitter, Manager as _, State};
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

use super::desktop_remote_model::prepare_selected_task_package_snapshot;
use crate::features::assistant::engine_pool::EnginePool;
use crate::features::assistant::remote_turn::{EnginePoolChatEngine, EnginePoolChatTurnPort};
use crate::features::codex_acp::AcpPool;
use crate::features::codex_acp::remote_turn::{AcpPoolCodeTurnPort, RemoteWorkspaceAuthorizations};
use crate::features::remote_control::controller::capabilities::CapabilitySnapshotManager;
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::controlling::ControllingDeviceManager;
use crate::features::remote_control::controller::controlling_session::ControllingSessionManager;
use crate::features::remote_control::controller::controlling_session::task_input_digest;
use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
use crate::features::remote_control::controller::protocol::MAX_TASK_INPUT_TEXT_BYTES;
use crate::features::remote_control::controller::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
};
use crate::features::remote_control::controller::task::{
    AdmissionOutcome, CancelOutcome, ChatTurnExecutor, CodeTurnExecutor, ControllerTaskEventSink,
    RemoteSessionLane, RemoteSessionProvisioner, RemoteTaskHooks, RemoteTaskLedger,
    RemoteTaskService, RemoteTaskTicket, TASK_MODE_CHAT, TASK_MODE_CODE,
    TASK_MODEL_SOURCE_CONTROLLED, TASK_MODEL_SOURCE_TEMPORARY,
};
#[cfg(test)]
use crate::features::remote_control::controller::task::{RemoteTaskEvent, RemoteTaskRequest};
use crate::features::remote_control::controller::temporary_model::{
    SharedTemporaryModelStore, TemporaryModelPackageV1,
};
use crate::features::sessions::application::dispatch_remote::{
    PreparedRemoteTask, RemoteDispatchReport, RemoteDispatchRequest, RemoteDispatchService,
    RemoteRetryHandle, RemoteTaskDispatchPort,
};
use crate::features::sessions::application::ports::UseCaseError;
use crate::features::sessions::application::submit_controlled::LocalControlledInput;
use crate::features::sessions::remote_proxy::outbox::RemoteRequestSemantics;
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
        Ok(controlled_session_exists(&self.store, session_id))
    }
}

fn controlled_session_exists(store: &SessionStore, session_id: &str) -> bool {
    match store.session_kind(session_id) {
        Ok(crate::features::sessions::SessionKind::ScheduledRun) => false,
        Ok(crate::features::sessions::SessionKind::Chat) => true,
        Ok(crate::features::sessions::SessionKind::RemoteProxy) => false,
        Err(_) => false,
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
            .state::<Arc<super::desktop_remote_journal::DesktopRemoteJournalRuntime>>();
        let Ok((lease, _)) = self.app.state::<
            crate::features::remote_control::controller::controlled::ControlledDeviceManager,
        >().live_runtime_context() else { return; };
        let runtime = self.app.state::<Arc<DesktopRemoteTaskRuntime>>();
        let Some(remote_ref) = runtime.service().remote_ref_for(session_id) else {
            return;
        };
        if runtime
            .service()
            .session_for(&remote_ref, &lease.remote_control_generation)
            .as_deref()
            != Some(session_id)
        {
            return;
        }
        if let Some(task_id) = task_id {
            let Ok(record) = runtime.service().status(task_id) else {
                return;
            };
            if record.session_id != session_id
                || record.lease.remote_control_generation != lease.remote_control_generation
            {
                return;
            }
        }
        if history
            .journal()
            .bind_generation(&lease.remote_control_generation)
            .is_err()
        {
            return;
        }
        let event_id = format!("journal-{}", uuid::Uuid::new_v4().simple());
        match history.journal().append_for_generation(
            &lease.remote_control_generation,
            &event_id,
            kind,
            session_id,
            task_id,
            payload,
        ) {
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
                super::desktop_remote_journal::publish_authoritative_event(
                    &self.app,
                    &lease.remote_control_generation,
                    &event,
                );
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
        None,
        &capabilities,
        &models,
        runtime.service(),
        monotonic_millis(),
    )
}

/// B-side composer submission. The command resolves the trusted B mapping;
/// the shared use case builds the frozen payload and enters the same admission,
/// executor and journal as A-originated input.
pub(crate) fn trusted_chat_remote_ref(
    service: &RemoteTaskService,
    session_id: &str,
    generation: &str,
) -> Result<String, String> {
    match service.lane_for_session(session_id, generation) {
        Some(RemoteSessionLane::Chat) => {}
        Some(RemoteSessionLane::Code | RemoteSessionLane::Unknown) => {
            return Err("task_mode_unsupported".to_owned());
        }
        None => return Err("task_session_not_found".to_owned()),
    }
    service
        .remote_ref_for(session_id)
        .ok_or_else(|| "task_session_not_found".to_owned())
}

fn resolve_local_controlled_input(
    service: &RemoteTaskService,
    session_id: String,
    text: String,
    generation: String,
) -> Result<LocalControlledInput, String> {
    let remote_ref = trusted_chat_remote_ref(service, &session_id, &generation)?;
    Ok(LocalControlledInput {
        remote_ref,
        generation,
        expected_session_id: session_id,
        title: String::new(),
        code_mode: false,
        text,
    })
}

#[tauri::command]
pub async fn desktop_remote_controlled_task_send(
    app: AppHandle,
    session_id: String,
    text: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<Value, String> {
    ensure_supported(&controlled)?;
    let (lease, _) = controlled
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    let service = runtime.service();
    let input = resolve_local_controlled_input(
        &service,
        session_id,
        text,
        lease.remote_control_generation,
    )?;
    app.state::<crate::app::desktop_remote_composition::DesktopLocalExecutionPort>()
        .submit_local(input)
        .await
        .map(|result| result.status)
        .map_err(|error| error.code().to_owned())
}

/// A/B 已完成各自可信映射与请求准备后共同进入的最终准入门禁。
/// 只接受准备阶段确认的精确 B Session，不再次 ensure 或改写 session_id。
pub(crate) fn submit_prepared_bound_task(
    app: &AppHandle,
    expected_session_id: &str,
    expected_remote_ref: &str,
    payload: &Value,
) -> Result<Value, String> {
    let controlled = app.state::<ControlledDeviceManager>();
    ensure_supported(&controlled)?;
    let (lease, negotiated_capabilities) = controlled
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    let validated = validated_task_payload(payload, &lease, &negotiated_capabilities)?;
    let submitted_session_id = validated
        .get("session_id")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_request_invalid".to_owned())?;
    if submitted_session_id != expected_session_id {
        return Err("task_session_not_found".to_owned());
    }
    let runtime = app.state::<Arc<DesktopRemoteTaskRuntime>>();
    let service = runtime.service();
    if !service.is_remote_session_for(expected_session_id, &lease.remote_control_generation) {
        return Err("task_session_not_found".to_owned());
    }
    let capabilities = app.state::<CapabilitySnapshotManager>();
    let models = app.state::<SharedTemporaryModelStore>();
    admit_and_dispatch(
        app,
        &validated,
        &lease,
        expected_session_id,
        Some(expected_remote_ref),
        &capabilities,
        &models,
        service,
        monotonic_millis(),
    )
}

fn admit_bound_task_with_ref(
    service: &RemoteTaskService,
    lease: &LeaseContext,
    payload: &Value,
    now_b: u64,
    expected_remote_ref: Option<&str>,
    install: impl FnOnce(&RemoteTaskTicket) -> std::result::Result<(), &'static str>,
) -> Result<AdmissionOutcome, String> {
    match expected_remote_ref {
        Some(remote_ref) => {
            service.submit_bound_for_ref_with_admission(lease, remote_ref, payload, now_b, install)
        }
        None => service.submit_bound_with_admission(lease, payload, now_b, install),
    }
    .map_err(|error| error.code.to_owned())
}

/// 接纳并派发一条已通过冻结校验、且会话已在远程映射中的 `task_submit`。
/// 命令入口与 wire 入口共用，避免两条实现漂移。
#[allow(clippy::too_many_arguments)]
fn admit_and_dispatch(
    app: &AppHandle,
    validated: &Value,
    lease: &LeaseContext,
    session_id: &str,
    expected_remote_ref: Option<&str>,
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
    let code_workspace = if mode == TASK_MODE_CODE {
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
        // Snapshot validation is read-only. Task-scoped authorization is
        // installed only after the locked binding/lane check and durable
        // ledger admission inside DEV-13's install closure below.
        capabilities
            .workspace_for_task(revision, lease, &workspace_id, now_b)
            .map_err(|error| error.code.to_owned())?;
        Some(workspace_id)
    } else {
        None
    };
    let authorizations = app
        .state::<Arc<DesktopRemoteTaskRuntime>>()
        .authorizations();
    let install = |ticket: &RemoteTaskTicket| {
        if let Some(package) = temporary_package {
            models
                .install(package, lease, &ticket.record.task_id, now_b)
                .map_err(|error| error.code)?;
        }
        if let Some(workspace_id) = code_workspace.as_deref() {
            if !authorizations.authorize(session_id, workspace_id) {
                return Err("task_workspace_capacity");
            }
        }
        Ok(())
    };
    let outcome = admit_bound_task_with_ref(
        &service,
        lease,
        validated,
        now_b,
        expected_remote_ref,
        install,
    )?;
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

/// DEV-29 host adapter. Registration and replacement of the legacy A command
/// belong to DEV-20/DEV-31; this adapter itself owns no outbox state.
pub(crate) struct DesktopRemoteTaskDispatchPort {
    app: AppHandle,
}

impl DesktopRemoteTaskDispatchPort {
    pub(crate) fn new(app: AppHandle) -> Self {
        Self { app }
    }
}

fn dispatch_error(code: &'static str) -> UseCaseError {
    UseCaseError::new(code, false)
}

fn lease_matches_remote_binding(
    lease: &LeaseContext,
    live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
) -> bool {
    lease.connection_id == live.connection_id
        && lease.controller_lease_id == live.controller_lease_id
        && lease.lease_epoch == live.lease_epoch
        && lease.relay_epoch == live.relay_epoch
        && lease.remote_control_generation == live.generation
}

fn with_current_dispatch_binding<T>(
    app: &AppHandle,
    live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
    commit: impl FnOnce() -> Result<T, String>,
) -> Result<T, UseCaseError> {
    let manager = app.state::<ControllingDeviceManager>();
    let lane = app.state::<ControllingLaneState>();
    let (lease, peer, boundary) = manager
        .authenticated_remote_proxy_identity()
        .map_err(|_| dispatch_error("remote_binding_mismatch"))?;
    if !lease_matches_remote_binding(&lease, live)
        || peer != live.peer_id
        || boundary != live.resume_boundary_id
    {
        return Err(dispatch_error("remote_binding_mismatch"));
    }
    manager
        .with_current_authenticated_remote_proxy_identity(&lane, &(lease, peer, boundary), commit)
        .map_err(|code| match code.as_str() {
            "remote_pending_persist_failed" => dispatch_error("remote_pending_persist_failed"),
            "remote_attempt_persist_failed" => dispatch_error("remote_attempt_persist_failed"),
            _ => dispatch_error("remote_binding_mismatch"),
        })
}

impl RemoteTaskDispatchPort for DesktopRemoteTaskDispatchPort {
    fn reserve_pending(
        &self,
        store: &SessionStore,
        local_session_id: &str,
        remote_ref: &str,
        live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
        pending: crate::features::sessions::remote_proxy::schema::PendingRemoteRequest,
    ) -> Result<crate::features::sessions::remote_proxy::schema::PendingReservation, UseCaseError>
    {
        with_current_dispatch_binding(&self.app, live, || {
            store
                .insert_remote_pending(local_session_id, remote_ref, live, pending)
                .map_err(|_| "remote_pending_persist_failed".to_owned())
        })
    }

    fn mark_dispatch_attempt(
        &self,
        store: &SessionStore,
        local_session_id: &str,
        remote_ref: &str,
        live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
        handle: &RemoteRetryHandle,
    ) -> Result<(), UseCaseError> {
        with_current_dispatch_binding(&self.app, live, || {
            store
                .mark_remote_dispatch_attempt(
                    local_session_id,
                    remote_ref,
                    live,
                    &handle.client_request_id,
                    &handle.task_id,
                    &handle.request_fingerprint,
                )
                .map_err(|_| "remote_attempt_persist_failed".to_owned())
        })
    }

    fn prepare(&self, request: &RemoteDispatchRequest) -> Result<PreparedRemoteTask, UseCaseError> {
        let manager = self.app.state::<ControllingDeviceManager>();
        let sessions = self.app.state::<ControllingSessionManager>();
        let (lease, capabilities) = manager
            .live_runtime_context()
            .map_err(|_| dispatch_error("controller_not_connected"))?;
        if !lease_matches_remote_binding(&lease, &request.live) {
            return Err(dispatch_error("remote_binding_mismatch"));
        }
        let semantics = &request.semantics;
        let session = sessions
            .state(&semantics.remote_session_ref, &lease)
            .map_err(|_| dispatch_error("remote_session_unavailable"))?;
        if session.session_id != semantics.session_id
            || session.mode != semantics.mode
            || session.capability_revision != Some(semantics.capability_revision)
            || semantics.mode != "chat"
            || !semantics.attachment_ids.is_empty()
            || !semantics.skill_ids.is_empty()
            || !semantics.workspace_ids.is_empty()
        {
            return Err(dispatch_error("remote_request_invalid"));
        }
        let temporary = match semantics.model_source.as_str() {
            TASK_MODEL_SOURCE_CONTROLLED if semantics.model_profile_id.is_none() => None,
            TASK_MODEL_SOURCE_TEMPORARY if session.temporary_model_available => {
                let selected_model_id = semantics
                    .model_profile_id
                    .as_deref()
                    .ok_or_else(|| dispatch_error("remote_model_profile_required"))?;
                Some(
                    prepare_selected_task_package_snapshot(
                        &lease,
                        &request.task_id,
                        selected_model_id,
                    )
                    .map_err(|error| dispatch_error(error.code))?,
                )
            }
            _ => return Err(dispatch_error("remote_model_source_invalid")),
        };
        let input = json!({
            "text": semantics.text,
            "attachment_ids": semantics.attachment_ids,
            "skill_ids": semantics.skill_ids,
        });
        let mut payload = json!({
            "v": 1,
            "type": "task_submit",
            "connection_id": lease.connection_id,
            "controller_lease_id": lease.controller_lease_id,
            "lease_epoch": lease.lease_epoch,
            "relay_epoch": lease.relay_epoch,
            "remote_control_generation": lease.remote_control_generation,
            "request_id": format!("request-{}", Uuid::new_v4().simple()),
            "task_id": request.task_id,
            "client_request_id": request.client_request_id,
            "session_id": semantics.session_id,
            "capability_revision": semantics.capability_revision.to_string(),
            "model_source": semantics.model_source,
            "mode": semantics.mode,
            "input_digest": semantics.input_digest,
            "input": input,
        });
        let model_profile_id = temporary
            .as_ref()
            .map(|prepared| prepared.model_profile_id.clone());
        if let Some(prepared) = temporary {
            payload["temporary_model_package"] = prepared.package;
        }
        validate_controller_runtime_payload_json(
            &payload.to_string(),
            SenderRole::ControllerA,
            Some(&RuntimeValidationContext {
                lease: Some(lease),
                negotiated_capabilities: capabilities,
            }),
        )
        .map_err(|_| dispatch_error("remote_request_invalid"))?;
        Ok(PreparedRemoteTask {
            model_profile_id,
            payload,
        })
    }

    fn send_encrypted(&self, payload: &Value) -> Result<(), UseCaseError> {
        let manager = self.app.state::<ControllingDeviceManager>();
        let business = self.app.state::<ControllingLaneState>();
        let (lease, capabilities) = manager
            .live_runtime_context()
            .map_err(|_| dispatch_error("controller_not_connected"))?;
        // Revalidate at the actual send point; a lease rotation between
        // prepare and barrier must become outcome_unknown, never a new send.
        validate_controller_runtime_payload_json(
            &payload.to_string(),
            SenderRole::ControllerA,
            Some(&RuntimeValidationContext {
                lease: Some(lease),
                negotiated_capabilities: capabilities.clone(),
            }),
        )
        .map_err(|_| dispatch_error("remote_binding_mismatch"))?;
        let frame = business
            .seal("task_submit", payload, &capabilities)
            .map_err(|_| dispatch_error("remote_encrypt_failed"))?;
        manager
            .send_authenticated_business(&frame)
            .map_err(|_| dispatch_error("remote_send_uncertain"))
    }
}

/// New unregistered A command boundary. The trusted live context comes from
/// SessionStore, never from UI-supplied lease fields. The returned status and
/// IDs are non-sensitive; no temporary package is projected to WebView.
#[derive(Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub(crate) enum RemoteTaskSubmitIntent {
    New {
        text: String,
        #[serde(rename = "modelSource")]
        model_source: String,
        #[serde(rename = "selectedModelId")]
        selected_model_id: Option<String>,
    },
    Retry {
        handle: RemoteRetryHandle,
    },
}

#[tauri::command]
pub fn desktop_remote_task_dispatch_a(
    service: State<'_, RemoteDispatchService>,
    store: State<'_, SessionStore>,
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    local_session_id: String,
    remote_ref: String,
    intent: RemoteTaskSubmitIntent,
) -> Result<RemoteDispatchReport, String> {
    if !manager.is_supported() {
        return Err(UNSUPPORTED.to_owned());
    }
    // Retry cannot carry task text, model choice, mode or any other mutable
    // semantics. The service authenticates this handle against durable pending
    // state and the live binding before replaying the original cached bytes.
    let (text, model_source, selected_model_id) = match intent {
        RemoteTaskSubmitIntent::New {
            text,
            model_source,
            selected_model_id,
        } => (text, model_source, selected_model_id),
        RemoteTaskSubmitIntent::Retry { handle } => {
            return service
                .dispatch_retry(&local_session_id, &remote_ref, handle)
                .map_err(|error| error.code().to_owned());
        }
    };
    let live = store
        .remote_dispatch_live_context(&local_session_id, &remote_ref)
        .map_err(|_| "remote_binding_mismatch".to_owned())?;
    let (lease, _) = manager
        .live_runtime_context()
        .map_err(|_| "controller_not_connected".to_owned())?;
    if !lease_matches_remote_binding(&lease, &live) {
        return Err("remote_binding_mismatch".to_owned());
    }
    let session = sessions
        .state(&remote_ref, &lease)
        .map_err(|_| "remote_session_unavailable".to_owned())?;
    let record = store
        .remote_proxy_snapshot(&local_session_id)
        .ok_or_else(|| "remote_proxy_not_found".to_owned())?;
    let controlled_session_id = record
        .controlled_session_id
        .ok_or_else(|| "remote_mapping_unconfirmed".to_owned())?;
    let text = text.trim().to_owned();
    if text.is_empty() || text.len() > MAX_TASK_INPUT_TEXT_BYTES {
        return Err("task_input_invalid".to_owned());
    }
    match model_source.as_str() {
        TASK_MODEL_SOURCE_CONTROLLED if selected_model_id.is_none() => {}
        TASK_MODEL_SOURCE_TEMPORARY
            if session.temporary_model_available
                && selected_model_id.as_deref().is_some_and(
                    crate::features::sessions::remote_proxy::schema::valid_remote_identifier,
                ) => {}
        _ => return Err("remote_model_source_invalid".to_owned()),
    }
    // Fresh A-authenticated capability is a prerequisite for NEW durable
    // outbox reservation. Retry is deliberately handled above using only the
    // frozen handle and never re-reads mutable capability/configuration.
    let capability_revision = sessions
        .capability_projection(&remote_ref, &lease)
        .map_err(|error| match error.code {
            "capability_snapshot_expired" | "session_context_stale" => {
                "capability_snapshot_stale".to_owned()
            }
            code => code.to_owned(),
        })?
        .revision();
    if session.capability_revision != Some(capability_revision) {
        return Err("capability_snapshot_stale".to_owned());
    }
    let request = RemoteDispatchRequest {
        task_id: format!("task-{}", Uuid::new_v4().simple()),
        client_request_id: format!("client-request-{}", Uuid::new_v4().simple()),
        live,
        semantics: RemoteRequestSemantics {
            local_session_id,
            remote_session_ref: remote_ref,
            controlled_session_id,
            session_id: session.session_id,
            capability_revision,
            mode: session.mode,
            model_source,
            model_profile_id: selected_model_id,
            model_execution_digest: String::new(),
            input_digest: task_input_digest(&text),
            text,
            attachment_ids: Vec::new(),
            skill_ids: Vec::new(),
            workspace_ids: Vec::new(),
        },
    };
    service
        .dispatch_new(request)
        .map_err(|error| error.code().to_owned())
}
#[tauri::command]
pub fn desktop_remote_task_abandon_prepared_a(
    service: State<'_, RemoteDispatchService>,
    local_session_id: String,
    client_request_id: String,
    task_id: String,
) -> Result<(), String> {
    service
        .abandon_prepared_remote_task(&local_session_id, &client_request_id, &task_id)
        .map_err(|error| error.code().to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn retry_intent_rejects_mutable_task_semantics() {
        let handle = json!({
            "taskId": "task-1",
            "clientRequestId": "request-1",
            "requestFingerprint": "a".repeat(64),
        });
        let valid = serde_json::from_value::<RemoteTaskSubmitIntent>(json!({
            "kind": "retry", "handle": handle.clone(),
        }));
        assert!(matches!(valid, Ok(RemoteTaskSubmitIntent::Retry { .. })));
        assert!(
            serde_json::from_value::<RemoteTaskSubmitIntent>(json!({
                "kind": "retry", "handle": handle, "text": "new task",
            }))
            .is_err()
        );
    }

    #[test]
    fn new_intent_accepts_only_narrow_model_selection() {
        let intent = serde_json::from_value::<RemoteTaskSubmitIntent>(json!({
            "kind": "new",
            "text": "hello",
            "modelSource": "controller_temporary",
            "selectedModelId": "model-1",
        }))
        .unwrap();
        assert!(matches!(intent, RemoteTaskSubmitIntent::New {
            model_source, selected_model_id: Some(id), ..
        } if model_source == "controller_temporary" && id == "model-1"));
        assert!(
            serde_json::from_value::<RemoteTaskSubmitIntent>(json!({
                "kind": "new", "text": "hello", "modelSource": "controller_temporary",
                "selectedModelId": "model-1", "taskId": "ui-forged",
            }))
            .is_err()
        );
    }

    #[test]
    fn new_dispatch_checks_authenticated_capability_before_outbox_reservation() {
        let source = include_str!("desktop_remote_task.rs");
        let start = source
            .find("pub fn desktop_remote_task_dispatch_a(")
            .unwrap();
        let end = source[start..]
            .find("pub fn desktop_remote_task_abandon_prepared_a(")
            .map(|offset| start + offset)
            .unwrap();
        let command = &source[start..end];
        let retry = command.find("RemoteTaskSubmitIntent::Retry").unwrap();
        let fresh = command.find(".capability_projection(").unwrap();
        let reserve = command.find(".dispatch_new(request)").unwrap();
        assert!(retry < fresh && fresh < reserve);
        assert!(command.contains("capability_snapshot_stale"));
    }

    #[test]
    fn local_controlled_input_uses_only_the_bound_b_session_and_server_ref() {
        use std::sync::atomic::{AtomicUsize, Ordering};

        struct Provisioner(AtomicUsize);
        impl RemoteSessionProvisioner for Provisioner {
            fn create_authoritative_session(
                &self,
                _title: &str,
                _code_mode: bool,
            ) -> std::result::Result<String, String> {
                let number = self.0.fetch_add(1, Ordering::SeqCst);
                Ok(if number == 0 {
                    "b-session".to_owned()
                } else {
                    "code-session".to_owned()
                })
            }

            fn session_exists(&self, session_id: &str) -> std::result::Result<bool, String> {
                Ok(matches!(
                    session_id,
                    "b-session" | "code-session" | "unknown-session"
                ))
            }
        }

        let provisioner = Arc::new(Provisioner(AtomicUsize::new(0)));
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.set_session_provisioner(provisioner.clone());
        assert_eq!(
            service
                .ensure_remote_session("opaque-ref", "", false, "generation-1")
                .unwrap(),
            "b-session"
        );
        let input = resolve_local_controlled_input(
            &service,
            "b-session".to_owned(),
            "hello".to_owned(),
            "generation-1".to_owned(),
        )
        .unwrap();
        assert_eq!(input.remote_ref, "opaque-ref");
        assert_eq!(input.expected_session_id, "b-session");
        assert_eq!(input.text, "hello");
        assert!(!input.code_mode);
        assert_eq!(
            resolve_local_controlled_input(
                &service,
                "unbound".to_owned(),
                "hello".to_owned(),
                "generation-1".to_owned(),
            )
            .unwrap_err(),
            "task_session_not_found"
        );
        assert_eq!(
            resolve_local_controlled_input(
                &service,
                "b-session".to_owned(),
                "hello".to_owned(),
                "generation-2".to_owned(),
            )
            .unwrap_err(),
            "task_session_not_found"
        );
        assert_eq!(
            service
                .ensure_remote_session("code-ref", "", true, "generation-1")
                .unwrap(),
            "code-session"
        );
        assert_eq!(
            resolve_local_controlled_input(
                &service,
                "code-session".to_owned(),
                "hello".to_owned(),
                "generation-1".to_owned(),
            )
            .unwrap_err(),
            "task_mode_unsupported"
        );
        service
            .switch_remote_session("unknown-ref", "unknown-session", "generation-1")
            .unwrap();
        assert_eq!(
            resolve_local_controlled_input(
                &service,
                "unknown-session".to_owned(),
                "hello".to_owned(),
                "generation-1".to_owned(),
            )
            .unwrap_err(),
            "task_mode_unsupported"
        );
        assert_eq!(provisioner.0.load(Ordering::SeqCst), 2);
        assert_eq!(service.ledger().live_task_count(), 0);
    }

    #[test]
    fn prepared_submit_keeps_expected_ref_through_the_atomic_dev13_admission() {
        use std::sync::atomic::{AtomicUsize, Ordering};

        struct Provisioner(AtomicUsize);
        impl RemoteSessionProvisioner for Provisioner {
            fn create_authoritative_session(
                &self,
                _: &str,
                _: bool,
            ) -> std::result::Result<String, String> {
                self.0.fetch_add(1, Ordering::SeqCst);
                Ok("b-session".to_owned())
            }

            fn session_exists(&self, session_id: &str) -> std::result::Result<bool, String> {
                Ok(session_id == "b-session")
            }
        }

        let lease = LeaseContext {
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "epoch-1".into(),
            relay_epoch: "relay-1".into(),
            remote_control_generation: "generation-1".into(),
        };
        let frame = |session_id: &str, task_id: &str| {
            json!({
                "connection_id": lease.connection_id,
                "controller_lease_id": lease.controller_lease_id,
                "lease_epoch": lease.lease_epoch,
                "relay_epoch": lease.relay_epoch,
                "remote_control_generation": lease.remote_control_generation,
                "task_id": task_id,
                "client_request_id": format!("client-{task_id}"),
                "session_id": session_id,
                "capability_revision": "1",
                "mode": TASK_MODE_CHAT,
                "model_source": TASK_MODEL_SOURCE_CONTROLLED,
                "input_digest": task_input_digest("hello"),
                "input": { "text": "hello" },
            })
        };
        let provisioner = Arc::new(Provisioner(AtomicUsize::new(0)));
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.set_session_provisioner(provisioner.clone());
        assert_eq!(
            service
                .ensure_remote_session("old-ref", "", false, "generation-1")
                .unwrap(),
            "b-session"
        );
        service
            .close_remote_session("old-ref", "generation-1")
            .unwrap();
        service
            .switch_remote_session("new-ref", "b-session", "generation-1")
            .unwrap();
        let claim = RemoteTaskRequest::from_payload(&frame("new-ref", "task-claim")).unwrap();
        assert_eq!(
            service
                .ensure_authenticated_task_session(&claim, &lease, "")
                .unwrap(),
            "b-session"
        );
        let installs = AtomicUsize::new(0);
        assert_eq!(
            admit_bound_task_with_ref(
                &service,
                &lease,
                &frame("b-session", "task-old"),
                1_000,
                Some("old-ref"),
                |_| {
                    installs.fetch_add(1, Ordering::SeqCst);
                    Ok(())
                },
            )
            .unwrap_err(),
            "task_session_binding_mismatch"
        );
        assert_eq!(installs.load(Ordering::SeqCst), 0);
        assert_eq!(service.ledger().live_task_count(), 0);
        assert!(matches!(
            admit_bound_task_with_ref(
                &service,
                &lease,
                &frame("b-session", "task-new"),
                1_001,
                Some("new-ref"),
                |_| {
                    installs.fetch_add(1, Ordering::SeqCst);
                    Ok(())
                },
            ),
            Ok(AdmissionOutcome::Admitted(_))
        ));
        assert_eq!(installs.load(Ordering::SeqCst), 1);
        assert_eq!(provisioner.0.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn code_workspace_authorization_occurs_only_after_ref_and_lane_admission() {
        struct Provisioner;
        impl RemoteSessionProvisioner for Provisioner {
            fn create_authoritative_session(
                &self,
                _: &str,
                _: bool,
            ) -> std::result::Result<String, String> {
                Ok("b-code-session".to_owned())
            }

            fn session_exists(&self, session_id: &str) -> std::result::Result<bool, String> {
                Ok(session_id == "b-code-session")
            }
        }

        let lease = LeaseContext {
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "epoch-1".into(),
            relay_epoch: "relay-1".into(),
            remote_control_generation: "generation-1".into(),
        };
        let frame = |mode: &str, task_id: &str| {
            let mut payload = json!({
                "connection_id": lease.connection_id,
                "controller_lease_id": lease.controller_lease_id,
                "lease_epoch": lease.lease_epoch,
                "relay_epoch": lease.relay_epoch,
                "remote_control_generation": lease.remote_control_generation,
                "task_id": task_id,
                "client_request_id": format!("client-{task_id}"),
                "session_id": "b-code-session",
                "capability_revision": "1",
                "mode": mode,
                "model_source": TASK_MODEL_SOURCE_CONTROLLED,
                "input_digest": task_input_digest("hello"),
                "input": { "text": "hello" },
            });
            if mode == TASK_MODE_CODE {
                payload["input"]["workspace_id"] = json!("workspace-1");
            }
            payload
        };
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.set_session_provisioner(Arc::new(Provisioner));
        service
            .ensure_remote_session("code-ref", "", true, "generation-1")
            .unwrap();
        let authorizations = RemoteWorkspaceAuthorizations::default();
        let authorize = |_: &RemoteTaskTicket| {
            authorizations
                .authorize("b-code-session", "workspace-1")
                .then_some(())
                .ok_or("task_workspace_capacity")
        };
        assert_eq!(
            admit_bound_task_with_ref(
                &service,
                &lease,
                &frame(TASK_MODE_CODE, "task-wrong-ref"),
                1_000,
                Some("old-ref"),
                authorize,
            )
            .unwrap_err(),
            "task_session_binding_mismatch"
        );
        assert!(!authorizations.is_authorized("b-code-session", "workspace-1"));
        assert_eq!(
            admit_bound_task_with_ref(
                &service,
                &lease,
                &frame(TASK_MODE_CHAT, "task-wrong-lane"),
                1_001,
                Some("code-ref"),
                authorize,
            )
            .unwrap_err(),
            "task_session_lane_mismatch"
        );
        assert!(!authorizations.is_authorized("b-code-session", "workspace-1"));
        assert_eq!(service.ledger().live_task_count(), 0);
        assert!(matches!(
            admit_bound_task_with_ref(
                &service,
                &lease,
                &frame(TASK_MODE_CODE, "task-code"),
                1_002,
                Some("code-ref"),
                authorize,
            ),
            Ok(AdmissionOutcome::Admitted(_))
        ));
        assert!(authorizations.is_authorized("b-code-session", "workspace-1"));
    }

    #[test]
    fn controlled_provisioner_never_reuses_a_remote_proxy_as_b_authority() {
        use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;

        let root = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(root.path()).unwrap();
        let local = store
            .create_new("model".into(), None, root.path().join("local"))
            .unwrap();
        assert!(controlled_session_exists(&store, &local.metadata.id));
        let remote = store
            .test_commit_remote_proxy_open(
                None,
                &LiveRemoteContext {
                    peer_id: "peer".into(),
                    resume_boundary_id: "boundary".into(),
                    connection_id: "connection".into(),
                    controller_lease_id: "lease".into(),
                    lease_epoch: "epoch".into(),
                    relay_epoch: "relay".into(),
                    generation: "generation".into(),
                    control_connected: true,
                    encrypted_channel_connected: true,
                },
            )
            .unwrap();
        assert!(!controlled_session_exists(&store, &remote.local_session_id));
    }

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
