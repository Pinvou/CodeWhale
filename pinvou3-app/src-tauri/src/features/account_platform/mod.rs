//! Desktop account-platform adapter.
//!
//! OAuth tokens never cross the Tauri command boundary. The WebView submits a
//! password only for the duration of an explicit sign-in request; the password
//! is never persisted. A successful session stores only its renewable refresh
//! token in the system credential store so the next launch can sign in again.

pub(crate) mod client_migration;
mod platform;
pub(crate) mod runtime_model;

use std::sync::atomic::{AtomicU64, Ordering};
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

const ISSUER_ENV: &str = "PINVOU_ACCOUNT_OIDC_ISSUER";
const TOKEN_URL_ENV: &str = "PINVOU_ACCOUNT_OIDC_TOKEN_URL";
const CLIENT_ID_ENV: &str = "PINVOU_ACCOUNT_OIDC_CLIENT_ID";
const ACCOUNT_URL_ENV: &str = "PINVOU_ACCOUNT_SERVICE_URL";
const IDENTITY_URL_ENV: &str = "PINVOU_IDENTITY_SERVICE_URL";
const TOKEN_GATEWAY_URL_ENV: &str = "PINVOU_TOKEN_GATEWAY_URL";
const PORTAL_URL_ENV: &str = "PINVOU_ACCOUNT_PORTAL_URL";
const DEFAULT_PORTAL_URL: &str = "https://pinvou.com/account";
const MAX_RESPONSE_BYTES: usize = 2 * 1024 * 1024;
const MAX_REQUEST_BODY_BYTES: usize = 64 * 1024;
const MAX_CHAT_REQUEST_BODY_BYTES: usize = 4 * 1024 * 1024;

#[derive(Debug, Clone)]
struct AccountPlatformConfig {
    token_url: Url,
    client_id: String,
    account_url: Url,
    identity_url: Option<Url>,
    token_gateway_url: Url,
}

impl AccountPlatformConfig {
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
            identity_url: optional_env(IDENTITY_URL_ENV)
                .map(|value| parse_service_base(IDENTITY_URL_ENV, &value))
                .transpose()?,
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
    platform_supported()
}

/// Opens the browser-owned account Portal session without accepting a URL
/// from the WebView. Credentials are deliberately absent from the fixed URL.
pub fn open_account_portal() -> Result<(), String> {
    let target = account_portal_url()?;
    crate::platform::os::open_target(target.as_str(), "账号平台个人中心")
}

fn account_portal_url() -> Result<Url, String> {
    account_portal_url_from(
        optional_env(PORTAL_URL_ENV)
            .as_deref()
            .unwrap_or(DEFAULT_PORTAL_URL),
    )
}

