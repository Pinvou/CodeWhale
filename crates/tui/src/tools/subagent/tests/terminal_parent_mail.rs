use super::*;
use tokio::sync::oneshot;
use wiremock::{Mock, MockServer, ResponseTemplate, matchers::method};

const WAIT: Duration = Duration::from_secs(15);

struct Gate {
    reached: oneshot::Receiver<Option<SubAgentResult>>,
    resume: oneshot::Sender<()>,
}

fn gate(manager: &mut SubAgentManager, id: &str, phase: ParentMailTestPhase) -> Gate {
    let (reached_tx, reached) = oneshot::channel();
    let (resume, resume_rx) = oneshot::channel();
    manager.parent_mail_test_pause = Some(ParentMailTestPause {
        agent_id: id.to_owned(),
        phase,
        reached: reached_tx,
        resume: resume_rx,
    });
    Gate { reached, resume }
}

impl Gate {
    async fn reached(&mut self) -> Option<SubAgentResult> {
        tokio::time::timeout(WAIT, &mut self.reached)
            .await
            .expect("worker reached the selected boundary")
            .expect("worker retained its boundary receipt")
    }
}

// The manager owns the actual worker JoinHandle, so interrupt exercises real
// cancellation. This receipt also runs when that future is dropped by abort.
struct WorkerDone(Option<oneshot::Sender<()>>);

impl Drop for WorkerDone {
    fn drop(&mut self) {
        if let Some(done) = self.0.take() {
            let _ = done.send(());
        }
    }
}

struct Worker {
    manager: SharedSubAgentManager,
    runtime: SubAgentRuntime,
    id: String,
    abort: tokio::task::AbortHandle,
    done: Option<oneshot::Receiver<()>>,
    server: MockServer,
    _workspace: tempfile::TempDir,
}

impl Drop for Worker {
    fn drop(&mut self) {
        self.abort.abort();
    }
}

