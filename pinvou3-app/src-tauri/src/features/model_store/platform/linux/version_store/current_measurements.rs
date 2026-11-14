//! Display-only measurements for ordinary current generations. Keeping these
//! separate from Version rows must not manufacture history or deletion rights.
use super::*;
use std::path::Path;

fn table(conn: &rusqlite::Connection) -> Result<(), String> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS model_current_version_measurements (
         card_id TEXT PRIMARY KEY REFERENCES store_models(card_id) ON DELETE CASCADE,
         version_id TEXT NOT NULL, resource_dir TEXT NOT NULL, measured_bytes INTEGER NOT NULL)",
    )
    .map_err(|error| error.to_string())
}

pub(super) fn invalidate_on(conn: &rusqlite::Connection, card_id: &str) -> Result<(), String> {
    table(conn)?;
    conn.execute(
        "DELETE FROM model_current_version_measurements WHERE card_id=?1",
        [card_id],
    )
    .map_err(|error| error.to_string())?;
    Ok(())
}

pub(in super::super) fn invalidate(card_id: &str) -> Result<(), String> {
    invalidate_on(&connection()?, card_id)
}

pub(in super::super) fn get(
    card_id: &str,
    version_id: &str,
    resource: &str,
) -> Result<Option<u64>, String> {
    let conn = connection()?;
    table(&conn)?;
    let bytes: Option<i64> = conn
        .query_row(
            "SELECT measured_bytes FROM model_current_version_measurements
             WHERE card_id=?1 AND version_id=?2 AND resource_dir=?3",
            params![card_id, version_id, resource],
            |row| row.get(0),
        )
        .optional()
        .map_err(|error| error.to_string())?;
    bytes
        .map(|bytes| u64::try_from(bytes).map_err(|error| error.to_string()))
        .transpose()
}

/// The endpoint has no cancellation token: use immediate lock attempts instead
/// of starting an uncancellable wait behind a long download. Preserve the
/// established card -> resource -> download lease -> quota order. Read the
/// current record only after acquiring this guard.
pub(in super::super) struct MeasurementGuard {
    _card: std::fs::File,
    _resources: std::fs::File,
    _download: std::fs::File,
    _quota: std::fs::File,
}

pub(in super::super) fn lock(card_id: &str) -> Result<MeasurementGuard, String> {
    let busy = || "version_measurement_busy".to_string();
    let safe = super::super::config::safe_path_component(card_id)?;
    let directory = super::super::config::data_dir().join("operation-locks");
    std::fs::create_dir_all(&directory).map_err(|error| error.to_string())?;
    let card = crate::platform::filesystem::open_private_lock_file(
        &directory.join(format!("{safe}.lock")),
    )
    .map_err(|error| error.to_string())?;
    card.try_lock().map_err(|_| busy())?;
    let resources = operation_lock_file("version-operations.lock")?;
    try_resource_lock(&resources, true).map_err(|_| busy())?;
    let download = try_download_lease(card_id)?.ok_or_else(busy)?;
    let quota = operation_lock_file("version-quota.lock")?;
    quota.try_lock().map_err(|_| busy())?;
    // A previous process may have left stopped plans. We own its canonical
    // locks and lease, so recover without recursively acquiring any of them.
    downloads::release_plan(card_id)?;
    recover_interrupted_records(card_id)?;
    Ok(MeasurementGuard {
        _card: card,
        _resources: resources,
        _download: download,
        _quota: quota,
    })
}

/// Scan first, then atomically publish every size. Ordinary resource display
/// and ordinary shared-root quota measurements are distinct: the latter must
/// include the full registered root, excluding separately charged generations.
pub(in super::super) fn remeasure(
    current: &db::StoreModelRecord,
    versions: &mut [Version],
) -> Result<(), String> {
    let mut measured = Vec::with_capacity(versions.len());
    for version in versions.iter() {
        let root = version
            .models_root
            .as_deref()
            .filter(|_| version.managed)
            .or(version.resource_dir.as_deref())
            .ok_or("version_measurement_path_unknown")?;
        measured.push((root.to_string(), quota_dir_size_bytes(Path::new(root))?));
    }
    let mut conn = connection()?;
    let tx = conn
        .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)
        .map_err(|error| error.to_string())?;
    let fresh = tx
        .query_row(
            "SELECT * FROM store_models WHERE card_id=?1",
            [&current.card_id],
            db::StoreModelRecord::from_row,
        )
        .map_err(|error| error.to_string())?;
    if serde_json::to_value(&fresh).ok() != serde_json::to_value(current).ok() {
        return Err("version_measurement_snapshot_changed".into());
    }
    table(&tx)?;
    downloads::remeasure_roots_on(&tx, &current.card_id)?;
    for (version, (resource, bytes)) in versions.iter().zip(&measured) {
        let bytes = i64::try_from(*bytes).map_err(|error| error.to_string())?;
        if version.state == "current" && !version.managed {
            tx.execute(
                "INSERT INTO model_current_version_measurements(card_id,version_id,resource_dir,measured_bytes)
                 VALUES(?1,?2,?3,?4) ON CONFLICT(card_id) DO UPDATE SET
                 version_id=excluded.version_id,resource_dir=excluded.resource_dir,measured_bytes=excluded.measured_bytes",
                params![current.card_id, version.version_id, resource, bytes],
            )
            .map_err(|error| error.to_string())?;
        } else {
            let changed = tx
                .execute(
                    "UPDATE model_versions SET measured_bytes=?3 WHERE card_id=?1 AND version_id=?2",
                    params![current.card_id, version.version_id, bytes],
                )
                .map_err(|error| error.to_string())?;
            if changed != 1 {
                return Err("version_measurement_version_changed".into());
            }
        }
    }
    tx.commit().map_err(|error| error.to_string())?;
    for (version, (_, bytes)) in versions.iter_mut().zip(measured) {
        version.measured_bytes = Some(bytes);
    }
    Ok(())
}

#[cfg(test)]
mod tests;
