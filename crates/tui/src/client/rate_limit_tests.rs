use super::*;

#[tokio::test]
async fn forkguard_rate_limit_native_and_raw_errors_reach_both_senders() {
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};
    for isolated in [false, true] {
        for escaped in [false, true] {
            for (status, raw, expected) in [
                (
                    200,
                    r#"{"base_resp":{"status_code":2045,"status_msg":"weekly plan quota exhausted fixture-config-secret"},"retry_after":7200}"#,
                    "window",
                ),
                (
                    429,
                    r#"{"base_resp":{"status_code":1002,"status_msg":"retry in 7200s fixture-config-secret"}}"#,
                    "window",
                ),
                (
                    429,
                    r#"{"error":{"message":"opaque fixture-config-secret"},"detail":"Billing quota has been exhausted"}"#,
                    "billing",
                ),
            ] {
                let server = MockServer::start().await;
                let secret = format!("fixture-secret-{}", uuid::Uuid::new_v4());
                let echoed = if escaped {
                    secret
                        .chars()
                        .map(|ch| format!("\\u{:04x}", u32::from(ch)))
                        .collect::<String>()
                } else {
                    secret.clone()
                };
                let raw = raw.replace("fixture-config-secret", &echoed);
                Mock::given(method("POST"))
                    .respond_with(
                        ResponseTemplate::new(status).set_body_raw(raw, "application/json"),
                    )
                    .expect(1)
                    .mount(&server)
                    .await;
                let mut client = DeepSeekClient::new(&Config {
                    provider: Some("minimax".into()),
                    providers: Some(crate::config::ProvidersConfig {
                        minimax: crate::config::ProviderConfig {
                            api_key: Some(secret.clone()),
                            base_url: Some(server.uri()),
                            ..Default::default()
                        },
                        ..Default::default()
                    }),
                    ..Default::default()
                })
                .unwrap();
                client.isolated_request_state = isolated;
                let error = client
                    .for_operation(uuid::Uuid::new_v4())
                    .send_json_with_retry(&server.uri(), &serde_json::json!({"fixture":true}))
                    .await
                    .unwrap_err();
                let typed = error.downcast_ref::<LlmError>().unwrap();
                assert!(!error.to_string().contains(&secret));
                assert!(!error.to_string().contains(&echoed));
                let diagnostic = if expected == "billing" {
                    "opaque"
                } else if status == 200 {
                    "weekly plan quota"
                } else {
                    "retry in 7200s"
                };
                assert!(error.to_string().contains(diagnostic));
                if expected == "window" {
                    assert!(
                        matches!(typed, LlmError::QuotaWindow { retry_after: Some(delay), .. } if *delay == Duration::from_secs(7200))
                    );
                    if !isolated {
                        assert!(
                            client.shared_quota.remaining().unwrap() > Duration::from_secs(7190)
                        );
                    }
                } else {
                    assert!(matches!(typed, LlmError::QuotaExhausted(_)));
                }
                assert_eq!(server.received_requests().await.unwrap().len(), 1);
            }
        }
    }
}

#[tokio::test(start_paused = true)]
async fn forkguard_rate_limit_local_tokens_consume_active_budget() {
    let client = DeepSeekClient::new(&Config {
        provider: Some("custom".into()),
        api_key: Some(uuid::Uuid::new_v4().to_string()),
        base_url: Some("https://fixture.example.test/v1".into()),
        ..Default::default()
    })
    .unwrap();
    *client.rate_limiter.lock().await = TokenBucket {
        enabled: true,
        capacity: 1.0,
        tokens: 0.0,
        refill_per_sec: 0.0001,
        normal_refill_per_sec: 0.0001,
        last_refill: Instant::now(),
    };
    assert!(
        crate::llm_client::rate_limit::active_timeout_total(
            Duration::from_secs(2),
            client.wait_for_rate_limit(),
        )
        .await
        .is_err()
    );
}

