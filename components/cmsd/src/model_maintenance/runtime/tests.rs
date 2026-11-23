use super::*;
use crate::app::App;
use crate::docker::fixtures as fx;
use std::os::unix::fs::PermissionsExt;
use std::path::PathBuf;
use std::sync::Arc;

fn fixture(tag: &str) -> (PathBuf, Arc<App>, u16) {
    let root = fx::tmpdir(tag);
    let mut app = Arc::try_unwrap(fx::make_app(&root, 128, 100)).ok().unwrap();
    app.config.proc_root = root.join("proc");
    std::fs::create_dir_all(&app.config.proc_root).unwrap();
    let port = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let card = app.config.cards_dir.join("native");
    let weights = root.join("weights");
    std::fs::create_dir_all(&card).unwrap();
    std::fs::create_dir_all(&weights).unwrap();
    std::fs::write(weights.join("model.tachyon"), b"data").unwrap();
    std::fs::write(card.join("common.sh"), "ENGINE_BIN_SHA256=1111111111111111\nMODEL_ID=TestNative\nARTIFACT=model.tachyon\nTOTAL_BYTES=4\n").unwrap();
    std::fs::write(card.join(".deploy-env"), format!("PORT={port}\nHOST=127.0.0.1\nMODEL_DIR={}\nENGINE_DIR={}/11111111\nARTIFACT=model.tachyon\nMODEL_ID=TestNative\nMAX_CONCURRENCY=1\n", weights.display(),app.config.engine_root.display())).unwrap();
    let cli = root.join("systemctl-fixture");
    std::fs::write(&cli,format!(r##"#!/bin/sh
printf '%s\n' "$*" >> '{}'
[ "$1" = --user ] && shift
state=inactive
if [ "$2" = megacube-model@native.service ] && [ -f '{}' ]; then state=$(cat '{}'); fi
[ "$state" = unknown ] && exit 1
case "$1" in
 show)
  pid=0
  active="$state"
  [ "$state" = active ] && pid=412
  [ "$state" = enabled ] && active=inactive
  printf 'ActiveState=%s\nSubState=dead\nMainPID=%s\nNRestarts=0\nResult=success\nLoadState=loaded\nUnitFileState=disabled\n' "$active" "$pid";;
 is-enabled) [ "$state" = enabled ] && echo enabled && exit 0; echo disabled; exit 1;;
 *) exit 97;;
esac
"##,root.join("calls").display(),root.join("legacy-state").display(),root.join("legacy-state").display())).unwrap();
    std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
    app.systemd = crate::systemd::Systemd::new(
        app.config.systemd_mode,
        app.config.unit_dir.clone(),
        app.config.env_dir(),
        "test".into(),
    )
    .with_state_namespace(true)
    .with_test_cli(&cli);
    (root, Arc::new(app), port)
}
fn change(app: &App, action: &str) -> Result<Value, ApiErr> {
    crate::model_maintenance::change(
        app,
        "native",
        format!("{{\"action\":\"{action}\",\"owner\":\"private-owner\"}}").as_bytes(),
        crate::peer::LocalPeer::current(),
    )
}

#[test]
fn unbound_listener_blocks_reservation_and_survives_card_deletion() {
    let (_root, app, port) = fixture("maintenance-unbound-port");
    let listener = std::net::TcpListener::bind(("127.0.0.1", port)).unwrap();
    assert_eq!(
        change(&app, "begin").unwrap_err().1,
        "runtime_not_quiescent"
    );
    drop(listener);
    assert_eq!(
        change(&app, "begin")
            .unwrap()
            .get("reservation_only")
            .and_then(Value::as_bool),
        Some(true)
    );
    let task = crate::ops::purge_with_maintenance(&app, "native", Some("private-owner")).unwrap();
    assert!(app.store.lock().unwrap().state.tasks[&task]
        .status
        .is_terminal());
    std::fs::remove_dir_all(app.config.cards_dir.join("native")).unwrap();
    let listener = std::net::TcpListener::bind(("127.0.0.1", port)).unwrap();
    assert_eq!(change(&app, "end").unwrap_err().1, "runtime_not_quiescent");
    drop(listener);
    assert_eq!(
        change(&app, "end")
            .unwrap()
            .get("maintenance")
            .and_then(Value::as_bool),
        Some(false)
    );
}

