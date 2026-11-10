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
    let disconnect =
        fixture
            .manager
            .disconnect_business(&fixture.sessions, &fixture.business, |lease| {
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
        .disconnect_business(&fixture.sessions, &fixture.business, |_| {});
    let duplicate =
        fixture
            .manager
            .disconnect_business(&fixture.sessions, &fixture.business, |_| {});
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

struct CapabilityDisconnectRun {
    release: Option<std::sync::mpsc::Sender<bool>>,
    capability: Option<std::thread::JoinHandle<bool>>,
    disconnect: Option<std::thread::JoinHandle<()>>,
    probe: Option<std::thread::JoinHandle<()>>,
}

impl CapabilityDisconnectRun {
    fn finish(mut self, proceed: bool) -> (bool, bool, bool) {
        if let Some(release) = self.release.take() {
            let _ = release.send(proceed);
        }
        let sent = self.capability.take().unwrap().join().unwrap_or(false);
        let disconnected = self.disconnect.take().unwrap().join().is_ok();
        let probed = self.probe.take().unwrap().join().is_ok();
        (sent, disconnected, probed)
    }
}

impl Drop for CapabilityDisconnectRun {
    fn drop(&mut self) {
        // Abort the capability callback BEFORE it tries owner, then release its
        // intent guard. Even an owner-first negative must not strand ABBA threads.
        if let Some(release) = self.release.take() {
            let _ = release.send(false);
        }
        if let Some(thread) = self.capability.take() {
            let _ = thread.join();
        }
        if let Some(thread) = self.disconnect.take() {
            let _ = thread.join();
        }
        if let Some(thread) = self.probe.take() {
            let _ = thread.join();
        }
    }
}

#[tokio::test]
async fn capability_send_and_disconnect_follow_the_same_intent_owner_order() {
    use std::sync::{Arc, mpsc};
    let fixture = connected(limits());
    let lease = fixture.lease.clone();
    let capabilities = fixture.capabilities.clone();
    let operation = fixture.operation.clone();
    let manager = Arc::new(fixture.manager);
    let sessions = Arc::new(fixture.sessions);
    let business = Arc::new(fixture.business);
    let mut outbound = fixture._outbound;
    let expected = manager.authenticated_remote_proxy_identity().unwrap();
    let (session, _) = sessions
        .open_with_outcome(
            Some("retirement-lock-order"),
            "Retirement lock order",
            "chat",
            &lease,
            &capabilities,
        )
        .unwrap();
    let remote_ref = session.session_ref;
    let (held, holding) = mpsc::channel();
    let (release, released) = mpsc::channel();
    let (attempt, attempting) = mpsc::channel();
    sessions.before_retirement_lock_for_test(move || {
        attempt.send(()).unwrap();
    });
    let worker_manager = manager.clone();
    let worker_sessions = sessions.clone();
    let worker_business = business.clone();
    let worker_ref = remote_ref.clone();
    let capability = std::thread::spawn(move || {
        worker_sessions
            .capability_request_with_send(&worker_ref, &expected.0, |payload| {
                // This is the real session-intent guard and real producer callback.
                held.send(()).unwrap();
                if released.recv_timeout(Duration::from_secs(30)) != Ok(true) {
                    return Err("controlled_order_probe_abort".into());
                }
                worker_manager.send_capability_for_owner(
                    &operation,
                    &expected,
                    &worker_business,
                    payload,
                    |_| "controller_send_failed".to_owned(),
                )
            })
            .is_ok()
    });
    let mut run = CapabilityDisconnectRun {
        release: Some(release),
        capability: Some(capability),
        disconnect: None,
        probe: None,
    };
    let held = holding.recv_timeout(Duration::from_secs(5));
    let disconnect_manager = manager.clone();
    let disconnect_sessions = sessions.clone();
    let disconnect_business = business.clone();
    run.disconnect = Some(std::thread::spawn(move || {
        drop(disconnect_manager.disconnect_business(
            &disconnect_sessions,
            &disconnect_business,
            |_| {},
        ));
    }));
    // The hook is inside the actual session gate, before its lock. An incorrect
    // owner-first composition reaches this signal already holding the owner.
    let attempted = attempting.recv_timeout(Duration::from_secs(5));
    let (probed, observing) = mpsc::channel();
    let probe_manager = manager.clone();
    run.probe = Some(std::thread::spawn(move || {
        let _ = probed.send(probe_manager.status());
    }));
    let observed = observing.recv_timeout(Duration::from_secs(5)).ok();
    let proceed = held.is_ok()
        && attempted.is_ok()
        && observed
            .as_ref()
            .is_some_and(|status| status.kind() == ControllingStatusKind::Connected);
    let (sent, disconnected, probed) = run.finish(proceed);
    assert!(
        held.is_ok(),
        "actual capability producer must hold the session intent"
    );
    assert!(
        attempted.is_ok(),
        "disconnect must reach the real session gate"
    );
    assert!(
        observed.is_some(),
        "disconnect must not hold owner while waiting for capability intents"
    );
    assert!(
        sent,
        "the actual capability enqueue must complete before retirement"
    );
    assert!(disconnected && probed);
    assert!(
        outbound.take_business_for_test().is_some(),
        "the actual encrypted producer must enqueue a frame"
    );
    assert_eq!(manager.status().kind(), ControllingStatusKind::Disconnected);
    assert!(!business.is_installed());
    assert!(
        sessions
            .pending_capability_ids_for_test(&remote_ref)
            .is_err()
    );
}
