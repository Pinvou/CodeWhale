//! Production glue between the Windows controlled-device owner and Relay.
//!
//! E2E-01 owns pairing registration: B reuses the authenticated desktop
//! endpoint socket, sends one generation-bound registration and validates
//! Relay's sealed result. E2E-02 adds the对称握手接线: the same channel takes
//! over from `controller_pairing_reserved`, runs the commitment/Finished
//! exchange and applies `controller_joined` to the device owner.

use std::sync::OnceLock;
use std::time::Instant;

use base64::Engine as _;
use serde_json::{Value, json};
use tauri::{AppHandle, Emitter, Manager as _};
use tokio::sync::oneshot;

use crate::features::remote_control::manager::RemoteControlManager;

use super::controlled::{
    ControlledDeviceManager, ControlledLease, ControlledOperation, ControlledRegistration,
    ControlledResumeStep, ControlledStatus,
};
use super::controlled_handshake::ControlledHandshake;
use super::controlling_handshake::JoinedLease;
use super::crypto::{
    AuthenticatedRelayTrustConfig, CodeWrapBinding, CodeWrapKeyPair,
    ControllerRole as HandshakeRole, HandshakeKeyPair, SessionKeys, load_relay_trust_config,
    transport_key_fingerprint,
};
use super::encrypted_lane::ControlledLaneState;
use super::protocol::LeaseContext;
use super::protocol::{
    ROLE_B, RegistrationValidationContext, SenderRole, is_self_echoed_handshake_frame,
    validate_controller_message_json, validate_controller_registration_message_json,
};
use super::resume::{
    build_controller_post_activation_liveness, build_controller_resume_activation_ack,
    build_controller_resume_prepare, build_controller_resume_ready_ack, new_resume_challenge,
    resume_frame_matches_lease,
};

const REGISTRATION_TIMEOUT_MS: u64 = 30_000;
const MAX_PAIRING_CODE_TTL_MS: u64 = 5 * 60_000;

// Relay signs a wall-clock expiry, whereas the B lifecycle owner and UI
// countdown use one local monotonic clock. Convert once at receipt; never
// expose either absolute clock origin to the WebView.
fn pairing_deadline_b(wire_expiry_ms: u64, wall_now_ms: u64, now_b: u64) -> Result<u64, String> {
    let remaining = wire_expiry_ms
        .checked_sub(wall_now_ms)
        .filter(|remaining| *remaining > 0)
        .ok_or_else(|| "pairing_code_expired".to_owned())?
        .min(MAX_PAIRING_CODE_TTL_MS);
    now_b
        .checked_add(remaining)
        .ok_or_else(|| "pairing_registration_invalid".to_owned())
}

fn wall_clock_millis() -> Result<u64, String> {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .ok()
        .and_then(|elapsed| u64::try_from(elapsed.as_millis()).ok())
        .ok_or_else(|| "pairing_clock_invalid".to_owned())
}

// Keep the transport's existing host projection at one narrow callsite.
// The controlled state owner constructs each status before it is published.
fn emit_controlled_status(app: &AppHandle, status: &ControlledStatus) {
    let _ = app.emit(CONTROLLED_STATUS_EVENT, status);
}
/// 端点通道就绪等待：单次上限与重试间隔（整体最多 3 次尝试）。
const ENDPOINT_READY_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(60);
const ENDPOINT_READY_RETRY_DELAY: std::time::Duration = std::time::Duration::from_secs(2);

/// 双端共用的排障日志写入器（追加写入 `~/.pinvou3/logs/remote-control.log`）。
///
/// `scope` 只区分端别：`controlled`（B 端）与 `controller`（A 端），便于同一台机器
/// 或同一份日志里对照同一次连接的阶段。只在「启停/注册/握手/连接结果」这些用户可
/// 感知的失败与成功节点写一行有界文本：不含占位符之外的任何标识（不写
/// endpoint_id/lease/token/连接码），便于用户把文件直接发给支持而不担心泄漏。
/// 写失败一律忽略，绝不影响主流程。
pub(crate) fn log_relay_event(scope: &str, stage: &str, detail: &str) {
    use std::io::Write as _;
    static LOG_WRITE_LOCK: OnceLock<parking_lot::Mutex<()>> = OnceLock::new();

    // Several transport/task callbacks can report transitions concurrently.
    // Serialize the complete append so two records cannot be spliced together.
    let _write_guard = LOG_WRITE_LOCK
        .get_or_init(|| parking_lot::Mutex::new(()))
        .lock();
    let timestamp_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|duration| duration.as_millis())
        .unwrap_or(0);
    let dir = crate::platform::paths::pinvou3_home().join("logs");
    if std::fs::create_dir_all(&dir).is_err() {
        return;
    }
    let Ok(mut file) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(dir.join("remote-control.log"))
    else {
        return;
    };
    let _ = writeln!(
        file,
        "ts_ms={timestamp_ms} [{scope}] stage={} detail={}",
        bounded_log_token(stage),
        bounded_log_token(detail)
    );
}

/// 有界化按位洗白：只保留可以安全落盘的字符集并限制长度，
/// 空白、`=`、`?`、`&` 等分隔符一律丢掉，避免把连接码或凭据片段带进日志。
pub(crate) fn bounded_log_token(value: &str) -> String {
    value
        .chars()
        .filter(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-' | ':' | '.' | '/'))
        .take(160)
        .collect()
}

/// B 端预握手阶段的自帧回投过滤：Relay 会把发送方的 `finished_receipt_commit`
/// 原样回投，B 只处理 `sender_role == A` 的对端帧，自己的回投必须跳过。
pub(crate) fn skip_self_echoed_frame(message: &Value) -> bool {
    is_self_echoed_handshake_frame(message, ROLE_B)
}

/// B 端（被控端）排障日志。
pub(crate) fn log_controlled_event(stage: &str, detail: &str) {
    log_relay_event("controlled", stage, detail);
}

/// A 端（控制端）排障日志。A 端此前没有任何落盘痕迹，导致「连接码无效或已过期」
/// 这类失败只能靠猜；这里补上与 B 端对称的阶段与结果码。
pub(crate) fn log_controller_event(stage: &str, detail: &str) {
    log_relay_event("controller", stage, detail);
}

/// 测试/内联断言用：日志行格式（不含时间戳，便于跨平台断言）。
pub(crate) fn relay_log_line(scope: &str, stage: &str, detail: &str) -> String {
    format!("[{scope}] stage={stage} detail={detail}")
}

/// 测试/内联断言用：B 端日志行格式。
pub(crate) fn controlled_log_line(stage: &str, detail: &str) -> String {
    relay_log_line("controlled", stage, detail)
}

/// 测试/内联断言用：A 端日志行格式。
pub(crate) fn controller_log_line(stage: &str, detail: &str) -> String {
    relay_log_line("controller", stage, detail)
}
const REGISTRATION_CAPABILITIES: &[&str] = &[
    "pairing_v1",
    "encrypted_frames_v1",
    "finished_receipt_v1",
    // E2E-03：B 端的权威 session / 任务台账（DEV-12/DEV-13）已实现，注册时
    // 一并声明 task_v1，使 A 的能力快照请求与任务请求能通过协商。
    "task_v1",
    "temporary_model_package_v1",
    // E2E-05：B 的权威 journal（DEV-19）已实现，声明 snapshot_journal_v1 使
    // journal_event / journal_replay 等正文事件能通过协商。
    "snapshot_journal_v1",
    // E2E-06：B 的 liveness watchdog（DEV-08）已实现，声明 role_liveness_v1 让
    // A 端心跳帧与后续 challenge/attestation 能通过协商。
    "role_liveness_v1",
    // E2E-06：B 的 resume 状态机（Relay controller-resume hooks 与本地
    // ControllerResumeGate）已可运行，声明 resume_v1 让双端协商交集包含恢复族；
    // 否则 A 端声明的 resume_v1 会被交集运算静默剔除。
    "resume_v1",
];

/// 注册声明的能力集合（供「A claim 与 B 注册同族」的一致性测试使用）。
pub(crate) fn registration_capabilities() -> &'static [&'static str] {
    REGISTRATION_CAPABILITIES
}
const CONTROLLED_STATUS_EVENT: &str = "desktop-remote-controlled-status";
/// 握手/协议层失败的本地错误码（有界，绝不透传对端文本）。
const HANDSHAKE_FAILED: &str = "invalid_message";
/// 部署期没有可用的 Relay 信任根：没有它就无法验签 transcript，必须失败关闭。
const RELAY_TRUST_UNCONFIGURED: &str = "relay_trust_unconfigured";

/// B 端单调时钟。注册、握手、watchdog 与状态投影必须共用同一个原点，否则
/// deadline 会由不同基准推导。
pub(crate) fn controlled_monotonic_millis() -> u64 {
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

pub(crate) struct ControlledRelayWorker {
    app: AppHandle,
    relay: RemoteControlManager,
    operation: ControlledOperation,
    code_wrap: CodeWrapKeyPair,
    /// 注册阶段声明、握手接管时一次性移交给 `ControlledHandshakeDriver`。
    handshake: Option<HandshakeKeyPair>,
    channel: crate::features::remote_control::manager::ControllerRelayChannel,
    /// 业务派发端口：由命令层注入（能力快照 / 任务接纳 / 取消）。
    business: std::sync::Arc<dyn ControlledBusinessPort>,
}

/// Narrow port over the generation-bound Relay lane. Keeps the registration
/// wait loop testable without a Tauri app handle, and is reused by the later
/// handshake/business dispatch tasks.
#[async_trait::async_trait]
pub(crate) trait ControlledRelayPort: Send {
    fn try_send(&self, message: Value) -> Result<(), String>;
    async fn recv(&mut self) -> Option<Value>;
}

#[async_trait::async_trait]
impl ControlledRelayPort for crate::features::remote_control::manager::ControllerRelayChannel {
    fn try_send(&self, message: Value) -> Result<(), String> {
        crate::features::remote_control::manager::ControllerRelayChannel::try_send(self, message)
    }

    async fn recv(&mut self) -> Option<Value> {
        crate::features::remote_control::manager::ControllerRelayChannel::recv(self).await
    }
}

/// `controller_joined` 之后的租约材料：租约身份来自 Relay，传输指纹由双方
/// 认证过的 transcript 派生。
#[derive(Debug)]
pub(crate) struct ControlledHandshakeOutcome {
    pub(crate) lease: JoinedLease,
    pub(crate) transcript_hash: [u8; 32],
    pub(crate) negotiated_capabilities: Vec<String>,
}

/// B 端握手推进器（E2E-02 第 3 步）：不含网络、不含 Tauri，只按冻结顺序把
/// 入站报文喂给 `ControlledHandshake`，并把要发送的下一帧交给调用方。
pub(crate) struct ControlledHandshakeDriver {
    keys: Option<HandshakeKeyPair>,
    handshake: Option<ControlledHandshake>,
}

impl ControlledHandshakeDriver {
    /// 注册阶段生成的握手密钥在握手接管时移交给 driver（一次性）。
    pub(crate) fn new(keys: HandshakeKeyPair) -> Self {
        Self {
            keys: Some(keys),
            handshake: None,
        }
    }

    pub(crate) fn is_active(&self) -> bool {
        self.handshake.is_some()
    }

    /// `controller_pairing_reserved`：验签 transcript、派生会话密钥与 receipt，
    /// 产出本端 `finished_receipt_commit`。未验签或重复接管都失败关闭。
    pub(crate) fn accept_reserved(
        &mut self,
        message: &Value,
        trust: &AuthenticatedRelayTrustConfig,
    ) -> Result<Value, &'static str> {
        if self.handshake.is_some() {
            return Err("controller_handshake_stage_invalid");
        }
        let keys = self.keys.take().ok_or("controller_handshake_key_invalid")?;
        let handshake =
            ControlledHandshake::new(message, keys, trust).map_err(|error| error.code)?;
        let commit = handshake.commit().map_err(|error| error.code)?;
        self.handshake = Some(handshake);
        Ok(commit)
    }

    /// `finished_receipt_commit`：记录对端 commitment 后产出本端 Finished。
    pub(crate) fn accept_peer_commitment(
        &mut self,
        message: &Value,
    ) -> Result<Value, &'static str> {
        let handshake = self
            .handshake
            .as_mut()
            .ok_or("controller_handshake_stage_invalid")?;
        handshake
            .accept_peer_commitment(message)
            .map_err(|error| error.code)?;
        handshake.finished().map_err(|error| error.code)
    }

    /// `encrypted_handshake_finished`：打开对端 Finished 并核对 commitment 后
    /// 产出本端 `finished_receipt_ack`。
    pub(crate) fn accept_peer_finished(&mut self, message: &Value) -> Result<Value, &'static str> {
        let handshake = self
            .handshake
            .as_mut()
            .ok_or("controller_handshake_stage_invalid")?;
        handshake
            .accept_peer_finished(message)
            .map_err(|error| error.code)
    }

    /// `controller_joined`：只有双收据完成且绑定与验签 transcript 完全一致时
    /// 才产出可安装的租约材料。
    pub(crate) fn accept_joined(
        &mut self,
        message: &Value,
    ) -> Result<ControlledHandshakeOutcome, &'static str> {
        let handshake = self
            .handshake
            .as_mut()
            .ok_or("controller_handshake_stage_invalid")?;
        let lease = handshake
            .accept_joined(message)
            .map_err(|error| error.code)?;
        Ok(ControlledHandshakeOutcome {
            lease,
            transcript_hash: handshake.transcript_hash(),
            negotiated_capabilities: handshake.negotiated_capabilities().to_vec(),
        })
    }

    /// 双收据完成后取出会话密钥（用于安装加密业务通道）；只允许成功一次。
    pub(crate) fn take_session(&mut self) -> Result<SessionKeys, &'static str> {
        let handshake = self
            .handshake
            .as_mut()
            .ok_or("controller_handshake_stage_invalid")?;
        handshake.take_session().map_err(|error| error.code)
    }
}

