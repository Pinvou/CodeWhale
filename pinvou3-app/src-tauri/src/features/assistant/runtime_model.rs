use anyhow::Result;
use async_trait::async_trait;
use serde::Serialize;
use std::fmt;
use std::sync::Arc;

use crate::platform::prefs::SavedModel;

/// Read-only turn ownership supplied by the engine host. Callbacks run under
/// the lifecycle lock and must not re-enter lifecycle or pool operations.
pub trait RuntimeTurnAuthority: Send + Sync {
    fn current_generation(&self, session_id: &str) -> Option<u64>;
    fn with_generation(
        &self,
        session_id: &str,
        generation: u64,
        callback: &mut dyn FnMut(),
    ) -> bool;
}

/// 模型凭据由谁负责提供。前端只消费这一语义，不需要认识具体模型或认证后端。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ModelCredentialMode {
    None,
    UserManaged,
    BackendManaged,
}

/// 引擎创建或复用前交给运行时模型提供器的上下文。
#[derive(Clone)]
pub struct RuntimeModelRequest {
    pub session_id: String,
    pub model: SavedModel,
    pub scheduled_unattended: bool,
}

/// 运行时提供器准备的敏感模型凭据。
///
/// 凭据只保存在内存中，不参与序列化；`Debug` 固定脱敏，避免 bridge 或测试日志
/// 意外输出明文。存在时它是本次引擎配置的最终凭据，优先于环境变量和本地凭据库。
/// Ordinary provider preparation leaves this absent. The built-in Account
/// Platform route supplies an ephemeral verified access token through the same
/// EnginePool provider seam; it never persists that token in saved settings.
#[derive(Clone, PartialEq, Eq)]
pub struct RuntimeModelCredential {
    api_key: String,
    request_idempotency_header: Option<String>,
}

impl RuntimeModelCredential {
    pub fn api_key(api_key: impl Into<String>) -> Result<Self> {
        let api_key = api_key.into();
        if api_key.trim().is_empty() {
            anyhow::bail!("runtime model API key must not be empty");
        }
        Ok(Self {
            api_key,
            request_idempotency_header: None,
        })
    }

    /// Account Gateway requires one operation key that the foundation generates
    /// and reuses across retries. Ordinary provider credentials do not opt in.
    pub(crate) fn account_platform_access_token(access_token: String) -> Result<Self> {
        let mut credential = Self::api_key(access_token)?;
        credential.request_idempotency_header = Some("Idempotency-Key".into());
        Ok(credential)
    }

    pub(crate) fn request_idempotency_header(&self) -> Option<&str> {
        self.request_idempotency_header.as_deref()
    }

    pub(crate) fn expose_api_key(&self) -> &str {
        &self.api_key
    }
}

impl fmt::Debug for RuntimeModelCredential {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("RuntimeModelCredential([REDACTED])")
    }
}

/// 已完成运行时准备的模型。
///
/// `revision` 只能包含非敏感版本标识（如令牌记录 ID 或更新时间），不得包含 API Key。
/// 当同一会话的 revision 或模型配置变化时，EnginePool 会回收旧引擎并使用新配置重建。
#[derive(Clone, PartialEq, Eq)]
pub struct PreparedRuntimeModel {
    pub model: SavedModel,
    pub credential: Option<RuntimeModelCredential>,
    pub revision: Option<String>,
}

impl PreparedRuntimeModel {
    pub fn unchanged(model: SavedModel) -> Self {
        Self {
            model,
            credential: None,
            revision: None,
        }
    }
}

/// Community 默认实现无需外部服务；Official/Enterprise 可在私有层实现后台托管凭据。
#[async_trait]
pub trait RuntimeModelProvider: Send + Sync {
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

    async fn prepare(&self, request: RuntimeModelRequest) -> Result<PreparedRuntimeModel>;

    fn bind_turn_authority(&self, _authority: Arc<dyn RuntimeTurnAuthority>) -> Result<()> {
        Ok(())
    }

    /// A user turn groups calls; the foundation owns each invocation identity.
    async fn begin_logical_turn(&self, _session_id: &str) -> Result<()> {
        Ok(())
    }
}

#[derive(Debug, Default)]
pub struct PassthroughRuntimeModelProvider;

