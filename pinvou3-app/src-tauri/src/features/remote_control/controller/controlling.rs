//! Windows controller-device (A) connection owner.
//!
//! A keeps the short pairing code and resume token only in memory. Closing the
//! remote message window is invisible here; only an explicit disconnect or
//! transport failure clears the live lease context.

use parking_lot::Mutex;
use serde::Serialize;
use uuid::Uuid;
use zeroize::{Zeroize, Zeroizing};

use super::liveness::validate_liveness_epoch;
use super::protocol::LeaseContext;
use super::relay_transport::{ControllerRole, ControllerSendError, ControllerTransportSender};
use super::resume::ControllerResumeGate;

const MAX_DEVICE_NAME_CHARS: usize = 80;

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum ControllingStatusKind {
    Idle,
    Connecting,
    SafetyHandshake,
    Connected,
    Reconnecting,
    Disconnected,
    Error,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ControllerOperation {
    intent: u64,
    claim_request_id: String,
}

impl ControllerOperation {
    /// A 端 claim 的稳定 id：Relay 以它做预留幂等键（重连只能换 `request_id`）。
    pub(crate) fn claim_request_id(&self) -> &str {
        &self.claim_request_id
    }
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ControllingStateError {
    pub(crate) code: &'static str,
}

impl ControllingStateError {
    const fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl std::fmt::Display for ControllingStateError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for ControllingStateError {}

type Result<T> = std::result::Result<T, ControllingStateError>;

#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControllingLeaseInfo {
    connection_id: String,
    controller_lease_id: String,
    lease_epoch: String,
    relay_epoch: String,
    remote_control_generation: String,
}

impl From<LeaseContext> for ControllingLeaseInfo {
    fn from(value: LeaseContext) -> Self {
        Self {
            connection_id: value.connection_id,
            controller_lease_id: value.controller_lease_id,
            lease_epoch: value.lease_epoch,
            relay_epoch: value.relay_epoch,
            remote_control_generation: value.remote_control_generation,
        }
    }
}

#[derive(Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControllingStatus {
    status: ControllingStatusKind,
    claim_request_id: Option<String>,
    has_pending_code: bool,
    device_display_name: Option<String>,
    lease: Option<ControllingLeaseInfo>,
    error_code: Option<&'static str>,
}

impl ControllingStatus {
    pub(crate) fn kind(&self) -> ControllingStatusKind {
        self.status
    }
}

impl std::fmt::Debug for ControllingStatus {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ControllingStatus")
            .field("status", &self.status)
            .field("claim_request_id", &self.claim_request_id)
            .field("has_pending_code", &self.has_pending_code)
            .field("device_display_name", &self.device_display_name)
            .field("lease", &self.lease)
            .field("error_code", &self.error_code)
            .finish()
    }
}

#[derive(Clone)]
pub(crate) struct ControllerLease {
    pub(crate) context: LeaseContext,
    /// B endpoint and pairing reservation are covered by the verified Relay transcript.
    pub(crate) peer_endpoint_id: String,
    pub(crate) reservation_id: String,
    pub(crate) device_display_name: String,
    pub(crate) liveness_epoch_a: String,
    pub(crate) resume_deadline_b: u64,
    pub(crate) resume_token: [u8; 32],
    pub(crate) transport_key_fingerprint: [u8; 32],
    /// 验签 transcript 协商出的能力集合（E2E-03 构造 payload 的准入上下文）。
    pub(crate) negotiated_capabilities: Vec<String>,
}

impl std::fmt::Debug for ControllerLease {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ControllerLease")
            .field("context", &self.context)
            .field("device_display_name", &self.device_display_name)
            .field("liveness_epoch_a", &self.liveness_epoch_a)
            .field("resume_deadline_b", &self.resume_deadline_b)
            .field("resume_token", &"[REDACTED]")
            .field("transport_key_fingerprint", &"[REDACTED]")
            .finish()
    }
}

/// E2E-06：A 端断线恢复所需的本地材料（令牌只以内存数组存在，Debug 脱敏）。
pub(crate) struct ControllingResumeMaterial {
    pub(crate) lease: LeaseContext,
    pub(crate) resume_token: [u8; 32],
    pub(crate) relay_deadline_identity_ms: u64,
}

impl std::fmt::Debug for ControllingResumeMaterial {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ControllingResumeMaterial")
            .field("lease", &self.lease)
            .field("resume_token", &"[REDACTED]")
            .field(
                "relay_deadline_identity_ms",
                &self.relay_deadline_identity_ms,
            )
            .finish()
    }
}

