//! Version snapshots and atomic activation. Credentials remain attached to the public card.
use super::{card_update, db};
use rusqlite::{OptionalExtension, params};
use serde::{Deserialize, Serialize};

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
}

fn connection() -> Result<rusqlite::Connection, String> {
    let conn = db::connect().map_err(|e| e.to_string())?;
    Ok(conn)
}

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
    conn.execute("INSERT INTO model_versions(card_id,version_id,state,record_json,models_root,runtime_card_id,resource_dir,managed,created_at,error)
        VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10) ON CONFLICT(card_id,version_id) DO UPDATE SET
        state=excluded.state,record_json=excluded.record_json,models_root=excluded.models_root,
        runtime_card_id=excluded.runtime_card_id,resource_dir=excluded.resource_dir,managed=excluded.managed,error=excluded.error",
        params![version.card_id,version.version_id,version.state,record,version.models_root,
            version.runtime_card_id,version.resource_dir,version.managed,version.created_at,version.error])
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
    Ok(list(Some(card_id))?.iter().any(|v| v.state != "current"))
}

pub fn retained_bytes() -> Result<u64, String> {
    let mut seen = std::collections::BTreeSet::new();
    let mut total = 0u64;
    for version in list(None)?.into_iter().filter(|v| v.state != "current") {
        let key = version
            .resource_dir
            .clone()
            .unwrap_or_else(|| format!("{}:{}", version.card_id, version.version_id));
        if seen.insert(key) {
            total = total.saturating_add(
                version
                    .record
                    .as_ref()
                    .map(|r| r.size_bytes.max(0) as u64)
                    .unwrap_or_else(|| {
                        version
                            .models_root
                            .as_ref()
                            .map(|p| super::disk::dir_size_bytes(std::path::Path::new(p)))
                            .unwrap_or(0)
                    }),
            );
        }
    }
    Ok(total)
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
    use std::os::unix::fs::OpenOptionsExt;
    let safe = super::config::safe_path_component(card_id)?;
    let lock_dir = super::config::data_dir().join("operation-locks");
    std::fs::create_dir_all(&lock_dir).map_err(|e| e.to_string())?;
    let file = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .mode(0o600)
        .open(lock_dir.join(format!("{safe}.lock")))
        .map_err(|e| e.to_string())?;
    if file.try_lock().is_err() {
        return Ok(());
    }
    for mut version in list(Some(card_id))?
        .into_iter()
        .filter(|v| v.state == "downloading")
    {
        version.state = "failed".into();
        version.error = Some("version_download_interrupted".into());
        put(&version)?;
    }
    Ok(())
}
