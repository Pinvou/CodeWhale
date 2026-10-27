//! Port/driver/keeper contracts. Unit budgets live in controlling_inbound.

use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::oneshot;
use tokio_tungstenite::{WebSocketStream, accept_hdr_async, tungstenite::Message};

use super::super::controlled_handshake::ControlledHandshake;
use super::super::controlling_inbound::{InboundSender, inbound_channel};
use super::super::encrypted_lane::EncryptedLane;
use super::super::protocol::{build_pairing_transcript_preimage, controller_capability_hash};
use super::super::relay_transport::{
    ControllerTransportReceiver, FramePriority, dispatch_test_transport_frame_to_sink,
    transport_channels,
};
use super::super::test_support::{
    ControllerReaderProtocol, PairingFixture, joined_message, pairing_fixture,
};
use super::*;

fn fixture() -> PairingFixture {
    let mut fixture = pairing_fixture("relay-inbound-tests");
    let mut capabilities: Vec<String> = super::super::controlling_handshake::claim_capabilities()
        .iter()
        .map(|value| (*value).to_owned())
        .collect();
    capabilities.sort();
    fixture.reservation["transcript"]["negotiated_capabilities"] = json!(capabilities);
    fixture.reservation["transcript"]["capability_hash"] =
        json!(controller_capability_hash(&capabilities).unwrap());
    let preimage = build_pairing_transcript_preimage(&fixture.reservation["transcript"]).unwrap();
    fixture.reservation["transcript_hash"] = json!(super::super::test_support::hex_lower(
        &Sha256::digest(&preimage)
    ));
    fixture.reservation["relay_signature_b64"] = json!(
        base64::engine::general_purpose::STANDARD
            .encode(fixture.relay.key_pair.sign(&preimage).as_ref())
    );
    fixture.joined = joined_message(&fixture.reservation, fixture.resume_token);
    fixture
}

fn limits() -> InboundLimits {
    InboundLimits {
        control_items: 8,
        business_items: 2,
        total_bytes: 128 * 1024,
        control_reserved_bytes: 64 * 1024,
    }
}

fn port(
    sender: ControllerTransportSender,
    limits: InboundLimits,
) -> (InboundSender, ControllerTransportPort) {
    let (ingress, receiver) = inbound_channel(limits);
    ingress.bind_transport(sender.clone());
    (ingress, ControllerTransportPort::new(sender, receiver))
}

fn raw_control(json: Value) -> ControllerTransportEvent {
    ControllerTransportEvent::Frame {
        priority: FramePriority::Control,
        remote_control_generation: "generation-0001".into(),
        lease_epoch: Some("1".into()),
        json: json.to_string(),
    }
}

struct Connected {
    manager: ControllingDeviceManager,
    operation: ControllerOperation,
    sessions: ControllingSessionManager,
    business: ControllingLaneState,
    sender: ControllerTransportSender,
    _outbound: ControllerTransportReceiver,
    ingress: InboundSender,
    port: ControllerTransportPort,
    b: EncryptedLane,
    lease: LeaseContext,
    capabilities: Vec<String>,
    trust: AuthenticatedRelayTrustConfig,
}

fn install(
    manager: &ControllingDeviceManager,
    business: &ControllingLaneState,
) -> (
    ControllerOperation,
    ControllerTransportSender,
    ControllerTransportReceiver,
    EncryptedLane,
    AuthenticatedRelayTrustConfig,
) {
    let fixture = fixture();
    let PairingFixture {
        relay,
        reservation,
        joined,
        a_keys,
        b_keys,
        ..
    } = fixture;
    let mut a = ControllingHandshake::new(
        reservation["request_id"].as_str().unwrap(),
        "claim-0001",
        "2345678",
        a_keys,
        relay.trust.clone(),
    );
    a.claim().unwrap();
    let mut b = ControlledHandshake::new(&reservation, b_keys, &relay.trust).unwrap();
    let a_commit = a.accept_reserved(&reservation).unwrap();
    let b_commit = b.commit().unwrap();
    b.accept_peer_commitment(&a_commit).unwrap();
    let a_finished = a.accept_peer_commitment(&b_commit).unwrap();
    let b_finished = b.finished().unwrap();
    a.accept_peer_finished(&b_finished).unwrap();
    b.accept_peer_finished(&a_finished).unwrap();
    let lease = a.accept_joined(&joined).unwrap();
    b.accept_joined(&joined).unwrap();
    let outcome = ControllingHandshakeOutcome {
        lease,
        peer_endpoint_id: a.peer_endpoint_id().to_owned(),
        reservation_id: a.reservation_id().to_owned(),
        negotiated_capabilities: a.negotiated_capabilities().to_vec(),
        transcript_hash: a.transcript_hash().unwrap(),
        session: a.take_session().unwrap(),
    };
    let operation = manager.connect("2345678").unwrap();
    manager.complete_reserved(operation.clone()).unwrap();
    let (sender, outbound) = transport_channels(
        ControllerRole::ControllerA,
        &outcome.lease.remote_control_generation,
        64,
        256,
        8 * 1024 * 1024,
        128 * 1024,
    );
    manager
        .replace_owned_transport(&operation, sender.clone(), business)
        .unwrap();
    complete_connected(manager, operation.clone(), &sender, business, outcome).unwrap();
    let b = EncryptedLane::new(
        HandshakeRole::B,
        &manager.live_lease().unwrap(),
        b.take_session().unwrap(),
    )
    .unwrap();
    (operation, sender, outbound, b, relay.trust)
}

fn connected(limits: InboundLimits) -> Connected {
    let manager = ControllingDeviceManager::default();
    let business = ControllingLaneState::default();
    let (operation, sender, outbound, b, trust) = install(&manager, &business);
    let (lease, capabilities) = manager.live_runtime_context().unwrap();
    let (ingress, port) = port(sender.clone(), limits);
    Connected {
        manager,
        operation,
        sessions: ControllingSessionManager::default(),
        business,
        sender,
        _outbound: outbound,
        ingress,
        port,
        b,
        lease,
        capabilities,
        trust,
    }
}

