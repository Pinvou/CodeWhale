use super::*;
use crate::docker::fixtures as fx;
use std::os::unix::fs::PermissionsExt;

fn declaration() -> serde_json::Value {
    serde_json::json!({"selection_policy":"latest-installed-v1",
        "selections":[{"logical_card":"model","card":"model--new","version_id":"new-revision"}],
        "archived_models":[{"logical_card":"model","card":"model","version_id":"old-revision"},
            {"logical_card":"model","card":"model--old","version_id":"older-revision"}],
        "models":[{"logical_card":"model","card":"model--new","version_id":"new-revision",
            "deployment_revision":"current-profile"}]})
}

#[test]
fn logical_identity_selects_only_current_and_history_is_not_a_candidate() {
    let value = parse(&declaration()).unwrap();
    assert_eq!(
        value.resolve("model").unwrap(),
        ("model--new".into(), Some("model".into()))
    );
    assert_eq!(value.resolve("model--new").unwrap().0, "model--new");
    assert!(!value.visible("model"));
    assert!(!value.visible("model--old"));
    assert!(value
        .resolve("model--old")
        .unwrap_err()
        .contains("archived_model_version"));
    assert!(value.visible("unrelated-experiment"));
    assert_eq!(
        value.metadata("model--new").unwrap()["deployment_revision"],
        "current-profile"
    );
    let mut broken = declaration();
    broken["models"] = serde_json::json!([]);
    let broken = parse(&broken).unwrap();
    assert_eq!(broken.resolve("model").unwrap().0, "model--new");
    assert!(
        !broken.routable("model--new"),
        "missing latest metadata never reopens the old directory"
    );
}

#[test]
fn malformed_ambiguous_or_legacy_inventory_never_selects_a_directory() {
    let mut value = declaration();
    value["selection_policy"] = serde_json::Value::Null;
    assert!(parse(&value).unwrap_err().contains("upgrade_required"));
    let mut value = declaration();
    let duplicate = value["selections"][0].clone();
    value["selections"].as_array_mut().unwrap().push(duplicate);
    assert!(parse(&value).is_err());
    let mut value = declaration();
    value["archived_models"][0]["card"] = "model--new".into();
    assert!(parse(&value).is_err());
    let mut value = declaration();
    value["selections"][0]["card"] = "../old".into();
    assert!(parse(&value).is_err());
    let mut value = declaration();
    value["models"][0]["card"] = "model".into();
    assert!(parse(&value).is_err());
    let mut value = declaration();
    value["models"][0]["version_id"] = "wrong-revision".into();
    assert!(parse(&value).is_err());
}

#[test]
fn retained_weights_without_current_deployment_cannot_be_started() {
    let mut value = declaration();
    value["selections"] = serde_json::json!([]);
    value["models"] = serde_json::json!([]);
    let snapshot = parse(&value).unwrap();
    assert!(snapshot
        .resolve("model")
        .unwrap_err()
        .contains("current_not_deployed"));
    assert!(!snapshot.visible("model"));
}

#[test]
fn shared_admission_blocks_publication_and_cached_archives_survive_restart() {
    let root = fx::tmpdir("current-selection-lock");
    let profile = root.join("profile");
    std::fs::create_dir_all(profile.join("model-store")).unwrap();
    let shared = resource_lock(&profile).unwrap();
    let writer = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(profile.join("model-store/version-operations.lock"))
        .unwrap();
    assert!(writer.try_lock().is_err());
    drop(shared);
    writer.try_lock().unwrap();
    assert!(resource_lock(&profile).is_err());
    drop(writer);
    let app = fx::make_app(&root, 128, 100);
    publish(&app, &declaration()).unwrap();
    let revision = app.store.lock().unwrap().state.lifecycle_revision;
    publish(&app, &declaration()).unwrap();
    assert_eq!(app.store.lock().unwrap().state.lifecycle_revision, revision);
    assert!(!cached(&app).unwrap().visible("model"));
    let empty = serde_json::json!({"selection_policy":"latest-installed-v1","selections":[],"models":[],"archived_models":[]});
    publish(&app, &empty).unwrap();
    let tombstones = cached(&app).unwrap();
    assert!(
        !tombstones.visible("model--new"),
        "history cleanup cannot resurrect an old current directory"
    );
    assert!(!tombstones.visible("model--old"));
    assert!(tombstones.resolve("model").is_err());
    assert!(
        admit(&app, "model").is_err(),
        "a missing live reader never authorizes a cached launch"
    );
    std::fs::write(app.config.state_dir.join("store-selection.json"), b"broken").unwrap();
    assert!(cached(&app).is_err());
    std::fs::remove_file(app.config.state_dir.join("store-selection.json")).unwrap();
    std::os::unix::fs::symlink("missing", app.config.state_dir.join("store-selection.json"))
        .unwrap();
    assert!(
        cached(&app).is_err(),
        "a dangling link is not proof that selection was never configured"
    );
}

