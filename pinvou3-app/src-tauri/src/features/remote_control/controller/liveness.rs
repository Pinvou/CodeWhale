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

#[cfg(test)]
const TEST_EVIDENCE_INTERVAL_MS: u64 = 5_000;
pub(crate) const LIVENESS_BLACKHOLE_SUSPECT_MS: u64 = 15_000;
/// 距 deadline 只剩这么多预算时就开始索取证据（留出一次往返 + relay 处理时间）。
pub(crate) const LIVENESS_EVIDENCE_LEAD_MS: u64 = 10_000;
pub(crate) const LIVENESS_TOTAL_BUDGET_MS: u64 = 30_000;
pub(crate) const LIVENESS_SAFETY_MARGIN_MS: u64 = 1_000;
pub(crate) const MAX_LIVENESS_RTT_MS: u64 = 5_000;
pub(crate) const LIVENESS_EVIDENCE_POLL_MS: u64 = 2_000;
pub(crate) const LIVENESS_ATTESTATION_ESCALATION_MS: u64 = 2_500;
/// Allow the first poll, escalation wait, next poll and full fallback RTT
/// before the existing suspect boundary. Round conservatively to seconds.
const SUSPECT_EVIDENCE_LEAD_MS: u64 = (2 * LIVENESS_EVIDENCE_POLL_MS
    + LIVENESS_ATTESTATION_ESCALATION_MS
    + MAX_LIVENESS_RTT_MS
    + 999)
    / 1_000
    * 1_000;
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
    frozen_deadline_b: Option<u64>,
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
            frozen_deadline_b: None,
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
        self.frozen_deadline_b = None;
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

    /// 撤销一个未发出或已在本地超时的挑战。它没有成为有效证明，因此不能进入
    /// used 集合；删除 pending 即可释放有界容量。
    pub(crate) fn cancel_challenge(&mut self, challenge_id: &str) {
        self.pending_challenges.remove(challenge_id);
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
                // A locally issued challenge can still produce acceptable evidence while
                // it is inside the frozen RTT budget. Do not flap the UI to reconnecting
                // during that short response window. This never extends the cryptographic
                // deadline, and an unanswered challenge becomes suspect after at most one
                // MAX_LIVENESS_RTT_MS window.
                let awaiting_timely_evidence = self.pending_challenges.values().any(|challenge| {
                    now_b >= challenge.sent_at_b
                        && now_b.saturating_sub(challenge.sent_at_b) <= MAX_LIVENESS_RTT_MS
                });
                if awaiting_timely_evidence {
                    return LivenessWatchdogState::Healthy;
                }
                return LivenessWatchdogState::Suspect;
            }
        }
        LivenessWatchdogState::Healthy
    }

    pub(crate) fn deadline_b(&self) -> Option<u64> {
        self.deadline_b
    }

    /// E2E-06：无证据、证据已到期或接近 deadline 时索取存活证据；也会在
    /// suspect 边界前提前索证，即使此时仍 healthy 且 deadline 余量充足。
    /// 当前 15 秒 suspect 窗减去 12 秒提前量，在最后有效证据推进后 3 秒
    /// 即满足条件；2 秒轮询通常在 3–5 秒触发，实际时机仍受调度及在途挑战影响。
    /// 这会提前产生索证流量，但发起挑战本身绝不延长认证证据或冻结恢复截止。
    pub(crate) fn needs_evidence(&self, now_b: u64) -> bool {
        match self.deadline_b {
            None => true,
            Some(deadline) => {
                if now_b >= deadline {
                    return true;
                }
                if self.last_advanced_at_b.is_some_and(|last| {
                    now_b.saturating_sub(last)
                        >= LIVENESS_BLACKHOLE_SUSPECT_MS.saturating_sub(SUSPECT_EVIDENCE_LEAD_MS)
                }) {
                    return true;
                }
                deadline.saturating_sub(now_b) <= LIVENESS_EVIDENCE_LEAD_MS
            }
        }
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

    /// A complete authenticated join/final activation starts one finite cycle.
    /// Ordinary transport activity and intermediate resume frames cannot call
    /// this constructor or reset the frozen cutoff of an existing watchdog.
    pub(crate) fn authenticated_cycle(
        liveness_epoch_a: impl Into<String>,
        now_b: u64,
    ) -> Result<Self> {
        let mut watchdog = Self::new(liveness_epoch_a)?;
        watchdog.deadline_b = Some(
            now_b
                .checked_add(30_000)
                .ok_or_else(|| LivenessError::new("deadline_overflow", "candidate_deadline"))?,
        );
        watchdog.last_advanced_at_b = Some(now_b);
        Ok(watchdog)
    }

    /// Called only at the first real transport interruption. A missing or
    /// expired authenticated proof grants no recovery budget. Repeated calls
    /// and later authenticated proofs may never extend this cycle's cutoff.
    pub(crate) fn freeze_deadline(&mut self, now_b: u64) -> Option<u64> {
        let deadline = self.deadline_b.filter(|deadline| now_b < *deadline)?;
        let frozen = self.frozen_deadline_b.get_or_insert(deadline);
        *frozen = (*frozen).min(deadline);
        Some(*frozen)
    }

    fn apply_evidence(
        &mut self,
        sequence: u64,
        candidate_deadline: u64,
        received_at_b: u64,
    ) -> Result<LivenessDeadlineUpdate> {
        if self
            .deadline_b
            .is_some_and(|deadline| received_at_b >= deadline)
            || self
                .frozen_deadline_b
                .is_some_and(|deadline| received_at_b >= deadline)
        {
            return Err(LivenessError::new("expired", "candidate_deadline"));
        }
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
        let deadline = apply_liveness_deadline(previous, candidate_deadline, policy);
        self.deadline_b = Some(
            self.frozen_deadline_b
                .map_or(deadline, |cutoff| deadline.min(cutoff)),
        );
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

/// A/B 心跳只更新 Relay 对相应认证 socket 的记录，不直接续期对端预算。
/// 序号必须单调递增且不能为 0，epoch 来自
/// `controller_joined` 下发的租约身份。
pub(crate) fn build_controller_liveness(
    lease: &LeaseContext,
    role: super::protocol::SenderRole,
    liveness_epoch: &str,
    heartbeat_sequence: u64,
) -> Result<serde_json::Value> {
    let sender_role = match role {
        super::protocol::SenderRole::ControllerA => "A",
        super::protocol::SenderRole::ControlledB => "B",
        _ => return Err(LivenessError::new("invalid_role", "controller_liveness")),
    };
    if heartbeat_sequence == 0 || !is_valid_liveness_epoch(liveness_epoch) {
        return Err(LivenessError::new("invalid_field", "controller_liveness"));
    }
    Ok(serde_json::json!({
        "v": 1,
        "type": "controller_liveness",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "sender_role": sender_role,
        "liveness_epoch": liveness_epoch,
        "heartbeat_sequence": heartbeat_sequence.to_string(),
    }))
}

fn is_valid_liveness_epoch(value: &str) -> bool {
    super::protocol::is_valid_identifier(value)
}

/// E2E-06：B → Relay 的存活挑战（`controller_liveness_challenge_a`）。Relay 会把
/// 它转给 A 直接应答，同时把它登记为「允许为该挑战签发 attestation」的凭据。
pub(crate) fn build_liveness_challenge(
    lease: &LeaseContext,
    request_id: &str,
    challenge_id: &str,
    challenge_b64: &str,
) -> Result<serde_json::Value> {
    if !is_valid_liveness_epoch(request_id)
        || !is_valid_liveness_epoch(challenge_id)
        || challenge_b64.is_empty()
        || challenge_b64.len() > MAX_LIVENESS_ID_BYTES
    {
        return Err(LivenessError::new(
            "invalid_field",
            "controller_liveness_challenge_a",
        ));
    }
    Ok(serde_json::json!({
        "v": 1,
        "type": "controller_liveness_challenge_a",
        "request_id": request_id,
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "challenge_id": challenge_id,
        "challenge_b64": challenge_b64,
    }))
}

/// E2E-06：B → Relay 的 attestation 兜底请求。必须复用同一个 challenge（Relay
/// 只对已登记的挑战签发签名证明）。
pub(crate) fn build_liveness_attestation_request(
    lease: &LeaseContext,
    request_id: &str,
    challenge_id: &str,
    challenge_b64: &str,
) -> Result<serde_json::Value> {
    let mut value = build_liveness_challenge(lease, request_id, challenge_id, challenge_b64)?;
    value["type"] = serde_json::json!("controller_liveness_attestation_request_a");
    Ok(value)
}

/// E2E-06：生成一次挑战的标识与随机挑战值（16 字节标准 base64）。
pub(crate) fn new_liveness_challenge() -> Result<(String, String)> {
    use base64::Engine as _;
    use ring::rand::{SecureRandom as _, SystemRandom};

    let mut bytes = [0_u8; 16];
    SystemRandom::new()
        .fill(&mut bytes)
        .map_err(|_| LivenessError::new("challenge_generation_failed", "challenge_b64"))?;
    Ok((
        format!("challenge-{}", uuid::Uuid::new_v4().simple()),
        base64::engine::general_purpose::STANDARD.encode(bytes),
    ))
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

pub(crate) fn validate_liveness_epoch(value: &str) -> Result<()> {
    validate_identifier(value, "liveness_epoch_a")
}

fn validate_identifier(value: &str, field: &'static str) -> Result<()> {
    if !super::protocol::is_valid_identifier(value) {
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
    fn cancelled_challenge_releases_pending_slot_without_marking_replay() {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        watchdog
            .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 1_000)
            .unwrap();
        watchdog.cancel_challenge("challenge-0001");
        watchdog
            .register_challenge("challenge-0001", "AQEBAQEBAQEBAQEBAQEBAQ==", 2_000)
            .unwrap();
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
    fn first_disconnect_freezes_authenticated_evidence_until_a_complete_new_cycle() {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        assert_eq!(watchdog.freeze_deadline(1_000), None);
        watchdog
            .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 6_000)
            .unwrap();
        watchdog
            .accept_direct_json(
                &direct_payload(1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA=="),
                &lease(),
                8_000,
            )
            .unwrap();
        assert_eq!(watchdog.freeze_deadline(20_000), Some(35_000));
        watchdog
            .register_challenge("challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ==", 21_000)
            .unwrap();
        watchdog
            .accept_direct_json(
                &direct_payload(2, "challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ=="),
                &lease(),
                22_000,
            )
            .unwrap();
        assert_eq!(watchdog.deadline_b(), Some(35_000));
        assert_eq!(watchdog.freeze_deadline(23_000), Some(35_000));
        watchdog
            .register_challenge("challenge-0003", "AgICAgICAgICAgICAgICAg==", 34_000)
            .unwrap();
        assert!(
            watchdog
                .accept_direct_json(
                    &direct_payload(3, "challenge-0003", "AgICAgICAgICAgICAgICAg=="),
                    &lease(),
                    35_000
                )
                .is_err()
        );
        assert_eq!(watchdog.deadline_b(), Some(35_000));
        assert_eq!(watchdog.state(35_000), LivenessWatchdogState::Expired);
        let next = ControllerLivenessWatchdog::authenticated_cycle("live-a-2", 36_000).unwrap();
        assert_eq!(next.deadline_b(), Some(66_000));
        assert!(ControllerLivenessWatchdog::authenticated_cycle("live-a-2", u64::MAX).is_err());
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
    fn fresh_pending_challenge_prevents_false_suspect_without_extending_deadline() {
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
        let original_deadline = watchdog.deadline_b();

        watchdog
            .register_challenge("challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ==", 16_000)
            .unwrap();
        assert_eq!(watchdog.state(16_000), LivenessWatchdogState::Healthy);
        assert_eq!(watchdog.deadline_b(), original_deadline);
        assert_eq!(watchdog.state(21_001), LivenessWatchdogState::Suspect);
        assert_eq!(watchdog.deadline_b(), original_deadline);
        assert_eq!(watchdog.state(30_000), LivenessWatchdogState::Expired);
    }

    #[test]
    fn long_session_reclaims_history_but_keeps_epoch_sequence_floors() {
        let nonce = "AAAAAAAAAAAAAAAAAAAAAA==";
        for attested in [false, true] {
            let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
            for sequence in 1..=128 {
                let sent = sequence * TEST_EVIDENCE_INTERVAL_MS;
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
            let now = 129 * TEST_EVIDENCE_INTERVAL_MS;
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
    fn liveness_identifiers_follow_the_frozen_wire_grammar() {
        for value in ["live:a-2", "live.a_2-3", "A"] {
            assert!(super::super::protocol::is_valid_identifier(value));
            assert!(validate_liveness_epoch(value).is_ok());
            assert!(ControllerLivenessWatchdog::authenticated_cycle(value, 1_000).is_ok());
        }
        for value in ["live~a-2", ":live-a-2", "live a-2", ""] {
            assert!(!super::super::protocol::is_valid_identifier(value));
            assert!(validate_liveness_epoch(value).is_err());
            assert!(ControllerLivenessWatchdog::authenticated_cycle(value, 1_000).is_err());
        }
        let mut watchdog = ControllerLivenessWatchdog::new("live:a-1").unwrap();
        watchdog.install_epoch("live:a-2").unwrap();
        watchdog
            .register_challenge("challenge:b-1", "AAAAAAAAAAAAAAAAAAAAAA==", 1_000)
            .unwrap();
        assert!(
            watchdog
                .register_challenge("challenge~b-2", "AQEBAQEBAQEBAQEBAQEBAQ==", 2_000)
                .is_err()
        );
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

    /// E2E-06：A 端心跳帧必须通过冻结校验（角色 A + role_liveness_v1），序号单调
    /// 且 epoch 合法；空 epoch 或 0 序号失败关闭。
    #[test]
    fn a_side_liveness_frame_matches_the_frozen_contract() {
        use super::super::protocol::{
            RuntimeValidationContext, SenderRole, validate_controller_message_json,
            validate_controller_runtime_message_json,
        };

        let lease = lease();
        let frame =
            build_controller_liveness(&lease, SenderRole::ControllerA, "live-a-1", 1).unwrap();
        validate_controller_message_json(&frame.to_string(), SenderRole::ControllerA).unwrap();
        validate_controller_runtime_message_json(
            &frame.to_string(),
            SenderRole::ControllerA,
            Some(&RuntimeValidationContext {
                lease: Some(lease.clone()),
                negotiated_capabilities: vec![
                    "encrypted_frames_v1".to_owned(),
                    "pairing_v1".to_owned(),
                    "role_liveness_v1".to_owned(),
                    "task_v1".to_owned(),
                ],
            }),
        )
        .expect("心跳帧必须通过运行时校验");
        assert_eq!(frame["sender_role"], "A");
        assert_eq!(frame["heartbeat_sequence"], "1");
        assert_eq!(frame["liveness_epoch"], "live-a-1");
        let b_frame =
            build_controller_liveness(&lease, SenderRole::ControlledB, "live-b-1", 1).unwrap();
        validate_controller_runtime_message_json(
            &b_frame.to_string(),
            SenderRole::ControlledB,
            Some(&RuntimeValidationContext {
                lease: Some(lease.clone()),
                negotiated_capabilities: vec!["role_liveness_v1".into()],
            }),
        )
        .unwrap();
        assert_eq!(b_frame["sender_role"], "B");
        assert!(build_controller_liveness(&lease, SenderRole::Relay, "live-a-1", 1).is_err());

        assert!(build_controller_liveness(&lease, SenderRole::ControllerA, "live-a-1", 0).is_err());
        assert!(build_controller_liveness(&lease, SenderRole::ControllerA, "", 1).is_err());
        assert!(build_controller_liveness(&lease, SenderRole::ControllerA, "live a", 1).is_err());
    }

    /// E2E-06：B 端提前索证策略——刚收到证据时为假，3 秒后即使仍 healthy
    /// 且 deadline 余量充足也为真；触发索证本身不推进证据截止。
    #[test]
    fn evidence_is_requested_only_when_the_budget_needs_it() {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        assert!(watchdog.needs_evidence(0), "从未拿到证据时必须索取");

        watchdog
            .register_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 0)
            .unwrap();
        watchdog
            .accept_direct_json(
                &direct_payload(1, "challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA=="),
                &lease(),
                0,
            )
            .unwrap();
        assert!(
            !watchdog.needs_evidence(0),
            "刚续期且余量充足时不必打扰对端"
        );
        assert!(!watchdog.needs_evidence(2_999));
        assert!(watchdog.needs_evidence(3_000));
        assert_eq!(SUSPECT_EVIDENCE_LEAD_MS, 12_000);
        assert_eq!(watchdog.state(3_000), LivenessWatchdogState::Healthy);
        assert_eq!(
            watchdog.deadline_b(),
            Some(29_000),
            "earlier requests cannot renew the deadline"
        );
        assert!(
            watchdog.needs_evidence(25_000),
            "距 deadline 只剩提前量时必须索取"
        );
        // 黑洞（长时间没有推进）同样触发。
        watchdog
            .register_challenge("challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ==", 20_000)
            .unwrap();
        assert!(watchdog.needs_evidence(20_000));
    }

    #[test]
    fn fallback_accepts_full_request_rtt_after_direct_wait_without_retimestamping_challenge() {
        let mut watchdog = ControllerLivenessWatchdog::new("live-a-1").unwrap();
        let nonce = "AAAAAAAAAAAAAAAAAAAAAA==";
        watchdog
            .register_challenge("challenge-0001", nonce, 0)
            .unwrap();
        let payload = attestation_payload(1, 1, "challenge-0001", nonce);
        assert_eq!(
            watchdog
                .accept_attestation_json(&payload, &lease(), 0, 8_000)
                .unwrap_err()
                .code,
            "rtt_exceeded"
        );
        watchdog
            .accept_attestation_json(&payload, &lease(), 4_000, 9_000)
            .unwrap();
        assert_eq!(watchdog.deadline_b(), Some(28_000));
    }

    /// E2E-06：挑战与 attestation 兜底请求必须通过冻结校验（角色 B +
    /// role_liveness_v1），并拒绝畸形标识/挑战值。
    #[test]
    fn b_side_evidence_frames_match_the_frozen_contract() {
        use super::super::protocol::{
            RuntimeValidationContext, SenderRole, validate_controller_message_json,
            validate_controller_runtime_message_json,
        };
        use base64::Engine as _;

        let lease = lease();
        let (challenge_id, challenge_b64) = new_liveness_challenge().unwrap();
        assert!(challenge_id.starts_with("challenge-"));
        assert_eq!(
            base64::engine::general_purpose::STANDARD
                .decode(&challenge_b64)
                .unwrap()
                .len(),
            16
        );
        let (other_id, other_b64) = new_liveness_challenge().unwrap();
        assert_ne!(
            (challenge_id.clone(), challenge_b64.clone()),
            (other_id, other_b64)
        );

        let context = RuntimeValidationContext {
            lease: Some(lease.clone()),
            negotiated_capabilities: vec![
                "encrypted_frames_v1".to_owned(),
                "pairing_v1".to_owned(),
                "role_liveness_v1".to_owned(),
                "task_v1".to_owned(),
            ],
        };
        for frame in [
            build_liveness_challenge(&lease, "request-live-1", &challenge_id, &challenge_b64)
                .unwrap(),
            build_liveness_attestation_request(
                &lease,
                "request-live-2",
                &challenge_id,
                &challenge_b64,
            )
            .unwrap(),
        ] {
            validate_controller_message_json(&frame.to_string(), SenderRole::ControlledB).unwrap();
            validate_controller_runtime_message_json(
                &frame.to_string(),
                SenderRole::ControlledB,
                Some(&context),
            )
            .expect("B 端存活证据帧必须通过运行时校验");
        }
        assert!(build_liveness_challenge(&lease, "request-live-3", "", &challenge_b64).is_err());
        assert!(
            build_liveness_attestation_request(&lease, "request-live-4", &challenge_id, "")
                .is_err()
        );
    }
}