fn journal_frame(
    b: &mut EncryptedLane,
    lease: &LeaseContext,
    capabilities: &[String],
    text: &str,
    sequence: u64,
) -> Value {
    let event = JournalEvent {
        event_id: format!("event-inbound-{sequence}"),
        epoch: "journal-inbound".into(),
        sequence,
        kind: "assistant_delta".into(),
        session_id: "session-inbound".into(),
        task_id: None,
        terminal_status: None,
        payload: super::super::journal::sanitize_event_payload(&json!({"text":text})),
    };
    journal_event_frame(b, lease, capabilities, &event)
}

fn journal_event_frame(
    b: &mut EncryptedLane,
    lease: &LeaseContext,
    capabilities: &[String],
    event: &JournalEvent,
) -> Value {
    event.validate_exportable().unwrap();
    let raw = serde_json::to_vec(event).unwrap();
    b.seal("journal_event", &json!({
        "v":1,"type":"journal_event","connection_id":lease.connection_id,
        "controller_lease_id":lease.controller_lease_id,"lease_epoch":lease.lease_epoch,
        "relay_epoch":lease.relay_epoch,"remote_control_generation":lease.remote_control_generation,
        "session_id":event.session_id,"journal_epoch":event.epoch,"event_sequence":event.sequence.to_string(),
        "event_kind":event.wire_kind().unwrap(),"event_b64":base64::engine::general_purpose::STANDARD.encode(&raw),
        "event_bytes":raw.len().to_string(),
    }), capabilities).unwrap()
}

fn admit(fixture: &Connected, frame: &Value) -> bool {
    let ingress = fixture.ingress.clone();
    dispatch_test_transport_frame_to_sink(&fixture.sender, frame, &move |event| {
        ingress.enqueue(event)
    })
}

fn disconnect(fixture: &Connected) {
    let ingress = fixture.ingress.clone();
    super::super::relay_transport::disconnect_test_transport_to_sink(
        &fixture.sender,
        &move |event| ingress.enqueue(event),
    );
}

fn resume_controls(lease: &LeaseContext) -> Vec<Value> {
    let base = |kind: &str, request: &str| {
        json!({
            "v":1,"type":kind,"request_id":request,"activation_id":"activation-inbound",
            "connection_id":lease.connection_id,"controller_lease_id":lease.controller_lease_id,
            "lease_epoch":lease.lease_epoch,"relay_epoch":lease.relay_epoch,
            "remote_control_generation":lease.remote_control_generation,
        })
    };
    let mut provisional = base(
        "controller_resume_provisional",
        "request-inbound-provisional",
    );
    provisional["original_deadline_ms"] = json!("30000");
    let mut activation = base(
        "controller_resume_activation_pending",
        "request-inbound-activation",
    );
    activation["original_deadline_ms"] = json!("30000");
    let mut liveness = base(
        "controller_post_activation_liveness_confirmed",
        "request-inbound-live",
    );
    liveness["liveness_epoch_b"] = json!("live-b-2");
    liveness["heartbeat_sequence_b"] = json!("1");
    vec![
        provisional,
        activation,
        liveness,
        super::super::test_support::resume_final_frame(
            lease,
            "activation-inbound",
            "request-inbound-final",
        ),
    ]
}

async fn keeper(fixture: &mut Connected) -> Vec<Value> {
    let mut status = Vec::new();
    let mut on_status =
        |value: &ControllingStatus| status.push(serde_json::to_value(value).unwrap());
    let mut sinks = ControllingLaneSinks {
        on_status: &mut on_status,
        on_session: &mut |_| {},
        on_mapping: &mut |_, _, _| Ok(()),
        on_task: &mut |_, _| {},
        on_journal: &mut |_| Ok(()),
        on_snapshot: &mut |_| Ok(()),
        on_liveness: &mut |_| true,
    };
    tokio::time::timeout(
        Duration::from_secs(5),
        serve_controlling_lane_with_evidence_context(
            &mut fixture.port,
            &fixture.manager,
            &fixture.sessions,
            &fixture.business,
            &mut sinks,
            Duration::from_secs(3),
            PeerEvidenceContext {
                trust: Some(&fixture.trust),
                monotonic: &|| 0,
                operation: fixture.operation.clone(),
            },
        ),
    )
    .await
    .unwrap();
    status
}

#[tokio::test]
async fn actual_port_fatal_and_closed_both_commit_current_owner_error() {
    let _env_lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let directory = tempfile::tempdir().unwrap();
    let _restore =
        crate::platform::test_support::EnvRestore::capture(&["PINVOU3_HOME", "CODEWHALE_HOME"]);
    // SAFETY: the crate-wide lock is held through environment restoration.
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("CODEWHALE_HOME", directory.path());
    }
    for closed in [false, true] {
        let mut fixture = connected(limits());
        fixture.ingress.enqueue(raw_control(
            json!({"v":1,"type":"error","code":"controller_busy"}),
        ));
        let prior = tokio::time::timeout(Duration::from_secs(5), fixture.port.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(prior["code"], "controller_busy");
        assert_eq!(public_failure_code(&fixture.port, false), "controller_busy");
        if closed {
            // This is the actual last producer, not a fake None-returning lane.
            let (replacement, _unused) = inbound_channel(limits());
            drop(std::mem::replace(&mut fixture.ingress, replacement));
        } else {
            for sequence in 1..=3 {
                let frame = journal_frame(
                    &mut fixture.b,
                    &fixture.lease,
                    &fixture.capabilities,
                    "fake-secret",
                    sequence,
                );
                assert!(admit(&fixture, &frame));
            }
        }
        assert_eq!(
            public_failure_code(&fixture.port, false),
            PEER_UNAVAILABLE_ERROR
        );
        let status = keeper(&mut fixture).await;
        assert_eq!(status.len(), 1);
        assert_eq!(status[0]["status"], "error");
        assert_eq!(status[0]["errorCode"], PEER_UNAVAILABLE_ERROR);
        assert!(!status[0].to_string().contains("fake-secret"));
        assert!(fixture.sender.is_stopped());
        assert!(fixture.sender.authenticated_lease().is_none());
        assert!(fixture.manager.live_lease().is_err());
        assert!(!fixture.business.is_installed());
        let written =
            std::fs::read_to_string(directory.path().join("logs/remote-control.log")).unwrap();
        let reason = if closed {
            "inbound_closed"
        } else {
            "inbound_item_capacity"
        };
        assert!(written.contains(&format!("stage=inbound_failed detail={reason}")));
        assert!(!written.contains("fake-secret"));
        assert!(!written.contains("claim-0001"));
        assert!(fixture.manager.connect("2345678").is_ok());
    }
}

