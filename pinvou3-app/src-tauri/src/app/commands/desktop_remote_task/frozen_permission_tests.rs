//! Real SessionStore + RemoteTaskService regressions for durable lane claims.
use super::*;
use crate::features::sessions::SerializableMode::Plan;
use crate::platform::prefs::{RemoteTaskPermissionPrefs, UserPrefs};

fn policy() -> RemoteTaskPermissionPrefs {
    RemoteTaskPermissionPrefs {
        chat: Some(Plan),
        code: Some(Plan),
    }
}

struct Provisioner {
    store: SessionStore,
    pending: parking_lot::Mutex<HashMap<String, Option<PendingRemoteMode>>>,
}

impl RemoteSessionProvisioner for Provisioner {
    fn create_authoritative_session(&self, _: &str, _: bool) -> Result<String, String> {
        Err("test_must_reuse_existing_session".into())
    }
    fn session_exists(&self, id: &str) -> Result<bool, String> {
        Ok(controlled_session_exists(&self.store, id))
    }
    fn claim_execution_lane(
        &self,
        id: &str,
        code: bool,
        _: Option<&std::path::Path>,
    ) -> Result<(), String> {
        freeze_first_remote_lane(&self.store, &self.pending, id, code, policy())?;
        // Match the successful production claim, not a test-only retained map.
        self.pending.lock().remove(id);
        Ok(())
    }
    fn forget_unclaimed_session(&self, id: &str) {
        self.pending.lock().remove(id);
    }
    fn resolve_code_workspace(&self, _: &str) -> Result<std::path::PathBuf, String> {
        Ok(self.store.manager.sessions_dir().to_path_buf())
    }
}

fn request(lease: &LeaseContext, code: bool) -> RemoteTaskRequest {
    let mode = if code { TASK_MODE_CODE } else { TASK_MODE_CHAT };
    let mut input = json!({ "text": "synthetic prompt", "attachment_ids": [], "skill_ids": [] });
    if code {
        input["workspace_id"] = json!("workspace-1");
    }
    RemoteTaskRequest::from_payload(&json!({
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "task_id": "synthetic-task", "client_request_id": "synthetic-request",
        "session_id": "remote-reference", "capability_revision": "1",
        "model_source": TASK_MODEL_SOURCE_CONTROLLED, "mode": mode,
        "input_digest": remote_task_input_digest("synthetic prompt", &[], &[]),
        "input": input,
    }))
    .unwrap()
}

fn lease(generation: &str) -> LeaseContext {
    LeaseContext {
        connection_id: "connection-1".into(),
        controller_lease_id: "lease-1".into(),
        lease_epoch: "1".into(),
        relay_epoch: "relay-1".into(),
        remote_control_generation: generation.into(),
    }
}

