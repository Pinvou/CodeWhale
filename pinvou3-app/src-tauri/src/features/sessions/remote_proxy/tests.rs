use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use super::binding::{AuthenticatedMapping, LiveRemoteContext};
use super::routing::{LocalBackendBinding, SessionRoute};
use super::schema::{
    PendingDispatchState, PendingRemoteRequest, ProjectionAttachment, ProjectionEntry,
    ProjectionKind, RetiredRequestDisposition,
};
use crate::features::sessions::{SessionKind, SessionStore};

fn context() -> LiveRemoteContext {
    LiveRemoteContext {
        peer_id: "authenticated-peer".into(),
        resume_boundary_id: "same-process-resume".into(),
        connection_id: "connection-1".into(),
        controller_lease_id: "lease-1".into(),
        lease_epoch: "epoch-1".into(),
        relay_epoch: "relay-1".into(),
        generation: "generation-1".into(),
        control_connected: true,
        encrypted_channel_connected: true,
    }
}

fn make_store(root: &std::path::Path) -> SessionStore {
    SessionStore::boot_at_test_dir(root).expect("test SessionStore")
}

#[test]
fn duplicate_archive_cleans_exact_cross_instance_active_residue() {
    let temp = tempfile::tempdir().unwrap();
    let first = make_store(temp.path());
    let opened = first
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    let second = make_store(temp.path());
    assert!(
        second
            .remote_proxy_snapshot(&opened.local_session_id)
            .is_some()
    );
    assert!(
        first
            .archive_remote_proxy(&opened.local_session_id, None)
            .unwrap()
    );
    assert!(
        second
            .archive_remote_proxy(&opened.local_session_id, None)
            .unwrap()
    );
    assert!(
        second
            .remote_proxy_snapshot(&opened.local_session_id)
            .is_none()
    );
    assert!(second.remote_proxies.lock().openings.is_empty());
    assert!(
        make_store(temp.path())
            .read_remote_proxy_history(&opened.local_session_id)
            .is_some()
    );
    assert!(
        second
            .prepare_remote_proxy_open(Some(&opened.local_session_id), &context())
            .is_err()
    );
}

#[test]
fn duplicate_archive_rejects_changed_active_residue_without_reopening_authority() {
    let temp = tempfile::tempdir().unwrap();
    let first = make_store(temp.path());
    let opened = first
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    let second = make_store(temp.path());
    assert!(
        first
            .archive_remote_proxy(&opened.local_session_id, None)
            .unwrap()
    );
    second
        .remote_proxies
        .lock()
        .records
        .get_mut(&opened.local_session_id)
        .unwrap()
        .ui
        .title = "Changed".into();
    assert_eq!(
        second
            .archive_remote_proxy(&opened.local_session_id, None)
            .unwrap_err()
            .to_string(),
        "remote_proxy_archive_conflict"
    );
    assert!(
        second
            .remote_proxy_snapshot(&opened.local_session_id)
            .is_some()
    );
    assert!(
        second
            .require_live_remote_mutation(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &context()
            )
            .is_err()
    );
    assert!(
        make_store(temp.path())
            .read_remote_proxy_history(&opened.local_session_id)
            .is_some()
    );
}

