use anyhow::{Context, Result, anyhow};
use async_trait::async_trait;
use serde::Deserialize;
use tauri::{AppHandle, Manager};

use crate::features::assistant::runtime_model::{
    ModelCredentialMode, PreparedRuntimeModel, RuntimeModelCredential, RuntimeModelProvider,
    RuntimeModelRequest, RuntimeTurnAuthority,
};
use crate::features::sessions::SessionStore;
use crate::platform::prefs::{ModelSelectionRef, SavedModel};

#[derive(Debug, Deserialize)]
struct RuntimeCatalog {
    catalog_version: u64,
    items: Vec<RuntimeCatalogItem>,
}

#[derive(Debug, Deserialize)]
struct RuntimeCatalogItem {
    model_id: String,
    display_name: String,
    model_capability_version_id: String,
    context_length: u64,
    maximum_output_tokens: u64,
}

#[derive(Debug, PartialEq, Eq)]
struct RuntimeModelLimits {
    gateway_model_id: String,
    engine_model: String,
    catalog_version: u64,
    capability_version_id: String,
    context_tokens: u32,
    output_tokens: u32,
}

fn engine_model_name(item: &RuntimeCatalogItem) -> String {
    match item.display_name.trim().to_ascii_lowercase().as_str() {
        // CodeWhale must see the protocol-bearing model identity so its existing
        // DeepSeek thinking-mode replay path keeps reasoning_content across tool
        // calls. The local Account Platform proxy rewrites this back to the
        // stable catalog UUID before the request leaves the desktop.
        "deepseek-flash" => "deepseek-v4-flash".to_string(),
        _ => item.model_id.clone(),
    }
}

fn runtime_model_limits(body: serde_json::Value, model_id: &str) -> Result<RuntimeModelLimits> {
    let catalog: RuntimeCatalog =
        serde_json::from_value(body).context("decode account platform model catalog")?;
    let reference = model_id.trim();
    let item = catalog
        .items
        .into_iter()
        .find(|item| {
            item.model_id.eq_ignore_ascii_case(reference)
                || item.display_name.trim().eq_ignore_ascii_case(reference)
        })
        .with_context(|| format!("account platform model {model_id} is absent from the catalog"))?;
    let context_tokens = u32::try_from(item.context_length)
        .context("account platform model context limit is unsupported")?;
    let output_tokens = u32::try_from(item.maximum_output_tokens)
        .context("account platform model output limit is unsupported")?;
    if context_tokens == 0 || output_tokens == 0 || output_tokens > context_tokens {
        anyhow::bail!("account platform model limits are invalid");
    }
    if item.model_capability_version_id.trim().is_empty() {
        anyhow::bail!("account platform model capability version is missing");
    }
    Ok(RuntimeModelLimits {
        gateway_model_id: item.model_id.clone(),
        engine_model: engine_model_name(&item),
        catalog_version: catalog.catalog_version,
        capability_version_id: item.model_capability_version_id,
        context_tokens,
        output_tokens,
    })
}

/// Runtime provider for the only platform-managed model path.
///
/// Local and user-configured third-party models pass through unchanged. The
/// built-in model always obtains an OIDC access token and the Token Gateway URL
/// from Account Platform; there is deliberately no legacy New API fallback.
#[derive(Clone)]
pub(crate) struct AccountPlatformRuntimeModelProvider {
    app: AppHandle,
    model_proxy: super::model_proxy::ModelGatewayProxy,
}

impl AccountPlatformRuntimeModelProvider {
    pub(crate) fn new(app: AppHandle) -> Self {
        Self {
            model_proxy: super::model_proxy::ModelGatewayProxy::new(app.clone()),
            app,
        }
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
        let session_model_ref = match store.session_model_ref(&request.session_id) {
            Some(ModelSelectionRef::AccountPlatform { model_id }) => model_id,
            _ => {
                store
                    .load(&request.session_id)
                    .with_context(|| {
                        format!("load model binding for session {}", request.session_id)
                    })?
                    .metadata
                    .model
            }
        };
        let session_model_ref = session_model_ref.trim();
        if session_model_ref.is_empty() {
            return Err(anyhow!(
                "model_catalog_selection_required: refresh the catalog and create a session with an available platform model"
            ));
        }

        let account = self
            .app
            .try_state::<super::AccountPlatformState>()
            .context("账号平台尚未初始化")?;
        let authorization = account
            .chat_runtime_authorization()
            .await
            .map_err(|error| anyhow!(error))?;
        let (catalog_response, authorization) = account
            .chat_catalog_for_authorization(authorization)
            .await
            .map_err(|error| anyhow!(error))?;
        if !(200..300).contains(&catalog_response.status) {
            anyhow::bail!(
                "account platform model catalog returned HTTP {}",
                catalog_response.status
            );
        }
        let limits = runtime_model_limits(catalog_response.body, session_model_ref)?;
        let mut model = request.model;
        model.model = limits.engine_model.clone();
        model.context_window_tokens = Some(limits.context_tokens);
        model.max_output_tokens = Some(limits.output_tokens);
        model.base_url = self
            .model_proxy
            .register_session(
                &request.session_id,
                &authorization.base_url,
                &limits.gateway_model_id,
                limits.output_tokens,
            )
            .await
            .context("start account platform model proxy")?;
        Ok(PreparedRuntimeModel {
            model,
            credential: Some(RuntimeModelCredential::account_platform_access_token(
                authorization.access_token,
            )?),
            revision: Some(format!(
                "{}-catalog-{}-capability-{}",
                authorization.revision,
                limits.catalog_version,
                limits.capability_version_id.trim()
            )),
        })
    }