#[tokio::test]
async fn port_keeps_in_flight_budget_through_work_and_select_cancellation() {
    let mut fixture = connected(limits());
    let frame = journal_frame(
        &mut fixture.b,
        &fixture.lease,
        &fixture.capabilities,
        "charged",
        1,
    );
    let bytes = frame.to_string().len();
    assert!(admit(&fixture, &frame));
    let event = fixture.port.next_lane_event().await.unwrap();
    assert!(matches!(event, ControllerLaneEvent::Frame(_)));
    assert_eq!(
        fixture.port.inbound.remaining_bytes(),
        limits().total_bytes - bytes
    );
    drop(event);
    assert_eq!(
        fixture.port.inbound.remaining_bytes(),
        limits().total_bytes - bytes
    );
    let mut waiting = Box::pin(fixture.port.next_lane_event());
    assert!(futures_util::poll!(&mut waiting).is_pending());
    drop(waiting);
    assert_eq!(fixture.port.inbound.remaining_bytes(), limits().total_bytes);
    let frame = journal_frame(
        &mut fixture.b,
        &fixture.lease,
        &fixture.capabilities,
        "after-cancel",
        2,
    );
    assert!(admit(&fixture, &frame));
    assert!(matches!(
        fixture.port.next_lane_event().await,
        Some(ControllerLaneEvent::Frame(_))
    ));
}

#[tokio::test]
async fn stale_fatal_and_receiver_drop_cannot_clear_replacement_with_same_lease() {
    let mut fixture = connected(limits());
    for sequence in 1..=3 {
        let frame = journal_frame(
            &mut fixture.b,
            &fixture.lease,
            &fixture.capabilities,
            "old",
            sequence,
        );
        assert!(admit(&fixture, &frame));
    }
    assert!(fixture.port.ingress_failure().is_some());
    fixture.manager.disconnect();
    fixture.business.clear();
    let (operation, sender, _outbound, mut b, _trust) =
        install(&fixture.manager, &fixture.business);
    let (new_ingress, mut new_port) = port(sender.clone(), limits());
    assert_eq!(fixture.manager.live_lease().unwrap(), fixture.lease);
    assert!(keeper(&mut fixture).await.is_empty());
    // Directly exercise the clear's guard too, rather than relying only on
    // the keeper's earlier owner check to make this assertion vacuous.
    fail_owned_lane(
        &fixture.manager,
        &fixture.operation,
        &fixture.business,
        PEER_UNAVAILABLE_ERROR,
        &mut |_| panic!("stale status"),
    );
    drop(fixture.port);
    drop(fixture.ingress);
    assert!(!sender.is_stopped());
    assert!(fixture.manager.with_lane_owner(&operation, || ()).is_ok());
    let frame = journal_frame(
        &mut b,
        &fixture.lease,
        &fixture.capabilities,
        "replacement",
        1,
    );
    let dispatch_ingress = new_ingress.clone();
    assert!(dispatch_test_transport_frame_to_sink(
        &sender,
        &frame,
        &move |event| dispatch_ingress.enqueue(event)
    ));
    assert!(matches!(
        new_port.next_lane_event().await,
        Some(ControllerLaneEvent::Frame(_))
    ));
    assert!(new_port.inbound.remaining_bytes() < limits().total_bytes);
    assert!(new_port.ingress_failure().is_none());
    assert_eq!(
        fixture
            .business
            .open(&frame, &fixture.capabilities)
            .unwrap()
            .0,
        "journal_event"
    );
    assert_eq!(
        fixture.manager.status().kind(),
        ControllingStatusKind::Connected
    );
}

