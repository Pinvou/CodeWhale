//! Real settings reads for cleanup admission; ordinary model selectors retain defaults.

use super::*;
use crate::platform::test_support::with_temp_home;

fn list(require_readable: Option<bool>) -> Result<ModelsView, String> {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(list_models(require_readable))
}

fn home_entries() -> Vec<std::ffi::OsString> {
    let mut entries = std::fs::read_dir(crate::platform::paths::settings_path().parent().unwrap())
        .unwrap()
        .map(|entry| entry.unwrap().file_name())
        .collect::<Vec<_>>();
    entries.sort();
    entries
}

#[test]
fn readable_model_list_rejects_corruption_without_replacing_settings() {
    with_temp_home("readable-model-list-invalid", || {
        let path = crate::platform::paths::settings_path();
        let original = b"{\"advanced\": invalid user data";
        std::fs::write(&path, original).unwrap();
        let entries = home_entries();
        assert_eq!(list(Some(true)).unwrap_err(), "model_settings_invalid");
        assert_eq!(std::fs::read(&path).unwrap(), original);
        assert_eq!(home_entries(), entries);

        // This opt-in must not change the legacy selector/bridge contract.
        assert!(!list(None).unwrap().models.is_empty());
        assert!(!list(Some(false)).unwrap().models.is_empty());
    });
}

#[test]
fn readable_model_list_rejects_directory_without_touching_its_contents() {
    with_temp_home("readable-model-list-directory", || {
        let path = crate::platform::paths::settings_path();
        std::fs::create_dir(&path).unwrap();
        let marker = path.join("keep-user-data");
        std::fs::write(&marker, b"keep").unwrap();
        let entries = home_entries();
        assert_eq!(list(Some(true)).unwrap_err(), "model_settings_unavailable");
        assert!(path.is_dir());
        assert_eq!(std::fs::read(marker).unwrap(), b"keep");
        assert_eq!(home_entries(), entries);
    });
}

#[test]
fn readable_model_list_rejects_permission_error_without_replacing_settings() {
    with_temp_home("readable-model-list-permission", || {
        let path = crate::platform::paths::settings_path();
        let original = b"{}";
        std::fs::write(&path, original).unwrap();
        let result =
            crate::platform::test_support::with_read_permission_denied(&path, || list(Some(true)))
                .unwrap();
        if let Some(result) = result {
            assert_eq!(result.unwrap_err(), "model_settings_unavailable");
        }
        assert_eq!(std::fs::read(&path).unwrap(), original);
    });
}

#[test]
fn readable_model_list_preserves_linked_default_without_credential_refresh_or_writes() {
    with_temp_home("readable-model-list-linked", || {
        let path = crate::platform::paths::settings_path();
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![serde_json::from_value(serde_json::json!({
            "id": "linked-default", "name": "Linked default", "preset": "openai_compatible",
            "model": "local-wire", "base_url": "http://127.0.0.1:8911/v1",
            "provider_kind": "custom", "model_store_card_id": "cleanup-card",
            "credential_ref": { "service": "pinvou3-model-api-key", "account": "model:linked-default", "version": 1 },
            "credential_state": "configured", "has_secret": true
        })).unwrap()];
        prefs.advanced.active_model_id = Some("linked-default".to_string());
        let original = serde_json::to_vec(&prefs).unwrap();
        std::fs::write(&path, &original).unwrap();
        let entries = home_entries();
        let snapshot = list(Some(true)).unwrap();
        let model = &snapshot
            .models
            .iter()
            .find(|item| item.model.id == "linked-default")
            .unwrap()
            .model;
        assert_eq!(snapshot.active_model_id.as_deref(), Some("linked-default"));
        assert_eq!(
            snapshot.default_model_ref,
            Some(crate::platform::prefs::ModelSelectionRef::local(
                "linked-default"
            ))
        );
        assert_eq!(model.model_store_card_id.as_deref(), Some("cleanup-card"));
        assert!(!model.model_store_link_detached);
        assert_eq!(
            model.credential_state,
            crate::platform::credential_store::CredentialState::Configured
        );
        assert!(model.has_secret);
        assert_eq!(std::fs::read(&path).unwrap(), original);
        assert_eq!(
            home_entries(),
            entries,
            "strict reads must not create keyring artifacts"
        );
        assert!(
            serde_json::to_value(&snapshot).unwrap()["models"][0]
                .get("api_key")
                .is_none()
        );
    });
}

