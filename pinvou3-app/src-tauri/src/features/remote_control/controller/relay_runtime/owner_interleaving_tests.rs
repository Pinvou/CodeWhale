//! Real Wry / desktop reader / manager / worker ownership interleavings.
//! Relay and A are controlled loopback peers using the production handshake.
// architecture-guard: allow-target-cfg -- Real Windows Wry test host only; unsupported GUI adapters are not simulated.
#![cfg(all(test, target_os = "windows"))]

use super::super::controlling_handshake::ControllingHandshake;
use super::super::encrypted_lane::ControllingLaneState;
use super::super::test_support::{bind_pairing_generation, joined_message, pairing_fixture};
use super::reader_resume_tests::{
    Socket, accept, processed, receive, registered, send, wait_prepared, wait_status,
};
use super::*;
use crate::features::remote_control::manager::ControllerRelayChannel;
use ring::signature::KeyPair as _;
use tokio::net::TcpListener;

#[derive(Clone, Copy)]
pub(crate) enum OwnerRaceCase {
    ReceivedDisconnect,
    ReceivedJoined,
    OldChallenge,
}

// The existing single host fixture constructs the actual worker. This helper
// receives domain states and a worker factory, with no host type or host API.
pub(super) struct WorkerFixtureInput {
    pub(super) operation: ControlledOperation,
    pub(super) keys: HandshakeKeyPair,
    pub(super) channel: ControllerRelayChannel,
    pub(super) processed: tokio::sync::mpsc::Sender<Value>,
    pub(super) before_effect: Option<WorkerFrameHook>,
    pub(super) evidence_hooks: Option<ControlledEvidenceHooks>,
}

struct Peer {
    operation: ControlledOperation,
    lease: LeaseContext,
    joined: Value,
    sender: crate::features::remote_control::manager::ControllerRelaySender,
    lane: ControllingLaneState,
    processed: tokio::sync::mpsc::Receiver<Value>,
    worker: Option<tokio::task::JoinHandle<()>>,
    relay: RemoteControlManager,
}

impl Drop for Peer {
    fn drop(&mut self) {
        self.relay
            .stop_controller_relay(self.operation.generation());
        if let Some(worker) = self.worker.take() {
            worker.abort();
        }
    }
}

struct Endpoint(RemoteControlManager);
impl Drop for Endpoint {
    fn drop(&mut self) {
        self.0.shutdown_loopback_test_endpoint();
    }
}

