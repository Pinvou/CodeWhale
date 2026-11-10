//! Real handshake/manager/key store with only the keeper's next-event boundary
//! controlled. This does not claim WebSocket closure or full UI execution.
use super::*;

struct EndLane<F> {
    sender: ControllerTransportSender,
    boundary: Option<F>,
}

#[async_trait::async_trait]
impl<F: FnOnce() + Send> ControllingLane for EndLane<F> {
    async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
        self.boundary.take().unwrap()();
        None
    }

    fn transport_sender(&self) -> Option<ControllerTransportSender> {
        Some(self.sender.clone())
    }
}

async fn end_keeper(fixture: &Connected, boundary: impl FnOnce() + Send) {
    let mut lane = EndLane {
        sender: fixture.sender.clone(),
        boundary: Some(boundary),
    };
    let mut sinks = ControllingLaneSinks {
        on_status: &mut |_| {},
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
            &mut lane,
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
}

#[tokio::test]
async fn keeper_closed_input_retires_real_installed_business_keys() {
    let fixture = connected(limits());
    assert!(fixture.business.is_installed());
    end_keeper(&fixture, || {}).await;
    assert!(!fixture.business.is_installed());
}

#[tokio::test]
async fn keeper_owner_loss_retires_its_original_business_keys() {
    let fixture = connected(limits());
    end_keeper(&fixture, || fixture.manager.stop()).await;
    assert!(!fixture.business.is_installed());
}

#[tokio::test]
async fn delayed_keeper_exit_preserves_successor_keys_and_recovery_gate() {
    let fixture = connected(limits());
    let successor = parking_lot::Mutex::new(None);
    end_keeper(&fixture, || {
        fixture.manager.disconnect();
        *successor.lock() = Some(install(&fixture.manager, &fixture.business));
        fixture.business.suspend();
    })
    .await;
    assert!(fixture.business.is_installed());
    assert!(fixture.business.is_suspended());
    assert!(!successor.lock().as_ref().unwrap().1.is_stopped());
}

#[tokio::test]
async fn stale_keeper_cannot_capture_successor_installation() {
    let fixture = connected(limits());
    fixture.manager.disconnect();
    let (_, sender, _outbound, _peer, _trust) = install(&fixture.manager, &fixture.business);
    fixture.business.suspend();
    end_keeper(&fixture, || {
        panic!("stale keeper must not poll the successor")
    })
    .await;
    assert!(fixture.business.is_installed());
    assert!(fixture.business.is_suspended());
    assert!(!sender.is_stopped());
}

#[tokio::test(start_paused = true)]
async fn disconnect_retires_keys_before_receipt_and_late_flush_preserves_reconnect() {
    let fixture = connected(limits());
    let old_keeper = fixture.business.retire_on_exit();
    let disconnect = fixture
        .manager
        .disconnect_business(&fixture.business, |lease| {
            assert_eq!(lease, Some(&fixture.lease));
        });
    assert!(!fixture.business.is_installed());
    assert_eq!(
        fixture.manager.status().kind(),
        ControllingStatusKind::Disconnected
    );
    assert!(fixture.manager.live_runtime_context().is_err());
    assert!(
        !fixture.sender.is_stopped(),
        "old final frame can still flush"
    );
    assert_eq!(fixture._outbound.pending_control_for_test(), 1);
    let (_, successor, _outbound, _peer, _trust) = install(&fixture.manager, &fixture.business);
    fixture.business.suspend();
    drop(old_keeper);
    let mut flushing = Box::pin(disconnect.flush());
    assert!(futures_util::poll!(flushing.as_mut()).is_pending());
    assert!(!fixture.sender.is_stopped());
    tokio::time::advance(Duration::from_secs(2)).await;
    flushing.await;
    assert!(fixture.sender.is_stopped());
    assert!(!successor.is_stopped());
    assert!(fixture.business.is_installed());
    assert!(fixture.business.is_suspended());
    assert_eq!(
        fixture.manager.status().kind(),
        ControllingStatusKind::Connected
    );
}

#[tokio::test]
async fn duplicate_and_cancelled_disconnect_scopes_only_stop_captured_transport() {
    let fixture = connected(limits());
    let old = fixture
        .manager
        .disconnect_business(&fixture.business, |_| {});
    let duplicate = fixture
        .manager
        .disconnect_business(&fixture.business, |_| {});
    let (_, successor, _outbound, _peer, _trust) = install(&fixture.manager, &fixture.business);
    // A future can be dropped before first poll. Local cleanup already committed.
    drop(old.flush());
    drop(duplicate);
    assert!(fixture.sender.is_stopped());
    assert!(!successor.is_stopped());
    assert!(fixture.business.is_installed());
    assert_eq!(
        fixture.manager.status().kind(),
        ControllingStatusKind::Connected
    );
}
