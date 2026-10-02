//! B 端握手对称实现（E2E-02）。
//!
//! 与 A 端 `controlling_handshake` 使用同一份签名 transcript 与同一套
//! commitment/receipt 规则，只是角色相反：B 用 transcript 里的 A 公钥做
//! ECDH，用 `ControllerRole::B` 派生收据密钥。relay 会把
//! `controller_pairing_reserved` 同时投递给 A 与 B（controller-router.js:161）。

use base64::Engine as _;
use serde_json::{Value, json};

use super::controlling_handshake::{JoinedLease, validate_joined_lease};
use super::crypto::{
    AuthenticatedRelayTrustConfig, ControllerRole, FinishedReceiver, FinishedSender,
    HandshakeBinding, HandshakeKeyPair, HandshakePublicKey, ReceiptSecret, SessionKeys,
    authenticate_relay_transcript,
};
use super::protocol::{
    SenderRole, parse_controller_pairing_transcript, validate_controller_message_json,
};

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ControlledHandshakeError {
    pub(crate) code: &'static str,
}

impl ControlledHandshakeError {
    fn new(code: &'static str) -> Self {
        Self { code }
    }
}

type Result<T> = std::result::Result<T, ControlledHandshakeError>;

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum ControlledHandshakeStage {
    Committed,
    PeerCommitted,
    Finished,
    Acked,
    Joined,
}

pub(crate) struct ControlledHandshake {
    request_id: String,
    pairing_id: String,
    reservation_id: String,
    relay_epoch: String,
    generation: String,
    transcript_hash_hex: String,
    transcript_hash: [u8; 32],
    negotiated_capabilities: Vec<String>,
    binding: HandshakeBinding,
    finished_sender: Option<FinishedSender>,
    finished_receiver: Option<FinishedReceiver>,
    receipt: ReceiptSecret,
    session: Option<SessionKeys>,
    peer_commitment: Option<String>,
    stage: ControlledHandshakeStage,
}

impl ControlledHandshake {
    /// 用 Relay 下发的签名 transcript 初始化 B 端握手。
    pub(crate) fn new(
        reserved: &Value,
        handshake: HandshakeKeyPair,
        trust: &AuthenticatedRelayTrustConfig,
    ) -> Result<Self> {
        let parsed = parse_controller_pairing_transcript(&reserved.to_string())
            .map_err(|_| ControlledHandshakeError::new("controller_transcript_invalid"))?;
        let context = authenticate_relay_transcript(trust, parsed)
            .map_err(|_| ControlledHandshakeError::new("controller_transcript_untrusted"))?;
        let transcript_hash = context
            .transcript_hash_bytes()
            .map_err(|_| ControlledHandshakeError::new("controller_transcript_invalid"))?;
        let binding = HandshakeBinding::new(
            context.relay_epoch(),
            context.pairing_id(),
            context.reservation_id(),
            transcript_hash,
        )
        .map_err(|_| ControlledHandshakeError::new("controller_transcript_invalid"))?;
        // B 的“对端”是 A：transcript 里 a = 主控端，b = 被控端。
        let peer = decode_public_key(context.local_handshake_public_key())
            .ok_or_else(|| ControlledHandshakeError::new("controller_transcript_invalid"))?;
        let session = handshake
            .into_session_keys(peer, &binding)
            .map_err(|_| ControlledHandshakeError::new("controller_handshake_key_invalid"))?;
        let (finished_sender, finished_receiver) = session.finished_pair(ControllerRole::B);
        let receipt = ReceiptSecret::generate()
            .map_err(|_| ControlledHandshakeError::new("controller_receipt_generation_failed"))?;
        Ok(Self {
            request_id: reserved
                .get("request_id")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned(),
            pairing_id: context.pairing_id().to_owned(),
            reservation_id: context.reservation_id().to_owned(),
            relay_epoch: context.relay_epoch().to_owned(),
            generation: context.remote_control_generation().to_owned(),
            transcript_hash_hex: hex_lower(&transcript_hash),
            transcript_hash,
            negotiated_capabilities: context.negotiated_capabilities().to_vec(),
            binding,
            finished_sender: Some(finished_sender),
            finished_receiver: Some(finished_receiver),
            receipt,
            session: Some(session),
            peer_commitment: None,
            stage: ControlledHandshakeStage::Committed,
        })
    }

    pub(crate) fn stage(&self) -> ControlledHandshakeStage {
        self.stage
    }

    /// B 的 `finished_receipt_commit`。
    pub(crate) fn commit(&self) -> Result<Value> {
        let commitment = self
            .receipt
            .commitment(&self.binding, ControllerRole::B)
            .map_err(|_| ControlledHandshakeError::new("controller_commitment_failed"))?;
        self.envelope(
            "finished_receipt_commit",
            json!({ "sender_role": "B", "commitment": hex_lower(&commitment) }),
        )
    }

