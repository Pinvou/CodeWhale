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
use serde_json::{json, Value};
use tauri::{AppHandle, Emitter, Manager as _};
use tokio::sync::oneshot;

use crate::features::remote_control::manager::RemoteControlManager;

use super::controlled::{
    ControlledDeviceManager, ControlledLease, ControlledOperation, ControlledRegistration,
    ControlledStatus,
};
use super::controlled_handshake::ControlledHandshake;
use super::controlling_handshake::JoinedLease;
use super::crypto::{
    AuthenticatedRelayTrustConfig, CodeWrapBinding, CodeWrapKeyPair, ControllerRole as HandshakeRole,
    HandshakeKeyPair, load_relay_trust_config, transport_key_fingerprint,
};
use super::protocol::LeaseContext;
use super::protocol::{
    validate_controller_message_json, validate_controller_registration_message_json,
    RegistrationValidationContext, SenderRole,
};

const REGISTRATION_TIMEOUT_MS: u64 = 30_000;
const REGISTRATION_CAPABILITIES: &[&str] = &[
    "pairing_v1",
    "encrypted_frames_v1",
    "finished_receipt_v1",
];
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

#[derive(Debug)]
pub(crate) enum RegistrationStep {
    /// Relay traffic that belongs to another phase (e.g. the synthetic
    /// `desktop_endpoint_registered` acknowledgement) is ignored here.
    Ignore,
    Registered(Box<ControlledRegistration>),
}

/// `controller_joined` 之后的租约材料：租约身份来自 Relay，传输指纹由双方
/// 认证过的 transcript 派生。
#[derive(Debug)]
pub(crate) struct ControlledHandshakeOutcome {
    pub(crate) lease: JoinedLease,
    pub(crate) transcript_hash: [u8; 32],
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
        let keys = self
            .keys
            .take()
            .ok_or("controller_handshake_key_invalid")?;
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
    pub(crate) fn accept_peer_finished(
        &mut self,
        message: &Value,
    ) -> Result<Value, &'static str> {
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
        let lease = handshake.accept_joined(message).map_err(|error| error.code)?;
        Ok(ControlledHandshakeOutcome {
            lease,
            transcript_hash: handshake.transcript_hash(),
        })
    }
}

/// Waits for the registration response for `request`. Extracted from the worker
/// so timeout / disconnect / ignore-unknown branches are covered by tests.
pub(crate) async fn next_registration_step<P: ControlledRelayPort>(
    port: &mut P,
    request: &Value,
    code_wrap: &CodeWrapKeyPair,
    timeout: std::time::Duration,
) -> Result<RegistrationStep, String> {
    let deadline = tokio::time::sleep(timeout);
    tokio::pin!(deadline);
    loop {
        tokio::select! {
            _ = &mut deadline => return Err("pairing_registration_timeout".to_owned()),
            message = port.recv() => {
                let Some(message) = message else {
                    return Err("desktop_relay_disconnected".to_owned());
                };
                if message.get("type").and_then(Value::as_str)
                    == Some("desktop_endpoint_registered")
                {
                    return Ok(RegistrationStep::Ignore);
                }
                return registration_from_response(code_wrap, request, &message)
                    .map(|registration| RegistrationStep::Registered(Box::new(registration)));
            }
        }
    }
}

