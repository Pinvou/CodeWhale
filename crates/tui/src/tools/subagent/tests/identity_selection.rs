use super::*;
use std::future::Future;
use std::task::Poll;

fn alias_chain(
    manager: &mut SubAgentManager,
    workspace: &Path,
) -> (
    String,
    String,
    String,
    mpsc::UnboundedReceiver<SubAgentInput>,
) {
    let (a, receiver) = manager.insert_test_running_agent_with_input("7e2950c4", workspace);
    let b = manager.insert_test_running_agent("9f83d2a1", workspace);
    let c = manager.insert_test_running_agent("86f4c3d2", workspace);
    manager.agents.get_mut(&a).unwrap().session_name = "alpha".into();
    manager.agents.get_mut(&b).unwrap().session_name = a.clone();
    manager.agents.get_mut(&c).unwrap().session_name = b.clone();
    (a, b, c, receiver)
}

#[tokio::test]
async fn forkguard_agent_identity_mail_wake_summary_and_interrupt_use_one_target() {
    let tmp = tempdir().unwrap();
    let mut manager = SubAgentManager::new(tmp.path().into(), 8);
    let (a, b, c, mut receiver) = alias_chain(&mut manager, tmp.path());
    let first = manager
        .queue_parent_message("alpha", "queued".into(), false)
        .unwrap();
    assert_eq!(first.agent_id, a);
    let second = manager
        .queue_running_parent_message_for_session("workspace", "alpha", "scoped".into())
        .unwrap();
    assert_eq!(second.agent_id, a);
    let summary = manager
        .coordination_summary_for_session("workspace", &a, 8)
        .unwrap();
    assert_eq!(summary.agent_id, a);
    assert_eq!(summary.queued_mail, 2);
    assert_eq!(manager.queued_mail_depth(&b), None);
    let receipt = manager
        .followup_child_for_session("workspace", "alpha", "wake".into())
        .unwrap();
    assert_eq!(receipt.agent_id, a);
    assert!(receipt.woke);
    for expected in ["queued", "scoped", "wake"] {
        assert_eq!(receiver.try_recv().unwrap().text, expected);
    }
    assert!(receiver.try_recv().is_err());
    assert_eq!(manager.queued_mail_depth(&b), None);
    let (prior, after) = manager
        .interrupt_child_for_session("workspace", "alpha", None, "pause".into())
        .unwrap();
    assert_eq!(prior.agent_id, a);
    assert_eq!(after.agent_id, a);
    assert!(matches!(after.status, SubAgentStatus::Interrupted(_)));
    for id in [&b, &c] {
        assert_eq!(
            manager.get_result(id).unwrap().status,
            SubAgentStatus::Running
        );
    }
    manager
        .queue_running_parent_message_for_session("workspace", &b, "next legal call".into())
        .unwrap();
    assert_eq!(manager.queued_mail_depth(&b), Some(1));
}

