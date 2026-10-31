//! Immutable, process-owned parser inputs for Linux. No mutable pathname is
//! granted read authority; directory fds only establish the session scope.
use super::{PrivateFileDirectory, PublishedPrivateFile, metadata_file_identity};
use std::fs::File;
use std::io::{self, Seek as _, Write as _};
use std::os::fd::{AsRawFd as _, FromRawFd as _};
use std::os::unix::fs::PermissionsExt as _;
use std::path::{Path, PathBuf};

pub(crate) fn publish_private_read_path(
    directory: PrivateFileDirectory,
    name: &std::ffi::OsStr,
    content: &[u8],
) -> io::Result<PublishedPrivateFile> {
    super::validate_private_file_name(name)?;
    let directory_path = std::fs::read_link(format!(
        "/proc/self/fd/{}",
        directory.directory_handle.as_raw_fd()
    ))?;
    let path = directory_path.join(name);
    // A replaced/deleted directory cannot establish a new publication scope.
    check_directory(&directory, &directory_path)?;
    let parent_path = directory_path
        .parent()
        .ok_or_else(|| io::Error::other("private parent missing"))?;
    let cleanup_parent = super::open_existing_private_file_directory(parent_path)?
        .ok_or_else(|| io::Error::other("private parent missing"))?;
    let child = cleanup_parent.open_child_directory(
        directory_path
            .file_name()
            .ok_or_else(|| io::Error::other("private directory missing"))?,
    )?;
    if metadata_file_identity(&child.metadata()?) != metadata_file_identity(&directory.metadata()?)
    {
        return Err(io::Error::other("private parent changed"));
    }
    // A constant name avoids putting user filenames into /proc diagnostics.
    // SAFETY: the name is a live NUL-terminated string; successful return owns
    // a fresh fd, adopted exactly once below. CLOEXEC prevents fd inheritance.
    let fd = unsafe {
        super::platform_libc::memfd_create(
            c"pinvou-attachment".as_ptr(),
            super::platform_libc::MFD_CLOEXEC | super::platform_libc::MFD_ALLOW_SEALING,
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: fd is a fresh, nonnegative owning descriptor from memfd_create.
    let mut file = unsafe { File::from_raw_fd(fd) };
    file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
    file.write_all(content)?;
    file.rewind()?;
    let seals = super::platform_libc::F_SEAL_WRITE
        | super::platform_libc::F_SEAL_GROW
        | super::platform_libc::F_SEAL_SHRINK
        | super::platform_libc::F_SEAL_SEAL;
    // SAFETY: file owns this fd; F_ADD_SEALS takes an integer bitmask. All
    // writes and size changes are irrevocably forbidden before publication.
    if unsafe { super::platform_libc::fcntl(fd, super::platform_libc::F_ADD_SEALS, seals) } < 0 {
        return Err(io::Error::last_os_error());
    }
    // Use the owner's PID, not /proc/self: external parsers run in children.
    let read_path = PathBuf::from(format!("/proc/{}/fd/{fd}", std::process::id()));
    if metadata_file_identity(&std::fs::metadata(&read_path)?)
        != metadata_file_identity(&file.metadata()?)
    {
        return Err(io::Error::other("private parser source unavailable"));
    }
    Ok(PublishedPrivateFile {
        file: parking_lot::Mutex::new(Some(file)),
        _directory: Some(directory),
        path,
        read_path,
        cleanup_parent,
        cleanup_requested: std::sync::atomic::AtomicBool::new(false),
    })
}

fn check_directory(directory: &PrivateFileDirectory, path: &Path) -> io::Result<()> {
    let metadata = std::fs::symlink_metadata(path)?;
    if !metadata.is_dir()
        || std::fs::canonicalize(path)? != path
        || metadata_file_identity(&metadata)
            != metadata_file_identity(&directory.directory_handle.metadata()?)
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "private publication directory changed",
        ));
    }
    Ok(())
}

pub(super) fn validate_scope(publication: &PublishedPrivateFile, root: &Path) -> io::Result<()> {
    let state = publication.file.lock();
    let file = state
        .as_ref()
        .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "private source closed"))?;
    if !publication.path.starts_with(root)
        || metadata_file_identity(&std::fs::metadata(&publication.read_path)?)
            != metadata_file_identity(&file.metadata()?)
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "private publication scope mismatch",
        ));
    }
    check_directory(
        publication
            ._directory
            .as_ref()
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "private anchor missing"))?,
        publication
            .path
            .parent()
            .ok_or_else(|| io::Error::other("private directory missing"))?,
    )
}

