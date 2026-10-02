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

use base64::Engine as _;
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
use super::controlling_session::{ControllingSessionManager, ControllingSessionProjection};
use super::crypto::{
    ControllerRole as HandshakeRole, HandshakeKeyPair, load_relay_trust_config,
    transport_key_fingerprint,
};
use super::encrypted_lane::ControllingLaneState;
use super::journal::{JournalEvent, MAX_EVENT_PAYLOAD_BYTES};
use super::protocol::LeaseContext;
use super::relay_transport::{
    AuthenticatedLease, ControllerRole, ControllerTransportConfig, ControllerTransportEvent,
    ControllerTransportSender, spawn_controller_transport,
};
use crate::features::remote_control::RemoteControlManager;

pub(crate) const CONTROLLER_STATUS_EVENT: &str = "desktop-remote-controller-status";
/// A 端远程会话投影事件（sessionId / capability revision）。
pub(crate) const CONTROLLER_SESSION_EVENT: &str = "desktop-remote-controller-session";
/// A 端任务状态事件（加密回传解密后的投影）。
pub(crate) const CONTROLLER_TASK_EVENT: &str = "desktop-remote-task-status";
/// A 端权威 journal 事件（E2E-05）：命令层据此写独立历史并投影正文。
pub(crate) const CONTROLLER_JOURNAL_EVENT: &str = "desktop-remote-controller-journal";

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

/// A 端权威 journal 落库端口（E2E-05）：由命令层实现——按会话引用写入 A 的
/// 独立历史，并把事件投影给 UI。feature 层只负责解密与校验。
pub(crate) trait ControllingJournalSink: Send + Sync {
    fn publish(&self, event: &JournalEvent);
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
    business: &ControllingLaneState,
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
            business.clear();
            return Err(fail_connection(manager, operation, code, on_status));
        }
        Err(_elapsed) => {
            let code = public_failure_code(port, true);
            sender.stop();
            business.clear();
            return Err(fail_connection(manager, operation, code, on_status));
        }
    };

    match complete_connected(manager, operation.clone(), sender, business, outcome) {
        Ok(status) => {
            on_status(&status);
            Ok(status)
        }
        Err(code) => {
            sender.stop();
            business.clear();
            // 本地收尾失败（指纹派生、租约安装）时不能停在非终态阶段：只有在
            // 本次尝试仍然待决时才投影为有界错误，已断开/已被取代时不覆盖。
            let _ = fail_connection(manager, operation, GENERIC_CONNECT_ERROR, on_status);
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
    business: &ControllingLaneState,
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
        negotiated_capabilities: outcome.negotiated_capabilities.clone(),
    };
    sender
        .install_authenticated_lease(AuthenticatedLease {
            context,
            negotiated_capabilities: outcome.negotiated_capabilities,
        })
        .map_err(|_| GENERIC_CONNECT_ERROR.to_owned())?;
    // 加密业务通道与租约同时就位：状态一旦进入 connected，业务帧既有租约准入
    // 也有会话密钥可用。
    business
        .install(HandshakeRole::A, &lease.context, outcome.session)
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
    if !manager.is_connecting(operation.clone()) {
        // 已被显式断开或新的连接取代：状态归对方所有，本次尝试只回报有界
        // 错误码，绝不覆盖别人的投影。
        return public_code.to_owned();
    }
    match manager.fail_connection(operation, public_code) {
        Ok(status) => {
            on_status(&status);
            public_code.to_owned()
        }
        // 竞态下仍可能失败（例如刚被取代）：同样只回报有界错误码。
        Err(error) => error.code.to_owned(),
    }
}

/// 通道保活期间的事件出口（状态/会话/任务），由 Tauri 层注入具体广播。
pub(crate) struct ControllingLaneSinks<'a> {
    pub(crate) on_status: &'a mut (dyn FnMut(&ControllingStatus) + Send),
    pub(crate) on_session: &'a mut (dyn FnMut(&ControllingSessionProjection) + Send),
    pub(crate) on_task: &'a mut (dyn FnMut(Value) + Send),
    /// 解密并校验过的权威 journal 事件（E2E-05）：A 端据此写独立历史并渲染正文。
    pub(crate) on_journal: &'a mut (dyn FnMut(&JournalEvent) + Send),
}

