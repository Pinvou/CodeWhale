//! Durable monotonic admission version shared by legacy and future v2 writes.
//! A separate marker survives old binaries dropping unknown state fields. This
//! is local rollback detection, not a hardware anti-rollback counter.
use crate::app::{api_err, ApiErr, App};
use crate::private_storage as storage;
use crate::store::StateStore;
use serde_json::{json, Value};
use std::sync::atomic::Ordering;

// The v1 state codec uses f64. Never publish an inexact protocol integer.
pub const MAX_REVISION: u64 = (1u64 << 53) - 1;

#[derive(Debug)]
enum RevisionError {
    IdentityBusy,
    Unavailable(String),
}

impl From<String> for RevisionError {
    fn from(message: String) -> Self {
        Self::Unavailable(message)
    }
}

impl From<&str> for RevisionError {
    fn from(message: &str) -> Self {
        Self::Unavailable(message.into())
    }
}

fn device_id(app: &App) -> Result<String, RevisionError> {
    crate::device::record(app)
        .map_err(|(status, code, message)| {
            if status == 503 && code == "identity_busy" {
                RevisionError::IdentityBusy
            } else {
                RevisionError::Unavailable(message)
            }
        })?
        .get("device_id")
        .and_then(crate::json::Value::as_str)
        .map(str::to_owned)
        .ok_or_else(|| "control device identity missing".into())
}

fn read(app: &App, device: &str) -> Result<Option<u64>, String> {
    let Some(raw) = storage::read(&app.config.state_dir.join("lifecycle-revision.json"))? else {
        return Ok(None);
    };
    let text = std::str::from_utf8(&raw).map_err(|_| "control version UTF-8 invalid")?;
    crate::json::Value::parse(text).map_err(|_| "control version JSON invalid")?;
    let value: Value = serde_json::from_slice(&raw).map_err(|_| "control version JSON invalid")?;
    if value.as_object().is_none_or(|v| v.len() != 3)
        || value["format"] != "megacube-lifecycle-revision-v1"
        || value["device_id"] != device
    {
        return Err("control version identity or schema mismatch".into());
    }
    value["revision"]
        .as_u64()
        .filter(|v| *v <= MAX_REVISION)
        .map(Some)
        .ok_or_else(|| "control version is not a safe integer".into())
}

fn publish(app: &App, device: &str, epoch: u64, replace: bool) -> Result<(), String> {
    let raw = serde_jcs::to_vec(&json!({"format":"megacube-lifecycle-revision-v1",
        "device_id":device,"revision":epoch}))
    .map_err(|_| "control version encoding failed")?;
    storage::publish(
        &app.config.state_dir.join("lifecycle-revision.json"),
        &raw,
        replace,
    )
}

fn next(epoch: u64) -> Result<u64, String> {
    epoch
        .checked_add(1)
        .filter(|v| *v <= MAX_REVISION)
        .ok_or_else(|| {
            "control version exhausted; retain state and request a version migration".into()
        })
}

fn initialize_locked(app: &App, store: &mut StateStore) -> Result<(), RevisionError> {
    storage::secure_state(&app.config.state_dir)?;
    let device = device_id(app)?;
    let marker = read(app, &device)?;
    let saved = store.state.lifecycle_revision;
    let epoch = match (marker, saved) {
        (None, None) => 0,
        (None, Some(_)) => return Err(
            "control version marker missing; restore the matching backup without deleting state"
                .into(),
        ),
        (Some(marker), Some(saved)) if marker == saved => {
            app.lifecycle_epoch.store(marker, Ordering::Release);
            return Ok(());
        }
        // An old writer, or a crash between marker and state publication, must
        // invalidate the old expected version even if no task was completed.
        (Some(marker), saved) => next(marker.max(saved.unwrap_or(0)))?,
    };
    publish(app, &device, epoch, marker.is_some())?;
    store.state.lifecycle_revision = Some(epoch);
    store.mark_dirty();
    store.save_if_dirty().map_err(|e| e.to_string())?;
    app.lifecycle_epoch.store(epoch, Ordering::Release);
    Ok(())
}

