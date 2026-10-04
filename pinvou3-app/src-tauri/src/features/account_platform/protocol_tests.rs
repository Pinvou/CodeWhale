use super::*;
use aws_lc_rs::{
    rsa::{KeyPair, KeySize},
    signature::KeyPair as _,
};
use codewhale_secrets::InMemoryKeyringStore;
use serde_json::json;
use std::time::{SystemTime, UNIX_EPOCH};

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

struct HttpFixture {
    base: Url,
    responses: Arc<std::sync::Mutex<Vec<(u16, Vec<u8>)>>>,
    calls: Arc<AtomicU64>,
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
        let responses = Arc::new(std::sync::Mutex::new(Vec::<(u16, Vec<u8>)>::new()));
        let calls = Arc::new(AtomicU64::new(0));
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
                counter.fetch_add(1, Ordering::SeqCst);
                let (status, body) = queue.lock().unwrap().remove(0);
                let header = format!(
                    "HTTP/1.1 {status} Test\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
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
            task,
        }
    }

    fn enqueue(&self, status: u16, body: Value) {
        self.responses
            .lock()
            .unwrap()
            .push((status, serde_json::to_vec(&body).unwrap()));
    }

    fn config(&self) -> AccountPlatformConfig {
        AccountPlatformConfig {
            issuer: "https://issuer.example.test/realms/test".into(),
            jwks_url: self.base.join("certs").unwrap(),
            token_url: self.base.join("token").unwrap(),
            authorization_url: self.base.join("authorize").unwrap(),
            client_id: "desktop-client".into(),
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

fn callback_copy() -> CallbackCopy {
    CallbackCopy {
        success_title: "Authorization received".into(),
        success_message: "Return to Pinvou to finish signing in.".into(),
        error_title: "Authorization not accepted".into(),
        error_message: "Return to Pinvou and try again.".into(),
    }
}

#[tokio::test]
async fn callback_copy_requires_all_localized_fields_and_bounded_utf8_before_login() {
    let state = state();
    for field in [
        "successTitle",
        "successMessage",
        "errorTitle",
        "errorMessage",
    ] {
        let mut json = json!({"successTitle":"title","successMessage":"message","errorTitle":"title","errorMessage":"message"});
        json.as_object_mut().unwrap().remove(field);
        assert!(serde_json::from_value::<CallbackCopy>(json).is_err());
    }
    for (title, message) in [
        (" ".to_string(), "message".to_string()),
        ("title".to_string(), "".to_string()),
        ("字".repeat(86), "message".to_string()),
        ("title".to_string(), "字".repeat(342)),
    ] {
        let mut copy = callback_copy();
        copy.success_title = title;
        copy.success_message = message;
        assert_eq!(
            state.login(copy).await.unwrap_err(),
            "account_platform_request_invalid: callback copy invalid"
        );
    }
    let mut copy = callback_copy();
    copy.success_title = "t".repeat(MAX_CALLBACK_TITLE_BYTES);
    copy.error_title = copy.success_title.clone();
    copy.success_message = "m".repeat(MAX_CALLBACK_MESSAGE_BYTES);
    copy.error_message = copy.success_message.clone();
    assert!(copy.validate().is_ok());
    copy.error_title.push('t');
    assert!(copy.validate().is_err());
    copy.error_title.pop();
    copy.error_message.push('m');
    assert!(copy.validate().is_err());
}

#[tokio::test]
async fn callback_page_escapes_localized_text_and_only_claims_authorization_received() {
    for success in [true, false] {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let task = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let copy = CallbackCopy {
                success_title: "授权已接收 <script> & \" '".into(),
                success_message: "ログインを完了するには Pinvou に戻ってください。".into(),
                error_title: "<img src=x onerror=alert(1)>".into(),
                error_message: "& < > \" '".into(),
            };
            copy.validate().unwrap();
            write_callback_response(&mut stream, success, &copy).await;
        });
        let mut stream = tokio::net::TcpStream::connect(address).await.unwrap();
        let mut bytes = Vec::new();
        stream.read_to_end(&mut bytes).await.unwrap();
        task.await.unwrap();
        let response = String::from_utf8(bytes).unwrap();
        let (headers, body) = response.split_once("\r\n\r\n").unwrap();
        let length: usize = headers
            .lines()
            .find_map(|line| {
                line.strip_prefix("Content-Length: ")
                    .and_then(|v| v.parse().ok())
            })
            .unwrap();
        assert_eq!(length, body.len());
        assert!(
            !body.contains("<script>")
                && !body.contains("<img")
                && !body.contains("Login complete")
        );
        if success {
            assert!(headers.starts_with("HTTP/1.1 200 OK"));
            assert!(body.contains("授权已接收 &lt;script&gt; &amp; &quot; &#39;"));
            assert!(body.contains("ログインを完了するには Pinvou に戻ってください。"));
        } else {
            assert!(headers.starts_with("HTTP/1.1 400 Bad Request"));
            assert!(body.contains("&lt;img src=x onerror=alert(1)&gt;"));
            assert!(body.contains("&amp; &lt; &gt; &quot; &#39;"));
        }
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
        nonce: "nonce".into(),
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
        nonce: "nonce".into(),
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
fn issuer_inference_only_accepts_exact_keycloak_authorization_shape() {
    let keycloak =
        Url::parse("https://issuer.example.test/realms/test/protocol/openid-connect/auth").unwrap();
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
async fn callback_ignores_malformed_disconnected_wrong_state_then_accepts_valid_request() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        wait_for_callback_with_timeout(
            listener,
            "expected",
            &callback_copy(),
            Duration::from_secs(2),
        )
        .await
    });
    for request in [
        "",
        "POST /callback?code=x&state=expected HTTP/1.1\r\n\r\n",
        "GET /callback?error=access_denied&state=wrong HTTP/1.1\r\n\r\n",
        "GET /callback?code=x&state=expected&state=wrong HTTP/1.1\r\n\r\n",
    ] {
        let mut stream = tokio::net::TcpStream::connect(address).await.unwrap();
        stream.write_all(request.as_bytes()).await.unwrap();
        stream.shutdown().await.unwrap();
        let mut response = Vec::new();
        let _ = stream.read_to_end(&mut response).await;
    }
    let mut stream = tokio::net::TcpStream::connect(address).await.unwrap();
    stream
        .write_all(b"GET /callback?code=final-code&state=expected HTTP/1.1\r\n\r\n")
        .await
        .unwrap();
    let mut response = Vec::new();
    stream.read_to_end(&mut response).await.unwrap();
    assert!(response.starts_with(b"HTTP/1.1 200"));
    assert_eq!(task.await.unwrap().unwrap(), "final-code");
}

