//! Desktop account-platform adapter.
//!
//! OAuth tokens never cross the Tauri command boundary. The WebView receives
//! only a session summary and allowlisted business API responses; refresh
//! tokens are persisted through the shared OS credential-store abstraction.

mod oidc;
mod platform;

use std::{
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use codewhale_secrets::{DefaultKeyringStore, KeyringStore, SecretsError};
use oidc::{CredentialRecord, IdentityBinding, TokenResponse, read_bounded};
use rand::Rng as _;
use reqwest::{Method, StatusCode, Url};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
    sync::Mutex,
};

const ENABLED_ENV: &str = "PINVOU_ACCOUNT_PLATFORM_ENABLED";
const ISSUER_ENV: &str = "PINVOU_ACCOUNT_OIDC_ISSUER";
const AUTHORIZATION_URL_ENV: &str = "PINVOU_ACCOUNT_OIDC_AUTHORIZATION_URL";
const TOKEN_URL_ENV: &str = "PINVOU_ACCOUNT_OIDC_TOKEN_URL";
const JWKS_URL_ENV: &str = "PINVOU_ACCOUNT_OIDC_JWKS_URL";
const CLIENT_ID_ENV: &str = "PINVOU_ACCOUNT_OIDC_CLIENT_ID";
const ACCOUNT_URL_ENV: &str = "PINVOU_ACCOUNT_SERVICE_URL";
const TOKEN_GATEWAY_URL_ENV: &str = "PINVOU_TOKEN_GATEWAY_URL";
const CALLBACK_TIMEOUT: Duration = Duration::from_secs(180);
const MAX_CALLBACK_BYTES: usize = 8 * 1024;
const MAX_RESPONSE_BYTES: usize = 2 * 1024 * 1024;
const MAX_REQUEST_BODY_BYTES: usize = 64 * 1024;
const MAX_CALLBACK_TITLE_BYTES: usize = 256;
const MAX_CALLBACK_MESSAGE_BYTES: usize = 1024;
const OIDC_CREDENTIAL_SERVICE: &str = "pinvou3-account-platform-oidc";
const REFRESH_TOKEN_KEY: &str = "desktop-refresh-token";

#[derive(Debug, Clone)]
struct AccountPlatformConfig {
    issuer: String,
    jwks_url: Url,
    authorization_url: Url,
    token_url: Url,
    client_id: String,
    account_url: Url,
    token_gateway_url: Url,
}

impl AccountPlatformConfig {
    fn from_env() -> Result<Self, String> {
        if !account_platform_enabled() {
            return Err("account_platform_disabled: account platform is disabled".to_string());
        }
        if !platform_supported() {
            return Err(
                "unsupported_platform: desktop OIDC requires Windows, macOS, or Linux".to_string(),
            );
        }

        let issuer = optional_env(ISSUER_ENV);
        let authorization_url = optional_env(AUTHORIZATION_URL_ENV)
            .or_else(|| issuer.as_ref().map(|value| oidc_endpoint(value, "auth")))
            .ok_or_else(|| missing_config(AUTHORIZATION_URL_ENV, ISSUER_ENV))?;
        let token_url = optional_env(TOKEN_URL_ENV)
            .or_else(|| issuer.as_ref().map(|value| oidc_endpoint(value, "token")))
            .ok_or_else(|| missing_config(TOKEN_URL_ENV, ISSUER_ENV))?;
        let client_id = required_env(CLIENT_ID_ENV)?;
        if client_id.len() > 200 || client_id.chars().any(char::is_whitespace) {
            return Err(format!(
                "account_platform_configuration_invalid: {CLIENT_ID_ENV} is invalid"
            ));
        }

        let authorization_url = parse_endpoint(AUTHORIZATION_URL_ENV, &authorization_url)?;
        let issuer = resolve_issuer(issuer, &authorization_url)?;
        let jwks_url = parse_endpoint(
            JWKS_URL_ENV,
            &optional_env(JWKS_URL_ENV).unwrap_or_else(|| oidc_endpoint(&issuer, "certs")),
        )?;
        Ok(Self {
            issuer,
            jwks_url,
            authorization_url,
            token_url: parse_endpoint(TOKEN_URL_ENV, &token_url)?,
            client_id,
            account_url: parse_service_base(ACCOUNT_URL_ENV, &required_env(ACCOUNT_URL_ENV)?)?,
            token_gateway_url: parse_service_base(
                TOKEN_GATEWAY_URL_ENV,
                &required_env(TOKEN_GATEWAY_URL_ENV)?,
            )?,
        })
    }
}

