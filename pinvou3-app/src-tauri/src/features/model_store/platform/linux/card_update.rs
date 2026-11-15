//! Explicit card updates: compare complete packages without executing their contents.
use std::collections::{BTreeMap, BTreeSet};

use serde_json::{Value, json};

use super::{db, script_runner, store_client::StoreCardDetail};

const ENTRIES: &[&str] = &[
    "install_engine",
    "download_model",
    "deploy",
    "enable",
    "stop",
    "uninstall",
    "uninstall_engine",
];

#[derive(Debug, Clone, serde::Serialize)]
pub struct Compatibility {
    pub kind: String,
    pub reason: String,
    pub can_update: bool,
    pub can_stage: bool,
    pub changes: Vec<Value>,
    pub current_weights: Option<super::weight_identity::WeightFiles>,
    pub next_weights: Option<super::weight_identity::WeightFiles>,
}

/// Content evidence takes precedence over mutable catalogue labels. Topology is
/// independently checked because identical bytes do not imply the same devices.
pub fn compatibility(
    rec: &db::StoreModelRecord,
    card: &StoreCardDetail,
    remote: Option<&db::ScriptBlob>,
) -> Compatibility {
    use super::weight_identity::{self as weights, MANIFEST};
    let optional = |s: Option<&str>| s.unwrap_or_default().trim().to_string();
    let quant = |s: Option<&str>| {
        optional(s)
            .chars()
            .filter(char::is_ascii_alphanumeric)
            .collect::<String>()
            .to_ascii_lowercase()
    };
    let mut result = Compatibility {
        kind: "unverified".into(),
        reason: "missing_weight_evidence".into(),
        can_update: false,
        can_stage: false,
        changes: Vec::new(),
        current_weights: None,
        next_weights: None,
    };
    for (field, current, next) in [
        (
            "source_repo_id",
            json!(rec.source_repo_id),
            json!(card.source_repo_id),
        ),
        (
            "source_filename",
            json!(rec.source_filename),
            json!(card.source_filename),
        ),
        (
            "quantization",
            json!(rec.quantization),
            json!(card.summary.quantization),
        ),
        (
            "size_bytes",
            json!(rec.size_bytes),
            json!(card.summary.size_bytes),
        ),
        (
            "deploy_topology",
            json!(rec.deploy_topology),
            json!(card.summary.deploy_topology),
        ),
    ] {
        if current != next {
            result
                .changes
                .push(json!({"field":field,"current":current,"next":next}));
        }
    }
    if rec.card_source == "builtin" || rec.card_id != card.summary.id {
        result.kind = "unsupported".into();
        result.reason = "card_identity".into();
        return result;
    }
    if rec.deploy_topology != card.summary.deploy_topology {
        result.kind = "topology_changed".into();
        result.reason = "topology_changed".into();
        return result;
    }
    let local = rec.assets_content.get(MANIFEST);
    if let Some(blob) = local {
        match weights::verified(blob, &blob.sha256) {
            Ok(files) => result.current_weights = Some(files),
            Err(reason) => {
                result.reason = reason;
                return result;
            }
        }
    }
    if let Some(hash) = card.asset_sha256.as_ref().and_then(|h| h.get(MANIFEST)) {
        let evidence =
            remote.or_else(|| local.filter(|blob| blob.sha256.eq_ignore_ascii_case(hash)));
        if let Some(blob) = evidence {
            match weights::verified(blob, hash) {
                Ok(files) => result.next_weights = Some(files),
                Err(reason) => {
                    result.reason = reason;
                    return result;
                }
            }
        }
    }
    let metadata_same = optional(rec.source_repo_id.as_deref())
        == optional(card.source_repo_id.as_deref())
        && rec.source_filename.as_deref().unwrap_or_default()
            == card.source_filename.as_deref().unwrap_or_default()
        && quant(rec.quantization.as_deref()) == quant(card.summary.quantization.as_deref())
        && (rec.size_bytes <= 0
            || card.summary.size_bytes == 0
            || rec.size_bytes as u64 == card.summary.size_bytes);
    match (&result.current_weights, &result.next_weights) {
        (Some(old), Some(new)) if old == new => {
            let next_name = card
                .source_filename
                .as_deref()
                .filter(|s| !s.trim().is_empty());
            if let Some(name) = next_name {
                let Ok(name) = weights::normalize_path(name) else {
                    result.reason = "invalid_weight_path".into();
                    return result;
                };
                if !new.contains_key(&name) {
                    result.reason = "filename_not_in_manifest".into();
                    return result;
                }
                if let Some(old_name) = rec
                    .source_filename
                    .as_deref()
                    .and_then(|p| weights::normalize_path(p).ok())
                {
                    if old_name != name && old.contains_key(&old_name) {
                        result.reason = "artifact_selection_changed".into();
                        return result;
                    }
                } else if old.len() != 1 {
                    result.reason = "artifact_selection_changed".into();
                    return result;
                }
            } else if old.len() != 1
                && rec.source_filename.as_deref().is_some_and(|name| {
                    weights::normalize_path(name).is_ok_and(|name| old.contains_key(&name))
                })
            {
                result.reason = "artifact_selection_changed".into();
                return result;
            }
            result.kind = "compatible".into();
            result.reason = "verified_weights".into();
            result.can_update = true;
        }
        (Some(_), Some(_)) => {
            result.kind = "weights_changed".into();
            result.reason = "weights_changed".into();
        }
        (None, None) if local.is_none() && !card.assets.contains_key(MANIFEST) && metadata_same => {
            result.kind = "compatible".into();
            result.reason = "unchanged_legacy_identity".into();
            result.can_update = true;
        }
        _ => {}
    }
    result.can_stage = result.current_weights.is_some()
        && !result.can_update
        && result.next_weights.is_some()
        && card.summary.deploy_topology == super::store_client::DeployTopology::Single
        && result
            .current_weights
            .as_ref()
            .is_some_and(|files| files.len() == 1)
        && result
            .next_weights
            .as_ref()
            .is_some_and(|files| files.len() == 1)
        && card.assets.contains_key("native_engine_process.py")
        && card.source_filename.is_some();
    // The destination needs our supported isolated native layout. The old
    // deployment only needs a verified single artifact and its complete offline
    // recipe: legacy native cards legitimately use shell process management.
    // Requiring a helper introduced by the new card would block that migration.
    if !result.can_update
        && result.current_weights.is_some()
        && result.next_weights.is_some()
        && !result.can_stage
    {
        result.reason = "versioning_unsupported".into();
    }
    if result.can_stage {
        let selected = card
            .source_filename
            .as_deref()
            .and_then(|name| weights::normalize_path(name).ok());
        if !selected.is_some_and(|name| {
            result
                .next_weights
                .as_ref()
                .is_some_and(|files| files.contains_key(&name))
        }) {
            result.can_stage = false;
            result.reason = "filename_not_in_manifest".into();
        }
    }
    if result.can_stage
        && ENTRIES[..6]
            .iter()
            .any(|key| !rec.scripts_content.contains_key(*key))
    {
        result.can_stage = false;
        result.reason = "missing_cached_recipe".into();
    }
    if result.can_stage
        && rec
            .scripts_content
            .values()
            .chain(rec.assets_content.values())
            .any(|blob| {
                !script_runner::sha256_hex(&blob.content).eq_ignore_ascii_case(&blob.sha256)
            })
    {
        result.can_stage = false;
        result.reason = "cached_recipe_integrity".into();
    }
    result
}

