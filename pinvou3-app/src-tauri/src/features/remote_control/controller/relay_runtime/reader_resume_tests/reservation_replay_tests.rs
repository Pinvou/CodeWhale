//! Real JS Router/LeaseManager -> native A worker and B reader/serve/manager.
//! Only endpoint authentication/pairing persistence and the A transport adapter
//! are controlled. B uses the actual loopback WebSocket/channel receipt path.
use super::*;
use crate::features::remote_control::controller as ctl;
use std::collections::VecDeque;
use std::io::{BufRead, BufReader, Write};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::{Mutex, mpsc};

struct Relay {
    child: Child,
    input: Option<ChildStdin>,
    replies: mpsc::Receiver<String>,
    reader: Option<std::thread::JoinHandle<()>>,
}

impl Relay {
    fn new() -> Self {
        let fixture = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../remote-control-relay/test/helpers/native-reservation-replay.js");
        let mut child = Command::new("node")
            .arg(fixture)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .unwrap();
        let input = child.stdin.take();
        let output = child.stdout.take().unwrap();
        let (sender, replies) = mpsc::sync_channel(1);
        let reader = std::thread::spawn(move || {
            for line in BufReader::new(output).lines() {
                if sender.send(line.unwrap()).is_err() {
                    break;
                }
            }
        });
        Self {
            child,
            input,
            replies,
            reader: Some(reader),
        }
    }

    fn call(&mut self, value: Value) -> Value {
        writeln!(self.input.as_mut().unwrap(), "{value}").unwrap();
        self.input.as_mut().unwrap().flush().unwrap();
        let line = self
            .replies
            .recv_timeout(Duration::from_secs(3))
            .expect("real Relay stdio response exceeded budget");
        assert!(line.len() < 128 * 1024);
        serde_json::from_str(&line).unwrap()
    }

    fn finish(&mut self) {
        drop(self.input.take());
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                assert!(status.success());
                break;
            }
            assert!(std::time::Instant::now() < deadline, "Relay exit timed out");
            std::thread::sleep(Duration::from_millis(5));
        }
        self.reader.take().unwrap().join().unwrap();
    }
}

impl Drop for Relay {
    fn drop(&mut self) {
        drop(self.input.take());
        if self.child.try_wait().ok().flatten().is_none() {
            let _ = self.child.kill();
        }
        let _ = self.child.wait();
        if let Some(reader) = self.reader.take() {
            let _ = reader.join();
        }
    }
}

struct Port {
    relay: Relay,
    socket: Socket,
    outgoing: Mutex<Vec<Value>>,
    incoming: VecDeque<Value>,
    handle: tauri::AppHandle,
    operation: ctl::controlled::ControlledOperation,
    first_deadline: Option<u64>,
    a_frames: Vec<String>,
    b_frames: Vec<String>,
    replay_count: usize,
    consumed: u64,
}

impl Port {
    async fn route(&mut self, role: &str, message: Value) {
        let response = self.relay.call(json!({ "role": role, "message": message }));
        self.consumed = response["consumed"].as_u64().unwrap();
        for delivery in response["deliveries"].as_array().unwrap() {
            let frame = delivery["message"].clone();
            if delivery["role"] == "A" {
                self.incoming.push_back(frame);
            } else {
                if frame["type"] == "controller_pairing_reserved"
                    && frame["request_id"]
                        .as_str()
                        .unwrap()
                        .starts_with("request-native-retry-")
                {
                    self.replay_count += 1;
                }
                send(&mut self.socket, &frame).await;
            }
        }
    }
}

#[async_trait::async_trait]
impl ctl::controlling_runtime::ControllingRelayPort for Port {
    fn send(&self, message: Value) -> Result<(), String> {
        self.outgoing.lock().unwrap().push(message);
        Ok(())
    }

    async fn recv(&mut self) -> Option<Value> {
        let outgoing = std::mem::take(&mut *self.outgoing.lock().unwrap());
        for message in outgoing {
            self.a_frames.push(message["type"].as_str().unwrap().into());
            self.route("A", message).await;
        }
        loop {
            if let Some(message) = self.incoming.pop_front() {
                return Some(message);
            }
            let incoming = tokio::time::timeout(Duration::from_secs(3), self.socket.next())
                .await
                .unwrap()
                .unwrap()
                .unwrap();
            let Message::Text(text) = incoming else {
                continue;
            };
            let frame: Value = serde_json::from_str(&text).unwrap();
            assert!(matches!(
                frame["type"].as_str(),
                Some(
                    "finished_receipt_commit"
                        | "encrypted_handshake_finished"
                        | "finished_receipt_ack"
                )
            ));
            let manager = self.handle.state::<ControlledDeviceManager>();
            let deadline = manager
                .preconnect_deadline(&self.operation)
                .unwrap()
                .unwrap()
                .deadline_b();
            if let Some(first) = self.first_deadline {
                assert_eq!(
                    deadline, first,
                    "replays must not renew B's handshake deadline"
                );
            } else {
                self.first_deadline = Some(deadline);
            }
            self.b_frames.push(frame["type"].as_str().unwrap().into());
            self.route("B", frame).await;
        }
    }
}

