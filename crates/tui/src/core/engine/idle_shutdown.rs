//! Actor-owned, non-destructive retirement for embedding hosts. The host owns
//! external admission; this boundary owns autonomous turns and their evidence.
//!
//! An in-flight MCP boot is deliberately not part of the ready check: the run
//! teardown joins the boot worker before the pool shuts down, and that join
//! drops the boot's settlement event and failure briefing for this incarnation
//! (an engine that is about to disappear has no future turn to brief).

use super::*;

// An incarnation-local embedding lifetime handshake, like spawn/completion;
// it grants no new user, model or remote operation. Keep the Op/wire protocol
// and its exhaustive parity checks unchanged.
pub(super) struct IdleRetirement {
    pub(super) ready: tokio::sync::oneshot::Sender<bool>,
    pub(super) commit: tokio::sync::oneshot::Receiver<()>,
}

impl Engine {
    pub(super) async fn next_operation_or_retirement(&mut self) -> Option<EngineRunInput> {
        // Biased toward the queued operation, matching the neighboring timed
        // wait: at a FIFO-drain site a queued control must not lose the
        // arbitration race to a retirement request. Acceptance still refuses
        // while any queue is non-empty, but the ordering intent should live
        // here rather than depend on that downstream refusal.
        tokio::select! {
            biased;
            op = self.rx_op.recv() => op.map(|op| EngineRunInput::Operation(Box::new(op))),
            Some(request) = self.rx_idle_retirement.recv() => Some(EngineRunInput::IdleRetirement(request)),
        }
    }

    fn can_retire_without_losing_work(&mut self) -> bool {
        if self.has_scheduled_goal_continuation()
            // Only the actor creates watcher clones — the sanctioned clone
            // site is the advisor dispatch inside the actor loop in engine.rs.
            // A second clone site would permanently refuse retirement, so any
            // new holder must be gated into this check. Each watcher holds its
            // clone through event delivery. Reuse that ownership invariant:
            // get_mut synchronizes with the final clone's drop; unlike the
            // enabled setting, it also covers a watcher switched off mid-run.
            || self.advisor_emission_guard.as_mut().is_some_and(|guard| Arc::get_mut(guard).is_none())
        {
            return false;
        }
        if self
            .config
            .goal_state
            .lock()
            .map(|state| state.snapshot().is_active())
            .unwrap_or(true)
        {
            return false;
        }
        let Ok(agents) = self.subagent_manager.try_read() else {
            return false;
        };
        if agents.has_live_work_for_session(&self.session.id) {
            return false;
        }
        let Ok(mut shells) = self.shell_manager.try_lock() else {
            return false;
        };
        if shells.has_live_or_unreported_work_for_session(&self.session.id) {
            return false;
        }
        // Delivery publishes before a child's task/Running status clears.
        // Recheck AFTER the live-work checks, with both producer owners locked:
        // a child that finished during preflight cannot leave a missed payload.
        // The actor is the only remaining autonomous work producer. External
        // submissions are excluded by the host contract alone — including
        // submissions still held by an in-flight `reserve()` permit, which
        // `is_empty()` cannot observe — so what these checks accept is exactly
        // what that contract leaves behind.
        self.rx_op.is_empty()
            && self.rx_subagent_completion.is_empty()
            && self.rx_steer.is_empty()
            && self.pending_steers.is_empty()
            && self.rx_approval.is_empty()
            && self.rx_user_input.is_empty()
            && self.tx_event.capacity() == self.tx_event.max_capacity()
    }

    /// Count inputs that arrived after the retirement commit crossed the
    /// boundary. The host contract excludes new external work from the moment
    /// retirement starts; anything seen here violated it, and every send had
    /// already returned `Ok`, so the caller surfaces the count instead of
    /// letting the engine drop it silently.
    pub(super) fn count_residual_inputs(&mut self) -> usize {
        self.rx_op.len()
            + self.rx_subagent_completion.len()
            + self.rx_steer.len()
            + self.rx_approval.len()
            + self.rx_user_input.len()
            + self.pending_steers.len()
    }