fn account_portal_url_from(value: &str) -> Result<Url, String> {
    let target = parse_endpoint(PORTAL_URL_ENV, value)?;
    if target.query().is_some() || target.fragment().is_some() {
        return Err("account_platform_configuration_invalid: account portal URL must not contain query or fragment".to_string());
    }
    Ok(target)
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

/// OIDC-backed credentials used only inside the Rust model runtime.
///
/// The access token never crosses the Tauri command boundary and this type is
/// intentionally not serializable or debuggable.
pub(crate) struct ChatRuntimeAuthorization {
    pub(crate) base_url: String,
    pub(crate) access_token: String,
    pub(crate) revision: String,
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

#[derive(Debug, Serialize, Deserialize)]
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
    #[serde(default = "default_expires_in")]
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
    pub context_version: Option<u64>,
    pub authorization_version: Option<u64>,
}

pub struct AccountPlatformState {
    session: Mutex<Option<RuntimeSession>>,
    login_guard: Mutex<()>,
    refresh_guard: Mutex<()>,
    client: reqwest::Client,
    credentials: SystemCredentialStore,
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
            client,
            credentials: SystemCredentialStore::new(),
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

    pub async fn send_sms(&self, input: SMSRequest) -> Result<Value, String> {
        let config = AccountPlatformConfig::from_env()?;
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
        let config = AccountPlatformConfig::from_env()?;
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
        let config = AccountPlatformConfig::from_env()?;
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
        let config = AccountPlatformConfig::from_env()?;
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
        let config = AccountPlatformConfig::from_env()?;
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
        let config = AccountPlatformConfig::from_env()?;
        let mut result = self
            .public_account_registration_status(
                &config,
                &pending.operation_id,
                &pending.query_token,
            )
            .await?;
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
            self.credentials
                .delete(&CredentialReference::for_account_platform_registration())
                .map_err(credential_store_error)?;
        }
        Ok(result)
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
        let config = AccountPlatformConfig::from_env()?;
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
        let config = AccountPlatformConfig::from_env()?;
        let prepared = prepare_request(&config, input)?;
        let mut token = self.access_token(&config, None).await?;
        let mut response = self.send(&prepared, &token).await?;
        if response.status() == StatusCode::UNAUTHORIZED {
            token = self.access_token(&config, Some(&token)).await?;
            response = self.send(&prepared, &token).await?;
        }
        let api_response = response_to_api(response).await?;
        if prepared.is_account_bootstrap() && (200..300).contains(&api_response.status) {
            let user_id = bootstrap_user_id(&api_response.body)?;
            self.remember_account_identity(&token, user_id).await?;
        }
        Ok(api_response)
    }

    pub(crate) async fn chat_runtime_authorization(
        &self,
    ) -> Result<ChatRuntimeAuthorization, String> {
        let config = AccountPlatformConfig::from_env()?;
        let mut access_token = self.access_token(&config, None).await?;
        let mut account_user_id = self
            .cached_account_identity(&access_token)
            .await
            .unwrap_or_default();
        if account_user_id.is_empty() {
            match self
                .bootstrap_account_identity(&config, &access_token)
                .await
            {
                Ok(user_id) => account_user_id = user_id,
                Err(error) if error.starts_with("authentication_required:") => {
                    access_token = self.access_token(&config, Some(&access_token)).await?;
                    account_user_id = self
                        .bootstrap_account_identity(&config, &access_token)
                        .await?;
                }
                Err(error) => return Err(error),
            }
        }
        let generation = self
            .session
            .lock()
            .await
            .as_ref()
            .filter(|session| {
                session.access_token == access_token
                    && session.account_user_id.as_deref() == Some(account_user_id.as_str())
            })
            .map(|session| session.generation)
            .ok_or_else(|| "authentication_required: sign in to Pinvou first".to_string())?;
        Ok(ChatRuntimeAuthorization {
            base_url: token_gateway_openai_base(&config.token_gateway_url),
            access_token,
            revision: format!("oidc-user-{account_user_id}-session-{generation}"),
        })
    }

    async fn cached_account_identity(&self, access_token: &str) -> Option<String> {
        self.session
            .lock()
            .await
            .as_ref()
            .filter(|session| session.access_token == access_token)
            .and_then(|session| session.account_user_id.clone())
    }

    async fn bootstrap_account_identity(
        &self,
        config: &AccountPlatformConfig,
        access_token: &str,
    ) -> Result<String, String> {
        let url = config
            .account_url
            .join("v1/account/bootstrap")
            .map_err(|_| {
                "account_platform_configuration_invalid: cannot resolve account bootstrap"
                    .to_string()
            })?;
        let response = self
            .client
            .post(url)
            .bearer_auth(access_token)
            .header("Accept", "application/json")
            .header(
                crate::platform::client_version::HEADER,
                crate::platform::client_version::CURRENT,
            )
            .header("X-Request-ID", random_urlsafe(18))
            .header("X-Device-ID", &self.device_id)
            .json(&serde_json::json!({}))
            .send()
            .await
            .map_err(|error| format!("account_platform_service_unavailable: {error}"))?;
        if response.status() == StatusCode::UNAUTHORIZED {
            return Err("authentication_required: account session was rejected".to_string());
        }
        let api_response = response_to_api(response).await?;
        if !(200..300).contains(&api_response.status) {
            let code = api_response
                .body
                .get("code")
                .and_then(Value::as_str)
                .unwrap_or("ACCOUNT_BOOTSTRAP_FAILED");
            return Err(format!(
                "account_platform_account_unavailable: account bootstrap failed with {code}"
            ));
        }
        let user_id = bootstrap_user_id(&api_response.body)?;
        self.remember_account_identity(access_token, user_id.clone())
            .await?;
        Ok(user_id)
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
        if !status.is_success() {
            let oauth_error = response.json::<OAuthErrorResponse>().await.ok();
            return Err(classify_token_error(
                flow,
                status,
                oauth_error.as_ref().and_then(|body| body.error.as_deref()),
                oauth_error
                    .as_ref()
                    .and_then(|body| body.error_description.as_deref()),
            ));
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
        let generation = self
            .session_generation
            .fetch_add(1, Ordering::Relaxed)
            .saturating_add(1);
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
            expires_at: Instant::now() + lifetime,
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
        let response = self
            .client
            .post(url)
            .header("Accept", "application/json")
            .header(
                crate::platform::client_version::HEADER,
                crate::platform::client_version::CURRENT,
            )
            .header("X-Request-ID", random_urlsafe(18))
            .header("X-Device-ID", &self.device_id)
            .json(&body)
            .send()
            .await
            .map_err(|error| format!("account_platform_service_unavailable: {error}"))?;
        let response = response_to_api(response).await?;
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
            return Err(format!("mobile_auth_failed: {code}{retry_after}"));
        }
        Ok(response.body)
    }

    async fn public_identity_request(
        &self,
        config: &AccountPlatformConfig,
        path: &str,
        body: Value,
    ) -> Result<Value, String> {
        let identity_url = config.identity_url.as_ref().ok_or_else(|| {
            format!("account_platform_configuration_missing: {IDENTITY_URL_ENV} is required for registration")
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
                let _ = self
                    .credentials
                    .delete(&CredentialReference::for_account_platform_registration());
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
            "oidc_direct_grant_disabled: in-app password sign-in is disabled".to_string()
        }
        (TokenFlow::Sms, Some("invalid_grant")) => {
            "mobile_auth_code_invalid: SMS code is invalid or expired".to_string()
        }
        (TokenFlow::Sms, Some("unauthorized_client")) => {
            "oidc_direct_grant_disabled: in-app SMS sign-in is disabled".to_string()
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

fn token_gateway_openai_base(base: &Url) -> String {
    let trimmed = base.as_str().trim_end_matches('/');
    if trimmed.ends_with("/v1") {
        trimmed.to_string()
    } else {
        format!("{trimmed}/v1")
    }
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
    if !matches!(method, Method::GET | Method::POST | Method::PUT) {
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
        let maximum = if parsed.path() == "/v1/chat/completions" {
            MAX_CHAT_REQUEST_BODY_BYTES
        } else {
            MAX_REQUEST_BODY_BYTES
        };
        if size > maximum {
            return Err("account_platform_request_invalid: request body is too large".to_string());
        }
    }
    if input.idempotency_key.is_some()
        && !matches!(
            parsed.path(),
            "/v1/recharge/orders" | "/v1/chat/completions"
        )
        && !(method == Method::PUT
            && parsed.path().starts_with("/v1/organizations/")
            && parsed.path().ends_with("/model-policy"))
    {
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
    if parsed.path() == "/v1/chat/completions" {
        let body = input.body.as_ref().ok_or_else(|| {
            "account_platform_request_invalid: chat request body is required".to_string()
        })?;
        if body.get("stream").and_then(Value::as_bool) == Some(true) {
            return Err(
                "account_platform_request_denied: streaming chat uses the native model runtime"
                    .to_string(),
            );
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
        | ("token", &Method::GET, ["v1", "models"], None)
        | ("token", &Method::GET, ["v1", "recharge", "orders"], None)
        | ("token", &Method::POST, ["v1", "recharge", "orders"], None)
        | ("token", &Method::GET, ["v1", "recharge", "orders", "recent"], None)
        | ("token", &Method::GET, ["v1", "me", "entitlement-packages"], None)
        | ("token", &Method::POST, ["v1", "chat", "completions"], None) => true,
        ("account", &Method::GET, ["v1", "organizations", id, "units"], None)
        | ("account", &Method::POST, ["v1", "organizations", id, "units"], None)
        | ("account", &Method::GET, ["v1", "organizations", id, "members"], None)
        | ("account", &Method::POST, ["v1", "organizations", id, "invitations"], None)
        | ("token", &Method::GET, ["v1", "recharge", "orders", id], None)
        | ("token", &Method::GET, ["v1", "organizations", id, "model-policy"], None)
        | ("token", &Method::PUT, ["v1", "organizations", id, "model-policy"], None) => safe_id(id),
        ("token", &Method::GET, ["v1", "me", "balance"], Some("consistency=strong")) => true,
        ("token", &Method::GET, ["v1", "me", "usage-requests"], query) => {
            usage_history_query_allowed(query)
        }
        _ => false,
    }
}

fn usage_history_query_allowed(query: Option<&str>) -> bool {
    let Some(query) = query else {
        return true;
    };
    let mut limit_seen = false;
    let mut cursor_seen = false;
    for part in query.split('&') {
        let Some((name, value)) = part.split_once('=') else {
            return false;
        };
        match name {
            "limit" if !limit_seen => {
                limit_seen = value
                    .parse::<u8>()
                    .ok()
                    .is_some_and(|limit| (1..=100).contains(&limit));
                if !limit_seen {
                    return false;
                }
            }
            "cursor" if !cursor_seen => {
                cursor_seen = !value.is_empty()
                    && value.len() <= 256
                    && value
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'));
                if !cursor_seen {
                    return false;
                }
            }
            _ => return false,
        }
    }
    limit_seen || cursor_seen
}

async fn response_to_api(response: reqwest::Response) -> Result<ApiResponse, String> {
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
    Ok(ApiResponse {
        status,
        body,
        context_version,
        authorization_version,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn config() -> AccountPlatformConfig {
        AccountPlatformConfig {
            token_url: Url::parse("https://id.example.test/token").unwrap(),
            identity_url: Some(Url::parse("https://id.example.test/").unwrap()),
            client_id: "desktop-client".to_string(),
            account_url: Url::parse("https://account.example.test/").unwrap(),
            token_gateway_url: Url::parse("https://token.example.test/").unwrap(),
        }
    }

    #[test]
    fn account_platform_is_the_only_supported_desktop_path() {
        assert_eq!(account_platform_enabled(), platform_supported());
    }

    #[test]
    fn token_errors_distinguish_password_and_saved_session_failures() {
        assert!(classify_token_error(
            TokenFlow::Password,
            StatusCode::BAD_REQUEST,
            Some("invalid_grant"),
            None,
        )
        .starts_with("oidc_credentials_invalid:"));
        assert!(classify_token_error(
            TokenFlow::Password,
            StatusCode::UNAUTHORIZED,
            Some("unauthorized_client"),
            None,
        )
        .starts_with("oidc_direct_grant_disabled:"));
        assert!(classify_token_error(
            TokenFlow::Refresh,
            StatusCode::BAD_REQUEST,
            Some("invalid_grant"),
            None,
        )
        .starts_with("authentication_required:"));
        assert!(classify_token_error(
            TokenFlow::Password,
            StatusCode::BAD_REQUEST,
            Some("invalid_grant"),
            Some("Account is not fully set up"),
        )
        .starts_with("oidc_account_setup_incomplete:"));
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

        assert!(prepare_request(
            &config(),
            ApiRequest {
                area: "token".to_string(),
                resource: "/v1/models".to_string(),
                method: "GET".to_string(),
                body: None,
                idempotency_key: None,
            },
        )
        .is_ok());

        assert!(prepare_request(
            &config(),
            ApiRequest {
                area: "token".to_string(),
                resource: "/v1/organizations/018f47b4-e124-7dca-9c85-6f7688436600/model-policy"
                    .to_string(),
                method: "PUT".to_string(),
                body: Some(serde_json::json!({
                    "expected_version": 0,
                    "mode": "ALLOW_ALL",
                    "model_ids": [],
                    "reason": "initial policy"
                })),
                idempotency_key: Some("model-policy-0001".to_string()),
            },
        )
        .is_ok());

        assert!(prepare_request(
            &config(),
            ApiRequest {
                area: "token".to_string(),
                resource: "/v1/me/usage-requests?limit=20&cursor=YWJjLTEyMw".to_string(),
                method: "GET".to_string(),
                body: None,
                idempotency_key: None,
            },
        )
        .is_ok());

        let chat = prepare_request(
            &config(),
            ApiRequest {
                area: "token".to_string(),
                resource: "/v1/chat/completions".to_string(),
                method: "POST".to_string(),
                body: Some(serde_json::json!({
                    "model": "example-model",
                    "messages": [{"role": "user", "content": "hello"}],
                    "stream": false
                })),
                idempotency_key: Some("desktop-request-0001".to_string()),
            },
        )
        .unwrap();
        assert_eq!(
            chat.url.as_str(),
            "https://token.example.test/v1/chat/completions"
        );

        assert!(prepare_request(
            &config(),
            ApiRequest {
                area: "token".to_string(),
                resource: "/v1/chat/completions".to_string(),
                method: "POST".to_string(),
                body: Some(serde_json::json!({"model": "example-model", "stream": true})),
                idempotency_key: Some("desktop-request-0002".to_string()),
            },
        )
        .is_err());

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

    #[test]
    fn account_portal_is_fixed_and_never_carries_credentials() {
        assert_eq!(
            account_portal_url_from("https://pinvou.com/account")
                .unwrap()
                .as_str(),
            "https://pinvou.com/account"
        );
        assert!(account_portal_url_from("https://portal.example.test/?token=secret").is_err());
        assert!(account_portal_url_from("https://user:secret@portal.example.test/").is_err());
    }

    #[test]
    fn chat_runtime_uses_the_gateway_openai_base() {
        assert_eq!(
            token_gateway_openai_base(&Url::parse("https://account.example/token/").unwrap()),
            "https://account.example/token/v1"
        );
        assert_eq!(
            token_gateway_openai_base(&Url::parse("https://account.example/token/v1/").unwrap()),
            "https://account.example/token/v1"
        );
    }
}
