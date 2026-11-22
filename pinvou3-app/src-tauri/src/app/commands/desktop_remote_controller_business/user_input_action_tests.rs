use super::*;
use crate::features::assistant::{
    remote_user_input::{self, InputTurnGuard},
    skill_materialization::RemoteTurnOwner,
};
use crate::features::remote_control::controller::{
    controlled::{ControlledLease, ControlledRegistration},
    relay_transport::{AuthenticatedLease, ControllerRole, transport_channels},
    task::{
        RemoteSessionProvisioner, RemoteTaskRequest, RemoteTaskTerminal,
        task_input_digest_for_value,
    },
};
use std::sync::atomic::{AtomicUsize, Ordering};

struct Sessions(String);
impl RemoteSessionProvisioner for Sessions {
    fn create_authoritative_session(&self, _: &str, _: bool) -> Result<String, String> {
        Ok(self.0.clone())
    }
    fn session_exists(&self, _: &str) -> Result<bool, String> {
        Ok(true)
    }
}
struct Fixture {
    controlled: ControlledDeviceManager,
    tasks: RemoteTaskService,
    lease: LeaseContext,
    body: Value,
    _input: InputTurnGuard,
}
impl Fixture {
    fn new() -> Self {
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
            connection_id: "connection-input".into(),
            controller_lease_id: "lease-input".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-input".into(),
            remote_control_generation: operation.generation().into(),
        };
        controlled
            .complete_connected(
                operation.clone(),
                ControlledLease {
                    context: lease.clone(),
                    liveness_epoch_a: "live-a-input".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [1; 32],
                    transport_key_fingerprint: [2; 32],
                    controller_device_name: "Controller".into(),
                    connected_at_b: 3_000,
                    negotiated_capabilities: vec!["task_v1".into(), "permission_v1".into()],
                },
            )
            .unwrap();
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
        let session_id = uuid::Uuid::new_v4().to_string();
        let task_id = format!("task-{}", uuid::Uuid::new_v4());
        let tasks = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        tasks.set_session_provisioner(Arc::new(Sessions(session_id.clone())));
        tasks
            .ensure_remote_session(
                "a-input-ref",
                "Remote",
                false,
                &lease.remote_control_generation,
            )
            .unwrap();
        let input = json!({"text":"Choose a health report", "attachment_ids":[], "skill_ids":[]});
        let request = RemoteTaskRequest::from_payload(&json!({
            "connection_id":lease.connection_id,"controller_lease_id":lease.controller_lease_id,
            "lease_epoch":lease.lease_epoch,"relay_epoch":lease.relay_epoch,
            "remote_control_generation":lease.remote_control_generation,"task_id":task_id,
            "client_request_id":"client-input","session_id":session_id,"capability_revision":"1",
            "mode":"chat","model_source":"controlled_device","input_digest":task_input_digest_for_value(&input).unwrap(),"input":input,
        })).unwrap();
        tasks.ledger().admit(&lease, &request, 3_100).unwrap();
        tasks.ledger().begin_dispatch(&task_id, 3_200).unwrap();
        let guard = RemoteInputTurn::register(
            &session_id,
            &task_id,
            1,
            Arc::new(RemoteTurnOwner::default()),
            tokio_util::sync::CancellationToken::new(),
            TurnBudget::for_test(std::time::Duration::from_secs(10)),
        )
        .unwrap();
        remote_user_input::bind_engine_turn(&session_id, &task_id, 1, "turn-input").unwrap();
        remote_user_input::record_for_turn(&session_id, 1, "turn-input", "call-input", json!([
            {"id":"report-kind","header":"Pick","question":"Choose a report","multi_select":false,
                "options":[{"label":"A","description":"First"},{"label":"B","description":"Second"}]}
        ])).unwrap();
        let id = guard.0.pending().unwrap()[0].input_id.clone();
        Self {
            controlled,
            tasks,
            lease,
            body: json!({"task_id":task_id,"input_id":id,"cancel":false,
            "answers":[{"question_id":"0","option_ids":["1"]}]}),
            _input: guard,
        }
    }
    fn handler(&self) -> UserInputActionHandler<'_> {
        UserInputActionHandler {
            controlled: &self.controlled,
            tasks: &self.tasks,
        }
    }
    fn task(&self) -> &str {
        self.body["task_id"].as_str().unwrap()
    }
}
fn mailbox(calls: Arc<AtomicUsize>) -> InputSender {
    Box::new(move |id, response| {
        assert_eq!(id, "call-input");
        let response = response.unwrap();
        assert_eq!(response.answers[0].id, "report-kind");
        assert_eq!(response.answers[0].value, "B");
        calls.fetch_add(1, Ordering::SeqCst);
        Ok(())
    })
}

