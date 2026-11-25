//! Cross-process exclusion for admission checks and model startup.
//!
//! Keep the file descriptor alive until the startup job has completed, including
//! cancellation and unwinding. Never remove the lock file: unlinking a locked
//! inode would let another process acquire a different lock under the same name.

use std::fs::File;
use std::path::Path;

pub struct StartupGuard {
    _file: File,
}

impl StartupGuard {
    pub fn acquire() -> Result<Self, String> {
        Self::at(&super::config::data_dir().join("model-startup.lock"))
    }

    pub fn for_card(card_id: &str) -> Result<Self, String> {
        let name = format!("card-{}.lock", super::script_runner::sha256_hex(card_id));
        Self::at(&super::config::data_dir().join(name))
            .map_err(|e| format!("该模型的操作保护尚未就绪，请等待当前操作完成后重试：{e}"))
    }

    fn at(path: &Path) -> Result<Self, String> {
        let file = crate::platform::filesystem::open_private_lock_file(path)
            .map_err(|e| format!("无法建立模型启动保护，请检查应用数据目录权限：{e}"))?;
        if !file.metadata().map_err(|e| e.to_string())?.is_file() {
            return Err("模型启动保护文件异常，请联系管理员检查应用数据目录。".into());
        }
        file.try_lock().map_err(|e| match e {
            std::fs::TryLockError::WouldBlock => {
                "另一个模型正在准备或启动，请等待其完成后再启用，届时会重新检查内存。".into()
            }
            std::fs::TryLockError::Error(error) => format!("无法锁定模型启动保护：{error}"),
        })?;
        Ok(Self { _file: file })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn independent_handles_cannot_check_and_start_concurrently() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("startup.lock");
        let first = StartupGuard::at(&path).unwrap();
        assert!(StartupGuard::at(&path).is_err());
        drop(first);
        assert!(StartupGuard::at(&path).is_ok());
        assert!(path.exists());
    }

    #[test]
    fn startup_slot_is_released_on_error_or_panic() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("startup.lock");
        let _ = std::panic::catch_unwind(|| {
            let _guard = StartupGuard::at(&path).unwrap();
            panic!("test unwind");
        });
        assert!(StartupGuard::at(&path).is_ok());
    }

    #[test]
    fn symlink_lock_is_rejected() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("target");
        std::fs::write(&target, b"untouched").unwrap();
        let link = dir.path().join("startup.lock");
        std::os::unix::fs::symlink(&target, &link).unwrap();
        assert!(StartupGuard::at(&link).is_err());
        assert_eq!(std::fs::read(target).unwrap(), b"untouched");
    }
}
