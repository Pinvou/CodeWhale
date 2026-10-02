//! B-owned authoritative remote journal and the A authenticated ingress adapter.

use std::collections::{HashMap, VecDeque};
use std::sync::Arc;

use parking_lot::Mutex;

use crate::features::behavior_telemetry::{
    RemoteControlEvent, RemoteModelSource, RemoteOutcome, RemoteReason, RemoteStage,
    track_remote_control_with_event_id,
};
use serde_json::Value;
use sha2::{Digest, Sha256};

use crate::app::commands::desktop_remote_task::DesktopRemoteTaskRuntime;
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::controlling_service::{
    AuthenticatedJournalFrame, AuthenticatedSnapshotFrame,
};
use crate::features::remote_control::controller::event_envelope::{
    AuthenticatedProxyEvent, DisplayKind, EventLease, JournalDisplay, ProxyEventCandidate,
    ProxyEventVerificationPort, ProxySessionBinding, SnapshotDisplay, TerminalDisplay,
    TerminalStatus,
};
use crate::features::remote_control::controller::journal::JournalSnapshot;
#[cfg(test)]
use crate::features::remote_control::controller::journal::ReplayOutcome;
use crate::features::remote_control::controller::journal::{JournalEvent, RemoteJournal};
use crate::features::remote_control::controller::ports::RemotePortError;
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::task::{RemoteTaskEvent, RemoteTaskEventSink};
use crate::features::sessions::SessionStore;
use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;
use crate::features::sessions::remote_proxy::projection::TerminalPendingCorrelation;
use crate::features::sessions::remote_proxy::schema::PendingRemoteRequest;

/// Managed state for B's authoritative journal only. A history lives solely
/// in SessionStore.RemoteProxy, never in a second file or store.
pub(crate) struct DesktopRemoteJournalRuntime {
    journal: Arc<RemoteJournal>,
}

/// The task owner's terminal/status events go to both the frontend (live UI)
/// and the authoritative journal this runtime owns.
pub(crate) struct DesktopTaskEventSink {
    app: tauri::AppHandle,
    journal: Arc<RemoteJournal>,
    telemetry_seen: Mutex<TaskTelemetryDedupe>,
}

const TASK_TELEMETRY_DEDUPE_CAPACITY: usize = 8192;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum TaskTelemetryLifecycle {
    Admitted,
    Terminal,
}

impl TaskTelemetryLifecycle {
    fn as_str(self) -> &'static str {
        match self {
            Self::Admitted => "admitted",
            Self::Terminal => "terminal",
        }
    }
}

#[derive(Default)]
struct TaskTelemetryState {
    admitted: bool,
    terminal: bool,
}

#[derive(Default)]
struct TaskTelemetryDedupe {
    tasks: HashMap<String, TaskTelemetryState>,
    insertion_order: VecDeque<String>,
}

impl TaskTelemetryDedupe {
    fn decide(&mut self, event: &RemoteTaskEvent) -> Option<String> {
        let lifecycle = match event.status.as_str() {
            "admitted" => TaskTelemetryLifecycle::Admitted,
            "succeeded" | "failed" | "cancelled" | "interrupted" => {
                TaskTelemetryLifecycle::Terminal
            }
            _ => return None,
        };
        if !self.tasks.contains_key(&event.task_id) {
            while self.tasks.len() >= TASK_TELEMETRY_DEDUPE_CAPACITY {
                let Some(oldest) = self.insertion_order.pop_front() else {
                    break;
                };
                self.tasks.remove(&oldest);
            }
            self.insertion_order.push_back(event.task_id.clone());
            self.tasks
                .insert(event.task_id.clone(), TaskTelemetryState::default());
        }
        let state = self
            .tasks
            .get_mut(&event.task_id)
            .expect("task telemetry state inserted");
        let first = match lifecycle {
            TaskTelemetryLifecycle::Admitted if !state.admitted && !state.terminal => {
                state.admitted = true;
                true
            }
            TaskTelemetryLifecycle::Terminal if !state.terminal => {
                state.terminal = true;
                true
            }
            _ => false,
        };
        first.then(|| stable_task_telemetry_event_id(&event.task_id, lifecycle))
    }
}

fn stable_task_telemetry_event_id(task_id: &str, lifecycle: TaskTelemetryLifecycle) -> String {
    let mut digest = Sha256::new();
    digest.update(b"pinvou-remote-task-telemetry-v1\0");
    digest.update(lifecycle.as_str().as_bytes());
    digest.update(b"\0");
    digest.update(task_id.as_bytes());
    format!(
        "beh_remote_{}",
        crate::platform::encoding::hex_lower(&digest.finalize())
    )
}

