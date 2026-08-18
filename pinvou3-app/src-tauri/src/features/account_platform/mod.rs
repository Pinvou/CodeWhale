//! Desktop account-platform adapter.
//!
//! OAuth tokens never cross the Tauri command boundary. The WebView submits a
//! password only for the duration of an explicit sign-in request; the password
//! is never persisted. A successful session stores only its renewable refresh
//! token in the system credential store so the next launch can sign in again.

mod platform;

use std::time::{Duration, Instant};

use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
use rand::RngCore as _;
use reqwest::{Method, StatusCode, Url};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::sync::Mutex;

use crate::platform::credential_store::{
    CredentialReference, CredentialStore, SystemCredentialStore,
};

const ENABLED_ENV: &str = "PINVOU_ACCOUNT_PLATFORM_ENABLED";
const ISSUER_ENV: &str = "PINVOU_ACCOUNT_OIDC_ISSUER";
const TOKEN_URL_ENV: &str = "PINVOU_ACCOUNT_OIDC_TOKEN_URL";
const CLIENT_ID_ENV: &str = "PINVOU_ACCOUNT_OIDC_CLIENT_ID";
const ACCOUNT_URL_ENV: &str = "PINVOU_ACCOUNT_SERVICE_URL";
const TOKEN_GATEWAY_URL_ENV: &str = "PINVOU_TOKEN_GATEWAY_URL";
const MAX_RESPONSE_BYTES: usize = 2 * 1024 * 1024;
const MAX_REQUEST_BODY_BYTES: usize = 64 * 1024;

#[derive(Debug, Clone)]
struct AccountPlatformConfig {
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

