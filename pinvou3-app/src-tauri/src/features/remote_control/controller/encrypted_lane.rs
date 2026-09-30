//! 加密业务帧通道（E2E-04）。
//!
//! 把握手派生的会话密钥包成一条可复用的「加密业务通道」：构造 / 解析冻结的
//! `encrypted_frame` 信封、做方向与代绑定检查、维护发送序号、拒绝重放与旧
//! lease。网络与状态所有者仍由调用方拥有（A 端走 relay transport，B 端走桌面
//! 端通道），因此本模块既能被 A 使用，也能被 B 使用。
//!
//! 两侧都先在**明文**上跑冻结运行时校验（角色 + 租约 + 协商能力），再加密；
//! 接收侧先校验外层信封，再解密，再校验明文——任何一层不符都失败关闭。

use base64::Engine as _;
use parking_lot::Mutex;
use serde_json::{Value, json};

use super::crypto::{ControllerRole, FrameDirection, LeaseBinding, SendCipher, ReceiveCipher, SessionKeys};
use super::protocol::{
    MAX_ENCRYPTED_WIRE_BYTES, RuntimeValidationContext, SenderRole,
    validate_controller_runtime_message_json, validate_controller_runtime_payload_json,
};

/// 有界错误码：不区分「解密失败/校验失败/代不符」，避免给出可区分信息。
pub(crate) const ENCRYPTED_FRAME_INVALID: &str = "encrypted_frame_invalid";

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum LaneFailure {
    /// 帧形状、方向、能力或租约不符。
    Invalid,
    /// 序号重放 / 已耗尽 / 旧 lease。
    Replay,
    /// 载荷过大。
    TooLarge,
}

impl LaneFailure {
    pub(crate) fn code(self) -> &'static str {
        match self {
            Self::Invalid => ENCRYPTED_FRAME_INVALID,
            Self::Replay => "encrypted_frame_replayed",
            Self::TooLarge => "encrypted_frame_too_large",
        }
    }
}

/// 一条会话密钥对应的加密业务通道（一个 lease 一个方向）。
pub(crate) struct EncryptedLane {
    role: ControllerRole,
    lease: super::protocol::LeaseContext,
    send: SendCipher,
    receive: ReceiveCipher,
    liveness: super::crypto::LivenessCipher,
    sequence: u64,
}

impl EncryptedLane {
    /// 用握手派生的会话密钥建立通道。租约五元组必须与握手时一致。
    pub(crate) fn new(
        role: ControllerRole,
        lease: &super::protocol::LeaseContext,
        session: SessionKeys,
    ) -> Result<Self, LaneFailure> {
        let epoch = lease
            .lease_epoch
            .parse::<u64>()
            .map_err(|_| LaneFailure::Invalid)?;
        let binding = LeaseBinding::new(
            lease.connection_id.clone(),
            lease.controller_lease_id.clone(),
            epoch,
            lease.relay_epoch.clone(),
            lease.remote_control_generation.clone(),
        )
        .map_err(|_| LaneFailure::Invalid)?;
        let ciphers = session
            .into_role_ciphers(role, binding)
            .map_err(|_| LaneFailure::Invalid)?;
        Ok(Self {
            role,
            lease: lease.clone(),
            send: ciphers.send,
            receive: ciphers.receive,
            liveness: ciphers.liveness_a,
            sequence: 0,
        })
    }

    /// E2E-06：A 端把 `controller_liveness_a` 明文封进
    /// `encrypted_controller_liveness_a`（外层控制帧、明文只有租约与密文）。
    pub(crate) fn seal_liveness(
        &self,
        inner: &Value,
        negotiated_capabilities: &[String],
    ) -> Result<Value, LaneFailure> {
        if self.role != ControllerRole::A {
            return Err(LaneFailure::Invalid);
        }
        if inner.get("type").and_then(Value::as_str) != Some("controller_liveness_a")
            || inner.get("role").and_then(Value::as_str) != Some("A")
        {
            return Err(LaneFailure::Invalid);
        }
        let plaintext = serde_json::to_vec(inner).map_err(|_| LaneFailure::Invalid)?;
        let sealed = self
            .liveness
            .seal(&plaintext)
            .map_err(|_| LaneFailure::Invalid)?;
        let frame = json!({
            "v": 1,
            "type": "encrypted_controller_liveness_a",
            "connection_id": self.lease.connection_id,
            "controller_lease_id": self.lease.controller_lease_id,
            "lease_epoch": self.lease.lease_epoch,
            "ciphertext_b64": base64::engine::general_purpose::STANDARD.encode(&sealed),
            "ciphertext_bytes": sealed.len().to_string(),
        });
        validate_controller_runtime_message_json(
            &frame.to_string(),
            SenderRole::ControllerA,
            Some(&self.runtime_context(negotiated_capabilities)),
        )
        .map_err(map_validation_failure)?;
        Ok(frame)
    }

