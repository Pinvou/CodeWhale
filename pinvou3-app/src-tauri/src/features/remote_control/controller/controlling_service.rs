//! A 端（主控）连接接线（E2E-02 第 2 步）。
//!
//! 把冻结握手序列接到真实 Relay transport 与 `ControllingDeviceManager`：
//! 生成握手密钥、加载受信 Relay 信任根、在验签 transcript 上一次性绑定
//! transport generation、驱动 claim→joined、安装认证租约并把阶段投影给状态
//! 所有者。失败/超时/断线统一映射为有限公共错误，且不产生半租约。
//!
//! 首版只接线普通会话所需的握手与租约；业务帧派发留给 E2E-04，liveness 与
//! resume 留给 E2E-06。

#[path = "controlling_dispatch.rs"]
mod dispatch;
use dispatch::*;
pub(crate) use dispatch::{AuthenticatedJournalFrame, AuthenticatedSnapshotFrame};

use std::sync::Arc;
use std::time::Duration;

use base64::Engine as _;
use serde_json::Value;
use tauri::{AppHandle, Emitter, Manager as _};
#[cfg(test)]
use tokio::sync::mpsc;
use uuid::Uuid;

use super::controlling::{
    ControllerLease, ControllerOperation, ControllingDeviceManager, ControllingStatus,
    ControllingStatusKind, ResumeStep,
};
use super::controlling_handshake::{ControllingHandshake, HandshakeError};
use super::controlling_inbound::{InboundCharge, InboundLimits, InboundReceiver, inbound_channel};
use super::controlling_runtime::{
    ControllingHandshakeOutcome, ControllingRelayPort, ControllingStageEvent,
    drive_controlling_handshake,
};
use super::controlling_session::{ControllingSessionManager, ControllingSessionProjection};
use super::crypto::{
    AuthenticatedRelayTrustConfig, ControllerRole as HandshakeRole, HandshakeKeyPair,
    load_relay_trust_config, transport_key_fingerprint,
};
use super::encrypted_lane::ControllingLaneState;
#[cfg(test)]
use super::journal::{JournalEvent, JournalSnapshot};
use super::protocol::{LeaseContext, SenderRole};
use super::recovery_budget::LivenessBudget;
use super::relay_runtime::log_controller_event;
use super::relay_transport::{
    AuthenticatedLease, ControllerRole, ControllerTransportConfig, ControllerTransportEvent,
    ControllerTransportSender, spawn_controller_transport,
};
use super::resume::{
    build_controller_resume_prepare, build_controller_resume_ready_ack, new_resume_challenge,
    relay_resume_deadline_ms, resume_frame_matches_lease,
};
use crate::features::behavior_telemetry::{
    RemoteControlEvent, RemoteOutcome, RemoteReason, RemoteStage, track_remote_control,
};
use crate::features::remote_control::RemoteControlManager;

pub(crate) const CONTROLLER_STATUS_EVENT: &str = "desktop-remote-controller-status";
/// A 端远程会话投影事件（sessionId / capability revision）。
/// A 端任务状态事件（加密回传解密后的投影）。
pub(crate) const CONTROLLER_TASK_EVENT: &str = "desktop-remote-task-status";

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

pub(crate) use super::lane_event::ControllerLaneEvent;
use super::lane_event::ControllerLaneFailure;

/// 握手完成后的通道抽象。只暴露「下一条通道事件」，使断线投影可以在不依赖
/// Tauri 的情况下被测试。
#[async_trait::async_trait]
pub(crate) trait ControllingLane: Send {
    async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent>;

    /// The port's original registration, never a fresh manager lookup. A real
    /// authenticated keeper must carry this exact transport and lease tuple.
    fn transport_sender(&self) -> Option<ControllerTransportSender>;

    /// Test-only synchronization after authenticated commit, before activation.
    /// No manager lock is held; production builds have no hook or new API.
    #[cfg(test)]
    fn before_resume_activation(&self) {}

    #[cfg(test)]
    fn after_resume_activation(&self) {}

    /// Release accepted business backlog only after authenticated resume and
    /// the existing business lane have both completed their authority checks.
    fn resume_business(&mut self) -> Result<(), ControllerLaneFailure> {
        Ok(())
    }

    /// E2E-06：发送恢复族的敏感控制帧（prepare/confirm/ready_ack）。默认实现
    /// 失败关闭，只有真实 transport 端口实现它；测试替身可覆写以记录帧。
    fn send_resume_control(&self, _message: Value) -> Result<(), String> {
        Err("controller_resume_unsupported".to_owned())
    }
}

pub(crate) trait ControllingJournalSink: Send + Sync {
    fn publish(&self, frame: &AuthenticatedJournalFrame<'_>) -> Result<(), String>;

    fn publish_task_rejection(
        &self,
        _operation: &ControllerOperation,
        _lease: &LeaseContext,
        _payload: &Value,
    ) -> Result<(), String> {
        Err("remote_rejection_not_configured".into())
    }

    fn publish_capability(
        &self,
        _operation: &ControllerOperation,
        _projection: &ControllingSessionProjection,
    ) -> Result<(), String> {
        Err("remote_capability_projection_not_configured".into())
    }

    /// Called only for a validated encrypted capability response belonging to
    /// the pending request. The app adapter confirms the exact B Session in
    /// SessionStore under the current authenticated lease and lane guard.
    fn confirm_mapping(
        &self,
        _operation: &ControllerOperation,
        _lease: &LeaseContext,
        _remote_ref: &str,
        _controlled_session_id: &str,
    ) -> Result<(), String> {
        Err("remote_mapping_not_configured".into())
    }

    /// E2E-05 缺口恢复：权威快照原子替换 A 端本地副本（默认忽略，命令层实现
    /// 为写入对应历史并广播）。
    fn publish_snapshot(&self, _frame: &AuthenticatedSnapshotFrame<'_>) -> Result<(), String> {
        Err("remote_projection_not_configured".to_owned())
    }

    /// AEAD-opened action result. The application adapter correlates its
    /// request and exact live RemoteProxy before releasing any bytes to UI.
    fn publish_action_result(
        &self,
        _operation: &ControllerOperation,
        _lease: &LeaseContext,
        _payload: &Value,
    ) -> Result<(), String> {
        Err("remote_action_not_configured".to_owned())
    }
}

/// A 端 Relay 端口。入站帧由 `spawn_controller_transport` 的事件 sink 投递；
/// 出站一律走敏感队列（claim 自带连接码、receipt ack 自带 receipt secret）。
pub(crate) struct ControllerTransportPort {
    sender: ControllerTransportSender,
    inbound: InboundReceiver,
    // The two production consumers finish processing a String before asking
    // for another event. Keep its allocation/item budget through that work,
    // including the handshake-to-keeper handoff; release on next or Drop.
    current_charge: Option<InboundCharge>,
    relay_error_code: Option<&'static str>,
    relay_error_seen: bool,
    lane_disconnected: bool,
}

impl ControllerTransportPort {
    fn new(sender: ControllerTransportSender, inbound: InboundReceiver) -> Self {
        Self {
            sender,
            inbound,
            current_charge: None,
            relay_error_code: None,
            relay_error_seen: false,
            lane_disconnected: false,
        }
    }
}

#[async_trait::async_trait]
impl ControllingLane for ControllerTransportPort {
    async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
        self.current_charge = None;
        let delivery = self.inbound.recv().await?;
        self.current_charge = delivery.charge;
        Some(delivery.event)
    }

    fn resume_business(&mut self) -> Result<(), ControllerLaneFailure> {
        self.inbound.resume_business()
    }

    fn transport_sender(&self) -> Option<ControllerTransportSender> {
        Some(self.sender.clone())
    }

    fn send_resume_control(&self, message: Value) -> Result<(), String> {
        self.sender
            .try_send_sensitive_resume_control(&message)
            .map_err(|_| "controller_resume_send_failed".to_owned())
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
                ControllerLaneEvent::Failed(_) => return None,
                ControllerLaneEvent::Disconnected(context) => {
                    if context.owner != self.sender.owner() {
                        continue;
                    }
                    self.lane_disconnected = true;
                    return None;
                }
                ControllerLaneEvent::Frame(json) => {
                    let Ok(value) = serde_json::from_str::<Value>(&json) else {
                        continue;
                    };
                    if value.get("type").and_then(Value::as_str) == Some("error") {
                        self.relay_error_code = relay_error_code(&value);
                        self.relay_error_seen = true;
                        // 原始码（协议枚举名，不含文案或凭据）直接落盘：闭集外的码会
                        // 被公共错误映射兜底成 invalid_message，只在端上排障日志里
                        // 保留原文才能定位 Relay 到底拒绝在哪一步。
                        log_controller_event(
                            "relay_error",
                            value
                                .get("code")
                                .and_then(Value::as_str)
                                .unwrap_or("unknown"),
                        );
                    }
                    return Some(value);
                }
            }
        }
    }

    fn relay_error_code(&self) -> Option<&'static str> {
        self.relay_error_code
    }

    fn relay_error_seen(&self) -> bool {
        self.relay_error_seen
    }

    fn lane_disconnected(&self) -> bool {
        self.lane_disconnected || self.inbound.disconnect_pending()
    }

    fn ingress_failure(&self) -> Option<ControllerLaneFailure> {
        self.inbound.failure()
    }
}

/// 绑定一条真实 Relay transport，并把入站事件投影为端口事件。generation 以
/// 空串启动：A 端只有验签 transcript 之后才会通过 `bind_generation` 绑定它。
fn spawn_controlling_lane(relay_url: &str) -> (ControllerTransportSender, ControllerTransportPort) {
    spawn_controlling_lane_with_limits(relay_url, InboundLimits::default())
}

fn spawn_controlling_lane_with_limits(
    relay_url: &str,
    limits: InboundLimits,
) -> (ControllerTransportSender, ControllerTransportPort) {
    spawn_controlling_lane_with_config_and_limits(
        ControllerTransportConfig::new(relay_url, ControllerRole::ControllerA, String::new()),
        limits,
    )
}

fn spawn_controlling_lane_with_config(
    config: ControllerTransportConfig,
) -> (ControllerTransportSender, ControllerTransportPort) {
    spawn_controlling_lane_with_config_and_limits(config, InboundLimits::default())
}

fn spawn_controlling_lane_with_config_and_limits(
    config: ControllerTransportConfig,
    limits: InboundLimits,
) -> (ControllerTransportSender, ControllerTransportPort) {
    let (inbound_tx, inbound_rx) = inbound_channel(limits);
    let ingress = inbound_tx.clone();
    let sink = Arc::new(move |event: ControllerTransportEvent| ingress.enqueue(event));
    let sender = spawn_controller_transport(config, sink);
    inbound_tx.bind_transport(sender.clone());
    let port = ControllerTransportPort::new(sender.clone(), inbound_rx);
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

/// 失败分类：已观察到的本地入站终态优先于已保存的 Relay 错误码；
/// 否则使用 Relay 明确码，断线与本地上限超时归为对端不可达；
/// 其余（验签失败、顺序错误、本地 generation 绑定失败）归为协议错误。
fn public_failure_code(
    port: &impl ControllingRelayPort,
    locally_unavailable: bool,
) -> &'static str {
    if port.ingress_failure().is_some() {
        return PEER_UNAVAILABLE_ERROR;
    }
    if let Some(code) = port.relay_error_code() {
        return code;
    }
    if locally_unavailable || port.lane_disconnected() {
        return PEER_UNAVAILABLE_ERROR;
    }
    GENERIC_CONNECT_ERROR
}

/// 失败归因（有界、不含凭据）：公共码 + 来源（Relay 明确报错 / 通道断开 / 本地校验）。
/// 只用于端上排障日志，不改变任何面向用户的错误语义。
fn failure_origin_detail(port: &impl ControllingRelayPort, code: &str) -> String {
    if let Some(reason) = port.ingress_failure() {
        // The actual log sink accepts a closed detail set. Keep this local
        // reason intact rather than constructing an unregistered string.
        return reason.code().to_owned();
    }
    let origin = if port.relay_error_seen() {
        "relay"
    } else if port.lane_disconnected() {
        "lane"
    } else {
        "local"
    };
    format!("{code}/origin:{origin}")
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
        Ok(Err(handshake_error)) => {
            let code = public_failure_code(port, false);
            // 本地握手层的原始有界码 + 失败归因：公共码会把闭集外的 Relay 错误码
            // 兜底成 invalid_message，只留公共码时无法区分「Relay 报了什么」与
            // 「本地校验拒绝」。两者都是协议枚举名，不含任何凭据。
            log_controller_event("handshake_failed", handshake_error.code);
            log_controller_event("connect_failed", &failure_origin_detail(port, code));
            sender.stop();
            let _ = manager.with_lane_owner(&operation, || business.clear());
            return Err(fail_connection(manager, operation, code, on_status));
        }
        Err(_elapsed) => {
            let code = public_failure_code(port, true);
            log_controller_event("connect_failed", &failure_origin_detail(port, code));
            sender.stop();
            let _ = manager.with_lane_owner(&operation, || business.clear());
            return Err(fail_connection(manager, operation, code, on_status));
        }
    };

    // A joined frame may already be in flight when ingress terminates or the
    // socket disconnect barrier arrives. Never forget that known handoff loss.
    if port.ingress_failure().is_some() || port.lane_disconnected() || sender.is_stopped() {
        let code = public_failure_code(port, sender.is_stopped());
        log_controller_event("connect_failed", &failure_origin_detail(port, code));
        sender.stop();
        fail_owned_lane(manager, &operation, business, code, on_status);
        return Err(code.to_owned());
    }
    match complete_connected(manager, operation.clone(), sender, business, outcome) {
        Ok(status) => {
            if port.ingress_failure().is_some() || port.lane_disconnected() || sender.is_stopped() {
                let code = public_failure_code(port, sender.is_stopped());
                log_controller_event("connect_failed", &failure_origin_detail(port, code));
                sender.stop();
                fail_owned_lane(manager, &operation, business, code, on_status);
                return Err(code.to_owned());
            }
            on_status(&status);
            Ok(status)
        }
        Err(code) => {
            // The first handoff check can pass before an ingress terminal
            // makes the real lease installer return Closed. Recheck before
            // cleanup so that local saturation retains its public reason.
            if port.ingress_failure().is_some() || port.lane_disconnected() {
                let code = public_failure_code(port, false);
                log_controller_event("connect_failed", &failure_origin_detail(port, code));
                sender.stop();
                fail_owned_lane(manager, &operation, business, code, on_status);
                return Err(code.to_owned());
            }
            sender.stop();
            let _ = manager.with_lane_owner(&operation, || business.clear());
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
        peer_endpoint_id: outcome.peer_endpoint_id,
        reservation_id: outcome.reservation_id,
        device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
        liveness_epoch_a: outcome.lease.liveness_epoch_a.clone(),
        liveness_epoch_b: outcome.lease.liveness_epoch_b.clone(),
        resume_deadline_b: outcome.lease.resume_deadline_ms,
        resume_token: outcome.lease.resume_token,
        transport_key_fingerprint: fingerprint,
        negotiated_capabilities: outcome.negotiated_capabilities.clone(),
    };
    // 加密业务通道与租约同时就位：状态一旦进入 connected，业务帧既有租约准入
    // 也有会话密钥可用。
    manager
        .complete_connected_with_install(operation, lease, || {
            sender
                .install_authenticated_lease(AuthenticatedLease {
                    context: context.clone(),
                    negotiated_capabilities: outcome.negotiated_capabilities,
                })
                .map_err(|_| {
                    super::controlling::ControllingStateError::new(GENERIC_CONNECT_ERROR)
                })?;
            business
                .install(HandshakeRole::A, &context, outcome.session)
                .map_err(|_| super::controlling::ControllingStateError::new(GENERIC_CONNECT_ERROR))
        })
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

/// Both Connecting and Connected can observe an ingress terminal. Every
/// mutation is fenced to the original intent/claim, including the clear.
fn fail_owned_lane(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    business: &ControllingLaneState,
    code: &'static str,
    on_status: &mut (impl FnMut(&ControllingStatus) + ?Sized),
) {
    if manager
        .with_lane_owner(operation, || business.clear())
        .is_err()
    {
        return;
    }
    if let Ok(status) = manager.fail_connection(operation.clone(), code) {
        on_status(&status);
    }
}

/// 握手完成后的通道保活与业务帧派发：首次断线冻结认证存活截止时间并挂起
/// 业务通道，恢复期间绝不延长预算；加密业务帧解密后按 payload_kind 派发到会话/
/// 任务状态域；未知帧、旧 lease/generation 帧与重放帧一律丢弃，绝不转发到
/// legacy WebUI 事件流。
pub(crate) async fn serve_controlling_lane(
    lane: &mut (impl ControllingLane + ?Sized),
    manager: &ControllingDeviceManager,
    sessions: &ControllingSessionManager,
    business: &ControllingLaneState,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    serve_controlling_lane_with_interval(
        lane,
        manager,
        sessions,
        business,
        sinks,
        Duration::from_millis(LIVENESS_HEARTBEAT_INTERVAL_MS),
    )
    .await
}

/// E2E-06：心跳间隔（远小于 30 秒存活预算，丢一两次仍能续期）。
/// 同时满足 Relay 侧 attestation 的新鲜度要求（observed_age ≤ 5 秒）。
pub(crate) const LIVENESS_HEARTBEAT_INTERVAL_MS: u64 = 3_000;

pub(crate) async fn serve_controlling_lane_with_interval(
    lane: &mut (impl ControllingLane + ?Sized),
    manager: &ControllingDeviceManager,
    sessions: &ControllingSessionManager,
    business: &ControllingLaneState,
    sinks: &mut ControllingLaneSinks<'_>,
    heartbeat_interval: Duration,
) {
    // Pin once for the whole authenticated lane; a later environment/profile
    // edit cannot replace its verifier while messages are being processed.
    let Some(operation) = manager.current_operation() else {
        return;
    };
    let trust = load_relay_trust_config(|name| std::env::var(name).ok());
    serve_controlling_lane_with_evidence_context(
        lane,
        manager,
        sessions,
        business,
        sinks,
        heartbeat_interval,
        PeerEvidenceContext {
            trust: trust.as_ref().ok(),
            monotonic: &lane_monotonic_millis,
            operation,
        },
    )
    .await;
}

struct PeerEvidenceContext<'a> {
    trust: Option<&'a AuthenticatedRelayTrustConfig>,
    monotonic: &'a (dyn Fn() -> u64 + Send + Sync),
    operation: ControllerOperation,
}

