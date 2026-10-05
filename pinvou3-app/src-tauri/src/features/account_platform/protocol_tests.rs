use super::*;
use aws_lc_rs::{
    rsa::{KeyPair, KeySize},
    signature::KeyPair as _,
};
use codewhale_secrets::InMemoryKeyringStore;
use serde_json::json;
use sha2::{Digest, Sha256};
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
};

struct SigningFixture {
    key: KeyPair,
    jwks: Value,
}

impl SigningFixture {
    fn new() -> Self {
        // Private material is generated only in memory, never logged or saved.
        let pair = KeyPair::generate(KeySize::Rsa2048).unwrap();
        let public = pair.public_key();
        let jwks = json!({"keys": [{
            "kty":"RSA", "kid":"ephemeral", "alg":"RS256", "use":"sig",
            "n":URL_SAFE_NO_PAD.encode(public.modulus().big_endian_without_leading_zero()),
            "e":URL_SAFE_NO_PAD.encode(public.exponent().big_endian_without_leading_zero())
        }]});
        Self { key: pair, jwks }
    }

    fn sign(&self, claims: &Value) -> String {
        let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"RS256","kid":"ephemeral","typ":"JWT"}"#);
        let payload = URL_SAFE_NO_PAD.encode(serde_json::to_vec(claims).unwrap());
        let input = format!("{header}.{payload}");
        let mut signature = vec![0u8; self.key.public_modulus_len()];
        self.key
            .sign(
                &aws_lc_rs::signature::RSA_PKCS1_SHA256,
                &aws_lc_rs::rand::SystemRandom::new(),
                input.as_bytes(),
                &mut signature,
            )
            .unwrap();
        format!("{input}.{}", URL_SAFE_NO_PAD.encode(signature))
    }
}

type FixtureResponse = (u16, Vec<u8>, Option<tokio::sync::oneshot::Receiver<()>>);

