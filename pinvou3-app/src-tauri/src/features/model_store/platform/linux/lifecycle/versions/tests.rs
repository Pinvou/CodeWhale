use super::*;

fn blob(content: &str) -> db::ScriptBlob {
    db::ScriptBlob {
        content: content.into(),
        sha256: script_runner::sha256_hex(content),
    }
}

fn fixture() -> (
    config::TempHome,
    ModelManager,
    db::StoreModelRecord,
    StoreCardDetail,
    CardPackage,
    String,
) {
    let temp = config::TempHome::new();
    db::init_db().unwrap();
    let models = temp.path().join("models");
    fs::create_dir_all(models.join("legacy")).unwrap();
    fs::write(models.join("legacy/old.tachyon"), b"old-weights").unwrap();
    let mut old_scripts = BTreeMap::new();
    for entry in [
        "install_engine",
        "download_model",
        "deploy",
        "enable",
        "stop",
        "uninstall",
    ] {
        old_scripts.insert(entry.into(), blob("#!/bin/bash\nexit 0\n"));
    }
    old_scripts.insert(
        "deploy".into(),
        blob(
            "#!/bin/bash\nprintf 'MODEL_DIR=\"%s/legacy\"\\n' \"$LMM_MODELS_DIR\" > .deploy-env\n",
        ),
    );
    // Legacy native deployments use shell scripts, without the newer Python
    // controller. Exercise that real migration shape throughout the lifecycle.
    let old_assets = BTreeMap::from([(
        "weights.sha256".into(),
        blob(&format!(
            "{}  old.tachyon\n",
            script_runner::sha256_hex("old-weights")
        )),
    )]);
    db::add_store_model(db::AddStoreModelParams {
        card_id: "version-test",
        model_name: "stable-name",
        card_name: "Old",
        org: None,
        description: None,
        engine_name: "native",
        engine_version: Some("old"),
        size_bytes: 11,
        quantization: Some("NVFP4"),
        source_repo_id: Some("org/old"),
        source_filename: Some("old.tachyon"),
        scripts: &Default::default(),
        status: "deployed",
        profiles: &[],
        deploy_topology: DeployTopology::Single,
        card_source: "store",
    })
    .unwrap();
    db::set_scripts_content("version-test", &old_scripts, &old_assets).unwrap();
    let runtime = config::data_dir().join("cards/version-test");
    fs::create_dir_all(&runtime).unwrap();
    fs::write(
        runtime.join(".deploy-env"),
        format!("MODEL_DIR=\"{}\"\n", models.join("legacy").display()),
    )
    .unwrap();
    let rec = db::get_store_model("version-test").unwrap().unwrap();
    let mut scripts = old_scripts.clone();
    scripts.insert("download_model".into(),blob("#!/bin/bash\nset -e\nmkdir -p \"$LMM_MODELS_DIR/new\"\nprintf new-weights > \"$LMM_MODELS_DIR/new/new.tachyon\"\n"));
    scripts.insert(
        "deploy".into(),
        blob("#!/bin/bash\nprintf 'MODEL_DIR=\"%s/new\"\\n' \"$LMM_MODELS_DIR\" > .deploy-env\n"),
    );
    let assets: BTreeMap<String, db::ScriptBlob> = BTreeMap::from([
        (
            "weights.sha256".into(),
            blob(&format!(
                "{}  new.tachyon\n",
                script_runner::sha256_hex("new-weights")
            )),
        ),
        (
            "native_engine_process.py".into(),
            blob("# isolated test controller\n"),
        ),
    ]);
    let card: StoreCardDetail=serde_json::from_value(serde_json::json!({
        "id":"version-test","name":"New","engine_name":"native","engine_version":"new",
        "source_repo_id":"org/new","source_filename":"new.tachyon","quantization":"NVFP4","size_bytes":11,
        "scripts":scripts.keys().map(|k|(k.clone(),format!("https://example.test/{k}"))).collect::<BTreeMap<_,_>>(),
        "script_sha256":scripts.iter().map(|(k,v)|(k.clone(),v.sha256.clone())).collect::<BTreeMap<_,_>>(),
        "assets":assets.keys().map(|k|(k.clone(),format!("https://example.test/{k}"))).collect::<BTreeMap<_,_>>(),
        "asset_sha256":assets.iter().map(|(k,v)|(k.clone(),v.sha256.clone())).collect::<BTreeMap<_,_>>()
    })).unwrap();
    let revision = card_update::revision(&card_update::remote_manifest(&card));
    let settings = Settings {
        models_dir: models.to_string_lossy().into_owned(),
        staging_dir: temp.path().join("staging").to_string_lossy().into_owned(),
        min_free_space_gb: 0.0,
        ..Settings::default()
    };
    fs::create_dir_all(&settings.staging_dir).unwrap();
    (
        temp,
        ModelManager::new(Some(settings), None),
        rec,
        card,
        CardPackage { scripts, assets },
        revision,
    )
}

