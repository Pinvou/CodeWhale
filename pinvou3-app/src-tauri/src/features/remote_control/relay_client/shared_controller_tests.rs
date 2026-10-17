//! Real loopback reader receipts. Tests never construct or write authority.
use super::super::controller::protocol::LeaseContext;
use super::super::controller::test_support::pairing_fixture;
use super::*;
use tokio::net::TcpListener;

enum Wire {
    Frame(Value),
    Close,
}

struct Peer {
    sender: RelaySender,
    inbound: RelayReceiver,
    wire: mpsc::Sender<Wire>,
    received: mpsc::Receiver<Value>,
    server: tokio::task::JoinHandle<()>,
}
impl Drop for Peer {
    fn drop(&mut self) {
        let _ = self.sender.try_send(RelayOutbound::Shutdown);
        self.server.abort();
    }
}

impl Peer {
    async fn new(generation: &str) -> Self {
        Self::with_protocol(generation, true).await
    }

    async fn with_protocol(generation: &str, controller_protocol: bool) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let relay_url = format!("ws://{}/relay", listener.local_addr().unwrap());
        let (wire, mut commands) = mpsc::channel(16);
        let (seen, received) = mpsc::channel(16);
        let server = tokio::spawn(async move {
            if !controller_protocol {
                // Real NoSubProtocol negotiation: the client must reconnect
                // without controller protocol, not just set a test flag.
                let (first, _) = listener.accept().await.unwrap();
                drop(tokio_tungstenite::accept_async(first).await.unwrap());
            }
            loop {
                let (stream, _) = listener.accept().await.unwrap();
                let mut socket = if controller_protocol {
                    tokio_tungstenite::accept_hdr_async(
                        stream,
                        super::super::controller::test_support::ControllerReaderProtocol,
                    )
                    .await
                    .unwrap()
                } else {
                    tokio_tungstenite::accept_async(stream).await.unwrap()
                };
                let Message::Text(register) = socket.next().await.unwrap().unwrap() else {
                    panic!("registration missing")
                };
                let register: Value = serde_json::from_str(&register).unwrap();
                socket
                    .send(Message::Text(
                        json!({"v":2, "type":"desktop_endpoint_registered",
                    "endpoint_id":register["endpoint_id"], "web_client_connected":false})
                        .to_string()
                        .into(),
                    ))
                    .await
                    .unwrap();
                loop {
                    tokio::select! {
                        command = commands.recv() => match command {
                            Some(Wire::Frame(frame)) => socket.send(Message::Text(frame.to_string().into())).await.unwrap(),
                            Some(Wire::Close) => { socket.close(None).await.unwrap(); break; },
                            None => return,
                        },
                        incoming = socket.next() => match incoming {
                            Some(Ok(Message::Text(text))) => {
                                seen.try_send(serde_json::from_str::<Value>(&text).unwrap()).unwrap();
                            },
                            Some(Ok(Message::Ping(body))) => socket.send(Message::Pong(body)).await.unwrap(),
                            Some(Ok(Message::Close(_))) | None => break,
                            Some(Err(_)) => break,
                            _ => {},
                        },
                    }
                }
            }
        });
        let (sender, inbound) = spawn(WebAccessConfig {
            relay_url,
            endpoint_id: "fixture-endpoint".into(),
            access_token: "fixture-browser".into(),
            desktop_secret: "fixture-desktop".into(),
            allow_host_workspace: false,
        });
        assert!(sender.controller_authority.bind(generation).is_some());
        let mut peer = Self {
            sender,
            inbound,
            wire,
            received,
            server,
        };
        peer.connection(true).await;
        peer
    }

    async fn connection(&mut self, connected: bool) {
        tokio::time::timeout(Duration::from_secs(3), async {
            loop {
                if matches!(self.inbound.recv().await, Some(RelayInbound::Connection {connected: actual, ..}) if actual == connected) { break; }
            }
        }).await.expect("real connection boundary timed out");
    }

    async fn read(&mut self, frame: &Value) -> Option<ControllerReadReceipt> {
        self.wire.send(Wire::Frame(frame.clone())).await.unwrap();
        tokio::time::timeout(Duration::from_secs(3), async {
            loop {
                match self.inbound.recv().await.expect("reader exited") {
                    RelayInbound::ControllerMessage { text, receipt } => {
                        let actual: Value = serde_json::from_str(&text).unwrap();
                        if &actual == frame {
                            return Some(receipt);
                        }
                    }
                    RelayInbound::Message(text) => {
                        let actual: Value = serde_json::from_str(&text).unwrap();
                        if &actual == frame {
                            return None;
                        }
                    }
                    _ => {}
                }
            }
        })
        .await
        .expect("real reader frame timed out")
    }

    async fn reconnect(&mut self) {
        self.wire.send(Wire::Close).await.unwrap();
        self.connection(false).await;
        self.connection(true).await;
    }
}

