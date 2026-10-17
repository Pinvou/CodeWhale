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
    };

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
