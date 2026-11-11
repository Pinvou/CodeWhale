//! Settings patches must wait across processes, then read the latest policy.
use super::*;
use std::process::{Child, Command};
use std::time::{Duration, Instant};

const CHILD_MODE: &str = "PINVOU_MODEL_STORE_SETTINGS_CHILD";

#[test]
fn settings_patch_child() {
    let Ok(mode) = std::env::var(CHILD_MODE) else {
        return;
    };
    let ready = std::env::var_os("PINVOU_MODEL_STORE_SETTINGS_READY").unwrap();
    fs::write(ready, "ready").unwrap();
    match mode.as_str() {
        "storage" => {
            super::super::service::set_storage_settings(
                None,
                Some(vec!["/child/import".into()]),
                None,
            )
            .unwrap();
        }
        "test-mode" => {
            super::super::service::disable_test_store().unwrap();
        }
        "reader" => {
            assert!(!load_settings().startup_update_check_enabled);
        }
        _ => panic!("unknown child mode"),
    }
}

struct ChildGuard(Child);
impl Drop for ChildGuard {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn spawn_waiter(home: &Path, mode: &str) -> ChildGuard {
    let ready = home.join("settings-child-ready");
    let child = Command::new(std::env::current_exe().unwrap())
        .args(["--exact", "features::model_store::platform::linux::config::transaction_tests::settings_patch_child", "--nocapture"])
        .env("HOME", home)
        .env_remove("PINVOU3_HOME")
        .env(CHILD_MODE, mode)
        .env("PINVOU_MODEL_STORE_SETTINGS_READY", &ready)
        .spawn().unwrap();
    let mut child = ChildGuard(child);
    let deadline = Instant::now() + Duration::from_secs(5);
    while !ready.exists() && Instant::now() < deadline {
        assert!(
            child.0.try_wait().unwrap().is_none(),
            "child exited before reaching settings call"
        );
        std::thread::sleep(Duration::from_millis(5));
    }
    assert!(ready.exists(), "child never reached settings call");
    std::thread::sleep(Duration::from_millis(100));
    assert!(
        child.0.try_wait().unwrap().is_none(),
        "settings call bypassed the cross-process lock"
    );
    child
}

fn finish(child: &mut ChildGuard) {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Some(status) = child.0.try_wait().unwrap() {
            assert!(status.success());
            return;
        }
        assert!(
            Instant::now() < deadline,
            "settings child did not finish after lock release"
        );
        std::thread::sleep(Duration::from_millis(5));
    }
}

#[test]
fn both_production_writers_wait_then_patch_the_fresh_cross_process_settings() {
    for mode in ["storage", "test-mode"] {
        let home = TempHome::new();
        let first = Settings {
            test_mode_enabled: true,
            ..Default::default()
        };
        save_settings(&first).unwrap();
        let lock = settings_lock().unwrap();
        let mut child = spawn_waiter(home.path(), mode);
        let mut latest = read_settings_unlocked().unwrap();
        latest.startup_update_check_enabled = false;
        latest.store_url = "https://updated.example.test/store".into();
        latest.models_dir = home
            .path()
            .join("new-models")
            .to_string_lossy()
            .into_owned();
        write_settings_unlocked(&latest).unwrap();
        drop(lock);
        finish(&mut child);
        let after = read_settings().unwrap();
        assert!(!after.startup_update_check_enabled);
        assert_eq!(after.store_url, latest.store_url);
        assert_eq!(after.models_dir, latest.models_dir);
        if mode == "storage" {
            assert_eq!(after.import_scan_dirs, ["/child/import"]);
            assert!(after.test_mode_enabled);
        } else {
            assert!(!after.test_mode_enabled);
        }
    }
}

#[test]
fn quarantining_readers_wait_for_the_same_lock_and_do_not_move_new_settings() {
    let home = TempHome::new();
    let broken = r#"{"startup_update_check_enabled":false,"gateway_port":"broken"}"#;
    fs::write(config_file(), broken).unwrap();
    let lock = settings_lock().unwrap();
    let mut child = spawn_waiter(home.path(), "reader");
    quarantine_broken_config(&config_file(), "test corruption");
    let latest = Settings {
        startup_update_check_enabled: false,
        ..Default::default()
    };
    write_settings_unlocked(&latest).unwrap();
    let expected = fs::read(config_file()).unwrap();
    drop(lock);
    finish(&mut child);
    assert_eq!(fs::read(config_file()).unwrap(), expected);
    assert_eq!(
        fs::read_to_string(config_file().with_extension("json.bad")).unwrap(),
        broken
    );
}

