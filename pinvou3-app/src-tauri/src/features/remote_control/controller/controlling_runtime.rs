//! A 端握手 worker（E2E-02）。
//!
//! 把 Relay 端口上的报文按冻结顺序喂给 [`ControllingHandshake`]，并把每个
//! 阶段结果投影给调用方 sink（命令层据此更新 `ControllingDeviceManager` 与
//! 前端状态）。本模块不拥有网络、不直接依赖 Tauri：generation 绑定、超时与
//! 重连由端口实现负责。

use serde_json::Value;

use super::controlling_handshake::{ControllingHandshake, HandshakeError, JoinedLease};

/// 端口抽象：`send` 交给传输层出站，`recv` 返回下一条入站报文。
#[async_trait::async_trait]
pub(crate) trait ControllingRelayPort: Send {
    fn send(&self, message: Value) -> Result<(), String>;
    async fn recv(&mut self) -> Option<Value>;

    /// Relay 明确返回的公共错误码（若有）。端口实现负责把它收敛到冻结的
    /// 公共错误闭集，状态所有者据此向用户给出有限错误。
    fn relay_error_code(&self) -> Option<&'static str> {
        None
    }

    /// 通道是否已断开。用于把失败映射为「对端不可达」而不是协议错误。
    fn lane_disconnected(&self) -> bool {
        false
    }
}

/// worker 向状态所有者投影的阶段事件。
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) enum ControllingStageEvent {
    /// claim 已发出（本地进入 connecting）。
    ClaimSent,
    /// transcript 已验签、预留已建立（本地进入 handshake）。事件携带验签后的
    /// generation：A 端据此一次性绑定 transport，之后才能安装租约。
    Reserved { remote_control_generation: String },
    /// 本端 Finished 已发出。
    FinishedSent,
    /// 收到 controller_joined，租约身份可用。
    Joined,
}

/// 握手完成后的租约材料：租约身份来自 `controller_joined`，能力与 transcript
/// 哈希来自验签后的 `controller_pairing_reserved`。调用方据此安装认证租约并
/// 派生传输指纹；本模块不产生任何可安装材料以外的状态。
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ControllingHandshakeOutcome {
    pub(crate) lease: JoinedLease,
    pub(crate) negotiated_capabilities: Vec<String>,
    pub(crate) transcript_hash: [u8; 32],
}

fn kind(message: &Value) -> Option<&str> {
    message.get("type").and_then(Value::as_str)
}

/// 按协议顺序推进 A 端握手，直到 `controller_joined`。
///
/// 任何未知类型、顺序错乱、绑定不匹配或验签失败都会立即返回错误，且不会
/// 产生任何可安装的租约材料。阶段回调返回错误时立即中止（例如本地
/// generation 绑定失败或操作已被新的连接取代）。
pub(crate) async fn drive_controlling_handshake<P: ControllingRelayPort>(
    port: &mut P,
    handshake: &mut ControllingHandshake,
    mut on_stage: impl FnMut(ControllingStageEvent) -> Result<(), HandshakeError>,
) -> Result<ControllingHandshakeOutcome, HandshakeError> {
    let claim = handshake.claim()?;
    port.send(claim)
        .map_err(|_| HandshakeError::new("controller_claim_failed"))?;
    on_stage(ControllingStageEvent::ClaimSent)?;

    // 1) 等 controller_pairing_reserved，产出本端 commit。
    let reserved = next_of_type(port, "controller_pairing_reserved").await?;
    let commit = handshake.accept_reserved(&reserved)?;
    port.send(commit)
        .map_err(|_| HandshakeError::new("controller_commit_failed"))?;
    on_stage(ControllingStageEvent::Reserved {
        remote_control_generation: handshake.remote_control_generation().to_owned(),
    })?;

    // 2) 等对端 commit，产出本端 Finished。
    let peer_commit = next_of_type(port, "finished_receipt_commit").await?;
    let finished = handshake.accept_peer_commitment(&peer_commit)?;
    port.send(finished)
        .map_err(|_| HandshakeError::new("controller_finished_failed"))?;
    on_stage(ControllingStageEvent::FinishedSent)?;

    // 3) 等对端 Finished，产出本端 receipt ack。
    let peer_finished = next_of_type(port, "encrypted_handshake_finished").await?;
    let ack = handshake.accept_peer_finished(&peer_finished)?;
    port.send(ack)
        .map_err(|_| HandshakeError::new("controller_ack_failed"))?;

    // 4) 等 controller_joined：只有到这里才产生租约身份。
    let joined = next_of_type(port, "controller_joined").await?;
    let lease = handshake.accept_joined(&joined)?;
    on_stage(ControllingStageEvent::Joined)?;
    let transcript_hash = handshake
        .transcript_hash()
        .ok_or_else(|| HandshakeError::new("controller_handshake_stage_invalid"))?;
    Ok(ControllingHandshakeOutcome {
        lease,
        negotiated_capabilities: handshake.negotiated_capabilities().to_vec(),
        transcript_hash,
    })
}

