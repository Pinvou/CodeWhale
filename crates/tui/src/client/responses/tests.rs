use super::*;

use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use futures_util::StreamExt;

use crate::config::{Config, ProviderConfig, ProvidersConfig, RetryConfig};
use crate::models::Message;
use crate::models::Role;
use crate::models::SystemPrompt;
use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, Request, Respond, ResponseTemplate};

/// The catalog Codex endpoint: `provider_base_url_is_official` matches the
/// exact URL, so this literal pins the official side of the fingerprint-less
/// replay arm (the config crate keeps the constant crate-private).
const OFFICIAL_CODEX_BASE_URL: &str = "https://chatgpt.com/backend-api";

/// Placeholder endpoint for body-builder tests that do not exercise the
/// fingerprint-less replay arm — its officialness only matters there.
const BODY_TEST_BASE_URL: &str = "https://relay.example.test/v1";

#[derive(Clone)]
struct RetryThenSuccess {
    attempts: Arc<AtomicUsize>,
    retry_status: u16,
    retry_body: &'static str,
}

impl Respond for RetryThenSuccess {
    fn respond(&self, _request: &Request) -> ResponseTemplate {
        if self.attempts.fetch_add(1, Ordering::SeqCst) == 0 {
            let mut response =
                ResponseTemplate::new(self.retry_status).set_body_string(self.retry_body);
            if self.retry_status == 429 {
                response = response.insert_header("Retry-After", "0");
            }
            return response;
        }

        ResponseTemplate::new(200)
            .insert_header("Content-Type", "text/event-stream")
            .set_body_string("data: [DONE]\n\n")
    }
}

#[derive(Clone)]
struct AlwaysError {
    attempts: Arc<AtomicUsize>,
    status: u16,
    body: &'static str,
}

impl Respond for AlwaysError {
    fn respond(&self, _request: &Request) -> ResponseTemplate {
        self.attempts.fetch_add(1, Ordering::SeqCst);
        ResponseTemplate::new(self.status).set_body_string(self.body)
    }
}

fn minimal_responses_request() -> MessageRequest {
    MessageRequest {
        model: "gpt-5.5".to_string(),
        messages: vec![Message {
            role: Role::User,
            content: vec![ContentBlock::Text {
                text: "hello".to_string(),
                cache_control: None,
            }],
        }],
        max_tokens: 128,
        system: None,
        tools: None,
        tool_choice: None,
        metadata: None,
        thinking: None,
        reasoning_effort: None,
        stream: None,
        temperature: None,
        top_p: None,
    }
}

fn test_codex_config(server: &MockServer) -> Config {
    Config {
        provider: Some("openai-codex".to_string()),
        retry: Some(RetryConfig {
            enabled: Some(true),
            max_retries: Some(1),
            initial_delay: Some(0.0),
            max_delay: Some(0.0),
            exponential_base: Some(1.0),
        }),
        providers: Some(ProvidersConfig {
            openai_codex: ProviderConfig {
                base_url: Some(server.uri()),
                ..ProviderConfig::default()
            },
            ..ProvidersConfig::default()
        }),
        ..Config::default()
    }
}

#[tokio::test]
async fn responses_stream_retries_rate_limited_request() {
    let server = MockServer::start().await;
    let attempts = Arc::new(AtomicUsize::new(0));
    Mock::given(method("POST"))
        .and(path(CODEX_RESPONSES_PATH))
        .respond_with(RetryThenSuccess {
            attempts: Arc::clone(&attempts),
            retry_status: 429,
            retry_body: "rate limited",
        })
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let _codex_token =
            crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", "test-token");
        let _legacy_codex_token = crate::test_support::EnvVarGuard::remove("CODEX_ACCESS_TOKEN");
        DeepSeekClient::new(&test_codex_config(&server)).unwrap()
    };
    let mut request = minimal_responses_request();
    request.max_tokens = 384_000;
    let prepared = client
        .prepare_outbound_request(request, true)
        .expect("responses request prepares");
    // The Codex OAuth Responses endpoint rejects `max_output_tokens`
    // ("Unsupported parameter"), so the prepared body must omit it even
    // though the resolved request envelope carries a cap.
    assert!(prepared.body.get("max_output_tokens").is_none());
    let mut stream = client.handle_responses_stream(&prepared).await.unwrap();

    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while let Some(event) = stream.next().await {
            event.unwrap();
        }
    })
    .await
    .expect("Responses retry stream should finish after [DONE]");

    assert_eq!(attempts.load(Ordering::SeqCst), 2);
    let requests = server
        .received_requests()
        .await
        .expect("recorded retry requests");
    assert_eq!(requests.len(), 2);
    for request in requests {
        let body: Value = serde_json::from_slice(&request.body).expect("Responses JSON");
        assert!(
            body.get("max_output_tokens").is_none(),
            "Codex Responses body must not name the unsupported output cap: {body}"
        );
    }
}

#[tokio::test]
async fn responses_stream_retries_transient_server_error() {
    let server = MockServer::start().await;
    let attempts = Arc::new(AtomicUsize::new(0));
    Mock::given(method("POST"))
        .and(path(CODEX_RESPONSES_PATH))
        .respond_with(RetryThenSuccess {
            attempts: Arc::clone(&attempts),
            retry_status: 503,
            retry_body: "temporarily unavailable",
        })
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let _codex_token =
            crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", "test-token");
        let _legacy_codex_token = crate::test_support::EnvVarGuard::remove("CODEX_ACCESS_TOKEN");
        DeepSeekClient::new(&test_codex_config(&server)).unwrap()
    };
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
        .unwrap();

    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while let Some(event) = stream.next().await {
            event.unwrap();
        }
    })
    .await
    .expect("Responses retry stream should finish after [DONE]");

    assert_eq!(attempts.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn responses_stream_retries_upstream_499_before_streaming() {
    let server = MockServer::start().await;
    let attempts = Arc::new(AtomicUsize::new(0));
    Mock::given(method("POST"))
        .and(path(CODEX_RESPONSES_PATH))
        .respond_with(RetryThenSuccess {
            attempts: Arc::clone(&attempts),
            retry_status: 499,
            retry_body: "upstream request cancelled",
        })
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let _codex_token =
            crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", "test-token");
        let _legacy_codex_token = crate::test_support::EnvVarGuard::remove("CODEX_ACCESS_TOKEN");
        DeepSeekClient::new(&test_codex_config(&server)).unwrap()
    };
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
        .unwrap();

    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while let Some(event) = stream.next().await {
            event.unwrap();
        }
    })
    .await
    .expect("Responses retry stream should finish after [DONE]");

    assert_eq!(attempts.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn responses_stream_finishes_on_semantic_terminal_event_without_done_marker() {
    let server = MockServer::start().await;
    let sse_body = concat!(
        "data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n",
        "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n",
    );
    Mock::given(method("POST"))
        .and(path(CODEX_RESPONSES_PATH))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/event-stream")
                .set_body_string(sse_body),
        )
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let _codex_token =
            crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", "test-token");
        let _legacy_codex_token = crate::test_support::EnvVarGuard::remove("CODEX_ACCESS_TOKEN");
        DeepSeekClient::new(&test_codex_config(&server)).unwrap()
    };
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
        .expect("semantic Responses stream opens");

    let mut saw_stop = false;
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while let Some(event) = stream.next().await {
            if matches!(event.unwrap(), StreamEvent::MessageStop) {
                saw_stop = true;
            }
        }
    })
    .await
    .expect("terminal event ends the stream without [DONE]");
    assert!(saw_stop);
}

#[tokio::test]
async fn responses_stream_surfaces_notice_for_web_search_call_items() {
    let server = MockServer::start().await;
    let sse_body = concat!(
        "data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n",
        "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"call_id\":\"call_1\"}}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\"}}\n\n",
        "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n",
    );
    Mock::given(method("POST"))
        .and(path(CODEX_RESPONSES_PATH))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/event-stream")
                .set_body_string(sse_body),
        )
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let _codex_token =
            crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", "test-token");
        let _legacy_codex_token = crate::test_support::EnvVarGuard::remove("CODEX_ACCESS_TOKEN");
        DeepSeekClient::new(&test_codex_config(&server)).unwrap()
    };
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
        .expect("semantic Responses stream opens");

    let mut saw_notice = false;
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while let Some(event) = stream.next().await {
            if let Ok(StreamEvent::ContentBlockStart {
                content_block: ContentBlockStart::Text { text },
                ..
            }) = event
                && text.contains("not replayed")
            {
                saw_notice = true;
            }
        }
    })
    .await
    .expect("stream terminates");
    assert!(saw_notice, "web_search_call must surface a visible notice");
}

