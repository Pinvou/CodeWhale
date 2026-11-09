//! Taking the service card lock must recover older interrupted revisions first.
use super::*;
use crate::features::model_store::platform::linux::version_store::{self, Version};
use std::collections::BTreeMap;
use std::fs;
use std::path::Path;

const CARD: &str = "service-recovery-card";

fn add_record() -> db::StoreModelRecord {
    db::add_store_model(db::AddStoreModelParams {
        card_id: CARD,
        model_name: "preserved-deployment",
        card_name: "Preserved deployment",
        org: None,
        description: None,
        engine_name: "native",
        engine_version: None,
        size_bytes: 11,
        quantization: None,
        source_repo_id: None,
        source_filename: None,
        scripts: &BTreeMap::new(),
        status: "deployed",
        profiles: &[],
        deploy_topology: super::super::super::store_client::DeployTopology::Single,
        card_source: "store",
    })
    .unwrap();
    db::add_api_key(CARD, Some("preserve service recovery credential")).unwrap();
    db::get_store_model(CARD).unwrap().unwrap()
}

fn interrupted(root: &Path, revision: char) -> Version {
            measured_bytes: None,
    let root = root.join(revision.to_string());
    fs::create_dir_all(&root).unwrap();
    fs::write(root.join("weights.incomplete"), b"old partial weights").unwrap();
    let version = Version {
            measured_bytes: None,
        card_id: CARD.into(),
        version_id: revision.to_string().repeat(64),
        state: "downloading".into(),
        record: None,
        models_root: Some(root.to_string_lossy().into_owned()),
        runtime_card_id: format!("service-recovery-runtime-{revision}"),
        resource_dir: Some(root.to_string_lossy().into_owned()),
        managed: true,
        created_at: "2026-10-28T00:00:00Z".into(),
        error: None,
        planned_bytes: 2 * 1024 * 1024 * 1024,
    };
    version_store::put(&version).unwrap();
    version
}

fn assert_recovered(before: &Version) {
    let after = version_store::get(&before.card_id, &before.version_id)
        .unwrap()
        .unwrap();
    assert_eq!(after.state, "failed");
    assert_eq!(after.planned_bytes, 0);
    assert_eq!(after.error.as_deref(), Some("version_download_interrupted"));
    assert_eq!(after.models_root, before.models_root);
    assert_eq!(after.resource_dir, before.resource_dir);
    assert_eq!(after.created_at, before.created_at);
    assert_eq!(
        fs::read(Path::new(before.models_root.as_deref().unwrap()).join("weights.incomplete"))
            .unwrap(),
        b"old partial weights"
    );
}

#[test]
fn card_lock_recovers_old_revision_before_starting_a_different_revision() {
    let home = config::TempHome::new();
    db::init_db().unwrap();
    let record = add_record();
    let keys = serde_json::to_value(db::list_api_keys_for_card(CARD).unwrap()).unwrap();
    let old = interrupted(&home.path().join("versions"), 'a');
    let operation = card_operation_lock(CARD).unwrap();
    // A generic try-lock recovery after acquiring this lock would skip itself;
    // the production service must use its owned-lock recovery path instead.
    assert_recovered(&old);
    assert_eq!(
        serde_json::to_value(db::get_store_model(CARD).unwrap().unwrap()).unwrap(),
        serde_json::to_value(record).unwrap()
    );
    assert_eq!(
        serde_json::to_value(db::list_api_keys_for_card(CARD).unwrap()).unwrap(),
        keys
    );

    let new = interrupted(&home.path().join("versions"), 'b');
    let new_before = serde_json::to_value(&new).unwrap();
    {
        let _quota = version_store::wait_quota_reservation_lock(|| false, || {}).unwrap();
        version_store::recover_interrupted_for_quota().unwrap();
    }
    assert_eq!(
        serde_json::to_value(version_store::get(CARD, &new.version_id).unwrap().unwrap()).unwrap(),
        new_before,
        "the newly acquired card lease must keep its next revision reserved"
    );
    drop(operation);
    let _next_operation = card_operation_lock(CARD).unwrap();
    assert_recovered(&new);
}

#[test]
fn rejected_card_lock_never_recovers_a_live_generation() {
    let home = config::TempHome::new();
    db::init_db().unwrap();
    let path = config::data_dir()
        .join("operation-locks")
        .join(format!("{CARD}.lock"));
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    let active = crate::platform::filesystem::open_private_lock_file(&path).unwrap();
    active.try_lock().unwrap();
    let downloading = interrupted(&home.path().join("versions"), 'a');
    let before = serde_json::to_value(&downloading).unwrap();

    assert!(card_operation_lock(CARD).unwrap_err().contains("其他窗口"));
    assert_eq!(
        serde_json::to_value(
            version_store::get(CARD, &downloading.version_id)
                .unwrap()
                .unwrap()
        )
        .unwrap(),
        before
    );
    assert_eq!(
        fs::read(Path::new(downloading.models_root.as_deref().unwrap()).join("weights.incomplete"))
            .unwrap(),
        b"old partial weights"
    );
    drop(active);
    let _operation = card_operation_lock(CARD).unwrap();
    assert_recovered(&downloading);
}