fn context(joined: &Value) -> LeaseContext {
    LeaseContext {
        connection_id: joined["connection_id"].as_str().unwrap().into(),
        controller_lease_id: joined["controller_lease_id"].as_str().unwrap().into(),
        lease_epoch: joined["lease_epoch"].as_str().unwrap().into(),
        relay_epoch: joined["relay_epoch"].as_str().unwrap().into(),
        remote_control_generation: joined["remote_control_generation"].as_str().unwrap().into(),
    }
}

fn final_frame(context: &LeaseContext) -> Value {
    json!({"v":1, "type":"controller_resumed", "request_id":"request-final-0001",
        "connection_id":context.connection_id, "controller_lease_id":context.controller_lease_id,
        "lease_epoch":context.lease_epoch, "relay_epoch":context.relay_epoch,
        "remote_control_generation":context.remote_control_generation, "activation_id":"activation-0001",
        "liveness_epoch_a":"live-a-0002", "liveness_epoch_b":"live-b-0002",
        "resume_deadline_ms":"1700000030000", "new_resume_token_b64":"AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM="})
}

fn capabilities() -> Vec<String> {
    vec![
        "encrypted_frames_v1".into(),
        "role_liveness_v1".into(),
        "task_v1".into(),
    ]
}

fn business(context: &LeaseContext) -> Value {
    json!({"v":1, "type":"encrypted_frame", "connection_id":context.connection_id,
        "controller_lease_id":context.controller_lease_id, "lease_epoch":context.lease_epoch,
        "relay_epoch":context.relay_epoch, "remote_control_generation":context.remote_control_generation,
        "frame_sequence":"1", "direction":"b_to_a", "payload_kind":"task_status",
        "ciphertext_b64":"AQID", "ciphertext_bytes":"3"})
}

