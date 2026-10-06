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
    let prepared = super::super::runtime_model::prepare_authorized_model(
        crate::platform::prefs::SavedModel::builtin_account_platform(),
        authorized,
    )
    .unwrap();
    assert_eq!(
        prepared.model.base_url,
        config.token_gateway_url.join("v1").unwrap().to_string()
    );
    assert_eq!(prepared.revision.as_deref(), Some(cached.revision.as_str()));
    let credential = prepared.credential.unwrap();
    assert_eq!(
        credential.request_idempotency_header(),
        Some("Idempotency-Key")
    );
    assert_eq!(credential.expose_api_key(), "access-value");
    assert!(!format!("{credential:?}").contains("access-value"));
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
        identity_url: None,
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

#[tokio::test]
async fn current_context_model_catalog_uses_authenticated_gateway_and_rejects_late_logout() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = Arc::new(state());
    state
        .install_tokens(tokens(None), None, binding(&config, "subject-a"))
        .await
        .unwrap();
    http.enqueue(
        200,
        json!({"user_id":"11111111-1111-1111-1111-111111111111"}),
    );
    http.enqueue(200, json!({"data":[{"id":"permitted-model"}]}));
    assert_eq!(
        state.model_catalog_with(&config).await.unwrap(),
        vec!["permitted-model"]
    );
    assert_eq!(http.calls.load(Ordering::SeqCst), 2);
    let release = http.enqueue_delayed(200, json!({"data":[{"id":"old-context-model"}]}));
    let task_state = state.clone();
    let task_config = http.config();
    let pending = tokio::spawn(async move { task_state.model_catalog_with(&task_config).await });
    http.wait_for_calls(3).await;
    state.logout().await.unwrap();
    release.send(()).unwrap();
    assert!(
        pending
            .await
            .unwrap()
            .unwrap_err()
            .starts_with("authentication_required")
    );
    assert!(state.session.lock().await.is_none());
}

struct ModelPrefsHome {
    path: std::path::PathBuf,
    previous: Option<std::ffi::OsString>,
}
impl ModelPrefsHome {
    fn new() -> Self {
        let path = std::env::temp_dir().join(format!(
            "pinvou-owned-model-catalog-{}-{}",
            std::process::id(),
            crate::platform::paths::tests::unique_suffix()
        ));
        std::fs::create_dir(&path).unwrap();
        let previous = std::env::var_os("PINVOU3_HOME");
        unsafe {
            std::env::set_var("PINVOU3_HOME", &path);
        }
        Self { path, previous }
    }
}
impl Drop for ModelPrefsHome {
    fn drop(&mut self) {
        match self.previous.take() {
            Some(value) => unsafe { std::env::set_var("PINVOU3_HOME", value) },
            None => unsafe { std::env::remove_var("PINVOU3_HOME") },
        }
        let _ = std::fs::remove_dir_all(&self.path);
    }
}

#[tokio::test]
async fn builtin_model_selection_revalidates_catalog_and_persists_only_authorized_wire_name() {
    let _env_lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|e| e.into_inner());
    let home = ModelPrefsHome::new();
    let prefs = crate::platform::prefs::UserPrefs::load();
    prefs.save().unwrap();
    let old = prefs.advanced.account_platform_default_model.clone();
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
    http.enqueue(200, json!({"data":[{"id":"allowed-wire-model"}]}));
    let before = std::fs::read(home.path.join("settings.json")).unwrap();
    assert_eq!(
        state
            .set_builtin_default_model_with(&config, "arbitrary-model".into())
            .await
            .unwrap_err(),
        "account_platform_model_not_available"
    );
    assert_eq!(
        std::fs::read(home.path.join("settings.json")).unwrap(),
        before
    );
    assert_eq!(
        crate::platform::prefs::UserPrefs::load()
            .advanced
            .account_platform_default_model,
        old
    );
    http.enqueue(200, json!({"data":[{"id":"allowed-wire-model"}]}));
    let result = state
        .set_builtin_default_model_with(&config, "allowed-wire-model".into())
        .await
        .unwrap();
    assert_eq!(result.default_model, "allowed-wire-model");
    assert_eq!(result.available_models, vec!["allowed-wire-model"]);
    let persisted = crate::platform::prefs::UserPrefs::load();
    assert_eq!(
        persisted.advanced.account_platform_default_model.as_deref(),
        Some("allowed-wire-model")
    );
    assert_eq!(
        persisted
            .model_by_id(crate::platform::prefs::BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
            .unwrap()
            .model,
        "allowed-wire-model"
    );
    http.enqueue(503, json!({"message":"isolated unavailable"}));
    assert_eq!(
        state
            .set_builtin_default_model_with(&config, "allowed-wire-model".into())
            .await
            .unwrap_err(),
        "account_platform_model_catalog_unavailable"
    );
    assert_eq!(
        crate::platform::prefs::UserPrefs::load()
            .advanced
            .account_platform_default_model
            .as_deref(),
        Some("allowed-wire-model")
    );
}

