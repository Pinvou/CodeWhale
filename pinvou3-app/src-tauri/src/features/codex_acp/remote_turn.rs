//! DEV-13 adapter: code-mode remote turns over the ACP pool.
//!
//! A remote code task never carries a host path. The A-side workspace field is
//! an opaque ID that must be registered here by the command layer *after* the
//! DEV-12 capability snapshot and the B workspace owner both accepted it. The
//! ACP session keeps running in its own preauthorized execution workspace, and
//! this adapter only submits the prompt text.

use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::time::Duration;
use tokio::time::Instant;

use async_trait::async_trait;
use parking_lot::{Mutex, RwLock};
use serde_json::{Value, json};
use tauri::{Listener as _, Manager as _};
use tokio_util::sync::CancellationToken;

use super::{AcpPool, AcpSession, RemotePermissionOwner};
use crate::features::remote_control::controller::journal::{
    JournalKind, StreamingTextSanitizer, sanitize_text,
};
use crate::features::remote_control::controller::permission::PermissionAuthority;
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::task::{
    ControllerTaskEventSink, PublishedAttachment, RemoteCodeTurnPort, RemoteTaskService,
    RemoteTaskState, TASK_CANCELLED_CODE, TaskLeaseBinding,
};
use crate::features::remote_control::controller::transfer::download::ArtifactDescriptor;
use crate::features::remote_control::controller::turn_budget::TurnBudget;

const POLL_INTERVAL: Duration = Duration::from_millis(200);
const BUSY_GRACE: Duration = Duration::from_secs(5);
const MAX_AUTHORIZED_REFS: usize = 64;
const REMOTE_PERMISSION_TTL_MS: u64 = 5 * 60 * 1000;

/// Tauri-independent boundary for turning files created by a successful ACP
/// turn into remote artifacts. Implementations must copy eligible project
/// files into B's private session storage before indexing them; the authorized
/// project path itself must never become an artifact source or wire value.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct RemoteCodeCaptureToken {
    pub(crate) task_sequence: u64,
    execution_id: uuid::Uuid,
}

impl RemoteCodeCaptureToken {
    pub(crate) fn new(task_sequence: u64) -> Self {
        Self {
            task_sequence,
            execution_id: uuid::Uuid::new_v4(),
        }
    }
}

pub(crate) trait RemoteCodeArtifactPort: Send + Sync {
    fn begin_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        workspace: &std::path::Path,
        task_sequence: u64,
    ) -> std::result::Result<RemoteCodeCaptureToken, String>;

    fn finish_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        events: Arc<dyn ControllerTaskEventSink>,
        token: &RemoteCodeCaptureToken,
        publication_allowed: &(dyn Fn() -> Result<(), String> + Sync),
    ) -> std::result::Result<Vec<ArtifactDescriptor>, String>;

    fn discard_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        token: &RemoteCodeCaptureToken,
    );
}

/// Panic-safe ownership of a live inventory capture. Once `begin` succeeds,
/// every unwind and every early return discards it. A successful staging call
/// removes the capture itself, so this drop then becomes an idempotent no-op.
struct CodeCaptureGuard {
    token: RemoteCodeCaptureToken,
    artifacts: Arc<dyn RemoteCodeArtifactPort>,
    lease: LeaseContext,
    session_id: String,
    task_id: String,
}

impl CodeCaptureGuard {
    fn begin(
        artifacts: Arc<dyn RemoteCodeArtifactPort>,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        workspace: &std::path::Path,
        task_sequence: u64,
    ) -> Result<Self, String> {
        // Allocate guard ownership before creating the capture. After begin
        // succeeds, constructing Self is allocation-free, closing the small
        // unwind window between resource acquisition and RAII ownership.
        let lease = lease.clone();
        let session_id = session_id.to_owned();
        let task_id = task_id.to_owned();
        let token = artifacts.begin_code_capture(
            &lease,
            &session_id,
            &task_id,
            workspace,
            task_sequence,
        )?;
        Ok(Self {
            token,
            artifacts,
            lease,
            session_id,
            task_id,
        })
    }
}

impl Drop for CodeCaptureGuard {
    fn drop(&mut self) {
        self.artifacts.discard_code_capture(
            &self.lease,
            &self.session_id,
            &self.task_id,
            &self.token,
        );
    }
}

#[derive(Default)]
struct AcpTurnRelayState {
    turn_id: Option<String>,
    last_seq: u64,
    assistant: StreamingTextSanitizer,
    reasoning: StreamingTextSanitizer,
    tools: HashMap<String, String>,
    terminal: Option<(String, Option<String>)>,
    stopped: bool,
}

struct AcpTurnEventRelay {
    app: tauri::AppHandle,
    listener_id: Option<u32>,
    state: Arc<Mutex<AcpTurnRelayState>>,
    events: Arc<dyn ControllerTaskEventSink>,
    session_id: String,
    task_id: String,
}

impl AcpTurnEventRelay {
    fn start(
        app: &tauri::AppHandle,
        session_id: &str,
        task_id: &str,
        turn_id: &str,
        events: Arc<dyn ControllerTaskEventSink>,
    ) -> Self {
        let state = Arc::new(Mutex::new(AcpTurnRelayState {
            turn_id: Some(turn_id.to_owned()),
            ..AcpTurnRelayState::default()
        }));
        let listener_state = Arc::clone(&state);
        let listener_events = Arc::clone(&events);
        let expected_session = session_id.to_owned();
        let listener_session = expected_session.clone();
        let listener_task = task_id.to_owned();
        let listener_id = app.listen("acp:event", move |event| {
            let Ok(envelope) = serde_json::from_str::<super::AcpEventEnvelope>(event.payload())
            else {
                return;
            };
            if envelope.session_id != listener_session {
                return;
            }
            relay_acp_event(
                &listener_state,
                &listener_events,
                &listener_session,
                &listener_task,
                &envelope,
            );
        });
        Self {
            app: app.clone(),
            listener_id: Some(listener_id),
            state,
            events,
            session_id: expected_session,
            task_id: task_id.to_owned(),
        }
    }

    fn finish(&mut self, completed: bool) -> Option<String> {
        if let Some(listener_id) = self.listener_id.take() {
            self.app.unlisten(listener_id);
        }
        let mut state = self.state.lock();
        if state.stopped {
            return terminal_result(&state).err();
        }
        state.stopped = true;
        let (assistant_tail, reasoning_tail) = close_acp_pending(&mut state, completed);
        let terminal = state.terminal.clone();
        if !reasoning_tail.is_empty() {
            self.events.emit_journal(
                JournalKind::Reasoning,
                &self.session_id,
                Some(&self.task_id),
                json!({ "text": reasoning_tail }),
            );
        }
        if !assistant_tail.is_empty() {
            self.events.emit_journal(
                JournalKind::AssistantDelta,
                &self.session_id,
                Some(&self.task_id),
                json!({ "text": assistant_tail }),
            );
        }
        acp_terminal_result(terminal.as_ref())
    }
}

