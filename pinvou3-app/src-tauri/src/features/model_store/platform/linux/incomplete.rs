//! Discover failed installations from durable task facts, without inventing a
//! model-library row or deriving lifecycle authority from a directory or log.
use std::collections::BTreeMap;
use std::fs::OpenOptions;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::{config, db, lifecycle::ModelManager, script_runner};

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(super) struct Context {
    source: String,
    models_dir: String,
    staging_dir: String,
    hf_endpoint: String,
    store_url: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(super) struct RetryReference {
    pub original_run_id: String,
    pub preview_sha256: String,
}

impl Context {
    pub(super) fn capture(settings: &config::Settings, source: &str) -> Self {
        Self {
            source: source.into(),
            models_dir: settings.models_dir.clone(),
            staging_dir: settings.staging_dir.clone(),
            hf_endpoint: settings.hf_endpoint.clone(),
            store_url: (source == "store").then(|| settings.store_url.clone()),
        }
    }

    pub(super) fn test_mode(&self) -> bool {
        self.source == "test_store"
    }
}

#[derive(Deserialize, Serialize)]
struct Receipt {
    card_id: String,
    kind: String,
    created_at: String,
    outcome: Option<Result<Value, String>>,
    cancelled: bool,
    #[serde(default)]
    install_context: Option<Context>,
}

fn safe_id(id: &str) -> Result<(), String> {
    if id.is_empty() || id.len() > 160 || config::safe_path_component(id).is_err() {
        return Err("invalid_installation_reference".into());
    }
    Ok(())
}

fn read(id: &str) -> Result<Receipt, String> {
    safe_id(id)?;
    let path = config::data_dir().join("runs").join(format!("{id}.json"));
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|_| "installation_record_unavailable")?;
    let metadata = file
        .metadata()
        .map_err(|_| "installation_record_unavailable")?;
    // SAFETY: geteuid has no preconditions.
    let uid = unsafe { libc::geteuid() };
    if !metadata.is_file()
        || metadata.uid() != uid
        || metadata.nlink() != 1
        || metadata.mode() & 0o077 != 0
    {
        return Err("installation_record_untrusted".into());
    }
    // Receipt omits the producer's unbounded `lines` array. Stream over those
    // unused logs while retaining the exact private metadata used for retry;
    // a legitimate large log must not exhaust the recovery input budget.
    let receipt: Receipt = serde_json::from_reader(std::io::BufReader::new(file))
        .map_err(|_| "installation_record_corrupt")?;
    safe_id(&receipt.card_id)?;
    Ok(receipt)
}

fn latest() -> Result<BTreeMap<String, (String, Receipt)>, String> {
    latest_excluding(None, None)
}

fn latest_excluding(
    accepted_run: Option<&str>,
    card_id: Option<&str>,
) -> Result<BTreeMap<String, (String, Receipt)>, String> {
    let directory = config::data_dir().join("runs");
    let entries = match std::fs::read_dir(directory) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(BTreeMap::new()),
        Err(_) => return Err("installation_index_unavailable".into()),
    };
    let mut result: BTreeMap<String, (String, Receipt)> = BTreeMap::new();
    for entry in entries {
        let path = entry.map_err(|_| "installation_index_unavailable")?.path();
        if path.extension().and_then(|value| value.to_str()) != Some("json") {
            continue;
        }
        let id = path
            .file_stem()
            .and_then(|value| value.to_str())
            .ok_or("invalid_installation_reference")?;
        if accepted_run == Some(id) {
            continue;
        }
        let receipt = read(id)?;
        if card_id.is_some_and(|card| card != receipt.card_id) {
            continue;
        }
        // Stream immutable history one bounded receipt at a time. The bound
        // covers distinct current identities, never lifetime operation count.
        if !result.contains_key(&receipt.card_id) && result.len() >= 4096 {
            return Err("installation_index_limit: model identities need review".into());
        }
        let time = chrono::DateTime::parse_from_rfc3339(&receipt.created_at)
            .map_err(|_| "installation_record_corrupt")?;
        if let Some((_, previous)) = result.get(&receipt.card_id) {
            let previous_time = chrono::DateTime::parse_from_rfc3339(&previous.created_at)
                .map_err(|_| "installation_record_corrupt")?;
            if time < previous_time {
                continue;
            }
            if time == previous_time {
                return Err("installation_order_ambiguous".into());
            }
        }
        result.insert(receipt.card_id.clone(), (id.into(), receipt));
    }
    Ok(result)
}