#[test]
fn unbound_legacy_unit_must_be_inactive_disabled_and_known() {
    let (root, app, _) = fixture("maintenance-unbound-unit");
    for (state, expected) in [
        ("active", "runtime_not_quiescent"),
        ("enabled", "runtime_not_quiescent"),
        ("unknown", "state_unknown"),
    ] {
        std::fs::write(root.join("legacy-state"), state).unwrap();
        assert_eq!(change(&app, "begin").unwrap_err().1, expected, "{state}");
    }
    std::fs::write(root.join("legacy-state"), "inactive").unwrap();
    change(&app, "begin").unwrap();
    std::fs::remove_dir_all(app.config.cards_dir.join("native")).unwrap();
    std::fs::write(root.join("legacy-state"), "active").unwrap();
    assert_eq!(change(&app, "end").unwrap_err().1, "runtime_not_quiescent");
    std::fs::write(root.join("legacy-state"), "inactive").unwrap();
    change(&app, "end").unwrap();
    let calls = std::fs::read_to_string(root.join("calls")).unwrap();
    assert!(
        !calls
            .lines()
            .any(|line| !line.contains("show ") && !line.contains("is-enabled ")),
        "{calls}"
    );
}

#[test]
fn unbound_loading_process_and_unknown_proc_state_cannot_reserve() {
    let (_root, app, port) = fixture("maintenance-unbound-process");
    let process = app.config.proc_root.join("412");
    std::fs::create_dir_all(&process).unwrap();
    std::os::unix::fs::symlink(
        app.config.engine_root.join("11111111/tachyon-serve"),
        process.join("exe"),
    )
    .unwrap();
    // A loading process may not listen yet, and may have been allocated another port.
    std::fs::write(
        process.join("cmdline"),
        format!(
            "tachyon-serve\0/weights/model.tachyon\0--port={}\0--model-id=TestNative\0",
            port.saturating_sub(1)
        ),
    )
    .unwrap();
    assert_eq!(
        change(&app, "begin").unwrap_err().1,
        "runtime_not_quiescent"
    );
    std::fs::remove_file(process.join("cmdline")).unwrap();
    assert_eq!(change(&app, "begin").unwrap_err().1, "state_unknown");
    std::fs::remove_dir_all(process).unwrap();
    std::fs::remove_dir(&app.config.proc_root).unwrap();
    assert_eq!(change(&app, "begin").unwrap_err().1, "state_unknown");
    std::fs::create_dir(&app.config.proc_root).unwrap();
    change(&app, "begin").unwrap();
    change(&app, "end").unwrap();
}

#[test]
fn an_excessive_process_table_is_unknown_instead_of_quiescent() {
    let (_root, app, _) = fixture("maintenance-proc-bound");
    for pid in 1..=MAX_PROC_ENTRIES + 1 {
        std::fs::create_dir(app.config.proc_root.join(pid.to_string())).unwrap();
    }
    assert_eq!(change(&app, "begin").unwrap_err().1, "state_unknown");
}

fn proc_stat(pid: u32, name: &str, ticks: u64) -> String {
    format!("{pid} ({name}) S {} {ticks} 0\n", vec!["0"; 18].join(" "))
}

#[test]
fn unreadable_exe_exclusion_needs_complete_non_native_identity() {
    let (root, app, _) = fixture("maintenance-unrelated-process");
    let process = app.config.proc_root.join("412");
    std::fs::create_dir(&process).unwrap();
    std::fs::write(process.join("stat"), proc_stat(412, "(sd-pam)", 100)).unwrap();
    let reference = reference(&app, "native", None).unwrap();
    for args in [
        b"(sd-pam)\0".as_slice(),
        b"/usr/lib/systemd/systemd\0--user\0",
        b"/usr/bin/ssh-agent\0-s\0",
    ] {
        std::fs::write(process.join("cmdline"), args).unwrap();
        assert!(unrelated_process(&process, &reference).unwrap());
    }
    for args in [
        b"tachyon-serve\0".as_slice(),
        b"other\0/engine/tachyon-serve (deleted)\0",
        b"other\0--model-id\0AnotherModel\0",
        b"other\0--host=127.0.0.1\0",
        b"other\0--port\x0018100\0",
        b"other\0--max-context=32768\0",
        b"other\0--max-concurrency=1\0",
        b"other\0--device-state-slots=16\0",
        b"other\0--host-state-slots=16\0",
    ] {
        std::fs::write(process.join("cmdline"), args).unwrap();
        assert!(!unrelated_process(&process, &reference).unwrap());
    }
    for key in ["native_weights", "native_model_id"] {
        let value = reference.get(key).and_then(Value::as_str).unwrap();
        std::fs::write(process.join("cmdline"), format!("other\0{value}\0")).unwrap();
        assert!(!unrelated_process(&process, &reference).unwrap());
    }
    std::fs::write(process.join("cmdline"), b"other\0").unwrap();
    std::fs::write(process.join("stat"), proc_stat(412, "tachyon-serve", 100)).unwrap();
    assert!(!unrelated_process(&process, &reference).unwrap());
    std::fs::remove_dir_all(root).unwrap();
}

