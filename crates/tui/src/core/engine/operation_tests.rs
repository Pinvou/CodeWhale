//! Drive the production Engine retry/round boundaries with controlled streams.
use super::*;
use crate::llm_client::mock::{MockLlmClient, canned};
use crate::llm_client::{LlmClient, PreparedStreamCall, StreamEventBox};
use crate::models::MessageRequest;
use std::collections::VecDeque;
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};

struct OperationClient {
    // None means a controlled connection failure before any stream content.
    streams: Mutex<VecDeque<Option<Vec<crate::models::StreamEvent>>>>,
    receipts: Mutex<Vec<(Option<uuid::Uuid>, MessageRequest)>>,
    preparations: AtomicUsize,
    prepared_receipts: Mutex<Vec<usize>>,
}

impl LlmClient for OperationClient {
    fn provider_name(&self) -> &'static str {
        "deepseek"
    }
    fn model(&self) -> &str {
        "deepseek-chat"
    }
    fn stream_operation_identity(&self, _: &MessageRequest) -> anyhow::Result<String> {
        panic!("Engine must not invoke the legacy identity hook")
    }
    fn prepare_stream_call(&self, request: MessageRequest) -> anyhow::Result<PreparedStreamCall> {
        let ordinal = self.preparations.fetch_add(1, Ordering::SeqCst) + 1;
        let identity =
            crate::llm_client::caller_stream_operation_identity("deepseek", None, &request)?;
        Ok(PreparedStreamCall::with_transport(
            request, identity, ordinal,
        ))
    }
    async fn create_prepared_message_stream_for_operation(
        &self,
        prepared: PreparedStreamCall,
        id: uuid::Uuid,
    ) -> anyhow::Result<StreamEventBox> {
        let ordinal = *prepared
            .transport::<usize>()
            .expect("fixture prepared payload");
        self.prepared_receipts.lock().unwrap().push(ordinal);
        self.dispatch(prepared.request, Some(id)).await
    }
    async fn create_message(
        &self,
        _: MessageRequest,
    ) -> anyhow::Result<crate::models::MessageResponse> {
        anyhow::bail!("streaming fixture only")
    }
    async fn create_message_stream(
        &self,
        request: MessageRequest,
    ) -> anyhow::Result<StreamEventBox> {
        self.dispatch(request, None).await
    }
    async fn create_message_stream_for_operation(
        &self,
        request: MessageRequest,
        id: uuid::Uuid,
    ) -> anyhow::Result<StreamEventBox> {
        self.dispatch(request, Some(id)).await
    }
}

impl OperationClient {
    async fn dispatch(
        &self,
        request: MessageRequest,
        id: Option<uuid::Uuid>,
    ) -> anyhow::Result<StreamEventBox> {
        self.receipts.lock().unwrap().push((id, request.clone()));
        let step = self
            .streams
            .lock()
            .unwrap()
            .pop_front()
            .expect("fixture exhausted");
        match step {
            Some(events)
                if !events
                    .iter()
                    .any(|event| matches!(event, crate::models::StreamEvent::MessageStop)) =>
            {
                let mut stream: Vec<anyhow::Result<crate::models::StreamEvent>> =
                    events.into_iter().map(Ok).collect();
                stream.push(Err(anyhow::anyhow!(
                    "Stream read error: error decoding response body"
                )));
                Ok(Box::pin(futures_util::stream::iter(stream)))
            }
            Some(events) => {
                MockLlmClient::new(vec![events])
                    .create_message_stream(request)
                    .await
            }
            None => Ok(Box::pin(futures_util::stream::once(async {
                Err(anyhow::anyhow!(
                    "error decoding response body: connection reset"
                ))
            }))),
        }
    }
}

async fn run_operation_fixture(
    streams: Vec<Option<Vec<crate::models::StreamEvent>>>,
) -> Vec<(Option<uuid::Uuid>, MessageRequest)> {
    run_prepared_operation_fixture(streams).await.0
}

async fn run_prepared_operation_fixture(
    streams: Vec<Option<Vec<crate::models::StreamEvent>>>,
) -> (Vec<(Option<uuid::Uuid>, MessageRequest)>, Vec<usize>, usize) {
    let workspace = tempdir().unwrap();
    fs::write(workspace.path().join("fixture.txt"), "fixture").unwrap();
    let client = Arc::new(OperationClient {
        streams: Mutex::new(streams.into()),
        receipts: Mutex::new(Vec::new()),
        preparations: AtomicUsize::new(0),
        prepared_receipts: Mutex::new(Vec::new()),
    });
    let (mut engine, _handle) = Engine::new_with_model_client(
        deterministic_engine_config(workspace.path()),
        &Config::default(),
        client.clone(),
    );
    engine.session.messages.push(Message {
        role: Role::User,
        content: vec![ContentBlock::Text {
            text: "same user prompt".into(),
            cache_control: None,
        }],
    });
    let mut registry =
        crate::tools::ToolRegistry::new(crate::tools::ToolContext::new(workspace.path()));
    registry.register(Arc::new(crate::tools::file::ReadFileTool));
    let tools = Some(registry.to_api_tools_with_cache(true));
    let policy = test_tool_surface(&engine, registry, tools, AppMode::Agent);
    let (status, error) = tokio::time::timeout(
        Duration::from_secs(20),
        engine.run_turn(
            &mut crate::core::turn::TurnContext::new(6),
            policy,
            None,
            None,
        ),
    )
    .await
    .expect("bounded fixture");
    assert_eq!(status, TurnOutcomeStatus::Completed, "{error:?}");
    assert!(client.streams.lock().unwrap().is_empty());
    let receipts = client.receipts.lock().unwrap().clone();
    let prepared = client.prepared_receipts.lock().unwrap().clone();
    (
        receipts,
        prepared,
        client.preparations.load(Ordering::SeqCst),
    )
}

