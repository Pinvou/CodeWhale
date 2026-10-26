//! B-owned authoritative remote journal and the A authenticated ingress adapter.

use std::collections::{HashMap, VecDeque};
use std::sync::Arc;

use parking_lot::Mutex;

use crate::features::behavior_telemetry::{
    RemoteControlEvent, RemoteModelSource, RemoteOutcome, RemoteReason, RemoteStage,
    track_remote_execution_event, track_remote_origin_event, unregister_remote_execution,
};
use serde_json::Value;

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
use crate::features::remote_control::controller::journal::{
    JournalEvent, JournalOwner, RemoteJournal,
};
use crate::features::remote_control::controller::ports::RemotePortError;
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::task::{
    RemoteTaskEvent, RemoteTaskEventSink, RemoteTaskLedger, RemoteTaskRecord,
};
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
    ledger: Arc<RemoteTaskLedger>,
    telemetry_seen: Mutex<TaskTelemetryDedupe>,
}

const TASK_TELEMETRY_DEDUPE_CAPACITY: usize = 8192;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum TaskTelemetryLifecycle {
    Admitted,
    Terminal,
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
    fn decide(&mut self, event: &RemoteTaskEvent) -> bool {
        let Some((stage, _, _)) = task_telemetry_stage(&event.status) else {
            return false;
        };
        let lifecycle = match stage {
            RemoteStage::TaskAdmit => TaskTelemetryLifecycle::Admitted,
            _ => TaskTelemetryLifecycle::Terminal,
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
        let state = self.tasks.entry(event.task_id.clone()).or_default();
        match lifecycle {
            TaskTelemetryLifecycle::Admitted if !state.admitted && !state.terminal => {
                state.admitted = true;
                true
            }
            TaskTelemetryLifecycle::Terminal if !state.terminal => {
                state.terminal = true;
                true
            }
            _ => false,
        }
    }
}

// Share the accepted statuses with dedupe: accepting an unknown terminal in
// the sink alone would still lose it at the second filter.
fn task_telemetry_stage(status: &str) -> Option<(RemoteStage, RemoteOutcome, RemoteReason)> {
    Some(match status {
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
        "failed" | "result_unknown" => (
            RemoteStage::TaskTerminal,
            RemoteOutcome::InternalError,
            RemoteReason::Unknown,
        ),
        "cancelled" | "interrupted" => (
            RemoteStage::TaskTerminal,
            RemoteOutcome::Rejected,
            RemoteReason::Unknown,
        ),
        _ => return None,
    })
}

/// Recovery rewrites updated_at_ms using a new process's monotonic origin.
/// Already-terminal durable records retain both original timestamps and remain
/// valid on replay. The persisted reason also covers records from older builds.
fn task_telemetry_duration_ms(event: &RemoteTaskEvent, record: &RemoteTaskRecord) -> Option<u64> {
    if event.status == "admitted" || record.terminal_reason.as_deref() == Some("process_restart") {
        return None;
    }
    record.updated_at_ms.checked_sub(record.admitted_at_ms)
}

impl DesktopTaskEventSink {
    pub(crate) fn new(
        app: tauri::AppHandle,
        journal: Arc<RemoteJournal>,
        ledger: Arc<RemoteTaskLedger>,
    ) -> Self {
        Self {
            app,
            journal,
            ledger,
            telemetry_seen: Mutex::new(TaskTelemetryDedupe::default()),
        }
    }

    fn track_task_once(
        &self,
        event: &RemoteTaskEvent,
        record: &crate::features::remote_control::controller::task::RemoteTaskRecord,
    ) {
        let Some((stage, outcome, reason)) = task_telemetry_stage(&event.status) else {
            return;
        };
        if !self.telemetry_seen.lock().decide(event) {
            return;
        }
        let source = match event.model_source {
            crate::features::remote_control::controller::task::RemoteTaskModelSource::ControlledDevice =>
                RemoteModelSource::Controlled,
            crate::features::remote_control::controller::task::RemoteTaskModelSource::ControllerTemporary =>
                RemoteModelSource::ControllerTemporary,
        };
        let mut metric = RemoteControlEvent::new(stage, outcome)
            .reason(reason)
            .model_source(source)
            .task(&event.task_id, true, Some(&record.mode));
        if stage == RemoteStage::TaskTerminal {
            metric = metric.status(if event.outcome_unknown {
                "unknown"
            } else {
                &event.status
            });
            if event.status == "failed" {
                if let Some(reason) = event.terminal_reason.as_deref() {
                    metric =
                        RemoteControlEvent::from_result::<()>(stage, &Err(reason.to_owned()), 0)
                            .model_source(source)
                            .task(&event.task_id, true, Some(&record.mode))
                            .status("failed");
                }
            }
        }
        if let Some(duration) = task_telemetry_duration_ms(event, record) {
            metric = metric.duration_ms(duration);
        }
        metric = metric.diagnostic_terminal(event.status == "cancelled", event.outcome_unknown);
        track_remote_execution_event(&self.app, &event.session_id, &event.task_id, metric);
        if stage == RemoteStage::TaskTerminal {
            unregister_remote_execution(&self.app, &event.session_id, &event.task_id);
        }
    }
}

const TASK_STATUS_EVENT: &str = "desktop-remote-task-event";
pub(crate) const REMOTE_PROXY_CAPABILITY_EVENT: &str = "desktop-remote-proxy-capability";

fn task_telemetry_record(
    ledger: &RemoteTaskLedger,
    event: &RemoteTaskEvent,
) -> Option<RemoteTaskRecord> {
    ledger.record(&event.task_id).ok().filter(|record| {
        record.session_id == event.session_id
            && record.lease.remote_control_generation == event.generation
    })
}

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
        // Execution telemetry follows B's durable ledger even after transport
        // loss, including recovery before DesktopRemoteTaskRuntime is managed.
        // The ledger is an explicit dependency; missing live transport must not
        // prevent recovery's interrupted/unknown terminal from being recorded.
        if let Some(record) = task_telemetry_record(&self.ledger, event) {
            self.track_task_once(event, &record);
        }
        let Some((lease, journal_owner)) = self
            .app
            .state::<ControlledDeviceManager>()
            .local_journal_context()
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
        // E2E-05：先落 B 权威 journal，再经同代加密通道回传 A。
        if let Some(stored) = self
            .journal
            .record_task_event_for_owner(&journal_owner, event)
        {
            publish_authoritative_event(
                &self.app,
                &lease.remote_control_generation,
                &journal_owner,
                &stored,
            );
        }
    }
}

