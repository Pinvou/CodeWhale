//! Exact registration replacement and crash recovery. Callers hold the shared
//! component lock. The journal predates every write and never adopts disk bytes.
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use super::{Record, binding, systemd};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Intent {
    format: String,
    before: Record,
    after: Record,
    old_text: Option<String>,
    new_text: Option<String>,
}

fn path() -> PathBuf {
    super::root().join("registration-transition.json")
}
fn invalid() -> String {
    "component_registration_changed: inspect the retained registration journal".into()
}
fn digest(text: &str) -> String {
    Sha256::digest(text.as_bytes())
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}
fn same(left: &Record, right: &Record) -> Result<bool, String> {
    Ok(serde_json::to_value(left).map_err(|_| invalid())?
        == serde_json::to_value(right).map_err(|_| invalid())?)
}
pub(super) fn pending() -> bool {
    std::fs::symlink_metadata(path()).is_ok()
}
fn text_at(path: &Path) -> Result<Option<String>, String> {
    super::control_file::read_optional(path, 512 * 1024)
        .map_err(|_| invalid())?
        .map(|bytes| String::from_utf8(bytes).map_err(|_| invalid()))
        .transpose()
}

fn clear() -> Result<(), String> {
    std::fs::remove_file(path()).map_err(|e| e.to_string())?;
    sync_directory(&super::root())
}
fn sync_directory(path: &Path) -> Result<(), String> {
    std::fs::File::open(path)
        .and_then(|file| file.sync_all())
        .map_err(|e| e.to_string())
}

pub(super) fn stopped_daemon(record: &Record, create: bool) -> Result<std::fs::File, String> {
    let file = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(create)
        .truncate(false)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(record.state.join("daemon.lock"))
        .map_err(|_| invalid())?;
    let metadata = file.metadata().map_err(|_| invalid())?;
    // SAFETY: geteuid has no preconditions.
    let uid = unsafe { libc::geteuid() };
    if !metadata.is_file() || metadata.uid() != uid || metadata.mode() & 0o022 != 0 {
        return Err(invalid());
    }
    file.try_lock()
        .map_err(|_| "service_still_running".to_owned())?;
    Ok(file)
}

