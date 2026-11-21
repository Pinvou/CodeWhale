//! Read-only, headless handoff of completed store installations. A directory or
//! experimental artifact alone is never an enrollment receipt. No script runs.
use std::collections::BTreeSet;
use std::io::Read;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::Path;

use rusqlite::{Connection, OpenFlags};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

fn checked_file(path: &Path, limit: u64) -> Result<Vec<u8>, String> {
    for part in path.ancestors() {
        if std::fs::symlink_metadata(part).is_ok_and(|m| m.file_type().is_symlink()) {
            return Err("store_inventory_linked_path".into());
        }
    }
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|e| e.to_string())?;
    let meta = file.metadata().map_err(|e| e.to_string())?;
    // SAFETY: these identity getters have no preconditions or side effects.
    let (uid, gid) = unsafe { (libc::geteuid(), libc::getegid()) };
    if !meta.is_file()
        || meta.uid() != uid
        || (meta.mode() & 0o002 != 0 || (meta.mode() & 0o020 != 0 && meta.gid() != gid))
        || meta.len() > limit
    {
        return Err("store_inventory_untrusted_file".into());
    }
    let mut bytes = Vec::new();
    Read::by_ref(&mut file)
        .take(limit + 1)
        .read_to_end(&mut bytes)
        .map_err(|e| e.to_string())?;
    if bytes.len() as u64 > limit {
        return Err("store_inventory_file_too_large".into());
    }
    Ok(bytes)
}

fn safe_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && !matches!(value, "." | "..")
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
}

/// Uses a read-only SQLite transaction, including WAL, and never creates a DB,
/// migrates schema, reconciles running flags, reads credentials or uses network.
pub fn inventory() -> Result<Value, String> {
    inventory_at(&crate::platform::paths::pinvou3_home().join("model-store"))
}

const SELECTION_POLICY: &str = "latest-installed-v1";
const MAX_METADATA_BYTES: usize = 32 * 1024 * 1024;
const RECIPE_FIELDS: &[&str] = &[
    "engine_name",
    "engine_version",
    "source_repo_id",
    "source_filename",
    "size_bytes",
    "quantization",
    "deploy_topology",
    "profiles_json",
    "active_profile_id",
    "scripts_content_json",
    "assets_content_json",
    "deployed_script_sha256",
    "models_root",
];