pub fn validate(rec: &db::StoreModelRecord, card: &StoreCardDetail) -> Result<(), String> {
    validate_inventory(
        &card.scripts,
        card.script_sha256.as_ref().unwrap_or(&BTreeMap::new()),
        &card.assets,
        card.asset_sha256.as_ref().unwrap_or(&BTreeMap::new()),
    )?;
    if compatibility(rec, card, None).can_update {
        Ok(())
    } else {
        Err("当前权重无法确认与新版一致，请先检查版本差异；现有部署已保留。".into())
    }
}

// Validate before fetching: a complete package must have independently supplied
// digests, and assets must never shadow entrypoints or runtime state.
pub fn validate_inventory(
    scripts: &BTreeMap<String, String>,
    script_hashes: &BTreeMap<String, String>,
    assets: &BTreeMap<String, String>,
    asset_hashes: &BTreeMap<String, String>,
) -> Result<(), String> {
    if ENTRIES[..6].iter().any(|key| !scripts.contains_key(*key))
        || scripts.len() != script_hashes.len()
        || assets.len() != asset_hashes.len()
    {
        return Err("卡片更新包的入口或校验清单不完整，已保留原版本。".into());
    }
    let checked_hash = |key: &str, url: &str, hashes: &BTreeMap<String, String>| {
        let hash = hashes.get(key).map(String::as_str).unwrap_or_default();
        if !url.starts_with("https://")
            || hash.len() != 64
            || !hash.bytes().all(|b| b.is_ascii_hexdigit())
        {
            return Err(format!(
                "卡片文件 {key} 缺少安全地址或有效校验值，已保留原版本。"
            ));
        }
        Ok(())
    };
    let mut paths = BTreeSet::new();
    for (key, url) in scripts {
        if !ENTRIES.contains(&key.as_str()) {
            return Err(format!("卡片更新包包含未知入口 {key}。"));
        }
        checked_hash(key, url, script_hashes)?;
        paths.insert(format!("{key}.sh"));
    }
    for (path, url) in assets {
        checked_hash(path, url, asset_hashes)?;
        if path.trim() != path
            || path.chars().any(char::is_control)
            || path.contains(['\\', ':'])
            || path
                .split('/')
                .any(|part| part.is_empty() || part.starts_with('.'))
            || !paths.insert(path.clone())
        {
            return Err("卡片附属文件路径无效或覆盖了入口/运行状态，已保留原版本。".into());
        }
    }
    for path in &paths {
        if path
            .match_indices('/')
            .any(|(i, _)| paths.contains(&path[..i]))
        {
            return Err("卡片文件与目录重名，已保留原版本。".into());
        }
    }
    Ok(())
}

