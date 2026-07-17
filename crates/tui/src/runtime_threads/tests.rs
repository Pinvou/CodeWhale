use super::*;
use crate::core::engine::{MockApprovalEvent, mock_engine_handle};
use crate::core::events::{Event as EngineEvent, TurnOutcomeStatus};
use std::time::{Duration, Instant};
use tokio::sync::oneshot;
use tokio::time::sleep;
use uuid::Uuid;

fn test_runtime_dir() -> PathBuf {
    std::env::temp_dir().join(format!("deepseek-runtime-threads-{}", Uuid::new_v4()))
}

fn test_manager_config(data_dir: PathBuf) -> RuntimeThreadManagerConfig {
    RuntimeThreadManagerConfig {
        task_data_dir: data_dir.clone(),
        data_dir,
        max_active_threads: 4,
    }
}

fn test_manager(data_dir: PathBuf) -> Result<RuntimeThreadManager> {
    RuntimeThreadManager::open(
        Config::default(),
        PathBuf::from("."),
        test_manager_config(data_dir),
    )
}

struct ApprovalTimeoutGuard {
    previous_ms: u64,
}

impl Drop for ApprovalTimeoutGuard {
    fn drop(&mut self) {
        set_test_approval_decision_timeout_ms(self.previous_ms);
    }
}

fn test_approval_timeout_ms(ms: u64) -> ApprovalTimeoutGuard {
    ApprovalTimeoutGuard {
        previous_ms: set_test_approval_decision_timeout_ms(ms),
    }
}

fn sample_thread(thread_id: &str) -> ThreadRecord {
    let now = Utc::now();
    ThreadRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        id: thread_id.to_string(),
        created_at: now,
        updated_at: now,
        model: DEFAULT_TEXT_MODEL.to_string(),
        model_provider: None,
        model_provider_id: None,
        workspace: PathBuf::from("."),
        mode: AppMode::Agent.as_setting().to_string(),
        allow_shell: false,
        trust_mode: false,
        auto_approve: false,
        latest_turn_id: None,
        latest_response_bookmark: None,
        archived: false,
        system_prompt: None,
        task_id: None,
        title: None,
        session_id: None,
    }
}

fn sample_turn(thread_id: &str, turn_id: &str, status: RuntimeTurnStatus) -> TurnRecord {
    let now = Utc::now();
    TurnRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        id: turn_id.to_string(),
        thread_id: thread_id.to_string(),
        status,
        input_summary: "sample".to_string(),
        created_at: now,
        started_at: Some(now),
        ended_at: None,
        duration_ms: None,
        usage: None,
        effective_provider: None,
        effective_provider_id: None,
        effective_billing_surface: None,
        effective_model: None,
        error: None,
        item_ids: Vec::new(),
        steer_count: 0,
    }
}

#[test]
fn runtime_compaction_uses_provider_route_context() {
    let limits = codewhale_config::route::RouteLimits {
        context_tokens: Some(272_000),
        input_tokens: None,
        output_tokens: None,
    };
    let config = runtime_compaction_config(
        ApiProvider::OpenaiCodex,
        "gpt-5.5",
        Some(limits),
        false,
        false,
        80.0,
    );

    assert!(config.enabled);
    // The threshold is 80% of the route's spendable input budget after
    // output reservation and headroom, not 80% of the raw context window.
    assert_eq!(config.token_threshold, 213_504);
    assert_eq!(config.effective_context_window, Some(272_000));
}

#[test]
fn legacy_turn_record_has_no_invented_route_provenance() {
    let turn = sample_turn("thr_legacy", "turn_legacy", RuntimeTurnStatus::Completed);
    let mut value = serde_json::to_value(turn).expect("serialize turn");
    let object = value.as_object_mut().expect("turn object");
    object.remove("effective_provider");
    object.remove("effective_provider_id");
    object.remove("effective_billing_surface");
    object.remove("effective_model");

    let restored: TurnRecord = serde_json::from_value(value).expect("deserialize legacy turn");
    assert_eq!(restored.effective_provider, None);
    assert_eq!(restored.effective_billing_surface, None);
    assert_eq!(restored.effective_model, None);
}

#[tokio::test]
async fn named_custom_thread_identity_round_trips_and_fails_closed_when_removed() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    custom.insert(
        "lm-studio".to_string(),
        crate::config::ProviderConfig {
            kind: Some("openai-compatible".to_string()),
            base_url: Some("http://127.0.0.1:1234/v1".to_string()),
            model: Some("local-default".to_string()),
            ..crate::config::ProviderConfig::default()
        },
    );
    let config = Config {
        provider: Some("lm-studio".to_string()),
        providers: Some(crate::config::ProvidersConfig {
            custom,
            ..crate::config::ProvidersConfig::default()
        }),
        ..Config::default()
    };
    let manager = RuntimeThreadManager::open(
        config.clone(),
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;

    let thread = manager
        .create_thread(CreateThreadRequest {
            model: Some("local-code-model".to_string()),
            model_provider: Some("lm-studio".to_string()),
            ..CreateThreadRequest::default()
        })
        .await?;
    let persisted = manager.get_thread(&thread.id).await?;
    assert_eq!(persisted.model_provider.as_deref(), Some("custom"));
    assert_eq!(persisted.model_provider_id.as_deref(), Some("lm-studio"));
    let serialized = serde_json::to_string(&persisted)?;
    assert!(serialized.contains("\"model_provider\":\"custom\""));
    assert!(serialized.contains("\"model_provider_id\":\"lm-studio\""));
    assert!(!serialized.contains("127.0.0.1:1234"));

    let route = manager.resolved_route_for_thread(&config, &persisted)?;
    assert_eq!(route.identity.provider, ApiProvider::Custom);
    assert_eq!(route.identity.key, "lm-studio");
    assert_eq!(route.model, "local-code-model");
    assert_eq!(route.config.deepseek_base_url(), "http://127.0.0.1:1234/v1");

    let err = manager
        .resolved_route_for_thread(&Config::default(), &persisted)
        .expect_err("removed provider must fail closed");
    let message = err.to_string();
    assert!(message.contains("[providers.lm-studio]"), "{message}");
    assert!(message.contains("will not fall back"), "{message}");

    let mut legacy_value = serde_json::to_value(&persisted)?;
    legacy_value
        .as_object_mut()
        .expect("thread object")
        .remove("model_provider");
    legacy_value
        .as_object_mut()
        .expect("thread object")
        .remove("model_provider_id");
    let legacy: ThreadRecord = serde_json::from_value(legacy_value)?;
    assert_eq!(legacy.model_provider, None);
    Ok(())
}

#[test]
fn legacy_literal_custom_thread_resume_requires_and_keeps_root_route() -> Result<()> {
    let config = Config {
        provider: Some("custom".to_string()),
        base_url: Some("http://127.0.0.1:18180/v1".to_string()),
        default_text_model: Some("legacy-default-model".to_string()),
        ..Config::default()
    };
    let manager = RuntimeThreadManager::open(
        config.clone(),
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;
    let mut persisted = sample_thread("thr_legacy_custom");
    persisted.model = "legacy-saved-model".to_string();
    persisted.model_provider = Some("custom".to_string());
    let restored: ThreadRecord = serde_json::from_str(&serde_json::to_string(&persisted)?)?;

    let route = manager.resolved_route_for_thread(&config, &restored)?;
    assert_eq!(route.identity.provider, ApiProvider::Custom);
    assert_eq!(route.identity.key, "custom");
    assert_eq!(route.model, "legacy-saved-model");
    assert_eq!(
        route.config.deepseek_base_url(),
        "http://127.0.0.1:18180/v1"
    );
    assert!(
        route
            .config
            .providers
            .as_ref()
            .is_none_or(|providers| !providers.custom.contains_key("custom")),
        "route resolution must not synthesize an ambiguous [providers.custom] table"
    );
    assert_eq!(
        route
            .config
            .resolve_provider_identity("custom")
            .map_err(anyhow::Error::msg)?,
        crate::config::ProviderIdentity {
            provider: ApiProvider::Custom,
            key: "custom".to_string(),
            exact_id: None,
        }
    );
    let repeated = manager.resolved_route_for_thread(&route.config, &restored)?;
    assert_eq!(repeated.identity.key, "custom");
    assert_eq!(repeated.model, "legacy-saved-model");
    assert_eq!(
        repeated.config.deepseek_base_url(),
        "http://127.0.0.1:18180/v1"
    );

    let named_config = {
        let mut custom = std::collections::HashMap::new();
        custom.insert(
            "lm-studio".to_string(),
            crate::config::ProviderConfig {
                kind: Some("openai-compatible".to_string()),
                base_url: Some("http://127.0.0.1:18181/v1".to_string()),
                model: Some("named-model".to_string()),
                ..crate::config::ProviderConfig::default()
            },
        );
        Config {
            provider: Some("lm-studio".to_string()),
            providers: Some(crate::config::ProvidersConfig {
                custom,
                ..crate::config::ProvidersConfig::default()
            }),
            ..Config::default()
        }
    };
    let error = manager
        .resolved_route_for_thread(&named_config, &restored)
        .expect_err("id-less root record must not migrate to a named table")
        .to_string();
    assert!(error.contains("root-level"), "{error}");
    assert!(error.contains("will not guess or fall back"), "{error}");

    Ok(())
}

#[tokio::test]
async fn root_custom_thread_and_turn_writers_omit_exact_id() -> Result<()> {
    let config = Config {
        provider: Some("custom".to_string()),
        base_url: Some("http://127.0.0.1:18180/v1".to_string()),
        default_text_model: Some("legacy-root-model".to_string()),
        ..Config::default()
    };
    let manager = RuntimeThreadManager::open(
        config,
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: Some("legacy-root-model".to_string()),
            ..CreateThreadRequest::default()
        })
        .await?;
    assert_eq!(thread.model_provider.as_deref(), Some("custom"));
    assert_eq!(thread.model_provider_id, None);
    assert!(!serde_json::to_string(&thread)?.contains("model_provider_id"));

    let mut harness = install_mock_engine(&manager, &thread.id).await;
    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "keep the root route".to_string(),
                ..StartTurnRequest::default()
            },
        )
        .await?;
    assert_eq!(turn.effective_provider.as_deref(), Some("custom"));
    assert_eq!(turn.effective_provider_id, None);
    assert!(!serde_json::to_string(&turn)?.contains("effective_provider_id"));
    match harness.rx_op.recv().await {
        Some(Op::SendMessage { route, .. }) => {
            assert_eq!(route.identity.key, "custom");
            assert_eq!(route.identity.exact_id, None);
            assert_eq!(
                route.config.deepseek_base_url(),
                "http://127.0.0.1:18180/v1"
            );
        }
        other => panic!("expected root custom send, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn real_turn_client_preflight_failure_writes_no_in_progress_record() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    custom.insert(
        "preflight-failure".to_string(),
        crate::config::ProviderConfig {
            kind: Some("openai-compatible".to_string()),
            base_url: Some("https://preflight.invalid/v1".to_string()),
            model: Some("preflight-model".to_string()),
            api_key: Some("test-key".to_string()),
            // Client construction rejects this independently of ambient auth,
            // keeping the async regression hermetic without a global env lock.
            insecure_skip_tls_verify: Some(true),
            ..crate::config::ProviderConfig::default()
        },
    );
    let manager = RuntimeThreadManager::open(
        Config {
            provider: Some("preflight-failure".to_string()),
            providers: Some(crate::config::ProvidersConfig {
                custom,
                ..crate::config::ProvidersConfig::default()
            }),
            ..Config::default()
        },
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;

    let error = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "must not become a zombie turn".to_string(),
                ..StartTurnRequest::default()
            },
        )
        .await
        .expect_err("missing credentials must fail before turn persistence")
        .to_string();

    assert!(
        error.contains("TLS certificate verification cannot be disabled"),
        "{error}"
    );
    assert!(manager.store.list_turns_for_thread(&thread.id)?.is_empty());
    assert_eq!(manager.get_thread(&thread.id).await?.latest_turn_id, None);
    Ok(())
}

#[tokio::test]
async fn closed_turn_mailbox_rolls_back_durable_records_and_active_claim() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let harness = install_mock_engine(&manager, &thread.id).await;
    let before_active = {
        let active = manager.active.lock().await;
        let state = active.engines.get(&thread.id).expect("installed engine");
        (
            state.active_turn.as_ref().map(|turn| turn.turn_id.clone()),
            state.route_identity.clone(),
            state.route_model.clone(),
            active.lru.clone(),
        )
    };
    let before_thread = serde_json::to_value(manager.get_thread(&thread.id).await?)?;
    let before_events = serde_json::to_value(manager.events_since(&thread.id, None)?)?;
    drop(harness.rx_op);

    let error = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "mailbox is already closed".to_string(),
                ..StartTurnRequest::default()
            },
        )
        .await
        .expect_err("closed mailbox must reject the turn")
        .to_string();
    assert!(error.contains("Failed to start turn"), "{error}");

    assert!(manager.store.list_turns_for_thread(&thread.id)?.is_empty());
    assert_eq!(
        serde_json::to_value(manager.get_thread(&thread.id).await?)?,
        before_thread
    );
    assert_eq!(
        serde_json::to_value(manager.events_since(&thread.id, None)?)?,
        before_events
    );
    assert_eq!(
        std::fs::read_dir(&manager.store.items_dir)?.count(),
        0,
        "failed send must remove the optimistic user item"
    );
    let after_active = {
        let active = manager.active.lock().await;
        let state = active.engines.get(&thread.id).expect("installed engine");
        (
            state.active_turn.as_ref().map(|turn| turn.turn_id.clone()),
            state.route_identity.clone(),
            state.route_model.clone(),
            active.lru.clone(),
        )
    };
    assert_eq!(after_active, before_active);
    Ok(())
}

#[tokio::test]
async fn cancellation_while_waiting_for_mailbox_capacity_claims_nothing() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let mut harness = install_mock_engine(&manager, &thread.id).await;

    for _ in 0..32 {
        harness.handle.try_send(Op::ListSubAgents)?;
    }
    let start_sender_count = harness.handle.tx_op.strong_count();
    let start_manager = manager.clone();
    let start_thread_id = thread.id.clone();
    let start_task = tokio::spawn(async move {
        start_manager
            .start_turn(
                &start_thread_id,
                StartTurnRequest {
                    prompt: "cancel before mailbox capacity".to_string(),
                    ..StartTurnRequest::default()
                },
            )
            .await
    });
    wait_for_sender_strong_count(&harness.handle.tx_op, start_sender_count + 2).await?;
    assert!(
        !start_task.is_finished(),
        "start should be waiting for capacity"
    );
    assert!(manager.store.list_turns_for_thread(&thread.id)?.is_empty());
    assert_eq!(manager.get_thread(&thread.id).await?.latest_turn_id, None);
    assert_eq!(manager.active_turn_flags(&thread.id, "missing").await, None);
    start_task.abort();
    let _ = start_task.await;
    for _ in 0..32 {
        assert!(matches!(
            harness.rx_op.recv().await,
            Some(Op::ListSubAgents)
        ));
    }

    for _ in 0..32 {
        harness.handle.try_send(Op::ListSubAgents)?;
    }
    let compact_sender_count = harness.handle.tx_op.strong_count();
    let compact_manager = manager.clone();
    let compact_thread_id = thread.id.clone();
    let compact_task = tokio::spawn(async move {
        compact_manager
            .compact_thread(&compact_thread_id, CompactThreadRequest::default())
            .await
    });
    wait_for_sender_strong_count(&harness.handle.tx_op, compact_sender_count + 2).await?;
    assert!(
        !compact_task.is_finished(),
        "compaction should be waiting for capacity"
    );
    assert!(manager.store.list_turns_for_thread(&thread.id)?.is_empty());
    assert_eq!(manager.get_thread(&thread.id).await?.latest_turn_id, None);
    compact_task.abort();
    let _ = compact_task.await;
    for _ in 0..32 {
        assert!(matches!(
            harness.rx_op.recv().await,
            Some(Op::ListSubAgents)
        ));
    }
    Ok(())
}

