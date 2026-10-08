//! Loopback production-transport receipts for logical-call idempotency.
use super::*;
use crate::models::StreamEvent;
use serde_json::json;
use wiremock::matchers::method;
use wiremock::{Mock, MockServer, ResponseTemplate};

fn request() -> MessageRequest {
    MessageRequest {
        model: "deepseek-v4-pro".into(),
        messages: vec![Message {
            role: Role::User,
            content: vec![ContentBlock::Text {
                text: "same user prompt".into(),
                cache_control: None,
            }],
        }],
        max_tokens: 64,
        system: None,
        tools: None,
        tool_choice: None,
        metadata: None,
        thinking: None,
        reasoning_effort: Some("off".into()),
        stream: Some(false),
        temperature: None,
        top_p: None,
    }
}

fn config(base: &str, header: Option<&str>) -> Config {
    Config {
        provider: Some("deepseek".into()),
        api_key: Some("owned-test-key".into()),
        base_url: Some(base.into()),
        default_text_model: Some("deepseek-v4-pro".into()),
        request_idempotency_header: header.map(str::to_owned),
        ..Config::default()
    }
}

#[tokio::test]
async fn forkguard_model_operation_auxiliary_retry_keys() {
    for lane in [
        "translation",
        "responses",
        "anthropic",
        "fim",
        "speech",
        "search",
    ] {
        for enabled in [false, true] {
            for parent in [None, Some(uuid::Uuid::new_v4())] {
                let server = MockServer::start().await;
                let attempts = Arc::new(std::sync::atomic::AtomicUsize::new(0));
                Mock::given(method("POST"))
                    .respond_with(move |_: &wiremock::Request| {
                        // The Anthropic non-streaming adapter is genuinely
                        // single-dispatch; do not invent retry semantics for it.
                        if attempts.fetch_add(1, std::sync::atomic::Ordering::SeqCst) == 0
                            && lane != "anthropic"
                        {
                            ResponseTemplate::new(503)
                        } else if lane == "responses" {
                            // The production Responses message adapter collects
                            // the provider's typed SSE events, unlike native
                            // search which decodes a non-streaming JSON reply.
                            let frames = [
                                json!({"type":"response.created","response":{"id":"owned-response","status":"in_progress"}}),
                                json!({"type":"response.output_item.added","output_index":0,
                                    "item":{"type":"message","id":"owned-message","role":"assistant","content":[]}}),
                                json!({"type":"response.content_part.added","item_id":"owned-message",
                                    "output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}),
                                json!({"type":"response.output_text.delta","item_id":"owned-message",
                                    "output_index":0,"content_index":0,"delta":"done"}),
                                json!({"type":"response.output_text.done","item_id":"owned-message",
                                    "output_index":0,"content_index":0,"text":"done"}),
                                json!({"type":"response.output_item.done","output_index":0,
                                    "item":{"type":"message","id":"owned-message","role":"assistant",
                                    "status":"completed","content":[{"type":"output_text","text":"done","annotations":[]}]}}),
                                json!({"type":"response.completed","response":{"id":"owned-response",
                                    "status":"completed","model":"gpt-5.5","output":[],
                                    "usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}),
                            ];
                            let sse = frames.iter().map(|frame| format!("data: {frame}\n\n")).collect::<String>();
                            ResponseTemplate::new(200).insert_header("content-type", "text/event-stream")
                                .set_body_string(sse)
                        } else {
                            let body = match lane {
                                "responses" | "search" => json!({
                                    "id":"owned-response", "object":"response", "status":"completed",
                                    "model":"owned-model", "output":[{"type":"message",
                                    "id":"owned-message", "role":"assistant", "status":"completed",
                                    "content":[{"type":"output_text","text":"done","annotations":[]}]}],
                                    "usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}
                                }),
                                "anthropic" => json!({
                                    "id":"owned-message", "type":"message", "role":"assistant",
                                    "model":"claude-sonnet-4-6", "content":[{"type":"text","text":"done"}],
                                    "stop_reason":"end_turn", "stop_sequence":null,
                                    "usage":{"input_tokens":1,"output_tokens":1}
                                }),
                                "fim" => json!({"choices":[{"text":"done"}]}),
                                "speech" => json!({"choices":[{"message":{"audio":{
                                    "data":"aGk=", "transcript":"hi"
                                }}}]}),
                                _ => json!({"choices":[{"message":{"role":"assistant","content":"done"}}]}),
                            };
                            ResponseTemplate::new(200).set_body_json(body)
                        }
                    })
                    .mount(&server)
                    .await;
                let mut cfg = config(&server.uri(), enabled.then_some("idempotency-key"));
                let model = match lane {
                    "responses" => "gpt-5.5",
                    "anthropic" => "claude-sonnet-4-6",
                    "speech" => "mimo-v2-tts",
                    _ => "deepseek-v4-pro",
                };
                if matches!(lane, "responses" | "anthropic") {
                    cfg.provider = Some("opencode-zen".into());
                    cfg.providers = Some(crate::config::ProvidersConfig {
                        opencode_zen: crate::config::ProviderConfig {
                            api_key: Some("owned-test-key".into()),
                            base_url: Some(server.uri()),
                            model: Some(model.into()),
                            ..Default::default()
                        },
                        ..Default::default()
                    });
                } else if lane == "speech" {
                    cfg.provider = Some("xiaomi-mimo".into());
                }
                cfg.default_text_model = Some(model.into());
                let mut client = DeepSeekClient::new(&cfg).unwrap();
                client.retry.enabled = true;
                client.retry.max_retries = 1;
                client.retry.initial_delay = 0.001;
                client.retry.max_delay = 0.001;
                client.isolated_request_state = true;
                client.operation_id = parent;
                for _ in 0..2 {
                    match lane {
                        "translation" | "responses" | "anthropic" => {
                            assert_eq!(
                                client
                                    .translate("hello", model, "English")
                                    .await
                                    .unwrap_or_else(|error| panic!("{lane}: {error}")),
                                "done"
                            );
                        }
                        "fim" => {
                            assert_eq!(
                                client
                                    .fim_completion(model, "prefix", "suffix", 64)
                                    .await
                                    .unwrap(),
                                "done"
                            );
                        }
                        "speech" => {
                            let result = client
                                .synthesize_speech(SpeechSynthesisRequest {
                                    model: model.into(),
                                    text: "hello".into(),
                                    instruction: None,
                                    audio_format: "wav".into(),
                                    voice: None,
                                })
                                .await
                                .unwrap();
                            assert_eq!(result.audio_bytes, b"hi");
                        }
                        "search" => {
                            let search = ProviderNativeSearchClient::new(client.clone()).unwrap();
                            assert_eq!(
                                search
                                    .search(&ProviderNativeSearchRequest {
                                        query: "owned query".into(),
                                        max_results: 1,
                                        domains: vec![],
                                    })
                                    .await
                                    .unwrap()
                                    .answer
                                    .as_deref(),
                                Some("done")
                            );
                        }
                        _ => unreachable!(),
                    }
                    assert_eq!(client.operation_id, parent, "{lane}: parent stays frozen");
                }
                let receipts = server.received_requests().await.unwrap();
                assert_eq!(
                    receipts.len(),
                    if lane == "anthropic" { 2 } else { 3 },
                    "{lane}"
                );
                for receipt in &receipts {
                    let body: serde_json::Value = serde_json::from_slice(&receipt.body).unwrap();
                    let suffix = match lane {
                        "responses" | "search" => "/responses",
                        "anthropic" => "/messages",
                        "fim" => "/beta/completions",
                        _ => "/chat/completions",
                    };
                    assert!(
                        receipt.url.path().ends_with(suffix),
                        "{lane}: wrong wire endpoint"
                    );
                    match lane {
                        "responses" | "search" => {
                            assert!(body.get("input").is_some(), "{lane}: Responses input");
                            assert!(body.get("messages").is_none(), "{lane}: not Chat");
                            if lane == "search" {
                                assert!(
                                    body["tools"]
                                        .as_array()
                                        .unwrap()
                                        .iter()
                                        .any(|tool| { tool["type"] == "web_search" })
                                );
                            }
                        }
                        "fim" => {
                            assert_eq!(body["prompt"], "prefix");
                            assert_eq!(body["suffix"], "suffix");
                            assert!(body.get("messages").is_none());
                        }
                        "speech" => {
                            assert!(body["messages"].as_array().unwrap().iter().any(|message| {
                                message["role"] == "assistant" && message["content"] == "hello"
                            }));
                            assert!(body.get("audio").is_some());
                        }
                        _ => {
                            assert!(body["messages"].is_array());
                            assert!(body.get("max_tokens").is_some());
                            assert!(body.get("input").is_none());
                        }
                    }
                }
                assert!(
                    receipts.windows(2).all(|pair| pair[0].body == pair[1].body),
                    "{lane}"
                );
                if enabled {
                    assert!(
                        receipts.iter().all(|receipt| {
                            receipt.headers.get_all("idempotency-key").iter().count() == 1
                        }),
                        "{lane}: exactly one logical key on the wire"
                    );
                    let ids: Vec<_> = receipts
                        .iter()
                        .map(|r| r.headers.get("idempotency-key").unwrap().to_str().unwrap())
                        .collect();
                    if lane == "anthropic" {
                        assert_ne!(ids[0], ids[1], "{lane}: new call gets a fresh key");
                    } else {
                        assert_eq!(ids[0], ids[1], "{lane}: retry is the same call");
                        assert_ne!(ids[1], ids[2], "{lane}: new call gets a fresh key");
                    }
                    uuid::Uuid::parse_str(ids[0]).unwrap();
                    if let Some(parent) = parent {
                        assert_ne!(
                            ids[0],
                            parent.to_string(),
                            "{lane}: auxiliary call is not its parent"
                        );
                    }
                } else {
                    assert!(
                        receipts
                            .iter()
                            .all(|r| !r.headers.contains_key("idempotency-key")),
                        "{lane}"
                    );
                }
            }
        }
    }
}

#[tokio::test]
async fn forkguard_model_operation_http_retry_and_new_calls() {
    let server = MockServer::start().await;
    let count = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let seen = count.clone();
    Mock::given(method("POST"))
        .respond_with(move |_: &wiremock::Request| {
            if seen.fetch_add(1, std::sync::atomic::Ordering::SeqCst) == 0 {
                ResponseTemplate::new(503)
            } else {
                ResponseTemplate::new(200).set_body_json(json!({"model":"deepseek-v4-pro",
                "choices":[{"message":{"role":"assistant","content":"done"}}]}))
            }
        })
        .mount(&server)
        .await;
    let mut client = DeepSeekClient::new(&config(&server.uri(), Some("Idempotency-Key"))).unwrap();
    client.retry.enabled = true;
    client.retry.max_retries = 1;
    client.retry.initial_delay = 0.001;
    client.retry.max_delay = 0.001;
    client.isolated_request_state = true;
    let operation = uuid::Uuid::new_v4();
    let mut deterministic_request = request();
    deterministic_request.temperature = Some(0.0);
    client
        .create_message_for_operation(deterministic_request.clone(), operation)
        .await
        .unwrap();
    // A second transport dispatch belonging to the same logical call retains
    // its ID; a separate user action with identical prompt gets a fresh one.
    client
        .create_message_for_operation(deterministic_request.clone(), operation)
        .await
        .unwrap();
    client
        .create_message(deterministic_request.clone())
        .await
        .unwrap();
    client.create_message(deterministic_request).await.unwrap();
    let receipts = server.received_requests().await.unwrap();
    assert_eq!(receipts.len(), 5);
    let ids: Vec<_> = receipts
        .iter()
        .map(|r| r.headers.get("idempotency-key").unwrap().to_str().unwrap())
        .collect();
    assert_eq!(ids[0], operation.to_string());
    assert_eq!(ids[0], ids[1]);
    assert_eq!(ids[1], ids[2]);
    assert_ne!(ids[2], ids[3]);
    assert_ne!(ids[3], ids[4]);
    assert!(receipts.windows(2).all(|pair| pair[0].body == pair[1].body));
}

#[tokio::test]
async fn forkguard_model_operation_compaction_outer_retry_keeps_id() {
    let server = MockServer::start().await;
    let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    Mock::given(method("POST"))
        .respond_with(move |_: &wiremock::Request| {
            if calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst) == 0 {
                ResponseTemplate::new(503)
            } else {
                ResponseTemplate::new(200).set_body_json(json!({"choices":[{"message":{
                    "role":"assistant", "content":"Primary request: migrate the session store. Current work: preserve existing sessions. Pending tasks: finish migration tests."
                }}]}))
            }
        })
        .mount(&server)
        .await;
    let mut client = DeepSeekClient::new(&config(&server.uri(), Some("idempotency-key"))).unwrap();
    client.retry.enabled = false; // The compaction loop must own the 503 retry.
    client.isolated_request_state = true;
    let prepared =
        crate::compaction::PreparedCompactionEnvelope::new(crate::compaction::CompactionConfig {
            model: "deepseek-v4-pro".into(),
            cache_summary: false,
            ..Default::default()
        });
    for _ in 0..2 {
        crate::compaction::compact_messages_safe(&client, &request().messages, None, &prepared)
            .await
            .unwrap();
    }
    let receipts = server.received_requests().await.unwrap();
    assert_eq!(receipts.len(), 3);
    assert!(receipts.windows(2).all(|pair| pair[0].body == pair[1].body));
    let ids: Vec<_> = receipts
        .iter()
        .map(|receipt| {
            receipt
                .headers
                .get("idempotency-key")
                .unwrap()
                .to_str()
                .unwrap()
        })
        .collect();
    assert_eq!(ids[0], ids[1], "outer transient retry is the same call");
    assert_ne!(
        ids[1], ids[2],
        "a separate identical compaction is a new call"
    );
}

#[test]
fn forkguard_model_operation_scoping_preserves_only_resolved_identity() {
    let mut enabled: Config = serde_json::from_value(json!({
        "provider":"alpha",
        "providers": {
            "alpha":{"kind":"openai-compatible", "base_url":"https://owned.example/v1", "model":"test-model"},
            "beta":{"kind":"openai-compatible", "base_url":"https://owned.example/v1", "model":"test-model"}
        }
    })).unwrap();
    enabled.request_idempotency_header = Some("idempotency-key".into());
    for target in ["alpha", "beta"] {
        let identity = enabled.resolve_provider_identity(target).unwrap();
        let mut scoped = enabled.clone();
        scoped.scope_to_provider_identity(&identity);
        assert_eq!(
            scoped.request_idempotency_header.is_some(),
            target == "alpha"
        );
        assert_eq!(scoped.provider.as_deref(), Some(target));
    }
    let deepseek = enabled.resolve_provider_identity("deepseek").unwrap();
    enabled.provider = Some("removed-provider".into());
    enabled.scope_to_provider_identity(&deepseek);
    assert!(
        enabled.request_idempotency_header.is_none(),
        "an unresolved source must fail closed even when api_provider falls back to Deepseek"
    );
}

#[tokio::test]
async fn forkguard_model_operation_opt_in_and_wire_identity() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(
                json!({"choices":[{"message":{"role":"assistant","content":"done"}}]}),
            ),
        )
        .mount(&server)
        .await;
    let ordinary = DeepSeekClient::new(&config(&server.uri(), None)).unwrap();
    ordinary.create_message(request()).await.unwrap();
    assert!(
        !server.received_requests().await.unwrap()[0]
            .headers
            .contains_key("idempotency-key")
    );
    let mut deterministic_request = request();
    deterministic_request.temperature = Some(0.0);
    ordinary
        .create_message(deterministic_request.clone())
        .await
        .unwrap();
    ordinary
        .create_message(deterministic_request)
        .await
        .unwrap();
    let ordinary_receipts = server.received_requests().await.unwrap();
    assert_eq!(
        ordinary_receipts.len(),
        2,
        "ordinary body cache is retained"
    );
    assert!(
        ordinary_receipts
            .iter()
            .all(|receipt| !receipt.headers.contains_key("idempotency-key"))
    );
    let enabled = DeepSeekClient::new(&config(&server.uri(), Some("idempotency-key"))).unwrap();
    let identity = enabled.stream_operation_identity(&request()).unwrap();
    let mut changed = request();
    changed.max_tokens += 1;
    assert_ne!(
        identity,
        enabled.stream_operation_identity(&changed).unwrap()
    );
    let mut metadata = request();
    metadata.metadata = Some(json!({"local_schedule":"changed"}));
    // Chat shaping drops this caller-local metadata: the sent facts match.
    assert_eq!(
        identity,
        enabled.stream_operation_identity(&metadata).unwrap()
    );
    let another =
        DeepSeekClient::new(&config("http://127.0.0.1:18999", Some("idempotency-key"))).unwrap();
    assert_ne!(
        identity,
        another.stream_operation_identity(&request()).unwrap()
    );
    let mut scoped = config(&server.uri(), Some("idempotency-key"));
    scoped.http_headers = Some(std::collections::HashMap::from([
        ("X-Pinvou-Context-Type".into(), "ORG".into()),
        ("X-Pinvou-Context-Id".into(), "owned-org-one".into()),
        ("X-Pinvou-Token-Account-Id".into(), "owned-token-one".into()),
    ]));
    let first = DeepSeekClient::new(&scoped)
        .unwrap()
        .stream_operation_identity(&request())
        .unwrap();
    scoped
        .http_headers
        .as_mut()
        .unwrap()
        .insert("X-Pinvou-Token-Account-Id".into(), "owned-token-two".into());
    assert_ne!(
        first,
        DeepSeekClient::new(&scoped)
            .unwrap()
            .stream_operation_identity(&request())
            .unwrap()
    );
    for invalid in ["Authorization", "Content-Type", "Cookie", "x\r\nbad"] {
        assert!(DeepSeekClient::new(&config(&server.uri(), Some(invalid))).is_err());
    }
    let parsed: Config =
        serde_json::from_value(json!({"request_idempotency_header":"idempotency-key"})).unwrap();
    assert!(parsed.request_idempotency_header.is_none());
    let ordinary_route =
        crate::route_runtime::resolve_runtime_route(&scoped, ApiProvider::Openai, Some("gpt-5.5"))
            .unwrap();
    assert!(ordinary_route.config.request_idempotency_header.is_none());
}

#[tokio::test]
async fn forkguard_model_operation_http1_stream_keeps_id() {
    use futures_util::StreamExt;
    let _lock = crate::test_support::lock_test_env();
    let _http1 = crate::test_support::EnvVarGuard::set("CODEWHALE_FORCE_HTTP1", "1");
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .respond_with(ResponseTemplate::new(200).insert_header("content-type", "text/event-stream")
            .set_body_string(concat!(
                "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n",
                "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
                "data: [DONE]\n\n",
            )))
        .mount(&server).await;
    let client = DeepSeekClient::new(&config(&server.uri(), Some("idempotency-key"))).unwrap();
    let id = uuid::Uuid::new_v4();
    for _ in 0..2 {
        let mut stream = client
            .create_message_stream_for_operation(request(), id)
            .await
            .unwrap();
        let mut text = String::new();
        while let Some(event) = stream.next().await {
            if let StreamEvent::ContentBlockDelta {
                delta: crate::models::Delta::TextDelta { text: part },
                ..
            } = event.unwrap()
            {
                text.push_str(&part);
            }
        }
        assert_eq!(text, "done");
    }
    let receipts = server.received_requests().await.unwrap();
    assert_eq!(receipts.len(), 2);
    assert!(receipts.iter().all(|receipt| {
        receipt
            .headers
            .get("idempotency-key")
            .unwrap()
            .to_str()
            .unwrap()
            == id.to_string()
    }));
}
