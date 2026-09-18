//! A 端（主控）连接接线（E2E-02 第 2 步）。
//!
//! 把冻结握手序列接到真实 Relay transport 与 `ControllingDeviceManager`：
//! 生成握手密钥、加载受信 Relay 信任根、在验签 transcript 上一次性绑定
//! transport generation、驱动 claim→joined、安装认证租约并把阶段投影给状态
//! 所有者。失败/超时/断线统一映射为有限公共错误，且不产生半租约。
//!
//! 首版只接线普通会话所需的握手与租约；业务帧派发留给 E2E-04，liveness 与
//! resume 留给 E2E-06。

use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;
use tauri::{AppHandle, Emitter, Manager as _};
use tokio::sync::mpsc;
use uuid::Uuid;

use super::controlling::{
    ControllerLease, ControllerOperation, ControllingDeviceManager, ControllingStatus,
};
use super::controlling_handshake::{ControllingHandshake, HandshakeError};
use super::controlling_runtime::{
    ControllingHandshakeOutcome, ControllingRelayPort, ControllingStageEvent,
    drive_controlling_handshake,
};
use super::crypto::{
    ControllerRole as HandshakeRole, HandshakeKeyPair, load_relay_trust_config,
    transport_key_fingerprint,
};
use super::protocol::LeaseContext;
use super::relay_transport::{
    AuthenticatedLease, ControllerRole, ControllerTransportConfig, ControllerTransportEvent,
    ControllerTransportSender, spawn_controller_transport,
};
use crate::features::remote_control::RemoteControlManager;

pub(crate) const CONTROLLER_STATUS_EVENT: &str = "desktop-remote-controller-status";

/// claim→joined 的本地上限。Relay 侧预留 10 秒、握手 20 秒（DEV-04），这里取
/// 40 秒兜底：超过即失败关闭，不留下半租约，也不会无限停留在 connecting。
const HANDSHAKE_DEADLINE: Duration = Duration::from_secs(40);
/// 冻结协议 v1 不携带设备显示名（`controller_joined` 无该字段）：首版用有界
/// 占位，真实来源待协议/会话初始化补齐（见任务文档的已登记决策）。
const DEVICE_DISPLAY_NAME_PLACEHOLDER: &str = "remote-device";
/// 协议与握手层失败的兜底公共错误码（CONTROLLER_ERROR_CODES 成员）。
const GENERIC_CONNECT_ERROR: &str = "invalid_message";
/// 对端不可达（断线或超时）的公共错误码。
const PEER_UNAVAILABLE_ERROR: &str = "endpoint_offline";

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) enum ControllerLaneEvent {
    Frame(String),
    Disconnected,
}

/// 握手完成后的通道抽象。只暴露「下一条通道事件」，使断线投影可以在不依赖
/// Tauri 的情况下被测试。
#[async_trait::async_trait]
pub(crate) trait ControllingLane: Send {
    async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent>;
}

/// A 端 Relay 端口。入站帧由 `spawn_controller_transport` 的事件 sink 投递；
/// 出站一律走敏感队列（claim 自带连接码、receipt ack 自带 receipt secret）。
pub(crate) struct ControllerTransportPort {
    sender: ControllerTransportSender,
    inbound: mpsc::UnboundedReceiver<ControllerLaneEvent>,
    relay_error_code: Option<&'static str>,
    lane_disconnected: bool,
}

#[async_trait::async_trait]
impl ControllingLane for ControllerTransportPort {
    async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
        self.inbound.recv().await
    }
}

#[async_trait::async_trait]
impl ControllingRelayPort for ControllerTransportPort {
    fn send(&self, message: Value) -> Result<(), String> {
        self.sender
            .try_send_sensitive_control(&message)
            .map_err(|_| "controller_lane_send_failed".to_owned())
    }

    async fn recv(&mut self) -> Option<Value> {
        loop {
            match self.next_lane_event().await? {
                ControllerLaneEvent::Disconnected => {
                    self.lane_disconnected = true;
                    return None;
                }
                ControllerLaneEvent::Frame(json) => {
                    let Ok(value) = serde_json::from_str::<Value>(&json) else {
                        continue;
                    };
                    if value.get("type").and_then(Value::as_str) == Some("error") {
                        self.relay_error_code = relay_error_code(&value);
                    }
                    return Some(value);
                }
            }
        }
    }