#[tokio::test]
async fn forkguard_model_operation_prepared_payload_survives_transparent_retries() {
    let (receipts, payloads, preparations) = run_prepared_operation_fixture(vec![
        None,
        None,
        None,
        Some(canned::simple_text_turn("done")),
    ])
    .await;
    assert_eq!(payloads, vec![1, 1, 1, 2]);
    assert_eq!(preparations, 2, "only the outer round prepares again");
    assert!(receipts.iter().all(|receipt| receipt.0 == receipts[0].0));
}

struct PreparationFailureClient {
    cancel: Mutex<Option<tokio_util::sync::CancellationToken>>,
    preparations: AtomicUsize,
}

struct OrdinaryPreparationClient;
impl LlmClient for OrdinaryPreparationClient {
    fn provider_name(&self) -> &'static str {
        "ordinary"
    }
    fn model(&self) -> &str {
        "ordinary-model"
    }
    fn stream_operation_identity(&self, _: &MessageRequest) -> anyhow::Result<String> {
        panic!("ordinary preparation must not serialize or hash identity")
    }
    async fn create_message(
        &self,
        _: MessageRequest,
    ) -> anyhow::Result<crate::models::MessageResponse> {
        anyhow::bail!("stream fixture only")
    }
    async fn create_message_stream(
        &self,
        request: MessageRequest,
    ) -> anyhow::Result<StreamEventBox> {
        MockLlmClient::new(vec![canned::simple_text_turn("done")])
            .create_message_stream(request)
            .await
    }
}

#[tokio::test]
async fn forkguard_model_operation_ordinary_default_prepares_without_identity() {
    // Exercise the object-safe blanket adapter and default dispatch, rather
    // than a provider-specific opt-in implementation.
    let client: &dyn crate::core::model_client::ModelClient = &OrdinaryPreparationClient;
    let request: MessageRequest = serde_json::from_value(serde_json::json!({
        "model": "ordinary-model", "messages": [], "max_tokens": 10
    }))
    .unwrap();
    let prepared = client.prepare_stream_call(request).unwrap();
    assert!(prepared.operation_identity.is_none());
    let mut stream = client
        .create_prepared_message_stream_for_operation(prepared, uuid::Uuid::new_v4())
        .await
        .unwrap();
    use futures_util::StreamExt;
    let mut stopped = false;
    while let Some(event) = stream.next().await {
        stopped |= matches!(event.unwrap(), crate::models::StreamEvent::MessageStop);
    }
    assert!(stopped);
}

impl LlmClient for PreparationFailureClient {
    fn provider_name(&self) -> &'static str {
        "deepseek"
    }
    fn model(&self) -> &str {
        "deepseek-chat"
    }
    fn prepare_stream_call(&self, _: MessageRequest) -> anyhow::Result<PreparedStreamCall> {
        self.preparations.fetch_add(1, Ordering::SeqCst);
        if let Some(token) = self.cancel.lock().unwrap().as_ref() {
            token.cancel();
        }
        Err(crate::llm_client::LlmError::authentication_error(
            "fixture preparation authentication failure",
        )
        .into())
    }
    async fn create_message(
        &self,
        _: MessageRequest,
    ) -> anyhow::Result<crate::models::MessageResponse> {
        panic!("failed preparation must not dispatch")
    }
    async fn create_message_stream(&self, _: MessageRequest) -> anyhow::Result<StreamEventBox> {
        panic!("failed preparation must not dispatch")
    }
}