/// Waits for the registration response for `request`. Unrelated controller
/// frames are consumed without extending the attempt's absolute deadline.
pub(crate) async fn wait_for_registration<P: ControlledRelayPort>(
    port: &mut P,
    request: &Value,
    code_wrap: &CodeWrapKeyPair,
    timeout: std::time::Duration,
) -> Result<ControlledRegistration, String> {
    let deadline = tokio::time::sleep(timeout);
    tokio::pin!(deadline);
    loop {
        tokio::select! {
            biased;
            _ = &mut deadline => return Err("pairing_registration_timeout".to_owned()),
            message = port.recv() => {
                let Some(message) = message else {
                    return Err("desktop_relay_disconnected".to_owned());
                };
                let kind = message.get("type").and_then(Value::as_str);
                match kind {
                    Some("desktop_relay_connection") => {
                        if message.get("connected").and_then(Value::as_bool) == Some(false) {
                            return Err("desktop_relay_disconnected".to_owned());
                        }
                        continue;
                    }
                    Some("desktop_endpoint_registered") => {
                        continue;
                    }
                    Some("desktop_pairing_registered") | Some("error") => {}
                    // The controller lane also carries transport and later-phase
                    // control frames. They are not registration responses and
                    // must never be fed to the registration validator.
                    _ => continue,
                }
                // relay 的拒绝必须保留原始有界码，便于定位（此前被映射成
                // pairing_registration_rejected 后丢失了原因）。
                if kind == Some("error") {
                    let code = message
                        .get("code")
                        .and_then(Value::as_str)
                        .unwrap_or("relay_error");
                    // The shared Relay uses v2 for its outer Web-access lane and
                    // controller-v1 for desktop control. A v2 version rejection
                    // for our v1 registration means the controller dispatcher is
                    // not enabled on that Relay; surface the deployment fault
                    // instead of collapsing it into a generic registration error.
                    if message.get("v").and_then(Value::as_u64) == Some(2)
                        && matches!(
                            code,
                            "unsupported_protocol_version" | "desktop_controller_unavailable"
                        )
                    {
                        log_controlled_event("registration_rejected", code);
                        return Err("desktop_controller_relay_unavailable".to_owned());
                    }
                    // Errors from connection setup or another in-flight request
                    // share this lane but are not replies to this registration.
                    if message.get("request_id") != request.get("request_id") {
                        continue;
                    }
                    log_controlled_event("registration_rejected", code);
                }
                return registration_from_response(code_wrap, request, &message);
            }
        }
    }
}

impl ControlledRelayWorker {
    pub(crate) async fn start(
        app: AppHandle,
        relay: RemoteControlManager,
        operation: ControlledOperation,
        business: std::sync::Arc<dyn ControlledBusinessPort>,
    ) -> Result<oneshot::Receiver<Result<ControlledStatus, String>>, String> {
        let code_wrap = CodeWrapKeyPair::generate().map_err(|_| "pairing_key_invalid")?;
        let handshake = HandshakeKeyPair::generate().map_err(|_| "handshake_key_invalid")?;
        let channel = relay.start_controller_relay(operation.generation()).await?;
        log_controlled_event("worker_started", "channel_ready");
        let (done_tx, done_rx) = oneshot::channel();
        let mut worker = Self {
            app,
            relay,
            operation,
            code_wrap,
            handshake: Some(handshake),
            channel,
            business,
        };
        tauri::async_runtime::spawn(async move {
            let result = worker.register_pairing().await;
            if let Err(error) = &result {
                worker.fail_registration(error);
            }
            let _ = done_tx.send(result);
            // Keep the generation-bound receiver alive after registration. Later
            // E2E tasks install handshake and encrypted business dispatch here.
            worker.serve().await;
        });
        Ok(done_rx)
    }

    async fn register_pairing(&mut self) -> Result<ControlledStatus, String> {
        let manager = self.app.state::<ControlledDeviceManager>();
        let registration_request_id = manager
            .registration_request_id(self.operation.clone())
            .map_err(|error| error.code.to_owned())?;
        // 注册帧必须在端点 socket 已认证为 B 之后发出，否则会被 relay 以角色无效
        // 拒绝；同时网络抖动可能让端点通道 20 秒内都没就绪，因此整体**有界重试**：
        // 每次重试复用同一个 registration_request_id，relay 侧按幂等键返回同一结果。
        const MAX_ATTEMPTS: u32 = 3;
        for attempt in 1..=MAX_ATTEMPTS {
            match self.relay.wait_endpoint_ready(ENDPOINT_READY_TIMEOUT).await {
                Ok(()) => log_controlled_event("endpoint_ready", "ok"),
                Err(error) => {
                    log_controlled_event("endpoint_ready", &error);
                    if attempt < MAX_ATTEMPTS {
                        tokio::time::sleep(ENDPOINT_READY_RETRY_DELAY).await;
                        continue;
                    }
                    return Err(error);
                }
            }
            let endpoint_id = self.relay.controller_endpoint_id()?;
            let request = registration_request(
                &registration_request_id,
                &endpoint_id,
                self.operation.generation(),
                &self.code_wrap,
                self.handshake
                    .as_ref()
                    .ok_or_else(|| "handshake_key_invalid".to_owned())?,
            )?;
            self.channel.try_send(request.clone())?;
            // 区分「连注册帧都没发出去」与「发出后等待/被拒」这两类失败。
            log_controlled_event("registration_sent", "waiting_registered");
            match wait_for_registration(
                &mut self.channel,
                &request,
                &self.code_wrap,
                std::time::Duration::from_millis(REGISTRATION_TIMEOUT_MS),
            )
            .await
            {
                Ok(mut registration) => {
                    let now_b = controlled_monotonic_millis();
                    registration.expires_at_ms = pairing_deadline_b(
                        registration.expires_at_ms,
                        wall_clock_millis()?,
                        now_b,
                    )?;
                    let status = manager
                        .complete_registration(self.operation.clone(), registration, now_b)
                        .map_err(|error| error.code.to_owned())?;
                    log_controlled_event("registered", "waiting_for_pairing");
                    emit_controlled_status(&self.app, &status);
                    return Ok(status);
                }
                Err(error) => {
                    log_controlled_event("registration_failed", &error);
                    let retryable = matches!(
                        error.as_str(),
                        "pairing_registration_timeout" | "desktop_relay_disconnected"
                    );
                    if retryable && attempt < MAX_ATTEMPTS {
                        tokio::time::sleep(ENDPOINT_READY_RETRY_DELAY).await;
                        continue;
                    }
                    return Err(error);
                }
            }
        }
        Err("pairing_registration_failed".to_owned())
    }

    fn fail_registration(&self, error: &str) {
        log_controlled_event("registration_failed", &sanitize_error(error));
        let manager = self.app.state::<ControlledDeviceManager>();
        if !manager.is_pending_handshake(self.operation.clone()) {
            // 用户已停止/断开，或本代已被刷新取代：不覆盖既有投影。
            return;
        }
        let bounded = sanitize_error(error);
        if let Ok(status) = manager.fail_operation(self.operation.clone(), bounded) {
            emit_controlled_status(&self.app, &status);
        }
    }

    /// E2E-02 第 3 步：`controller_pairing_reserved` 到达后用
    /// `ControlledHandshake` 接管，按冻结顺序完成双收据交换，并把
    /// `controller_joined` 应用到设备所有者。
    ///
    /// 任何顺序错误、绑定不符或验签失败都失败关闭（投影为有界错误的 error
    /// 状态并结束本代通道）；握手完成后的帧只做排空，业务帧派发属 E2E-04，
    /// 未知帧绝不转发到 legacy WebUI 事件流。
    async fn serve(mut self) {
        let Some(keys) = self.handshake.take() else {
            self.fail_handshake(HANDSHAKE_FAILED);
            return;
        };
        let mut driver = ControlledHandshakeDriver::new(keys);
        let mut joined = false;
        let mut evidence = LivenessEvidenceState::default();
        let mut resume = ControlledResumeAttempt::default();
        let mut poll =
            tokio::time::interval(std::time::Duration::from_millis(LIVENESS_EVIDENCE_POLL_MS));
        poll.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        poll.tick().await;
        'serve: loop {
            tokio::select! {
                biased;
                _ = poll.tick() => {
                    if joined && !resume.is_active() {
                        self.pump_liveness_evidence(&mut evidence);
                    }
                    continue 'serve;
                }
                message = self.channel.recv() => {
                    let Some(message) = message else { break 'serve };
                    if message.get("type").and_then(Value::as_str) == Some("desktop_controller_stop") {
                        break 'serve;
                    }
                    if joined {
                        // E2E-06：恢复进行中时所有帧都由恢复状态机处理，业务帧与
                        // 存活证据一律不派发（Relay 侧同样禁止业务帧）。
                        if resume.is_active() {
                            let manager = self.app.state::<ControlledDeviceManager>();
                            let lane = self.app.state::<ControlledLaneState>();
                            match handle_controlled_resume_frame(
                                &self.channel,
                                manager.inner(),
                                lane.inner(),
                                &mut resume,
                                &message,
                            ) {
                                ControlledResumeOutcome::Failed => break 'serve,
                                ControlledResumeOutcome::Completed
                                | ControlledResumeOutcome::Progressed => {
                                    let status = manager.poll_watchdog(controlled_monotonic_millis());
                                    emit_controlled_status(&self.app, &status);
                                }
                                ControlledResumeOutcome::Ignored => {}
                            }
                            continue 'serve;
                        }
                        match message.get("type").and_then(Value::as_str) {
                            Some("encrypted_frame") => {
                                self.dispatch_business_frame(&message).await;
                            }
                            // E2E-06：Relay 签名兜底证明 → 续期 B 的 watchdog。
                            Some("controller_liveness_attestation_a") => {
                                self.accept_liveness_attestation(&message, &mut evidence);
                            }
                            // E2E-06：A 的直接存活证明 → 优先于兜底，续期后不再升级。
                            Some("encrypted_controller_liveness_a") => {
                                self.accept_direct_liveness(&message, &mut evidence);
                            }
                            Some("error") => {
                                let code = message
                                    .get("code")
                                    .and_then(Value::as_str)
                                    .unwrap_or("relay_error");
                                log_controlled_event("runtime_relay_error", code);
                            }
                            // E2E-06：A 端显式断开 → 终结租约并结束本代通道。
                            Some("controller_disconnect") => {
                                if self.accept_remote_disconnect(&message) {
                                    break 'serve;
                                }
                            }
                            // E2E-06：desktop socket 掉线 → 挂起业务通道并启动恢复。
                            Some("desktop_relay_connection") => {
                                let connected = message
                                    .get("connected")
                                    .and_then(Value::as_bool)
                                    .unwrap_or(true);
                                if !connected {
                                    let manager = self.app.state::<ControlledDeviceManager>();
                                    let lane = self.app.state::<ControlledLaneState>();
                                    lane.suspend();
                                    if !begin_controlled_resume(
                                        &self.channel,
                                        manager.inner(),
                                        &mut resume,
                                    ) {
                                        break 'serve;
                                    }
                                }
                            }
                            _ => {}
                        }
                        continue 'serve;
                    }
                    // Relay 会把本端 commit 原样回投（router 契约，见
                    // `controller-router.test.js`）。自帧回投不是对端输入，跳过而不是
                    // 失败关闭：否则 B 会在收到自己 commitment 的瞬间以
                    // `controller_handshake_rejected` 结束整条通道（2026-09-20 真机）。
                    if skip_self_echoed_frame(&message) {
                        continue 'serve;
                    }
                    let result = match message.get("type").and_then(Value::as_str) {
                        Some("controller_pairing_reserved") => {
                            self.begin_handshake(&message, &mut driver)
                        }
                        Some("finished_receipt_commit") => driver
                            .accept_peer_commitment(&message)
                            .map(Some)
                            .map_err(str::to_owned),
                        Some("encrypted_handshake_finished") => driver
                            .accept_peer_finished(&message)
                            .map(Some)
                            .map_err(str::to_owned),
                        Some("controller_joined") => {
                            match self.complete_handshake(&message, &mut driver) {
                                Ok(()) => {
                                    joined = true;
                                    Ok(None)
                                }
                                Err(code) => Err(code),
                            }
                        }
                        _ => Ok(None),
                    };
                    match result {
                        Ok(Some(frame)) => {
                            if let Err(error) = self.channel.try_send(frame) {
                                self.fail_handshake(&error);
                                return;
                            }
                            if message.get("type").and_then(Value::as_str)
                                == Some("controller_pairing_reserved")
                            {
                                let manager = self.app.state::<ControlledDeviceManager>();
                                match manager.mark_handshake_pending(self.operation.clone()) {
                                    Ok(status) => {
                                        emit_controlled_status(&self.app, &status);
                                    }
                                    Err(error) => {
                                        self.fail_handshake(error.code);
                                        return;
                                    }
                                }
                            }
                        }
                        Ok(None) => {}
                        Err(code) => {
                            self.fail_handshake(&code);
                            return;
                        }
                    }
                }
            }
        }
        // 通道关闭（stop/refresh/断线）时丢弃出站旁路，避免旧 generation 继续回传。
        self.app
            .state::<ControlledOutboundState>()
            .clear_generation(self.channel.generation());
    }

