//! Quota admission must discover interrupted work without opening its history.
use super::*;

const INTERRUPTED_CARD: &str = "interrupted-other-card";
const RESERVED_BYTES: u64 = 2 * 1024 * 1024 * 1024;

fn interrupted_candidate(root: &Path, card_id: &str) -> (Version, PathBuf) {
    let revision = "e".repeat(64);
    // Interrupted production staging has already created private owned
    // ancestors. A create_dir_all fixture would instead inherit a group-write
    // umask and correctly fail the next stage's ownership guard.
    let root = owned_directory(root, &[".versions", card_id, &revision]).unwrap();
    let partial = root.join("weights.incomplete");
    fs::write(&partial, b"part").unwrap();
    let version = Version {
        measured_bytes: None,
        card_id: card_id.into(),
        version_id: revision,
        state: "downloading".into(),
        record: None,
        models_root: Some(root.to_string_lossy().into_owned()),
        runtime_card_id: "interrupted-other-runtime".into(),
        resource_dir: Some(root.to_string_lossy().into_owned()),
        managed: true,
        created_at: "2026-10-28T00:00:00Z".into(),
        error: None,
        planned_bytes: RESERVED_BYTES,
    };
    version_store::put(&version).unwrap();
    assert_eq!(version_store::retained_bytes().unwrap(), RESERVED_BYTES);
    (version, partial)
}

fn assert_recovered(version: &Version, partial: &Path) {
    let recovered = version_store::get(&version.card_id, &version.version_id)
        .unwrap()
        .unwrap();
    assert_eq!(recovered.state, "failed");
    assert_eq!(recovered.planned_bytes, 0);
    assert_eq!(
        recovered.error.as_deref(),
        Some("version_download_interrupted")
    );
    assert_eq!(recovered.models_root, version.models_root);
    assert_eq!(recovered.resource_dir, version.resource_dir);
    assert_eq!(fs::read(partial).unwrap(), b"part");
}

#[test]
fn interrupted_quota_admission_recovers_other_card_before_staging() {
    let (_temp, mut manager, current, card, package, revision) = fixture();
    let (interrupted, partial) =
        interrupted_candidate(&manager.settings.models_path(), INTERRUPTED_CARD);
    manager.settings.storage_quota_gb = Some(1.0);
    let current_before = serde_json::to_value(&current).unwrap();

    // No versions/history endpoint for INTERRUPTED_CARD is called. Starting B
    // directly after a restart must reclaim A's plan and keep A's actual bytes.
    manager
        .stage_verified(
            &current,
            &card,
            &revision,
            package,
            &mut |_| {},
            Default::default(),
        )
        .unwrap();

    assert_recovered(&interrupted, &partial);
    let staged = version_store::get(&current.card_id, &revision)
        .unwrap()
        .unwrap();
    assert_eq!(staged.state, "ready");
    assert_eq!(staged.planned_bytes, 0);
    assert_eq!(
        fs::read(Path::new(staged.models_root.as_deref().unwrap()).join("new/new.tachyon"))
            .unwrap(),
        b"new-weights"
    );
    assert_eq!(
        serde_json::to_value(db::get_store_model(&current.card_id).unwrap().unwrap()).unwrap(),
        current_before
    );
    assert!(version_store::retained_bytes().unwrap() < 1024 * 1024);
}