struct HttpFixture {
    base: Url,
    responses: Arc<std::sync::Mutex<Vec<FixtureResponse>>>,
    calls: Arc<AtomicU64>,
    requests: Arc<std::sync::Mutex<Vec<Vec<u8>>>>,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for HttpFixture {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl HttpFixture {
    async fn new() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let base = Url::parse(&format!("http://{}/", listener.local_addr().unwrap())).unwrap();
        let redirect_header = format!(
            "Location: http://{}/redirect-target\r\n",
            listener.local_addr().unwrap()
        );
        let responses = Arc::new(std::sync::Mutex::new(Vec::<FixtureResponse>::new()));
        let calls = Arc::new(AtomicU64::new(0));
        let requests = Arc::new(std::sync::Mutex::new(Vec::new()));
        let recorded = requests.clone();
        let queue = responses.clone();
        let counter = calls.clone();
        let task = tokio::spawn(async move {
            while let Ok((mut stream, _)) = listener.accept().await {
                let mut bytes = Vec::new();
                let mut chunk = [0u8; 4096];
                let mut body_len = None;
                let mut header_len = 0;
                loop {
                    let n = stream.read(&mut chunk).await.unwrap();
                    if n == 0 {
                        break;
                    }
                    bytes.extend_from_slice(&chunk[..n]);
                    if body_len.is_none() {
                        if let Some(end) = bytes.windows(4).position(|v| v == b"\r\n\r\n") {
                            header_len = end + 4;
                            let text = String::from_utf8_lossy(&bytes[..end]);
                            body_len = Some(
                                text.lines()
                                    .find_map(|line| {
                                        line.to_ascii_lowercase()
                                            .strip_prefix("content-length: ")
                                            .and_then(|v| v.parse::<usize>().ok())
                                    })
                                    .unwrap_or(0),
                            );
                        }
                    }
                    if body_len.is_some_and(|length| bytes.len() >= header_len + length) {
                        break;
                    }
                }
                recorded.lock().unwrap().push(bytes);
                counter.fetch_add(1, Ordering::SeqCst);
                let (status, body, gate) = queue.lock().unwrap().remove(0);
                if let Some(gate) = gate {
                    let _ = gate.await;
                }
                let location = if status == 302 {
                    redirect_header.as_str()
                } else {
                    ""
                };
                let header = format!(
                    "HTTP/1.1 {status} Test\r\n{location}Content-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                stream.write_all(header.as_bytes()).await.unwrap();
                let _ = stream.write_all(&body).await;
                let _ = stream.shutdown().await;
            }
        });
        Self {
            base,
            responses,
            calls,
            requests,
            task,
        }
    }

    fn enqueue(&self, status: u16, body: Value) {
        self.responses
            .lock()
            .unwrap()
            .push((status, serde_json::to_vec(&body).unwrap(), None));
    }

    fn enqueue_delayed(&self, status: u16, body: Value) -> tokio::sync::oneshot::Sender<()> {
        let (release, wait) = tokio::sync::oneshot::channel();
        self.responses.lock().unwrap().push((
            status,
            serde_json::to_vec(&body).unwrap(),
            Some(wait),
        ));
        release
    }

    async fn wait_for_calls(&self, expected: u64) {
        tokio::time::timeout(Duration::from_secs(3), async {
            while self.calls.load(Ordering::SeqCst) < expected {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
    }

    fn config(&self) -> AccountPlatformConfig {
        AccountPlatformConfig {
            issuer: "https://issuer.example.test/realms/test".into(),
            jwks_url: self.base.join("certs").unwrap(),
            token_url: self.base.join("token").unwrap(),
            client_id: "desktop-client".into(),
            identity_url: None,
            account_url: self.base.clone(),
            token_gateway_url: self.base.clone(),
        }
    }
}

fn claims(config: &AccountPlatformConfig) -> Value {
    let issued_at = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs();
    json!({"iss":config.issuer,"aud":config.client_id,"sub":"isolated-subject",
        "nonce":"nonce-value", "iat":issued_at, "exp":issued_at+300})
}

fn tokens(id_token: Option<String>) -> TokenResponse {
    TokenResponse {
        access_token: "access-value".into(),
        refresh_token: Some("refresh-value".into()),
        expires_in: 300,
        token_type: "Bearer".into(),
        id_token,
        received_at: Instant::now(),
    }
}

struct MemoryCredentials(InMemoryKeyringStore);
impl DesktopCredentials for MemoryCredentials {
    fn probe(&self) -> Result<(), SecretsError> {
        Ok(())
    }
}
impl KeyringStore for MemoryCredentials {
    fn get(&self, key: &str) -> Result<Option<String>, SecretsError> {
        self.0.get(key)
    }
    fn set(&self, key: &str, value: &str) -> Result<(), SecretsError> {
        self.0.set(key, value)
    }
    fn delete(&self, key: &str) -> Result<(), SecretsError> {
        self.0.delete(key)
    }
    fn backend_name(&self) -> &'static str {
        "isolated-memory-test"
    }
}

fn state() -> AccountPlatformState {
    let mut state = AccountPlatformState::new().unwrap();
    state.credentials = Arc::new(MemoryCredentials(InMemoryKeyringStore::new()));
    state
}

fn sms_credentials() -> SMSLoginCredentials {
    SMSLoginCredentials {
        mobile: "+8613800138000".into(),
        challenge_id: "4c947809-a2e8-4ef4-851b-f424984d1825".into(),
        code: "123456".into(),
    }
}

#[tokio::test]
async fn sms_login_verifies_identity_and_keeps_secrets_native() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    let signer = SigningFixture::new();
    let mut sms_claims = claims(&config);
    sms_claims.as_object_mut().unwrap().remove("nonce");
    http.enqueue(
        200,
        json!({"access_token":"sms-access", "refresh_token":"sms-refresh",
        "expires_in":300, "token_type":"Bearer", "id_token":signer.sign(&sms_claims)}),
    );
    http.enqueue(200, signer.jwks.clone());
    state
        .login_sms_with(&config, sms_credentials())
        .await
        .unwrap();
    let session = state.session.lock().await;
    assert_eq!(
        session.as_ref().unwrap().identity.subject,
        "isolated-subject"
    );
    assert_eq!(session.as_ref().unwrap().access_token, "sms-access");
    let requests = http.requests.lock().unwrap();
    let request = String::from_utf8_lossy(&requests[0]);
    assert!(request.contains("username=13800138000"));
    assert!(request.contains("sms_challenge_id=4c947809-a2e8-4ef4-851b-f424984d1825"));
    assert!(request.contains("sms_code=123456"));
    assert!(request.contains("scope=openid+profile+email+offline_access"));
    assert!(!request.contains("password="));
}

#[tokio::test]
async fn logout_fences_inflight_sms_login() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = Arc::new(state());
    let release = http.enqueue_delayed(
        200,
        json!({"access_token":"sms-access", "refresh_token":"sms-refresh",
        "expires_in":300, "token_type":"Bearer"}),
    );
    let login_state = state.clone();
    let login =
        tokio::spawn(async move { login_state.login_sms_with(&config, sms_credentials()).await });
    http.wait_for_calls(1).await;
    let logout_state = state.clone();
    let logout = tokio::spawn(async move { logout_state.logout().await });
    tokio::time::timeout(Duration::from_secs(3), async {
        while state.session_epoch.load(Ordering::SeqCst) != 2 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    release.send(()).unwrap();
    assert_eq!(
        login.await.unwrap().unwrap_err(),
        "authentication_required: account session changed"
    );
    assert!(!logout.await.unwrap().unwrap().authenticated);
    assert!(state.session.lock().await.is_none());
    assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn sms_login_rejects_unbound_identity_and_does_not_store_tokens() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    for (id_token, expires_in, expected) in [
        (Value::Null, 300, "oidc_token_response_invalid"),
        (json!("unsigned-token"), 300, "oidc_id_token_invalid"),
        (Value::Null, 0, "oidc_token_response_invalid"),
    ] {
        let mut body = json!({"access_token":"sms-access", "refresh_token":"sms-refresh",
            "expires_in":expires_in, "token_type":"Bearer"});
        if !id_token.is_null() {
            body["id_token"] = id_token;
        }
        http.enqueue(200, body);
        assert_eq!(
            state
                .login_sms_with(&config, sms_credentials())
                .await
                .unwrap_err(),
            expected
        );
        assert!(state.session.lock().await.is_none());
        assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
    }
    assert_eq!(http.calls.load(Ordering::SeqCst), 3);
}

#[tokio::test]
async fn public_mobile_requests_use_fixed_json_contract_without_bearer_or_provider_message() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    http.enqueue(
        429,
        json!({"code":"SMS_RATE_LIMITED", "message":"private provider detail",
        "details":{"retry_after_seconds":60}}),
    );
    assert_eq!(
        state
            .public_account_request(
                &config,
                "v1/auth/sms/challenges",
                json!({"mobile":"13800138000"})
            )
            .await
            .unwrap_err(),
        "mobile_auth_failed:SMS_RATE_LIMITED:60"
    );
    let request = String::from_utf8(http.requests.lock().unwrap()[0].clone()).unwrap();
    assert!(request.starts_with("POST /v1/auth/sms/challenges "));
    assert!(!request.to_ascii_lowercase().contains("authorization:"));
    assert!(request.to_ascii_lowercase().contains("x-request-id:"));
    http.enqueue(200, Value::Null);
    assert_eq!(
        state
            .public_account_request(&config, "v1/auth/register", json!({}))
            .await
            .unwrap_err(),
        "account_platform_response_invalid"
    );
}

struct FaultCredentials {
    inner: InMemoryKeyringStore,
    fail: AtomicU64,
}
impl DesktopCredentials for FaultCredentials {
    fn probe(&self) -> Result<(), SecretsError> {
        Ok(())
    }
}
impl KeyringStore for FaultCredentials {
    fn get(&self, key: &str) -> Result<Option<String>, SecretsError> {
        if self.fail.load(Ordering::SeqCst) == 1 {
            return Err(SecretsError::Keyring("private-backend-detail".into()));
        }
        self.inner.get(key)
    }
    fn set(&self, key: &str, value: &str) -> Result<(), SecretsError> {
        if self.fail.load(Ordering::SeqCst) == 2 {
            return Err(SecretsError::Keyring("private-backend-detail".into()));
        }
        self.inner.set(key, value)
    }
    fn delete(&self, key: &str) -> Result<(), SecretsError> {
        if self.fail.load(Ordering::SeqCst) == 3 {
            return Err(SecretsError::Keyring("private-backend-detail".into()));
        }
        self.inner.delete(key)
    }
    fn backend_name(&self) -> &'static str {
        "isolated-fault-test"
    }
}

#[tokio::test]
async fn credential_backend_failures_do_not_install_new_session_or_claim_successful_logout() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let store = Arc::new(FaultCredentials {
        inner: InMemoryKeyringStore::new(),
        fail: AtomicU64::new(0),
    });
    let mut state = state();
    state.credentials = store.clone();
    let identity = IdentityBinding {
        issuer: config.issuer.clone(),
        client_id: config.client_id.clone(),
        token_url: config.token_url.to_string(),
        subject: "subject".into(),
        nonce: Some("nonce".into()),
    };
    let raw = serde_json::to_string(&CredentialRecord {
        refresh_token: "original-refresh".into(),
        identity: identity.clone(),
    })
    .unwrap();
    store.set(REFRESH_TOKEN_KEY, &raw).unwrap();
    store.fail.store(1, Ordering::SeqCst);
    assert_eq!(
        state.access_token(&config, None).await.unwrap_err(),
        "credential_store_failed"
    );
    assert_eq!(http.calls.load(Ordering::SeqCst), 0);
    store.fail.store(2, Ordering::SeqCst);
    assert_eq!(
        state
            .install_tokens(tokens(None), None, identity)
            .await
            .unwrap_err(),
        "credential_store_failed"
    );
    assert!(state.session.lock().await.is_none());
    assert!(store.inner.get(REFRESH_TOKEN_KEY).unwrap().as_deref() == Some(raw.as_str()));
    store.fail.store(3, Ordering::SeqCst);
    assert_eq!(state.logout().await.unwrap_err(), "credential_store_failed");
    assert!(store.inner.get(REFRESH_TOKEN_KEY).unwrap().as_deref() == Some(raw.as_str()));
    store.fail.store(0, Ordering::SeqCst);
    state.logout().await.unwrap();
    assert!(store.inner.get(REFRESH_TOKEN_KEY).unwrap().is_none());
}

#[tokio::test]
async fn confirmed_invalid_grant_is_auth_required_even_when_native_cleanup_fails() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let store = Arc::new(FaultCredentials {
        inner: InMemoryKeyringStore::new(),
        fail: AtomicU64::new(0),
    });
    let mut state = state();
    state.credentials = store.clone();
    let identity = IdentityBinding {
        issuer: config.issuer.clone(),
        client_id: config.client_id.clone(),
        token_url: config.token_url.to_string(),
        subject: "subject".into(),
        nonce: Some("nonce".into()),
    };
    let raw = serde_json::to_string(&CredentialRecord {
        refresh_token: "rejected-refresh".into(),
        identity: identity.clone(),
    })
    .unwrap();
    store.set(REFRESH_TOKEN_KEY, &raw).unwrap();
    *state.session.lock().await = Some(RuntimeSession {
        access_token: "expired-access".into(),
        expires_at: Instant::now(),
        identity,
        generation: 1,
        account_user_id: None,
    });
    store.fail.store(3, Ordering::SeqCst);
    http.enqueue(400, json!({"error":"invalid_grant"}));
    assert_eq!(
        state.access_token(&config, None).await.unwrap_err(),
        "authentication_required: credential_cleanup_failed"
    );
    assert!(state.session.lock().await.is_none());
    assert!(store.inner.get(REFRESH_TOKEN_KEY).unwrap().as_deref() == Some(raw.as_str()));
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn initial_id_token_requires_real_signature_issuer_audience_subject_expiry_nonce() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let signing = SigningFixture::new();
    let client = state().client;
    let valid = claims(&config);
    http.enqueue(200, signing.jwks.clone());
    let identity = oidc::verify_identity(
        &client,
        &config,
        &tokens(Some(signing.sign(&valid))),
        None,
        Some("nonce-value"),
    )
    .await
    .unwrap();
    assert!(identity.matches(&config));
    for (field, value) in [
        ("iss", json!("https://other.example.test")),
        ("aud", json!("wrong-client")),
        ("sub", json!("")),
        ("exp", json!(1)),
        ("nonce", json!("wrong")),
        ("azp", json!("wrong")),
        ("aud", json!(["desktop-client", "another-client"])),
        ("exp", json!("tomorrow")),
        ("iat", json!("yesterday")),
    ] {
        let mut invalid = valid.clone();
        invalid[field] = value;
        http.enqueue(200, signing.jwks.clone());
        assert!(
            oidc::verify_identity(
                &client,
                &config,
                &tokens(Some(signing.sign(&invalid))),
                None,
                Some("nonce-value")
            )
            .await
            .is_err(),
            "invalid {field} accepted"
        );
    }
    let other = SigningFixture::new();
    http.enqueue(200, signing.jwks.clone());
    assert!(
        oidc::verify_identity(
            &client,
            &config,
            &tokens(Some(other.sign(&valid))),
            None,
            Some("nonce-value")
        )
        .await
        .is_err()
    );
    assert!(
        oidc::verify_identity(&client, &config, &tokens(None), None, Some("nonce-value"))
            .await
            .is_err()
    );
    let mut missing = valid.clone();
    missing.as_object_mut().unwrap().remove("iat");
    http.enqueue(200, signing.jwks.clone());
    assert!(
        oidc::verify_identity(
            &client,
            &config,
            &tokens(Some(signing.sign(&missing))),
            None,
            Some("nonce-value")
        )
        .await
        .is_err()
    );
}

