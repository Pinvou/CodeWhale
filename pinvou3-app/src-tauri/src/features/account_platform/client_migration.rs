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
    ensure_initial_settings(&settings_path)?;
    let settings_backup = migration_dir.join("settings-pre-account-platform-v1.json");
    copy_backup_once(&settings_path, &settings_backup)?;
    let (legacy_model_present, legacy_model_references) =
        read_legacy_model_credential_references(&settings_backup)?;

    // The immutable backup retains explicit legacy credential references even
    // after the first attempt has converted settings and cleanup needs a retry.
    if settings_path.exists() {
        let _: UserPrefs = serde_json::from_slice(&std::fs::read(&settings_path)?)
            .context("invalid source settings; migration left the file unchanged")?;
    }
    // UserPrefs owns the settings schema migration and persists it under the
    // same process-wide lock used by all settings commands.
    let prefs = UserPrefs::load();
    // A failed normalization save must not become a durable Complete report.
    let persisted: UserPrefs = serde_json::from_slice(
        &std::fs::read(&settings_path).context("read persisted settings migration")?,
    )
    .context("parse persisted settings migration")?;
    let settings_migrated = persisted.model_path_schema_version == MIGRATION_VERSION
        && persisted
            .model_by_id(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
            .is_some()
        && !persisted
            .advanced
            .saved_models
            .iter()
            .any(|model| model.id == LEGACY_BUILTIN_LLMAPI_MODEL_ID)
        && prefs.model_path_schema_version == MIGRATION_VERSION
        && prefs
            .model_by_id(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
            .is_some()
        && !prefs
            .advanced
            .saved_models
            .iter()
            .any(|model| model.id == LEGACY_BUILTIN_LLMAPI_MODEL_ID);
    anyhow::ensure!(
        settings_migrated,
        "account platform settings migration did not converge"
    );

    let mut report = ClientModelMigrationReport {
        settings_migrated,
        ..ClientModelMigrationReport::default()
    };
    copy_backup_once(
        &home.join("sessions/_session_models.json"),
        &migration_dir.join("session-models-pre-account-platform-v1.json"),
    )?;
    copy_backup_once(
        &home.join("scheduled-runs/session-profiles.json"),
        &migration_dir.join("scheduled-profiles-pre-account-platform-v1.json"),
    )?;
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
    for (service, account) in references {
        let reference = CredentialReference {
            service,
            account,
            version: 1,
        };
        // A custom model may still use the same stored key. Retiring the built-in
        // route never owns credentials referenced by a surviving user model.
        if prefs.advanced.saved_models.iter().any(|model| {
            model.credential_ref.as_ref().is_some_and(|active| {
                active.service == reference.service && active.account == reference.account
            })
        }) {
            continue;
        }
        match credentials.get(&reference) {
            Ok(_) if credentials.os_keyring_unreachable(&reference) => report
                .cleanup_errors
                .push("credential_inspection_unavailable".to_string()),
            Ok(None) => {}
            Ok(Some(_)) => match credentials.delete(&reference) {
                Ok(()) => report.credentials_removed += 1,
                Err(_) => report
                    .cleanup_errors
                    .push("credential_cleanup_failed".to_string()),
            },
            Err(_) => report
                .cleanup_errors
                .push("credential_inspection_failed".to_string()),
        }
    }

    if bindings_path.exists() {
        let backup = migration_dir.join("legacy-llmapi-bindings-v1.json");
        copy_backup_once(&bindings_path, &backup)?;
        if report.cleanup_errors.is_empty() {
            std::fs::remove_file(&bindings_path)
                .with_context(|| format!("remove inactive bindings {}", bindings_path.display()))?;
            if let Some(parent) = bindings_path.parent() {
                let _ = std::fs::remove_dir(parent);
            }
        }
    }

    if !report.cleanup_errors.is_empty() {
        report.state = MigrationState::CleanupPending;
    }
    persist_report(&report_path, &report)?;
    Ok(report)
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

fn ensure_initial_settings(path: &Path) -> Result<()> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata) => {
            anyhow::ensure!(metadata.is_file(), "source settings is not a regular file");
            return Ok(());
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => return Err(error).context("inspect initial settings"),
    }
    // Missing settings are deliberately not persisted by the generic loader.
    // Create them through its normal transaction, rechecking inside that lock:
    // another settings command may already have saved a user's configuration.
    UserPrefs::update_transaction(|prefs| {
        match std::fs::symlink_metadata(path) {
            Ok(metadata) if metadata.is_file() => {
                let bytes = std::fs::read(path)
                    .map_err(|_| "read concurrent settings failed".to_string())?;
                *prefs = serde_json::from_slice(&bytes)
                    .map_err(|_| "invalid concurrent settings".to_string())?;
            }
            Ok(_) => return Err("source settings is not a regular file".to_string()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(_) => return Err("inspect concurrent settings failed".to_string()),
        }
        Ok(())
    })
    .map_err(anyhow::Error::msg)?;
    Ok(())
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
    let bindings = value
        .get("bindings")
        .and_then(Value::as_object)
        .context("invalid legacy binding collection; retain it for retry")?;
    bindings
        .values()
        .map(|binding| {
            let user = binding
                .get("pinvou_user_id")
                .and_then(Value::as_str)
                .filter(|value| !value.trim().is_empty())
                .context("invalid legacy binding user; retain it for retry")?;
            let device = binding
                .get("device_binding_id")
                .and_then(Value::as_str)
                .filter(|value| !value.trim().is_empty())
                .context("invalid legacy binding device; retain it for retry")?;
            Ok((user.trim().to_string(), device.trim().to_string()))
        })
        .collect()
}

fn read_legacy_model_credential_references(path: &Path) -> Result<(bool, Vec<(String, String)>)> {
    if !path.exists() {
        return Ok((false, Vec::new()));
    }
    let value: Value = serde_json::from_slice(
        &std::fs::read(path).with_context(|| format!("read {}", path.display()))?,
    )
    .with_context(|| format!("parse {}", path.display()))?;
    let models = value
        .get("advanced")
        .and_then(|advanced| advanced.get("saved_models"))
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or(&[]);
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
    let advanced = value.get("advanced");
    let legacy_reference = advanced.is_some_and(|advanced| {
        advanced.get("active_model_id").and_then(Value::as_str)
            == Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID)
            || advanced.get("builtin_llmapi_default_model").is_some()
            || models.iter().any(|model| {
                model.get("vision_model_id").and_then(Value::as_str)
                    == Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID)
            })
    });
    Ok((!legacy_models.is_empty() || legacy_reference, references))
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
    fn fresh_home_initializes_settings_once_without_changing_generic_load() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        let settings = root.path().join("settings.json");
        let _ = UserPrefs::load();
        assert!(!settings.exists());
        let store = MemoryCredentialStore::default();
        let first = migrate_at(root.path(), &store).unwrap();
        assert_eq!(first.state, MigrationState::Complete);
        let bytes = std::fs::read(&settings).unwrap();
        let prefs: UserPrefs = serde_json::from_slice(&bytes).unwrap();
        assert!(
            prefs
                .model_by_id(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
                .is_some()
        );
        assert_eq!(
            migrate_at(root.path(), &store).unwrap().state,
            MigrationState::Complete
        );
        assert_eq!(std::fs::read(&settings).unwrap(), bytes);
    }

    #[test]
    fn initial_settings_preserve_concurrent_preferences_and_reject_nonfiles() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        for _ in 0..16 {
            let root = TestDir::new();
            let _home = EnvHomeGuard::set(root.path());
            let barrier = std::sync::Arc::new(std::sync::Barrier::new(2));
            let start = barrier.clone();
            let path = root.path().join("settings.json");
            let initializer = std::thread::spawn(move || {
                start.wait();
                ensure_initial_settings(&path)
            });
            barrier.wait();
            UserPrefs::update_transaction(|prefs| {
                prefs.advanced.account_platform_default_model = Some("user-selected-wire".into());
                Ok(())
            })
            .unwrap();
            initializer.join().unwrap().unwrap();
            assert_eq!(
                UserPrefs::load()
                    .advanced
                    .account_platform_default_model
                    .as_deref(),
                Some("user-selected-wire")
            );
        }
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        let settings = root.path().join("settings.json");
        std::fs::create_dir(&settings).unwrap();
        assert!(ensure_initial_settings(&settings).is_err());
        assert!(settings.is_dir());
        assert_eq!(std::fs::read_dir(settings).unwrap().count(), 0);
    }

    #[test]
    fn orphan_legacy_references_still_check_only_retired_fixed_credential_keys() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        for hint in ["active", "vision", "default"] {
            let root = TestDir::new();
            let _home = EnvHomeGuard::set(root.path());
            let mut prefs = UserPrefs::default();
            let mut ordinary = SavedModel::builtin_account_platform();
            ordinary.id = "ordinary-model".into();
            if hint == "vision" {
                ordinary.vision_model_id = Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID.into());
            }
            prefs.advanced.saved_models = vec![ordinary];
            if hint == "active" {
                prefs.advanced.active_model_id = Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID.into());
            }
            let mut value = serde_json::to_value(&prefs).unwrap();
            if hint == "active" {
                value["advanced"]
                    .as_object_mut()
                    .unwrap()
                    .remove("saved_models");
            }
            if hint == "default" {
                value["advanced"]["builtin_llmapi_default_model"] =
                    serde_json::json!("preserved-wire");
            }
            std::fs::write(
                root.path().join("settings.json"),
                serde_json::to_vec(&value).unwrap(),
            )
            .unwrap();
            let store = MemoryCredentialStore::default();
            let retired = [
                CredentialReference {
                    service: "pinvou3-llmapi-admin".into(),
                    account: "newapi-admin".into(),
                    version: 1,
                },
                CredentialReference {
                    service: "pinvou3-model-api-key".into(),
                    account: format!("model:{LEGACY_BUILTIN_LLMAPI_MODEL_ID}"),
                    version: 1,
                },
            ];
            for reference in &retired {
                store.set(reference, "public-retired-fixture").unwrap();
            }
            let unrelated = CredentialReference::for_model("unrelated-user-model");
            store.set(&unrelated, "public-unrelated-fixture").unwrap();
            let report = migrate_at(root.path(), &store).unwrap();
            assert_eq!(report.state, MigrationState::Complete);
            assert_eq!(report.credentials_removed, 2);
            assert!(
                retired
                    .iter()
                    .all(|reference| store.get(reference).unwrap().is_none())
            );
            assert!(store.get(&unrelated).unwrap().is_some());
        }
    }

    #[test]
    fn failed_session_mapping_write_keeps_history_resolvable_and_retryable() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        seed_legacy_settings(root.path(), false);
        let path = root.path().join("sessions/_session_models.json");
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        let original = serde_json::to_vec(
            &serde_json::json!({"existing-session":LEGACY_BUILTIN_LLMAPI_MODEL_ID}),
        )
        .unwrap();
        std::fs::write(&path, &original).unwrap();
        let Some(lock) = crate::platform::test_support::deny_atomic_replacement_for_test(&path)
        else {
            return;
        };
        assert!(migrate_at(root.path(), &MemoryCredentialStore::default()).is_err());
        assert_eq!(std::fs::read(&path).unwrap(), original);
        assert!(!root.path().join("migrations").join(REPORT_FILE).exists());
        let store = crate::features::sessions::SessionStore::boot().unwrap();
        let id = store.session_model_override("existing-session").unwrap();
        assert_eq!(id, LEGACY_BUILTIN_LLMAPI_MODEL_ID);
        let prefs = UserPrefs::load();
        assert_eq!(
            prefs.model_by_id(&id).unwrap().id,
            BUILTIN_ACCOUNT_PLATFORM_MODEL_ID
        );
        drop(lock);
        assert_eq!(
            migrate_at(root.path(), &MemoryCredentialStore::default())
                .unwrap()
                .state,
            MigrationState::Complete
        );
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
        assert_eq!(
            std::fs::read(
                root.path()
                    .join("migrations/session-models-pre-account-platform-v1.json")
            )
            .unwrap(),
            br#"{"session-1":"builtin_llmapi"}"#
        );
        assert_eq!(
            std::fs::read(
                root.path()
                    .join("migrations/scheduled-profiles-pre-account-platform-v1.json")
            )
            .unwrap(),
            br#"{"run-1":{"model_id":"builtin_llmapi","label":"builtin_llmapi"}}"#
        );
        assert!(!binding_dir.join("bindings.json").exists());
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
            !migrated
                .advanced
                .saved_models
                .iter()
                .any(|model| model.id == LEGACY_BUILTIN_LLMAPI_MODEL_ID)
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
    struct CleanupStore {
        inner: MemoryCredentialStore,
        fail_delete: std::cell::Cell<bool>,
        unreachable: bool,
    }
    impl CredentialStore for CleanupStore {
        fn get(
            &self,
            reference: &CredentialReference,
        ) -> Result<Option<String>, crate::platform::credential_store::CredentialError> {
            self.inner.get(reference)
        }
        fn set(
            &self,
            reference: &CredentialReference,
            value: &str,
        ) -> Result<(), crate::platform::credential_store::CredentialError> {
            self.inner.set(reference, value)
        }
        fn delete(
            &self,
            reference: &CredentialReference,
        ) -> Result<(), crate::platform::credential_store::CredentialError> {
            if self.fail_delete.get() {
                Err(crate::platform::credential_store::CredentialError::new(
                    "isolated failure",
                ))
            } else {
                self.inner.delete(reference)
            }
        }
        fn os_keyring_unreachable(&self, _: &CredentialReference) -> bool {
            self.unreachable
        }
    }

    fn seed_legacy_settings(root: &Path, shared: bool) -> CredentialReference {
        let reference = CredentialReference {
            service: "owned-migration-fixture".into(),
            account: "legacy-key".into(),
            version: 1,
        };
        let mut legacy = SavedModel::builtin_account_platform();
        legacy.id = LEGACY_BUILTIN_LLMAPI_MODEL_ID.into();
        legacy.credential_ref = Some(reference.clone());
        legacy.alias = Some("preserved label".into());
        legacy.context_window_tokens = Some(64_000);
        legacy.reasoning_effort = Some("high".into());
        let mut ordinary = legacy.clone();
        ordinary.id = "ordinary-model".into();
        ordinary.vision_model_id = Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID.into());
        if !shared {
            ordinary.credential_ref = None;
        } else {
            // Store identity is service/account, independent of schema version.
            ordinary.credential_ref.as_mut().unwrap().version = 2;
        }
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![legacy, ordinary];
        prefs.advanced.active_model_id = Some(LEGACY_BUILTIN_LLMAPI_MODEL_ID.into());
        std::fs::write(
            root.join("settings.json"),
            serde_json::to_vec(&prefs).unwrap(),
        )
        .unwrap();
        reference
    }

    #[test]
    fn explicit_credential_cleanup_retries_from_backup_after_settings_cutover() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        let reference = seed_legacy_settings(root.path(), false);
        let original = std::fs::read(root.path().join("settings.json")).unwrap();
        let store = CleanupStore {
            inner: MemoryCredentialStore::default(),
            fail_delete: std::cell::Cell::new(true),
            unreachable: false,
        };
        store.set(&reference, "isolated-credential-value").unwrap();
        let first = migrate_at(root.path(), &store).unwrap();
        assert_eq!(first.state, MigrationState::CleanupPending);
        assert!(store.get(&reference).unwrap().is_some());
        let prefs = UserPrefs::load();
        let model = prefs
            .model_by_id(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
            .unwrap();
        assert_eq!(model.alias.as_deref(), Some("preserved label"));
        assert_eq!(model.context_window_tokens, Some(64_000));
        assert_eq!(model.reasoning_effort.as_deref(), Some("high"));
        assert_eq!(
            prefs
                .model_by_id("ordinary-model")
                .unwrap()
                .vision_model_id
                .as_deref(),
            Some(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
        );
        assert_eq!(
            std::fs::read(
                root.path()
                    .join("migrations/settings-pre-account-platform-v1.json")
            )
            .unwrap(),
            original
        );
        store.fail_delete.set(false);
        let retry = migrate_at(root.path(), &store).unwrap();
        assert_eq!(retry.state, MigrationState::Complete);
        assert!(store.get(&reference).unwrap().is_none());
        assert_eq!(retry.credentials_removed, 1);
    }

    #[test]
    fn migration_keeps_a_credential_still_referenced_by_an_ordinary_model() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        let reference = seed_legacy_settings(root.path(), true);
        let store = MemoryCredentialStore::default();
        store.set(&reference, "isolated-shared-value").unwrap();
        assert_eq!(
            migrate_at(root.path(), &store).unwrap().state,
            MigrationState::Complete
        );
        assert!(store.get(&reference).unwrap().is_some());
        let mut preserved = reference.clone();
        preserved.version = 2;
        assert_eq!(
            UserPrefs::load()
                .model_by_id("ordinary-model")
                .unwrap()
                .credential_ref
                .as_ref(),
            Some(&preserved)
        );
    }

    #[test]
    fn unreachable_keyring_miss_does_not_finalize_cleanup() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        let root = TestDir::new();
        let _home = EnvHomeGuard::set(root.path());
        let reference = seed_legacy_settings(root.path(), false);
        let store = CleanupStore {
            inner: MemoryCredentialStore::default(),
            fail_delete: std::cell::Cell::new(false),
            unreachable: true,
        };
        let report = migrate_at(root.path(), &store).unwrap();
        assert_eq!(report.state, MigrationState::CleanupPending);
        store.set(&reference, "public-fixture-value").unwrap();
        let report = migrate_at(root.path(), &store).unwrap();
        assert!(store.inner.get(&reference).unwrap().is_some());
        assert_eq!(report.state, MigrationState::CleanupPending);
        assert!(
            report
                .cleanup_errors
                .iter()
                .all(|error| error == "credential_inspection_unavailable")
        );
    }

    #[test]
    fn invalid_source_settings_and_binding_shapes_are_retained_without_complete_report() {
        let _env_lock = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        for source in [r#"{"advanced":{"saved_models":"invalid"}}"#, "invalid-json"] {
            let root = TestDir::new();
            let _home = EnvHomeGuard::set(root.path());
            std::fs::write(root.path().join("settings.json"), source).unwrap();
            assert!(migrate_at(root.path(), &MemoryCredentialStore::default()).is_err());
            assert_eq!(
                std::fs::read(root.path().join("settings.json")).unwrap(),
                source.as_bytes()
            );
            assert!(!root.path().join("migrations").join(REPORT_FILE).exists());
        }
        for value in [
            serde_json::json!({"bindings":null}),
            serde_json::json!({"bindings":{"one":{"pinvou_user_id":"user"}}}),
        ] {
            let root = TestDir::new();
            let _home = EnvHomeGuard::set(root.path());
            seed_legacy_settings(root.path(), false);
            let path = root.path().join("llmapi-hub/bindings.json");
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            let original = serde_json::to_vec(&value).unwrap();
            std::fs::write(&path, &original).unwrap();
            assert!(migrate_at(root.path(), &MemoryCredentialStore::default()).is_err());
            assert_eq!(std::fs::read(&path).unwrap(), original);
            assert!(!root.path().join("migrations").join(REPORT_FILE).exists());
        }
    }
}
