use super::*;
use crate::features::sessions::with_isolated_store_restoring_home;
use std::path::Path;

fn with_session(action: impl FnOnce(&SessionStore, &str, &Path)) {
    with_isolated_store_restoring_home(|store| {
        let root = store.manager.sessions_dir();
        let session = store
            .create_new("synthetic-model".into(), None, root.join("local"))
            .unwrap();
        std::fs::write(
            root.parent().unwrap().join("settings.json"),
            br#"{"remote_task_permissions":{"chat":"plan","code":"plan"}}"#,
        )
        .unwrap();
        action(store, &session.metadata.id, root);
    });
}

#[test]
fn immutable_lane_and_mode_survive_fresh_store_and_same_mode_cross_lane_claims() {
    use SerializableMode::Plan;
    for code in [false, true] {
        with_session(|store, id, _| {
            initialize_new_session(store, id, code, Plan).unwrap();
            assert_eq!(for_session(store, id, code).unwrap(), Plan);
            assert_eq!(for_session(store, id, !code).unwrap_err(), CHANGED);
            let reopened = SessionStore::boot().unwrap();
            assert_eq!(for_session(&reopened, id, code).unwrap(), Plan);
            assert_eq!(for_session(&reopened, id, !code).unwrap_err(), CHANGED);
            assert_eq!(
                initialize_new_session(&reopened, id, !code, Plan).unwrap_err(),
                CHANGED
            );
            assert_eq!(for_session(&reopened, id, code).unwrap(), Plan);
        });
    }
}

#[test]
fn an_existing_fact_never_resets_user_mode_even_when_policy_is_changed_to_match_it() {
    use SerializableMode::{Plan, Yolo};
    with_session(|store, id, root| {
        initialize_new_session(store, id, false, Plan).unwrap();
        store.set_mode_and_persist(id, Yolo).unwrap();
        assert_eq!(
            initialize_new_session(store, id, false, Plan).unwrap_err(),
            CHANGED
        );
        assert_eq!(store.strict_durable_mode_entry(id).unwrap(), Some(Yolo));
        assert_eq!(store.mode_state(id).mode, Yolo);
        std::fs::write(
            root.parent().unwrap().join("settings.json"),
            br#"{"remote_task_permissions":{"chat":"yolo","code":"yolo"}}"#,
        )
        .unwrap();
        assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
        assert_eq!(for_session(store, id, true).unwrap_err(), CHANGED);
        assert_eq!(
            initialize_new_session(store, id, false, Yolo).unwrap_err(),
            CHANGED
        );
    });
}

#[test]
fn missing_legacy_or_damaged_facts_never_authorize_an_existing_mode() {
    use SerializableMode::Plan;
    with_session(|store, id, root| {
        store.set_mode_and_persist(id, Plan).unwrap();
        assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
        assert!(!root.join(id).join("remote-task-permission.json").exists());
        initialize_new_session(store, id, false, Plan).unwrap();
        let path = root.join(id).join("remote-task-permission.json");
        let original = std::fs::read(&path).unwrap();
        for damaged in [
            b"{".to_vec(), vec![b' '; 4097],
            serde_json::to_vec(&serde_json::json!({"version":2,"session_id":id,"code":false,"mode":"plan"})).unwrap(),
            serde_json::to_vec(&serde_json::json!({"version":1,"session_id":"other","code":false,"mode":"plan"})).unwrap(),
            serde_json::to_vec(&serde_json::json!({"version":1,"session_id":id,"code":false,"mode":"plan","extra":true})).unwrap(),
        ] {
            std::fs::write(&path, &damaged).unwrap();
            assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
            assert_eq!(initialize_new_session(store, id, false, Plan).unwrap_err(), CHANGED);
            assert_eq!(std::fs::read(&path).unwrap(), damaged, "never repair by overwriting evidence");
        }
        std::fs::write(&path, original).unwrap();
        assert_eq!(for_session(store, id, false).unwrap(), Plan);
        std::fs::remove_file(&path).unwrap();
        assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
    });
}

#[test]
fn durable_mode_errors_do_not_fall_back_to_the_live_cache() {
    use SerializableMode::Plan;
    with_session(|store, id, root| {
        initialize_new_session(store, id, false, Plan).unwrap();
        let path = root.join("_session_mode_states.json");
        std::fs::write(&path, b"{").unwrap();
        assert_eq!(
            store.durable_mode_entry(id),
            Some(Plan),
            "ordinary restore fallback remains unchanged"
        );
        assert!(store.strict_durable_mode_entry(id).is_err());
        assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
        std::fs::write(&path, vec![b' '; 16 * 1024 * 1024 + 1]).unwrap();
        assert!(store.strict_durable_mode_entry(id).is_err());
        assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
        std::fs::remove_file(&path).unwrap();
        assert_eq!(store.strict_durable_mode_entry(id).unwrap(), None);
        assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
        std::fs::create_dir(&path).unwrap();
        assert_eq!(store.durable_mode_entry(id), Some(Plan));
        assert!(store.strict_durable_mode_entry(id).is_err());
        assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
    });
}