#[test]
fn exact_byte_budget_admission_reserves_fingerprint_attempt_projection_and_terminal() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let active = store.test_commit_remote_proxy_open(None, &live).unwrap();
    let filler = store.test_commit_remote_proxy_open(None, &live).unwrap();
    store
        .confirm_remote_mapping(
            &active.local_session_id,
            &live,
            &AuthenticatedMapping {
                remote_session_ref: active.remote_session_ref.clone(),
                controlled_session_id: "b-budget".into(),
            },
        )
        .unwrap();
    let pending = PendingRemoteRequest {
        client_request_id: "budget-client".into(),
        task_id: "budget-task".into(),
        request_fingerprint: "a".repeat(64),
        dispatch_state: PendingDispatchState::PreparedNeverAttempted,
        reservation_fingerprint: None,
    };
    {
        let mut state = store.remote_proxies.lock();
        state
            .records
            .get_mut(&active.local_session_id)
            .unwrap()
            .pending
            .push(pending.clone());
        let bytes = serde_json::to_vec(&super::schema::RemoteProxyFile {
            schema_version: super::schema::REMOTE_PROXY_SCHEMA_VERSION,
            records: state.records.values().cloned().collect(),
        })
        .unwrap();
        let required = state.required_budget(&bytes).unwrap();
        state
            .records
            .get_mut(&active.local_session_id)
            .unwrap()
            .pending
            .clear();
        state
            .records
            .get_mut(&filler.local_session_id)
            .unwrap()
            .peer_id
            .push_str(&"p".repeat(super::schema::MAX_REMOTE_PROXY_FILE_BYTES - required));
        state.persist().unwrap();
    }
    store
        .insert_remote_pending(
            &active.local_session_id,
            &active.remote_session_ref,
            &live,
            pending,
        )
        .unwrap();
    assert_eq!(
        store.remote_proxies.lock().persisted_required.get(),
        Some(super::schema::MAX_REMOTE_PROXY_FILE_BYTES)
    );
    store
        .replace_prepared_remote_fingerprint(
            &active.local_session_id,
            &active.remote_session_ref,
            &live,
            "budget-client",
            "budget-task",
            &"a".repeat(64),
            &"b".repeat(64),
        )
        .unwrap();
    store
        .mark_remote_dispatch_attempt(
            &active.local_session_id,
            &active.remote_session_ref,
            &live,
            "budget-client",
            "budget-task",
            &"b".repeat(64),
        )
        .unwrap();
    for sequence in 1..=10 {
        let mut entry = projection_entry(sequence, "answer");
        entry.task_id = Some("budget-task".into());
        store
            .project_authenticated_remote_entry(
                &active.local_session_id,
                &active.remote_session_ref,
                &live,
                "b-budget",
                entry,
            )
            .unwrap();
    }
    // Exercise the actual stored-schema escaped epoch upper bound, not only
    // ASCII producer fixtures. 64*128 input bytes exceed the old 20KiB reserve.
    {
        let mut state = store.remote_proxies.lock();
        let mut record = state.records[&active.local_session_id].clone();
        record.retired_epochs = (0..64)
            .map(|index| format!("{}{:02}", "\u{0001}".repeat(126), index))
            .collect();
        assert!(serde_json::to_vec(&record.retired_epochs).unwrap().len() > 20 * 1024);
        super::persistence::persist_changed(&mut state, &active.local_session_id, record).unwrap();
    }
    let mut terminal = projection_entry(11, "succeeded");
    terminal.kind = ProjectionKind::Status;
    terminal.task_id = Some("budget-task".into());
    store
        .project_authenticated_remote_terminal(
            &active.local_session_id,
            &active.remote_session_ref,
            &live,
            "b-budget",
            "budget-client",
            "budget-task",
            &"b".repeat(64),
            terminal,
        )
        .unwrap();
    let record = store
        .remote_proxy_snapshot(&active.local_session_id)
        .unwrap();
    assert!(record.pending.is_empty());
    assert_eq!(record.retired_requests.len(), 1);
    assert!(
        std::fs::metadata(temp.path().join("sessions/_remote_proxies.json"))
            .unwrap()
            .len()
            <= super::schema::MAX_REMOTE_PROXY_FILE_BYTES as u64
    );
}

#[test]
fn legacy_over_reserved_pending_file_loads_and_can_abandon_without_new_admission() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let mut records = Vec::new();
    for index in 0..17 {
        let opened = store
            .test_commit_remote_proxy_open(None, &context())
            .unwrap();
        let mut record = store
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap();
        record.pending.push(PendingRemoteRequest {
            client_request_id: format!("legacy-request-{index}"),
            task_id: format!("legacy-task-{index}"),
            request_fingerprint: "a".repeat(64),
            dispatch_state: PendingDispatchState::PreparedNeverAttempted,
            reservation_fingerprint: None,
        });
        records.push(record);
    }
    let bytes = serde_json::to_vec(&super::schema::RemoteProxyFile {
        schema_version: super::schema::REMOTE_PROXY_SCHEMA_VERSION,
        records: records.clone(),
    })
    .unwrap();
    assert!(bytes.len() < 65536);
    drop(store);
    std::fs::write(temp.path().join("sessions/_remote_proxies.json"), bytes).unwrap();
    let restarted = make_store(temp.path());
    assert!(!restarted.remote_proxies.lock().load_failed);
    assert!(
        restarted
            .test_commit_remote_proxy_open(None, &context())
            .is_err()
    );
    for record in records {
        let pending = &record.pending[0];
        restarted
            .abandon_unattempted_remote_pending(
                &record.local_session_id,
                &pending.client_request_id,
                &pending.task_id,
                &pending.request_fingerprint,
            )
            .unwrap();
    }
    assert!(
        restarted
            .test_commit_remote_proxy_open(None, &context())
            .is_ok()
    );
}

#[test]
fn fully_used_legacy_json_can_seal_without_growing_active_dependency_marker() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let opened = store
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    let mut record = store
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap();
    let mut file = super::schema::RemoteProxyFile {
        schema_version: super::schema::REMOTE_PROXY_SCHEMA_VERSION,
        records: vec![record.clone()],
    };
    let initial = serde_json::to_vec(&file).unwrap().len();
    record
        .peer_id
        .push_str(&"p".repeat(super::schema::MAX_REMOTE_PROXY_FILE_BYTES - initial));
    file.records[0] = record;
    let bytes = serde_json::to_vec(&file).unwrap();
    assert_eq!(bytes.len(), super::schema::MAX_REMOTE_PROXY_FILE_BYTES);
    drop(store);
    std::fs::write(temp.path().join("sessions/_remote_proxies.json"), bytes).unwrap();
    let restarted = make_store(temp.path());
    assert!(!restarted.remote_proxies.lock().load_failed);
    assert!(
        restarted
            .archive_remote_proxy(&opened.local_session_id, None)
            .unwrap()
    );
    assert!(restarted.remote_proxies.lock().records.is_empty());
    assert!(
        restarted
            .is_archived_remote_proxy(&opened.local_session_id)
            .unwrap()
    );
    let again = make_store(temp.path());
    assert!(
        again
            .read_remote_proxy_history(&opened.local_session_id)
            .is_some()
    );
    assert!(
        again
            .prepare_remote_proxy_open(Some(&opened.local_session_id), &context())
            .is_err()
    );
}