fn resolve_issuer(explicit: Option<String>, authorization_url: &Url) -> Result<String, String> {
    let issuer = match explicit {
        Some(value) => value,
        None => {
            let path = authorization_url
                .path()
                .strip_suffix("/protocol/openid-connect/auth")
                .ok_or_else(|| {
                    "account_platform_configuration_missing: explicit issuer is required"
                        .to_string()
                })?;
            let mut derived = authorization_url.clone();
            derived.set_path(path);
            derived.set_query(None);
            derived.to_string().trim_end_matches('/').to_string()
        }
    };
    let issuer_url = parse_endpoint(ISSUER_ENV, &issuer)?;
    if issuer_url.query().is_some() {
        return Err("account_platform_configuration_invalid: issuer query".into());
    }
    Ok(issuer)
}

fn optional_env(name: &str) -> Option<String> {
    std::env::var(name)
        .ok()
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
}

fn required_env(name: &str) -> Result<String, String> {
    optional_env(name)
        .ok_or_else(|| format!("account_platform_configuration_missing: {name} is required"))
}

fn missing_config(primary: &str, fallback: &str) -> String {
    format!("account_platform_configuration_missing: {primary} or {fallback} is required")
}

fn oidc_endpoint(issuer: &str, endpoint: &str) -> String {
    format!(
        "{}/protocol/openid-connect/{endpoint}",
        issuer.trim_end_matches('/')
    )
}

fn parse_endpoint(name: &str, value: &str) -> Result<Url, String> {
    let url = Url::parse(value).map_err(|_| {
        format!("account_platform_configuration_invalid: {name} must be an absolute URL")
    })?;
    if !url.username().is_empty() || url.password().is_some() || url.fragment().is_some() {
        return Err(format!(
            "account_platform_configuration_invalid: {name} must not contain credentials or a fragment"
        ));
    }
    let secure = url.scheme() == "https";
    let loopback = url.scheme() == "http"
        && matches!(url.host_str(), Some("127.0.0.1" | "localhost" | "[::1]"));
    if !secure && !loopback {
        return Err(format!(
            "account_platform_configuration_invalid: {name} must use HTTPS (HTTP is allowed only for loopback development)"
        ));
    }
    Ok(url)
}

fn parse_service_base(name: &str, value: &str) -> Result<Url, String> {
    let mut url = parse_endpoint(name, value)?;
    if url.query().is_some() {
        return Err(format!(
            "account_platform_configuration_invalid: {name} must not contain a query"
        ));
    }
    url.set_path(&format!("{}/", url.path().trim_end_matches('/')));
    Ok(url)
}

pub fn account_platform_enabled() -> bool {
    enabled_value(optional_env(ENABLED_ENV).as_deref())
}

fn enabled_value(value: Option<&str>) -> bool {
    value.is_some_and(|value| {
        matches!(
            value.trim().to_ascii_lowercase().as_str(),
            "1" | "true" | "yes" | "on"
        )
    })
}

const fn platform_supported() -> bool {
    platform::desktop_oidc_supported()
}

