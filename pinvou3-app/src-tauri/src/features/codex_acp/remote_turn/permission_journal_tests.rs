use super::super::{
    CodexAcpPendingPermission, PendingPermission, RemotePermissionProjection,
    mint_remote_permission_in_maps, take_remote_permission_resolution,
};
use super::*;
use crate::features::remote_control::controller::{
    journal::{RemoteJournal, ReplayOutcome},
    permission::{
        DecisionAuthority, DispatchCommit, PermissionDecision, PermissionDecisionOwner,
        PermissionView,
    },
    task::{RemoteTaskLedger, RemoteTaskRequest, RemoteTaskTerminal, task_input_digest_for_value},
};
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use tokio::sync::{Mutex as AsyncMutex, oneshot};

struct JournalSink {
    journal: RemoteJournal,
    next: AtomicUsize,
}

impl JournalSink {
    fn new() -> Self {
        Self {
            journal: RemoteJournal::new("epoch-journal"),
            next: AtomicUsize::new(1),
        }
    }
    fn states(&self) -> Vec<String> {
        let ReplayOutcome::Events(events) = self
            .journal
            .replay_after_for("session-journal", "epoch-journal", 0)
            .unwrap()
        else {
            panic!("expected retained real journal events")
        };
        events
            .into_iter()
            .map(|event| {
                event.validate_exportable().unwrap();
                assert_eq!(event.session_id, "session-journal");
                assert_eq!(event.task_id.as_deref(), Some("task-journal"));
                event.payload["status"].as_str().unwrap().to_owned()
            })
            .collect()
    }
}

impl ControllerTaskEventSink for JournalSink {
    fn emit(&self, _: Value) {}
    fn emit_journal(&self, kind: JournalKind, session: &str, task: Option<&str>, payload: Value) {
        // Test-only issuer seam; the real journal encoding/export validation
        // runs here. Production retains AdmissionEventSink and JournalOwner.
        self.journal
            .append(
                &format!(
                    "permission-event-{}",
                    self.next.fetch_add(1, Ordering::SeqCst)
                ),
                kind,
                session,
                task,
                payload,
            )
            .unwrap();
    }
}

struct Fixture {
    owner: RemotePermissionOwner,
    request: RemoteTaskRequest,
    tasks: RemoteTaskService,
    authority: PermissionAuthority,
    pending: AsyncMutex<HashMap<String, PendingPermission>>,
    owners: Mutex<HashMap<String, RemotePermissionOwner>>,
    clock: Arc<AtomicU64>,
}

impl Fixture {
    fn new() -> Self {
        let lease = LeaseContext {
            connection_id: "connection-journal".into(),
            controller_lease_id: "lease-journal".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-journal".into(),
            remote_control_generation: "generation-journal".into(),
        };
        let input = json!({"text":"approval round", "attachment_ids":[], "skill_ids":[], "workspace_id":"workspace-journal"});
        let request = RemoteTaskRequest::from_payload(&json!({
            "connection_id":lease.connection_id, "controller_lease_id":lease.controller_lease_id,
            "lease_epoch":lease.lease_epoch, "relay_epoch":lease.relay_epoch,
            "remote_control_generation":lease.remote_control_generation,
            "task_id":"task-journal", "client_request_id":"client-journal", "session_id":"session-journal",
            "capability_revision":"1", "mode":"code", "model_source":"controlled_device",
            "input_digest":task_input_digest_for_value(&input).unwrap(), "input":input,
        })).unwrap();
        let tasks = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        tasks.ledger().admit(&lease, &request, 1).unwrap();
        let record = tasks.ledger().begin_dispatch("task-journal", 2).unwrap();
        let owner = RemotePermissionOwner {
            task_sequence: record.sequence,
            session_id: record.session_id,
            task_id: record.task_id,
            lease,
        };
        let clock = Arc::new(AtomicU64::new(1_000));
        let observed = clock.clone();
        let authority =
            PermissionAuthority::with_clock(Arc::new(move || observed.load(Ordering::SeqCst)));
        Self {
            owners: Mutex::new(HashMap::from([(owner.session_id.clone(), owner.clone())])),
            owner,
            request,
            tasks,
            authority,
            pending: AsyncMutex::new(HashMap::new()),
            clock,
        }
    }