    fn relay_error_code(&self) -> Option<&'static str> {
        self.relay_error_code
    }

    fn lane_disconnected(&self) -> bool {
        self.lane_disconnected
    }
}

/// 绑定一条真实 Relay transport，并把入站事件投影为端口事件。generation 以
/// 空串启动：A 端只有验签 transcript 之后才会通过 `bind_generation` 绑定它。
fn spawn_controlling_lane(relay_url: &str) -> (ControllerTransportSender, ControllerTransportPort) {
    let (inbound_tx, inbound_rx) = mpsc::unbounded_channel();
    let sink = Arc::new(move |event: ControllerTransportEvent| match event {
        ControllerTransportEvent::Frame { json, .. } => {
            let _ = inbound_tx.send(ControllerLaneEvent::Frame(json));
        }
        ControllerTransportEvent::Disconnected { .. } => {
            let _ = inbound_tx.send(ControllerLaneEvent::Disconnected);
        }
        ControllerTransportEvent::Connected { .. } | ControllerTransportEvent::RttSample { .. } => {
        }
    });
    let sender = spawn_controller_transport(
        ControllerTransportConfig::new(relay_url, ControllerRole::ControllerA, String::new()),
        sink,
    );
    let port = ControllerTransportPort {
        sender: sender.clone(),
        inbound: inbound_rx,
        relay_error_code: None,
        lane_disconnected: false,
    };
    (sender, port)
}

/// Relay `error` 帧里的公共错误码闭集。未知或畸形码一律退回兜底码，绝不把
/// 对端文本透传到本地面。
fn relay_error_code(value: &Value) -> Option<&'static str> {
    match value.get("code").and_then(Value::as_str)? {
        "pairing_code_invalid" => Some("pairing_code_invalid"),
        "controller_busy" => Some("controller_busy"),
        "endpoint_offline" => Some("endpoint_offline"),
        "version_incompatible" => Some("version_incompatible"),
        "invalid_message" => Some("invalid_message"),
        _ => None,
    }
}

/// 失败分类：Relay 明确给出的码优先；断线与本地上限超时归为对端不可达；
/// 其余（验签失败、顺序错误、本地 generation 绑定失败）归为协议错误。
fn public_failure_code(port: &impl ControllingRelayPort, timed_out: bool) -> &'static str {
    if let Some(code) = port.relay_error_code() {
        return code;
    }
    if timed_out || port.lane_disconnected() {
        return PEER_UNAVAILABLE_ERROR;
    }
    GENERIC_CONNECT_ERROR
}

/// A 端连接核心：推到 `controller_joined`、安装租约并进入 connected。
///
/// 不依赖 Tauri（状态广播通过回调注入）；任何失败都会停止本次 transport、
/// 清空连接码并把状态投影为带公共错误码的 error，绝不产生半租约。
pub(crate) async fn run_controlling_connection<P: ControllingRelayPort>(
    manager: &ControllingDeviceManager,
    operation: ControllerOperation,
    port: &mut P,
    handshake: &mut ControllingHandshake,
    sender: &ControllerTransportSender,
    timeout: Duration,
    on_status: &mut impl FnMut(&ControllingStatus),
) -> Result<ControllingStatus, String> {
    let driven = tokio::time::timeout(
        timeout,
        drive_controlling_handshake(port, handshake, |stage| match stage {
            ControllingStageEvent::ClaimSent => Ok(()),
            ControllingStageEvent::Reserved {
                remote_control_generation,
            } => {
                sender
                    .bind_generation(&remote_control_generation)
                    .map_err(|_| HandshakeError::new("controller_generation_bind_failed"))?;
                let status = manager
                    .complete_reserved(operation.clone())
                    .map_err(|error| HandshakeError::new(error.code))?;
                on_status(&status);
                Ok(())
            }
            ControllingStageEvent::FinishedSent => Ok(()),
            // 租约材料在 driver 返回后统一安装：这里只推进阶段。
            ControllingStageEvent::Joined => Ok(()),
        }),
    )
    .await;

    let outcome = match driven {
        Ok(Ok(outcome)) => outcome,
        Ok(Err(_handshake_error)) => {
            let code = public_failure_code(port, false);
            sender.stop();
            return Err(fail_connection(manager, operation, code, on_status));
        }
        Err(_elapsed) => {
            let code = public_failure_code(port, true);
            sender.stop();
            return Err(fail_connection(manager, operation, code, on_status));
        }
    };

    match complete_connected(manager, operation, sender, outcome) {
        Ok(status) => {
            on_status(&status);
            Ok(status)
        }
        Err(code) => {
            sender.stop();
            Err(code)
        }
    }
}

