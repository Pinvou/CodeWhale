//! A profile's receipt is evidence about one registration, not authority over
//! whichever service happens to use the same unit name later.
use std::collections::BTreeMap;
use std::fs::OpenOptions;
use std::io::Read;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use super::{Record, systemd};

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct Registration {
    pub(super) fragment: PathBuf,
    pub(super) unit_sha256: String,
    pub(super) config_sha256: String,
    pub(super) binary_sha256: String,
    pub(super) drop_in_sha256: Option<String>,
}

impl Registration {
    pub(super) fn matches_hashes(&self, unit: &str, config: &str, binary: &str) -> bool {
        self.unit_sha256 == unit
            && self.config_sha256 == config
            && self.binary_sha256 == binary
            && self.drop_in_sha256.is_none()
    }
}

fn drift() -> String {
    "service_configuration_changed: current registration differs from the saved binding; inspect before control".into()
}

fn hex(bytes: impl AsRef<[u8]>) -> String {
    bytes
        .as_ref()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

pub(super) fn hash(path: &Path, limit: u64) -> Result<String, String> {
    let mut file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|_| drift())?;
    let metadata = file.metadata().map_err(|_| drift())?;
    if !metadata.is_file() || metadata.len() > limit || metadata.mode() & 0o022 != 0 {
        return Err(drift());
    }
    let mut digest = Sha256::new();
    let mut bytes = [0; 65536];
    let mut total = 0;
    loop {
        let size = file.read(&mut bytes).map_err(|_| drift())?;
        if size == 0 {
            break;
        }
        total += size as u64;
        if total > limit {
            return Err(drift());
        }
        digest.update(&bytes[..size]);
    }
    Ok(hex(digest.finalize()))
}

pub(super) fn override_path(record: &Record) -> PathBuf {
    systemd::unit_directory().join(format!("{}.d/90-pinvou-cmsd.conf", record.unit))
}

pub(super) fn override_text(record: &Record) -> Result<String, String> {
    Ok(format!(
        "# Pinvou managed cmsd component\n[Service]\nExecStart=\nExecStart={}\n",
        systemd::quoted(&record.binary)?.replace('$', "$$")
    ))
}

pub(super) fn config_matches(record: &Record) -> Result<(), String> {
    let text = super::control_file::read_registration_text(&record.config, 512 * 1024)
        .map_err(|_| drift())?
        .ok_or_else(drift)?;
    let value: toml::Value = toml::from_str(&text).map_err(|_| drift())?;
    for (key, expected) in [
        ("socket_path", &record.socket),
        ("state_dir", &record.state),
    ] {
        if value.get(key).and_then(toml::Value::as_str).map(Path::new) != Some(expected.as_path()) {
            return Err(
                "unverified_service_config: explicit matching socket_path and state_dir required"
                    .into(),
            );
        }
    }
    if value.get("systemd_mode").and_then(toml::Value::as_str) != Some("user") {
        return Err("unverified_service_config: user service mode required".into());
    }
    Ok(())
}

pub(super) fn capture(record: &Record) -> Result<Registration, String> {
    capture_at(
        record,
        &systemd::show(&record.unit)?,
        &systemd::unit_directory(),
    )
}

