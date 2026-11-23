use super::*;
use crate::docker::fixtures as fx;
use crate::store_adoption::runtime;
use std::io::{Read, Write};
use std::os::unix::fs::PermissionsExt;
use std::sync::atomic::{AtomicBool, Ordering};
struct Fixture {
    root: PathBuf,
    app: Arc<App>,
    card: Card,
    stop: Arc<AtomicBool>,
    health: Option<std::thread::JoinHandle<()>>,
}
fn read_health_request(connection: &mut impl Read) -> std::io::Result<()> {
    let mut request = Vec::new();
    let mut chunk = [0u8; 512];
    while request.len() < 4096 {
        let remaining = (4096 - request.len()).min(chunk.len());
        let read = connection.read(&mut chunk[..remaining])?;
        if read == 0 {
            return Err(std::io::ErrorKind::UnexpectedEof.into());
        }
        request.extend_from_slice(&chunk[..read]);
        if request.windows(4).any(|bytes| bytes == b"\r\n\r\n") {
            return Ok(());
        }
    }
    Err(std::io::ErrorKind::InvalidData.into())
}
impl Drop for Fixture {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        if let Some(thread) = self.health.take() {
            thread.join().unwrap();
        }
        let _ = std::fs::remove_dir_all(&self.root);
    }
}
fn fixture(tag: &str) -> Fixture {
    let root = fx::tmpdir(tag);
    let mut app = Arc::try_unwrap(fx::make_app(&root, 128, 0)).ok().unwrap();
    app.config.proc_root = root.join("proc");
    app.config.native_unit_namespace = "state".into();
    let engine = app.config.engine_root.join("01234567");
    std::fs::create_dir_all(&engine).unwrap();
    std::fs::write(engine.join("tachyon-serve"), b"engine").unwrap();
    let weights = root.join("weights");
    std::fs::create_dir_all(&weights).unwrap();
    std::fs::write(weights.join("model.tachyon"), b"model").unwrap();
    let directory = app.config.cards_dir.join("native");
    std::fs::create_dir_all(&directory).unwrap();
    std::fs::write(
        directory.join("common.sh"),
        "ENGINE_BIN_SHA256=01234567\nMODEL_ID=Native\nARTIFACT=model.tachyon\nTOTAL_BYTES=5\n",
    )
    .unwrap();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    listener.set_nonblocking(true).unwrap();
    let stop = Arc::new(AtomicBool::new(false));
    let stop_thread = stop.clone();
    let health = std::thread::spawn(move || {
        while !stop_thread.load(Ordering::Relaxed) {
            match listener.accept() {
                Ok((mut conn, _)) => {
                    conn.set_read_timeout(Some(std::time::Duration::from_secs(1)))
                        .unwrap();
                    // The client writes the request in several pieces. Closing
                    // with unread request bytes can reset the connection and
                    // discard an otherwise successful health response.
                    if read_health_request(&mut conn).is_err() {
                        continue;
                    }
                    let body = r#"{"data":[{"id":"Native"}]}"#;
                    write!(conn,"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",body.len(),body).unwrap();
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    std::thread::sleep(std::time::Duration::from_millis(2))
                }
                Err(e) => panic!("private health listener: {e}"),
            }
        }
    });
    std::fs::write(directory.join(".deploy-env"),format!("PORT={port}\nHOST=127.0.0.1\nMODEL_DIR={}\nENGINE_DIR={}\nARTIFACT=model.tachyon\nMODEL_ID=Native\nMAX_CONCURRENCY=1\n",weights.display(),engine.display())).unwrap();
    let process = app.config.proc_root.join("1234");
    std::fs::create_dir_all(process.join("fd")).unwrap();
    std::os::unix::fs::symlink(engine.join("tachyon-serve"), process.join("exe")).unwrap();
    let args = [
        "tachyon-serve".to_owned(),
        weights.join("model.tachyon").display().to_string(),
        "--port".into(),
        port.to_string(),
        "--model-id".into(),
        "Native".into(),
        "--host".into(),
        "127.0.0.1".into(),
        "--max-context".into(),
        "32768".into(),
        "--max-concurrency".into(),
        "1".into(),
        "--device-state-slots".into(),
        "16".into(),
        "--host-state-slots".into(),
        "16".into(),
    ];
    std::fs::write(process.join("cmdline"), args.join("\0") + "\0").unwrap();
    std::fs::write(
        process.join("stat"),
        "1234 (tachyon-serve) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 123456 20",
    )
    .unwrap();
    std::fs::write(
        process.join("cgroup"),
        "0::/user.slice/megacube-model@native.service\n",
    )
    .unwrap();
    std::fs::write(process.join("environ"), b"").unwrap();
    std::os::unix::fs::symlink("socket:[45678]", process.join("fd/3")).unwrap();
    std::fs::create_dir_all(app.config.proc_root.join("net")).unwrap();
    std::fs::write(
        app.config.proc_root.join("net/tcp"),
        format!("header\n0: 0100007F:{port:04X} 00000000:0000 0A 0:0 0:0 0 1000 0 45678\n"),
    )
    .unwrap();
    let cli = root.join("systemctl");
    std::fs::write(&cli,format!(r#"#!/bin/sh
[ "$1" = --user ] && shift
printf '%s\n' "$*" >> '{root_path}/systemctl.calls'
if [ "$1" = show ] && [ "$2" = megacube-model@native.service ] && [ -f '{root_path}/state/store-runtime/native.json' ]; then
  if [ -f '{root_path}/replacement-on-save' ]; then
printf 'ActiveState=active\nSubState=running\nMainPID=9999\nLoadState=loaded\nUnitFileState=disabled\n'; exit 0
  fi
  if [ -f '{root_path}/boot-unknown-on-save' ]; then
printf 'ActiveState=active\nSubState=running\nMainPID=1234\nLoadState=loaded\nUnitFileState=unknown\n'; exit 0
  fi
fi
case "$*" in
  *'--property=Restart --value'*) printf 'no\n';;
  'show megacube-model@native.service '*)
if [ -f '{root_path}/no-native' ]; then
  printf 'ActiveState=inactive\nSubState=dead\nMainPID=0\nLoadState=loaded\nUnitFileState=disabled\n'
else
  printf 'ActiveState=active\nSubState=running\nMainPID=1234\nLoadState=loaded\nUnitFileState=disabled\n'
fi;;
  'show '*)
