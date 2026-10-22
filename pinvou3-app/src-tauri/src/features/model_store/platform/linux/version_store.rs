//! Version snapshots and atomic activation. Credentials remain attached to the public card.
use super::{card_update, db};
use rusqlite::{OptionalExtension, params};
use serde::{Deserialize, Serialize};

mod downloads;
pub use downloads::{DownloadReservation, download_paths};

pub(super) fn retire_download_on(
    conn: &rusqlite::Connection,
    card_id: &str,
) -> rusqlite::Result<()> {
    downloads::retire_on(conn, card_id)
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Version {
    pub card_id: String,
    pub version_id: String,
    pub state: String,
    pub record: Option<db::StoreModelRecord>,
    pub models_root: Option<String>,
    pub runtime_card_id: String,
    pub resource_dir: Option<String>,
    pub managed: bool,
    pub created_at: String,
    pub error: Option<String>,
    /// Total candidate bytes reserved while downloading, including existing
    /// unrelated files. Failed/interrupted candidates charge actual files only.
    #[serde(default)]
    pub planned_bytes: u64,
}

fn connection() -> Result<rusqlite::Connection, String> {
    let conn = db::connect().map_err(|e| e.to_string())?;
    Ok(conn)
}

/// Shared holders retain references throughout preparation, download, deployment
/// and enablement, including queue waits: scripts may create shared resources at
/// any of those stages. Destructive operations wait for those holders rather
/// than failing immediately; all waiting is cancellable before any mutation.
fn operation_lock_file(name: &str) -> Result<std::fs::File, String> {
    let root = super::config::data_dir();
    std::fs::create_dir_all(&root).map_err(|e| e.to_string())?;
    // Keep the existing inode/path: moving locks while older processes are
    // alive would silently split their mutual exclusion.
    crate::platform::filesystem::open_private_lock_file(&root.join(name)).map_err(|e| e.to_string())
}

fn try_resource_lock(lock: &std::fs::File, exclusive: bool) -> Result<(), std::fs::TryLockError> {
    if exclusive {
        lock.try_lock()
    } else {
        lock.try_lock_shared()
    }
}

#[cfg(test)]
pub fn resource_operation_lock(exclusive: bool) -> Result<std::fs::File, String> {
    let lock = operation_lock_file("version-operations.lock")?;
    try_resource_lock(&lock, exclusive).map_err(|e| e.to_string())?;
    Ok(lock)
}

pub fn wait_resource_operation_lock(
    exclusive: bool,
    cancelled: impl Fn() -> bool,
    on_wait: impl FnMut(),
) -> Result<std::fs::File, String> {
    wait_operation_lock("version-operations.lock", exclusive, cancelled, on_wait)
}

/// Serialize quota checking and publishing its reservation across app processes.
/// Release before running scripts; downloads still hold shared resource locks.
pub fn wait_quota_reservation_lock(
    cancelled: impl Fn() -> bool,
    on_wait: impl FnMut(),
) -> Result<std::fs::File, String> {
    wait_operation_lock("version-quota.lock", true, cancelled, on_wait)
}

fn wait_operation_lock(
    name: &str,
    exclusive: bool,
    cancelled: impl Fn() -> bool,
    mut on_wait: impl FnMut(),
) -> Result<std::fs::File, String> {
    let lock = operation_lock_file(name)?;
    let mut reported = false;
    loop {
        if cancelled() {
            return Err("已取消等待模型资源操作".into());
        }
        match try_resource_lock(&lock, exclusive) {
            Ok(()) => return Ok(lock),
            Err(std::fs::TryLockError::WouldBlock) => {
                if !reported {
                    on_wait();
                    reported = true;
                }
                std::thread::sleep(std::time::Duration::from_millis(50));
            }
            Err(std::fs::TryLockError::Error(error)) => return Err(error.to_string()),
        }
    }
}

#[cfg(test)]
mod resource_lock_tests;

fn from_row(row: &rusqlite::Row<'_>) -> rusqlite::Result<Version> {
    let raw: Option<String> = row.get("record_json")?;
    let record = raw
        .map(|s| serde_json::from_str(&s))
        .transpose()
        .map_err(|e| {
            rusqlite::Error::FromSqlConversionFailure(0, rusqlite::types::Type::Text, Box::new(e))
        })?;
    Ok(Version {
        card_id: row.get("card_id")?,
        version_id: row.get("version_id")?,
        state: row.get("state")?,
        record,
        models_root: row.get("models_root")?,
        runtime_card_id: row.get("runtime_card_id")?,
        resource_dir: row.get("resource_dir")?,
        managed: row.get("managed")?,
        created_at: row.get("created_at")?,
        error: row.get("error")?,
        planned_bytes: row.get::<_, i64>("planned_bytes")?.max(0) as u64,
    })
}

pub fn list(card_id: Option<&str>) -> Result<Vec<Version>, String> {
    let conn = connection()?;
    let mut query=conn.prepare("SELECT * FROM model_versions WHERE (?1 IS NULL OR card_id=?1) ORDER BY created_at DESC")
        .map_err(|e| e.to_string())?;
    query
        .query_map([card_id], from_row)
        .map_err(|e| e.to_string())?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| e.to_string())
}