#[tokio::test]
async fn caller_cancellation_after_engine_acceptance_keeps_owned_turn_lifecycle() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let mut harness = install_mock_engine(&manager, &thread.id).await;

    // Block the first start-event append so the public future remains
    // cancellable after the operation has entered the engine mailbox.
    let event_state_guard = manager.store.state.lock().await;
    let start_manager = manager.clone();
    let thread_id = thread.id.clone();
    let start_task = tokio::spawn(async move {
        start_manager
            .start_turn(
                &thread_id,
                StartTurnRequest {
                    prompt: "the lifecycle outlives its caller".to_string(),
                    ..StartTurnRequest::default()
                },
            )
            .await
    });
    assert!(matches!(
        tokio::time::timeout(Duration::from_secs(2), harness.rx_op.recv()).await?,
        Some(Op::SendMessage { .. })
    ));
    let turns = manager.store.list_turns_for_thread(&thread.id)?;
    assert_eq!(turns.len(), 1);
    let turn_id = turns[0].id.clone();
    assert_eq!(turns[0].status, RuntimeTurnStatus::InProgress);
    assert_eq!(turns[0].item_ids.len(), 1);
    assert_eq!(
        manager.store.load_item(&turns[0].item_ids[0])?.turn_id,
        turn_id
    );
    assert!(
        manager
            .active_turn_flags(&thread.id, &turn_id)
            .await
            .is_some()
    );

    start_task.abort();
    let _ = start_task.await;
    drop(event_state_guard);

    harness
        .tx_event
        .send(EngineEvent::MessageStarted { index: 0 })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::MessageDelta {
            index: 0,
            content: "owned monitor is live".to_string(),
        })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::MessageComplete { index: 0 })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;
    let terminal = wait_for_terminal_turn(&manager, &turn_id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Completed);
    assert_eq!(manager.active_turn_flags(&thread.id, &turn_id).await, None);

    let lifecycle: Vec<String> = manager
        .events_since(&thread.id, None)?
        .iter()
        .filter(|event| event.turn_id.as_deref() == Some(turn_id.as_str()))
        .map(|event| event.event.clone())
        .collect();
    assert_eq!(
        &lifecycle[..3],
        &["turn.started", "item.started", "item.completed"]
    );
    assert_eq!(lifecycle.last().map(String::as_str), Some("turn.completed"));
    Ok(())
}

#[tokio::test]
async fn thread_updates_while_start_waits_for_capacity_survive_latest_turn_write() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let mut harness = install_mock_engine(&manager, &thread.id).await;
    for _ in 0..32 {
        harness.handle.try_send(Op::ListSubAgents)?;
    }
    let sender_count = harness.handle.tx_op.strong_count();

    let start_manager = manager.clone();
    let thread_id = thread.id.clone();
    let start_task = tokio::spawn(async move {
        start_manager
            .start_turn(
                &thread_id,
                StartTurnRequest {
                    prompt: "preserve concurrent metadata".to_string(),
                    ..StartTurnRequest::default()
                },
            )
            .await
    });
    wait_for_sender_strong_count(&harness.handle.tx_op, sender_count + 2).await?;
    assert!(!start_task.is_finished());

    manager
        .update_thread(
            &thread.id,
            UpdateThreadRequest {
                title: Some("new title while queued".to_string()),
                ..UpdateThreadRequest::default()
            },
        )
        .await?;
    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::ListSubAgents)
    ));
    let turn = tokio::time::timeout(Duration::from_secs(2), start_task).await???;
    let mut saw_send = false;
    for _ in 0..32 {
        if matches!(harness.rx_op.recv().await, Some(Op::SendMessage { .. })) {
            saw_send = true;
            break;
        }
    }
    assert!(
        saw_send,
        "accepted send must remain behind refresh operations"
    );

    harness
        .tx_event
        .send(EngineEvent::MessageStarted { index: 0 })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::MessageDelta {
            index: 0,
            content: "metadata retained".to_string(),
        })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::MessageComplete { index: 0 })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;
    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Completed);
    assert_eq!(turn.item_ids.len(), 1);
    assert!(
        terminal.item_ids.contains(&turn.item_ids[0]),
        "the accepted user item must survive later assistant-item writes"
    );
    assert_eq!(
        manager.store.load_turn(&turn.id)?.item_ids,
        terminal.item_ids
    );
    let updated = manager.get_thread(&thread.id).await?;
    assert_eq!(updated.title.as_deref(), Some("new title while queued"));
    assert_eq!(updated.latest_turn_id.as_deref(), Some(turn.id.as_str()));
    Ok(())
}

#[tokio::test]
async fn execution_update_while_start_waits_rejects_stale_operation() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let mut harness = install_mock_engine(&manager, &thread.id).await;
    for _ in 0..32 {
        harness.handle.try_send(Op::ListSubAgents)?;
    }
    let sender_count = harness.handle.tx_op.strong_count();
    let start_manager = manager.clone();
    let thread_id = thread.id.clone();
    let start_task = tokio::spawn(async move {
        start_manager
            .start_turn(
                &thread_id,
                StartTurnRequest {
                    prompt: "must not use stale mode".to_string(),
                    ..StartTurnRequest::default()
                },
            )
            .await
    });
    wait_for_sender_strong_count(&harness.handle.tx_op, sender_count + 2).await?;

    manager
        .update_thread(
            &thread.id,
            UpdateThreadRequest {
                mode: Some(AppMode::Plan.as_setting().to_string()),
                ..UpdateThreadRequest::default()
            },
        )
        .await?;
    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::ListSubAgents)
    ));
    let error = tokio::time::timeout(Duration::from_secs(2), start_task)
        .await??
        .expect_err("stale operation must fail")
        .to_string();
    assert!(error.contains("execution settings changed"), "{error}");
    for _ in 0..31 {
        assert!(matches!(
            harness.rx_op.recv().await,
            Some(Op::ListSubAgents)
        ));
    }
    assert!(harness.rx_op.try_recv().is_err());
    assert!(manager.store.list_turns_for_thread(&thread.id)?.is_empty());
    let updated = manager.get_thread(&thread.id).await?;
    assert_eq!(updated.mode, AppMode::Plan.as_setting());
    assert_eq!(updated.latest_turn_id, None);
    assert_eq!(manager.active_turn_flags(&thread.id, "missing").await, None);
    Ok(())
}

#[tokio::test]
async fn compact_lifecycle_outlives_caller_and_preserves_concurrent_thread_updates() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let mut harness = install_mock_engine(&manager, &thread.id).await;
    for _ in 0..32 {
        harness.handle.try_send(Op::ListSubAgents)?;
    }
    let sender_count = harness.handle.tx_op.strong_count();

    let compact_manager = manager.clone();
    let thread_id = thread.id.clone();
    let compact_task = tokio::spawn(async move {
        compact_manager
            .compact_thread(&thread_id, CompactThreadRequest::default())
            .await
    });
    wait_for_sender_strong_count(&harness.handle.tx_op, sender_count + 2).await?;
    assert!(!compact_task.is_finished());
    manager
        .update_thread(
            &thread.id,
            UpdateThreadRequest {
                title: Some("title before compact claim".to_string()),
                ..UpdateThreadRequest::default()
            },
        )
        .await?;
    // Once capacity is released, block the acknowledgement events so the
    // API future can be dropped after the engine accepted the operation.
    let event_state_guard = manager.store.state.lock().await;
    let mut saw_compact = false;
    for _ in 0..33 {
        if matches!(
            tokio::time::timeout(Duration::from_secs(2), harness.rx_op.recv()).await?,
            Some(Op::CompactContext { .. })
        ) {
            saw_compact = true;
            break;
        }
    }
    assert!(
        saw_compact,
        "manual compaction must enter the engine mailbox"
    );
    let turns = manager.store.list_turns_for_thread(&thread.id)?;
    assert_eq!(turns.len(), 1);
    let turn_id = turns[0].id.clone();
    compact_task.abort();
    let _ = compact_task.await;
    drop(event_state_guard);

    harness
        .tx_event
        .send(EngineEvent::CompactionStarted {
            id: "manual_owned".to_string(),
            auto: false,
            message: "compaction started".to_string(),
        })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::CompactionCompleted {
            id: "manual_owned".to_string(),
            auto: false,
            message: "compaction completed".to_string(),
            messages_before: Some(4),
            messages_after: Some(2),
            summary_prompt: None,
        })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;
    let terminal = wait_for_terminal_turn(&manager, &turn_id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Completed);
    assert_eq!(manager.active_turn_flags(&thread.id, &turn_id).await, None);
    let updated = manager.get_thread(&thread.id).await?;
    assert_eq!(updated.title.as_deref(), Some("title before compact claim"));
    assert_eq!(updated.latest_turn_id.as_deref(), Some(turn_id.as_str()));
    Ok(())
}

#[tokio::test]
async fn concurrent_turn_starts_leave_one_claim_and_one_consistent_durable_turn() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let mut harness = install_mock_engine(&manager, &thread.id).await;
    let first = manager.start_turn(
        &thread.id,
        StartTurnRequest {
            prompt: "first concurrent turn".to_string(),
            ..StartTurnRequest::default()
        },
    );
    let second = manager.start_turn(
        &thread.id,
        StartTurnRequest {
            prompt: "second concurrent turn".to_string(),
            ..StartTurnRequest::default()
        },
    );

    let (first, second) = tokio::join!(first, second);
    let (turn, rejection) = match (first, second) {
        (Ok(turn), Err(error)) | (Err(error), Ok(turn)) => (turn, error),
        (first, second) => {
            panic!("expected one accepted turn and one rejection: {first:?} {second:?}")
        }
    };
    assert!(
        rejection.to_string().contains("already has an active turn"),
        "{rejection}"
    );
    let turns = manager.store.list_turns_for_thread(&thread.id)?;
    assert_eq!(turns.len(), 1);
    assert_eq!(turns[0].id, turn.id);
    assert_eq!(
        manager.get_thread(&thread.id).await?.latest_turn_id,
        Some(turn.id.clone())
    );
    assert_eq!(
        manager.active_turn_flags(&thread.id, &turn.id).await,
        Some((false, false))
    );
    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::SendMessage { .. })
    ));
    assert!(harness.rx_op.try_recv().is_err());
    Ok(())
}

#[test]
fn legacy_custom_thread_stays_on_root_when_literal_table_coexists() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    custom.insert(
        "custom".to_string(),
        crate::config::ProviderConfig {
            kind: Some("openai-compatible".to_string()),
            base_url: Some("http://127.0.0.1:18182/v1".to_string()),
            model: Some("table-model".to_string()),
            ..crate::config::ProviderConfig::default()
        },
    );
    let config = Config {
        provider: Some("custom".to_string()),
        base_url: Some("http://127.0.0.1:18181/v1".to_string()),
        default_text_model: Some("legacy-root-model".to_string()),
        providers: Some(crate::config::ProvidersConfig {
            custom,
            ..crate::config::ProvidersConfig::default()
        }),
        ..Config::default()
    };
    let manager = RuntimeThreadManager::open(
        config.clone(),
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;
    let mut legacy = sample_thread("thr_ambiguous_legacy_custom");
    legacy.model = "legacy-saved-model".to_string();
    legacy.model_provider = Some("custom".to_string());
    legacy.model_provider_id = None;

    let root = manager.resolved_route_for_thread(&config, &legacy)?;
    assert_eq!(root.identity.provider, ApiProvider::Custom);
    assert_eq!(root.identity.key, "custom");
    assert_eq!(root.identity.exact_id, None);
    assert_eq!(root.config.deepseek_base_url(), "http://127.0.0.1:18181/v1");

    legacy.model_provider_id = Some("custom".to_string());
    let exact = manager.resolved_route_for_thread(&config, &legacy)?;
    assert_eq!(exact.identity.provider, ApiProvider::Custom);
    assert_eq!(exact.identity.key, "custom");
    assert_eq!(exact.identity.exact_id.as_deref(), Some("custom"));
    assert_eq!(
        exact.config.deepseek_base_url(),
        "http://127.0.0.1:18182/v1"
    );
    let root_only = Config {
        provider: Some("custom".to_string()),
        base_url: Some("http://127.0.0.1:18181/v1".to_string()),
        default_text_model: Some("legacy-root-model".to_string()),
        ..Config::default()
    };
    let error = manager
        .resolved_route_for_thread(&root_only, &legacy)
        .expect_err("exact literal table thread must not fall back to root")
        .to_string();
    assert!(error.contains("[providers.custom]"), "{error}");
    assert!(error.contains("will not fall back"), "{error}");
    Ok(())
}

#[tokio::test]
async fn empty_imported_custom_id_fails_closed_when_root_and_table_coexist() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    custom.insert(
        "custom".to_string(),
        crate::config::ProviderConfig {
            kind: Some("openai-compatible".to_string()),
            base_url: Some("http://127.0.0.1:18182/v1".to_string()),
            model: Some("table-model".to_string()),
            ..crate::config::ProviderConfig::default()
        },
    );
    let config = Config {
        provider: Some("custom".to_string()),
        base_url: Some("http://127.0.0.1:18181/v1".to_string()),
        default_text_model: Some("legacy-root-model".to_string()),
        providers: Some(crate::config::ProvidersConfig {
            custom,
            ..crate::config::ProvidersConfig::default()
        }),
        ..Config::default()
    };
    let manager = RuntimeThreadManager::open(
        config.clone(),
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;

    let mut imported = sample_thread("thr_empty_custom_id");
    imported.model_provider = Some("custom".to_string());
    imported.model_provider_id = Some("   ".to_string());
    let error = manager
        .resolved_route_for_thread(&config, &imported)
        .expect_err("malformed imported identity must not acquire the root route")
        .to_string();
    assert!(error.contains("empty exact provider id"), "{error}");

    let before = manager.store.list_threads()?.len();
    let request_error = manager
        .create_thread(CreateThreadRequest {
            model_provider: Some("custom".to_string()),
            model_provider_id: Some(String::new()),
            ..CreateThreadRequest::default()
        })
        .await
        .expect_err("malformed create request must fail before persistence")
        .to_string();
    assert!(
        request_error.contains("empty exact provider id"),
        "{request_error}"
    );
    assert_eq!(manager.store.list_threads()?.len(), before);
    Ok(())
}

#[tokio::test]
async fn thread_records_and_create_requests_preserve_provider_kind_id_pairing() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    custom.insert(
        "openai".to_string(),
        crate::config::ProviderConfig {
            kind: Some("openai-compatible".to_string()),
            base_url: Some("http://127.0.0.1:18183/v1".to_string()),
            model: Some("custom-openai-model".to_string()),
            ..crate::config::ProviderConfig::default()
        },
    );
    let config = Config {
        provider: Some("openai".to_string()),
        providers: Some(crate::config::ProvidersConfig {
            custom,
            ..crate::config::ProvidersConfig::default()
        }),
        ..Config::default()
    };
    let manager = RuntimeThreadManager::open(
        config.clone(),
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;

    for provider_id in [None, Some("openai".to_string())] {
        let mut built_in = sample_thread("thr_builtin_openai_collision");
        built_in.model_provider = Some("openai".to_string());
        built_in.model_provider_id = provider_id;
        let error = manager
            .resolved_route_for_thread(&config, &built_in)
            .expect_err("built-in thread must not route through same-key custom endpoint")
            .to_string();
        assert!(error.contains("requires built-in 'openai'"), "{error}");
        assert!(error.contains("shadows"), "{error}");
    }

    let mut exact_custom = sample_thread("thr_custom_openai_collision");
    exact_custom.model = "custom-openai-model".to_string();
    exact_custom.model_provider = Some("custom".to_string());
    exact_custom.model_provider_id = Some("openai".to_string());
    let route = manager.resolved_route_for_thread(&config, &exact_custom)?;
    assert_eq!(route.identity.provider, ApiProvider::Custom);
    assert_eq!(route.identity.key, "openai");
    assert_eq!(
        route.config.deepseek_base_url(),
        "http://127.0.0.1:18183/v1"
    );

    let mut auto_thread = exact_custom.clone();
    auto_thread.id = "thr_auto_openai_collision".to_string();
    auto_thread.model = "auto".to_string();
    manager.store.save_thread(&auto_thread)?;
    let mut restored_turn = sample_turn(
        &auto_thread.id,
        "turn_openai_collision",
        RuntimeTurnStatus::Completed,
    );
    restored_turn.effective_provider = Some("openai".to_string());
    restored_turn.effective_provider_id = None;
    restored_turn.effective_model = Some("custom-openai-model".to_string());
    manager.store.save_turn(&restored_turn)?;
    let turn_error = manager
        .resolved_route_for_thread(&config, &auto_thread)
        .expect_err("restored built-in turn must not be captured by custom endpoint")
        .to_string();
    assert!(
        turn_error.contains("requires built-in 'openai'"),
        "{turn_error}"
    );

    restored_turn.effective_provider = Some("custom".to_string());
    restored_turn.effective_provider_id = Some("openai".to_string());
    manager.store.save_turn(&restored_turn)?;
    let restored_custom = manager.resolved_route_for_thread(&config, &auto_thread)?;
    assert_eq!(restored_custom.identity.provider, ApiProvider::Custom);
    assert_eq!(restored_custom.identity.key, "openai");
    assert_eq!(restored_custom.model, "custom-openai-model");

    let request_error = manager
        .create_thread(CreateThreadRequest {
            model_provider: Some("openai".to_string()),
            model_provider_id: Some("openai".to_string()),
            ..CreateThreadRequest::default()
        })
        .await
        .expect_err("built-in request must fail closed under exact custom shadow")
        .to_string();
    assert!(
        request_error.contains("requires built-in 'openai'"),
        "{request_error}"
    );

    let created = manager
        .create_thread(CreateThreadRequest {
            model_provider: Some("custom".to_string()),
            model_provider_id: Some("openai".to_string()),
            ..CreateThreadRequest::default()
        })
        .await?;
    assert_eq!(created.model_provider.as_deref(), Some("custom"));
    assert_eq!(created.model_provider_id.as_deref(), Some("openai"));
    assert_eq!(created.model, "custom-openai-model");
    Ok(())
}