async fn peer(
    manager: &ControlledDeviceManager,
    factory: &impl Fn(WorkerFixtureInput) -> ControlledRelayWorker,
    relay: &RemoteControlManager,
    listener: &TcpListener,
    socket: Option<Socket>,
    hook: Option<WorkerFrameHook>,
    evidence_hooks: Option<ControlledEvidenceHooks>,
    finish_joined: bool,
) -> (Peer, Socket) {
    let operation = manager.start(None, Vec::new()).unwrap();
    let mut fixture = pairing_fixture("relay-owner-interleaving");
    // This fixture exercises the actual poll's role heartbeat, so negotiate
    // that supported capability before re-signing the production transcript.
    let mut capabilities: Vec<String> = serde_json::from_value(
        fixture.reservation["transcript"]["negotiated_capabilities"].clone(),
    )
    .unwrap();
    capabilities.push("role_liveness_v1".into());
    capabilities.sort_unstable();
    capabilities.dedup();
    fixture.reservation["transcript"]["capability_hash"] =
        json!(super::super::protocol::controller_capability_hash(&capabilities).unwrap());
    fixture.reservation["transcript"]["negotiated_capabilities"] = json!(capabilities);
    bind_pairing_generation(&mut fixture, operation.generation());
    // The child owns these overrides. The paused predecessor already verified
    // its signed reservation; rotating the test pin cannot mask a stale joined
    // mutation, which uses that predecessor's retained handshake session.
    unsafe {
        std::env::set_var("PINVOU_REMOTE_RELAY_SIGNING_KEY_ID", &fixture.relay.key_id);
        std::env::set_var(
            "PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64",
            base64::engine::general_purpose::STANDARD
                .encode(fixture.relay.key_pair.public_key().as_ref()),
        );
    }
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
    let sender = channel.sender();
    let (processed_tx, mut processed_rx) = tokio::sync::mpsc::channel(128);
    let worker = factory(WorkerFixtureInput {
        operation: operation.clone(),
        keys: fixture.b_keys,
        channel,
        processed: processed_tx,
        before_effect: hook,
        evidence_hooks,
    });
    let task = tokio::spawn(worker.serve());
    let mut socket = match socket {
        Some(socket) => socket,
        None => {
            let mut socket = accept(listener).await;
            registered(&mut socket).await;
            socket
        }
    };
    let mut a = ControllingHandshake::new(
        fixture.reservation["request_id"].as_str().unwrap(),
        "claim-0001",
        "2345678",
        fixture.a_keys,
        fixture.relay.trust,
    );
    let a_commit = a.accept_reserved(&fixture.reservation).unwrap();
    send(&mut socket, &fixture.reservation).await;
    processed(&mut processed_rx, &fixture.reservation).await;
    assert_eq!(
        manager.status().kind(),
        ControlledStatusKind::HandshakePending
    );
    let b_commit = receive(&mut socket, "finished_receipt_commit").await;
    let a_finished = a.accept_peer_commitment(&b_commit).unwrap();
    send(&mut socket, &a_commit).await;
    let b_finished = receive(&mut socket, "encrypted_handshake_finished").await;
    a.accept_peer_finished(&b_finished).unwrap();
    send(&mut socket, &a_finished).await;
    receive(&mut socket, "finished_receipt_ack").await;
    let joined_lease = a.accept_joined(&joined).unwrap();
    let lease = LeaseContext {
        connection_id: joined_lease.connection_id,
        controller_lease_id: joined_lease.controller_lease_id,
        lease_epoch: joined_lease.lease_epoch.to_string(),
        relay_epoch: joined_lease.relay_epoch,
        remote_control_generation: joined_lease.remote_control_generation,
    };
    let lane = ControllingLaneState::default();
    lane.install(HandshakeRole::A, &lease, a.take_session().unwrap())
        .unwrap();
    if finish_joined {
        send(&mut socket, &joined).await;
        processed(&mut processed_rx, &joined).await;
        assert_eq!(manager.status().kind(), ControlledStatusKind::Connected);
    }
    (
        Peer {
            operation,
            lease,
            joined,
            sender,
            lane,
            processed: processed_rx,
            worker: Some(task),
            relay: relay.clone(),
        },
        socket,
    )
}

fn assert_successor_crypto(
    manager: &ControlledDeviceManager,
    business: &ControlledLaneState,
    successor: &Peer,
) {
    let (_, capabilities) = manager.live_runtime_context().unwrap();
    assert_successor_crypto_for(business, successor, &capabilities);
}

fn assert_successor_crypto_for(
    business: &ControlledLaneState,
    successor: &Peer,
    capabilities: &[String],
) {
    let lease = &successor.lease;
    let payload = json!({"v":1,"type":"task_status",
        "connection_id":lease.connection_id,"controller_lease_id":lease.controller_lease_id,
        "lease_epoch":lease.lease_epoch,"relay_epoch":lease.relay_epoch,
        "remote_control_generation":lease.remote_control_generation,
        "request_id":"request-owner-proof","task_id":"task-owner-proof",
        "session_id":"session-owner-proof","task_status":"admitted"});
    // Local crypto identity probe only: no task is dispatched or asserted to
    // exist. A real G1 writer and consumer must still share the G1 session.
    let frame = business
        .seal("task_status", &payload, capabilities)
        .unwrap();
    let (kind, opened) = successor.lane.open(&frame, capabilities).unwrap();
    assert_eq!(kind, "task_status");
    assert_eq!(opened, payload);
}

