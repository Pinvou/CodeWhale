//! Same logical password-reset retries retain the first opaque password reference.
//! Only a digest of request facts is retained in process memory; no password,
//! SMS code, handle, or recovery credential crosses into the WebView or a file.
use super::*;
use sha2::{Digest, Sha256};

pub(super) struct PendingReset {
    facts: [u8; 32],
    epoch: u64,
    reference: registration::PasswordReference,
}

impl AccountPlatformState {
    pub(super) async fn reset_password_operation(
        &self,
        config: &AccountPlatformConfig,
        password: String,
        mobile: String,
        challenge: String,
        code: String,
    ) -> Result<Value, String> {
        let epoch = self.session_epoch.load(Ordering::SeqCst);
        // Serialize preparation and transmission: concurrent retries must not
        // generate two references for the same challenge/request facts.
        let mut pending = self.password_reset_memory.lock().await;
        self.require_session_epoch(epoch)?;
        let encoded = serde_json::to_vec(&(
            config.issuer.as_str(),
            config.account_url.as_str(),
            config.identity_url.as_ref().map(Url::as_str),
            &mobile,
            &challenge,
            &code,
            &password,
        ))
        .map_err(|_| "account_platform_response_invalid".to_string())?;
        let facts: [u8; 32] = Sha256::digest(&encoded).into();
        if pending
            .as_ref()
            .is_some_and(|entry| entry.facts == facts && entry.epoch != epoch)
        {
            return Err("authentication_required: account session changed".into());
        }
        if !pending.as_ref().is_some_and(|entry| entry.facts == facts) {
            let reference = self.password_precheck(config, password).await?;
            self.require_session_epoch(epoch)?;
            *pending = Some(PendingReset {
                facts,
                epoch,
                reference,
            });
        }
        let reference = &pending
            .as_ref()
            .expect("prepared password reference")
            .reference;
        // Do not replace an expired reference after an uncertain send. Account
        // owns the persisted reset operation and Identity owns idempotent revoke.
        let result = self
            .public_account_request(
                config,
                "v1/auth/password-resets",
                serde_json::json!({"mobile": mobile, "challenge_id": challenge, "code": code,
                "password_handle": reference.password_handle,
                "password_policy_version": reference.password_policy_version}),
            )
            .await?;
        *pending = None;
        drop(pending);
        self.logout_if_epoch(epoch).await?;
        Ok(result)
    }
}