#[tokio::test]
async fn config_reload_updates_next_turn_route_without_mutating_engine_route() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    custom.insert(
        "lm-studio".to_string(),
        crate::config::ProviderConfig {
            kind: Some("openai-compatible".to_string()),
            base_url: Some("http://127.0.0.1:18181/v1".to_string()),
            model: Some("local-model".to_string()),
            api_key: Some("old-local-test-key".to_string()),
            ..crate::config::ProviderConfig::default()
        },
    );
    let config = Config {
        provider: Some("lm-studio".to_string()),
        providers: Some(crate::config::ProvidersConfig {
            custom,
            ..crate::config::ProvidersConfig::default()
        }),
        ..Config::default()
    };
    let manager = RuntimeThreadManager::open(
        config.clone(),
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: Some("local-model".to_string()),
            model_provider: Some("lm-studio".to_string()),
            ..CreateThreadRequest::default()
        })
        .await?;
    let mut harness = install_mock_engine(&manager, &thread.id).await;

    let mut reloaded = config;
    let provider = reloaded
        .providers
        .as_mut()
        .and_then(|providers| providers.custom.get_mut("lm-studio"))
        .expect("named custom provider");
    provider.base_url = Some("http://127.0.0.1:18182/v1".to_string());
    provider.api_key = Some("new-local-test-key".to_string());
    manager.reload_config(reloaded).await?;

    let refreshed = manager.resolved_route_for_thread(&manager.read_config(), &thread)?;
    assert_eq!(refreshed.identity.key, "lm-studio");
    assert_eq!(
        refreshed.config.deepseek_base_url(),
        "http://127.0.0.1:18182/v1"
    );
    for _ in 0..3 {
        let op = harness.rx_op.recv().await.expect("runtime control op");
        assert!(
            matches!(
                op,
                Op::SetCompaction { .. }
                    | Op::SetStreamChunkTimeout { .. }
                    | Op::SetSubagentRuntimeConfig { .. }
            ),
            "reload must not mutate an engine provider route: {op:?}"
        );
    }
    let compact_turn = manager
        .compact_thread(
            &thread.id,
            CompactThreadRequest {
                reason: Some("verify refreshed route".to_string()),
            },
        )
        .await?;
    assert_eq!(compact_turn.effective_provider.as_deref(), Some("custom"));
    assert_eq!(
        compact_turn.effective_provider_id.as_deref(),
        Some("lm-studio")
    );
    assert_eq!(compact_turn.effective_model.as_deref(), Some("local-model"));
    match harness.rx_op.recv().await {
        Some(Op::CompactContext { route, compaction }) => {
            assert_eq!(route.identity.key, "lm-studio");
            assert_eq!(
                route.config.deepseek_base_url(),
                "http://127.0.0.1:18182/v1"
            );
            assert_eq!(compaction.model, "local-model");
            assert_eq!(
                compaction.effective_context_window,
                Some(crate::route_budget::route_context_window_tokens(
                    ApiProvider::Custom,
                    "local-model",
                    crate::route_budget::known_route_limits(route.candidate.limits),
                ))
            );
        }
        other => panic!("expected typed compact route, got {other:?}"),
    }

    Ok(())
}

#[tokio::test]
async fn config_sync_reports_removed_named_custom_route_and_keeps_mailbox_clean() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    custom.insert(
        "lm-studio".to_string(),
        crate::config::ProviderConfig {
            kind: Some("openai-compatible".to_string()),
            base_url: Some("http://127.0.0.1:18181/v1".to_string()),
            model: Some("local-model".to_string()),
            api_key: Some("local-test-key".to_string()),
            ..crate::config::ProviderConfig::default()
        },
    );
    let config = Config {
        provider: Some("lm-studio".to_string()),
        providers: Some(crate::config::ProvidersConfig {
            custom,
            ..crate::config::ProvidersConfig::default()
        }),
        ..Config::default()
    };
    let manager = RuntimeThreadManager::open(
        config,
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: Some("local-model".to_string()),
            model_provider: Some("lm-studio".to_string()),
            ..CreateThreadRequest::default()
        })
        .await?;
    let mut harness = install_mock_engine(&manager, &thread.id).await;

    let err = manager
        .reload_config(Config::default())
        .await
        .expect_err("removed named custom route must fail config reload");

    let message = err.to_string();
    assert!(message.contains(&thread.id), "{message}");
    assert!(message.contains("lm-studio"), "{message}");
    assert!(harness.rx_op.try_recv().is_err());
    Ok(())
}

#[tokio::test]
async fn create_thread_uses_requested_named_custom_provider_default_model() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    for (name, base_url, model) in [
        ("custom-a", "http://127.0.0.1:18181/v1", "model-a"),
        ("custom-b", "http://127.0.0.1:18182/v1", "model-b"),
    ] {
        custom.insert(
            name.to_string(),
            crate::config::ProviderConfig {
                kind: Some("openai-compatible".to_string()),
                base_url: Some(base_url.to_string()),
                model: Some(model.to_string()),
                ..Default::default()
            },
        );
    }
    let config = Config {
        provider: Some("custom-b".to_string()),
        providers: Some(crate::config::ProvidersConfig {
            custom,
            ..Default::default()
        }),
        ..Default::default()
    };
    let manager = RuntimeThreadManager::open(
        config.clone(),
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;

    let thread = manager
        .create_thread(CreateThreadRequest {
            model_provider: Some("custom-a".to_string()),
            ..Default::default()
        })
        .await?;

    assert_eq!(thread.model_provider.as_deref(), Some("custom"));
    assert_eq!(thread.model_provider_id.as_deref(), Some("custom-a"));
    assert_eq!(thread.model, "model-a");
    let route = manager.resolved_route_for_thread(&config, &thread)?;
    assert_eq!(route.identity.key, "custom-a");
    assert_eq!(
        route.config.deepseek_base_url(),
        "http://127.0.0.1:18181/v1"
    );
    Ok(())
}

#[tokio::test]
async fn create_thread_uses_requested_non_current_builtin_default_model() -> Result<()> {
    let config = Config {
        provider: Some("openrouter".to_string()),
        default_text_model: Some(DEFAULT_TEXT_MODEL.to_string()),
        ..Default::default()
    };
    let manager = RuntimeThreadManager::open(
        config,
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;

    let thread = manager
        .create_thread(CreateThreadRequest {
            model_provider: Some("zai".to_string()),
            ..Default::default()
        })
        .await?;

    assert_eq!(thread.model_provider.as_deref(), Some("zai"));
    assert_eq!(thread.model, crate::config::DEFAULT_ZAI_MODEL);
    Ok(())
}

#[tokio::test]
async fn simultaneous_named_custom_auto_threads_keep_exact_routes() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    for (name, base_url, model) in [
        ("custom-a", "http://127.0.0.1:18181/v1", "model-a"),
        ("custom-b", "http://127.0.0.1:18182/v1", "model-b"),
    ] {
        custom.insert(
            name.to_string(),
            crate::config::ProviderConfig {
                kind: Some("openai-compatible".to_string()),
                base_url: Some(base_url.to_string()),
                model: Some(model.to_string()),
                ..Default::default()
            },
        );
    }
    let manager = RuntimeThreadManager::open(
        Config {
            provider: Some("custom-b".to_string()),
            providers: Some(crate::config::ProvidersConfig {
                custom,
                ..Default::default()
            }),
            ..Default::default()
        },
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;
    let thread_a = manager
        .create_thread(CreateThreadRequest {
            model: Some("auto".to_string()),
            model_provider: Some("custom-a".to_string()),
            ..Default::default()
        })
        .await?;
    let thread_b = manager
        .create_thread(CreateThreadRequest {
            model: Some("auto".to_string()),
            model_provider: Some("custom-b".to_string()),
            ..Default::default()
        })
        .await?;
    let mut harness_a = install_mock_engine(&manager, &thread_a.id).await;
    let mut harness_b = install_mock_engine(&manager, &thread_b.id).await;

    let request_a = manager.start_turn(
        &thread_a.id,
        StartTurnRequest {
            prompt: "route A".to_string(),
            ..Default::default()
        },
    );
    let request_b = manager.start_turn(
        &thread_b.id,
        StartTurnRequest {
            prompt: "route B".to_string(),
            ..Default::default()
        },
    );
    let (turn_a, turn_b) = tokio::join!(request_a, request_b);
    let turn_a = turn_a?;
    let turn_b = turn_b?;

    assert_eq!(turn_a.effective_provider.as_deref(), Some("custom"));
    assert_eq!(turn_a.effective_provider_id.as_deref(), Some("custom-a"));
    assert_eq!(turn_a.effective_model.as_deref(), Some("model-a"));
    assert_eq!(turn_b.effective_provider.as_deref(), Some("custom"));
    assert_eq!(turn_b.effective_provider_id.as_deref(), Some("custom-b"));
    assert_eq!(turn_b.effective_model.as_deref(), Some("model-b"));
    match harness_a.rx_op.recv().await {
        Some(Op::SendMessage { route, .. }) => {
            assert_eq!(route.identity.provider, ApiProvider::Custom);
            assert_eq!(route.identity.key, "custom-a");
            assert_eq!(route.model, "model-a");
        }
        other => panic!("expected custom A send, got {other:?}"),
    }
    match harness_b.rx_op.recv().await {
        Some(Op::SendMessage { route, .. }) => {
            assert_eq!(route.identity.provider, ApiProvider::Custom);
            assert_eq!(route.identity.key, "custom-b");
            assert_eq!(route.model, "model-b");
        }
        other => panic!("expected custom B send, got {other:?}"),
    }
    Ok(())
}

#[test]
fn turn_record_persists_billing_surface_without_raw_endpoint() {
    let mut turn = sample_turn("thr_surface", "turn_surface", RuntimeTurnStatus::Completed);
    turn.effective_provider = Some(ApiProvider::Stepfun.as_str().to_string());
    turn.effective_billing_surface = Some(crate::pricing::STEPFUN_PAYG_BILLING_SURFACE.to_string());
    turn.effective_model = Some("step-3.7-flash".to_string());

    let value = serde_json::to_value(turn).expect("serialize turn");
    assert_eq!(
        value["effective_billing_surface"],
        crate::pricing::STEPFUN_PAYG_BILLING_SURFACE
    );
    assert!(value.get("base_url").is_none());
    assert!(value.get("effective_base_url").is_none());
}

#[tokio::test]
async fn aggregate_usage_keeps_codex_tokens_without_api_dollar_pricing() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let mut thread = sample_thread("thr_mixed_routes");
    thread.model = "auto".to_string();
    manager.store.save_thread(&thread)?;

    let usage = Usage {
        input_tokens: 10_000,
        output_tokens: 1_000,
        ..Usage::default()
    };
    let mut deepseek = sample_turn(&thread.id, "turn_deepseek", RuntimeTurnStatus::Completed);
    deepseek.usage = Some(usage.clone());
    deepseek.effective_provider = Some(ApiProvider::Deepseek.as_str().to_string());
    deepseek.effective_model = Some("deepseek-v4-flash".to_string());
    manager.store.save_turn(&deepseek)?;

    let mut codex = sample_turn(&thread.id, "turn_codex", RuntimeTurnStatus::Completed);
    codex.usage = Some(usage);
    codex.effective_provider = Some(ApiProvider::OpenaiCodex.as_str().to_string());
    codex.effective_model = Some("gpt-5.5".to_string());
    manager.store.save_turn(&codex)?;

    let report = manager
        .aggregate_usage(None, None, UsageGroupBy::Provider)
        .await?;
    assert_eq!(report.totals.turns, 2);
    assert_eq!(report.totals.input_tokens, 20_000);
    assert!(report.totals.cost_usd > 0.0);

    let deepseek_bucket = report
        .buckets
        .iter()
        .find(|bucket| bucket.key == ApiProvider::Deepseek.as_str())
        .expect("DeepSeek bucket");
    let codex_bucket = report
        .buckets
        .iter()
        .find(|bucket| bucket.key == ApiProvider::OpenaiCodex.as_str())
        .expect("Codex bucket");
    assert!(deepseek_bucket.cost_usd > 0.0);
    assert_eq!(codex_bucket.cost_usd, 0.0);
    assert_eq!(codex_bucket.input_tokens, 10_000);
    assert_eq!(report.totals.cost_usd, deepseek_bucket.cost_usd);
    Ok(())
}

#[tokio::test]
async fn aggregate_usage_prices_each_turn_at_its_recorded_time() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let mut thread = sample_thread("thr_historical_pricing");
    thread.model = "claude-sonnet-5".to_string();
    manager.store.save_thread(&thread)?;

    let usage = Usage {
        input_tokens: 1_000_000,
        output_tokens: 0,
        ..Usage::default()
    };
    for (turn_id, created_at) in [
        ("turn_intro", "2026-08-31T23:59:59Z"),
        ("turn_standard", "2026-09-01T00:00:00Z"),
    ] {
        let mut turn = sample_turn(&thread.id, turn_id, RuntimeTurnStatus::Completed);
        turn.created_at = created_at.parse().expect("recorded turn time");
        turn.usage = Some(usage.clone());
        turn.effective_provider = Some(ApiProvider::Anthropic.as_str().to_string());
        turn.effective_model = Some("claude-sonnet-5".to_string());
        manager.store.save_turn(&turn)?;
    }

    let report = manager
        .aggregate_usage(None, None, UsageGroupBy::Model)
        .await?;

    assert_eq!(report.totals.turns, 2);
    assert!((report.totals.cost_usd - 5.0).abs() < f64::EPSILON);
    assert_eq!(report.buckets.len(), 1);
    assert!((report.buckets[0].cost_usd - 5.0).abs() < f64::EPSILON);
    Ok(())
}

#[tokio::test]
async fn aggregate_usage_prices_only_stepfun_payg_surface() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let mut thread = sample_thread("thr_stepfun_surfaces");
    thread.model = "step-3.7-flash".to_string();
    manager.store.save_thread(&thread)?;

    let usage = Usage {
        input_tokens: 1_000_000,
        output_tokens: 500_000,
        prompt_cache_hit_tokens: Some(250_000),
        ..Usage::default()
    };
    for (turn_id, surface) in [
        (
            "turn_stepfun_payg",
            crate::pricing::STEPFUN_PAYG_BILLING_SURFACE,
        ),
        (
            "turn_stepfun_plan",
            crate::pricing::STEPFUN_PLAN_BILLING_SURFACE,
        ),
    ] {
        let mut turn = sample_turn(&thread.id, turn_id, RuntimeTurnStatus::Completed);
        turn.usage = Some(usage.clone());
        turn.effective_provider = Some(ApiProvider::Stepfun.as_str().to_string());
        turn.effective_billing_surface = Some(surface.to_string());
        turn.effective_model = Some("step-3.7-flash".to_string());
        manager.store.save_turn(&turn)?;
    }

    let report = manager
        .aggregate_usage(None, None, UsageGroupBy::Provider)
        .await?;
    assert_eq!(report.totals.turns, 2);
    assert!((report.totals.cost_usd - 0.735).abs() < 1e-12);
    Ok(())
}

fn sample_item(turn_id: &str, item_id: &str, status: TurnItemLifecycleStatus) -> TurnItemRecord {
    TurnItemRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        id: item_id.to_string(),
        turn_id: turn_id.to_string(),
        kind: TurnItemKind::Status,
        status,
        summary: "sample item".to_string(),
        detail: None,
        metadata: None,
        artifact_refs: Vec::new(),
        started_at: Some(Utc::now()),
        ended_at: None,
    }
}

async fn install_mock_engine(
    manager: &RuntimeThreadManager,
    thread_id: &str,
) -> crate::core::engine::MockEngineHandle {
    let harness = mock_engine_handle();
    manager
        .install_test_engine(thread_id, harness.handle.clone())
        .await
        .expect("install mock engine");
    harness
}

async fn wait_for_sender_strong_count<T>(
    sender: &tokio::sync::mpsc::Sender<T>,
    minimum: usize,
) -> Result<()> {
    tokio::time::timeout(Duration::from_secs(2), async {
        while sender.strong_count() < minimum {
            tokio::task::yield_now().await;
        }
    })
    .await
    .map_err(|_| anyhow!("Timed out waiting for mailbox reservation"))?;
    Ok(())
}

