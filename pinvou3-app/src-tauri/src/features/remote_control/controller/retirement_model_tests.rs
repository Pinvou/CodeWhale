//! App-level retirement tests using the actual temporary model store.
//! The lightweight state/auth harness retains its existing manager tests;
//! model adapters and credential DTOs remain in the actual App target.

use super::controlled::tests::{enter_handshake, lease, register};
use super::controlled::{
    ControlledCleanupScope, ControlledCleanupSink, ControlledDeviceManager, ControlledStatusKind,
};
use super::protocol::LeaseContext;
use parking_lot::Mutex;
use std::sync::{Arc, atomic::Ordering};

fn pending_permission_for_test(
    authority: &super::permission::PermissionAuthority,
    context: &LeaseContext,
    task_id: &str,
) -> String {
    authority
        .request(
            context,
            task_id,
            super::permission::PermissionRequest {
                operation_type: "file_write".into(),
                risk_level: "medium".into(),
                target_scope: "workspace/report.md".into(),
                preview: "write report".into(),
                expires_at_ms: 60_000,
            },
            1_000,
        )
        .unwrap()
        .permission_request_id
}

fn install_model_for_retirement_test(
    models: &super::temporary_model::TemporaryModelStore,
    context: &LeaseContext,
    task_id: &str,
) -> serde_json::Value {
    use super::temporary_model::TemporaryModelPackageV1;
    let wire = serde_json::json!({
        "schema": "temporary_model_package/v1", "package_id": "package-retirement-test",
        "binding": {"connection_id": context.connection_id,
            "controller_lease_id": context.controller_lease_id,
            "lease_epoch": context.lease_epoch, "relay_epoch": context.relay_epoch,
            "remote_control_generation": context.remote_control_generation, "task_id": task_id},
        "primary": {"display_name":"Retirement fixture", "preset":"openai",
            "wire_api":"openai_chat_completions", "base_url":"https://api.openai.com/v1",
            "model":"fixture-model", "image_capability_override":"pinvou"},
        "primary_credential": {"kind":"api_key", "secret":"fixture-not-a-real-secret"}
    });
    let package = TemporaryModelPackageV1::from_wire_value(&wire).unwrap();
    models.install(package, context, task_id, 1_000).unwrap();
    wire
}

