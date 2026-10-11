//! Windows-only real Wry/RelayClient/manager/worker wiring regression.
//! Relay/A wire inputs are simulated with real authenticated fixtures. No
//! renderer, real provider, external host, or production deployment is involved.
// architecture-guard: allow-target-cfg -- Test-only real Windows controlled-device Wry host: no cross-OS GUI/unsupported adapter is simulated; the whole module is excluded outside Windows.
#![cfg(target_os = "windows")]
use super::*;
use futures_util::{SinkExt, StreamExt};
use tokio::net::{TcpListener, TcpStream};
use tokio_tungstenite::{WebSocketStream, tungstenite::Message};

type Socket = WebSocketStream<TcpStream>;

async fn accept(listener: &TcpListener) -> Socket {
    let (stream, _) = listener.accept().await.unwrap();
    tokio_tungstenite::accept_hdr_async(
        stream,
        super::super::test_support::ControllerReaderProtocol,
    )
    .await
    .unwrap()
}

async fn send(socket: &mut Socket, value: &Value) {
    if value["v"] == 1 {
        let role = if value["sender_role"] == "A" {
            SenderRole::ControllerA
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

async fn receive(socket: &mut Socket, kind: &str) -> Value {
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

async fn processed(receiver: &mut tokio::sync::mpsc::Receiver<Value>, expected: &Value) {
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

async fn registered(socket: &mut Socket) {
    let frame = receive(socket, "desktop_endpoint_register").await;
    send(
        socket,
        &json!({"v":2,"type":"desktop_endpoint_registered",
        "endpoint_id":frame["endpoint_id"],"web_client_connected":false}),
    )
    .await;
}

async fn wait_status(
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

async fn wait_prepared(manager: &ControlledDeviceManager) {
    tokio::time::timeout(Duration::from_secs(3), async {
        while manager.resume_phase_for_test() != Some(super::super::resume::ResumePhase::Prepared) {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .expect("disconnect must freeze/prepare before the new socket upgrade");
}

// Any unexpected business entry fails rather than returning plausible output.
struct NoBusiness;
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

pub(crate) async fn resume_round_trip<T>(
    build_host: impl FnOnce() -> (tauri::AppHandle, T),
    retire_endpoint: bool,
) {
    use super::super::controlling_handshake::ControllingHandshake;
    use super::super::encrypted_lane::ControllingLaneState;
    use super::super::test_support::{hex_lower, joined_message, pairing_fixture};
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
    let manager = handle.state::<ControlledDeviceManager>();
    let operation = manager.start(None, Vec::new()).unwrap();
    // Match a genuine operation before signing, not a forged post-sign tuple.
    fixture.reservation["remote_control_generation"] = json!(operation.generation());
    fixture.reservation["transcript"]["remote_control_generation"] = json!(operation.generation());
    let preimage = super::super::protocol::build_pairing_transcript_preimage(
        &fixture.reservation["transcript"],
    )
    .unwrap();
    fixture.reservation["transcript_hash"] = json!(hex_lower(&Sha256::digest(&preimage)));
    fixture.reservation["relay_signature_b64"] = json!(
        base64::engine::general_purpose::STANDARD
            .encode(fixture.relay.key_pair.sign(&preimage).as_ref())
    );
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
    assert!(!handle.state::<ControlledLaneState>().is_suspended());
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
    outbound.clear_owned(&successor.sender());
    drop(successor);
}
