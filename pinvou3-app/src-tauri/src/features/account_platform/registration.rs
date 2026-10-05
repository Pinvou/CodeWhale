//! Registration password references and query credentials stay separate from
//! the verified OAuth session. Pending credentials use the OS keyring only.
use super::*;

const PENDING_KEY: &str = "pending-registration";

#[derive(Deserialize)]
pub(super) struct PasswordReference {
    pub password_handle: String,
    pub password_policy_version: String,
    pub expires_at: String,
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct PendingRegistration {
    pub operation_id: String,
    pub query_token: String,
    pub username: String,
    pub mobile: String,
    pub expires_at: String,
    issuer: String,
    account_url: String,
}

pub(super) struct PendingMemory {
    pub epoch: u64,
    pub persisted: bool,
    pub pending: PendingRegistration,
}

pub(super) fn password_reference(handle: &str, version: &str) -> Result<(), String> {
    if [handle, version]
        .iter()
        .any(|value| value.is_empty() || value.len() > 4096 || value.chars().any(char::is_control))
    {
        return Err("identity_adapter_response_invalid".into());
    }
    Ok(())
}

fn valid_expiry(value: &str) -> bool {
    chrono::DateTime::parse_from_rfc3339(value).is_ok_and(|time| time > chrono::Utc::now())
}

pub(super) fn validate_result(result: &Value, operation_id: &str) -> Result<(), String> {
    if result
        .get("registration_operation_id")
        .and_then(Value::as_str)
        != Some(operation_id)
        || result
            .get("password_preserved")
            .and_then(Value::as_bool)
            .is_none()
        || !matches!(
            result.get("status").and_then(Value::as_str),
            Some(
                "SMS_PENDING"
                    | "REGISTRATION_IN_PROGRESS"
                    | "IDENTITY_CREATING"
                    | "IDENTITY_CREATED"
                    | "ACCOUNT_COMMITTING"
                    | "ACCOUNT_COMMITTED"
                    | "PROVISIONING"
                    | "ACTIVE"
                    | "REPAIR_REQUIRED"
                    | "COMPENSATION_PENDING"
                    | "COMPENSATED"
                    | "EXPIRED"
            )
        )
    {
        return Err("account_platform_response_invalid".into());
    }
    Ok(())
}

impl AccountPlatformState {
    pub(super) async fn password_precheck(
        &self,
        config: &AccountPlatformConfig,
        password: String,
    ) -> Result<PasswordReference, String> {
        let base = config
            .identity_url
            .as_ref()
            .ok_or_else(|| "account_platform_configuration_missing".to_string())?;
        let url = base
            .join("v1/registration/password-prechecks")
            .map_err(|_| "account_platform_configuration_invalid".to_string())?;
        let response = self
            .client
            .post(url)
            .header("Accept", "application/json")
            .header("X-Request-ID", random_urlsafe(18))
            .json(&serde_json::json!({"password": password}))
            .send()
            .await
            .map_err(|_| "identity_adapter_unavailable".to_string())?;
        let response = response_to_api(response).await?;
        if !(200..300).contains(&response.status) {
            return Err(mobile::public_error(&response.body));
        }
        let reference: PasswordReference = serde_json::from_value(response.body)
            .map_err(|_| "identity_adapter_response_invalid".to_string())?;
        password_reference(
            &reference.password_handle,
            &reference.password_policy_version,
        )?;
        if !valid_expiry(&reference.expires_at) {
            return Err("identity_adapter_response_invalid".into());
        }
        Ok(reference)
    }

