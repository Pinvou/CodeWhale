//! Offline artifacts are operator-selected, not an arbitrary update URL. A
//! checksum proves integrity against the supplied manifest, not publisher trust.
use std::io::{Read, Write};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Manifest {
    pub format: String,
    pub component: String,
    pub version: String,
    pub architecture: String,
    pub os: String,
    pub api: String,
    pub state_format: String,
    pub sha256: String,
    pub size: u64,
}

pub fn verify(directory: &Path) -> Result<(Manifest, PathBuf), String> {
    if !directory.is_absolute()
        || std::fs::symlink_metadata(directory)
            .map_err(|e| e.to_string())?
            .file_type()
            .is_symlink()
    {
        return Err("invalid_package_path".into());
    }
    let binary = directory.join("cmsd");
    let manifest_path = directory.join("manifest.json");
    for path in [&binary, &manifest_path] {
        let metadata = std::fs::symlink_metadata(path).map_err(|e| e.to_string())?;
        if !metadata.is_file() || metadata.file_type().is_symlink() {
            return Err("linked_package_file".into());
        }
    }
    let raw = std::fs::read(&manifest_path).map_err(|e| e.to_string())?;
    if raw.len() > 16384 {
        return Err("invalid_package_manifest".into());
    }
    let manifest: Manifest =
        serde_json::from_slice(&raw).map_err(|e| format!("invalid_package_manifest: {e}"))?;
    if manifest.format != "megacube-cmsd-component-v1"
        || manifest.component != "cmsd"
        || manifest.os != "linux"
        || manifest.architecture != std::env::consts::ARCH
        || manifest.api != "v1"
        || manifest.state_format != "cmsd-json-v1"
        || manifest.size == 0
        || manifest.size > 256 * 1024 * 1024
        || manifest.sha256.len() != 64
        || !manifest
            .sha256
            .bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
        || semver::Version::parse(&manifest.version).is_err()
    {
        return Err("incompatible_package".into());
    }
    let mut file = std::fs::File::open(&binary).map_err(|e| e.to_string())?;
    if file.metadata().map_err(|e| e.to_string())?.len() != manifest.size {
        return Err("package_size_mismatch".into());
    }
    let mut header = [0u8; 20];
    file.read_exact(&mut header).map_err(|e| e.to_string())?;
    let machine = u16::from_le_bytes([header[18], header[19]]);
    let expected_machine = match std::env::consts::ARCH {
        "aarch64" => 183,
        "x86_64" => 62,
        _ => return Err("unsupported_architecture".into()),
    };
    if &header[..4] != b"\x7fELF" || header[4] != 2 || header[5] != 1 || machine != expected_machine
    {
        return Err("package_binary_architecture_mismatch".into());
    }
    let actual = hash(&binary)?;
    if actual != manifest.sha256 {
        return Err("package_checksum_mismatch".into());
    }
    Ok((manifest, binary))
}

pub fn hash(path: &Path) -> Result<String, String> {
    let mut input = std::fs::File::open(path).map_err(|e| e.to_string())?;
    let mut digest = Sha256::new();
    let mut bytes = [0u8; 65536];
    loop {
        let count = input.read(&mut bytes).map_err(|e| e.to_string())?;
        if count == 0 {
            break;
        }
        digest.update(&bytes[..count]);
    }
    Ok(digest
        .finalize()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect())
}