#[test]
fn actual_reader_contract_pins_the_task_and_rejects_file_drift() {
    let root = fx::tmpdir("current-selection-reader");
    let mut app = fx::make_app(&root, 128, 100);
    fx::write_docker_card(
        &app.config.cards_dir,
        "model--new",
        "qa-current",
        18198,
        Some("0.01"),
    );
    let card = app.config.cards_dir.join("model--new");
    let mut files = serde_json::Map::new();
    for name in ["common.sh", "vendor/.deploy-env"] {
        let bytes = std::fs::read(card.join(name)).unwrap();
        files.insert(
            name.into(),
            hex(ring::digest::digest(&ring::digest::SHA256, &bytes).as_ref()).into(),
        );
    }
    let mut value = declaration();
    value["format"] = "pinvou-installed-models-v1".into();
    value["cards_root"] = app.config.cards_dir.to_string_lossy().as_ref().into();
    value["skipped"] = serde_json::json!([]);
    value["models"][0] = serde_json::json!({"card":"model--new","logical_card":"model","version_id":"new-revision",
        "origin":"store","store_status":"deployed","downloaded_at":"today","files":files,
        "deployment_revision":"current-profile"});
    let program = root.join("reader.py");
    std::fs::write(
        &program,
        format!("#!/usr/bin/python3\nprint({:?})\n", value.to_string()),
    )
    .unwrap();
    std::fs::set_permissions(&program, std::fs::Permissions::from_mode(0o700)).unwrap();
    let profile = root.join("profile");
    std::fs::create_dir_all(profile.join("model-store")).unwrap();
    let cfg = &mut Arc::get_mut(&mut app).unwrap().config;
    cfg.store_inventory_program = Some(program);
    cfg.store_inventory_profile = Some(profile);
    let guard = admit(&app, "model").unwrap();
    assert_eq!(guard.card, "model--new");
    let task = app.task_create_exclusive("enable", &guard.card).unwrap();
    guard.pin_task(&app, &task).unwrap();
    let record = app.store.lock().unwrap().state.tasks[&task].clone();
    assert_eq!(
        record
            .selection
            .as_ref()
            .unwrap()
            .get("selected_version")
            .and_then(Value::as_str),
        Some("new-revision")
    );
    let restored = crate::store::StateStore::load(&app.config.state_file());
    assert_eq!(restored.state.tasks[&task].selection, record.selection);
    std::fs::write(card.join("common.sh"), "modified").unwrap();
    assert!(guard.verify(&app).is_err());
}

#[test]
fn install_delegation_never_replays_a_known_historical_local_package() {
    let root = fx::tmpdir("current-install-route");
    let app = fx::make_app(&root, 128, 100);
    publish(&app, &declaration()).unwrap();
    assert_eq!(
        installation_route(&app, "model", true).unwrap(),
        ("model".into(), false)
    );
    assert_eq!(
        installation_route(&app, "model--new", true).unwrap(),
        ("model".into(), false)
    );
    assert!(installation_route(&app, "model--old", true).is_err());
    assert_eq!(
        installation_route(&app, "new-experiment", true).unwrap(),
        ("new-experiment".into(), true)
    );
}

#[test]
fn explicit_current_stop_clears_logical_boot_but_archive_migration_preserves_it() {
    let root = fx::tmpdir("current-boot-stop");
    let app = fx::make_app(&root, 128, 100);
    publish(&app, &declaration()).unwrap();
    save_boot(&app, "model", true).unwrap();
    disarm_current_boot(&app, "model").unwrap();
    assert_eq!(
        boot_preference(&app, "model").unwrap(),
        Some(true),
        "retiring an old physical alias must preserve its replacement's preference"
    );
    disarm_current_boot(&app, "model--new").unwrap();
    assert_eq!(boot_preference(&app, "model").unwrap(), Some(false));
    fence_archived(&app, &cached(&app).unwrap()).unwrap();
    assert_eq!(
        boot_preference(&app, "model").unwrap(),
        Some(false),
        "periodic reconciliation must not re-enable a stopped logical model"
    );
    restore_current_boot(&app, "model--new", true).unwrap();
    assert_eq!(
        boot_preference(&app, "model").unwrap(),
        Some(true),
        "failed cross-model swaps can restore the previous current model's intent"
    );
}

