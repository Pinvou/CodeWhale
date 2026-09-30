//! E2E 双端测试夹具：用真实 Ed25519 密钥按冻结 transcript 结构重新签名，得到
//! 可被两端验签的 `controller_pairing_reserved` / `controller_joined`。
//!
//! 仅测试编译。早期 DEV-05/DEV-07/DEV-11 的模块各自内联了同类夹具，此处不再
//! 回改它们（避免动已审核代码），但 E2E 的新测试统一复用本模块，避免同一份
//! 签名逻辑继续复制。

use base64::Engine as _;
use ring::rand::SystemRandom;
use ring::signature::{Ed25519KeyPair, KeyPair};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

use super::crypto::{
    AuthenticatedRelayTrustConfig, ControllerRole, HandshakeBinding, HandshakeKeyPair,
    HandshakePublicKey, SessionKeys,
};
use super::protocol::build_pairing_transcript_preimage;

pub(crate) struct SignedRelay {
    pub(crate) trust: AuthenticatedRelayTrustConfig,
    pub(crate) key_id: String,
    pub(crate) key_pair: Ed25519KeyPair,
}

/// 生成一把可控签名的 Relay 密钥并把它固定为本端信任根。
pub(crate) fn signed_relay(key_id: &str) -> SignedRelay {
    let rng = SystemRandom::new();
    let pkcs8 = Ed25519KeyPair::generate_pkcs8(&rng).unwrap();
    let key_pair = Ed25519KeyPair::from_pkcs8(pkcs8.as_ref()).unwrap();
    let public_key =
        base64::engine::general_purpose::STANDARD.encode(key_pair.public_key().as_ref());
    let trust =
        AuthenticatedRelayTrustConfig::from_pinned_keys([(key_id.to_owned(), public_key)]).unwrap();
    SignedRelay {
        trust,
        key_id: key_id.to_owned(),
        key_pair,
    }
}

pub(crate) fn encode(bytes: [u8; 32]) -> String {
    base64::engine::general_purpose::STANDARD.encode(bytes)
}