fn sha256(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

fn digest(value: &Value) -> Result<String, String> {
    Ok(sha256(
        &serde_json::to_vec(value).map_err(|e| e.to_string())?,
    ))
}

fn columns(connection: &Connection, table: &str) -> Result<BTreeSet<String>, String> {
    connection
        .prepare(&format!("PRAGMA table_info({table})"))
        .map_err(|e| e.to_string())?
        .query_map([], |row| row.get(1))
        .map_err(|e| e.to_string())?
        .collect::<Result<_, _>>()
        .map_err(|e| e.to_string())
}

// Borrow SQLite's text before making any owned copies. The budget covers both
// store rows and version bindings, including legacy recipes with cached files.
fn account_metadata(row: &rusqlite::Row<'_>, used: &mut usize) -> rusqlite::Result<()> {
    for index in 0..row.as_ref().column_count() {
        if let rusqlite::types::ValueRef::Text(text) = row.get_ref(index)? {
            if text.len() > MAX_METADATA_BYTES.saturating_sub(*used) {
                return Err(rusqlite::Error::FromSqlConversionFailure(
                    index,
                    rusqlite::types::Type::Text,
                    Box::new(std::io::Error::other(
                        "store_inventory_metadata_limit_exceeded",
                    )),
                ));
            }
            *used += text.len();
        }
    }
    Ok(())
}

fn envelope(
    cards: &Path,
    models: Vec<Value>,
    skipped: Vec<Value>,
    selections: Vec<Value>,
    archived: Vec<Value>,
) -> Value {
    json!({"format":"pinvou-installed-models-v1", "selection_policy":SELECTION_POLICY,
        "cards_root":cards,"models":models,"skipped":skipped,
        "selections":selections,"archived_models":archived})
}

fn inventory_at(root: &Path) -> Result<Value, String> {
    let database = root.join("models.db");
    let cards = root.join("cards");
    let Some(meta) = super::control_file::metadata_optional(&database)
        .map_err(|error| format!("store_inventory_database_unavailable: {error}"))?
    else {
        return Ok(envelope(&cards, vec![], vec![], vec![], vec![]));
    };
    // SAFETY: these identity getters have no preconditions or side effects.
    let (uid, gid) = unsafe { (libc::geteuid(), libc::getegid()) };
    if !meta.is_file()
        || meta.file_type().is_symlink()
        || meta.uid() != uid
        || (meta.mode() & 0o002 != 0 || (meta.mode() & 0o020 != 0 && meta.gid() != gid))
    {
        return Err("store_inventory_untrusted_database".into());
    }
    for part in database.ancestors() {
        if std::fs::symlink_metadata(part).is_ok_and(|m| m.file_type().is_symlink()) {
            return Err("store_inventory_linked_path".into());
        }
    }
    let mut connection = Connection::open_with_flags(
        &database,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )
    .map_err(|e| e.to_string())?;
    connection
        .busy_timeout(std::time::Duration::from_secs(2))
        .map_err(|e| e.to_string())?;
    let tx = connection.transaction().map_err(|e| e.to_string())?;
    let fields = columns(&tx, "store_models")?;
    for required in ["card_id", "status", "downloaded_at", "card_source"] {
        if !fields.contains(required) {
            return Err("store_inventory_schema_unsupported".into());
        }
    }
    let optional = |name: &str| {
        if fields.contains(name) {
            name.to_string()
        } else {
            "NULL".into()
        }
    };
    let recipe_fields = RECIPE_FIELDS
        .iter()
        .map(|name| optional(name))
        .collect::<Vec<_>>()
        .join(",");
    let query = format!(
        "SELECT card_id,status,downloaded_at,card_source,{},{},{},{} FROM store_models ORDER BY card_id LIMIT 513",
        optional("runtime_card_id"),
        optional("active_version_id"),
        optional("card_update_pending"),
        recipe_fields
    );
    let mut statement = tx.prepare(&query).map_err(|e| e.to_string())?;
    let mut metadata_bytes = 0;
    let rows = statement
        .query_map([], |row| {
            account_metadata(row, &mut metadata_bytes)?;
            let mut recipe = serde_json::Map::new();
            for (i, name) in RECIPE_FIELDS.iter().enumerate() {
                let value = match row.get_ref(i + 7)? {
                    rusqlite::types::ValueRef::Null => Value::Null,
                    rusqlite::types::ValueRef::Integer(value) => json!(value),
                    rusqlite::types::ValueRef::Real(value) => json!(value),
                    rusqlite::types::ValueRef::Text(_) => {
                        Value::String(row.get::<_, String>(i + 7)?)
                    }
                    _ => {
                        return Err(rusqlite::Error::InvalidColumnType(
                            i + 7,
                            name.to_string(),
                            row.get_ref(i + 7)?.data_type(),
                        ));
                    }
                };
                recipe.insert(name.to_string(), value);
            }
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, Option<String>>(2)?.unwrap_or_default(),
                row.get::<_, Option<String>>(3)?,
                row.get::<_, Option<String>>(4)?,
                row.get::<_, Option<String>>(5)?,
                row.get::<_, Option<bool>>(6)?.unwrap_or(false),
                Value::Object(recipe),
            ))
        })
        .map_err(|e| e.to_string())?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| e.to_string())?;
    if rows.len() > 512 {
        return Err("store_inventory_limit_exceeded".into());
    }
    let history_fields = columns(&tx, "model_versions")?;
    let versions = if !history_fields.is_empty() {
        for field in ["card_id", "version_id", "runtime_card_id", "state"] {
            if !history_fields.contains(field) {
                return Err("store_inventory_versions_schema_unsupported".into());
            }
        }
        let mut statement = tx.prepare("SELECT card_id,version_id,runtime_card_id,state FROM model_versions ORDER BY card_id,version_id LIMIT 4097").map_err(|e|e.to_string())?;
        let versions = statement
            .query_map([], |row| {
                account_metadata(row, &mut metadata_bytes)?;
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, String>(1)?,
                    row.get::<_, String>(2)?,
                    row.get::<_, String>(3)?,
                ))
            })
            .map_err(|e| e.to_string())?
            .collect::<Result<Vec<_>, _>>()
            .map_err(|e| e.to_string())?;
        if versions.len() > 4096 {
            return Err("store_inventory_limit_exceeded".into());
        }
        versions
    } else {
        Vec::new()
    };

    let mut models = Vec::new();
    let mut skipped = Vec::new();
    let mut selections = Vec::new();
    let mut archived = Vec::new();
    let mut seen = BTreeSet::new();
    for (logical, status, installed, source, runtime, version, pending, recipe) in rows {
        if !safe_id(&logical) {
            return Err("store_inventory_invalid_binding".into());
        }
        if !matches!(source.as_deref(), Some("store" | "local")) || installed.trim().is_empty() {
            skipped.push(json!({"card":logical,"reason":"not_completed_store_installation"}));
            continue;
        }
        // A versioned record never recovers its runtime by guessing the natural directory.
        if version.is_some() && runtime.as_ref().is_none_or(|v| v.is_empty()) {
            return Err("store_inventory_missing_runtime_binding".into());
        }
        let card = runtime.unwrap_or_else(|| logical.clone());
        if !safe_id(&card) {
            return Err("store_inventory_invalid_binding".into());
        }
        let versioned = version.is_some();
        let version = match version {
            Some(id) if safe_id(&id) => id,
            Some(_) => return Err("store_inventory_invalid_version".into()),
            None => format!(
                "legacy-{}",
                digest(&json!({"logical":logical,"recipe":recipe}))?
            ),
        };
        let selection = json!({"logical_card":logical,"card":card,"version_id":version});
        // A completed current download remains an explicit cleanup identity even
        // before deploy or after keep-weights uninstall. A retained history/ready
        // row cannot become current merely because the top-level row names it.
        let binding_error = if versioned {
            let mut matches = versions.iter().filter(|(owner, id, runtime, _)| {
                owner == &logical && id == &version && runtime == &card
            });
            match (matches.next().map(|row| row.3.as_str()), matches.next()) {
                (Some("current"), None) => None,
                (Some("deleting"), None) => Some("store_deletion_incomplete"),
                _ => Some("store_inventory_current_version_mismatch"),
            }
        } else if status != "deployed"
            && (card != logical || versions.iter().any(|(_, _, runtime, _)| runtime == &card))
        {
            Some("store_inventory_current_version_mismatch")
        } else {
            None
        };
        if !seen.insert(card.clone()) {
            return Err("store_inventory_duplicate_runtime_card".into());
        }
        // Publish identity BEFORE filesystem checks: a broken current package must block old-directory fallbacks.
        selections.push(selection);
        if status != "deployed" {
            let reason =
                binding_error.unwrap_or(if matches!(status.as_str(), "downloaded" | "pending") {
                    "current_not_deployed"
                } else {
                    "store_inventory_invalid_status"
                });
            skipped.push(json!({"card":logical,"reason":reason}));
            continue;
        }
        let result = (|| -> Result<Value, String> {
            if let Some(reason) = binding_error {
                return Err(reason.into());
            }
            if pending {
                return Err("store_redeploy_required".into());
            }
            let directory = cards.join(&card);
            let mut files = serde_json::Map::new();
            let mut names: BTreeSet<String> = [
                "common.sh",
                "install_engine.sh",
                "download_model.sh",
                "card.json",
                "deploy.sh",
                "enable.sh",
                "stop.sh",
                "uninstall.sh",
                "native_engine_process.py",
                ".deploy-env",
                "vendor/.deploy-env",
                "vendor/.env",
                "vendor/start.sh",
                "vendor/compose.yaml",
                "vendor/compose.yml",
                "vendor/docker-compose.yml",
                "vendor/docker-compose.yaml",
            ]
            .into_iter()
            .map(str::to_owned)
            .collect();
            if let Some(raw) = recipe["assets_content_json"].as_str() {
                let assets: serde_json::Map<String, Value> = serde_json::from_str(raw)
                    .map_err(|_| "store_inventory_invalid_recipe".to_string())?;
                let mut source_files = 0;
                for name in assets.keys() {
                    if name.chars().any(char::is_control) {
                        return Err("store_inventory_invalid_recipe".into());
                    }
                    let relative = super::script_runner::sanitize_relative_path(name)
                        .map_err(|_| "store_inventory_invalid_recipe".to_string())?;
                    // Identity files follow import discovery rules, not the
                    // narrower filter used to replay explicitly bundled source.
                    if !super::card_assets::is_card_state_file(&relative) {
                        source_files += 1;
                        if source_files > 1024 {
                            return Err("store_inventory_limit_exceeded".into());
                        }
                        names.insert(relative.to_string_lossy().into_owned());
                    }
                }
            }
            for name in names {
                let path = directory.join(&name);
                if std::fs::symlink_metadata(&path)
                    .is_err_and(|e| e.kind() == std::io::ErrorKind::NotFound)
                {
                    continue;
                }
                let bytes = checked_file(&path, 256 * 1024)?;
                files.insert(name, Value::String(sha256(&bytes)));
            }
            if !files.contains_key("common.sh") && !files.contains_key("install_engine.sh") {
                return Err("store_driver_metadata_missing".into());
            }
            let deployment_revision = digest(
                &json!({"logical_card":logical,"card":card,"version_id":version,"recipe":recipe,"files":files}),
            )?;
            Ok(
                json!({"card":card,"logical_card":logical,"version_id":version,"origin":source,
                "store_status":status,"downloaded_at":installed,"files":files,
                // This exact database value already participates in recipe /
                // deployment_revision. Missing legacy roots remain unknown;
                // neither current settings nor MODEL_DIR proves the old root.
                "models_root":recipe["models_root"],
                "deployment_revision":deployment_revision,"fingerprint":deployment_revision}),
            )
        })();
        match result {
            Ok(value) => models.push(value),
            Err(reason) => skipped.push(json!({"card":logical,"reason":reason})),
        }
    }
    {
        for (logical, version, card, _state) in versions {
            if !safe_id(&logical) || !safe_id(&version) || !safe_id(&card) {
                return Err("store_inventory_invalid_archive".into());
            }
            // All non-selected versions, including ready/failed and removed-current snapshots, are archive-only.
            if selections
                .iter()
                .any(|s| s["card"] == card && s["logical_card"] != logical)
            {
                return Err("store_inventory_conflicting_archive_binding".into());
            }
            if !selections.iter().any(|s| s["card"] == card) {
                let item = json!({"logical_card":logical,"card":card,"version_id":version});
                if !archived.contains(&item) {
                    archived.push(item);
                }
            }
        }
    }
    let value = envelope(&cards, models, skipped, selections, archived);
    if serde_json::to_vec(&value).map_err(|e| e.to_string())?.len() > 1024 * 1024 {
        return Err("store_inventory_limit_exceeded".into());
    }
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    #[test]
    fn compose_cards_need_no_common_script_and_null_unfinished_rows_are_skipped() {
        let root = tempfile::tempdir().unwrap();
        let db = Connection::open(root.path().join("models.db")).unwrap();
        db.execute_batch("CREATE TABLE store_models(card_id TEXT,status TEXT,downloaded_at TEXT,card_source TEXT); INSERT INTO store_models VALUES('compose','deployed','today','local'); INSERT INTO store_models VALUES('unfinished','downloading',NULL,'store');").unwrap();
        let dir = root.path().join("cards/compose");
        std::fs::create_dir_all(&dir).unwrap();
        let metadata = dir.join("install_engine.sh");
        std::fs::write(&metadata, "VENDOR_DIR=/example\ncompose.yaml\n").unwrap();
        let value = inventory_at(root.path()).unwrap();
        assert_eq!(value["models"].as_array().unwrap().len(), 1);
        assert_eq!(
            value["skipped"][0]["reason"],
            "not_completed_store_installation"
        );
        let actual = dir.join("actual");
        std::fs::rename(&metadata, &actual).unwrap();
        std::os::unix::fs::symlink(actual, metadata).unwrap();
        assert!(
            inventory_at(root.path()).unwrap()["models"]
                .as_array()
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn missing_profile_is_empty_and_does_not_create_it() {
        let root = tempfile::tempdir().unwrap();
        let missing = root.path().join("absent");
        assert_eq!(inventory_at(&missing).unwrap()["models"], json!([]));
        assert!(!missing.exists());
    }

    #[test]
    fn unknown_database_paths_do_not_publish_an_empty_selection() {
        use std::os::unix::fs::symlink;
        let root = tempfile::tempdir().unwrap();
        let database = root.path().join("models.db");
        symlink("missing-database", &database).unwrap();
        assert!(inventory_at(root.path()).is_err());
        assert!(
            database
                .symlink_metadata()
                .unwrap()
                .file_type()
                .is_symlink()
        );
        let linked = root.path().join("linked");
        symlink("missing-store", &linked).unwrap();
        assert!(inventory_at(&linked).is_err());
        std::fs::remove_file(&linked).unwrap();
        std::fs::write(&linked, b"not a directory").unwrap();
        assert!(inventory_at(&linked).is_err());
        assert_eq!(std::fs::read(&linked).unwrap(), b"not a directory");
    }

    #[test]
    fn denied_database_path_is_unknown_without_changing_its_permissions() {
        let root = tempfile::tempdir().unwrap();
        let blocked = root.path().join("blocked");
        std::fs::create_dir(&blocked).unwrap();
        std::fs::set_permissions(&blocked, std::fs::Permissions::from_mode(0)).unwrap();
        // SAFETY: geteuid only reads the process identity.
        if unsafe { libc::geteuid() } != 0 {
            assert!(inventory_at(&blocked).is_err());
        }
        assert_eq!(blocked.symlink_metadata().unwrap().mode() & 0o777, 0);
        std::fs::set_permissions(&blocked, std::fs::Permissions::from_mode(0o700)).unwrap();
    }

    #[test]
    fn only_completed_store_installs_are_handed_off_and_runtime_versions_are_preserved() {
        let root = tempfile::tempdir().unwrap();
        let database = root.path().join("models.db");
        let connection = Connection::open(&database).unwrap();
        connection.execute_batch("CREATE TABLE store_models(card_id TEXT,status TEXT,downloaded_at TEXT,card_source TEXT,runtime_card_id TEXT);
          INSERT INTO store_models VALUES('store-card','deployed','today','store','store-card--version');
          INSERT INTO store_models VALUES('local-card','downloaded','today','local',NULL);
          INSERT INTO store_models VALUES('experiment','registered','today','local',NULL);
          INSERT INTO store_models VALUES('test','deployed','today','builtin',NULL);
          INSERT INTO store_models VALUES('unknown','deployed','today',NULL,NULL);").unwrap();
        std::fs::set_permissions(&database, std::fs::Permissions::from_mode(0o600)).unwrap();
        for card in ["store-card--version", "local-card", "experiment"] {
            let path = root.path().join("cards").join(card);
            std::fs::create_dir_all(&path).unwrap();
            std::fs::write(path.join("common.sh"), b"MODEL_ID=x\n").unwrap();
        }
        let before = std::fs::read(&database).unwrap();
        let value = inventory_at(root.path()).unwrap();
        assert_eq!(value["models"].as_array().unwrap().len(), 1);
        assert_eq!(value["models"][0]["card"], "store-card--version");
        assert_eq!(value["skipped"].as_array().unwrap().len(), 4);
        assert_eq!(std::fs::read(&database).unwrap(), before);
        let second = inventory_at(root.path()).unwrap();
        assert_eq!(value, second);
        let file = root.path().join("cards/local-card/common.sh");
        std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o666)).unwrap();
        assert_eq!(
            inventory_at(root.path()).unwrap()["models"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
    }
    fn versioned_fixture() -> tempfile::TempDir {
        let root = tempfile::tempdir().unwrap();
        let db = Connection::open(root.path().join("models.db")).unwrap();
        db.execute_batch("CREATE TABLE store_models(card_id TEXT,status TEXT,downloaded_at TEXT,card_source TEXT,runtime_card_id TEXT,active_version_id TEXT,card_update_pending INTEGER,active_profile_id TEXT);
            INSERT INTO store_models VALUES('qwen','deployed','today','store','qwen--v2','v2',0,'large');
            CREATE TABLE model_versions(card_id TEXT,version_id TEXT,runtime_card_id TEXT,state TEXT);
            INSERT INTO model_versions VALUES('qwen','v1','qwen','history'),('qwen','v2','qwen--v2','current'),('qwen','v3','qwen--v3','ready');").unwrap();
        for card in ["qwen", "qwen--v2", "qwen--v3"] {
            let dir = root.path().join("cards").join(card);
            std::fs::create_dir_all(&dir).unwrap();
            std::fs::write(dir.join("common.sh"), "MODEL_ID=qwen\n").unwrap();
            std::fs::write(dir.join(".deploy-env"), "MAX_CONTEXT=262144\n").unwrap();
        }
        root
    }

    #[test]
    fn current_pointer_is_authoritative_and_every_other_version_is_archive_only() {
        let root = versioned_fixture();
        let before = std::fs::read(root.path().join("models.db")).unwrap();
        let value = inventory_at(root.path()).unwrap();
        assert_eq!(value["selection_policy"], SELECTION_POLICY);
        assert_eq!(
            value["selections"],
            json!([{"logical_card":"qwen","card":"qwen--v2","version_id":"v2"}])
        );
        assert_eq!(value["models"].as_array().unwrap().len(), 1);
        assert_eq!(value["models"][0]["card"], "qwen--v2");
        assert_eq!(value["models"][0]["version_id"], "v2");
        assert_eq!(value["archived_models"].as_array().unwrap().len(), 2);
        assert!(
            value["archived_models"]
                .as_array()
                .unwrap()
                .iter()
                .all(|v| v["card"] != "qwen--v2")
        );
        assert_eq!(
            std::fs::read(root.path().join("models.db")).unwrap(),
            before
        );
        assert_eq!(inventory_at(root.path()).unwrap(), value);
    }

    #[test]
    fn missing_or_untrusted_current_files_and_pending_redeploy_keep_the_pointer_without_falling_back()
     {
        let root = versioned_fixture();
        let file = root.path().join("cards/qwen--v2/common.sh");
        std::fs::remove_file(&file).unwrap();
        let value = inventory_at(root.path()).unwrap();
        assert_eq!(value["models"], json!([]));
        assert_eq!(value["selections"][0]["card"], "qwen--v2");
        assert_eq!(
            value["skipped"][0]["reason"],
            "store_driver_metadata_missing"
        );
        std::os::unix::fs::symlink(root.path().join("cards/qwen/common.sh"), &file).unwrap();
        let value = inventory_at(root.path()).unwrap();
        assert_eq!(value["models"], json!([]));
        assert_eq!(value["selections"][0]["version_id"], "v2");
        let db = Connection::open(root.path().join("models.db")).unwrap();
        db.execute("UPDATE store_models SET card_update_pending=1", [])
            .unwrap();
        let value = inventory_at(root.path()).unwrap();
        assert_eq!(value["models"], json!([]));
        assert_eq!(value["skipped"][0]["reason"], "store_redeploy_required");
        assert_eq!(value["selections"][0]["card"], "qwen--v2");
    }

    #[test]
    fn uninstall_retaining_weights_and_removed_records_cannot_revive_any_directory() {
        let root = versioned_fixture();
        let db = Connection::open(root.path().join("models.db")).unwrap();
        for status in ["downloaded", "pending"] {
            db.execute("UPDATE store_models SET status=?1", [status])
                .unwrap();
            let value = inventory_at(root.path()).unwrap();
            assert_eq!(
                value["selections"],
                json!([{"logical_card":"qwen","card":"qwen--v2","version_id":"v2"}])
            );
            assert_eq!(value["models"], json!([]));
            assert_eq!(value["skipped"][0]["reason"], "current_not_deployed");
            assert!(
                value["archived_models"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .all(|v| v["card"] != "qwen--v2")
            );
        }
        db.execute("DELETE FROM store_models", []).unwrap();
        let value = inventory_at(root.path()).unwrap();
        assert_eq!(value["selections"], json!([]));
        assert_eq!(value["models"], json!([]));
        assert_eq!(value["archived_models"].as_array().unwrap().len(), 3);
    }

    #[test]
    fn context_or_profile_changes_invalidate_the_deployment_fingerprint_without_changing_version_id()
     {
        let root = versioned_fixture();
        let first = inventory_at(root.path()).unwrap();
        std::fs::write(
            root.path().join("cards/qwen--v2/.deploy-env"),
            "MAX_CONTEXT=32768\n",
        )
        .unwrap();
        let second = inventory_at(root.path()).unwrap();
        assert_eq!(first["selections"], second["selections"]);
        assert_ne!(
            first["models"][0]["deployment_revision"],
            second["models"][0]["deployment_revision"]
        );
        let db = Connection::open(root.path().join("models.db")).unwrap();
        db.execute("UPDATE store_models SET active_profile_id='small'", [])
            .unwrap();
        let third = inventory_at(root.path()).unwrap();
        assert_ne!(
            second["models"][0]["deployment_revision"],
            third["models"][0]["deployment_revision"]
        );
    }

    #[test]
    fn broken_version_bindings_and_duplicate_current_runtime_fail_closed() {
        let root = versioned_fixture();
        let db = Connection::open(root.path().join("models.db")).unwrap();
        db.execute("UPDATE store_models SET runtime_card_id=NULL", [])
            .unwrap();
        assert_eq!(
            inventory_at(root.path()).unwrap_err(),
            "store_inventory_missing_runtime_binding"
        );
        db.execute("UPDATE store_models SET runtime_card_id='qwen--v2'", [])
            .unwrap();
        db.execute("INSERT INTO store_models SELECT 'other',status,downloaded_at,card_source,runtime_card_id,active_version_id,card_update_pending,active_profile_id FROM store_models",[]).unwrap();
        assert_eq!(
            inventory_at(root.path()).unwrap_err(),
            "store_inventory_duplicate_runtime_card"
        );
    }
    #[test]
    fn incomplete_current_deletion_keeps_identity_but_never_hands_off_a_startable_model() {
        let root = versioned_fixture();
        let db = Connection::open(root.path().join("models.db")).unwrap();
        db.execute(
            "UPDATE model_versions SET state='deleting' WHERE version_id='v2'",
            [],
        )
        .unwrap();
        let value = inventory_at(root.path()).unwrap();
        assert_eq!(value["models"], json!([]));
        assert_eq!(value["selections"][0]["card"], "qwen--v2");
        assert_eq!(value["skipped"][0]["reason"], "store_deletion_incomplete");
        assert!(
            value["archived_models"]
                .as_array()
                .unwrap()
                .iter()
                .all(|row| row["card"] != "qwen--v2")
        );
    }

    fn set_assets(root: &Path, assets: &serde_json::Map<String, Value>) {
        let db = Connection::open(root.join("models.db")).unwrap();
        if !columns(&db, "store_models")
            .unwrap()
            .contains("assets_content_json")
        {
            db.execute_batch("ALTER TABLE store_models ADD COLUMN assets_content_json TEXT")
                .unwrap();
        }
        db.execute(
            "UPDATE store_models SET assets_content_json=?1",
            [serde_json::to_string(assets).unwrap()],
        )
        .unwrap();
    }

    #[test]
    fn legacy_dependency_snapshots_do_not_displace_current_source_identity_files() {
        let root = versioned_fixture();
        let directory = root.path().join("cards/qwen--v2");
        let mut assets: serde_json::Map<String, Value> = (0..1100)
            .map(|index| {
                (
                    format!(".venv/lib/site-packages/package/module{index}.py"),
                    json!({"content":"old dependency snapshot","sha256":"old"}),
                )
            })
            .collect();
        for name in [".venv/large.py", "vendor/.cache/large.bin"] {
            let path = directory.join(name);
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(&path, vec![b'x'; 256 * 1024 + 1]).unwrap();
            assets.insert(name.into(), json!({"content":"old generated file"}));
        }
        assets.insert(
            ".deploy-env".into(),
            json!({"content":"old deployment config"}),
        );
        assets.insert(
            "vendor\\helper.py".into(),
            json!({"content":"old source snapshot"}),
        );
        std::fs::write(directory.join("vendor/helper.py"), "current source\n").unwrap();
        set_assets(root.path(), &assets);
        let before = std::fs::read(root.path().join("models.db")).unwrap();
        let first = inventory_at(root.path()).unwrap();
        assert_eq!(first["selections"][0]["card"], "qwen--v2");
        assert_eq!(first["models"].as_array().unwrap().len(), 1);
        assert_eq!(first["archived_models"].as_array().unwrap().len(), 2);
        assert_eq!(
            first["models"][0]["files"],
            json!({
                "common.sh":sha256(b"MODEL_ID=qwen\n"),
                ".deploy-env":sha256(b"MAX_CONTEXT=262144\n"),
                "vendor/helper.py":sha256(b"current source\n"),
            })
        );
        assert_eq!(
            std::fs::read(root.path().join("models.db")).unwrap(),
            before
        );
        assert_eq!(
            std::fs::metadata(directory.join(".venv/large.py"))
                .unwrap()
                .len(),
            256 * 1024 + 1
        );

        // Filtering identity files must not normalize away recipe changes.
        assets.insert(
            ".venv/large.py".into(),
            json!({"content":"changed dependency snapshot"}),
        );
        set_assets(root.path(), &assets);
        let second = inventory_at(root.path()).unwrap();
        assert_eq!(first["selections"], second["selections"]);
        assert_eq!(first["models"][0]["files"], second["models"][0]["files"]);
        assert_ne!(
            first["models"][0]["deployment_revision"],
            second["models"][0]["deployment_revision"]
        );
        std::fs::write(directory.join("vendor/helper.py"), "changed source\n").unwrap();
        let third = inventory_at(root.path()).unwrap();
        assert_eq!(
            third["models"][0]["files"]["vendor/helper.py"],
            sha256(b"changed source\n")
        );
        assert_ne!(
            second["models"][0]["deployment_revision"],
            third["models"][0]["deployment_revision"]
        );
    }

    #[test]
    fn effective_source_limit_and_unsafe_excluded_paths_still_fail_closed() {
        let root = versioned_fixture();
        let assets = (0..1025)
            .map(|index| (format!("src/module{index}.py"), json!({"content":"source"})))
            .collect();
        set_assets(root.path(), &assets);
        let result = inventory_at(root.path()).unwrap();
        assert_eq!(result["models"], json!([]));
        assert_eq!(result["selections"][0]["card"], "qwen--v2");
        assert_eq!(
            result["skipped"][0]["reason"],
            "store_inventory_limit_exceeded"
        );
        for path in [
            "../.venv/file.py",
            ".cache/../../file",
            ".venv/invalid\0.py",
        ] {
            set_assets(
                root.path(),
                &[(path.into(), json!({"content":"invalid"}))]
                    .into_iter()
                    .collect(),
            );
            let result = inventory_at(root.path()).unwrap();
            assert_eq!(result["models"], json!([]));
            assert_eq!(result["selections"][0]["card"], "qwen--v2");
            assert_eq!(
                result["skipped"][0]["reason"],
                "store_inventory_invalid_recipe"
            );
        }
    }

    #[test]
    fn text_metadata_budget_covers_multiple_rows_before_owned_copies() {
        let root = versioned_fixture();
        let db = Connection::open(root.path().join("models.db")).unwrap();
        db.execute_batch("ALTER TABLE store_models ADD COLUMN assets_content_json TEXT")
            .unwrap();
        let large = "x".repeat(MAX_METADATA_BYTES / 2);
        db.execute("UPDATE store_models SET assets_content_json=?1", [&large])
            .unwrap();
        db.execute("INSERT INTO store_models SELECT 'other',status,downloaded_at,card_source,'other--v2',active_version_id,card_update_pending,active_profile_id,assets_content_json FROM store_models",[]).unwrap();
        let before = std::fs::read(root.path().join("models.db")).unwrap();
        assert!(
            inventory_at(root.path())
                .unwrap_err()
                .contains("store_inventory_metadata_limit_exceeded")
        );
        assert_eq!(
            std::fs::read(root.path().join("models.db")).unwrap(),
            before
        );

        // A single oversized text value must be rejected before UTF-8 decoding.
        db.execute("DELETE FROM store_models WHERE card_id='other'", [])
            .unwrap();
        db.execute(
            "UPDATE store_models SET assets_content_json=CAST(?1 AS TEXT)",
            [vec![0xff; MAX_METADATA_BYTES + 1]],
        )
        .unwrap();
        assert!(
            inventory_at(root.path())
                .unwrap_err()
                .contains("store_inventory_metadata_limit_exceeded")
        );

        // Version rows share the remaining budget rather than starting a new one.
        let identity_bytes: i64 = db.query_row(
            "SELECT length(card_id)+length(status)+length(downloaded_at)+length(card_source)+length(runtime_card_id)+length(active_version_id)+length(active_profile_id) FROM store_models",
            [],
            |row| row.get(0),
        ).unwrap();
        db.execute(
            "UPDATE store_models SET assets_content_json=?1",
            ["x".repeat(MAX_METADATA_BYTES - identity_bytes as usize - 1)],
        )
        .unwrap();
        assert!(
            inventory_at(root.path())
                .unwrap_err()
                .contains("store_inventory_metadata_limit_exceeded")
        );
    }

    #[test]
    fn fixed_models_root_is_emitted_and_changes_current_deployment_identity() {
        let root = versioned_fixture();
        let db = Connection::open(root.path().join("models.db")).unwrap();
        db.execute_batch("ALTER TABLE store_models ADD COLUMN models_root TEXT")
            .unwrap();
        let fixed = root.path().join("downloaded-A");
        std::fs::create_dir_all(&fixed).unwrap();
        db.execute(
            "UPDATE store_models SET models_root=?1",
            [fixed.to_string_lossy().as_ref()],
        )
        .unwrap();
        let first = inventory_at(root.path()).unwrap();
        assert_eq!(first["models"][0]["models_root"], json!(fixed));
        assert_eq!(first, inventory_at(root.path()).unwrap());
        let other = root.path().join("downloaded-C");
        std::fs::create_dir_all(&other).unwrap();
        db.execute(
            "UPDATE store_models SET models_root=?1",
            [other.to_string_lossy().as_ref()],
        )
        .unwrap();
        let second = inventory_at(root.path()).unwrap();
        assert_eq!(first["selections"], second["selections"]);
        assert_eq!(second["models"][0]["models_root"], json!(other));
        assert_ne!(
            first["models"][0]["deployment_revision"],
            second["models"][0]["deployment_revision"]
        );
        db.execute("UPDATE store_models SET models_root=NULL", [])
            .unwrap();
        let legacy = inventory_at(root.path()).unwrap();
        assert!(legacy["models"][0]["models_root"].is_null());
        assert_eq!(legacy["selections"], second["selections"]);
        // The preceding v1 schema has no authoritative root. Keep it unknown;
        // an existing runtime remains observable and needs no script replay.
        let old_schema = versioned_fixture();
        assert!(inventory_at(old_schema.path()).unwrap()["models"][0]["models_root"].is_null());
    }

    #[test]
    fn cleanup_current_legacy_download_requires_positive_store_receipt() {
        let root = tempfile::tempdir().unwrap();
        let db = Connection::open(root.path().join("models.db")).unwrap();
        db.execute_batch("CREATE TABLE store_models(card_id TEXT,status TEXT,downloaded_at TEXT,card_source TEXT); INSERT INTO store_models VALUES('local','downloaded','today','local'),('incomplete','downloaded',NULL,'local'),('artifact','downloaded','today','artifact');").unwrap();
        let value = inventory_at(root.path()).unwrap();
        assert_eq!(value["selections"].as_array().unwrap().len(), 1);
        assert_eq!(value["selections"][0]["card"], "local");
        assert_eq!(value["models"], json!([]));
        assert_eq!(value["archived_models"], json!([]));
        assert!(
            value["skipped"]
                .as_array()
                .unwrap()
                .iter()
                .any(|row| row["card"] == "local" && row["reason"] == "current_not_deployed")
        );
        assert!(
            !root.path().join("cards").exists(),
            "a current receipt is distinct from directory discovery"
        );
    }

    #[test]
    fn cleanup_current_versioned_download_never_promotes_history_ready_or_deleting() {
        for state in ["current", "history", "ready", "deleting", "failed"] {
            let root = versioned_fixture();
            let db = Connection::open(root.path().join("models.db")).unwrap();
            db.execute("UPDATE store_models SET status='downloaded'", [])
                .unwrap();
            db.execute(
                "UPDATE model_versions SET state=?1 WHERE version_id='v2'",
                [state],
            )
            .unwrap();
            let value = inventory_at(root.path()).unwrap();
            assert_eq!(value["selections"][0]["card"], "qwen--v2");
            assert_eq!(value["models"], json!([]));
            assert_eq!(
                value["skipped"][0]["reason"] == "current_not_deployed",
                state == "current",
                "{state}"
            );
            assert!(
                value["archived_models"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .all(|row| row["card"] != "qwen--v2")
            );
            assert!(
                value["archived_models"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|row| row["card"] == "qwen--v3")
            );
        }
        for mutation in [
            "UPDATE model_versions SET runtime_card_id='other' WHERE version_id='v2'",
            "UPDATE model_versions SET card_id='other' WHERE version_id='v2'",
            "UPDATE store_models SET active_version_id='missing'",
            "INSERT INTO model_versions VALUES('qwen','v2','qwen--v2','history')",
            "UPDATE store_models SET active_version_id=NULL,runtime_card_id='qwen'",
        ] {
            let root = versioned_fixture();
            let db = Connection::open(root.path().join("models.db")).unwrap();
            db.execute("UPDATE store_models SET status='pending'", [])
                .unwrap();
            db.execute_batch(mutation).unwrap();
            if let Ok(value) = inventory_at(root.path()) {
                assert!(
                    value["skipped"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .all(|row| row["reason"] != "current_not_deployed"),
                    "{mutation}"
                );
                assert_eq!(value["models"], json!([]));
            }
        }
    }
}
