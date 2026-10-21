//! Explicit component hand-off from the known trusted-bootstrap format. The
//! private receipt is local provenance, not remote trust or model authority.
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

use super::{Record, binding, package, systemd};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct Import {
    root: PathBuf,
    operation_id: String,
    machine_binding: String,
    sha256: String,
    device_id: String,
    completed_at: u64,
    imported_at: u64,
}

impl Import {
    pub(super) fn complete(mut self) -> Self {
        self.imported_at = super::now();
        self
    }
    pub(super) fn sha256(&self) -> &str {
        &self.sha256
    }
    pub(super) fn public(&self) -> Value {
        json!({"root":self.root,"operationId":self.operation_id,"sha256":self.sha256,
            "deviceId":self.device_id,"completedAt":self.completed_at,"importedAt":(self.imported_at != 0).then_some(self.imported_at)})
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Intent {
    format: u32,
    machine_id: String,
    sha256: String,
    root: PathBuf,
    unit: String,
    socket: PathBuf,
    gateway_port: u16,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ResultReceipt {
    state: String,
    device_id: String,
    version: String,
    sha256: String,
    socket: PathBuf,
    unit: String,
    managed_by_platform: bool,
    lifecycle_authority_granted: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Journal {
    intent: Intent,
    operation_id: String,
    phase: String,
    created_at: u64,
    config_sha256: String,
    unit_sha256: String,
    result: ResultReceipt,
    completed_at: u64,
}

fn invalid() -> String {
    "bootstrap_provenance_unverified: inspect the completed bootstrap receipt and live service binding".into()
}

fn installation_root(record: &Record) -> Option<PathBuf> {
    if record.owned_unit || record.managed || record.binary.file_name()? != "cmsd" {
        return None;
    }
    let release = record.binary.parent()?;
    let hash = release.file_name()?.to_str()?;
    if hash.len() != 64
        || !hash
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return None;
    }
    let releases = release.parent()?;
    (releases.file_name()? == "releases")
        .then(|| releases.parent().map(Path::to_owned))
        .flatten()
}

pub(super) fn has_journal(record: &Record) -> bool {
    installation_root(record)
        .is_some_and(|root| std::fs::symlink_metadata(root.join("bootstrap.json")).is_ok())
}

fn private(path: &Path, directory: bool, mode: u32) -> Result<(), String> {
    let metadata = std::fs::symlink_metadata(path).map_err(|_| invalid())?;
    // SAFETY: geteuid has no preconditions.
    if metadata.uid() != unsafe { libc::geteuid() }
        || metadata.file_type().is_symlink()
        || metadata.mode() & 0o777 != mode
        || (directory && !metadata.is_dir())
        || (!directory && (!metadata.is_file() || metadata.nlink() != 1))
    {
        return Err(invalid());
    }
    Ok(())
}

pub(super) fn inspect(record: &Record) -> Result<Option<Import>, String> {
    if !has_journal(record) {
        return Ok(None);
    }
    let root = installation_root(record).ok_or_else(invalid)?;
    for parent in root.ancestors() {
        if std::fs::symlink_metadata(parent)
            .map_err(|_| invalid())?
            .file_type()
            .is_symlink()
        {
            return Err(invalid());
        }
    }
    private(&root, true, 0o700)?;
    private(&root.join("releases"), true, 0o700)?;
    private(record.binary.parent().ok_or_else(invalid)?, true, 0o700)?;
    private(&record.binary, false, 0o700)?;
    private(&record.config, false, 0o600)?;
    let journal_path = root.join("bootstrap.json");
    private(&journal_path, false, 0o600)?;
    if std::fs::metadata(&journal_path)
        .map_err(|_| invalid())?
        .len()
        > 512 * 1024
    {
        return Err(invalid());
    }
    let journal: Journal = super::read_json(&journal_path).map_err(|_| invalid())?;
    verify_receipt(record, &root, &journal)?;
    private(&systemd::unit_directory().join(&record.unit), false, 0o600)?;
    let current = binding::capture(record)?;
    if !current.matches_hashes(
        &journal.unit_sha256,
        &journal.config_sha256,
        &journal.intent.sha256,
    ) {
        return Err(invalid());
    }
    let live = systemd::bind_live(&record.unit, &record.socket)?;
    if live.binary != record.binary
        || live.config != record.config
        || live.state != record.state
        || live.version != journal.result.version
    {
        return Err(invalid());
    }
    let facts = crate::features::megacube::Client::connect()?.device()?;
    if facts.get("error").is_some_and(|error| !error.is_null())
        || facts
            .get("http_status")
            .and_then(Value::as_u64)
            .is_some_and(|status| status != 200)
        || facts.get("device_id").and_then(Value::as_str) != Some(journal.result.device_id.as_str())
    {
        return Err(invalid());
    }
    Ok(Some(Import {
        root,
        operation_id: journal.operation_id,
        machine_binding: journal.intent.machine_id,
        sha256: journal.intent.sha256,
        device_id: journal.result.device_id,
        completed_at: journal.completed_at,
        imported_at: 0,
    }))
}

fn verify_receipt(record: &Record, root: &Path, journal: &Journal) -> Result<(), String> {
    let machine = std::fs::read_to_string("/etc/machine-id").map_err(|_| invalid())?;
    let binding = Sha256::digest(machine.trim().as_bytes())
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let intent = &journal.intent;
    let result = &journal.result;
    if journal.phase != "succeeded"
        || intent.format != 1
        || intent.machine_id != binding
        || intent.root != root
        || intent.unit != record.unit
        || intent.socket != record.socket
        || record.config != root.join("cmsd.toml")
        || record.state != root.join("state")
        || record.binary != root.join("releases").join(&intent.sha256).join("cmsd")
        || record.original_unit.is_none()
        || !record.installed
        || record.maintenance_owner.is_some()
        || journal.operation_id.len() < 8
        || journal.operation_id.len() > 128
        || !journal
            .operation_id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b))
        || journal.created_at == 0
        || journal.completed_at < journal.created_at
        || intent.gateway_port < 1024
        || result.state != "installed"
        || result.device_id.is_empty()
        || result.version != record.version
        || result.sha256 != intent.sha256
        || result.unit != intent.unit
        || result.socket != intent.socket
        || result.managed_by_platform
        || result.lifecycle_authority_granted
        || package::hash(&record.binary)? != intent.sha256
    {
        return Err(invalid());
    }
    let text = super::control_file::read_registration_text(&record.config, 512 * 1024)
        .map_err(|_| invalid())?
        .ok_or_else(invalid)?;
    let config: toml::Value = toml::from_str(&text).map_err(|_| invalid())?;
    let unit_dir = config
        .get("unit_dir")
        .and_then(toml::Value::as_str)
        .map(Path::new);
    let scoped = config
        .get("native_unit_namespace")
        .and_then(toml::Value::as_str)
        == Some("state");
    let unit_dir_matches = if scoped {
        unit_dir == Some(systemd::unit_directory().as_path())
    } else {
        unit_dir == Some(root.join("units").as_path())
    };
    if config.get("gateway_listen").and_then(toml::Value::as_str)
        != Some(format!("127.0.0.1:{}", intent.gateway_port).as_str())
        || !unit_dir_matches
    {
        return Err(invalid());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    fn fixture() -> (tempfile::TempDir, Record, Value) {
        let directory = tempfile::tempdir().unwrap();
        let root = directory.path();
        let hash = Sha256::digest(b"fixture binary")
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let binary = root.join("releases").join(&hash).join("cmsd");
        std::fs::create_dir_all(binary.parent().unwrap()).unwrap();
        std::fs::write(&binary, b"fixture binary").unwrap();
        let config = root.join("cmsd.toml");
        std::fs::write(
            &config,
            format!(
                "gateway_listen = \"127.0.0.1:18080\"\nunit_dir = {:?}\n",
                root.join("units")
            ),
        )
        .unwrap();
        // Production bootstrap configuration is private (inspect verifies 0600).
        std::fs::set_permissions(&config, std::fs::Permissions::from_mode(0o600)).unwrap();
        let record: Record = serde_json::from_value(json!({"unit":"cmsd-bootstrap-fixture.service", "socket":root.join("cmsd.sock"),
            "config":config,"state":root.join("state"),"binary":binary,"version":"0.2.0","owned_unit":false,"managed":false,
            "installed":true,"original_unit":"fixture original unit","previous":null})).unwrap();
        let machine = std::fs::read_to_string("/etc/machine-id").unwrap();
        let machine = Sha256::digest(machine.trim().as_bytes())
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let receipt = json!({"intent":{"format":1,"machine_id":machine,"sha256":hash,"root":root,"unit":record.unit,"socket":record.socket,"gateway_port":18080},
            "phase":"succeeded","operation_id":"fixture-operation","created_at":1,"completed_at":2,"unit_sha256":"checked-by-capture","config_sha256":"checked-by-capture",
            "result":{"state":"installed","device_id":"device-fixture","version":"0.2.0","sha256":hash,"unit":record.unit,"socket":record.socket,"managed_by_platform":false,"lifecycle_authority_granted":false}});
        (directory, record, receipt)
    }

    #[test]
    fn handoff_requires_completed_matching_intent_and_no_prior_authority() {
        let (directory, record, baseline) = fixture();
        assert!(
            verify_receipt(
                &record,
                directory.path(),
                &serde_json::from_value(baseline.clone()).unwrap()
            )
            .is_ok()
        );
        for (pointer, value) in [
            ("/phase", json!("prepared")),
            ("/intent/machine_id", json!("foreign-machine")),
            ("/intent/root", json!("/different/root")),
            ("/intent/unit", json!("cmsd-other.service")),
            ("/intent/socket", json!("/different.sock")),
            ("/intent/sha256", json!("0".repeat(64))),
            ("/intent/gateway_port", json!(10)),
            ("/operation_id", json!("bad/operation")),
            ("/completed_at", json!(0)),
            ("/result/lifecycle_authority_granted", json!(true)),
            ("/result/managed_by_platform", json!(true)),
            ("/result/version", json!("9.0.0")),
        ] {
            let mut changed = baseline.clone();
            *changed.pointer_mut(pointer).unwrap() = value;
            let journal = serde_json::from_value(changed).unwrap();
            assert!(
                verify_receipt(&record, directory.path(), &journal).is_err(),
                "{pointer}"
            );
        }
        let template = systemd::unit_text(&record).unwrap();
        assert!(template.lines().any(|line| {
            line.starts_with("UnsetEnvironment=")
                && line
                    .split_whitespace()
                    .any(|key| key == "CMSD_CONTROL_LISTEN")
        }));
        assert!(
            !systemd::legacy_unit_text(&record)
                .unwrap()
                .contains("UnsetEnvironment=")
        );
    }

    #[test]
    fn scoped_bootstrap_requires_the_real_user_unit_search_directory() {
        let (directory, record, baseline) = fixture();
        let journal = serde_json::from_value(baseline).unwrap();
        let text = |units: &Path| {
            format!(
                "gateway_listen = \"127.0.0.1:18080\"\nnative_unit_namespace = \"state\"\nunit_dir = {:?}\n",
                units
            )
        };
        std::fs::write(&record.config, text(&systemd::unit_directory())).unwrap();
        assert!(verify_receipt(&record, directory.path(), &journal).is_ok());
        std::fs::write(&record.config, text(&directory.path().join("units"))).unwrap();
        assert!(verify_receipt(&record, directory.path(), &journal).is_err());
    }

    #[test]
    fn duplicate_receipt_fields_do_not_override_an_incomplete_phase() {
        let (_, _, baseline) = fixture();
        let raw = serde_json::to_string(&baseline).unwrap().replace(
            "\"phase\":\"succeeded\"",
            "\"phase\":\"prepared\",\"phase\":\"succeeded\"",
        );
        assert!(serde_json::from_str::<Journal>(&raw).is_err());
    }
}