struct RuntimeSession {
    access_token: String,
    expires_at: Instant,
    identity: IdentityBinding,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SessionStatus {
    pub enabled: bool,
    pub configured: bool,
    pub authenticated: bool,
    pub error_code: Option<&'static str>,
}

/// Localized UI text provided by the application's central language dictionary.
/// Authorization codes, provider errors and OAuth tokens never enter this DTO.
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CallbackCopy {
    pub success_title: String,
    pub success_message: String,
    pub error_title: String,
    pub error_message: String,
}

impl CallbackCopy {
    fn validate(&self) -> Result<(), String> {
        for (value, max) in [
            (&self.success_title, MAX_CALLBACK_TITLE_BYTES),
            (&self.error_title, MAX_CALLBACK_TITLE_BYTES),
            (&self.success_message, MAX_CALLBACK_MESSAGE_BYTES),
            (&self.error_message, MAX_CALLBACK_MESSAGE_BYTES),
        ] {
            if value.trim().is_empty() || value.len() > max {
                return Err("account_platform_request_invalid: callback copy invalid".into());
            }
        }
        Ok(())
    }
}

trait DesktopCredentials: KeyringStore {
    fn probe(&self) -> Result<(), SecretsError>;
}
impl DesktopCredentials for DefaultKeyringStore {
    fn probe(&self) -> Result<(), SecretsError> {
        DefaultKeyringStore::probe(self)
    }
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ApiRequest {
    pub area: String,
    pub resource: String,
    pub method: String,
    pub body: Option<Value>,
    pub idempotency_key: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ApiResponse {
    pub status: u16,
    pub body: Value,
}

pub struct AccountPlatformState {
    session: Mutex<Option<RuntimeSession>>,
    login_guard: Mutex<()>,
    refresh_guard: Mutex<()>,
    credentials: Arc<dyn DesktopCredentials>,
    session_epoch: AtomicU64,
    client: reqwest::Client,
}

impl AccountPlatformState {
    pub fn new() -> Result<Self, String> {
        let client = reqwest::Client::builder()
            .connect_timeout(Duration::from_secs(10))
            .timeout(Duration::from_secs(30))
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .map_err(|_| "account_platform_http_client_unavailable".to_string())?;
        Ok(Self {
            session: Mutex::new(None),
            login_guard: Mutex::new(()),
            refresh_guard: Mutex::new(()),
            credentials: Arc::new(DefaultKeyringStore::new(OIDC_CREDENTIAL_SERVICE)),
            session_epoch: AtomicU64::new(0),
            client,
        })
    }

    pub async fn status(&self) -> SessionStatus {
        if !account_platform_enabled() {
            return SessionStatus {
                enabled: false,
                configured: false,
                authenticated: false,
                error_code: Some("account_platform_disabled"),
            };
        }
        if !platform_supported() {
            return SessionStatus {
                enabled: true,
                configured: false,
                authenticated: false,
                error_code: Some("unsupported_platform"),
            };
        }
        let config = match AccountPlatformConfig::from_env() {
            Ok(config) => config,
            Err(error) => {
                let error_code = if error.starts_with("account_platform_configuration_invalid:") {
                    "account_platform_configuration_invalid"
                } else {
                    "account_platform_configuration_missing"
                };
                return SessionStatus {
                    enabled: true,
                    configured: false,
                    authenticated: false,
                    error_code: Some(error_code),
                };
            }
        };
        if self.credentials.probe().is_err() {
            return SessionStatus {
                enabled: true,
                configured: false,
                authenticated: false,
                error_code: Some("credential_store_unavailable"),
            };
        }
        let in_memory = self
            .session
            .lock()
            .await
            .as_ref()
            .is_some_and(|session| session.identity.matches(&config));
        let persisted = match self.credentials.get(REFRESH_TOKEN_KEY) {
            Ok(value) => {
                value.is_some_and(|value| CredentialRecord::parse(&value, &config).is_ok())
            }
            Err(_) => {
                return SessionStatus {
                    enabled: true,
                    configured: true,
                    authenticated: false,
                    error_code: Some("credential_store_failed"),
                };
            }
        };
        SessionStatus {
            enabled: true,
            configured: true,
            authenticated: in_memory || persisted,
            error_code: None,
        }
    }

    pub async fn login(&self, callback_copy: CallbackCopy) -> Result<SessionStatus, String> {
        callback_copy.validate()?;
        let _login = self.login_guard.lock().await;
        let epoch = self.session_epoch.load(Ordering::SeqCst);
        let config = AccountPlatformConfig::from_env()?;
        self.credentials
            .probe()
            .map_err(|_| "credential_store_unavailable".to_string())?;
        let listener = TcpListener::bind("127.0.0.1:0")
            .await
            .map_err(|_| "oidc_loopback_unavailable".to_string())?;
        let port = listener
            .local_addr()
            .map_err(|_| "oidc_loopback_unavailable".to_string())?
            .port();
        let redirect_uri = format!("http://127.0.0.1:{port}/callback");
        let (verifier, challenge) = create_pkce();
        let oauth_state = random_urlsafe(32);
        let nonce = random_urlsafe(32);
        let authorization_url =
            authorization_url(&config, &redirect_uri, &oauth_state, &challenge, &nonce)?;

        crate::platform::os::open_target(authorization_url.as_str(), "Pinvou account login")
            .map_err(|_| "oidc_browser_open_failed".to_string())?;
        let code = wait_for_callback(listener, &oauth_state, &callback_copy).await?;
        let tokens = self
            .exchange_code(&config, &code, &redirect_uri, &verifier)
            .await?;
        self.complete_login(&config, tokens, &nonce, epoch).await?;
        Ok(self.status().await)
    }

    async fn complete_login(
        &self,
        config: &AccountPlatformConfig,
        tokens: TokenResponse,
        nonce: &str,
        epoch: u64,
    ) -> Result<(), String> {
        let _refresh = self.refresh_guard.lock().await;
        if self.session_epoch.load(Ordering::SeqCst) != epoch {
            return Err("authentication_required".into());
        }
        let identity =
            oidc::verify_identity(&self.client, config, &tokens, None, Some(nonce)).await?;
        self.install_tokens(tokens, None, identity).await?;
        Ok(())
    }

    pub async fn logout(&self) -> Result<SessionStatus, String> {
        let _refresh = self.refresh_guard.lock().await;
        self.session_epoch.fetch_add(1, Ordering::SeqCst);
        *self.session.lock().await = None;
        self.credentials
            .delete(REFRESH_TOKEN_KEY)
            .map_err(|_| "credential_store_failed".to_string())?;
        Ok(self.status().await)
    }

    pub async fn request(&self, input: ApiRequest) -> Result<ApiResponse, String> {
        let config = AccountPlatformConfig::from_env()?;
        let prepared = prepare_request(&config, input)?;
        let token = self.access_token(&config, None).await?;
        let mut response = self.send(&prepared, &token).await?;
        if response.status() == StatusCode::UNAUTHORIZED {
            let refreshed = self.access_token(&config, Some(&token)).await?;
            response = self.send(&prepared, &refreshed).await?;
        }
        response_to_api(response).await
    }

    async fn exchange_code(
        &self,
        config: &AccountPlatformConfig,
        code: &str,
        redirect_uri: &str,
        verifier: &str,
    ) -> Result<TokenResponse, String> {
        self.token_request(
            config,
            &[
                ("grant_type", "authorization_code"),
                ("client_id", config.client_id.as_str()),
                ("code", code),
                ("redirect_uri", redirect_uri),
                ("code_verifier", verifier),
            ],
        )
        .await
    }

    async fn token_request(
        &self,
        config: &AccountPlatformConfig,
        form: &[(&str, &str)],
    ) -> Result<TokenResponse, String> {
        let response = self
            .client
            .post(config.token_url.clone())
            .form(form)
            .send()
            .await
            .map_err(|_| "oidc_token_endpoint_unavailable".to_string())?;
        let status = response.status();
        let received_at = Instant::now();
        let bytes = read_bounded(response).await?;
        if !status.is_success() {
            let is_refresh = form
                .iter()
                .any(|&(key, value)| key == "grant_type" && value == "refresh_token");
            let invalid_grant = serde_json::from_slice::<Value>(&bytes)
                .ok()
                .is_some_and(|v| v.get("error").and_then(Value::as_str) == Some("invalid_grant"));
            if is_refresh && status == StatusCode::BAD_REQUEST && invalid_grant {
                return Err("authentication_required".into());
            }
            return Err("oidc_token_exchange_failed".into());
        }
        let mut tokens: TokenResponse = serde_json::from_slice(&bytes)
            .map_err(|_| "oidc_token_response_invalid".to_string())?;
        tokens.received_at = received_at;
        tokens.expires_at()?;
        Ok(tokens)
    }

    async fn install_tokens(
        &self,
        tokens: TokenResponse,
        previous_refresh_token: Option<&str>,
        identity: IdentityBinding,
    ) -> Result<String, String> {
        let refresh_token = tokens
            .refresh_token
            .as_deref()
            .or(previous_refresh_token)
            .ok_or_else(|| {
                "oidc_refresh_token_missing: identity provider did not issue a refresh token"
                    .to_string()
            })?;
        let expires_at = tokens.expires_at()?;
        let record = serde_json::to_string(&CredentialRecord {
            refresh_token: refresh_token.into(),
            identity: identity.clone(),
        })
        .map_err(|_| "credential_store_failed".to_string())?;
        self.credentials
            .set(REFRESH_TOKEN_KEY, &record)
            .map_err(|_| "credential_store_failed".to_string())?;
        let access_token = tokens.access_token;
        *self.session.lock().await = Some(RuntimeSession {
            access_token: access_token.clone(),
            expires_at,
            identity,
        });
        Ok(access_token)
    }

    async fn access_token(
        &self,
        config: &AccountPlatformConfig,
        rejected_access_token: Option<&str>,
    ) -> Result<String, String> {
        if let Some(session) = self.session.lock().await.as_ref() {
            if session.identity.matches(config)
                && access_token_is_usable(session, rejected_access_token)
            {
                return Ok(session.access_token.clone());
            }
        }
        let _refresh = self.refresh_guard.lock().await;
        if let Some(session) = self.session.lock().await.as_ref() {
            if session.identity.matches(config)
                && access_token_is_usable(session, rejected_access_token)
            {
                return Ok(session.access_token.clone());
            }
        }
        let raw = self
            .credentials
            .get(REFRESH_TOKEN_KEY)
            .map_err(|_| "credential_store_failed".to_string())?
            .ok_or_else(|| "authentication_required: sign in to Pinvou first".to_string())?;
        let record = CredentialRecord::parse(&raw, config)?;
        let form = [
            ("grant_type", "refresh_token"),
            ("client_id", config.client_id.as_str()),
            ("refresh_token", record.refresh_token.as_str()),
        ];
        let tokens = match self.token_request(config, &form).await {
            Ok(tokens) => tokens,
            Err(error) => {
                if error == "authentication_required" {
                    *self.session.lock().await = None;
                    if self.credentials.delete(REFRESH_TOKEN_KEY).is_err() {
                        return Err("authentication_required: credential_cleanup_failed".into());
                    }
                }
                return Err(error);
            }
        };
        let identity =
            oidc::verify_identity(&self.client, config, &tokens, Some(&record.identity), None)
                .await?;
        self.install_tokens(tokens, Some(&record.refresh_token), identity)
            .await
    }

    async fn send(
        &self,
        request: &PreparedRequest,
        access_token: &str,
    ) -> Result<reqwest::Response, String> {
        let mut builder = self
            .client
            .request(request.method.clone(), request.url.clone())
            .bearer_auth(access_token)
            .header("Accept", "application/json")
            .header("X-Request-ID", random_urlsafe(18));
        if let Some(key) = request.idempotency_key.as_ref() {
            builder = builder.header("Idempotency-Key", key);
        }
        if let Some(body) = request.body.as_ref() {
            builder = builder.json(body);
        }
        builder
            .send()
            .await
            .map_err(|_| "account_platform_service_unavailable".to_string())
    }
}

fn access_token_is_usable(session: &RuntimeSession, rejected: Option<&str>) -> bool {
    session.expires_at > Instant::now() + Duration::from_secs(30)
        && rejected.is_none_or(|token| token != session.access_token)
}

fn authorization_url(
    config: &AccountPlatformConfig,
    redirect_uri: &str,
    state: &str,
    challenge: &str,
    nonce: &str,
) -> Result<Url, String> {
    let mut url = config.authorization_url.clone();
    {
        let mut query = url.query_pairs_mut();
        query
            .clear()
            .append_pair("response_type", "code")
            .append_pair("client_id", &config.client_id)
            .append_pair("redirect_uri", redirect_uri)
            .append_pair("scope", "openid profile email offline_access")
            .append_pair("state", state)
            .append_pair("nonce", nonce)
            .append_pair("code_challenge", challenge)
            .append_pair("code_challenge_method", "S256");
    }
    Ok(url)
}

fn random_urlsafe(bytes: usize) -> String {
    let mut value = vec![0_u8; bytes];
    rand::rng().fill_bytes(&mut value);
    URL_SAFE_NO_PAD.encode(value)
}

fn create_pkce() -> (String, String) {
    let verifier = random_urlsafe(48);
    let challenge = URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()));
    (verifier, challenge)
}

async fn wait_for_callback(
    listener: TcpListener,
    expected_state: &str,
    copy: &CallbackCopy,
) -> Result<String, String> {
    wait_for_callback_with_timeout(listener, expected_state, copy, CALLBACK_TIMEOUT).await
}

async fn wait_for_callback_with_timeout(
    listener: TcpListener,
    expected_state: &str,
    copy: &CallbackCopy,
    timeout: Duration,
) -> Result<String, String> {
    let deadline = Instant::now() + timeout;
    loop {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Err("oidc_callback_timeout: browser login did not complete".to_string());
        }
        let accepted = tokio::time::timeout(remaining, listener.accept())
            .await
            .map_err(|_| "oidc_callback_timeout: browser login did not complete".to_string())?
            .map_err(|_| "oidc_callback_failed".to_string())?;
        let (mut stream, _) = accepted;
        let io_timeout =
            Duration::from_secs(5).min(deadline.saturating_duration_since(Instant::now()));
        let request =
            match tokio::time::timeout(io_timeout, read_callback_request(&mut stream)).await {
                Ok(Ok(request)) => request,
                _ => continue,
            };
        match parse_callback_request(&request, expected_state) {
            Ok(code) => {
                let _ = tokio::time::timeout(
                    deadline.saturating_duration_since(Instant::now()),
                    write_callback_response(&mut stream, true, copy),
                )
                .await;
                return Ok(code);
            }
            Err(error) => {
                let _ = tokio::time::timeout(
                    io_timeout.min(deadline.saturating_duration_since(Instant::now())),
                    write_callback_response(&mut stream, false, copy),
                )
                .await;
                if error.starts_with("oidc_authorization_") {
                    return Err(error);
                }
            }
        }
    }
}

