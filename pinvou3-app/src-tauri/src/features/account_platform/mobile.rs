//! Public mobile-auth requests and SMS sign-in. Secrets never enter Debug
//! output; provider messages are reduced to the existing finite error contract.
use super::*;

#[derive(Deserialize)]
pub struct SMSRequest {
    pub username: String,
    pub mobile: String,
    pub password: String,
    pub purpose: String,
    pub captcha_id: String,
    pub captcha_offset: u16,
}

#[derive(Deserialize)]
pub struct SliderCaptchaRequest {
    pub mobile: String,
    pub purpose: String,
}

#[derive(Deserialize)]
pub struct MobileRegistrationPrecheck {
    pub username: String,
    pub mobile: String,
    pub password: String,
}

#[derive(Deserialize)]
pub struct SMSLoginCredentials {
    pub mobile: String,
    pub challenge_id: String,
    pub code: String,
}

#[derive(Deserialize)]
pub struct MobileRegistration {
    pub username: String,
    pub mobile: String,
    pub challenge_id: String,
    pub code: String,
    pub password: String,
}

fn invalid_request() -> String {
    "mobile_auth_failed:INVALID_REQUEST".into()
}

fn mobile_number(value: &str) -> Result<&str, String> {
    let value = value.trim();
    let value = value.strip_prefix("+86").unwrap_or(value);
    let value = value.strip_prefix("86").unwrap_or(value);
    let bytes = value.as_bytes();
    if bytes.len() != 11
        || bytes[0] != b'1'
        || !(b'3'..=b'9').contains(&bytes[1])
        || !bytes.iter().all(u8::is_ascii_digit)
    {
        return Err("mobile_auth_failed:MOBILE_INVALID".into());
    }
    Ok(value)
}

fn purpose(value: &str) -> Result<String, String> {
    let value = value.trim().to_ascii_uppercase();
    match value.as_str() {
        "REGISTER" | "LOGIN" => Ok(value),
        _ => Err(invalid_request()),
    }
}

fn challenge_id(value: &str) -> Result<&str, String> {
    let value = value.trim();
    uuid::Uuid::parse_str(value)
        .ok()
        .filter(|id| !id.is_nil())
        .ok_or_else(invalid_request)?;
    Ok(value)
}

// Keep policy decisions in Account Service. These are transport limits rather
// than a second username/password/SMS-provider policy.
fn registration_fields(username: &str, password: &str) -> Result<(), String> {
    if username.trim().is_empty()
        || username.len() > 320
        || username.chars().any(char::is_control)
        || password.is_empty()
        || password.len() > 4096
    {
        return Err(invalid_request());
    }
    Ok(())
}

fn sms_code(value: &str) -> Result<&str, String> {
    let value = value.trim();
    if value.is_empty() || value.len() > 128 || value.chars().any(char::is_control) {
        return Err(invalid_request());
    }
    Ok(value)
}

fn public_error(body: &Value) -> String {
    let code = match body.get("code").and_then(Value::as_str) {
        Some(
            code @ ("INVALID_REQUEST"
            | "MOBILE_INVALID"
            | "MOBILE_ALREADY_REGISTERED"
            | "USERNAME_INVALID"
            | "USERNAME_ALREADY_REGISTERED"
            | "PASSWORD_INVALID"
            | "CAPTCHA_INVALID"
            | "SMS_RATE_LIMITED"
            | "SMS_CODE_INVALID"
            | "SMS_UNAVAILABLE"
            | "AUTH_INTERNAL_ERROR"),
        ) => code,
        _ => "AUTH_INTERNAL_ERROR",
    };
    let retry = if code == "SMS_RATE_LIMITED" {
        body.pointer("/details/retry_after_seconds")
            .and_then(Value::as_u64)
            .filter(|seconds| (1..=86400).contains(seconds))
    } else {
        None
    };
    match retry {
        Some(seconds) => format!("mobile_auth_failed:{code}:{seconds}"),
        None => format!("mobile_auth_failed:{code}"),
    }
}

