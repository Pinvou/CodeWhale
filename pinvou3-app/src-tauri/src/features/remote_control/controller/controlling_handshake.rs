//! A 端 claim 与安全握手序列（E2E-02）。
//!
//! 本模块只负责「按冻结协议推进握手状态」：不拥有网络、不拥有连接状态。
//! 调用方（`desktop_remote_controller` 的 worker）把消息喂进来、把返回的
//! 消息发出去。顺序错乱、角色不符、绑定不匹配、验签失败一律失败关闭，
//! 且半握手不会产生任何可安装的租约材料。

use base64::Engine as _;
use serde_json::{Value, json};
use zeroize::Zeroize;

use super::crypto::{
    AuthenticatedRelayTrustConfig, ControllerRole, FinishedReceiver, FinishedSender,
    HandshakeBinding, HandshakeKeyPair, HandshakePublicKey, ReceiptSecret, SessionKeys,
    authenticate_relay_transcript,
};
use super::protocol::{
    SenderRole, parse_controller_pairing_transcript, validate_controller_message_json,
};

/// A 端 claim 声明的能力集合（与冻结协议常量保持一致的最小集）。
/// E2E-03 起声明 `task_v1`：A 端已能按冻结 schema 构造 capability/task 请求，
/// 且 B 端（DEV-12/DEV-13）已实现快照与任务服务；E2E-05 起声明
/// `snapshot_journal_v1`（A 独立历史 + B 权威 journal 已由 DEV-19 实现）。
/// `role_liveness_v1` / `resume_v1` 的实现接线属 E2E-06。
const CLAIM_CAPABILITIES: &[&str] = &[
    "pairing_v1",
    "encrypted_frames_v1",
    "finished_receipt_v1",
    "role_liveness_v1",
    "resume_v1",
    "task_v1",
    "temporary_model_package_v1",
    "snapshot_journal_v1",
];

/// A 端 claim 声明的能力集合（供「A claim 与 B 注册同族」的一致性测试使用）。
pub(crate) fn claim_capabilities() -> &'static [&'static str] {
    CLAIM_CAPABILITIES
}
const RESERVATION_TIMEOUT_MS: u64 = 30_000;

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct HandshakeError {
    pub(crate) code: &'static str,
}

impl HandshakeError {
    pub(crate) fn new(code: &'static str) -> Self {
        Self { code }
    }
}

type Result<T> = std::result::Result<T, HandshakeError>;

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum HandshakeStage {
    Claimed,
    Reserved,
    CommitSent,
    FinishedSent,
    Joined,
}

/// Relay 在 `controller_joined` 中下发的租约身份（A/B 共用）。业务密钥仍由
/// 调用方用 `into_role_ciphers` 在拿到真实 `LeaseBinding` 之后派生。
#[derive(Clone, Eq, PartialEq)]
pub(crate) struct JoinedLease {
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: u64,
    pub(crate) relay_epoch: String,
    pub(crate) remote_control_generation: String,
    pub(crate) liveness_epoch_a: String,
    pub(crate) resume_deadline_ms: u64,
    pub(crate) resume_token: [u8; RESUME_TOKEN_BYTES],
}

/// `resume_token` 是恢复凭证：Debug 只允许出现脱敏占位，绝不打印明文或
/// base64 形式。
impl std::fmt::Debug for JoinedLease {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("JoinedLease")
            .field("connection_id", &self.connection_id)
            .field("controller_lease_id", &self.controller_lease_id)
            .field("lease_epoch", &self.lease_epoch)
            .field("relay_epoch", &self.relay_epoch)
            .field("remote_control_generation", &self.remote_control_generation)
            .field("liveness_epoch_a", &self.liveness_epoch_a)
            .field("resume_deadline_ms", &self.resume_deadline_ms)
            .field("resume_token", &"[REDACTED]")
            .finish()
    }
}

/// `controller_joined` 的租约恢复凭证字节数（协议固定 32 字节）。
pub(crate) const RESUME_TOKEN_BYTES: usize = 32;

