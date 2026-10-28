use super::*;
use crate::features::model_store::platform::linux::{config, orphans};
use std::process::{Child, Command};

struct Descendant {
    pid: i32,
    starttime: u64,
}

impl Drop for Descendant {
    fn drop(&mut self) {
        if identity(self.pid)
            .ok()
            .flatten()
            .is_some_and(|current| current.starttime == self.starttime && alive(&current))
        {
            let _ = nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(self.pid),
                nix::sys::signal::Signal::SIGKILL,
            );
        }
    }
}

struct Sleeper(Child);

impl Sleeper {
    fn new() -> Self {
        Self(Command::new("sleep").arg("60").spawn().unwrap())
    }
}

impl Drop for Sleeper {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[test]
fn legacy_recovery_checks_trusted_card_identity_without_sending_signals() {
    let _home = config::TempHome::new();
    let mut child = Sleeper::new();
    let _receipt = orphans::RunGuard::register_for_card(
        child.0.id() as i32,
        false,
        "a human label is not a card identity",
        Some("writer-card"),
    );
    let original = fs::read(record_file()).unwrap();
    assert!(is_recovery_blocked("writer-card").unwrap());
    assert!(!is_recovery_blocked("another-card").unwrap());
    assert!(child.0.try_wait().unwrap().is_none());
    assert_eq!(fs::read(record_file()).unwrap(), original);
}

#[test]
fn legacy_recovery_unknown_card_receipt_blocks_instead_of_guessing_from_label() {
    let _home = config::TempHome::new();
    let mut child = Sleeper::new();
    let _receipt = orphans::RunGuard::register(child.0.id() as i32, true, "unrelated label");
    assert!(is_recovery_blocked("any-card").unwrap());
    assert!(child.0.try_wait().unwrap().is_none());
}

#[test]
fn legacy_recovery_rejects_corrupt_or_unreadable_receipts_without_replacing_them() {
    for malformed in [true, false] {
        let _home = config::TempHome::new();
        let path = record_file();
        if malformed {
            fs::write(&path, b"broken interrupted JSON").unwrap();
        } else {
            fs::create_dir(&path).unwrap();
            fs::write(path.join("keep"), b"keep").unwrap();
        }
        assert!(is_recovery_blocked("any-card").is_err());
        if malformed {
            assert_eq!(fs::read(path).unwrap(), b"broken interrupted JSON");
        } else {
            assert_eq!(fs::read(path.join("keep")).unwrap(), b"keep");
        }
    }
}

#[test]
fn legacy_recovery_ignores_proven_old_boot_and_pid_reuse() {
    let _home = config::TempHome::new();
    let mut child = Sleeper::new();
    let _receipt = orphans::RunGuard::register(child.0.id() as i32, false, "old receipt");
    let raw = fs::read(record_file()).unwrap();
    let mut records: Vec<RunRecord> = serde_json::from_slice(&raw).unwrap();
    records[0].boot_id = "malformed-boot".into();
    fs::write(record_file(), serde_json::to_vec(&records).unwrap()).unwrap();
    assert!(is_recovery_blocked("any-card").is_err());
    records[0].boot_id = uuid::Uuid::new_v4().to_string();
    fs::write(record_file(), serde_json::to_vec(&records).unwrap()).unwrap();
    assert!(!is_recovery_blocked("any-card").unwrap());

    let mut records: Vec<RunRecord> = serde_json::from_slice(&raw).unwrap();
    records[0].starttime = records[0].starttime.saturating_add(1);
    fs::write(record_file(), serde_json::to_vec(&records).unwrap()).unwrap();
    assert!(!is_recovery_blocked("any-card").unwrap());
    assert!(child.0.try_wait().unwrap().is_none());
}

#[test]
fn legacy_recovery_permission_errors_preserve_receipts_and_live_processes() {
    use std::os::unix::fs::PermissionsExt;
    struct Restore(std::path::PathBuf, fs::Permissions);
    impl Drop for Restore {
        fn drop(&mut self) {
            fs::set_permissions(&self.0, self.1.clone()).unwrap();
        }
    }
    let _home = config::TempHome::new();
    let mut child = Sleeper::new();
    let _receipt = orphans::RunGuard::register(child.0.id() as i32, false, "legacy writer");
    let path = record_file();
    let original = fs::read(&path).unwrap();
    let restore = Restore(path.clone(), fs::metadata(&path).unwrap().permissions());
    fs::set_permissions(&path, fs::Permissions::from_mode(0o000)).unwrap();
    if fs::read(&path).is_err() {
        assert!(is_recovery_blocked("card").is_err());
    } else {
        eprintln!("receipt permission refusal unavailable to this privileged test runner");
    }
    drop(restore);
    assert_eq!(fs::read(path).unwrap(), original);
    assert!(child.0.try_wait().unwrap().is_none());
}

#[test]
fn legacy_recovery_retains_plan_after_the_leader_exits_but_its_descendant_lives() {
    use std::os::unix::process::CommandExt;
    let home = config::TempHome::new();
    let child_path = home.path().join("writer-pid");
    let release = home.path().join("release-launcher");
    let mut command = Command::new("sh");
    command
        .args([
            "-c",
            "sleep 60 & echo $! > \"$1\"; while [ ! -e \"$2\" ]; do sleep 0.01; done",
            "legacy-recovery-fixture",
        ])
        .arg(&child_path)
        .arg(&release);
    // Only the async-signal-safe setpgid syscall runs between fork and exec.
    // Match existing orphan fixtures, including user-mode emulation support.
    unsafe {
        command.pre_exec(|| {
            nix::unistd::setpgid(nix::unistd::Pid::from_raw(0), nix::unistd::Pid::from_raw(0))
                .map_err(std::io::Error::from)
        });
    }
    let mut leader = Sleeper(command.spawn().unwrap());
    let pid = leader.0.id() as i32;
    assert_eq!(identity(pid).unwrap().unwrap().pgid, pid);
    let _receipt = orphans::RunGuard::register_for_card(pid, false, "legacy writer", Some("card"));
    let deadline = Instant::now() + Duration::from_secs(5);
    let child_pid = loop {
        if let Ok(raw) = fs::read_to_string(&child_path) {
            if let Ok(pid) = raw.trim().parse::<i32>() {
                break pid;
            }
        }
        assert!(
            Instant::now() < deadline,
            "launcher did not publish its descendant"
        );
        std::thread::sleep(Duration::from_millis(5));
    };
    let child = Descendant {
        pid: child_pid,
        starttime: identity(child_pid).unwrap().unwrap().starttime,
    };
    fs::write(release, b"release").unwrap();
    assert!(leader.0.wait().unwrap().success());
    assert!(is_recovery_blocked("card").unwrap());
    assert!(alive(&identity(child_pid).unwrap().unwrap()));
    drop(child);
    let deadline = Instant::now() + Duration::from_secs(5);
    while is_recovery_blocked("card").unwrap() {
        assert!(
            Instant::now() < deadline,
            "stopped descendants still blocked recovery"
        );
        std::thread::sleep(Duration::from_millis(5));
    }
}