        Ok(Self {
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

#[derive(Debug)]
struct RuntimeSession {
    access_token: String,
    refresh_token: String,
    expires_at: Instant,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SessionStatus {
    pub enabled: bool,
    pub configured: bool,
    pub authenticated: bool,
    pub error_code: Option<&'static str>,
}

#[derive(Debug, Deserialize)]
pub struct LoginCredentials {
    pub account: String,
    pub password: String,
}

#[derive(Debug, Deserialize)]
struct TokenResponse {
    access_token: String,
    refresh_token: Option<String>,
    #[serde(default = "default_expires_in")]
    expires_in: u64,
}

#[derive(Debug, Deserialize)]
struct OAuthErrorResponse {
    error: Option<String>,
}

#[derive(Debug, Clone, Copy)]
enum TokenFlow {
    Password,
    Refresh,
}

const fn default_expires_in() -> u64 {
    300
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
    client: reqwest::Client,
    credentials: SystemCredentialStore,
}

impl AccountPlatformState {
    pub fn new() -> Result<Self, String> {
        let client = reqwest::Client::builder()
            .connect_timeout(Duration::from_secs(10))
            .timeout(Duration::from_secs(30))
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .map_err(|error| format!("account_platform_http_client_unavailable: {error}"))?;
        Ok(Self {
            session: Mutex::new(None),
            login_guard: Mutex::new(()),
            refresh_guard: Mutex::new(()),
            client,
            credentials: SystemCredentialStore::new(),
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
        let _login = self.login_guard.lock().await;
        let config = AccountPlatformConfig::from_env()?;
        let _refresh = self.refresh_guard.lock().await;
        let account = credentials.account.trim();
        if account.is_empty()
            || account.len() > 320
            || credentials.password.is_empty()
            || credentials.password.len() > 4096
        {
            self.clear_authentication().await?;
            return Err("oidc_credentials_invalid: account or password is invalid".to_string());
        }
        self.clear_authentication().await?;
        let form = [
            ("grant_type", "password"),
            ("client_id", config.client_id.as_str()),
            ("username", account),
            ("password", credentials.password.as_str()),
            ("scope", "openid profile email offline_access"),
        ];
        let tokens = self
            .token_request(&config, &form, TokenFlow::Password)
            .await?;
        self.install_tokens(tokens, None).await?;
        drop(_refresh);
        Ok(self.status().await)
    }

    pub async fn logout(&self) -> Result<SessionStatus, String> {
        let _refresh = self.refresh_guard.lock().await;
        self.clear_authentication().await?;
        drop(_refresh);
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

    async fn token_request(
        &self,
        config: &AccountPlatformConfig,
        form: &[(&str, &str)],
        flow: TokenFlow,
    ) -> Result<TokenResponse, String> {
        let response = self
            .client
            .post(config.token_url.clone())
            .form(form)
            .send()
            .await
            .map_err(|error| format!("oidc_token_endpoint_unavailable: {error}"))?;
        let status = response.status();
        if !status.is_success() {
            let oauth_error = response
                .json::<OAuthErrorResponse>()
                .await
                .ok()
                .and_then(|body| body.error);
            return Err(classify_token_error(flow, status, oauth_error.as_deref()));
        }
        response
            .json::<TokenResponse>()
            .await
            .map_err(|_| "oidc_token_response_invalid: token response is invalid".to_string())
            .and_then(validate_token_response)
    }

    async fn install_tokens(
        &self,
        tokens: TokenResponse,
        previous_refresh_token: Option<&str>,
    ) -> Result<String, String> {
        let refresh_token = tokens
            .refresh_token
            .filter(|token| !token.is_empty())
            .or_else(|| previous_refresh_token.map(str::to_string))
            .ok_or_else(|| {
                "oidc_refresh_token_missing: identity provider did not issue a refresh token"
                    .to_string()
            })?;
        let access_token = tokens.access_token;
        let lifetime = Duration::from_secs(tokens.expires_in);
        self.credentials
            .set(
                &CredentialReference::for_account_platform_refresh_token(),
                &refresh_token,
            )
            .map_err(credential_store_error)?;
        *self.session.lock().await = Some(RuntimeSession {
            access_token: access_token.clone(),
            refresh_token,
            expires_at: Instant::now() + lifetime,
        });
        Ok(access_token)
    }

    async fn access_token(
        &self,
        config: &AccountPlatformConfig,
        rejected_access_token: Option<&str>,
    ) -> Result<String, String> {
        if let Some(session) = self.session.lock().await.as_ref() {
            if access_token_is_usable(session, rejected_access_token) {
                return Ok(session.access_token.clone());
            }
        }
        let _refresh = self.refresh_guard.lock().await;
        if let Some(session) = self.session.lock().await.as_ref() {
            if access_token_is_usable(session, rejected_access_token) {
                return Ok(session.access_token.clone());
            }
        }
        let refresh_token = match self.runtime_refresh_token().await {
            Some(token) => token,
            None => self
                .persisted_refresh_token()?
                .ok_or_else(|| "authentication_required: sign in to Pinvou first".to_string())?,
        };
        let form = [
            ("grant_type", "refresh_token"),
            ("client_id", config.client_id.as_str()),
            ("refresh_token", refresh_token.as_str()),
        ];
        let tokens = match self.token_request(config, &form, TokenFlow::Refresh).await {
            Ok(tokens) => tokens,
            Err(error) => {
                self.clear_authentication().await?;
                return Err(error);
            }
        };
        self.install_tokens(tokens, Some(&refresh_token)).await
    }

    async fn ensure_restored(&self, config: &AccountPlatformConfig) -> Result<bool, String> {
        if self.session.lock().await.is_some() {
            return Ok(true);
        }
        let _refresh = self.refresh_guard.lock().await;
        if self.session.lock().await.is_some() {
            return Ok(true);
        }
        let Some(refresh_token) = self.persisted_refresh_token()? else {
            return Ok(false);
        };
        let form = [
            ("grant_type", "refresh_token"),
            ("client_id", config.client_id.as_str()),
            ("refresh_token", refresh_token.as_str()),
        ];
        let tokens = match self.token_request(config, &form, TokenFlow::Refresh).await {
            Ok(tokens) => tokens,
            Err(error) => {
                self.clear_authentication().await?;
                return Err(error);
            }
        };
        self.install_tokens(tokens, Some(&refresh_token)).await?;
        Ok(true)
    }

    async fn runtime_refresh_token(&self) -> Option<String> {
        self.session
            .lock()
            .await
            .as_ref()
            .map(|session| session.refresh_token.clone())
            .filter(|token| !token.is_empty())
    }

    fn persisted_refresh_token(&self) -> Result<Option<String>, String> {
        self.credentials
            .get(&CredentialReference::for_account_platform_refresh_token())
            .map(|value| value.filter(|token| !token.trim().is_empty()))
            .map_err(credential_store_error)
    }

    async fn clear_authentication(&self) -> Result<(), String> {
        *self.session.lock().await = None;
        self.credentials
            .delete(&CredentialReference::for_account_platform_refresh_token())
            .map_err(credential_store_error)
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
            .map_err(|error| format!("account_platform_service_unavailable: {error}"))
    }
}

fn access_token_is_usable(session: &RuntimeSession, rejected: Option<&str>) -> bool {
    session.expires_at > Instant::now() + Duration::from_secs(30)
        && rejected.is_none_or(|token| token != session.access_token)
}

fn validate_token_response(tokens: TokenResponse) -> Result<TokenResponse, String> {
    if tokens.access_token.trim().is_empty() || tokens.expires_in == 0 {
        return Err("oidc_token_response_invalid: access token is missing or expired".to_string());
    }
    Ok(tokens)
}

fn classify_token_error(flow: TokenFlow, status: StatusCode, oauth_error: Option<&str>) -> String {
    match (flow, oauth_error) {
        (TokenFlow::Password, Some("invalid_grant")) => {
            "oidc_credentials_invalid: account or password is incorrect".to_string()
        }
        (TokenFlow::Password, Some("unauthorized_client")) => {
            "oidc_direct_grant_disabled: in-app password sign-in is disabled".to_string()
        }
        (TokenFlow::Refresh, Some("invalid_grant")) => {
            "authentication_required: saved sign-in has expired".to_string()
        }
        _ => format!(
            "oidc_token_exchange_failed: token endpoint returned {}",
            status.as_u16()
        ),
    }
}

fn session_error_code(error: &str) -> &'static str {
    match error.split(':').next().unwrap_or_default() {
        "credential_store_unavailable" => "credential_store_unavailable",
        "oidc_token_endpoint_unavailable" => "oidc_token_endpoint_unavailable",
        "oidc_token_response_invalid" => "oidc_token_response_invalid",
        "oidc_token_exchange_failed" => "oidc_token_exchange_failed",
        "oidc_direct_grant_disabled" => "oidc_direct_grant_disabled",
        _ => "authentication_required",
    }
}

fn credential_store_error(error: impl std::fmt::Display) -> String {
    format!("credential_store_unavailable: {error}")
}

fn random_urlsafe(bytes: usize) -> String {
    let mut value = vec![0_u8; bytes];
    rand::rng().fill_bytes(&mut value);
    URL_SAFE_NO_PAD.encode(value)
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
        | ("token", &Method::GET, ["v1", "recharge", "products"], None)
        | ("token", &Method::POST, ["v1", "recharge", "orders"], None)
        | ("token", &Method::GET, ["v1", "recharge", "orders", "recent"], None) => true,
        ("account", &Method::GET, ["v1", "organizations", id, "units"], None)
        | ("account", &Method::POST, ["v1", "organizations", id, "units"], None)
        | ("account", &Method::GET, ["v1", "organizations", id, "members"], None)
        | ("account", &Method::POST, ["v1", "organizations", id, "invitations"], None)
        | ("token", &Method::GET, ["v1", "recharge", "orders", id], None) => safe_id(id),
        ("token", &Method::GET, ["v1", "me", "balance"], Some("consistency=strong")) => true,
        ("token", &Method::GET, ["v1", "me", "usage-requests"], query) => {
            usage_history_query_allowed(query)
        }
        _ => false,
    }
}

fn usage_history_query_allowed(query: Option<&str>) -> bool {
    match query {
        None => true,
        Some(query) => query
            .strip_prefix("limit=")
            .filter(|value| !value.is_empty() && !value.contains('&'))
            .and_then(|value| value.parse::<u8>().ok())
            .is_some_and(|limit| (1..=100).contains(&limit)),
    }
}

async fn response_to_api(response: reqwest::Response) -> Result<ApiResponse, String> {
    let status = response.status().as_u16();
    if response
        .content_length()
        .is_some_and(|length| length > MAX_RESPONSE_BYTES as u64)
    {
        return Err("account_platform_response_too_large: response exceeded 2 MiB".to_string());
    }
    let bytes = response
        .bytes()
        .await
        .map_err(|error| format!("account_platform_response_invalid: {error}"))?;
    if bytes.len() > MAX_RESPONSE_BYTES {
        return Err("account_platform_response_too_large: response exceeded 2 MiB".to_string());
    }
    let body = if bytes.is_empty() {
        Value::Null
    } else {
        serde_json::from_slice(&bytes)
            .map_err(|_| "account_platform_response_invalid: expected JSON response".to_string())?
    };
    Ok(ApiResponse { status, body })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn config() -> AccountPlatformConfig {
        AccountPlatformConfig {
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
    fn token_errors_distinguish_password_and_saved_session_failures() {
        assert!(classify_token_error(
            TokenFlow::Password,
            StatusCode::BAD_REQUEST,
            Some("invalid_grant")
        )
        .starts_with("oidc_credentials_invalid:"));
        assert!(classify_token_error(
            TokenFlow::Password,
            StatusCode::UNAUTHORIZED,
            Some("unauthorized_client")
        )
        .starts_with("oidc_direct_grant_disabled:"));
        assert!(classify_token_error(
            TokenFlow::Refresh,
            StatusCode::BAD_REQUEST,
            Some("invalid_grant")
        )
        .starts_with("authentication_required:"));
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

        let usage = prepare_request(
            &config(),
            ApiRequest {
                area: "token".to_string(),
                resource: "/v1/me/usage-requests?limit=50".to_string(),
                method: "GET".to_string(),
                body: None,
                idempotency_key: None,
            },
        )
        .unwrap();
        assert_eq!(
            usage.url.as_str(),
            "https://token.example.test/v1/me/usage-requests?limit=50"
        );

        for resource in [
            "https://evil.example/v1/me/balance",
            "/v1/me/balance?consistency=eventual",
            "/metrics",
            "/v1/token-accounts/eligible?model=x&admin=true",
            "/v1/token-accounts",
            "/v1/me/usage-requests?limit=101",
        ] {
            assert!(prepare_request(
                &config(),
                ApiRequest {
                    area: "token".to_string(),
                    resource: resource.to_string(),
                    method: "GET".to_string(),
                    body: None,
                    idempotency_key: None,
                },
            )
            .is_err());
        }
    }

    #[test]
    fn endpoints_reject_credentials_and_insecure_remote_http() {
        assert!(parse_endpoint("OIDC", "https://user:secret@example.test/token").is_err());
        assert!(parse_endpoint("OIDC", "http://example.test/token").is_err());
        assert!(parse_endpoint("OIDC", "http://127.0.0.1:8080/token").is_ok());
    }
}
