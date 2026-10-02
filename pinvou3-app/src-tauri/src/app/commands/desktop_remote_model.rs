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

/// One immutable settings snapshot determines both the opaque active model ID
/// and the exact task-bound package. No settings/keychain write occurs here.
pub(crate) struct PreparedActiveTaskPackage {
    pub(crate) model_profile_id: String,
    pub(crate) package: Value,
}

fn prepare_task_package_from_snapshot(
    prefs: &UserPrefs,
    credentials: &impl CredentialStore,
    live: &LeaseContext,
    task_id: &str,
    package_id: &str,
) -> Result<PreparedActiveTaskPackage, TemporaryModelError> {
    let primary = prefs.active_model().cloned().ok_or(TemporaryModelError {
        code: "model_not_configured",
    })?;
    prepare_model_package_from_snapshot(prefs, primary, credentials, live, task_id, package_id)
}

fn prepare_model_package_from_snapshot(
    prefs: &UserPrefs,
    primary: SavedModel,
    credentials: &impl CredentialStore,
    live: &LeaseContext,
    task_id: &str,
    package_id: &str,
) -> Result<PreparedActiveTaskPackage, TemporaryModelError> {
    let model_profile_id = primary.id.clone();
    let (primary, vision) = prepare_export_models(prefs, primary, credentials)?;
    let package = build_task_package(live, &primary, vision.as_ref(), task_id, package_id)?;
    Ok(PreparedActiveTaskPackage {
        model_profile_id,
        package: package.to_wire_value(),
    })
}

/// Selects an opaque saved-model ID from exactly one read-only settings
/// snapshot. The returned profile identity and exported package cannot come
/// from different model selections, even if settings change during key reads.
pub(crate) fn prepare_selected_task_package_snapshot(
    live: &LeaseContext,
    task_id: &str,
    selected_model_id: &str,
) -> Result<PreparedActiveTaskPackage, TemporaryModelError> {
    prepare_selected_task_package_with_store(
        live,
        task_id,
        selected_model_id,
        &SystemCredentialStore::new(),
        &format!("package-{}", uuid::Uuid::new_v4().simple()),
    )
}

fn prepare_selected_task_package_with_store(
    live: &LeaseContext,
    task_id: &str,
    selected_model_id: &str,
    credentials: &impl CredentialStore,
    package_id: &str,
) -> Result<PreparedActiveTaskPackage, TemporaryModelError> {
    let prefs =
        UserPrefs::load_read_only_model_snapshot().map_err(|code| TemporaryModelError { code })?;
    let primary = prefs
        .model_by_id(selected_model_id)
        .cloned()
        .ok_or(TemporaryModelError {
            code: "model_not_configured",
        })?;
    prepare_model_package_from_snapshot(&prefs, primary, credentials, live, task_id, package_id)
}

/// Fail closed on legacy plaintext credentials; run the normal settings
/// migration separately before retrying. Preparing a remote task must not
/// write settings or keychain before DEV-29's durable pending reservation.
pub(crate) fn prepare_active_task_package_snapshot(
    live: &LeaseContext,
    task_id: &str,
) -> Result<PreparedActiveTaskPackage, TemporaryModelError> {
    prepare_active_task_package_with_store(
        live,
        task_id,
        &SystemCredentialStore::new(),
        &format!("package-{}", uuid::Uuid::new_v4().simple()),
    )
}

