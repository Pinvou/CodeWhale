//! Preconnection absolute deadlines, final admission and exact timer ownership.
use super::*;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::mpsc;
use std::time::{Duration, Instant};

fn waiting(now: u64, expiry: u64) -> (Arc<ControlledDeviceManager>, ControlledOperation) {
    let manager = Arc::new(ControlledDeviceManager::default());
    let operation = manager.start(None, Vec::new()).unwrap();
    manager
        .complete_registration(
            operation.clone(),
            ControlledRegistration {
                pairing_code: "2345678".into(),
                sealed_code_b64: "sealed".into(),
                expires_at_ms: expiry,
            },
            now,
        )
        .unwrap();
    (manager, operation)
}

fn pending() -> (Arc<ControlledDeviceManager>, ControlledOperation) {
    let (manager, operation) = waiting(0, 100);
    manager
        .mark_pairing_reserved_at(operation.clone(), 20_000, || 1)
        .unwrap();
    manager
        .mark_handshake_pending_at(operation.clone(), || 2)
        .unwrap();
    (manager, operation)
}

fn lease(operation: &ControlledOperation, now: u64) -> ControlledLease {
    ControlledLease {
        context: LeaseContext {
            connection_id: "connection-preconnect-0001".into(),
            controller_lease_id: "lease-preconnect-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-preconnect-0001".into(),
            remote_control_generation: operation.generation().into(),
        },
        liveness_epoch_a: "liveness-preconnect-0001".into(),
        resume_deadline_b: now + 30_000,
        resume_token: [1; 32],
        transport_key_fingerprint: [2; 32],
        controller_device_name: "controller".into(),
        connected_at_b: now,
        negotiated_capabilities: vec![
            "encrypted_frames_v1".into(),
            "pairing_v1".into(),
            "task_v1".into(),
        ],
    }
}

#[derive(Default)]
struct Cleanup(Mutex<Vec<String>>);
impl ControlledCleanupSink for Cleanup {
    fn cleanup(&self, generation: &str, scope: ControlledCleanupScope) {
        assert!(scope.remote_tasks && scope.session_keys && scope.temporary_models);
        self.0.lock().push(generation.into());
    }
}

#[test]
fn waiting_expiry_clears_code_authority_once_and_allows_next_start() {
    let (manager, operation) = waiting(0, 100);
    let cleanup = Arc::new(Cleanup::default());
    manager.register_cleanup_sink(cleanup.clone());
    let before = manager.poll_watchdog(99).unwrap();
    assert_eq!(before.kind(), ControlledStatusKind::WaitingForPairing);
    assert_eq!(before.pairing_code.as_deref(), Some("2345678"));
    let expired = manager.poll_watchdog(100).unwrap();
    assert_eq!(expired.kind(), ControlledStatusKind::Error);
    assert_eq!(
        expired.error_code.as_deref(),
        Some("desktop_remote_pairing_expired")
    );
    assert!(expired.pairing_code.is_none());
    assert!(expired.pairing_remaining_ms.is_none());
    {
        let state = manager.inner.lock();
        assert!(state.pairing.is_none());
        assert!(state.preconnect_deadline.is_none());
        assert!(state.generation.is_none());
        assert!(state.registration_request_id.is_none());
    }
    manager.poll_watchdog(200).unwrap();
    assert_eq!(*cleanup.0.lock(), vec![operation.generation().to_owned()]);
    let next = manager.start(None, Vec::new()).unwrap();
    assert_ne!(operation, next);
    manager
        .complete_registration(
            next.clone(),
            ControlledRegistration {
                pairing_code: "2345678".into(),
                sealed_code_b64: "sealed".into(),
                expires_at_ms: 400,
            },
            200,
        )
        .unwrap();
    assert_eq!(
        manager.status().kind(),
        ControlledStatusKind::WaitingForPairing
    );
}

#[test]
fn reservation_cannot_renew_an_expired_waiting_code() {
    let (manager, operation) = waiting(0, 100);
    let old = manager.preconnect_deadline(&operation).unwrap().unwrap();
    assert_eq!(
        manager
            .mark_pairing_reserved_at(operation.clone(), 20_100, || 100)
            .unwrap_err()
            .code,
        "desktop_remote_pairing_expired"
    );
    assert_eq!(manager.preconnect_deadline(&operation).unwrap(), Some(old));
    assert_eq!(
        manager.status().kind(),
        ControlledStatusKind::WaitingForPairing
    );
}