impl DesktopTaskEventSink {
    pub(crate) fn new(app: tauri::AppHandle, journal: Arc<RemoteJournal>) -> Self {
        Self {
            app,
            journal,
            telemetry_seen: Mutex::new(TaskTelemetryDedupe::default()),
        }
    }

    fn track_task_once(&self, event: &RemoteTaskEvent) {
        let (stage, outcome, reason) = match event.status.as_str() {
            "admitted" => (
                RemoteStage::TaskAdmit,
                RemoteOutcome::Ok,
                RemoteReason::None,
            ),
            "succeeded" => (
                RemoteStage::TaskTerminal,
                RemoteOutcome::Ok,
                RemoteReason::None,
            ),
            "failed" => (
                RemoteStage::TaskTerminal,
                RemoteOutcome::InternalError,
                RemoteReason::Unknown,
            ),
            "cancelled" | "interrupted" => (
                RemoteStage::TaskTerminal,
                RemoteOutcome::Rejected,
                RemoteReason::Unknown,
            ),
            _ => return,
        };
        let Some(event_id) = self.telemetry_seen.lock().decide(event) else {
            return;
        };
        let source = match event.model_source {
            crate::features::remote_control::controller::task::RemoteTaskModelSource::ControlledDevice =>
                RemoteModelSource::Controlled,
            crate::features::remote_control::controller::task::RemoteTaskModelSource::ControllerTemporary =>
                RemoteModelSource::ControllerTemporary,
        };
        track_remote_control_with_event_id(
            &self.app,
            RemoteControlEvent::new(stage, outcome)
                .reason(reason)
                .model_source(source),
            event_id,
        );
    }
}

const TASK_STATUS_EVENT: &str = "desktop-remote-task-event";
pub(crate) const REMOTE_PROXY_CAPABILITY_EVENT: &str = "desktop-remote-proxy-capability";

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
    if event.generation != lease.remote_control_generation {
        return;
    }
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
    if runtime
        .service()
        .status(&event.task_id)
        .map(|record| {
            record.session_id != event.session_id
                || record.lease.remote_control_generation != event.generation
        })
        .unwrap_or(true)
    {
        return;
    }
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
        if result.is_ok() {
            "task_status_sent"
        } else {
            "task_status_not_sent"
        },
        if result.is_ok() {
            event.status.as_str()
        } else {
            "queue_failed"
        },
    );
}

