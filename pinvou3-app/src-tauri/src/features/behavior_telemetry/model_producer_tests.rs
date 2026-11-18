//! Controlled model transports, real Engine terminal selection and telemetry.
//! No live provider is called. REPL uses the real local Python kernel.
use super::*;
use deepseek_tui::core::engine::{Engine, EngineConfig};
use deepseek_tui::core::events::{Event, TurnErrorSource, TurnOutcomeStatus};
use deepseek_tui::core::model_client::ModelClient;
use deepseek_tui::core::ops::{Op, UserInputProvenance};
use deepseek_tui::models::{ContentBlockStart, MessageRequest, MessageResponse, StreamEvent};
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

type ModelStream = Pin<Box<dyn futures_util::Stream<Item = anyhow::Result<StreamEvent>> + Send>>;
struct ModelFixture {
    kind: &'static str,
    calls: AtomicUsize,
    recovered: AtomicBool,
}

#[async_trait::async_trait]
impl ModelClient for ModelFixture {
    fn provider_name(&self) -> &str {
        "deepseek"
    }
    fn model(&self) -> &str {
        "deepseek-chat"
    }
    async fn create_message(&self, _: MessageRequest) -> Result<MessageResponse> {
        anyhow::bail!("stream-only fixture")
    }
    async fn create_message_stream(&self, _: MessageRequest) -> Result<ModelStream> {
        let call = self.calls.fetch_add(1, Ordering::SeqCst);
        if !self.recovered.load(Ordering::SeqCst) {
            match self.kind {
                "request" => anyhow::bail!("unexpected end of response"),
                "read" => {
                    return Ok(Box::pin(futures_util::stream::iter([Err(
                        anyhow::anyhow!("unexpected EOF while reading response"),
                    )])));
                }
                "frame" => {
                    return Ok(Box::pin(futures_util::stream::iter([Ok(
                        StreamEvent::Error {
                            error: json!({"message":"unexpected end of response"}),
                        },
                    )])));
                }
                _ => {}
            }
        }
        let text = if self.kind == "repl" && call == 0 {
            // The first actual context refresh succeeds. Rebind the kernel's
            // persistent open; the next refresh raises a real CSV parser EOF,
            // restoring open first so the following user turn can recover.
            "```repl\nimport builtins, csv\ndef fail_once(*args, **kwargs):\n    globals()['open'] = builtins.open\n    return next(csv.reader(['\"unfinished'], strict=True))\nopen = fail_once\nprint('primed')\n```"
        } else if self.kind == "repl" && call == 1 {
            "```repl\nprint('must fail before execution')\n```"
        } else {
            "recovered"
        };
        Ok(Box::pin(futures_util::stream::iter([
            Ok(StreamEvent::ContentBlockStart {
                index: 0,
                content_block: ContentBlockStart::Text { text: text.into() },
            }),
            Ok(StreamEvent::ContentBlockStop { index: 0 }),
            Ok(StreamEvent::MessageStop),
        ])))
    }
    async fn health_check(&self) -> Result<bool> {
        Ok(true)
    }
}

async fn terminal(
    handle: &deepseek_tui::core::engine::EngineHandle,
) -> (TurnOutcomeStatus, Option<String>, Option<TurnErrorSource>) {
    tokio::time::timeout(std::time::Duration::from_secs(60), async {
        let mut rx = handle.rx_event.write().await;
        while let Some(event) = rx.recv().await {
            if let Event::TurnComplete {
                status,
                error,
                error_source,
                ..
            } = event
            {
                return (status, error, error_source);
            }
        }
        panic!("engine ended without a terminal")
    })
    .await
    .expect("fixture turn timed out")
}