/// A/B 共用的 `controller_joined` 校验：先做 Relay 角色与形状校验，再逐项核对
/// 调用方已经认证过的 transcript 绑定，最后解析出租约身份与恢复材料。任何
/// 缺失、非规范整数、非 32 字节 token 或绑定不符都失败关闭，且不产生部分
/// 租约材料。
pub(crate) fn validate_joined_lease(
    message: &Value,
    pairing_id: &str,
    reservation_id: &str,
    relay_epoch: &str,
    transcript_hash_hex: &str,
) -> std::result::Result<JoinedLease, &'static str> {
    let validated = validate_controller_message_json(&message.to_string(), SenderRole::Relay)
        .map_err(|_| "controller_joined_invalid")?;
    if validated.get("type").and_then(Value::as_str) != Some("controller_joined") {
        return Err("controller_joined_invalid");
    }
    for (field, expected) in [
        ("pairing_id", pairing_id),
        ("reservation_id", reservation_id),
        ("relay_epoch", relay_epoch),
        ("transcript_hash", transcript_hash_hex),
    ] {
        if validated.get(field).and_then(Value::as_str) != Some(expected) {
            return Err("controller_joined_binding_mismatch");
        }
    }
    let string = |field: &'static str| -> std::result::Result<String, &'static str> {
        validated
            .get(field)
            .and_then(Value::as_str)
            .map(str::to_owned)
            .ok_or("controller_joined_invalid")
    };
    let lease_epoch = string("lease_epoch")?
        .parse::<u64>()
        .map_err(|_| "controller_joined_invalid")?;
    let resume_deadline_ms = string("resume_deadline_ms")?
        .parse::<u64>()
        .map_err(|_| "controller_joined_invalid")?;
    if lease_epoch == 0 || resume_deadline_ms == 0 {
        return Err("controller_joined_invalid");
    }
    let raw_token = base64::engine::general_purpose::STANDARD
        .decode(
            validated
                .get("resume_token_b64")
                .and_then(Value::as_str)
                .ok_or("controller_joined_invalid")?,
        )
        .map_err(|_| "controller_joined_invalid")?;
    let resume_token: [u8; RESUME_TOKEN_BYTES] = raw_token
        .try_into()
        .map_err(|_| "controller_joined_invalid")?;
    Ok(JoinedLease {
        connection_id: string("connection_id")?,
        controller_lease_id: string("controller_lease_id")?,
        lease_epoch,
        relay_epoch: string("relay_epoch")?,
        remote_control_generation: string("remote_control_generation")?,
        liveness_epoch_a: string("liveness_epoch_a")?,
        resume_deadline_ms,
        resume_token,
    })
}

pub(crate) struct ControllingHandshake {
    request_id: String,
    claim_request_id: String,
    pairing_code: String,
    handshake: Option<HandshakeKeyPair>,
    trust: AuthenticatedRelayTrustConfig,
    binding: Option<HandshakeBinding>,
    transcript_hash_hex: String,
    transcript_hash: Option<[u8; 32]>,
    negotiated_capabilities: Vec<String>,
    pairing_id: String,
    peer_endpoint_id: String,
    reservation_id: String,
    relay_epoch: String,
    generation: String,
    session: Option<SessionKeys>,
    finished_sender: Option<FinishedSender>,
    finished_receiver: Option<FinishedReceiver>,
    receipt: Option<ReceiptSecret>,
    peer_commitment: Option<String>,
    stage: HandshakeStage,
}

impl ControllingHandshake {
    pub(crate) fn new(
        request_id: impl Into<String>,
        claim_request_id: impl Into<String>,
        pairing_code: impl Into<String>,
        handshake: HandshakeKeyPair,
        trust: AuthenticatedRelayTrustConfig,
    ) -> Self {
        Self {
            request_id: request_id.into(),
            claim_request_id: claim_request_id.into(),
            pairing_code: pairing_code.into(),
            handshake: Some(handshake),
            trust,
            binding: None,
            transcript_hash_hex: String::new(),
            transcript_hash: None,
            negotiated_capabilities: Vec::new(),
            pairing_id: String::new(),
            peer_endpoint_id: String::new(),
            reservation_id: String::new(),
            relay_epoch: String::new(),
            generation: String::new(),
            session: None,
            finished_sender: None,
            finished_receiver: None,
            receipt: None,
            peer_commitment: None,
            stage: HandshakeStage::Claimed,
        }
    }

    pub(crate) fn stage(&self) -> HandshakeStage {
        self.stage
    }

