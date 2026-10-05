//! Desktop account-platform adapter.
//!
//! OAuth tokens never cross the Tauri command boundary. The WebView receives
//! only a session summary and allowlisted business API responses; refresh
//! tokens are persisted through the shared OS credential-store abstraction.

mod mobile;
mod oidc;
mod platform;
mod runtime;

pub use mobile::{
    MobileRegistration, MobileRegistrationPrecheck, SMSLoginCredentials, SMSRequest,
    SliderCaptchaRequest,
};

use crate::platform::credential_store::CredentialReference;
pub(crate) use runtime::ChatRuntimeAuthorization;
use runtime::{bootstrap_user_id, session_error_code, usage_history_query_allowed};

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
use tokio::sync::Mutex;

const ENABLED_ENV: &str = "PINVOU_ACCOUNT_PLATFORM_ENABLED";
const ISSUER_ENV: &str = "PINVOU_ACCOUNT_OIDC_ISSUER";
const TOKEN_URL_ENV: &str = "PINVOU_ACCOUNT_OIDC_TOKEN_URL";
const JWKS_URL_ENV: &str = "PINVOU_ACCOUNT_OIDC_JWKS_URL";
const CLIENT_ID_ENV: &str = "PINVOU_ACCOUNT_OIDC_CLIENT_ID";
const ACCOUNT_URL_ENV: &str = "PINVOU_ACCOUNT_SERVICE_URL";
const TOKEN_GATEWAY_URL_ENV: &str = "PINVOU_TOKEN_GATEWAY_URL";
const MAX_RESPONSE_BYTES: usize = 2 * 1024 * 1024;
const MAX_REQUEST_BODY_BYTES: usize = 64 * 1024;
const MAX_CHAT_REQUEST_BODY_BYTES: usize = 4 * 1024 * 1024;
const REFRESH_TOKEN_KEY: &str = "desktop-refresh-token";

#[derive(Debug, Clone)]
struct AccountPlatformConfig {
    issuer: String,
    jwks_url: Url,
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
        let token_url = optional_env(TOKEN_URL_ENV)
            .or_else(|| issuer.as_ref().map(|value| oidc_endpoint(value, "token")))
            .ok_or_else(|| missing_config(TOKEN_URL_ENV, ISSUER_ENV))?;
        let client_id = required_env(CLIENT_ID_ENV)?;
        if client_id.len() > 200 || client_id.chars().any(char::is_whitespace) {
            return Err(format!(
                "account_platform_configuration_invalid: {CLIENT_ID_ENV} is invalid"
            ));
        }

        let token_url = parse_endpoint(TOKEN_URL_ENV, &token_url)?;
        let issuer = resolve_issuer(issuer, &token_url)?;
        let jwks_url = parse_endpoint(
            JWKS_URL_ENV,
            &optional_env(JWKS_URL_ENV).unwrap_or_else(|| oidc_endpoint(&issuer, "certs")),
        )?;
        Ok(Self {
            issuer,
            jwks_url,
            token_url,
            client_id,
            account_url: parse_service_base(ACCOUNT_URL_ENV, &required_env(ACCOUNT_URL_ENV)?)?,
            token_gateway_url: parse_service_base(
                TOKEN_GATEWAY_URL_ENV,
                &required_env(TOKEN_GATEWAY_URL_ENV)?,
            )?,
        })
    }
}

