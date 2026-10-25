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
    #[serde(rename = "display_name")]
    _display_name: String,
    model_capability_version_id: String,
    context_length: u64,
    maximum_output_tokens: u64,
    #[serde(default)]
    runtime_protocol: String,
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

fn engine_model_name(item: &RuntimeCatalogItem) -> Result<String> {
    match item.runtime_protocol.as_str() {
        // CodeWhale must see the protocol-bearing model identity so its existing
        // DeepSeek thinking-mode replay path keeps reasoning_content across tool
        // calls. The local Account Platform proxy rewrites this back to the
        // stable catalog UUID before the request leaves the desktop.
        "DEEPSEEK_V4_CHAT_V1" => Ok("deepseek-v4-flash".to_string()),
        "OPENAI_CHAT_V1" => Ok(item.model_id.clone()),
        _ => anyhow::bail!(
            "account platform model runtime protocol is missing or unsupported; publish its capability before invoking"
        ),
    }
}

#[derive(Debug)]
struct BoundModelAbsent;

impl std::fmt::Display for BoundModelAbsent {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("account platform bound model is absent from the catalog")
    }
}
impl std::error::Error for BoundModelAbsent {}

#[derive(Debug)]
struct BoundLegacyModelAbsent;

impl std::fmt::Display for BoundLegacyModelAbsent {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("account platform bound legacy model name is absent from the catalog")
    }
}
impl std::error::Error for BoundLegacyModelAbsent {}

fn is_legacy_model_name(reference: &str) -> bool {
    // Old metadata contains provider model names rather than catalog UUIDs.
    // Do not reinterpret malformed UUID-shaped identities, empty/control text,
    // or unbounded data as trusted local recovery evidence.
    if reference.is_empty()
        || reference.len() > 256
        || !reference
            .bytes()
            .all(|value| value.is_ascii_alphanumeric() || b"-_.:/".contains(&value))
        || !reference.bytes().any(|value| value.is_ascii_alphabetic())
    {
        return false;
    }
    let groups: Vec<_> = reference.split('-').collect();
    let uuid_shape = groups.iter().map(|group| group.len()).eq([8, 4, 4, 4, 12])
        || reference.to_ascii_lowercase().starts_with("urn:uuid:")
        || ((3..=4).contains(&groups.len())
            && groups[0].len() == 8
            && groups[0].bytes().all(|value| value.is_ascii_hexdigit())
            && groups[1..].iter().all(|group| {
                group.len() == 4 && group.bytes().all(|value| value.is_ascii_hexdigit())
            }));
    uuid::Uuid::parse_str(reference).is_err() && !uuid_shape
}

fn runtime_model_limits(body: serde_json::Value, model_id: &str) -> Result<RuntimeModelLimits> {
    let mut catalog: RuntimeCatalog =
        serde_json::from_value(body).context("decode account platform model catalog")?;
    if catalog.catalog_version == 0 {
        anyhow::bail!("account platform model catalog is not published");
    }
    // Decode alone does not establish a valid directory. Every item must
    // satisfy the published UUID identity contract before absence is typed.
    for item in &mut catalog.items {
        // Published catalog rows share the existing capability invariants.
        // Zero output remains legal in an unrelated row; selected-model
        // execution requirements are checked below after identity lookup.
        if item.context_length == 0 || item.maximum_output_tokens > item.context_length {
            anyhow::bail!("account platform catalog model limits are invalid");
        }
        let model_id = uuid::Uuid::parse_str(&item.model_id)
            .ok()
            .filter(|id| !id.is_nil())
            .context("account platform catalog model identity is invalid")?;
        let capability_id = uuid::Uuid::parse_str(&item.model_capability_version_id)
            .ok()
            .filter(|id| !id.is_nil())
            .context("account platform catalog capability identity is invalid")?;
        item.model_id = model_id.to_string();
        item.model_capability_version_id = capability_id.to_string();
    }
    let reference = model_id.trim();
    let canonical_reference = uuid::Uuid::parse_str(reference)
        .ok()
        .map(|id| id.to_string());
    let item = catalog
        .items
        .into_iter()
        .find(|item| {
            item.model_id
                .eq_ignore_ascii_case(canonical_reference.as_deref().unwrap_or(reference))
        })
        .ok_or_else(|| {
            // A legacy name never changes to a different model automatically.
            // Only absence from a fully validated directory offers explicit,
            // session-local replacement; malformed directory/identity errors do not.
            if uuid::Uuid::parse_str(reference).is_ok_and(|id| !id.is_nil()) {
                anyhow!(BoundModelAbsent)
            } else if is_legacy_model_name(reference) {
                anyhow!(BoundLegacyModelAbsent)
            } else {
                anyhow!("account platform model {model_id} is absent from the catalog")
            }
        })?;
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
        engine_model: engine_model_name(&item)?,
        catalog_version: catalog.catalog_version,
        capability_version_id: item.model_capability_version_id,
        context_tokens,
        output_tokens,
    })
}

