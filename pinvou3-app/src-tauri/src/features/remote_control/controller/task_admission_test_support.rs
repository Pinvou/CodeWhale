use std::sync::Arc;

use super::controlled::ControlledDeviceManager;
use super::protocol::LeaseContext;
use super::task::RemoteTaskService;

pub(crate) fn task_admission_fixture() -> (
    Arc<ControlledDeviceManager>,
    Arc<RemoteTaskService>,
    LeaseContext,
    serde_json::Value,
) {
    use super::task::{RemoteSessionProvisioner, RemoteTaskLedger};
    struct Sessions;
    impl RemoteSessionProvisioner for Sessions {
        fn create_authoritative_session(
            &self,
            _: &str,
            _: bool,
        ) -> std::result::Result<String, String> {
            Ok("b-session-admission".into())
        }
        fn session_exists(&self, _: &str) -> std::result::Result<bool, String> {
            Ok(true)
        }
    }
    let manager = Arc::new(ControlledDeviceManager::default());
    let operation = manager.start(None, Vec::new()).unwrap();
    manager
        .complete_registration(
            operation.clone(),
            crate::features::remote_control::controller::controlled::ControlledRegistration {
                pairing_code: "2345678".into(),
                sealed_code_b64: "sealed".into(),
                expires_at_ms: 30_000,
            },
            1_000,
        )
        .unwrap();
    manager.mark_pairing_reserved(operation.clone()).unwrap();
    manager.mark_handshake_pending(operation.clone()).unwrap();
    manager
        .complete_connected(
            operation.clone(),
            crate::features::remote_control::controller::controlled::ControlledLease {
                context: LeaseContext {
                    connection_id: "connection-0001".into(),
                    controller_lease_id: "lease-0001".into(),
                    lease_epoch: "1".into(),
                    relay_epoch: "relay-epoch-0001".into(),
                    remote_control_generation: operation.generation().to_owned(),
                },
                liveness_epoch_a: "live-a-1".into(),
                resume_deadline_b: 30_000,
                resume_token: [1; 32],
                transport_key_fingerprint: [2; 32],
                controller_device_name: "Controller".into(),
                connected_at_b: 3_000,
                negotiated_capabilities: vec!["task_v1".into()],
            },
        )
        .unwrap();
    let context = manager.live_lease().unwrap();
    // Exercise the real transport authority gate, not a Connected DTO
    // without a transport. This unit fixture does not claim socket proof.
    let (transport, _receiver) =
        crate::features::remote_control::controller::relay_transport::transport_channels(
            crate::features::remote_control::controller::relay_transport::ControllerRole::ControlledB,
            operation.generation(), 4, 4, 4096, 512,
        );
    transport
        .install_authenticated_lease(
            crate::features::remote_control::controller::relay_transport::AuthenticatedLease {
                context: context.clone(),
                negotiated_capabilities: vec!["task_v1".into()],
            },
        )
        .unwrap();
    manager
        .replace_authenticated_transport(operation.clone(), transport)
        .unwrap();
    let service = Arc::new(RemoteTaskService::new(Arc::new(
        RemoteTaskLedger::in_memory(),
    )));
    service.set_session_provisioner(Arc::new(Sessions));
    service
        .ensure_remote_session("a-ref-admission", "Chat", false, operation.generation())
        .unwrap();
    let payload = serde_json::json!({
        "connection_id": context.connection_id, "controller_lease_id": context.controller_lease_id,
        "lease_epoch": context.lease_epoch, "relay_epoch": context.relay_epoch,
        "remote_control_generation": context.remote_control_generation,
        "task_id": "task-admission", "client_request_id": "client-admission",
        "session_id": "b-session-admission", "capability_revision": "1",
        "mode": "chat", "model_source": "controlled_device", "input_digest": crate::features::sessions::remote_proxy::outbox::remote_task_input_digest("hello", &[], &[]),
        "input": {"text": "hello", "attachment_ids": [], "skill_ids": []}
    });
    (manager, service, context, payload)
}