    /// 记录 A 的 commitment；只有它到达后才允许回送 B 的 Finished。
    pub(crate) fn accept_peer_commitment(&mut self, message: &Value) -> Result<()> {
        if self.stage != ControlledHandshakeStage::Committed {
            return Err(ControlledHandshakeError::new(
                "controller_handshake_stage_invalid",
            ));
        }
        self.assert_peer_envelope(message, "finished_receipt_commit", "A")?;
        let commitment = message
            .get("commitment")
            .and_then(Value::as_str)
            .filter(|value| is_lower_hex(value, 64))
            .ok_or_else(|| ControlledHandshakeError::new("controller_commit_invalid"))?
            .to_owned();
        self.peer_commitment = Some(commitment);
        self.stage = ControlledHandshakeStage::PeerCommitted;
        Ok(())
    }

    /// B 的 `encrypted_handshake_finished`。
    pub(crate) fn finished(&mut self) -> Result<Value> {
        if self.stage != ControlledHandshakeStage::PeerCommitted {
            return Err(ControlledHandshakeError::new(
                "controller_handshake_stage_invalid",
            ));
        }
        let sender = self
            .finished_sender
            .take()
            .ok_or_else(|| ControlledHandshakeError::new("controller_handshake_stage_invalid"))?;
        let sealed = sender
            .seal(&self.receipt)
            .map_err(|_| ControlledHandshakeError::new("controller_finished_seal_failed"))?;
        self.stage = ControlledHandshakeStage::Finished;
        self.envelope(
            "encrypted_handshake_finished",
            json!({
                "sender_role": "B",
                "ciphertext_b64": base64::engine::general_purpose::STANDARD.encode(&sealed),
                "ciphertext_bytes": sealed.len().to_string(),
            }),
        )
    }

    /// 打开 A 的 Finished，校验其 receipt 与 A 的 commitment 一致后产出 B 的
    /// `finished_receipt_ack`（receiver_role = 回执方 B，sender_role = A）。
    pub(crate) fn accept_peer_finished(&mut self, message: &Value) -> Result<Value> {
        if self.stage != ControlledHandshakeStage::Finished {
            return Err(ControlledHandshakeError::new(
                "controller_handshake_stage_invalid",
            ));
        }
        self.assert_peer_envelope(message, "encrypted_handshake_finished", "A")?;
        let ciphertext = base64::engine::general_purpose::STANDARD
            .decode(
                message
                    .get("ciphertext_b64")
                    .and_then(Value::as_str)
                    .ok_or_else(|| ControlledHandshakeError::new("controller_finished_invalid"))?,
            )
            .map_err(|_| ControlledHandshakeError::new("controller_finished_invalid"))?;
        let receiver = self
            .finished_receiver
            .take()
            .ok_or_else(|| ControlledHandshakeError::new("controller_handshake_stage_invalid"))?;
        let peer_receipt = receiver
            .open(&ciphertext)
            .map_err(|_| ControlledHandshakeError::new("controller_finished_invalid"))?;
        let commitment = peer_receipt
            .commitment(&self.binding, ControllerRole::A)
            .map_err(|_| ControlledHandshakeError::new("controller_commitment_failed"))?;
        if self.peer_commitment.as_deref() != Some(hex_lower(&commitment).as_str()) {
            return Err(ControlledHandshakeError::new(
                "controller_commitment_mismatch",
            ));
        }
        let ack = self.envelope(
            "finished_receipt_ack",
            json!({
                "sender_role": "A",
                "receiver_role": "B",
                // Relay 的收据校验契约（`controller-lease.js` 的 `acceptReceipt`）：
                // 帧里带的必须是 `sender_role` 那一方（即刚收到 Finished 的对端）
                // 的 receipt secret，Relay 用它去核对对端先前的 commitment。
                // 这里必须回传刚解开的 `peer_receipt`，不是本端自己的 receipt。
                "receipt_secret_b64": peer_receipt.to_base64(),
            }),
        )?;
        self.stage = ControlledHandshakeStage::Acked;
        Ok(ack)
    }

    /// 双收据完成后交出会话密钥，供调用方用真实 `LeaseBinding` 派生业务密钥。
    pub(crate) fn take_session(&mut self) -> Result<SessionKeys> {
        // `controller_joined` 之后 stage 前进为 Joined，密钥仍只交出一次。
        if !matches!(
            self.stage,
            ControlledHandshakeStage::Acked | ControlledHandshakeStage::Joined
        ) {
            return Err(ControlledHandshakeError::new(
                "controller_handshake_stage_invalid",
            ));
        }
        self.session
            .take()
            .ok_or_else(|| ControlledHandshakeError::new("controller_handshake_stage_invalid"))
    }