pub(super) async fn ownership_round_trip(
    manager: &ControlledDeviceManager,
    business: &ControlledLaneState,
    outbound: &ControlledOutboundState,
    relay: &RemoteControlManager,
    listener: &TcpListener,
    factory: impl Fn(WorkerFixtureInput) -> ControlledRelayWorker,
    case: OwnerRaceCase,
) {
    let _endpoint = Endpoint(relay.clone());
    if matches!(case, OwnerRaceCase::OldChallenge) {
        old_tick_round_trip(manager, business, relay, listener, &factory).await;
        return;
    }
    let (entered_tx, entered_rx) = tokio::sync::oneshot::channel();
    let (release_tx, release_rx) = std::sync::mpsc::channel();
    let release_rx = Mutex::new(release_rx);
    let predicate: fn(&Value) -> bool = match case {
        OwnerRaceCase::ReceivedDisconnect => {
            |message| message["type"] == "desktop_relay_connection" && message["connected"] == false
        }
        OwnerRaceCase::ReceivedJoined => |message| message["type"] == "controller_joined",
        OwnerRaceCase::OldChallenge => unreachable!("poll case handled before recv cases"),
    };
    let hook = WorkerFrameHook {
        predicate,
        action: Box::new(move |frame| {
            entered_tx.send(frame).unwrap();
            release_rx
                .lock()
                .recv_timeout(Duration::from_secs(15))
                .expect("owner interleaving release timed out");
        }),
    };
    let finish_initial = matches!(case, OwnerRaceCase::ReceivedDisconnect);
    let (mut predecessor, mut socket) = peer(
        manager,
        &factory,
        relay,
        listener,
        None,
        Some(hook),
        None,
        finish_initial,
    )
    .await;
    let inherited = match case {
        OwnerRaceCase::ReceivedDisconnect => {
            socket.close(None).await.unwrap();
            drop(socket);
            None
        }
        OwnerRaceCase::ReceivedJoined => {
            send(&mut socket, &predecessor.joined).await;
            Some(socket)
        }
        OwnerRaceCase::OldChallenge => unreachable!("poll case handled before recv cases"),
    };
    let actual_old_frame = tokio::time::timeout(Duration::from_secs(5), entered_rx)
        .await
        .unwrap()
        .unwrap();
    // The hook follows the real channel.recv return and releases no R or C
    // lock into this driver. Replace by real lifecycle/relay APIs and finish a
    // second genuine signed handshake before the old branch may mutate.
    manager.stop();
    relay.stop_controller_relay(predecessor.operation.generation());
    let (successor, mut socket) = peer(
        manager, &factory, relay, listener, inherited, None, None, true,
    )
    .await;
    let status = serde_json::to_value(manager.status()).unwrap();
    let deadline = manager.watchdog_deadline_for_test().unwrap();
    release_tx.send(()).unwrap();
    processed(&mut predecessor.processed, &actual_old_frame).await;
    tokio::time::timeout(Duration::from_secs(3), predecessor.worker.take().unwrap())
        .await
        .unwrap()
        .unwrap();
    let after = serde_json::to_value(manager.status()).unwrap();
    for field in ["status", "statusIntent", "generation", "errorCode"] {
        assert_eq!(
            after.get(field),
            status.get(field),
            "old worker changed the fully authenticated G1 {field}"
        );
    }
    assert_eq!(
        manager.watchdog_deadline_for_test(),
        Some(deadline),
        "old D froze or extended G1 watchdog"
    );
    assert!(!business.is_suspended());
    assert!(
        outbound.sender().unwrap().same_binding(&successor.sender),
        "old joined or cleanup replaced G1 outbound binding"
    );
    assert_successor_crypto(manager, business, &successor);
    // Rejecting G0 must not break G1's next genuine outage/resume admission.
    socket.close(None).await.unwrap();
    drop(socket);
    wait_prepared(manager).await;
    assert_eq!(manager.watchdog_deadline_for_test(), Some(deadline));
    assert!(business.is_suspended());
    let mut replacement = accept(&listener).await;
    registered(&mut replacement).await;
    let prepare = receive(&mut replacement, "controller_resume_prepare").await;
    assert_eq!(
        prepare["remote_control_generation"],
        successor.operation.generation()
    );
    wait_status(
        manager,
        ControlledStatusKind::Reconnecting,
        "successor_after_old_worker",
    )
    .await;
    manager.stop();
}

