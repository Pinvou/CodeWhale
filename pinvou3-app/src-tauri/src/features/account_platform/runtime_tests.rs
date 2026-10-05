//! Real loopback protocol regression cases, using only isolated fixture data.
//! Included as a child of protocol_tests so the existing signed-token and
//! keyring fixtures remain the single test boundary.

use super::*;

fn binding(config: &AccountPlatformConfig, subject: &str) -> IdentityBinding {
    IdentityBinding {
        issuer: config.issuer.clone(),
        client_id: config.client_id.clone(),
        token_url: config.token_url.to_string(),
        subject: subject.into(),
        nonce: Some("verified-fixture-nonce".into()),
    }
}

fn save_record(state: &AccountPlatformState, config: &AccountPlatformConfig) -> String {
    let raw = serde_json::to_string(&CredentialRecord {
        refresh_token: "isolated-refresh-value".into(),
        identity: binding(config, "isolated-subject"),
    })
    .unwrap();
    state.credentials.set(REFRESH_TOKEN_KEY, &raw).unwrap();
    raw
}

#[tokio::test]
async fn automatic_restore_is_guest_without_record_and_refreshes_bound_record_over_http() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    assert!(!state.ensure_restored(&config).await.unwrap());
    assert_eq!(http.calls.load(Ordering::SeqCst), 0);
    save_record(&state, &config);
    http.enqueue(200, json!({"access_token":"restored-access","refresh_token":"rotated-refresh","expires_in":300,"token_type":"Bearer"}));
    assert!(state.ensure_restored(&config).await.unwrap());
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
    assert!(state.ensure_restored(&config).await.unwrap());
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
    let memory = state.session.lock().await;
    let restored = memory.as_ref().unwrap();
    assert_eq!(restored.access_token, "restored-access");
    assert_eq!(restored.identity.subject, "isolated-subject");
    assert!(restored.account_user_id.is_none());
    assert!(restored.expires_at > Instant::now());
}

#[tokio::test]
async fn automatic_restore_preserves_temporary_failure_then_clears_confirmed_invalid_grant() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    let raw = save_record(&state, &config);
    http.enqueue(503, json!({"error":"temporarily_unavailable"}));
    assert_eq!(
        state.ensure_restored(&config).await.unwrap_err(),
        "oidc_token_exchange_failed"
    );
    assert_eq!(
        state.credentials.get(REFRESH_TOKEN_KEY).unwrap().as_deref(),
        Some(raw.as_str())
    );
    assert!(state.session.lock().await.is_none());
    let before_invalid_grant = state.session_epoch.load(Ordering::SeqCst);
    http.enqueue(400, json!({"error":"invalid_grant"}));
    assert!(
        state
            .ensure_restored(&config)
            .await
            .unwrap_err()
            .starts_with("authentication_required")
    );
    assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
    assert!(state.session.lock().await.is_none());
    assert!(state.session_epoch.load(Ordering::SeqCst) > before_invalid_grant);
    assert_eq!(http.calls.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn automatic_restore_never_sends_mismatched_identity_to_another_provider() {
    let http = HttpFixture::new().await;
    let mut config = http.config();
    let state = state();
    let raw = save_record(&state, &config);
    config.issuer = "https://different-issuer.example.test".into();
    assert!(
        state
            .ensure_restored(&config)
            .await
            .unwrap_err()
            .starts_with("authentication_required")
    );
    assert_eq!(http.calls.load(Ordering::SeqCst), 0);
    assert_eq!(
        state.credentials.get(REFRESH_TOKEN_KEY).unwrap().as_deref(),
        Some(raw.as_str())
    );
}

#[tokio::test]
async fn native_chat_bootstraps_account_and_only_returns_credentials_inside_rust() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    state
        .install_tokens(tokens(None), None, binding(&config, "subject-a"))
        .await
        .unwrap();
    http.enqueue(
        200,
        json!({"user_id":"11111111-1111-1111-1111-111111111111"}),
    );
    let authorized = state
        .chat_runtime_authorization_with(&config)
        .await
        .unwrap();
    assert_eq!(
        authorized.base_url,
        config.token_gateway_url.join("v1").unwrap().to_string()
    );
    assert_eq!(authorized.access_token, "access-value");
    assert!(authorized.revision.starts_with("oidc-session-"));
    assert!(!authorized.revision.contains("11111111"));
    assert!(!authorized.revision.contains("access-value"));
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
    let cached = state
        .chat_runtime_authorization_with(&config)
        .await
        .unwrap();
    assert_eq!(cached.revision, authorized.revision);
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn refresh_changes_chat_revision_and_keeps_only_same_verified_identity_account() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    let identity = binding(&config, "subject-a");
    state
        .install_tokens(tokens(None), None, identity.clone())
        .await
        .unwrap();
    http.enqueue(
        200,
        json!({"user_id":"11111111-1111-1111-1111-111111111111"}),
    );
    let original = state
        .chat_runtime_authorization_with(&config)
        .await
        .unwrap();
    let mut refreshed = tokens(None);
    refreshed.access_token = "refreshed-access".into();
    state
        .install_tokens(refreshed, Some("refresh-value"), identity)
        .await
        .unwrap();
    let refreshed = state
        .chat_runtime_authorization_with(&config)
        .await
        .unwrap();
    assert_ne!(original.revision, refreshed.revision);
    assert_eq!(refreshed.access_token, "refreshed-access");
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
    let mut next = tokens(None);
    next.access_token = "another-user-access".into();
    state
        .install_tokens(next, None, binding(&config, "subject-b"))
        .await
        .unwrap();
    assert!(
        state
            .session
            .lock()
            .await
            .as_ref()
            .unwrap()
            .account_user_id
            .is_none()
    );
    http.enqueue(
        200,
        json!({"user_id":"22222222-2222-2222-2222-222222222222"}),
    );
    let switched = state
        .chat_runtime_authorization_with(&config)
        .await
        .unwrap();
    assert_ne!(switched.revision, refreshed.revision);
    assert_eq!(http.calls.load(Ordering::SeqCst), 2);
    assert_eq!(
        state
            .session
            .lock()
            .await
            .as_ref()
            .unwrap()
            .account_user_id
            .as_deref(),
        Some("22222222-2222-2222-2222-222222222222")
    );
}

