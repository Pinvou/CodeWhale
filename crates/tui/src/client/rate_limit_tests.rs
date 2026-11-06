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

async fn hold_http_response(
    reply: &'static [u8],
) -> (String, Arc<AtomicUsize>, tokio::task::JoinHandle<()>) {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let attempts = Arc::new(AtomicUsize::new(0));
    let observed = attempts.clone();
    let fixture = tokio::spawn(async move {
        let mut connections = tokio::task::JoinSet::new();
        while let Ok((mut socket, _)) = listener.accept().await {
            let observed = observed.clone();
            connections.spawn(async move {
                let mut request = [0; 4096];
                if socket.read(&mut request).await.unwrap_or(0) == 0 {
                    return;
                }
                observed.fetch_add(1, Ordering::Relaxed);
                let _ = socket.write_all(reply).await;
                std::future::pending::<()>().await;
                drop(socket);
            });
        }
    });
    (url, attempts, fixture)
}

#[tokio::test]
async fn forkguard_rate_limit_stalled_error_body_preserves_the_retry_after() {
    use super::stream_entry::{
        StreamHttpPolicy, StreamOpenRequest, client_for_policy, open_sse_response,
    };

    let (url, attempts, fixture) = hold_http_response(
        b"HTTP/1.1 429 Too Many Requests\r\nRetry-After: 7200\r\nContent-Length: 128\r\n\r\n",
    )
    .await;
    let state = tempfile::tempdir().unwrap();
    let mut client = DeepSeekClient::new(&Config {
        api_key: Some(format!("owned-fixture-{}", uuid::Uuid::new_v4())),
        base_url: Some(url.clone()),
        ..Default::default()
    })
    .unwrap();
    client.shared_quota = shared_quota(
        uuid::Uuid::new_v4().to_string(),
        None,
        Some(state.path().to_owned()),
    )
    .unwrap();
    client.retry.rate_limit.recovery_budget_secs = 1;
    client.stream_idle_timeout = Duration::from_secs(3);
    let open = StreamOpenRequest {
        policy: StreamHttpPolicy::DualWithH1Fallback,
        open_timeout: Duration::from_secs(1),
        idle_timeout: client.stream_idle_timeout,
    };
    let body = serde_json::json!({"fixture": true});
    let open_stream = |policy| {
        let transport =
            client_for_policy(&client.http_client, client.http1_fallback_client(), policy);
        client.open_stream_json_with_retry(transport, &url, &body, policy)
    };
    let persisted = state
        .path()
        .join(format!("{}.json", client.shared_quota.key));
    let peer = async {
        tokio::time::timeout(Duration::from_millis(700), async {
            while !persisted.is_file() {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            open_stream(StreamHttpPolicy::DualWithH1Fallback).await
        })
        .await
    };
    let (result, peer_result) = tokio::join!(
        tokio::time::timeout(
            Duration::from_secs(2),
            open_sse_response(&open, open_stream)
        ),
        peer,
    );
    fixture.abort();

    let error = result.expect("known status must finish").unwrap_err();
    assert!(
        matches!(error.downcast_ref::<LlmError>(), Some(LlmError::QuotaWindow { retry_after: Some(delay), .. }) if *delay == Duration::from_secs(7200))
    );
    let peer_error = peer_result
        .expect("persist and share the header minimum before reading the stalled body")
        .unwrap_err();
    assert!(
        matches!(peer_error.downcast_ref::<LlmError>(), Some(LlmError::QuotaWindow { retry_after: Some(delay), .. }) if *delay > Duration::from_secs(7195))
    );
    assert_eq!(attempts.load(Ordering::Relaxed), 1);
}

#[tokio::test]
async fn forkguard_rate_limit_incomplete_native_success_body_never_reopens() {
    use super::stream_entry::{
        StreamHttpPolicy, StreamOpenRequest, client_for_policy, open_sse_response,
    };
    let (url, attempts, fixture) = hold_http_response(
        b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 128\r\n\r\n{",
    )
    .await;
    let mut client = DeepSeekClient::new(&Config {
        provider: Some("minimax".into()),
        providers: Some(crate::config::ProvidersConfig {
            minimax: crate::config::ProviderConfig {
                api_key: Some(format!("owned-fixture-{}", uuid::Uuid::new_v4())),
                base_url: Some(url.clone()),
                ..Default::default()
            },
            ..Default::default()
        }),
        ..Default::default()
    })
    .unwrap();
    client.stream_idle_timeout = Duration::from_millis(600);
    let open = StreamOpenRequest {
        policy: StreamHttpPolicy::DualWithH1Fallback,
        open_timeout: Duration::from_millis(350),
        idle_timeout: client.stream_idle_timeout,
    };
    let body = serde_json::json!({"fixture": true});
    let result = tokio::time::timeout(
        Duration::from_secs(3),
        open_sse_response(&open, |policy| {
            let transport =
                client_for_policy(&client.http_client, client.http1_fallback_client(), policy);
            client.open_stream_json_with_retry(transport, &url, &body, policy)
        }),
    )
    .await;
    fixture.abort();

    let error = result
        .expect("native response inspection is bounded")
        .unwrap_err();
    assert!(
        matches!(error.downcast_ref::<LlmError>(), Some(LlmError::ParseError(message)) if message.contains("provider JSON envelope"))
    );
    assert_eq!(attempts.load(Ordering::Relaxed), 1);
}

#[tokio::test]
async fn forkguard_rate_limit_blocked_receipt_after_headers_never_reopens() {
    use super::stream_entry::{
        StreamHttpPolicy, StreamOpenRequest, client_for_policy, open_sse_response,
    };
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};

    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .respond_with(ResponseTemplate::new(200).set_body_string("accepted"))
        .expect(1)
        .mount(&server)
        .await;
    let url = server.uri();
    let client = DeepSeekClient::new(&Config {
        api_key: Some(format!("owned-fixture-{}", uuid::Uuid::new_v4())),
        base_url: Some(url.clone()),
        ..Default::default()
    })
    .unwrap();
    let saw_recovered = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let observed = saw_recovered.clone();
    let observer: crate::llm_client::rate_limit::RecoveryObserver = Arc::new(move |receipt| {
        let observed = observed.clone();
        Box::pin(async move {
            if receipt.state == "recovered" {
                observed.store(true, Ordering::Relaxed);
                std::future::pending::<()>().await;
            }
        })
    });
    let open = StreamOpenRequest {
        policy: StreamHttpPolicy::DualWithH1Fallback,
        open_timeout: Duration::from_millis(500),
        idle_timeout: client.stream_idle_timeout,
    };
    let body = serde_json::json!({"fixture": true});
    client.shared_quota.pause(Duration::from_millis(250));
    let result = crate::llm_client::rate_limit::with_observer(
        observer,
        open_sse_response(&open, |policy| {
            let transport =
                client_for_policy(&client.http_client, client.http1_fallback_client(), policy);
            client.open_stream_json_with_retry(transport, &url, &body, policy)
        }),
    )
    .await;

    assert!(saw_recovered.load(Ordering::Relaxed));
    assert!(
        matches!(result.unwrap_err().downcast_ref::<LlmError>(), Some(LlmError::ParseError(message)) if message.contains("processing the response headers"))
    );
    server.verify().await;
}

#[tokio::test]
async fn forkguard_rate_limit_blocked_terminal_receipt_preserves_quota_result() {
    use super::stream_entry::{
        StreamHttpPolicy, StreamOpenRequest, client_for_policy, open_sse_response,
    };
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};

    for wire_request in [true, false] {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(429).insert_header("Retry-After", "7200"))
            .expect(u64::from(wire_request))
            .mount(&server)
            .await;
        let url = server.uri();
        let client = DeepSeekClient::new(&Config {
            api_key: Some(format!("owned-fixture-{}", uuid::Uuid::new_v4())),
            base_url: Some(url.clone()),
            ..Default::default()
        })
        .unwrap();
        if !wire_request {
            client.shared_quota.pause(Duration::from_secs(7200));
        }
        let saw_terminal = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let observed = saw_terminal.clone();
        let observer: crate::llm_client::rate_limit::RecoveryObserver = Arc::new(move |receipt| {
            let observed = observed.clone();
            Box::pin(async move {
                if receipt.state == "interrupted" {
                    observed.store(true, Ordering::Relaxed);
                    std::future::pending::<()>().await;
                }
            })
        });
        let open = StreamOpenRequest {
            policy: StreamHttpPolicy::DualWithH1Fallback,
            open_timeout: Duration::from_millis(500),
            idle_timeout: client.stream_idle_timeout,
        };
        let body = serde_json::json!({"fixture": true});
        let result = crate::llm_client::rate_limit::with_observer(
            observer,
            open_sse_response(&open, |policy| {
                let transport =
                    client_for_policy(&client.http_client, client.http1_fallback_client(), policy);
                client.open_stream_json_with_retry(transport, &url, &body, policy)
            }),
        )
        .await;

        assert!(saw_terminal.load(Ordering::Relaxed));
        assert!(
            matches!(result.unwrap_err().downcast_ref::<LlmError>(), Some(LlmError::QuotaWindow { retry_after: Some(delay), .. }) if *delay > Duration::from_secs(7195))
        );
        server.verify().await;
    }
}