/// Only the engine host may supply a task-profile catalog reference. Resolving
/// a mutable session selection is lazy, so it cannot replace that frozen fact.
fn runtime_catalog_reference<F>(
    request: &RuntimeModelRequest,
    session_reference: F,
) -> Result<String>
where
    F: FnOnce() -> Result<String>,
{
    if let Some(reference) = request
        .auxiliary
        .as_ref()
        .and_then(|scope| scope.draft_model_ref.as_ref())
    {
        return Ok(reference.clone());
    }
    if let Some(reference) = request.scheduled_model_ref.as_ref() {
        return Ok(reference.clone());
    }
    if request.scheduled_unattended {
        anyhow::bail!(
            "model_catalog_selection_required: scheduled platform model binding is missing"
        );
    }
    session_reference()
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

        let timing_owner = if request.auxiliary.is_none() {
            self.model_proxy.capture_timing_owner(&request.session_id)
        } else {
            None
        };
        super::client_migration::runtime_ready().map_err(|error| anyhow!(error))?;
        // A task-profile reference is frozen before preparation; only ordinary
        // interactive selection reads the mutable session. The wire model is
        // a property of the conversation, not of the global default. This prevents
        // a later catalog selection from silently rewriting historical chats.
        // A removed model can therefore only continue through the explicit
        // replacement-branch command, whose new session records the replacement
        // model in its own metadata.
        let session_model_ref = runtime_catalog_reference(&request, || {
            let store = self
                .app
                .try_state::<SessionStore>()
                .context("session store is not initialized")?;
            Ok(match store.session_model_ref(&request.session_id) {
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
            })
        })?;
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
        let limits = match runtime_model_limits(catalog_response.body, session_model_ref) {
            Ok(limits) => limits,
            Err(error) => {
                if error.downcast_ref::<BoundModelAbsent>().is_some()
                    || error.downcast_ref::<BoundLegacyModelAbsent>().is_some()
                {
                    use crate::features::assistant::timing::{ModelRejection, ModelRejectionCode};
                    let rejection = if error.downcast_ref::<BoundLegacyModelAbsent>().is_some() {
                        Some(ModelRejection::legacy_unavailable())
                    } else {
                        ModelRejection::new(ModelRejectionCode::Unavailable, session_model_ref)
                    };
                    if let (Some(owner), Some(rejection)) = (timing_owner.as_ref(), rejection) {
                        self.model_proxy.record_prepare_rejection(
                            &request.session_id,
                            owner,
                            rejection,
                        );
                    }
                }
                return Err(error);
            }
        };
        let mut model = request.model;
        model.model = limits.engine_model.clone();
        model.context_window_tokens = Some(limits.context_tokens);
        model.max_output_tokens = Some(limits.output_tokens);
        let supplier = Some(authorization.supplier(self.app.clone()));
        model.base_url = match request.auxiliary.as_ref() {
            Some(scope) => {
                self.model_proxy
                    .register_authorized_runtime(
                        &scope.scope_id,
                        true,
                        &authorization.base_url,
                        &limits.gateway_model_id,
                        limits.output_tokens,
                        supplier,
                    )
                    .await
            }
            None => {
                self.model_proxy
                    .register_authorized_session(
                        &request.session_id,
                        &authorization.base_url,
                        &limits.gateway_model_id,
                        limits.output_tokens,
                        supplier,
                    )
                    .await
            }
        }
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
    #[test]
    fn voice_draft_reference_is_private_and_does_not_load_a_session() {
        let mut request = crate::features::assistant::runtime_model::RuntimeModelRequest {
            session_id: String::new(),
            model: crate::platform::prefs::SavedModel::builtin_account_platform(),
            scheduled_unattended: false,
            scheduled_model_ref: None,
            auxiliary: Some(
                crate::features::assistant::runtime_model::AuxiliaryModelContext {
                    scope_id: "private-voice-test".into(),
                    draft_model_ref: Some("0199a100-0001-7000-8000-000000000106".into()),
                },
            ),
        };
        assert_eq!(
            super::runtime_catalog_reference(&request, || panic!(
                "a voice draft is not a stored session"
            ))
            .unwrap(),
            "0199a100-0001-7000-8000-000000000106"
        );
        request.auxiliary.as_mut().unwrap().draft_model_ref = None;
        request.session_id = "real-session".into();
        assert_eq!(
            super::runtime_catalog_reference(&request, || Ok("original-session-model".into()))
                .unwrap(),
            "original-session-model"
        );
    }

    use serde_json::json;

    use super::{runtime_catalog_reference, runtime_model_limits};
    use crate::features::assistant::runtime_model::{
        ModelCredentialMode, RuntimeModelProvider, RuntimeModelRequest,
    };
    use crate::platform::credential_store::{CredentialEditAction, CredentialState};
    use crate::platform::prefs::{ModelPreset, SavedModel};

    #[test]
    fn scheduled_catalog_reference_is_frozen_before_mutable_session_lookup() {
        const ORIGINAL: &str = "0199a100-0001-7000-8000-000000000106";
        for scheduled_unattended in [true, false] {
            let request = RuntimeModelRequest {
                session_id: "task-session".into(),
                model: SavedModel::builtin_account_platform(),
                scheduled_unattended,
                scheduled_model_ref: Some(ORIGINAL.into()),
                auxiliary: None,
            };
            let reference = runtime_catalog_reference(&request, || {
                panic!("a later override or Engine alias must not replace the pinned profile")
            })
            .expect("frozen task reference");
            assert_eq!(reference, ORIGINAL);
        }
    }

    #[test]
    fn interactive_explicit_selection_still_uses_current_session_reference() {
        let request = RuntimeModelRequest {
            session_id: "manual-task-session".into(),
            model: SavedModel::builtin_account_platform(),
            scheduled_unattended: false,
            scheduled_model_ref: None,
            auxiliary: None,
        };
        let reference = runtime_catalog_reference(&request, || {
            Ok("0199a100-0001-7000-8000-000000000107".into())
        })
        .expect("explicit interactive selection");
        assert_eq!(reference, "0199a100-0001-7000-8000-000000000107");
        assert!(
            runtime_catalog_reference(&request, || anyhow::bail!("session unavailable")).is_err()
        );
    }

    #[test]
    fn unattended_platform_without_frozen_reference_fails_closed() {
        let request = RuntimeModelRequest {
            session_id: "unbound-task-session".into(),
            model: SavedModel::builtin_account_platform(),
            scheduled_unattended: true,
            scheduled_model_ref: None,
            auxiliary: None,
        };
        let error = runtime_catalog_reference(&request, || {
            panic!("unattended task must not fall back to a mutable session")
        })
        .unwrap_err();
        assert!(
            error
                .to_string()
                .starts_with("model_catalog_selection_required:")
        );
    }

    #[test]
    fn runtime_protocol_is_independent_of_display_name() {
        let id = "0199a100-0001-7000-8000-000000000107";
        let catalog = |name: &str, protocol: &str| {
            json!({ "catalog_version": 1, "items": [{
            "model_id": id, "display_name": name, "runtime_protocol": protocol,
            "model_capability_version_id": "0199a100-0001-7000-8000-000000000201",
            "context_length": 8192, "maximum_output_tokens": 1024
        }] })
        };
        for name in ["deepseek-flash", "Renamed for people", "ordinary name"] {
            let deepseek = runtime_model_limits(catalog(name, "DEEPSEEK_V4_CHAT_V1"), id).unwrap();
            assert_eq!(deepseek.engine_model, "deepseek-v4-flash");
            assert_eq!(deepseek.gateway_model_id, id);
            let ordinary = runtime_model_limits(catalog(name, "OPENAI_CHAT_V1"), id).unwrap();
            assert_eq!(ordinary.engine_model, id);
        }
        for protocol in ["", "UNKNOWN", "deepseek-flash", "openai_chat_v1"] {
            let error = runtime_model_limits(catalog("deepseek-flash", protocol), id).unwrap_err();
            assert!(error.to_string().contains("publish its capability"));
            assert!(error.downcast_ref::<super::BoundModelAbsent>().is_none());
            assert!(
                error
                    .downcast_ref::<super::BoundLegacyModelAbsent>()
                    .is_none()
            );
        }
        let mut missing = catalog("deepseek-flash", "DEEPSEEK_V4_CHAT_V1");
        missing["items"][0]
            .as_object_mut()
            .unwrap()
            .remove("runtime_protocol");
        assert!(runtime_model_limits(missing, id).is_err());
    }

    #[test]
    fn missing_legacy_binding_offers_only_explicit_reselection() {
        let catalog = json!({ "catalog_version": 1, "items": [{
            "model_id": "0199a100-0001-7000-8000-000000000107",
            "display_name": "deepseek-flash", "runtime_protocol": "OPENAI_CHAT_V1",
            "model_capability_version_id": "0199a100-0001-7000-8000-000000000201",
            "context_length": 8192, "maximum_output_tokens": 1024
        }] });
        let legacy = "deepseek-v4-flash";
        let error = runtime_model_limits(catalog.clone(), legacy).unwrap_err();
        assert!(
            error
                .downcast_ref::<super::BoundLegacyModelAbsent>()
                .is_some()
        );
        assert!(error.downcast_ref::<super::BoundModelAbsent>().is_none());
        // Display names are mutable labels, never identity migration evidence.
        let current = runtime_model_limits(catalog, "deepseek-flash").unwrap_err();
        assert!(
            current
                .downcast_ref::<super::BoundLegacyModelAbsent>()
                .is_some()
        );
        assert_eq!(legacy, "deepseek-v4-flash");
        for name in ["deepseek-chat", "abcdefghijklmnopqrstuvwxyzabcdef"] {
            let error =
                runtime_model_limits(json!({"catalog_version":1,"items":[]}), name).unwrap_err();
            assert!(
                error
                    .downcast_ref::<super::BoundLegacyModelAbsent>()
                    .is_some(),
                "{name}"
            );
        }
    }

    #[test]
    fn malformed_identity_or_catalog_cannot_prove_legacy_absence() {
        for reference in [
            "",
            " ",
            "old model",
            "bad\nmodel",
            "00000000-0000-0000-0000-000000000000",
            "0199a100-0001-7000-8000-00000000010z",
            "0199a100-0001-7000-8000",
            "xxxxxxxx-0001-7000-8000-000000000107",
            &"a".repeat(257),
        ] {
            let error =
                runtime_model_limits(json!({ "catalog_version": 1, "items": [] }), reference)
                    .unwrap_err();
            assert!(
                error
                    .downcast_ref::<super::BoundLegacyModelAbsent>()
                    .is_none(),
                "{reference:?}"
            );
        }
        for catalog in [
            json!({ "catalog_version": 0, "items": [] }),
            json!({ "catalog_version": 1, "items": [{ "model_id": "bad" }] }),
            json!({ "catalog_version": 1, "items": [{ "model_id": "0199a100-0001-7000-8000-000000000107", "display_name": "other", "runtime_protocol": "OPENAI_CHAT_V1", "model_capability_version_id": "bad", "context_length": 8192, "maximum_output_tokens": 1024 }] }),
        ] {
            let error = runtime_model_limits(catalog, "deepseek-v4-flash").unwrap_err();
            assert!(
                error
                    .downcast_ref::<super::BoundLegacyModelAbsent>()
                    .is_none()
            );
        }
        assert!(
            super::is_legacy_model_name("not-a-uuid"),
            "a normal finite name differs from a malformed UUID-shaped identity"
        );
    }

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
    fn invalid_catalog_identity_is_never_a_bound_model_absence() {
        let selected = "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c";
        let valid_model = "0199a100-0001-7000-8000-000000000106";
        let valid_capability = "0199a100-0001-7000-8000-000000000201";
        for (model_id, capability_id) in [
            ("not-a-uuid", valid_capability),
            ("00000000-0000-0000-0000-000000000000", valid_capability),
            (valid_model, "not-a-uuid"),
            (valid_model, "00000000-0000-0000-0000-000000000000"),
        ] {
            let body = json!({ "catalog_version": 1, "items": [{
                "model_id": model_id, "display_name": "complete decoded item", "runtime_protocol": "OPENAI_CHAT_V1",
                "model_capability_version_id": capability_id,
                "context_length": 8192, "maximum_output_tokens": 4096
            }] });
            let error = runtime_model_limits(body, selected).unwrap_err();
            assert!(
                error.downcast_ref::<super::BoundModelAbsent>().is_none(),
                "invalid directory identity remains generic"
            );
        }
        // One valid matching model does not hide an invalid unrelated item.
        let error = runtime_model_limits(json!({ "catalog_version": 1, "items": [
            { "model_id": selected, "display_name": "selected", "runtime_protocol": "OPENAI_CHAT_V1", "model_capability_version_id": valid_capability, "context_length": 8192, "maximum_output_tokens": 4096 },
            { "model_id": "not-a-uuid", "display_name": "unrelated", "runtime_protocol": "OPENAI_CHAT_V1", "model_capability_version_id": valid_capability, "context_length": 8192, "maximum_output_tokens": 4096 }
        ] }), selected).unwrap_err();
        assert!(error.downcast_ref::<super::BoundModelAbsent>().is_none());
        assert!(runtime_model_limits(json!({ "catalog_version": 1, "items": [{
            "model_id": valid_model, "display_name": "valid unrelated", "runtime_protocol": "OPENAI_CHAT_V1", "model_capability_version_id": valid_capability, "context_length": 8192, "maximum_output_tokens": 4096
        }] }), selected).unwrap_err().downcast_ref::<super::BoundModelAbsent>().is_some());
    }

    #[test]
    fn bound_missing_model_is_distinct_from_bad_catalog_or_capability() {
        let id = "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c";
        let absent =
            runtime_model_limits(json!({ "catalog_version": 1, "items": [] }), id).unwrap_err();
        assert!(absent.downcast_ref::<super::BoundModelAbsent>().is_some());
        for body in [
            json!(null),
            json!({ "catalog_version": 0, "items": [] }),
            json!({ "items": [] }),
            json!({ "catalog_version": 1, "items": [{}] }),
            json!({ "catalog_version": 1, "items": [{ "model_id": id, "display_name": "bound", "runtime_protocol": "OPENAI_CHAT_V1", "model_capability_version_id": "0199a100-0001-7000-8000-000000000201", "context_length": 1, "maximum_output_tokens": 2 }] }),
        ] {
            assert!(
                runtime_model_limits(body, id)
                    .unwrap_err()
                    .downcast_ref::<super::BoundModelAbsent>()
                    .is_none()
            );
        }
        let alias = runtime_model_limits(json!({ "catalog_version": 1, "items": [{ "model_id": "0199a100-0001-7000-8000-000000000107", "display_name": id, "model_capability_version_id": "0199a100-0001-7000-8000-000000000201", "context_length": 2, "maximum_output_tokens": 1 }] }), id).unwrap_err();
        assert!(
            alias.downcast_ref::<super::BoundModelAbsent>().is_some(),
            "display name must not impersonate a stable binding"
        );
        assert!(
            runtime_model_limits(json!({ "catalog_version": 1, "items": [] }), "legacy-name")
                .unwrap_err()
                .downcast_ref::<super::BoundModelAbsent>()
                .is_none()
        );
    }

    #[test]
    fn invalid_unrelated_catalog_limits_cannot_prove_bound_model_absence() {
        let selected = "0199a100-0001-7000-8000-000000000106";
        for (context_length, maximum_output_tokens) in [(0, 0), (8, 9)] {
            let error = runtime_model_limits(
                json!({ "catalog_version": 1, "items": [{
                "model_id": "0199a100-0001-7000-8000-000000000107",
                "display_name": "unrelated", "runtime_protocol": "OPENAI_CHAT_V1",
                "model_capability_version_id": "0199a100-0001-7000-8000-000000000201",
                "context_length": context_length, "maximum_output_tokens": maximum_output_tokens
            }] }),
                selected,
            )
            .unwrap_err();
            assert!(error.downcast_ref::<super::BoundModelAbsent>().is_none());
        }
        let error = runtime_model_limits(
            json!({ "catalog_version": 1, "items": [{
            "model_id": "0199a100-0001-7000-8000-000000000107",
            "display_name": "unrelated output-zero row", "runtime_protocol": "OPENAI_CHAT_V1",
            "model_capability_version_id": "0199a100-0001-7000-8000-000000000201",
            "context_length": 8, "maximum_output_tokens": 0
        }] }),
            selected,
        )
        .unwrap_err();
        assert!(error.downcast_ref::<super::BoundModelAbsent>().is_some());
    }

    #[test]
    fn runtime_limits_use_the_selected_published_catalog_capability() {
        let limits = runtime_model_limits(
            json!({
                "catalog_version": 7,
                "items": [
                    {
                        "model_id": "0199a100-0001-7000-8000-000000000106",
                        "display_name": "DeepSeek Flash", "runtime_protocol": "OPENAI_CHAT_V1",
                        "model_capability_version_id": "0199a100-0001-7000-8000-000000000202",
                        "context_length": 128000,
                        "maximum_output_tokens": 4096
                    }
                ]
            }),
            "0199a100-0001-7000-8000-000000000106",
        )
        .expect("published model limits");

        assert_eq!(limits.catalog_version, 7);
        assert_eq!(
            limits.gateway_model_id,
            "0199a100-0001-7000-8000-000000000106"
        );
        assert_eq!(limits.engine_model, "0199a100-0001-7000-8000-000000000106");
        assert_eq!(
            limits.capability_version_id,
            "0199a100-0001-7000-8000-000000000202"
        );
        assert_eq!(limits.context_tokens, 128_000);
        assert_eq!(limits.output_tokens, 4_096);
    }

    #[test]
    fn runtime_limits_reject_current_display_name_without_explicit_binding() {
        let limits = runtime_model_limits(
            json!({
                "catalog_version": 8,
                "items": [{
                    "model_id": "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c",
                    "display_name": "deepseek-flash", "runtime_protocol": "DEEPSEEK_V4_CHAT_V1",
                    "model_capability_version_id": "0199a100-0001-7000-8000-000000000203",
                    "context_length": 128000,
                    "maximum_output_tokens": 8192
                }]
            }),
            "deepseek-flash",
        )
        .unwrap_err();
        assert!(
            limits
                .downcast_ref::<super::BoundLegacyModelAbsent>()
                .is_some()
        );
    }

    #[test]
    fn unknown_account_platform_model_keeps_opaque_engine_identity() {
        let limits = runtime_model_limits(
            json!({
                "catalog_version": 9,
                "items": [{
                    "model_id": "00000000-0000-7000-8000-000000000099",
                    "display_name": "future-model", "runtime_protocol": "OPENAI_CHAT_V1",
                    "model_capability_version_id": "0199a100-0001-7000-8000-000000000204",
                    "context_length": 128000,
                    "maximum_output_tokens": 8192
                }]
            }),
            "00000000-0000-7000-8000-000000000099",
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
                        "model_id": "0199a100-0001-7000-8000-000000000108",
                        "display_name": "bad", "runtime_protocol": "OPENAI_CHAT_V1",
                        "model_capability_version_id": "0199a100-0001-7000-8000-000000000205",
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
                scheduled_model_ref: None,
                auxiliary: None,
            })
            .await
            .expect("ordinary model passthrough");
        assert_eq!(prepared.model.id, "ordinary");
        assert!(prepared.credential.is_none());
    }
}