#[tokio::test]
async fn model_selection_and_normalized_context_mutations_are_serialized() {
    let _env_lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|error| error.into_inner());
    let _home = ModelPrefsHome::new();
    crate::platform::prefs::UserPrefs::load();
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = Arc::new(state());
    state
        .install_tokens(tokens(None), None, binding(&config, "subject-a"))
        .await
        .unwrap();
    http.enqueue(
        200,
        json!({"user_id":"11111111-1111-1111-1111-111111111111"}),
    );
    state
        .chat_runtime_authorization_with(&config)
        .await
        .unwrap();
    let release = http.enqueue_delayed(200, json!({"data":[{"id":"allowed-wire"}]}));
    let setter_state = state.clone();
    let setter_config = config.clone();
    let setter = tokio::spawn(async move {
        setter_state
            .set_builtin_default_model_with(&setter_config, "allowed-wire".into())
            .await
    });
    http.wait_for_calls(2).await;
    http.enqueue(200, json!({"context_type":"PERSONAL"}));
    let mutation_state = state.clone();
    let mutation = tokio::spawn(async move {
        mutation_state
            .request_with(
                &config,
                ApiRequest {
                    area: "account".into(),
                    method: "PUT".into(),
                    resource: "/v1/me/ignored/../active-context".into(),
                    body: Some(json!({"context_type":"PERSONAL", "organization_id":null, "expected_context_version":1})),
                    idempotency_key: None,
                },
            )
            .await
    });
    tokio::time::sleep(Duration::from_millis(25)).await;
    assert_eq!(http.calls.load(Ordering::SeqCst), 2);
    release.send(()).unwrap();
    assert_eq!(setter.await.unwrap().unwrap().default_model, "allowed-wire");
    assert_eq!(mutation.await.unwrap().unwrap().status, 200);
    assert_eq!(http.calls.load(Ordering::SeqCst), 3);
}

#[tokio::test]
async fn password_reset_requires_exact_authoritative_no_content_and_keeps_session_on_other_success()
{
    for status in [200, 201, 204] {
        let http = HttpFixture::new().await;
        let state = state();
        let mut config = http.config();
        config.identity_url = Some(http.base.clone());
        save_record(&state, &config);
        let epoch = state.session_epoch.load(Ordering::SeqCst);
        http.enqueue(200,json!({"password_handle":"fixture-handle","password_policy_version":"v1","expires_at":"2099-01-01T00:00:00Z"}));
        http.responses.lock().unwrap().push((
            status,
            if status == 204 {
                Vec::new()
            } else {
                b"{}".to_vec()
            },
            None,
        ));
        let result = state
            .reset_password_with(
                &config,
                mobile::PasswordReset {
                    mobile: "13800000000".into(),
                    challenge_id: "4c947809-a2e8-4ef4-851b-f424984d1825".into(),
                    code: "123456".into(),
                    password: "FixturePassword123".into(),
                },
            )
            .await;
        assert_eq!(result.is_ok(), status == 204);
        assert_eq!(
            state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none(),
            status == 204
        );
        assert_eq!(
            state.session_epoch.load(Ordering::SeqCst),
            epoch + u64::from(status == 204)
        );
        let requests = http.requests.lock().unwrap();
        assert_eq!(requests.len(), 2);
        let request = String::from_utf8(requests[1].clone()).unwrap();
        assert!(!request.contains("FixturePassword123") && request.contains("fixture-handle"));
    }
}