    /// `controller_pairing_claim`：声明本端握手公钥与所需能力。
    ///
    /// 连接码只在 claim 中一次性使用：报文提交成功后立刻从内存里清除，之后
    /// 任何重试都只能失败关闭（`pairing_code` 不再是合法码）。
    pub(crate) fn claim(&mut self) -> Result<Value> {
        let handshake = self
            .handshake
            .as_ref()
            .ok_or_else(|| HandshakeError::new("controller_handshake_stage_invalid"))?;
        let value = json!({
            "v": 1,
            "type": "controller_pairing_claim",
            "request_id": self.request_id,
            "claim_request_id": self.claim_request_id,
            "pairing_code": self.pairing_code,
            "handshake_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(handshake.public_key().to_bytes()),
            "key_algorithm": "x25519",
            "capabilities": CLAIM_CAPABILITIES,
        });
        validate_controller_message_json(&value.to_string(), SenderRole::ControllerA)
            .map_err(|_| HandshakeError::new("controller_claim_invalid"))?;
        self.pairing_code.zeroize();
        Ok(value)
    }

    /// 处理 `controller_pairing_reserved`：验签 transcript、派生会话密钥、
    /// 生成 receipt 并返回 `finished_receipt_commit`。
    pub(crate) fn accept_reserved(&mut self, message: &Value) -> Result<Value> {
        self.require_stage(HandshakeStage::Claimed)?;
        let parsed = parse_controller_pairing_transcript(&message.to_string())
            .map_err(|_| HandshakeError::new("controller_transcript_invalid"))?;
        let context = authenticate_relay_transcript(&self.trust, parsed)
            .map_err(|_| HandshakeError::new("controller_transcript_untrusted"))?;
        let transcript_hash = context
            .transcript_hash_bytes()
            .map_err(|_| HandshakeError::new("controller_transcript_invalid"))?;
        let binding = HandshakeBinding::new(
            context.relay_epoch(),
            context.pairing_id(),
            context.reservation_id(),
            transcript_hash,
        )
        .map_err(|_| HandshakeError::new("controller_transcript_invalid"))?;
        let peer = decode_public_key(context.peer_handshake_public_key_b64())
            .ok_or_else(|| HandshakeError::new("controller_transcript_invalid"))?;
        let keypair = self
            .handshake
            .take()
            .ok_or_else(|| HandshakeError::new("controller_handshake_stage_invalid"))?;
        let session = keypair
            .into_session_keys(peer, &binding)
            .map_err(|_| HandshakeError::new("controller_handshake_key_invalid"))?;
        let (finished_sender, finished_receiver) = session.finished_pair(ControllerRole::A);
        let receipt = ReceiptSecret::generate()
            .map_err(|_| HandshakeError::new("controller_receipt_generation_failed"))?;
        let commitment = receipt
            .commitment(&binding, ControllerRole::A)
            .map_err(|_| HandshakeError::new("controller_commitment_failed"))?;
        self.transcript_hash_hex = hex_lower(&transcript_hash);
        self.transcript_hash = Some(transcript_hash);
        self.negotiated_capabilities = context.negotiated_capabilities().to_vec();
        self.pairing_id = context.pairing_id().to_owned();
        self.peer_endpoint_id = context.peer_endpoint_id().to_owned();
        self.reservation_id = context.reservation_id().to_owned();
        self.relay_epoch = context.relay_epoch().to_owned();
        self.generation = context.remote_control_generation().to_owned();
        let commit = self.envelope(
            "finished_receipt_commit",
            json!({ "sender_role": "A", "commitment": hex_lower(&commitment) }),
        )?;
        self.binding = Some(binding);
        self.session = Some(session);
        self.finished_sender = Some(finished_sender);
        self.finished_receiver = Some(finished_receiver);
        self.receipt = Some(receipt);
        self.stage = HandshakeStage::Reserved;
        validate_controller_message_json(&commit.to_string(), SenderRole::ControllerA)
            .map_err(|_| HandshakeError::new("controller_commit_invalid"))
    }