#[tokio::test]
async fn forkguard_rate_limit_unknown_quota_sender_and_billing_boundary() {
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, Request, ResponseTemplate};
    for (provider, message, expected_wires) in [
        ("custom", "TPM exceeded", 2),
        ("custom", "Credit balance exhausted", 1),
        ("openai", "TPM exceeded", 1),
    ] {
        let server = MockServer::start().await;
        let count = Arc::new(AtomicUsize::new(0));
        let seen = count.clone();
        let first_wire = Arc::new(std::sync::Mutex::new(None));
        let first = first_wire.clone();
        Mock::given(method("POST"))
            .respond_with(move |_: &Request| {
                if seen.fetch_add(1, Ordering::SeqCst) == 0 {
                    *first.lock().unwrap() = Some(Instant::now());
                    ResponseTemplate::new(429).insert_header("retry-after", "1")
                        .set_body_json(serde_json::json!({"error":{"type":"insufficient_quota","message":message}}))
                } else {
                    assert!(first.lock().unwrap().unwrap().elapsed() >= Duration::from_secs(1));
                    ResponseTemplate::new(200).set_body_string("ok")
                }
            })
            .mount(&server).await;
        let mut client = DeepSeekClient::new(&Config {
            provider: Some(provider.into()),
            api_key: Some(uuid::Uuid::new_v4().to_string()),
            base_url: Some(server.uri()),
            providers: Some(crate::config::ProvidersConfig {
                openai: crate::config::ProviderConfig {
                    api_key: Some(uuid::Uuid::new_v4().to_string()),
                    base_url: Some(server.uri()),
                    ..Default::default()
                },
                ..Default::default()
            }),
            ..Default::default()
        })
        .unwrap();
        client.retry.rate_limit.initial_delay_secs = 1;
        client.retry.rate_limit.max_delay_secs = 1;
        let result = client
            .for_operation(uuid::Uuid::new_v4())
            .send_json_with_retry(&server.uri(), &serde_json::json!({"fixture":true}))
            .await;
        assert_eq!(count.load(Ordering::SeqCst), expected_wires);
        if expected_wires == 2 {
            assert_eq!(result.unwrap().text().await.unwrap(), "ok");
        } else {
            assert!(matches!(
                result.unwrap_err().downcast_ref::<LlmError>(),
                Some(LlmError::QuotaExhausted(_))
            ));
        }
    }
}

#[test]
fn forkguard_rate_limit_identity_uses_effective_accounts_and_normalized_headers() {
    let _env = crate::test_support::lock_test_env();
    let home = tempfile::tempdir().unwrap();
    let _home = crate::test_support::EnvVarGuard::set("CODEWHALE_HOME", home.path());
    let token = crate::test_support::future_test_jwt(&uuid::Uuid::new_v4().to_string());
    let _token = crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCESS_TOKEN", token);
    let _account = crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCOUNT_ID", "account-a");
    let config = Config {
        provider: Some("openai-codex".into()),
        base_url: Some("https://relay.example.test/v1".into()),
        ..Default::default()
    };
    let first = DeepSeekClient::new(&config).unwrap();
    let _second_account =
        crate::test_support::EnvVarGuard::set("OPENAI_CODEX_ACCOUNT_ID", "account-b");
    let second = DeepSeekClient::new(&config).unwrap();
    assert_ne!(first.shared_quota.key, second.shared_quota.key);
    first.shared_quota.pause(Duration::from_secs(3600));
    assert!(second.shared_quota.remaining().is_none());

    let mut upper = config.clone();
    upper.http_headers = Some(HashMap::from([
        (" OpenAI-Project ".into(), " project-fixture ".into()),
        ("Authorization".into(), "ignored-auth-a".into()),
        ("X-Request-Id".into(), "trace-a".into()),
    ]));
    let mut lower = config;
    lower.http_headers = Some(HashMap::from([
        ("openai-project".into(), "project-fixture".into()),
        ("Authorization".into(), "ignored-auth-b".into()),
        ("X-Request-Id".into(), "trace-b".into()),
    ]));
    let upper = DeepSeekClient::new(&upper).unwrap();
    let lower = DeepSeekClient::new(&lower).unwrap();
    assert!(Arc::ptr_eq(&upper.shared_quota, &lower.shared_quota));
    upper.shared_quota.pause(Duration::from_secs(10));
    assert!(lower.shared_quota.remaining().is_some());
}

