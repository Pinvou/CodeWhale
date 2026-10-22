//! Ordinary downloads share quota admission with isolated version staging.
//!
//! Their scripts may use arbitrary names below a shared models root. Account
//! that root conservatively, excluding registered, separately charged isolated
//! trees, and never treat this measurement as authority to delete its contents.
use super::*;
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

#[derive(Clone)]
struct Download {
    card_id: String,
    root: PathBuf,
    size_bytes: u64,
    downloading: bool,
}

fn table(conn: &rusqlite::Connection) -> rusqlite::Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS model_download_reservations (
        card_id TEXT PRIMARY KEY, models_root TEXT NOT NULL, size_bytes INTEGER NOT NULL,
        downloading INTEGER NOT NULL);
        CREATE TABLE IF NOT EXISTS model_download_retained_roots (
        card_id TEXT NOT NULL, models_root TEXT NOT NULL,
        PRIMARY KEY(card_id, models_root))",
    )
}

fn downloads() -> Result<Vec<Download>, String> {
    let conn = connection()?;
    table(&conn).map_err(|e| e.to_string())?;
    let mut query = conn
        .prepare("SELECT * FROM model_download_reservations")
        .map_err(|e| e.to_string())?;
    query
        .query_map([], |row| {
            Ok(Download {
                card_id: row.get("card_id")?,
                root: PathBuf::from(row.get::<_, String>("models_root")?),
                size_bytes: row.get::<_, i64>("size_bytes")?.max(0) as u64,
                downloading: row.get("downloading")?,
            })
        })
        .map_err(|e| e.to_string())?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| e.to_string())
}

fn retained_roots(card_id: Option<&str>) -> Result<Vec<PathBuf>, String> {
    let conn = connection()?;
    table(&conn).map_err(|e| e.to_string())?;
    let mut query = conn
        .prepare(
            "SELECT models_root FROM model_download_retained_roots
            WHERE (?1 IS NULL OR card_id=?1)",
        )
        .map_err(|e| e.to_string())?;
    query
        .query_map([card_id], |row| Ok(PathBuf::from(row.get::<_, String>(0)?)))
        .map_err(|e| e.to_string())?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| e.to_string())
}

fn lease_file(card_id: &str) -> Result<std::fs::File, String> {
    let safe = super::super::config::safe_path_component(card_id)?;
    let directory = super::super::config::data_dir().join("download-locks");
    std::fs::create_dir_all(&directory).map_err(|e| e.to_string())?;
    crate::platform::filesystem::open_private_lock_file(&directory.join(format!("{safe}.lock")))
        .map_err(|e| e.to_string())
}

fn release_plan(card_id: &str) -> Result<(), String> {
    let conn = connection()?;
    table(&conn).map_err(|e| e.to_string())?;
    conn.execute(
        "UPDATE model_download_reservations SET downloading=0 WHERE card_id=?1",
        [card_id],
    )
    .map_err(|e| e.to_string())?;
    Ok(())
}

/// A lease makes process crashes distinguishable from slow active downloads,
/// including direct manager callers that do not use the service's card lock.
pub struct DownloadReservation {
    card_id: String,
    pub root: PathBuf,
    _lease: std::fs::File,
    finished: bool,
}

impl DownloadReservation {
    pub fn acquire(
        card_id: &str,
        configured_root: &Path,
        cancelled: impl Fn() -> bool,
        mut on_wait: impl FnMut(),
    ) -> Result<Self, String> {
        let lease = lease_file(card_id)?;
        let mut reported = false;
        loop {
            if cancelled() {
                return Err("已取消等待下载配额登记".into());
            }
            match lease.try_lock() {
                Ok(()) => break,
                Err(std::fs::TryLockError::WouldBlock) => {
                    if !reported {
                        on_wait();
                        reported = true;
                    }
                    std::thread::sleep(std::time::Duration::from_millis(50));
                }
                Err(std::fs::TryLockError::Error(e)) => return Err(e.to_string()),
            }
        }
        let stored = db::get_store_model(card_id).map_err(|e| e.to_string())?;
        let registered = downloads()?
            .into_iter()
            .find(|d| d.card_id == card_id)
            .map(|d| d.root)
            .or_else(|| stored.and_then(|r| r.models_root.map(PathBuf::from)));
        let root = registered
            .clone()
            .unwrap_or_else(|| configured_root.to_path_buf());
        if registered.is_some() {
            // A retry must not follow a replaced component, even when the old
            // leaf is absent and create_dir_all would otherwise recreate it
            // through a new link. Positive absence permits same-root recovery.
            for path in root.ancestors() {
                match path.symlink_metadata() {
                    Ok(metadata) if metadata.is_dir() => {}
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                    _ => return Err("已登记下载目录或祖先不是普通目录，已保留原记录。".into()),
                }
            }
        }
        std::fs::create_dir_all(&root).map_err(|e| e.to_string())?;
        // A user may explicitly choose a symlink for a first download. Resolve
        // it once and persist the real root; later config/link changes do not
        // redirect the registered retry location.
        let root = root.canonicalize().map_err(|e| e.to_string())?;
        for version in list(None)?.into_iter().filter(|v| v.managed) {
            if version
                .models_root
                .as_deref()
                .is_some_and(|p| root.starts_with(p))
            {
                return Err("普通下载目录位于已登记的隔离版本内，未修改任何资源。".into());
            }
        }
        // A previous owner's lease has ended: a retry replaces its old plan,
        // while retaining every file in its original root for quota accounting.
        release_plan(card_id)?;
        Ok(Self {
            card_id: card_id.into(),
            root,
            _lease: lease,
            finished: false,
        })
    }