    pub(super) async fn remember_pending_registration(
        &self,
        config: &AccountPlatformConfig,
        username: &str,
        mobile: &str,
        epoch: u64,
        result: &mut Value,
    ) -> Result<(), String> {
        let field = |name: &str| {
            result
                .get(name)
                .and_then(Value::as_str)
                .ok_or_else(|| "account_platform_response_invalid".to_string())
        };
        let operation_id = field("registration_operation_id")?;
        uuid::Uuid::parse_str(operation_id)
            .ok()
            .filter(|id| !id.is_nil())
            .ok_or_else(|| "account_platform_response_invalid".to_string())?;
        let query_token = field("registration_query_token")?;
        let expires_at = field("registration_query_token_expires_at")?;
        if query_token.is_empty()
            || query_token.len() > 4096
            || query_token.chars().any(char::is_control)
            || !valid_expiry(expires_at)
        {
            return Err("account_platform_response_invalid".into());
        }
        let pending = PendingRegistration {
            operation_id: operation_id.into(),
            query_token: query_token.into(),
            username: username.trim().into(),
            mobile: mobile.into(),
            expires_at: expires_at.into(),
            issuer: config.issuer.clone(),
            account_url: config.account_url.to_string(),
        };
        let encoded = serde_json::to_string(&pending)
            .map_err(|_| "registration_recovery_invalid".to_string())?;
        let mut memory = self.registration_memory.lock().await;
        self.require_session_epoch(epoch)?;
        let persisted = self.registration_credentials.probe().is_ok()
            && self
                .registration_credentials
                .set(PENDING_KEY, &encoded)
                .is_ok();
        self.require_session_epoch(epoch)?;
        self.registration_epoch.store(epoch, Ordering::SeqCst);
        *memory = Some(PendingMemory {
            epoch,
            persisted,
            pending,
        });
        let object = result
            .as_object_mut()
            .ok_or_else(|| "account_platform_response_invalid".to_string())?;
        object.insert("recovery_persisted".into(), Value::Bool(persisted));
        object.remove("registration_query_token");
        // The query credential remains Native-only even when the OS keyring is
        // unavailable. Process memory is lost at restart; no file fallback.
        Ok(())
    }

    pub(super) async fn pending_registration(
        &self,
        config: &AccountPlatformConfig,
    ) -> Result<Option<PendingRegistration>, String> {
        let mut memory = self.registration_memory.lock().await;
        // OS persistence is permitted at process startup, but must not revive
        // a previous process-local identity epoch after login or logout.
        if self.registration_epoch.load(Ordering::SeqCst)
            != self.session_epoch.load(Ordering::SeqCst)
        {
            *memory = None;
            return Ok(None);
        }
        if memory
            .as_ref()
            .is_some_and(|value| value.epoch != self.session_epoch.load(Ordering::SeqCst))
        {
            *memory = None;
        }
        if let Some(value) = memory.as_ref() {
            if value.pending.issuer != config.issuer
                || value.pending.account_url != config.account_url.as_str()
            {
                return Err("registration_recovery_conflict".into());
            }
            return Ok(Some(value.pending.clone()));
        }
        let Some(encoded) = self
            .registration_credentials
            .get(PENDING_KEY)
            .map_err(|_| "credential_store_failed".to_string())?
        else {
            return Ok(None);
        };
        let pending: PendingRegistration = serde_json::from_str(&encoded)
            .map_err(|_| "registration_recovery_invalid".to_string())?;
        if pending.issuer != config.issuer || pending.account_url != config.account_url.as_str() {
            return Err("registration_recovery_conflict".into());
        }
        uuid::Uuid::parse_str(&pending.operation_id)
            .ok()
            .filter(|id| !id.is_nil())
            .ok_or_else(|| "registration_recovery_invalid".to_string())?;
        if pending.query_token.is_empty()
            || pending.query_token.len() > 4096
            || pending.query_token.chars().any(char::is_control)
        {
            return Err("registration_recovery_invalid".into());
        }
        Ok(Some(pending))
    }

    pub async fn registration_status(&self) -> Result<Value, String> {
        let _registration = self.registration_guard.lock().await;
        self.registration_status_with(&AccountPlatformConfig::from_env()?)
            .await
    }

