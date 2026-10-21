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
    let active_runtime = runtime_path(&active).unwrap();
    assert_eq!(
        fs::read_to_string(active_runtime.join(".deploy-env")).unwrap(),
        format!(
            "MODEL_DIR=\"{}\"\n",
            Path::new(staged.models_root.as_deref().unwrap())
                .join("new")
                .display()
        )
    );
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
    mgr.switch_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap();
    let restored = db::get_store_model(&current.card_id).unwrap().unwrap();
    assert_eq!(restored.source_filename, current.source_filename);
    assert_eq!(restored.engine_version, current.engine_version);
    assert_eq!(
        fs::read_to_string(runtime_path(&restored).unwrap().join(".deploy-env")).unwrap(),
        format!(
            "MODEL_DIR=\"{}/legacy\"\n",
            mgr.settings.models_path().display()
        )
    );
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
fn inaccessible_current_storage_or_runtime_keeps_records_and_can_be_retried() {
    use std::os::unix::fs::PermissionsExt;
    assert_ne!(
        fs::metadata("/proc/self").unwrap().uid(),
        0,
        "permission-denial regression must run as an unprivileged user"
    );
    for lock_runtime in [false, true] {
        let (_temp, mgr, current, card, pkg, id) = fixture();
        let key = db::add_api_key(&current.card_id, Some("permission test")).unwrap();
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
        let active = mgr.get_record(&current.card_id).unwrap();
        let root = PathBuf::from(active.models_root.as_ref().unwrap());
        let runtime = runtime_path(&active).unwrap();
        let locked = if lock_runtime {
            runtime.parent().unwrap()
        } else {
            root.parent().unwrap()
        };
        let permissions = fs::metadata(locked).unwrap().permissions();
        fs::set_permissions(locked, fs::Permissions::from_mode(0o000)).unwrap();
        let result = mgr.uninstall(&current.card_id, true, &mut |_| {}, Default::default());
        fs::set_permissions(locked, permissions).unwrap();
        assert!(
            result.is_err(),
            "inaccessible path must not authorize record-only removal"
        );
        assert_eq!(
            fs::read(root.join("new/new.tachyon")).unwrap(),
            b"new-weights"
        );
        assert!(runtime.exists());
        assert_eq!(
            serde_json::to_value(mgr.get_record(&current.card_id).unwrap()).unwrap(),
            serde_json::to_value(&active).unwrap()
        );
        assert_eq!(
            version_store::get(&current.card_id, &id)
                .unwrap()
                .unwrap()
                .state,
            "current"
        );
        assert!(db::find_api_key(&key.key_value).unwrap().is_some());
        mgr.uninstall(&current.card_id, true, &mut |_| {}, Default::default())
            .unwrap();
        assert!(!root.exists());
        assert!(!runtime.exists());
        assert!(db::get_store_model(&current.card_id).unwrap().is_none());
        assert!(
            version_store::list(Some(&current.card_id))
                .unwrap()
                .is_empty()
        );
        assert!(db::find_api_key(&key.key_value).unwrap().is_none());
    }
}

#[test]
fn inaccessible_history_runtime_does_not_delete_its_weights_or_record() {
    use std::os::unix::fs::PermissionsExt;
    assert_ne!(
        fs::metadata("/proc/self").unwrap().uid(),
        0,
        "permission-denial regression must run as an unprivileged user"
    );
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let version = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let root = PathBuf::from(version.models_root.as_ref().unwrap());
    let runtime = config::data_dir()
        .join("cards")
        .join(&version.runtime_card_id);
    let parent = runtime.parent().unwrap();
    let permissions = fs::metadata(parent).unwrap().permissions();
    fs::set_permissions(parent, fs::Permissions::from_mode(0o000)).unwrap();
    let result = mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default());
    fs::set_permissions(parent, permissions).unwrap();
    assert!(result.is_err());
    assert_eq!(
        fs::read(root.join("new/new.tachyon")).unwrap(),
        b"new-weights"
    );
    assert_eq!(
        version_store::get(&current.card_id, &id)
            .unwrap()
            .unwrap()
            .state,
        "ready"
    );
    assert!(runtime.exists());
    mgr.delete_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    assert!(!root.exists());
    assert!(!runtime.exists());
    assert!(version_store::get(&current.card_id, &id).unwrap().is_none());
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
    let cancelled = || script_runner::RunControl {
        cancel: Some(std::sync::Arc::new(std::sync::atomic::AtomicBool::new(
            true,
        ))),
        ..Default::default()
    };
    assert!(
        mgr.switch_version(&current.card_id, &id, &mut |_| {}, cancelled())
            .unwrap_err()
            .to_string()
            .contains("已取消")
    );
    assert!(
        mgr.delete_version(&current.card_id, &id, &mut |_| {}, cancelled())
            .unwrap_err()
            .to_string()
            .contains("已取消")
    );
    assert!(
        mgr.uninstall(&current.card_id, true, &mut |_| {}, cancelled())
            .unwrap_err()
            .to_string()
            .contains("已取消")
    );
    drop(first);
    drop(second);
    let writer = version_store::resource_operation_lock(true).unwrap();
    assert!(version_store::resource_operation_lock(false).is_err());
    assert!(
        mgr.deploy(&current.card_id, None, &mut |_| {}, cancelled())
            .unwrap_err()
            .to_string()
            .contains("已取消")
    );
    assert!(
        mgr.enable(&current.card_id, &mut |_| {}, cancelled())
            .unwrap_err()
            .to_string()
            .contains("已取消")
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

#[test]
fn legacy_history_cleanup_finds_nested_weights_and_preserves_unrelated_files() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    let root = mgr.settings.models_path().join("legacy");
    fs::create_dir_all(root.join("nested")).unwrap();
    fs::rename(root.join("old.tachyon"), root.join("nested/old.tachyon")).unwrap();
    fs::write(root.join("nested/keep.txt"), b"keep").unwrap();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    mgr.delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap();
    assert!(!root.join("nested/old.tachyon").exists());
    assert_eq!(fs::read(root.join("nested/keep.txt")).unwrap(), b"keep");
    assert!(
        version_store::get(&current.card_id, &old_id)
            .unwrap()
            .is_none()
    );
}