#[tokio::test]
async fn responses_stream_fails_fast_on_non_retryable_provider_error() {
    let server = MockServer::start().await;
    let attempts = Arc::new(AtomicUsize::new(0));
    Mock::given(method("POST"))
        .and(path(CODEX_RESPONSES_PATH))
        .respond_with(AlwaysError {
            attempts: Arc::clone(&attempts),
            status: 403,
            body: "<html><title>Access Denied</title><body>Security alert. Contact support. Ray ID 1234abcd.</body></html>",
        })
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let _codex_token =
            crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", "test-token");
        let _legacy_codex_token = crate::test_support::EnvVarGuard::remove("CODEX_ACCESS_TOKEN");
        DeepSeekClient::new(&test_codex_config(&server)).unwrap()
    };

    let err = match client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
    {
        Ok(_) => panic!("non-retryable Responses errors should fail fast"),
        Err(err) => err,
    };

    assert_eq!(attempts.load(Ordering::SeqCst), 1);
    let message = format!("{err:#}");
    assert!(
        message.contains("Responses API request failed"),
        "{message}"
    );
    assert!(message.contains("OpenAI Codex"), "{message}");
    assert!(message.contains("Access Denied"), "{message}");
    assert!(
        message.contains("blocked before it reached the model"),
        "{message}"
    );
    // #3884: the structured LlmError must stay downcastable through the
    // context layers so sub-agent failure records can classify it.
    assert!(
        err.downcast_ref::<crate::llm_client::LlmError>().is_some(),
        "LlmError should survive the anyhow chain"
    );
}

#[test]
fn responses_body_serializes_the_child_catalog_without_duplication() {
    // Mirror of the Anthropic contract: the real child catalog fixture
    // maps 1:1 into Responses function tools with one canonical `read` entry.
    // Skills are discoverable through tool_search, so the child wire catalog
    // carries no load_skill at all.
    let tools = crate::tools::subagent::kimi_general_child_request_tools_fixture();
    let mut request = minimal_responses_request();
    request.tools = Some(tools);
    let body = build_responses_body(&request);
    let serialized = body["tools"]
        .as_array()
        .expect("tools serialize as an array");
    let reads: Vec<_> = serialized
        .iter()
        .filter(|tool| tool["name"] == "read")
        .collect();
    assert_eq!(
        reads.len(),
        1,
        "exactly one canonical read definition reaches the Responses wire"
    );
    assert!(
        reads[0]["parameters"]["properties"].is_object(),
        "read keeps a valid parameters schema: {}",
        reads[0]
    );
    assert!(
        serialized.iter().all(|tool| tool["name"] != "load_skill"),
        "load_skill must not appear on the child Responses wire"
    );
}

#[tokio::test]
async fn responses_stream_open_preserves_wire_headers_through_shared_seam() {
    use wiremock::matchers::header;

    let server = MockServer::start().await;
    // Every wire-specific header (SSE accept, Responses beta opt-in,
    // originator, bearer auth from the default headers) must survive the
    // shared stream-entry open path; the mock only answers when all are
    // present.
    Mock::given(method("POST"))
        .and(path(CODEX_RESPONSES_PATH))
        .and(header("Accept", "text/event-stream"))
        .and(header("OpenAI-Beta", "responses=experimental"))
        .and(header("originator", "codex_cli_rs"))
        .and(header("Authorization", "Bearer test-token"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/event-stream")
                .set_body_string("data: [DONE]\n\n"),
        )
        .expect(1)
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let _codex_token =
            crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", "test-token");
        let _legacy_codex_token = crate::test_support::EnvVarGuard::remove("CODEX_ACCESS_TOKEN");
        DeepSeekClient::new(&test_codex_config(&server)).unwrap()
    };
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
        .expect("stream opens with preserved headers");

    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while let Some(event) = stream.next().await {
            event.unwrap();
        }
    })
    .await
    .expect("stream should finish after [DONE]");
}

#[tokio::test]
async fn responses_stream_inserts_boundary_between_reasoning_summary_parts() {
    let server = MockServer::start().await;
    let sse_body = concat!(
        "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\"}}\n\n",
        "data: {\"type\":\"response.reasoning_summary_part.added\",\"item_id\":\"rs_1\",\"summary_index\":0,\"part\":{\"type\":\"summary_text\",\"text\":\"\"}}\n\n",
        "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"partA\"}\n\n",
        "data: {\"type\":\"response.reasoning_summary_part.added\",\"item_id\":\"rs_1\",\"summary_index\":1,\"part\":{\"type\":\"summary_text\",\"text\":\"\"}}\n\n",
        "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"partB\"}\n\n",
        "data: {\"type\":\"response.output_item.done\"}\n\n",
        "data: [DONE]\n\n",
    );
    Mock::given(method("POST"))
        .and(path(CODEX_RESPONSES_PATH))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/event-stream")
                .set_body_string(sse_body),
        )
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let _codex_token =
            crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", "test-token");
        let _legacy_codex_token = crate::test_support::EnvVarGuard::remove("CODEX_ACCESS_TOKEN");
        DeepSeekClient::new(&test_codex_config(&server)).unwrap()
    };
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
        .unwrap();

    let mut thinking = String::new();
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while let Some(event) = stream.next().await {
            if let StreamEvent::ContentBlockDelta {
                delta: Delta::ThinkingDelta { thinking: chunk },
                ..
            } = event.unwrap()
            {
                thinking.push_str(&chunk);
            }
        }
    })
    .await
    .expect("Responses reasoning stream should finish after [DONE]");

    // The second summary part must be separated from the first by a
    // paragraph break, and no separator may precede the first part.
    assert_eq!(thinking, "partA\n\npartB");
}

#[test]
fn codex_reasoning_effort_uses_responses_labels() {
    assert_eq!(codex_responses_reasoning_effort("max"), Some("xhigh"));
    assert_eq!(codex_responses_reasoning_effort("maximum"), Some("xhigh"));
    assert_eq!(codex_responses_reasoning_effort("xhigh"), Some("xhigh"));
    assert_eq!(codex_responses_reasoning_effort("ultra"), Some("xhigh"));
    assert_eq!(codex_responses_reasoning_effort("ultracode"), Some("xhigh"));
    assert_eq!(codex_responses_reasoning_effort("high"), Some("high"));
    assert_eq!(codex_responses_reasoning_effort("medium"), Some("medium"));
    assert_eq!(codex_responses_reasoning_effort("minimal"), Some("low"));
    assert_eq!(codex_responses_reasoning_effort("auto"), Some("medium"));
    assert_eq!(codex_responses_reasoning_effort("off"), Some("low"));
}