fn fence(app: &App, result: Result<(), RevisionError>) -> Result<(), ApiErr> {
    result.map_err(|error| {
        let message = match error {
            RevisionError::IdentityBusy => {
                return api_err(503, "identity_busy", "设备身份正在读取，请稍后重试");
            }
            RevisionError::Unavailable(message) => message,
        };
        let mut error = app
            .lifecycle_revision_error
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        if error.is_none() {
            *error = Some(message.clone());
        }
        api_err(503, "lifecycle_version_unavailable", &message)
    })
}

/// Called before recovery workers start. Identity contention requires startup
/// to retry; other failures fence mutations while retaining discovery/rescue.
pub fn initialize(app: &App) -> Result<(), ApiErr> {
    let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    fence(app, initialize_locked(app, &mut store))
}

/// Caller holds the same Store lock used by every v1 task admission. Gaps are
/// intentional: the marker is fsynced before accepting any external effect.
pub fn reserve(app: &App, store: &mut StateStore) -> Result<(), ApiErr> {
    if let Some(message) = app
        .lifecycle_revision_error
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .clone()
    {
        return Err(api_err(503, "lifecycle_version_unavailable", &message));
    }
    let result = (|| {
        if store.state.lifecycle_revision.is_none() {
            initialize_locked(app, store)?;
        }
        let current = store
            .state
            .lifecycle_revision
            .ok_or("control version uninitialized")?;
        let device = device_id(app)?;
        if read(app, &device)? != Some(current) {
            return Err(
                "control version changed or disappeared; retain state and restart to reconcile"
                    .into(),
            );
        }
        let epoch = next(current)?;
        publish(app, &device, epoch, true)?;
        store.state.lifecycle_revision = Some(epoch);
        store.mark_dirty();
        app.lifecycle_epoch.store(epoch, Ordering::Release);
        Ok(())
    })();
    fence(app, result)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::docker::fixtures as fx;
    use std::sync::{Arc, Barrier};

    fn reopen(app: &App, store: StateStore) -> App {
        let config = app.config.clone();
        let systemd = crate::systemd::Systemd::new(
            config.systemd_mode,
            config.unit_dir.clone(),
            config.env_dir(),
            "test".to_owned(),
        );
        App::new(config, store, systemd)
    }

    #[test]
    fn admission_is_durable_and_concurrent_requests_spend_once() {
        let root = fx::tmpdir("epoch-admission");
        let app = fx::make_app(&root, 128, 100);
        initialize(&app).unwrap();
        let barrier = Arc::new(Barrier::new(12));
        let jobs: Vec<_> = (0..12)
            .map(|_| {
                let app = app.clone();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    barrier.wait();
                    app.task_create_exclusive("enable", "test-card")
                })
            })
            .collect();
        let accepted = jobs
            .into_iter()
            .filter_map(|j| j.join().unwrap().ok())
            .collect::<Vec<_>>();
        assert_eq!(accepted.len(), 1);
        assert_eq!(app.lifecycle_epoch.load(Ordering::Acquire), 1);
        let loaded = StateStore::try_load(&app.config.state_file()).unwrap();
        assert_eq!(loaded.state.lifecycle_revision, Some(1));
        assert!(loaded.state.tasks.contains_key(&accepted[0]));
        let reopened = reopen(&app, loaded);
        initialize(&reopened).unwrap();
        assert_eq!(reopened.lifecycle_epoch.load(Ordering::Acquire), 1);
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn old_writer_or_interrupted_admission_invalidates_previous_version() {
        let root = fx::tmpdir("epoch-legacy");
        let app = fx::make_app(&root, 128, 100);
        initialize(&app).unwrap();
        let id = app.task_create_exclusive("enable", "test-card").unwrap();
        app.task_finish(&id, Ok("test".into()));
        let mut loaded = StateStore::try_load(&app.config.state_file()).unwrap();
        // Simulate the actual legacy codec which drops unknown fields.
        loaded.state.lifecycle_revision = None;
        loaded.mark_dirty();
        loaded.save().unwrap();
        let reopened = reopen(&app, loaded);
        initialize(&reopened).unwrap();
        assert_eq!(reopened.lifecycle_epoch.load(Ordering::Acquire), 2);
        // Marker publication succeeded, task/state publication did not.
        publish(&reopened, &device_id(&reopened).unwrap(), 3, true).unwrap();
        let loaded = StateStore::try_load(&reopened.config.state_file()).unwrap();
        let reopened = reopen(&reopened, loaded);
        initialize(&reopened).unwrap();
        assert_eq!(reopened.lifecycle_epoch.load(Ordering::Acquire), 4);
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn corrupt_missing_or_linked_marker_never_admits_a_task() {
        for mode in ["corrupt", "missing", "link"] {
            let root = fx::tmpdir(&format!("epoch-{mode}"));
            let app = fx::make_app(&root, 128, 100);
            initialize(&app).unwrap();
            let path = app.config.state_dir.join("lifecycle-revision.json");
            let original = std::fs::read(&path).unwrap();
            std::fs::remove_file(&path).unwrap();
            match mode {
                "corrupt" => {
                    std::fs::write(&path, b"broken").unwrap();
                }
                "link" => {
                    std::os::unix::fs::symlink("device-identity.json", &path).unwrap();
                }
                _ => {}
            }
            assert_eq!(
                app.task_create_exclusive("disable", "test-card")
                    .unwrap_err()
                    .1,
                "lifecycle_version_unavailable"
            );
            assert!(app.store.lock().unwrap().state.tasks.is_empty());
            assert!(app.lifecycle_revision_error.lock().unwrap().is_some());
            // In-process edits cannot silently reopen admission.
            std::fs::remove_file(&path).ok();
            std::fs::write(&path, original).unwrap();
            assert!(app.task_create_install("test-card").is_err());
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn exhaustion_or_state_save_failure_never_accepts_a_task() {
        let root = fx::tmpdir("epoch-exhausted");
        let app = fx::make_app(&root, 128, 100);
        initialize(&app).unwrap();
        publish(&app, &device_id(&app).unwrap(), MAX_REVISION, true).unwrap();
        app.store.lock().unwrap().state.lifecycle_revision = Some(MAX_REVISION);
        assert!(app.task_create_exclusive("enable", "test-card").is_err());
        assert!(app.store.lock().unwrap().state.tasks.is_empty());
        std::fs::remove_dir_all(root).unwrap();
        let root = fx::tmpdir("epoch-save-fail");
        let app = fx::make_app(&root, 128, 100);
        initialize(&app).unwrap();
        let path = app.config.state_file();
        std::fs::remove_file(&path).unwrap();
        std::fs::create_dir(&path).unwrap();
        assert_eq!(
            app.task_create_install("test-card").unwrap_err().1,
            "task_persist_failed"
        );
        assert!(app.store.lock().unwrap().state.tasks.is_empty());
        assert_eq!(read(&app, &device_id(&app).unwrap()).unwrap(), Some(1));
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[cfg(test)]
mod identity_contention_tests {
    use super::*;
    use crate::docker::fixtures as fx;
    use std::sync::atomic::{AtomicBool, AtomicUsize};
    use std::sync::{Arc, Barrier};

    #[test]
    fn busy_admission_retains_version_and_recovers_without_restart() {
        let root = fx::tmpdir("epoch-identity-busy");
        let app = fx::make_app(&root, 128, 100);
        initialize(&app).unwrap();
        let marker = app.config.state_dir.join("lifecycle-revision.json");
        let original = std::fs::read(&marker).unwrap();
        let state = std::fs::read(app.config.state_file()).unwrap();
        let lock = storage::lock(&app.config.state_dir.join("device-identity.lock")).unwrap();
        assert_eq!(
            crate::device::inventory(&app).unwrap_err().1,
            "identity_busy"
        );
        let error = reserve(&app, &mut app.store.lock().unwrap()).unwrap_err();
        assert_eq!((error.0, error.1.as_str()), (503, "identity_busy"));
        assert!(app.lifecycle_revision_error.lock().unwrap().is_none());
        assert_eq!(app.lifecycle_epoch.load(Ordering::Acquire), 0);
        assert_eq!(std::fs::read(&marker).unwrap(), original);
        assert_eq!(std::fs::read(app.config.state_file()).unwrap(), state);
        assert!(app.store.lock().unwrap().state.tasks.is_empty());
        drop(lock);
        crate::device::inventory(&app).unwrap();
        reserve(&app, &mut app.store.lock().unwrap()).unwrap();
        assert_eq!(app.lifecycle_epoch.load(Ordering::Acquire), 1);
        assert!(app.lifecycle_revision_error.lock().unwrap().is_none());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn real_inventory_contention_is_retryable_and_does_not_poison_admission() {
        let root = fx::tmpdir("epoch-natural-identity-contention");
        let app = fx::make_app(&root, 128, 100);
        initialize(&app).unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let reads = Arc::new(AtomicUsize::new(0));
        let barrier = Arc::new(Barrier::new(2));
        let reader = {
            let app = app.clone();
            let stop = stop.clone();
            let reads = reads.clone();
            let barrier = barrier.clone();
            std::thread::spawn(move || {
                barrier.wait();
                while !stop.load(Ordering::Relaxed) {
                    let _ = crate::device::inventory(&app);
                    reads.fetch_add(1, Ordering::Relaxed);
                }
            })
        };
        barrier.wait();
        let mut observed = None;
        let mut admitted = 0;
        for _ in 0..1000 {
            let mut store = app.store.lock().unwrap();
            if let Err(error) = reserve(&app, &mut store) {
                observed = Some(error);
                break;
            }
            store.save_if_dirty().unwrap();
            admitted += 1;
        }
        stop.store(true, Ordering::Relaxed);
        reader.join().unwrap();
        // A legal scheduler may produce no overlap. The held-lock test forces
        // Busy; this test checks actual failures and recovery without requiring
        // a particular scheduling outcome.
        eprintln!(
            "inventory/reserve samples: reads={}, admitted={}, busy={}",
            reads.load(Ordering::Relaxed),
            admitted,
            usize::from(observed.is_some())
        );
        if let Some(error) = observed {
            assert_eq!((error.0, error.1.as_str()), (503, "identity_busy"));
        }
        assert!(app.lifecycle_revision_error.lock().unwrap().is_none());
        crate::device::inventory(&app).unwrap();
        let before = app.lifecycle_epoch.load(Ordering::Acquire);
        reserve(&app, &mut app.store.lock().unwrap()).unwrap();
        assert_eq!(app.lifecycle_epoch.load(Ordering::Acquire), before + 1);
        assert!(app.lifecycle_revision_error.lock().unwrap().is_none());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn busy_initialization_preserves_fresh_saved_and_legacy_state_for_startup_retry() {
        for mode in ["fresh", "saved", "legacy"] {
            let root = fx::tmpdir(&format!("epoch-startup-busy-{mode}"));
            let app = fx::make_app(&root, 128, 100);
            storage::secure_state(&app.config.state_dir).unwrap();
            if mode != "fresh" {
                initialize(&app).unwrap();
                if mode == "legacy" {
                    let mut store = app.store.lock().unwrap();
                    store.state.lifecycle_revision = None;
                    store.mark_dirty();
                    store.save().unwrap();
                }
            }
            let saved = app.store.lock().unwrap().state.lifecycle_revision;
            let marker = app.config.state_dir.join("lifecycle-revision.json");
            let original_marker = std::fs::read(&marker).ok();
            let original_state = std::fs::read(app.config.state_file()).ok();
            let lock = storage::lock(&app.config.state_dir.join("device-identity.lock")).unwrap();
            let error = initialize(&app).unwrap_err();
            assert_eq!((error.0, error.1.as_str()), (503, "identity_busy"));
            assert!(app.lifecycle_revision_error.lock().unwrap().is_none());
            assert_eq!(app.store.lock().unwrap().state.lifecycle_revision, saved);
            assert_eq!(std::fs::read(&marker).ok(), original_marker);
            assert_eq!(std::fs::read(app.config.state_file()).ok(), original_state);
            drop(lock);
            // The startup caller retries initialization before recovery/acceptance.
            initialize(&app).unwrap();
            let expected = u64::from(mode == "legacy");
            assert_eq!(app.lifecycle_epoch.load(Ordering::Acquire), expected);
            reserve(&app, &mut app.store.lock().unwrap()).unwrap();
            assert_eq!(app.lifecycle_epoch.load(Ordering::Acquire), expected + 1);
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn identity_and_marker_faults_still_latch_after_files_are_restored() {
        use std::os::unix::fs::PermissionsExt;
        for mode in [
            "identity-corrupt",
            "identity-missing",
            "identity-machine",
            "identity-id",
            "marker-corrupt",
            "marker-missing",
            "marker-device",
            "lock-permissions",
        ] {
            let root = fx::tmpdir(&format!("epoch-permanent-{mode}"));
            let app = fx::make_app(&root, 128, 100);
            initialize(&app).unwrap();
            let identity = app.config.state_dir.join("device-identity.json");
            let marker = app.config.state_dir.join("lifecycle-revision.json");
            let lock = app.config.state_dir.join("device-identity.lock");
            let identity_before = std::fs::read(&identity).unwrap();
            let marker_before = std::fs::read(&marker).unwrap();
            match mode {
                "identity-corrupt" => std::fs::write(&identity, b"broken").unwrap(),
                "identity-missing" => std::fs::remove_file(&identity).unwrap(),
                "identity-machine" | "identity-id" => {
                    let mut value: Value = serde_json::from_slice(&identity_before).unwrap();
                    value[if mode == "identity-machine" {
                        "machine"
                    } else {
                        "device_id"
                    }] = "changed".into();
                    std::fs::write(&identity, value.to_string()).unwrap();
                }
                "marker-corrupt" => std::fs::write(&marker, b"broken").unwrap(),
                "marker-missing" => std::fs::remove_file(&marker).unwrap(),
                "marker-device" => {
                    let mut value: Value = serde_json::from_slice(&marker_before).unwrap();
                    value["device_id"] = "dev-other".into();
                    std::fs::write(&marker, value.to_string()).unwrap();
                }
                "lock-permissions" => {
                    std::fs::set_permissions(&lock, std::fs::Permissions::from_mode(0o644))
                        .unwrap();
                }
                _ => unreachable!(),
            }
            let error = reserve(&app, &mut app.store.lock().unwrap()).unwrap_err();
            assert_eq!(
                (error.0, error.1.as_str()),
                (503, "lifecycle_version_unavailable"),
                "{mode}"
            );
            assert!(
                app.lifecycle_revision_error.lock().unwrap().is_some(),
                "{mode}"
            );
            assert_eq!(app.lifecycle_epoch.load(Ordering::Acquire), 0, "{mode}");
            assert!(app.store.lock().unwrap().state.tasks.is_empty());
            std::fs::write(&identity, &identity_before).unwrap();
            std::fs::set_permissions(&identity, std::fs::Permissions::from_mode(0o600)).unwrap();
            std::fs::write(&marker, &marker_before).unwrap();
            std::fs::set_permissions(&marker, std::fs::Permissions::from_mode(0o600)).unwrap();
            std::fs::set_permissions(&lock, std::fs::Permissions::from_mode(0o600)).unwrap();
            assert_eq!(
                reserve(&app, &mut app.store.lock().unwrap()).unwrap_err().1,
                "lifecycle_version_unavailable",
                "{mode}"
            );
            std::fs::remove_dir_all(root).unwrap();
        }
    }
}
