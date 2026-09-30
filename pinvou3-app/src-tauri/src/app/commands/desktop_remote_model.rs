//! Windows remote temporary-model commands (DEV-15).
//!
//! The frontend only ever sees opaque model IDs, display names and an
//! "not exportable" reason. Building the v1 package, resolving the API key from
//! the OS credential store and binding it to the task all happen in Rust, and
//! the package is handed to the task admission path as one atomic step.

use std::sync::Arc;

use serde::Serialize;
use serde_json::Value;
use tauri::State;

use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::temporary_model::{
    SharedTemporaryModelStore, TemporaryModelBinding, TemporaryModelError, TemporaryModelPackageV1,
    TemporaryModelStore, export_decision, project_saved_model,
};
use crate::platform::credential_store::{CredentialStore, SystemCredentialStore};
use crate::platform::prefs::{SavedModel, UserPrefs};

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

/// Frontend projection of one saved model. No key, no `credential_ref` and no
/// package content cross this boundary.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct RemoteModelDescriptor {
    model_id: String,
    display_name: String,
    exportable: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    reason: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    vision_model_id: Option<String>,
}

fn display_name(model: &SavedModel) -> String {
    let trimmed = model.name.trim();
    if !trimmed.is_empty() {
        return trimmed.to_owned();
    }
    if let Some(alias) = model.alias.as_deref().map(str::trim) {
        if !alias.is_empty() {
            return alias.to_owned();
        }
    }
    model.model.clone()
}

pub(crate) fn project_descriptor(model: &SavedModel) -> RemoteModelDescriptor {
    let decision = export_decision(model);
    RemoteModelDescriptor {
        model_id: model.id.clone(),
        display_name: display_name(model),
        exportable: decision.exportable,
        reason: decision.reason.map(str::to_owned),
        vision_model_id: decision
            .exportable
            .then(|| model.vision_model_id.clone())
            .flatten(),
    }
}

/// Builds the v1 package for one task. `package_id` is supplied by the caller
/// so a retry of the same task keeps a stable identity.
pub(crate) fn build_task_package(
    live: &LeaseContext,
    primary: &SavedModel,
    vision: Option<&SavedModel>,
    task_id: &str,
    package_id: &str,
) -> Result<TemporaryModelPackageV1, TemporaryModelError> {
    let epoch = live
        .lease_epoch
        .parse::<u64>()
        .map_err(|_| TemporaryModelError {
            code: "model_binding_invalid",
        })?;
    let package = TemporaryModelPackageV1 {
        package_id: package_id.to_owned(),
        binding: TemporaryModelBinding {
            connection_id: live.connection_id.clone(),
            controller_lease_id: live.controller_lease_id.clone(),
            lease_epoch: epoch,
            task_id: task_id.to_owned(),
        },
        primary: project_saved_model(primary)?,
        vision_fallback: vision.map(project_saved_model).transpose()?,
    };
    package.validate()?;
    Ok(package)
}

fn hydrate_export_secret(
    mut model: SavedModel,
    store: &impl CredentialStore,
) -> Result<SavedModel, TemporaryModelError> {
    if let Some(reference) = model.credential_ref.as_ref() {
        model.api_key = store
            .get(reference)
            .map_err(|_| TemporaryModelError {
                code: "model_credential_unavailable",
            })?
            .filter(|value| !value.trim().is_empty())
            .ok_or(TemporaryModelError {
                code: "model_credential_missing",
            })?;
    }
    Ok(model)
}

fn prepare_export_models(
    prefs: &UserPrefs,
    primary: SavedModel,
    store: &impl CredentialStore,
) -> Result<(SavedModel, Option<SavedModel>), TemporaryModelError> {
    let vision = match primary.vision_model_id.as_deref() {
        None => None,
        Some(id) if id == primary.id => {
            return Err(TemporaryModelError {
                code: "model_vision_reference_cycle",
            });
        }
        Some(id) => {
            let model = prefs.model_by_id(id).cloned().ok_or(TemporaryModelError {
                code: "model_vision_reference_missing",
            })?;
            if model.vision_model_id.is_some() {
                return Err(TemporaryModelError {
                    code: "model_vision_reference_nested",
                });
            }
            Some(model)
        }
    };
    let primary = hydrate_export_secret(primary, store)?;
    let vision = vision
        .map(|model| hydrate_export_secret(model, store))
        .transpose()?;
    // Project both routes here so list/exportability and actual package build
    // use exactly the same acceptance boundary.
    project_saved_model(&primary)?;
    if let Some(vision) = vision.as_ref() {
        project_saved_model(vision)?;
    }
    Ok((primary, vision))
}

/// Resolve A's current active model and build the task-bound package entirely
/// in Rust. The returned value goes straight into the encrypted business
/// payload; it is never projected through the WebView.
pub(crate) fn build_active_task_package(
    live: &LeaseContext,
    task_id: &str,
) -> Result<Value, TemporaryModelError> {
    let prefs = UserPrefs::load();
    let primary = prefs.active_model().cloned().ok_or(TemporaryModelError {
        code: "model_not_configured",
    })?;
    let credentials = SystemCredentialStore::new();
    let (primary, vision) = prepare_export_models(&prefs, primary, &credentials)?;
    build_task_package(
        live,
        &primary,
        vision.as_ref(),
        task_id,
        &format!("package-{}", uuid::Uuid::new_v4().simple()),
    )
    .map(|package| package.to_wire_value())
}

