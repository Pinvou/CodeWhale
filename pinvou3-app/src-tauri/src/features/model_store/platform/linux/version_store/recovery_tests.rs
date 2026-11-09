//! A real process lease, rather than a stale DB state, determines recovery.
use super::*;
use std::fs;
use std::os::unix::fs::{PermissionsExt, symlink};
use std::os::unix::process::ExitStatusExt;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::mpsc;
use std::time::{Duration, Instant};

const CARD: &str = "recovery-process-card";
const CHILD_FLAG: &str = "PINVOU_QUOTA_RECOVERY_LOCK_CHILD";

fn candidate(root: &Path, revision: char, state: &str) -> Version {
            measured_bytes: None,
    let root = root.join(revision.to_string());
    fs::create_dir_all(&root).unwrap();
    fs::write(
        root.join("partial.incomplete"),
        b"preserve interrupted bytes",
    )
    .unwrap();
    let version = Version {
            measured_bytes: None,
        card_id: CARD.into(),
        version_id: revision.to_string().repeat(64),
        state: state.into(),
        record: None,
        models_root: Some(root.to_string_lossy().into_owned()),
        runtime_card_id: format!("recovery-runtime-{revision}"),
        resource_dir: Some(root.to_string_lossy().into_owned()),
        managed: true,
        created_at: "2026-10-28T00:00:00Z".into(),
        error: (state == "failed").then(|| "earlier failure".into()),
        planned_bytes: if state == "downloading" {
            2 * 1024 * 1024 * 1024
        } else {
            0
        },
    };
    put(&version).unwrap();
    version
}

fn snapshot(version: &Version) -> serde_json::Value {
    serde_json::to_value(get(&version.card_id, &version.version_id).unwrap().unwrap()).unwrap()
}

fn recover_under_quota_lock() -> Result<(), String> {
    let _quota = wait_quota_reservation_lock(|| false, || {})?;
    recover_interrupted_for_quota()
}

struct LockChild(Child);

impl LockChild {
    fn kill_and_reap(&mut self) -> std::process::ExitStatus {
        self.0.kill().unwrap();
        self.0.wait().unwrap()
    }
}