async fn wait_for_terminal_turn(
    manager: &RuntimeThreadManager,
    turn_id: &str,
    timeout: Duration,
) -> Result<TurnRecord> {
    let deadline = Instant::now() + timeout;
    loop {
        let turn = manager.store.load_turn(turn_id)?;
        if matches!(
            turn.status,
            RuntimeTurnStatus::Completed
                | RuntimeTurnStatus::Failed
                | RuntimeTurnStatus::Interrupted
                | RuntimeTurnStatus::Canceled
        ) {
            return Ok(turn);
        }
        if Instant::now() >= deadline {
            bail!("Timed out waiting for turn {turn_id}");
        }
        sleep(Duration::from_millis(20)).await;
    }
}

#[test]
fn store_load_thread_rejects_newer_schema_version() {
    let dir = test_runtime_dir();
    let store = RuntimeThreadStore::open(dir.clone()).expect("open store");

    // Construct a thread record persisted with a future schema version.
    let mut thread = sample_thread("thr_future");
    thread.schema_version = CURRENT_RUNTIME_SCHEMA_VERSION + 1;

    // Bypass save_thread (which would respect our local schema_version)
    // by writing the JSON directly so we can simulate a future writer.
    let path = store.threads_dir.join(format!("{}.json", thread.id));
    std::fs::create_dir_all(path.parent().unwrap()).expect("mkdirs");
    let payload = serde_json::to_string(&thread).expect("serialize thread");
    std::fs::write(&path, payload).expect("write thread");

    let err = store
        .load_thread(&thread.id)
        .expect_err("load_thread must reject newer schema");
    let msg = format!("{err:#}");
    assert!(msg.contains("newer than supported"), "got: {msg}");

    // Cleanup so we don't leak across tests.
    let _ = std::fs::remove_dir_all(dir);
}

#[cfg(unix)]
#[test]
fn store_open_rejects_symlinked_state_file() {
    let dir = test_runtime_dir();
    std::fs::create_dir_all(&dir).expect("mkdir runtime dir");
    let target = dir.join("outside-state.json");
    let link = dir.join("state.json");
    std::fs::write(
        &target,
        serde_json::to_string(&RuntimeStoreState::default()).unwrap(),
    )
    .expect("write target");
    std::os::unix::fs::symlink(&target, &link).expect("symlink state");

    let err = RuntimeThreadStore::open(dir.clone()).expect_err("symlink state should fail");
    assert!(format!("{err:#}").contains("must not be a symlink"));

    let _ = std::fs::remove_dir_all(dir);
}

#[test]
fn store_open_rejects_root_traversal() {
    let dir = test_runtime_dir();
    let bad_root = dir.join("runtime").join("..").join("outside");

    let err = RuntimeThreadStore::open(bad_root).expect_err("traversal root should fail");
    assert!(format!("{err:#}").contains("cannot contain '..'"));

    let _ = std::fs::remove_dir_all(dir);
}

#[cfg(unix)]
#[test]
fn store_open_rejects_symlinked_store_directory() {
    let dir = test_runtime_dir();
    std::fs::create_dir_all(&dir).expect("mkdir runtime dir");
    let outside = dir.join("outside-items");
    let link = dir.join("items");
    std::fs::create_dir_all(&outside).expect("mkdir outside");
    std::os::unix::fs::symlink(&outside, &link).expect("symlink items dir");

    let err = RuntimeThreadStore::open(dir.clone()).expect_err("symlink items dir should fail");
    assert!(
        format!("{err:#}").contains("directory must not be a symlink"),
        "got: {err:#}"
    );

    let _ = std::fs::remove_dir_all(dir);
}

#[cfg(unix)]
#[test]
fn store_list_items_rejects_symlinked_item_file() {
    let dir = test_runtime_dir();
    let store = RuntimeThreadStore::open(dir.clone()).expect("open store");
    let item = sample_item("turn_link", "item_link", TurnItemLifecycleStatus::Completed);
    let target = dir.join("outside-item.json");
    let link = store.items_dir.join(format!("{}.json", item.id));
    std::fs::write(&target, serde_json::to_string(&item).unwrap()).expect("write target");
    std::os::unix::fs::symlink(&target, &link).expect("symlink item");

    let err = store
        .list_items_for_turn(&item.turn_id)
        .expect_err("symlink item should fail");
    assert!(format!("{err:#}").contains("must not be a symlink"));

    let _ = std::fs::remove_dir_all(dir);
}

#[cfg(unix)]
#[test]
fn store_list_items_rejects_swapped_symlinked_store_directory() {
    let dir = test_runtime_dir();
    let store = RuntimeThreadStore::open(dir.clone()).expect("open store");
    let outside = dir.join("outside-items");
    std::fs::create_dir_all(&outside).expect("mkdir outside");
    std::fs::remove_dir_all(&store.items_dir).expect("remove items dir");
    std::os::unix::fs::symlink(&outside, &store.items_dir).expect("symlink items dir");

    let err = store
        .list_items_for_turn("turn_link")
        .expect_err("swapped symlink items dir should fail");
    assert!(
        format!("{err:#}").contains("directory must not be a symlink"),
        "got: {err:#}"
    );

    let _ = std::fs::remove_dir_all(dir);
}

#[test]
fn store_load_thread_defaults_missing_session_id() {
    let dir = test_runtime_dir();
    let store = RuntimeThreadStore::open(dir.clone()).expect("open store");
    let thread = sample_thread("thr_legacy_session");
    let path = store.threads_dir.join(format!("{}.json", thread.id));
    std::fs::create_dir_all(path.parent().unwrap()).expect("mkdirs");
    let mut payload = serde_json::to_value(&thread).expect("serialize thread");
    payload
        .as_object_mut()
        .expect("thread object")
        .remove("session_id");
    std::fs::write(
        &path,
        serde_json::to_string(&payload).expect("encode thread"),
    )
    .expect("write thread");

    let loaded = store
        .load_thread(&thread.id)
        .expect("legacy thread should load");
    assert_eq!(loaded.session_id, None);

    let _ = std::fs::remove_dir_all(dir);
}

#[tokio::test]
async fn seed_thread_keeps_tool_results_on_preceding_turn() -> Result<()> {
    let dir = test_runtime_dir();
    let manager = test_manager(dir.clone())?;
    let thread = sample_thread("thr_seed_blocks");
    manager.store.save_thread(&thread)?;
    let messages = vec![
        Message {
            role: "user".to_string(),
            content: vec![ContentBlock::Text {
                text: "check the files".to_string(),
                cache_control: None,
            }],
        },
        Message {
            role: "assistant".to_string(),
            content: vec![
                ContentBlock::Thinking {
                    thinking: "need a tool".to_string(),
                    signature: Some("sig-1".to_string()),
                },
                ContentBlock::ToolUse {
                    id: "tool-1".to_string(),
                    name: "shell".to_string(),
                    input: json!({ "cmd": "one" }),
                    caller: None,
                },
                ContentBlock::ToolUse {
                    id: "tool-2".to_string(),
                    name: "shell".to_string(),
                    input: json!({ "cmd": "two" }),
                    caller: None,
                },
            ],
        },
        Message {
            role: "user".to_string(),
            content: vec![ContentBlock::ToolResult {
                tool_use_id: "tool-1".to_string(),
                content: "one".to_string(),
                is_error: None,
                content_blocks: Some(vec![json!({
                    "type": "text",
                    "text": "structured one"
                })]),
            }],
        },
        Message {
            role: "user".to_string(),
            content: vec![ContentBlock::ToolResult {
                tool_use_id: "tool-2".to_string(),
                content: "two".to_string(),
                is_error: Some(true),
                content_blocks: None,
            }],
        },
        Message {
            role: "assistant".to_string(),
            content: vec![ContentBlock::Text {
                text: "done".to_string(),
                cache_control: None,
            }],
        },
    ];

    manager
        .seed_thread_from_messages(&thread.id, &messages)
        .await?;
    let turns = manager.store.list_turns_for_thread(&thread.id)?;
    assert_eq!(turns.len(), 1);

    let restored = manager.reconstruct_messages_from_turns(&turns)?;
    let roles = restored
        .iter()
        .map(|message| message.role.as_str())
        .collect::<Vec<_>>();
    assert_eq!(roles, vec!["user", "assistant", "user", "assistant"]);
    assert_eq!(restored[2].content.len(), 2);

    match &restored[2].content[0] {
        ContentBlock::ToolResult {
            tool_use_id,
            content,
            is_error,
            content_blocks,
        } => {
            assert_eq!(tool_use_id, "tool-1");
            assert_eq!(content, "one");
            assert_eq!(*is_error, None);
            assert_eq!(
                content_blocks
                    .as_ref()
                    .and_then(|blocks| blocks[0].get("text")),
                Some(&json!("structured one"))
            );
        }
        other => panic!("expected first tool result, got {other:?}"),
    }
    match &restored[2].content[1] {
        ContentBlock::ToolResult {
            tool_use_id,
            content,
            is_error,
            content_blocks,
        } => {
            assert_eq!(tool_use_id, "tool-2");
            assert_eq!(content, "two");
            assert_eq!(*is_error, Some(true));
            assert!(content_blocks.is_none());
        }
        other => panic!("expected second tool result, got {other:?}"),
    }

    let _ = std::fs::remove_dir_all(dir);
    Ok(())
}

#[test]
fn current_runtime_schema_version_is_two_on_v066() {
    // Locks the bump in (issue #124). Bump deliberately when persisted
    // shape changes.
    assert_eq!(CURRENT_RUNTIME_SCHEMA_VERSION, 2);
}

#[test]
fn store_rejects_path_like_record_ids() {
    let dir = test_runtime_dir();
    let store = RuntimeThreadStore::open(dir.clone()).expect("open store");

    let err = store
        .load_thread("../outside")
        .expect_err("path traversal id should fail");
    assert!(
        format!("{err:#}").contains("unsupported characters"),
        "got: {err:#}"
    );

    let mut thread = sample_thread("thr_bad/id");
    let err = store
        .save_thread(&thread)
        .expect_err("path separator id should fail");
    assert!(
        format!("{err:#}").contains("unsupported characters"),
        "got: {err:#}"
    );

    thread.id = " thr_bad".to_string();
    let err = store
        .save_thread(&thread)
        .expect_err("whitespace id should fail");
    assert!(format!("{err:#}").contains("whitespace"), "got: {err:#}");

    let _ = std::fs::remove_dir_all(dir);
}

#[test]
fn store_load_turn_rejects_newer_schema_version() {
    let dir = test_runtime_dir();
    let store = RuntimeThreadStore::open(dir.clone()).expect("open store");

    let mut turn = sample_turn("thr_t", "trn_future", RuntimeTurnStatus::InProgress);
    turn.schema_version = CURRENT_RUNTIME_SCHEMA_VERSION + 1;

    let path = store.turns_dir.join(format!("{}.json", turn.id));
    std::fs::create_dir_all(path.parent().unwrap()).expect("mkdirs");
    std::fs::write(&path, serde_json::to_string(&turn).expect("serialize turn"))
        .expect("write turn");

    let err = store
        .load_turn(&turn.id)
        .expect_err("load_turn must reject newer schema");
    assert!(
        format!("{err:#}").contains("newer than supported"),
        "got: {err:#}"
    );

    let _ = std::fs::remove_dir_all(dir);
}

#[test]
fn store_load_item_rejects_newer_schema_version() {
    let dir = test_runtime_dir();
    let store = RuntimeThreadStore::open(dir.clone()).expect("open store");

    let mut item = sample_item("trn_t", "itm_future", TurnItemLifecycleStatus::InProgress);
    item.schema_version = CURRENT_RUNTIME_SCHEMA_VERSION + 1;

    let path = store.items_dir.join(format!("{}.json", item.id));
    std::fs::create_dir_all(path.parent().unwrap()).expect("mkdirs");
    std::fs::write(&path, serde_json::to_string(&item).expect("serialize item"))
        .expect("write item");

    let err = store
        .load_item(&item.id)
        .expect_err("load_item must reject newer schema");
    assert!(
        format!("{err:#}").contains("newer than supported"),
        "got: {err:#}"
    );

    let _ = std::fs::remove_dir_all(dir);
}

#[test]
fn enforce_lru_capacity_does_not_loop_when_all_threads_are_active() {
    let mut active = ActiveThreads::default();
    let harness_a = mock_engine_handle();
    let harness_b = mock_engine_handle();

    active.engines.insert(
        "thr_a".to_string(),
        ActiveThreadState {
            engine: harness_a.handle,
            active_turn: Some(ActiveTurnState {
                turn_id: "turn_a".to_string(),
                interrupt_requested: false,
                auto_approve: true,
                trust_mode: false,
            }),
            route_identity: crate::config::ProviderIdentity {
                provider: ApiProvider::Deepseek,
                key: "deepseek".to_string(),
                exact_id: Some("deepseek".to_string()),
            },
            route_model: DEFAULT_TEXT_MODEL.to_string(),
            client_preflight_required: false,
        },
    );
    active.engines.insert(
        "thr_b".to_string(),
        ActiveThreadState {
            engine: harness_b.handle,
            active_turn: Some(ActiveTurnState {
                turn_id: "turn_b".to_string(),
                interrupt_requested: false,
                auto_approve: true,
                trust_mode: false,
            }),
            route_identity: crate::config::ProviderIdentity {
                provider: ApiProvider::Deepseek,
                key: "deepseek".to_string(),
                exact_id: Some("deepseek".to_string()),
            },
            route_model: DEFAULT_TEXT_MODEL.to_string(),
            client_preflight_required: false,
        },
    );
    active.lru.push_back("thr_a".to_string());
    active.lru.push_back("thr_b".to_string());

    let evicted = enforce_lru_capacity(&mut active, 2);
    assert!(evicted.is_empty(), "no idle threads should be evicted");
    assert_eq!(active.engines.len(), 2);
    assert_eq!(active.lru.len(), 2);
}

#[test]
fn approval_decision_keeps_trust_mode_out_of_tool_approval() {
    assert!(matches!(
        RuntimeThreadManager::approval_decision(false, false, false),
        RuntimeApprovalDecision::DenyTool
    ));
    assert!(matches!(
        RuntimeThreadManager::approval_decision(false, true, false),
        RuntimeApprovalDecision::DenyTool
    ));
    assert!(matches!(
        RuntimeThreadManager::approval_decision(true, false, false),
        RuntimeApprovalDecision::ApproveTool
    ));
    assert!(matches!(
        RuntimeThreadManager::approval_decision(true, false, true),
        RuntimeApprovalDecision::DenyTool
    ));
    assert!(matches!(
        RuntimeThreadManager::approval_decision(true, true, true),
        RuntimeApprovalDecision::RetryWithFullAccess
    ));
}

#[test]
fn open_recovers_queued_and_in_progress_turns() -> Result<()> {
    let runtime_dir = test_runtime_dir();
    let store = RuntimeThreadStore::open(runtime_dir.clone())?;
    let thread = sample_thread("thr_recover");
    store.save_thread(&thread)?;

    let mut queued_turn = sample_turn(&thread.id, "turn_queued", RuntimeTurnStatus::Queued);
    let mut in_progress_turn =
        sample_turn(&thread.id, "turn_running", RuntimeTurnStatus::InProgress);
    let completed_turn = sample_turn(&thread.id, "turn_done", RuntimeTurnStatus::Completed);

    let queued_item = sample_item(
        &queued_turn.id,
        "item_queued",
        TurnItemLifecycleStatus::Queued,
    );
    let in_progress_item = sample_item(
        &in_progress_turn.id,
        "item_running",
        TurnItemLifecycleStatus::InProgress,
    );
    let completed_item = sample_item(
        &completed_turn.id,
        "item_done",
        TurnItemLifecycleStatus::Completed,
    );

    queued_turn.item_ids = vec![queued_item.id.clone()];
    in_progress_turn.item_ids = vec![in_progress_item.id.clone()];

    store.save_item(&queued_item)?;
    store.save_item(&in_progress_item)?;
    store.save_item(&completed_item)?;
    store.save_turn(&queued_turn)?;
    store.save_turn(&in_progress_turn)?;
    store.save_turn(&completed_turn)?;

    let manager = test_manager(runtime_dir)?;

    let queued_turn = manager.store.load_turn(&queued_turn.id)?;
    assert_eq!(queued_turn.status, RuntimeTurnStatus::Interrupted);
    assert_eq!(queued_turn.error.as_deref(), Some(RUNTIME_RESTART_REASON));
    assert!(queued_turn.ended_at.is_some());
    assert!(queued_turn.duration_ms.is_some());

    let in_progress_turn = manager.store.load_turn(&in_progress_turn.id)?;
    assert_eq!(in_progress_turn.status, RuntimeTurnStatus::Interrupted);
    assert_eq!(
        in_progress_turn.error.as_deref(),
        Some(RUNTIME_RESTART_REASON)
    );
    assert!(in_progress_turn.ended_at.is_some());
    assert!(in_progress_turn.duration_ms.is_some());

    let completed_turn = manager.store.load_turn(&completed_turn.id)?;
    assert_eq!(completed_turn.status, RuntimeTurnStatus::Completed);
    assert!(completed_turn.error.is_none());

    let queued_item = manager.store.load_item("item_queued")?;
    assert_eq!(queued_item.status, TurnItemLifecycleStatus::Interrupted);
    assert!(queued_item.ended_at.is_some());

    let in_progress_item = manager.store.load_item("item_running")?;
    assert_eq!(
        in_progress_item.status,
        TurnItemLifecycleStatus::Interrupted
    );
    assert!(in_progress_item.ended_at.is_some());

    let completed_item = manager.store.load_item("item_done")?;
    assert_eq!(completed_item.status, TurnItemLifecycleStatus::Completed);

    Ok(())
}

