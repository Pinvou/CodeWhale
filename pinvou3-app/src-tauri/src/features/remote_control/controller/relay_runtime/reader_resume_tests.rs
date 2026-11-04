//! Windows-only real Wry/RelayClient/manager/worker wiring regression.
//! Relay/A wire inputs are simulated with real authenticated fixtures. No
//! renderer, real provider, external host, or production deployment is involved.
// architecture-guard: allow-target-cfg -- Test-only real Windows controlled-device Wry host: no cross-OS GUI/unsupported adapter is simulated; the whole module is excluded outside Windows.
// architecture-guard: allow-reader-test-host-type -- Windows test-only real reader regression accepts AppHandle from the app-owned Wry factory; no host construction or event/command API exception.
#![cfg(all(test, target_os = "windows"))]
use super::*;
use futures_util::{SinkExt, StreamExt};
use tokio::net::{TcpListener, TcpStream};
use tokio_tungstenite::{WebSocketStream, tungstenite::Message};

pub(super) type Socket = WebSocketStream<TcpStream>;

#[path = "reader_resume_tests/journal_owner_tests.rs"]
pub(crate) mod journal_owner_tests;

fn fixture_wall_clock_millis() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis()
        .try_into()
        .unwrap()
}

// Reusing a registered endpoint enqueues its transport setup acknowledgement
// before application frames. Skip only that known frame, and leave the final
// Joined receipt as the channel's last authenticated receipt.
async fn next_application_frame(
    channel: &mut crate::features::remote_control::manager::ControllerRelayChannel,
) -> Value {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let frame = channel.recv().await.expect("controller channel closed");
            if frame["v"] == 2 && frame["type"] == "desktop_endpoint_registered" {
                continue;
            }
            return frame;
        }
    })
    .await
    .expect("controller application frame timed out")
}

pub(super) async fn accept(listener: &TcpListener) -> Socket {
    let (stream, _) = listener.accept().await.unwrap();
    tokio_tungstenite::accept_hdr_async(
        stream,
        super::super::test_support::ControllerReaderProtocol,
    )
    .await
    .unwrap()
}

pub(super) async fn send(socket: &mut Socket, value: &Value) {
    if value["v"] == 1 {
        let role = if value["sender_role"] == "A"
            || (value["type"] == "encrypted_frame" && value["direction"] == "a_to_b")
        {
            SenderRole::ControllerA
        } else if value["sender_role"] == "B" {
            SenderRole::ControlledB
        } else {
            SenderRole::Relay
        };
        validate_controller_message_json(&value.to_string(), role).unwrap();
    }
    socket
        .send(Message::Text(value.to_string().into()))
        .await
        .unwrap();
}

pub(super) async fn receive(socket: &mut Socket, kind: &str) -> Value {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            match socket.next().await.unwrap().unwrap() {
                Message::Text(text) => {
                    let frame: Value = serde_json::from_str(&text).unwrap();
                    if frame["type"] == kind {
                        return frame;
                    }
                }
                Message::Ping(body) => socket.send(Message::Pong(body)).await.unwrap(),
                Message::Pong(_) => {}
                other => panic!("unexpected wire message while waiting for {kind}: {other:?}"),
            }
        }
    })
    .await
    .unwrap_or_else(|_| panic!("wire timeout waiting for {kind}"))
}

pub(super) async fn processed(receiver: &mut tokio::sync::mpsc::Receiver<Value>, expected: &Value) {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let frame = receiver
                .recv()
                .await
                .expect("worker exited before processing frame");
            if &frame == expected {
                break;
            }
        }
    })
    .await
    .expect("actual worker completion barrier timed out");
}

pub(super) async fn registered(socket: &mut Socket) {
    let frame = receive(socket, "desktop_endpoint_register").await;
    send(
        socket,
        &json!({"v":2,"type":"desktop_endpoint_registered",
        "endpoint_id":frame["endpoint_id"],"web_client_connected":false}),
    )
    .await;
}