impl Drop for AcpTurnEventRelay {
    fn drop(&mut self) {
        let _ = self.finish(false);
    }
}

fn acp_terminal_error((status, _message): &(String, Option<String>)) -> Option<String> {
    match status.to_ascii_lowercase().as_str() {
        "completed" => None,
        "interrupted" => Some(TASK_CANCELLED_CODE.to_owned()),
        "limitreached" => Some("task_turn_limit_reached".to_owned()),
        "refused" => Some("task_turn_refused".to_owned()),
        _ => Some("task_engine_failed".to_owned()),
    }
}

fn acp_terminal_result(terminal: Option<&(String, Option<String>)>) -> Option<String> {
    terminal.map_or_else(
        || Some("task_terminal_unavailable".to_owned()),
        acp_terminal_error,
    )
}

fn terminal_result(state: &AcpTurnRelayState) -> Result<(), String> {
    let terminal = state
        .terminal
        .as_ref()
        .ok_or_else(|| "task_turn_terminal_missing".to_owned())?;
    acp_terminal_error(terminal).map_or(Ok(()), Err)
}

#[cfg(test)]
fn artifact_journal_payload(descriptor: ArtifactDescriptor) -> Value {
    json!({
        "artifact_id": descriptor.artifact_id,
        "display_name": descriptor.display_name,
        "mime_type": descriptor.mime_type,
        "total_bytes": descriptor.total_bytes,
        "sha256": descriptor.sha256,
        "preview_kind": descriptor.preview_kind,
    })
}

fn protocol_text(data: &Value) -> Option<&str> {
    let content = data.get("update")?.get("content")?;
    content
        .as_str()
        .or_else(|| content.get("text").and_then(Value::as_str))
}

fn close_acp_pending(state: &mut AcpTurnRelayState, confirmed: bool) -> (String, String) {
    let completed = confirmed && terminal_result(state).is_ok();
    let assistant = if completed {
        state.assistant.finish_completed()
    } else {
        state.assistant.abort_pending()
    };
    let reasoning = if completed {
        state.reasoning.finish_completed()
    } else {
        state.reasoning.abort_pending()
    };
    (assistant, reasoning)
}

fn finish_acp_streams(state: &mut AcpTurnRelayState, projected: &mut Vec<(JournalKind, Value)>) {
    let (assistant, reasoning) = close_acp_pending(state, true);
    if !reasoning.is_empty() {
        projected.push((JournalKind::Reasoning, json!({ "text": reasoning })));
    }
    if !assistant.is_empty() {
        projected.push((JournalKind::AssistantDelta, json!({ "text": assistant })));
    }
}

// The production sink only journals/enqueues and emits distinct remote events.
// Deliver under the close lock: no prepared callback batch escapes after close.
fn relay_acp_event(
    state: &Mutex<AcpTurnRelayState>,
    events: &Arc<dyn ControllerTaskEventSink>,
    session_id: &str,
    task_id: &str,
    envelope: &super::AcpEventEnvelope,
) {
    let mut state = state.lock();
    for (kind, payload) in project_acp_event(&mut state, envelope) {
        events.emit_journal(kind, session_id, Some(task_id), payload);
    }
}

fn project_acp_event(
    state: &mut AcpTurnRelayState,
    envelope: &super::AcpEventEnvelope,
) -> Vec<(JournalKind, Value)> {
    if state.stopped || state.terminal.is_some() || envelope.seq <= state.last_seq {
        return Vec::new();
    }
    if envelope.event.event_type == "turn_started" {
        if state.turn_id.is_some() && state.turn_id == envelope.turn_id {
            state.last_seq = envelope.seq;
        }
        return Vec::new();
    }
    if envelope.turn_id.as_deref() != state.turn_id.as_deref() || state.turn_id.is_none() {
        return Vec::new();
    }
    state.last_seq = envelope.seq;
    let mut projected = Vec::new();
    match envelope.event.event_type.as_str() {
        "agent_message_chunk" => {
            if let Some(text) = protocol_text(&envelope.event.data) {
                let text = state.assistant.push(text);
                if !text.is_empty() {
                    projected.push((JournalKind::AssistantDelta, json!({ "text": text })));
                }
            }
        }
        "agent_thought_chunk" => {
            if let Some(text) = protocol_text(&envelope.event.data) {
                let text = state.reasoning.push(text);
                if !text.is_empty() {
                    projected.push((JournalKind::Reasoning, json!({ "text": text })));
                }
            }
        }
        "tool_call" | "tool_call_update" => {
            let update = envelope.event.data.get("update").unwrap_or(&Value::Null);
            let Some(tool_id) = update.get("toolCallId").and_then(Value::as_str) else {
                return projected;
            };
            if let Some(title) = update.get("title").and_then(Value::as_str) {
                state.tools.insert(tool_id.to_owned(), title.to_owned());
            }
            let tool_name = state
                .tools
                .get(tool_id)
                .map(String::as_str)
                .unwrap_or("tool");
            let tool_state = update.get("status").and_then(Value::as_str).unwrap_or(
                if envelope.event.event_type == "tool_call" {
                    "running"
                } else {
                    "updated"
                },
            );
            let text = update
                .get("content")
                .and_then(|content| {
                    content
                        .as_str()
                        .or_else(|| content.get("text").and_then(Value::as_str))
                })
                .map(sanitize_text);
            // `rawInput`, `rawOutput`, locations and metadata are deliberately
            // excluded: they can contain credentials and B-local paths.
            projected.push((
                JournalKind::Tool,
                json!({
                    "tool_id": tool_id,
                    "tool_name": tool_name,
                    "tool_state": tool_state,
                    "text": text,
                }),
            ));
        }
        "turn_completed" => {
            let status = envelope.event.data.get("status").and_then(Value::as_str);
            let message = envelope
                .event
                .data
                .get("error")
                .or_else(|| envelope.event.data.get("message"))
                .and_then(Value::as_str)
                .map(sanitize_text);
            state.terminal = Some((status.unwrap_or("Failed").to_owned(), message));
            finish_acp_streams(state, &mut projected);
        }
        _ => {}
    }
    projected
}

