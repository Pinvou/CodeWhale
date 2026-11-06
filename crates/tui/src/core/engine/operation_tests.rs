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

struct RateRefusalClient {
    calls: AtomicUsize,
}
impl LlmClient for RateRefusalClient {
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
        anyhow::bail!("stream fixture only")
    }
    async fn create_message_stream(
        &self,
        request: MessageRequest,
    ) -> anyhow::Result<StreamEventBox> {
        if self.calls.fetch_add(1, Ordering::SeqCst) == 0 {
            MockLlmClient::new(vec![canned::tool_call_turn(
                "call-read",
                "read_file",
                r#"{"path":"fixture.txt"}"#,
            )])
            .create_message_stream(request)
            .await
        } else {
            Err(crate::llm_client::LlmError::RateLimited {
                message: "recovery exhausted".into(),
                retry_after: Some(Duration::from_secs(3600)),
            }
            .into())
        }
    }
}

#[test]
fn forkguard_rate_limit_interrupt_preserves_executed_tool_history() {
    crate::test_support::run_on_engine_thread(|| async {
        let workspace = tempdir().unwrap();
        fs::write(workspace.path().join("fixture.txt"), "fixture").unwrap();
        let client = Arc::new(RateRefusalClient {
            calls: AtomicUsize::new(0),
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
        let (status, error) = engine
            .run_turn(
                &mut crate::core::turn::TurnContext::new(6),
                policy,
                None,
                None,
            )
            .await;
        assert_eq!(status, TurnOutcomeStatus::Interrupted, "{error:?}");
        assert!(error.is_some());
        assert_eq!(client.calls.load(Ordering::SeqCst), 2);
        let results = engine.session.messages.iter().flat_map(|message| &message.content)
        .filter(|block| matches!(block, ContentBlock::ToolResult { tool_use_id, .. } if tool_use_id == "call-read")).count();
        assert_eq!(
            results, 1,
            "an exhausted model recovery must not replay executed tools"
        );
    });
}

#[test]
fn forkguard_rate_limit_terminal_barrier_parks_children_and_honors_cancel() {
    crate::test_support::run_on_engine_thread(|| async {
        for cancelled in [false, true] {
            let workspace = tempdir().unwrap();
            let client = Arc::new(RateRefusalClient {
                calls: AtomicUsize::new(1),
            });
            let (mut engine, _handle) = Engine::new_with_model_client(
                deterministic_engine_config(workspace.path()),
                &Config::default(),
                client,
            );
            engine.session.messages.push(Message {
                role: Role::User,
                content: vec![ContentBlock::Text {
                    text: "terminal fixture".into(),
                    cache_control: None,
                }],
            });
            let registry =
                crate::tools::ToolRegistry::new(crate::tools::ToolContext::new(workspace.path()));
            let tools = Some(registry.to_api_tools_with_cache(true));
            let policy = test_tool_surface(&engine, registry, tools, AppMode::Agent);
            let (status, _) = engine
                .run_turn(
                    &mut crate::core::turn::TurnContext::new(2),
                    policy,
                    None,
                    None,
                )
                .await;
            assert_eq!(status, TurnOutcomeStatus::Interrupted);
            let recovery_pause = std::mem::take(&mut engine.model_recovery_interrupted);
            assert!(recovery_pause);
            let token = CancellationToken::new();
            let (mailbox, _receiver) = Mailbox::new(token.clone());
            let children = Arc::new(ForegroundChildRegistry::new());
            let child_token = token.child_token();
            let registration = children
                .register("recovery-owned-child", child_token.clone())
                .unwrap();
            let parked = registration.parking_signal();
            let child = tokio::spawn(async move {
                child_token.cancelled().await;
                drop(registration);
            });
            let (flush_tx, flush_rx) = tokio::sync::oneshot::channel();
            let drain_handle = tokio::spawn(async move {
                let _ = flush_rx.await;
            });
            let barrier = TurnMailboxBarrier {
                mailbox,
                cancel_token: token,
                foreground_children: children,
                flush_tx,
                drain_handle,
            };
            if cancelled {
                engine.cancel_token.cancel();
            }
            settle_turn_mailbox(
                Some(barrier),
                status,
                recovery_pause,
                engine.cancel_token.is_cancelled(),
            )
            .await;
            child.await.unwrap();
            assert_eq!(
                parked.load(Ordering::Acquire),
                !cancelled,
                "provider recovery preserves continuation, explicit cancellation does not"
            );
        }
    });
}

struct TransportDropClient {
    inner: crate::client::DeepSeekClient,
    calls: AtomicUsize,
    drops: usize,
    partial: bool,
    ids: Mutex<Vec<uuid::Uuid>>,
}
impl LlmClient for TransportDropClient {
    fn provider_name(&self) -> &'static str {
        "deepseek"
    }
    fn model(&self) -> &str {
        LlmClient::model(&self.inner)
    }
    async fn create_message(
        &self,
        request: MessageRequest,
    ) -> anyhow::Result<crate::models::MessageResponse> {
        self.inner.create_message(request).await
    }
    async fn create_message_stream(
        &self,
        request: MessageRequest,
    ) -> anyhow::Result<StreamEventBox> {
        self.create_message_stream_for_operation(request, uuid::Uuid::new_v4())
            .await
    }
    async fn create_message_stream_for_operation(
        &self,
        request: MessageRequest,
        id: uuid::Uuid,
    ) -> anyhow::Result<StreamEventBox> {
        self.ids.lock().unwrap().push(id);
        let stream = self
            .inner
            .create_message_stream_for_operation(request, id)
            .await?;
        if self.calls.fetch_add(1, Ordering::SeqCst) < self.drops {
            drop(stream);
            let mut events = Vec::new();
            if self.partial {
                events.extend([
                    Ok(canned::message_start("partial-refusal-fixture")),
                    Ok(canned::text_block_start(0)),
                    Ok(canned::text_delta(0, "already delivered partial answer")),
                ]);
            }
            events.push(Err(anyhow::anyhow!(
                "Stream read error: connection reset before content"
            )));
            Ok(Box::pin(futures_util::stream::iter(events)))
        } else {
            Ok(stream)
        }
    }
}

#[test]
fn forkguard_rate_limit_stream_reopen_keeps_budget_identity_and_receipts() {
    crate::test_support::run_on_engine_thread(|| async {
        use wiremock::matchers::method;
        use wiremock::{Mock, MockServer, Request, ResponseTemplate};
        for (max_attempts, drops, refusal_index, partial, expected_wires) in [
            (2, 1, 1, false, 2),
            (3, 1, 1, false, 3),
            (4, 3, 3, false, 4),
            (2, 1, 0, true, 3),
            (2, 1, 0, false, 2),
        ] {
            let server = MockServer::start().await;
            let wires = Arc::new(AtomicUsize::new(0));
            let seen = wires.clone();
            Mock::given(method("POST")).respond_with(move |_: &Request| {
            if seen.fetch_add(1, Ordering::SeqCst) == refusal_index {
                ResponseTemplate::new(429).insert_header("retry-after", "0")
                    .set_body_json(serde_json::json!({"error":{"code":"insufficient_quota","message":"TPM exceeded"}}))
            } else {
                ResponseTemplate::new(200).insert_header("content-type", "text/event-stream")
                    .set_body_string(concat!(
                        "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n",
                        "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
                        "data: [DONE]\n\n"))
            }
        }).mount(&server).await;
            let mut policy = crate::config::RateLimitRetryPolicy::default();
            policy.max_attempts = max_attempts;
            policy.initial_delay_secs = 1;
            policy.max_delay_secs = 1;
            let config = Config {
                provider: Some("custom".into()),
                // Wiremock may reuse a server address between independent cases.
                // Each case owns a separate credential and recovery episode.
                api_key: Some(format!(
                    "owned-{max_attempts}-{drops}-{refusal_index}-{partial}"
                )),
                base_url: Some(server.uri()),
                retry: Some(crate::config::RetryConfig {
                    rate_limit: policy,
                    enabled: None,
                    max_retries: Some(0),
                    initial_delay: None,
                    max_delay: None,
                    exponential_base: None,
                }),
                ..Default::default()
            };
            let client = Arc::new(TransportDropClient {
                inner: crate::client::DeepSeekClient::new(&config).unwrap(),
                calls: AtomicUsize::new(0),
                drops,
                partial,
                ids: Mutex::new(Vec::new()),
            });
            let workspace = tempdir().unwrap();
            let (mut engine, handle) = Engine::new_with_model_client(
                deterministic_engine_config(workspace.path()),
                &config,
                client.clone(),
            );
            engine.session.messages.push(Message {
                role: Role::User,
                content: vec![ContentBlock::Text {
                    text: "stream recovery fixture".into(),
                    cache_control: None,
                }],
            });
            let registry =
                crate::tools::ToolRegistry::new(crate::tools::ToolContext::new(workspace.path()));
            let tools = Some(registry.to_api_tools_with_cache(true));
            let surface = test_tool_surface(&engine, registry, tools, AppMode::Agent);
            let (status, error) = engine
                .run_turn(
                    &mut crate::core::turn::TurnContext::new(2),
                    surface,
                    None,
                    None,
                )
                .await;
            assert_eq!(
                status,
                if max_attempts == 3 || partial {
                    TurnOutcomeStatus::Completed
                } else {
                    TurnOutcomeStatus::Interrupted
                },
                "{error:?}"
            );
            assert_eq!(wires.load(Ordering::SeqCst), expected_wires);
            let ids = client.ids.lock().unwrap().clone();
            assert_eq!(ids.len(), drops + 1);
            if partial {
                assert_ne!(
                    ids[0], ids[1],
                    "persisted partial content changes the logical request"
                );
            } else {
                assert!(
                    ids.iter().all(|id| *id == ids[0]),
                    "ordinary routes retain a logical operation through inner and outer recovery"
                );
            }
            let expected = if max_attempts == 3 || partial {
                "waiting"
            } else {
                "interrupted"
            };
            let mut events = handle.rx_event.write().await;
            let mut receipt_seen = false;
            while let Ok(event) = events.try_recv() {
                if let Event::ModelRecovery { receipt } = event {
                    receipt_seen |= receipt.state == expected
                        && receipt.operation_id.as_deref() == Some(ids[0].to_string().as_str());
                }
            }
            assert!(
                receipt_seen,
                "transparent reopen must publish its recovery state"
            );
            let requests = server.received_requests().await.unwrap();
            if partial {
                assert_eq!(requests[0].body, requests[1].body);
                assert_ne!(requests[1].body, requests[2].body);
                assert!(
                    String::from_utf8_lossy(&requests[2].body)
                        .contains("already delivered partial answer")
                );
            } else {
                assert!(
                    requests
                        .iter()
                        .all(|request| request.body == requests[0].body)
                );
            }
            assert!(
                requests
                    .iter()
                    .all(|request| !request.headers.contains_key("idempotency-key")),
                "runtime identity does not opt a provider into a wire header"
            );
        }
    });
}

#[test]
fn forkguard_rate_limit_vision_tool_dispatch_publishes_recovery() {
    crate::test_support::run_on_engine_thread(|| async {
        use base64::Engine as _;
        use wiremock::matchers::method;
        use wiremock::{Mock, MockServer, Request, ResponseTemplate};
        let server = MockServer::start().await;
        let calls = Arc::new(AtomicUsize::new(0));
        let seen = calls.clone();
        Mock::given(method("POST"))
            .respond_with(move |_: &Request| {
                if seen.fetch_add(1, Ordering::SeqCst) == 0 {
                    ResponseTemplate::new(429).insert_header("retry-after", "0")
                } else {
                    ResponseTemplate::new(200).set_body_json(
                        serde_json::json!({"choices":[{"message":{"content":"vision done"}}]}),
                    )
                }
            })
            .mount(&server)
            .await;
        let workspace = tempdir().unwrap();
        let png = base64::engine::general_purpose::STANDARD.decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=").unwrap();
        fs::write(workspace.path().join("tiny.png"), png).unwrap();
        let base = server.uri();
        let mut policy = crate::config::RateLimitRetryPolicy::default();
        policy.initial_delay_secs = 1;
        policy.max_delay_secs = 1;
        let route = crate::client::DeepSeekClient::new(&Config {
            api_key: Some("owned-vision-key".into()),
            base_url: Some(base.clone()),
            retry: Some(crate::config::RetryConfig {
                rate_limit: policy,
                enabled: None,
                max_retries: None,
                initial_delay: None,
                max_delay: None,
                exponential_base: None,
            }),
            ..Default::default()
        })
        .unwrap();
        let context = crate::tools::ToolContext::new(workspace.path())
            .with_origin_turn_id("vision-tool-turn");
        let mut registry = crate::tools::ToolRegistry::new(context);
        registry.register(Arc::new(
            crate::vision::tools::ImageAnalyzeTool::new_with_route_client(
                crate::config::VisionModelConfig {
                    model: "vision-fixture".into(),
                    api_key: Some("owned-vision-key".into()),
                    base_url: Some(base),
                },
                Some(route),
            ),
        ));
        let (tx, mut rx) = tokio::sync::mpsc::channel(64);
        Engine::execute_tool_with_lock(
            Arc::new(tokio::sync::RwLock::new(())),
            true,
            false,
            tx,
            None,
            "image_analyze".into(),
            serde_json::json!({"image_path":"tiny.png"}),
            workspace.path().to_owned(),
            Some(&registry),
            None,
            None,
            None,
        )
        .await
        .unwrap();
        let mut receipts = Vec::new();
        while let Ok(event) = rx.try_recv() {
            if let Event::ModelRecovery { receipt } = event {
                receipts.push(receipt);
            }
        }
        assert!(receipts.iter().any(|r| r.state == "waiting"));
        assert!(receipts.iter().any(|r| r.state == "recovered"));
        assert!(
            receipts
                .iter()
                .all(|r| r.turn_id.as_deref() == Some("vision-tool-turn")
                    && r.operation_id.is_some())
        );
        assert!(
            receipts
                .iter()
                .all(|r| r.operation_id == receipts[0].operation_id)
        );
        assert_eq!(calls.load(Ordering::SeqCst), 2);
    });
}
