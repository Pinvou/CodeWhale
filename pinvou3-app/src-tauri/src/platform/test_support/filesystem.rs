//! Shared filesystem-only test primitives. Bounded consumers can link these
//! without importing application paths, preferences, keyrings or ZIP helpers.

#[cfg(all(test, unix))]
pub(crate) fn plant_fifo(path: &std::path::Path) {
    let c_path = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
    // SAFETY: mkfifo on a fresh temp path; no other thread touches it.
    let rc = unsafe { libc::mkfifo(c_path.as_ptr(), 0o600) };
    assert_eq!(rc, 0, "fixture: mkfifo must succeed");
}

#[cfg(test)]
pub(crate) fn permission_bits(path: &std::path::Path) -> Option<u32> {
    crate::platform::filesystem::permission_bits(path)
        .ok()
        .flatten()
}

#[cfg(test)]
pub(crate) fn loosen_to_world_readable_for_test(path: &std::path::Path) -> bool {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o644)).is_ok()
    }
    #[cfg(not(unix))]
    {
        let _ = path;
        false
    }
}

/// Run only after a real permission-denied read, restoring the original mode
/// even when the callback panics. Unsupported/privileged hosts report `None`;
/// unrelated filesystem errors remain errors rather than becoming skipped tests.
#[cfg(test)]
pub(crate) fn with_read_permission_denied<T>(
    path: &std::path::Path,
    run: impl FnOnce() -> T,
) -> std::io::Result<Option<T>> {
    let metadata = std::fs::metadata(path)?;
    if !metadata.is_file() {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            "permission-denied fixture requires a regular file",
        ));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;

        struct Restore<'a> {
            path: &'a std::path::Path,
            original: Option<std::fs::Permissions>,
        }
        impl Restore<'_> {
            fn restore(&mut self) -> std::io::Result<()> {
                if let Some(original) = &self.original {
                    std::fs::set_permissions(self.path, original.clone())?;
                    self.original = None;
                }
                Ok(())
            }
        }
        impl Drop for Restore<'_> {
            fn drop(&mut self) {
                if let Err(error) = self.restore() {
                    // Avoid hiding a callback panic with a second panic. On the
                    // normal path restore errors are returned by `restore()?`.
                    eprintln!("permission-denied fixture permission restoration failed: {error}");
                }
            }
        }

        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o000))?;
        let mut restore = Restore {
            path,
            original: Some(metadata.permissions()),
        };
        let result = match std::fs::File::open(path) {
            Err(error) if error.kind() == std::io::ErrorKind::PermissionDenied => Ok(Some(run())),
            Err(error) => Err(error),
            Ok(_) => {
                eprintln!("PERMISSION-SKIP: privileged runner can read the denied fixture");
                Ok(None)
            }
        };
        restore.restore()?;
        result
    }
    #[cfg(not(unix))]
    {
        let _ = run;
        eprintln!("PERMISSION-SKIP: read permission denial is unavailable on this platform");
        Ok(None)
    }
}

#[cfg(test)]
pub(crate) fn assert_refused_without_hanging(
    label: &str,
    f: impl FnOnce() -> bool + Send + 'static,
) {
    let (tx, rx) = std::sync::mpsc::channel();
    let worker = std::thread::spawn(move || {
        let _ = tx.send(f());
    });
    let refused = rx
        .recv_timeout(std::time::Duration::from_secs(5))
        .unwrap_or_else(|_| panic!("{label} must refuse without hanging (5s bound)"));
    worker.join().unwrap();
    assert!(refused, "{label} must be refused");
}

#[cfg(test)]
mod tests {
    #[cfg(unix)]
    use crate::platform::paths::tests::plant_fifo;
    use crate::platform::test_support::{
        assert_refused_without_hanging, loosen_to_world_readable_for_test, permission_bits,
        with_read_permission_denied,
    };

    #[test]
    fn read_permission_fixture_runs_only_for_denial_and_restores_after_panic() {
        let file = tempfile::NamedTempFile::new().unwrap();
        std::fs::write(file.path(), b"original").unwrap();
        let before = permission_bits(file.path());
        let called = std::cell::Cell::new(false);
        let result = with_read_permission_denied(file.path(), || {
            called.set(true);
            assert_eq!(
                std::fs::File::open(file.path()).unwrap_err().kind(),
                std::io::ErrorKind::PermissionDenied
            );
            42
        })
        .unwrap();
        assert_eq!(called.get(), result.is_some());
        assert!(result.is_none() || result == Some(42));
        assert_eq!(permission_bits(file.path()), before);
        assert_eq!(std::fs::read(file.path()).unwrap(), b"original");

        let panicked = std::panic::catch_unwind(|| {
            with_read_permission_denied(file.path(), || panic!("fixture callback panic")).unwrap()
        });
        assert_eq!(panicked.is_err(), result.is_some());
        assert_eq!(permission_bits(file.path()), before);
        assert_eq!(std::fs::read(file.path()).unwrap(), b"original");
    }

    #[test]
    fn read_permission_fixture_preserves_unrelated_filesystem_errors() {
        let root = tempfile::tempdir().unwrap();
        assert_eq!(
            with_read_permission_denied(&root.path().join("missing"), || ())
                .unwrap_err()
                .kind(),
            std::io::ErrorKind::NotFound
        );
        assert_eq!(
            with_read_permission_denied(root.path(), || ())
                .unwrap_err()
                .kind(),
            std::io::ErrorKind::InvalidInput
        );
    }

    #[test]
    fn bounded_refusal_requires_a_real_refusal_result() {
        assert_refused_without_hanging("positive fixture", || true);
        assert!(
            std::panic::catch_unwind(|| {
                assert_refused_without_hanging("negative fixture", || false);
            })
            .is_err()
        );
    }

    #[test]
    fn permission_helpers_use_the_actual_platform_implementation() {
        let file = tempfile::NamedTempFile::new().unwrap();
        assert_eq!(
            permission_bits(file.path()),
            crate::platform::filesystem::permission_bits(file.path()).unwrap()
        );
        if loosen_to_world_readable_for_test(file.path()) {
            assert_eq!(permission_bits(file.path()), Some(0o644));
        } else {
            assert_eq!(permission_bits(file.path()), None);
        }
    }

    #[cfg(unix)]
    #[test]
    fn fifo_fixture_is_a_real_nonregular_file() {
        use std::os::unix::fs::FileTypeExt as _;
        let root = tempfile::tempdir().unwrap();
        let fifo = root.path().join("fixture.fifo");
        plant_fifo(&fifo);
        assert!(
            std::fs::symlink_metadata(&fifo)
                .unwrap()
                .file_type()
                .is_fifo()
        );
    }
}