#[tokio::test]
async fn forkguard_agent_identity_public_old_id_and_explicit_name_are_distinct() {
    let tmp = tempdir().unwrap();
    let mut inner = SubAgentManager::new(tmp.path().into(), 8);
    let (a, b, c, _receiver) = alias_chain(&mut inner, tmp.path());
    inner.agents.get_mut(&a).unwrap().status = SubAgentStatus::Completed;
    let manager = Arc::new(tokio::sync::RwLock::new(inner));
    let tool = AgentTool::new(manager.clone(), stub_runtime());
    let context = ToolContext::new(tmp.path());
    tool.execute(json!({"action":"cancel", "agent_id":a}), &context)
        .await
        .unwrap();
    assert_eq!(
        manager.read().await.get_result(&b).unwrap().status,
        SubAgentStatus::Running
    );
    // The explicit name selects B even though it looks exactly like A's id.
    tool.execute(json!({"action":"cancel", "name":a}), &context)
        .await
        .unwrap();
    assert_eq!(
        manager.read().await.get_result(&b).unwrap().status,
        SubAgentStatus::Cancelled
    );
    assert_eq!(
        manager.read().await.get_result(&c).unwrap().status,
        SubAgentStatus::Running
    );
    // Ordinary, unambiguous legacy aliases are still accepted, including a
    // prefix that is not the eight-hex issued-ID syntax.
    let ordinary = manager
        .write()
        .await
        .insert_test_running_agent("plain_retry", tmp.path());
    tool.execute(
        json!({"action":"cancel", "agent_id":"plain_retry"}),
        &context,
    )
    .await
    .unwrap();
    assert_eq!(
        manager.read().await.get_result(&ordinary).unwrap().status,
        SubAgentStatus::Cancelled
    );
    let prefixed = {
        let mut guard = manager.write().await;
        let id = guard.insert_test_running_agent("d1e2f3a4", tmp.path());
        guard.agents.get_mut(&id).unwrap().session_name = "agent_reviewer".into();
        id
    };
    tool.execute(
        json!({"action":"cancel","agent_id":"agent_reviewer"}),
        &context,
    )
    .await
    .unwrap();
    assert_eq!(
        manager.read().await.get_result(&prefixed).unwrap().status,
        SubAgentStatus::Cancelled
    );
    assert_eq!(
        manager.read().await.get_result(&c).unwrap().status,
        SubAgentStatus::Running
    );
}

#[tokio::test]
async fn forkguard_agent_identity_foreign_and_evicted_ids_never_fall_back_to_names() {
    let tmp = tempdir().unwrap();
    let mut inner = SubAgentManager::new(tmp.path().into(), 8);
    let (a, b, _c, _receiver) = alias_chain(&mut inner, tmp.path());
    inner.assign_test_session_owner(&a, "foreign");
    let manager = Arc::new(tokio::sync::RwLock::new(inner));
    let tool = AgentTool::new(manager.clone(), stub_runtime());
    let context = ToolContext::new(tmp.path());
    assert!(
        tool.execute(json!({"action":"cancel", "agent_id":a}), &context)
            .await
            .is_err()
    );
    assert_eq!(
        manager.read().await.get_result(&b).unwrap().status,
        SubAgentStatus::Running
    );
    {
        let mut guard = manager.write().await;
        guard.agents.remove(&a);
        guard.worker_records.remove(&a);
        guard.resume_targets.remove(&a);
    }
    assert!(
        tool.execute(json!({"action":"cancel", "id":a}), &context)
            .await
            .is_err()
    );
    tool.execute(json!({"action":"cancel", "session_name":a}), &context)
        .await
        .unwrap();
    assert_eq!(
        manager.read().await.get_result(&b).unwrap().status,
        SubAgentStatus::Cancelled
    );
}

#[tokio::test]
async fn forkguard_agent_identity_bulk_stop_keeps_foreign_alias_holder_running() {
    let tmp = tempdir().unwrap();
    let mut manager = SubAgentManager::new(tmp.path().into(), 8);
    let (a, b, c, _receiver) = alias_chain(&mut manager, tmp.path());
    manager.assign_test_session_owner(&c, "foreign");
    assert_eq!(manager.cancel_all_running_for_session("workspace"), 2);
    for id in [&a, &b] {
        assert_eq!(
            manager.get_result(id).unwrap().status,
            SubAgentStatus::Cancelled
        );
    }
    assert_eq!(
        manager.get_result(&c).unwrap().status,
        SubAgentStatus::Running
    );
    assert_eq!(manager.cancel_all_running_for_session("workspace"), 0);
}