#[test]
fn reservation_switches_phase_once_and_pending_does_not_renew_budget() {
    let (manager, operation) = waiting(0, 100);
    let waiting_ticket = manager.preconnect_deadline(&operation).unwrap().unwrap();
    manager
        .mark_pairing_reserved_at(operation.clone(), 20_099, || 99)
        .unwrap();
    let handshake = manager.preconnect_deadline(&operation).unwrap().unwrap();
    assert_ne!(waiting_ticket, handshake);
    assert_eq!(handshake.deadline_b(), 20_099);
    assert!(
        manager
            .expire_preconnect(&waiting_ticket, || 100)
            .unwrap()
            .is_none()
    );
    manager
        .mark_handshake_pending_at(operation.clone(), || 100)
        .unwrap();
    assert_eq!(
        manager.preconnect_deadline(&operation).unwrap(),
        Some(handshake.clone())
    );
    assert!(
        manager
            .mark_pairing_reserved_at(operation.clone(), 30_000, || 101)
            .is_err()
    );
    assert!(
        manager
            .mark_handshake_pending_at(operation.clone(), || 101)
            .is_err()
    );
    assert_eq!(
        manager.preconnect_deadline(&operation).unwrap(),
        Some(handshake)
    );
    assert_eq!(
        manager.poll_watchdog(20_098).unwrap().kind(),
        ControlledStatusKind::HandshakePending
    );
    let expired = manager.poll_watchdog(20_099).unwrap();
    assert_eq!(expired.kind(), ControlledStatusKind::Error);
    assert_eq!(
        expired.error_code.as_deref(),
        Some("desktop_remote_handshake_timeout")
    );
}

#[test]
fn connected_clears_original_ticket_and_late_timer_does_not_kill_same_operation() {
    let (manager, operation) = pending();
    let ticket = manager.preconnect_deadline(&operation).unwrap().unwrap();
    let materials = lease(&operation, 19_999);
    let (sender, _receiver) = super::super::relay_transport::transport_channels(
        super::super::relay_transport::ControllerRole::ControlledB,
        operation.generation(),
        8,
        2,
        8192,
        1024,
    );
    sender
        .install_authenticated_lease(super::super::relay_transport::AuthenticatedLease {
            context: materials.context.clone(),
            negotiated_capabilities: materials.negotiated_capabilities.clone(),
        })
        .unwrap();
    let called = AtomicUsize::new(0);
    let status = manager
        .complete_connected_with_install(
            operation.clone(),
            materials,
            || 19_999,
            || {
                called.fetch_add(1, Ordering::SeqCst);
                Ok(Arc::new(sender) as Arc<dyn ControlledTransportAuthority>)
            },
        )
        .unwrap();
    assert_eq!(called.load(Ordering::SeqCst), 1);
    assert_eq!(status.kind(), ControlledStatusKind::Connected);
    assert!(manager.preconnect_deadline(&operation).unwrap().is_none());
    assert!(
        manager
            .expire_preconnect(&ticket, || 20_001)
            .unwrap()
            .is_none()
    );
    assert_eq!(
        manager.poll_watchdog(20_001).unwrap().kind(),
        ControlledStatusKind::Connected
    );
    assert!(manager.live_lease().is_ok());
}

#[test]
fn joined_at_or_after_deadline_never_calls_shared_install_without_reaper_tick() {
    for now in [20_000, 20_001] {
        let (manager, operation) = pending();
        let ticket = manager.preconnect_deadline(&operation).unwrap().unwrap();
        let called = AtomicUsize::new(0);
        let error = manager
            .complete_connected_with_install(
                operation.clone(),
                lease(&operation, 19_999),
                || now,
                || {
                    called.fetch_add(1, Ordering::SeqCst);
                    Err(ControlledStateError::new("unexpected_install"))
                },
            )
            .unwrap_err();
        assert_eq!(error.code, "desktop_remote_handshake_timeout");
        assert_eq!(called.load(Ordering::SeqCst), 0);
        assert!(manager.inner.lock().lease_context.is_none());
        assert!(
            manager
                .expire_preconnect(&ticket, || now)
                .unwrap()
                .is_some()
        );
        assert_eq!(manager.status().kind(), ControlledStatusKind::Error);
    }
}

#[test]
fn refresh_invalidates_old_timer_even_while_generation_is_still_retiring() {
    let (manager, operation) = pending();
    let ticket = manager.preconnect_deadline(&operation).unwrap().unwrap();
    let retiring = manager.begin_refresh().unwrap();
    assert_eq!(operation.generation(), retiring.generation());
    assert_ne!(operation, retiring);
    assert!(manager.inner.lock().preconnect_deadline.is_none());
    assert_eq!(
        manager
            .expire_preconnect(&ticket, || u64::MAX)
            .unwrap_err()
            .code,
        "desktop_remote_stale_result"
    );
    assert_eq!(
        manager.status().kind(),
        ControlledStatusKind::RetiringGeneration
    );
    let next = manager.complete_refresh_revoked(retiring).unwrap();
    assert_ne!(next.generation(), operation.generation());
    assert!(manager.expire_preconnect(&ticket, || u64::MAX).is_err());
    assert_eq!(manager.status().kind(), ControlledStatusKind::Registering);
}