impl Worker {
    async fn start(max_steps: u32, phase: ParentMailTestPhase) -> (Self, Gate) {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "id": "chatcmpl-terminal-mail",
                "model": "deepseek-v4-flash",
                "choices": [{"index": 0, "message": {
                    "role": "assistant", "content": "Finished this step"
                }, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 9, "completion_tokens": 3, "total_tokens": 12}
            })))
            .mount(&server)
            .await;
        let workspace = tempdir().expect("owned workspace");
        let manager = new_shared_subagent_manager(workspace.path().to_path_buf(), 4);
        let config = crate::config::Config {
            api_key: Some("test-key".to_owned()),
            base_url: Some(format!("{}/v1", server.uri())),
            ..crate::config::Config::default()
        };
        let mut runtime = stub_runtime();
        runtime.client = DeepSeekClient::new(&config).expect("loopback client");
        // Resume may rebuild the model-aware client; keep that route local too.
        runtime.api_config = Some(Arc::new(config));
        runtime.manager = Arc::clone(&manager);
        runtime.context = ToolContext::new(workspace.path().to_path_buf());
        let id = format!("agent_{}", &Uuid::new_v4().simple().to_string()[..8]);
        let (input_tx, input_rx) = mpsc::unbounded_channel();
        let agent = SubAgent::new(
            id.clone(),
            FleetRole::Worker,
            "Finish the task".to_owned(),
            make_assignment(),
            runtime.model.clone(),
            None,
            Some(vec![]),
            input_tx,
            workspace.path().to_path_buf(),
            "boot_terminal_mail".to_owned(),
        );
        let mut spec = make_worker_spec(&id, workspace.path().to_path_buf());
        spec.agent_type = FleetRole::Worker;
        spec.runtime_profile = runtime.worker_profile.clone();
        spec.tool_profile = AgentWorkerToolProfile::Explicit(vec![]);
        spec.role = Some("worker".to_owned());
        spec.max_steps = max_steps;
        let task = SubAgentTask {
            manager_handle: Arc::clone(&manager),
            runtime: runtime.clone(),
            agent_id: id.clone(),
            agent_type: FleetRole::Worker,
            prompt: "Finish the task".to_owned(),
            assignment: make_assignment(),
            allowed_tools: Some(vec![]),
            fork_context: false,
            started_at: Instant::now(),
            max_steps,
            token_budget: None,
            wall_time: Duration::from_secs(60),
            input_rx,
            launch_gate: None,
            _foreground_child_registration: None,
        };
        let (done_tx, done) = oneshot::channel();
        let receipt = WorkerDone(Some(done_tx));
        let mut locked = manager.write().await;
        locked.agents.insert(id.clone(), agent);
        locked.register_worker(spec);
        let gate = gate(&mut locked, &id, phase);
        let handle = tokio::spawn(async move {
            let _receipt = receipt;
            run_subagent_task(task).await;
        });
        let abort = handle.abort_handle();
        locked.agents.get_mut(&id).unwrap().task_handle = Some(handle);
        drop(locked);
        (
            Self {
                manager,
                runtime,
                id,
                abort,
                done: Some(done),
                server,
                _workspace: workspace,
            },
            gate,
        )
    }

    async fn finish(&mut self) -> SubAgentResult {
        let done = self.done.take().expect("single retirement");
        let receipt = tokio::time::timeout(WAIT, done).await;
        if !matches!(receipt, Ok(Ok(()))) {
            self.abort.abort();
        }
        tokio::time::timeout(WAIT, async {
            while !self.abort.is_finished() {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("owned worker retired");
        assert!(
            matches!(receipt, Ok(Ok(()))),
            "worker must deliver its retirement receipt"
        );
        self.manager.read().await.get_result(&self.id).unwrap()
    }

    async fn requests(&self) -> Vec<Value> {
        self.server
            .received_requests()
            .await
            .unwrap()
            .iter()
            .map(|request| serde_json::from_slice(&request.body).unwrap())
            .collect()
    }
}

fn matching_user_texts<'a>(checkpoint: &'a SubAgentCheckpoint, prefix: &str) -> Vec<&'a str> {
    checkpoint
        .messages
        .iter()
        .filter(|message| message.role == Role::User)
        .flat_map(|message| &message.content)
        .filter_map(|block| match block {
            ContentBlock::Text { text, .. } if text.starts_with(prefix) => Some(text.as_str()),
            _ => None,
        })
        .collect()
}

#[tokio::test]
async fn forkguard_terminal_mail_before_fence_reaches_next_model_request_in_fifo_order() {
    let (mut worker, mut paused) = Worker::start(2, ParentMailTestPhase::BeforeFinalDrain).await;
    paused.reached().await;
    {
        let mut manager = worker.manager.write().await;
        for text in ["terminal-mail:first", "terminal-mail:second"] {
            assert!(
                manager
                    .followup_child(&worker.id, text.to_owned())
                    .unwrap()
                    .woke
            );
        }
    }
    let _ = paused.resume.send(());
    let result = worker.finish().await;
    assert_eq!(result.status, SubAgentStatus::Completed);
    assert_eq!(result.steps_taken, 2);
    let requests = worker.requests().await;
    assert_eq!(requests.len(), 2);
    let texts: Vec<_> = requests[1]["messages"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|message| message["role"] == "user")
        .filter_map(|message| message["content"].as_str())
        .filter(|text| text.starts_with("terminal-mail:"))
        .collect();
    assert_eq!(texts, ["terminal-mail:first", "terminal-mail:second"]);
}

