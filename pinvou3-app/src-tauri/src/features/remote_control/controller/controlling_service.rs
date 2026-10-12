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
    ControllingStatusKind, ResumeStep,
};
use super::controlling_handshake::{ControllingHandshake, HandshakeError};
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
use super::journal::{JournalEvent, JournalSnapshot, MAX_EVENT_PAYLOAD_BYTES};
use super::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_runtime_message_json,
};
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

    /// E2E-06：发送恢复族的敏感控制帧（prepare/confirm/ready_ack）。默认实现
    /// 失败关闭，只有真实 transport 端口实现它；测试替身可覆写以记录帧。
    fn send_resume_control(&self, _message: Value) -> Result<(), String> {
        Err("controller_resume_unsupported".to_owned())
    }
}

/// A 端权威 journal 落库端口（E2E-05）：由命令层实现——按会话引用写入 A 的
/// SessionStore.RemoteProxy 投影正文；feature 层只负责解密与校验。
/// Constructed only inside the AEAD-opened business-frame dispatcher. The
/// app receives an authenticated frame, never a caller-asserted boolean.
pub(crate) struct AuthenticatedJournalFrame<'a> {
    operation: &'a ControllerOperation,
    lease: &'a LeaseContext,
    event: &'a JournalEvent,
}

impl AuthenticatedJournalFrame<'_> {
    pub(crate) fn operation(&self) -> &ControllerOperation {
        self.operation
    }
    pub(crate) fn lease(&self) -> &LeaseContext {
        self.lease
    }
    pub(crate) fn event(&self) -> &JournalEvent {
        self.event
    }
}

pub(crate) struct AuthenticatedSnapshotFrame<'a> {
    operation: &'a ControllerOperation,
    lease: &'a LeaseContext,
    snapshot: &'a JournalSnapshot,
}

impl AuthenticatedSnapshotFrame<'_> {
    pub(crate) fn operation(&self) -> &ControllerOperation {
        self.operation
    }
    pub(crate) fn lease(&self) -> &LeaseContext {
        self.lease
    }
    pub(crate) fn snapshot(&self) -> &JournalSnapshot {
        self.snapshot
    }
}

