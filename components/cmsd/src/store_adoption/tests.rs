use super::*;
use crate::docker::fixtures as fx;
use std::os::unix::fs::PermissionsExt;
use std::path::PathBuf;

fn stopped_fixture(tag: &str) -> (PathBuf, Arc<App>, serde_json::Value) {
    let root = fx::tmpdir(tag);
    let app = fx::make_app(&root, 128, 100);
    fx::write_docker_card(
        &app.config.cards_dir,
        "store-card--version",
        "qa-store-only",
        18198,
        Some("0.01"),
    );
    let dir = app.config.cards_dir.join("store-card--version");
    let files: serde_json::Map<String, serde_json::Value> = ["common.sh", "vendor/.deploy-env"]
        .into_iter()
        .map(|name| {
            let raw = std::fs::read(dir.join(name)).unwrap();
            (
                name.into(),
                hex(ring::digest::digest(&ring::digest::SHA256, &raw).as_ref()).into(),
            )
        })
        .collect();
    for name in ["common.sh", "vendor/.deploy-env"] {
        std::fs::set_permissions(dir.join(name), std::fs::Permissions::from_mode(0o644)).unwrap();
    }
    let entry = serde_json::json!({"card":"store-card--version","logical_card":"store-card","origin":"local","store_status":"deployed","downloaded_at":"2026-10-20","files":files});
    (root, app, entry)
}
#[test]
fn stopped_store_installation_enrolls_idempotently_without_start_or_default_change() {
    let (_root, app, entry) = stopped_fixture("store-adopt-stopped");
    let card = verify_entry(&app, &entry).unwrap();
    assert!(enroll(&app, &card, &entry).unwrap());
    let (revision, tasks) = {
        let store = app.store.lock().unwrap();
        assert!(store.state.models[&card.name].lifecycle_owned);
        assert_eq!(store.state.models[&card.name].status, ModelStatus::Stopped);
        assert!(store.state.active_model.is_none());
        (store.state.lifecycle_revision, store.state.tasks.len())
    };
    assert!(!enroll(&app, &card, &entry).unwrap());
    let store = app.store.lock().unwrap();
    assert_eq!(store.state.lifecycle_revision, revision);
    assert_eq!(store.state.tasks.len(), tasks);
    assert!(!fx::docker_calls(&_root)
        .iter()
        .any(|s| s.starts_with("start ") || s.starts_with("run ") || s.starts_with("stop ")));
    let recovery = crate::recovery::view(&app, &card.name).unwrap().unwrap();
    assert_eq!(
        recovery.get("desired_running").and_then(Value::as_bool),
        Some(false)
    );
    assert!(!crate::recovery::boot_enabled(&recovery));
}
#[test]
fn file_drift_incomplete_records_and_maintenance_never_grant_control() {
    let (_root, app, mut entry) = stopped_fixture("store-adopt-reject");
    let card = verify_entry(&app, &entry).unwrap();
    entry["store_status"] = "registered".into();
    assert!(verify_entry(&app, &entry).is_err());
    entry["store_status"] = "deployed".into();
    entry["origin"] = "discovered".into();
    assert!(verify_entry(&app, &entry).is_err());
    entry["origin"] = "local".into();
    let _task = app.task_create_exclusive("enable", "other-card").unwrap();
    assert!(enroll(&app, &card, &entry).is_err());
    std::fs::write(
        app.config.cards_dir.join(&card.name).join("common.sh"),
        "changed",
    )
    .unwrap();
    assert_eq!(
        verify_entry(&app, &entry).unwrap_err(),
        "store_card_changed_since_inventory"
    );
    assert!(app.store.lock().unwrap().state.models.is_empty());
}
#[test]
fn a_colliding_external_container_is_not_taken_over() {
    let (root, app, entry) = stopped_fixture("store-adopt-collision");
    let card = verify_entry(&app, &entry).unwrap();
    fx::set_container(
        &root,
        "qa-store-only",
        "another/image",
        true,
        "always",
        0,
        false,
        1234,
        18198,
        8000,
        "",
    );
    assert_eq!(
        enroll(&app, &card, &entry).unwrap_err(),
        "store_container_binding_mismatch"
    );
    assert!(!app
        .store
        .lock()
        .unwrap()
        .state
        .models
        .get(&card.name)
        .is_some_and(|m| m.lifecycle_owned));
    assert!(!fx::docker_calls(&root)
        .iter()
        .any(|s| s.starts_with("update ") || s.starts_with("stop ")));
}
#[test]
fn headless_receipt_reconciles_version_alias_and_reports_reader_failure() {
    let (root, app, mut entry) = stopped_fixture("store-adopt-reader");
    entry["version_id"] = "current".into();
    let helper = root.join("inventory");
    std::fs::create_dir_all(root.join("model-store")).unwrap();
    let inventory = serde_json::json!({"format":"pinvou-installed-models-v1","cards_root":app.config.cards_dir,"models":[entry],"skipped":[],
        "selection_policy":"latest-installed-v1","selections":[{"logical_card":"store-card","card":"store-card--version","version_id":"current"}],"archived_models":[]});
    std::fs::write(
        &helper,
        format!("#!/bin/sh\ncat <<'JSON'\n{inventory}\nJSON\n"),
    )
    .unwrap();
    std::fs::set_permissions(&helper, std::fs::Permissions::from_mode(0o700)).unwrap();
    crate::private_storage::secure_state(&app.config.state_dir).unwrap();
    crate::private_storage::publish(
        &app.config.state_dir.join("store-reader.json"),
        serde_json::json!({"format":"pinvou-store-reader-v1","program":helper,"profile":root})
            .to_string()
            .as_bytes(),
        true,
    )
    .unwrap();
    reconcile(&app);
    assert_eq!(
        bindings(&app).get("store-card").and_then(Value::as_str),
        Some("store-card--version")
    );
    let before = app.store.lock().unwrap().state.lifecycle_revision;
    std::fs::remove_file(app.config.state_dir.join("store-adoption-v1")).unwrap();
    reconcile(&app);
    assert!(app.config.state_dir.join("store-adoption-v1").is_file());
    assert_eq!(app.store.lock().unwrap().state.lifecycle_revision, before);
    std::fs::write(&helper, "#!/bin/sh\nexit 1\n").unwrap();
    reconcile(&app);
    assert_eq!(
        app.store_adoption_status
            .lock()
            .unwrap()
            .get("phase")
            .and_then(Value::as_str),
        Some("unavailable")
    );
    assert_eq!(
        bindings(&app).get("store-card").and_then(Value::as_str),
        Some("store-card--version")
    );
}
#[test]
fn native_binding_checks_process_identity_and_rejects_pid_reuse() {
    let root = fx::tmpdir("store-adopt-native");
    let mut app = Arc::try_unwrap(fx::make_app(&root, 128, 100)).ok().unwrap();
    app.config.proc_root = root.join("proc");
    let engine = app.config.engine_root.join("01234567");
    std::fs::create_dir_all(&engine).unwrap();
    std::fs::write(engine.join("tachyon-serve"), "bin").unwrap();
    let weights = root.join("weights");
    std::fs::create_dir_all(&weights).unwrap();
    std::fs::write(weights.join("model.tachyon"), "model").unwrap();
    let dir = app.config.cards_dir.join("native");
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::write(
        dir.join("common.sh"),
        "ENGINE_BIN_SHA256=01234567\nMODEL_ID=Native\nARTIFACT=model.tachyon\nTOTAL_BYTES=5\n",
    )
    .unwrap();
    std::fs::write(dir.join(".deploy-env"),format!("PORT=18199\nHOST=127.0.0.1\nMODEL_DIR={}\nENGINE_DIR={}\nARTIFACT=model.tachyon\nMODEL_ID=Native\nMAX_CONCURRENCY=1\n",weights.display(),engine.display())).unwrap();
    let proc = app.config.proc_root.join("1234");
    std::fs::create_dir_all(&proc).unwrap();
    std::os::unix::fs::symlink(engine.join("tachyon-serve"), proc.join("exe")).unwrap();
    std::fs::write(
        proc.join("cmdline"),
        format!(
            "tachyon-serve\0{}\0--port\018199\0--model-id\0Native\0--host\0127.0.0.1\0--max-context\032768\0--max-concurrency\01\0--device-state-slots\016\0--host-state-slots\016\0",
            weights.join("model.tachyon").display()
        ),
    )
    .unwrap();
    let stat = "1234 (tachyon-serve) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 123456 20";
    std::fs::write(proc.join("stat"), stat).unwrap();
    std::fs::write(proc.join("cgroup"), "0::/user.slice/app.scope\n").unwrap();
    std::fs::write(proc.join("environ"), b"").unwrap();
    std::fs::create_dir_all(proc.join("fd")).unwrap();
    std::os::unix::fs::symlink("socket:[45678]", proc.join("fd/3")).unwrap();
    std::fs::create_dir_all(app.config.proc_root.join("net")).unwrap();
    std::fs::write(
        app.config.proc_root.join("net/tcp"),
        format!(
            "header\n0: 0100007F:{:04X} 00000000:0000 0A 0:0 0:0 0 1000 0 45678\n",
            18199
        ),
    )
    .unwrap();
    let cli = root.join("systemctl");
    std::fs::write(&cli, format!(r#"#!/bin/sh
[ "$1" = --user ] && shift
printf '%s\n' "$*" >> '{}'
case "$1" in
 show) if [ "$3" = --property=Restart ]; then echo no; else printf 'ActiveState=inactive\nSubState=dead\nMainPID=0\nResult=success\nLoadState=not-found\nUnitFileState=disabled\n'; fi ;;
 is-enabled) echo disabled; exit 1 ;;
esac
exit 0
"#,root.join("systemctl.calls").display())).unwrap();
    std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
    app.systemd = crate::systemd::Systemd::new(
        app.config.systemd_mode,
        app.config.unit_dir.clone(),
        app.config.env_dir(),
        "test".into(),
    )
    .with_test_cli(&cli);
    let app = Arc::new(app);
    let card = crate::ops::find_card(&app, "native").unwrap();
    let files: serde_json::Map<String, serde_json::Value> = ["common.sh", ".deploy-env"]
        .into_iter()
        .map(|name| {
            let bytes = std::fs::read(dir.join(name)).unwrap();
            (
                name.to_owned(),
                hex(ring::digest::digest(&ring::digest::SHA256, &bytes).as_ref()).into(),
            )
        })
        .collect();
    let entry = serde_json::json!({"card":"native","logical_card":"native","origin":"local","store_status":"deployed","downloaded_at":"2026-10-21","files":files});
    let binding = runtime::discover(&app, &card).unwrap().unwrap();
    let actual_args = std::fs::read(proc.join("cmdline")).unwrap();
    std::fs::write(
        proc.join("cmdline"),
        String::from_utf8(actual_args.clone())
            .unwrap()
            .replace("32768", "262144"),
    )
    .unwrap();
    assert!(runtime::discover(&app, &card)
        .err()
        .unwrap()
        .starts_with("native_instance_mismatch"));
    assert!(enroll(&app, &card, &entry)
        .unwrap_err()
        .starts_with("native_instance_mismatch"));
    assert!(!app
        .store
        .lock()
        .unwrap()
        .state
        .models
        .get("native")
        .is_some_and(|model| model.lifecycle_owned));
    assert!(!crate::recovery::managed(&app, "native"));
    assert!(native_state(&app, "native").unwrap().is_none());
    assert!(!root.join("systemctl.calls").exists());
    std::fs::write(proc.join("cmdline"), &actual_args).unwrap();
    std::fs::write(proc.join("environ"), b"TACHYON_PROJ_W4A4=1\0").unwrap();
    assert!(runtime::discover(&app, &card)
        .err()
        .unwrap()
        .starts_with("native_instance_mismatch"));
    assert!(enroll(&app, &card, &entry)
        .unwrap_err()
        .starts_with("native_instance_mismatch"));
    assert!(!crate::recovery::managed(&app, "native"));
    assert!(!root.join("systemctl.calls").exists());
    std::fs::write(proc.join("environ"), b"").unwrap();
    runtime::save(&app, "native", &binding).unwrap();
    assert_eq!(
        native_state(&app, "native").unwrap().unwrap().main_pid,
        1234
    );
    std::fs::write(proc.join("cmdline"), b"unrelated\0").unwrap();
    assert!(native_state(&app, "native").is_err());
    std::fs::write(proc.join("stat"), stat.replace("123456", "999999")).unwrap();
    assert!(!native_state(&app, "native").unwrap().unwrap().is_active());
    assert!(stop_native(&app, "native").unwrap()); // Reused PID is never signalled.
    assert!(native_state(&app, "native").unwrap().is_none());
}