    /// 处理对端 `finished_receipt_commit` 并返回 `encrypted_handshake_finished`。
    pub(crate) fn accept_peer_commitment(&mut self, message: &Value) -> Result<Value> {
        self.require_stage(HandshakeStage::Reserved)?;
        self.assert_peer_envelope(message, "finished_receipt_commit", "B")?;
        let commitment = message
            .get("commitment")
            .and_then(Value::as_str)
            .filter(|value| is_lower_hex(value, 64))
            .ok_or_else(|| HandshakeError::new("controller_commit_invalid"))?
            .to_owned();
        let sender = self
            .finished_sender
            .take()
            .ok_or_else(|| HandshakeError::new("controller_handshake_stage_invalid"))?;
        let receipt = self
            .receipt
            .as_ref()
            .ok_or_else(|| HandshakeError::new("controller_handshake_stage_invalid"))?;
        let sealed = sender
            .seal(receipt)
            .map_err(|_| HandshakeError::new("controller_finished_seal_failed"))?;
        let finished = self.envelope(
            "encrypted_handshake_finished",
            json!({
                "sender_role": "A",
                "ciphertext_b64": base64::engine::general_purpose::STANDARD.encode(&sealed),
                "ciphertext_bytes": sealed.len().to_string(),
            }),
        )?;
        self.peer_commitment = Some(commitment);
        self.stage = HandshakeStage::CommitSent;
        validate_controller_message_json(&finished.to_string(), SenderRole::ControllerA)
            .map_err(|_| HandshakeError::new("controller_finished_invalid"))
    }

    /// 处理对端 `encrypted_handshake_finished`：校验 receipt commitment 与
    /// 握手期提交一致后返回 `finished_receipt_ack`。
    pub(crate) fn accept_peer_finished(&mut self, message: &Value) -> Result<Value> {
        self.require_stage(HandshakeStage::CommitSent)?;
        self.assert_peer_envelope(message, "encrypted_handshake_finished", "B")?;
        let ciphertext = base64::engine::general_purpose::STANDARD
            .decode(
                message
                    .get("ciphertext_b64")
                    .and_then(Value::as_str)
                    .ok_or_else(|| HandshakeError::new("controller_finished_invalid"))?,
            )
            .map_err(|_| HandshakeError::new("controller_finished_invalid"))?;
        let binding = self
            .binding
            .clone()
            .ok_or_else(|| HandshakeError::new("controller_handshake_stage_invalid"))?;
        let receiver = self
            .finished_receiver
            .take()
            .ok_or_else(|| HandshakeError::new("controller_handshake_stage_invalid"))?;
        let peer_receipt = receiver
            .open(&ciphertext)
            .map_err(|_| HandshakeError::new("controller_finished_invalid"))?;
        let commitment = peer_receipt
            .commitment(&binding, ControllerRole::B)
            .map_err(|_| HandshakeError::new("controller_commitment_failed"))?;
        if self.peer_commitment.as_deref() != Some(hex_lower(&commitment).as_str()) {
            return Err(HandshakeError::new("controller_commitment_mismatch"));
        }
        let ack = self.envelope(
            "finished_receipt_ack",
            json!({
                // 协议语义：receiver_role = 回执方（收到对端 Finished 的一侧），
                // sender_role = 产出被回执 Finished 的一侧。
                "sender_role": "B",
                "receiver_role": "A",
                // Relay 的收据校验契约（`controller-lease.js` 的 `acceptReceipt`）：
                // 帧里带的必须是 `sender_role`（B）的 receipt secret —— 也就是本端
                // 刚从对端 Finished 里解开的 `peer_receipt`，而不是本端自己的 receipt。
                "receipt_secret_b64": peer_receipt.to_base64(),
            }),
        )?;
        self.stage = HandshakeStage::FinishedSent;
        validate_controller_message_json(&ack.to_string(), SenderRole::ControllerA)
            .map_err(|_| HandshakeError::new("controller_ack_invalid"))
    }

    /// 处理 `controller_joined`：只有走到这里才产生租约身份，调用方随后用
    /// `take_session` 派生业务密钥并安装租约。
    pub(crate) fn accept_joined(&mut self, message: &Value) -> Result<JoinedLease> {
        self.require_stage(HandshakeStage::FinishedSent)?;
        let lease = validate_joined_lease(
            message,
            &self.pairing_id,
            &self.reservation_id,
            &self.relay_epoch,
            &self.transcript_hash_hex,
        )
        .map_err(HandshakeError::new)?;
        self.stage = HandshakeStage::Joined;
        Ok(lease)
    }