pub(super) fn cleanup_directory(publication: &mut PublishedPrivateFile) {
    let Some(name) = publication.path.parent().and_then(Path::file_name) else {
        eprintln!("[private-file] cleanup directory unavailable");
        return;
    };
    let Some(directory) = publication._directory.as_ref() else {
        eprintln!("[private-file] cleanup anchor unavailable");
        return;
    };
    let current = match publication.cleanup_parent.open_child_directory(name) {
        Ok(directory) => directory,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return,
        Err(_) => {
            eprintln!("[private-file] cleanup anchor unavailable; foreign entries retained");
            return;
        }
    };
    let (Ok(current_metadata), Ok(owned_metadata)) = (current.metadata(), directory.metadata())
    else {
        eprintln!("[private-file] cleanup identity unavailable; foreign entries retained");
        return;
    };
    if metadata_file_identity(&current_metadata) != metadata_file_identity(&owned_metadata) {
        eprintln!("[private-file] cleanup identity changed; foreign entries retained");
        return;
    }
    // No recursive pathname cleanup and no leaf deletion on Linux. Keep any
    // foreign entries for the normal reaper/operator instead of deleting them.
    if publication.cleanup().is_err() {
        eprintln!("[private-file] source cleanup failed; foreign entries retained");
        return;
    }
    publication._directory.take();
    if publication
        .cleanup_parent
        .remove_empty_child_directory(name)
        .is_err()
    {
        eprintln!("[private-file] empty-directory cleanup failed; foreign entries retained");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn assert_source_released(read_path: &Path, original: &std::fs::Metadata) {
        let original_identity = metadata_file_identity(original).expect("Linux file identity");
        // Descriptor numbers are process-wide and other tests can reuse this
        // slot immediately. A surviving proc path is only a leak when it still
        // refers to the original sealed source; unexpected stat errors must fail.
        let current_identity = match std::fs::metadata(read_path) {
            Ok(metadata) => metadata_file_identity(&metadata),
            Err(error) if error.kind() == io::ErrorKind::NotFound => None,
            Err(error) => panic!("could not verify source release: {error}"),
        };
        assert_ne!(
            current_identity,
            Some(original_identity),
            "the original sealed source is still open at its descriptor"
        );
    }

    #[test]
    fn sealed_source_survives_path_replacements_and_child_readers() {
        let root = tempfile::tempdir().unwrap();
        let folder = root.path().join("upload");
        let owner = publish_private_read_path(
            super::super::open_private_file_directory(&folder).unwrap(),
            std::ffi::OsStr::new("report.txt"),
            b"verified bytes",
        )
        .unwrap();
        owner.validate_scope(root.path()).unwrap();
        assert!(
            !owner.path().exists(),
            "no mutable leaf grants read authority"
        );
        std::fs::write(owner.path(), b"foreign replacement").unwrap();
        assert_eq!(owner.read_bounded(64).unwrap(), b"verified bytes");
        assert_eq!(std::fs::read(owner.read_path()).unwrap(), b"verified bytes");
        assert!(std::fs::write(owner.read_path(), b"tampered").is_err());
        let output = std::process::Command::new("cat")
            .arg(owner.read_path())
            .output()
            .unwrap();
        assert!(output.status.success());
        assert_eq!(output.stdout, b"verified bytes");
        assert!(owner.read_bounded(2).is_err());
        let read_path = owner.read_path().to_owned();
        let original = std::fs::metadata(&read_path).unwrap();
        owner.request_cleanup();
        drop(owner);
        assert_source_released(&read_path, &original);
        assert_eq!(
            std::fs::read(folder.join("report.txt")).unwrap(),
            b"foreign replacement"
        );
    }

    #[test]
    fn replaced_scope_is_rejected_and_last_owner_releases_the_source() {
        let root = tempfile::tempdir().unwrap();
        let folder = root.path().join("upload");
        let owner = publish_private_read_path(
            super::super::open_private_file_directory(&folder).unwrap(),
            std::ffi::OsStr::new("report.txt"),
            b"private",
        )
        .unwrap();
        let read_path = owner.read_path().to_owned();
        let original = std::fs::metadata(&read_path).unwrap();
        std::fs::rename(&folder, root.path().join("retired")).unwrap();
        std::fs::create_dir(&folder).unwrap();
        assert!(owner.validate_scope(root.path()).is_err());
        assert_eq!(std::fs::read(&read_path).unwrap(), b"private");
        drop(owner);
        assert_source_released(&read_path, &original);
    }
}