    async fn mint(
        &self,
        callback: &str,
    ) -> (
        PermissionView,
        oneshot::Receiver<super::super::RequestPermissionResponse>,
    ) {
        let (response_tx, receiver) = oneshot::channel();
        self.pending.lock().await.insert(
            format!("key-{callback}"),
            PendingPermission {
                callback_instance: Arc::new(()),
                view: CodexAcpPendingPermission {
                    session_id: self.owner.session_id.clone(),
                    tool_call_id: format!("tool-{callback}"),
                    request: Value::Null,
                },
                option_ids: vec!["allow-once".into(), "reject-once".into()],
                option_kinds: HashMap::from([
                    ("allow-once".into(), "allow_once".into()),
                    ("reject-once".into(), "reject_once".into()),
                ]),
                remote_owner: Some(self.owner.clone()),
                remote_request_id: Some(callback.into()),
                remote_approval_id: None,
                remote_projection: Some(RemotePermissionProjection {
                    operation_type: "tool_call".into(),
                    risk_level: "medium".into(),
                    target_scope: "workspace-journal".into(),
                    preview: "approval journal fixture".into(),
                }),
                generation: 1,
                turn_id: Some("turn-journal".into()),
                activity: super::super::stall::new_activity_clock(),
                bridge: None,
                // Projection fixture: no SDK connection; the shared resolver
                // drives its controlled oneshot. SDK acceptance is tested separately.
                sdk_responder: None,
                response_tx,
            },
        );
        let view = mint_remote_permission_in_maps(
            &self.pending,
            &self.owners,
            &self.owner,
            callback,
            &self.authority,
            &self.tasks.ledger(),
            60_000,
        )
        .await
        .unwrap();
        (view, receiver)
    }

    fn decide(&self, id: &str, decision: PermissionDecision) {
        self.authority
            .decide_for_owner(
                DecisionAuthority::ControllerA,
                &self.owner.lease,
                &self.owner.task_id,
                id,
                decision,
                &PermissionDecisionOwner {
                    session_id: self.owner.session_id.clone(),
                    task_sequence: self.owner.task_sequence,
                },
            )
            .unwrap();
        if decision == PermissionDecision::Allow {
            assert_eq!(
                self.authority
                    .commit_dispatch(id, self.authority.now_ms())
                    .unwrap(),
                DispatchCommit::Committed
            );
        }
    }

    async fn respond(&self, id: &str, allow: bool) {
        // Runtime/turn activation is the controlled seam. The production
        // pending consumption, exact owner/approval match, option selection and
        // real oneshot response below are shared with AcpPool's real responder.
        let mut pending = self.pending.lock().await;
        let resolved =
            take_remote_permission_resolution(&mut pending, &self.owner, id, allow).unwrap();
        assert_eq!(resolved.request.turn_id.as_deref(), Some("turn-journal"));
        resolved
            .request
            .response_tx
            .send(resolved.response)
            .unwrap();
    }
}