#[test]
fn reader_registration_is_account_bound_profile_bound_and_idempotent() {
    let root = fx::tmpdir("store-reader-register");
    let mut app = Arc::try_unwrap(fx::make_app(&root, 128, 100)).ok().unwrap();
    app.config.cards_dir = root.join("model-store/cards");
    let helper = root.join("reader");
    std::fs::write(&helper, "#!/bin/sh\nexit 0\n").unwrap();
    std::fs::set_permissions(&helper, std::fs::Permissions::from_mode(0o700)).unwrap();
    let raw = serde_json::json!({"program":helper,"profile":root}).to_string();
    let mut peer = crate::peer::LocalPeer::current();
    peer.uid += 1;
    assert_eq!(
        register_reader(&app, raw.as_bytes(), peer).unwrap_err().0,
        403
    );
    let peer = crate::peer::LocalPeer::current();
    register_reader(&app, raw.as_bytes(), peer).unwrap();
    let path = app.config.state_dir.join("store-reader.json");
    let modified = std::fs::metadata(&path).unwrap().modified().unwrap();
    // A lost initial snapshot must not wait for the 30-second periodic scan.
    app.store_adoption_requested
        .store(false, std::sync::atomic::Ordering::Release);
    register_reader(&app, raw.as_bytes(), peer).unwrap();
    assert!(app
        .store_adoption_requested
        .load(std::sync::atomic::Ordering::Acquire));
    assert_eq!(
        std::fs::metadata(&path).unwrap().modified().unwrap(),
        modified
    );
    let bad = serde_json::json!({"program":helper,"profile":root.join("wrong")}).to_string();
    assert_eq!(
        register_reader(&app, bad.as_bytes(), peer).unwrap_err().1,
        "store_reader_profile_mismatch"
    );
    assert_eq!(
        reader_view(&app).get("profile").and_then(Value::as_str),
        root.to_str()
    );
}

