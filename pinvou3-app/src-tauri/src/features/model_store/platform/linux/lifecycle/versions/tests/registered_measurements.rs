use super::*;

#[test]
fn failed_stage_invalidates_publish_size_without_masking_the_original_error() {
    use std::os::unix::fs::{MetadataExt, PermissionsExt};
    if fs::metadata("/proc/self").unwrap().uid() == 0 {
        return;
    }
    let (_home, manager, current, card, package, revision) = fixture();
    let root = manager
        .settings
        .models_path()
        .join(".versions")
        .join(&current.card_id)
        .join(&revision);
    let mut blocked = false;
    let result = manager.stage_verified(
        &current,
        &card,
        &revision,
        package,
        &mut |line| {
            if line.starts_with("独立校验新版权重") {
                fs::set_permissions(&root, fs::Permissions::from_mode(0o000)).unwrap();
                blocked = true;
            }
        },
        Default::default(),
    );
    fs::set_permissions(&root, fs::Permissions::from_mode(0o700)).unwrap();
    assert!(blocked);
    let error = result.unwrap_err().to_string();
    assert!(
        !error.contains("无法核算"),
        "terminal scan must not replace the original validation error"
    );
    let failed = version_store::get(&current.card_id, &revision)
        .unwrap()
        .unwrap();
    assert_eq!(failed.state, "failed");
    assert_eq!(failed.error.as_deref(), Some(error.as_str()));
    assert_eq!(failed.planned_bytes, 0);
    assert_eq!(failed.measured_bytes, None);
    assert!(version_store::retained_bytes().unwrap() >= 11);
}

#[test]
fn ordinary_current_remeasurement_survives_reopening_without_manufacturing_history() {
    let (_home, manager, current, _card, _package, _revision) = fixture();
    let root = manager.settings.models_path();
    let mut download =
        version_store::DownloadReservation::acquire(&current.card_id, &root, || false, || {})
            .unwrap();
    download.publish(11).unwrap();
    download.start_scripts().unwrap();
    db::connect()
        .unwrap()
        .execute(
            "UPDATE store_models SET models_root=?1 WHERE card_id=?2",
            rusqlite::params![root.to_string_lossy(), current.card_id],
        )
        .unwrap();
    download.complete().unwrap();
    drop(download);
    fs::write(root.join("legacy/extra-weight-cache"), vec![0; 4096]).unwrap();
    fs::write(root.join("unrelated-cache"), vec![0; 8192]).unwrap();
    let before = manager.versions(&current.card_id, false).unwrap();
    assert_eq!(before[0]["size_bytes"], 11);
    let measured = manager.versions(&current.card_id, true).unwrap();
    assert_eq!(measured[0]["size_bytes"], 11 + 4096);
    let reopened = manager.versions(&current.card_id, false).unwrap();
    assert_eq!(reopened[0]["size_bytes"], measured[0]["size_bytes"]);
    assert_eq!(version_store::retained_bytes().unwrap(), 4096 + 8192);
    assert!(
        version_store::list(Some(&current.card_id))
            .unwrap()
            .is_empty()
    );
    assert!(!version_store::has_history(&current.card_id).unwrap());
}

#[test]
fn explicit_remeasurement_reports_unreadable_resource_without_publishing_partial_success() {
    use std::os::unix::fs::{MetadataExt, PermissionsExt};
    if fs::metadata("/proc/self").unwrap().uid() == 0 {
        return;
    }
    let (_home, manager, current, _card, _package, _revision) = fixture();
    let before = manager.versions(&current.card_id, true).unwrap();
    let resource = manager.settings.models_path().join("legacy");
    fs::set_permissions(&resource, fs::Permissions::from_mode(0o000)).unwrap();
    let failed = manager.versions(&current.card_id, true);
    fs::set_permissions(&resource, fs::Permissions::from_mode(0o700)).unwrap();
    assert!(failed.is_err());
    assert_eq!(manager.versions(&current.card_id, false).unwrap(), before);
}