#[tokio::test]
async fn forkguard_rate_limit_restores_default_capacity_and_durable_deadlines() {
    let directory = tempfile::tempdir().unwrap();
    let key = uuid::Uuid::new_v4().to_string();
    let quota = shared_quota(key.clone(), Some(16), Some(directory.path().to_owned())).unwrap();
    quota.initialize().await.unwrap();
    quota.refused().await;
    assert_eq!(quota.concurrency.as_ref().unwrap().gate.capacity(), 8);
    for _ in 0..24 {
        quota.recovered().await;
    }
    assert_eq!(quota.concurrency.as_ref().unwrap().gate.capacity(), 16);
    quota.limiter.lock().await.enabled = false;
    quota.refused().await;
    for _ in 0..24 {
        quota.recovered().await;
    }
    assert_eq!(quota.concurrency.as_ref().unwrap().gate.capacity(), 16);

    quota.pause(Duration::from_secs(1000));
    quota.persist().await.unwrap();
    quota.pause(Duration::from_secs(3600));
    quota.persist().await.unwrap();
    drop(quota);
    forget_quota_process_state_for_restart(&key);
    let restored = shared_quota(key.clone(), Some(16), Some(directory.path().to_owned())).unwrap();
    restored.initialize().await.unwrap();
    assert!(restored.remaining().unwrap() > Duration::from_secs(3595));
    drop(restored);
    forget_quota_process_state_for_restart(&key);
    tokio::fs::write(
        directory.path().join(format!("{key}.json")),
        b"corrupt fixture",
    )
    .await
    .unwrap();
    let corrupt = shared_quota(key.clone(), Some(16), Some(directory.path().to_owned())).unwrap();
    assert!(corrupt.initialize().await.is_err());
    // Repairing the receipt permits the next manual call; a failed load is not cached forever.
    tokio::fs::write(
        directory.path().join(format!("{key}.json")),
        serde_json::to_vec(&(chrono::Utc::now() - chrono::Duration::seconds(1))).unwrap(),
    )
    .await
    .unwrap();
    corrupt.initialize().await.unwrap();
    assert!(corrupt.remaining().is_none());
    let blocked_directory = tempfile::NamedTempFile::new().unwrap();
    let blocked = shared_quota(
        uuid::Uuid::new_v4().to_string(),
        Some(1),
        Some(blocked_directory.path().to_owned()),
    )
    .unwrap();
    blocked.pause(Duration::from_secs(3600));
    assert!(blocked.persist().await.is_err());
    assert!(blocked.remaining().unwrap() > Duration::from_secs(3595));
}