#[tokio::test]
async fn thread_lifecycle_persists_across_restart() -> Result<()> {
    let runtime_dir = test_runtime_dir();
    let manager = test_manager(runtime_dir.clone())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let tx_event = harness.tx_event;
    tokio::spawn(async move {
        if matches!(rx_op.recv().await, Some(Op::SendMessage { .. })) {
            let _ = tx_event
                .send(EngineEvent::TurnStarted {
                    turn_id: "engine_turn_1".to_string(),
                    created_at: chrono::Utc::now(),
                    route: None,
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageStarted { index: 0 })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageDelta {
                    index: 0,
                    content: "mock response".to_string(),
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageComplete { index: 0 })
                .await;
            let _ = tx_event
                .send(EngineEvent::TurnComplete {
                    usage: Usage {
                        input_tokens: 10,
                        output_tokens: 12,
                        ..Usage::default()
                    },
                    status: TurnOutcomeStatus::Completed,
                    error: None,
                    tool_catalog: None,
                    base_url: None,
                })
                .await;
        }
    });

    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "first prompt".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;
    let completed = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(completed.status, RuntimeTurnStatus::Completed);

    drop(manager);

    let reopened = test_manager(runtime_dir)?;
    let detail = reopened.get_thread_detail(&thread.id).await?;
    assert_eq!(detail.thread.id, thread.id);
    assert_eq!(detail.turns.len(), 1);
    assert!(detail.latest_seq >= 1);
    assert!(!detail.items.is_empty());
    let events = reopened.events_since(&thread.id, None)?;
    assert!(
        events.iter().any(|ev| ev.event == "turn.completed"),
        "expected turn.completed event after restart"
    );
    Ok(())
}

#[tokio::test]
async fn completed_turn_without_engine_output_fails() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let tx_event = harness.tx_event;
    tokio::spawn(async move {
        if matches!(rx_op.recv().await, Some(Op::SendMessage { .. })) {
            let _ = tx_event
                .send(EngineEvent::TurnStarted {
                    turn_id: "engine_empty_turn".to_string(),
                    created_at: chrono::Utc::now(),
                    route: None,
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::TurnComplete {
                    usage: Usage {
                        input_tokens: 10,
                        output_tokens: 0,
                        ..Usage::default()
                    },
                    status: TurnOutcomeStatus::Completed,
                    error: None,
                    tool_catalog: None,
                    base_url: None,
                })
                .await;
        }
    });

    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "empty turn".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;

    let failed = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(failed.status, RuntimeTurnStatus::Failed);
    assert_eq!(failed.error.as_deref(), Some(EMPTY_TURN_REASON));

    let events = manager.events_since(&thread.id, None)?;
    assert!(events.iter().any(|ev| {
        ev.event == "item.failed"
            && ev
                .payload
                .get("item")
                .and_then(|item| item.get("kind"))
                .and_then(Value::as_str)
                == Some("error")
    }));
    assert!(events.iter().any(|ev| {
        ev.event == "turn.completed"
            && ev
                .payload
                .get("turn")
                .and_then(|turn| turn.get("status"))
                .and_then(Value::as_str)
                == Some("failed")
    }));
    Ok(())
}

#[tokio::test]
async fn preturn_control_status_does_not_make_empty_turn_succeed() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let tx_event = harness.tx_event;
    tokio::spawn(async move {
        if matches!(rx_op.recv().await, Some(Op::SendMessage { .. })) {
            let _ = tx_event
                .send(EngineEvent::AgentComplete {
                    id: "stale_agent".to_string(),
                    result: "stale completion".to_string(),
                    role: None,
                    failed: false,
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::status("Compaction settings updated"))
                .await;
            let _ = tx_event
                .send(EngineEvent::TurnStarted {
                    turn_id: "engine_empty_after_control_status".to_string(),
                    created_at: chrono::Utc::now(),
                    route: None,
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::TurnComplete {
                    usage: Usage::default(),
                    status: TurnOutcomeStatus::Completed,
                    error: None,
                    tool_catalog: None,
                    base_url: None,
                })
                .await;
        }
    });

    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "empty after setup".to_string(),
                ..Default::default()
            },
        )
        .await?;
    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Failed);
    assert_eq!(terminal.error.as_deref(), Some(EMPTY_TURN_REASON));
    assert!(
        manager
            .store
            .list_items_for_turn(&turn.id)?
            .iter()
            .all(|item| {
                item.summary != "Compaction settings updated"
                    && !item.summary.contains("stale_agent")
            })
    );
    Ok(())
}

#[tokio::test]
async fn engine_error_remains_failed_after_nominal_turn_complete() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let tx_event = harness.tx_event;
    tokio::spawn(async move {
        if matches!(rx_op.recv().await, Some(Op::SendMessage { .. })) {
            let _ = tx_event
                .send(EngineEvent::TurnStarted {
                    turn_id: "engine_error_then_complete".to_string(),
                    created_at: chrono::Utc::now(),
                    route: None,
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::error(
                    crate::error_taxonomy::ErrorEnvelope::fatal("provider exploded"),
                ))
                .await;
            let _ = tx_event
                .send(EngineEvent::TurnComplete {
                    usage: Usage::default(),
                    status: TurnOutcomeStatus::Completed,
                    error: None,
                    tool_catalog: None,
                    base_url: None,
                })
                .await;
        }
    });

    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "surface the failure".to_string(),
                ..Default::default()
            },
        )
        .await?;
    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Failed);
    assert_eq!(terminal.error.as_deref(), Some("provider exploded"));
    Ok(())
}

#[tokio::test]
async fn create_thread_defaults_auto_approve_to_false() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    assert!(!thread.auto_approve);
    Ok(())
}

#[tokio::test]
async fn update_thread_workspace_persists_event_and_evicts_idle_engine() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let old_workspace = std::env::temp_dir().join("codewhale-runtime-old-workspace");
    let new_workspace = std::env::temp_dir().join("codewhale-runtime-new-workspace");
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: Some(old_workspace.clone()),
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;

    let updated = manager
        .update_thread(
            &thread.id,
            UpdateThreadRequest {
                workspace: Some(new_workspace.clone()),
                ..UpdateThreadRequest::default()
            },
        )
        .await?;

    assert_eq!(updated.workspace, new_workspace);
    assert_eq!(
        manager.store.load_thread(&thread.id)?.workspace,
        new_workspace
    );
    {
        let active = manager.active.lock().await;
        assert!(
            !active.engines.contains_key(&thread.id),
            "workspace changes must evict the stale cached engine"
        );
        assert!(!active.lru.iter().any(|id| id == &thread.id));
    }

    match tokio::time::timeout(Duration::from_secs(1), rx_op.recv()).await {
        Ok(Some(Op::Shutdown)) => {}
        other => panic!("expected cached engine shutdown, got {other:?}"),
    }

    let events = manager.events_since(&thread.id, None)?;
    let event = events
        .iter()
        .rev()
        .find(|event| event.event == "thread.updated")
        .expect("thread.updated event");
    let workspace_value = serde_json::to_value(&updated.workspace)?;
    assert_eq!(
        event
            .payload
            .get("changes")
            .and_then(|changes| changes.get("workspace")),
        Some(&workspace_value)
    );
    Ok(())
}

#[tokio::test]
async fn update_thread_workspace_rejects_empty_path() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let err = manager
        .update_thread(
            &thread.id,
            UpdateThreadRequest {
                workspace: Some(PathBuf::new()),
                ..UpdateThreadRequest::default()
            },
        )
        .await
        .expect_err("empty workspace must be rejected");
    assert!(format!("{err:#}").contains("workspace must not be empty"));
    Ok(())
}

#[tokio::test]
async fn update_thread_workspace_rejects_active_turn() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let old_workspace = std::env::temp_dir().join("codewhale-runtime-active-old");
    let new_workspace = std::env::temp_dir().join("codewhale-runtime-active-new");
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: Some(old_workspace.clone()),
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    {
        let mut active = manager.active.lock().await;
        let state = active.engines.get_mut(&thread.id).expect("mock engine");
        state.active_turn = Some(ActiveTurnState {
            turn_id: "turn_live".to_string(),
            interrupt_requested: false,
            auto_approve: false,
            trust_mode: false,
        });
    }

    let err = manager
        .update_thread(
            &thread.id,
            UpdateThreadRequest {
                workspace: Some(new_workspace),
                ..UpdateThreadRequest::default()
            },
        )
        .await
        .expect_err("workspace update during active turn must fail");

    assert!(format!("{err:#}").contains("active turn"));
    assert_eq!(
        manager.store.load_thread(&thread.id)?.workspace,
        old_workspace
    );
    {
        let active = manager.active.lock().await;
        assert!(
            active.engines.contains_key(&thread.id),
            "active engine should stay cached after rejected update"
        );
    }
    assert!(
        tokio::time::timeout(Duration::from_millis(100), rx_op.recv())
            .await
            .is_err(),
        "rejected workspace update must not shut down the active engine"
    );
    Ok(())
}

#[tokio::test]
async fn start_turn_passes_effective_auto_approve_to_engine() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: Some(false),
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;

    let _turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "override approval".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: Some(true),
                ..Default::default()
            },
        )
        .await?;

    match rx_op.recv().await {
        Some(Op::SendMessage { auto_approve, .. }) => assert!(auto_approve),
        other => panic!("expected SendMessage op, got {other:?}"),
    }

    Ok(())
}

#[tokio::test]
async fn start_turn_can_override_thread_auto_approve_to_false() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: Some(true),
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;

    let _turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "disable approval".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: Some(false),
                ..Default::default()
            },
        )
        .await?;

    match rx_op.recv().await {
        Some(Op::SendMessage { auto_approve, .. }) => assert!(!auto_approve),
        other => panic!("expected SendMessage op, got {other:?}"),
    }

    Ok(())
}

#[tokio::test]
async fn compact_thread_preserves_thread_auto_approve_policy() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: Some(false),
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;

    let turn = manager
        .compact_thread(&thread.id, CompactThreadRequest::default())
        .await?;

    assert!(matches!(
        rx_op.recv().await,
        Some(Op::CompactContext { .. })
    ));
    assert_eq!(
        manager.active_turn_flags(&thread.id, &turn.id).await,
        Some((false, false))
    );

    Ok(())
}

#[tokio::test]
async fn closed_compaction_mailbox_rolls_back_durable_records_and_active_claim() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let harness = install_mock_engine(&manager, &thread.id).await;
    let before_active = {
        let active = manager.active.lock().await;
        let state = active.engines.get(&thread.id).expect("installed engine");
        (
            state.active_turn.as_ref().map(|turn| turn.turn_id.clone()),
            state.route_identity.clone(),
            state.route_model.clone(),
            active.lru.clone(),
        )
    };
    let before_thread = serde_json::to_value(manager.get_thread(&thread.id).await?)?;
    let before_events = serde_json::to_value(manager.events_since(&thread.id, None)?)?;
    drop(harness.rx_op);

    let error = manager
        .compact_thread(&thread.id, CompactThreadRequest::default())
        .await
        .expect_err("closed mailbox must reject compaction")
        .to_string();
    assert!(error.contains("Failed to trigger compaction"), "{error}");

    assert!(manager.store.list_turns_for_thread(&thread.id)?.is_empty());
    assert_eq!(
        serde_json::to_value(manager.get_thread(&thread.id).await?)?,
        before_thread
    );
    assert_eq!(
        serde_json::to_value(manager.events_since(&thread.id, None)?)?,
        before_events
    );
    let after_active = {
        let active = manager.active.lock().await;
        let state = active.engines.get(&thread.id).expect("installed engine");
        (
            state.active_turn.as_ref().map(|turn| turn.turn_id.clone()),
            state.route_identity.clone(),
            state.route_model.clone(),
            active.lru.clone(),
        )
    };
    assert_eq!(after_active, before_active);
    Ok(())
}

#[tokio::test]
async fn compact_thread_receipt_keeps_exact_named_custom_identity() -> Result<()> {
    let mut custom = std::collections::HashMap::new();
    custom.insert(
        "lm-studio".to_string(),
        crate::config::ProviderConfig {
            kind: Some("openai-compatible".to_string()),
            base_url: Some("http://127.0.0.1:1234/v1".to_string()),
            model: Some("local-code-model".to_string()),
            ..Default::default()
        },
    );
    let manager = RuntimeThreadManager::open(
        Config {
            provider: Some("lm-studio".to_string()),
            providers: Some(crate::config::ProvidersConfig {
                custom,
                ..Default::default()
            }),
            ..Default::default()
        },
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;

    let turn = manager
        .compact_thread(&thread.id, CompactThreadRequest::default())
        .await?;

    assert!(matches!(
        rx_op.recv().await,
        Some(Op::CompactContext { .. })
    ));
    assert_eq!(turn.effective_provider.as_deref(), Some("custom"));
    assert_eq!(turn.effective_provider_id.as_deref(), Some("lm-studio"));
    Ok(())
}

#[tokio::test]
async fn compact_thread_with_real_engine_reaches_terminal_status() -> Result<()> {
    let manager = RuntimeThreadManager::open(
        Config {
            // This test intentionally crosses the real-engine boundary. Give
            // client preflight a hermetic credential and closed-loopback URL;
            // the assertion permits the resulting terminal failure.
            api_key: Some("runtime-thread-test-key".to_string()),
            base_url: Some("http://127.0.0.1:1/v1".to_string()),
            ..Config::default()
        },
        PathBuf::from("."),
        test_manager_config(test_runtime_dir()),
    )?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let turn = manager
        .compact_thread(&thread.id, CompactThreadRequest::default())
        .await?;
    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;

    assert!(matches!(
        terminal.status,
        RuntimeTurnStatus::Completed | RuntimeTurnStatus::Failed
    ));
    assert!(
        terminal.ended_at.is_some(),
        "manual compaction should reach a terminal turn state"
    );
    assert_eq!(manager.active_turn_flags(&thread.id, &turn.id).await, None);

    let expected_status = match terminal.status {
        RuntimeTurnStatus::Completed => "completed",
        RuntimeTurnStatus::Failed => "failed",
        other => panic!("unexpected non-terminal compaction status: {other:?}"),
    };
    let events = manager.events_since(&thread.id, None)?;
    assert!(events.iter().any(|ev| {
        ev.event == "turn.completed"
            && ev
                .payload
                .get("turn")
                .and_then(|turn| turn.get("status"))
                .and_then(Value::as_str)
                == Some(expected_status)
    }));
    Ok(())
}

#[tokio::test]
async fn multi_turn_continuity_same_thread() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let tx_event = harness.tx_event;
    tokio::spawn(async move {
        let mut turn_index = 0u8;
        while let Some(op) = rx_op.recv().await {
            if !matches!(op, Op::SendMessage { .. }) {
                continue;
            }
            turn_index = turn_index.saturating_add(1);
            let _ = tx_event
                .send(EngineEvent::TurnStarted {
                    turn_id: format!("engine_turn_{turn_index}"),
                    created_at: chrono::Utc::now(),
                    route: None,
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageStarted { index: 0 })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageDelta {
                    index: 0,
                    content: format!("reply {turn_index}"),
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageComplete { index: 0 })
                .await;
            let _ = tx_event
                .send(EngineEvent::TurnComplete {
                    usage: Usage {
                        input_tokens: 5,
                        output_tokens: 5,
                        ..Usage::default()
                    },
                    status: TurnOutcomeStatus::Completed,
                    error: None,
                    tool_catalog: None,
                    base_url: None,
                })
                .await;
            if turn_index >= 2 {
                break;
            }
        }
    });

    let turn_1 = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "first".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;
    let turn_1 = wait_for_terminal_turn(&manager, &turn_1.id, Duration::from_secs(2)).await?;
    assert_eq!(turn_1.status, RuntimeTurnStatus::Completed);

    let turn_2 = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "second".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;
    let turn_2 = wait_for_terminal_turn(&manager, &turn_2.id, Duration::from_secs(2)).await?;
    assert_eq!(turn_2.status, RuntimeTurnStatus::Completed);

    let detail = manager.get_thread_detail(&thread.id).await?;
    assert_eq!(
        detail.thread.latest_turn_id.as_deref(),
        Some(turn_2.id.as_str())
    );
    assert_eq!(detail.turns.len(), 2);
    assert!(detail.items.iter().any(|item| {
        item.kind == TurnItemKind::UserMessage && item.detail.as_deref() == Some("first")
    }));
    assert!(detail.items.iter().any(|item| {
        item.kind == TurnItemKind::UserMessage && item.detail.as_deref() == Some("second")
    }));

    let events = manager.events_since(&thread.id, None)?;
    let started = events
        .iter()
        .filter(|ev| ev.event == "turn.started")
        .count();
    let completed = events
        .iter()
        .filter(|ev| ev.event == "turn.completed")
        .count();
    assert_eq!(started, 2);
    assert_eq!(completed, 2);
    Ok(())
}

#[tokio::test]
async fn get_thread_detail_batches_items_by_turn_without_losing_order() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let base = Utc::now();
    let mut first_turn = sample_turn(
        &thread.id,
        "turn_detail_batch_first",
        RuntimeTurnStatus::Completed,
    );
    first_turn.created_at = base;
    let mut second_turn = sample_turn(
        &thread.id,
        "turn_detail_batch_second",
        RuntimeTurnStatus::Completed,
    );
    second_turn.created_at = base + chrono::Duration::seconds(1);
    manager.store.save_turn(&first_turn)?;
    manager.store.save_turn(&second_turn)?;

    let mut first_late = sample_item(
        &first_turn.id,
        "item_detail_first_late",
        TurnItemLifecycleStatus::Completed,
    );
    first_late.started_at = Some(base + chrono::Duration::seconds(5));
    let mut first_early = sample_item(
        &first_turn.id,
        "item_detail_first_early",
        TurnItemLifecycleStatus::Completed,
    );
    first_early.started_at = Some(base + chrono::Duration::seconds(1));
    let mut second_item = sample_item(
        &second_turn.id,
        "item_detail_second",
        TurnItemLifecycleStatus::Completed,
    );
    second_item.started_at = Some(base + chrono::Duration::seconds(2));
    let unrelated = sample_item(
        "turn_detail_batch_unrelated",
        "item_detail_unrelated",
        TurnItemLifecycleStatus::Completed,
    );

    manager.store.save_item(&first_late)?;
    manager.store.save_item(&second_item)?;
    manager.store.save_item(&unrelated)?;
    manager.store.save_item(&first_early)?;

    let detail = manager.get_thread_detail(&thread.id).await?;
    let item_ids: Vec<&str> = detail.items.iter().map(|item| item.id.as_str()).collect();
    assert_eq!(
        item_ids,
        vec![
            "item_detail_first_early",
            "item_detail_first_late",
            "item_detail_second"
        ]
    );
    Ok(())
}