pub fn stage(
    directory: &Path,
    releases: &Path,
    expected_hash: &str,
    state_namespace_required: bool,
    profile_budget_required: bool,
    model_boot_policy_required: bool,
    store_adoption_required: bool,
    state: &Path,
) -> Result<(Manifest, PathBuf), String> {
    let (manifest, source) = verify(directory)?;
    if manifest.sha256 != expected_hash {
        return Err("package_changed_since_preview".into());
    }
    let release = releases.join(&manifest.sha256);
    super::private_directory(&release)?;
    let destination = release.join("cmsd");
    if !destination.exists() {
        let mut temp = tempfile::NamedTempFile::new_in(&release).map_err(|e| e.to_string())?;
        std::io::copy(
            &mut std::fs::File::open(source).map_err(|e| e.to_string())?,
            &mut temp,
        )
        .map_err(|e| e.to_string())?;
        temp.flush().map_err(|e| e.to_string())?;
        temp.as_file().sync_all().map_err(|e| e.to_string())?;
        if hash(temp.path())? != manifest.sha256 {
            return Err("package_changed_during_copy".into());
        }
        temp.as_file()
            .set_permissions(std::fs::Permissions::from_mode(0o700))
            .map_err(|e| e.to_string())?;
        temp.persist(&destination).map_err(|e| e.to_string())?;
    }
    if std::fs::symlink_metadata(&destination)
        .map_err(|e| e.to_string())?
        .file_type()
        .is_symlink()
        || hash(&destination)? != manifest.sha256
    {
        return Err("cached_package_changed".into());
    }
    let mut command = Command::new(&destination);
    command.arg("--component-info");
    let output = crate::platform::process::output_with_timeout(command, Duration::from_secs(4))?;
    let info: serde_json::Value =
        serde_json::from_slice(&output.stdout).map_err(|_| "package_probe_failed")?;
    if !output.status.success()
        || info["component"] != "cmsd"
        || info["version"] != manifest.version
        || info["architecture"] != manifest.architecture
        || info["os"] != "linux"
        || info["api"] != "v1"
        || info["state_format"] != "cmsd-json-v1"
        || info["update_contract"] != 1
    {
        return Err("package_probe_incompatible".into());
    }
    if state_namespace_required && info["native_unit_namespace"] != 1 {
        return Err("component_namespace_unsupported".into());
    }
    if profile_budget_required && info["native_profile_budget"] != 1 {
        return Err("component_budget_unsupported".into());
    }
    if model_boot_policy_required && info["model_boot_policy"] != 1 {
        return Err("component_boot_policy_unsupported".into());
    }
    if store_adoption_required && info["store_adoption"] != 1 {
        return Err("component_store_adoption_unsupported".into());
    }
    super::selection_policy::verify_component(&info)?;
    super::selection_policy::verify_state_compatibility(&info, state)?;
    super::write_json(&release.join("manifest.json"), &manifest)?;
    Ok((manifest, destination))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> (tempfile::TempDir, Manifest) {
        let directory = tempfile::tempdir().unwrap();
        let mut header = vec![0u8; 32];
        header[..4].copy_from_slice(b"\x7fELF");
        header[4] = 2;
        header[5] = 1;
        let machine = if std::env::consts::ARCH == "aarch64" {
            183u16
        } else {
            62u16
        };
        header[18..20].copy_from_slice(&machine.to_le_bytes());
        std::fs::write(directory.path().join("cmsd"), &header).unwrap();
        let manifest = Manifest {
            format: "megacube-cmsd-component-v1".into(),
            component: "cmsd".into(),
            version: "0.1.0".into(),
            architecture: std::env::consts::ARCH.into(),
            os: "linux".into(),
            api: "v1".into(),
            state_format: "cmsd-json-v1".into(),
            sha256: hash(&directory.path().join("cmsd")).unwrap(),
            size: header.len() as u64,
        };
        std::fs::write(
            directory.path().join("manifest.json"),
            serde_json::to_vec(&manifest).unwrap(),
        )
        .unwrap();
        (directory, manifest)
    }

    #[test]
    fn package_rejects_corruption_and_wrong_architecture_before_execution() {
        let (directory, mut manifest) = fixture();
        assert!(verify(directory.path()).is_ok());
        std::fs::write(directory.path().join("cmsd"), b"changed").unwrap();
        assert_eq!(
            verify(directory.path()).unwrap_err(),
            "package_size_mismatch"
        );
        manifest.architecture = "other".into();
        std::fs::write(
            directory.path().join("manifest.json"),
            serde_json::to_vec(&manifest).unwrap(),
        )
        .unwrap();
        assert_eq!(
            verify(directory.path()).unwrap_err(),
            "incompatible_package"
        );
    }

    #[test]
    fn preview_digest_is_pinned_and_linked_files_are_refused() {
        let (directory, _) = fixture();
        let releases = tempfile::tempdir().unwrap();
        let state = tempfile::tempdir().unwrap();
        assert_eq!(
            stage(
                directory.path(),
                releases.path(),
                &"0".repeat(64),
                true,
                true,
                true,
                true,
                state.path(),
            )
            .unwrap_err(),
            "package_changed_since_preview"
        );
        assert_eq!(std::fs::read_dir(releases.path()).unwrap().count(), 0);
        let original = directory.path().join("cmsd");
        let target = directory.path().join("other");
        std::fs::rename(&original, &target).unwrap();
        std::os::unix::fs::symlink(target, &original).unwrap();
        assert_eq!(verify(directory.path()).unwrap_err(), "linked_package_file");
    }
}
