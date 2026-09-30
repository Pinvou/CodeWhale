//! Role-isolated liveness state and B's conservative local watchdog.
//!
//! The wire contract is frozen in [`super::protocol`]. This module owns only
//! the local state transitions: A's authenticated evidence may replace the
//! deadline on a new sequence, an equal sequence may only tighten it, and no
//! B-side activity can extend the budget. Callers must AEAD-open direct A
//! liveness before handing the plaintext here; Relay attestation input must
//! additionally pass the authenticated Relay-signature path installed by the
//! transport owner.

use std::collections::HashMap;

use serde_json::Value;

use super::protocol::{
    DeadlineUpdate, LeaseContext, MAX_CONTROL_MESSAGE_BYTES, apply_liveness_deadline,
    validate_controller_liveness_payload_json, validate_liveness_attestation_message_json,
};

pub(crate) const LIVENESS_INTERVAL_MS: u64 = 5_000;
pub(crate) const LIVENESS_BLACKHOLE_SUSPECT_MS: u64 = 15_000;
pub(crate) const LIVENESS_TOTAL_BUDGET_MS: u64 = 30_000;
pub(crate) const LIVENESS_SAFETY_MARGIN_MS: u64 = 1_000;
pub(crate) const MAX_LIVENESS_RTT_MS: u64 = 5_000;
pub(crate) const MAX_LIVENESS_ATTESTATION_AGE_MS: u64 = 5_000;
const MAX_PENDING_CHALLENGES: usize = 8;
const MAX_USED_CHALLENGES: usize = 64;
const MAX_LIVENESS_ID_BYTES: usize = 128;

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum LivenessDeadlineUpdate {
    Installed,
    Replaced,
    Tightened,
}

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum LivenessWatchdogState {
    AwaitingEvidence,
    Healthy,
    Suspect,
    Expired,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct LivenessError {
    pub(crate) code: &'static str,
    pub(crate) field: &'static str,
}

impl LivenessError {
    const fn new(code: &'static str, field: &'static str) -> Self {
        Self { code, field }
    }
}

impl std::fmt::Display for LivenessError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(formatter, "{}: {}", self.code, self.field)
    }
}

impl std::error::Error for LivenessError {}

type Result<T> = std::result::Result<T, LivenessError>;

#[derive(Debug, Clone, Eq, PartialEq)]
struct PendingChallenge {
    challenge_b64: String,
    sent_at_b: u64,
}

#[derive(Debug)]
pub(crate) struct ControllerLivenessWatchdog {
    liveness_epoch_a: String,
    heartbeat_sequence_a: u64,
    last_attestation_sequence: u64,
    deadline_b: Option<u64>,
    last_advanced_at_b: Option<u64>,
    pending_challenges: HashMap<String, PendingChallenge>,
    used_challenges: HashMap<(String, String), u64>,
}

impl ControllerLivenessWatchdog {
    pub(crate) fn new(liveness_epoch_a: impl Into<String>) -> Result<Self> {
        let liveness_epoch_a = liveness_epoch_a.into();
        validate_identifier(&liveness_epoch_a, "liveness_epoch_a")?;
        Ok(Self {
            liveness_epoch_a,
            heartbeat_sequence_a: 0,
            last_attestation_sequence: 0,
            deadline_b: None,
            last_advanced_at_b: None,
            pending_challenges: HashMap::new(),
            used_challenges: HashMap::new(),
        })
    }

    /// Install an epoch only from an authenticated `controller_joined` or
    /// final `controller_resumed` result. Resume must not call this until all
    /// DEV-09 activation gates have completed.
    pub(crate) fn install_epoch(&mut self, liveness_epoch_a: impl Into<String>) -> Result<()> {
        let liveness_epoch_a = liveness_epoch_a.into();
        validate_identifier(&liveness_epoch_a, "liveness_epoch_a")?;
        if liveness_epoch_a == self.liveness_epoch_a {
            return Err(LivenessError::new("stale_context", "liveness_epoch_a"));
        }
        self.liveness_epoch_a = liveness_epoch_a;
        self.heartbeat_sequence_a = 0;
        self.last_attestation_sequence = 0;
        self.deadline_b = None;
        self.last_advanced_at_b = None;
        self.pending_challenges.clear();
        self.used_challenges.clear();
        Ok(())
    }

