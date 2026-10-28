//! A download script shares its owner's open-file-description lease. Closing
//! the app must not make a still-writing child look like an interrupted plan.
use std::fs::File;

pub const BUSY: &str = "该模型仍有下载脚本运行，已保留完整存储预留；请等待脚本退出后重试。";

pub(super) fn open(card_id: &str) -> Result<File, String> {
    let safe = super::super::config::safe_path_component(card_id)?;
    let directory = super::super::config::data_dir().join("download-locks");
    std::fs::create_dir_all(&directory).map_err(|e| e.to_string())?;
    // Existing ordinary downloads already use this exact inode/path.
    crate::platform::filesystem::open_private_lock_file(&directory.join(format!("{safe}.lock")))
        .map_err(|e| e.to_string())
}

pub fn try_acquire(card_id: &str) -> Result<Option<File>, String> {
    let file = open(card_id)?;
    match file.try_lock() {
        Ok(()) => {
            // Older scripts did not inherit a lease. A strict receipt/proc
            // check conservatively keeps known live or uncertain old work.
            if super::super::orphans::is_recovery_blocked(card_id)? {
                Ok(None)
            } else {
                Ok(Some(file))
            }
        }
        Err(std::fs::TryLockError::WouldBlock) => Ok(None),
        Err(std::fs::TryLockError::Error(error)) => Err(error.to_string()),
    }
}

pub fn acquire(
    card_id: &str,
    cancelled: impl Fn() -> bool,
    mut on_wait: impl FnMut(),
) -> Result<DownloadLease, String> {
    let mut reported = false;
    loop {
        if cancelled() {
            return Err("已取消等待下载配额登记".into());
        }
        if let Some(file) = try_acquire(card_id)? {
            return Ok(DownloadLease {
                card_id: card_id.into(),
                file: Some(file),
                stopped: true,
            });
        }
        if !reported {
            on_wait();
            reported = true;
        }
        std::thread::sleep(std::time::Duration::from_millis(50));
    }
}

/// Parent ownership and the barrier between attempts/finalization. The runner
/// borrows the held OFD, and all its parent clones are gone when it returns.
pub struct DownloadLease {
    card_id: String,
    file: Option<File>,
    stopped: bool,
}

impl DownloadLease {
    pub fn script_file(&mut self) -> Result<&File, String> {
        self.stopped = false;
        self.file.as_ref().ok_or_else(|| BUSY.to_string())
    }

    /// Never call flock(LOCK_UN): it unlocks the OFD shared with descendants.
    /// Retain the independently acquired new OFD through retry/DB finalization.
    pub fn confirm_stopped(&mut self) -> Result<(), String> {
        if self.stopped {
            return Ok(());
        }
        self.file.take();
        self.file = Some(try_acquire(&self.card_id)?.ok_or(BUSY)?);
        self.stopped = true;
        Ok(())
    }
}

#[cfg(test)]
mod tests;