    /// 验签 transcript 里 B 拥有的 generation（绑定 transport 用）。
    pub(crate) fn remote_control_generation(&self) -> &str {
        &self.generation
    }

    /// B 的 endpoint 身份来自 Relay 签名 transcript，不接受 UI 声明。
    pub(crate) fn peer_endpoint_id(&self) -> &str {
        &self.peer_endpoint_id
    }

    pub(crate) fn reservation_id(&self) -> &str {
        &self.reservation_id
    }

    /// Relay 在 transcript 中协商出的能力集合（安装租约的准入上下文）。
    pub(crate) fn negotiated_capabilities(&self) -> &[String] {
        &self.negotiated_capabilities
    }

    /// 认证 transcript 的哈希原始字节；只在本端验签成功后可用。
    pub(crate) fn transcript_hash(&self) -> Option<[u8; 32]> {
        self.transcript_hash
    }

    /// 握手成功后交出会话密钥，供调用方用真实 `LeaseBinding` 派生业务密钥。
    pub(crate) fn take_session(&mut self) -> Result<SessionKeys> {
        self.require_stage(HandshakeStage::Joined)?;
        self.session
            .take()
            .ok_or_else(|| HandshakeError::new("controller_handshake_stage_invalid"))
    }

    fn require_stage(&self, expected: HandshakeStage) -> Result<()> {
        if self.stage == expected {
            Ok(())
        } else {
            Err(HandshakeError::new("controller_handshake_stage_invalid"))
        }
    }