#[test]
fn readable_model_list_rejects_legacy_credentials_before_migration() {
    with_temp_home("readable-model-list-legacy-secret", || {
        let path = crate::platform::paths::settings_path();
        let mut prefs = serde_json::to_value(UserPrefs::default()).unwrap();
        prefs["advanced"]["custom_api_key"] = serde_json::json!("example-not-a-real-key");
        let original = serde_json::to_vec(&prefs).unwrap();
        std::fs::write(&path, &original).unwrap();
        let entries = home_entries();
        assert_eq!(
            list(Some(true)).unwrap_err(),
            "model_credential_migration_required"
        );
        assert_eq!(std::fs::read(&path).unwrap(), original);
        assert_eq!(
            home_entries(),
            entries,
            "strict reads must not migrate credentials"
        );
    });
}

#[test]
fn readable_model_list_requires_existing_settings_but_other_reads_keep_first_run_defaults() {
    with_temp_home("readable-model-list-first-run", || {
        let path = crate::platform::paths::settings_path();
        assert_eq!(list(Some(true)).unwrap_err(), "model_settings_unavailable");
        assert!(!path.exists());
        assert!(home_entries().is_empty());
        // Remote-task projections and ordinary selectors retain their original
        // first-run behavior; only the strict cleanup read requires a file.
        let projection = UserPrefs::load_read_only_model_snapshot().unwrap();
        assert!(!projection.advanced.saved_models.is_empty());
        assert!(!path.exists());
        assert!(!list(None).unwrap().models.is_empty());
        assert!(!list(Some(false)).unwrap().models.is_empty());
    });
}

fn write_cleanup_candidate() -> Vec<u8> {
    let mut prefs = UserPrefs::default();
    prefs.memory_enabled = false;
    prefs.advanced.custom_model_name = Some("keep-unrelated-preference".to_string());
    prefs.advanced.saved_models = vec![
        serde_json::from_value(serde_json::json!({
            "id": "cleanup-default", "name": "Cleanup default", "preset": "openai_compatible",
            "model": "local-wire", "base_url": "http://127.0.0.1:8911/v1",
            "provider_kind": "custom", "model_store_card_id": "cleanup-card",
            "credential_ref": { "service": "pinvou3-model-api-key", "account": "model:cleanup-default", "version": 1 },
            "credential_state": "configured", "has_secret": true
        })).unwrap(),
        serde_json::from_value(serde_json::json!({
            "id": "user-fallback", "name": "User fallback", "preset": "openai_compatible",
            "model": "other-wire", "base_url": "https://example.test/v1", "provider_kind": "custom"
        })).unwrap(),
    ];
    prefs
        .set_default_model_ref(crate::platform::prefs::ModelSelectionRef::local(
            "cleanup-default",
        ))
        .unwrap();
    prefs.record_explicit_model_context_selection("cleanup-default");
    prefs.record_explicit_model_context_selection("user-fallback");
    let original = serde_json::to_vec(&prefs).unwrap();
    std::fs::write(crate::platform::paths::settings_path(), &original).unwrap();
    let snapshot = list(Some(true)).unwrap();
    assert!(
        snapshot
            .models
            .iter()
            .any(|item| item.model.id == "cleanup-default"
                && item.model.model_store_card_id.as_deref() == Some("cleanup-card"))
    );
    original
}