if [ -f '{root_path}/current-active' ]; then
  printf 'ActiveState=active\nSubState=running\nMainPID=1234\nLoadState=loaded\nUnitFileState=disabled\n'
else
  printf 'ActiveState=inactive\nSubState=dead\nMainPID=0\nLoadState=not-found\nUnitFileState=disabled\n'
fi;;
  'is-enabled '*) printf 'disabled\n';;
esac
exit 0
"#,root_path=root.display())).unwrap();
    std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
    app.systemd = crate::systemd::Systemd::new(
        app.config.systemd_mode,
        app.config.unit_dir.clone(),
        app.config.env_dir(),
        "test".into(),
    )
    .with_state_namespace(true)
    .with_test_cli(&cli);
    let app = Arc::new(app);
    let card = find_card(&app, "native").unwrap();
    Fixture {
        root,
        app,
        card,
        stop,
        health: Some(health),
    }
}
#[test]
fn health_fixture_waits_for_complete_fragmented_headers() {
    let f = fixture("native-health-fragmented-request");
    let port = f.card.deploy.as_ref().unwrap().port;
    let mut connection = std::net::TcpStream::connect(("127.0.0.1", port)).unwrap();
    connection
        .set_read_timeout(Some(std::time::Duration::from_millis(100)))
        .unwrap();
    connection
        .write_all(b"GET /v1/models HTTP/1.1\r\n")
        .unwrap();
    let mut byte = [0];
    let error = connection.read(&mut byte).unwrap_err();
    assert!(matches!(
        error.kind(),
        std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
    ));
    connection
        .write_all(b"Host: localhost\r\nConnection: close\r\n\r\n")
        .unwrap();
    connection
        .set_read_timeout(Some(std::time::Duration::from_secs(3)))
        .unwrap();
    let mut response = String::new();
    connection.read_to_string(&mut response).unwrap();
    assert!(response.starts_with("HTTP/1.1 200 OK\r\n"));
    assert!(response.ends_with(r#"{"data":[{"id":"Native"}]}"#));
}
#[test]
fn readonly_preflight_and_explicit_enable_retain_discovered_legacy_binding() {
    let f = fixture("native-explicit-legacy-binding");
    let profile = native_profile::resolve(&f.app, &f.card).unwrap();
    let discovered = runtime::discover(&f.app, &f.card).unwrap().unwrap();
    assert_eq!(discovered.pid, 1234);
    let discovered_by_preflight = verify_native_runtime(&f.app, &f.card, &profile).unwrap();
    assert!(discovered_by_preflight.is_some());
    assert!(
        crate::store_adoption::native_state(&f.app, "native")
            .unwrap()
            .is_none(),
        "preflight must remain read-only"
    );
    assert!(!f.app.config.state_dir.join("store-runtime").exists());
    let result = enable_inner(
        &f.app,
        "enable-task",
        &f.card,
        f.card.deploy.as_ref().unwrap(),
    );
    let calls = std::fs::read_to_string(f.root.join("systemctl.calls")).unwrap();
    let native = crate::store_adoption::native_state(&f.app, "native").unwrap();
    println!(
        "explicit enable result={result:?}; stored_binding={}; cli_calls={calls:?}",
        native.is_some()
    );
    assert!(result.is_ok(),"existing healthy legacy runtime must be retained without requiring budget for a duplicate: {result:?}");
    assert_eq!(native.unwrap().main_pid, 1234);
    assert!(
        !calls
            .lines()
            .any(|line| line.starts_with("start ") || line.starts_with("stop ")),
        "existing runtime must not be started or stopped"
    );
    assert!(
        calls
            .lines()
            .any(|line| line.starts_with("show megacube-model@native.service --property=Restart")),
        "legacy independent recovery must be verified"
    );
}
fn owned(f: &Fixture) -> bool {
    f.app
        .store
        .lock()
        .unwrap()
        .state
        .models
        .get("native")
        .is_some_and(|m| m.lifecycle_owned)
}
fn enable(f: &Fixture) -> Result<String, String> {
    enable_inner(
        &f.app,
        "enable-task",
        &f.card,
        f.card.deploy.as_ref().unwrap(),
    )
}
fn calls(f: &Fixture) -> String {
    std::fs::read_to_string(f.root.join("systemctl.calls")).unwrap_or_default()
}
#[test]
fn binding_save_failure_cannot_arm_recovery_or_grant_control() {
    let f = fixture("native-binding-save-error");
    std::fs::create_dir_all(&f.app.config.state_dir).unwrap();
    std::fs::write(
        f.app.config.state_dir.join("store-runtime"),
        b"preserve unknown entry",
    )
    .unwrap();
    let result = enable(&f);
    println!("binding save failure: {result:?}");
    assert!(result.is_err());
    assert!(!owned(&f));
    assert!(!crate::recovery::managed(&f.app, "native"));
    assert_eq!(
        std::fs::read(f.app.config.state_dir.join("store-runtime")).unwrap(),
        b"preserve unknown entry"
    );
    assert!(!f
        .app
        .systemd
        .unit_dir
        .join("megacube-model@native.service.d")
        .exists());
    assert!(!calls(&f).lines().any(|v| v.starts_with("start ")
        || v.starts_with("stop ")
        || v.starts_with("is-enabled ")));
}
#[test]
fn discovery_mismatch_cannot_write_binding_or_control() {
    let f = fixture("native-binding-profile-mismatch");
    let path = f.app.config.proc_root.join("1234/cmdline");
    let raw = std::fs::read_to_string(&path)
        .unwrap()
        .replace("32768", "262144");
    std::fs::write(path, raw).unwrap();
    assert!(enable(&f)
        .unwrap_err()
        .starts_with("native_instance_mismatch"));
    assert!(!owned(&f));
    assert!(!f.app.config.state_dir.join("store-runtime").exists());
    assert!(!crate::recovery::managed(&f.app, "native"));
    let scoped = f.app.systemd.unit_name_for("native");
    let prefix = scoped.split_once('@').unwrap().0;
    assert!(!f
        .app
        .systemd
        .unit_dir
        .join(format!("{prefix}@.service"))
        .exists());
}
#[test]
fn unknown_generation_or_boot_after_save_cannot_grant_control() {
    for flag in ["replacement-on-save", "boot-unknown-on-save"] {
        let f = fixture(&format!("native-binding-{flag}"));
        std::fs::write(f.root.join(flag), b"test transition").unwrap();
        let result = enable(&f);
        println!("{flag}: {result:?}");
        assert!(result.is_err());
        assert!(!owned(&f));
        assert!(!crate::recovery::managed(&f.app, "native"));
        assert!(
            f.app
                .config
                .state_dir
                .join("store-runtime/native.json")
                .is_file(),
            "uncertain binding must remain recoverable"
        );
        assert!(!f
            .app
            .systemd
            .unit_dir
            .join("megacube-model@native.service.d")
            .exists());
        assert!(!calls(&f).lines().any(|v| v.starts_with("start ")
            || v.starts_with("stop ")
            || v.starts_with("disable ")));
    }
}
#[test]
fn direct_process_without_unit_is_retained() {
    let f = fixture("native-direct-process-binding");
    std::fs::write(
        f.app.config.proc_root.join("1234/cgroup"),
        "0::/user.slice/app.scope\n",
    )
    .unwrap();
    assert!(enable(&f).is_ok());
    assert!(owned(&f));
    assert_eq!(
        crate::store_adoption::native_state(&f.app, "native")
            .unwrap()
            .unwrap()
            .main_pid,
        1234
    );
    assert!(!calls(&f)
        .lines()
        .any(|v| v.starts_with("start ") || v.starts_with("stop ")));
    assert!(!f
        .app
        .systemd
        .unit_dir
        .join("megacube-model@native.service.d")
        .exists());
}
#[test]
fn existing_standalone_current_unit_needs_no_store_binding() {
    let f = fixture("native-existing-own-current-unit");
    let unit = f.app.systemd.unit_name_for("native");
    std::fs::write(f.root.join("current-active"), b"active").unwrap();
    std::fs::write(
        f.app.config.proc_root.join("1234/cgroup"),
        format!("0::/user.slice/{unit}\n"),
    )
    .unwrap();
    let result = enable(&f);
    println!("standalone active: {result:?}");
    assert!(result.is_ok());
    assert!(owned(&f));
    assert!(crate::store_adoption::native_state(&f.app, "native")
        .unwrap()
        .is_none());
    assert!(!calls(&f)
        .lines()
        .any(|v| v.starts_with("start ") || v.starts_with("stop ")));
}
#[test]
fn absent_standalone_runtime_preserves_normal_budget_admission() {
    let mut f = fixture("native-absent-standalone-runtime");
    f.stop.store(true, Ordering::Relaxed);
    f.health.take().unwrap().join().unwrap();
    std::fs::remove_dir_all(f.app.config.proc_root.join("1234")).unwrap();
    std::fs::write(f.root.join("no-native"), b"absent").unwrap();
    let result = enable(&f);
    println!("absent standalone: {result:?}");
    assert!(
        result.unwrap_err().contains("内存不够"),
        "normal no-runtime model must reach ordinary budget admission"
    );
    assert!(owned(&f));
    assert!(crate::store_adoption::native_state(&f.app, "native")
        .unwrap()
        .is_none());
    assert!(!calls(&f)
        .lines()
        .any(|v| v.starts_with("start ") || v.starts_with("stop ")));
}
