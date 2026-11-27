//! One user manager is shared by all desktop profiles and the platform bootstrap.
//! Never unlink a lock file: a replacement inode would allow concurrent writers.
use std::ffi::CString;
use std::fs::{File, OpenOptions};
use std::os::fd::{AsRawFd, FromRawFd};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

pub(super) struct Guard {
    // Fields drop in declaration order: release the profile before the shared lock.
    _profile: File,
    _shared: File,
}

fn uid() -> u32 {
    // SAFETY: geteuid has no preconditions.
    unsafe { libc::geteuid() }
}

fn runtime_directory() -> PathBuf {
    // XDG_RUNTIME_DIR and PINVOU3_HOME must not partition this coordination domain.
    Path::new("/run/user").join(uid().to_string())
}

fn trusted(file: &File, directory: bool) -> Result<(), String> {
    let metadata = file.metadata().map_err(|e| e.to_string())?;
    let mode = if directory { 0o700 } else { 0o600 };
    if metadata.uid() != uid()
        || metadata.mode() & 0o777 != mode
        || (directory && !metadata.is_dir())
        || (!directory && (!metadata.is_file() || metadata.nlink() != 1))
    {
        return Err("untrusted_component_lock: inspect the user runtime directory".into());
    }
    Ok(())
}

fn open_at(parent: &File, name: &str, flags: i32, mode: u32) -> Result<File, String> {
    let name = CString::new(name).map_err(|_| "invalid_component_lock_path")?;
    // SAFETY: parent and name remain valid during openat; a successful fd is owned.
    let fd = unsafe {
        libc::openat(
            parent.as_raw_fd(),
            name.as_ptr(),
            flags | libc::O_NOFOLLOW | libc::O_CLOEXEC,
            mode as libc::mode_t,
        )
    };
    if fd < 0 {
        return Err(format!(
            "component_lock_unavailable: {}",
            std::io::Error::last_os_error()
        ));
    }
    // SAFETY: fd is a fresh owned descriptor returned by openat.
    Ok(unsafe { File::from_raw_fd(fd) })
}

pub(super) fn acquire(profile: &Path) -> Result<Guard, String> {
    acquire_at(&runtime_directory(), profile)
}

fn acquire_at(runtime: &Path, profile: &Path) -> Result<Guard, String> {
    // Missing /run/user/<uid> is an explicit error, never a profile-local fallback.
    let parent = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(runtime)
        .map_err(|e| format!("component_lock_unavailable: {e}"))?;
    trusted(&parent, true)?;
    let directory_name = c"megacube-component-locks";
    // SAFETY: the descriptor and fixed directory name are valid for mkdirat.
    if unsafe { libc::mkdirat(parent.as_raw_fd(), directory_name.as_ptr(), 0o700) } < 0
        && std::io::Error::last_os_error().kind() != std::io::ErrorKind::AlreadyExists
    {
        return Err(format!(
            "component_lock_unavailable: {}",
            std::io::Error::last_os_error()
        ));
    }
    let directory = open_at(
        &parent,
        "megacube-component-locks",
        libc::O_RDONLY | libc::O_DIRECTORY,
        0,
    )?;
    trusted(&directory, true)?;
    let shared = open_at(
        &directory,
        "cmsd-user-manager.lock",
        libc::O_RDWR | libc::O_CREAT,
        0o600,
    )?;
    trusted(&shared, false)?;
    try_lock(&shared)?;

    super::private_directory(profile)?;
    let local = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(profile)
        .map_err(|e| e.to_string())?;
    trusted(&local, true)?;
    let profile = open_at(
        &local,
        "component.lock",
        libc::O_RDWR | libc::O_CREAT,
        0o600,
    )?;
    trusted(&profile, false)?;
    try_lock(&profile)?;
    Ok(Guard {
        _profile: profile,
        _shared: shared,
    })
}

fn try_lock(file: &File) -> Result<(), String> {
    file.try_lock().map_err(|error| match error {
        std::fs::TryLockError::WouldBlock => {
            "component_busy: another component operation is in progress".into()
        }
        other => format!("component_lock_unavailable: {other}"),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{PermissionsExt, symlink};
    use std::process::Command;

    #[test]
    fn different_profiles_share_one_flock_with_python_and_do_not_inherit_it() {
        let runtime = tempfile::tempdir().unwrap();
        std::fs::set_permissions(runtime.path(), std::fs::Permissions::from_mode(0o700)).unwrap();
        let lock_path = runtime
            .path()
            .join("megacube-component-locks/cmsd-user-manager.lock");
        let guard = acquire_at(runtime.path(), &runtime.path().join("profile-a")).unwrap();
        assert!(
            acquire_at(runtime.path(), &runtime.path().join("profile-b"))
                .err()
                .unwrap()
                .starts_with("component_busy")
        );
        let probe = |busy| {
            Command::new("python3").args(["-c", "import fcntl,sys\nf=open(sys.argv[1],'r+')\ntry: fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)\nexcept BlockingIOError: sys.exit(17)\n", lock_path.to_str().unwrap()]).status().unwrap().code() == Some(if busy {17} else {0})
        };
        assert!(probe(true));
        assert_ne!(
            // SAFETY: F_GETFD only reads descriptor flags of an fd owned by this fixture.
            unsafe { libc::fcntl(guard._shared.as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
            0
        );
        drop(guard);
        assert!(probe(false));
        let _second = acquire_at(runtime.path(), &runtime.path().join("profile-b")).unwrap();
        assert!(probe(true));
    }

    #[test]
    fn missing_or_untrusted_runtime_and_linked_lock_are_not_fallbacks() {
        let runtime = tempfile::tempdir().unwrap();
        let profile = runtime.path().join("profile");
        assert!(acquire_at(&runtime.path().join("missing"), &profile).is_err());
        assert!(!profile.exists());
        std::fs::set_permissions(runtime.path(), std::fs::Permissions::from_mode(0o755)).unwrap();
        assert!(acquire_at(runtime.path(), &profile).is_err());
        std::fs::set_permissions(runtime.path(), std::fs::Permissions::from_mode(0o700)).unwrap();
        let directory = runtime.path().join("megacube-component-locks");
        super::super::private_directory(&directory).unwrap();
        let target = runtime.path().join("other");
        std::fs::write(&target, b"retain").unwrap();
        symlink(&target, directory.join("cmsd-user-manager.lock")).unwrap();
        assert!(acquire_at(runtime.path(), &profile).is_err());
        assert_eq!(std::fs::read(target).unwrap(), b"retain");
        assert_eq!(
            runtime_directory(),
            Path::new("/run/user").join(uid().to_string())
        );
    }
}
