//! DEV-13 adapter: code-mode remote turns over the ACP pool.
//!
//! A remote code task never carries a host path. The A-side workspace field is
//! an opaque ID that must be registered here by the command layer *after* the
//! DEV-12 capability snapshot and the B workspace owner both accepted it. The
//! ACP session keeps running in its own preauthorized execution workspace, and
//! this adapter only submits the prompt text.

use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::time::{Duration, Instant};

use async_trait::async_trait;
use parking_lot::{Mutex, RwLock};
use serde_json::{Value, json};
use tauri::{Listener as _, Manager as _};
use tokio_util::sync::CancellationToken;

use super::{AcpPool, RemotePermissionOwner};
use crate::features::remote_control::controller::journal::{
    JournalKind, StreamingTextSanitizer, sanitize_text,
};
use crate::features::remote_control::controller::permission::PermissionAuthority;
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::task::{
    ControllerTaskEventSink, PublishedAttachment, RemoteCodeTurnPort, RemoteTaskService,
    RemoteTaskState, TASK_CANCELLED_CODE,
};
use crate::features::remote_control::controller::transfer::download::ArtifactDescriptor;

const POLL_INTERVAL: Duration = Duration::from_millis(200);
const BUSY_GRACE: Duration = Duration::from_secs(5);
const MAX_AUTHORIZED_REFS: usize = 64;
const REMOTE_PERMISSION_TTL_MS: u64 = 5 * 60 * 1000;

/// Tauri-independent boundary for turning files created by a successful ACP
/// turn into remote artifacts. Implementations must copy eligible project
/// files into B's private session storage before indexing them; the authorized
/// project path itself must never become an artifact source or wire value.
pub(crate) trait RemoteCodeArtifactPort: Send + Sync {
    fn begin_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        workspace: &std::path::Path,
    ) -> std::result::Result<(), String>;

    fn finish_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        events: Arc<dyn ControllerTaskEventSink>,
    ) -> std::result::Result<Vec<ArtifactDescriptor>, String>;

    fn discard_code_capture(&self, lease: &LeaseContext, session_id: &str, task_id: &str);
}

/// Panic-safe ownership of a live inventory capture. Once `begin` succeeds,
/// every unwind and every early return discards it. A successful staging call
/// removes the capture itself, so this drop then becomes an idempotent no-op.
struct CodeCaptureGuard {
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
    ) -> Result<Self, String> {
        // Allocate guard ownership before creating the capture. After begin
        // succeeds, constructing Self is allocation-free, closing the small
        // unwind window between resource acquisition and RAII ownership.
        let lease = lease.clone();
        let session_id = session_id.to_owned();
        let task_id = task_id.to_owned();
        artifacts.begin_code_capture(&lease, &session_id, &task_id, workspace)?;
        Ok(Self {
            artifacts,
            lease,
            session_id,
            task_id,
        })
    }
}

