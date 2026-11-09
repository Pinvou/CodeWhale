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
        downloading INTEGER NOT NULL, target_started INTEGER NOT NULL DEFAULT 1);
        CREATE TABLE IF NOT EXISTS model_download_retained_roots (
        card_id TEXT NOT NULL, models_root TEXT NOT NULL,
        PRIMARY KEY(card_id, models_root));
        CREATE TABLE IF NOT EXISTS model_download_root_measurements (
        models_root TEXT PRIMARY KEY, measured_bytes INTEGER NOT NULL)",
    )?;
    let has_started_column = || {
        conn.query_row(
            "SELECT EXISTS(SELECT 1 FROM pragma_table_info('model_download_reservations')
             WHERE name='target_started')",
            [],
            |row| row.get::<_, bool>(0),
        )
    };
    if !has_started_column()? {
        // Existing rows may already own partial files. Only new attempts can
        // prove that their scripts have not started; never unpin old targets.
        if let Err(error) = conn.execute(
            "ALTER TABLE model_download_reservations ADD COLUMN target_started INTEGER NOT NULL DEFAULT 1",
            [],
        ) {
            // Another process can finish this additive migration first.
            if !has_started_column()? {
                return Err(error);
            }
        }
    }
    Ok(())
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

pub(super) fn has_pending_plan(card_id: &str) -> Result<bool, String> {
    let conn = connection()?;
    table(&conn).map_err(|e| e.to_string())?;
    conn.query_row(
        "SELECT EXISTS(SELECT 1 FROM model_download_reservations WHERE card_id=?1 AND downloading=1)",
        [card_id], |row| row.get(0),
    ).map_err(|e| e.to_string())
}

/// Walk one download root now, excluding every currently registered managed
/// version tree (those bytes are charged by their own version rows).
fn walk_root_now(root: &Path) -> Result<u64, String> {
    let isolated_roots = list(None)?
        .into_iter()
        .filter(|v| v.managed)
        .filter_map(|v| v.models_root.map(PathBuf::from))
        .collect::<Vec<_>>();
    quota_dir_size_bytes_excluding(root, &isolated_roots)
}

/// Register a download root's freshly measured size (review round 7): quota
/// admission reads this instead of walking every download root under the
/// global quota lock. Measurements are refreshed at publish, completion and
/// uninstall transitions; bytes written by an in-flight script after its
/// publish stay covered by that script's full-size reservation.
fn upsert_root_measurement_on(
    conn: &rusqlite::Connection,
    root: &Path,
    bytes: u64,
) -> Result<(), String> {
    table(conn).map_err(|e| e.to_string())?;
    conn.execute(
        "INSERT INTO model_download_root_measurements(models_root,measured_bytes)
        VALUES(?1,?2) ON CONFLICT(models_root) DO UPDATE SET measured_bytes=excluded.measured_bytes",
        params![
            root.to_string_lossy(),
            i64::try_from(bytes).map_err(|e| e.to_string())?
        ],
    )
    .map_err(|e| e.to_string())?;
    Ok(())
}

fn upsert_root_measurement(root: &Path, bytes: u64) -> Result<(), String> {
    upsert_root_measurement_on(&connection()?, root, bytes)
}

/// Registered size of one download root; a root without a measurement (first
/// read after this column existed) calibrates once with a fail-closed walk.
fn registered_root_bytes(root: &Path) -> Result<u64, String> {
    let conn = connection()?;
    table(&conn).map_err(|e| e.to_string())?;
    let stored: Option<i64> = conn
        .query_row(
            "SELECT measured_bytes FROM model_download_root_measurements WHERE models_root=?1",
            [root.to_string_lossy().as_ref()],
            |row| row.get(0),
        )
        .map(Some)
        .or_else(|error| match error {
            rusqlite::Error::QueryReturnedNoRows => Ok(None),
            other => Err(other),
        })
        .map_err(|e| e.to_string())?;
    match stored.and_then(|bytes| u64::try_from(bytes).ok()) {
        Some(bytes) => Ok(bytes),
        None => {
            let bytes = walk_root_now(root)?;
            upsert_root_measurement(root, bytes)?;
            Ok(bytes)
        }
    }
}

