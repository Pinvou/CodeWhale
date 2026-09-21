//! 跨特性共享的测试 helper（全部条目 `#[cfg(test)]`，不进发布产物）。
//!
//! 模块声明位于 `platform/mod.rs`（`#[cfg(test)] pub(crate) mod test_support;`），
//! 不进发布产物。

/// Test helper: drop the current user's read permission on `dir` (Unix chmod
/// 000) to simulate an unscannable installed directory (e.g. the marketplace
/// scope's DenyAll default enumeration degradation). Returns `None` when the
/// simulation cannot take effect — non-Unix has no POSIX mode bits, and root
/// is not constrained by 000 — so callers skip the assertions that depend on
/// EACCES semantics (covered on non-Unix-root environments, e.g. the ubuntu CI
/// runner). The returned guard restores 0755 on drop, including panic
/// unwinding, so the temp home stays cleanable even when an assertion fires
/// while the directory is unreadable.
pub(crate) fn make_dir_unreadable_for_test(dir: &std::path::Path) -> Option<UnreadableDirForTest> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        if std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o000)).is_err() {
            return None;
        }
        // Root is not constrained by 000: without an observable EACCES the
        // simulation is ineffective, so restore and report failure.
        if std::fs::read_dir(dir).is_ok() {
            let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o755));
            return None;
        }
        Some(UnreadableDirForTest {
            dir: dir.to_path_buf(),
        })
    }
    #[cfg(not(unix))]
    {
        let _ = dir;
        None
    }
}

/// Restore pairing for [`make_dir_unreadable_for_test`]: restores 0755 (test
/// directories are created by `create_dir_all` with default permissions) on
/// drop, in the RAII guard house pattern. Only ever constructed on Unix.
pub(crate) struct UnreadableDirForTest {
    dir: std::path::PathBuf,
}

impl Drop for UnreadableDirForTest {
    fn drop(&mut self) {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let _ = std::fs::set_permissions(&self.dir, std::fs::Permissions::from_mode(0o755));
        }
        #[cfg(not(unix))]
        {
            let _ = &self.dir;
        }
    }
}