#[async_trait]
impl RuntimeModelProvider for PassthroughRuntimeModelProvider {
    async fn prepare(&self, request: RuntimeModelRequest) -> Result<PreparedRuntimeModel> {
        Ok(PreparedRuntimeModel::unchanged(request.model))
    }
}

#[cfg(test)]
mod tests {
    use super::{
        ModelCredentialMode, PassthroughRuntimeModelProvider, PreparedRuntimeModel,
        RuntimeModelCredential, RuntimeModelProvider, RuntimeModelRequest,
    };
    use crate::platform::credential_store::{CredentialEditAction, CredentialState};
    use crate::platform::prefs::{ModelPreset, SavedModel};

    /// Official 仍提供带非空校验的公开构造器（account_platform / bridge 层使用）；
    /// 测试统一经它构造，保持与生产路径一致。
    fn credential(api_key: &str) -> RuntimeModelCredential {
        RuntimeModelCredential::api_key(api_key).expect("runtime credential")
    }

    fn model() -> SavedModel {
        SavedModel {
            id: "model-1".to_string(),
            name: "Model 1".to_string(),
            alias: None,
            preset: ModelPreset::OpenaiCompatible,
            context_window_tokens: None,
            max_output_tokens: None,
            reasoning_effort: None,
            model: "model-1".to_string(),
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

    #[tokio::test]
    async fn passthrough_provider_preserves_model_without_private_dependencies() {
        let provider = PassthroughRuntimeModelProvider;
        let prepared = provider
            .prepare(RuntimeModelRequest {
                session_id: "session-1".to_string(),
                model: model(),
                scheduled_unattended: false,
            })
            .await
            .expect("prepare model");

        assert_eq!(prepared.model.id, "model-1");
        assert_eq!(prepared.revision, None);
        assert_eq!(
            provider.credential_mode(&prepared.model, true),
            ModelCredentialMode::UserManaged
        );
        assert_eq!(
            provider.credential_mode(&prepared.model, false),
            ModelCredentialMode::None
        );
    }

    #[test]
    fn credential_modes_have_stable_wire_names() {
        assert_eq!(
            serde_json::to_string(&ModelCredentialMode::BackendManaged)
                .expect("serialize credential mode"),
            "\"backend_managed\""
        );
    }

    #[test]
    fn runtime_credential_debug_output_is_redacted() {
        let rendered = format!("{:?}", credential("runtime-secret"));
        assert!(rendered.contains("[REDACTED]"));
        assert!(!rendered.contains("runtime-secret"));
        // Official 构造器带非空校验；空/空白 key 必须被拒绝。
        assert!(RuntimeModelCredential::api_key("  ").is_err());
    }

    #[test]
    fn account_gateway_operation_header_is_transient_and_affects_runtime_identity() {
        let ordinary = credential("same-in-memory-value");
        let account =
            RuntimeModelCredential::account_platform_access_token("same-in-memory-value".into())
                .unwrap();
        assert!(ordinary.request_idempotency_header().is_none());
        assert_eq!(
            account.request_idempotency_header(),
            Some("Idempotency-Key")
        );
        assert!(ordinary != account);
        assert_eq!(format!("{account:?}"), "RuntimeModelCredential([REDACTED])");
    }

    #[test]
    fn revision_change_requires_engine_rebuild() {
        // EnginePool's rebuild decision (PreparedRuntimeState::requires_rebuild_from)
        // is whole-struct equality: any change to revision or the runtime credential counts as requiring teardown and rebuild.
        let previous = PreparedRuntimeModel {
            model: model(),
            credential: Some(credential("runtime-secret")),
            revision: Some("revision-1".to_string()),
        };
        let unchanged = previous.clone();
        let rotated = PreparedRuntimeModel {
            model: model(),
            credential: Some(credential("runtime-secret")),
            revision: Some("revision-2".to_string()),
        };
        let rotated_credential = PreparedRuntimeModel {
            model: model(),
            credential: Some(credential("runtime-secret-2")),
            revision: Some("revision-1".to_string()),
        };

        assert!(unchanged == previous);
        assert!(rotated != previous);
        assert!(rotated_credential != previous);
    }
}