/// Concentrate's parameter reference documents `model`, `input`, `stream`,
/// `max_output_tokens`, `tools`/`tool_choice`/`parallel_tool_calls`, and
/// `reasoning.effort`; `store`, `include`, `instructions`, and
/// `reasoning.summary` are absent. The body sends only documented fields and
/// carries the system prompt as a leading `system` input item.
/// https://concentrate.ai/docs/api-reference/endpoint/request-parameters
#[test]
fn concentrate_responses_body_sends_only_documented_fields() {
    let mut request = minimal_responses_request();
    request.model = "openai/gpt-5.6-sol".to_string();
    request.system = Some(SystemPrompt::Text(
        "You are the Codewhale test system prompt.".to_string(),
    ));
    request.reasoning_effort = Some("high".to_string());
    request.tools = Some(vec![Tool {
        tool_type: None,
        name: "read".to_string(),
        description: "Read a file".to_string(),
        input_schema: serde_json::json!({
            "type": "object",
            "properties": { "path": { "type": "string" } },
            "required": ["path"]
        }),
        allowed_callers: None,
        defer_loading: None,
        input_examples: None,
        strict: None,
        cache_control: None,
    }]);

    let body = build_responses_body_for_provider(
        &request,
        ApiProvider::Concentrate,
        ApiProvider::Concentrate.as_str(),
        "fp-concentrate-endpoint",
        BODY_TEST_BASE_URL,
    );
    let documented = [
        "model",
        "input",
        "max_output_tokens",
        "temperature",
        "top_p",
        "stream",
        "text",
        "reasoning",
        "tools",
        "tool_choice",
        "parallel_tool_calls",
        "routing",
        "cache_control",
        "prompt_cache_options",
    ];
    for key in body.as_object().expect("object body").keys() {
        assert!(
            documented.contains(&key.as_str()),
            "undocumented top-level field `{key}` reached the Concentrate wire: {body}"
        );
    }
    assert_eq!(
        body["model"], "openai/gpt-5.6-sol",
        "provider/model ids pass through verbatim"
    );
    assert_eq!(body["stream"], true);
    assert!(body.get("store").is_none(), "{body}");
    assert!(body.get("include").is_none(), "{body}");
    assert!(body.get("instructions").is_none(), "{body}");
    let input = body["input"].as_array().expect("input array");
    assert_eq!(input[0]["type"], "message");
    assert_eq!(input[0]["role"], "system");
    assert_eq!(input[0]["content"][0]["type"], "input_text");
    assert_eq!(
        input[0]["content"][0]["text"],
        "You are the Codewhale test system prompt."
    );
    assert_eq!(input[1]["role"], "user");
    assert_eq!(body["reasoning"], serde_json::json!({ "effort": "high" }));
    assert_eq!(body["tools"][0]["type"], "function");
    assert_eq!(body["tools"][0]["name"], "read");
    assert_eq!(body["tools"][0]["strict"], false);
    assert_eq!(body["tool_choice"], "auto");
    assert_eq!(body["parallel_tool_calls"], true);

    // The same request on the generic Responses path still carries the
    // OpenAI-only fields, so the Concentrate branch is a deliberate subset.
    let generic = build_responses_body_for_provider(
        &request,
        ApiProvider::Openai,
        ApiProvider::Openai.as_str(),
        "fp-generic-endpoint",
        BODY_TEST_BASE_URL,
    );
    assert!(
        generic.get("store").is_some()
            && generic.get("include").is_some()
            && generic.get("instructions").is_some()
    );
}

#[test]
fn deepseek_flash_responses_body_uses_stateless_0731_contract() {
    let mut request = minimal_responses_request();
    request.model = "deepseek-v4-flash".to_string();
    request.reasoning_effort = Some("xhigh".to_string());
    request.temperature = Some(1.0);
    request.top_p = Some(0.95);
    request.messages.insert(
        0,
        Message {
            role: Role::Assistant,
            content: vec![ContentBlock::Thinking {
                thinking: "preserve this tool-loop reasoning".to_string(),
                signature: None,
                state: None,
            }],
        },
    );

    let body = build_responses_body_for_provider(
        &request,
        ApiProvider::Deepseek,
        ApiProvider::Deepseek.as_str(),
        "fp-deepseek-endpoint",
        BODY_TEST_BASE_URL,
    );

    assert_eq!(body["model"], "deepseek-v4-flash");
    assert_eq!(body["max_output_tokens"], 128);
    assert_eq!(body["temperature"], 1.0);
    assert!(
        (body["top_p"].as_f64().expect("top_p number") - 0.95).abs() < 1e-6,
        "{}",
        body["top_p"]
    );
    assert_eq!(body.pointer("/reasoning/effort"), Some(&json!("max")));
    assert!(body.pointer("/reasoning/summary").is_none());
    assert!(body.get("include").is_none());
    assert!(body.get("store").is_none());
    assert_eq!(
        body.pointer("/input/0/content/0/type"),
        Some(&json!("reasoning_text"))
    );
    assert_eq!(
        body.pointer("/input/0/content/0/text"),
        Some(&json!("preserve this tool-loop reasoning"))
    );
}

#[test]
fn codex_responses_body_omits_the_output_cap_the_backend_rejects() {
    // The Codex OAuth Responses endpoint answers `max_output_tokens` with
    // "Unsupported parameter: max_output_tokens", which killed every
    // gpt-5.6-sol sub-agent turn. The omission must be route-specific:
    // other Responses providers keep the central cap on the wire.
    let mut request = minimal_responses_request();
    request.max_tokens = 4_096;

    let codex = build_responses_body_for_provider(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        "fp-codex-endpoint",
        OFFICIAL_CODEX_BASE_URL,
    );
    assert!(
        codex.get("max_output_tokens").is_none(),
        "Codex Responses body names a parameter its backend rejects: {codex}"
    );
    assert!(
        codex.get("max_tokens").is_none() && codex.get("max_completion_tokens").is_none(),
        "no alternate output-cap spelling may sneak onto the Codex wire: {codex}"
    );

    let deepseek = build_responses_body_for_provider(
        &request,
        ApiProvider::Deepseek,
        ApiProvider::Deepseek.as_str(),
        "fp-deepseek-endpoint",
        BODY_TEST_BASE_URL,
    );
    assert_eq!(deepseek["max_output_tokens"], json!(4_096));
}

#[test]
fn codex_replays_only_exact_model_opaque_reasoning_state() {
    const SENTINEL: &str = "readable private reasoning must not be replayed";
    const ENDPOINT_FP: &str = "fp-codex-endpoint";
    let state = OpaqueReasoningState {
        provider: ApiProvider::OpenaiCodex.as_str().to_string(),
        api: "openai-responses".to_string(),
        model: "gpt-5.5".to_string(),
        id: Some("rs_opaque".to_string()),
        encrypted_content: "enc_opaque_payload".to_string(),
        endpoint: Some(ENDPOINT_FP.to_string()),
    };
    let mut request = minimal_responses_request();
    request.messages.insert(
        0,
        Message {
            role: Role::Assistant,
            content: vec![ContentBlock::Thinking {
                thinking: SENTINEL.to_string(),
                signature: None,
                state: Some(state),
            }],
        },
    );

    let exact = build_responses_body_for_provider(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        ENDPOINT_FP,
        OFFICIAL_CODEX_BASE_URL,
    );
    let exact_wire = exact.to_string();
    assert!(!exact_wire.contains(SENTINEL), "{exact}");
    assert_eq!(exact.pointer("/input/0/type"), Some(&json!("reasoning")));
    assert_eq!(exact.pointer("/input/0/id"), Some(&json!("rs_opaque")));
    assert_eq!(exact.pointer("/input/0/summary"), Some(&json!([])));
    assert_eq!(
        exact.pointer("/input/0/encrypted_content"),
        Some(&json!("enc_opaque_payload"))
    );

    request.model = "gpt-5.6".to_string();
    let switched_model = build_responses_body_for_provider(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        ENDPOINT_FP,
        OFFICIAL_CODEX_BASE_URL,
    );
    assert!(!switched_model.to_string().contains(SENTINEL));
    assert!(
        switched_model
            .get("input")
            .and_then(Value::as_array)
            .is_some_and(|items| items.iter().all(|item| item["type"] != "reasoning")),
        "{switched_model}"
    );

    let switched_provider = build_responses_body_for_provider(
        &request,
        ApiProvider::Deepseek,
        ApiProvider::Deepseek.as_str(),
        ENDPOINT_FP,
        BODY_TEST_BASE_URL,
    );
    let switched_wire = switched_provider.to_string();
    assert!(!switched_wire.contains(SENTINEL), "{switched_provider}");
    assert!(
        !switched_wire.contains("enc_opaque_payload"),
        "{switched_provider}"
    );
}

#[tokio::test]
async fn codex_stream_captures_encrypted_reasoning_as_opaque_state() {
    let server = MockServer::start().await;
    let sse_body = concat!(
        "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\"}}\n\n",
        "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"visible summary\"}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[],\"encrypted_content\":\"enc_state\"}}\n\n",
        "data: [DONE]\n\n",
    );
    Mock::given(method("POST"))
        .and(path(CODEX_RESPONSES_PATH))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/event-stream")
                .set_body_string(sse_body),
        )
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let _codex_token =
            crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", "test-token");
        let _legacy_codex_token = crate::test_support::EnvVarGuard::remove("CODEX_ACCESS_TOKEN");
        DeepSeekClient::new(&test_codex_config(&server)).unwrap()
    };
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
        .unwrap();
    let mut captured = None;
    while let Some(event) = stream.next().await {
        if let StreamEvent::ContentBlockDelta {
            delta: Delta::ReasoningStateDelta { state },
            ..
        } = event.unwrap()
        {
            captured = Some(state);
        }
    }

    let state = captured.expect("encrypted reasoning state delta");
    assert_eq!(state.provider, ApiProvider::OpenaiCodex.as_str());
    assert_eq!(state.api, "openai-responses");
    assert_eq!(state.model, "gpt-5.5");
    assert_eq!(state.id.as_deref(), Some("rs_1"));
    assert_eq!(state.encrypted_content, "enc_state");
    assert_eq!(
        state.endpoint,
        Some(codewhale_config::catalog::base_url_fingerprint(
            &client.base_url
        )),
        "the captured state is bound to the capturing endpoint"
    );
}