/// 握手完成后的业务帧派发：本波尚无认证 liveness 证据，断线失败关闭并清空
/// 业务通道（恢复与 liveness 属 E2E-06），加密业务帧解密后按 payload_kind 派发到会话/
/// 任务状态域；未知帧、旧 lease/generation 帧与重放帧一律丢弃，绝不转发到
/// legacy WebUI 事件流。
pub(crate) async fn serve_controlling_lane(
    lane: &mut (impl ControllingLane + ?Sized),
    manager: &ControllingDeviceManager,
    sessions: &ControllingSessionManager,
    business: &ControllingLaneState,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    while let Some(event) = lane.next_lane_event().await {
        match event {
            ControllerLaneEvent::Disconnected => {
                // 断线后不再有可用租约/会话密钥：先清空业务通道，再投影状态。
                business.clear();
                // This wave has no authenticated liveness owner yet. Do not
                // invent or extend a recovery deadline from a disconnect.
                let status = manager.mark_transport_disconnected(
                    super::relay_runtime::controlled_monotonic_millis(),
                    None,
                );
                (sinks.on_status)(&status);
            }
            ControllerLaneEvent::Frame(json) => {
                dispatch_inbound_frame(manager, sessions, business, &json, sinks);
            }
        }
    }
}

/// 解密并派发一条入站业务帧。任何一层不符（非加密帧、无活租约、解密/校验
/// 失败）都静默丢弃——攻击者不能通过伪造帧影响任何状态。
fn dispatch_inbound_frame(
    manager: &ControllingDeviceManager,
    sessions: &ControllingSessionManager,
    business: &ControllingLaneState,
    json: &str,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    let Ok(frame) = serde_json::from_str::<Value>(json) else {
        return;
    };
    if frame.get("type").and_then(Value::as_str) != Some("encrypted_frame") {
        return;
    }
    let Ok((lease, capabilities)) = manager.live_runtime_context() else {
        return;
    };
    let Ok((payload_kind, payload)) = business.open(&frame, &capabilities) else {
        return;
    };
    match payload_kind.as_str() {
        "capability_snapshot" => {
            if let Ok(projection) = sessions.apply_capability_snapshot_for_lease(&lease, &payload) {
                (sinks.on_session)(&projection);
            }
        }
        "task_status" => {
            if let Some(event) = task_event_projection(&lease, &payload) {
                (sinks.on_task)(event);
            }
        }
        "journal_event" => {
            if let Some(event) = journal_event_from_wire(&payload) {
                (sinks.on_journal)(&event);
            }
        }
        _ => {}
    }
}

/// wire `journal_event` → 权威事件。`event_b64` 是事件本体（冻结的 camelCase
/// 形状），明文侧还有 journal_epoch/event_sequence/event_kind 供缺口检测；三者
/// 必须互相一致，否则丢弃。事件体积有硬上限。明文 `event_kind` 是冻结闭集
/// （如 `terminal`），事件本体保留更细的内部类型，因此只核对 epoch/序号/会话。
fn journal_event_from_wire(payload: &Value) -> Option<JournalEvent> {
    let encoded = payload.get("event_b64").and_then(Value::as_str)?;
    let declared = payload
        .get("event_bytes")
        .and_then(Value::as_str)
        .and_then(|value| value.parse::<usize>().ok())?;
    let raw = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .ok()?;
    if raw.len() != declared || raw.len() > MAX_EVENT_PAYLOAD_BYTES {
        return None;
    }
    let event: JournalEvent = serde_json::from_slice(&raw).ok()?;
    if payload.get("journal_epoch").and_then(Value::as_str) != Some(event.epoch.as_str())
        || payload.get("event_sequence").and_then(Value::as_str)
            != Some(event.sequence.to_string().as_str())
        || payload.get("session_id").and_then(Value::as_str) != Some(event.session_id.as_str())
    {
        return None;
    }
    Some(event)
}

