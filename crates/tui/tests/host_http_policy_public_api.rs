//! Compile the documented interface as a real external crate consumer.
use codewhale_tui::config::{ModelHttpPolicy, VisionModelConfig};
use codewhale_tui::tools::{ToolContext, ToolRegistryBuilder};
use std::sync::{
    Arc,
    atomic::{AtomicUsize, Ordering},
};

#[derive(Debug, Default)]
struct HostPolicy(AtomicUsize);
impl ModelHttpPolicy for HostPolicy {
    fn configure(
        &self,
        builder: reqwest::ClientBuilder,
        _: &str,
    ) -> anyhow::Result<reqwest::ClientBuilder> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Ok(builder.no_proxy())
    }
}

#[test]
fn forkguard_host_http_policy_public_registry_accepts_original_three_field_literal() {
    let policy = Arc::new(HostPolicy::default());
    let workspace = tempfile::tempdir().unwrap();
    let registry = ToolRegistryBuilder::new()
        .with_vision_http_policy(Some(policy.clone()))
        .with_vision_tools(
            VisionModelConfig {
                model: "owned-vision".into(),
                api_key: None,
                base_url: Some("http://127.0.0.1:9/v1".into()),
            },
            None,
        )
        .build(ToolContext::new(workspace.path()));
    assert!(registry.get("image_analyze").is_some());
    assert_eq!(policy.0.load(Ordering::SeqCst), 1);
}
