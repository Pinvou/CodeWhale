//! Public-boundary integration evidence: real Engine, synthetic local tools only.
use codewhale_tui::{
    AppMode, ApprovalMode,
    config::Config,
    core::{
        engine::{Engine, EngineConfig, EngineHandle},
        events::{Event, TurnOutcomeStatus},
        model_client::{ModelClient, SharedModelClient},
        ops::{Op, UserInputProvenance},
    },
    models::{ContentBlock, MessageRequest, MessageResponse, StreamEvent},
};
use serde_json::json;
use std::{
    collections::VecDeque,
    fs,
    path::Path,
    sync::{Arc, Mutex},
    time::Duration,
};
use tempfile::tempdir;

type EventStream =
    std::pin::Pin<Box<dyn futures_util::Stream<Item = anyhow::Result<StreamEvent>> + Send>>;

fn model_turn_event_timeout() -> Duration {
    Duration::from_secs(30)
}
fn deterministic_engine_config(workspace: &Path) -> EngineConfig {
    EngineConfig {
        workspace: workspace.to_path_buf(),
        snapshots_enabled: false,
        subagents_enabled: false,
        skills_dir: workspace.join("skills"),
        explicit_skills_root_only: true,
        mcp_config_path: workspace.join("no-mcp.json"),
        notes_path: workspace.join("notes.md"),
        memory_path: workspace.join("memory.md"),
        ..EngineConfig::default()
    }
}
fn external_user_message_op(content: &str, mode: AppMode, config: &Config) -> Op {
    Op::SendMessage {
        content: content.into(),
        mode,
        route: Box::new(
            codewhale_tui::route_runtime::resolve_runtime_route(
                config,
                config.api_provider(),
                Some("local-model"),
            )
            .expect("test route"),
        ),
        compaction: Default::default(),
        goal_objective: None,
        goal_token_budget: None,
        goal_status: Default::default(),
        reasoning_effort: None,
        reasoning_effort_auto: false,
        auto_model: false,
        allow_shell: false,
        trust_mode: false,
        auto_approve: false,
        approval_mode: ApprovalMode::Suggest,
        translation_enabled: false,
        allowed_tools: Some(vec!["File".into()]),
        dynamic_tools: Vec::new(),
        hook_executor: None,
        verbosity: None,
        provenance: UserInputProvenance::ExternalUser,
        turn_tool_security: None,
        submission_id: None,
    }
}

struct MockLlmClient {
    turns: Mutex<VecDeque<anyhow::Result<Vec<StreamEvent>>>>,
    requests: Mutex<Vec<MessageRequest>>,
    initial_network_error: AtomicBool,
}
impl MockLlmClient {
    fn new(turns: Vec<Vec<StreamEvent>>) -> Self {
        Self {
            turns: Mutex::new(turns.into_iter().map(Ok).collect()),
            requests: Mutex::new(Vec::new()),
            initial_network_error: AtomicBool::new(false),
        }
    }
    fn call_count(&self) -> usize {
        self.requests.lock().unwrap().len()
    }
    fn captured_requests(&self) -> Vec<MessageRequest> {
        self.requests.lock().unwrap().clone()
    }
}
#[async_trait::async_trait]
impl ModelClient for MockLlmClient {
    fn provider_name(&self) -> &str {
        "partial-tool-test"
    }
    fn model(&self) -> &str {
        "local-model"
    }
    async fn create_message(&self, _: MessageRequest) -> anyhow::Result<MessageResponse> {
        anyhow::bail!("streaming required")
    }
    async fn create_message_stream(&self, request: MessageRequest) -> anyhow::Result<EventStream> {
        self.requests.lock().unwrap().push(request);
        if self.initial_network_error.swap(false, Ordering::SeqCst) {
            return Ok(Box::pin(futures_util::stream::iter([Err(
                anyhow::anyhow!("Stream read error: error decoding response body"),
            )])));
        }
        let events = self
            .turns
            .lock()
            .unwrap()
            .pop_front()
            .expect("unexpected extra model request")?;
        Ok(Box::pin(futures_util::stream::iter(
            events.into_iter().map(Ok),
        )))
    }
    async fn health_check(&self) -> anyhow::Result<bool> {
        Ok(true)
    }
}

