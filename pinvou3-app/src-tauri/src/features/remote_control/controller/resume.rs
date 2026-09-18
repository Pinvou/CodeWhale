//! Local endpoint state gate for the controller resume protocol.
//!
//! Relay owns the two-role activation transition. This type owns the local
//! role's one-shot token and transport-key fingerprint, refuses every business
//! gate until the final authenticated result, and never extends the deadline
//! installed by DEV-08.

use zeroize::{Zeroize, Zeroizing};

use base64::Engine as _;

use super::protocol::{LeaseContext, SenderRole, validate_controller_message_json};
use super::relay_transport::ControllerRole;

const RESUME_TOKEN_BYTES: usize = 32;
const FINGERPRINT_BYTES: usize = 32;

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum ResumePhase {
    Connected,
    Disconnected,
    Prepared,
    Provisional,
    Confirmed,
    ActivationPending,
    PostActivationLiveness,
    ReadyAcknowledged,
    Resumed,
    Terminated,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ResumeStateError {
    pub(crate) code: &'static str,
}

impl ResumeStateError {
    const fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl std::fmt::Display for ResumeStateError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for ResumeStateError {}

type Result<T> = std::result::Result<T, ResumeStateError>;

#[derive(Debug)]
struct ResumeMaterial {
    token: Zeroizing<[u8; RESUME_TOKEN_BYTES]>,
    key_fingerprint: [u8; FINGERPRINT_BYTES],
}

#[derive(Debug)]
pub(crate) struct ControllerResumeGate {
    role: ControllerRole,
    lease: LeaseContext,
    original_deadline_b: u64,
    phase: ResumePhase,
    activation_id: Option<String>,
    current: ResumeMaterial,
    retired: Option<ResumeMaterial>,
    resumed_liveness_epoch: Option<String>,
}

impl ControllerResumeGate {
    pub(crate) fn new(
        role: ControllerRole,
        lease: LeaseContext,
        resume_token: [u8; RESUME_TOKEN_BYTES],
        transport_key_fingerprint: [u8; FINGERPRINT_BYTES],
        original_deadline_b: u64,
    ) -> Self {
        Self {
            role,
            lease,
            original_deadline_b,
            phase: ResumePhase::Connected,
            activation_id: None,
            current: ResumeMaterial {
                token: Zeroizing::new(resume_token),
                key_fingerprint: transport_key_fingerprint,
            },
            retired: None,
            resumed_liveness_epoch: None,
        }
    }

    pub(crate) fn phase(&self) -> ResumePhase {
        self.phase
    }

    pub(crate) fn is_business_allowed(&self) -> bool {
        matches!(self.phase, ResumePhase::Connected | ResumePhase::Resumed)
    }

    pub(crate) fn original_deadline_b(&self) -> u64 {
        self.original_deadline_b
    }

    /// A transport interruption starts resume without changing B's local A
    /// watchdog deadline. The caller supplies that DEV-08 deadline on every
    /// transition so a stale gate cannot accidentally manufacture more budget.
    pub(crate) fn disconnect(
        &mut self,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        self.transition(ResumePhase::Disconnected, now_b, watchdog_deadline_b)
    }

    pub(crate) fn prepare(&mut self, now_b: u64, watchdog_deadline_b: Option<u64>) -> Result<()> {
        self.expect_phase(ResumePhase::Disconnected, now_b, watchdog_deadline_b)?;
        self.phase = ResumePhase::Prepared;
        Ok(())
    }

    pub(crate) fn accept_provisional(
        &mut self,
        activation_id: impl Into<String>,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        self.expect_phase(ResumePhase::Prepared, now_b, watchdog_deadline_b)?;
        self.activation_id = Some(activation_id.into());
        self.phase = ResumePhase::Provisional;
        Ok(())
    }

    pub(crate) fn confirm(
        &mut self,
        activation_id: &str,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        self.expect_phase(ResumePhase::Provisional, now_b, watchdog_deadline_b)?;
        self.require_activation(activation_id)?;
        self.phase = ResumePhase::Confirmed;
        Ok(())
    }

    pub(crate) fn accept_activation_pending(
        &mut self,
        activation_id: &str,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        self.expect_phase(ResumePhase::Confirmed, now_b, watchdog_deadline_b)?;
        self.require_activation(activation_id)?;
        self.phase = ResumePhase::ActivationPending;
        Ok(())
    }

    pub(crate) fn acknowledge_activation(
        &mut self,
        activation_id: &str,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        if self.role != ControllerRole::ControlledB {
            return self.terminate("controller_resume_role_invalid");
        }
        self.expect_phase(ResumePhase::Confirmed, now_b, watchdog_deadline_b)?;
        self.require_activation(activation_id)?;
        self.phase = ResumePhase::ActivationPending;
        Ok(())
    }

    pub(crate) fn send_post_activation_liveness(
        &mut self,
        activation_id: &str,
        epoch_b: impl Into<String>,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<String> {
        if self.role != ControllerRole::ControlledB {
            let _ = self.terminate("controller_resume_role_invalid");
            return Err(ResumeStateError::new("controller_resume_role_invalid"));
        }
        self.expect_phase(ResumePhase::ActivationPending, now_b, watchdog_deadline_b)?;
        self.require_activation(activation_id)?;
        let epoch = epoch_b.into();
        if epoch.is_empty() || epoch.len() > 128 {
            let _ = self.terminate("controller_resume_liveness_invalid");
            return Err(ResumeStateError::new("controller_resume_liveness_invalid"));
        }
        self.phase = ResumePhase::PostActivationLiveness;
        Ok(epoch)
    }

    pub(crate) fn acknowledge_ready(
        &mut self,
        activation_id: &str,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        self.expect_phase(
            ResumePhase::PostActivationLiveness,
            now_b,
            watchdog_deadline_b,
        )?;
        self.require_activation(activation_id)?;
        self.phase = ResumePhase::ReadyAcknowledged;
        Ok(())
    }

    /// E2E-06：A/B 都会收到 Relay 的 `controller_post_activation_liveness_confirmed`。
    /// A 由 ActivationPending 进入 PostActivationLiveness；B 已经处于该相位
    /// （它自己发出了 post-activation liveness），重复投递只做幂等确认，
    /// 不得回退或延长任何相位。其他相位一律失败关闭。
    pub(crate) fn accept_post_activation_liveness_confirmed(
        &mut self,
        activation_id: &str,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        match self.phase {
            ResumePhase::ActivationPending => {
                self.expect_phase(ResumePhase::ActivationPending, now_b, watchdog_deadline_b)?;
                self.require_activation(activation_id)?;
                self.phase = ResumePhase::PostActivationLiveness;
                Ok(())
            }
            ResumePhase::PostActivationLiveness => {
                self.expect_phase(
                    ResumePhase::PostActivationLiveness,
                    now_b,
                    watchdog_deadline_b,
                )?;
                self.require_activation(activation_id)?;
                Ok(())
            }
            _ => self.terminate("controller_resume_phase_invalid"),
        }
    }

    /// Consume the old role material only with a complete, role-addressed
    /// final result. The new token and transport fingerprint become the only
    /// accepted values afterward.
    pub(crate) fn accept_resumed(
        &mut self,
        activation_id: &str,
        new_liveness_epoch: impl Into<String>,
        new_resume_token: [u8; RESUME_TOKEN_BYTES],
        new_key_fingerprint: [u8; FINGERPRINT_BYTES],
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        self.expect_phase(ResumePhase::ReadyAcknowledged, now_b, watchdog_deadline_b)?;
        self.require_activation(activation_id)?;
        if new_resume_token[..] == self.current.token[..]
            || new_key_fingerprint == self.current.key_fingerprint
        {
            return self.terminate("controller_resume_material_reused");
        }
        let retired_token = *self.current.token;
        self.retired = Some(ResumeMaterial {
            token: Zeroizing::new(retired_token),
            key_fingerprint: self.current.key_fingerprint,
        });
        self.current = ResumeMaterial {
            token: Zeroizing::new(new_resume_token),
            key_fingerprint: new_key_fingerprint,
        };
        self.resumed_liveness_epoch = Some(new_liveness_epoch.into());
        self.phase = ResumePhase::Resumed;
        Ok(())
    }

    pub(crate) fn token_matches(&self, role: ControllerRole, token: &[u8]) -> Result<bool> {
        if self.phase == ResumePhase::Terminated
            || self.role != role
            || token.len() != RESUME_TOKEN_BYTES
        {
            return Ok(false);
        }
        Ok(constant_time_equal(token, &self.current.token[..]))
    }

    pub(crate) fn key_fingerprint_matches(
        &self,
        role: ControllerRole,
        fingerprint: &[u8],
    ) -> Result<bool> {
        if self.phase == ResumePhase::Terminated
            || self.role != role
            || fingerprint.len() != FINGERPRINT_BYTES
        {
            return Ok(false);
        }
        Ok(constant_time_equal(
            fingerprint,
            &self.current.key_fingerprint[..],
        ))
    }

    pub(crate) fn resumed_liveness_epoch(&self) -> Option<&str> {
        self.resumed_liveness_epoch.as_deref()
    }

    /// E2E-06：恢复准备帧必须按冻结协议把本端令牌交给 Relay（`resume_token_b64`）。
    /// 只以固定大小数组拷出，调用方用完即弃，绝不进入 Debug/序列化路径。
    pub(crate) fn current_token(&self) -> [u8; RESUME_TOKEN_BYTES] {
        *self.current.token
    }

    pub(crate) fn terminate(&mut self, code: &'static str) -> Result<()> {
        self.current.token.zeroize();
        self.current.key_fingerprint.zeroize();
        if let Some(retired) = self.retired.as_mut() {
            retired.token.zeroize();
            retired.key_fingerprint.zeroize();
        }
        self.phase = ResumePhase::Terminated;
        Err(ResumeStateError::new(code))
    }

    fn expect_phase(
        &mut self,
        expected: ResumePhase,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        if self.phase != expected || !self.deadline_is_current(now_b, watchdog_deadline_b) {
            return self.terminate("controller_resume_phase_invalid");
        }
        Ok(())
    }

    fn transition(
        &mut self,
        next: ResumePhase,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        if !matches!(self.phase, ResumePhase::Connected)
            || !self.deadline_is_current(now_b, watchdog_deadline_b)
        {
            return self.terminate("controller_resume_phase_invalid");
        }
        self.phase = next;
        Ok(())
    }

    fn deadline_is_current(&self, now_b: u64, watchdog_deadline_b: Option<u64>) -> bool {
        now_b < self.original_deadline_b
            && watchdog_deadline_b
                .is_none_or(|deadline| deadline <= self.original_deadline_b && now_b < deadline)
    }

    fn ensure_activation(&self, activation_id: &str) -> std::result::Result<(), ResumeStateError> {
        if self.activation_id.as_deref() != Some(activation_id) {
            return Err(ResumeStateError::new(
                "controller_resume_activation_invalid",
            ));
        }
        Ok(())
    }

    fn require_activation(&mut self, activation_id: &str) -> Result<()> {
        if let Err(error) = self.ensure_activation(activation_id) {
            return self.terminate(error.code);
        }
        Ok(())
    }
}

fn constant_time_equal(left: &[u8], right: &[u8]) -> bool {
    if left.len() != right.len() {
        return false;
    }
    let mut difference = 0_u8;
    for (a, b) in left.iter().zip(right) {
        difference |= a ^ b;
    }
    difference == 0
}

/// E2E-06：恢复期间本端传输挑战值（16 字节随机，base64）。
/// 它只用于让 Relay 把两个角色的候选绑成同一个 activation_id。
pub(crate) fn new_resume_challenge() -> Result<String> {
    use ring::rand::{SecureRandom as _, SystemRandom};

    let mut bytes = [0_u8; 16];
    SystemRandom::new()
        .fill(&mut bytes)
        .map_err(|_| ResumeStateError::new("controller_resume_entropy"))?;
    Ok(base64::engine::general_purpose::STANDARD.encode(bytes))
}

/// E2E-06：构造 `controller_resume_prepare`（冻结 schema：租约五元组 +
/// sender_role + resume_token_b64 + transport_challenge_b64 + original_deadline_ms）。
/// `original_deadline_ms` 使用 Relay 在 `controller_joined` 下发的预算，绝不放大。
pub(crate) fn build_controller_resume_prepare(
    lease: &LeaseContext,
    role: ControllerRole,
    request_id: &str,
    resume_token: &[u8; RESUME_TOKEN_BYTES],
    transport_challenge_b64: &str,
    original_deadline_ms: u64,
) -> Result<serde_json::Value> {
    if original_deadline_ms == 0 || transport_challenge_b64.is_empty() {
        return Err(ResumeStateError::new("controller_resume_material_invalid"));
    }
    let frame = serde_json::json!({
        "v": 1,
        "type": "controller_resume_prepare",
        "request_id": request_id,
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "sender_role": role_wire(role),
        "resume_token_b64": base64::engine::general_purpose::STANDARD.encode(resume_token),
        "transport_challenge_b64": transport_challenge_b64,
        "original_deadline_ms": original_deadline_ms.to_string(),
    });
    validate_controller_message_json(&frame.to_string(), sender_role(role))
        .map_err(|_| ResumeStateError::new("controller_resume_frame_invalid"))?;
    Ok(frame)
}

/// E2E-06：构造 B 端的 `controller_resume_activation_ack`（确认接受候选，字段与
/// prepare 同族 + activation_id + original_deadline_ms）。
pub(crate) fn build_controller_resume_activation_ack(
    lease: &LeaseContext,
    request_id: &str,
    activation_id: &str,
    original_deadline_ms: u64,
) -> Result<serde_json::Value> {
    if activation_id.is_empty() || original_deadline_ms == 0 {
        return Err(ResumeStateError::new("controller_resume_frame_invalid"));
    }
    let frame = serde_json::json!({
        "v": 1,
        "type": "controller_resume_activation_ack",
        "request_id": request_id,
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "activation_id": activation_id,
        "original_deadline_ms": original_deadline_ms.to_string(),
    });
    validate_controller_message_json(&frame.to_string(), SenderRole::ControlledB)
        .map_err(|_| ResumeStateError::new("controller_resume_frame_invalid"))?;
    Ok(frame)
}

/// E2E-06：构造 B 端的 `controller_post_activation_liveness`（第一条激活后存活
/// 证据，序号固定为 1，epoch 必须与旧值不同）。
pub(crate) fn build_controller_post_activation_liveness(
    lease: &LeaseContext,
    request_id: &str,
    activation_id: &str,
    liveness_epoch_b: &str,
) -> Result<serde_json::Value> {
    if activation_id.is_empty() || liveness_epoch_b.is_empty() {
        return Err(ResumeStateError::new("controller_resume_frame_invalid"));
    }
    let frame = serde_json::json!({
        "v": 1,
        "type": "controller_post_activation_liveness",
        "request_id": request_id,
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "activation_id": activation_id,
        "liveness_epoch_b": liveness_epoch_b,
        "heartbeat_sequence_b": "1",
    });
    validate_controller_message_json(&frame.to_string(), SenderRole::ControlledB)
        .map_err(|_| ResumeStateError::new("controller_resume_frame_invalid"))?;
    Ok(frame)
}

/// E2E-06：构造 `controller_resume_ready_ack`（A/B 各自独立 ack 同一 activation
/// 与 B 的第一条 post-activation liveness）。字段来自 Relay 的 confirmed 帧，
/// 因此只做形状与角色自检。
pub(crate) fn build_controller_resume_ready_ack(
    lease: &LeaseContext,
    role: ControllerRole,
    request_id: &str,
    activation_id: &str,
    liveness_epoch_b: &str,
    heartbeat_sequence_b: &str,
) -> Result<serde_json::Value> {
    if activation_id.is_empty() || liveness_epoch_b.is_empty() || heartbeat_sequence_b.is_empty() {
        return Err(ResumeStateError::new("controller_resume_frame_invalid"));
    }
    let frame = serde_json::json!({
        "v": 1,
        "type": "controller_resume_ready_ack",
        "request_id": request_id,
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "activation_id": activation_id,
        "sender_role": role_wire(role),
        "liveness_epoch_b": liveness_epoch_b,
        "heartbeat_sequence_b": heartbeat_sequence_b,
    });
    validate_controller_message_json(&frame.to_string(), sender_role(role))
        .map_err(|_| ResumeStateError::new("controller_resume_frame_invalid"))?;
    Ok(frame)
}

fn role_wire(role: ControllerRole) -> &'static str {
    match role {
        ControllerRole::ControllerA => "A",
        ControllerRole::ControlledB => "B",
    }
}

fn sender_role(role: ControllerRole) -> SenderRole {
    match role {
        ControllerRole::ControllerA => SenderRole::ControllerA,
        ControllerRole::ControlledB => SenderRole::ControlledB,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::liveness::ControllerLivenessWatchdog;

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    fn watchdog(deadline: u64) -> ControllerLivenessWatchdog {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        watchdog
            .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 0)
            .unwrap();
        watchdog
            .accept_direct_json(
                &serde_json::json!({
                    "v": 1,
                    "type": "controller_liveness_a",
                    "connection_id": "connection-0001",
                    "controller_lease_id": "lease-0001",
                    "lease_epoch": "1",
                    "relay_epoch": "relay-epoch-0001",
                    "remote_control_generation": "generation-0001",
                    "role": "A",
                    "liveness_epoch_a": "live-a-1",
                    "heartbeat_sequence_a": "1",
                    "challenge_id": "challenge-0001",
                    "challenge_b64": "AAAAAAAAAAAAAAAAAAAAAA=="
                })
                .to_string(),
                &lease(),
                1_000,
            )
            .unwrap();
        assert!(watchdog.shorten_deadline(deadline));
        watchdog
    }

    #[test]
    fn complete_resume_keeps_watchdog_budget_and_rotates_material_once() {
        let mut gate = ControllerResumeGate::new(
            ControllerRole::ControlledB,
            lease(),
            [1; 32],
            [2; 32],
            30_000,
        );
        assert!(gate.is_business_allowed());
        let watchdog = watchdog(25_000);
        gate.disconnect(1_000, watchdog.deadline_b()).unwrap();
        assert!(!gate.is_business_allowed());
        gate.prepare(2_000, watchdog.deadline_b()).unwrap();
        gate.accept_provisional("activation-0001", 3_000, watchdog.deadline_b())
            .unwrap();
        assert!(!gate.is_business_allowed());
        gate.confirm("activation-0001", 4_000, watchdog.deadline_b())
            .unwrap();
        gate.acknowledge_activation("activation-0001", 5_000, watchdog.deadline_b())
            .unwrap();
        assert_eq!(
            gate.send_post_activation_liveness(
                "activation-0001",
                "live-b-2",
                6_000,
                watchdog.deadline_b()
            )
            .unwrap(),
            "live-b-2"
        );
        assert!(!gate.is_business_allowed());
        gate.acknowledge_ready("activation-0001", 7_000, watchdog.deadline_b())
            .unwrap();
        gate.accept_resumed(
            "activation-0001",
            "live-a-2",
            [3; 32],
            [4; 32],
            8_000,
            watchdog.deadline_b(),
        )
        .unwrap();
        assert!(gate.is_business_allowed());
        assert_eq!(gate.resumed_liveness_epoch(), Some("live-a-2"));
        assert_eq!(gate.original_deadline_b(), 30_000);
        assert!(
            gate.token_matches(ControllerRole::ControlledB, &[3; 32])
                .unwrap()
        );
        assert!(
            !gate
                .token_matches(ControllerRole::ControllerA, &[3; 32])
                .unwrap()
        );
        assert!(
            !gate
                .token_matches(ControllerRole::ControlledB, &[1; 32])
                .unwrap()
        );
        assert!(
            gate.key_fingerprint_matches(ControllerRole::ControlledB, &[4; 32])
                .unwrap()
        );
        assert!(
            !gate
                .key_fingerprint_matches(ControllerRole::ControlledB, &[2; 32])
                .unwrap()
        );
    }

    #[test]
    fn pending_states_and_deadline_expiry_never_reopen_business() {
        let mut gate = ControllerResumeGate::new(
            ControllerRole::ControllerA,
            lease(),
            [5; 32],
            [6; 32],
            30_000,
        );
        gate.disconnect(1_000, Some(29_000)).unwrap();
        gate.prepare(2_000, Some(29_000)).unwrap();
        gate.accept_provisional("activation-0001", 3_000, Some(29_000))
            .unwrap();
        assert!(!gate.is_business_allowed());
        assert_eq!(
            gate.confirm("activation-other", 4_000, Some(29_000))
                .unwrap_err()
                .code,
            "controller_resume_activation_invalid"
        );
        assert_eq!(gate.phase(), ResumePhase::Terminated);
        assert!(!gate.is_business_allowed());

        let mut expired = ControllerResumeGate::new(
            ControllerRole::ControllerA,
            lease(),
            [5; 32],
            [6; 32],
            30_000,
        );
        expired.disconnect(1_000, Some(29_000)).unwrap();
        assert_eq!(
            expired.prepare(30_000, Some(29_000)).unwrap_err().code,
            "controller_resume_phase_invalid"
        );
        assert_eq!(expired.phase(), ResumePhase::Terminated);
    }

    #[test]
    fn wrong_role_or_reused_material_is_terminal() {
        let mut a = ControllerResumeGate::new(
            ControllerRole::ControllerA,
            lease(),
            [7; 32],
            [8; 32],
            30_000,
        );
        a.disconnect(1_000, Some(29_000)).unwrap();
        a.prepare(2_000, Some(29_000)).unwrap();
        a.accept_provisional("activation-0001", 3_000, Some(29_000))
            .unwrap();
        a.confirm("activation-0001", 4_000, Some(29_000)).unwrap();
        assert_eq!(
            a.acknowledge_activation("activation-0001", 5_000, Some(29_000))
                .unwrap_err()
                .code,
            "controller_resume_role_invalid"
        );
        assert_eq!(a.phase(), ResumePhase::Terminated);

        let mut b = ControllerResumeGate::new(
            ControllerRole::ControlledB,
            lease(),
            [9; 32],
            [10; 32],
            30_000,
        );
        b.disconnect(1_000, Some(29_000)).unwrap();
        b.prepare(2_000, Some(29_000)).unwrap();
        b.accept_provisional("activation-0001", 3_000, Some(29_000))
            .unwrap();
        b.confirm("activation-0001", 4_000, Some(29_000)).unwrap();
        b.acknowledge_activation("activation-0001", 5_000, Some(29_000))
            .unwrap();
        b.send_post_activation_liveness("activation-0001", "live-b-2", 6_000, Some(29_000))
            .unwrap();
        b.acknowledge_ready("activation-0001", 7_000, Some(29_000))
            .unwrap();
        assert_eq!(
            b.accept_resumed(
                "activation-0001",
                "live-a-2",
                [9; 32],
                [11; 32],
                8_000,
                Some(29_000),
            )
            .unwrap_err()
            .code,
            "controller_resume_material_reused"
        );
        assert_eq!(b.phase(), ResumePhase::Terminated);
    }

    #[test]
    fn watchdog_expiration_blocks_every_transition_even_after_prepare_retry() {
        let mut gate = ControllerResumeGate::new(
            ControllerRole::ControlledB,
            lease(),
            [12; 32],
            [13; 32],
            30_000,
        );
        gate.disconnect(1_000, Some(25_000)).unwrap();
        gate.prepare(2_000, Some(25_000)).unwrap();
        assert_eq!(
            gate.accept_provisional("activation-0001", 26_000, Some(25_000))
                .unwrap_err()
                .code,
            "controller_resume_phase_invalid"
        );
        assert_eq!(gate.phase(), ResumePhase::Terminated);
        assert_eq!(
            gate.prepare(26_000, Some(25_000)).unwrap_err().code,
            "controller_resume_phase_invalid"
        );
    }

    /// E2E-06：A 角色没有「发送 post-activation liveness」这一步，它靠 Relay 的
    /// confirmed 帧从 ActivationPending 进入 PostActivationLiveness；B 角色重复
    /// 收到同一 confirmed 帧必须幂等。
    #[test]
    fn controller_role_advances_on_relay_confirmation_and_b_is_idempotent() {
        let mut a = ControllerResumeGate::new(
            ControllerRole::ControllerA,
            lease(),
            [14; 32],
            [15; 32],
            30_000,
        );
        a.disconnect(1_000, Some(29_000)).unwrap();
        a.prepare(2_000, Some(29_000)).unwrap();
        a.accept_provisional("activation-0001", 3_000, Some(29_000))
            .unwrap();
        a.confirm("activation-0001", 4_000, Some(29_000)).unwrap();
        a.accept_activation_pending("activation-0001", 5_000, Some(29_000))
            .unwrap();
        assert!(!a.is_business_allowed());
        // 其他激活标识失败关闭（候选不可混淆）。
        let mut wrong = ControllerResumeGate::new(
            ControllerRole::ControllerA,
            lease(),
            [14; 32],
            [15; 32],
            30_000,
        );
        wrong.disconnect(1_000, Some(29_000)).unwrap();
        wrong.prepare(2_000, Some(29_000)).unwrap();
        wrong
            .accept_provisional("activation-0001", 3_000, Some(29_000))
            .unwrap();
        wrong
            .confirm("activation-0001", 4_000, Some(29_000))
            .unwrap();
        wrong
            .accept_activation_pending("activation-0001", 5_000, Some(29_000))
            .unwrap();
        assert_eq!(
            wrong
                .accept_post_activation_liveness_confirmed("activation-0002", 6_000, Some(29_000))
                .unwrap_err()
                .code,
            "controller_resume_activation_invalid"
        );
        assert_eq!(wrong.phase(), ResumePhase::Terminated);

        // 相位不符（还没进入 ActivationPending）失败关闭。
        let mut early = ControllerResumeGate::new(
            ControllerRole::ControllerA,
            lease(),
            [14; 32],
            [15; 32],
            30_000,
        );
        early.disconnect(1_000, Some(29_000)).unwrap();
        early.prepare(2_000, Some(29_000)).unwrap();
        early
            .accept_provisional("activation-0001", 3_000, Some(29_000))
            .unwrap();
        assert_eq!(
            early
                .accept_post_activation_liveness_confirmed("activation-0001", 4_000, Some(29_000))
                .unwrap_err()
                .code,
            "controller_resume_phase_invalid"
        );

        a.accept_post_activation_liveness_confirmed("activation-0001", 6_000, Some(29_000))
            .unwrap();
        a.acknowledge_ready("activation-0001", 7_000, Some(29_000))
            .unwrap();
        a.accept_resumed(
            "activation-0001",
            "live-a-2",
            [16; 32],
            [17; 32],
            8_000,
            Some(29_000),
        )
        .unwrap();
        assert!(a.is_business_allowed());
        assert_eq!(a.resumed_liveness_epoch(), Some("live-a-2"));

        let mut b = ControllerResumeGate::new(
            ControllerRole::ControlledB,
            lease(),
            [18; 32],
            [19; 32],
            30_000,
        );
        b.disconnect(1_000, Some(29_000)).unwrap();
        b.prepare(2_000, Some(29_000)).unwrap();
        b.accept_provisional("activation-0001", 3_000, Some(29_000))
            .unwrap();
        b.confirm("activation-0001", 4_000, Some(29_000)).unwrap();
        b.acknowledge_activation("activation-0001", 5_000, Some(29_000))
            .unwrap();
        b.send_post_activation_liveness("activation-0001", "live-b-2", 6_000, Some(29_000))
            .unwrap();
        // Relay 的 confirmed 帧对 B 是幂等确认（重复投递不改变相位）。
        b.accept_post_activation_liveness_confirmed("activation-0001", 7_000, Some(29_000))
            .unwrap();
        assert_eq!(b.phase(), ResumePhase::PostActivationLiveness);
        b.accept_post_activation_liveness_confirmed("activation-0001", 8_000, Some(29_000))
            .unwrap();
        assert_eq!(b.phase(), ResumePhase::PostActivationLiveness);
    }

    /// E2E-06：出站恢复帧必须直接通过冻结 schema 校验，且畸形输入失败关闭；
    /// 挑战值每次不同（16 字节随机）。
    #[test]
    fn resume_frames_match_the_frozen_contract_and_reject_malformed_input() {
        let lease = lease();
        let prepare = build_controller_resume_prepare(
            &lease,
            ControllerRole::ControllerA,
            "request-resume-prepare-1",
            &[3; 32],
            &new_resume_challenge().unwrap(),
            30_000,
        )
        .unwrap();
        assert_eq!(prepare["type"], "controller_resume_prepare");
        assert_eq!(prepare["sender_role"], "A");
        assert_eq!(prepare["original_deadline_ms"], "30000");
        assert_eq!(
            prepare["resume_token_b64"],
            base64::engine::general_purpose::STANDARD.encode([3_u8; 32])
        );
        assert_eq!(
            build_controller_resume_prepare(
                &lease,
                ControllerRole::ControllerA,
                "request-resume-prepare-1",
                &[3; 32],
                "",
                30_000,
            )
            .unwrap_err()
            .code,
            "controller_resume_material_invalid"
        );
        assert_eq!(
            build_controller_resume_prepare(
                &lease,
                ControllerRole::ControllerA,
                "request-resume-prepare-1",
                &[3; 32],
                &new_resume_challenge().unwrap(),
                0,
            )
            .unwrap_err()
            .code,
            "controller_resume_material_invalid"
        );

        let ready = build_controller_resume_ready_ack(
            &lease,
            ControllerRole::ControlledB,
            "request-resume-ready-1",
            "activation-0001",
            "live-b-2",
            "1",
        )
        .unwrap();
        assert_eq!(ready["type"], "controller_resume_ready_ack");
        assert_eq!(ready["sender_role"], "B");
        assert_eq!(ready["heartbeat_sequence_b"], "1");
        assert_eq!(
            build_controller_resume_ready_ack(
                &lease,
                ControllerRole::ControlledB,
                "request-resume-ready-1",
                "",
                "live-b-2",
                "1",
            )
            .unwrap_err()
            .code,
            "controller_resume_frame_invalid"
        );

        let first = new_resume_challenge().unwrap();
        let second = new_resume_challenge().unwrap();
        assert_ne!(first, second, "恢复挑战必须每次重新生成");
        assert_eq!(
            base64::engine::general_purpose::STANDARD
                .decode(first)
                .unwrap()
                .len(),
            16
        );

        // E2E-06：B 端的两个专属出站帧也必须直接过冻结 schema。
        let activation_ack = build_controller_resume_activation_ack(
            &lease,
            "request-resume-ack-1",
            "activation-0001",
            30_000,
        )
        .unwrap();
        assert_eq!(activation_ack["type"], "controller_resume_activation_ack");
        assert_eq!(activation_ack["original_deadline_ms"], "30000");
        assert_eq!(
            build_controller_resume_activation_ack(
                &lease,
                "request-resume-ack-1",
                "",
                30_000,
            )
            .unwrap_err()
            .code,
            "controller_resume_frame_invalid"
        );
        let post_liveness = build_controller_post_activation_liveness(
            &lease,
            "request-resume-liveness-1",
            "activation-0001",
            "live-b-2",
        )
        .unwrap();
        assert_eq!(
            post_liveness["type"],
            "controller_post_activation_liveness"
        );
        assert_eq!(post_liveness["heartbeat_sequence_b"], "1");
        assert_eq!(post_liveness["liveness_epoch_b"], "live-b-2");
        assert_eq!(
            build_controller_post_activation_liveness(
                &lease,
                "request-resume-liveness-1",
                "activation-0001",
                "",
            )
            .unwrap_err()
            .code,
            "controller_resume_frame_invalid"
        );
    }
}
