//! Settings faults must not turn a store-specific sync into a default-file save.
use super::*;
use crate::platform::{
    credential_store::{CredentialReference, CredentialStore, SystemCredentialStore},
    paths,
    test_support::{with_read_permission_denied, with_temp_home},
};
use std::{fs, path::PathBuf};

struct CredentialSnapshot {
    path: PathBuf,
    bytes: Vec<u8>,
    entries: Vec<std::ffi::OsString>,
}

impl CredentialSnapshot {
    fn seed() -> Self {
        let reference = CredentialReference::for_model(&format!(
            "linked-settings-sentinel-{}",
            uuid::Uuid::new_v4()
        ));
        SystemCredentialStore::new()
            .set(&reference, "test-fixture-value-not-a-real-credential")
            .unwrap();
        let path = paths::pinvou3_home().join("secrets/secrets.json");
        Self {
            bytes: fs::read(&path).unwrap(),
            path,
            entries: home_entries(),
        }
    }

    fn assert_unchanged(&self) {
        assert_eq!(fs::read(&self.path).unwrap(), self.bytes);
        assert_eq!(home_entries(), self.entries, "no new credential artifacts");
    }
}

fn home_entries() -> Vec<std::ffi::OsString> {
    let mut entries = fs::read_dir(paths::pinvou3_home())
        .unwrap()
        .map(|entry| entry.unwrap().file_name())
        .filter(|name| name != "settings.json")
        .collect::<Vec<_>>();
    entries.sort();
    entries
}

fn probe_in_flight(
    server: &Server,
) -> (
    thread::JoinHandle<Result<Vec<String>, String>>,
    std::sync::mpsc::SyncSender<()>,
) {
    let (entered, release) = server.hold_next_response();
    let syncing = thread::spawn(|| super::super::super::sync(None));
    entered.recv_timeout(Duration::from_secs(3)).unwrap();
    (syncing, release)
}

#[test]
fn linked_settings_fault_sync_preserves_invalid_settings_after_a_valid_preview() {
    with_temp_home("linked-settings-sync-invalid", || {
        let server = Server::new();
        setup(CARD, &server.base);
        let credentials = CredentialSnapshot::seed();
        let (syncing, release) = probe_in_flight(&server);
        let path = paths::settings_path();
        let invalid = b"{\"advanced\": invalid concurrent settings edit";
        fs::write(&path, invalid).unwrap();
        release.send(()).unwrap();
        let result = syncing.join().unwrap();
        assert_eq!(
            fs::read(path).unwrap(),
            invalid,
            "sync must not save defaults"
        );
        assert_eq!(result.unwrap_err(), "model_settings_invalid");
        credentials.assert_unchanged();
        assert_eq!(server.count(), 1, "the real sync must reach the HTTP probe");
    });
}

#[test]
fn linked_settings_fault_sync_does_not_recreate_settings_removed_during_probe() {
    with_temp_home("linked-settings-sync-missing", || {
        let server = Server::new();
        setup(CARD, &server.base);
        let credentials = CredentialSnapshot::seed();
        let (syncing, release) = probe_in_flight(&server);
        let path = paths::settings_path();
        fs::remove_file(&path).unwrap();
        release.send(()).unwrap();
        let result = syncing.join().unwrap();
        assert!(!path.exists(), "sync must not recreate removed settings");
        assert_eq!(result.unwrap_err(), "model_settings_unavailable");
        credentials.assert_unchanged();
        assert_eq!(server.count(), 1);
    });
}

#[test]
fn linked_settings_fault_sync_preserves_denied_settings_during_probe() {
    with_temp_home("linked-settings-sync-denied", || {
        let server = Server::new();
        setup(CARD, &server.base);
        let credentials = CredentialSnapshot::seed();
        let path = paths::settings_path();
        let original = fs::read(&path).unwrap();
        let (syncing, release) = probe_in_flight(&server);
        let mut syncing = Some(syncing);
        let result = with_read_permission_denied(&path, || {
            release.send(()).unwrap();
            syncing.take().unwrap().join().unwrap()
        })
        .unwrap();
        let Some(result) = result else {
            // A privileged host cannot exercise this refusal. Release the real
            // request after the helper has restored permissions and join it.
            release.send(()).unwrap();
            syncing.take().unwrap().join().unwrap().unwrap();
            return;
        };
        assert_eq!(
            fs::read(path).unwrap(),
            original,
            "sync must not replace unreadable settings"
        );
        assert_eq!(result.unwrap_err(), "model_settings_unavailable");
        credentials.assert_unchanged();
        assert_eq!(server.count(), 1);
    });
}

#[test]
fn linked_settings_fault_sync_rejects_an_invalid_preview_without_writes() {
    with_temp_home("linked-settings-sync-invalid-preview", || {
        let server = Server::new();
        setup(CARD, &server.base);
        let credentials = CredentialSnapshot::seed();
        let path = paths::settings_path();
        let invalid = b"{\"advanced\": invalid original settings";
        fs::write(&path, invalid).unwrap();
        let result = super::super::super::sync(None);
        assert_eq!(fs::read(path).unwrap(), invalid);
        assert_eq!(result.unwrap_err(), "model_settings_invalid");
        credentials.assert_unchanged();
        assert_eq!(
            server.count(),
            0,
            "invalid preview must not start capability probes"
        );
    });
}

#[test]
fn linked_settings_fault_follow_does_not_migrate_unrelated_pending_credentials() {
    with_temp_home("linked-settings-follow-pending", || {
        let server = Server::new();
        setup(CARD, &server.base);
        let credentials = CredentialSnapshot::seed();
        let path = paths::settings_path();
        let mut prefs: serde_json::Value =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        prefs["advanced"]["custom_api_key"] =
            serde_json::json!("test-fixture-pending-not-a-real-key");
        let original = serde_json::to_vec(&prefs).unwrap();
        fs::write(&path, &original).unwrap();
        let result = super::super::super::follow_deployment_context(CARD, "stable-id");
        assert_eq!(
            fs::read(path).unwrap(),
            original,
            "follow must preserve unrelated pending credentials"
        );
        assert_eq!(result.unwrap_err(), "model_credential_migration_required");
        credentials.assert_unchanged();
        assert_eq!(server.count(), 0);
    });
}

#[test]
fn linked_settings_fault_sync_control_repairs_context_and_preserves_other_state() {
    with_temp_home("linked-settings-sync-control", || {
        let server = Server::new();
        let mut expected = setup(CARD, &server.base);
        let credentials = CredentialSnapshot::seed();
        assert_eq!(super::super::super::sync(None).unwrap(), ["stable-id"]);
        let saved = UserPrefs::load_read_only_model_snapshot().unwrap();
        expected.context_window_tokens = Some(262144);
        assert_eq!(saved.model_by_id("stable-id"), Some(&expected));
        assert_eq!(saved.advanced.active_model_id.as_deref(), Some("stable-id"));
        assert_eq!(saved.advanced.model_store_context_repairs.len(), 1);
        credentials.assert_unchanged();
        assert_eq!(server.count(), 1);
    });
}