pub(super) fn classify_mobile_grant_error(
    form: &[(&str, &str)],
    status: StatusCode,
    bytes: &[u8],
) -> Option<&'static str> {
    if !status.is_client_error()
        || form
            .iter()
            .any(|&(key, value)| key == "grant_type" && value == "refresh_token")
    {
        return None;
    }
    let body: Value = serde_json::from_slice(bytes).ok()?;
    if body.get("error").and_then(Value::as_str) != Some("invalid_grant") {
        return None;
    }
    if body
        .get("error_description")
        .and_then(Value::as_str)
        .is_some_and(|message| {
            message
                .to_ascii_lowercase()
                .contains("account is not fully set up")
        })
    {
        return Some("oidc_account_setup_incomplete");
    }
    if form.iter().any(|&(key, _)| key == "sms_challenge_id") {
        return Some("mobile_auth_code_invalid");
    }
    None
}

impl AccountPlatformState {
    pub async fn send_sms(&self, input: SMSRequest) -> Result<Value, String> {
        let purpose = purpose(&input.purpose)?;
        let mobile = mobile_number(&input.mobile)?;
        let captcha_id = challenge_id(&input.captcha_id)?;
        if input.username.len() > 320 || input.password.len() > 4096 {
            return Err(invalid_request());
        }
        if purpose == "REGISTER" {
            registration_fields(&input.username, &input.password)?;
        }
        self.public_account_request(
            &AccountPlatformConfig::from_env()?,
            "v1/auth/sms/challenges",
            serde_json::json!({"username": input.username.trim(), "mobile": mobile,
                "password": input.password, "purpose": purpose, "captcha_id": captcha_id,
                "captcha_offset": input.captcha_offset}),
        )
        .await
    }

    pub async fn create_slider_captcha(
        &self,
        input: SliderCaptchaRequest,
    ) -> Result<Value, String> {
        self.public_account_request(&AccountPlatformConfig::from_env()?, "v1/auth/captcha/challenges",
            serde_json::json!({"mobile": mobile_number(&input.mobile)?, "purpose": purpose(&input.purpose)?})).await
    }

    pub async fn precheck_mobile_registration(
        &self,
        input: MobileRegistrationPrecheck,
    ) -> Result<Value, String> {
        registration_fields(&input.username, &input.password)?;
        self.public_account_request(&AccountPlatformConfig::from_env()?, "v1/auth/registration/precheck",
            serde_json::json!({"username": input.username.trim(), "mobile": mobile_number(&input.mobile)?,
                "password": input.password})).await
    }

    pub async fn register_mobile(&self, input: MobileRegistration) -> Result<Value, String> {
        registration_fields(&input.username, &input.password)?;
        self.public_account_request(&AccountPlatformConfig::from_env()?, "v1/auth/register",
            serde_json::json!({"username": input.username.trim(), "mobile": mobile_number(&input.mobile)?,
                "challenge_id": challenge_id(&input.challenge_id)?, "code": sms_code(&input.code)?,
                "password": input.password})).await
    }

    pub async fn login_sms(
        &self,
        credentials: SMSLoginCredentials,
    ) -> Result<SessionStatus, String> {
        mobile_number(&credentials.mobile)?;
        challenge_id(&credentials.challenge_id)?;
        sms_code(&credentials.code)?;
        let before_login = self.session_epoch.load(Ordering::SeqCst);
        let _login = self.login_guard.lock().await;
        self.require_session_epoch(before_login)?;
        self.login_sms_with(&AccountPlatformConfig::from_env()?, credentials)
            .await?;
        Ok(SessionStatus {
            enabled: true,
            configured: true,
            authenticated: true,
            error_code: None,
        })
    }