/// 把一条权威 journal 事件回传当前主控端（E2E-05）：会话身份改写回 A 的引用，
/// 用 B 的加密通道封装，经同一代 endpoint lane 发送。未连接、未协商
/// `snapshot_journal_v1`、非远程会话或通道未就位时静默跳过（本地 B 事件照常）。
fn publish_if_current<T>(
    journal: &RemoteJournal,
    appended_generation: &str,
    owner: &JournalOwner,
    lease: &LeaseContext,
    event: &JournalEvent,
    mapped_session: Option<&str>,
    send: impl FnOnce() -> T,
) -> Option<T> {
    if lease.remote_control_generation != appended_generation
        || mapped_session != Some(event.session_id.as_str())
        || journal
            .require_current_event_for_owner(owner, event)
            .is_err()
    {
        return None;
    }
    Some(send())
}

pub(crate) fn publish_authoritative_event(
    app: &tauri::AppHandle,
    appended_generation: &str,
    owner: &JournalOwner,
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
        owner,
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
    pub(crate) fn from_controlled_manager(
        manager: &crate::features::remote_control::controller::controlled::ControlledDeviceManager,
    ) -> Arc<Self> {
        Arc::new(Self {
            journal: manager.journal(),
        })
    }

    #[cfg(test)]
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
        operation: &crate::features::remote_control::controller::controlling::ControllerOperation,
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
        manager.with_owned_authenticated_remote_proxy_identity(
            operation,
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
    let candidate = match (event.terminal_status.as_deref(), pending) {
        (Some("succeeded" | "failed" | "cancelled" | "interrupted"), Some(pending)) => {
            if pending.task_id != display.task_id() {
                return Err("remote_projection_terminal_correlation_mismatch".into());
            }
            let status = match event.terminal_status.as_deref() {
                Some("succeeded") => TerminalStatus::Succeeded,
                Some("failed") => TerminalStatus::Failed,
                Some("cancelled") => TerminalStatus::Cancelled,
                Some("interrupted") => TerminalStatus::Interrupted,
                _ => return Err("remote_projection_terminal_status_invalid".into()),
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
        (Some("succeeded" | "failed" | "cancelled" | "interrupted" | "result_unknown"), _)
        | (None, _) => ProxyEventCandidate::journal(binding, display),
        (Some(_), _) => return Err("remote_projection_terminal_status_invalid".into()),
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
    fn publish_task_rejection(
        &self,
        operation: &crate::features::remote_control::controller::controlling::ControllerOperation,
        lease: &LeaseContext,
        payload: &Value,
    ) -> Result<(), String> {
        use tauri::{Emitter as _, Manager as _};
        let field = |key| {
            payload
                .get(key)
                .and_then(Value::as_str)
                .ok_or_else(|| "remote_rejection_invalid".to_owned())
        };
        let code = field("rejectionCode")?;
        if field("status")? != "failed"
            || !crate::features::remote_control::controller::task_rejection::is_definite(code)
        {
            return Err("remote_rejection_invalid".into());
        }
        let remote_ref = field("sessionId")?;
        let task_id = field("taskId")?;
        let client_id = field("clientRequestId")?;
        let manager = self.app.state::<crate::features::remote_control::controller::controlling::ControllingDeviceManager>();
        let lane = self.app.state::<crate::features::remote_control::controller::encrypted_lane::ControllingLaneState>();
        let store = self.app.state::<SessionStore>();
        let identity = manager
            .authenticated_remote_proxy_identity()
            .map_err(|_| "remote_rejection_stale".to_owned())?;
        if &identity.0 != lease {
            return Err("remote_rejection_stale".into());
        }
        manager.with_owned_authenticated_remote_proxy_identity(operation, &lane, &identity, || {
            let local_id = store.remote_proxy_local_id_for_ref(remote_ref)
                .ok_or_else(|| "remote_rejection_unknown".to_owned())?;
            let live = store.remote_dispatch_live_context(&local_id, remote_ref)
                .map_err(|_| "remote_rejection_stale".to_owned())?;
            if !super::desktop_remote_task::lease_matches_remote_binding(lease, &live)
                || identity.1 != live.peer_id || identity.2 != live.resume_boundary_id {
                return Err("remote_rejection_stale".into());
            }
            store.reject_authenticated_remote_request(&local_id, remote_ref, &live, task_id, client_id, code)
                .map_err(|_| "remote_rejection_mismatch".to_owned())?;
            track_remote_origin_event(&self.app, task_id, RemoteControlEvent::from_result::<()>(
                RemoteStage::TaskRejected, &Err(code.to_owned()), 0,
            ).task(task_id, false, None));
            let record = store.remote_proxy_snapshot(&local_id).ok_or_else(|| "remote_rejection_unknown".to_owned())?;
            self.app.emit("desktop_remote_proxy", serde_json::json!({
                "localSessionId":local_id, "kind":"rejection", "remoteSessionRef":remote_ref,
                "controlledSessionId":record.controlled_session_id,
                "connectionId":live.connection_id, "controllerLeaseId":live.controller_lease_id,
                "leaseEpoch":live.lease_epoch, "relayEpoch":live.relay_epoch, "generation":live.generation,
                "fields":{"sequence":record.retired_requests.len()},
            }))
                .map_err(|_| "remote_rejection_publish_failed".to_owned())
        })
    }

    fn publish_action_result(
        &self,
        operation: &crate::features::remote_control::controller::controlling::ControllerOperation,
        lease: &LeaseContext,
        payload: &Value,
    ) -> Result<(), String> {
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
        manager.with_owned_authenticated_remote_proxy_identity(operation, &lane, &identity, || {
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
        operation: &crate::features::remote_control::controller::controlling::ControllerOperation,
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
        manager.with_owned_authenticated_remote_proxy_identity(
            operation,
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
        operation: &crate::features::remote_control::controller::controlling::ControllerOperation,
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
        manager.with_owned_authenticated_remote_proxy_identity(
            operation,
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
        let result = self.project_frame(
            frame.operation(),
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
        );
        if result.is_ok() {
            if let (Some(task_id), Some(status)) = (
                frame.event().task_id.as_deref(),
                frame.event().terminal_status.as_deref(),
            ) {
                crate::features::behavior_telemetry::track_remote_result_received(
                    &self.app, task_id, status,
                );
            }
        }
        result
    }

    fn publish_snapshot(&self, frame: &AuthenticatedSnapshotFrame<'_>) -> Result<(), String> {
        if !frame.snapshot().verify_digest() {
            return Err("remote_projection_snapshot_digest_invalid".into());
        }
        let result = self.project_frame(
            frame.operation(),
            frame.lease(),
            &frame.snapshot().session_id,
            |_, binding, _| {
                let candidate = snapshot_candidate(frame.snapshot(), binding.clone())?;
                AuthenticatedProxyEvent::verify(candidate, &SnapshotFrameProof { frame, binding })
                    .map_err(|_| "remote_projection_proof_rejected".to_owned())
            },
        );
        // Snapshot recovery is also an authenticated result-delivery path.
        // Only record after the exact live mapping and snapshot were applied;
        // stable task/status IDs deduplicate this against live/replayed frames.
        if result.is_ok() {
            for event in &frame.snapshot().events {
                if let (Some(task), Some(status)) =
                    (event.task_id.as_deref(), event.terminal_status.as_deref())
                {
                    crate::features::behavior_telemetry::track_remote_result_received(
                        &self.app, task, status,
                    );
                }
            }
        }
        result
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
    fn recovered_terminal_telemetry_reads_its_owned_ledger_without_a_runtime() {
        use crate::features::remote_control::controller::task::RemoteTaskRequest;

        for (admitted_at, reopened_at, dispatched, completed) in [
            (10_000, 1, false, false),
            (10_000, 1, true, false),
            (100, 50_000, false, false),
            (100, 50_000, true, false),
            (10_000, 1, true, true),
            (100, 50_000, true, true),
        ] {
            let directory = tempfile::tempdir().unwrap();
            let path = directory.path().join("task-ledger.json");
            let lease = LeaseContext {
                connection_id: "connection-1".into(),
                controller_lease_id: "lease-1".into(),
                lease_epoch: "1".into(),
                relay_epoch: "relay-1".into(),
                remote_control_generation: "generation-1".into(),
            };
            let request = RemoteTaskRequest::from_payload(&json!({
                "connection_id": lease.connection_id,
                "controller_lease_id": lease.controller_lease_id,
                "lease_epoch": lease.lease_epoch,
                "relay_epoch": lease.relay_epoch,
                "remote_control_generation": lease.remote_control_generation,
                "task_id": "task-1", "client_request_id": "client-1",
                "session_id": "session-1", "capability_revision": "1",
                "mode": "chat", "model_source": "controlled_device",
                "input_digest": crate::features::sessions::remote_proxy::outbox::remote_task_input_digest("hello", &[], &[]),
                "input": {"text": "hello", "attachment_ids": [], "skill_ids": []},
            })).unwrap();
            {
                let ledger = RemoteTaskLedger::open_recovered(path.clone(), 0).unwrap();
                ledger.admit(&lease, &request, admitted_at).unwrap();
                assert_eq!(
                    task_telemetry_duration_ms(
                        &task_event("task-1", "admitted"),
                        &ledger.record("task-1").unwrap()
                    ),
                    None
                );
                if dispatched {
                    ledger.begin_dispatch("task-1", admitted_at + 1).unwrap();
                }
                if completed {
                    ledger.complete("task-1", crate::features::remote_control::controller::task::RemoteTaskTerminal::Succeeded, None, admitted_at + 500).unwrap();
                }
            }
            let restored = RemoteTaskLedger::open_recovered(path.clone(), reopened_at).unwrap();
            let recovered = restored.recover_after_restart(reopened_at);
            assert_eq!(recovered.len(), usize::from(!completed));
            let persisted = restored.record("task-1").unwrap();
            let mut event = task_event("task-1", persisted.wire_status());
            event.outcome_unknown = persisted.outcome_unknown;
            let record = task_telemetry_record(&restored, &event).unwrap();
            assert_eq!(
                record.terminal_reason.as_deref(),
                if completed {
                    Some("completed")
                } else {
                    Some("process_restart")
                }
            );
            assert_eq!(record.outcome_unknown, dispatched && !completed);
            assert_eq!(
                task_telemetry_duration_ms(&event, &record),
                if completed { Some(500) } else { None }
            );
            // A second restart must neither invent recovered durations nor drop
            // a previously completed task's valid original elapsed interval.
            let replayed = RemoteTaskLedger::open_recovered(path, reopened_at + 100_000).unwrap();
            assert_eq!(
                task_telemetry_duration_ms(&event, &replayed.record("task-1").unwrap()),
                if completed { Some(500) } else { None }
            );
            let mut seen = TaskTelemetryDedupe::default();
            assert!(seen.decide(&event), "recovery emits one terminal");
            assert!(!seen.decide(&event));
            event.generation = "another-generation".into();
            assert!(task_telemetry_record(&restored, &event).is_none());
        }
    }

    #[test]
    fn task_telemetry_counts_each_lifecycle_once() {
        let mut seen = TaskTelemetryDedupe::default();
        assert!(seen.decide(&task_event("task-1", "admitted")));
        assert!(!seen.decide(&task_event("task-1", "admitted")));
        assert!(seen.decide(&task_event("task-1", "succeeded")));
        assert!(!seen.decide(&task_event("task-1", "failed")));
        assert!(!seen.decide(&task_event("task-1", "running")));
        assert_eq!(seen.tasks.len(), 1);
    }

    #[test]
    fn result_unknown_crosses_both_filters_once_without_claiming_success() {
        let mut seen = TaskTelemetryDedupe::default();
        let mut event = task_event("unknown-task", "result_unknown");
        event.outcome_unknown = true;
        assert!(matches!(
            task_telemetry_stage(&event.status),
            Some((
                RemoteStage::TaskTerminal,
                RemoteOutcome::InternalError,
                RemoteReason::Unknown
            ))
        ));
        assert!(seen.decide(&event));
        assert!(!seen.decide(&event));
        assert!(!seen.decide(&task_event("unknown-task", "admitted")));
        assert!(!seen.decide(&task_event("unknown-task", "succeeded")));
        assert!(task_telemetry_stage("running").is_none());
    }

    #[test]
    fn task_telemetry_capacity_evicts_one_without_boundary_duplicates() {
        let mut seen = TaskTelemetryDedupe::default();
        for index in 0..TASK_TELEMETRY_DEDUPE_CAPACITY {
            assert!(seen.decide(&task_event(format!("task-{index}"), "admitted")));
        }
        assert!(!seen.decide(&task_event("task-8191", "admitted")));
        assert!(seen.decide(&task_event("overflow", "admitted")));
        assert!(!seen.decide(&task_event("overflow", "admitted")));
        assert_eq!(seen.tasks.len(), TASK_TELEMETRY_DEDUPE_CAPACITY);
        assert!(!seen.tasks.contains_key("task-0"));
        assert!(seen.tasks.contains_key("task-1"));
    }

    #[test]
    fn evicted_task_replays_reenter_local_dedupe() {
        let mut seen = TaskTelemetryDedupe::default();
        assert!(seen.decide(&task_event("old-task", "admitted")));
        assert!(seen.decide(&task_event("old-task", "succeeded")));
        for index in 0..TASK_TELEMETRY_DEDUPE_CAPACITY {
            assert!(seen.decide(&task_event(format!("new-task-{index}"), "admitted")));
        }
        // Cross-restart/eviction event IDs belong to remote_execution, not this
        // bounded in-memory notification filter.
        assert!(seen.decide(&task_event("old-task", "admitted")));
        assert!(seen.decide(&task_event("old-task", "succeeded")));
        assert!(!seen.decide(&task_event("old-task", "cancelled")));
        assert_eq!(seen.tasks.len(), TASK_TELEMETRY_DEDUPE_CAPACITY);
    }

    #[test]
    fn terminal_before_admission_suppresses_late_admission() {
        let mut seen = TaskTelemetryDedupe::default();
        assert!(seen.decide(&task_event("task-1", "failed")));
        assert!(!seen.decide(&task_event("task-1", "admitted")));
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
        let winners = handles
            .into_iter()
            .map(|handle| handle.join().expect("worker completes"))
            .filter(|decision| *decision)
            .count();
        assert_eq!(winners, 1);
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
        let old_handle = journal.test_owner_for_generation("generation-1").unwrap();
        journal.bind_generation("generation-2").unwrap();
        let new_owner = lease("generation-2");
        let new_handle = journal.test_owner_for_generation("generation-2").unwrap();
        // The new ref maps to the same B session. Neither a captured old
        // lease nor reinterpreting the old event under the new lease may send.
        assert!(
            publish_if_current(
                &journal,
                "generation-1",
                &old_handle,
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
                &old_handle,
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
                &new_handle,
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