#[tokio::test]
async fn forkguard_agent_identity_hierarchy_checks_the_same_object_it_cancels() {
    let tmp = tempdir().unwrap();
    let mut inner = SubAgentManager::new(tmp.path().into(), 8);
    let (a, b, _c, _receiver) = alias_chain(&mut inner, tmp.path());
    let caller = inner.insert_test_running_agent("3a1f8c2d", tmp.path());
    let record = inner.worker_records.get_mut(&a).unwrap();
    record.parent_run_id = Some(caller.clone());
    record.spec.parent_run_id = Some(caller.clone());
    let manager = Arc::new(tokio::sync::RwLock::new(inner));
    let context = ToolContext::new(tmp.path());
    // The name selects the sibling, so it must fail before any mutation.
    assert!(
        cancel_agent_from_input(&json!({"name":a}), manager.clone(), &context, Some(&caller))
            .await
            .is_err()
    );
    assert_eq!(
        manager.read().await.get_result(&b).unwrap().status,
        SubAgentStatus::Running
    );
    // The old receipt id selects the caller's actual descendant.
    cancel_agent_from_input(
        &json!({"agent_id":a}),
        manager.clone(),
        &context,
        Some(&caller),
    )
    .await
    .unwrap();
    assert_eq!(
        manager.read().await.get_result(&a).unwrap().status,
        SubAgentStatus::Cancelled
    );
    assert_eq!(
        manager.read().await.get_result(&b).unwrap().status,
        SubAgentStatus::Running
    );
}

#[tokio::test]
async fn forkguard_agent_identity_all_wait_faces_keep_the_captured_id_after_name_reuse() {
    for until in ["completion", "all", "activity"] {
        let tmp = tempdir().unwrap();
        let mut inner = SubAgentManager::new(tmp.path().into(), 8);
        let (a, _receiver) = inner.insert_test_running_agent_with_input("7e2950c4", tmp.path());
        inner.agents.get_mut(&a).unwrap().session_name = "alpha".into();
        let manager = Arc::new(tokio::sync::RwLock::new(inner));
        let tool = AgentTool::new(manager.clone(), stub_runtime());
        let context = ToolContext::new(tmp.path());
        let mut waiting = Box::pin(tool.execute(
            json!({"action":"wait", "name":"alpha", "until":until, "timeout_secs":1}),
            &context,
        ));
        // Poll through watch-set capture and the first pending timer. No sleep
        // guesses or test-only hooks replace the production wait path.
        let first = std::future::poll_fn(|cx| Poll::Ready(waiting.as_mut().poll(cx))).await;
        assert!(
            first.is_pending(),
            "{until} must wait on the original live child"
        );
        let b = {
            let mut guard = manager.write().await;
            let b = guard.insert_test_running_agent("9f83d2a1", tmp.path());
            guard.agents.get_mut(&b).unwrap().session_name = a.clone();
            guard.agents.get_mut(&a).unwrap().status = SubAgentStatus::Completed;
            b
        };
        let result = tokio::time::timeout(Duration::from_secs(3), waiting)
            .await
            .unwrap()
            .unwrap();
        let payload: Value = serde_json::from_str(&result.content).unwrap();
        assert_eq!(payload["timed_out"], false, "{until}: {payload}");
        assert_eq!(payload["settled"][0]["agent_id"], a, "{until}: {payload}");
        assert_eq!(
            manager.read().await.get_result(&b).unwrap().status,
            SubAgentStatus::Running
        );
    }
}

#[tokio::test]
async fn forkguard_agent_identity_user_continue_wakes_the_receipted_child() {
    let tmp = tempdir().unwrap();
    let mut inner = SubAgentManager::new(tmp.path().into(), 8);
    let (a, b, _c, mut receiver) = alias_chain(&mut inner, tmp.path());
    let manager = Arc::new(tokio::sync::RwLock::new(inner));
    let outcome = manager
        .write()
        .await
        .continue_child_from_user_for_session(
            "workspace",
            manager.clone(),
            None,
            "alpha",
            "continue A",
        )
        .unwrap();
    assert_eq!(outcome.agent_id, a);
    assert_eq!(outcome.target_agent_id, a);
    assert!(outcome.delivered);
    assert_eq!(receiver.try_recv().unwrap().text, "continue A");
    assert_eq!(manager.read().await.queued_mail_depth(&b), None);
}

