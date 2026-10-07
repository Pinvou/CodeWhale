//! Desktop account-platform adapter.
//!
//! OAuth tokens never cross the Tauri command boundary. The WebView submits a
//! password only for the duration of an explicit sign-in request; the password
//! is never persisted. A successful session stores only its renewable refresh
//! token in the system credential store so the next launch can sign in again.

mod platform;

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use rand::Rng as _;
use reqwest::{Method, StatusCode, Url};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::sync::Mutex;

use crate::platform::credential_store::{
    CredentialReference, CredentialStore, SystemCredentialStore,
};
use crate::platform::prefs::Language;

#[cfg(any(debug_assertions, test))]
const ISSUER_ENV: &str = "PINVOU_ACCOUNT_OIDC_ISSUER";
#[cfg(any(debug_assertions, test))]
const TOKEN_URL_ENV: &str = "PINVOU_ACCOUNT_OIDC_TOKEN_URL";
#[cfg(any(debug_assertions, test))]
const CLIENT_ID_ENV: &str = "PINVOU_ACCOUNT_OIDC_CLIENT_ID";
#[cfg(any(debug_assertions, test))]
const ACCOUNT_URL_ENV: &str = "PINVOU_ACCOUNT_SERVICE_URL";
#[cfg(any(debug_assertions, test))]
const IDENTITY_URL_ENV: &str = "PINVOU_IDENTITY_SERVICE_URL";
#[cfg(any(debug_assertions, test))]
const TOKEN_GATEWAY_URL_ENV: &str = "PINVOU_TOKEN_GATEWAY_URL";
#[cfg(any(debug_assertions, test))]
const PORTAL_URL_ENV: &str = "PINVOU_ACCOUNT_PORTAL_URL";
#[cfg(any(debug_assertions, test))]
const DEFAULT_PORTAL_URL: &str = "https://account.pinvou.com/";
const INSTALLED_CONFIG_RELATIVE_PATH: &str = "config/account-platform.json";
const ACCOUNT_PORTAL_CONFIG_FIELD: &str = "account_portal_url";
const IDENTITY_SERVICE_CONFIG_FIELD: &str = "identity_service_url";
const TOKEN_GATEWAY_CONFIG_FIELD: &str = "token_gateway_url";
const MAX_INSTALLED_CONFIG_BYTES: u64 = 64 * 1024;
const MAX_RESPONSE_BYTES: usize = 2 * 1024 * 1024;
const MAX_REQUEST_BODY_BYTES: usize = 64 * 1024;

#[derive(Debug, Clone)]
struct AccountPlatformConfig {
    token_url: Url,
    client_id: String,
    account_url: Url,
    identity_url: Option<Url>,
    token_gateway_url: Option<Url>,
    portal_url: Url,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct InstalledAccountPlatformConfig {
    schema_version: u32,
    #[serde(default)]
    oidc_issuer: Option<String>,
    #[serde(default)]
    oidc_token_url: Option<String>,
    oidc_client_id: String,
    account_service_url: String,
    #[serde(default)]
    identity_service_url: Option<String>,
    #[serde(default)]
    token_gateway_url: Option<String>,
    account_portal_url: String,
}

impl AccountPlatformConfig {
    fn current() -> Result<Self, String> {
        let installed = Self::from_installed_resource();
        #[cfg(debug_assertions)]
        if installed.is_err() && account_environment_present() {
            return Self::from_env();
        }
        installed
    }

    fn from_installed_resource() -> Result<Self, String> {
        let resource_dir = crate::platform::paths::runtime_resource_dir().ok_or_else(|| {
            "account_platform_configuration_missing: installed resource directory is unavailable"
                .to_string()
        })?;
        let path = resource_dir.join(INSTALLED_CONFIG_RELATIVE_PATH);
        let metadata = std::fs::metadata(&path).map_err(|_| {
            "account_platform_configuration_missing: installed account platform config is unavailable"
                .to_string()
        })?;
        if !metadata.is_file() || metadata.len() == 0 || metadata.len() > MAX_INSTALLED_CONFIG_BYTES
        {
            return Err(
                "account_platform_configuration_invalid: installed account platform config has an invalid size"
                    .to_string(),
            );
        }
        let raw = std::fs::read_to_string(path).map_err(|_| {
            "account_platform_configuration_invalid: installed account platform config is unreadable"
                .to_string()
        })?;
        Self::from_installed_json(&raw)
    }

    fn from_installed_json(raw: &str) -> Result<Self, String> {
        let installed: InstalledAccountPlatformConfig = serde_json::from_str(raw).map_err(|_| {
            "account_platform_configuration_invalid: installed account platform config is invalid"
                .to_string()
        })?;
        if installed.schema_version != 1 {
            return Err(
                "account_platform_configuration_invalid: installed account platform config schema is unsupported"
                    .to_string(),
            );
        }
        let issuer = nonempty(installed.oidc_issuer);
        let token_url = nonempty(installed.oidc_token_url)
            .or_else(|| issuer.as_ref().map(|value| oidc_endpoint(value, "token")))
            .ok_or_else(|| {
                "account_platform_configuration_missing: oidc_token_url or oidc_issuer is required"
                    .to_string()
            })?;
        let client_id = validate_client_id(installed.oidc_client_id)?;
        Ok(Self {
            token_url: parse_endpoint("oidc_token_url", &token_url)?,
            client_id,
            account_url: parse_service_base("account_service_url", &installed.account_service_url)?,
            identity_url: nonempty(installed.identity_service_url)
                .map(|value| parse_service_base("identity_service_url", &value))
                .transpose()?,
            token_gateway_url: nonempty(installed.token_gateway_url)
                .map(|value| parse_service_base("token_gateway_url", &value))
                .transpose()?,
            portal_url: account_portal_url_from(&installed.account_portal_url)?,
        })
    }

    #[cfg(any(debug_assertions, test))]
    fn from_env() -> Result<Self, String> {
        if !platform_supported() {
            return Err(
                "unsupported_platform: desktop OIDC requires Windows, macOS, or Linux".to_string(),
            );
        }

        let issuer = optional_env(ISSUER_ENV);
        let token_url = optional_env(TOKEN_URL_ENV)
            .or_else(|| issuer.as_ref().map(|value| oidc_endpoint(value, "token")))
            .ok_or_else(|| missing_config(TOKEN_URL_ENV, ISSUER_ENV))?;
        let client_id = validate_client_id(required_env(CLIENT_ID_ENV)?)?;

        Ok(Self {
            token_url: parse_endpoint(TOKEN_URL_ENV, &token_url)?,
            client_id,
            account_url: parse_service_base(ACCOUNT_URL_ENV, &required_env(ACCOUNT_URL_ENV)?)?,
            identity_url: optional_env(IDENTITY_URL_ENV)
                .map(|value| parse_service_base(IDENTITY_URL_ENV, &value))
                .transpose()?,
            token_gateway_url: optional_env(TOKEN_GATEWAY_URL_ENV)
                .map(|value| parse_service_base(TOKEN_GATEWAY_URL_ENV, &value))
                .transpose()?,
            portal_url: account_portal_url_from(
                optional_env(PORTAL_URL_ENV)
                    .as_deref()
                    .unwrap_or(DEFAULT_PORTAL_URL),
            )?,
        })
    }
}

#[cfg(debug_assertions)]
fn account_environment_present() -> bool {
    [
        ISSUER_ENV,
        TOKEN_URL_ENV,
        CLIENT_ID_ENV,
        ACCOUNT_URL_ENV,
        IDENTITY_URL_ENV,
        TOKEN_GATEWAY_URL_ENV,
        PORTAL_URL_ENV,
    ]
    .into_iter()
    .any(|name| std::env::var_os(name).is_some())
}

fn nonempty(value: Option<String>) -> Option<String> {
    value
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
}

fn validate_client_id(client_id: String) -> Result<String, String> {
    let client_id = client_id.trim().to_string();
    if client_id.is_empty() || client_id.len() > 200 || client_id.chars().any(char::is_whitespace) {
        return Err(
            "account_platform_configuration_invalid: oidc_client_id is invalid".to_string(),
        );
    }
    Ok(client_id)
}

#[cfg(any(debug_assertions, test))]
fn optional_env(name: &str) -> Option<String> {
    std::env::var(name)
        .ok()
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
}

#[cfg(any(debug_assertions, test))]
fn required_env(name: &str) -> Result<String, String> {
    optional_env(name)
        .ok_or_else(|| format!("account_platform_configuration_missing: {name} is required"))
}

#[cfg(any(debug_assertions, test))]
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
    platform_supported()
}

#[derive(Debug, Deserialize)]
struct PortalSessionHandoffResponse {
    ticket: String,
}

enum PortalSessionHandoffResult {
    Ticket(String),
    Unauthorized,
}

const ACCOUNT_PORTAL_LABEL: &str = "账号平台个人中心";

impl AccountPlatformState {
    pub async fn open_portal(&self, language: Language) -> Result<(), String> {
        let config = AccountPlatformConfig::current()?;
        let target = self.portal_target(&config, language.locale_tag()).await?;
        crate::platform::os::open_target(target.as_str(), ACCOUNT_PORTAL_LABEL)
    }