/// 安装认证租约并把状态推进到 connected。租约安装先于状态投影：状态一旦
/// 是 connected，transport 上必然存在同一 generation 的认证租约。
fn complete_connected(
    manager: &ControllingDeviceManager,
    operation: ControllerOperation,
    sender: &ControllerTransportSender,
    outcome: ControllingHandshakeOutcome,
) -> Result<ControllingStatus, String> {
    let context = LeaseContext {
        connection_id: outcome.lease.connection_id.clone(),
        controller_lease_id: outcome.lease.controller_lease_id.clone(),
        lease_epoch: outcome.lease.lease_epoch.to_string(),
        relay_epoch: outcome.lease.relay_epoch.clone(),
        remote_control_generation: outcome.lease.remote_control_generation.clone(),
    };
    let fingerprint = transport_key_fingerprint(outcome.transcript_hash, HandshakeRole::A)
        .map_err(|_| GENERIC_CONNECT_ERROR.to_owned())?;
    let lease = ControllerLease {
        context: context.clone(),
        device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
        liveness_epoch_a: outcome.lease.liveness_epoch_a.clone(),
        resume_deadline_b: outcome.lease.resume_deadline_ms,
        resume_token: outcome.lease.resume_token,
        transport_key_fingerprint: fingerprint,
    };
    sender
        .install_authenticated_lease(AuthenticatedLease {
            context,
            negotiated_capabilities: outcome.negotiated_capabilities,
        })
        .map_err(|_| GENERIC_CONNECT_ERROR.to_owned())?;
    manager
        .complete_connected(operation, lease)
        .map_err(|error| error.code.to_owned())
}

fn fail_connection(
    manager: &ControllingDeviceManager,
    operation: ControllerOperation,
    public_code: &'static str,
    on_status: &mut impl FnMut(&ControllingStatus),
) -> String {
    match manager.fail_connection(operation, public_code) {
        Ok(status) => {
            on_status(&status);
            public_code.to_owned()
        }
        // 已被新的连接取代：新 intent 拥有状态，这里只回报有界错误码。
        Err(error) => error.code.to_owned(),
    }
}

/// 握手完成后的通道保活：断线投影为 reconnecting（恢复与 liveness 属 E2E-06），
/// 已认证业务帧留给 E2E-04 派发；首版对未知帧只做丢弃，绝不转发到 legacy
/// WebUI 事件流。
pub(crate) async fn serve_controlling_lane(
    lane: &mut (impl ControllingLane + ?Sized),
    manager: &ControllingDeviceManager,
    mut on_status: impl FnMut(&ControllingStatus),
) {
    while let Some(event) = lane.next_lane_event().await {
        if matches!(event, ControllerLaneEvent::Disconnected) {
            let status = manager.mark_transport_disconnected();
            on_status(&status);
        }
    }
}