#[test]
fn prepare_switch_rollback_and_cleanup_preserve_credentials_and_old_weights() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    let key = db::add_api_key(&current.card_id, Some("stable credential")).unwrap();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    assert_eq!(
        serde_json::to_value(db::get_store_model(&current.card_id).unwrap().unwrap()).unwrap(),
        serde_json::to_value(&current).unwrap()
    );
    let staged = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(staged.state, "ready");
    assert_eq!(
        fs::read(mgr.settings.models_path().join("legacy/old.tachyon")).unwrap(),
        b"old-weights"
    );
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    assert_eq!(active.active_version_id.as_deref(), Some(id.as_str()));
    assert_eq!(active.model_name, current.model_name);
    assert_eq!(
        db::list_api_keys_for_card(&current.card_id).unwrap()[0].key_value,
        key.key_value
    );
    let old_id = version_store::active_id(&current);
    assert_eq!(
        version_store::get(&current.card_id, &old_id)
            .unwrap()
            .unwrap()
            .state,
        "history"
    );
    assert!(
        mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(mgr.delete_engine("native", Some("old")).is_err());
    assert!(mgr.force_remove_record(&current.card_id).is_err());
    mgr.switch_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap();
    let restored = db::get_store_model(&current.card_id).unwrap().unwrap();
    assert_eq!(restored.source_filename, current.source_filename);
    assert_eq!(restored.engine_version, current.engine_version);
    let new_root = PathBuf::from(staged.models_root.unwrap());
    let outcome = mgr
        .delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    assert!(outcome["freed_bytes"].as_u64().unwrap() > 0);
    assert!(!new_root.exists());
    assert!(
        mgr.settings
            .models_path()
            .join("legacy/old.tachyon")
            .exists()
    );
    assert!(db::find_api_key(&key.key_value).unwrap().is_some());
}

#[test]
fn changed_weights_with_unchanged_metadata_are_detected_and_labels_can_be_corrected() {
    let (_temp, _mgr, current, mut card, pkg, _id) = fixture();
    card.source_repo_id = current.source_repo_id.clone();
    card.source_filename = current.source_filename.clone();
    let assessment = card_update::compatibility(&current, &card, pkg.assets.get("weights.sha256"));
    assert_eq!(assessment.kind, "weights_changed");
    assert!(!assessment.can_update);
    let old = current.assets_content["weights.sha256"].clone();
    card.source_filename = current.source_filename.clone();
    card.source_repo_id = Some("corrected/source".into());
    card.asset_sha256
        .as_mut()
        .unwrap()
        .insert("weights.sha256".into(), old.sha256.clone());
    card.summary.quantization = Some(" nv-fp4 ".into());
    let assessment = card_update::compatibility(&current, &card, Some(&old));
    assert!(assessment.can_update);
    card.summary.deploy_topology = DeployTopology::Dual;
    assert_eq!(
        card_update::compatibility(&current, &card, Some(&old)).kind,
        "topology_changed"
    );
}

#[test]
fn hash_failure_preserves_active_record_and_partial_download_can_be_cleaned() {
    let (_temp, mgr, current, card, mut pkg, id) = fixture();
    pkg.scripts.insert("download_model".into(),blob("#!/bin/bash\nmkdir -p \"$LMM_MODELS_DIR/new\"\nprintf corrupt > \"$LMM_MODELS_DIR/new/new.tachyon\"\n"));
    assert!(
        mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
            .is_err()
    );
    let failed = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(failed.state, "failed");
    assert_eq!(
        db::get_store_model(&current.card_id)
            .unwrap()
            .unwrap()
            .source_filename,
        current.source_filename
    );
    mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    assert!(!Path::new(failed.models_root.as_ref().unwrap()).exists());
}