#[test]
fn retirement_clears_models_even_when_the_task_ledger_cannot_persist() {
    use super::task::{
        RemoteTaskLedger, RemoteTaskLedgerStore, RemoteTaskRecord, RemoteTaskRequest,
        RemoteTaskService, task_input_digest_for_value,
    };
    struct SwitchStore(std::sync::atomic::AtomicBool);
    impl RemoteTaskLedgerStore for SwitchStore {
        fn persist(&self, _: &[RemoteTaskRecord]) -> std::result::Result<(), String> {
            if self.0.load(Ordering::SeqCst) {
                Err("fixture-write-failed".into())
            } else {
                Ok(())
            }
        }
    }
    struct TaskCleanup(Arc<RemoteTaskService>);
    impl ControlledCleanupSink for TaskCleanup {
        fn cleanup(&self, generation: &str, scope: ControlledCleanupScope) {
            if scope.remote_tasks {
                self.0.interrupt_generation_and_clear_sessions(
                    generation,
                    "remote_control_stopped",
                    2_000,
                );
            }
        }
    }
    let manager = ControlledDeviceManager::default();
    let writes = Arc::new(SwitchStore(std::sync::atomic::AtomicBool::new(false)));
    let ledger = Arc::new(RemoteTaskLedger::new(writes.clone()));
    let service = Arc::new(RemoteTaskService::new(ledger.clone()));
    // Same order as the composition root: task retirement first, then the
    // independent model sink. Actual ledger/store, test-only owner wiring.
    manager.register_cleanup_sink(Arc::new(TaskCleanup(service.clone())));
    let models = Arc::new(super::temporary_model::TemporaryModelStore::default());
    service.hooks().set_model_scope(models.clone());
    models.register_controlled_cleanup(&manager);
    let operation = manager.start(None, Vec::new()).unwrap();
    register(&manager, &operation);
    enter_handshake(&manager, &operation);
    manager
        .complete_connected(operation.clone(), lease(operation.generation()))
        .unwrap();
    let context = manager.live_lease().unwrap();
    let input = serde_json::json!({"text":"fixture", "attachment_ids":[], "skill_ids":[]});
    let package = serde_json::json!({
        "schema":"temporary_model_package/v1", "package_id":"package-ledger-failure",
        "binding":{"connection_id":context.connection_id, "controller_lease_id":context.controller_lease_id,
            "lease_epoch":context.lease_epoch, "relay_epoch":context.relay_epoch,
            "remote_control_generation":context.remote_control_generation, "task_id":"retirement-model-task"},
        "primary":{"display_name":"Retirement fixture", "preset":"openai", "wire_api":"openai_chat_completions",
            "base_url":"https://api.openai.com/v1", "model":"fixture-model", "image_capability_override":"pinvou"},
        "primary_credential":{"kind":"api_key", "secret":"fixture-not-a-real-secret"}
    });
    let body = serde_json::json!({"v":1, "type":"task_submit", "connection_id":context.connection_id,
        "controller_lease_id":context.controller_lease_id, "lease_epoch":context.lease_epoch,
        "relay_epoch":context.relay_epoch, "remote_control_generation":context.remote_control_generation,
        "request_id":"request-retirement", "task_id":"retirement-model-task", "client_request_id":"client-retirement",
        "session_id":"session-retirement", "capability_revision":"1", "model_source":"controller_temporary", "mode":"chat",
        "input_digest":task_input_digest_for_value(&input).unwrap(), "input":input, "temporary_model_package":package});
    ledger
        .admit(
            &context,
            &RemoteTaskRequest::from_payload(&body).unwrap(),
            1_000,
        )
        .unwrap();
    let installed = super::temporary_model::TemporaryModelPackageV1::from_wire_value(
        &body["temporary_model_package"],
    )
    .unwrap();
    models
        .install(installed, &context, "retirement-model-task", 1_000)
        .unwrap();
    let before = ledger.record("retirement-model-task").unwrap();
    writes.0.store(true, Ordering::SeqCst);
    manager.stop();
    assert!(
        models.is_empty(),
        "failed ledger publication cannot retain transient model credentials"
    );
    let unchanged = ledger.record("retirement-model-task").unwrap();
    assert_eq!(
        unchanged.state, before.state,
        "do not invent a successful persisted terminal transition"
    );
    assert_eq!(unchanged.transition_sequence, before.transition_sequence);
    manager.stop();
    writes.0.store(false, Ordering::SeqCst);
    let report =
        service.interrupt_generation(operation.generation(), "remote_control_stopped", 3_000);
    assert!(report.failed.is_empty());
    assert_eq!(report.committed.len(), 1);
    assert_eq!(report.committed[0].task_id, "retirement-model-task");
    assert!(
        ledger
            .record("retirement-model-task")
            .unwrap()
            .is_terminal()
    );
    assert!(models.is_empty());
}

#[test]
fn retirement_denies_pre_admission_permissions_but_keeps_resume_and_dispatch_history() {
    use super::permission::{DecisionAuthority, PermissionAuthority, PermissionDecision};
    for terminal in ["stop", "remote_disconnect", "watchdog"] {
        let manager = ControlledDeviceManager::default();
        let authority = Arc::new(PermissionAuthority::with_clock(Arc::new(|| 1_000)));
        authority.register_controlled_cleanup(&manager);
        let models = Arc::new(super::temporary_model::TemporaryModelStore::default());
        models.register_controlled_cleanup(&manager);
        let operation = manager.start(None, Vec::new()).unwrap();
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
        manager
            .complete_connected(operation.clone(), lease(operation.generation()))
            .unwrap();
        let context = manager.live_lease().unwrap();
        // No task service admission exists for this pending record.
        let pending = pending_permission_for_test(&authority, &context, "pre-admission-task");
        let mut other = context.clone();
        other.remote_control_generation.push_str("-other");
        install_model_for_retirement_test(&models, &context, "old-model-task");
        install_model_for_retirement_test(&models, &other, "other-model-task");
        let unrelated = pending_permission_for_test(&authority, &other, "other-task");
        let committed = pending_permission_for_test(&authority, &context, "committed-task");
        let awaiting = pending_permission_for_test(&authority, &context, "awaiting-dispatch-task");
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &context,
                "awaiting-dispatch-task",
                &awaiting,
                PermissionDecision::Allow,
                1_000,
            )
            .unwrap();
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &context,
                "committed-task",
                &committed,
                PermissionDecision::Allow,
                1_000,
            )
            .unwrap();
        authority.commit_dispatch(&committed, 1_000).unwrap();
        let committed_before = serde_json::to_value(authority.view(&committed).unwrap()).unwrap();
        manager.mark_transport_disconnected(4_000);
        assert_eq!(authority.view(&pending).unwrap().state, "pending");
        assert!(
            models.contains_task("old-model-task"),
            "legal reconnecting preserves materials"
        );
        match terminal {
            "stop" => {
                manager.stop();
            }
            "remote_disconnect" => {
                manager
                    .accept_remote_disconnect_for(&operation, &context)
                    .unwrap();
            }
            _ => {
                let deadline = manager.watchdog_deadline_for_test().unwrap();
                let status = manager.poll_watchdog(deadline).unwrap();
                assert_eq!(status.kind(), ControlledStatusKind::Error);
                assert_eq!(
                    serde_json::to_value(&status).unwrap()["errorCode"].as_str(),
                    Some("desktop_remote_watchdog_expired")
                );
            }
        }
        assert_eq!(
            authority.view(&pending).unwrap().state,
            "denied",
            "{terminal}"
        );
        assert_eq!(authority.view(&unrelated).unwrap().state, "pending");
        assert!(!models.contains_task("old-model-task"), "{terminal}");
        assert!(models.contains_task("other-model-task"));
        assert_eq!(authority.view(&awaiting).unwrap().state, "denied");
        assert_eq!(
            serde_json::to_value(authority.view(&committed).unwrap()).unwrap(),
            committed_before
        );
        manager.stop();
        assert_eq!(authority.view(&pending).unwrap().state, "denied");
        assert!(models.contains_task("other-model-task"));
    }
}