#[test]
fn readable_store_cleanup_preserves_corruption_after_candidate_read() {
    with_temp_home("readable-cleanup-invalid", || {
        let mut original = write_cleanup_candidate();
        original.extend_from_slice(b" invalid concurrent edit");
        let path = crate::platform::paths::settings_path();
        std::fs::write(&path, &original).unwrap();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        let result = delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store);
        assert!(
            std::fs::read(&path).unwrap() == original,
            "cleanup must not replace corrupt user settings with defaults"
        );
        assert!(result.unwrap_err().contains("model_settings_invalid"));
        assert!(
            store.ops().is_empty(),
            "failed reads must preserve credentials"
        );
    });
}

#[test]
fn readable_store_cleanup_preserves_directory_after_candidate_read() {
    with_temp_home("readable-cleanup-directory", || {
        let original = write_cleanup_candidate();
        let path = crate::platform::paths::settings_path();
        std::fs::remove_file(&path).unwrap();
        std::fs::create_dir(&path).unwrap();
        let marker = path.join("keep-user-data");
        std::fs::write(&marker, &original).unwrap();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        let result = delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store);
        assert!(result.unwrap_err().contains("model_settings_unavailable"));
        assert!(path.is_dir());
        assert!(std::fs::read(marker).unwrap() == original);
        assert!(store.ops().is_empty());
    });
}

#[test]
fn readable_store_cleanup_does_not_recreate_missing_settings_after_candidate_read() {
    with_temp_home("readable-cleanup-missing", || {
        write_cleanup_candidate();
        let path = crate::platform::paths::settings_path();
        std::fs::remove_file(&path).unwrap();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        let result = delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store);
        assert!(result.unwrap_err().contains("model_settings_unavailable"));
        assert!(
            !path.exists(),
            "cleanup must not recreate settings the user removed"
        );
        assert!(store.ops().is_empty());
    });
}

#[test]
fn readable_store_cleanup_preserves_denied_settings_after_candidate_read() {
    with_temp_home("readable-cleanup-denied", || {
        let original = write_cleanup_candidate();
        let path = crate::platform::paths::settings_path();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        let result = crate::platform::test_support::with_read_permission_denied(&path, || {
            delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store)
        })
        .unwrap();
        assert!(
            std::fs::read(&path).unwrap() == original,
            "cleanup must not overwrite settings it could not read"
        );
        if let Some(result) = result {
            assert!(result.unwrap_err().contains("model_settings_unavailable"));
        }
        assert!(store.ops().is_empty());
    });
}

#[test]
fn readable_store_cleanup_preserves_pending_credentials_after_candidate_read() {
    with_temp_home("readable-cleanup-pending-credential", || {
        let mut prefs: serde_json::Value =
            serde_json::from_slice(&write_cleanup_candidate()).unwrap();
        prefs["advanced"]["custom_api_key"] = serde_json::json!("example-not-a-real-key");
        let original = serde_json::to_vec(&prefs).unwrap();
        let path = crate::platform::paths::settings_path();
        std::fs::write(&path, &original).unwrap();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        let result = delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store);
        assert!(
            std::fs::read(&path).unwrap() == original,
            "cleanup must not migrate credentials or rewrite other preferences"
        );
        assert!(
            result
                .unwrap_err()
                .contains("model_credential_migration_required")
        );
        assert!(store.ops().is_empty());
    });
}