fn local_digest(path: &str, blob: &db::ScriptBlob) -> String {
    let actual = script_runner::sha256_hex(&blob.content);
    if !actual.eq_ignore_ascii_case(&blob.sha256) {
        format!("invalid:{}:{actual}", blob.sha256.to_ascii_lowercase())
    } else {
        super::native_card_compat::upstream_digest(path, &actual)
    }
}

pub fn local_manifest(rec: &db::StoreModelRecord) -> Value {
    local_manifest_with(rec, local_digest)
}

/// A reminder is a metadata comparison, never permission to execute a package.
/// Avoid rehashing cached source trees during a background startup check.
pub fn notice_manifest(rec: &db::StoreModelRecord) -> Value {
    local_manifest_with(rec, |path, blob| {
        super::native_card_compat::upstream_digest(path, &blob.sha256)
    })
}

fn local_manifest_with(
    rec: &db::StoreModelRecord,
    digest: impl Fn(&str, &db::ScriptBlob) -> String,
) -> Value {
    json!({
        "name": rec.card_name, "org": rec.org, "description": rec.description,
        "engine_name": rec.engine_name, "engine_version": rec.engine_version,
        "size_bytes": rec.size_bytes, "quantization": rec.quantization,
        "source_repo_id": rec.source_repo_id, "source_filename": rec.source_filename,
        "profiles": rec.profiles, "deploy_topology": rec.deploy_topology,
        "scripts": rec.scripts_content.iter().map(|(k, v)| (k.clone(), digest(k,v))).collect::<BTreeMap<_, _>>(),
        "assets": rec.assets_content.iter().map(|(k, v)| (k.clone(), digest(k,v))).collect::<BTreeMap<_, _>>(),
    })
}

pub fn remote_manifest(card: &StoreCardDetail) -> Value {
    json!({
        "name": card.summary.name, "org": card.summary.org, "description": card.summary.description,
        "engine_name": card.summary.engine_name, "engine_version": card.summary.engine_version,
        "size_bytes": card.summary.size_bytes, "quantization": card.summary.quantization,
        "source_repo_id": card.source_repo_id, "source_filename": card.source_filename,
        "profiles": card.profiles, "deploy_topology": card.summary.deploy_topology,
        "scripts": normalized_hashes(card.script_sha256.as_ref()),
        "assets": normalized_hashes(card.asset_sha256.as_ref()),
    })
}

fn normalized_hashes(hashes: Option<&BTreeMap<String, String>>) -> BTreeMap<String, String> {
    hashes
        .into_iter()
        .flat_map(|map| map.iter())
        .map(|(path, hash)| {
            (
                path.clone(),
                super::native_card_compat::upstream_digest(path, hash),
            )
        })
        .collect()
}