#[test]
fn joined_waiting_for_owner_samples_clock_after_lock_and_rejects_old_lease_time() {
    let (manager, operation) = pending();
    let clock = Arc::new(AtomicU64::new(19_999));
    let observed = Arc::new(AtomicU64::new(0));
    let installs = Arc::new(AtomicUsize::new(0));
    let (entered_tx, entered_rx) = mpsc::sync_channel(0);
    let (result_tx, result_rx) = mpsc::sync_channel(0);
    let guard = manager.inner.lock();
    let worker_manager = manager.clone();
    let worker_clock = clock.clone();
    let worker_observed = observed.clone();
    let worker_installs = installs.clone();
    let worker = std::thread::spawn(move || {
        let old_materials = lease(&operation, 19_999);
        entered_tx.send(()).unwrap();
        let result = worker_manager.complete_connected_with_install(
            operation,
            old_materials,
            || {
                let now = worker_clock.load(Ordering::SeqCst);
                worker_observed.store(now, Ordering::SeqCst);
                now
            },
            || {
                worker_installs.fetch_add(1, Ordering::SeqCst);
                Err(ControlledStateError::new("unexpected_install"))
            },
        );
        result_tx.send(result).unwrap();
    });
    entered_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    clock.store(20_000, Ordering::SeqCst);
    drop(guard);
    let result = result_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    assert_eq!(result.unwrap_err().code, "desktop_remote_handshake_timeout");
    assert_eq!(observed.load(Ordering::SeqCst), 20_000);
    assert_eq!(installs.load(Ordering::SeqCst), 0);
    let deadline = Instant::now() + Duration::from_secs(5);
    while !worker.is_finished() {
        assert!(
            Instant::now() < deadline,
            "final admission thread failed to finish"
        );
        std::thread::yield_now();
    }
    worker.join().unwrap();
}

#[test]
fn reservation_rejects_expired_or_over_cap_deadline_without_consuming_waiting_code() {
    for (deadline, expected) in [
        (1, "desktop_remote_handshake_timeout"),
        (20_002, "desktop_remote_handshake_deadline_invalid"),
    ] {
        let (manager, operation) = waiting(0, 100);
        let original = manager.preconnect_deadline(&operation).unwrap();
        assert_eq!(
            manager
                .mark_pairing_reserved_at(operation.clone(), deadline, || 1)
                .unwrap_err()
                .code,
            expected
        );
        assert_eq!(manager.preconnect_deadline(&operation).unwrap(), original);
        assert_eq!(manager.status().pairing_code.as_deref(), Some("2345678"));
        manager
            .mark_pairing_reserved_at(operation.clone(), 20_001, || 1)
            .unwrap();
        assert_eq!(
            manager
                .preconnect_deadline(&operation)
                .unwrap()
                .unwrap()
                .deadline_b(),
            20_001
        );
    }
}

#[test]
fn registration_waiting_for_owner_rechecks_expiry_inside_lock() {
    let manager = Arc::new(ControlledDeviceManager::default());
    let operation = manager.start(None, Vec::new()).unwrap();
    let clock = Arc::new(AtomicU64::new(99));
    let (entered_tx, entered_rx) = mpsc::sync_channel(0);
    let (result_tx, result_rx) = mpsc::sync_channel(0);
    let guard = manager.inner.lock();
    let worker_manager = manager.clone();
    let worker_clock = clock.clone();
    let worker = std::thread::spawn(move || {
        entered_tx.send(()).unwrap();
        result_tx
            .send(worker_manager.complete_registration_at(
                operation,
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: 100,
                },
                || worker_clock.load(Ordering::SeqCst),
            ))
            .unwrap();
    });
    entered_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    clock.store(100, Ordering::SeqCst);
    drop(guard);
    assert_eq!(
        result_rx
            .recv_timeout(Duration::from_secs(5))
            .unwrap()
            .unwrap_err()
            .code,
        "pairing_code_expired"
    );
    assert_eq!(manager.status().kind(), ControlledStatusKind::Registering);
    assert!(manager.inner.lock().pairing.is_none());
    assert!(manager.inner.lock().preconnect_deadline.is_none());
    let deadline = Instant::now() + Duration::from_secs(5);
    while !worker.is_finished() {
        assert!(Instant::now() < deadline);
        std::thread::yield_now();
    }
    worker.join().unwrap();
}