async fn read_callback_request(stream: &mut tokio::net::TcpStream) -> std::io::Result<String> {
    let mut request = Vec::with_capacity(1024);
    let mut chunk = [0_u8; 1024];
    loop {
        let read = stream.read(&mut chunk).await?;
        if read == 0 {
            break;
        }
        request.extend_from_slice(&chunk[..read]);
        if request.len() > MAX_CALLBACK_BYTES {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "callback request exceeds 8 KiB",
            ));
        }
        if request.windows(4).any(|window| window == b"\r\n\r\n")
            || request.windows(2).any(|window| window == b"\n\n")
        {
            break;
        }
    }
    String::from_utf8(request)
        .map_err(|error| std::io::Error::new(std::io::ErrorKind::InvalidData, error))
}

fn parse_callback_request(request: &str, expected_state: &str) -> Result<String, String> {
    if !request.ends_with("\r\n\r\n") {
        return Err("oidc_callback_invalid: incomplete headers".into());
    }
    let first = request
        .lines()
        .next()
        .ok_or_else(|| "oidc_callback_invalid: missing request line".to_string())?;
    let mut parts = first.split_whitespace();
    if parts.next() != Some("GET") {
        return Err("oidc_callback_invalid: callback must use GET".to_string());
    }
    let target = parts
        .next()
        .ok_or_else(|| "oidc_callback_invalid: missing callback target".to_string())?;
    if !matches!(parts.next(), Some("HTTP/1.0" | "HTTP/1.1"))
        || parts.next().is_some()
        || !target.starts_with("/callback?")
        || target.contains('\\')
    {
        return Err("oidc_callback_invalid: invalid request target".into());
    }
    let url = Url::parse(&format!("http://127.0.0.1{target}"))
        .map_err(|_| "oidc_callback_invalid: malformed callback target".to_string())?;
    if url.path() != "/callback" || url.fragment().is_some() {
        return Err("oidc_callback_invalid: unexpected callback path".to_string());
    }
    let mut query = std::collections::HashMap::new();
    for (key, value) in url.query_pairs().into_owned() {
        if query.insert(key, value).is_some() {
            return Err("oidc_callback_invalid: duplicate parameter".into());
        }
    }
    if query.get("state").map(String::as_str) != Some(expected_state) {
        return Err("oidc_callback_invalid: state mismatch".to_string());
    }
    if query.contains_key("error") {
        if query.contains_key("code") {
            return Err("oidc_callback_invalid: contradictory response".into());
        }
        return Err("oidc_authorization_failed".into());
    }
    query
        .get("code")
        .filter(|code| !code.is_empty() && code.len() <= 4096)
        .cloned()
        .ok_or_else(|| "oidc_callback_invalid: authorization code is missing".to_string())
}