use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

fn argument_chunks(text: &str) -> Vec<&str> {
    let mut parts = Vec::new();
    let mut start = 0;
    while start < text.len() {
        let mut end = (start + 64).min(text.len());
        while !text.is_char_boundary(end) {
            end += 1;
        }
        parts.push(&text[start..end]);
        start = end;
    }
    parts
}

async fn terminal_events(handle: &EngineHandle) -> Vec<Event> {
    let mut events = Vec::new();
    loop {
        let event = tokio::time::timeout(model_turn_event_timeout(), async {
            handle.rx_event.write().await.recv().await
        })
        .await
        .expect("partial-input Engine timeout")
        .expect("Engine event stream ended before terminal event");
        let done = matches!(event, Event::TurnComplete { .. });
        events.push(event);
        if done {
            return events;
        }
    }
}

fn auto_write_op(config: &Config) -> Op {
    let mut op = external_user_message_op(
        "Exercise synthetic workspace tools.",
        AppMode::Agent,
        config,
    );
    if let Op::SendMessage { auto_approve, .. } = &mut op {
        *auto_approve = true;
    }
    op
}

#[tokio::test]
async fn partial_input_engine_interleaved_tools_keep_complete_arguments_and_history() {
    let workspace = tempdir().expect("workspace");
    fs::write(workspace.path().join("fixture.txt"), "read fixture").expect("fixture");
    let content = "fn sample() { println!(\"中文🙂\"); }\n".repeat(384);
    let write = json!({"action":"write","path":"written.txt","content":content});
    let read = json!({"action":"read","path":"fixture.txt"});
    let write_json = serde_json::to_string(&write).expect("serialize write");
    let read_json = serde_json::to_string(&read).expect("serialize read");
    let mut turn = vec![
        canned::message_start("partial-input-interleaved"),
        canned::tool_use_block_start(0, "write-call", "File"),
        canned::tool_use_block_start(1, "read-call", "File"),
    ];
    let writes = argument_chunks(&write_json);
    let reads = argument_chunks(&read_json);
    for index in 0..writes.len().max(reads.len()) {
        if let Some(part) = writes.get(index) {
            turn.push(canned::tool_input_delta(0, part));
        }
        if let Some(part) = reads.get(index) {
            turn.push(canned::tool_input_delta(1, part));
        }
    }
    turn.extend([
        canned::block_stop(1),
        canned::block_stop(0),
        canned::message_delta("tool_use", None),
        canned::message_stop(),
    ]);
    let mock = Arc::new(MockLlmClient::new(vec![
        turn,
        canned::simple_text_turn("done"),
    ]));
    let client: SharedModelClient = mock.clone();
    let config = Config::default();
    let (engine, handle) = Engine::new_with_model_client(
        deterministic_engine_config(workspace.path()),
        &config,
        client,
    );
    let task = tokio::spawn(engine.run());
    handle.send(auto_write_op(&config)).await.expect("send");
    let events = terminal_events(&handle).await;
    let mut starts = std::collections::HashMap::new();
    let mut successes = 0;
    for event in &events {
        match event {
            Event::ToolCallStarted { id, input, .. } => {
                assert!(
                    starts.insert(id.as_str(), input).is_none(),
                    "duplicate tool start"
                );
            }
            Event::ToolCallComplete { result, .. } => {
                assert!(result.as_ref().expect("tool result").success);
                successes += 1;
            }
            Event::TurnComplete { status, error, .. } => {
                assert_eq!(*status, TurnOutcomeStatus::Completed, "{error:?}")
            }
            _ => {}
        }
    }
    assert_eq!(starts.len(), 2);
    assert_eq!(starts["write-call"], &write);
    assert_eq!(starts["read-call"], &read);
    assert_eq!(successes, 2);
    assert_eq!(
        fs::read_to_string(workspace.path().join("written.txt")).expect("written file"),
        content
    );
    let requests = mock.captured_requests();
    assert_eq!(requests.len(), 2);
    let history_input = requests[1]
        .messages
        .iter()
        .flat_map(|message| &message.content)
        .find_map(|block| {
            if let ContentBlock::ToolUse { id, input, .. } = block {
                (id == "write-call").then_some(input)
            } else {
                None
            }
        })
        .expect("tool arguments in follow-up history");
    assert_eq!(history_input["path"], write["path"]);
    assert_eq!(history_input["content"], write["content"]);
    handle.send(Op::Shutdown).await.expect("shutdown");
    task.await.expect("Engine task");
}

