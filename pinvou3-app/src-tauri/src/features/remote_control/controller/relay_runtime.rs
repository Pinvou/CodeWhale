//! Production glue between the Windows controlled-device owner and Relay.
//!
//! E2E-01 owns pairing registration: B reuses the authenticated desktop
//! endpoint socket, sends one generation-bound registration and validates
//! Relay's sealed result. E2E-02 adds the对称握手接线: the same channel takes
//! over from `controller_pairing_reserved`, runs the commitment/Finished
//! exchange and applies `controller_joined` to the device owner.

use std::collections::HashMap;
use std::sync::OnceLock;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use base64::Engine as _;
use parking_lot::Mutex;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tauri::{AppHandle, Emitter, Manager as _};
use tokio::sync::oneshot;

use crate::features::behavior_telemetry::{
    RemoteControlEvent, RemoteOutcome, RemoteReason, RemoteStage, track_remote_control,
};
use crate::features::remote_control::controller::ports::RemotePortError;
use crate::features::remote_control::manager::RemoteControlManager;

use super::controlled::{
    ControlledDeviceManager, ControlledLease, ControlledOperation, ControlledRegistration,
    ControlledResumeStep, ControlledStatus, ControlledStatusKind,
};
use super::controlled_handshake::ControlledHandshake;
use super::controlling_handshake::JoinedLease;
use super::crypto::{
    AuthenticatedRelayTrustConfig, CodeWrapBinding, CodeWrapKeyPair,
    ControllerRole as HandshakeRole, HandshakeKeyPair, SessionKeys, load_relay_trust_config,
    transport_key_fingerprint,
};
use super::encrypted_lane::ControlledLaneState;
use super::protocol::LeaseContext;
use super::protocol::{
    ROLE_B, RegistrationValidationContext, SenderRole, is_self_echoed_handshake_frame,
    validate_controller_message_json, validate_controller_registration_message_json,
};
use super::resume::{
    build_controller_post_activation_liveness, build_controller_resume_activation_ack,
    build_controller_resume_prepare, build_controller_resume_ready_ack, new_resume_challenge,
    resume_frame_matches_lease,
};

const REGISTRATION_TIMEOUT_MS: u64 = 30_000;
const MAX_PAIRING_CODE_TTL_MS: u64 = 5 * 60_000;
const REMOTE_ACTION_CACHE_CAPACITY: usize = 256;
const REMOTE_ACTION_CACHE_TTL: Duration = Duration::from_secs(10 * 60);

#[derive(Clone)]
struct CachedRemoteActionResult {
    fingerprint: [u8; 32],
    response: Value,
    touched_at: Instant,
}

#[derive(Default)]
struct RemoteActionResultCache {
    entries: Mutex<HashMap<String, CachedRemoteActionResult>>,
}

enum RemoteActionCacheLookup {
    Miss,
    Hit(Value),
    Conflict,
}

impl RemoteActionResultCache {
    fn lookup(&self, request_id: &str, fingerprint: [u8; 32]) -> RemoteActionCacheLookup {
        let mut entries = self.entries.lock();
        let now = Instant::now();
        entries.retain(|_, entry| {
            now.saturating_duration_since(entry.touched_at) <= REMOTE_ACTION_CACHE_TTL
        });
        match entries.get_mut(request_id) {
            Some(entry) if entry.fingerprint == fingerprint => {
                entry.touched_at = now;
                RemoteActionCacheLookup::Hit(entry.response.clone())
            }
            Some(_) => RemoteActionCacheLookup::Conflict,
            None => RemoteActionCacheLookup::Miss,
        }
    }

    fn insert(&self, request_id: String, fingerprint: [u8; 32], response: Value) {
        let mut entries = self.entries.lock();
        let now = Instant::now();
        entries.retain(|_, entry| {
            now.saturating_duration_since(entry.touched_at) <= REMOTE_ACTION_CACHE_TTL
        });
        if entries.len() >= REMOTE_ACTION_CACHE_CAPACITY {
            if let Some(oldest) = entries
                .iter()
                .min_by_key(|(_, entry)| entry.touched_at)
                .map(|(request_id, _)| request_id.clone())
            {
                entries.remove(&oldest);
            }
        }
        entries.insert(
            request_id,
            CachedRemoteActionResult {
                fingerprint,
                response,
                touched_at: now,
            },
        );
    }
}

fn remote_action_fingerprint(
    lease: &LeaseContext,
    session_ref: &str,
    request_id: &str,
    operation: &str,
    body: &Value,
) -> [u8; 32] {
    let mut hasher = Sha256::new();
    let body_bytes = body.to_string().into_bytes();
    for field in [
        lease.connection_id.as_bytes(),
        lease.controller_lease_id.as_bytes(),
        lease.lease_epoch.as_bytes(),
        lease.relay_epoch.as_bytes(),
        lease.remote_control_generation.as_bytes(),
        session_ref.as_bytes(),
        request_id.as_bytes(),
        operation.as_bytes(),
        body_bytes.as_slice(),
    ] {
        hasher.update((field.len() as u64).to_be_bytes());
        hasher.update(field);
    }
    hasher.finalize().into()
}

// Relay signs a wall-clock expiry, whereas the B lifecycle owner and UI
// countdown use one local monotonic clock. Convert once at receipt; never
// expose either absolute clock origin to the WebView.
fn pairing_deadline_b(wire_expiry_ms: u64, wall_now_ms: u64, now_b: u64) -> Result<u64, String> {
    let remaining = wire_expiry_ms
        .checked_sub(wall_now_ms)
        .filter(|remaining| *remaining > 0)
        .ok_or_else(|| "pairing_code_expired".to_owned())?
        .min(MAX_PAIRING_CODE_TTL_MS);
    now_b
        .checked_add(remaining)
        .ok_or_else(|| "pairing_registration_invalid".to_owned())
}

fn wall_clock_millis() -> Result<u64, String> {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .ok()
        .and_then(|elapsed| u64::try_from(elapsed.as_millis()).ok())
        .ok_or_else(|| "pairing_clock_invalid".to_owned())
}

// Keep the transport's existing host projection at one narrow callsite.
// The controlled state owner constructs each status before it is published.
fn emit_controlled_status(app: &AppHandle, status: &ControlledStatus) {
    track_controlled_status_transition(app, status);
    let _ = app.emit(CONTROLLED_STATUS_EVENT, status);
}

/// Both worker and command projections pass through this transition gate.
/// It retains only the fixed phase enum in memory; no generation or code is
/// used for correlation or written to behavior telemetry.
pub(crate) fn track_controlled_status_transition(app: &AppHandle, status: &ControlledStatus) {
    static LAST: OnceLock<Mutex<Option<ControlledStatusKind>>> = OnceLock::new();
    let mut last = LAST.get_or_init(|| Mutex::new(None)).lock();
    let current = status.kind();
    let previous = last.replace(current);
    if previous == Some(current) {
        return;
    }
    let event = match (previous, current) {
        (Some(ControlledStatusKind::WaitingForPairing), ControlledStatusKind::PairingReserved) => {
            Some(RemoteControlEvent::new(
                RemoteStage::Claim,
                RemoteOutcome::Ok,
            ))
        }
        (Some(ControlledStatusKind::HandshakePending), ControlledStatusKind::Connected) => Some(
            RemoteControlEvent::new(RemoteStage::Handshake, RemoteOutcome::Ok),
        ),
        (Some(ControlledStatusKind::Connected), ControlledStatusKind::Reconnecting) => Some(
            RemoteControlEvent::new(RemoteStage::Disconnection, RemoteOutcome::Ok),
        ),
        (
            Some(
                ControlledStatusKind::Reconnecting
                | ControlledStatusKind::ResumeProvisional
                | ControlledStatusKind::ResumeActivatedPendingLiveness,
            ),
            ControlledStatusKind::Connected,
        ) => Some(RemoteControlEvent::new(
            RemoteStage::Resume,
            RemoteOutcome::Ok,
        )),
        (
            Some(
                ControlledStatusKind::Reconnecting
                | ControlledStatusKind::ResumeProvisional
                | ControlledStatusKind::ResumeActivatedPendingLiveness,
            ),
            ControlledStatusKind::Error,
        ) => Some(
            RemoteControlEvent::new(RemoteStage::Resume, RemoteOutcome::Rejected)
                .reason(RemoteReason::ResumeFailed),
        ),
        (Some(ControlledStatusKind::HandshakePending), ControlledStatusKind::Error) => Some(
            RemoteControlEvent::new(RemoteStage::Handshake, RemoteOutcome::Rejected)
                .reason(RemoteReason::HandshakeFailed),
        ),
        _ => None,
    };
    drop(last);
    if let Some(event) = event {
        track_remote_control(app, event);
    }
}
/// 端点通道就绪等待：单次上限与重试间隔（整体最多 3 次尝试）。
const ENDPOINT_READY_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(60);
const ENDPOINT_READY_RETRY_DELAY: std::time::Duration = std::time::Duration::from_secs(2);

/// 双端共用的排障日志写入器（追加写入 `~/.pinvou3/logs/remote-control.log`）。
///
/// `scope` 只区分端别：`controlled`（B 端）与 `controller`（A 端），便于同一台机器
/// 或同一份日志里对照同一次连接的阶段。只在「启停/注册/握手/连接结果」这些用户可
/// 感知的失败与成功节点写一行有界文本：不含占位符之外的任何标识（不写
/// endpoint_id/lease/token/连接码），便于用户把文件直接发给支持而不担心泄漏。
/// 写失败一律忽略，绝不影响主流程。
pub(crate) fn log_relay_event(scope: &str, stage: &str, detail: &str) {
    write_relay_event(scope, stage, None, detail);
}

/// Remote-action diagnostics add only the closed operation name to the common
/// log envelope. Request/session identifiers and action bodies never reach the
/// sink, while A/B traces can still be correlated by operation and timestamp.
pub(crate) fn log_remote_action_event(scope: &str, stage: &str, operation: &str, detail: &str) {
    write_relay_event(scope, stage, Some(operation), detail);
}

// Closed diagnostic projection for the controlled task admission boundary.
// Keep this list synchronized with the stable codes accepted by
// `desktop_remote_controller_business::submit_error`. Unknown static codes
// remain private and collapse to `other` at the sink.
const CONTROLLED_PORT_LOG_ERROR_CODES: &[&str] = &[
    "capability_admission_capacity",
    "capability_descriptor_capacity",
    "capability_descriptor_duplicate",
    "capability_display_invalid",
    "capability_expiry_overflow",
    "capability_id_invalid",
    "capability_revision_exhausted",
    "capability_snapshot_expired",
    "capability_snapshot_missing",
    "capability_snapshot_stale",
    "capability_workspace_capacity",
    "capability_workspace_duplicate",
    "capability_workspace_not_authorized",
    "controlled_session_projection_failed",
    "desktop_remote_not_connected",
    "lease_unavailable",
    "model_binding_invalid",
    "model_binding_stale",
    "model_credential_unavailable",
    "model_credential_invalid",
    "model_credential_kind_unsupported",
    "model_display_name_invalid",
    "model_endpoint_insecure",
    "model_endpoint_invalid",
    "model_endpoint_local",
    "model_endpoint_mode_unsupported",
    "model_endpoint_private",
    "model_endpoint_unresolved",
    "model_image_override_unsupported",
    "model_not_exportable",
    "model_package_capacity",
    "model_package_field_invalid",
    "model_package_id_invalid",
    "model_package_invalid",
    "model_package_missing",
    "model_package_missing_field",
    "model_package_schema_unsupported",
    "model_package_too_large",
    "model_package_unknown_field",
    "model_preset_unsupported",
    "model_provider_kind_unsupported",
    "model_reasoning_effort_unsupported",
    "model_route_invalid",
    "model_runtime_unavailable",
    "model_secret_invalid",
    "model_secret_too_large",
    "model_vendor_unsupported",
    "model_wire_api_mismatch",
    "model_wire_api_unsupported",
    "model_wire_name_invalid",
    "task_binding_stale",
    "task_input_digest_invalid",
    "task_input_invalid",
    "task_lease_mismatch",
    "task_ledger_capacity",
    "task_ledger_corrupt",
    "task_ledger_persist_failed",
    "task_ledger_serialize_failed",
    "task_ledger_unreadable",
    "task_ledger_version_unsupported",
    "task_ledger_write_failed",
    "task_mode_invalid",
    "task_mode_unsupported",
    "task_model_source_invalid",
    "task_model_source_unsupported",
    "task_not_found",
    "task_payload_invalid_enum",
    "task_payload_invalid_field",
    "task_payload_invalid_json",
    "task_payload_invalid_message",
    "task_payload_invalid_type",
    "task_payload_length_mismatch",
    "task_payload_message_too_large",
    "task_payload_missing_field",
    "task_payload_replay",
    "task_payload_role_mismatch",
    "task_payload_stale_context",
    "task_payload_unknown_field",
    "task_payload_unknown_message_type",
    "task_payload_unsupported_capability",
    "task_payload_unsupported_version",
    "task_request_conflict",
    "task_request_invalid",
    "task_result_digest_invalid",
    "task_session_backend_bind_failed",
    "task_session_binding_mismatch",
    "task_session_busy",
    "task_session_create_failed",
    "task_session_generation_conflict",
    "task_session_generation_stale",
    "task_session_lane_conflict",
    "task_session_lane_mismatch",
    "task_session_lane_unknown",
    "task_session_lookup_failed",
    "task_session_map_conflict",
    "task_session_not_found",
    "task_session_provisioner_unavailable",
    "task_skill_invalid",
    "task_state_conflict",
    "task_status_binding_invalid",
    "task_workspace_capacity",
    "task_workspace_not_authorized",
    "task_workspace_required",
    "task_workspace_unexpected",
    "temporary_model_capability_required",
];

/// Log a stable application-boundary error without collapsing its code to
/// `other` when it belongs to the explicit controlled-task diagnostic set.
/// User input, identifiers and infrastructure error strings never reach the
/// sink, and future port errors remain private until deliberately reviewed.
pub(crate) fn log_controlled_port_error(stage: &str, error: RemotePortError) {
    write_relay_event_inner("controlled", stage, None, log_port_error_detail(error));
}

fn write_relay_event(scope: &str, stage: &str, operation: Option<&str>, detail: &str) {
    write_relay_event_inner(scope, stage, operation, log_detail(detail));
}

fn write_relay_event_inner(
    scope: &str,
    stage: &str,
    operation: Option<&str>,
    detail: &'static str,
) {
    use std::io::Write as _;
    static LOG_WRITE_LOCK: OnceLock<parking_lot::Mutex<()>> = OnceLock::new();

    // Several transport/task callbacks can report transitions concurrently.
    // Serialize the complete append so two records cannot be spliced together.
    let _write_guard = LOG_WRITE_LOCK
        .get_or_init(|| parking_lot::Mutex::new(()))
        .lock();
    let timestamp_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|duration| duration.as_millis())
        .unwrap_or(0);
    let dir = crate::platform::paths::pinvou3_home().join("logs");
    if std::fs::create_dir_all(&dir).is_err() {
        return;
    }
    let Ok(mut file) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(dir.join("remote-control.log"))
    else {
        return;
    };
    if let Some(operation) = operation {
        let _ = writeln!(
            file,
            "ts_ms={timestamp_ms} pid={} [{}] stage={} operation={} detail={}",
            std::process::id(),
            log_scope(scope),
            log_stage(stage),
            log_remote_action_operation(operation),
            detail
        );
    } else {
        let _ = writeln!(
            file,
            "ts_ms={timestamp_ms} pid={} [{}] stage={} detail={}",
            std::process::id(),
            log_scope(scope),
            log_stage(stage),
            detail
        );
    }
}

fn log_scope(value: &str) -> &'static str {
    match value {
        "controlled" => "controlled",
        "controller" => "controller",
        _ => "unknown",
    }
}

/// Semantic allowlists also reject identifiers consisting only of letters or digits.
fn log_stage(value: &str) -> &'static str {
    const STAGES: &[&str] = &[
        "controlled_status",
        "controlled_result",
        "capability_handshake",
        "start_rejected",
        "start_failed",
        "connect_requested",
        "connect_rejected",
        "connect_started",
        "connected",
        "connect_failed",
        "status",
        "handshake_failed",
        "registration_rejected",
        "registration_sent",
        "registration_failed",
        "registered",
        "worker_started",
        "endpoint_ready",
        "runtime_relay_error",
        "liveness_challenge",
        "liveness_attestation",
        "liveness_direct",
        "task_submit_frame_received",
        "task_submit_endpoint_received",
        "task_submit_socket_written",
        "task_submit_socket_write_failed",
        "task_submit_decrypted",
        "task_submit_dispatched",
        "task_submit_dispatch_failed",
        "task_submit_status_sent",
        "task_submit_business_entered",
        "task_submit_business_accepted",
        "task_submit_business_rejected",
        "task_submit_context_validated",
        "task_submit_payload_validated",
        "task_submit_identity_validated",
        "task_submit_capability_validated",
        "task_submit_mapping_ready",
        "task_submit_mapping_projected",
        "task_submit_prepared",
        "controlled_submission_admission_entered",
        "task_model_source_received",
        "business_frame_dropped",
        "business_frame_rejected",
        "business_dispatch_failed",
        "business_response_seal_failed",
        "business_response_send_failed",
        "capability_response_sent",
        "capability_snapshot_failed",
        "capability_snapshot_applied",
        "capability_snapshot_rejected",
        "capability_error_rejected",
        "journal_event_rejected",
        "journal_event_ignored",
        "journal_event_applied",
        "journal_event_verified",
        "journal_event_gap",
        "journal_snapshot_rejected",
        "task_status_sent",
        "task_status_not_sent",
        "journal_event_not_sent",
        "journal_event_sent",
        "journal_snapshot_sent",
        "remote_action_received",
        "remote_action_requested",
        "remote_action_validation_failed",
        "remote_action_failed",
        "remote_action_sent",
        "remote_action_result_received",
        "remote_action_completed",
        "remote_action_timeout",
        "relay_reset_enqueue_failed",
        "relay_frame_oversized",
        "relay_frame_rejected",
        "relay_read_failed",
        "relay_connect_failed",
        "relay_send_failed",
        "access_policy_load_failed",
        "revocation_frame_rejected",
        "revocation_ack_persist_failed",
        "local_event_forward_failed",
        "relay_message_rejected",
        "revoked_config_remove_failed",
        "revoked_ledger_remove_failed",
        "session_id_rejected",
        "session_scope_lookup_failed",
        "stream_reset_enqueue_failed",
        "attachment_digest_missing",
        "attachment_uploads_expired",
        "artifact_downloads_expired",
        "task_terminal_persist_failed",
        "task_rejected_persist_failed",
        "task_dispatch_rejected",
        "artifact_capture_failed",
        "workspace_auth_restore_failed",
        "attachment_release_failed",
        "attachment_finalize_failed",
        "attachment_cleanup_failed",
        "journal_append_rejected",
        "knowledge_model_load_failed",
        "knowledge_mount_emit_failed",
        "web_session_operation_failed",
        "web_workspace_operation_failed",
        "web_acp_operation_failed",
        "lifecycle_checkpoint",
    ];
    STAGES
        .iter()
        .copied()
        .find(|stage| *stage == value)
        .unwrap_or("other")
}

