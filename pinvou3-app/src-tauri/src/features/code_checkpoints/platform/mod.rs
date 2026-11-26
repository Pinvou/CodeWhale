//! Windows needs to discard stat metadata that can hide equal-length edits.

use std::fs::{self, File};
use std::path::Path;
use std::process::Command;
use std::time::{Duration, Instant};

use anyhow::{Context, Result};

use super::{checkpoints_dir, git_ok};

/// A Windows checkpoint operation still holds the workspace or metadata lock.
#[derive(Debug, thiserror::Error)]
#[error("检查点影子仓库正在使用")]
pub struct CheckpointWorkspaceBusy;

pub(super) fn lock_workspace(ledger_root: &Path) -> Result<Option<File>> {
    lock_repository(ledger_root, "workspace.lock", Duration::ZERO)
}

pub(super) fn lock_metadata(ledger_root: &Path, wait: Duration) -> Result<Option<File>> {
    lock_repository(ledger_root, "metadata.lock", wait)
}

fn lock_repository(ledger_root: &Path, name: &str, wait: Duration) -> Result<Option<File>> {
    if !cfg!(windows) {
        return Ok(None);
    }
    let dir = checkpoints_dir(ledger_root);
    fs::create_dir_all(&dir)?;
    let lock = crate::platform::filesystem::open_private_lock_file(&dir.join(name))?;
    let deadline = Instant::now() + wait;
    loop {
        match lock.try_lock() {
            Ok(()) => return Ok(Some(lock)),
            Err(fs::TryLockError::WouldBlock) => {
                let remaining = deadline.saturating_duration_since(Instant::now());
                if remaining.is_zero() {
                    return Err(CheckpointWorkspaceBusy.into());
                }
                std::thread::sleep(remaining.min(Duration::from_millis(25)));
            }
            Err(fs::TryLockError::Error(error)) => {
                return Err(error).context("锁定检查点影子仓库失败");
            }
        }
    }
}

pub(super) const fn list_can_quarantine() -> bool {
    // Read-only callers cannot rename the live index while a writer owns it.
    !cfg!(windows)
}

pub(super) fn constrain_git(command: &mut Command) {
    if cfg!(windows) {
        command.args([
            "-c",
            "core.hooksPath=/dev/null",
            "-c",
            "core.fsmonitor=false",
        ]);
    }
}

pub(super) fn constrain_attributes(info_dir: &Path) -> Result<()> {
    if cfg!(windows) {
        // Git reads this file ahead of workspace .gitattributes. Keep built-in
        // line ending and other Git attribute behavior, but disable named
        // external filters before forcing an index-wide content refresh.
        crate::platform::filesystem::atomic_write_private(
            &info_dir.join("attributes"),
            b"* -filter\n",
        )?;
    }
    Ok(())
}

pub(super) fn stage_workspace(repo: &Path, execution_root: &Path) -> Result<()> {
    if cfg!(windows) {
        let tree = git_ok(repo, execution_root, &["write-tree"])?;
        git_ok(repo, execution_root, &["read-tree", tree.trim()])?;
    }
    git_ok(repo, execution_root, &["add", "-A"])?;
    Ok(())
}

#[cfg(all(test, windows))]
mod tests {
    use super::*;
    use crate::features::code_checkpoints::{
        CheckpointKind, create_checkpoint, diff_checkpoint, git_available, repo_dir,
        restore_checkpoint,
    };
    use std::time::{Duration, UNIX_EPOCH};

    #[test]
    fn windows_checkpoint_rehashes_equal_length_edits_with_the_same_mtime() {
        assert!(
            git_available(),
            "Windows checkpoint regression requires Git"
        );
        let ledger = tempfile::tempdir().unwrap();
        let exec = tempfile::tempdir().unwrap();
        let changed_path = exec.path().join("tracked.txt");
        let mtime = UNIX_EPOCH + Duration::from_secs(946_684_800);
        let write_at_same_time = |text: &str| {
            fs::write(&changed_path, text).unwrap();
            fs::File::options()
                .write(true)
                .open(&changed_path)
                .unwrap()
                .set_times(fs::FileTimes::new().set_modified(mtime))
                .unwrap();
            assert_eq!(
                fs::metadata(&changed_path).unwrap().modified().unwrap(),
                mtime
            );
        };
        write_at_same_time("v1\n");
        fs::write(exec.path().join("removed.txt"), "will be removed\n").unwrap();
        let first = create_checkpoint(
            ledger.path(),
            exec.path(),
            Some(1),
            CheckpointKind::Turn,
            "first",
        )
        .unwrap();

        let discard = create_checkpoint(
            ledger.path(),
            exec.path(),
            Some(2),
            CheckpointKind::Turn,
            "unsent",
        )
        .unwrap();
        let lock = lock_workspace(ledger.path()).unwrap().unwrap();
        let rejected = diff_checkpoint(ledger.path(), exec.path(), &first.id, false).unwrap_err();
        assert!(rejected.is::<CheckpointWorkspaceBusy>());
        assert!(super::super::drop_checkpoint(ledger.path(), &discard.id).unwrap());
        drop(lock);

        write_at_same_time("v2\n");
        fs::write(exec.path().join(".gitignore"), "tracked.txt\n").unwrap();
        fs::write(
            exec.path().join(".gitattributes"),
            "*.txt filter=project text eol=lf\n",
        )
        .unwrap();
        fs::write(exec.path().join(".env"), "excluded fixture\n").unwrap();
        fs::write(exec.path().join("new.txt"), "new file\n").unwrap();
        fs::remove_file(exec.path().join("removed.txt")).unwrap();
        let diff = diff_checkpoint(ledger.path(), exec.path(), &first.id, true).unwrap();
        for expected in ["tracked.txt", "removed.txt", "new.txt"] {
            assert!(
                diff.changes.iter().any(|change| change.path == expected),
                "{expected} missing: {:?}",
                diff.changes
            );
        }
        assert!(diff.patch.contains("+v2"));
        assert!(!diff.changes.iter().any(|change| change.path == ".env"));
        assert!(!diff.patch.contains("excluded fixture"));

        write_at_same_time("v3\n");
        let second = create_checkpoint(
            ledger.path(),
            exec.path(),
            Some(2),
            CheckpointKind::Turn,
            "second",
        )
        .unwrap();
        let repo = repo_dir(ledger.path());
        let attributes = git_ok(
            &repo,
            exec.path(),
            &["check-attr", "filter", "text", "--", "tracked.txt"],
        )
        .unwrap();
        assert!(attributes.contains("tracked.txt: filter: unset"));
        assert!(attributes.contains("tracked.txt: text: set"));
        let tracked = |commit| {
            git_ok(
                &repo,
                exec.path(),
                &["show", &format!("{commit}:tracked.txt")],
            )
            .unwrap()
        };
        assert_eq!(tracked(&first.commit), "v1\n");
        assert_eq!(tracked(&second.commit), "v3\n");
        write_at_same_time("v4\n");
        let undo = restore_checkpoint(ledger.path(), exec.path(), &first.id).unwrap();
        assert_eq!(fs::read_to_string(changed_path).unwrap(), "v1\n");
        assert_eq!(tracked(&undo.commit), "v4\n");
        assert_eq!(
            fs::read_to_string(exec.path().join(".env")).unwrap(),
            "excluded fixture\n"
        );
    }
}
