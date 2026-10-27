//! Ordinary native purge must use the same final runtime proof as store uninstall.
use super::*;
use crate::docker::fixtures as fx;
use std::os::unix::fs::PermissionsExt;

fn fixture(tag: &str, stop_mode: &str) -> (PathBuf, Arc<App>, PathBuf) {
    let root = fx::tmpdir(tag);
    let mut app = fx::make_app(&root, 128, 100);
    let proc_root = root.join("proc");
    std::fs::create_dir(&proc_root).unwrap();
    Arc::get_mut(&mut app).unwrap().config.proc_root = proc_root;
    let cli = root.join("systemctl");
    let active = root.join("runtime.active");
    let calls = root.join("systemctl.calls");
    std::fs::write(&active, "active").unwrap();
    std::fs::write(&cli, format!(r#"#!/bin/sh
[ "$1" = --user ] && shift
printf '%s\n' "$*" >> '{}'
case "$1" in
  show)
    if [ "$3" = --property=Restart ]; then echo no; exit 0; fi
    if [ -f '{}' ]; then
      printf 'ActiveState=active\nSubState=running\nMainPID=4321\nResult=success\nLoadState=loaded\nUnitFileState=disabled\n'
    else
      pid=0
      if [ '{}' = residual_pid ]; then pid=4321; fi
      printf 'ActiveState=inactive\nSubState=dead\nMainPID=%s\nResult=success\nLoadState=loaded\nUnitFileState=disabled\n' "$pid"
    fi
    ;;
  stop)
    if [ '{mode}' != missing ] && [ '{mode}' != create_target ]; then
      test -f '{fixture_root}/weights/model.tachyon' || exit 9
    fi
    if [ '{mode}' = replace_target ]; then mv '{fixture_root}/weights' '{fixture_root}/weights-original'; fi
    if [ '{mode}' = replace_target ] || [ '{mode}' = create_target ]; then
      mkdir -p '{fixture_root}/weights'
      printf new > '{fixture_root}/weights/model.tachyon'
    fi
    rm -f '{}'
    ;;
  is-enabled)
    if [ '{}' = autostart ]; then echo enabled; else echo disabled; fi
    ;;
esac
exit 0
"#, calls.display(), active.display(), stop_mode, active.display(), stop_mode, mode=stop_mode, fixture_root=root.display())).unwrap();
    std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
    let config = app.config.clone();
    Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
        config.systemd_mode,
        config.unit_dir.clone(),
        config.env_dir(),
        "test".into(),
    )
    .with_test_cli(&cli);
    app.systemd.write_template().unwrap();
    let weights = root.join("weights");
    let engine = app.config.engine_root.join("01234567");
    let card = app.config.cards_dir.join("native");
    std::fs::create_dir_all(&weights).unwrap();
    std::fs::create_dir_all(&engine).unwrap();
    std::fs::create_dir_all(&card).unwrap();
    std::fs::write(weights.join("model.tachyon"), b"model").unwrap();
    std::fs::write(engine.join("tachyon-serve"), b"engine").unwrap();
    std::fs::write(
        card.join("common.sh"),
        "ENGINE_BIN_SHA256=01234567\nMODEL_ID=Native\nARTIFACT=model.tachyon\nTOTAL_BYTES=5\n",
    )
    .unwrap();
    let port = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    std::fs::write(card.join(".deploy-env"), format!("PORT={port}\nHOST=127.0.0.1\nMODEL_DIR={}\nENGINE_DIR={}\nARTIFACT=model.tachyon\nMODEL_ID=Native\nMAX_CONCURRENCY=1\n", weights.display(), engine.display())).unwrap();
    upsert_model_state(&app, "native", |model| {
        model.lifecycle_owned = true;
        model.status = ModelStatus::Stopped;
        model.main_pid = None;
    });
    (root, app, weights)
}