#[test]
fn legacy_missing_weight_can_clear_history_without_manual_database_repair() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    fs::remove_file(mgr.settings.models_path().join("legacy/old.tachyon")).unwrap();
    let result = mgr
        .delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap();
    assert_eq!(result["freed_bytes"], 0);
    assert!(!version_store::has_history(&current.card_id).unwrap());
    // This public recovery route must unblock normal uninstall as well.
    mgr.uninstall(&current.card_id, false, &mut |_| {}, Default::default())
        .unwrap();
    assert!(
        version_store::get(&current.card_id, &old_id)
            .unwrap()
            .is_none()
    );
}

#[test]
fn legacy_cleanup_retains_history_for_ambiguous_nested_weights() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    let root = mgr.settings.models_path().join("legacy");
    fs::create_dir(root.join("nested")).unwrap();
    fs::copy(root.join("old.tachyon"), root.join("nested/old.tachyon")).unwrap();
    assert!(
        mgr.delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert_eq!(
        version_store::get(&current.card_id, &old_id)
            .unwrap()
            .unwrap()
            .state,
        "history"
    );
    assert!(root.join("old.tachyon").exists());
    assert!(root.join("nested/old.tachyon").exists());
    fs::remove_dir_all(&root).unwrap();
    let result = mgr
        .delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap();
    assert_eq!(result["freed_bytes"], 0);
    assert!(
        version_store::get(&current.card_id, &old_id)
            .unwrap()
            .is_none()
    );
}

#[test]
fn failed_hf_candidate_requires_full_capacity_then_resumes_without_stacking_stale_plan() {
    let (_temp, mut mgr, current, card, mut pkg, id) = fixture();
    let hash = script_runner::sha256_hex("new-weights");
    let prefix = "KBv97_iyz3IQ9bCSq3sTENHhlOw=";
    pkg.scripts.insert("download_model".into(), blob(&format!(
        "#!/bin/bash\nset -e\ncache=\"$LMM_MODELS_DIR/new/.cache/huggingface/download\"\nmkdir -p \"$cache\"\nweight=\"$cache/{prefix}.{hash}.incomplete\"\nif [ -f \"$weight\" ]; then printf eights >> \"$weight\"; mv \"$weight\" \"$LMM_MODELS_DIR/new/new.tachyon\"; else printf new-w > \"$weight\"; echo '完整性校验失败' >&2; exit 1; fi\n"
    )));
    assert!(
        mgr.stage_verified(
            &current,
            &card,
            &id,
            pkg.clone(),
            &mut |_| {},
            Default::default()
        )
        .is_err()
    );
    let failed = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(failed.state, "failed");
    let root = Path::new(failed.models_root.as_deref().unwrap());
    let cache_file = root.join(format!(
        "new/.cache/huggingface/download/{prefix}.{hash}.incomplete"
    ));
    assert_eq!(fs::read(&cache_file).unwrap(), b"new-w");
    let used = db::total_managed_bytes().unwrap() as u64 + version_store::retained_bytes().unwrap();
    mgr.settings.storage_quota_gb = Some((used + 6) as f64 / 1073741824.0);
    let error = mgr
        .stage_verified(
            &current,
            &card,
            &id,
            pkg.clone(),
            &mut |_| {},
            Default::default(),
        )
        .unwrap_err();
    assert!(error.to_string().contains("超出存储配额"), "{error}");
    assert_eq!(fs::read(&cache_file).unwrap(), b"new-w");
    assert!(!root.join("new/new.tachyon").exists());
    let rejected = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(rejected.state, "failed");
    assert_eq!(rejected.planned_bytes, 0);
    assert_eq!(
        db::total_managed_bytes().unwrap() as u64 + version_store::retained_bytes().unwrap(),
        used
    );
    // Simulate a crash before interruption recovery. The stale plan is replaced
    // rather than added to the full new reservation; the script still performs
    // actual HF continuation with its side effects intact.
    let mut interrupted = failed.clone();
    interrupted.state = "downloading".into();
    interrupted.planned_bytes = used + 1024;
    version_store::put(&interrupted).unwrap();
    mgr.settings.storage_quota_gb = Some((used + card.summary.size_bytes) as f64 / 1073741824.0);
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let ready = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(ready.state, "ready");
    assert_eq!(ready.planned_bytes, 0);
    assert_eq!(
        fs::read(root.join("new/new.tachyon")).unwrap(),
        b"new-weights"
    );
}