async fn serve_controlling_lane_with_evidence_context(
    lane: &mut (impl ControllingLane + ?Sized),
    manager: &ControllingDeviceManager,
    sessions: &ControllingSessionManager,
    business: &ControllingLaneState,
    sinks: &mut ControllingLaneSinks<'_>,
    heartbeat_interval: Duration,
    evidence: PeerEvidenceContext<'_>,
) {
    let owned_sender = lane.transport_sender();
    let registration = owned_sender
        .as_ref()
        .map(|sender| sender.disconnect_context());
    let Some(registration) = registration else {
        return;
    };
    // Every keeper, including isolated tests, uses its original registration.
    let operation = evidence.operation.clone().for_lane(registration.clone());
    if manager.with_lane_owner(&operation, || ()).is_err() {
        return;
    }
    let mut owned_liveness = |frame: Value| {
        if manager.with_lane_owner(&operation, || ()).is_err() {
            return false;
        }
        // Production binds this sink to the port's original sender below.
        // Keep admission/backpressure injectable through the same interface;
        // tests must still pass the identical registration/owner checks.
        (sinks.on_liveness)(frame)
    };
    let mut owned_sinks = ControllingLaneSinks {
        on_status: &mut *sinks.on_status,
        on_session: &mut *sinks.on_session,
        on_mapping: &mut *sinks.on_mapping,
        on_task: &mut *sinks.on_task,
        on_journal: &mut *sinks.on_journal,
        on_snapshot: &mut *sinks.on_snapshot,
        on_liveness: &mut owned_liveness,
    };
    let sinks = &mut owned_sinks;
    let mut heartbeat = tokio::time::interval(heartbeat_interval);
    heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    heartbeat.tick().await;
    let mut liveness = LivenessExpectations::default();
    let mut journal = JournalExpectations::default();
    let mut resume = ResumeAttempt::default();
    let mut budget = LivenessBudget::authenticated_cycle((evidence.monotonic)());
    let mut resume_deadline = budget.timer();
    loop {
        tokio::select! {
            biased;
            _ = heartbeat.tick() => {
                if manager.with_lane_owner(&operation, || ()).is_err() { return; }
                // 恢复期间不发心跳：业务与存活闸门都已挂起。
                if !resume.is_active() {
                    if let Ok((runtime, epoch_b, epoch_a)) = manager.with_peer_liveness_context(&operation, |runtime, b, a| (runtime.clone(), b.to_owned(), a.to_owned())) {
                        if let Some(lease) = runtime.lease.as_ref() {
                            if let Some(next) = liveness.next_sequence() {
                                if let Ok(frame) = super::liveness::build_controller_liveness(lease, SenderRole::ControllerA, &epoch_a, next) { (sinks.on_liveness)(frame); }
                            }
                            if let Some(request) = budget.request_peer_evidence(lease, &epoch_b, (evidence.monotonic)()) {
                                if !(sinks.on_liveness)(request) {
                                    budget.cancel_peer_request();
                                }
                            }
                        }
                    }
                    if let Ok(runtime) = manager.with_peer_liveness_context(&operation, |runtime, _, _| runtime.clone()) {
                        let Some(lease) = runtime.lease else { return; };
                        let capabilities = runtime.negotiated_capabilities;
                        for (session_ref, pending) in journal.due_replays(
                            &lease.remote_control_generation, tokio::time::Instant::now()
                        ) {
                            let _ = request_journal_replay(
                                manager, &operation, business, &lease, &capabilities, &session_ref,
                                &pending.epoch, pending.after, &pending.request_id,
                            );
                        }
                    }
                }
            }
            // 本地恢复预算耗尽：清空通道并按有界错误结束本次连接（Relay 侧
            // 自己的 deadline 是权威，本地只保证不会无限等待）。
            _ = tokio::time::sleep_until(resume_deadline) => {
                if manager.with_lane_owner(&operation, || business.clear()).is_err() { return; }
                // Connected B-evidence expiry is not a failed resume. Keep
                // late resume failures fenced to Reconnecting, but retire
                // this still-owned connected lane when its finite budget ends.
                if resume.is_active() {
                    fail_resume(manager, &operation, sinks);
                } else if let Ok(status) = manager.fail_connection(operation.clone(), PEER_UNAVAILABLE_ERROR) {
                    (sinks.on_status)(&status);
                }
                return;
            }
            event = lane.next_lane_event() => {
                if manager.with_lane_owner(&operation, || ()).is_err() { return; }
                let Some(event) = event else { return };
                match event {
                    ControllerLaneEvent::Failed(reason) => {
                        log_controller_event("inbound_failed", reason.code());
                        fail_owned_lane(manager, &operation, business, PEER_UNAVAILABLE_ERROR, sinks.on_status);
                        return;
                    }
                    ControllerLaneEvent::Disconnected(context) => {
                        if context.owner != registration.owner {
                            continue;
                        }
                        if resume.is_active() {
                            continue;
                        }
                        // A pre-authentication D carries no lease authority.
                        // Its non-reusable original port/intent may still fail
                        // that exact initial attempt, including a D queued
                        // after joined but before local handoff. Never use None
                        // to suspend/prepare the authenticated lease or budget.
                        if context.lease.is_none()
                            && (context.generation.is_empty()
                                || context.generation == registration.generation)
                        {
                            if manager.with_lane_owner(&operation, || business.clear()).is_err() {
                                return;
                            }
                            if let Ok(status) = manager.fail_connection(
                                operation.clone(), PEER_UNAVAILABLE_ERROR,
                            ) {
                                (sinks.on_status)(&status);
                            }
                            return;
                        }
                        if context != registration {
                            continue;
                        }
                        // 断线：保留密钥材料并挂起业务/存活帧，随后启动恢复流程。
                        let now_b = (evidence.monotonic)();
                        let Ok(status) = manager.mark_registered_transport_disconnected(
                            &operation, &registration, &context, now_b,
                            || budget.freeze(now_b), business,
                        ) else { return; };
                        (sinks.on_status)(&status);
                        if begin_resume(lane, manager, &operation, &mut resume, now_b) {
                            resume_deadline = budget.timer();
                        } else {
                            let _ = manager.with_lane_owner(&operation, || business.clear());
                            fail_resume(manager, &operation, sinks);
                            return;
                        }
                    }
                    ControllerLaneEvent::Frame(json) => {
                        if resume.is_active() {
                            let now_b = (evidence.monotonic)();
                            match handle_resume_frame(lane, manager, &operation, business, &mut resume, now_b, &json) {
                                ResumeOutcome::Progressed | ResumeOutcome::Ignored => {}
                                ResumeOutcome::Completed(status) => {
                                    if let Err(reason) = lane.resume_business() {
                                        log_controller_event("inbound_failed", reason.code());
                                        fail_owned_lane(manager, &operation, business, PEER_UNAVAILABLE_ERROR, sinks.on_status);
                                        return;
                                    }
                                    resume.reset();
                                    liveness = LivenessExpectations::default();
                                    // A transport resume is not a journal
                                    // response. Preserve stable pending replay
                                    // IDs and retry them on the next heartbeat.
                                    journal.rearm_pending(tokio::time::Instant::now());
                                    // Only the complete authenticated final result
                                    // can establish the next finite local cycle.
                                    budget = LivenessBudget::authenticated_cycle(now_b);
                                    resume_deadline = budget.timer();
                                    // The production sink owns the final state
                                    // fence. Never invoke it under the owner lock.
                                    (sinks.on_status)(&status);
                                }
                                ResumeOutcome::Failed => {
                                    let _ = manager.with_lane_owner(&operation, || business.clear());
                                    fail_resume(manager, &operation, sinks);
                                    return;
                                }
                            }
                        } else if budget.consume_peer_error(&json) {
                            log_controller_event("peer_evidence_rejected", "relay_rejected");
                        } else if serde_json::from_str::<Value>(&json).ok().and_then(|frame| frame["type"].as_str().map(str::to_owned)).as_deref() == Some("controller_liveness_attestation_b") {
                            let received_at = tokio::time::Instant::now();
                            let now_a = (evidence.monotonic)();
                            if let Some(trust) = evidence.trust {
                                if manager.with_peer_liveness_context(&operation, |runtime, epoch_b, _| budget.accept_peer_evidence(&json, trust, runtime, epoch_b, now_a, received_at)).is_ok_and(|accepted| accepted.is_ok()) {
                                    resume_deadline = budget.timer();
                                }
                            }
                        } else {
                            dispatch_inbound_frame(
                                manager,
                                &operation,
                                sessions,
                                business,
                                &mut journal,
                                &mut liveness,
                                &json,
                                sinks,
                            );
                        }
                    }
                }
            }
        }
    }
}

/// 恢复尝试的本地状态：`challenge_b64` 非空即代表一次进行中的恢复。
#[derive(Default)]
struct ResumeAttempt {
    lease: Option<LeaseContext>,
    activation_id: Option<String>,
    challenge_b64: String,
    confirmed: bool,
    ready_sent: bool,
    peer_epoch_b: Option<String>,
}

impl ResumeAttempt {
    fn is_active(&self) -> bool {
        !self.challenge_b64.is_empty()
    }

    fn reset(&mut self) {
        *self = Self::default();
    }
}

enum ResumeOutcome {
    Ignored,
    Progressed,
    Completed(ControllingStatus),
    Failed,
}

/// 本地单调毫秒（与 B 端 watchdog 同源），用于恢复预算比较。
fn lane_monotonic_millis() -> u64 {
    super::relay_runtime::controlled_monotonic_millis()
}

/// 恢复失败收尾：清空活动上下文并把状态投影为有界错误。
fn fail_resume(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    if let Ok(status) = manager.fail_resume_connection(operation.clone(), PEER_UNAVAILABLE_ERROR) {
        (sinks.on_status)(&status);
    }
}

/// 断线后启动恢复：取旧租约的恢复材料、生成挑战并发出 `controller_resume_prepare`。
fn begin_resume(
    lane: &(impl ControllingLane + ?Sized),
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    resume: &mut ResumeAttempt,
    now_b: u64,
) -> bool {
    let Ok(material) = manager.begin_resume(operation.clone(), now_b) else {
        return false;
    };
    let Ok(challenge) = new_resume_challenge() else {
        return false;
    };
    let request_id = format!("request-resume-{}", Uuid::new_v4().simple());
    let Ok(frame) = build_controller_resume_prepare(
        &material.lease,
        ControllerRole::ControllerA,
        &request_id,
        &material.resume_token,
        &challenge,
        material.relay_deadline_identity_ms,
    ) else {
        return false;
    };
    if lane.send_resume_control(frame).is_err() {
        return false;
    }
    resume.lease = Some(material.lease);
    resume.activation_id = None;
    resume.challenge_b64 = challenge;
    resume.confirmed = false;
    resume.ready_sent = false;
    true
}