#[test]
fn delayed_permission_retirement_keeps_a_successor_with_reused_connection_fields() {
    use super::permission::PermissionAuthority;
    struct PausedCleanup {
        old: String,
        entered: std::sync::mpsc::Sender<()>,
        release: Mutex<std::sync::mpsc::Receiver<()>>,
    }
    impl ControlledCleanupSink for PausedCleanup {
        fn cleanup(&self, generation: &str, _: ControlledCleanupScope) {
            // A failed fixture must not panic again from manager Drop or
            // wait for a test orchestrator that is already unwinding.
            if generation == self.old && !std::thread::panicking() {
                self.entered.send(()).unwrap();
                self.release
                    .lock()
                    .recv_timeout(std::time::Duration::from_secs(5))
                    .unwrap();
            }
        }
    }
    let manager = Arc::new(ControlledDeviceManager::default());
    let old = manager.start(None, Vec::new()).unwrap();
    register(&manager, &old);
    enter_handshake(&manager, &old);
    manager
        .complete_connected(old.clone(), lease(old.generation()))
        .unwrap();
    let context = manager.live_lease().unwrap();
    let (entered_tx, entered_rx) = std::sync::mpsc::channel();
    let (release_tx, release_rx) = std::sync::mpsc::channel();
    manager.register_cleanup_sink(Arc::new(PausedCleanup {
        old: old.generation().into(),
        entered: entered_tx,
        release: Mutex::new(release_rx),
    }));
    let authority = Arc::new(PermissionAuthority::with_clock(Arc::new(|| 1_000)));
    authority.register_controlled_cleanup(&manager);
    let pending = pending_permission_for_test(&authority, &context, "old-unadmitted-task");
    let models = Arc::new(super::temporary_model::TemporaryModelStore::default());
    models.register_controlled_cleanup(&manager);
    install_model_for_retirement_test(&models, &context, "reused-model-task");
    let retiring = manager.clone();
    let thread = std::thread::spawn(move || retiring.accept_remote_disconnect_for(&old, &context));
    entered_rx
        .recv_timeout(std::time::Duration::from_secs(2))
        .unwrap();
    let successor = manager.start(None, Vec::new()).unwrap();
    register(&manager, &successor);
    enter_handshake(&manager, &successor);
    manager
        .complete_connected(successor.clone(), lease(successor.generation()))
        .unwrap();
    let fresh = pending_permission_for_test(
        &authority,
        &manager.live_lease().unwrap(),
        "new-unadmitted-task",
    );
    let successor_context = manager.live_lease().unwrap();
    install_model_for_retirement_test(&models, &successor_context, "reused-model-task");
    release_tx.send(()).unwrap();
    thread.join().unwrap().unwrap();
    assert_eq!(authority.view(&pending).unwrap().state, "denied");
    assert_eq!(authority.view(&fresh).unwrap().state, "pending");
    assert!(
        models
            .with_credentials("reused-model-task", &successor_context, |_, _| ())
            .is_ok()
    );
    assert_eq!(
        manager.current_generation().as_deref(),
        Some(successor.generation())
    );
}
