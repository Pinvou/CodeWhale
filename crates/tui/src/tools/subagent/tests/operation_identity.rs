use super::*;
use crate::config::Config;
use wiremock::matchers::method;
use wiremock::{Mock, MockServer, ResponseTemplate};

#[tokio::test]
async fn forkguard_model_operation_child_retries_keep_id() {
    let server = MockServer::start().await;
    let calls = Arc::new(AtomicUsize::new(0));
    let observed = calls.clone();
    Mock::given(method("POST"))
        .respond_with(move |_: &wiremock::Request| {
            if observed.fetch_add(1, Ordering::SeqCst) == 0 {
                ResponseTemplate::new(503)
            } else {
                ResponseTemplate::new(200).set_body_json(serde_json::json!({
                    "choices":[{"message":{"role":"assistant","content":"done"}}]
                }))
            }
        })
        .mount(&server)
        .await;
    let mut runtime = stub_runtime();
    runtime.client = DeepSeekClient::new(&Config {
        provider: Some("deepseek".into()),
        api_key: Some("owned-test-key".into()),
        base_url: Some(server.uri()),
        default_text_model: Some("deepseek-v4-pro".into()),
        request_idempotency_header: Some("idempotency-key".into()),
        retry: Some(crate::config::RetryConfig {
            enabled: Some(false),
            max_retries: Some(0),
            initial_delay: Some(0.0),
            max_delay: Some(0.0),
            exponential_base: Some(1.0),
        }),
        ..Config::default()
    })
    .unwrap();
    runtime.model = "deepseek-v4-pro".into();
    let request = MessageRequest {
        model: runtime.model.clone(),
        messages: vec![Message {
            role: Role::User,
            content: vec![ContentBlock::Text {
                text: "same prompt".into(),
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
    };
    for _ in 0..2 {
        let response = tokio::time::timeout(
            Duration::from_secs(10),
            request_subagent_model_response_with_retries(
                &runtime,
                "owned-test-agent",
                0,
                2,
                request.clone(),
            ),
        )
        .await
        .expect("bounded agent retry")
        .expect("agent response");
        assert!(
            response
                .0
                .content
                .iter()
                .any(|block| matches!(block, ContentBlock::Text { text, .. } if text == "done"))
        );
    }
    let receipts = server.received_requests().await.unwrap();
    assert_eq!(receipts.len(), 3, "outer child retry and one new call");
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
    assert_eq!(ids[0], ids[1]);
    assert_ne!(ids[1], ids[2]);
    assert!(receipts.windows(2).all(|pair| pair[0].body == pair[1].body));
}