pub(crate) trait ControllingJournalSink: Send + Sync {
    fn publish(&self, frame: &AuthenticatedJournalFrame<'_>) -> Result<(), String>;

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
    inbound: mpsc::UnboundedReceiver<ControllerLaneEvent>,
    relay_error_code: Option<&'static str>,
    relay_error_seen: bool,
    lane_disconnected: bool,
}

#[async_trait::async_trait]
impl ControllingLane for ControllerTransportPort {
    async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
        self.inbound.recv().await
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
        relay_error_seen: false,
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

/// 失败归因（有界、不含凭据）：公共码 + 来源（Relay 明确报错 / 通道断开 / 本地校验）。
/// 只用于端上排障日志，不改变任何面向用户的错误语义。
fn failure_origin_detail(port: &impl ControllingRelayPort, code: &str) -> String {
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

    match complete_connected(manager, operation.clone(), sender, business, outcome) {
        Ok(status) => {
            on_status(&status);
            Ok(status)
        }
        Err(code) => {
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

/// 通道保活期间的事件出口（状态/会话/任务），由 Tauri 层注入具体广播。
pub(crate) struct ControllingLaneSinks<'a> {
    pub(crate) on_status: &'a mut (dyn FnMut(&ControllingStatus) + Send),
    pub(crate) on_session: &'a mut (dyn FnMut(&ControllingSessionProjection) + Send),
    /// Authenticated capability response mapping, before A capability state advances.
    pub(crate) on_mapping:
        &'a mut (dyn FnMut(&LeaseContext, &str, &str) -> Result<(), String> + Send),
    pub(crate) on_task: &'a mut (dyn FnMut(&LeaseContext, Value) + Send),
    /// 解密并校验过的 B 权威 journal：A 端仅投影到 RemoteProxy。
    pub(crate) on_journal:
        &'a mut (dyn FnMut(&AuthenticatedJournalFrame<'_>) -> Result<(), String> + Send),
    /// 权威快照（E2E-05 缺口恢复）：A 端据此原子替换本地副本。
    pub(crate) on_snapshot:
        &'a mut (dyn FnMut(&AuthenticatedSnapshotFrame<'_>) -> Result<(), String> + Send),
    /// E2E-06：待发送的 A 端心跳帧（明文控制帧，绑定当前租约）。
    // True means authenticated transport admission succeeded, not remote receipt.
    pub(crate) on_liveness: &'a mut (dyn FnMut(Value) -> bool + Send),
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
    let operation = evidence.operation.clone();
    let Ok(owned_sender) = manager.transport_for_operation(&operation) else {
        return;
    };
    let mut owned_liveness = |frame: Value| {
        if manager.with_lane_owner(&operation, || ()).is_err() {
            return false;
        }
        if let Some(sender) = owned_sender.as_ref() {
            return sender.try_send_authenticated_control(&frame).is_ok();
        }
        #[cfg(test)]
        {
            (sinks.on_liveness)(frame)
        }
        #[cfg(not(test))]
        {
            false
        }
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
                    ControllerLaneEvent::Disconnected => {
                        if resume.is_active() {
                            continue;
                        }
                        // 断线：保留密钥材料并挂起业务/存活帧，随后启动恢复流程。
                        let now_b = (evidence.monotonic)();
                        let cutoff = budget.freeze(now_b);
                        let Ok(status) = manager.mark_owned_transport_disconnected(&operation, now_b, cutoff, business) else { return; };
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
                                ResumeOutcome::Completed => {
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
    Completed,
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
            let Ok(confirm) = business.seal_resume_confirm(&request_id(), activation_id) else {
                return ResumeOutcome::Failed;
            };
            if lane.send_resume_control(confirm).is_err() {
                return ResumeOutcome::Failed;
            }
            resume.activation_id = Some(activation_id.to_owned());
            ResumeOutcome::Progressed
        }
        // B 的确认只用于交叉验证（本地相位由 Relay 的帧推进）。
        "encrypted_resume_confirm" => match business.open_resume_confirm(&frame) {
            Ok(()) => ResumeOutcome::Progressed,
            Err(_) => ResumeOutcome::Ignored,
        },
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
            let Ok(fingerprint) = business.resume_fingerprint(activation_id) else {
                return ResumeOutcome::Failed;
            };
            let Some(resume_request_id) = frame.get("request_id").and_then(Value::as_str) else {
                return ResumeOutcome::Failed;
            };
            if manager
                .complete_resumed(
                    operation,
                    activation_id,
                    resume_request_id,
                    now_b,
                    (liveness_epoch_a, liveness_epoch_b),
                    token,
                    fingerprint,
                    relay_deadline,
                )
                .is_err()
            {
                return ResumeOutcome::Failed;
            }
            if manager
                .with_lane_owner(owner, || business.activate())
                .is_err()
            {
                return ResumeOutcome::Failed;
            }
            ResumeOutcome::Completed
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

/// 解密并派发一条入站业务帧。任何一层不符（非加密帧、无活租约、解密/校验
/// 失败）都静默丢弃——攻击者不能通过伪造帧影响任何状态。
fn dispatch_inbound_frame(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    sessions: &ControllingSessionManager,
    business: &ControllingLaneState,
    journal: &mut JournalExpectations,
    liveness: &mut LivenessExpectations,
    json: &str,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    let Ok(frame) = serde_json::from_str::<Value>(json) else {
        return;
    };
    let Ok((runtime, epoch_a)) = manager
        .with_peer_liveness_context(operation, |runtime, _, a| (runtime.clone(), a.to_owned()))
    else {
        return;
    };
    let Some(lease) = runtime.lease else {
        return;
    };
    let capabilities = runtime.negotiated_capabilities;
    if frame.get("type").and_then(Value::as_str) == Some("error") {
        if let Some(request_id) = frame.get("request_id").and_then(Value::as_str) {
            // A correlated negative response may advance retry to the next
            // heartbeat; it never confirms/ACKs a journal cursor.
            journal.expedite_negative_response(request_id, tokio::time::Instant::now());
        }
        let relay_code = frame
            .get("code")
            .and_then(Value::as_str)
            .unwrap_or("invalid_message");
        log_controller_event("runtime_relay_error", relay_code);
        let outcome = sessions.fail_pending_capability_for_lease_with_guard(
            &lease,
            "capability_snapshot_unavailable",
            |commit| {
                manager.with_lane_owner(operation, commit).map_err(|_| {
                    super::controlling_session::SessionError::new("session_context_stale")
                })
            },
        );
        match outcome {
            Ok(projection) => {
                log_controller_event("capability_snapshot_failed", "relay_rejected");
                (sinks.on_session)(&projection);
            }
            Err(error) => {
                log_controller_event("capability_error_rejected", error.code);
            }
        }
        return;
    }
    // Relay 按冻结协议把 B 的挑战作为顶层明文控制帧转发给 A。它不是业务
    // `encrypted_frame`，但必须先通过角色、租约和能力绑定校验，随后才生成由
    // A/B 会话密钥保护的直接存活证明。此前该分支位于 encrypted_frame 解密后，
    // 导致真实挑战在到达应答逻辑之前被静默丢弃。
    if frame.get("type").and_then(Value::as_str) == Some("controller_liveness_challenge_a") {
        dispatch_liveness_challenge(
            manager,
            operation,
            business,
            liveness,
            &frame,
            &lease,
            &capabilities,
            &epoch_a,
            sinks,
        );
        return;
    }
    if frame.get("type").and_then(Value::as_str) != Some("encrypted_frame") {
        return;
    }
    let Ok(Ok((payload_kind, payload))) =
        manager.with_lane_owner(operation, || business.open(&frame, &capabilities))
    else {
        return;
    };
    dispatch_authenticated_payload(
        manager,
        operation,
        sessions,
        business,
        journal,
        &lease,
        &capabilities,
        &payload_kind,
        payload,
        sinks,
    );
}

/// AEAD admission and projection are separate boundaries: final shared-state
/// commits still validate this original owner after a replacement can occur.
fn dispatch_authenticated_payload(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    sessions: &ControllingSessionManager,
    business: &ControllingLaneState,
    journal: &mut JournalExpectations,
    lease: &LeaseContext,
    capabilities: &[String],
    payload_kind: &str,
    payload: Value,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    if manager.with_lane_owner(operation, || ()).is_err() {
        return;
    }
    match payload_kind {
        "capability_snapshot" => {
            match sessions.apply_capability_snapshot_for_lease_with_mapping_and_guard(
                &lease,
                &payload,
                |remote_ref, controlled_session_id| {
                    (sinks.on_mapping)(&lease, remote_ref, controlled_session_id).map_err(|_| {
                        super::controlling_session::SessionError::new("remote_mapping_unavailable")
                    })
                },
                |commit| {
                    manager.with_lane_owner(operation, commit).map_err(|_| {
                        super::controlling_session::SessionError::new("session_context_stale")
                    })
                },
            ) {
                Ok(projection) => {
                    log_controller_event("capability_snapshot_applied", "ready");
                    (sinks.on_session)(&projection);
                }
                Err(error) => {
                    log_controller_event("capability_snapshot_rejected", error.code);
                }
            }
        }
        "capability_snapshot_error" => {
            match sessions.apply_capability_error_for_lease_with_guard(&lease, &payload, |commit| {
                manager.with_lane_owner(operation, commit).map_err(|_| {
                    super::controlling_session::SessionError::new("session_context_stale")
                })?
            }) {
                Ok(projection) => {
                    log_controller_event("capability_snapshot_failed", "unavailable");
                    (sinks.on_session)(&projection);
                }
                Err(error) => {
                    log_controller_event("capability_error_rejected", error.code);
                }
            }
        }
        "task_status" => {
            if let Some(event) = task_event_projection(&lease, &payload) {
                log_controller_event(
                    "task_status_received",
                    event
                        .get("status")
                        .and_then(Value::as_str)
                        .unwrap_or("unknown"),
                );
                (sinks.on_task)(&lease, event);
            }
        }
        "remote_action_result" => {
            // Only the authenticated decrypt path may reach this callback.
            // The adapter never emits the raw result to WebView.
            (sinks.on_task)(&lease, payload);
        }
        "journal_event" => {
            let Some(event) = journal_event_from_wire(&payload) else {
                log_controller_event("journal_event_rejected", "invalid_envelope_or_identity");
                return;
            };
            if journal.is_retired(&event.session_id, &event.epoch) {
                log_controller_event("journal_event_ignored", "retired_epoch");
                return;
            }
            match project_journal_event(journal, &event, &mut |event| {
                (sinks.on_journal)(&AuthenticatedJournalFrame {
                    operation,
                    lease: &lease,
                    event,
                })
            }) {
                Ok(JournalAdmission::Apply) => {
                    log_controller_event("journal_event_applied", &event.kind);
                }
                Ok(JournalAdmission::Duplicate) => {
                    log_controller_event("journal_event_verified", "duplicate");
                }
                Err(_) => log_controller_event("journal_event_rejected", "projection_failed"),
                Ok(JournalAdmission::RequestReplay { epoch, after }) => {
                    log_controller_event("journal_event_gap", "replay_requested");
                    begin_and_send_replay(
                        journal,
                        &event.session_id,
                        &epoch,
                        after,
                        event.sequence,
                        &lease.remote_control_generation,
                        tokio::time::Instant::now(),
                        |pending| {
                            request_journal_replay(
                                manager,
                                operation,
                                business,
                                &lease,
                                &capabilities,
                                &event.session_id,
                                &pending.epoch,
                                pending.after,
                                &pending.request_id,
                            )
                        },
                    );
                }
            }
        }
        "session_snapshot" => {
            let Some(snapshot) = journal_snapshot_from_wire(&payload) else {
                return;
            };
            // Equal-base snapshots still enter the authoritative sink so
            // DEV-30 can verify immutable content; transport cache is not ACK
            // authority and may never skip that comparison.
            match project_journal_snapshot(journal, &snapshot, &mut |snapshot| {
                (sinks.on_snapshot)(&AuthenticatedSnapshotFrame {
                    operation,
                    lease: &lease,
                    snapshot,
                })
            }) {
                Ok(()) => {}
                Err(_) => log_controller_event("journal_snapshot_rejected", "projection_failed"),
            }
        }
        _ => {}
    }
}

/// 严格校验并应答 B 的顶层存活挑战。Relay 只能转发挑战，不能构造这里返回的
/// AEAD 证明；因此明文控制帧仍不会被直接当成 B 的有效存活证据。
fn dispatch_liveness_challenge(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    business: &ControllingLaneState,
    liveness: &mut LivenessExpectations,
    frame: &Value,
    lease: &LeaseContext,
    capabilities: &[String],
    epoch: &str,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    let context = RuntimeValidationContext {
        lease: Some(lease.clone()),
        negotiated_capabilities: capabilities.to_vec(),
    };
    let Ok(challenge) = validate_controller_runtime_message_json(
        &frame.to_string(),
        SenderRole::ControlledB,
        Some(&context),
    ) else {
        return;
    };
    let Some(challenge_id) = challenge.get("challenge_id").and_then(Value::as_str) else {
        return;
    };
    let Some(challenge_b64) = challenge.get("challenge_b64").and_then(Value::as_str) else {
        return;
    };
    let now_a = lane_monotonic_millis();
    if !liveness.can_answer(challenge_id, now_a) {
        return;
    }
    let Some(sequence) = liveness.next_sequence() else {
        return;
    };
    let inner = serde_json::json!({
        "v": 1,
        "type": "controller_liveness_a",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "role": "A",
        "liveness_epoch_a": epoch,
        "heartbeat_sequence_a": sequence.to_string(),
        "challenge_id": challenge_id,
        "challenge_b64": challenge_b64,
    });
    let Ok(Ok(response)) =
        manager.with_lane_owner(operation, || business.seal_liveness(&inner, capabilities))
    else {
        return;
    };
    if (sinks.on_liveness)(response) {
        liveness.record_answered(challenge_id, now_a);
    }
}

/// wire `session_snapshot` → 权威快照。base64 体积与声明一致、摘要与快照本体
/// 自带的 digest 一致才接受；事件条目沿用与增量事件相同的体积上限。
fn journal_snapshot_from_wire(payload: &Value) -> Option<JournalSnapshot> {
    let encoded = payload.get("snapshot_b64").and_then(Value::as_str)?;
    let declared = payload
        .get("snapshot_bytes")
        .and_then(Value::as_str)
        .and_then(|value| value.parse::<usize>().ok())?;
    let raw = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .ok()?;
    if raw.len() != declared || raw.len() > MAX_SNAPSHOT_BYTES {
        return None;
    }
    let snapshot: JournalSnapshot = serde_json::from_slice(&raw).ok()?;
    if !snapshot.verify_digest()
        || payload.get("session_id").and_then(Value::as_str) != Some(snapshot.session_id.as_str())
        || payload.get("sha256").and_then(Value::as_str) != Some(snapshot.digest.as_str())
        || payload.get("journal_epoch").and_then(Value::as_str) != Some(snapshot.epoch.as_str())
        || payload.get("base_sequence").and_then(Value::as_str)
            != Some(snapshot.sequence.to_string().as_str())
    {
        return None;
    }
    Some(snapshot)
}

/// 快照帧的硬上限：256 条事件 × 单条上限 + 信封开销。
const MAX_SNAPSHOT_BYTES: usize = 256 * MAX_EVENT_PAYLOAD_BYTES;

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
    if event.validate_exportable().is_err()
        || payload.get("event_kind").and_then(Value::as_str) != event.wire_kind()
        || payload.get("journal_epoch").and_then(Value::as_str) != Some(event.epoch.as_str())
        || payload.get("event_sequence").and_then(Value::as_str)
            != Some(event.sequence.to_string().as_str())
        || payload.get("session_id").and_then(Value::as_str) != Some(event.session_id.as_str())
    {
        return None;
    }
    Some(event)
}

/// A 端 transport 顺序提示；重复仍须交给权威投影端口验证内容等价。
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) enum JournalAdmission {
    Apply,
    RequestReplay { epoch: String, after: u64 },
    Duplicate,
}

/// 纯函数：依据本端已知的 (epoch, last_sequence) 与到达事件推导接纳动作。
/// 换 epoch 时从该 epoch 的 0 号位请求回放（B 侧保留窗口不足会给快照）。
pub(crate) fn journal_admission(
    expected: Option<(&str, u64)>,
    event: &JournalEvent,
) -> JournalAdmission {
    match expected {
        None if event.sequence == 1 => JournalAdmission::Apply,
        None => JournalAdmission::RequestReplay {
            epoch: event.epoch.clone(),
            after: 0,
        },
        Some((epoch, _)) if epoch != event.epoch => JournalAdmission::RequestReplay {
            epoch: event.epoch.clone(),
            after: 0,
        },
        Some((_, last)) if event.sequence <= last => JournalAdmission::Duplicate,
        Some((_, last)) if event.sequence == last.saturating_add(1) => JournalAdmission::Apply,
        Some((_, last)) => JournalAdmission::RequestReplay {
            epoch: event.epoch.clone(),
            after: last,
        },
    }
}

fn project_journal_event(
    cache: &mut JournalExpectations,
    event: &JournalEvent,
    sink: &mut dyn FnMut(&JournalEvent) -> Result<(), String>,
) -> Result<JournalAdmission, String> {
    let admission = journal_admission(cache.expected(&event.session_id), event);
    if matches!(
        admission,
        JournalAdmission::Apply | JournalAdmission::Duplicate
    ) {
        sink(event)?;
        if admission == JournalAdmission::Apply {
            cache.mark_applied(event);
        }
    }
    Ok(admission)
}

fn project_journal_snapshot(
    cache: &mut JournalExpectations,
    snapshot: &JournalSnapshot,
    sink: &mut dyn FnMut(&JournalSnapshot) -> Result<(), String>,
) -> Result<(), String> {
    if cache.is_retired(&snapshot.session_id, &snapshot.epoch) {
        return Ok(());
    }
    let same_or_older = cache
        .expected(&snapshot.session_id)
        .filter(|(epoch, _)| *epoch == snapshot.epoch)
        .map(|(_, sequence)| sequence.cmp(&snapshot.sequence));
    if same_or_older == Some(std::cmp::Ordering::Greater) {
        return Ok(());
    }
    // An equal-base snapshot may carry conflicting content. The durable
    // projection port must decide; this cache cannot pronounce it duplicate.
    sink(snapshot)?;
    if same_or_older != Some(std::cmp::Ordering::Equal) {
        cache.mark_snapshot(snapshot);
    }
    Ok(())
}

/// A 端存活状态：与心跳共享的单调序号 + 已应答过的挑战（同一挑战只答一次，
/// 避免重复投递导致重复加密大帧）。
#[derive(Default)]
pub(crate) struct LivenessExpectations {
    sequence: u64,
    answered: std::collections::BTreeMap<String, u64>,
}

const MAX_ANSWERED_LIVENESS_CHALLENGES: usize = 64;

impl LivenessExpectations {
    pub(crate) fn next_sequence(&mut self) -> Option<u64> {
        self.sequence = self.sequence.checked_add(1)?;
        Some(self.sequence)
    }