fn escape_html(value: &str) -> String {
    let mut escaped = String::with_capacity(value.len());
    for character in value.chars() {
        match character {
            '&' => escaped.push_str("&amp;"),
            '<' => escaped.push_str("&lt;"),
            '>' => escaped.push_str("&gt;"),
            '"' => escaped.push_str("&quot;"),
            '\'' => escaped.push_str("&#39;"),
            _ => escaped.push(character),
        }
    }
    escaped
}

async fn write_callback_response(
    stream: &mut tokio::net::TcpStream,
    success: bool,
    copy: &CallbackCopy,
) {
    let (status, title, message) = if success {
        ("200 OK", &copy.success_title, &copy.success_message)
    } else {
        ("400 Bad Request", &copy.error_title, &copy.error_message)
    };
    let title = escape_html(title);
    let message = escape_html(message);
    let body = format!(
        "<!doctype html><meta charset=utf-8><meta name=referrer content=no-referrer><title>{title}</title><h1>{title}</h1><p>{message}</p>"
    );
    let response = format!(
        "HTTP/1.1 {status}\r\nContent-Type: text/html; charset=utf-8\r\nContent-Security-Policy: default-src 'none'; style-src 'unsafe-inline'\r\nCache-Control: no-store\r\nX-Content-Type-Options: nosniff\r\nConnection: close\r\nContent-Length: {}\r\n\r\n{body}",
        body.len()
    );
    let _ = stream.write_all(response.as_bytes()).await;
    let _ = stream.shutdown().await;
}

