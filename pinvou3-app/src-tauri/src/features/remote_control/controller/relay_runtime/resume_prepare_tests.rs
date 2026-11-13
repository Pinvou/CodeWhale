//! Canonical lifecycle authorization and per-socket prepare regeneration.
use super::*;
use std::sync::atomic::AtomicBool;

#[derive(Default)]
struct Port {
    epoch: Mutex<Option<u64>>,
    frames: Mutex<Vec<(Value, u64, u64)>>,
    race_once: AtomicBool,
}

#[async_trait::async_trait]
impl ControlledRelayPort for Port {
    fn try_send(&self, _frame: Value) -> Result<(), String> {
        panic!("prepare must use the deadline/socket-scoped enqueue")
    }
    fn resume_socket_epoch(&self, _lease: &LeaseContext) -> Option<u64> {
        *self.epoch.lock()
    }
    fn try_send_resume_prepare(
        &self,
        frame: Value,
        epoch: u64,
        cutoff: u64,
    ) -> Result<bool, String> {
        if self.race_once.swap(false, Ordering::SeqCst) || *self.epoch.lock() != Some(epoch) {
            return Ok(false);
        }
        self.frames.lock().push((frame, epoch, cutoff));
        Ok(true)
    }
    async fn recv(&mut self) -> Option<Value> {
        None
    }
}

fn prepared(
    port: &Port,
) -> (
    ControlledDeviceManager,
    ControlledTransportOwner,
    ControlledResumeAttempt,
) {
    use crate::features::remote_control::controller::test_support::{
        lease_context, pairing_fixture, resume_reader_transport,
    };
    let manager = ControlledDeviceManager::default();
    let operation = manager.start(None, Vec::new()).unwrap();
    let fixture = pairing_fixture("prepare-requeue-test");
    let lease = LeaseContext {
        remote_control_generation: operation.generation().to_owned(),
        ..lease_context(&fixture.reservation)
    };
    let now = controlled_monotonic_millis();
    let capabilities = vec![
        "resume_v1".into(),
        "role_liveness_v1".into(),
        "encrypted_frames_v1".into(),
    ];
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
    manager
        .complete_connected(
            operation.clone(),
            ControlledLease {
                context: lease.clone(),
                liveness_epoch_a: "live-a-1".into(),
                resume_deadline_b: 1_700_000_030_000,
                resume_token: [1; 32],
                transport_key_fingerprint: [2; 32],
                controller_device_name: "controller".into(),
                connected_at_b: now,
                negotiated_capabilities: capabilities.clone(),
            },
        )
        .unwrap();
    let transport = resume_reader_transport(
        super::super::relay_transport::ControllerRole::ControlledB,
        lease,
        capabilities,
    );
    manager
        .replace_authenticated_transport(operation.clone(), transport.clone())
        .unwrap();
    let owner = manager.transport_owner_for_test(&operation).unwrap();
    super::super::relay_transport::disconnect_test_transport(&transport);
    let mut attempt = ControlledResumeAttempt::default();
    assert!(begin_owned_controlled_resume(
        port,
        &manager,
        &owner,
        &mut attempt,
        || {}
    ));
    (manager, owner, attempt)
}

fn connected(
    port: &Port,
    manager: &ControlledDeviceManager,
    attempt: &mut ControlledResumeAttempt,
) {
    assert_eq!(
        retry_controlled_resume_prepare(port, manager, attempt, controlled_monotonic_millis),
        ControlledResumeOutcome::Ignored
    );
}

#[test]
fn prepare_waits_for_controller_socket_and_rebuilds_only_once_per_successor() {
    let port = Port::default();
    let (manager, _, mut attempt) = prepared(&port);
    let cutoff = manager.watchdog_deadline_for_test();
    assert!(
        port.frames.lock().is_empty(),
        "disconnected socket must not pre-spend its successor epoch"
    );
    *port.epoch.lock() = Some(3);
    connected(&port, &manager, &mut attempt);
    let first = port.frames.lock()[0].clone();
    assert_eq!(first.1, 3);
    assert_eq!(Some(first.2), cutoff);
    assert_eq!(
        first.0["resume_token_b64"],
        base64::engine::general_purpose::STANDARD.encode([1; 32])
    );
    validate_controller_message_json(&first.0.to_string(), SenderRole::ControlledB).unwrap();
    for _ in 0..3 {
        connected(&port, &manager, &mut attempt);
    }
    assert_eq!(
        port.frames.lock().len(),
        1,
        "duplicate connectivity must not duplicate prepare"
    );
    *port.epoch.lock() = None; // no controller mode, including a Legacy socket
    connected(&port, &manager, &mut attempt);
    *port.epoch.lock() = Some(7); // intervening sockets consumed several epochs
    connected(&port, &manager, &mut attempt);
    let frames = port.frames.lock();
    assert_eq!(frames.len(), 2);
    assert_eq!(frames[1].1, 7);
    assert_eq!(
        frames[1].0, first.0,
        "retry must keep Relay's exact request/challenge identity"
    );
    assert_eq!(frames[1].2, first.2);
    assert_eq!(manager.watchdog_deadline_for_test(), cutoff);
    assert_eq!(
        attempt.request_ids.len(),
        1,
        "socket retries must not accumulate error receipts"
    );
}