    /// Publish under version-quota.lock, then check admission before any script.
    /// Ordinary scripts choose their own subdirectories in a shared root, so no
    /// preexisting file can safely reduce this full-size reservation.
    pub fn publish(&self, size_bytes: u64) -> Result<(), String> {
        let conn = connection()?;
        table(&conn).map_err(|e| e.to_string())?;
        conn.execute(
            "INSERT INTO model_download_reservations(card_id,models_root,size_bytes,downloading)
            VALUES(?1,?2,?3,1) ON CONFLICT(card_id) DO UPDATE SET
            models_root=excluded.models_root,size_bytes=excluded.size_bytes,downloading=1",
            params![
                self.card_id,
                self.root.to_string_lossy(),
                i64::try_from(size_bytes).map_err(|e| e.to_string())?
            ],
        )
        .map_err(|e| e.to_string())?;
        Ok(())
    }

    /// Call under the same quota lock after add_store_model_with_root has
    /// atomically registered the model and fixed root. Keep this non-active
    /// row so extra caches remain charged even after successful completion.
    pub fn complete(&mut self) -> Result<(), String> {
        release_plan(&self.card_id)?;
        self.finished = true;
        Ok(())
    }
}

impl Drop for DownloadReservation {
    fn drop(&mut self) {
        if !self.finished {
            if let Err(error) = release_plan(&self.card_id) {
                eprintln!("[model-store] 保留下载配额记录，等待下次恢复：{error}");
            }
        }
    }
}

/// Failed/completed plans charge their root's actual remaining bytes. Every
/// active ordinary download additionally reserves its full declared size: even
/// a matching basename/digest may belong to another card or be copied rather
/// than reused by a script. Conservative overcount is preferable to exceeding
/// an opt-in quota. Isolated staging uses the same full additional reservation
/// until the host can verify an explicit download-target contract.
pub fn retained_download_bytes() -> Result<u64, String> {
    let all = downloads()?;
    let records = db::list_store_models().map_err(|e| e.to_string())?;
    let isolated_roots = list(None)?
        .into_iter()
        .filter(|v| v.managed)
        .filter_map(|v| v.models_root.map(PathBuf::from))
        .collect::<Vec<_>>();
    let mut roots = BTreeMap::<PathBuf, (u64, u64)>::new();
    // A completed uninstall ends this card's retry location, but its shared
    // download root may still contain caches or unrelated files. Merge those
    // roots into the same path map so reinstalls and other cards charge them
    // once without letting them choose the next installation's target.
    for root in retained_roots(None)? {
        roots.entry(root).or_default();
    }
    for mut download in all {
        if download.downloading {
            let lease = lease_file(&download.card_id)?;
            match lease.try_lock() {
                Ok(()) => {
                    release_plan(&download.card_id)?;
                    download.downloading = false;
                }
                Err(std::fs::TryLockError::WouldBlock) => {}
                Err(std::fs::TryLockError::Error(e)) => return Err(e.to_string()),
            }
        }
        let covered = records
            .iter()
            .find(|r| r.card_id == download.card_id)
            .map_or(0, |r| r.size_bytes.max(0) as u64);
        let remaining = if download.downloading {
            download.size_bytes
        } else {
            0
        };
        let root = roots.entry(download.root).or_default();
        root.0 = root.0.saturating_add(remaining);
        // Legacy rows without a saved root still contribute when a retry has
        // explicitly associated that card with this registered download root.
        if records
            .iter()
            .find(|r| r.card_id == download.card_id)
            .is_some_and(|r| r.models_root.is_none())
        {
            root.1 = root.1.saturating_add(covered);
        }
    }
    roots
        .into_iter()
        .try_fold(0u64, |sum, (root, (planned, legacy_covered))| {
            let covered = records
                .iter()
                .filter(|r| {
                    r.models_root
                        .as_deref()
                        .is_some_and(|p| Path::new(p) == root)
                })
                .fold(legacy_covered, |bytes, record| {
                    bytes.saturating_add(record.size_bytes.max(0) as u64)
                });
            let actual = quota_dir_size_bytes_excluding(&root, &isolated_roots)?;
            Ok(sum.saturating_add(actual.saturating_sub(covered).saturating_add(planned)))
        })
}

pub fn download_paths(card_id: &str) -> Result<Vec<PathBuf>, String> {
    let mut paths = retained_roots(Some(card_id))?;
    paths.extend(
        downloads()?
            .into_iter()
            .filter(|d| d.card_id == card_id)
            .map(|d| d.root),
    );
    paths.sort();
    paths.dedup();
    Ok(paths)
}

/// End the installation's retry target in the caller's successful full-delete
/// transaction. Keep roots separately for conservative residual accounting and
/// disclosure; a subsequent new installation may use the configured directory.
pub(super) fn retire_on(conn: &rusqlite::Connection, card_id: &str) -> rusqlite::Result<()> {
    table(conn)?;
    conn.execute(
        "INSERT OR IGNORE INTO model_download_retained_roots(card_id,models_root)
        SELECT card_id,models_root FROM model_download_reservations WHERE card_id=?1",
        [card_id],
    )?;
    conn.execute(
        "DELETE FROM model_download_reservations WHERE card_id=?1",
        [card_id],
    )?;
    Ok(())
}

pub(super) fn forget_on(conn: &rusqlite::Connection, card_id: &str) -> Result<(), String> {
    table(conn).map_err(|e| e.to_string())?;
    conn.execute(
        "DELETE FROM model_download_reservations WHERE card_id=?1",
        [card_id],
    )
    .map_err(|e| e.to_string())?;
    conn.execute(
        "DELETE FROM model_download_retained_roots WHERE card_id=?1",
        [card_id],
    )
    .map_err(|e| e.to_string())?;
    Ok(())
}

#[cfg(test)]
mod tests;