async fn old_tick_round_trip(
    manager: &ControlledDeviceManager,
    business: &ControlledLaneState,
    relay: &RemoteControlManager,
    listener: &TcpListener,
    factory: &impl Fn(WorkerFixtureInput) -> ControlledRelayWorker,
) {
    let (entered_tx, entered_rx) = tokio::sync::oneshot::channel();
    let (release_pump_tx, release_pump_rx) = std::sync::mpsc::channel();
    let release_pump_rx = Mutex::new(release_pump_rx);
    let (registered_tx, registered_rx) = tokio::sync::oneshot::channel();
    let (release_send_tx, release_send_rx) = std::sync::mpsc::channel();
    let release_send_rx = Mutex::new(release_send_rx);
    let evidence_now = std::sync::Arc::new(AtomicU64::new(0));
    let hooks = ControlledEvidenceHooks {
        now_b: evidence_now.clone(),
        before_pump: Mutex::new(Some(Box::new(move || {
            entered_tx.send(()).unwrap();
            release_pump_rx
                .lock()
                .recv_timeout(Duration::from_secs(15))
                .unwrap();
        }))),
        after_registration: Mutex::new(Some(Box::new(move |result| {
            registered_tx.send(result).unwrap();
            release_send_rx
                .lock()
                .recv_timeout(Duration::from_secs(15))
                .unwrap();
        }))),
    };
    let (mut predecessor, socket) = peer(
        manager,
        factory,
        relay,
        listener,
        None,
        None,
        Some(hooks),
        true,
    )
    .await;
    let entered = tokio::time::timeout(Duration::from_secs(5), entered_rx)
        .await
        .unwrap();
    assert!(
        entered.is_ok(),
        "G0 failed before its successful heartbeat/poll-tail barrier: {:?}",
        manager.status().kind()
    );
    // The actual G0 heartbeat completed under its original owner before the
    // poll-tail observer. No C/R/S lock is held while this driver replaces it.
    manager.stop();
    relay.stop_controller_relay(predecessor.operation.generation());
    let (successor, mut socket) = peer(
        manager,
        factory,
        relay,
        listener,
        Some(socket),
        None,
        None,
        true,
    )
    .await;
    let deadline = manager.watchdog_deadline_for_test().unwrap();
    let cycle_start = deadline.checked_sub(30_000).unwrap();
    // Advance domain inputs, not wall time or the global product clock. G1 has
    // no pending challenge before its first 2s worker poll. The existing
    // watchdog policy's 15s boundary is observed at exact supplied instants.
    evidence_now.store(cycle_start + 14_500, Ordering::Release);
    release_pump_tx.send(()).unwrap();
    let registration = tokio::time::timeout(Duration::from_secs(5), registered_rx)
        .await
        .unwrap()
        .unwrap();
    let before_cancel = manager
        .poll_watchdog_for(&successor.operation, cycle_start + 15_000)
        .unwrap();
    let mut admitted_before_cancel = 0;
    let business_admission = manager.with_current_business_lease(&successor.lease, || {
        admitted_before_cancel += 1;
    });
    // Always release G0 and observe cleanup/next tick before evaluating the
    // disputed snapshot. A negative control must not leave a blocked helper.
    release_send_tx.send(()).unwrap();
    tokio::time::timeout(Duration::from_secs(5), predecessor.worker.take().unwrap())
        .await
        .unwrap()
        .unwrap();
    let after_cancel = manager
        .poll_watchdog_for(&successor.operation, cycle_start + 16_000)
        .unwrap();
    assert_eq!(after_cancel.kind(), ControlledStatusKind::Reconnecting);
    assert_eq!(manager.watchdog_deadline_for_test(), Some(deadline));
    assert_eq!(
        before_cancel.kind(),
        ControlledStatusKind::Reconnecting,
        "old poll challenge masked the successor's Suspect boundary"
    );
    assert!(business_admission.is_err());
    assert_eq!(admitted_before_cancel, 0);
    assert_eq!(registration, Err("desktop_remote_stale_result"));
    let (_, capabilities) = manager.liveness_runtime_context().unwrap();
    assert_successor_crypto_for(business, &successor, &capabilities);
    // Current-owner challenges remain usable in liveness-suspect, including
    // the genuine socket send and exact-owner cancellation.
    let owner = manager
        .transport_owner_for_test(&successor.operation)
        .unwrap();
    let (lease, challenge_id, challenge_b64) = manager
        .begin_owned_liveness_challenge(&owner, cycle_start + 16_000)
        .unwrap();
    assert_eq!(lease, successor.lease);
    let challenge = super::super::liveness::build_liveness_challenge(
        &lease,
        "request-current-challenge",
        &challenge_id,
        &challenge_b64,
    )
    .unwrap();
    successor.sender.try_send(challenge).unwrap();
    let sent = receive(&mut socket, "controller_liveness_challenge_a").await;
    assert_eq!(sent["challenge_id"], challenge_id);
    manager
        .cancel_owned_liveness_challenge(&owner, &challenge_id)
        .unwrap();
    assert_eq!(manager.watchdog_deadline_for_test(), Some(deadline));
    // The next current transport D still starts bounded recovery from that
    // same cryptographic deadline, even though business was already suspect.
    socket.close(None).await.unwrap();
    drop(socket);
    wait_prepared(manager).await;
    assert_eq!(manager.watchdog_deadline_for_test(), Some(deadline));
    assert!(business.is_suspended());
    let mut replacement = accept(listener).await;
    registered(&mut replacement).await;
    let prepare = receive(&mut replacement, "controller_resume_prepare").await;
    assert_eq!(
        prepare["remote_control_generation"],
        successor.operation.generation()
    );
    manager.stop();
}