#[tokio::test]
async fn callback_slow_partial_request_does_not_exceed_whole_flow_deadline() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        wait_for_callback_with_timeout(
            listener,
            "expected",
            &callback_copy(),
            Duration::from_millis(80),
        )
        .await
    });
    let mut stream = tokio::net::TcpStream::connect(address).await.unwrap();
    stream.write_all(b"GET /callback?").await.unwrap();
    let result = tokio::time::timeout(Duration::from_secs(1), task)
        .await
        .unwrap()
        .unwrap();
    assert!(result.unwrap_err().starts_with("oidc_callback_timeout"));
}

#[test]
fn callback_error_can_cancel_only_after_matching_state() {
    assert!(
        parse_callback_request(
            "GET /callback?error=private-data&state=wrong HTTP/1.1\r\n\r\n",
            "expected"
        )
        .unwrap_err()
        .starts_with("oidc_callback_invalid")
    );
    assert_eq!(
        parse_callback_request(
            "GET /callback?error=private-data&state=expected HTTP/1.1\r\n\r\n",
            "expected"
        )
        .unwrap_err(),
        "oidc_authorization_failed"
    );
    for request in [
        "GET /callback?code=x&state=expected HTTP/1.1",
        "GET /callback?code=x&state=expected#fragment HTTP/1.1\r\n\r\n",
        "GET /callback?code=x&state=expected HTTP/1.1 extra\r\n\r\n",
    ] {
        assert!(parse_callback_request(request, "expected").is_err());
    }
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
                nonce: "nonce".into(),
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
            nonce: "nonce".into(),
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
async fn logout_fences_previously_started_browser_login_without_provider_request() {
    let http = HttpFixture::new().await;
    let state = state();
    let epoch = state.session_epoch.load(Ordering::SeqCst);
    state.logout().await.unwrap();
    assert_eq!(
        state
            .complete_login(&http.config(), tokens(None), "nonce", epoch)
            .await
            .unwrap_err(),
        "authentication_required"
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
        nonce: "nonce".into(),
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
    assert!(record.identity.subject == "subject" && record.identity.nonce == "nonce");
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
    assert_eq!(response.status, 403);
    let value = serde_json::to_value(response).unwrap();
    assert_eq!(value["body"]["code"], "FORBIDDEN");
    for key in ["access_token", "refresh_token", "id_token"] {
        assert!(value.get(key).is_none() && value["body"].get(key).is_none());
    }
}