#[tauri::command]
pub async fn desktop_remote_model_list_exportable(
    controlled: State<'_, ControlledDeviceManager>,
) -> Result<Vec<RemoteModelDescriptor>, String> {
    if !controlled.is_supported() {
        return Err(UNSUPPORTED.to_owned());
    }
    let prefs = UserPrefs::load();
    let credentials = SystemCredentialStore::new();
    Ok(prefs
        .advanced
        .saved_models
        .iter()
        .map(|model| {
            prepare_export_models(&prefs, model.clone(), &credentials)
                .map(|(hydrated, _)| project_descriptor(&hydrated))
                .unwrap_or_else(|error| RemoteModelDescriptor {
                    model_id: model.id.clone(),
                    display_name: display_name(model),
                    exportable: false,
                    reason: Some(error.code.to_owned()),
                    vision_model_id: None,
                })
        })
        .collect())
}

#[tauri::command]
pub fn desktop_remote_model_install(
    package: Value,
    task_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    store: State<'_, SharedTemporaryModelStore>,
) -> Result<Value, String> {
    if !controlled.is_supported() {
        return Err(UNSUPPORTED.to_owned());
    }
    let live = controlled
        .live_lease()
        .map_err(|error| error.code.to_owned())?;
    let package = TemporaryModelPackageV1::from_wire_value(&package)
        .map_err(|error| error.code.to_owned())?;
    store
        .install(package, &live, &task_id, monotonic_millis())
        .map_err(|error| error.code.to_owned())?;
    store
        .redacted_route(&task_id, &live)
        .map_err(|error| error.code.to_owned())
}

#[tauri::command]
pub fn desktop_remote_model_release(
    task_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    store: State<'_, SharedTemporaryModelStore>,
) -> Result<bool, String> {
    if !controlled.is_supported() {
        return Err(UNSUPPORTED.to_owned());
    }
    Ok(store.clear_task(&task_id))
}

#[tauri::command]
pub fn desktop_remote_model_active(
    controlled: State<'_, ControlledDeviceManager>,
    store: State<'_, SharedTemporaryModelStore>,
) -> Result<Option<String>, String> {
    if !controlled.is_supported() {
        return Err(UNSUPPORTED.to_owned());
    }
    Ok(store.active_task())
}