/// 按 Relay/B 的恢复帧推进本地相位。任何相位不符、对端确认验签失败或
/// `controller_resumed` 材料不完整都失败关闭。
fn handle_resume_frame(
    lane: &(impl ControllingLane + ?Sized),
    manager: &ControllingDeviceManager,
    owner: &ControllerOperation,
    business: &ControllingLaneState,
    resume: &mut ResumeAttempt,
    now_b: u64,
    json: &str,
) -> ResumeOutcome {
    let Ok(frame) = serde_json::from_str::<Value>(json) else {
        return ResumeOutcome::Ignored;
    };
    let Some(kind) = frame.get("type").and_then(Value::as_str) else {
        return ResumeOutcome::Ignored;
    };
    let operation = owner.clone();
    if manager.with_lane_owner(owner, || ()).is_err() {
        return ResumeOutcome::Failed;
    }
    let Some(lease) = resume.lease.clone() else {
        return ResumeOutcome::Failed;
    };
    // 审核修复：Relay/A 的恢复帧必须绑定当前租约；旧代际或旧租约的残留帧一律
    // 丢弃，既不推进相位也不终止候选。
    if !resume_frame_matches_lease(&lease, &frame) {
        return ResumeOutcome::Ignored;
    }
    let request_id = || format!("request-resume-{}", Uuid::new_v4().simple());
    match kind {
        "controller_resume_provisional" => {
            let Some(activation_id) = frame.get("activation_id").and_then(Value::as_str) else {
                return ResumeOutcome::Ignored;
            };
            if resume.activation_id.is_some() {
                return ResumeOutcome::Ignored;
            }
            if manager
                .advance_resume(operation, ResumeStep::Provisional { activation_id }, now_b)
                .is_err()
            {
                return ResumeOutcome::Failed;
            }
            let Ok(Ok(confirm)) = manager.with_lane_owner(owner, || {
                business.seal_resume_confirm(&request_id(), activation_id)
            }) else {
                return ResumeOutcome::Failed;
            };
            if lane.send_resume_control(confirm).is_err() {
                return ResumeOutcome::Failed;
            }
            resume.activation_id = Some(activation_id.to_owned());
            ResumeOutcome::Progressed
        }
        // B 的确认只用于交叉验证（本地相位由 Relay 的帧推进）。
        "encrypted_resume_confirm" => {
            match manager.with_lane_owner(owner, || business.open_resume_confirm(&frame)) {
                Ok(Ok(())) => ResumeOutcome::Progressed,
                Ok(Err(_)) => ResumeOutcome::Ignored,
                Err(_) => ResumeOutcome::Failed,
            }
        }
        "controller_resume_activation_pending" => {
            let Some(activation_id) = frame.get("activation_id").and_then(Value::as_str) else {
                return ResumeOutcome::Ignored;
            };
            if resume.confirmed {
                return ResumeOutcome::Ignored;
            }
            if manager
                .advance_resume(
                    operation.clone(),
                    ResumeStep::Confirmed { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ResumeOutcome::Failed;
            }
            if manager
                .advance_resume(
                    operation,
                    ResumeStep::ActivationPending { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ResumeOutcome::Failed;
            }
            resume.confirmed = true;
            ResumeOutcome::Progressed
        }
        "controller_post_activation_liveness_confirmed" => {
            let Some(activation_id) = frame.get("activation_id").and_then(Value::as_str) else {
                return ResumeOutcome::Ignored;
            };
            let Some(liveness_epoch_b) = frame.get("liveness_epoch_b").and_then(Value::as_str)
            else {
                return ResumeOutcome::Ignored;
            };
            let Some(heartbeat_sequence_b) =
                frame.get("heartbeat_sequence_b").and_then(Value::as_str)
            else {
                return ResumeOutcome::Ignored;
            };
            if resume.ready_sent {
                return ResumeOutcome::Ignored;
            }
            if manager
                .advance_resume(
                    operation.clone(),
                    ResumeStep::LivenessConfirmed { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ResumeOutcome::Failed;
            }
            let Ok(ready) = build_controller_resume_ready_ack(
                &lease,
                ControllerRole::ControllerA,
                &request_id(),
                activation_id,
                liveness_epoch_b,
                heartbeat_sequence_b,
            ) else {
                return ResumeOutcome::Failed;
            };
            // 本端 ready_ack 也是 gate 的最后一步本地相位：只有完成它，
            // `controller_resumed` 才可能被接受。
            if manager
                .advance_resume(
                    operation,
                    ResumeStep::ReadyAcknowledged { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ResumeOutcome::Failed;
            }
            if lane.send_resume_control(ready).is_err() {
                return ResumeOutcome::Failed;
            }
            resume.ready_sent = true;
            resume.peer_epoch_b = Some(liveness_epoch_b.to_owned());
            ResumeOutcome::Progressed
        }
        "controller_resumed" => {
            if !resume.ready_sent {
                return ResumeOutcome::Ignored;
            }
            let Some(activation_id) = frame.get("activation_id").and_then(Value::as_str) else {
                return ResumeOutcome::Ignored;
            };
            let Some(liveness_epoch_a) = frame.get("liveness_epoch_a").and_then(Value::as_str)
            else {
                return ResumeOutcome::Ignored;
            };
            let Some(liveness_epoch_b) = frame.get("liveness_epoch_b").and_then(Value::as_str)
            else {
                return ResumeOutcome::Ignored;
            };
            if resume.peer_epoch_b.as_deref() != Some(liveness_epoch_b) {
                return ResumeOutcome::Failed;
            }
            let Some(new_token) = frame.get("new_resume_token_b64").and_then(Value::as_str) else {
                return ResumeOutcome::Ignored;
            };
            let Some(relay_deadline) = relay_resume_deadline_ms(&frame) else {
                return ResumeOutcome::Failed;
            };
            let Ok(decoded) = base64::engine::general_purpose::STANDARD.decode(new_token) else {
                return ResumeOutcome::Ignored;
            };
            let Ok(token) = <[u8; 32]>::try_from(decoded.as_slice()) else {
                return ResumeOutcome::Ignored;
            };
            let Ok(Ok(fingerprint)) =
                manager.with_lane_owner(owner, || business.resume_fingerprint(activation_id))
            else {
                return ResumeOutcome::Failed;
            };
            let Some(resume_request_id) = frame.get("request_id").and_then(Value::as_str) else {
                return ResumeOutcome::Failed;
            };
            let Ok(status) = manager.complete_resumed(
                operation,
                activation_id,
                resume_request_id,
                now_b,
                (liveness_epoch_a, liveness_epoch_b),
                token,
                fingerprint,
                relay_deadline,
            ) else {
                return ResumeOutcome::Failed;
            };
            #[cfg(test)]
            lane.before_resume_activation();
            if manager
                .with_status_projection(owner, &status, || business.activate())
                .is_err()
            {
                return ResumeOutcome::Failed;
            }
            #[cfg(test)]
            lane.after_resume_activation();
            ResumeOutcome::Completed(status)
        }
        "error" => ResumeOutcome::Failed,
        _ => ResumeOutcome::Ignored,
    }
}

/// 生成下一条心跳帧：只在存在活租约（connected）时产出，序号单调递增；租约
/// 变更后序号从 1 重新开始（epoch 同步换新，接收侧按新 epoch 接受）。
fn next_liveness_frame(
    manager: &ControllingDeviceManager,
    liveness: &mut LivenessExpectations,
) -> Option<Value> {
    let (lease, _) = manager.live_runtime_context().ok()?;
    let epoch = manager.live_liveness_epoch()?;
    let next = liveness.next_sequence()?;
    let frame = crate::features::remote_control::controller::liveness::build_controller_liveness(
        &lease,
        SenderRole::ControllerA,
        &epoch,
        next,
    )
    .ok()?;
    Some(frame)
}

/// 把状态投影压成一行排障文本：与 UI 收到的 `status` / `errorCode` 同源，
/// 不含连接码、租约、指纹或设备标识。
fn controller_status_log_detail(status: &ControllingStatus) -> String {
    let Ok(value) = serde_json::to_value(status) else {
        return "unavailable".to_owned();
    };
    let state = value
        .get("status")
        .and_then(Value::as_str)
        .unwrap_or("unknown");
    match value.get("errorCode").and_then(Value::as_str) {
        Some(code) => format!("state:{state}/error:{code}"),
        None => format!("state:{state}"),
    }
}

/// The production keeper's synchronous state consumer. Both transition tracking
/// and publication use the same captured snapshot under one final owner/state
/// fence. Ports must not re-enter manager; unavailable telemetry remains optional.
/// A single keeper calls this before processing its next event, so no asynchronous
/// status queue or cross-cycle replay is introduced here.
fn keeper_status_sink<'a>(
    manager: &'a ControllingDeviceManager,
    operation: &'a ControllerOperation,
    mut track: impl FnMut(RemoteControlEvent) + Send + 'a,
    mut publish: impl FnMut(&ControllingStatus) + Send + 'a,
) -> impl FnMut(&ControllingStatus) + Send + 'a {
    let mut previous_status = ControllingStatusKind::Connected;
    move |status| {
        let _ = manager.with_status_projection(operation, status, || {
            let current = status.kind();
            if current != previous_status {
                let event = match (previous_status, current) {
                    (ControllingStatusKind::Connected, ControllingStatusKind::Reconnecting) => {
                        Some(RemoteControlEvent::new(
                            RemoteStage::Disconnection,
                            RemoteOutcome::Ok,
                        ))
                    }
                    (ControllingStatusKind::Reconnecting, ControllingStatusKind::Connected) => {
                        Some(RemoteControlEvent::new(
                            RemoteStage::Resume,
                            RemoteOutcome::Ok,
                        ))
                    }
                    (ControllingStatusKind::Reconnecting, ControllingStatusKind::Error) => Some(
                        RemoteControlEvent::new(RemoteStage::Resume, RemoteOutcome::Rejected)
                            .reason(RemoteReason::ResumeFailed),
                    ),
                    _ => None,
                };
                if let Some(event) = event {
                    track(event.role(false));
                }
                previous_status = current;
            }
            publish(status);
        });
    }
}

/// 连接入口的早期准备：取待决连接码、加载 Relay 信任根、生成握手密钥。
/// 任何失败都与 `run_controlling_connection` 的失败路径走同一收敛
/// （`fail_connection`：清空待决连接码并投影有界公共错误码），不让 manager
/// 停在 Connecting；返回给命令层的错误码保持调用方原有语义。
fn prepare_connect_materials(
    manager: &ControllingDeviceManager,
    operation: ControllerOperation,
    trust_lookup: impl Fn(&str) -> Option<String>,
    on_status: &mut impl FnMut(&ControllingStatus),
) -> Result<(String, AuthenticatedRelayTrustConfig, HandshakeKeyPair), String> {
    let code = match manager.pending_code(operation.clone()) {
        Ok(code) => code,
        Err(error) => {
            let code = error.code;
            let _ = fail_connection(manager, operation, GENERIC_CONNECT_ERROR, on_status);
            return Err(code.to_owned());
        }
    };
    // 部署期预置的信任根缺失或非法时失败关闭：没有受信 Relay 就不存在可验签
    // 的 transcript，绝不能降级为「无认证握手」。
    let trust = match load_relay_trust_config(trust_lookup) {
        Ok(trust) => trust,
        Err(_) => {
            return Err(fail_connection(
                manager,
                operation,
                GENERIC_CONNECT_ERROR,
                on_status,
            ));
        }
    };
    let keys = match HandshakeKeyPair::generate() {
        Ok(keys) => keys,
        Err(_) => {
            return Err(fail_connection(
                manager,
                operation,
                GENERIC_CONNECT_ERROR,
                on_status,
            ));
        }
    };
    Ok((code, trust, keys))
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
    let mut emit = {
        let operation = operation.clone();
        move |status: &ControllingStatus| {
            let _ = manager.with_status_projection(&operation, status, || {
                log_controller_event("status", &controller_status_log_detail(status));
                let _ = app.emit(CONTROLLER_STATUS_EVENT, status);
            });
        }
    };
    let (code, trust, keys) = prepare_connect_materials(
        manager,
        operation.clone(),
        |name| std::env::var(name).ok(),
        &mut emit,
    )?;
    let mut handshake = ControllingHandshake::new(
        format!("request-{}", Uuid::new_v4().simple()),
        operation.claim_request_id().to_owned(),
        code,
        keys,
        trust.clone(),
    );
    let (sender, mut port) = spawn_controlling_lane(&relay.controller_relay_ws_url());
    // 先登记 transport，保证并发的断开/重连能立刻停止本次尝试。
    let business = app.state::<ControllingLaneState>();
    if let Err(error) = manager.replace_owned_transport(&operation, sender.clone(), &business) {
        sender.stop();
        let _ = fail_connection(manager, operation, GENERIC_CONNECT_ERROR, &mut emit);
        return Err(error.code.to_owned());
    }
    let operation = operation.for_transport(sender.owner());

    // A 端排障日志：先把「这次到底连的是哪个端点的哪条路径」写死，再把每个状态
    // 投影与最终结果码记下来。没有这几行时，「连接码无效或已过期」无法区分
    // 「码不对」与「两端其实在各自的 Relay 上」。
    let endpoint_detail = relay.controller_endpoint_log_detail();
    log_controller_event("connect_started", &endpoint_detail);
    let status = match run_controlling_connection(
        manager,
        operation.clone(),
        &mut port,
        &mut handshake,
        &sender,
        business.inner(),
        HANDSHAKE_DEADLINE,
        &mut emit,
    )
    .await
    {
        Ok(status) => {
            log_controller_event("connected", &endpoint_detail);
            status
        }
        Err(error) => {
            // 错误码已由 `fail_connection` 收敛为有界集合，直接落盘即可。
            log_controller_event("connect_failed", &error);
            return Err(error);
        }
    };

    let keeper = app.clone();
    tauri::async_runtime::spawn(async move {
        let manager = keeper.state::<ControllingDeviceManager>();
        let sessions = keeper.state::<ControllingSessionManager>();
        let business = keeper.state::<ControllingLaneState>();
        let mut on_status = keeper_status_sink(
            manager.inner(),
            &operation,
            |event| track_remote_control(&keeper, event),
            |status| {
                log_controller_event("status", &controller_status_log_detail(status));
                let _ = keeper.emit(CONTROLLER_STATUS_EVENT, status);
            },
        );
        let capability_sink = Arc::clone(&journal);
        let mut on_session = |projection: &ControllingSessionProjection| {
            let _ = capability_sink.publish_capability(&operation, projection);
        };
        let mapping = Arc::clone(&journal);
        let mut on_mapping = |lease: &LeaseContext, remote_ref: &str, controlled_id: &str| {
            mapping.confirm_mapping(&operation, lease, remote_ref, controlled_id)
        };
        let action_sink = Arc::clone(&journal);
        let mut on_task = |lease: &LeaseContext, event: Value| {
            if event.get("type").and_then(Value::as_str) == Some("remote_action_result") {
                let _ = action_sink.publish_action_result(&operation, lease, &event);
            } else if event.get("rejectionCode").and_then(Value::as_str).is_some() {
                let _ = action_sink.publish_task_rejection(&operation, lease, &event);
            } else {
                let _ = manager
                    .with_lane_owner(&operation, || keeper.emit(CONTROLLER_TASK_EVENT, event));
            }
        };
        let mut on_journal = |frame: &AuthenticatedJournalFrame<'_>| {
            // A 端经 app 组合适配器写入 SessionStore.RemoteProxy。
            journal.publish(frame)
        };
        let mut on_snapshot = |frame: &AuthenticatedSnapshotFrame<'_>| {
            // E2E-05 缺口恢复：权威快照原子替换 A 的本地副本。
            journal.publish_snapshot(frame)
        };
        let mut on_liveness = |frame: Value| {
            // Pinned to this connection even if the singleton is replaced.
            sender.try_send_authenticated_control(&frame).is_ok()
        };
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut on_mapping,
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        serve_controlling_lane_with_evidence_context(
            &mut port,
            manager.inner(),
            sessions.inner(),
            business.inner(),
            &mut sinks,
            Duration::from_millis(LIVENESS_HEARTBEAT_INTERVAL_MS),
            PeerEvidenceContext {
                trust: Some(&trust),
                monotonic: &lane_monotonic_millis,
                operation: operation.clone(),
            },
        )
        .await;
    });
    Ok(status)
}

#[cfg(test)]
#[path = "controlling_resume_projection_tests.rs"]
mod resume_projection_tests;