    pub(crate) fn register_challenge(
        &mut self,
        challenge_id: impl Into<String>,
        challenge_b64: impl Into<String>,
        sent_at_b: u64,
    ) -> Result<()> {
        let challenge_id = challenge_id.into();
        let challenge_b64 = challenge_b64.into();
        validate_identifier(&challenge_id, "challenge_id")?;
        if challenge_b64.is_empty() || challenge_b64.len() > MAX_LIVENESS_ID_BYTES {
            return Err(LivenessError::new("invalid_field", "challenge_b64"));
        }
        self.prune_challenges(sent_at_b);
        if self
            .used_challenges
            .contains_key(&(challenge_id.clone(), challenge_b64.clone()))
        {
            return Err(LivenessError::new("replay", "challenge_id"));
        }
        if self.pending_challenges.contains_key(&challenge_id) {
            return Err(LivenessError::new("challenge_conflict", "challenge_id"));
        }
        if self.pending_challenges.len() >= MAX_PENDING_CHALLENGES {
            return Err(LivenessError::new(
                "challenge_capacity_exhausted",
                "challenge_id",
            ));
        }
        let _ = self.pending_challenges.insert(
            challenge_id,
            PendingChallenge {
                challenge_b64,
                sent_at_b,
            },
        );
        Ok(())
    }

    /// Accept AEAD-authenticated direct A liveness plaintext. The validator
    /// binds the full lease context, epoch, role, sequence and challenge; this
    /// method additionally enforces the challenge's local RTT and deadline
    /// monotonicity.
    pub(crate) fn accept_direct_json(
        &mut self,
        input: &str,
        lease: &LeaseContext,
        received_at_b: u64,
    ) -> Result<LivenessDeadlineUpdate> {
        self.ensure_challenge_capacity(received_at_b)?;
        let challenge_id = direct_field(input, "challenge_id")?;
        let Some(pending) = self.pending_challenges.get(&challenge_id) else {
            return Err(LivenessError::new("unknown_challenge", "challenge_id"));
        };
        let challenge_b64 = pending.challenge_b64.clone();
        let sent_at_b = pending.sent_at_b;
        let message = validate_controller_liveness_payload_json(
            input,
            lease,
            &self.liveness_epoch_a,
            self.heartbeat_sequence_a,
            &challenge_id,
            &challenge_b64,
        )
        .map_err(|error| LivenessError::new(error.code, leaked_field(error.field.as_str())))?;
        let sequence = object_u64(&message, "heartbeat_sequence_a")?;
        let measured_rtt_ms = elapsed(sent_at_b, received_at_b, "measured_rtt_ms")?;
        if measured_rtt_ms > MAX_LIVENESS_RTT_MS {
            return Err(LivenessError::new("rtt_exceeded", "measured_rtt_ms"));
        }
        let candidate = direct_candidate_deadline(received_at_b, measured_rtt_ms)?;
        let update = self.apply_evidence(sequence, candidate, received_at_b)?;
        self.consume_challenge(challenge_id, challenge_b64, received_at_b)?;
        Ok(update)
    }