#[tokio::test]
async fn forkguard_rate_limit_blocked_persistence_respects_recovery_deadline() {
    use super::stream_entry::{
        StreamHttpPolicy, StreamOpenRequest, client_for_policy, open_sse_response,
    };
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};

    let native = ResponseTemplate::new(200).set_body_json(
        serde_json::json!({"base_resp":{"status_code":1002,"status_msg":"rate limit"}}),
    );
    for (response, budget, open_timeout) in [
        (ResponseTemplate::new(429), 1, Duration::from_secs(4)),
        (ResponseTemplate::new(429), 600, Duration::from_millis(500)),
        (native, 1, Duration::from_secs(4)),
    ] {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(response.insert_header("Retry-After", "7200"))
            .expect(1)
            .mount(&server)
            .await;
        let url = server.uri();
        let state = tempfile::tempdir().unwrap();
        let mut client = DeepSeekClient::new(&Config {
            provider: Some("minimax".into()),
            providers: Some(crate::config::ProvidersConfig {
                minimax: crate::config::ProviderConfig {
                    api_key: Some(format!("owned-fixture-{}", uuid::Uuid::new_v4())),
                    base_url: Some(url.clone()),
                    ..Default::default()
                },
                ..Default::default()
            }),
            rate_limit_recovery_dir: Some(state.path().to_owned()),
            ..Default::default()
        })
        .unwrap();
        client.retry.rate_limit.recovery_budget_secs = budget;
        let _blocked = client.shared_quota.persistence.lock().await;
        let open = StreamOpenRequest {
            policy: StreamHttpPolicy::DualWithH1Fallback,
            open_timeout,
            idle_timeout: client.stream_idle_timeout,
        };
        let body = serde_json::json!({"fixture": true});
        let result = tokio::time::timeout(
            Duration::from_secs(2),
            open_sse_response(&open, |policy| {
                let transport =
                    client_for_policy(&client.http_client, client.http1_fallback_client(), policy);
                client.open_stream_json_with_retry(transport, &url, &body, policy)
            }),
        )
        .await
        .expect("either deadline must bound a blocked quota store");
        let error = result.unwrap_err();
        assert!(
            matches!(error.downcast_ref::<LlmError>(), Some(LlmError::QuotaWindow { retry_after: Some(delay), .. }) if *delay == Duration::from_secs(7200)),
            "{error:#}"
        );
        assert!(client.shared_quota.remaining().unwrap() > Duration::from_secs(7195));
        server.verify().await;
    }
}

