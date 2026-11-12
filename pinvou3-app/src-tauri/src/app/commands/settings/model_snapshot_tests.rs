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

#[test]
fn effective_model_config_snapshot_reports_empty_selection_after_default_removed() {
    use crate::features::assistant::platform::bridge::Pinvou3Bridge;

    with_temp_home("effective-config-default-removed", || {
        let _env = effective_config_env(&[]);
        let mut prefs = UserPrefs::default();
        prefs.memory_enabled = false;
        prefs.advanced.saved_models = vec![
            serde_json::from_value(serde_json::json!({
                "id": "store-default", "name": "Store default", "preset": "openai_compatible",
                "model": "store-wire", "base_url": "http://127.0.0.1:8911/v1",
                "provider_kind": "custom", "model_store_card_id": "store-card",
                "credential_ref": { "service": "pinvou3-model-api-key", "account": "model:store-default", "version": 1 },
                "credential_state": "configured", "has_secret": true
            }))
            .unwrap(),
            serde_json::from_value(serde_json::json!({
                "id": "user-fallback", "name": "User fallback", "preset": "openai_compatible",
                "model": "user-wire", "base_url": "https://example.test/v1", "provider_kind": "custom"
            }))
            .unwrap(),
        ];
        prefs
            .set_default_model_ref(crate::platform::prefs::ModelSelectionRef::local(
                "store-default",
            ))
            .unwrap();
        assert!(prefs.model_selection_schema_version >= 1);
        // Deleting the registered default clears the typed reference without
        // re-selecting another retained configuration (user-confirmed PR
        // semantics); the display snapshot must agree with the send gate.
        prefs.remove_model("store-default");
        assert!(prefs.advanced.default_model_ref.is_none());
        assert!(prefs.advanced.active_model_id.is_none());
        let mut bridge = Pinvou3Bridge::test_fixture(None);
        bridge.prefs = prefs;
        assert_eq!(bridge.prefs.active_model().unwrap().id, "user-fallback");
        // The command must clear a stale pool-bridge binding before projection.
        bridge.session_model = bridge.prefs.active_model().cloned();
        let config = effective_model_config_for_selection(&mut bridge, None).unwrap();
        assert!(bridge.session_model.is_none());
        assert_eq!(config.model, "");
        assert_eq!(config.base_url, "");
        assert_eq!(config.provider, "");
        assert_eq!(config.preset, "");
        assert_eq!(config.credential_state, CredentialState::Missing);
        assert!(!config.has_secret);
        assert_eq!(config.credential_mode, ModelCredentialMode::None);
        assert!(config.model_ref.is_none());
        assert!(config.env_overrides.is_empty());
        assert!(
            !config.requires_user_api_key,
            "no endpoint is selected; the API-key gate must not fire"
        );
        assert_eq!(
            serde_json::to_value(&config).unwrap()["model"],
            serde_json::json!("")
        );

        // Process overrides cannot conjure a selection after deletion either.
        {
            let _env = effective_config_env(&[
                ("DEEPSEEK_MODEL", "unrelated-env-model"),
                ("DEEPSEEK_BASE_URL", "https://env.example.invalid/v1"),
                (MODEL_API_KEY_ENV_OVERRIDE, "fixture-only-key-sentinel"),
                ("DEEPSEEK_PROVIDER", "unrelated-env-provider"),
            ]);
            let config = effective_model_config_for_selection(&mut bridge, None).unwrap();
            assert_eq!(config.model, "");
            assert_eq!(config.base_url, "");
            assert_eq!(config.provider, "");
            assert_eq!(config.api_key, "");
            assert_eq!(config.credential_state, CredentialState::Missing);
            assert_eq!(config.credential_mode, ModelCredentialMode::None);
            assert!(!config.requires_user_api_key);
            assert!(!config.has_secret);
            assert!(config.model_ref.is_none());
            assert!(config.env_overrides.is_empty());
        }

        // Re-registering an explicit default restores the resolved display.
        let mut restored = bridge.prefs.clone();
        restored
            .set_default_model_ref(crate::platform::prefs::ModelSelectionRef::local(
                "user-fallback",
            ))
            .unwrap();
        bridge.prefs = restored;
        let config = effective_model_config_for_selection(&mut bridge, None).unwrap();
        assert_eq!(config.model, "user-wire");
        assert_eq!(config.base_url, "https://example.test/v1");
        // The remote endpoint keeps the ordinary key requirement; only the
        // selection-required state suppresses it.
        assert!(config.requires_user_api_key);
        assert_eq!(config.credential_mode, ModelCredentialMode::UserManaged);
        assert_eq!(
            config.model_ref,
            Some(ModelSelectionRef::local("user-fallback"))
        );
    });
}