#[derive(Debug)]
struct PreparedRequest {
    method: Method,
    url: Url,
    body: Option<Value>,
    idempotency_key: Option<String>,
}

fn prepare_request(
    config: &AccountPlatformConfig,
    input: ApiRequest,
) -> Result<PreparedRequest, String> {
    let method = Method::from_bytes(input.method.as_bytes())
        .map_err(|_| "account_platform_request_invalid: invalid method".to_string())?;
    if !matches!(method, Method::GET | Method::POST) {
        return Err("account_platform_request_denied: method is not allowlisted".to_string());
    }
    let parsed = Url::parse(&format!("http://desktop.invalid{}", input.resource))
        .map_err(|_| "account_platform_request_invalid: malformed resource".to_string())?;
    if parsed.host_str() != Some("desktop.invalid")
        || parsed.fragment().is_some()
        || !route_allowed(&input.area, &method, parsed.path(), parsed.query())
    {
        return Err("account_platform_request_denied: route is not allowlisted".to_string());
    }
    let base = match input.area.as_str() {
        "account" => &config.account_url,
        "token" => &config.token_gateway_url,
        _ => return Err("account_platform_request_denied: unknown service area".to_string()),
    };
    let mut url = base
        .join(parsed.path().trim_start_matches('/'))
        .map_err(|_| "account_platform_request_invalid: cannot resolve resource".to_string())?;
    url.set_query(parsed.query());

    if method == Method::GET && input.body.is_some() {
        return Err("account_platform_request_invalid: GET body is not allowed".to_string());
    }
    if let Some(body) = input.body.as_ref() {
        let size = serde_json::to_vec(body)
            .map_err(|_| "account_platform_request_invalid: invalid JSON body".to_string())?
            .len();
        if size > MAX_REQUEST_BODY_BYTES {
            return Err("account_platform_request_invalid: request body is too large".to_string());
        }
    }
    if input.idempotency_key.is_some() && parsed.path() != "/v1/recharge/orders" {
        return Err(
            "account_platform_request_denied: idempotency key is not allowed for this route"
                .to_string(),
        );
    }
    if let Some(key) = input.idempotency_key.as_ref() {
        if key.is_empty()
            || key.len() > 128
            || !key
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
        {
            return Err("account_platform_request_invalid: invalid idempotency key".to_string());
        }
    }
    Ok(PreparedRequest {
        method,
        url,
        body: input.body,
        idempotency_key: input.idempotency_key,
    })
}