#[test]
fn deepseek_responses_reasoning_effort_uses_documented_labels() {
    assert_eq!(responses_reasoning_effort("low", true), Some("low"));
    assert_eq!(responses_reasoning_effort("medium", true), Some("high"));
    assert_eq!(responses_reasoning_effort("high", true), Some("high"));
    assert_eq!(responses_reasoning_effort("xhigh", true), Some("max"));
    assert_eq!(responses_reasoning_effort("max", true), Some("max"));
    // The off tier must disable thinking on the wire, not collapse into
    // low: DeepSeek documents `reasoning.effort: "none"` as the off value.
    assert_eq!(responses_reasoning_effort("off", true), Some("none"));
    assert_eq!(responses_reasoning_effort("disabled", true), Some("none"));
    assert_eq!(responses_reasoning_effort("none", true), Some("none"));
    assert_eq!(responses_reasoning_effort("false", true), Some("none"));
    // minimal stays a low tier for DeepSeek (undocumented label preserved
    // for Codex compatibility).
    assert_eq!(responses_reasoning_effort("minimal", true), Some("low"));
}

#[test]
fn codex_responses_body_uses_responses_reasoning_not_deepseek_thinking() {
    let request = MessageRequest {
        model: "gpt-5.5".to_string(),
        messages: vec![Message {
            role: Role::User,
            content: vec![ContentBlock::Text {
                text: "hello".to_string(),
                cache_control: None,
            }],
        }],
        max_tokens: 128,
        system: None,
        tools: None,
        tool_choice: None,
        metadata: None,
        thinking: None,
        reasoning_effort: Some("max".to_string()),
        stream: None,
        temperature: None,
        top_p: None,
    };

    let body = build_responses_body(&request);

    assert_eq!(
        body.pointer("/reasoning/effort").and_then(Value::as_str),
        Some("xhigh")
    );
    assert_eq!(
        body.pointer("/reasoning/summary").and_then(Value::as_str),
        Some("auto")
    );
    assert!(body.get("thinking").is_none());
    assert!(body.get("reasoning_effort").is_none());
}

#[test]
fn responses_failed_event_reports_nested_error() {
    let event = json!({
        "type": "response.failed",
        "response": {
            "id": "resp_123",
            "error": {
                "code": "rate_limit_exceeded",
                "message": "Please retry later"
            }
        }
    });

    let (code, message) = responses_event_error_details(&event);

    assert_eq!(code, "rate_limit_exceeded");
    assert_eq!(message, "Please retry later");
}

#[test]
fn responses_incomplete_event_reports_reason() {
    let event = json!({
        "type": "response.incomplete",
        "response": {
            "id": "resp_123",
            "status": "incomplete",
            "error": null,
            "incomplete_details": {
                "reason": "content_filter"
            }
        }
    });

    let (code, message) = responses_event_error_details(&event);

    assert_eq!(code, "content_filter");
    assert_eq!(message, "response incomplete: content_filter");
}

#[test]
fn responses_incomplete_stop_reason_preserves_provider_reason() {
    assert_eq!(
        responses_stop_reason(
            &json!({
                "status": "incomplete",
                "incomplete_details": { "reason": "max_output_tokens" }
            }),
            false,
        ),
        "incomplete:max_output_tokens"
    );
    assert_eq!(
        responses_stop_reason(&json!({"status": "incomplete"}), false),
        "incomplete:max_tokens"
    );
}

#[test]
fn parse_responses_usage_derives_cache_miss_and_reasoning() {
    let usage = json!({
        "input_tokens": 1000,
        "output_tokens": 200,
        "input_tokens_details": { "cached_tokens": 600 },
        "output_tokens_details": { "reasoning_tokens": 120 }
    });

    let parsed = parse_responses_usage(&usage);

    assert_eq!(parsed.input_tokens, 1000);
    assert_eq!(parsed.output_tokens, 200);
    assert_eq!(parsed.prompt_cache_hit_tokens, Some(600));
    // Cache-miss is derived as input minus the cached hit when cached > 0.
    assert_eq!(parsed.prompt_cache_miss_tokens, Some(400));
    // Reasoning surfaces from output_tokens_details (Responses dialect).
    assert_eq!(parsed.reasoning_tokens, Some(120));

    // Without cached/reasoning details, the derived fields stay None.
    let bare = json!({ "input_tokens": 1000, "output_tokens": 200 });
    let parsed_bare = parse_responses_usage(&bare);
    assert_eq!(parsed_bare.prompt_cache_hit_tokens, None);
    assert_eq!(parsed_bare.prompt_cache_miss_tokens, None);
    assert_eq!(parsed_bare.reasoning_tokens, None);
}

#[test]
fn parse_responses_usage_saturates_u64_fields() {
    let parsed = parse_responses_usage(&json!({
        "input_tokens": u64::MAX,
        "output_tokens": u64::MAX,
        "input_tokens_details": { "cached_tokens": u64::MAX },
        "output_tokens_details": { "reasoning_tokens": u64::MAX }
    }));
    assert_eq!(parsed.input_tokens, u32::MAX);
    assert_eq!(parsed.output_tokens, u32::MAX);
    assert_eq!(parsed.prompt_cache_hit_tokens, Some(u32::MAX));
    assert_eq!(parsed.prompt_cache_miss_tokens, Some(0));
    assert_eq!(parsed.reasoning_tokens, Some(u32::MAX));
}

#[test]
fn parse_responses_usage_reads_deepseek_top_level_cache_fields() {
    // DeepSeek's Responses dialect reports cache telemetry as top-level
    // `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` with
    // `cache_write_tokens` nested under `input_tokens_details` -- none of
    // which the old parser read (it only looked at
    // `input_tokens_details.cached_tokens`, which DeepSeek leaves unset,
    // so every V4 Flash turn recorded cache_hit = None).
    let usage = json!({
        "input_tokens": 1_000,
        "output_tokens": 200,
        "prompt_cache_hit_tokens": 600,
        "prompt_cache_miss_tokens": 200,
        "input_tokens_details": { "cached_tokens": 999, "cache_write_tokens": 100 },
        "output_tokens_details": { "reasoning_tokens": 120 }
    });

    let parsed = parse_responses_usage(&usage);

    // Top-level DeepSeek fields win over the nested OpenAI-style shape,
    // and the explicit miss is trusted over the derived fallback.
    assert_eq!(parsed.prompt_cache_hit_tokens, Some(600));
    assert_eq!(parsed.prompt_cache_miss_tokens, Some(200));
    assert_eq!(parsed.prompt_cache_write_tokens, Some(100));
    // `input_tokens` remains the provider-reported total; the pricing
    // layer partitions it into hit / miss / write classes.
    assert_eq!(parsed.input_tokens, 1_000);
    assert_eq!(parsed.output_tokens, 200);
    assert_eq!(parsed.reasoning_tokens, Some(120));

    // The parsed fields must reach the pricing classes unchanged: 600 hit
    // at the cache-read rate, 100 write at the creation rate, and the
    // remaining 300 (200 reported miss + 100 uncategorized) at the miss
    // rate -- instead of the pre-fix all-raw-input miss billing.
    let classes = crate::pricing::token_usage_for_pricing(&parsed);
    assert_eq!(classes.input, 300);
    assert_eq!(classes.cache_read, 600);
    assert_eq!(classes.cache_write, 100);
}

