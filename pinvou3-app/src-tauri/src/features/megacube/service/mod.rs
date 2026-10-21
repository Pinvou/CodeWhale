//! cmsd component lifecycle, separate from model lifecycle. Desktop commands
//! submit durable jobs to a detached same-binary worker; no LLM tool controls it.
mod platform;

pub use platform::{bundled_package, inspect_package, logs, run_worker, submit};

/// Namespace frontend recovery receipts by profile and actual local endpoint.
/// This is a storage key, never an authorization token or device identity.
pub fn status() -> Result<serde_json::Value, String> {
    use sha2::{Digest, Sha256};
    let mut value = platform::status()?;
    let mut hash = Sha256::new();
    hash.update(
        crate::platform::paths::pinvou3_home()
            .as_os_str()
            .to_string_lossy()
            .as_bytes(),
    );
    hash.update([0]);
    hash.update(
        value
            .get("socketPath")
            .and_then(serde_json::Value::as_str)
            .unwrap_or("")
            .as_bytes(),
    );
    let key: String = hash
        .finalize()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect();
    value["recoveryScope"] = serde_json::Value::String(key);
    // A reviewed repair receipt can restore a handoff missed by older builds.
    // It is only an expected-binding intent: the frontend still verifies the
    // original daemon task/health, and the pool applies an atomic idle/CAS gate.
    value["switchFollowRecovery"] =
        recovery_receipts(value["recoveryScope"].as_str().unwrap_or(""));
    Ok(value)
}

fn recovery_receipts(scope: &str) -> serde_json::Value {
    let path = crate::platform::paths::pinvou3_home().join("megacube/switch-follow-recovery.json");
    read_recovery_receipts(&path, scope)
}

fn read_recovery_receipts(path: &std::path::Path, scope: &str) -> serde_json::Value {
    let empty = serde_json::json!([]);
    let Ok(metadata) = std::fs::symlink_metadata(&path) else {
        return empty;
    };
    if !metadata.is_file() || metadata.file_type().is_symlink() || metadata.len() > 65536 {
        return empty;
    }
    let Ok(bytes) = std::fs::read(path) else {
        return empty;
    };
    let Ok(value) = serde_json::from_slice::<serde_json::Value>(&bytes) else {
        return empty;
    };
    if value["version"] != 1 || value["recoveryScope"] != scope {
        return empty;
    }
    let Some(records) = value["pending"].as_array() else {
        return empty;
    };
    serde_json::Value::Array(
        records
            .iter()
            .take(50)
            .filter(|record| {
                [
                    record["task"]["id"].as_str(),
                    record["task"]["card"].as_str(),
                    record["sessionId"].as_str(),
                ]
                .into_iter()
                .all(|text| {
                    text.is_some_and(|text| {
                        !text.is_empty()
                            && text.len() <= 128
                            && text
                                .bytes()
                                .all(|byte| byte.is_ascii_alphanumeric() || b"-_.".contains(&byte))
                    })
                }) && record["task"]["kind"] == "switch"
                    && record["started"].as_u64().is_some()
                    && ["binding", "fallback"].iter().all(|field| {
                        record[*field].is_null()
                            || record[*field]
                                .as_str()
                                .is_some_and(|text| text.len() <= 128)
                    })
            })
            .cloned()
            .collect(),
    )
}

#[cfg(test)]
mod handoff_recovery_tests {
    use super::read_recovery_receipts;
    use serde_json::json;

    #[test]
    fn accepts_only_same_scope_bounded_switch_intents() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("receipt.json");
        let valid = json!({"task":{"id":"t-original", "card":"ornith35", "kind":"switch"},
            "sessionId":"original-chat", "binding":null, "fallback":"registered9", "started":1});
        let mut invalid = valid.clone();
        invalid["task"]["id"] = json!("../other");
        std::fs::write(
            &path,
            json!({"version":1, "recoveryScope":"scope-a",
            "pending":[valid.clone(), invalid]})
            .to_string(),
        )
        .unwrap();
        assert_eq!(read_recovery_receipts(&path, "scope-a"), json!([valid]));
        assert_eq!(read_recovery_receipts(&path, "scope-b"), json!([]));
        std::fs::write(&path, "invalid").unwrap();
        assert_eq!(read_recovery_receipts(&path, "scope-a"), json!([]));
    }

    #[test]
    fn rejects_oversize_or_nonregular_recovery_files() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("large.json");
        std::fs::write(&path, vec![b' '; 65537]).unwrap();
        assert_eq!(read_recovery_receipts(&path, "scope-a"), json!([]));
        assert_eq!(read_recovery_receipts(dir.path(), "scope-a"), json!([]));
    }
}