#[cfg(test)]
#[path = "controlling_inbound_tests.rs"]
mod inbound_tests;

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::collections::VecDeque;

    use serde_json::json;

    use super::*;

    // Test-only fixture uses the request-correlated, mapping-checked
    // capability admission path rather than the removed direct-apply API.
    trait SessionCapabilityFixture {
        fn apply_verified_snapshot_for_test(
            &self,
            remote_ref: &str,
            payload: &Value,
            lease: &LeaseContext,
        ) -> std::result::Result<
            ControllingSessionProjection,
            super::super::controlling_session::SessionError,
        >;
    }

    impl SessionCapabilityFixture for ControllingSessionManager {
        fn apply_verified_snapshot_for_test(
            &self,
            remote_ref: &str,
            payload: &Value,
            lease: &LeaseContext,
        ) -> std::result::Result<
            ControllingSessionProjection,
            super::super::controlling_session::SessionError,
        > {
            let request = self.capability_request(remote_ref, lease)?;
            let mut response = payload.clone();
            response["request_id"] = json!(request.request_id);
            response["session_id"] = json!(request.session_id);
            response["controlled_session_id"] = json!("b-session-1");
            self.apply_capability_snapshot_for_lease_with_mapping(
                lease,
                &response,
                |actual_ref, b_session| {
                    assert_eq!(actual_ref, remote_ref);
                    assert_eq!(b_session, "b-session-1");
                    Ok(())
                },
            )
        }
    }
    use crate::features::remote_control::controller::controlled_handshake::ControlledHandshake;
    use crate::features::remote_control::controller::crypto::AuthenticatedRelayTrustConfig;
    use crate::features::remote_control::controller::encrypted_lane::EncryptedLane;
    use crate::features::remote_control::controller::relay_transport::transport_channels;
    use crate::features::remote_control::controller::test_support::{
        PairingFixture, encode, lease_context, paired_session_keys, pairing_fixture,
        role_session_keys, signed_relay, signed_reservation,
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
                // Relay router 契约：发送方的 `finished_receipt_commit` 会被原样回投
                // 给发送方（`remote-control-relay/test/controller-router.test.js`）。
                // 这里按真实链路注入自帧回投：A 端必须跳过它，而不是当成对端
                // commitment 失败关闭（2026-09-20 真机正是此处的对偶问题）。
                2 => self
                    .outbound
                    .borrow()
                    .iter()
                    .find(|message| message["type"] == "finished_receipt_commit")
                    .cloned(),
                3 => Some(self.peer.commit().unwrap()),
                4 => {
                    let a_commit = self
                        .outbound
                        .borrow()
                        .iter()
                        .find(|message| message["type"] == "finished_receipt_commit")?
                        .clone();
                    self.peer.accept_peer_commitment(&a_commit).unwrap();
                    Some(self.peer.finished().unwrap())
                }
                5 => {
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

    fn registered_lane_sender(manager: &ControllingDeviceManager) -> ControllerTransportSender {
        let (context, capabilities) = manager.live_runtime_context().unwrap();
        let sender = super::super::test_support::resume_reader_transport(
            ControllerRole::ControllerA,
            context,
            capabilities,
        );
        manager.replace_transport(sender.clone());
        sender
    }

    #[derive(Default)]
    struct TestLane {
        events: VecDeque<ControllerLaneEvent>,
        reader_transport: Option<ControllerTransportSender>,
        authorize_resume_final: bool,
        /// E2E-06：恢复族出站帧（prepare/confirm/ready_ack）的记录口。
        resume_frames: Arc<std::sync::Mutex<Vec<Value>>>,
    }

    impl TestLane {
        fn bind(&mut self, manager: &ControllingDeviceManager) {
            if self.reader_transport.is_none() {
                self.reader_transport = Some(registered_lane_sender(manager));
            }
        }

        fn with_events(events: Vec<ControllerLaneEvent>) -> Self {
            Self {
                events: VecDeque::from(events),
                ..Self::default()
            }
        }

        fn resume_frames(&self) -> Vec<Value> {
            self.resume_frames.lock().unwrap().clone()
        }
    }

    #[async_trait::async_trait]
    impl ControllingLane for TestLane {
        fn transport_sender(&self) -> Option<ControllerTransportSender> {
            self.reader_transport.clone()
        }

        async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
            let event = self.events.pop_front();
            if let Some(sender) = self.reader_transport.as_ref() {
                match event.as_ref() {
                    Some(ControllerLaneEvent::Disconnected(_)) => {
                        super::super::relay_transport::disconnect_test_transport(sender);
                    }
                    Some(ControllerLaneEvent::Frame(text)) if self.authorize_resume_final => {
                        if let Ok(frame) = serde_json::from_str::<Value>(text)
                            && frame["type"] == "controller_resumed"
                        {
                            super::super::relay_transport::dispatch_test_transport_frame(
                                sender, &frame,
                            );
                        }
                    }
                    _ => {}
                }
            }
            event
        }

        fn send_resume_control(&self, message: Value) -> Result<(), String> {
            self.resume_frames.lock().unwrap().push(message);
            Ok(())
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
    async fn connect_attempt_retry_reaches_connected_through_the_real_a_port() {
        use super::super::test_support::ControllerReaderProtocol;
        use futures_util::{SinkExt, StreamExt};
        use tokio::io::AsyncReadExt;
        use tokio::net::TcpListener;
        use tokio_tungstenite::{accept_hdr_async, tungstenite::Message};

        let PairingFixture {
            relay,
            reservation,
            joined,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture(RELAY_KEY_ID);
        let mut peer = ControlledHandshake::new(&reservation, b_keys, &relay.trust).unwrap();
        let mut handshake = a_handshake(&reservation, a_keys, relay.trust.clone());
        let expected_request = reservation["request_id"].clone();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let config = ControllerTransportConfig::new(
            format!("ws://{}", listener.local_addr().unwrap()),
            ControllerRole::ControllerA,
            "",
        )
        .with_connect_timeout(Duration::from_millis(250));
        let (release, held) = tokio::sync::oneshot::channel::<()>();
        let mut server = tokio::spawn(async move {
            let (mut first, _) = listener.accept().await.unwrap();
            let mut bytes = [0_u8; 2048];
            let mut received = 0;
            loop {
                let n = first.read(&mut bytes).await.unwrap();
                if n == 0 {
                    break;
                }
                received += n;
            }
            assert!(received > 0, "attempt must reach upgrade then be dropped");
            let (stream, _) = listener.accept().await.unwrap();
            let mut socket = accept_hdr_async(stream, ControllerReaderProtocol)
                .await
                .unwrap();
            let Message::Text(claim) = socket.next().await.unwrap().unwrap() else {
                panic!("claim");
            };
            let claim: Value = serde_json::from_str(&claim).unwrap();
            assert_eq!(claim["type"], "controller_pairing_claim");
            assert_eq!(claim["request_id"], expected_request);
            socket
                .send(Message::Text(reservation.to_string().into()))
                .await
                .unwrap();
            let Message::Text(commit) = socket.next().await.unwrap().unwrap() else {
                panic!("A commit");
            };
            let commit: Value = serde_json::from_str(&commit).unwrap();
            assert_eq!(commit["type"], "finished_receipt_commit");
            // Match the Relay's sender echo before delivering B's real output.
            socket
                .send(Message::Text(commit.to_string().into()))
                .await
                .unwrap();
            socket
                .send(Message::Text(peer.commit().unwrap().to_string().into()))
                .await
                .unwrap();
            peer.accept_peer_commitment(&commit).unwrap();
            socket
                .send(Message::Text(peer.finished().unwrap().to_string().into()))
                .await
                .unwrap();
            let Message::Text(finished) = socket.next().await.unwrap().unwrap() else {
                panic!("A finished");
            };
            let ack = peer
                .accept_peer_finished(&serde_json::from_str::<Value>(&finished).unwrap())
                .unwrap();
            assert_eq!(ack["receiver_role"], "B");
            let Message::Text(receipt) = socket.next().await.unwrap().unwrap() else {
                panic!("A receipt ack");
            };
            assert_eq!(
                serde_json::from_str::<Value>(&receipt).unwrap()["type"],
                "finished_receipt_ack"
            );
            socket
                .send(Message::Text(joined.to_string().into()))
                .await
                .unwrap();
            let _ = held.await;
        });
        let (sender, mut port) = spawn_controlling_lane_with_config(config);
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        let business = ControllingLaneState::default();
        manager
            .replace_owned_transport(&operation, sender.clone(), &business)
            .unwrap();
        let mut statuses = Vec::new();
        let result = tokio::time::timeout(
            Duration::from_secs(10),
            run_controlling_connection(
                &manager,
                operation,
                &mut port,
                &mut handshake,
                &sender,
                &business,
                Duration::from_secs(8),
                &mut |s| statuses.push(projection(s)["status"].clone()),
            ),
        )
        .await;
        let installed = sender.authenticated_lease();
        let manager_owned_sender_running = manager.is_running();
        sender.stop();
        let _ = release.send(());
        if !matches!(&result, Ok(Ok(_))) {
            server.abort();
        }
        let server_result = tokio::time::timeout(Duration::from_secs(1), &mut server).await;
        if server_result.is_err() {
            server.abort();
            let _ = server.await;
        }
        let status = result
            .expect("A scenario must finish within its independent hard limit")
            .expect("first attempt expiry must not end the actual A handshake");
        server_result
            .expect("test server must finish after release")
            .unwrap();
        assert_eq!(projection(&status)["status"], "connected");
        assert_eq!(projection(&manager.status())["status"], "connected");
        assert_eq!(
            statuses,
            vec![json!("safety_handshake"), json!("connected")]
        );
        assert_eq!(installed.unwrap().context, manager.live_lease().unwrap());
        assert!(manager_owned_sender_running);
        assert!(!port.lane_disconnected());
    }

    #[tokio::test]
    async fn connect_attempt_retries_cannot_extend_the_a_operation_budget() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        use tokio::io::AsyncReadExt;
        use tokio::net::TcpListener;

        let PairingFixture {
            relay,
            reservation,
            a_keys,
            ..
        } = pairing_fixture(RELAY_KEY_ID);
        let mut handshake = a_handshake(&reservation, a_keys, relay.trust.clone());
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let config = ControllerTransportConfig::new(
            format!("ws://{}", listener.local_addr().unwrap()),
            ControllerRole::ControllerA,
            "",
        )
        .with_connect_timeout(Duration::from_millis(250));
        let attempts = Arc::new(AtomicUsize::new(0));
        let count = attempts.clone();
        let server = tokio::spawn(async move {
            loop {
                let (mut socket, _) = listener.accept().await.unwrap();
                count.fetch_add(1, Ordering::SeqCst);
                let mut bytes = [0_u8; 2048];
                while socket.read(&mut bytes).await.unwrap() != 0 {}
            }
        });
        let (sender, mut port) = spawn_controlling_lane_with_config(config);
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        let business = ControllingLaneState::default();
        manager
            .replace_owned_transport(&operation, sender.clone(), &business)
            .unwrap();
        let mut statuses = Vec::new();
        let budget = Duration::from_millis(1400);
        let start = tokio::time::Instant::now();
        let result = tokio::time::timeout(
            Duration::from_secs(3),
            run_controlling_connection(
                &manager,
                operation.clone(),
                &mut port,
                &mut handshake,
                &sender,
                &business,
                budget,
                &mut |s| statuses.push(projection(s)),
            ),
        )
        .await;
        let elapsed_at_return = start.elapsed();
        let stopped_by_driver = sender.is_stopped();
        let count_at_stop = attempts.load(Ordering::SeqCst);
        tokio::time::sleep(Duration::from_millis(700)).await;
        let count_after_stop = attempts.load(Ordering::SeqCst);
        sender.stop(); // Failure cleanup, never evidence of the production Stop.
        server.abort();
        let _ = server.await;
        assert_eq!(
            result
                .expect("outer budget must finish before the independent hard limit")
                .unwrap_err(),
            PEER_UNAVAILABLE_ERROR
        );
        assert!(elapsed_at_return >= budget && elapsed_at_return < budget + Duration::from_secs(1));
        assert!(
            stopped_by_driver,
            "outer budget failure must stop the production sender"
        );
        assert!(
            count_at_stop >= 2,
            "attempt timeout must permit a real second connection"
        );
        assert_eq!(
            count_at_stop, count_after_stop,
            "outer failure stops future retries"
        );
        assert!(sender.is_stopped());
        assert!(sender.authenticated_lease().is_none());
        assert!(manager.live_lease().is_err());
        assert!(manager.pending_code(operation).is_err());
        assert_eq!(statuses.len(), 1);
        assert_eq!(statuses[0]["status"], "error");
        assert!(
            !port.lane_disconnected(),
            "outer timer, not a first-attempt Disconnected"
        );
        assert!(manager.connect("234-4567").is_ok());
    }

    #[tokio::test]
    async fn connect_attempt_replacement_stops_pending_old_upgrade_without_reviving_owner() {
        use tokio::io::AsyncReadExt;
        use tokio::net::TcpListener;

        let PairingFixture {
            relay,
            reservation,
            a_keys,
            ..
        } = pairing_fixture(RELAY_KEY_ID);
        let mut handshake = a_handshake(&reservation, a_keys, relay.trust.clone());
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let config = ControllerTransportConfig::new(
            format!("ws://{}", listener.local_addr().unwrap()),
            ControllerRole::ControllerA,
            "",
        )
        .with_connect_timeout(Duration::from_secs(2));
        let (sender, mut port) = spawn_controlling_lane_with_config(config);
        let manager = ControllingDeviceManager::default();
        let business = ControllingLaneState::default();
        let operation = manager.connect("234-4567").unwrap();
        manager
            .replace_owned_transport(&operation, sender.clone(), &business)
            .unwrap();
        let mut statuses = Vec::new();
        let mut on_status = |s: &ControllingStatus| statuses.push(projection(s));
        let old_driver = run_controlling_connection(
            &manager,
            operation.clone(),
            &mut port,
            &mut handshake,
            &sender,
            &business,
            Duration::from_secs(5),
            &mut on_status,
        );
        let replacement = async {
            let (mut pending, _) = listener.accept().await.unwrap();
            let mut bytes = [0_u8; 2048];
            assert!(pending.read(&mut bytes).await.unwrap() > 0);
            // The public manager entry invalidates the old intent and stops
            // its registered real transport while upgrade is still pending.
            let next = manager.connect("345-5678").unwrap();
            let (next_sender, next_receiver) =
                transport_channels(ControllerRole::ControllerA, "", 2, 2, 8192, 1024);
            manager
                .replace_owned_transport(&next, next_sender.clone(), &business)
                .unwrap();
            while pending.read(&mut bytes).await.unwrap() != 0 {}
            (next, next_sender, next_receiver)
        };
        let result = tokio::time::timeout(Duration::from_secs(3), async {
            tokio::join!(old_driver, replacement)
        })
        .await;
        sender.stop(); // Cleanup only; the next-owner assertions do not depend on it.
        let (old_result, (next, next_sender, _next_receiver)) =
            result.expect("replacement must cancel pending upgrade promptly");
        let new_running = manager.is_running() && !next_sender.is_stopped();
        let new_status = projection(&manager.status());
        let new_code_retained = manager.pending_code(next).is_ok();
        manager.stop();
        assert!(old_result.is_err());
        assert!(
            statuses.is_empty(),
            "old failure cannot publish over the new intent"
        );
        assert!(new_running);
        assert_eq!(new_status["status"], "connecting");
        assert!(new_code_retained);
        assert!(manager.pending_code(operation).is_err());
        assert!(sender.authenticated_lease().is_none());
    }

    #[test]
    fn early_connect_material_failure_projects_error_and_clears_pending_code() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        let mut projected = Vec::new();
        // 只配一半的信任根必然失败关闭（lookup 注入，不触碰进程级环境变量）。
        let result = prepare_connect_materials(
            &manager,
            operation.clone(),
            |name| (name == "PINVOU_REMOTE_RELAY_SIGNING_KEY_ID").then(|| "relay-key-1".to_owned()),
            &mut |status: &ControllingStatus| projected.push(projection(status)),
        );
        assert_eq!(result.err().unwrap(), GENERIC_CONNECT_ERROR);
        assert_eq!(projected.len(), 1);
        assert_eq!(projected[0]["status"], "error");
        assert_eq!(projected[0]["errorCode"], GENERIC_CONNECT_ERROR);
        // 待决连接码已清空：同一 operation 取不到码，用户可以直接重新连接。
        assert!(manager.pending_code(operation).is_err());
        assert!(manager.connect("234-4567").is_ok());
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

    fn adapter_fixture_lease() -> ControllerLease {
        ControllerLease {
            context: LeaseContext {
                connection_id: "connection-0001".into(),
                controller_lease_id: "controller-lease-0001".into(),
                lease_epoch: "1".into(),
                relay_epoch: "epoch-test-1".into(),
                remote_control_generation: "generation-0001".into(),
            },
            peer_endpoint_id: "endpoint-0001".into(),
            reservation_id: "reservation-0001".into(),
            device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
            liveness_epoch_a: "liveness-epoch-a-0001".into(),
            liveness_epoch_b: "liveness-epoch-b-0001".into(),
            resume_deadline_b: 30_000,
            resume_token: [7_u8; 32],
            transport_key_fingerprint: [9_u8; 32],
            negotiated_capabilities: vec![
                "encrypted_frames_v1".into(),
                "pairing_v1".into(),
                "task_v1".into(),
            ],
        }
    }

    #[tokio::test]
    async fn actual_adapter_current_disconnect_preserves_full_registration_and_prepares_resume() {
        assert_actual_adapter_case("current").await;
    }

    #[tokio::test]
    async fn actual_adapter_pre_auth_disconnect_fails_only_its_original_attempt() {
        assert_actual_adapter_case("pre_auth").await;
    }

    #[tokio::test]
    async fn actual_adapter_old_queued_disconnect_preserves_replacement_pairing_code() {
        assert_actual_adapter_case("replacement").await;
    }

    async fn assert_actual_adapter_case(case: &str) {
        let manager = ControllingDeviceManager::default();
        let business = ControllingLaneState::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        let mut fixture = pairing_fixture("relay-actual-adapter");
        super::super::test_support::bind_pairing_generation(
            &mut fixture,
            &format!("generation-{}", Uuid::new_v4().simple()),
        );
        let context = lease_context(&fixture.reservation);
        let (session, _) =
            paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let mut lease = adapter_fixture_lease();
        lease.context = context.clone();
        lease.negotiated_capabilities.push("resume_v1".into());
        let (sender, receiver) = transport_channels(
            ControllerRole::ControllerA,
            &context.remote_control_generation,
            8,
            2,
            8192,
            1024,
        );
        // The inbox stays unbound from the transport on purpose: the test's
        // producer teardown must not stop the sender before the resume
        // prepare exercises the real transport queue below.
        let (ingress_tx, inbound) = inbound_channel(InboundLimits::default());
        let install_sender = || {
            sender
                .install_authenticated_lease(AuthenticatedLease {
                    context: context.clone(),
                    negotiated_capabilities: lease.negotiated_capabilities.clone(),
                })
                .unwrap()
        };
        if case != "pre_auth" {
            install_sender();
        }
        let expected = sender.disconnect_context();
        let sink_tx = ingress_tx.clone();
        let sink = move |event: ControllerTransportEvent| {
            let ControllerTransportEvent::Disconnected { context, .. } = &event else {
                panic!("expected the actual transport disconnect producer");
            };
            assert_eq!(context, &expected);
            sink_tx.enqueue(event);
        };
        // Exercise the actual transport producer, suspension, production
        // adapter, inbox, production port and keeper. The WebSocket driver
        // itself is outside this A adapter test's scope.
        super::super::relay_transport::disconnect_test_transport_to_sink(&sender, &sink);
        drop(sink);
        drop(ingress_tx);
        if case == "pre_auth" {
            install_sender();
        }
        manager
            .replace_owned_transport(&operation, sender.clone(), &business)
            .unwrap();
        business
            .install(HandshakeRole::A, &context, session)
            .unwrap();
        manager
            .complete_connected(operation.clone(), lease)
            .unwrap();
        let mut port = ControllerTransportPort::new(sender.clone(), inbound);
        let replacement = if case == "replacement" {
            manager.disconnect();
            business.clear();
            Some(manager.connect("345-6789").unwrap())
        } else {
            None
        };
        let sessions = ControllingSessionManager::default();
        let mut seen = Vec::new();
        let mut on_status = |status: &ControllingStatus| seen.push(status.kind());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut |_| panic!("disconnect dispatched a session"),
            on_mapping: &mut |_, _, _| panic!("disconnect committed a mapping"),
            on_task: &mut |_, _| panic!("disconnect dispatched a task"),
            on_journal: &mut |_| panic!("disconnect committed a journal"),
            on_snapshot: &mut |_| panic!("disconnect committed a snapshot"),
            on_liveness: &mut |_| false,
        };
        let clock = || 5_000;
        serve_controlling_lane_with_evidence_context(
            &mut port,
            &manager,
            &sessions,
            &business,
            &mut sinks,
            Duration::from_secs(60),
            PeerEvidenceContext {
                trust: None,
                monotonic: &clock,
                operation,
            },
        )
        .await;
        match case {
            "current" => {
                // The accepted disconnect still drives Reconnecting and the
                // validated resume prepare; the producer teardown's Closed
                // terminal then ends the lane with the bounded error.
                assert_eq!(
                    seen,
                    vec![
                        ControllingStatusKind::Reconnecting,
                        ControllingStatusKind::Error
                    ]
                );
                assert_eq!(
                    receiver.pending_control_for_test(),
                    1,
                    "the actual port must enqueue a validated resume prepare"
                );
            }
            "pre_auth" => {
                assert_eq!(seen, vec![ControllingStatusKind::Error]);
                assert_eq!(
                    projection(&manager.status())["errorCode"],
                    "endpoint_offline"
                );
                assert!(!business.is_installed());
                assert!(sender.is_stopped());
                assert_eq!(
                    receiver.pending_control_for_test(),
                    0,
                    "None lease context cannot acquire resume authority"
                );
            }
            "replacement" => {
                assert!(seen.is_empty());
                assert_eq!(manager.status().kind(), ControllingStatusKind::Connecting);
                assert_eq!(
                    manager.pending_code(replacement.unwrap()).unwrap(),
                    "3456789"
                );
                assert_eq!(receiver.pending_control_for_test(), 0);
            }
            other => panic!("unknown adapter case {other}"),
        }
    }

    #[tokio::test]
    async fn keeper_without_original_registration_cannot_read_or_send() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(operation, adapter_fixture_lease())
            .unwrap();
        let mut lane = TestLane::with_events(vec![ControllerLaneEvent::Frame("{}".into())]);
        let mut sinks = ControllingLaneSinks {
            on_status: &mut |_| panic!("unregistered keeper cannot project"),
            on_session: &mut |_| panic!("unregistered keeper cannot project"),
            on_mapping: &mut |_, _, _| panic!("unregistered keeper cannot map"),
            on_task: &mut |_, _| panic!("unregistered keeper cannot dispatch"),
            on_journal: &mut |_| panic!("unregistered keeper cannot dispatch"),
            on_snapshot: &mut |_| panic!("unregistered keeper cannot dispatch"),
            on_liveness: &mut |_| panic!("unregistered keeper cannot send"),
        };
        serve_controlling_lane(
            &mut lane,
            &manager,
            &ControllingSessionManager::default(),
            &ControllingLaneState::default(),
            &mut sinks,
        )
        .await;
        assert_eq!(
            lane.events.len(),
            1,
            "entry must reject before reading a frame"
        );
        assert_eq!(manager.status().kind(), ControllingStatusKind::Connected);
    }

    #[tokio::test]
    async fn post_handshake_disconnect_projects_reconnecting_and_keeps_draining() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(operation, adapter_fixture_lease())
            .unwrap();
        let (context, capabilities) = manager.live_runtime_context().unwrap();
        let reader_transport = super::super::test_support::resume_reader_transport(
            ControllerRole::ControllerA,
            context,
            capabilities,
        );
        manager.replace_transport(reader_transport.clone());
        let mut lane = TestLane {
            events: VecDeque::from(vec![
                ControllerLaneEvent::Disconnected(reader_transport.disconnect_context()),
                ControllerLaneEvent::Frame("{}".to_owned()),
            ]),
            reader_transport: Some(reader_transport),
            ..TestLane::default()
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
        let mut on_task = |_: &LeaseContext, event: Value| tasks_seen.push(event);
        let mut journal_seen: Vec<JournalEvent> = Vec::new();
        let mut on_journal = |frame: &AuthenticatedJournalFrame<'_>| {
            journal_seen.push(frame.event().clone());
            Ok(())
        };
        let mut snapshots_seen: Vec<JournalSnapshot> = Vec::new();
        let mut on_snapshot = |frame: &AuthenticatedSnapshotFrame<'_>| {
            snapshots_seen.push(frame.snapshot().clone());
            Ok(())
        };
        let mut on_liveness = |_frame: Value| true;
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        lane.bind(&manager);
        serve_controlling_lane(&mut lane, &manager, &sessions, &business, &mut sinks).await;
        assert_eq!(statuses, vec!["reconnecting".to_owned()]);
        assert!(sessions_seen.is_empty());
        assert!(tasks_seen.is_empty());
        assert!(journal_seen.is_empty());
        let frames = lane.resume_frames();
        assert_eq!(frames.len(), 1, "断线后必须发出恢复准备帧");
        assert_eq!(frames[0]["type"], "controller_resume_prepare");
        assert_eq!(frames[0]["sender_role"], "A");
        assert_eq!(frames[0]["original_deadline_ms"], "30000");
        assert!(
            !frames[0].to_string().contains("resume_token_b64\":\"[7, 7"),
            "恢复令牌必须以 base64 传输而不是调试形式"
        );
        assert_eq!(
            projection(&manager.status())["status"],
            "reconnecting",
            "断线后不能继续声称 connected"
        );
    }

    /// E2E-08：完整旅程——连接 → 能力快照 → 普通文本任务（A 构造并封帧、B 解封
    /// 校验）→ 权威 journal 正文与终态回流到 A → 显式断开 → 断开后旧 generation
    /// 的残留帧一律丢弃、业务通道不再可用。
    ///
    /// B 侧真实的接纳/执行由 DEV-13 的服务用例覆盖；本用例聚焦「一条旅程里各段
    /// 状态是否互相衔接」，因此 B 的回应帧按冻结形状直接产出。
    #[tokio::test]
    async fn full_journey_connect_snapshot_task_text_and_disconnect_drops_stale_frames() {
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        let PairingFixture {
            reservation,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture("relay-key-service-journey");
        let (a_session, b_session) = paired_session_keys(a_keys, b_keys, &reservation);
        let lease = lease_context(&reservation);
        manager
            .complete_connected(
                operation,
                ControllerLease {
                    context: lease.clone(),
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "liveness-epoch-a-0001".into(),
                    liveness_epoch_b: "liveness-epoch-b-0001".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7_u8; 32],
                    transport_key_fingerprint: [9_u8; 32],
                    negotiated_capabilities: vec![
                        "encrypted_frames_v1".into(),
                        "pairing_v1".into(),
                        "snapshot_journal_v1".into(),
                        "task_v1".into(),
                    ],
                },
            )
            .unwrap();
        let business = ControllingLaneState::default();
        business
            .install(HandshakeRole::A, &lease, a_session)
            .unwrap();
        let capabilities = vec![
            "encrypted_frames_v1".to_owned(),
            "pairing_v1".to_owned(),
            "snapshot_journal_v1".to_owned(),
            "task_v1".to_owned(),
        ];
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .map(|(projection, _)| projection)
            .unwrap();
        // 旅程第 2 段：B 的能力快照先落到 A 的会话状态（任务意图需要能力 revision）。
        sessions
            .apply_verified_snapshot_for_test(
                &session.session_ref,
                &json!({
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

        // A → B：真实任务意图（冻结 payload）封帧后必须能被 B 解封并校验。
        let intent = sessions
            .task_intent(&session.session_ref, "hello from A", &lease)
            .unwrap();
        let submit_frame = business
            .seal("task_submit", &intent.payload, &capabilities)
            .unwrap();
        let mut responder = EncryptedLane::new(HandshakeRole::B, &lease, b_session).unwrap();
        let (kind, payload) = responder.open(&submit_frame, &capabilities).unwrap();
        assert_eq!(kind, "task_submit");
        assert_eq!(payload["type"], "task_submit");
        assert_eq!(payload["input"]["text"], "hello from A");

        let refresh = sessions
            .capability_request(&session.session_ref, &lease)
            .unwrap();

        // B → A：能力快照、终态与正文（journal_event）。
        let snapshot_frame = responder
            .seal(
                "capability_snapshot",
                &json!({
                    "v": 1,
                    "type": "capability_snapshot",
                    "connection_id": lease.connection_id,
                    "controller_lease_id": lease.controller_lease_id,
                    "lease_epoch": lease.lease_epoch,
                    "relay_epoch": lease.relay_epoch,
                    "remote_control_generation": lease.remote_control_generation,
                    "request_id": refresh.request_id,
                    "session_id": session.session_id,
                    "controlled_session_id": "b-session-1",
                    "capability_revision": "4",
                    "expires_at_ms": "30000",
                    "modes": ["chat"],
                    "tools": [],
                    "skills": [],
                    "workspaces": [],
                }),
                &capabilities,
            )
            .unwrap();
        let status_frame = responder
            .seal(
                "task_status",
                &json!({
                    "v": 1,
                    "type": "task_status",
                    "connection_id": lease.connection_id,
                    "controller_lease_id": lease.controller_lease_id,
                    "lease_epoch": lease.lease_epoch,
                    "relay_epoch": lease.relay_epoch,
                    "remote_control_generation": lease.remote_control_generation,
                    "request_id": "request-status-1",
                    "task_id": intent.task_id.clone().unwrap_or_default(),
                    "session_id": session.session_id,
                    "task_status": "succeeded",
                    "result_digest": "0".repeat(64),
                }),
                &capabilities,
            )
            .unwrap();
        let text_event = JournalEvent {
            event_id: "event-0001".to_owned(),
            epoch: "epoch-1".to_owned(),
            sequence: 1,
            kind: "assistant_delta".to_owned(),
            session_id: session.session_id.clone(),
            task_id: intent.task_id.clone(),
            terminal_status: None,
            payload: json!({"text": "hello from B"}),
        };
        let raw = serde_json::to_vec(&text_event).unwrap();
        let text_frame = responder
            .seal(
                "journal_event",
                &json!({
                    "v": 1,
                    "type": "journal_event",
                    "connection_id": lease.connection_id,
                    "controller_lease_id": lease.controller_lease_id,
                    "lease_epoch": lease.lease_epoch,
                    "relay_epoch": lease.relay_epoch,
                    "remote_control_generation": lease.remote_control_generation,
                    "session_id": text_event.session_id,
                    "journal_epoch": text_event.epoch,
                    "event_sequence": text_event.sequence.to_string(),
                    "event_kind": text_event.kind,
                    "event_b64": base64::engine::general_purpose::STANDARD.encode(&raw),
                    "event_bytes": raw.len().to_string(),
                }),
                &capabilities,
            )
            .unwrap();

        let mut lane = TestLane::with_events(vec![
            ControllerLaneEvent::Frame(snapshot_frame.to_string()),
            ControllerLaneEvent::Frame(status_frame.to_string()),
            ControllerLaneEvent::Frame(text_frame.to_string()),
        ]);
        let mut session_projections = Vec::new();
        let mut task_events = Vec::new();
        let mut journal_seen: Vec<JournalEvent> = Vec::new();
        let mut on_status = |_: &ControllingStatus| {};
        let mut on_session = |projection: &ControllingSessionProjection| {
            session_projections.push(projection.capability_revision);
        };
        let mut on_task = |_: &LeaseContext, event: Value| task_events.push(event);
        let mut on_journal = |frame: &AuthenticatedJournalFrame<'_>| {
            journal_seen.push(frame.event().clone());
            Ok(())
        };
        let mut on_snapshot = |_: &AuthenticatedSnapshotFrame<'_>| Ok(());
        let mut on_liveness = |_: Value| true;
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        lane.bind(&manager);
        serve_controlling_lane(&mut lane, &manager, &sessions, &business, &mut sinks).await;
        assert_eq!(
            session_projections,
            vec![Some(4)],
            "能力快照必须落到会话状态"
        );
        assert_eq!(task_events.len(), 1);
        assert_eq!(task_events[0]["status"], "succeeded");
        assert_eq!(task_events[0]["sessionId"], session.session_id);
        assert_eq!(journal_seen.len(), 1);
        assert_eq!(journal_seen[0].payload["text"], "hello from B");

        // 显式断开：本地回到 disconnected，业务通道被清空。
        let status = manager.disconnect();
        business.clear();
        assert_eq!(projection(&status)["status"], "disconnected");
        assert!(!business.is_installed());

        // 断开后即使旧 generation 的残留帧到达，也不得产生任何投影。
        let mut foreign_lease = lease.clone();
        foreign_lease.remote_control_generation = "generation-other".into();
        let mut foreign = EncryptedLane::new(
            HandshakeRole::B,
            &foreign_lease,
            responder_session(&foreign_lease),
        )
        .unwrap();
        let stale = foreign
            .seal(
                "task_status",
                &json!({
                    "v": 1,
                    "type": "task_status",
                    "connection_id": foreign_lease.connection_id,
                    "controller_lease_id": foreign_lease.controller_lease_id,
                    "lease_epoch": foreign_lease.lease_epoch,
                    "relay_epoch": foreign_lease.relay_epoch,
                    "remote_control_generation": foreign_lease.remote_control_generation,
                    "request_id": "request-status-2",
                    "task_id": "task-0001",
                    "session_id": session.session_id,
                    "task_status": "succeeded",
                    "result_digest": "0".repeat(64),
                }),
                &capabilities,
            )
            .unwrap();
        let mut stale_lane =
            TestLane::with_events(vec![ControllerLaneEvent::Frame(stale.to_string())]);
        // Retain the original reader identity after explicit disconnect.
        stale_lane.reader_transport = lane.reader_transport.clone();
        let seen_before = task_events.len();
        let mut stale_status = |_: &ControllingStatus| {};
        let mut stale_session = |_: &ControllingSessionProjection| {};
        let mut stale_task = |_: &LeaseContext, event: Value| task_events.push(event);
        let mut stale_journal = |_: &AuthenticatedJournalFrame<'_>| Ok(());
        let mut stale_snapshot = |_: &AuthenticatedSnapshotFrame<'_>| Ok(());
        let mut stale_liveness = |_: Value| true;
        let mut stale_sinks = ControllingLaneSinks {
            on_status: &mut stale_status,
            on_session: &mut stale_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut stale_task,
            on_journal: &mut stale_journal,
            on_snapshot: &mut stale_snapshot,
            on_liveness: &mut stale_liveness,
        };
        serve_controlling_lane(
            &mut stale_lane,
            &manager,
            &sessions,
            &business,
            &mut stale_sinks,
        )
        .await;
        assert_eq!(
            task_events.len(),
            seen_before,
            "断开后的残留帧不得产生任务投影"
        );
        assert_eq!(journal_seen.len(), 1, "断开后的残留帧不得产生正文投影");
    }

    /// E2E-06：A 端按 Relay/B 的帧顺序完成整条恢复序列：prepare → provisional →
    /// (对端 confirm) → activation_pending → post_activation_liveness_confirmed →
    /// ready_ack → resumed，且业务闸门在 resumed 之前始终关闭。
    #[tokio::test]
    async fn resume_sequence_reopens_business_only_after_controller_resumed() {
        assert_resume_service_sequence(false).await;
        assert_resume_service_sequence(true).await;
    }

    async fn assert_resume_service_sequence(reader_authorized: bool) {
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
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "liveness-epoch-a-0001".into(),
                    liveness_epoch_b: "liveness-epoch-b-0001".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7_u8; 32],
                    transport_key_fingerprint: [9_u8; 32],
                    negotiated_capabilities: vec![
                        "encrypted_frames_v1".into(),
                        "pairing_v1".into(),
                        "resume_v1".into(),
                        "task_v1".into(),
                    ],
                },
            )
            .unwrap();
        let lease = manager.live_lease().unwrap();
        let (context, capabilities) = manager.live_runtime_context().unwrap();
        let reader_transport = super::super::test_support::resume_reader_transport(
            ControllerRole::ControllerA,
            context,
            capabilities,
        );
        manager.replace_transport(reader_transport.clone());
        let sessions = ControllingSessionManager::default();
        let business = ControllingLaneState::default();
        // 安装真实加密通道：恢复确认与 resumed 后的业务帧都走同一条通道。
        let PairingFixture {
            reservation,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture("relay-key-service-resume");
        let session = role_session_keys(
            HandshakeRole::A,
            a_keys,
            b_keys.public_key().to_bytes(),
            &reservation,
        );
        let lane_lease = LeaseContext {
            connection_id: lease.connection_id.clone(),
            controller_lease_id: lease.controller_lease_id.clone(),
            lease_epoch: lease.lease_epoch.clone(),
            relay_epoch: lease.relay_epoch.clone(),
            remote_control_generation: lease.remote_control_generation.clone(),
        };
        business
            .install(HandshakeRole::A, &lane_lease, session)
            .unwrap();

        let mut lane = TestLane::with_events(vec![
            ControllerLaneEvent::Disconnected(reader_transport.disconnect_context()),
            ControllerLaneEvent::Frame(
                json!({
                    "v": 1,
                    "type": "controller_resume_provisional",
                    "activation_id": "activation-0001",
                    "original_deadline_ms": "30000",
                    "connection_id": lane_lease.connection_id,
                    "controller_lease_id": lane_lease.controller_lease_id,
                    "lease_epoch": lane_lease.lease_epoch,
                    "relay_epoch": lane_lease.relay_epoch,
                    "remote_control_generation": lane_lease.remote_control_generation,
                })
                .to_string(),
            ),
            ControllerLaneEvent::Frame(
                json!({
                    "v": 1,
                    "type": "controller_resume_activation_pending",
                    "activation_id": "activation-0001",
                    "original_deadline_ms": "30000",
                    "connection_id": lane_lease.connection_id,
                    "controller_lease_id": lane_lease.controller_lease_id,
                    "lease_epoch": lane_lease.lease_epoch,
                    "relay_epoch": lane_lease.relay_epoch,
                    "remote_control_generation": lane_lease.remote_control_generation,
                })
                .to_string(),
            ),
            ControllerLaneEvent::Frame(
                json!({
                    "v": 1,
                    "type": "controller_post_activation_liveness_confirmed",
                    "activation_id": "activation-0001",
                    "liveness_epoch_b": "live-b-2",
                    "heartbeat_sequence_b": "1",
                    "connection_id": lane_lease.connection_id,
                    "controller_lease_id": lane_lease.controller_lease_id,
                    "lease_epoch": lane_lease.lease_epoch,
                    "relay_epoch": lane_lease.relay_epoch,
                    "remote_control_generation": lane_lease.remote_control_generation,
                })
                .to_string(),
            ),
            ControllerLaneEvent::Frame(
                json!({
                    "v": 1,
                    "type": "controller_resumed",
                    "request_id": "request-resume-final-0001",
                    "activation_id": "activation-0001",
                    "liveness_epoch_a": "live-a-2",
                    "liveness_epoch_b": "live-b-2",
                    "resume_deadline_ms": "1700000030000",
                    "new_resume_token_b64": base64::engine::general_purpose::STANDARD.encode([3_u8; 32]),
                    "connection_id": lane_lease.connection_id,
                    "controller_lease_id": lane_lease.controller_lease_id,
                    "lease_epoch": lane_lease.lease_epoch,
                    "relay_epoch": lane_lease.relay_epoch,
                    "remote_control_generation": lane_lease.remote_control_generation,
                })
                .to_string(),
            ),
        ]);
        lane.reader_transport = Some(reader_transport);
        lane.authorize_resume_final = reader_authorized;
        let mut statuses: Vec<String> = Vec::new();
        let mut on_status = |status: &ControllingStatus| {
            statuses.push(projection(status)["status"].as_str().unwrap().to_owned());
        };
        let mut on_session = |_: &ControllingSessionProjection| {};
        let mut on_task = |_: &LeaseContext, _: Value| {};
        let mut on_journal = |_: &AuthenticatedJournalFrame<'_>| Ok(());
        let mut on_snapshot = |_: &AuthenticatedSnapshotFrame<'_>| Ok(());
        let mut on_liveness = |_: Value| true;
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        lane.bind(&manager);
        serve_controlling_lane(&mut lane, &manager, &sessions, &business, &mut sinks).await;

        let frames = lane.resume_frames();
        let kinds: Vec<&str> = frames
            .iter()
            .filter_map(|frame| frame.get("type").and_then(Value::as_str))
            .collect();
        assert_eq!(
            kinds,
            vec![
                "controller_resume_prepare",
                "encrypted_resume_confirm",
                "controller_resume_ready_ack",
            ],
            "恢复必须按冻结顺序发出 prepare/confirm/ready_ack"
        );
        assert_eq!(frames[2]["liveness_epoch_b"], "live-b-2");
        if !reader_authorized {
            assert_eq!(
                statuses,
                vec!["reconnecting".to_owned(), "error".to_owned()]
            );
            assert!(manager.live_lease().is_err());
            assert!(!business.is_installed() || business.is_suspended());
            return;
        }
        assert_eq!(
            statuses,
            vec!["reconnecting".to_owned(), "connected".to_owned()],
            "恢复成功必须发布完整成功投影"
        );
        assert_eq!(
            projection(&manager.status())["status"],
            "connected",
            "controller_resumed 之后必须回到 connected"
        );
        assert_eq!(
            manager.live_liveness_epoch().as_deref(),
            Some("live-a-2"),
            "恢复必须采用 Relay 下发的 A 端 liveness epoch"
        );
        assert!(!business.is_suspended(), "恢复后业务通道必须重新打开");
        let now_a = lane_monotonic_millis();
        manager.mark_transport_disconnected(now_a, Some(now_a + 30_000));
        let second = manager
            .begin_resume(manager.current_operation().unwrap(), now_a)
            .unwrap();
        assert_eq!(second.relay_deadline_identity_ms, 1_700_000_030_000);
        assert!(
            manager.send_resume_control(&json!({})).is_err(),
            "非恢复族的帧不得走恢复敏感队列"
        );
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
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    liveness_epoch_b: "live-b-1".into(),
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
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .map(|(projection, _)| projection)
            .unwrap();
        let capability_request = sessions
            .capability_request(&session.session_ref, &lease)
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
            "request_id": capability_request.request_id,
            "session_id": session.session_id,
            "controlled_session_id": "b-session-1",
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
            ..TestLane::default()
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
        let mut on_task = |_: &LeaseContext, event: Value| task_events.push(event);
        let mut journal_events: Vec<JournalEvent> = Vec::new();
        let mut on_journal = |frame: &AuthenticatedJournalFrame<'_>| {
            journal_events.push(frame.event().clone());
            Ok(())
        };
        let mut snapshots_seen: Vec<JournalSnapshot> = Vec::new();
        let mut on_snapshot = |frame: &AuthenticatedSnapshotFrame<'_>| {
            snapshots_seen.push(frame.snapshot().clone());
            Ok(())
        };
        let mut on_liveness = |_frame: Value| true;
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        lane.bind(&manager);
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

    /// E2E-06：Relay 顶层存活挑战必须经租约校验并由 A 返回加密证明。
    #[tokio::test]
    async fn top_level_liveness_challenge_produces_direct_encrypted_proof() {
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
            "role_liveness_v1".to_owned(),
        ];

        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation,
                ControllerLease {
                    context: lease.clone(),
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    liveness_epoch_b: "live-b-1".into(),
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

        let challenge = super::super::liveness::build_liveness_challenge(
            &lease,
            "request-liveness-0001",
            "challenge-0001",
            "AAAAAAAAAAAAAAAAAAAAAA==",
        )
        .unwrap();
        let mut stale = challenge.clone();
        stale["remote_control_generation"] = json!("generation-stale");
        let mut lane = TestLane::with_events(vec![
            ControllerLaneEvent::Frame(stale.to_string()),
            ControllerLaneEvent::Frame(challenge.to_string()),
            ControllerLaneEvent::Frame(challenge.to_string()),
            ControllerLaneEvent::Frame(challenge.to_string()),
        ]);
        let sessions = ControllingSessionManager::default();
        let mut responses = Vec::new();
        let mut on_status = |_: &ControllingStatus| {};
        let mut on_session = |_: &ControllingSessionProjection| {};
        let mut on_task = |_: &LeaseContext, _: Value| {};
        let mut on_journal = |_: &AuthenticatedJournalFrame<'_>| Ok(());
        let mut on_snapshot = |_: &AuthenticatedSnapshotFrame<'_>| Ok(());
        let mut on_liveness = |frame: Value| {
            responses.push(frame);
            responses.len() > 1
        };
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };

        lane.bind(&manager);
        serve_controlling_lane(&mut lane, &manager, &sessions, &business, &mut sinks).await;
        drop(sinks);
        assert_eq!(
            responses.len(),
            2,
            "failed admission is retryable; admitted replay and old generation are dropped"
        );
        assert_eq!(responses[0]["type"], "encrypted_controller_liveness_a");

        let responder = EncryptedLane::new(HandshakeRole::B, &lease, b_session).unwrap();
        let proof = responder.open_liveness(&responses[1]).unwrap();
        assert_eq!(proof["type"], "controller_liveness_a");
        assert_eq!(proof["challenge_id"], "challenge-0001");
        assert_eq!(proof["heartbeat_sequence_a"], "2");
        assert_eq!(proof["liveness_epoch_a"], "live-a-1");
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

    #[test]
    fn answered_challenges_are_bounded_and_recent_replays_are_never_evicted() {
        let mut state = LivenessExpectations::default();
        for index in 0..MAX_ANSWERED_LIVENESS_CHALLENGES {
            let id = format!("challenge-{index}");
            assert!(state.can_answer(&id, 1_000));
            state.record_answered(&id, 1_000);
        }
        assert!(!state.can_answer("overflow", 31_000));
        assert!(!state.can_answer("challenge-0", 31_000));
        assert_eq!(state.answered.len(), MAX_ANSWERED_LIVENESS_CHALLENGES);
        assert!(state.can_answer("overflow", 31_001));
        assert!(state.answered.is_empty());
        // Looking up a retry before successful enqueue does not consume it.
        assert!(state.can_answer("retry", 31_001));
        assert!(state.can_answer("retry", 31_001));
    }

    /// E2E-04 出口：同一进程内完成「A 发加密任务 → B 解密派发 → B 加密回传 →
    /// A 解密投影」的普通文本任务闭环（B 的接纳/执行由 DEV-13 的服务用例覆盖，
    /// 这里用端口桩返回冻结形状的 task_status）。
    #[tokio::test]
    async fn dual_end_task_round_trip_lands_in_a_side_state() {
        use crate::features::remote_control::controller::relay_runtime::{
            ControlledBusinessPort, dispatch_decrypted_payload,
        };

        struct TerminalPort(bool);

        #[async_trait::async_trait]
        impl ControlledBusinessPort for TerminalPort {
            async fn remote_action(
                &self,
                _lease: &LeaseContext,
                _session_ref: &str,
                _operation: &str,
                _body: &Value,
            ) -> Result<Value, String> {
                Err("remote_action_unavailable".to_owned())
            }
            async fn capability_snapshot(
                &self,
                _lease: &LeaseContext,
                request_id: &str,
                _session_id: &str,
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
                if self.0 {
                    return Err("task_permission_mode_required".to_owned());
                }
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

            async fn replay_journal(
                &self,
                _lease: &LeaseContext,
                _session_ref: &str,
                _journal_epoch: &str,
                _after_sequence: u64,
            ) -> Result<Vec<(String, Value)>, String> {
                Ok(Vec::new())
            }

            async fn session_snapshot(
                &self,
                _lease: &LeaseContext,
                _session_ref: &str,
                _request_id: &str,
            ) -> Result<Value, String> {
                Ok(serde_json::json!({"v": 1, "type": "session_snapshot"}))
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
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    liveness_epoch_b: "live-b-1".into(),
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
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .map(|(projection, _)| projection)
            .unwrap();
        sessions
            .apply_verified_snapshot_for_test(
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
        let responses = dispatch_decrypted_payload(&TerminalPort(false), &lease, &kind, &payload)
            .await
            .unwrap();
        assert_eq!(responses.len(), 1);
        let (response_kind, response) = responses.into_iter().next().unwrap();
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

        // A second task is refused before admission. Its encrypted failure
        // needs no journal event to reach A with both correlation IDs intact.
        let mut denied = intent.payload.clone();
        denied["task_id"] = serde_json::json!("task-denied-round-trip");
        denied["client_request_id"] = serde_json::json!("client-denied-round-trip");
        denied["request_id"] = serde_json::json!("request-denied-round-trip");
        let denied_frame = business
            .seal("task_submit", &denied, &capabilities)
            .unwrap();
        let (kind, payload) = b_lane.open(&denied_frame, &capabilities).unwrap();
        let responses = dispatch_decrypted_payload(&TerminalPort(true), &lease, &kind, &payload)
            .await
            .unwrap();
        assert_eq!(responses.len(), 1);
        let rejection_frame = b_lane
            .seal(responses[0].0, &responses[0].1, &capabilities)
            .unwrap();

        // A：保活任务解密并投影到任务状态域。
        let mut lane = TestLane {
            events: VecDeque::from(vec![
                ControllerLaneEvent::Frame(response_frame.to_string()),
                ControllerLaneEvent::Frame(journal_frame.to_string()),
                ControllerLaneEvent::Frame(rejection_frame.to_string()),
            ]),
            ..TestLane::default()
        };
        let mut task_events = Vec::new();
        let mut session_projections = Vec::new();
        let mut on_status = |_: &ControllingStatus| {};
        let mut on_session = |projection: &ControllingSessionProjection| {
            session_projections.push(projection.session_id.clone());
        };
        let mut on_task = |_: &LeaseContext, event: Value| task_events.push(event);
        let mut journal_events: Vec<JournalEvent> = Vec::new();
        let mut on_journal = |frame: &AuthenticatedJournalFrame<'_>| {
            journal_events.push(frame.event().clone());
            Ok(())
        };
        let mut snapshots_seen: Vec<JournalSnapshot> = Vec::new();
        let mut on_snapshot = |frame: &AuthenticatedSnapshotFrame<'_>| {
            snapshots_seen.push(frame.snapshot().clone());
            Ok(())
        };
        let mut on_liveness = |_frame: Value| true;
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        lane.bind(&manager);
        serve_controlling_lane(&mut lane, &manager, &sessions, &business, &mut sinks).await;

        assert!(session_projections.is_empty());
        assert_eq!(task_events.len(), 2);
        assert_eq!(task_events[1]["status"], "failed");
        assert_eq!(task_events[1]["taskId"], "task-denied-round-trip");
        assert_eq!(
            task_events[1]["clientRequestId"],
            "client-denied-round-trip"
        );
        assert_eq!(
            task_events[1]["rejectionCode"],
            "task_permission_mode_required"
        );
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

    /// E2E-05 缺口恢复：权威快照经加密通道到达后原子替换 A 的本地副本；摘要或
    /// 基准不匹配的帧一律丢弃。
    #[tokio::test]
    async fn session_snapshot_frames_replace_the_local_copy() {
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
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation,
                ControllerLease {
                    context: lease.clone(),
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    liveness_epoch_b: "live-b-1".into(),
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
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .map(|(projection, _)| projection)
            .unwrap();

        let snapshot = JournalSnapshot {
            epoch: "epoch-9".to_owned(),
            session_id: session.session_id.clone(),
            sequence: 7,
            digest: String::new(),
            events: vec![JournalEvent {
                event_id: "event-1".to_owned(),
                epoch: "epoch-9".to_owned(),
                sequence: 7,
                kind: "assistant_delta".to_owned(),
                session_id: session.session_id.clone(),
                task_id: None,
                terminal_status: None,
                payload: serde_json::json!({"text": "hello"}),
            }],
        };
        let snapshot = JournalSnapshot {
            digest: snapshot.compute_digest(),
            ..snapshot
        };
        let raw = serde_json::to_vec(&snapshot).unwrap();
        let wire = |digest: &str, base: u64| {
            serde_json::json!({
                "v": 1,
                "type": "session_snapshot",
                "connection_id": lease.connection_id,
                "controller_lease_id": lease.controller_lease_id,
                "lease_epoch": lease.lease_epoch,
                "relay_epoch": lease.relay_epoch,
                "remote_control_generation": lease.remote_control_generation,
                "request_id": "request-snapshot-1",
                "session_id": session.session_id,
                "journal_epoch": snapshot.epoch,
                "base_sequence": base.to_string(),
                "snapshot_b64": base64::engine::general_purpose::STANDARD.encode(&raw),
                "snapshot_bytes": raw.len().to_string(),
                "sha256": digest,
            })
        };
        let mut responder = EncryptedLane::new(HandshakeRole::B, &lease, b_session).unwrap();
        let good = responder
            .seal(
                "session_snapshot",
                &wire(&snapshot.digest, 7),
                &capabilities,
            )
            .unwrap();
        let bad_digest = responder
            .seal("session_snapshot", &wire(&"b".repeat(64), 7), &capabilities)
            .unwrap();
        let bad_base = responder
            .seal(
                "session_snapshot",
                &wire(&snapshot.digest, 6),
                &capabilities,
            )
            .unwrap();

        let mut lane = TestLane {
            events: VecDeque::from(vec![
                ControllerLaneEvent::Frame(bad_digest.to_string()),
                ControllerLaneEvent::Frame(bad_base.to_string()),
                ControllerLaneEvent::Frame(good.to_string()),
            ]),
            ..TestLane::default()
        };
        let mut snapshots_seen: Vec<JournalSnapshot> = Vec::new();
        let mut on_status = |_: &ControllingStatus| {};
        let mut on_session = |_: &ControllingSessionProjection| {};
        let mut on_task = |_: &LeaseContext, _: Value| {};
        let mut on_journal = |_: &AuthenticatedJournalFrame<'_>| Ok(());
        let mut on_snapshot = |frame: &AuthenticatedSnapshotFrame<'_>| {
            snapshots_seen.push(frame.snapshot().clone());
            Ok(())
        };
        let mut on_liveness = |_: Value| true;
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        lane.bind(&manager);
        serve_controlling_lane(&mut lane, &manager, &sessions, &business, &mut sinks).await;
        assert_eq!(snapshots_seen.len(), 1, "只有摘要与基准都正确的快照被接受");
        assert_eq!(snapshots_seen[0].epoch, "epoch-9");
        assert_eq!(snapshots_seen[0].sequence, 7);
    }

    /// E2E-05 缺口恢复：连续/首个事件落库，缺中间事件请求回放（同会话只保留一个
    /// 在途请求），重复事件忽略，换 epoch 从 0 号位重新回放。
    #[test]
    fn journal_gaps_are_detected_and_replayed_once() {
        let event = |epoch: &str, sequence: u64| JournalEvent {
            event_id: format!("event-{epoch}-{sequence}"),
            epoch: epoch.to_owned(),
            sequence,
            kind: "assistant_delta".to_owned(),
            session_id: "session-a".to_owned(),
            task_id: None,
            terminal_status: None,
            payload: serde_json::json!({"text": "x"}),
        };
        assert_eq!(
            journal_admission(None, &event("epoch-1", 1)),
            JournalAdmission::Apply,
            "首个事件直接落库"
        );
        assert_eq!(
            journal_admission(Some(("epoch-1", 1)), &event("epoch-1", 2)),
            JournalAdmission::Apply
        );
        assert_eq!(
            journal_admission(Some(("epoch-1", 2)), &event("epoch-1", 2)),
            JournalAdmission::Duplicate
        );
        assert_eq!(
            journal_admission(Some(("epoch-1", 2)), &event("epoch-1", 5)),
            JournalAdmission::RequestReplay {
                epoch: "epoch-1".into(),
                after: 2
            }
        );
        assert_eq!(
            journal_admission(Some(("epoch-1", 5)), &event("epoch-2", 1)),
            JournalAdmission::RequestReplay {
                epoch: "epoch-2".into(),
                after: 0
            }
        );

        // 期望值推进与在途请求去重。
        let mut expectations = JournalExpectations::default();
        let now = tokio::time::Instant::now();
        assert!(expectations.expected("session-a").is_none());
        expectations.mark_applied(&event("epoch-1", 1));
        assert_eq!(expectations.expected("session-a"), Some(("epoch-1", 1)));
        let pending = expectations
            .begin_replay("session-a", "epoch-1", 1, 3, "generation-1", now)
            .unwrap();
        assert_eq!(pending.epoch, "epoch-1");
        assert_eq!(pending.after, 1);
        assert!(
            expectations
                .begin_replay("session-a", "epoch-1", 1, 3, "generation-1", now)
                .is_none()
        );
        // A partial response must not clear a request for sequence 3.
        expectations.mark_applied(&event("epoch-1", 2));
        assert_eq!(expectations.expected("session-a"), Some(("epoch-1", 2)));
        assert!(
            expectations
                .begin_replay("session-a", "epoch-1", 2, 3, "generation-1", now)
                .is_none()
        );
        expectations.mark_applied(&event("epoch-1", 3));
        assert!(
            expectations
                .begin_replay("session-a", "epoch-1", 3, 5, "generation-1", now)
                .is_some()
        );
    }

    /// E2E-05 缺口恢复：回放请求必须是冻结形状且通过运行时校验（角色 A +
    /// snapshot_journal_v1）。
    #[test]
    fn journal_replay_request_matches_the_frozen_contract() {
        use crate::features::remote_control::controller::protocol::{
            RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
        };

        let PairingFixture { reservation, .. } = pairing_fixture(RELAY_KEY_ID);
        let lease = lease_context(&reservation);
        let payload =
            build_journal_replay_request(&lease, "session-a-ref", "epoch-1", 7, "request-replay-1")
                .unwrap();
        validate_controller_runtime_payload_json(
            &payload.to_string(),
            SenderRole::ControllerA,
            Some(&RuntimeValidationContext {
                lease: Some(lease.clone()),
                negotiated_capabilities: vec![
                    "encrypted_frames_v1".to_owned(),
                    "pairing_v1".to_owned(),
                    "snapshot_journal_v1".to_owned(),
                    "task_v1".to_owned(),
                ],
            }),
        )
        .expect("回放请求必须满足冻结契约");
        assert_eq!(payload["after_sequence"], "7");
        assert_eq!(payload["journal_epoch"], "epoch-1");
        assert_eq!(payload["session_id"], "session-a-ref");
        assert!(build_journal_replay_request(&lease, "", "epoch-1", 0, "request-1").is_err());
    }

    /// E2E-06：保活任务按间隔发送 A 端心跳帧（序号单调、epoch 来自租约），且
    /// 只在 connected 时发送；未连接时没有帧可发。
    #[tokio::test]
    async fn keeper_emits_monotonic_liveness_heartbeats() {
        struct PendingLane(ControllerTransportSender);

        #[async_trait::async_trait]
        impl ControllingLane for PendingLane {
            fn transport_sender(&self) -> Option<ControllerTransportSender> {
                Some(self.0.clone())
            }

            async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
                std::future::pending().await
            }
        }

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
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    liveness_epoch_b: "live-b-1".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7_u8; 32],
                    transport_key_fingerprint: [9_u8; 32],
                    negotiated_capabilities: vec![
                        "encrypted_frames_v1".into(),
                        "pairing_v1".into(),
                        "role_liveness_v1".into(),
                        "task_v1".into(),
                    ],
                },
            )
            .unwrap();
        let sessions = ControllingSessionManager::default();
        let business = ControllingLaneState::default();
        let mut lane = PendingLane(registered_lane_sender(&manager));
        let mut frames = Vec::new();
        let mut on_status = |_: &ControllingStatus| {};
        let mut on_session = |_: &ControllingSessionProjection| {};
        let mut on_task = |_: &LeaseContext, _: Value| {};
        let mut on_journal = |_: &AuthenticatedJournalFrame<'_>| Ok(());
        let mut on_snapshot = |_: &AuthenticatedSnapshotFrame<'_>| Ok(());
        let mut on_liveness = |frame: Value| {
            frames.push(frame);
            true
        };
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        let outcome = tokio::time::timeout(
            Duration::from_millis(150),
            serve_controlling_lane_with_interval(
                &mut lane,
                &manager,
                &sessions,
                &business,
                &mut sinks,
                Duration::from_millis(20),
            ),
        )
        .await;
        assert!(outcome.is_err(), "长连接保活任务不应自行结束");
        assert!(
            frames
                .iter()
                .any(|frame| frame["type"] == "controller_liveness_attestation_request_b")
        );
        let frames: Vec<_> = frames
            .iter()
            .filter(|frame| frame["type"] == "controller_liveness")
            .collect();
        assert!(frames.len() >= 2, "应按间隔持续发送心跳");
        assert_eq!(frames[0]["type"], "controller_liveness");
        assert_eq!(frames[0]["sender_role"], "A");
        assert_eq!(frames[0]["liveness_epoch"], "live-a-1");
        assert_eq!(frames[0]["heartbeat_sequence"], "1");
        assert_eq!(frames[1]["heartbeat_sequence"], "2");
        assert_eq!(frames[0]["connection_id"], "connection-0001");
        assert_eq!(frames[0]["lease_epoch"], "1");
    }

    /// Real Node Router, LeaseManager receipt gate and signed liveness hooks
    /// feed the actual Rust A service over local IPC. Storage/enrollment,
    /// endpoint Finished crypto and socket transport are separate test scopes;
    /// no fabricated Relay or fabricated signed response is used here.
    #[tokio::test(start_paused = true)]
    async fn real_node_router_b_proof_keeps_a_alive_then_b_loss_expires_despite_a_tx() {
        use std::io::{BufRead as _, Write as _};
        use std::process::{Command, Stdio};

        struct NodeRpc {
            child: std::process::Child,
            input: std::process::ChildStdin,
            output: std::io::BufReader<std::process::ChildStdout>,
        }
        impl NodeRpc {
            fn read(&mut self) -> Value {
                let mut line = String::new();
                assert!(
                    self.output.read_line(&mut line).unwrap() > 0,
                    "Node fixture must reply within its 15s watchdog"
                );
                assert!(line.len() < 64 * 1024);
                serde_json::from_str(&line).unwrap()
            }
            fn send(&mut self, command: Value) -> Value {
                writeln!(self.input, "{command}").unwrap();
                self.input.flush().unwrap();
                self.read()
            }
        }
        impl Drop for NodeRpc {
            fn drop(&mut self) {
                let _ = self.child.kill();
                let _ = self.child.wait();
            }
        }
        struct IpcLane(
            mpsc::UnboundedReceiver<ControllerLaneEvent>,
            ControllerTransportSender,
        );
        #[async_trait::async_trait]
        impl ControllingLane for IpcLane {
            fn transport_sender(&self) -> Option<ControllerTransportSender> {
                Some(self.1.clone())
            }

            async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
                self.0.recv().await
            }
        }
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
        let b_manager = super::super::controlled::ControlledDeviceManager::default();
        let b_operation = b_manager.start(None, Vec::new()).unwrap();
        let mut child = Command::new("node")
            .env("PINVOU_TEST_B_GENERATION", b_operation.generation())
            .arg(root.join("remote-control-relay/test/fixtures/peer-liveness-router.js"))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .expect("local Node runtime is required for the deterministic Relay contract");
        let mut rpc = NodeRpc {
            input: child.stdin.take().unwrap(),
            output: std::io::BufReader::new(child.stdout.take().unwrap()),
            child,
        };
        let startup = rpc.read();
        let trust = AuthenticatedRelayTrustConfig::from_pinned_keys([(
            startup["key_id"].as_str().unwrap().to_owned(),
            startup["public_key_b64"].as_str().unwrap().to_owned(),
        )])
        .unwrap();
        let reserved = &startup["reserved"];
        let parsed =
            super::super::protocol::parse_controller_pairing_transcript(&reserved.to_string())
                .unwrap();
        let install_context =
            super::super::crypto::authenticate_relay_transcript(&trust, parsed).unwrap();
        let joined = super::super::controlling_handshake::validate_joined_lease(
            &startup["joined"],
            &install_context,
        )
        .unwrap();
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("2345678").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation,
                ControllerLease {
                    context: LeaseContext {
                        connection_id: joined.connection_id,
                        controller_lease_id: joined.controller_lease_id,
                        lease_epoch: joined.lease_epoch.to_string(),
                        relay_epoch: joined.relay_epoch,
                        remote_control_generation: joined.remote_control_generation,
                    },
                    peer_endpoint_id: "endpoint-ipc".into(),
                    reservation_id: reserved["reservation_id"].as_str().unwrap().into(),
                    device_display_name: "test-device".into(),
                    liveness_epoch_a: joined.liveness_epoch_a,
                    liveness_epoch_b: joined.liveness_epoch_b,
                    resume_deadline_b: joined.resume_deadline_ms,
                    resume_token: joined.resume_token,
                    transport_key_fingerprint: [8; 32],
                    negotiated_capabilities: serde_json::from_value(
                        startup["capabilities"].clone(),
                    )
                    .unwrap(),
                },
            )
            .unwrap();
        let b_context = manager.live_lease().unwrap();
        let b_epoch = startup["joined"]["liveness_epoch_b"].as_str().unwrap();
        super::super::controlled_heartbeat::test_support::connect(
            &b_manager,
            &b_operation,
            b_context.clone(),
            startup["joined"]["liveness_epoch_a"].as_str().unwrap(),
            serde_json::from_value(startup["capabilities"].clone()).unwrap(),
        );
        let b_port = super::super::controlled_heartbeat::test_support::CapturePort::default();
        let mut b_heartbeat = super::super::controlled_heartbeat::ControlledHeartbeat::default();
        b_heartbeat.install(b_context, b_epoch).unwrap();
        let origin = tokio::time::Instant::now();
        let clock = || u64::try_from(origin.elapsed().as_millis()).unwrap();
        let (tx, rx) = mpsc::unbounded_channel();
        let mut lane = IpcLane(rx, registered_lane_sender(&manager));
        let mut a_heartbeats_after_b_loss = 0;
        let mut proofs = 0;
        let mut proof_rejections = 0;
        let mut on_liveness = |frame: Value| {
            let now = clock();
            if now >= 35_000 && frame["type"] == "controller_liveness" {
                a_heartbeats_after_b_loss += 1;
            }
            let b_frame = if now < 35_000 {
                b_heartbeat
                    .pump(&b_manager, &b_operation, false, &b_port)
                    .unwrap();
                b_port.frames.lock().pop()
            } else {
                None
            };
            let reply = rpc.send(json!({"frame": frame, "clock_ms": now, "b_frame": b_frame}));
            for frame in reply["deliveries"].as_array().unwrap() {
                if frame["type"] == "controller_liveness_attestation_b" {
                    proofs += 1;
                }
                if frame["type"] == "error" {
                    proof_rejections += 1;
                }
                tx.send(ControllerLaneEvent::Frame(frame.to_string()))
                    .unwrap();
            }
            // Even Relay rejection was successfully transmitted by A. This
            // must not be mistaken for authenticated B evidence.
            true
        };
        let mut status = Vec::new();
        let mut on_status = |value: &ControllingStatus| status.push(value.kind());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut |_| {},
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut |_, _| {},
            on_journal: &mut |_| Ok(()),
            on_snapshot: &mut |_| Ok(()),
            on_liveness: &mut on_liveness,
        };
        let sessions = ControllingSessionManager::default();
        let current_lease = manager.live_lease().unwrap();
        let caps: Vec<String> = serde_json::from_value(startup["capabilities"].clone()).unwrap();
        sessions
            .open_with_outcome(
                Some("capability-unrelated"),
                "Pending",
                "chat",
                &current_lease,
                &caps,
            )
            .unwrap();
        sessions
            .capability_request("capability-unrelated", &current_lease)
            .unwrap();
        let business = ControllingLaneState::default();
        tokio::time::timeout(
            Duration::from_secs(70),
            serve_controlling_lane_with_evidence_context(
                &mut lane,
                &manager,
                &sessions,
                &business,
                &mut sinks,
                Duration::from_secs(3),
                PeerEvidenceContext {
                    trust: Some(&trust),
                    monotonic: &clock,
                    operation: manager.current_operation().unwrap(),
                },
            ),
        )
        .await
        .expect("B loss must end A inside the finite authenticated cutoff");
        assert!(
            (35_000..65_000).contains(&clock()),
            "healthy B must survive the initial 30s, and lost B must not renew forever"
        );
        assert!(proofs >= 10);
        assert!(
            proof_rejections >= 5,
            "the real Router remains alive after stale B evidence"
        );
        assert!(
            sessions
                .state("capability-unrelated", &current_lease)
                .unwrap()
                .capability_error
                .is_none(),
            "peer proof rejection must not fail a capability request"
        );
        assert!(
            a_heartbeats_after_b_loss >= 5,
            "A TX remains successful after B silence"
        );
        assert_eq!(manager.status().kind(), ControllingStatusKind::Error);
        assert!(manager.live_lease().is_err());
        assert_eq!(status.last(), Some(&ControllingStatusKind::Error));
    }

    #[tokio::test(start_paused = true)]
    async fn keeper_replaced_after_heartbeat_snapshot_never_sends_peer_request_on_new_owner() {
        struct PendingLane(ControllerTransportSender);
        #[async_trait::async_trait]
        impl ControllingLane for PendingLane {
            fn transport_sender(&self) -> Option<ControllerTransportSender> {
                Some(self.0.clone())
            }

            async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
                std::future::pending().await
            }
        }
        fn connect(manager: &ControllingDeviceManager, generation: &str) -> ControllerOperation {
            let operation = manager.connect("2345678").unwrap();
            manager.complete_reserved(operation.clone()).unwrap();
            manager
                .complete_connected(
                    operation.clone(),
                    ControllerLease {
                        context: LeaseContext {
                            connection_id: "connection-owner".into(),
                            controller_lease_id: "lease-owner".into(),
                            lease_epoch: "1".into(),
                            relay_epoch: "relay-owner".into(),
                            remote_control_generation: generation.into(),
                        },
                        peer_endpoint_id: "endpoint-owner".into(),
                        reservation_id: "reservation-owner".into(),
                        device_display_name: "Owner".into(),
                        liveness_epoch_a: "live-a-owner".into(),
                        liveness_epoch_b: "live-b-owner".into(),
                        resume_deadline_b: 30_000,
                        resume_token: [1; 32],
                        transport_key_fingerprint: [2; 32],
                        negotiated_capabilities: vec!["role_liveness_v1".into()],
                    },
                )
                .unwrap();
            operation
        }
        let manager = ControllingDeviceManager::default();
        let old = connect(&manager, "generation-old");
        let mut lane = PendingLane(registered_lane_sender(&manager));
        let mut calls = 0;
        let mut replacement = None;
        let mut on_liveness = |frame: Value| {
            calls += 1;
            assert_eq!(frame["type"], "controller_liveness");
            assert_eq!(frame["remote_control_generation"], "generation-old");
            manager.disconnect();
            replacement = Some(connect(&manager, "generation-new"));
            registered_lane_sender(&manager);
            true
        };
        let mut statuses = Vec::new();
        let mut on_status = |status: &ControllingStatus| statuses.push(status.kind());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut |_| {},
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut |_, _| {},
            on_journal: &mut |_| Ok(()),
            on_snapshot: &mut |_| Ok(()),
            on_liveness: &mut on_liveness,
        };
        let origin = tokio::time::Instant::now();
        let clock = || u64::try_from(origin.elapsed().as_millis()).unwrap();
        serve_controlling_lane_with_evidence_context(
            &mut lane,
            &manager,
            &ControllingSessionManager::default(),
            &ControllingLaneState::default(),
            &mut sinks,
            Duration::from_secs(3),
            PeerEvidenceContext {
                trust: None,
                monotonic: &clock,
                operation: old.clone(),
            },
        )
        .await;
        drop(sinks);
        assert_eq!(
            calls, 1,
            "old pending proof request must not reach the new owner"
        );
        assert!(statuses.is_empty(), "old timeout cannot fail the new owner");
        assert_eq!(manager.current_operation(), replacement);
        assert_eq!(manager.status().kind(), ControllingStatusKind::Connected);
        assert_eq!(
            manager.live_lease().unwrap().remote_control_generation,
            "generation-new"
        );
        // Production spawn passes its original intent explicitly. A delayed
        // keeper starting only after replacement must exit without any TX.
        let mut no_tx = |_: Value| -> bool { panic!("stale keeper cannot send") };
        let mut sinks = ControllingLaneSinks {
            on_status: &mut |_| panic!("stale keeper cannot project"),
            on_session: &mut |_| {},
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut |_, _| {},
            on_journal: &mut |_| Ok(()),
            on_snapshot: &mut |_| Ok(()),
            on_liveness: &mut no_tx,
        };
        serve_controlling_lane_with_evidence_context(
            &mut lane,
            &manager,
            &ControllingSessionManager::default(),
            &ControllingLaneState::default(),
            &mut sinks,
            Duration::from_secs(3),
            PeerEvidenceContext {
                trust: None,
                monotonic: &clock,
                operation: old,
            },
        )
        .await;
        assert_eq!(manager.status().kind(), ControllingStatusKind::Connected);
    }
}