impl Intent {
    fn changed_path(&self) -> PathBuf {
        if self.before.owned_unit {
            systemd::unit_directory().join(&self.before.unit)
        } else {
            binding::override_path(&self.before)
        }
    }
    fn planned_after(before: &Record, target: &Record) -> Result<Record, String> {
        if !binding::same_target(before, target) {
            return Err(invalid());
        }
        let old = before.registration.as_ref().ok_or_else(invalid)?;
        binding::config_matches(target)?;
        if !target.installed {
            // Uninstall removes only this proven owned registration. Retain its
            // artifact hashes for Restore; absence cannot authorize any other
            // receipt, executable, configuration or maintenance-owner change.
            let mut after = before.clone();
            after.installed = false;
            if !before.owned_unit
                || !before.installed
                || before.pending_install
                || !same(&after, target)?
            {
                return Err(invalid());
            }
            return Ok(after);
        }
        let mut after = target.clone();
        after.pending_install = false;
        after.registration = Some(binding::Registration {
            fragment: old.fragment.clone(),
            unit_sha256: if target.owned_unit {
                digest(&systemd::unit_text(target)?)
            } else {
                old.unit_sha256.clone()
            },
            config_sha256: binding::hash(&target.config, 512 * 1024)?,
            binary_sha256: binding::hash(&target.binary, 512 * 1024 * 1024)?,
            drop_in_sha256: if !target.owned_unit && target.managed {
                Some(digest(&binding::override_text(target)?))
            } else {
                None
            },
        });
        if after
            .registration
            .as_ref()
            .ok_or_else(invalid)?
            .config_sha256
            != old.config_sha256
        {
            return Err(invalid());
        }
        Ok(after)
    }
    fn validate(&self) -> Result<(), String> {
        if self.format != "cmsd-registration-transition-v1"
            || !systemd::unit_valid(&self.before.unit)
            || !same(
                &Self::planned_after(&self.before, &self.after)?,
                &self.after,
            )?
        {
            return Err(invalid());
        }
        let old = self.before.registration.as_ref().ok_or_else(invalid)?;
        let new = self.after.registration.as_ref().ok_or_else(invalid)?;
        if old.fragment != systemd::unit_directory().join(&self.before.unit) {
            // Only a restorable, exact user unit can receive our override.
            return Err(invalid());
        }
        if self.before.owned_unit {
            let expected_text = self
                .after
                .installed
                .then(|| systemd::unit_text(&self.after))
                .transpose()?;
            if self
                .old_text
                .as_ref()
                .is_some_and(|text| digest(text) != old.unit_sha256)
                || (self.old_text.is_none() && self.before.installed)
                || self.new_text != expected_text
            {
                return Err(invalid());
            }
        } else {
            if self
                .before
                .original_unit
                .as_ref()
                .is_none_or(|text| digest(text) != old.unit_sha256)
                || self.old_text.as_ref().map(|text| digest(text)) != old.drop_in_sha256
                || self.new_text.as_ref().map(|text| digest(text)) != new.drop_in_sha256
                || (self.after.managed
                    && self.new_text.as_deref()
                        != Some(binding::override_text(&self.after)?.as_str()))
            {
                return Err(invalid());
            }
        }
        Ok(())
    }
    fn verify_receipt(&self) -> Result<(), String> {
        let saved = super::saved_record()?.ok_or_else(invalid)?;
        if !same(&saved, &self.before)? && !same(&saved, &self.after)? {
            return Err(invalid());
        }
        Ok(())
    }
    fn verify_boundary(&self) -> Result<(), String> {
        let properties = systemd::show(&self.before.unit)?;
        if properties.get("MainPID").map(String::as_str) != Some("0")
            || !matches!(
                properties.get("ActiveState").map(String::as_str),
                Some("inactive" | "failed")
            )
            || systemd::socket_peer(&self.before.socket)?.is_some()
        {
            return Err(invalid());
        }
        let loaded = properties.get("LoadState").map(String::as_str);
        if !matches!(loaded, Some("loaded" | "not-found"))
            // Only a validated durable removal intent permits the unit to be
            // absent after reload. Ordinary missing registrations still fail
            // binding::verify before any intent can be created.
            || (loaded == Some("not-found") && self.before.installed && self.after.installed)
            || (loaded == Some("loaded")
                && properties.get("FragmentPath").map(PathBuf::from)
                    != Some(
                        self.before
                            .registration
                            .as_ref()
                            .ok_or_else(invalid)?
                            .fragment
                            .clone(),
                    ))
        {
            return Err(invalid());
        }
        let drop_in = binding::override_path(&self.before);
        let allowed = if self.before.owned_unit {
            None
        } else {
            Some(drop_in.as_path())
        };
        let directory = systemd::unit_directory().join(format!("{}.d", self.before.unit));
        match std::fs::symlink_metadata(&directory) {
            Ok(metadata) => {
                if metadata.file_type().is_symlink() || !metadata.is_dir() || self.before.owned_unit
                {
                    return Err(invalid());
                }
                for entry in std::fs::read_dir(&directory).map_err(|_| invalid())? {
                    if Some(entry.map_err(|_| invalid())?.path().as_path()) != allowed {
                        return Err(invalid());
                    }
                }
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(_) => return Err(invalid()),
        }
        let loaded_drop_ins = properties
            .get("DropInPaths")
            .map(String::as_str)
            .unwrap_or("");
        if !loaded_drop_ins.is_empty()
            && (self.before.owned_unit || Some(loaded_drop_ins) != drop_in.to_str())
        {
            return Err(invalid());
        }
        Ok(())
    }
    fn verify_disk(&self) -> Result<(), String> {
        self.validate()?;
        self.verify_receipt()?;
        self.verify_boundary()?;
        binding::verify_artifacts(&self.before)?;
        binding::verify_artifacts(&self.after)?;
        if !self.before.owned_unit {
            let old = self.before.registration.as_ref().ok_or_else(invalid)?;
            if binding::hash(&old.fragment, 512 * 1024)? != old.unit_sha256 {
                return Err(invalid());
            }
        }
        let actual = text_at(&self.changed_path())?;
        if actual != self.old_text && actual != self.new_text {
            return Err(invalid());
        }
        Ok(())
    }
    fn publish_text(&self, text: &Option<String>) -> Result<(), String> {
        self.verify_disk()?;
        let path = self.changed_path();
        if let Some(text) = text {
            super::write_bytes(&path, text.as_bytes())
        } else {
            if std::fs::symlink_metadata(&path).is_ok() {
                std::fs::remove_file(&path).map_err(|e| e.to_string())?;
                sync_directory(path.parent().ok_or_else(invalid)?)?;
            }
            Ok(())
        }
    }
    fn rollback_locked(&self) -> Result<(), String> {
        self.publish_text(&self.old_text)?;
        systemd::run("systemctl", &["--user", "daemon-reload"])?;
        let mut before = self.before.clone();
        binding::verify(&mut before)?;
        super::persist(&self.before)?;
        clear()
    }
}

pub(super) fn recover_locked() -> Result<(), String> {
    match std::fs::symlink_metadata(path()) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error.to_string()),
        Ok(_) => {}
    }
    let intent: Intent = super::read_json(&path())?;
    intent.verify_disk()?;
    let _daemon = stopped_daemon(&intent.before, false)?;
    intent.rollback_locked()
}