#[tokio::test]
async fn forkguard_model_operation_preparation_failure_uses_error_event_and_biased_cancel() {
    for cancel in [false, true] {
        let workspace = tempdir().unwrap();
        let client = Arc::new(PreparationFailureClient {
            cancel: Mutex::new(None),
            preparations: AtomicUsize::new(0),
        });
        let (mut engine, handle) = Engine::new_with_model_client(
            deterministic_engine_config(workspace.path()),
            &Config::default(),
            client.clone(),
        );
        if cancel {
            *client.cancel.lock().unwrap() = Some(engine.cancel_token.clone());
        }
        engine.session.messages.push(Message {
            role: Role::User,
            content: vec![ContentBlock::Text {
                text: "prepare fixture".into(),
                cache_control: None,
            }],
        });
        let registry =
            crate::tools::ToolRegistry::new(crate::tools::ToolContext::new(workspace.path()));
        let policy = test_tool_surface(&engine, registry, None, AppMode::Agent);
        let (status, error) = tokio::time::timeout(
            Duration::from_secs(20),
            engine.run_turn(
                &mut crate::core::turn::TurnContext::new(6),
                policy,
                None,
                None,
            ),
        )
        .await
        .expect("bounded preparation fixture");
        assert_eq!(client.preparations.load(Ordering::SeqCst), 1);
        let mut receiver = handle.rx_event.write().await;
        let mut errors = Vec::new();
        while let Ok(event) = receiver.try_recv() {
            if let Event::Error { envelope, .. } = event {
                errors.push(envelope.message);
            }
        }
        if cancel {
            assert_eq!(status, TurnOutcomeStatus::Interrupted);
            assert!(
                errors.is_empty(),
                "cancellation wins over ready preparation error"
            );
        } else {
            assert_eq!(status, TurnOutcomeStatus::Failed);
            assert_eq!(
                errors.len(),
                1,
                "preparation errors use the normal Event error channel"
            );
            assert!(errors[0].contains("fixture preparation authentication failure"));
            assert!(error.is_some());
        }
    }
}

#[tokio::test]
async fn forkguard_model_operation_inner_and_outer_stream_retries() {
    // Two inner replacements, then the outer request rebuild after the third
    // no-content failure. All four dispatches are one logical operation.
    let receipts = run_operation_fixture(vec![
        None,
        None,
        None,
        Some(canned::simple_text_turn("done")),
    ])
    .await;
    assert_eq!(receipts.len(), 4);
    let first = receipts[0]
        .0
        .expect("operation-aware boundary must be used");
    assert!(receipts.iter().all(|receipt| receipt.0 == Some(first)));
    let body = serde_json::to_vec(&receipts[0].1).unwrap();
    assert!(
        receipts
            .iter()
            .all(|receipt| serde_json::to_vec(&receipt.1).unwrap() == body)
    );
    let new_turn = run_operation_fixture(vec![Some(canned::simple_text_turn("done"))]).await;
    assert_ne!(
        new_turn[0].0,
        Some(first),
        "new identical user action is not a retry"
    );
}

#[tokio::test]
async fn forkguard_model_operation_completed_identical_round_gets_new_id() {
    let receipts = run_operation_fixture(vec![
        Some(vec![
            canned::message_start("reasoning-only"),
            crate::models::StreamEvent::ContentBlockStart {
                index: 0,
                content_block: crate::models::ContentBlockStart::Thinking {
                    thinking: String::new(),
                },
            },
            canned::thinking_delta(0, "reasoning with no final channel"),
            canned::block_stop(0),
            canned::message_delta("stop", None),
            canned::message_stop(),
        ]),
        Some(canned::simple_text_turn("done")),
    ])
    .await;
    assert_eq!(receipts.len(), 2);
    assert!(receipts.iter().all(|receipt| receipt.0.is_some()));
    assert_eq!(
        serde_json::to_vec(&receipts[0].1).unwrap(),
        serde_json::to_vec(&receipts[1].1).unwrap(),
        "the completed reasoning-only round retries without changing the body"
    );
    assert_ne!(
        receipts[0].0, receipts[1].0,
        "a completed round ends its call"
    );
}

#[tokio::test]
async fn forkguard_model_operation_tool_followup_gets_new_id() {
    let receipts = run_operation_fixture(vec![
        Some(canned::tool_call_turn(
            "call-read",
            "read_file",
            r#"{"path":"fixture.txt"}"#,
        )),
        Some(canned::simple_text_turn("done")),
    ])
    .await;
    assert_eq!(receipts.len(), 2);
    assert!(receipts.iter().all(|receipt| receipt.0.is_some()));
    assert_ne!(receipts[0].0, receipts[1].0);
    assert!(receipts[1].1.messages.iter().any(|message| {
        message.content.iter().any(|block|
        matches!(block, ContentBlock::ToolResult { tool_use_id, .. } if tool_use_id == "call-read")
    )
    }));
}

#[tokio::test]
async fn forkguard_model_operation_preserved_partial_changes_call() {
    let receipts = run_operation_fixture(vec![
        Some(vec![
            canned::message_start("partial-fixture"),
            canned::text_block_start(0),
            canned::text_delta(0, "already delivered partial answer"),
        ]),
        Some(canned::simple_text_turn("continued")),
    ])
    .await;
    assert_eq!(receipts.len(), 2);
    assert!(receipts.iter().all(|receipt| receipt.0.is_some()));
    assert_ne!(receipts[0].0, receipts[1].0);
    assert_ne!(
        serde_json::to_vec(&receipts[0].1).unwrap(),
        serde_json::to_vec(&receipts[1].1).unwrap()
    );
    assert!(receipts[1].1.messages.iter().any(|message| {
        message.role == Role::Assistant
            && message.content.iter().any(|block| {
                matches!(block, ContentBlock::Text { text, .. }
                    if text == "already delivered partial answer")
            })
    }));
}