#[test]
fn parse_responses_usage_keeps_old_shape_with_cache_write_fallback() {
    // OpenAI-style payloads still parse from `input_tokens_details` alone:
    // hit from `cached_tokens` (fallback), miss derived as input minus
    // hit, and the write class from `cache_write_tokens` when present.
    let usage = json!({
        "input_tokens": 1_000,
        "output_tokens": 200,
        "input_tokens_details": { "cached_tokens": 600, "cache_write_tokens": 100 }
    });

    let parsed = parse_responses_usage(&usage);

    assert_eq!(parsed.input_tokens, 1_000);
    assert_eq!(parsed.prompt_cache_hit_tokens, Some(600));
    assert_eq!(parsed.prompt_cache_miss_tokens, Some(400));
    assert_eq!(parsed.prompt_cache_write_tokens, Some(100));
    assert_eq!(parsed.reasoning_tokens, None);
}

/// Regression fixture for the reasoning double-billing bug: a real
/// Responses usage payload has to survive the whole way into the pricing
/// conversion without reasoning tokens being charged twice. OpenAI's
/// `output_tokens` is already the *total* billable completion count, with
/// `output_tokens_details.reasoning_tokens` a subset of it.
#[test]
fn responses_usage_reaches_pricing_conversion_without_double_billing_reasoning() {
    use crate::config::ApiProvider;
    use crate::pricing::{calculate_turn_cost_estimate_for_provider, token_usage_for_pricing};

    let usage = parse_responses_usage(&json!({
        "input_tokens": 10_000,
        "output_tokens": 4_000,
        "total_tokens": 14_000,
        "input_tokens_details": { "cached_tokens": 6_000 },
        "output_tokens_details": { "reasoning_tokens": 3_500 }
    }));

    let classes = token_usage_for_pricing(&usage);
    assert_eq!(classes.output, 4_000, "reasoning must not inflate output");
    assert_eq!(classes.input, 4_000);
    assert_eq!(classes.cache_read, 6_000);
    assert_eq!(classes.cache_write, 0);

    // gpt-5.5: 0.50 cache-read / 5.00 input / 30.00 output per million.
    let cost = calculate_turn_cost_estimate_for_provider(ApiProvider::Openai, "gpt-5.5", &usage)
        .expect("direct OpenAI route is priced");
    let expected = 0.006 * 0.50 + 0.004 * 5.00 + 0.004 * 30.00;
    assert!(
        (cost.usd - expected).abs() < 1e-12,
        "expected {expected}, got {}",
        cost.usd
    );

    // The bug charged the 3_500 reasoning tokens a second time at the
    // output rate; assert the difference explicitly so a reintroduction is
    // unambiguous rather than a silent number change.
    let double_billed = expected + 0.0035 * 30.00;
    assert!((cost.usd - double_billed).abs() > 1e-6);
}

#[test]
fn responses_input_includes_user_role_tool_results() {
    let request = MessageRequest {
        model: "gpt-5.5".to_string(),
        messages: vec![
            Message {
                role: Role::Assistant,
                content: vec![ContentBlock::ToolUse {
                    id: "call_abc|fc_123".to_string(),
                    name: "checklist_write".to_string(),
                    input: json!({"items": []}),
                    caller: None,
                    thought_signature: None,
                }],
            },
            Message {
                role: Role::User,
                content: vec![ContentBlock::ToolResult {
                    tool_use_id: "call_abc|fc_123".to_string(),
                    content: "<6 items>".to_string(),
                    is_error: None,
                    content_blocks: None,
                }],
            },
        ],
        max_tokens: 128,
        system: None,
        tools: None,
        tool_choice: None,
        metadata: None,
        thinking: None,
        reasoning_effort: None,
        stream: None,
        temperature: None,
        top_p: None,
    };

    let input = convert_messages_to_responses_input(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        "fp-codex-endpoint",
        OFFICIAL_CODEX_BASE_URL,
    );

    assert_eq!(input[0]["type"], "function_call");
    assert_eq!(input[0]["call_id"], "call_abc");
    assert_eq!(input[0]["name"], "checklist_write");
    assert_eq!(input[1]["type"], "function_call_output");
    assert_eq!(input[1]["call_id"], "call_abc");
    assert_eq!(input[1]["output"], "<6 items>");
}

#[test]
fn responses_input_encodes_tool_call_names() {
    let request = MessageRequest {
        model: "gpt-5.5".to_string(),
        messages: vec![Message {
            role: Role::Assistant,
            content: vec![ContentBlock::ToolUse {
                id: "call_abc|fc_123".to_string(),
                name: "web.run".to_string(),
                input: json!({}),
                caller: None,
                thought_signature: None,
            }],
        }],
        max_tokens: 128,
        system: None,
        tools: None,
        tool_choice: None,
        metadata: None,
        thinking: None,
        reasoning_effort: None,
        stream: None,
        temperature: None,
        top_p: None,
    };

    let input = convert_messages_to_responses_input(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        "fp-codex-endpoint",
        OFFICIAL_CODEX_BASE_URL,
    );

    assert_eq!(input[0]["type"], "function_call");
    assert_eq!(input[0]["name"], to_api_tool_name("web.run"));
}

#[test]
fn responses_function_tool_sanitizes_root_composition_schema() {
    let tool = Tool {
        tool_type: None,
        name: "web.run".to_string(),
        description: "Apply patch".to_string(),
        input_schema: json!({
            "type": "object",
            "properties": {
                "patch": {"type": "string"},
                "replace": {"type": "array"},
                "changes": {"type": "array"}
            },
            "oneOf": [
                {"required": ["patch"]},
                {"required": ["replace"]},
                {"required": ["changes"]}
            ]
        }),
        allowed_callers: None,
        defer_loading: None,
        input_examples: None,
        strict: None,
        cache_control: None,
    };

    let payload = tool_to_responses_function(&tool);
    let parameters = &payload["parameters"];

    assert_eq!(payload["name"], to_api_tool_name("web.run"));
    assert_eq!(parameters["type"], "object");
    assert!(parameters.get("oneOf").is_none());
    assert!(parameters.get("anyOf").is_none());
    assert!(parameters.get("allOf").is_none());
    assert!(parameters.get("enum").is_none());
    assert!(parameters.get("not").is_none());
    assert!(parameters["properties"].get("patch").is_some());
    assert!(parameters["properties"].get("replace").is_some());
    assert!(parameters["properties"].get("changes").is_some());
    assert_eq!(
        payload["description"],
        "Apply patch\n\nExactly one of these parameter groups must be provided: `changes` | `patch` | `replace`."
    );
    assert!(tool.input_schema.get("oneOf").is_some());
}

#[test]
fn responses_function_tool_trims_description_before_constraint_note() {
    let tool = Tool {
        tool_type: None,
        name: "apply_patch".to_string(),
        description: "Apply patch\n".to_string(),
        input_schema: json!({
            "type": "object",
            "properties": {
                "patch": {"type": "string"},
                "replace": {"type": "array"},
                "changes": {"type": "array"}
            },
            "oneOf": [
                {"required": ["patch"]},
                {"required": ["replace"]},
                {"required": ["changes"]}
            ]
        }),
        allowed_callers: None,
        defer_loading: None,
        input_examples: None,
        strict: None,
        cache_control: None,
    };

    let payload = tool_to_responses_function(&tool);

    assert_eq!(
        payload["description"],
        "Apply patch\n\nExactly one of these parameter groups must be provided: `changes` | `patch` | `replace`."
    );
}

#[test]
fn responses_function_tool_leaves_description_unchanged_without_constraint_note() {
    let tool = Tool {
        tool_type: None,
        name: "lookup".to_string(),
        description: "Lookup".to_string(),
        input_schema: json!({
            "type": "object",
            "properties": {
                "query": {"type": "string"}
            }
        }),
        allowed_callers: None,
        defer_loading: None,
        input_examples: None,
        strict: None,
        cache_control: None,
    };

    let payload = tool_to_responses_function(&tool);

    assert_eq!(payload["description"], "Lookup");
}