#[tokio::test]
async fn forkguard_terminal_mail_after_fence_rejects_both_ingress_without_mutation() {
    let (mut worker, mut before) = Worker::start(1, ParentMailTestPhase::BeforeFinalDrain).await;
    before.reached().await;
    let mut after = {
        let mut manager = worker.manager.write().await;
        manager
            .queue_running_parent_message(&worker.id, "queue-only".to_owned())
            .unwrap();
        gate(
            &mut manager,
            &worker.id,
            ParentMailTestPhase::AfterFinalDrain,
        )
    };
    let _ = before.resume.send(());
    after.reached().await;
    let checks = {
        let mut manager = worker.manager.write().await;
        let status = manager.agents[&worker.id].status.clone();
        let queue_before: Vec<_> = manager.queued_mail[&worker.id]
            .iter()
            .map(|m| m.text.clone())
            .collect();
        let pending_before = manager
            .pending_follow_ups
            .get(&worker.id)
            .map(|m| m.lock().unwrap().len());
        let woke_before = manager.woken_agents.get(&worker.id).copied();
        let message =
            manager.queue_running_parent_message(&worker.id, "rejected message".to_owned());
        let followup = manager.followup_child(&worker.id, "rejected followup".to_owned());
        let queue_after: Vec<_> = manager.queued_mail[&worker.id]
            .iter()
            .map(|m| m.text.clone())
            .collect();
        let pending_after = manager
            .pending_follow_ups
            .get(&worker.id)
            .map(|m| m.lock().unwrap().len());
        (
            status,
            message.is_err(),
            followup.is_err(),
            queue_before,
            queue_after,
            pending_before == pending_after,
            woke_before == manager.woken_agents.get(&worker.id).copied(),
        )
    };
    let _ = after.resume.send(());
    let result = worker.finish().await;
    assert_eq!(checks.0, SubAgentStatus::Running);
    assert!(
        checks.1 && checks.2,
        "neither ingress may acknowledge sealed mail"
    );
    assert_eq!(checks.3, ["queue-only"]);
    assert_eq!(checks.3, checks.4);
    assert!(
        checks.5 && checks.6,
        "rejection must not allocate tickets or wake the child"
    );
    assert_eq!(result.status, SubAgentStatus::Completed);
    assert_eq!(worker.requests().await.len(), 1);
    let mut manager = worker.manager.write().await;
    let (_, late) = manager
        .interrupt_child_by_id(&worker.id, None, "late interrupt".to_owned())
        .unwrap();
    assert_eq!(late.status, result.status);
    assert_eq!(late.checkpoint, result.checkpoint);
}

#[tokio::test]
async fn forkguard_terminal_mail_step_limit_keeps_oversized_suffix_without_extra_call() {
    let (mut worker, mut paused) = Worker::start(1, ParentMailTestPhase::BeforeFinalDrain).await;
    paused.reached().await;
    let first = format!("terminal-mail:first:{}", "a".repeat(180 * 1024));
    let second = format!("terminal-mail:second:{}", "b".repeat(180 * 1024));
    let inbox = {
        let mut manager = worker.manager.write().await;
        for text in [&first, &second] {
            assert!(
                manager
                    .followup_child(&worker.id, text.clone())
                    .unwrap()
                    .woke
            );
        }
        Arc::clone(&manager.pending_follow_ups[&worker.id])
    };
    let _ = paused.resume.send(());
    let result = worker.finish().await;
    assert!(
        matches!(&result.status, SubAgentStatus::Failed(reason) if reason.contains("step budget exhausted"))
    );
    assert_eq!(result.steps_taken, 1);
    assert_eq!(worker.requests().await.len(), 1);
    let cp = result.checkpoint.as_ref().unwrap();
    assert!(!cp.continuable);
    assert_eq!(
        matching_user_texts(cp, "terminal-mail:"),
        [first.as_str(), second.as_str()]
    );
    assert!(
        inbox.lock().unwrap().is_empty(),
        "winning final checkpoint acknowledges retained tickets"
    );
    assert!(
        !worker
            .manager
            .read()
            .await
            .pending_follow_ups
            .contains_key(&worker.id)
    );
}