fn resolve_issuer(explicit: Option<String>, token_url: &Url) -> Result<String, String> {
    let issuer = match explicit {
        Some(value) => value,
        None => {
            let path = token_url
                .path()
                .strip_suffix("/protocol/openid-connect/token")
                .ok_or_else(|| {
                    "account_platform_configuration_missing: explicit issuer is required"
                        .to_string()
                })?;
            let mut derived = token_url.clone();
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
    generation: u64,
    account_user_id: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SessionStatus {
    pub enabled: bool,
    pub configured: bool,
    pub authenticated: bool,
    pub error_code: Option<&'static str>,
}

/// The password exists only for this explicit sign-in request, never in the
/// credential record, command response or debug output.
#[derive(Deserialize)]
pub struct LoginCredentials {
    pub account: String,
    pub password: String,
}
impl LoginCredentials {
    fn validate(&self) -> Result<(), String> {
        let account = self.account.trim();
        if account.is_empty()
            || account.len() > 320
            || self.password.is_empty()
            || self.password.len() > 4096
            || account.chars().any(char::is_control)
        {
            return Err("oidc_credentials_invalid: account or password is invalid".into());
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
    session_generation: AtomicU64,
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
            credentials: Arc::new(DefaultKeyringStore::new(
                CredentialReference::for_account_platform_refresh_token().service,
            )),
            session_epoch: AtomicU64::new(0),
            session_generation: AtomicU64::new(0),
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
        let restored = match self.ensure_restored(&config).await {
            Ok(authenticated) => authenticated,
            Err(error) => {
                return SessionStatus {
                    enabled: true,
                    configured: true,
                    authenticated: false,
                    error_code: Some(session_error_code(&error)),
                };
            }
        };
        SessionStatus {
            enabled: true,
            configured: true,
            authenticated: restored,
            error_code: None,
        }
    }

    pub async fn login(&self, credentials: LoginCredentials) -> Result<SessionStatus, String> {
        credentials.validate()?;
        let before_login = self.session_epoch.load(Ordering::SeqCst);
        let _login = self.login_guard.lock().await;
        self.require_session_epoch(before_login)?;
        let config = AccountPlatformConfig::from_env()?;
        self.login_with(&config, credentials).await?;
        Ok(SessionStatus {
            enabled: true,
            configured: true,
            authenticated: true,
            error_code: None,
        })
    }

    async fn login_with(
        &self,
        config: &AccountPlatformConfig,
        credentials: LoginCredentials,
    ) -> Result<(), String> {
        credentials.validate()?;
        let before_login = self.session_epoch.load(Ordering::SeqCst);
        self.credentials
            .probe()
            .map_err(|_| "credential_store_unavailable".to_string())?;
        // Change identity only inside the same refresh/credential ownership lock.
        // Logout increments the epoch before awaiting this lock, fencing a late login.
        let _refresh = self.refresh_guard.lock().await;
        self.require_session_epoch(before_login)?;
        let epoch = self.session_epoch.fetch_add(1, Ordering::SeqCst) + 1;
        *self.session.lock().await = None;
        self.credentials
            .delete(REFRESH_TOKEN_KEY)
            .map_err(|_| "credential_store_failed".to_string())?;
        let form = [
            ("grant_type", "password"),
            ("client_id", config.client_id.as_str()),
            ("username", credentials.account.trim()),
            ("password", credentials.password.as_str()),
            ("scope", "openid profile email offline_access"),
        ];
        let tokens = self.token_request(config, &form).await?;
        self.complete_login(config, tokens, None, epoch).await
    }

    // Caller owns refresh_guard. No initial login can install an unsigned or
    // previously bound ID token; ROPC has no authorization-request nonce.
    async fn complete_login(
        &self,
        config: &AccountPlatformConfig,
        tokens: TokenResponse,
        nonce: Option<&str>,
        epoch: u64,
    ) -> Result<(), String> {
        self.require_session_epoch(epoch)?;
        let identity = oidc::verify_identity(&self.client, config, &tokens, None, nonce).await?;
        self.require_session_epoch(epoch)?;
        self.install_tokens(tokens, None, identity).await?;
        self.require_session_epoch(epoch)?;
        Ok(())
    }

    pub async fn logout(&self) -> Result<SessionStatus, String> {
        self.session_epoch.fetch_add(1, Ordering::SeqCst);
        let _refresh = self.refresh_guard.lock().await;
        *self.session.lock().await = None;
        self.credentials
            .delete(REFRESH_TOKEN_KEY)
            .map_err(|_| "credential_store_failed".to_string())?;
        // Do not call restoring status while owning refresh_guard, or reload a
        // persisted credential after a failed delete and falsely report logout.
        Ok(SessionStatus {
            enabled: account_platform_enabled(),
            configured: AccountPlatformConfig::from_env().is_ok(),
            authenticated: false,
            error_code: None,
        })
    }

    pub async fn request(&self, input: ApiRequest) -> Result<ApiResponse, String> {
        self.request_with(&AccountPlatformConfig::from_env()?, input)
            .await
    }

    async fn request_with(
        &self,
        config: &AccountPlatformConfig,
        input: ApiRequest,
    ) -> Result<ApiResponse, String> {
        let epoch = self.session_epoch.load(Ordering::SeqCst);
        let is_bootstrap = input.area == "account"
            && input.method == "POST"
            && input.resource == "/v1/account/bootstrap";
        let prepared = prepare_request(config, input)?;
        let mut token = self.access_token(config, None).await?;
        self.require_session_epoch(epoch)?;
        let mut response = self.send(&prepared, &token).await?;
        self.require_session_epoch(epoch)?;
        if response.status() == StatusCode::UNAUTHORIZED {
            token = self.access_token(config, Some(&token)).await?;
            self.require_session_epoch(epoch)?;
            response = self.send(&prepared, &token).await?;
        }
        let response = response_to_api(response).await?;
        self.require_session_epoch(epoch)?;
        if is_bootstrap && (200..300).contains(&response.status) {
            self.remember_account_identity(config, &token, bootstrap_user_id(&response.body)?)
                .await?;
        }
        Ok(response)
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
            if let Some(error) = mobile::classify_mobile_grant_error(form, status, &bytes) {
                return Err(error.into());
            }
            if status == StatusCode::BAD_REQUEST && invalid_grant {
                return Err(if is_refresh {
                    "authentication_required"
                } else {
                    "oidc_credentials_invalid"
                }
                .into());
            }
            if !is_refresh
                && status.is_client_error()
                && serde_json::from_slice::<Value>(&bytes)
                    .ok()
                    .is_some_and(|body| {
                        matches!(
                            body.get("error").and_then(Value::as_str),
                            Some("unauthorized_client" | "unsupported_grant_type")
                        )
                    })
            {
                return Err("oidc_direct_grant_disabled".into());
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
        let mut current = self.session.lock().await;
        let account_user_id = current
            .as_ref()
            .filter(|old| {
                old.identity.subject == identity.subject
                    && old.identity.issuer == identity.issuer
                    && old.identity.client_id == identity.client_id
                    && old.identity.token_url == identity.token_url
            })
            .and_then(|old| old.account_user_id.clone());
        *current = Some(RuntimeSession {
            access_token: access_token.clone(),
            expires_at,
            identity,
            account_user_id,
            generation: self.session_generation.fetch_add(1, Ordering::SeqCst) + 1,
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
        let epoch = self.session_epoch.load(Ordering::SeqCst);
        let _refresh = self.refresh_guard.lock().await;
        self.require_session_epoch(epoch)?;
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
                    self.session_epoch.fetch_add(1, Ordering::SeqCst);
                    *self.session.lock().await = None;
                    if self.credentials.delete(REFRESH_TOKEN_KEY).is_err() {
                        return Err("authentication_required: credential_cleanup_failed".into());
                    }
                }
                return Err(error);
            }
        };
        self.require_session_epoch(epoch)?;
        let identity =
            oidc::verify_identity(&self.client, config, &tokens, Some(&record.identity), None)
                .await?;
        self.require_session_epoch(epoch)?;
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

fn random_urlsafe(bytes: usize) -> String {
    let mut value = vec![0_u8; bytes];
    rand::rng().fill_bytes(&mut value);
    URL_SAFE_NO_PAD.encode(value)
}

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
        let limit = if input.area == "token" && parsed.path() == "/v1/chat/completions" {
            if body
                .get("stream")
                .is_some_and(|value| value != &Value::Bool(false))
            {
                return Err(
                    "account_platform_request_denied: streaming is handled by the model runtime"
                        .into(),
                );
            }
            MAX_CHAT_REQUEST_BODY_BYTES
        } else {
            MAX_REQUEST_BODY_BYTES
        };
        if size > limit {
            return Err("account_platform_request_invalid: request body is too large".to_string());
        }
    }
    if input.idempotency_key.is_some()
        && !matches!(
            parsed.path(),
            "/v1/recharge/orders" | "/v1/chat/completions"
        )
    {
        return Err(
            "account_platform_request_denied: idempotency key is not allowed for this route"
                .to_string(),
        );
    }
    if let Some(key) = input.idempotency_key.as_ref() {
        if key.len() < 16
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
        ("token", &Method::POST, ["v1", "chat", "completions"], None) => true,
        ("token", &Method::GET, ["v1", "me", "usage-requests"], query) => {
            usage_history_query_allowed(query)
        }
        ("account", &Method::POST, ["v1", "account", "bootstrap"], None)
        | ("account", &Method::GET, ["v1", "me", "organizations"], None)
        | ("account", &Method::POST, ["v1", "sessions", "current", "active-context"], None)
        | ("token", &Method::GET, ["v1", "recharge", "products"], None)
        | ("token", &Method::GET, ["v1", "recharge", "orders"], None)
        | ("token", &Method::GET, ["v1", "me", "entitlement-packages"], None)
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
            token_url: Url::parse("https://id.example.test/token").unwrap(),
            client_id: "desktop-client".to_string(),
            account_url: Url::parse("https://account.example.test/").unwrap(),
            token_gateway_url: Url::parse("https://token.example.test/").unwrap(),
        }
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