#[test]
fn offline_archive_rejects_live_and_pending_then_releases_capacity() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let opened = store
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    assert_eq!(
        store
            .archive_remote_proxy(&opened.local_session_id, Some(&context()))
            .unwrap_err()
            .to_string(),
        "remote_proxy_archive_online"
    );
    {
        let mut state = store.remote_proxies.lock();
        let mut record = state.records[&opened.local_session_id].clone();
        record.pending.push(PendingRemoteRequest {
            client_request_id: "pending".into(),
            task_id: "pending-task".into(),
            request_fingerprint: "a".repeat(64),
            dispatch_state: PendingDispatchState::PreparedNeverAttempted,
            reservation_fingerprint: None,
        });
        super::persistence::persist_changed(&mut state, &opened.local_session_id, record).unwrap();
    }
    assert_eq!(
        store
            .archive_remote_proxy(&opened.local_session_id, None)
            .unwrap_err()
            .to_string(),
        "remote_proxy_archive_pending"
    );
    store
        .abandon_unattempted_remote_pending(
            &opened.local_session_id,
            "pending",
            "pending-task",
            &"a".repeat(64),
        )
        .unwrap();
    assert!(
        store
            .archive_remote_proxy(&opened.local_session_id, None)
            .unwrap()
    );
    assert!(
        store
            .remote_proxy_snapshot(&opened.local_session_id)
            .is_none()
    );
    assert!(
        store
            .read_remote_proxy_history(&opened.local_session_id)
            .is_some()
    );
    assert!(
        store
            .prepare_remote_proxy_open(Some(&opened.local_session_id), &context())
            .is_err()
    );
}

fn assert_invalid_cache_is_isolated(root: &std::path::Path) {
    let sessions_dir = root.join("sessions");
    let path = sessions_dir.join("_remote_proxies.json");
    let bytes = std::fs::read(&path).unwrap();
    assert!(super::RemoteProxyState::load(path.clone(), &sessions_dir).is_err());
    // Repeating the app's boot fallback must not repeat the remote failure.
    for _ in 0..2 {
        let store = make_store(root);
        let local = store
            .create_new("/model".into(), None, root.to_path_buf())
            .unwrap();
        assert!(store.load(&local.metadata.id).is_ok());
        assert_eq!(
            store.session_kind(&local.metadata.id).unwrap(),
            SessionKind::Chat
        );
        store.set_local_backend_resolver(Arc::new(|_| Ok(LocalBackendBinding::Engine)));
        assert_eq!(
            store
                .route_session(&local.metadata.id, true, None, None)
                .unwrap(),
            SessionRoute::Engine
        );
        assert!(
            store
                .list()
                .unwrap()
                .iter()
                .any(|item| item.id == local.metadata.id)
        );
        assert!(store.prepare_remote_proxy_open(None, &context()).is_err());
        assert!(store.remote_proxies.lock().persist().is_err());
        assert_eq!(std::fs::read(&path).unwrap(), bytes);
    }
}

#[test]
fn missing_archive_bank_disables_remote_authority_without_blocking_local_sessions() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let opened = store
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    store
        .archive_remote_proxy(&opened.local_session_id, None)
        .unwrap();
    assert_eq!(
        store.session_kind(&opened.local_session_id).unwrap(),
        SessionKind::RemoteProxy
    );
    assert_eq!(
        store
            .route_session(&opened.local_session_id, false, None, None)
            .unwrap(),
        SessionRoute::RemoteReadOnly
    );
    assert!(
        store
            .route_session(
                &opened.local_session_id,
                true,
                Some(&opened.remote_session_ref),
                Some(&context())
            )
            .is_err()
    );
    let forged = deepseek_tui::session_manager::create_saved_session_with_id_and_mode(
        opened.local_session_id.clone(),
        &[],
        "/model",
        temp.path(),
        0,
        None,
        None,
    );
    assert!(store.save(&forged).is_err());
    assert!(store.delete(&opened.local_session_id).is_err());
    assert!(
        store
            .read_remote_proxy_history(&opened.local_session_id)
            .is_some()
    );
    drop(store);
    // Exact test-owned bank only. Its durable anchor must not recreate it.
    let bank = temp
        .path()
        .join("sessions/_remote_proxies.archives.sqlite3");
    std::fs::remove_file(&bank).unwrap();
    let restarted = make_store(temp.path());
    let local = restarted
        .create_new("/model".into(), None, temp.path().to_path_buf())
        .unwrap();
    assert!(restarted.load(&local.metadata.id).is_ok());
    assert_eq!(
        restarted.session_kind(&local.metadata.id).unwrap(),
        SessionKind::Chat
    );
    restarted.set_local_backend_resolver(Arc::new(|_| Ok(LocalBackendBinding::Acp)));
    assert_eq!(
        restarted
            .route_session(&local.metadata.id, true, None, None)
            .unwrap(),
        SessionRoute::Acp
    );
    restarted.save(&local).unwrap();
    restarted.delete(&local.metadata.id).unwrap();
    assert!(restarted.session_kind(&opened.local_session_id).is_err());
    assert!(
        restarted
            .prepare_remote_proxy_open(None, &context())
            .is_err()
    );
    assert!(!bank.exists());
}

