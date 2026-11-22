//! Durable completion of one exact owned configuration digest transition.
//! The journal authorizes no executable, unit, drop-in or state replacement.
use super::{Record, binding};
use serde::{Deserialize, Serialize};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Change {
    before: Record,
    after: Record,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct Intent {
    format: String,
    old_sha256: String,
    new_sha256: String,
    service: Change,
    backup: Option<Change>,
}

fn path() -> std::path::PathBuf {
    super::root().join("config-migration.json")
}
fn invalid() -> String {
    "component_config_migration_changed: inspect the retained configuration journal".into()
}
fn same(left: &Record, right: &Record) -> Result<bool, String> {
    Ok(serde_json::to_value(left).map_err(|_| invalid())?
        == serde_json::to_value(right).map_err(|_| invalid())?)
}
fn verify_saved(path: &std::path::Path, change: &Change, applied: bool) -> Result<(), String> {
    let saved: Record = super::read_json(path)?;
    if !same(&saved, &change.before)? && !(applied && same(&saved, &change.after)?) {
        return Err(invalid());
    }
    Ok(())
}

pub(super) fn pending() -> bool {
    std::fs::symlink_metadata(path()).is_ok()
}

impl Intent {
    pub(super) fn prepare(
        before: &Record,
        after: &Record,
        backup_before: Option<Record>,
        backup_after: Option<Record>,
        old_sha256: String,
        new_sha256: String,
    ) -> Result<Self, String> {
        let backup = match (backup_before, backup_after) {
            (Some(before), Some(after)) => Some(Change { before, after }),
            (None, None) => None,
            _ => return Err(invalid()),
        };
        let intent = Self {
            format: "cmsd-config-digest-migration-v1".into(),
            old_sha256,
            new_sha256,
            service: Change {
                before: before.clone(),
                after: after.clone(),
            },
            backup,
        };
        intent.validate()?;
        Ok(intent)
    }
    fn validate(&self) -> Result<(), String> {
        let valid_hash = |value: &str| {
            value.len() == 64
                && value
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        };
        if self.format != "cmsd-config-digest-migration-v1"
            || self.old_sha256 == self.new_sha256
            || !valid_hash(&self.old_sha256)
            || !valid_hash(&self.new_sha256)
        {
            return Err(invalid());
        }
        for change in std::iter::once(&self.service).chain(self.backup.iter()) {
            if !binding::same_target(&change.before, &self.service.before)
                || !same(
                    &binding::plan_config_revision(
                        &change.before,
                        &self.old_sha256,
                        &self.new_sha256,
                    )?,
                    &change.after,
                )?
            {
                return Err(invalid());
            }
        }
        Ok(())
    }
    pub(super) fn begin(&self) -> Result<(), String> {
        self.validate()?;
        if pending() || binding::config_sha256(&self.service.before)? != self.old_sha256 {
            return Err(invalid());
        }
        self.verify_receipts(false)?;
        let mut current = self.service.before.clone();
        binding::verify(&mut current)?;
        super::write_json(&path(), self)
    }
    fn verify_receipts(&self, applied: bool) -> Result<(), String> {
        verify_saved(&super::root().join("service.json"), &self.service, applied)?;
        let backup = super::root().join("backup.json");
        if let Some(change) = &self.backup {
            verify_saved(&backup, change, applied)?;
        } else if std::fs::symlink_metadata(backup).is_ok() {
            return Err(invalid());
        }
        Ok(())
    }
    pub(super) fn commit_locked(&self) -> Result<(), String> {
        self.validate()?;
        if binding::config_sha256(&self.service.after)? != self.new_sha256 {
            return Err(invalid());
        }
        self.verify_receipts(true)?;
        // Every executable/unit/drop-in digest is still the original digest.
        let mut current = self.service.after.clone();
        binding::verify(&mut current)?;
        super::persist(&self.service.after)?;
        if let Some(backup) = &self.backup {
            super::write_json(&super::root().join("backup.json"), &backup.after)?;
        }
        clear()
    }
}
fn clear() -> Result<(), String> {
    std::fs::remove_file(path()).map_err(|e| e.to_string())?;
    std::fs::File::open(super::root())
        .and_then(|file| file.sync_all())
        .map_err(|e| e.to_string())
}
pub(super) fn recover_locked() -> Result<(), String> {
    match std::fs::symlink_metadata(path()) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error.to_string()),
        Ok(_) => {}
    }
    let intent: Intent = super::read_json(&path())?;
    intent.validate()?;
    let actual = binding::config_sha256(&intent.service.before)?;
    if actual == intent.new_sha256 {
        intent.commit_locked()
    } else if actual == intent.old_sha256 {
        // The worker never applied the migration. Preserve the old receipt.
        intent.verify_receipts(false)?;
        let mut current = intent.service.before;
        binding::verify(&mut current)?;
        clear()
    } else {
        Err(invalid())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    fn receipt(path: &std::path::Path) -> Record {
        serde_json::from_value(json!({
            "unit":"pinvou-cmsd-migration-test.service","socket":path.join("sock"),
            "config":path.join("cmsd.toml"),"state":path.join("state"),
            "binary":path.join("cmsd"),"version":"0.1.0","owned_unit":true,
            "managed":true,"installed":true,"original_unit":null,"previous":null,
            "registration":{"fragment":path.join("unit"),"unit_sha256":"unit",
            "config_sha256":"0".repeat(64),"binary_sha256":"binary","drop_in_sha256":null}
        }))
        .unwrap()
    }
    fn intent(path: &std::path::Path) -> Intent {
        let before = receipt(path);
        let after =
            binding::plan_config_revision(&before, &"0".repeat(64), &"1".repeat(64)).unwrap();
        Intent::prepare(
            &before,
            &after,
            Some(before.clone()),
            Some(after.clone()),
            "0".repeat(64),
            "1".repeat(64),
        )
        .unwrap()
    }
    #[test]
    fn migration_intent_authorizes_only_the_exact_config_digest() {
        let root = tempfile::tempdir().unwrap();
        let intent = intent(root.path());
        intent.validate().unwrap();
        for field in ["unit_sha256", "binary_sha256", "drop_in_sha256"] {
            let mut invalid = intent.clone();
            let mut after = serde_json::to_value(&invalid.service.after).unwrap();
            after["registration"][field] = json!("replacement");
            invalid.service.after = serde_json::from_value(after).unwrap();
            assert!(invalid.validate().is_err(), "must not authorize {field}");
        }
        let mut invalid = intent.clone();
        invalid.service.after.binary = root.path().join("other");
        assert!(invalid.validate().is_err());
        let mut invalid = intent;
        invalid.backup.as_mut().unwrap().before.state = root.path().join("other-state");
        assert!(invalid.validate().is_err());
    }
    #[test]
    fn interrupted_receipt_commit_accepts_only_the_exact_before_or_after() {
        let root = tempfile::tempdir().unwrap();
        let intent = intent(root.path());
        let path = root.path().join("service.json");
        super::super::write_json(&path, &intent.service.before).unwrap();
        verify_saved(&path, &intent.service, false).unwrap();
        verify_saved(&path, &intent.service, true).unwrap();
        super::super::write_json(&path, &intent.service.after).unwrap();
        assert!(verify_saved(&path, &intent.service, false).is_err());
        verify_saved(&path, &intent.service, true).unwrap();
        let mut foreign = intent.service.before.clone();
        foreign.binary = root.path().join("foreign");
        super::super::write_json(&path, &foreign).unwrap();
        assert!(verify_saved(&path, &intent.service, true).is_err());
    }
}
