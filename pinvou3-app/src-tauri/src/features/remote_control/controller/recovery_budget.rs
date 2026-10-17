//! A's finite local liveness cycle, renewed only by pinned Relay proof of B.
//! Local TX admission and Relay wall timestamps never grant peer lifetime.

use std::time::Duration;

use serde_json::{Value, json};

use super::crypto::{AuthenticatedRelayTrustConfig, authenticate_relay_liveness_attestation};
use super::liveness::{
    MAX_LIVENESS_RTT_MS, attestation_candidate_deadline, new_liveness_challenge,
};
use super::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_runtime_message_json,
};

const TOTAL_BUDGET: Duration = Duration::from_secs(30);

#[derive(Debug)]
pub(crate) struct LivenessBudget {
    cutoff: Option<u64>,
    frozen: bool,
    expires_at: tokio::time::Instant,
    pending: Option<PeerEvidenceRequest>,
    heartbeat_sequence_b: u64,
    attestation_sequence: u64,
}

#[derive(Debug)]
struct PeerEvidenceRequest {
    lease: LeaseContext,
    epoch_b: String,
    request_id: String,
    challenge_id: String,
    challenge_b64: String,
    sent_at_a: u64,
}

impl LivenessBudget {
    pub(crate) fn authenticated_cycle(now_a: u64) -> Self {
        let cutoff = now_a.checked_add(TOTAL_BUDGET.as_millis() as u64);
        Self {
            cutoff,
            frozen: false,
            pending: None,
            heartbeat_sequence_b: 0,
            attestation_sequence: 0,
            expires_at: tokio::time::Instant::now()
                + if cutoff.is_some() {
                    TOTAL_BUDGET
                } else {
                    Duration::ZERO
                },
        }
    }

    pub(crate) fn request_peer_evidence(
        &mut self,
        lease: &LeaseContext,
        epoch_b: &str,
        now_a: u64,
    ) -> Option<Value> {
        if self.frozen || !self.cutoff.is_some_and(|cutoff| now_a < cutoff) {
            return None;
        }
        if let Some(pending) = &self.pending {
            if now_a.checked_sub(pending.sent_at_a)? <= MAX_LIVENESS_RTT_MS {
                return None;
            }
        }
        let (challenge_id, challenge_b64) = new_liveness_challenge().ok()?;
        let request_id = format!("request-peer-evidence-{}", uuid::Uuid::new_v4().simple());
        let frame = json!({
            "v": 1, "type": "controller_liveness_attestation_request_b",
            "request_id": request_id, "connection_id": lease.connection_id,
            "controller_lease_id": lease.controller_lease_id, "lease_epoch": lease.lease_epoch,
            "relay_epoch": lease.relay_epoch, "remote_control_generation": lease.remote_control_generation,
            "challenge_id": challenge_id, "challenge_b64": challenge_b64,
        });
        self.pending = Some(PeerEvidenceRequest {
            lease: lease.clone(),
            epoch_b: epoch_b.to_owned(),
            request_id,
            challenge_id,
            challenge_b64,
            sent_at_a: now_a,
        });
        Some(frame)
    }

    pub(crate) fn cancel_peer_request(&mut self) {
        self.pending = None;
    }

    /// Relay errors have their own request domain. An exact pending proof
    /// rejection only consumes that request; it never changes the cutoff or
    /// fails an unrelated capability/journal operation.
    pub(crate) fn consume_peer_error(&mut self, input: &str) -> bool {
        let Ok(frame) = super::protocol::validate_controller_message_json(input, SenderRole::Relay)
        else {
            return false;
        };
        if frame["type"] != "error"
            || !self
                .pending
                .as_ref()
                .is_some_and(|pending| frame["request_id"] == pending.request_id)
        {
            return false;
        }
        self.pending = None;
        true
    }