#[tokio::test]
async fn fast_allow_and_deny_terminal_drain_registered_results_after_real_acp_removal_once() {
    for decision in [PermissionDecision::Allow, PermissionDecision::Deny] {
        let fixture = Fixture::new();
        let sink = JournalSink::new();
        let mut journal = PermissionJournal::new(fixture.owner.clone());
        let (view, receiver) = fixture.mint("callback-one").await;
        journal.register("callback-one", &view, &sink).unwrap();
        fixture.decide(&view.permission_request_id, decision);
        fixture
            .respond(
                &view.permission_request_id,
                decision == PermissionDecision::Allow,
            )
            .await;
        assert!(fixture.pending.lock().await.is_empty());
        let response = serde_json::to_value(receiver.await.unwrap()).unwrap();
        assert_eq!(
            response["outcome"]["optionId"],
            if decision == PermissionDecision::Allow {
                "allow-once"
            } else {
                "reject-once"
            }
        );
        let mut turn = AcpTurnRelayState {
            turn_id: Some("turn-journal".into()),
            ..AcpTurnRelayState::default()
        };
        let terminal = serde_json::from_value::<super::super::AcpEventEnvelope>(json!({
            "version":1, "sessionId":"session-journal", "turnId":"turn-journal", "seq":1,
            "timestamp":"2026-11-11T00:00:00Z", "event":{"type":"turn_completed", "data":{"status":"Completed"}},
        })).unwrap();
        project_acp_event(&mut turn, &terminal);
        assert!(
            turn.terminal.is_some(),
            "the real projector has already observed the fast terminal"
        );
        project_permission_result(
            Ok(()),
            &mut journal,
            &fixture.authority,
            &fixture.tasks,
            &sink,
        )
        .unwrap();
        assert_eq!(
            sink.states(),
            vec![
                "pending",
                if decision == PermissionDecision::Allow {
                    "allowed"
                } else {
                    "denied"
                }
            ]
        );
        project_permission_result(
            Ok(()),
            &mut journal,
            &fixture.authority,
            &fixture.tasks,
            &sink,
        )
        .unwrap();
        assert_eq!(
            sink.states().len(),
            2,
            "final drain cannot duplicate the decision"
        );
        fixture
            .tasks
            .ledger()
            .complete(
                &fixture.owner.task_id,
                RemoteTaskTerminal::Succeeded,
                None,
                3,
            )
            .unwrap();
        assert!(
            journal
                .project_all(&fixture.authority, &fixture.tasks, &sink)
                .is_err()
        );
        assert_eq!(
            sink.states().len(),
            2,
            "late old callback cannot append after task terminal"
        );
    }
}

#[tokio::test]
async fn a_fresh_pending_snapshot_projects_a_response_from_this_same_poll() {
    let fixture = Fixture::new();
    let sink = JournalSink::new();
    let mut journal = PermissionJournal::new(fixture.owner.clone());
    let (view, receiver) = fixture.mint("callback-one").await;
    journal.register("callback-one", &view, &sink).unwrap();
    let old = HashSet::from(["callback-one".to_owned()]);
    fixture.decide(&view.permission_request_id, PermissionDecision::Allow);
    fixture.respond(&view.permission_request_id, true).await;
    receiver.await.unwrap();
    assert_eq!(
        journal
            .project_missing(&fixture.authority, &fixture.tasks, &sink, &old)
            .unwrap(),
        0
    );
    assert!(fixture.pending.lock().await.is_empty());
    assert_eq!(
        journal
            .project_missing(&fixture.authority, &fixture.tasks, &sink, &HashSet::new())
            .unwrap(),
        1
    );
    assert_eq!(sink.states(), vec!["pending", "allowed"]);
}

#[tokio::test(start_paused = true)]
async fn cancelled_or_failed_round_preserves_primary_error_while_draining_known_decisions() {
    use crate::features::remote_control::controller::turn_budget::TURN_TIMEOUT_CODE;
    for expected in [TASK_CANCELLED_CODE, TURN_TIMEOUT_CODE, "task_engine_failed"] {
        let fixture = Fixture::new();
        let sink = JournalSink::new();
        let mut journal = PermissionJournal::new(fixture.owner.clone());
        let (view, receiver) = fixture.mint("callback-one").await;
        journal.register("callback-one", &view, &sink).unwrap();
        fixture.decide(&view.permission_request_id, PermissionDecision::Deny);
        fixture.respond(&view.permission_request_id, false).await;
        receiver.await.unwrap();
        let budget = TurnBudget::for_test(Duration::from_secs(1));
        let cancel = CancellationToken::new();
        let result = if expected == TASK_CANCELLED_CODE {
            cancel.cancel();
            budget.run(&cancel, async { Ok(()) }).await
        } else if expected == TURN_TIMEOUT_CODE {
            budget
                .run(&cancel, async {
                    tokio::time::sleep(Duration::from_secs(2)).await;
                    Ok(())
                })
                .await
        } else {
            Err(expected.to_owned())
        };
        assert_eq!(result.as_ref().unwrap_err(), expected);
        assert_eq!(
            project_permission_result(
                result,
                &mut journal,
                &fixture.authority,
                &fixture.tasks,
                &sink
            )
            .unwrap_err(),
            expected
        );
        assert_eq!(sink.states(), vec!["pending", "denied"]);
    }
}