    async fn portal_target(
        &self,
        config: &AccountPlatformConfig,
        locale: &str,
    ) -> Result<Url, String> {
        let restored = portal_restore_state(self.ensure_restored(config).await)?;
        if !restored {
            return Ok(portal_login_target(&config.portal_url, locale));
        }
        let mut access_token = match self.access_token(config, None).await {
            Ok(access_token) => access_token,
            Err(error) if is_unauthenticated_error(&error) => {
                return Ok(portal_login_target(&config.portal_url, locale));
            }
            Err(error) => return Err(error),
        };

        let generation = self.token_generation(&access_token).await?;
        let result = self
            .request_portal_session_handoff(config, &access_token)
            .await?;
        self.require_generation(generation).await?;
        match result {
            PortalSessionHandoffResult::Ticket(ticket) => {
                return Ok(portal_handoff_target(&config.portal_url, &ticket, locale));
            }
            PortalSessionHandoffResult::Unauthorized => {}
        }

        access_token = match self
            .access_token_in_session(config, Some(&access_token), Some(generation))
            .await
        {
            Ok(access_token) => access_token,
            Err(error) if is_unauthenticated_error(&error) => {
                return Ok(portal_login_target(&config.portal_url, locale));
            }
            Err(error) => return Err(error),
        };
        let result = self
            .request_portal_session_handoff(config, &access_token)
            .await?;
        self.require_generation(generation).await?;
        match result {
            PortalSessionHandoffResult::Ticket(ticket) => {
                Ok(portal_handoff_target(&config.portal_url, &ticket, locale))
            }
            PortalSessionHandoffResult::Unauthorized => {
                Ok(portal_login_target(&config.portal_url, locale))
            }
        }
    }

    async fn request_portal_session_handoff(
        &self,
        config: &AccountPlatformConfig,
        access_token: &str,
    ) -> Result<PortalSessionHandoffResult, String> {
        let endpoint = config
            .portal_url
            .join("portal-session-handoff")
            .map_err(|_| {
                "account_platform_configuration_invalid: cannot resolve portal handoff".to_string()
            })?;
        let response = self
            .client
            .post(endpoint)
            .bearer_auth(&access_token)
            .header(
                crate::platform::client_version::HEADER,
                crate::platform::client_version::CURRENT,
            )
            .send()
            .await
            .map_err(|error| format!("portal_session_handoff_unavailable: {error}"))?;
        if response.status() == StatusCode::UNAUTHORIZED {
            return Ok(PortalSessionHandoffResult::Unauthorized);
        }
        if !response.status().is_success() {
            return Err(format!(
                "portal_session_handoff_failed: identity returned {}",
                response.status()
            ));
        }
        let bytes = bounded_response_bytes(response).await?;
        let handoff =
            serde_json::from_slice::<PortalSessionHandoffResponse>(&bytes).map_err(|_| {
                "portal_session_handoff_invalid: handoff response is invalid".to_string()
            })?;
        if handoff.ticket.trim().is_empty() {
            return Err("portal_session_handoff_invalid: handoff ticket is empty".to_string());
        }
        Ok(PortalSessionHandoffResult::Ticket(handoff.ticket))
    }
}

fn portal_restore_state(result: Result<bool, String>) -> Result<bool, String> {
    match result {
        Ok(restored) => Ok(restored),
        Err(error) if is_unauthenticated_error(&error) => Ok(false),
        Err(error) => Err(error),
    }
}

fn portal_login_target(portal_url: &Url, locale: &str) -> Url {
    let mut target = portal_url.clone();
    target.set_path("auth/login");
    target.set_query(None);
    target.set_fragment(None);
    let return_to = format!("/?locale={locale}");
    target
        .query_pairs_mut()
        .append_pair("force_login", "1")
        .append_pair("return_to", &return_to)
        .append_pair("locale", locale);
    target
}

fn portal_handoff_target(portal_url: &Url, ticket: &str, locale: &str) -> Url {
    let mut target = portal_url.clone();
    target.set_path("portal-session-handoff");
    target.set_query(None);
    target.query_pairs_mut().append_pair("locale", locale);
    target.set_fragment(Some(&format!("ticket={ticket}")));
    target
}

fn account_portal_url_from(value: &str) -> Result<Url, String> {
    let target = parse_endpoint(ACCOUNT_PORTAL_CONFIG_FIELD, value)?;
    if target.query().is_some() || target.fragment().is_some() {
        return Err("account_platform_configuration_invalid: account portal URL must not contain query or fragment".to_string());
    }
    Ok(target)
}

fn is_unauthenticated_error(error: &str) -> bool {
    error.starts_with("authentication_required:")
        || error.starts_with("oidc_refresh_token_missing:")
        || error == "oidc_credentials_invalid: account or password is incorrect"
}

const fn platform_supported() -> bool {
    platform::desktop_oidc_supported()
}

