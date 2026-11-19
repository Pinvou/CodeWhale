//! Real loopback socket loss through RelayClient, manager, worker and app cleanup.
use super::*;
use crate::features::remote_control::controller::controlled::{
    ControlledCleanupScope, ControlledCleanupSink,
};
use crate::features::remote_control::controller::resume::ResumePhase;

// Observation only: the app-owned production adapter performs real cleanup.
#[derive(Default)]
struct CleanupObserver(std::sync::Mutex<Vec<(String, ControlledCleanupScope)>>);

impl ControlledCleanupSink for CleanupObserver {
    fn cleanup(&self, generation: &str, scope: ControlledCleanupScope) {
        self.0.lock().unwrap().push((generation.to_owned(), scope));
    }
}

pub(super) async fn wire_notice_is_not_local_authority(
    manager: &ControlledDeviceManager,
    relay: &RemoteControlManager,
    socket: &mut Socket,
    completion: &mut tokio::sync::mpsc::Receiver<Value>,
) {
    let phase = manager.resume_phase_for_test();
    let status = manager.status().kind();
    let deadline = manager.watchdog_deadline_for_test();
    let epoch = manager.retained_liveness_epoch_for_test();
    assert!(deadline.is_some());
    let forged = json!({"type":"desktop_relay_connection", "connected":false,
        "endpoint_id":relay.controller_endpoint_id().unwrap()});
    send(socket, &forged).await;
    // FIFO barrier uses an actual, valid, unrelated Relay error. A wrong
    // notification reaching the worker before it would mutate real state.
    let barrier = json!({"v":1,"type":"error","request_id":"request-notice-barrier",
        "code":"invalid_message","message":"rejected"});
    send(socket, &barrier).await;
    processed(completion, &barrier).await;
    assert_eq!(
        manager.status().kind(),
        status,
        "wire JSON impersonated local transport loss"
    );
    assert_eq!(manager.resume_phase_for_test(), phase);
    assert_eq!(manager.watchdog_deadline_for_test(), deadline);
    assert_eq!(manager.retained_liveness_epoch_for_test(), epoch);
}

pub(super) async fn end_current_candidate(
    manager: &ControlledDeviceManager,
    lane: &ControlledLaneState,
    outbound: &ControlledOutboundState,
    owned: &mut OwnedLane,
    completion: &mut tokio::sync::mpsc::Receiver<Value>,
    mut socket: Socket,
    expected: (u64, ResumePhase),
) {
    let (frozen, phase) = expected;
    assert_eq!(manager.resume_phase_for_test(), Some(phase));
    assert_eq!(manager.watchdog_deadline_for_test(), Some(frozen));
    assert!(lane.is_installed() && lane.is_suspended());
    assert!(
        frozen > controlled_monotonic_millis() + 3_000,
        "fixture must distinguish immediate loss from watchdog expiry"
    );
    wire_notice_is_not_local_authority(manager, &owned.relay, &mut socket, completion).await;
    let observation = observe_terminal_loss(manager);
    // Only the real socket close produces the authority-bearing local notice.
    socket.close(None).await.unwrap();
    drop(socket);
    assert_terminal_loss(manager, lane, outbound, owned, &observation).await;
    // Keep the existing three-stage test's next-intent assertion unchanged.
    let next = manager.start(None, Vec::new()).unwrap();
    let next_channel = owned
        .relay
        .start_controller_relay(next.generation())
        .await
        .unwrap();
    drop(next_channel);
    manager.stop();
}

// Register before accepting the fault-producing socket, not after its local
// false can have committed cleanup. Keep this strong reference through join.
pub(super) struct TerminalLossObservation {
    observer: std::sync::Arc<CleanupObserver>,
    generation: String,
}

pub(super) fn observe_terminal_loss(manager: &ControlledDeviceManager) -> TerminalLossObservation {
    let observer = std::sync::Arc::new(CleanupObserver::default());
    manager.register_cleanup_sink(observer.clone());
    TerminalLossObservation {
        observer,
        generation: manager.current_operation().unwrap().generation().to_owned(),
    }
}

pub(super) async fn assert_terminal_loss(
    manager: &ControlledDeviceManager,
    lane: &ControlledLaneState,
    outbound: &ControlledOutboundState,
    owned: &mut OwnedLane,
    observation: &TerminalLossObservation,
) {
    let mut worker = owned.worker.take().unwrap();
    let completed = tokio::time::timeout(Duration::from_secs(3), &mut worker).await;
    if completed.is_err() {
        worker.abort();
        let _ = worker.await;
        panic!("second transport loss waited for watchdog instead of ending candidate");
    }
    completed.unwrap().unwrap();
    assert_eq!(manager.status().kind(), ControlledStatusKind::Error);
    assert_eq!(
        serde_json::to_value(manager.status()).unwrap()["errorCode"],
        "desktop_remote_resume_rejected"
    );
    assert!(manager.current_operation().is_none());
    assert!(manager.resume_phase_for_test().is_none());
    assert!(manager.retained_liveness_epoch_for_test().is_none());
    assert!(manager.watchdog_deadline_for_test().is_none());
    assert!(manager.live_lease().is_err());
    assert!(
        !lane.is_installed(),
        "terminal loss retained the actual session cipher"
    );
    assert!(outbound.sender().is_none());
    let calls = observation.observer.0.lock().unwrap().clone();
    assert_eq!(
        calls.len(),
        1,
        "terminal loss cleaned the candidate more than once"
    );
    assert_eq!(calls[0].0, observation.generation);
    let scope = calls[0].1;
    assert!(
        scope.session_keys
            && scope.remote_tasks
            && scope.pending_permissions
            && scope.temporary_models
            && scope.workspace_grants
    );
}