#[tokio::test]
async fn forkguard_terminal_mail_interrupt_wins_before_final_checkpoint_ack() {
    let (mut worker, mut before) = Worker::start(1, ParentMailTestPhase::BeforeFinalDrain).await;
    before.reached().await;
    let mut terminal = {
        let mut manager = worker.manager.write().await;
        for text in ["terminal-mail:first", "terminal-mail:second"] {
            assert!(
                manager
                    .followup_child(&worker.id, text.to_owned())
                    .unwrap()
                    .woke
            );
        }
        gate(
            &mut manager,
            &worker.id,
            ParentMailTestPhase::BeforeTerminalCommit,
        )
    };
    let _ = before.resume.send(());
    let late_result = terminal
        .reached()
        .await
        .expect("actual worker final result");
    let (old_checkpoint, interrupted) = {
        let mut manager = worker.manager.write().await;
        let old = manager.agents[&worker.id].checkpoint.clone().unwrap();
        let (_, interrupted) = manager
            .interrupt_child_by_id(&worker.id, None, "interrupt wins".to_owned())
            .unwrap();
        (old, interrupted)
    };
    let _ = terminal.resume.send(());
    let retired = worker.finish().await;
    assert!(
        old_checkpoint.continuable,
        "local final snapshot must not publish before arbitration"
    );
    assert!(!late_result.checkpoint.as_ref().unwrap().continuable);
    assert!(matches!(interrupted.status, SubAgentStatus::Interrupted(_)));
    assert_eq!(retired.checkpoint, Some(old_checkpoint.clone()));
    assert_eq!(worker.requests().await.len(), 1);
    // Replay the actual producer payload through the real late-consumer guard;
    // the original task itself was genuinely aborted by interrupt above.
    let mut manager = worker.manager.write().await;
    assert!(!manager.finish_terminal_result(&worker.id, late_result, false, true));
    assert_eq!(manager.agents[&worker.id].checkpoint, Some(old_checkpoint));
    let queued: Vec<_> = manager.queued_mail[&worker.id]
        .iter()
        .map(|m| m.text.as_str())
        .collect();
    assert_eq!(queued, ["terminal-mail:first", "terminal-mail:second"]);
    let resumed = manager
        .resume_from_checkpoint(
            Arc::clone(&worker.manager),
            worker.runtime.clone(),
            &worker.id,
            "terminal-mail:third",
        )
        .unwrap();
    // Hold the manager lock until abort: inspect the real resume birth snapshot
    // without allowing another model request or leaving a spawned worker behind.
    let cp = manager.agents[&resumed.agent_id]
        .checkpoint
        .clone()
        .unwrap();
    let reopened = !manager.agents[&resumed.agent_id].parent_mail_closed;
    let handle = manager
        .agents
        .get_mut(&resumed.agent_id)
        .unwrap()
        .task_handle
        .take()
        .unwrap();
    handle.abort();
    manager
        .interrupt_child_by_id(&resumed.agent_id, None, "test retirement".to_owned())
        .unwrap();
    drop(manager);
    assert!(handle.await.unwrap_err().is_cancelled());
    assert!(reopened);
    assert!(cp.continuable);
    // Resume embeds parked mail in its assembled initial user prompt rather
    // than creating a separate message for each item.
    let resumed_input = cp
        .messages
        .iter()
        .filter(|message| message.role == Role::User)
        .flat_map(|message| &message.content)
        .filter_map(|block| match block {
            ContentBlock::Text { text, .. } => Some(text.as_str()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("\n");
    let mut positions = Vec::new();
    for marker in [
        "terminal-mail:first",
        "terminal-mail:second",
        "terminal-mail:third",
    ] {
        assert_eq!(resumed_input.matches(marker).count(), 1);
        positions.push(resumed_input.find(marker).unwrap());
    }
    assert!(positions.windows(2).all(|pair| pair[0] < pair[1]));
    assert_eq!(worker.requests().await.len(), 1);
}