#[test]
fn readable_store_cleanup_clears_typed_default_and_preserves_unrelated_preferences() {
    with_temp_home("readable-cleanup-default", || {
        write_cleanup_candidate();
        let before = UserPrefs::load_read_only_model_snapshot().unwrap();
        assert_eq!(
            before.advanced.default_model_ref,
            Some(crate::platform::prefs::ModelSelectionRef::local(
                "cleanup-default"
            ))
        );
        assert!(!before.advanced.default_model_selection_revision.is_empty());
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        assert!(
            delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store).unwrap()
        );
        let after = UserPrefs::load_read_only_model_snapshot().unwrap();
        assert_strict_selection_round_trip(&after);
        assert!(after.model_by_id("cleanup-default").is_none());
        assert!(after.model_by_id("user-fallback").is_some());
        assert!(after.advanced.default_model_ref.is_none());
        assert!(after.advanced.active_model_id.is_none());
        assert_eq!(
            after.advanced.default_model_selection_revision,
            before.advanced.default_model_selection_revision
        );
        assert_eq!(
            after.advanced.account_platform_default_model,
            before.advanced.account_platform_default_model
        );
        assert!(
            !after
                .advanced
                .model_store_context_repairs
                .contains_key("cleanup-default")
        );
        assert!(after.has_explicit_model_context_selection("user-fallback"));
        assert!(!after.memory_enabled);
        assert_eq!(
            after.advanced.custom_model_name.as_deref(),
            Some("keep-unrelated-preference")
        );
        assert_eq!(
            store.ops(),
            vec!["delete:pinvou3-model-api-key:model:cleanup-default".to_string()]
        );
    });
}

#[test]
fn readable_store_cleanup_rechecks_concurrent_card_reassignment() {
    with_temp_home("readable-cleanup-reassigned", || {
        let mut prefs: serde_json::Value =
            serde_json::from_slice(&write_cleanup_candidate()).unwrap();
        prefs["advanced"]["saved_models"][0]["model_store_card_id"] =
            serde_json::json!("another-card");
        let path = crate::platform::paths::settings_path();
        std::fs::write(&path, serde_json::to_vec(&prefs).unwrap()).unwrap();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        assert!(
            !delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store).unwrap()
        );
        let after = UserPrefs::load_read_only_model_snapshot().unwrap();
        assert_eq!(
            after
                .model_by_id("cleanup-default")
                .unwrap()
                .model_store_card_id
                .as_deref(),
            Some("another-card")
        );
        assert_eq!(
            after.advanced.active_model_id.as_deref(),
            Some("cleanup-default")
        );
        assert!(!after.memory_enabled);
        assert_eq!(
            after.advanced.custom_model_name.as_deref(),
            Some("keep-unrelated-preference")
        );
        assert!(store.ops().is_empty());
    });
}

#[test]
fn readable_store_transaction_rejects_invalid_state_before_any_callbacks() {
    with_temp_home("readable-cleanup-transaction-invalid", || {
        let original = write_cleanup_candidate();
        let path = crate::platform::paths::settings_path();
        let mut invalid = original.clone();
        invalid.extend_from_slice(b" invalid concurrent edit");
        for bytes in [Some(invalid), None] {
            if let Some(bytes) = &bytes {
                std::fs::write(&path, bytes).unwrap();
            } else {
                std::fs::remove_file(&path).unwrap();
            }
            let mutated = std::cell::Cell::new(false);
            let committed = std::cell::Cell::new(false);
            let result = UserPrefs::update_readable_model_transaction_with_post_commit(
                |_| {
                    mutated.set(true);
                    Ok(())
                },
                || committed.set(true),
            );
            assert!(result.is_err());
            assert!(!mutated.get());
            assert!(!committed.get());
            if let Some(bytes) = bytes {
                assert!(std::fs::read(&path).unwrap() == bytes);
            } else {
                assert!(!path.exists());
            }
        }
    });
}