fn log_detail(value: &str) -> &'static str {
    const DETAILS: &[&str] = &[
        "ok",
        "failed",
        "unknown",
        "unavailable",
        "accepted",
        "rejected",
        "sent",
        "send_failed",
        "requested",
        "expired",
        "ready",
        "duplicate",
        "connected",
        "reconnecting",
        "registering",
        "waiting_for_pairing",
        "pairing_reserved",
        "handshake_pending",
        "stopped",
        "error",
        "waiting_registered",
        "channel_ready",
        "opening",
        "dispatching",
        "forwarding_to_worker",
        "validating",
        "admitted",
        "transport_error",
        "controlled_device",
        "controller_temporary",
        "no_live_lease",
        "relay_rejected",
        "projection_failed",
        "retired_epoch",
        "replay_requested",
        "invalid_envelope_or_identity",
        "lease_unavailable",
        "session_mapping_missing",
        "status_unavailable",
        "outbound_unavailable",
        "seal_failed",
        "queue_failed",
        "generation_or_binding_stale",
        "permission_expired",
        "permission_denied",
        "task_conflict",
        "missing",
        "remote_binding_mismatch",
        "controller_not_connected",
        "remote_session_unavailable",
        "remote_proxy_not_found",
        "remote_mapping_unconfirmed",
        "remote_action_invalid",
        "remote_action_capacity",
        "remote_action_failed",
        "remote_encrypt_failed",
        "remote_send_uncertain",
        "task_session_not_found",
        "task_binding_stale",
        "transfer_unknown",
        "transfer_chunk_invalid",
        "transfer_chunk_out_of_order",
        "artifact_unavailable",
        "runtime_started",
        "start_requested",
        "refresh_requested",
        "refresh_retiring",
        "refresh_connection_cleanup_begin",
        "refresh_connection_cleanup_done",
        "refresh_relay_stop_begin",
        "refresh_relay_stop_done",
        "refresh_state_rotation_begin",
        "refresh_state_rotation_done",
        "refresh_worker_start_begin",
        "refresh_worker_start_done",
        "refresh_registration_wait_begin",
        "refresh_registration_wait_done",
        "refresh_registration_wait_failed",
        "refresh_completed",
        "refresh_failed",
        "stop_requested",
        "stop_completed",
        "disconnect_requested",
        "disconnect_completed",
    ];
    if let Some(value) = DETAILS.iter().copied().find(|detail| *detail == value) {
        return value;
    }
    super::protocol::CONTROLLER_ERROR_CODES
        .iter()
        .copied()
        .find(|code| *code == value)
        .unwrap_or("other")
}

fn log_port_error_detail(error: RemotePortError) -> &'static str {
    CONTROLLED_PORT_LOG_ERROR_CODES
        .iter()
        .copied()
        .find(|code| *code == error.code())
        .unwrap_or("other")
}

fn log_remote_action_operation(value: &str) -> &'static str {
    super::protocol::REMOTE_ACTION_OPERATIONS
        .iter()
        .copied()
        .find(|operation| *operation == value)
        .unwrap_or("other")
}

/// B 端预握手阶段的自帧回投过滤：Relay 会把发送方的 `finished_receipt_commit`
/// 原样回投，B 只处理 `sender_role == A` 的对端帧，自己的回投必须跳过。
pub(crate) fn skip_self_echoed_frame(message: &Value) -> bool {
    is_self_echoed_handshake_frame(message, ROLE_B)
}

/// B 端（被控端）排障日志。
pub(crate) fn log_controlled_event(stage: &str, detail: &str) {
    log_relay_event("controlled", stage, detail);
}

/// A 端（控制端）排障日志。A 端此前没有任何落盘痕迹，导致「连接码无效或已过期」
/// 这类失败只能靠猜；这里补上与 B 端对称的阶段与结果码。
pub(crate) fn log_controller_event(stage: &str, detail: &str) {
    log_relay_event("controller", stage, detail);
}

/// Production failure projections deliberately accept the raw source values
/// only to prove that the boundary discards them before touching the sink.
pub(crate) fn log_attachment_digest_omitted(_file_name: &str) {
    log_controlled_event("attachment_digest_missing", "missing");
}

pub(crate) fn log_task_persist_failure<T: ?Sized>(rejected: bool, _raw_error: &T) {
    log_controlled_event(
        if rejected {
            "task_rejected_persist_failed"
        } else {
            "task_terminal_persist_failed"
        },
        "failed",
    );
}

pub(crate) fn log_artifact_capture_failure<T: ?Sized>(_raw_error: &T) {
    log_controlled_event("artifact_capture_failed", "failed");
}

pub(crate) fn log_attachment_cleanup_failure(_path: &std::path::Path, _raw_error: &std::io::Error) {
    log_controlled_event("attachment_cleanup_failed", "failed");
}

/// 测试/内联断言用：日志行格式（不含时间戳，便于跨平台断言）。
pub(crate) fn relay_log_line(scope: &str, stage: &str, detail: &str) -> String {
    format!("[{scope}] stage={stage} detail={detail}")
}

/// 测试/内联断言用：B 端日志行格式。
pub(crate) fn controlled_log_line(stage: &str, detail: &str) -> String {
    relay_log_line("controlled", stage, detail)
}

/// 测试/内联断言用：A 端日志行格式。
pub(crate) fn controller_log_line(stage: &str, detail: &str) -> String {
    relay_log_line("controller", stage, detail)
}

pub(crate) fn remote_action_log_line(
    scope: &str,
    stage: &str,
    operation: &str,
    detail: &str,
) -> String {
    format!(
        "[{}] stage={} operation={} detail={}",
        log_scope(scope),
        log_stage(stage),
        log_remote_action_operation(operation),
        log_detail(detail)
    )
}
const REGISTRATION_CAPABILITIES: &[&str] = &[
    "pairing_v1",
    "encrypted_frames_v1",
    "finished_receipt_v1",
    // E2E-03：B 端的权威 session / 任务台账（DEV-12/DEV-13）已实现，注册时
    // 一并声明 task_v1，使 A 的能力快照请求与任务请求能通过协商。
    "task_v1",
    "temporary_model_package_v1",
    // DEV-23：审批、附件和成果均通过统一 remote_action 加密通道；只有在生产
    // 注册能力中显式声明，A/B 才能协商出相应 operation capability。
    "permission_v1",
    "attachment_v1",
    "artifact_v1",
    // E2E-05：B 的权威 journal（DEV-19）已实现，声明 snapshot_journal_v1 使
    // journal_event / journal_replay 等正文事件能通过协商。
    "snapshot_journal_v1",
    // E2E-06：B 的 liveness watchdog（DEV-08）已实现，声明 role_liveness_v1 让
    // A 端心跳帧与后续 challenge/attestation 能通过协商。
    "role_liveness_v1",
    // E2E-06：B 的 resume 状态机（Relay controller-resume hooks 与本地
    // ControllerResumeGate）已可运行，声明 resume_v1 让双端协商交集包含恢复族；
    // 否则 A 端声明的 resume_v1 会被交集运算静默剔除。
    "resume_v1",
];

/// 注册声明的能力集合（供「A claim 与 B 注册同族」的一致性测试使用）。
pub(crate) fn registration_capabilities() -> &'static [&'static str] {
    REGISTRATION_CAPABILITIES
}
const CONTROLLED_STATUS_EVENT: &str = "desktop-remote-controlled-status";
/// 握手/协议层失败的本地错误码（有界，绝不透传对端文本）。
const HANDSHAKE_FAILED: &str = "invalid_message";
/// 部署期没有可用的 Relay 信任根：没有它就无法验签 transcript，必须失败关闭。
const RELAY_TRUST_UNCONFIGURED: &str = "relay_trust_unconfigured";

/// B 端单调时钟。注册、握手、watchdog 与状态投影必须共用同一个原点，否则
/// deadline 会由不同基准推导。
pub(crate) fn controlled_monotonic_millis() -> u64 {
    super::clock::monotonic_millis()
}

pub(crate) struct ControlledRelayWorker {
    app: AppHandle,
    relay: RemoteControlManager,
    operation: ControlledOperation,
    code_wrap: CodeWrapKeyPair,
    /// 注册阶段声明、握手接管时一次性移交给 `ControlledHandshakeDriver`。
    handshake: Option<HandshakeKeyPair>,
    channel: crate::features::remote_control::manager::ControllerRelayChannel,
    /// 业务派发端口：由命令层注入（能力快照 / 任务接纳 / 取消）。
    business: std::sync::Arc<dyn ControlledBusinessPort>,
    /// Generation-local action outcomes survive same-lease transport resume.
    /// Dropping the generation owner clears them; every hit is fingerprint-bound.
    action_results: std::sync::Arc<RemoteActionResultCache>,
    business_epoch: std::sync::Arc<AtomicU64>,
}

/// Narrow port over the generation-bound Relay lane. Keeps the registration
/// wait loop testable without a Tauri app handle, and is reused by the later
/// handshake/business dispatch tasks.
#[async_trait::async_trait]
pub(crate) trait ControlledRelayPort: Send {
    fn try_send(&self, message: Value) -> Result<(), String>;
    async fn recv(&mut self) -> Option<Value>;
}

#[async_trait::async_trait]
impl ControlledRelayPort for crate::features::remote_control::manager::ControllerRelayChannel {
    fn try_send(&self, message: Value) -> Result<(), String> {
        crate::features::remote_control::manager::ControllerRelayChannel::try_send(self, message)
    }

    async fn recv(&mut self) -> Option<Value> {
        crate::features::remote_control::manager::ControllerRelayChannel::recv(self).await
    }
}

/// `controller_joined` 之后的租约材料：租约身份来自 Relay，传输指纹由双方
/// 认证过的 transcript 派生。
#[derive(Debug)]
pub(crate) struct ControlledHandshakeOutcome {
    pub(crate) lease: JoinedLease,
    pub(crate) transcript_hash: [u8; 32],
    pub(crate) negotiated_capabilities: Vec<String>,
}

/// B 端握手推进器（E2E-02 第 3 步）：不含网络、不含 Tauri，只按冻结顺序把
/// 入站报文喂给 `ControlledHandshake`，并把要发送的下一帧交给调用方。
pub(crate) struct ControlledHandshakeDriver {
    keys: Option<HandshakeKeyPair>,
    handshake: Option<ControlledHandshake>,
}

impl ControlledHandshakeDriver {
    /// 注册阶段生成的握手密钥在握手接管时移交给 driver（一次性）。
    pub(crate) fn new(keys: HandshakeKeyPair) -> Self {
        Self {
            keys: Some(keys),
            handshake: None,
        }
    }

    pub(crate) fn is_active(&self) -> bool {
        self.handshake.is_some()
    }

    /// `controller_pairing_reserved`：验签 transcript、派生会话密钥与 receipt，
    /// 产出本端 `finished_receipt_commit`。未验签或重复接管都失败关闭。
    pub(crate) fn accept_reserved(
        &mut self,
        message: &Value,
        trust: &AuthenticatedRelayTrustConfig,
    ) -> Result<Value, &'static str> {
        if self.handshake.is_some() {
            return Err("controller_handshake_stage_invalid");
        }
        let keys = self.keys.take().ok_or("controller_handshake_key_invalid")?;
        let handshake =
            ControlledHandshake::new(message, keys, trust).map_err(|error| error.code)?;
        let commit = handshake.commit().map_err(|error| error.code)?;
        self.handshake = Some(handshake);
        Ok(commit)
    }

    /// `finished_receipt_commit`：记录对端 commitment 后产出本端 Finished。
    pub(crate) fn accept_peer_commitment(
        &mut self,
        message: &Value,
    ) -> Result<Value, &'static str> {
        let handshake = self
            .handshake
            .as_mut()
            .ok_or("controller_handshake_stage_invalid")?;
        handshake
            .accept_peer_commitment(message)
            .map_err(|error| error.code)?;
        handshake.finished().map_err(|error| error.code)
    }

    /// `encrypted_handshake_finished`：打开对端 Finished 并核对 commitment 后
    /// 产出本端 `finished_receipt_ack`。
    pub(crate) fn accept_peer_finished(&mut self, message: &Value) -> Result<Value, &'static str> {
        let handshake = self
            .handshake
            .as_mut()
            .ok_or("controller_handshake_stage_invalid")?;
        handshake
            .accept_peer_finished(message)
            .map_err(|error| error.code)
    }

    /// `controller_joined`：只有双收据完成且绑定与验签 transcript 完全一致时
    /// 才产出可安装的租约材料。
    pub(crate) fn accept_joined(
        &mut self,
        message: &Value,
    ) -> Result<ControlledHandshakeOutcome, &'static str> {
        let handshake = self
            .handshake
            .as_mut()
            .ok_or("controller_handshake_stage_invalid")?;
        let lease = handshake
            .accept_joined(message)
            .map_err(|error| error.code)?;
        Ok(ControlledHandshakeOutcome {
            lease,
            transcript_hash: handshake.transcript_hash(),
            negotiated_capabilities: handshake.negotiated_capabilities().to_vec(),
        })
    }

    /// 双收据完成后取出会话密钥（用于安装加密业务通道）；只允许成功一次。
    pub(crate) fn take_session(&mut self) -> Result<SessionKeys, &'static str> {
        let handshake = self
            .handshake
            .as_mut()
            .ok_or("controller_handshake_stage_invalid")?;
        handshake.take_session().map_err(|error| error.code)
    }
}

/// Waits for the registration response for `request`. Unrelated controller
/// frames are consumed without extending the attempt's absolute deadline.
pub(crate) async fn wait_for_registration<P: ControlledRelayPort>(
    port: &mut P,
    request: &Value,
    code_wrap: &CodeWrapKeyPair,
    timeout: std::time::Duration,
) -> Result<ControlledRegistration, String> {
    let deadline = tokio::time::sleep(timeout);
    tokio::pin!(deadline);
    loop {
        tokio::select! {
            biased;
            _ = &mut deadline => return Err("pairing_registration_timeout".to_owned()),
            message = port.recv() => {
                let Some(message) = message else {
                    return Err("desktop_relay_disconnected".to_owned());
                };
                let kind = message.get("type").and_then(Value::as_str);
                match kind {
                    Some("desktop_relay_connection") => {
                        if message.get("connected").and_then(Value::as_bool) == Some(false) {
                            return Err("desktop_relay_disconnected".to_owned());
                        }
                        continue;
                    }
                    Some("desktop_endpoint_registered") => {
                        continue;
                    }
                    Some("desktop_pairing_registered") | Some("error") => {}
                    // The controller lane also carries transport and later-phase
                    // control frames. They are not registration responses and
                    // must never be fed to the registration validator.
                    _ => continue,
                }
                // relay 的拒绝必须保留原始有界码，便于定位（此前被映射成
                // pairing_registration_rejected 后丢失了原因）。
                if kind == Some("error") {
                    let code = message
                        .get("code")
                        .and_then(Value::as_str)
                        .unwrap_or("relay_error");
                    // The shared Relay uses v2 for its outer Web-access lane and
                    // controller-v1 for desktop control. A v2 version rejection
                    // for our v1 registration means the controller dispatcher is
                    // not enabled on that Relay; surface the deployment fault
                    // instead of collapsing it into a generic registration error.
                    if message.get("v").and_then(Value::as_u64) == Some(2)
                        && matches!(
                            code,
                            "unsupported_protocol_version" | "desktop_controller_unavailable"
                        )
                    {
                        log_controlled_event("registration_rejected", code);
                        return Err("desktop_controller_relay_unavailable".to_owned());
                    }
                    // Errors from connection setup or another in-flight request
                    // share this lane but are not replies to this registration.
                    if message.get("request_id") != request.get("request_id") {
                        continue;
                    }
                    log_controlled_event("registration_rejected", code);
                }
                return registration_from_response(code_wrap, request, &message);
            }
        }
    }
}

impl ControlledRelayWorker {
    pub(crate) async fn start(
        app: AppHandle,
        relay: RemoteControlManager,
        operation: ControlledOperation,
        business: std::sync::Arc<dyn ControlledBusinessPort>,
    ) -> Result<oneshot::Receiver<Result<ControlledStatus, String>>, String> {
        let code_wrap = CodeWrapKeyPair::generate().map_err(|_| "pairing_key_invalid")?;
        let handshake = HandshakeKeyPair::generate().map_err(|_| "handshake_key_invalid")?;
        let channel = relay.start_controller_relay(operation.generation()).await?;
        log_controlled_event("worker_started", "channel_ready");
        let (done_tx, done_rx) = oneshot::channel();
        let mut worker = Self {
            app,
            relay,
            operation,
            code_wrap,
            handshake: Some(handshake),
            channel,
            business,
            action_results: std::sync::Arc::new(RemoteActionResultCache::default()),
            business_epoch: std::sync::Arc::new(AtomicU64::new(0)),
        };
        tauri::async_runtime::spawn(async move {
            let result = worker.register_pairing().await;
            if let Err(error) = &result {
                worker.fail_registration(error);
            }
            let _ = done_tx.send(result);
            // Keep the generation-bound receiver alive after registration. Later
            // E2E tasks install handshake and encrypted business dispatch here.
            worker.serve().await;
        });
        Ok(done_rx)
    }

    async fn register_pairing(&mut self) -> Result<ControlledStatus, String> {
        let manager = self.app.state::<ControlledDeviceManager>();
        let registration_request_id = manager
            .registration_request_id(self.operation.clone())
            .map_err(|error| error.code.to_owned())?;
        // 注册帧必须在端点 socket 已认证为 B 之后发出，否则会被 relay 以角色无效
        // 拒绝；同时网络抖动可能让端点通道 20 秒内都没就绪，因此整体**有界重试**：
        // 每次重试复用同一个 registration_request_id，relay 侧按幂等键返回同一结果。
        const MAX_ATTEMPTS: u32 = 3;
        for attempt in 1..=MAX_ATTEMPTS {
            match self.relay.wait_endpoint_ready(ENDPOINT_READY_TIMEOUT).await {
                Ok(()) => log_controlled_event("endpoint_ready", "ok"),
                Err(error) => {
                    log_controlled_event("endpoint_ready", &error);
                    if attempt < MAX_ATTEMPTS {
                        tokio::time::sleep(ENDPOINT_READY_RETRY_DELAY).await;
                        continue;
                    }
                    return Err(error);
                }
            }
            let endpoint_id = self.relay.controller_endpoint_id()?;
            let request = registration_request(
                &registration_request_id,
                &endpoint_id,
                self.operation.generation(),
                &self.code_wrap,
                self.handshake
                    .as_ref()
                    .ok_or_else(|| "handshake_key_invalid".to_owned())?,
            )?;
            self.channel.try_send(request.clone())?;
            // 区分「连注册帧都没发出去」与「发出后等待/被拒」这两类失败。
            log_controlled_event("registration_sent", "waiting_registered");
            match wait_for_registration(
                &mut self.channel,
                &request,
                &self.code_wrap,
                std::time::Duration::from_millis(REGISTRATION_TIMEOUT_MS),
            )
            .await
            {
                Ok(mut registration) => {
                    let now_b = controlled_monotonic_millis();
                    registration.expires_at_ms = pairing_deadline_b(
                        registration.expires_at_ms,
                        wall_clock_millis()?,
                        now_b,
                    )?;
                    let status = manager
                        .complete_registration(self.operation.clone(), registration, now_b)
                        .map_err(|error| error.code.to_owned())?;
                    log_controlled_event("registered", "waiting_for_pairing");
                    emit_controlled_status(&self.app, &status);
                    return Ok(status);
                }
                Err(error) => {
                    log_controlled_event("registration_failed", &error);
                    let retryable = matches!(
                        error.as_str(),
                        "pairing_registration_timeout" | "desktop_relay_disconnected"
                    );
                    if retryable && attempt < MAX_ATTEMPTS {
                        tokio::time::sleep(ENDPOINT_READY_RETRY_DELAY).await;
                        continue;
                    }
                    return Err(error);
                }
            }
        }
        Err("pairing_registration_failed".to_owned())
    }