    fn can_answer(&mut self, challenge_id: &str, now_a: u64) -> bool {
        self.answered.retain(|_, answered_at| {
            now_a.saturating_sub(*answered_at) <= super::liveness::LIVENESS_TOTAL_BUDGET_MS
        });
        !self.answered.contains_key(challenge_id)
            && self.answered.len() < MAX_ANSWERED_LIVENESS_CHALLENGES
    }

    /// Seal/enqueue failures leave the challenge retryable. At capacity we
    /// fail closed instead of evicting replay history inside its validity window.
    fn record_answered(&mut self, challenge_id: &str, now_a: u64) {
        self.answered.insert(challenge_id.to_owned(), now_a);
    }
}

/// Conservative transport replay cache, never projection/ACK authority.
/// A restart may lose these hints; only the DEV-30 durable projection decides
/// duplicate content and confirmed cursor. In-flight replay suppresses only
/// redundant transport requests, not calls into the projection sink.
/// The authenticated journal sink sends ACK only after DEV-30's RemoteProxy
/// projection confirms the durable cursor. This transport cache does not
/// grant ACK authority or introduce another protocol message type.
const MAX_PENDING_REPLAYS: usize = 256;
const REPLAY_RETRY_INTERVAL: Duration = Duration::from_secs(3);

#[derive(Clone, Debug)]
pub(crate) struct PendingReplay {
    request_id: String,
    epoch: String,
    after: u64,
    target_sequence: u64,
    generation: String,
    next_retry_at: tokio::time::Instant,
}

#[derive(Default)]
pub(crate) struct JournalExpectations {
    applied: std::collections::BTreeMap<String, (String, u64)>,
    awaiting_replay: std::collections::BTreeMap<String, PendingReplay>,
    retired_epochs: std::collections::BTreeMap<String, std::collections::BTreeSet<String>>,
}

impl JournalExpectations {
    pub(crate) fn expected(&self, session_id: &str) -> Option<(&str, u64)> {
        self.applied
            .get(session_id)
            .map(|(epoch, sequence)| (epoch.as_str(), *sequence))
    }