pub(super) fn release_plan(card_id: &str) -> Result<(), String> {
    let mut conn = connection()?;
    table(&conn).map_err(|e| e.to_string())?;
    let tx = conn
        .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)
        .map_err(|e| e.to_string())?;
    // A first attempt rejected before scripts is not a retry target. Preserve
    // its entire root for quota/residual accounting without pinning the next
    // installation there. Keep old targets and installed records unchanged.
    let pending = "card_id=?1 AND target_started=0
        AND NOT EXISTS(SELECT 1 FROM store_models WHERE card_id=?1)";
    tx.execute(
        &format!(
            "INSERT OR IGNORE INTO model_download_retained_roots(card_id,models_root)
        SELECT card_id,models_root FROM model_download_reservations WHERE {pending}"
        ),
        [card_id],
    )
    .map_err(|e| e.to_string())?;
    tx.execute(
        &format!("DELETE FROM model_download_reservations WHERE {pending}"),
        [card_id],
    )
    .map_err(|e| e.to_string())?;
    tx.execute(
        "UPDATE model_download_reservations SET downloading=0,target_started=1 WHERE card_id=?1",
        [card_id],
    )
    .map_err(|e| e.to_string())?;
    tx.commit().map_err(|e| e.to_string())
}

/// A lease makes process crashes distinguishable from slow active downloads,
/// including direct manager callers that do not use the service's card lock.
pub struct DownloadReservation {
    card_id: String,
    pub root: PathBuf,
    lease: super::DownloadLease,
    finished: bool,
    preserve_target: bool,
}

impl DownloadReservation {
    pub fn acquire(
        card_id: &str,
        configured_root: &Path,
        cancelled: impl Fn() -> bool,
        mut on_wait: impl FnMut(),
    ) -> Result<Self, String> {
        let lease = super::acquire_download_lease(card_id, cancelled, &mut on_wait)?;
        // Owning the lease excludes previous local fd-holding writers. Recover
        // an unstarted first attempt before choosing the current target.
        release_plan(card_id)?;
        let stored = db::get_store_model(card_id).map_err(|e| e.to_string())?;
        let installed = stored.is_some();
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
        let preserve_target = installed || registered.is_some();
        Ok(Self {
            card_id: card_id.into(),
            root,
            lease,
            finished: false,
            preserve_target,
        })
    }

    pub(in super::super) fn script_lease(&mut self) -> &mut super::DownloadLease {
        &mut self.lease
    }

    pub(in super::super) fn confirm_stopped(&mut self) -> Result<(), String> {
        self.lease.confirm_stopped()
    }

    /// Publish under version-quota.lock, then check admission before any script.
    /// Ordinary scripts choose their own subdirectories in a shared root, so no
    /// preexisting file can safely reduce this full-size reservation.
    pub fn publish(&self, size_bytes: u64) -> Result<(), String> {
        let conn = connection()?;
        table(&conn).map_err(|e| e.to_string())?;
        conn.execute(
            "INSERT INTO model_download_reservations(card_id,models_root,size_bytes,downloading,target_started)
            VALUES(?1,?2,?3,1,?4) ON CONFLICT(card_id) DO UPDATE SET
            models_root=excluded.models_root,size_bytes=excluded.size_bytes,downloading=1,
            target_started=MAX(model_download_reservations.target_started,excluded.target_started)",
            params![
                self.card_id,
                self.root.to_string_lossy(),
                i64::try_from(size_bytes).map_err(|e| e.to_string())?,
                self.preserve_target,
            ],
        )
        .map_err(|e| e.to_string())?;
        // Register the root's current size so later quota reads charge its
        // retained bytes without re-walking it (review round 7).
        upsert_root_measurement(&self.root, walk_root_now(&self.root)?)?;
        Ok(())
    }