#[tokio::test]
async fn retained_ordered_tail_reaches_authenticated_projection_after_resume_with_b_silent() {
    let mut fixture = connected(limits());
    let terminal = JournalEvent {
        event_id: "event-last-terminal".into(),
        epoch: "journal-inbound".into(),
        sequence: 2,
        kind: "task_status".into(),
        session_id: "session-inbound".into(),
        task_id: Some("task-inbound".into()),
        terminal_status: Some("succeeded".into()),
        payload: json!({"status":"succeeded"}),
    };
    let earlier = JournalEvent {
        event_id: "event-earlier-tail".into(),
        epoch: "journal-inbound".into(),
        sequence: 1,
        kind: "assistant_delta".into(),
        session_id: "session-inbound".into(),
        task_id: None,
        terminal_status: None,
        payload: super::super::journal::sanitize_event_payload(&json!({"text":"before terminal"})),
    };
    let earlier_frame = journal_event_frame(
        &mut fixture.b,
        &fixture.lease,
        &fixture.capabilities,
        &earlier,
    );
    let frame = journal_event_frame(
        &mut fixture.b,
        &fixture.lease,
        &fixture.capabilities,
        &terminal,
    );
    assert!(admit(&fixture, &earlier_frame));
    assert!(admit(&fixture, &frame));
    disconnect(&fixture);
    assert!(fixture.port.inbound.disconnect_pending());
    // The frozen Relay notices are schema/context checked by the real reader,
    // including its final socket-epoch authority. Only Relay networking is
    // modeled; this is not a claim of a live cross-machine resume service.
    for control in resume_controls(&fixture.lease) {
        assert!(admit(&fixture, &control));
    }
    let mut seen = Vec::new();
    let (done, received) = oneshot::channel();
    let mut done = Some(done);
    // The serving keeper fences every dispatch with the lane's original
    // registration, so the dispatched operation carries that context.
    let expected_operation = fixture
        .operation
        .clone()
        .for_lane(fixture.sender.disconnect_context());
    let mut on_journal = |authenticated: &AuthenticatedJournalFrame<'_>| {
        assert_eq!(authenticated.operation(), &expected_operation);
        assert_eq!(authenticated.lease(), &fixture.lease);
        seen.push(authenticated.event().clone());
        if seen.len() == 2
            && let Some(done) = done.take()
        {
            let _ = done.send(());
        }
        Ok(())
    };
    let mut sinks = ControllingLaneSinks {
        on_status: &mut |_| {},
        on_session: &mut |_| {},
        on_mapping: &mut |_, _, _| Ok(()),
        on_task: &mut |_, _| {},
        on_journal: &mut on_journal,
        on_snapshot: &mut |_| Ok(()),
        on_liveness: &mut |_| true,
    };
    let mut serving = Box::pin(serve_controlling_lane_with_evidence_context(
        &mut fixture.port,
        &fixture.manager,
        &fixture.sessions,
        &fixture.business,
        &mut sinks,
        Duration::from_secs(3),
        PeerEvidenceContext {
            trust: Some(&fixture.trust),
            monotonic: &|| 0,
            operation: fixture.operation.clone(),
        },
    ));
    tokio::time::timeout(Duration::from_secs(5), async {
        tokio::select! {
            result=received=>result.unwrap(),
            _=&mut serving=>panic!("keeper terminated before accepted terminal projection"),
        }
    })
    .await
    .unwrap();
    drop(serving);
    drop(sinks);
    drop(on_journal);
    assert_eq!(seen, vec![earlier, terminal]);
    assert_eq!(
        fixture.manager.status().kind(),
        ControllingStatusKind::Connected
    );
    assert_eq!(fixture.manager.live_lease().unwrap(), fixture.lease);
    assert!(!fixture.business.is_suspended());
    assert!(fixture.sender.authenticated_lease().is_some());
    // Replaying the exact B ciphertext is rejected by the unchanged cipher;
    // the projection was reached by its first authenticated consumption.
    assert!(
        fixture
            .business
            .open(&frame, &fixture.capabilities)
            .is_err()
    );
}

#[tokio::test(start_paused = true)]
async fn repeated_disconnect_and_waiting_business_keep_the_first_absolute_cutoff() {
    let mut fixture = connected(limits());
    let frame = journal_frame(
        &mut fixture.b,
        &fixture.lease,
        &fixture.capabilities,
        "waiting",
        1,
    );
    assert!(admit(&fixture, &frame));
    disconnect(&fixture);
    let ingress = fixture.ingress.clone();
    let transport = fixture.sender.clone();
    let origin = tokio::time::Instant::now();
    let clock = || u64::try_from(origin.elapsed().as_millis()).unwrap();
    let mut statuses = Vec::new();
    let mut on_status = |value: &ControllingStatus| statuses.push(value.kind());
    let mut sinks = ControllingLaneSinks {
        on_status: &mut on_status,
        on_session: &mut |_| {},
        on_mapping: &mut |_, _, _| Ok(()),
        on_task: &mut |_, _| {},
        on_journal: &mut |_| panic!("business must remain paused before authenticated resume"),
        on_snapshot: &mut |_| Ok(()),
        on_liveness: &mut |_| true,
    };
    let mut serving = Box::pin(serve_controlling_lane_with_evidence_context(
        &mut fixture.port,
        &fixture.manager,
        &fixture.sessions,
        &fixture.business,
        &mut sinks,
        Duration::from_secs(3),
        PeerEvidenceContext {
            trust: Some(&fixture.trust),
            monotonic: &clock,
            operation: fixture.operation.clone(),
        },
    ));
    assert!(futures_util::poll!(&mut serving).is_pending());
    assert_eq!(
        fixture.manager.status().kind(),
        ControllingStatusKind::Reconnecting
    );
    for _ in 0..4 {
        tokio::time::advance(Duration::from_secs(5)).await;
        let sink = ingress.clone();
        super::super::relay_transport::disconnect_test_transport_to_sink(
            &transport,
            &move |event| sink.enqueue(event),
        );
        assert!(futures_util::poll!(&mut serving).is_pending());
    }
    tokio::time::advance(Duration::from_millis(9_999)).await;
    assert!(futures_util::poll!(&mut serving).is_pending());
    assert_eq!(
        fixture.manager.status().kind(),
        ControllingStatusKind::Reconnecting
    );
    tokio::time::advance(Duration::from_millis(1)).await;
    assert!(futures_util::poll!(&mut serving).is_ready());
    drop(serving);
    drop(sinks);
    drop(on_status);
    assert_eq!(clock(), 30_000);
    assert_eq!(
        statuses,
        vec![
            ControllingStatusKind::Reconnecting,
            ControllingStatusKind::Error
        ]
    );
    assert!(fixture.sender.is_stopped());
    assert!(!fixture.business.is_installed());
}