#[test]
fn failed_fact_publication_leaves_mode_untouched_and_cannot_be_overwritten() {
    use SerializableMode::{Plan, Yolo};
    with_session(|store, id, root| {
        store.set_mode_and_persist(id, Yolo).unwrap();
        let mode_path = root.join("_session_mode_states.json");
        let before = std::fs::read(&mode_path).unwrap();
        let blocked = root.join(id).join("remote-task-permission.json");
        std::fs::create_dir_all(&blocked).unwrap();
        assert_eq!(
            initialize_new_session(store, id, false, Plan).unwrap_err(),
            CHANGED
        );
        assert_eq!(std::fs::read(mode_path).unwrap(), before);
        assert_eq!(store.mode_state(id).mode, Yolo);
        assert!(blocked.is_dir());
    });
}

#[test]
fn fact_survives_failed_mode_write_without_allowing_retry_to_reset_modes() {
    use SerializableMode::Plan;
    with_session(|store, id, root| {
        let blocked = root.join("_session_mode_states.json");
        std::fs::create_dir(&blocked).unwrap();
        assert_eq!(
            initialize_new_session(store, id, true, Plan).unwrap_err(),
            "task_session_backend_bind_failed"
        );
        let fact = frozen::read(store, id).unwrap().unwrap();
        assert!(fact.code);
        assert_eq!(fact.mode, Plan);
        assert_eq!(for_session(store, id, true).unwrap_err(), CHANGED);
        assert_eq!(
            initialize_new_session(store, id, false, Plan).unwrap_err(),
            CHANGED
        );
        std::fs::remove_dir(&blocked).unwrap();
        // Neither the original creator's retry nor a fresh instance may turn
        // an existing fact into permission to perform a missing mode write.
        assert_eq!(
            initialize_new_session(store, id, true, Plan).unwrap_err(),
            CHANGED
        );
        assert!(!blocked.exists());
        let reopened = SessionStore::boot().unwrap();
        assert_eq!(for_session(&reopened, id, true).unwrap_err(), CHANGED);
        assert_eq!(frozen::read(&reopened, id).unwrap(), Some(fact));
    });
}

#[test]
fn independent_store_instances_first_claim_different_lanes_have_one_winner() {
    use SerializableMode::Plan;
    with_session(|store, id, _| {
        let stores = [SessionStore::boot().unwrap(), SessionStore::boot().unwrap()];
        let barrier = std::sync::Barrier::new(2);
        let results = std::thread::scope(|scope| {
            let handles: Vec<_> = stores
                .iter()
                .enumerate()
                .map(|(index, instance)| {
                    let barrier = &barrier;
                    scope.spawn(move || {
                        barrier.wait();
                        (
                            index == 1,
                            initialize_new_session(instance, id, index == 1, Plan),
                        )
                    })
                })
                .collect();
            handles
                .into_iter()
                .map(|handle| handle.join().unwrap())
                .collect::<Vec<_>>()
        });
        assert_eq!(
            results.iter().filter(|(_, result)| result.is_ok()).count(),
            1
        );
        let winner = results.iter().find(|(_, result)| result.is_ok()).unwrap().0;
        assert_eq!(frozen::read(store, id).unwrap().unwrap().code, winner);
        let reopened = SessionStore::boot().unwrap();
        assert_eq!(for_session(&reopened, id, winner).unwrap(), Plan);
        assert_eq!(for_session(&reopened, id, !winner).unwrap_err(), CHANGED);
        assert!(
            !std::fs::read_dir(store.manager.sessions_dir().join(id))
                .unwrap()
                .any(|entry| entry
                    .unwrap()
                    .file_name()
                    .to_string_lossy()
                    .starts_with(".remote-permission-"))
        );
    });
}

#[test]
fn replacing_the_session_directory_or_deleting_the_session_rejects_old_authority() {
    use SerializableMode::Plan;
    with_session(|store, id, root| {
        initialize_new_session(store, id, false, Plan).unwrap();
        let original = root.join(id);
        let retired = root.join(format!("{id}-retired-fixture"));
        std::fs::rename(&original, &retired).unwrap();
        std::fs::create_dir(&original).unwrap();
        assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
        std::fs::remove_dir(&original).unwrap();
        std::fs::rename(&retired, &original).unwrap();
        assert_eq!(for_session(store, id, false).unwrap(), Plan);
        store.delete(id).unwrap();
        assert_eq!(for_session(store, id, false).unwrap_err(), CHANGED);
        assert_eq!(
            initialize_new_session(store, id, false, Plan).unwrap_err(),
            CHANGED
        );
        assert!(frozen::read(store, id).unwrap().is_none());
    });
}
