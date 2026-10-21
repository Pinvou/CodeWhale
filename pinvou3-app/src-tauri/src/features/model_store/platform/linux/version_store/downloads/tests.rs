use super::*;
use std::fs;

fn setup() -> super::super::super::config::TempHome {
    let home = super::super::super::config::TempHome::new();
    db::init_db().unwrap();
    home
}

#[test]
fn ordinary_downloads_share_live_quota_and_failed_files_survive_plan_release() {
    let home = setup();
    let root = home.path().join("models");
    let first = DownloadReservation::acquire("first", &root, || false, || {}).unwrap();
    first.publish(80).unwrap();
    assert_eq!(super::super::retained_bytes().unwrap(), 80);
    let second = DownloadReservation::acquire("second", &root, || false, || {}).unwrap();
    second.publish(80).unwrap();
    assert_eq!(super::super::retained_bytes().unwrap(), 160);
    fs::write(root.join("partial"), vec![0; 10]).unwrap();
    drop(first);
    assert_eq!(super::super::retained_bytes().unwrap(), 90);
    drop(second);
    assert_eq!(super::super::retained_bytes().unwrap(), 10);
    assert!(root.join("partial").exists());
}

#[test]
fn ordinary_download_crash_recovery_releases_only_plan_and_keeps_registered_root() {
    let home = setup();
    let root = home.path().join("original");
    let mut interrupted = DownloadReservation::acquire("first", &root, || false, || {}).unwrap();
    interrupted.publish(80).unwrap();
    fs::write(root.join("partial"), vec![0; 10]).unwrap();
    // Releasing the lease without running the cleanup models process death.
    interrupted.finished = true;
    drop(interrupted);
    assert_eq!(super::super::retained_bytes().unwrap(), 10);
    assert!(!downloads().unwrap()[0].downloading);
    let moved_default = home.path().join("new-default");
    let retry = DownloadReservation::acquire("first", &moved_default, || false, || {}).unwrap();
    assert_eq!(retry.root, root);
    assert!(!moved_default.exists());
}

#[test]
fn ordinary_files_never_grant_cross_card_resume_credit_even_with_matching_names() {
    let home = setup();
    let root = home.path().join("models");
    let current = DownloadReservation::acquire("first", &root, || false, || {}).unwrap();
    fs::create_dir_all(root.join("A")).unwrap();
    fs::write(root.join("A/model.safetensors"), vec![0; 10]).unwrap();
    current.publish(10).unwrap();
    assert_eq!(super::super::retained_bytes().unwrap(), 20);
}

#[test]
fn ordinary_root_accounting_skips_only_registered_managed_generations() {
    let home = setup();
    let root = home.path().join("models");
    let download = DownloadReservation::acquire("first", &root, || false, || {}).unwrap();
    download.publish(10).unwrap();
    let isolated = root.join(".versions/other/revision");
    fs::create_dir_all(&isolated).unwrap();
    fs::write(isolated.join("partial"), vec![0; 20]).unwrap();
    super::super::put(&Version {
        card_id: "other".into(),
        version_id: "revision".into(),
        state: "failed".into(),
        record: None,
        models_root: Some(isolated.to_string_lossy().into_owned()),
        runtime_card_id: "other--revision".into(),
        resource_dir: Some(isolated.to_string_lossy().into_owned()),
        managed: true,
        created_at: "now".into(),
        error: None,
        planned_bytes: 0,
    })
    .unwrap();
    assert_eq!(super::super::retained_bytes().unwrap(), 30);
}

#[test]
fn completed_ordinary_root_keeps_extra_cache_charged_without_double_billing_record() {
    let home = setup();
    let root = home.path().join("models");
    let mut download = DownloadReservation::acquire("first", &root, || false, || {}).unwrap();
    download.publish(10).unwrap();
    db::add_store_model_with_root(
        db::AddStoreModelParams {
            card_id: "first",
            model_name: "first",
            card_name: "first",
            org: None,
            description: None,
            engine_name: "native",
            engine_version: None,
            size_bytes: 10,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: super::super::super::store_client::DeployTopology::Single,
            card_source: "store",
        },
        &root.to_string_lossy(),
    )
    .unwrap();
    fs::write(root.join("model"), vec![0; 10]).unwrap();
    fs::write(root.join("leftover-cache"), vec![0; 7]).unwrap();
    download.complete().unwrap();
    drop(download);
    assert_eq!(db::total_managed_bytes().unwrap(), 10);
    assert_eq!(super::super::retained_bytes().unwrap(), 7);
    assert!(!downloads().unwrap()[0].downloading);
    db::delete_store_model("first").unwrap();
    assert_eq!(super::super::retained_bytes().unwrap(), 17);
    assert!(root.join("model").exists());
}

#[test]
fn cancelling_ordinary_download_wait_does_not_replace_an_active_plan() {
    let home = setup();
    let root = home.path().join("models");
    let first = DownloadReservation::acquire("first", &root, || false, || {}).unwrap();
    first.publish(80).unwrap();
    let cancelled = DownloadReservation::acquire("first", &root, || true, || {});
    assert!(cancelled.err().unwrap().contains("已取消"));
    assert_eq!(super::super::retained_bytes().unwrap(), 80);
}

#[test]
fn declared_size_holes_cannot_absorb_another_cards_full_reservation() {
    let home = setup();
    let root = home.path().join("models");
    fs::create_dir_all(&root).unwrap();
    let register = |id, size| {
        db::add_store_model_with_root(
            db::AddStoreModelParams {
                card_id: id,
                model_name: id,
                card_name: id,
                org: None,
                description: None,
                engine_name: "native",
                engine_version: None,
                size_bytes: size,
                quantization: None,
                source_repo_id: None,
                source_filename: None,
                scripts: &BTreeMap::new(),
                status: "downloaded",
                profiles: &[],
                deploy_topology: super::super::super::store_client::DeployTopology::Single,
                card_source: "store",
            },
            &root.to_string_lossy(),
        )
        .unwrap()
    };
    register("A", 100);
    fs::write(root.join("A"), vec![0; 10]).unwrap();
    let mut downloading = DownloadReservation::acquire("B", &root, || false, || {}).unwrap();
    downloading.publish(8).unwrap();
    assert_eq!(db::total_managed_bytes().unwrap(), 100);
    assert_eq!(super::super::retained_bytes().unwrap(), 8);
    fs::write(root.join("B"), vec![0; 8]).unwrap();
    register("B", 8);
    downloading.complete().unwrap();
    assert_eq!(db::total_managed_bytes().unwrap(), 108);
    assert_eq!(super::super::retained_bytes().unwrap(), 0);
}

#[test]
fn first_download_resolves_user_selected_symlink_and_retries_pin_the_real_root() {
    use std::os::unix::fs::symlink;
    let home = setup();
    let real = home.path().join("actual");
    let selected = home.path().join("selected");
    fs::create_dir(&real).unwrap();
    symlink(&real, &selected).unwrap();
    let first = DownloadReservation::acquire("first", &selected, || false, || {}).unwrap();
    assert_eq!(first.root, real);
    first.publish(80).unwrap();
    drop(first);
    fs::remove_file(&selected).unwrap();
    let other = home.path().join("other");
    fs::create_dir(&other).unwrap();
    symlink(&other, &selected).unwrap();
    let retry = DownloadReservation::acquire("first", &selected, || false, || {}).unwrap();
    assert_eq!(retry.root, real);
    drop(retry);
    fs::remove_dir(&real).unwrap();
    symlink(&other, &real).unwrap();
    assert!(DownloadReservation::acquire("first", &selected, || false, || {}).is_err());
}