#[tokio::test]
async fn interrupt_turn_marks_interrupted_after_cleanup() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let tx_event = harness.tx_event;
    let cancel_token = harness.cancel_token;
    let cleanup_delay = Duration::from_millis(140);
    tokio::spawn(async move {
        if matches!(rx_op.recv().await, Some(Op::SendMessage { .. })) {
            let _ = tx_event
                .send(EngineEvent::TurnStarted {
                    turn_id: "engine_turn_interrupt".to_string(),
                    created_at: chrono::Utc::now(),
                    route: None,
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageStarted { index: 0 })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageDelta {
                    index: 0,
                    content: "partial".to_string(),
                })
                .await;
            cancel_token.cancelled().await;
            sleep(cleanup_delay).await;
        }
    });

    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "interrupt me".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;

    sleep(Duration::from_millis(20)).await;
    let interrupted_at = Instant::now();
    let interrupt_result = manager.interrupt_turn(&thread.id, &turn.id).await?;
    assert_eq!(interrupt_result.status, RuntimeTurnStatus::InProgress);

    let final_turn = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(3)).await?;
    assert_eq!(final_turn.status, RuntimeTurnStatus::Interrupted);
    assert!(
        interrupted_at.elapsed() >= cleanup_delay,
        "turn transitioned before cleanup finished"
    );

    let events = manager.events_since(&thread.id, None)?;
    let interrupt_seq = events
        .iter()
        .find(|ev| ev.event == "turn.interrupt_requested")
        .map(|ev| ev.seq)
        .context("missing turn.interrupt_requested event")?;
    let completed = events
        .iter()
        .find(|ev| ev.event == "turn.completed")
        .context("missing turn.completed event")?;
    assert!(completed.seq > interrupt_seq);
    assert_eq!(
        completed
            .payload
            .get("turn")
            .and_then(|turn| turn.get("status"))
            .and_then(Value::as_str),
        Some("interrupted")
    );
    Ok(())
}

#[tokio::test]
async fn approval_required_with_stale_active_turn_is_denied() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: Some(true),
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let mut harness = install_mock_engine(&manager, &thread.id).await;
    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "needs approval".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: Some(true),
                ..Default::default()
            },
        )
        .await?;

    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::SendMessage { .. })
    ));
    {
        let mut active = manager.active.lock().await;
        let state = active
            .engines
            .get_mut(&thread.id)
            .context("missing active thread state")?;
        state.active_turn = None;
    }

    harness
        .tx_event
        .send(EngineEvent::ApprovalRequired {
            approval_key: "test_key".to_string(),
            approval_grouping_key: "test_key".to_string(),
            id: "tool_stale".to_string(),
            tool_name: "exec_command".to_string(),
            description: "stale approval".to_string(),
            input: serde_json::json!({}),
            intent_summary: None,
            approval_force_prompt: false,
        })
        .await?;

    assert_eq!(
        harness.recv_approval_event().await,
        Some(MockApprovalEvent::Denied {
            id: "tool_stale".to_string(),
        })
    );

    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage {
                input_tokens: 0,
                output_tokens: 0,
                ..Usage::default()
            },
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;

    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Completed);
    Ok(())
}

#[tokio::test]
async fn approval_required_awaits_external_decision_allow() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let mut harness = install_mock_engine(&manager, &thread.id).await;
    let _turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "needs approval".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;
    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::SendMessage { .. })
    ));

    harness
        .tx_event
        .send(EngineEvent::ApprovalRequired {
            approval_key: "key1".to_string(),
            approval_grouping_key: "key1".to_string(),
            id: "tool_external_allow".to_string(),
            tool_name: "exec_command".to_string(),
            description: "external allow".to_string(),
            input: serde_json::json!({}),
            intent_summary: Some("I will update the config file.".to_string()),
            approval_force_prompt: false,
        })
        .await?;

    let deadline = Instant::now() + Duration::from_secs(2);
    while Instant::now() < deadline && manager.pending_approvals_count() == 0 {
        sleep(Duration::from_millis(20)).await;
    }
    assert_eq!(manager.pending_approvals_count(), 1);

    let events = manager.events_since(&thread.id, None)?;
    let approval_event = events
        .iter()
        .rev()
        .find(|event| event.event == "approval.required")
        .context("missing approval.required event")?;
    assert_eq!(
        approval_event
            .payload
            .get("intent_summary")
            .and_then(Value::as_str),
        Some("I will update the config file.")
    );

    assert!(manager.deliver_external_approval(
        "tool_external_allow",
        ExternalApprovalDecision::Allow { remember: false },
    ));
    assert_eq!(
        harness.recv_approval_event().await,
        Some(MockApprovalEvent::Approved {
            id: "tool_external_allow".to_string(),
        })
    );
    assert_eq!(manager.pending_approvals_count(), 0);

    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;
    Ok(())
}

#[tokio::test]
async fn approval_required_external_deny_is_denied() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let mut harness = install_mock_engine(&manager, &thread.id).await;
    let _turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "needs approval".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;
    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::SendMessage { .. })
    ));

    harness
        .tx_event
        .send(EngineEvent::ApprovalRequired {
            approval_key: "key2".to_string(),
            approval_grouping_key: "key2".to_string(),
            id: "tool_external_deny".to_string(),
            tool_name: "exec_command".to_string(),
            description: "external deny".to_string(),
            input: serde_json::json!({}),
            intent_summary: None,
            approval_force_prompt: false,
        })
        .await?;

    let deadline = Instant::now() + Duration::from_secs(2);
    while Instant::now() < deadline && manager.pending_approvals_count() == 0 {
        sleep(Duration::from_millis(20)).await;
    }
    assert_eq!(manager.pending_approvals_count(), 1);

    assert!(manager.deliver_external_approval(
        "tool_external_deny",
        ExternalApprovalDecision::Deny { remember: false },
    ));
    assert_eq!(
        harness.recv_approval_event().await,
        Some(MockApprovalEvent::Denied {
            id: "tool_external_deny".to_string(),
        })
    );

    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;
    Ok(())
}

#[tokio::test]
async fn approval_timeout_denies_clears_ui_and_next_turn_can_start() -> Result<()> {
    let _timeout_guard = test_approval_timeout_ms(25);
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let mut harness = install_mock_engine(&manager, &thread.id).await;
    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "needs approval".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;
    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::SendMessage { .. })
    ));

    harness
        .tx_event
        .send(EngineEvent::ApprovalRequired {
            approval_key: "timeout_key".to_string(),
            approval_grouping_key: "timeout_key".to_string(),
            id: "tool_timeout".to_string(),
            tool_name: "exec_command".to_string(),
            description: "external timeout".to_string(),
            input: serde_json::json!({}),
            intent_summary: None,
            approval_force_prompt: false,
        })
        .await?;

    let decision = tokio::time::timeout(Duration::from_secs(2), harness.recv_approval_event())
        .await
        .context("approval timeout should deny the engine")?;
    assert_eq!(
        decision,
        Some(MockApprovalEvent::Denied {
            id: "tool_timeout".to_string(),
        })
    );
    assert_eq!(manager.pending_approvals_count(), 0);

    let events = manager.events_since(&thread.id, None)?;
    assert!(
        events.iter().any(|event| {
            event.event == "approval.timeout"
                && event.payload.get("approval_id").and_then(Value::as_str) == Some("tool_timeout")
        }),
        "timeout event should be persisted"
    );
    assert!(
        events.iter().any(|event| {
            event.event == "approval.decided"
                && event.payload.get("approval_id").and_then(Value::as_str) == Some("tool_timeout")
                && event.payload.get("decision").and_then(Value::as_str) == Some("deny")
                && event.payload.get("timeout").and_then(Value::as_bool) == Some(true)
        }),
        "timeout should also emit approval.decided so clients can clear pending UI"
    );

    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;
    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Completed);

    let _next = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "after timeout".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;
    assert!(
        matches!(harness.rx_op.recv().await, Some(Op::SendMessage { .. })),
        "thread should accept a fresh turn after approval timeout cleanup"
    );

    Ok(())
}

#[tokio::test]
async fn thinking_delta_emits_agent_reasoning_item() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: Some(true),
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;
    let mut harness = install_mock_engine(&manager, &thread.id).await;
    let mut event_rx = manager.subscribe_events();
    let _turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "show your thinking".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: Some(true),
                ..Default::default()
            },
        )
        .await?;
    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::SendMessage { .. })
    ));

    harness
        .tx_event
        .send(EngineEvent::ThinkingStarted { index: 0 })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::ThinkingDelta {
            index: 0,
            content: "Let me reason about this.".to_string(),
        })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::ThinkingComplete { index: 0 })
        .await?;
    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;

    // A busy or constrained runner can be quiet for more than one 200 ms poll
    // even though the engine is still making progress. Keep polling until the
    // actual deadline instead of treating the first quiet interval as failure.
    let deadline = Instant::now() + Duration::from_secs(5);
    let mut delta_seen = false;
    let mut completed_seen = false;
    while Instant::now() < deadline && (!delta_seen || !completed_seen) {
        match tokio::time::timeout(Duration::from_millis(200), event_rx.recv()).await {
            Ok(Ok(record)) => {
                if record.event == "item.delta"
                    && record.payload.get("kind").and_then(|v| v.as_str())
                        == Some("agent_reasoning")
                {
                    delta_seen = true;
                    assert_eq!(
                        record.payload.get("delta").and_then(|v| v.as_str()),
                        Some("Let me reason about this.")
                    );
                }
                if record.event == "item.completed"
                    && record
                        .payload
                        .get("item")
                        .and_then(|v| v.get("kind"))
                        .and_then(|v| v.as_str())
                        == Some("agent_reasoning")
                {
                    completed_seen = true;
                }
            }
            Ok(Err(_)) => break,
            Err(_) => continue,
        }
    }
    assert!(delta_seen, "expected item.delta with kind=agent_reasoning");
    assert!(
        completed_seen,
        "expected item.completed for the reasoning item"
    );
    Ok(())
}

#[tokio::test]
async fn deliver_external_approval_for_unknown_id_returns_false() {
    let manager = test_manager(test_runtime_dir()).expect("manager");
    assert!(!manager.deliver_external_approval(
        "no_such_approval",
        ExternalApprovalDecision::Allow { remember: false },
    ));
    assert_eq!(manager.pending_approvals_count(), 0);
}

#[tokio::test]
async fn approval_required_remember_flips_thread_auto_approve() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;
    assert!(!manager.store.load_thread(&thread.id)?.auto_approve);

    let mut harness = install_mock_engine(&manager, &thread.id).await;
    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "needs approval".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;
    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::SendMessage { .. })
    ));

    harness
        .tx_event
        .send(EngineEvent::ApprovalRequired {
            approval_key: "key3".to_string(),
            approval_grouping_key: "key3".to_string(),
            id: "tool_remember".to_string(),
            tool_name: "exec_command".to_string(),
            description: "remember=true".to_string(),
            input: serde_json::json!({}),
            intent_summary: None,
            approval_force_prompt: false,
        })
        .await?;

    let deadline = Instant::now() + Duration::from_secs(2);
    while Instant::now() < deadline && manager.pending_approvals_count() == 0 {
        sleep(Duration::from_millis(20)).await;
    }
    assert!(manager.deliver_external_approval(
        "tool_remember",
        ExternalApprovalDecision::Allow { remember: true },
    ));
    let _ = harness.recv_approval_event().await;

    assert!(
        manager.store.load_thread(&thread.id)?.auto_approve,
        "remember=true should flip thread auto_approve"
    );
    assert_eq!(
        manager.active_turn_flags(&thread.id, &turn.id).await,
        Some((true, false)),
        "remember=true should update the active turn used by subsequent approvals"
    );

    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;
    Ok(())
}

#[tokio::test]
async fn elevation_required_with_stale_active_turn_is_denied() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: Some(true),
            auto_approve: Some(true),
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let mut harness = install_mock_engine(&manager, &thread.id).await;
    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "needs elevation".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: Some(true),
                auto_approve: Some(true),
                ..Default::default()
            },
        )
        .await?;

    assert!(matches!(
        harness.rx_op.recv().await,
        Some(Op::SendMessage { .. })
    ));
    {
        let mut active = manager.active.lock().await;
        let state = active
            .engines
            .get_mut(&thread.id)
            .context("missing active thread state")?;
        state.active_turn = None;
    }

    harness
        .tx_event
        .send(EngineEvent::ElevationRequired {
            tool_id: "tool_stale_elevated".to_string(),
            tool_name: "exec_command".to_string(),
            command: None,
            denial_reason: "sandbox denied".to_string(),
            blocked_network: false,
            blocked_write: false,
        })
        .await?;

    assert_eq!(
        harness.recv_approval_event().await,
        Some(MockApprovalEvent::Denied {
            id: "tool_stale_elevated".to_string(),
        })
    );

    harness
        .tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage {
                input_tokens: 0,
                output_tokens: 0,
                ..Usage::default()
            },
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;

    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Completed);
    Ok(())
}

#[tokio::test]
async fn steer_turn_on_active_turn_records_item_and_event() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let mut rx_steer = harness.rx_steer;
    let tx_event = harness.tx_event;
    let (steer_seen_tx, steer_seen_rx) = oneshot::channel::<String>();
    tokio::spawn(async move {
        if matches!(rx_op.recv().await, Some(Op::SendMessage { .. })) {
            let _ = tx_event
                .send(EngineEvent::TurnStarted {
                    turn_id: "engine_turn_steer".to_string(),
                    created_at: chrono::Utc::now(),
                    route: None,
                })
                .await;
            if let Some(steer) = rx_steer.recv().await {
                let _ = steer_seen_tx.send(steer);
            }
            let _ = tx_event
                .send(EngineEvent::MessageStarted { index: 0 })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageDelta {
                    index: 0,
                    content: "steered response".to_string(),
                })
                .await;
            let _ = tx_event
                .send(EngineEvent::MessageComplete { index: 0 })
                .await;
            let _ = tx_event
                .send(EngineEvent::TurnComplete {
                    usage: Usage {
                        input_tokens: 8,
                        output_tokens: 9,
                        ..Usage::default()
                    },
                    status: TurnOutcomeStatus::Completed,
                    error: None,
                    tool_catalog: None,
                    base_url: None,
                })
                .await;
        }
    });

    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "initial".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;

    let steer_text = "add bullet list".to_string();
    let steered_turn = manager
        .steer_turn(
            &thread.id,
            &turn.id,
            SteerTurnRequest {
                prompt: steer_text.clone(),
            },
        )
        .await?;
    assert_eq!(steered_turn.steer_count, 1);
    let observed_steer = steer_seen_rx
        .await
        .context("driver did not receive steer")?;
    assert_eq!(observed_steer, steer_text);

    let final_turn = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(final_turn.status, RuntimeTurnStatus::Completed);
    assert_eq!(final_turn.steer_count, 1);

    let events = manager.events_since(&thread.id, None)?;
    assert!(events.iter().any(|ev| ev.event == "turn.steered"));
    assert!(events.iter().any(|ev| {
        ev.event == "item.completed"
            && ev
                .payload
                .get("item")
                .and_then(|item| item.get("detail"))
                .and_then(Value::as_str)
                == Some("add bullet list")
    }));
    Ok(())
}