/// 任务状态投影（UI 任务域字段）；wire 的 `task_status` 只有状态与可选结果摘要。
fn task_event_projection(lease: &LeaseContext, payload: &Value) -> Option<Value> {
    let task_id = payload.get("task_id").and_then(Value::as_str)?;
    let status = payload.get("task_status").and_then(Value::as_str)?;
    Some(serde_json::json!({
        "taskId": task_id,
        "status": status,
        "sessionId": payload.get("session_id").and_then(Value::as_str),
        "connectionId": lease.connection_id,
        "leaseEpoch": lease.lease_epoch,
        "remoteControlGeneration": lease.remote_control_generation,
        "resultDigest": payload.get("result_digest").and_then(Value::as_str),
        "outcomeUnknown": status == "result_unknown",
    }))
}

/// A 端连接入口：加载信任根、生成握手密钥、绑定受控 Relay 通道、推进握手，
/// 并在成功后把通道保活交给后台任务。
pub(crate) async fn connect_controlling_device(
    app: &AppHandle,
    manager: &ControllingDeviceManager,
    relay: &RemoteControlManager,
    operation: ControllerOperation,
    journal: Arc<dyn ControllingJournalSink>,
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
    let business = app.state::<ControllingLaneState>();
    business.clear();

    let mut emit = |status: &ControllingStatus| {
        let _ = app.emit(CONTROLLER_STATUS_EVENT, status);
    };
    let status = run_controlling_connection(
        manager,
        operation,
        &mut port,
        &mut handshake,
        &sender,
        business.inner(),
        HANDSHAKE_DEADLINE,
        &mut emit,
    )
    .await?;

    let keeper = app.clone();
    tauri::async_runtime::spawn(async move {
        let manager = keeper.state::<ControllingDeviceManager>();
        let sessions = keeper.state::<ControllingSessionManager>();
        let business = keeper.state::<ControllingLaneState>();
        let mut on_status = |status: &ControllingStatus| {
            let _ = keeper.emit(CONTROLLER_STATUS_EVENT, status);
        };
        let mut on_session = |projection: &ControllingSessionProjection| {
            let _ = keeper.emit(CONTROLLER_SESSION_EVENT, projection);
        };
        let mut on_task = |event: Value| {
            let _ = keeper.emit(CONTROLLER_TASK_EVENT, event);
        };
        let mut on_journal = |event: &JournalEvent| {
            // A 端独立历史由命令层端口写入（feature 层不反向依赖 app 层）。
            journal.publish(event);
        };
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_task: &mut on_task,
            on_journal: &mut on_journal,
        };
        serve_controlling_lane(
            &mut port,
            manager.inner(),
            sessions.inner(),
            business.inner(),
            &mut sinks,
        )
        .await;
    });
    Ok(status)
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::collections::VecDeque;

    use super::*;
    use crate::features::remote_control::controller::controlled_handshake::ControlledHandshake;
    use crate::features::remote_control::controller::crypto::AuthenticatedRelayTrustConfig;
    use crate::features::remote_control::controller::encrypted_lane::EncryptedLane;
    use crate::features::remote_control::controller::relay_transport::transport_channels;
    use crate::features::remote_control::controller::test_support::{
        PairingFixture, encode, lease_context, paired_session_keys, pairing_fixture, signed_relay,
        signed_reservation,
    };

    const RELAY_KEY_ID: &str = "relay-key-service";

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
        let PairingFixture {
            relay,
            reservation,
            joined,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture(RELAY_KEY_ID);
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
            &ControllingLaneState::default(),
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
        let relay = signed_relay(RELAY_KEY_ID);
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
            &ControllingLaneState::default(),
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
        assert!(
            statuses
                .last()
                .is_some_and(|status| projection(status)["status"] == "error")
        );
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
            let relay = signed_relay(RELAY_KEY_ID);
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
                &ControllingLaneState::default(),
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
        let trusted = signed_relay(RELAY_KEY_ID);
        let attacker = signed_relay("relay-key-attacker");
        let a_keys = HandshakeKeyPair::generate().unwrap();
        let b_keys = HandshakeKeyPair::generate().unwrap();
        let forged = signed_reservation(
            &attacker,
            &encode(a_keys.public_key().to_bytes()),
            &encode(b_keys.public_key().to_bytes()),
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
            &ControllingLaneState::default(),
            Duration::from_secs(5),
            &mut |_: &ControllingStatus| {},
        )
        .await
        .unwrap_err();
        assert_eq!(error, "invalid_message");
        assert_eq!(
            projection(&manager.status())["errorCode"],
            "invalid_message"
        );
        assert!(sender.authenticated_lease().is_none());
        assert_eq!(
            sender.generation(),
            "",
            "untrusted transcript must not bind"
        );

        // 2) transport 已经绑定过 generation 时，Reserved 阶段必须中止。
        let PairingFixture {
            relay,
            reservation,
            joined,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture(RELAY_KEY_ID);
        let mut port = peer_port(reservation.clone(), joined, b_keys, &relay.trust);
        let mut handshake = a_handshake(&reservation, a_keys, relay.trust.clone());
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        let (sender, _receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0000",
            2,
            2,
            8192,
            1024,
        );
        let error = run_controlling_connection(
            &manager,
            operation,
            &mut port,
            &mut handshake,
            &sender,
            &ControllingLaneState::default(),
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
    async fn post_handshake_disconnect_without_evidence_fails_closed_and_keeps_draining() {
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
                    negotiated_capabilities: vec![
                        "encrypted_frames_v1".into(),
                        "pairing_v1".into(),
                        "task_v1".into(),
                    ],
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
        let sessions = ControllingSessionManager::default();
        let business = ControllingLaneState::default();
        let mut sessions_seen = Vec::new();
        let mut tasks_seen = Vec::new();
        let mut on_status = |status: &ControllingStatus| {
            statuses.push(projection(status)["status"].as_str().unwrap().to_owned());
        };
        let mut on_session = |projection: &ControllingSessionProjection| {
            sessions_seen.push(projection.session_id.clone());
        };
        let mut on_task = |event: Value| tasks_seen.push(event);
        let mut journal_seen: Vec<JournalEvent> = Vec::new();
        let mut on_journal = |event: &JournalEvent| journal_seen.push(event.clone());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_task: &mut on_task,
            on_journal: &mut on_journal,
        };
        serve_controlling_lane(&mut lane, &manager, &sessions, &business, &mut sinks).await;
        assert_eq!(statuses, vec!["error".to_owned()]);
        assert!(sessions_seen.is_empty());
        assert!(tasks_seen.is_empty());
        assert!(journal_seen.is_empty());
        assert_eq!(
            projection(&manager.status())["status"],
            "error",
            "没有认证存活证据时不得制造恢复预算"
        );
        assert_eq!(
            projection(&manager.status())["errorCode"],
            "endpoint_offline"
        );
        assert!(manager.live_runtime_context().is_err());
    }

    /// 用户已显式断开（或尝试已被取代）时，在途失败不得把状态覆盖成 error。
    #[tokio::test]
    async fn in_flight_failure_does_not_override_an_explicit_disconnect() {
        let relay = signed_relay(RELAY_KEY_ID);
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
            disconnected: true,
        };
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        let status = manager.disconnect();
        assert_eq!(projection(&status)["status"], "disconnected");
        let (sender, _receiver) =
            transport_channels(ControllerRole::ControllerA, "", 2, 2, 8192, 1024);
        let error = run_controlling_connection(
            &manager,
            operation,
            &mut port,
            &mut handshake,
            &sender,
            &ControllingLaneState::default(),
            Duration::from_secs(5),
            &mut |_: &ControllingStatus| {},
        )
        .await
        .unwrap_err();
        assert_eq!(error, "endpoint_offline");
        assert_eq!(
            projection(&manager.status())["status"],
            "disconnected",
            "在途失败的收尾不得覆盖用户的显式断开"
        );
        assert!(sender.authenticated_lease().is_none());
        assert!(sender.is_stopped());
    }

    /// E2E-04：握手完成后的加密业务帧由保活任务解密并派发到会话/任务状态域；
    /// 旧 generation、重放或伪造帧不产生任何副作用。
    #[tokio::test]
    async fn encrypted_inbound_frames_dispatch_to_session_and_task_state() {
        let PairingFixture {
            reservation,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture(RELAY_KEY_ID);
        let (a_session, b_session) = paired_session_keys(a_keys, b_keys, &reservation);
        let lease = lease_context(&reservation);
        let capabilities = vec![
            "encrypted_frames_v1".to_owned(),
            "pairing_v1".to_owned(),
            "snapshot_journal_v1".to_owned(),
            "task_v1".to_owned(),
        ];

        // A 端状态：活租约 + 会话意图 + 加密通道（与连接完成后的状态一致）。
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation,
                ControllerLease {
                    context: lease.clone(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7_u8; 32],
                    transport_key_fingerprint: [9_u8; 32],
                    negotiated_capabilities: capabilities.clone(),
                },
            )
            .unwrap();
        let business = ControllingLaneState::default();
        business
            .install(HandshakeRole::A, &lease, a_session)
            .unwrap();
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open(None, "Session", "chat", &lease, &capabilities)
            .unwrap();

        // B 端用同一份会话密钥封帧（角色 B）。
        let mut responder = EncryptedLane::new(HandshakeRole::B, &lease, b_session).unwrap();
        let snapshot = serde_json::json!({
            "v": 1,
            "type": "capability_snapshot",
            "connection_id": lease.connection_id,
            "controller_lease_id": lease.controller_lease_id,
            "lease_epoch": lease.lease_epoch,
            "relay_epoch": lease.relay_epoch,
            "remote_control_generation": lease.remote_control_generation,
            "request_id": "request-snapshot-1",
            "capability_revision": "4",
            "expires_at_ms": "30000",
            "modes": ["chat"],
            "tools": [],
            "skills": [],
            "workspaces": [],
        });
        let snapshot_frame = responder
            .seal("capability_snapshot", &snapshot, &capabilities)
            .unwrap();
        let status = serde_json::json!({
            "v": 1,
            "type": "task_status",
            "connection_id": lease.connection_id,
            "controller_lease_id": lease.controller_lease_id,
            "lease_epoch": lease.lease_epoch,
            "relay_epoch": lease.relay_epoch,
            "remote_control_generation": lease.remote_control_generation,
            "request_id": "request-status-1",
            "task_id": "task-0001",
            "session_id": session.session_id,
            "task_status": "succeeded",
            "result_digest": "0".repeat(64),
        });
        let status_frame = responder
            .seal("task_status", &status, &capabilities)
            .unwrap();
        // 绑定另一份租约（异 generation）的加密帧：必须在派发前被丢弃。
        let mut foreign_lease = lease.clone();
        foreign_lease.remote_control_generation = "generation-other".into();
        let mut foreign_responder = EncryptedLane::new(
            HandshakeRole::B,
            &foreign_lease,
            responder_session(&foreign_lease),
        )
        .unwrap();
        let mut foreign_status = status.clone();
        foreign_status["remote_control_generation"] = serde_json::json!("generation-other");
        let foreign = foreign_responder
            .seal("task_status", &foreign_status, &capabilities)
            .unwrap();

        let mut lane = TestLane {
            events: VecDeque::from(vec![
                ControllerLaneEvent::Frame(snapshot_frame.to_string()),
                ControllerLaneEvent::Frame(foreign.to_string()),
                ControllerLaneEvent::Frame(status_frame.to_string()),
            ]),
        };
        let mut statuses = Vec::new();
        let mut session_projections = Vec::new();
        let mut task_events = Vec::new();
        let mut on_status = |status: &ControllingStatus| {
            statuses.push(projection(status)["status"].as_str().unwrap().to_owned())
        };
        let mut on_session = |projection: &ControllingSessionProjection| {
            session_projections.push(projection.capability_revision);
        };
        let mut on_task = |event: Value| task_events.push(event);
        let mut journal_events: Vec<JournalEvent> = Vec::new();
        let mut on_journal = |event: &JournalEvent| journal_events.push(event.clone());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_task: &mut on_task,
            on_journal: &mut on_journal,
        };
        serve_controlling_lane(&mut lane, &manager, &sessions, &business, &mut sinks).await;

        assert!(statuses.is_empty(), "派发业务帧不得改变连接状态");
        assert_eq!(
            session_projections,
            vec![Some(4)],
            "能力快照必须落进会话状态，且旧 generation 的帧不得派发"
        );
        assert_eq!(task_events.len(), 1, "任务状态必须投影为一条任务事件");
        assert_eq!(task_events[0]["taskId"], "task-0001");
        assert_eq!(task_events[0]["status"], "succeeded");
        assert_eq!(task_events[0]["sessionId"], session.session_id);
        assert_eq!(task_events[0]["resultDigest"], "0".repeat(64));
        assert_eq!(task_events[0]["outcomeUnknown"], false);
        assert!(journal_events.is_empty());
    }

    /// 旧 generation 的响应方通道（同一会话密钥、另一份租约）。
    fn responder_session(
        _lease: &crate::features::remote_control::controller::protocol::LeaseContext,
    ) -> crate::features::remote_control::controller::crypto::SessionKeys {
        // 该帧只需在密码学上合法：用一组新密钥对派生即可（A 端会因租约/生成代
        // 不符而丢弃，不会解密成功）。
        let (_a, b) = paired_session_keys(
            HandshakeKeyPair::generate().unwrap(),
            HandshakeKeyPair::generate().unwrap(),
            &pairing_fixture(RELAY_KEY_ID).reservation,
        );
        b
    }

    /// E2E-04 出口：同一进程内完成「A 发加密任务 → B 解密派发 → B 加密回传 →
    /// A 解密投影」的普通文本任务闭环（B 的接纳/执行由 DEV-13 的服务用例覆盖，
    /// 这里用端口桩返回冻结形状的 task_status）。
    #[tokio::test]
    async fn dual_end_task_round_trip_lands_in_a_side_state() {
        use crate::features::remote_control::controller::relay_runtime::{
            ControlledBusinessPort, dispatch_decrypted_payload,
        };

        struct TerminalPort;

        #[async_trait::async_trait]
        impl ControlledBusinessPort for TerminalPort {
            async fn capability_snapshot(
                &self,
                _lease: &LeaseContext,
                request_id: &str,
                known_revision: u64,
            ) -> Result<Value, String> {
                Ok(serde_json::json!({
                    "v": 1,
                    "type": "capability_snapshot",
                    "request_id": request_id,
                    "capability_revision": (known_revision + 1).to_string(),
                }))
            }

            async fn submit_task(
                &self,
                lease: &LeaseContext,
                session_ref: &str,
                payload: &Value,
            ) -> Result<Value, String> {
                // 与 B 的 RemoteTaskService::status_payload 一致：冻结 wire 形状。
                Ok(serde_json::json!({
                    "v": 1,
                    "type": "task_status",
                    "connection_id": lease.connection_id,
                    "controller_lease_id": lease.controller_lease_id,
                    "lease_epoch": lease.lease_epoch,
                    "relay_epoch": lease.relay_epoch,
                    "remote_control_generation": lease.remote_control_generation,
                    "request_id": "status-1",
                    "task_id": payload["task_id"],
                    "session_id": session_ref,
                    "task_status": "succeeded",
                    "result_digest": "0".repeat(64),
                }))
            }

            async fn cancel_task(
                &self,
                _lease: &LeaseContext,
                _session_ref: &str,
                _task_id: &str,
            ) -> Result<Option<Value>, String> {
                Ok(None)
            }
        }

        let PairingFixture {
            reservation,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture(RELAY_KEY_ID);
        let (a_session, b_session) = paired_session_keys(a_keys, b_keys, &reservation);
        let lease = lease_context(&reservation);
        let capabilities = vec![
            "encrypted_frames_v1".to_owned(),
            "pairing_v1".to_owned(),
            "snapshot_journal_v1".to_owned(),
            "task_v1".to_owned(),
        ];

        // A 端：活租约 + 加密通道 + 已拿到能力快照的会话意图。
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation,
                ControllerLease {
                    context: lease.clone(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7_u8; 32],
                    transport_key_fingerprint: [9_u8; 32],
                    negotiated_capabilities: capabilities.clone(),
                },
            )
            .unwrap();
        let business = ControllingLaneState::default();
        business
            .install(HandshakeRole::A, &lease, a_session)
            .unwrap();
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open(None, "Session", "chat", &lease, &capabilities)
            .unwrap();
        sessions
            .apply_capability_snapshot(
                &session.session_ref,
                &serde_json::json!({
                    "v": 1,
                    "type": "capability_snapshot",
                    "connection_id": lease.connection_id,
                    "controller_lease_id": lease.controller_lease_id,
                    "lease_epoch": lease.lease_epoch,
                    "relay_epoch": lease.relay_epoch,
                    "remote_control_generation": lease.remote_control_generation,
                    "request_id": "request-snapshot-1",
                    "capability_revision": "4",
                    "expires_at_ms": "30000",
                    "modes": ["chat"],
                    "tools": [],
                    "skills": [],
                    "workspaces": [],
                }),
                &lease,
            )
            .unwrap();

        // A：构造并加密任务提交。
        let intent = sessions
            .task_intent(&session.session_ref, "hello from A", &lease)
            .unwrap();
        let frame = business
            .seal("task_submit", &intent.payload, &capabilities)
            .unwrap();

        // B：解密 → 派发 → 加密回传。
        let mut b_lane = EncryptedLane::new(HandshakeRole::B, &lease, b_session).unwrap();
        let (kind, payload) = b_lane.open(&frame, &capabilities).unwrap();
        assert_eq!(kind, "task_submit");
        let (response_kind, response) =
            dispatch_decrypted_payload(&TerminalPort, &lease, &kind, &payload)
                .await
                .unwrap()
                .unwrap();
        assert_eq!(response_kind, "task_status");
        let response_frame = b_lane
            .seal(response_kind, &response, &capabilities)
            .unwrap();

        // B 的权威 journal 事件（E2E-05）：正文与终态都以 journal_event 回传。
        let journal_event = JournalEvent {
            event_id: "event-0001".to_owned(),
            epoch: "epoch-1".to_owned(),
            sequence: 1,
            kind: "assistant_delta".to_owned(),
            session_id: session.session_id.clone(),
            task_id: intent.task_id.clone(),
            terminal_status: None,
            payload: serde_json::json!({"text": "hello from B"}),
        };
        let raw = serde_json::to_vec(&journal_event).unwrap();
        let journal_frame = b_lane
            .seal(
                "journal_event",
                &serde_json::json!({
                    "v": 1,
                    "type": "journal_event",
                    "connection_id": lease.connection_id,
                    "controller_lease_id": lease.controller_lease_id,
                    "lease_epoch": lease.lease_epoch,
                    "relay_epoch": lease.relay_epoch,
                    "remote_control_generation": lease.remote_control_generation,
                    "session_id": journal_event.session_id,
                    "journal_epoch": journal_event.epoch,
                    "event_sequence": journal_event.sequence.to_string(),
                    "event_kind": journal_event.kind,
                    "event_b64": base64::engine::general_purpose::STANDARD.encode(&raw),
                    "event_bytes": raw.len().to_string(),
                }),
                &capabilities,
            )
            .unwrap();

        // A：保活任务解密并投影到任务状态域。
        let mut lane = TestLane {
            events: VecDeque::from(vec![
                ControllerLaneEvent::Frame(response_frame.to_string()),
                ControllerLaneEvent::Frame(journal_frame.to_string()),
            ]),
        };
        let mut task_events = Vec::new();
        let mut session_projections = Vec::new();
        let mut on_status = |_: &ControllingStatus| {};
        let mut on_session = |projection: &ControllingSessionProjection| {
            session_projections.push(projection.session_id.clone());
        };
        let mut on_task = |event: Value| task_events.push(event);
        let mut journal_events: Vec<JournalEvent> = Vec::new();
        let mut on_journal = |event: &JournalEvent| journal_events.push(event.clone());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_task: &mut on_task,
            on_journal: &mut on_journal,
        };
        serve_controlling_lane(&mut lane, &manager, &sessions, &business, &mut sinks).await;

        assert!(session_projections.is_empty());
        assert_eq!(task_events.len(), 1);
        assert_eq!(task_events[0]["taskId"], intent.task_id.clone().unwrap());
        assert_eq!(task_events[0]["status"], "succeeded");
        assert_eq!(task_events[0]["sessionId"], session.session_id);
        assert_eq!(task_events[0]["resultDigest"], "0".repeat(64));
        assert_eq!(
            journal_events.len(),
            1,
            "正文事件必须解密并投影给 A 端历史/渲染"
        );
        assert_eq!(journal_events[0].kind, "assistant_delta");
        assert_eq!(journal_events[0].payload["text"], "hello from B");
    }
}
