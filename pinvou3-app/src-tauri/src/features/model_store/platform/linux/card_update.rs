//! Pure validation for explicit script-package updates (no execution or disk writes).
use std::collections::{BTreeMap, BTreeSet};

const ENTRIES: &[&str] = &[
    "install_engine",
    "download_model",
    "deploy",
    "enable",
    "stop",
    "uninstall",
    "uninstall_engine",
];

pub fn validate_inventory(
    scripts: &BTreeMap<String, String>,
    script_hashes: &BTreeMap<String, String>,
    assets: &BTreeMap<String, String>,
    asset_hashes: &BTreeMap<String, String>,
) -> Result<(), String> {
    for required in &ENTRIES[..6] {
        if !scripts.contains_key(*required) {
            return Err(format!("卡片更新包缺少 {required}，已保留原版本。"));
        }
    }
    let mut paths = BTreeSet::new();
    for (key, url) in scripts {
        if !ENTRIES.contains(&key.as_str()) {
            return Err(format!("卡片更新包包含未知入口 {key}。"));
        }
        checked_hash(key, url, script_hashes)?;
        paths.insert(format!("{key}.sh"));
    }
    if scripts.len() != script_hashes.len() || assets.len() != asset_hashes.len() {
        return Err("卡片文件清单与校验清单不一致，已保留原版本。".into());
    }
    for (path, url) in assets {
        checked_hash(path, url, asset_hashes)?;
        let components: Vec<_> = path.split('/').collect();
        if path.contains(['\\', ':'])
            || components
                .iter()
                .any(|part| part.is_empty() || part.starts_with('.'))
            || !paths.insert(path.clone())
        {
            return Err("卡片附属文件路径无效或覆盖了入口/运行状态，已保留原版本。".into());
        }
    }
    for path in &paths {
        for (index, _) in path.match_indices('/') {
            if paths.contains(&path[..index]) {
                return Err("卡片文件与目录重名，已保留原版本。".into());
            }
        }
    }
    Ok(())
}

fn checked_hash(key: &str, url: &str, hashes: &BTreeMap<String, String>) -> Result<(), String> {
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
}

#[cfg(test)]
mod tests {
    use super::*;
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