fn failed_install(receipt: &Receipt) -> bool {
    matches!(receipt.kind.as_str(), "download" | "install_local")
        && !matches!(receipt.outcome, Some(Ok(_)))
}

pub(super) fn summaries() -> Result<Vec<Value>, String> {
    let mut items = Vec::new();
    for (_, (id, receipt)) in latest()? {
        if !failed_install(&receipt)
            || db::get_store_model(&receipt.card_id)
                .map_err(|error| error.to_string())?
                .is_some()
        {
            continue;
        }
        let terminal = receipt.outcome.is_some();
        let source_known = receipt.kind == "install_local" || receipt.install_context.is_some();
        let (error, error_truncated) = match receipt.outcome.and_then(Result::err) {
            Some(error) => {
                let (text, truncated) = super::log_page::summary(&error);
                (Some(text), truncated)
            }
            None => (None, false),
        };
        items.push(
            json!({"runId":id,"cardId":receipt.card_id,"kind":receipt.kind,
            "createdAt":receipt.created_at,"cancelled":receipt.cancelled,"terminal":terminal,
            "canPreview":terminal&&source_known,"legacyContext":receipt.install_context.is_none(),
            "error":error,"errorTruncated":error_truncated,"modelWritesGranted":false}),
        );
    }
    Ok(items)
}

pub(super) struct Preview {
    pub card_id: String,
    pub kind: String,
    pub context: Context,
    pub package_sha256: String,
    pub public: Value,
    receipt_sha256: String,
}

fn receipt_digest(receipt: &Receipt) -> Result<String, String> {
    serde_json::to_string(receipt)
        .map(|value| script_runner::sha256_hex(&value))
        .map_err(|error| error.to_string())
}

pub(super) fn recheck(
    run_id: &str,
    preview: &Preview,
    expected_sha256: &str,
) -> Result<(), String> {
    let receipt = read(run_id)?;
    if receipt_digest(&receipt)? != preview.receipt_sha256
        || preview.public["previewSha256"].as_str() != Some(expected_sha256)
    {
        return Err("installation_preview_changed".into());
    }
    if db::get_store_model(&preview.card_id)
        .map_err(|error| error.to_string())?
        .is_some()
    {
        return Err("installation_already_in_library".into());
    }
    super::orphans::ensure_run_quiet(run_id)?;
    Ok(())
}

fn candidate(run_id: &str, accepted_run: Option<&str>) -> Result<Receipt, String> {
    let receipt = read(run_id)?;
    if !failed_install(&receipt) || receipt.outcome.is_none() {
        return Err("installation_not_terminal".into());
    }
    if db::get_store_model(&receipt.card_id)
        .map_err(|error| error.to_string())?
        .is_some()
    {
        return Err("installation_already_in_library".into());
    }
    if latest_excluding(accepted_run, Some(&receipt.card_id))?
        .get(&receipt.card_id)
        .map(|(id, _)| id.as_str())
        != Some(run_id)
    {
        return Err("installation_superseded".into());
    }
    super::maintenance::ensure_clear(&receipt.card_id)?;
    super::orphans::ensure_run_quiet(run_id)?;
    Ok(receipt)
}

fn context(receipt: &Receipt) -> Result<Context, String> {
    let source = if receipt.kind == "install_local" {
        "local"
    } else {
        receipt
            .install_context
            .as_ref()
            .map(|context| context.source.as_str())
            .ok_or("installation_original_source_unknown")?
    };
    if !["local", "store", "test_store"].contains(&source) {
        return Err("installation_original_source_unknown".into());
    }
    let context = Context::capture(&config::load_settings(), source);
    if receipt
        .install_context
        .as_ref()
        .is_some_and(|original| original != &context)
    {
        return Err("installation_storage_or_source_changed".into());
    }
    Ok(context)
}