#[test]
fn parallel_stages_reserve_quota_before_scripts_and_release_after_failure() {
    use std::sync::mpsc;
    let (_temp, mut mgr, current, mut card, pkg, _) = fixture();
    // Leave enough quota for either candidate, but not both. A stopped script
    // has not written any weights yet, so only its reservation can exclude B.
    card.summary.size_bytes = 64 * 1024;
    let id = card_update::revision(&card_update::remote_manifest(&card));
    let mut other = current.clone();
    other.card_id = "parallel-version".into();
    other.model_name = "parallel-model".into();
    db::add_store_model(db::AddStoreModelParams {
        card_id: &other.card_id,
        model_name: &other.model_name,
        card_name: &other.card_name,
        org: other.org.as_deref(),
        description: other.description.as_deref(),
        engine_name: &other.engine_name,
        engine_version: other.engine_version.as_deref(),
        size_bytes: other.size_bytes,
        quantization: other.quantization.as_deref(),
        source_repo_id: other.source_repo_id.as_deref(),
        source_filename: other.source_filename.as_deref(),
        scripts: &Default::default(),
        status: &other.status,
        profiles: &other.profiles,
        deploy_topology: other.deploy_topology,
        card_source: &other.card_source,
    })
    .unwrap();
    let mut other_card = card.clone();
    other_card.summary.id = other.card_id.clone();
    let other_id = card_update::revision(&card_update::remote_manifest(&other_card));
    mgr.settings.storage_quota_gb = Some(96.0 * 1024.0 / 1073741824.0);
    let settings = mgr.settings.clone();
    let first = current.clone();
    let first_id = id.clone();
    let mut failed_pkg = pkg.clone();
    failed_pkg.scripts.insert(
        "download_model".into(),
        blob("#!/bin/bash\necho '完整性校验失败' >&2\nexit 1\n"),
    );
    let (entered_tx, entered_rx) = mpsc::channel();
    let (release_tx, release_rx) = mpsc::channel();
    let first_thread = std::thread::spawn(move || {
        let manager = ModelManager::new(Some(settings), None);
        manager.stage_verified(
            &first,
            &card,
            &first_id,
            failed_pkg,
            &mut move |line| {
                if line.starts_with("下载并校验新版卡片") {
                    entered_tx.send(()).unwrap();
                    release_rx.recv_timeout(Duration::from_secs(10)).unwrap();
                }
            },
            Default::default(),
        )
    });
    entered_rx.recv_timeout(Duration::from_secs(10)).unwrap();
    let during = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let actual = super::super::super::disk::dir_size_bytes(Path::new(
        during.models_root.as_deref().unwrap(),
    ));
    let retained = version_store::retained_bytes().unwrap();
    let second = mgr.stage_verified(
        &other,
        &other_card,
        &other_id,
        pkg.clone(),
        &mut |_| {},
        Default::default(),
    );
    release_tx.send(()).unwrap();
    let first_result = first_thread.join().unwrap();
    assert_eq!(during.state, "downloading");
    assert!(during.planned_bytes >= 64 * 1024);
    assert!(actual < during.planned_bytes);
    assert_eq!(retained, during.planned_bytes);
    assert!(second.unwrap_err().to_string().contains("配额"));
    assert!(
        version_store::get(&other.card_id, &other_id)
            .unwrap()
            .is_none()
    );
    assert!(first_result.is_err());
    let failed = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(failed.state, "failed");
    assert_eq!(failed.planned_bytes, 0);
    assert_eq!(version_store::retained_bytes().unwrap(), actual);
    mgr.stage_verified(
        &other,
        &other_card,
        &other_id,
        pkg,
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
}

#[test]
fn interrupted_download_releases_reservation_but_keeps_actual_files_charged() {
    let (_temp, mgr, current, card, mut pkg, id) = fixture();
    pkg.scripts.insert(
        "download_model".into(),
        blob("#!/bin/bash\nprintf part > \"$LMM_MODELS_DIR/new.tachyon\"\necho '完整性校验失败' >&2\nexit 1\n"),
    );
    assert!(
        mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
            .is_err()
    );
    let mut interrupted = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let root = Path::new(interrupted.models_root.as_deref().unwrap());
    let actual = super::super::super::disk::dir_size_bytes(root);
    assert_eq!(interrupted.planned_bytes, 0);
    assert_eq!(version_store::retained_bytes().unwrap(), actual);
    interrupted.state = "downloading".into();
    interrupted.planned_bytes = actual + 1000;
    version_store::put(&interrupted).unwrap();
    assert_eq!(version_store::retained_bytes().unwrap(), actual + 1000);
    version_store::recover_interrupted(&current.card_id).unwrap();
    let recovered = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(recovered.state, "failed");
    assert_eq!(recovered.planned_bytes, 0);
    assert_eq!(version_store::retained_bytes().unwrap(), actual);
    assert_eq!(fs::read(root.join("new.tachyon")).unwrap(), b"part");
}

#[test]
fn cancelling_quota_reservation_wait_never_registers_a_candidate() {
    use std::sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    };
    let (_temp, mgr, current, card, pkg, id) = fixture();
    let _lock = version_store::wait_quota_reservation_lock(|| false, || {}).unwrap();
    let flag = Arc::new(AtomicBool::new(false));
    let control = script_runner::RunControl {
        cancel: Some(flag.clone()),
        ..Default::default()
    };
    let result = mgr.stage_verified(
        &current,
        &card,
        &id,
        pkg,
        &mut |line| {
            if line.contains("登记存储配额") {
                flag.store(true, Ordering::SeqCst);
            }
        },
        control,
    );
    assert!(result.unwrap_err().to_string().contains("已取消"));
    assert!(
        version_store::list(Some(&current.card_id))
            .unwrap()
            .is_empty()
    );
    assert!(!mgr.settings.models_path().join(".versions").exists());
}