#[tokio::test(start_paused = true)]
async fn local_full_and_closed_during_reconnect_retire_owner_without_another_resume() {
    let _env_lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let directory = tempfile::tempdir().unwrap();
    let _restore =
        crate::platform::test_support::EnvRestore::capture(&["PINVOU3_HOME", "CODEWHALE_HOME"]);
    // SAFETY: the crate-wide lock is held through environment restoration.
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("CODEWHALE_HOME", directory.path());
    }
    for closed in [false, true] {
        let mut fixture = connected(limits());
        disconnect(&fixture);
        let producer = fixture.ingress.clone();
        let (replacement, _unused) = inbound_channel(limits());
        drop(std::mem::replace(&mut fixture.ingress, replacement));
        let control = resume_controls(&fixture.lease).remove(0);
        let sender = fixture.sender.clone();
        let mut statuses = Vec::new();
        let mut on_status = |value: &ControllingStatus| statuses.push(value.kind());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut |_| {},
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut |_, _| {},
            on_journal: &mut |_| panic!("business before authenticated resume"),
            on_snapshot: &mut |_| Ok(()),
            on_liveness: &mut |_| true,
        };
        let mut serving = Box::pin(serve_controlling_lane_with_evidence_context(
            &mut fixture.port,
            &fixture.manager,
            &fixture.sessions,
            &fixture.business,
            &mut sinks,
            Duration::from_secs(3),
            PeerEvidenceContext {
                trust: Some(&fixture.trust),
                monotonic: &|| 0,
                operation: fixture.operation.clone(),
            },
        ));
        assert!(futures_util::poll!(&mut serving).is_pending());
        assert_eq!(
            fixture.manager.status().kind(),
            ControllingStatusKind::Reconnecting
        );
        if !closed {
            for _ in 0..9 {
                let sink = producer.clone();
                assert!(dispatch_test_transport_frame_to_sink(
                    &sender,
                    &control,
                    &move |event| sink.enqueue(event)
                ));
            }
        }
        drop(producer);
        tokio::time::timeout(Duration::from_secs(5), &mut serving)
            .await
            .unwrap();
        drop(serving);
        drop(sinks);
        drop(on_status);
        assert_eq!(
            statuses,
            vec![
                ControllingStatusKind::Reconnecting,
                ControllingStatusKind::Error
            ]
        );
        assert_eq!(
            fixture.port.ingress_failure(),
            Some(if closed {
                ControllerLaneFailure::Closed
            } else {
                ControllerLaneFailure::ItemCapacity
            })
        );
        assert!(sender.is_stopped());
        assert!(sender.authenticated_lease().is_none());
        assert!(!fixture.business.is_installed());
        assert!(fixture.manager.connect("2345678").is_ok());
    }
    let written =
        std::fs::read_to_string(directory.path().join("logs/remote-control.log")).unwrap();
    assert!(written.contains("stage=inbound_failed detail=inbound_closed"));
    assert!(written.contains("stage=inbound_failed detail=inbound_item_capacity"));
}

#[tokio::test]
async fn ingress_terminal_does_not_wait_for_an_admitted_mutations_lease_guard() {
    let _env_lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let directory = tempfile::tempdir().unwrap();
    let _restore =
        crate::platform::test_support::EnvRestore::capture(&["PINVOU3_HOME", "CODEWHALE_HOME"]);
    // SAFETY: the crate-wide lock is held through environment restoration.
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("CODEWHALE_HOME", directory.path());
    }
    let mut fixture = connected(InboundLimits {
        business_items: 1,
        ..limits()
    });
    let first = journal_frame(
        &mut fixture.b,
        &fixture.lease,
        &fixture.capabilities,
        "first",
        1,
    );
    assert!(admit(&fixture, &first));
    let second = journal_frame(
        &mut fixture.b,
        &fixture.lease,
        &fixture.capabilities,
        "second",
        2,
    );
    let ingress = fixture.ingress.clone();
    let event = ControllerTransportEvent::Frame {
        priority: FramePriority::Business,
        remote_control_generation: fixture.lease.remote_control_generation.clone(),
        lease_epoch: Some(fixture.lease.lease_epoch.clone()),
        json: second.to_string(),
    };
    let (done, received) = std::sync::mpsc::channel();
    let mut job = Some((ingress, event, done));
    let mut worker = None;
    let mut returned_while_held = false;
    assert!(
        fixture
            .sender
            .with_business_authority(&fixture.lease, &mut || {
                let (ingress, event, done) = job.take().unwrap();
                worker = Some(std::thread::spawn(move || {
                    ingress.enqueue(event);
                    done.send(()).unwrap();
                }));
                // If a regression takes the lease writer lock in emit, first release
                // this guard on timeout, then join; the test cannot strand a thread.
                returned_while_held = received.recv_timeout(Duration::from_secs(5)).is_ok();
            })
    );
    worker.unwrap().join().unwrap();
    assert!(returned_while_held);
    assert!(fixture.sender.is_stopped());
    assert!(
        !fixture
            .sender
            .with_business_authority(&fixture.lease, &mut || panic!(
                "new authority after local terminal"
            ))
    );
    assert_eq!(keeper(&mut fixture).await[0]["status"], "error");
    let written =
        std::fs::read_to_string(directory.path().join("logs/remote-control.log")).unwrap();
    assert!(written.contains("stage=inbound_failed detail=inbound_item_capacity"));
}

async fn receive(socket: &mut WebSocketStream<TcpStream>) -> Value {
    loop {
        match socket.next().await.unwrap().unwrap() {
            Message::Text(text) => return serde_json::from_str(&text).unwrap(),
            Message::Ping(bytes) => socket.send(Message::Pong(bytes)).await.unwrap(),
            _ => panic!("unexpected non-text test WebSocket frame"),
        }
    }
}

#[derive(Clone, Copy)]
enum HandoffFault {
    None,
    Full,
    Disconnect,
}

/// Real B handshake produces the replies; only the socket is replaced here.
/// The live Port, inbox, classifier, driver and installation are unchanged.
struct HandshakingPort {
    inner: ControllerTransportPort,
    ingress: InboundSender,
    peer: std::cell::RefCell<ControlledHandshake>,
    reservation: Value,
    joined: Value,
    fault: HandoffFault,
}

impl HandshakingPort {
    fn push(&self, frame: &Value) -> bool {
        let ingress = self.ingress.clone();
        dispatch_test_transport_frame_to_sink(&self.inner.sender, frame, &move |event| {
            ingress.enqueue(event)
        })
    }
}

#[async_trait::async_trait]
impl ControllingRelayPort for HandshakingPort {
    fn send(&self, message: Value) -> Result<(), String> {
        self.inner.send(message.clone())?;
        match message["type"].as_str().unwrap() {
            "controller_pairing_claim" => assert!(self.push(&self.reservation)),
            "finished_receipt_commit" => {
                let mut peer = self.peer.borrow_mut();
                let commit = peer.commit().unwrap();
                peer.accept_peer_commitment(&message).unwrap();
                let finished = peer.finished().unwrap();
                // Relay may echo A's own commit; transport retains its real
                // admission decision, including any upstream echo filtering.
                self.push(&message);
                assert!(self.push(&commit));
                assert!(self.push(&finished));
            }
            "encrypted_handshake_finished" => {
                self.peer
                    .borrow_mut()
                    .accept_peer_finished(&message)
                    .unwrap();
            }
            "finished_receipt_ack" => {
                self.peer.borrow_mut().accept_joined(&self.joined).unwrap();
                assert!(self.push(&self.joined));
            }
            _ => panic!("unexpected handshake send kind"),
        }
        Ok(())
    }