/// A 端连接入口：加载信任根、生成握手密钥、绑定受控 Relay 通道、推进握手，
/// 并在成功后把通道保活交给后台任务。
pub(crate) async fn connect_controlling_device(
    app: &AppHandle,
    manager: &ControllingDeviceManager,
    relay: &RemoteControlManager,
    operation: ControllerOperation,
) -> Result<ControllingStatus, String> {
    let code = manager
        .pending_code(operation.clone())
        .map_err(|error| error.code.to_owned())?;
    // 部署期预置的信任根缺失或非法时失败关闭：没有受信 Relay 就不存在可验签
    // 的 transcript，绝不能降级为「无认证握手」。
    let trust = load_relay_trust_config(|name| std::env::var(name).ok())
        .map_err(|_| GENERIC_CONNECT_ERROR.to_owned())?;
    let keys = HandshakeKeyPair::generate().map_err(|_| GENERIC_CONNECT_ERROR.to_owned())?;
    let mut handshake = ControllingHandshake::new(
        format!("request-{}", Uuid::new_v4().simple()),
        operation.claim_request_id().to_owned(),
        code,
        keys,
        trust,
    );
    let (sender, mut port) = spawn_controlling_lane(&relay.controller_relay_ws_url());
    // 先登记 transport，保证并发的断开/重连能立刻停止本次尝试。
    manager.replace_transport(sender.clone());

    let mut emit = |status: &ControllingStatus| {
        let _ = app.emit(CONTROLLER_STATUS_EVENT, status);
    };
    let status = run_controlling_connection(
        manager,
        operation,
        &mut port,
        &mut handshake,
        &sender,
        HANDSHAKE_DEADLINE,
        &mut emit,
    )
    .await?;

    let keeper = app.clone();
    tauri::async_runtime::spawn(async move {
        let manager = keeper.state::<ControllingDeviceManager>();
        serve_controlling_lane(&mut port, manager.inner(), |status| {
            let _ = keeper.emit(CONTROLLER_STATUS_EVENT, status);
        })
        .await;
    });
    Ok(status)
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::collections::VecDeque;

    use base64::Engine as _;
    use ring::rand::SystemRandom;
    use ring::signature::{Ed25519KeyPair, KeyPair};
    use sha2::{Digest, Sha256};

    use super::*;
    use crate::features::remote_control::controller::controlled_handshake::ControlledHandshake;
    use crate::features::remote_control::controller::crypto::AuthenticatedRelayTrustConfig;
    use crate::features::remote_control::controller::protocol::build_pairing_transcript_preimage;
    use crate::features::remote_control::controller::relay_transport::transport_channels;

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
        let key_id = "relay-key-service".to_owned();
        let trust =
            AuthenticatedRelayTrustConfig::from_pinned_keys([(key_id.clone(), public_key)])
                .unwrap();
        SignedRelay {
            trust,
            key_id,
            key_pair,
        }
    }

    fn encoded(bytes: [u8; 32]) -> String {
        base64::engine::general_purpose::STANDARD.encode(bytes)
    }

    fn hex_lower(bytes: &[u8]) -> String {
        bytes.iter().map(|byte| format!("{byte:02x}")).collect()
    }

    /// 以冻结 fixture 的 transcript 为模板替换双方握手公钥并重新签名，得到
    /// 一份真实可验签的 `controller_pairing_reserved`。
    fn signed_reservation(
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
        transcript["handshake_public_key_a_b64"] = serde_json::json!(a_public_b64);
        transcript["handshake_public_key_b_b64"] = serde_json::json!(b_public_b64);
        let preimage = build_pairing_transcript_preimage(&transcript).unwrap();
        message["transcript"] = transcript;
        message["transcript_hash"] = serde_json::json!(hex_lower(&Sha256::digest(&preimage)));
        message["relay_signing_key_id"] = serde_json::json!(relay.key_id);
        message["relay_signature_b64"] = serde_json::json!(base64::engine::general_purpose::STANDARD
            .encode(relay.key_pair.sign(&preimage).as_ref()));
        message
    }

    fn joined_message(reserved: &Value, token: [u8; 32]) -> Value {
        serde_json::json!({
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

    /// 由真实 `ControlledHandshake` 扮演 B：按冻结顺序投喂 commit / Finished /
    /// joined，并用收到的 A 端报文推进自己的状态机。
    struct ControlledPeerPort {
        step: usize,
        reservation: Value,
        joined: Value,
        peer: ControlledHandshake,
        outbound: RefCell<Vec<Value>>,
    }

    #[async_trait::async_trait]
    impl ControllingRelayPort for ControlledPeerPort {
        fn send(&self, message: Value) -> Result<(), String> {
            self.outbound.borrow_mut().push(message);
            Ok(())
        }

        async fn recv(&mut self) -> Option<Value> {
            self.step += 1;
            match self.step {
                1 => Some(self.reservation.clone()),
                2 => Some(self.peer.commit().unwrap()),
                3 => {
                    let a_commit = self
                        .outbound
                        .borrow()
                        .iter()
                        .find(|message| message["type"] == "finished_receipt_commit")?
                        .clone();
                    self.peer.accept_peer_commitment(&a_commit).unwrap();
                    Some(self.peer.finished().unwrap())
                }
                4 => {
                    let a_finished = self
                        .outbound
                        .borrow()
                        .iter()
                        .find(|message| message["type"] == "encrypted_handshake_finished")?
                        .clone();
                    let ack = self.peer.accept_peer_finished(&a_finished).unwrap();
                    assert_eq!(ack["sender_role"], "A");
                    assert_eq!(ack["receiver_role"], "B");
                    Some(self.joined.clone())
                }
                _ => None,
            }
        }
    }

    /// 只按脚本投递入站帧的端口：用于失败路径（Relay 错误、断线）。
    struct ScriptedPort {
        inbound: VecDeque<Value>,
        relay_error: Option<&'static str>,
        disconnected: bool,
    }

    #[async_trait::async_trait]
    impl ControllingRelayPort for ScriptedPort {
        fn send(&self, _message: Value) -> Result<(), String> {
            Ok(())
        }

        async fn recv(&mut self) -> Option<Value> {
            if let Some(message) = self.inbound.pop_front() {
                if message["type"] == "error" {
                    self.relay_error = relay_error_code(&message);
                }
                return Some(message);
            }
            if self.disconnected {
                return None;
            }
            std::future::pending().await
        }

        fn relay_error_code(&self) -> Option<&'static str> {
            self.relay_error
        }

        fn lane_disconnected(&self) -> bool {
            self.disconnected
        }
    }

    struct TestLane {
        events: VecDeque<ControllerLaneEvent>,
    }

    #[async_trait::async_trait]
    impl ControllingLane for TestLane {
        async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
            self.events.pop_front()
        }
    }

    fn peer_port(
        reservation: Value,
        joined: Value,
        b_keys: HandshakeKeyPair,
        trust: &AuthenticatedRelayTrustConfig,
    ) -> ControlledPeerPort {
        let peer = ControlledHandshake::new(&reservation, b_keys, trust).unwrap();
        ControlledPeerPort {
            step: 0,
            reservation,
            joined,
            peer,
            outbound: RefCell::new(Vec::new()),
        }
    }

    fn a_handshake(
        reservation: &Value,
        a_keys: HandshakeKeyPair,
        trust: AuthenticatedRelayTrustConfig,
    ) -> ControllingHandshake {
        ControllingHandshake::new(
            reservation["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            trust,
        )
    }

    /// 状态投影就是前端契约：测试直接核对序列化后的字段，避免为测试放宽
    /// 状态所有者的私有字段。
    fn projection(status: &ControllingStatus) -> Value {
        serde_json::to_value(status).unwrap()
    }

    #[tokio::test]
    async fn happy_path_installs_the_lease_and_reaches_connected() {
        let relay = signed_relay();
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let b_keys = HandshakeKeyPair::generate().unwrap();
        let reservation = signed_reservation(
            &relay,
            &encoded(a_keys.public_key().to_bytes()),
            &encoded(b_keys.public_key().to_bytes()),
        );
        let joined = joined_message(&reservation, [7_u8; 32]);
        let mut port = peer_port(reservation.clone(), joined, b_keys, &relay.trust);
        let mut handshake = a_handshake(&reservation, a_keys, relay.trust.clone());

        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        let (sender, _receiver) =
            transport_channels(ControllerRole::ControllerA, "", 2, 2, 8192, 1024);
        let mut statuses = Vec::new();
        let status = run_controlling_connection(
            &manager,
            operation.clone(),
            &mut port,
            &mut handshake,
            &sender,
            Duration::from_secs(5),
            &mut |status: &ControllingStatus| {
                statuses.push(projection(status)["status"].as_str().unwrap().to_owned());
            },
        )
        .await
        .unwrap();

        assert_eq!(projection(&status)["status"], "connected");
        assert_eq!(
            statuses,
            vec!["safety_handshake".to_owned(), "connected".to_owned()],
            "阶段投影必须按 connecting → safety_handshake → connected 推进"
        );
        assert_eq!(
            manager.live_lease().unwrap().controller_lease_id,
            "controller-lease-0001"
        );
        // generation 已绑定到验签 transcript，且租约带同一 generation 与协商能力。
        assert_eq!(sender.generation(), "generation-0001");
        let installed = sender.authenticated_lease().unwrap();
        assert_eq!(
            installed.context.remote_control_generation,
            "generation-0001"
        );
        assert_eq!(installed.context.connection_id, "connection-0001");
        assert_eq!(installed.context.lease_epoch, "1");
        assert_eq!(
            installed.negotiated_capabilities,
            vec![
                "encrypted_frames_v1".to_owned(),
                "pairing_v1".to_owned(),
                "task_v1".to_owned()
            ]
        );
    }

    #[tokio::test]
    async fn relay_rejection_is_surfaced_as_a_bounded_public_error() {
        let relay = signed_relay();
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let mut handshake = ControllingHandshake::new(
            "request-0001",
            "claim-0001",
            "2345678",
            a_keys,
            relay.trust.clone(),
        );
        let mut port = ScriptedPort {
            inbound: VecDeque::from(vec![serde_json::json!({
                "v": 1,
                "type": "error",
                "request_id": "request-0001",
                "code": "pairing_code_invalid",
                "message": "pairing code rejected",
            })]),
            relay_error: None,
            disconnected: false,
        };
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        let (sender, _receiver) =
            transport_channels(ControllerRole::ControllerA, "", 2, 2, 8192, 1024);
        let mut statuses = Vec::new();
        let error = run_controlling_connection(
            &manager,
            operation,
            &mut port,
            &mut handshake,
            &sender,
            Duration::from_secs(5),
            &mut |status: &ControllingStatus| statuses.push(status.clone()),
        )
        .await
        .unwrap_err();

        assert_eq!(error, "pairing_code_invalid");
        let status = projection(&manager.status());
        assert_eq!(status["status"], "error");
        assert_eq!(status["errorCode"], "pairing_code_invalid");
        assert_eq!(status["hasPendingCode"], false);
        assert!(statuses.last().is_some_and(|status| projection(status)["status"] == "error"));
        // 失败路径不留半租约，也不再占用 transport。
        assert!(sender.authenticated_lease().is_none());
        assert!(sender.is_stopped());
        assert_eq!(sender.generation(), "");
    }

    #[tokio::test]
    async fn disconnect_and_timeout_map_to_peer_unavailable() {
        for (disconnected, timeout, expected) in [
            (true, Duration::from_secs(5), "endpoint_offline"),
            (false, Duration::from_millis(20), "endpoint_offline"),
        ] {
            let relay = signed_relay();
            let a_keys = HandshakeKeyPair::generate().unwrap();
            let mut handshake = ControllingHandshake::new(
                "request-0001",
                "claim-0001",
                "2345678",
                a_keys,
                relay.trust.clone(),
            );
            let mut port = ScriptedPort {
                inbound: VecDeque::new(),
                relay_error: None,
                disconnected,
            };
            let manager = ControllingDeviceManager::default();
            let operation = manager.connect("234-4567").unwrap();
            let (sender, _receiver) =
                transport_channels(ControllerRole::ControllerA, "", 2, 2, 8192, 1024);
            let error = run_controlling_connection(
                &manager,
                operation,
                &mut port,
                &mut handshake,
                &sender,
                timeout,
                &mut |_: &ControllingStatus| {},
            )
            .await
            .unwrap_err();
            assert_eq!(error, expected);
            let status = projection(&manager.status());
            assert_eq!(status["status"], "error");
            assert_eq!(status["errorCode"], expected);
            assert!(sender.authenticated_lease().is_none());
            assert!(sender.is_stopped());
        }
    }

    #[tokio::test]
    async fn untrusted_transcript_and_generation_bind_failure_fail_closed() {
        // 1) 用另一把 Relay 签名密钥签发的 transcript 必须失败关闭。
        let trusted = signed_relay();
        let attacker = signed_relay();
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let b_keys = HandshakeKeyPair::generate().unwrap();
        let forged = signed_reservation(
            &attacker,
            &encoded(a_keys.public_key().to_bytes()),
            &encoded(b_keys.public_key().to_bytes()),
        );
        let mut handshake = a_handshake(&forged, a_keys, trusted.trust.clone());
        let mut port = ScriptedPort {
            inbound: VecDeque::from(vec![forged]),
            relay_error: None,
            disconnected: false,
        };
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        let (sender, _receiver) =
            transport_channels(ControllerRole::ControllerA, "", 2, 2, 8192, 1024);
        let error = run_controlling_connection(
            &manager,
            operation,
            &mut port,
            &mut handshake,
            &sender,
            Duration::from_secs(5),
            &mut |_: &ControllingStatus| {},
        )
        .await
        .unwrap_err();
        assert_eq!(error, "invalid_message");
        assert_eq!(projection(&manager.status())["errorCode"], "invalid_message");
        assert!(sender.authenticated_lease().is_none());
        assert_eq!(sender.generation(), "", "untrusted transcript must not bind");

        // 2) transport 已经绑定过 generation 时，Reserved 阶段必须中止。
        let relay = signed_relay();
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let b_keys = HandshakeKeyPair::generate().unwrap();
        let reservation = signed_reservation(
            &relay,
            &encoded(a_keys.public_key().to_bytes()),
            &encoded(b_keys.public_key().to_bytes()),
        );
        let joined = joined_message(&reservation, [7_u8; 32]);
        let mut port = peer_port(reservation.clone(), joined, b_keys, &relay.trust);
        let mut handshake = a_handshake(&reservation, a_keys, relay.trust.clone());
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        let (sender, _receiver) =
            transport_channels(ControllerRole::ControllerA, "generation-0000", 2, 2, 8192, 1024);
        let error = run_controlling_connection(
            &manager,
            operation,
            &mut port,
            &mut handshake,
            &sender,
            Duration::from_secs(5),
            &mut |_: &ControllingStatus| {},
        )
        .await
        .unwrap_err();
        assert_eq!(error, "invalid_message");
        assert_eq!(projection(&manager.status())["status"], "error");
        assert_eq!(sender.generation(), "generation-0000");
        assert!(sender.authenticated_lease().is_none());
        assert!(sender.is_stopped());
    }

    #[tokio::test]
    async fn post_handshake_disconnect_projects_reconnecting_and_keeps_draining() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation,
                ControllerLease {
                    context: LeaseContext {
                        connection_id: "connection-0001".into(),
                        controller_lease_id: "controller-lease-0001".into(),
                        lease_epoch: "1".into(),
                        relay_epoch: "epoch-test-1".into(),
                        remote_control_generation: "generation-0001".into(),
                    },
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "liveness-epoch-a-0001".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7_u8; 32],
                    transport_key_fingerprint: [9_u8; 32],
                },
            )
            .unwrap();
        let mut lane = TestLane {
            events: VecDeque::from(vec![
                ControllerLaneEvent::Disconnected,
                ControllerLaneEvent::Frame("{}".to_owned()),
            ]),
        };
        let mut statuses = Vec::new();
        serve_controlling_lane(&mut lane, &manager, |status| {
            statuses.push(projection(status)["status"].as_str().unwrap().to_owned());
        })
        .await;
        assert_eq!(statuses, vec!["reconnecting".to_owned()]);
        assert_eq!(
            projection(&manager.status())["status"],
            "reconnecting",
            "断线后不能继续声称 connected"
        );
    }
}