#[test]
fn quarantine_blocks_all_production_settings_patches_until_recovery() {
    let _home = TempHome::new();
    let broken = r#"{"startup_update_check_enabled":false,"gateway_port":"broken","models_dir":"/keep/original"}"#;
    fs::write(config_file(), broken).unwrap();
    assert!(startup_update_check_enabled().is_err());
    let mut patched = false;
    assert!(
        update_settings(|settings| {
            patched = true;
            settings.test_mode_enabled = true;
            Ok(())
        })
        .is_err()
    );
    assert!(
        !patched,
        "an unreadable snapshot must never reach the patch"
    );
    assert!(!load_settings().startup_update_check_enabled);
    let backup = config_file().with_extension("json.bad");
    assert!(!config_file().exists());
    assert_eq!(fs::read_to_string(&backup).unwrap(), broken);
    assert!(!startup_update_check_enabled().unwrap());
    let error = super::super::service::disable_test_store().unwrap_err();
    assert!(error.contains("隔离留存"), "{error}");
    assert!(!startup_update_check_enabled().unwrap());
    let error = super::super::service::set_storage_settings(
        None,
        Some(vec!["/recovered/import".into()]),
        None,
    )
    .unwrap_err();
    assert!(error.contains("隔离留存"), "{error}");
    let error = super::super::service::set_storage_settings(None, None, Some(true)).unwrap_err();
    assert!(error.contains("隔离留存"), "{error}");
    assert!(!startup_update_check_enabled().unwrap());
    assert!(
        !config_file().exists(),
        "a rejected save must not recreate settings"
    );
    assert!(!config_file().with_extension("json.tmp").exists());
    assert_eq!(fs::read_to_string(&backup).unwrap(), broken);
    assert!(crate::features::model_store::store_weights_roots_checked().is_err());
}

#[test]
fn valid_restoration_allows_fresh_patches_without_losing_protected_roots_or_consent() {
    let home = TempHome::new();
    let backup = config_file().with_extension("json.bad");
    let broken = r#"{"models_dir":"/keep/original","gateway_port":"broken"}"#;
    fs::write(&backup, broken).unwrap();
    assert!(try_load_settings().is_err());
    let models = home
        .path()
        .join("restored-models")
        .to_string_lossy()
        .into_owned();
    let scan = home
        .path()
        .join("restored-scan")
        .to_string_lossy()
        .into_owned();
    let old_models = home
        .path()
        .join("historical-models")
        .to_string_lossy()
        .into_owned();
    let old_scan = home
        .path()
        .join("historical-scan")
        .to_string_lossy()
        .into_owned();
    let restored = Settings {
        models_dir: models.clone(),
        models_dir_history: vec![old_models.clone()],
        import_scan_dirs: vec![scan.clone()],
        import_scan_dirs_history: vec![old_scan.clone()],
        store_url: "https://restored.example.test/store".into(),
        test_mode_enabled: true,
        startup_update_check_enabled: false,
        ..Default::default()
    };
    // Manual repair restores the actual valid configuration, not a synthesized
    // settings save that could erase the unknown roots in the backup.
    fs::write(config_file(), serde_json::to_vec(&restored).unwrap()).unwrap();
    super::super::service::disable_test_store().unwrap();
    super::super::service::set_storage_settings(None, Some(Vec::new()), None).unwrap();
    let after = try_load_settings().unwrap();
    assert!(!after.test_mode_enabled);
    assert!(!after.startup_update_check_enabled);
    assert_eq!(after.store_url, restored.store_url);
    assert_eq!(after.models_dir, models);
    assert_eq!(after.models_dir_history, [old_models]);
    assert!(after.import_scan_dirs.is_empty());
    assert_eq!(after.import_scan_dirs_history, [old_scan, scan]);
    assert_eq!(fs::read_to_string(&backup).unwrap(), broken);
    super::super::service::set_storage_settings(None, None, Some(true)).unwrap();
    assert!(startup_update_check_enabled().unwrap());
    assert_eq!(fs::read_to_string(&backup).unwrap(), broken);
    fs::write(config_file(), "{}").unwrap();
    assert!(
        startup_update_check_enabled().unwrap(),
        "a valid restored old configuration preserves its old default"
    );
}

#[test]
fn explicit_manual_abandonment_of_quarantine_allows_fresh_settings_creation() {
    let _home = TempHome::new();
    let backup = config_file().with_extension("json.bad");
    fs::write(&backup, r#"{"models_dir":"/abandoned/custom/root"}"#).unwrap();
    assert!(super::super::service::disable_test_store().is_err());
    assert!(!config_file().exists());
    assert!(
        backup.exists(),
        "ordinary saving must not abandon the backup"
    );
    // This is an operator's explicit filesystem action; no automatic save path
    // removes the marker or claims to restore the abandoned roots.
    fs::remove_file(&backup).unwrap();
    super::super::service::disable_test_store().unwrap();
    let after = try_load_settings().unwrap();
    assert!(
        after.startup_update_check_enabled,
        "fresh installs keep the old default"
    );
    assert!(!after.test_mode_enabled);
    assert!(after.models_dir_history.is_empty());
    assert!(after.import_scan_dirs_history.is_empty());
    assert_ne!(after.models_dir, "/abandoned/custom/root");
    assert!(!backup.exists());
    assert!(config_file().is_file());
}

#[test]
fn rejected_transaction_does_not_publish_a_partial_settings_patch() {
    let _home = TempHome::new();
    update_settings(|settings| {
        settings.startup_update_check_enabled = false;
        Ok(())
    })
    .unwrap();
    let before = fs::read(config_file()).unwrap();
    assert!(
        update_settings(|settings| {
            settings.startup_update_check_enabled = true;
            Err("rejected patch".into())
        })
        .is_err()
    );
    assert_eq!(fs::read(config_file()).unwrap(), before);
}