    fn fail_registration(&self, error: &str) {
        log_controlled_event("registration_failed", &sanitize_error(error));
        let manager = self.app.state::<ControlledDeviceManager>();
        if !manager.is_pending_handshake(self.operation.clone()) {
            // 用户已停止/断开，或本代已被刷新取代：不覆盖既有投影。
            return;
        }
        let bounded = sanitize_error(error);
        if let Ok(status) = manager.fail_operation(self.operation.clone(), bounded) {
            emit_controlled_status(&self.app, &status);
        }
    }

    /// E2E-02 第 3 步：`controller_pairing_reserved` 到达后用
    /// `ControlledHandshake` 接管，按冻结顺序完成双收据交换，并把
    /// `controller_joined` 应用到设备所有者。
    ///
    /// 任何顺序错误、绑定不符或验签失败都失败关闭（投影为有界错误的 error
    /// 状态并结束本代通道）；握手完成后的帧只做排空，业务帧派发属 E2E-04，
    /// 未知帧绝不转发到 legacy WebUI 事件流。
    async fn serve(mut self) {
        let Some(keys) = self.handshake.take() else {
            self.fail_handshake(HANDSHAKE_FAILED);
            return;
        };
        let mut driver = ControlledHandshakeDriver::new(keys);
        let mut joined = false;
        let mut heartbeat = super::controlled_heartbeat::ControlledHeartbeat::default();
        let mut evidence = LivenessEvidenceState::default();
        let mut resume = ControlledResumeAttempt::default();
        let business_worker = self.business_worker();
        let mut poll =
            tokio::time::interval(std::time::Duration::from_millis(LIVENESS_EVIDENCE_POLL_MS));
        poll.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        poll.tick().await;
        'serve: loop {
            tokio::select! {
                biased;
                _ = poll.tick() => {
                    if joined {
                        let manager = self.app.state::<ControlledDeviceManager>();
                        let before = manager.status().kind();
                        let Ok(status) = manager.poll_watchdog_for(&self.operation, controlled_monotonic_millis()) else {
                            break 'serve;
                        };
                        if status.kind() != before {
                            emit_controlled_status(&self.app, &status);
                        }
                        if matches!(status.kind(), ControlledStatusKind::Error | ControlledStatusKind::Stopped) {
                            break 'serve;
                        }
                    }
                    if joined && !resume.is_active() {
                        if let Err(code) = heartbeat.pump(
                            self.app.state::<ControlledDeviceManager>().inner(),
                            &self.operation, resume.is_active(), &self.channel,
                        ) {
                            self.fail_runtime(&code);
                            break 'serve;
                        }
                        self.pump_liveness_evidence(&mut evidence);
                    }
                    continue 'serve;
                }
                message = self.channel.recv() => {
                    let Some(message) = message else { break 'serve };
                    if message.get("type").and_then(Value::as_str) == Some("desktop_controller_stop") {
                        break 'serve;
                    }
                    if joined {
                        // E2E-06：恢复进行中时所有帧都由恢复状态机处理，业务帧与
                        // 存活证据一律不派发（Relay 侧同样禁止业务帧）。
                        if resume.is_active() {
                            let manager = self.app.state::<ControlledDeviceManager>();
                            let lane = self.app.state::<ControlledLaneState>();
                            match handle_controlled_resume_frame(
                                &self.channel,
                                manager.inner(),
                                lane.inner(),
                                &mut resume,
                                &message,
                            ) {
                                ControlledResumeOutcome::Failed => {
                                    self.fail_runtime("desktop_remote_resume_rejected");
                                    break 'serve;
                                },
                                ControlledResumeOutcome::Completed => {
                                    let epoch = message.get("liveness_epoch_b").and_then(Value::as_str).unwrap_or_default();
                                    let Ok((lease, _)) = manager.liveness_runtime_context() else { break 'serve };
                                    if heartbeat.install(lease, epoch).is_err() {
                                        self.fail_runtime(HANDSHAKE_FAILED);
                                        break 'serve;
                                    }
                                    evidence = LivenessEvidenceState::default();
                                    let Ok(status) = manager.poll_watchdog_for(&self.operation, controlled_monotonic_millis()) else { break 'serve };
                                    emit_controlled_status(&self.app, &status);
                                }
                                ControlledResumeOutcome::Progressed => {
                                    let Ok(status) = manager.poll_watchdog_for(&self.operation, controlled_monotonic_millis()) else { break 'serve };
                                    emit_controlled_status(&self.app, &status);
                                }
                                ControlledResumeOutcome::Ignored => {}
                            }
                            continue 'serve;
                        }
                        match message.get("type").and_then(Value::as_str) {
                            Some("encrypted_frame") => {
                                if let Some(job) = self.admit_business_frame(&message) {
                                    if business_worker.try_submit(job).is_err() {
                                        if let Ok(status) = self.app.state::<ControlledDeviceManager>()
                                            .fail_operation(self.operation.clone(), "desktop_remote_business_queue_full".into()) {
                                            emit_controlled_status(&self.app, &status);
                                        }
                                        break 'serve;
                                    }
                                }
                            }
                            // E2E-06：Relay 签名兜底证明 → 续期 B 的 watchdog。
                            Some("controller_liveness_attestation_a") => {
                                self.accept_liveness_attestation(&message, &mut evidence);
                            }
                            // E2E-06：A 的直接存活证明 → 优先于兜底，续期后不再升级。
                            Some("encrypted_controller_liveness_a") => {
                                self.accept_direct_liveness(&message, &mut evidence);
                            }
                            Some("error") => {
                                let code = message
                                    .get("code")
                                    .and_then(Value::as_str)
                                    .unwrap_or("relay_error");
                                log_controlled_event("runtime_relay_error", code);
                            }
                            // E2E-06：A 端显式断开 → 终结租约并结束本代通道。
                            Some("controller_disconnect") => {
                                if self.accept_remote_disconnect(&message) {
                                    break 'serve;
                                }
                            }
                            // E2E-06：desktop socket 掉线 → 挂起业务通道并启动恢复。
                            Some("desktop_relay_connection") => {
                                let connected = message
                                    .get("connected")
                                    .and_then(Value::as_bool)
                                    .unwrap_or(true);
                                if !connected {
                                    let manager = self.app.state::<ControlledDeviceManager>();
                                    let lane = self.app.state::<ControlledLaneState>();
                                    // Do not abort a potentially executed remote action
                                    // before it caches its result. Retain one executor so
                                    // resumed retries cannot overtake it; discard only
                                    // unstarted work and fence old-epoch response frames.
                                    if manager.with_local_cancel_lease(|_| {
                                        self.business_epoch.fetch_add(1, Ordering::AcqRel);
                                        lane.suspend();
                                    }).is_err() {
                                        self.business_epoch.fetch_add(1, Ordering::AcqRel);
                                        lane.suspend();
                                    }
                                    business_worker.clear_pending();
                                    if !begin_controlled_resume(
                                        &self.channel,
                                        manager.inner(),
                                        &mut resume,
                                    ) {
                                        break 'serve;
                                    }
                                }
                            }
                            _ => {}
                        }
                        continue 'serve;
                    }
                    // Relay 会把本端 commit 原样回投（router 契约，见
                    // `controller-router.test.js`）。自帧回投不是对端输入，跳过而不是
                    // 失败关闭：否则 B 会在收到自己 commitment 的瞬间以
                    // `controller_handshake_rejected` 结束整条通道（2026-09-20 真机）。
                    if skip_self_echoed_frame(&message) {
                        continue 'serve;
                    }
                    let result = match message.get("type").and_then(Value::as_str) {
                        Some("controller_pairing_reserved") => {
                            self.begin_handshake(&message, &mut driver)
                        }
                        Some("finished_receipt_commit") => driver
                            .accept_peer_commitment(&message)
                            .map(Some)
                            .map_err(str::to_owned),
                        Some("encrypted_handshake_finished") => driver
                            .accept_peer_finished(&message)
                            .map(Some)
                            .map_err(str::to_owned),
                        Some("controller_joined") => {
                            match self.complete_handshake(&message, &mut driver) {
                                Ok(()) => {
                                    let manager = self.app.state::<ControlledDeviceManager>();
                                    let Ok((lease, _)) = manager.liveness_runtime_context() else { return };
                                    let epoch = message.get("liveness_epoch_b").and_then(Value::as_str).unwrap_or_default();
                                    if heartbeat.install(lease, epoch).is_err() {
                                        self.fail_runtime(HANDSHAKE_FAILED);
                                        return;
                                    }
                                    joined = true;
                                    Ok(None)
                                }
                                Err(code) => Err(code),
                            }
                        }
                        _ => Ok(None),
                    };
                    match result {
                        Ok(Some(frame)) => {
                            if let Err(error) = self.channel.try_send(frame) {
                                self.fail_handshake(&error);
                                return;
                            }
                            if message.get("type").and_then(Value::as_str)
                                == Some("controller_pairing_reserved")
                            {
                                let manager = self.app.state::<ControlledDeviceManager>();
                                match manager.mark_handshake_pending(self.operation.clone()) {
                                    Ok(status) => {
                                        emit_controlled_status(&self.app, &status);
                                    }
                                    Err(error) => {
                                        self.fail_handshake(error.code);
                                        return;
                                    }
                                }
                            }
                        }
                        Ok(None) => {}
                        Err(code) => {
                            self.fail_handshake(&code);
                            return;
                        }
                    }
                }
            }
        }
        drop(business_worker);
        // 通道关闭（stop/refresh/断线）时丢弃出站旁路，避免旧 generation 继续回传。
        self.app
            .state::<ControlledOutboundState>()
            .clear_generation(self.channel.generation());
    }

    /// E2E-06：按 watchdog 需求发起挑战 / 升级到 Relay 签名兜底 / 作废超时挑战。
    fn pump_liveness_evidence(&self, evidence: &mut LivenessEvidenceState) {
        let manager = self.app.state::<ControlledDeviceManager>();
        let now_b = controlled_monotonic_millis();
        let needs = manager.liveness_evidence_needed(now_b);
        match liveness_evidence_action(needs, evidence, now_b) {
            LivenessEvidenceAction::Idle => {}
            LivenessEvidenceAction::Challenge => {
                let Ok((lease, _)) = manager.liveness_runtime_context() else {
                    return;
                };
                let Ok((challenge_id, challenge_b64)) = manager.begin_liveness_challenge(now_b)
                else {
                    return;
                };
                let request_id = format!("request-liveness-{}", uuid::Uuid::new_v4().simple());
                let Ok(frame) = super::liveness::build_liveness_challenge(
                    &lease,
                    &request_id,
                    &challenge_id,
                    &challenge_b64,
                ) else {
                    manager.cancel_liveness_challenge(&challenge_id);
                    return;
                };
                if self.channel.try_send(frame).is_ok() {
                    evidence.outstanding = Some((challenge_id, challenge_b64, now_b));
                    evidence.escalated = false;
                    evidence.attestation_sent_at_b = None;
                    log_controlled_event("liveness_challenge", "sent");
                } else {
                    manager.cancel_liveness_challenge(&challenge_id);
                    log_controlled_event("liveness_challenge", "send_failed");
                }
            }
            LivenessEvidenceAction::Escalate => {
                let Ok((lease, _)) = manager.liveness_runtime_context() else {
                    return;
                };
                let Some((challenge_id, challenge_b64, _)) = evidence.outstanding.clone() else {
                    return;
                };
                let request_id = format!("request-attest-{}", uuid::Uuid::new_v4().simple());
                let Ok(frame) = super::liveness::build_liveness_attestation_request(
                    &lease,
                    &request_id,
                    &challenge_id,
                    &challenge_b64,
                ) else {
                    return;
                };
                if self.channel.try_send(frame).is_ok() {
                    evidence.record_attestation_sent(now_b);
                    log_controlled_event("liveness_attestation", "requested");
                }
            }
            LivenessEvidenceAction::Reset => {
                if let Some((challenge_id, _, _)) = evidence.outstanding.take() {
                    manager.cancel_liveness_challenge(&challenge_id);
                }
                evidence.escalated = false;
                evidence.attestation_sent_at_b = None;
                log_controlled_event("liveness_challenge", "expired");
            }
        }
    }

    /// E2E-06：A 端显式断开（`controller_disconnect`）。返回 true 表示已被接受，
    /// 本代通道应当结束（租约、会话密钥与远端任务都已清理）。
    fn accept_remote_disconnect(&self, message: &Value) -> bool {
        // 先做冻结校验（角色 A + 租约 + 能力），再交给状态所有者终结租约。
        let manager = self.app.state::<ControlledDeviceManager>();
        let Ok((lease, capabilities)) = manager.live_runtime_context() else {
            return false;
        };
        let context =
            crate::features::remote_control::controller::protocol::RuntimeValidationContext {
                lease: Some(lease),
                negotiated_capabilities: capabilities,
            };
        let validated = crate::features::remote_control::controller::protocol::validate_controller_runtime_message_json(
            &message.to_string(),
            crate::features::remote_control::controller::protocol::SenderRole::ControllerA,
            Some(&context),
        );
        if validated.is_err() {
            return false;
        }
        match manager.accept_remote_disconnect() {
            Ok(status) => {
                self.app.state::<ControlledLaneState>().clear();
                self.app.state::<ControlledOutboundState>().clear();
                emit_controlled_status(&self.app, &status);
                true
            }
            Err(_) => false,
        }
    }

    /// E2E-06：接受 Relay 签名 attestation 续期本端 watchdog（挑战与请求时间
    /// 由本端记录，跨机时间一律不用）。
    /// E2E-06：A 的直接存活证明（`encrypted_controller_liveness_a`）：解密后用
    /// 明文走 `accept_direct_liveness`（挑战、RTT 与序号单调性由 watchdog 校验），
    /// 成功后清除未完成挑战，避免再走 Relay 兜底。
    fn accept_direct_liveness(&self, frame: &Value, evidence: &mut LivenessEvidenceState) {
        let manager = self.app.state::<ControlledDeviceManager>();
        let business = self.app.state::<ControlledLaneState>();
        let Ok(inner) = business.open_liveness(frame) else {
            return;
        };
        let Some(challenge_id) = inner.get("challenge_id").and_then(Value::as_str) else {
            return;
        };
        match &evidence.outstanding {
            Some((outstanding, _, _)) if outstanding == challenge_id => {}
            // 不是当前挑战的证明（重放或过期）一律忽略。
            _ => return,
        }
        let now_b = controlled_monotonic_millis();
        match manager.accept_direct_liveness(&inner.to_string(), now_b) {
            Ok(()) => {
                evidence.outstanding = None;
                evidence.escalated = false;
                evidence.attestation_sent_at_b = None;
                log_controlled_event("liveness_direct", "accepted");
            }
            Err(error) => log_controlled_event("liveness_direct", error.code),
        }
    }

    /// E2E-06：接受 Relay 签名 attestation 续期本端 watchdog（挑战与请求时间
    /// 由本端记录，跨机时间一律不用）。
    fn accept_liveness_attestation(&self, message: &Value, evidence: &mut LivenessEvidenceState) {
        let Some(sent_at_b) = evidence.attestation_request_start(message) else {
            return;
        };
        let Ok(trust) = load_relay_trust_config(|name| std::env::var(name).ok()) else {
            // Missing/invalid administrator pins cannot authorize liveness or
            // consume the outstanding challenge. The watchdog remains closed.
            return;
        };
        let manager = self.app.state::<ControlledDeviceManager>();
        let now_b = controlled_monotonic_millis();
        match manager.accept_liveness_attestation(&message.to_string(), &trust, sent_at_b, now_b) {
            Ok(()) => {
                evidence.outstanding = None;
                evidence.escalated = false;
                evidence.attestation_sent_at_b = None;
                log_controlled_event("liveness_attestation", "accepted");
            }
            Err(error) => log_controlled_event("liveness_attestation", error.code),
        }
    }

    /// 接管握手：本地进入安全握手阶段并产出本端 commit。信任根缺失或非法时
    /// 失败关闭，绝不降级为无认证握手。
    fn begin_handshake(
        &self,
        message: &Value,
        driver: &mut ControlledHandshakeDriver,
    ) -> Result<Option<Value>, String> {
        let trust = load_relay_trust_config(|name| std::env::var(name).ok())
            .map_err(|_| RELAY_TRUST_UNCONFIGURED.to_owned())?;
        let manager = self.app.state::<ControlledDeviceManager>();
        let commit = driver
            .accept_reserved(message, &trust)
            .map_err(str::to_owned)?;
        let status = manager
            .mark_pairing_reserved(self.operation.clone())
            .map_err(|error| error.code.to_owned())?;
        emit_controlled_status(&self.app, &status);
        Ok(Some(commit))
    }

    /// 应用 `controller_joined`：构造 B 端租约（含双方一致的传输指纹）并进入
    /// connected。租约未真正落地前不投影 connected。
    fn complete_handshake(
        &self,
        message: &Value,
        driver: &mut ControlledHandshakeDriver,
    ) -> Result<(), String> {
        let outcome = driver
            .accept_joined(message)
            .map_err(|code| code.to_owned())?;
        let lease = controlled_lease_from_outcome(outcome, controlled_monotonic_millis())?;
        let manager = self.app.state::<ControlledDeviceManager>();
        // 加密业务通道先于 connected 就位：状态一旦是 connected，业务帧既有租约
        // 准入也有会话密钥可用。
        let Ok(session) = driver.take_session() else {
            return Err(self.fail_completed_handshake(&manager));
        };
        let business = self.app.state::<ControlledLaneState>();
        business.clear();
        if business
            .install(super::crypto::ControllerRole::B, &lease.context, session)
            .is_err()
        {
            return Err(self.fail_completed_handshake(&manager));
        }
        // 出站旁路（journal 事件回传）与通道一起就位。
        let outbound = self.app.state::<ControlledOutboundState>();
        outbound.install(self.channel.sender());
        let status = manager
            .complete_connected(self.operation.clone(), lease)
            .map_err(|error| error.code.to_owned())?;
        emit_controlled_status(&self.app, &status);
        Ok(())
    }

    /// 握手成功后本地收尾失败（无权安装会话密钥/状态竞争）：失败关闭并返回
    /// 有界错误码，绝不留下「connected 但无业务通道」的状态。
    fn fail_completed_handshake(&self, manager: &ControlledDeviceManager) -> String {
        let business = self.app.state::<ControlledLaneState>();
        business.clear();
        if let Ok(status) =
            manager.fail_operation(self.operation.clone(), HANDSHAKE_FAILED.to_owned())
        {
            emit_controlled_status(&self.app, &status);
        }
        HANDSHAKE_FAILED.to_owned()
    }

    /// 握手失败收尾：投影为有界错误并结束本代通道（dropping the channel 会
    /// 清掉该 generation 的通道绑定，本地停止/刷新入口保持可用）。
    fn fail_handshake(&self, error: &str) {
        log_controlled_event("handshake_failed", &sanitize_error(error));
        let manager = self.app.state::<ControlledDeviceManager>();
        if !manager.is_pending_handshake(self.operation.clone()) {
            // 用户已停止/断开，或本代已被刷新取代：不覆盖既有投影。
            return;
        }
        let bounded = sanitize_error(error);
        if let Ok(status) = manager.fail_operation(self.operation.clone(), bounded) {
            emit_controlled_status(&self.app, &status);
        }
    }

    fn fail_runtime(&self, error: &str) {
        let manager = self.app.state::<ControlledDeviceManager>();
        // Exact operation validation precedes all cleanup. A retired worker
        // must not clear the replacement generation's keys or projection.
        if let Ok(status) = manager.fail_operation(self.operation.clone(), sanitize_error(error)) {
            emit_controlled_status(&self.app, &status);
        }
    }

    /// 已认证业务帧：解密 → 派发 → 用同一通道加密回包。任何一层不符（无活租约、
    /// 解密/校验失败、业务拒绝）都不产生副作用；业务拒绝时回传有界错误的
    /// `task_status` 由命令层决定（这里只负责通道语义）。
    fn admit_business_frame(&self, frame: &Value) -> Option<ControlledBusinessJob> {
        let is_task_submit =
            frame.get("payload_kind").and_then(Value::as_str) == Some("task_submit");
        if is_task_submit {
            log_controlled_event("task_submit_frame_received", "opening");
        }
        let manager = self.app.state::<ControlledDeviceManager>();
        let Ok((lease, capabilities)) = manager.live_runtime_context() else {
            log_controlled_event("business_frame_dropped", "no_live_lease");
            return None;
        };
        let business = self.app.state::<ControlledLaneState>();
        let (payload_kind, payload) = match business.open(frame, &capabilities) {
            Ok(opened) => opened,
            Err(error) => {
                log_controlled_event("business_frame_rejected", error.code());
                return None;
            }
        };
        if payload_kind == "task_submit" {
            log_controlled_event("task_submit_decrypted", "dispatching");
        }
        Some(ControlledBusinessJob {
            transport_epoch: self.business_epoch.load(Ordering::Acquire),
            lease,
            capabilities,
            payload_kind: payload_kind.to_owned(),
            payload,
        })
    }

    fn business_worker(
        &self,
    ) -> super::business_worker::SerialBusinessWorker<ControlledBusinessJob> {
        let app = self.app.clone();
        let port = self.business.clone();
        let cache = self.action_results.clone();
        let sender = self.channel.sender();
        let epoch = self.business_epoch.clone();
        // Each validated plaintext is <=512KiB. Four queued jobs plus one
        // in-flight job bound memory and retain one strictly sequential executor.
        super::business_worker::SerialBusinessWorker::start(4, move |job| {
            let app = app.clone();
            let port = port.clone();
            let cache = cache.clone();
            let sender = sender.clone();
            let epoch = epoch.clone();
            async move {
                dispatch_controlled_business_job(&app, port.as_ref(), &cache, &sender, &epoch, job)
                    .await;
            }
        })
    }
}

struct ControlledBusinessJob {
    transport_epoch: u64,
    lease: LeaseContext,
    capabilities: Vec<String>,
    payload_kind: String,
    payload: Value,
}

async fn dispatch_controlled_business_job(
    app: &AppHandle,
    port: &dyn ControlledBusinessPort,
    cache: &RemoteActionResultCache,
    sender: &crate::features::remote_control::manager::ControllerRelaySender,
    epoch: &AtomicU64,
    job: ControlledBusinessJob,
) {
    let ControlledBusinessJob {
        transport_epoch,
        lease,
        capabilities,
        payload_kind,
        payload,
    } = job;
    let manager = app.state::<ControlledDeviceManager>();
    if epoch.load(Ordering::Acquire) != transport_epoch
        || manager.with_current_business_lease(&lease, || ()).is_err()
    {
        return;
    }
    let is_task_submit = payload_kind == "task_submit";
    let responses =
        match dispatch_decrypted_payload_cached(port, cache, &lease, &payload_kind, &payload).await
        {
            Ok(responses) => responses,
            Err(error) => {
                log_controlled_event("business_dispatch_failed", &error);
                if is_task_submit {
                    log_controlled_event("task_submit_dispatch_failed", &error);
                }
                return;
            }
        };
    if is_task_submit {
        log_controlled_event("task_submit_dispatched", "accepted");
    }
    // Async business may finish after stop/disconnect/re-pair. Seal/send while
    // pinned to the same current live lease; no late response may enter a new
    // transport cycle or resurrect suspended business authority.
    let _ = manager.with_current_business_lease(&lease, || {
        if epoch.load(Ordering::Acquire) != transport_epoch {
            return;
        }
        let business = app.state::<ControlledLaneState>();
        // 一条请求可能产出多帧（journal 回放），逐帧加密回传；单帧失败即停止。
        for (response_kind, response) in responses {
            let frame = match business.seal(response_kind, &response, &capabilities) {
                Ok(frame) => frame,
                Err(error) => {
                    log_controlled_event("business_response_seal_failed", error.code());
                    return;
                }
            };
            if sender.try_send(frame).is_err() {
                log_controlled_event("business_response_send_failed", response_kind);
                return;
            }
            if is_task_submit && response_kind == "task_status" {
                log_controlled_event("task_submit_status_sent", "accepted");
            }
            if matches!(
                response_kind,
                "capability_snapshot" | "capability_snapshot_error"
            ) {
                log_controlled_event("capability_response_sent", response_kind);
            }
        }
    });
}

pub(crate) use super::liveness::{LIVENESS_ATTESTATION_ESCALATION_MS, LIVENESS_EVIDENCE_POLL_MS};
/// 挑战的最长等待：超过后作废并重新发起。
pub(crate) const LIVENESS_CHALLENGE_TIMEOUT_MS: u64 = 8_000;

/// 本轮应当采取的证据动作（纯函数判定，worker 只负责发帧）。
#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum LivenessEvidenceAction {
    /// 余量充足，不打扰对端。
    Idle,
    /// 发起新的存活挑战。
    Challenge,
    /// 用同一挑战索取 Relay 签名 attestation。
    Escalate,
    /// 旧挑战超时作废，等待下一轮重新发起。
    Reset,
}

/// 证据状态：当前未完成的挑战（id、挑战值、发出时刻）与是否已升级过。
#[derive(Debug, Default, Clone, Eq, PartialEq)]
pub(crate) struct LivenessEvidenceState {
    pub(crate) outstanding: Option<(String, String, u64)>,
    pub(crate) escalated: bool,
    // Separate request RTT from the original direct challenge age. Neither
    // timestamp changes the frozen disconnect/recovery deadline.
    pub(crate) attestation_sent_at_b: Option<u64>,
}

impl LivenessEvidenceState {
    fn record_attestation_sent(&mut self, now_b: u64) {
        self.escalated = true;
        self.attestation_sent_at_b = Some(now_b);
    }

    fn attestation_request_start(&self, message: &Value) -> Option<u64> {
        let (challenge_id, _, _) = self.outstanding.as_ref()?;
        if !self.escalated
            || message.get("challenge_id").and_then(Value::as_str) != Some(challenge_id.as_str())
        {
            return None;
        }
        self.attestation_sent_at_b
    }
}

/// 依据 watchdog 的需求与当前未完成挑战推导本轮动作。
pub(crate) fn liveness_evidence_action(
    needs_evidence: bool,
    state: &LivenessEvidenceState,
    now_b: u64,
) -> LivenessEvidenceAction {
    if !needs_evidence {
        return LivenessEvidenceAction::Idle;
    }
    match &state.outstanding {
        None => LivenessEvidenceAction::Challenge,
        Some((_, _, sent_at)) => {
            let age = now_b.saturating_sub(*sent_at);
            if state.escalated {
                // Preserve a full fallback request RTT without changing the
                // original challenge validity or frozen recovery cutoff.
                return if age >= super::liveness::LIVENESS_TOTAL_BUDGET_MS
                    || state.attestation_sent_at_b.is_none_or(|request_start| {
                        now_b.saturating_sub(request_start) > super::liveness::MAX_LIVENESS_RTT_MS
                    }) {
                    LivenessEvidenceAction::Reset
                } else {
                    LivenessEvidenceAction::Idle
                };
            }
            if age >= LIVENESS_CHALLENGE_TIMEOUT_MS {
                return LivenessEvidenceAction::Reset;
            }
            if !state.escalated && age >= LIVENESS_ATTESTATION_ESCALATION_MS {
                return LivenessEvidenceAction::Escalate;
            }
            LivenessEvidenceAction::Idle
        }
    }
}

/// B 端业务端口：worker 只负责解密/映射/回传，真正的业务（能力快照、任务
/// 接纳与取消）由命令层实现，避免两条实现漂移。
#[async_trait::async_trait]
pub(crate) trait ControlledBusinessPort: Send + Sync {
    /// A closed, lease-bound desktop action. The implementation resolves the
    /// authoritative B session before touching permission or transfer state.
    async fn remote_action(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        operation: &str,
        body: &Value,
    ) -> Result<Value, String>;

    /// 构造 wire `capability_snapshot`（`known_revision` 命中未过期快照时复用）。
    async fn capability_snapshot(
        &self,
        lease: &LeaseContext,
        request_id: &str,
        session_id: &str,
        known_revision: u64,
    ) -> Result<Value, String>;

    /// 接纳/派发一条 `task_submit`，返回该任务的 wire `task_status` 载荷。
    /// `session_ref` 是 A 端声明并在 wire 上使用的会话引用。
    async fn submit_task(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        payload: &Value,
    ) -> Result<Value, String>;

    /// 取消一条任务，返回 wire `task_status` 载荷（若服务不产出则返回 None）。
    async fn cancel_task(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        task_id: &str,
    ) -> Result<Option<Value>, String>;

    /// E2E-05 缺口恢复：按 `after_sequence` 回放权威事件；缺口太旧时返回一份
    /// `session_snapshot`，因此返回的是 (wire payload_kind, payload) 列表。
    async fn replay_journal(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        journal_epoch: &str,
        after_sequence: u64,
    ) -> Result<Vec<(String, Value)>, String>;

    /// E2E-05 缺口恢复：给出一份权威快照（`session_snapshot` wire 载荷）。
    async fn session_snapshot(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        request_id: &str,
    ) -> Result<Value, String>;
}

/// 解密后的业务帧派发（纯函数，便于测试）：按 payload_kind 调用业务端口，
/// 并把回包里的 `session_id` 统一改写回 A 端的会话引用，保证两侧同一 wire 身份。
pub(crate) async fn dispatch_decrypted_payload<P: ControlledBusinessPort + ?Sized>(
    port: &P,
    lease: &LeaseContext,
    payload_kind: &str,
    payload: &Value,
) -> Result<Vec<(&'static str, Value)>, String> {
    let action_results = RemoteActionResultCache::default();
    dispatch_decrypted_payload_cached(port, &action_results, lease, payload_kind, payload).await
}

async fn dispatch_decrypted_payload_cached<P: ControlledBusinessPort + ?Sized>(
    port: &P,
    action_results: &RemoteActionResultCache,
    lease: &LeaseContext,
    payload_kind: &str,
    payload: &Value,
) -> Result<Vec<(&'static str, Value)>, String> {
    match payload_kind {
        "remote_action_request" => {
            let session_ref = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "remote_action_invalid".to_owned())?;
            let request_id = payload
                .get("request_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "remote_action_invalid".to_owned())?;
            let operation = payload
                .get("operation")
                .and_then(Value::as_str)
                .ok_or_else(|| "remote_action_invalid".to_owned())?;
            let body = payload
                .get("body")
                .ok_or_else(|| "remote_action_invalid".to_owned())?;
            let fingerprint =
                remote_action_fingerprint(lease, session_ref, request_id, operation, body);
            log_remote_action_event(
                "controlled",
                "remote_action_received",
                operation,
                "requested",
            );
            match action_results.lookup(request_id, fingerprint) {
                RemoteActionCacheLookup::Hit(response) => {
                    log_remote_action_event(
                        "controlled",
                        "remote_action_completed",
                        operation,
                        "duplicate",
                    );
                    return Ok(vec![("remote_action_result", response)]);
                }
                RemoteActionCacheLookup::Conflict => {
                    log_remote_action_event(
                        "controlled",
                        "remote_action_failed",
                        operation,
                        "result_unknown",
                    );
                    return Ok(vec![(
                        "remote_action_result",
                        remote_action_response(
                            lease,
                            session_ref,
                            request_id,
                            operation,
                            Err("result_unknown".to_owned()),
                        ),
                    )]);
                }
                RemoteActionCacheLookup::Miss => {}
            }
            let action = port
                .remote_action(lease, session_ref, operation, body)
                .await;
            match &action {
                Ok(_) => log_remote_action_event(
                    "controlled",
                    "remote_action_completed",
                    operation,
                    "ok",
                ),
                Err(code) => log_remote_action_event(
                    "controlled",
                    "remote_action_failed",
                    operation,
                    public_remote_action_error(code),
                ),
            }
            let response =
                remote_action_response(lease, session_ref, request_id, operation, action);
            action_results.insert(request_id.to_owned(), fingerprint, response.clone());
            Ok(vec![("remote_action_result", response)])
        }
        "capability_snapshot_request" => {
            let request_id = payload
                .get("request_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let known_revision = payload
                .get("known_revision")
                .and_then(Value::as_str)
                .and_then(|value| value.parse::<u64>().ok())
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let session_id = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            match port
                .capability_snapshot(lease, request_id, session_id, known_revision)
                .await
            {
                Ok(response) => Ok(vec![("capability_snapshot", response)]),
                Err(_) => Ok(vec![(
                    "capability_snapshot_error",
                    capability_snapshot_error(lease, request_id, session_id),
                )]),
            }
        }
        "task_submit" => {
            let session_ref = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?
                .to_owned();
            let response = port.submit_task(lease, &session_ref, payload).await?;
            Ok(vec![(
                "task_status",
                rewrite_session_id(response, &session_ref)?,
            )])
        }
        "task_cancel" => {
            let session_ref = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?
                .to_owned();
            let task_id = payload
                .get("task_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            match port.cancel_task(lease, &session_ref, task_id).await? {
                Some(response) => Ok(vec![(
                    "task_status",
                    rewrite_session_id(response, &session_ref)?,
                )]),
                None => Ok(Vec::new()),
            }
        }
        "journal_replay_request" => {
            let session_ref = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let journal_epoch = payload
                .get("journal_epoch")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let after_sequence = payload
                .get("after_sequence")
                .and_then(Value::as_str)
                .and_then(|value| value.parse::<u64>().ok())
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let frames = port
                .replay_journal(lease, session_ref, journal_epoch, after_sequence)
                .await?;
            Ok(frames
                .into_iter()
                .filter_map(|(kind, payload)| match kind.as_str() {
                    "journal_event" => Some(("journal_event", payload)),
                    "session_snapshot" => Some(("session_snapshot", payload)),
                    _ => None,
                })
                .collect())
        }
        "session_snapshot_request" => {
            let session_ref = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let request_id = payload
                .get("request_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let snapshot = port
                .session_snapshot(lease, session_ref, request_id)
                .await?;
            Ok(vec![("session_snapshot", snapshot)])
        }
        // liveness / 审批 / 附件 / 成果等能力属后续任务：未知类型一律丢弃。
        _ => Ok(Vec::new()),
    }
}

fn remote_action_response(
    lease: &LeaseContext,
    session_ref: &str,
    request_id: &str,
    operation: &str,
    action: Result<Value, String>,
) -> Value {
    let (ok, body, error_code) = match action {
        Ok(body) => (true, body, None),
        Err(code) => (
            false,
            serde_json::json!({}),
            Some(public_remote_action_error(&code)),
        ),
    };
    let mut response = serde_json::json!({
        "v": 1, "type": "remote_action_result",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "request_id": request_id,
        "session_id": session_ref,
        "operation": operation,
        "ok": ok,
        "body": body,
    });
    if let Some(error_code) = error_code {
        response["error_code"] = Value::String(error_code.to_owned());
    }
    response
}

fn public_remote_action_error(code: &str) -> &'static str {
    match code {
        "permission_expired" => "permission_expired",
        "permission_already_decided" => "permission_already_decided",
        "permission_not_found" => "permission_not_found",
        "task_session_not_found" => "task_session_not_found",
        "task_binding_stale" => "task_binding_stale",
        "transfer_unknown" => "transfer_unknown",
        "transfer_chunk_invalid" => "transfer_chunk_invalid",
        "transfer_chunk_out_of_order" => "transfer_chunk_out_of_order",
        "artifact_unavailable" => "artifact_unavailable",
        "result_unknown" => "result_unknown",
        _ => "remote_action_failed",
    }
}

fn capability_snapshot_error(lease: &LeaseContext, request_id: &str, session_id: &str) -> Value {
    let payload = json!({
        "v": 1,
        "type": "capability_snapshot_error",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "request_id": request_id,
        "session_id": session_id,
        "code": "capability_snapshot_unavailable",
    });
    payload
}

fn rewrite_session_id(mut payload: Value, session_ref: &str) -> Result<Value, String> {
    let object = payload
        .as_object_mut()
        .ok_or_else(|| "task_request_invalid".to_owned())?;
    object.insert(
        "session_id".to_owned(),
        Value::String(session_ref.to_owned()),
    );
    Ok(payload)
}

/// B → A 的 wire `journal_event`（E2E-05）。会话身份按第 4 节决策统一使用 A 的
/// 会话引用；事件本体 base64 编码，明文字段与本体保持一致以便 A 端做缺口检测。
///
/// 明文 `event_kind` 使用冻结的 `EVENT_KINDS` 闭集，而事件本体保留内部更细的
/// `JournalKind`（例如 `task_status`）。无法映射到冻结闭集的事件（附件、快照、
/// 非终态任务状态）不回传：A 端的任务状态由 `task_status` 帧负责，正文按
/// user_message / assistant_delta / reasoning / tool / permission / error /
/// terminal / artifact 回传。
pub(crate) fn build_journal_event_payload(
    lease: &LeaseContext,
    session_ref: &str,
    event: &super::journal::JournalEvent,
) -> Option<Value> {
    event.validate_exportable().ok()?;
    let wire_kind = event.wire_kind()?;
    // B owns the local session id while the wire contract owns A's opaque
    // session id. Envelope and authenticated body must use the same identity.
    let mut wire_event = event.clone();
    wire_event.session_id = session_ref.to_owned();
    let raw = serde_json::to_vec(&wire_event).ok()?;
    if raw.len() > super::journal::MAX_EVENT_PAYLOAD_BYTES {
        return None;
    }
    Some(json!({
        "v": 1,
        "type": "journal_event",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "session_id": session_ref,
        "journal_epoch": wire_event.epoch,
        "event_sequence": wire_event.sequence.to_string(),
        "event_kind": wire_kind,
        "event_b64": base64::engine::general_purpose::STANDARD.encode(&raw),
        "event_bytes": raw.len().to_string(),
    }))
}

/// B 端出站旁路：权威 journal 事件需要经同一代 endpoint lane 回传 A，而 lane 的
/// 接收端由 worker 独占，因此这里只保存可克隆发送句柄（断开/结束时清空）。
#[derive(Default)]
pub(crate) struct ControlledOutboundState {
    sender:
        parking_lot::Mutex<Option<crate::features::remote_control::manager::ControllerRelaySender>>,
}

impl ControlledOutboundState {
    pub(crate) fn clear_generation(&self, generation: &str) {
        let mut sender = self.sender.lock();
        if sender
            .as_ref()
            .is_some_and(|sender| sender.generation() == generation)
        {
            *sender = None;
        }
    }
    pub(crate) fn install(
        &self,
        sender: crate::features::remote_control::manager::ControllerRelaySender,
    ) {
        *self.sender.lock() = Some(sender);
    }

    pub(crate) fn clear(&self) {
        *self.sender.lock() = None;
    }

    pub(crate) fn sender(
        &self,
    ) -> Option<crate::features::remote_control::manager::ControllerRelaySender> {
        self.sender.lock().clone()
    }
}

/// 由 `controller_joined` 材料构造 B 端租约（纯函数）。传输指纹与 A 端使用
/// 同一份验签 transcript、按 B 角色域分离派生；租约在真正落地前不投影状态。
/// E2E-06：B 端恢复尝试状态（`challenge_b64` 非空即代表一次进行中的恢复）。
#[derive(Default)]
pub(crate) struct ControlledResumeAttempt {
    operation: Option<ControlledOperation>,
    // Five actual send points in the frozen resume exchange, never peer IDs.
    request_ids: Vec<String>,
    lease: Option<LeaseContext>,
    activation_id: Option<String>,
    challenge_b64: String,
    relay_deadline_identity_ms: u64,
    liveness_epoch_b: String,
    peer_confirmed: bool,
    confirmed: bool,
    ack_sent: bool,
    liveness_sent: bool,
    ready_sent: bool,
}

impl ControlledResumeAttempt {
    pub(crate) fn is_active(&self) -> bool {
        !self.challenge_b64.is_empty()
    }

    fn reset(&mut self) {
        *self = Self::default();
    }
}

/// E2E-06：B 端恢复帧处理结果。
#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum ControlledResumeOutcome {
    Ignored,
    Progressed,
    Completed,
    Failed,
}

/// E2E-06：`desktop socket` 掉线后启动 B 端恢复：投影 reconnecting、取恢复材料，
/// 并按冻结协议发出 `controller_resume_prepare`。任一步失败返回 false，调用方
/// 必须结束本代通道（此时管理器的恢复失败路径已经清理租约与本代凭证）。
pub(crate) fn begin_controlled_resume<P: ControlledRelayPort + ?Sized>(
    port: &P,
    manager: &ControlledDeviceManager,
    resume: &mut ControlledResumeAttempt,
) -> bool {
    let Some(operation) = manager.current_operation() else {
        return false;
    };
    let now_b = controlled_monotonic_millis();
    let _ = manager.mark_transport_disconnected(now_b);
    let Ok(material) = manager.begin_resume(operation.clone(), now_b) else {
        return false;
    };
    let Ok(challenge) = new_resume_challenge() else {
        return false;
    };
    let request_id = format!("request-resume-{}", uuid::Uuid::new_v4().simple());
    let Ok(frame) = build_controller_resume_prepare(
        &material.lease,
        super::relay_transport::ControllerRole::ControlledB,
        &request_id,
        &material.resume_token,
        &challenge,
        material.relay_deadline_identity_ms,
    ) else {
        return false;
    };
    if port.try_send(frame).is_err() {
        return false;
    }
    resume.operation = Some(operation);
    resume.request_ids = vec![request_id];
    resume.lease = Some(material.lease);
    resume.challenge_b64 = challenge;
    resume.relay_deadline_identity_ms = material.relay_deadline_identity_ms;
    // 激活后的 liveness epoch 必须与旧值不同（Relay 会据此拒绝复用）。
    resume.liveness_epoch_b = format!("live-b-{}", uuid::Uuid::new_v4().simple());
    resume.activation_id = None;
    resume.peer_confirmed = false;
    resume.confirmed = false;
    resume.ack_sent = false;
    resume.liveness_sent = false;
    resume.ready_sent = false;
    true
}

/// E2E-06：按 Relay/A 的恢复帧推进 B 端相位。任何相位不符、对端确认验签失败或
/// `controller_resumed` 材料不完整都失败关闭（管理器侧已清理本代）。
pub(crate) fn handle_controlled_resume_frame<P: ControlledRelayPort + ?Sized>(
    port: &P,
    manager: &ControlledDeviceManager,
    lane: &ControlledLaneState,
    resume: &mut ControlledResumeAttempt,
    message: &Value,
) -> ControlledResumeOutcome {
    let Some(kind) = message.get("type").and_then(Value::as_str) else {
        return ControlledResumeOutcome::Ignored;
    };
    let Some(operation) = manager.current_operation() else {
        return ControlledResumeOutcome::Failed;
    };
    let Some(lease) = resume.lease.clone() else {
        return ControlledResumeOutcome::Failed;
    };
    if resume.operation.as_ref() != Some(&operation) {
        return ControlledResumeOutcome::Ignored;
    }
    // Relay errors deliberately have no lease tuple. Authority comes from
    // strict protocol validation and this operation's locally sent request.
    if kind == "error" {
        if validate_controller_message_json(&message.to_string(), SenderRole::Relay).is_err()
            || !message
                .get("request_id")
                .and_then(Value::as_str)
                .is_some_and(|id| resume.request_ids.iter().any(|sent| sent == id))
        {
            return ControlledResumeOutcome::Ignored;
        }
        return ControlledResumeOutcome::Failed;
    }
    // 审核修复：Relay/A 的恢复帧必须绑定当前租约；旧代际或旧租约的残留帧一律
    // 丢弃，既不推进相位也不终止候选。
    if !resume_frame_matches_lease(&lease, message) {
        return ControlledResumeOutcome::Ignored;
    }
    let role = super::relay_transport::ControllerRole::ControlledB;
    let now_b = controlled_monotonic_millis();
    let request_id = || format!("request-resume-{}", uuid::Uuid::new_v4().simple());
    match kind {
        "controller_resume_provisional" => {
            let Some(activation_id) = message.get("activation_id").and_then(Value::as_str) else {
                return ControlledResumeOutcome::Ignored;
            };
            if resume.activation_id.is_some() {
                return ControlledResumeOutcome::Ignored;
            }
            if manager
                .advance_resume(
                    operation.clone(),
                    ControlledResumeStep::Provisional { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            let sent_id = request_id();
            let Ok(confirm) = lane.seal_resume_confirm(&sent_id, activation_id) else {
                return ControlledResumeOutcome::Failed;
            };
            if port.try_send(confirm).is_err() {
                return ControlledResumeOutcome::Failed;
            }
            resume.request_ids.push(sent_id);
            resume.activation_id = Some(activation_id.to_owned());
            ControlledResumeOutcome::Progressed
        }
        "encrypted_resume_confirm" => {
            if message.get("activation_id").and_then(Value::as_str)
                != resume.activation_id.as_deref()
                || lane.open_resume_confirm(message).is_err()
                || resume.ack_sent
            {
                return ControlledResumeOutcome::Ignored;
            }
            let Some(activation_id) = resume.activation_id.as_deref() else {
                return ControlledResumeOutcome::Ignored;
            };
            // Relay cannot emit pending until B acknowledges activation. B's
            // own confirm was queued first on this same FIFO socket; now the
            // verified A confirm authorizes B's local acceptance and ACK.
            resume.peer_confirmed = true;
            if !resume.confirmed {
                if manager
                    .advance_resume(
                        operation.clone(),
                        ControlledResumeStep::Confirmed { activation_id },
                        now_b,
                    )
                    .is_err()
                {
                    return ControlledResumeOutcome::Failed;
                }
                resume.confirmed = true;
            }
            let sent_id = request_id();
            let Ok(ack) = build_controller_resume_activation_ack(
                &lease,
                &sent_id,
                activation_id,
                resume.relay_deadline_identity_ms,
            ) else {
                return ControlledResumeOutcome::Failed;
            };
            if manager
                .advance_resume(
                    operation.clone(),
                    ControlledResumeStep::ActivationAcknowledged { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            if port.try_send(ack).is_err() {
                return ControlledResumeOutcome::Failed;
            }
            resume.request_ids.push(sent_id);
            resume.ack_sent = true;
            ControlledResumeOutcome::Progressed
        }
        "controller_resume_activation_pending" => {
            let Some(activation_id) = message.get("activation_id").and_then(Value::as_str) else {
                return ControlledResumeOutcome::Ignored;
            };
            if !resume.ack_sent || !resume.peer_confirmed || resume.liveness_sent {
                return ControlledResumeOutcome::Ignored;
            }
            if manager
                .advance_resume(
                    operation,
                    ControlledResumeStep::PostActivationLiveness {
                        activation_id,
                        liveness_epoch_b: &resume.liveness_epoch_b,
                    },
                    now_b,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            let sent_id = request_id();
            let Ok(liveness) = build_controller_post_activation_liveness(
                &lease,
                &sent_id,
                activation_id,
                &resume.liveness_epoch_b,
            ) else {
                return ControlledResumeOutcome::Failed;
            };
            if port.try_send(liveness).is_err() {
                return ControlledResumeOutcome::Failed;
            }
            resume.request_ids.push(sent_id);
            resume.liveness_sent = true;
            ControlledResumeOutcome::Progressed
        }
        "controller_post_activation_liveness_confirmed" => {
            let Some(activation_id) = message.get("activation_id").and_then(Value::as_str) else {
                return ControlledResumeOutcome::Ignored;
            };
            let Some(liveness_epoch_b) = message.get("liveness_epoch_b").and_then(Value::as_str)
            else {
                return ControlledResumeOutcome::Ignored;
            };
            let Some(heartbeat_sequence_b) =
                message.get("heartbeat_sequence_b").and_then(Value::as_str)
            else {
                return ControlledResumeOutcome::Ignored;
            };
            if !resume.liveness_sent || resume.ready_sent {
                return ControlledResumeOutcome::Ignored;
            }
            let sent_id = request_id();
            let Ok(ready) = build_controller_resume_ready_ack(
                &lease,
                role,
                &sent_id,
                activation_id,
                liveness_epoch_b,
                heartbeat_sequence_b,
            ) else {
                return ControlledResumeOutcome::Failed;
            };
            if manager
                .advance_resume(
                    operation,
                    ControlledResumeStep::ReadyAcknowledged { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            if port.try_send(ready).is_err() {
                return ControlledResumeOutcome::Failed;
            }
            resume.request_ids.push(sent_id);
            resume.ready_sent = true;
            ControlledResumeOutcome::Progressed
        }
        "controller_resumed" => {
            if !resume.ready_sent {
                return ControlledResumeOutcome::Ignored;
            }
            if message.get("liveness_epoch_b").and_then(Value::as_str)
                != Some(resume.liveness_epoch_b.as_str())
            {
                return ControlledResumeOutcome::Ignored;
            }
            let Some(activation_id) = message.get("activation_id").and_then(Value::as_str) else {
                return ControlledResumeOutcome::Ignored;
            };
            let Some(liveness_epoch_a) = message.get("liveness_epoch_a").and_then(Value::as_str)
            else {
                return ControlledResumeOutcome::Ignored;
            };
            let Some(new_token) = message.get("new_resume_token_b64").and_then(Value::as_str)
            else {
                return ControlledResumeOutcome::Ignored;
            };
            let Some(relay_deadline) = super::resume::relay_resume_deadline_ms(message) else {
                return ControlledResumeOutcome::Failed;
            };
            let Ok(decoded) = base64::engine::general_purpose::STANDARD.decode(new_token) else {
                return ControlledResumeOutcome::Ignored;
            };
            let Ok(token) = <[u8; 32]>::try_from(decoded.as_slice()) else {
                return ControlledResumeOutcome::Ignored;
            };
            let Ok(fingerprint) = lane.resume_fingerprint(activation_id) else {
                return ControlledResumeOutcome::Failed;
            };
            let Some(resume_request_id) = message.get("request_id").and_then(Value::as_str) else {
                return ControlledResumeOutcome::Ignored;
            };
            if manager
                .complete_resumed(
                    operation,
                    activation_id,
                    resume_request_id,
                    now_b,
                    liveness_epoch_a,
                    token,
                    fingerprint,
                    relay_deadline,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            lane.activate();
            resume.reset();
            ControlledResumeOutcome::Completed
        }
        _ => ControlledResumeOutcome::Ignored,
    }
}

fn controlled_lease_from_outcome(
    outcome: ControlledHandshakeOutcome,
    now_b: u64,
) -> Result<ControlledLease, String> {
    let context = LeaseContext {
        connection_id: outcome.lease.connection_id.clone(),
        controller_lease_id: outcome.lease.controller_lease_id.clone(),
        lease_epoch: outcome.lease.lease_epoch.to_string(),
        relay_epoch: outcome.lease.relay_epoch.clone(),
        remote_control_generation: outcome.lease.remote_control_generation.clone(),
    };
    let fingerprint = transport_key_fingerprint(outcome.transcript_hash, HandshakeRole::B)
        .map_err(|_| HANDSHAKE_FAILED.to_owned())?;
    Ok(ControlledLease {
        context,
        liveness_epoch_a: outcome.lease.liveness_epoch_a.clone(),
        resume_deadline_b: outcome.lease.resume_deadline_ms,
        resume_token: outcome.lease.resume_token,
        transport_key_fingerprint: fingerprint,
        negotiated_capabilities: outcome.negotiated_capabilities.clone(),
        // 冻结协议 v1 不携带主控设备显示名（controller_joined 无该字段）：首版
        // 留空，由 UI 的 i18n 兜底文案显示未知设备，真实来源待协议扩展。
        controller_device_name: String::new(),
        connected_at_b: now_b,
    })
}

fn registration_request(
    registration_request_id: &str,
    endpoint_id: &str,
    generation: &str,
    code_wrap: &CodeWrapKeyPair,
    handshake: &HandshakeKeyPair,
) -> Result<Value, String> {
    let value = json!({
        "v": 1,
        "type": "desktop_pairing_register",
        "request_id": format!("request-{registration_request_id}"),
        "registration_request_id": registration_request_id,
        "endpoint_id": endpoint_id,
        "remote_control_generation": generation,
        "code_wrap_public_key_b64": base64::engine::general_purpose::STANDARD
            .encode(code_wrap.public_key().to_bytes()),
        "handshake_public_key_b64": base64::engine::general_purpose::STANDARD
            .encode(handshake.public_key().to_bytes()),
        "key_algorithm": "x25519",
        "capabilities": REGISTRATION_CAPABILITIES,
    });
    validate_controller_message_json(&value.to_string(), SenderRole::ControlledB)
        .map_err(|_| "pairing_registration_invalid".to_owned())
}

pub(crate) fn registration_from_response(
    code_wrap: &CodeWrapKeyPair,
    request: &Value,
    response: &Value,
) -> Result<ControlledRegistration, String> {
    let context = RegistrationValidationContext::new(request.clone(), None)
        .map_err(|_| "pairing_registration_invalid".to_owned())?;
    let validated = validate_controller_registration_message_json(
        &response.to_string(),
        SenderRole::Relay,
        &context,
    )
    .map_err(|_| "pairing_registration_rejected".to_owned())?;
    if validated.get("type").and_then(Value::as_str) == Some("error") {
        return Err(validated
            .get("code")
            .and_then(Value::as_str)
            .unwrap_or("pairing_registration_rejected")
            .to_owned());
    }

    let string = |field: &str| -> Result<String, String> {
        validated
            .get(field)
            .and_then(Value::as_str)
            .map(str::to_owned)
            .ok_or_else(|| "pairing_registration_invalid".to_owned())
    };
    let expires_at_ms = string("expires_at_ms")?
        .parse::<u64>()
        .map_err(|_| "pairing_registration_invalid".to_owned())?;
    let binding = CodeWrapBinding::new(
        string("registration_request_id")?,
        string("pairing_id")?,
        string("remote_control_generation")?,
        string("relay_epoch")?,
        expires_at_ms,
    )
    .map_err(|_| "pairing_registration_invalid".to_owned())?;
    let sealed = base64::engine::general_purpose::STANDARD
        .decode(string("sealed_code_b64")?)
        .map_err(|_| "pairing_registration_invalid".to_owned())?;
    let pairing_code = code_wrap
        .open_sealed_code(&binding, &sealed)
        .map_err(|_| "pairing_code_invalid".to_owned())?
        .expose()
        .to_owned();
    Ok(ControlledRegistration {
        pairing_code,
        sealed_code_b64: string("sealed_code_b64")?,
        expires_at_ms,
    })
}

fn sanitize_error(value: &str) -> String {
    let bounded = value
        .chars()
        .filter(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-'))
        .take(64)
        .collect::<String>();
    if bounded.is_empty() {
        "unspecified".to_owned()
    } else {
        bounded
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compact_value_fingerprint_preserves_the_original_serialized_bytes() {
        let lease = LeaseContext {
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "epoch-1".into(),
            relay_epoch: "relay-1".into(),
            remote_control_generation: "generation-1".into(),
        };
        for body in [
            json!({}),
            json!({"text":"中文\nquoted\""}),
            json!({"n":u64::MAX}),
            json!({"a":[null,true,3.25],"z":{"state":"completed"}}),
        ] {
            let bytes = serde_json::to_vec(&body).unwrap();
            assert_eq!(body.to_string().as_bytes(), bytes);
            let mut previous = Sha256::new();
            for field in [
                lease.connection_id.as_bytes(),
                lease.controller_lease_id.as_bytes(),
                lease.lease_epoch.as_bytes(),
                lease.relay_epoch.as_bytes(),
                lease.remote_control_generation.as_bytes(),
                b"session-1",
                b"request-1",
                b"artifact_index",
                bytes.as_slice(),
            ] {
                previous.update((field.len() as u64).to_be_bytes());
                previous.update(field);
            }
            let expected: [u8; 32] = previous.finalize().into();
            assert_eq!(
                remote_action_fingerprint(
                    &lease,
                    "session-1",
                    "request-1",
                    "artifact_index",
                    &body
                ),
                expected
            );
        }
    }

    #[test]
    fn relay_wall_expiry_becomes_a_bounded_local_monotonic_deadline() {
        assert_eq!(
            pairing_deadline_b(1_030_000, 1_000_000, 70).unwrap(),
            30_070
        );
        assert_eq!(
            pairing_deadline_b(2_000_000, 1_000_000, 70).unwrap(),
            70 + MAX_PAIRING_CODE_TTL_MS
        );
        assert_eq!(
            pairing_deadline_b(1_000_000, 1_000_000, 70).unwrap_err(),
            "pairing_code_expired"
        );
        assert_eq!(
            pairing_deadline_b(999_999, 1_000_000, 70).unwrap_err(),
            "pairing_code_expired"
        );
    }
    use crate::features::remote_control::controller::controlling_handshake::ControllingHandshake;
    use crate::features::remote_control::controller::crypto::{CodeWrapKeyPair, HandshakeKeyPair};
    use crate::features::remote_control::controller::test_support::PairingFixture;
    use std::collections::VecDeque;
    use std::time::Duration;

    #[test]
    fn relay_log_lines_are_scoped_and_keep_the_legacy_controlled_prefix() {
        assert_eq!(
            controlled_log_line("registered", "waiting_for_pairing"),
            "[controlled] stage=registered detail=waiting_for_pairing"
        );
        assert_eq!(
            controller_log_line("connect_failed", "pairing_code_invalid"),
            "[controller] stage=connect_failed detail=pairing_code_invalid"
        );
        assert_eq!(
            remote_action_log_line(
                "controller",
                "remote_action_validation_failed",
                "artifact_index",
                "unsupported_capability"
            ),
            "[controller] stage=remote_action_validation_failed operation=artifact_index detail=unsupported_capability"
        );
    }

    #[test]
    fn log_allowlist_rejects_identifier_shaped_values_and_secrets() {
        for sentinel in [
            "AB12CD3",
            "generation123",
            "deviceAlice",
            "sklive12345678",
            "C:/Users/Alice",
        ] {
            assert_eq!(log_scope(sentinel), "unknown");
            assert_eq!(log_stage(sentinel), "other");
            assert_eq!(log_detail(sentinel), "other");
            assert_eq!(log_remote_action_operation(sentinel), "other");
        }
        assert_eq!(log_stage("controlled_status"), "controlled_status");
        assert_eq!(log_detail("pairing_code_invalid"), "pairing_code_invalid");
        for stage in [
            "task_submit_endpoint_received",
            "task_submit_socket_written",
            "task_submit_business_entered",
            "task_submit_business_rejected",
            "task_submit_context_validated",
            "task_submit_payload_validated",
            "task_submit_identity_validated",
            "task_submit_capability_validated",
            "task_submit_mapping_ready",
            "task_submit_mapping_projected",
            "task_submit_prepared",
            "controlled_submission_admission_entered",
            "task_model_source_received",
        ] {
            assert_eq!(log_stage(stage), stage);
        }
        for detail in ["forwarding_to_worker", "validating", "admitted"] {
            assert_eq!(log_detail(detail), detail);
        }
        assert_eq!(
            log_port_error_detail(RemotePortError::new("task_workspace_required", false)),
            "task_workspace_required"
        );
        for code in CONTROLLED_PORT_LOG_ERROR_CODES {
            assert_eq!(
                log_port_error_detail(RemotePortError::new(code, false)),
                *code
            );
        }
        assert_eq!(
            log_port_error_detail(RemotePortError::new("internal_token_like_literal", false)),
            "other"
        );
        assert_eq!(
            log_remote_action_operation("artifact_index"),
            "artifact_index"
        );
        assert_eq!(log_stage("lifecycle_checkpoint"), "lifecycle_checkpoint");
        for detail in [
            "runtime_started",
            "refresh_requested",
            "refresh_state_rotation_begin",
            "refresh_registration_wait_done",
            "refresh_failed",
        ] {
            assert_eq!(log_detail(detail), detail);
        }
    }

    #[test]
    fn real_remote_failure_projections_do_not_persist_any_sensitive_category() {
        struct RestoreHome(Option<std::ffi::OsString>);
        impl Drop for RestoreHome {
            fn drop(&mut self) {
                match self.0.take() {
                    Some(value) => unsafe { std::env::set_var("PINVOU3_HOME", value) },
                    None => unsafe { std::env::remove_var("PINVOU3_HOME") },
                }
            }
        }
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let directory = tempfile::tempdir().unwrap();
        let _restore = RestoreHome(std::env::var_os("PINVOU3_HOME"));
        unsafe { std::env::set_var("PINVOU3_HOME", directory.path()) };
        let sentinels = [
            "2222222",
            "endpointSentinelA1",
            "leaseSentinelB2",
            "tokenSentinelC3-credential-000001",
            "keySentinelD4-credential-00000001",
            "198.51.100.77",
            "deviceSentinelE5",
            "promptSentinelF6",
            "replySentinelG7",
            "toolArgsSentinelH8",
            "fileSentinelI9.txt",
            "C:/private/sentinel/J0",
            "contentSentinelK1",
        ];
        let raw = sentinels.join("|");
        log_attachment_digest_omitted(&raw);
        log_task_persist_failure(false, &raw);
        log_task_persist_failure(true, &raw);
        log_artifact_capture_failure(&raw);
        let raw_path = std::path::Path::new(&raw);
        let raw_error = std::io::Error::other(raw.clone());
        log_attachment_cleanup_failure(raw_path, &raw_error);
        log_remote_action_event("controller", &raw, &raw, &raw);
        let written =
            std::fs::read_to_string(directory.path().join("logs/remote-control.log")).unwrap();
        for sentinel in sentinels {
            assert!(!written.contains(sentinel));
        }
        for stage in [
            "attachment_digest_missing",
            "task_terminal_persist_failed",
            "task_rejected_persist_failed",
            "artifact_capture_failed",
            "attachment_cleanup_failed",
        ] {
            assert!(written.contains(&format!("stage={stage}")));
        }
    }

    /// Relay 会把发送方的 commit 原样回投（router 契约）：B 只吃 `sender_role == A`
    /// 的对端帧，自己的回投必须被跳过，否则真机上会在拿到预留的下一帧直接
    /// `controller_handshake_rejected`（2026-09-20 实测）。
    /// Relay 的收据校验契约（`controller-lease.js` 的 `acceptReceipt`）：
    /// `finished_receipt_ack` 里带的必须是 `sender_role` 那一方（对端）的 receipt
    /// secret，且能重算出对端先前的 commitment。这里在客户端侧做同构校验——真机
    /// 首次暴露的 `finished_receipt_invalid`（端上兜底成 `invalid_message`）就是
    /// 两端各自误传了自己的 receipt。
    #[test]
    fn receipt_ack_carries_the_peer_receipt_matching_its_commitment() {
        use crate::features::remote_control::controller::crypto::{
            ControllerRole as HandshakeRole, ReceiptSecret,
        };
        use crate::features::remote_control::controller::test_support::{
            handshake_binding, hex_lower, pairing_fixture,
        };

        let PairingFixture {
            relay,
            reservation,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture("relay-key-runtime");
        let binding = handshake_binding(&reservation);
        let mut driver = ControlledHandshakeDriver::new(b_keys);
        let mut a = ControllingHandshake::new(
            reservation["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            relay.trust.clone(),
        );
        let b_commit = driver.accept_reserved(&reservation, &relay.trust).unwrap();
        let a_commit = a.accept_reserved(&reservation).unwrap();
        let b_finished = driver.accept_peer_commitment(&a_commit).unwrap();
        let a_finished = a.accept_peer_commitment(&b_commit).unwrap();
        let b_ack = driver.accept_peer_finished(&a_finished).unwrap();
        let a_ack = a.accept_peer_finished(&b_finished).unwrap();

        // A 提交的 ack 声明 sender_role=B：必须携带 B 的 receipt（对得上 B 的 commitment）。
        let from_a_ack =
            ReceiptSecret::from_base64_for_test(a_ack["receipt_secret_b64"].as_str().unwrap())
                .unwrap();
        assert_eq!(
            hex_lower(&from_a_ack.commitment(&binding, HandshakeRole::B).unwrap()),
            b_commit["commitment"].as_str().unwrap()
        );
        // B 提交的 ack 声明 sender_role=A：必须携带 A 的 receipt。
        let from_b_ack =
            ReceiptSecret::from_base64_for_test(b_ack["receipt_secret_b64"].as_str().unwrap())
                .unwrap();
        assert_eq!(
            hex_lower(&from_b_ack.commitment(&binding, HandshakeRole::A).unwrap()),
            a_commit["commitment"].as_str().unwrap()
        );
        // 反向核对：把 A 的 receipt 当成 B 的 receipt 去算，必然对不上 B 的 commitment
        //（这正是修复前两端实际发出的内容）。
        assert_ne!(
            hex_lower(&from_b_ack.commitment(&binding, HandshakeRole::B).unwrap()),
            b_commit["commitment"].as_str().unwrap()
        );
    }

    #[test]
    fn only_peer_handshake_frames_are_dispatched_on_the_controlled_lane() {
        let frame = |kind: &str, role: &str| {
            json!({
                "v": 1,
                "type": kind,
                "request_id": "request-0001",
                "pairing_id": "pairing-0001",
                "reservation_id": "reservation-0001",
                "remote_control_generation": "generation-0001",
                "relay_epoch": "relay-epoch-0001",
                "transcript_hash": "00".repeat(32),
                "sender_role": role,
                "commitment": "11".repeat(32),
                "ciphertext_b64": "AQID",
                "ciphertext_bytes": "3",
            })
        };
        assert!(skip_self_echoed_frame(&frame(
            "finished_receipt_commit",
            "B"
        )));
        assert!(!skip_self_echoed_frame(&frame(
            "finished_receipt_commit",
            "A"
        )));
        assert!(skip_self_echoed_frame(&frame(
            "encrypted_handshake_finished",
            "B"
        )));
        assert!(!skip_self_echoed_frame(&frame(
            "encrypted_handshake_finished",
            "A"
        )));
        // 其它帧（预留、joined、加密业务帧）绝不按自帧吞掉。
        assert!(!skip_self_echoed_frame(&frame(
            "controller_pairing_reserved",
            "B"
        )));
        assert!(!skip_self_echoed_frame(&frame("controller_joined", "B")));
        assert!(!skip_self_echoed_frame(&frame("encrypted_frame", "B")));
    }

    /// Deterministic fake lane: queued messages, then either hang or close.
    struct FakePort {
        queued: VecDeque<Value>,
        sent: std::sync::Mutex<Vec<Value>>,
        hang: bool,
        recv_delay: Duration,
    }

    impl FakePort {
        fn new(messages: Vec<Value>, hang: bool) -> Self {
            Self {
                queued: messages.into(),
                sent: std::sync::Mutex::new(Vec::new()),
                hang,
                recv_delay: Duration::ZERO,
            }
        }

        fn delayed(messages: Vec<Value>, hang: bool, recv_delay: Duration) -> Self {
            Self {
                queued: messages.into(),
                sent: std::sync::Mutex::new(Vec::new()),
                hang,
                recv_delay,
            }
        }
    }

    #[async_trait::async_trait]
    impl ControlledRelayPort for FakePort {
        fn try_send(&self, message: Value) -> Result<(), String> {
            self.sent.lock().unwrap().push(message);
            Ok(())
        }

        async fn recv(&mut self) -> Option<Value> {
            if !self.recv_delay.is_zero() {
                tokio::time::sleep(self.recv_delay).await;
            }
            if let Some(message) = self.queued.pop_front() {
                return Some(message);
            }
            if self.hang {
                std::future::pending::<()>().await;
            }
            None
        }
    }

    fn registration_request_fixture() -> (Value, CodeWrapKeyPair) {
        let receiver = CodeWrapKeyPair::generate().unwrap();
        let handshake = HandshakeKeyPair::generate().unwrap();
        let request = json!({
            "v": 1,
            "type": "desktop_pairing_register",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "endpoint_id": "endpoint-0001",
            "remote_control_generation": "generation-0001",
            "code_wrap_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(receiver.public_key().to_bytes()),
            "handshake_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(handshake.public_key().to_bytes()),
            "key_algorithm": "x25519",
            "capabilities": ["pairing_v1"],
        });
        (request, receiver)
    }

    fn registered_response(receiver: &CodeWrapKeyPair) -> Value {
        let code_wrap = CodeWrapKeyPair::generate().unwrap();
        let binding = CodeWrapBinding::new(
            "registration-0001",
            "pairing-0001",
            "generation-0001",
            "relay-epoch-0001",
            1_800_000_000_000,
        )
        .unwrap();
        let sealed = code_wrap
            .seal_pairing_code(receiver.public_key(), &binding, "2345678")
            .unwrap();
        json!({
            "v": 1,
            "type": "desktop_pairing_registered",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "pairing_id": "pairing-0001",
            "remote_control_generation": "generation-0001",
            "sealed_code_b64": base64::engine::general_purpose::STANDARD.encode(sealed),
            "expires_at_ms": "1800000000000",
            "relay_epoch": "relay-epoch-0001",
        })
    }

    fn block_on<F: std::future::Future>(future: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
            .block_on(future)
    }

    /// E2E-06：双方声明的能力族必须一致——Relay 的协商是「A claim ∩ B 注册」，
    /// 任一侧漏声明都会让整族能力静默失效（resume_v1 是本轮新增的族）。
    #[test]
    fn both_roles_declare_the_same_frozen_capability_families() {
        let mut claim = super::super::controlling_handshake::claim_capabilities().to_vec();
        let mut registration = super::registration_capabilities().to_vec();
        assert_eq!(
            claim.len(),
            registration.len(),
            "A claim 与 B 注册声明的能力族数量必须一致"
        );
        claim.sort_unstable();
        registration.sort_unstable();
        assert_eq!(
            claim, registration,
            "A claim 与 B 注册声明的能力集合必须一致"
        );
        for family in [
            "pairing_v1",
            "encrypted_frames_v1",
            "finished_receipt_v1",
            "role_liveness_v1",
            "resume_v1",
            "task_v1",
            "temporary_model_package_v1",
            "permission_v1",
            "attachment_v1",
            "artifact_v1",
            "snapshot_journal_v1",
        ] {
            assert!(claim.contains(&family), "A 端未声明 {family}");
        }
    }

    #[test]
    fn registration_ignores_the_transport_ack_then_accepts_the_sealed_result() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![
                json!({ "v": 2, "type": "desktop_endpoint_registered", "endpoint_id": "endpoint-0001" }),
                registered_response(&receiver),
            ],
            false,
        );
        let registration = block_on(wait_for_registration(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap();
        assert_eq!(registration.pairing_code, "2345678");
        assert!(!format!("{registration:?}").contains("2345678"));
    }

    #[test]
    fn registration_ignores_relay_connection_frame_before_the_sealed_result() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![
                json!({
                    "type": "desktop_relay_connection",
                    "endpoint_id": "endpoint-0001",
                    "connected": true,
                    "error": null,
                }),
                registered_response(&receiver),
            ],
            false,
        );
        block_on(wait_for_registration(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap();
    }

    #[test]
    fn registration_treats_relay_disconnect_frame_as_retryable_disconnect() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![json!({
                "type": "desktop_relay_connection",
                "endpoint_id": "endpoint-0001",
                "connected": false,
                "error": "transport_closed",
            })],
            false,
        );
        assert_eq!(
            block_on(wait_for_registration(
                &mut port,
                &request,
                &receiver,
                Duration::from_millis(500),
            ))
            .unwrap_err(),
            "desktop_relay_disconnected"
        );
    }

    #[test]
    fn registration_wait_times_out_with_a_bounded_code() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(Vec::new(), true);
        assert_eq!(
            block_on(wait_for_registration(
                &mut port,
                &request,
                &receiver,
                Duration::from_millis(20),
            ))
            .unwrap_err(),
            "pairing_registration_timeout"
        );
    }

    #[test]
    fn ignored_frames_do_not_extend_the_registration_deadline() {
        let (request, receiver) = registration_request_fixture();
        let ignored = vec![
            json!({ "v": 2, "type": "desktop_endpoint_registered", "endpoint_id": "endpoint-0001" }),
            json!({
                "type": "desktop_relay_connection",
                "endpoint_id": "endpoint-0001",
                "connected": true,
                "error": null,
            }),
            json!({
                "v": 1,
                "type": "error",
                "request_id": "request-from-another-phase",
                "code": "enrollment_required",
                "message": "not this registration",
            }),
        ];
        let mut port = FakePort::delayed(ignored, true, Duration::from_millis(50));
        let started = std::time::Instant::now();
        assert_eq!(
            block_on(wait_for_registration(
                &mut port,
                &request,
                &receiver,
                Duration::from_millis(130),
            ))
            .unwrap_err(),
            "pairing_registration_timeout"
        );
        assert!(
            started.elapsed() < Duration::from_millis(220),
            "ignored frames must not restart the attempt timeout"
        );
    }

    #[test]
    fn closed_relay_lane_fails_closed() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(Vec::new(), false);
        assert_eq!(
            block_on(wait_for_registration(
                &mut port,
                &request,
                &receiver,
                Duration::from_millis(500),
            ))
            .unwrap_err(),
            "desktop_relay_disconnected"
        );
    }

    #[test]
    fn relay_rejection_is_surfaced_as_a_bounded_code() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![json!({
                "v": 1,
                "type": "error",
                "request_id": "request-registration-0001",
                "code": "enrollment_required",
                "message": "pairing registration rejected",
            })],
            false,
        );
        let error = block_on(wait_for_registration(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap_err();
        assert_eq!(error, "enrollment_required");
        assert!(
            error
                .chars()
                .all(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-')),
            "relay failures must stay bounded: {error}"
        );
        assert!(!error.contains(' '));
    }

    #[test]
    fn registration_ignores_an_error_for_another_request() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![
                json!({
                    "v": 1,
                    "type": "error",
                    "request_id": "request-from-another-phase",
                    "code": "enrollment_required",
                    "message": "pairing registration rejected",
                }),
                registered_response(&receiver),
            ],
            false,
        );
        block_on(wait_for_registration(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap();
    }

    #[test]
    fn disabled_controller_lane_has_a_specific_bounded_error() {
        let (request, receiver) = registration_request_fixture();
        for relay_code in [
            "unsupported_protocol_version",
            "desktop_controller_unavailable",
        ] {
            let mut port = FakePort::new(
                vec![json!({
                    "v": 2,
                    "type": "error",
                    "code": relay_code,
                    "message": "desktop controller is unavailable",
                })],
                false,
            );
            assert_eq!(
                block_on(wait_for_registration(
                    &mut port,
                    &request,
                    &receiver,
                    Duration::from_millis(500),
                ))
                .unwrap_err(),
                "desktop_controller_relay_unavailable"
            );
        }
    }

    fn keys() -> (CodeWrapKeyPair, CodeWrapKeyPair) {
        (
            CodeWrapKeyPair::generate().unwrap(),
            CodeWrapKeyPair::generate().unwrap(),
        )
    }

    #[test]
    fn relay_result_is_bound_and_opened_without_exposing_the_code() {
        let (relay, receiver) = keys();
        let handshake = HandshakeKeyPair::generate().unwrap();
        let request = json!({
            "v": 1,
            "type": "desktop_pairing_register",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "endpoint_id": "endpoint-0001",
            "remote_control_generation": "generation-0001",
            "code_wrap_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(receiver.public_key().to_bytes()),
            "handshake_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(handshake.public_key().to_bytes()),
            "key_algorithm": "x25519",
            "capabilities": ["pairing_v1"],
        });
        let binding = CodeWrapBinding::new(
            "registration-0001",
            "pairing-0001",
            "generation-0001",
            "relay-epoch-0001",
            1_800_000_000_000,
        )
        .unwrap();
        let sealed = relay
            .seal_pairing_code(receiver.public_key(), &binding, "2345678")
            .unwrap();
        let response = json!({
            "v": 1,
            "type": "desktop_pairing_registered",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "pairing_id": "pairing-0001",
            "remote_control_generation": "generation-0001",
            "sealed_code_b64": base64::engine::general_purpose::STANDARD.encode(sealed),
            "expires_at_ms": "1800000000000",
            "relay_epoch": "relay-epoch-0001",
        });

        let registration = registration_from_response(&receiver, &request, &response).unwrap();
        assert_eq!(registration.pairing_code, "2345678");
        let debug = format!("{registration:?}");
        assert!(!debug.contains("2345678"));
    }

    #[test]
    fn cross_generation_relay_result_fails_closed() {
        let (relay, receiver) = keys();
        let handshake = HandshakeKeyPair::generate().unwrap();
        let request = json!({
            "v": 1,
            "type": "desktop_pairing_register",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "endpoint_id": "endpoint-0001",
            "remote_control_generation": "generation-0001",
            "code_wrap_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(receiver.public_key().to_bytes()),
            "handshake_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(handshake.public_key().to_bytes()),
            "key_algorithm": "x25519",
            "capabilities": ["pairing_v1"],
        });
        let binding = CodeWrapBinding::new(
            "registration-0001",
            "pairing-0001",
            "generation-0001",
            "relay-epoch-0001",
            1_800_000_000_000,
        )
        .unwrap();
        let sealed = relay
            .seal_pairing_code(receiver.public_key(), &binding, "2345678")
            .unwrap();
        let response = json!({
            "v": 1,
            "type": "desktop_pairing_registered",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "pairing_id": "pairing-0001",
            "remote_control_generation": "generation-other",
            "sealed_code_b64": base64::engine::general_purpose::STANDARD.encode(sealed),
            "expires_at_ms": "1800000000000",
            "relay_epoch": "relay-epoch-0001",
        });
        assert_eq!(
            registration_from_response(&receiver, &request, &response).unwrap_err(),
            "pairing_registration_rejected"
        );
    }

    /// 用真实 A 端握手跑完双收据交换，返回已推进到 Acked 的 driver 与 A 端状态。
    fn drive_through_receipts(
        reservation: &Value,
        b_keys: HandshakeKeyPair,
        a_keys: HandshakeKeyPair,
        trust: &AuthenticatedRelayTrustConfig,
    ) -> (ControlledHandshakeDriver, ControllingHandshake) {
        let mut driver = ControlledHandshakeDriver::new(b_keys);
        let b_commit = driver.accept_reserved(reservation, trust).unwrap();
        let mut a = ControllingHandshake::new(
            reservation["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            trust.clone(),
        );
        let a_commit = a.accept_reserved(reservation).unwrap();
        let b_finished = driver.accept_peer_commitment(&a_commit).unwrap();
        let a_finished = a.accept_peer_commitment(&b_commit).unwrap();
        driver.accept_peer_finished(&a_finished).unwrap();
        a.accept_peer_finished(&b_finished).unwrap();
        (driver, a)
    }

    /// E2E-02 第 3 步：B 端 driver 与真实 A 端握手完成双收据交换，并且只有在
    /// 收到 `controller_joined` 后才产出与 A 端同一份 transcript 绑定的材料。
    #[test]
    fn driver_completes_both_receipts_and_yields_the_joined_lease() {
        let PairingFixture {
            relay,
            reservation,
            joined,
            a_keys,
            b_keys,
            resume_token,
        } = crate::features::remote_control::controller::test_support::pairing_fixture(
            "relay-key-runtime",
        );
        let mut driver = ControlledHandshakeDriver::new(b_keys);
        let mut a = ControllingHandshake::new(
            reservation["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            relay.trust.clone(),
        );

        // 未接管前的顺序违规必须失败关闭，且不产生任何材料。
        assert_eq!(
            driver.accept_peer_finished(&reservation).unwrap_err(),
            "controller_handshake_stage_invalid"
        );
        assert_eq!(
            driver.accept_joined(&joined).unwrap_err(),
            "controller_handshake_stage_invalid"
        );
        assert!(!driver.is_active());

        let b_commit = driver.accept_reserved(&reservation, &relay.trust).unwrap();
        assert!(driver.is_active());
        assert_eq!(b_commit["type"], "finished_receipt_commit");
        assert_eq!(b_commit["sender_role"], "B");
        // 重复接管同一预留必须拒绝。
        assert_eq!(
            driver
                .accept_reserved(&reservation, &relay.trust)
                .unwrap_err(),
            "controller_handshake_stage_invalid"
        );
        // 收到 A 的 commitment 之前不能回送 Finished。
        assert_eq!(
            driver.accept_joined(&joined).unwrap_err(),
            "controller_handshake_stage_invalid"
        );

        let a_commit = a.accept_reserved(&reservation).unwrap();
        let b_finished = driver.accept_peer_commitment(&a_commit).unwrap();
        assert_eq!(b_finished["type"], "encrypted_handshake_finished");
        assert_eq!(b_finished["sender_role"], "B");
        let a_finished = a.accept_peer_commitment(&b_commit).unwrap();
        let b_ack = driver.accept_peer_finished(&a_finished).unwrap();
        assert_eq!(b_ack["type"], "finished_receipt_ack");
        assert_eq!(b_ack["sender_role"], "A");
        assert_eq!(b_ack["receiver_role"], "B");
        // A 校验 B 的 Finished 与其 commitment 一致后产出 ack。
        let a_ack = a.accept_peer_finished(&b_finished).unwrap();
        assert_eq!(a_ack["sender_role"], "B");
        assert_eq!(a_ack["receiver_role"], "A");

        let outcome = driver.accept_joined(&joined).unwrap();
        assert_eq!(outcome.lease.connection_id, "connection-0001");
        assert_eq!(outcome.lease.controller_lease_id, "controller-lease-0001");
        assert_eq!(outcome.lease.lease_epoch, 1);
        assert_eq!(outcome.lease.remote_control_generation, "generation-0001");
        assert_eq!(outcome.lease.liveness_epoch_a, "liveness-epoch-a-0001");
        assert_eq!(outcome.lease.resume_deadline_ms, 30_000);
        assert_eq!(outcome.lease.resume_token, resume_token);
        // 两端绑定同一份验签 transcript（传输指纹的派生输入）。
        assert_eq!(outcome.transcript_hash, a.transcript_hash().unwrap());
        // 双收据完成后 joined 只允许一次。
        assert_eq!(
            driver.accept_joined(&joined).unwrap_err(),
            "controller_handshake_stage_invalid"
        );
    }

    /// 绑定不符的 `controller_joined` 必须失败关闭。
    #[test]
    fn driver_rejects_joined_with_a_foreign_reservation_binding() {
        let PairingFixture {
            relay,
            reservation,
            joined,
            a_keys,
            b_keys,
            ..
        } = crate::features::remote_control::controller::test_support::pairing_fixture(
            "relay-key-runtime",
        );
        let (mut driver, _a) = drive_through_receipts(&reservation, b_keys, a_keys, &relay.trust);
        let mut foreign = joined;
        foreign["reservation_id"] = json!("reservation-other");
        assert_eq!(
            driver.accept_joined(&foreign).unwrap_err(),
            "controller_joined_binding_mismatch"
        );
    }

    /// B 端租约必须带上与验签 transcript 绑定的传输指纹、恢复材料与单调时钟
    /// 连接时刻，并把恢复凭证从 Debug 输出里剔除。
    #[test]
    fn controlled_lease_binds_fingerprint_and_recovery_material() {
        let PairingFixture {
            relay,
            reservation,
            joined,
            a_keys,
            b_keys,
            resume_token,
            ..
        } = crate::features::remote_control::controller::test_support::pairing_fixture(
            "relay-key-runtime",
        );
        let (mut driver, _a) = drive_through_receipts(&reservation, b_keys, a_keys, &relay.trust);
        let outcome = driver.accept_joined(&joined).unwrap();
        let expected =
            transport_key_fingerprint(outcome.transcript_hash, HandshakeRole::B).unwrap();
        let lease = controlled_lease_from_outcome(outcome, 12_345).unwrap();

        assert_eq!(lease.context.controller_lease_id, "controller-lease-0001");
        assert_eq!(lease.context.lease_epoch, "1");
        assert_eq!(lease.context.remote_control_generation, "generation-0001");
        assert_eq!(lease.liveness_epoch_a, "liveness-epoch-a-0001");
        assert_eq!(lease.resume_deadline_b, 30_000);
        assert_eq!(lease.resume_token, resume_token);
        assert_eq!(lease.transport_key_fingerprint, expected);
        assert_eq!(
            lease.controller_device_name, "",
            "冻结协议不携带主控设备名，首版留空由 UI 兜底"
        );
        assert_eq!(lease.connected_at_b, 12_345);
        let debug = format!("{lease:?}");
        assert!(debug.contains("[REDACTED]"));
        assert!(
            !debug.contains(&base64::engine::general_purpose::STANDARD.encode(resume_token)),
            "恢复凭证不得出现在 Debug 输出里"
        );
        assert_eq!(
            lease.negotiated_capabilities,
            vec![
                "encrypted_frames_v1".to_owned(),
                "pairing_v1".to_owned(),
                "task_v1".to_owned()
            ]
        );
    }

    /// E2E-04：解密后的业务帧按 payload_kind 派发，回包里的 session_id 统一改写
    /// 回 A 端的会话引用；未知类型与畸形载荷不产生任何动作。
    #[tokio::test]
    async fn decrypted_payloads_dispatch_and_rewrite_the_session_reference() {
        use std::sync::Arc;

        #[derive(Default)]
        struct StubPort {
            action_calls: std::sync::atomic::AtomicUsize,
            block: Option<(Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>)>,
        }

        #[async_trait::async_trait]
        impl ControlledBusinessPort for StubPort {
            async fn remote_action(
                &self,
                _lease: &LeaseContext,
                _session_ref: &str,
                _operation: &str,
                _body: &Value,
            ) -> Result<Value, String> {
                self.action_calls
                    .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                if let Some((started, release)) = &self.block {
                    started.notify_one();
                    release.notified().await;
                }
                Ok(json!({"applied": true}))
            }
            async fn capability_snapshot(
                &self,
                _lease: &LeaseContext,
                request_id: &str,
                _session_id: &str,
                known_revision: u64,
            ) -> Result<Value, String> {
                if known_revision == 999 {
                    return Err("capability_source_unavailable".to_owned());
                }
                Ok(json!({
                    "v": 1,
                    "type": "capability_snapshot",
                    "request_id": request_id,
                    "capability_revision": (known_revision + 1).to_string(),
                }))
            }

            async fn submit_task(
                &self,
                _lease: &LeaseContext,
                session_ref: &str,
                payload: &Value,
            ) -> Result<Value, String> {
                // B 权威会话 id 与 A 的引用不同，由派发层改写回去。
                Ok(json!({
                    "v": 1,
                    "type": "task_status",
                    "task_id": payload["task_id"],
                    "session_id": format!("{session_ref}-authoritative"),
                    "task_status": "succeeded",
                }))
            }

            async fn cancel_task(
                &self,
                _lease: &LeaseContext,
                _session_ref: &str,
                task_id: &str,
            ) -> Result<Option<Value>, String> {
                Ok(Some(json!({
                    "v": 1,
                    "type": "task_status",
                    "task_id": task_id,
                    "session_id": "session-authoritative",
                    "task_status": "cancelled",
                })))
            }

            async fn replay_journal(
                &self,
                _lease: &LeaseContext,
                _session_ref: &str,
                _journal_epoch: &str,
                _after_sequence: u64,
            ) -> Result<Vec<(String, Value)>, String> {
                Ok(Vec::new())
            }

            async fn session_snapshot(
                &self,
                _lease: &LeaseContext,
                _session_ref: &str,
                _request_id: &str,
            ) -> Result<Value, String> {
                Ok(json!({"v": 1, "type": "session_snapshot"}))
            }
        }

        let PairingFixture { reservation, .. } =
            crate::features::remote_control::controller::test_support::pairing_fixture(
                "relay-key-dispatch",
            );
        let lease =
            crate::features::remote_control::controller::test_support::lease_context(&reservation);
        let port = StubPort::default();

        let action_results = RemoteActionResultCache::default();
        let action = json!({
            "request_id":"action-1","session_id":"session-a-ref",
            "operation":"permission_decide",
            "body":{"task_id":"task-1","permission_request_id":"permission-1","decision":"allow"}
        });
        let first = dispatch_decrypted_payload_cached(
            &port,
            &action_results,
            &lease,
            "remote_action_request",
            &action,
        )
        .await
        .unwrap();
        let retry = dispatch_decrypted_payload_cached(
            &port,
            &action_results,
            &lease,
            "remote_action_request",
            &action,
        )
        .await
        .unwrap();
        assert_eq!(first, retry);
        assert_eq!(
            port.action_calls.load(std::sync::atomic::Ordering::SeqCst),
            1
        );
        let conflict = dispatch_decrypted_payload_cached(
            &port,
            &action_results,
            &lease,
            "remote_action_request",
            &json!({
                "request_id":"action-1","session_id":"other-session",
                "operation":"permission_decide","body":action["body"],
            }),
        )
        .await
        .unwrap();
        assert_eq!(conflict[0].1["error_code"], "result_unknown");
        assert_eq!(
            port.action_calls.load(std::sync::atomic::Ordering::SeqCst),
            1,
            "a reused request id with a different binding must never execute"
        );

        let frames = dispatch_decrypted_payload(
            &port,
            &lease,
            "capability_snapshot_request",
            &json!({"request_id": "request-1", "session_id": "session-a-ref", "known_revision": "4"}),
        )
        .await
        .unwrap();
        assert_eq!(frames.len(), 1);
        let (kind, response) = frames.into_iter().next().unwrap();
        assert_eq!(kind, "capability_snapshot");
        assert_eq!(response["request_id"], "request-1");
        assert_eq!(response["capability_revision"], "5");

        let frames = dispatch_decrypted_payload(
            &port,
            &lease,
            "capability_snapshot_request",
            &json!({
                "request_id": "request-failed",
                "session_id": "session-a-ref",
                "known_revision": "999",
            }),
        )
        .await
        .unwrap();
        let (kind, response) = frames.into_iter().next().unwrap();
        assert_eq!(kind, "capability_snapshot_error");
        assert_eq!(response["request_id"], "request-failed");
        assert_eq!(response["session_id"], "session-a-ref");
        assert_eq!(response["code"], "capability_snapshot_unavailable");

        let frames = dispatch_decrypted_payload(
            &port,
            &lease,
            "task_submit",
            &json!({
                "task_id": "task-0001",
                "session_id": "session-a-ref",
            }),
        )
        .await
        .unwrap();
        assert_eq!(frames.len(), 1);
        let (kind, response) = frames.into_iter().next().unwrap();
        assert_eq!(kind, "task_status");
        assert_eq!(response["task_id"], "task-0001");
        assert_eq!(
            response["session_id"], "session-a-ref",
            "回包必须改写成 A 端的会话引用"
        );

        let frames = dispatch_decrypted_payload(
            &port,
            &lease,
            "task_cancel",
            &json!({"task_id": "task-0001", "session_id": "session-a-ref"}),
        )
        .await
        .unwrap();
        assert_eq!(frames.len(), 1);
        let (kind, response) = frames.into_iter().next().unwrap();
        assert_eq!(kind, "task_status");
        assert_eq!(response["session_id"], "session-a-ref");

        // 未知类型丢弃；缺字段的载荷失败关闭。
        assert!(
            dispatch_decrypted_payload(&port, &lease, "journal_event", &json!({}))
                .await
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            dispatch_decrypted_payload(&port, &lease, "task_submit", &json!({"task_id": "t"}))
                .await
                .unwrap_err(),
            "task_request_invalid"
        );

        // A disconnect may occur after the side effect but before its result
        // is cached. Keep this in-flight call on the same serial executor;
        // the resumed retry must see the cache, never re-execute the action.
        let started = Arc::new(tokio::sync::Notify::new());
        let release = Arc::new(tokio::sync::Notify::new());
        let blocking = Arc::new(StubPort {
            action_calls: Default::default(),
            block: Some((started.clone(), release.clone())),
        });
        let cache = Arc::new(RemoteActionResultCache::default());
        let epoch = Arc::new(std::sync::atomic::AtomicU64::new(0));
        let outputs = Arc::new(parking_lot::Mutex::new(Vec::new()));
        let completions = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let done = Arc::new(tokio::sync::Notify::new());
        let worker = super::super::business_worker::SerialBusinessWorker::start(4, {
            let port = blocking.clone();
            let cache = cache.clone();
            let epoch = epoch.clone();
            let outputs = outputs.clone();
            let completions = completions.clone();
            let done = done.clone();
            let lease = lease.clone();
            move |(admitted_epoch, payload): (u64, Value)| {
                let port = port.clone();
                let cache = cache.clone();
                let epoch = epoch.clone();
                let outputs = outputs.clone();
                let completions = completions.clone();
                let done = done.clone();
                let lease = lease.clone();
                async move {
                    let result = dispatch_decrypted_payload_cached(
                        port.as_ref(),
                        &cache,
                        &lease,
                        "remote_action_request",
                        &payload,
                    )
                    .await
                    .unwrap();
                    if epoch.load(std::sync::atomic::Ordering::Acquire) == admitted_epoch {
                        outputs.lock().push(result);
                    }
                    completions.fetch_add(1, std::sync::atomic::Ordering::AcqRel);
                    done.notify_one();
                }
            }
        });
        worker.try_submit((0, action.clone())).unwrap();
        tokio::time::timeout(Duration::from_secs(2), started.notified())
            .await
            .unwrap();
        worker
            .try_submit((0, json!({"request_id":"discard-queued"})))
            .unwrap();
        epoch.store(1, std::sync::atomic::Ordering::Release);
        worker.clear_pending();
        worker.try_submit((1, action)).unwrap();
        release.notify_one();
        tokio::time::timeout(Duration::from_secs(2), async {
            while completions.load(std::sync::atomic::Ordering::Acquire) < 2 {
                done.notified().await;
            }
        })
        .await
        .unwrap();
        assert_eq!(
            blocking
                .action_calls
                .load(std::sync::atomic::Ordering::Acquire),
            1
        );
        assert_eq!(
            outputs.lock().len(),
            1,
            "only the resumed retry may publish its cached result"
        );
    }

    /// E2E-05：B → A 的 journal_event 信封必须通过冻结运行时校验（角色 B +
    /// snapshot_journal_v1），且明文字段与事件本体一致。
    #[test]
    fn journal_event_payload_matches_the_frozen_contract() {
        use crate::features::remote_control::controller::journal::JournalEvent;
        use crate::features::remote_control::controller::protocol::{
            RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
        };

        let PairingFixture { reservation, .. } =
            crate::features::remote_control::controller::test_support::pairing_fixture(
                "relay-key-journal",
            );
        let lease =
            crate::features::remote_control::controller::test_support::lease_context(&reservation);
        let event = JournalEvent {
            event_id: "task-task-0001:1".to_owned(),
            epoch: "epoch-1".to_owned(),
            sequence: 1,
            kind: "task_status".to_owned(),
            session_id: "session-authoritative".to_owned(),
            task_id: Some("task-0001".to_owned()),
            terminal_status: Some("succeeded".to_owned()),
            payload: json!({"status": "succeeded", "sequence": "1"}),
        };
        let payload = build_journal_event_payload(&lease, "session-a-ref", &event).unwrap();
        assert_eq!(payload["session_id"], "session-a-ref");
        assert_eq!(payload["journal_epoch"], "epoch-1");
        assert_eq!(payload["event_sequence"], "1");
        assert_eq!(
            payload["event_kind"], "terminal",
            "内部 task_status（终态）在 wire 上映射为冻结闭集的 terminal"
        );
        let capabilities = vec![
            "encrypted_frames_v1".to_owned(),
            "pairing_v1".to_owned(),
            "snapshot_journal_v1".to_owned(),
            "task_v1".to_owned(),
        ];
        validate_controller_runtime_payload_json(
            &payload.to_string(),
            SenderRole::ControlledB,
            Some(&RuntimeValidationContext {
                lease: Some(lease.clone()),
                negotiated_capabilities: capabilities,
            }),
        )
        .expect("journal_event 信封必须满足冻结契约");
        // 解码回来的事件与本体一致。
        let decoded: JournalEvent = serde_json::from_slice(
            &base64::engine::general_purpose::STANDARD
                .decode(payload["event_b64"].as_str().unwrap())
                .unwrap(),
        )
        .unwrap();
        let mut expected = event.clone();
        expected.session_id = "session-a-ref".to_owned();
        assert_eq!(decoded, expected);
        // 进行中的任务状态与附件/快照不回传（由 task_status 帧或后续任务负责）。
        let mut running = event.clone();
        running.kind = "task_status".to_owned();
        running.terminal_status = None;
        assert!(build_journal_event_payload(&lease, "session-a-ref", &running).is_none());
        let mut attachment = event.clone();
        attachment.kind = "attachment".to_owned();
        assert!(build_journal_event_payload(&lease, "session-a-ref", &attachment).is_none());
    }

    /// E2E-06：B 端恢复序列——掉线后发 prepare，provisional 回 confirm，校验对端
    /// confirm 后依次发 activation_ack / post_activation_liveness / ready_ack，
    /// `controller_resumed` 后重开业务闸门。
    #[test]
    fn controlled_resume_sequence_sends_every_frame_and_reopens_business() {
        use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
        use crate::features::remote_control::controller::test_support::{
            lease_context, paired_session_keys, pairing_fixture,
        };

        let fixture = pairing_fixture("relay-key-b-resume");
        let (a_session, b_session) =
            paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        // `controlled_monotonic_millis` has a process-wide origin. Anchor the
        // fixture to the current clock so a long-running/full test suite does
        // not turn this otherwise-valid resume lease into an expired one.
        let now_b = controlled_monotonic_millis();
        let resume_deadline_b = now_b.saturating_add(30_000);
        // 租约的 generation 必须与本代一致（complete_connected 会据此校验）。
        let lease_context = LeaseContext {
            remote_control_generation: operation.generation().to_owned(),
            ..lease_context(&fixture.reservation)
        };
        manager
            .complete_registration(
                operation.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: resume_deadline_b,
                },
                now_b,
            )
            .unwrap();
        manager.mark_pairing_reserved(operation.clone()).unwrap();
        manager.mark_handshake_pending(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation.clone(),
                ControlledLease {
                    context: lease_context.clone(),
                    liveness_epoch_a: "live-a-1".into(),
                    resume_deadline_b,
                    resume_token: [1; 32],
                    transport_key_fingerprint: [2; 32],
                    controller_device_name: "Controller PC".into(),
                    connected_at_b: now_b,
                    negotiated_capabilities: vec![
                        "encrypted_frames_v1".into(),
                        "resume_v1".into(),
                        "role_liveness_v1".into(),
                    ],
                },
            )
            .unwrap();
        let transport = super::super::test_support::resume_reader_transport(
            super::super::relay_transport::ControllerRole::ControlledB,
            lease_context.clone(),
            vec![
                "encrypted_frames_v1".into(),
                "resume_v1".into(),
                "role_liveness_v1".into(),
            ],
        );
        manager
            .replace_authenticated_transport(operation.clone(), transport.clone())
            .unwrap();
        let lane = ControlledLaneState::default();
        lane.install(HandshakeRole::B, &lease_context, b_session)
            .unwrap();
        let peer = ControllingLaneState::default();
        peer.install(HandshakeRole::A, &lease_context, a_session)
            .unwrap();

        let port = FakePort::new(vec![], false);
        let heartbeat_port =
            super::super::controlled_heartbeat::test_support::CapturePort::default();
        let mut heartbeat = super::super::controlled_heartbeat::ControlledHeartbeat::default();
        heartbeat
            .install(lease_context.clone(), "live-b-1")
            .unwrap();
        heartbeat
            .pump(&manager, &operation, false, &heartbeat_port)
            .unwrap();
        assert_eq!(heartbeat_port.frames.lock()[0]["heartbeat_sequence"], "1");
        let mut resume = ControlledResumeAttempt::default();
        lane.suspend();
        super::super::relay_transport::disconnect_test_transport(&transport);
        assert!(begin_controlled_resume(&port, &manager, &mut resume));
        heartbeat
            .pump(&manager, &operation, resume.is_active(), &heartbeat_port)
            .unwrap();
        assert_eq!(
            heartbeat_port.frames.lock().len(),
            1,
            "recovery never emits role evidence"
        );
        assert!(resume.is_active(), "恢复尝试必须处于进行中");
        let sent_request = port.sent.lock().unwrap()[0]["request_id"]
            .as_str()
            .unwrap()
            .to_owned();
        let before_error = manager.status();
        for error in [
            json!({"v":1,"type":"error","request_id":"request-foreign-0001",
                "code":"invalid_message","message":"resume rejected"}),
            json!({"v":2,"type":"error","request_id":sent_request,
                "code":"invalid_message","message":"legacy rejected"}),
            json!({"v":1,"type":"error","request_id":sent_request,"message":"missing code"}),
        ] {
            assert_eq!(
                handle_controlled_resume_frame(&port, &manager, &lane, &mut resume, &error),
                ControlledResumeOutcome::Ignored
            );
            assert_eq!(manager.status(), before_error);
        }
        // The worker consumes Failed synchronously with fail_operation before
        // ending the lane; tuple-less Relay rejection must not wait for TTL.
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({"v":1,"type":"error","request_id":sent_request,
                "code":"invalid_message","message":"resume rejected"})
            ),
            ControlledResumeOutcome::Failed
        );
        let activation = "activation-0001";

        // provisional → B 回 confirm。
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({
                    "type": "controller_resume_provisional",
                    "activation_id": activation,
                    "connection_id": lease_context.connection_id,
                    "controller_lease_id": lease_context.controller_lease_id,
                    "lease_epoch": lease_context.lease_epoch,
                    "relay_epoch": lease_context.relay_epoch,
                    "remote_control_generation": lease_context.remote_control_generation,
                }),
            ),
            ControlledResumeOutcome::Progressed
        );
        // A 的 confirm（同一 resume 秘密）必须验签通过。
        let mismatched_confirm = peer
            .seal_resume_confirm("request-resume-wrong", "activation-wrong")
            .unwrap();
        let sent_before = port.sent.lock().unwrap().len();
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &mismatched_confirm
            ),
            ControlledResumeOutcome::Ignored
        );
        assert_eq!(port.sent.lock().unwrap().len(), sent_before);
        assert!(!resume.peer_confirmed && !resume.ack_sent);
        let peer_confirm = peer
            .seal_resume_confirm("request-resume-peer", activation)
            .unwrap();
        assert_eq!(
            handle_controlled_resume_frame(&port, &manager, &lane, &mut resume, &peer_confirm),
            ControlledResumeOutcome::Progressed
        );
        assert!(
            resume.ack_sent,
            "B must send ACK before Relay can generate pending"
        );
        assert_eq!(
            port.sent.lock().unwrap().last().unwrap()["type"],
            "controller_resume_activation_ack"
        );
        let sent_before = port.sent.lock().unwrap().len();
        assert_eq!(
            handle_controlled_resume_frame(&port, &manager, &lane, &mut resume, &peer_confirm),
            ControlledResumeOutcome::Ignored
        );
        assert_eq!(port.sent.lock().unwrap().len(), sent_before);
        // 审核回归：旧 generation 的恢复帧必须被丢弃且不影响相位。
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({
                    "type": "controller_resume_activation_pending",
                    "activation_id": activation,
                    "connection_id": lease_context.connection_id,
                    "controller_lease_id": lease_context.controller_lease_id,
                    "lease_epoch": lease_context.lease_epoch,
                    "relay_epoch": lease_context.relay_epoch,
                    "remote_control_generation": "generation-stale",
                }),
            ),
            ControlledResumeOutcome::Ignored
        );
        // Relay pending follows B's ACK; now B sends post-activation liveness.
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({
                    "type": "controller_resume_activation_pending",
                    "activation_id": activation,
                    "connection_id": lease_context.connection_id,
                    "controller_lease_id": lease_context.controller_lease_id,
                    "lease_epoch": lease_context.lease_epoch,
                    "relay_epoch": lease_context.relay_epoch,
                    "remote_control_generation": lease_context.remote_control_generation,
                }),
            ),
            ControlledResumeOutcome::Progressed
        );
        let epoch_b = resume.liveness_epoch_b.clone();
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({
                    "type": "controller_post_activation_liveness_confirmed",
                    "activation_id": activation,
                    "liveness_epoch_b": epoch_b,
                    "heartbeat_sequence_b": "1",
                    "connection_id": lease_context.connection_id,
                    "controller_lease_id": lease_context.controller_lease_id,
                    "lease_epoch": lease_context.lease_epoch,
                    "relay_epoch": lease_context.relay_epoch,
                    "remote_control_generation": lease_context.remote_control_generation,
                }),
            ),
            ControlledResumeOutcome::Progressed
        );
        let final_frame = json!({
            "v": 1, "type": "controller_resumed", "request_id": "request-final-resume", "activation_id": activation,
            "liveness_epoch_a": "live-a-2", "liveness_epoch_b": epoch_b,
            "resume_deadline_ms": "1700000030000",
            "new_resume_token_b64": base64::engine::general_purpose::STANDARD.encode([3_u8; 32]),
            "connection_id": lease_context.connection_id,
            "controller_lease_id": lease_context.controller_lease_id,
            "lease_epoch": lease_context.lease_epoch,
            "relay_epoch": lease_context.relay_epoch,
            "remote_control_generation": lease_context.remote_control_generation,
        });
        super::super::protocol::validate_controller_runtime_message_json(
            &final_frame.to_string(),
            SenderRole::Relay,
            Some(&super::super::protocol::RuntimeValidationContext {
                lease: Some(lease_context.clone()),
                negotiated_capabilities: vec![
                    "encrypted_frames_v1".into(),
                    "resume_v1".into(),
                    "role_liveness_v1".into(),
                ],
            }),
        )
        .unwrap();
        let mut wrong_epoch = final_frame.clone();
        wrong_epoch["liveness_epoch_b"] = json!("live-b-foreign");
        assert_eq!(
            handle_controlled_resume_frame(&port, &manager, &lane, &mut resume, &wrong_epoch),
            ControlledResumeOutcome::Ignored
        );
        assert!(resume.is_active());
        super::super::relay_transport::dispatch_test_transport_frame(&transport, &final_frame);
        assert!(
            lane.is_suspended(),
            "foreign B epoch cannot reopen the lane or install heartbeat state"
        );
        assert_eq!(
            handle_controlled_resume_frame(&port, &manager, &lane, &mut resume, &final_frame,),
            ControlledResumeOutcome::Completed
        );
        assert!(!resume.is_active());
        assert!(manager.live_lease().is_ok(), "恢复完成后必须回到 connected");
        assert_eq!(manager.live_liveness_epoch().as_deref(), Some("live-a-2"));
        assert!(!lane.is_suspended(), "恢复完成后业务通道必须重新打开");
        heartbeat.install(lease_context.clone(), &epoch_b).unwrap();
        heartbeat
            .pump(&manager, &operation, resume.is_active(), &heartbeat_port)
            .unwrap();
        let heartbeat_frames = heartbeat_port.frames.lock();
        assert_eq!(heartbeat_frames[1]["liveness_epoch"], epoch_b);
        assert_eq!(heartbeat_frames[1]["heartbeat_sequence"], "1");
        drop(heartbeat_frames);
        let now_b = controlled_monotonic_millis();
        manager.mark_transport_disconnected(now_b);
        let second = manager
            .begin_resume(manager.current_operation().unwrap(), now_b)
            .unwrap();
        assert_eq!(second.relay_deadline_identity_ms, 1_700_000_030_000);
        let sent = port.sent.lock().unwrap();
        let kinds: Vec<&str> = sent
            .iter()
            .filter_map(|frame| frame.get("type").and_then(Value::as_str))
            .collect();
        assert_eq!(
            kinds,
            vec![
                "controller_resume_prepare",
                "encrypted_resume_confirm",
                "controller_resume_activation_ack",
                "controller_post_activation_liveness",
                "controller_resume_ready_ack",
            ]
        );
    }