#[tokio::test]
async fn expiry_and_pending_terminal_denial_use_authority_state_without_false_allow_or_duplicates()
{
    for expired in [false, true] {
        let fixture = Fixture::new();
        let sink = JournalSink::new();
        let mut journal = PermissionJournal::new(fixture.owner.clone());
        let (view, _receiver) = fixture.mint("callback-one").await;
        journal.register("callback-one", &view, &sink).unwrap();
        if expired {
            fixture.clock.store(61_000, Ordering::SeqCst);
            fixture.authority.sweep_expired(61_000);
        } else {
            fixture
                .authority
                .decide_for_owner(
                    DecisionAuthority::ControllerA,
                    &fixture.owner.lease,
                    &fixture.owner.task_id,
                    &view.permission_request_id,
                    PermissionDecision::Allow,
                    &PermissionDecisionOwner {
                        session_id: fixture.owner.session_id.clone(),
                        task_sequence: fixture.owner.task_sequence,
                    },
                )
                .unwrap();
        }
        assert_eq!(
            journal
                .project_all(&fixture.authority, &fixture.tasks, &sink)
                .unwrap(),
            usize::from(expired)
        );
        fixture.authority.deny_all_for_task(
            &fixture.owner.task_id,
            "task_terminal",
            fixture.authority.now_ms(),
        );
        project_permission_result(
            Ok(()),
            &mut journal,
            &fixture.authority,
            &fixture.tasks,
            &sink,
        )
        .unwrap();
        assert_eq!(
            sink.states(),
            vec!["pending", if expired { "expired" } else { "denied" }]
        );
        assert_eq!(
            journal
                .project_all(&fixture.authority, &fixture.tasks, &sink)
                .unwrap(),
            0
        );
    }
}

#[tokio::test]
async fn registered_distinct_approval_ids_each_project_exactly_once() {
    let fixture = Fixture::new();
    let sink = JournalSink::new();
    let mut journal = PermissionJournal::new(fixture.owner.clone());
    let (first, first_rx) = fixture.mint("callback-one").await;
    journal.register("callback-one", &first, &sink).unwrap();
    fixture.decide(&first.permission_request_id, PermissionDecision::Allow);
    fixture.respond(&first.permission_request_id, true).await;
    first_rx.await.unwrap();
    let (second, second_rx) = fixture.mint("callback-two").await;
    assert_ne!(first.permission_request_id, second.permission_request_id);
    journal.register("callback-two", &second, &sink).unwrap();
    fixture.decide(&second.permission_request_id, PermissionDecision::Deny);
    fixture.respond(&second.permission_request_id, false).await;
    second_rx.await.unwrap();
    assert_eq!(
        journal
            .project_all(&fixture.authority, &fixture.tasks, &sink)
            .unwrap(),
        2
    );
    assert_eq!(
        journal
            .project_all(&fixture.authority, &fixture.tasks, &sink)
            .unwrap(),
        0
    );
    let states = sink.states();
    assert_eq!(states.iter().filter(|s| s.as_str() == "allowed").count(), 1);
    assert_eq!(states.iter().filter(|s| s.as_str() == "denied").count(), 1);
    assert_eq!(states.len(), 4);
}

