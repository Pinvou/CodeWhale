use std::time::{Duration, Instant};

use anyhow::{anyhow, bail, Context, Result};
use async_trait::async_trait;

use crate::features::assistant::runtime_model::{
    ModelCredentialMode, PreparedRuntimeModel, RuntimeModelCredential, RuntimeModelProvider,
    RuntimeModelRequest,
};
use crate::platform::credential_store::{CredentialStore, SystemCredentialStore};
use crate::platform::prefs::SavedModel;

const PREPARE_TIMEOUT: Duration = Duration::from_secs(25);

/// Official 内置模型的运行时提供器。
///
/// 公共 EnginePool 只认识 RuntimeModelProvider；账户开通、凭据读取和内置模型路由
/// 都封装在这里，避免聊天、定时任务和 bridge 各自维护一套专用分支。
#[derive(Debug, Default)]
pub(crate) struct LlmApiRuntimeModelProvider;

fn prepare_builtin_model() -> Result<PreparedRuntimeModel> {
    crate::features::llmapi_hub::provisioning::ensure_binding_for_current_user()
        .map_err(|error| anyhow!(error.to_tauri_error()))?;
    let model =
        crate::features::llmapi_hub::provisioning::ready_saved_model_from_local_binding_system()
            .map_err(|error| anyhow!(error.to_tauri_error()))?;
    let reference = model
        .credential_ref
        .clone()
        .context("内置模型凭据引用缺失")?;
    let api_key = SystemCredentialStore::new()
        .get(&reference)?
        .context("内置模型凭据缺失，请重试开通")?;
    let credential = RuntimeModelCredential::api_key(api_key)?;

    Ok(PreparedRuntimeModel {
        model,
        credential: Some(credential),
        revision: Some(format!("credential-v{}", reference.version)),
    })
}

#[async_trait]
impl RuntimeModelProvider for LlmApiRuntimeModelProvider {
    fn credential_mode(
        &self,
        model: &SavedModel,
        user_api_key_required: bool,
    ) -> ModelCredentialMode {
        if model.is_builtin_llmapi() {
            ModelCredentialMode::BackendManaged
        } else if user_api_key_required {
            ModelCredentialMode::UserManaged
        } else {
            ModelCredentialMode::None
        }
    }

    async fn prepare(&self, request: RuntimeModelRequest) -> Result<PreparedRuntimeModel> {
        if !request.model.is_builtin_llmapi() {
            return Ok(PreparedRuntimeModel::unchanged(request.model));
        }

        let started_at = Instant::now();
        log::info!(
            "[pinvou3][runtime_model] builtin preparation start sid={} scheduled_unattended={}",
            request.session_id,
            request.scheduled_unattended
        );
        let task = tokio::task::spawn_blocking(prepare_builtin_model);
        let prepared = match tokio::time::timeout(PREPARE_TIMEOUT, task).await {
            Ok(Ok(result)) => result,
            Ok(Err(error)) => Err(anyhow!("内置模型凭据准备任务失败：{error}")),
            Err(_) => {
                bail!("内置模型凭据准备超时，请稍后重试");
            }
        }?;
        log::info!(
            "[pinvou3][runtime_model] builtin preparation ok sid={} elapsed_ms={}",
            request.session_id,
            started_at.elapsed().as_millis()
        );
        Ok(prepared)
    }
}

#[cfg(test)]
mod tests {
    use super::LlmApiRuntimeModelProvider;
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
        let provider = LlmApiRuntimeModelProvider;
        assert_eq!(
            provider.credential_mode(&SavedModel::builtin_llmapi(), true),
            ModelCredentialMode::BackendManaged
        );
        assert_eq!(
            provider.credential_mode(&ordinary_model(), true),
            ModelCredentialMode::UserManaged
        );
    }

    #[tokio::test]
    async fn ordinary_model_does_not_touch_private_provisioning() {
        let provider = LlmApiRuntimeModelProvider;
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
}