/// The Responses API projection of [`ContentBlock::ImageUrl`].
///
/// Responses is the odd one out: the image part carries `image_url` as a bare
/// string rather than the nested object Chat Completions uses. Getting that
/// wrong produces a schema error from OpenAI rather than anything that names
/// the image, so it is worth pinning explicitly.
#[test]
fn user_image_becomes_an_input_image_item() {
    const DATA_URL: &str = "data:image/png;base64,QUJD";

    let mut request = minimal_responses_request();
    request.messages[0].content.push(ContentBlock::ImageUrl {
        image_url: crate::models::ImageUrlContent {
            url: DATA_URL.to_string(),
        },
    });

    let items = convert_messages_to_responses_input(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        "fp-codex-endpoint",
        OFFICIAL_CODEX_BASE_URL,
    );

    let user = items
        .iter()
        .find(|item| item["role"] == "user")
        .expect("a user item");
    let content = user["content"].as_array().expect("content items");

    let image = content
        .iter()
        .find(|part| part["type"] == "input_image")
        .expect("an input_image part");
    assert_eq!(
        image["image_url"], DATA_URL,
        "Responses takes image_url as a bare string, not a nested object: {image}"
    );

    assert!(
        content.iter().any(|part| part["type"] == "input_text"),
        "the accompanying question must survive: {user}"
    );
}

#[test]
fn tool_result_image_becomes_native_function_output_content() {
    let mut request = minimal_responses_request();
    request.messages = vec![
        Message {
            role: Role::Assistant,
            content: vec![ContentBlock::ToolUse {
                id: "call_image_1".to_string(),
                name: "read".to_string(),
                input: serde_json::json!({"path": "shot.png"}),
                caller: None,
                thought_signature: None,
            }],
        },
        Message {
            role: Role::User,
            content: vec![ContentBlock::ToolResult {
                tool_use_id: "call_image_1".to_string(),
                content: "screenshot captured".to_string(),
                is_error: Some(false),
                content_blocks: Some(vec![serde_json::json!({
                    "type": "image",
                    "mime_type": "image/png",
                    "data": "QUJD",
                })]),
            }],
        },
    ];

    let items = convert_messages_to_responses_input(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        "fp-codex-endpoint",
        OFFICIAL_CODEX_BASE_URL,
    );
    let output = items
        .iter()
        .find(|item| item["type"] == "function_call_output")
        .expect("function output");
    let content = output["output"].as_array().expect("rich output array");

    assert_eq!(
        content[0],
        serde_json::json!({
            "type": "input_text",
            "text": "screenshot captured",
        })
    );
    assert_eq!(content[1]["type"], "input_image");
    assert_eq!(content[1]["image_url"], "data:image/png;base64,QUJD");
}

/// A `system`-role history message — the shape a compaction summary, a branch
/// summary, or an imported journal `system` entry takes once it reaches
/// `MessageRequest::messages` — must survive the Responses conversion. The
/// Chat Completions adapter already keeps it
/// (`request_builder_preserves_internal_system_messages`); dropping it here
/// silently deletes the only record of everything the compaction replaced.
#[test]
fn responses_input_keeps_system_role_history_messages() {
    let mut request = minimal_responses_request();
    request.messages.insert(
        0,
        Message {
            role: Role::System,
            content: vec![ContentBlock::Text {
                text: "[compaction summary] the user is porting the parser".to_string(),
                cache_control: None,
            }],
        },
    );

    let items = convert_messages_to_responses_input(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        "fp-codex-endpoint",
        OFFICIAL_CODEX_BASE_URL,
    );

    let system = items
        .iter()
        .find(|item| item["role"] == "system")
        .expect("system-role history message survives conversion");
    assert_eq!(system["type"], "message");
    assert_eq!(
        system["content"][0],
        serde_json::json!({
            "type": "input_text",
            "text": "[compaction summary] the user is porting the parser",
        })
    );
}

/// A `wire = "responses"` Custom table captures encrypted reasoning exactly
/// like the Codex backend: the stream yields an opaque reasoning-state delta
/// tagged with the Custom provider string, so the replay gate
/// (`state.provider == provider.as_str()`) matches on the next turn
/// (Pinvou PR #625).
#[tokio::test]
async fn forkguard_custom_responses_stream_captures_encrypted_reasoning_as_opaque_state() {
    let server = MockServer::start().await;
    let sse_body = concat!(
        "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_custom\"}}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_custom\",\"summary\":[],\"encrypted_content\":\"enc_custom_state\"}}\n\n",
        "data: [DONE]\n\n",
    );
    Mock::given(method("POST"))
        .and(path("/v1/responses"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/event-stream")
                .set_body_string(sse_body),
        )
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let config = crate::test_support::custom_named_table_config(
            "pinvou_responses",
            Some("responses"),
            &format!("{}/v1", server.uri()),
            "custom-responses-key",
            "gpt-6-sol",
        );
        DeepSeekClient::new(&config).expect("Custom responses client should resolve")
        // `DeepSeekClient::new` reads the table's `wire` dialect
        // (`provider_wire_format_for_config`), so this ambient client speaks
        // Responses; after the runtime-route fix the per-turn
        // `from_candidate` client carries the same wire.
    };
    assert_eq!(client.wire_format, WireFormat::Responses);
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
        .unwrap();
    let mut captured = None;
    while let Some(event) = stream.next().await {
        if let StreamEvent::ContentBlockDelta {
            delta: Delta::ReasoningStateDelta { state },
            ..
        } = event.unwrap()
        {
            captured = Some(state);
        }
    }

    let state = captured.expect("encrypted reasoning state delta on the Custom route");
    assert_eq!(
        state.provider, "custom/pinvou_responses",
        "the tag must carry the minting table, not the shared `custom` slug"
    );
    assert_eq!(state.api, "openai-responses");
    assert_eq!(state.id.as_deref(), Some("rs_custom"));
    assert_eq!(state.encrypted_content, "enc_custom_state");
    assert_eq!(
        state.model, "gpt-5.5",
        "the captured wire model is the replay gate's other key"
    );
    assert_eq!(
        state.endpoint,
        Some(codewhale_config::catalog::base_url_fingerprint(
            &client.base_url
        )),
        "the captured state is bound to the minting table's endpoint"
    );
}

/// A Chat-wire Custom table must not capture encrypted reasoning even if a
/// Responses-shaped stream reaches `handle_responses_stream`: the capture
/// gate keys on the transport's wire, not just on the stream's event shape.
#[tokio::test]
async fn forkguard_custom_chat_stream_does_not_capture_encrypted_reasoning() {
    let server = MockServer::start().await;
    let sse_body = concat!(
        "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_chat\"}}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_chat\",\"summary\":[],\"encrypted_content\":\"enc_chat_state\"}}\n\n",
        "data: [DONE]\n\n",
    );
    Mock::given(method("POST"))
        .and(path("/v1/chat/completions"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/event-stream")
                .set_body_string(sse_body),
        )
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        // No `wire`: the legacy table default is the Chat transport.
        let config = crate::test_support::custom_named_table_config(
            "pinvou_chat_table",
            None,
            &format!("{}/v1", server.uri()),
            "custom-chat-key",
            "vendor-model",
        );
        DeepSeekClient::new(&config).expect("Custom chat client should resolve")
    };
    assert_eq!(client.wire_format, WireFormat::ChatCompletions);
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("request prepares"),
        )
        .await
        .unwrap();
    let mut captured = None;
    while let Some(event) = stream.next().await {
        if let StreamEvent::ContentBlockDelta {
            delta: Delta::ReasoningStateDelta { state },
            ..
        } = event.unwrap()
        {
            captured = Some(state);
        }
    }
    assert!(
        captured.is_none(),
        "a Chat-wire Custom table must not mint opaque reasoning state: {captured:?}"
    );
}

/// A reasoning item without (or with an empty) `encrypted_content` must not
/// be captured — replaying an empty blob would poison every later turn —
/// but the stream itself keeps flowing and closes the block.
#[tokio::test]
async fn forkguard_custom_responses_capture_tolerates_missing_or_empty_encrypted_content() {
    let server = MockServer::start().await;
    let sse_body = concat!(
        "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_missing\"}}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_missing\",\"summary\":[]}}\n\n",
        "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_empty\"}}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_empty\",\"summary\":[],\"encrypted_content\":\"\"}}\n\n",
        "data: [DONE]\n\n",
    );
    Mock::given(method("POST"))
        .and(path("/v1/responses"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/event-stream")
                .set_body_string(sse_body),
        )
        .mount(&server)
        .await;

    let client = {
        let _env_lock = crate::test_support::lock_test_env();
        let config = crate::test_support::custom_named_table_config(
            "pinvou_responses",
            Some("responses"),
            &format!("{}/v1", server.uri()),
            "custom-responses-key",
            "gpt-6-sol",
        );
        DeepSeekClient::new(&config).expect("Custom responses client should resolve")
    };
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(minimal_responses_request(), true)
                .expect("responses request prepares"),
        )
        .await
        .unwrap();
    let mut captured = None;
    let mut blocks_closed = 0;
    while let Some(event) = stream.next().await {
        match event.unwrap() {
            StreamEvent::ContentBlockDelta {
                delta: Delta::ReasoningStateDelta { state },
                ..
            } => captured = Some(state),
            StreamEvent::ContentBlockStop { .. } => blocks_closed += 1,
            _ => {}
        }
    }
    assert!(
        captured.is_none(),
        "missing or empty encrypted_content must not be captured: {captured:?}"
    );
    assert_eq!(blocks_closed, 2, "both reasoning blocks still close");
}