#[test]
fn malformed_and_oversized_remote_cache_do_not_block_local_startup() {
    for bytes in [
        b"{broken".to_vec(),
        vec![b' '; super::schema::MAX_REMOTE_PROXY_FILE_BYTES + 1],
    ] {
        let temp = tempfile::tempdir().unwrap();
        let store = make_store(temp.path());
        drop(store);
        std::fs::write(temp.path().join("sessions/_remote_proxies.json"), bytes).unwrap();
        assert_invalid_cache_is_isolated(temp.path());
    }
}

#[test]
fn remote_cache_local_id_collision_does_not_block_local_startup() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let opened = store
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    drop(store);
    let local = deepseek_tui::session_manager::create_saved_session_with_id_and_mode(
        opened.local_session_id.clone(),
        &[],
        "/model",
        temp.path(),
        0,
        None,
        None,
    );
    let local_path = temp
        .path()
        .join(format!("sessions/{}.json", opened.local_session_id));
    let local_bytes = serde_json::to_vec(&local).unwrap();
    std::fs::write(&local_path, &local_bytes).unwrap();
    assert_invalid_cache_is_isolated(temp.path());
    let restarted = make_store(temp.path());
    assert_eq!(
        restarted
            .load(&opened.local_session_id)
            .unwrap()
            .metadata
            .id,
        opened.local_session_id
    );
    assert!(
        restarted
            .list()
            .unwrap()
            .iter()
            .any(|item| item.id == opened.local_session_id)
    );
    assert_eq!(std::fs::read(local_path).unwrap(), local_bytes);
}

fn projection_entry(sequence: u64, body: &str) -> ProjectionEntry {
    ProjectionEntry {
        journal_epoch: "journal-1".into(),
        sequence,
        event_id: format!("event-{sequence}"),
        kind: ProjectionKind::AssistantMessage,
        task_id: Some("task-1".into()),
        body: Some(body.into()),
        attachments: Vec::new(),
    }
}

#[test]
fn remote_open_is_idempotent_only_within_original_live_boundary() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
    assert_eq!(
        store
            .test_commit_remote_proxy_open(Some(&opened.local_session_id), &live)
            .unwrap(),
        opened
    );
    assert_eq!(
        store.session_kind(&opened.local_session_id).unwrap(),
        SessionKind::RemoteProxy
    );
    let record = store
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap();
    assert_eq!(record.remote_session_ref, opened.remote_session_ref);
    assert!(record.controlled_session_id.is_none());
    assert!(
        store
            .route_session(
                &opened.local_session_id,
                true,
                Some(&opened.remote_session_ref),
                Some(&live)
            )
            .is_err()
    );
    assert_eq!(
        store
            .route_session(&opened.local_session_id, false, None, None)
            .unwrap(),
        SessionRoute::RemoteReadOnly
    );

    let restarted = make_store(temp.path());
    assert_eq!(
        restarted
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap(),
        record
    );
    assert!(
        restarted
            .test_commit_remote_proxy_open(Some(&opened.local_session_id), &live)
            .is_err()
    );
    assert!(
        restarted
            .confirm_remote_mapping(
                &opened.local_session_id,
                &live,
                &AuthenticatedMapping {
                    remote_session_ref: opened.remote_session_ref.clone(),
                    controlled_session_id: "b-session".into(),
                }
            )
            .is_err()
    );
    let fresh = restarted
        .test_commit_remote_proxy_open(None, &live)
        .unwrap();
    assert_ne!(fresh.local_session_id, opened.local_session_id);
    assert_ne!(fresh.remote_session_ref, opened.remote_session_ref);
}