#[cfg(test)]
mod dev19_wire_tests {
    use super::*;
    use crate::features::remote_control::controller::journal::{JournalKind, RemoteJournal};
    use serde_json::json;

    #[test]
    fn wire_event_requires_exact_kind_identity_and_sanitized_body() {
        let journal = RemoteJournal::new("epoch-1");
        let event = journal
            .append(
                "event-1",
                JournalKind::UserMessage,
                "remote-a",
                None,
                json!({"text":"safe"}),
            )
            .unwrap();
        let encode = |event: &JournalEvent, wire_kind: &str| {
            let raw = serde_json::to_vec(event).unwrap();
            json!({
                "event_b64": base64::engine::general_purpose::STANDARD.encode(&raw),
                "event_bytes": raw.len().to_string(), "journal_epoch": event.epoch,
                "event_sequence": event.sequence.to_string(), "session_id": event.session_id,
                "event_kind": wire_kind,
            })
        };
        assert_eq!(
            journal_event_from_wire(&encode(&event, "user_message")),
            Some(event.clone())
        );
        assert!(journal_event_from_wire(&encode(&event, "terminal")).is_none());
        let mut tampered = event.clone();
        tampered.event_id = "bad event id".into();
        assert!(journal_event_from_wire(&encode(&tampered, "user_message")).is_none());
        let mut tampered = event.clone();
        tampered.payload = json!({"text":"sk-secret"});
        assert!(journal_event_from_wire(&encode(&tampered, "user_message")).is_none());
    }

