//! B socket evidence producer. This never renews either endpoint's local budget.

use super::controlled::{ControlledDeviceManager, ControlledOperation};
use super::protocol::{LeaseContext, SenderRole};
use super::relay_runtime::ControlledRelayPort;

#[derive(Default)]
pub(crate) struct ControlledHeartbeat {
    lease: Option<LeaseContext>,
    epoch: Option<String>,
    sequence: u64,
}

#[cfg(test)]
pub(crate) mod test_support {
    use super::super::controlled::{ControlledLease, ControlledRegistration};
    use super::*;
    use serde_json::Value;

    #[derive(Default)]
    pub(crate) struct CapturePort {
        pub(crate) frames: parking_lot::Mutex<Vec<Value>>,
        pub(crate) reject: std::sync::atomic::AtomicBool,
    }
    #[async_trait::async_trait]
    impl ControlledRelayPort for CapturePort {
        fn try_send(&self, frame: Value) -> Result<(), String> {
            if self.reject.load(std::sync::atomic::Ordering::Relaxed) {
                return Err("send_failed".into());
            }
            self.frames.lock().push(frame);
            Ok(())
        }
        async fn recv(&mut self) -> Option<Value> {
            None
        }
    }
    pub(crate) fn connect(
        manager: &ControlledDeviceManager,
        operation: &ControlledOperation,
        lease: LeaseContext,
        epoch_a: &str,
        capabilities: Vec<String>,
    ) {
        manager
            .complete_registration(
                operation.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: 30_000,
                },
                0,
            )
            .unwrap();
        manager.mark_pairing_reserved(operation.clone()).unwrap();
        manager.mark_handshake_pending(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation.clone(),
                ControlledLease {
                    context: lease,
                    liveness_epoch_a: epoch_a.into(),
                    resume_deadline_b: 30_000,
                    resume_token: [1; 32],
                    transport_key_fingerprint: [2; 32],
                    controller_device_name: "test-controller".into(),
                    connected_at_b: 0,
                    negotiated_capabilities: capabilities,
                },
            )
            .unwrap();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use test_support::*;