fn capture_at(
    record: &Record,
    properties: &BTreeMap<String, String>,
    units: &Path,
) -> Result<Registration, String> {
    if properties.get("LoadState").map(String::as_str) != Some("loaded")
        || properties.get("NeedDaemonReload").map(String::as_str) != Some("no")
    {
        return Err(drift());
    }
    let fragment = properties
        .get("FragmentPath")
        .map(PathBuf::from)
        .ok_or_else(drift)?;
    if !fragment.is_absolute() || (record.owned_unit && fragment != units.join(&record.unit)) {
        return Err(drift());
    }
    // Custom drop-ins may override paths, credentials or the executable. They
    // require a separate migration instead of being silently retained/removed.
    let drop_in = units.join(format!("{}.d/90-pinvou-cmsd.conf", record.unit));
    let paths = properties
        .get("DropInPaths")
        .map(String::as_str)
        .unwrap_or("");
    let expected_drop_in = record.managed && !record.owned_unit;
    if (expected_drop_in && paths != drop_in.to_str().ok_or_else(drift)?)
        || (!expected_drop_in && !paths.is_empty())
    {
        return Err(
            "unverified_service_drop_ins: inspect custom service overrides before migration".into(),
        );
    }
    config_matches(record)?;
    Ok(Registration {
        fragment: fragment.clone(),
        unit_sha256: hash(&fragment, 512 * 1024)?,
        config_sha256: hash(&record.config, 512 * 1024)?,
        binary_sha256: hash(&record.binary, 512 * 1024 * 1024)?,
        drop_in_sha256: if expected_drop_in {
            Some(hash(&drop_in, 512 * 1024)?)
        } else {
            None
        },
    })
}

pub(super) fn verify_artifacts(record: &Record) -> Result<(), String> {
    let Some(saved) = record.registration.as_ref() else {
        // Legacy rollback receipts are checked against their original unit or
        // content-addressed package; never invent a hash from a stale receipt.
        return super::verify_cached(record);
    };
    if hash(&record.config, 512 * 1024)? != saved.config_sha256
        || hash(&record.binary, 512 * 1024 * 1024)? != saved.binary_sha256
    {
        return Err(drift());
    }
    Ok(())
}

/// The receipt is written before the unit. Its expected bytes authorize only
/// recovery of this exact candidate, never a same-name foreign registration.
pub(super) fn prepare_install(record: &Record) -> Result<Registration, String> {
    if !record.owned_unit || !record.managed || record.installed || !record.pending_install {
        return Err(drift());
    }
    config_matches(record)?;
    let fragment = systemd::unit_directory().join(&record.unit);
    Ok(Registration {
        fragment,
        unit_sha256: hex(Sha256::digest(systemd::unit_text(record)?.as_bytes())),
        config_sha256: hash(&record.config, 512 * 1024)?,
        binary_sha256: hash(&record.binary, 512 * 1024 * 1024)?,
        drop_in_sha256: None,
    })
}

fn verify_pending_install(
    record: &Record,
    properties: &BTreeMap<String, String>,
    units: &Path,
) -> Result<(), String> {
    let expected = record.registration.as_ref().ok_or_else(drift)?;
    if !record.pending_install
        || record.installed
        || !record.owned_unit
        || !record.managed
        || record.original_unit.is_some()
        || expected.fragment != units.join(&record.unit)
        || expected.drop_in_sha256.is_some()
        || expected.unit_sha256 != hex(Sha256::digest(systemd::unit_text(record)?.as_bytes()))
        || properties.get("MainPID").map(String::as_str) != Some("0")
        || properties
            .get("DropInPaths")
            .is_some_and(|paths| !paths.is_empty())
        || std::fs::symlink_metadata(units.join(format!("{}.d", record.unit))).is_ok()
        || systemd::socket_peer(&record.socket)?.is_some()
    {
        return Err(drift());
    }
    config_matches(record)?;
    verify_artifacts(record)?;
    let path = units.join(&record.unit);
    match std::fs::symlink_metadata(&path) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            if properties.get("LoadState").map(String::as_str) != Some("not-found") {
                return Err(drift());
            }
        }
        Ok(_) => {
            if hash(&path, 512 * 1024)? != expected.unit_sha256 {
                return Err(drift());
            }
            if properties.get("LoadState").map(String::as_str) == Some("loaded")
                && properties.get("FragmentPath").map(PathBuf::from) != Some(path)
            {
                return Err(drift());
            }
        }
        Err(_) => return Err(drift()),
    }
    Ok(())
}

