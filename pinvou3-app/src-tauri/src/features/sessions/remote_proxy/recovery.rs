//! User-confirmed recovery of the active RemoteProxy file.

use std::ffi::OsStr;
use std::fs::{File, TryLockError};
use std::io::Read as _;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use uuid::Uuid;

use crate::platform::filesystem::{self, MovePlainFileOutcome};

use super::{RemoteProxyLoadFailure, RemoteProxyState};

const EPOCH_NAME: &str = "_remote_proxy_recovery_epoch";
const EXPORT_DIR: &str = "_remote_proxy_recovery";

pub(super) fn try_write_owner(path: &Path) -> Result<File> {
    let lock = filesystem::open_private_lock_file(&path.with_extension("recovery.lock"))?;
    match lock.try_lock() {
        Ok(()) => Ok(lock),
        Err(TryLockError::WouldBlock) => bail!("remote_proxy_recovery_busy"),
        Err(TryLockError::Error(error)) => Err(error.into()),
    }
}

pub(super) fn read_epoch(path: &Path) -> Result<Option<Uuid>> {
    let parent = path.parent().context("RemoteProxy parent is missing")?;
    match std::fs::symlink_metadata(parent.join(EPOCH_NAME)) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(error.into()),
        Ok(_) => {}
    }
    let Some(directory) = filesystem::open_existing_private_file_directory(parent)? else {
        bail!("remote_proxy_recovery_epoch_changed");
    };
    let Some(file) = directory.open_plain_file(OsStr::new(EPOCH_NAME))? else {
        bail!("remote_proxy_recovery_epoch_changed");
    };
    let mut bytes = Vec::new();
    file.take(37).read_to_end(&mut bytes)?;
    if bytes.len() != 36 {
        bail!("remote_proxy_recovery_epoch_invalid");
    }
    Ok(Some(Uuid::parse_str(std::str::from_utf8(&bytes)?)?))
}

impl super::super::SessionStore {
    pub(crate) fn recover_remote_proxy_file(
        &self,
        quarantine_confirmed: bool,
    ) -> Result<Option<PathBuf>> {
        let mut state = self.remote_proxies.lock();
        if !state.load_failed {
            bail!("remote_proxy_recovery_not_needed");
        }
        let path = state.path.clone();
        let sessions = self.manager.sessions_dir();
        let _writer = try_write_owner(&path)?;
        let failure = match RemoteProxyState::load(path.clone(), sessions) {
            Ok(loaded) => {
                *state = loaded;
                return Ok(None);
            }
            Err(error) => error,
        };
        if !quarantine_confirmed {
            bail!("remote_proxy_recovery_retry_failed");
        }
        match failure.downcast_ref::<RemoteProxyLoadFailure>() {
            Some(
                RemoteProxyLoadFailure::Unreadable
                | RemoteProxyLoadFailure::UnsupportedVersion
                | RemoteProxyLoadFailure::ArchiveUnavailable,
            ) => {
                bail!("remote_proxy_recovery_quarantine_unavailable");
            }
            _ => {}
        }

        // Verify the independent archive bank before taking the active file
        // offline, even when the malformed input never reached archive checks.
        super::archive::ArchiveStore::at(path.with_extension("archives.sqlite3"))
            .require_present()?;
        let directory = filesystem::open_existing_private_file_directory(sessions)?
            .context("RemoteProxy directory is missing")?;
        let name = path
            .file_name()
            .context("RemoteProxy filename is missing")?;
        if directory.open_plain_file(name)?.is_none() {
            bail!("remote_proxy_recovery_source_changed");
        }
        let exports = match directory.open_child_directory(OsStr::new(EXPORT_DIR)) {
            Ok(existing) => existing,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                directory.create_private_child_directory(OsStr::new(EXPORT_DIR))?
            }
            Err(error) => return Err(error.into()),
        };
        let epoch = Uuid::new_v4();
        let label = epoch.to_string();
        let destination = exports.create_private_child_directory(OsStr::new(&label))?;
        // Fence older in-memory writers before removing the active path. The
        // move is anchored, refuses an existing destination and preserves all
        // original bytes even when the source exceeds the read budget.
        directory.atomic_write_private_file(OsStr::new(EPOCH_NAME), label.as_bytes())?;
        if directory.move_plain_file_to(name, &destination, name)? != MovePlainFileOutcome::Moved {
            bail!("remote_proxy_recovery_source_changed");
        }
        let exported = sessions.join(EXPORT_DIR).join(label).join(name);
        let loaded = RemoteProxyState::load(path, sessions)?;
        *state = loaded;
        Ok(Some(exported))
    }
}