    /// E2E-06：B 端打开 A 的存活证明，返回 `controller_liveness_a` 明文。
    pub(crate) fn open_liveness(&self, frame: &Value) -> Result<Value, LaneFailure> {
        if self.role != ControllerRole::B {
            return Err(LaneFailure::Invalid);
        }
        if frame.get("type").and_then(Value::as_str) != Some("encrypted_controller_liveness_a") {
            return Err(LaneFailure::Invalid);
        }
        let sealed = base64::engine::general_purpose::STANDARD
            .decode(
                frame
                    .get("ciphertext_b64")
                    .and_then(Value::as_str)
                    .ok_or(LaneFailure::Invalid)?,
            )
            .map_err(|_| LaneFailure::Invalid)?;
        let declared = frame
            .get("ciphertext_bytes")
            .and_then(Value::as_str)
            .and_then(|value| value.parse::<u64>().ok())
            .ok_or(LaneFailure::Invalid)?;
        if sealed.len() as u64 != declared {
            return Err(LaneFailure::Invalid);
        }
        // 外层字段必须绑定当前租约（否则可能是旧 lease 的证明）。
        for (field, expected) in [
            ("connection_id", self.lease.connection_id.as_str()),
            (
                "controller_lease_id",
                self.lease.controller_lease_id.as_str(),
            ),
            ("lease_epoch", self.lease.lease_epoch.as_str()),
        ] {
            if frame.get(field).and_then(Value::as_str) != Some(expected) {
                return Err(LaneFailure::Invalid);
            }
        }
        let plaintext = self
            .liveness
            .open(&sealed)
            .map_err(|_| LaneFailure::Invalid)?;
        serde_json::from_slice(&plaintext).map_err(|_| LaneFailure::Invalid)
    }

    pub(crate) fn lease(&self) -> &super::protocol::LeaseContext {
        &self.lease
    }

    /// 加密一条业务载荷并产出冻结的 `encrypted_frame` 信封。明文与信封都必须
    /// 通过冻结运行时校验（角色、租约、协商能力）。
    pub(crate) fn seal(
        &mut self,
        payload_kind: &str,
        payload: &Value,
        negotiated_capabilities: &[String],
    ) -> Result<Value, LaneFailure> {
        let context = self.runtime_context(negotiated_capabilities);
        let plaintext =
            serde_json::to_vec(payload).map_err(|_| LaneFailure::Invalid)?;
        if plaintext.len() > MAX_ENCRYPTED_WIRE_BYTES {
            return Err(LaneFailure::TooLarge);
        }
        validate_controller_runtime_payload_json(
            std::str::from_utf8(&plaintext).map_err(|_| LaneFailure::Invalid)?,
            self.sender_role(),
            Some(&context),
        )
        .map_err(map_validation_failure)?;
        let sequence = self
            .sequence
            .checked_add(1)
            .ok_or(LaneFailure::Replay)?;
        let epoch = self
            .lease
            .lease_epoch
            .parse::<u64>()
            .map_err(|_| LaneFailure::Invalid)?;
        let ciphertext = self
            .send
            .seal(epoch, sequence, payload_kind, &plaintext)
            .map_err(|error| match error.code {
                "sequence_exhausted" | "replay" => LaneFailure::Replay,
                "plaintext_too_large" | "ciphertext_too_large" => LaneFailure::TooLarge,
                _ => LaneFailure::Invalid,
            })?;
        self.sequence = sequence;
        let frame = self.frame(payload_kind, sequence, &ciphertext);
        validate_controller_runtime_message_json(
            &frame.to_string(),
            self.sender_role(),
            Some(&context),
        )
        .map_err(map_validation_failure)?;
        Ok(frame)
    }