#[tokio::test]
async fn forkguard_agent_identity_checkpoint_retry_keeps_old_receipt_and_resumed_id() {
    let tmp = tempdir().unwrap();
    let manager = new_shared_subagent_manager(tmp.path().into(), 8);
    let mut runtime = stub_runtime();
    runtime.manager = manager.clone();
    let mut guard = manager.write().await;
    let (a, _handle) = guard.insert_test_interrupted_continuable_agent(
        "paused_original",
        tmp.path(),
        vec![Message {
            role: Role::User,
            content: vec![ContentBlock::Text {
                text: "original A checkpoint".into(),
                cache_control: None,
            }],
        }],
    );
    guard.assign_test_session_owner(&a, "workspace");
    let b = guard.insert_test_running_agent("9f83d2a1", tmp.path());
    guard.agents.get_mut(&b).unwrap().session_name = a.clone();
    let resumed = guard
        .resume_from_checkpoint_for_session(
            "workspace",
            manager.clone(),
            runtime.clone(),
            &a,
            "continue A",
        )
        .unwrap();
    assert_ne!(resumed.agent_id, a);
    assert_ne!(resumed.agent_id, b);
    assert!(resumed.continued_from_checkpoint);
    assert!(
        guard.agents[&resumed.agent_id]
            .prompt
            .contains("original A checkpoint")
    );
    let c = guard.insert_test_running_agent("86f4c3d2", tmp.path());
    guard.agents.get_mut(&c).unwrap().session_name = resumed.agent_id.clone();
    let again = guard
        .resume_from_checkpoint_for_session(
            "workspace",
            manager.clone(),
            runtime.clone(),
            &a,
            "retry same resume",
        )
        .unwrap();
    assert_eq!(again.agent_id, resumed.agent_id);
    assert!(!again.continued_from_checkpoint);
    assert!(matches!(
        guard.get_result(&a).unwrap().status,
        SubAgentStatus::Interrupted(_)
    ));
    assert_eq!(
        guard.get_result(&b).unwrap().status,
        SubAgentStatus::Running
    );
    assert_eq!(
        guard.get_result(&c).unwrap().status,
        SubAgentStatus::Running
    );
    assert_eq!(guard.queued_mail_depth(&c), None);

    // The existing successor remains the target even when its live receiver
    // has closed before terminal publication. Do not fabricate a wake from a
    // successful snapshot lookup. No provider is run by this fixture.
    let target = resumed.agent_id;
    if let Some(task) = guard.agents.get_mut(&target).unwrap().task_handle.take() {
        task.abort();
    }
    let (closed_tx, closed_rx) = mpsc::unbounded_channel();
    drop(closed_rx);
    guard.agents.get_mut(&target).unwrap().input_tx = Some(closed_tx);
    guard.agents.get_mut(&target).unwrap().task_handle = Some(tokio::spawn(std::future::pending()));
    drop(guard);
    let context = ToolContext::new(tmp.path());
    let public = AgentTool::new(manager.clone(), runtime.clone());
    let hidden = coord::AgentsFollowupTool::new(manager.clone()).with_runtime(runtime.clone());
    for (surface, text) in [(0, "public closed"), (1, "hidden closed")] {
        let input = json!({"action":"followup","agent_id":a,"message":text});
        let result = if surface == 0 {
            public.execute(input, &context).await
        } else {
            hidden.execute(input, &context).await
        }
        .unwrap();
        let receipt: Value = serde_json::from_str(&result.content).unwrap();
        assert_eq!(receipt["agent_id"], target);
        assert_eq!(receipt["woke"], false);
        assert_eq!(receipt["continued_from_checkpoint"], false);
        assert_eq!(receipt["queue_depth"], surface + 1);
        let status = public
            .execute(json!({"action":"status","agent_id":target}), &context)
            .await
            .unwrap();
        let projection: Value = serde_json::from_str(&status.content).unwrap();
        let delivery = &projection["follow_up"]["latest_delivery"];
        assert_eq!(delivery["delivered"], false);
        assert_eq!(delivery["message_preview"], text);
        assert!(
            delivery["reason"]
                .as_str()
                .is_some_and(|note| note.contains("closed"))
        );
    }
    let outcome = manager
        .write()
        .await
        .continue_child_from_user_for_session(
            "workspace",
            manager.clone(),
            Some(runtime.clone()),
            &a,
            "operator closed",
        )
        .unwrap();
    assert_eq!(outcome.target_agent_id, target);
    assert!(!outcome.delivered);
    assert!(!outcome.resumed);
    let (tx, mut rx) = mpsc::unbounded_channel();
    manager
        .write()
        .await
        .agents
        .get_mut(&target)
        .unwrap()
        .input_tx = Some(tx);
    let result = public
        .execute(
            json!({"action":"followup","agent_id":a,"message":"healthy next"}),
            &context,
        )
        .await
        .unwrap();
    let receipt: Value = serde_json::from_str(&result.content).unwrap();
    assert_eq!(receipt["woke"], true);
    assert_eq!(receipt["queue_depth"], 0);
    for text in [
        "public closed",
        "hidden closed",
        "operator closed",
        "healthy next",
    ] {
        assert_eq!(rx.try_recv().unwrap().text, text);
    }
    assert!(rx.try_recv().is_err());
    let status = public
        .execute(json!({"action":"status","agent_id":target}), &context)
        .await
        .unwrap();
    let projection: Value = serde_json::from_str(&status.content).unwrap();
    let delivery = &projection["follow_up"]["latest_delivery"];
    assert_eq!(delivery["delivered"], true);
    assert_eq!(delivery["message_preview"], "healthy next");
    assert!(delivery["reason"].is_null());
    {
        let mut guard = manager.write().await;
        let mut terminal = guard.get_result(&target).unwrap();
        terminal.status = SubAgentStatus::Completed;
        assert!(guard.finish_terminal_result(&target, terminal, true, false));
    }
    let rejected = json!({"action":"followup","agent_id":a,"message":"must not disappear"});
    assert!(public.execute(rejected.clone(), &context).await.is_err());
    assert!(hidden.execute(rejected, &context).await.is_err());
    let mut guard = manager.write().await;
    assert!(
        guard
            .continue_child_from_user_for_session(
                "workspace",
                manager.clone(),
                Some(runtime),
                &a,
                "operator terminal"
            )
            .is_err()
    );
    assert_eq!(guard.resume_targets.get(&a), Some(&target));
    assert!(matches!(
        guard.get_result(&a).unwrap().status,
        SubAgentStatus::Interrupted(_)
    ));
    assert_eq!(
        guard.get_result(&c).unwrap().status,
        SubAgentStatus::Running
    );
    assert_eq!(guard.queued_mail_depth(&c), None);
}