// Call only while holding ENV_LOCK (all callers use with_temp_home). The guard
// restores even on panic, before that shared lock is released.
fn effective_config_env(
    overrides: &[(&'static str, &str)],
) -> crate::platform::test_support::EnvRestore {
    let names = [
        "DEEPSEEK_MODEL",
        "DEEPSEEK_BASE_URL",
        MODEL_API_KEY_ENV_OVERRIDE,
        "DEEPSEEK_PROVIDER",
    ];
    let restore = crate::platform::test_support::EnvRestore::capture(&names);
    for name in names {
        // SAFETY: the caller holds the crate-wide ENV_LOCK via with_temp_home.
        unsafe { std::env::remove_var(name) };
    }
    for (name, value) in overrides {
        // SAFETY: the caller holds the same ENV_LOCK for this guard's lifetime.
        unsafe { std::env::set_var(name, value) };
    }
    restore
}

fn effective_config_user_model(id: &str, base_url: &str) -> SavedModel {
    serde_json::from_value(serde_json::json!({
        "id": id, "name": id, "preset": "openai_compatible",
        "model": "user-wire", "base_url": base_url,
        "provider_kind": "custom", "vendor": "openai",
        "credential_state": "unavailable", "has_secret": true,
        "credential_ref": { "service": "pinvou3-model-api-key", "account": format!("model:{id}"), "version": 1 }
    }))
    .unwrap()
}

#[test]
fn effective_model_config_backend_projection_ignores_local_overrides_and_secret_metadata() {
    use crate::features::assistant::platform::bridge::Pinvou3Bridge;

    with_temp_home("effective-config-backend-projection", || {
        let overrides = [
            ("DEEPSEEK_MODEL", "unrelated-env-model"),
            ("DEEPSEEK_BASE_URL", "https://env.example.invalid/v1"),
            (MODEL_API_KEY_ENV_OVERRIDE, "fixture-only-key-sentinel"),
            ("DEEPSEEK_PROVIDER", "unrelated-env-provider"),
        ];
        let _env = effective_config_env(&overrides);
        let mut carrier = SavedModel::builtin_account_platform();
        // A backend-managed carrier owns no user secret, even if stale metadata
        // arrived at this projection before the ordinary prefs refresh.
        carrier.has_secret = true;
        carrier.credential_state = CredentialState::Configured;
        carrier.credential_ref = Some(CredentialReference::for_model(&carrier.id));
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![carrier];
        let global = ModelSelectionRef::account_platform("global-catalog-model");
        let session = ModelSelectionRef::account_platform("session-catalog-model");
        prefs.set_default_model_ref(global.clone()).unwrap();
        let store = SessionStore::boot().unwrap();
        store
            .set_session_model_ref("selected-chat", Some(session.clone()))
            .unwrap();
        let mut bridge = Pinvou3Bridge::test_fixture(Some(effective_config_user_model(
            "stale-pool-binding",
            "https://stale.example.invalid/v1",
        )));
        bridge.prefs = prefs;
        let config = effective_model_config_for_selection(
            &mut bridge,
            store.session_model_ref("selected-chat"),
        )
        .unwrap();
        assert_eq!(config.model_ref, Some(session));
        assert_eq!(config.model, "session-catalog-model");
        assert_eq!(bridge.session_model.as_ref().unwrap().model, config.model);
        assert_eq!(
            bridge.prefs.advanced.default_model_ref,
            Some(global.clone())
        );
        assert_eq!(config.credential_mode, ModelCredentialMode::BackendManaged);
        assert!(!config.requires_user_api_key);
        assert_eq!(config.credential_state, CredentialState::Missing);
        assert!(!config.has_secret);
        assert_eq!(config.api_key, "");
        assert_eq!(config.base_url, "");
        // Same source as Pinvou3Bridge::provider's account_gateway_runtime
        // branch after preparation; local provider env never owns that route.
        assert_eq!(config.provider, "openai");
        assert!(config.env_overrides.is_empty());
        let serialized = serde_json::to_string(&config).unwrap();
        for (_, sentinel) in overrides {
            assert!(!serialized.contains(sentinel));
        }

        let config = effective_model_config_for_selection(
            &mut bridge,
            store.session_model_ref("unbound-chat"),
        )
        .unwrap();
        assert_eq!(config.model_ref, Some(global));
        assert_eq!(config.model, "global-catalog-model");
        assert_eq!(config.credential_mode, ModelCredentialMode::BackendManaged);
    });
}

#[test]
fn effective_model_config_preserves_user_and_direct_credentials_and_source_refs() {
    use crate::features::assistant::platform::bridge::Pinvou3Bridge;

    with_temp_home("effective-config-user-direct", || {
        let _env = effective_config_env(&[]);
        let cloud = effective_config_user_model("cloud", "https://user.example.invalid/v1");
        let direct = effective_config_user_model("direct", "http://127.0.0.1:8911/v1");
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![SavedModel::builtin_account_platform(), cloud, direct];
        prefs
            .set_default_model_ref(ModelSelectionRef::account_platform("global-catalog-model"))
            .unwrap();
        let store = SessionStore::boot().unwrap();
        let local = ModelSelectionRef::local("cloud");
        store
            .set_session_model_ref("user-chat", Some(local.clone()))
            .unwrap();
        let mut bridge = Pinvou3Bridge::test_fixture(None);
        bridge.prefs = prefs;
        let config =
            effective_model_config_for_selection(&mut bridge, store.session_model_ref("user-chat"))
                .unwrap();
        assert_eq!(config.model_ref, Some(local));
        assert_eq!(config.credential_mode, ModelCredentialMode::UserManaged);
        assert!(config.requires_user_api_key);
        assert_eq!(config.credential_state, CredentialState::Unavailable);
        assert!(config.has_secret);
        assert_eq!(config.model, "user-wire");
        assert_eq!(config.base_url, "https://user.example.invalid/v1");
        assert_eq!(config.provider, "openai");
        assert_eq!(config.api_key, "");

        // A retained explicit session selection also survives clearing the
        // global typed default; only an unbound chat requires a new selection.
        bridge.prefs.advanced.default_model_ref = None;
        let config =
            effective_model_config_for_selection(&mut bridge, store.session_model_ref("user-chat"))
                .unwrap();
        assert_eq!(config.model_ref, Some(ModelSelectionRef::local("cloud")));
        assert_eq!(config.credential_state, CredentialState::Unavailable);

        bridge
            .prefs
            .set_default_model_ref(ModelSelectionRef::local("direct"))
            .unwrap();
        let config = effective_model_config_for_selection(&mut bridge, None).unwrap();
        assert_eq!(config.model_ref, Some(ModelSelectionRef::local("direct")));
        assert_eq!(config.credential_mode, ModelCredentialMode::None);
        assert!(!config.requires_user_api_key);
        assert_eq!(config.credential_state, CredentialState::Unavailable);
        assert!(config.has_secret);
        assert_eq!(config.base_url, "http://127.0.0.1:8911/v1");

        // User/direct paths retain their existing bridge env precedence. A
        // loopback override remains unauthenticated; a cloud override needs a
        // user key. Both report env status without returning any secret bytes.
        for (base_url, mode) in [
            ("http://127.0.0.1:8912/v1", ModelCredentialMode::None),
            (
                "https://env.example.invalid/v1",
                ModelCredentialMode::UserManaged,
            ),
        ] {
            let _env = effective_config_env(&[
                ("DEEPSEEK_MODEL", "user-env-model"),
                ("DEEPSEEK_BASE_URL", base_url),
                (MODEL_API_KEY_ENV_OVERRIDE, "fixture-only-key-sentinel"),
                ("DEEPSEEK_PROVIDER", "user-env-provider"),
            ]);
            let config = effective_model_config_for_selection(&mut bridge, None).unwrap();
            assert_eq!(config.model_ref, Some(ModelSelectionRef::local("direct")));
            assert_eq!(config.credential_mode, mode);
            assert_eq!(
                config.requires_user_api_key,
                mode == ModelCredentialMode::UserManaged
            );
            assert_eq!(config.credential_state, CredentialState::EnvOverride);
            assert!(config.has_secret);
            assert_eq!(config.model, "user-env-model");
            assert_eq!(config.base_url, base_url);
            assert_eq!(config.provider, "user-env-provider");
            assert_eq!(config.api_key, "");
            assert_eq!(
                config.env_overrides,
                vec!["model", "base_url", "api_key", "provider"]
            );
        }
    });
}

#[test]
fn effective_model_config_rejects_deleted_references_and_preserves_legacy_fallback() {
    use crate::features::assistant::platform::bridge::Pinvou3Bridge;

    with_temp_home("effective-config-ref-boundaries", || {
        let _env = effective_config_env(&[]);
        let mut prefs = UserPrefs::default();
        prefs.advanced.saved_models = vec![effective_config_user_model(
            "retained",
            "http://127.0.0.1:8911/v1",
        )];
        prefs
            .set_default_model_ref(ModelSelectionRef::local("retained"))
            .unwrap();
        let mut bridge = Pinvou3Bridge::test_fixture(None);
        bridge.prefs = prefs;
        assert!(
            effective_model_config_for_selection(
                &mut bridge,
                Some(ModelSelectionRef::local("deleted-session-model")),
            )
            .unwrap_err()
            .starts_with("model_selection_unavailable:")
        );
        bridge.prefs.advanced.default_model_ref =
            Some(ModelSelectionRef::local("deleted-default-model"));
        assert!(
            effective_model_config_for_selection(&mut bridge, None)
                .unwrap_err()
                .starts_with("model_selection_unavailable:")
        );
        assert!(
            effective_model_config_for_selection(
                &mut bridge,
                Some(ModelSelectionRef::account_platform(" ")),
            )
            .unwrap_err()
            .starts_with("model_selection_unavailable:")
        );

        bridge.prefs.advanced.default_model_ref = None;
        bridge.prefs.model_selection_schema_version = 0;
        let config = effective_model_config_for_selection(&mut bridge, None).unwrap();
        assert_eq!(config.model, "user-wire");
        assert_eq!(config.base_url, "http://127.0.0.1:8911/v1");
        assert!(config.model_ref.is_none());
        assert_eq!(config.credential_mode, ModelCredentialMode::None);
        assert_eq!(bridge.session_model.as_ref().unwrap().id, "retained");
    });
}

#[test]
fn draft_image_capability_rejects_cleared_default_without_reporting_a_retained_route() {
    use crate::features::assistant::platform::bridge::Pinvou3Bridge;

    with_temp_home("draft-image-cleared-default", || {
        let _env = effective_config_env(&[
            ("DEEPSEEK_MODEL", "gpt-4o"),
            ("DEEPSEEK_BASE_URL", "https://env.example.invalid/v1"),
            (MODEL_API_KEY_ENV_OVERRIDE, "image-projection-fixture"),
        ]);
        let mut retained = effective_config_user_model("retained", "https://example.test/v1");
        retained.model = "gpt-4o".into();
        let mut bridge = Pinvou3Bridge::test_fixture(None);
        bridge.prefs.advanced.saved_models = vec![retained.clone()];
        bridge.prefs.advanced.active_model_id = None;
        bridge.prefs.advanced.default_model_ref = None;
        bridge.prefs.model_selection_schema_version = 1;
        bridge.session_model = Some(retained);

        // The retained model and process overrides both advertise native
        // cloud image input, but neither selects a model for this draft.
        assert_eq!(bridge.effective_image_capability().as_str(), "supported");
        let error = draft_image_input_capability(&mut bridge).unwrap_err();
        assert!(error.starts_with("model_selection_required:"));
        assert!(bridge.session_model.is_none());
    });
}

#[test]
fn draft_image_capability_resolves_the_typed_default_and_preserves_legacy_selection() {
    use crate::features::assistant::platform::bridge::Pinvou3Bridge;

    with_temp_home("draft-image-default-binding", || {
        let _env = effective_config_env(&[]);
        let mut first = effective_config_user_model("first", "https://example.test/v1");
        first.model = "gpt-4o".into();
        let selected = effective_config_user_model("selected", "http://127.0.0.1:8911/v1");
        let mut bridge = Pinvou3Bridge::test_fixture(None);
        bridge.prefs.advanced.saved_models = vec![first.clone(), selected];
        bridge.prefs.advanced.active_model_id = Some("first".into());
        bridge.prefs.advanced.default_model_ref = Some(ModelSelectionRef::local("selected"));
        bridge.prefs.model_selection_schema_version = 1;
        bridge.session_model = Some(first);

        let info = draft_image_input_capability(&mut bridge).unwrap();
        assert_eq!(bridge.session_model.as_ref().unwrap().id, "selected");
        assert_eq!(info.capability, "unknown");
        assert_eq!(info.image_mode, "unsupported");
        assert!(info.is_local_endpoint);
        assert_eq!(info.vision_is_local_endpoint, None);

        bridge.prefs.advanced.default_model_ref = Some(ModelSelectionRef::local("deleted"));
        assert!(
            draft_image_input_capability(&mut bridge)
                .unwrap_err()
                .starts_with("model_selection_unavailable:")
        );

        bridge.prefs.advanced.default_model_ref = None;
        bridge.prefs.model_selection_schema_version = 0;
        let info = draft_image_input_capability(&mut bridge).unwrap();
        assert_eq!(bridge.session_model.as_ref().unwrap().id, "first");
        assert_eq!(info.capability, "supported");
        assert_eq!(info.image_mode, "native");
        assert!(!info.is_local_endpoint);
    });
}
