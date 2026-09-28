//! Windows remote task commands (DEV-13).
//!
//! The command layer is a thin adapter: it re-validates the wire payload
//! against the live lease and the negotiated capabilities, resolves the opaque
//! workspace reference through the DEV-12 snapshot, and hands the request to
//! the B task owner. DEV-20 owns final handler registration.

use std::collections::HashSet;
use std::sync::Arc;
use std::time::Instant;

use deepseek_tui::models::{ContentBlock, Message, Role};
use serde::Deserialize;
use serde_json::{Value, json};
use sha2::{Digest as _, Sha256};
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
use crate::features::remote_control::controller::controlling_session::{
    build_bound_task_cancel_payload, task_input_digest,
};
use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
use crate::features::remote_control::controller::protocol::MAX_TASK_INPUT_TEXT_BYTES;
use crate::features::remote_control::controller::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
};
use crate::features::remote_control::controller::task::{
    AdmissionOutcome, CancelOutcome, ChatTurnExecutor, CodeTurnExecutor, ControllerTaskEventSink,
    RemoteSessionLane, RemoteSessionProvisioner, RemoteTaskHooks, RemoteTaskLedger,
    RemoteTaskModelSource, RemoteTaskService, RemoteTaskTicket, TASK_MODE_CHAT, TASK_MODE_CODE,
    TASK_MODEL_SOURCE_CONTROLLED, TASK_MODEL_SOURCE_TEMPORARY,
};
#[cfg(test)]
use crate::features::remote_control::controller::task::{RemoteTaskEvent, RemoteTaskRequest};
use crate::features::remote_control::controller::temporary_model::{
    SharedTemporaryModelStore, TemporaryModelPackageV1,
};
use crate::features::remote_control::controller::transfer::upload::PublishedAttachment;
use crate::features::sessions::application::dispatch_remote::{
    PreparedRemoteTask, RemoteCancelReport, RemoteDispatchReport, RemoteDispatchRequest,
    RemoteDispatchService, RemoteRetryHandle, RemoteTaskDispatchPort,
};
use crate::features::sessions::application::ports::UseCaseError;
use crate::features::sessions::application::submit_controlled::LocalControlledInput;
use crate::features::sessions::remote_proxy::outbox::{
    RemoteRequestSemantics, remote_task_input_digest,
};
use crate::features::sessions::{SessionStore, transcript_revision};

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
const TASK_EVENT: &str = "desktop-remote-task-status";
const TASK_STATUS_EVENT: &str = "desktop-remote-task-event";
pub(crate) const CONTROLLED_SESSION_EVENT: &str = "desktop-remote-controlled-session";
const CONTROLLED_JOURNAL_EVENT: &str = "desktop-remote-controlled-journal";
const TASK_CONTENT_EVENT: &str = "desktop-remote-task-content";

fn capability_allows_skills(
    capability: &crate::features::remote_control::controller::controlling_session::ControllerCapabilityProjection,
    mode: &str,
    skill_ids: &[String],
) -> bool {
    let mut seen = HashSet::new();
    skill_ids.len() <= 32
        && skill_ids.iter().all(|skill_id| {
            seen.insert(skill_id.as_str())
                && capability.skills.iter().any(|skill| {
                    skill.skill_id == *skill_id && skill.scopes.iter().any(|scope| scope == mode)
                })
        })
}

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
    app: AppHandle,
    store: SessionStore,
    pool: EnginePool,
    acp_pool: Arc<AcpPool>,
}

impl DesktopRemoteSessionProvisioner {
    pub(crate) fn new(
        app: AppHandle,
        store: SessionStore,
        pool: EnginePool,
        acp_pool: Arc<AcpPool>,
    ) -> Self {
        Self {
            app,
            store,
            pool,
            acp_pool,
        }
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
        Ok(controlled_session_exists(&self.store, session_id))
    }

