//! Real authenticated reader/business replay with a synchronous, lock-free
//! capture barrier. Relay/A wire fixtures are local; no provider or renderer.
use super::*;
use crate::features::remote_control::controller::controlled::ControlledLease;
use crate::features::remote_control::controller::controlling_handshake::ControllingHandshake;
use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
use crate::features::remote_control::controller::journal::{JournalEvent, JournalKind};
use crate::features::remote_control::controller::test_support::{
    hex_lower, joined_message, pairing_fixture,
};
use ring::signature::KeyPair as _;
use std::sync::{Arc, mpsc};

pub(crate) struct JournalReplayBarrier {
    armed: std::sync::atomic::AtomicBool,
    entered: mpsc::SyncSender<()>,
    release: std::sync::Mutex<mpsc::Receiver<()>>,
    outcome: mpsc::SyncSender<String>,
}

impl JournalReplayBarrier {
    pub(crate) fn pause_once(&self) -> bool {
        if !self.armed.swap(false, Ordering::AcqRel) {
            return false;
        }
        self.entered.send(()).unwrap();
        // Deliberately synchronous: dropping/aborting the old async worker
        // cannot cancel the in-progress production poll before its J check.
        self.release
            .lock()
            .unwrap()
            .recv_timeout(Duration::from_secs(30))
            .unwrap();
        true
    }
    pub(crate) fn complete(&self, code: &str) {
        self.outcome.send(code.to_owned()).unwrap();
    }
}

struct Connected {
    socket: Socket,
    peer: ControllingLaneState,
    lease: LeaseContext,
    capabilities: Vec<String>,
    relay: RemoteControlManager,
    worker: tokio::task::JoinHandle<()>,
}
impl Drop for Connected {
    fn drop(&mut self) {
        self.relay
            .stop_controller_relay(&self.lease.remote_control_generation);
        self.worker.abort();
    }
}

