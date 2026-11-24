use super::*;
use std::sync::Arc;

#[derive(Debug)]
struct IdentityPolicy;

impl ModelHttpPolicy for IdentityPolicy {
    fn configure(
        &self,
        builder: reqwest::ClientBuilder,
        _base_url: &str,
    ) -> Result<reqwest::ClientBuilder> {
        Ok(builder)
    }
}

#[test]
fn forkguard_host_http_policy_clone_merge_and_vision_identity() {
    let base: Arc<dyn ModelHttpPolicy> = Arc::new(IdentityPolicy);
    let override_policy: Arc<dyn ModelHttpPolicy> = Arc::new(IdentityPolicy);
    let configured = Config {
        model_http_policy: Some(base.clone()),
        vision_model: Some(VisionModelConfig {
            model: "fixture-vision".into(),
            api_key: None,
            base_url: Some("http://127.0.0.1:9/v1".into()),
        }),
        ..Config::default()
    };
    assert!(Arc::ptr_eq(
        configured.clone().model_http_policy.as_ref().unwrap(),
        &base
    ));
    assert!(Arc::ptr_eq(
        configured.vision_http_policy().as_ref().unwrap(),
        &base
    ));
    let mut explicit_vision = configured.clone();
    explicit_vision.vision_model_http_policy = Some(override_policy.clone());
    assert!(Arc::ptr_eq(
        explicit_vision.vision_http_policy().as_ref().unwrap(),
        &override_policy
    ));
    for (base_policy, explicit, expected) in [
        (None, None, None),
        (Some(base.clone()), None, Some(base.clone())),
        (
            None,
            Some(override_policy.clone()),
            Some(override_policy.clone()),
        ),
        (
            Some(base),
            Some(override_policy.clone()),
            Some(override_policy),
        ),
    ] {
        for vision in [false, true] {
            let merged = merge_config(
                Config {
                    model_http_policy: (!vision).then(|| base_policy.clone()).flatten(),
                    vision_model_http_policy: vision.then(|| base_policy.clone()).flatten(),
                    ..Config::default()
                },
                Config {
                    model_http_policy: (!vision).then(|| explicit.clone()).flatten(),
                    vision_model_http_policy: vision.then(|| explicit.clone()).flatten(),
                    ..Config::default()
                },
            );
            match (
                if vision {
                    merged.vision_model_http_policy
                } else {
                    merged.model_http_policy
                },
                expected.clone(),
            ) {
                (Some(actual), Some(expected)) => assert!(Arc::ptr_eq(&actual, &expected)),
                (None, None) => (),
                _ => panic!("host policy merge changed identity or optional baseline"),
            }
        }
    }
}

#[test]
fn forkguard_host_http_policy_cannot_be_supplied_by_toml() {
    let config: Config = toml::from_str(
        "model_http_policy = { disable = true }\nvision_model_http_policy = { disable = true }\n[vision_model]\nmodel = 'fixture-vision'\nmodel_http_policy = { disable = true }\n",
    ).unwrap();
    assert!(config.model_http_policy.is_none());
    assert!(config.vision_model_http_policy.is_none());
    let vision: VisionModelConfig = serde_json::from_value(serde_json::json!({
        "model": "fixture-vision", "model_http_policy": { "disable": true }
    }))
    .unwrap();
    assert_eq!(vision.model, "fixture-vision");
}