#[test]
fn explicit_store_reselection_replaces_archive_tombstones_in_both_directions() {
    let root = fx::tmpdir("current-selection-rollback");
    let app = fx::make_app(&root, 128, 100);
    publish(&app, &declaration()).unwrap();
    let mut rollback = declaration();
    rollback["selections"][0]["card"] = "model--old".into();
    rollback["selections"][0]["version_id"] = "older-revision".into();
    rollback["models"][0]["card"] = "model--old".into();
    rollback["models"][0]["version_id"] = "older-revision".into();
    rollback["models"][0]["deployment_revision"] = "restored-profile".into();
    // Even when history rows were cleaned, only the explicit current pointer
    // removes its own tombstone; unrelated retired directories stay fenced.
    rollback["archived_models"] = serde_json::json!([]);
    let restored = publish(&app, &rollback).unwrap();
    assert_eq!(restored.resolve("model").unwrap().0, "model--old");
    assert!(restored.routable("model--old"));
    assert!(!restored.visible("model--new"));
    assert!(!restored.visible("model"));
    assert_eq!(
        restored.metadata("model--old").unwrap()["deployment_revision"],
        "restored-profile"
    );
    assert!(cached(&app).unwrap().routable("model--old"));
    let upgraded = publish(&app, &declaration()).unwrap();
    assert!(upgraded.routable("model--new"));
    assert!(!upgraded.visible("model--old"));
}

#[test]
fn unknown_logical_boot_receipts_are_not_empty_preferences_or_rewritten() {
    for kind in ["dangling", "fifo", "writable"] {
        let root = fx::tmpdir(&format!("logical-boot-control-{kind}"));
        let app = fx::make_app(&root, 128, 100);
        crate::private_storage::secure_state(&app.config.state_dir).unwrap();
        let path = app.config.state_dir.join("logical-model-boot.json");
        assert_eq!(boot_preference(&app, "model").unwrap(), None);
        match kind {
            "dangling" => std::os::unix::fs::symlink(root.join("missing"), &path).unwrap(),
            "fifo" => {
                use std::os::unix::ffi::OsStrExt;
                let name = std::ffi::CString::new(path.as_os_str().as_bytes()).unwrap();
                assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
            }
            _ => {
                std::fs::write(&path, b"{\"model\":true}").unwrap();
                std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o666)).unwrap();
            }
        }
        assert!(boot_preference(&app, "model").is_err(), "{kind}");
        assert!(save_boot(&app, "model", false).is_err(), "{kind}");
        assert!(std::fs::symlink_metadata(path).is_ok());
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[test]
fn cleanup_current_selection_is_visible_but_never_routable_or_historical() {
    let mut value = declaration();
    value["models"] = serde_json::json!([]);
    value["skipped"] = serde_json::json!([{"card":"model","reason":"current_not_deployed"}]);
    let snapshot = parse(&value).unwrap();
    assert!(snapshot.visible("model--new"));
    assert!(!snapshot.routable("model--new"));
    assert!(snapshot.maintenance_target("model--new").unwrap());
    assert!(snapshot.maintenance_target("model--old").is_err());
    assert!(snapshot.maintenance_target("model").is_err());
    assert!(!snapshot.maintenance_target("standalone").unwrap());
    assert!(parse(&snapshot.raw)
        .unwrap()
        .maintenance_target("model--new")
        .unwrap());
    for reason in [
        "store_deletion_incomplete",
        "store_inventory_current_version_mismatch",
        "store_inventory_invalid_status",
        "store_driver_metadata_missing",
    ] {
        value["skipped"][0]["reason"] = reason.into();
        assert!(
            parse(&value)
                .unwrap()
                .maintenance_target("model--new")
                .is_err(),
            "{reason}"
        );
    }
    value.as_object_mut().unwrap().remove("skipped");
    assert!(parse(&value)
        .unwrap()
        .maintenance_target("model--new")
        .is_err());
}
