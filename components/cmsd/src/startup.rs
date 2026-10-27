//! Lifetime instance exclusion and conservative, bounded socket occupancy probe.
use std::fs::{File, OpenOptions};
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::Path;
use std::time::Duration;

/// A local-only build must not discard authority owned by a newer component.
/// Inspect before creating state, units or listeners; never remove these records.
pub fn check_supported_state(state: &Path) -> io::Result<()> {
    for name in [
        "identity-v2",
        "remote-v2",
        "control-v2",
        "control-v2.initialized.json",
    ] {
        match std::fs::symlink_metadata(state.join(name)) {
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(error),
            Ok(_) => return Err(io::Error::new(io::ErrorKind::Unsupported,
                format!("unsupported authority state {name}; preserve it and use a compatible component"))),
        }
    }
    Ok(())
}

pub fn lock(path: &Path) -> io::Result<File> {
    let file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)?;
    let metadata = file.metadata()?;
    // SAFETY: geteuid has no preconditions. Same UID is the local trust domain.
    if !metadata.is_file() || metadata.uid() != unsafe { libc::geteuid() } || metadata.nlink() != 1
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "runtime lock must be a single-link regular file owned by this uid",
        ));
    }
    file.try_lock().map_err(|error| {
        io::Error::other(format!("runtime lock is held or unavailable: {error}"))
    })?;
    // Upgrade the old empty 0644 lock after acquiring it, without truncating,
    // replacing its inode, or touching a live legacy instance's lock.
    file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
    Ok(file)
}

pub fn socket_claim(socket: &Path) -> io::Result<File> {
    let mut name = socket
        .file_name()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "socket must have a filename"))?
        .to_os_string();
    name.push(".lock");
    lock(&socket.with_file_name(name))
}

fn checked_socket(path: &Path) -> io::Result<Option<File>> {
    // Pin the filesystem inode, which is distinct from the listener's socket
    // descriptor. A replacement must not reuse an old observation's identity.
    let file = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
    {
        Ok(file) => file,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(error),
    };
    let metadata = file.metadata()?;
    // Never unlink a regular file, link, foreign endpoint or aliased socket.
    // SAFETY: geteuid has no preconditions.
    if !metadata.file_type().is_socket()
        || metadata.uid() != unsafe { libc::geteuid() }
        || metadata.nlink() != 1
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "socket is not a single owned Unix socket; original path retained",
        ));
    }
    Ok(Some(file))
}