#[test]
fn authenticated_mapping_and_full_live_context_gate_mutations() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
    let another = store.test_commit_remote_proxy_open(None, &live).unwrap();
    let mapping = AuthenticatedMapping {
        remote_session_ref: opened.remote_session_ref.clone(),
        controlled_session_id: "b-authority".into(),
    };
    assert!(
        store
            .confirm_remote_mapping(&another.local_session_id, &live, &mapping)
            .is_err()
    );
    assert!(
        store
            .confirm_remote_mapping(
                &opened.local_session_id,
                &live,
                &AuthenticatedMapping {
                    remote_session_ref: "forged".into(),
                    controlled_session_id: "b-authority".into(),
                }
            )
            .is_err()
    );
    store
        .confirm_remote_mapping(&opened.local_session_id, &live, &mapping)
        .unwrap();
    assert_ne!(
        opened.remote_session_ref, mapping.controlled_session_id,
        "A's remote reference and B's Session id are different namespaces"
    );
    let dispatch_context = store
        .remote_dispatch_live_context(&opened.local_session_id, &opened.remote_session_ref)
        .unwrap();
    assert_eq!(dispatch_context, live);
    store
        .require_live_remote_mutation(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &dispatch_context,
        )
        .unwrap();
    store
        .confirm_remote_mapping(&opened.local_session_id, &live, &mapping)
        .unwrap();
    assert!(
        store
            .confirm_remote_mapping(
                &opened.local_session_id,
                &live,
                &AuthenticatedMapping {
                    controlled_session_id: "different-b-session".into(),
                    ..mapping.clone()
                }
            )
            .is_err()
    );
    assert_eq!(
        store
            .route_session(
                &opened.local_session_id,
                true,
                Some(&opened.remote_session_ref),
                Some(&live)
            )
            .unwrap(),
        SessionRoute::RemoteControl
    );
    let mut wrong_connection = live.clone();
    wrong_connection.connection_id = "old".into();
    let mut wrong_lease = live.clone();
    wrong_lease.controller_lease_id = "lease-2".into();
    let mut wrong_generation = live.clone();
    wrong_generation.generation = "generation-2".into();
    let mut wrong_peer = live.clone();
    wrong_peer.peer_id = "another-peer".into();
    for changed in [
        &wrong_connection,
        &wrong_lease,
        &wrong_generation,
        &wrong_peer,
    ] {
        assert!(
            store
                .route_session(
                    &opened.local_session_id,
                    true,
                    Some(&opened.remote_session_ref),
                    Some(changed)
                )
                .is_err()
        );
    }
    store.test_expire_remote_binding(&opened.local_session_id);
    assert_eq!(
        store
            .route_session(&opened.local_session_id, false, None, None)
            .unwrap(),
        SessionRoute::RemoteReadOnly
    );
    assert!(
        store
            .route_session(
                &opened.local_session_id,
                true,
                Some(&opened.remote_session_ref),
                Some(&live)
            )
            .is_err()
    );
    let mut repaired = live.clone();
    repaired.resume_boundary_id = "new-pairing".into();
    let fresh = store
        .test_commit_remote_proxy_open(None, &repaired)
        .unwrap();
    assert_ne!(fresh.remote_session_ref, opened.remote_session_ref);
    assert!(
        store
            .route_session(
                &opened.local_session_id,
                true,
                Some(&opened.remote_session_ref),
                Some(&live)
            )
            .is_err()
    );
    assert!(
        store
            .test_commit_remote_proxy_open(Some(&opened.local_session_id), &repaired)
            .is_err()
    );
}

#[test]
fn three_routes_use_durable_kind_and_trusted_backend_binding() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let engine = store
        .create_new("model".into(), None, temp.path().join("engine"))
        .unwrap();
    let acp = store
        .create_new("model".into(), None, temp.path().join("acp"))
        .unwrap();
    let remote = store
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    let resolutions = Arc::new(AtomicUsize::new(0));
    let resolution_count = Arc::clone(&resolutions);
    let acp_id = acp.metadata.id.clone();
    store.set_local_backend_resolver(Arc::new(move |id| {
        resolution_count.fetch_add(1, Ordering::SeqCst);
        Ok(if id == acp_id {
            LocalBackendBinding::Acp
        } else {
            LocalBackendBinding::Engine
        })
    }));
    assert_eq!(
        store
            .route_session(&engine.metadata.id, true, None, None)
            .unwrap(),
        SessionRoute::Engine
    );
    assert_eq!(
        store
            .route_session(&acp.metadata.id, true, None, None)
            .unwrap(),
        SessionRoute::Acp
    );
    assert_eq!(resolutions.load(Ordering::SeqCst), 2);
    assert_eq!(
        store
            .route_session(&remote.local_session_id, false, None, None)
            .unwrap(),
        SessionRoute::RemoteReadOnly
    );
    assert!(
        store
            .route_session(&remote.local_session_id, true, None, None)
            .is_err()
    );
    assert_eq!(
        resolutions.load(Ordering::SeqCst),
        2,
        "RemoteProxy never resolves local Engine/ACP"
    );
    let forged_local = deepseek_tui::session_manager::create_saved_session_with_id_and_mode(
        remote.local_session_id.clone(),
        &[],
        "model",
        temp.path(),
        0,
        None,
        None,
    );
    assert!(store.save(&forged_local).is_err());
    assert!(store.save_session_atomic(&forged_local).is_err());
    assert!(store.delete(&remote.local_session_id).is_err());
}

