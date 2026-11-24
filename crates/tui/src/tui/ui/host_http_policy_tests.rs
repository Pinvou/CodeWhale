//! Actual Fleet activation validation feeds the real readiness consumer.
use super::*;
use crate::provider_readiness::{
    ProviderReadinessSnapshot, ResolvedProviderReadiness, resolve_for_model,
};
use crate::test_support::{EnvVarGuard, lock_test_env};

#[derive(Debug)]
struct HostTransportDenied;
impl crate::config::ModelHttpPolicy for HostTransportDenied {
    fn configure(
        &self,
        _: reqwest::ClientBuilder,
        _: &str,
    ) -> anyhow::Result<reqwest::ClientBuilder> {
        anyhow::bail!("owned-fleet-private-error-canary")
    }
}

#[test]
fn forkguard_host_http_policy_fleet_activation_does_not_poison_credential_readiness() {
    let _env = lock_test_env();
    let home = tempfile::tempdir().unwrap();
    let _home = EnvVarGuard::set("CODEWHALE_HOME", home.path());
    let _backend = EnvVarGuard::remove("CODEWHALE_SECRET_BACKEND");
    let _cli_key = EnvVarGuard::remove("CODEWHALE_CLI_API_KEY");
    let _cli_source = EnvVarGuard::remove("CODEWHALE_CLI_API_KEY_SOURCE");
    let _legacy_source = EnvVarGuard::remove("DEEPSEEK_API_KEY_SOURCE");
    let _key = EnvVarGuard::remove("DEEPSEEK_API_KEY");
    let model = crate::config::DEFAULT_TEXT_MODEL;
    let mut config = Config {
        provider: Some("deepseek".into()),
        api_key: Some("owned-fixture-key".into()),
        base_url: Some("http://127.0.0.1:9/v1".into()),
        model_http_policy: Some(std::sync::Arc::new(HostTransportDenied)),
        ..Config::default()
    };
    let error = validate_fleet_route_activation(&config, ApiProvider::Deepseek, model)
        .expect_err("real activation must reject host transport setup");
    assert_eq!(error.category, ErrorCategory::State);
    assert_eq!(error.code, "model_transport_setup");
    assert!(!error.message.contains("owned-fleet-private-error-canary"));
    let mut health = ProviderReadinessSnapshot::default();
    health.record_success(&config, ApiProvider::Deepseek, model);
    health.record_failure(&config, ApiProvider::Deepseek, model, &error);
    assert!(
        matches!(
            resolve_for_model(&config, ApiProvider::Deepseek, model, &health),
            ResolvedProviderReadiness::Ready
        ),
        "local host failure must not replace known credential health"
    );

    config.model_http_policy = None;
    config.api_key = Some(String::new());
    // Local endpoints intentionally allow keyless use. An official hosted
    // route requires credentials, without sending a request during validation.
    config.base_url = Some("https://api.deepseek.com/v1".into());
    let error = validate_fleet_route_activation(&config, ApiProvider::Deepseek, model)
        .expect_err("real missing credentials still reject activation");
    assert_eq!(error.category, ErrorCategory::Authentication);
    assert_eq!(error.code, "route_validation_failed");
    assert_eq!(error.severity, ErrorSeverity::Error);
    assert!(!error.recoverable);
}
