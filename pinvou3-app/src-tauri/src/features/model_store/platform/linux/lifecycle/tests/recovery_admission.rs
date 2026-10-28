//! Exercise ordinary downloads after a different card's process interrupted staging.
use super::*;
use crate::features::model_store::platform::linux::version_store;

const INTERRUPTED_CARD: &str = "interrupted-other-card";
const RESERVED_BYTES: u64 = 2 * 1024 * 1024 * 1024;

fn interrupted_candidate(root: &std::path::Path) -> (version_store::Version, std::path::PathBuf) {
    let revision = "e".repeat(64);
    let root = root
        .join(".versions")
        .join(INTERRUPTED_CARD)
        .join(&revision);
    fs::create_dir_all(&root).unwrap();
    let partial = root.join("weights.incomplete");
    fs::write(&partial, b"part").unwrap();
    let version = version_store::Version {
        card_id: INTERRUPTED_CARD.into(),
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

fn assert_recovered(version: &version_store::Version, partial: &std::path::Path) {
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
fn interrupted_quota_admission_recovers_other_card_before_ordinary_download() {
    let temp = setup_temp_db();
    let root = temp.path().join("models");
    let (interrupted, partial) = interrupted_candidate(&root);
    let mut settings = download_settings(&root);
    settings.storage_quota_gb = Some(1.0);
    let manager = manager_with_card(
        settings,
        local_script_card(&temp.path().join("scripts"), None),
    );

    // Invoke the production download entry directly; the abandoned card's
    // history has never been read and no recovery API is called by the test.
    manager
        .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
        .unwrap();

    assert_recovered(&interrupted, &partial);
    let downloaded = db::get_store_model("card-dl").unwrap().unwrap();
    assert_eq!(downloaded.status, "downloaded");
    assert_eq!(ran_times(&root.join("engine-installed")), 1);
    assert_eq!(ran_times(&root.join("download-ran")), 1);
    assert!(version_store::retained_bytes().unwrap() < 1024 * 1024);
}

#[test]
fn interrupted_quota_admission_keeps_other_live_card_reserved_during_ordinary_download() {
    let temp = setup_temp_db();
    let root = temp.path().join("models");
    let (interrupted, partial) = interrupted_candidate(&root);
    let lock_dir = config::data_dir().join("operation-locks");
    fs::create_dir_all(&lock_dir).unwrap();
    let operation = crate::platform::filesystem::open_private_lock_file(
        &lock_dir.join(format!("{INTERRUPTED_CARD}.lock")),
    )
    .unwrap();
    operation.try_lock().unwrap();
    let mut settings = download_settings(&root);
    settings.storage_quota_gb = Some(1.0);
    let manager = manager_with_card(
        settings,
        local_script_card(&temp.path().join("scripts"), None),
    );

    let error = manager
        .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
        .unwrap_err();
    assert!(error.to_string().contains("超出存储配额"), "{error}");
    assert!(db::get_store_model("card-dl").unwrap().is_none());
    assert_eq!(ran_times(&root.join("engine-installed")), 0);
    assert_eq!(ran_times(&root.join("download-ran")), 0);
    assert_eq!(
        serde_json::to_value(
            version_store::get(&interrupted.card_id, &interrupted.version_id)
                .unwrap()
                .unwrap()
        )
        .unwrap(),
        serde_json::to_value(&interrupted).unwrap()
    );
    assert_eq!(fs::read(&partial).unwrap(), b"part");

    drop(operation);
    manager
        .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
        .unwrap();
    assert_recovered(&interrupted, &partial);
    assert_eq!(ran_times(&root.join("engine-installed")), 1);
    assert_eq!(ran_times(&root.join("download-ran")), 1);
}
