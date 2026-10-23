// Engine producer fixture. The integration harness supplies the host observer,
// keeping telemetry independent of the assistant feature.
use super::*;
use deepseek_tui::core::engine::{Engine, EngineConfig};
use deepseek_tui::core::events::Event;
use deepseek_tui::core::model_client::ModelClient;
use deepseek_tui::core::ops::{Op, UserInputProvenance};
use deepseek_tui::models::{
    ContentBlockStart, Delta, MessageDelta, MessageRequest, MessageResponse, StreamEvent, Usage,
};
use std::collections::VecDeque;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::time::Duration;
type FixtureStream =
    Pin<Box<dyn futures_util::Stream<Item = anyhow::Result<StreamEvent>> + Send + 'static>>;

struct ScriptedModel(Mutex<VecDeque<Vec<StreamEvent>>>);
#[async_trait::async_trait]
impl ModelClient for ScriptedModel {
    fn provider_name(&self) -> &str {
        "deepseek"
    }
    fn model(&self) -> &str {
        "deepseek-chat"
    }
    async fn create_message(&self, _: MessageRequest) -> Result<MessageResponse> {
        anyhow::bail!("stream-only fixture")
    }
    async fn create_message_stream(&self, _: MessageRequest) -> Result<FixtureStream> {
        let events = self
            .0
            .lock()
            .unwrap()
            .pop_front()
            .ok_or_else(|| anyhow::anyhow!("script exhausted"))?;
        Ok(Box::pin(futures_util::stream::iter(
            events.into_iter().map(Ok),
        )))
    }
    async fn health_check(&self) -> Result<bool> {
        Ok(true)
    }
}
fn start() -> StreamEvent {
    StreamEvent::MessageStart {
        message: MessageResponse {
            id: "diagnostic_fixture".into(),
            r#type: "message".into(),
            role: "assistant".into(),
            content: vec![],
            model: "deepseek-chat".into(),
            stop_reason: None,
            stop_sequence: None,
            container: None,
            usage: Usage::default(),
        },
    }
}
fn stop(reason: &str) -> StreamEvent {
    StreamEvent::MessageDelta {
        delta: MessageDelta {
            stop_reason: Some(reason.into()),
            stop_sequence: None,
        },
        usage: None,
    }
}
fn script(command: &str, timeout: Option<u64>) -> Arc<ScriptedModel> {
    let mut input = json!({"command":command});
    if let Some(timeout) = timeout {
        input["timeout"] = json!(timeout);
    }
    Arc::new(ScriptedModel(Mutex::new(VecDeque::from([
        vec![
            start(),
            StreamEvent::ContentBlockStart {
                index: 0,
                content_block: ContentBlockStart::ToolUse {
                    id: "shell_call_fixture".into(),
                    name: "bash".into(),
                    input: Value::Null,
                    caller: None,
                    thought_signature: None,
                },
            },
            StreamEvent::ContentBlockDelta {
                index: 0,
                delta: Delta::InputJsonDelta {
                    partial_json: input.to_string(),
                },
            },
            StreamEvent::ContentBlockStop { index: 0 },
            stop("tool_use"),
            StreamEvent::MessageStop,
        ],
        vec![
            start(),
            StreamEvent::ContentBlockStart {
                index: 0,
                content_block: ContentBlockStart::Text {
                    text: "done".into(),
                },
            },
            StreamEvent::ContentBlockStop { index: 0 },
            stop("end_turn"),
            StreamEvent::MessageStop,
        ],
    ]))))
}
struct RuntimeHome(Option<OsString>);
impl Drop for RuntimeHome {
    fn drop(&mut self) {
        unsafe {
            match &self.0 {
                Some(value) => std::env::set_var("CODEWHALE_HOME", value),
                None => std::env::remove_var("CODEWHALE_HOME"),
            }
        }
    }
}

