//! Account identity and credentials for the existing native model runtime.
//! The bearer credential stays in Rust; only a nonsecret revision is exposed
//! to the EnginePool's existing model invalidation boundary.

use super::*;

pub(crate) struct ChatRuntimeAuthorization {
    pub(crate) base_url: String,
    pub(crate) access_token: String,
    pub(crate) revision: String,
}

impl AccountPlatformState {
    pub(super) async fn ensure_restored(
        &self,
        config: &AccountPlatformConfig,
    ) -> Result<bool, String> {
        if self.session.lock().await.as_ref().is_some_and(|session| {
            session.identity.matches(config) && access_token_is_usable(session, None)
        }) {
            return Ok(true);
        }
        // Absence is an ordinary guest state. A failed read is not absence;
        // preserve the existing record and surface the temporary store error.
        if self
            .credentials
            .get(REFRESH_TOKEN_KEY)
            .map_err(|_| "credential_store_failed".to_string())?
            .is_none()
        {
            return Ok(false);
        }
        self.access_token(config, None).await?;
        Ok(true)
    }

    pub(crate) async fn chat_runtime_authorization(
        &self,
    ) -> Result<ChatRuntimeAuthorization, String> {
        self.chat_runtime_authorization_with(&AccountPlatformConfig::from_env()?)
            .await
    }

    pub(super) async fn chat_runtime_authorization_with(
        &self,
        config: &AccountPlatformConfig,
    ) -> Result<ChatRuntimeAuthorization, String> {
        let epoch = self.session_epoch.load(Ordering::SeqCst);
        let mut token = self.access_token(config, None).await?;
        self.require_session_epoch(epoch)?;
        let user_id = match self.cached_account_identity(config, &token).await {
            Some(user_id) => user_id,
            None => match self.bootstrap_account_identity(config, &token).await {
                Ok(user_id) => user_id,
                Err(error) if error == "authentication_required: account session was rejected" => {
                    self.require_session_epoch(epoch)?;
                    token = self.access_token(config, Some(&token)).await?;
                    self.require_session_epoch(epoch)?;
                    self.bootstrap_account_identity(config, &token).await?
                }
                Err(error) => return Err(error),
            },
        };
        self.require_session_epoch(epoch)?;
        let session = self.session.lock().await;
        let current = session
            .as_ref()
            .filter(|session| {
                session.identity.matches(config)
                    && session.access_token == token
                    && session.expires_at > Instant::now()
                    && session.account_user_id.as_deref() == Some(user_id.as_str())
            })
            .ok_or_else(|| "authentication_required: account session changed".to_string())?;
        Ok(ChatRuntimeAuthorization {
            base_url: token_gateway_openai_base(&config.token_gateway_url),
            access_token: token,
            revision: format!("oidc-session-{}", current.generation),
        })
    }

    pub(super) fn require_session_epoch(&self, expected: u64) -> Result<(), String> {
        if self.session_epoch.load(Ordering::SeqCst) != expected {
            return Err("authentication_required: account session changed".into());
        }
        Ok(())
    }

    async fn cached_account_identity(
        &self,
        config: &AccountPlatformConfig,
        token: &str,
    ) -> Option<String> {
        self.session
            .lock()
            .await
            .as_ref()
            .filter(|session| session.identity.matches(config) && session.access_token == token)
            .and_then(|session| session.account_user_id.clone())
    }

    async fn bootstrap_account_identity(
        &self,
        config: &AccountPlatformConfig,
        token: &str,
    ) -> Result<String, String> {
        let request = prepare_request(
            config,
            ApiRequest {
                area: "account".into(),
                resource: "/v1/account/bootstrap".into(),
                method: "POST".into(),
                body: Some(serde_json::json!({})),
                idempotency_key: None,
            },
        )?;
        let response = response_to_api(self.send(&request, token).await?).await?;
        if response.status == 401 {
            return Err("authentication_required: account session was rejected".into());
        }
        if !(200..300).contains(&response.status) {
            // Do not return provider-controlled messages or profile fields.
            return Err("account_platform_account_unavailable".into());
        }
        let user_id = bootstrap_user_id(&response.body)?;
        self.remember_account_identity(config, token, user_id.clone())
            .await?;
        Ok(user_id)
    }

    pub(super) async fn remember_account_identity(
        &self,
        config: &AccountPlatformConfig,
        token: &str,
        user_id: String,
    ) -> Result<(), String> {
        let mut session = self.session.lock().await;
        let current = session
            .as_mut()
            .filter(|session| session.identity.matches(config) && session.access_token == token)
            .ok_or_else(|| "authentication_required: account session changed".to_string())?;
        current.account_user_id = Some(user_id);
        Ok(())
    }
}

pub(super) fn session_error_code(error: &str) -> &'static str {
    match error.split(':').next().unwrap_or_default() {
        "authentication_required" => "authentication_required",
        "credential_store_failed" => "credential_store_failed",
        "oidc_jwks_unavailable" => "oidc_jwks_unavailable",
        "oidc_id_token_invalid" => "oidc_id_token_invalid",
        "oidc_token_response_invalid" => "oidc_token_response_invalid",
        "oidc_token_endpoint_unavailable" => "oidc_token_endpoint_unavailable",
        "oidc_token_exchange_failed" => "oidc_token_exchange_failed",
        _ => "account_platform_service_unavailable",
    }
}

pub(super) fn bootstrap_user_id(body: &Value) -> Result<String, String> {
    body.get("user_id")
        .and_then(Value::as_str)
        .filter(|value| {
            !value.is_empty()
                && value.len() <= 128
                && value
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
        })
        .map(str::to_string)
        .ok_or_else(|| "account_platform_response_invalid: account user ID missing".to_string())
}

fn token_gateway_openai_base(base: &Url) -> String {
    let trimmed = base.as_str().trim_end_matches('/');
    if trimmed.ends_with("/v1") {
        trimmed.to_string()
    } else {
        format!("{trimmed}/v1")
    }
}

pub(super) fn usage_history_query_allowed(query: Option<&str>) -> bool {
    let Some(query) = query else { return true };
    let (mut limit_seen, mut cursor_seen) = (false, false);
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