/// E2E-06：恢复流程的本地相位推进（service 层按 Relay/B 的帧顺序调用）。
#[derive(Debug, Clone, Copy)]
pub(crate) enum ResumeStep<'a> {
    Provisional { activation_id: &'a str },
    Confirmed { activation_id: &'a str },
    ActivationPending { activation_id: &'a str },
    LivenessConfirmed { activation_id: &'a str },
    ReadyAcknowledged { activation_id: &'a str },
}

/// 恢复失败的统一公共错误码（保守默认：A 需要重新取码连接）。
const RESUME_FAILED_ERROR: &str = "endpoint_offline";

struct PendingCode {
    claim_request_id: String,
    value: Zeroizing<String>,
}

impl Drop for PendingCode {
    fn drop(&mut self) {
        self.value.zeroize();
    }
}

struct Inner {
    phase: ControllingStatusKind,
    intent: u64,
    claim_request_id: Option<String>,
    pending_code: Option<PendingCode>,
    lease: Option<ControllerLease>,
    /// Identity of the last terminal transition only; never grants a live lease.
    retired_lease: Option<LeaseContext>,
    resume_gate: Option<ControllerResumeGate>,
    transport: Option<ControllerTransportSender>,
    error_code: Option<&'static str>,
}

impl Default for Inner {
    fn default() -> Self {
        Self {
            phase: ControllingStatusKind::Idle,
            intent: 0,
            claim_request_id: None,
            pending_code: None,
            lease: None,
            retired_lease: None,
            resume_gate: None,
            transport: None,
            error_code: None,
        }
    }
}

#[derive(Default)]
pub(crate) struct ControllingDeviceManager {
    inner: Mutex<Inner>,
}

impl ControllingDeviceManager {
    pub(crate) fn is_supported(&self) -> bool {
        super::super::platform::desktop_control_supported()
    }

    pub(crate) fn replace_transport(&self, transport: ControllerTransportSender) {
        let mut inner = self.inner.lock();
        if let Some(previous) = inner.transport.replace(transport) {
            previous.stop();
        }
    }

    pub(crate) fn stop(&self) {
        let mut inner = self.inner.lock();
        clear_live_context(&mut inner);
    }

    pub(crate) fn is_running(&self) -> bool {
        self.inner
            .lock()
            .transport
            .as_ref()
            .is_some_and(|transport| !transport.is_stopped())
    }

    pub(crate) fn connect(&self, raw_code: &str) -> Result<ControllerOperation> {
        let code = normalize_pairing_code(raw_code)?;
        let mut inner = self.inner.lock();
        if !matches!(
            inner.phase,
            ControllingStatusKind::Idle
                | ControllingStatusKind::Connecting
                | ControllingStatusKind::Disconnected
                | ControllingStatusKind::Error
        ) {
            return Err(ControllingStateError::new("controller_already_connecting"));
        }
        clear_live_context(&mut inner);
        inner.retired_lease = None;
        inner.intent += 1;
        let claim_request_id = format!("claim_{}", Uuid::new_v4().simple());
        inner.claim_request_id = Some(claim_request_id.clone());
        inner.pending_code = Some(PendingCode {
            claim_request_id: claim_request_id.clone(),
            value: Zeroizing::new(code),
        });
        inner.phase = ControllingStatusKind::Connecting;
        inner.error_code = None;
        Ok(ControllerOperation {
            intent: inner.intent,
            claim_request_id,
        })
    }

    pub(crate) fn pending_code(&self, operation: ControllerOperation) -> Result<String> {
        let inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        inner
            .pending_code
            .as_ref()
            .map(|pending| pending.value.to_string())
            .ok_or_else(|| ControllingStateError::new("pairing_code_unavailable"))
    }

    pub(crate) fn complete_reserved(
        &self,
        operation: ControllerOperation,
    ) -> Result<ControllingStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControllingStatusKind::Connecting {
            return Err(ControllingStateError::new("controller_phase_invalid"));
        }
        inner.pending_code = None;
        inner.phase = ControllingStatusKind::SafetyHandshake;
        Ok(status_locked(&inner))
    }

    pub(crate) fn complete_connected(
        &self,
        operation: ControllerOperation,
        lease: ControllerLease,
    ) -> Result<ControllingStatus> {
        let device_name = normalize_device_name(&lease.device_display_name)?;
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControllingStatusKind::SafetyHandshake {
            return Err(ControllingStateError::new("controller_phase_invalid"));
        }
        let gate = ControllerResumeGate::new(
            ControllerRole::ControllerA,
            lease.context.clone(),
            lease.resume_token,
            lease.transport_key_fingerprint,
            lease.resume_deadline_b,
        );
        inner.resume_gate = Some(gate);
        inner.lease = Some(ControllerLease {
            device_display_name: device_name,
            ..lease
        });
        inner.pending_code = None;
        inner.phase = ControllingStatusKind::Connected;
        Ok(status_locked(&inner))
    }

    pub(crate) fn mark_transport_disconnected(
        &self,
        now_a: u64,
        evidence_deadline_a: Option<u64>,
    ) -> ControllingStatus {
        let mut inner = self.inner.lock();
        if inner.phase == ControllingStatusKind::Connected {
            if inner
                .resume_gate
                .as_mut()
                .is_some_and(|gate| gate.disconnect(now_a, evidence_deadline_a).is_ok())
            {
                inner.phase = ControllingStatusKind::Reconnecting;
            } else {
                clear_live_context(&mut inner);
                inner.phase = ControllingStatusKind::Error;
                inner.error_code = Some("endpoint_offline");
            }
        }
        status_locked(&inner)
    }

    pub(crate) fn complete_resumed(
        &self,
        operation: ControllerOperation,
        activation_id: &str,
        now_b: u64,
        liveness_epoch_a: impl Into<String>,
        resume_token: [u8; 32],
        transport_key_fingerprint: [u8; 32],
        new_deadline_b: u64,
    ) -> Result<ControllingStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControllingStatusKind::Reconnecting {
            return Err(ControllingStateError::new("controller_phase_invalid"));
        }
        let Some(lease) = inner.lease.clone() else {
            return Err(ControllingStateError::new("lease_unavailable"));
        };
        // 先让旧 gate 用完整的 `controller_resumed`（同一 activation + 轮换后的
        // 令牌/指纹）消费旧材料；任何不符都终止候选。
        let liveness_epoch_a = liveness_epoch_a.into();
        let accepted = new_deadline_b > 0
            && validate_liveness_epoch(&liveness_epoch_a).is_ok()
            && match inner.resume_gate.as_mut() {
                Some(gate) => gate
                    .accept_resumed(
                        activation_id,
                        liveness_epoch_a,
                        resume_token,
                        transport_key_fingerprint,
                        now_b,
                        None,
                    )
                    .is_ok(),
                None => false,
            };
        if !accepted {
            clear_live_context(&mut inner);
            inner.phase = ControllingStatusKind::Error;
            inner.error_code = Some(RESUME_FAILED_ERROR);
            return Err(ControllingStateError::new("controller_resume_failed"));
        }
        let liveness_epoch_a = inner
            .resume_gate
            .as_ref()
            .and_then(|gate| gate.resumed_liveness_epoch().map(str::to_owned))
            .unwrap_or_default();
        // Disconnect clears transport admission. Reinstall only after the complete
        // authenticated resume gate succeeds, before publishing Connected.
        if let Some(transport) = inner.transport.as_ref() {
            if transport
                .install_authenticated_lease(super::relay_transport::AuthenticatedLease {
                    context: lease.context.clone(),
                    negotiated_capabilities: lease.negotiated_capabilities.clone(),
                })
                .is_err()
            {
                clear_live_context(&mut inner);
                inner.phase = ControllingStatusKind::Error;
                inner.error_code = Some(RESUME_FAILED_ERROR);
                return Err(ControllingStateError::new("controller_resume_failed"));
            }
        }
        inner.resume_gate = Some(ControllerResumeGate::new(
            ControllerRole::ControllerA,
            lease.context.clone(),
            resume_token,
            transport_key_fingerprint,
            new_deadline_b,
        ));
        if let Some(lease) = inner.lease.as_mut() {
            lease.liveness_epoch_a = liveness_epoch_a;
            lease.resume_deadline_b = new_deadline_b;
        }
        inner.phase = ControllingStatusKind::Connected;
        Ok(status_locked(&inner))
    }

    /// E2E-06：断线后取恢复材料并让本地 gate 进入 `Prepared`。相位不符、没有活
    /// 租约或 gate 已终止时失败关闭（并把连接投影为有界错误的终态）。
    pub(crate) fn begin_resume(
        &self,
        operation: ControllerOperation,
        now_b: u64,
    ) -> Result<ControllingResumeMaterial> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControllingStatusKind::Reconnecting {
            return Err(ControllingStateError::new("controller_phase_invalid"));
        }
        let lease = inner
            .lease
            .clone()
            .ok_or_else(|| ControllingStateError::new("lease_unavailable"))?;
        let prepared = match inner.resume_gate.as_mut() {
            Some(gate) => gate.prepare(now_b, None).is_ok(),
            None => false,
        };
        if !prepared {
            clear_live_context(&mut inner);
            inner.phase = ControllingStatusKind::Error;
            inner.error_code = Some(RESUME_FAILED_ERROR);
            return Err(ControllingStateError::new("controller_resume_failed"));
        }
        Ok(ControllingResumeMaterial {
            lease: lease.context,
            resume_token: lease.resume_token,
            relay_deadline_identity_ms: lease.resume_deadline_b,
        })
    }

    /// E2E-06：按帧顺序推进本地恢复相位；任一步失败即终止候选并投影有界错误。
    pub(crate) fn advance_resume(
        &self,
        operation: ControllerOperation,
        step: ResumeStep<'_>,
        now_b: u64,
    ) -> Result<ControllingStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControllingStatusKind::Reconnecting {
            return Err(ControllingStateError::new("controller_phase_invalid"));
        }
        let deadline = None;
        let advanced = match inner.resume_gate.as_mut() {
            Some(gate) => match step {
                ResumeStep::Provisional { activation_id } => gate
                    .accept_provisional(activation_id, now_b, deadline)
                    .is_ok(),
                ResumeStep::Confirmed { activation_id } => {
                    gate.confirm(activation_id, now_b, deadline).is_ok()
                }
                ResumeStep::ActivationPending { activation_id } => gate
                    .accept_activation_pending(activation_id, now_b, deadline)
                    .is_ok(),
                ResumeStep::LivenessConfirmed { activation_id } => gate
                    .accept_post_activation_liveness_confirmed(activation_id, now_b, deadline)
                    .is_ok(),
                ResumeStep::ReadyAcknowledged { activation_id } => gate
                    .acknowledge_ready(activation_id, now_b, deadline)
                    .is_ok(),
            },
            None => false,
        };
        if !advanced {
            clear_live_context(&mut inner);
            inner.phase = ControllingStatusKind::Error;
            inner.error_code = Some(RESUME_FAILED_ERROR);
            return Err(ControllingStateError::new("controller_resume_failed"));
        }
        Ok(status_locked(&inner))
    }

    /// E2E-06：当前连接尝试的 operation（恢复流程在 service 层按帧推进时需要它）。
    pub(crate) fn current_operation(&self) -> Option<ControllerOperation> {
        let inner = self.inner.lock();
        inner
            .claim_request_id
            .as_ref()
            .map(|request| ControllerOperation {
                intent: inner.intent,
                claim_request_id: request.clone(),
            })
    }

    /// E2E-06：发送恢复族的敏感控制帧（prepare/confirm/ready_ack）。
    pub(crate) fn send_resume_control(
        &self,
        frame: &serde_json::Value,
    ) -> std::result::Result<(), ControllerSendError> {
        let inner = self.inner.lock();
        let transport = inner
            .transport
            .as_ref()
            .ok_or(ControllerSendError::Unauthenticated)?;
        transport.try_send_sensitive_resume_control(frame)
    }

    pub(crate) fn fail_connection(
        &self,
        operation: ControllerOperation,
        public_error_code: &'static str,
    ) -> Result<ControllingStatus> {
        let code = normalize_public_error(public_error_code)?;
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        let retired = inner.lease.as_ref().map(|lease| lease.context.clone());
        clear_live_context(&mut inner);
        inner.phase = ControllingStatusKind::Error;
        inner.error_code = Some(code);
        Ok(terminal_status_with_retired_identity(&inner, retired))
    }

    pub(crate) fn disconnect(&self) -> ControllingStatus {
        let mut inner = self.inner.lock();
        let retired = inner.lease.as_ref().map(|lease| lease.context.clone());
        clear_live_context(&mut inner);
        inner.phase = ControllingStatusKind::Disconnected;
        inner.error_code = None;
        terminal_status_with_retired_identity(&inner, retired)
    }

    pub(crate) fn live_lease(&self) -> Result<LeaseContext> {
        let inner = self.inner.lock();
        if inner.phase != ControllingStatusKind::Connected {
            return Err(ControllingStateError::new("controller_not_connected"));
        }
        inner
            .lease
            .as_ref()
            .map(|lease| lease.context.clone())
            .ok_or_else(|| ControllingStateError::new("controller_not_connected"))
    }

    /// Authenticated lease identity from the signed handshake. A caller must also check
    /// the encrypted lane before opening a RemoteProxy; this getter alone is not admission.
    pub(crate) fn authenticated_remote_proxy_identity(
        &self,
    ) -> Result<(LeaseContext, String, String)> {
        let inner = self.inner.lock();
        if inner.phase != ControllingStatusKind::Connected {
            return Err(ControllingStateError::new("controller_not_connected"));
        }
        let lease = inner
            .lease
            .as_ref()
            .ok_or_else(|| ControllingStateError::new("controller_not_connected"))?;
        if lease.peer_endpoint_id.is_empty() || lease.reservation_id.is_empty() {
            return Err(ControllingStateError::new(
                "controller_identity_unavailable",
            ));
        }
        Ok((
            lease.context.clone(),
            lease.peer_endpoint_id.clone(),
            lease.reservation_id.clone(),
        ))
    }

    /// The final RemoteProxy commit is guarded by the same live lease and encrypted
    /// channel ownership. Disconnect, generation replacement, or channel suspension
    /// cannot interleave between this check and the caller's durable commit.
    pub(crate) fn with_current_authenticated_remote_proxy_identity<T>(
        &self,
        lane: &super::encrypted_lane::ControllingLaneState,
        expected: &(LeaseContext, String, String),
        commit: impl FnOnce() -> std::result::Result<T, String>,
    ) -> std::result::Result<T, String> {
        let inner = self.inner.lock();
        let lease = inner
            .lease
            .as_ref()
            .ok_or_else(|| "remote_proxy_channel_unavailable".to_owned())?;
        if inner.phase != ControllingStatusKind::Connected
            || lease.context != expected.0
            || lease.peer_endpoint_id != expected.1
            || lease.reservation_id != expected.2
        {
            return Err("remote_proxy_channel_changed".into());
        }
        lane.with_active(&lease.context, commit)
    }

    /// 本次连接尝试是否仍然拥有待决状态（connecting / safety_handshake）。
    /// 在途 worker 的失败收尾据此判断「该不该把状态投影为 error」：用户已经
    /// 显式断开、或尝试已被新 intent 取代时不得覆盖既有状态。
    pub(crate) fn is_connecting(&self, operation: ControllerOperation) -> bool {
        let inner = self.inner.lock();
        ensure_operation(&inner, &operation).is_ok()
            && matches!(
                inner.phase,
                ControllingStatusKind::Connecting | ControllingStatusKind::SafetyHandshake
            )
    }

    /// 活租约上下文 + 协商能力：A 端构造任何 payload 都以此为唯一准入依据。
    pub(crate) fn live_runtime_context(&self) -> Result<(LeaseContext, Vec<String>)> {
        let inner = self.inner.lock();
        if inner.phase != ControllingStatusKind::Connected {
            return Err(ControllingStateError::new("controller_not_connected"));
        }
        let lease = inner
            .lease
            .as_ref()
            .ok_or_else(|| ControllingStateError::new("controller_not_connected"))?;
        Ok((lease.context.clone(), lease.negotiated_capabilities.clone()))
    }

    /// 发送一条已认证明文载荷对应的密文帧（E2E-04）。租约未安装或 transport
    /// 已停止时失败关闭，绝不退回未加密通道。
    pub(crate) fn send_authenticated_business(
        &self,
        frame: &serde_json::Value,
    ) -> std::result::Result<(), ControllerSendError> {
        let inner = self.inner.lock();
        let transport = inner
            .transport
            .as_ref()
            .ok_or(ControllerSendError::Unauthenticated)?;
        transport.try_send_business(frame)
    }

    /// The A task/cancel path may enqueue only on the exact authenticated
    /// lease for which it sealed the frame. Identity comparison and enqueue
    /// share the manager lock, so disconnect/rebind cannot interleave there.
    pub(crate) fn send_authenticated_business_for_identity(
        &self,
        expected: &(LeaseContext, String, String),
        frame: &serde_json::Value,
    ) -> std::result::Result<(), ControllerSendError> {
        let inner = self.inner.lock();
        let lease = inner
            .lease
            .as_ref()
            .ok_or(ControllerSendError::Unauthenticated)?;
        if inner.phase != ControllingStatusKind::Connected
            || lease.context != expected.0
            || lease.peer_endpoint_id != expected.1
            || lease.reservation_id != expected.2
        {
            return Err(ControllerSendError::Unauthenticated);
        }
        let transport = inner
            .transport
            .as_ref()
            .ok_or(ControllerSendError::Unauthenticated)?;
        transport.try_send_business(frame)
    }

    /// 发送一条租约内的控制帧（E2E-06 心跳等，明文但绑定当前租约）。
    pub(crate) fn send_authenticated_control(
        &self,
        frame: &serde_json::Value,
    ) -> std::result::Result<(), ControllerSendError> {
        let inner = self.inner.lock();
        let transport = inner
            .transport
            .as_ref()
            .ok_or(ControllerSendError::Unauthenticated)?;
        transport.try_send_authenticated_control(frame)
    }

    /// 当前租约的 A 端 liveness epoch（心跳帧的 epoch 字段）。
    pub(crate) fn live_liveness_epoch(&self) -> Option<String> {
        let inner = self.inner.lock();
        if inner.phase != ControllingStatusKind::Connected {
            return None;
        }
        inner
            .lease
            .as_ref()
            .map(|lease| lease.liveness_epoch_a.clone())
    }

    /// E2E-06：尽力发送显式断开帧（失败不影响本地断开，本地状态始终权威）。
    pub(crate) fn send_disconnect(&self, reason: &str) -> bool {
        self.send_disconnect_with_receipt(reason).is_some()
    }

    pub(crate) fn send_disconnect_with_receipt(
        &self,
        reason: &str,
    ) -> Option<tokio::sync::oneshot::Receiver<()>> {
        let inner = self.inner.lock();
        if inner.phase != ControllingStatusKind::Connected {
            return None;
        }
        let Some(lease) = inner.lease.as_ref() else {
            return None;
        };
        let request_id = format!("request-disconnect-{}", Uuid::new_v4().simple());
        let Ok(frame) = build_controller_disconnect(&lease.context, &request_id, reason) else {
            return None;
        };
        let Some(transport) = inner.transport.as_ref() else {
            return None;
        };
        transport.send_control_with_receipt(&frame).ok()
    }

    pub(crate) fn status(&self) -> ControllingStatus {
        status_locked(&self.inner.lock())
    }
}