    /// E2E-06：按 watchdog 需求发起挑战 / 升级到 Relay 签名兜底 / 作废超时挑战。
    fn pump_liveness_evidence(&self, evidence: &mut LivenessEvidenceState) {
        let manager = self.app.state::<ControlledDeviceManager>();
        let now_b = controlled_monotonic_millis();
        let needs = manager.liveness_evidence_needed(now_b);
        match liveness_evidence_action(needs, evidence, now_b) {
            LivenessEvidenceAction::Idle => {}
            LivenessEvidenceAction::Challenge => {
                let Ok((lease, _)) = manager.liveness_runtime_context() else {
                    return;
                };
                let Ok((challenge_id, challenge_b64)) = manager.begin_liveness_challenge(now_b)
                else {
                    return;
                };
                let request_id = format!("request-liveness-{}", uuid::Uuid::new_v4().simple());
                let Ok(frame) = super::liveness::build_liveness_challenge(
                    &lease,
                    &request_id,
                    &challenge_id,
                    &challenge_b64,
                ) else {
                    manager.cancel_liveness_challenge(&challenge_id);
                    return;
                };
                if self.channel.try_send(frame).is_ok() {
                    evidence.outstanding = Some((challenge_id, challenge_b64, now_b));
                    evidence.escalated = false;
                    log_controlled_event("liveness_challenge", "sent");
                } else {
                    manager.cancel_liveness_challenge(&challenge_id);
                    log_controlled_event("liveness_challenge", "send_failed");
                }
            }
            LivenessEvidenceAction::Escalate => {
                let Ok((lease, _)) = manager.liveness_runtime_context() else {
                    return;
                };
                let Some((challenge_id, challenge_b64, _)) = evidence.outstanding.clone() else {
                    return;
                };
                let request_id = format!("request-attest-{}", uuid::Uuid::new_v4().simple());
                let Ok(frame) = super::liveness::build_liveness_attestation_request(
                    &lease,
                    &request_id,
                    &challenge_id,
                    &challenge_b64,
                ) else {
                    return;
                };
                if self.channel.try_send(frame).is_ok() {
                    evidence.escalated = true;
                    log_controlled_event("liveness_attestation", "requested");
                }
            }
            LivenessEvidenceAction::Reset => {
                if let Some((challenge_id, _, _)) = evidence.outstanding.take() {
                    manager.cancel_liveness_challenge(&challenge_id);
                }
                evidence.escalated = false;
                log_controlled_event("liveness_challenge", "expired");
            }
        }
    }

    /// E2E-06：A 端显式断开（`controller_disconnect`）。返回 true 表示已被接受，
    /// 本代通道应当结束（租约、会话密钥与远端任务都已清理）。
    fn accept_remote_disconnect(&self, message: &Value) -> bool {
        // 先做冻结校验（角色 A + 租约 + 能力），再交给状态所有者终结租约。
        let manager = self.app.state::<ControlledDeviceManager>();
        let Ok((lease, capabilities)) = manager.live_runtime_context() else {
            return false;
        };
        let context =
            crate::features::remote_control::controller::protocol::RuntimeValidationContext {
                lease: Some(lease),
                negotiated_capabilities: capabilities,
            };
        let validated = crate::features::remote_control::controller::protocol::validate_controller_runtime_message_json(
            &message.to_string(),
            crate::features::remote_control::controller::protocol::SenderRole::ControllerA,
            Some(&context),
        );
        if validated.is_err() {
            return false;
        }
        match manager.accept_remote_disconnect() {
            Ok(status) => {
                self.app.state::<ControlledLaneState>().clear();
                self.app.state::<ControlledOutboundState>().clear();
                emit_controlled_status(&self.app, &status);
                true
            }
            Err(_) => false,
        }
    }

    /// E2E-06：接受 Relay 签名 attestation 续期本端 watchdog（挑战与请求时间
    /// 由本端记录，跨机时间一律不用）。
    /// E2E-06：A 的直接存活证明（`encrypted_controller_liveness_a`）：解密后用
    /// 明文走 `accept_direct_liveness`（挑战、RTT 与序号单调性由 watchdog 校验），
    /// 成功后清除未完成挑战，避免再走 Relay 兜底。
    fn accept_direct_liveness(&self, frame: &Value, evidence: &mut LivenessEvidenceState) {
        let manager = self.app.state::<ControlledDeviceManager>();
        let business = self.app.state::<ControlledLaneState>();
        let Ok(inner) = business.open_liveness(frame) else {
            return;
        };
        let Some(challenge_id) = inner.get("challenge_id").and_then(Value::as_str) else {
            return;
        };
        match &evidence.outstanding {
            Some((outstanding, _, _)) if outstanding == challenge_id => {}
            // 不是当前挑战的证明（重放或过期）一律忽略。
            _ => return,
        }
        let now_b = controlled_monotonic_millis();
        match manager.accept_direct_liveness(&inner.to_string(), now_b) {
            Ok(()) => {
                evidence.outstanding = None;
                evidence.escalated = false;
                log_controlled_event("liveness_direct", "accepted");
            }
            Err(error) => log_controlled_event("liveness_direct", error.code),
        }
    }

    /// E2E-06：接受 Relay 签名 attestation 续期本端 watchdog（挑战与请求时间
    /// 由本端记录，跨机时间一律不用）。
    fn accept_liveness_attestation(&self, message: &Value, evidence: &mut LivenessEvidenceState) {
        let Some((challenge_id, _, sent_at_b)) = evidence.outstanding.clone() else {
            return;
        };
        if message.get("challenge_id").and_then(Value::as_str) != Some(challenge_id.as_str()) {
            return;
        }
        let manager = self.app.state::<ControlledDeviceManager>();
        let now_b = controlled_monotonic_millis();
        match manager.accept_liveness_attestation(&message.to_string(), sent_at_b, now_b) {
            Ok(()) => {
                evidence.outstanding = None;
                evidence.escalated = false;
                log_controlled_event("liveness_attestation", "accepted");
            }
            Err(error) => log_controlled_event("liveness_attestation", error.code),
        }
    }

    /// 接管握手：本地进入安全握手阶段并产出本端 commit。信任根缺失或非法时
    /// 失败关闭，绝不降级为无认证握手。
    fn begin_handshake(
        &self,
        message: &Value,
        driver: &mut ControlledHandshakeDriver,
    ) -> Result<Option<Value>, String> {
        let trust = load_relay_trust_config(|name| std::env::var(name).ok())
            .map_err(|_| RELAY_TRUST_UNCONFIGURED.to_owned())?;
        let manager = self.app.state::<ControlledDeviceManager>();
        let commit = driver
            .accept_reserved(message, &trust)
            .map_err(str::to_owned)?;
        let status = manager
            .mark_pairing_reserved(self.operation.clone())
            .map_err(|error| error.code.to_owned())?;
        emit_controlled_status(&self.app, &status);
        Ok(Some(commit))
    }

    /// 应用 `controller_joined`：构造 B 端租约（含双方一致的传输指纹）并进入
    /// connected。租约未真正落地前不投影 connected。
    fn complete_handshake(
        &self,
        message: &Value,
        driver: &mut ControlledHandshakeDriver,
    ) -> Result<(), String> {
        let outcome = driver
            .accept_joined(message)
            .map_err(|code| code.to_owned())?;
        let lease = controlled_lease_from_outcome(outcome, controlled_monotonic_millis())?;
        let manager = self.app.state::<ControlledDeviceManager>();
        // 加密业务通道先于 connected 就位：状态一旦是 connected，业务帧既有租约
        // 准入也有会话密钥可用。
        let Ok(session) = driver.take_session() else {
            return Err(self.fail_completed_handshake(&manager));
        };
        let business = self.app.state::<ControlledLaneState>();
        business.clear();
        if business
            .install(super::crypto::ControllerRole::B, &lease.context, session)
            .is_err()
        {
            return Err(self.fail_completed_handshake(&manager));
        }
        // 出站旁路（journal 事件回传）与通道一起就位。
        let outbound = self.app.state::<ControlledOutboundState>();
        outbound.install(self.channel.sender());
        let status = manager
            .complete_connected(self.operation.clone(), lease)
            .map_err(|error| error.code.to_owned())?;
        emit_controlled_status(&self.app, &status);
        Ok(())
    }

    /// 握手成功后本地收尾失败（无权安装会话密钥/状态竞争）：失败关闭并返回
    /// 有界错误码，绝不留下「connected 但无业务通道」的状态。
    fn fail_completed_handshake(&self, manager: &ControlledDeviceManager) -> String {
        let business = self.app.state::<ControlledLaneState>();
        business.clear();
        if let Ok(status) =
            manager.fail_operation(self.operation.clone(), HANDSHAKE_FAILED.to_owned())
        {
            emit_controlled_status(&self.app, &status);
        }
        HANDSHAKE_FAILED.to_owned()
    }

    /// 握手失败收尾：投影为有界错误并结束本代通道（dropping the channel 会
    /// 清掉该 generation 的通道绑定，本地停止/刷新入口保持可用）。
    fn fail_handshake(&self, error: &str) {
        log_controlled_event("handshake_failed", &sanitize_error(error));
        let manager = self.app.state::<ControlledDeviceManager>();
        if !manager.is_pending_handshake(self.operation.clone()) {
            // 用户已停止/断开，或本代已被刷新取代：不覆盖既有投影。
            return;
        }
        let bounded = sanitize_error(error);
        if let Ok(status) = manager.fail_operation(self.operation.clone(), bounded) {
            emit_controlled_status(&self.app, &status);
        }
    }

    /// 已认证业务帧：解密 → 派发 → 用同一通道加密回包。任何一层不符（无活租约、
    /// 解密/校验失败、业务拒绝）都不产生副作用；业务拒绝时回传有界错误的
    /// `task_status` 由命令层决定（这里只负责通道语义）。
    async fn dispatch_business_frame(&self, frame: &Value) {
        let is_task_submit =
            frame.get("payload_kind").and_then(Value::as_str) == Some("task_submit");
        if is_task_submit {
            log_controlled_event("task_submit_frame_received", "opening");
        }
        let manager = self.app.state::<ControlledDeviceManager>();
        let Ok((lease, capabilities)) = manager.live_runtime_context() else {
            log_controlled_event("business_frame_dropped", "no_live_lease");
            return;
        };
        let business = self.app.state::<ControlledLaneState>();
        let (payload_kind, payload) = match business.open(frame, &capabilities) {
            Ok(opened) => opened,
            Err(error) => {
                log_controlled_event("business_frame_rejected", error.code());
                return;
            }
        };
        if payload_kind == "task_submit" {
            log_controlled_event("task_submit_decrypted", "dispatching");
        }
        let responses = match dispatch_decrypted_payload(
            self.business.as_ref(),
            &lease,
            &payload_kind,
            &payload,
        )
        .await
        {
            Ok(responses) => responses,
            Err(error) => {
                log_controlled_event("business_dispatch_failed", &error);
                if is_task_submit {
                    log_controlled_event("task_submit_dispatch_failed", &error);
                }
                return;
            }
        };
        if is_task_submit {
            log_controlled_event("task_submit_dispatched", "accepted");
        }
        // 一条请求可能产出多帧（journal 回放），逐帧加密回传；单帧失败即停止。
        for (response_kind, response) in responses {
            let frame = match business.seal(response_kind, &response, &capabilities) {
                Ok(frame) => frame,
                Err(error) => {
                    log_controlled_event("business_response_seal_failed", error.code());
                    return;
                }
            };
            if self.channel.try_send(frame).is_err() {
                log_controlled_event("business_response_send_failed", response_kind);
                return;
            }
            if is_task_submit && response_kind == "task_status" {
                log_controlled_event("task_submit_status_sent", "accepted");
            }
            if matches!(
                response_kind,
                "capability_snapshot" | "capability_snapshot_error"
            ) {
                log_controlled_event("capability_response_sent", response_kind);
            }
        }
    }
}