/// Captured synchronously by the ACP admission path. Never look up the
/// session's CURRENT turn/runtime when this old remote task later expires.
pub(super) struct OwnedAcpTurn {
    runtime: Arc<AcpSession>,
    turn_id: String,
}

impl OwnedAcpTurn {
    pub(super) fn new(runtime: Arc<AcpSession>, turn_id: String) -> Self {
        Self { runtime, turn_id }
    }

    pub(super) fn is_active(&self) -> bool {
        self.runtime.bridge.current_turn_id().as_deref() == Some(self.turn_id.as_str())
    }

    pub(super) async fn retire(&self, pool: &AcpPool, reason: &str) {
        // Fence synchronously before even scheduling potentially blocked
        // cleanup; a newer turn on a reused Arc must remain untouched.
        if !self.runtime.seal_remote_turn(&self.turn_id, reason) {
            return;
        }
        let pool = pool.clone();
        let runtime = self.runtime.clone();
        crate::features::remote_control::controller::turn_budget::settle_owned(async move {
            pool.finish_remote_retirement(runtime).await;
        })
        .await;
    }
}

/// Bounded registry of opaque `(session, workspace)` authorizations. Entries
/// are written only by the command layer and dropped when the connection ends.
#[derive(Default)]
pub(crate) struct RemoteWorkspaceAuthorizations {
    entries: Mutex<HashSet<(String, String, String)>>,
}

impl RemoteWorkspaceAuthorizations {
    pub(crate) fn authorize(&self, session_id: &str, workspace_id: &str, generation: &str) -> bool {
        let mut entries = self.entries.lock();
        if entries.iter().any(|(session, workspace, owner)| {
            session == session_id && (workspace != workspace_id || owner != generation)
        }) {
            return false;
        }
        if entries.len() >= MAX_AUTHORIZED_REFS
            && !entries.contains(&(
                session_id.to_owned(),
                workspace_id.to_owned(),
                generation.to_owned(),
            ))
        {
            return false;
        }
        entries.insert((
            session_id.to_owned(),
            workspace_id.to_owned(),
            generation.to_owned(),
        ));
        true
    }

    pub(crate) fn is_authorized(
        &self,
        session_id: &str,
        workspace_id: &str,
        generation: &str,
    ) -> bool {
        self.entries.lock().contains(&(
            session_id.to_owned(),
            workspace_id.to_owned(),
            generation.to_owned(),
        ))
    }

    pub(crate) fn workspace_for_session(
        &self,
        session_id: &str,
        generation: &str,
    ) -> Option<String> {
        let entries = self.entries.lock();
        let mut matches = entries
            .iter()
            .filter(|(candidate, _, owner)| candidate == session_id && owner == generation)
            .map(|(_, workspace, _)| workspace.clone());
        let workspace = matches.next()?;
        // A code Session is bound to one project. Ambiguity must fail closed.
        matches.next().is_none().then_some(workspace)
    }

    pub(crate) fn revoke_session(&self, session_id: &str) {
        self.entries
            .lock()
            .retain(|(candidate, _, _)| candidate != session_id);
    }

    pub(crate) fn clear_generation(&self, generation: &str) {
        self.entries
            .lock()
            .retain(|(_, _, owner)| owner != generation);
    }

    pub(crate) fn clear(&self) {
        self.entries.lock().clear();
    }
}

pub(crate) struct AcpPoolCodeTurnPort {
    app: tauri::AppHandle,
    pool: Arc<AcpPool>,
    authorizations: Arc<RemoteWorkspaceAuthorizations>,
    permission_authority: Arc<PermissionAuthority>,
    tasks: Arc<RemoteTaskService>,
    artifacts: RwLock<Option<Arc<dyn RemoteCodeArtifactPort>>>,
}

impl AcpPoolCodeTurnPort {
    pub(crate) fn new(
        app: tauri::AppHandle,
        pool: Arc<AcpPool>,
        authorizations: Arc<RemoteWorkspaceAuthorizations>,
        permission_authority: Arc<PermissionAuthority>,
        tasks: Arc<RemoteTaskService>,
    ) -> Self {
        Self {
            app,
            pool,
            authorizations,
            permission_authority,
            tasks,
            artifacts: RwLock::new(None),
        }
    }

    pub(crate) fn set_artifact_port(&self, artifacts: Arc<dyn RemoteCodeArtifactPort>) {
        *self.artifacts.write() = Some(artifacts);
    }

    async fn sync_remote_permissions(
        &self,
        owner: &RemotePermissionOwner,
        registered: &mut HashMap<String, String>,
        resolved: &mut HashSet<String>,
        events: &Arc<dyn ControllerTaskEventSink>,
    ) -> std::result::Result<(), String> {
        let pending = self.pool.pending_remote_permissions(owner).await;
        let current = pending
            .iter()
            .map(|request| request.callback_id.clone())
            .collect::<HashSet<_>>();
        let now =
            crate::features::remote_control::controller::relay_runtime::controlled_monotonic_millis(
            );
        self.permission_authority
            .sweep_expired(self.permission_authority.now_ms());
        for request in pending {
            if let Some(approval_id) = registered.get(&request.callback_id) {
                if request.approval_id.as_deref() != Some(approval_id) {
                    return Err("permission_binding_stale".to_owned());
                }
                let view = self
                    .permission_authority
                    .view(approval_id)
                    .map_err(|error| error.code.to_owned())?;
                let decision = match view.state.as_str() {
                    "allowed" => Some(true),
                    "denied" | "expired" => Some(false),
                    _ => None,
                };
                if let Some(allow) = decision {
                    let resolution = self
                        .pool
                        .respond_remote_permission(owner, approval_id, allow)
                        .await;
                    if resolution.is_err()
                        && self
                            .pool
                            .pending_remote_permissions(owner)
                            .await
                            .iter()
                            .any(|pending| pending.approval_id.as_deref() == Some(approval_id))
                    {
                        return Err("permission_dispatch_failed".to_owned());
                    }
                }
                continue;
            }
            let view = self
                .pool
                .mint_remote_permission(
                    owner,
                    &request.callback_id,
                    &self.permission_authority,
                    &self.tasks.ledger(),
                    REMOTE_PERMISSION_TTL_MS,
                )
                .await
                .map_err(|_| "permission_binding_stale".to_owned())?;
            registered.insert(
                request.callback_id.clone(),
                view.permission_request_id.clone(),
            );
            let record = self
                .tasks
                .status(&owner.task_id)
                .map_err(|error| error.code.to_owned())?;
            if record.sequence != owner.task_sequence || record.session_id != owner.session_id
                || record.lease != crate::features::remote_control::controller::task::TaskLeaseBinding::from_context(&owner.lease)
            {
                return Err("permission_binding_stale".to_owned());
            }
            if record.state == RemoteTaskState::Running {
                self.tasks
                    .enter_waiting_permission(&owner.task_id, owner.task_sequence, now)
                    .map_err(|error| error.code.to_owned())?;
            } else if record.state != RemoteTaskState::WaitingPermission {
                return Err("task_state_conflict".to_owned());
            }
            events.emit_journal(
                JournalKind::Permission,
                &owner.session_id,
                Some(&owner.task_id),
                serde_json::json!({
                    "status": "pending",
                    "operation_type": view.operation_type,
                    "risk_level": view.risk_level,
                    "target_scope": view.target_scope,
                }),
            );
        }

        for (callback_id, request_id) in registered.iter().filter(|(id, _)| !current.contains(*id))
        {
            if !resolved.insert(callback_id.clone()) {
                continue;
            }
            let view = self
                .permission_authority
                .view(request_id)
                .map_err(|error| error.code.to_owned())?;
            events.emit_journal(
                JournalKind::Permission,
                &owner.session_id,
                Some(&owner.task_id),
                serde_json::json!({
                    "status": view.state,
                    "operation_type": view.operation_type,
                    "risk_level": view.risk_level,
                    "target_scope": view.target_scope,
                    "decision": view.decision,
                }),
            );
            let record = self
                .tasks
                .status(&owner.task_id)
                .map_err(|error| error.code.to_owned())?;
            if record.state == RemoteTaskState::WaitingPermission {
                self.tasks.resume_running(&owner.task_id, owner.task_sequence,
                    crate::features::remote_control::controller::relay_runtime::controlled_monotonic_millis())
                    .map_err(|error| error.code.to_owned())?;
            }
        }
        Ok(())
    }
}