    pub(crate) fn mark_applied(&mut self, event: &JournalEvent) {
        if let Some((old_epoch, _)) = self.applied.get(&event.session_id) {
            if old_epoch != &event.epoch {
                self.retired_epochs
                    .entry(event.session_id.clone())
                    .or_default()
                    .insert(old_epoch.clone());
            }
        }
        self.applied.insert(
            event.session_id.clone(),
            (event.epoch.clone(), event.sequence),
        );
        if self
            .awaiting_replay
            .get(&event.session_id)
            .is_some_and(|pending| {
                pending.epoch == event.epoch && event.sequence >= pending.target_sequence
            })
        {
            self.awaiting_replay.remove(&event.session_id);
        }
    }

    /// 快照基准对空事件窗口同样有效，归属使用快照的明确 session_id。
    pub(crate) fn mark_snapshot(&mut self, snapshot: &JournalSnapshot) {
        if let Some((old_epoch, _)) = self.applied.get(&snapshot.session_id) {
            if old_epoch != &snapshot.epoch {
                self.retired_epochs
                    .entry(snapshot.session_id.clone())
                    .or_default()
                    .insert(old_epoch.clone());
            }
        }
        self.applied.insert(
            snapshot.session_id.clone(),
            (snapshot.epoch.clone(), snapshot.sequence),
        );
        if self
            .awaiting_replay
            .get(&snapshot.session_id)
            .is_some_and(|pending| {
                pending.epoch == snapshot.epoch && snapshot.sequence >= pending.target_sequence
            })
        {
            self.awaiting_replay.remove(&snapshot.session_id);
        }
    }