#[tokio::test]
async fn forkguard_rate_limit_definitive_stream_status_does_not_probe_or_fallback() {
    use super::stream_entry::{
        StreamHttpPolicy, StreamOpenRequest, client_for_policy, open_sse_response,
    };
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};

    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .respond_with(ResponseTemplate::new(401))
        .expect(2)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .respond_with(ResponseTemplate::new(500).set_delay(Duration::from_secs(2)))
        .expect(0)
        .mount(&server)
        .await;
    let url = server.uri();
    let client = DeepSeekClient::new(&Config {
        api_key: Some(format!("owned-fixture-{}", uuid::Uuid::new_v4())),
        base_url: Some(url.clone()),
        ..Default::default()
    })
    .unwrap();
    let open = StreamOpenRequest {
        policy: StreamHttpPolicy::DualWithH1Fallback,
        open_timeout: Duration::from_secs(1),
        idle_timeout: client.stream_idle_timeout,
    };
    let body = serde_json::json!({"fixture": true});
    for _ in 0..2 {
        let error = open_sse_response(&open, |policy| {
            let transport =
                client_for_policy(&client.http_client, client.http1_fallback_client(), policy);
            client.open_stream_json_with_retry(transport, &url, &body, policy)
        })
        .await
        .unwrap_err();
        assert!(matches!(
            error.downcast_ref::<LlmError>(),
            Some(LlmError::AuthenticationError(_))
        ));
    }
    server.verify().await;
}