#[async_trait]
impl RemoteCodeTurnPort for AcpPoolCodeTurnPort {
    async fn run_code_turn_with_attachments(
        &self,
        session_id: &str,
        task_id: &str,
        lease: &LeaseContext,
        workspace_id: &str,
        content: String,
        skill_ids: Vec<String>,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String> {
        let budget = TurnBudget::current_or_new()?;
        if cancel.is_cancelled() {
            return Err(TASK_CANCELLED_CODE.to_owned());
        }
        if !self.authorizations.is_authorized(
            session_id,
            workspace_id,
            &lease.remote_control_generation,
        ) {
            return Err("task_workspace_not_authorized".to_owned());
        }
        let record = self
            .tasks
            .status(task_id)
            .map_err(|_| "task_binding_stale".to_owned())?;
        if record.is_terminal()
            || record.session_id != session_id
            || record.lease != TaskLeaseBinding::from_context(lease)
        {
            return Err("task_binding_stale".to_owned());
        }
        if cancel.is_cancelled() {
            return Err(TASK_CANCELLED_CODE.to_owned());
        }
        let authorized_path = self
            .app
            .state::<crate::features::remote_control::controller::controlled::ControlledDeviceManager>()
            .resolve_admitted_code_workspace(lease, workspace_id)
            .map_err(|_| "task_workspace_not_authorized".to_owned())?;
        let workspace = self
            .pool
            .workspace_info(session_id)
            .map_err(|_| "task_session_backend_unavailable".to_owned())?;
        if workspace.workspace_kind != super::CodexWorkspaceKind::Project
            || std::path::Path::new(&workspace.workspace_path) != authorized_path
        {
            return Err("task_workspace_binding_stale".to_owned());
        }
        let artifact_port = self
            .artifacts
            .read()
            .clone()
            .ok_or_else(|| "artifact_capture_unavailable".to_owned())?;
        let owner = RemotePermissionOwner {
            task_sequence: record.sequence,
            session_id: session_id.to_owned(),
            task_id: task_id.to_owned(),
            lease: lease.clone(),
        };
        // Reserve the actual idle runtime before expensive preflight. This is
        // not yet a permission owner; Drop rolls back every failure/yield path.
        let reservation = self.pool.reserve_remote_turn(owner.clone());
        let mut reservation = budget
            .run(&cancel, async {
                reservation
                    .await
                    .map_err(|_| "task_session_backend_unavailable".to_owned())
            })
            .await?;
        let skill_path = authorized_path.clone();
        let private_skill_prompt = budget
            .run(&cancel, async {
                tokio::task::spawn_blocking(move || {
                    crate::features::assistant::skill_materialization::remote_turn_skill_prompt(
                        crate::features::marketplace::ConnectorScope::Code,
                        Some(&skill_path),
                        &skill_ids,
                    )
                })
                .await
                .map_err(|_| "task_skill_unavailable".to_owned())?
            })
            .await?;
        let actual_content = if private_skill_prompt.is_empty() {
            content.clone()
        } else {
            format!("{private_skill_prompt}\n<user_request>\n{content}\n</user_request>")
        };
        let _attachment_read_owners: Vec<_> = attachments
            .iter()
            .filter_map(|attachment| attachment.read_owner.clone())
            .collect();
        let attachments = if attachments.is_empty() {
            Vec::new()
        } else {
            let ledger_root = self
                .pool
                .session_store
                .session_roots(session_id)
                .map_err(|_| "task_session_not_found".to_owned())?
                .ledger;
            budget
                .run(&cancel, async {
                    tokio::task::spawn_blocking(move || {
                        attachments
                            .into_iter()
                            .map(|attachment| {
                                crate::features::files::file_ingest::ingest_staged_attachment(
                                    &ledger_root,
                                    &attachment,
                                )
                            })
                            .collect::<Result<Vec<_>, _>>()
                    })
                    .await
                    .map_err(|_| "task_attachment_unavailable".to_owned())?
                })
                .await?
        };
        // Inventory starts only after every fallible preflight that does not
        // need a capture (skills, attachment ingestion, permission ownership)
        // has succeeded. From here onward Drop guarantees capture cleanup.
        let capture_port = artifact_port.clone();
        let capture_lease = lease.clone();
        let capture_session = session_id.to_owned();
        let capture_task = task_id.to_owned();
        let capture = budget
            .run(&cancel, async {
                tokio::task::spawn_blocking(move || {
                    CodeCaptureGuard::begin(
                        capture_port,
                        &capture_lease,
                        &capture_session,
                        &capture_task,
                        &authorized_path,
                        record.sequence,
                    )
                })
                .await
                .map_err(|_| "artifact_capture_unavailable".to_owned())?
            })
            .await?;

        // Subscribe before prompt submission so the authoritative journal
        // cannot miss the first real ACP turn event.
        let mut turn_relay = AcpTurnEventRelay::start(
            &self.app,
            session_id,
            task_id,
            reservation.turn_id(),
            Arc::clone(&events),
        );

        let mut result = budget
            .run(&cancel, async {
                reservation
                    .submit(&actual_content, &content, &attachments, &cancel, budget)
                    .map_err(|_| {
                        if cancel.is_cancelled() {
                            TASK_CANCELLED_CODE.to_owned()
                        } else {
                            "task_engine_unavailable".to_owned()
                        }
                    })?;
                let mut registered = HashMap::new();
                let mut resolved = HashSet::new();
                let mut missing_terminal_since = None;
                loop {
                    if turn_relay.state.lock().terminal.is_some() {
                        return terminal_result(&turn_relay.state.lock());
                    }
                    self.sync_remote_permissions(&owner, &mut registered, &mut resolved, &events)
                        .await?;
                    if reservation.is_active() {
                        missing_terminal_since = None;
                    } else {
                        let since = missing_terminal_since.get_or_insert_with(Instant::now);
                        if since.elapsed() >= BUSY_GRACE {
                            return Err("task_turn_terminal_missing".to_owned());
                        }
                    }
                    tokio::select! {
                        _ = cancel.cancelled() => return Err(TASK_CANCELLED_CODE.to_owned()),
                        _ = tokio::time::sleep(POLL_INTERVAL) => {}
                    }
                }
            })
            .await;
        if let Err(error) = &result {
            reservation.retire(error).await;
        }
        let terminal_error = turn_relay.finish(result.is_ok());
        if result.is_ok() {
            if let Some(error) = terminal_error {
                result = Err(error);
            }
        }
        if result.is_ok() {
            let artifact_lease = lease.clone();
            let artifact_session = session_id.to_owned();
            let artifact_task = task_id.to_owned();
            let artifact_events = events.clone();
            let artifact_cancel = cancel.clone();
            result = budget
                .run(&cancel, async {
                    tokio::task::spawn_blocking(move || {
                        // The worker owns capture cleanup even if its waiter times
                        // out; publication rechecks the same frozen deadline.
                        let _capture = capture;
                        artifact_port
                            .finish_code_capture(
                                &artifact_lease,
                                &artifact_session,
                                &artifact_task,
                                artifact_events,
                                &_capture.token,
                                &|| budget.checkpoint(&artifact_cancel),
                            )
                            .map(|_| ())
                    })
                    .await
                    .map_err(|_| "artifact_capture_unavailable".to_owned())?
                })
                .await;
        } else {
            crate::features::remote_control::controller::turn_budget::settle_owned(async move {
                // Drop may wait for the exact capture's publication lock.
                tokio::task::spawn_blocking(move || drop(capture))
                    .await
                    .ok();
            })
            .await;
        }
        let dangling = budget
            .run(&cancel, async {
                Ok(self.pool.pending_remote_permissions(&owner).await)
            })
            .await
            .unwrap_or_default();
        // A late old port must not deny a reused task ID's new approvals.
        // If cleanup already owns the gate, its terminal hook supplies this
        // denial; a missing/stale record grants no authority over a new owner.
        let _ =
            self.tasks
                .ledger()
                .with_task_resources(&owner.task_id, owner.task_sequence, || {
                    self.permission_authority.deny_all_for_task(
                        &owner.task_id,
                        "task_terminal",
                        self.permission_authority.now_ms(),
                    )
                });
        if !dangling.is_empty() {
            for request in dangling {
                events.emit_journal(
                    JournalKind::Permission,
                    &owner.session_id,
                    Some(&owner.task_id),
                    serde_json::json!({
                        "status": "denied",
                        "operation_type": request.projection.operation_type,
                        "risk_level": request.projection.risk_level,
                        "target_scope": request.projection.target_scope,
                        "decision": "deny",
                    }),
                );
            }
        }
        if result.is_ok() {
            result = budget.checkpoint(&cancel);
        }
        if let Err(error) = &result {
            reservation.retire(error).await;
        }
        settle_turn_result(result, budget, &cancel, reservation.release()).await
    }
}

/// Cleanup ownership survives its bounded wait, but that wait grants no new
/// execution time. A late success must recheck the originally admitted budget.
async fn settle_turn_result(
    result: Result<(), String>,
    budget: TurnBudget,
    cancel: &CancellationToken,
    cleanup: impl std::future::Future<Output = ()>,
) -> Result<(), String> {
    cleanup.await;
    result.and_then(|()| budget.checkpoint(cancel))
}

/// Poll interval exported for the wiring layer and tests.
pub(crate) const REMOTE_CODE_TURN_POLL_INTERVAL: Duration = POLL_INTERVAL;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::transfer::download::PreviewKind;
    use std::sync::atomic::{AtomicUsize, Ordering};