#[tokio::test]
async fn forkguard_agent_identity_wait_reports_real_retention_as_gone() {
    for until in ["completion", "all", "activity"] {
        for another_child in [false, true] {
            let tmp = tempdir().unwrap();
            let mut inner = SubAgentManager::new(tmp.path().into(), 8);
            let a = inner.insert_test_running_agent("7e2950c4", tmp.path());
            // Long-running but recently active work is legitimate. Only the
            // age is controlled; the real terminal arbiter and cleanup run.
            inner.agents.get_mut(&a).unwrap().started_at =
                Instant::now() - Duration::from_secs(3601);
            let manager = Arc::new(tokio::sync::RwLock::new(inner));
            let tool = AgentTool::new(manager.clone(), stub_runtime());
            let context = ToolContext::new(tmp.path());
            let mut waiting = Box::pin(tool.execute(
                json!({"action":"wait","agent_id":a,"until":until,"timeout_secs":1}),
                &context,
            ));
            assert!(
                std::future::poll_fn(|cx| Poll::Ready(waiting.as_mut().poll(cx)))
                    .await
                    .is_pending()
            );
            let b = {
                let mut guard = manager.write().await;
                let mut terminal = guard.get_result(&a).unwrap();
                terminal.status = SubAgentStatus::Completed;
                assert!(guard.finish_terminal_result(&a, terminal, true, false));
                let b = another_child.then(|| {
                    let b = guard.insert_test_running_agent("9f83d2a1", tmp.path());
                    guard.agents.get_mut(&b).unwrap().session_name = a.clone();
                    b
                });
                guard.cleanup_for_session("workspace", COMPLETED_AGENT_RETENTION);
                assert!(!guard.agents.contains_key(&a));
                b
            };
            let result = tokio::time::timeout(Duration::from_secs(3), waiting)
                .await
                .unwrap()
                .unwrap();
            let payload: Value = serde_json::from_str(&result.content).unwrap();
            assert_eq!(payload["timed_out"], false, "{until}: {payload}");
            assert_eq!(payload["settled"][0]["agent_id"], a, "{until}: {payload}");
            assert_eq!(
                payload["settled"][0]["status"], "gone",
                "{until}: {payload}"
            );
            if let Some(b) = b {
                assert_eq!(
                    manager.read().await.get_result(&b).unwrap().status,
                    SubAgentStatus::Running
                );
                assert!(
                    tool.execute(json!({"action":"status","agent_id":b}), &context)
                        .await
                        .is_ok()
                );
            }
        }
    }
}