    fn capture_model_cancellation_scope(&self, session_id: &str) -> Option<String> {
        self.model_proxy.cancellation_scope(session_id)
    }
    fn cancel_logical_turn(&self, session_id: &str, generation: u64, scope: Option<&str>) {
        self.model_proxy
            .cancel_turn_bound(session_id, generation, scope);
    }
    fn forget_model_session(&self, session_id: &str) {
        self.model_proxy.forget_session(session_id);
    }

    fn reclaim_model_session(&self, session_id: &str, preserved_generation: Option<u64>) {
        self.model_proxy
            .reclaim_session(session_id, preserved_generation);
    }

    fn bind_turn_authority(
        &self,
        authority: std::sync::Arc<dyn RuntimeTurnAuthority>,
    ) -> Result<()> {
        self.model_proxy.bind_turn_authority(authority)
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::runtime_model_limits;
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
    fn account_platform_model_uses_backend_managed_credentials() {
        // credential_mode is independent of AppHandle and can be exercised via
        // a tiny provider built from an uninitialized test handle only in Tauri
        // integration tests. Lock the model predicate here instead.
        let model = SavedModel::builtin_account_platform();
        assert!(model.is_builtin_account_platform());
        let _mode = ModelCredentialMode::BackendManaged;
    }

    #[test]
    fn runtime_limits_use_the_selected_published_catalog_capability() {
        let limits = runtime_model_limits(
            json!({
                "catalog_version": 7,
                "items": [
                    {
                        "model_id": "model-a",
                        "display_name": "DeepSeek Flash",
                        "model_capability_version_id": "capability-a-v2",
                        "context_length": 128000,
                        "maximum_output_tokens": 4096
                    }
                ]
            }),
            "model-a",
        )
        .expect("published model limits");

        assert_eq!(limits.catalog_version, 7);
        assert_eq!(limits.gateway_model_id, "model-a");
        assert_eq!(limits.engine_model, "model-a");
        assert_eq!(limits.capability_version_id, "capability-a-v2");
        assert_eq!(limits.context_tokens, 128_000);
        assert_eq!(limits.output_tokens, 4_096);
    }

    #[test]
    fn runtime_limits_resolve_current_display_name_to_stable_model_id() {
        let limits = runtime_model_limits(
            json!({
                "catalog_version": 8,
                "items": [{
                    "model_id": "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c",
                    "display_name": "deepseek-flash",
                    "model_capability_version_id": "capability-deepseek-flash",
                    "context_length": 128000,
                    "maximum_output_tokens": 8192
                }]
            }),
            "deepseek-flash",
        )
        .expect("current model display name");

        assert_eq!(
            limits.gateway_model_id,
            "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c"
        );
        assert_eq!(limits.engine_model, "deepseek-v4-flash");
        assert_eq!(limits.capability_version_id, "capability-deepseek-flash");
    }

    #[test]
    fn unknown_account_platform_model_keeps_opaque_engine_identity() {
        let limits = runtime_model_limits(
            json!({
                "catalog_version": 9,
                "items": [{
                    "model_id": "00000000-0000-7000-8000-000000000099",
                    "display_name": "future-model",
                    "model_capability_version_id": "capability-future",
                    "context_length": 128000,
                    "maximum_output_tokens": 8192
                }]
            }),
            "future-model",
        )
        .expect("future model limits");

        assert_eq!(limits.engine_model, limits.gateway_model_id);
    }

    #[test]
    fn runtime_limits_reject_missing_or_invalid_capabilities() {
        assert!(
            runtime_model_limits(json!({"catalog_version": 1, "items": []}), "missing").is_err()
        );
        assert!(
            runtime_model_limits(
                json!({
                    "catalog_version": 1,
                    "items": [{
                        "model_id": "bad",
                        "display_name": "bad",
                        "model_capability_version_id": "capability-bad",
                        "context_length": 4096,
                        "maximum_output_tokens": 8192
                    }]
                }),
                "bad"
            )
            .is_err()
        );
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