#[test]
fn readable_store_transaction_commits_before_callback_and_keeps_lock_held() {
    with_temp_home("readable-cleanup-transaction-barrier", || {
        let before: UserPrefs = serde_json::from_slice(&write_cleanup_candidate()).unwrap();
        let path = crate::platform::paths::settings_path();
        let committed = std::cell::Cell::new(false);
        let returned = UserPrefs::update_readable_model_transaction_with_post_commit(
            |prefs| {
                prefs.remove_model("cleanup-default");
                Ok(())
            },
            || {
                assert!(!UserPrefs::user_prefs_lock_free_for_test());
                let persisted: UserPrefs =
                    serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
                assert!(persisted.model_by_id("cleanup-default").is_none());
                assert!(persisted.advanced.active_model_id.is_none());
                assert!(persisted.advanced.default_model_ref.is_none());
                assert_eq!(
                    persisted.advanced.default_model_selection_revision,
                    before.advanced.default_model_selection_revision
                );
                assert_eq!(
                    persisted.advanced.account_platform_default_model,
                    before.advanced.account_platform_default_model
                );
                assert!(persisted.model_by_id("user-fallback").is_some());
                assert!(!persisted.has_explicit_model_context_selection("cleanup-default"));
                assert!(persisted.has_explicit_model_context_selection("user-fallback"));
                committed.set(true);
            },
        )
        .unwrap();
        assert!(committed.get());
        assert!(returned.advanced.active_model_id.is_none());
        assert!(returned.advanced.default_model_ref.is_none());
        assert_eq!(
            returned.advanced.default_model_selection_revision,
            before.advanced.default_model_selection_revision
        );
        assert_eq!(
            returned.advanced.account_platform_default_model,
            before.advanced.account_platform_default_model
        );
        assert_eq!(
            serde_json::to_value(&returned).unwrap(),
            serde_json::from_slice::<serde_json::Value>(&std::fs::read(path).unwrap()).unwrap()
        );
        assert!(!returned.memory_enabled);
        assert_eq!(
            returned.advanced.custom_model_name.as_deref(),
            Some("keep-unrelated-preference")
        );
    });
}

fn with_legacy_cleanup_locale(label: &str, run: impl FnOnce(Language)) {
    with_temp_home(label, || {
        let _locale = crate::platform::test_support::EnvRestore::capture(&[
            "LANGUAGE",
            "LC_ALL",
            "LC_MESSAGES",
            "LANG",
        ]);
        // SAFETY: with_temp_home holds the shared ENV_LOCK. Native platforms
        // may obtain their actual preferred UI locale outside these variables.
        unsafe {
            std::env::set_var("LANGUAGE", "ja_JP");
            std::env::set_var("LC_ALL", "ja_JP.UTF-8");
            std::env::set_var("LC_MESSAGES", "ja_JP.UTF-8");
            std::env::set_var("LANG", "ja_JP.UTF-8");
        }
        // Obtain the existing public load contract on a valid legacy control,
        // without exposing the prefs parser's private locale helper to tests.
        let path = crate::platform::paths::settings_path();
        std::fs::write(&path, br#"{"theme":"liquid-dark"}"#).unwrap();
        let expected_language = UserPrefs::load().language;
        std::fs::remove_file(path).unwrap();
        run(expected_language);
    });
}

fn legacy_cleanup_preferences(remove_target: bool) -> serde_json::Value {
    let mut prefs: serde_json::Value = serde_json::from_slice(&write_cleanup_candidate()).unwrap();
    let fields = prefs.as_object_mut().unwrap();
    fields.insert("theme".to_string(), serde_json::json!("liquid-dark"));
    fields.remove("color_scheme");
    fields.remove("language");
    if remove_target {
        prefs["advanced"]["saved_models"]
            .as_array_mut()
            .unwrap()
            .retain(|model| model["id"] != "cleanup-default");
        prefs["advanced"]["active_model_id"] = serde_json::json!("user-fallback");
    }
    prefs
}

#[test]
fn readable_store_legacy_cleanup_preserves_appearance_and_system_language() {
    with_legacy_cleanup_locale("readable-cleanup-legacy-fields", |expected_language| {
        let prefs = legacy_cleanup_preferences(false);
        let path = crate::platform::paths::settings_path();
        std::fs::write(&path, serde_json::to_vec(&prefs).unwrap()).unwrap();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        assert!(
            delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store).unwrap()
        );
        let after: UserPrefs = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert_eq!(after.theme, Theme::LiquidDark);
        assert_eq!(after.color_scheme, ColorScheme::Dark);
        assert_eq!(after.language, expected_language);
        assert!(!after.memory_enabled);
        assert_eq!(
            after.advanced.custom_model_name.as_deref(),
            Some("keep-unrelated-preference")
        );
    });
}