#[tokio::test]
async fn forkguard_agent_identity_bad_inspect_keeps_cleanup_for_the_next_valid_call() {
    let tmp = tempdir().unwrap();
    let mut inner = SubAgentManager::new(tmp.path().into(), 8);
    let retired = inner.insert_test_running_agent("7e2950c4", tmp.path());
    let foreign = inner.insert_test_running_agent("9f83d2a1", tmp.path());
    inner.assign_test_session_owner(&foreign, "other-session");
    for id in [&retired, &foreign] {
        inner.agents.get_mut(id).unwrap().started_at = Instant::now() - Duration::from_secs(3601);
        let mut terminal = inner.get_result(id).unwrap();
        terminal.status = SubAgentStatus::Completed;
        assert!(inner.finish_terminal_result(id, terminal, true, false));
        let record = inner.worker_records.get_mut(id).unwrap();
        record.completed_at_ms = Some(epoch_millis_now().saturating_sub(3_601_000));
    }
    inner.cleanup_for_session("other-session", COMPLETED_AGENT_RETENTION);
    let current = inner.insert_test_running_agent("86f4c3d2", tmp.path());
    let manager = Arc::new(tokio::sync::RwLock::new(inner));
    let context = ToolContext::new(tmp.path());
    let (own_handle, foreign_handle) = {
        let mut store = context.runtime.handle_store.lock().await;
        (
            store.insert_text(
                format!("agent:{retired}"),
                "transcript",
                "expired own evidence",
            ),
            store.insert_text(format!("agent:{foreign}"), "transcript", "foreign evidence"),
        )
    };
    let tool = AgentTool::new(manager.clone(), stub_runtime());
    assert!(
        tool.execute(
            json!({"action":"status","agent_id":"agent_deadbeef"}),
            &context
        )
        .await
        .is_err()
    );
    {
        let store = context.runtime.handle_store.lock().await;
        assert!(store.get(&own_handle).is_some());
        assert!(store.get(&foreign_handle).is_some());
    }
    assert!(
        tool.execute(json!({"action":"status","agent_id":current}), &context)
            .await
            .is_ok()
    );
    let store = context.runtime.handle_store.lock().await;
    assert!(
        store.get(&own_handle).is_none(),
        "next valid inspect must finish own eviction"
    );
    assert!(
        store.get(&foreign_handle).is_some(),
        "foreign eviction remains with its owner"
    );
}