#[tokio::test]
async fn forkguard_model_terminal_provenance_excludes_repl_eof_and_recovers() {
    let _home = TestHome::new();
    let work = tempfile::tempdir().unwrap();
    let previous = std::env::var_os("CODEWHALE_HOME");
    struct RestoreHome(Option<OsString>);
    impl Drop for RestoreHome {
        fn drop(&mut self) {
            unsafe {
                if let Some(value) = &self.0 {
                    std::env::set_var("CODEWHALE_HOME", value);
                } else {
                    std::env::remove_var("CODEWHALE_HOME");
                }
            }
        }
    }
    let _restore = RestoreHome(previous);
    unsafe {
        std::env::set_var("CODEWHALE_HOME", work.path().join("runtime"));
    }
    for kind in ["request", "read", "frame", "repl"] {
        let folder = work.path().join(kind);
        std::fs::create_dir_all(&folder).unwrap();
        let model = Arc::new(ModelFixture {
            kind,
            calls: AtomicUsize::new(0),
            recovered: AtomicBool::new(false),
        });
        let api = deepseek_tui::config::Config::default();
        let config = EngineConfig {
            workspace: folder.clone(),
            session_id: Some(format!("eof_{kind}")),
            notes_path: folder.join("notes.md"),
            mcp_config_path: folder.join("mcp.json"),
            skills_dir: folder.join("skills"),
            explicit_skills_root_only: true,
            subagent_state_root: Some(folder.join("agents")),
            snapshots_enabled: false,
            subagents_enabled: false,
            terminal_chrome_enabled: false,
            max_steps: 8,
            ..EngineConfig::default()
        };
        let (engine, handle) = Engine::new_with_model_client(config, &api, model.clone());
        let running = tokio::spawn(engine.run());
        let send = || Op::SendMessage {
            content: "run controlled fixture".into(),
            mode: deepseek_tui::AppMode::Agent,
            route: Box::new(
                deepseek_tui::route_runtime::resolve_runtime_route(
                    &api,
                    api.api_provider(),
                    Some("deepseek-chat"),
                )
                .unwrap(),
            ),
            compaction: Box::new(deepseek_tui::compaction::CompactionConfig::default()),
            goal_objective: None,
            goal_token_budget: None,
            goal_status: deepseek_tui::tools::goal::GoalStatus::Paused,
            reasoning_effort: None,
            reasoning_effort_auto: false,
            auto_model: false,
            allow_shell: false,
            trust_mode: true,
            auto_approve: true,
            approval_mode: deepseek_tui::ApprovalMode::Auto,
            translation_enabled: false,
            allowed_tools: None,
            dynamic_tools: vec![],
            hook_executor: None,
            verbosity: None,
            provenance: UserInputProvenance::ExternalUser,
            turn_tool_security: None,
            submission_id: None,
        };
        handle.send(send()).await.unwrap();
        let first = terminal(&handle).await;
        model.recovered.store(true, Ordering::SeqCst);
        handle.send(send()).await.unwrap();
        let second = terminal(&handle).await;
        handle.send(Op::Shutdown).await.unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(10), running)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(first.0, TurnOutcomeStatus::Failed, "{kind}: {first:?}");
        let error = first.1.as_deref().unwrap();
        let source = if kind == "repl" {
            assert!(
                error.contains("REPL context refresh failed")
                    && error.contains("unexpected end of data"),
                "{error}"
            );
            assert_eq!(first.2, Some(TurnErrorSource::Runtime));
            FailureSource::AssistantRuntime
        } else {
            assert_eq!(first.2, Some(TurnErrorSource::Model));
            FailureSource::Model
        };
        let (code, _) = source.classify(error);
        assert_eq!(
            code,
            if kind == "repl" {
                "unknown"
            } else {
                "stream_unexpected_eof"
            }
        );
        let runtime = test_runtime("http://localhost:1");
        let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
        let mut event = diagnostic_failure(&format!("model_{kind}"), "failed_turn");
        event.diagnostic_hint = Some(super::super::diagnostics::DiagnosticHint {
            code: code.into(),
            http: None,
            exit: None,
            site: "assistant_error",
        });
        telemetry.track(event);
        assert_eq!(
            telemetry.outbox.lock().unwrap().records[0].event["diagnostic_code"],
            code
        );
        assert_eq!(second.0, TurnOutcomeStatus::Completed, "{kind}: {second:?}");
        assert!(
            second.1.is_none() && second.2.is_none(),
            "old source must not leak: {second:?}"
        );
    }
}