#[test]
fn failed_deploy_and_atomic_commit_leave_current_version_usable() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let mut target = version_store::get(&current.card_id, &id).unwrap().unwrap();
    target
        .record
        .as_mut()
        .unwrap()
        .scripts_content
        .insert("deploy".into(), blob("#!/bin/bash\nexit 1\n"));
    version_store::put(&target).unwrap();
    assert!(
        mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert_eq!(
        db::get_store_model(&current.card_id)
            .unwrap()
            .unwrap()
            .source_filename,
        current.source_filename
    );
    assert_eq!(
        version_store::list(Some(&current.card_id)).unwrap().len(),
        1
    );
    target
        .record
        .as_mut()
        .unwrap()
        .scripts_content
        .insert("deploy".into(), blob("#!/bin/bash\nexit 0\n"));
    version_store::put(&target).unwrap();
    db::connect().unwrap().execute_batch("CREATE TRIGGER reject_activation BEFORE UPDATE ON store_models BEGIN SELECT RAISE(ABORT,'injected failure'); END;").unwrap();
    assert!(
        mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert_eq!(
        version_store::list(Some(&current.card_id)).unwrap().len(),
        1
    );
    assert!(
        db::get_store_model(&current.card_id)
            .unwrap()
            .unwrap()
            .active_version_id
            .is_none()
    );
}

#[test]
fn running_shared_and_untrusted_directories_are_not_deleted() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let target = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let mut shared = target.clone();
    shared.card_id = "another-card".into();
    shared.version_id = "another-version".into();
    version_store::put(&shared).unwrap();
    assert!(
        mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(Path::new(target.models_root.as_ref().unwrap()).exists());
    version_store::remove(&shared.card_id, &shared.version_id).unwrap();
    db::set_running(&current.card_id, true).unwrap();
    assert!(
        mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .is_err()
    );
    db::set_running(&current.card_id, false).unwrap();
    let marker = Path::new(target.models_root.as_ref().unwrap()).join(".pinvou-version.json");
    fs::write(&marker, "wrong-owner").unwrap();
    assert!(
        mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(Path::new(target.models_root.as_ref().unwrap()).exists());
}

#[test]
fn legacy_cleanup_rejects_links_and_counts_only_exclusive_weight_blocks() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    let old_file = mgr.settings.models_path().join("legacy/old.tachyon");
    let retained_file = old_file.with_file_name("retained.tachyon");
    fs::rename(&old_file, &retained_file).unwrap();
    std::os::unix::fs::symlink(&retained_file, &old_file).unwrap();
    assert!(
        mgr.delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(old_file.symlink_metadata().unwrap().is_symlink());
    assert_eq!(fs::read(&retained_file).unwrap(), b"old-weights");
    assert_eq!(
        version_store::get(&current.card_id, &old_id)
            .unwrap()
            .unwrap()
            .state,
        "history"
    );
    // Broken links are also ownership mismatches, not already removed files.
    fs::remove_file(&retained_file).unwrap();
    assert!(
        mgr.delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
            .is_err()
    );
    fs::remove_file(&old_file).unwrap();
    fs::write(&retained_file, b"old-weights").unwrap();
    fs::hard_link(&retained_file, &old_file).unwrap();
    let result = mgr
        .delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap();
    assert_eq!(result["freed_bytes"], 0);
    assert!(!old_file.exists());
    assert_eq!(fs::read(&retained_file).unwrap(), b"old-weights");
    assert_eq!(
        db::get_store_model(&current.card_id)
            .unwrap()
            .unwrap()
            .active_version_id
            .as_deref(),
        Some(id.as_str())
    );
}

#[test]
fn interrupted_cleanup_and_cancelled_download_remain_manageable() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    let cancel = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let control = script_runner::RunControl {
        cancel: Some(cancel.clone()),
        prompt: None,
    };
    assert!(
        mgr.stage_verified(
            &current,
            &card,
            &id,
            pkg,
            &mut |_| cancel.store(true, std::sync::atomic::Ordering::SeqCst),
            control
        )
        .is_err()
    );
    let mut target = version_store::get(&current.card_id, &id).unwrap().unwrap();
    target.state = "deleting".into();
    version_store::put(&target).unwrap();
    fs::remove_dir_all(target.models_root.as_ref().unwrap()).unwrap();
    mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    assert!(version_store::get(&current.card_id, &id).unwrap().is_none());
    assert!(
        mgr.settings
            .models_path()
            .join("legacy/old.tachyon")
            .exists()
    );
}

#[test]
fn actual_qwen_replacement_is_a_weight_upgrade_not_a_metadata_correction() {
    let (_temp, _mgr, mut current, mut card, mut pkg, _id) = fixture();
    current.engine_name = "驭光（H3C自研推理引擎）".into();
    current.engine_version = Some(String::new());
    assert!(
        !current
            .assets_content
            .contains_key("native_engine_process.py")
    );
    current.source_repo_id = Some("pinvou/Qwen3.8-27B-NVFP4-Tachyon".into());
    current.source_filename = Some("qwen3_8_27b_nvfp4_policy.tachyon".into());
    current.size_bytes = 21_944_459_776;
    current.assets_content.insert("weights.sha256".into(),blob("c1e9636bd763ef50c6b8e1d647639940e1df99b4bb7e23c42ad8d14993ea8094  qwen3_8_27b_nvfp4_policy.tachyon\n"));
    card.source_repo_id = current.source_repo_id.clone();
    card.source_filename = Some("qwen3_8_27b_nvfp4.tachyon".into());
    card.summary.size_bytes = 22_240_278_272;
    let next = blob(
        "05c74c488b56a30097b25973888b31a16ddcfa1dec90556a2ae5e0c3201e50bb  qwen3_8_27b_nvfp4.tachyon\n",
    );
    card.asset_sha256
        .as_mut()
        .unwrap()
        .insert("weights.sha256".into(), next.sha256.clone());
    pkg.assets.insert("weights.sha256".into(), next.clone());
    let result = card_update::compatibility(&current, &card, Some(&next));
    assert_eq!(result.kind, "weights_changed");
    assert!(result.can_stage);
    assert!(!result.can_update);
    current.scripts_content.clear();
    assert!(!card_update::compatibility(&current, &card, Some(&next)).can_stage);
}

#[test]
fn upgrade_requires_a_supported_rollback_layout_and_a_selected_verified_artifact() {
    let (_temp, _mgr, mut current, mut card, pkg, _id) = fixture();
    let evidence = pkg.assets.get(weight_identity::MANIFEST);
    assert!(card_update::compatibility(&current, &card, evidence).can_stage);
    let original = current.assets_content[weight_identity::MANIFEST].clone();
    current.assets_content.insert(
        weight_identity::MANIFEST.into(),
        blob(&format!(
            "{}{}  other.tachyon\n",
            original.content,
            "a".repeat(64)
        )),
    );
    let assessment = card_update::compatibility(&current, &card, evidence);
    assert!(!assessment.can_stage);
    assert_eq!(assessment.reason, "versioning_unsupported");
    current
        .assets_content
        .insert(weight_identity::MANIFEST.into(), original);
    let controller = card.assets.remove("native_engine_process.py").unwrap();
    assert!(!card_update::compatibility(&current, &card, evidence).can_stage);
    card.assets
        .insert("native_engine_process.py".into(), controller);
    card.source_filename = Some("not-in-manifest.tachyon".into());
    let assessment = card_update::compatibility(&current, &card, evidence);
    assert!(!assessment.can_stage);
    assert_eq!(assessment.reason, "filename_not_in_manifest");
}

#[test]
fn unsafe_paths_and_live_unrecorded_processes_block_cleanup() {
    let (temp, mgr, current, card, pkg, id) = fixture();
    let outside = temp.path().join("outside");
    fs::create_dir_all(&outside).unwrap();
    std::os::unix::fs::symlink(&outside, mgr.settings.models_path().join(".versions")).unwrap();
    assert!(
        mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
            .is_err()
    );
    assert_eq!(fs::read_dir(outside).unwrap().count(), 0);
    let mut child = std::process::Command::new("python3")
        .args(["-c", "import time; time.sleep(30)"])
        .arg(mgr.settings.models_path().join("legacy/old.tachyon"))
        .spawn()
        .unwrap();
    // Popen/exec may race /proc argv population; wait only for this test child.
    let mut detected = false;
    for _ in 0..100 {
        if ensure_unused(
            &runtime_path(&current).unwrap(),
            Some(&mgr.settings.models_path().join("legacy")),
        )
        .is_err()
        {
            detected = true;
            break;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    child.kill().unwrap();
    child.wait().unwrap();
    assert!(detected);
}

#[test]
fn interrupted_stage_is_recoverable_and_cancelled_cleanup_does_not_remove_files() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let mut row = version_store::get(&current.card_id, &id).unwrap().unwrap();
    row.state = "downloading".into();
    version_store::put(&row).unwrap();
    mgr.versions(&current.card_id).unwrap();
    assert_eq!(
        version_store::get(&current.card_id, &id)
            .unwrap()
            .unwrap()
            .state,
        "failed"
    );
    let control = script_runner::RunControl {
        cancel: Some(std::sync::Arc::new(std::sync::atomic::AtomicBool::new(
            true,
        ))),
        prompt: None,
    };
    assert!(
        mgr.delete_version(&current.card_id, &id, &mut |_| {}, control)
            .is_err()
    );
    assert!(Path::new(row.models_root.as_ref().unwrap()).exists());
    assert_eq!(
        version_store::get(&current.card_id, &id)
            .unwrap()
            .unwrap()
            .state,
        "failed"
    );
    mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    assert!(
        mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .is_ok()
    );
}

#[test]
fn changed_storage_settings_before_switch_cannot_archive_an_unresolved_old_version() {
    let (temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let mut settings = mgr.settings.clone();
    settings.models_dir = temp
        .path()
        .join("other-models")
        .to_string_lossy()
        .into_owned();
    fs::create_dir_all(&settings.models_dir).unwrap();
    let relocated = ModelManager::new(Some(settings), None);
    let error = relocated
        .switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap_err();
    assert!(error.to_string().contains("不能保证回退"));
    assert_eq!(
        serde_json::to_value(db::get_store_model(&current.card_id).unwrap().unwrap()).unwrap(),
        serde_json::to_value(&current).unwrap()
    );
    assert_eq!(
        version_store::get(&current.card_id, &id)
            .unwrap()
            .unwrap()
            .state,
        "ready"
    );
    assert_eq!(
        version_store::list(Some(&current.card_id)).unwrap().len(),
        1
    );
    assert!(
        mgr.settings
            .models_path()
            .join("legacy/old.tachyon")
            .exists()
    );
}

#[test]
fn rollback_uses_pinned_storage_after_settings_change_and_rejects_corrupt_recipe() {
    let (temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    let mut settings = mgr.settings.clone();
    settings.models_dir = temp
        .path()
        .join("other-models")
        .to_string_lossy()
        .into_owned();
    fs::create_dir_all(&settings.models_dir).unwrap();
    let relocated = ModelManager::new(Some(settings), None);
    relocated
        .switch_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap();
    let mut target = version_store::get(&current.card_id, &id).unwrap().unwrap();
    target
        .record
        .as_mut()
        .unwrap()
        .scripts_content
        .get_mut("deploy")
        .unwrap()
        .content
        .push_str("\n# corrupted");
    version_store::put(&target).unwrap();
    assert!(
        relocated
            .switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert_eq!(
        db::get_store_model(&current.card_id)
            .unwrap()
            .unwrap()
            .source_filename,
        current.source_filename
    );
}

#[test]
fn isolated_purge_checks_shared_references_before_deleting_weights() {
    let (_temp, mgr, current, card, mut pkg, id) = fixture();
    pkg.scripts.insert("uninstall".into(), blob(
        "#!/bin/bash\nprintf '%s' \"$LMM_PURGE_WEIGHTS\" > uninstall-purge-intent\nif [ \"$LMM_PURGE_WEIGHTS\" = 1 ]; then rm -rf \"$LMM_MODELS_DIR/new\"; exit 99; fi\n"
    ));
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    mgr.delete_version(
        &current.card_id,
        &version_store::active_id(&current),
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    let resource = legacy_resource_dir(&active, &mgr.settings.models_path()).unwrap();
    db::add_store_model(db::AddStoreModelParams {
        card_id: "shared-current",
        model_name: "shared",
        card_name: "Shared",
        org: None,
        description: None,
        engine_name: "native",
        engine_version: Some("new"),
        size_bytes: 11,
        quantization: None,
        source_repo_id: card.source_repo_id.as_deref(),
        source_filename: card.source_filename.as_deref(),
        scripts: &Default::default(),
        status: "deployed",
        profiles: &[],
        deploy_topology: DeployTopology::Single,
        card_source: "store",
    })
    .unwrap();
    let runtime = config::data_dir().join("cards/shared-current");
    fs::create_dir_all(&runtime).unwrap();
    fs::write(
        runtime.join(".deploy-env"),
        format!("MODEL_DIR=\"{}\"\n", resource.display()),
    )
    .unwrap();
    assert!(
        mgr.uninstall(&current.card_id, true, &mut |_| {}, Default::default())
            .is_err()
    );
    assert_eq!(
        fs::read(resource.join("new.tachyon")).unwrap(),
        b"new-weights"
    );
    assert!(
        !runtime_path(&active)
            .unwrap()
            .join("uninstall-purge-intent")
            .exists()
    );
    assert!(db::get_store_model(&current.card_id).unwrap().is_some());
    db::delete_store_model("shared-current").unwrap();
    mgr.uninstall(&current.card_id, true, &mut |_| {}, Default::default())
        .unwrap();
    assert!(!Path::new(active.models_root.as_ref().unwrap()).exists());
    assert!(db::get_store_model(&current.card_id).unwrap().is_none());
}

#[test]
fn explicit_purge_after_history_cleanup_removes_the_active_isolated_storage() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    let key = db::add_api_key(&current.card_id, Some("purge test")).unwrap();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    mgr.delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap();
    let root = db::get_store_model(&current.card_id)
        .unwrap()
        .unwrap()
        .models_root
        .unwrap();
    let outcome = mgr
        .uninstall(&current.card_id, true, &mut |_| {}, Default::default())
        .unwrap();
    assert!(!outcome.record_kept);
    assert!(!Path::new(&root).exists());
    assert!(db::get_store_model(&current.card_id).unwrap().is_none());
    assert!(db::find_api_key(&key.key_value).unwrap().is_none());
    assert!(
        version_store::list(Some(&current.card_id))
            .unwrap()
            .is_empty()
    );
}

#[test]
fn interrupted_current_purge_can_retry_without_a_marker_after_the_commit_point() {
    use std::os::unix::fs::PermissionsExt;
    let (_temp, mgr, current, card, pkg, id) = fixture();
    let key = db::add_api_key(&current.card_id, Some("retry test")).unwrap();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    mgr.delete_version(
        &current.card_id,
        &version_store::active_id(&current),
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    let root = PathBuf::from(active.models_root.as_ref().unwrap());
    let parent = root.parent().unwrap();
    fs::set_permissions(parent, fs::Permissions::from_mode(0o500)).unwrap();
    let result = mgr.uninstall(&current.card_id, true, &mut |_| {}, Default::default());
    fs::set_permissions(parent, fs::Permissions::from_mode(0o700)).unwrap();
    assert!(result.is_err());
    assert!(root.exists());
    assert!(!root.join(".pinvou-version.json").exists());
    assert_eq!(
        version_store::get(&current.card_id, &id)
            .unwrap()
            .unwrap()
            .state,
        "deleting"
    );
    assert!(!version_store::has_history(&current.card_id).unwrap());
    assert_eq!(version_store::retained_bytes().unwrap(), 0);
    let rows = mgr.versions(&current.card_id).unwrap();
    assert_eq!(rows[0]["state"], "deleting");
    assert_eq!(rows[0]["can_delete"], false);
    assert!(
        mgr.deploy(&current.card_id, None, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(
        mgr.enable(&current.card_id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(
        mgr.uninstall(&current.card_id, false, &mut |_| {}, Default::default())
            .is_err()
    );
    mgr.uninstall(&current.card_id, true, &mut |_| {}, Default::default())
        .unwrap();
    assert!(!root.exists());
    assert!(db::get_store_model(&current.card_id).unwrap().is_none());
    assert!(
        version_store::list(Some(&current.card_id))
            .unwrap()
            .is_empty()
    );
    assert!(db::find_api_key(&key.key_value).unwrap().is_none());
}

#[test]
fn a_current_version_without_a_marker_is_not_an_authorized_empty_purge() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    mgr.delete_version(
        &current.card_id,
        &version_store::active_id(&current),
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    let root = PathBuf::from(active.models_root.as_ref().unwrap());
    fs::remove_file(root.join(".pinvou-version.json")).unwrap();
    assert!(
        mgr.uninstall(&current.card_id, true, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(root.join("new/new.tachyon").exists());
    fs::remove_dir_all(root.join("new")).unwrap();
    assert!(
        mgr.uninstall(&current.card_id, true, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(root.exists());
    assert_eq!(
        version_store::get(&current.card_id, &id)
            .unwrap()
            .unwrap()
            .state,
        "current"
    );
}

#[test]
fn absent_evidence_never_turns_changed_paths_or_sizes_into_reusable_weights() {
    let (_temp, _mgr, mut current, mut card, _pkg, _id) = fixture();
    current.assets_content.clear();
    card.assets.clear();
    card.asset_sha256 = Some(BTreeMap::new());
    card.source_repo_id = current.source_repo_id.clone();
    card.source_filename = current.source_filename.clone();
    card.summary.quantization = Some(" nv-fp4 ".into());
    assert!(card_update::compatibility(&current, &card, None).can_update);
    card.source_filename = Some("old.tachyon ".into());
    assert_eq!(
        card_update::compatibility(&current, &card, None).kind,
        "unverified"
    );
    card.source_filename = current.source_filename.clone();
    card.summary.size_bytes = 12;
    assert!(!card_update::compatibility(&current, &card, None).can_update);
    card.summary.size_bytes = 11;
    card.source_repo_id = Some("other/source".into());
    assert!(!card_update::compatibility(&current, &card, None).can_update);
}

#[test]
fn clearing_a_selected_artifact_in_a_multi_file_manifest_requires_review() {
    let (_temp, _mgr, mut current, mut card, _pkg, _id) = fixture();
    let manifest = blob(&format!(
        "{}  old.tachyon\n{}  other.tachyon\n",
        "a".repeat(64),
        "b".repeat(64)
    ));
    current
        .assets_content
        .insert(weight_identity::MANIFEST.into(), manifest.clone());
    card.asset_sha256
        .as_mut()
        .unwrap()
        .insert(weight_identity::MANIFEST.into(), manifest.sha256.clone());
    for selection in [None, Some("".into()), Some("  ".into())] {
        card.source_filename = selection;
        let result = card_update::compatibility(&current, &card, Some(&manifest));
        assert!(!result.can_update);
        assert_eq!(result.reason, "artifact_selection_changed");
    }
    card.source_filename = current.source_filename.clone();
    assert!(card_update::compatibility(&current, &card, Some(&manifest)).can_update);
}

#[test]
fn switching_resolves_defaults_and_rollback_preserves_the_saved_profile() {
    let (_temp, mgr, current, mut card, mut pkg, _id) = fixture();
    let profiles = serde_json::json!([
        {"id":"default","name":"Default"}, {"id":"custom","name":"Custom"}
    ]);
    card.profiles = serde_json::from_value(profiles.clone()).unwrap();
    db::connect()
        .unwrap()
        .execute(
            "UPDATE store_models SET profiles_json=?1,active_profile_id='custom' WHERE card_id=?2",
            rusqlite::params![profiles.to_string(), current.card_id],
        )
        .unwrap();
    let mut scripts = current.scripts_content.clone();
    scripts
        .get_mut("deploy")
        .unwrap()
        .content
        .push_str("printf '%s' \"$LMM_DEPLOY_PROFILE\" > chosen-profile\n");
    scripts.get_mut("deploy").unwrap().sha256 =
        script_runner::sha256_hex(&scripts["deploy"].content);
    db::set_scripts_content(&current.card_id, &scripts, &current.assets_content).unwrap();
    let current = db::get_store_model(&current.card_id).unwrap().unwrap();
    pkg.scripts
        .get_mut("deploy")
        .unwrap()
        .content
        .push_str("printf '%s' \"$LMM_DEPLOY_PROFILE\" > chosen-profile\n");
    let deploy = pkg.scripts.get_mut("deploy").unwrap();
    deploy.sha256 = script_runner::sha256_hex(&deploy.content);
    card.script_sha256
        .as_mut()
        .unwrap()
        .insert("deploy".into(), deploy.sha256.clone());
    let id = card_update::revision(&card_update::remote_manifest(&card));
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    assert_eq!(active.active_profile_id.as_deref(), Some("default"));
    assert_eq!(
        fs::read_to_string(runtime_path(&active).unwrap().join("chosen-profile")).unwrap(),
        "default"
    );
    mgr.switch_version(
        &current.card_id,
        &version_store::active_id(&current),
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    let restored = db::get_store_model(&current.card_id).unwrap().unwrap();
    assert_eq!(restored.active_profile_id.as_deref(), Some("custom"));
    assert_eq!(
        fs::read_to_string(runtime_path(&restored).unwrap().join("chosen-profile")).unwrap(),
        "custom"
    );
}

#[test]
fn partial_recursive_cleanup_keeps_the_marker_and_can_be_retried() {
    use std::os::unix::fs::PermissionsExt;
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let mut version = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let root = PathBuf::from(version.resource_dir.as_ref().unwrap());
    let locked = root.join("locked");
    fs::create_dir(&locked).unwrap();
    fs::write(locked.join("weights"), b"partial").unwrap();
    fs::set_permissions(&locked, fs::Permissions::from_mode(0o500)).unwrap();
    version.state = "deleting".into();
    version_store::put(&version).unwrap();
    let result = remove_managed_resource(&root);
    fs::set_permissions(&locked, fs::Permissions::from_mode(0o700)).unwrap();
    assert!(result.is_err());
    assert!(root.join(".pinvou-version.json").exists());
    mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    assert!(!root.exists());
    assert!(version_store::get(&current.card_id, &id).unwrap().is_none());
}

#[test]
fn deleting_without_a_marker_accepts_only_an_empty_resource_root() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let mut version = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let root = PathBuf::from(version.resource_dir.as_ref().unwrap());
    version.state = "deleting".into();
    version_store::put(&version).unwrap();
    fs::remove_file(root.join(".pinvou-version.json")).unwrap();
    assert!(
        mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(root.join("new/new.tachyon").exists());
    fs::remove_dir_all(root.join("new")).unwrap();
    mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    assert!(!root.exists());
}

#[test]
fn new_upgrades_require_intact_old_weights_but_history_remains_a_recovery_route() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let old_file = mgr.settings.models_path().join("legacy/old.tachyon");
    for content in [Some(b"corrupt".as_slice()), None] {
        if let Some(content) = content {
            fs::write(&old_file, content).unwrap();
        } else {
            fs::remove_file(&old_file).unwrap();
        }
        assert!(
            mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
                .is_err()
        );
        assert_eq!(
            db::get_store_model(&current.card_id)
                .unwrap()
                .unwrap()
                .active_version_id,
            None
        );
        assert_eq!(
            version_store::get(&current.card_id, &id)
                .unwrap()
                .unwrap()
                .state,
            "ready"
        );
    }
    fs::write(&old_file, b"old-weights").unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    let active_root = legacy_resource_dir(&active, &mgr.settings.models_path()).unwrap();
    fs::remove_file(find_weight(&active_root, "new.tachyon").unwrap()).unwrap();
    mgr.switch_version(
        &current.card_id,
        &version_store::active_id(&current),
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    assert_eq!(
        db::get_store_model(&current.card_id)
            .unwrap()
            .unwrap()
            .source_filename,
        current.source_filename
    );
}

#[test]
fn downloaded_undeployed_cards_still_protect_shared_legacy_weights() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    db::add_store_model(db::AddStoreModelParams {
        card_id: "other-downloaded-card",
        model_name: "other",
        card_name: "Other",
        org: None,
        description: None,
        engine_name: "native",
        engine_version: Some("old"),
        size_bytes: 11,
        quantization: None,
        source_repo_id: current.source_repo_id.as_deref(),
        source_filename: current.source_filename.as_deref(),
        scripts: &Default::default(),
        status: "downloaded",
        profiles: &[],
        deploy_topology: DeployTopology::Single,
        card_source: "store",
    })
    .unwrap();
    let mut other_scripts = current.scripts_content.clone();
    other_scripts.insert("uninstall".into(), blob(
        "#!/bin/bash\nif [ \"$LMM_PURGE_WEIGHTS\" = 1 ]; then rm -rf \"$LMM_MODELS_DIR/legacy\"; fi\n"
    ));
    db::set_scripts_content(
        "other-downloaded-card",
        &other_scripts,
        &current.assets_content,
    )
    .unwrap();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    // Protect the reverse direction too: the ordinary legacy card must not
    // delete bytes retained for the upgraded card's rollback.
    assert!(
        mgr.uninstall(
            "other-downloaded-card",
            true,
            &mut |_| {},
            Default::default()
        )
        .is_err()
    );
    assert!(
        db::get_store_model("other-downloaded-card")
            .unwrap()
            .is_some()
    );
    assert!(
        mgr.settings
            .models_path()
            .join("legacy/old.tachyon")
            .exists()
    );
    assert!(
        mgr.delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(
        mgr.settings
            .models_path()
            .join("legacy/old.tachyon")
            .exists()
    );
    assert_eq!(
        version_store::get(&current.card_id, &old_id)
            .unwrap()
            .unwrap()
            .state,
        "history"
    );
    // Even a corrected source label cannot hide matching verified weight bytes.
    db::connect().unwrap().execute(
        "UPDATE store_models SET source_repo_id='corrected/alias' WHERE card_id='other-downloaded-card'", []
    ).unwrap();
    assert!(
        mgr.uninstall(
            "other-downloaded-card",
            true,
            &mut |_| {},
            Default::default()
        )
        .is_err()
    );
    assert!(
        mgr.delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(
        mgr.settings
            .models_path()
            .join("legacy/old.tachyon")
            .exists()
    );
}

#[test]
fn shared_resource_locks_allow_readers_and_exclude_reference_deletion() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let first = version_store::resource_operation_lock(false).unwrap();
    let second = version_store::resource_operation_lock(false).unwrap();
    assert!(version_store::resource_operation_lock(true).is_err());
    assert!(
        mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .unwrap_err()
            .to_string()
            .contains("资源操作")
    );
    assert!(
        mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .unwrap_err()
            .to_string()
            .contains("资源操作")
    );
    assert!(
        mgr.uninstall(&current.card_id, true, &mut |_| {}, Default::default())
            .unwrap_err()
            .to_string()
            .contains("资源操作")
    );
    drop(first);
    drop(second);
    let writer = version_store::resource_operation_lock(true).unwrap();
    assert!(version_store::resource_operation_lock(false).is_err());
    assert!(
        mgr.deploy(&current.card_id, None, &mut |_| {}, Default::default())
            .unwrap_err()
            .to_string()
            .contains("资源操作")
    );
    assert!(
        mgr.enable(&current.card_id, &mut |_| {}, Default::default())
            .unwrap_err()
            .to_string()
            .contains("资源操作")
    );
    drop(writer);
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
}

#[test]
fn rollback_reports_pinned_residuals_after_default_storage_changes() {
    let (temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let mut settings = mgr.settings.clone();
    let unrelated = temp.path().join("other-models");
    settings.models_dir = unrelated.to_string_lossy().into_owned();
    fs::create_dir_all(unrelated.join("stable-name")).unwrap();
    fs::write(unrelated.join("stable-name/unrelated"), b"keep me").unwrap();
    let relocated = ModelManager::new(Some(settings), None);
    relocated
        .switch_version(
            &current.card_id,
            &version_store::active_id(&current),
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
    relocated
        .delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let outcome = relocated
        .uninstall(&current.card_id, false, &mut |_| {}, Default::default())
        .unwrap();
    let old = mgr
        .settings
        .models_path()
        .join("legacy")
        .canonicalize()
        .unwrap()
        .to_string_lossy()
        .into_owned();
    assert!(outcome.remaining_paths.contains(&old));
    assert!(
        outcome
            .remaining_paths
            .iter()
            .all(|p| !Path::new(p).starts_with(&unrelated))
    );
    let forced = relocated.force_remove_record(&current.card_id).unwrap();
    assert!(forced.residual_paths.contains(&old));
    assert!(
        forced
            .residual_paths
            .iter()
            .all(|p| !Path::new(p).starts_with(&unrelated))
    );
    assert!(Path::new(&old).join("old.tachyon").exists());
    assert!(unrelated.join("stable-name/unrelated").exists());
}
