//! Actual route installation and Engine event boundaries; no provider traffic.
use super::*;
use crate::error_taxonomy::ErrorCategory;
use std::sync::atomic::{AtomicU8, Ordering};

const PRIVATE_DETAIL: &str = "owned-host-diagnostic-canary";

#[derive(Debug)]
struct RejectingTransport(AtomicU8);

impl crate::config::ModelHttpPolicy for RejectingTransport {
    fn configure(
        &self,
        builder: reqwest::ClientBuilder,
        _: &str,
    ) -> anyhow::Result<reqwest::ClientBuilder> {
        match self.0.load(Ordering::Acquire) {
            0 => anyhow::bail!("{PRIVATE_DETAIL}"),
            1 => Ok(builder.user_agent(format!("{PRIVATE_DETAIL}\ninvalid"))),
            _ => Ok(builder.no_proxy()),
        }
    }
}

fn config_with_policy(policy: Arc<RejectingTransport>) -> Config {
    let mut config = goal_custom_route_config();
    config.model_http_policy = Some(policy);
    config
}

fn assert_safe_setup(envelope: &ErrorEnvelope) {
    assert_eq!(envelope.category, ErrorCategory::State);
    assert_eq!(envelope.code, "model_transport_setup");
    assert!(
        envelope.recoverable,
        "host can correct configuration without credential recovery"
    );
    assert!(!format!("{envelope:?}").contains(PRIVATE_DETAIL));
    assert!(!envelope.message.contains(PRIVATE_DETAIL));
}

#[test]
fn forkguard_host_http_policy_primary_diagnostics_hide_callback_and_builder_errors() {
    for mode in [0, 1] {
        let config = config_with_policy(Arc::new(RejectingTransport(AtomicU8::new(mode))));
        let error = DeepSeekClient::new(&config)
            .err()
            .expect("host setup must fail");
        assert!(!format!("{error:#?}").contains(PRIVATE_DETAIL));
        assert_safe_setup(
            error
                .downcast_ref::<ErrorEnvelope>()
                .expect("typed setup failure"),
        );
        let route = *resolved_route_for_test(&config, "local-model");
        let preflight = route
            .clone()
            .preflight()
            .expect_err("real route preflight rejects");
        assert!(!preflight.contains(PRIVATE_DETAIL));
        assert!(preflight.contains("model HTTP transport setup failed"));
        assert_safe_setup(
            &route
                .validate_with_error_envelope()
                .expect_err("typed route rejection"),
        );
    }
}

#[tokio::test]
async fn forkguard_host_http_policy_engine_rejection_preserves_configuration_category() {
    let _env = lock_test_env();
    let root = tempdir().unwrap();
    let _home = EnvVarGuard::set("CODEWHALE_HOME", root.path());
    for mode in [0, 1] {
        let policy = Arc::new(RejectingTransport(AtomicU8::new(mode)));
        let config = config_with_policy(policy.clone());
        let (mut engine, handle) = Engine::new(
            EngineConfig {
                workspace: root.path().to_path_buf(),
                snapshots_enabled: false,
                subagents_enabled: false,
                terminal_chrome_enabled: false,
                ..EngineConfig::default()
            },
            &config,
        );
        assert_safe_setup(
            engine
                .deepseek_client_error
                .as_ref()
                .expect("stored typed setup failure"),
        );
        let outcome = engine
            .handle_send_message(
                "owned request".into(),
                AppMode::Agent,
                *resolved_route_for_test(&config, "local-model"),
                CompactionConfig::default(),
                None,
                None,
                crate::tools::goal::GoalStatus::Active,
                None,
                false,
                false,
                false,
                false,
                false,
                crate::tui::approval::ApprovalMode::Suggest,
                false,
                None,
                Vec::new(),
                None,
                None,
                UserInputProvenance::ExternalUser,
                None,
            )
            .await;
        assert!(matches!(outcome, SendMessageOutcome::NotStarted { .. }));
        let mut events = handle.rx_event.write().await;
        let drained: Vec<_> = std::iter::from_fn(|| events.try_recv().ok()).collect();
        let errors: Vec<_> = drained
            .iter()
            .filter_map(|event| match event {
                Event::Error { envelope, .. } => Some(envelope),
                _ => None,
            })
            .collect();
        assert_eq!(errors.len(), 1);
        assert_safe_setup(errors[0]);
        assert!(drained.iter().any(|event| matches!(
            event,
            Event::TurnComplete {
                status: TurnOutcomeStatus::Failed,
                ..
            }
        )));
        assert!(!format!("{drained:?}").contains(PRIVATE_DETAIL));
        assert!(
            !serde_json::to_string(&errors)
                .unwrap()
                .contains(PRIVATE_DETAIL),
            "serialized public error envelopes are safe"
        );
        drop(events);
        policy.0.store(2, Ordering::Release);
        engine
            .install_resolved_runtime_route(*resolved_route_for_test(&config, "local-model"))
            .unwrap();
        assert!(engine.deepseek_client.is_some());
        assert!(
            engine.deepseek_client_error.is_none(),
            "later corrected host configuration is usable"
        );
    }
    let _key = EnvVarGuard::remove("DEEPSEEK_API_KEY");
    let no_key = Config {
        provider: Some("deepseek".into()),
        api_key: Some(String::new()),
        ..Config::default()
    };
    let (engine, _handle) = Engine::new(EngineConfig::default(), &no_key);
    assert_eq!(
        engine.deepseek_client_error.as_ref().unwrap().category,
        ErrorCategory::Authentication,
        "genuine missing credentials retain authentication recovery"
    );
}