async fn next_of_type<P: ControllingRelayPort>(
    port: &mut P,
    expected: &'static str,
) -> Result<Value, HandshakeError> {
    match port.recv().await {
        Some(message) if kind(&message) == Some(expected) => Ok(message),
        // 端口断开或消息类型不符都失败关闭：迟到/错序报文不得被当作下一步。
        _ => Err(HandshakeError::new("controller_handshake_rejected")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine as _;
    use ring::rand::SystemRandom;
    use ring::signature::{Ed25519KeyPair, KeyPair};
    use sha2::{Digest, Sha256};
    use std::collections::VecDeque;

    use super::super::controlling_handshake::HandshakeStage;
    use super::super::crypto::{AuthenticatedRelayTrustConfig, HandshakeKeyPair};
    use super::super::protocol::build_pairing_transcript_preimage;

    struct FakePort {
        inbound: VecDeque<Value>,
        outbound: Vec<Value>,
    }

    #[async_trait::async_trait]
    impl ControllingRelayPort for FakePort {
        fn send(&self, _message: Value) -> Result<(), String> {
            // 记录在测试主体里完成：这里只保证不把错误往外抛。
            Ok(())
        }

        async fn recv(&mut self) -> Option<Value> {
            self.inbound.pop_front()
        }
    }

    fn signed_relay() -> (AuthenticatedRelayTrustConfig, Ed25519KeyPair, String) {
        let rng = SystemRandom::new();
        let pkcs8 = Ed25519KeyPair::generate_pkcs8(&rng).unwrap();
        let key_pair = Ed25519KeyPair::from_pkcs8(pkcs8.as_ref()).unwrap();
        let key_id = "relay-key-runtime".to_owned();
        let trust = AuthenticatedRelayTrustConfig::from_pinned_keys([(
            key_id.clone(),
            base64::engine::general_purpose::STANDARD.encode(key_pair.public_key().as_ref()),
        )])
        .unwrap();
        (trust, key_pair, key_id)
    }

    fn signed_reservation(
        key_pair: &Ed25519KeyPair,
        key_id: &str,
        a_public: &str,
        b_public: &str,
    ) -> Value {
        let fixture: Value = serde_json::from_str(include_str!(
            "../../../../../../remote-control-relay/test/fixtures/controller-protocol-v1.json"
        ))
        .unwrap();
        let mut message =
            fixture["lease_install_vector"]["verification_input"]["reserved_message"].clone();
        let mut transcript = message["transcript"].clone();
        transcript["handshake_public_key_a_b64"] = serde_json::json!(a_public);
        transcript["handshake_public_key_b_b64"] = serde_json::json!(b_public);
        let preimage = build_pairing_transcript_preimage(&transcript).unwrap();
        message["transcript"] = transcript;
        message["transcript_hash"] = serde_json::json!(hex(&Sha256::digest(&preimage)));
        message["relay_signing_key_id"] = serde_json::json!(key_id);
        message["relay_signature_b64"] = serde_json::json!(
            base64::engine::general_purpose::STANDARD.encode(key_pair.sign(&preimage).as_ref())
        );
        message
    }

    fn hex(bytes: &[u8]) -> String {
        bytes.iter().map(|byte| format!("{byte:02x}")).collect()
    }

    /// worker 只做编排：一条错序报文必须失败关闭，且不产生 Joined 事件。
    #[tokio::test]
    async fn out_of_order_inbound_fails_closed_without_joined() {
        let (trust, _, _) = signed_relay();
        let mut handshake = ControllingHandshake::new(
            "request-claim-0001",
            "claim-0001",
            "2345678",
            HandshakeKeyPair::generate().unwrap(),
            trust,
        );
        let mut port = FakePort {
            inbound: VecDeque::from(vec![serde_json::json!({
                "v": 1, "type": "controller_joined"
            })]),
            outbound: Vec::new(),
        };
        let mut stages = Vec::new();
        let error = drive_controlling_handshake(&mut port, &mut handshake, |event| {
            stages.push(event);
            Ok(())
        })
        .await
        .unwrap_err();
        assert_eq!(error.code, "controller_handshake_rejected");
        assert_eq!(stages, vec![ControllingStageEvent::ClaimSent]);
        assert_eq!(handshake.stage(), HandshakeStage::Claimed);
    }

    /// 阶段回调（例如 transport generation 绑定或本地状态投影）失败时必须
    /// 立即中止：不继续推进握手，也不产生任何可安装的租约材料。
    #[tokio::test]
    async fn stage_callback_failure_aborts_before_any_lease_material() {
        let (trust, relay_key, key_id) = signed_relay();
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let b_keys = HandshakeKeyPair::generate().unwrap();
        let encoded = |bytes: [u8; 32]| base64::engine::general_purpose::STANDARD.encode(bytes);
        let reserved = signed_reservation(
            &relay_key,
            &key_id,
            &encoded(a_keys.public_key().to_bytes()),
            &encoded(b_keys.public_key().to_bytes()),
        );
        let mut handshake = ControllingHandshake::new(
            reserved["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            trust,
        );
        let mut port = FakePort {
            inbound: VecDeque::from(vec![reserved]),
            outbound: Vec::new(),
        };
        let error = drive_controlling_handshake(&mut port, &mut handshake, |event| match event {
            ControllingStageEvent::Reserved { .. } => {
                Err(HandshakeError::new("controller_generation_bind_failed"))
            }
            _ => Ok(()),
        })
        .await
        .unwrap_err();
        assert_eq!(error.code, "controller_generation_bind_failed");
        assert_eq!(handshake.stage(), HandshakeStage::Reserved);
        assert!(
            handshake.take_session().is_err(),
            "an aborted stage must not yield installable lease material"
        );
    }

    /// 正向路径：端口按协议顺序投喂 B 侧协商结果，worker 走到 Joined。
    #[tokio::test]
    async fn happy_path_reaches_joined_with_the_expected_stage_order() {
        use std::cell::RefCell;

        struct ScriptedPort {
            step: usize,
            reserved: Value,
            joined: Value,
            peer: super::super::controlled_handshake::ControlledHandshake,
            outbound: RefCell<Vec<Value>>,
        }

        #[async_trait::async_trait]
        impl ControllingRelayPort for ScriptedPort {
            fn send(&self, message: Value) -> Result<(), String> {
                self.outbound.borrow_mut().push(message);
                Ok(())
            }

            async fn recv(&mut self) -> Option<Value> {
                self.step += 1;
                match self.step {
                    1 => Some(self.reserved.clone()),
                    2 => Some(self.peer.commit().unwrap()),
                    3 => {
                        // A 的 commit 已经在上一轮发出，但只在 Finished 阶段
                        // 才允许 B 回送自己的 Finished。
                        let a_commit = self
                            .outbound
                            .borrow()
                            .iter()
                            .find(|message| message["type"] == "finished_receipt_commit")?
                            .clone();
                        self.peer.accept_peer_commitment(&a_commit).unwrap();
                        Some(self.peer.finished().unwrap())
                    }
                    4 => Some(self.joined.clone()),
                    _ => None,
                }
            }
        }

        let (trust, relay_key, key_id) = signed_relay();
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let b_keys = HandshakeKeyPair::generate().unwrap();
        let a_public = a_keys.public_key().to_bytes();
        let b_public = b_keys.public_key().to_bytes();
        let encoded = |bytes: [u8; 32]| base64::engine::general_purpose::STANDARD.encode(bytes);
        let reserved =
            signed_reservation(&relay_key, &key_id, &encoded(a_public), &encoded(b_public));
        let joined = serde_json::json!({
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
            "resume_token_b64": base64::engine::general_purpose::STANDARD.encode([7_u8; 32]),
            "resume_deadline_ms": "30000",
        });
        let peer =
            super::super::controlled_handshake::ControlledHandshake::new(&reserved, b_keys, &trust)
                .unwrap();
        let mut a = ControllingHandshake::new(
            reserved["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            trust,
        );
        let mut port = ScriptedPort {
            step: 0,
            reserved,
            joined,
            peer,
            outbound: RefCell::new(Vec::new()),
        };
        let mut stages = Vec::new();
        let outcome = drive_controlling_handshake(&mut port, &mut a, |event| {
            stages.push(event);
            Ok(())
        })
        .await
        .unwrap();
        let lease = outcome.lease;
        assert_eq!(lease.controller_lease_id, "controller-lease-0001");
        assert_eq!(lease.lease_epoch, 1);
        assert_eq!(
            stages,
            vec![
                ControllingStageEvent::ClaimSent,
                ControllingStageEvent::Reserved {
                    remote_control_generation: "generation-0001".into(),
                },
                ControllingStageEvent::FinishedSent,
                ControllingStageEvent::Joined,
            ]
        );
        assert_eq!(lease.liveness_epoch_a, "liveness-epoch-a-0001");
        assert_eq!(lease.resume_deadline_ms, 30_000);
        assert_eq!(lease.resume_token, [7_u8; 32]);
        let debug = format!("{lease:?}");
        assert!(debug.contains("[REDACTED]"));
        assert!(
            !debug.contains(&base64::engine::general_purpose::STANDARD.encode(lease.resume_token)),
            "恢复凭证不得出现在 Debug 输出里"
        );
        assert_eq!(outcome.transcript_hash.len(), 32);
        assert!(
            outcome
                .negotiated_capabilities
                .contains(&"pairing_v1".to_owned())
        );
        // worker 只负责编排；业务密钥仍在调用方用真实 LeaseBinding 派生。
        assert!(a.take_session().is_ok());
        let outbound_types: Vec<String> = port
            .outbound
            .borrow()
            .iter()
            .map(|message| message["type"].as_str().unwrap().to_owned())
            .collect();
        assert_eq!(
            outbound_types,
            vec![
                "controller_pairing_claim",
                "finished_receipt_commit",
                "encrypted_handshake_finished",
                "finished_receipt_ack",
            ]
        );
    }
}