#[derive(Debug)]
struct RuntimeSession {
    access_token: String,
    refresh_token: String,
    expires_at: Instant,
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

#[derive(Debug, Deserialize)]
pub struct LoginCredentials {
    pub account: String,
    pub password: String,
}

#[derive(Debug, Deserialize)]
pub struct PortalOpenRequest {
    pub language: Language,
}

#[derive(Debug, Deserialize)]
pub struct SMSRequest {
    pub username: String,
    pub mobile: String,
    pub password_handle: String,
    pub password_policy_version: String,
    pub purpose: String,
    pub captcha_id: String,
    pub captcha_offset: u16,
}

#[derive(Debug, Deserialize)]
pub struct SliderCaptchaRequest {
    pub mobile: String,
    pub purpose: String,
}

#[derive(Debug, Deserialize)]
pub struct MobileRegistrationPrecheck {
    pub username: String,
    pub mobile: String,
    pub password: String,
}

#[derive(Debug, Deserialize)]
pub struct SMSLoginCredentials {
    pub mobile: String,
    pub challenge_id: String,
    pub code: String,
}

#[derive(Debug, Deserialize)]
pub struct PasswordReset {
    pub mobile: String,
    pub challenge_id: String,
    pub code: String,
    pub password: String,
}

#[derive(Debug, Deserialize)]
pub struct MobileRegistration {
    pub username: String,
    pub mobile: String,
    pub challenge_id: String,
    pub code: String,
    pub password_handle: String,
    pub password_policy_version: String,
    pub registration_operation_id: String,
    #[serde(default)]
    pub registration_query_token: String,
}

#[derive(Debug, Serialize, Deserialize, PartialEq, Eq)]
struct PendingRegistrationCredential {
    operation_id: String,
    query_token: String,
    username: String,
    expires_at: String,
}

#[derive(Debug, Deserialize)]
struct TokenResponse {
    access_token: String,
    refresh_token: Option<String>,
    expires_in: u64,
}

#[derive(Debug, Deserialize)]
struct OAuthErrorResponse {
    error: Option<String>,
    error_description: Option<String>,
}

#[derive(Debug, Clone, Copy)]
enum TokenFlow {
    Password,
    Sms,
    Refresh,
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
    pub body_text: Option<String>,
    pub context_version: Option<u64>,
    pub authorization_version: Option<u64>,
    pub personal_authorization_version: Option<u64>,
    pub platform_authorization_version: Option<u64>,
    pub organization_authorization_version: Option<u64>,
}

pub struct AccountPlatformState {
    session: Mutex<Option<RuntimeSession>>,
    login_guard: Mutex<()>,
    refresh_guard: Mutex<()>,
    registration_guard: std::sync::Mutex<()>,
    client: reqwest::Client,
    credentials: Arc<dyn CredentialStore + Send + Sync>,
    session_generation: AtomicU64,
    device_id: String,
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
            registration_guard: std::sync::Mutex::new(()),
            client,
            credentials: Arc::new(SystemCredentialStore::new()),
            session_generation: AtomicU64::new(0),
            device_id: random_urlsafe(24),
        })
    }

    pub async fn status(&self) -> SessionStatus {
        if !platform_supported() {
            return SessionStatus {
                enabled: true,
                configured: false,
                authenticated: false,
                error_code: Some("unsupported_platform"),
            };
        }
        let config = match AccountPlatformConfig::current() {
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
        let config = AccountPlatformConfig::current()?;
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

    pub async fn send_sms(&self, input: SMSRequest) -> Result<Value, String> {
        let config = AccountPlatformConfig::current()?;
        let purpose = input.purpose.trim().to_ascii_uppercase();
        if purpose != "REGISTER" && purpose != "LOGIN" && purpose != "PASSWORD_RESET" {
            return Err("mobile_auth_request_invalid: invalid SMS purpose".to_string());
        }
        let mut result = self.public_account_request(
            &config,
            "v1/auth/sms/challenges",
            serde_json::json!({
                "username": input.username.trim(), "mobile": input.mobile.trim(),
                "password_handle": input.password_handle, "password_policy_version": input.password_policy_version,
                "purpose": purpose, "captcha_id": input.captcha_id.trim(), "captcha_offset": input.captcha_offset,
            }),
        )
        .await?;
        if purpose == "REGISTER" {
            let operation_id = result
                .get("registration_operation_id")
                .and_then(Value::as_str)
                .unwrap_or_default();
            let query_token = result
                .get("registration_query_token")
                .and_then(Value::as_str)
                .unwrap_or_default();
            let expires_at = result
                .get("registration_query_token_expires_at")
                .and_then(Value::as_str)
                .unwrap_or_default();
            if operation_id.is_empty() || query_token.is_empty() {
                return Err(
                    "mobile_auth_response_invalid: registration recovery credential is missing"
                        .to_string(),
                );
            }
            let credential = PendingRegistrationCredential {
                operation_id: operation_id.to_string(),
                query_token: query_token.to_string(),
                username: input.username.trim().to_string(),
                expires_at: expires_at.to_string(),
            };
            let encoded = serde_json::to_string(&credential).map_err(|_| {
                "mobile_auth_response_invalid: registration recovery credential is invalid"
                    .to_string()
            })?;
            let _registration = self.registration_guard.lock().map_err(|_| {
                "registration_recovery_unavailable: credential lock failed".to_string()
            })?;
            if self
                .credentials
                .set(
                    &CredentialReference::for_account_platform_registration(),
                    &encoded,
                )
                .is_ok()
            {
                if let Some(object) = result.as_object_mut() {
                    object.remove("registration_query_token");
                    object.insert("recovery_persisted".to_string(), Value::Bool(true));
                }
            } else if let Some(object) = result.as_object_mut() {
                object.insert("recovery_persisted".to_string(), Value::Bool(false));
            }
        }
        Ok(result)
    }

    pub async fn create_slider_captcha(
        &self,
        input: SliderCaptchaRequest,
    ) -> Result<Value, String> {
        let config = AccountPlatformConfig::current()?;
        let purpose = input.purpose.trim().to_ascii_uppercase();
        if purpose != "REGISTER" && purpose != "LOGIN" && purpose != "PASSWORD_RESET" {
            return Err("mobile_auth_request_invalid: invalid captcha purpose".to_string());
        }
        self.public_account_request(
            &config,
            "v1/auth/captcha/challenges",
            serde_json::json!({"mobile": input.mobile.trim(), "purpose": purpose}),
        )
        .await
    }

    pub async fn precheck_mobile_registration(
        &self,
        input: MobileRegistrationPrecheck,
    ) -> Result<Value, String> {
        let config = AccountPlatformConfig::current()?;
        let reference = self
            .public_identity_request(
                &config,
                "v1/registration/password-prechecks",
                serde_json::json!({"password": input.password}),
            )
            .await?;
        let password_handle = reference
            .get("password_handle")
            .and_then(Value::as_str)
            .ok_or_else(|| {
                "identity_adapter_response_invalid: password handle is missing".to_string()
            })?;
        let password_policy_version = reference
            .get("password_policy_version")
            .and_then(Value::as_str)
            .ok_or_else(|| {
                "identity_adapter_response_invalid: password policy version is missing".to_string()
            })?;
        let mut result = self.public_account_request(
            &config, "v1/auth/registration/precheck",
            serde_json::json!({"username": input.username.trim(), "mobile": input.mobile.trim(),
                "password_handle": password_handle, "password_policy_version": password_policy_version}),
        ).await?;
        if let (Some(result), Some(reference)) = (result.as_object_mut(), reference.as_object()) {
            for key in ["password_handle", "password_policy_version", "expires_at"] {
                if let Some(value) = reference.get(key) {
                    result.insert(key.to_string(), value.clone());
                }
            }
        }
        Ok(result)
    }

    pub async fn register_mobile(&self, input: MobileRegistration) -> Result<Value, String> {
        let config = AccountPlatformConfig::current()?;
        let query_token = if input.registration_query_token.trim().is_empty() {
            let pending = self.pending_registration()?.ok_or_else(|| {
                "registration_recovery_missing: pending registration credential is unavailable"
                    .to_string()
            })?;
            if pending.operation_id != input.registration_operation_id.trim() {
                return Err(
                    "registration_recovery_conflict: pending registration operation changed"
                        .to_string(),
                );
            }
            pending.query_token
        } else {
            input.registration_query_token.trim().to_string()
        };
        self.public_account_request(
            &config,
            "v1/auth/register",
            serde_json::json!({
                "username": input.username.trim(), "mobile": input.mobile.trim(), "challenge_id": input.challenge_id.trim(),
                "code": input.code.trim(), "password_handle": input.password_handle,
                "password_policy_version": input.password_policy_version,
                "registration_operation_id": input.registration_operation_id.trim(),
                "registration_query_token": query_token,
            }),
        )
        .await
    }

    pub async fn reset_password(&self, input: PasswordReset) -> Result<Value, String> {
        let config = AccountPlatformConfig::current()?;
        let reference = self
            .public_identity_request(
                &config,
                "v1/registration/password-prechecks",
                serde_json::json!({"password": input.password}),
            )
            .await?;
        self.public_account_request(
            &config,
            "v1/auth/password-resets",
            serde_json::json!({
                "mobile": input.mobile.trim(), "challenge_id": input.challenge_id.trim(), "code": input.code.trim(),
                "password_handle": reference.get("password_handle").and_then(Value::as_str).unwrap_or_default(),
                "password_policy_version": reference.get("password_policy_version").and_then(Value::as_str).unwrap_or_default(),
            }),
        ).await
    }

    pub async fn registration_status(&self) -> Result<Value, String> {
        let Some(pending) = self.pending_registration()? else {
            return Ok(Value::Null);
        };
        let config = AccountPlatformConfig::current()?;
        let mut result = match self
            .public_account_registration_status(
                &config,
                &pending.operation_id,
                &pending.query_token,
            )
            .await
        {
            Ok(result) => result,
            Err(error) if error.starts_with("registration_recovery_not_found:") => {
                self.clear_pending_registration(&pending)?;
                return Err(
                    "registration_recovery_unavailable: registration operation cannot be queried"
                        .to_string(),
                );
            }
            Err(error) => return Err(error),
        };
        if let Some(object) = result.as_object_mut() {
            object.insert(
                "username".to_string(),
                Value::String(pending.username.clone()),
            );
        }
        if matches!(
            result.get("status").and_then(Value::as_str),
            Some("ACTIVE" | "COMPENSATED" | "EXPIRED")
        ) {
            self.clear_pending_registration(&pending)?;
        }
        Ok(result)
    }

    fn clear_pending_registration(
        &self,
        expected: &PendingRegistrationCredential,
    ) -> Result<(), String> {
        let _registration = self
            .registration_guard
            .lock()
            .map_err(|_| "registration_recovery_unavailable: credential lock failed".to_string())?;
        if self.pending_registration()?.as_ref() == Some(expected) {
            self.credentials
                .delete(&CredentialReference::for_account_platform_registration())
                .map_err(credential_store_error)?;
        }
        Ok(())
    }

    fn pending_registration(&self) -> Result<Option<PendingRegistrationCredential>, String> {
        let Some(encoded) = self
            .credentials
            .get(&CredentialReference::for_account_platform_registration())
            .map_err(credential_store_error)?
        else {
            return Ok(None);
        };
        serde_json::from_str(&encoded).map(Some).map_err(|_| {
            "registration_recovery_invalid: stored registration credential is invalid".to_string()
        })
    }

    pub async fn login_sms(
        &self,
        credentials: SMSLoginCredentials,
    ) -> Result<SessionStatus, String> {
        let _login = self.login_guard.lock().await;
        let config = AccountPlatformConfig::current()?;
        let _refresh = self.refresh_guard.lock().await;
        self.clear_authentication().await?;
        let form = [
            ("grant_type", "password"),
            ("client_id", config.client_id.as_str()),
            ("username", mainland_username(credentials.mobile.trim())),
            ("sms_challenge_id", credentials.challenge_id.trim()),
            ("sms_code", credentials.code.trim()),
            ("scope", "openid profile email offline_access"),
        ];
        let tokens = self.token_request(&config, &form, TokenFlow::Sms).await?;
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
        let config = AccountPlatformConfig::current()?;
        let prepared = prepare_request(&config, input)?;
        let mut token = self.access_token(&config, None).await?;
        let generation = self.token_generation(&token).await?;
        let mut response = self.send(&prepared, &token).await?;
        if response.status() == StatusCode::UNAUTHORIZED {
            token = self
                .access_token_in_session(&config, Some(&token), Some(generation))
                .await?;
            response = self.send(&prepared, &token).await?;
        }
        let api_response = response_to_api(response).await?;
        self.require_generation(generation).await?;
        if prepared.is_account_bootstrap() && (200..300).contains(&api_response.status) {
            let user_id = bootstrap_user_id(&api_response.body)?;
            self.remember_account_identity(&token, user_id).await?;
        }
        Ok(api_response)
    }

    async fn remember_account_identity(
        &self,
        access_token: &str,
        user_id: String,
    ) -> Result<(), String> {
        let mut session = self.session.lock().await;
        let current = session
            .as_mut()
            .filter(|session| session.access_token == access_token)
            .ok_or_else(|| "authentication_required: account session changed".to_string())?;
        current.account_user_id = Some(user_id);
        Ok(())
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
            .header("X-Device-ID", &self.device_id)
            .form(form)
            .send()
            .await
            .map_err(|error| format!("oidc_token_endpoint_unavailable: {error}"))?;
        let status = response.status();
        let bytes = bounded_response_bytes(response).await?;
        if !status.is_success() {
            let oauth_error = serde_json::from_slice::<OAuthErrorResponse>(&bytes).ok();
            return Err(classify_token_error(
                flow,
                status,
                oauth_error.as_ref().and_then(|body| body.error.as_deref()),
                oauth_error
                    .as_ref()
                    .and_then(|body| body.error_description.as_deref()),
            ));
        }
        serde_json::from_slice::<TokenResponse>(&bytes)
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
        let expires_at = Instant::now().checked_add(lifetime).ok_or_else(|| {
            "oidc_token_response_invalid: expiry cannot be represented".to_string()
        })?;
        let generation = if previous_refresh_token.is_some() {
            self.session
                .lock()
                .await
                .as_ref()
                .map(|session| session.generation)
                .unwrap_or_else(|| self.session_generation.load(Ordering::Relaxed))
        } else {
            self.session_generation
                .fetch_add(1, Ordering::Relaxed)
                .saturating_add(1)
        };
        self.credentials
            .set(
                &CredentialReference::for_account_platform_refresh_token(),
                &refresh_token,
            )
            .map_err(credential_store_error)?;
        let account_user_id = self
            .session
            .lock()
            .await
            .as_ref()
            .and_then(|session| session.account_user_id.clone());
        *self.session.lock().await = Some(RuntimeSession {
            access_token: access_token.clone(),
            refresh_token,
            expires_at,
            generation,
            account_user_id,
        });
        Ok(access_token)
    }

    async fn access_token(
        &self,
        config: &AccountPlatformConfig,
        rejected_access_token: Option<&str>,
    ) -> Result<String, String> {
        self.access_token_in_session(config, rejected_access_token, None)
            .await
    }

    async fn token_generation(&self, access_token: &str) -> Result<u64, String> {
        self.session
            .lock()
            .await
            .as_ref()
            .filter(|session| session.access_token == access_token)
            .map(|session| session.generation)
            .ok_or_else(|| "authentication_required: account session changed".to_string())
    }

    async fn require_generation(&self, expected: u64) -> Result<(), String> {
        if self
            .session
            .lock()
            .await
            .as_ref()
            .is_some_and(|session| session.generation == expected)
        {
            Ok(())
        } else {
            Err("authentication_required: account session changed".to_string())
        }
    }

    async fn access_token_in_session(
        &self,
        config: &AccountPlatformConfig,
        rejected_access_token: Option<&str>,
        expected: Option<u64>,
    ) -> Result<String, String> {
        if let Some(generation) = expected {
            self.require_generation(generation).await?;
        }
        if let Some(session) = self.session.lock().await.as_ref() {
            if expected.is_some_and(|generation| generation != session.generation) {
                return Err("authentication_required: account session changed".to_string());
            }
            if access_token_is_usable(session, rejected_access_token) {
                return Ok(session.access_token.clone());
            }
        }
        let _refresh = self.refresh_guard.lock().await;
        if let Some(generation) = expected {
            self.require_generation(generation).await?;
        }
        if let Some(session) = self.session.lock().await.as_ref() {
            if expected.is_some_and(|generation| generation != session.generation) {
                return Err("authentication_required: account session changed".to_string());
            }
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
                if refresh_failure_invalidates_authentication(&error) {
                    self.clear_authentication().await?;
                }
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
                if refresh_failure_invalidates_authentication(&error) {
                    self.clear_authentication().await?;
                }
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
        self.session_generation.fetch_add(1, Ordering::Relaxed);
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
            .header("X-Device-ID", &self.device_id)
            .header("Accept", "application/json")
            .header(
                crate::platform::client_version::HEADER,
                crate::platform::client_version::CURRENT,
            )
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

    async fn public_account_request(
        &self,
        config: &AccountPlatformConfig,
        path: &str,
        body: Value,
    ) -> Result<Value, String> {
        let url = config.account_url.join(path).map_err(|_| {
            "account_platform_configuration_invalid: cannot resolve account endpoint".to_string()
        })?;
        let operation = account_public_operation(path);
        let request_id = random_urlsafe(18);
        log::info!(
            "[account_platform_auth] request_started operation={operation} request_id={request_id}"
        );
        let response = self
            .client
            .post(url)
            .header("Accept", "application/json")
            .header(
                crate::platform::client_version::HEADER,
                crate::platform::client_version::CURRENT,
            )
            .header("X-Request-ID", &request_id)
            .header("X-Device-ID", &self.device_id)
            .json(&body)
            .send()
            .await
            .map_err(|error| {
                let category = transport_error_category(&error);
                log::warn!(
                    "[account_platform_auth] request_failed operation={operation} request_id={request_id} phase=transport category={category}"
                );
                account_platform_transport_error(&error)
            })?;
        let status = response.status().as_u16();
        let response = response_to_api(response).await.map_err(|error| {
            log::warn!(
                "[account_platform_auth] request_failed operation={operation} request_id={request_id} phase=response_decode status={status}"
            );
            error
        })?;
        if !(200..300).contains(&response.status) {
            let code = response
                .body
                .get("code")
                .and_then(Value::as_str)
                .unwrap_or("MOBILE_AUTH_FAILED");
            let retry_after = response
                .body
                .pointer("/details/retry_after_seconds")
                .and_then(Value::as_u64)
                .map(|seconds| format!(":{seconds}"))
                .unwrap_or_default();
            log::warn!(
                "[account_platform_auth] request_rejected operation={operation} request_id={request_id} status={} code={code}",
                response.status
            );
            return Err(format!("mobile_auth_failed: {code}{retry_after}"));
        }
        log::info!(
            "[account_platform_auth] request_succeeded operation={operation} request_id={request_id} status={}",
            response.status
        );
        Ok(response.body)
    }

    async fn public_identity_request(
        &self,
        config: &AccountPlatformConfig,
        path: &str,
        body: Value,
    ) -> Result<Value, String> {
        let identity_url = config.identity_url.as_ref().ok_or_else(|| {
            format!(
                "account_platform_configuration_missing: {IDENTITY_SERVICE_CONFIG_FIELD} is required for registration"
            )
        })?;
        let url = identity_url.join(path).map_err(|_| {
            "account_platform_configuration_invalid: cannot resolve identity endpoint".to_string()
        })?;
        let response = self
            .client
            .post(url)
            .header("Accept", "application/json")
            .header(
                crate::platform::client_version::HEADER,
                crate::platform::client_version::CURRENT,
            )
            .header("X-Request-ID", random_urlsafe(18))
            .json(&body)
            .send()
            .await
            .map_err(|error| format!("identity_adapter_unavailable: {error}"))?;
        let response = response_to_api(response).await?;
        if !(200..300).contains(&response.status) {
            let code = response
                .body
                .get("code")
                .and_then(Value::as_str)
                .unwrap_or("IDENTITY_REQUEST_FAILED");
            return Err(format!("mobile_auth_failed: {code}"));
        }
        Ok(response.body)
    }

    async fn public_account_registration_status(
        &self,
        config: &AccountPlatformConfig,
        operation_id: &str,
        query_token: &str,
    ) -> Result<Value, String> {
        let url = config
            .account_url
            .join(&format!("v1/auth/registration-operations/{operation_id}"))
            .map_err(|_| "account_platform_configuration_invalid: cannot resolve registration status endpoint".to_string())?;
        let response = self
            .client
            .get(url)
            .header("Accept", "application/json")
            .header(
                crate::platform::client_version::HEADER,
                crate::platform::client_version::CURRENT,
            )
            .header("X-Request-ID", random_urlsafe(18))
            .header("X-Device-ID", &self.device_id)
            .header("X-Registration-Query-Token", query_token)
            .send()
            .await
            .map_err(|error| format!("account_platform_service_unavailable: {error}"))?;
        let response = response_to_api(response).await?;
        if !(200..300).contains(&response.status) {
            if response.status == 404 {
                return Err(
                    "registration_recovery_not_found: registration operation cannot be queried"
                        .to_string(),
                );
            }
            return Err(
                "registration_recovery_unavailable: registration operation cannot be queried"
                    .to_string(),
            );
        }
        Ok(response.body)
    }
}

fn access_token_is_usable(session: &RuntimeSession, rejected: Option<&str>) -> bool {
    session.expires_at > Instant::now() + Duration::from_secs(30)
        && rejected.is_none_or(|token| token != session.access_token)
}

fn refresh_failure_invalidates_authentication(error: &str) -> bool {
    error.starts_with("authentication_required:")
}

fn validate_token_response(tokens: TokenResponse) -> Result<TokenResponse, String> {
    if tokens.access_token.trim().is_empty() || tokens.expires_in == 0 {
        return Err("oidc_token_response_invalid: access token is missing or expired".to_string());
    }
    Ok(tokens)
}

fn classify_token_error(
    flow: TokenFlow,
    status: StatusCode,
    oauth_error: Option<&str>,
    oauth_description: Option<&str>,
) -> String {
    let account_setup_incomplete = oauth_error == Some("invalid_grant")
        && matches!(flow, TokenFlow::Password | TokenFlow::Sms)
        && oauth_description.is_some_and(|description| {
            let description = description.to_ascii_lowercase();
            description.contains("account is not fully set up")
                || description.contains("resolve_required_actions")
        });
    if account_setup_incomplete {
        return "oidc_account_setup_incomplete: account profile requires administrator attention"
            .to_string();
    }
    match (flow, oauth_error) {
        (TokenFlow::Password, Some("invalid_grant")) => {
            "oidc_credentials_invalid: account or password is incorrect".to_string()
        }
        (TokenFlow::Password, Some("unauthorized_client")) => {
            "oidc_direct_grant_disabled: client configuration is invalid or in-app password sign-in is disabled"
                .to_string()
        }
        (TokenFlow::Sms, Some("invalid_grant")) => {
            "mobile_auth_code_invalid: SMS code is invalid or expired".to_string()
        }
        (TokenFlow::Sms, Some("unauthorized_client")) => {
            "oidc_direct_grant_disabled: client configuration is invalid or in-app SMS sign-in is disabled"
                .to_string()
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
        "oidc_account_setup_incomplete" => "oidc_account_setup_incomplete",
        _ => "authentication_required",
    }
}

fn credential_store_error(error: impl std::fmt::Display) -> String {
    format!("credential_store_unavailable: {error}")
}

fn account_public_operation(path: &str) -> &'static str {
    match path {
        "v1/auth/captcha/challenges" => "captcha_challenge",
        "v1/auth/sms/challenges" => "sms_challenge",
        "v1/auth/registration/precheck" => "registration_precheck",
        "v1/auth/register" => "mobile_registration",
        "v1/auth/password-resets" => "password_reset",
        _ => "public_account_request",
    }
}

fn transport_error_category(error: &reqwest::Error) -> &'static str {
    if error.is_timeout() {
        "timeout"
    } else if error.is_connect() {
        "connect"
    } else if error.is_builder() {
        "request_builder"
    } else if error.is_redirect() {
        "redirect"
    } else {
        "request"
    }
}

fn account_platform_transport_error(error: &reqwest::Error) -> String {
    let category = transport_error_category(error);
    let cause = std::error::Error::source(error)
        .map(ToString::to_string)
        .unwrap_or_else(|| "no additional transport detail".to_string());
    format!("account_platform_service_unavailable: {category}: {error}; cause={cause}")
}

fn random_urlsafe(bytes: usize) -> String {
    let mut value = vec![0_u8; bytes];
    rand::rng().fill_bytes(&mut value);
    URL_SAFE_NO_PAD.encode(value)
}

fn mainland_username(value: &str) -> &str {
    value
        .strip_prefix("+86")
        .or_else(|| value.strip_prefix("86"))
        .unwrap_or(value)
}

#[derive(Debug)]
struct PreparedRequest {
    method: Method,
    url: Url,
    body: Option<Value>,
    idempotency_key: Option<String>,
}

impl PreparedRequest {
    fn is_account_bootstrap(&self) -> bool {
        self.method == Method::POST && self.url.path().ends_with("/v1/account/bootstrap")
    }
}

fn bootstrap_user_id(body: &Value) -> Result<String, String> {
    body.get("user_id")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|value| !value.is_empty() && value.len() <= 128)
        .map(str::to_string)
        .ok_or_else(|| {
            "account_platform_response_invalid: account bootstrap omitted user_id".to_string()
        })
}

fn prepare_request(
    config: &AccountPlatformConfig,
    input: ApiRequest,
) -> Result<PreparedRequest, String> {
    let method = Method::from_bytes(input.method.as_bytes())
        .map_err(|_| "account_platform_request_invalid: invalid method".to_string())?;
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
        "token" => config.token_gateway_url.as_ref().ok_or_else(|| {
            format!(
                "account_platform_configuration_missing: {TOKEN_GATEWAY_CONFIG_FIELD} is required"
            )
        })?,
        _ => return Err("account_platform_request_denied: unknown service area".to_string()),
    };
    if method == Method::GET && input.body.is_some() {
        return Err("account_platform_request_invalid: GET body is not allowed".to_string());
    }
    if input.idempotency_key.is_some() {
        return Err(
            "account_platform_request_denied: idempotency key is not allowed for this route"
                .to_string(),
        );
    }
    if let Some(body) = &input.body {
        let size = serde_json::to_vec(body)
            .map_err(|_| "account_platform_request_invalid: invalid JSON body".to_string())?
            .len();
        if size > MAX_REQUEST_BODY_BYTES {
            return Err("account_platform_request_invalid: request body is too large".to_string());
        }
    }
    let mut url = base
        .join(parsed.path().trim_start_matches('/'))
        .map_err(|_| "account_platform_request_invalid: cannot resolve resource".to_string())?;
    url.set_query(parsed.query());
    Ok(PreparedRequest {
        method,
        url,
        body: input.body,
        idempotency_key: None,
    })
}