pub(super) async fn wait_status(
    manager: &ControlledDeviceManager,
    expected: ControlledStatusKind,
    stage: &'static str,
) {
    tokio::time::timeout(Duration::from_secs(3), async {
        while manager.status().kind() != expected {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap_or_else(|_| {
        panic!(
            "reader regression stage {stage}: expected {expected:?}, actual {:?}",
            manager.status().kind()
        )
    });
}

pub(super) async fn wait_prepared(manager: &ControlledDeviceManager) {
    tokio::time::timeout(Duration::from_secs(3), async {
        while manager.resume_phase_for_test() != Some(super::super::resume::ResumePhase::Prepared) {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .expect("disconnect must freeze/prepare before the new socket upgrade");
}

// Any unexpected business entry fails rather than returning plausible output.
pub(super) struct NoBusiness;
#[async_trait::async_trait]
impl ControlledBusinessPort for NoBusiness {
    async fn remote_action(
        &self,
        _: &LeaseContext,
        _: &str,
        _: &str,
        _: &Value,
    ) -> Result<Value, String> {
        panic!("unexpected business dispatch")
    }
    async fn capability_snapshot(
        &self,
        _: &LeaseContext,
        _: &str,
        _: &str,
        _: u64,
    ) -> Result<Value, String> {
        panic!("unexpected capability dispatch")
    }
    async fn submit_task(&self, _: &LeaseContext, _: &str, _: &Value) -> Result<Value, String> {
        panic!("unexpected task dispatch")
    }
    async fn cancel_task(
        &self,
        _: &LeaseContext,
        _: &str,
        _: &str,
    ) -> Result<Option<Value>, String> {
        panic!("unexpected cancellation")
    }
    async fn replay_journal(
        &self,
        _: &LeaseContext,
        _: &str,
        _: &str,
        _: u64,
    ) -> Result<Vec<(String, Value)>, String> {
        panic!("unexpected replay")
    }
    async fn session_snapshot(&self, _: &LeaseContext, _: &str, _: &str) -> Result<Value, String> {
        panic!("unexpected snapshot")
    }
}

struct OwnedLane {
    relay: RemoteControlManager,
    generation: String,
    worker: Option<tokio::task::JoinHandle<()>>,
}

impl Drop for OwnedLane {
    fn drop(&mut self) {
        self.relay.stop_controller_relay(&self.generation);
        if let Some(worker) = &self.worker {
            worker.abort();
        }
        self.relay.shutdown_loopback_test_endpoint();
    }
}

// The same actual handshake/reader receipt supports a success control and a
// final-admission expiry test, without a serve timer or reaper winning first.
async fn current_joined_receipt(
    socket: &mut Socket,
    worker: &mut ControlledRelayWorker,
    reserved: &Value,
    a_keys: HandshakeKeyPair,
    trust: super::super::crypto::AuthenticatedRelayTrustConfig,
) -> (ControlledHandshakeDriver, Value) {
    use super::super::controlling_handshake::ControllingHandshake;
    let mut peer = ControllingHandshake::new(
        reserved["request_id"].as_str().unwrap(),
        "claim-0001",
        "2345678",
        a_keys,
        trust,
    );
    let a_commit = peer.accept_reserved(reserved).unwrap();
    let mut driver = ControlledHandshakeDriver::new(worker.handshake.take().unwrap());
    send(socket, reserved).await;
    let incoming = next_application_frame(&mut worker.channel).await;
    assert_eq!(incoming, *reserved);
    let b_commit = worker
        .begin_handshake(&incoming, &mut driver)
        .unwrap()
        .unwrap();
    worker.channel.try_send(b_commit).unwrap();
    worker
        .app
        .state::<ControlledDeviceManager>()
        .mark_handshake_pending_at(worker.operation.clone(), controlled_monotonic_millis)
        .unwrap();
    let b_commit = receive(socket, "finished_receipt_commit").await;
    let a_finished = peer.accept_peer_commitment(&b_commit).unwrap();
    send(socket, &a_commit).await;
    let incoming = tokio::time::timeout(Duration::from_secs(3), worker.channel.recv())
        .await
        .unwrap()
        .unwrap();
    let b_finished = driver.accept_peer_commitment(&incoming).unwrap();
    worker.channel.try_send(b_finished).unwrap();
    let b_finished = receive(socket, "encrypted_handshake_finished").await;
    peer.accept_peer_finished(&b_finished).unwrap();
    send(socket, &a_finished).await;
    let incoming = tokio::time::timeout(Duration::from_secs(3), worker.channel.recv())
        .await
        .unwrap()
        .unwrap();
    let ack = driver.accept_peer_finished(&incoming).unwrap();
    worker.channel.try_send(ack).unwrap();
    receive(socket, "finished_receipt_ack").await;
    let joined = super::super::test_support::joined_message(reserved, [7; 32]);
    peer.accept_joined(&joined).unwrap();
    send(socket, &joined).await;
    let incoming = tokio::time::timeout(Duration::from_secs(3), worker.channel.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(incoming, joined);
    // Do not recv another message: the real channel must retain this Joined
    // receipt for authenticated_transport, rather than a hand-built proof.
    (driver, incoming)
}

pub(crate) async fn resume_round_trip<T>(
    build_host: impl FnOnce() -> (tauri::AppHandle, T),
    retire_endpoint: bool,
) {
    use super::super::controlling_handshake::ControllingHandshake;
    use super::super::encrypted_lane::ControllingLaneState;
    use super::super::test_support::{
        bind_pairing_generation, hex_lower, joined_message, pairing_fixture,
    };
    use ring::signature::KeyPair as _;
    let directory = tempfile::tempdir().unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("ws://{}/relay", listener.local_addr().unwrap());
    let mut fixture = pairing_fixture("relay-reader-resume");
    // Process-private overrides only. Both persisted and active configs are
    // loopback, so even a failure cannot schedule revocation at a real host.
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("PINVOU_REMOTE_RELAY_WS_URL", &url);
        std::env::set_var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL", &url);
        std::env::set_var("PINVOU_REMOTE_RELAY_SIGNING_KEY_ID", &fixture.relay.key_id);
        std::env::set_var(
            "PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64",
            base64::engine::general_purpose::STANDARD
                .encode(fixture.relay.key_pair.public_key().as_ref()),
        );
    }
    // App owns host construction. Keep its guard alive through all readers.
    let (handle, _host) = build_host();
    if std::env::var("PINVOU_READER_FINAL_ADMISSION_SCENARIO").as_deref() == Ok("1") {
        let manager = handle.state::<ControlledDeviceManager>();
        // The app factory manages this exact Relay and registers the production
        // DesktopControlledCleanup adapter. No task exists before pairing.
        let relay = handle.state::<RemoteControlManager>().inner().clone();
        let mut owned = OwnedLane {
            relay: relay.clone(),
            generation: String::new(),
            worker: None,
        };
        // Retain the old channel across production cleanup and successor
        // admission: drop must not be the reason the lane becomes available.
        let old = manager.start(None, Vec::new()).unwrap();
        let mut old_channel = relay
            .start_controller_relay(old.generation())
            .await
            .unwrap();
        let mut connected = accept(&listener).await;
        registered(&mut connected).await;
        let now = controlled_monotonic_millis();
        manager
            .complete_registration(
                old.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: now + 300,
                },
                now,
            )
            .unwrap();
        let deadline = manager
            .preconnect_deadline(&old)
            .unwrap()
            .unwrap()
            .deadline_b();
        manager.poll_watchdog(deadline).unwrap();
        let status = serde_json::to_value(manager.status()).unwrap();
        assert_eq!(status["errorCode"], "desktop_remote_pairing_expired");
        assert!(manager.current_operation().is_none());
        assert!(
            tokio::time::timeout(Duration::from_secs(1), old_channel.recv())
                .await
                .unwrap()
                .is_none()
        );
        let successor = manager.start(None, Vec::new()).unwrap();
        let mut successor_channel = relay
            .start_controller_relay(successor.generation())
            .await
            .unwrap();
        drop(old_channel);
        let cleanup =
            handle.state::<std::sync::Arc<dyn super::super::controlled::ControlledCleanupSink>>();
        cleanup.cleanup(
            old.generation(),
            super::super::controlled::ControlledCleanupScope {
                session_keys: true,
                workspace_grants: true,
                remote_tasks: true,
                pending_permissions: true,
                temporary_models: true,
            },
        );
        let probe = json!({"v":1,"type":"error","request_id":"cleanup_probe","code":"pairing_code_invalid","message":"cleanup probe"});
        send(&mut connected, &probe).await;
        let received = next_application_frame(&mut successor_channel).await;
        assert_eq!(received, probe);
        assert_eq!(
            manager.current_operation().unwrap().generation(),
            successor.generation()
        );
        manager.stop();
        drop(successor_channel);
        let mut socket = Some(connected);
        for scenario in 0..4 {
            let operation = manager.start(None, Vec::new()).unwrap();
            let channel = relay
                .start_controller_relay(operation.generation())
                .await
                .unwrap();
            owned.generation = operation.generation().into();
            if socket.is_none() {
                let mut connected = accept(&listener).await;
                registered(&mut connected).await;
                socket = Some(connected);
            }
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
            let a_keys = HandshakeKeyPair::generate().unwrap();
            let b_keys = HandshakeKeyPair::generate().unwrap();
            // Scenario 0 models a valid Relay whose wall clock is 60 seconds
            // behind B. The reservation is live in that Relay's own domain.
            let relay_wall =
                fixture_wall_clock_millis().saturating_sub(if scenario == 0 { 60_000 } else { 0 });
            let mut reserved = super::super::test_support::signed_reservation_expiring_at(
                &fixture.relay,
                &base64::engine::general_purpose::STANDARD.encode(a_keys.public_key().to_bytes()),
                &base64::engine::general_purpose::STANDARD.encode(b_keys.public_key().to_bytes()),
                relay_wall + 20_000,
            );
            reserved["remote_control_generation"] = json!(operation.generation());
            reserved["transcript"]["remote_control_generation"] = json!(operation.generation());
            let preimage =
                super::super::protocol::build_pairing_transcript_preimage(&reserved["transcript"])
                    .unwrap();
            reserved["transcript_hash"] = json!(hex_lower(&Sha256::digest(&preimage)));
            reserved["relay_signature_b64"] = json!(
                base64::engine::general_purpose::STANDARD
                    .encode(fixture.relay.key_pair.sign(&preimage).as_ref())
            );
            if scenario == 0 {
                // Adversarial outer-only change: still a valid signature over
                // the authenticated transcript. This is not a normal Relay output.
                reserved["handshake_expires_at_ms"] =
                    json!((fixture_wall_clock_millis() + 60_000).to_string());
            } else if scenario == 1 {
                // Reach actual signature verification: update the public hash
                // after changing the signed field, but retain the old signature.
                let changed = (fixture_wall_clock_millis() + 10_000).to_string();
                reserved["transcript"]["expires_at_ms"] = json!(changed);
                reserved["handshake_expires_at_ms"] =
                    reserved["transcript"]["expires_at_ms"].clone();
                let preimage = super::super::protocol::build_pairing_transcript_preimage(
                    &reserved["transcript"],
                )
                .unwrap();
                reserved["transcript_hash"] = json!(hex_lower(&Sha256::digest(&preimage)));
                assert!(
                    super::super::protocol::parse_controller_pairing_transcript(
                        &reserved.to_string()
                    )
                    .is_ok()
                );
            }
            let (processed_tx, mut processed_rx) = tokio::sync::mpsc::channel(8);
            let mut worker = ControlledRelayWorker {
                app: handle.clone(),
                relay: relay.clone(),
                operation: operation.clone(),
                code_wrap: CodeWrapKeyPair::generate().unwrap(),
                handshake: Some(b_keys),
                channel,
                business: std::sync::Arc::new(NoBusiness),
                action_results: std::sync::Arc::new(RemoteActionResultCache::default()),
                business_epoch: std::sync::Arc::new(AtomicU64::new(0)),
                processed_frames: Some(processed_tx),
                transport_owner: None,
                before_frame_effect: None,
                activation_probe: None,
                evidence_hooks: None,
            };
            let socket = socket.as_mut().unwrap();
            if scenario < 2 {
                owned.worker = Some(tokio::spawn(worker.serve()));
                let sent_at_b = controlled_monotonic_millis();
                send(socket, &reserved).await;
                processed(&mut processed_rx, &reserved).await;
                if scenario == 0 {
                    assert_eq!(
                        manager.status().kind(),
                        ControlledStatusKind::HandshakePending,
                        "authenticated reservation must survive a different Relay wall-clock origin"
                    );
                    receive(socket, "finished_receipt_commit").await;
                    let deadline = manager
                        .preconnect_deadline(&operation)
                        .unwrap()
                        .unwrap()
                        .deadline_b();
                    assert!(
                        deadline >= sent_at_b + 20_000
                            && deadline <= controlled_monotonic_millis() + 20_000,
                        "local budget is twenty seconds from authenticated receipt, independent of either wire expiry"
                    );
                }
                tokio::time::timeout(
                    Duration::from_secs(if scenario == 0 { 23 } else { 3 }),
                    owned.worker.take().unwrap(),
                )
                .await
                .unwrap()
                .unwrap();
                let status = serde_json::to_value(manager.status()).unwrap();
                assert_eq!(status["status"], "error");
                assert_eq!(
                    status["errorCode"],
                    if scenario == 0 {
                        "desktop_remote_handshake_timeout"
                    } else {
                        "controller_transcript_untrusted"
                    }
                );
                if scenario == 1 {
                    tokio::time::timeout(Duration::from_millis(100), async {
                        while let Some(Ok(message)) = socket.next().await {
                            match message {
                                Message::Text(text) => assert_ne!(
                                    serde_json::from_str::<Value>(&text).unwrap()["type"],
                                    "finished_receipt_commit"
                                ),
                                Message::Ping(body) => {
                                    socket.send(Message::Pong(body)).await.unwrap()
                                }
                                Message::Pong(_) => {}
                                other => panic!("unexpected socket event: {other:?}"),
                            }
                        }
                        panic!("endpoint must remain live after signature rejection");
                    })
                    .await
                    .unwrap_err();
                }
            } else {
                let (mut driver, joined) = current_joined_receipt(
                    socket,
                    &mut worker,
                    &reserved,
                    a_keys,
                    fixture.relay.trust.clone(),
                )
                .await;
                let deadline = manager
                    .preconnect_deadline(&operation)
                    .unwrap()
                    .unwrap()
                    .deadline_b();
                assert!(
                    controlled_monotonic_millis() < deadline,
                    "handshake setup exhausted its local budget"
                );
                if scenario == 3 {
                    while controlled_monotonic_millis() < deadline {
                        tokio::time::sleep(Duration::from_millis(
                            deadline.saturating_sub(controlled_monotonic_millis()),
                        ))
                        .await;
                    }
                    let diagnostic_path =
                        crate::platform::paths::pinvou3_home().join("logs/remote-control.log");
                    let diagnostic_start = std::fs::read(&diagnostic_path)
                        .map(|bytes| bytes.len())
                        .unwrap_or(0);
                    assert_eq!(
                        worker.complete_handshake(&joined, &mut driver).unwrap_err(),
                        "desktop_remote_handshake_timeout"
                    );
                    // Serve forwards this error to the outer failure helper.
                    // It must not log a second transition after the owner is terminal.
                    worker.fail_handshake("desktop_remote_handshake_timeout");
                    let diagnostic = std::fs::read_to_string(&diagnostic_path).unwrap();
                    assert_eq!(
                        diagnostic[diagnostic_start..]
                            .matches(
                                "stage=handshake_failed detail=desktop_remote_handshake_timeout"
                            )
                            .count(),
                        1
                    );
                    let status = serde_json::to_value(manager.status()).unwrap();
                    assert_eq!(status["status"], "error");
                    assert_eq!(status["errorCode"], "desktop_remote_handshake_timeout");
                    assert!(status.get("pairingCode").is_none());
                } else {
                    worker.complete_handshake(&joined, &mut driver).unwrap();
                    assert_eq!(manager.status().kind(), ControlledStatusKind::Connected);
                    assert!(manager.live_lease().is_ok());
                    assert!(handle.state::<ControlledLaneState>().is_installed());
                    assert!(handle.state::<ControlledOutboundState>().sender().is_some());
                    manager.stop();
                    // Clear this test's installed business state before the
                    // next case; the production sink above retires the Relay.
                    handle.state::<ControlledLaneState>().clear();
                    handle
                        .state::<ControlledOutboundState>()
                        .clear_owned(&worker.channel.sender());
                }
                drop(worker);
            }
            assert!(manager.current_operation().is_none());
            assert!(manager.live_lease().is_err());
            assert!(!handle.state::<ControlledLaneState>().is_installed());
            assert!(handle.state::<ControlledOutboundState>().sender().is_none());
        }
        let next = manager.start(None, Vec::new()).unwrap();
        assert!(
            relay
                .start_controller_relay(next.generation())
                .await
                .is_ok()
        );
        manager.stop();
        return;
    }
    if std::env::var("PINVOU_READER_PRECONNECT_SCENARIO").as_deref() == Ok("1") {
        let manager = handle.state::<ControlledDeviceManager>();
        let relay = RemoteControlManager::new(handle.clone());
        // Exercise the actual retry loop and real RelayClient disconnects.
        // The simulated peer seals valid wire input with the Rust protocol
        // primitive; this does not run the JS Relay or its persistent store.
        let mut socket = Some({
            let manager = handle.state::<ControlledDeviceManager>();
            let operation = manager.start(None, Vec::new()).unwrap();
            let channel = relay
                .start_controller_relay(operation.generation())
                .await
                .unwrap();
            let code_wrap = CodeWrapKeyPair::generate().unwrap();
            let receiver = code_wrap.public_key();
            let mut worker = ControlledRelayWorker {
                app: handle.clone(),
                relay: relay.clone(),
                operation: operation.clone(),
                code_wrap,
                handshake: Some(HandshakeKeyPair::generate().unwrap()),
                channel,
                business: std::sync::Arc::new(NoBusiness),
                action_results: std::sync::Arc::new(RemoteActionResultCache::default()),
                business_epoch: std::sync::Arc::new(AtomicU64::new(0)),
                processed_frames: None,
                transport_owner: None,
                before_frame_effect: None,
                activation_probe: None,
                evidence_hooks: None,
            };
            let before_first_send = controlled_monotonic_millis();
            let task = tokio::spawn(async move {
                let status = worker.register_pairing().await;
                (worker, status)
            });
            let mut socket = accept(&listener).await;
            registered(&mut socket).await;
            let first = receive(&mut socket, "desktop_pairing_register").await;
            let after_first_send = controlled_monotonic_millis();
            let mut last = first.clone();
            for _ in 0..2 {
                // A real WebSocket close reaches the production reader, which sends
                // its retryable disconnect notification to this worker's channel.
                socket.close(None).await.unwrap();
                socket = accept(&listener).await;
                registered(&mut socket).await;
                last = receive(&mut socket, "desktop_pairing_register").await;
                assert_eq!(
                    last, first,
                    "retries must reuse the same registration and keys"
                );
            }
            assert!(controlled_monotonic_millis() > after_first_send);
            let relay_expiry =
                fixture_wall_clock_millis().saturating_sub(60_000) + MAX_PAIRING_CODE_TTL_MS;
            let string = |key: &str| last[key].as_str().unwrap().to_owned();
            let binding = CodeWrapBinding::new(
                string("registration_request_id"),
                "pairing-reader-retry-0001",
                string("remote_control_generation"),
                "relay-reader-retry-0001",
                relay_expiry,
            )
            .unwrap();
            let sealed = CodeWrapKeyPair::generate()
                .unwrap()
                .seal_pairing_code(receiver, &binding, "2345678")
                .unwrap();
            send(&mut socket, &json!({"v":1,"type":"desktop_pairing_registered",
        "request_id":last["request_id"], "registration_request_id":last["registration_request_id"],
        "pairing_id":"pairing-reader-retry-0001", "remote_control_generation":last["remote_control_generation"],
        "relay_epoch":"relay-reader-retry-0001", "expires_at_ms":relay_expiry.to_string(),
        "sealed_code_b64":base64::engine::general_purpose::STANDARD.encode(sealed)})).await;
            let (worker, status) = tokio::time::timeout(Duration::from_secs(3), task)
                .await
                .unwrap()
                .unwrap();
            let status = status.unwrap();
            assert_eq!(status.kind(), ControlledStatusKind::WaitingForPairing);
            assert_eq!(
                serde_json::to_value(status).unwrap()["pairingCode"],
                "2345678"
            );
            let ticket = manager.preconnect_deadline(&operation).unwrap().unwrap();
            assert!(ticket.deadline_b() >= before_first_send + MAX_PAIRING_CODE_TTL_MS);
            assert!(
                ticket.deadline_b() <= after_first_send + MAX_PAIRING_CODE_TTL_MS,
                "the second/third send or final response must not renew the first-send budget"
            );
            assert!(manager.live_lease().is_err());
            assert!(!handle.state::<ControlledLaneState>().is_installed());
            assert!(handle.state::<ControlledOutboundState>().sender().is_none());
            manager.stop();
            relay.stop_controller_relay(operation.generation());
            drop(worker);
            socket
        });
        let mut owned = OwnedLane {
            relay: relay.clone(),
            generation: String::new(),
            worker: None,
        };
        for scenario in 0..3 {
            let diagnostic_path =
                crate::platform::paths::pinvou3_home().join("logs/remote-control.log");
            let diagnostic_start = std::fs::read(&diagnostic_path)
                .map(|bytes| bytes.len())
                .unwrap_or(0);
            let operation = manager.start(None, Vec::new()).unwrap();
            let channel = relay
                .start_controller_relay(operation.generation())
                .await
                .unwrap();
            owned.generation = operation.generation().into();
            if socket.is_none() {
                let mut connected = accept(&listener).await;
                registered(&mut connected).await;
                socket = Some(connected);
            }
            let now = controlled_monotonic_millis();
            manager
                .complete_registration(
                    operation.clone(),
                    ControlledRegistration {
                        pairing_code: "2345678".into(),
                        sealed_code_b64: "sealed".into(),
                        expires_at_ms: now + if scenario == 0 { 300 } else { 10_000 },
                    },
                    now,
                )
                .unwrap();
            let (processed_tx, mut processed_rx) = tokio::sync::mpsc::channel(128);
            let keys = HandshakeKeyPair::generate().unwrap();
            // A valid reservation from a Relay whose wall clock is 60 seconds
            // behind B still receives one real local twenty-second budget.
            fixture.reservation = super::super::test_support::signed_reservation_expiring_at(
                &fixture.relay,
                &base64::engine::general_purpose::STANDARD
                    .encode(fixture.a_keys.public_key().to_bytes()),
                &base64::engine::general_purpose::STANDARD.encode(keys.public_key().to_bytes()),
                fixture_wall_clock_millis().saturating_sub(60_000) + 20_000,
            );
            fixture.reservation["remote_control_generation"] = json!(operation.generation());
            fixture.reservation["transcript"]["remote_control_generation"] =
                json!(operation.generation());
            let preimage = super::super::protocol::build_pairing_transcript_preimage(
                &fixture.reservation["transcript"],
            )
            .unwrap();
            fixture.reservation["transcript_hash"] = json!(hex_lower(&Sha256::digest(&preimage)));
            fixture.reservation["relay_signature_b64"] = json!(
                base64::engine::general_purpose::STANDARD
                    .encode(fixture.relay.key_pair.sign(&preimage).as_ref())
            );
            let worker = ControlledRelayWorker {
                app: handle.clone(),
                relay: relay.clone(),
                operation: operation.clone(),
                code_wrap: CodeWrapKeyPair::generate().unwrap(),
                handshake: Some(keys),
                channel,
                business: std::sync::Arc::new(NoBusiness),
                action_results: std::sync::Arc::new(RemoteActionResultCache::default()),
                business_epoch: std::sync::Arc::new(AtomicU64::new(0)),
                processed_frames: Some(processed_tx),
                transport_owner: None,
                before_frame_effect: None,
                activation_probe: None,
                evidence_hooks: None,
            };
            owned.worker = Some(tokio::spawn(worker.serve()));
            let socket = socket.as_mut().unwrap();
            if scenario != 0 {
                let sent_at_b = controlled_monotonic_millis();
                send(socket, &fixture.reservation).await;
                processed(&mut processed_rx, &fixture.reservation).await;
                assert_eq!(
                    manager.status().kind(),
                    ControlledStatusKind::HandshakePending
                );
                let b_commit = receive(socket, "finished_receipt_commit").await;
                assert_eq!(
                    manager.status().kind(),
                    ControlledStatusKind::HandshakePending
                );
                let original = manager.preconnect_deadline(&operation).unwrap().unwrap();
                assert!(
                    original.deadline_b() >= sent_at_b + 20_000
                        && original.deadline_b() <= controlled_monotonic_millis() + 20_000
                );
                if scenario == 2 {
                    for _ in 0..8 {
                        let unrelated = json!({"v":1,"type":"error","request_id":"unrelated-preconnect-request",
                            "code":"pairing_code_invalid","message":"reservation expired"});
                        send(socket, &unrelated).await;
                        processed(&mut processed_rx, &unrelated).await;
                        // Relay's real self-echo path must also leave the budget fixed.
                        send(socket, &b_commit).await;
                        processed(&mut processed_rx, &b_commit).await;
                        assert_eq!(
                            manager.preconnect_deadline(&operation).unwrap(),
                            Some(original.clone())
                        );
                    }
                }
            }
            // No status poll/reaper drives expiration: the actual socket/worker does.
            tokio::time::timeout(
                Duration::from_secs(if scenario == 0 { 3 } else { 23 }),
                owned.worker.take().unwrap(),
            )
            .await
            .unwrap()
            .unwrap();
            let status = manager.status();
            assert_eq!(status.kind(), ControlledStatusKind::Error);
            let status = serde_json::to_value(status).unwrap();
            let expected_error = if scenario == 0 {
                "desktop_remote_pairing_expired"
            } else {
                "desktop_remote_handshake_timeout"
            };
            assert_eq!(status["errorCode"].as_str(), Some(expected_error));
            let diagnostic = std::fs::read_to_string(&diagnostic_path).unwrap();
            assert_eq!(
                diagnostic[diagnostic_start..]
                    .matches(&format!("stage=preconnect_expired detail={expected_error}"))
                    .count(),
                1
            );
            assert!(status.get("pairingCode").is_none());
            assert!(manager.current_operation().is_none());
            assert!(manager.live_lease().is_err());
            assert!(!handle.state::<ControlledLaneState>().is_installed());
            assert!(handle.state::<ControlledOutboundState>().sender().is_none());
            // The next loop must acquire a fresh channel on the same still-live socket.
        }
        let next = manager.start(None, Vec::new()).unwrap();
        assert_eq!(manager.status().kind(), ControlledStatusKind::Registering);
        assert!(
            relay
                .start_controller_relay(next.generation())
                .await
                .is_ok()
        );
        manager.stop();
        relay.shutdown_loopback_test_endpoint();
        return;
    }
    let owner_case = std::env::var("PINVOU_READER_OWNER_CASE").ok();
    let race = match owner_case.as_deref() {
        Some("disconnect") => {
            Some(super::owner_interleaving_tests::OwnerRaceCase::ReceivedDisconnect)
        }
        Some("joined") => Some(super::owner_interleaving_tests::OwnerRaceCase::ReceivedJoined),
        Some("challenge") => Some(super::owner_interleaving_tests::OwnerRaceCase::OldChallenge),
        _ => None,
    };
    if let Some(race) = race {
        let manager = handle.state::<ControlledDeviceManager>();
        let business = handle.state::<ControlledLaneState>();
        let outbound = handle.state::<ControlledOutboundState>();
        let relay = RemoteControlManager::new(handle.clone());
        super::owner_interleaving_tests::ownership_round_trip(
            manager.inner(),
            business.inner(),
            outbound.inner(),
            &relay,
            &listener,
            |input| ControlledRelayWorker {
                transport_owner: None,
                app: handle.clone(),
                relay: relay.clone(),
                operation: input.operation,
                code_wrap: CodeWrapKeyPair::generate().unwrap(),
                handshake: Some(input.keys),
                channel: input.channel,
                business: std::sync::Arc::new(NoBusiness),
                action_results: std::sync::Arc::new(RemoteActionResultCache::default()),
                business_epoch: std::sync::Arc::new(AtomicU64::new(0)),
                processed_frames: Some(input.processed),
                before_frame_effect: input.before_effect,
                activation_probe: None,
                evidence_hooks: input.evidence_hooks,
            },
            race,
        )
        .await;
        return;
    }
    let observe_activation = owner_case.as_deref() == Some("activation");
    let activation_observed = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let activation_probe = observe_activation.then(|| {
        let handle = handle.clone();
        let observed = activation_observed.clone();
        Box::new(move || {
            assert!(
                handle
                    .state::<ControlledDeviceManager>()
                    .lifecycle_lock_held_for_test(),
                "actual resume activation escaped the C ownership commit"
            );
            observed.fetch_add(1, Ordering::SeqCst);
        }) as Box<dyn FnOnce() + Send + Sync>
    });
    let manager = handle.state::<ControlledDeviceManager>();
    let operation = manager.start(None, Vec::new()).unwrap();
    bind_pairing_generation(&mut fixture, operation.generation());
    let joined = joined_message(&fixture.reservation, fixture.resume_token);
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
        relay: relay.clone(),
        operation: operation.clone(),
        code_wrap: CodeWrapKeyPair::generate().unwrap(),
        handshake: Some(fixture.b_keys),
        channel,
        business: std::sync::Arc::new(NoBusiness),
        action_results: std::sync::Arc::new(RemoteActionResultCache::default()),
        business_epoch: std::sync::Arc::new(AtomicU64::new(0)),
        processed_frames: Some(processed_tx),
        before_frame_effect: None,
        activation_probe,
        evidence_hooks: None,
    };
    owned.worker = Some(tokio::spawn(worker.serve()));
    let mut socket = accept(&listener).await;
    registered(&mut socket).await;
    let mut peer = ControllingHandshake::new(
        fixture.reservation["request_id"].as_str().unwrap(),
        "claim-0001",
        "2345678",
        fixture.a_keys,
        fixture.relay.trust,
    );
    let a_commit = peer.accept_reserved(&fixture.reservation).unwrap();
    send(&mut socket, &fixture.reservation).await;
    processed(&mut processed_rx, &fixture.reservation).await;
    assert_eq!(
        manager.status().kind(),
        ControlledStatusKind::HandshakePending
    );
    let b_commit = receive(&mut socket, "finished_receipt_commit").await;
    let a_finished = peer.accept_peer_commitment(&b_commit).unwrap();
    send(&mut socket, &a_commit).await;
    let b_finished = receive(&mut socket, "encrypted_handshake_finished").await;
    peer.accept_peer_finished(&b_finished).unwrap();
    send(&mut socket, &a_finished).await;
    receive(&mut socket, "finished_receipt_ack").await;
    let accepted = peer.accept_joined(&joined).unwrap();
    let lease = LeaseContext {
        connection_id: accepted.connection_id,
        controller_lease_id: accepted.controller_lease_id,
        lease_epoch: accepted.lease_epoch.to_string(),
        relay_epoch: accepted.relay_epoch,
        remote_control_generation: accepted.remote_control_generation,
    };
    let a_lane = ControllingLaneState::default();
    a_lane
        .install(HandshakeRole::A, &lease, peer.take_session().unwrap())
        .unwrap();
    send(&mut socket, &joined).await;
    wait_status(
        &manager,
        ControlledStatusKind::Connected,
        "initial_connected",
    )
    .await;
    if retire_endpoint {
        // Unlike the direct channel rotation regression, this retires a live
        // worker after the full authenticated handshake and actual joined frame.
        relay.refresh().unwrap();
        tokio::time::timeout(Duration::from_secs(3), owned.worker.take().unwrap())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(manager.status().kind(), ControlledStatusKind::Error);
        assert!(manager.live_lease().is_err());
        assert!(!handle.state::<ControlledLaneState>().is_installed());
        assert!(handle.state::<ControlledOutboundState>().sender().is_none());
        let _new_socket = lifecycle_registered(&listener).await;
        // Waiting-for-pairing has no watchdog. EOF must retire it immediately,
        // not leave a code and permanently busy state with no receiving worker.
        let waiting = manager.start(None, Vec::new()).unwrap();
        manager
            .complete_registration(
                waiting.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: controlled_monotonic_millis() + 30_000,
                },
                controlled_monotonic_millis(),
            )
            .unwrap();
        let channel = relay
            .start_controller_relay(waiting.generation())
            .await
            .unwrap();
        let worker = ControlledRelayWorker {
            transport_owner: None,
            app: handle.clone(),
            relay: relay.clone(),
            operation: waiting.clone(),
            code_wrap: CodeWrapKeyPair::generate().unwrap(),
            handshake: Some(HandshakeKeyPair::generate().unwrap()),
            channel,
            business: std::sync::Arc::new(NoBusiness),
            action_results: std::sync::Arc::new(RemoteActionResultCache::default()),
            business_epoch: std::sync::Arc::new(AtomicU64::new(0)),
            processed_frames: None,
            before_frame_effect: None,
            activation_probe: None,
            evidence_hooks: None,
        };
        let waiting_worker = tokio::spawn(worker.serve());
        relay.stop_current().unwrap();
        tokio::time::timeout(Duration::from_secs(3), waiting_worker)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(manager.status().kind(), ControlledStatusKind::Error);
        assert!(
            serde_json::to_value(manager.status())
                .unwrap()
                .get("pairingCode")
                .is_none()
        );
        assert!(manager.current_operation().is_none());
        assert!(
            manager.start(None, Vec::new()).is_ok(),
            "retired waiting worker must not leave busy"
        );
        manager.stop();
        return;
    }
    let frozen = manager.watchdog_deadline_for_test().unwrap();
    // A real socket close goes through RelayClient's connection event and the
    // manager reader, rather than directly invoking a resume helper.
    socket.close(None).await.unwrap();
    drop(socket);
    // No new upgrade can complete until this test accepts its socket. Force
    // the real disconnected branch first, rather than depending on scheduling.
    wait_prepared(&manager).await;
    assert!(!owned.worker.as_ref().unwrap().is_finished());
    assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
    let mut socket = accept(&listener).await;
    registered(&mut socket).await;
    let prepare = receive(&mut socket, "controller_resume_prepare").await;
    assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
    assert!(handle.state::<ControlledLaneState>().is_suspended());
    let activation = "activation-reader-0001";
    let provisional = json!({
        "v":1, "type":"controller_resume_provisional", "request_id":prepare["request_id"],
        "connection_id":prepare["connection_id"], "controller_lease_id":prepare["controller_lease_id"],
        "lease_epoch":prepare["lease_epoch"], "relay_epoch":prepare["relay_epoch"],
        "remote_control_generation":prepare["remote_control_generation"],
        "activation_id":activation, "original_deadline_ms":prepare["original_deadline_ms"],
    });
    let mut stale = provisional.clone();
    stale["remote_control_generation"] = json!("generation-retired");
    let before_stale = manager.status().kind();
    let before_epoch = manager.retained_liveness_epoch_for_test();
    assert!(
        before_epoch.is_some(),
        "observe retained evidence, not a None projection"
    );
    send(&mut socket, &stale).await;
    processed(&mut processed_rx, &stale).await;
    assert_eq!(
        manager.resume_phase_for_test(),
        Some(super::super::resume::ResumePhase::Prepared)
    );
    assert_eq!(
        manager.status().kind(),
        before_stale,
        "retired generation must not advance resume"
    );
    assert_eq!(manager.retained_liveness_epoch_for_test(), before_epoch);
    assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
    send(&mut socket, &provisional).await;
    receive(&mut socket, "encrypted_resume_confirm").await;
    assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
    assert_eq!(
        manager.status().kind(),
        ControlledStatusKind::ResumeProvisional
    );
    // Crucially, bare encrypted_resume_confirm must traverse the manager
    // allowlist: dropping it makes the ACK wait fail, not a zero-test pass.
    let wrong = a_lane
        .seal_resume_confirm("request-peer-wrong", "activation-wrong")
        .unwrap();
    send(&mut socket, &wrong).await;
    processed(&mut processed_rx, &wrong).await;
    assert_eq!(
        manager.resume_phase_for_test(),
        Some(super::super::resume::ResumePhase::Provisional)
    );
    assert_eq!(
        manager.status().kind(),
        ControlledStatusKind::ResumeProvisional,
        "wrong activation must not confirm or produce an ACK"
    );
    assert_eq!(manager.retained_liveness_epoch_for_test(), before_epoch);
    assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
    let confirm = a_lane
        .seal_resume_confirm("request-peer-current", activation)
        .unwrap();
    send(&mut socket, &confirm).await;
    receive(&mut socket, "controller_resume_activation_ack").await;
    assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
    let mut pending = provisional.clone();
    pending["type"] = json!("controller_resume_activation_pending");
    send(&mut socket, &pending).await;
    let liveness = receive(&mut socket, "controller_post_activation_liveness").await;
    let mut confirmed = pending.clone();
    confirmed
        .as_object_mut()
        .unwrap()
        .remove("original_deadline_ms");
    confirmed["type"] = json!("controller_post_activation_liveness_confirmed");
    confirmed["liveness_epoch_b"] = liveness["liveness_epoch_b"].clone();
    confirmed["heartbeat_sequence_b"] = json!("1");
    send(&mut socket, &confirmed).await;
    receive(&mut socket, "controller_resume_ready_ack").await;
    assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
    let mut resumed = confirmed.clone();
    resumed
        .as_object_mut()
        .unwrap()
        .remove("heartbeat_sequence_b");
    resumed["type"] = json!("controller_resumed");
    resumed["liveness_epoch_a"] = json!("live-a-reader-2");
    resumed["resume_deadline_ms"] = json!("1700000030000");
    resumed["new_resume_token_b64"] =
        json!(base64::engine::general_purpose::STANDARD.encode([3_u8; 32]));
    let mut wrong_final = resumed.clone();
    wrong_final["liveness_epoch_b"] = json!("liveness-b-retired-0001");
    send(&mut socket, &wrong_final).await;
    processed(&mut processed_rx, &wrong_final).await;
    assert_eq!(
        manager.status().kind(),
        ControlledStatusKind::ResumeActivatedPendingLiveness
    );
    assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
    assert_eq!(manager.retained_liveness_epoch_for_test(), before_epoch);
    send(&mut socket, &resumed).await;
    wait_status(
        &manager,
        ControlledStatusKind::Connected,
        "resumed_connected",
    )
    .await;
    processed(&mut processed_rx, &resumed).await;
    assert!(!handle.state::<ControlledLaneState>().is_suspended());
    if observe_activation {
        assert_eq!(
            activation_observed.load(Ordering::SeqCst),
            1,
            "the real reader final must reach the in-commit activation exactly once"
        );
    }
    assert_eq!(
        manager.live_liveness_epoch().as_deref(),
        Some("live-a-reader-2")
    );
    // Final authenticated activation establishes a new cycle. A second
    // disconnect freezes that cycle's existing deadline, not now + 30 seconds.
    let second_frozen = manager.watchdog_deadline_for_test().unwrap();
    socket.close(None).await.unwrap();
    drop(socket);
    wait_prepared(&manager).await;
    let mut socket = accept(&listener).await;
    registered(&mut socket).await;
    let second_prepare = receive(&mut socket, "controller_resume_prepare").await;
    assert_eq!(manager.watchdog_deadline_for_test(), Some(second_frozen));
    let foreign_error = json!({"v":1,"type":"error","request_id":"request-foreign",
        "code":"invalid_message","message":"rejected"});
    let before_foreign = manager.status().kind();
    let before_foreign_epoch = manager.retained_liveness_epoch_for_test();
    assert!(before_foreign_epoch.is_some());
    send(&mut socket, &foreign_error).await;
    processed(&mut processed_rx, &foreign_error).await;
    assert_eq!(
        manager.resume_phase_for_test(),
        Some(super::super::resume::ResumePhase::Prepared)
    );
    assert_eq!(
        manager.status().kind(),
        before_foreign,
        "foreign request cannot fail this recovery"
    );
    assert_eq!(manager.watchdog_deadline_for_test(), Some(second_frozen));
    assert_eq!(
        manager.retained_liveness_epoch_for_test(),
        before_foreign_epoch
    );
    send(
        &mut socket,
        &json!({"v":1,"type":"error","request_id":second_prepare["request_id"],
        "code":"invalid_message","message":"rejected"}),
    )
    .await;
    wait_status(&manager, ControlledStatusKind::Error, "own_error_closed").await;
    tokio::time::timeout(Duration::from_secs(3), owned.worker.take().unwrap())
        .await
        .unwrap()
        .unwrap();
    assert!(manager.live_lease().is_err());
    // Dropping the worker really releases the generation lane, so a new
    // subscriber can start; do not merely assert a routing predicate.
    let replacement = relay
        .start_controller_relay(operation.generation())
        .await
        .unwrap();
    drop(replacement);
    let retired_reader = relay.replace_loopback_reader_for_test();
    let mut replacement_socket = accept(&listener).await;
    registered(&mut replacement_socket).await;
    // Registration forwarding precedes the manager projection. Wait for the
    // real projection before subscribing, so an in-flight real registration
    // cannot race its synthetic replay into this routing-negative assertion.
    tokio::time::timeout(Duration::from_secs(3), async {
        while relay.status().status
            != crate::features::remote_control::protocol::WebAccessStatusKind::WaitingWebClient
        {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .expect("replacement reader registration was not committed");
    let mut replacement = relay
        .start_controller_relay("generation-reader-successor")
        .await
        .unwrap();
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let frame = replacement.recv().await.expect("replacement reader exited");
            if frame["type"] == "desktop_endpoint_registered" {
                break;
            }
            assert_eq!(frame["type"], "desktop_relay_connection");
        }
    })
    .await
    .expect("replacement registration timed out");
    relay.assert_retired_reader_fenced_for_test(&retired_reader, &mut replacement);
    endpoint_rotation_round_trip(
        &relay,
        &listener,
        replacement,
        &handle.state::<ControlledOutboundState>(),
    )
    .await;
}

/// Refresh/stop run their real persistence and socket lifecycle in this private
/// loopback process. Revocation sockets use legacy v2 (no controller protocol).
struct LifecycleProtocol;
impl tokio_tungstenite::tungstenite::handshake::server::Callback for LifecycleProtocol {
    fn on_request(
        self,
        request: &tokio_tungstenite::tungstenite::handshake::server::Request,
        mut response: tokio_tungstenite::tungstenite::handshake::server::Response,
    ) -> Result<
        tokio_tungstenite::tungstenite::handshake::server::Response,
        tokio_tungstenite::tungstenite::handshake::server::ErrorResponse,
    > {
        if let Some(protocol) = request.headers().get("Sec-WebSocket-Protocol") {
            assert_eq!(protocol, super::super::protocol::CONTROLLER_SUBPROTOCOL);
            response
                .headers_mut()
                .insert("Sec-WebSocket-Protocol", protocol.clone());
        }
        Ok(response)
    }
}

async fn lifecycle_registered(listener: &TcpListener) -> Socket {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let (stream, _) = listener.accept().await.unwrap();
            let mut socket = tokio_tungstenite::accept_hdr_async(stream, LifecycleProtocol)
                .await
                .unwrap();
            let Message::Text(text) = socket.next().await.unwrap().unwrap() else {
                panic!("lifecycle first frame must be registration or revocation");
            };
            let frame: Value = serde_json::from_str(&text).unwrap();
            match frame["type"].as_str().unwrap() {
                "desktop_endpoint_revoke" => {
                    send(
                        &mut socket,
                        &json!({"v":2,"type":"desktop_endpoint_revoked",
                        "endpoint_id":frame["endpoint_id"]}),
                    )
                    .await;
                }
                "desktop_endpoint_register" => {
                    send(
                        &mut socket,
                        &json!({"v":2,"type":"desktop_endpoint_registered",
                        "endpoint_id":frame["endpoint_id"],"web_client_connected":false}),
                    )
                    .await;
                    return socket;
                }
                other => panic!("unexpected lifecycle frame {other}"),
            }
        }
    })
    .await
    .expect("lifecycle loopback registration timeout")
}

