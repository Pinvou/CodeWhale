//! Component control records are observations, never a reason to wait on a
//! special file or treat an unreadable/linked receipt as an absent gate.
use std::fs::OpenOptions;
use std::io::{self, Read};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::Path;

fn invalid() -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, "untrusted_component_file")
}

/// ENOENT may also mean a dangling ancestor. Only a genuinely absent path is
/// optional; another file appearing while we inspect absence is uncertainty.
fn verify_absent(path: &Path) -> io::Result<()> {
    for (index, part) in path.ancestors().enumerate() {
        match std::fs::symlink_metadata(part) {
            Ok(metadata) if index == 0 || !metadata.is_dir() => return Err(invalid()),
            Ok(_) => {}
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(error),
        }
    }
    Ok(())
}

/// One checked descriptor covers metadata and the bounded read. External user
/// configs/units may legitimately be 0644; they must still be same-user regular
/// files without group/other write permission. No mode or content is repaired.
pub(super) fn read_optional(path: &Path, limit: u64) -> io::Result<Option<Vec<u8>>> {
    let mut file = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
    {
        Ok(file) => file,
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            verify_absent(path)?;
            return Ok(None);
        }
        Err(error) => return Err(error),
    };
    let metadata = file.metadata()?;
    // SAFETY: geteuid only reads the effective identity.
    let uid = unsafe { libc::geteuid() };
    if !metadata.is_file()
        || metadata.uid() != uid
        || metadata.mode() & 0o022 != 0
        || metadata.len() > limit
    {
        return Err(invalid());
    }
    let mut bytes = Vec::new();
    Read::by_ref(&mut file)
        .take(limit + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() as u64 > limit {
        return Err(invalid());
    }
    Ok(Some(bytes))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{FileTypeExt, PermissionsExt, symlink};

    #[test]
    fn missing_is_optional_and_safe_external_read_modes_remain_supported() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("receipt");
        assert_eq!(read_optional(&path, 8).unwrap(), None);
        assert_eq!(read_optional(&path.join("also-missing"), 8).unwrap(), None);
        for mode in [0o600, 0o640, 0o644] {
            std::fs::write(&path, b"receipt").unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(mode)).unwrap();
            assert_eq!(read_optional(&path, 8).unwrap(), Some(b"receipt".to_vec()));
            assert_eq!(std::fs::metadata(&path).unwrap().mode() & 0o777, mode);
        }
    }

    #[test]
    fn linked_receipts_and_dangling_ancestors_are_unknown_not_absent() {
        let directory = tempfile::tempdir().unwrap();
        let target = directory.path().join("target");
        let link = directory.path().join("receipt");
        symlink(&target, &link).unwrap();
        assert!(read_optional(&link, 32).is_err());
        assert!(read_optional(&link.join("receipt"), 32).is_err());
        std::fs::write(&target, b"retained").unwrap();
        assert!(read_optional(&link, 32).is_err());
        assert_eq!(std::fs::read(&target).unwrap(), b"retained");
        assert!(
            std::fs::symlink_metadata(&link)
                .unwrap()
                .file_type()
                .is_symlink()
        );
    }

    #[test]
    fn special_writable_and_oversize_files_are_preserved_and_refused() {
        let directory = tempfile::tempdir().unwrap();
        assert!(read_optional(directory.path(), 32).is_err());
        let path = directory.path().join("receipt");
        let name = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
        // SAFETY: name is a live terminated path and mode is valid for mkfifo.
        assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
        assert!(read_optional(&path, 32).is_err());
        assert!(
            std::fs::symlink_metadata(&path)
                .unwrap()
                .file_type()
                .is_fifo()
        );
        std::fs::remove_file(&path).unwrap();
        std::fs::write(&path, b"retained").unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o620)).unwrap();
        assert!(read_optional(&path, 32).is_err());
        assert_eq!(std::fs::metadata(&path).unwrap().mode() & 0o777, 0o620);
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        assert!(read_optional(&path, 7).is_err());
        assert_eq!(read_optional(&path, 8).unwrap(), Some(b"retained".to_vec()));
        assert_eq!(std::fs::read(&path).unwrap(), b"retained");
    }
}