/// The replay gate matches Custom-tagged reasoning state by endpoint-scoped
/// provider tag, endpoint fingerprint, and exact model: an exact
/// table+endpoint+model match replays the encrypted item, while a model
/// switch, a different table, a different provider, or an edited `base_url`
/// must not — table A's encrypted reasoning never rides to table B or to
/// whatever endpoint table A later points at. A pre-fingerprint state (no
/// endpoint field) carries no proof of origin, so on a Custom tag it fails
/// closed too: it must not ride a table's wire even at the table's current
/// URL, because the URL may not be the one that minted it. (Built-in tags
/// take the narrower official-endpoint rule, pinned by the two
/// legacy-state tests further down.)
#[test]
fn forkguard_custom_responses_replays_only_exact_model_opaque_reasoning_state() {
    const SENTINEL: &str = "readable private reasoning must not be replayed";
    const MINTING_TABLE: &str = "custom/pinvou_responses";
    const MINTING_ENDPOINT_FP: &str = "fp-pinvou-responses-endpoint";
    let state = OpaqueReasoningState {
        provider: MINTING_TABLE.to_string(),
        api: "openai-responses".to_string(),
        model: "gpt-6-sol".to_string(),
        id: Some("rs_custom".to_string()),
        encrypted_content: "enc_custom_payload".to_string(),
        endpoint: Some(MINTING_ENDPOINT_FP.to_string()),
    };
    let mut request = minimal_responses_request();
    request.model = "gpt-6-sol".to_string();
    request.messages.insert(
        0,
        Message {
            role: Role::Assistant,
            content: vec![ContentBlock::Thinking {
                thinking: SENTINEL.to_string(),
                signature: None,
                state: Some(state),
            }],
        },
    );

    let exact = build_responses_body_for_provider(
        &request,
        ApiProvider::Custom,
        MINTING_TABLE,
        MINTING_ENDPOINT_FP,
        BODY_TEST_BASE_URL,
    );
    let exact_wire = exact.to_string();
    assert!(!exact_wire.contains(SENTINEL), "{exact}");
    assert_eq!(exact.pointer("/input/0/type"), Some(&json!("reasoning")));
    assert_eq!(exact.pointer("/input/0/id"), Some(&json!("rs_custom")));
    assert_eq!(exact.pointer("/input/0/summary"), Some(&json!([])));
    assert_eq!(
        exact.pointer("/input/0/encrypted_content"),
        Some(&json!("enc_custom_payload"))
    );

    // Same table and model, but the table's base_url was edited: the tag
    // alone pins the table NAME, so the endpoint fingerprint must stop the
    // old endpoint's opaque blobs from riding to the new one.
    let switched_endpoint = build_responses_body_for_provider(
        &request,
        ApiProvider::Custom,
        MINTING_TABLE,
        "fp-new-endpoint-after-base-url-edit",
        BODY_TEST_BASE_URL,
    );
    assert!(
        !switched_endpoint.to_string().contains("enc_custom_payload"),
        "state must not replay onto a re-pointed endpoint: {switched_endpoint}"
    );
    assert!(
        switched_endpoint
            .get("input")
            .and_then(Value::as_array)
            .is_some_and(|items| items.iter().all(|item| item["type"] != "reasoning")),
        "{switched_endpoint}"
    );

    // States minted before endpoint fingerprints existed carry none and fail
    // closed on Custom: with no proof of which endpoint minted the blob, a
    // stable table tag proves nothing about the URL behind it, so the blob
    // must not ride the wire even at the table's current URL. The state
    // stays in history and is dropped on every turn until compaction gives
    // the session a clean slate; fresh captures carry fingerprints and
    // replay resumes from the next captured turn.
    request.messages[0].content = vec![ContentBlock::Thinking {
        thinking: SENTINEL.to_string(),
        signature: None,
        state: Some(OpaqueReasoningState {
            provider: MINTING_TABLE.to_string(),
            api: "openai-responses".to_string(),
            model: "gpt-6-sol".to_string(),
            id: Some("rs_legacy".to_string()),
            encrypted_content: "enc_legacy_payload".to_string(),
            endpoint: None,
        }),
    }];
    let legacy_state = build_responses_body_for_provider(
        &request,
        ApiProvider::Custom,
        MINTING_TABLE,
        MINTING_ENDPOINT_FP,
        BODY_TEST_BASE_URL,
    );
    let legacy_wire = legacy_state.to_string();
    assert!(!legacy_wire.contains(SENTINEL), "{legacy_state}");
    assert!(
        !legacy_wire.contains("enc_legacy_payload"),
        "fingerprint-less Custom state must fail closed, not replay: {legacy_state}"
    );
    assert!(
        legacy_state
            .get("input")
            .and_then(Value::as_array)
            .is_some_and(|items| items.iter().all(|item| item["type"] != "reasoning")),
        "{legacy_state}"
    );

    // The legacy-root custom table (identity "custom", tag "custom") is
    // endpoint-scoped Custom identity too and fails closed the same way.
    request.messages[0].content = vec![ContentBlock::Thinking {
        thinking: SENTINEL.to_string(),
        signature: None,
        state: Some(OpaqueReasoningState {
            provider: "custom".to_string(),
            api: "openai-responses".to_string(),
            model: "gpt-6-sol".to_string(),
            id: Some("rs_legacy_root".to_string()),
            encrypted_content: "enc_legacy_root_payload".to_string(),
            endpoint: None,
        }),
    }];
    let legacy_root = build_responses_body_for_provider(
        &request,
        ApiProvider::Custom,
        "custom",
        MINTING_ENDPOINT_FP,
        BODY_TEST_BASE_URL,
    );
    assert!(
        !legacy_root.to_string().contains("enc_legacy_root_payload"),
        "fingerprint-less legacy-root Custom state must fail closed: {legacy_root}"
    );

    // Same model on a DIFFERENT named table: the shared `custom` slug must
    // not match, so table A's opaque state never rides table B's wire.
    request.messages[0].content = vec![ContentBlock::Thinking {
        thinking: SENTINEL.to_string(),
        signature: None,
        state: Some(OpaqueReasoningState {
            provider: MINTING_TABLE.to_string(),
            api: "openai-responses".to_string(),
            model: "gpt-6-sol".to_string(),
            id: Some("rs_custom".to_string()),
            encrypted_content: "enc_custom_payload".to_string(),
            endpoint: Some(MINTING_ENDPOINT_FP.to_string()),
        }),
    }];
    let switched_table = build_responses_body_for_provider(
        &request,
        ApiProvider::Custom,
        "custom/other_relay",
        MINTING_ENDPOINT_FP,
        BODY_TEST_BASE_URL,
    );
    let switched_table_wire = switched_table.to_string();
    assert!(!switched_table_wire.contains(SENTINEL));
    assert!(
        !switched_table_wire.contains("enc_custom_payload")
            && !switched_table_wire.contains("enc_legacy_payload"),
        "cross-table replay must drop the foreign encrypted item: {switched_table}"
    );
    assert!(
        switched_table
            .get("input")
            .and_then(Value::as_array)
            .is_some_and(|items| items.iter().all(|item| item["type"] != "reasoning")),
        "{switched_table}"
    );

    // State minted by a DIFFERENT provider (Codex) must not replay on a
    // Custom route even with the same model and api shape: the tag is exact.
    request.messages[0].content = vec![ContentBlock::Thinking {
        thinking: SENTINEL.to_string(),
        signature: None,
        state: Some(OpaqueReasoningState {
            provider: ApiProvider::OpenaiCodex.as_str().to_string(),
            api: "openai-responses".to_string(),
            model: "gpt-6-sol".to_string(),
            id: Some("rs_codex".to_string()),
            encrypted_content: "enc_codex_payload".to_string(),
            endpoint: Some(MINTING_ENDPOINT_FP.to_string()),
        }),
    }];
    let codex_state_on_custom = build_responses_body_for_provider(
        &request,
        ApiProvider::Custom,
        MINTING_TABLE,
        MINTING_ENDPOINT_FP,
        BODY_TEST_BASE_URL,
    );
    let codex_state_wire = codex_state_on_custom.to_string();
    assert!(!codex_state_wire.contains(SENTINEL));
    assert!(
        !codex_state_wire.contains("enc_codex_payload"),
        "foreign-provider state must not replay onto a Custom route: {codex_state_on_custom}"
    );

    request.model = "gpt-6-luna".to_string();
    let switched_model = build_responses_body_for_provider(
        &request,
        ApiProvider::Custom,
        MINTING_TABLE,
        MINTING_ENDPOINT_FP,
        BODY_TEST_BASE_URL,
    );
    assert!(!switched_model.to_string().contains(SENTINEL));
    assert!(
        switched_model
            .get("input")
            .and_then(Value::as_array)
            .is_some_and(|items| items.iter().all(|item| item["type"] != "reasoning")),
        "{switched_model}"
    );
}

