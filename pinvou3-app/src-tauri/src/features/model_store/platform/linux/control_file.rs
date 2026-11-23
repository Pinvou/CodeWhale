//! Read local store control evidence without following links or waiting on a
//! special file. An unknown path is never an empty inventory or an absent gate.
use std::fs::{File, Metadata, OpenOptions};
use std::io::{self, Read};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::Path;

fn invalid() -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, "untrusted_store_control_file")
}

fn parents(path: &Path) -> io::Result<()> {
    for parent in path.ancestors().skip(1) {
        match std::fs::symlink_metadata(parent) {
            Ok(metadata) if !metadata.is_dir() => return Err(invalid()),
            Ok(_) => {}
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(error),
        }
    }
    Ok(())
}

fn absent(path: &Path) -> io::Result<()> {
    parents(path)?;
    match std::fs::symlink_metadata(path) {
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
        Ok(_) => Err(invalid()),
    }
}

/// SQLite keeps its own read-only/WAL descriptor. Check its path before deciding
/// that the database was never created; a link or invalid ancestor is unknown.
pub(super) fn metadata_optional(path: &Path) -> io::Result<Option<Metadata>> {
    parents(path)?;
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() => Err(invalid()),
        Ok(metadata) => Ok(Some(metadata)),
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            absent(path)?;
            Ok(None)
        }
        Err(error) => Err(error),
    }
}

pub(super) fn verify_file(file: &File, limit: Option<u64>) -> io::Result<()> {
    let metadata = file.metadata()?;
    // SAFETY: geteuid only observes the effective identity.
    let uid = unsafe { libc::geteuid() };
    if !metadata.is_file()
        || metadata.uid() != uid
        || metadata.mode() & 0o022 != 0
        || limit.is_some_and(|limit| metadata.len() > limit)
    {
        return Err(invalid());
    }
    Ok(())
}

/// A limit of None is reserved for streaming original task journals: their
/// immutable log arrays may legitimately exceed the small receipt read budget.
pub(super) fn open_optional(path: &Path, limit: Option<u64>) -> io::Result<Option<File>> {
    parents(path)?;
    let file = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
    {
        Ok(file) => file,
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            absent(path)?;
            return Ok(None);
        }
        Err(error) => return Err(error),
    };
    verify_file(&file, limit)?;
    Ok(Some(file))
}

pub(super) fn read_optional(path: &Path, limit: u64) -> io::Result<Option<Vec<u8>>> {
    let Some(mut file) = open_optional(path, Some(limit))? else {
        return Ok(None);
    };
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
    fn absence_requires_existing_ancestors_to_be_plain_directories() {
        let root = tempfile::tempdir().unwrap();
        let missing = root.path().join("missing/receipt");
        assert!(metadata_optional(&missing).unwrap().is_none());
        assert!(read_optional(&missing, 16).unwrap().is_none());
        for present in [false, true] {
            let target = root.path().join("target");
            if present {
                std::fs::create_dir(&target).unwrap();
            }
            let linked = root.path().join("linked");
            symlink(&target, &linked).unwrap();
            assert!(metadata_optional(&linked).is_err());
            assert!(metadata_optional(&linked.join("receipt")).is_err());
            assert!(read_optional(&linked.join("receipt"), 16).is_err());
            std::fs::remove_file(linked).unwrap();
        }
        let plain = root.path().join("file");
        std::fs::write(&plain, b"retained").unwrap();
        assert!(metadata_optional(&plain.join("receipt")).is_err());
        assert!(read_optional(&plain.join("receipt"), 16).is_err());
        assert_eq!(std::fs::read(&plain).unwrap(), b"retained");
    }

    #[test]
    fn reads_the_checked_regular_file_with_a_bound_without_repairing_it() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("receipt");
        for mode in [0o600, 0o640, 0o644] {
            std::fs::write(&path, b"retained").unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(mode)).unwrap();
            let inode = path.metadata().unwrap().ino();
            assert_eq!(read_optional(&path, 8).unwrap(), Some(b"retained".to_vec()));
            assert!(read_optional(&path, 7).is_err());
            assert_eq!(path.metadata().unwrap().ino(), inode);
            assert_eq!(path.metadata().unwrap().mode() & 0o777, mode);
        }
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o660)).unwrap();
        assert!(read_optional(&path, 8).is_err());
        assert_eq!(path.metadata().unwrap().mode() & 0o777, 0o660);
    }

    #[test]
    fn fifo_directory_and_leaf_links_are_unknown_and_remain_untouched() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("receipt");
        let name = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
        // SAFETY: name is a terminated path owned by this test.
        assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
        assert!(read_optional(&path, 16).is_err());
        assert!(path.symlink_metadata().unwrap().file_type().is_fifo());
        std::fs::remove_file(&path).unwrap();
        symlink("missing", &path).unwrap();
        assert!(read_optional(&path, 16).is_err());
        assert!(path.symlink_metadata().unwrap().file_type().is_symlink());
        assert!(read_optional(root.path(), 16).is_err());
    }
}