#[tokio::test]
async fn forkguard_rate_limit_rechecks_cooldown_after_admission_queues() {
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, Request, ResponseTemplate};
    for queued_on_tokens in [false, true] {
        let server = MockServer::start().await;
        let wire_time = Arc::new(std::sync::Mutex::new(None));
        let receipt = wire_time.clone();
        Mock::given(method("POST"))
            .respond_with(move |_: &Request| {
                *receipt.lock().unwrap() = Some(Instant::now());
                ResponseTemplate::new(200).set_body_string("ok")
            })
            .mount(&server)
            .await;
        let mut client = DeepSeekClient::new(&Config {
            api_key: Some("owned-fixture".into()),
            base_url: Some(server.uri()),
            ..Default::default()
        })
        .unwrap();
        let quota = shared_quota(uuid::Uuid::new_v4().to_string(), Some(1), None).unwrap();
        client.shared_quota = quota.clone();
        client.request_concurrency = quota.concurrency.clone();
        client.rate_limiter = quota.limiter.clone();
        {
            let mut bucket = quota.limiter.lock().await;
            bucket.enabled = queued_on_tokens;
            bucket.tokens = 0.0;
            bucket.refill_per_sec = 10.0;
            bucket.last_refill = Instant::now();
        }
        let blocker = ProviderRequestPermit {
            lease: Arc::new(std::sync::Mutex::new(None)),
            limiter: quota.concurrency.as_ref().unwrap().clone(),
        };
        if !queued_on_tokens {
            blocker.resume().await;
        }
        let url = server.uri();
        let receipts = Arc::new(std::sync::Mutex::new(Vec::new()));
        let seen_receipts = receipts.clone();
        let observer: crate::llm_client::rate_limit::RecoveryObserver = Arc::new(move |receipt| {
            seen_receipts.lock().unwrap().push(receipt);
            Box::pin(async {})
        });
        let call = client.for_operation(uuid::Uuid::new_v4());
        let _lease = call.acquire_provider_request_permit().await;
        let body = serde_json::json!({"fixture": true});
        let sender = crate::llm_client::rate_limit::with_operation(
            Default::default(),
            crate::llm_client::rate_limit::with_observer(
                observer,
                call.send_json_with_retry(&url, &body),
            ),
        );
        tokio::pin!(sender);
        // Poll until the actual send path has reached the held slot or token
        // sleep before publishing the peer's new cooldown. No scheduler race.
        assert!(futures_util::poll!(sender.as_mut()).is_pending());
        assert!(wire_time.lock().unwrap().is_none());
        if queued_on_tokens {
            assert!(quota.limiter.lock().await.tokens < 0.0);
        } else {
            assert_eq!(quota.concurrency.as_ref().unwrap().active(), 1);
        }
        let not_before = Instant::now() + Duration::from_millis(250);
        quota.pause(Duration::from_millis(250));
        blocker.suspend();
        tokio::time::timeout(Duration::from_secs(10), sender)
            .await
            .unwrap()
            .unwrap();
        assert!(
            wire_time.lock().unwrap().unwrap() >= not_before,
            "a queued request must obey a peer's new cooldown"
        );
        let receipts = receipts.lock().unwrap();
        assert!(
            receipts
                .iter()
                .any(|r| r.state == "waiting" && r.attempt == 0)
        );
        assert!(
            receipts
                .iter()
                .any(|r| r.state == "recovered" && r.attempt == 1)
        );
    }
}

#[tokio::test]
async fn forkguard_rate_limit_scope_admission_and_leases() {
    let key = uuid::Uuid::new_v4().to_string();
    let first = shared_quota(key.clone(), Some(4), None).unwrap();
    let second = shared_quota(key, Some(2), None).unwrap();
    let other = shared_quota(uuid::Uuid::new_v4().to_string(), Some(4), None).unwrap();
    assert!(Arc::ptr_eq(&first, &second));
    assert!(!Arc::ptr_eq(&first, &other));
    first.pause(Duration::from_secs(3600));
    assert!(second.remaining().unwrap() > Duration::from_secs(3598));
    assert!(other.remaining().is_none());
    let limiter = first.concurrency.as_ref().unwrap();
    assert_eq!(limiter.limit(), 2);
    let lease = ProviderRequestPermit {
        lease: Arc::new(std::sync::Mutex::new(None)),
        limiter: limiter.clone(),
    };
    lease.resume().await;
    assert_eq!(limiter.active(), 1);
    lease.suspend();
    assert_eq!(limiter.active(), 0);
    first.refused().await;
    assert_eq!(limiter.gate.capacity(), 1);
    let policy = crate::config::RateLimitRetryPolicy {
        max_attempts: 3,
        ..Default::default()
    };
    assert!(first.admit_recovery(&policy).is_none());
    assert!(second.admit_recovery(&policy).is_none());
    assert!(first.admit_recovery(&policy).is_some());
    assert!(other.admit_recovery(&policy).is_none());
    for _ in 0..3 {
        first.recovered().await;
    }
    assert_eq!(limiter.gate.capacity(), 2);
    lease.resume().await;
    assert_eq!(limiter.active(), 1);
    drop(lease);
    assert_eq!(limiter.active(), 0);
}