#[tokio::test]
async fn refresh_id_token_keeps_verified_subject_and_optional_nonce_binding() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let signing = SigningFixture::new();
    let client = state().client;
    http.enqueue(200, signing.jwks.clone());
    let identity = oidc::verify_identity(
        &client,
        &config,
        &tokens(Some(signing.sign(&claims(&config)))),
        None,
        Some("nonce-value"),
    )
    .await
    .unwrap();
    let restored = oidc::verify_identity(&client, &config, &tokens(None), Some(&identity), None)
        .await
        .unwrap();
    assert_eq!(restored.subject, identity.subject);
    let mut without_nonce = claims(&config);
    without_nonce.as_object_mut().unwrap().remove("nonce");
    http.enqueue(200, signing.jwks.clone());
    assert!(
        oidc::verify_identity(
            &client,
            &config,
            &tokens(Some(signing.sign(&without_nonce))),
            Some(&identity),
            None
        )
        .await
        .is_ok()
    );
    for (field, value) in [("sub", json!("other")), ("nonce", json!("other"))] {
        let mut changed = claims(&config);
        changed[field] = value;
        http.enqueue(200, signing.jwks.clone());
        assert!(
            oidc::verify_identity(
                &client,
                &config,
                &tokens(Some(signing.sign(&changed))),
                Some(&identity),
                None
            )
            .await
            .is_err()
        );
    }
}