#[test]
fn enqueue_socket_race_does_not_abort_or_renew_the_candidate() {
    let port = Port::default();
    let (manager, _, mut attempt) = prepared(&port);
    let cutoff = manager.watchdog_deadline_for_test();
    let request = attempt.request_ids.clone();
    let challenge = attempt.challenge_b64.clone();
    *port.epoch.lock() = Some(2);
    port.race_once.store(true, Ordering::SeqCst);
    connected(&port, &manager, &mut attempt);
    assert!(port.frames.lock().is_empty());
    assert_eq!(attempt.prepare_epoch, None);
    *port.epoch.lock() = Some(4);
    connected(&port, &manager, &mut attempt);
    assert_eq!(port.frames.lock().len(), 1);
    assert_eq!(attempt.prepare_epoch, Some(4));
    assert_eq!(attempt.request_ids, request);
    assert_eq!(attempt.challenge_b64, challenge);
    assert_eq!(manager.watchdog_deadline_for_test(), cutoff);
}

#[test]
fn expired_or_provisional_candidate_cannot_enqueue_again() {
    let port = Port::default();
    let (manager, owner, mut attempt) = prepared(&port);
    *port.epoch.lock() = Some(2);
    let cutoff = manager.watchdog_deadline_for_test().unwrap();
    assert_eq!(
        retry_controlled_resume_prepare(&port, &manager, &mut attempt, || cutoff),
        ControlledResumeOutcome::Ignored
    );
    assert!(
        port.frames.lock().is_empty(),
        "exact frozen cutoff cannot admit a prepare"
    );
    manager
        .poll_watchdog_for(owner.operation(), cutoff)
        .unwrap();
    assert_eq!(manager.status().kind(), ControlledStatusKind::Error);
    connected(&port, &manager, &mut attempt);
    assert!(port.frames.lock().is_empty());

    // A separate candidate exercises later-phase rejection without moving the
    // first candidate's monotonic clock backwards after its expiry.
    let port = Port::default();
    let (manager, owner, mut attempt) = prepared(&port);
    let cutoff = manager.watchdog_deadline_for_test().unwrap();
    *port.epoch.lock() = Some(2);
    manager
        .advance_owned_resume(
            &owner,
            ControlledResumeStep::Provisional {
                activation_id: "activation-prepare-1",
            },
            controlled_monotonic_millis(),
        )
        .unwrap();
    connected(&port, &manager, &mut attempt);
    assert!(
        port.frames.lock().is_empty(),
        "later recovery phases must not be prepared again"
    );
    assert_eq!(manager.watchdog_deadline_for_test(), Some(cutoff));
}

#[test]
fn same_generation_transport_replacement_and_stop_reject_old_candidate() {
    let port = Port::default();
    let (manager, owner, mut attempt) = prepared(&port);
    let replacement = super::super::test_support::resume_reader_transport(
        super::super::relay_transport::ControllerRole::ControlledB,
        owner.lease().clone(),
        vec![
            "resume_v1".into(),
            "role_liveness_v1".into(),
            "encrypted_frames_v1".into(),
        ],
    );
    manager
        .replace_authenticated_transport(owner.operation().clone(), replacement)
        .unwrap();
    *port.epoch.lock() = Some(9);
    connected(&port, &manager, &mut attempt);
    assert!(
        port.frames.lock().is_empty(),
        "same tuple is not the same transport owner"
    );
    manager.stop();
    let next = manager.start(None, Vec::new()).unwrap();
    connected(&port, &manager, &mut attempt);
    assert!(port.frames.lock().is_empty());
    assert_eq!(manager.current_operation(), Some(next));
}