    /// 处理 Relay 的 `controller_joined`：双收据完成后才允许，返回租约身份与
    /// 恢复材料（与 A 端共用同一份校验与解析）。
    pub(crate) fn accept_joined(&mut self, message: &Value) -> Result<JoinedLease> {
        if self.stage != ControlledHandshakeStage::Acked {
            return Err(ControlledHandshakeError::new(
                "controller_handshake_stage_invalid",
            ));
        }
        let lease = validate_joined_lease(
            message,
            &self.pairing_id,
            &self.reservation_id,
            &self.relay_epoch,
            &self.transcript_hash_hex,
        )
        .map_err(ControlledHandshakeError::new)?;
        self.stage = ControlledHandshakeStage::Joined;
        Ok(lease)
    }

    /// 验签 transcript 的哈希原始字节（B 端派生传输指纹用）。
    pub(crate) fn transcript_hash(&self) -> [u8; 32] {
        self.transcript_hash
    }

    /// Relay 在 transcript 中协商出的能力集合（业务帧与回包的准入上下文）。
    pub(crate) fn negotiated_capabilities(&self) -> &[String] {
        &self.negotiated_capabilities
    }

    fn assert_peer_envelope(
        &self,
        message: &Value,
        kind: &'static str,
        role: &'static str,
    ) -> Result<()> {
        let validated =
            validate_controller_message_json(&message.to_string(), SenderRole::ControllerA)
                .map_err(|_| ControlledHandshakeError::new("controller_handshake_rejected"))?;
        if validated.get("type").and_then(Value::as_str) != Some(kind)
            || validated.get("sender_role").and_then(Value::as_str) != Some(role)
        {
            return Err(ControlledHandshakeError::new(
                "controller_handshake_rejected",
            ));
        }
        for (field, expected) in [
            ("pairing_id", self.pairing_id.as_str()),
            ("reservation_id", self.reservation_id.as_str()),
            ("relay_epoch", self.relay_epoch.as_str()),
            ("transcript_hash", self.transcript_hash_hex.as_str()),
        ] {
            if validated.get(field).and_then(Value::as_str) != Some(expected) {
                return Err(ControlledHandshakeError::new(
                    "controller_handshake_binding_mismatch",
                ));
            }
        }
        Ok(())
    }

    fn envelope(&self, kind: &'static str, extra: Value) -> Result<Value> {
        let mut object = json!({
            "v": 1,
            "type": kind,
            "request_id": self.request_id,
            "pairing_id": self.pairing_id,
            "reservation_id": self.reservation_id,
            "remote_control_generation": self.generation,
            "relay_epoch": self.relay_epoch,
            "transcript_hash": self.transcript_hash_hex,
        });
        if let Some(extra) = extra.as_object() {
            for (key, value) in extra {
                object[key.as_str()] = value.clone();
            }
        }
        validate_controller_message_json(&object.to_string(), SenderRole::ControlledB)
            .map_err(|_| ControlledHandshakeError::new("controller_message_invalid"))?;
        Ok(object)
    }
}

fn decode_public_key(encoded: &str) -> Option<HandshakePublicKey> {
    let raw = base64::engine::general_purpose::STANDARD
        .decode(encoded.as_bytes())
        .ok()?;
    let bytes: [u8; 32] = raw.try_into().ok()?;
    Some(HandshakePublicKey::from_bytes(bytes))
}