#[tokio::test]
async fn steer_receipts_outlive_caller_cancellation_after_engine_acceptance() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let mut rx_steer = harness.rx_steer;
    let tx_event = harness.tx_event;

    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "initial".to_string(),
                ..Default::default()
            },
        )
        .await?;
    assert!(matches!(rx_op.recv().await, Some(Op::SendMessage { .. })));

    // Hold publication after durable persistence and mailbox acceptance so the
    // API future can be cancelled while the detached receipt task is pending.
    let emit_guard = manager.event_emit.lock().await;
    let steer_manager = manager.clone();
    let thread_id = thread.id.clone();
    let turn_id = turn.id.clone();
    let steer_task = tokio::spawn(async move {
        steer_manager
            .steer_turn(
                &thread_id,
                &turn_id,
                SteerTurnRequest {
                    prompt: "keep the accepted steer".to_string(),
                },
            )
            .await
    });
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(2), rx_steer.recv()).await?,
        Some("keep the accepted steer".to_string())
    );
    steer_task.abort();
    assert!(
        steer_task
            .await
            .expect_err("caller task must be cancelled")
            .is_cancelled()
    );
    drop(emit_guard);

    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        let events = manager.events_since(&thread.id, None)?;
        let steered = events.iter().any(|event| event.event == "turn.steered");
        let completed = events.iter().any(|event| {
            event.event == "item.completed"
                && event
                    .payload
                    .get("item")
                    .and_then(|item| item.get("detail"))
                    .and_then(Value::as_str)
                    == Some("keep the accepted steer")
        });
        if steered && completed {
            break;
        }
        if Instant::now() >= deadline {
            bail!("detached steer receipts were not persisted after caller cancellation");
        }
        tokio::task::yield_now().await;
    }

    let persisted_turn = manager.store.load_turn(&turn.id)?;
    assert_eq!(persisted_turn.steer_count, 1);
    let items = manager.store.list_items_for_turn(&turn.id)?;
    let steer_item = items
        .iter()
        .find(|item| item.detail.as_deref() == Some("keep the accepted steer"))
        .context("accepted steer item must remain durable")?;
    assert!(persisted_turn.item_ids.contains(&steer_item.id));

    tx_event
        .send(EngineEvent::MessageStarted { index: 0 })
        .await?;
    tx_event
        .send(EngineEvent::MessageDelta {
            index: 0,
            content: "accepted steer completed".to_string(),
        })
        .await?;
    tx_event
        .send(EngineEvent::MessageComplete { index: 0 })
        .await?;
    tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;
    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Completed);
    Ok(())
}

#[tokio::test]
async fn steer_rejects_a_terminal_durable_turn_without_dispatch_or_item() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let mut rx_steer = harness.rx_steer;
    let tx_event = harness.tx_event;

    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "initial".to_string(),
                ..Default::default()
            },
        )
        .await?;
    assert!(matches!(rx_op.recv().await, Some(Op::SendMessage { .. })));
    let original_item_ids = turn.item_ids.clone();
    {
        let _turn_mutation = manager.store.turn_mutation.lock();
        let mut terminal = manager.store.load_turn(&turn.id)?;
        terminal.status = RuntimeTurnStatus::Completed;
        terminal.ended_at = Some(Utc::now());
        manager.store.save_turn(&terminal)?;
    }

    let error = manager
        .steer_turn(
            &thread.id,
            &turn.id,
            SteerTurnRequest {
                prompt: "must be rejected".to_string(),
            },
        )
        .await
        .expect_err("terminal turn must reject steering");
    assert!(error.to_string().contains("no longer in progress"));
    assert!(
        tokio::time::timeout(Duration::from_millis(100), rx_steer.recv())
            .await
            .is_err(),
        "rejected terminal steer must not reach the engine"
    );
    let persisted = manager.store.load_turn(&turn.id)?;
    assert_eq!(persisted.steer_count, 0);
    assert_eq!(persisted.item_ids, original_item_ids);
    assert_eq!(manager.store.list_items_for_turn(&turn.id)?.len(), 1);

    // Restore the synthetic record and let the real monitor settle normally.
    {
        let _turn_mutation = manager.store.turn_mutation.lock();
        let mut active = manager.store.load_turn(&turn.id)?;
        active.status = RuntimeTurnStatus::InProgress;
        active.ended_at = None;
        manager.store.save_turn(&active)?;
    }
    tx_event
        .send(EngineEvent::MessageStarted { index: 0 })
        .await?;
    tx_event
        .send(EngineEvent::MessageDelta {
            index: 0,
            content: "terminal rejection test completed".to_string(),
        })
        .await?;
    tx_event
        .send(EngineEvent::MessageComplete { index: 0 })
        .await?;
    tx_event
        .send(EngineEvent::TurnComplete {
            usage: Usage::default(),
            status: TurnOutcomeStatus::Completed,
            error: None,
            tool_catalog: None,
            base_url: None,
        })
        .await?;
    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Completed);
    Ok(())
}

#[tokio::test]
async fn concurrent_event_publication_keeps_live_and_durable_sequence_order() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let mut live_rx = manager.subscribe_events();

    let mut emitters = Vec::new();
    for index in 0..24_u64 {
        let emitter = manager.clone();
        let thread_id = thread.id.clone();
        emitters.push(tokio::spawn(async move {
            emitter
                .emit_event(
                    &thread_id,
                    None,
                    None,
                    "test.concurrent",
                    json!({ "index": index }),
                )
                .await
        }));
    }
    for emitter in emitters {
        emitter.await??;
    }

    let mut live = Vec::new();
    for _ in 0..24 {
        live.push(tokio::time::timeout(Duration::from_secs(2), live_rx.recv()).await??);
    }
    assert!(live.windows(2).all(|pair| pair[0].seq < pair[1].seq));

    let durable: Vec<_> = manager
        .events_since(&thread.id, None)?
        .into_iter()
        .filter(|event| event.event == "test.concurrent")
        .collect();
    assert_eq!(durable.len(), 24);
    assert_eq!(
        live.iter()
            .map(|event| (event.seq, event.payload.clone()))
            .collect::<Vec<_>>(),
        durable
            .iter()
            .map(|event| (event.seq, event.payload.clone()))
            .collect::<Vec<_>>(),
        "broadcast order must exactly match append order"
    );
    Ok(())
}

#[tokio::test]
async fn closed_engine_event_stream_fails_turn_items_and_evicts_engine() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest::default())
        .await?;
    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let tx_event = harness.tx_event;

    let turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "engine stream will close".to_string(),
                ..Default::default()
            },
        )
        .await?;
    assert!(matches!(rx_op.recv().await, Some(Op::SendMessage { .. })));
    drop(tx_event);

    let terminal = wait_for_terminal_turn(&manager, &turn.id, Duration::from_secs(2)).await?;
    assert_eq!(terminal.status, RuntimeTurnStatus::Failed);
    let terminal_error = terminal.error.as_deref().unwrap_or_default();
    assert!(
        terminal.error.as_deref().is_some_and(|error| {
            error.contains("Failed to monitor") || error.contains("without producing any output")
        }),
        "unexpected terminal error: {terminal_error:?}"
    );
    assert!(
        manager
            .store
            .list_items_for_turn(&turn.id)?
            .iter()
            .all(|item| !matches!(
                item.status,
                TurnItemLifecycleStatus::Queued | TurnItemLifecycleStatus::InProgress
            ))
    );
    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        if !manager.active.lock().await.engines.contains_key(&thread.id) {
            break;
        }
        if Instant::now() >= deadline {
            bail!("failed engine was not evicted");
        }
        tokio::task::yield_now().await;
    }
    assert!(matches!(rx_op.recv().await, Some(Op::Shutdown)));
    Ok(())
}

#[tokio::test]
async fn compaction_lifecycle_emits_item_events_with_compaction_counts() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;

    let harness = install_mock_engine(&manager, &thread.id).await;
    let mut rx_op = harness.rx_op;
    let tx_event = harness.tx_event;
    tokio::spawn(async move {
        let mut op_count = 0usize;
        while let Some(op) = rx_op.recv().await {
            match op {
                Op::SendMessage { .. } => {
                    op_count = op_count.saturating_add(1);
                    let _ = tx_event
                        .send(EngineEvent::TurnStarted {
                            turn_id: "engine_turn_auto".to_string(),
                            created_at: chrono::Utc::now(),
                            route: None,
                        })
                        .await;
                    let _ = tx_event
                        .send(EngineEvent::CompactionStarted {
                            id: "auto_compact_1".to_string(),
                            auto: true,
                            message: "auto compact begin".to_string(),
                        })
                        .await;
                    let _ = tx_event
                        .send(EngineEvent::CompactionCompleted {
                            id: "auto_compact_1".to_string(),
                            auto: true,
                            message: "auto compact done".to_string(),
                            messages_before: Some(7),
                            messages_after: Some(3),
                            summary_prompt: None,
                        })
                        .await;
                    let _ = tx_event
                        .send(EngineEvent::TurnComplete {
                            usage: Usage {
                                input_tokens: 3,
                                output_tokens: 3,
                                ..Usage::default()
                            },
                            status: TurnOutcomeStatus::Completed,
                            error: None,
                            tool_catalog: None,
                            base_url: None,
                        })
                        .await;
                }
                Op::CompactContext { .. } => {
                    op_count = op_count.saturating_add(1);
                    let _ = tx_event
                        .send(EngineEvent::CompactionStarted {
                            id: "manual_compact_1".to_string(),
                            auto: false,
                            message: "manual compact begin".to_string(),
                        })
                        .await;
                    let _ = tx_event
                        .send(EngineEvent::CompactionCompleted {
                            id: "manual_compact_1".to_string(),
                            auto: false,
                            message: "manual compact done".to_string(),
                            messages_before: Some(5),
                            messages_after: Some(2),
                            summary_prompt: Some(
                                "## 📋 Conversation Summary (Auto-Generated)\n\nkey facts."
                                    .to_string(),
                            ),
                        })
                        .await;
                    let _ = tx_event
                        .send(EngineEvent::TurnComplete {
                            usage: Usage {
                                input_tokens: 1,
                                output_tokens: 1,
                                ..Usage::default()
                            },
                            status: TurnOutcomeStatus::Completed,
                            error: None,
                            tool_catalog: None,
                            base_url: None,
                        })
                        .await;
                }
                _ => {}
            }
            if op_count >= 2 {
                break;
            }
        }
    });

    let auto_turn = manager
        .start_turn(
            &thread.id,
            StartTurnRequest {
                prompt: "trigger auto".to_string(),
                input_summary: None,
                model: None,
                mode: None,
                allow_shell: None,
                trust_mode: None,
                auto_approve: None,
                ..Default::default()
            },
        )
        .await?;
    let auto_turn = wait_for_terminal_turn(&manager, &auto_turn.id, Duration::from_secs(2)).await?;
    assert_eq!(auto_turn.status, RuntimeTurnStatus::Completed);

    let manual_turn = manager
        .compact_thread(
            &thread.id,
            CompactThreadRequest {
                reason: Some("manual request".to_string()),
            },
        )
        .await?;
    let manual_turn =
        wait_for_terminal_turn(&manager, &manual_turn.id, Duration::from_secs(2)).await?;
    assert_eq!(manual_turn.status, RuntimeTurnStatus::Completed);

    let events = manager.events_since(&thread.id, None)?;
    assert!(events.iter().any(|ev| {
        ev.event == "item.started"
            && ev
                .payload
                .get("item")
                .and_then(|item| item.get("kind"))
                .and_then(Value::as_str)
                == Some("context_compaction")
            && ev.payload.get("auto").and_then(Value::as_bool) == Some(true)
    }));
    assert!(events.iter().any(|ev| {
        ev.event == "item.completed"
            && ev
                .payload
                .get("item")
                .and_then(|item| item.get("kind"))
                .and_then(Value::as_str)
                == Some("context_compaction")
            && ev.payload.get("auto").and_then(Value::as_bool) == Some(true)
            && ev.payload.get("messages_before").and_then(Value::as_u64) == Some(7)
            && ev.payload.get("messages_after").and_then(Value::as_u64) == Some(3)
    }));
    assert!(events.iter().any(|ev| {
        ev.event == "item.completed"
            && ev
                .payload
                .get("item")
                .and_then(|item| item.get("kind"))
                .and_then(Value::as_str)
                == Some("context_compaction")
            && ev.payload.get("auto").and_then(Value::as_bool) == Some(false)
            && ev.payload.get("messages_before").and_then(Value::as_u64) == Some(5)
            && ev.payload.get("messages_after").and_then(Value::as_u64) == Some(2)
    }));

    // The manual compact carried a summary_prompt → it must be persisted into
    // the thread record so engine reloads restore it. The auto compact carried
    // None → exactly one summary section, from the manual pass.
    let record = manager.get_thread(&thread.id).await?;
    let record_prompt = record.system_prompt.expect("record keeps a system prompt");
    assert!(record_prompt.contains(COMPACTION_SUMMARY_BEGIN));
    assert!(record_prompt.contains("Conversation Summary (Auto-Generated)"));
    assert!(record_prompt.contains("key facts."));
    assert_eq!(record_prompt.matches(COMPACTION_SUMMARY_BEGIN).count(), 1);
    Ok(())
}

#[test]
fn summarize_text_truncates() {
    let out = summarize_text("abcdefghijklmnopqrstuvwxyz", 10);
    assert_eq!(out, "abcdefg...");
}

#[test]
fn approval_decision_requires_auto_approve_and_trust_for_full_access() {
    assert_eq!(
        RuntimeThreadManager::approval_decision(false, false, false),
        RuntimeApprovalDecision::DenyTool
    );
    assert_eq!(
        RuntimeThreadManager::approval_decision(false, true, false),
        RuntimeApprovalDecision::DenyTool
    );
    assert_eq!(
        RuntimeThreadManager::approval_decision(true, false, false),
        RuntimeApprovalDecision::ApproveTool
    );
    assert_eq!(
        RuntimeThreadManager::approval_decision(true, false, true),
        RuntimeApprovalDecision::DenyTool
    );
    assert_eq!(
        RuntimeThreadManager::approval_decision(true, true, true),
        RuntimeApprovalDecision::RetryWithFullAccess
    );
}