#[test]
fn all_current_recipe_assets_are_verified_without_executing_them() {
    let (_root, app, mut entry) = stopped_fixture("store-adopt-assets");
    let card = app.config.cards_dir.join("store-card--version");
    for name in [
        "deploy.sh",
        "stop.sh",
        "client-migration.json",
        "native_engine_process.py",
        "weights.sha256",
        "host_cuda_driver.py",
        "extra-guide.md",
        "metadata.json",
    ] {
        let bytes = b"source data, never executed";
        std::fs::write(card.join(name), bytes).unwrap();
        entry["files"][name] =
            hex(ring::digest::digest(&ring::digest::SHA256, bytes).as_ref()).into();
    }
    assert!(entry["files"].as_object().unwrap().len() > 9);
    assert!(verify_entry(&app, &entry).is_ok());
    entry["files"]["../escape"] = "a".repeat(64).into();
    assert!(verify_entry(&app, &entry).is_err());
    entry["files"].as_object_mut().unwrap().remove("../escape");
    std::fs::write(card.join("client-migration.json"), "drift").unwrap();
    assert!(verify_entry(&app, &entry).is_err());
}

#[test]
#[ignore = "opt-in production receipt read only"]
fn inspect_real_current_store_receipt() {
    let path = std::env::var("CMSD_QA_STORE_INVENTORY_FILE").unwrap();
    let value: serde_json::Value = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    let entry = value["models"]
        .as_array()
        .unwrap()
        .iter()
        .find(|row| row["logical_card"] == "qwen38-27b-nvfp4-tachyon")
        .unwrap();
    let root = fx::tmpdir("real-receipt-readonly");
    let mut app = fx::make_app(&root, 128, 100);
    Arc::get_mut(&mut app).unwrap().config.cards_dir =
        std::path::PathBuf::from(value["cards_root"].as_str().unwrap());
    let card = verify_entry(&app, entry).unwrap();
    println!(
        "{}",
        serde_json::json!({"card":card.name,"verified_files":entry["files"].as_object().unwrap().len(),"scope":"Actual production receipt and files through production verifier, read-only"})
    );
}