    /// Accept a Relay-signed fallback proof. Cross-machine `issued_at_relay`
    /// is intentionally ignored; age, remaining TTL and request RTT are all
    /// conservatively deducted from B's monotonic receive time.
    pub(crate) fn accept_attestation_json(
        &mut self,
        input: &str,
        lease: &LeaseContext,
        request_sent_at_b: u64,
        received_at_b: u64,
    ) -> Result<LivenessDeadlineUpdate> {
        self.ensure_challenge_capacity(received_at_b)?;
        let challenge_id = direct_field(input, "challenge_id")?;
        let Some(pending) = self.pending_challenges.get(&challenge_id) else {
            return Err(LivenessError::new("unknown_challenge", "challenge_id"));
        };
        let challenge_b64 = pending.challenge_b64.clone();
        let message = validate_liveness_attestation_message_json(
            input,
            lease,
            &self.liveness_epoch_a,
            self.heartbeat_sequence_a,
            self.last_attestation_sequence,
            &challenge_id,
            &challenge_b64,
        )
        .map_err(|error| LivenessError::new(error.code, leaked_field(error.field.as_str())))?;
        let observed_age_ms = object_u64(&message, "observed_age_ms")?;
        let remaining_ttl_ms = object_u64(&message, "remaining_ttl_ms")?;
        let measured_rtt_ms = elapsed(request_sent_at_b, received_at_b, "measured_rtt_ms")?;
        if observed_age_ms > MAX_LIVENESS_ATTESTATION_AGE_MS {
            return Err(LivenessError::new("attestation_too_old", "observed_age_ms"));
        }
        if measured_rtt_ms > MAX_LIVENESS_RTT_MS {
            return Err(LivenessError::new("rtt_exceeded", "measured_rtt_ms"));
        }
        let candidate = attestation_candidate_deadline(
            received_at_b,
            observed_age_ms,
            remaining_ttl_ms,
            measured_rtt_ms,
        )?;
        let sequence = object_u64(&message, "heartbeat_sequence_a")?;
        let attestation_sequence = object_u64(&message, "attestation_sequence")?;
        if attestation_sequence <= self.last_attestation_sequence {
            return Err(LivenessError::new("replay", "attestation_sequence"));
        }
        let update = self.apply_evidence(sequence, candidate, received_at_b)?;
        self.last_attestation_sequence = attestation_sequence;
        self.consume_challenge(challenge_id, challenge_b64, received_at_b)?;
        Ok(update)
    }

    pub(crate) fn state(&self, now_b: u64) -> LivenessWatchdogState {
        let Some(deadline) = self.deadline_b else {
            return LivenessWatchdogState::AwaitingEvidence;
        };
        if now_b >= deadline {
            return LivenessWatchdogState::Expired;
        }
        if let Some(last_advanced) = self.last_advanced_at_b {
            if now_b.saturating_sub(last_advanced) >= LIVENESS_BLACKHOLE_SUSPECT_MS {
                return LivenessWatchdogState::Suspect;
            }
        }
        LivenessWatchdogState::Healthy
    }

    pub(crate) fn deadline_b(&self) -> Option<u64> {
        self.deadline_b
    }

    /// Explicit disconnect, Relay epoch change or other local failure may only
    /// shorten the installed deadline. It can never create a fresh budget.
    pub(crate) fn shorten_deadline(&mut self, now_b: u64) -> bool {
        match self.deadline_b {
            Some(deadline) if deadline > now_b => {
                self.deadline_b = Some(now_b);
                true
            }
            _ => false,
        }
    }

    fn apply_evidence(
        &mut self,
        sequence: u64,
        candidate_deadline: u64,
        received_at_b: u64,
    ) -> Result<LivenessDeadlineUpdate> {
        let (policy, update) = if sequence < self.heartbeat_sequence_a {
            return Err(LivenessError::new("replay", "heartbeat_sequence_a"));
        } else if sequence == self.heartbeat_sequence_a {
            (
                DeadlineUpdate::TightenOnly,
                LivenessDeadlineUpdate::Tightened,
            )
        } else {
            (
                DeadlineUpdate::Replace,
                if self.deadline_b.is_none() {
                    LivenessDeadlineUpdate::Installed
                } else {
                    LivenessDeadlineUpdate::Replaced
                },
            )
        };
        let previous = self.deadline_b.unwrap_or(candidate_deadline);
        self.deadline_b = Some(apply_liveness_deadline(
            previous,
            candidate_deadline,
            policy,
        ));
        if sequence > self.heartbeat_sequence_a {
            self.heartbeat_sequence_a = sequence;
            self.last_advanced_at_b = Some(received_at_b);
        }
        Ok(update)
    }

    fn consume_challenge(
        &mut self,
        challenge_id: String,
        challenge_b64: String,
        received_at_b: u64,
    ) -> Result<()> {
        self.pending_challenges.remove(&challenge_id);
        if self.used_challenges.len() >= MAX_USED_CHALLENGES {
            return Err(LivenessError::new(
                "challenge_capacity_exhausted",
                "challenge_id",
            ));
        }
        self.used_challenges
            .insert((challenge_id, challenge_b64), received_at_b);
        Ok(())
    }