fn route_allowed(area: &str, method: &Method, path: &str, query: Option<&str>) -> bool {
    if area == "token" {
        return *method == Method::GET
            && path == "/v1/me/balance"
            && matches!(query, None | Some("consistency=strong"));
    }
    if area != "account" || query.is_some() {
        return false;
    }
    match (method, path) {
        (&Method::GET, "/v1/me" | "/v1/me/authorization-context") => true,
        (
            &Method::POST,
            "/v1/account/bootstrap"
            | "/v1/auth/captcha/challenges"
            | "/v1/me/profile-change-requests"
            | "/v1/me/password-changes",
        ) => true,
        _ => {
            let segments: Vec<_> = path.split('/').collect();
            match (method, segments.as_slice()) {
                (&Method::GET, ["", "v1", "me", "profile-change-requests", id])
                | (
                    &Method::POST,
                    [
                        "",
                        "v1",
                        "me",
                        "profile-change-requests",
                        id,
                        "mobile-challenges",
                    ],
                )
                | (
                    &Method::POST,
                    [
                        "",
                        "v1",
                        "me",
                        "profile-change-requests",
                        id,
                        "actions",
                        "complete",
                    ],
                ) => uuid::Uuid::parse_str(id).is_ok(),
                _ => false,
            }
        }
    }
}