    pub(crate) fn is_retired(&self, session_id: &str, epoch: &str) -> bool {
        self.retired_epochs
            .get(session_id)
            .is_some_and(|epochs| epochs.contains(epoch))
    }

    /// One stable request per session/epoch gap. The bounded entry survives
    /// both successful queueing without response and local seal/send failure;
    /// heartbeat retries it with the exact same request_id.
    pub(crate) fn begin_replay(
        &mut self,
        session_id: &str,
        epoch: &str,
        after: u64,
        target_sequence: u64,
        generation: &str,
        now: tokio::time::Instant,
    ) -> Option<PendingReplay> {
        if let Some(pending) = self.awaiting_replay.get_mut(session_id) {
            if pending.epoch == epoch && pending.generation == generation {
                pending.target_sequence = pending.target_sequence.max(target_sequence);
                return None;
            }
        }
        if !self.awaiting_replay.contains_key(session_id)
            && self.awaiting_replay.len() >= MAX_PENDING_REPLAYS
        {
            return None;
        }
        let pending = PendingReplay {
            request_id: format!("request-replay-{}", Uuid::new_v4().simple()),
            epoch: epoch.to_owned(),
            after,
            target_sequence,
            generation: generation.to_owned(),
            next_retry_at: now + REPLAY_RETRY_INTERVAL,
        };
        self.awaiting_replay
            .insert(session_id.to_owned(), pending.clone());
        Some(pending)
    }