    /// 解析并解密一条来自对端的 `encrypted_frame`，返回 (payload_kind, 明文)。
    pub(crate) fn open(
        &mut self,
        frame: &Value,
        negotiated_capabilities: &[String],
    ) -> Result<(String, Value), LaneFailure> {
        let context = self.runtime_context(negotiated_capabilities);
        let validated = validate_controller_runtime_message_json(
            &frame.to_string(),
            self.peer_role(),
            Some(&context),
        )
        .map_err(map_validation_failure)?;
        let payload_kind = validated
            .get("payload_kind")
            .and_then(Value::as_str)
            .ok_or(LaneFailure::Invalid)?;
        let lease_epoch = uint_field(&validated, "lease_epoch")?;
        let sequence = uint_field(&validated, "frame_sequence")?;
        let encoded = validated
            .get("ciphertext_b64")
            .and_then(Value::as_str)
            .ok_or(LaneFailure::Invalid)?;
        let declared_bytes = uint_field(&validated, "ciphertext_bytes")?;
        let ciphertext = base64::engine::general_purpose::STANDARD
            .decode(encoded)
            .map_err(|_| LaneFailure::Invalid)?;
        if ciphertext.len() as u64 != declared_bytes {
            return Err(LaneFailure::Invalid);
        }
        let plaintext = self
            .receive
            .open(lease_epoch, sequence, payload_kind, &ciphertext)
            .map_err(|error| match error.code {
                "replay" | "sequence_exhausted" => LaneFailure::Replay,
                "plaintext_too_large" | "ciphertext_too_large" => LaneFailure::TooLarge,
                _ => LaneFailure::Invalid,
            })?;
        let payload = validate_controller_runtime_payload_json(
            std::str::from_utf8(&plaintext).map_err(|_| LaneFailure::Invalid)?,
            self.peer_role(),
            Some(&context),
        )
        .map_err(map_validation_failure)?;
        Ok((payload_kind.to_owned(), payload))
    }

    fn frame(&self, payload_kind: &str, sequence: u64, ciphertext: &[u8]) -> Value {
        json!({
            "v": 1,
            "type": "encrypted_frame",
            "connection_id": self.lease.connection_id,
            "controller_lease_id": self.lease.controller_lease_id,
            "lease_epoch": self.lease.lease_epoch,
            "relay_epoch": self.lease.relay_epoch,
            "remote_control_generation": self.lease.remote_control_generation,
            "frame_sequence": sequence.to_string(),
            "direction": std::str::from_utf8(self.direction().wire()).unwrap_or_default(),
            "payload_kind": payload_kind,
            "ciphertext_b64": base64::engine::general_purpose::STANDARD.encode(ciphertext),
            "ciphertext_bytes": ciphertext.len().to_string(),
        })
    }

    fn direction(&self) -> FrameDirection {
        match self.role {
            ControllerRole::A => FrameDirection::AToB,
            ControllerRole::B => FrameDirection::BToA,
        }
    }

    fn sender_role(&self) -> SenderRole {
        match self.role {
            ControllerRole::A => SenderRole::ControllerA,
            ControllerRole::B => SenderRole::ControlledB,
        }
    }

    fn peer_role(&self) -> SenderRole {
        match self.role {
            ControllerRole::A => SenderRole::ControlledB,
            ControllerRole::B => SenderRole::ControllerA,
        }
    }

    fn runtime_context(&self, negotiated_capabilities: &[String]) -> RuntimeValidationContext {
        RuntimeValidationContext {
            lease: Some(self.lease.clone()),
            negotiated_capabilities: negotiated_capabilities.to_vec(),
        }
    }
}

fn uint_field(value: &Value, field: &'static str) -> Result<u64, LaneFailure> {
    value
        .get(field)
        .and_then(Value::as_str)
        .and_then(|text| text.parse::<u64>().ok())
        .ok_or(LaneFailure::Invalid)
}

fn map_validation_failure(error: super::protocol::ControllerProtocolError) -> LaneFailure {
    match error.code {
        "message_too_large" | "plaintext_too_large" => LaneFailure::TooLarge,
        _ => LaneFailure::Invalid,
    }
}

/// 一条连接上唯一的加密业务通道（内存态，断开即清空）。
#[derive(Default)]
struct LaneStore {
    lane: Mutex<Option<EncryptedLane>>,
}

impl LaneStore {
    /// 安装（或替换）通道：只有完整握手 + 租约安装之后才允许调用。
    pub(crate) fn install(
        &self,
        role: ControllerRole,
        lease: &super::protocol::LeaseContext,
        session: SessionKeys,
    ) -> Result<(), LaneFailure> {
        let lane = EncryptedLane::new(role, lease, session)?;
        *self.lane.lock() = Some(lane);
        Ok(())
    }

    pub(crate) fn clear(&self) {
        *self.lane.lock() = None;
    }