    #[test]
    fn wire_snapshot_rejects_tampering_and_cross_session_envelope() {
        let journal = RemoteJournal::new("epoch-1");
        journal
            .append(
                "event-1",
                JournalKind::UserMessage,
                "remote-a",
                None,
                json!({"text":"safe"}),
            )
            .unwrap();
        let snapshot = journal.snapshot_for("remote-a").unwrap();
        let encode = |snapshot: &JournalSnapshot, session_id: &str| {
            let raw = serde_json::to_vec(snapshot).unwrap();
            json!({
                "snapshot_b64": base64::engine::general_purpose::STANDARD.encode(&raw),
                "snapshot_bytes": raw.len().to_string(), "sha256": snapshot.digest,
                "journal_epoch": snapshot.epoch, "base_sequence": snapshot.sequence.to_string(),
                "session_id": session_id,
            })
        };
        assert!(journal_snapshot_from_wire(&encode(&snapshot, "remote-a")).is_some());
        assert!(journal_snapshot_from_wire(&encode(&snapshot, "remote-other")).is_none());
        let mut tampered = snapshot.clone();
        tampered.events[0].payload["text"] = json!("changed");
        assert!(journal_snapshot_from_wire(&encode(&tampered, "remote-a")).is_none());
    }

    #[test]
    fn duplicate_event_and_equal_base_snapshot_still_verify_durable_content() {
        let journal = RemoteJournal::new("epoch-1");
        let event = journal
            .append(
                "event-1",
                JournalKind::UserMessage,
                "remote-a",
                None,
                json!({"text":"safe"}),
            )
            .unwrap();
        let snapshot = journal.snapshot_for("remote-a").unwrap();
        let mut cache = JournalExpectations::default();
        let mut stored_event: Option<JournalEvent> = None;
        let mut event_calls = 0;
        let mut event_sink = |candidate: &JournalEvent| {
            event_calls += 1;
            match &stored_event {
                Some(stored) if stored != candidate => Err("projection_content_conflict".into()),
                Some(_) => Ok(()),
                None => {
                    stored_event = Some(candidate.clone());
                    Ok(())
                }
            }
        };
        assert_eq!(
            project_journal_event(&mut cache, &event, &mut event_sink).unwrap(),
            JournalAdmission::Apply
        );
        assert_eq!(
            project_journal_event(&mut cache, &event, &mut event_sink).unwrap(),
            JournalAdmission::Duplicate
        );
        let mut changed = event.clone();
        changed.payload = json!({"text":"changed"});
        assert!(project_journal_event(&mut cache, &changed, &mut event_sink).is_err());
        assert_eq!(event_calls, 3);
        assert_eq!(cache.expected("remote-a"), Some(("epoch-1", 1)));

        let mut stored_snapshot: Option<JournalSnapshot> = None;
        let mut snapshot_calls = 0;
        let mut snapshot_sink = |candidate: &JournalSnapshot| {
            snapshot_calls += 1;
            match &stored_snapshot {
                Some(stored) if stored != candidate => Err("projection_content_conflict".into()),
                Some(_) => Ok(()),
                None => {
                    stored_snapshot = Some(candidate.clone());
                    Ok(())
                }
            }
        };
        project_journal_snapshot(&mut cache, &snapshot, &mut snapshot_sink).unwrap();
        project_journal_snapshot(&mut cache, &snapshot, &mut snapshot_sink).unwrap();
        let mut changed = snapshot.clone();
        changed.events[0].payload = json!({"text":"changed"});
        changed.digest = changed.compute_digest();
        assert!(project_journal_snapshot(&mut cache, &changed, &mut snapshot_sink).is_err());
        assert_eq!(snapshot_calls, 3);
        assert_eq!(cache.expected("remote-a"), Some(("epoch-1", 1)));
    }