#[test]
fn failed_candidate_keeps_registered_directory_after_default_storage_changes() {
    let (temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(
        &current,
        &card,
        &id,
        pkg.clone(),
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    let next_id = "a".repeat(64);
    let mut failed_pkg = pkg.clone();
    failed_pkg.scripts.insert("download_model".into(), blob("#!/bin/bash\nmkdir -p \"$LMM_MODELS_DIR/new\"\nprintf new-w > \"$LMM_MODELS_DIR/new/new.tachyon\"\necho '完整性校验失败' >&2\nexit 1\n"));
    assert!(
        mgr.stage_verified(
            &active,
            &card,
            &next_id,
            failed_pkg,
            &mut |_| {},
            Default::default()
        )
        .is_err()
    );
    let before = version_store::get(&current.card_id, &next_id)
        .unwrap()
        .unwrap();
    let old_root = PathBuf::from(before.models_root.as_deref().unwrap());
    let mut settings = mgr.settings.clone();
    let new_default = temp.path().join("new-default");
    fs::create_dir_all(&new_default).unwrap();
    settings.models_dir = new_default.to_string_lossy().into_owned();
    let relocated = ModelManager::new(Some(settings), None);
    relocated
        .stage_verified(
            &active,
            &card,
            &next_id,
            pkg,
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
    let after = version_store::get(&current.card_id, &next_id)
        .unwrap()
        .unwrap();
    assert_eq!(after.models_root, before.models_root);
    assert_eq!(after.resource_dir, before.resource_dir);
    assert_eq!(after.runtime_card_id, before.runtime_card_id);
    assert_eq!(after.created_at, before.created_at);
    assert_eq!(
        fs::read(old_root.join("new/new.tachyon")).unwrap(),
        b"new-weights"
    );
    assert!(!new_default.join(".versions").exists());
    relocated
        .delete_version(&current.card_id, &next_id, &mut |_| {}, Default::default())
        .unwrap();
    assert!(
        !old_root.exists(),
        "the original partial directory remains tracked for cleanup"
    );
}

#[test]
fn retry_rechecks_permissions_of_both_managed_ancestors() {
    use std::os::unix::fs::PermissionsExt;
    let (_temp, mgr, current, card, mut pkg, id) = fixture();
    let successful_pkg = pkg.clone();
    pkg.scripts.insert(
        "download_model".into(),
        blob("#!/bin/bash\nprintf partial > \"$LMM_MODELS_DIR/new.tachyon\"\necho '完整性校验失败' >&2\nexit 1\n"),
    );
    assert!(
        mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
            .is_err()
    );
    let before = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let root = PathBuf::from(before.models_root.as_deref().unwrap());
    for ancestor in root.ancestors().skip(1).take(2) {
        let permissions = fs::metadata(ancestor).unwrap().permissions();
        fs::set_permissions(ancestor, fs::Permissions::from_mode(0o770)).unwrap();
        let result = mgr.stage_verified(
            &current,
            &card,
            &id,
            successful_pkg.clone(),
            &mut |_| {},
            Default::default(),
        );
        fs::set_permissions(ancestor, permissions).unwrap();
        assert!(result.unwrap_err().to_string().contains("归属"));
        assert_eq!(
            serde_json::to_value(version_store::get(&current.card_id, &id).unwrap().unwrap())
                .unwrap(),
            serde_json::to_value(&before).unwrap()
        );
        assert_eq!(fs::read(root.join("new.tachyon")).unwrap(), b"partial");
    }
    // Remove the deliberate partial at a different layout before the successful
    // recipe creates its own file, so final verification remains unambiguous.
    fs::remove_file(root.join("new.tachyon")).unwrap();
    mgr.stage_verified(
        &current,
        &card,
        &id,
        successful_pkg,
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    assert_eq!(
        version_store::get(&current.card_id, &id)
            .unwrap()
            .unwrap()
            .state,
        "ready"
    );
}

#[test]
fn retry_rejects_replaced_marker_without_losing_registered_resources() {
    let (_temp, mgr, current, card, mut pkg, id) = fixture();
    pkg.scripts.insert(
        "download_model".into(),
        blob("#!/bin/bash\nprintf partial > \"$LMM_MODELS_DIR/new.tachyon\"\necho '完整性校验失败' >&2\nexit 1\n"),
    );
    assert!(
        mgr.stage_verified(
            &current,
            &card,
            &id,
            pkg.clone(),
            &mut |_| {},
            Default::default()
        )
        .is_err()
    );
    let before = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let root = Path::new(before.models_root.as_deref().unwrap());
    fs::write(root.join(".pinvou-version.json"), b"foreign owner").unwrap();
    assert!(
        mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
            .unwrap_err()
            .to_string()
            .contains("归属")
    );
    let after = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(after.models_root, before.models_root);
    assert_eq!(after.state, "failed");
    assert_eq!(fs::read(root.join("new.tachyon")).unwrap(), b"partial");
}

#[test]
fn missing_legacy_directory_link_is_not_treated_as_absent() {
    use std::os::unix::fs::symlink;
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    let root = mgr.settings.models_path().join("legacy");
    fs::remove_dir_all(&root).unwrap();
    symlink(root.join("missing-target"), &root).unwrap();
    assert!(
        mgr.delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(
        version_store::get(&current.card_id, &old_id)
            .unwrap()
            .is_some()
    );
}

#[test]
fn legacy_absence_cannot_be_proven_through_a_linked_subdirectory() {
    use std::os::unix::fs::symlink;
    let (temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    let root = mgr.settings.models_path().join("legacy");
    let external = temp.path().join("external-weight");
    fs::create_dir(&external).unwrap();
    fs::rename(root.join("old.tachyon"), external.join("old.tachyon")).unwrap();
    symlink(&external, root.join("nested")).unwrap();
    assert!(
        mgr.delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
            .is_err()
    );
    assert!(
        version_store::get(&current.card_id, &old_id)
            .unwrap()
            .is_some()
    );
    assert_eq!(
        fs::read(external.join("old.tachyon")).unwrap(),
        b"old-weights"
    );
}

#[test]
fn new_candidate_can_create_a_new_default_root_while_current_version_stays_pinned() {
    let (temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(
        &current,
        &card,
        &id,
        pkg.clone(),
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    let mut settings = mgr.settings.clone();
    let next_root = temp.path().join("not-created-yet/nested/models");
    settings.models_dir = next_root.to_string_lossy().into_owned();
    assert!(!next_root.exists());
    let relocated = ModelManager::new(Some(settings), None);
    let next_id = "b".repeat(64);
    relocated
        .stage_verified(
            &active,
            &card,
            &next_id,
            pkg,
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
    let prepared = version_store::get(&current.card_id, &next_id)
        .unwrap()
        .unwrap();
    assert!(Path::new(prepared.models_root.as_deref().unwrap()).starts_with(&next_root));
    assert_eq!(
        db::get_store_model(&current.card_id)
            .unwrap()
            .unwrap()
            .models_root,
        active.models_root
    );
}

#[test]
fn isolated_version_residuals_include_its_runtime_directory() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let active = mgr.get_record(&current.card_id).unwrap();
    let runtime = runtime_path(&active).unwrap();
    assert!(runtime.exists());
    let residuals = mgr.residual_paths(&active);
    assert!(residuals.contains(&runtime.to_string_lossy().into_owned()));
    assert!(residuals.contains(active.models_root.as_ref().unwrap()));
}

#[test]
fn managed_residuals_remain_charged_after_retry_activation_and_archival() {
    let (_temp, mut mgr, current, card, pkg, id) = fixture();
    let mut failed_pkg = pkg.clone();
    failed_pkg.scripts.insert(
        "download_model".into(),
        blob(
            "#!/bin/bash\nset -e\nmkdir -p \"$LMM_MODELS_DIR/new\"\nprintf new-w > \"$LMM_MODELS_DIR/new/new.tachyon\"\nhead -c 65536 /dev/zero > \"$LMM_MODELS_DIR/unknown.part\"\necho '完整性校验失败' >&2\nexit 1\n",
        ),
    );
    assert!(
        mgr.stage_verified(
            &current,
            &card,
            &id,
            failed_pkg,
            &mut |_| {},
            Default::default()
        )
        .is_err()
    );
    let failed = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let root = PathBuf::from(failed.models_root.as_deref().unwrap());
    let actual = super::super::super::disk::dir_size_bytes(&root);
    assert_eq!(failed.state, "failed");
    assert_eq!(failed.planned_bytes, 0);
    assert_eq!(version_store::retained_bytes().unwrap(), actual);

    mgr.stage_verified(
        &current,
        &card,
        &id,
        pkg.clone(),
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    let ready = version_store::get(&current.card_id, &id).unwrap().unwrap();
    let actual = super::super::super::disk::dir_size_bytes(&root);
    assert_eq!(ready.state, "ready");
    assert_eq!(ready.planned_bytes, 0);
    assert_eq!(
        fs::metadata(root.join("unknown.part")).unwrap().len(),
        65536
    );
    assert!(actual > card.summary.size_bytes + 65536);
    assert_eq!(version_store::retained_bytes().unwrap(), actual);

    // Removing only the residual's charge would admit the next candidate. It
    // must remain excluded in ready, current and history states alike.
    let check_next_stage = |manager: &mut ModelManager, active: &db::StoreModelRecord| {
        let charged =
            db::total_managed_bytes().unwrap() as u64 + version_store::retained_bytes().unwrap();
        manager.settings.storage_quota_gb = Some((charged - 32768) as f64 / 1073741824.0);
        let next_id = "d".repeat(64);
        let error = manager
            .stage_verified(
                active,
                &card,
                &next_id,
                pkg.clone(),
                &mut |_| {},
                Default::default(),
            )
            .unwrap_err();
        assert!(error.to_string().contains("配额"));
        assert!(
            version_store::get(&current.card_id, &next_id)
                .unwrap()
                .is_none()
        );
    };
    check_next_stage(&mut mgr, &current);

    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    let current_version = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(current_version.state, "current");
    assert_eq!(current_version.planned_bytes, 0);
    let legacy_bytes = current.size_bytes as u64;
    assert_eq!(
        version_store::retained_bytes().unwrap(),
        legacy_bytes + actual.saturating_sub(active.size_bytes as u64)
    );
    check_next_stage(&mut mgr, &active);
    let mut shared = current_version.clone();
    shared.card_id = "shared-resource-record".into();
    shared.version_id = "e".repeat(64);
    shared.state = "history".into();
    version_store::put(&shared).unwrap();
    assert_eq!(
        version_store::retained_bytes().unwrap(),
        legacy_bytes + actual.saturating_sub(active.size_bytes as u64),
        "a retained reference and the current generation charge one resource"
    );
    version_store::remove(&shared.card_id, &shared.version_id).unwrap();

    mgr.switch_version(
        &current.card_id,
        &version_store::active_id(&current),
        &mut |_| {},
        Default::default(),
    )
    .unwrap();
    let active = db::get_store_model(&current.card_id).unwrap().unwrap();
    let history = version_store::get(&current.card_id, &id).unwrap().unwrap();
    assert_eq!(history.state, "history");
    assert_eq!(history.planned_bytes, 0);
    assert_eq!(version_store::retained_bytes().unwrap(), actual);
    check_next_stage(&mut mgr, &active);
    assert_eq!(
        fs::metadata(root.join("unknown.part")).unwrap().len(),
        65536
    );
}

#[test]
fn unreadable_residuals_block_quota_admission_in_every_retained_state() {
    use std::os::unix::fs::PermissionsExt;
    assert_ne!(
        fs::metadata("/proc/self").unwrap().uid(),
        0,
        "permission-denial regression must run as an unprivileged user"
    );
    for state in ["failed", "ready", "current", "history"] {
        let (_temp, mut mgr, current, card, pkg, id) = fixture();
        mgr.stage_verified(
            &current,
            &card,
            &id,
            pkg.clone(),
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
        if ["current", "history"].contains(&state) {
            mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
                .unwrap();
        }
        if state == "history" {
            mgr.switch_version(
                &current.card_id,
                &version_store::active_id(&current),
                &mut |_| {},
                Default::default(),
            )
            .unwrap();
        }
        let mut retained = version_store::get(&current.card_id, &id).unwrap().unwrap();
        if state == "failed" {
            retained.state = state.into();
            retained.record = None;
            version_store::put(&retained).unwrap();
        }
        assert_eq!(retained.state, state);
        let root = PathBuf::from(retained.models_root.as_deref().unwrap());
        let residual = root.join("residual");
        fs::create_dir(&residual).unwrap();
        let partial = residual.join("partial.bin");
        fs::write(&partial, vec![0; 65536]).unwrap();
        // Counting while holding the existing reservation lock must not take
        // that lock recursively. The admission caller owns synchronization.
        let reservation = version_store::wait_quota_reservation_lock(|| false, || {}).unwrap();
        let charged =
            db::total_managed_bytes().unwrap() as u64 + version_store::retained_bytes().unwrap();
        drop(reservation);
        mgr.settings.storage_quota_gb = Some((charged - 32768) as f64 / 1073741824.0);
        let active = db::get_store_model(&current.card_id).unwrap().unwrap();
        let next_id = "f".repeat(64);
        let permissions = fs::metadata(&residual).unwrap().permissions();
        fs::set_permissions(&residual, fs::Permissions::from_mode(0o000)).unwrap();
        let measurement = version_store::retained_bytes();
        let replacing = version_store::retained_bytes_without_reservation(&current.card_id, &id);
        let blocked = mgr.stage_verified(
            &active,
            &card,
            &next_id,
            pkg.clone(),
            &mut |_| {},
            Default::default(),
        );
        let retry = (state == "failed").then(|| {
            mgr.stage_verified(
                &active,
                &card,
                &id,
                pkg.clone(),
                &mut |_| {},
                Default::default(),
            )
        });
        fs::set_permissions(&residual, permissions).unwrap();
        assert!(measurement.is_err(), "{state} hid an unreadable residual");
        assert!(
            replacing.is_err(),
            "{state} retry hid an unreadable residual"
        );
        assert!(blocked.unwrap_err().to_string().contains("无法核算"));
        if let Some(retry) = retry {
            assert!(retry.unwrap_err().to_string().contains("无法核算"));
        }
        // Restoring access makes the scan possible, but the retained bytes
        // still prevent a second stage from fitting into the configured quota.
        assert_eq!(
            db::total_managed_bytes().unwrap() as u64 + version_store::retained_bytes().unwrap(),
            charged
        );
        let error = mgr
            .stage_verified(
                &active,
                &card,
                &next_id,
                pkg,
                &mut |_| {},
                Default::default(),
            )
            .unwrap_err();
        assert!(error.to_string().contains("配额"));
        assert!(
            version_store::get(&current.card_id, &next_id)
                .unwrap()
                .is_none()
        );
        assert_eq!(fs::metadata(&partial).unwrap().len(), 65536);
        assert_eq!(
            serde_json::to_value(version_store::get(&current.card_id, &id).unwrap().unwrap())
                .unwrap(),
            serde_json::to_value(&retained).unwrap()
        );
    }
}

#[test]
fn managed_switch_rechecks_registered_directory_ownership() {
    use std::os::unix::fs::{PermissionsExt, symlink};
    for mutation in [
        "marker",
        "root_link",
        "card_permissions",
        "versions_permissions",
        "resource_record",
        "record_root",
    ] {
        let (temp, mgr, current, card, pkg, id) = fixture();
        mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
            .unwrap();
        let target = version_store::get(&current.card_id, &id).unwrap().unwrap();
        let root = PathBuf::from(target.models_root.as_deref().unwrap());
        let marker = root.join(".pinvou-version.json");
        let saved_marker = fs::read(&marker).unwrap();
        let moved = temp.path().join("moved-ready-version");
        let changed_parent = match mutation {
            "card_permissions" => Some(root.parent().unwrap()),
            "versions_permissions" => Some(root.parent().unwrap().parent().unwrap()),
            _ => None,
        };
        let saved_permissions = changed_parent.map(|p| fs::metadata(p).unwrap().permissions());
        match mutation {
            "marker" => fs::write(&marker, "wrong-owner").unwrap(),
            "root_link" => {
                fs::rename(&root, &moved).unwrap();
                symlink(&moved, &root).unwrap();
            }
            "resource_record" | "record_root" => {
                let mut changed = target.clone();
                if mutation == "resource_record" {
                    changed.resource_dir = Some(temp.path().join("other").display().to_string());
                } else {
                    changed.record.as_mut().unwrap().models_root =
                        Some(temp.path().join("other").display().to_string());
                }
                version_store::put(&changed).unwrap();
            }
            _ => fs::set_permissions(changed_parent.unwrap(), fs::Permissions::from_mode(0o770))
                .unwrap(),
        }
        let before_switch = version_store::get(&current.card_id, &id).unwrap().unwrap();
        let result = mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default());
        assert!(result.is_err(), "managed target accepted {mutation}");
        assert_eq!(
            serde_json::to_value(db::get_store_model(&current.card_id).unwrap().unwrap()).unwrap(),
            serde_json::to_value(&current).unwrap(),
            "rejected target must leave the active deployment unchanged"
        );
        assert_eq!(
            serde_json::to_value(version_store::get(&current.card_id, &id).unwrap().unwrap())
                .unwrap(),
            serde_json::to_value(&before_switch).unwrap(),
            "rejected target must keep its complete version record"
        );
        assert_eq!(
            fs::read(root.join("new/new.tachyon")).unwrap(),
            b"new-weights"
        );
        match mutation {
            "marker" => fs::write(&marker, saved_marker).unwrap(),
            "root_link" => {
                fs::remove_file(&root).unwrap();
                fs::rename(moved, &root).unwrap();
            }
            "resource_record" | "record_root" => version_store::put(&target).unwrap(),
            _ => fs::set_permissions(changed_parent.unwrap(), saved_permissions.unwrap()).unwrap(),
        }
        mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .unwrap();
        mgr.switch_version(
            &current.card_id,
            &version_store::active_id(&current),
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
    }
}

#[test]
fn new_upgrade_requires_current_managed_ownership_but_history_can_recover() {
    use std::os::unix::fs::{PermissionsExt, symlink};
    for mutation in [
        "marker",
        "root_link",
        "card_permissions",
        "versions_permissions",
    ] {
        let (temp, mut mgr, current, mut card, mut pkg, id) = fixture();
        mgr.stage_verified(
            &current,
            &card,
            &id,
            pkg.clone(),
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
        mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .unwrap();
        let active = mgr.get_record(&current.card_id).unwrap();
        let root = PathBuf::from(active.models_root.as_deref().unwrap());

        // Put B on a separate root so changing A's ancestors cannot merely
        // trigger the target's ownership guard instead of the current guard.
        mgr.settings.models_dir = temp.path().join("second-models").display().to_string();
        pkg.scripts.insert(
            "download_model".into(),
            blob("#!/bin/bash\nset -e\nmkdir -p \"$LMM_MODELS_DIR/new\"\nprintf third-weights > \"$LMM_MODELS_DIR/new/new.tachyon\"\n"),
        );
        pkg.assets.insert(
            "weights.sha256".into(),
            blob(&format!(
                "{}  new.tachyon\n",
                script_runner::sha256_hex("third-weights")
            )),
        );
        card.summary.size_bytes = 13;
        card.script_sha256 = Some(
            pkg.scripts
                .iter()
                .map(|(k, v)| (k.clone(), v.sha256.clone()))
                .collect(),
        );
        card.asset_sha256 = Some(
            pkg.assets
                .iter()
                .map(|(k, v)| (k.clone(), v.sha256.clone()))
                .collect(),
        );
        let next_id = card_update::revision(&card_update::remote_manifest(&card));
        mgr.stage_verified(
            &active,
            &card,
            &next_id,
            pkg,
            &mut |_| {},
            Default::default(),
        )
        .unwrap();
        let ready = version_store::get(&current.card_id, &next_id)
            .unwrap()
            .unwrap();
        let marker = root.join(".pinvou-version.json");
        let saved_marker = fs::read(&marker).unwrap();
        let moved = temp.path().join("moved-current-version");
        let changed_parent = match mutation {
            "card_permissions" => Some(root.parent().unwrap()),
            "versions_permissions" => Some(root.parent().unwrap().parent().unwrap()),
            _ => None,
        };
        let saved_permissions = changed_parent.map(|p| fs::metadata(p).unwrap().permissions());
        match mutation {
            "marker" => fs::write(&marker, "wrong-owner").unwrap(),
            "root_link" => {
                fs::rename(&root, &moved).unwrap();
                symlink(&moved, &root).unwrap();
            }
            _ => fs::set_permissions(changed_parent.unwrap(), fs::Permissions::from_mode(0o770))
                .unwrap(),
        }
        let result =
            mgr.switch_version(&current.card_id, &next_id, &mut |_| {}, Default::default());
        assert!(
            result.is_err(),
            "upgrade accepted unsafe current {mutation}"
        );
        assert_eq!(
            serde_json::to_value(mgr.get_record(&current.card_id).unwrap()).unwrap(),
            serde_json::to_value(&active).unwrap(),
        );
        assert_eq!(
            serde_json::to_value(
                version_store::get(&current.card_id, &next_id)
                    .unwrap()
                    .unwrap()
            )
            .unwrap(),
            serde_json::to_value(&ready).unwrap(),
        );
        match mutation {
            "marker" => fs::write(&marker, saved_marker).unwrap(),
            "root_link" => {
                fs::remove_file(&root).unwrap();
                fs::rename(moved, &root).unwrap();
            }
            _ => fs::set_permissions(changed_parent.unwrap(), saved_permissions.unwrap()).unwrap(),
        }
        mgr.switch_version(&current.card_id, &next_id, &mut |_| {}, Default::default())
            .unwrap();

        // A validated history target remains an escape route when the active
        // generation's ownership has subsequently become invalid.
        fs::write(
            Path::new(ready.models_root.as_deref().unwrap()).join(".pinvou-version.json"),
            "wrong-owner",
        )
        .unwrap();
        mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
            .unwrap();
        assert_eq!(
            version_store::active_id(&mgr.get_record(&current.card_id).unwrap()),
            id
        );
    }
}

#[test]
fn changed_legacy_history_has_manual_recovery_and_explicit_record_only_escape() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    let key = db::add_api_key(&current.card_id, Some("escape")).unwrap();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    mgr.switch_version(&current.card_id, &id, &mut |_| {}, Default::default())
        .unwrap();
    let old_id = version_store::active_id(&current);
    let damaged = mgr.settings.models_path().join("legacy/old.tachyon");
    fs::write(&damaged, b"externally changed").unwrap();
    let error = mgr
        .delete_version(&current.card_id, &old_id, &mut |_| {}, Default::default())
        .unwrap_err();
    assert!(error.to_string().contains("手动移走或删除"));
    assert!(error.to_string().contains("只移除记录"));
    let staged = version_store::get(&current.card_id, &id).unwrap().unwrap();
    // No global exclusive lock is needed: abandoning metadata never touches a
    // resource another card's long-running shared operation is using.
    let _other_download = version_store::resource_operation_lock(false).unwrap();
    let outcome = mgr.force_remove_record(&current.card_id).unwrap();
    assert!(db::get_store_model(&current.card_id).unwrap().is_none());
    assert!(
        version_store::list(Some(&current.card_id))
            .unwrap()
            .is_empty()
    );
    assert!(db::find_api_key(&key.key_value).unwrap().is_none());
    assert_eq!(fs::read(&damaged).unwrap(), b"externally changed");
    assert!(
        Path::new(staged.models_root.as_deref().unwrap())
            .join("new/new.tachyon")
            .exists()
    );
    assert!(
        outcome
            .residual_paths
            .contains(&mgr.settings.models_path().to_string_lossy().into_owned())
    );
    assert!(
        outcome
            .residual_paths
            .contains(staged.models_root.as_ref().unwrap())
    );
    assert!(outcome.warning.contains("没有被删除"));
}

#[test]
fn abandoning_history_rolls_back_model_versions_and_credentials_together() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
    let key = db::add_api_key(&current.card_id, Some("rollback")).unwrap();
    mgr.stage_verified(&current, &card, &id, pkg, &mut |_| {}, Default::default())
        .unwrap();
    let connection = db::connect().unwrap();
    connection.execute_batch("CREATE TRIGGER refuse_credential_delete BEFORE DELETE ON api_keys BEGIN SELECT RAISE(ABORT,'injected'); END;").unwrap();
    assert!(mgr.force_remove_record(&current.card_id).is_err());
    assert!(db::get_store_model(&current.card_id).unwrap().is_some());
    assert!(version_store::get(&current.card_id, &id).unwrap().is_some());
    assert!(db::find_api_key(&key.key_value).unwrap().is_some());
    assert!(
        mgr.settings
            .models_path()
            .join("legacy/old.tachyon")
            .exists()
    );
}

#[test]
fn weight_hash_reports_file_bytes_and_remains_cancellable_before_reading() {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("large.tachyon");
    fs::write(&path, vec![b'x'; 2 * 1024 * 1024]).unwrap();
    let mut progress = Vec::new();
    let digest = file_digest(&path, &Default::default(), &mut |line| {
        progress.push(line.to_owned())
    })
    .unwrap();
    assert_eq!(
        digest,
        script_runner::sha256_hex(&"x".repeat(2 * 1024 * 1024))
    );
    assert!(progress.first().unwrap().contains("0 / 2097152"));
    assert!(progress.last().unwrap().contains("2097152 / 2097152"));
    assert!(progress.last().unwrap().contains("100.0%"));
    let cancel = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let control = script_runner::RunControl {
        cancel: Some(cancel.clone()),
        prompt: None,
    };
    let error = file_digest(&path, &control, &mut |_| {
        cancel.store(true, std::sync::atomic::Ordering::SeqCst)
    })
    .unwrap_err();
    assert!(error.to_string().contains("已取消"));
}

#[test]
fn ordinary_download_reservation_blocks_overlapping_stage_until_released() {
    let (_temp, mut manager, current, mut card, package, _) = fixture();
    card.summary.size_bytes = 64 * 1024;
    let revision = card_update::revision(&card_update::remote_manifest(&card));
    manager.settings.storage_quota_gb = Some(100.0 * 1024.0 / 1073741824.0);
    let ordinary = version_store::DownloadReservation::acquire(
        "ordinary",
        &manager.settings.models_path(),
        || false,
        || {},
    )
    .unwrap();
    ordinary.publish(80 * 1024).unwrap();
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
    drop(ordinary);
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
}

#[test]
fn relocated_ordinary_weight_backup_cannot_reduce_stage_retry_reservation() {
    let (_temp, mut manager, current, card, mut package, revision) = fixture();
    // The script itself is unchanged between attempts. A user moves its failed
    // ordinary output aside as evidence, so the next run would write a new copy.
    package.scripts.insert("download_model".into(), blob(
        "#!/bin/bash\nset -e\nmkdir -p \"$LMM_MODELS_DIR/new\"\nprintf ran >> \"$LMM_MODELS_DIR/attempts\"\nprintf bad-weights > \"$LMM_MODELS_DIR/new/new.tachyon\"\necho '完整性校验失败' >&2\nexit 1\n",
    ));
    assert!(
        manager
            .stage_verified(
                &current,
                &card,
                &revision,
                package.clone(),
                &mut |_| {},
                Default::default()
            )
            .is_err()
    );
    let failed = version_store::get(&current.card_id, &revision)
        .unwrap()
        .unwrap();
    let root = Path::new(failed.models_root.as_deref().unwrap());
    fs::create_dir(root.join("backup")).unwrap();
    fs::rename(
        root.join("new/new.tachyon"),
        root.join("backup/new.tachyon"),
    )
    .unwrap();
    let used = db::total_managed_bytes().unwrap() as u64 + version_store::retained_bytes().unwrap();
    manager.settings.storage_quota_gb = Some(used as f64 / 1073741824.0);
    let error = manager
        .stage_verified(
            &current,
            &card,
            &revision,
            package,
            &mut |_| {},
            Default::default(),
        )
        .unwrap_err();
    assert!(error.to_string().contains("超出存储配额"), "{error}");
    assert_eq!(fs::read(root.join("attempts")).unwrap(), b"ran");
    assert_eq!(
        fs::read(root.join("backup/new.tachyon")).unwrap(),
        b"bad-weights"
    );
    assert!(!root.join("new/new.tachyon").exists());
    assert_eq!(
        version_store::get(&current.card_id, &revision)
            .unwrap()
            .unwrap()
            .state,
        "failed"
    );
}

#[test]
fn relocated_hf_cache_backup_cannot_reduce_full_retry_reservation() {
    let (_temp, mut manager, current, card, mut package, revision) = fixture();
    let hash = script_runner::sha256_hex("new-weights");
    let prefix = "KBv97_iyz3IQ9bCSq3sTENHhlOw=";
    package.scripts.insert("download_model".into(), blob(&format!(
        "#!/bin/bash\nset -e\ncache=\"$LMM_MODELS_DIR/new/.cache/huggingface/download\"\nmkdir -p \"$cache\"\nprintf ran >> \"$LMM_MODELS_DIR/attempts\"\nweight=\"$cache/{prefix}.{hash}.incomplete\"\nif [ -f \"$weight\" ]; then printf eights >> \"$weight\"; mv \"$weight\" \"$LMM_MODELS_DIR/new/new.tachyon\"; else printf new-w > \"$weight\"; echo '完整性校验失败' >&2; exit 1; fi\n"
    )));
    assert!(
        manager
            .stage_verified(
                &current,
                &card,
                &revision,
                package.clone(),
                &mut |_| {},
                Default::default()
            )
            .is_err()
    );
    let failed = version_store::get(&current.card_id, &revision)
        .unwrap()
        .unwrap();
    let root = Path::new(failed.models_root.as_deref().unwrap());
    fs::create_dir(root.join("backup")).unwrap();
    fs::rename(root.join("new/.cache"), root.join("backup/.cache")).unwrap();
    let cached = root.join(format!(
        "backup/.cache/huggingface/download/{prefix}.{hash}.incomplete"
    ));
    let used = db::total_managed_bytes().unwrap() as u64 + version_store::retained_bytes().unwrap();
    manager.settings.storage_quota_gb = Some((used + 6) as f64 / 1073741824.0);
    let error = manager
        .stage_verified(
            &current,
            &card,
            &revision,
            package,
            &mut |_| {},
            Default::default(),
        )
        .unwrap_err();
    assert!(error.to_string().contains("超出存储配额"), "{error}");
    assert_eq!(fs::read(&cached).unwrap(), b"new-w");
    assert_eq!(fs::read(root.join("attempts")).unwrap(), b"ran");
    assert!(!root.join("new/.cache").exists());
    assert!(!root.join("new/new.tachyon").exists());
}