#[test]
fn token_contract_rejects_missing_empty_type_and_expiry_overflow() {
    for body in [
        json!({"access_token":"a","expires_in":300}),
        json!({"access_token":"a","token_type":"Bearer"}),
        json!({"access_token":"a","token_type":"Bearer","expires_in":-1}),
    ] {
        assert!(serde_json::from_value::<TokenResponse>(body).is_err());
    }
    for (access, refresh, kind, expiry) in [
        ("", Some("r"), "Bearer", 300),
        ("a\r\n", Some("r"), "Bearer", 300),
        ("a", Some(""), "Bearer", 300),
        ("a", Some("r"), "MAC", 300),
        ("a", Some("r"), "Bearer", 0),
        ("a", Some("r"), "Bearer", u64::MAX),
    ] {
        let value = TokenResponse {
            access_token: access.into(),
            refresh_token: refresh.map(str::to_string),
            token_type: kind.into(),
            expires_in: expiry,
            id_token: None,
            received_at: Instant::now(),
        };
        assert!(value.expires_at().is_err());
    }
}

#[test]
fn issuer_inference_only_accepts_exact_keycloak_token_shape() {
    let keycloak =
        Url::parse("https://issuer.example.test/realms/test/protocol/openid-connect/token")
            .unwrap();
    assert_eq!(
        resolve_issuer(None, &keycloak).unwrap(),
        "https://issuer.example.test/realms/test"
    );
    let generic = Url::parse("https://issuer.example.test/authorize").unwrap();
    assert!(
        resolve_issuer(None, &generic)
            .unwrap_err()
            .starts_with("account_platform_configuration_missing")
    );
    assert_eq!(
        resolve_issuer(Some("https://public.example.test/custom/".into()), &generic).unwrap(),
        "https://public.example.test/custom/"
    );
    assert!(resolve_issuer(Some("http://remote.example.test/realm".into()), &generic).is_err());
    assert!(
        resolve_issuer(
            Some("https://issuer.example.test/realm?wrong=1".into()),
            &generic
        )
        .is_err()
    );
}

