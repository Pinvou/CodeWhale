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
    /// The actor acknowledges only a *consumed* commit, so the host can tell
    /// a committed retirement from a send that raced a simultaneous cancel.
    pub(super) commit_ack: tokio::sync::oneshot::Sender<()>,
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
        // try_lock keeps this gate non-blocking like the sibling checks: the
        // goal state is shared with the host, so a host holding it across
        // this call must defer retirement instead of deadlocking the actor
        // before the readiness receipt is even sent. Uncertain is refuse.
        if self
            .config
            .goal_state
            .try_lock()
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

    /// Snapshot work visible after the sealed commit wait. Autonomous shell
    /// evidence and events can arrive there too, without an external send.
    /// Event slots include reserved permits; shell lock contention is unknown
    /// work, so report it conservatively. This does not observe arbitrary host
    /// shared-state writes or arrivals after the snapshot.
    pub(super) fn retirement_residuals(&mut self) -> (usize, usize, bool) {
        let inputs = self.rx_op.len()
            + self.rx_subagent_completion.len()
            + self.rx_steer.len()
            + self.rx_approval.len()
            + self.rx_user_input.len()
            + self.pending_steers.len();
        let event_slots = self.tx_event.max_capacity() - self.tx_event.capacity();
        let shell_work = self.shell_manager.try_lock().map_or(true, |mut shells| {
            shells.has_live_or_unreported_work_for_session(&self.session.id)
        });
        (inputs, event_slots, shell_work)
    }

    pub(super) async fn accept_idle_shutdown(&mut self, request: IdleRetirement) -> bool {
        let IdleRetirement {
            ready,
            commit,
            commit_ack,
        } = request;
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
        // fired *during* the proposal break the wait: on a fresh token, a host
        // blocked forever inside its synchronous predicate must not pin the
        // actor. A token already latched by an earlier cancelled turn does not
        // escape — that stale state must not make retirement permanently
        // unavailable, and here it deliberately trades actor liveness for it:
        // with the escape disarmed, only the commit itself can break a
        // blocked predicate. Commit still wins a simultaneous cancel: a host
        // send that lands while the cancel arm resolves can return `Ok`
        // without being seen, so `Ok(true)` requires the commit ack, never
        // the send alone.
        let wait_cancel = self.cancel_token.clone();
        let committed = tokio::select! {
            biased;
            result = commit => {
                // Acknowledge only a consumed commit: the ack, not the send
                // receipt, is what the host treats as "committed".
                let ok = result.is_ok();
                if ok {
                    let _ = commit_ack.send(());
                }
                ok
            }
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
    async fn forkguard_idle_shutdown_sealed_wait_reports_autonomous_work() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        assert_eq!(engine.retirement_residuals(), (0, 0, false));
        let shells = Arc::clone(&engine.shell_manager);
        let completion = engine.tx_subagent_completion.clone();
        let events = engine.tx_event.clone();
        let session = engine.session.id.clone();
        let (ready, receipt) = tokio::sync::oneshot::channel();
        let (commit, committed) = tokio::sync::oneshot::channel();
        let (commit_ack, ack) = tokio::sync::oneshot::channel();
        let pending = tokio::spawn(async move {
            let accepted = engine
                .accept_idle_shutdown(IdleRetirement {
                    ready,
                    commit: committed,
                    commit_ack,
                })
                .await;
            (accepted, engine)
        });
        assert!(receipt.await.unwrap());
        // The actor is sealed between ready and commit, not draining wakes.
        shells
            .lock()
            .unwrap()
            .seed_session_shell_for_test("late-shell", &session, false);
        completion
            .send(SubAgentCompletion {
                owner_session_id: session,
                agent_id: "late-child".into(),
                payload: "late completion".into(),
            })
            .unwrap();
        events
            .try_send(Event::SteerDropped {
                steer_id: "late-event".into(),
            })
            .unwrap();
        commit.send(()).unwrap();
        ack.await.unwrap();
        let (accepted, mut engine) = tokio::time::timeout(Duration::from_secs(10), pending)
            .await
            .unwrap()
            .unwrap();
        assert!(accepted);
        assert_eq!(engine.retirement_residuals(), (1, 1, true));
        assert_eq!(
            engine.rx_subagent_completion.try_recv().unwrap().payload,
            "late completion"
        );
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
        let (commit_ack, _ack) = tokio::sync::oneshot::channel();
        // Timeout-wrapped so a regression that wedges the handshake fails the
        // assertion instead of hanging the whole test process. The budgets in
        // this module are generous wall-clock headroom for loaded machines,
        // not timing assertions.
        assert!(
            !tokio::time::timeout(
                Duration::from_secs(10),
                engine.accept_idle_shutdown(IdleRetirement {
                    ready,
                    commit: committed,
                    commit_ack,
                })
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
                Duration::from_secs(10),
                engine.accept_idle_shutdown(request)
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
                Duration::from_secs(10),
                engine.accept_idle_shutdown(request)
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
        let (commit_ack, _ack) = tokio::sync::oneshot::channel();
        let pending = tokio::spawn(async move {
            let accepted = tokio::time::timeout(
                Duration::from_secs(10),
                engine.accept_idle_shutdown(IdleRetirement {
                    ready,
                    commit: committed,
                    commit_ack,
                }),
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
        let (commit_ack, _ack) = tokio::sync::oneshot::channel();
        handle
            .idle_retirement
            .as_ref()
            .unwrap()
            .send(IdleRetirement {
                ready,
                commit: committed,
                commit_ack,
            })
            .await
            .unwrap();
        // The biased timed wait for the goal continuation must serve the
        // retirement arm instead of sleeping through the whole window.
        let input = tokio::time::timeout(Duration::from_millis(2000), engine.next_run_input(false))
            .await
            .expect("the armed retirement request must not wait out the goal timer");
        let Some(EngineRunInput::IdleRetirement(request)) = input else {
            panic!("the retirement arm must fire while the goal wait is armed");
        };
        assert!(
            !tokio::time::timeout(
                Duration::from_secs(10),
                engine.accept_idle_shutdown(request)
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
        let (commit_ack, _ack) = tokio::sync::oneshot::channel();
        let accept = tokio::spawn(async move {
            engine
                .accept_idle_shutdown(IdleRetirement {
                    ready,
                    commit: committed,
                    commit_ack,
                })
                .await
        });
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
        let (commit_ack, ack) = tokio::sync::oneshot::channel();
        let pending = tokio::spawn(async move {
            let accepted = tokio::time::timeout(
                Duration::from_secs(10),
                engine.accept_idle_shutdown(IdleRetirement {
                    ready,
                    commit: committed,
                    commit_ack,
                }),
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
        assert!(
            ack.await.is_err(),
            "an escaped proposal must not be acknowledged as committed"
        );
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
        let (commit_ack, ack) = tokio::sync::oneshot::channel();
        let pending = tokio::spawn(async move {
            let accepted = tokio::time::timeout(
                Duration::from_secs(10),
                engine.accept_idle_shutdown(IdleRetirement {
                    ready,
                    commit: committed,
                    commit_ack,
                }),
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
        assert!(
            ack.await.is_ok(),
            "a consumed commit must be acknowledged to the host"
        );
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

    #[tokio::test]
    async fn forkguard_idle_shutdown_queued_operation_wins_the_retirement_arbitration() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, handle) = fixture(home.path());
        handle
            .send(Op::SetAdvisorEnabled { enabled: false })
            .await
            .unwrap();
        let (ready, _receipt) = tokio::sync::oneshot::channel();
        let (_commit, committed) = tokio::sync::oneshot::channel();
        let (commit_ack, _ack) = tokio::sync::oneshot::channel();
        handle
            .idle_retirement
            .as_ref()
            .unwrap()
            .send(IdleRetirement {
                ready,
                commit: committed,
                commit_ack,
            })
            .await
            .unwrap();
        // Both inputs are queued before the drain runs, so the biased arm
        // order — not luck — must decide: the FIFO drain site serves the
        // operation first and still reaches the retirement request after.
        let first = tokio::time::timeout(
            Duration::from_millis(2000),
            engine.next_operation_or_retirement(),
        )
        .await
        .expect("both inputs are queued; the drain must resolve");
        assert!(
            matches!(first, Some(EngineRunInput::Operation(_))),
            "a queued operation must not lose the arbitration race to a retirement request"
        );
        let second = tokio::time::timeout(
            Duration::from_millis(2000),
            engine.next_operation_or_retirement(),
        )
        .await
        .expect("the retirement request must follow the queued operation");
        assert!(
            matches!(second, Some(EngineRunInput::IdleRetirement(_))),
            "the retirement request must still be served after the op"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_parked_child_queued_mail_gates_retirement() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        let child = {
            let mut agents = engine.subagent_manager.write().await;
            let (child, _continuation) = agents.insert_test_interrupted_continuable_agent(
                "parked",
                home.path(),
                vec![crate::models::Message {
                    role: crate::models::Role::User,
                    content: vec![crate::models::ContentBlock::Text {
                        text: "recoverable work".into(),
                        cache_control: None,
                    }],
                }],
            );
            agents.assign_test_session_owner(&child, &engine.session.id);
            child
        };
        // A follow-up accepted for a parked child lands in the in-memory
        // mailbox with a queue-only receipt. It is accepted work: the
        // retirement gate must refuse until the owner session drains it.
        {
            let mut agents = engine.subagent_manager.write().await;
            let receipt = agents
                .followup_child(&child, "accepted while parked".to_string())
                .expect("queue-only follow-up receipt");
            assert!(!receipt.woke, "a parked child cannot be woken");
        }
        assert!(
            !engine.can_retire_without_losing_work(),
            "queued parent mail for a parked child must survive retirement"
        );
        {
            let mut agents = engine.subagent_manager.write().await;
            agents.clear_test_queued_mail(&child);
        }
        assert!(
            engine.can_retire_without_losing_work(),
            "draining the parked mail restores the idle certification"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_delivered_follow_up_counter_tracks_live_children_only() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        // Hold the receiver for the whole test: dropping it would close the
        // child's input channel and turn the wake delivery into a failure.
        let (_child_rx, child) = {
            let mut agents = engine.subagent_manager.write().await;
            let (child, rx) = agents.insert_test_running_agent_with_input("countered", home.path());
            agents.assign_test_session_owner(&child, &engine.session.id);
            (rx, child)
        };
        {
            let mut agents = engine.subagent_manager.write().await;
            let receipt = agents
                .followup_child(&child, "delivered to a live child".to_string())
                .expect("wake receipt");
            assert!(
                receipt.woke,
                "a running child with a live input channel wakes"
            );
        }
        assert!(
            !engine.can_retire_without_losing_work(),
            "a delivered follow-up the child has not consumed is accepted work"
        );
        {
            let mut agents = engine.subagent_manager.write().await;
            agents.commit_test_terminal_result(
                &child,
                crate::tools::subagent::SubAgentStatus::Cancelled,
            );
        }
        assert!(
            engine.can_retire_without_losing_work(),
            "a terminal child can never consume its counter: retirement must not refuse on bookkeeping no drain can clear"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_mail_on_a_dead_end_child_does_not_gate_retirement() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        let child = {
            let mut agents = engine.subagent_manager.write().await;
            let (child, _continuation) = agents.insert_test_interrupted_continuable_agent(
                "dead-end",
                home.path(),
                vec![crate::models::Message {
                    role: crate::models::Role::User,
                    content: vec![crate::models::ContentBlock::Text {
                        text: "recoverable work".into(),
                        cache_control: None,
                    }],
                }],
            );
            agents.assign_test_session_owner(&child, &engine.session.id);
            child
        };
        {
            let mut agents = engine.subagent_manager.write().await;
            agents
                .followup_child(&child, "queued for a resumable child".to_string())
                .expect("queue-only follow-up receipt");
        }
        assert!(
            !engine.can_retire_without_losing_work(),
            "mail a resume could still deliver is accepted work"
        );
        {
            let mut agents = engine.subagent_manager.write().await;
            agents.mark_test_agent_failed(&child);
        }
        assert!(
            engine.can_retire_without_losing_work(),
            "followup refuses a failed child, so its parked mail has no remaining delivery path and must not gate forever"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_child_owned_shell_evidence_does_not_block_retirement() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        let session = engine.session.id.clone();
        {
            let mut shells = engine.shell_manager.lock().unwrap();
            shells.seed_agent_shell_for_test("child-job", &session, "agent_child", true);
        }
        assert!(
            !engine.can_retire_without_losing_work(),
            "a running agent-owned shell is live work regardless of owner"
        );
        {
            let mut shells = engine.shell_manager.lock().unwrap();
            shells.finish_seeded_shell_for_test("child-job");
        }
        assert!(
            engine.can_retire_without_losing_work(),
            "the parent stream suppresses child-owned completions and a terminal owner never claims them, so finished child-owned evidence must not gate"
        );
        {
            let mut shells = engine.shell_manager.lock().unwrap();
            shells.seed_session_shell_for_test("parent-job", &session, false);
        }
        assert!(
            !engine.can_retire_without_losing_work(),
            "parent-owned finished-unreported evidence must still gate retirement"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_active_goal_state_gates_retirement() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        {
            let mut state = engine.config.goal_state.lock().expect("goal state");
            state.sync_from_host_status(
                Some("retirement-goal"),
                None,
                crate::tools::goal::GoalStatus::Active,
            );
        }
        assert!(
            !engine.can_retire_without_losing_work(),
            "an active goal state is a continuation the host has not observed"
        );
        {
            let mut state = engine.config.goal_state.lock().expect("goal state");
            state.sync_from_host_status(
                Some("retirement-goal"),
                None,
                crate::tools::goal::GoalStatus::Complete,
            );
        }
        assert!(
            engine.can_retire_without_losing_work(),
            "a completed goal state no longer holds accepted work"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_empty_interrupt_checkpoint_does_not_gate_forever() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        {
            let mut agents = engine.subagent_manager.write().await;
            let (child, _rx) = agents.insert_test_running_agent_with_input("early", home.path());
            agents.assign_test_session_owner(&child, &engine.session.id);
            let (_, interrupted) = agents
                .interrupt_child(&child, None, "before first checkpoint".into())
                .unwrap();
            assert!(interrupted.checkpoint.as_ref().unwrap().messages.is_empty());
            let receipt = agents
                .followup_child(&child, "no resume route".into())
                .unwrap();
            assert!(!receipt.woke);
            assert!(receipt.continuation_handle.is_none());
        }
        assert!(
            engine.can_retire_without_losing_work(),
            "an empty placeholder is not a usable resume route"
        );
    }

    #[tokio::test]
    async fn forkguard_idle_shutdown_goal_lock_contention_retries_after_release() {
        let home = tempfile::tempdir().unwrap();
        let (mut engine, _handle) = fixture(home.path());
        let goal = Arc::clone(&engine.config.goal_state);
        let held = goal.lock().unwrap();
        assert!(
            !engine.can_retire_without_losing_work(),
            "an unknown goal state must refuse retirement"
        );
        drop(held);
        assert!(
            engine.can_retire_without_losing_work(),
            "contention must not latch the gate closed"
        );
    }
}