impl ControlledRelayWorker {
    pub(crate) async fn start(
        app: AppHandle,
        relay: RemoteControlManager,
        operation: ControlledOperation,
    ) -> Result<
        oneshot::Receiver<Result<ControlledStatus, String>>,
        String,
    > {
        let code_wrap = CodeWrapKeyPair::generate().map_err(|_| "pairing_key_invalid")?;
        let handshake = HandshakeKeyPair::generate().map_err(|_| "handshake_key_invalid")?;
        let channel = relay
            .start_controller_relay(operation.generation())
            .await?;
        let (done_tx, done_rx) = oneshot::channel();
        let mut worker = Self {
            app,
            relay,
            operation,
            code_wrap,
            handshake: Some(handshake),
            channel,
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
        loop {
            match next_registration_step(
                &mut self.channel,
                &request,
                &self.code_wrap,
                std::time::Duration::from_millis(REGISTRATION_TIMEOUT_MS),
            )
            .await?
            {
                RegistrationStep::Ignore => continue,
                RegistrationStep::Registered(registration) => {
                    let status = manager
                        .complete_registration(self.operation.clone(), *registration, 0)
                        .map_err(|error| error.code.to_owned())?;
                    let _ = self.app.emit(CONTROLLED_STATUS_EVENT, &status);
                    return Ok(status);
                }
            }
        }
    }

    fn fail_registration(&self, error: &str) {
        let manager = self.app.state::<ControlledDeviceManager>();
        if !manager.is_pending_handshake(self.operation.clone()) {
            // 用户已停止/断开，或本代已被刷新取代：不覆盖既有投影。
            return;
        }
        let bounded = sanitize_error(error);
        if let Ok(status) = manager.fail_operation(self.operation.clone(), bounded) {
            let _ = self.app.emit(CONTROLLED_STATUS_EVENT, &status);
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
        while let Some(message) = self.channel.recv().await {
            if joined {
                continue;
            }
            let result = match message.get("type").and_then(Value::as_str) {
                Some("controller_pairing_reserved") => self.begin_handshake(&message, &mut driver),
                Some("finished_receipt_commit") => driver
                    .accept_peer_commitment(&message)
                    .map(Some)
                    .map_err(str::to_owned),
                Some("encrypted_handshake_finished") => driver
                    .accept_peer_finished(&message)
                    .map(Some)
                    .map_err(str::to_owned),
                Some("controller_joined") => match self.complete_handshake(&message, &mut driver) {
                    Ok(()) => {
                        joined = true;
                        Ok(None)
                    }
                    Err(code) => Err(code),
                },
                _ => Ok(None),
            };
            match result {
                Ok(Some(frame)) => {
                    if let Err(error) = self.channel.try_send(frame) {
                        self.fail_handshake(&error);
                        return;
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
        let status = manager
            .mark_claimed(self.operation.clone())
            .map_err(|error| error.code.to_owned())?;
        let _ = self.app.emit(CONTROLLED_STATUS_EVENT, &status);
        driver
            .accept_reserved(message, &trust)
            .map(Some)
            .map_err(|code| code.to_owned())
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
        let status = manager
            .complete_connected(self.operation.clone(), lease)
            .map_err(|error| error.code.to_owned())?;
        let _ = self.app.emit(CONTROLLED_STATUS_EVENT, &status);
        Ok(())
    }

    /// 握手失败收尾：投影为有界错误并结束本代通道（dropping the channel 会
    /// 清掉该 generation 的通道绑定，本地停止/刷新入口保持可用）。
    fn fail_handshake(&self, error: &str) {
        let manager = self.app.state::<ControlledDeviceManager>();
        if !manager.is_pending_handshake(self.operation.clone()) {
            // 用户已停止/断开，或本代已被刷新取代：不覆盖既有投影。
            return;
        }
        let bounded = sanitize_error(error);
        if let Ok(status) = manager.fail_operation(self.operation.clone(), bounded) {
            let _ = self.app.emit(CONTROLLED_STATUS_EVENT, &status);
        }
    }
}

/// 由 `controller_joined` 材料构造 B 端租约（纯函数）。传输指纹与 A 端使用
/// 同一份验签 transcript、按 B 角色域分离派生；租约在真正落地前不投影状态。
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
    use crate::features::remote_control::controller::crypto::{CodeWrapKeyPair, HandshakeKeyPair};
    use crate::features::remote_control::controller::controlling_handshake::ControllingHandshake;
    use crate::features::remote_control::controller::test_support::PairingFixture;
    use std::collections::VecDeque;
    use std::time::Duration;

    /// Deterministic fake lane: queued messages, then either hang or close.
    struct FakePort {
        queued: VecDeque<Value>,
        sent: std::sync::Mutex<Vec<Value>>,
        hang: bool,
    }

    impl FakePort {
        fn new(messages: Vec<Value>, hang: bool) -> Self {
            Self {
                queued: messages.into(),
                sent: std::sync::Mutex::new(Vec::new()),
                hang,
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
        let first = block_on(next_registration_step(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap();
        assert!(matches!(first, RegistrationStep::Ignore));
        let second = block_on(next_registration_step(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap();
        match second {
            RegistrationStep::Registered(registration) => {
                assert_eq!(registration.pairing_code, "2345678");
                assert!(!format!("{registration:?}").contains("2345678"));
            }
            RegistrationStep::Ignore => panic!("sealed result must not be ignored"),
        }
    }

    #[test]
    fn registration_wait_times_out_with_a_bounded_code() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(Vec::new(), true);
        assert_eq!(
            block_on(next_registration_step(
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
    fn closed_relay_lane_fails_closed() {
        let (request, receiver) = registration_request_fixture();
        let mut port = FakePort::new(Vec::new(), false);
        assert_eq!(
            block_on(next_registration_step(
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
                "code": "endpoint_not_enrolled",
                "message": "pairing registration rejected",
            })],
            false,
        );
        let error = block_on(next_registration_step(
            &mut port,
            &request,
            &receiver,
            Duration::from_millis(500),
        ))
        .unwrap_err();
        assert!(
            error
                .chars()
                .all(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-')),
            "relay failures must stay bounded: {error}"
        );
        assert!(!error.contains(' '));
    }

    fn keys() -> (CodeWrapKeyPair, CodeWrapKeyPair) {
        (CodeWrapKeyPair::generate().unwrap(), CodeWrapKeyPair::generate().unwrap())
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
            driver.accept_reserved(&reservation, &relay.trust).unwrap_err(),
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
        let (mut driver, _a) =
            drive_through_receipts(&reservation, b_keys, a_keys, &relay.trust);
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
        let (mut driver, _a) =
            drive_through_receipts(&reservation, b_keys, a_keys, &relay.trust);
        let outcome = driver.accept_joined(&joined).unwrap();
        let expected =
            transport_key_fingerprint(outcome.transcript_hash, HandshakeRole::B).unwrap();
        let lease = controlled_lease_from_outcome(outcome, 12_345).unwrap();

        assert_eq!(lease.context.controller_lease_id, "controller-lease-0001");
        assert_eq!(lease.context.lease_epoch, "1");
        assert_eq!(
            lease.context.remote_control_generation,
            "generation-0001"
        );
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
    }
}
