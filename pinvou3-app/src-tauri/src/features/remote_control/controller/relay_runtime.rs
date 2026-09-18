//! Production glue between the Windows controlled-device owner and Relay.
//!
//! E2E-01 only owns pairing registration: B reuses the authenticated desktop
//! endpoint socket, sends one generation-bound registration, validates Relay's
//! sealed result, and keeps the channel alive for later handshake phases.

use base64::Engine as _;
use serde_json::{json, Value};
use tauri::{AppHandle, Emitter, Manager as _};
use tokio::sync::oneshot;

use crate::features::remote_control::manager::RemoteControlManager;

use super::controlled::{
    ControlledDeviceManager, ControlledOperation, ControlledRegistration, ControlledStatus,
};
use super::crypto::{CodeWrapKeyPair, CodeWrapBinding, HandshakeKeyPair};
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

pub(crate) struct ControlledRelayWorker {
    app: AppHandle,
    relay: RemoteControlManager,
    operation: ControlledOperation,
    code_wrap: CodeWrapKeyPair,
    handshake: HandshakeKeyPair,
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
            handshake,
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
            &self.handshake,
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
        let bounded = sanitize_error(error);
        if let Ok(status) = manager.fail_operation(self.operation.clone(), bounded) {
            let _ = self.app.emit(CONTROLLED_STATUS_EVENT, &status);
        }
    }

    async fn serve(mut self) {
        // A dropped channel is intentional on stop/refresh and clears the old
        // generation binding. Unknown messages are ignored until E2E-02; they
        // must never be forwarded to the legacy WebUI event stream.
        while let Some(_message) = self.channel.recv().await {}
    }
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
}