    fn prune_challenges(&mut self, now_b: u64) {
        self.pending_challenges.retain(|_, challenge| {
            now_b.saturating_sub(challenge.sent_at_b) <= LIVENESS_TOTAL_BUDGET_MS
        });
        // The sequence floors survive history GC for the whole epoch. Retain
        // consumed nonces for the full validity window, not the whole session.
        self.used_challenges.retain(|_, consumed_at_b| {
            now_b.saturating_sub(*consumed_at_b) <= LIVENESS_TOTAL_BUDGET_MS
        });
    }

    fn ensure_challenge_capacity(&mut self, received_at_b: u64) -> Result<()> {
        self.prune_challenges(received_at_b);
        if self.used_challenges.len() >= MAX_USED_CHALLENGES {
            return Err(LivenessError::new(
                "challenge_capacity_exhausted",
                "challenge_id",
            ));
        }
        Ok(())
    }
}

pub(crate) fn direct_candidate_deadline(received_at_b: u64, measured_rtt_ms: u64) -> Result<u64> {
    if measured_rtt_ms > MAX_LIVENESS_RTT_MS {
        return Err(LivenessError::new("rtt_exceeded", "measured_rtt_ms"));
    }
    let deduction = measured_rtt_ms
        .checked_add(LIVENESS_SAFETY_MARGIN_MS)
        .ok_or_else(|| LivenessError::new("deadline_overflow", "candidate_deadline"))?;
    let absolute_deadline = received_at_b
        .checked_add(LIVENESS_TOTAL_BUDGET_MS)
        .ok_or_else(|| LivenessError::new("deadline_overflow", "candidate_deadline"))?;
    Ok(absolute_deadline
        .checked_sub(deduction)
        .ok_or_else(|| LivenessError::new("deadline_overflow", "candidate_deadline"))?)
}

pub(crate) fn attestation_candidate_deadline(
    received_at_b: u64,
    observed_age_ms: u64,
    remaining_ttl_ms: u64,
    measured_rtt_ms: u64,
) -> Result<u64> {
    if observed_age_ms > MAX_LIVENESS_ATTESTATION_AGE_MS {
        return Err(LivenessError::new("attestation_too_old", "observed_age_ms"));
    }
    if remaining_ttl_ms > LIVENESS_TOTAL_BUDGET_MS {
        return Err(LivenessError::new("invalid_field", "remaining_ttl_ms"));
    }
    if measured_rtt_ms > MAX_LIVENESS_RTT_MS {
        return Err(LivenessError::new("rtt_exceeded", "measured_rtt_ms"));
    }
    let remaining_total = LIVENESS_TOTAL_BUDGET_MS
        .checked_sub(observed_age_ms)
        .ok_or_else(|| LivenessError::new("attestation_too_old", "observed_age_ms"))?;
    let base = remaining_ttl_ms.min(remaining_total);
    let deduction = measured_rtt_ms
        .checked_add(LIVENESS_SAFETY_MARGIN_MS)
        .ok_or_else(|| LivenessError::new("deadline_overflow", "candidate_deadline"))?;
    let absolute_deadline = received_at_b
        .checked_add(base)
        .ok_or_else(|| LivenessError::new("deadline_overflow", "candidate_deadline"))?;
    Ok(absolute_deadline
        .checked_sub(deduction)
        .ok_or_else(|| LivenessError::new("deadline_overflow", "candidate_deadline"))?)
}

fn direct_field(input: &str, field: &'static str) -> Result<String> {
    if input.len() > MAX_CONTROL_MESSAGE_BYTES {
        return Err(LivenessError::new("message_too_large", "json"));
    }
    let value: Value =
        serde_json::from_str(input).map_err(|_| LivenessError::new("invalid_json", "json"))?;
    value
        .get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty() && value.len() <= MAX_LIVENESS_ID_BYTES)
        .map(str::to_owned)
        .ok_or_else(|| LivenessError::new("invalid_field", field))
}

fn object_u64(object: &Value, field: &'static str) -> Result<u64> {
    object
        .get(field)
        .and_then(Value::as_str)
        .and_then(|value| value.parse::<u64>().ok())
        .ok_or_else(|| LivenessError::new("invalid_field", field))
}

