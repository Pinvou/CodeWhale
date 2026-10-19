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
fn explicit_purge_after_history_cleanup_removes_the_active_isolated_storage() {
    let (_temp, mgr, current, card, pkg, id) = fixture();
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
    assert!(
        version_store::list(Some(&current.card_id))
            .unwrap()
            .is_empty()
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