pub(crate) fn hex_lower(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

/// 以冻结 fixture 的 transcript 为模板替换双方握手公钥并重新签名。
pub(crate) fn signed_reservation(
    relay: &SignedRelay,
    a_public_b64: &str,
    b_public_b64: &str,
) -> Value {
    let fixture: Value = serde_json::from_str(include_str!(
        "../../../../../../remote-control-relay/test/fixtures/controller-protocol-v1.json"
    ))
    .unwrap();
    let mut message =
        fixture["lease_install_vector"]["verification_input"]["reserved_message"].clone();
    let mut transcript = message["transcript"].clone();
    transcript["handshake_public_key_a_b64"] = json!(a_public_b64);
    transcript["handshake_public_key_b_b64"] = json!(b_public_b64);
    let preimage = build_pairing_transcript_preimage(&transcript).unwrap();
    message["transcript"] = transcript;
    message["transcript_hash"] = json!(hex_lower(&Sha256::digest(&preimage)));
    message["relay_signing_key_id"] = json!(relay.key_id);
    message["relay_signature_b64"] = json!(
        base64::engine::general_purpose::STANDARD.encode(relay.key_pair.sign(&preimage).as_ref())
    );
    message
}

/// Relay 在两个角色都提交 receipt 后下发的 `controller_joined`。
pub(crate) fn joined_message(reserved: &Value, token: [u8; 32]) -> Value {
    json!({
        "v": 1,
        "type": "controller_joined",
        "request_id": reserved["request_id"],
        "pairing_id": reserved["pairing_id"],
        "reservation_id": reserved["reservation_id"],
        "transcript_hash": reserved["transcript_hash"],
        "controller_connection_identity": reserved["transcript"]["controller_connection_identity"],
        "connection_id": "connection-0001",
        "controller_lease_id": "controller-lease-0001",
        "lease_epoch": "1",
        "relay_epoch": reserved["relay_epoch"],
        "remote_control_generation": reserved["remote_control_generation"],
        "liveness_epoch_a": "liveness-epoch-a-0001",
        "liveness_epoch_b": "liveness-epoch-b-0001",
        "resume_token_b64": base64::engine::general_purpose::STANDARD.encode(token),
        "resume_deadline_ms": "30000",
    })
}

/// 一组自洽的双端材料：A/B 密钥对、可验签的预留报文与 joined 通知。
pub(crate) struct PairingFixture {
    pub(crate) relay: SignedRelay,
    pub(crate) reservation: Value,
    pub(crate) joined: Value,
    pub(crate) a_keys: HandshakeKeyPair,
    pub(crate) b_keys: HandshakeKeyPair,
    pub(crate) resume_token: [u8; 32],
}

pub(crate) fn pairing_fixture(key_id: &str) -> PairingFixture {
    let relay = signed_relay(key_id);
    let a_keys = HandshakeKeyPair::generate().unwrap();
    let b_keys = HandshakeKeyPair::generate().unwrap();
    let reservation = signed_reservation(
        &relay,
        &encode(a_keys.public_key().to_bytes()),
        &encode(b_keys.public_key().to_bytes()),
    );
    let resume_token = [7_u8; 32];
    let joined = joined_message(&reservation, resume_token);
    PairingFixture {
        relay,
        reservation,
        joined,
        a_keys,
        b_keys,
        resume_token,
    }
}

/// 从验签 transcript 报文里解出握手绑定（relay_epoch / pairing_id /
/// reservation_id / transcript_hash 原始字节）。
pub(crate) fn handshake_binding(reserved: &Value) -> HandshakeBinding {
    let transcript_hash = reserved["transcript_hash"]
        .as_str()
        .expect("fixture carries a transcript hash");
    let mut bytes = [0_u8; 32];
    for (index, chunk) in transcript_hash.as_bytes().chunks_exact(2).enumerate() {
        bytes[index] =
            u8::from_str_radix(std::str::from_utf8(chunk).unwrap(), 16).unwrap();
    }
    HandshakeBinding::new(
        reserved["relay_epoch"].as_str().unwrap(),
        reserved["pairing_id"].as_str().unwrap(),
        reserved["reservation_id"].as_str().unwrap(),
        bytes,
    )
    .unwrap()
}

/// 用给定角色的握手密钥与对端公钥派生会话密钥（与真实握手路径一致）。
pub(crate) fn role_session_keys(
    _role: ControllerRole,
    keys: HandshakeKeyPair,
    peer_public: [u8; 32],
    reserved: &Value,
) -> SessionKeys {
    // 会话密钥本身与角色无关（角色只在收据/业务密钥派生时区分），这里保留
    // 参数以便调用点显式声明意图。
    let binding = handshake_binding(reserved);
    keys.into_session_keys(HandshakePublicKey::from_bytes(peer_public), &binding)
        .unwrap()
}

/// 一次派生双向会话密钥：会话 root 与角色无关，因此 A 用 (a_secret, b_public)、
/// B 用 (b_secret, a_public) 会得到同一份 root，角色差异只体现在业务密钥方向。
pub(crate) fn paired_session_keys(
    a_keys: HandshakeKeyPair,
    b_keys: HandshakeKeyPair,
    reserved: &Value,
) -> (SessionKeys, SessionKeys) {
    let a_public = a_keys.public_key().to_bytes();
    let b_public = b_keys.public_key().to_bytes();
    let a_session = role_session_keys(ControllerRole::A, a_keys, b_public, reserved);
    let b_session = role_session_keys(ControllerRole::B, b_keys, a_public, reserved);
    (a_session, b_session)
}

/// 由 fixture 的预留报文构造对应的租约上下文。
pub(crate) fn lease_context(reserved: &Value) -> super::protocol::LeaseContext {
    super::protocol::LeaseContext {
        connection_id: "connection-0001".into(),
        controller_lease_id: "lease-0001".into(),
        lease_epoch: "1".into(),
        relay_epoch: reserved["relay_epoch"].as_str().unwrap().to_owned(),
        remote_control_generation: reserved["remote_control_generation"]
            .as_str()
            .unwrap()
            .to_owned(),
    }
}