    async fn recv(&mut self) -> Option<Value> {
        let value = self.inner.recv().await?;
        if value["type"] == "controller_joined" {
            match self.fault {
                HandoffFault::None => {}
                HandoffFault::Full => {
                    for _ in 0..8 {
                        self.ingress.enqueue(raw_control(self.joined.clone()));
                    }
                }
                HandoffFault::Disconnect => {
                    self.ingress
                        .enqueue(ControllerTransportEvent::Disconnected {
                            remote_control_generation: self.joined["remote_control_generation"]
                                .as_str()
                                .unwrap()
                                .into(),
                            lease_epoch: Some("1".into()),
                            context: self.inner.sender.disconnect_context(),
                            error: "fake-wire-close-detail".into(),
                        });
                }
            }
        }
        Some(value)
    }

    fn ingress_failure(&self) -> Option<ControllerLaneFailure> {
        self.inner.ingress_failure()
    }

    fn lane_disconnected(&self) -> bool {
        self.inner.lane_disconnected()
    }
}

struct HandshakeCase {
    manager: ControllingDeviceManager,
    operation: ControllerOperation,
    business: ControllingLaneState,
    sender: ControllerTransportSender,
    _outbound: ControllerTransportReceiver,
    port: HandshakingPort,
    handshake: ControllingHandshake,
}

fn handshaking(fault: HandoffFault) -> HandshakeCase {
    let fixture = fixture();
    let PairingFixture {
        relay,
        reservation,
        joined,
        a_keys,
        b_keys,
        ..
    } = fixture;
    let handshake = ControllingHandshake::new(
        reservation["request_id"].as_str().unwrap(),
        "claim-0001",
        "2345678",
        a_keys,
        relay.trust.clone(),
    );
    let peer = ControlledHandshake::new(&reservation, b_keys, &relay.trust).unwrap();
    let manager = ControllingDeviceManager::default();
    let operation = manager.connect("2345678").unwrap();
    let business = ControllingLaneState::default();
    let (sender, outbound) = transport_channels(
        ControllerRole::ControllerA,
        "",
        64,
        256,
        8 * 1024 * 1024,
        128 * 1024,
    );
    manager
        .replace_owned_transport(&operation, sender.clone(), &business)
        .unwrap();
    let (ingress, inner) = port(sender.clone(), limits());
    HandshakeCase {
        manager,
        operation,
        business,
        sender,
        _outbound: outbound,
        handshake,
        port: HandshakingPort {
            inner,
            ingress,
            peer: std::cell::RefCell::new(peer),
            reservation,
            joined,
            fault,
        },
    }
}

#[tokio::test]
async fn actual_handshake_port_reports_full_and_closed_as_local_finite_errors() {
    for closed in [false, true] {
        let mut case = handshaking(HandoffFault::None);
        let producer = case.port.ingress.clone();
        let (replacement, _receiver) = inbound_channel(limits());
        drop(std::mem::replace(&mut case.port.ingress, replacement));
        let reservation = case.port.reservation.clone();
        assert!(!case.sender.is_stopped());
        let mut status = Vec::new();
        let mut on_status = |value: &ControllingStatus| status.push(value.kind());
        let running = run_controlling_connection(
            &case.manager,
            case.operation.clone(),
            &mut case.port.inner,
            &mut case.handshake,
            &case.sender,
            &case.business,
            Duration::from_secs(5),
            &mut on_status,
        );
        let failing = async move {
            if !closed {
                for _ in 0..9 {
                    producer.enqueue(raw_control(reservation.clone()));
                }
            }
            drop(producer);
        };
        // Poll the real driver through claim into its pending recv before
        // terminating ingress; this exercises the recv failure, not just a
        // claim send that had already been stopped before the driver ran.
        let (result, ()) = tokio::join!(biased; running, failing);
        let error = result.unwrap_err();
        assert_eq!(error, PEER_UNAVAILABLE_ERROR);
        assert_eq!(status, vec![ControllingStatusKind::Error]);
        assert!(case.sender.is_stopped());
        assert!(case.sender.authenticated_lease().is_none());
        assert!(!case.business.is_installed());
        assert!(case.manager.connect("2345678").is_ok());
    }
}

#[tokio::test]
async fn joined_already_delivered_cannot_hide_fatal_or_pending_disconnect_at_install() {
    for fault in [HandoffFault::Full, HandoffFault::Disconnect] {
        let mut case = handshaking(fault);
        let mut status = Vec::new();
        let error = run_controlling_connection(
            &case.manager,
            case.operation.clone(),
            &mut case.port,
            &mut case.handshake,
            &case.sender,
            &case.business,
            Duration::from_secs(5),
            &mut |value| status.push(value.kind()),
        )
        .await
        .unwrap_err();
        assert_eq!(error, PEER_UNAVAILABLE_ERROR);
        assert_eq!(
            status,
            vec![
                ControllingStatusKind::SafetyHandshake,
                ControllingStatusKind::Error
            ]
        );
        assert!(case.sender.is_stopped());
        assert!(case.sender.authenticated_lease().is_none());
        assert!(!case.business.is_installed());
        assert!(case.manager.live_lease().is_err());
    }
    let mut case = handshaking(HandoffFault::None);
    let status = run_controlling_connection(
        &case.manager,
        case.operation.clone(),
        &mut case.port,
        &mut case.handshake,
        &case.sender,
        &case.business,
        Duration::from_secs(5),
        &mut |_| {},
    )
    .await
    .unwrap();
    assert_eq!(status.kind(), ControllingStatusKind::Connected);
    assert!(case.business.is_installed());
    assert!(!case.sender.is_stopped());
}