fn elapsed(from: u64, to: u64, field: &'static str) -> Result<u64> {
    to.checked_sub(from)
        .ok_or_else(|| LivenessError::new("invalid_clock", field))
}

fn validate_identifier(value: &str, field: &'static str) -> Result<()> {
    if value.is_empty()
        || value.len() > MAX_LIVENESS_ID_BYTES
        || !value
            .as_bytes()
            .first()
            .is_some_and(u8::is_ascii_alphanumeric)
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'~' | b'-'))
    {
        return Err(LivenessError::new("invalid_field", field));
    }
    Ok(())
}

fn leaked_field(field: &str) -> &'static str {
    match field {
        "json" | "liveness_payload" | "attestation" => "message",
        "challenge_id" => "challenge_id",
        "challenge_b64" => "challenge_b64",
        "heartbeat_sequence_a" => "heartbeat_sequence_a",
        "attestation_sequence" => "attestation_sequence",
        "observed_age_ms" => "observed_age_ms",
        "remaining_ttl_ms" => "remaining_ttl_ms",
        "measured_rtt_ms" => "measured_rtt_ms",
        "liveness_epoch_a" => "liveness_epoch_a",
        _ => "message",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "epoch-test-1".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    fn direct_payload(sequence: u64, challenge_id: &str, challenge_b64: &str) -> String {
        serde_json::json!({
            "v": 1,
            "type": "controller_liveness_a",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "1",
            "relay_epoch": "epoch-test-1",
            "remote_control_generation": "generation-0001",
            "role": "A",
            "liveness_epoch_a": "live-a-1",
            "heartbeat_sequence_a": sequence.to_string(),
            "challenge_id": challenge_id,
            "challenge_b64": challenge_b64
        })
        .to_string()
    }

    fn attestation_payload(
        sequence: u64,
        attestation_sequence: u64,
        challenge_id: &str,
        challenge_b64: &str,
    ) -> String {
        serde_json::json!({
            "v": 1,
            "type": "controller_liveness_attestation_a",
            "request_id": "request-0001",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "1",
            "relay_epoch": "epoch-test-1",
            "remote_control_generation": "generation-0001",
            "liveness_epoch_a": "live-a-1",
            "heartbeat_sequence_a": sequence.to_string(),
            "attestation_sequence": attestation_sequence.to_string(),
            "challenge_id": challenge_id,
            "challenge_b64": challenge_b64,
            "observed_age_ms": "5000",
            "remaining_ttl_ms": "25000",
            "issued_at_relay": "9000",
            "relay_signing_key_id": "relay-key-test-1",
            "signature_algorithm": "ed25519",
            "relay_signature_b64": format!("{}==", "A".repeat(86))
        })
        .to_string()
    }

    #[test]
    fn deadline_formulas_are_conservative() {
        assert_eq!(direct_candidate_deadline(100_000, 5_000).unwrap(), 124_000);
        assert_eq!(
            attestation_candidate_deadline(100_000, 5_000, 25_000, 5_000).unwrap(),
            119_000
        );
        assert_eq!(
            direct_candidate_deadline(100_000, 5_001).unwrap_err().code,
            "rtt_exceeded"
        );
        assert_eq!(
            attestation_candidate_deadline(100_000, 5_001, 24_999, 0)
                .unwrap_err()
                .code,
            "attestation_too_old"
        );
        assert_eq!(
            attestation_candidate_deadline(u64::MAX, 0, 30_000, 0)
                .unwrap_err()
                .code,
            "deadline_overflow"
        );
    }

    #[test]
    fn direct_evidence_installs_advances_tightens_and_rejects_replay() {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        watchdog
            .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 1_000)
            .unwrap();
        assert_eq!(
            watchdog
                .accept_direct_json(
                    &direct_payload(1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA=="),
                    &lease(),
                    3_000,
                )
                .unwrap(),
            LivenessDeadlineUpdate::Installed
        );
        assert_eq!(watchdog.deadline_b(), Some(30_000));
        assert_eq!(watchdog.state(10_000), LivenessWatchdogState::Healthy);
        assert_eq!(watchdog.state(30_000), LivenessWatchdogState::Expired);

        watchdog
            .register_challenge("challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ==", 6_000)
            .unwrap();
        assert_eq!(
            watchdog
                .accept_direct_json(
                    &direct_payload(2, "challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ=="),
                    &lease(),
                    8_000,
                )
                .unwrap(),
            LivenessDeadlineUpdate::Replaced
        );
        assert_eq!(watchdog.deadline_b(), Some(35_000));

        watchdog
            .register_challenge("challenge-0003", "AgICAgICAgICAgICAgICAg==", 11_000)
            .unwrap();
        assert_eq!(
            watchdog
                .accept_direct_json(
                    &direct_payload(2, "challenge-0003", "AgICAgICAgICAgICAgICAg=="),
                    &lease(),
                    13_000,
                )
                .unwrap(),
            LivenessDeadlineUpdate::Tightened
        );
        assert_eq!(watchdog.deadline_b(), Some(35_000));

        watchdog
            .register_challenge("challenge-0004", "AwMDAwMDAwMDAwMDAwMDAw==", 16_000)
            .unwrap();
        assert_eq!(
            watchdog
                .accept_direct_json(
                    &direct_payload(1, "challenge-0004", "AwMDAwMDAwMDAwMDAwMDAw=="),
                    &lease(),
                    18_000,
                )
                .unwrap_err()
                .code,
            "replay"
        );
        assert_eq!(
            watchdog
                .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 20_000)
                .unwrap_err()
                .code,
            "replay"
        );
    }

    #[test]
    fn direct_challenge_controls_rtt_and_context() {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        watchdog
            .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 2_000)
            .unwrap();
        assert_eq!(
            watchdog
                .accept_direct_json(
                    &direct_payload(1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA=="),
                    &lease(),
                    1_000,
                )
                .unwrap_err()
                .code,
            "invalid_clock"
        );
        assert_eq!(
            watchdog
                .accept_direct_json(
                    &direct_payload(1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA=="),
                    &lease(),
                    8_000,
                )
                .unwrap_err()
                .code,
            "rtt_exceeded"
        );
        let stale = direct_payload(1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==")
            .replace("generation-0001", "generation-old");
        assert_eq!(
            watchdog
                .accept_direct_json(&stale, &lease(), 3_000)
                .unwrap_err()
                .code,
            "stale_context"
        );
        assert_eq!(watchdog.deadline_b(), None);
    }

    #[test]
    fn attestation_uses_strict_sequence_and_one_time_challenge() {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        watchdog
            .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 1_000)
            .unwrap();
        assert_eq!(
            watchdog
                .accept_attestation_json(
                    &attestation_payload(1, 1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA=="),
                    &lease(),
                    1_000,
                    6_000,
                )
                .unwrap(),
            LivenessDeadlineUpdate::Installed
        );
        assert_eq!(watchdog.deadline_b(), Some(25_000));
        assert_eq!(
            watchdog
                .accept_attestation_json(
                    &attestation_payload(1, 1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA=="),
                    &lease(),
                    1_000,
                    6_000,
                )
                .unwrap_err()
                .code,
            "unknown_challenge"
        );

        watchdog
            .register_challenge("challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ==", 10_000)
            .unwrap();
        assert_eq!(
            watchdog
                .accept_attestation_json(
                    &attestation_payload(2, 1, "challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ=="),
                    &lease(),
                    10_000,
                    15_000,
                )
                .unwrap_err()
                .code,
            "replay"
        );
        assert_eq!(watchdog.deadline_b(), Some(25_000));
    }

    #[test]
    fn suspect_and_local_failure_never_extend_deadline() {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        watchdog
            .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 0)
            .unwrap();
        watchdog
            .accept_direct_json(
                &direct_payload(1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA=="),
                &lease(),
                1_000,
            )
            .unwrap();
        assert_eq!(watchdog.state(16_000), LivenessWatchdogState::Suspect);
        assert_ne!(watchdog.state(16_000), LivenessWatchdogState::Expired);
        assert!(watchdog.shorten_deadline(17_000));
        assert_eq!(watchdog.deadline_b(), Some(17_000));
        assert_eq!(watchdog.state(17_000), LivenessWatchdogState::Expired);
        assert!(!watchdog.shorten_deadline(99_000));
        assert_eq!(watchdog.deadline_b(), Some(17_000));
    }

    #[test]
    fn long_session_reclaims_history_but_keeps_epoch_sequence_floors() {
        let nonce = "AAAAAAAAAAAAAAAAAAAAAA==";
        for attested in [false, true] {
            let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
            for sequence in 1..=128 {
                let sent = sequence * LIVENESS_INTERVAL_MS;
                let received = sent + 1_000;
                let challenge = format!("challenge-{sequence}");
                watchdog
                    .register_challenge(&challenge, nonce, sent)
                    .unwrap();
                if attested {
                    watchdog
                        .accept_attestation_json(
                            &attestation_payload(sequence, sequence, &challenge, nonce),
                            &lease(),
                            sent,
                            received,
                        )
                        .unwrap();
                } else {
                    watchdog
                        .accept_direct_json(
                            &direct_payload(sequence, &challenge, nonce),
                            &lease(),
                            received,
                        )
                        .unwrap();
                }
                assert_eq!(watchdog.state(received), LivenessWatchdogState::Healthy);
                assert!(watchdog.pending_challenges.is_empty());
                assert!(watchdog.used_challenges.len() <= 7);
                assert_eq!(
                    watchdog
                        .register_challenge(&challenge, nonce, received)
                        .unwrap_err()
                        .code,
                    "replay"
                );
            }
            let now = 129 * LIVENESS_INTERVAL_MS;
            watchdog
                .register_challenge("challenge-1", nonce, now)
                .unwrap();
            let deadline = watchdog.deadline_b();
            let error = if attested {
                watchdog.accept_attestation_json(
                    &attestation_payload(1, 1, "challenge-1", nonce),
                    &lease(),
                    now,
                    now + 1_000,
                )
            } else {
                watchdog.accept_direct_json(
                    &direct_payload(1, "challenge-1", nonce),
                    &lease(),
                    now + 1_000,
                )
            }
            .unwrap_err();
            assert_eq!(error.code, "replay");
            assert_eq!(watchdog.deadline_b(), deadline);
        }
    }

    #[test]
    fn expired_pending_challenges_do_not_exhaust_capacity_or_accept_late_proofs() {
        let nonce = "AAAAAAAAAAAAAAAAAAAAAA==";
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        for index in 0..MAX_PENDING_CHALLENGES {
            watchdog
                .register_challenge(format!("challenge-{index}"), nonce, 0)
                .unwrap();
        }
        watchdog
            .register_challenge("fresh", nonce, LIVENESS_TOTAL_BUDGET_MS + 1)
            .unwrap();
        assert_eq!(watchdog.pending_challenges.len(), 1);
        assert_eq!(
            watchdog
                .accept_attestation_json(
                    &attestation_payload(1, 1, "challenge-0", nonce),
                    &lease(),
                    LIVENESS_TOTAL_BUDGET_MS + 1,
                    LIVENESS_TOTAL_BUDGET_MS + 2,
                )
                .unwrap_err()
                .code,
            "unknown_challenge"
        );
        assert_eq!(watchdog.deadline_b(), None);
    }

    #[test]
    fn epoch_installation_is_explicit_and_reset_is_complete() {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        watchdog
            .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 0)
            .unwrap();
        watchdog
            .accept_direct_json(
                &direct_payload(1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA=="),
                &lease(),
                1_000,
            )
            .unwrap();
        assert_eq!(
            watchdog.install_epoch("live-a-1").unwrap_err().code,
            "stale_context"
        );
        watchdog.install_epoch("live-a-2").unwrap();
        assert_eq!(watchdog.deadline_b(), None);
        assert_eq!(
            watchdog
                .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 0)
                .unwrap(),
            ()
        );
        watchdog
            .register_challenge("challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ==", 1_000)
            .unwrap();
        let old_epoch = direct_payload(1, "challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ==");
        assert_eq!(
            watchdog
                .accept_direct_json(&old_epoch, &lease(), 2_000)
                .unwrap_err()
                .code,
            "stale_context"
        );
    }
}