#[test]
fn interrupted_quota_admission_keeps_other_live_card_reserved_during_staging() {
    let (_temp, mut manager, current, card, package, revision) = fixture();
    let (interrupted, partial) =
        interrupted_candidate(&manager.settings.models_path(), INTERRUPTED_CARD);
    let lock_dir = config::data_dir().join("operation-locks");
    fs::create_dir_all(&lock_dir).unwrap();
    let operation = crate::platform::filesystem::open_private_lock_file(
        &lock_dir.join(format!("{INTERRUPTED_CARD}.lock")),
    )
    .unwrap();
    operation.try_lock().unwrap();
    manager.settings.storage_quota_gb = Some(1.0);

    let error = manager
        .stage_verified(
            &current,
            &card,
            &revision,
            package.clone(),
            &mut |_| {},
            Default::default(),
        )
        .unwrap_err();
    assert!(error.to_string().contains("超出存储配额"), "{error}");
    assert!(
        version_store::get(&current.card_id, &revision)
            .unwrap()
            .is_none()
    );
    assert_eq!(
        serde_json::to_value(
            version_store::get(&interrupted.card_id, &interrupted.version_id)
                .unwrap()
                .unwrap()
        )
        .unwrap(),
        serde_json::to_value(&interrupted).unwrap()
    );
    assert_eq!(version_store::retained_bytes().unwrap(), RESERVED_BYTES);
    assert_eq!(fs::read(&partial).unwrap(), b"part");

    // Releasing the real per-card lock is the only changed input. The next
    // admission must now recover it without a visit to the interrupted card.
    drop(operation);
    manager
        .stage_verified(
            &current,
            &card,
            &revision,
            package,
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
    assert_recovered(&interrupted, &partial);
}

#[test]
fn interrupted_quota_admission_recovers_old_revision_of_the_same_card_while_owned() {
    let (_temp, mut manager, current, card, package, revision) = fixture();
    let (interrupted, partial) =
        interrupted_candidate(&manager.settings.models_path(), &current.card_id);
    assert_ne!(interrupted.version_id, revision);
    manager.settings.storage_quota_gb = Some(1.0);
    let lock_dir = config::data_dir().join("operation-locks");
    fs::create_dir_all(&lock_dir).unwrap();
    let operation = crate::platform::filesystem::open_private_lock_file(
        &lock_dir.join(format!("{}.lock", current.card_id)),
    )
    .unwrap();
    operation.try_lock().unwrap();

    // Mirror the production service's already-owned lock context, so the
    // generic quota sweep cannot and must not try-lock this same card again.
    version_store::recover_interrupted_while_locked(&current.card_id, &operation).unwrap();
    manager
        .stage_verified(
            &current,
            &card,
            &revision,
            package,
            &mut |_| {},
            Default::default(),
        )
        .unwrap();

    assert_recovered(&interrupted, &partial);
    let new = version_store::get(&current.card_id, &revision)
        .unwrap()
        .unwrap();
    assert_eq!(new.state, "ready");
    assert_eq!(new.planned_bytes, 0);
    assert_eq!(
        fs::read(Path::new(new.models_root.as_deref().unwrap()).join("new/new.tachyon")).unwrap(),
        b"new-weights"
    );
    assert_eq!(
        serde_json::to_value(db::get_store_model(&current.card_id).unwrap().unwrap()).unwrap(),
        serde_json::to_value(&current).unwrap()
    );
}

#[test]
fn interrupted_quota_admission_recovers_same_card_ordinary_plan_before_staging() {
    let (_home, mut manager, current, card, package, revision) = fixture();
    let root = manager.settings.models_path();
    let old = version_store::DownloadReservation::acquire(&current.card_id, &root, || false, || {})
        .unwrap();
    old.publish(RESERVED_BYTES).unwrap();
    old.start_scripts().unwrap();
    let partial = root.join("ordinary.incomplete");
    fs::write(&partial, b"part").unwrap();
    drop(old);
    // Recreate the durable crash state after the process lost its lease: the
    // installed model transaction committed, but ordinary completion did not.
    db::connect()
        .unwrap()
        .execute(
            "UPDATE model_download_reservations SET downloading=1 WHERE card_id=?1",
            [&current.card_id],
        )
        .unwrap();
    let lock_dir = config::data_dir().join("operation-locks");
    fs::create_dir_all(&lock_dir).unwrap();
    let operation = crate::platform::filesystem::open_private_lock_file(
        &lock_dir.join(format!("{}.lock", current.card_id)),
    )
    .unwrap();
    operation.try_lock().unwrap();
    version_store::recover_interrupted_while_locked(&current.card_id, &operation).unwrap();
    let pending: bool = db::connect()
        .unwrap()
        .query_row(
            "SELECT downloading FROM model_download_reservations WHERE card_id=?1",
            [&current.card_id],
            |row| row.get(0),
        )
        .unwrap();
    assert!(
        !pending,
        "service takeover must retire the old ordinary plan first"
    );
    manager.settings.storage_quota_gb = Some(1.0);
    manager
        .stage_verified(
            &current,
            &card,
            &revision,
            package,
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
    assert_eq!(
        version_store::get(&current.card_id, &revision)
            .unwrap()
            .unwrap()
            .state,
        "ready"
    );
    assert_eq!(fs::read(partial).unwrap(), b"part");
    assert_eq!(
        serde_json::to_value(db::get_store_model(&current.card_id).unwrap().unwrap()).unwrap(),
        serde_json::to_value(&current).unwrap()
    );
    assert!(version_store::retained_bytes().unwrap() < 1024 * 1024);
}