    pub(crate) fn accept_peer_evidence(
        &mut self,
        input: &str,
        trust: &AuthenticatedRelayTrustConfig,
        runtime: &RuntimeValidationContext,
        epoch_b: &str,
        now_a: u64,
        received_at: tokio::time::Instant,
    ) -> Result<(), &'static str> {
        if self.frozen || !self.cutoff.is_some_and(|cutoff| now_a < cutoff) {
            return Err("expired");
        }
        let pending = self.pending.as_ref().ok_or("unknown_challenge")?;
        if runtime.lease.as_ref() != Some(&pending.lease) || epoch_b != pending.epoch_b {
            return Err("stale_context");
        }
        let frame =
            validate_controller_runtime_message_json(input, SenderRole::Relay, Some(runtime))
                .map_err(|error| error.code)?;
        if frame["type"] != "controller_liveness_attestation_b"
            || frame["liveness_epoch_b"] != pending.epoch_b
            || frame["request_id"] != pending.request_id
            || frame["challenge_id"] != pending.challenge_id
            || frame["challenge_b64"] != pending.challenge_b64
        {
            return Err("stale_context");
        }
        authenticate_relay_liveness_attestation(input, trust).map_err(|error| error.code)?;
        let counter = |field: &str| {
            frame[field]
                .as_str()
                .and_then(|value| value.parse::<u64>().ok())
                .ok_or("invalid_field")
        };
        let sequence = counter("heartbeat_sequence_b")?;
        let attestation = counter("attestation_sequence")?;
        if sequence == 0
            || sequence < self.heartbeat_sequence_b
            || attestation <= self.attestation_sequence
        {
            return Err("replay");
        }
        let rtt = now_a
            .checked_sub(pending.sent_at_a)
            .ok_or("invalid_clock")?;
        let candidate = attestation_candidate_deadline(
            now_a,
            counter("observed_age_ms")?,
            counter("remaining_ttl_ms")?,
            rtt,
        )
        .map_err(|error| error.code)?;
        if candidate <= now_a {
            return Err("expired");
        }
        let cutoff = if sequence > self.heartbeat_sequence_b {
            candidate
        } else {
            self.cutoff.ok_or("expired")?.min(candidate)
        };
        self.cutoff = Some(cutoff);
        self.expires_at = received_at + Duration::from_millis(cutoff - now_a);
        self.heartbeat_sequence_b = sequence;
        self.attestation_sequence = attestation;
        self.pending = None;
        Ok(())
    }

    pub(crate) fn freeze(&mut self, now_a: u64) -> Option<u64> {
        self.frozen = true;
        self.pending = None;
        self.cutoff.filter(|cutoff| now_a < *cutoff)
    }

    pub(crate) fn timer(&self) -> tokio::time::Instant {
        self.expires_at
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-test".into(),
            controller_lease_id: "lease-test".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-test".into(),
            remote_control_generation: "generation-test".into(),
        }
    }

    #[tokio::test(start_paused = true)]
    async fn only_an_exact_strict_peer_error_consumes_pending_without_touching_budget() {
        let mut budget = LivenessBudget::authenticated_cycle(1_000);
        let request = budget
            .request_peer_evidence(&test_lease(), "live-b-1", 1_001)
            .unwrap();
        let timer = budget.timer();
        let cutoff = budget.cutoff;
        let mut error = json!({"v": 1, "type": "error", "request_id": "another-request", "code": "invalid_message", "message": "invalid message"});
        assert!(!budget.consume_peer_error(&error.to_string()));
        error["request_id"] = request["request_id"].clone();
        error["unexpected"] = json!(true);
        assert!(!budget.consume_peer_error(&error.to_string()));
        error.as_object_mut().unwrap().remove("unexpected");
        assert!(budget.consume_peer_error(&error.to_string()));
        assert!(!budget.consume_peer_error(&error.to_string()));
        assert_eq!(budget.cutoff, cutoff);
        assert_eq!(budget.timer(), timer);
        assert!(
            budget
                .request_peer_evidence(&test_lease(), "live-b-1", 1_002)
                .is_some()
        );
    }

    fn signed_proof(request: &Value, heartbeat: u64, attestation: u64, age: u64) -> Value {
        let key = ring::signature::Ed25519KeyPair::from_seed_unchecked(&[42; 32]).unwrap();
        let mut proof = request.clone();
        proof["type"] = json!("controller_liveness_attestation_b");
        proof["liveness_epoch_b"] = json!("live-b-1");
        proof["heartbeat_sequence_b"] = json!(heartbeat.to_string());
        proof["attestation_sequence"] = json!(attestation.to_string());
        proof["observed_age_ms"] = json!(age.to_string());
        proof["remaining_ttl_ms"] = json!((30_000_u64.saturating_sub(age)).to_string());
        proof["issued_at_relay"] = json!("999999999");
        proof["relay_signing_key_id"] = json!("relay-key-peer-fixture");
        proof["signature_algorithm"] = json!("ed25519");
        resign(&mut proof, &key);
        proof
    }

    fn resign(proof: &mut Value, key: &ring::signature::Ed25519KeyPair) {
        use base64::Engine as _;
        let preimage =
            super::super::protocol::build_peer_liveness_attestation_preimage(proof).unwrap();
        proof["relay_signature_b64"] =
            json!(base64::engine::general_purpose::STANDARD.encode(key.sign(&preimage).as_ref()));
    }

    fn trust() -> AuthenticatedRelayTrustConfig {
        use ring::signature::KeyPair as _;
        let key = ring::signature::Ed25519KeyPair::from_seed_unchecked(&[42; 32]).unwrap();
        AuthenticatedRelayTrustConfig::from_test_enrollment([(
            "relay-key-peer-fixture".into(),
            key.public_key().as_ref().try_into().unwrap(),
        )])
    }

    fn runtime() -> RuntimeValidationContext {
        RuntimeValidationContext {
            lease: Some(test_lease()),
            negotiated_capabilities: vec!["role_liveness_v1".into()],
        }
    }

    #[test]
    fn node_peer_b_golden_signature_is_authenticated_in_rust_and_direction_bound() {
        use base64::Engine as _;
        let fixture: Value = serde_json::from_str(include_str!(
            "../../../../../../remote-control-relay/test/fixtures/controller-protocol-v1.json"
        ))
        .unwrap();
        let vector = &fixture["peer_b_attestation_vector"];
        let preimage =
            super::super::protocol::build_peer_liveness_attestation_preimage(&vector["message"])
                .unwrap();
        assert_eq!(
            preimage
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<String>(),
            vector["preimage_hex"]
        );
        let public: [u8; 32] = base64::engine::general_purpose::STANDARD
            .decode(vector["public_key_b64"].as_str().unwrap())
            .unwrap()
            .try_into()
            .unwrap();
        let pin = AuthenticatedRelayTrustConfig::from_test_enrollment([(
            "relay-key-peer-fixture".into(),
            public,
        )]);
        authenticate_relay_liveness_attestation(&vector["message"].to_string(), &pin).unwrap();
        let mut wrong_domain = vector["message"].clone();
        wrong_domain["type"] = json!("controller_liveness_attestation_a");
        assert!(authenticate_relay_liveness_attestation(&wrong_domain.to_string(), &pin).is_err());
        for field in vector["message"]
            .as_object()
            .unwrap()
            .keys()
            .filter(|field| field.as_str() != "relay_signature_b64")
        {
            let mut changed = vector["message"].clone();
            changed[field] = if field == "v" {
                json!(2)
            } else {
                json!(format!("{}x", changed[field].as_str().unwrap()))
            };
            assert!(
                authenticate_relay_liveness_attestation(&changed.to_string(), &pin).is_err(),
                "{field}"
            );
        }
    }

    #[tokio::test(start_paused = true)]
    async fn signed_peer_evidence_new_sequence_renews_equal_only_tightens_and_disconnect_freezes() {
        let mut budget = LivenessBudget::authenticated_cycle(0);
        let request = budget
            .request_peer_evidence(&test_lease(), "live-b-1", 9_000)
            .unwrap();
        let proof = signed_proof(&request, 2, 1, 2_000);
        budget
            .accept_peer_evidence(
                &proof.to_string(),
                &trust(),
                &runtime(),
                "live-b-1",
                10_000,
                tokio::time::Instant::now(),
            )
            .unwrap();
        assert_eq!(budget.cutoff, Some(36_000));
        assert!(
            budget
                .accept_peer_evidence(
                    &proof.to_string(),
                    &trust(),
                    &runtime(),
                    "live-b-1",
                    10_001,
                    tokio::time::Instant::now()
                )
                .is_err()
        );
        let request = budget
            .request_peer_evidence(&test_lease(), "live-b-1", 12_000)
            .unwrap();
        let proof = signed_proof(&request, 2, 2, 0);
        budget
            .accept_peer_evidence(
                &proof.to_string(),
                &trust(),
                &runtime(),
                "live-b-1",
                13_000,
                tokio::time::Instant::now(),
            )
            .unwrap();
        assert_eq!(budget.cutoff, Some(36_000));
        let request = budget
            .request_peer_evidence(&test_lease(), "live-b-1", 14_000)
            .unwrap();
        let proof = signed_proof(&request, 1, 3, 0);
        assert_eq!(
            budget.accept_peer_evidence(
                &proof.to_string(),
                &trust(),
                &runtime(),
                "live-b-1",
                15_000,
                tokio::time::Instant::now()
            ),
            Err("replay")
        );
        assert_eq!(budget.cutoff, Some(36_000));
        let in_flight = signed_proof(&request, 3, 3, 0);
        assert_eq!(budget.freeze(15_000), Some(36_000));
        assert!(
            budget
                .accept_peer_evidence(
                    &in_flight.to_string(),
                    &trust(),
                    &runtime(),
                    "live-b-1",
                    15_001,
                    tokio::time::Instant::now()
                )
                .is_err()
        );
        assert_eq!(budget.freeze(20_000), Some(36_000));
        assert_eq!(budget.cutoff, Some(36_000));
    }

    #[tokio::test(start_paused = true)]
    async fn rejected_peer_proofs_leave_all_authority_unchanged() {
        let key = ring::signature::Ed25519KeyPair::from_seed_unchecked(&[42; 32]).unwrap();
        let mut budget = LivenessBudget::authenticated_cycle(0);
        let request = budget
            .request_peer_evidence(&test_lease(), "live-b-1", 10_000)
            .unwrap();
        let valid = signed_proof(&request, 1, 1, 0);
        let timer = budget.timer();
        for (field, bad) in [
            ("connection_id", "other"),
            ("controller_lease_id", "other"),
            ("lease_epoch", "2"),
            ("relay_epoch", "other"),
            ("remote_control_generation", "other"),
            ("liveness_epoch_b", "old-b"),
            ("request_id", "other"),
            ("challenge_id", "other"),
            ("challenge_b64", "AQEBAQEBAQEBAQEBAQEBAQ=="),
            ("heartbeat_sequence_b", "0"),
            ("attestation_sequence", "0"),
            ("observed_age_ms", "5001"),
            ("remaining_ttl_ms", "0"),
            ("remaining_ttl_ms", "30001"),
            ("relay_signing_key_id", "untrusted"),
        ] {
            let mut proof = valid.clone();
            proof[field] = json!(bad);
            resign(&mut proof, &key);
            assert!(
                budget
                    .accept_peer_evidence(
                        &proof.to_string(),
                        &trust(),
                        &runtime(),
                        "live-b-1",
                        11_000,
                        tokio::time::Instant::now()
                    )
                    .is_err(),
                "{field}"
            );
            assert_eq!(budget.cutoff, Some(30_000));
            assert_eq!(budget.timer(), timer);
            assert_eq!(
                (budget.heartbeat_sequence_b, budget.attestation_sequence),
                (0, 0)
            );
            assert_eq!(
                budget.pending.as_ref().unwrap().challenge_id,
                request["challenge_id"]
            );
        }
        for now_a in [9_999, 15_001, 30_000] {
            assert!(
                budget
                    .accept_peer_evidence(
                        &valid.to_string(),
                        &trust(),
                        &runtime(),
                        "live-b-1",
                        now_a,
                        tokio::time::Instant::now()
                    )
                    .is_err()
            );
            assert_eq!(budget.cutoff, Some(30_000));
        }
        let mut unsupported = runtime();
        unsupported.negotiated_capabilities.clear();
        assert!(
            budget
                .accept_peer_evidence(
                    &valid.to_string(),
                    &trust(),
                    &unsupported,
                    "live-b-1",
                    11_000,
                    tokio::time::Instant::now()
                )
                .is_err()
        );
        budget
            .accept_peer_evidence(
                &valid.to_string(),
                &trust(),
                &runtime(),
                "live-b-1",
                11_000,
                tokio::time::Instant::now(),
            )
            .unwrap();
        assert_eq!(budget.cutoff, Some(39_000));
    }

    #[tokio::test(start_paused = true)]
    async fn response_timer_is_receive_anchored_and_pending_challenge_is_bounded() {
        let mut budget = LivenessBudget::authenticated_cycle(0);
        let old = budget
            .request_peer_evidence(&test_lease(), "live-b-1", 1_000)
            .unwrap();
        assert!(
            budget
                .request_peer_evidence(&test_lease(), "live-b-1", 6_000)
                .is_none()
        );
        let request = budget
            .request_peer_evidence(&test_lease(), "live-b-1", 6_001)
            .unwrap();
        assert_ne!(old["challenge_id"], request["challenge_id"]);
        assert!(
            budget
                .accept_peer_evidence(
                    &signed_proof(&old, 1, 1, 0).to_string(),
                    &trust(),
                    &runtime(),
                    "live-b-1",
                    7_000,
                    tokio::time::Instant::now()
                )
                .is_err()
        );
        let received = tokio::time::Instant::now();
        tokio::time::advance(Duration::from_secs(2)).await;
        budget
            .accept_peer_evidence(
                &signed_proof(&request, 1, 1, 5_000).to_string(),
                &trust(),
                &runtime(),
                "live-b-1",
                11_001,
                received,
            )
            .unwrap();
        assert_eq!(budget.cutoff, Some(30_001));
        assert_eq!(budget.timer(), received + Duration::from_millis(19_000));
        assert_eq!(
            budget.timer() - tokio::time::Instant::now(),
            Duration::from_secs(17)
        );
    }

    #[tokio::test(start_paused = true)]
    async fn first_disconnect_keeps_only_remaining_budget_even_after_retries_and_new_heartbeats() {
        let mut budget = LivenessBudget::authenticated_cycle(2_000);
        let deadline = budget.timer();
        tokio::time::advance(Duration::from_secs(15)).await;
        assert_eq!(budget.freeze(17_000), Some(32_000));
        assert_eq!(
            deadline - tokio::time::Instant::now(),
            Duration::from_secs(15)
        );
        tokio::time::advance(Duration::from_secs(5)).await;
        assert!(
            budget
                .request_peer_evidence(&test_lease(), "live-b-1", 22_000)
                .is_none()
        );
        assert_eq!(budget.freeze(22_000), Some(32_000));
        assert_eq!(budget.timer(), deadline);
        assert_eq!(
            budget.timer() - tokio::time::Instant::now(),
            Duration::from_secs(10)
        );
        tokio::time::advance(Duration::from_secs(10)).await;
        assert_eq!(budget.freeze(32_000), None);
        assert!(budget.timer() <= tokio::time::Instant::now());
    }

    #[tokio::test(start_paused = true)]
    async fn own_requests_and_expired_evidence_cannot_renew_but_complete_activation_can() {
        let mut budget = LivenessBudget::authenticated_cycle(2_000);
        let first = budget.timer();
        tokio::time::advance(Duration::from_secs(8)).await;
        budget
            .request_peer_evidence(&test_lease(), "live-b-1", 10_000)
            .unwrap();
        budget.cancel_peer_request();
        assert_eq!(budget.timer(), first);
        assert_eq!(budget.cutoff, Some(32_000));
        budget
            .request_peer_evidence(&test_lease(), "live-b-1", 10_000)
            .unwrap();
        assert_eq!(budget.cutoff, Some(32_000));
        assert_eq!(budget.timer(), first);
        tokio::time::advance(TOTAL_BUDGET).await;
        assert!(
            budget
                .request_peer_evidence(&test_lease(), "live-b-1", 40_000)
                .is_none()
        );
        assert_eq!(budget.cutoff, Some(32_000));
        assert_eq!(budget.freeze(40_000), None);
        let new_cycle = LivenessBudget::authenticated_cycle(40_000);
        assert_eq!(new_cycle.cutoff, Some(70_000));
        assert_eq!(
            new_cycle.timer() - tokio::time::Instant::now(),
            TOTAL_BUDGET
        );
        let overflow = LivenessBudget::authenticated_cycle(u64::MAX);
        assert_eq!(overflow.cutoff, None);
        assert!(overflow.timer() <= tokio::time::Instant::now());
    }
}
