//! Private bounded identity storage. Callers hold the associated file lock.
use std::fs::{File, OpenOptions};
use std::io::{Read, Write};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::Path;

const LIMIT: u64 = 64 * 1024;

pub fn directory(path: &Path, private: bool) -> Result<(), String> {
    for part in path.ancestors() {
        if std::fs::symlink_metadata(part).is_ok_and(|m| m.file_type().is_symlink()) {
            return Err("identity directory cannot be linked".into());
        }
    }
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(path)
        .map_err(|e| e.to_string())?;
    validate_directory(path, private)
}

/// Inspect an existing directory without creating or repairing it.
pub fn validate_directory(path: &Path, private: bool) -> Result<(), String> {
    for part in path.ancestors() {
        if std::fs::symlink_metadata(part).is_ok_and(|m| m.file_type().is_symlink()) {
            return Err("identity directory cannot be linked".into());
        }
    }
    let m = std::fs::symlink_metadata(path).map_err(|e| e.to_string())?;
    // SAFETY: geteuid has no preconditions.
    if !m.is_dir() || m.uid() != unsafe { libc::geteuid() } || (private && m.mode() & 0o077 != 0) {
        return Err("identity directory ownership or permissions invalid".into());
    }
    Ok(())
}

pub fn secure_state(path: &Path) -> Result<(), String> {
    directory(path, false)?;
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
        .map_err(|e| e.to_string())?;
    file.set_permissions(std::fs::Permissions::from_mode(0o700))
        .map_err(|e| e.to_string())
}

fn validate(file: &File, private: bool) -> Result<(), String> {
    let m = file.metadata().map_err(|e| e.to_string())?;
    // SAFETY: geteuid has no preconditions.
    let invalid_mode = if private {
        m.mode() & 0o077 != 0
    } else {
        m.mode() & 0o002 != 0 || (m.mode() & 0o020 != 0 && m.gid() != unsafe { libc::getegid() })
    };
    if !m.is_file() || m.uid() != unsafe { libc::geteuid() } || invalid_mode {
        return Err("identity file ownership or permissions invalid".into());
    }
    Ok(())
}

/// Only an occupied lock is retryable. Metadata/open/locking failures retain
/// their failure classification; callers must not infer busy from a message.
#[derive(Debug)]
pub enum LockError {
    Busy,
    Unavailable(String),
}

pub fn lock(path: &Path) -> Result<File, LockError> {
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
        .map_err(|e| LockError::Unavailable(e.to_string()))?;
    validate(&file, true).map_err(LockError::Unavailable)?;
    file.try_lock().map_err(|error| match error {
        std::fs::TryLockError::WouldBlock => LockError::Busy,
        std::fs::TryLockError::Error(error) => LockError::Unavailable(error.to_string()),
    })?;
    Ok(file)
}

pub fn read(path: &Path) -> Result<Option<Vec<u8>>, String> {
    read_limit(path, LIMIT)
}

pub fn read_limit(path: &Path, limit: u64) -> Result<Option<Vec<u8>>, String> {
    read_file(path, limit, true)
}

/// Control receipts may already be owner-controlled 0644 files (systemd
/// drop-ins). Preserve the store's same-effective-group file contract; reject
/// world write and writable foreign groups. Private identity reads stay 0600.
/// Only an actually absent path is empty, including its parent directories.
pub fn read_control(path: &Path, limit: u64) -> Result<Option<Vec<u8>>, String> {
    let parent = path.parent().ok_or("control record parent missing")?;
    if !control_directory(parent)? {
        return Ok(None);
    }
    read_file(path, limit, false)
}

/// Inspect without creating, chmodding, or following a control directory/link.
pub fn control_directory(path: &Path) -> Result<bool, String> {
    for part in path.ancestors() {
        match std::fs::symlink_metadata(part) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err("control directory cannot be linked".into());
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.to_string()),
        }
    }
    let metadata = match std::fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(error) => return Err(error.to_string()),
    };
    if !metadata.is_dir()
        || metadata.uid() != unsafe { libc::geteuid() }
        || metadata.mode() & 0o002 != 0
        || (metadata.mode() & 0o020 != 0 && metadata.gid() != unsafe { libc::getegid() })
    {
        return Err("control directory ownership or permissions invalid".into());
    }
    Ok(true)
}