    fn assert_peer_envelope(
        &self,
        message: &Value,
        kind: &'static str,
        role: &'static str,
    ) -> Result<()> {
        let validated =
            validate_controller_message_json(&message.to_string(), SenderRole::ControlledB)
                .map_err(|_| HandshakeError::new("controller_handshake_rejected"))?;
        if validated.get("type").and_then(Value::as_str) != Some(kind)
            || validated.get("sender_role").and_then(Value::as_str) != Some(role)
        {
            return Err(HandshakeError::new("controller_handshake_rejected"));
        }
        for (field, expected) in [
            ("pairing_id", self.pairing_id.as_str()),
            ("reservation_id", self.reservation_id.as_str()),
            ("relay_epoch", self.relay_epoch.as_str()),
            ("transcript_hash", self.transcript_hash_hex.as_str()),
        ] {
            if validated.get(field).and_then(Value::as_str) != Some(expected) {
                return Err(HandshakeError::new("controller_handshake_binding_mismatch"));
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
        let extra = extra
            .as_object()
            .ok_or_else(|| HandshakeError::new("controller_message_invalid"))?;
        for (key, value) in extra {
            object[key.as_str()] = value.clone();
        }
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

    use super::super::crypto::LeaseBinding;
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
        let public_key =
            base64::engine::general_purpose::STANDARD.encode(key_pair.public_key().as_ref());
        let key_id = "relay-key-e2e".to_owned();
        let trust = AuthenticatedRelayTrustConfig::from_pinned_keys([(key_id.clone(), public_key)])
            .unwrap();
        SignedRelay {
            trust,
            key_id,
            key_pair,
        }
    }

    /// 以冻结 fixture 的 transcript 为模板，替换双方握手公钥并用真实
    /// Ed25519 密钥重新签名，使测试能扮演 B 完成后续收据阶段。
    fn signed_reservation(relay: &SignedRelay, a_public_b64: &str, b_public_b64: &str) -> Value {
        let fixture = fixture();
        let mut message =
            fixture["lease_install_vector"]["verification_input"]["reserved_message"].clone();
        let mut transcript = message["transcript"].clone();
        transcript["handshake_public_key_a_b64"] = json!(a_public_b64);
        transcript["handshake_public_key_b_b64"] = json!(b_public_b64);
        let preimage = build_pairing_transcript_preimage(&transcript).unwrap();
        let transcript_hash = hex_lower(&Sha256::digest(&preimage));
        message["transcript"] = transcript;
        message["transcript_hash"] = json!(transcript_hash);
        message["relay_signing_key_id"] = json!(relay.key_id);
        message["relay_signature_b64"] = json!(
            base64::engine::general_purpose::STANDARD
                .encode(relay.key_pair.sign(&preimage).as_ref())
        );
        message
    }

    fn field(message: &Value, name: &str) -> String {
        message[name].as_str().unwrap().to_owned()
    }

    fn hex_bytes(value: &str) -> [u8; 32] {
        let mut output = [0_u8; 32];
        for (index, chunk) in value.as_bytes().chunks_exact(2).enumerate() {
            output[index] = u8::from_str_radix(std::str::from_utf8(chunk).unwrap(), 16).unwrap();
        }
        output
    }

    fn trust_fixture() -> (AuthenticatedRelayTrustConfig, Value) {
        let fixture: Value = serde_json::from_str(include_str!(
            "../../../../../../remote-control-relay/test/fixtures/controller-protocol-v1.json"
        ))
        .unwrap();
        let key =
            fixture["untrusted_authentication_inputs"][1]["value"]["keys"][0]["public_key_b64"]
                .as_str()
                .unwrap()
                .to_owned();
        let trust =
            AuthenticatedRelayTrustConfig::from_pinned_keys([("relay-key-test-1".to_owned(), key)])
                .unwrap();
        (
            trust,
            fixture["lease_install_vector"]["verification_input"]["reserved_message"].clone(),
        )
    }

    fn new_handshake(trust: AuthenticatedRelayTrustConfig) -> ControllingHandshake {
        ControllingHandshake::new(
            "request-claim-0001",
            "claim-0001",
            "2345678",
            HandshakeKeyPair::generate().unwrap(),
            trust,
        )
    }

    #[test]
    fn claim_is_protocol_valid_and_carries_the_local_handshake_key() {
        let (trust, _) = trust_fixture();
        let mut handshake = new_handshake(trust);
        let claim = handshake.claim().unwrap();
        validate_controller_message_json(&claim.to_string(), SenderRole::ControllerA).unwrap();
        assert_eq!(claim["claim_request_id"], json!("claim-0001"));
        assert_eq!(claim["pairing_code"], json!("2345678"));
        assert_eq!(claim["key_algorithm"], json!("x25519"));
        assert_eq!(
            base64::engine::general_purpose::STANDARD
                .decode(claim["handshake_public_key_b64"].as_str().unwrap())
                .unwrap()
                .len(),
            32
        );
        // E2E-02：连接码只在 claim 中一次性使用，提交成功后立刻清除；重复 claim
        // 只能失败关闭，绝不重放已被消费的码。
        assert!(handshake.pairing_code.is_empty());
        assert_eq!(
            handshake.claim().unwrap_err().code,
            "controller_claim_invalid"
        );
    }

    #[test]
    fn out_of_order_steps_fail_closed_without_touching_state() {
        let (trust, reserved) = trust_fixture();
        let mut handshake = new_handshake(trust);
        assert_eq!(
            handshake
                .accept_peer_commitment(&reserved)
                .unwrap_err()
                .code,
            "controller_handshake_stage_invalid"
        );
        assert_eq!(
            handshake.accept_joined(&reserved).unwrap_err().code,
            "controller_handshake_stage_invalid"
        );
        assert_eq!(handshake.stage(), HandshakeStage::Claimed);
        assert_eq!(
            handshake.take_session().unwrap_err().code,
            "controller_handshake_stage_invalid"
        );
    }

    #[test]
    fn untrusted_and_malformed_reservations_are_rejected() {
        let (trust, reserved) = trust_fixture();
        let attacker = AuthenticatedRelayTrustConfig::from_pinned_keys([(
            "relay-key-test-1".to_owned(),
            base64::engine::general_purpose::STANDARD.encode([9_u8; 32]),
        )])
        .unwrap();
        let mut handshake = new_handshake(attacker);
        assert_eq!(
            handshake.accept_reserved(&reserved).unwrap_err().code,
            "controller_transcript_untrusted"
        );
        assert_eq!(handshake.stage(), HandshakeStage::Claimed);

        let mut handshake = new_handshake(trust);
        assert_eq!(
            handshake
                .accept_reserved(&json!({ "v": 1, "type": "controller_pairing_reserved" }))
                .unwrap_err()
                .code,
            "controller_transcript_invalid"
        );
    }

    #[test]
    fn trusted_reservation_derives_a_commit_without_any_lease_material() {
        let (trust, reserved) = trust_fixture();
        let mut handshake = new_handshake(trust);
        let commit = handshake.accept_reserved(&reserved).unwrap();
        validate_controller_message_json(&commit.to_string(), SenderRole::ControllerA).unwrap();
        assert_eq!(commit["type"], json!("finished_receipt_commit"));
        assert_eq!(commit["sender_role"], json!("A"));
        assert!(is_lower_hex(commit["commitment"].as_str().unwrap(), 64));
        assert_eq!(handshake.stage(), HandshakeStage::Reserved);
        // 半握手不产出任何可安装的租约材料。
        assert_eq!(
            handshake
                .accept_joined(&json!({ "v": 1, "type": "controller_joined" }))
                .unwrap_err()
                .code,
            "controller_handshake_stage_invalid"
        );
        assert_eq!(
            handshake.take_session().unwrap_err().code,
            "controller_handshake_stage_invalid"
        );
    }

    /// E2E-02 核心：A 端全序列（claim → reserved → commit → finished → ack →
    /// joined）在真实签名 transcript 与模拟 B 的配合下走完，且只有在双收据
    /// 交换完成、`controller_joined` 到达后才产出租约材料。
    #[test]
    fn full_handshake_sequence_yields_a_lease_only_after_both_receipts() {
        let relay = signed_relay();
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let b_keys = HandshakeKeyPair::generate().unwrap();
        let a_public_bytes = a_keys.public_key().to_bytes();
        let b_public_bytes = b_keys.public_key().to_bytes();
        let encoded = |bytes: [u8; 32]| base64::engine::general_purpose::STANDARD.encode(bytes);
        let reserved =
            signed_reservation(&relay, &encoded(a_public_bytes), &encoded(b_public_bytes));
        let request_id = field(&reserved, "request_id");

        let mut a = ControllingHandshake::new(
            request_id.clone(),
            "claim-0001",
            "2345678",
            a_keys,
            relay.trust.clone(),
        );
        let a_commit = a.accept_reserved(&reserved).unwrap();
        let transcript_hash_hex = field(&a_commit, "transcript_hash");
        let binding = HandshakeBinding::new(
            field(&reserved, "relay_epoch"),
            field(&reserved, "pairing_id"),
            field(&reserved, "reservation_id"),
            hex_bytes(&transcript_hash_hex),
        )
        .unwrap();

        // ---- 模拟 B：同一 transcript、独立派生，产出自己的 commit/finished ----
        let b_session = b_keys
            .into_session_keys(HandshakePublicKey::from_bytes(a_public_bytes), &binding)
            .unwrap();
        let (b_sender, b_receiver) = b_session.finished_pair(ControllerRole::B);
        let b_receipt = ReceiptSecret::generate().unwrap();
        let b_commitment = hex_lower(&b_receipt.commitment(&binding, ControllerRole::B).unwrap());
        let envelope = |kind: &str, extra: Value| {
            let mut value = json!({
                "v": 1,
                "type": kind,
                "request_id": request_id.clone(),
                "pairing_id": field(&reserved, "pairing_id"),
                "reservation_id": field(&reserved, "reservation_id"),
                "remote_control_generation": field(&reserved, "remote_control_generation"),
                "relay_epoch": field(&reserved, "relay_epoch"),
                "transcript_hash": transcript_hash_hex.clone(),
                "sender_role": "B",
            });
            if let Some(object) = extra.as_object() {
                for (key, entry) in object {
                    value[key.as_str()] = entry.clone();
                }
            }
            value
        };

        let a_finished = a
            .accept_peer_commitment(&envelope(
                "finished_receipt_commit",
                json!({ "commitment": b_commitment }),
            ))
            .unwrap();
        assert_eq!(a.stage(), HandshakeStage::CommitSent);

        // B 打开 A 的 Finished，确认 A 的 receipt 与其 commit 一致。
        let a_ciphertext = base64::engine::general_purpose::STANDARD
            .decode(field(&a_finished, "ciphertext_b64"))
            .unwrap();
        let a_receipt = b_receiver.open(&a_ciphertext).unwrap();
        assert_eq!(
            hex_lower(&a_receipt.commitment(&binding, ControllerRole::A).unwrap()),
            field(&a_commit, "commitment"),
            "A's Finished must reveal the receipt committed earlier"
        );

        // B 回自己的 Finished；A 校验 commitment 一致后产出 ack。
        let b_ciphertext = b_sender.seal(&b_receipt).unwrap();
        let a_ack = a
            .accept_peer_finished(&envelope(
                "encrypted_handshake_finished",
                json!({
                    "ciphertext_b64": base64::engine::general_purpose::STANDARD.encode(&b_ciphertext),
                    "ciphertext_bytes": b_ciphertext.len().to_string(),
                }),
            ))
            .unwrap();
        assert_eq!(a.stage(), HandshakeStage::FinishedSent);
        assert_eq!(
            base64::engine::general_purpose::STANDARD
                .decode(field(&a_ack, "receipt_secret_b64"))
                .unwrap()
                .len(),
            32
        );

        // 收据阶段没有任何租约材料。
        assert_eq!(
            a.take_session().unwrap_err().code,
            "controller_handshake_stage_invalid"
        );

        let joined = json!({
            "v": 1,
            "type": "controller_joined",
            "request_id": request_id.clone(),
            "pairing_id": field(&reserved, "pairing_id"),
            "reservation_id": field(&reserved, "reservation_id"),
            "transcript_hash": transcript_hash_hex,
            "controller_connection_identity": field(&reserved["transcript"], "controller_connection_identity"),
            "connection_id": "connection-0001",
            "controller_lease_id": "controller-lease-0001",
            "lease_epoch": "1",
            "relay_epoch": field(&reserved, "relay_epoch"),
            "remote_control_generation": field(&reserved, "remote_control_generation"),
            "liveness_epoch_a": "liveness-epoch-a-0001",
            "liveness_epoch_b": "liveness-epoch-b-0001",
            "resume_token_b64": base64::engine::general_purpose::STANDARD.encode([7_u8; 32]),
            "resume_deadline_ms": "30000",
        });
        let lease = a.accept_joined(&joined).unwrap();
        assert_eq!(lease.controller_lease_id, "controller-lease-0001");
        assert_eq!(lease.lease_epoch, 1);
        assert_eq!(lease.connection_id, "connection-0001");

        let session = a.take_session().unwrap();
        let ciphers = session
            .into_role_ciphers(
                ControllerRole::A,
                LeaseBinding::new(
                    lease.connection_id,
                    lease.controller_lease_id,
                    lease.lease_epoch,
                    lease.relay_epoch,
                    lease.remote_control_generation,
                )
                .unwrap(),
            )
            .unwrap();
        let _ = ciphers;
    }

    #[test]
    fn peer_commitment_must_match_before_any_ack_is_produced() {
        let (trust, reserved) = trust_fixture();
        let mut handshake = new_handshake(trust);
        handshake.accept_reserved(&reserved).unwrap();
        let forged = json!({
            "v": 1,
            "type": "finished_receipt_commit",
            "request_id": "request-claim-0001",
            "pairing_id": handshake.pairing_id,
            "reservation_id": handshake.reservation_id,
            "remote_control_generation": handshake.generation,
            "relay_epoch": handshake.relay_epoch,
            "transcript_hash": handshake.transcript_hash_hex,
            "sender_role": "B",
            "commitment": "0".repeat(64),
        });
        let finished = handshake.accept_peer_commitment(&forged).unwrap();
        validate_controller_message_json(&finished.to_string(), SenderRole::ControllerA).unwrap();
        assert_eq!(finished["sender_role"], json!("A"));
        assert!(finished["ciphertext_b64"].as_str().is_some());

        // 对端 Finished 无法用任意密文伪造：直接喂随机密文必须失败关闭。
        let mut forged_finished = finished.clone();
        forged_finished["ciphertext_b64"] =
            json!(base64::engine::general_purpose::STANDARD.encode([1_u8; 64]));
        forged_finished["ciphertext_bytes"] = json!("64");
        let mut racing = handshake;
        let error = racing.accept_peer_finished(&forged_finished).unwrap_err();
        assert!(
            matches!(
                error.code,
                "controller_finished_invalid"
                    | "controller_handshake_rejected"
                    | "controller_handshake_binding_mismatch"
            ),
            "a forged Finished must fail closed, got {}",
            error.code
        );
    }
}