fn route_allowed(area: &str, method: &Method, path: &str, query: Option<&str>) -> bool {
    let segments: Vec<_> = path.trim_matches('/').split('/').collect();
    let safe_id = |value: &str| {
        !value.is_empty()
            && value.len() <= 128
            && value
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    };
    match (area, method, segments.as_slice(), query) {
        ("account", &Method::POST, ["v1", "account", "bootstrap"], None)
        | ("account", &Method::GET, ["v1", "me", "organizations"], None)
        | ("account", &Method::POST, ["v1", "sessions", "current", "active-context"], None)
        | ("account", &Method::POST, ["v1", "organizations"], None)
        | ("token", &Method::GET, ["v1", "recharge", "products"], None)
        | ("token", &Method::POST, ["v1", "recharge", "orders"], None)
        | ("token", &Method::GET, ["v1", "recharge", "orders", "recent"], None)
        | ("token", &Method::GET, ["v1", "token-accounts"], None)
        | ("token", &Method::POST, ["v1", "token-accounts"], None) => true,
        ("account", &Method::GET, ["v1", "organizations", id, "units"], None)
        | ("account", &Method::POST, ["v1", "organizations", id, "units"], None)
        | ("account", &Method::GET, ["v1", "organizations", id, "members"], None)
        | ("account", &Method::POST, ["v1", "organizations", id, "invitations"], None)
        | ("token", &Method::GET, ["v1", "recharge", "orders", id], None)
        | ("token", &Method::GET, ["v1", "usage-requests", id], None) => safe_id(id),
        ("token", &Method::GET, ["v1", "token-accounts", id, "credentials"], None)
        | ("token", &Method::POST, ["v1", "token-accounts", id, "credentials"], None) => {
            safe_id(id)
        }
        ("token", &Method::GET, ["v1", "me", "balance"], Some("consistency=strong")) => true,
        ("token", &Method::GET, ["v1", "token-accounts", "eligible"], Some(query)) => {
            query.starts_with("model=") && !query[6..].is_empty() && !query[6..].contains('&')
        }
        _ => false,
    }
}

