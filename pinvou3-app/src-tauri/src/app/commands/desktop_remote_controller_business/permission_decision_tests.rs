use super::*;
use crate::features::remote_control::controller::{
    controlled::{ControlledDeviceManager, ControlledLease, ControlledRegistration},
    permission::PermissionRequest,
    relay_transport::{AuthenticatedLease, ControllerRole, transport_channels},
    task::{RemoteSessionProvisioner, RemoteTaskLedger, RemoteTaskRequest, RemoteTaskTerminal},
};
use std::sync::{
    Arc,
    atomic::{AtomicU64, AtomicUsize, Ordering},
};

struct Sessions;
impl RemoteSessionProvisioner for Sessions {
    fn create_authoritative_session(&self, _: &str, _: bool) -> Result<String, String> {
        Ok("b-session-permission".into())
    }
    fn session_exists(&self, _: &str) -> Result<bool, String> {
        Ok(true)
    }
}

struct Fixture {
    controlled: ControlledDeviceManager,
    tasks: RemoteTaskService,
    authority: Arc<PermissionAuthority>,
    lease: LeaseContext,
    body: Value,
    now: Arc<AtomicU64>,
}

impl Fixture {
    fn new(decision: &str) -> Self {
        let controlled = ControlledDeviceManager::default();
        let operation = controlled.start(None, Vec::new()).unwrap();
        controlled
            .complete_registration(
                operation.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: 30_000,
                },
                1_000,
            )
            .unwrap();
        controlled.mark_pairing_reserved(operation.clone()).unwrap();
        controlled
            .mark_handshake_pending(operation.clone())
            .unwrap();
        let lease = LeaseContext {
            connection_id: "connection-permission".into(),
            controller_lease_id: "lease-permission".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-permission".into(),
            remote_control_generation: operation.generation().into(),
        };
        controlled
            .complete_connected(
                operation.clone(),
                ControlledLease {
                    context: lease.clone(),
                    liveness_epoch_a: "live-a-permission".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [1; 32],
                    transport_key_fingerprint: [2; 32],
                    controller_device_name: "Controller".into(),
                    connected_at_b: 3_000,
                    negotiated_capabilities: vec!["task_v1".into(), "permission_v1".into()],
                },
            )
            .unwrap();
        // Actual transport authority gate with a controlled authenticated lease;
        // this fixture does not claim socket/cryptographic-handshake coverage.
        let (transport, _receiver) = transport_channels(
            ControllerRole::ControlledB,
            operation.generation(),
            4,
            4,
            4096,
            512,
        );
        transport
            .install_authenticated_lease(AuthenticatedLease {
                context: lease.clone(),
                negotiated_capabilities: vec!["task_v1".into(), "permission_v1".into()],
            })
            .unwrap();
        controlled
            .replace_authenticated_transport(operation, transport)
            .unwrap();
        let tasks = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        Self::bind_session(&tasks, &lease);
        let input = json!({"text":"approval fixture", "attachment_ids":[], "skill_ids":[], "workspace_id":"workspace-permission"});
        let request = RemoteTaskRequest::from_payload(&json!({
            "connection_id":lease.connection_id, "controller_lease_id":lease.controller_lease_id,
            "lease_epoch":lease.lease_epoch, "relay_epoch":lease.relay_epoch,
            "remote_control_generation":lease.remote_control_generation, "task_id":"task-permission",
            "client_request_id":"client-permission", "session_id":"b-session-permission", "capability_revision":"1",
            "mode":"code", "model_source":"controlled_device",
            "input_digest":crate::features::remote_control::controller::task::task_input_digest_for_value(&input).unwrap(), "input":input,
        })).unwrap();
        tasks.ledger().admit(&lease, &request, 3_100).unwrap();
        tasks
            .ledger()
            .begin_dispatch("task-permission", 3_200)
            .unwrap();
        let now = Arc::new(AtomicU64::new(3_300));
        let clock = now.clone();
        let authority = Arc::new(PermissionAuthority::with_clock(Arc::new(move || {
            clock.load(Ordering::SeqCst)
        })));
        let approval = authority
            .request(
                &lease,
                "task-permission",
                PermissionRequest {
                    operation_type: "tool_call".into(),
                    risk_level: "medium".into(),
                    target_scope: "workspace-permission".into(),
                    preview: "controlled permission fixture".into(),
                    expires_at_ms: 30_000,
                },
                3_250,
            )
            .unwrap();
        let body = json!({"task_id":"task-permission", "permission_request_id":approval.permission_request_id, "decision":decision});
        Self {
            controlled,
            tasks,
            authority,
            lease,
            body,
            now,
        }
    }
    fn bind_session(tasks: &RemoteTaskService, lease: &LeaseContext) {
        tasks.set_session_provisioner(Arc::new(Sessions));
        tasks
            .ensure_remote_session(
                "a-ref-permission",
                "Remote",
                true,
                &lease.remote_control_generation,
            )
            .unwrap();
    }
    fn handler(&self) -> PermissionDecisionHandler<'_> {
        PermissionDecisionHandler {
            controlled: &self.controlled,
            tasks: &self.tasks,
            authority: &self.authority,
        }
    }
    fn approval(&self) -> &str {
        self.body["permission_request_id"].as_str().unwrap()
    }
    fn finish(&self, terminal: RemoteTaskTerminal) {
        self.tasks
            .ledger()
            .complete("task-permission", terminal, None, 3_400)
            .unwrap();
        self.authority
            .deny_all_for_task("task-permission", "task_terminal", 3_400);
    }
}