#[tokio::test]
async fn forkguard_rate_limit_isolated_classifier_normalizes_native_json_refusal() {
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .respond_with(ResponseTemplate::new(200).set_body_json(
            serde_json::json!({"base_resp":{"status_code":1002,"status_msg":"owned-fixture"}}),
        ))
        .expect(1)
        .mount(&server)
        .await;
    let mut client = DeepSeekClient::new(&Config {
        provider: Some("minimax".into()),
        providers: Some(crate::config::ProvidersConfig {
            minimax: crate::config::ProviderConfig {
                api_key: Some("owned-fixture".into()),
                base_url: Some(server.uri()),
                ..Default::default()
            },
            ..Default::default()
        }),
        ..Default::default()
    })
    .unwrap();
    client.retry.enabled = false;
    let error = client
        .send_with_isolated_retry(|| client.http_client.post(server.uri()))
        .await
        .unwrap_err();
    assert!(matches!(
        error.downcast_ref::<LlmError>(),
        Some(LlmError::RateLimited { .. })
    ));
    assert!(!error.to_string().contains("owned-fixture"));
}

// The environment lock must cover construction and wire dispatch: this is an
// exact-credential boundary under source-marked process-wide CLI overrides.
#[allow(clippy::await_holding_lock)]
#[tokio::test(flavor = "current_thread")]
async fn forkguard_rate_limit_auxiliary_route_keeps_its_explicit_credential() {
    use wiremock::matchers::{header, method};
    use wiremock::{Mock, MockServer, ResponseTemplate};
    let _env = crate::test_support::lock_test_env();
    let _key =
        crate::test_support::EnvVarGuard::set("CODEWHALE_CLI_API_KEY", "main-cli-fixture-key");
    let _source = crate::test_support::EnvVarGuard::set("CODEWHALE_CLI_API_KEY_SOURCE", "cli");
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(header("authorization", "Bearer independent-vision-fixture"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(serde_json::json!({"fixture": true})),
        )
        .expect(1)
        .mount(&server)
        .await;
    let route = DeepSeekClient::auxiliary_json_route(
        None,
        &server.uri(),
        "independent-vision-fixture",
        "vision-model",
    )
    .unwrap();
    assert_eq!(route.api_key, "independent-vision-fixture");
    route
        .request_auxiliary_json(&server.uri(), &serde_json::json!({}), uuid::Uuid::new_v4())
        .await
        .unwrap();
}

#[tokio::test]
async fn forkguard_rate_limit_auxiliary_apis_publish_distinct_runtime_identities() {
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, Request, ResponseTemplate};
    let server = MockServer::start().await;
    let calls = Arc::new(AtomicUsize::new(0));
    let seen_calls = calls.clone();
    Mock::given(method("POST")).respond_with(move |_: &Request| {
        if seen_calls.fetch_add(1, Ordering::SeqCst) % 2 == 0 {
            ResponseTemplate::new(429).insert_header("retry-after", "0")
        } else {
            ResponseTemplate::new(200).set_body_json(serde_json::json!({"choices":[{"message":{"content":"translated"},"text":"completed"}]}))
        }
    }).mount(&server).await;
    let mut policy = crate::config::RateLimitRetryPolicy::default();
    policy.initial_delay_secs = 1;
    policy.max_delay_secs = 1;
    let client = DeepSeekClient::new(&Config {
        api_key: Some("owned-auxiliary-key".into()),
        base_url: Some(server.uri()),
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
    let receipts = Arc::new(std::sync::Mutex::new(Vec::new()));
    let seen = receipts.clone();
    let observer: crate::llm_client::rate_limit::RecoveryObserver = Arc::new(move |receipt| {
        let seen = seen.clone();
        Box::pin(async move {
            seen.lock().unwrap().push(receipt);
        })
    });
    crate::llm_client::rate_limit::with_observer(observer, async {
        assert_eq!(
            client
                .translate("fixture", "deepseek-chat", "English")
                .await
                .unwrap(),
            "translated"
        );
        assert_eq!(
            client
                .fim_completion("deepseek-chat", "fixture", "suffix", 16)
                .await
                .unwrap(),
            "completed"
        );
    })
    .await;
    let receipts = receipts.lock().unwrap();
    let ids: std::collections::BTreeSet<_> = receipts
        .iter()
        .map(|r| r.operation_id.clone().unwrap())
        .collect();
    assert_eq!(ids.len(), 2);
    for id in ids {
        let states: Vec<_> = receipts
            .iter()
            .filter(|r| r.operation_id.as_deref() == Some(id.as_str()))
            .map(|r| (r.state.as_str(), r.attempt))
            .collect();
        assert_eq!(states, [("waiting", 1), ("recovered", 2)]);
    }
    assert_eq!(calls.load(Ordering::SeqCst), 4);
}

// Simulate a process boundary without bypassing the production file reader.
fn forget_quota_process_state_for_restart(key: &str) {
    let mut registry = quota_registry().lock().unwrap();
    assert_eq!(
        Arc::strong_count(registry.get(key).unwrap()),
        1,
        "no live client may be forgotten"
    );
    registry.remove(key);
}

#[tokio::test]
async fn forkguard_rate_limit_episode_survives_last_auxiliary_client_drop() {
    let key = uuid::Uuid::new_v4().to_string();
    let policy = crate::config::RateLimitRetryPolicy {
        max_attempts: 2,
        ..Default::default()
    };
    let first = shared_quota(key.clone(), Some(16), None).unwrap();
    first.refused().await;
    assert!(first.admit_recovery(&policy).is_none());
    assert!(first.admit_recovery(&policy).is_some());
    drop(first);
    let next = shared_quota(key, Some(16), None).unwrap();
    assert!(
        next.admit_recovery(&policy).is_some(),
        "client turnover cannot reset the process recovery episode"
    );
    assert_eq!(next.concurrency.as_ref().unwrap().gate.capacity(), 8);
    next.recovered().await;
    assert!(
        next.admit_recovery(&policy).is_none(),
        "a real success ends the shared refusal episode"
    );
    let isolated = shared_quota(uuid::Uuid::new_v4().to_string(), Some(16), None).unwrap();
    assert_eq!(isolated.concurrency.as_ref().unwrap().gate.capacity(), 16);
}

#[tokio::test]
async fn forkguard_rate_limit_isolated_unknown_quota_uses_route_classification() {
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};
    let server = MockServer::start().await;
    Mock::given(method("POST")).respond_with(ResponseTemplate::new(429).set_body_json(
        serde_json::json!({"error":{"code":null,"type":"insufficient_quota","message":"TPM exceeded"}})
    )).expect(1).mount(&server).await;
    let mut client = DeepSeekClient::new(&Config {
        provider: Some("custom".into()),
        api_key: Some(uuid::Uuid::new_v4().to_string()),
        base_url: Some(server.uri()),
        ..Default::default()
    })
    .unwrap();
    client.retry.enabled = false;
    let error = client
        .send_with_isolated_retry(|| client.http_client.post(server.uri()))
        .await
        .unwrap_err();
    assert!(matches!(
        error.downcast_ref::<LlmError>(),
        Some(LlmError::RateLimited { .. })
    ));
}