    /// 出站旁路（journal 回传句柄）：安装后可取用同一代 sender，清空后不再可发。
    #[test]
    fn controlled_outbound_state_installs_and_clears() {
        let state = ControlledOutboundState::default();
        assert!(state.sender().is_none());
        state.clear();
        assert!(state.sender().is_none());
    }

    /// E2E-06：证据动作遵循 watchdog 策略——余量充足不打扰；需要证据且无挑战时
    /// 发起；挑战过久升级为 Relay 签名兜底；再超时作废重来。
    #[test]
    fn liveness_evidence_actions_follow_the_watchdog_policy() {
        let idle = LivenessEvidenceState::default();
        assert_eq!(
            liveness_evidence_action(false, &idle, 0),
            LivenessEvidenceAction::Idle
        );
        assert_eq!(
            liveness_evidence_action(true, &idle, 0),
            LivenessEvidenceAction::Challenge
        );
        let outstanding = LivenessEvidenceState {
            outstanding: Some(("challenge-1".to_owned(), "AAAA".to_owned(), 1_000)),
            escalated: false,
            attestation_sent_at_b: None,
        };
        assert_eq!(
            liveness_evidence_action(true, &outstanding, 2_000),
            LivenessEvidenceAction::Idle,
            "刚发出的挑战应等待直接应答"
        );
        assert_eq!(
            liveness_evidence_action(true, &outstanding, 3_500),
            LivenessEvidenceAction::Escalate
        );
        let escalated = LivenessEvidenceState {
            escalated: true,
            attestation_sent_at_b: Some(3_500),
            ..outstanding.clone()
        };
        assert_eq!(
            liveness_evidence_action(true, &escalated, 3_500),
            LivenessEvidenceAction::Idle,
            "同一挑战只升级一次"
        );
        assert_eq!(
            liveness_evidence_action(true, &escalated, 12_000),
            LivenessEvidenceAction::Reset,
            "挑战超时后作废重来"
        );
        assert_eq!(
            liveness_evidence_action(false, &outstanding, 12_000),
            LivenessEvidenceAction::Idle
        );
    }