async fn assert_rejected_control_events(handle: &EngineHandle, compaction: bool) {
    let mut events = handle.rx_event.write().await;
    let drained: Vec<_> = std::iter::from_fn(|| events.try_recv().ok()).collect();
    let errors: Vec<_> = drained
        .iter()
        .filter_map(|event| match event {
            Event::Error { envelope, .. } => Some(envelope),
            _ => None,
        })
        .collect();
    assert_eq!(errors.len(), 1);
    assert_safe_setup(errors[0]);
    assert!(drained.iter().any(|event| matches!(
        event,
        Event::TurnComplete {
            status: TurnOutcomeStatus::Failed,
            ..
        }
    )));
    if compaction {
        assert!(
            drained
                .iter()
                .any(|event| matches!(event, Event::CompactionFailed { .. }))
        );
    }
    assert!(!format!("{drained:?}").contains(PRIVATE_DETAIL));
    assert!(
        !serde_json::to_string(&errors)
            .unwrap()
            .contains(PRIVATE_DETAIL)
    );
}

#[tokio::test]
async fn forkguard_host_http_policy_control_rejections_preserve_configuration_category() {
    let _env = lock_test_env();
    let root = tempdir().unwrap();
    let _home = EnvVarGuard::set("CODEWHALE_HOME", root.path());
    for injected in [false, true] {
        for mode in [0, 1] {
            let config = config_with_policy(Arc::new(RejectingTransport(AtomicU8::new(mode))));
            let engine_config = EngineConfig {
                workspace: root.path().to_path_buf(),
                snapshots_enabled: false,
                terminal_chrome_enabled: false,
                ..EngineConfig::default()
            };
            let model = Arc::new(FailingGoalModelClient {
                calls: std::sync::atomic::AtomicUsize::new(0),
                message: "control must not call the injected model".into(),
            });
            let (mut engine, handle) = if injected {
                let client: crate::core::model_client::SharedModelClient = model.clone();
                Engine::new_with_model_client(engine_config, &config, client)
            } else {
                Engine::new(engine_config, &config)
            };
            assert_safe_setup(
                engine
                    .deepseek_client_error
                    .as_ref()
                    .expect("startup concrete setup receipt"),
            );
            // Purge before any route installation exercises the constructor
            // receipt; compaction then exercises tolerated injected preflight.
            engine.handle_purge().await;
            assert_rejected_control_events(&handle, false).await;
            engine
                .handle_manual_compaction_op(
                    "owned-policy-compaction".into(),
                    *resolved_route_for_test(&config, "local-model"),
                    CompactionConfig::default(),
                )
                .await;
            assert_rejected_control_events(&handle, true).await;
            assert_eq!(model.calls.load(Ordering::SeqCst), 0);
        }
    }
}