impl Drop for ControllingDeviceManager {
    fn drop(&mut self) {
        let mut inner = self.inner.lock();
        clear_live_context(&mut inner);
    }
}

/// E2E-06：A 端显式断开帧（`controller_disconnect`）。reason 只允许有界标识符，
/// 不携带任何用户文本；Relay 收到后会终结当前租约并通知 B。
pub(crate) fn build_controller_disconnect(
    lease: &LeaseContext,
    request_id: &str,
    reason: &str,
) -> Result<serde_json::Value> {
    let bounded: String = reason
        .chars()
        .filter(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-'))
        .take(64)
        .collect();
    if bounded.is_empty() || request_id.is_empty() || request_id.len() > 128 {
        return Err(ControllingStateError::new("controller_disconnect_invalid"));
    }
    Ok(serde_json::json!({
        "v": 1,
        "type": "controller_disconnect",
        "request_id": request_id,
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "sender_role": "A",
        "reason": bounded,
    }))
}

pub(crate) fn normalize_pairing_code(raw: &str) -> Result<String> {
    let mut code = String::with_capacity(7);
    for character in raw.chars() {
        match character {
            ' ' | '-' | '\t' | '\r' | '\n' => continue,
            'a'..='z' => code.push(character.to_ascii_uppercase()),
            _ => code.push(character),
        }
    }
    if code.len() != 7
        || !code.bytes().all(|byte| {
            (byte.is_ascii_digit() && !matches!(byte, b'0' | b'1'))
                || matches!(byte, b'A'..=b'H' | b'J'..=b'N' | b'P'..=b'Z')
        })
    {
        return Err(ControllingStateError::new("pairing_code_invalid"));
    }
    Ok(code)
}

fn normalize_device_name(raw: &str) -> Result<String> {
    let name = raw.trim();
    if name.is_empty() || name.chars().count() > MAX_DEVICE_NAME_CHARS {
        return Err(ControllingStateError::new("device_name_invalid"));
    }
    Ok(name.to_owned())
}

fn normalize_public_error(code: &'static str) -> Result<&'static str> {
    match code {
        "pairing_code_invalid"
        | "controller_busy"
        | "endpoint_offline"
        | "version_incompatible"
        // Relay 侧的兜底公共错误：握手/协议层失败统一收敛到这里，不新增
        // 用户可见的错误码（见 CONTROLLER_ERROR_CODES）。
        | "invalid_message" => Ok(code),
        _ => Err(ControllingStateError::new("controller_error_invalid")),
    }
}

fn ensure_operation(inner: &Inner, operation: &ControllerOperation) -> Result<()> {
    if inner.intent != operation.intent
        || inner.claim_request_id.as_deref() != Some(operation.claim_request_id.as_str())
    {
        return Err(ControllingStateError::new("controller_stale_result"));
    }
    Ok(())
}

fn clear_live_context(inner: &mut Inner) {
    if let Some(lease) = inner.lease.as_ref() {
        inner.retired_lease = Some(lease.context.clone());
    }
    if let Some(transport) = inner.transport.take() {
        transport.stop();
    }
    inner.pending_code = None;
    inner.lease = None;
    if let Some(gate) = inner.resume_gate.as_mut() {
        let _ = gate.terminate("controller_cleanup");
    }
    inner.resume_gate = None;
}

fn status_locked(inner: &Inner) -> ControllingStatus {
    ControllingStatus {
        status: inner.phase,
        claim_request_id: inner.claim_request_id.clone(),
        has_pending_code: inner.pending_code.is_some(),
        device_display_name: inner
            .lease
            .as_ref()
            .map(|lease| lease.device_display_name.clone()),
        lease: inner
            .lease
            .as_ref()
            .filter(|_| {
                matches!(
                    inner.phase,
                    ControllingStatusKind::Connected | ControllingStatusKind::Reconnecting
                )
            })
            .map(|lease| lease.context.clone().into())
            .or_else(|| {
                matches!(
                    inner.phase,
                    ControllingStatusKind::Disconnected | ControllingStatusKind::Error
                )
                .then(|| inner.retired_lease.clone().map(Into::into))
                .flatten()
            }),
        error_code: inner.error_code,
    }
}

/// Terminal status may retain the retired identity for late-event filtering;
/// live-lease/runtime admission reads only the cleared active lease.
fn terminal_status_with_retired_identity(
    inner: &Inner,
    retired: Option<LeaseContext>,
) -> ControllingStatus {
    let mut status = status_locked(inner);
    if matches!(
        status.status,
        ControllingStatusKind::Disconnected | ControllingStatusKind::Error
    ) {
        status.lease = retired
            .or_else(|| inner.retired_lease.clone())
            .map(Into::into);
    }
    status
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn identity_bound_business_send_rejects_rebind_before_transport_enqueue() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager.complete_connected(operation, lease()).unwrap();
        let (sender, _receiver) = super::super::relay_transport::transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            2,
            2,
            8192,
            1024,
        );
        sender
            .install_authenticated_lease(super::super::relay_transport::AuthenticatedLease {
                context: lease().context,
                negotiated_capabilities: vec!["encrypted_frames_v1".into(), "task_v1".into()],
            })
            .unwrap();
        manager.replace_transport(sender);
        let identity = manager.authenticated_remote_proxy_identity().unwrap();
        let mut wrong_peer = identity.clone();
        wrong_peer.1 = "different-peer".into();
        assert_eq!(
            manager.send_authenticated_business_for_identity(&wrong_peer, &serde_json::json!({})),
            Err(ControllerSendError::Unauthenticated),
        );
        let mut wrong_lease = identity.clone();
        wrong_lease.0.lease_epoch = "different-epoch".into();
        assert_eq!(
            manager.send_authenticated_business_for_identity(&wrong_lease, &serde_json::json!({})),
            Err(ControllerSendError::Unauthenticated),
        );
        let mut wrong_boundary = identity.clone();
        wrong_boundary.2 = "different-boundary".into();
        assert_eq!(
            manager
                .send_authenticated_business_for_identity(&wrong_boundary, &serde_json::json!({})),
            Err(ControllerSendError::Unauthenticated),
        );
        assert!(matches!(
            manager.send_authenticated_business_for_identity(&identity, &serde_json::json!({})),
            Err(ControllerSendError::InvalidFrame(_)),
        ));
        manager.disconnect();
        assert_eq!(
            manager.send_authenticated_business_for_identity(&identity, &serde_json::json!({})),
            Err(ControllerSendError::Unauthenticated),
        );
    }

    #[test]
    fn remote_proxy_commit_requires_current_manager_and_active_exact_lane() {
        use super::super::crypto::ControllerRole;
        use super::super::encrypted_lane::ControllingLaneState;
        use crate::features::remote_control::controller::test_support::{
            lease_context, paired_session_keys, pairing_fixture,
        };

        let fixture = pairing_fixture("relay-key-projection-guard");
        let context = lease_context(&fixture.reservation);
        let (a_keys, _) = paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("2345678").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation,
                ControllerLease {
                    context: context.clone(),
                    ..lease()
                },
            )
            .unwrap();
        let expected = manager.authenticated_remote_proxy_identity().unwrap();
        let lane = ControllingLaneState::default();
        let calls = std::cell::Cell::new(0);
        let commit = || {
            calls.set(calls.get() + 1);
            Ok(())
        };
        assert!(
            manager
                .with_current_authenticated_remote_proxy_identity(&lane, &expected, commit)
                .is_err()
        );
        assert_eq!(calls.get(), 0);
        let mut wrong = context.clone();
        wrong.controller_lease_id = "foreign-lease".into();
        lane.install(ControllerRole::A, &wrong, a_keys).unwrap();
        assert!(
            manager
                .with_current_authenticated_remote_proxy_identity(&lane, &expected, commit)
                .is_err()
        );
        assert_eq!(calls.get(), 0);
        let fixture = pairing_fixture("relay-key-projection-guard-active");
        let context = lease_context(&fixture.reservation);
        let (a_keys, _) = paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        manager.disconnect();
        let operation = manager.connect("2345678").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation,
                ControllerLease {
                    context: context.clone(),
                    ..lease()
                },
            )
            .unwrap();
        let expected = manager.authenticated_remote_proxy_identity().unwrap();
        lane.install(ControllerRole::A, &context, a_keys).unwrap();
        lane.suspend();
        assert!(
            manager
                .with_current_authenticated_remote_proxy_identity(&lane, &expected, commit)
                .is_err()
        );
        assert_eq!(calls.get(), 0);
        lane.activate();
        manager
            .with_current_authenticated_remote_proxy_identity(&lane, &expected, commit)
            .unwrap();
        assert_eq!(calls.get(), 1);
        let mut replaced = expected.clone();
        replaced.1 = "foreign-endpoint".into();
        assert!(
            manager
                .with_current_authenticated_remote_proxy_identity(&lane, &replaced, commit)
                .is_err()
        );
        assert_eq!(calls.get(), 1);
        manager.disconnect();
        assert!(
            manager
                .with_current_authenticated_remote_proxy_identity(&lane, &expected, commit)
                .is_err()
        );
        assert_eq!(calls.get(), 1);
    }

    #[test]
    fn serialized_status_matches_the_shared_bridge_fixture() {
        let fixtures: serde_json::Value = serde_json::from_str(include_str!(
            "../../../../../tests/fixtures/desktop_remote_status.json"
        ))
        .unwrap();
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("2345678").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        let mut connected = manager
            .complete_connected(operation.clone(), lease())
            .unwrap();
        connected.claim_request_id = Some("claim-0001".into());
        assert_eq!(
            serde_json::to_value(&connected).unwrap(),
            fixtures["controllerConnected"]
        );
        let mut failed = manager
            .fail_connection(operation, "endpoint_offline")
            .unwrap();
        failed.claim_request_id = Some("claim-0001".into());
        assert_eq!(
            serde_json::to_value(failed).unwrap(),
            fixtures["controllerError"]
        );
    }

    fn lease() -> ControllerLease {
        ControllerLease {
            context: LeaseContext {
                connection_id: "connection-0001".into(),
                controller_lease_id: "lease-0001".into(),
                lease_epoch: "1".into(),
                relay_epoch: "relay-epoch-0001".into(),
                remote_control_generation: "generation-0001".into(),
            },
            peer_endpoint_id: "endpoint-0001".into(),
            reservation_id: "reservation-0001".into(),
            device_display_name: " Studio PC ".into(),
            liveness_epoch_a: "live-a-1".into(),
            resume_deadline_b: 30_000,
            resume_token: [1; 32],
            transport_key_fingerprint: [2; 32],
            negotiated_capabilities: vec![
                "encrypted_frames_v1".into(),
                "pairing_v1".into(),
                "task_v1".into(),
            ],
        }
    }

    #[test]
    fn pairing_code_normalization_and_invalid_characters_fail_closed() {
        assert_eq!(normalize_pairing_code(" 234-567 8 \t").unwrap(), "2345678");
        assert_eq!(normalize_pairing_code("abc-defg").unwrap(), "ABCDEFG");
        for code in [
            "0123456", "1234567", "234567", "234567I", "234567O", "234/567",
        ] {
            assert_eq!(
                normalize_pairing_code(code).unwrap_err().code,
                "pairing_code_invalid",
                "{code}"
            );
        }
    }

    #[test]
    fn connection_result_is_generation_and_intent_gated() {
        let manager = ControllingDeviceManager::default();
        let first = manager.connect("234-4567").unwrap();
        assert_eq!(manager.pending_code(first.clone()).unwrap(), "2344567");
        let second = manager.connect("345-6789").unwrap();
        assert_eq!(
            manager.complete_reserved(first.clone()).unwrap_err().code,
            "controller_stale_result"
        );
        assert_eq!(
            manager.complete_reserved(second.clone()).unwrap().status,
            ControllingStatusKind::SafetyHandshake
        );
        let connected = manager.complete_connected(second, lease()).unwrap();
        assert_eq!(connected.status, ControllingStatusKind::Connected);
        assert_eq!(connected.device_display_name.as_deref(), Some("Studio PC"));
        assert_eq!(manager.live_lease().unwrap(), lease().context);
    }

    #[test]
    fn public_connection_errors_are_bounded_and_old_code_is_not_retried() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        assert_eq!(
            manager
                .fail_connection(operation.clone(), "pairing_code_not_found")
                .unwrap_err()
                .code,
            "controller_error_invalid"
        );
        // E2E-02：协议层失败收敛到的 Relay 兜底码必须可用，且失败路径必须
        // 清空连接码与租约，不能停留在 connecting。
        let status = manager
            .fail_connection(operation.clone(), "invalid_message")
            .unwrap();
        assert_eq!(status.status, ControllingStatusKind::Error);
        assert_eq!(status.error_code, Some("invalid_message"));
        assert!(!status.has_pending_code);
        assert_eq!(status.lease, None);
        assert_eq!(
            manager.pending_code(operation).unwrap_err().code,
            "pairing_code_unavailable"
        );

        let operation = manager.connect("345-6789").unwrap();
        let status = manager
            .fail_connection(operation, "version_incompatible")
            .unwrap();
        assert_eq!(status.status, ControllingStatusKind::Error);
        assert_eq!(status.error_code, Some("version_incompatible"));
        assert_eq!(status.has_pending_code, false);
    }

    #[test]
    fn terminal_error_retains_identity_but_never_live_authority() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(operation.clone(), lease())
            .unwrap();
        let failed = manager
            .fail_connection(operation, "invalid_message")
            .unwrap();
        assert_eq!(failed.status, ControllingStatusKind::Error);
        assert_eq!(failed.lease, Some(lease().context.into()));
        assert_eq!(manager.status().lease, failed.lease);
        assert!(manager.live_lease().is_err());
        assert!(manager.live_runtime_context().is_err());
        manager.connect("234-4567").unwrap();
        assert_eq!(manager.status().lease, None);
    }

    #[test]
    fn transport_disconnect_and_resume_do_not_expose_the_old_token() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(operation.clone(), lease())
            .unwrap();
        let reconnecting = manager.mark_transport_disconnected(5_000, Some(30_000));
        assert_eq!(reconnecting.status, ControllingStatusKind::Reconnecting);
        assert_eq!(reconnecting.lease, Some(lease().context.into()));
        // 恢复材料可取（旧令牌只在内存里），Debug 输出不得含令牌字节。
        let material = manager.begin_resume(operation.clone(), 1_000).unwrap();
        assert_eq!(material.relay_deadline_identity_ms, 30_000);
        assert!(format!("{material:?}").contains("[REDACTED]"));
        let activation = "activation-0001";
        manager
            .advance_resume(
                operation.clone(),
                ResumeStep::Provisional {
                    activation_id: activation,
                },
                2_000,
            )
            .unwrap();
        // 相位顺序错乱必须终止候选（此处先确认，再重复 provisional）。
        manager
            .advance_resume(
                operation.clone(),
                ResumeStep::Confirmed {
                    activation_id: activation,
                },
                3_000,
            )
            .unwrap();
        assert_eq!(
            manager
                .advance_resume(
                    operation.clone(),
                    ResumeStep::Provisional {
                        activation_id: activation,
                    },
                    4_000,
                )
                .unwrap_err()
                .code,
            "controller_resume_failed"
        );
        assert_eq!(
            manager.status().error_code,
            Some("endpoint_offline"),
            "恢复失败必须投影有界公共错误"
        );
        assert!(manager.live_lease().is_err());
        assert_eq!(manager.status().lease, Some(lease().context.into()));
        let outer_failed = manager
            .fail_connection(operation.clone(), "endpoint_offline")
            .unwrap();
        assert_eq!(outer_failed.lease, Some(lease().context.into()));

        // 重新开始一次完整恢复：相位全序列后进入 connected。
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(operation.clone(), lease())
            .unwrap();
        let (sender, _receiver) = super::super::relay_transport::transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            2,
            2,
            8192,
            1024,
        );
        sender
            .install_authenticated_lease(super::super::relay_transport::AuthenticatedLease {
                context: lease().context,
                negotiated_capabilities: lease().negotiated_capabilities,
            })
            .unwrap();
        manager.replace_transport(sender.clone());
        // The real disconnect callback invalidates sender admission.
        sender.clear_authenticated_lease();
        manager.mark_transport_disconnected(5_000, Some(30_000));
        manager.begin_resume(operation.clone(), 1_000).unwrap();
        for (step, now_b) in [
            (
                ResumeStep::Provisional {
                    activation_id: activation,
                },
                2_000,
            ),
            (
                ResumeStep::Confirmed {
                    activation_id: activation,
                },
                3_000,
            ),
            (
                ResumeStep::ActivationPending {
                    activation_id: activation,
                },
                4_000,
            ),
            (
                ResumeStep::LivenessConfirmed {
                    activation_id: activation,
                },
                5_000,
            ),
            (
                ResumeStep::ReadyAcknowledged {
                    activation_id: activation,
                },
                6_000,
            ),
        ] {
            manager
                .advance_resume(operation.clone(), step, now_b)
                .unwrap();
            assert!(
                sender.authenticated_lease().is_none(),
                "intermediate resume steps cannot admit business"
            );
        }
        let resumed = manager
            .complete_resumed(
                operation, activation, 7_000, "live:a-2", [3; 32], [4; 32], 30_000,
            )
            .unwrap();
        assert_eq!(
            sender.authenticated_lease().unwrap().context,
            lease().context
        );
        let frame = serde_json::json!({
            "v": 1, "type": "encrypted_frame", "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001", "lease_epoch": "1",
            "relay_epoch": "relay-epoch-0001", "remote_control_generation": "generation-0001",
            "frame_sequence": "1", "direction": "a_to_b", "payload_kind": "task_submit",
            "ciphertext_b64": "AQID", "ciphertext_bytes": "3"
        });
        sender
            .try_send_business(&frame)
            .expect("complete resumed restores actual transport admission");
        sender.try_send_business(&frame).unwrap();
        assert_eq!(
            sender.try_send_business(&frame),
            Err(ControllerSendError::QueueFull(
                super::super::relay_transport::FramePriority::Business,
            )),
            "two real queued frames exhaust the bounded business queue"
        );
        assert_eq!(resumed.status, ControllingStatusKind::Connected);
        assert_eq!(
            manager.live_lease().unwrap().controller_lease_id,
            "lease-0001"
        );
    }

    #[test]
    fn invalid_resumed_cycle_metadata_fails_closed_before_installation() {
        for (deadline, epoch, generation) in [
            (0, "live-a-2", "generation-0001"),
            (60_000, "", "generation-0001"),
            (60_000, "invalid epoch", "generation-0001"),
            (60_000, "live~a-2", "generation-0001"),
            (60_000, "live-a-2", "generation-replaced"),
        ] {
            let manager = ControllingDeviceManager::default();
            let operation = manager.connect("234-4567").unwrap();
            manager.complete_reserved(operation.clone()).unwrap();
            manager
                .complete_connected(operation.clone(), lease())
                .unwrap();
            let (sender, _receiver) = super::super::relay_transport::transport_channels(
                ControllerRole::ControllerA,
                generation,
                2,
                2,
                8192,
                1024,
            );
            manager.replace_transport(sender.clone());
            manager.mark_transport_disconnected(5_000, Some(30_000));
            manager.begin_resume(operation.clone(), 6_000).unwrap();
            let activation = "activation-0001";
            for (step, now_b) in [
                (
                    ResumeStep::Provisional {
                        activation_id: activation,
                    },
                    7_000,
                ),
                (
                    ResumeStep::Confirmed {
                        activation_id: activation,
                    },
                    8_000,
                ),
                (
                    ResumeStep::ActivationPending {
                        activation_id: activation,
                    },
                    9_000,
                ),
                (
                    ResumeStep::LivenessConfirmed {
                        activation_id: activation,
                    },
                    10_000,
                ),
                (
                    ResumeStep::ReadyAcknowledged {
                        activation_id: activation,
                    },
                    11_000,
                ),
            ] {
                manager
                    .advance_resume(operation.clone(), step, now_b)
                    .unwrap();
            }
            assert_eq!(
                manager
                    .complete_resumed(
                        operation, activation, 12_000, epoch, [3; 32], [4; 32], deadline
                    )
                    .unwrap_err()
                    .code,
                "controller_resume_failed"
            );
            assert_eq!(manager.status().status, ControllingStatusKind::Error);
            assert!(manager.live_lease().is_err());
            let inner = manager.inner.lock();
            assert!(inner.lease.is_none());
            assert!(inner.resume_gate.is_none());
            assert!(sender.authenticated_lease().is_none());
            assert!(
                sender.is_stopped(),
                "failed installation stops the current transport owner"
            );
        }
    }

    #[test]
    fn process_restart_uses_an_empty_manager_and_no_stale_operations() {
        let old = ControllingDeviceManager::default();
        let operation = old.connect("234-4567").unwrap();
        old.complete_reserved(operation).unwrap();
        drop(old);
        let manager = ControllingDeviceManager::default();
        assert_eq!(manager.status().status, ControllingStatusKind::Idle);
        assert_eq!(
            manager
                .complete_reserved(ControllerOperation {
                    intent: 1,
                    claim_request_id: "claim_old".into(),
                })
                .unwrap_err()
                .code,
            "controller_stale_result"
        );
    }

    /// E2E-06：显式断开帧必须通过冻结校验（角色 A + encrypted_frames_v1），
    /// 且只在 connected 且有 transport 时尽力发送。
    #[test]
    fn explicit_disconnect_frame_is_frozen_and_lease_bound() {
        use super::super::protocol::{
            RuntimeValidationContext, SenderRole, validate_controller_message_json,
            validate_controller_runtime_message_json,
        };

        let fixture = lease();
        let frame = build_controller_disconnect(
            &fixture.context,
            "request-disconnect-0001",
            "user_disconnect",
        )
        .unwrap();
        validate_controller_message_json(&frame.to_string(), SenderRole::ControllerA).unwrap();
        validate_controller_runtime_message_json(
            &frame.to_string(),
            SenderRole::ControllerA,
            Some(&RuntimeValidationContext {
                lease: Some(fixture.context.clone()),
                negotiated_capabilities: vec![
                    "encrypted_frames_v1".to_owned(),
                    "pairing_v1".to_owned(),
                    "task_v1".to_owned(),
                ],
            }),
        )
        .expect("断开帧必须通过运行时校验");
        assert_eq!(frame["sender_role"], "A");
        assert_eq!(frame["reason"], "user_disconnect");
        // reason 只保留有界标识符字符：全部被过滤时失败关闭，否则截断到有界长度。
        assert!(build_controller_disconnect(&fixture.context, "request-1", " 用户 断开 ").is_err());
        let sanitized =
            build_controller_disconnect(&fixture.context, "request-1", "user disconnect!").unwrap();
        assert_eq!(sanitized["reason"], "userdisconnect");

        // 未连接时没有可发送的断开帧。
        let manager = ControllingDeviceManager::default();
        assert!(!manager.send_disconnect("user_disconnect"));
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager.complete_connected(operation, fixture).unwrap();
        // 测试用 transport_channels 不启动网络任务，入队即视为送出成功。
        let (sender, _receiver) = super::super::relay_transport::transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            2,
            2,
            8192,
            1024,
        );
        // 断开帧是租约控制帧：transport 必须先装上同一租约。
        sender
            .install_authenticated_lease(super::super::relay_transport::AuthenticatedLease {
                context: lease().context,
                negotiated_capabilities: vec![
                    "encrypted_frames_v1".into(),
                    "pairing_v1".into(),
                    "task_v1".into(),
                ],
            })
            .unwrap();
        manager.replace_transport(sender);
        assert!(manager.send_disconnect("user_disconnect"));
        // 断开后本地状态立即权威：不再声称 connected。
        assert_eq!(
            manager.disconnect().status,
            ControllingStatusKind::Disconnected
        );
        assert!(!manager.send_disconnect("user_disconnect"));
    }

    /// E2E-02：在途 worker 只能在自己的尝试仍然待决时收尾为 error。
    #[test]
    fn pending_attempt_is_the_only_owner_of_failure_projection() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        assert!(manager.is_connecting(operation.clone()));
        manager.complete_reserved(operation.clone()).unwrap();
        assert!(manager.is_connecting(operation.clone()));
        manager
            .complete_connected(operation.clone(), lease())
            .unwrap();
        assert!(!manager.is_connecting(operation.clone()));
        assert_eq!(
            manager
                .complete_reserved(operation.clone())
                .unwrap_err()
                .code,
            "controller_phase_invalid",
            "connected 之后不得再回到待决阶段"
        );

        // 已连接时必须先显式断开才能发起新的连接（connect 拒绝 connected 阶段）。
        manager.disconnect();
        let operation = manager.connect("345-6789").unwrap();
        assert!(manager.is_connecting(operation.clone()));
        manager.disconnect();
        assert!(
            !manager.is_connecting(operation),
            "用户已断开时在途失败不得覆盖状态"
        );
    }
}