pub(super) fn verify(record: &mut Record) -> Result<(), String> {
    let properties = systemd::show(&record.unit)?;
    if record.pending_install {
        return verify_pending_install(record, &properties, &systemd::unit_directory());
    }
    if !record.installed {
        return verify_absent(record, &properties, &systemd::unit_directory());
    }
    let current = capture_at(record, &properties, &systemd::unit_directory())?;
    if let Some(saved) = record.registration.as_ref() {
        if saved != &current {
            return Err(drift());
        }
    } else {
        verify_legacy(record, &current)?;
    }
    let pid = properties
        .get("MainPID")
        .and_then(|v| v.parse::<u32>().ok())
        .unwrap_or(0);
    if pid != 0 {
        let live = systemd::bind_process(&record.unit, &record.socket, false)?;
        if live.binary != record.binary
            || live.config != record.config
            || live.state != record.state
        {
            return Err(drift());
        }
    } else if systemd::socket_peer(&record.socket)?.is_some() {
        // No unit process can own this live socket. Never act on the peer by
        // guessing that it is the failed service from the receipt.
        return Err("service_socket_mismatch".into());
    }
    record.registration = Some(current);
    Ok(())
}

fn verify_absent(
    record: &Record,
    properties: &BTreeMap<String, String>,
    units: &Path,
) -> Result<(), String> {
    if !record.owned_unit
        || properties.get("LoadState").map(String::as_str) != Some("not-found")
        || properties.get("MainPID").map(String::as_str) != Some("0")
        || std::fs::symlink_metadata(units.join(&record.unit)).is_ok()
        || std::fs::symlink_metadata(units.join(format!("{}.d", record.unit))).is_ok()
        || systemd::socket_peer(&record.socket)?.is_some()
    {
        return Err(drift());
    }
    config_matches(record)?;
    verify_artifacts(record)
}

fn verify_legacy(record: &Record, current: &Registration) -> Result<(), String> {
    let expected = if record.owned_unit {
        systemd::unit_text(record)?
    } else {
        record.original_unit.clone().ok_or_else(drift)?
    };
    if hex(Sha256::digest(expected.as_bytes())) != current.unit_sha256
        && !(record.owned_unit
            && hex(Sha256::digest(
                systemd::legacy_unit_text(record)?.as_bytes(),
            )) == current.unit_sha256)
    {
        return Err(drift());
    }
    if record.managed
        && !record.owned_unit
        && hash(&override_path(record), 512 * 1024)?
            != hex(Sha256::digest(override_text(record)?.as_bytes()))
    {
        return Err(drift());
    }
    super::verify_cached(record)?;
    // External legacy receipts have no trusted binary digest. Re-establish the
    // binding with the actual live process before upgrading their receipt.
    if !record.managed {
        let live = systemd::bind_process(&record.unit, &record.socket, false)?;
        if live.binary != record.binary
            || live.config != record.config
            || live.state != record.state
        {
            return Err(drift());
        }
    }
    Ok(())
}

pub(super) fn config_sha256(record: &Record) -> Result<String, String> {
    hash(&record.config, 512 * 1024)
}

/// Prepare only the config digest change authorized by this exact migration.
/// This is a plan until apply_config succeeds; executable/registration hashes
/// stay unchanged, including every retained previous component.
pub(super) fn plan_config_revision(
    record: &Record,
    original_sha256: &str,
    merged_sha256: &str,
) -> Result<Record, String> {
    fn advance(record: &mut Record, target: &Record, old: &str, new: &str) -> Result<(), String> {
        if !same_target(record, target) {
            return Err(drift());
        }
        let registration = record.registration.as_mut().ok_or_else(drift)?;
        if registration.config_sha256 != old {
            return Err(drift());
        }
        registration.config_sha256 = new.to_owned();
        if let Some(previous) = record.previous.as_mut() {
            advance(previous, target, old, new)?;
        }
        Ok(())
    }
    let mut planned = record.clone();
    advance(&mut planned, record, original_sha256, merged_sha256)?;
    Ok(planned)
}