#[test]
fn successful_claim_removal_mapping_retirement_and_restart_cannot_change_lane() {
    for code in [false, true] {
        crate::features::sessions::with_isolated_store_restoring_home(|store| {
            let session = store
                .create_new(
                    "model".into(),
                    None,
                    store.manager.sessions_dir().join("local"),
                )
                .unwrap();
            let id = &session.metadata.id;
            let mut prefs = UserPrefs::default();
            prefs.remote_task_permissions = policy();
            prefs.save().unwrap();
            let provisioner = Arc::new(Provisioner {
                store: store.clone(),
                pending: parking_lot::Mutex::new(HashMap::from([(id.clone(), None)])),
            });
            let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
            service.set_session_provisioner(provisioner.clone());
            let first = lease("generation-1");
            service
                .switch_remote_session("remote-reference", id, &first.remote_control_generation)
                .unwrap();
            assert_eq!(
                service
                    .ensure_authenticated_task_session(&request(&first, code), &first, "test")
                    .unwrap(),
                *id
            );
            assert!(
                provisioner.pending.lock().is_empty(),
                "successful production-style claim removes pending state"
            );
            service.clear_sessions();
            assert_eq!(
                service.lane_for_session(id, &first.remote_control_generation),
                None
            );
            let second = lease("generation-2");
            service
                .switch_remote_session("remote-reference", id, &second.remote_control_generation)
                .unwrap();
            assert_eq!(
                service
                    .ensure_authenticated_task_session(&request(&second, !code), &second, "test")
                    .unwrap_err()
                    .code,
                "task_permission_mode_changed"
            );
            assert_eq!(
                service.lane_for_session(id, &second.remote_control_generation),
                Some(RemoteSessionLane::Unknown)
            );
            assert_eq!(
                service
                    .ensure_authenticated_task_session(&request(&second, code), &second, "test")
                    .unwrap(),
                *id
            );
            assert!(provisioner.pending.lock().is_empty());
            service.clear_sessions();

            let reopened = SessionStore::boot().unwrap();
            let fresh = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
            let fresh_provisioner = Arc::new(Provisioner {
                store: reopened,
                pending: parking_lot::Mutex::new(HashMap::new()),
            });
            fresh.set_session_provisioner(fresh_provisioner.clone());
            fresh
                .switch_remote_session("remote-reference", id, &second.remote_control_generation)
                .unwrap();
            if code {
                assert_eq!(
                    fresh
                        .claim_local_session(id, &second.remote_control_generation)
                        .unwrap_err()
                        .code,
                    "task_permission_mode_changed",
                    "B-local Unknown must not guess Chat for a Code fact"
                );
            } else {
                assert_eq!(
                    fresh
                        .claim_local_session(id, &second.remote_control_generation)
                        .unwrap()
                        .1,
                    RemoteSessionLane::Chat
                );
            }
            assert_eq!(
                fresh
                    .ensure_authenticated_task_session(&request(&second, code), &second, "test")
                    .unwrap(),
                *id
            );
            assert!(fresh_provisioner.pending.lock().is_empty());
            assert!(
                fresh.ledger().active_for_session(id).is_none(),
                "claiming never redispatches a task"
            );
        });
    }
}

#[test]
fn failed_first_write_consumes_initialization_permit_even_after_evidence_is_removed() {
    crate::features::sessions::with_isolated_store_restoring_home(|store| {
        let root = store.manager.sessions_dir();
        let session = store
            .create_new("model".into(), None, root.join("local"))
            .unwrap();
        let id = &session.metadata.id;
        let pending = parking_lot::Mutex::new(HashMap::from([(id.clone(), None)]));
        let mode_file = root.join("_session_mode_states.json");
        std::fs::create_dir(&mode_file).unwrap();
        assert_eq!(
            freeze_first_remote_lane(store, &pending, id, true, policy()).unwrap_err(),
            "task_session_backend_bind_failed"
        );
        assert_eq!(pending.lock().get(id), Some(&Some((true, Plan))));
        let fact = root.join(id).join("remote-task-permission.json");
        std::fs::remove_file(&fact).unwrap();
        std::fs::remove_dir(&mode_file).unwrap();
        for code in [true, false] {
            assert_eq!(
                freeze_first_remote_lane(store, &pending, id, code, policy()).unwrap_err(),
                "task_permission_mode_changed"
            );
            assert!(!fact.exists());
            assert!(!mode_file.exists());
        }
        pending.lock().clear();
        let reopened = SessionStore::boot().unwrap();
        for code in [true, false] {
            assert_eq!(
                freeze_first_remote_lane(&reopened, &pending, id, code, policy()).unwrap_err(),
                "task_permission_mode_changed"
            );
            assert!(!fact.exists());
        }
    });
}

#[test]
fn b_local_unknown_legacy_session_without_a_fact_is_not_enrolled() {
    crate::features::sessions::with_isolated_store_restoring_home(|store| {
        let session = store
            .create_new(
                "model".into(),
                None,
                store.manager.sessions_dir().join("local"),
            )
            .unwrap();
        let id = &session.metadata.id;
        store.set_mode_and_persist(id, Plan).unwrap();
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.set_session_provisioner(Arc::new(Provisioner {
            store: store.clone(),
            pending: parking_lot::Mutex::new(HashMap::new()),
        }));
        service
            .switch_remote_session("remote-reference", id, "generation-1")
            .unwrap();
        assert_eq!(
            service
                .claim_local_session(id, "generation-1")
                .unwrap_err()
                .code,
            "task_permission_mode_changed"
        );
        assert!(
            !store
                .manager
                .sessions_dir()
                .join(id)
                .join("remote-task-permission.json")
                .exists()
        );
    });
}
