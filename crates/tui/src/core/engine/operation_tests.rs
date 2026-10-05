//! Drive the production Engine retry/round boundaries with controlled streams.
use super::*;
use crate::llm_client::mock::{MockLlmClient, canned};
use crate::llm_client::{LlmClient, StreamEventBox};
use crate::models::MessageRequest;
use std::collections::VecDeque;
use std::sync::Mutex;

struct OperationClient {
    // None means a controlled connection failure before any stream content.
    streams: Mutex<VecDeque<Option<Vec<crate::models::StreamEvent>>>>,
    receipts: Mutex<Vec<(Option<uuid::Uuid>, MessageRequest)>>,
}

impl LlmClient for OperationClient {
    fn provider_name(&self) -> &'static str {
        "deepseek"
    }
    fn model(&self) -> &str {
        "deepseek-chat"
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
    let workspace = tempdir().unwrap();
    fs::write(workspace.path().join("fixture.txt"), "fixture").unwrap();
    let client = Arc::new(OperationClient {
        streams: Mutex::new(streams.into()),
        receipts: Mutex::new(Vec::new()),
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
    client.receipts.lock().unwrap().clone()
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