pub fn get(card_id: &str, version_id: &str) -> Result<Option<Version>, String> {
    connection()?
        .query_row(
            "SELECT * FROM model_versions WHERE card_id=?1 AND version_id=?2",
            params![card_id, version_id],
            from_row,
        )
        .optional()
        .map_err(|e| e.to_string())
}

fn put_on(conn: &rusqlite::Connection, version: &Version) -> Result<(), String> {
    let record = version
        .record
        .as_ref()
        .map(serde_json::to_string)
        .transpose()
        .map_err(|e| e.to_string())?;
    let planned = if version.state == "downloading" {
        i64::try_from(version.planned_bytes).map_err(|e| e.to_string())?
    } else {
        0
    };
    conn.execute("INSERT INTO model_versions(card_id,version_id,state,record_json,models_root,runtime_card_id,resource_dir,managed,created_at,error,planned_bytes)
        VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11) ON CONFLICT(card_id,version_id) DO UPDATE SET
        state=excluded.state,record_json=excluded.record_json,models_root=excluded.models_root,
        runtime_card_id=excluded.runtime_card_id,resource_dir=excluded.resource_dir,managed=excluded.managed,error=excluded.error,planned_bytes=excluded.planned_bytes",
        params![version.card_id,version.version_id,version.state,record,version.models_root,
            version.runtime_card_id,version.resource_dir,version.managed,version.created_at,version.error,planned])
        .map_err(|e| e.to_string())?;
    Ok(())
}

pub fn put(version: &Version) -> Result<(), String> {
    put_on(&connection()?, version)
}

pub fn remove(card_id: &str, version_id: &str) -> Result<(), String> {
    let conn = connection()?;
    conn.execute(
        "DELETE FROM model_versions WHERE card_id=?1 AND version_id=?2 AND state!='current'",
        params![card_id, version_id],
    )
    .map_err(|e| e.to_string())?;
    Ok(())
}

pub fn active_id(rec: &db::StoreModelRecord) -> String {
    rec.active_version_id
        .clone()
        .unwrap_or_else(|| card_update::revision(&card_update::local_manifest(rec)))
}

pub fn activate(
    expected: &db::StoreModelRecord,
    old: &Version,
    next: &Version,
) -> Result<(), String> {
    let mut conn = connection()?;
    let tx = conn
        .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)
        .map_err(|e| e.to_string())?;
    let current = tx
        .query_row(
            "SELECT * FROM store_models WHERE card_id=?1",
            [&expected.card_id],
            db::StoreModelRecord::from_row,
        )
        .map_err(|e| e.to_string())?;
    if current.running || serde_json::to_value(&current).ok() != serde_json::to_value(expected).ok()
    {
        return Err("模型状态已变化，未切换版本；请刷新后重试。".into());
    }
    let persisted = tx
        .query_row(
            "SELECT * FROM model_versions WHERE card_id=?1 AND version_id=?2",
            params![next.card_id, next.version_id],
            from_row,
        )
        .map_err(|e| e.to_string())?;
    if persisted.models_root != next.models_root
        || persisted.resource_dir != next.resource_dir
        || persisted.managed != next.managed
        || persisted.record.as_ref().map(card_update::local_manifest)
            != next.record.as_ref().map(card_update::local_manifest)
        || !["ready", "history"].contains(&persisted.state.as_str())
        || persisted.runtime_card_id != next.runtime_card_id
    {
        return Err("目标版本状态已变化，未切换版本。".into());
    }
    put_on(&tx, old)?;
    let mut active = next.clone();
    active.state = "current".into();
    active.planned_bytes = 0;
    let rec = active.record.as_mut().ok_or("目标版本尚未完成下载验证")?;
    rec.card_id = current.card_id.clone();
    rec.model_name = current.model_name.clone();
    rec.active_version_id = Some(next.version_id.clone());
    write_active(&tx, rec)?;
    put_on(&tx, &active)?;
    tx.commit().map_err(|e| e.to_string())
}