    pub(super) async fn login_sms_with(
        &self,
        config: &AccountPlatformConfig,
        credentials: SMSLoginCredentials,
    ) -> Result<(), String> {
        let mobile = mobile_number(&credentials.mobile)?;
        let challenge = challenge_id(&credentials.challenge_id)?;
        let code = sms_code(&credentials.code)?;
        let before_login = self.session_epoch.load(Ordering::SeqCst);
        self.credentials
            .probe()
            .map_err(|_| "credential_store_unavailable".to_string())?;
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
            ("username", mobile),
            ("sms_challenge_id", challenge),
            ("sms_code", code),
            ("scope", "openid profile email offline_access"),
        ];
        let tokens = self.token_request(config, &form).await?;
        self.complete_login(config, tokens, None, epoch).await
    }

    pub(super) async fn public_account_request(
        &self,
        config: &AccountPlatformConfig,
        path: &str,
        body: Value,
    ) -> Result<Value, String> {
        // Only the four fixed paths above are callable; no bearer credential is
        // attached to public authentication requests.
        let url = config
            .account_url
            .join(path)
            .map_err(|_| "account_platform_configuration_invalid".to_string())?;
        let response = self
            .client
            .post(url)
            .header("Accept", "application/json")
            .header("X-Request-ID", random_urlsafe(18))
            .json(&body)
            .send()
            .await
            .map_err(|_| "account_platform_service_unavailable".to_string())?;
        let response = response_to_api(response).await?;
        if !(200..300).contains(&response.status) {
            return Err(public_error(&response.body));
        }
        if !response.body.is_object() {
            return Err("account_platform_response_invalid".into());
        }
        Ok(response.body)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn provider_error_details_never_cross_mobile_boundary() {
        let malicious = serde_json::json!({"code":"SMS_UNAVAILABLE:secret", "message":"secret",
            "details":{"retry_after_seconds":60}});
        assert_eq!(
            public_error(&malicious),
            "mobile_auth_failed:AUTH_INTERNAL_ERROR"
        );
        for retry in [
            serde_json::json!(0),
            serde_json::json!(86401),
            serde_json::json!(-1),
            serde_json::json!(1.5),
            serde_json::json!("60"),
        ] {
            assert_eq!(
                public_error(
                    &serde_json::json!({"code":"SMS_RATE_LIMITED", "details":{"retry_after_seconds":retry}})
                ),
                "mobile_auth_failed:SMS_RATE_LIMITED"
            );
        }
        assert_eq!(
            public_error(
                &serde_json::json!({"code":"SMS_RATE_LIMITED", "details":{"retry_after_seconds":86400}})
            ),
            "mobile_auth_failed:SMS_RATE_LIMITED:86400"
        );
        assert_eq!(
            public_error(
                &serde_json::json!({"code":"SMS_UNAVAILABLE", "details":{"retry_after_seconds":60}})
            ),
            "mobile_auth_failed:SMS_UNAVAILABLE"
        );
    }

    #[test]
    fn sms_grant_errors_do_not_change_refresh_contract() {
        let body = br#"{"error":"invalid_grant","error_description":"private detail"}"#;
        assert_eq!(
            classify_mobile_grant_error(
                &[("sms_challenge_id", "id")],
                StatusCode::BAD_REQUEST,
                body
            ),
            Some("mobile_auth_code_invalid")
        );
        assert_eq!(
            classify_mobile_grant_error(
                &[("grant_type", "refresh_token"), ("sms_challenge_id", "id")],
                StatusCode::BAD_REQUEST,
                body
            ),
            None
        );
        assert_eq!(
            classify_mobile_grant_error(&[], StatusCode::BAD_REQUEST, body),
            None
        );
        assert_eq!(
            classify_mobile_grant_error(&[], StatusCode::INTERNAL_SERVER_ERROR, body),
            None
        );
    }

    #[test]
    fn mobile_inputs_reject_invalid_phone_and_nil_challenge() {
        for phone in ["13800138000", "+8613800138000", "8613800138000"] {
            assert_eq!(mobile_number(phone).unwrap(), "13800138000");
        }
        for phone in [
            "",
            "1380013800",
            "12800138000",
            "1380013800x",
            "13800138000&secret",
        ] {
            assert!(mobile_number(phone).is_err());
        }
        assert!(challenge_id("00000000-0000-0000-0000-000000000000").is_err());
        assert!(sms_code("123\nsecret").is_err());
    }
}