async fn assert_lifecycle_route(
    socket: &mut Socket,
    lane: &mut crate::features::remote_control::manager::ControllerRelayChannel,
) {
    let marker = json!({"v":1,"type":"error","request_id":"request-lifecycle-route",
        "code":"invalid_message","message":"rejected"});
    send(socket, &marker).await;
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let frame = lane.recv().await.expect("successor lane must stay open");
            if frame == marker {
                return;
            }
            assert!(matches!(
                frame["type"].as_str(),
                Some("desktop_relay_connection" | "desktop_endpoint_registered")
            ));
        }
    })
    .await
    .expect("new reader did not route to the new lane");
}

async fn endpoint_rotation_round_trip(
    relay: &RemoteControlManager,
    listener: &TcpListener,
    mut old: crate::features::remote_control::manager::ControllerRelayChannel,
    outbound: &ControlledOutboundState,
) {
    let old_sender = old.sender();
    outbound.install(old_sender.clone());
    relay.queue_controller_frame_for_loopback_test(json!({"type": "encrypted_frame"}));
    relay.refresh().unwrap();
    assert!(old_sender.try_send(json!({})).is_err());
    assert!(
        tokio::time::timeout(Duration::from_secs(3), old.recv())
            .await
            .unwrap()
            .is_none(),
        "retired channel must discard frames queued before refresh"
    );
    // Reuse the generation deliberately: old Drop must check socket+binding,
    // not clear a successor just because its generation string matches.
    let mut current = relay
        .start_controller_relay(old.generation())
        .await
        .unwrap();
    outbound.install(current.sender());
    outbound.clear_owned(&old_sender);
    assert!(outbound.sender().unwrap().same_binding(&current.sender()));
    let mut socket = lifecycle_registered(listener).await;
    drop(old);
    assert_lifecycle_route(&mut socket, &mut current).await;
    let old_sender = current.sender();
    relay.queue_controller_frame_for_loopback_test(json!({"type": "encrypted_frame"}));
    relay.stop_current().unwrap();
    assert!(old_sender.try_send(json!({})).is_err());
    assert!(
        tokio::time::timeout(Duration::from_secs(3), current.recv())
            .await
            .unwrap()
            .is_none(),
        "retired channel must discard frames queued before stop"
    );
    // This first start after stop has no endpoint. Its preinstalled lane must
    // survive activate, while the stopped lane cannot cause permanent busy.
    let mut successor = relay
        .start_controller_relay(current.generation())
        .await
        .unwrap();
    outbound.install(successor.sender());
    outbound.clear_owned(&old_sender);
    assert!(outbound.sender().unwrap().same_binding(&successor.sender()));
    let mut successor_socket = lifecycle_registered(listener).await;
    drop(current);
    assert_lifecycle_route(&mut successor_socket, &mut successor).await;
    // Exercise a real unsolicited endpoint revocation while its lane is alive.
    // A plain endpoint.take() leaves the sender/generation installed forever.
    let revoked_id = relay.controller_endpoint_id().unwrap();
    outbound.clear_owned(&successor.sender());
    let mut revoked = successor;
    let old_sender = revoked.sender();
    send(
        &mut successor_socket,
        &json!({"v":2,"type":"desktop_endpoint_revoked","endpoint_id":revoked_id}),
    )
    .await;
    tokio::time::timeout(Duration::from_secs(3), async {
        while relay.status().status
            != crate::features::remote_control::protocol::WebAccessStatusKind::Revoked
        {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .expect("live endpoint revocation did not retire the endpoint");
    assert!(old_sender.try_send(json!({})).is_err());
    assert!(
        tokio::time::timeout(Duration::from_secs(3), revoked.recv())
            .await
            .unwrap()
            .is_none()
    );
    // Deliberately reuse the generation: old Drop and late reader callbacks
    // must not retire the successor's different socket/binding.
    let mut restarted = relay
        .start_controller_relay(revoked.generation())
        .await
        .unwrap();
    let mut restarted_socket = lifecycle_registered(listener).await;
    drop(revoked);
    assert_lifecycle_route(&mut restarted_socket, &mut restarted).await;
    drop(restarted);
}

/// The connection-level controller-lane outage signal (synthesized locally when
/// the relay negotiates only legacy v2, or relay-sent for a deployment without
/// the controller dispatcher) must reach the bound controller channel without
/// poisoning the web endpoint status; endpoint-scoped legacy errors keep their
/// existing status projection and stay out of the controller lane.
///
/// The driver builds the host and the manager so this fixture never names a
/// host type: the architecture guard permits exactly one such factory here.
pub(crate) async fn controller_lane_outage_round_trip<T>(
    build_relay: impl FnOnce() -> (RemoteControlManager, T),
) {
    let directory = tempfile::tempdir().unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("ws://{}/relay", listener.local_addr().unwrap());
    // Process-private overrides only. Both persisted and active configs are
    // loopback, so even a failure cannot schedule revocation at a real host.
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("PINVOU_REMOTE_RELAY_WS_URL", &url);
        std::env::set_var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL", &url);
    }
    // The driver owns host construction. Keep its guard alive through the lane.
    let (relay, _host) = build_relay();
    let mut channel = relay
        .start_controller_relay("generation-lane-outage")
        .await
        .unwrap();
    let _owned = OwnedLane {
        relay: relay.clone(),
        generation: "generation-lane-outage".into(),
        worker: None,
    };
    let mut socket = accept(&listener).await;
    registered(&mut socket).await;

    // relay_client::notify_controller_unavailable 的精确帧形状：
    // v2、无 endpoint_id、无 request_id。
    let signal = json!({"v":2,"type":"error","code":"desktop_controller_unavailable",
        "message":"desktop controller is unavailable"});
    send(&mut socket, &signal).await;
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let frame = channel
                .recv()
                .await
                .expect("controller lane must stay open");
            if frame == signal {
                return;
            }
            assert!(matches!(
                frame["type"].as_str(),
                Some("desktop_relay_connection" | "desktop_endpoint_registered")
            ));
        }
    })
    .await
    .expect("controller-lane outage signal did not reach the bound channel");
    // One socket's frames are processed in order: observing the signal means
    // the earlier registration projection is already committed.
    let status = relay.status();
    assert_eq!(
        status.status,
        crate::features::remote_control::protocol::WebAccessStatusKind::WaitingWebClient,
        "connection-level controller-lane signal must not poison web endpoint status"
    );
    assert_eq!(status.last_error, None);

    // 同 code 但带匹配 endpoint_id 的帧仍是端点级 legacy 错误：保持原有状态
    // 投影，且不进入 controller 通道。
    let scoped = json!({"v":2,"type":"error","code":"desktop_controller_unavailable",
        "message":"endpoint-scoped outage",
        "endpoint_id":relay.controller_endpoint_id().unwrap()});
    send(&mut socket, &scoped).await;
    tokio::time::timeout(Duration::from_secs(3), async {
        while relay.status().status
            != crate::features::remote_control::protocol::WebAccessStatusKind::Error
        {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .expect("endpoint-scoped legacy error kept its status projection");
    assert_eq!(
        relay.status().last_error.as_deref(),
        Some("endpoint-scoped outage")
    );
    assert!(
        tokio::time::timeout(Duration::from_millis(200), channel.recv())
            .await
            .is_err(),
        "endpoint-scoped legacy error must not enter the controller lane"
    );
}
