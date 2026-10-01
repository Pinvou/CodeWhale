//! Local endpoint state gate for the controller resume protocol.
//!
//! Relay owns the two-role activation transition. This type owns the local
//! role's one-shot token and transport-key fingerprint, refuses every business
//! gate until the final authenticated result, and never extends the deadline
//! frozen from DEV-08 evidence at the first transport interruption. Relay's
//! wire deadline is a separate clock-domain value and is never a B clock.

use zeroize::{Zeroize, Zeroizing};

use super::protocol::LeaseContext;
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
    relay_deadline_identity_ms: u64,
    frozen_deadline_b: Option<u64>,
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
        relay_deadline_identity_ms: u64,
    ) -> Self {
        Self {
            role,
            lease,
            relay_deadline_identity_ms,
            frozen_deadline_b: None,
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

    pub(crate) fn relay_deadline_identity_ms(&self) -> u64 {
        self.relay_deadline_identity_ms
    }

    pub(crate) fn frozen_deadline_b(&self) -> Option<u64> {
        self.frozen_deadline_b
    }

    /// Freeze authenticated local evidence independently of the opaque Relay
    /// wire deadline identity:
    /// neither the Relay wall clock nor a later proof can grant local budget.
    pub(crate) fn disconnect(
        &mut self,
        now_b: u64,
        watchdog_deadline_b: Option<u64>,
    ) -> Result<()> {
        if self.phase != ResumePhase::Connected {
            return self.terminate("controller_resume_phase_invalid");
        }
        let Some(deadline) = watchdog_deadline_b.filter(|deadline| now_b < *deadline) else {
            return self.terminate("controller_resume_phase_invalid");
        };
        self.frozen_deadline_b = Some(deadline);
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
        self.frozen_deadline_b
            .is_some_and(|deadline| now_b < deadline)
            && watchdog_deadline_b.is_none_or(|deadline| now_b < deadline)
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
        assert_eq!(gate.relay_deadline_identity_ms(), 30_000);
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
    fn every_resume_phase_obeys_one_frozen_local_cutoff_not_the_relay_clock() {
        fn step(gate: &mut ControllerResumeGate, step: usize, now_b: u64) -> Result<()> {
            let later_evidence = Some(u64::MAX);
            match step {
                1 => gate.prepare(now_b, later_evidence),
                2 => gate.accept_provisional("activation-0001", now_b, later_evidence),
                3 => gate.confirm("activation-0001", now_b, later_evidence),
                4 => gate.acknowledge_activation("activation-0001", now_b, later_evidence),
                5 => gate
                    .send_post_activation_liveness(
                        "activation-0001",
                        "live-b-2",
                        now_b,
                        later_evidence,
                    )
                    .map(|_| ()),
                6 => gate.acknowledge_ready("activation-0001", now_b, later_evidence),
                7 => gate.accept_resumed(
                    "activation-0001",
                    "live-a-2",
                    [3; 32],
                    [4; 32],
                    now_b,
                    later_evidence,
                ),
                _ => unreachable!(),
            }
        }
        for boundary_step in 1..=7 {
            let mut gate = ControllerResumeGate::new(
                ControllerRole::ControlledB,
                lease(),
                [1; 32],
                [2; 32],
                30_000,
            );
            // A healthy long session can outlive its opaque Relay identity.
            gate.disconnect(50_000, Some(72_000)).unwrap();
            assert_eq!(gate.relay_deadline_identity_ms(), 30_000);
            assert_eq!(gate.frozen_deadline_b(), Some(72_000));
            for prior in 1..boundary_step {
                step(&mut gate, prior, 51_000 + prior as u64).unwrap();
                assert!(!gate.is_business_allowed());
                assert_eq!(gate.frozen_deadline_b(), Some(72_000));
            }
            assert!(step(&mut gate, boundary_step, 72_000).is_err());
            assert_eq!(gate.phase(), ResumePhase::Terminated);
            assert!(
                !gate
                    .token_matches(ControllerRole::ControlledB, &[1; 32])
                    .unwrap()
            );
        }
        for role in [ControllerRole::ControllerA, ControllerRole::ControlledB] {
            for evidence in [None, Some(50_000), Some(49_999)] {
                let mut gate = ControllerResumeGate::new(role, lease(), [1; 32], [2; 32], u64::MAX);
                assert!(gate.disconnect(50_000, evidence).is_err());
                assert_eq!(gate.phase(), ResumePhase::Terminated);
            }
        }
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
}