#[test]
fn opening_manager_recovers_stale_queued_and_in_progress_work() -> Result<()> {
    let data_dir = test_runtime_dir();
    let manager = test_manager(data_dir.clone())?;
    let started_at = Utc::now() - chrono::Duration::seconds(5);
    let created_at = started_at - chrono::Duration::seconds(1);

    let thread = ThreadRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        id: "thr_restart".to_string(),
        created_at,
        updated_at: created_at,
        model: DEFAULT_TEXT_MODEL.to_string(),
        model_provider: None,
        model_provider_id: None,
        workspace: PathBuf::from("."),
        mode: "agent".to_string(),
        allow_shell: false,
        trust_mode: false,
        auto_approve: false,
        latest_turn_id: Some("turn_in_progress".to_string()),
        latest_response_bookmark: None,
        archived: false,
        system_prompt: None,
        task_id: None,
        title: None,
        session_id: None,
    };
    manager.store.save_thread(&thread)?;

    let completed_item = TurnItemRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        id: "item_completed".to_string(),
        turn_id: "turn_in_progress".to_string(),
        kind: TurnItemKind::Status,
        status: TurnItemLifecycleStatus::Completed,
        summary: "done".to_string(),
        detail: None,
        metadata: None,
        artifact_refs: Vec::new(),
        started_at: Some(started_at),
        ended_at: Some(started_at + chrono::Duration::seconds(1)),
    };
    let in_progress_item = TurnItemRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        id: "item_in_progress".to_string(),
        turn_id: "turn_in_progress".to_string(),
        kind: TurnItemKind::ToolCall,
        status: TurnItemLifecycleStatus::InProgress,
        summary: "running".to_string(),
        detail: None,
        metadata: None,
        artifact_refs: Vec::new(),
        started_at: Some(started_at),
        ended_at: None,
    };
    let queued_item = TurnItemRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        id: "item_queued".to_string(),
        turn_id: "turn_queued".to_string(),
        kind: TurnItemKind::ToolCall,
        status: TurnItemLifecycleStatus::Queued,
        summary: "queued".to_string(),
        detail: None,
        metadata: None,
        artifact_refs: Vec::new(),
        started_at: None,
        ended_at: None,
    };
    manager.store.save_item(&completed_item)?;
    manager.store.save_item(&in_progress_item)?;
    manager.store.save_item(&queued_item)?;

    manager.store.save_turn(&TurnRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        id: "turn_in_progress".to_string(),
        thread_id: thread.id.clone(),
        status: RuntimeTurnStatus::InProgress,
        input_summary: "hello".to_string(),
        created_at,
        started_at: Some(started_at),
        ended_at: None,
        duration_ms: None,
        usage: None,
        effective_provider: None,
        effective_provider_id: None,
        effective_billing_surface: None,
        effective_model: None,
        error: None,
        item_ids: vec![completed_item.id.clone(), in_progress_item.id.clone()],
        steer_count: 0,
    })?;
    manager.store.save_turn(&TurnRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        id: "turn_queued".to_string(),
        thread_id: thread.id.clone(),
        status: RuntimeTurnStatus::Queued,
        input_summary: "later".to_string(),
        created_at,
        started_at: None,
        ended_at: None,
        duration_ms: None,
        usage: None,
        effective_provider: None,
        effective_provider_id: None,
        effective_billing_surface: None,
        effective_model: None,
        error: None,
        item_ids: vec![queued_item.id.clone()],
        steer_count: 0,
    })?;
    drop(manager);

    let recovered = test_manager(data_dir)?;

    let recovered_thread = recovered.store.load_thread(&thread.id)?;
    assert!(recovered_thread.updated_at >= thread.updated_at);

    let recovered_in_progress_turn = recovered.store.load_turn("turn_in_progress")?;
    assert_eq!(
        recovered_in_progress_turn.status,
        RuntimeTurnStatus::Interrupted
    );
    assert_eq!(
        recovered_in_progress_turn.error.as_deref(),
        Some(RUNTIME_RESTART_REASON)
    );
    assert!(recovered_in_progress_turn.ended_at.is_some());
    assert!(
        recovered_in_progress_turn
            .duration_ms
            .is_some_and(|duration| duration >= 5_000)
    );

    let recovered_queued_turn = recovered.store.load_turn("turn_queued")?;
    assert_eq!(recovered_queued_turn.status, RuntimeTurnStatus::Interrupted);
    assert_eq!(
        recovered_queued_turn.error.as_deref(),
        Some(RUNTIME_RESTART_REASON)
    );
    assert!(recovered_queued_turn.ended_at.is_some());
    assert_eq!(recovered_queued_turn.duration_ms, None);

    assert_eq!(
        recovered.store.load_item(&completed_item.id)?.status,
        TurnItemLifecycleStatus::Completed
    );
    let recovered_in_progress_item = recovered.store.load_item(&in_progress_item.id)?;
    assert_eq!(
        recovered_in_progress_item.status,
        TurnItemLifecycleStatus::Interrupted
    );
    assert!(recovered_in_progress_item.ended_at.is_some());

    let recovered_queued_item = recovered.store.load_item(&queued_item.id)?;
    assert_eq!(
        recovered_queued_item.status,
        TurnItemLifecycleStatus::Interrupted
    );
    assert!(recovered_queued_item.ended_at.is_some());

    Ok(())
}

#[test]
fn parse_mode_defaults_to_agent() {
    assert_eq!(parse_mode("unknown"), AppMode::Agent);
    assert_eq!(parse_mode("plan"), AppMode::Plan);
}

#[test]
fn parse_mode_opt_resolves_explicit_tokens_and_aliases() {
    assert_eq!(parse_mode_opt("agent"), Some(AppMode::Agent));
    assert_eq!(parse_mode_opt("1"), Some(AppMode::Agent));
    assert_eq!(parse_mode_opt("plan"), Some(AppMode::Plan));
    assert_eq!(parse_mode_opt("2"), Some(AppMode::Plan));
    assert_eq!(parse_mode_opt("auto"), Some(AppMode::Agent));
    assert_eq!(parse_mode_opt("3"), None);
    assert_eq!(parse_mode_opt("yolo"), Some(AppMode::Yolo));
    assert_eq!(parse_mode_opt("4"), Some(AppMode::Yolo));
    assert_eq!(parse_mode_opt(" PLAN "), Some(AppMode::Plan));
}

#[test]
fn parse_mode_opt_rejects_prompt_fragments() {
    for input in [
        "plan a trip to Tokyo",
        "switch the agent on",
        "enter yolo mode",
        "agent of chaos",
        "mode",
    ] {
        assert_eq!(parse_mode_opt(input), None);
    }
}

#[test]
fn parse_mode_wrapper_defaults_and_resolves_numeric_aliases() {
    assert_eq!(parse_mode("plan a trip to Tokyo"), AppMode::Agent);
    assert_eq!(parse_mode("auto"), AppMode::Agent);
    assert_eq!(parse_mode("1"), AppMode::Agent);
    assert_eq!(parse_mode("2"), AppMode::Plan);
    assert_eq!(parse_mode("3"), AppMode::Agent);
    assert_eq!(parse_mode("4"), AppMode::Yolo);
}

fn rebind_event(event: &str, agent_id: &str, seq: u64) -> RuntimeEventRecord {
    RuntimeEventRecord {
        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
        seq,
        timestamp: Utc::now(),
        thread_id: "thr_test".to_string(),
        turn_id: Some("turn_test".to_string()),
        item_id: None,
        event: event.to_string(),
        payload: json!({ "agent_id": agent_id }),
    }
}

#[test]
fn collect_agent_rebind_hints_resumes_a_mid_fanout_session() {
    // Mirror what runtime_threads persists during a real fanout: three
    // workers spawned, two finished, one still running when the session
    // was killed. The TUI re-attach must rebuild placeholders for the
    // running worker AND the two completed workers (the fanout card
    // tracks all of them so the dot-grid stays accurate post-resume).
    let events = vec![
        rebind_event("agent.spawned", "agent_a", 1),
        rebind_event("agent.spawned", "agent_b", 2),
        rebind_event("agent.spawned", "agent_c", 3),
        rebind_event("agent.progress", "agent_a", 4),
        rebind_event("agent.completed", "agent_a", 5),
        rebind_event("agent.progress", "agent_b", 6),
        rebind_event("agent.completed", "agent_b", 7),
        rebind_event("agent.progress", "agent_c", 8),
    ];
    let hints = collect_agent_rebind_hints(&events);
    assert_eq!(hints.len(), 3, "every fanout worker must be rebound");
    let by_id: std::collections::BTreeMap<&str, AgentRebindStatus> = hints
        .iter()
        .map(|h| (h.agent_id.as_str(), h.status))
        .collect();
    assert_eq!(by_id.get("agent_a"), Some(&AgentRebindStatus::Completed));
    assert_eq!(by_id.get("agent_b"), Some(&AgentRebindStatus::Completed));
    assert_eq!(
        by_id.get("agent_c"),
        Some(&AgentRebindStatus::InProgress),
        "in-flight worker must rebind in InProgress, not downgrade"
    );
}

#[test]
fn collect_agent_rebind_hints_ignores_unrelated_events() {
    // Status / tool events should not produce phantom hints — only the
    // agent.* family carries the contract we re-bind from.
    let events = vec![
        RuntimeEventRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            seq: 1,
            timestamp: Utc::now(),
            thread_id: "thr".to_string(),
            turn_id: None,
            item_id: None,
            event: "tool.completed".to_string(),
            payload: json!({"name": "read_file"}),
        },
        rebind_event("agent.spawned", "agent_x", 2),
        RuntimeEventRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            seq: 3,
            timestamp: Utc::now(),
            thread_id: "thr".to_string(),
            turn_id: None,
            item_id: None,
            event: "compaction.completed".to_string(),
            payload: json!({"messages_after": 12}),
        },
    ];
    let hints = collect_agent_rebind_hints(&events);
    assert_eq!(hints.len(), 1);
    assert_eq!(hints[0].agent_id, "agent_x");
}

#[test]
fn collect_agent_rebind_hints_does_not_downgrade_completed_to_in_progress() {
    // Out-of-order replay: a stale `agent.progress` arriving after the
    // completed event must NOT clobber the terminal status. This matters
    // when an event log is concatenated from interrupted segments.
    let events = vec![
        rebind_event("agent.spawned", "agent_y", 1),
        rebind_event("agent.completed", "agent_y", 2),
        rebind_event("agent.progress", "agent_y", 3),
    ];
    let hints = collect_agent_rebind_hints(&events);
    assert_eq!(hints.len(), 1);
    assert_eq!(hints[0].status, AgentRebindStatus::Completed);
}

/// Helper for the `fork_at_user_message` tests: write a sequence of
/// (user, assistant) turns under the given thread id. Each turn gets
/// one UserMessage item carrying `user_text` in `detail` plus one
/// AgentMessage item. Turn `created_at` is monotonically increasing
/// so the chronological sort in `list_turns_for_thread` is stable.
fn seed_turns_with_user_messages(
    manager: &RuntimeThreadManager,
    thread_id: &str,
    user_texts: &[&str],
) -> Result<Vec<String>> {
    let mut turn_ids = Vec::new();
    let base = Utc::now();
    for (offset, text) in user_texts.iter().enumerate() {
        let created_at = base + chrono::Duration::milliseconds(offset as i64);
        let turn_id = format!("turn_test_{offset}");
        let user_item_id = format!("item_user_{offset}");
        let asst_item_id = format!("item_asst_{offset}");
        manager.store.save_item(&TurnItemRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            id: user_item_id.clone(),
            turn_id: turn_id.clone(),
            kind: TurnItemKind::UserMessage,
            status: TurnItemLifecycleStatus::Completed,
            summary: (*text).to_string(),
            detail: Some((*text).to_string()),
            metadata: None,
            artifact_refs: Vec::new(),
            started_at: Some(created_at),
            ended_at: Some(created_at),
        })?;
        manager.store.save_item(&TurnItemRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            id: asst_item_id.clone(),
            turn_id: turn_id.clone(),
            kind: TurnItemKind::AgentMessage,
            status: TurnItemLifecycleStatus::Completed,
            summary: format!("reply {offset}"),
            detail: Some(format!("reply {offset}")),
            metadata: None,
            artifact_refs: Vec::new(),
            started_at: Some(created_at),
            ended_at: Some(created_at),
        })?;
        manager.store.save_turn(&TurnRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            id: turn_id.clone(),
            thread_id: thread_id.to_string(),
            status: RuntimeTurnStatus::Completed,
            input_summary: (*text).to_string(),
            created_at,
            started_at: Some(created_at),
            ended_at: Some(created_at),
            duration_ms: Some(0),
            usage: None,
            effective_provider: None,
            effective_provider_id: None,
            effective_billing_surface: None,
            effective_model: None,
            error: None,
            item_ids: vec![user_item_id, asst_item_id],
            steer_count: 0,
        })?;
        turn_ids.push(turn_id);
    }
    Ok(turn_ids)
}

#[tokio::test]
async fn fork_at_user_message_drops_tail_and_returns_user_text() -> Result<()> {
    // Seed three completed user/assistant turns. Backtracking with
    // depth=0 should drop only the most recent turn ("third") and
    // hand back its original text so the caller can refill the
    // composer.
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;
    seed_turns_with_user_messages(&manager, &thread.id, &["first", "second", "third"])?;

    let (forked, original_text) = manager.fork_at_user_message(&thread.id, 0).await?;
    assert_eq!(original_text.as_deref(), Some("third"));
    assert_ne!(forked.id, thread.id);

    let forked_turns = manager.store.list_turns_for_thread(&forked.id)?;
    assert_eq!(
        forked_turns.len(),
        2,
        "depth=0 should drop the most recent turn"
    );
    let summaries: Vec<&str> = forked_turns
        .iter()
        .map(|t| t.input_summary.as_str())
        .collect();
    assert_eq!(summaries, vec!["first", "second"]);
    Ok(())
}

#[tokio::test]
async fn fork_at_user_message_depth_one_drops_two_turns() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;
    seed_turns_with_user_messages(&manager, &thread.id, &["a", "b", "c", "d"])?;

    let (forked, original_text) = manager.fork_at_user_message(&thread.id, 1).await?;
    assert_eq!(original_text.as_deref(), Some("c"));
    let forked_turns = manager.store.list_turns_for_thread(&forked.id)?;
    let summaries: Vec<&str> = forked_turns
        .iter()
        .map(|t| t.input_summary.as_str())
        .collect();
    assert_eq!(summaries, vec!["a", "b"]);
    Ok(())
}

#[tokio::test]
async fn fork_at_user_message_out_of_range_errors() -> Result<()> {
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;
    seed_turns_with_user_messages(&manager, &thread.id, &["only"])?;

    let err = manager.fork_at_user_message(&thread.id, 5).await.err();
    assert!(err.is_some(), "depth past the end should bail out");
    Ok(())
}

#[tokio::test]
async fn fork_at_user_message_does_not_mutate_source() -> Result<()> {
    // The source thread must be untouched: turns still present, items
    // still present, latest_turn_id still pointing at the original
    // tail. Backtrack creates a sibling, never edits in place.
    let manager = test_manager(test_runtime_dir())?;
    let thread = manager
        .create_thread(CreateThreadRequest {
            model: None,
            workspace: None,
            mode: None,
            allow_shell: None,
            trust_mode: None,
            auto_approve: None,
            archived: false,
            system_prompt: None,
            task_id: None,
            ..Default::default()
        })
        .await?;
    let turn_ids = seed_turns_with_user_messages(&manager, &thread.id, &["x", "y", "z"])?;

    let _ = manager.fork_at_user_message(&thread.id, 0).await?;

    let source_turns = manager.store.list_turns_for_thread(&thread.id)?;
    assert_eq!(
        source_turns.len(),
        3,
        "source thread must still hold every turn after fork"
    );
    for tid in &turn_ids {
        assert!(
            manager.store.load_turn(tid).is_ok(),
            "turn {tid} must remain on disk"
        );
    }
    Ok(())
}

// ── compaction summary persistence (merge_summary_into_prompt) ──

#[test]
fn summary_merge_appends_section_to_base_prompt() {
    let merged = merge_summary_into_prompt(
        Some("You are a helpful agent."),
        "## 📋 Conversation Summary (Auto-Generated)\n\nUser prefers lists.",
    );
    assert!(merged.starts_with("You are a helpful agent."));
    assert!(merged.contains(COMPACTION_SUMMARY_BEGIN));
    assert!(merged.contains("User prefers lists."));
    assert!(merged.ends_with(COMPACTION_SUMMARY_END));
    // Reload restore keys on the marker: SyncSession maps the record to
    // SystemPrompt::Text and extract_compaction_summary_prompt checks
    // `contains("Conversation Summary (Auto-Generated)")`.
    assert!(merged.contains("Conversation Summary (Auto-Generated)"));
}

#[test]
fn summary_merge_replaces_existing_section_idempotently() {
    let first = merge_summary_into_prompt(Some("Base prompt."), "summary v1");
    let second = merge_summary_into_prompt(Some(&first), "summary v2");
    assert!(second.contains("summary v2"));
    assert!(!second.contains("summary v1"));
    assert_eq!(
        second.matches(COMPACTION_SUMMARY_BEGIN).count(),
        1,
        "repeated compactions must swap the section, not stack duplicates"
    );
    assert!(second.starts_with("Base prompt."));
}

#[test]
fn summary_merge_handles_missing_base() {
    let merged = merge_summary_into_prompt(None, "only summary");
    assert!(merged.starts_with(COMPACTION_SUMMARY_BEGIN));
    assert!(merged.contains("only summary"));
    let empty_base = merge_summary_into_prompt(Some(""), "only summary");
    assert!(empty_base.starts_with(COMPACTION_SUMMARY_BEGIN));
}

#[test]
fn summary_strip_preserves_text_after_section() {
    let with_tail = format!(
        "Base.\n\n{COMPACTION_SUMMARY_BEGIN}\nold summary\n{COMPACTION_SUMMARY_END}\n\nTrailing rules."
    );
    let stripped = strip_summary_section(&with_tail);
    assert!(stripped.contains("Base."));
    assert!(stripped.contains("Trailing rules."));
    assert!(!stripped.contains("old summary"));
    // Re-merge keeps the tail intact.
    let merged = merge_summary_into_prompt(Some(&with_tail), "new summary");
    assert!(merged.contains("Trailing rules."));
    assert!(merged.contains("new summary"));
}

#[test]
fn summary_strip_handles_missing_end_sentinel() {
    let broken = format!("Base.\n\n{COMPACTION_SUMMARY_BEGIN}\ntruncated…");
    let stripped = strip_summary_section(&broken);
    assert_eq!(stripped, "Base.");
}
