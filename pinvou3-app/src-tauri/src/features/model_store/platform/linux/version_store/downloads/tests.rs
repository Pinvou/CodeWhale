use super::*;
use std::fs;

fn setup() -> super::super::super::config::TempHome {
    let home = super::super::super::config::TempHome::new();
    db::init_db().unwrap();
    home
}

fn complete_download(card_id: &str, root: &Path, size: u64) {
    let mut download = DownloadReservation::acquire(card_id, root, || false, || {}).unwrap();
    download.publish(size).unwrap();
    fs::write(
        root.join(format!("{card_id}-weights")),
        vec![0; size as usize],
    )
    .unwrap();
    db::add_store_model_with_root(
        db::AddStoreModelParams {
            card_id,
            model_name: card_id,
            card_name: card_id,
            org: None,
            description: None,
            engine_name: "native",
            engine_version: None,
            size_bytes: size as i64,
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
    download.complete().unwrap();
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
fn retained_root_schema_preserves_existing_ordinary_retry_reservations() {
    let home = setup();
    let root = home.path().join("old-root");
    fs::create_dir(&root).unwrap();
    let conn = connection().unwrap();
    conn.execute_batch(
        "CREATE TABLE model_download_reservations (
        card_id TEXT PRIMARY KEY, models_root TEXT NOT NULL, size_bytes INTEGER NOT NULL,
        downloading INTEGER NOT NULL)",
    )
    .unwrap();
    conn.execute(
        "INSERT INTO model_download_reservations VALUES('card',?1,10,0)",
        [root.to_string_lossy()],
    )
    .unwrap();
    let retry =
        DownloadReservation::acquire("card", &home.path().join("new-root"), || false, || {})
            .unwrap();
    assert_eq!(retry.root, root);
    assert_eq!(downloads().unwrap().len(), 1);
    assert!(retained_roots(None).unwrap().is_empty());
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
fn full_delete_releases_install_target_but_keeps_each_old_roots_residuals_charged() {
    let home = setup();
    let first = home.path().join("first");
    complete_download("card", &first, 10);
    fs::write(first.join("leftover-cache"), vec![0; 7]).unwrap();
    // The card's uninstall script removes its own weights; unknown shared-root
    // files survive the successful database deletion and remain quota-visible.
    fs::remove_file(first.join("card-weights")).unwrap();
    db::delete_store_model("card").unwrap();
    assert!(downloads().unwrap().is_empty());
    assert_eq!(retained_roots(Some("card")).unwrap(), [first.clone()]);
    assert_eq!(super::super::retained_bytes().unwrap(), 7);

    let second = home.path().join("second");
    let new_install = DownloadReservation::acquire("card", &second, || false, || {}).unwrap();
    assert_eq!(new_install.root, second);
    new_install.publish(10).unwrap();
    assert_eq!(super::super::retained_bytes().unwrap(), 17);
    drop(new_install);
    complete_download("card", &second, 10);
    fs::write(second.join("leftover-cache"), vec![0; 3]).unwrap();
    assert_eq!(super::super::retained_bytes().unwrap(), 10);
    fs::remove_file(second.join("card-weights")).unwrap();
    db::delete_store_model("card").unwrap();
    assert_eq!(super::super::retained_bytes().unwrap(), 10);
    assert_eq!(download_paths("card").unwrap(), [first, second]);

    let third = home.path().join("third");
    let failed = DownloadReservation::acquire("card", &third, || false, || {}).unwrap();
    assert_eq!(failed.root, third);
    failed.publish(10).unwrap();
    fs::write(third.join("partial"), vec![0; 2]).unwrap();
    drop(failed);
    assert_eq!(super::super::retained_bytes().unwrap(), 12);
    let retry =
        DownloadReservation::acquire("card", &home.path().join("fourth"), || false, || {}).unwrap();
    assert_eq!(retry.root, third, "failed retries retain their own target");
}

#[test]
fn repeated_same_root_reinstalls_deduplicate_retained_and_current_accounting() {
    let home = setup();
    let root = home.path().join("models");
    fs::create_dir(&root).unwrap();
    fs::write(root.join("leftover-cache"), vec![0; 7]).unwrap();
    for _ in 0..3 {
        complete_download("card", &root, 10);
        assert_eq!(super::super::retained_bytes().unwrap(), 7);
        fs::remove_file(root.join("card-weights")).unwrap();
        db::delete_store_model("card").unwrap();
        assert_eq!(retained_roots(Some("card")).unwrap(), [root.clone()]);
        assert_eq!(super::super::retained_bytes().unwrap(), 7);
    }
    complete_download("peer", &root, 10);
    assert_eq!(super::super::retained_bytes().unwrap(), 7);
    fs::remove_file(root.join("peer-weights")).unwrap();
    db::delete_store_model("peer").unwrap();
    assert_eq!(retained_roots(None).unwrap().len(), 2);
    assert_eq!(super::super::retained_bytes().unwrap(), 7);
    let reinstall = DownloadReservation::acquire("card", &root, || false, || {}).unwrap();
    reinstall.publish(10).unwrap();
    assert_eq!(super::super::retained_bytes().unwrap(), 17);
    assert_eq!(download_paths("card").unwrap(), [root]);
}

#[test]
fn delete_failure_rolls_back_target_retirement_with_the_model_and_credentials() {
    let home = setup();
    let root = home.path().join("models");
    complete_download("card", &root, 10);
    let key = db::add_api_key("card", Some("test")).unwrap();
    let conn = connection().unwrap();
    conn.execute_batch("CREATE TRIGGER refuse_credential_delete BEFORE DELETE ON api_keys BEGIN SELECT RAISE(ABORT,'injected'); END;").unwrap();
    assert!(db::delete_store_model("card").is_err());
    assert!(db::get_store_model("card").unwrap().is_some());
    assert!(db::find_api_key(&key.key_value).unwrap().is_some());
    assert_eq!(downloads().unwrap()[0].root, root);
    assert!(retained_roots(Some("card")).unwrap().is_empty());
    let retry =
        DownloadReservation::acquire("card", &home.path().join("new-default"), || false, || {})
            .unwrap();
    assert_eq!(retry.root, root);
}

#[test]
fn force_remove_discloses_and_atomically_forgets_current_and_retired_roots() {
    let home = setup();
    let old_root = home.path().join("old");
    complete_download("card", &old_root, 10);
    db::delete_store_model("card").unwrap();
    let current_root = home.path().join("current");
    complete_download("card", &current_root, 10);
    let key = db::add_api_key("card", Some("test")).unwrap();
    let manager = super::super::super::lifecycle::ModelManager::new(None, None);
    let conn = connection().unwrap();
    conn.execute_batch("CREATE TRIGGER refuse_credential_delete BEFORE DELETE ON api_keys BEGIN SELECT RAISE(ABORT,'injected'); END;").unwrap();
    assert!(manager.force_remove_record("card").is_err());
    assert_eq!(retained_roots(Some("card")).unwrap(), [old_root.clone()]);
    assert_eq!(downloads().unwrap()[0].root, current_root);
    assert!(db::get_store_model("card").unwrap().is_some());
    assert!(db::find_api_key(&key.key_value).unwrap().is_some());
    conn.execute_batch("DROP TRIGGER refuse_credential_delete")
        .unwrap();
    let outcome = manager.force_remove_record("card").unwrap();
    for root in [&old_root, &current_root] {
        assert!(
            outcome
                .residual_paths
                .contains(&root.to_string_lossy().into_owned())
        );
        assert!(root.join("card-weights").exists());
    }
    assert_eq!(outcome.warning, "force_remove_residual_paths");
    assert!(retained_roots(Some("card")).unwrap().is_empty());
    assert!(downloads().unwrap().is_empty());
    assert_eq!(super::super::retained_bytes().unwrap(), 0);
    assert!(db::get_store_model("card").unwrap().is_none());
    assert!(db::find_api_key(&key.key_value).unwrap().is_none());
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
