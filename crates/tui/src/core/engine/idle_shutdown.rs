//! Actor-owned, non-destructive retirement for embedding hosts. The host owns
//! external admission; this boundary owns autonomous turns and their evidence.

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
        tokio::select! {
            op = self.rx_op.recv() => op.map(|op| EngineRunInput::Operation(Box::new(op))),
            Some(request) = self.rx_idle_retirement.recv() => Some(EngineRunInput::IdleRetirement(request)),
        }
    }

    fn can_retire_without_losing_work(&mut self) -> bool {
        if self.has_scheduled_goal_continuation()
            // Only the actor creates watcher clones. Each watcher holds its
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
        // The actor is the only remaining autonomous work producer, and the
        // embedding host has already excluded new external submissions.
        self.rx_op.is_empty()
            && self.rx_subagent_completion.is_empty()
            && self.rx_steer.is_empty()
            && self.pending_steers.is_empty()
            && self.rx_approval.is_empty()
            && self.rx_user_input.is_empty()
            && self.tx_event.capacity() == self.tx_event.max_capacity()
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
        let idle = self.can_retire_without_losing_work();
        if ready.send(idle).is_err() || !idle {
            return false;
        }
        commit.await.is_ok()
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
        assert!(!engine.accept_idle_shutdown(ready, committed).await);
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
            !engine
                .accept_idle_shutdown(request.ready, request.commit)
                .await
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
            engine
                .accept_idle_shutdown(request.ready, request.commit)
                .await
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
            let accepted = engine.accept_idle_shutdown(ready, committed).await;
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
}
