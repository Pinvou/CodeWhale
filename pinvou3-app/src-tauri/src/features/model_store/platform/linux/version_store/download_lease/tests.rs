use super::*;
use crate::features::model_store::platform::linux::{config, script_runner};
use std::collections::HashMap;
use std::os::fd::AsRawFd;
use std::os::unix::fs::PermissionsExt;
use std::time::{Duration, Instant};

struct Writer(i32);
impl Drop for Writer {
    fn drop(&mut self) {
        if let Ok(group) = nix::unistd::getpgid(Some(nix::unistd::Pid::from_raw(self.0))) {
            assert_ne!(group, nix::unistd::getpgrp());
            let _ = nix::sys::signal::killpg(group, nix::sys::signal::Signal::SIGKILL);
        }
    }
}

#[test]
fn only_explicit_download_option_inherits_lease_and_parent_cloexec_stays_set() {
    for inherit in [false, true] {
        let home = config::TempHome::new();
        let root = home.path();
        let mut lease = acquire("inherit-test", || false, || {}).unwrap();
        let original_fd = lease.file.as_ref().unwrap().as_raw_fd();
        // SAFETY: F_GETFD only reads flags on the live parent-owned descriptor.
        let before = unsafe { libc::fcntl(original_fd, libc::F_GETFD) };
        assert_ne!(before & libc::FD_CLOEXEC, 0);
        let entry = root.join("script.sh");
        std::fs::write(
            &entry,
            r#"#!/bin/sh
sh -c 'sleep 30' >/dev/null 2>&1 &
printf '%s\n' "$!" > "$LMM_MODELS_DIR/writer.pid"
"#,
        )
        .unwrap();
        std::fs::set_permissions(&entry, std::fs::Permissions::from_mode(0o700)).unwrap();
        let env = HashMap::from([
            ("LMM_CARD_ID".into(), "inherit-test".into()),
            ("LMM_MODELS_DIR".into(), root.to_string_lossy().into_owned()),
        ]);
        let file = lease.script_file().unwrap();
        script_runner::run_script(
            &entry,
            &env,
            script_runner::RunScriptOptions {
                download_lease: inherit.then_some(file),
                ..Default::default()
            },
        )
        .unwrap();
        let writer = Writer(
            std::fs::read_to_string(root.join("writer.pid"))
                .unwrap()
                .trim()
                .parse()
                .unwrap(),
        );
        // SAFETY: the original parent OFD remains open until confirm_stopped.
        assert_eq!(unsafe { libc::fcntl(original_fd, libc::F_GETFD) }, before);
        let result = lease.confirm_stopped();
        assert_eq!(
            result.is_err(),
            inherit,
            "ordinary deployment daemons must not inherit a download lease"
        );
        drop(writer);
        let deadline = Instant::now() + Duration::from_secs(5);
        while lease.confirm_stopped().is_err() {
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(10));
        }
    }
}

#[test]
fn waiting_for_a_live_download_lease_remains_cancellable() {
    let _home = config::TempHome::new();
    let _owner = acquire("cancel-test", || false, || {}).unwrap();
    let cancelled = std::cell::Cell::new(false);
    let mut waits = 0;
    let result = acquire(
        "cancel-test",
        || cancelled.get(),
        || {
            waits += 1;
            cancelled.set(true);
        },
    );
    assert!(matches!(result, Err(error) if error.contains("已取消")));
    assert_eq!(waits, 1);
    assert!(try_acquire("cancel-test").unwrap().is_none());
}