#[test]
fn readable_store_legacy_cleanup_noop_preserves_appearance_and_system_language() {
    with_legacy_cleanup_locale("readable-cleanup-legacy-noop", |expected_language| {
        // The UI read the candidate first; another writer removed it while
        // restoring an older preferences file before conditional cleanup.
        let prefs = legacy_cleanup_preferences(true);
        let path = crate::platform::paths::settings_path();
        std::fs::write(&path, serde_json::to_vec(&prefs).unwrap()).unwrap();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        assert!(
            !delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store).unwrap()
        );
        let after: UserPrefs = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert_eq!(after.color_scheme, ColorScheme::Dark);
        assert_eq!(after.language, expected_language);
        assert_eq!(
            after.advanced.active_model_id.as_deref(),
            Some("user-fallback")
        );
        assert!(store.ops().is_empty());
    });
}

#[test]
fn readable_store_legacy_cleanup_keeps_explicit_language_and_appearance() {
    with_legacy_cleanup_locale("readable-cleanup-explicit-fields", |_| {
        let mut prefs = legacy_cleanup_preferences(false);
        prefs["language"] = serde_json::json!("en");
        prefs["color_scheme"] = serde_json::json!("light");
        let path = crate::platform::paths::settings_path();
        std::fs::write(&path, serde_json::to_vec(&prefs).unwrap()).unwrap();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        assert!(
            delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store).unwrap()
        );
        let after: UserPrefs = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert_eq!(after.theme, Theme::LiquidDark);
        assert_eq!(after.color_scheme, ColorScheme::Light);
        assert_eq!(after.language, Language::En);
    });
}

#[test]
fn readable_store_legacy_cleanup_migrates_thinking_once_without_changing_readonly_projection() {
    with_temp_home("readable-cleanup-legacy-thinking", || {
        let mut prefs: serde_json::Value =
            serde_json::from_slice(&write_cleanup_candidate()).unwrap();
        prefs
            .as_object_mut()
            .unwrap()
            .remove("local_thinking_default_migrated");
        let retained = &mut prefs["advanced"]["saved_models"][1];
        retained["preset"] = serde_json::json!("local_vllm");
        retained["model"] = serde_json::json!("legacy-retained");
        retained["base_url"] = serde_json::json!("http://127.0.0.1:8123/v1");
        retained["reasoning_effort"] = serde_json::json!("off");
        let path = crate::platform::paths::settings_path();
        let original = serde_json::to_vec(&prefs).unwrap();
        std::fs::write(&path, &original).unwrap();
        // Existing remote-task snapshots are model-only and read-only: the new
        // whole-preferences transaction must not silently change that purpose.
        let projection = UserPrefs::load_read_only_model_snapshot().unwrap();
        assert_eq!(
            projection
                .model_by_id("user-fallback")
                .unwrap()
                .reasoning_effort
                .as_deref(),
            Some("off")
        );
        assert!(!projection.local_thinking_default_migrated);
        assert!(std::fs::read(&path).unwrap() == original);

        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        assert!(
            delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store).unwrap()
        );
        let mut after: UserPrefs = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert!(after.local_thinking_default_migrated);
        assert!(
            after
                .model_by_id("user-fallback")
                .unwrap()
                .reasoning_effort
                .is_none()
        );
        after
            .advanced
            .saved_models
            .iter_mut()
            .find(|model| model.id == "user-fallback")
            .unwrap()
            .reasoning_effort = Some("off".to_string());
        std::fs::write(&path, serde_json::to_vec(&after).unwrap()).unwrap();
        assert!(
            !delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store).unwrap()
        );
        let explicit: UserPrefs = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert_eq!(
            explicit
                .model_by_id("user-fallback")
                .unwrap()
                .reasoning_effort
                .as_deref(),
            Some("off")
        );
    });
}