    pub(crate) fn due_replays(
        &mut self,
        generation: &str,
        now: tokio::time::Instant,
    ) -> Vec<(String, PendingReplay)> {
        self.awaiting_replay
            .retain(|_, pending| pending.generation == generation);
        let mut due = Vec::new();
        for (session_ref, pending) in &mut self.awaiting_replay {
            if pending.next_retry_at <= now {
                pending.next_retry_at = now + REPLAY_RETRY_INTERVAL;
                due.push((session_ref.clone(), pending.clone()));
            }
        }
        due
    }

    pub(crate) fn expedite_negative_response(
        &mut self,
        request_id: &str,
        now: tokio::time::Instant,
    ) {
        if let Some(pending) = self
            .awaiting_replay
            .values_mut()
            .find(|pending| pending.request_id == request_id)
        {
            pending.next_retry_at = now;
        }
    }

    pub(crate) fn rearm_pending(&mut self, now: tokio::time::Instant) {
        for pending in self.awaiting_replay.values_mut() {
            pending.next_retry_at = now;
        }
    }
}

/// 冻结形状的 `journal_replay_request`（A→B 加密业务帧）。
pub(crate) fn build_journal_replay_request(
    lease: &LeaseContext,
    session_ref: &str,
    journal_epoch: &str,
    after_sequence: u64,
    request_id: &str,
) -> Result<Value, &'static str> {
    if session_ref.is_empty()
        || journal_epoch.is_empty()
        || request_id.is_empty()
        || request_id.len() > 128
    {
        return Err("journal_request_invalid");
    }
    Ok(serde_json::json!({
        "v": 1,
        "type": "journal_replay_request",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "request_id": request_id,
        "session_id": session_ref,
        "journal_epoch": journal_epoch,
        "after_sequence": after_sequence.to_string(),
    }))
}

/// 请求缺失区间的回放；失败不影响缺口事件的丢弃语义（下次事件仍会重试）。
fn request_journal_replay(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    business: &ControllingLaneState,
    lease: &LeaseContext,
    capabilities: &[String],
    session_ref: &str,
    journal_epoch: &str,
    after_sequence: u64,
    request_id: &str,
) -> bool {
    let Ok(payload) = build_journal_replay_request(
        lease,
        session_ref,
        journal_epoch,
        after_sequence,
        request_id,
    ) else {
        return false;
    };
    let Ok(sender) = manager.transport_for_operation(operation) else {
        return false;
    };
    manager
        .with_lane_owner(operation, || {
            let Ok(frame) = business.seal("journal_replay_request", &payload, capabilities) else {
                return false;
            };
            sender
                .as_ref()
                .is_some_and(|sender| sender.try_send_business(&frame).is_ok())
        })
        .unwrap_or(false)
}