#[test]
fn native_purge_stops_real_runtime_even_when_cached_stopped() {
    let (root, app, weights) = fixture("r05-purge-native-stale-cache", "normal");
    let task = purge(&app, "native").unwrap();
    let terminal = fx::wait_terminal(&app, &task, 10);
    assert_eq!(
        terminal.status,
        crate::store::TaskStatus::Succeeded,
        "{:?}",
        terminal.error
    );
    assert!(!weights.exists());
    assert!(!root.join("runtime.active").exists());
    let calls = std::fs::read_to_string(root.join("systemctl.calls")).unwrap();
    assert!(
        calls
            .lines()
            .any(|line| line == "stop megacube-model@native.service"),
        "{calls}"
    );
    assert!(!app
        .store
        .lock()
        .unwrap()
        .state
        .models
        .contains_key("native"));
    std::fs::remove_dir_all(root).unwrap();
}

#[test]
fn native_purge_preserves_weights_and_ownership_if_final_proof_is_not_quiet() {
    for mode in [
        "residual_pid",
        "autostart",
        "unknown_processes",
        "busy_port",
    ] {
        let (root, app, weights) = fixture(&format!("r05-purge-native-{mode}"), mode);
        if mode == "unknown_processes" {
            std::fs::remove_dir(&app.config.proc_root).unwrap();
        }
        let listener = if mode == "busy_port" {
            let port = app.store.lock().unwrap().state.models["native"].port;
            Some(std::net::TcpListener::bind(("127.0.0.1", port)).unwrap())
        } else {
            None
        };
        let task = purge(&app, "native").unwrap();
        let terminal = fx::wait_terminal(&app, &task, 10);
        assert_eq!(
            terminal.status,
            crate::store::TaskStatus::Failed,
            "{mode}: {:?}",
            terminal.result
        );
        assert_eq!(
            std::fs::read(weights.join("model.tachyon")).unwrap(),
            b"model",
            "{mode}"
        );
        assert!(
            app.store.lock().unwrap().state.models["native"].lifecycle_owned,
            "{mode}"
        );
        assert!(app
            .systemd
            .unit_name_for("native")
            .ends_with("@native.service"));
        drop(listener);
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[test]
fn native_purge_does_not_delete_a_target_replaced_or_created_while_stopping() {
    for mode in ["replace_target", "create_target"] {
        let (root, app, weights) = fixture(&format!("r05-purge-native-{mode}"), mode);
        if mode == "create_target" {
            std::fs::remove_dir_all(&weights).unwrap();
        }
        let task = purge(&app, "native").unwrap();
        let terminal = fx::wait_terminal(&app, &task, 10);
        assert_eq!(
            terminal.status,
            crate::store::TaskStatus::Failed,
            "{mode}: {:?}",
            terminal.result
        );
        assert_eq!(
            std::fs::read(weights.join("model.tachyon")).unwrap(),
            b"new",
            "{mode}"
        );
        assert!(app.store.lock().unwrap().state.models["native"].lifecycle_owned);
        if mode == "replace_target" {
            assert_eq!(
                std::fs::read(root.join("weights-original/model.tachyon")).unwrap(),
                b"model"
            );
        }
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[test]
fn native_purge_can_retire_a_genuinely_missing_target_after_stopping() {
    let (root, app, weights) = fixture("r05-purge-native-missing", "missing");
    std::fs::remove_dir_all(&weights).unwrap();
    let task = purge(&app, "native").unwrap();
    let terminal = fx::wait_terminal(&app, &task, 10);
    assert_eq!(
        terminal.status,
        crate::store::TaskStatus::Succeeded,
        "{:?}",
        terminal.error
    );
    assert!(!weights.exists());
    assert!(!root.join("runtime.active").exists());
    assert!(!app
        .store
        .lock()
        .unwrap()
        .state
        .models
        .contains_key("native"));
    std::fs::remove_dir_all(root).unwrap();
}
