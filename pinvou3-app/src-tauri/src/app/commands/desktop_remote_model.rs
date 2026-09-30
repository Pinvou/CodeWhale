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

#[tauri::command]
pub async fn desktop_remote_model_list_exportable(
    controlled: State<'_, ControlledDeviceManager>,
) -> Result<Vec<RemoteModelDescriptor>, String> {
    if !controlled.is_supported() {
        return Err(UNSUPPORTED.to_owned());
    }
    let prefs = UserPrefs::load();
    Ok(prefs
        .advanced
        .saved_models
        .iter()
        .map(project_descriptor)
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
    fn shared_store_is_empty_on_creation() {
        let store = shared_model_store();
        assert!(store.is_empty());
        assert!(store.active_task().is_none());
    }
}