impl RemoteTaskEventSink for DesktopTaskEventSink {
    fn publish_task_event(&self, event: &RemoteTaskEvent) {
        use tauri::Emitter as _;
        use tauri::Manager as _;
        let Some(lease) = self
            .app
            .state::<ControlledDeviceManager>()
            .with_local_cancel_lease(Clone::clone)
            .ok()
        else {
            return;
        };
        if event.generation != lease.remote_control_generation {
            return;
        }
        let _ = self.app.emit(TASK_STATUS_EVENT, event);
        publish_authoritative_task_status(&self.app, event);
        // Only the current authenticated B session/generation can allocate a
        // visible journal sequence. Ordinary local sessions remain local.
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
        // Decide telemetry exactly once per published lifecycle transition.
        // A bounded local cache suppresses hot replays; its stable anonymous
        // event ID keeps evicted replays idempotent in the existing pipeline.
        self.track_task_once(event);
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
    let journal_runtime = app.state::<Arc<DesktopRemoteJournalRuntime>>();
    if publish_if_current(
        journal_runtime.journal(),
        appended_generation,
        &lease,
        event,
        mapped_session.as_deref(),
        || {
            publish_current_authoritative_event(app, &lease, &capabilities, &session_ref, event);
        },
    )
    .is_none()
    {
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

impl DesktopRemoteJournalRuntime {
    pub(crate) fn journal_only(epoch: impl Into<String>) -> Arc<Self> {
        Arc::new(Self {
            journal: Arc::new(RemoteJournal::new(epoch)),
        })
    }

    pub(crate) fn journal(&self) -> &Arc<RemoteJournal> {
        &self.journal
    }
}

/// A 端 journal 入口只把已认证事件投影进 SessionStore.RemoteProxy。
/// （按会话引用定位；未绑定时只投影事件），并广播给前端渲染正文。
pub(crate) struct DesktopControllerJournalSink {
    app: tauri::AppHandle,
}

impl DesktopControllerJournalSink {
    pub(crate) fn new(app: tauri::AppHandle, _runtime: Arc<DesktopRemoteJournalRuntime>) -> Self {
        Self { app }
    }

    fn project_frame(
        &self,
        frame_lease: &LeaseContext,
        remote_ref: &str,
        make_authenticated: impl FnOnce(
            &SessionStore,
            ProxySessionBinding,
            &LiveRemoteContext,
        ) -> Result<AuthenticatedProxyEvent, String>,
    ) -> Result<(), String> {
        use tauri::Manager as _;
        let manager = self.app.state::<
            crate::features::remote_control::controller::controlling::ControllingDeviceManager,
        >();
        let lane = self.app.state::<
            crate::features::remote_control::controller::encrypted_lane::ControllingLaneState,
        >();
        let store = self.app.state::<SessionStore>();
        let (current, peer, boundary) = manager
            .authenticated_remote_proxy_identity()
            .map_err(|_| "remote_projection_context_stale".to_owned())?;
        if &current != frame_lease {
            return Err("remote_projection_frame_lease_stale".into());
        }
        let live = LiveRemoteContext {
            peer_id: peer.clone(),
            resume_boundary_id: boundary.clone(),
            connection_id: current.connection_id.clone(),
            controller_lease_id: current.controller_lease_id.clone(),
            lease_epoch: current.lease_epoch.clone(),
            relay_epoch: current.relay_epoch.clone(),
            generation: current.remote_control_generation.clone(),
            control_connected: true,
            encrypted_channel_connected: true,
        };
        manager.with_current_authenticated_remote_proxy_identity(
            &lane,
            &(current.clone(), peer.clone(), boundary.clone()),
            || {
                let local_id = store
                    .current_remote_proxy_id_for_ref(remote_ref, &live)
                    .map_err(|_| "remote_projection_ref_stale".to_owned())?;
                let record = store
                    .remote_proxy_snapshot(&local_id)
                    .ok_or_else(|| "remote_projection_ref_stale".to_owned())?;
                let controlled_id = record
                    .controlled_session_id
                    .ok_or_else(|| "remote_projection_mapping_missing".to_owned())?;
                store
                    .require_live_remote_projection_mapping(
                        &local_id,
                        remote_ref,
                        &live,
                        &controlled_id,
                    )
                    .map_err(|_| "remote_projection_mapping_stale".to_owned())?;
                let event_lease = EventLease::new(
                    peer,
                    boundary,
                    current.connection_id,
                    current.controller_lease_id,
                    current.lease_epoch,
                    current.relay_epoch,
                    current.remote_control_generation,
                )
                .map_err(|_| "remote_projection_lease_invalid".to_owned())?;
                let binding = ProxySessionBinding::new(
                    event_lease,
                    local_id,
                    remote_ref.to_owned(),
                    controlled_id,
                )
                .map_err(|_| "remote_projection_binding_invalid".to_owned())?;
                let authenticated = make_authenticated(&store, binding, &live)?;
                self.app
                    .state::<Arc<crate::app::remote_projection_adapter::RemoteProjectionAdapter>>()
                    .project_authenticated_now(authenticated)
                    .map(|_| ())
                    .map_err(|error| error.code().to_owned())
            },
        )
    }
}

fn display_kind(event: &JournalEvent) -> Result<DisplayKind, String> {
    match event.kind.as_str() {
        "user_message" => Ok(DisplayKind::UserMessage),
        "assistant_delta" => Ok(DisplayKind::AssistantMessage),
        "reasoning" => Ok(DisplayKind::Reasoning),
        "tool" => Ok(DisplayKind::Tool),
        "permission" => Ok(DisplayKind::Approval),
        "error" => Ok(DisplayKind::Error),
        "cancel" | "task_status" => Ok(DisplayKind::Status),
        "artifact" => Ok(DisplayKind::Artifact),
        _ => Err("remote_projection_event_kind_invalid".into()),
    }
}

const REMOTE_TOOL_DISPLAY_SCHEMA: &str = "remote_tool_display/v1";

fn remote_tool_display_body(event: &JournalEvent) -> Option<String> {
    if event.kind != "tool" {
        return None;
    }
    let tool_id = event.payload.get("tool_id").and_then(Value::as_str)?;
    let tool_name = event.payload.get("tool_name").and_then(Value::as_str)?;
    let tool_state = event.payload.get("tool_state").and_then(Value::as_str)?;
    let output = event.payload.get("text").and_then(Value::as_str);
    Some(
        serde_json::json!({
            "schema": REMOTE_TOOL_DISPLAY_SCHEMA,
            "toolId": tool_id,
            "toolName": tool_name,
            "toolState": tool_state,
            "output": output,
        })
        .to_string(),
    )
}

fn display_event(event: &JournalEvent) -> Result<JournalDisplay, String> {
    event
        .validate_exportable()
        .map_err(|_| "remote_projection_event_invalid".to_owned())?;
    let body = if let Some(status) = event.terminal_status.as_deref() {
        if event.payload.get("status").and_then(Value::as_str) != Some(status) {
            return Err("remote_projection_terminal_status_mismatch".into());
        }
        Some(status.to_owned())
    } else if event.kind == "tool" {
        // A malformed or legacy tool event must not fall through to the
        // generic JSON/text body. Without the complete display identity the
        // UI cannot safely join start/end events into a native tool card.
        remote_tool_display_body(event)
    } else {
        event
            .payload
            .get("text")
            .or_else(|| event.payload.get("message"))
            .or_else(|| event.payload.get("delta"))
            .and_then(Value::as_str)
            .map(str::to_owned)
            .or_else(|| Some(event.payload.to_string()))
    };
    JournalDisplay::new(
        event.epoch.clone(),
        event.sequence,
        event.event_id.clone(),
        display_kind(event)?,
        event
            .task_id
            .clone()
            .ok_or_else(|| "remote_projection_task_missing".to_owned())?,
        body,
        Vec::new(),
    )
    .map_err(|_| "remote_projection_display_invalid".to_owned())
}

fn journal_candidate(
    event: &JournalEvent,
    binding: ProxySessionBinding,
    pending: Option<&PendingRemoteRequest>,
) -> Result<ProxyEventCandidate, String> {
    if event.session_id != binding.remote_session_ref() {
        return Err("remote_projection_ref_mismatch".into());
    }
    let display = display_event(event)?;
    let candidate = match event.terminal_status.as_deref() {
        Some("succeeded" | "failed" | "cancelled" | "interrupted") if pending.is_some() => {
            let pending = pending.expect("matched guard");
            if pending.task_id != display.task_id() {
                return Err("remote_projection_terminal_correlation_mismatch".into());
            }
            let status = match event.terminal_status.as_deref() {
                Some("succeeded") => TerminalStatus::Succeeded,
                Some("failed") => TerminalStatus::Failed,
                Some("cancelled") => TerminalStatus::Cancelled,
                Some("interrupted") => TerminalStatus::Interrupted,
                _ => unreachable!(),
            };
            let terminal = TerminalDisplay::new(
                display,
                pending.client_request_id.clone(),
                pending.request_fingerprint.clone(),
                status,
            )
            .map_err(|_| "remote_projection_terminal_invalid".to_owned())?;
            ProxyEventCandidate::terminal(binding, terminal)
        }
        // Unknown is a displayable status, not a final outcome proof.
        Some("succeeded" | "failed" | "cancelled" | "interrupted" | "result_unknown") | None => {
            ProxyEventCandidate::journal(binding, display)
        }
        Some(_) => return Err("remote_projection_terminal_status_invalid".into()),
    };
    candidate.map_err(|_| "remote_projection_candidate_invalid".to_owned())
}

fn snapshot_candidate(
    snapshot: &JournalSnapshot,
    binding: ProxySessionBinding,
) -> Result<ProxyEventCandidate, String> {
    if !snapshot.verify_digest() || snapshot.session_id != binding.remote_session_ref() {
        return Err("remote_projection_snapshot_invalid".into());
    }
    let entries = snapshot
        .events
        .iter()
        .map(display_event)
        .collect::<Result<Vec<_>, _>>()?;
    let display = SnapshotDisplay::new(snapshot.epoch.clone(), snapshot.sequence, entries)
        .map_err(|_| "remote_projection_snapshot_invalid".to_owned())?;
    ProxyEventCandidate::snapshot(binding, display)
        .map_err(|_| "remote_projection_candidate_invalid".to_owned())
}

/// The verifier derives its expected projection independently from the
/// decoder-owned frame token. It cannot be constructed from a UI or a plain
/// JournalEvent, and it checks the exact binding captured before the write.
struct JournalFrameProof<'frame, 'event> {
    frame: &'frame AuthenticatedJournalFrame<'event>,
    binding: ProxySessionBinding,
    pending: Option<PendingRemoteRequest>,
}

impl ProxyEventVerificationPort for JournalFrameProof<'_, '_> {
    fn verify(&self, candidate: &ProxyEventCandidate) -> Result<(), RemotePortError> {
        let expected = journal_candidate(
            self.frame.event(),
            self.binding.clone(),
            self.pending.as_ref(),
        )
        .map_err(|_| RemotePortError::new("remote_projection_proof_mismatch", false))?;
        if &expected == candidate {
            Ok(())
        } else {
            Err(RemotePortError::new(
                "remote_projection_proof_mismatch",
                false,
            ))
        }
    }
}

struct SnapshotFrameProof<'frame, 'event> {
    frame: &'frame AuthenticatedSnapshotFrame<'event>,
    binding: ProxySessionBinding,
}

impl ProxyEventVerificationPort for SnapshotFrameProof<'_, '_> {
    fn verify(&self, candidate: &ProxyEventCandidate) -> Result<(), RemotePortError> {
        let expected = snapshot_candidate(self.frame.snapshot(), self.binding.clone())
            .map_err(|_| RemotePortError::new("remote_projection_proof_mismatch", false))?;
        if &expected == candidate {
            Ok(())
        } else {
            Err(RemotePortError::new(
                "remote_projection_proof_mismatch",
                false,
            ))
        }
    }
}

impl crate::features::remote_control::controller::controlling_service::ControllingJournalSink
    for DesktopControllerJournalSink
{
    fn publish_action_result(&self, lease: &LeaseContext, payload: &Value) -> Result<(), String> {
        use tauri::Manager as _;
        let manager = self.app.state::<
            crate::features::remote_control::controller::controlling::ControllingDeviceManager,
        >();
        let lane = self.app.state::<
            crate::features::remote_control::controller::encrypted_lane::ControllingLaneState,
        >();
        let store = self.app.state::<SessionStore>();
        let actions = self
            .app
            .state::<Arc<super::desktop_remote_action::DesktopRemoteActionState>>();
        let request_id = payload
            .get("request_id")
            .and_then(Value::as_str)
            .ok_or_else(|| "remote_action_result_invalid".to_owned())?;
        let (local_session_id, remote_ref, expected_live) = actions
            .pending_binding(request_id)
            .ok_or_else(|| "remote_action_unknown".to_owned())?;
        let identity = manager
            .authenticated_remote_proxy_identity()
            .map_err(|_| "remote_action_binding_stale".to_owned())?;
        if &identity.0 != lease
            || !super::desktop_remote_task::lease_matches_remote_binding(lease, &expected_live)
            || identity.1 != expected_live.peer_id
            || identity.2 != expected_live.resume_boundary_id
        {
            return Err("remote_action_binding_stale".to_owned());
        }
        manager.with_current_authenticated_remote_proxy_identity(&lane, &identity, || {
            let current_live = store
                .remote_dispatch_live_context(&local_session_id, &remote_ref)
                .map_err(|_| "remote_action_binding_stale".to_owned())?;
            store
                .require_live_remote_mutation(&local_session_id, &remote_ref, &current_live)
                .map_err(|_| "remote_action_binding_stale".to_owned())?;
            if !actions.settle_authenticated(lease, &current_live, payload) {
                return Err("remote_action_result_mismatch".to_owned());
            }
            Ok(())
        })
    }
    fn publish_capability(
        &self,
        projection: &crate::features::remote_control::controller::controlling_session::ControllingSessionProjection,
    ) -> Result<(), String> {
        use tauri::{Emitter as _, Manager as _};
        let manager = self.app.state::<
            crate::features::remote_control::controller::controlling::ControllingDeviceManager,
        >();
        let lane = self.app.state::<
            crate::features::remote_control::controller::encrypted_lane::ControllingLaneState,
        >();
        let store = self.app.state::<crate::features::sessions::SessionStore>();
        let (lease, peer_id, resume_boundary_id) = manager
            .authenticated_remote_proxy_identity()
            .map_err(|_| "remote_capability_context_stale".to_owned())?;
        if projection.connection_id != lease.connection_id
            || projection.lease_epoch != lease.lease_epoch
            || projection.remote_control_generation != lease.remote_control_generation
            || projection.session_id != projection.session_ref
        {
            return Err("remote_capability_context_stale".into());
        }
        let context = crate::features::sessions::remote_proxy::binding::LiveRemoteContext {
            peer_id: peer_id.clone(),
            resume_boundary_id: resume_boundary_id.clone(),
            connection_id: lease.connection_id.clone(),
            controller_lease_id: lease.controller_lease_id.clone(),
            lease_epoch: lease.lease_epoch.clone(),
            relay_epoch: lease.relay_epoch.clone(),
            generation: lease.remote_control_generation.clone(),
            control_connected: true,
            encrypted_channel_connected: true,
        };
        manager.with_current_authenticated_remote_proxy_identity(
            &lane,
            &(lease.clone(), peer_id, resume_boundary_id),
            || {
                let local_id = store
                    .current_remote_proxy_id_for_ref(&projection.session_ref, &context)
                    .map_err(|_| "remote_capability_context_stale".to_owned())?;
                let mapping_confirmed = store
                    .require_live_remote_mutation(&local_id, &projection.session_ref, &context)
                    .is_ok();
                self.app
                    .emit(
                        REMOTE_PROXY_CAPABILITY_EVENT,
                        serde_json::json!({
                            "localSessionId": local_id,
                            "remoteSessionRef": projection.session_ref,
                            "sessionId": projection.session_id,
                            "connectionId": lease.connection_id,
                            "controllerLeaseId": lease.controller_lease_id,
                            "leaseEpoch": lease.lease_epoch,
                            "relayEpoch": lease.relay_epoch,
                            "remoteControlGeneration": lease.remote_control_generation,
                            "capabilityRequestId": projection.capability_request_id,
                            "capabilityRevision": projection.capability_revision,
                            "capabilityValidForMs": projection.capability_valid_for_ms,
                            "capabilityError": projection.capability_error,
                            "mappingConfirmed": mapping_confirmed,
                            "temporaryModelAvailable": projection.temporary_model_available,
                            "modes": projection.modes,
                            "workspaceCount": projection.workspace_count,
                            "taskCount": projection.task_count,
                        }),
                    )
                    .map_err(|_| "remote_capability_publish_failed".to_owned())
            },
        )
    }

    fn confirm_mapping(
        &self,
        lease: &LeaseContext,
        remote_ref: &str,
        controlled_session_id: &str,
    ) -> Result<(), String> {
        use tauri::Manager as _;
        let manager = self.app.state::<
            crate::features::remote_control::controller::controlling::ControllingDeviceManager,
        >();
        let lane = self.app.state::<
            crate::features::remote_control::controller::encrypted_lane::ControllingLaneState,
        >();
        let store = self.app.state::<crate::features::sessions::SessionStore>();
        let (current_lease, peer_id, resume_boundary_id) = manager
            .authenticated_remote_proxy_identity()
            .map_err(|_| "remote_mapping_unavailable".to_owned())?;
        if &current_lease != lease {
            return Err("remote_mapping_stale".into());
        }
        let context = crate::features::sessions::remote_proxy::binding::LiveRemoteContext {
            peer_id: peer_id.clone(),
            resume_boundary_id: resume_boundary_id.clone(),
            connection_id: lease.connection_id.clone(),
            controller_lease_id: lease.controller_lease_id.clone(),
            lease_epoch: lease.lease_epoch.clone(),
            relay_epoch: lease.relay_epoch.clone(),
            generation: lease.remote_control_generation.clone(),
            control_connected: true,
            encrypted_channel_connected: true,
        };
        manager.with_current_authenticated_remote_proxy_identity(
            &lane,
            &(lease.clone(), peer_id, resume_boundary_id),
            || {
                let local_id = store
                    .remote_proxy_local_id_for_ref(remote_ref)
                    .ok_or_else(|| "remote_mapping_not_found".to_owned())?;
                store
                    .confirm_remote_mapping(
                        &local_id,
                        &context,
                        &crate::features::sessions::remote_proxy::binding::AuthenticatedMapping {
                            remote_session_ref: remote_ref.to_owned(),
                            controlled_session_id: controlled_session_id.to_owned(),
                        },
                    )
                    .map_err(|_| "remote_mapping_rejected".to_owned())
            },
        )
    }

    fn publish(&self, frame: &AuthenticatedJournalFrame<'_>) -> Result<(), String> {
        self.project_frame(
            frame.lease(),
            &frame.event().session_id,
            |store, binding, live| {
                let pending = if matches!(
                    frame.event().terminal_status.as_deref(),
                    Some("succeeded" | "failed" | "cancelled" | "interrupted")
                ) {
                    let task_id = frame
                        .event()
                        .task_id
                        .as_deref()
                        .ok_or_else(|| "remote_projection_task_missing".to_owned())?;
                    match store
                        .pending_terminal_for_live_mapping(
                            binding.local_session_id(),
                            binding.remote_session_ref(),
                            live,
                            binding.controlled_session_id(),
                            task_id,
                        )
                        .map_err(|_| "remote_projection_terminal_correlation_invalid".to_owned())?
                    {
                        TerminalPendingCorrelation::NoPending => None,
                        TerminalPendingCorrelation::Matched(pending) => Some(pending),
                    }
                } else {
                    None
                };
                let candidate =
                    journal_candidate(frame.event(), binding.clone(), pending.as_ref())?;
                AuthenticatedProxyEvent::verify(
                    candidate,
                    &JournalFrameProof {
                        frame,
                        binding,
                        pending,
                    },
                )
                .map_err(|_| "remote_projection_proof_rejected".to_owned())
            },
        )
    }

    fn publish_snapshot(&self, frame: &AuthenticatedSnapshotFrame<'_>) -> Result<(), String> {
        if !frame.snapshot().verify_digest() {
            return Err("remote_projection_snapshot_digest_invalid".into());
        }
        self.project_frame(
            frame.lease(),
            &frame.snapshot().session_id,
            |_, binding, _| {
                let candidate = snapshot_candidate(frame.snapshot(), binding.clone())?;
                AuthenticatedProxyEvent::verify(candidate, &SnapshotFrameProof { frame, binding })
                    .map_err(|_| "remote_projection_proof_rejected".to_owned())
            },
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::journal::JournalKind;
    use serde_json::json;

    fn runtime() -> Arc<DesktopRemoteJournalRuntime> {
        DesktopRemoteJournalRuntime::journal_only("epoch-1")
    }

    #[test]
    fn tool_projection_preserves_only_the_safe_display_shape() {
        let event = JournalEvent {
            event_id: "event-tool-1".into(),
            epoch: "epoch-1".into(),
            sequence: 1,
            kind: "tool".into(),
            session_id: "session-1".into(),
            task_id: Some("task-1".into()),
            terminal_status: None,
            payload: json!({
                "tool_id": "call-1",
                "tool_name": "weather",
                "tool_state": "completed",
                "text": "safe output",
            }),
        };
        let display = display_event(&event).expect("safe tool display");
        let body: Value = serde_json::from_str(display.body().expect("tool body")).unwrap();
        assert_eq!(display.kind(), DisplayKind::Tool);
        assert_eq!(body["schema"], REMOTE_TOOL_DISPLAY_SCHEMA);
        assert_eq!(body["toolId"], "call-1");
        assert_eq!(body["toolName"], "weather");
        assert_eq!(body["toolState"], "completed");
        assert_eq!(body["output"], "safe output");
        assert!(body.get("args").is_none());
    }

    #[test]
    fn incomplete_tool_projection_never_falls_back_to_a_raw_body() {
        let event = JournalEvent {
            event_id: "event-tool-legacy".into(),
            epoch: "epoch-1".into(),
            sequence: 1,
            kind: "tool".into(),
            session_id: "session-1".into(),
            task_id: Some("task-1".into()),
            terminal_status: None,
            payload: json!({ "text": "legacy raw tool output" }),
        };
        let display = display_event(&event).expect("legacy tool display");
        assert_eq!(display.kind(), DisplayKind::Tool);
        assert_eq!(display.body(), None);
    }

    fn task_event(task_id: impl Into<String>, status: &str) -> RemoteTaskEvent {
        use crate::features::remote_control::controller::task::RemoteTaskModelSource;

        RemoteTaskEvent {
            task_id: task_id.into(),
            session_id: "session-1".into(),
            generation: "generation-1".into(),
            model_source: RemoteTaskModelSource::ControlledDevice,
            status: status.into(),
            sequence: "1".into(),
            transition_sequence: 1,
            outcome_unknown: false,
            terminal_reason: None,
            result_digest: None,
        }
    }

    #[test]
    fn task_telemetry_counts_each_lifecycle_once_with_stable_ids() {
        let mut seen = TaskTelemetryDedupe::default();
        let admitted = task_event("task-1", "admitted");
        let admitted_id = seen.decide(&admitted).expect("first admission");
        assert!(seen.decide(&admitted).is_none());

        let succeeded = task_event("task-1", "succeeded");
        let terminal_id = seen.decide(&succeeded).expect("first terminal");
        assert!(seen.decide(&succeeded).is_none());
        assert!(seen.decide(&task_event("task-1", "failed")).is_none());
        assert_ne!(admitted_id, terminal_id);
        assert_eq!(seen.tasks.len(), 1);
    }

    #[test]
    fn task_telemetry_capacity_evicts_one_without_boundary_duplicates() {
        let mut seen = TaskTelemetryDedupe::default();
        for index in 0..(TASK_TELEMETRY_DEDUPE_CAPACITY - 1) {
            assert!(
                seen.decide(&task_event(format!("task-{index}"), "admitted"))
                    .is_some()
            );
        }
        assert_eq!(seen.tasks.len(), 8191);

        let boundary = task_event("task-8191", "admitted");
        let boundary_id = seen.decide(&boundary).expect("8192nd admission");
        assert_eq!(seen.tasks.len(), 8192);
        assert!(seen.decide(&boundary).is_none());

        let overflow = task_event("task-8192", "admitted");
        let overflow_id = seen.decide(&overflow).expect("8193rd admission");
        assert_eq!(seen.tasks.len(), 8192);
        assert!(seen.decide(&overflow).is_none());
        assert_ne!(boundary_id, overflow_id);
    }

    #[test]
    fn evicted_task_replays_reuse_lifecycle_event_ids() {
        let mut seen = TaskTelemetryDedupe::default();
        let admitted = task_event("old-task", "admitted");
        let admitted_id = seen.decide(&admitted).expect("old admission");
        let terminal = task_event("old-task", "succeeded");
        let terminal_id = seen.decide(&terminal).expect("old terminal");
        for index in 0..TASK_TELEMETRY_DEDUPE_CAPACITY {
            assert!(
                seen.decide(&task_event(format!("new-task-{index}"), "admitted"))
                    .is_some()
            );
        }
        assert_eq!(seen.tasks.len(), TASK_TELEMETRY_DEDUPE_CAPACITY);
        assert_eq!(seen.decide(&admitted), Some(admitted_id));
        assert_eq!(seen.decide(&terminal), Some(terminal_id));
        assert!(seen.decide(&task_event("old-task", "cancelled")).is_none());
        assert_eq!(seen.tasks.len(), TASK_TELEMETRY_DEDUPE_CAPACITY);
    }

    #[test]
    fn terminal_before_admission_suppresses_late_admission() {
        let mut seen = TaskTelemetryDedupe::default();
        assert!(seen.decide(&task_event("task-1", "failed")).is_some());
        assert!(seen.decide(&task_event("task-1", "admitted")).is_none());
    }

    #[test]
    fn concurrent_task_telemetry_transition_has_one_local_winner() {
        let seen = Arc::new(Mutex::new(TaskTelemetryDedupe::default()));
        let event = task_event("concurrent-task", "admitted");
        let handles = (0..8)
            .map(|_| {
                let seen = Arc::clone(&seen);
                let event = event.clone();
                std::thread::spawn(move || seen.lock().decide(&event))
            })
            .collect::<Vec<_>>();
        let decisions = handles
            .into_iter()
            .filter_map(|handle| handle.join().expect("worker completes"))
            .collect::<Vec<_>>();
        assert_eq!(decisions.len(), 1);
        assert_eq!(
            decisions[0],
            stable_task_telemetry_event_id("concurrent-task", TaskTelemetryLifecycle::Admitted)
        );
    }

    #[test]
    fn b_snapshot_window_fits_a_projection_window() {
        assert!(
            crate::features::remote_control::controller::journal::JOURNAL_CAPACITY
                <= crate::features::sessions::remote_proxy::schema::MAX_PROJECTION_ENTRIES
        );
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
        assert!(
            publish_if_current(
                &journal,
                "generation-1",
                &new_owner,
                &old,
                Some("b-session"),
                || sent.set(sent.get() + 1),
            )
            .is_none()
        );
        assert!(
            publish_if_current(
                &journal,
                "generation-1",
                &old_owner,
                &old,
                Some("b-session"),
                || sent.set(sent.get() + 1),
            )
            .is_none()
        );
        assert_eq!(sent.get(), 0);

        let current = journal
            .append_for_generation(
                "generation-2",
                "event-current",
                JournalKind::UserMessage,
                "b-session",
                None,
                json!({"text":"new"}),
            )
            .unwrap();
        assert!(
            publish_if_current(
                &journal,
                "generation-2",
                &new_owner,
                &current,
                Some("b-session"),
                || sent.set(sent.get() + 1),
            )
            .is_some()
        );
        assert_eq!(sent.get(), 1);
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
        let replay = match runtime
            .journal()
            .replay_after_for("session-0001", "epoch-2", 0)
            .unwrap()
        {
            ReplayOutcome::Gap(snapshot) => snapshot,
            ReplayOutcome::Events(_) => panic!("a foreign epoch must produce a snapshot"),
        };
        assert_eq!(replay.epoch, "epoch-1");
        assert_eq!(replay.sequence, 1);
    }
}