#[test]
fn pending_is_bounded_and_durable_without_a_second_projection_writer() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
    store
        .confirm_remote_mapping(
            &opened.local_session_id,
            &live,
            &AuthenticatedMapping {
                remote_session_ref: opened.remote_session_ref.clone(),
                controlled_session_id: "b".into(),
            },
        )
        .unwrap();
    let pending = PendingRemoteRequest {
        client_request_id: "request-1".into(),
        task_id: "task-1".into(),
        request_fingerprint: "a".repeat(64),
        dispatch_state: PendingDispatchState::PreparedNeverAttempted,
        reservation_fingerprint: None,
    };
    store
        .insert_remote_pending(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            pending.clone(),
        )
        .unwrap();
    store
        .insert_remote_pending(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            pending,
        )
        .unwrap();
    for index in 2..=64 {
        store
            .insert_remote_pending(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                PendingRemoteRequest {
                    client_request_id: format!("request-{index}"),
                    task_id: format!("task-{index}"),
                    request_fingerprint: format!("{index:064x}"),
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                    reservation_fingerprint: None,
                },
            )
            .unwrap();
    }
    assert!(
        store
            .insert_remote_pending(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                PendingRemoteRequest {
                    client_request_id: "request-65".into(),
                    task_id: "task-65".into(),
                    request_fingerprint: "b".repeat(64),
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                    reservation_fingerprint: None,
                },
            )
            .is_err()
    );
    assert_eq!(
        store
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .pending
            .len(),
        64
    );
    let restarted = make_store(temp.path());
    assert_eq!(
        restarted
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .pending
            .len(),
        64
    );
}

#[test]
fn unsupported_schema_versions_fail_without_rewriting_disk() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    store
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    let path = temp.path().join("sessions/_remote_proxies.json");
    drop(store);
    let pristine =
        serde_json::from_slice::<serde_json::Value>(&std::fs::read(&path).unwrap()).unwrap();
    for version in [0, 1, 2, 99] {
        let mut unsupported = pristine.clone();
        unsupported["schema_version"] = version.into();
        let bytes = serde_json::to_vec(&unsupported).unwrap();
        std::fs::write(&path, &bytes).unwrap();
        assert_invalid_cache_is_isolated(temp.path());
        assert_eq!(std::fs::read(&path).unwrap(), bytes);
    }
}

#[test]
fn authenticated_projection_is_readable_after_restart_but_offline_writes_fail() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let first = store.test_commit_remote_proxy_open(None, &live).unwrap();
    let second = store.test_commit_remote_proxy_open(None, &live).unwrap();
    store
        .confirm_remote_mapping(
            &first.local_session_id,
            &live,
            &AuthenticatedMapping {
                remote_session_ref: first.remote_session_ref.clone(),
                controlled_session_id: "b-1".into(),
            },
        )
        .unwrap();
    let entry = projection_entry(1, "authenticated body");
    assert_eq!(
        store
            .project_authenticated_remote_entry(
                &first.local_session_id,
                &first.remote_session_ref,
                &live,
                "b-1",
                entry.clone(),
            )
            .unwrap(),
        super::projection::ProjectionWrite::Inserted
    );
    assert_eq!(
        store
            .project_authenticated_remote_entry(
                &first.local_session_id,
                &first.remote_session_ref,
                &live,
                "b-1",
                entry.clone(),
            )
            .unwrap(),
        super::projection::ProjectionWrite::Unchanged
    );
    assert!(
        store
            .project_authenticated_remote_entry(
                &second.local_session_id,
                &first.remote_session_ref,
                &live,
                "b-1",
                entry.clone(),
            )
            .is_err()
    );
    let restarted = make_store(temp.path());
    let ids = restarted
        .list_remote_proxy_history()
        .into_iter()
        .map(|item| item.local_session_id)
        .collect::<Vec<_>>();
    assert!(ids.contains(&first.local_session_id));
    assert!(ids.contains(&second.local_session_id));
    let view = restarted
        .read_remote_proxy_history(&first.local_session_id)
        .unwrap();
    assert_eq!(view.projection.entries, vec![entry]);
    assert_eq!(view.authenticated_cursor.unwrap().sequence, 1);
    assert!(restarted.read_remote_proxy_history("missing").is_none());
    assert!(
        restarted
            .project_authenticated_remote_entry(
                &first.local_session_id,
                &first.remote_session_ref,
                &live,
                "b-1",
                projection_entry(2, "offline write"),
            )
            .is_err()
    );
}