#[test]
fn token_lifetime_is_anchored_at_receipt_and_never_extended_by_verification() {
    let mut response = tokens(None);
    response.received_at = Instant::now().checked_sub(Duration::from_secs(60)).unwrap();
    let remaining = response
        .expires_at()
        .unwrap()
        .saturating_duration_since(Instant::now());
    assert!(remaining > Duration::from_secs(235) && remaining <= Duration::from_secs(240));
    response.expires_in = 30;
    assert!(response.expires_at().is_err());
}

#[tokio::test]
async fn mismatched_or_legacy_credentials_are_not_sent_or_erased() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    for raw in [
        "legacy-raw-refresh".to_string(),
        serde_json::to_string(&CredentialRecord {
            refresh_token: "old-refresh".into(),
            identity: IdentityBinding {
                issuer: "https://other.example.test".into(),
                client_id: config.client_id.clone(),
                token_url: config.token_url.to_string(),
                subject: "subject".into(),
                nonce: Some("nonce".into()),
            },
        })
        .unwrap(),
    ] {
        state.credentials.set(REFRESH_TOKEN_KEY, &raw).unwrap();
        assert!(
            state
                .access_token(&config, None)
                .await
                .unwrap_err()
                .starts_with("authentication_required")
        );
        assert_eq!(
            state.credentials.get(REFRESH_TOKEN_KEY).unwrap().as_deref(),
            Some(raw.as_str())
        );
    }
    assert_eq!(http.calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn refresh_temporary_failure_preserves_record_invalid_grant_clears_it() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    let raw = serde_json::to_string(&CredentialRecord {
        refresh_token: "refresh".into(),
        identity: IdentityBinding {
            issuer: config.issuer.clone(),
            client_id: config.client_id.clone(),
            token_url: config.token_url.to_string(),
            subject: "subject".into(),
            nonce: Some("nonce".into()),
        },
    })
    .unwrap();
    state.credentials.set(REFRESH_TOKEN_KEY, &raw).unwrap();
    for (status, error) in [
        (503, "unavailable"),
        (401, "invalid_client"),
        (400, "invalid_client"),
    ] {
        http.enqueue(status, json!({"error":error}));
        assert_eq!(
            state.access_token(&config, None).await.unwrap_err(),
            "oidc_token_exchange_failed"
        );
        assert_eq!(
            state.credentials.get(REFRESH_TOKEN_KEY).unwrap().as_deref(),
            Some(raw.as_str())
        );
    }
    http.enqueue(400, json!({"error":"invalid_grant"}));
    assert_eq!(
        state.access_token(&config, None).await.unwrap_err(),
        "authentication_required"
    );
    assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
}