fn monotonic_millis() -> u64 {
    use std::sync::OnceLock;
    use std::time::Instant;
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

/// Shared state builder for DEV-20: the store doubles as the DEV-13 model
/// cleanup hook.
pub(crate) fn shared_model_store() -> SharedTemporaryModelStore {
    Arc::new(TemporaryModelStore::default())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::platform::credential_store::{CredentialError, CredentialReference};
    use crate::platform::prefs::{
        ImageCapabilityOverride, MODEL_PROVIDER_KIND_CODING_PLAN, ModelPreset,
    };

    fn model(id: &str) -> SavedModel {
        SavedModel {
            id: id.into(),
            name: "DeepSeek".into(),
            alias: None,
            preset: ModelPreset::Deepseek,
            context_window_tokens: None,
            max_output_tokens: None,
            reasoning_effort: None,
            model: "deepseek-chat".into(),
            base_url: "https://api.deepseek.com/v1".into(),
            provider_kind: Some("official_api".into()),
            vendor: Some("deepseek".into()),
            endpoint_mode: None,
            image_capability_override: ImageCapabilityOverride::Pinvou,
            vision_model_id: None,
            api_key: "sk-value".into(),
            credential_ref: None,
            credential_state: Default::default(),
            has_secret: true,
            credential_action: None,
        }
    }

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "3".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    struct NoCredentials;

    struct EnvRestore(Vec<(&'static str, Option<std::ffi::OsString>)>);

    impl EnvRestore {
        fn capture(names: &[&'static str]) -> Self {
            Self(
                names
                    .iter()
                    .map(|name| (*name, std::env::var_os(name)))
                    .collect(),
            )
        }
    }

    impl Drop for EnvRestore {
        fn drop(&mut self) {
            for (name, value) in &self.0 {
                // SAFETY: tests that construct EnvRestore hold the crate-wide ENV_LOCK.
                unsafe {
                    match value {
                        Some(value) => std::env::set_var(name, value),
                        None => std::env::remove_var(name),
                    }
                }
            }
        }
    }

    impl CredentialStore for NoCredentials {
        fn get(&self, _reference: &CredentialReference) -> Result<Option<String>, CredentialError> {
            Ok(None)
        }

        fn set(
            &self,
            _reference: &CredentialReference,
            _value: &str,
        ) -> Result<(), CredentialError> {
            Ok(())
        }

        fn delete(&self, _reference: &CredentialReference) -> Result<(), CredentialError> {
            Ok(())
        }
    }

    #[test]
    fn descriptor_projection_never_leaks_credentials_or_refs() {
        let descriptor = project_descriptor(&model("model-0001"));
        let encoded = serde_json::to_string(&descriptor).unwrap();
        assert!(descriptor.exportable);
        assert!(!encoded.contains("sk-value"));
        assert!(!encoded.contains("credential"));
        assert!(encoded.contains("DeepSeek"));
    }

    #[test]
    fn descriptor_reports_the_reason_for_non_exportable_models() {
        let mut builtin = model(crate::platform::prefs::BUILTIN_LLMAPI_MODEL_ID);
        builtin.name = "内置模型".into();
        let descriptor = project_descriptor(&builtin);
        assert!(!descriptor.exportable);
        assert_eq!(
            descriptor.reason.as_deref(),
            Some("model_not_exportable_builtin")
        );

        let mut plan = model("model-0002");
        plan.provider_kind = Some(MODEL_PROVIDER_KIND_CODING_PLAN.to_owned());
        assert_eq!(
            project_descriptor(&plan).reason.as_deref(),
            Some("model_not_exportable_subscription")
        );
    }

    #[test]
    fn package_builder_binds_the_task_and_rejects_a_bad_epoch() {
        let package = build_task_package(
            &lease(),
            &model("model-0001"),
            None,
            "task-0001",
            "package-0001",
        )
        .unwrap();
        assert_eq!(package.binding.task_id, "task-0001");
        assert_eq!(package.binding.lease_epoch, 3);
        assert!(package.vision_fallback.is_none());

        let vision = model("model-vision");
        let package = build_task_package(
            &lease(),
            &model("model-0001"),
            Some(&vision),
            "task-0001",
            "package-0001",
        )
        .unwrap();
        assert!(package.vision_fallback.is_some());

        let mut broken = lease();
        broken.lease_epoch = "not-a-number".into();
        assert_eq!(
            build_task_package(
                &broken,
                &model("model-0001"),
                None,
                "task-0001",
                "package-0001"
            )
            .unwrap_err()
            .code,
            "model_binding_invalid"
        );
    }

    #[test]
    fn export_rejects_missing_cyclic_and_nested_vision_references() {
        let mut primary = model("primary");
        primary.vision_model_id = Some("missing".into());
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![primary.clone()];
        assert_eq!(
            prepare_export_models(&prefs, primary, &NoCredentials)
                .unwrap_err()
                .code,
            "model_vision_reference_missing"
        );

        let mut primary = model("primary");
        primary.vision_model_id = Some("primary".into());
        prefs.advanced.saved_models = vec![primary.clone()];
        assert_eq!(
            prepare_export_models(&prefs, primary, &NoCredentials)
                .unwrap_err()
                .code,
            "model_vision_reference_cycle"
        );

        let mut primary = model("primary");
        primary.vision_model_id = Some("vision".into());
        let mut vision = model("vision");
        vision.vision_model_id = Some("third".into());
        prefs.advanced.saved_models = vec![primary.clone(), vision];
        assert_eq!(
            prepare_export_models(&prefs, primary, &NoCredentials)
                .unwrap_err()
                .code,
            "model_vision_reference_nested"
        );
    }

    #[test]
    fn export_prepares_one_layer_vision_route_with_its_own_credential() {
        let mut primary = model("primary");
        primary.vision_model_id = Some("vision".into());
        let mut vision = model("vision");
        vision.api_key = "sk-vision".into();
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![primary.clone(), vision];

        let (primary, vision) = prepare_export_models(&prefs, primary, &NoCredentials).unwrap();
        assert_eq!(primary.api_key, "sk-value");
        assert_eq!(vision.unwrap().api_key, "sk-vision");
    }

    #[test]
    fn export_ignores_process_model_environment_as_one_route_group() {
        let _lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let _restore = EnvRestore::capture(&[
            "DEEPSEEK_PROVIDER",
            "DEEPSEEK_MODEL",
            "DEEPSEEK_BASE_URL",
            "DEEPSEEK_API_KEY",
        ]);
        // SAFETY: this test holds the crate-wide ENV_LOCK.
        unsafe {
            std::env::set_var("DEEPSEEK_PROVIDER", "vllm");
            std::env::set_var("DEEPSEEK_MODEL", "environment-model");
            std::env::set_var("DEEPSEEK_BASE_URL", "https://environment.invalid/v1");
            std::env::set_var("DEEPSEEK_API_KEY", "environment-key");
        }
        let primary = model("primary");
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![primary.clone()];

        let (exported, vision) = prepare_export_models(&prefs, primary, &NoCredentials).unwrap();

        assert_eq!(exported.preset, ModelPreset::Deepseek);
        assert_eq!(exported.model, "deepseek-chat");
        assert_eq!(exported.base_url, "https://api.deepseek.com/v1");
        assert_eq!(exported.api_key, "sk-value");
        assert!(vision.is_none());
    }

    #[test]
    fn shared_store_is_empty_on_creation() {
        let store = shared_model_store();
        assert!(store.is_empty());
        assert!(store.active_task().is_none());
    }
}