/// E2E-06：证据轮询间隔（远小于 30 秒预算）。
pub(crate) const LIVENESS_EVIDENCE_POLL_MS: u64 = 2_000;
/// 挑战发出后多久升级到 Relay 签名 attestation 兜底（直接应答尚未接线）。
pub(crate) const LIVENESS_ATTESTATION_ESCALATION_MS: u64 = 2_500;
/// 挑战的最长等待：超过后作废并重新发起。
pub(crate) const LIVENESS_CHALLENGE_TIMEOUT_MS: u64 = 8_000;

/// 本轮应当采取的证据动作（纯函数判定，worker 只负责发帧）。
#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum LivenessEvidenceAction {
    /// 余量充足，不打扰对端。
    Idle,
    /// 发起新的存活挑战。
    Challenge,
    /// 用同一挑战索取 Relay 签名 attestation。
    Escalate,
    /// 旧挑战超时作废，等待下一轮重新发起。
    Reset,
}

/// 证据状态：当前未完成的挑战（id、挑战值、发出时刻）与是否已升级过。
#[derive(Debug, Default, Clone, Eq, PartialEq)]
pub(crate) struct LivenessEvidenceState {
    pub(crate) outstanding: Option<(String, String, u64)>,
    pub(crate) escalated: bool,
}

/// 依据 watchdog 的需求与当前未完成挑战推导本轮动作。
pub(crate) fn liveness_evidence_action(
    needs_evidence: bool,
    state: &LivenessEvidenceState,
    now_b: u64,
) -> LivenessEvidenceAction {
    if !needs_evidence {
        return LivenessEvidenceAction::Idle;
    }
    match &state.outstanding {
        None => LivenessEvidenceAction::Challenge,
        Some((_, _, sent_at)) => {
            let age = now_b.saturating_sub(*sent_at);
            if age >= LIVENESS_CHALLENGE_TIMEOUT_MS {
                return LivenessEvidenceAction::Reset;
            }
            if !state.escalated && age >= LIVENESS_ATTESTATION_ESCALATION_MS {
                return LivenessEvidenceAction::Escalate;
            }
            LivenessEvidenceAction::Idle
        }
    }
}

/// B 端业务端口：worker 只负责解密/映射/回传，真正的业务（能力快照、任务
/// 接纳与取消）由命令层实现，避免两条实现漂移。
#[async_trait::async_trait]
pub(crate) trait ControlledBusinessPort: Send + Sync {
    /// 构造 wire `capability_snapshot`（`known_revision` 命中未过期快照时复用）。
    async fn capability_snapshot(
        &self,
        lease: &LeaseContext,
        request_id: &str,
        session_id: &str,
        known_revision: u64,
    ) -> Result<Value, String>;

    /// 接纳/派发一条 `task_submit`，返回该任务的 wire `task_status` 载荷。
    /// `session_ref` 是 A 端声明并在 wire 上使用的会话引用。
    async fn submit_task(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        payload: &Value,
    ) -> Result<Value, String>;

    /// 取消一条任务，返回 wire `task_status` 载荷（若服务不产出则返回 None）。
    async fn cancel_task(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        task_id: &str,
    ) -> Result<Option<Value>, String>;

    /// E2E-05 缺口恢复：按 `after_sequence` 回放权威事件；缺口太旧时返回一份
    /// `session_snapshot`，因此返回的是 (wire payload_kind, payload) 列表。
    async fn replay_journal(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        journal_epoch: &str,
        after_sequence: u64,
    ) -> Result<Vec<(String, Value)>, String>;

    /// E2E-05 缺口恢复：给出一份权威快照（`session_snapshot` wire 载荷）。
    async fn session_snapshot(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        request_id: &str,
    ) -> Result<Value, String>;
}

