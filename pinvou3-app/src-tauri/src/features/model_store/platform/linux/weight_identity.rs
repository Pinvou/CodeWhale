//! Weight identity is the verified file inventory, independent of display metadata.
use std::collections::BTreeMap;

use super::{db::ScriptBlob, script_runner};

pub const MANIFEST: &str = "weights.sha256";
pub type WeightFiles = BTreeMap<String, String>;

pub fn normalize_path(path: &str) -> Result<String, String> {
    let path = path.trim_start_matches("./");
    if path.is_empty()
        || path.contains(['\\', ':'])
        || path.chars().any(char::is_control)
        || path
            .split('/')
            .any(|part| part.is_empty() || part == "." || part == "..")
    {
        return Err("invalid_weight_path".into());
    }
    Ok(path.into())
}

pub fn parse(content: &str) -> Result<WeightFiles, String> {
    if content.len() > 1024 * 1024 {
        return Err("invalid_weight_manifest".into());
    }
    let mut files = BTreeMap::new();
    for line in content
        .trim_start_matches('\u{feff}')
        .lines()
        .filter(|line| !line.trim().is_empty() && !line.trim_start().starts_with('#'))
    {
        let (hash, path) = line
            .split_once("  ")
            .or_else(|| line.split_once(" *"))
            .ok_or("invalid_weight_manifest")?;
        if hash.len() != 64 || !hash.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Err("invalid_weight_manifest".into());
        }
        let path = normalize_path(path)?;
        if files.insert(path, hash.to_ascii_lowercase()).is_some() {
            return Err("duplicate_weight_path".into());
        }
    }
    if files.is_empty() {
        return Err("invalid_weight_manifest".into());
    }
    Ok(files)
}

pub fn verified(blob: &ScriptBlob, expected: &str) -> Result<WeightFiles, String> {
    if !blob.sha256.eq_ignore_ascii_case(expected)
        || !script_runner::sha256_hex(&blob.content).eq_ignore_ascii_case(expected)
    {
        return Err("weight_manifest_integrity".into());
    }
    parse(&blob.content)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn equivalent_manifests_ignore_order_line_endings_markers_and_digest_case() {
        let a = parse(&format!(
            "{}  ./a\r\n{} *dir/b\r\n",
            "A".repeat(64),
            "b".repeat(64)
        ))
        .unwrap();
        let b = parse(&format!(
            "{}  dir/b\n{}  a\n",
            "b".repeat(64),
            "a".repeat(64)
        ))
        .unwrap();
        assert_eq!(a, b);
        assert_eq!(
            a,
            parse(&format!(
                "\u{feff}# generated\n{}  a\n{}  dir/b\n",
                "a".repeat(64),
                "b".repeat(64)
            ))
            .unwrap()
        );
    }
    #[test]
    fn unsafe_duplicate_empty_and_malformed_inventories_are_rejected() {
        for path in [
            "/absolute",
            "../escape",
            "a/../escape",
            "a\\b",
            "a//b",
            "C:/x",
            "",
        ] {
            assert!(
                parse(&format!("{}  {path}\n", "a".repeat(64))).is_err(),
                "{path}"
            );
        }
        assert!(parse("").is_err());
        assert!(parse("not-a-digest  file").is_err());
        assert!(parse(&format!("{}  a\n{}  ./a\n", "a".repeat(64), "b".repeat(64))).is_err());
    }
}