/// The legacy migration is fail-closed wherever the endpoint can move. A
/// fingerprint-less state from a built-in provider keeps replaying only on
/// that provider's official endpoint — the one origin the gate vouches for
/// without a fingerprint — so pre-upgrade sessions on the official route
/// keep their reasoning continuity.
#[test]
fn forkguard_fixed_endpoint_legacy_state_without_fingerprint_keeps_replaying() {
    const SENTINEL: &str = "readable private reasoning must not be replayed";
    const ENDPOINT_FP: &str = "fp-codex-endpoint";
    let mut request = minimal_responses_request();
    request.messages.insert(
        0,
        Message {
            role: Role::Assistant,
            content: vec![ContentBlock::Thinking {
                thinking: SENTINEL.to_string(),
                signature: None,
                state: Some(OpaqueReasoningState {
                    provider: ApiProvider::OpenaiCodex.as_str().to_string(),
                    api: "openai-responses".to_string(),
                    model: request.model.clone(),
                    id: Some("rs_legacy_codex".to_string()),
                    encrypted_content: "enc_legacy_codex_payload".to_string(),
                    endpoint: None,
                }),
            }],
        },
    );
    let body = build_responses_body_for_provider(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        ENDPOINT_FP,
        OFFICIAL_CODEX_BASE_URL,
    );
    assert_eq!(
        body.pointer("/input/0/type"),
        Some(&json!("reasoning")),
        "fingerprint-less fixed-endpoint state must keep replaying: {body}"
    );
    assert_eq!(
        body.pointer("/input/0/encrypted_content"),
        Some(&json!("enc_legacy_codex_payload"))
    );
}

/// The carve-out above must not extend to a re-pointed built-in: config and
/// env can move a built-in provider's base URL while its tag stays put
/// (`OPENAI_CODEX_BASE_URL`, a root/base_url override, a CLI flag), so a
/// fingerprint-less state carries no proof it came from the new endpoint.
/// The same tag that keeps replaying on the official route must fail closed
/// the moment the client points elsewhere — same rule, same direction as the
/// Custom fail-closed arm above.
#[test]
fn forkguard_repointed_builtin_legacy_state_without_fingerprint_fails_closed() {
    const SENTINEL: &str = "readable private reasoning must not be replayed";
    let mut request = minimal_responses_request();
    request.messages.insert(
        0,
        Message {
            role: Role::Assistant,
            content: vec![ContentBlock::Thinking {
                thinking: SENTINEL.to_string(),
                signature: None,
                state: Some(OpaqueReasoningState {
                    provider: ApiProvider::OpenaiCodex.as_str().to_string(),
                    api: "openai-responses".to_string(),
                    model: request.model.clone(),
                    id: Some("rs_legacy_codex".to_string()),
                    encrypted_content: "enc_legacy_codex_payload".to_string(),
                    endpoint: None,
                }),
            }],
        },
    );
    let body = build_responses_body_for_provider(
        &request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        "fp-codex-endpoint",
        "https://proxy.example.test/backend-api",
    );
    assert!(
        !body.to_string().contains("enc_legacy_codex_payload"),
        "a fingerprint-less built-in state must not replay onto a re-pointed endpoint: {body}"
    );
    assert!(
        body.get("input")
            .and_then(Value::as_array)
            .is_some_and(|items| items.iter().all(|item| item["type"] != "reasoning")),
        "{body}"
    );
}

/// Capture and replay must agree through the real per-turn client: the state
/// captured off turn 1's stream, placed back into history the way the turn
/// loop commits it (a Thinking block ahead of any tool call), reaches turn
/// 2's prepared request body as the paired reasoning item. The pure replay
/// test above pins the gate in isolation; this pins the seam where the
/// capture side's tag and endpoint fingerprint must equal the replay side's.
#[tokio::test]
async fn forkguard_custom_responses_captured_state_replays_on_the_next_turn() {
    let server = MockServer::start().await;
    let sse_body = concat!(
        "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_custom\"}}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_custom\",\"summary\":[],\"encrypted_content\":\"enc_custom_state\"}}\n\n",
        "data: [DONE]\n\n",
    );
    Mock::given(method("POST"))
        .and(path("/v1/responses"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/event-stream")
                .set_body_string(sse_body),
        )
        .expect(1)
        .mount(&server)
        .await;

    let _env_lock = crate::test_support::lock_test_env();
    let config = Config {
        provider: Some("pinvou_responses".to_string()),
        providers: Some(ProvidersConfig {
            custom: [(
                "pinvou_responses".to_string(),
                ProviderConfig {
                    kind: Some("openai-compatible".to_string()),
                    wire: Some("responses".to_string()),
                    base_url: Some(format!("{}/v1", server.uri())),
                    api_key: Some("custom-responses-key".to_string()),
                    model: Some("gpt-6-sol".to_string()),
                    ..ProviderConfig::default()
                },
            )]
            .into_iter()
            .collect(),
            ..ProvidersConfig::default()
        }),
        ..Config::default()
    };
    let route = crate::route_runtime::resolve_runtime_route(
        &config,
        ApiProvider::Custom,
        Some("gpt-6-sol"),
    )
    .expect("named table resolves");
    let client = DeepSeekClient::from_candidate(&route.config, &route.candidate)
        .expect("per-turn client builds");

    let mut first = minimal_responses_request();
    first.model = "gpt-6-sol".to_string();
    let mut stream = client
        .handle_responses_stream(
            &client
                .prepare_outbound_request(first, true)
                .expect("first request prepares"),
        )
        .await
        .unwrap();
    let mut captured = None;
    while let Some(event) = stream.next().await {
        if let StreamEvent::ContentBlockDelta {
            delta: Delta::ReasoningStateDelta { state },
            ..
        } = event.expect("first-turn stream event")
        {
            captured = Some(state);
        }
    }
    let state = captured.expect("state captured on turn 1");
    assert_eq!(state.provider, "custom/pinvou_responses");
    assert_eq!(state.encrypted_content, "enc_custom_state");

    // Turn loop placement: the Thinking block (state included) precedes any
    // tool call in the committed history.
    let mut follow_up = minimal_responses_request();
    follow_up.model = "gpt-6-sol".to_string();
    follow_up.messages.insert(
        0,
        Message {
            role: Role::Assistant,
            content: vec![ContentBlock::Thinking {
                thinking: String::new(),
                signature: None,
                state: Some(state),
            }],
        },
    );
    let prepared = client
        .prepare_outbound_request(follow_up, true)
        .expect("second request prepares");
    let second = &prepared.body;
    assert_eq!(
        second.pointer("/input/0/type"),
        Some(&serde_json::json!("reasoning")),
        "the reasoning item must lead the replayed input: {second}"
    );
    assert_eq!(
        second.pointer("/input/0/encrypted_content"),
        Some(&serde_json::json!("enc_custom_state")),
        "turn 2's wire carries the state captured on turn 1: {second}"
    );
    assert_eq!(
        second.pointer("/input/0/id"),
        Some(&serde_json::json!("rs_custom"))
    );
}
