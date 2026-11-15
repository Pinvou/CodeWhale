//! Revalidate mutable facts before accepting preparation or changing resources.
use super::*;

fn card_rows(table: &str, card_id: &str) -> i64 {
    db::connect()
        .unwrap()
        .query_row(
            &format!("SELECT COUNT(*) FROM {table} WHERE card_id=?1"),
            [card_id],
            |row| row.get(0),
        )
        .unwrap()
}

#[test]
fn purge_checks_the_database_active_identity_before_any_uninstall_effect() {
    for explicit_active in [false, true] {
        let (_home, manager, current, _card, _package, _revision) = fixture();
        let models = manager.settings.models_path();
        let weight = models.join("legacy/old.tachyon");
        let sentinel = models.join("uninstall-ran");
        let runtime = runtime_path(&current).unwrap();
        let deployment = fs::read(runtime.join(".deploy-env")).unwrap();
        let mut scripts = current.scripts_content.clone();
        scripts.insert(
            "uninstall".into(),
            blob(
                "#!/bin/bash\nset -e\nprintf ran > \"$LMM_MODELS_DIR/uninstall-ran\"\nrm -f \"$LMM_MODELS_DIR/legacy/old.tachyon\" .deploy-env\n",
            ),
        );
        db::set_scripts_content(&current.card_id, &scripts, &current.assets_content).unwrap();
        let current = manager.get_record(&current.card_id).unwrap();
        let active = version_store::active_id(&current);
        if explicit_active {
            db::connect()
                .unwrap()
                .execute(
                    "UPDATE store_models SET active_version_id=?1 WHERE card_id=?2",
                    rusqlite::params![active, current.card_id],
                )
                .unwrap();
        }
        let current = manager.get_record(&current.card_id).unwrap();
        version_store::put(&Version {
            card_id: current.card_id.clone(),
            version_id: active.clone(),
            state: "current".into(),
            record: Some(current.clone()),
            models_root: Some(models.to_string_lossy().into_owned()),
            runtime_card_id: current.card_id.clone(),
            resource_dir: Some(models.join("legacy").to_string_lossy().into_owned()),
            managed: false,
            created_at: now(),
            error: None,
            planned_bytes: 0,
            measured_bytes: None,
        })
        .unwrap();
        let key = db::add_api_key(&current.card_id, Some("purge admission fixture"))
            .unwrap()
            .key_value;
        let mut download =
            version_store::DownloadReservation::acquire(&current.card_id, &models, || false, || {})
                .unwrap();
        download.publish(11).unwrap();
        download.start_scripts().unwrap();
        download.complete().unwrap();
        drop(download);
        manager.versions(&current.card_id, true).unwrap();
        let before = serde_json::to_value(&current).unwrap();
        let version_before =
            serde_json::to_value(version_store::get(&current.card_id, &active).unwrap()).unwrap();
        assert!(!version_store::has_history(&current.card_id).unwrap());
        assert_eq!(
            card_rows("model_download_reservations", &current.card_id),
            1
        );
        assert_eq!(
            card_rows("model_current_version_measurements", &current.card_id),
            1
        );

        let result = manager.uninstall(&current.card_id, true, &mut |_| {}, Default::default());
        if explicit_active {
            let outcome = result.expect("an explicit sole active generation can still be purged");
            assert!(outcome.purged_weights);
            assert!(!outcome.record_kept);
            assert_eq!(fs::read(&sentinel).unwrap(), b"ran");
            assert!(!weight.exists());
            assert!(db::get_store_model(&current.card_id).unwrap().is_none());
            assert!(db::find_api_key(&key).unwrap().is_none());
            assert!(
                version_store::get(&current.card_id, &active)
                    .unwrap()
                    .is_none()
            );
            assert_eq!(
                card_rows("model_download_reservations", &current.card_id),
                0
            );
        } else {
            let error = result.unwrap_err().to_string();
            assert!(error.contains("先在历史版本中清理保留版本"), "{error}");
            assert!(!sentinel.exists(), "the destructive script must never run");
            assert_eq!(fs::read(&weight).unwrap(), b"old-weights");
            assert_eq!(fs::read(runtime.join(".deploy-env")).unwrap(), deployment);
            assert_eq!(
                serde_json::to_value(db::get_store_model(&current.card_id).unwrap().unwrap())
                    .unwrap(),
                before
            );
            assert_eq!(
                serde_json::to_value(version_store::get(&current.card_id, &active).unwrap())
                    .unwrap(),
                version_before
            );
            assert!(db::find_api_key(&key).unwrap().is_some());
            assert_eq!(
                card_rows("model_download_reservations", &current.card_id),
                1
            );
            assert_eq!(
                card_rows("model_download_retained_roots", &current.card_id),
                0
            );
            assert_eq!(
                card_rows("model_current_version_measurements", &current.card_id),
                1,
                "refusal must precede even measurement invalidation"
            );
        }
    }
}