pub(super) fn same_target(previous: &Record, current: &Record) -> bool {
    previous.unit == current.unit
        && previous.socket == current.socket
        && previous.config == current.config
        && previous.state == current.state
        && previous.owned_unit == current.owned_unit
        && previous.original_unit == current.original_unit
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{FileTypeExt, PermissionsExt};

    fn fixture(path: &Path) -> (Record, BTreeMap<String, String>) {
        let record = Record {
            unit: "cmsd-qa.service".into(),
            socket: path.join("cmsd.sock"),
            config: path.join("cmsd.toml"),
            state: path.join("state"),
            binary: path.join("cmsd"),
            version: "1.0.0".into(),
            owned_unit: true,
            managed: false,
            installed: true,
            original_unit: None,
            previous: None,
            maintenance_owner: None,
            registration: None,
            pending_install: false,
            bootstrap_import: None,
            factory_import: None,
        };
        std::fs::write(&record.binary, "fixture executable").unwrap();
        std::fs::write(
            &record.config,
            format!(
                "socket_path = {:?}\nstate_dir = {:?}\nsystemd_mode = \"user\"\n",
                record.socket, record.state
            ),
        )
        .unwrap();
        std::fs::write(
            path.join(&record.unit),
            systemd::unit_text(&record).unwrap(),
        )
        .unwrap();
        for file in [&record.binary, &record.config, &path.join(&record.unit)] {
            std::fs::set_permissions(file, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        let properties = BTreeMap::from([
            ("LoadState".into(), "loaded".into()),
            ("NeedDaemonReload".into(), "no".into()),
            ("DropInPaths".into(), "".into()),
            (
                "FragmentPath".into(),
                path.join(&record.unit).to_str().unwrap().into(),
            ),
            ("MainPID".into(), "0".into()),
        ]);
        (record, properties)
    }

    #[test]
    fn stale_receipt_detects_replacement_config_binary_and_drop_ins() {
        let directory = tempfile::tempdir().unwrap();
        let (mut record, mut properties) = fixture(directory.path());
        let original = capture_at(&record, &properties, directory.path()).unwrap();
        record.registration = Some(original.clone());
        std::fs::write(directory.path().join(&record.unit), "foreign replacement").unwrap();
        assert_ne!(
            capture_at(&record, &properties, directory.path()).unwrap(),
            original
        );
        std::fs::write(&record.binary, "changed executable").unwrap();
        assert!(verify_artifacts(&record).is_err());
        std::fs::write(&record.config, "socket_path = \"/different.sock\"\n").unwrap();
        assert!(capture_at(&record, &properties, directory.path()).is_err());
        properties.insert("DropInPaths".into(), "/tmp/other.conf".into());
        assert!(capture_at(&record, &properties, directory.path()).is_err());
        let mut previous = record.clone();
        previous.state = directory.path().join("other-state");
        assert!(!same_target(&previous, &record));
    }

    #[test]
    fn restore_never_overwrites_a_new_registration_with_the_same_name() {
        let directory = tempfile::tempdir().unwrap();
        let (mut record, mut properties) = fixture(directory.path());
        record.registration = Some(capture_at(&record, &properties, directory.path()).unwrap());
        record.installed = false;
        properties.insert("LoadState".into(), "not-found".into());
        assert!(verify_absent(&record, &properties, directory.path()).is_err());
        std::fs::remove_file(directory.path().join(&record.unit)).unwrap();
        assert!(verify_absent(&record, &properties, directory.path()).is_ok());
        std::fs::create_dir(directory.path().join(format!("{}.d", record.unit))).unwrap();
        assert!(verify_absent(&record, &properties, directory.path()).is_err());
    }
    #[test]
    fn first_install_can_recover_its_exact_unit_before_reload_but_never_foreign_content() {
        let directory = tempfile::tempdir().unwrap();
        let (mut record, mut properties) = fixture(directory.path());
        record.managed = true;
        record.installed = false;
        record.pending_install = true;
        record.registration = Some(Registration {
            fragment: directory.path().join(&record.unit),
            unit_sha256: hex(Sha256::digest(
                systemd::unit_text(&record).unwrap().as_bytes(),
            )),
            config_sha256: hash(&record.config, 512 * 1024).unwrap(),
            binary_sha256: hash(&record.binary, 512 * 1024 * 1024).unwrap(),
            drop_in_sha256: None,
        });
        properties.insert("LoadState".into(), "not-found".into());
        properties.insert("NeedDaemonReload".into(), "yes".into());
        assert!(verify_pending_install(&record, &properties, directory.path()).is_ok());
        std::fs::remove_file(directory.path().join(&record.unit)).unwrap();
        assert!(verify_pending_install(&record, &properties, directory.path()).is_ok());
        std::fs::write(directory.path().join(&record.unit), "foreign replacement").unwrap();
        assert!(verify_pending_install(&record, &properties, directory.path()).is_err());
        std::fs::remove_file(directory.path().join(&record.unit)).unwrap();
        std::fs::write(&record.binary, "changed candidate").unwrap();
        assert!(verify_pending_install(&record, &properties, directory.path()).is_err());
    }
    #[test]
    fn config_digest_migration_preserves_all_other_evidence_and_previous_receipts() {
        let directory = tempfile::tempdir().unwrap();
        let (mut record, properties) = fixture(directory.path());
        record.registration = Some(capture_at(&record, &properties, directory.path()).unwrap());
        let mut old_version = record.clone();
        let registration = old_version.registration.as_mut().unwrap();
        registration.binary_sha256 = "historical-binary-digest".into();
        registration.unit_sha256 = "historical-unit-digest".into();
        registration.drop_in_sha256 = Some("historical-drop-in-digest".into());
        record.previous = Some(Box::new(old_version));
        let old = record.registration.as_ref().unwrap().config_sha256.clone();
        let new = "a".repeat(64);
        let planned = plan_config_revision(&record, &old, &new).unwrap();
        assert_eq!(record.registration.as_ref().unwrap().config_sha256, old);
        let mut expected = record.clone();
        expected.registration.as_mut().unwrap().config_sha256 = new.clone();
        expected
            .previous
            .as_mut()
            .unwrap()
            .registration
            .as_mut()
            .unwrap()
            .config_sha256 = new;
        assert_eq!(
            serde_json::to_value(planned).unwrap(),
            serde_json::to_value(expected).unwrap()
        );
        assert!(plan_config_revision(&record, &"b".repeat(64), &"c".repeat(64)).is_err());
        record.previous.as_mut().unwrap().config = directory.path().join("foreign-config");
        assert!(plan_config_revision(&record, &old, &"c".repeat(64)).is_err());
    }

    #[test]
    fn service_config_unknown_files_are_refused_without_waiting_or_repairing() {
        let directory = tempfile::tempdir().unwrap();
        let (record, _) = fixture(directory.path());
        std::fs::set_permissions(&record.config, std::fs::Permissions::from_mode(0o644)).unwrap();
        config_matches(&record).unwrap();
        std::fs::remove_file(&record.config).unwrap();
        assert!(config_matches(&record).is_err());
        std::os::unix::fs::symlink(directory.path().join("missing"), &record.config).unwrap();
        assert!(config_matches(&record).is_err());
        std::fs::remove_file(&record.config).unwrap();
        let name = std::ffi::CString::new(record.config.as_os_str().as_encoded_bytes()).unwrap();
        // SAFETY: name is a terminated path owned by this temporary test.
        assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
        assert!(config_matches(&record).is_err());
        assert!(
            std::fs::symlink_metadata(&record.config)
                .unwrap()
                .file_type()
                .is_fifo()
        );
    }
}
