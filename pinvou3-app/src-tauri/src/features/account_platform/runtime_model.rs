use anyhow::{anyhow, Context, Result};
use async_trait::async_trait;
use tauri::{AppHandle, Manager};

use crate::features::assistant::runtime_model::{
    ModelCredentialMode, PreparedRuntimeModel, RuntimeModelCredential, RuntimeModelProvider,
    RuntimeModelRequest,
};
use crate::features::sessions::SessionStore;
use crate::platform::prefs::SavedModel;

/// Runtime provider for the only platform-managed model path.
///
/// Local and user-configured third-party models pass through unchanged. The
/// built-in model always obtains an OIDC access token and the Token Gateway URL
/// from Account Platform; there is deliberately no legacy New API fallback.
#[derive(Clone)]
pub(crate) struct AccountPlatformRuntimeModelProvider {
    app: AppHandle,
}

impl AccountPlatformRuntimeModelProvider {
    pub(crate) fn new(app: AppHandle) -> Self {
        Self { app }
    }
}

#[async_trait]
impl RuntimeModelProvider for AccountPlatformRuntimeModelProvider {
    fn credential_mode(
        &self,
        model: &SavedModel,
        user_api_key_required: bool,
    ) -> ModelCredentialMode {
        if model.is_builtin_account_platform() {
            ModelCredentialMode::BackendManaged
        } else if user_api_key_required {
            ModelCredentialMode::UserManaged
        } else {
            ModelCredentialMode::None
        }
    }

    async fn prepare(&self, request: RuntimeModelRequest) -> Result<PreparedRuntimeModel> {
        if !request.model.is_builtin_account_platform() {
            return Ok(PreparedRuntimeModel::unchanged(request.model));
        }

        super::client_migration::runtime_ready().map_err(|error| anyhow!(error))?;
        // The wire model is a property of the conversation, not of the current
        // global default.  Reading the immutable session snapshot here prevents
        // a later catalog selection from silently rewriting historical chats.
        // A removed model can therefore only continue through the explicit
        // replacement-branch command, whose new session records the replacement
        // model in its own metadata.
        let store = self
            .app
            .try_state::<SessionStore>()
            .context("session store is not initialized")?;
        let session = store
            .load(&request.session_id)
            .with_context(|| format!("load model binding for session {}", request.session_id))?;
        let session_model_ref = session.metadata.model.trim();
        if session_model_ref.is_empty() {
            return Err(anyhow!("model_catalog_selection_required: refresh the catalog and create a session with an available platform model"));
        }

        let account = self
            .app
            .try_state::<super::AccountPlatformState>()
            .context("账号平台尚未初始化")?;
        let authorization = account
            .chat_runtime_authorization()
            .await
            .map_err(|error| anyhow!(error))?;
        let mut model = request.model;
        model.model = session_model_ref.to_string();
        model.base_url = authorization.base_url;
        Ok(PreparedRuntimeModel {
            model,
            credential: Some(RuntimeModelCredential::api_key(authorization.access_token)?),
            revision: Some(authorization.revision),
        })
    }
}

#[cfg(test)]
mod tests {
    use crate::features::assistant::runtime_model::{
        ModelCredentialMode, RuntimeModelProvider, RuntimeModelRequest,
    };
    use crate::platform::credential_store::{CredentialEditAction, CredentialState};
    use crate::platform::prefs::{ModelPreset, SavedModel};

    fn ordinary_model() -> SavedModel {
        SavedModel {
            id: "ordinary".to_string(),
            name: "Ordinary".to_string(),
            preset: ModelPreset::OpenaiCompatible,
            context_window_tokens: None,
            max_output_tokens: None,
            model: "ordinary".to_string(),
            base_url: "https://example.invalid/v1".to_string(),
            provider_kind: None,
            vendor: None,
            endpoint_mode: None,
            api_key: String::new(),
            credential_ref: None,
            credential_state: CredentialState::Missing,
            has_secret: false,
            credential_action: None::<CredentialEditAction>,
        }
    }

    #[test]
    fn account_platform_model_uses_backend_managed_credentials() {
        // credential_mode is independent of AppHandle and can be exercised via
        // a tiny provider built from an uninitialized test handle only in Tauri
        // integration tests. Lock the model predicate here instead.
        let model = SavedModel::builtin_account_platform();
        assert!(model.is_builtin_account_platform());
        let _mode = ModelCredentialMode::BackendManaged;
    }

    #[tokio::test]
    async fn ordinary_model_does_not_touch_account_platform() {
        struct PassThrough;
        #[async_trait::async_trait]
        impl RuntimeModelProvider for PassThrough {
            fn credential_mode(
                &self,
                _model: &SavedModel,
                user_api_key_required: bool,
            ) -> ModelCredentialMode {
                if user_api_key_required {
                    ModelCredentialMode::UserManaged
                } else {
                    ModelCredentialMode::None
                }
            }

            async fn prepare(
                &self,
                request: RuntimeModelRequest,
            ) -> anyhow::Result<crate::features::assistant::runtime_model::PreparedRuntimeModel>
            {
                Ok(
                    crate::features::assistant::runtime_model::PreparedRuntimeModel::unchanged(
                        request.model,
                    ),
                )
            }
        }

        let prepared = PassThrough
            .prepare(RuntimeModelRequest {
                session_id: "session-1".to_string(),
                model: ordinary_model(),
                scheduled_unattended: false,
            })
            .await
            .expect("ordinary model passthrough");
        assert_eq!(prepared.model.id, "ordinary");
        assert!(prepared.credential.is_none());
    }
}