struct DigestProcess(Option<std::process::Child>);

impl DigestProcess {
    fn new(child: std::process::Child) -> Self {
        Self(Some(child))
    }

    fn stop(&mut self) {
        // Observe both results before asserting, so a failed kill still joins.
        let Some(mut child) = self.0.take() else {
            return;
        };
        let killed = child.kill();
        let joined = child.wait();
        killed.unwrap();
        joined.unwrap();
    }
}

impl Drop for DigestProcess {
    fn drop(&mut self) {
        // Panic paths also retire only this owned child before TempHome drops.
        if let Some(mut child) = self.0.take() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

#[test]
fn legacy_cleanup_rechecks_processes_started_at_the_end_of_the_digest() {
    for use_runtime in [false, true] {
        let (_home, manager, current, card, package, revision) = fixture();
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
        manager
            .switch_version(&current.card_id, &revision, &mut |_| {}, Default::default())
            .unwrap();
        let old_id = version_store::active_id(&current);
        let old = version_store::get(&current.card_id, &old_id)
            .unwrap()
            .unwrap();
        let runtime = runtime_path(&current).unwrap();
        let resource = PathBuf::from(old.resource_dir.as_ref().unwrap());
        let weight = resource.join("old.tachyon");
        let cwd = if use_runtime { &runtime } else { &resource };
        let before = serde_json::to_value(&old).unwrap();
        let active_before =
            serde_json::to_value(manager.get_record(&current.card_id).unwrap()).unwrap();
        let mut child = None;
        let mut detected = false;
        let result = manager.delete_version(
            &current.card_id,
            &old_id,
            &mut |line| {
                if child.is_none() && line == "LMM_PROGRESS_BYTES=11/11" {
                    child = Some(DigestProcess::new(spawn_holding_argv_form(
                        &weight,
                        "--unrelated-argument",
                        Some(cwd),
                    )));
                    detected = wait_until_resource_blocked(&runtime, &resource);
                }
            },
            Default::default(),
        );
        let mut child = child.expect("the real digest must reach its final progress callback");
        child.stop();
        assert!(detected, "the owned child must expose the selected cwd");
        assert!(result.unwrap_err().to_string().contains("仍有运行进程"));
        assert_eq!(fs::read(&weight).unwrap(), b"old-weights");
        assert!(runtime.join(".deploy-env").exists());
        assert_eq!(
            serde_json::to_value(
                version_store::get(&current.card_id, &old_id)
                    .unwrap()
                    .unwrap()
            )
            .unwrap(),
            before,
            "refusal must precede the deleting marker"
        );
        assert_eq!(
            serde_json::to_value(manager.get_record(&current.card_id).unwrap()).unwrap(),
            active_before
        );
    }
}

#[test]
fn switching_rechecks_both_generations_after_the_target_digest() {
    for reference in [
        "current-resource",
        "current-runtime",
        "target-resource",
        "target-runtime",
    ] {
        let (_home, manager, current, card, package, revision) = fixture();
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
        let target = version_store::get(&current.card_id, &revision)
            .unwrap()
            .unwrap();
        let target_record = target.record.as_ref().unwrap();
        let current_runtime = runtime_path(&current).unwrap();
        let current_resource = manager.settings.models_path().join("legacy");
        let target_runtime = runtime_path(target_record).unwrap();
        let target_resource = PathBuf::from(target.resource_dir.as_ref().unwrap());
        let (runtime, resource, cwd, weight) = if reference.starts_with("current") {
            (
                &current_runtime,
                &current_resource,
                if reference.ends_with("runtime") {
                    &current_runtime
                } else {
                    &current_resource
                },
                current_resource.join("old.tachyon"),
            )
        } else {
            (
                &target_runtime,
                &target_resource,
                if reference.ends_with("runtime") {
                    &target_runtime
                } else {
                    &target_resource
                },
                target_resource.join("new/new.tachyon"),
            )
        };
        let before = serde_json::to_value(&current).unwrap();
        let versions_before = serde_json::to_value(version_store::list(None).unwrap()).unwrap();
        let mut target_digest = false;
        let mut detected = false;
        let mut child = None;
        let result = manager.switch_version(
            &current.card_id,
            &revision,
            &mut |line| {
                if line.starts_with("校验目标版本权重") {
                    target_digest = true;
                }
                if target_digest && child.is_none() && line == "LMM_PROGRESS_BYTES=11/11" {
                    child = Some(DigestProcess::new(spawn_holding_argv_form(
                        &weight,
                        "--unrelated-argument",
                        Some(cwd),
                    )));
                    detected = wait_until_resource_blocked(runtime, resource);
                }
            },
            Default::default(),
        );
        let mut child = child.expect("the target digest must report its final progress");
        child.stop();
        assert!(detected, "the owned process must hold {reference}");
        assert!(result.unwrap_err().to_string().contains("仍有运行进程"));
        assert!(
            !target_runtime.join(".deploy-env").exists(),
            "deployment must not run"
        );
        assert_eq!(
            fs::read(current_resource.join("old.tachyon")).unwrap(),
            b"old-weights"
        );
        assert_eq!(
            fs::read(target_resource.join("new/new.tachyon")).unwrap(),
            b"new-weights"
        );
        assert_eq!(
            serde_json::to_value(manager.get_record(&current.card_id).unwrap()).unwrap(),
            before
        );
        assert_eq!(
            serde_json::to_value(version_store::list(None).unwrap()).unwrap(),
            versions_before
        );
    }
}

#[test]
fn repeated_preparation_checks_structure_without_rewriting_or_digesting_weights() {
    use std::os::unix::fs::symlink;
    for mutation in ["missing-weight", "linked-weight", "marker", "complete"] {
        let (home, manager, current, mut card, mut package, _) = fixture();
        let script = blob(
            "#!/bin/bash\nset -e\nmkdir -p \"$LMM_MODELS_DIR/new\"\nprintf new-weights > \"$LMM_MODELS_DIR/new/new.tachyon\"\nprintf x >> \"$LMM_MODELS_DIR/download-attempts\"\n",
        );
        card.script_sha256
            .as_mut()
            .unwrap()
            .insert("download_model".into(), script.sha256.clone());
        package.scripts.insert("download_model".into(), script);
        let revision = card_update::revision(&card_update::remote_manifest(&card));
        manager
            .stage_verified(
                &current,
                &card,
                &revision,
                package.clone(),
                &mut |_| {},
                Default::default(),
            )
            .unwrap();
        let version = version_store::get(&current.card_id, &revision)
            .unwrap()
            .unwrap();
        let root = PathBuf::from(version.models_root.as_ref().unwrap());
        let weight = root.join("new/new.tachyon");
        let saved = home.path().join("saved-weight");
        match mutation {
            "missing-weight" => fs::remove_file(&weight).unwrap(),
            "linked-weight" => {
                fs::rename(&weight, &saved).unwrap();
                symlink(&saved, &weight).unwrap();
            }
            "marker" => fs::write(root.join(".pinvou-version.json"), "wrong-owner").unwrap(),
            _ => {}
        }
        let before = serde_json::to_value(&version).unwrap();
        let active_before =
            serde_json::to_value(manager.get_record(&current.card_id).unwrap()).unwrap();
        let marker_before = fs::read(root.join(".pinvou-version.json")).unwrap();
        let mut progress = Vec::new();
        let result = manager.stage_verified(
            &current,
            &card,
            &revision,
            package,
            &mut |line| progress.push(line.to_owned()),
            Default::default(),
        );
        assert_eq!(
            result.is_ok(),
            mutation == "complete",
            "{mutation}: {result:?}"
        );
        assert!(
            progress.is_empty(),
            "a repeat must not download or digest weights"
        );
        assert_eq!(fs::read(root.join("download-attempts")).unwrap(), b"x");
        assert_eq!(
            fs::read(root.join(".pinvou-version.json")).unwrap(),
            marker_before
        );
        assert_eq!(
            serde_json::to_value(
                version_store::get(&current.card_id, &revision)
                    .unwrap()
                    .unwrap()
            )
            .unwrap(),
            before
        );
        assert_eq!(
            serde_json::to_value(manager.get_record(&current.card_id).unwrap()).unwrap(),
            active_before
        );
        match mutation {
            "missing-weight" => assert!(!weight.exists()),
            "linked-weight" => {
                assert!(weight.symlink_metadata().unwrap().is_symlink());
                assert_eq!(fs::read(&saved).unwrap(), b"new-weights");
            }
            _ => assert_eq!(fs::read(&weight).unwrap(), b"new-weights"),
        }
    }
}

#[test]
fn repeated_preparation_keeps_managed_and_legacy_current_history_layouts() {
    let (_home, manager, current, card, package, revision) = fixture();
    let legacy_package = CardPackage {
        scripts: current.scripts_content.clone(),
        assets: current.assets_content.clone(),
    };
    let mut legacy_card = card.clone();
    legacy_card.summary.name = current.card_name.clone();
    legacy_card.summary.org = current.org.clone();
    legacy_card.summary.description = current.description.clone();
    legacy_card.summary.engine_name = current.engine_name.clone();
    legacy_card.summary.engine_version = current.engine_version.clone();
    legacy_card.summary.size_bytes = current.size_bytes as u64;
    legacy_card.summary.quantization = current.quantization.clone();
    legacy_card.summary.deploy_topology = current.deploy_topology;
    legacy_card.source_repo_id = current.source_repo_id.clone();
    legacy_card.source_filename = current.source_filename.clone();
    legacy_card.profiles = current.profiles.clone();
    legacy_card.scripts = legacy_package
        .scripts
        .keys()
        .map(|name| (name.clone(), format!("https://example.test/{name}")))
        .collect();
    legacy_card.assets = legacy_package
        .assets
        .keys()
        .map(|name| (name.clone(), format!("https://example.test/{name}")))
        .collect();
    legacy_card.script_sha256 = Some(
        legacy_package
            .scripts
            .iter()
            .map(|(name, blob)| (name.clone(), blob.sha256.clone()))
            .collect(),
    );
    legacy_card.asset_sha256 = Some(
        legacy_package
            .assets
            .iter()
            .map(|(name, blob)| (name.clone(), blob.sha256.clone()))
            .collect(),
    );
    let old_id = version_store::active_id(&current);
    assert_eq!(
        card_update::revision(&card_update::remote_manifest(&legacy_card)),
        old_id,
        "each repeated request must carry its own matching package identity"
    );
    manager
        .stage_verified(
            &current,
            &card,
            &revision,
            package.clone(),
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
    manager
        .switch_version(&current.card_id, &revision, &mut |_| {}, Default::default())
        .unwrap();
    let active = manager.get_record(&current.card_id).unwrap();
    let before = serde_json::to_value(version_store::list(None).unwrap()).unwrap();
    for (id, card, package) in [
        (&revision, &card, &package),
        (&old_id, &legacy_card, &legacy_package),
    ] {
        manager
            .stage_verified(
                &active,
                card,
                id,
                package.clone(),
                &mut |_| {},
                Default::default(),
            )
            .unwrap();
    }
    assert_eq!(
        serde_json::to_value(version_store::list(None).unwrap()).unwrap(),
        before
    );
    manager
        .switch_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap();
    let legacy = manager.get_record(&current.card_id).unwrap();
    let before = serde_json::to_value(version_store::list(None).unwrap()).unwrap();
    for (id, card, package) in [
        (&revision, &card, &package),
        (&old_id, &legacy_card, &legacy_package),
    ] {
        manager
            .stage_verified(
                &legacy,
                card,
                id,
                package.clone(),
                &mut |_| {},
                Default::default(),
            )
            .unwrap();
    }
    assert_eq!(
        serde_json::to_value(version_store::list(None).unwrap()).unwrap(),
        before
    );
}