fn is_lower_hex(value: &str, length: usize) -> bool {
    value.len() == length
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn hex_lower(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        out.push(HEX[(byte >> 4) as usize] as char);
        out.push(HEX[(byte & 0x0f) as usize] as char);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use ring::rand::SystemRandom;
    use ring::signature::{Ed25519KeyPair, KeyPair};
    use sha2::{Digest, Sha256};

    use super::super::controlling_handshake::ControllingHandshake;
    use super::super::protocol::build_pairing_transcript_preimage;

    fn fixture() -> Value {
        serde_json::from_str(include_str!(
            "../../../../../../remote-control-relay/test/fixtures/controller-protocol-v1.json"
        ))
        .unwrap()
    }

    struct SignedRelay {
        trust: AuthenticatedRelayTrustConfig,
        key_id: String,
        key_pair: Ed25519KeyPair,
    }

    fn signed_relay() -> SignedRelay {
        let rng = SystemRandom::new();
        let pkcs8 = Ed25519KeyPair::generate_pkcs8(&rng).unwrap();
        let key_pair = Ed25519KeyPair::from_pkcs8(pkcs8.as_ref()).unwrap();
        let encoded =
            base64::engine::general_purpose::STANDARD.encode(key_pair.public_key().as_ref());
        let key_id = "relay-key-pair".to_owned();
        let trust =
            AuthenticatedRelayTrustConfig::from_pinned_keys([(key_id.clone(), encoded)]).unwrap();
        SignedRelay {
            trust,
            key_id,
            key_pair,
        }
    }

    fn signed_reservation(relay: &SignedRelay, a_public: &str, b_public: &str) -> Value {
        let mut message =
            fixture()["lease_install_vector"]["verification_input"]["reserved_message"].clone();
        let mut transcript = message["transcript"].clone();
        transcript["handshake_public_key_a_b64"] = json!(a_public);
        transcript["handshake_public_key_b_b64"] = json!(b_public);
        let preimage = build_pairing_transcript_preimage(&transcript).unwrap();
        message["transcript"] = transcript;
        message["transcript_hash"] = json!(hex_lower(&Sha256::digest(&preimage)));
        message["relay_signing_key_id"] = json!(relay.key_id);
        message["relay_signature_b64"] = json!(
            base64::engine::general_purpose::STANDARD
                .encode(relay.key_pair.sign(&preimage).as_ref())
        );
        message
    }

    /// E2E-02 双端契约：A 与 B 用同一签名 transcript 独立推进，只有双方都
    /// 收到有效 Finished 并各自产出 ack 之后，B 才交出会话密钥；A 在没有
    /// controller_joined 前始终拿不到租约材料。
    #[test]
    fn pair_handshake_completes_both_receipts_in_order() {
        let relay = signed_relay();
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let b_keys = HandshakeKeyPair::generate().unwrap();
        let a_public = a_keys.public_key().to_bytes();
        let b_public = b_keys.public_key().to_bytes();
        let encoded = |bytes: [u8; 32]| base64::engine::general_purpose::STANDARD.encode(bytes);
        let reserved = signed_reservation(&relay, &encoded(a_public), &encoded(b_public));

        let mut a = ControllingHandshake::new(
            reserved["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            relay.trust.clone(),
        );
        let a_commit = a.accept_reserved(&reserved).unwrap();

        let mut b = ControlledHandshake::new(&reserved, b_keys, &relay.trust).unwrap();
        let b_commit = b.commit().unwrap();
        // 顺序约束：B 在收到 A 的 commitment 之前不能回 Finished。
        assert_eq!(
            b.finished().unwrap_err().code,
            "controller_handshake_stage_invalid"
        );

        let a_finished = a.accept_peer_commitment(&b_commit).unwrap();
        b.accept_peer_commitment(&a_commit).unwrap();
        assert_eq!(
            a.accept_peer_commitment(&b_commit).unwrap_err().code,
            "controller_handshake_stage_invalid"
        );
        let b_finished = b.finished().unwrap();

        let a_ack = a.accept_peer_finished(&b_finished).unwrap();
        assert_eq!(
            a.stage(),
            super::super::controlling_handshake::HandshakeStage::FinishedSent
        );
        // 双收据完成后 B 才交出会话密钥；A 仍缺 controller_joined。
        assert_eq!(
            a.take_session().unwrap_err().code,
            "controller_handshake_stage_invalid"
        );
        let b_ack = b.accept_peer_finished(&a_finished).unwrap();
        assert_eq!(b.stage(), ControlledHandshakeStage::Acked);
        assert!(b.take_session().is_ok());
        assert_eq!(b_ack["sender_role"], json!("A"));
        assert_eq!(a_ack["sender_role"], json!("B"));
        assert_eq!(a_ack["receiver_role"], json!("A"));
    }

    #[test]
    fn untrusted_transcript_cannot_start_the_b_side_handshake() {
        let relay = signed_relay();
        let attacker = AuthenticatedRelayTrustConfig::from_pinned_keys([(
            "relay-key-other".to_owned(),
            base64::engine::general_purpose::STANDARD.encode([5_u8; 32]),
        )])
        .unwrap();
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let b_keys = HandshakeKeyPair::generate().unwrap();
        let encoded = |bytes: [u8; 32]| base64::engine::general_purpose::STANDARD.encode(bytes);
        let reserved = signed_reservation(
            &relay,
            &encoded(a_keys.public_key().to_bytes()),
            &encoded(b_keys.public_key().to_bytes()),
        );
        assert_eq!(
            ControlledHandshake::new(&reserved, b_keys, &attacker)
                .err()
                .expect("untrusted transcript must be rejected")
                .code,
            "controller_transcript_untrusted"
        );
    }
}