fn read_file(path: &Path, limit: u64, private: bool) -> Result<Option<Vec<u8>>, String> {
    let file = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC | libc::O_NONBLOCK)
        .open(path)
    {
        Ok(file) => file,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e.to_string()),
    };
    validate(&file, private)?;
    if file.metadata().map_err(|e| e.to_string())?.len() > limit {
        return Err("identity record exceeds limit".into());
    }
    let mut raw = Vec::new();
    file.take(limit + 1)
        .read_to_end(&mut raw)
        .map_err(|e| e.to_string())?;
    if raw.len() as u64 > limit {
        return Err("identity record exceeds limit".into());
    }
    Ok(Some(raw))
}

pub fn publish(path: &Path, raw: &[u8], replace: bool) -> Result<(), String> {
    publish_limit(path, raw, replace, LIMIT)
}

pub fn publish_limit(path: &Path, raw: &[u8], replace: bool, limit: u64) -> Result<(), String> {
    if raw.len() as u64 > limit {
        return Err("identity record exceeds limit".into());
    }
    if std::fs::symlink_metadata(path).is_ok() {
        read_limit(path, limit)?;
    }
    publish_bytes(path, raw, replace)
}

/// Old diagnostic contents never authorize or veto lifecycle work. Only the
/// fixed private leaf's metadata admits safe replacement/removal; do not read
/// its old payload or apply identity-record content limits here.
fn diagnostic_metadata(metadata: &std::fs::Metadata, uid: u32) -> bool {
    metadata.is_file()
        && metadata.uid() == uid
        && metadata.mode() & 0o077 == 0
        && metadata.nlink() == 1
}

fn diagnostic_leaf(path: &Path) -> Result<Option<File>, String> {
    let parent = path.parent().ok_or("diagnostic parent missing")?;
    if !control_directory(parent)? {
        return Ok(None);
    }
    validate_directory(parent, true)?;
    let file = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
    {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(error.to_string()),
    };
    let metadata = file.metadata().map_err(|error| error.to_string())?;
    // SAFETY: geteuid has no preconditions. Same UID is the local trust domain.
    if !diagnostic_metadata(&metadata, unsafe { libc::geteuid() }) {
        return Err("diagnostic file ownership, type, links or permissions invalid".into());
    }
    Ok(Some(file))
}

pub fn read_diagnostic_limit(path: &Path, limit: u64) -> Result<Option<Vec<u8>>, String> {
    let Some(_entry) = diagnostic_leaf(path)? else {
        return Ok(None);
    };
    read_limit(path, limit)
}

pub fn publish_diagnostic_limit(path: &Path, raw: &[u8], limit: u64) -> Result<(), String> {
    if raw.len() as u64 > limit {
        return Err("diagnostic record exceeds limit".into());
    }
    let _entry = diagnostic_leaf(path)?;
    publish_bytes(path, raw, true)
}

pub fn retire_diagnostic(path: &Path) -> Result<(), String> {
    if let Some(_entry) = diagnostic_leaf(path)? {
        std::fs::remove_file(path)
            .and_then(|_| File::open(path.parent().unwrap())?.sync_all())
            .map_err(|error| error.to_string())?;
    }
    Ok(())
}

