//! Controlled model transports, real Engine terminal selection and telemetry.
//! No live provider is called. REPL uses the real local Python kernel.
use super::super::remote_control::{RemoteModelFailure, RemoteModelFailureEvidence};
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
                "frame" | "spoof" => {
                    let message = if self.kind == "spoof" {
                        "Network error: Connection failed: provider message"
                    } else {
                        "unexpected end of response"
                    };
                    return Ok(Box::pin(futures_util::stream::iter([Ok(
                        StreamEvent::Error {
                            error: json!({"message":message}),
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
) -> (
    TurnOutcomeStatus,
    Option<String>,
    Option<TurnErrorSource>,
    Option<RemoteModelFailure>,
) {
    tokio::time::timeout(std::time::Duration::from_secs(60), async {
        let mut rx = handle.rx_event.write().await;
        let mut evidence = RemoteModelFailureEvidence::default();
        while let Some(event) = rx.recv().await {
            match event {
                Event::TurnStarted { .. } => evidence = RemoteModelFailureEvidence::default(),
                Event::Error { envelope, .. } => {
                    evidence.observe_error(&envelope.code, &envelope.message);
                }
                Event::TurnComplete {
                    status,
                    error,
                    error_source,
                    ..
                } => {
                    let failure = if status == TurnOutcomeStatus::Failed
                        && error_source == Some(TurnErrorSource::Model)
                    {
                        evidence.selected_failure(error.as_deref())
                    } else {
                        None
                    };
                    return (status, error, error_source, failure);
                }
                _ => {}
            }
        }
        panic!("engine ended without a terminal")
    })
    .await
    .expect("fixture turn timed out")
}

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

fn set_fixture_engine_home(path: &std::path::Path) -> RestoreHome {
    let previous = std::env::var_os("CODEWHALE_HOME");
    unsafe {
        std::env::set_var("CODEWHALE_HOME", path);
    }
    RestoreHome(previous)
}

fn fixture_engine_config(folder: &std::path::Path, kind: &str) -> EngineConfig {
    EngineConfig {
        workspace: folder.to_path_buf(),
        session_id: Some(format!("diagnostic_{kind}")),
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
    }
}

fn fixture_message(api: &deepseek_tui::config::Config) -> Op {
    Op::SendMessage {
        content: "run controlled fixture".into(),
        mode: deepseek_tui::AppMode::Agent,
        route: Box::new(
            deepseek_tui::route_runtime::resolve_runtime_route(
                api,
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
    }
}

#[tokio::test]
async fn forkguard_model_terminal_provenance_excludes_repl_eof_and_recovers() {
    let _home = TestHome::new();
    let work = tempfile::tempdir().unwrap();
    let _restore = set_fixture_engine_home(&work.path().join("runtime"));
    for kind in ["request", "read", "frame", "repl", "spoof"] {
        let folder = work.path().join(kind);
        std::fs::create_dir_all(&folder).unwrap();
        let model = Arc::new(ModelFixture {
            kind,
            calls: AtomicUsize::new(0),
            recovered: AtomicBool::new(false),
        });
        let api = deepseek_tui::config::Config::default();
        let config = fixture_engine_config(&folder, kind);
        let (engine, handle) = Engine::new_with_model_client(config, &api, model.clone());
        let running = tokio::spawn(engine.run());
        let send = || fixture_message(&api);
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
        assert_eq!(first.3, None, "{kind}: untyped failures must stay unknown");
        if kind == "spoof" {
            assert_eq!(first.2, Some(TurnErrorSource::Model));
            assert_eq!(error, "Network error: Connection failed: provider message");
            assert_eq!(second.0, TurnOutcomeStatus::Completed);
            continue;
        }
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

#[tokio::test]
async fn remote_model_diagnostic_uses_typed_engine_failure_from_rejected_proxy_connect() {
    use deepseek_tui::config::{Config, ModelHttpPolicy};
    use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

    #[derive(Debug)]
    struct Proxy(reqwest::Proxy);
    impl ModelHttpPolicy for Proxy {
        fn configure(
            &self,
            builder: reqwest::ClientBuilder,
            _: &str,
        ) -> Result<reqwest::ClientBuilder> {
            Ok(builder.no_proxy().proxy(self.0.clone()))
        }
    }

    let _home = TestHome::new();
    let work = tempfile::tempdir().unwrap();
    let _restore = set_fixture_engine_home(&work.path().join("runtime"));
    let listener = tokio::net::TcpListener::bind(("127.0.0.1", 0))
        .await
        .unwrap();
    let address = listener.local_addr().unwrap();
    let requests = Arc::new(AtomicUsize::new(0));
    let observed = Arc::clone(&requests);
    let proxy = tokio::spawn(async move {
        loop {
            let (stream, _) = listener.accept().await.unwrap();
            let mut stream = BufReader::new(stream);
            let mut line = String::new();
            stream.read_line(&mut line).await.unwrap();
            assert!(line.starts_with("CONNECT diagnostic-proxy-test.invalid:443 "));
            observed.fetch_add(1, Ordering::SeqCst);
            stream
                .get_mut()
                .write_all(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
                .await
                .unwrap();
        }
    });
    let mut api = Config::default();
    api.provider = Some("deepseek".into());
    api.api_key = Some("local-fixture-key".into());
    api.base_url = Some("https://diagnostic-proxy-test.invalid/".into());
    api.model_http_policy = Some(Arc::new(Proxy(
        reqwest::Proxy::https(format!("http://{address}")).unwrap(),
    )));
    let (engine, handle) = Engine::new(fixture_engine_config(work.path(), "proxy"), &api);
    let running = tokio::spawn(engine.run());
    handle.send(fixture_message(&api)).await.unwrap();
    let result = terminal(&handle).await;
    handle.send(Op::Shutdown).await.unwrap();
    tokio::time::timeout(Duration::from_secs(10), running)
        .await
        .unwrap()
        .unwrap();
    proxy.abort();

    assert!(requests.load(Ordering::SeqCst) > 0);
    assert_eq!(result.0, TurnOutcomeStatus::Failed);
    assert_eq!(result.2, Some(TurnErrorSource::Model));
    assert_eq!(result.3, Some(RemoteModelFailure::Network), "{result:?}");
}
