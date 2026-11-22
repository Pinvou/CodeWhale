//! Component switching and compensation. Callers hold the shared UID/profile
//! locks. Restore executable/registration only; never replay a state snapshot.
use serde::{Deserialize, Serialize};

use super::{Job, Record, binding, systemd};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub(super) struct Recovery {
    pub status: String,
    pub previous_state: String,
    pub error: Option<String>,
}

fn state(record: &Record, was_running: bool) -> &'static str {
    if !record.installed {
        "uninstalled"
    } else if was_running {
        "running"
    } else {
        "stopped"
    }
}

fn ensure_fence(record: &mut Record, job: &Job) -> Result<(), String> {
    let path = record.state.join("component-maintenance.json");
    if std::fs::symlink_metadata(&path).is_ok() {
        let value: serde_json::Value = super::read_json(&path)?;
        if record.maintenance_owner.as_deref().is_some_and(|owner| {
            value.get("owner").and_then(serde_json::Value::as_str) == Some(owner)
        }) {
            return Ok(());
        }
        return Err("maintenance_owned".into());
    }
    // Readiness may have succeeded before a later persistence failure. Reacquire
    // admission before stopping that candidate; new tasks must block recovery.
    super::begin_maintenance(record, job)
}

pub(super) fn compensate(
    job: &mut Job,
    mut before: Record,
    published: &mut Record,
    was_running: bool,
) -> Result<(), String> {
    let previous_state = state(&before, was_running).to_owned();
    let _ = super::stage(job, "restoring_previous");
    let result = (|| {
        if !binding::same_target(&before, published) {
            return Err("rollback_binding_mismatch".into());
        }
        binding::verify(published)?;
        super::verify_cached(&before)?;
        binding::verify_artifacts(&before)?;
        ensure_fence(published, job)?;
        before.maintenance_owner = published.maintenance_owner.clone();
        if published.installed {
            systemd::control("stop", &published.unit)?;
            // Keep stopped compensation inactive even when stop retained a
            // failed state. Later explicit activation also clears counters that
            // can remain while the manager already reports inactive.
            if systemd::show(&published.unit)?
                .get("ActiveState")
                .is_some_and(|state| state == "failed")
            {
                systemd::reset_failed_verified(published)?;
            }
        }
        if before.installed {
            super::write_registration(&mut before)?;
            // The owned registration is now published. Keep its recovery
            // receipt accurate even if writing the reader fails next.
            super::persist(&before)?;
            super::write_store_reader(&before)?;
            if was_running {
                systemd::start_verified(&before)?;
                systemd::wait_ready(&before)?;
                super::end_maintenance(&mut before)?;
            } else {
                super::end_stopped_maintenance(&mut before)?;
                let properties = systemd::show(&before.unit)?;
                if properties.get("ActiveState").map(String::as_str) != Some("inactive")
                    || properties.get("MainPID").map(String::as_str) != Some("0")
                {
                    return Err("previous_component_not_stopped".into());
                }
            }
            binding::verify(&mut before)?;
        } else {
            if !before.owned_unit {
                return Err("external_installation".into());
            }
            let unit = systemd::unit_directory().join(&before.unit);
            let registration_present = match std::fs::symlink_metadata(&unit) {
                Ok(_) => true,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => false,
                Err(error) => return Err(error.to_string()),
            };
            if registration_present {
                if !published.installed && !published.pending_install {
                    return Err("service_configuration_changed: unexpected registration during compensation".into());
                }
                // Restore the verified old software through its own journal
                // before removing that exact registration. Removal authorizes
                // only installed=false, never an unrelated receipt replacement.
                before.installed = true;
                super::write_registration(&mut before)?;
                super::registration::remove(&mut before)?;
            } else {
                if published.installed {
                    return Err("service_configuration_changed: registration disappeared during compensation".into());
                }
                // A failed registration may already have restored absence.
                // Preserve that valid absent/pending preparation; do not create
                // another unit just to remove it during compensation.
                binding::verify(&mut before)?;
            }
            super::end_stopped_maintenance(&mut before)?;
            binding::verify(&mut before)?;
        }
        *published = before;
        Ok(())
    })();
    job.recovery = Some(Recovery {
        status: if result.is_ok() {
            "succeeded"
        } else {
            "failed"
        }
        .into(),
        previous_state,
        error: result.as_ref().err().cloned(),
    });
    let _ = super::save_job(job);
    result
}

pub(super) fn switch_to_saved(
    job: &mut Job,
    record: &mut Record,
    mut previous: Record,
) -> Result<(), String> {
    if !binding::same_target(&previous, record) {
        return Err("rollback_binding_mismatch".into());
    }
    super::verify_cached(&previous)?;
    binding::verify_artifacts(&previous)?;
    if previous.bootstrap_import.is_none() {
        previous.bootstrap_import = record.bootstrap_import.clone();
    }
    let was_running = systemd::show(&record.unit)?
        .get("ActiveState")
        .is_some_and(|value| value == "active");
    super::archive_state(record, job)?;
    super::begin_maintenance(record, job)?;
    let before = record.clone();
    let mut published = before.clone();
    previous.maintenance_owner = record.maintenance_owner.clone();
    let result = (|| {
        super::stage(job, "stopping")?;
        if record.installed {
            systemd::control("stop", &record.unit)?;
        }
        previous.installed = true;
        super::stage(job, "installing")?;
        super::write_registration(&mut previous)?;
        published = previous.clone();
        super::persist(&previous)?;
        super::write_store_reader(&previous)?;
        super::stage(job, "starting")?;
        systemd::start_verified(&previous)?;
        systemd::wait_ready(&previous)?;
        super::end_maintenance(&mut previous)?;
        *record = previous;
        Ok::<(), String>(())
    })();
    if let Err(error) = result {
        let recovery = compensate(job, before, &mut published, was_running);
        return Err(format!(
            "{}_failed: {error}; previous_component_restore={}",
            job.action,
            recovery.err().unwrap_or_else(|| "succeeded".into())
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn recovery_does_not_claim_another_owner_or_follow_a_linked_fence() {
        let directory = tempfile::tempdir().unwrap();
        let mut record: Record = serde_json::from_value(serde_json::json!({
            "unit":"cmsd-test.service","socket":directory.path().join("sock"),"state":directory.path(),
            "config":directory.path().join("config"),"binary":directory.path().join("cmsd"),"version":"0.1.0",
            "owned_unit":true,"managed":false,"installed":true,"original_unit":null,"previous":null,"maintenance_owner":"this-owner"
        })).unwrap();
        let job: Job = serde_json::from_value(serde_json::json!({"id":"component-test","action":"rollback","package":null,
            "status":"running","stage":"starting","created_at":1,"finished_at":null,"pid":null,"start_ticks":null,"boot_id":"test","error":null})).unwrap();
        let path = directory.path().join("component-maintenance.json");
        super::super::write_json(&path, &serde_json::json!({"owner":"another-owner"})).unwrap();
        assert!(ensure_fence(&mut record, &job).is_err());
        super::super::write_json(&path, &serde_json::json!({"owner":"this-owner"})).unwrap();
        assert!(ensure_fence(&mut record, &job).is_ok());
        std::fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink(directory.path().join("missing"), &path).unwrap();
        assert!(ensure_fence(&mut record, &job).is_err());
    }
}
