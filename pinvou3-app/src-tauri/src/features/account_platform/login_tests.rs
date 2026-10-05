//! The actual password grant path over isolated HTTP and real signed ID tokens.
//! No provider account, realm, operating-system credential, or password is used.
use super::*;

fn credentials() -> LoginCredentials {
    LoginCredentials {
        account: " isolated-account ".into(),
        password: "isolated-password +&字".into(),
    }
}

fn password_response(signer: &SigningFixture, config: &AccountPlatformConfig) -> Value {
    let mut identity = claims(config);
    identity.as_object_mut().unwrap().remove("nonce");
    json!({"access_token":"isolated-access", "refresh_token":"isolated-refresh", "expires_in":300,
        "token_type":"Bearer", "id_token":signer.sign(&identity)})
}

#[tokio::test]
async fn password_login_verifies_identity_and_persists_only_bound_refresh_record() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let signer = SigningFixture::new();
    http.enqueue(200, password_response(&signer, &config));
    http.enqueue(200, signer.jwks.clone());
    let state = state();
    state.login_with(&config, credentials()).await.unwrap();
    let requests = http.requests.lock().unwrap();
    let raw = &requests[0];
    let end = raw.windows(4).position(|part| part == b"\r\n\r\n").unwrap() + 4;
    let parsed = Url::parse(&format!(
        "https://fixture.invalid/?{}",
        std::str::from_utf8(&raw[end..]).unwrap()
    ))
    .unwrap();
    let form: std::collections::HashMap<_, _> = parsed.query_pairs().into_owned().collect();
    assert_eq!(form.len(), 5);
    assert_eq!(form["grant_type"], "password");
    assert_eq!(form["client_id"], "desktop-client");
    assert_eq!(form["username"], "isolated-account");
    assert!(form["password"] == credentials().password);
    assert_eq!(form["scope"], "openid profile email offline_access");
    // There are five pairs, not an authorization-code/nonce/verifier exchange.
    assert!(
        !form.contains_key("nonce")
            && !form.contains_key("code")
            && !form.contains_key("code_verifier")
    );
    drop(requests);
    let raw = state.credentials.get(REFRESH_TOKEN_KEY).unwrap().unwrap();
    assert!(!raw.contains("isolated-password") && !raw.contains("isolated-access"));
    let record = CredentialRecord::parse(&raw, &config).unwrap();
    assert!(record.identity.nonce.is_none());
    assert_eq!(record.identity.subject, "isolated-subject");
    assert!(state.ensure_restored(&config).await.unwrap());
    assert_eq!(http.calls.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn password_login_requires_signed_identity_and_rejects_unsolicited_nonce() {
    for scenario in ["missing-id", "nonce", "different-audience", "bad-signature"] {
        let http = HttpFixture::new().await;
        let config = http.config();
        let signer = SigningFixture::new();
        let mut response = password_response(&signer, &config);
        match scenario {
            "missing-id" => {
                response.as_object_mut().unwrap().remove("id_token");
            }
            "nonce" => {
                response["id_token"] = Value::String(signer.sign(&claims(&config)));
            }
            "different-audience" => {
                let mut value = claims(&config);
                value.as_object_mut().unwrap().remove("nonce");
                value["aud"] = json!("other-client");
                response["id_token"] = Value::String(signer.sign(&value));
            }
            _ => {
                response["id_token"] = Value::String("not-a-signed-token".into());
            }
        }
        http.enqueue(200, response);
        http.enqueue(200, signer.jwks.clone());
        let state = state();
        let error = state.login_with(&config, credentials()).await.unwrap_err();
        assert!(matches!(
            error.as_str(),
            "oidc_id_token_invalid" | "oidc_token_response_invalid"
        ));
        assert!(state.session.lock().await.is_none());
        assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
    }
}

#[tokio::test]
async fn password_errors_are_static_and_refresh_failure_preserves_confirmed_auth_boundary() {
    for (status, code, expected) in [
        (400, "invalid_grant", "oidc_credentials_invalid"),
        (400, "unauthorized_client", "oidc_direct_grant_disabled"),
        (400, "unsupported_grant_type", "oidc_direct_grant_disabled"),
        (503, "private-provider-detail", "oidc_token_exchange_failed"),
    ] {
        let http = HttpFixture::new().await;
        http.enqueue(
            status,
            json!({"error":code, "error_description":"private-provider-detail isolated-password"}),
        );
        let state = state();
        assert_eq!(
            state
                .login_with(&http.config(), credentials())
                .await
                .unwrap_err(),
            expected
        );
        assert!(state.session.lock().await.is_none());
        assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
    }
}

#[tokio::test]
async fn password_validation_rejects_before_any_transport_or_credential_change() {
    let http = HttpFixture::new().await;
    let state = state();
    for (account, password) in [
        (" ".into(), "value".into()),
        ("user".into(), "".into()),
        ("u".repeat(321), "value".into()),
        ("user".into(), "p".repeat(4097)),
        ("bad\nuser".into(), "value".into()),
    ] {
        assert!(
            state
                .login_with(&http.config(), LoginCredentials { account, password })
                .await
                .unwrap_err()
                .starts_with("oidc_credentials_invalid")
        );
    }
    assert_eq!(http.calls.load(Ordering::SeqCst), 0);
    assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
}

#[tokio::test]
async fn password_exchange_redirect_is_not_followed_or_returned_to_webview() {
    let http = HttpFixture::new().await;
    http.enqueue(302, json!({"error_description":"private-redirect"}));
    http.enqueue(200, json!({"error_description":"must-not-follow"}));
    let state = state();
    assert_eq!(
        state
            .login_with(&http.config(), credentials())
            .await
            .unwrap_err(),
        "oidc_token_exchange_failed"
    );
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
    assert!(state.session.lock().await.is_none());
}

#[tokio::test]
async fn password_identity_refresh_rejects_subject_change_and_nonce_insertion() {
    for changed_subject in [false, true] {
        let http = HttpFixture::new().await;
        let config = http.config();
        let signer = SigningFixture::new();
        http.enqueue(200, password_response(&signer, &config));
        http.enqueue(200, signer.jwks.clone());
        let state = state();
        state.login_with(&config, credentials()).await.unwrap();
        let original = state.credentials.get(REFRESH_TOKEN_KEY).unwrap().unwrap();
        let mut value = claims(&config);
        if changed_subject {
            value.as_object_mut().unwrap().remove("nonce");
            value["sub"] = json!("different-subject");
        }
        http.enqueue(200, json!({"access_token":"new-access", "refresh_token":"untrusted-refresh", "expires_in":300, "token_type":"Bearer", "id_token":signer.sign(&value)}));
        http.enqueue(200, signer.jwks.clone());
        assert_eq!(
            state
                .access_token(&config, Some("isolated-access"))
                .await
                .unwrap_err(),
            "oidc_id_token_invalid"
        );
        assert!(
            state.credentials.get(REFRESH_TOKEN_KEY).unwrap().as_deref() == Some(original.as_str())
        );
        assert_eq!(
            state.session.lock().await.as_ref().unwrap().access_token,
            "isolated-access"
        );
    }
}

#[tokio::test]
async fn logout_during_password_exchange_fences_late_login_before_jwks_or_install() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let signer = SigningFixture::new();
    let release = http.enqueue_delayed(200, password_response(&signer, &config));
    let state = Arc::new(state());
    let pending_state = state.clone();
    let pending_config = config.clone();
    let pending = tokio::spawn(async move {
        pending_state
            .login_with(&pending_config, credentials())
            .await
    });
    http.wait_for_calls(1).await;
    let previous_epoch = state.session_epoch.load(Ordering::SeqCst);
    let logout_state = state.clone();
    let logout = tokio::spawn(async move { logout_state.logout().await });
    tokio::time::timeout(Duration::from_secs(3), async {
        while state.session_epoch.load(Ordering::SeqCst) == previous_epoch {
            tokio::task::yield_now().await;
        }
    })
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
    assert!(!logout.await.unwrap().unwrap().authenticated);
    assert!(state.session.lock().await.is_none());
    assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn successful_password_login_replaces_previous_account_and_bootstrap_cache() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    let old = IdentityBinding {
        issuer: config.issuer.clone(),
        client_id: config.client_id.clone(),
        token_url: config.token_url.to_string(),
        subject: "previous-subject".into(),
        nonce: None,
    };
    state.install_tokens(tokens(None), None, old).await.unwrap();
    state.session.lock().await.as_mut().unwrap().account_user_id =
        Some("previous-account-id".into());
    let signer = SigningFixture::new();
    http.enqueue(200, password_response(&signer, &config));
    http.enqueue(200, signer.jwks.clone());
    state.login_with(&config, credentials()).await.unwrap();
    let current = state.session.lock().await;
    assert_eq!(
        current.as_ref().unwrap().identity.subject,
        "isolated-subject"
    );
    assert!(current.as_ref().unwrap().account_user_id.is_none());
    assert_eq!(current.as_ref().unwrap().generation, 2);
}