async fn bounded_response_bytes(mut response: reqwest::Response) -> Result<Vec<u8>, String> {
    if response
        .content_length()
        .is_some_and(|length| length > MAX_RESPONSE_BYTES as u64)
    {
        return Err("account_platform_response_too_large: response exceeded 2 MiB".to_string());
    }
    let mut bytes = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|_| "account_platform_response_invalid: response body unavailable".to_string())?
    {
        if chunk.len() > MAX_RESPONSE_BYTES - bytes.len() {
            return Err("account_platform_response_too_large: response exceeded 2 MiB".to_string());
        }
        bytes.extend_from_slice(&chunk);
    }
    Ok(bytes)
}

async fn response_to_api(response: reqwest::Response) -> Result<ApiResponse, String> {
    let balance_response = response.url().path().ends_with("/v1/me/balance");
    let status = response.status().as_u16();
    let context_version = response
        .headers()
        .get("X-Context-Version")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<u64>().ok());
    let authorization_version = response
        .headers()
        .get("X-Authorization-Version")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<u64>().ok());
    let personal_authorization_version = response
        .headers()
        .get("X-Personal-Authorization-Version")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<u64>().ok());
    let platform_authorization_version = response
        .headers()
        .get("X-Platform-Authorization-Version")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<u64>().ok());
    let organization_authorization_version = response
        .headers()
        .get("X-Organization-Authorization-Version")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<u64>().ok());
    let bytes = bounded_response_bytes(response).await?;
    let body = if bytes.is_empty() {
        Value::Null
    } else {
        serde_json::from_slice(&bytes)
            .map_err(|_| "account_platform_response_invalid: expected JSON response".to_string())?
    };
    Ok(ApiResponse {
        status,
        body,
        body_text: if balance_response {
            Some(String::from_utf8(bytes.to_vec()).map_err(|_| {
                "account_platform_response_invalid: expected UTF-8 JSON".to_string()
            })?)
        } else {
            None
        },
        context_version,
        authorization_version,
        personal_authorization_version,
        platform_authorization_version,
        organization_authorization_version,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::sync::Mutex as StdMutex;
    use std::thread;

    #[derive(Default)]
    struct MemoryCredentialStore {
        values: StdMutex<HashMap<(String, String), String>>,
    }

    impl MemoryCredentialStore {
        fn with_refresh_token(value: &str) -> Arc<Self> {
            let store = Arc::new(Self::default());
            store
                .set(
                    &CredentialReference::for_account_platform_refresh_token(),
                    value,
                )
                .unwrap();
            store
        }

        fn refresh_token(&self) -> Option<String> {
            self.get(&CredentialReference::for_account_platform_refresh_token())
                .unwrap()
        }
    }

    impl CredentialStore for MemoryCredentialStore {
        fn get(
            &self,
            reference: &CredentialReference,
        ) -> Result<Option<String>, crate::platform::credential_store::CredentialError> {
            Ok(self
                .values
                .lock()
                .unwrap()
                .get(&(reference.service.clone(), reference.account.clone()))
                .cloned())
        }

        fn set(
            &self,
            reference: &CredentialReference,
            value: &str,
        ) -> Result<(), crate::platform::credential_store::CredentialError> {
            self.values.lock().unwrap().insert(
                (reference.service.clone(), reference.account.clone()),
                value.to_string(),
            );
            Ok(())
        }

        fn delete(
            &self,
            reference: &CredentialReference,
        ) -> Result<(), crate::platform::credential_store::CredentialError> {
            self.values
                .lock()
                .unwrap()
                .remove(&(reference.service.clone(), reference.account.clone()));
            Ok(())
        }
    }

    fn spawn_portal_fixture(
        expected_requests: usize,
        mut respond: impl FnMut(&str) -> (u16, &'static str) + Send + 'static,
    ) -> (Url, thread::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let handle = thread::spawn(move || {
            for _ in 0..expected_requests {
                let (mut stream, _) = listener.accept().unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                let mut request = Vec::new();
                let mut buffer = [0_u8; 4096];
                loop {
                    let count = stream.read(&mut buffer).unwrap();
                    if count == 0 {
                        break;
                    }
                    request.extend_from_slice(&buffer[..count]);
                    let Some(header_end) = request.windows(4).position(|part| part == b"\r\n\r\n")
                    else {
                        continue;
                    };
                    let headers = String::from_utf8_lossy(&request[..header_end + 4]);
                    let content_length = headers
                        .lines()
                        .find_map(|line| {
                            let (name, value) = line.split_once(':')?;
                            name.eq_ignore_ascii_case("content-length")
                                .then(|| value.trim().parse::<usize>().ok())
                                .flatten()
                        })
                        .unwrap_or(0);
                    if request.len() >= header_end + 4 + content_length {
                        break;
                    }
                }
                let request = String::from_utf8(request).unwrap();
                let (status, body) = respond(&request);
                let reason = match status {
                    200 => "OK",
                    400 => "Bad Request",
                    401 => "Unauthorized",
                    503 => "Service Unavailable",
                    _ => "Fixture",
                };
                write!(
                    stream,
                    "HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                )
                .unwrap();
            }
        });
        (Url::parse(&format!("http://{address}/")).unwrap(), handle)
    }

    fn portal_test_state(
        credentials: Arc<MemoryCredentialStore>,
        session: Option<RuntimeSession>,
    ) -> AccountPlatformState {
        AccountPlatformState {
            session: Mutex::new(session),
            login_guard: Mutex::new(()),
            refresh_guard: Mutex::new(()),
            registration_guard: std::sync::Mutex::new(()),
            client: reqwest::Client::builder()
                .redirect(reqwest::redirect::Policy::none())
                .build()
                .unwrap(),
            credentials,
            session_generation: AtomicU64::new(0),
            device_id: "fixture-device".to_string(),
        }
    }

    fn portal_runtime_session(access: &str, refresh: &str) -> RuntimeSession {
        RuntimeSession {
            access_token: access.to_string(),
            refresh_token: refresh.to_string(),
            expires_at: Instant::now() + Duration::from_secs(300),
            generation: 1,
            account_user_id: None,
        }
    }

    struct AccountEnvSnapshot(Vec<(&'static str, Option<std::ffi::OsString>)>);

    impl AccountEnvSnapshot {
        fn take() -> Self {
            Self(
                [
                    ISSUER_ENV,
                    TOKEN_URL_ENV,
                    CLIENT_ID_ENV,
                    ACCOUNT_URL_ENV,
                    IDENTITY_URL_ENV,
                    TOKEN_GATEWAY_URL_ENV,
                    PORTAL_URL_ENV,
                ]
                .into_iter()
                .map(|key| (key, std::env::var_os(key)))
                .collect(),
            )
        }
    }

    impl Drop for AccountEnvSnapshot {
        fn drop(&mut self) {
            for (key, value) in &self.0 {
                // SAFETY: every test using this guard holds the process-wide
                // environment lock until after the snapshot is dropped.
                unsafe {
                    match value {
                        Some(value) => std::env::set_var(key, value),
                        None => std::env::remove_var(key),
                    }
                }
            }
        }
    }

    fn config() -> AccountPlatformConfig {
        AccountPlatformConfig {
            token_url: Url::parse("https://id.example.test/token").unwrap(),
            identity_url: Some(Url::parse("https://id.example.test/").unwrap()),
            client_id: "desktop-client".to_string(),
            account_url: Url::parse("https://account.example.test/").unwrap(),
            token_gateway_url: Some(Url::parse("https://token.example.test/").unwrap()),
            portal_url: Url::parse("https://portal.example.test/").unwrap(),
        }
    }

    #[test]
    fn account_platform_is_the_only_supported_desktop_path() {
        assert_eq!(account_platform_enabled(), platform_supported());
    }

    #[test]
    fn installed_configuration_contains_the_canonical_production_endpoints() {
        let config = AccountPlatformConfig::from_installed_json(include_str!(
            "../../../resources/common/config/account-platform.json"
        ))
        .unwrap();
        assert_eq!(config.client_id, "pinvou-desktop");
        assert_eq!(
            config.token_url.as_str(),
            "https://account.pinvou.com/realms/pinvou/protocol/openid-connect/token"
        );
        assert_eq!(
            config.account_url.as_str(),
            "https://account.pinvou.com/account/"
        );
        assert_eq!(
            config.identity_url.as_ref().map(Url::as_str),
            Some("https://account.pinvou.com/identity/")
        );
        assert_eq!(
            config.token_gateway_url.as_ref().map(Url::as_str),
            Some("https://account.pinvou.com/token/")
        );
        assert_eq!(config.portal_url.as_str(), "https://account.pinvou.com/");
    }

    #[test]
    fn installed_configuration_rejects_unknown_or_insecure_fields() {
        let unknown = r#"{"schema_version":1,"oidc_issuer":"https://id.example.test/realms/pinvou","oidc_client_id":"desktop","account_service_url":"https://account.example.test","account_portal_url":"https://account.example.test","unexpected":true}"#;
        assert!(AccountPlatformConfig::from_installed_json(unknown).is_err());

        let insecure = r#"{"schema_version":1,"oidc_issuer":"http://id.example.test/realms/pinvou","oidc_client_id":"desktop","account_service_url":"https://account.example.test","account_portal_url":"https://account.example.test"}"#;
        assert!(AccountPlatformConfig::from_installed_json(insecure).is_err());
    }

    #[test]
    fn token_errors_distinguish_password_and_saved_session_failures() {
        assert!(
            classify_token_error(
                TokenFlow::Password,
                StatusCode::BAD_REQUEST,
                Some("invalid_grant"),
                None,
            )
            .starts_with("oidc_credentials_invalid:")
        );
        let unauthorized_client = classify_token_error(
            TokenFlow::Password,
            StatusCode::UNAUTHORIZED,
            Some("unauthorized_client"),
            None,
        );
        assert!(unauthorized_client.starts_with("oidc_direct_grant_disabled:"));
        assert!(unauthorized_client.contains("client configuration is invalid"));
        assert!(
            classify_token_error(
                TokenFlow::Refresh,
                StatusCode::BAD_REQUEST,
                Some("invalid_grant"),
                None,
            )
            .starts_with("authentication_required:")
        );
        assert!(
            classify_token_error(
                TokenFlow::Password,
                StatusCode::BAD_REQUEST,
                Some("invalid_grant"),
                Some("Account is not fully set up"),
            )
            .starts_with("oidc_account_setup_incomplete:")
        );
    }

    #[test]
    fn public_account_diagnostics_use_fixed_non_sensitive_operation_names() {
        assert_eq!(
            account_public_operation("v1/auth/sms/challenges"),
            "sms_challenge"
        );
        assert_eq!(
            account_public_operation("v1/auth/captcha/challenges"),
            "captcha_challenge"
        );
        assert_eq!(
            account_public_operation("unexpected"),
            "public_account_request"
        );
    }

    #[test]
    fn authentication_configuration_does_not_require_token_gateway() {
        let _lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let _snapshot = AccountEnvSnapshot::take();
        // SAFETY: the process-wide environment lock is held for this test.
        unsafe {
            std::env::set_var(ISSUER_ENV, "https://identity.example.test/realms/pinvou");
            std::env::remove_var(TOKEN_URL_ENV);
            std::env::set_var(CLIENT_ID_ENV, "desktop-client");
            std::env::set_var(ACCOUNT_URL_ENV, "https://account.example.test/account/");
            std::env::remove_var(IDENTITY_URL_ENV);
            std::env::remove_var(TOKEN_GATEWAY_URL_ENV);
            std::env::remove_var(PORTAL_URL_ENV);
        }

        let config = AccountPlatformConfig::from_env().unwrap();
        assert_eq!(
            config.token_url.as_str(),
            "https://identity.example.test/realms/pinvou/protocol/openid-connect/token"
        );
        assert!(config.token_gateway_url.is_none());

        let error = prepare_request(
            &config,
            ApiRequest {
                area: "token".to_string(),
                resource: "/v1/me/balance?consistency=strong".to_string(),
                method: "GET".to_string(),
                body: None,
                idempotency_key: None,
            },
        )
        .unwrap_err();
        assert_eq!(
            error,
            "account_platform_configuration_missing: token_gateway_url is required"
        );
    }

    #[test]
    fn request_proxy_is_a_narrow_allowlist() {
        for (area, method, path) in [
            ("account", "POST", "/v1/account/bootstrap"),
            ("account", "GET", "/v1/me"),
            ("account", "POST", "/v1/me/profile-change-requests"),
            ("account", "POST", "/v1/auth/captcha/challenges"),
            ("token", "GET", "/v1/me/balance?consistency=strong"),
        ] {
            assert!(
                prepare_request(
                    &config(),
                    ApiRequest {
                        area: area.into(),
                        method: method.into(),
                        resource: path.into(),
                        body: None,
                        idempotency_key: None
                    }
                )
                .is_ok()
            );
        }
        for path in [
            "https://evil.example/v1/me/balance",
            "/v1/me/balance?consistency=eventual",
            "/v1/models",
            "/v1/recharge/products",
            "/v1/chat/completions",
            "/v1/me/usage-requests",
            "/metrics",
        ] {
            assert!(
                prepare_request(
                    &config(),
                    ApiRequest {
                        area: "token".into(),
                        method: "GET".into(),
                        resource: path.into(),
                        body: None,
                        idempotency_key: None
                    }
                )
                .is_err()
            );
        }
        for path in [
            "/v1/me/organizations",
            "/v1/sessions/current/active-context",
            "/admin/v1/users",
            "/v1/me/account-closure",
            "/v1/me/profile-change-requests/not-a-uuid",
        ] {
            assert!(
                prepare_request(
                    &config(),
                    ApiRequest {
                        area: "account".into(),
                        method: "GET".into(),
                        resource: path.into(),
                        body: None,
                        idempotency_key: None
                    }
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

    #[test]
    fn account_portal_is_fixed_and_never_carries_credentials() {
        assert_eq!(
            account_portal_url_from("https://account.pinvou.com/")
                .unwrap()
                .as_str(),
            "https://account.pinvou.com/"
        );
        assert!(account_portal_url_from("https://portal.example.test/?token=secret").is_err());
        assert!(account_portal_url_from("https://user:secret@portal.example.test/").is_err());
    }

    #[test]
    fn portal_open_targets_carry_locale_and_keep_the_ticket_out_of_the_query() {
        let portal = Url::parse("https://account.pinvou.com/").unwrap();
        let login = portal_login_target(&portal, "ja");
        assert_eq!(login.path(), "/auth/login");
        assert_eq!(
            login
                .query_pairs()
                .find(|(key, _)| key == "locale")
                .unwrap()
                .1,
            "ja"
        );
        assert_eq!(
            login
                .query_pairs()
                .find(|(key, _)| key == "return_to")
                .unwrap()
                .1,
            "/?locale=ja"
        );
        assert!(login.fragment().is_none());

        let handoff = portal_handoff_target(&portal, "one-time-ticket", "en");
        assert_eq!(
            handoff.as_str(),
            "https://account.pinvou.com/portal-session-handoff?locale=en#ticket=one-time-ticket"
        );
        assert_eq!(handoff.query(), Some("locale=en"));
    }

    #[test]
    fn portal_open_request_accepts_only_supported_desktop_languages() {
        let request: PortalOpenRequest = serde_json::from_str(r#"{"language":"ja"}"#).unwrap();
        assert_eq!(request.language, Language::Ja);
        assert!(serde_json::from_str::<PortalOpenRequest>(r#"{"language":"fr"}"#).is_err());
    }

    #[test]
    fn portal_restore_falls_back_only_for_definite_authentication_failures() {
        assert_eq!(portal_restore_state(Ok(true)).unwrap(), true);
        assert_eq!(portal_restore_state(Ok(false)).unwrap(), false);
        assert_eq!(
            portal_restore_state(Err(
                "authentication_required: saved sign-in has expired".to_string()
            ))
            .unwrap(),
            false
        );
        assert_eq!(
            portal_restore_state(Err(
                "oidc_refresh_token_missing: identity provider omitted refresh token".to_string()
            ))
            .unwrap(),
            false
        );
        assert!(
            portal_restore_state(Err("oidc_token_endpoint_unavailable: timed out".to_string()))
                .is_err()
        );
    }

    #[tokio::test]
    async fn portal_handoff_unauthorized_refreshes_and_retries_before_login() {
        let mut request_index = 0;
        let (base, server) = spawn_portal_fixture(3, move |request| {
            request_index += 1;
            match request_index {
                1 => {
                    assert!(request.starts_with("POST /portal-session-handoff "));
                    assert!(
                        request
                            .to_ascii_lowercase()
                            .contains("authorization: bearer stale-access")
                    );
                    assert!(request.to_ascii_lowercase().contains(&format!(
                        "x-pinvou-client-version: {}",
                        crate::platform::client_version::CURRENT
                    )));
                    (401, r#"{"code":"LOGIN_REQUIRED"}"#)
                }
                2 => {
                    assert!(request.starts_with("POST /token "));
                    assert!(request.contains("grant_type=refresh_token"));
                    assert!(request.contains("refresh_token=saved-refresh"));
                    (
                        200,
                        r#"{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":300}"#,
                    )
                }
                3 => {
                    assert!(request.starts_with("POST /portal-session-handoff "));
                    assert!(
                        request
                            .to_ascii_lowercase()
                            .contains("authorization: bearer fresh-access")
                    );
                    assert!(request.to_ascii_lowercase().contains(&format!(
                        "x-pinvou-client-version: {}",
                        crate::platform::client_version::CURRENT
                    )));
                    (200, r#"{"ticket":"single-use-ticket"}"#)
                }
                _ => unreachable!(),
            }
        });
        let credentials = MemoryCredentialStore::with_refresh_token("saved-refresh");
        let state = portal_test_state(
            credentials.clone(),
            Some(portal_runtime_session("stale-access", "saved-refresh")),
        );
        let mut config = config();
        config.token_url = base.join("token").unwrap();
        config.portal_url = base;

        let target = state.portal_target(&config, "ja").await.unwrap();
        server.join().unwrap();
        assert_eq!(target.path(), "/portal-session-handoff");
        assert_eq!(target.query(), Some("locale=ja"));
        assert_eq!(target.fragment(), Some("ticket=single-use-ticket"));
        assert_eq!(
            credentials.refresh_token().as_deref(),
            Some("fresh-refresh")
        );
    }

    #[tokio::test]
    async fn portal_restore_uses_persisted_refresh_before_handoff() {
        let mut request_index = 0;
        let (base, server) = spawn_portal_fixture(2, move |request| {
            request_index += 1;
            match request_index {
                1 => {
                    assert!(request.starts_with("POST /token "));
                    (
                        200,
                        r#"{"access_token":"restored-access","refresh_token":"rotated-refresh","expires_in":300}"#,
                    )
                }
                2 => {
                    assert!(
                        request
                            .to_ascii_lowercase()
                            .contains("authorization: bearer restored-access")
                    );
                    (200, r#"{"ticket":"restored-ticket"}"#)
                }
                _ => unreachable!(),
            }
        });
        let credentials = MemoryCredentialStore::with_refresh_token("persisted-refresh");
        let state = portal_test_state(credentials, None);
        let mut config = config();
        config.token_url = base.join("token").unwrap();
        config.portal_url = base;

        let target = state.portal_target(&config, "en").await.unwrap();
        server.join().unwrap();
        assert_eq!(target.fragment(), Some("ticket=restored-ticket"));
    }

    #[tokio::test]
    async fn transient_refresh_failure_preserves_authentication_and_does_not_open_login() {
        let mut request_index = 0;
        let (base, server) = spawn_portal_fixture(2, move |request| {
            request_index += 1;
            if request_index == 1 {
                (401, r#"{"code":"LOGIN_REQUIRED"}"#)
            } else {
                assert!(request.starts_with("POST /token "));
                (503, r#"{"error":"temporarily_unavailable"}"#)
            }
        });
        let credentials = MemoryCredentialStore::with_refresh_token("saved-refresh");
        let state = portal_test_state(
            credentials.clone(),
            Some(portal_runtime_session("stale-access", "saved-refresh")),
        );
        let mut config = config();
        config.token_url = base.join("token").unwrap();
        config.portal_url = base;

        let error = state.portal_target(&config, "en").await.unwrap_err();
        server.join().unwrap();
        assert!(error.starts_with("oidc_token_exchange_failed:"));
        assert_eq!(
            credentials.refresh_token().as_deref(),
            Some("saved-refresh")
        );
        assert!(state.session.lock().await.is_some());
    }

    #[tokio::test]
    async fn invalid_refresh_clears_authentication_and_opens_login() {
        let mut request_index = 0;
        let (base, server) = spawn_portal_fixture(2, move |_| {
            request_index += 1;
            if request_index == 1 {
                (401, r#"{"code":"LOGIN_REQUIRED"}"#)
            } else {
                (400, r#"{"error":"invalid_grant"}"#)
            }
        });
        let credentials = MemoryCredentialStore::with_refresh_token("expired-refresh");
        let state = portal_test_state(
            credentials.clone(),
            Some(portal_runtime_session("stale-access", "expired-refresh")),
        );
        let mut config = config();
        config.token_url = base.join("token").unwrap();
        config.portal_url = base;

        let target = state.portal_target(&config, "zh-Hans").await.unwrap();
        server.join().unwrap();
        assert_eq!(target.path(), "/auth/login");
        assert!(credentials.refresh_token().is_none());
        assert!(state.session.lock().await.is_none());
    }

    #[tokio::test]
    async fn handoff_dependency_failure_is_not_downgraded_to_browser_login() {
        let (base, server) =
            spawn_portal_fixture(1, |_| (503, r#"{"code":"SESSION_HANDOFF_UNAVAILABLE"}"#));
        let credentials = MemoryCredentialStore::with_refresh_token("saved-refresh");
        let state = portal_test_state(
            credentials,
            Some(portal_runtime_session("current-access", "saved-refresh")),
        );
        let mut config = config();
        config.portal_url = base;

        let error = state.portal_target(&config, "en").await.unwrap_err();
        server.join().unwrap();
        assert!(error.contains("identity returned 503"));
    }

    #[tokio::test]
    async fn missing_saved_login_opens_browser_login_without_network_request() {
        let state = portal_test_state(Arc::new(MemoryCredentialStore::default()), None);
        let target = state.portal_target(&config(), "ja").await.unwrap();
        assert_eq!(target.path(), "/auth/login");
        assert_eq!(
            target
                .query_pairs()
                .find(|(key, _)| key == "locale")
                .unwrap()
                .1,
            "ja"
        );
        assert_eq!(
            target
                .query_pairs()
                .find(|(key, _)| key == "return_to")
                .unwrap()
                .1,
            "/?locale=ja"
        );
    }

    fn independent_read_request(stream: &mut std::net::TcpStream) -> String {
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut bytes = Vec::new();
        let mut buf = [0u8; 2048];
        loop {
            let n = stream.read(&mut buf).unwrap();
            if n == 0 {
                break;
            }
            bytes.extend_from_slice(&buf[..n]);
            if let Some(p) = bytes.windows(4).position(|w| w == b"\r\n\r\n") {
                let header = String::from_utf8_lossy(&bytes[..p]);
                let length = header
                    .lines()
                    .find_map(|s| {
                        let (k, v) = s.split_once(':')?;
                        k.eq_ignore_ascii_case("content-length")
                            .then(|| v.trim().parse::<usize>().ok())
                            .flatten()
                    })
                    .unwrap_or(0);
                if bytes.len() >= p + 4 + length {
                    break;
                }
            }
            assert!(
                bytes.len() < 128 * 1024,
                "controlled request fixture oversized"
            );
        }
        String::from_utf8(bytes).unwrap()
    }

    fn independent_write_response(stream: &mut std::net::TcpStream, status: u16, body: &str) {
        write!(stream,"HTTP/1.1 {status} Fixture\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",body.len()).unwrap();
    }

    fn independent_env_config(url: &Url) {
        unsafe {
            for (key, value) in [
                (CLIENT_ID_ENV, "controlled-desktop"),
                (TOKEN_URL_ENV, url.as_str()),
                (ACCOUNT_URL_ENV, url.as_str()),
                (PORTAL_URL_ENV, url.as_str()),
            ] {
                std::env::set_var(key, value);
            }
            for key in [ISSUER_ENV, IDENTITY_URL_ENV, TOKEN_GATEWAY_URL_ENV] {
                std::env::remove_var(key);
            }
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn independent_rejected_request_cannot_switch_login_identity() {
        let _env = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let _snapshot = AccountEnvSnapshot::take();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let url = Url::parse(&format!("http://{}/", listener.local_addr().unwrap())).unwrap();
        independent_env_config(&url);
        let (first_tx, first_rx) = std::sync::mpsc::channel();
        let (continue_tx, continue_rx) = std::sync::mpsc::channel();
        let thread = thread::spawn(move || {
            let (mut first, _) = listener.accept().unwrap();
            let a = independent_read_request(&mut first);
            assert!(
                a.to_ascii_lowercase()
                    .contains("authorization: bearer public-access-a")
            );
            first_tx.send(()).unwrap();
            let (mut login, _) = listener.accept().unwrap();
            let b = independent_read_request(&mut login);
            assert!(b.contains("grant_type=password"));
            independent_write_response(
                &mut login,
                200,
                r#"{"access_token":"public-access-b","refresh_token":"public-refresh-b","expires_in":300}"#,
            );
            continue_rx.recv_timeout(Duration::from_secs(5)).unwrap();
            independent_write_response(&mut first, 401, r#"{"code":"UNAUTHENTICATED"}"#);
            drop(first);
            listener.set_nonblocking(true).unwrap();
            let end = Instant::now() + Duration::from_millis(650);
            let mut replayed_as_b = 0;
            while Instant::now() < end {
                match listener.accept() {
                    Ok((mut s, _)) => {
                        let req = independent_read_request(&mut s);
                        if req
                            .to_ascii_lowercase()
                            .contains("authorization: bearer public-access-b")
                        {
                            replayed_as_b += 1
                        }
                        independent_write_response(&mut s, 200, r#"{"status":"PENDING"}"#);
                        break;
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(5))
                    }
                    Err(e) => panic!("controlled listener error: {e}"),
                }
            }
            replayed_as_b
        });
        let state = Arc::new(portal_test_state(
            MemoryCredentialStore::with_refresh_token("public-refresh-a"),
            Some(portal_runtime_session(
                "public-access-a",
                "public-refresh-a",
            )),
        ));
        state.session_generation.store(1, Ordering::Relaxed);
        let request_state = state.clone();
        let request = tokio::spawn(async move {
            request_state
                .request(ApiRequest {
                    area: "account".into(),
                    resource: "/v1/me/profile-change-requests".into(),
                    method: "POST".into(),
                    body: Some(serde_json::json!({"display_name":"public-user-a-name"})),
                    idempotency_key: None,
                })
                .await
        });
        first_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        state
            .login(LoginCredentials {
                account: "public-user-b".into(),
                password: "PUBLIC_SYNTHETIC_PASSWORD_B".into(),
            })
            .await
            .unwrap();
        continue_tx.send(()).unwrap();
        let result = request.await.unwrap();
        let replayed_as_b = thread.join().unwrap();
        assert!(
            result.is_err(),
            "old logical request was acknowledged after different-user login"
        );
        assert_eq!(
            replayed_as_b, 0,
            "old logical request replayed with new login bearer"
        );
    }

    fn independent_large_response_fixture(body: String) -> (Url, thread::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let url = Url::parse(&format!("http://{}/", listener.local_addr().unwrap())).unwrap();
        let join = thread::spawn(move || {
            let (mut s, _) = listener.accept().unwrap();
            independent_read_request(&mut s);
            s.set_write_timeout(Some(Duration::from_secs(5))).unwrap();
            let _ = write!(
                s,
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                body.len()
            );
        });
        (url, join)
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn independent_token_body_enforces_existing_response_limit() {
        let body = format!(
            "{}{}",
            " ".repeat(MAX_RESPONSE_BYTES + 1),
            r#"{"access_token":"public-access","refresh_token":"public-refresh","expires_in":300}"#
        );
        let (url, join) = independent_large_response_fixture(body);
        let state = portal_test_state(Arc::new(MemoryCredentialStore::default()), None);
        let mut cfg = config();
        cfg.token_url = url;
        let result = state
            .token_request(&cfg, &[("grant_type", "password")], TokenFlow::Password)
            .await;
        join.join().unwrap();
        assert!(result.is_err(), "oversized OIDC token body accepted");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn independent_handoff_body_enforces_existing_response_limit() {
        let body = format!(
            "{}{}",
            " ".repeat(MAX_RESPONSE_BYTES + 1),
            r#"{"ticket":"PUBLIC_SYNTHETIC_TICKET"}"#
        );
        let (url, join) = independent_large_response_fixture(body);
        let state = portal_test_state(Arc::new(MemoryCredentialStore::default()), None);
        let mut cfg = config();
        cfg.portal_url = url;
        let result = state
            .request_portal_session_handoff(&cfg, "public-access")
            .await;
        join.join().unwrap();
        assert!(result.is_err(), "oversized handoff body accepted");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn independent_provider_expiry_overflow_is_error_not_panic() {
        let state = Arc::new(portal_test_state(
            Arc::new(MemoryCredentialStore::default()),
            None,
        ));
        let s = state.clone();
        let result = tokio::spawn(async move {
            s.install_tokens(
                TokenResponse {
                    access_token: "public-access".into(),
                    refresh_token: Some("public-refresh".into()),
                    expires_in: u64::MAX,
                },
                None,
            )
            .await
        })
        .await;
        assert!(
            matches!(result, Ok(Err(_))),
            "unrepresentable provider expiry panicked or installed an unusable session"
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn independent_registration_status_cannot_delete_new_recovery() {
        let _env = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let _snapshot = AccountEnvSnapshot::take();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let url = Url::parse(&format!("http://{}/", listener.local_addr().unwrap())).unwrap();
        independent_env_config(&url);
        let (first_tx, first_rx) = std::sync::mpsc::channel();
        let (finish_tx, finish_rx) = std::sync::mpsc::channel();
        let thread = thread::spawn(move || {
            let (mut a, _) = listener.accept().unwrap();
            let first = independent_read_request(&mut a);
            assert!(
                first.contains(
                    "/v1/auth/registration-operations/11111111-1111-4111-8111-111111111111"
                )
            );
            first_tx.send(()).unwrap();
            let (mut b, _) = listener.accept().unwrap();
            let second = independent_read_request(&mut b);
            assert!(second.contains("/v1/auth/sms/challenges"));
            independent_write_response(
                &mut b,
                200,
                r#"{"registration_operation_id":"22222222-2222-4222-8222-222222222222","registration_query_token":"PUBLIC_QUERY_B","registration_query_token_expires_at":"2099-01-01T00:00:00Z"}"#,
            );
            finish_rx.recv_timeout(Duration::from_secs(5)).unwrap();
            independent_write_response(&mut a, 200, r#"{"status":"ACTIVE"}"#);
        });
        let store = Arc::new(MemoryCredentialStore::default());
        store.set(&CredentialReference::for_account_platform_registration(),r#"{"operation_id":"11111111-1111-4111-8111-111111111111","query_token":"PUBLIC_QUERY_A","username":"public-a","expires_at":"2099-01-01T00:00:00Z"}"#).unwrap();
        let state = Arc::new(portal_test_state(store.clone(), None));
        let s = state.clone();
        let first = tokio::spawn(async move { s.registration_status().await });
        first_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        state
            .send_sms(SMSRequest {
                username: "public-b".into(),
                mobile: "+8613800000000".into(),
                password_handle: "PUBLIC_HANDLE".into(),
                password_policy_version: "v1".into(),
                purpose: "REGISTER".into(),
                captcha_id: "PUBLIC_CAPTCHA".into(),
                captcha_offset: 0,
            })
            .await
            .unwrap();
        finish_tx.send(()).unwrap();
        first.await.unwrap().unwrap();
        thread.join().unwrap();
        let pending = state.pending_registration().unwrap();
        assert!(
            pending.is_some_and(|p| p.operation_id == "22222222-2222-4222-8222-222222222222"),
            "old terminal registration query deleted newly persisted recovery credential"
        );
    }
}