    #[tokio::test(start_paused = true)]
    async fn exact_cleanup_cannot_extend_the_frozen_deadline_or_hide_cancellation() {
        use crate::features::remote_control::controller::turn_budget::{
            TURN_TIMEOUT_CODE, settle_owned,
        };
        let budget = TurnBudget::for_test(Duration::from_secs(2));
        let cancel = CancellationToken::new();
        let completed = Arc::new(AtomicUsize::new(0));
        let observed = completed.clone();
        let result = settle_turn_result(
            Ok(()),
            budget,
            &cancel,
            settle_owned(async move {
                tokio::time::sleep(Duration::from_secs(3)).await;
                observed.fetch_add(1, Ordering::SeqCst);
            }),
        )
        .await;
        assert_eq!(result.unwrap_err(), TURN_TIMEOUT_CODE);
        assert_eq!(completed.load(Ordering::SeqCst), 1);

        let budget = TurnBudget::for_test(Duration::from_secs(30));
        let cancelled = cancel.clone();
        let result = settle_turn_result(
            Ok(()),
            budget,
            &cancel,
            settle_owned(async move {
                tokio::time::sleep(Duration::from_secs(1)).await;
                cancelled.cancel();
            }),
        )
        .await;
        assert_eq!(result.unwrap_err(), TASK_CANCELLED_CODE);
    }

    #[derive(Default)]
    struct CaptureProbe {
        begins: AtomicUsize,
        discards: AtomicUsize,
    }