/// Shared atomic publication only. Each caller must keep its own admission:
/// identity writers retain bounded old-content validation above; diagnostics
/// independently require trusted private leaf metadata without reading content.
fn publish_bytes(path: &Path, raw: &[u8], replace: bool) -> Result<(), String> {
    let parent = path.parent().ok_or("identity parent missing")?;
    let nonce = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|e| e.to_string())?
        .as_nanos();
    let temp = parent.join(format!(".identity-{}-{nonce}.tmp", std::process::id()));
    let result = (|| -> Result<(), String> {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
            .open(&temp)
            .map_err(|e| e.to_string())?;
        file.write_all(raw)
            .and_then(|_| file.sync_all())
            .map_err(|e| e.to_string())?;
        if replace {
            std::fs::rename(&temp, path).map_err(|e| e.to_string())?;
        } else {
            // Publish a complete file without replacing an existing identity.
            std::fs::hard_link(&temp, path).map_err(|e| e.to_string())?;
            std::fs::remove_file(&temp).map_err(|e| e.to_string())?;
        }
        File::open(parent)
            .and_then(|f| f.sync_all())
            .map_err(|e| e.to_string())
    })();
    let _ = std::fs::remove_file(temp);
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn control_reader_keeps_readable_owned_files_but_rejects_write_authority_and_links() {
        let root = crate::docker::fixtures::tmpdir("control-storage-reader");
        let path = root.join("record");
        assert!(read_control(&path, 16).unwrap().is_none());
        std::fs::write(&path, b"preserve").unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert_eq!(read_control(&path, 16).unwrap().unwrap(), b"preserve");
        assert!(
            read(&path).is_err(),
            "private identity contract is still 0600"
        );
        assert!(read_control(&path, 7).is_err());
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o666)).unwrap();
        assert!(read_control(&path, 16).is_err());
        std::fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink(root.join("missing"), &path).unwrap();
        assert!(read_control(&path, 16).is_err());
        std::fs::remove_file(&path).unwrap();
        let parent = root.join("linked");
        std::os::unix::fs::symlink(root.join("missing"), &parent).unwrap();
        assert!(read_control(&parent.join("record"), 16).is_err());
        assert!(control_directory(&parent).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn diagnostic_replacement_ignores_old_payload_but_identity_limits_remain_strict() {
        let root = crate::docker::fixtures::tmpdir("diagnostic-content-admission");
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
        directory(&root, true).unwrap();
        let path = root.join("source");
        for raw in [b"broken".to_vec(), vec![b'x'; 4097]] {
            std::fs::write(&path, &raw).unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
            if raw.len() > 4096 {
                assert!(read_diagnostic_limit(&path, 4096).is_err());
                assert!(publish_limit(&path, b"new identity", true, 4096).is_err());
                assert_eq!(std::fs::read(&path).unwrap(), raw);
            }
            publish_diagnostic_limit(&path, b"new diagnostic", 4096).unwrap();
            assert_eq!(
                read_diagnostic_limit(&path, 4096).unwrap().unwrap(),
                b"new diagnostic"
            );
            std::fs::write(&path, &raw).unwrap();
            retire_diagnostic(&path).unwrap();
            assert!(!path.exists());
        }
        // Metadata-only write admission does not read even a private old leaf
        // with no read permission; the caller already owns its parent/name.
        std::fs::write(&path, vec![b'x'; 4097]).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o0)).unwrap();
        publish_diagnostic_limit(&path, b"new diagnostic", 4096).unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), b"new diagnostic");
        let metadata = std::fs::metadata(&path).unwrap();
        let uid = unsafe { libc::geteuid() };
        assert!(diagnostic_metadata(&metadata, uid));
        assert!(
            !diagnostic_metadata(&metadata, uid.wrapping_add(1)),
            "foreign UID metadata is never admitted"
        );
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn diagnostic_reads_writes_and_retirement_reject_untrusted_paths_without_changes() {
        for kind in [
            "symlink",
            "hardlink",
            "fifo",
            "directory",
            "public-leaf",
            "public-parent",
            "linked-parent",
        ] {
            let root = crate::docker::fixtures::tmpdir(&format!("diagnostic-metadata-{kind}"));
            std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
            directory(&root, true).unwrap();
            let parent = root.join("native-journals");
            directory(&parent, true).unwrap();
            let source = parent.join("model.json");
            let target = root.join("original");
            std::fs::write(&target, b"preserve").unwrap();
            std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o600)).unwrap();
            match kind {
                "symlink" => std::os::unix::fs::symlink(&target, &source).unwrap(),
                "hardlink" => std::fs::hard_link(&target, &source).unwrap(),
                "directory" => std::fs::create_dir(&source).unwrap(),
                "fifo" => {
                    use std::os::unix::ffi::OsStrExt;
                    let name = std::ffi::CString::new(source.as_os_str().as_bytes()).unwrap();
                    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
                }
                "linked-parent" => {
                    std::fs::remove_dir(&parent).unwrap();
                    let outside = root.join("other");
                    directory(&outside, true).unwrap();
                    std::fs::write(outside.join("model.json"), b"preserve").unwrap();
                    std::os::unix::fs::symlink(&outside, &parent).unwrap();
                }
                _ => {
                    std::fs::write(&source, b"preserve").unwrap();
                    std::fs::set_permissions(
                        &source,
                        std::fs::Permissions::from_mode(if kind == "public-leaf" {
                            0o644
                        } else {
                            0o600
                        }),
                    )
                    .unwrap();
                    if kind == "public-parent" {
                        std::fs::set_permissions(&parent, std::fs::Permissions::from_mode(0o755))
                            .unwrap();
                    }
                }
            }
            assert!(read_diagnostic_limit(&source, 4096).is_err(), "{kind}");
            assert!(
                publish_diagnostic_limit(&source, b"replacement", 4096).is_err(),
                "{kind}"
            );
            assert!(retire_diagnostic(&source).is_err(), "{kind}");
            assert!(
                std::fs::symlink_metadata(&source).is_ok(),
                "{kind}: leaf removed"
            );
            assert_eq!(std::fs::read(&target).unwrap(), b"preserve");
            if kind == "linked-parent" {
                assert_eq!(std::fs::read(source).unwrap(), b"preserve");
            }
            std::fs::remove_dir_all(root).unwrap();
        }
    }
}
