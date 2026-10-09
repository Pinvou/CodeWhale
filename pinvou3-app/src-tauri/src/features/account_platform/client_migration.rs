use std::collections::BTreeSet;
use std::path::Path;

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::platform::credential_store::{
    CredentialReference, CredentialStore, SystemCredentialStore,
};
use crate::platform::prefs::{
    BUILTIN_ACCOUNT_PLATFORM_MODEL_ID, LEGACY_BUILTIN_LLMAPI_MODEL_ID, UserPrefs,
};

const MIGRATION_VERSION: u32 = 1;
const REPORT_FILE: &str = "account-platform-model-v1.json";
const LEGACY_IDENTITY_FILES: [&str; 4] = [
    "bindings.json",
    "device.json",
    "identity.json",
    "session.json",
];
const LEGACY_CACHE_FILES: [&str; 3] = ["models.json", "catalog.json", "login-cache.json"];

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum MigrationState {
    Complete,
    CleanupPending,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub(crate) struct ClientModelMigrationReport {
    pub schema_version: u32,
    pub state: MigrationState,
    pub settings_migrated: bool,
    pub session_bindings_migrated: usize,
    pub scheduled_profiles_migrated: usize,
    pub legacy_bindings_discovered: usize,
    pub credentials_removed: usize,
    pub cleanup_errors: Vec<String>,
    #[serde(default)]
    pub legacy_identity_files_discovered: usize,
    #[serde(default)]
    pub legacy_cache_files_discovered: usize,
    #[serde(default)]
    pub preserved_user_models: usize,
}

impl Default for ClientModelMigrationReport {
    fn default() -> Self {
        Self {
            schema_version: MIGRATION_VERSION,
            state: MigrationState::Complete,
            settings_migrated: false,
            session_bindings_migrated: 0,
            scheduled_profiles_migrated: 0,
            legacy_bindings_discovered: 0,
            credentials_removed: 0,
            cleanup_errors: Vec::new(),
            legacy_identity_files_discovered: 0,
            legacy_cache_files_discovered: 0,
            preserved_user_models: 0,
        }
    }
}

pub(crate) fn migrate_if_needed() -> Result<ClientModelMigrationReport> {
    migrate_at(
        &crate::platform::paths::pinvou3_home(),
        &SystemCredentialStore::new(),
    )
}

fn migrate_at<S: CredentialStore>(
    home: &Path,
    credentials: &S,
) -> Result<ClientModelMigrationReport> {
    let migration_dir = home.join("migrations");
    std::fs::create_dir_all(&migration_dir)
        .with_context(|| format!("create migration directory {}", migration_dir.display()))?;
    let report_path = migration_dir.join(REPORT_FILE);
    if let Some(report) = load_completed_report(&report_path)? {
        return Ok(report);
    }

    let settings_path = home.join("settings.json");
    let settings_existed = settings_path
        .try_exists()
        .context("inspect account platform settings before migration")?;
    let settings_backup = migration_dir.join("settings-pre-account-platform-v1.json");
    copy_backup_once(&settings_path, &settings_backup)?;
    let (current_legacy_model_present, mut legacy_model_references) =
        read_legacy_model_credential_references(&settings_path)?;
    // A cleanup retry runs after UserPrefs has removed the legacy model from
    // settings. Its immutable pre-migration backup remains the original source
    // of those credential identities; losing them must not imply completion.
    let (backed_up_legacy_model_present, backed_up_references) =
        read_legacy_model_credential_references(&settings_backup)?;
    legacy_model_references.extend(backed_up_references);
    let legacy_model_present = current_legacy_model_present || backed_up_legacy_model_present;

    // UserPrefs owns the settings schema migration and persists it under the
    // same process-wide lock used by all settings commands.
    let prefs = UserPrefs::load();
    let settings_migrated = prefs.model_path_schema_version == MIGRATION_VERSION
        && prefs
            .model_by_id(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
            .is_some()
        && prefs.model_by_id(LEGACY_BUILTIN_LLMAPI_MODEL_ID).is_none();
    anyhow::ensure!(
        settings_migrated,
        "account platform settings migration did not converge"
    );
    // A fresh installation has no file for UserPrefs::load to normalize and
    // deliberately does not persist defaults during that read. Initialize it
    // through the same locked owner transaction before requiring disk proof.
    // Existing settings are never rescued by this path after a failed save.
    if !settings_existed {
        UserPrefs::update_transaction(|_| Ok(()))
            .map_err(anyhow::Error::msg)
            .context("initialize fresh account platform settings")?;
    }
    // UserPrefs::load may retain its normalized in-memory value when its save
    // fails. Destructive cleanup requires the actual persisted migration, not
    // that in-memory value or a completion report alone.
    let persisted_prefs: UserPrefs = serde_json::from_slice(
        &std::fs::read(&settings_path).context("read persisted account platform settings")?,
    )
    .context("parse persisted account platform settings")?;
    anyhow::ensure!(
        persisted_prefs.model_path_schema_version == MIGRATION_VERSION
            && persisted_prefs
                .model_by_id(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
                .is_some()
            && persisted_prefs
                .model_by_id(LEGACY_BUILTIN_LLMAPI_MODEL_ID)
                .is_none(),
        "account platform settings migration did not persist"
    );

    let mut report = ClientModelMigrationReport {
        settings_migrated,
        ..ClientModelMigrationReport::default()
    };
    let inventory = inventory_at(home)?;
    report.legacy_identity_files_discovered = inventory.legacy_identity_files;
    report.legacy_cache_files_discovered = inventory.legacy_cache_files;
    report.preserved_user_models = inventory.preserved_user_models;
    report.session_bindings_migrated = migrate_model_ids_in_json(
        &home.join("sessions").join("_session_models.json"),
        JsonModelIdScope::EveryStringValue,
    )?;
    report.scheduled_profiles_migrated = migrate_model_ids_in_json(
        &home.join("scheduled-runs").join("session-profiles.json"),
        JsonModelIdScope::ModelIdFields,
    )?;

    let bindings_path = home.join("llmapi-hub").join("bindings.json");
    let legacy_bindings = read_legacy_binding_keys(&bindings_path)?;
    report.legacy_bindings_discovered = legacy_bindings.len();
    let mut references = BTreeSet::new();
    if legacy_model_present || bindings_path.exists() {
        references.insert((
            "pinvou3-llmapi-admin".to_string(),
            "newapi-admin".to_string(),
        ));
        references.insert((
            "pinvou3-model-api-key".to_string(),
            format!("model:{LEGACY_BUILTIN_LLMAPI_MODEL_ID}"),
        ));
    }
    references.extend(legacy_model_references);
    for (user_id, device_id) in &legacy_bindings {
        references.insert((
            "pinvou3-llmapi-token".to_string(),
            format!("llmapi:{user_id}:{device_id}"),
        ));
        references.insert((
            "pinvou3-llmapi-user-session".to_string(),
            format!("llmapi-session:{user_id}:{device_id}"),
        ));
        references.insert((
            "pinvou3-llmapi-user-password".to_string(),
            format!("llmapi-password:{user_id}:{device_id}"),
        ));
    }
    // The keyring is keyed by (service, account). A legacy model can share a
    // reference with a retained user model; that credential is still owned by
    // the retained model and must survive migration and subsequent retries.
    let retained_model_references: BTreeSet<_> = prefs
        .advanced
        .saved_models
        .iter()
        .filter(|model| model.id != LEGACY_BUILTIN_LLMAPI_MODEL_ID)
        .filter_map(|model| model.credential_ref.as_ref())
        .map(|reference| {
            (
                reference.service.trim().to_string(),
                reference.account.trim().to_string(),
            )
        })
        .collect();
    references.retain(|reference| !retained_model_references.contains(reference));
    for (service, account) in references {
        let reference = CredentialReference {
            service,
            account,
            version: 1,
        };
        let inspection = credentials.get(&reference);
        // A fallback store miss cannot prove that the unavailable OS keyring
        // has no legacy credential. Keep the cleanup retryable in that state.
        if credentials.os_keyring_unreachable(&reference) {
            report.cleanup_errors.push(format!(
                "credential_inspection_unavailable:{}",
                reference.service
            ));
            continue;
        }
        match inspection {
            Ok(None) => {}
            Ok(Some(_)) => match credentials.delete(&reference) {
                Ok(()) => report.credentials_removed += 1,
                Err(error) => report.cleanup_errors.push(format!(
                    "credential_cleanup_failed:{}:{}",
                    reference.service,
                    error.user_message()
                )),
            },
            Err(error) => report.cleanup_errors.push(format!(
                "credential_inspection_failed:{}:{}",
                reference.service,
                error.user_message()
            )),
        }
    }

    cleanup_legacy_files(home, &migration_dir, &mut report)?;

    if !report.cleanup_errors.is_empty() {
        report.state = MigrationState::CleanupPending;
    }
    persist_report(&report_path, &report)?;
    Ok(report)
}

fn cleanup_legacy_files(
    home: &Path,
    migration_dir: &Path,
    report: &mut ClientModelMigrationReport,
) -> Result<()> {
    let legacy_root = home.join("llmapi-hub");
    let files: Vec<_> = LEGACY_IDENTITY_FILES
        .iter()
        .chain(LEGACY_CACHE_FILES.iter())
        .map(|name| (name, legacy_root.join(name)))
        .filter(|(_, path)| path.exists())
        .collect();
    for (name, source) in &files {
        let backup = migration_dir.join(format!(
            "legacy-llmapi-{}-v1.json",
            name.trim_end_matches(".json")
        ));
        if let Err(error) = copy_backup_once(source, &backup) {
            report
                .cleanup_errors
                .push(format!("legacy_backup_failed:{name}:{error:#}"));
        }
    }
    if report.cleanup_errors.is_empty() {
        for (name, source) in files {
            if let Err(error) = std::fs::remove_file(&source) {
                report
                    .cleanup_errors
                    .push(format!("legacy_cleanup_failed:{name}:{error}"));
            }
        }
        if report.cleanup_errors.is_empty() {
            let _ = std::fs::remove_dir(&legacy_root);
        }
    }
    Ok(())
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub(crate) struct ClientMigrationPreflight {
    pub schema_version: u32,
    pub legacy_identity_files: usize,
    pub legacy_cache_files: usize,
    pub legacy_credential_references: usize,
    pub preserved_user_models: usize,
    pub legacy_selection_present: bool,
    pub settings_valid: bool,
    pub session_bindings_valid: bool,
    pub scheduled_profiles_valid: bool,
}

/// Performs the install-gate inventory and migration simulation without
/// writing configuration or opening credential values.
pub(crate) fn preflight() -> Result<ClientMigrationPreflight> {
    preflight_at(&crate::platform::paths::pinvou3_home())
}

pub(crate) fn runtime_ready() -> Result<(), String> {
    let report = crate::platform::paths::pinvou3_home()
        .join("migrations")
        .join(REPORT_FILE);
    match load_completed_report(&report) {
        Ok(Some(_)) => Ok(()),
        Ok(None) => Err("account_platform_migration_incomplete: retry migration or reinstall the current version".to_string()),
        Err(_) => Err("account_platform_migration_invalid: restore the pre-switch configuration backup".to_string()),
    }
}

fn preflight_at(home: &Path) -> Result<ClientMigrationPreflight> {
    let settings_path = home.join("settings.json");
    let mut settings = if settings_path.exists() {
        serde_json::from_slice::<Value>(
            &std::fs::read(&settings_path)
                .with_context(|| format!("read {}", settings_path.display()))?,
        )
        .with_context(|| format!("parse {}", settings_path.display()))?
    } else {
        serde_json::json!({})
    };
    let legacy_selection_present = settings
        .pointer("/advanced/active_model_id")
        .and_then(Value::as_str)
        == Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID);
    let credential_references = read_legacy_model_credential_references(&settings_path)?
        .1
        .len();
    let inventory = inventory_value(&settings, home);
    replace_legacy_model_ids(&mut settings, JsonModelIdScope::ModelIdFields, false);
    serde_json::to_vec(&settings).context("serialize staged settings migration")?;

    let session_bindings_valid = simulate_json_migration(
        &home.join("sessions").join("_session_models.json"),
        JsonModelIdScope::EveryStringValue,
    )?;
    let scheduled_profiles_valid = simulate_json_migration(
        &home.join("scheduled-runs").join("session-profiles.json"),
        JsonModelIdScope::ModelIdFields,
    )?;
    let bindings = read_legacy_binding_keys(&home.join("llmapi-hub").join("bindings.json"))?;
    Ok(ClientMigrationPreflight {
        schema_version: MIGRATION_VERSION,
        legacy_identity_files: inventory.legacy_identity_files,
        legacy_cache_files: inventory.legacy_cache_files,
        legacy_credential_references: credential_references + bindings.len() * 3,
        preserved_user_models: inventory.preserved_user_models,
        legacy_selection_present,
        settings_valid: true,
        session_bindings_valid,
        scheduled_profiles_valid,
    })
}

#[derive(Default)]
struct ClientMigrationInventory {
    legacy_identity_files: usize,
    legacy_cache_files: usize,
    preserved_user_models: usize,
}

fn inventory_at(home: &Path) -> Result<ClientMigrationInventory> {
    let settings_path = home.join("settings.json");
    let settings = if settings_path.exists() {
        serde_json::from_slice::<Value>(&std::fs::read(&settings_path)?)?
    } else {
        serde_json::json!({})
    };
    Ok(inventory_value(&settings, home))
}

fn inventory_value(settings: &Value, home: &Path) -> ClientMigrationInventory {
    let models = settings
        .pointer("/advanced/saved_models")
        .and_then(Value::as_array);
    let preserved_user_models = models
        .map(|items| {
            items
                .iter()
                .filter(|model| {
                    model.get("id").and_then(Value::as_str) != Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID)
                })
                .count()
        })
        .unwrap_or(0);
    let legacy_root = home.join("llmapi-hub");
    let legacy_identity_files = LEGACY_IDENTITY_FILES
        .iter()
        .filter(|name| legacy_root.join(name).exists())
        .count();
    let legacy_cache_files = LEGACY_CACHE_FILES
        .iter()
        .filter(|name| legacy_root.join(name).exists())
        .count();
    ClientMigrationInventory {
        legacy_identity_files,
        legacy_cache_files,
        preserved_user_models,
    }
}

fn simulate_json_migration(path: &Path, scope: JsonModelIdScope) -> Result<bool> {
    if !path.exists() {
        return Ok(true);
    }
    let mut value: Value = serde_json::from_slice(&std::fs::read(path)?)
        .with_context(|| format!("parse staged migration input {}", path.display()))?;
    replace_legacy_model_ids(&mut value, scope, false);
    serde_json::to_vec(&value).context("serialize staged migration output")?;
    Ok(true)
}

fn load_completed_report(path: &Path) -> Result<Option<ClientModelMigrationReport>> {
    if !path.exists() {
        return Ok(None);
    }
    let report: ClientModelMigrationReport = serde_json::from_slice(
        &std::fs::read(path)
            .with_context(|| format!("read migration report {}", path.display()))?,
    )
    .with_context(|| format!("parse migration report {}", path.display()))?;
    Ok(
        (report.schema_version == MIGRATION_VERSION && report.state == MigrationState::Complete)
            .then_some(report),
    )
}

fn persist_report(path: &Path, report: &ClientModelMigrationReport) -> Result<()> {
    let bytes = serde_json::to_vec_pretty(report).context("serialize client model migration")?;
    deepseek_tui::utils::write_atomic(path, &bytes)
        .with_context(|| format!("persist migration report {}", path.display()))
}

fn copy_backup_once(source: &Path, destination: &Path) -> Result<()> {
    if !source.exists() || destination.exists() {
        return Ok(());
    }
    let bytes = std::fs::read(source)
        .with_context(|| format!("read migration source {}", source.display()))?;
    deepseek_tui::utils::write_atomic(destination, &bytes)
        .with_context(|| format!("write migration backup {}", destination.display()))
}

#[derive(Clone, Copy)]
enum JsonModelIdScope {
    EveryStringValue,
    ModelIdFields,
}

fn migrate_model_ids_in_json(path: &Path, scope: JsonModelIdScope) -> Result<usize> {
    if !path.exists() {
        return Ok(0);
    }
    let bytes = std::fs::read(path).with_context(|| format!("read {}", path.display()))?;
    let mut value: Value =
        serde_json::from_slice(&bytes).with_context(|| format!("parse {}", path.display()))?;
    let changed = replace_legacy_model_ids(&mut value, scope, false);
    if changed > 0 {
        let payload = serde_json::to_vec_pretty(&value).context("serialize migrated model ids")?;
        deepseek_tui::utils::write_atomic(path, &payload)
            .with_context(|| format!("write migrated model ids {}", path.display()))?;
    }
    Ok(changed)
}

fn replace_legacy_model_ids(
    value: &mut Value,
    scope: JsonModelIdScope,
    parent_is_model_id: bool,
) -> usize {
    match value {
        Value::String(current)
            if current == LEGACY_BUILTIN_LLMAPI_MODEL_ID
                && (matches!(scope, JsonModelIdScope::EveryStringValue) || parent_is_model_id) =>
        {
            *current = BUILTIN_ACCOUNT_PLATFORM_MODEL_ID.to_string();
            1
        }
        Value::Array(items) => items
            .iter_mut()
            .map(|item| replace_legacy_model_ids(item, scope, parent_is_model_id))
            .sum(),
        Value::Object(map) => map
            .iter_mut()
            .map(|(key, item)| {
                replace_legacy_model_ids(item, scope, key == "model_id" || key == "modelId")
            })
            .sum(),
        _ => 0,
    }
}

fn read_legacy_binding_keys(path: &Path) -> Result<Vec<(String, String)>> {
    if !path.exists() {
        return Ok(Vec::new());
    }
    let value: Value = serde_json::from_slice(
        &std::fs::read(path).with_context(|| format!("read {}", path.display()))?,
    )
    .with_context(|| format!("parse {}", path.display()))?;
    let Some(bindings) = value.get("bindings").and_then(Value::as_object) else {
        return Ok(Vec::new());
    };
    Ok(bindings
        .values()
        .filter_map(|binding| {
            Some((
                binding.get("pinvou_user_id")?.as_str()?.trim().to_string(),
                binding
                    .get("device_binding_id")?
                    .as_str()?
                    .trim()
                    .to_string(),
            ))
        })
        .filter(|(user, device)| !user.is_empty() && !device.is_empty())
        .collect())
}

fn read_legacy_model_credential_references(path: &Path) -> Result<(bool, Vec<(String, String)>)> {
    if !path.exists() {
        return Ok((false, Vec::new()));
    }
    let value: Value = serde_json::from_slice(
        &std::fs::read(path).with_context(|| format!("read {}", path.display()))?,
    )
    .with_context(|| format!("parse {}", path.display()))?;
    let Some(models) = value
        .get("advanced")
        .and_then(|advanced| advanced.get("saved_models"))
        .and_then(Value::as_array)
    else {
        return Ok((false, Vec::new()));
    };
    let legacy_models: Vec<_> = models
        .iter()
        .filter(|model| {
            model.get("id").and_then(Value::as_str) == Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID)
        })
        .collect();
    let references = legacy_models
        .iter()
        .filter_map(|model| model.get("credential_ref"))
        .filter_map(|reference| {
            Some((
                reference.get("service")?.as_str()?.trim().to_string(),
                reference.get("account")?.as_str()?.trim().to_string(),
            ))
        })
        .filter(|(service, account)| !service.is_empty() && !account.is_empty())
        .collect();
    Ok((!legacy_models.is_empty(), references))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::platform::credential_store::MemoryCredentialStore;
    use crate::platform::paths::tests::{ENV_LOCK, unique_suffix};
    use crate::platform::prefs::SavedModel;

    struct EnvHomeGuard(Option<std::ffi::OsString>);

    struct TestDir(std::path::PathBuf);

    impl TestDir {
        fn new() -> Self {
            let path = std::env::temp_dir().join(format!(
                "pinvou-account-platform-migration-{}-{}",
                std::process::id(),
                unique_suffix()
            ));
            std::fs::create_dir_all(&path).expect("create migration test directory");
            Self(path)
        }

        fn path(&self) -> &Path {
            &self.0
        }
    }

    impl Drop for TestDir {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    impl EnvHomeGuard {
        fn set(path: &Path) -> Self {
            let previous = std::env::var_os("PINVOU3_HOME");
            unsafe { std::env::set_var("PINVOU3_HOME", path) };
            Self(previous)
        }
    }

    impl Drop for EnvHomeGuard {
        fn drop(&mut self) {
            match self.0.take() {
                Some(value) => unsafe { std::env::set_var("PINVOU3_HOME", value) },
                None => unsafe { std::env::remove_var("PINVOU3_HOME") },
            }
        }
    }

    #[test]
    fn model_id_replacement_is_scoped_and_idempotent() {
        let mut value = serde_json::json!({
            "sessions": {
                "a": { "model_id": "builtin_llmapi", "note": "builtin_llmapi" }
            }
        });
        assert_eq!(
            replace_legacy_model_ids(&mut value, JsonModelIdScope::ModelIdFields, false),
            1
        );
        assert_eq!(
            value["sessions"]["a"]["model_id"],
            "builtin_account_platform"
        );
        assert_eq!(value["sessions"]["a"]["note"], "builtin_llmapi");
        assert_eq!(
            replace_legacy_model_ids(&mut value, JsonModelIdScope::ModelIdFields, false),
            0
        );
    }

    #[test]
    fn fresh_install_initializes_settings_before_recording_completion() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        let settings_path = root.path().join("settings.json");
        assert!(!settings_path.exists());
        // The real owner intentionally leaves a missing settings file absent.
        let _ = UserPrefs::load();
        assert!(!settings_path.exists());
        let report = migrate_at(root.path(), &MemoryCredentialStore::default()).unwrap();
        assert_eq!(report.state, MigrationState::Complete);
        assert!(report.settings_migrated);
        assert_eq!(report.credentials_removed, 0);
        let persisted: UserPrefs =
            serde_json::from_slice(&std::fs::read(&settings_path).unwrap()).unwrap();
        assert_eq!(persisted.model_path_schema_version, MIGRATION_VERSION);
        assert!(
            persisted
                .model_by_id(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
                .is_some()
        );
        assert!(
            persisted
                .model_by_id(LEGACY_BUILTIN_LLMAPI_MODEL_ID)
                .is_none()
        );
        assert!(
            !root
                .path()
                .join("migrations/settings-pre-account-platform-v1.json")
                .exists()
        );
    }

    #[test]
    fn full_migration_switches_references_and_is_idempotent() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        let binding_dir = root.path().join("llmapi-hub");
        std::fs::create_dir_all(&binding_dir).unwrap();
        std::fs::write(
            binding_dir.join("bindings.json"),
            serde_json::to_vec(&serde_json::json!({
                "bindings": {
                    "u_1:dev_1": {
                        "pinvou_user_id": "u_1",
                        "device_binding_id": "dev_1"
                    }
                }
            }))
            .unwrap(),
        )
        .unwrap();
        std::fs::write(binding_dir.join("device.json"), r#"{"device":"legacy"}"#).unwrap();
        std::fs::write(binding_dir.join("models.json"), r#"{"items":[]}"#).unwrap();
        let mut prefs = UserPrefs::default();
        let mut legacy_model = SavedModel::builtin_account_platform();
        legacy_model.id = LEGACY_BUILTIN_LLMAPI_MODEL_ID.to_string();
        legacy_model.base_url = "https://legacy.invalid/v1".to_string();
        legacy_model.model = "legacy-wire-model".to_string();
        legacy_model.credential_ref = Some(CredentialReference {
            service: "legacy-explicit-service".into(),
            account: "legacy-explicit-account".into(),
            version: 1,
        });
        prefs.advanced.saved_models = vec![legacy_model];
        prefs.advanced.active_model_id = Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID.to_string());
        std::fs::write(
            root.path().join("settings.json"),
            serde_json::to_vec(&prefs).unwrap(),
        )
        .unwrap();
        let sessions = root.path().join("sessions");
        std::fs::create_dir_all(&sessions).unwrap();
        std::fs::write(
            sessions.join("_session_models.json"),
            r#"{"session-1":"builtin_llmapi"}"#,
        )
        .unwrap();
        let scheduled = root.path().join("scheduled-runs");
        std::fs::create_dir_all(&scheduled).unwrap();
        std::fs::write(
            scheduled.join("session-profiles.json"),
            r#"{"run-1":{"model_id":"builtin_llmapi","label":"builtin_llmapi"}}"#,
        )
        .unwrap();

        let preflight = preflight_at(root.path()).expect("read-only migration preflight");
        assert_eq!(preflight.legacy_identity_files, 2);
        assert_eq!(preflight.legacy_cache_files, 1);
        assert_eq!(preflight.legacy_credential_references, 4);
        assert!(preflight.legacy_selection_present);
        assert!(preflight.settings_valid && preflight.session_bindings_valid);

        let store = MemoryCredentialStore::default();
        for reference in [
            CredentialReference {
                service: "pinvou3-llmapi-token".into(),
                account: "llmapi:u_1:dev_1".into(),
                version: 1,
            },
            CredentialReference {
                service: "pinvou3-llmapi-user-session".into(),
                account: "llmapi-session:u_1:dev_1".into(),
                version: 1,
            },
            CredentialReference {
                service: "legacy-explicit-service".into(),
                account: "legacy-explicit-account".into(),
                version: 1,
            },
        ] {
            store.set(&reference, "secret-value").unwrap();
        }

        let first = migrate_at(root.path(), &store).expect("first migration");
        assert_eq!(first.state, MigrationState::Complete);
        assert!(first.settings_migrated);
        assert_eq!(first.session_bindings_migrated, 1);
        assert_eq!(first.scheduled_profiles_migrated, 1);
        assert_eq!(first.legacy_bindings_discovered, 1);
        assert_eq!(first.credentials_removed, 3);
        assert!(!binding_dir.join("bindings.json").exists());
        assert!(!binding_dir.join("device.json").exists());
        assert!(!binding_dir.join("models.json").exists());
        assert!(
            root.path()
                .join("migrations/legacy-llmapi-bindings-v1.json")
                .exists()
        );
        let migrated = UserPrefs::load();
        assert_eq!(
            migrated.advanced.active_model_id.as_deref(),
            Some(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
        );
        assert_eq!(
            migrated.advanced.account_platform_default_model.as_deref(),
            Some("legacy-wire-model")
        );
        assert!(
            migrated
                .model_by_id(LEGACY_BUILTIN_LLMAPI_MODEL_ID)
                .is_none()
        );
        assert_eq!(
            serde_json::from_slice::<Value>(
                &std::fs::read(sessions.join("_session_models.json")).unwrap()
            )
            .unwrap()["session-1"],
            BUILTIN_ACCOUNT_PLATFORM_MODEL_ID
        );
        let scheduled_value: Value = serde_json::from_slice(
            &std::fs::read(scheduled.join("session-profiles.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(
            scheduled_value["run-1"]["model_id"],
            BUILTIN_ACCOUNT_PLATFORM_MODEL_ID
        );
        assert_eq!(scheduled_value["run-1"]["label"], "builtin_llmapi");
        assert!(
            store
                .get(&CredentialReference {
                    service: "pinvou3-llmapi-token".into(),
                    account: "llmapi:u_1:dev_1".into(),
                    version: 1,
                })
                .unwrap()
                .is_none()
        );

        let second = migrate_at(root.path(), &store).expect("idempotent migration");
        assert_eq!(second.state, MigrationState::Complete);
        assert_eq!(second.session_bindings_migrated, 1);
        assert_eq!(second.credentials_removed, 3);
    }

    #[derive(Clone, Copy)]
    enum CleanupFault {
        InspectOnce,
        DeleteOnce,
        Unreachable,
    }

    struct FaultingCredentialStore {
        inner: MemoryCredentialStore,
        fault: CleanupFault,
        armed: std::sync::atomic::AtomicBool,
    }

    impl CredentialStore for FaultingCredentialStore {
        fn get(
            &self,
            reference: &CredentialReference,
        ) -> std::result::Result<Option<String>, crate::platform::credential_store::CredentialError>
        {
            if reference.service == "legacy-explicit-service"
                && matches!(self.fault, CleanupFault::InspectOnce)
                && self.armed.swap(false, std::sync::atomic::Ordering::SeqCst)
            {
                return Err(crate::platform::credential_store::CredentialError::new(
                    "controlled credential inspection failure",
                ));
            }
            self.inner.get(reference)
        }

        fn set(
            &self,
            reference: &CredentialReference,
            value: &str,
        ) -> std::result::Result<(), crate::platform::credential_store::CredentialError> {
            self.inner.set(reference, value)
        }

        fn delete(
            &self,
            reference: &CredentialReference,
        ) -> std::result::Result<(), crate::platform::credential_store::CredentialError> {
            if reference.service == "legacy-explicit-service"
                && matches!(self.fault, CleanupFault::DeleteOnce)
                && self.armed.swap(false, std::sync::atomic::Ordering::SeqCst)
            {
                return Err(crate::platform::credential_store::CredentialError::new(
                    "controlled credential deletion failure",
                ));
            }
            self.inner.delete(reference)
        }

        fn os_keyring_unreachable(&self, _reference: &CredentialReference) -> bool {
            matches!(self.fault, CleanupFault::Unreachable)
                && self.armed.load(std::sync::atomic::Ordering::SeqCst)
        }
    }

    fn legacy_cleanup_fixture(home: &Path, shared_user_model: bool) -> CredentialReference {
        let reference = CredentialReference {
            service: "legacy-explicit-service".into(),
            account: "legacy-explicit-account".into(),
            version: 1,
        };
        let mut legacy = SavedModel::builtin_account_platform();
        legacy.id = LEGACY_BUILTIN_LLMAPI_MODEL_ID.into();
        legacy.model = "controlled-legacy-model".into();
        legacy.credential_ref = Some(reference.clone());
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![legacy];
        prefs.advanced.active_model_id = Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID.into());
        if shared_user_model {
            let mut retained = SavedModel::builtin_account_platform();
            retained.id = "retained-user-model".into();
            retained.model = "controlled-user-model".into();
            retained.credential_ref = Some(reference.clone());
            prefs.advanced.saved_models.push(retained);
        }
        std::fs::write(
            home.join("settings.json"),
            serde_json::to_vec(&prefs).unwrap(),
        )
        .unwrap();
        reference
    }

    #[test]
    fn cleanup_retry_keeps_original_references_after_inspection_or_deletion_failure() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        for fault in [CleanupFault::InspectOnce, CleanupFault::DeleteOnce] {
            let root = TestDir::new();
            let _home = EnvHomeGuard::set(root.path());
            let reference = legacy_cleanup_fixture(root.path(), false);
            let store = FaultingCredentialStore {
                inner: MemoryCredentialStore::default(),
                fault,
                armed: std::sync::atomic::AtomicBool::new(true),
            };
            store.set(&reference, "controlled-test-value").unwrap();
            let first = migrate_at(root.path(), &store).unwrap();
            assert_eq!(first.state, MigrationState::CleanupPending);
            assert!(
                UserPrefs::load()
                    .model_by_id(LEGACY_BUILTIN_LLMAPI_MODEL_ID)
                    .is_none()
            );
            assert!(store.inner.get(&reference).unwrap().is_some());
            let backup = root
                .path()
                .join("migrations/settings-pre-account-platform-v1.json");
            let original_backup = std::fs::read(&backup).unwrap();
            let second = migrate_at(root.path(), &store).unwrap();
            assert_eq!(second.state, MigrationState::Complete);
            assert_eq!(second.credentials_removed, 1);
            assert!(store.inner.get(&reference).unwrap().is_none());
            assert_eq!(std::fs::read(&backup).unwrap(), original_backup);
            assert_eq!(
                migrate_at(root.path(), &store).unwrap().credentials_removed,
                1
            );
        }
    }

    #[test]
    fn unavailable_keyring_miss_stays_pending_until_the_store_is_reachable() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        legacy_cleanup_fixture(root.path(), false);
        let store = FaultingCredentialStore {
            inner: MemoryCredentialStore::default(),
            fault: CleanupFault::Unreachable,
            armed: std::sync::atomic::AtomicBool::new(true),
        };
        let first = migrate_at(root.path(), &store).unwrap();
        assert_eq!(first.state, MigrationState::CleanupPending);
        assert_eq!(first.credentials_removed, 0);
        assert!(!first.cleanup_errors.is_empty());
        assert!(
            load_completed_report(&root.path().join("migrations").join(REPORT_FILE))
                .unwrap()
                .is_none()
        );
        store
            .armed
            .store(false, std::sync::atomic::Ordering::SeqCst);
        assert_eq!(
            migrate_at(root.path(), &store).unwrap().state,
            MigrationState::Complete
        );
    }

    #[test]
    fn cleanup_preserves_credentials_still_owned_by_a_retained_user_model() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        let reference = legacy_cleanup_fixture(root.path(), true);
        let store = MemoryCredentialStore::default();
        store.set(&reference, "controlled-shared-value").unwrap();
        let report = migrate_at(root.path(), &store).unwrap();
        assert_eq!(report.state, MigrationState::Complete);
        assert_eq!(report.credentials_removed, 0);
        assert!(store.get(&reference).unwrap().is_some());
        let prefs = UserPrefs::load();
        assert_eq!(
            prefs
                .model_by_id("retained-user-model")
                .unwrap()
                .credential_ref
                .as_ref(),
            Some(&reference)
        );
    }

    #[test]
    fn corrupt_original_backup_stops_cleanup_without_removing_the_pending_credential() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        let reference = legacy_cleanup_fixture(root.path(), false);
        let store = FaultingCredentialStore {
            inner: MemoryCredentialStore::default(),
            fault: CleanupFault::DeleteOnce,
            armed: std::sync::atomic::AtomicBool::new(true),
        };
        store.set(&reference, "controlled-test-value").unwrap();
        assert_eq!(
            migrate_at(root.path(), &store).unwrap().state,
            MigrationState::CleanupPending
        );
        std::fs::write(
            root.path()
                .join("migrations/settings-pre-account-platform-v1.json"),
            b"{",
        )
        .unwrap();
        assert!(migrate_at(root.path(), &store).is_err());
        assert!(store.inner.get(&reference).unwrap().is_some());
    }
}