pub(super) fn retry_request(run_id: &str) -> Result<(String, String, Context), String> {
    let receipt = candidate(run_id, None)?;
    let context = context(&receipt)?;
    Ok((receipt.card_id, receipt.kind, context))
}

pub(super) fn request_identity(run_id: &str) -> Result<(String, String, Context), String> {
    let receipt = read(run_id)?;
    if !failed_install(&receipt) || receipt.outcome.is_none() {
        return Err("installation_not_terminal".into());
    }
    let context = context(&receipt)?;
    Ok((receipt.card_id, receipt.kind, context))
}

pub(super) fn preview(run_id: &str) -> Result<Preview, String> {
    preview_for_retry(run_id, None)
}

pub(super) fn preview_for_retry(
    run_id: &str,
    accepted_run: Option<&str>,
) -> Result<Preview, String> {
    let receipt = candidate(run_id, accepted_run)?;
    let receipt_sha256 = receipt_digest(&receipt)?;
    let context = context(&receipt)?;
    let manager = if context.test_mode() {
        ModelManager::new_test_store()
    } else {
        ModelManager::new(None, None)
    };
    let (name, package_sha256) = if receipt.kind == "install_local" {
        ModelManager::preview_local_install(&receipt.card_id)
    } else {
        manager.preview_store_install(&receipt.card_id)
    }
    .map_err(|error| error.to_string())?;
    let stamp = json!({"run_id":run_id,"receipt_sha256":receipt_sha256,"card_id":receipt.card_id,"kind":receipt.kind,"context":context,"package_sha256":package_sha256});
    let digest = script_runner::sha256_hex(
        &serde_json::to_string(&stamp).map_err(|error| error.to_string())?,
    );
    let public = json!({"runId":run_id,"cardId":receipt.card_id,"kind":receipt.kind,"cardName":name,
        "previewSha256":digest,"packageSha256":package_sha256,"legacyContext":receipt.install_context.is_none(),
        "modelsDir":context.models_dir,"stagingDir":context.staging_dir,"modelWritesGranted":false});
    Ok(Preview {
        card_id: receipt.card_id,
        kind: receipt.kind,
        context,
        package_sha256,
        public,
        receipt_sha256,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    fn receipt(id: &str, kind: &str, time: &str, outcome: Value) {
        let value = json!({"card_id":"local-fixture","kind":kind,"created_at":time,"outcome":outcome,"cancelled":false,"lines":[]});
        super::super::execution_fence::private_write(
            &config::data_dir().join("runs").join(format!("{id}.json")),
            &serde_json::to_vec(&value).unwrap(),
        )
        .unwrap();
    }

    fn local_package() {
        let dir = config::data_dir().join("cards/local-fixture");
        std::fs::create_dir_all(&dir).unwrap();
        for script in [
            "install_engine",
            "download_model",
            "deploy",
            "enable",
            "stop",
            "uninstall",
        ] {
            std::fs::write(dir.join(format!("{script}.sh")), "#!/bin/bash\nexit 0\n").unwrap();
        }
        std::fs::write(dir.join("common.sh"), "# fixture\n").unwrap();
        std::fs::write(
            dir.join("card.json"),
            r#"{"name":"Local fixture","engine_name":"test-engine"}"#,
        )
        .unwrap();
    }

    #[test]
    fn terminal_failures_without_a_row_remain_visible_and_do_not_resurface_after_successful_uninstall()
     {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        receipt(
            "failure",
            "install_local",
            "2026-10-18T00:00:00Z",
            json!({"Err":"fixture failure"}),
        );
        let items = summaries().unwrap();
        assert_eq!(items.len(), 1);
        assert_eq!(items[0]["modelWritesGranted"], false);
        receipt(
            "later",
            "uninstall",
            "2026-10-18T00:00:01Z",
            json!({"Ok":{}}),
        );
        assert!(summaries().unwrap().is_empty());
        assert!(preview("failure").err().unwrap().contains("superseded"));
    }

    #[test]
    fn preview_pins_current_package_and_context_but_never_imports_runtime_as_package() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        local_package();
        receipt(
            "failure",
            "install_local",
            "2026-10-18T00:00:00Z",
            json!({"Err":"fixture failure"}),
        );
        let first = preview("failure").unwrap();
        let directory = config::data_dir().join("cards/local-fixture");
        std::fs::create_dir_all(directory.join(".venv")).unwrap();
        std::fs::write(directory.join(".venv/runtime"), "keep runtime").unwrap();
        assert_eq!(
            first.public["previewSha256"],
            preview("failure").unwrap().public["previewSha256"]
        );
        std::fs::write(directory.join("deploy.sh"), "#!/bin/bash\nexit 42\n").unwrap();
        assert_ne!(
            first.public["previewSha256"],
            preview("failure").unwrap().public["previewSha256"]
        );
        assert_eq!(db::list_store_models().unwrap().len(), 0);
    }

    #[test]
    fn unknown_store_source_active_records_and_untrusted_receipts_cannot_resume() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        receipt(
            "download",
            "download",
            "2026-10-18T00:00:00Z",
            json!({"Err":"fixture failure"}),
        );
        assert!(!summaries().unwrap()[0]["canPreview"].as_bool().unwrap());
        assert!(
            preview("download")
                .err()
                .unwrap()
                .contains("source_unknown")
        );
        receipt(
            "running",
            "install_local",
            "2026-10-18T00:00:01Z",
            Value::Null,
        );
        assert!(preview("running").err().unwrap().contains("not_terminal"));
        let path = config::data_dir().join("runs/running.json");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert!(read("running").err().unwrap().contains("untrusted"));
        assert!(read("../running").is_err());
        std::fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink(config::data_dir().join("runs/download.json"), path).unwrap();
        assert!(read("running").is_err());
    }

    #[test]
    fn completed_history_does_not_exhaust_latest_installation_recovery() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        local_package();
        for i in 0..5000 {
            receipt(
                &format!("old-{i}"),
                "stop",
                &chrono::DateTime::from_timestamp(1_577_836_800 + i, 0)
                    .unwrap()
                    .to_rfc3339(),
                json!({"Ok":null}),
            );
        }
        receipt(
            "unique-failure",
            "install_local",
            "2026-10-21T00:00:00Z",
            json!({"Err":"unique recovery proof"}),
        );
        assert_eq!(summaries().unwrap()[0]["runId"], "unique-failure");
        assert!(preview("unique-failure").is_ok());
        assert!(read("old-0").is_ok(), "old IDs remain on disk");
        receipt(
            "later-uninstall",
            "uninstall",
            "2026-10-21T00:00:01Z",
            json!({"Ok":null}),
        );
        assert!(summaries().unwrap().is_empty());
        assert!(
            preview("unique-failure")
                .err()
                .unwrap()
                .contains("superseded")
        );
    }

    #[test]
    fn streaming_receipts_still_reject_invalid_json_and_shared_files() {
        let _home = config::TempHome::new();
        receipt(
            "private",
            "install_local",
            "2026-10-21T00:00:00Z",
            json!({"Err":"fixture"}),
        );
        let path = config::data_dir().join("runs/private.json");
        std::fs::write(&path, b"{").unwrap();
        assert!(read("private").err().unwrap().contains("corrupt"));
        receipt(
            "private",
            "install_local",
            "2026-10-21T00:00:00Z",
            json!({"Err":"fixture"}),
        );
        let linked = config::data_dir().join("runs/shared.json");
        std::fs::hard_link(&path, &linked).unwrap();
        assert!(read("private").err().unwrap().contains("untrusted"));
        std::fs::remove_file(linked).unwrap();
        assert!(read("private").is_ok());
    }
}
