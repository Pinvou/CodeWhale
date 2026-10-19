//! Native launch receipts must start in a private directory, independent of umask.
use std::fs::{self, File, OpenOptions};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::Path;

fn checked_directory(path: &Path, uid: u32) -> Result<File, String> {
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(nix::libc::O_DIRECTORY | nix::libc::O_NOFOLLOW)
        .open(path)
        .map_err(|_| "原生运行目录不是可安全访问的普通目录，已保留现有数据。".to_string())?;
    if file.metadata().map_err(|e| e.to_string())?.uid() != uid {
        return Err("原生运行目录不属于当前用户，已保留现有数据。".into());
    }
    Ok(file)
}

pub(super) fn prepare(card: &Path) -> Result<(), String> {
    let uid = fs::metadata("/proc/self").map_err(|e| e.to_string())?.uid();
    let parent = checked_directory(card, uid)?;
    let path = card.join(".runtime");
    match fs::DirBuilder::new().mode(0o700).create(&path) {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {}
        Err(error) => return Err(error.to_string()),
    }
    let directory = checked_directory(&path, uid)?;
    if directory.metadata().map_err(|e| e.to_string())?.mode() & 0o022 == 0 {
        return Ok(());
    }
    // A previous failed first launch may have created only its diagnostic log.
    // Bootstrap that case only under an already private card directory. Never
    // legitimize a receipt or any other state that was writable by other users.
    if parent.metadata().map_err(|e| e.to_string())?.mode() & 0o077 != 0 {
        return Err(
            "已有原生运行目录权限不安全，无法确认归属；已保留数据，请核对目录权限。".into(),
        );
    }
    for entry in fs::read_dir(&path).map_err(|e| e.to_string())? {
        let entry = entry.map_err(|e| e.to_string())?;
        let metadata = entry.path().symlink_metadata().map_err(|e| e.to_string())?;
        if entry.file_name() != "healthcheck.log"
            || !metadata.is_file()
            || metadata.uid() != uid
            || metadata.nlink() != 1
        {
            return Err(
                "已有原生运行目录包含需核对的状态，未修改权限或认领进程；请联系管理员核对。".into(),
            );
        }
    }
    directory
        .set_permissions(fs::Permissions::from_mode(0o700))
        .map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn card() -> tempfile::TempDir {
        let temp = tempfile::tempdir().unwrap();
        fs::set_permissions(temp.path(), fs::Permissions::from_mode(0o700)).unwrap();
        temp
    }
    #[test]
    fn new_native_runtime_is_private() {
        let temp = card();
        prepare(temp.path()).unwrap();
        assert_eq!(
            fs::metadata(temp.path().join(".runtime")).unwrap().mode() & 0o777,
            0o700
        );
    }
    #[test]
    fn failed_first_launch_with_only_diagnostics_can_be_bootstrapped() {
        let temp = card();
        let path = temp.path().join(".runtime");
        fs::create_dir(&path).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o775)).unwrap();
        fs::write(path.join("healthcheck.log"), "previous failure").unwrap();
        prepare(temp.path()).unwrap();
        assert_eq!(fs::metadata(&path).unwrap().mode() & 0o777, 0o700);
        assert_eq!(
            fs::read_to_string(path.join("healthcheck.log")).unwrap(),
            "previous failure"
        );
    }
    #[test]
    fn writable_existing_process_state_is_never_promoted_to_private_ownership() {
        for name in [
            "serve.identity.json",
            "serve.pid",
            "process.lock",
            "other-state",
        ] {
            let temp = card();
            let path = temp.path().join(".runtime");
            fs::create_dir(&path).unwrap();
            fs::set_permissions(&path, fs::Permissions::from_mode(0o775)).unwrap();
            fs::write(path.join(name), "untrusted").unwrap();
            assert!(prepare(temp.path()).is_err());
            assert_eq!(fs::metadata(&path).unwrap().mode() & 0o777, 0o775);
            assert_eq!(fs::read_to_string(path.join(name)).unwrap(), "untrusted");
        }
    }
    #[test]
    fn linked_runtime_and_nonprivate_parent_are_not_repaired() {
        let temp = card();
        let outside = card();
        std::os::unix::fs::symlink(outside.path(), temp.path().join(".runtime")).unwrap();
        assert!(prepare(temp.path()).is_err());
        fs::remove_file(temp.path().join(".runtime")).unwrap();
        fs::create_dir(temp.path().join(".runtime")).unwrap();
        fs::set_permissions(temp.path(), fs::Permissions::from_mode(0o775)).unwrap();
        fs::set_permissions(
            temp.path().join(".runtime"),
            fs::Permissions::from_mode(0o775),
        )
        .unwrap();
        assert!(prepare(temp.path()).is_err());
    }
}