fn write_active(conn: &rusqlite::Connection, r: &db::StoreModelRecord) -> Result<(), String> {
    conn.execute("UPDATE store_models SET card_name=?2,org=?3,description=?4,engine_name=?5,engine_version=?6,
        size_bytes=?7,quantization=?8,source_repo_id=?9,source_filename=?10,status=?11,running=0,
        scripts_content_json=?12,assets_content_json=?13,profiles_json=?14,active_profile_id=?15,
        downloaded_at=?16,deployed_at=?17,endpoint_url=?18,endpoint_model=?19,scripts_fetched_at=?20,
        deploy_topology=?21,deployed_script_sha256=?22,card_source=?23,card_update_pending=?24,
        container_name=?25,models_root=?26,runtime_card_id=?27,active_version_id=?28,
        script_install_engine=?29,script_download_model=?30,script_deploy=?31,script_enable=?32,script_stop=?33,script_uninstall=?34
        WHERE card_id=?1",params![r.card_id,r.card_name,r.org,r.description,r.engine_name,r.engine_version,
        r.size_bytes,r.quantization,r.source_repo_id,r.source_filename,r.status,
        serde_json::to_string(&r.scripts_content).map_err(|e|e.to_string())?,
        serde_json::to_string(&r.assets_content).map_err(|e|e.to_string())?,
        serde_json::to_string(&r.profiles).map_err(|e|e.to_string())?,r.active_profile_id,
        r.downloaded_at,r.deployed_at,r.endpoint_url,r.endpoint_model,r.scripts_fetched_at,
        if r.deploy_topology==super::store_client::DeployTopology::Single {"single"} else {"dual"},
        r.deployed_script_sha256,r.card_source,r.card_update_pending,r.container_name,
        r.models_root,r.runtime_card_id,r.active_version_id,
        r.scripts.get("install_engine").cloned().flatten(),r.scripts.get("download_model").cloned().flatten(),
        r.scripts.get("deploy").cloned().flatten(),r.scripts.get("enable").cloned().flatten(),
        r.scripts.get("stop").cloned().flatten(),r.scripts.get("uninstall").cloned().flatten()])
        .map_err(|e|e.to_string())?;
    Ok(())
}

pub fn has_history(card_id: &str) -> Result<bool, String> {
    let active = db::get_store_model(card_id)
        .map_err(|e| e.to_string())?
        .as_ref()
        .map(active_id);
    Ok(list(Some(card_id))?
        .iter()
        .any(|v| Some(&v.version_id) != active.as_ref()))
}

pub fn retained_bytes() -> Result<u64, String> {
    retained_bytes_replacing(None)
}

/// A retry replaces its own reservation while keeping its existing files
/// charged. Call under the quota reservation lock before publishing the new plan.
pub fn retained_bytes_without_reservation(card_id: &str, version_id: &str) -> Result<u64, String> {
    retained_bytes_replacing(Some((card_id, version_id)))
}

/// Quota admission must not treat an unreadable retained tree as empty. Unlike
/// the best-effort UI measurement, propagate every scan error except a positively
/// absent root. Do not follow links, including a replaced root directory.
pub(super) fn quota_dir_size_bytes(root: &std::path::Path) -> Result<u64, String> {
    quota_dir_size_bytes_excluding(root, &[])
}