#[test]
fn projection_writer_enforces_immutable_keys_bounds_and_secret_rejection() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
    store
        .confirm_remote_mapping(
            &opened.local_session_id,
            &live,
            &AuthenticatedMapping {
                remote_session_ref: opened.remote_session_ref.clone(),
                controlled_session_id: "b-1".into(),
            },
        )
        .unwrap();
    let mut entry = projection_entry(
        1,
        "A user may discuss Bearer tokens without erasing history",
    );
    entry.attachments.push(ProjectionAttachment {
        id: "attachment-1".into(),
        display_name: "report.txt".into(),
        content_type: "text/plain".into(),
        byte_length: 123,
        checksum_sha256: "a".repeat(64),
    });
    store
        .project_authenticated_remote_entry(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            "b-1",
            entry.clone(),
        )
        .unwrap();
    let mut conflict = entry.clone();
    conflict.body = Some("different".into());
    assert!(
        store
            .project_authenticated_remote_entry(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                "b-1",
                conflict,
            )
            .is_err()
    );
    assert!(
        store
            .project_authenticated_remote_entry(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                "b-1",
                projection_entry(2, &"x".repeat(16 * 1024 + 1)),
            )
            .is_err()
    );
    let mut too_many_attachments = projection_entry(2, "body");
    too_many_attachments.attachments = vec![entry.attachments[0].clone(); 17];
    assert!(
        store
            .project_authenticated_remote_entry(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                "b-1",
                too_many_attachments,
            )
            .is_err()
    );
    let mut json = serde_json::to_value(entry).unwrap();
    json["authorization_header"] = "secret".into();
    assert!(serde_json::from_value::<ProjectionEntry>(json).is_err());
    let mut attachment = serde_json::to_value(ProjectionAttachment {
        id: "a".into(),
        display_name: "file".into(),
        content_type: "text/plain".into(),
        byte_length: 1,
        checksum_sha256: "a".repeat(64),
    })
    .unwrap();
    attachment["api_key"] = "secret".into();
    assert!(serde_json::from_value::<ProjectionAttachment>(attachment).is_err());
}

#[test]
fn pending_write_uses_protocol_field_rules_and_keeps_recovered_uncertainty() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
    store
        .confirm_remote_mapping(
            &opened.local_session_id,
            &live,
            &AuthenticatedMapping {
                remote_session_ref: opened.remote_session_ref.clone(),
                controlled_session_id: "b-1".into(),
            },
        )
        .unwrap();
    let write = |pending| {
        store.insert_remote_pending(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            pending,
        )
    };
    let valid = PendingRemoteRequest {
        client_request_id: "request-1".into(),
        task_id: "task-1".into(),
        request_fingerprint: "a".repeat(64),
        dispatch_state: PendingDispatchState::PreparedNeverAttempted,
        reservation_fingerprint: None,
    };
    let mut invalid = valid.clone();
    invalid.client_request_id = "bad/slash".into();
    assert!(write(invalid).is_err());
    let mut accepted = valid.clone();
    accepted.client_request_id = "-request".into();
    accepted.task_id = ":task".into();
    write(accepted).unwrap();
    let mut invalid = valid.clone();
    invalid.task_id = "t".repeat(129);
    assert!(write(invalid).is_err());
    let mut invalid = valid.clone();
    invalid.request_fingerprint = "A".repeat(64);
    assert!(write(invalid).is_err());
    let encoded = serde_json::to_value(&valid).unwrap();
    assert!(encoded.get("reservation_fingerprint").is_none());
    assert_eq!(
        serde_json::from_value::<PendingRemoteRequest>(encoded).unwrap(),
        valid
    );
    for alias in ["short".to_owned(), "A".repeat(64)] {
        let mut invalid = valid.clone();
        invalid.reservation_fingerprint = Some(alias);
        assert!(write(invalid).is_err());
    }
    write(valid.clone()).unwrap();
    write(valid.clone()).unwrap();
    assert!(
        store
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .pending[0]
            .dispatch_state
            == PendingDispatchState::PreparedNeverAttempted
    );
    let mut conflict = valid;
    conflict.request_fingerprint = "b".repeat(64);
    assert!(write(conflict).is_err());
}

#[test]
fn v3_cannot_masquerade_as_legacy_by_erasing_security_fields() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    store
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    let path = temp.path().join("sessions/_remote_proxies.json");
    let pristine: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    drop(store);
    for removed in [
        vec!["projection"],
        vec!["retired_epochs"],
        vec!["retired_requests"],
        vec!["projection", "retired_epochs"],
    ] {
        let mut tampered = pristine.clone();
        for field in removed {
            tampered["records"][0]
                .as_object_mut()
                .unwrap()
                .remove(field);
        }
        std::fs::write(&path, serde_json::to_vec(&tampered).unwrap()).unwrap();
        assert_invalid_cache_is_isolated(temp.path());
    }
    for tamper in [
        "retired-type",
        "retired-requests-type",
        "projection-version",
        "record-version",
    ] {
        let mut tampered = pristine.clone();
        match tamper {
            "retired-type" => tampered["records"][0]["retired_epochs"] = "old".into(),
            "retired-requests-type" => tampered["records"][0]["retired_requests"] = "old".into(),
            "projection-version" => {
                tampered["records"][0]["projection"]["schema_version"] = 99.into()
            }
            _ => tampered["records"][0]["schema_version"] = 1.into(),
        }
        std::fs::write(&path, serde_json::to_vec(&tampered).unwrap()).unwrap();
        assert_invalid_cache_is_isolated(temp.path());
    }
    std::fs::write(&path, serde_json::to_vec(&pristine).unwrap()).unwrap();
    assert!(SessionStore::boot_at_test_dir(temp.path()).is_ok());
}