    /// Persist the target immediately before entering the first script stage.
    /// A crash or failure after this point must keep possible script outputs in
    /// the same retry location, including engine probes/installers.
    pub fn start_scripts(&self) -> Result<(), String> {
        let changed = connection()?
            .execute(
                "UPDATE model_download_reservations SET target_started=1
                 WHERE card_id=?1 AND models_root=?2 AND downloading=1",
                params![self.card_id, self.root.to_string_lossy()],
            )
            .map_err(|e| e.to_string())?;
        if changed != 1 {
            return Err("下载配额登记已变化，未执行模型脚本。".into());
        }
        Ok(())
    }

    /// Call under the same quota lock after add_store_model_with_root has
    /// atomically registered the model and fixed root. Keep this non-active
    /// row so extra caches remain charged even after successful completion.
    pub fn complete(&mut self) -> Result<(), String> {
        self.confirm_stopped()?;
        release_plan(&self.card_id)?;
        // The scripts have written their final bytes; refresh the root's
        // registered size so post-download reality is what later quota reads
        // charge (the covered record's own size is subtracted at read time).
        upsert_root_measurement(&self.root, walk_root_now(&self.root)?)?;
        self.finished = true;
        Ok(())
    }
}

impl Drop for DownloadReservation {
    fn drop(&mut self) {
        if !self.finished {
            if let Err(error) = self
                .confirm_stopped()
                .and_then(|()| release_plan(&self.card_id))
            {
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
    let mut roots = BTreeMap::<PathBuf, (u64, u64)>::new();
    // A completed uninstall ends this card's retry location, but its shared
    // download root may still contain caches or unrelated files. Merge those
    // roots into the same path map so reinstalls and other cards charge them
    // once without letting them choose the next installation's target.
    for root in retained_roots(None)? {
        roots.entry(root).or_default();
    }
    for mut download in all {
        if download.downloading
            && let Some(_lease) = super::try_download_lease(&download.card_id)?
        {
            release_plan(&download.card_id)?;
            download.downloading = false;
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
            // Registered accounting (review round 7): read the root's
            // registered measurement instead of walking every download root
            // under the quota lock; a root without one calibrates once.
            let actual = registered_root_bytes(&root)?;
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
pub(super) fn retire_on(conn: &rusqlite::Connection, card_id: &str) -> Result<(), String> {
    table(conn).map_err(|e| e.to_string())?;
    let roots: Vec<String> = conn
        .prepare("SELECT models_root FROM model_download_reservations WHERE card_id=?1")
        .and_then(|mut query| {
            query
                .query_map([card_id], |row| row.get::<_, String>(0))
                .and_then(|rows| rows.collect::<Result<Vec<_>, _>>())
        })
        .map_err(|e| e.to_string())?;
    conn.execute(
        "INSERT OR IGNORE INTO model_download_retained_roots(card_id,models_root)
        SELECT card_id,models_root FROM model_download_reservations WHERE card_id=?1",
        [card_id],
    )
    .map_err(|e| e.to_string())?;
    conn.execute(
        "DELETE FROM model_download_reservations WHERE card_id=?1",
        [card_id],
    )
    .map_err(|e| e.to_string())?;
    // The uninstall script has removed its weights; refresh each retired
    // root's registered size in the same transaction so the residual charge
    // matches what actually remains (caches, unrelated files) instead of the
    // pre-uninstall walk.
    for root in roots {
        let _ =
            upsert_root_measurement_on(conn, Path::new(&root), walk_root_now(Path::new(&root))?);
    }
    Ok(())
}

pub(super) fn forget_on(conn: &rusqlite::Connection, card_id: &str) -> Result<(), String> {
    table(conn).map_err(|e| e.to_string())?;
    // Abandonment drops every charge for the card's roots by design; a later
    // read of a still-shared root calibrates a fresh measurement once. The
    // roots must be captured before their membership rows are deleted.
    conn.execute(
        "DELETE FROM model_download_root_measurements WHERE models_root IN (
            SELECT models_root FROM model_download_reservations WHERE card_id=?1
            UNION SELECT models_root FROM model_download_retained_roots WHERE card_id=?1)",
        [card_id],
    )
    .map_err(|e| e.to_string())?;
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