fn prepare_active_task_package_with_store(
    live: &LeaseContext,
    task_id: &str,
    credentials: &impl CredentialStore,
    package_id: &str,
) -> Result<PreparedActiveTaskPackage, TemporaryModelError> {
    let prefs =
        UserPrefs::load_read_only_model_snapshot().map_err(|code| TemporaryModelError { code })?;
    prepare_task_package_from_snapshot(&prefs, credentials, live, task_id, package_id)
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
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
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
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
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
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
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
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
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
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicUsize, Ordering};

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

    struct ReadOnlyCredentials {
        reads: AtomicUsize,
        switch_settings_on_read: Mutex<Option<(std::path::PathBuf, Vec<u8>)>>,
    }

    impl ReadOnlyCredentials {
        fn new() -> Self {
            Self {
                reads: AtomicUsize::new(0),
                switch_settings_on_read: Mutex::new(None),
            }
        }
    }

    impl CredentialStore for ReadOnlyCredentials {
        fn get(&self, _reference: &CredentialReference) -> Result<Option<String>, CredentialError> {
            self.reads.fetch_add(1, Ordering::SeqCst);
            if let Some((path, bytes)) = self.switch_settings_on_read.lock().unwrap().take() {
                std::fs::write(path, bytes).unwrap();
            }
            Ok(Some("sk-test-only".into()))
        }

        fn set(
            &self,
            _reference: &CredentialReference,
            _value: &str,
        ) -> Result<(), CredentialError> {
            panic!("remote preparation must not write keychain")
        }

        fn delete(&self, _reference: &CredentialReference) -> Result<(), CredentialError> {
            panic!("remote preparation must not delete keychain")
        }
    }

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
            panic!("package preparation must never write a credential")
        }

        fn delete(&self, _reference: &CredentialReference) -> Result<(), CredentialError> {
            panic!("package preparation must never delete a credential")
        }
    }

    #[test]
    fn prepared_profile_and_package_use_the_same_immutable_snapshot() {
        let mut first = model("first");
        first.model = "first-model".into();
        let mut second = model("second");
        second.model = "second-model".into();
        let mut prefs = UserPrefs::default();
        prefs.advanced.active_model_id = Some(first.id.clone());
        prefs.advanced.saved_models = vec![first, second];
        let captured = prefs.clone();
        // A settings switch after capture cannot make a profile from one
        // snapshot label a package built from another snapshot.
        prefs.advanced.active_model_id = Some("second".into());
        let prepared = prepare_task_package_from_snapshot(
            &captured,
            &NoCredentials,
            &lease(),
            "task-0001",
            "package-0001",
        )
        .unwrap();
        assert_eq!(prepared.model_profile_id, "first");
        assert_eq!(prepared.package["primary"]["model"], "first-model");
        assert_eq!(prepared.package["binding"]["task_id"], "task-0001");
        assert_eq!(prefs.active_model().unwrap().id, "second");
    }

    #[test]
    fn active_prepare_is_read_only_and_uses_one_snapshot_across_external_switch() {
        let _guard = crate::platform::paths::tests::ENV_LOCK.lock().unwrap();
        let _restore = EnvRestore::capture(&["PINVOU3_HOME"]);
        let home = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("PINVOU3_HOME", home.path()) };
        let path = crate::platform::paths::settings_path();
        let mut first = model("first");
        first.model = "first-model".into();
        first.credential_ref = Some(first.credential_reference());
        first.api_key.clear();
        let mut second = model("second");
        second.model = "second-model".into();
        second.credential_ref = Some(second.credential_reference());
        second.api_key.clear();
        let mut prefs = UserPrefs::default();
        prefs.advanced.active_model_id = Some("first".into());
        prefs.advanced.saved_models = vec![first, second];
        let original = serde_json::to_vec(&prefs).unwrap();
        std::fs::write(&path, &original).unwrap();
        let credentials = ReadOnlyCredentials::new();
        let prepared = prepare_active_task_package_with_store(
            &lease(),
            "task-0001",
            &credentials,
            "package-0001",
        )
        .unwrap();
        assert_eq!(prepared.model_profile_id, "first");
        assert_eq!(prepared.package["primary"]["model"], "first-model");
        assert_eq!(credentials.reads.load(Ordering::SeqCst), 1);
        assert_eq!(std::fs::read(&path).unwrap(), original);

        prefs.advanced.active_model_id = Some("second".into());
        let switched = serde_json::to_vec(&prefs).unwrap();
        let switching_credentials = ReadOnlyCredentials::new();
        // Fault injection: an external settings edit occurs during keychain
        // read. The preparation itself still performs no settings write.
        *switching_credentials
            .switch_settings_on_read
            .lock()
            .unwrap() = Some((path.clone(), switched.clone()));
        let prepared = prepare_active_task_package_with_store(
            &lease(),
            "task-0002",
            &switching_credentials,
            "package-0002",
        )
        .unwrap();
        assert_eq!(prepared.model_profile_id, "first");
        assert_eq!(prepared.package["primary"]["model"], "first-model");
        assert_eq!(std::fs::read(&path).unwrap(), switched);
    }

    #[test]
    fn selected_prepare_uses_exact_id_from_one_read_only_snapshot() {
        let _guard = crate::platform::paths::tests::ENV_LOCK.lock().unwrap();
        let _restore = EnvRestore::capture(&["PINVOU3_HOME"]);
        let home = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("PINVOU3_HOME", home.path()) };
        let path = crate::platform::paths::settings_path();
        let mut active = model("active");
        active.model = "active-model".into();
        active.credential_ref = Some(active.credential_reference());
        active.api_key.clear();
        let mut selected = model("selected");
        selected.model = "selected-model".into();
        selected.credential_ref = Some(selected.credential_reference());
        selected.api_key.clear();
        let mut prefs = UserPrefs::default();
        prefs.advanced.active_model_id = Some(active.id.clone());
        prefs.advanced.saved_models = vec![active, selected];
        let original = serde_json::to_vec(&prefs).unwrap();
        std::fs::write(&path, &original).unwrap();
        let credentials = ReadOnlyCredentials::new();
        let prepared = prepare_selected_task_package_with_store(
            &lease(),
            "task-1",
            "selected",
            &credentials,
            "package-1",
        )
        .unwrap();
        assert_eq!(prepared.model_profile_id, "selected");
        assert_eq!(prepared.package["primary"]["model"], "selected-model");
        assert_eq!(credentials.reads.load(Ordering::SeqCst), 1);
        assert_eq!(std::fs::read(&path).unwrap(), original);
        assert_eq!(
            prepare_selected_task_package_with_store(
                &lease(),
                "task-2",
                "missing",
                &credentials,
                "package-2",
            )
            .err()
            .unwrap()
            .code,
            "model_not_configured"
        );
        assert_eq!(credentials.reads.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn active_prepare_rejects_legacy_plaintext_before_credential_access() {
        let _guard = crate::platform::paths::tests::ENV_LOCK.lock().unwrap();
        let _restore = EnvRestore::capture(&["PINVOU3_HOME"]);
        let home = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("PINVOU3_HOME", home.path()) };
        let path = crate::platform::paths::settings_path();
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![model("first")];
        prefs.advanced.active_model_id = Some("first".into());
        let mut settings = serde_json::to_value(&prefs).unwrap();
        settings["advanced"]["saved_models"][0]["api_key"] =
            serde_json::json!("legacy-example-not-real");
        let original = serde_json::to_vec(&settings).unwrap();
        std::fs::write(&path, &original).unwrap();
        let credentials = ReadOnlyCredentials::new();
        let error = prepare_active_task_package_with_store(
            &lease(),
            "task-0001",
            &credentials,
            "package-0001",
        )
        .err()
        .unwrap();
        assert_eq!(error.code, "model_credential_migration_required");
        assert_eq!(credentials.reads.load(Ordering::SeqCst), 0);
        assert_eq!(std::fs::read(&path).unwrap(), original);
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