#[test]
fn enrollment_uses_the_matching_published_port_after_read_only_discovery() {
    let (root, app, entry) = stopped_fixture("store-network-second-binding");
    let card = verify_entry(&app, &entry).unwrap();
    let deploy = card.docker_deploy.as_ref().unwrap();
    std::fs::create_dir_all(&deploy.model_dir).unwrap();
    let ports = serde_json::json!({
        "7000/tcp":[{"HostIp":"127.0.0.1","HostPort":"18197"}],
        "8000/tcp":[{"HostIp":"127.0.0.1","HostPort":"18198"}]
    });
    let inspect = serde_json::json!([{"Name":"/qa-store-only","Path":"entry",
        "Args":["--mem-fraction-static","0.01"],
        "State":{"Status":"running","Running":true,"Pid":4321,"ExitCode":0},
        "Config":{"Image":"fixture/img:1.0"},"Mounts":[{"Source":deploy.model_dir}],
        "HostConfig":{"NetworkMode":"bridge","RestartPolicy":{"Name":"no"},"PortBindings":ports},
        "NetworkSettings":{"Ports":ports}}]);
    let response = root.join("network-inspect.json");
    std::fs::write(&response, inspect.to_string()).unwrap();
    let cli = PathBuf::from(&app.config.docker_cli);
    let script = std::fs::read_to_string(&cli).unwrap().replace(
        "  inspect)",
        &format!("  inspect)\n    cat '{}'; exit 0", response.display()),
    );
    let temporary = root.join("network-docker.tmp");
    std::fs::write(&temporary, script).unwrap();
    std::fs::set_permissions(&temporary, std::fs::Permissions::from_mode(0o700)).unwrap();
    std::fs::rename(temporary, cli).unwrap();
    let observed = app.docker.inspect("qa-store-only").unwrap().unwrap();
    assert_eq!(observed.host_port, Some(18197));
    crate::ops::register_container_managed(&app, &card, &observed);
    assert_eq!(
        app.store.lock().unwrap().state.models[&card.name].port,
        18197
    );
    assert!(enroll(&app, &card, &entry).unwrap());
    let model = app.store.lock().unwrap().state.models[&card.name].clone();
    assert!(model.lifecycle_owned);
    assert_eq!(model.port, 18198);
    assert!(crate::container_runtime::routable(&app, &model));
    assert!(!fx::docker_calls(&root)
        .iter()
        .any(|call| call.starts_with("start ") || call.starts_with("stop ")));
    std::fs::remove_dir_all(root).unwrap();
}

#[test]
fn completed_store_installation_cannot_enroll_an_unpublished_runtime() {
    let (root, app, entry) = stopped_fixture("store-unpublished-runtime");
    let card = verify_entry(&app, &entry).unwrap();
    fx::set_container(
        &root,
        "qa-store-only",
        "fixture/img:1.0",
        true,
        "always",
        0,
        false,
        4321,
        0,
        8000,
        "--mem-fraction-static,0.01",
    );
    assert_eq!(
        enroll(&app, &card, &entry).unwrap_err(),
        "store_container_binding_mismatch"
    );
    assert!(!app
        .store
        .lock()
        .unwrap()
        .state
        .models
        .get(&card.name)
        .is_some_and(|model| model.lifecycle_owned));
    assert!(fx::state_line(&root, "qa-store-only")
        .unwrap()
        .contains("^true^always^"));
    assert!(!fx::docker_calls(&root)
        .iter()
        .any(|call| call.starts_with("update ")
            || call.starts_with("start ")
            || call.starts_with("stop ")));
    std::fs::remove_dir_all(root).unwrap();
}