#[tokio::test]
async fn shared_real_reader_legacy_fallback_never_mints_controller_authority() {
    let fixture = pairing_fixture("shared-reader-legacy-fallback");
    let context = context(&fixture.joined);
    let mut peer = Peer::with_protocol(&context.remote_control_generation, false).await;
    assert!(
        peer.read(&fixture.joined).await.is_none(),
        "v2 fallback minted join authority"
    );
    assert!(
        peer.read(&final_frame(&context)).await.is_none(),
        "v2 fallback minted final authority"
    );
    assert_eq!(
        peer.sender.try_send_controller_owned(
            &context.remote_control_generation,
            peer.sender
                .controller_authority
                .binding(&context.remote_control_generation)
                .unwrap(),
            business(&context)
        ),
        Err(RelayTrySendError::ControllerProtocolUnavailable)
    );
    let legacy = json!({"v":2, "type":"rpc_response", "request_id":"legacy-request-0001"});
    peer.sender
        .try_send(RelayOutbound::Message(legacy.clone()))
        .unwrap();
    let actual = tokio::time::timeout(Duration::from_secs(3), peer.received.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(actual, legacy, "fallback must preserve actual v2 traffic");
}

#[tokio::test]
async fn shared_real_reader_prepare_targets_one_next_socket_not_old_business() {
    use super::super::controller::relay_transport::ControllerRole;
    use super::super::controller::resume::build_controller_resume_prepare;
    let fixture = pairing_fixture("shared-reader-prepare");
    let context = context(&fixture.joined);
    let mut peer = Peer::new(&context.remote_control_generation).await;
    let authority = peer.sender.controller_authority.clone();
    let joined = peer.read(&fixture.joined).await.unwrap();
    assert!(authority.install_joined(joined, &context, capabilities()));
    let old_business = authority
        .outbound_scope(&business(&context))
        .unwrap()
        .unwrap();
    assert!(authority.frame_is_current(&old_business));
    peer.wire.send(Wire::Close).await.unwrap();
    peer.connection(false).await;
    assert!(!authority.frame_is_current(&old_business));
    assert!(
        peer.sender
            .try_send(RelayOutbound::Message(business(&context)))
            .is_err()
    );
    let prepare = build_controller_resume_prepare(
        &context,
        ControllerRole::ControlledB,
        "request-prepare-0001",
        &[7_u8; 32],
        "BwcHBwcHBwcHBwcHBwcHBw==",
        30_000,
    )
    .unwrap();
    let scope = authority.outbound_scope(&prepare).unwrap().unwrap();
    assert!(!authority.frame_is_current(&scope));
    peer.sender
        .try_send(RelayOutbound::Message(prepare.clone()))
        .unwrap();
    peer.connection(true).await;
    assert!(authority.frame_is_current(&scope));
    let actual = tokio::time::timeout(Duration::from_secs(3), peer.received.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(actual, prepare);
    peer.reconnect().await;
    assert!(
        !authority.frame_is_current(&scope),
        "prepare may target only one next socket"
    );
    assert!(!authority.frame_is_current(&old_business));
}

#[tokio::test]
async fn shared_real_reader_same_generation_replacement_rejects_old_owner() {
    let fixture = pairing_fixture("shared-reader-same-generation");
    let context = context(&fixture.joined);
    let generation = &context.remote_control_generation;
    let mut peer = Peer::new(generation).await;
    let authority = peer.sender.controller_authority.clone();
    let old_binding = authority.binding(generation).unwrap();
    let old_join = peer.read(&fixture.joined).await.unwrap();
    assert!(authority.install_joined(old_join.clone(), &context, capabilities()));
    let old_scope = authority
        .outbound_scope(&business(&context))
        .unwrap()
        .unwrap();
    authority.retire_owned(generation, old_binding);
    let new_binding = authority.bind(generation).unwrap();
    assert_ne!(old_binding, new_binding);
    assert!(!authority.install_joined(old_join, &context, capabilities()));
    let new_join = peer.read(&fixture.joined).await.unwrap();
    assert!(authority.install_joined(new_join, &context, capabilities()));
    assert!(!authority.frame_is_current(&old_scope));
    assert!(
        peer.sender
            .try_send_controller_owned(generation, old_binding, business(&context))
            .is_err()
    );
    authority.retire_owned(generation, old_binding);
    assert_eq!(authority.binding(generation), Some(new_binding));
    peer.sender
        .try_send_controller_owned(generation, new_binding, business(&context))
        .unwrap();
    let actual = tokio::time::timeout(Duration::from_secs(3), peer.received.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(actual, business(&context));
    assert!(
        peer.received.try_recv().is_err(),
        "old owner enqueued a frame"
    );
}

#[tokio::test]
async fn shared_real_reader_current_final_is_bound_and_one_shot() {
    let fixture = pairing_fixture("shared-reader-positive");
    let context = context(&fixture.joined);
    let mut peer = Peer::new(&context.remote_control_generation).await;
    let authority = peer.sender.controller_authority.clone();
    assert!(
        peer.read(&final_frame(&context)).await.is_none(),
        "final cannot invent the original lease"
    );
    let joined = peer.read(&fixture.joined).await.unwrap();
    assert!(authority.install_joined(joined.clone(), &context, capabilities()));
    assert!(!authority.install_joined(joined, &context, capabilities()));
    peer.reconnect().await;
    let final_message = final_frame(&context);
    let receipt = peer.read(&final_message).await.unwrap();
    assert!(!SharedControllerAuthority::default().restore(
        receipt.clone(),
        &context,
        "activation-0001",
        "request-final-0001"
    ));
    assert!(!authority.restore(
        receipt.clone(),
        &context,
        "activation-wrong",
        "request-final-0001"
    ));
    assert!(!authority.restore(
        receipt.clone(),
        &context,
        "activation-0001",
        "request-wrong"
    ));
    let mut wrong_context = context.clone();
    wrong_context.lease_epoch = "2".into();
    assert!(!authority.restore(
        receipt.clone(),
        &wrong_context,
        "activation-0001",
        "request-final-0001"
    ));
    let duplicate = peer.read(&final_message).await.unwrap();
    assert!(authority.restore(
        receipt.clone(),
        &context,
        "activation-0001",
        "request-final-0001"
    ));
    assert!(!authority.restore(receipt, &context, "activation-0001", "request-final-0001"));
    assert!(!authority.restore(duplicate, &context, "activation-0001", "request-final-0001"));
}

#[tokio::test]
async fn shared_real_reader_queued_final_dies_at_socket_close_and_stop() {
    let fixture = pairing_fixture("shared-reader-retired");
    let context = context(&fixture.joined);
    let mut peer = Peer::new(&context.remote_control_generation).await;
    let authority = peer.sender.controller_authority.clone();
    let joined = peer.read(&fixture.joined).await.unwrap();
    assert!(authority.install_joined(joined, &context, capabilities()));
    peer.reconnect().await;
    let final_message = final_frame(&context);
    let old = peer.read(&final_message).await.unwrap();
    // The proof came through the actual reader but remains unconsumed when
    // the receiving socket closes. A replacement must not resurrect it.
    peer.reconnect().await;
    assert!(!authority.restore(old, &context, "activation-0001", "request-final-0001"));
    let current = peer.read(&final_message).await.unwrap();
    peer.sender.try_send(RelayOutbound::Shutdown).unwrap();
    assert!(!authority.restore(current, &context, "activation-0001", "request-final-0001"));
    assert!(!authority.owns(&context.remote_control_generation));
}

#[tokio::test]
async fn shared_real_reader_generation_and_strict_wire_binding_fail_closed() {
    let fixture = pairing_fixture("shared-reader-generation");
    let context = context(&fixture.joined);
    let mut peer = Peer::new(&context.remote_control_generation).await;
    let authority = peer.sender.controller_authority.clone();
    let old_join = peer.read(&fixture.joined).await.unwrap();
    authority.retire(&context.remote_control_generation);
    assert!(authority.bind("generation-new-0001").is_some());
    assert!(!authority.install_joined(old_join, &context, capabilities()));
    assert!(peer.read(&fixture.joined).await.is_none());
    authority.retire(&context.remote_control_generation);
    assert!(
        authority.owns("generation-new-0001"),
        "retiring old generation cannot close its successor"
    );
    authority.retire("generation-new-0001");
    assert!(authority.bind(&context.remote_control_generation).is_some());
    let join = peer.read(&fixture.joined).await.unwrap();
    assert!(authority.install_joined(join, &context, capabilities()));
    peer.reconnect().await;
    let valid = final_frame(&context);
    for field in [
        "connection_id",
        "controller_lease_id",
        "lease_epoch",
        "relay_epoch",
        "remote_control_generation",
    ] {
        let mut wrong = valid.clone();
        wrong[field] = json!("foreign-0001");
        assert!(
            peer.read(&wrong).await.is_none(),
            "foreign tuple field {field} minted authority"
        );
    }
    let mut invalid = valid.clone();
    invalid.as_object_mut().unwrap().remove("request_id");
    assert!(peer.read(&invalid).await.is_none());
    let final_receipt = peer.read(&valid).await.unwrap();
    assert!(authority.restore(
        final_receipt,
        &context,
        "activation-0001",
        "request-final-0001"
    ));
}
