use super::*;
use std::fs;
use std::os::unix::fs::{MetadataExt, PermissionsExt};

fn ordinary() -> (
    super::super::super::config::TempHome,
    db::StoreModelRecord,
    Version,
) {
    let home = super::super::super::config::TempHome::new();
    db::init_db().unwrap();
    let root = home.path().join("models");
    let resource = root.join("ordinary");
    let mut reservation = DownloadReservation::acquire("ordinary", &root, || false, || {}).unwrap();
    reservation.publish(11).unwrap();
    reservation.start_scripts().unwrap();
    fs::create_dir(&resource).unwrap();
    fs::write(resource.join("weight"), b"old-weights").unwrap();
    fs::write(root.join("unrelated-cache"), vec![0; 19]).unwrap();
    db::add_store_model_with_root(
        db::AddStoreModelParams {
            card_id: "ordinary",
            model_name: "ordinary",
            card_name: "ordinary",
            org: None,
            description: None,
            engine_name: "native",
            engine_version: None,
            size_bytes: 11,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &Default::default(),
            status: "deployed",
            profiles: &[],
            deploy_topology: super::super::super::store_client::DeployTopology::Single,
            card_source: "store",
        },
        &root.to_string_lossy(),
    )
    .unwrap();
    reservation.complete().unwrap();
    drop(reservation);
    let record = db::get_store_model("ordinary").unwrap().unwrap();
    let current = Version {
        card_id: record.card_id.clone(),
        version_id: active_id(&record),
        state: "current".into(),
        record: Some(record.clone()),
        models_root: record.models_root.clone(),
        runtime_card_id: record.card_id.clone(),
        resource_dir: Some(resource.to_string_lossy().into_owned()),
        managed: false,
        created_at: record.downloaded_at.clone(),
        error: None,
        planned_bytes: 0,
        measured_bytes: None,
    };
    (home, record, current)
}

fn shared_root_measurement(root: &str) -> i64 {
    connection()
        .unwrap()
        .query_row(
            "SELECT measured_bytes FROM model_download_root_measurements WHERE models_root=?1",
            [root],
            |row| row.get(0),
        )
        .unwrap()
}

#[test]
fn ordinary_current_measurement_is_bounded_keyed_and_does_not_create_history() {
    let (_home, record, current) = ordinary();
    let mut versions = vec![current.clone()];
    let _guard = lock(&record.card_id).unwrap();
    remeasure(&record, &mut versions).unwrap();
    let resource = current.resource_dir.as_deref().unwrap();
    assert_eq!(
        get(&record.card_id, &current.version_id, resource).unwrap(),
        Some(11)
    );
    assert!(
        get(&record.card_id, "different-revision", resource)
            .unwrap()
            .is_none()
    );
    assert!(
        get(&record.card_id, &current.version_id, "/different-resource")
            .unwrap()
            .is_none()
    );
    assert!(list(Some(&record.card_id)).unwrap().is_empty());
    assert!(!has_history(&record.card_id).unwrap());
    remeasure(&record, &mut versions).unwrap();
    let count: i64 = connection()
        .unwrap()
        .query_row(
            "SELECT COUNT(*) FROM model_current_version_measurements WHERE card_id=?1",
            [&record.card_id],
            |row| row.get(0),
        )
        .unwrap();
    assert_eq!(count, 1);
}

#[test]
fn remeasure_refreshes_complete_shared_root_separately_from_resource_display() {
    let (_home, record, current) = ordinary();
    let root = record.models_root.as_deref().unwrap();
    fs::write(Path::new(root).join("new-unrelated-cache"), vec![0; 4096]).unwrap();
    let mut versions = vec![current];
    let _guard = lock(&record.card_id).unwrap();
    remeasure(&record, &mut versions).unwrap();
    assert_eq!(versions[0].measured_bytes, Some(11));
    assert_eq!(shared_root_measurement(root), 11 + 19 + 4096);
    assert_eq!(retained_bytes().unwrap(), 19 + 4096);
}

#[test]
fn failed_display_write_rolls_back_shared_root_refresh_and_preserves_returned_sizes() {
    let (_home, record, current) = ordinary();
    let root = record.models_root.as_deref().unwrap();
    fs::write(Path::new(root).join("new-cache"), vec![0; 4096]).unwrap();
    let _guard = lock(&record.card_id).unwrap();
    connection().unwrap().execute_batch(
        "CREATE TRIGGER reject_current_measurement BEFORE INSERT ON model_current_version_measurements
         BEGIN SELECT RAISE(ABORT,'injected measurement failure'); END;"
    ).unwrap();
    let mut versions = vec![current];
    let error = remeasure(&record, &mut versions).unwrap_err();
    assert!(error.contains("injected measurement failure"));
    assert_eq!(versions[0].measured_bytes, None);
    assert_eq!(shared_root_measurement(root), 30);
    assert!(
        get(
            &record.card_id,
            &versions[0].version_id,
            versions[0].resource_dir.as_deref().unwrap()
        )
        .unwrap()
        .is_none()
    );
}

#[test]
fn terminal_measurement_failure_invalidates_a_previously_registered_value() {
    let (_home, record, mut version) = ordinary();
    if fs::metadata("/proc/self").unwrap().uid() == 0 {
        return;
    }
    let root = Path::new(version.models_root.as_deref().unwrap()).to_path_buf();
    version.measured_bytes = Some(0);
    version.state = "failed".into();
    version.managed = true;
    version.record = None;
    version.resource_dir = version.models_root.clone();
    version.planned_bytes = 0;
    fs::set_permissions(&root, fs::Permissions::from_mode(0o000)).unwrap();
    refresh_measured(&mut version);
    put(&version).unwrap();
    let blocked = retained_bytes();
    fs::set_permissions(&root, fs::Permissions::from_mode(0o700)).unwrap();
    assert_eq!(version.measured_bytes, None);
    assert!(blocked.is_err());
    assert!(retained_bytes().unwrap() >= 30);
    assert!(
        super::super::get(&record.card_id, &version.version_id)
            .unwrap()
            .unwrap()
            .measured_bytes
            .is_some()
    );
}

#[test]
fn ordinary_retry_invalidates_same_revision_and_resource_measurement() {
    let (_home, record, current) = ordinary();
    let resource = current.resource_dir.as_deref().unwrap().to_string();
    {
        let _guard = lock(&record.card_id).unwrap();
        remeasure(&record, &mut [current.clone()]).unwrap();
    }
    assert!(
        get(&record.card_id, &current.version_id, &resource)
            .unwrap()
            .is_some()
    );
    let retry = DownloadReservation::acquire(
        &record.card_id,
        Path::new(record.models_root.as_deref().unwrap()),
        || false,
        || {},
    )
    .unwrap();
    retry.publish(11).unwrap();
    assert!(
        get(&record.card_id, &current.version_id, &resource)
            .unwrap()
            .is_none()
    );
}

#[test]
fn explicit_measurement_refuses_a_busy_resource_instead_of_waiting_without_cancellation() {
    let (_home, record, _current) = ordinary();
    let _resources = resource_operation_lock(false).unwrap();
    assert_eq!(
        lock(&record.card_id).err().unwrap(),
        "version_measurement_busy"
    );
}