async fn response_to_api(response: reqwest::Response) -> Result<ApiResponse, String> {
    let status = response.status().as_u16();
    let bytes = read_bounded(response).await?;
    let body = if bytes.is_empty() {
        Value::Null
    } else {
        serde_json::from_slice(&bytes)
            .map_err(|_| "account_platform_response_invalid: expected JSON response".to_string())?
    };
    Ok(ApiResponse { status, body })
}

#[cfg(test)]
mod protocol_tests;

#[cfg(test)]
mod tests {
    use super::*;

    fn config() -> AccountPlatformConfig {
        AccountPlatformConfig {
            issuer: "https://id.example.test".into(),
            jwks_url: Url::parse("https://id.example.test/certs").unwrap(),
            authorization_url: Url::parse("https://id.example.test/authorize").unwrap(),
            token_url: Url::parse("https://id.example.test/token").unwrap(),
            client_id: "desktop-client".to_string(),
            account_url: Url::parse("https://account.example.test/").unwrap(),
            token_gateway_url: Url::parse("https://token.example.test/").unwrap(),
        }
    }

    #[test]
    fn pkce_uses_s256_and_does_not_reveal_verifier() {
        let (verifier, challenge) = create_pkce();
        assert!(verifier.len() >= 43);
        assert_ne!(verifier, challenge);
        assert_eq!(
            challenge,
            URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()))
        );
        let url = authorization_url(
            &config(),
            "http://127.0.0.1:49152/callback",
            "state-value",
            &challenge,
            "nonce-value",
        )
        .unwrap();
        assert!(!url.as_str().contains(&verifier));
        assert!(url.as_str().contains("code_challenge_method=S256"));
        let query: std::collections::HashMap<_, _> = url.query_pairs().into_owned().collect();
        assert_eq!(
            query.get("scope").map(String::as_str),
            Some("openid profile email offline_access")
        );
        assert_eq!(query.get("nonce").map(String::as_str), Some("nonce-value"));
        assert_eq!(query.get("state").map(String::as_str), Some("state-value"));
    }

    #[test]
    fn compatibility_flag_is_closed_by_default() {
        assert!(!enabled_value(None));
        assert!(!enabled_value(Some("false")));
        assert!(!enabled_value(Some("0")));
        assert!(enabled_value(Some("true")));
        assert!(enabled_value(Some(" ON ")));
    }

    #[test]
    fn callback_requires_exact_path_and_state() {
        let request = "GET /callback?code=abc&state=expected HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n";
        assert_eq!(parse_callback_request(request, "expected").unwrap(), "abc");
        assert!(
            parse_callback_request(request, "other")
                .unwrap_err()
                .contains("state mismatch")
        );
        assert!(
            parse_callback_request(
                "GET /other?code=abc&state=expected HTTP/1.1\r\n\r\n",
                "expected"
            )
            .is_err()
        );
    }

    #[test]
    fn request_proxy_is_a_narrow_allowlist() {
        let prepared = prepare_request(
            &config(),
            ApiRequest {
                area: "token".to_string(),
                resource: "/v1/me/balance?consistency=strong".to_string(),
                method: "GET".to_string(),
                body: None,
                idempotency_key: None,
            },
        )
        .unwrap();
        assert_eq!(
            prepared.url.as_str(),
            "https://token.example.test/v1/me/balance?consistency=strong"
        );

        for resource in [
            "https://evil.example/v1/me/balance",
            "/v1/me/balance?consistency=eventual",
            "/metrics",
            "/v1/token-accounts/eligible?model=x&admin=true",
        ] {
            assert!(
                prepare_request(
                    &config(),
                    ApiRequest {
                        area: "token".to_string(),
                        resource: resource.to_string(),
                        method: "GET".to_string(),
                        body: None,
                        idempotency_key: None,
                    },
                )
                .is_err()
            );
        }
    }

    #[test]
    fn endpoints_reject_credentials_and_insecure_remote_http() {
        assert!(parse_endpoint("OIDC", "https://user:secret@example.test/token").is_err());
        assert!(parse_endpoint("OIDC", "http://example.test/token").is_err());
        assert!(parse_endpoint("OIDC", "http://127.0.0.1:8080/token").is_ok());
    }
}