fn begin_and_send_replay(
    cache: &mut JournalExpectations,
    session_id: &str,
    epoch: &str,
    after: u64,
    target_sequence: u64,
    generation: &str,
    now: tokio::time::Instant,
    send: impl FnOnce(&PendingReplay) -> bool,
) -> bool {
    let Some(pending) =
        cache.begin_replay(session_id, epoch, after, target_sequence, generation, now)
    else {
        return false;
    };
    // A queued request is not a response. Keep the entry even if the local
    // seal/send fails, so a timer drives the next attempt without new events.
    send(&pending)
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
        trust.clone(),
    );
    let (sender, mut port) = spawn_controlling_lane(&relay.controller_relay_ws_url());
    // 先登记 transport，保证并发的断开/重连能立刻停止本次尝试。
    let business = app.state::<ControllingLaneState>();
    if let Err(error) = manager.replace_owned_transport(&operation, sender.clone(), &business) {
        sender.stop();
        return Err(error.code.to_owned());
    }

    // A 端排障日志：先把「这次到底连的是哪个端点的哪条路径」写死，再把每个状态
    // 投影与最终结果码记下来。没有这几行时，「连接码无效或已过期」无法区分
    // 「码不对」与「两端其实在各自的 Relay 上」。
    let endpoint_detail = relay.controller_endpoint_log_detail();
    log_controller_event("connect_started", &endpoint_detail);
    let mut emit = |status: &ControllingStatus| {
        let _ = manager.with_lane_owner(&operation, || {
            log_controller_event("status", &controller_status_log_detail(status));
            let _ = app.emit(CONTROLLER_STATUS_EVENT, status);
        });
    };
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
        let mut previous_status = ControllingStatusKind::Connected;
        let mut on_status = |status: &ControllingStatus| {
            let _ = manager.with_lane_owner(&operation, || {
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
                        (ControllingStatusKind::Reconnecting, ControllingStatusKind::Error) => {
                            Some(
                                RemoteControlEvent::new(
                                    RemoteStage::Resume,
                                    RemoteOutcome::Rejected,
                                )
                                .reason(RemoteReason::ResumeFailed),
                            )
                        }
                        _ => None,
                    };
                    if let Some(event) = event {
                        track_remote_control(&keeper, event);
                    }
                    previous_status = current;
                }
                log_controller_event("status", &controller_status_log_detail(status));
                let _ = keeper.emit(CONTROLLER_STATUS_EVENT, status);
            });
        };
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

    #[derive(Default)]
    struct TestLane {
        events: VecDeque<ControllerLaneEvent>,
        reader_transport: Option<ControllerTransportSender>,
        authorize_resume_final: bool,
        /// E2E-06：恢复族出站帧（prepare/confirm/ready_ack）的记录口。
        resume_frames: Arc<std::sync::Mutex<Vec<Value>>>,
    }

    impl TestLane {
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
        async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
            let event = self.events.pop_front();
            if let Some(sender) = self.reader_transport.as_ref() {
                match event.as_ref() {
                    Some(ControllerLaneEvent::Disconnected) => {
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
                },
            )
            .unwrap();
        let mut lane = TestLane {
            events: VecDeque::from(vec![
                ControllerLaneEvent::Disconnected,
                ControllerLaneEvent::Frame("{}".to_owned()),
            ]),
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
            ControllerLaneEvent::Disconnected,
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
            vec!["reconnecting".to_owned()],
            "恢复成功不产生错误投影"
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

    /// 连接后的 Relay 错误必须立即结束唯一待处理的能力请求，不能退化成前端等待超时。
    #[test]
    fn connected_relay_error_fails_the_unique_pending_capability_request() {
        let fixture = pairing_fixture(RELAY_KEY_ID);
        let lease = lease_context(&fixture.reservation);
        let capabilities = vec!["encrypted_frames_v1".to_owned(), "task_v1".to_owned()];
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation.clone(),
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
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .map(|(projection, _)| projection)
            .unwrap();
        let request = sessions
            .capability_request(&session.session_ref, &lease)
            .unwrap();
        let business = ControllingLaneState::default();
        let mut projections = Vec::new();
        let mut on_status = |_: &ControllingStatus| {};
        let mut on_session =
            |projection: &ControllingSessionProjection| projections.push(projection.clone());
        let mut on_task = |_: &LeaseContext, _: Value| {};
        let mut on_journal = |_: &AuthenticatedJournalFrame<'_>| Ok(());
        let mut on_snapshot = |_: &AuthenticatedSnapshotFrame<'_>| Ok(());
        let mut on_liveness = |_: Value| false;
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        let mut journal = JournalExpectations::default();
        let mut liveness = LivenessExpectations::default();
        dispatch_inbound_frame(
            &manager,
            &manager.current_operation().unwrap(),
            &sessions,
            &business,
            &mut journal,
            &mut liveness,
            &json!({
                "v": 1,
                "type": "error",
                "request_id": "request-invalid",
                "code": "invalid_message",
                "message": "invalid message",
            })
            .to_string(),
            &mut sinks,
        );
        drop(sinks);

        assert_eq!(projections.len(), 1);
        assert_eq!(
            projections[0].capability_request_id.as_deref(),
            Some(request.request_id.as_str())
        );
        assert_eq!(
            projections[0].capability_error.as_deref(),
            Some("capability_snapshot_unavailable")
        );
        // Replacement can happen after the keeper's cheap event fence. Reuse
        // the same lease bytes deliberately: only the original operation owns
        // this plaintext error, and must not consume the new pending request.
        manager.with_lane_owner(&operation, || ()).unwrap();
        manager.disconnect();
        let replacement = manager.connect("234-4567").unwrap();
        manager.complete_reserved(replacement.clone()).unwrap();
        manager
            .complete_connected(
                replacement,
                ControllerLease {
                    context: lease.clone(),
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-2".into(),
                    liveness_epoch_b: "live-b-2".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7; 32],
                    transport_key_fingerprint: [9; 32],
                    negotiated_capabilities: capabilities.clone(),
                },
            )
            .unwrap();
        let next_request = sessions
            .capability_request(&session.session_ref, &lease)
            .unwrap();
        let mut ignored_session =
            |_: &ControllingSessionProjection| panic!("old error reached new owner");
        let mut stale_sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut ignored_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        dispatch_inbound_frame(&manager, &operation, &sessions, &business, &mut journal, &mut liveness,
            &json!({"v":1,"type":"error","request_id":"old-error","code":"invalid_message","message":"invalid message"}).to_string(), &mut stale_sinks);
        let still_pending = sessions
            .fail_pending_capability_for_lease(&lease, "capability_snapshot_unavailable")
            .unwrap();
        assert_eq!(
            still_pending.capability_request_id.as_deref(),
            Some(next_request.request_id.as_str())
        );
        assert!(!request_journal_replay(
            &manager,
            &operation,
            &business,
            &lease,
            &capabilities,
            &session.session_ref,
            "journal-epoch",
            0,
            "old-replay"
        ));
    }

    /// E2E-06：Relay 顶层存活挑战必须经租约校验并由 A 返回加密证明。
    #[test]
    fn opened_capability_error_cannot_mutate_a_replacement_owner_with_identical_lease() {
        let fixture = pairing_fixture(RELAY_KEY_ID);
        let lease = lease_context(&fixture.reservation);
        let (a_keys, b_keys) =
            paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let capabilities = vec!["encrypted_frames_v1".into(), "task_v1".into()];
        let manager = ControllingDeviceManager::default();
        let connect = || {
            let operation = manager.connect("234-4567").unwrap();
            manager.complete_reserved(operation.clone()).unwrap();
            manager
                .complete_connected(
                    operation.clone(),
                    ControllerLease {
                        context: lease.clone(),
                        peer_endpoint_id: "endpoint-0001".into(),
                        reservation_id: "reservation-0001".into(),
                        device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                        liveness_epoch_a: "live-a-1".into(),
                        liveness_epoch_b: "live-b-1".into(),
                        resume_deadline_b: 30_000,
                        resume_token: [7; 32],
                        transport_key_fingerprint: [9; 32],
                        negotiated_capabilities: capabilities.clone(),
                    },
                )
                .unwrap();
            operation
        };
        let old = connect();
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .unwrap()
            .0;
        let request = sessions
            .capability_request(&session.session_ref, &lease)
            .unwrap();
        let payload = json!({ "v":1,"type":"capability_snapshot_error", "connection_id":lease.connection_id,
            "controller_lease_id":lease.controller_lease_id,"lease_epoch":lease.lease_epoch,"relay_epoch":lease.relay_epoch,
            "remote_control_generation":lease.remote_control_generation,"request_id":request.request_id,"session_id":session.session_id,
            "code":"capability_snapshot_unavailable" });
        let business = ControllingLaneState::default();
        business.install(HandshakeRole::A, &lease, a_keys).unwrap();
        let mut b = EncryptedLane::new(HandshakeRole::B, &lease, b_keys).unwrap();
        let encrypted = b
            .seal("capability_snapshot_error", &payload, &capabilities)
            .unwrap();
        let (kind, opened) = manager
            .with_lane_owner(&old, || business.open(&encrypted, &capabilities))
            .unwrap()
            .unwrap();
        // The old frame passed AEAD before the singleton was replaced. The
        // intent/request deliberately remain pending with identical lease bytes.
        manager.disconnect();
        let _new = connect();
        let mut sinks = ControllingLaneSinks {
            on_status: &mut |_| {},
            on_session: &mut |_| panic!("stale capability error projected"),
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut |_, _| {},
            on_journal: &mut |_| Ok(()),
            on_snapshot: &mut |_| Ok(()),
            on_liveness: &mut |_| false,
        };
        dispatch_authenticated_payload(
            &manager,
            &old,
            &sessions,
            &business,
            &mut JournalExpectations::default(),
            &lease,
            &capabilities,
            &kind,
            opened,
            &mut sinks,
        );
        let pending = sessions
            .fail_pending_capability_for_lease(&lease, "capability_snapshot_unavailable")
            .unwrap();
        assert_eq!(
            pending.capability_request_id.as_deref(),
            Some(request.request_id.as_str())
        );
    }

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

        struct TerminalPort;

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
        let responses = dispatch_decrypted_payload(&TerminalPort, &lease, &kind, &payload)
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

        // A：保活任务解密并投影到任务状态域。
        let mut lane = TestLane {
            events: VecDeque::from(vec![
                ControllerLaneEvent::Frame(response_frame.to_string()),
                ControllerLaneEvent::Frame(journal_frame.to_string()),
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
        struct PendingLane;

        #[async_trait::async_trait]
        impl ControllingLane for PendingLane {
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
                &mut PendingLane,
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
        struct IpcLane(mpsc::UnboundedReceiver<ControllerLaneEvent>);
        #[async_trait::async_trait]
        impl ControllingLane for IpcLane {
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
        let mut lane = IpcLane(rx);
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
        struct PendingLane;
        #[async_trait::async_trait]
        impl ControllingLane for PendingLane {
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
        let mut calls = 0;
        let mut replacement = None;
        let mut on_liveness = |frame: Value| {
            calls += 1;
            assert_eq!(frame["type"], "controller_liveness");
            assert_eq!(frame["remote_control_generation"], "generation-old");
            manager.disconnect();
            replacement = Some(connect(&manager, "generation-new"));
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
            &mut PendingLane,
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
            &mut PendingLane,
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