pub(crate) async fn run_shell_producer_fixture(
    observe: fn(&deepseek_tui::tools::shell::SharedShellManager, &str, &str, &str) -> Option<Value>,
) {
    // TestHome holds the harness-wide environment lock for the entire Engine
    // lifetime. All runtime state belongs to this fixture, never the user home.
    let _home = TestHome::new();
    let work = tempfile::tempdir().unwrap();
    let _restore = RuntimeHome(std::env::var_os("CODEWHALE_HOME"));
    unsafe {
        std::env::set_var("CODEWHALE_HOME", work.path().join("runtime"));
    }
    for (index, command, timeout, expected_exit) in [
        (0, "exit 7", None, Some(7_i64)),
        (1, "echo 'HTTP 429'; exit 7", None, Some(7)),
        (2, "exit 0", None, None),
        (3, "sleep 4", Some(1), None),
    ] {
        let folder = work.path().join(format!("case_{index}"));
        std::fs::create_dir_all(&folder).unwrap();
        let session = format!("shell_diagnostic_session_{index}");
        let manager = deepseek_tui::tools::shell::new_shared_shell_manager(folder.join("jobs"));
        let api = deepseek_tui::config::Config::default();
        let config = EngineConfig {
            workspace: folder.clone(),
            session_id: Some(session.clone()),
            notes_path: folder.join("notes.md"),
            mcp_config_path: folder.join("mcp.json"),
            skills_dir: folder.join("skills"),
            explicit_skills_root_only: true,
            subagent_state_root: Some(folder.join("agents")),
            snapshots_enabled: false,
            subagents_enabled: false,
            terminal_chrome_enabled: false,
            trust_mode: true,
            runtime_services: deepseek_tui::tools::spec::RuntimeToolServices {
                shell_manager: Some(manager.clone()),
                ..Default::default()
            },
            ..EngineConfig::default()
        };
        let (engine, handle) =
            Engine::new_with_model_client(config, &api, script(command, timeout));
        let running = tokio::spawn(engine.run());
        let route = deepseek_tui::route_runtime::resolve_runtime_route(
            &api,
            api.api_provider(),
            Some("deepseek-chat"),
        )
        .unwrap();
        handle
            .send(Op::SendMessage {
                content: "run fixture".into(),
                mode: deepseek_tui::AppMode::Agent,
                route: Box::new(route),
                compaction: Box::new(deepseek_tui::compaction::CompactionConfig::default()),
                goal_objective: None,
                goal_token_budget: None,
                goal_status: deepseek_tui::tools::goal::GoalStatus::Paused,
                reasoning_effort: None,
                reasoning_effort_auto: false,
                auto_model: false,
                allow_shell: true,
                trust_mode: true,
                auto_approve: true,
                approval_mode: deepseek_tui::ApprovalMode::Auto,
                translation_enabled: false,
                allowed_tools: Some(vec!["bash".into()]),
                dynamic_tools: vec![],
                hook_executor: None,
                verbosity: None,
                provenance: UserInputProvenance::ExternalUser,
                turn_tool_security: None,
                submission_id: None,
            })
            .await
            .unwrap();
        let observed = tokio::time::timeout(Duration::from_secs(30), async {
            let mut rx = handle.rx_event.write().await;
            let mut turn = None;
            let mut observation = None;
            while let Some(event) = rx.recv().await {
                match event {
                    Event::TurnStarted { turn_id, .. } => turn = Some(turn_id),
                    Event::ToolCallComplete { id, name, result } => {
                        assert_eq!(name, "bash");
                        let success = result.as_ref().is_ok_and(|result| result.success);
                        let original_metadata = result
                            .as_ref()
                            .ok()
                            .and_then(|result| result.metadata.as_ref());
                        let fallback = (!success && original_metadata.is_none())
                            .then(|| observe(&manager, &session, turn.as_deref().unwrap(), &id))
                            .flatten();
                        let metadata = original_metadata.or(fallback.as_ref()).cloned();
                        let statuses = manager
                            .lock()
                            .unwrap()
                            .list_jobs_for_session(&session)
                            .into_iter()
                            .filter(|job| {
                                job.origin_turn_id.as_deref() == turn.as_deref()
                                    && job.origin_tool_call_id.as_deref() == Some(id.as_str())
                                    && job.owner_agent_id.is_none()
                            })
                            .map(|job| job.status)
                            .collect::<Vec<_>>();
                        observation = Some((
                            success,
                            result.err().map(|e| e.to_string()).unwrap_or_default(),
                            metadata,
                            statuses,
                        ));
                    }
                    Event::TurnComplete { .. } => break,
                    _ => {}
                }
            }
            observation
        })
        .await;
        handle.send(Op::Shutdown).await.unwrap();
        let mut running = running;
        if tokio::time::timeout(Duration::from_secs(10), &mut running)
            .await
            .is_err()
        {
            running.abort();
            panic!("fixture engine shutdown timed out");
        }
        let (success, error, metadata, statuses) = observed
            .expect("fixture turn timed out")
            .expect("real default bash did not execute");
        assert_eq!(success, index == 2);
        assert_eq!(
            statuses.len(),
            1,
            "Engine must produce one job with its real origin tuple"
        );
        let runtime = test_runtime("http://localhost:1");
        let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
        let mut event = BehaviorEvent::new("tool_call_completed")
            .session(&session)
            .turn("witness_turn_fixture")
            .string("operation_id", "shell_operation_fixture")
            .integer("attempt_index", 1)
            .tool("shell", "bash", "cli")
            .success(success)
            .integer("duration_ms", 1);
        if !success {
            event = event
                .string("error_category", "tool_error")
                .diagnostic_error(&error, "tool_result")
                .diagnostic_tool_metadata(metadata.as_ref());
        }
        telemetry.track(event);
        let outbox = telemetry.outbox.lock().unwrap();
        let event = &outbox.records[0].event;
        if let Some(exit) = expected_exit {
            assert_eq!(statuses[0], deepseek_tui::tools::shell::ShellStatus::Failed);
            assert_eq!(metadata.as_ref().unwrap()["exit_code"], exit);
            assert_eq!(event["diagnostic_code"], "process_failed");
            assert_eq!(event["diagnostic_exit_code"], exit);
            assert!(event.get("diagnostic_http_status").is_none());
            assert!(!event.to_string().contains("HTTP 429"));
        } else if index == 3 {
            assert_eq!(statuses[0], deepseek_tui::tools::shell::ShellStatus::Killed);
            assert!(
                error.contains("Command timed out after"),
                "fixture must actually reach the timeout producer"
            );
            assert!(event.get("diagnostic_exit_code").is_none());
            assert!(
                metadata.is_none(),
                "Killed is not proof of a natural failure or typed timeout"
            );
        } else {
            assert_eq!(
                statuses[0],
                deepseek_tui::tools::shell::ShellStatus::Completed
            );
            assert!(
                !event
                    .as_object()
                    .unwrap()
                    .keys()
                    .any(|key| key.starts_with("diagnostic_"))
            );
        }
    }
}