fn pending_request(client: &str, task: &str) -> PendingRemoteRequest {
    PendingRemoteRequest {
        client_request_id: client.into(),
        task_id: task.into(),
        request_fingerprint: "a".repeat(64),
        dispatch_state: PendingDispatchState::PreparedNeverAttempted,
        reservation_fingerprint: None,
    }
}

#[test]
fn never_attempted_abandon_is_atomic_and_retires_both_ids_across_records() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
    store
        .confirm_remote_mapping(
            &opened.local_session_id,
            &live,
            &AuthenticatedMapping {
                remote_session_ref: opened.remote_session_ref.clone(),
                controlled_session_id: "b-session".into(),
            },
        )
        .unwrap();
    let pending = pending_request("request-1", "task-1");
    store
        .insert_remote_pending(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            pending.clone(),
        )
        .unwrap();
    let original_path = {
        let mut state = store.remote_proxies.lock();
        std::mem::replace(
            &mut state.path,
            temp.path().join("missing-parent/remote.json"),
        )
    };
    assert!(
        store
            .abandon_unattempted_remote_pending(
                &opened.local_session_id,
                "request-1",
                "task-1",
                &pending.request_fingerprint
            )
            .is_err()
    );
    store.remote_proxies.lock().path = original_path;
    let record = store
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap();
    assert_eq!(record.pending.len(), 1);
    assert!(record.retired_requests.is_empty());
    drop(store);
    let restarted = make_store(temp.path());
    assert_eq!(
        restarted
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .pending[0]
            .dispatch_state,
        PendingDispatchState::PreparedNeverAttempted
    );
    restarted
        .abandon_unattempted_remote_pending(
            &opened.local_session_id,
            "request-1",
            "task-1",
            &pending.request_fingerprint,
        )
        .unwrap();
    assert!(
        restarted
            .abandon_unattempted_remote_pending(
                &opened.local_session_id,
                "request-1",
                "task-1",
                &pending.request_fingerprint
            )
            .is_err()
    );
    let retired = &restarted
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap()
        .retired_requests[0];
    assert_eq!(
        retired.disposition,
        RetiredRequestDisposition::AbandonedNeverAttempted
    );
    let another = restarted
        .test_commit_remote_proxy_open(None, &live)
        .unwrap();
    restarted
        .confirm_remote_mapping(
            &another.local_session_id,
            &live,
            &AuthenticatedMapping {
                remote_session_ref: another.remote_session_ref.clone(),
                controlled_session_id: "another-b-session".into(),
            },
        )
        .unwrap();
    for attempt in [
        pending_request("request-1", "task-2"),
        pending_request("request-2", "task-1"),
    ] {
        assert!(
            restarted
                .insert_remote_pending(
                    &another.local_session_id,
                    &another.remote_session_ref,
                    &live,
                    attempt
                )
                .is_err()
        );
    }
}

#[test]
fn v3_rejects_incomplete_or_conflicting_outbox_contract_without_disk_rewrite() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    store
        .test_commit_remote_proxy_open(None, &context())
        .unwrap();
    let path = temp.path().join("sessions/_remote_proxies.json");
    let pristine: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    drop(store);
    let pending = serde_json::json!({
        "client_request_id": "request-1",
        "task_id": "task-1",
        "request_fingerprint": "a".repeat(64),
        "dispatch_state": "prepared_never_attempted"
    });
    let retired = serde_json::json!({
        "client_request_id": "request-2",
        "task_id": "task-2",
        "request_fingerprint": "b".repeat(64),
        "disposition": "abandoned_never_attempted"
    });
    let mut cases = Vec::new();
    let mut missing_dispatch = pristine.clone();
    let mut item = pending.clone();
    item.as_object_mut().unwrap().remove("dispatch_state");
    missing_dispatch["records"][0]["pending"] = serde_json::json!([item]);
    cases.push(missing_dispatch);
    let mut old_boolean = pristine.clone();
    let mut item = pending.clone();
    item["outcome_unknown"] = false.into();
    old_boolean["records"][0]["pending"] = serde_json::json!([item]);
    cases.push(old_boolean);
    let mut unknown_state = pristine.clone();
    let mut item = pending.clone();
    item["dispatch_state"] = "sent".into();
    unknown_state["records"][0]["pending"] = serde_json::json!([item]);
    cases.push(unknown_state);
    let mut missing_disposition = pristine.clone();
    let mut item = retired.clone();
    item.as_object_mut().unwrap().remove("disposition");
    missing_disposition["records"][0]["retired_requests"] = serde_json::json!([item]);
    cases.push(missing_disposition);
    let mut duplicate_id = pristine.clone();
    duplicate_id["records"][0]["pending"] = serde_json::json!([pending]);
    let mut item = retired;
    item["client_request_id"] = "request-1".into();
    duplicate_id["records"][0]["retired_requests"] = serde_json::json!([item]);
    cases.push(duplicate_id);
    for case in cases {
        let bytes = serde_json::to_vec(&case).unwrap();
        std::fs::write(&path, &bytes).unwrap();
        assert_invalid_cache_is_isolated(temp.path());
        assert_eq!(std::fs::read(&path).unwrap(), bytes);
    }
}