#[tokio::test(start_paused = true)]
async fn actual_port_waiting_for_reservation_keeps_the_original_forty_second_deadline() {
    let mut case = handshaking(HandoffFault::None);
    let producer = case.port.ingress.clone();
    let start = tokio::time::Instant::now();
    let mut statuses = Vec::new();
    let mut on_status = |value: &ControllingStatus| statuses.push(value.kind());
    // Use the real Port directly: the peer intentionally does not reply to
    // the claim. This covers actual waiting, unlike an immediate fake None.
    let mut running = Box::pin(run_controlling_connection(
        &case.manager,
        case.operation.clone(),
        &mut case.port.inner,
        &mut case.handshake,
        &case.sender,
        &case.business,
        HANDSHAKE_DEADLINE,
        &mut on_status,
    ));
    assert!(futures_util::poll!(&mut running).is_pending());
    for _ in 0..7 {
        tokio::time::advance(Duration::from_secs(5)).await;
        producer.enqueue(ControllerTransportEvent::Connected {
            remote_control_generation: String::new(),
            lease_epoch: None,
        });
        producer.enqueue(ControllerTransportEvent::RttSample {
            remote_control_generation: String::new(),
            millis: 1,
        });
        assert!(futures_util::poll!(&mut running).is_pending());
    }
    tokio::time::advance(Duration::from_millis(4_999)).await;
    assert!(futures_util::poll!(&mut running).is_pending());
    tokio::time::advance(Duration::from_millis(1)).await;
    assert_eq!(running.as_mut().await.unwrap_err(), PEER_UNAVAILABLE_ERROR);
    drop(running);
    drop(on_status);
    assert_eq!(start.elapsed(), Duration::from_secs(40));
    assert_eq!(statuses, vec![ControllingStatusKind::Error]);
    assert!(case.sender.is_stopped());
    assert!(!case.business.is_installed());
    assert!(case.manager.connect("2345678").is_ok());
}

#[tokio::test]
async fn install_terminal_after_all_handoff_checks_retains_local_error_and_real_diagnostic() {
    use std::sync::atomic::{AtomicUsize, Ordering};

    let _env_lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let directory = tempfile::tempdir().unwrap();
    let _restore =
        crate::platform::test_support::EnvRestore::capture(&["PINVOU3_HOME", "CODEWHALE_HOME"]);
    // SAFETY: the crate-wide environment lock is held through restoration.
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("CODEWHALE_HOME", directory.path());
    }
    let mut case = handshaking(HandoffFault::None);
    let ingress = case.port.ingress.clone();
    let joined = case.port.joined.clone();
    let executions = Arc::new(AtomicUsize::new(0));
    let count = executions.clone();
    // The hook is on this exact sender, inside the real installer, after the
    // service's entire initial check. The outer manager lock stays held.
    // The hook mutex and lease writer are not held on callback entry; it does
    // not reacquire the manager lock or capture a strong sender/receiver.
    // Reader validation, queue saturation, cancellation and install's Closed
    // result remain real.
    case.sender.before_initial_install_for_test(move |sender| {
        count.fetch_add(1, Ordering::SeqCst);
        for _ in 0..8 {
            let sink = ingress.clone();
            assert!(dispatch_test_transport_frame_to_sink(
                sender,
                &joined,
                &move |event| sink.enqueue(event)
            ));
        }
    });
    let mut statuses = Vec::new();
    let error = run_controlling_connection(
        &case.manager,
        case.operation.clone(),
        &mut case.port,
        &mut case.handshake,
        &case.sender,
        &case.business,
        Duration::from_secs(5),
        &mut |status| statuses.push(status.kind()),
    )
    .await
    .unwrap_err();
    assert_eq!(executions.load(Ordering::SeqCst), 1);
    assert_eq!(error, PEER_UNAVAILABLE_ERROR);
    assert_eq!(
        serde_json::to_value(case.manager.status()).unwrap()["errorCode"],
        PEER_UNAVAILABLE_ERROR
    );
    assert_eq!(
        statuses,
        vec![
            ControllingStatusKind::SafetyHandshake,
            ControllingStatusKind::Error
        ]
    );
    assert_eq!(
        case.port.ingress_failure(),
        Some(ControllerLaneFailure::ItemCapacity)
    );
    assert!(case.sender.is_stopped());
    assert!(case.sender.authenticated_lease().is_none());
    assert!(!case.business.is_installed());
    assert!(case.manager.live_lease().is_err());
    let written =
        std::fs::read_to_string(directory.path().join("logs/remote-control.log")).unwrap();
    assert!(written.contains("stage=connect_failed detail=inbound_item_capacity"));
    assert!(!written.contains("claim-0001"));
    assert!(case.manager.connect("2345678").is_ok());
}

#[test]
fn inbound_diagnostics_survive_the_real_finite_log_sink_without_untrusted_text() {
    let _env_lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let directory = tempfile::tempdir().unwrap();
    let _restore =
        crate::platform::test_support::EnvRestore::capture(&["PINVOU3_HOME", "CODEWHALE_HOME"]);
    // SAFETY: the crate-wide environment lock is held through restoration.
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("CODEWHALE_HOME", directory.path());
    }
    for reason in [
        ControllerLaneFailure::ItemCapacity,
        ControllerLaneFailure::ByteBudget,
        ControllerLaneFailure::Closed,
    ] {
        log_controller_event("inbound_failed", reason.code());
    }
    log_controller_event("inbound_failed", "inbound_closed fake-secret-sentinel");
    let written =
        std::fs::read_to_string(directory.path().join("logs/remote-control.log")).unwrap();
    for reason in [
        ControllerLaneFailure::ItemCapacity,
        ControllerLaneFailure::ByteBudget,
        ControllerLaneFailure::Closed,
    ] {
        assert!(written.contains(&format!("stage=inbound_failed detail={}", reason.code())));
    }
    assert!(written.contains("stage=inbound_failed detail=other"));
    assert!(!written.contains("fake-secret-sentinel"));
}