#[test]
fn readable_store_cleanup_preserves_other_local_and_account_defaults() {
    use crate::platform::prefs::{ModelSelectionRef, SavedModel};
    let selections = [
        ModelSelectionRef::local("user-fallback"),
        ModelSelectionRef::account_platform(SavedModel::builtin_account_platform().model),
    ];
    for (index, selection) in selections.into_iter().enumerate() {
        with_temp_home(&format!("readable-cleanup-other-default-{index}"), || {
            write_cleanup_candidate();
            let mut prefs = UserPrefs::load_read_only_model_snapshot().unwrap();
            prefs.set_default_model_ref(selection.clone()).unwrap();
            let before = prefs.clone();
            std::fs::write(
                crate::platform::paths::settings_path(),
                serde_json::to_vec(&prefs).unwrap(),
            )
            .unwrap();
            let store = crate::platform::credential_store::RecordingCredentialStore::new();
            assert!(
                delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store)
                    .unwrap()
            );
            let after = UserPrefs::load_read_only_model_snapshot().unwrap();
            assert_strict_selection_round_trip(&after);
            assert!(after.model_by_id("cleanup-default").is_none());
            assert_eq!(after.advanced.default_model_ref, Some(selection));
            assert_eq!(
                after.advanced.active_model_id,
                before.advanced.active_model_id
            );
            assert_eq!(
                after.advanced.default_model_selection_revision,
                before.advanced.default_model_selection_revision
            );
            assert_eq!(
                after.advanced.account_platform_default_model,
                before.advanced.account_platform_default_model
            );
            assert_eq!(
                after.model_by_id("user-fallback"),
                before.model_by_id("user-fallback")
            );
            let account_id = SavedModel::builtin_account_platform().id;
            assert_eq!(
                after.model_by_id(&account_id),
                before.model_by_id(&account_id)
            );
            assert!(
                !after
                    .advanced
                    .model_store_context_repairs
                    .contains_key("cleanup-default")
            );
            assert!(after.has_explicit_model_context_selection("user-fallback"));
            assert!(!after.memory_enabled);
            assert_eq!(
                after.advanced.custom_model_name,
                before.advanced.custom_model_name
            );
            assert_eq!(
                store.ops(),
                vec!["delete:pinvou3-model-api-key:model:cleanup-default".to_string()]
            );
        });
    }
}

#[test]
fn readable_model_list_migrates_legacy_account_reference_only_in_memory() {
    use crate::platform::prefs::{ModelSelectionRef, SavedModel};
    with_temp_home("readable-list-legacy-account", || {
        let mut prefs = UserPrefs::default();
        let mut legacy = SavedModel::builtin_account_platform();
        let catalog = legacy.model.clone();
        let current_id = legacy.id.clone();
        legacy.id = "builtin_llmapi".to_string();
        prefs.advanced.saved_models = vec![legacy];
        prefs.advanced.active_model_id = Some("builtin_llmapi".to_string());
        let path = crate::platform::paths::settings_path();
        let original = serde_json::to_vec(&prefs).unwrap();
        std::fs::write(&path, &original).unwrap();
        let entries = home_entries();
        let snapshot = list(Some(true)).unwrap();
        assert_eq!(
            snapshot.active_model_id.as_deref(),
            Some(current_id.as_str())
        );
        assert_eq!(
            snapshot.default_model_ref,
            Some(ModelSelectionRef::account_platform(catalog))
        );
        assert!(snapshot.models.iter().any(|row| row.model.id == current_id));
        assert!(
            !snapshot
                .models
                .iter()
                .any(|row| row.model.id == "builtin_llmapi")
        );
        assert_eq!(std::fs::read(&path).unwrap(), original);
        assert_eq!(
            home_entries(),
            entries,
            "strict projection creates no keyring artifacts"
        );
    });
}