#[tokio::test]
async fn logout_fences_previously_started_login_without_provider_request() {
    let http = HttpFixture::new().await;
    let state = state();
    let epoch = state.session_epoch.load(Ordering::SeqCst);
    state.logout().await.unwrap();
    assert_eq!(
        state
            .complete_login(&http.config(), tokens(None), Some("nonce"), epoch)
            .await
            .unwrap_err(),
        "authentication_required: account session changed"
    );
    assert!(state.session.lock().await.is_none());
    assert!(state.credentials.get(REFRESH_TOKEN_KEY).unwrap().is_none());
    assert_eq!(http.calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn refresh_success_rotates_native_record_without_new_id_token_or_local_lifetime_cap() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let state = state();
    let identity = IdentityBinding {
        issuer: config.issuer.clone(),
        client_id: config.client_id.clone(),
        token_url: config.token_url.to_string(),
        subject: "subject".into(),
        nonce: Some("nonce".into()),
    };
    let raw = serde_json::to_string(&CredentialRecord {
        refresh_token: "original-refresh".into(),
        identity,
    })
    .unwrap();
    state.credentials.set(REFRESH_TOKEN_KEY, &raw).unwrap();
    http.enqueue(
        200,
        json!({"access_token":"rotated-access","refresh_token":"rotated-refresh",
        "token_type":"Bearer","expires_in":90,"refresh_expires_in":0}),
    );
    assert!(state.access_token(&config, None).await.unwrap() == "rotated-access");
    let updated = state.credentials.get(REFRESH_TOKEN_KEY).unwrap().unwrap();
    let record = CredentialRecord::parse(&updated, &config).unwrap();
    assert!(record.refresh_token == "rotated-refresh");
    assert!(
        record.identity.subject == "subject" && record.identity.nonce.as_deref() == Some("nonce")
    );
    let session = state.session.lock().await;
    let remaining = session
        .as_ref()
        .unwrap()
        .expires_at
        .saturating_duration_since(Instant::now());
    assert!(remaining > Duration::from_secs(85) && remaining <= Duration::from_secs(90));
    assert_eq!(http.calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn id_token_access_hash_and_jwk_purpose_are_verified() {
    let http = HttpFixture::new().await;
    let config = http.config();
    let signing = SigningFixture::new();
    let client = state().client;
    let mut valid = claims(&config);
    let digest = Sha256::digest(b"access-value");
    valid["at_hash"] = json!(URL_SAFE_NO_PAD.encode(&digest[..digest.len() / 2]));
    http.enqueue(200, signing.jwks.clone());
    assert!(
        oidc::verify_identity(
            &client,
            &config,
            &tokens(Some(signing.sign(&valid))),
            None,
            Some("nonce-value")
        )
        .await
        .is_ok()
    );
    valid["at_hash"] = json!("wrong");
    http.enqueue(200, signing.jwks.clone());
    assert!(
        oidc::verify_identity(
            &client,
            &config,
            &tokens(Some(signing.sign(&valid))),
            None,
            Some("nonce-value")
        )
        .await
        .is_err()
    );
    for (field, value) in [
        ("use", json!("enc")),
        ("key_ops", json!(["encrypt"])),
        ("alg", json!("RS512")),
        ("kid", json!("wrong")),
    ] {
        let mut jwks = signing.jwks.clone();
        jwks["keys"][0][field] = value;
        http.enqueue(200, jwks);
        assert!(
            oidc::verify_identity(
                &client,
                &config,
                &tokens(Some(signing.sign(&claims(&config)))),
                None,
                Some("nonce-value")
            )
            .await
            .is_err()
        );
    }
    let mut ambiguous = signing.jwks.clone();
    ambiguous["keys"]
        .as_array_mut()
        .unwrap()
        .push(signing.jwks["keys"][0].clone());
    http.enqueue(200, ambiguous);
    assert!(
        oidc::verify_identity(
            &client,
            &config,
            &tokens(Some(signing.sign(&claims(&config)))),
            None,
            Some("nonce-value")
        )
        .await
        .is_err()
    );
    let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"HS256","kid":"ephemeral"}"#);
    let token = format!(
        "{header}.{}.ignored",
        URL_SAFE_NO_PAD.encode(serde_json::to_vec(&claims(&config)).unwrap())
    );
    assert!(
        oidc::verify_identity(
            &client,
            &config,
            &tokens(Some(token)),
            None,
            Some("nonce-value")
        )
        .await
        .is_err()
    );
}

#[tokio::test]
async fn streaming_response_without_content_length_is_bounded_before_complete_allocation() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.unwrap();
        let mut request = [0u8; 4096];
        let _ = stream.read(&mut request).await;
        stream
            .write_all(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
            .await
            .unwrap();
        let body = vec![b'x'; MAX_RESPONSE_BYTES + 1];
        let _ = stream.write_all(&body).await;
        let _ = stream.shutdown().await;
    });
    let response = state()
        .client
        .get(format!("http://{address}/"))
        .send()
        .await
        .unwrap();
    assert!(response.content_length().is_none());
    assert_eq!(
        read_bounded(response).await.unwrap_err(),
        "account_platform_response_too_large"
    );
    task.await.unwrap();
}