#[test]
fn unreadable_exe_exclusion_keeps_incomplete_or_untrusted_proc_files_unknown() {
    let (root, app, _) = fixture("maintenance-unrelated-process-files");
    let process = app.config.proc_root.join("412");
    std::fs::create_dir(&process).unwrap();
    std::fs::write(process.join("stat"), proc_stat(412, "other", 100)).unwrap();
    let reference = reference(&app, "native", None).unwrap();
    for args in [
        Vec::new(),
        b"partial-argv".to_vec(),
        b"\0empty-first-arg\0".to_vec(),
        vec![0xff, 0],
        vec![b'x'; 1024 * 1024 + 1],
        b"x\0".repeat(16_385),
    ] {
        std::fs::write(process.join("cmdline"), args).unwrap();
        assert!(unrelated_process(&process, &reference).is_err());
    }
    std::fs::write(process.join("cmdline"), b"other\0").unwrap();
    for stat in [
        "corrupt".into(),
        proc_stat(413, "other", 100),
        proc_stat(412, "other", 0),
        proc_stat(412, "other", 100).replace(") S ", ") Z "),
    ] {
        std::fs::write(process.join("stat"), stat).unwrap();
        assert!(unrelated_process(&process, &reference).is_err());
    }
    std::fs::write(process.join("stat"), proc_stat(412, "other", 100)).unwrap();
    std::fs::remove_file(process.join("cmdline")).unwrap();
    std::os::unix::fs::symlink("missing", process.join("cmdline")).unwrap();
    assert!(unrelated_process(&process, &reference).is_err());
    std::fs::remove_file(process.join("cmdline")).unwrap();
    use std::os::unix::ffi::OsStrExt;
    let path = std::ffi::CString::new(process.join("cmdline").as_os_str().as_bytes()).unwrap();
    // SAFETY: valid terminated path inside this test's private directory.
    assert_eq!(unsafe { libc::mkfifo(path.as_ptr(), 0o600) }, 0);
    assert!(unrelated_process(&process, &reference).is_err());
    std::fs::remove_dir_all(root).unwrap();
}

#[test]
fn nondumpable_unrelated_process_does_not_block_native_reservation_or_release() {
    use std::io::Read;
    use std::os::fd::AsRawFd;
    // Root can inspect nondumpable processes; this regression is about the
    // supported non-root desktop service and requires real ptrace denial.
    // SAFETY: geteuid has no preconditions.
    if unsafe { libc::geteuid() } == 0 {
        return;
    }
    let (root, app, _) = fixture("maintenance-nondumpable-process");
    let (mut ready, writer) = std::os::unix::net::UnixStream::pair().unwrap();
    let read_fd = ready.as_raw_fd();
    let write_fd = writer.as_raw_fd();
    // SAFETY: the child performs only the libc calls below before parking; it
    // never enters Rust allocation, locks, destructors or the test runtime.
    let pid = unsafe { libc::fork() };
    assert!(pid >= 0);
    if pid == 0 {
        // SAFETY: these are inherited valid fds and static, terminated strings.
        // The child owns its process and changes only its own dumpability/name.
        unsafe {
            libc::close(read_fd);
            if libc::prctl(libc::PR_SET_DUMPABLE, 0, 0, 0, 0) != 0
                || libc::prctl(libc::PR_SET_NAME, c"qa-unrelated".as_ptr(), 0, 0, 0) != 0
            {
                libc::_exit(99);
            }
            libc::write(write_fd, b"R".as_ptr().cast(), 1);
            libc::close(write_fd);
            loop {
                libc::pause();
            }
        }
    }
    struct Child(libc::pid_t);
    impl Drop for Child {
        fn drop(&mut self) {
            // SAFETY: this guard owns only the exact child PID just forked. It
            // has not been reaped, so that PID cannot be reused by another task.
            unsafe {
                libc::kill(self.0, libc::SIGKILL);
                libc::waitpid(self.0, std::ptr::null_mut(), 0);
            }
        }
    }
    let _child = Child(pid);
    drop(writer);
    ready
        .set_read_timeout(Some(std::time::Duration::from_secs(2)))
        .unwrap();
    let mut byte = [0];
    ready.read_exact(&mut byte).unwrap();
    assert_eq!(byte, [b'R']);
    let process = app.config.proc_root.join(pid.to_string());
    std::os::unix::fs::symlink(format!("/proc/{pid}"), &process).unwrap();
    assert_eq!(
        std::fs::read_link(process.join("exe")).unwrap_err().kind(),
        std::io::ErrorKind::PermissionDenied
    );
    assert_eq!(
        change(&app, "begin").unwrap().get("reservation_only"),
        Some(&Value::Bool(true))
    );
    assert_eq!(
        change(&app, "end").unwrap().get("maintenance"),
        Some(&Value::Bool(false))
    );
    assert!(root.join("weights/model.tachyon").is_file());
    std::fs::remove_dir_all(root).unwrap();
}