    pub(super) async fn registration_status_with(
        &self,
        config: &AccountPlatformConfig,
    ) -> Result<Value, String> {
        let epoch = self.session_epoch.load(Ordering::SeqCst);
        let Some(pending) = self.pending_registration(config).await? else {
            return Ok(Value::Null);
        };
        let url = config
            .account_url
            .join(&format!(
                "v1/auth/registration-operations/{}",
                pending.operation_id
            ))
            .map_err(|_| "account_platform_configuration_invalid".to_string())?;
        let response = self
            .client
            .get(url)
            .header("Accept", "application/json")
            .header("X-Request-ID", random_urlsafe(18))
            .header("X-Registration-Query-Token", &pending.query_token)
            .send()
            .await
            .map_err(|_| "account_platform_service_unavailable".to_string())?;
        let response = response_to_api(response).await?;
        if !(200..300).contains(&response.status) {
            return Err("registration_recovery_unavailable".into());
        }
        self.require_session_epoch(epoch)?;
        let mut result = response.body;
        validate_result(&result, &pending.operation_id)?;
        let object = result
            .as_object_mut()
            .ok_or_else(|| "account_platform_response_invalid".to_string())?;
        if object
            .get("registration_operation_id")
            .and_then(Value::as_str)
            != Some(pending.operation_id.as_str())
        {
            return Err("account_platform_response_invalid".into());
        }
        if let Some(username) = object.get("username").and_then(Value::as_str) {
            if !username.is_empty() && username != pending.username {
                return Err("account_platform_response_invalid".into());
            }
        }
        object.insert("username".into(), Value::String(pending.username));
        if matches!(
            object.get("status").and_then(Value::as_str),
            Some("ACTIVE" | "COMPENSATED" | "EXPIRED")
        ) {
            let mut memory = self.registration_memory.lock().await;
            let persisted = match memory.as_ref() {
                Some(value)
                    if value.pending.operation_id == pending.operation_id
                        && value.epoch == epoch =>
                {
                    value.persisted
                }
                Some(_) => return Err("registration_recovery_conflict".into()),
                None => true,
            };
            if persisted {
                self.registration_credentials
                    .delete(PENDING_KEY)
                    .map_err(|_| "credential_store_failed".to_string())?;
            }
            *memory = None;
        }
        Ok(result)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use codewhale_secrets::InMemoryKeyringStore;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    struct Credentials {
        memory: InMemoryKeyringStore,
        fail: bool,
    }
    impl DesktopCredentials for Credentials {
        fn probe(&self) -> Result<(), SecretsError> {
            if self.fail {
                Err(SecretsError::Keyring("fixture-only-private-detail".into()))
            } else {
                Ok(())
            }
        }
    }
    impl KeyringStore for Credentials {
        fn get(&self, key: &str) -> Result<Option<String>, SecretsError> {
            self.probe()?;
            self.memory.get(key)
        }
        fn set(&self, key: &str, value: &str) -> Result<(), SecretsError> {
            self.probe()?;
            self.memory.set(key, value)
        }
        fn delete(&self, key: &str) -> Result<(), SecretsError> {
            self.probe()?;
            self.memory.delete(key)
        }
        fn backend_name(&self) -> &'static str {
            "isolated-fixture"
        }
    }
    fn state(fail: bool) -> AccountPlatformState {
        let mut state = AccountPlatformState::new().unwrap();
        state.registration_credentials = Arc::new(Credentials {
            memory: InMemoryKeyringStore::new(),
            fail,
        });
        state
    }
    fn config(base: Url) -> AccountPlatformConfig {
        AccountPlatformConfig {
            issuer: "https://fixture.invalid/realm".into(),
            jwks_url: base.join("certs").unwrap(),
            token_url: base.join("token").unwrap(),
            client_id: "fixture-desktop".into(),
            identity_url: Some(base.clone()),
            account_url: base.clone(),
            token_gateway_url: base,
        }
    }
    async fn http(status: u16, body: Value) -> (Url, tokio::task::JoinHandle<String>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let base = Url::parse(&format!("http://{}/", listener.local_addr().unwrap())).unwrap();
        let task = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut bytes = Vec::new();
            loop {
                let mut part = [0u8; 4096];
                let size = stream.read(&mut part).await.unwrap();
                if size == 0 {
                    break;
                }
                bytes.extend_from_slice(&part[..size]);
                if let Some(end) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
                    let headers = String::from_utf8_lossy(&bytes[..end]).to_ascii_lowercase();
                    let length = headers
                        .lines()
                        .find_map(|line| line.strip_prefix("content-length: "))
                        .and_then(|value| value.parse::<usize>().ok())
                        .unwrap_or(0);
                    if bytes.len() >= end + 4 + length {
                        break;
                    }
                }
            }
            let body = serde_json::to_vec(&body).unwrap();
            stream.write_all(format!("HTTP/1.1 {status} Fixture\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",body.len()).as_bytes()).await.unwrap();
            stream.write_all(&body).await.unwrap();
            String::from_utf8(bytes).unwrap()
        });
        (base, task)
    }
    const OP: &str = "4c947809-a2e8-4ef4-851b-f424984d1825";
    fn sms() -> Value {
        serde_json::json!({"registration_operation_id":OP,"registration_query_token":"fixture-query-proof","registration_query_token_expires_at":"2099-01-01T00:00:00Z"})
    }
    #[tokio::test]
    async fn password_precheck_sends_secret_only_in_fixed_bounded_post() {
        let (base,request)=http(200,serde_json::json!({"password_handle":"fixture-handle","password_policy_version":"v1","expires_at":"2099-01-01T00:00:00Z"})).await;
        let result = state(false)
            .password_precheck(&config(base), "FixturePassword123".into())
            .await
            .unwrap();
        assert_eq!(result.password_handle, "fixture-handle");
        let request = request.await.unwrap();
        let first = request.lines().next().unwrap();
        assert_eq!(first, "POST /v1/registration/password-prechecks HTTP/1.1");
        assert!(!first.contains("FixturePassword"));
        assert!(!request.to_ascii_lowercase().contains("authorization:"));
        let body = request.split("\r\n\r\n").nth(1).unwrap();
        assert_eq!(
            serde_json::from_str::<Value>(body).unwrap(),
            serde_json::json!({"password":"FixturePassword123"})
        );
    }
    #[tokio::test]
    async fn password_reference_bad_shapes_expiry_and_private_errors_fail_closed() {
        for body in [
            serde_json::json!({}),
            serde_json::json!({"password_handle":"h","password_policy_version":"v","expires_at":"2000-01-01T00:00:00Z"}),
            serde_json::json!({"password_handle":"h\nprivate","password_policy_version":"v","expires_at":"2099-01-01T00:00:00Z"}),
        ] {
            let (base, request) = http(200, body).await;
            assert_eq!(
                state(false)
                    .password_precheck(&config(base), "FixturePassword123".into())
                    .await
                    .err()
                    .unwrap(),
                "identity_adapter_response_invalid"
            );
            request.await.unwrap();
        }
        let (base, request) = http(
            503,
            serde_json::json!({"code":"untrusted-secret","message":"private-body"}),
        )
        .await;
        assert_eq!(
            state(false)
                .password_precheck(&config(base), "FixturePassword123".into())
                .await
                .err()
                .unwrap(),
            "mobile_auth_failed:AUTH_INTERNAL_ERROR"
        );
        request.await.unwrap();
    }
    #[tokio::test]
    async fn pending_proof_is_native_only_and_restart_binds_exact_authority() {
        let state = state(false);
        let cfg = config(Url::parse("http://127.0.0.1:33333/").unwrap());
        let mut body = sms();
        state
            .remember_pending_registration(&cfg, "FixtureUser", "13800000000", 0, &mut body)
            .await
            .unwrap();
        assert_eq!(body.get("recovery_persisted"), Some(&Value::Bool(true)));
        assert!(body.get("registration_query_token").is_none());
        assert_eq!(
            state
                .pending_registration(&cfg)
                .await
                .unwrap()
                .unwrap()
                .operation_id,
            OP
        );
        *state.registration_memory.lock().await = None;
        assert_eq!(
            state
                .pending_registration(&cfg)
                .await
                .unwrap()
                .unwrap()
                .query_token,
            "fixture-query-proof"
        );
        let mut changed = cfg.clone();
        changed.issuer = "https://different.invalid".into();
        assert_eq!(
            state.pending_registration(&changed).await.err().unwrap(),
            "registration_recovery_conflict"
        );
    }
    #[tokio::test]
    async fn unavailable_keyring_keeps_only_same_process_proof_and_epoch_invalidates_it() {
        let state = state(true);
        let cfg = config(Url::parse("http://127.0.0.1:33333/").unwrap());
        let mut body = sms();
        state
            .remember_pending_registration(&cfg, "FixtureUser", "13800000000", 0, &mut body)
            .await
            .unwrap();
        assert_eq!(body.get("recovery_persisted"), Some(&Value::Bool(false)));
        assert!(body.get("registration_query_token").is_none());
        assert_eq!(
            state
                .pending_registration(&cfg)
                .await
                .unwrap()
                .unwrap()
                .operation_id,
            OP
        );
        state.session_epoch.fetch_add(1, Ordering::SeqCst);
        assert!(state.pending_registration(&cfg).await.unwrap().is_none());
        assert!(state.registration_memory.lock().await.is_none());
    }
    #[tokio::test]
    async fn status_uses_fixed_uuid_path_header_proof_and_closes_only_terminal_owned_record() {
        let (base,request)=http(200,serde_json::json!({"registration_operation_id":OP,"status":"ACTIVE","password_preserved":true})).await;
        let state = state(true);
        let cfg = config(base);
        let mut body = sms();
        state
            .remember_pending_registration(&cfg, "FixtureUser", "13800000000", 0, &mut body)
            .await
            .unwrap();
        let result = state.registration_status_with(&cfg).await.unwrap();
        assert_eq!(result["username"], "FixtureUser");
        assert_eq!(result["password_preserved"], true);
        assert!(state.registration_memory.lock().await.is_none());
        let request = request.await.unwrap();
        assert_eq!(
            request.lines().next().unwrap(),
            format!("GET /v1/auth/registration-operations/{OP} HTTP/1.1")
        );
        assert!(
            request
                .to_ascii_lowercase()
                .contains("x-registration-query-token: fixture-query-proof")
        );
        assert!(
            !request
                .lines()
                .next()
                .unwrap()
                .contains("fixture-query-proof")
        );
        assert!(!request.to_ascii_lowercase().contains("authorization:"));
    }
    #[tokio::test]
    async fn malformed_or_failed_status_keeps_current_proof_for_retry() {
        for (status, body) in [
            (404, serde_json::json!({})),
            (
                200,
                serde_json::json!({"registration_operation_id":"different"}),
            ),
            (
                200,
                serde_json::json!({"registration_operation_id":OP,"status":"ACTIVE"}),
            ),
            (
                200,
                serde_json::json!({"registration_operation_id":OP,"status":"PREPARED","password_preserved":false}),
            ),
        ] {
            let (base, request) = http(status, body).await;
            let state = state(true);
            let cfg = config(base);
            let mut body = sms();
            state
                .remember_pending_registration(&cfg, "FixtureUser", "13800000000", 0, &mut body)
                .await
                .unwrap();
            assert!(state.registration_status_with(&cfg).await.is_err());
            assert_eq!(
                state
                    .pending_registration(&cfg)
                    .await
                    .unwrap()
                    .unwrap()
                    .operation_id,
                OP
            );
            request.await.unwrap();
        }
    }

    #[tokio::test]
    async fn persisted_proof_cannot_revive_after_process_identity_epoch_changes() {
        let state = state(false);
        let cfg = config(Url::parse("http://127.0.0.1:33333/").unwrap());
        state
            .remember_pending_registration(&cfg, "FixtureUser", "13800000000", 0, &mut sms())
            .await
            .unwrap();
        state.session_epoch.fetch_add(1, Ordering::SeqCst);
        for _ in 0..2 {
            assert!(state.pending_registration(&cfg).await.unwrap().is_none());
        }
        assert!(
            state
                .registration_credentials
                .get(PENDING_KEY)
                .unwrap()
                .is_some()
        );
        state
            .remember_pending_registration(&cfg, "NewUser", "13900000000", 1, &mut sms())
            .await
            .unwrap();
        assert_eq!(
            state
                .pending_registration(&cfg)
                .await
                .unwrap()
                .unwrap()
                .username,
            "NewUser"
        );
    }
}