    pub(super) async fn accept_idle_shutdown(
        &mut self,
        ready: tokio::sync::oneshot::Sender<bool>,
        commit: tokio::sync::oneshot::Receiver<()>,
    ) -> bool {
        // This handler is reached only between complete actor operations.
        // An autonomous turn that won first finishes normally; no cancel token
        // is fired to make it idle. No next_run_input runs during the proposal.
        if ready.is_closed() {
            return false;
        }
        // Snapshot before readiness is offered: the escape below must not
        // depend on scheduling between `ready.send` and entering the wait.
        let already_cancelled = self.cancel_token.is_cancelled();
        let idle = self.can_retire_without_losing_work();
        if ready.send(idle).is_err() || !idle {
            return false;
        }
        // Wait for the commit at the same sealed boundary, but let a cancel
        // fired *during* the proposal break the wait: a host blocked forever
        // inside its synchronous predicate must not pin the actor. A token
        // already latched by an earlier cancelled turn does not escape — that
        // stale state must not make retirement permanently unavailable.
        // Commit still wins a simultaneous cancel: the host must never see a
        // `send` the actor silently ignored.
        let wait_cancel = self.cancel_token.clone();
        let committed = tokio::select! {
            biased;
            result = commit => result.is_ok(),
            () = async {
                if already_cancelled {
                    std::future::pending::<()>().await;
                } else {
                    wait_cancel.cancelled().await;
                }
            } => false,
        };
        committed
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture(root: &std::path::Path) -> (Engine, EngineHandle) {
        Engine::new(
            EngineConfig {
                workspace: root.to_owned(),
                mcp_config_path: root.join("mcp.json"),
                ..Default::default()
            },
            &Config::default(),
        )
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_does_not_overtake_accepted_operations() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, handle) = fixture(home.path());
        handle
            .send(Op::SetAdvisorEnabled { enabled: false })
            .await
            .unwrap();
        let (ready, receipt) = tokio::sync::oneshot::channel();
        let (_commit, committed) = tokio::sync::oneshot::channel();
        // Timeout-wrapped so a regression that wedges the handshake fails the
        // assertion instead of hanging the whole test process.
        assert!(
            !tokio::time::timeout(
                Duration::from_secs(5),
                engine.accept_idle_shutdown(ready, committed)
            )
            .await
            .expect("the refusal path must not wait on the commit")
        );
        assert!(!receipt.await.unwrap());
        assert!(matches!(
            engine.rx_op.try_recv().unwrap(),
            Op::SetAdvisorEnabled { enabled: false }
        ));
        assert!(engine.can_retire_without_losing_work());
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_closed_lifetime_channel_leaves_operations_usable() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, mut handle) = fixture(home.path());
        drop(handle.idle_retirement.take());
        assert!(
            tokio::time::timeout(
                Duration::from_millis(10),
                engine.next_operation_or_retirement()
            )
            .await
            .is_err()
        );
        handle
            .send(Op::SetAdvisorEnabled { enabled: false })
            .await
            .unwrap();
        assert!(matches!(
            engine.next_operation_or_retirement().await,
            Some(EngineRunInput::Operation(_))
        ));
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_checks_host_completion_after_actor_ready() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, handle) = fixture(home.path());
        let host_idle = Arc::new(std::sync::atomic::AtomicBool::new(true));
        let retiring_handle = handle.clone();
        let observed = host_idle.clone();
        let retiring = tokio::spawn(async move {
            retiring_handle
                .shutdown_when_idle(|| observed.load(std::sync::atomic::Ordering::Acquire))
                .await
        });
        let Some(EngineRunInput::IdleRetirement(request)) =
            engine.next_operation_or_retirement().await
        else {
            panic!("the request must reach the actor before host completion changes");
        };
        // The actor's event queue is empty, but its host has received the final
        // event and is still persisting/emitting it. A pre-request check was true.
        host_idle.store(false, std::sync::atomic::Ordering::Release);
        assert!(engine.can_retire_without_losing_work());
        assert!(
            !tokio::time::timeout(
                Duration::from_secs(5),
                engine.accept_idle_shutdown(request.ready, request.commit)
            )
            .await
            .expect("the refusal path must not wait on the commit")
        );
        assert!(!retiring.await.unwrap().unwrap());
        assert!(!engine.cancel_token.is_cancelled());
        handle
            .send(Op::SetAdvisorEnabled { enabled: false })
            .await
            .unwrap();
        assert!(matches!(
            engine.next_operation_or_retirement().await,
            Some(EngineRunInput::Operation(_))
        ));

        host_idle.store(true, std::sync::atomic::Ordering::Release);
        let retiring_handle = handle.clone();
        let retiring = tokio::spawn(async move {
            retiring_handle
                .shutdown_when_idle(|| host_idle.load(std::sync::atomic::Ordering::Acquire))
                .await
        });
        let Some(EngineRunInput::IdleRetirement(request)) =
            engine.next_operation_or_retirement().await
        else {
            panic!("the idle retry must reach the actor");
        };
        assert!(
            tokio::time::timeout(
                Duration::from_secs(5),
                engine.accept_idle_shutdown(request.ready, request.commit)
            )
            .await
            .expect("the committed path must resolve promptly")
        );
        assert!(retiring.await.unwrap().unwrap());
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_cancelled_proposal_does_not_stop_the_actor() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        let (ready, receipt) = tokio::sync::oneshot::channel();
        let (commit, committed) = tokio::sync::oneshot::channel();
        let pending = tokio::spawn(async move {
            let accepted = tokio::time::timeout(
                Duration::from_secs(5),
                engine.accept_idle_shutdown(ready, committed),
            )
            .await
            .expect("the cancel path must not wedge the actor");
            (accepted, engine)
        });
        assert!(receipt.await.unwrap());
        drop(commit);
        let (accepted, mut engine) = pending.await.unwrap();
        assert!(!accepted);
        assert!(!engine.cancel_token.is_cancelled());
        assert!(engine.can_retire_without_losing_work());
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_preserves_queued_child_goal_and_live_worker() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        engine
            .tx_subagent_completion
            .send(SubAgentCompletion {
                owner_session_id: engine.session.id.clone(),
                agent_id: "finished-child".into(),
                payload: "the exact completed child result".into(),
            })
            .unwrap();
        assert!(!engine.can_retire_without_losing_work());
        assert_eq!(
            engine.rx_subagent_completion.try_recv().unwrap().payload,
            "the exact completed child result"
        );
        let deadline = Instant::now() + Duration::from_secs(60);
        engine.scheduled_goal_continuation = Some(ScheduledGoalContinuation {
            id: 42,
            dynamic_tools: Vec::new(),
            enqueued: false,
            ready_at: Some(deadline),
            was_delayed: true,
        });
        assert!(!engine.can_retire_without_losing_work());
        assert_eq!(
            engine
                .scheduled_goal_continuation
                .as_ref()
                .unwrap()
                .ready_at,
            Some(deadline)
        );
        engine.scheduled_goal_continuation = None;
        {
            let mut agents = engine.subagent_manager.write().await;
            let child = agents.insert_test_running_agent("child", home.path());
            agents.assign_test_session_owner(&child, &engine.session.id);
        }
        assert!(
            !engine.can_retire_without_losing_work(),
            "UI/heartbeat counts cannot certify retirement"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_advisor_clone_and_delivery_both_gate_retirement() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, handle) = fixture(home.path());
        let guard = Arc::new(tokio::sync::Mutex::new(
            crate::tools::subagent::EmissionGuard::new(),
        ));
        let watcher = guard.clone();
        engine.advisor_emission_guard = Some(guard);
        engine.config.advisor_config.enabled = false;
        assert!(
            !engine.can_retire_without_losing_work(),
            "switching off is not a join receipt"
        );
        engine
            .tx_event
            .try_send(Event::status("completed advisor output".to_string()))
            .unwrap();
        drop(watcher);
        assert!(
            !engine.can_retire_without_losing_work(),
            "undelivered output must survive the watcher"
        );
        handle.rx_event.write().await.recv().await.unwrap();
        assert!(
            engine.can_retire_without_losing_work(),
            "a historical watcher must not permanently refuse retirement"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_every_pending_input_queue_blocks_retirement() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        // A queued steer must refuse retirement until it is drained.
        let steer = SteerMessage {
            id: "queued-steer".to_string(),
            target: SteerTarget {
                session_epoch: 0,
                turn_generation: 1,
            },
            content: "steer must not be lost".to_string(),
        };
        engine.rx_steer = {
            let (tx, rx) = mpsc::channel(1);
            tx.send(steer.clone()).await.unwrap();
            rx
        };
        assert!(!engine.can_retire_without_losing_work());
        engine.rx_steer = mpsc::channel(1).1;
        engine.pending_steers.push(steer);
        assert!(
            !engine.can_retire_without_losing_work(),
            "a committed pending steer is accepted work"
        );
        engine.pending_steers.clear();
        // An undelivered approval decision and an undelivered user-input
        // decision are the same class of accepted work.
        engine.rx_approval = {
            let (tx, rx) = mpsc::channel(1);
            tx.send(ApprovalDecision::Approved {
                id: "approval-1".to_string(),
                ack: None,
            })
            .await
            .unwrap();
            rx
        };
        assert!(!engine.can_retire_without_losing_work());
        engine.rx_approval = mpsc::channel(1).1;
        engine.rx_user_input = {
            let (tx, rx) = mpsc::channel(1);
            tx.send(UserInputDecision::Cancelled {
                id: "input-1".to_string(),
            })
            .await
            .unwrap();
            rx
        };
        assert!(!engine.can_retire_without_losing_work());
        engine.rx_user_input = mpsc::channel(1).1;
        assert!(engine.can_retire_without_losing_work());
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_live_and_unreported_shell_work_gates_retirement() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        let session = engine.session.id.clone();
        {
            let mut shells = engine.shell_manager.lock().unwrap();
            shells.seed_session_shell_for_test("live-job", &session, true);
        }
        assert!(
            !engine.can_retire_without_losing_work(),
            "a running session shell is live work"
        );
        {
            let mut shells = engine.shell_manager.lock().unwrap();
            shells.finish_seeded_shell_for_test("live-job");
        }
        assert!(
            !engine.can_retire_without_losing_work(),
            "finished output not yet delivered must survive retirement"
        );
        {
            let mut shells = engine.shell_manager.lock().unwrap();
            shells.mark_seeded_completion_reported_for_test("live-job");
        }
        assert!(
            engine.can_retire_without_losing_work(),
            "delivered shell evidence no longer blocks retirement"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_timed_goal_wait_still_serves_the_retirement_arm() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, handle) = fixture(home.path());
        let deadline = Instant::now() + Duration::from_secs(60);
        engine.scheduled_goal_continuation = Some(ScheduledGoalContinuation {
            id: 7,
            dynamic_tools: Vec::new(),
            enqueued: false,
            ready_at: Some(deadline),
            was_delayed: true,
        });
        let (ready, receipt) = tokio::sync::oneshot::channel();
        let (_commit, committed) = tokio::sync::oneshot::channel();
        handle
            .idle_retirement
            .as_ref()
            .unwrap()
            .send(IdleRetirement {
                ready,
                commit: committed,
            })
            .await
            .unwrap();
        // The biased timed wait for the goal continuation must serve the
        // retirement arm instead of sleeping through the whole window.
        let input = tokio::time::timeout(Duration::from_millis(500), engine.next_run_input(false))
            .await
            .expect("the armed retirement request must not wait out the goal timer");
        let Some(EngineRunInput::IdleRetirement(request)) = input else {
            panic!("the retirement arm must fire while the goal wait is armed");
        };
        assert!(
            !tokio::time::timeout(
                Duration::from_secs(5),
                engine.accept_idle_shutdown(request.ready, request.commit)
            )
            .await
            .expect("the refusal path must not wait on the commit"),
            "a scheduled goal continuation refuses retirement"
        );
        assert!(!receipt.await.unwrap());
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_actor_abort_fails_the_host_commit_send() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        let (ready, receipt) = tokio::sync::oneshot::channel();
        let (commit, committed) = tokio::sync::oneshot::channel();
        let accept =
            tokio::spawn(async move { engine.accept_idle_shutdown(ready, committed).await });
        assert!(
            receipt.await.unwrap(),
            "a clean fixture must offer retirement"
        );
        // The actor incarnation dies mid-handshake: the engine and the commit
        // receiver it owns drop with the aborted task. Awaiting the handle
        // makes the drop deterministic before the host-side send.
        accept.abort();
        let _ = accept.await;
        assert!(
            commit.send(()).is_err(),
            "the host must see the proposal expire instead of a silent no-op"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_fresh_cancel_escapes_a_wedged_commit_wait() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, handle) = fixture(home.path());
        let (ready, receipt) = tokio::sync::oneshot::channel();
        let (commit, committed) = tokio::sync::oneshot::channel();
        let pending = tokio::spawn(async move {
            let accepted = tokio::time::timeout(
                Duration::from_secs(5),
                engine.accept_idle_shutdown(ready, committed),
            )
            .await
            .expect("a fresh cancel must break the commit wait");
            (accepted, engine)
        });
        assert!(receipt.await.unwrap());
        // The host cancels while its commit is still outstanding: the actor
        // must resume instead of waiting forever on the commit.
        handle.cancel();
        let (accepted, mut engine) = pending.await.unwrap();
        assert!(!accepted, "the cancel escape must refuse, not accept");
        assert!(engine.cancel_token.is_cancelled());
        drop(commit);
        assert!(
            engine.can_retire_without_losing_work(),
            "the refused proposal leaves the actor usable"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_stale_cancelled_turn_does_not_refuse_retirement() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, handle) = fixture(home.path());
        // Latch a cancellation from a (synthetic) earlier turn: the idle slot
        // keeps that token until the next turn swaps it.
        handle.cancel();
        assert!(engine.cancel_token.is_cancelled());
        let (ready, receipt) = tokio::sync::oneshot::channel();
        let (commit, committed) = tokio::sync::oneshot::channel();
        let pending = tokio::spawn(async move {
            let accepted = tokio::time::timeout(
                Duration::from_secs(5),
                engine.accept_idle_shutdown(ready, committed),
            )
            .await
            .expect("a stale cancelled token must not wedge the proposal");
            (accepted, engine)
        });
        assert!(
            receipt.await.unwrap(),
            "a latched cancellation must not refuse the proposal itself"
        );
        commit.send(()).unwrap();
        let (accepted, mut engine) = pending.await.unwrap();
        assert!(
            accepted,
            "commit must win over the already-fired token, or a committed host is silently refused"
        );
        assert!(
            engine.can_retire_without_losing_work(),
            "the accepted proposal leaves nothing stuck"
        );
    }
}