#[tokio::test]
async fn native_chat_bootstrap_failure_does_not_install_identity_or_leak_provider_detail() {
    for (status, body, expected) in [
        (
            503,
            json!({"code":"untrusted-provider-detail"}),
            "account_platform_account_unavailable",
        ),
        (
            200,
            json!({"user_id":""}),
            "account_platform_response_invalid: account user ID missing",
        ),
        (
            200,
            json!({"profile":"untrusted-private-profile"}),
            "account_platform_response_invalid: account user ID missing",
        ),
    ] {
        let http = HttpFixture::new().await;
        let config = http.config();
        let state = state();
        state
            .install_tokens(tokens(None), None, binding(&config, "subject-a"))
            .await
            .unwrap();
        http.enqueue(status, body);
        let error = match state.chat_runtime_authorization_with(&config).await {
            Ok(_) => panic!("unexpected native authorization"),
            Err(error) => error,
        };
        assert_eq!(error, expected);
        assert!(
            state
                .session
                .lock()
                .await
                .as_ref()
                .unwrap()
                .account_user_id
                .is_none()
        );
    }
}

#[tokio::test]
async fn logout_during_account_bootstrap_cannot_restore_identity_or_retry_as_another_user() {
    let http = HttpFixture::new().await;
    let mut config = http.config();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    config.account_url =
        Url::parse(&format!("http://{}/", listener.local_addr().unwrap())).unwrap();
    let (started_tx, started_rx) = tokio::sync::oneshot::channel();
    let (release_tx, release_rx) = tokio::sync::oneshot::channel();
    let server = tokio::spawn(async move {
        let (mut connection, _) = listener.accept().await.unwrap();
        let mut request = Vec::new();
        let mut buffer = [0u8; 4096];
        loop {
            let count = connection.read(&mut buffer).await.unwrap();
            assert_ne!(count, 0);
            request.extend_from_slice(&buffer[..count]);
            if let Some(end) = request.windows(4).position(|window| window == b"\r\n\r\n") {
                let header = String::from_utf8_lossy(&request[..end]);
                let length = header
                    .lines()
                    .find_map(|line| {
                        line.to_ascii_lowercase()
                            .strip_prefix("content-length: ")
                            .and_then(|value| value.parse::<usize>().ok())
                    })
                    .unwrap();
                if request.len() >= end + 4 + length {
                    break;
                }
            }
        }
        started_tx.send(()).unwrap();
        release_rx.await.unwrap();
        let body =
            serde_json::to_vec(&json!({"user_id":"11111111-1111-1111-1111-111111111111"})).unwrap();
        connection.write_all(format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",body.len()).as_bytes()).await.unwrap();
        let _ = connection.write_all(&body).await;
        let _ = connection.shutdown().await;
    });
    let state = Arc::new(state());
    state
        .install_tokens(tokens(None), None, binding(&config, "subject-a"))
        .await
        .unwrap();
    let pending_state = state.clone();
    let pending_config = config.clone();
    let pending = tokio::spawn(async move {
        pending_state
            .chat_runtime_authorization_with(&pending_config)
            .await
    });
    tokio::time::timeout(Duration::from_secs(3), started_rx)
        .await
        .unwrap()
        .unwrap();
    state.logout().await.unwrap();
    let mut replacement = tokens(None);
    replacement.access_token = "another-user-access".into();
    state
        .install_tokens(replacement, None, binding(&config, "subject-b"))
        .await
        .unwrap();
    release_tx.send(()).unwrap();
    let result = tokio::time::timeout(Duration::from_secs(3), pending)
        .await
        .unwrap()
        .unwrap();
    assert!(match result {
        Err(error) => error.starts_with("authentication_required"),
        Ok(_) => false,
    });
    assert!(
        state
            .session
            .lock()
            .await
            .as_ref()
            .unwrap()
            .account_user_id
            .is_none()
    );
    assert_eq!(http.calls.load(Ordering::SeqCst), 0);
    server.await.unwrap();
}

#[test]
fn history_and_chat_routes_preserve_existing_management_and_reject_unknown_queries() {
    let http_config = AccountPlatformConfig {
        issuer: "https://id.example.test".into(),
        jwks_url: Url::parse("https://id.example.test/certs").unwrap(),
        token_url: Url::parse("https://id.example.test/token").unwrap(),
        client_id: "desktop-client".into(),
        account_url: Url::parse("https://account.example.test/").unwrap(),
        token_gateway_url: Url::parse("https://token.example.test/").unwrap(),
    };
    for (area, resource) in [
        ("account", "/v1/me/organizations"),
        ("token", "/v1/token-accounts"),
        ("token", "/v1/token-accounts/eligible?model=example"),
        ("token", "/v1/me/usage-requests"),
        ("token", "/v1/me/usage-requests?limit=50&cursor=YWJjLTEyMw"),
    ] {
        assert!(
            prepare_request(
                &http_config,
                ApiRequest {
                    area: area.into(),
                    resource: resource.into(),
                    method: "GET".into(),
                    body: None,
                    idempotency_key: None
                }
            )
            .is_ok()
        );
    }
    assert!(
        prepare_request(
            &http_config,
            ApiRequest {
                area: "account".into(),
                resource: "/v1/organizations".into(),
                method: "POST".into(),
                body: Some(json!({"name":"isolated-organization"})),
                idempotency_key: None
            }
        )
        .is_err()
    );
    for query in [
        "limit=101",
        "limit=0",
        "limit=2&limit=3",
        "cursor=",
        "cursor=abc&cursor=def",
        "wallet_id=other",
        "limit=5&requester_id=other",
    ] {
        assert!(
            prepare_request(
                &http_config,
                ApiRequest {
                    area: "token".into(),
                    resource: format!("/v1/me/usage-requests?{query}"),
                    method: "GET".into(),
                    body: None,
                    idempotency_key: None
                }
            )
            .is_err()
        );
    }
    let mut chat = ApiRequest {
        area: "token".into(),
        resource: "/v1/chat/completions".into(),
        method: "POST".into(),
        body: Some(json!({"model":"fixture-model","stream":true})),
        idempotency_key: Some("isolated-chat-request-0001".into()),
    };
    assert!(prepare_request(&http_config, chat).is_err());
    chat = ApiRequest {
        area: "token".into(),
        resource: "/v1/chat/completions".into(),
        method: "POST".into(),
        body: Some(json!({"model":"fixture-model","stream":false})),
        idempotency_key: Some("isolated-chat-request-0001".into()),
    };
    assert!(prepare_request(&http_config, chat).is_ok());
}

#[tokio::test]
async fn management_late_unauthorized_response_cannot_retry_as_replacement_identity() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = Arc::new(state());
    state
        .install_tokens(tokens(None), None, binding(&config, "subject-a"))
        .await
        .unwrap();
    let release = http.enqueue_delayed(401, json!({"code":"LOGIN_REQUIRED"}));
    let pending_state = state.clone();
    let pending_config = config.clone();
    let pending = tokio::spawn(async move {
        pending_state
            .request_with(
                &pending_config,
                ApiRequest {
                    area: "account".into(),
                    resource: "/v1/me/organizations".into(),
                    method: "GET".into(),
                    body: None,
                    idempotency_key: None,
                },
            )
            .await
    });
    http.wait_for_calls(1).await;
    state.logout().await.unwrap();
    let mut replacement = tokens(None);
    replacement.access_token = "different-user-access".into();
    state
        .install_tokens(replacement, None, binding(&config, "subject-b"))
        .await
        .unwrap();
    release.send(()).unwrap();
    assert!(
        pending
            .await
            .unwrap()
            .unwrap_err()
            .starts_with("authentication_required")
    );
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
    assert_eq!(
        state
            .session
            .lock()
            .await
            .as_ref()
            .unwrap()
            .identity
            .subject,
        "subject-b"
    );
}

#[tokio::test]
async fn management_bootstrap_remembers_only_the_current_verified_account() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    state
        .install_tokens(tokens(None), None, binding(&config, "subject-a"))
        .await
        .unwrap();
    http.enqueue(
        200,
        json!({"user_id":"11111111-1111-1111-1111-111111111111"}),
    );
    let result = state
        .request_with(
            &config,
            ApiRequest {
                area: "account".into(),
                resource: "/v1/account/bootstrap".into(),
                method: "POST".into(),
                body: Some(json!({})),
                idempotency_key: None,
            },
        )
        .await
        .unwrap();
    assert_eq!(result.status, 200);
    let authorization = state
        .chat_runtime_authorization_with(&config)
        .await
        .unwrap();
    assert_eq!(
        authorization.base_url,
        format!("{}v1", config.token_gateway_url)
    );
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
}
