use anyhow::{Context, Result, anyhow};
use async_trait::async_trait;
use tauri::{AppHandle, Manager};

use crate::features::assistant::runtime_model::{
    ModelCredentialMode, PreparedRuntimeModel, RuntimeModelCredential, RuntimeModelProvider,
    RuntimeModelRequest,
};
use crate::platform::prefs::SavedModel;

/// Official 内置模型的运行时提供器。
///
/// 公共 EnginePool 只认识 RuntimeModelProvider；账户开通、凭据读取和内置模型路由
/// 都封装在这里，避免聊天、定时任务和 bridge 各自维护一套专用分支。
#[derive(Clone, Default)]
pub(crate) struct AccountPlatformRuntimeModelProvider {
    app: Option<AppHandle>,
}

impl AccountPlatformRuntimeModelProvider {
    pub(crate) fn new(app: AppHandle) -> Self {
        Self { app: Some(app) }
    }

    async fn prepare_account_model(&self, model: SavedModel) -> Result<PreparedRuntimeModel> {
        let result: Result<PreparedRuntimeModel> = async {
            let account = self
                .app
                .as_ref()
                .context("account_platform_service_unavailable")?
                .try_state::<crate::features::account_platform::AccountPlatformState>()
                .context("account_platform_service_unavailable")?;
            let authorization: crate::features::account_platform::ChatRuntimeAuthorization =
                account
                    .chat_runtime_authorization()
                    .await
                    .map_err(|error| anyhow!(error))?;
            prepare_authorized_model(model, authorization)
        }
        .await;
        // This source boundary distinguishes account-runtime failures from
        // identical static codes produced by ordinary providers/connectors.
        result.context("account_platform")
    }
}

/// Final mapping used by the production provider after verified authorization.
pub(super) fn prepare_authorized_model(
    mut model: SavedModel,
    authorization: super::ChatRuntimeAuthorization,
) -> Result<PreparedRuntimeModel> {
    model.base_url = authorization.base_url;
    Ok(PreparedRuntimeModel {
        model,
        credential: Some(RuntimeModelCredential::account_platform_access_token(
            authorization.access_token,
        )?),
        revision: Some(authorization.revision),
    })
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
        self.prepare_account_model(request.model).await
    }
}

#[cfg(test)]
mod tests {
    use super::AccountPlatformRuntimeModelProvider;
    use crate::features::assistant::runtime_model::{
        ModelCredentialMode, RuntimeModelProvider, RuntimeModelRequest,
    };
    use crate::platform::credential_store::{CredentialEditAction, CredentialState};
    use crate::platform::prefs::{ModelPreset, SavedModel};

    fn ordinary_model() -> SavedModel {
        SavedModel {
            id: "ordinary".to_string(),
            name: "Ordinary".to_string(),
            alias: None,
            preset: ModelPreset::OpenaiCompatible,
            context_window_tokens: None,
            max_output_tokens: None,
            reasoning_effort: None,
            model: "ordinary".to_string(),
            base_url: "https://example.invalid/v1".to_string(),
            provider_kind: None,
            vendor: None,
            endpoint_mode: None,
            image_capability_override: Default::default(),
            vision_model_id: None,
            api_key: String::new(),
            credential_ref: None,
            credential_state: CredentialState::Missing,
            has_secret: false,
            credential_action: None::<CredentialEditAction>,
        }
    }

    #[test]
    fn builtin_model_uses_backend_managed_credentials() {
        let provider = AccountPlatformRuntimeModelProvider::default();
        assert_eq!(
            provider.credential_mode(&SavedModel::builtin_account_platform(), true),
            ModelCredentialMode::BackendManaged
        );
        assert_eq!(
            provider.credential_mode(&ordinary_model(), true),
            ModelCredentialMode::UserManaged
        );
    }

    #[tokio::test]
    async fn ordinary_model_does_not_touch_account_platform() {
        let provider = AccountPlatformRuntimeModelProvider::default();
        let prepared = provider
            .prepare(RuntimeModelRequest {
                session_id: "session-1".to_string(),
                model: ordinary_model(),
                scheduled_unattended: false,
            })
            .await
            .expect("ordinary model passthrough");

        assert_eq!(prepared.model.id, "ordinary");
        assert!(prepared.credential.is_none());
        assert!(prepared.revision.is_none());
    }

    #[tokio::test]
    async fn account_runtime_error_keeps_source_and_static_code_for_chat_localization() {
        let error = AccountPlatformRuntimeModelProvider::default()
            .prepare_account_model(SavedModel::builtin_account_platform())
            .await
            .err()
            .expect("an absent app runtime cannot prepare account credentials");
        assert_eq!(
            format!("{error:#}"),
            "account_platform: account_platform_service_unavailable"
        );
    }
}