    pub(crate) fn is_installed(&self) -> bool {
        self.lane.lock().is_some()
    }

    /// 加密业务载荷；通道未安装（或已被清理）时失败关闭。
    pub(crate) fn seal(
        &self,
        payload_kind: &str,
        payload: &Value,
        negotiated_capabilities: &[String],
    ) -> Result<Value, LaneFailure> {
        let mut guard = self.lane.lock();
        let lane = guard.as_mut().ok_or(LaneFailure::Invalid)?;
        lane.seal(payload_kind, payload, negotiated_capabilities)
    }

    pub(crate) fn open(
        &self,
        frame: &Value,
        negotiated_capabilities: &[String],
    ) -> Result<(String, Value), LaneFailure> {
        let mut guard = self.lane.lock();
        let lane = guard.as_mut().ok_or(LaneFailure::Invalid)?;
        lane.open(frame, negotiated_capabilities)
    }
}

/// 同一个 app 进程可能同时「主控某台设备」并「被某台设备控制」，两条连接各有
/// 自己的会话密钥与序号，因此 A/B 各持一份独立状态（Tauri 按类型管理）。
macro_rules! lane_state {
    ($name:ident, $doc:literal) => {
        #[doc = $doc]
        #[derive(Default)]
        pub(crate) struct $name {
            store: LaneStore,
        }

        impl $name {
            pub(crate) fn install(
                &self,
                role: ControllerRole,
                lease: &super::protocol::LeaseContext,
                session: SessionKeys,
            ) -> Result<(), LaneFailure> {
                self.store.install(role, lease, session)
            }

            pub(crate) fn clear(&self) {
                self.store.clear()
            }

            pub(crate) fn is_installed(&self) -> bool {
                self.store.is_installed()
            }

            pub(crate) fn seal(
                &self,
                payload_kind: &str,
                payload: &Value,
                negotiated_capabilities: &[String],
            ) -> Result<Value, LaneFailure> {
                self.store.seal(payload_kind, payload, negotiated_capabilities)
            }

            pub(crate) fn open(
                &self,
                frame: &Value,
                negotiated_capabilities: &[String],
            ) -> Result<(String, Value), LaneFailure> {
                self.store.open(frame, negotiated_capabilities)
            }

            /// E2E-06：A 端封装存活证明 / B 端解封（角色不匹配失败关闭）。
            pub(crate) fn seal_liveness(
                &self,
                inner: &Value,
                negotiated_capabilities: &[String],
            ) -> Result<Value, LaneFailure> {
                let guard = self.store.lane.lock();
                let lane = guard.as_ref().ok_or(LaneFailure::Invalid)?;
                lane.seal_liveness(inner, negotiated_capabilities)
            }

            pub(crate) fn open_liveness(&self, frame: &Value) -> Result<Value, LaneFailure> {
                let guard = self.store.lane.lock();
                let lane = guard.as_ref().ok_or(LaneFailure::Invalid)?;
                lane.open_liveness(frame)
            }
        }
    };
}