#[test]
fn readable_store_cleanup_last_user_keeps_builtin_without_selecting_it() {
    use crate::platform::prefs::SavedModel;
    with_temp_home("readable-cleanup-last-local-model", || {
        write_cleanup_candidate();
        let mut prefs = UserPrefs::load_read_only_model_snapshot().unwrap();
        prefs.remove_model("user-fallback");
        std::fs::write(
            crate::platform::paths::settings_path(),
            serde_json::to_vec(&prefs).unwrap(),
        )
        .unwrap();
        let store = crate::platform::credential_store::RecordingCredentialStore::new();
        assert!(
            delete_model_if_store_link_inner("cleanup-default", "cleanup-card", &store).unwrap()
        );
        let after = UserPrefs::load_read_only_model_snapshot().unwrap();
        assert_strict_selection_round_trip(&after);
        let builtin_id = SavedModel::builtin_account_platform().id;
        assert_eq!(after.advanced.saved_models.len(), 1);
        assert_eq!(after.advanced.saved_models[0].id, builtin_id);
        assert!(after.advanced.default_model_ref.is_none());
        assert!(after.advanced.active_model_id.is_none());
        assert!(
            !after
                .advanced
                .model_store_context_repairs
                .contains_key("cleanup-default")
        );
        assert_eq!(
            store.ops(),
            vec!["delete:pinvou3-model-api-key:model:cleanup-default".to_string()]
        );
    });
}

/// A cleanup commit must survive both independent strict readers and a later
/// strict write transaction. None is intentional authority, not a migration hint.
fn assert_strict_selection_round_trip(expected: &UserPrefs) {
    let assert_selection = |actual: &UserPrefs| {
        assert_eq!(
            actual.advanced.default_model_ref,
            expected.advanced.default_model_ref
        );
        assert_eq!(
            actual.advanced.active_model_id,
            expected.advanced.active_model_id
        );
        assert_eq!(
            actual.advanced.default_model_selection_revision,
            expected.advanced.default_model_selection_revision
        );
        assert_eq!(
            actual.advanced.account_platform_default_model,
            expected.advanced.account_platform_default_model
        );
        assert_eq!(actual.model_selection_schema_version, 1);
        assert_eq!(actual.model_path_schema_version, 1);
        assert_eq!(
            actual.advanced.model_store_context_repairs,
            expected.advanced.model_store_context_repairs
        );
    };
    let path = crate::platform::paths::settings_path();
    let committed = std::fs::read(&path).unwrap();
    let disk: UserPrefs = serde_json::from_slice(&committed).unwrap();
    assert_selection(&disk);
    assert_selection(&UserPrefs::load_existing_read_only_model_snapshot().unwrap());
    assert_selection(&UserPrefs::load_read_only_model_snapshot().unwrap());
    assert_eq!(
        std::fs::read(&path).unwrap(),
        committed,
        "strict readers must not persist migrations"
    );

    let mutation_seen = std::cell::Cell::new(false);
    let committed_seen = std::cell::Cell::new(false);
    let writable = UserPrefs::update_readable_model_transaction_with_post_commit(
        |prefs| {
            assert_selection(prefs);
            mutation_seen.set(true);
            Ok(())
        },
        || committed_seen.set(true),
    )
    .unwrap();
    assert!(mutation_seen.get());
    assert!(committed_seen.get());
    assert_selection(&writable); // Includes the production post-commit writable reload.
    let recommitted = std::fs::read(&path).unwrap();
    let disk: UserPrefs = serde_json::from_slice(&recommitted).unwrap();
    assert_selection(&disk);
    assert_selection(&UserPrefs::load_existing_read_only_model_snapshot().unwrap());
    assert_selection(&UserPrefs::load_read_only_model_snapshot().unwrap());
    assert_eq!(std::fs::read(&path).unwrap(), recommitted);
}