#[tokio::test]
async fn terminal_projection_rejects_success_without_masking_execution_or_stale_owner_errors() {
    let fixture = Fixture::new();
    let sink = JournalSink::new();
    let mut journal = PermissionJournal::new(fixture.owner.clone());
    let (view, receiver) = fixture.mint("callback-terminal").await;
    journal.register("callback-terminal", &view, &sink).unwrap();
    fixture.decide(&view.permission_request_id, PermissionDecision::Allow);
    fixture.respond(&view.permission_request_id, true).await;
    receiver.await.unwrap();
    fixture
        .tasks
        .ledger()
        .complete(
            &fixture.owner.task_id,
            RemoteTaskTerminal::Interrupted,
            None,
            3,
        )
        .unwrap();

    assert_eq!(
        project_permission_result(
            Ok(()),
            &mut journal,
            &fixture.authority,
            &fixture.tasks,
            &sink,
        )
        .unwrap_err(),
        "permission_task_terminal"
    );
    assert_eq!(
        project_permission_result(
            Err("original_failure".into()),
            &mut journal,
            &fixture.authority,
            &fixture.tasks,
            &sink,
        )
        .unwrap_err(),
        "original_failure"
    );
    let mut stale_owner = fixture.owner.clone();
    stale_owner.task_sequence += 1;
    let mut stale_journal = PermissionJournal::new(stale_owner);
    assert_eq!(
        stale_journal
            .project_all(&fixture.authority, &fixture.tasks, &sink)
            .unwrap_err(),
        "permission_binding_stale"
    );
    assert_eq!(sink.states(), vec!["pending"]);
}

#[tokio::test]
async fn retired_owner_cannot_consume_or_project_into_the_reused_task() {
    let fixture = Fixture::new();
    let sink = JournalSink::new();
    let mut journal = PermissionJournal::new(fixture.owner.clone());
    let (view, receiver) = fixture.mint("callback-one").await;
    journal.register("callback-one", &view, &sink).unwrap();
    fixture.decide(&view.permission_request_id, PermissionDecision::Allow);
    let mut foreign = fixture.owner.clone();
    foreign.task_sequence += 1;
    assert!(
        take_remote_permission_resolution(
            &mut *fixture.pending.lock().await,
            &foreign,
            &view.permission_request_id,
            true
        )
        .is_err()
    );
    fixture.respond(&view.permission_request_id, true).await;
    receiver.await.unwrap();
    fixture
        .tasks
        .ledger()
        .complete(
            &fixture.owner.task_id,
            RemoteTaskTerminal::Interrupted,
            None,
            3,
        )
        .unwrap();
    for index in 0..256 {
        let mut request = fixture.request.clone();
        request.task_id = format!("task-filler-{index}");
        request.client_request_id = format!("client-filler-{index}");
        fixture
            .tasks
            .ledger()
            .admit(&fixture.owner.lease, &request, 4)
            .unwrap();
        fixture
            .tasks
            .ledger()
            .complete(&request.task_id, RemoteTaskTerminal::Succeeded, None, 5)
            .unwrap();
    }
    fixture
        .tasks
        .ledger()
        .admit(&fixture.owner.lease, &fixture.request, 6)
        .unwrap();
    let new = fixture
        .tasks
        .ledger()
        .begin_dispatch(&fixture.owner.task_id, 7)
        .unwrap();
    assert_ne!(new.sequence, fixture.owner.task_sequence);
    assert_eq!(
        project_permission_result(
            Ok(()),
            &mut journal,
            &fixture.authority,
            &fixture.tasks,
            &sink
        )
        .unwrap_err(),
        "permission_binding_stale"
    );
    assert_eq!(sink.states(), vec!["pending"]);
    assert_eq!(
        project_permission_result(
            Err("original_failure".into()),
            &mut journal,
            &fixture.authority,
            &fixture.tasks,
            &sink
        )
        .unwrap_err(),
        "original_failure"
    );
}