    fn connected() -> (ControlledDeviceManager, ControlledOperation, LeaseContext) {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        let lease = LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "epoch-test-1".into(),
            remote_control_generation: operation.generation().into(),
        };
        connect(
            &manager,
            &operation,
            lease.clone(),
            "live-a-1",
            vec!["role_liveness_v1".into()],
        );
        (manager, operation, lease)
    }

    #[test]
    fn production_pump_emits_only_installed_owner_and_successful_sequences() {
        let (manager, operation, lease) = connected();
        let port = CapturePort::default();
        let mut heartbeat = ControlledHeartbeat::default();
        heartbeat.pump(&manager, &operation, false, &port).unwrap();
        assert!(port.frames.lock().is_empty());
        heartbeat.install(lease.clone(), "live-b-1").unwrap();
        heartbeat.pump(&manager, &operation, true, &port).unwrap();
        assert!(port.frames.lock().is_empty());
        port.reject
            .store(true, std::sync::atomic::Ordering::Relaxed);
        assert!(heartbeat.pump(&manager, &operation, false, &port).is_err());
        assert_eq!(heartbeat.sequence, 0);
        port.reject
            .store(false, std::sync::atomic::Ordering::Relaxed);
        heartbeat.pump(&manager, &operation, false, &port).unwrap();
        let frame = port.frames.lock()[0].clone();
        assert_eq!(frame["sender_role"], "B");
        assert_eq!(frame["heartbeat_sequence"], "1");
        super::super::protocol::validate_controller_runtime_message_json(
            &frame.to_string(),
            SenderRole::ControlledB,
            Some(&super::super::protocol::RuntimeValidationContext {
                lease: Some(lease.clone()),
                negotiated_capabilities: vec!["role_liveness_v1".into()],
            }),
        )
        .unwrap();
        heartbeat.install(lease, "live-b-1").unwrap();
        heartbeat.pump(&manager, &operation, false, &port).unwrap();
        assert_eq!(port.frames.lock()[1]["heartbeat_sequence"], "2");
        heartbeat.sequence = u64::MAX;
        assert!(heartbeat.pump(&manager, &operation, false, &port).is_err());
        assert_eq!(port.frames.lock().len(), 2);
    }

    #[test]
    fn transport_loss_and_retired_worker_cannot_publish_role_evidence() {
        let (manager, operation, lease) = connected();
        let port = CapturePort::default();
        let mut heartbeat = ControlledHeartbeat::default();
        heartbeat.install(lease, "live-b-1").unwrap();
        manager.mark_transport_disconnected(1);
        assert!(heartbeat.pump(&manager, &operation, false, &port).is_err());
        assert!(port.frames.lock().is_empty());
        manager.stop();
        manager.start(None, Vec::new()).unwrap();
        assert!(heartbeat.pump(&manager, &operation, false, &port).is_err());
        assert_eq!(heartbeat.sequence, 0);
    }

    #[test]
    fn different_lease_and_healthy_role_activity_never_renew_local_peer_budget() {
        let (manager, operation, lease) = connected();
        let port = CapturePort::default();
        let mut heartbeat = ControlledHeartbeat::default();
        let mut foreign = lease.clone();
        foreign.controller_lease_id = "lease-other".into();
        heartbeat.install(foreign, "live-b-1").unwrap();
        assert!(heartbeat.pump(&manager, &operation, false, &port).is_err());
        assert!(port.frames.lock().is_empty());
        heartbeat.install(lease, "live-b-1").unwrap();
        assert_eq!(
            manager.poll_watchdog(0).unwrap().kind(),
            super::super::controlled::ControlledStatusKind::Connected
        );
        manager.poll_watchdog(15_000).unwrap();
        heartbeat.pump(&manager, &operation, false, &port).unwrap();
        assert_eq!(
            port.frames.lock().len(),
            1,
            "liveness-suspect permits the exact current socket's evidence"
        );
        assert_eq!(
            manager.poll_watchdog(30_000).unwrap().kind(),
            super::super::controlled::ControlledStatusKind::Error,
            "a B heartbeat at 15s cannot extend the original 30s peer-A budget"
        );
    }

    #[test]
    fn production_poll_and_authenticated_cycle_installation_are_wired() {
        let source = include_str!("relay_runtime.rs");
        let serve = source
            .split("async fn serve(mut self)")
            .nth(1)
            .unwrap()
            .split("fn pump_liveness_evidence")
            .next()
            .unwrap();
        let poll = serve
            .split("_ = poll.tick() =>")
            .nth(1)
            .unwrap()
            .split("message = self.channel.recv()")
            .next()
            .unwrap();
        assert!(poll.contains("if joined && !resume.is_active()"));
        assert!(poll.contains("heartbeat.pump("));
        assert!(poll.contains("&self.operation, resume.is_active(), &self.channel"));
        assert_eq!(
            serve.matches("heartbeat.install(lease, epoch)").count(),
            2,
            "joined and final resume must both install the verified B epoch"
        );
        let fixture = include_str!(
            "../../../../../../remote-control-relay/test/fixtures/peer-liveness-router.js"
        );
        assert!(!fixture.contains("healthy_b"));
        assert!(!fixture.contains("++bSequence"));
    }
}

impl ControlledHeartbeat {
    /// Called only after the joined/final-resumed authentication gate succeeds.
    /// The final B epoch must also match the post-activation evidence we sent.
    pub(crate) fn install(&mut self, lease: LeaseContext, epoch: &str) -> Result<(), &'static str> {
        if !super::protocol::is_valid_identifier(epoch) {
            return Err("controller_handshake_failed");
        }
        if self.lease.as_ref() == Some(&lease) && self.epoch.as_deref() == Some(epoch) {
            return Ok(());
        }
        self.lease = Some(lease);
        self.epoch = Some(epoch.to_owned());
        self.sequence = 0;
        Ok(())
    }

    /// Shared by the production poll and integration tests. Hold the manager's
    /// exact operation/lease admission through the original channel enqueue.
    pub(crate) fn pump<P: ControlledRelayPort + ?Sized>(
        &mut self,
        manager: &ControlledDeviceManager,
        operation: &ControlledOperation,
        resume_active: bool,
        port: &P,
    ) -> Result<(), String> {
        if resume_active {
            return Ok(());
        }
        let Some(lease) = self.lease.clone() else {
            return Ok(());
        };
        manager
            .with_owned_liveness_lease(operation, &lease, || {
                let epoch = self
                    .epoch
                    .as_deref()
                    .ok_or("controller_liveness_epoch_missing")?;
                let sequence = self
                    .sequence
                    .checked_add(1)
                    .ok_or("controller_liveness_sequence_exhausted")?;
                let frame = super::liveness::build_controller_liveness(
                    &lease,
                    SenderRole::ControlledB,
                    epoch,
                    sequence,
                )
                .map_err(|error| error.code.to_owned())?;
                port.try_send(frame)?;
                self.sequence = sequence;
                Ok(())
            })
            .map_err(|error| error.code.to_owned())?
    }
}