pub(super) fn write(target: &mut Record) -> Result<(), String> {
    if pending() {
        return Err("component_registration_recovery_required".into());
    }
    if !target.installed {
        return Err(invalid());
    }
    let mut before = super::saved_record()?.ok_or_else(invalid)?;
    binding::verify(&mut before)?;
    // The staged/retained candidate is independently authorized before taking
    // its snapshot. Never bless changed executable bytes by hashing them anew.
    if target.managed {
        super::verify_cached(target)?;
    } else {
        if target.registration.is_none() {
            return Err(invalid());
        }
        binding::verify_artifacts(target)?;
    }
    let after = Intent::planned_after(&before, target)?;
    let changed = if before.owned_unit {
        systemd::unit_directory().join(&before.unit)
    } else {
        binding::override_path(&before)
    };
    let intent = Intent {
        format: "cmsd-registration-transition-v1".into(),
        old_text: text_at(&changed)?,
        new_text: if after.owned_unit {
            Some(systemd::unit_text(&after)?)
        } else if after.managed {
            Some(binding::override_text(&after)?)
        } else {
            None
        },
        before,
        after,
    };
    apply(intent, target)
}

/// Delete only a currently verified Pinvou-owned unit. Reuse the registration
/// journal so a failed reload or interrupted worker restores its exact bytes
/// and receipt rather than stranding an installed receipt without a unit.
pub(super) fn remove(target: &mut Record) -> Result<(), String> {
    if pending() {
        return Err("component_registration_recovery_required".into());
    }
    let mut before = super::saved_record()?.ok_or_else(invalid)?;
    binding::verify(&mut before)?;
    if !same(&before, target)? {
        return Err(invalid());
    }
    let mut after = before.clone();
    after.installed = false;
    let after = Intent::planned_after(&before, &after)?;
    let intent = Intent {
        format: "cmsd-registration-transition-v1".into(),
        old_text: text_at(&systemd::unit_directory().join(&before.unit))?,
        new_text: None,
        before,
        after,
    };
    apply(intent, target)
}