/// Caller holds the socket claim for the daemon lifetime. Only two explicit
/// ConnectionRefused observations of the same owned inode permit stale unlink.
/// Busy, deadline, permission and other failures retain the existing endpoint.
pub fn prepare_socket(path: &Path) -> io::Result<()> {
    let Some(before_entry) = checked_socket(path)? else {
        return Ok(());
    };
    let before = before_entry.metadata()?;
    for attempt in 0..2 {
        match crate::http::connect_unix(path, Duration::from_millis(200)) {
            Ok(_) => {
                return Err(io::Error::new(
                    io::ErrorKind::AddrInUse,
                    "socket has an active listener; original endpoint retained",
                ))
            }
            Err(error) if error.kind() == io::ErrorKind::ConnectionRefused => {}
            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                if checked_socket(path)?.is_none() {
                    return Ok(());
                }
                return Err(io::Error::new(
                    io::ErrorKind::AddrInUse,
                    "socket path changed while probing; retained",
                ));
            }
            Err(error) => {
                return Err(io::Error::new(
                    error.kind(),
                    format!("cannot prove socket is stale; original endpoint retained: {error}"),
                ))
            }
        }
        if attempt == 0 {
            std::thread::sleep(Duration::from_millis(20));
        }
    }
    let Some(after_entry) = checked_socket(path)? else {
        return Ok(());
    };
    let after = after_entry.metadata()?;
    if before.ino() != after.ino() || before.dev() != after.dev() {
        return Err(io::Error::new(
            io::ErrorKind::AddrInUse,
            "socket inode changed while probing; retained",
        ));
    }
    // Keep both entry descriptors alive through the unlink.
    std::fs::remove_file(path)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn lock_keeps_its_inode_and_rejects_aliases_without_touching_targets() {
        let root = crate::docker::fixtures::tmpdir("startup-lock");
        let path = root.join("lock");
        std::fs::write(&path, "legacy-empty-or-diagnostic").unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        let inode = path.metadata().unwrap().ino();
        let guard = lock(&path).unwrap();
        assert_eq!(path.metadata().unwrap().mode() & 0o777, 0o600);
        assert_eq!(inode, path.metadata().unwrap().ino());
        assert!(lock(&path).is_err());
        drop(guard);
        assert!(lock(&path).is_ok());
        let link = root.join("link");
        std::os::unix::fs::symlink(&path, &link).unwrap();
        assert!(lock(&link).is_err());
        let hard = root.join("hard");
        std::fs::hard_link(&path, &hard).unwrap();
        assert!(lock(&hard).is_err());
        assert_eq!(
            std::fs::read_to_string(&path).unwrap(),
            "legacy-empty-or-diagnostic"
        );
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn active_and_non_socket_paths_are_never_removed_but_owned_stale_socket_recovers() {
        let root = crate::docker::fixtures::tmpdir("startup-socket");
        let path = root.join("api.sock");
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        let inode = path.metadata().unwrap().ino();
        let _claim = socket_claim(&path).unwrap();
        assert!(socket_claim(&path).is_err());
        assert!(prepare_socket(&path).is_err());
        assert_eq!(inode, path.metadata().unwrap().ino());
        drop(listener);
        prepare_socket(&path).unwrap();
        assert!(!path.exists());
        std::fs::write(&path, "preserve").unwrap();
        assert!(prepare_socket(&path).is_err());
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "preserve");
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[cfg(test)]
mod supported_state_tests {
    use super::*;

    #[test]
    fn local_state_inspection_does_not_create_or_modify_files() {
        let root = crate::docker::fixtures::tmpdir("local-state-compatibility");
        let missing = root.join("not-created");
        check_supported_state(&missing).unwrap();
        assert!(!missing.exists());
        for name in [
            "identity-v2",
            "remote-v2",
            "control-v2",
            "control-v2.initialized.json",
        ] {
            let marker = root.join(name);
            std::fs::write(&marker, b"retain-authority").unwrap();
            assert_eq!(
                check_supported_state(&root).unwrap_err().kind(),
                io::ErrorKind::Unsupported
            );
            assert_eq!(std::fs::read(&marker).unwrap(), b"retain-authority");
            std::fs::remove_file(marker).unwrap();
        }
        std::os::unix::fs::symlink(root.join("absent"), root.join("remote-v2")).unwrap();
        assert_eq!(
            check_supported_state(&root).unwrap_err().kind(),
            io::ErrorKind::Unsupported
        );
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[cfg(test)]
mod socket_inode_tests {
    use super::*;

    #[test]
    fn stale_inode_observation_cannot_be_reused_by_a_replacement_listener() {
        let root = crate::docker::fixtures::tmpdir("startup-socket-inode");
        let path = root.join("api.sock");
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        drop(listener);
        let entry = checked_socket(&path).unwrap().unwrap();
        let before = entry.metadata().unwrap();
        std::fs::remove_file(&path).unwrap();
        let replacement = std::os::unix::net::UnixListener::bind(&path).unwrap();
        let after = path.metadata().unwrap();
        assert_ne!((before.dev(), before.ino()), (after.dev(), after.ino()));
        assert!(prepare_socket(&path).is_err());
        drop(entry);
        assert_eq!(path.metadata().unwrap().ino(), after.ino());
        drop(replacement);
        std::fs::remove_dir_all(root).unwrap();
    }
}