    fn claim_execution_lane(
        &self,
        session_id: &str,
        code_mode: bool,
        workspace_id: Option<&str>,
    ) -> std::result::Result<(), String> {
        if !code_mode {
            return Ok(());
        }
        let workspace_id = workspace_id.ok_or_else(|| "task_workspace_required".to_owned())?;
        let path = self
            .app
            .state::<ControlledDeviceManager>()
            .resolve_code_workspace(workspace_id)
            .map_err(|error| error.code.to_owned())?;
        let path = crate::features::codex_acp::validate_codex_project_workspace(&path)
            .map_err(|_| "task_workspace_not_authorized".to_owned())?;
        self.acp_pool
            .agents()
            .set_acp_workspace(
                session_id,
                crate::features::codex_acp::AgentBackend::CodexAcp,
                crate::features::codex_acp::CodexWorkspaceKind::Project,
                Some(path),
            )
            .map_err(|_| "task_session_backend_bind_failed".to_owned())
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
        // B may finish or cancel an already admitted local task while the
        // encrypted lane is reconnecting. Keep the authoritative B journal;
        // forwarding to A remains gated by the connected transport below.
        let Ok(lease) = self.app.state::<
            crate::features::remote_control::controller::controlled::ControlledDeviceManager,
        >().with_local_cancel_lease(Clone::clone) else { return; };
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
    code_turn: Arc<AcpPoolCodeTurnPort>,
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
        let acp_pool = Arc::new(acp_pool);
        service.set_session_provisioner(Arc::new(DesktopRemoteSessionProvisioner::new(
            app.clone(),
            store.clone(),
            pool.clone(),
            Arc::clone(&acp_pool),
        )));
        service.register_executor(
            TASK_MODE_CHAT,
            Arc::new(ChatTurnExecutor::new(Arc::new(
                EnginePoolChatTurnPort::new(Arc::new(EnginePoolChatEngine::new(
                    app.clone(),
                    Arc::new(pool),
                    temporary_models,
                ))),
            ))),
        );
        let code_turn =
            Arc::new(AcpPoolCodeTurnPort::new(
                app.clone(),
                acp_pool,
                Arc::clone(&authorizations),
                app.state::<Arc<
                    crate::features::remote_control::controller::permission::PermissionAuthority,
                >>()
                .inner()
                .clone(),
                Arc::clone(&service),
            ));
        service.register_executor(
            TASK_MODE_CODE,
            Arc::new(CodeTurnExecutor::new(code_turn.clone())),
        );
        // The composition root installs DesktopTaskEventSink with the shared
        // authoritative journal before recovery or task admission.
        Arc::new(Self {
            service,
            authorizations,
            code_turn,
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

    pub(crate) fn set_code_artifact_port(
        &self,
        artifacts: Arc<dyn crate::features::codex_acp::remote_turn::RemoteCodeArtifactPort>,
    ) {
        self.code_turn.set_artifact_port(artifacts);
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

/// Capability exchange precedes A's mode choice, so it must not silently pin
/// the B session to chat. The first authenticated task claims chat/code once.
pub(crate) fn ensure_unclaimed_controlled_session(
    app: &AppHandle,
    remote_session_ref: &str,
    title: &str,
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
        .ensure_unclaimed_remote_session(remote_session_ref, title, generation)
        .map_err(|error| error.code.to_owned())?;
    if !controlled
        .live_runtime_context()
        .is_ok_and(|(lease, _)| lease.remote_control_generation == generation)
    {
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
    let lane = app
        .state::<Arc<DesktopRemoteTaskRuntime>>()
        .service()
        .lane_for_session(session_id, &lease.remote_control_generation)
        .ok_or_else(|| "task_session_not_found".to_owned())?;
    let lane = match lane {
        RemoteSessionLane::Code => TASK_MODE_CODE,
        // The B-local composer is the trusted claimant for an Unknown lane and
        // claims it as chat. Project that effective lane until an authenticated
        // A code task claims it first and emits a new projection.
        RemoteSessionLane::Chat | RemoteSessionLane::Unknown => TASK_MODE_CHAT,
    };
    app.emit(
        CONTROLLED_SESSION_EVENT,
        serde_json::json!({
            "sessionId": session_id,
            "sessionRef": remote_session_ref,
            "generation": lease.remote_control_generation,
            "lane": lane,
        }),
    )
    .map_err(|_| "controlled_session_projection_failed".to_owned())
}

/// B-side composer submission. The command resolves the trusted B mapping;
/// the shared use case builds the frozen payload and enters the same admission,
/// executor and journal as A-originated input.
pub(crate) fn trusted_local_remote_binding(
    service: &RemoteTaskService,
    authorizations: &RemoteWorkspaceAuthorizations,
    session_id: &str,
    generation: &str,
) -> Result<(String, RemoteSessionLane, Option<String>), String> {
    let (remote_ref, lane) = service
        .claim_local_session(session_id, generation)
        .map_err(|error| error.code.to_owned())?;
    let workspace_id = match lane {
        RemoteSessionLane::Code => Some(
            authorizations
                .workspace_for_session(session_id)
                .ok_or_else(|| "task_workspace_not_authorized".to_owned())?,
        ),
        RemoteSessionLane::Chat | RemoteSessionLane::Unknown => None,
    };
    Ok((remote_ref, lane, workspace_id))
}

fn resolve_local_controlled_input_for_runtime(
    runtime: &DesktopRemoteTaskRuntime,
    session_id: String,
    text: String,
    generation: String,
    skill_ids: Vec<String>,
    attachments: Vec<PublishedAttachment>,
) -> Result<LocalControlledInput, String> {
    let service = runtime.service();
    let (remote_ref, lane, workspace_id) = trusted_local_remote_binding(
        &service,
        &runtime.authorizations(),
        &session_id,
        &generation,
    )?;
    Ok(LocalControlledInput {
        remote_ref,
        generation,
        expected_session_id: session_id,
        title: String::new(),
        code_mode: lane == RemoteSessionLane::Code,
        workspace_id,
        text,
        skill_ids,
        attachments,
    })
}

#[cfg(test)]
fn resolve_local_controlled_input(
    service: &RemoteTaskService,
    session_id: String,
    text: String,
    generation: String,
) -> Result<LocalControlledInput, String> {
    let (remote_ref, lane) = service
        .claim_local_session(&session_id, &generation)
        .map_err(|error| error.code.to_owned())?;
    Ok(LocalControlledInput {
        remote_ref,
        generation,
        expected_session_id: session_id,
        title: String::new(),
        code_mode: lane == RemoteSessionLane::Code,
        workspace_id: None,
        text,
        skill_ids: Vec::new(),
        attachments: Vec::new(),
    })
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct LocalControlledAttachmentSource {
    path: String,
}

fn local_controlled_attachments(
    session_root: &std::path::Path,
    sources: Vec<LocalControlledAttachmentSource>,
) -> Result<Vec<PublishedAttachment>, String> {
    if sources.len() > 8 {
        return Err("task_attachments_invalid".to_owned());
    }
    let mut published = Vec::with_capacity(sources.len());
    for source in sources {
        let attachment = (|| {
            let source_path = crate::features::files::file_ingest::validate_path(&source.path)
                .map_err(|_| "task_attachments_invalid".to_owned())?;
            let metadata = std::fs::metadata(&source_path)
                .map_err(|_| "task_attachments_invalid".to_owned())?;
            if !metadata.is_file()
                || metadata.len() > crate::features::files::file_ingest::MAX_FILE_BYTES
            {
                return Err("task_attachments_invalid".to_owned());
            }
            let display_name = source_path
                .file_name()
                .and_then(|name| name.to_str())
                .filter(|name| !name.is_empty())
                .ok_or_else(|| "task_attachments_invalid".to_owned())?
                .to_owned();
            let handle_id = format!("local-attach-{}", Uuid::new_v4().simple());
            let directory = session_root.join("remote_uploads").join(&handle_id);
            std::fs::create_dir_all(&directory)
                .map_err(|_| "task_attachment_unavailable".to_owned())?;
            let path = directory.join(super::desktop_remote_attachment::safe_file_name(
                &display_name,
            ));
            if std::fs::copy(&source_path, &path).is_err() {
                let _ = std::fs::remove_dir_all(&directory);
                return Err("task_attachment_unavailable".to_owned());
            }
            let bytes = std::fs::read(&path).map_err(|_| {
                let _ = std::fs::remove_dir_all(&directory);
                "task_attachment_unavailable".to_owned()
            })?;
            if bytes.len() as u64 > crate::features::files::file_ingest::MAX_FILE_BYTES {
                let _ = std::fs::remove_dir_all(&directory);
                return Err("task_attachments_invalid".to_owned());
            }
            let sha256 = Sha256::digest(&bytes)
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect();
            Ok(PublishedAttachment {
                handle_id,
                path,
                display_name,
                mime_type: "application/octet-stream".to_owned(),
                bytes: bytes.len() as u64,
                sha256,
            })
        })();
        match attachment {
            Ok(attachment) => published.push(attachment),
            Err(error) => {
                drop(LocalAttachmentCleanup::new(Some(&published)));
                return Err(error);
            }
        }
    }
    Ok(published)
}

#[tauri::command]
pub async fn desktop_remote_controlled_task_send(
    app: AppHandle,
    session_id: String,
    text: String,
    skill_ids: Vec<String>,
    attachments: Vec<LocalControlledAttachmentSource>,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<Value, String> {
    ensure_supported(&controlled)?;
    let (lease, _) = controlled
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    let service = runtime.service();
    let session_root = app
        .state::<SessionStore>()
        .session_roots(&session_id)
        .map(|roots| roots.ledger)
        .map_err(|_| "task_session_not_found".to_owned())?;
    let attachments = tokio::task::spawn_blocking(move || {
        local_controlled_attachments(&session_root, attachments)
    })
    .await
    .map_err(|_| "task_attachments_invalid".to_owned())??;
    let mut attachment_cleanup = LocalAttachmentCleanup::new(Some(&attachments));
    let input = resolve_local_controlled_input_for_runtime(
        runtime.inner(),
        session_id.clone(),
        text,
        lease.remote_control_generation.clone(),
        skill_ids,
        attachments,
    )?;
    let result = app
        .state::<crate::app::desktop_remote_composition::DesktopLocalExecutionPort>()
        .submit_local(input)
        .await
        .map_err(|error| error.code().to_owned());
    let result = match result {
        Ok(result) => {
            // The backend transferred its own guard into the spawned task.
            attachment_cleanup.disarm();
            result
        }
        Err(error) => return Err(error),
    };
    let mut status = result.status;
    let task_id = status
        .get("task_id")
        .and_then(Value::as_str)
        .ok_or_else(|| "task_status_invalid".to_owned())?;
    let record = service
        .status(task_id)
        .map_err(|error| error.code.to_owned())?;
    if record.session_id != session_id
        || record.lease.remote_control_generation != lease.remote_control_generation
        || controlled
            .live_runtime_context()
            .map(|(current, _)| current != lease)
            .unwrap_or(true)
    {
        return Err("task_generation_stale".to_owned());
    }
    let model_source = RemoteTaskModelSource::from_record(&record)
        .ok_or_else(|| "task_model_source_invalid".to_owned())?;
    status["modelSource"] = serde_json::json!(model_source.as_str());
    status["generation"] = serde_json::json!(lease.remote_control_generation);
    Ok(status)
}

/// B-local task stop. The WebView chooses only a task ID; the current lease,
/// generation and B-authoritative session are verified under the lifecycle
/// owner lock before the ledger accepts cancellation. This never substitutes
/// permission denial for task cancellation.
#[tauri::command]
pub fn desktop_remote_controlled_task_stop(
    app: AppHandle,
    task_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: RuntimeState<'_>,
) -> Result<Value, String> {
    ensure_supported(&controlled)?;
    let service = runtime.service();
    let outcome = controlled
        .with_local_cancel_lease(|lease| {
            service.prepare_local_cancel(lease, &task_id, monotonic_millis())
        })
        .map_err(|error| error.code.to_owned())?
        .map_err(|error| error.code.to_owned())?;
    service.finish_cancel_outcome(&outcome);
    let (record, status) = match &outcome {
        CancelOutcome::CancelledBeforeDispatch(record) | CancelOutcome::AlreadyTerminal(record) => {
            (record, record.wire_status())
        }
        CancelOutcome::CancellationRequested(record) => (record, "cancel_requested"),
    };
    let model_source = RemoteTaskModelSource::from_record(record)
        .ok_or_else(|| "task_model_source_invalid".to_owned())?;
    let projection = serde_json::json!({
        "taskId": record.task_id,
        "sessionId": record.session_id,
        "generation": record.lease.remote_control_generation,
        "modelSource": model_source.as_str(),
        "status": status,
        "cancelRequested": record.cancel_requested,
    });
    if matches!(outcome, CancelOutcome::CancellationRequested(_)) {
        let _ = app.emit(TASK_STATUS_EVENT, &projection);
    }
    Ok(projection)
}

/// A/B 已完成各自可信映射与请求准备后共同进入的最终准入门禁。
/// 只接受准备阶段确认的精确 B Session，不再次 ensure 或改写 session_id。
pub(crate) fn submit_prepared_bound_task(
    app: &AppHandle,
    expected_session_id: &str,
    expected_remote_ref: &str,
    payload: &Value,
) -> Result<Value, String> {
    submit_prepared_bound_task_inner(app, expected_session_id, expected_remote_ref, payload, None)
}

/// B-local input has already resolved attachments on B. Their paths stay in
/// this in-process descriptor vector while the authoritative task payload
/// contains only opaque handle IDs.
pub(crate) fn submit_prepared_local_bound_task(
    app: &AppHandle,
    expected_session_id: &str,
    expected_remote_ref: &str,
    payload: &Value,
    attachments: Vec<PublishedAttachment>,
) -> Result<Value, String> {
    submit_prepared_bound_task_inner(
        app,
        expected_session_id,
        expected_remote_ref,
        payload,
        Some(attachments),
    )
}

struct LocalAttachmentCleanup(Vec<std::path::PathBuf>);

impl LocalAttachmentCleanup {
    fn new(attachments: Option<&Vec<PublishedAttachment>>) -> Self {
        Self(
            attachments
                .into_iter()
                .flatten()
                .filter_map(|attachment| {
                    let directory = attachment.path.parent()?;
                    let root = directory.parent()?;
                    (directory.file_name()?.to_str()? == attachment.handle_id
                        && root.file_name()?.to_str()? == "remote_uploads")
                        .then(|| directory.to_owned())
                })
                .collect(),
        )
    }

    fn disarm(&mut self) {
        self.0.clear();
    }
}

impl Drop for LocalAttachmentCleanup {
    fn drop(&mut self) {
        for directory in self.0.drain(..) {
            let _ = std::fs::remove_dir_all(directory);
        }
    }
}

fn submit_prepared_bound_task_inner(
    app: &AppHandle,
    expected_session_id: &str,
    expected_remote_ref: &str,
    payload: &Value,
    local_attachments: Option<Vec<PublishedAttachment>>,
) -> Result<Value, String> {
    let local_attachment_cleanup = LocalAttachmentCleanup::new(local_attachments.as_ref());
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
        local_attachments,
        local_attachment_cleanup,
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
    local_attachments: Option<Vec<PublishedAttachment>>,
    local_attachment_cleanup: LocalAttachmentCleanup,
) -> Result<Value, String> {
    let request = RemoteTaskRequest::from_payload(validated)
        .map_err(|error| error.code.to_owned())?;
    let skill_ids = request
        .skill_ids()
        .map_err(|error| error.code.to_owned())?;
    let mode = request.mode.clone();
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
        request.workspace_id.clone()
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
    // Pin one exact B-authoritative capability revision through the durable
    // ledger admission call. Snapshot refresh uses a writer token and cannot
    // replace policy in the gap between this validation and acceptance. The
    // token is released before attachment consumption or executor dispatch.
    let capability_admission = capabilities
        .pin_task_admission(
            &request.capability_revision,
            lease,
            &mode,
            &skill_ids,
            code_workspace.as_deref(),
            now_b,
        )
        .map_err(|error| error.code.to_owned())?;
    let outcome = admit_bound_task_with_ref(
        &service,
        lease,
        validated,
        now_b,
        expected_remote_ref,
        install,
    )?;
    drop(capability_admission);
    match outcome {
        AdmissionOutcome::Replay(record) => service
            .status_payload(&record.task_id)
            .map_err(|error| error.code.to_owned()),
        AdmissionOutcome::Admitted(ticket) => {
            let task_id = ticket.record.task_id.clone();
            // Once the ledger has admitted a task, every synchronous failure
            // must first publish a durable terminal and release transfer/model
            // hooks. Never use `?` to escape this branch with a live turn lock.
            let ready = (|| -> Result<_, &'static str> {
                let attachment_ids = ticket
                    .input
                    .get("attachment_ids")
                    .and_then(Value::as_array)
                    .ok_or("task_attachments_invalid")?
                    .iter()
                    .map(|id| id.as_str().map(str::to_owned))
                    .collect::<Option<Vec<_>>>()
                    .ok_or("task_attachments_invalid")?;
                // A handle is only a reference. Consume completed B receipts
                // once, under the current B lease gate, after admission.
                let attachments = if let Some(attachments) = local_attachments {
                    let exact = attachment_ids.len() == attachments.len()
                        && attachment_ids
                            .iter()
                            .zip(&attachments)
                            .all(|(id, attachment)| id == &attachment.handle_id);
                    if !exact {
                        return Err("task_attachments_invalid");
                    }
                    attachments
                } else if attachment_ids.is_empty() {
                    Vec::new()
                } else {
                    let controlled = app.state::<ControlledDeviceManager>();
                    let uploads = app.state::<
                        Arc<super::desktop_remote_attachment::DesktopRemoteAttachmentRuntime>,
                    >();
                    controlled
                        .with_local_cancel_lease(|current| {
                            if current != lease {
                                return Err("task_generation_stale".to_owned());
                            }
                            uploads.uploads().consume_completed(
                                &attachment_ids,
                                current,
                                session_id,
                                &task_id,
                            )
                        })
                        .map_err(|_| "task_generation_stale")?
                        .map_err(|_| "task_attachments_invalid")?
                };
                let projected_model_source = RemoteTaskModelSource::from_record(&ticket.record)
                    .ok_or("task_model_source_invalid")?;
                if model_source == TASK_MODEL_SOURCE_TEMPORARY {
                    temporary_model_gate(&model_source, &task_id, models)
                        .map_err(|_| "model_package_missing")?;
                }
                let task_session_id = ticket.record.session_id.clone();
                let prompt = validated
                    .get("input")
                    .and_then(|input| input.get("text"))
                    .and_then(Value::as_str)
                    .ok_or("task_request_invalid")?
                    .to_owned();
                let session_store = app.state::<SessionStore>().inner().clone();
                let baseline_messages = session_store
                    .load(&task_session_id)
                    .map_err(|_| "task_session_not_found")?
                    .messages
                    .len();
                let status = service
                    .status_payload(&task_id)
                    .map_err(|_| "task_status_invalid")?;
                let artifact_runtime = app
                    .state::<Arc<super::desktop_remote_artifact::DesktopRemoteArtifactRuntime>>()
                    .inner()
                    .clone();
                let capture_private_artifacts = ticket.record.mode == TASK_MODE_CHAT;
                if capture_private_artifacts {
                    artifact_runtime
                        .begin_task_capture(&session_store, lease, &task_session_id, &task_id)
                        .map_err(|_| "artifact_capture_failed")?;
                }
                Ok((
                    attachments,
                    projected_model_source,
                    task_session_id,
                    prompt,
                    session_store,
                    baseline_messages,
                    status,
                    artifact_runtime,
                    capture_private_artifacts,
                ))
            })();
            let (
                attachments,
                projected_model_source,
                task_session_id,
                prompt,
                session_store,
                baseline_messages,
                status,
                artifact_runtime,
                capture_private_artifacts,
            ) = match ready {
                Ok(ready) => ready,
                Err(reason) => {
                    service
                        .fail_admitted(&task_id, reason, monotonic_millis())
                        .map_err(|error| error.code.to_owned())?;
                    return service
                        .status_payload(&task_id)
                        .map_err(|error| error.code.to_owned());
                }
            };
            if model_source == TASK_MODEL_SOURCE_TEMPORARY {
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "task_temporary_model_installed",
                    "ready",
                );
            }
            // The persisted ledger intentionally excludes prompt text. Project
            // it once to B's local UI only after admission so the controlled
            // device can visibly confirm what it is executing.
            if !prompt.is_empty() {
                let _ = app.emit(
                    TASK_STATUS_EVENT,
                    serde_json::json!({
                        "taskId": &task_id,
                        "sessionId": session_id,
                        "generation": &lease.remote_control_generation,
                        "modelSource": projected_model_source.as_str(),
                        "status": "admitted",
                        "text": &prompt,
                        "messageIndex": baseline_messages,
                    }),
                );
            }
            let events: Arc<dyn ControllerTaskEventSink> =
                Arc::new(TaskEventBridge { app: app.clone() });
            let artifact_events = Arc::clone(&events);
            let artifact_store = session_store.clone();
            let artifact_lease = lease.clone();
            let artifact_session_id = task_session_id.clone();
            let artifact_task_id = task_id.clone();
            let dispatch_service = Arc::clone(&service);
            let local_attachment_cleanup = local_attachment_cleanup;
            crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                "task_execution_scheduled",
                "admitted",
            );
            // Publish every admitted projection before dispatch can synchronously
            // advance the ledger to running on the worker thread.
            let _ = app.emit(TASK_EVENT, &status);
            tauri::async_runtime::spawn(async move {
                let _local_attachment_cleanup = local_attachment_cleanup;
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "task_execution_started",
                    "running",
                );
                match dispatch_service
                    .dispatch_with_attachments(
                        ticket,
                        attachments,
                        events,
                        CancellationToken::new(),
                        monotonic_millis(),
                    )
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
                if capture_private_artifacts {
                    match artifact_runtime.finish_task_capture(
                        &artifact_store,
                        &artifact_lease,
                        &artifact_session_id,
                        &artifact_task_id,
                    ) {
                        Ok(descriptors) => {
                            for descriptor in descriptors {
                                artifact_events.emit_journal(
                                    crate::features::remote_control::controller::journal::JournalKind::Artifact,
                                    &artifact_session_id,
                                    Some(&artifact_task_id),
                                    json!({
                                        "artifact_id": descriptor.artifact_id,
                                        "display_name": descriptor.display_name,
                                        "mime_type": descriptor.mime_type,
                                        "total_bytes": descriptor.total_bytes,
                                        "sha256": descriptor.sha256,
                                        "preview_kind": descriptor.preview_kind,
                                    }),
                                );
                            }
                        }
                        Err(error) => {
                            eprintln!(
                                "[remote-control] failed to capture remote task artifacts: {error}"
                            );
                        }
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

    fn validate_completed_uploads(
        &self,
        local_session_id: &str,
        remote_ref: &str,
        live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
        task_id: &str,
        attachment_ids: &[String],
    ) -> Result<(), String> {
        self.app
            .state::<Arc<super::desktop_remote_action::DesktopRemoteActionState>>()
            .validate_completed_uploads(local_session_id, remote_ref, live, task_id, attachment_ids)
            .map_err(str::to_owned)
    }
}

fn dispatch_error(code: &'static str) -> UseCaseError {
    UseCaseError::new(code, false)
}

pub(super) fn lease_matches_remote_binding(
    lease: &LeaseContext,
    live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
) -> bool {
    lease.connection_id == live.connection_id
        && lease.controller_lease_id == live.controller_lease_id
        && lease.lease_epoch == live.lease_epoch
        && lease.relay_epoch == live.relay_epoch
        && lease.remote_control_generation == live.generation
}

pub(super) fn with_current_dispatch_binding<T>(
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
            "remote_attachment_unconfirmed"
            | "remote_attachment_count_invalid"
            | "remote_attachment_duplicate"
            | "remote_attachment_binding_mismatch" => {
                dispatch_error("remote_attachment_unconfirmed")
            }
            "remote_task_not_cancellable" => dispatch_error("remote_task_not_cancellable"),
            _ => dispatch_error("remote_binding_mismatch"),
        })
}

impl RemoteTaskDispatchPort for DesktopRemoteTaskDispatchPort {
    fn send_bound_cancel(
        &self,
        store: &SessionStore,
        local_session_id: &str,
        remote_ref: &str,
        live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
        task_id: &str,
    ) -> Result<String, UseCaseError> {
        let manager = self.app.state::<ControllingDeviceManager>();
        let sessions = self.app.state::<ControllingSessionManager>();
        let (lease, _, _) = manager
            .authenticated_remote_proxy_identity()
            .map_err(|_| dispatch_error("remote_binding_mismatch"))?;
        let session_id = with_current_dispatch_binding(&self.app, live, || {
            store
                .require_attempted_remote_task(local_session_id, remote_ref, live, task_id)
                .map_err(|_| "remote_task_not_cancellable".to_owned())?;
            let session = sessions
                .state(remote_ref, &lease)
                .map_err(|_| "remote_binding_mismatch".to_owned())?;
            Ok(session.session_id)
        })?;
        let request_id = format!("request-{}", Uuid::new_v4().simple());
        let payload = build_bound_task_cancel_payload(&lease, &session_id, &request_id, task_id)
            .map_err(|_| dispatch_error("remote_request_invalid"))?;
        self.send_encrypted(&payload, live)?;
        Ok(request_id)
    }

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
        attachment_ids: &[String],
    ) -> Result<(), UseCaseError> {
        with_current_dispatch_binding(&self.app, live, || {
            if !attachment_ids.is_empty() {
                self.validate_completed_uploads(
                    local_session_id,
                    remote_ref,
                    live,
                    &handle.task_id,
                    attachment_ids,
                )?;
            }
            store
                .mark_remote_dispatch_attempt(
                    local_session_id,
                    remote_ref,
                    live,
                    &handle.client_request_id,
                    &handle.task_id,
                    &handle.request_fingerprint,
                )
                .map_err(|_| "remote_attempt_persist_failed".to_owned())?;
            if !attachment_ids.is_empty() {
                self.app
                    .state::<Arc<super::desktop_remote_action::DesktopRemoteActionState>>()
                    .retire_uploaded_receipts(
                        local_session_id,
                        remote_ref,
                        live,
                        &handle.task_id,
                        attachment_ids,
                    );
            }
            Ok(())
        })
    }

    fn replace_prepared_fingerprint(
        &self,
        store: &SessionStore,
        local_session_id: &str,
        remote_ref: &str,
        live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
        handle: &RemoteRetryHandle,
        attachment_ids: &[String],
        new_fingerprint: &str,
    ) -> Result<(), UseCaseError> {
        with_current_dispatch_binding(&self.app, live, || {
            self.validate_completed_uploads(
                local_session_id,
                remote_ref,
                live,
                &handle.task_id,
                attachment_ids,
            )?;
            store
                .replace_prepared_remote_fingerprint(
                    local_session_id,
                    remote_ref,
                    live,
                    &handle.client_request_id,
                    &handle.task_id,
                    &handle.request_fingerprint,
                    new_fingerprint,
                )
                .map_err(|_| "remote_pending_persist_failed".to_owned())
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
        let capability = sessions
            .capability_projection(&semantics.remote_session_ref, &lease)
            .map_err(|_| dispatch_error("capability_snapshot_stale"))?;
        let valid_mode = capability
            .modes
            .iter()
            .any(|available| available == &semantics.mode);
        let valid_workspace = match semantics.mode.as_str() {
            TASK_MODE_CHAT => semantics.workspace_ids.is_empty(),
            TASK_MODE_CODE => match semantics.workspace_ids.as_slice() {
                [workspace_id] => capability.workspaces.iter().any(|workspace| {
                    workspace.code_allowed && workspace.workspace_id == *workspace_id
                }),
                _ => false,
            },
            _ => false,
        };
        let valid_skills =
            capability_allows_skills(&capability, &semantics.mode, &semantics.skill_ids);
        if session.session_id != semantics.session_id
            || session.capability_revision != Some(semantics.capability_revision)
            || capability.revision() != semantics.capability_revision
            || !semantics.attachment_ids.is_empty()
            || !valid_mode
            || !valid_workspace
            || !valid_skills
        {
            return Err(dispatch_error("remote_request_invalid"));
        }
        let temporary = match semantics.model_source.as_str() {
            TASK_MODEL_SOURCE_CONTROLLED if semantics.model_profile_id.is_none() => None,
            TASK_MODEL_SOURCE_TEMPORARY
                if semantics.mode == TASK_MODE_CHAT && session.temporary_model_available =>
            {
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
        let mut input = input;
        if let [workspace_id] = semantics.workspace_ids.as_slice() {
            input["workspace_id"] = json!(workspace_id);
        }
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

    fn send_encrypted(
        &self,
        payload: &Value,
        live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
    ) -> Result<(), UseCaseError> {
        let manager = self.app.state::<ControllingDeviceManager>();
        let business = self.app.state::<ControllingLaneState>();
        let identity = manager
            .authenticated_remote_proxy_identity()
            .map_err(|_| dispatch_error("remote_binding_mismatch"))?;
        if !lease_matches_remote_binding(&identity.0, live)
            || identity.1 != live.peer_id
            || identity.2 != live.resume_boundary_id
        {
            return Err(dispatch_error("remote_binding_mismatch"));
        }
        let (lease, capabilities) = manager
            .live_runtime_context()
            .map_err(|_| dispatch_error("controller_not_connected"))?;
        if lease != identity.0 {
            return Err(dispatch_error("remote_binding_mismatch"));
        }
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
        let kind = payload
            .get("type")
            .and_then(Value::as_str)
            .filter(|kind| matches!(*kind, "task_submit" | "task_cancel"))
            .ok_or_else(|| dispatch_error("remote_request_invalid"))?;
        let frame = business
            .seal(kind, payload, &capabilities)
            .map_err(|_| dispatch_error("remote_encrypt_failed"))?;
        manager
            .send_authenticated_business_for_identity(&identity, &frame)
            .map_err(|_| dispatch_error("remote_send_uncertain"))
    }
}

/// New unregistered A command boundary. The trusted live context comes from
/// SessionStore, never from UI-supplied lease fields. The returned status and
/// IDs are non-sensitive; no temporary package is projected to WebView.
#[derive(Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub(crate) enum RemoteTaskReserveIntent {
    New {
        text: String,
        mode: String,
        #[serde(rename = "workspaceId")]
        workspace_id: Option<String>,
        #[serde(rename = "skillIds", default)]
        skill_ids: Vec<String>,
        #[serde(rename = "modelSource")]
        model_source: String,
        #[serde(rename = "selectedModelId")]
        selected_model_id: Option<String>,
    },
}

#[tauri::command]
pub fn desktop_remote_task_reserve_a(
    service: State<'_, RemoteDispatchService>,
    store: State<'_, SessionStore>,
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    local_session_id: String,
    remote_ref: String,
    intent: RemoteTaskReserveIntent,
) -> Result<RemoteDispatchReport, String> {
    if !manager.is_supported() {
        return Err(UNSUPPORTED.to_owned());
    }
    let (text, mode, workspace_id, skill_ids, model_source, selected_model_id) = match intent {
        RemoteTaskReserveIntent::New {
            text,
            mode,
            workspace_id,
            skill_ids,
            model_source,
            selected_model_id,
        } => (
            text,
            mode,
            workspace_id,
            skill_ids,
            model_source,
            selected_model_id,
        ),
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
    let capability = sessions
        .capability_projection(&remote_ref, &lease)
        .map_err(|error| match error.code {
            "capability_snapshot_expired" | "session_context_stale" => {
                "capability_snapshot_stale".to_owned()
            }
            code => code.to_owned(),
        })?;
    if !capability.modes.iter().any(|available| available == &mode) {
        return Err("task_mode_unsupported".to_owned());
    }
    let workspace_ids =
        match mode.as_str() {
            TASK_MODE_CHAT if workspace_id.is_none() => Vec::new(),
            TASK_MODE_CODE => {
                let workspace_id = workspace_id
                    .as_deref()
                    .ok_or_else(|| "task_workspace_required".to_owned())?;
                if !capability.workspaces.iter().any(|workspace| {
                    workspace.code_allowed && workspace.workspace_id == workspace_id
                }) {
                    return Err("capability_workspace_not_authorized".to_owned());
                }
                vec![workspace_id.to_owned()]
            }
            _ => return Err("task_workspace_unexpected".to_owned()),
        };
    if !capability_allows_skills(&capability, &mode, &skill_ids) {
        return Err("task_skill_invalid".to_owned());
    }
    match model_source.as_str() {
        TASK_MODEL_SOURCE_CONTROLLED if selected_model_id.is_none() => {}
        TASK_MODEL_SOURCE_TEMPORARY
            if mode == TASK_MODE_CHAT
                && session.temporary_model_available
                && selected_model_id.as_deref().is_some_and(
                    crate::features::sessions::remote_proxy::schema::valid_remote_identifier,
                ) => {}
        _ => return Err("remote_model_source_invalid".to_owned()),
    }
    // Fresh A-authenticated capability is a prerequisite for durable reserve.
    // Commit takes only the frozen handle and never re-reads mutable settings.
    let capability_revision = capability.revision();
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
            mode,
            model_source,
            model_profile_id: selected_model_id,
            model_execution_digest: String::new(),
            input_digest: remote_task_input_digest(&text, &[], &skill_ids),
            text,
            attachment_ids: Vec::new(),
            skill_ids,
            workspace_ids,
        },
    };
    service
        .reserve_new(request)
        .map_err(|error| error.code().to_owned())
}

/// A separate, explicit irreversible action. A successful reserve has already
/// persisted PreparedNeverAttempted; this command alone may cross the durable
/// attempt barrier and send. Attachment IDs are B-authenticated upload receipts
/// verified by the host port before they enter the final fingerprint.
#[tauri::command]
pub fn desktop_remote_task_commit_a(
    service: State<'_, RemoteDispatchService>,
    local_session_id: String,
    remote_ref: String,
    handle: RemoteRetryHandle,
    attachment_ids: Vec<String>,
) -> Result<RemoteDispatchReport, String> {
    service
        .commit_prepared(&local_session_id, &remote_ref, handle, &attachment_ids)
        .map_err(|error| error.code().to_owned())
}

#[tauri::command]
pub fn desktop_remote_task_cancel_a(
    service: State<'_, RemoteDispatchService>,
    local_session_id: String,
    remote_ref: String,
    task_id: String,
) -> Result<RemoteCancelReport, String> {
    service
        .cancel_task(&local_session_id, &remote_ref, &task_id)
        .map_err(|error| error.code().to_owned())
}
#[tauri::command]
pub fn desktop_remote_task_abandon_prepared_a(
    app: AppHandle,
    service: State<'_, RemoteDispatchService>,
    local_session_id: String,
    client_request_id: String,
    task_id: String,
) -> Result<(), String> {
    service
        .abandon_prepared_remote_task(&local_session_id, &client_request_id, &task_id)
        .map_err(|error| error.code().to_owned())?;
    app.state::<Arc<super::desktop_remote_action::DesktopRemoteActionState>>()
        .discard_task_receipts(&local_session_id, &task_id);
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_capability_selection_preserves_chat_and_code_skill_scopes() {
        use crate::features::remote_control::controller::controlling_session::{
            ControllerCapabilityProjection, ControllerSkillDescriptor,
        };

        let capability = ControllerCapabilityProjection {
            remote_session_ref: "session-ref-1".into(),
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-1".into(),
            remote_control_generation: "generation-1".into(),
            capability_revision: 1,
            expires_at_ms: u64::MAX,
            modes: vec![TASK_MODE_CHAT.into(), TASK_MODE_CODE.into()],
            tools: Vec::new(),
            skills: vec![
                ControllerSkillDescriptor {
                    skill_id: "chat-only".into(),
                    display_name: "Chat".into(),
                    scopes: vec![TASK_MODE_CHAT.into()],
                },
                ControllerSkillDescriptor {
                    skill_id: "shared".into(),
                    display_name: "Shared".into(),
                    scopes: vec![TASK_MODE_CHAT.into(), TASK_MODE_CODE.into()],
                },
            ],
            workspaces: Vec::new(),
        };
        assert!(capability_allows_skills(
            &capability,
            TASK_MODE_CHAT,
            &["chat-only".into(), "shared".into()],
        ));
        assert!(capability_allows_skills(
            &capability,
            TASK_MODE_CODE,
            &["shared".into()],
        ));
        assert!(!capability_allows_skills(
            &capability,
            TASK_MODE_CODE,
            &["chat-only".into()],
        ));
    }

    #[test]
    fn b_local_chat_and_code_inputs_keep_paths_out_of_authoritative_payloads() {
        let path = std::env::temp_dir().join(format!(
            "pinvou-controlled-local-{}.txt",
            Uuid::new_v4().simple()
        ));
        let session_root = std::env::temp_dir().join(format!(
            "pinvou-controlled-session-{}",
            Uuid::new_v4().simple()
        ));
        std::fs::create_dir_all(&session_root).unwrap();
        std::fs::write(&path, b"local attachment body").unwrap();
        let attachments = local_controlled_attachments(
            &session_root,
            vec![LocalControlledAttachmentSource {
                path: path.to_string_lossy().into_owned(),
            }],
        )
        .unwrap();
        assert_eq!(attachments.len(), 1);
        assert!(attachments[0].handle_id.starts_with("local-attach-"));
        assert!(
            attachments[0]
                .path
                .starts_with(session_root.join("remote_uploads"))
        );
        let attachment_ids = vec![attachments[0].handle_id.clone()];

        for (mode, workspace_id, skill_ids) in [
            (TASK_MODE_CHAT, None, Vec::<String>::new()),
            (
                TASK_MODE_CODE,
                Some("workspace-1"),
                vec!["skill-1".to_owned()],
            ),
        ] {
            let mut input = json!({
                "text": "process the attachment",
                "attachment_ids": attachment_ids,
                "skill_ids": skill_ids,
            });
            if let Some(workspace_id) = workspace_id {
                input["workspace_id"] = json!(workspace_id);
            }
            let payload = json!({
                "connection_id": "connection-1",
                "controller_lease_id": "lease-1",
                "lease_epoch": "epoch-1",
                "relay_epoch": "relay-1",
                "remote_control_generation": "generation-1",
                "task_id": format!("task-{mode}"),
                "client_request_id": format!("request-{mode}"),
                "session_id": "session-1",
                "capability_revision": "1",
                "model_source": TASK_MODEL_SOURCE_CONTROLLED,
                "mode": mode,
                "input_digest": crate::features::sessions::remote_proxy::outbox::remote_task_input_digest(
                    "process the attachment",
                    &attachment_ids,
                    input["skill_ids"].as_array().unwrap().iter().map(|id| id.as_str().unwrap().to_owned()).collect::<Vec<_>>().as_slice(),
                ),
                "input": input,
            });
            let request = RemoteTaskRequest::from_payload(&payload).unwrap();
            assert_eq!(request.input["attachment_ids"], json!(attachment_ids));
            assert_eq!(
                request.input["skill_ids"].as_array().unwrap().is_empty(),
                mode == TASK_MODE_CHAT
            );
            assert!(
                !serde_json::to_string(&payload)
                    .unwrap()
                    .contains(path.to_string_lossy().as_ref())
            );
        }
        let _ = std::fs::remove_file(path);
        let _ = std::fs::remove_dir_all(session_root);
    }

    #[test]
    fn reserve_intent_rejects_retry_and_ui_supplied_task_identity() {
        let handle = json!({
            "taskId": "task-1",
            "clientRequestId": "request-1",
            "requestFingerprint": "a".repeat(64),
        });
        assert!(
            serde_json::from_value::<RemoteTaskReserveIntent>(json!({
                "kind": "retry", "handle": handle,
            }))
            .is_err()
        );
    }

    #[test]
    fn new_intent_accepts_only_narrow_model_selection() {
        let intent = serde_json::from_value::<RemoteTaskReserveIntent>(json!({
            "kind": "new",
            "text": "hello",
            "mode": "chat",
            "workspaceId": null,
            "skillIds": [],
            "modelSource": "controller_temporary",
            "selectedModelId": "model-1",
        }))
        .unwrap();
        assert!(matches!(intent, RemoteTaskReserveIntent::New {
            model_source, selected_model_id: Some(id), ..
        } if model_source == "controller_temporary" && id == "model-1"));
        assert!(
            serde_json::from_value::<RemoteTaskReserveIntent>(json!({
                "kind": "new", "text": "hello", "mode": "chat", "workspaceId": null,
                "skillIds": [], "modelSource": "controller_temporary",
                "selectedModelId": "model-1", "taskId": "ui-forged",
            }))
            .is_err()
        );
    }

    #[test]
    fn new_reserve_checks_authenticated_capability_before_outbox_reservation() {
        let source = include_str!("desktop_remote_task.rs");
        let start = source
            .find("pub fn desktop_remote_task_reserve_a(")
            .unwrap();
        let end = source[start..]
            .find("pub fn desktop_remote_task_abandon_prepared_a(")
            .map(|offset| start + offset)
            .unwrap();
        let command = &source[start..end];
        let fresh = command.find(".capability_projection(").unwrap();
        let reserve = command.find(".reserve_new(request)").unwrap();
        assert!(fresh < reserve);
        assert!(command.contains("capability_snapshot_stale"));
    }

    #[test]
    fn b_final_admission_pins_capability_until_the_ledger_decides() {
        let source = include_str!("desktop_remote_task.rs");
        let start = source.find("fn admit_and_dispatch(").unwrap();
        let end = source[start..]
            .find("pub(crate) fn cancel_decrypted_task")
            .map(|offset| start + offset)
            .unwrap();
        let admission = &source[start..end];
        let pin = admission.find(".pin_task_admission(").unwrap();
        let ledger = admission.find("admit_bound_task_with_ref(").unwrap();
        let release = admission.find("drop(capability_admission)").unwrap();
        let attachment_consume = admission.find("consume_completed(").unwrap();
        assert!(pin < ledger && ledger < release && release < attachment_consume);
        for authority in [
            "&request.capability_revision",
            "lease,",
            "&mode,",
            "&skill_ids,",
            "code_workspace.as_deref()",
            "now_b,",
        ] {
            assert!(admission[pin..ledger].contains(authority), "{authority}");
        }
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
            generation: "generation-0001".into(),
            model_source: RemoteTaskModelSource::ControllerTemporary,
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
        assert!(encoded.contains("controller_temporary"));
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