#[tokio::test]
async fn real_websocket_reader_and_product_sink_bound_valid_b_ciphertext_and_close_owner() {
    for (inbound_limits, expected) in [
        (limits(), ControllerLaneFailure::ItemCapacity),
        (
            InboundLimits {
                business_items: 8,
                total_bytes: 76 * 1024,
                ..limits()
            },
            ControllerLaneFailure::ByteBudget,
        ),
    ] {
        assert_websocket_ingress_failure(inbound_limits, expected).await;
    }
}

async fn assert_websocket_ingress_failure(
    inbound_limits: InboundLimits,
    expected: ControllerLaneFailure,
) {
    let fixture = fixture();
    let PairingFixture {
        relay,
        reservation,
        joined,
        a_keys,
        b_keys,
        ..
    } = fixture;
    let trust = relay.trust.clone();
    let mut handshake = ControllingHandshake::new(
        reservation["request_id"].as_str().unwrap(),
        "claim-0001",
        "2345678",
        a_keys,
        trust.clone(),
    );
    let mut b = ControlledHandshake::new(&reservation, b_keys, &trust).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = format!("ws://{}", listener.local_addr().unwrap());
    let (go, start) = oneshot::channel();
    let (release, finish) = oneshot::channel();
    let peer = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut socket = accept_hdr_async(stream, ControllerReaderProtocol)
            .await
            .unwrap();
        assert_eq!(
            receive(&mut socket).await["type"],
            "controller_pairing_claim"
        );
        socket
            .send(Message::Text(reservation.to_string().into()))
            .await
            .unwrap();
        let a_commit = receive(&mut socket).await;
        b.accept_peer_commitment(&a_commit).unwrap();
        socket
            .send(Message::Text(a_commit.to_string().into()))
            .await
            .unwrap();
        socket
            .send(Message::Text(b.commit().unwrap().to_string().into()))
            .await
            .unwrap();
        socket
            .send(Message::Text(b.finished().unwrap().to_string().into()))
            .await
            .unwrap();
        let a_finished = receive(&mut socket).await;
        b.accept_peer_finished(&a_finished).unwrap();
        assert_eq!(receive(&mut socket).await["type"], "finished_receipt_ack");
        let lease = b.accept_joined(&joined).unwrap();
        socket
            .send(Message::Text(joined.to_string().into()))
            .await
            .unwrap();
        start.await.unwrap();
        let context = LeaseContext {
            connection_id: lease.connection_id,
            controller_lease_id: lease.controller_lease_id,
            lease_epoch: lease.lease_epoch.to_string(),
            relay_epoch: lease.relay_epoch,
            remote_control_generation: lease.remote_control_generation,
        };
        let caps = b.negotiated_capabilities().to_vec();
        let mut cipher =
            EncryptedLane::new(HandshakeRole::B, &context, b.take_session().unwrap()).unwrap();
        for sequence in 1..=3 {
            // Use the real writer's sanitization and character limit, keeping
            // this a valid large journal event rather than an invalid token.
            let frame = journal_frame(&mut cipher, &context, &caps, &"x ".repeat(2048), sequence);
            let raw = frame.to_string();
            assert!(raw.len() > 4096 && raw.len() < 12 * 1024);
            // In the byte-budget case the real reader may cancel after the
            // second frame. The parent's exact local-failure assertion still
            // rejects an unrelated network failure or a missing saturation.
            if socket.send(Message::Text(raw.into())).await.is_err() {
                break;
            }
        }
        let _ = finish.await;
    });
    let manager = ControllingDeviceManager::default();
    let operation = manager.connect("2345678").unwrap();
    let business = ControllingLaneState::default();
    let (sender, mut port) = spawn_controlling_lane_with_limits(&endpoint, inbound_limits);
    manager
        .replace_owned_transport(&operation, sender.clone(), &business)
        .unwrap();
    tokio::time::timeout(
        Duration::from_secs(10),
        run_controlling_connection(
            &manager,
            operation.clone(),
            &mut port,
            &mut handshake,
            &sender,
            &business,
            Duration::from_secs(5),
            &mut |_| {},
        ),
    )
    .await
    .unwrap()
    .unwrap();
    assert_eq!(manager.status().kind(), ControllingStatusKind::Connected);
    let joined_remaining = port.inbound.remaining_bytes();
    assert!(joined_remaining < inbound_limits.total_bytes);
    go.send(()).unwrap();
    let failure = tokio::time::timeout(Duration::from_secs(5), port.inbound.wait_for_failure())
        .await
        .unwrap();
    assert_eq!(failure, expected);
    // Fatal returns all queued large frames, retaining only the exact charge
    // observed before the peer was allowed to start its burst.
    assert_eq!(port.inbound.remaining_bytes(), joined_remaining);
    assert!(sender.is_stopped());
    let sessions = ControllingSessionManager::default();
    let mut status = Vec::new();
    let mut on_status = |value: &ControllingStatus| status.push(value.kind());
    let mut sinks = ControllingLaneSinks {
        on_status: &mut on_status,
        on_session: &mut |_| {},
        on_mapping: &mut |_, _, _| Ok(()),
        on_task: &mut |_, _| {},
        on_journal: &mut |_| panic!("fatal must retire queued projection"),
        on_snapshot: &mut |_| Ok(()),
        on_liveness: &mut |_| true,
    };
    tokio::time::timeout(
        Duration::from_secs(5),
        serve_controlling_lane_with_evidence_context(
            &mut port,
            &manager,
            &sessions,
            &business,
            &mut sinks,
            Duration::from_secs(3),
            PeerEvidenceContext {
                trust: Some(&trust),
                monotonic: &|| 0,
                operation,
            },
        ),
    )
    .await
    .unwrap();
    assert_eq!(status, vec![ControllingStatusKind::Error]);
    assert!(!business.is_installed());
    assert!(manager.live_lease().is_err());
    assert_eq!(port.inbound.remaining_bytes(), inbound_limits.total_bytes);
    let _ = release.send(());
    peer.await.unwrap();
}