#[tokio::test]
async fn committed_allow_survives_terminal_during_the_real_handler_await_and_never_redispatches() {
    let fixture = Fixture::new("allow");
    let entered = Arc::new(tokio::sync::Notify::new());
    let release = Arc::new(tokio::sync::Notify::new());
    let calls = Arc::new(AtomicUsize::new(0));
    let handler = fixture.handler();
    let response = handler.handle(&fixture.lease, "a-ref-permission", &fixture.body, {
        let entered = entered.clone();
        let release = release.clone();
        let calls = calls.clone();
        move |_, _, decision| async move {
            assert_eq!(decision, PermissionDecision::Allow);
            calls.fetch_add(1, Ordering::SeqCst);
            entered.notify_one();
            release.notified().await;
            Ok(())
        }
    });
    tokio::pin!(response);
    tokio::select! { _=entered.notified()=>{}, result=&mut response=>panic!("response completed before ACP barrier: {result:?}") }
    fixture.finish(RemoteTaskTerminal::Succeeded);
    let retained = fixture.authority.view(fixture.approval()).unwrap();
    release.notify_one();
    assert_eq!(response.await.unwrap(), json!({"state":"allowed"}));
    let replay = fixture
        .handler()
        .handle(
            &fixture.lease,
            "a-ref-permission",
            &fixture.body,
            |_, _, _| async { panic!("committed retry must not resend ACP") },
        )
        .await
        .unwrap();
    assert_eq!(replay, json!({"state":"allowed"}));
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert_eq!(
        fixture.authority.view(fixture.approval()).unwrap(),
        retained
    );
}

#[tokio::test]
async fn concurrent_same_decision_reads_commit_while_only_one_acp_response_is_pending() {
    let fixture = Fixture::new("allow");
    let entered = Arc::new(tokio::sync::Notify::new());
    let release = Arc::new(tokio::sync::Notify::new());
    let handler = fixture.handler();
    let first = handler.handle(&fixture.lease, "a-ref-permission", &fixture.body, {
        let entered = entered.clone();
        let release = release.clone();
        move |_, _, _| async move {
            entered.notify_one();
            release.notified().await;
            Ok(())
        }
    });
    tokio::pin!(first);
    tokio::select! {_=entered.notified()=>{}, result=&mut first=>panic!("first ended before barrier: {result:?}")}
    let known = fixture
        .handler()
        .handle(
            &fixture.lease,
            "a-ref-permission",
            &fixture.body,
            |_, _, _| async { panic!("in-flight committed retry must not redispatch") },
        )
        .await
        .unwrap();
    assert_eq!(known, json!({"state":"allowed"}));
    release.notify_one();
    assert_eq!(first.await.unwrap(), known);
}

#[tokio::test]
async fn deny_and_expiry_receipts_remain_truthful_after_task_terminal() {
    for expired in [false, true] {
        let fixture = Fixture::new(if expired { "allow" } else { "deny" });
        if expired {
            fixture.now.store(30_000, Ordering::SeqCst);
        }
        let terminal_fixture = &fixture;
        let outcome = fixture
            .handler()
            .handle(
                &fixture.lease,
                "a-ref-permission",
                &fixture.body,
                move |_, _, effective| async move {
                    assert_eq!(effective, PermissionDecision::Deny);
                    terminal_fixture.finish(RemoteTaskTerminal::Cancelled);
                    Ok(())
                },
            )
            .await
            .unwrap();
        assert_eq!(
            outcome,
            json!({"state":if expired{"expired"}else{"denied"}})
        );
        assert_eq!(
            fixture
                .handler()
                .handle(
                    &fixture.lease,
                    "a-ref-permission",
                    &fixture.body,
                    |_, _, _| async { panic!("terminal receipt must not resend") }
                )
                .await
                .unwrap(),
            outcome
        );
    }
}