    #[test]
    fn controlled_role_heartbeat_matches_relay_fixture_and_fences_replaced_lease() {
        use super::super::controlled_heartbeat::{
            ControlledHeartbeat,
            test_support::{CapturePort, connect},
        };
        let expected: Value = serde_json::from_str(include_str!(
            "../../../../../tests/fixtures/controller_role_b_heartbeat.json"
        ))
        .unwrap();
        let lease = LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "epoch-test-1".into(),
            remote_control_generation: "generation-0001".into(),
        };
        assert_eq!(
            super::super::liveness::build_controller_liveness(
                &lease,
                SenderRole::ControlledB,
                "live-b-1",
                1,
            )
            .unwrap(),
            expected,
            "the shared production builder must match the Relay fixture exactly"
        );
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        let mut owned_lease = lease;
        owned_lease.remote_control_generation = operation.generation().into();
        connect(
            &manager,
            &operation,
            owned_lease.clone(),
            "live-a-1",
            vec!["role_liveness_v1".into()],
        );
        let port = CapturePort::default();
        let mut state = ControlledHeartbeat::default();
        state.pump(&manager, &operation, false, &port).unwrap();
        assert!(port.frames.lock().is_empty());
        state.install(owned_lease.clone(), "live-b-1").unwrap();
        state.pump(&manager, &operation, false, &port).unwrap();
        let mut owned_expected = expected;
        owned_expected["remote_control_generation"] = json!(operation.generation());
        assert_eq!(port.frames.lock()[0], owned_expected);
        state.pump(&manager, &operation, false, &port).unwrap();
        assert_eq!(port.frames.lock()[1]["heartbeat_sequence"], "2");
        state.install(owned_lease.clone(), "live-b-1").unwrap();
        state.pump(&manager, &operation, false, &port).unwrap();
        assert_eq!(port.frames.lock()[2]["heartbeat_sequence"], "3");
        let mut replacement = owned_lease.clone();
        replacement.remote_control_generation = "generation-0002".into();
        state.install(replacement, "live-b-1").unwrap();
        assert!(state.pump(&manager, &operation, false, &port).is_err());
        assert_eq!(port.frames.lock().len(), 3);
        state.install(owned_lease, "live-b-2").unwrap();
        state.pump(&manager, &operation, false, &port).unwrap();
        let resumed = port.frames.lock()[3].clone();
        assert_eq!(resumed["liveness_epoch"], "live-b-2");
        assert_eq!(resumed["heartbeat_sequence"], "1");
        manager.stop();
        manager.start(None, Vec::new()).unwrap();
        assert!(state.pump(&manager, &operation, false, &port).is_err());
        assert_eq!(port.frames.lock().len(), 4);
        // Sequence overflow and failed-enqueue retry are covered by the same
        // producer's tests; do not reintroduce a weaker test-only builder.
    }

    #[test]
    fn fallback_request_rtt_does_not_include_direct_wait_or_reset_challenge_age() {
        let mut state = LivenessEvidenceState {
            outstanding: Some((
                "challenge-1".into(),
                "AAAAAAAAAAAAAAAAAAAAAA==".into(),
                1_000,
            )),
            ..Default::default()
        };
        let response = json!({"challenge_id": "challenge-1"});
        assert!(state.attestation_request_start(&response).is_none());
        // A 2.5s threshold on a 2s pump escalates after a 4s direct wait.
        state.record_attestation_sent(5_000);
        assert_eq!(state.attestation_request_start(&response), Some(5_000));
        assert_eq!(state.outstanding.as_ref().unwrap().2, 1_000);
        assert!(
            state
                .attestation_request_start(&json!({"challenge_id": "other"}))
                .is_none()
        );
        assert_eq!(
            liveness_evidence_action(true, &state, 9_000),
            LivenessEvidenceAction::Idle
        );
        assert_eq!(
            liveness_evidence_action(true, &state, 10_000),
            LivenessEvidenceAction::Idle
        );
        assert_eq!(
            liveness_evidence_action(true, &state, 10_001),
            LivenessEvidenceAction::Reset
        );
    }

    #[test]
    fn quantized_poll_preserves_full_fallback_rtt_and_frozen_authentication_cutoff() {
        use super::super::liveness::{ControllerLivenessWatchdog, LivenessWatchdogState};
        let mut watchdog = ControllerLivenessWatchdog::authenticated_cycle("live-a-1", 0).unwrap();
        assert!(!watchdog.needs_evidence(2_000));
        assert!(watchdog.needs_evidence(4_000));
        let nonce = "AAAAAAAAAAAAAAAAAAAAAA==";
        watchdog
            .register_challenge("challenge-1", nonce, 4_000)
            .unwrap();
        let frozen = watchdog.freeze_deadline(4_000);
        let mut state = LivenessEvidenceState {
            outstanding: Some(("challenge-1".into(), nonce.into(), 4_000)),
            ..Default::default()
        };
        assert_eq!(
            liveness_evidence_action(true, &state, 6_000),
            LivenessEvidenceAction::Idle
        );
        assert_eq!(
            liveness_evidence_action(true, &state, 8_000),
            LivenessEvidenceAction::Escalate
        );
        state.record_attestation_sent(8_000);
        // At the original 8s challenge timeout the fallback still has 1s left.
        assert_eq!(
            liveness_evidence_action(true, &state, 12_000),
            LivenessEvidenceAction::Idle
        );
        assert_eq!(
            liveness_evidence_action(true, &state, 13_000),
            LivenessEvidenceAction::Idle
        );
        let response = json!({
            "v": 1, "type": "controller_liveness_attestation_a", "request_id": "request-1",
            "connection_id": "connection-0001", "controller_lease_id": "lease-0001", "lease_epoch": "1",
            "relay_epoch": "epoch-test-1", "remote_control_generation": "generation-0001",
            "liveness_epoch_a": "live-a-1", "heartbeat_sequence_a": "1", "attestation_sequence": "1",
            "challenge_id": "challenge-1", "challenge_b64": nonce,
            "observed_age_ms": "5000", "remaining_ttl_ms": "25000", "issued_at_relay": "9000",
            "relay_signing_key_id": "relay-key-test-1", "signature_algorithm": "ed25519",
            "relay_signature_b64": format!("{}==", "A".repeat(86)),
        });
        let lease = LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "epoch-test-1".into(),
            remote_control_generation: "generation-0001".into(),
        };
        watchdog
            .accept_attestation_json(
                &response.to_string(),
                &lease,
                state.attestation_request_start(&response).unwrap(),
                13_000,
            )
            .unwrap();
        assert_eq!(
            watchdog.deadline_b(),
            frozen,
            "proof cannot extend a frozen cutoff"
        );
        assert_eq!(watchdog.state(13_000), LivenessWatchdogState::Healthy);
        assert_eq!(
            liveness_evidence_action(true, &state, 13_001),
            LivenessEvidenceAction::Reset
        );
        assert_eq!(
            watchdog.state(frozen.unwrap()),
            LivenessWatchdogState::Expired
        );
    }
}