fn apply(intent: Intent, target: &mut Record) -> Result<(), String> {
    intent.verify_disk()?;
    if serde_json::to_vec_pretty(&intent)
        .map_err(|_| invalid())?
        .len()
        > 512 * 1024
    {
        return Err(invalid());
    }
    let daemon = stopped_daemon(&intent.before, false)?;
    super::write_json(&path(), &intent)?;
    let result: Result<(), String> = (|| {
        intent.publish_text(&intent.new_text)?;
        systemd::run("systemctl", &["--user", "daemon-reload"])?;
        let mut after = intent.after.clone();
        binding::verify(&mut after)?;
        super::persist(&intent.after)?;
        clear()?;
        *target = intent.after.clone();
        Ok(())
    })();
    drop(daemon);
    if let Err(error) = result {
        // Clearing the directory entry can itself fail to sync. Re-establish
        // the same exact intent before compensation instead of losing its proof.
        let recovery = if pending() {
            recover_locked()
        } else {
            intent
                .verify_disk()
                .and_then(|()| super::write_json(&path(), &intent))
                .and_then(|()| recover_locked())
        };
        return Err(format!(
            "registration_failed: {error}; registration_restore={}",
            recovery.err().unwrap_or_else(|| "succeeded".into())
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::os::unix::fs::{FileTypeExt, PermissionsExt};

    fn intent(directory: &Path, external: bool) -> Intent {
        let config = directory.join("cmsd.toml");
        let state = directory.join("state");
        let socket = directory.join("cmsd.sock");
        std::fs::write(
            &config,
            format!("socket_path = {socket:?}\nstate_dir = {state:?}\nsystemd_mode = \"user\"\n"),
        )
        .unwrap();
        let mut before: Record = serde_json::from_value(json!({
            "unit":"pinvou-cmsd-registration-test.service","socket":socket,"state":state,
            "config":config,"binary":directory.join("old-cmsd"),"version":"0.1.0",
            "owned_unit":!external,"managed":!external,"installed":true,
            "original_unit":null,"previous":null
        }))
        .unwrap();
        std::fs::write(&before.binary, "old executable").unwrap();
        for path in [&before.config, &before.binary] {
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        let old_unit = systemd::unit_text(&before).unwrap();
        if external {
            before.original_unit = Some(old_unit.clone());
        }
        before.registration = Some(binding::Registration {
            fragment: systemd::unit_directory().join(&before.unit),
            unit_sha256: digest(&old_unit),
            config_sha256: binding::hash(&before.config, 512 * 1024).unwrap(),
            binary_sha256: binding::hash(&before.binary, 512 * 1024).unwrap(),
            drop_in_sha256: None,
        });
        let mut target = before.clone();
        target.binary = directory.join("new-cmsd");
        target.managed = true;
        std::fs::write(&target.binary, "new executable").unwrap();
        for path in [&before.config, &before.binary, &target.binary] {
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        let after = Intent::planned_after(&before, &target).unwrap();
        Intent {
            format: "cmsd-registration-transition-v1".into(),
            old_text: if external { None } else { Some(old_unit) },
            new_text: Some(if external {
                binding::override_text(&after).unwrap()
            } else {
                systemd::unit_text(&after).unwrap()
            }),
            before,
            after,
        }
    }

    #[test]
    fn journal_authorizes_only_prepared_unit_and_drop_in_bytes() {
        for external in [false, true] {
            let directory = tempfile::tempdir().unwrap();
            let intent = intent(directory.path(), external);
            intent.validate().unwrap();
            let mut foreign = intent.clone();
            foreign.new_text = Some("foreign replacement".into());
            assert!(foreign.validate().is_err());
            let mut foreign = intent.clone();
            foreign.old_text = Some("foreign old registration".into());
            assert!(foreign.validate().is_err());
            let mut foreign = intent.clone();
            foreign.after.registration.as_mut().unwrap().unit_sha256 = "unplanned digest".into();
            assert!(foreign.validate().is_err());
            let mut foreign = intent.clone();
            foreign.after.state = directory.path().join("different-state");
            assert!(foreign.validate().is_err());
            let mut foreign = intent.clone();
            foreign.after.registration.as_mut().unwrap().binary_sha256 =
                "unplanned executable".into();
            assert!(foreign.validate().is_err());
            if external {
                let mut foreign = intent;
                foreign.before.original_unit = Some("foreign original unit".into());
                assert!(foreign.validate().is_err());
            }
        }
    }

    #[test]
    fn owned_unit_removal_changes_only_installed_and_retains_restore_evidence() {
        let directory = tempfile::tempdir().unwrap();
        let previous = intent(directory.path(), false);
        let mut after = previous.before.clone();
        after.installed = false;
        let removal = Intent {
            format: previous.format,
            before: previous.before,
            after,
            old_text: previous.old_text,
            new_text: None,
        };
        removal.validate().unwrap();
        assert!(
            same(&removal.before, &{
                let mut restored = removal.after.clone();
                restored.installed = true;
                restored
            })
            .unwrap()
        );

        let mut foreign = removal.clone();
        foreign.old_text = None;
        assert!(
            foreign.validate().is_err(),
            "absence is not removal authority"
        );
        let mut foreign = removal.clone();
        foreign.new_text = removal.old_text.clone();
        assert!(foreign.validate().is_err());
        for (field, value) in [
            ("version", json!("different")),
            ("managed", json!(false)),
            ("maintenance_owner", json!("another-owner")),
            ("pending_install", json!(true)),
            ("previous", serde_json::to_value(&removal.before).unwrap()),
        ] {
            let mut foreign = removal.clone();
            let mut after = serde_json::to_value(&foreign.after).unwrap();
            after[field] = value;
            foreign.after = serde_json::from_value(after).unwrap();
            assert!(foreign.validate().is_err(), "cannot change {field}");
        }
        let mut foreign = removal.clone();
        foreign.after.registration.as_mut().unwrap().binary_sha256 = "newly-blessed".into();
        assert!(foreign.validate().is_err());
        let mut foreign = removal;
        foreign.before.installed = false;
        assert!(foreign.validate().is_err());

        let directory = tempfile::tempdir().unwrap();
        let mut external = intent(directory.path(), true);
        external.after = external.before.clone();
        external.after.installed = false;
        external.new_text = None;
        assert!(
            external.validate().is_err(),
            "external units cannot be removed"
        );
    }

    #[test]
    fn a_stopped_manager_cannot_authorize_a_live_or_linked_daemon_lock() {
        let directory = tempfile::tempdir().unwrap();
        let mut record = intent(directory.path(), false).before;
        record.state = directory.path().join("state");
        std::fs::create_dir(&record.state).unwrap();
        let path = record.state.join("daemon.lock");
        std::fs::write(&path, b"").unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        let guard = stopped_daemon(&record, false).unwrap();
        assert_eq!(
            stopped_daemon(&record, false).unwrap_err(),
            "service_still_running"
        );
        drop(guard);
        drop(stopped_daemon(&record, false).unwrap());
        std::fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink(directory.path().join("old-cmsd"), &path).unwrap();
        assert!(stopped_daemon(&record, false).is_err());
    }

    #[test]
    fn offline_lock_creation_rejects_special_or_writable_existing_files_without_repair() {
        let directory = tempfile::tempdir().unwrap();
        let mut record = intent(directory.path(), false).before;
        record.state = directory.path().join("state");
        std::fs::create_dir(&record.state).unwrap();
        let path = record.state.join("daemon.lock");
        assert!(stopped_daemon(&record, false).is_err());
        assert!(!path.exists());
        drop(stopped_daemon(&record, true).unwrap());
        assert!(std::fs::metadata(&path).unwrap().is_file());
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        drop(stopped_daemon(&record, true).unwrap());
        assert_eq!(
            std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o644
        );
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o620)).unwrap();
        assert!(stopped_daemon(&record, true).is_err());
        assert_eq!(
            std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o620
        );
        std::fs::remove_file(&path).unwrap();
        let name = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
        // SAFETY: name is a live terminated path and mode is valid for mkfifo.
        assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
        assert!(stopped_daemon(&record, true).is_err());
        assert!(
            std::fs::symlink_metadata(&path)
                .unwrap()
                .file_type()
                .is_fifo()
        );
    }

    #[test]
    fn registration_snapshot_rejects_linked_and_unbounded_files() {
        let directory = tempfile::tempdir().unwrap();
        let file = directory.path().join("unit");
        assert!(text_at(&file).unwrap().is_none());
        std::fs::write(&file, "authorized unit").unwrap();
        std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o600)).unwrap();
        assert_eq!(text_at(&file).unwrap().as_deref(), Some("authorized unit"));
        let link = directory.path().join("linked-unit");
        std::os::unix::fs::symlink(&file, &link).unwrap();
        assert!(text_at(&link).is_err());
        let opened = std::fs::OpenOptions::new().write(true).open(&file).unwrap();
        opened.set_len(512 * 1024 + 1).unwrap();
        assert!(text_at(&file).is_err());
    }
}
