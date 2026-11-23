//! Real inference-client builders, including the fallback and standalone vision.
use super::*;
use crate::config::{ModelHttpPolicy, VisionModelConfig};
use crate::tools::spec::{ToolContext, ToolSpec};
use crate::vision::tools::ImageAnalyzeTool;
use std::net::SocketAddr;
use std::sync::{
    Arc,
    atomic::{AtomicUsize, Ordering},
};
use wiremock::{
    Mock, MockServer, ResponseTemplate,
    matchers::{header, method},
};

#[derive(Debug)]
struct FixturePolicy {
    address: SocketAddr,
    calls: AtomicUsize,
}

impl ModelHttpPolicy for FixturePolicy {
    fn configure(
        &self,
        builder: reqwest::ClientBuilder,
        base_url: &str,
    ) -> Result<reqwest::ClientBuilder> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        anyhow::ensure!(
            reqwest::Url::parse(base_url)?.host_str() == Some("localhost"),
            "unprepared route"
        );
        let mut headers = reqwest::header::HeaderMap::new();
        headers.insert(
            "x-host-policy",
            reqwest::header::HeaderValue::from_static("applied"),
        );
        Ok(builder
            .default_headers(headers)
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .resolve_to_addrs("localhost", &[self.address]))
    }
}

#[tokio::test]
async fn forkguard_host_http_policy_reaches_primary_fallback_and_vision() {
    let server = MockServer::start().await;
    // The primary route dials twice (h2 + http1 twins) under its own key;
    // the independent vision route dials once under the vision key.
    let vision_response = ResponseTemplate::new(200).set_body_json(serde_json::json!(
        {"choices":[{"message":{"content":"owned fixture"}}]}
    ));
    Mock::given(method("POST"))
        .and(header("x-host-policy", "applied"))
        .and(header("authorization", "Bearer owned-fixture-key"))
        .respond_with(vision_response.clone())
        .expect(2)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(header("x-host-policy", "applied"))
        .and(header("authorization", "Bearer owned-vision-key"))
        .respond_with(vision_response)
        .expect(1)
        .mount(&server)
        .await;
    let address: SocketAddr = server.uri().trim_start_matches("http://").parse().unwrap();
    let base = format!("http://localhost:{}/v1", address.port());
    let policy = Arc::new(FixturePolicy {
        address,
        calls: AtomicUsize::new(0),
    });
    let config = Config {
        provider: Some("deepseek".into()),
        api_key: Some("owned-fixture-key".into()),
        base_url: Some(base.clone()),
        default_text_model: Some("deepseek-v4-pro".into()),
        model_http_policy: Some(policy.clone()),
        vision_model_http_policy: Some(policy.clone()),
        vision_model: Some(VisionModelConfig {
            model: "owned-vision-model".into(),
            // A distinct key keeps the auxiliary route OFF the parent-reuse
            // fast path: an independent vision credential is a fresh client
            // birth, which is where the vision policy must consult.
            api_key: Some("owned-vision-key".into()),
            base_url: Some(base.clone()),
        }),
        ..Config::default()
    };
    let client = DeepSeekClient::new(&config).unwrap();
    assert_eq!(policy.calls.load(Ordering::SeqCst), 2);
    for transport in [&client.http_client, client.http1_fallback_client()] {
        transport
            .post(format!("{base}/chat/completions"))
            .body("owned fixture")
            .send()
            .await
            .unwrap()
            .error_for_status()
            .unwrap();
    }
    let vision = ImageAnalyzeTool::new_with_route_client(
        config.clone().vision_model_config().unwrap(),
        Some(client.clone()),
    );
    let workspace = tempfile::tempdir().unwrap();
    std::fs::write(workspace.path().join("sample.png"), b"owned image fixture").unwrap();
    vision
        .execute(
            serde_json::json!({"image_path":"sample.png"}),
            &ToolContext::new(workspace.path().to_path_buf()),
        )
        .await
        .unwrap();
    // The vision dial's auxiliary client birth consults the policy for both
    // transports (h2 + http1 twins), like the primary build does: 2 + 2.
    assert_eq!(policy.calls.load(Ordering::SeqCst), 4);
    assert_eq!(server.received_requests().await.unwrap().len(), 3);
}

#[tokio::test]
async fn forkguard_host_http_policy_construction_denial_fails_closed() {
    let untrusted: Config =
        serde_json::from_value(serde_json::json!({"model_http_policy": {"disable": true}}))
            .unwrap();
    assert!(untrusted.model_http_policy.is_none());
    let policy = Arc::new(FixturePolicy {
        address: "127.0.0.1:9".parse().unwrap(),
        calls: AtomicUsize::new(0),
    });
    let config = Config {
        provider: Some("deepseek".into()),
        api_key: Some("owned-fixture-key".into()),
        base_url: Some("https://unprepared.invalid/v1".into()),
        model_http_policy: Some(policy.clone()),
        ..Config::default()
    };
    assert!(DeepSeekClient::new(&config).is_err());
    assert_eq!(policy.calls.load(Ordering::SeqCst), 1);
    // PR #151 on the post-refactor transport: an independent vision route is
    // a fresh auxiliary build, and that birth is where the vision policy is
    // consulted — one more configure call, then the dial fails closed.
    let parent_config = Config {
        provider: Some("deepseek".into()),
        api_key: Some("owned-fixture-key".into()),
        base_url: Some("https://primary.invalid/v1".into()),
        ..Config::default()
    };
    let parent = DeepSeekClient::new(&parent_config).unwrap();
    let route = DeepSeekClient::auxiliary_json_route_with_policy(
        Some(&parent),
        "https://vision-endpoint.invalid/v1",
        "owned-fixture-key",
        "owned-fixture",
        Some(policy.clone()),
    );
    assert!(route.is_err());
    assert_eq!(policy.calls.load(Ordering::SeqCst), 2);
}