async fn connect(
    handle: &AppHandle,
    relay: &RemoteControlManager,
    listener: &TcpListener,
    business: Arc<dyn ControlledBusinessPort>,
) -> Connected {
    let manager = handle.state::<ControlledDeviceManager>();
    manager.stop();
    let operation = manager.start(None, Vec::new()).unwrap();
    let mut fixture = pairing_fixture("relay-journal-owner");
    unsafe {
        std::env::set_var("PINVOU_REMOTE_RELAY_SIGNING_KEY_ID", &fixture.relay.key_id);
        std::env::set_var(
            "PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64",
            base64::engine::general_purpose::STANDARD
                .encode(fixture.relay.key_pair.public_key().as_ref()),
        );
    }
    fixture.reservation["remote_control_generation"] = json!(operation.generation());
    fixture.reservation["transcript"]["remote_control_generation"] = json!(operation.generation());
    let mut capabilities: Vec<String> = serde_json::from_value(
        fixture.reservation["transcript"]["negotiated_capabilities"].clone(),
    )
    .unwrap();
    capabilities.push("snapshot_journal_v1".into());
    capabilities.sort_unstable();
    fixture.reservation["transcript"]["capability_hash"] =
        json!(super::super::super::protocol::controller_capability_hash(&capabilities).unwrap());
    fixture.reservation["transcript"]["negotiated_capabilities"] = json!(capabilities);
    let preimage = super::super::super::protocol::build_pairing_transcript_preimage(
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
    let channel = relay
        .start_controller_relay(operation.generation())
        .await
        .unwrap();
    let (tx, mut rx) = tokio::sync::mpsc::channel(128);
    let worker = tokio::spawn(
        ControlledRelayWorker {
            transport_owner: None,
            app: handle.clone(),
            relay: relay.clone(),
            operation: operation.clone(),
            code_wrap: CodeWrapKeyPair::generate().unwrap(),
            handshake: Some(fixture.b_keys),
            channel,
            business,
            action_results: Arc::new(RemoteActionResultCache::default()),
            business_epoch: Arc::new(AtomicU64::new(0)),
            processed_frames: Some(tx),
            before_frame_effect: None,
            activation_probe: None,
            evidence_hooks: None,
        }
        .serve(),
    );
    let mut socket = accept(listener).await;
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
    processed(&mut rx, &fixture.reservation).await;
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
    let lane = ControllingLaneState::default();
    lane.install(HandshakeRole::A, &lease, peer.take_session().unwrap())
        .unwrap();
    send(&mut socket, &joined).await;
    wait_status(
        &manager,
        ControlledStatusKind::Connected,
        "journal-owner-connected",
    )
    .await;
    let (_, capabilities) = manager.live_runtime_context().unwrap();
    assert!(capabilities.iter().any(|v| v == "snapshot_journal_v1"));
    Connected {
        socket,
        peer: lane,
        lease,
        capabilities,
        relay: relay.clone(),
        worker,
    }
}

fn replay_request(lane: &Connected, session: &str, epoch: &str) -> Value {
    lane.peer.seal("journal_replay_request", &json!({
        "v":1,"type":"journal_replay_request","connection_id":lane.lease.connection_id,
        "controller_lease_id":lane.lease.controller_lease_id,"lease_epoch":lane.lease.lease_epoch,
        "relay_epoch":lane.lease.relay_epoch,"remote_control_generation":lane.lease.remote_control_generation,
        "request_id":"request-journal-owner", "session_id":session,"journal_epoch":epoch,"after_sequence":"0"
    }), &lane.capabilities).unwrap()
}

pub(crate) async fn journal_owner_round_trip<T>(
    build_host: impl FnOnce(
        Arc<JournalReplayBarrier>,
    ) -> (AppHandle, Arc<dyn ControlledBusinessPort>, T),
    prepare_session: impl Fn(&AppHandle, &LeaseContext, &str) -> String,
) {
    let directory = tempfile::tempdir().unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("ws://{}/relay", listener.local_addr().unwrap());
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("CODEWHALE_HOME", directory.path().join("cw"));
        std::env::set_var("PINVOU_REMOTE_RELAY_WS_URL", &url);
        std::env::set_var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL", &url);
    }
    let (entered_tx, entered_rx) = mpsc::sync_channel(1);
    let (release_tx, release_rx) = mpsc::sync_channel(1);
    let (outcome_tx, outcome_rx) = mpsc::sync_channel(1);
    let barrier = Arc::new(JournalReplayBarrier {
        armed: std::sync::atomic::AtomicBool::new(false),
        entered: entered_tx,
        release: std::sync::Mutex::new(release_rx),
        outcome: outcome_tx,
    });
    let (handle, business, _host) = build_host(barrier.clone());
    let manager = handle.state::<ControlledDeviceManager>();
    let relay = RemoteControlManager::new(handle.clone());
    let mut old = connect(&handle, &relay, &listener, business.clone()).await;
    let journal = manager.journal();
    let (_, old_owner) = manager.local_journal_context().unwrap();
    let old_session = prepare_session(&handle, &old.lease, "remote-owner-old");
    journal
        .append_for_owner(
            &old_owner,
            "old-event",
            JournalKind::UserMessage,
            &old_session,
            None,
            json!({"text":"old"}),
        )
        .unwrap();
    let old_epoch = journal
        .snapshot_for_owner(&old_owner, &old_session)
        .unwrap()
        .epoch;
    barrier.armed.store(true, Ordering::Release);
    let request = replay_request(&old, "remote-owner-old", &old_epoch);
    send(&mut old.socket, &request).await;
    tokio::task::spawn_blocking(move || entered_rx.recv_timeout(Duration::from_secs(5)).unwrap())
        .await
        .unwrap();
    eprintln!("journal owner fixture: captured old business owner");
    // The endpoint socket normally survives controller-generation changes.
    // This fixture's final connect accepts a fresh socket, so explicitly
    // close the old wire instead of waiting for an upgrade that was not asked
    // for. The synchronous business poll remains paused independently.
    old.socket.close(None).await.unwrap();
    // These 66 transitions intentionally use real manager lifecycle methods,
    // rather than claiming 66 wire handshakes. The first and final lanes do
    // traverse actual signed handshake, reader, worker, map and app business.
    for n in 0..66 {
        manager.stop();
        let operation = manager.start(None, Vec::new()).unwrap();
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
        manager.mark_pairing_reserved(operation.clone()).unwrap();
        manager.mark_handshake_pending(operation.clone()).unwrap();
        let mut context = old.lease.clone();
        context.remote_control_generation = operation.generation().into();
        manager
            .complete_connected(
                operation,
                ControlledLease {
                    context,
                    liveness_epoch_a: "live-owner".into(),
                    resume_deadline_b: now + 30_000,
                    resume_token: [1; 32],
                    transport_key_fingerprint: [2; 32],
                    controller_device_name: "test".into(),
                    connected_at_b: now,
                    negotiated_capabilities: old.capabilities.clone(),
                },
            )
            .unwrap();
        let (_, owner) = manager.local_journal_context().unwrap();
        journal
            .append_for_owner(
                &owner,
                &format!("generation-{n}"),
                JournalKind::UserMessage,
                &old_session,
                None,
                json!({"text":"successor"}),
            )
            .unwrap();
    }
    eprintln!("journal owner fixture: 66 lifecycle rotations complete");
    let mut current = connect(&handle, &relay, &listener, business).await;
    eprintln!("journal owner fixture: successor authenticated");
    let current_session = prepare_session(&handle, &current.lease, "remote-owner-current");
    let (_, owner) = manager.local_journal_context().unwrap();
    journal
        .append_for_owner(
            &owner,
            "current-first",
            JournalKind::UserMessage,
            &current_session,
            None,
            json!({"text":"current"}),
        )
        .unwrap();
    journal
        .append_for_owner(
            &owner,
            "current-second",
            JournalKind::UserMessage,
            "b-second",
            None,
            json!({"text":"second"}),
        )
        .unwrap();
    let before = journal.owner_test_state(&owner);
    release_tx.send(()).unwrap();
    let outcome = tokio::task::spawn_blocking(move || {
        outcome_rx.recv_timeout(Duration::from_secs(5)).unwrap()
    })
    .await
    .unwrap();
    assert_eq!(outcome, "journal_generation_stale");
    assert_eq!(
        journal.owner_test_state(&owner),
        before,
        "late actual app replay must not mutate successor state"
    );
    let no_old_business = tokio::time::timeout(Duration::from_millis(100), async {
        while let Some(Ok(message)) = old.socket.next().await {
            if let Message::Text(text) = message {
                let frame: Value = serde_json::from_str(&text).unwrap();
                assert!(
                    !matches!(
                        frame["payload_kind"].as_str(),
                        Some("journal_event" | "session_snapshot")
                    ),
                    "late old reply: {frame}"
                );
            }
        }
    })
    .await;
    let _ = no_old_business; // timeout or closed old lane are both valid silence.
    let epoch = journal
        .snapshot_for_owner(&owner, &current_session)
        .unwrap()
        .epoch;
    let request = replay_request(&current, "remote-owner-current", &epoch);
    send(&mut current.socket, &request).await;
    let frame = receive(&mut current.socket, "encrypted_frame").await;
    let (kind, payload) = current.peer.open(&frame, &current.capabilities).unwrap();
    assert_eq!(kind, "journal_event");
    assert_eq!(payload["session_id"], "remote-owner-current");
    let event: JournalEvent = serde_json::from_slice(
        &base64::engine::general_purpose::STANDARD
            .decode(payload["event_b64"].as_str().unwrap())
            .unwrap(),
    )
    .unwrap();
    assert_eq!(event.event_id, "current-first");
    assert_eq!(event.session_id, "remote-owner-current");
    manager.stop();
    drop(current);
    drop(old);
    relay.shutdown_loopback_test_endpoint();
}