#[tokio::test]
async fn business_proxy_reads_json_and_preserves_http_failure_without_returning_oauth_tokens() {
    let http = HttpFixture::new().await;
    let state = state();
    let prepared = prepare_request(
        &http.config(),
        ApiRequest {
            area: "token".into(),
            resource: "/v1/me/balance?consistency=strong".into(),
            method: "GET".into(),
            body: None,
            idempotency_key: None,
        },
    )
    .unwrap();
    http.enqueue(403, json!({"code":"FORBIDDEN","message":"Access denied"}));
    let response = response_to_api(state.send(&prepared, "access-value").await.unwrap())
        .await
        .unwrap();
    let request = String::from_utf8(http.requests.lock().unwrap()[0].clone()).unwrap();
    assert!(request.lines().any(|line| {
        line.split_once(':').is_some_and(|(name, value)| {
            name.eq_ignore_ascii_case("X-Device-ID") && value.trim() == state.device_id
        })
    }));
    assert!(
        !request.contains("x-pinvou-client-source:")
            && !request.contains("registration_query_token")
    );
    assert_eq!(response.status, 403);
    let value = serde_json::to_value(response).unwrap();
    assert_eq!(value["body"]["code"], "FORBIDDEN");
    for key in ["access_token", "refresh_token", "id_token"] {
        assert!(value.get(key).is_none() && value["body"].get(key).is_none());
    }
}

