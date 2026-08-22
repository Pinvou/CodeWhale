use std::collections::BTreeSet;
use std::path::Path;

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::platform::credential_store::{
    CredentialReference, CredentialStore, SystemCredentialStore,
};
use crate::platform::prefs::{
    UserPrefs, BUILTIN_ACCOUNT_PLATFORM_MODEL_ID, LEGACY_BUILTIN_LLMAPI_MODEL_ID,
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
    let settings_backup = migration_dir.join("settings-pre-account-platform-v1.json");
    copy_backup_once(&settings_path, &settings_backup)?;
    let (legacy_model_present, legacy_model_references) =
        read_legacy_model_credential_references(&settings_path)?;

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

    let mut report = ClientModelMigrationReport {
        settings_migrated,
        ..ClientModelMigrationReport::default()
    };
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
        match credentials.get(&reference) {
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
    use crate::platform::paths::tests::{unique_suffix, ENV_LOCK};
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
        assert!(!binding_dir.join("bindings.json").exists());
        assert!(root
            .path()
            .join("migrations/legacy-llmapi-bindings-v1.json")
            .exists());
        let migrated = UserPrefs::load();
        assert_eq!(
            migrated.advanced.active_model_id.as_deref(),
            Some(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
        );
        assert_eq!(
            migrated.advanced.account_platform_default_model.as_deref(),
            Some("legacy-wire-model")
        );
        assert!(migrated
            .model_by_id(LEGACY_BUILTIN_LLMAPI_MODEL_ID)
            .is_none());
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
        assert!(store
            .get(&CredentialReference {
                service: "pinvou3-llmapi-token".into(),
                account: "llmapi:u_1:dev_1".into(),
                version: 1,
            })
            .unwrap()
            .is_none());

        let second = migrate_at(root.path(), &store).expect("idempotent migration");
        assert_eq!(second.state, MigrationState::Complete);
        assert_eq!(second.session_bindings_migrated, 1);
        assert_eq!(second.credentials_removed, 3);
    }
}