impl Drop for LockChild {
    fn drop(&mut self) {
        // Also reap on a failed assertion: no orphan may retain a test lock.
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn operation_path() -> PathBuf {
    super::super::config::data_dir()
        .join("operation-locks")
        .join(format!("{CARD}.lock"))
}

#[test]
fn quota_recovery_retains_a_live_process_plan_then_recovers_after_sigkill() {
    if let Some(ready) = std::env::var_os(CHILD_FLAG) {
        let path = operation_path();
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        let _lease = crate::platform::filesystem::open_private_lock_file(&path).unwrap();
        _lease.try_lock().unwrap();
        fs::write(ready, b"card lease held").unwrap();
        // Parent SIGKILL must release the descriptor; unwinding this scope
        // would not reproduce an abruptly terminated downloader.
        std::thread::sleep(Duration::from_secs(30));
        panic!("parent did not terminate the lock-holder child");
    }

    let home = super::super::config::TempHome::new();
    db::init_db().unwrap();
    let interrupted = candidate(&home.path().join("versions"), 'a', "downloading");
    let other_interrupted = candidate(&home.path().join("versions"), 'b', "downloading");
    let ready_version = candidate(&home.path().join("versions"), 'c', "ready");
    let failed_version = candidate(&home.path().join("versions"), 'd', "failed");
    let ready_before = snapshot(&ready_version);
    let failed_before = snapshot(&failed_version);
    let ready = home.path().join("child-card-lock-ready");
    let module = module_path!().split_once("::").unwrap().1;
    let test =
        format!("{module}::quota_recovery_retains_a_live_process_plan_then_recovers_after_sigkill");
    let mut child = LockChild(
        Command::new(std::env::current_exe().unwrap())
            .args(["--exact", &test, "--nocapture", "--test-threads=1"])
            .env(CHILD_FLAG, &ready)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap(),
    );
    let deadline = Instant::now() + Duration::from_secs(5);
    while !ready.is_file() {
        assert!(
            child.0.try_wait().unwrap().is_none(),
            "lock-holder child exited before handshake"
        );
        assert!(
            Instant::now() < deadline,
            "lock-holder child never acquired its card lease"
        );
        std::thread::sleep(Duration::from_millis(5));
    }

    // A blocking card-lock acquisition would deadlock against a worker that
    // already owns the card and is waiting for quota. Bound that attempt, then
    // always terminate/reap the child before asserting or leaving TempHome.
    let (completed, receive) = mpsc::channel();
    let recovery = std::thread::spawn(move || {
        completed.send(recover_under_quota_lock()).unwrap();
    });
    let while_live = receive.recv_timeout(Duration::from_secs(5));
    let live_snapshot = snapshot(&interrupted);
    let status = child.kill_and_reap();
    recovery.join().unwrap();
    assert_eq!(status.signal(), Some(9), "child must die via SIGKILL");
    while_live
        .expect("quota recovery must not wait for an active card")
        .unwrap();
    assert_eq!(live_snapshot, serde_json::to_value(&interrupted).unwrap());
    assert_eq!(
        snapshot(&other_interrupted),
        serde_json::to_value(&other_interrupted).unwrap()
    );

    // The old rows remain 'downloading' after SIGKILL; the next unrelated quota
    // admission must recover all revisions once the OS releases this card lock.
    recover_under_quota_lock().unwrap();
    for before in [&interrupted, &other_interrupted] {
        let recovered = get(&before.card_id, &before.version_id).unwrap().unwrap();
        assert_eq!(recovered.state, "failed");
        assert_eq!(recovered.planned_bytes, 0);
        assert_eq!(
            recovered.error.as_deref(),
            Some("version_download_interrupted")
        );
        assert_eq!(recovered.created_at, before.created_at);
        assert_eq!(recovered.models_root, before.models_root);
        assert_eq!(recovered.resource_dir, before.resource_dir);
        assert_eq!(recovered.runtime_card_id, before.runtime_card_id);
        assert_eq!(
            fs::read(Path::new(before.models_root.as_deref().unwrap()).join("partial.incomplete"))
                .unwrap(),
            b"preserve interrupted bytes"
        );
    }
    assert_eq!(snapshot(&ready_version), ready_before);
    assert_eq!(snapshot(&failed_version), failed_before);
}

#[test]
fn quota_recovery_rejects_unsafe_lock_paths_without_releasing_the_plan() {
    for kind in ["symlink", "directory", "permission"] {
        let home = super::super::config::TempHome::new();
        db::init_db().unwrap();
        let interrupted = candidate(&home.path().join("versions"), 'a', "downloading");
        let before = snapshot(&interrupted);
        let path = operation_path();
        let directory = path.parent().unwrap();
        fs::create_dir_all(directory).unwrap();
        let unrelated = home.path().join("unrelated");
        fs::write(&unrelated, b"do not alter lock targets").unwrap();
        fs::set_permissions(&unrelated, fs::Permissions::from_mode(0o644)).unwrap();
        match kind {
            "symlink" => symlink(&unrelated, &path).unwrap(),
            "directory" => fs::create_dir(&path).unwrap(),
            "permission" => {
                fs::set_permissions(directory, fs::Permissions::from_mode(0o000)).unwrap();
                if !matches!(path.symlink_metadata(), Err(error) if error.kind() == std::io::ErrorKind::PermissionDenied)
                {
                    fs::set_permissions(directory, fs::Permissions::from_mode(0o700)).unwrap();
                    eprintln!(
                        "permission-denial subcase not executed: this process bypasses directory mode bits"
                    );
                    continue;
                }
            }
            _ => unreachable!(),
        }
        let outcome = recover_under_quota_lock();
        if kind == "permission" {
            fs::set_permissions(directory, fs::Permissions::from_mode(0o700)).unwrap();
        }
        assert!(
            outcome.is_err(),
            "{kind} must fail closed, not act like a busy lock"
        );
        assert_eq!(snapshot(&interrupted), before);
        assert_eq!(fs::read(&unrelated).unwrap(), b"do not alter lock targets");
        assert_eq!(
            fs::metadata(&unrelated).unwrap().permissions().mode() & 0o777,
            0o644
        );
        assert_eq!(
            fs::read(
                Path::new(interrupted.models_root.as_deref().unwrap()).join("partial.incomplete")
            )
            .unwrap(),
            b"preserve interrupted bytes"
        );
    }
}