#[tokio::test]
async fn delayed_password_reset_cannot_clear_a_new_identity_epoch() {
    let http = HttpFixture::new().await;
    let state = Arc::new(state());
    let mut config = http.config();
    config.identity_url = Some(http.base.clone());
    save_record(&state, &config);
    http.enqueue(200,json!({"password_handle":"fixture-handle","password_policy_version":"v1","expires_at":"2099-01-01T00:00:00Z"}));
    let (release, gate) = tokio::sync::oneshot::channel();
    http.responses
        .lock()
        .unwrap()
        .push((204, Vec::new(), Some(gate)));
    let pending = {
        let state = state.clone();
        tokio::spawn(async move {
            state
                .reset_password_with(
                    &config,
                    mobile::PasswordReset {
                        mobile: "13800000000".into(),
                        challenge_id: "4c947809-a2e8-4ef4-851b-f424984d1825".into(),
                        code: "123456".into(),
                        password: "FixturePassword123".into(),
                    },
                )
                .await
        })
    };
    tokio::time::timeout(Duration::from_secs(3), async {
        while http.calls.load(Ordering::SeqCst) < 2 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    // Model the already-owned new-login installation under the same credential lock;
    // provider authentication is covered separately by the signed-token regressions.
    {
        let _guard = state.refresh_guard.lock().await;
        state.session_epoch.fetch_add(1, Ordering::SeqCst);
        state
            .credentials
            .set(REFRESH_TOKEN_KEY, "new-identity-owned-record")
            .unwrap();
    }
    release.send(()).unwrap();
    assert_eq!(
        pending.await.unwrap().unwrap_err(),
        "authentication_required: account session changed"
    );
    assert_eq!(
        state.credentials.get(REFRESH_TOKEN_KEY).unwrap().as_deref(),
        Some("new-identity-owned-record")
    );
}

#[tokio::test]
async fn password_reset_uncertain_reply_reuses_first_reference_without_resending_password() {
    let http = HttpFixture::new().await;
    let state = state();
    let mut config = http.config();
    config.identity_url = Some(http.base.clone());
    save_record(&state, &config);
    let input = || mobile::PasswordReset {
        mobile: "13800000000".into(),
        challenge_id: "4c947809-a2e8-4ef4-851b-f424984d1825".into(),
        code: "123456".into(),
        password: "FixturePassword123".into(),
    };
    http.enqueue(200,json!({"password_handle":"first-reference","password_policy_version":"v1","expires_at":"2099-01-01T00:00:00Z"}));
    http.enqueue(503, json!({"code":"PROFILE_CHANGE_FAILED"}));
    assert!(state.reset_password_with(&config, input()).await.is_err());
    assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_some());
    http.responses.lock().unwrap().push((204, Vec::new(), None));
    state.reset_password_with(&config, input()).await.unwrap();
    let requests = http.requests.lock().unwrap();
    assert_eq!(requests.len(), 3);
    let body = |index: usize| {
        let bytes = &requests[index];
        let end = bytes
            .windows(4)
            .position(|item| item == b"\r\n\r\n")
            .unwrap()
            + 4;
        serde_json::from_slice::<Value>(&bytes[end..]).unwrap()
    };
    assert_eq!(body(1), body(2));
    assert_eq!(
        body(2).get("password_handle").and_then(Value::as_str),
        Some("first-reference")
    );
    assert!(body(2).get("password").is_none());
    assert!(state.password_reset_memory.lock().await.is_none());
    assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
}

#[tokio::test]
async fn password_reset_different_password_or_challenge_never_reuses_an_old_reference() {
    for changed_password in [false, true] {
        let http = HttpFixture::new().await;
        let state = state();
        let mut config = http.config();
        config.identity_url = Some(http.base.clone());
        for round in 0..2 {
            http.enqueue(200,json!({"password_handle":format!("reference-{round}"),"password_policy_version":"v1","expires_at":"2099-01-01T00:00:00Z"}));
            http.enqueue(503, json!({"code":"PROFILE_CHANGE_FAILED"}));
            let input = mobile::PasswordReset {
                mobile: "13800000000".into(),
                challenge_id: if round == 1 && !changed_password {
                    "6cd9468a-c0e4-45c1-a0e3-65329d71c520"
                } else {
                    "4c947809-a2e8-4ef4-851b-f424984d1825"
                }
                .into(),
                code: "123456".into(),
                password: if round == 1 && changed_password {
                    "DifferentFixturePassword"
                } else {
                    "FixturePassword123"
                }
                .into(),
            };
            assert!(state.reset_password_with(&config, input).await.is_err());
        }
        let requests = http.requests.lock().unwrap();
        assert_eq!(requests.len(), 4);
        assert!(String::from_utf8_lossy(&requests[1]).contains("reference-0"));
        assert!(String::from_utf8_lossy(&requests[3]).contains("reference-1"));
    }
}

#[tokio::test]
async fn profile_get_completion_clears_native_session_only_after_authoritative_completed() {
    for status in [
        "VERIFIED",
        "IDENTITY_UPDATED",
        "ACCOUNT_COMMITTED",
        "RETRY",
        "COMPLETED",
    ] {
        let http = HttpFixture::new().await;
        let state = state();
        let config = http.config();
        save_record(&state, &config);
        state
            .install_tokens(tokens(None), None, binding(&config, "isolated-subject"))
            .await
            .unwrap();
        let epoch = state.session_epoch.load(Ordering::SeqCst);
        http.enqueue(200, json!({"status":status}));
        let result = state
            .request_with(
                &config,
                ApiRequest {
                    area: "account".into(),
                    method: "GET".into(),
                    resource:
                        "/v1/me/./profile-change-requests/4c947809-a2e8-4ef4-851b-f424984d1825"
                            .into(),
                    body: None,
                    idempotency_key: None,
                },
            )
            .await
            .unwrap();
        assert_eq!(
            result.body.get("status").and_then(Value::as_str),
            Some(status)
        );
        assert_eq!(state.session.lock().await.is_none(), status == "COMPLETED");
        assert_eq!(
            state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none(),
            status == "COMPLETED"
        );
        assert_eq!(
            state.session_epoch.load(Ordering::SeqCst),
            epoch + u64::from(status == "COMPLETED")
        );
        assert_eq!(http.calls.load(Ordering::SeqCst), 1);
    }
}

#[test]
fn version_observation_invalidates_models_only_for_current_session_and_new_authority() {
    let state = state();
    let response = |context, authorization| ApiResponse {
        status: 200,
        body: json!({}),
        context_version: Some(context),
        authorization_version: Some(authorization),
    };
    state.observe_context_versions(&response(3, 7), 0);
    assert_eq!(state.context_revision(), 1);
    state.observe_context_versions(&response(2, 7), 0);
    assert_eq!(state.context_revision(), 1);
    state.observe_context_versions(&response(3, 8), 0);
    assert_eq!(state.context_revision(), 2);
    state.session_epoch.store(1, Ordering::SeqCst);
    state.observe_context_versions(&response(200, 200), 0);
    assert_eq!(state.context_revision(), 2);
    state.observe_context_versions(&response(1, 1), 1);
    assert_eq!(state.context_revision(), 3);
    state.observe_context_versions(&response(1, 1), 1);
    assert_eq!(state.context_revision(), 3);
}

#[test]
fn context_authorization_pair_is_scoped_and_atomic_under_late_responses() {
    let state = std::sync::Arc::new(state());
    let response = |context, authorization| ApiResponse {
        status: 200,
        body: json!({}),
        context_version: Some(context),
        authorization_version: Some(authorization),
    };
    state.observe_context_versions(&response(7, 100), 0);
    state.observe_context_versions(&response(8, 1), 0);
    state.observe_context_versions(&response(8, 2), 0);
    state.observe_context_versions(&response(8, 1), 0);
    assert_eq!(*state.observed_context.lock().unwrap(), (0, 8, 1));
    assert_eq!(state.context_revision(), 4);
    std::thread::scope(|scope| {
        for _ in 0..16 {
            let state = &state;
            scope.spawn(move || state.observe_context_versions(&response(7, 200), 0));
        }
        let state = &state;
        scope.spawn(move || state.observe_context_versions(&response(9, 3), 0));
    });
    assert_eq!(*state.observed_context.lock().unwrap(), (0, 9, 3));
    assert_eq!(state.context_revision(), 5);
    state.observe_context_versions(&response(9, 0), 0);
    state.observe_context_versions(&response(0, 200), 0);
    assert_eq!(*state.observed_context.lock().unwrap(), (0, 9, 3));
    assert_eq!(state.context_revision(), 5);
}