pub(crate) async fn round_trip<T>(build_host: impl FnOnce() -> (tauri::AppHandle, T)) {
    let directory = tempfile::tempdir().unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("ws://{}/relay", listener.local_addr().unwrap());
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("PINVOU_REMOTE_RELAY_WS_URL", &url);
        std::env::set_var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL", &url);
    }
    let (handle, _host) = build_host();
    let manager = handle.state::<ControlledDeviceManager>();
    let operation = manager.start(None, Vec::new()).unwrap();
    let b_keys = HandshakeKeyPair::generate().unwrap();
    let a_keys = HandshakeKeyPair::generate().unwrap();
    let mut node = Relay::new();
    let hello = node.call(json!({"init":true,"generation":operation.generation(),
        "publicKey":base64::engine::general_purpose::STANDARD.encode(b_keys.public_key().to_bytes())}));
    let key_id = hello["keyId"].as_str().unwrap();
    let public_key = hello["publicKey"].as_str().unwrap();
    unsafe {
        std::env::set_var("PINVOU_REMOTE_RELAY_SIGNING_KEY_ID", key_id);
        std::env::set_var("PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64", public_key);
    }
    let trust = AuthenticatedRelayTrustConfig::from_pinned_keys([(
        key_id.to_owned(),
        public_key.to_owned(),
    )])
    .unwrap();
    let now = controlled_monotonic_millis();
    manager
        .complete_registration(
            operation.clone(),
            ControlledRegistration {
                pairing_code: "2345678".into(),
                sealed_code_b64: "sealed".into(),
                expires_at_ms: now + 30_000,
            },
            now,
        )
        .unwrap();
    let relay = RemoteControlManager::new(handle.clone());
    let channel = relay
        .start_controller_relay(operation.generation())
        .await
        .unwrap();
    let mut owned = OwnedLane {
        relay: relay.clone(),
        generation: operation.generation().into(),
        worker: None,
    };
    let (processed_tx, mut processed_rx) = tokio::sync::mpsc::channel(128);
    let worker = ControlledRelayWorker {
        transport_owner: None,
        app: handle.clone(),
        relay,
        operation: operation.clone(),
        code_wrap: CodeWrapKeyPair::generate().unwrap(),
        handshake: Some(b_keys),
        channel,
        business: std::sync::Arc::new(NoBusiness),
        action_results: std::sync::Arc::new(RemoteActionResultCache::default()),
        business_epoch: std::sync::Arc::new(AtomicU64::new(0)),
        processed_frames: Some(processed_tx),
        before_frame_effect: None,
        activation_probe: None,
        evidence_hooks: None,
    };
    owned.worker = Some(tokio::spawn(worker.serve()));
    let mut socket = accept(&listener).await;
    registered(&mut socket).await;
    let mut port = Port {
        relay: node,
        socket,
        outgoing: Mutex::new(Vec::new()),
        incoming: VecDeque::new(),
        handle: handle.clone(),
        operation: operation.clone(),
        first_deadline: None,
        a_frames: Vec::new(),
        b_frames: Vec::new(),
        replay_count: 0,
        consumed: 0,
    };
    let mut a = ctl::controlling_handshake::ControllingHandshake::new(
        "request-native-first",
        "claim-native-stable",
        "2345678",
        a_keys,
        trust,
    );
    let mut stages = Vec::new();
    let outcome =
        ctl::controlling_runtime::drive_controlling_handshake(&mut port, &mut a, |stage| {
            stages.push(stage);
            Ok(())
        })
        .await
        .unwrap();
    wait_status(
        &manager,
        ControlledStatusKind::Connected,
        "real Router Joined",
    )
    .await;
    assert_eq!(
        manager.live_lease().unwrap().context.controller_lease_id,
        outcome.lease.controller_lease_id
    );
    assert!(handle.state::<ControlledLaneState>().is_installed());
    assert_eq!(port.consumed, 1, "one real lease consumption");
    assert_eq!(port.replay_count, 6);
    assert_eq!(
        port.a_frames,
        [
            "controller_pairing_claim",
            "finished_receipt_commit",
            "encrypted_handshake_finished",
            "finished_receipt_ack"
        ]
    );
    assert_eq!(
        port.b_frames,
        [
            "finished_receipt_commit",
            "encrypted_handshake_finished",
            "finished_receipt_ack"
        ]
    );
    assert_eq!(
        stages,
        [
            ctl::controlling_runtime::ControllingStageEvent::ClaimSent,
            ctl::controlling_runtime::ControllingStageEvent::Reserved {
                remote_control_generation: operation.generation().into()
            },
            ctl::controlling_runtime::ControllingStageEvent::FinishedSent,
            ctl::controlling_runtime::ControllingStageEvent::Joined
        ]
    );
    let mut seen_replays = 0;
    while let Ok(frame) = processed_rx.try_recv() {
        if frame["type"] == "controller_pairing_reserved" {
            seen_replays += 1;
        }
    }
    assert_eq!(
        seen_replays, 7,
        "real B serve consumed initial notification plus six retries"
    );
    manager.stop();
    owned.relay.stop_controller_relay(operation.generation());
    tokio::time::timeout(Duration::from_secs(3), owned.worker.take().unwrap())
        .await
        .unwrap()
        .unwrap();
    port.socket.close(None).await.unwrap();
    port.relay.finish();
}