#[path = "runtime_tests.rs"]
mod runtime_tests;

#[path = "login_tests.rs"]
mod login_tests;

#[test]
fn profile_completion_uses_shared_canonical_request_fact() {
    let id = "4c947809-a2e8-4ef4-851b-f424984d1825";
    for (resource, expected) in [
        (
            format!("/v1/me/profile-change-requests/{id}/actions/complete"),
            true,
        ),
        (
            format!("/v1/me/./profile-change-requests/{id}/actions/complete"),
            true,
        ),
        (
            format!("/v1/me/unused/../profile-change-requests/{id}/actions/complete"),
            true,
        ),
        (
            format!("/v1/me/profile-change-requests/{id}/actions/complete?unexpected=1"),
            false,
        ),
        (
            "/v1/me/profile-change-requests/not-a-uuid/actions/complete".to_string(),
            true,
        ),
        (
            format!("/v1/me/profile-change-requests/{id}/actions/complete#fragment"),
            false,
        ),
        (
            "/v1/me/profile-change-requests/invalid/id/actions/complete".to_string(),
            false,
        ),
    ] {
        let request = ApiRequest {
            area: "account".into(),
            resource,
            method: "POST".into(),
            body: Some(json!({})),
            idempotency_key: None,
        };
        assert_eq!(profile_completion_requested(&request), expected);
        assert_eq!(
            profile_completion_requested(&ApiRequest {
                area: "token".into(),
                method: "POST".into(),
                resource: request.resource.clone(),
                body: None,
                idempotency_key: None
            }),
            false
        );
    }
}

#[test]
fn profile_status_get_uses_same_completion_invalidation_predicate() {
    for (resource, expected) in [
        ("/v1/me/./profile-change-requests/fixture-id", true),
        (
            "/v1/me/profile-change-requests/fixture-id?anything=1",
            false,
        ),
        (
            "/v1/me/profile-change-requests/fixture-id/mobile-challenges",
            false,
        ),
        (
            "/v1/me/profile-change-requests/fixture-id/actions/complete",
            false,
        ),
    ] {
        let request = ApiRequest {
            area: "account".into(),
            method: "GET".into(),
            resource: resource.into(),
            body: None,
            idempotency_key: None,
        };
        assert_eq!(profile_completion_requested(&request), expected);
    }
}