    #[test]
    fn queued_or_failed_replay_retries_with_stable_id_until_confirmed() {
        let mut cache = JournalExpectations::default();
        let now = tokio::time::Instant::now();
        let mut request_id = String::new();
        assert!(begin_and_send_replay(
            &mut cache,
            "remote-a",
            "epoch-1",
            3,
            5,
            "generation-1",
            now,
            |pending| {
                request_id = pending.request_id.clone();
                assert_eq!((pending.epoch.as_str(), pending.after), ("epoch-1", 3));
                true
            }
        ));
        assert!(
            cache
                .due_replays(
                    "generation-1",
                    now + REPLAY_RETRY_INTERVAL - Duration::from_millis(1)
                )
                .is_empty()
        );
        let due = cache.due_replays("generation-1", now + REPLAY_RETRY_INTERVAL);
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].1.request_id, request_id);
        assert!(
            cache
                .due_replays("generation-1", now + REPLAY_RETRY_INTERVAL)
                .is_empty()
        );
        assert!(!begin_and_send_replay(
            &mut cache,
            "remote-a",
            "epoch-1",
            3,
            5,
            "generation-1",
            now,
            |_| { panic!("one session cannot have two concurrent requests") }
        ));
        // A correlated negative response advances this same request to the
        // next heartbeat without replacing its identity.
        cache.expedite_negative_response(&request_id, now + Duration::from_secs(4));
        let due = cache.due_replays("generation-1", now + Duration::from_secs(4));
        assert_eq!(due[0].1.request_id, request_id);
        let mut confirmed = JournalEvent {
            event_id: "event-5".into(),
            epoch: "epoch-1".into(),
            sequence: 5,
            kind: "user_message".into(),
            session_id: "remote-a".into(),
            task_id: None,
            terminal_status: None,
            payload: json!({"text":"safe"}),
        };
        cache.mark_applied(&confirmed);
        assert!(
            cache
                .due_replays("generation-1", now + Duration::from_secs(30))
                .is_empty()
        );
        confirmed.sequence = 6;
        assert!(!begin_and_send_replay(
            &mut cache,
            "remote-a",
            "epoch-1",
            5,
            6,
            "generation-1",
            now,
            |_| {
                // Local seal or queue failure does not discard retry state.
                false
            }
        ));
        let retry = cache.due_replays("generation-1", now + REPLAY_RETRY_INTERVAL);
        assert_eq!(retry.len(), 1);
        assert_ne!(retry[0].1.request_id, request_id);
        cache.rearm_pending(now + Duration::from_secs(4));
        let resumed = cache.due_replays("generation-1", now + Duration::from_secs(4));
        assert_eq!(resumed[0].1.request_id, retry[0].1.request_id);
        let mut snapshot = JournalSnapshot {
            epoch: "epoch-1".into(),
            session_id: "remote-a".into(),
            sequence: 6,
            digest: String::new(),
            events: vec![confirmed],
        };
        snapshot.digest = snapshot.compute_digest();
        assert!(snapshot.verify_digest());
        cache.mark_snapshot(&snapshot);
        assert!(
            cache
                .due_replays("generation-1", now + Duration::from_secs(30))
                .is_empty()
        );
    }

    #[test]
    fn replay_pending_capacity_is_bounded() {
        let mut cache = JournalExpectations::default();
        let now = tokio::time::Instant::now();
        for index in 0..MAX_PENDING_REPLAYS {
            assert!(
                cache
                    .begin_replay(
                        &format!("session-{index}"),
                        "epoch-1",
                        0,
                        1,
                        "generation-1",
                        now
                    )
                    .is_some()
            );
        }
        assert!(
            cache
                .begin_replay("overflow", "epoch-1", 0, 1, "generation-1", now)
                .is_none()
        );
        assert_eq!(
            cache
                .due_replays("generation-1", now + REPLAY_RETRY_INTERVAL)
                .len(),
            MAX_PENDING_REPLAYS
        );
        assert!(
            cache
                .due_replays("generation-2", now + Duration::from_secs(6))
                .is_empty()
        );
        assert!(cache.awaiting_replay.is_empty());
    }

    #[test]
    fn late_old_epoch_projection_cannot_clear_new_epoch_replay() {
        let now = tokio::time::Instant::now();
        let event = |epoch: &str, sequence: u64| JournalEvent {
            event_id: format!("event-{epoch}-{sequence}"),
            epoch: epoch.into(),
            sequence,
            kind: "user_message".into(),
            session_id: "remote-a".into(),
            task_id: None,
            terminal_status: None,
            payload: json!({"text":"safe"}),
        };
        let snapshot = |entry: JournalEvent| {
            let mut snapshot = JournalSnapshot {
                epoch: entry.epoch.clone(),
                session_id: entry.session_id.clone(),
                sequence: entry.sequence,
                digest: String::new(),
                events: vec![entry],
            };
            snapshot.digest = snapshot.compute_digest();
            assert!(snapshot.verify_digest());
            snapshot
        };
        let mut cache = JournalExpectations::default();
        cache.mark_applied(&event("epoch-1", 1));
        let future = event("epoch-2", 5);
        assert_eq!(
            journal_admission(cache.expected("remote-a"), &future),
            JournalAdmission::RequestReplay {
                epoch: "epoch-2".into(),
                after: 0
            }
        );
        let mut request_id = String::new();
        assert!(begin_and_send_replay(
            &mut cache,
            "remote-a",
            "epoch-2",
            0,
            5,
            "generation-1",
            now,
            |pending| {
                request_id = pending.request_id.clone();
                true
            },
        ));
        let mut event_sink = |_: &JournalEvent| Ok(());
        let late = event("epoch-1", 2);
        assert_eq!(
            project_journal_event(&mut cache, &late, &mut event_sink).unwrap(),
            JournalAdmission::Apply
        );
        let mut snapshot_sink = |_: &JournalSnapshot| Ok(());
        project_journal_snapshot(&mut cache, &snapshot(late), &mut snapshot_sink).unwrap();
        assert_eq!(cache.awaiting_replay["remote-a"].request_id, request_id);
        let due = cache.due_replays("generation-1", now + REPLAY_RETRY_INTERVAL);
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].1.epoch, "epoch-2");
        assert_eq!(due[0].1.request_id, request_id);
        project_journal_snapshot(
            &mut cache,
            &snapshot(event("epoch-2", 5)),
            &mut snapshot_sink,
        )
        .unwrap();
        assert!(cache.awaiting_replay.is_empty());
        assert!(
            cache
                .due_replays("generation-1", now + Duration::from_secs(30))
                .is_empty()
        );
    }
}