    impl RemoteCodeArtifactPort for CaptureProbe {
        fn begin_code_capture(
            &self,
            _lease: &LeaseContext,
            _session_id: &str,
            _task_id: &str,
            _workspace: &std::path::Path,
            task_sequence: u64,
        ) -> Result<RemoteCodeCaptureToken, String> {
            self.begins.fetch_add(1, Ordering::SeqCst);
            Ok(RemoteCodeCaptureToken::new(task_sequence))
        }

        fn finish_code_capture(
            &self,
            _lease: &LeaseContext,
            _session_id: &str,
            _task_id: &str,
            _events: Arc<dyn ControllerTaskEventSink>,
            _token: &RemoteCodeCaptureToken,
            publication_allowed: &(dyn Fn() -> Result<(), String> + Sync),
        ) -> Result<Vec<ArtifactDescriptor>, String> {
            publication_allowed()?;
            Ok(Vec::new())
        }

        fn discard_code_capture(
            &self,
            _lease: &LeaseContext,
            _session_id: &str,
            _task_id: &str,
            _token: &RemoteCodeCaptureToken,
        ) {
            self.discards.fetch_add(1, Ordering::SeqCst);
        }
    }

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-1".into(),
            remote_control_generation: "generation-1".into(),
        }
    }

    fn begin_after_preflight(
        probe: Arc<CaptureProbe>,
        preflight: Result<(), String>,
    ) -> Result<CodeCaptureGuard, String> {
        preflight?;
        let artifacts: Arc<dyn RemoteCodeArtifactPort> = probe;
        CodeCaptureGuard::begin(
            artifacts,
            &lease(),
            "session-1",
            "task-1",
            std::path::Path::new("."),
            1,
        )
    }

    #[test]
    fn skill_attachment_and_permission_preflight_failures_never_start_a_capture() {
        for code in [
            "task_skills_invalid",
            "task_attachment_unavailable",
            "remote_permission_owner_conflict",
        ] {
            let probe = Arc::new(CaptureProbe::default());
            assert_eq!(
                begin_after_preflight(Arc::clone(&probe), Err(code.to_owned()))
                    .err()
                    .as_deref(),
                Some(code)
            );
            assert_eq!(probe.begins.load(Ordering::SeqCst), 0);
            assert_eq!(probe.discards.load(Ordering::SeqCst), 0);
        }
    }

    #[test]
    fn live_capture_is_discarded_on_normal_return_and_unwind() {
        let probe = Arc::new(CaptureProbe::default());
        drop(begin_after_preflight(Arc::clone(&probe), Ok(())).unwrap());
        assert_eq!(probe.begins.load(Ordering::SeqCst), 1);
        assert_eq!(probe.discards.load(Ordering::SeqCst), 1);

        let unwind_probe = Arc::clone(&probe);
        let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(move || {
            let _capture = begin_after_preflight(unwind_probe, Ok(())).unwrap();
            panic!("exercise guard unwind");
        }));
        assert_eq!(probe.begins.load(Ordering::SeqCst), 2);
        assert_eq!(probe.discards.load(Ordering::SeqCst), 2);
    }

    fn envelope(
        seq: u64,
        turn_id: &str,
        event_type: &str,
        data: Value,
    ) -> super::super::AcpEventEnvelope {
        serde_json::from_value(json!({
            "version": 1,
            "sessionId": "session-0001",
            "turnId": turn_id,
            "seq": seq,
            "timestamp": "2026-09-28T00:00:00Z",
            "event": { "type": event_type, "data": data },
        }))
        .unwrap()
    }

    #[test]
    fn acp_pending_blocks_survive_tools_and_channel_interleaving() {
        let mut state = AcpTurnRelayState {
            turn_id: Some("turn-1".into()),
            ..AcpTurnRelayState::default()
        };
        assert!(
            project_acp_event(
                &mut state,
                &envelope(
                    1,
                    "turn-1",
                    "agent_message_chunk",
                    json!({"update":{"content":{"text":"目录"}}})
                )
            )
            .is_empty()
        );
        assert!(
            project_acp_event(
                &mut state,
                &envelope(
                    2,
                    "turn-1",
                    "agent_thought_chunk",
                    json!({"update":{"content":{"text":"推理目录"}}})
                )
            )
            .is_empty()
        );
        let tool = project_acp_event(
            &mut state,
            &envelope(
                3,
                "turn-1",
                "tool_call",
                json!({"update":{"toolCallId":"tool-1","title":"read_file","status":"in_progress"}}),
            ),
        );
        assert_eq!(tool.len(), 1);
        assert_eq!(tool[0].0, JournalKind::Tool);
        let message = project_acp_event(
            &mut state,
            &envelope(
                4,
                "turn-1",
                "agent_message_chunk",
                json!({"update":{"content":{"text":"/秘密/file.txt "}}}),
            ),
        );
        assert_eq!(
            message,
            vec![(JournalKind::AssistantDelta, json!({"text":"[PATH] "}))]
        );
        let thought = project_acp_event(
            &mut state,
            &envelope(
                5,
                "turn-1",
                "agent_thought_chunk",
                json!({"update":{"content":{"text":"/秘密/file.txt "}}}),
            ),
        );
        assert_eq!(
            thought,
            vec![(JournalKind::Reasoning, json!({"text":"[PATH] "}))]
        );

        let mut state = AcpTurnRelayState {
            turn_id: Some("turn-1".into()),
            ..AcpTurnRelayState::default()
        };
        let first = project_acp_event(
            &mut state,
            &envelope(
                1,
                "turn-1",
                "agent_message_chunk",
                json!({"update":{"content":{"text":"Authorization "}}}),
            ),
        );
        assert_eq!(
            first,
            vec![(
                JournalKind::AssistantDelta,
                json!({"text":"Authorization "})
            )]
        );
        let tool = project_acp_event(
            &mut state,
            &envelope(
                2,
                "turn-1",
                "tool_call_update",
                json!({"update":{"toolCallId":"tool-1","status":"completed"}}),
            ),
        );
        assert_eq!(tool.len(), 1);
        assert_eq!(tool[0].0, JournalKind::Tool);
        let next = project_acp_event(
            &mut state,
            &envelope(
                3,
                "turn-1",
                "agent_message_chunk",
                json!({"update":{"content":{"text":"= 中文口令 multi words "}}}),
            ),
        );
        assert!(!serde_json::to_string(&next[0].1).unwrap().contains("口令"));
        assert!(next.iter().all(|(_, payload)| {
            payload["text"]
                .as_str()
                .is_some_and(|text| !text.contains("multi") && !text.contains("words"))
        }));
    }

    #[test]
    fn acp_only_completed_releases_pending_tail_and_terminal_fences_late_chunks() {
        for status in [Some("Completed"), Some("Interrupted"), Some("Failed"), None] {
            let mut state = AcpTurnRelayState {
                turn_id: Some("turn-1".into()),
                ..AcpTurnRelayState::default()
            };
            assert!(
                project_acp_event(
                    &mut state,
                    &envelope(
                        1,
                        "turn-1",
                        "agent_message_chunk",
                        json!({"update":{"content":{"text":"普通正文"}}})
                    )
                )
                .is_empty()
            );
            assert!(
                project_acp_event(
                    &mut state,
                    &envelope(
                        2,
                        "turn-1",
                        "agent_thought_chunk",
                        json!({"update":{"content":{"text":"普通推理"}}})
                    )
                )
                .is_empty()
            );
            let terminal = project_acp_event(
                &mut state,
                &envelope(3, "turn-1", "turn_completed", json!({"status":status})),
            );
            let successful = status == Some("Completed");
            assert_eq!(
                terminal,
                vec![
                    (
                        JournalKind::Reasoning,
                        json!({"text":if successful {"普通推理"} else {"[REDACTED]"}})
                    ),
                    (
                        JournalKind::AssistantDelta,
                        json!({"text":if successful {"普通正文"} else {"[REDACTED]"}})
                    ),
                ]
            );
            assert!(
                project_acp_event(
                    &mut state,
                    &envelope(
                        4,
                        "turn-1",
                        "agent_message_chunk",
                        json!({"update":{"content":{"text":"late private data "}}})
                    )
                )
                .is_empty()
            );
            assert_eq!(state.last_seq, 3);
            assert_eq!(
                close_acp_pending(&mut state, false),
                (String::new(), String::new())
            );
        }
    }

    #[test]
    fn acp_abort_path_never_infers_success_from_observed_terminal() {
        let mut state = AcpTurnRelayState {
            turn_id: Some("turn-1".into()),
            ..AcpTurnRelayState::default()
        };
        assert!(state.assistant.push("目录").is_empty());
        assert!(state.reasoning.push("推理目录").is_empty());
        // Even a recorded Completed does not authorize a Drop/error caller
        // to finish the ambiguous block as ordinary source text.
        state.terminal = Some(("Completed".into(), None));
        assert_eq!(
            close_acp_pending(&mut state, false),
            ("[REDACTED]".into(), "[REDACTED]".into())
        );
        assert_eq!(
            close_acp_pending(&mut state, false),
            (String::new(), String::new())
        );
    }

    #[test]
    fn acp_delivery_cannot_be_overtaken_by_close_and_late_tools_are_rejected() {
        struct BarrierSink {
            prepared: std::sync::mpsc::Sender<()>,
            release: Mutex<std::sync::mpsc::Receiver<()>>,
            delivered: Mutex<Vec<(JournalKind, Value)>>,
        }
        impl ControllerTaskEventSink for BarrierSink {
            fn emit(&self, _: Value) {}
            fn emit_journal(&self, kind: JournalKind, _: &str, _: Option<&str>, payload: Value) {
                self.prepared.send(()).unwrap();
                self.release
                    .lock()
                    .recv_timeout(Duration::from_secs(5))
                    .unwrap();
                self.delivered.lock().push((kind, payload));
            }
        }
        let (prepared_tx, prepared_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::sync_channel(1);
        let sink = Arc::new(BarrierSink {
            prepared: prepared_tx,
            release: Mutex::new(release_rx),
            delivered: Mutex::new(Vec::new()),
        });
        let events: Arc<dyn ControllerTaskEventSink> = sink.clone();
        let state = Arc::new(Mutex::new(AcpTurnRelayState {
            turn_id: Some("turn-1".into()),
            ..AcpTurnRelayState::default()
        }));
        let worker_state = Arc::clone(&state);
        let worker_events = Arc::clone(&events);
        let worker = std::thread::spawn(move || {
            relay_acp_event(
                &worker_state,
                &worker_events,
                "session-0001",
                "task-1",
                &envelope(
                    1,
                    "turn-1",
                    "agent_message_chunk",
                    json!({"update":{"content":{"text":"safe "}}}),
                ),
            );
        });
        prepared_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(state.try_lock().is_none());
        assert!(sink.delivered.lock().is_empty());
        release_tx.send(()).unwrap();
        worker.join().unwrap();
        state.lock().stopped = true;
        for (seq, event_type) in [
            (2, "tool_call"),
            (3, "tool_call_update"),
            (4, "agent_message_chunk"),
        ] {
            relay_acp_event(
                &state,
                &events,
                "session-0001",
                "task-1",
                &envelope(
                    seq,
                    "turn-1",
                    event_type,
                    json!({"update":{"toolCallId":"tool-1","title":"read_file","content":{"text":"late "}}}),
                ),
            );
        }
        assert_eq!(state.lock().last_seq, 1);
        assert_eq!(
            sink.delivered.lock().as_slice(),
            &[(JournalKind::AssistantDelta, json!({"text":"safe "}))]
        );
    }

    #[test]
    fn acp_projection_uses_real_order_and_never_exports_raw_tool_fields() {
        let mut state = AcpTurnRelayState {
            turn_id: Some("turn-1".into()),
            ..AcpTurnRelayState::default()
        };
        assert!(
            project_acp_event(
                &mut state,
                &envelope(1, "turn-1", "turn_started", json!({ "status": "running" }))
            )
            .is_empty()
        );

        let assistant = project_acp_event(
            &mut state,
            &envelope(
                2,
                "turn-1",
                "agent_message_chunk",
                json!({ "update": { "content": { "type": "text", "text": "hello " } } }),
            ),
        );
        assert_eq!(assistant[0].0, JournalKind::AssistantDelta);
        assert_eq!(assistant[0].1["text"], "hello ");

        let tool = project_acp_event(
            &mut state,
            &envelope(
                3,
                "turn-1",
                "tool_call",
                json!({
                    "update": {
                        "toolCallId": "tool-1",
                        "title": "read_file",
                        "status": "in_progress",
                        "content": { "text": "visible result" },
                        "rawInput": { "token": "must-not-cross" },
                        "rawOutput": { "path": "C:\\Users\\private" }
                    }
                }),
            ),
        );
        assert_eq!(tool.last().unwrap().0, JournalKind::Tool);
        let serialized = serde_json::to_string(&tool).unwrap();
        assert!(serialized.contains("visible result"));
        assert!(!serialized.contains("must-not-cross"));
        assert!(!serialized.contains("Users"));

        // Native app-event retries and unrelated turns cannot duplicate or
        // contaminate the authoritative remote journal.
        assert!(
            project_acp_event(&mut state, &envelope(3, "turn-1", "tool_call", json!({})))
                .is_empty()
        );
        assert!(
            project_acp_event(
                &mut state,
                &envelope(
                    4,
                    "turn-foreign",
                    "agent_message_chunk",
                    json!({ "update": { "content": { "text": "foreign" } } })
                )
            )
            .is_empty()
        );
    }

    #[test]
    fn acp_terminal_status_drives_the_remote_task_outcome() {
        let mut state = AcpTurnRelayState {
            turn_id: Some("turn-1".into()),
            ..AcpTurnRelayState::default()
        };
        project_acp_event(
            &mut state,
            &envelope(1, "turn-1", "turn_started", json!({})),
        );
        project_acp_event(
            &mut state,
            &envelope(
                2,
                "turn-1",
                "turn_completed",
                json!({ "status": "Refused", "error": "provider refused" }),
            ),
        );
        assert_eq!(
            state.terminal.as_ref().and_then(acp_terminal_error),
            Some("task_turn_refused".to_owned())
        );
    }

    #[test]
    fn relay_never_claims_a_foreign_first_start_and_requires_authoritative_terminal() {
        let mut state = AcpTurnRelayState {
            turn_id: Some("remote-turn-owned".into()),
            ..AcpTurnRelayState::default()
        };
        project_acp_event(
            &mut state,
            &envelope(100, "local-old", "turn_started", json!({})),
        );
        project_acp_event(
            &mut state,
            &envelope(
                101,
                "local-old",
                "turn_completed",
                json!({"status":"Completed"}),
            ),
        );
        assert_eq!(state.turn_id.as_deref(), Some("remote-turn-owned"));
        assert_eq!(
            state.last_seq, 0,
            "foreign sequence must not suppress actual runtime events"
        );
        assert_eq!(
            acp_terminal_result(state.terminal.as_ref()),
            Some("task_terminal_unavailable".into())
        );
        assert_eq!(
            terminal_result(&state).unwrap_err(),
            "task_turn_terminal_missing"
        );
        project_acp_event(
            &mut state,
            &envelope(1, "remote-turn-owned", "turn_started", json!({})),
        );
        project_acp_event(
            &mut state,
            &envelope(
                2,
                "remote-turn-owned",
                "turn_completed",
                json!({"status":"Completed"}),
            ),
        );
        assert_eq!(acp_terminal_result(state.terminal.as_ref()), None);
        assert!(terminal_result(&state).is_ok());
        let mut unbound = AcpTurnRelayState::default();
        project_acp_event(
            &mut unbound,
            &envelope(1, "local-old", "turn_started", json!({})),
        );
        assert!(unbound.turn_id.is_none(), "no auto-claim fallback");
    }

    #[test]
    fn artifact_journal_projection_contains_metadata_but_never_a_host_path() {
        let private_path = r"C:\Users\controlled\private-project\report.md";
        let payload = artifact_journal_payload(ArtifactDescriptor {
            artifact_id: "artifact-0001".into(),
            task_id: "task-0001".into(),
            display_name: "report.md".into(),
            mime_type: "text/plain".into(),
            total_bytes: 12,
            sha256: "a".repeat(64),
            preview_kind: PreviewKind::Text,
        });
        let encoded = serde_json::to_string(&payload).unwrap();
        assert!(encoded.contains("artifact-0001"));
        assert!(encoded.contains("report.md"));
        assert!(!encoded.contains(private_path));
        assert!(payload.get("path").is_none());
    }

    #[test]
    fn authorization_registry_is_bounded_and_session_scoped() {
        let registry = RemoteWorkspaceAuthorizations::default();
        assert!(registry.authorize("session-0001", "workspace-0001", "generation-1"));
        assert!(registry.is_authorized("session-0001", "workspace-0001", "generation-1"));
        assert_eq!(
            registry
                .workspace_for_session("session-0001", "generation-1")
                .as_deref(),
            Some("workspace-0001")
        );
        assert!(!registry.is_authorized("session-0002", "workspace-0001", "generation-1"));
        assert!(!registry.is_authorized("session-0001", "workspace-0002", "generation-1"));
        assert!(!registry.is_authorized("session-0001", "workspace-0001", "generation-2"));

        assert!(!registry.authorize("session-0001", "workspace-0002", "generation-1"));
        assert!(!registry.authorize("session-0001", "workspace-0001", "generation-2"));
        assert_eq!(
            registry
                .workspace_for_session("session-0001", "generation-1")
                .as_deref(),
            Some("workspace-0001"),
            "a code session remains bound to its first authenticated project"
        );

        registry.revoke_session("session-0001");
        assert!(!registry.is_authorized("session-0001", "workspace-0001", "generation-1"));
    }

    #[test]
    fn authorization_registry_rejects_growth_past_the_cap() {
        let registry = RemoteWorkspaceAuthorizations::default();
        for index in 0..MAX_AUTHORIZED_REFS {
            assert!(registry.authorize(
                &format!("session-{index:04}"),
                &format!("workspace-{index:04}"),
                "generation-1"
            ));
        }
        assert!(!registry.authorize("session-overflow", "workspace-overflow", "generation-1"));
        // Re-authorizing an existing pair stays idempotent at the cap.
        assert!(registry.authorize("session-0000", "workspace-0000", "generation-1"));
        registry.clear();
        assert!(registry.authorize("session-0001", "workspace-overflow", "generation-1"));
    }

    #[test]
    fn generation_cleanup_reclaims_capacity_without_revoking_a_replacement() {
        let registry = RemoteWorkspaceAuthorizations::default();
        assert!(registry.authorize("replacement", "workspace-new", "generation-new"));
        for index in 0..MAX_AUTHORIZED_REFS - 1 {
            assert!(registry.authorize(&format!("old-{index}"), "workspace-old", "generation-old"));
        }
        assert!(!registry.authorize("overflow", "workspace-old", "generation-old"));
        registry.clear_generation("generation-old");
        assert!(registry.is_authorized("replacement", "workspace-new", "generation-new"));
        for index in 0..MAX_AUTHORIZED_REFS - 1 {
            assert!(!registry.is_authorized(
                &format!("old-{index}"),
                "workspace-old",
                "generation-old"
            ));
            assert!(registry.authorize(
                &format!("next-{index}"),
                "workspace-next",
                "generation-next"
            ));
        }
        registry.revoke_session("replacement");
        assert!(registry.authorize("after-close", "workspace-close", "generation-next"));
    }
}