#[tokio::test]
async fn partial_input_engine_malformed_final_preserves_raw_arguments_and_does_not_write() {
    let workspace = tempdir().expect("workspace");
    let raw = format!(
        "{{\"action\":\"write\",\"path\":\"never.txt\",\"content\":\"{}",
        "unfinished ".repeat(256)
    );
    let mut turn = vec![
        canned::message_start("malformed-partial"),
        canned::tool_use_block_start(0, "bad-call", "File"),
    ];
    turn.extend(
        argument_chunks(&raw)
            .into_iter()
            .map(|part| canned::tool_input_delta(0, part)),
    );
    turn.extend([
        canned::block_stop(0),
        canned::message_delta("tool_use", None),
        canned::message_stop(),
    ]);
    let mock = Arc::new(MockLlmClient::new(vec![
        turn,
        canned::simple_text_turn("recovered"),
    ]));
    let client: SharedModelClient = mock.clone();
    let config = Config::default();
    let (engine, handle) = Engine::new_with_model_client(
        deterministic_engine_config(workspace.path()),
        &config,
        client,
    );
    let task = tokio::spawn(engine.run());
    handle.send(auto_write_op(&config)).await.expect("send");
    let events = terminal_events(&handle).await;
    let input = events
        .iter()
        .find_map(|event| match event {
            Event::ToolCallStarted { id, input, .. } if id == "bad-call" => Some(input),
            _ => None,
        })
        .expect("malformed tool lifecycle");
    assert_eq!(input, &json!({"raw_arguments":raw}));
    assert!(!workspace.path().join("never.txt").exists());
    assert!(events.iter().any(|event| matches!(event, Event::ToolCallComplete { result, .. } if result.as_ref().map_or(true, |value| !value.success))));
    assert_eq!(
        mock.call_count(),
        2,
        "validation feedback must reach the next model request"
    );
    handle.send(Op::Shutdown).await.expect("shutdown");
    task.await.expect("Engine task");
}

struct PartialToolModel {
    requests: Mutex<Vec<MessageRequest>>,
    calls: AtomicUsize,
    entered: Arc<tokio::sync::Notify>,
    dropped: Arc<AtomicBool>,
    network_error: bool,
}

struct StreamDropFlag(Arc<AtomicBool>);
impl Drop for StreamDropFlag {
    fn drop(&mut self) {
        self.0.store(true, Ordering::SeqCst);
    }
}