#[tokio::test]
async fn forkguard_rate_limit_http1_does_not_retry_ordinary_server_errors() {
    use super::stream_entry::StreamHttpPolicy;
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};

    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .respond_with(ResponseTemplate::new(503))
        .expect(1)
        .mount(&server)
        .await;
    let client = DeepSeekClient::new(&Config {
        api_key: Some(format!("owned-fixture-{}", uuid::Uuid::new_v4())),
        base_url: Some(server.uri()),
        ..Default::default()
    })
    .unwrap();
    let result = client
        .open_stream_json_with_retry(
            client.http1_fallback_client(),
            &server.uri(),
            &serde_json::json!({"fixture": true}),
            StreamHttpPolicy::Http1Only,
        )
        .await;
    assert!(result.is_err());
    assert_eq!(server.received_requests().await.unwrap().len(), 1);
}

#[tokio::test(start_paused = true)]
async fn forkguard_rate_limit_recovery_expires_while_queued_for_a_provider_slot() {
    let state = tempfile::tempdir().unwrap();
    let mut client = DeepSeekClient::new(&Config {
        api_key: Some(format!("owned-fixture-{}", uuid::Uuid::new_v4())),
        base_url: Some("http://127.0.0.1:1".into()),
        ..Default::default()
    })
    .unwrap();
    let quota = shared_quota(
        uuid::Uuid::new_v4().to_string(),
        Some(1),
        Some(state.path().to_owned()),
    )
    .unwrap();
    client.shared_quota = quota.clone();
    client.request_concurrency = quota.concurrency.clone();
    client.rate_limiter = quota.limiter.clone();
    client.retry.rate_limit.recovery_budget_secs = 2;
    quota.initialize().await.unwrap();
    let blocker = ProviderRequestPermit {
        lease: std::sync::Mutex::new(None),
        limiter: quota.concurrency.as_ref().unwrap().clone(),
    };
    blocker.resume().await;

    let operation_id = uuid::Uuid::new_v4();
    let call = client.for_operation(operation_id);
    let _lease = call.acquire_provider_request_permit().await;
    let recovery = call.recovery_for_operation(operation_id).unwrap();
    recovery.lock().unwrap().started = Some(tokio::time::Instant::now());
    let result = tokio::time::timeout(
        Duration::from_secs(3),
        call.send_stream_open_with_retry(|| call.http_client.get("http://127.0.0.1:1")),
    )
    .await
    .expect("recovery deadline must stop a queued stream open");
    assert!(matches!(
        result.unwrap_err().downcast_ref::<LlmError>(),
        Some(LlmError::QuotaWindow {
            retry_after: None,
            ..
        })
    ));
    assert_eq!(recovery.lock().unwrap().wire_attempts, Some(0));
    assert_eq!(quota.concurrency.as_ref().unwrap().active(), 1);
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
            lease: std::sync::Mutex::new(None),
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
        lease: std::sync::Mutex::new(None),
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
    client.isolated_request_state = true;
    let error = client
        .send_with_retry(|| client.http_client.post(server.uri()))
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
    client.isolated_request_state = true;
    let error = client
        .send_with_retry(|| client.http_client.post(server.uri()))
        .await
        .unwrap_err();
    assert!(matches!(
        error.downcast_ref::<LlmError>(),
        Some(LlmError::RateLimited { .. })
    ));
}