impl Drop for CodeCaptureGuard {
    fn drop(&mut self) {
        self.artifacts
            .discard_code_capture(&self.lease, &self.session_id, &self.task_id);
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
        events: Arc<dyn ControllerTaskEventSink>,
    ) -> Self {
        let state = Arc::new(Mutex::new(AcpTurnRelayState::default()));
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
            let projected = {
                let mut state = listener_state.lock();
                project_acp_event(&mut state, &envelope)
            };
            for (kind, payload) in projected {
                listener_events.emit_journal(
                    kind,
                    &listener_session,
                    Some(&listener_task),
                    payload,
                );
            }
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

    fn finish(&mut self) -> Option<String> {
        if let Some(listener_id) = self.listener_id.take() {
            self.app.unlisten(listener_id);
        }
        let (assistant_tail, reasoning_tail, terminal) = {
            let mut state = self.state.lock();
            if state.stopped {
                return state.terminal.as_ref().and_then(acp_terminal_error);
            }
            state.stopped = true;
            (
                state.assistant.finish(),
                state.reasoning.finish(),
                state.terminal.clone(),
            )
        };
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
        terminal.as_ref().and_then(acp_terminal_error)
    }
}

impl Drop for AcpTurnEventRelay {
    fn drop(&mut self) {
        let _ = self.finish();
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

fn flush_acp_streams(state: &mut AcpTurnRelayState, projected: &mut Vec<(JournalKind, Value)>) {
    let reasoning = state.reasoning.finish();
    if !reasoning.is_empty() {
        projected.push((JournalKind::Reasoning, json!({ "text": reasoning })));
    }
    let assistant = state.assistant.finish();
    if !assistant.is_empty() {
        projected.push((JournalKind::AssistantDelta, json!({ "text": assistant })));
    }
}

fn project_acp_event(
    state: &mut AcpTurnRelayState,
    envelope: &super::AcpEventEnvelope,
) -> Vec<(JournalKind, Value)> {
    if state.stopped || envelope.seq <= state.last_seq {
        return Vec::new();
    }
    if envelope.event.event_type == "turn_started" {
        if state.turn_id.is_none() {
            state.turn_id.clone_from(&envelope.turn_id);
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
            let reasoning = state.reasoning.finish();
            if !reasoning.is_empty() {
                projected.push((JournalKind::Reasoning, json!({ "text": reasoning })));
            }
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
            flush_acp_streams(state, &mut projected);
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
            flush_acp_streams(state, &mut projected);
            let status = envelope.event.data.get("status").and_then(Value::as_str);
            let message = envelope
                .event
                .data
                .get("error")
                .or_else(|| envelope.event.data.get("message"))
                .and_then(Value::as_str)
                .map(sanitize_text);
            state.terminal = Some((status.unwrap_or("Failed").to_owned(), message));
        }
        _ => {}
    }
    projected
}

/// Bounded registry of opaque `(session, workspace)` authorizations. Entries
/// are written only by the command layer and dropped when the connection ends.
#[derive(Default)]
pub(crate) struct RemoteWorkspaceAuthorizations {
    entries: Mutex<HashSet<(String, String)>>,
}

impl RemoteWorkspaceAuthorizations {
    pub(crate) fn authorize(&self, session_id: &str, workspace_id: &str) -> bool {
        let mut entries = self.entries.lock();
        if entries
            .iter()
            .any(|(session, workspace)| session == session_id && workspace != workspace_id)
        {
            return false;
        }
        if entries.len() >= MAX_AUTHORIZED_REFS
            && !entries.contains(&(session_id.to_owned(), workspace_id.to_owned()))
        {
            return false;
        }
        entries.insert((session_id.to_owned(), workspace_id.to_owned()));
        true
    }

    pub(crate) fn is_authorized(&self, session_id: &str, workspace_id: &str) -> bool {
        self.entries
            .lock()
            .contains(&(session_id.to_owned(), workspace_id.to_owned()))
    }

    pub(crate) fn workspace_for_session(&self, session_id: &str) -> Option<String> {
        let entries = self.entries.lock();
        let mut matches = entries
            .iter()
            .filter(|(candidate, _)| candidate == session_id)
            .map(|(_, workspace)| workspace.clone());
        let workspace = matches.next()?;
        // A code Session is bound to one project. Ambiguity must fail closed.
        matches.next().is_none().then_some(workspace)
    }

    pub(crate) fn revoke_session(&self, session_id: &str) {
        self.entries
            .lock()
            .retain(|(candidate, _)| candidate != session_id);
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
            if record.session_id != owner.session_id
                || record.lease != crate::features::remote_control::controller::task::TaskLeaseBinding::from_context(&owner.lease)
            {
                return Err("permission_binding_stale".to_owned());
            }
            if record.state == RemoteTaskState::Running {
                self.tasks
                    .enter_waiting_permission(&owner.task_id, now)
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
                self.tasks.resume_running(&owner.task_id,
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
        if !self.authorizations.is_authorized(session_id, workspace_id) {
            return Err("task_workspace_not_authorized".to_owned());
        }
        let authorized_path = self
            .app
            .state::<crate::features::remote_control::controller::controlled::ControlledDeviceManager>()
            .resolve_code_workspace(workspace_id)
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
        let private_skill_prompt =
            crate::features::assistant::skill_materialization::remote_turn_skill_prompt(
                crate::features::marketplace::ConnectorScope::Code,
                Some(&authorized_path),
                &skill_ids,
            )?;
        let actual_content = if private_skill_prompt.is_empty() {
            content.clone()
        } else {
            format!("{private_skill_prompt}\n<user_request>\n{content}\n</user_request>")
        };
        let attachments = if attachments.is_empty() {
            Vec::new()
        } else {
            let ledger_root = self
                .pool
                .session_store
                .session_roots(session_id)
                .map_err(|_| "task_session_not_found".to_owned())?
                .ledger;
            tokio::task::spawn_blocking(move || {
                attachments
                    .into_iter()
                    .map(|attachment| {
                        crate::features::files::file_ingest::ingest_staged_attachment(
                            &ledger_root,
                            &attachment.path,
                            &attachment.display_name,
                            attachment.bytes,
                            &attachment.sha256,
                        )
                    })
                    .collect::<Result<Vec<_>, _>>()
            })
            .await
            .map_err(|_| "task_attachment_unavailable".to_owned())??
        };
        let owner = RemotePermissionOwner {
            session_id: session_id.to_owned(),
            task_id: task_id.to_owned(),
            lease: lease.clone(),
        };
        self.pool
            .claim_remote_permission_owner(owner.clone())
            .map_err(|_| "remote_permission_owner_conflict".to_owned())?;
        // Inventory starts only after every fallible preflight that does not
        // need a capture (skills, attachment ingestion, permission ownership)
        // has succeeded. From here onward Drop guarantees capture cleanup.
        let _capture = match CodeCaptureGuard::begin(
            Arc::clone(&artifact_port),
            lease,
            session_id,
            task_id,
            &authorized_path,
        ) {
            Ok(capture) => capture,
            Err(error) => {
                self.pool.release_remote_permission_owner(&owner).await;
                return Err(error);
            }
        };

        // Subscribe before prompt submission so the authoritative journal
        // cannot miss the first real ACP turn event.
        let mut turn_relay =
            AcpTurnEventRelay::start(&self.app, session_id, task_id, Arc::clone(&events));

        let mut result = async {
            self.pool
                .send_message_with_display_content(
                    session_id,
                    actual_content,
                    content,
                    attachments,
                    Vec::new(),
                )
                .await
                .map_err(|_| "task_engine_unavailable".to_owned())?;

            // The ACP prompt is submitted in the background, so wait for the busy
            // transition before trusting a "not busy" observation.
            let mut observed_busy = false;
            let mut registered = HashMap::new();
            let mut resolved = HashSet::new();
            let started = Instant::now();
            loop {
                if cancel.is_cancelled() {
                    self.pool.cancel(session_id).await;
                    return Err(TASK_CANCELLED_CODE.to_owned());
                }
                self.sync_remote_permissions(&owner, &mut registered, &mut resolved, &events)
                    .await?;
                let busy = self.pool.is_session_busy(session_id).await;
                if busy {
                    observed_busy = true;
                } else if observed_busy {
                    return Ok(());
                } else if started.elapsed() >= BUSY_GRACE {
                    return Err("task_turn_not_started".to_owned());
                }
                tokio::select! {
                    _ = cancel.cancelled() => {
                        self.pool.cancel(session_id).await;
                        return Err(TASK_CANCELLED_CODE.to_owned());
                    }
                    _ = tokio::time::sleep(POLL_INTERVAL) => {}
                }
            }
        }
        .await;
        let terminal_error = turn_relay.finish();
        if result.is_ok() {
            if let Some(error) = terminal_error {
                result = Err(error);
            }
        }
        if result.is_ok() {
            match artifact_port.finish_code_capture(lease, session_id, task_id, Arc::clone(&events))
            {
                Ok(_) => {}
                Err(error) => result = Err(error),
            }
        }
        let dangling = self.pool.pending_remote_permissions(&owner).await;
        if !dangling.is_empty() {
            self.permission_authority.deny_all_for_task(
                &owner.task_id,
                "task_terminal",
                self.permission_authority.now_ms(),
            );
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
        self.pool.release_remote_permission_owner(&owner).await;
        result
    }
}

/// Poll interval exported for the wiring layer and tests.
pub(crate) const REMOTE_CODE_TURN_POLL_INTERVAL: Duration = POLL_INTERVAL;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::transfer::download::PreviewKind;
    use std::sync::atomic::{AtomicUsize, Ordering};

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
        ) -> Result<(), String> {
            self.begins.fetch_add(1, Ordering::SeqCst);
            Ok(())
        }

        fn finish_code_capture(
            &self,
            _lease: &LeaseContext,
            _session_id: &str,
            _task_id: &str,
            _events: Arc<dyn ControllerTaskEventSink>,
        ) -> Result<Vec<ArtifactDescriptor>, String> {
            Ok(Vec::new())
        }

        fn discard_code_capture(&self, _lease: &LeaseContext, _session_id: &str, _task_id: &str) {
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
    fn acp_projection_uses_real_order_and_never_exports_raw_tool_fields() {
        let mut state = AcpTurnRelayState::default();
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
        let mut state = AcpTurnRelayState::default();
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
        assert!(registry.authorize("session-0001", "workspace-0001"));
        assert!(registry.is_authorized("session-0001", "workspace-0001"));
        assert_eq!(
            registry.workspace_for_session("session-0001").as_deref(),
            Some("workspace-0001")
        );
        assert!(!registry.is_authorized("session-0002", "workspace-0001"));
        assert!(!registry.is_authorized("session-0001", "workspace-0002"));

        assert!(!registry.authorize("session-0001", "workspace-0002"));
        assert_eq!(
            registry.workspace_for_session("session-0001").as_deref(),
            Some("workspace-0001"),
            "a code session remains bound to its first authenticated project"
        );

        registry.revoke_session("session-0001");
        assert!(!registry.is_authorized("session-0001", "workspace-0001"));
    }

    #[test]
    fn authorization_registry_rejects_growth_past_the_cap() {
        let registry = RemoteWorkspaceAuthorizations::default();
        for index in 0..MAX_AUTHORIZED_REFS {
            assert!(registry.authorize(
                &format!("session-{index:04}"),
                &format!("workspace-{index:04}")
            ));
        }
        assert!(!registry.authorize("session-overflow", "workspace-overflow"));
        // Re-authorizing an existing pair stays idempotent at the cap.
        assert!(registry.authorize("session-0000", "workspace-0000"));
        registry.clear();
        assert!(registry.authorize("session-0001", "workspace-overflow"));
    }
}