lane_state!(ControllingLaneState, "A 端（主控）加密业务通道状态。");
lane_state!(ControlledLaneState, "B 端（被控）加密业务通道状态。");

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::test_support::{
        PairingFixture, lease_context, paired_session_keys, pairing_fixture, role_session_keys,
    };
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use serde_json::json;

    fn capabilities() -> Vec<String> {
        vec![
            "encrypted_frames_v1".to_owned(),
            "pairing_v1".to_owned(),
            "task_v1".to_owned(),
        ]
    }

    /// 用同一份握手材料派生两端通道：A 的 send 对应 B 的 receive，反之亦然。
    fn paired_lanes() -> (EncryptedLane, EncryptedLane, LeaseContext) {
        let PairingFixture {
            reservation,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture("relay-key-lane");
        let (a_session, b_session) = paired_session_keys(a_keys, b_keys, &reservation);
        let lease = lease_context(&reservation);
        let a = EncryptedLane::new(ControllerRole::A, &lease, a_session).unwrap();
        let b = EncryptedLane::new(ControllerRole::B, &lease, b_session).unwrap();
        (a, b, lease)
    }

    fn task_submit_payload(lease: &LeaseContext) -> Value {
        json!({
            "v": 1,
            "type": "task_submit",
            "connection_id": lease.connection_id,
            "controller_lease_id": lease.controller_lease_id,
            "lease_epoch": lease.lease_epoch,
            "relay_epoch": lease.relay_epoch,
            "remote_control_generation": lease.remote_control_generation,
            "request_id": "request-task-1",
            "task_id": "task-0001",
            "client_request_id": "client-request-1",
            "session_id": "session-0001",
            "capability_revision": "4",
            "model_source": "controlled_device",
            "mode": "chat",
            "input_digest": "0".repeat(64),
            "input": {"text": "hello", "attachment_ids": [], "skill_ids": []},
        })
    }

    fn task_status_payload(lease: &LeaseContext) -> Value {
        json!({
            "v": 1,
            "type": "task_status",
            "connection_id": lease.connection_id,
            "controller_lease_id": lease.controller_lease_id,
            "lease_epoch": lease.lease_epoch,
            "relay_epoch": lease.relay_epoch,
            "remote_control_generation": lease.remote_control_generation,
            "request_id": "request-status-1",
            "task_id": "task-0001",
            "session_id": "session-0001",
            "task_status": "admitted",
        })
    }

    #[test]
    fn encrypted_frames_round_trip_between_roles_without_plaintext_leak() {
        let (mut a, mut b, lease) = paired_lanes();
        let payload = task_submit_payload(&lease);
        let frame = a.seal("task_submit", &payload, &capabilities()).unwrap();
        // 信封里不得出现明文或摘要。
        let encoded = frame.to_string();
        assert!(!encoded.contains("hello"));
        assert!(!encoded.contains(&"0".repeat(64)));
        assert_eq!(frame["direction"], "a_to_b");
        assert_eq!(frame["payload_kind"], "task_submit");
        let (kind, opened) = b.open(&frame, &capabilities()).unwrap();
        assert_eq!(kind, "task_submit");
        assert_eq!(opened, payload);

        // 反向：B 的状态回传只有 A 能用同一通道打开。
        let status = task_status_payload(&lease);
        let frame = b.seal("task_status", &status, &capabilities()).unwrap();
        assert_eq!(frame["direction"], "b_to_a");
        let (kind, opened) = a.open(&frame, &capabilities()).unwrap();
        assert_eq!(kind, "task_status");
        assert_eq!(opened, status);
    }

    #[test]
    fn replay_wrong_direction_and_tampering_fail_closed() {
        let (mut a, mut b, lease) = paired_lanes();
        let payload = task_submit_payload(&lease);
        let first = a.seal("task_submit", &payload, &capabilities()).unwrap();
        let second = a.seal("task_submit", &payload, &capabilities()).unwrap();
        assert_eq!(first["frame_sequence"], "1");
        assert_eq!(second["frame_sequence"], "2");
        // 冻结设计带重放窗口：窗口内尚未消费的序号允许乱序到达，
        b.open(&second, &capabilities()).unwrap();
        b.open(&first, &capabilities()).unwrap();
        assert_eq!(
            b.open(&first, &capabilities()).unwrap_err(),
            LaneFailure::Replay,
            "已提交过的序号不得再次消费"
        );
        // 同一帧重复投递同样被拒绝。
        let third = a.seal("task_submit", &payload, &capabilities()).unwrap();
        b.open(&third, &capabilities()).unwrap();
        assert_eq!(b.open(&third, &capabilities()).unwrap_err(), LaneFailure::Replay);

        // 方向/载荷类型/密文任一被篡改都失败关闭。
        let mut flipped = a.seal("task_submit", &payload, &capabilities()).unwrap();
        flipped["direction"] = json!("b_to_a");
        assert_eq!(b.open(&flipped, &capabilities()).unwrap_err(), LaneFailure::Invalid);
        let mut wrong_kind = a.seal("task_submit", &payload, &capabilities()).unwrap();
        wrong_kind["payload_kind"] = json!("task_cancel");
        assert_eq!(
            b.open(&wrong_kind, &capabilities()).unwrap_err(),
            LaneFailure::Invalid
        );
        let mut tampered = a.seal("task_submit", &payload, &capabilities()).unwrap();
        tampered["ciphertext_b64"] = json!("AQID");
        assert!(matches!(
            b.open(&tampered, &capabilities()).unwrap_err(),
            LaneFailure::Invalid | LaneFailure::Replay
        ));
    }

    #[test]
    fn foreign_lease_and_missing_capabilities_fail_closed() {
        let (mut a, mut b, lease) = paired_lanes();
        let payload = task_submit_payload(&lease);
        let frame = a.seal("task_submit", &payload, &capabilities()).unwrap();
        // 能力未协商：外层校验即拒绝。
        assert_eq!(
            b.open(&frame, &["pairing_v1".to_owned()]).unwrap_err(),
            LaneFailure::Invalid
        );
        // 跨 lease（旧 generation/epoch）帧被拒。
        let mut foreign = frame.clone();
        foreign["remote_control_generation"] = json!("generation-other");
        assert_eq!(
            b.open(&foreign, &capabilities()).unwrap_err(),
            LaneFailure::Invalid
        );
        let mut foreign_epoch = frame.clone();
        foreign_epoch["lease_epoch"] = json!("2");
        assert_eq!(
            b.open(&foreign_epoch, &capabilities()).unwrap_err(),
            LaneFailure::Invalid
        );
        // 正常帧仍然可用（失败关闭不影响合法帧）。
        b.open(&frame, &capabilities()).unwrap();
    }

    #[test]
    fn lane_state_installs_clears_and_requires_the_handshake_material() {
        let state = ControllingLaneState::default();
        assert!(!state.is_installed());
        assert_eq!(
            state
                .seal("task_submit", &json!({}), &capabilities())
                .unwrap_err(),
            LaneFailure::Invalid
        );
        let PairingFixture {
            reservation,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture("relay-key-lane-state");
        let session = role_session_keys(
            ControllerRole::A,
            a_keys,
            b_keys.public_key().to_bytes(),
            &reservation,
        );
        let lease = lease_context(&reservation);
        state.install(ControllerRole::A, &lease, session).unwrap();
        assert!(state.is_installed());
        let payload = task_submit_payload(&lease);
        let frame = state.seal("task_submit", &payload, &capabilities()).unwrap();
        assert_eq!(frame["direction"], "a_to_b");
        state.clear();
        assert!(!state.is_installed());
        assert_eq!(
            state
                .seal("task_submit", &payload, &capabilities())
                .unwrap_err(),
            LaneFailure::Invalid
        );
    }

    /// 另一对握手密钥无法打开本对的帧（会话密钥绑定 + AEAD 失败关闭）。
    #[test]
    fn another_pairs_session_keys_cannot_open_the_frame() {
        let (mut a, _b, lease) = paired_lanes();
        let payload = task_submit_payload(&lease);
        let frame = a.seal("task_submit", &payload, &capabilities()).unwrap();
        let (_other_a, mut other_b, _) = paired_lanes();
        assert_eq!(
            other_b.open(&frame, &capabilities()).unwrap_err(),
            LaneFailure::Invalid
        );
    }

    /// E2E-06：A 端存活证明经通道封装后只有 B 能打开；角色不匹配、外层租约不符
    /// 或内层不是 `controller_liveness_a` 都失败关闭。
    #[test]
    fn liveness_proof_round_trips_through_the_lane() {
        let (a, b, lease) = paired_lanes();
        let inner = json!({
            "v": 1,
            "type": "controller_liveness_a",
            "connection_id": lease.connection_id,
            "controller_lease_id": lease.controller_lease_id,
            "lease_epoch": lease.lease_epoch,
            "relay_epoch": lease.relay_epoch,
            "remote_control_generation": lease.remote_control_generation,
            "role": "A",
            "liveness_epoch_a": "live-a-1",
            "heartbeat_sequence_a": "1",
            "challenge_id": "challenge-0001",
            "challenge_b64": "AAAAAAAAAAAAAAAAAAAAAA==",
        });
        let frame = a
            .seal_liveness(
                &inner,
                &[
                    "encrypted_frames_v1".to_owned(),
                    "pairing_v1".to_owned(),
                    "role_liveness_v1".to_owned(),
                ],
            )
            .unwrap();
        assert_eq!(frame["type"], "encrypted_controller_liveness_a");
        assert!(!frame.to_string().contains("live-a-1"));
        assert_eq!(b.open_liveness(&frame).unwrap(), inner);
        // 外层租约不符：旧 lease 的证明不得被接受。
        let mut foreign = frame.clone();
        foreign["lease_epoch"] = json!("2");
        assert_eq!(b.open_liveness(&foreign).unwrap_err(), LaneFailure::Invalid);
        // 角色不匹配：B 不能封装存活证明。
        assert_eq!(
            b.seal_liveness(&inner, &capabilities()).unwrap_err(),
            LaneFailure::Invalid
        );
        // 内层类型不符失败关闭。
        let mut wrong_inner = inner.clone();
        wrong_inner["type"] = json!("controller_liveness");
        assert_eq!(
            a.seal_liveness(&wrong_inner, &capabilities()).unwrap_err(),
            LaneFailure::Invalid
        );
    }
}
