//! Strict terminal loss after a real unaccepted prepare write or Legacy close.
use super::*;

pub(super) async fn receive_prepare(socket: &mut Socket) -> Value {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            match socket
                .next()
                .await
                .expect("successor socket closed")
                .unwrap()
            {
                Message::Text(text) => {
                    let frame: Value = serde_json::from_str(&text).unwrap();
                    if frame["type"] == "controller_resume_prepare" {
                        validate_controller_message_json(&text, SenderRole::ControlledB).unwrap();
                        return frame;
                    }
                }
                Message::Ping(body) => socket.send(Message::Pong(body)).await.unwrap(),
                _ => {}
            }
        }
    })
    .await
    .expect("successor socket did not receive regenerated prepare")
}

pub(super) async fn fault_ends_candidate(
    listener: &TcpListener,
    manager: &ControlledDeviceManager,
    lane: &ControlledLaneState,
    outbound: &ControlledOutboundState,
    owned: &mut OwnedLane,
    completion: &mut tokio::sync::mpsc::Receiver<Value>,
    case: &str,
) {
    let relay = owned.relay.clone();
    let frozen = manager.watchdog_deadline_for_test().unwrap();
    assert_eq!(
        manager.resume_phase_for_test(),
        Some(super::super::super::resume::ResumePhase::Prepared)
    );
    assert!(lane.is_installed() && lane.is_suspended());
    assert!(frozen > controlled_monotonic_millis() + 3_000);
    let observation = second_disconnect_tests::observe_terminal_loss(manager);
    if case == "prepare_write" {
        assert!(relay.failed_prepare_digest_for_test().is_none());
        let mut failed = accept(listener).await;
        // No setup-ACK send races the deliberate close. The real client has
        // already written registration before opening controller authority.
        let Message::Text(register) = failed.next().await.unwrap().unwrap() else {
            panic!("register missing")
        };
        assert_eq!(
            serde_json::from_str::<Value>(&register).unwrap()["type"],
            "desktop_endpoint_register"
        );
        tokio::time::timeout(Duration::from_secs(3), async {
            loop {
                match failed.next().await {
                    None | Some(Err(_)) | Some(Ok(Message::Close(_))) => break,
                    Some(Ok(Message::Text(text))) => {
                        panic!("injected pre-write failure delivered bytes: {text}")
                    }
                    Some(Ok(Message::Ping(body))) => {
                        let _ = failed.send(Message::Pong(body)).await;
                    }
                    _ => {}
                }
            }
        })
        .await
        .expect("actual failed prepare writer did not close its socket");
        assert!(
            relay.failed_prepare_digest_for_test().is_some(),
            "fault must hit the real prepared-frame writer"
        );
    } else {
        assert_eq!(case, "prepare_legacy");
        // A real missing subprotocol rejects the first upgrade. The actual
        // fallback then registers a Legacy-only socket, consuming an epoch.
        let (offer, _) = listener.accept().await.unwrap();
        drop(tokio_tungstenite::accept_async(offer).await.unwrap());
        let (stream, _) = listener.accept().await.unwrap();
        let mut legacy = tokio_tungstenite::accept_async(stream).await.unwrap();
        registered(&mut legacy).await;
        let connected = json!({"type":"desktop_relay_connection", "endpoint_id":relay.controller_endpoint_id().unwrap(), "connected":true, "error":null});
        processed(completion, &connected).await;
        assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
        assert_eq!(
            manager.resume_phase_for_test(),
            Some(super::super::super::resume::ResumePhase::Prepared)
        );
        assert!(lane.is_installed() && lane.is_suspended());
        // The worker has finished handling connectivity. A Ping/Pong read
        // barrier also drains already queued outgoing frames before success.
        let marker = vec![17, 72];
        legacy
            .send(Message::Ping(marker.clone().into()))
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(3), async {
            loop {
                match legacy.next().await.unwrap().unwrap() {
                    Message::Pong(body) if body.as_ref() == marker.as_slice() => break,
                    Message::Text(text) => assert_ne!(
                        serde_json::from_str::<Value>(&text).unwrap()["v"],
                        1,
                        "controller prepare leaked to Legacy"
                    ),
                    Message::Ping(body) => legacy.send(Message::Pong(body)).await.unwrap(),
                    _ => {}
                }
            }
        })
        .await
        .expect("Legacy traffic barrier failed");
        legacy.close(None).await.unwrap();
    }
    // Keep the join window strictly before the original watchdog cutoff.
    assert!(frozen > controlled_monotonic_millis() + 3_000);
    // The actual fault/close has now produced the second owned false.
    // Reuse the existing real-worker/key/outbound/single-cleanup assertions;
    // no cleanup callback in this fixture mutates product state.
    second_disconnect_tests::assert_terminal_loss(manager, lane, outbound, owned, &observation)
        .await;

    // Only after the old candidate is terminal may a new intent bind. Exercise
    // the real reader/channel, not just a successful allocation, on its socket.
    let next = manager.start(None, Vec::new()).unwrap();
    let mut next_channel = relay
        .start_controller_relay(next.generation())
        .await
        .unwrap();
    let mut next_socket = lifecycle_registered(listener).await;
    assert_lifecycle_route(&mut next_socket, &mut next_channel).await;
    drop(next_channel);
    manager.stop();
    next_socket.close(None).await.unwrap();
}