#[async_trait::async_trait]
impl ModelClient for PartialToolModel {
    fn provider_name(&self) -> &str {
        "partial-tool-test"
    }
    fn model(&self) -> &str {
        "local-model"
    }
    async fn create_message(
        &self,
        _: codewhale_tui::models::MessageRequest,
    ) -> anyhow::Result<codewhale_tui::models::MessageResponse> {
        anyhow::bail!("partial tool test expects streaming")
    }
    async fn create_message_stream(
        &self,
        request: codewhale_tui::models::MessageRequest,
    ) -> anyhow::Result<EventStream> {
        self.requests.lock().unwrap().push(request);
        let call = self.calls.fetch_add(1, Ordering::SeqCst);
        if call > 0 {
            let events = if call == 1 {
                canned::tool_call_turn(
                    "reused-call",
                    "File",
                    r#"{"action":"read","path":"fixture.txt"}"#,
                )
            } else {
                canned::simple_text_turn("retry completed")
            };
            return Ok(Box::pin(futures_util::stream::iter(
                events.into_iter().map(Ok),
            )));
        }
        let entered = self.entered.clone();
        let dropped = self.dropped.clone();
        let network_error = self.network_error;
        Ok(Box::pin(async_stream::stream! {
            let _drop_flag = StreamDropFlag(dropped);
            yield Ok(canned::message_start("partial-tool-start"));
            yield Ok(canned::tool_use_block_start(0, "reused-call", "File"));
            yield Ok(canned::tool_input_delta(0, r#"{"action":"write","path":"never.txt","#));
            yield Ok(canned::tool_input_delta(0, r#""content":"unfinished"#));
            for _ in 0..16 { yield Ok(canned::tool_input_delta(0, " longer unfinished text")); }
            entered.notify_one();
            if network_error {
                yield Err(anyhow::anyhow!("Stream read error: error decoding response body"));
            } else {
                std::future::pending::<()>().await;
            }
        }))
    }
    async fn health_check(&self) -> anyhow::Result<bool> {
        Ok(true)
    }
}

#[tokio::test]
async fn partial_input_engine_cancel_then_new_attempt_resets_reused_tool_id() {
    let workspace = tempdir().expect("workspace");
    fs::write(workspace.path().join("fixture.txt"), "fixture").expect("fixture");
    let model = Arc::new(PartialToolModel {
        requests: Mutex::new(Vec::new()),
        calls: AtomicUsize::new(0),
        entered: Arc::new(tokio::sync::Notify::new()),
        dropped: Arc::new(AtomicBool::new(false)),
        network_error: false,
    });
    let client: SharedModelClient = model.clone();
    let config = Config::default();
    let (engine, handle) = Engine::new_with_model_client(
        deterministic_engine_config(workspace.path()),
        &config,
        client,
    );
    let task = tokio::spawn(engine.run());
    handle.send(auto_write_op(&config)).await.expect("send");
    tokio::time::timeout(model_turn_event_timeout(), model.entered.notified())
        .await
        .expect("partial deltas not consumed");
    handle.cancel();
    let cancelled = terminal_events(&handle).await;
    assert!(cancelled.iter().any(|event| matches!(
        event,
        Event::TurnComplete {
            status: TurnOutcomeStatus::Interrupted,
            ..
        }
    )));
    assert!(
        !cancelled
            .iter()
            .any(|event| matches!(event, Event::ToolCallStarted { .. }))
    );
    assert!(model.dropped.load(Ordering::SeqCst));
    assert_eq!(
        model.calls.load(Ordering::SeqCst),
        1,
        "cancel must not retry"
    );
    assert!(!workspace.path().join("never.txt").exists());
    handle
        .send(external_user_message_op(
            "Read fixture after cancellation.",
            AppMode::Agent,
            &config,
        ))
        .await
        .expect("retry send");
    let retried = terminal_events(&handle).await;
    let input = retried
        .iter()
        .find_map(|event| match event {
            Event::ToolCallStarted { id, input, .. } if id == "reused-call" => Some(input),
            _ => None,
        })
        .expect("new tool start");
    assert_eq!(input, &json!({"action":"read","path":"fixture.txt"}));
    assert!(retried.iter().any(|event| matches!(
        event,
        Event::ToolCallComplete { id, result: Ok(result), .. }
            if id == "reused-call" && result.success
    )));
    assert!(retried.iter().any(|event| matches!(
        event,
        Event::TurnComplete {
            status: TurnOutcomeStatus::Completed,
            ..
        }
    )));
    assert_eq!(model.calls.load(Ordering::SeqCst), 3);
    assert!(!workspace.path().join("never.txt").exists());
    handle.send(Op::Shutdown).await.expect("shutdown");
    task.await.expect("Engine task");
}

#[tokio::test]
async fn partial_input_engine_network_error_preserves_partial_tool_feedback() {
    let workspace = tempdir().expect("workspace");
    fs::write(workspace.path().join("fixture.txt"), "fixture").expect("fixture");
    let model = Arc::new(PartialToolModel {
        requests: Mutex::new(Vec::new()),
        calls: AtomicUsize::new(0),
        entered: Arc::new(tokio::sync::Notify::new()),
        dropped: Arc::new(AtomicBool::new(false)),
        network_error: true,
    });
    let config = Config::default();
    let (engine, handle) = Engine::new_with_model_client(
        deterministic_engine_config(workspace.path()),
        &config,
        model.clone(),
    );
    let task = tokio::spawn(engine.run());
    handle.send(auto_write_op(&config)).await.expect("send");
    let events = terminal_events(&handle).await;
    assert!(events.iter().any(|event| matches!(event,
        Event::TurnComplete {status: TurnOutcomeStatus::Failed,error:Some(error),..}
            if error.contains("response body"))));
    // The current interactive path retains the partial mirror and reports its
    // validation failure through the ordinary tool-feedback loop. It then lets
    // the model finish that loop before surfacing the original stream error.
    // This pins compatibility, not a general guarantee against partial dispatch.
    let completions: Vec<_> = events
        .iter()
        .filter_map(|event| match event {
            Event::ToolCallComplete { id, result, .. } if id == "reused-call" => Some(result),
            _ => None,
        })
        .collect();
    assert_eq!(completions.len(), 2);
    assert!(
        completions[0]
            .as_ref()
            .expect_err("partial writer validation failure")
            .to_string()
            .contains("missing required field 'content'")
    );
    assert!(
        completions[1]
            .as_ref()
            .expect("complete reader result")
            .success
    );
    let starts: Vec<_> = events
        .iter()
        .filter_map(|event| match event {
            Event::ToolCallStarted { id, input, .. } => Some((id, input)),
            _ => None,
        })
        .collect();
    assert_eq!(
        starts.len(),
        1,
        "only the complete follow-up reader publishes a start"
    );
    assert_eq!(starts[0].0, "reused-call");
    assert_eq!(starts[0].1, &json!({"action":"read","path":"fixture.txt"}));
    assert_eq!(
        model.calls.load(Ordering::SeqCst),
        3,
        "tool-error feedback and reader result feedback"
    );
    {
        let requests = model.requests.lock().unwrap();
        let partial = requests[1]
            .messages
            .iter()
            .flat_map(|message| &message.content)
            .find_map(|block| match block {
                ContentBlock::ToolUse { id, input, .. } if id == "reused-call" => Some(input),
                _ => None,
            })
            .expect("partial mirror in tool-error feedback history");
        assert_eq!(partial["path"], "never.txt");
        assert!(partial.get("content").is_none());
    }
    assert!(model.dropped.load(Ordering::SeqCst));
    assert!(!workspace.path().join("never.txt").exists());
    handle.send(Op::Shutdown).await.expect("shutdown");
    task.await.expect("Engine task");
}

mod canned {
    use codewhale_tui::models::{
        ContentBlockStart, Delta, MessageDelta, MessageResponse, StreamEvent, Usage,
    };
    use serde_json::Value;

    /// `MessageStart` event with a synthetic message envelope.
    #[must_use]
    pub fn message_start(id: &str) -> StreamEvent {
        StreamEvent::MessageStart {
            message: MessageResponse {
                id: id.to_string(),
                r#type: "message".to_string(),
                role: "assistant".to_string(),
                content: vec![],
                model: "mock-model".to_string(),
                stop_reason: None,
                stop_sequence: None,
                container: None,
                usage: Usage::default(),
            },
        }
    }

    /// Open a text content block at `index`.
    #[must_use]
    pub fn text_block_start(index: u32) -> StreamEvent {
        StreamEvent::ContentBlockStart {
            index,
            content_block: ContentBlockStart::Text {
                text: String::new(),
            },
        }
    }

    /// Append `text` to the content block at `index`.
    #[must_use]
    pub fn text_delta(index: u32, text: &str) -> StreamEvent {
        StreamEvent::ContentBlockDelta {
            index,
            delta: Delta::TextDelta {
                text: text.to_string(),
            },
        }
    }

    /// Open a tool_use content block at `index`.
    #[must_use]
    pub fn tool_use_block_start(index: u32, id: &str, name: &str) -> StreamEvent {
        StreamEvent::ContentBlockStart {
            index,
            content_block: ContentBlockStart::ToolUse {
                id: id.to_string(),
                name: name.to_string(),
                input: Value::Null,
                caller: None,
                thought_signature: None,
            },
        }
    }

    /// Stream partial JSON for a tool's input arguments.
    #[must_use]
    pub fn tool_input_delta(index: u32, partial_json: &str) -> StreamEvent {
        StreamEvent::ContentBlockDelta {
            index,
            delta: Delta::InputJsonDelta {
                partial_json: partial_json.to_string(),
            },
        }
    }

    /// Close the content block at `index`.
    #[must_use]
    pub fn block_stop(index: u32) -> StreamEvent {
        StreamEvent::ContentBlockStop { index }
    }

    /// Emit a `message_delta` carrying `stop_reason` and optional `usage`.
    #[must_use]
    pub fn message_delta(stop_reason: &str, usage: Option<Usage>) -> StreamEvent {
        StreamEvent::MessageDelta {
            delta: MessageDelta {
                stop_reason: Some(stop_reason.to_string()),
                stop_sequence: None,
            },
            usage,
        }
    }

    /// Final `message_stop` sentinel.
    #[must_use]
    pub fn message_stop() -> StreamEvent {
        StreamEvent::MessageStop
    }

    /// Convenience: a complete "assistant emits this text" turn ending with
    /// `stop_reason = "end_turn"`.
    #[must_use]
    pub fn simple_text_turn(text: &str) -> Vec<StreamEvent> {
        vec![
            message_start("mock_msg_1"),
            text_block_start(0),
            text_delta(0, text),
            block_stop(0),
            message_delta("end_turn", None),
            message_stop(),
        ]
    }

    /// Convenience: a turn that emits one assistant tool_call and stops.
    #[must_use]
    pub fn tool_call_turn(call_id: &str, tool_name: &str, args_json: &str) -> Vec<StreamEvent> {
        vec![
            message_start("mock_msg_tool"),
            tool_use_block_start(0, call_id, tool_name),
            tool_input_delta(0, args_json),
            block_stop(0),
            message_delta("tool_use", None),
            message_stop(),
        ]
    }
}

// === Tests ===

#[tokio::test]
async fn partial_input_engine_network_error_before_any_content_can_retry() {
    let workspace = tempdir().expect("workspace");
    let model = Arc::new(MockLlmClient::new(vec![canned::simple_text_turn(
        "recovered",
    )]));
    model.initial_network_error.store(true, Ordering::SeqCst);
    let config = Config::default();
    let (engine, handle) = Engine::new_with_model_client(
        deterministic_engine_config(workspace.path()),
        &config,
        model.clone(),
    );
    let task = tokio::spawn(engine.run());
    handle.send(auto_write_op(&config)).await.expect("send");
    let events = terminal_events(&handle).await;
    assert!(events.iter().any(|event| matches!(
        event,
        Event::TurnComplete {
            status: TurnOutcomeStatus::Completed,
            ..
        }
    )));
    assert_eq!(
        model.call_count(),
        2,
        "a transient stream failure before content may retry"
    );
    assert!(
        !events
            .iter()
            .any(|event| matches!(event, Event::ToolCallStarted { .. }))
    );
    handle.send(Op::Shutdown).await.expect("shutdown");
    task.await.expect("Engine task");
}

/// Headless recovery discards the uncommitted tool fragment. The partial-input
/// history clone in InteractiveNetworkDrop is unreachable while a tool is open:
/// that resume policy requires tool_uses_empty. Pin the actual public contract.
#[tokio::test]
async fn partial_input_engine_headless_resume_discards_partial_history_and_resets_tool_id() {
    let workspace = tempdir().expect("workspace");
    fs::write(workspace.path().join("fixture.txt"), "fixture").expect("fixture");
    let model = Arc::new(PartialToolModel {
        requests: Mutex::new(Vec::new()),
        calls: AtomicUsize::new(0),
        entered: Arc::new(tokio::sync::Notify::new()),
        dropped: Arc::new(AtomicBool::new(false)),
        network_error: true,
    });
    let config = Config::default();
    let mut engine_config = deterministic_engine_config(workspace.path());
    engine_config.terminal_chrome_enabled = false;
    let (engine, handle) = Engine::new_with_model_client(engine_config, &config, model.clone());
    let task = tokio::spawn(engine.run());
    handle.send(auto_write_op(&config)).await.expect("send");
    let events = terminal_events(&handle).await;
    assert!(events.iter().any(|event| matches!(
        event,
        Event::TurnComplete {
            status: TurnOutcomeStatus::Completed,
            error: None,
            ..
        }
    )));
    assert_eq!(
        model.calls.load(Ordering::SeqCst),
        3,
        "one resume followed by tool-result feedback"
    );
    let starts: Vec<_> = events
        .iter()
        .filter_map(|event| match event {
            Event::ToolCallStarted { id, input, .. } => Some((id, input)),
            _ => None,
        })
        .collect();
    assert_eq!(starts.len(), 1, "unfinished tool must never dispatch");
    assert_eq!(starts[0].0, "reused-call");
    let expected = json!({"action":"read","path":"fixture.txt"});
    assert_eq!(starts[0].1, &expected);
    assert!(events.iter().any(|event| matches!(event,
        Event::ToolCallComplete { id, result: Ok(result), .. } if id == "reused-call" && result.success)));
    assert!(!workspace.path().join("never.txt").exists());
    assert!(model.dropped.load(Ordering::SeqCst));
    {
        let requests = model.requests.lock().unwrap();
        assert_eq!(requests.len(), 3);
        assert_eq!(
            serde_json::to_value(&requests[0].messages).unwrap(),
            serde_json::to_value(&requests[1].messages).unwrap(),
            "headless resume reissues persisted history without partial tool arguments"
        );
        assert!(
            !requests[1]
                .messages
                .iter()
                .flat_map(|message| &message.content)
                .any(
                    |block| matches!(block, ContentBlock::ToolUse { id, .. } if id == "reused-call")
                )
        );
        let history_input = requests[2]
            .messages
            .iter()
            .flat_map(|message| &message.content)
            .find_map(|block| match block {
                ContentBlock::ToolUse { id, input, .. } if id == "reused-call" => Some(input),
                _ => None,
            })
            .expect("fresh complete tool arguments in follow-up history");
        assert_eq!(history_input["action"], expected["action"]);
        assert_eq!(history_input["path"], expected["path"]);
        assert!(
            history_input.get("content").is_none(),
            "unfinished writer must not contaminate fresh reader history"
        );
    }
    handle.send(Op::Shutdown).await.expect("shutdown");
    task.await.expect("Engine task");
}