#[tokio::test]
async fn first_acp_error_is_preserved_but_committed_retry_does_not_dispatch_again() {
    let fixture = Fixture::new("allow");
    assert_eq!(
        fixture
            .handler()
            .handle(
                &fixture.lease,
                "a-ref-permission",
                &fixture.body,
                |_, _, _| async { Err("permission_dispatch_failed".to_owned()) }
            )
            .await
            .unwrap_err(),
        "permission_dispatch_failed"
    );
    fixture.finish(RemoteTaskTerminal::Failed);
    assert_eq!(
        fixture
            .handler()
            .handle(
                &fixture.lease,
                "a-ref-permission",
                &fixture.body,
                |_, _, _| async { panic!("receipt read must not retry an unknown ACP operation") }
            )
            .await
            .unwrap(),
        json!({"state":"allowed"})
    );
}

#[tokio::test]
async fn a_terminal_task_without_a_committed_receipt_never_obtains_a_new_allow() {
    let fixture = Fixture::new("allow");
    fixture.finish(RemoteTaskTerminal::Cancelled);
    let before = fixture.authority.view(fixture.approval()).unwrap();
    assert_eq!(
        fixture
            .handler()
            .handle(
                &fixture.lease,
                "a-ref-permission",
                &fixture.body,
                |_, _, _| async { panic!("terminal first request cannot dispatch") }
            )
            .await
            .unwrap_err(),
        "task_session_binding_mismatch"
    );
    assert_eq!(fixture.authority.view(fixture.approval()).unwrap(), before);
}

#[tokio::test]
async fn conflicting_decision_and_retired_connection_cannot_read_or_change_a_committed_result() {
    let fixture = Fixture::new("allow");
    fixture
        .handler()
        .handle(
            &fixture.lease,
            "a-ref-permission",
            &fixture.body,
            |_, _, _| async { Ok(()) },
        )
        .await
        .unwrap();
    fixture.finish(RemoteTaskTerminal::Succeeded);
    let before = fixture.authority.view(fixture.approval()).unwrap();
    let mut conflict = fixture.body.clone();
    conflict["decision"] = json!("deny");
    assert_eq!(
        fixture
            .handler()
            .handle(
                &fixture.lease,
                "a-ref-permission",
                &conflict,
                |_, _, _| async { panic!("conflicting decision cannot dispatch") }
            )
            .await
            .unwrap_err(),
        "permission_already_decided"
    );
    assert_eq!(fixture.authority.view(fixture.approval()).unwrap(), before);
    fixture.controlled.stop();
    assert_eq!(
        fixture
            .handler()
            .handle(
                &fixture.lease,
                "a-ref-permission",
                &fixture.body,
                |_, _, _| async { panic!("retired connection cannot dispatch") }
            )
            .await
            .unwrap_err(),
        "desktop_remote_not_connected"
    );
    assert_eq!(fixture.authority.view(fixture.approval()).unwrap(), before);
}

#[tokio::test]
async fn a_new_task_sequence_with_reused_visible_ids_cannot_read_the_old_receipt() {
    let fixture = Fixture::new("allow");
    fixture
        .handler()
        .handle(
            &fixture.lease,
            "a-ref-permission",
            &fixture.body,
            |_, _, _| async { Ok(()) },
        )
        .await
        .unwrap();
    fixture.finish(RemoteTaskTerminal::Succeeded);
    // A restored ledger with a new admission is an exact deterministic seam
    // for the post-pruning owner. The actual receipt's frozen owner is retained.
    let mut replacement = fixture.tasks.ledger().record("task-permission").unwrap();
    replacement.sequence += 1;
    let ledger = RemoteTaskLedger::restore(
        Arc::new(crate::features::remote_control::controller::task::InMemoryTaskLedgerStore),
        vec![replacement],
    )
    .unwrap();
    let tasks = RemoteTaskService::new(Arc::new(ledger));
    Fixture::bind_session(&tasks, &fixture.lease);
    let handler = PermissionDecisionHandler {
        controlled: &fixture.controlled,
        tasks: &tasks,
        authority: &fixture.authority,
    };
    assert_eq!(
        handler
            .handle(
                &fixture.lease,
                "a-ref-permission",
                &fixture.body,
                |_, _, _| async { panic!("new task owner cannot dispatch an old receipt") }
            )
            .await
            .unwrap_err(),
        "permission_binding_stale"
    );
}