/// 解密后的业务帧派发（纯函数，便于测试）：按 payload_kind 调用业务端口，
/// 并把回包里的 `session_id` 统一改写回 A 端的会话引用，保证两侧同一 wire 身份。
pub(crate) async fn dispatch_decrypted_payload<P: ControlledBusinessPort + ?Sized>(
    port: &P,
    lease: &LeaseContext,
    payload_kind: &str,
    payload: &Value,
) -> Result<Vec<(&'static str, Value)>, String> {
    match payload_kind {
        "capability_snapshot_request" => {
            let request_id = payload
                .get("request_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let known_revision = payload
                .get("known_revision")
                .and_then(Value::as_str)
                .and_then(|value| value.parse::<u64>().ok())
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let session_id = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            match port
                .capability_snapshot(lease, request_id, session_id, known_revision)
                .await
            {
                Ok(response) => Ok(vec![("capability_snapshot", response)]),
                Err(_) => Ok(vec![(
                    "capability_snapshot_error",
                    capability_snapshot_error(lease, request_id, session_id),
                )]),
            }
        }
        "task_submit" => {
            let session_ref = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?
                .to_owned();
            let response = port.submit_task(lease, &session_ref, payload).await?;
            Ok(vec![(
                "task_status",
                rewrite_session_id(response, &session_ref)?,
            )])
        }
        "task_cancel" => {
            let session_ref = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?
                .to_owned();
            let task_id = payload
                .get("task_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            match port.cancel_task(lease, &session_ref, task_id).await? {
                Some(response) => Ok(vec![(
                    "task_status",
                    rewrite_session_id(response, &session_ref)?,
                )]),
                None => Ok(Vec::new()),
            }
        }
        "journal_replay_request" => {
            let session_ref = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let journal_epoch = payload
                .get("journal_epoch")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let after_sequence = payload
                .get("after_sequence")
                .and_then(Value::as_str)
                .and_then(|value| value.parse::<u64>().ok())
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let frames = port
                .replay_journal(lease, session_ref, journal_epoch, after_sequence)
                .await?;
            Ok(frames
                .into_iter()
                .filter_map(|(kind, payload)| match kind.as_str() {
                    "journal_event" => Some(("journal_event", payload)),
                    "session_snapshot" => Some(("session_snapshot", payload)),
                    _ => None,
                })
                .collect())
        }
        "session_snapshot_request" => {
            let session_ref = payload
                .get("session_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let request_id = payload
                .get("request_id")
                .and_then(Value::as_str)
                .ok_or_else(|| "task_request_invalid".to_owned())?;
            let snapshot = port
                .session_snapshot(lease, session_ref, request_id)
                .await?;
            Ok(vec![("session_snapshot", snapshot)])
        }
        // liveness / 审批 / 附件 / 成果等能力属后续任务：未知类型一律丢弃。
        _ => Ok(Vec::new()),
    }
}

fn capability_snapshot_error(lease: &LeaseContext, request_id: &str, session_id: &str) -> Value {
    let payload = json!({
        "v": 1,
        "type": "capability_snapshot_error",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "request_id": request_id,
        "session_id": session_id,
        "code": "capability_snapshot_unavailable",
    });
    payload
}

fn rewrite_session_id(mut payload: Value, session_ref: &str) -> Result<Value, String> {
    let object = payload
        .as_object_mut()
        .ok_or_else(|| "task_request_invalid".to_owned())?;
    object.insert(
        "session_id".to_owned(),
        Value::String(session_ref.to_owned()),
    );
    Ok(payload)
}

/// B → A 的 wire `journal_event`（E2E-05）。会话身份按第 4 节决策统一使用 A 的
/// 会话引用；事件本体 base64 编码，明文字段与本体保持一致以便 A 端做缺口检测。
///
/// 明文 `event_kind` 使用冻结的 `EVENT_KINDS` 闭集，而事件本体保留内部更细的
/// `JournalKind`（例如 `task_status`）。无法映射到冻结闭集的事件（附件、快照、
/// 非终态任务状态）不回传：A 端的任务状态由 `task_status` 帧负责，正文按
/// user_message / assistant_delta / reasoning / tool / permission / error /
/// terminal / artifact 回传。
pub(crate) fn build_journal_event_payload(
    lease: &LeaseContext,
    session_ref: &str,
    event: &super::journal::JournalEvent,
) -> Option<Value> {
    event.validate_exportable().ok()?;
    let wire_kind = event.wire_kind()?;
    // B owns the local session id while the wire contract owns A's opaque
    // session id. Envelope and authenticated body must use the same identity.
    let mut wire_event = event.clone();
    wire_event.session_id = session_ref.to_owned();
    let raw = serde_json::to_vec(&wire_event).ok()?;
    if raw.len() > super::journal::MAX_EVENT_PAYLOAD_BYTES {
        return None;
    }
    Some(json!({
        "v": 1,
        "type": "journal_event",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "session_id": session_ref,
        "journal_epoch": wire_event.epoch,
        "event_sequence": wire_event.sequence.to_string(),
        "event_kind": wire_kind,
        "event_b64": base64::engine::general_purpose::STANDARD.encode(&raw),
        "event_bytes": raw.len().to_string(),
    }))
}

/// B 端出站旁路：权威 journal 事件需要经同一代 endpoint lane 回传 A，而 lane 的
/// 接收端由 worker 独占，因此这里只保存可克隆发送句柄（断开/结束时清空）。
#[derive(Default)]
pub(crate) struct ControlledOutboundState {
    sender:
        parking_lot::Mutex<Option<crate::features::remote_control::manager::ControllerRelaySender>>,
}

impl ControlledOutboundState {
    pub(crate) fn clear_generation(&self, generation: &str) {
        let mut sender = self.sender.lock();
        if sender
            .as_ref()
            .is_some_and(|sender| sender.generation() == generation)
        {
            *sender = None;
        }
    }
    pub(crate) fn install(
        &self,
        sender: crate::features::remote_control::manager::ControllerRelaySender,
    ) {
        *self.sender.lock() = Some(sender);
    }

    pub(crate) fn clear(&self) {
        *self.sender.lock() = None;
    }

    pub(crate) fn sender(
        &self,
    ) -> Option<crate::features::remote_control::manager::ControllerRelaySender> {
        self.sender.lock().clone()
    }
}

/// 由 `controller_joined` 材料构造 B 端租约（纯函数）。传输指纹与 A 端使用
/// 同一份验签 transcript、按 B 角色域分离派生；租约在真正落地前不投影状态。
/// E2E-06：B 端恢复尝试状态（`challenge_b64` 非空即代表一次进行中的恢复）。
#[derive(Default)]
pub(crate) struct ControlledResumeAttempt {
    lease: Option<LeaseContext>,
    activation_id: Option<String>,
    challenge_b64: String,
    original_deadline_b: u64,
    liveness_epoch_b: String,
    peer_confirmed: bool,
    confirmed: bool,
    ack_sent: bool,
    liveness_sent: bool,
    ready_sent: bool,
}

impl ControlledResumeAttempt {
    pub(crate) fn is_active(&self) -> bool {
        !self.challenge_b64.is_empty()
    }

    fn reset(&mut self) {
        *self = Self::default();
    }
}

/// E2E-06：B 端恢复帧处理结果。
#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum ControlledResumeOutcome {
    Ignored,
    Progressed,
    Completed,
    Failed,
}

/// E2E-06：`desktop socket` 掉线后启动 B 端恢复：投影 reconnecting、取恢复材料，
/// 并按冻结协议发出 `controller_resume_prepare`。任一步失败返回 false，调用方
/// 必须结束本代通道（此时管理器的恢复失败路径已经清理租约与本代凭证）。
pub(crate) fn begin_controlled_resume<P: ControlledRelayPort + ?Sized>(
    port: &P,
    manager: &ControlledDeviceManager,
    resume: &mut ControlledResumeAttempt,
) -> bool {
    let Some(operation) = manager.current_operation() else {
        return false;
    };
    let now_b = controlled_monotonic_millis();
    let _ = manager.mark_transport_disconnected(now_b);
    let Ok(material) = manager.begin_resume(operation, now_b) else {
        return false;
    };
    let Ok(challenge) = new_resume_challenge() else {
        return false;
    };
    let request_id = format!("request-resume-{}", uuid::Uuid::new_v4().simple());
    let Ok(frame) = build_controller_resume_prepare(
        &material.lease,
        super::relay_transport::ControllerRole::ControlledB,
        &request_id,
        &material.resume_token,
        &challenge,
        material.original_deadline_b,
    ) else {
        return false;
    };
    if port.try_send(frame).is_err() {
        return false;
    }
    resume.lease = Some(material.lease);
    resume.challenge_b64 = challenge;
    resume.original_deadline_b = material.original_deadline_b;
    // 激活后的 liveness epoch 必须与旧值不同（Relay 会据此拒绝复用）。
    resume.liveness_epoch_b = format!("live-b-{}", uuid::Uuid::new_v4().simple());
    resume.activation_id = None;
    resume.peer_confirmed = false;
    resume.confirmed = false;
    resume.ack_sent = false;
    resume.liveness_sent = false;
    resume.ready_sent = false;
    true
}

/// E2E-06：按 Relay/A 的恢复帧推进 B 端相位。任何相位不符、对端确认验签失败或
/// `controller_resumed` 材料不完整都失败关闭（管理器侧已清理本代）。
pub(crate) fn handle_controlled_resume_frame<P: ControlledRelayPort + ?Sized>(
    port: &P,
    manager: &ControlledDeviceManager,
    lane: &ControlledLaneState,
    resume: &mut ControlledResumeAttempt,
    message: &Value,
) -> ControlledResumeOutcome {
    let Some(kind) = message.get("type").and_then(Value::as_str) else {
        return ControlledResumeOutcome::Ignored;
    };
    let Some(operation) = manager.current_operation() else {
        return ControlledResumeOutcome::Failed;
    };
    let Some(lease) = resume.lease.clone() else {
        return ControlledResumeOutcome::Failed;
    };
    // 审核修复：Relay/A 的恢复帧必须绑定当前租约；旧代际或旧租约的残留帧一律
    // 丢弃，既不推进相位也不终止候选。
    if !resume_frame_matches_lease(&lease, message) {
        return ControlledResumeOutcome::Ignored;
    }
    let role = super::relay_transport::ControllerRole::ControlledB;
    let now_b = controlled_monotonic_millis();
    let request_id = || format!("request-resume-{}", uuid::Uuid::new_v4().simple());
    match kind {
        "controller_resume_provisional" => {
            let Some(activation_id) = message.get("activation_id").and_then(Value::as_str) else {
                return ControlledResumeOutcome::Ignored;
            };
            if resume.activation_id.is_some() {
                return ControlledResumeOutcome::Ignored;
            }
            if manager
                .advance_resume(
                    operation.clone(),
                    ControlledResumeStep::Provisional { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            let Ok(confirm) = lane.seal_resume_confirm(&request_id(), activation_id) else {
                return ControlledResumeOutcome::Failed;
            };
            if port.try_send(confirm).is_err() {
                return ControlledResumeOutcome::Failed;
            }
            resume.activation_id = Some(activation_id.to_owned());
            ControlledResumeOutcome::Progressed
        }
        "encrypted_resume_confirm" => match lane.open_resume_confirm(message) {
            Ok(()) => {
                resume.peer_confirmed = true;
                ControlledResumeOutcome::Progressed
            }
            Err(_) => ControlledResumeOutcome::Ignored,
        },
        "controller_resume_activation_pending" => {
            let Some(activation_id) = message.get("activation_id").and_then(Value::as_str) else {
                return ControlledResumeOutcome::Ignored;
            };
            if resume.ack_sent || !resume.peer_confirmed {
                return ControlledResumeOutcome::Ignored;
            }
            if !resume.confirmed {
                if manager
                    .advance_resume(
                        operation.clone(),
                        ControlledResumeStep::Confirmed { activation_id },
                        now_b,
                    )
                    .is_err()
                {
                    return ControlledResumeOutcome::Failed;
                }
                resume.confirmed = true;
            }
            let Ok(ack) = build_controller_resume_activation_ack(
                &lease,
                &request_id(),
                activation_id,
                resume.original_deadline_b,
            ) else {
                return ControlledResumeOutcome::Failed;
            };
            if port.try_send(ack).is_err() {
                return ControlledResumeOutcome::Failed;
            }
            if manager
                .advance_resume(
                    operation.clone(),
                    ControlledResumeStep::ActivationAcknowledged { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            resume.ack_sent = true;
            if manager
                .advance_resume(
                    operation,
                    ControlledResumeStep::PostActivationLiveness {
                        activation_id,
                        liveness_epoch_b: &resume.liveness_epoch_b,
                    },
                    now_b,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            let Ok(liveness) = build_controller_post_activation_liveness(
                &lease,
                &request_id(),
                activation_id,
                &resume.liveness_epoch_b,
            ) else {
                return ControlledResumeOutcome::Failed;
            };
            if port.try_send(liveness).is_err() {
                return ControlledResumeOutcome::Failed;
            }
            resume.liveness_sent = true;
            ControlledResumeOutcome::Progressed
        }
        "controller_post_activation_liveness_confirmed" => {
            let Some(activation_id) = message.get("activation_id").and_then(Value::as_str) else {
                return ControlledResumeOutcome::Ignored;
            };
            let Some(liveness_epoch_b) = message.get("liveness_epoch_b").and_then(Value::as_str)
            else {
                return ControlledResumeOutcome::Ignored;
            };
            let Some(heartbeat_sequence_b) =
                message.get("heartbeat_sequence_b").and_then(Value::as_str)
            else {
                return ControlledResumeOutcome::Ignored;
            };
            if !resume.liveness_sent || resume.ready_sent {
                return ControlledResumeOutcome::Ignored;
            }
            let Ok(ready) = build_controller_resume_ready_ack(
                &lease,
                role,
                &request_id(),
                activation_id,
                liveness_epoch_b,
                heartbeat_sequence_b,
            ) else {
                return ControlledResumeOutcome::Failed;
            };
            if port.try_send(ready).is_err() {
                return ControlledResumeOutcome::Failed;
            }
            if manager
                .advance_resume(
                    operation,
                    ControlledResumeStep::ReadyAcknowledged { activation_id },
                    now_b,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            resume.ready_sent = true;
            ControlledResumeOutcome::Progressed
        }
        "controller_resumed" => {
            if !resume.ready_sent {
                return ControlledResumeOutcome::Ignored;
            }
            let Some(activation_id) = message.get("activation_id").and_then(Value::as_str) else {
                return ControlledResumeOutcome::Ignored;
            };
            let Some(liveness_epoch_a) = message.get("liveness_epoch_a").and_then(Value::as_str)
            else {
                return ControlledResumeOutcome::Ignored;
            };
            let Some(new_token) = message.get("new_resume_token_b64").and_then(Value::as_str)
            else {
                return ControlledResumeOutcome::Ignored;
            };
            let Ok(decoded) = base64::engine::general_purpose::STANDARD.decode(new_token) else {
                return ControlledResumeOutcome::Ignored;
            };
            let Ok(token) = <[u8; 32]>::try_from(decoded.as_slice()) else {
                return ControlledResumeOutcome::Ignored;
            };
            let Ok(fingerprint) = lane.resume_fingerprint(activation_id) else {
                return ControlledResumeOutcome::Failed;
            };
            if manager
                .complete_resumed(
                    operation,
                    activation_id,
                    now_b,
                    liveness_epoch_a,
                    token,
                    fingerprint,
                )
                .is_err()
            {
                return ControlledResumeOutcome::Failed;
            }
            lane.activate();
            resume.reset();
            ControlledResumeOutcome::Completed
        }
        "error" => ControlledResumeOutcome::Failed,
        _ => ControlledResumeOutcome::Ignored,
    }
}

fn controlled_lease_from_outcome(
    outcome: ControlledHandshakeOutcome,
    now_b: u64,
) -> Result<ControlledLease, String> {
    let context = LeaseContext {
        connection_id: outcome.lease.connection_id.clone(),
        controller_lease_id: outcome.lease.controller_lease_id.clone(),
        lease_epoch: outcome.lease.lease_epoch.to_string(),
        relay_epoch: outcome.lease.relay_epoch.clone(),
        remote_control_generation: outcome.lease.remote_control_generation.clone(),
    };
    let fingerprint = transport_key_fingerprint(outcome.transcript_hash, HandshakeRole::B)
        .map_err(|_| HANDSHAKE_FAILED.to_owned())?;
    Ok(ControlledLease {
        context,
        liveness_epoch_a: outcome.lease.liveness_epoch_a.clone(),
        resume_deadline_b: outcome.lease.resume_deadline_ms,
        resume_token: outcome.lease.resume_token,
        transport_key_fingerprint: fingerprint,
        negotiated_capabilities: outcome.negotiated_capabilities.clone(),
        // 冻结协议 v1 不携带主控设备显示名（controller_joined 无该字段）：首版
        // 留空，由 UI 的 i18n 兜底文案显示未知设备，真实来源待协议扩展。
        controller_device_name: String::new(),
        connected_at_b: now_b,
    })
}

fn registration_request(
    registration_request_id: &str,
    endpoint_id: &str,
    generation: &str,
    code_wrap: &CodeWrapKeyPair,
    handshake: &HandshakeKeyPair,
) -> Result<Value, String> {
    let value = json!({
        "v": 1,
        "type": "desktop_pairing_register",
        "request_id": format!("request-{registration_request_id}"),
        "registration_request_id": registration_request_id,
        "endpoint_id": endpoint_id,
        "remote_control_generation": generation,
        "code_wrap_public_key_b64": base64::engine::general_purpose::STANDARD
            .encode(code_wrap.public_key().to_bytes()),
        "handshake_public_key_b64": base64::engine::general_purpose::STANDARD
            .encode(handshake.public_key().to_bytes()),
        "key_algorithm": "x25519",
        "capabilities": REGISTRATION_CAPABILITIES,
    });
    validate_controller_message_json(&value.to_string(), SenderRole::ControlledB)
        .map_err(|_| "pairing_registration_invalid".to_owned())
}

pub(crate) fn registration_from_response(
    code_wrap: &CodeWrapKeyPair,
    request: &Value,
    response: &Value,
) -> Result<ControlledRegistration, String> {
    let context = RegistrationValidationContext::new(request.clone(), None)
        .map_err(|_| "pairing_registration_invalid".to_owned())?;
    let validated = validate_controller_registration_message_json(
        &response.to_string(),
        SenderRole::Relay,
        &context,
    )
    .map_err(|_| "pairing_registration_rejected".to_owned())?;
    if validated.get("type").and_then(Value::as_str) == Some("error") {
        return Err(validated
            .get("code")
            .and_then(Value::as_str)
            .unwrap_or("pairing_registration_rejected")
            .to_owned());
    }

    let string = |field: &str| -> Result<String, String> {
        validated
            .get(field)
            .and_then(Value::as_str)
            .map(str::to_owned)
            .ok_or_else(|| "pairing_registration_invalid".to_owned())
    };
    let expires_at_ms = string("expires_at_ms")?
        .parse::<u64>()
        .map_err(|_| "pairing_registration_invalid".to_owned())?;
    let binding = CodeWrapBinding::new(
        string("registration_request_id")?,
        string("pairing_id")?,
        string("remote_control_generation")?,
        string("relay_epoch")?,
        expires_at_ms,
    )
    .map_err(|_| "pairing_registration_invalid".to_owned())?;
    let sealed = base64::engine::general_purpose::STANDARD
        .decode(string("sealed_code_b64")?)
        .map_err(|_| "pairing_registration_invalid".to_owned())?;
    let pairing_code = code_wrap
        .open_sealed_code(&binding, &sealed)
        .map_err(|_| "pairing_code_invalid".to_owned())?
        .expose()
        .to_owned();
    Ok(ControlledRegistration {
        pairing_code,
        sealed_code_b64: string("sealed_code_b64")?,
        expires_at_ms,
    })
}

fn sanitize_error(value: &str) -> String {
    let bounded = value
        .chars()
        .filter(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-'))
        .take(64)
        .collect::<String>();
    if bounded.is_empty() {
        "unspecified".to_owned()
    } else {
        bounded
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn relay_wall_expiry_becomes_a_bounded_local_monotonic_deadline() {
        assert_eq!(
            pairing_deadline_b(1_030_000, 1_000_000, 70).unwrap(),
            30_070
        );
        assert_eq!(
            pairing_deadline_b(2_000_000, 1_000_000, 70).unwrap(),
            70 + MAX_PAIRING_CODE_TTL_MS
        );
        assert_eq!(
            pairing_deadline_b(1_000_000, 1_000_000, 70).unwrap_err(),
            "pairing_code_expired"
        );
        assert_eq!(
            pairing_deadline_b(999_999, 1_000_000, 70).unwrap_err(),
            "pairing_code_expired"
        );
    }
    use crate::features::remote_control::controller::controlling_handshake::ControllingHandshake;
    use crate::features::remote_control::controller::crypto::{CodeWrapKeyPair, HandshakeKeyPair};
    use crate::features::remote_control::controller::test_support::PairingFixture;
    use std::collections::VecDeque;
    use std::time::Duration;

    #[test]
    fn relay_log_lines_are_scoped_and_keep_the_legacy_controlled_prefix() {
        assert_eq!(
            controlled_log_line("registered", "waiting_for_pairing"),
            "[controlled] stage=registered detail=waiting_for_pairing"
        );
        assert_eq!(
            controller_log_line("connect_failed", "pairing_code_invalid"),
            "[controller] stage=connect_failed detail=pairing_code_invalid"
        );
    }

    #[test]
    fn bounded_log_token_drops_separators_so_credentials_cannot_be_pasted_in() {
        assert_eq!(
            bounded_log_token("code=AB12CD3 token=hunter2?x=1"),
            "codeAB12CD3tokenhunter2x1"
        );
        assert_eq!(bounded_log_token(&"a".repeat(400)).len(), 160);
    }

    /// Relay 会把发送方的 commit 原样回投（router 契约）：B 只吃 `sender_role == A`
    /// 的对端帧，自己的回投必须被跳过，否则真机上会在拿到预留的下一帧直接
    /// `controller_handshake_rejected`（2026-09-20 实测）。
    /// Relay 的收据校验契约（`controller-lease.js` 的 `acceptReceipt`）：
    /// `finished_receipt_ack` 里带的必须是 `sender_role` 那一方（对端）的 receipt
    /// secret，且能重算出对端先前的 commitment。这里在客户端侧做同构校验——真机
    /// 首次暴露的 `finished_receipt_invalid`（端上兜底成 `invalid_message`）就是
    /// 两端各自误传了自己的 receipt。
    #[test]
    fn receipt_ack_carries_the_peer_receipt_matching_its_commitment() {
        use crate::features::remote_control::controller::crypto::{
            ControllerRole as HandshakeRole, ReceiptSecret,
        };
        use crate::features::remote_control::controller::test_support::{
            handshake_binding, hex_lower, pairing_fixture,
        };

        let PairingFixture {
            relay,
            reservation,
            a_keys,
            b_keys,
            ..
        } = pairing_fixture("relay-key-runtime");
        let binding = handshake_binding(&reservation);
        let mut driver = ControlledHandshakeDriver::new(b_keys);
        let mut a = ControllingHandshake::new(
            reservation["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            relay.trust.clone(),
        );
        let b_commit = driver.accept_reserved(&reservation, &relay.trust).unwrap();
        let a_commit = a.accept_reserved(&reservation).unwrap();
        let b_finished = driver.accept_peer_commitment(&a_commit).unwrap();
        let a_finished = a.accept_peer_commitment(&b_commit).unwrap();
        let b_ack = driver.accept_peer_finished(&a_finished).unwrap();
        let a_ack = a.accept_peer_finished(&b_finished).unwrap();

        // A 提交的 ack 声明 sender_role=B：必须携带 B 的 receipt（对得上 B 的 commitment）。
        let from_a_ack =
            ReceiptSecret::from_base64_for_test(a_ack["receipt_secret_b64"].as_str().unwrap())
                .unwrap();
        assert_eq!(
            hex_lower(&from_a_ack.commitment(&binding, HandshakeRole::B).unwrap()),
            b_commit["commitment"].as_str().unwrap()
        );
        // B 提交的 ack 声明 sender_role=A：必须携带 A 的 receipt。
        let from_b_ack =
            ReceiptSecret::from_base64_for_test(b_ack["receipt_secret_b64"].as_str().unwrap())
                .unwrap();
        assert_eq!(
            hex_lower(&from_b_ack.commitment(&binding, HandshakeRole::A).unwrap()),
            a_commit["commitment"].as_str().unwrap()
        );
        // 反向核对：把 A 的 receipt 当成 B 的 receipt 去算，必然对不上 B 的 commitment
        //（这正是修复前两端实际发出的内容）。
        assert_ne!(
            hex_lower(&from_b_ack.commitment(&binding, HandshakeRole::B).unwrap()),
            b_commit["commitment"].as_str().unwrap()
        );
    }

    #[test]
    fn only_peer_handshake_frames_are_dispatched_on_the_controlled_lane() {
        let frame = |kind: &str, role: &str| {
            json!({
                "v": 1,
                "type": kind,
                "request_id": "request-0001",
                "pairing_id": "pairing-0001",
                "reservation_id": "reservation-0001",
                "remote_control_generation": "generation-0001",
                "relay_epoch": "relay-epoch-0001",
                "transcript_hash": "00".repeat(32),
                "sender_role": role,
                "commitment": "11".repeat(32),
                "ciphertext_b64": "AQID",
                "ciphertext_bytes": "3",
            })
        };
        assert!(skip_self_echoed_frame(&frame(
            "finished_receipt_commit",
            "B"
        )));
        assert!(!skip_self_echoed_frame(&frame(
            "finished_receipt_commit",
            "A"
        )));
        assert!(skip_self_echoed_frame(&frame(
            "encrypted_handshake_finished",
            "B"
        )));
        assert!(!skip_self_echoed_frame(&frame(
            "encrypted_handshake_finished",
            "A"
        )));
        // 其它帧（预留、joined、加密业务帧）绝不按自帧吞掉。
        assert!(!skip_self_echoed_frame(&frame(
            "controller_pairing_reserved",
            "B"
        )));
        assert!(!skip_self_echoed_frame(&frame("controller_joined", "B")));
        assert!(!skip_self_echoed_frame(&frame("encrypted_frame", "B")));
    }

    /// Deterministic fake lane: queued messages, then either hang or close.
    struct FakePort {
        queued: VecDeque<Value>,
        sent: std::sync::Mutex<Vec<Value>>,
        hang: bool,
        recv_delay: Duration,
    }

    impl FakePort {
        fn new(messages: Vec<Value>, hang: bool) -> Self {
            Self {
                queued: messages.into(),
                sent: std::sync::Mutex::new(Vec::new()),
                hang,
                recv_delay: Duration::ZERO,
            }
        }

        fn delayed(messages: Vec<Value>, hang: bool, recv_delay: Duration) -> Self {
            Self {
                queued: messages.into(),
                sent: std::sync::Mutex::new(Vec::new()),
                hang,
                recv_delay,
            }
        }
    }

    #[async_trait::async_trait]
    impl ControlledRelayPort for FakePort {
        fn try_send(&self, message: Value) -> Result<(), String> {
            self.sent.lock().unwrap().push(message);
            Ok(())
        }

        async fn recv(&mut self) -> Option<Value> {
            if !self.recv_delay.is_zero() {
                tokio::time::sleep(self.recv_delay).await;
            }
            if let Some(message) = self.queued.pop_front() {
                return Some(message);
            }
            if self.hang {
                std::future::pending::<()>().await;
            }
            None
        }
    }

    fn registration_request_fixture() -> (Value, CodeWrapKeyPair) {
        let receiver = CodeWrapKeyPair::generate().unwrap();
        let handshake = HandshakeKeyPair::generate().unwrap();
        let request = json!({
            "v": 1,
            "type": "desktop_pairing_register",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "endpoint_id": "endpoint-0001",
            "remote_control_generation": "generation-0001",
            "code_wrap_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(receiver.public_key().to_bytes()),
            "handshake_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(handshake.public_key().to_bytes()),
            "key_algorithm": "x25519",
            "capabilities": ["pairing_v1"],
        });
        (request, receiver)
    }

    fn registered_response(receiver: &CodeWrapKeyPair) -> Value {
        let code_wrap = CodeWrapKeyPair::generate().unwrap();
        let binding = CodeWrapBinding::new(
            "registration-0001",
            "pairing-0001",
            "generation-0001",
            "relay-epoch-0001",
            1_800_000_000_000,
        )
        .unwrap();
        let sealed = code_wrap
            .seal_pairing_code(receiver.public_key(), &binding, "2345678")
            .unwrap();
        json!({
            "v": 1,
            "type": "desktop_pairing_registered",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "pairing_id": "pairing-0001",
            "remote_control_generation": "generation-0001",
            "sealed_code_b64": base64::engine::general_purpose::STANDARD.encode(sealed),
            "expires_at_ms": "1800000000000",
            "relay_epoch": "relay-epoch-0001",
        })
    }

    fn block_on<F: std::future::Future>(future: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
            .block_on(future)
    }

    /// E2E-06：双方声明的能力族必须一致——Relay 的协商是「A claim ∩ B 注册」，
    /// 任一侧漏声明都会让整族能力静默失效（resume_v1 是本轮新增的族）。
    #[test]
    fn both_roles_declare_the_same_frozen_capability_families() {
        let mut claim = super::super::controlling_handshake::claim_capabilities().to_vec();
        let mut registration = super::registration_capabilities().to_vec();
        assert_eq!(
            claim.len(),
            registration.len(),
            "A claim 与 B 注册声明的能力族数量必须一致"
        );
        claim.sort_unstable();
        registration.sort_unstable();
        assert_eq!(
            claim, registration,
            "A claim 与 B 注册声明的能力集合必须一致"
        );
        for family in [
            "pairing_v1",
            "encrypted_frames_v1",
            "finished_receipt_v1",
            "role_liveness_v1",
            "resume_v1",
            "task_v1",
            "temporary_model_package_v1",
            "snapshot_journal_v1",
        ] {
            assert!(claim.contains(&family), "A 端未声明 {family}");
        }
    }

    #[test]
    fn registration_ignores_the_transport_ack_then_accepts_the_sealed_result() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![
                json!({ "v": 2, "type": "desktop_endpoint_registered", "endpoint_id": "endpoint-0001" }),
                registered_response(&receiver),
            ],
            false,
        );
        let registration = block_on(wait_for_registration(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap();
        assert_eq!(registration.pairing_code, "2345678");
        assert!(!format!("{registration:?}").contains("2345678"));
    }

    #[test]
    fn registration_ignores_relay_connection_frame_before_the_sealed_result() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![
                json!({
                    "type": "desktop_relay_connection",
                    "endpoint_id": "endpoint-0001",
                    "connected": true,
                    "error": null,
                }),
                registered_response(&receiver),
            ],
            false,
        );
        block_on(wait_for_registration(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap();
    }

    #[test]
    fn registration_treats_relay_disconnect_frame_as_retryable_disconnect() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![json!({
                "type": "desktop_relay_connection",
                "endpoint_id": "endpoint-0001",
                "connected": false,
                "error": "transport_closed",
            })],
            false,
        );
        assert_eq!(
            block_on(wait_for_registration(
                &mut port,
                &request,
                &receiver,
                Duration::from_millis(500),
            ))
            .unwrap_err(),
            "desktop_relay_disconnected"
        );
    }

    #[test]
    fn registration_wait_times_out_with_a_bounded_code() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(Vec::new(), true);
        assert_eq!(
            block_on(wait_for_registration(
                &mut port,
                &request,
                &receiver,
                Duration::from_millis(20),
            ))
            .unwrap_err(),
            "pairing_registration_timeout"
        );
    }

    #[test]
    fn ignored_frames_do_not_extend_the_registration_deadline() {
        let (request, receiver) = registration_request_fixture();
        let ignored = vec![
            json!({ "v": 2, "type": "desktop_endpoint_registered", "endpoint_id": "endpoint-0001" }),
            json!({
                "type": "desktop_relay_connection",
                "endpoint_id": "endpoint-0001",
                "connected": true,
                "error": null,
            }),
            json!({
                "v": 1,
                "type": "error",
                "request_id": "request-from-another-phase",
                "code": "enrollment_required",
                "message": "not this registration",
            }),
        ];
        let mut port = FakePort::delayed(ignored, true, Duration::from_millis(50));
        let started = std::time::Instant::now();
        assert_eq!(
            block_on(wait_for_registration(
                &mut port,
                &request,
                &receiver,
                Duration::from_millis(130),
            ))
            .unwrap_err(),
            "pairing_registration_timeout"
        );
        assert!(
            started.elapsed() < Duration::from_millis(220),
            "ignored frames must not restart the attempt timeout"
        );
    }

    #[test]
    fn closed_relay_lane_fails_closed() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(Vec::new(), false);
        assert_eq!(
            block_on(wait_for_registration(
                &mut port,
                &request,
                &receiver,
                Duration::from_millis(500),
            ))
            .unwrap_err(),
            "desktop_relay_disconnected"
        );
    }

    #[test]
    fn relay_rejection_is_surfaced_as_a_bounded_code() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![json!({
                "v": 1,
                "type": "error",
                "request_id": "request-registration-0001",
                "code": "enrollment_required",
                "message": "pairing registration rejected",
            })],
            false,
        );
        let error = block_on(wait_for_registration(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap_err();
        assert_eq!(error, "enrollment_required");
        assert!(
            error
                .chars()
                .all(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-')),
            "relay failures must stay bounded: {error}"
        );
        assert!(!error.contains(' '));
    }

    #[test]
    fn registration_ignores_an_error_for_another_request() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(
            vec![
                json!({
                    "v": 1,
                    "type": "error",
                    "request_id": "request-from-another-phase",
                    "code": "enrollment_required",
                    "message": "pairing registration rejected",
                }),
                registered_response(&receiver),
            ],
            false,
        );
        block_on(wait_for_registration(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap();
    }

    #[test]
    fn disabled_controller_lane_has_a_specific_bounded_error() {
        let (request, receiver) = registration_request_fixture();
        for relay_code in [
            "unsupported_protocol_version",
            "desktop_controller_unavailable",
        ] {
            let mut port = FakePort::new(
                vec![json!({
                    "v": 2,
                    "type": "error",
                    "code": relay_code,
                    "message": "desktop controller is unavailable",
                })],
                false,
            );
            assert_eq!(
                block_on(wait_for_registration(
                    &mut port,
                    &request,
                    &receiver,
                    Duration::from_millis(500),
                ))
                .unwrap_err(),
                "desktop_controller_relay_unavailable"
            );
        }
    }

    fn keys() -> (CodeWrapKeyPair, CodeWrapKeyPair) {
        (
            CodeWrapKeyPair::generate().unwrap(),
            CodeWrapKeyPair::generate().unwrap(),
        )
    }

    #[test]
    fn relay_result_is_bound_and_opened_without_exposing_the_code() {
        let (relay, receiver) = keys();
        let handshake = HandshakeKeyPair::generate().unwrap();
        let request = json!({
            "v": 1,
            "type": "desktop_pairing_register",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "endpoint_id": "endpoint-0001",
            "remote_control_generation": "generation-0001",
            "code_wrap_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(receiver.public_key().to_bytes()),
            "handshake_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(handshake.public_key().to_bytes()),
            "key_algorithm": "x25519",
            "capabilities": ["pairing_v1"],
        });
        let binding = CodeWrapBinding::new(
            "registration-0001",
            "pairing-0001",
            "generation-0001",
            "relay-epoch-0001",
            1_800_000_000_000,
        )
        .unwrap();
        let sealed = relay
            .seal_pairing_code(receiver.public_key(), &binding, "2345678")
            .unwrap();
        let response = json!({
            "v": 1,
            "type": "desktop_pairing_registered",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "pairing_id": "pairing-0001",
            "remote_control_generation": "generation-0001",
            "sealed_code_b64": base64::engine::general_purpose::STANDARD.encode(sealed),
            "expires_at_ms": "1800000000000",
            "relay_epoch": "relay-epoch-0001",
        });

        let registration = registration_from_response(&receiver, &request, &response).unwrap();
        assert_eq!(registration.pairing_code, "2345678");
        let debug = format!("{registration:?}");
        assert!(!debug.contains("2345678"));
    }

    #[test]
    fn cross_generation_relay_result_fails_closed() {
        let (relay, receiver) = keys();
        let handshake = HandshakeKeyPair::generate().unwrap();
        let request = json!({
            "v": 1,
            "type": "desktop_pairing_register",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "endpoint_id": "endpoint-0001",
            "remote_control_generation": "generation-0001",
            "code_wrap_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(receiver.public_key().to_bytes()),
            "handshake_public_key_b64": base64::engine::general_purpose::STANDARD
                .encode(handshake.public_key().to_bytes()),
            "key_algorithm": "x25519",
            "capabilities": ["pairing_v1"],
        });
        let binding = CodeWrapBinding::new(
            "registration-0001",
            "pairing-0001",
            "generation-0001",
            "relay-epoch-0001",
            1_800_000_000_000,
        )
        .unwrap();
        let sealed = relay
            .seal_pairing_code(receiver.public_key(), &binding, "2345678")
            .unwrap();
        let response = json!({
            "v": 1,
            "type": "desktop_pairing_registered",
            "request_id": "request-registration-0001",
            "registration_request_id": "registration-0001",
            "pairing_id": "pairing-0001",
            "remote_control_generation": "generation-other",
            "sealed_code_b64": base64::engine::general_purpose::STANDARD.encode(sealed),
            "expires_at_ms": "1800000000000",
            "relay_epoch": "relay-epoch-0001",
        });
        assert_eq!(
            registration_from_response(&receiver, &request, &response).unwrap_err(),
            "pairing_registration_rejected"
        );
    }

    /// 用真实 A 端握手跑完双收据交换，返回已推进到 Acked 的 driver 与 A 端状态。
    fn drive_through_receipts(
        reservation: &Value,
        b_keys: HandshakeKeyPair,
        a_keys: HandshakeKeyPair,
        trust: &AuthenticatedRelayTrustConfig,
    ) -> (ControlledHandshakeDriver, ControllingHandshake) {
        let mut driver = ControlledHandshakeDriver::new(b_keys);
        let b_commit = driver.accept_reserved(reservation, trust).unwrap();
        let mut a = ControllingHandshake::new(
            reservation["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            trust.clone(),
        );
        let a_commit = a.accept_reserved(reservation).unwrap();
        let b_finished = driver.accept_peer_commitment(&a_commit).unwrap();
        let a_finished = a.accept_peer_commitment(&b_commit).unwrap();
        driver.accept_peer_finished(&a_finished).unwrap();
        a.accept_peer_finished(&b_finished).unwrap();
        (driver, a)
    }

    /// E2E-02 第 3 步：B 端 driver 与真实 A 端握手完成双收据交换，并且只有在
    /// 收到 `controller_joined` 后才产出与 A 端同一份 transcript 绑定的材料。
    #[test]
    fn driver_completes_both_receipts_and_yields_the_joined_lease() {
        let PairingFixture {
            relay,
            reservation,
            joined,
            a_keys,
            b_keys,
            resume_token,
        } = crate::features::remote_control::controller::test_support::pairing_fixture(
            "relay-key-runtime",
        );
        let mut driver = ControlledHandshakeDriver::new(b_keys);
        let mut a = ControllingHandshake::new(
            reservation["request_id"].as_str().unwrap(),
            "claim-0001",
            "2345678",
            a_keys,
            relay.trust.clone(),
        );

        // 未接管前的顺序违规必须失败关闭，且不产生任何材料。
        assert_eq!(
            driver.accept_peer_finished(&reservation).unwrap_err(),
            "controller_handshake_stage_invalid"
        );
        assert_eq!(
            driver.accept_joined(&joined).unwrap_err(),
            "controller_handshake_stage_invalid"
        );
        assert!(!driver.is_active());

        let b_commit = driver.accept_reserved(&reservation, &relay.trust).unwrap();
        assert!(driver.is_active());
        assert_eq!(b_commit["type"], "finished_receipt_commit");
        assert_eq!(b_commit["sender_role"], "B");
        // 重复接管同一预留必须拒绝。
        assert_eq!(
            driver
                .accept_reserved(&reservation, &relay.trust)
                .unwrap_err(),
            "controller_handshake_stage_invalid"
        );
        // 收到 A 的 commitment 之前不能回送 Finished。
        assert_eq!(
            driver.accept_joined(&joined).unwrap_err(),
            "controller_handshake_stage_invalid"
        );

        let a_commit = a.accept_reserved(&reservation).unwrap();
        let b_finished = driver.accept_peer_commitment(&a_commit).unwrap();
        assert_eq!(b_finished["type"], "encrypted_handshake_finished");
        assert_eq!(b_finished["sender_role"], "B");
        let a_finished = a.accept_peer_commitment(&b_commit).unwrap();
        let b_ack = driver.accept_peer_finished(&a_finished).unwrap();
        assert_eq!(b_ack["type"], "finished_receipt_ack");
        assert_eq!(b_ack["sender_role"], "A");
        assert_eq!(b_ack["receiver_role"], "B");
        // A 校验 B 的 Finished 与其 commitment 一致后产出 ack。
        let a_ack = a.accept_peer_finished(&b_finished).unwrap();
        assert_eq!(a_ack["sender_role"], "B");
        assert_eq!(a_ack["receiver_role"], "A");

        let outcome = driver.accept_joined(&joined).unwrap();
        assert_eq!(outcome.lease.connection_id, "connection-0001");
        assert_eq!(outcome.lease.controller_lease_id, "controller-lease-0001");
        assert_eq!(outcome.lease.lease_epoch, 1);
        assert_eq!(outcome.lease.remote_control_generation, "generation-0001");
        assert_eq!(outcome.lease.liveness_epoch_a, "liveness-epoch-a-0001");
        assert_eq!(outcome.lease.resume_deadline_ms, 30_000);
        assert_eq!(outcome.lease.resume_token, resume_token);
        // 两端绑定同一份验签 transcript（传输指纹的派生输入）。
        assert_eq!(outcome.transcript_hash, a.transcript_hash().unwrap());
        // 双收据完成后 joined 只允许一次。
        assert_eq!(
            driver.accept_joined(&joined).unwrap_err(),
            "controller_handshake_stage_invalid"
        );
    }

    /// 绑定不符的 `controller_joined` 必须失败关闭。
    #[test]
    fn driver_rejects_joined_with_a_foreign_reservation_binding() {
        let PairingFixture {
            relay,
            reservation,
            joined,
            a_keys,
            b_keys,
            ..
        } = crate::features::remote_control::controller::test_support::pairing_fixture(
            "relay-key-runtime",
        );
        let (mut driver, _a) = drive_through_receipts(&reservation, b_keys, a_keys, &relay.trust);
        let mut foreign = joined;
        foreign["reservation_id"] = json!("reservation-other");
        assert_eq!(
            driver.accept_joined(&foreign).unwrap_err(),
            "controller_joined_binding_mismatch"
        );
    }

    /// B 端租约必须带上与验签 transcript 绑定的传输指纹、恢复材料与单调时钟
    /// 连接时刻，并把恢复凭证从 Debug 输出里剔除。
    #[test]
    fn controlled_lease_binds_fingerprint_and_recovery_material() {
        let PairingFixture {
            relay,
            reservation,
            joined,
            a_keys,
            b_keys,
            resume_token,
            ..
        } = crate::features::remote_control::controller::test_support::pairing_fixture(
            "relay-key-runtime",
        );
        let (mut driver, _a) = drive_through_receipts(&reservation, b_keys, a_keys, &relay.trust);
        let outcome = driver.accept_joined(&joined).unwrap();
        let expected =
            transport_key_fingerprint(outcome.transcript_hash, HandshakeRole::B).unwrap();
        let lease = controlled_lease_from_outcome(outcome, 12_345).unwrap();

        assert_eq!(lease.context.controller_lease_id, "controller-lease-0001");
        assert_eq!(lease.context.lease_epoch, "1");
        assert_eq!(lease.context.remote_control_generation, "generation-0001");
        assert_eq!(lease.liveness_epoch_a, "liveness-epoch-a-0001");
        assert_eq!(lease.resume_deadline_b, 30_000);
        assert_eq!(lease.resume_token, resume_token);
        assert_eq!(lease.transport_key_fingerprint, expected);
        assert_eq!(
            lease.controller_device_name, "",
            "冻结协议不携带主控设备名，首版留空由 UI 兜底"
        );
        assert_eq!(lease.connected_at_b, 12_345);
        let debug = format!("{lease:?}");
        assert!(debug.contains("[REDACTED]"));
        assert!(
            !debug.contains(&base64::engine::general_purpose::STANDARD.encode(resume_token)),
            "恢复凭证不得出现在 Debug 输出里"
        );
        assert_eq!(
            lease.negotiated_capabilities,
            vec![
                "encrypted_frames_v1".to_owned(),
                "pairing_v1".to_owned(),
                "task_v1".to_owned()
            ]
        );
    }

    /// E2E-04：解密后的业务帧按 payload_kind 派发，回包里的 session_id 统一改写
    /// 回 A 端的会话引用；未知类型与畸形载荷不产生任何动作。
    #[tokio::test]
    async fn decrypted_payloads_dispatch_and_rewrite_the_session_reference() {
        struct StubPort;

        #[async_trait::async_trait]
        impl ControlledBusinessPort for StubPort {
            async fn capability_snapshot(
                &self,
                _lease: &LeaseContext,
                request_id: &str,
                _session_id: &str,
                known_revision: u64,
            ) -> Result<Value, String> {
                if known_revision == 999 {
                    return Err("capability_source_unavailable".to_owned());
                }
                Ok(json!({
                    "v": 1,
                    "type": "capability_snapshot",
                    "request_id": request_id,
                    "capability_revision": (known_revision + 1).to_string(),
                }))
            }

            async fn submit_task(
                &self,
                _lease: &LeaseContext,
                session_ref: &str,
                payload: &Value,
            ) -> Result<Value, String> {
                // B 权威会话 id 与 A 的引用不同，由派发层改写回去。
                Ok(json!({
                    "v": 1,
                    "type": "task_status",
                    "task_id": payload["task_id"],
                    "session_id": format!("{session_ref}-authoritative"),
                    "task_status": "succeeded",
                }))
            }

            async fn cancel_task(
                &self,
                _lease: &LeaseContext,
                _session_ref: &str,
                task_id: &str,
            ) -> Result<Option<Value>, String> {
                Ok(Some(json!({
                    "v": 1,
                    "type": "task_status",
                    "task_id": task_id,
                    "session_id": "session-authoritative",
                    "task_status": "cancelled",
                })))
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
                Ok(json!({"v": 1, "type": "session_snapshot"}))
            }
        }

        let PairingFixture { reservation, .. } =
            crate::features::remote_control::controller::test_support::pairing_fixture(
                "relay-key-dispatch",
            );
        let lease =
            crate::features::remote_control::controller::test_support::lease_context(&reservation);
        let port = StubPort;

        let frames = dispatch_decrypted_payload(
            &port,
            &lease,
            "capability_snapshot_request",
            &json!({"request_id": "request-1", "known_revision": "4"}),
        )
        .await
        .unwrap();
        assert_eq!(frames.len(), 1);
        let (kind, response) = frames.into_iter().next().unwrap();
        assert_eq!(kind, "capability_snapshot");
        assert_eq!(response["request_id"], "request-1");
        assert_eq!(response["capability_revision"], "5");

        let frames = dispatch_decrypted_payload(
            &port,
            &lease,
            "capability_snapshot_request",
            &json!({
                "request_id": "request-failed",
                "session_id": "session-a-ref",
                "known_revision": "999",
            }),
        )
        .await
        .unwrap();
        let (kind, response) = frames.into_iter().next().unwrap();
        assert_eq!(kind, "capability_snapshot_error");
        assert_eq!(response["request_id"], "request-failed");
        assert_eq!(response["session_id"], "session-a-ref");
        assert_eq!(response["code"], "capability_snapshot_unavailable");

        let frames = dispatch_decrypted_payload(
            &port,
            &lease,
            "task_submit",
            &json!({
                "task_id": "task-0001",
                "session_id": "session-a-ref",
            }),
        )
        .await
        .unwrap();
        assert_eq!(frames.len(), 1);
        let (kind, response) = frames.into_iter().next().unwrap();
        assert_eq!(kind, "task_status");
        assert_eq!(response["task_id"], "task-0001");
        assert_eq!(
            response["session_id"], "session-a-ref",
            "回包必须改写成 A 端的会话引用"
        );

        let frames = dispatch_decrypted_payload(
            &port,
            &lease,
            "task_cancel",
            &json!({"task_id": "task-0001", "session_id": "session-a-ref"}),
        )
        .await
        .unwrap();
        assert_eq!(frames.len(), 1);
        let (kind, response) = frames.into_iter().next().unwrap();
        assert_eq!(kind, "task_status");
        assert_eq!(response["session_id"], "session-a-ref");

        // 未知类型丢弃；缺字段的载荷失败关闭。
        assert!(
            dispatch_decrypted_payload(&port, &lease, "journal_event", &json!({}))
                .await
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            dispatch_decrypted_payload(&port, &lease, "task_submit", &json!({"task_id": "t"}))
                .await
                .unwrap_err(),
            "task_request_invalid"
        );
    }

    /// E2E-05：B → A 的 journal_event 信封必须通过冻结运行时校验（角色 B +
    /// snapshot_journal_v1），且明文字段与事件本体一致。
    #[test]
    fn journal_event_payload_matches_the_frozen_contract() {
        use crate::features::remote_control::controller::journal::JournalEvent;
        use crate::features::remote_control::controller::protocol::{
            RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
        };

        let PairingFixture { reservation, .. } =
            crate::features::remote_control::controller::test_support::pairing_fixture(
                "relay-key-journal",
            );
        let lease =
            crate::features::remote_control::controller::test_support::lease_context(&reservation);
        let event = JournalEvent {
            event_id: "task-task-0001:1".to_owned(),
            epoch: "epoch-1".to_owned(),
            sequence: 1,
            kind: "task_status".to_owned(),
            session_id: "session-authoritative".to_owned(),
            task_id: Some("task-0001".to_owned()),
            terminal_status: Some("succeeded".to_owned()),
            payload: json!({"status": "succeeded", "sequence": "1"}),
        };
        let payload = build_journal_event_payload(&lease, "session-a-ref", &event).unwrap();
        assert_eq!(payload["session_id"], "session-a-ref");
        assert_eq!(payload["journal_epoch"], "epoch-1");
        assert_eq!(payload["event_sequence"], "1");
        assert_eq!(
            payload["event_kind"], "terminal",
            "内部 task_status（终态）在 wire 上映射为冻结闭集的 terminal"
        );
        let capabilities = vec![
            "encrypted_frames_v1".to_owned(),
            "pairing_v1".to_owned(),
            "snapshot_journal_v1".to_owned(),
            "task_v1".to_owned(),
        ];
        validate_controller_runtime_payload_json(
            &payload.to_string(),
            SenderRole::ControlledB,
            Some(&RuntimeValidationContext {
                lease: Some(lease.clone()),
                negotiated_capabilities: capabilities,
            }),
        )
        .expect("journal_event 信封必须满足冻结契约");
        // 解码回来的事件与本体一致。
        let decoded: JournalEvent = serde_json::from_slice(
            &base64::engine::general_purpose::STANDARD
                .decode(payload["event_b64"].as_str().unwrap())
                .unwrap(),
        )
        .unwrap();
        let mut expected = event.clone();
        expected.session_id = "session-a-ref".to_owned();
        assert_eq!(decoded, expected);
        // 进行中的任务状态与附件/快照不回传（由 task_status 帧或后续任务负责）。
        let mut running = event.clone();
        running.kind = "task_status".to_owned();
        running.terminal_status = None;
        assert!(build_journal_event_payload(&lease, "session-a-ref", &running).is_none());
        let mut attachment = event.clone();
        attachment.kind = "attachment".to_owned();
        assert!(build_journal_event_payload(&lease, "session-a-ref", &attachment).is_none());
    }

    /// E2E-06：B 端恢复序列——掉线后发 prepare，provisional 回 confirm，校验对端
    /// confirm 后依次发 activation_ack / post_activation_liveness / ready_ack，
    /// `controller_resumed` 后重开业务闸门。
    #[test]
    fn controlled_resume_sequence_sends_every_frame_and_reopens_business() {
        use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
        use crate::features::remote_control::controller::test_support::{
            lease_context, paired_session_keys, pairing_fixture,
        };

        let fixture = pairing_fixture("relay-key-b-resume");
        let (a_session, b_session) =
            paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        // `controlled_monotonic_millis` has a process-wide origin. Anchor the
        // fixture to the current clock so a long-running/full test suite does
        // not turn this otherwise-valid resume lease into an expired one.
        let now_b = controlled_monotonic_millis();
        let resume_deadline_b = now_b.saturating_add(30_000);
        // 租约的 generation 必须与本代一致（complete_connected 会据此校验）。
        let lease_context = LeaseContext {
            remote_control_generation: operation.generation().to_owned(),
            ..lease_context(&fixture.reservation)
        };
        manager
            .complete_registration(
                operation.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: resume_deadline_b,
                },
                now_b,
            )
            .unwrap();
        manager.mark_pairing_reserved(operation.clone()).unwrap();
        manager.mark_handshake_pending(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation.clone(),
                ControlledLease {
                    context: lease_context.clone(),
                    liveness_epoch_a: "live-a-1".into(),
                    resume_deadline_b,
                    resume_token: [1; 32],
                    transport_key_fingerprint: [2; 32],
                    controller_device_name: "Controller PC".into(),
                    connected_at_b: now_b,
                    negotiated_capabilities: vec!["encrypted_frames_v1".into(), "resume_v1".into()],
                },
            )
            .unwrap();
        let lane = ControlledLaneState::default();
        lane.install(HandshakeRole::B, &lease_context, b_session)
            .unwrap();
        let peer = ControllingLaneState::default();
        peer.install(HandshakeRole::A, &lease_context, a_session)
            .unwrap();

        let port = FakePort::new(vec![], false);
        let mut resume = ControlledResumeAttempt::default();
        assert!(begin_controlled_resume(&port, &manager, &mut resume));
        assert!(resume.is_active(), "恢复尝试必须处于进行中");
        let activation = "activation-0001";

        // provisional → B 回 confirm。
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({
                    "type": "controller_resume_provisional",
                    "activation_id": activation,
                    "connection_id": lease_context.connection_id,
                    "controller_lease_id": lease_context.controller_lease_id,
                    "lease_epoch": lease_context.lease_epoch,
                    "relay_epoch": lease_context.relay_epoch,
                    "remote_control_generation": lease_context.remote_control_generation,
                }),
            ),
            ControlledResumeOutcome::Progressed
        );
        // A 的 confirm（同一 resume 秘密）必须验签通过。
        let peer_confirm = peer
            .seal_resume_confirm("request-resume-peer", activation)
            .unwrap();
        assert_eq!(
            handle_controlled_resume_frame(&port, &manager, &lane, &mut resume, &peer_confirm),
            ControlledResumeOutcome::Progressed
        );
        // 审核回归：旧 generation 的恢复帧必须被丢弃且不影响相位。
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({
                    "type": "controller_resume_activation_pending",
                    "activation_id": activation,
                    "connection_id": lease_context.connection_id,
                    "controller_lease_id": lease_context.controller_lease_id,
                    "lease_epoch": lease_context.lease_epoch,
                    "relay_epoch": lease_context.relay_epoch,
                    "remote_control_generation": "generation-stale",
                }),
            ),
            ControlledResumeOutcome::Ignored
        );
        // activation_pending → activation_ack + post_activation_liveness。
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({
                    "type": "controller_resume_activation_pending",
                    "activation_id": activation,
                    "connection_id": lease_context.connection_id,
                    "controller_lease_id": lease_context.controller_lease_id,
                    "lease_epoch": lease_context.lease_epoch,
                    "relay_epoch": lease_context.relay_epoch,
                    "remote_control_generation": lease_context.remote_control_generation,
                }),
            ),
            ControlledResumeOutcome::Progressed
        );
        let epoch_b = resume.liveness_epoch_b.clone();
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({
                    "type": "controller_post_activation_liveness_confirmed",
                    "activation_id": activation,
                    "liveness_epoch_b": epoch_b,
                    "heartbeat_sequence_b": "1",
                    "connection_id": lease_context.connection_id,
                    "controller_lease_id": lease_context.controller_lease_id,
                    "lease_epoch": lease_context.lease_epoch,
                    "relay_epoch": lease_context.relay_epoch,
                    "remote_control_generation": lease_context.remote_control_generation,
                }),
            ),
            ControlledResumeOutcome::Progressed
        );
        assert_eq!(
            handle_controlled_resume_frame(
                &port,
                &manager,
                &lane,
                &mut resume,
                &json!({
                    "type": "controller_resumed",
                    "activation_id": activation,
                    "liveness_epoch_a": "live-a-2",
                    "liveness_epoch_b": epoch_b,
                    "new_resume_token_b64": base64::engine::general_purpose::STANDARD
                        .encode([3_u8; 32]),
                    "connection_id": lease_context.connection_id,
                    "controller_lease_id": lease_context.controller_lease_id,
                    "lease_epoch": lease_context.lease_epoch,
                    "relay_epoch": lease_context.relay_epoch,
                    "remote_control_generation": lease_context.remote_control_generation,
                }),
            ),
            ControlledResumeOutcome::Completed
        );
        assert!(!resume.is_active());
        assert!(manager.live_lease().is_ok(), "恢复完成后必须回到 connected");
        assert_eq!(manager.live_liveness_epoch().as_deref(), Some("live-a-2"));
        assert!(!lane.is_suspended(), "恢复完成后业务通道必须重新打开");
        let sent = port.sent.lock().unwrap();
        let kinds: Vec<&str> = sent
            .iter()
            .filter_map(|frame| frame.get("type").and_then(Value::as_str))
            .collect();
        assert_eq!(
            kinds,
            vec![
                "controller_resume_prepare",
                "encrypted_resume_confirm",
                "controller_resume_activation_ack",
                "controller_post_activation_liveness",
                "controller_resume_ready_ack",
            ]
        );
    }

    /// 出站旁路（journal 回传句柄）：安装后可取用同一代 sender，清空后不再可发。
    #[test]
    fn controlled_outbound_state_installs_and_clears() {
        let state = ControlledOutboundState::default();
        assert!(state.sender().is_none());
        state.clear();
        assert!(state.sender().is_none());
    }

    /// E2E-06：证据动作遵循 watchdog 策略——余量充足不打扰；需要证据且无挑战时
    /// 发起；挑战过久升级为 Relay 签名兜底；再超时作废重来。
    #[test]
    fn liveness_evidence_actions_follow_the_watchdog_policy() {
        let idle = LivenessEvidenceState::default();
        assert_eq!(
            liveness_evidence_action(false, &idle, 0),
            LivenessEvidenceAction::Idle
        );
        assert_eq!(
            liveness_evidence_action(true, &idle, 0),
            LivenessEvidenceAction::Challenge
        );
        let outstanding = LivenessEvidenceState {
            outstanding: Some(("challenge-1".to_owned(), "AAAA".to_owned(), 1_000)),
            escalated: false,
        };
        assert_eq!(
            liveness_evidence_action(true, &outstanding, 2_000),
            LivenessEvidenceAction::Idle,
            "刚发出的挑战应等待直接应答"
        );
        assert_eq!(
            liveness_evidence_action(true, &outstanding, 3_500),
            LivenessEvidenceAction::Escalate
        );
        let escalated = LivenessEvidenceState {
            escalated: true,
            ..outstanding.clone()
        };
        assert_eq!(
            liveness_evidence_action(true, &escalated, 3_500),
            LivenessEvidenceAction::Idle,
            "同一挑战只升级一次"
        );
        assert_eq!(
            liveness_evidence_action(true, &escalated, 12_000),
            LivenessEvidenceAction::Reset,
            "挑战超时后作废重来"
        );
        assert_eq!(
            liveness_evidence_action(false, &outstanding, 12_000),
            LivenessEvidenceAction::Idle
        );
    }
}
