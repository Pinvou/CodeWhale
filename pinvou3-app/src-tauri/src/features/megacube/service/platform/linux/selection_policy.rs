//! Component compatibility and the persistent config sentinel for current-only
//! model selection. Preparing a migration never mutates the running service.
use std::io::Read;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::Path;

pub(super) fn verify_component(info: &serde_json::Value) -> Result<(), String> {
    if info["store_selection"] != 1 {
        return Err("component_store_selection_unsupported".into());
    }
    if info["native_profile_from_card"] != 1 {
        return Err("component_native_profile_unsupported".into());
    }
    Ok(())
}

/// Candidate capability is checked while the original service is still running.
/// Legacy full components omit this optional restriction; unknown new scopes fail closed.
pub(super) fn verify_state_compatibility(
    info: &serde_json::Value,
    state: &Path,
) -> Result<(), String> {
    match info.get("authority_scope") {
        None => return Ok(()),
        Some(value) if value == "local-v1" => {}
        _ => return Err("component_authority_scope_unsupported".into()),
    }
    for name in [
        "identity-v2",
        "remote-v2",
        "control-v2",
        "control-v2.initialized.json",
    ] {
        match std::fs::symlink_metadata(state.join(name)) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            _ => return Err("component_authority_state_unsupported".into()),
        }
    }
    Ok(())
}

pub(super) struct PreparedConfig {
    original: String,
    merged: String,
}
impl PreparedConfig {
    pub(super) fn needs_update(&self) -> bool {
        self.original != self.merged
    }

    pub(super) fn revision_pair(&self) -> (String, String) {
        use sha2::{Digest, Sha256};
        let digest = |text: &str| {
            Sha256::digest(text.as_bytes())
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<String>()
        };
        (digest(&self.original), digest(&self.merged))
    }

    pub(super) fn plan_receipt(&self, record: &super::Record) -> Result<super::Record, String> {
        let (original, merged) = self.revision_pair();
        super::binding::plan_config_revision(record, &original, &merged)
    }
}

fn read_config(path: &Path) -> Result<String, String> {
    for part in path.ancestors() {
        if std::fs::symlink_metadata(part).is_ok_and(|m| m.file_type().is_symlink()) {
            return Err("unverified_service_config".into());
        }
    }
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|_| "unverified_service_config")?;
    let meta = file.metadata().map_err(|_| "unverified_service_config")?;
    // SAFETY: geteuid has no preconditions and only reads the effective identity.
    let uid = unsafe { libc::geteuid() };
    if !meta.is_file() || meta.uid() != uid || meta.len() > 512 * 1024 || meta.mode() & 0o022 != 0 {
        return Err("unverified_service_config".into());
    }
    let mut text = String::new();
    Read::by_ref(&mut file)
        .take(512 * 1024 + 1)
        .read_to_string(&mut text)
        .map_err(|_| "unverified_service_config")?;
    if text.len() > 512 * 1024 {
        return Err("unverified_service_config".into());
    }
    Ok(text)
}

pub(super) fn prepare_config(path: &Path) -> Result<PreparedConfig, String> {
    let original = read_config(path)?;
    let value: toml::Value = toml::from_str(&original).map_err(|_| "unverified_service_config")?;
    let table = value.as_table().ok_or("unverified_service_config")?;
    match table.get("require_store_selection") {
        Some(v) if v.as_str() == Some("v1") => {
            return Ok(PreparedConfig {
                merged: original.clone(),
                original,
            });
        }
        Some(_) => return Err("unverified_service_config".into()),
        None => {}
    }

    // Prepending a root key preserves comments, tables and operator formatting.
    let merged = format!("require_store_selection = \"v1\"\n{original}");
    Ok(PreparedConfig { original, merged })
}

/// Called only after the candidate was verified and the maintenance stop
/// completed; never rewrite a config changed since that preflight.
pub(super) fn apply_config(path: &Path, prepared: &PreparedConfig) -> Result<bool, String> {
    if read_config(path)? != prepared.original {
        return Err("service_configuration_changed".into());
    }
    if prepared.original == prepared.merged {
        return Ok(false);
    }
    super::write_bytes(path, prepared.merged.as_bytes())?;
    Ok(true)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;
    #[test]
    fn legacy_components_cannot_restore_directory_discovery_or_fixed_native_context() {
        assert_eq!(
            verify_component(&serde_json::json!({"store_adoption":1})).unwrap_err(),
            "component_store_selection_unsupported"
        );
        assert_eq!(
            verify_component(&serde_json::json!({"store_selection":1})).unwrap_err(),
            "component_native_profile_unsupported"
        );
        assert!(
            verify_component(
                &serde_json::json!({"store_selection":1,"native_profile_from_card":1})
            )
            .is_ok()
        );
    }
    #[test]
    fn config_preflight_is_read_only_and_atomic_merge_preserves_operator_settings() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("cmsd.toml");
        let text = "gateway_listen='127.0.0.1:8000'\nreserve_gib=8\n";
        std::fs::write(&path, text).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        let prepared = prepare_config(&path).unwrap();
        assert_eq!(std::fs::read_to_string(&path).unwrap(), text);
        assert!(apply_config(&path, &prepared).unwrap());
        assert!(std::fs::read_to_string(&path).unwrap().ends_with(text));
        let value: toml::Value = toml::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        assert_eq!(value["gateway_listen"].as_str(), Some("127.0.0.1:8000"));
        assert_eq!(value["reserve_gib"].as_integer(), Some(8));
        assert_eq!(value["require_store_selection"].as_str(), Some("v1"));
        let same = prepare_config(&path).unwrap();
        assert!(!apply_config(&path, &same).unwrap());
    }
    #[test]
    fn unknown_policy_and_changed_or_linked_configs_fail_before_rewrite() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("cmsd.toml");
        std::fs::write(&path, "require_store_selection='v2'\n").unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        assert!(prepare_config(&path).is_err());
        std::fs::write(&path, "reserve_gib=8\n").unwrap();
        let prepared = prepare_config(&path).unwrap();
        std::fs::write(&path, "reserve_gib=16\n").unwrap();
        assert!(apply_config(&path, &prepared).is_err());
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "reserve_gib=16\n");
        std::fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink(root.path().join("missing"), &path).unwrap();
        assert!(prepare_config(&path).is_err());
    }
}

#[cfg(test)]
mod state_compatibility_tests {
    use super::*;

    #[test]
    fn local_candidate_preserves_and_rejects_newer_authority_before_switching() {
        let root = tempfile::tempdir().unwrap();
        let info = serde_json::json!({"authority_scope":"local-v1"});
        verify_state_compatibility(&info, root.path()).unwrap();
        for name in [
            "identity-v2",
            "remote-v2",
            "control-v2",
            "control-v2.initialized.json",
        ] {
            let marker = root.path().join(name);
            std::fs::write(&marker, b"retained-authority").unwrap();
            assert_eq!(
                verify_state_compatibility(&info, root.path()).unwrap_err(),
                "component_authority_state_unsupported"
            );
            assert_eq!(std::fs::read(&marker).unwrap(), b"retained-authority");
            // Existing full candidates retain their own compatibility checks.
            verify_state_compatibility(&serde_json::json!({}), root.path()).unwrap();
            std::fs::remove_file(marker).unwrap();
        }
        assert_eq!(
            verify_state_compatibility(
                &serde_json::json!({"authority_scope":"unknown"}),
                root.path()
            )
            .unwrap_err(),
            "component_authority_scope_unsupported"
        );
    }
}