pub fn revision(manifest: &Value) -> String {
    script_runner::sha256_hex(&manifest.to_string())
}

pub fn preview(rec: &db::StoreModelRecord, card: &StoreCardDetail) -> Value {
    let remote = remote_manifest(card);
    json!({
        "card_id": rec.card_id, "card_name": rec.card_name,
        "changed": local_manifest(rec) != remote, "revision": revision(&remote),
        "running": rec.running,
        "current_engine": rec.engine_name, "current_version": rec.engine_version,
        "next_engine": card.summary.engine_name, "next_version": card.summary.engine_version,
        "description": card.summary.description,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn audited_controller_repairs_are_the_same_catalogue_revision() {
        use super::super::native_card_compat::{BROKEN, FIXED, LAUNCH_FIXED};
        for hash in [BROKEN, LAUNCH_FIXED, FIXED] {
            let hashes = BTreeMap::from([("native_engine_process.py".into(), hash.into())]);
            assert_eq!(
                normalized_hashes(Some(&hashes))["native_engine_process.py"],
                BROKEN
            );
        }
        let hashes = BTreeMap::from([("other.py".into(), FIXED.into())]);
        assert_eq!(normalized_hashes(Some(&hashes))["other.py"], FIXED);
    }
    fn package() -> (BTreeMap<String, String>, BTreeMap<String, String>) {
        let scripts = ENTRIES[..6]
            .iter()
            .map(|key| (key.to_string(), "https://example.test/file".into()))
            .collect();
        let hashes = ENTRIES[..6]
            .iter()
            .map(|key| (key.to_string(), "a".repeat(64)))
            .collect();
        (scripts, hashes)
    }
    #[test]
    fn complete_hashed_package_is_accepted() {
        let (scripts, hashes) = package();
        assert!(validate_inventory(&scripts, &hashes, &BTreeMap::new(), &BTreeMap::new()).is_ok());
    }
    #[test]
    fn missing_entry_or_hash_is_rejected() {
        let (mut scripts, mut hashes) = package();
        hashes.remove("stop");
        assert!(validate_inventory(&scripts, &hashes, &BTreeMap::new(), &BTreeMap::new()).is_err());
        scripts.remove("stop");
        assert!(validate_inventory(&scripts, &hashes, &BTreeMap::new(), &BTreeMap::new()).is_err());
    }
    #[test]
    fn asset_cannot_replace_runtime_or_entry() {
        let (scripts, hashes) = package();
        for path in [
            "../common.sh",
            "/tmp/x",
            ".runtime/serve.pid",
            ".deploy-env",
            " .deploy-env",
            " .runtime/serve.identity.json",
            "enable.sh ",
            "helper.py\n",
            "a/\tb",
            "enable.sh",
            "a\\b",
            "a//b",
            "a/./b",
            "C:/x",
        ] {
            let assets = BTreeMap::from([(path.into(), "https://example.test/file".into())]);
            let ah = BTreeMap::from([(path.into(), "b".repeat(64))]);
            assert!(
                validate_inventory(&scripts, &hashes, &assets, &ah).is_err(),
                "{path}"
            );
        }
    }
    #[test]
    fn file_directory_collision_is_rejected() {
        let (scripts, hashes) = package();
        let assets: BTreeMap<String, String> = BTreeMap::from([
            ("a".into(), "https://example.test/file".into()),
            ("a/b".into(), "https://example.test/file".into()),
        ]);
        let ah = assets.keys().map(|k| (k.clone(), "b".repeat(64))).collect();
        assert!(validate_inventory(&scripts, &hashes, &assets, &ah).is_err());
    }
    #[test]
    fn insecure_source_and_invalid_digest_are_rejected() {
        let (mut scripts, mut hashes) = package();
        scripts.insert("enable".into(), "http://example.test/file".into());
        assert!(validate_inventory(&scripts, &hashes, &BTreeMap::new(), &BTreeMap::new()).is_err());
        scripts.insert("enable".into(), "https://example.test/file".into());
        hashes.insert("enable".into(), "x".repeat(64));
        assert!(validate_inventory(&scripts, &hashes, &BTreeMap::new(), &BTreeMap::new()).is_err());
    }
}