#[tokio::test]
async fn real_task_and_authenticated_lease_gates_deliver_one_canonical_answer() {
    let fixture = Fixture::new();
    let listed = fixture
        .handler()
        .handle(
            &fixture.lease,
            "a-input-ref",
            "user_input_list",
            &json!({"task_id":fixture.task()}),
            |_| async { Err::<InputSender, _>("read cannot load engine".into()) },
        )
        .await
        .unwrap();
    assert_eq!(listed["requests"][0]["input_id"], fixture.body["input_id"]);
    let calls = Arc::new(AtomicUsize::new(0));
    for _ in 0..2 {
        let output = fixture
            .handler()
            .handle(
                &fixture.lease,
                "a-input-ref",
                "user_input_respond",
                &fixture.body,
                {
                    let calls = calls.clone();
                    move |_| async move { Ok(mailbox(calls)) }
                },
            )
            .await
            .unwrap();
        assert_eq!(output, json!({"state":"accepted"}));
    }
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let listed = fixture
        .handler()
        .handle(
            &fixture.lease,
            "a-input-ref",
            "user_input_list",
            &json!({"task_id":fixture.task()}),
            |_| async { Err::<InputSender, _>("read cannot load engine".into()) },
        )
        .await
        .unwrap();
    assert_eq!(listed, json!({"requests":[]}));
}

#[tokio::test]
async fn wrong_session_lease_and_question_ids_cannot_reach_the_mailbox() {
    let fixture = Fixture::new();
    let mut wrong_lease = fixture.lease.clone();
    wrong_lease.lease_epoch = "2".into();
    for (lease, session_ref, body) in [
        (&wrong_lease, "a-input-ref", fixture.body.clone()),
        (&fixture.lease, "other-reference", fixture.body.clone()),
    ] {
        assert!(
            fixture
                .handler()
                .handle(lease, session_ref, "user_input_respond", &body, |_| async {
                    panic!("unbound request cannot capture an engine")
                })
                .await
                .is_err()
        );
    }
    let mut wrong_question = fixture.body.clone();
    wrong_question["input_id"] = json!("another-input");
    assert!(
        fixture
            .handler()
            .handle(
                &fixture.lease,
                "a-input-ref",
                "user_input_respond",
                &wrong_question,
                |_| async {
                    Ok(Box::new(
                        |_: &str, _: Option<UserInputResponse>| -> Result<(), String> {
                            panic!("unknown question cannot send")
                        },
                    ) as InputSender)
                }
            )
            .await
            .is_err()
    );
    assert_eq!(fixture._input.0.pending().unwrap().len(), 1);
}

#[tokio::test(start_paused = true)]
async fn terminal_cancel_disconnect_or_deadline_during_capture_prevents_late_answer() {
    for stop in ["terminal", "cancel", "disconnect", "deadline"] {
        let fixture = Fixture::new();
        let entered = Arc::new(tokio::sync::Notify::new());
        let release = Arc::new(tokio::sync::Notify::new());
        let handler = fixture.handler();
        let response = handler.handle(
            &fixture.lease,
            "a-input-ref",
            "user_input_respond",
            &fixture.body,
            {
                let entered = entered.clone();
                let release = release.clone();
                move |_| async move {
                    entered.notify_one();
                    release.notified().await;
                    Ok(Box::new(
                        |_: &str, _: Option<UserInputResponse>| -> Result<(), String> {
                            panic!("late response must not enqueue")
                        },
                    ) as InputSender)
                }
            },
        );
        tokio::pin!(response);
        tokio::select! { _=entered.notified()=>{}, result=&mut response=>panic!("premature completion: {result:?}") }
        match stop {
            "terminal" => {
                fixture
                    .tasks
                    .ledger()
                    .complete(fixture.task(), RemoteTaskTerminal::Interrupted, None, 3_400)
                    .unwrap();
            }
            "cancel" => {
                fixture
                    .tasks
                    .ledger()
                    .request_cancel(fixture.task(), 3_400)
                    .unwrap();
            }
            "disconnect" => {
                fixture.controlled.stop();
            }
            "deadline" => tokio::time::advance(std::time::Duration::from_secs(10)).await,
            _ => unreachable!(),
        }
        release.notify_one();
        assert!(response.await.is_err(), "{stop}");
    }
    let next = Fixture::new();
    let calls = Arc::new(AtomicUsize::new(0));
    let seen = calls.clone();
    next.handler()
        .handle(
            &next.lease,
            "a-input-ref",
            "user_input_respond",
            &next.body,
            move |_| async move { Ok(mailbox(seen)) },
        )
        .await
        .unwrap();
    assert_eq!(
        calls.load(Ordering::SeqCst),
        1,
        "next task works after failed waits"
    );
}