fn quota_dir_size_bytes_excluding(
    root: &std::path::Path,
    excluded: &[std::path::PathBuf],
) -> Result<u64, String> {
    let scan_error = |path: &std::path::Path, error: std::io::Error| {
        format!("无法核算版本目录配额 {}：{error}", path.display())
    };
    for ancestor in root.ancestors().skip(1) {
        match ancestor.symlink_metadata() {
            Ok(meta) if meta.file_type().is_symlink() => {
                return Err("无法核算版本目录配额：祖先路径包含链接。".into());
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(scan_error(ancestor, error)),
        }
    }
    match root.symlink_metadata() {
        Ok(meta) if meta.is_dir() => {}
        Ok(_) => return Err("无法核算版本目录配额：根路径不是普通目录。".into()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(0),
        Err(error) => return Err(scan_error(root, error)),
    }
    let mut total = 0u64;
    let mut pending = vec![root.to_path_buf()];
    while let Some(directory) = pending.pop() {
        for entry in std::fs::read_dir(&directory).map_err(|error| scan_error(&directory, error))? {
            let entry = entry.map_err(|error| scan_error(&directory, error))?;
            let path = entry.path();
            if excluded.contains(&path) {
                continue;
            }
            let meta = path
                .symlink_metadata()
                .map_err(|error| scan_error(&path, error))?;
            if meta.is_dir() {
                pending.push(path);
            } else if meta.is_file() {
                total = total
                    .checked_add(meta.len())
                    .ok_or("版本目录配额统计溢出，已中止下载。")?;
            }
        }
    }
    Ok(total)
}

#[cfg(test)]
mod quota_tests;

fn retained_bytes_replacing(replacing: Option<(&str, &str)>) -> Result<u64, String> {
    // A current managed generation is already charged by store_models. Keep
    // charging its files above that amount, and deduplicate shared resources
    // before subtracting the active record's contribution.
    let mut resources = std::collections::BTreeMap::<String, (u64, u64)>::new();
    for version in list(None)? {
        let active = if ["current", "deleting"].contains(&version.state.as_str()) {
            db::get_store_model(&version.card_id)
                .map_err(|e| e.to_string())?
                .filter(|r| active_id(r) == version.version_id)
        } else {
            None
        };
        // Legacy generations retain their registered-size accounting. Their
        // models_root may be a shared parent containing unrelated models.
        if !version.managed && (version.state == "current" || active.is_some()) {
            continue;
        }
        let key = version
            .resource_dir
            .clone()
            .unwrap_or_else(|| format!("{}:{}", version.card_id, version.version_id));
        let actual = || {
            version
                .models_root
                .as_ref()
                .map(|p| quota_dir_size_bytes(std::path::Path::new(p)))
                .unwrap_or(Ok(0))
        };
        let registered = version.record.as_ref().map(|r| r.size_bytes.max(0) as u64);
        let covered = active.as_ref().map_or(0, |r| r.size_bytes.max(0) as u64);
        let charge = if active.is_some() {
            actual()?.max(covered)
        } else if version.state == "downloading" {
            let actual = actual()?;
            if replacing == Some((version.card_id.as_str(), version.version_id.as_str())) {
                actual
            } else {
                actual.max(version.planned_bytes)
            }
        } else if version.state == "failed" {
            actual()?
        } else if version.managed {
            actual()?.max(registered.unwrap_or(0))
        } else {
            match registered {
                Some(bytes) => bytes,
                None => actual()?,
            }
        };
        let resource = resources.entry(key).or_default();
        resource.0 = resource.0.max(charge);
        resource.1 = resource.1.saturating_add(covered);
    }
    let version_bytes = resources.values().fold(0u64, |total, (charge, covered)| {
        total.saturating_add(charge.saturating_sub(*covered))
    });
    Ok(version_bytes.saturating_add(downloads::retained_download_bytes()?))
}

pub fn engine_is_retained(name: &str, version: Option<&str>) -> Result<bool, String> {
    Ok(list(None)?
        .into_iter()
        .filter(|v| v.state != "current")
        .filter_map(|v| v.record)
        .any(|r| {
            r.engine_name == name
                && r.engine_version.as_deref().unwrap_or_default() == version.unwrap_or_default()
        }))
}

/// A process crash releases the operation lock. Expose interrupted candidates
/// for retry/cleanup without touching scripts, processes or downloaded files.
pub fn recover_interrupted(card_id: &str) -> Result<(), String> {
    let safe = super::config::safe_path_component(card_id)?;
    let lock_dir = super::config::data_dir().join("operation-locks");
    std::fs::create_dir_all(&lock_dir).map_err(|e| e.to_string())?;
    let file =
        crate::platform::filesystem::open_private_lock_file(&lock_dir.join(format!("{safe}.lock")))
            .map_err(|e| e.to_string())?;
    if file.try_lock().is_err() {
        return Ok(());
    }
    for mut version in list(Some(card_id))?
        .into_iter()
        .filter(|v| v.state == "downloading")
    {
        version.state = "failed".into();
        version.planned_bytes = 0;
        version.error = Some("version_download_interrupted".into());
        put(&version)?;
    }
    Ok(())
}

/// Explicit abandonment keeps every byte and removes all metadata atomically.
/// Only the force-remove endpoint may call this; ordinary deletion retains its
/// history/shared-resource guards. The caller discloses registered paths first.
pub fn abandon_card_records(card_id: &str) -> Result<(), String> {
    let mut conn = connection()?;
    let tx = conn
        .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)
        .map_err(|e| e.to_string())?;
    downloads::forget_on(&tx, card_id)?;
    tx.execute("DELETE FROM model_versions WHERE card_id=?1", [card_id])
        .map_err(|e| e.to_string())?;
    tx.execute("DELETE FROM store_models WHERE card_id=?1", [card_id])
        .map_err(|e| e.to_string())?;
    tx.execute("DELETE FROM api_keys WHERE card_id=?1", [card_id])
        .map_err(|e| e.to_string())?;
    tx.commit().map_err(|e| e.to_string())
}
