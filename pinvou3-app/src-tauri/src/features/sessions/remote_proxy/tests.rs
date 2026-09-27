use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use super::binding::{AuthenticatedMapping, LiveRemoteContext};
use super::routing::{LocalBackendBinding, SessionRoute};
use super::schema::{
    AuthenticatedCursor, AuthenticatedProjection, AuthenticatedSnapshot, PendingDispatchState,
    PendingRemoteRequest, ProjectionAttachment, ProjectionEntry, ProjectionKind, RemoteUiMetadata,
    RetiredRequestDisposition, SnapshotCandidate, SnapshotVerificationPort,
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

struct TestAuthenticatedSnapshotVerifier;

impl SnapshotVerificationPort for TestAuthenticatedSnapshotVerifier {
    fn verify_authenticated_snapshot(
        &self,
        _local_session_id: &str,
        _remote_ref: &str,
        _context: &LiveRemoteContext,
        _candidate: &SnapshotCandidate,
    ) -> anyhow::Result<()> {
        Ok(())
    }
}

fn verified_snapshot(
    opened: &super::binding::RemoteOpen,
    live: &LiveRemoteContext,
    projection: AuthenticatedProjection,
    cursor: AuthenticatedCursor,
) -> anyhow::Result<AuthenticatedSnapshot> {
    let verifier: Arc<dyn SnapshotVerificationPort> = Arc::new(TestAuthenticatedSnapshotVerifier);
    AuthenticatedSnapshot::verify(
        &opened.local_session_id,
        &opened.remote_session_ref,
        live,
        SnapshotCandidate { projection, cursor },
        verifier.as_ref(),
    )
}

#[test]
fn remote_open_is_idempotent_only_within_original_live_boundary() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
    assert_eq!(
        store
            .open_remote_proxy(Some(&opened.local_session_id), &live)
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
            .open_remote_proxy(Some(&opened.local_session_id), &live)
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
    let fresh = restarted.open_remote_proxy(None, &live).unwrap();
    assert_ne!(fresh.local_session_id, opened.local_session_id);
    assert_ne!(fresh.remote_session_ref, opened.remote_session_ref);
}

#[test]
fn authenticated_mapping_and_full_live_context_gate_mutations() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
    let another = store.open_remote_proxy(None, &live).unwrap();
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
    store.disconnect_remote_proxy(&opened.local_session_id);
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
    let mut resumed = live.clone();
    resumed.connection_id = "connection-2".into();
    resumed.lease_epoch = "epoch-2".into();
    resumed.generation = "generation-2".into();
    store
        .resume_remote_proxy(&opened.local_session_id, &resumed)
        .unwrap();
    assert!(
        store
            .route_session(
                &opened.local_session_id,
                true,
                Some(&opened.remote_session_ref),
                Some(&resumed)
            )
            .is_err()
    );
    store
        .confirm_remote_mapping(&opened.local_session_id, &resumed, &mapping)
        .unwrap();
    assert_eq!(
        store
            .route_session(
                &opened.local_session_id,
                true,
                Some(&opened.remote_session_ref),
                Some(&resumed)
            )
            .unwrap(),
        SessionRoute::RemoteControl
    );
    let mut repaired = resumed.clone();
    repaired.resume_boundary_id = "new-pairing".into();
    assert!(
        store
            .resume_remote_proxy(&opened.local_session_id, &repaired)
            .is_err()
    );
    let fresh = store.open_remote_proxy(None, &repaired).unwrap();
    assert_ne!(fresh.remote_session_ref, opened.remote_session_ref);
    assert!(
        store
            .route_session(
                &opened.local_session_id,
                true,
                Some(&opened.remote_session_ref),
                Some(&resumed)
            )
            .is_err()
    );
    assert!(
        store
            .open_remote_proxy(Some(&opened.local_session_id), &repaired)
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
    let remote = store.open_remote_proxy(None, &context()).unwrap();
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
fn metadata_cursor_and_pending_are_bounded_and_field_scoped() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
    store
        .update_remote_ui(
            &opened.local_session_id,
            RemoteUiMetadata {
                title: "A local title".into(),
                pinned: true,
                hidden: false,
                last_opened_at: None,
            },
        )
        .unwrap();
    let mut first = projection_entry(1, "first");
    first.journal_epoch = "e".into();
    assert!(
        store
            .append_remote_projection(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                first.clone()
            )
            .is_err()
    );
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
    store
        .append_remote_projection(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            first.clone(),
        )
        .unwrap();
    let mut second = projection_entry(2, "second");
    second.journal_epoch = "e".into();
    store
        .append_remote_projection(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            second,
        )
        .unwrap();
    assert!(
        store
            .append_remote_projection(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                first
            )
            .is_err()
    );
    let pending = PendingRemoteRequest {
        client_request_id: "request-1".into(),
        task_id: "task-1".into(),
        request_fingerprint: "a".repeat(64),
        dispatch_state: PendingDispatchState::PreparedNeverAttempted,
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
                }
            )
            .is_err()
    );
    let record = store
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap();
    assert_eq!(record.pending.len(), 64);
    assert_eq!(record.ui.title, "A local title");
    assert_eq!(record.authenticated_cursor.unwrap().sequence, 2);
    let restarted = make_store(temp.path());
    assert!(
        restarted
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .pending[0]
            .dispatch_state
            == PendingDispatchState::PreparedNeverAttempted
    );
}

#[test]
fn version_zero_records_migrate_without_granting_live_authority() {
    let temp = tempfile::tempdir().unwrap();
    let sessions = temp.path().join("sessions");
    std::fs::create_dir_all(&sessions).unwrap();
    let path = sessions.join("_remote_proxies.json");
    std::fs::write(
        &path,
        serde_json::to_vec(&serde_json::json!({
            "schema_version": 0,
            "records": [{
                "local_session_id": "remote-legacy",
                "peer_id": "authenticated-peer",
                "remote_session_ref": "historical-ref",
                "controlled_session_id": "b-session",
                "title": "old title"
            }]
        }))
        .unwrap(),
    )
    .unwrap();
    let store = make_store(temp.path());
    let record = store.remote_proxy_snapshot("remote-legacy").unwrap();
    assert_eq!(record.schema_version, 3);
    assert_eq!(record.ui.title, "old title");
    assert!(
        store
            .route_session(
                "remote-legacy",
                true,
                Some("historical-ref"),
                Some(&context())
            )
            .is_err()
    );
    assert_eq!(
        store
            .route_session("remote-legacy", false, None, None)
            .unwrap(),
        SessionRoute::RemoteReadOnly
    );
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&std::fs::read(&path).unwrap()).unwrap()["schema_version"],
        3
    );
    drop(store);
    let mut corrupted =
        serde_json::from_slice::<serde_json::Value>(&std::fs::read(&path).unwrap()).unwrap();
    corrupted["schema_version"] = 99.into();
    std::fs::write(&path, serde_json::to_vec(&corrupted).unwrap()).unwrap();
    assert!(SessionStore::boot_at_test_dir(temp.path()).is_err());
}

#[test]
fn offline_projection_is_readable_and_enumerable_after_restart() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let first = store.open_remote_proxy(None, &live).unwrap();
    let second = store.open_remote_proxy(None, &live).unwrap();
    store
        .update_remote_ui(
            &first.local_session_id,
            RemoteUiMetadata {
                title: "first".into(),
                pinned: true,
                hidden: false,
                last_opened_at: Some("2026-09-27T10:00:00+08:00".into()),
            },
        )
        .unwrap();
    store
        .update_remote_ui(
            &second.local_session_id,
            RemoteUiMetadata {
                title: "second".into(),
                pinned: false,
                hidden: true,
                last_opened_at: Some("2026-09-27T11:00:00+08:00".into()),
            },
        )
        .unwrap();
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
    store
        .append_remote_projection(
            &first.local_session_id,
            &first.remote_session_ref,
            &live,
            entry.clone(),
        )
        .unwrap();
    assert!(
        store
            .append_remote_projection(
                &first.local_session_id,
                &first.remote_session_ref,
                &live,
                entry.clone(),
            )
            .is_err()
    );
    assert!(
        store
            .append_remote_projection(
                &second.local_session_id,
                &first.remote_session_ref,
                &live,
                entry.clone()
            )
            .is_err()
    );
    let restarted = make_store(temp.path());
    let list = restarted.list_remote_proxy_history();
    assert_eq!(
        list.iter()
            .map(|item| item.local_session_id.as_str())
            .collect::<Vec<_>>(),
        vec![
            first.local_session_id.as_str(),
            second.local_session_id.as_str()
        ]
    );
    assert_eq!(list[0].projected_entry_count, 1);
    let view = restarted
        .read_remote_proxy_history(&first.local_session_id)
        .unwrap();
    assert_eq!(view.projection.schema_version, 1);
    assert_eq!(view.projection.entries, vec![entry]);
    assert_eq!(view.authenticated_cursor.unwrap().sequence, 1);
    assert!(restarted.read_remote_proxy_history("missing").is_none());
    assert!(
        restarted
            .append_remote_projection(
                &first.local_session_id,
                &first.remote_session_ref,
                &live,
                projection_entry(2, "offline write")
            )
            .is_err()
    );
}

#[test]
fn projection_contract_enforces_dedup_bounds_and_structural_secret_rejection() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
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
        .append_remote_projection(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            entry.clone(),
        )
        .unwrap();
    let mut conflict = entry.clone();
    conflict.body = Some("different".into());
    assert!(
        store
            .append_remote_projection(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                conflict
            )
            .is_err()
    );
    assert!(
        store
            .append_remote_projection(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                projection_entry(2, &"x".repeat(16 * 1024 + 1))
            )
            .is_err()
    );
    let mut too_many_attachments = projection_entry(2, "body");
    too_many_attachments.attachments = vec![entry.attachments[0].clone(); 17];
    assert!(
        store
            .append_remote_projection(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                too_many_attachments
            )
            .is_err()
    );
    let large = AuthenticatedProjection {
        schema_version: 1,
        entries: (2..=71)
            .map(|sequence| projection_entry(sequence, &"x".repeat(16 * 1024)))
            .collect(),
    };
    assert!(
        verified_snapshot(
            &opened,
            &live,
            large,
            AuthenticatedCursor {
                journal_epoch: "journal-1".into(),
                sequence: 71
            }
        )
        .is_err()
    );
    let full = AuthenticatedProjection {
        schema_version: 1,
        entries: (2..=513)
            .map(|sequence| projection_entry(sequence, "x"))
            .collect(),
    };
    let full_snapshot = verified_snapshot(
        &opened,
        &live,
        full,
        AuthenticatedCursor {
            journal_epoch: "journal-1".into(),
            sequence: 513,
        },
    )
    .unwrap();
    store
        .replace_remote_projection_from_authenticated_snapshot(full_snapshot)
        .unwrap();
    assert!(
        store
            .append_remote_projection(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                projection_entry(514, "overflow")
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
fn legacy_v1_without_projection_is_upgraded_deterministically() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let opened = store.open_remote_proxy(None, &context()).unwrap();
    let path = temp.path().join("sessions/_remote_proxies.json");
    drop(store);
    let mut file: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    file["schema_version"] = 1.into();
    file["records"][0]["schema_version"] = 1.into();
    file["records"][0]["authenticated_cursor"] = serde_json::json!({
        "journal_epoch": "unbacked-old-epoch", "sequence": 7
    });
    file["records"][0]["pending"] = serde_json::json!([{
        "client_request_id": "-request-1",
        "task_id": ":task-1",
        "request_fingerprint": "a".repeat(64),
        "outcome_unknown": false,
    }]);
    file["records"][0]
        .as_object_mut()
        .unwrap()
        .remove("projection");
    file["records"][0]
        .as_object_mut()
        .unwrap()
        .remove("retired_epochs");
    file["records"][0]
        .as_object_mut()
        .unwrap()
        .remove("retired_requests");
    std::fs::write(&path, serde_json::to_vec(&file).unwrap()).unwrap();
    let migrated = make_store(temp.path());
    let view = migrated
        .read_remote_proxy_history(&opened.local_session_id)
        .unwrap();
    assert!(view.projection.entries.is_empty());
    assert!(view.authenticated_cursor.is_none());
    assert_eq!(view.projection.schema_version, 1);
    assert!(
        migrated
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .pending[0]
            .dispatch_state
            == PendingDispatchState::AttemptMayHaveOccurred
    );
    assert_eq!(
        migrated
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .pending[0]
            .client_request_id,
        "-request-1"
    );
    let rewritten: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    assert_eq!(rewritten["schema_version"], 3);
    assert_eq!(rewritten["records"][0]["schema_version"], 3);
    assert_eq!(rewritten["records"][0]["projection"]["schema_version"], 1);
    assert_eq!(
        rewritten["records"][0]["retired_epochs"],
        serde_json::json!([])
    );
    assert_eq!(
        rewritten["records"][0]["retired_requests"],
        serde_json::json!([])
    );
    drop(migrated);
    let reopened = make_store(temp.path());
    assert_eq!(
        reopened
            .read_remote_proxy_history(&opened.local_session_id)
            .unwrap(),
        view
    );
    assert!(
        reopened
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .pending[0]
            .dispatch_state
            == PendingDispatchState::AttemptMayHaveOccurred
    );
}

#[test]
fn v1_and_v2_reject_malformed_or_conflicting_pending_without_rewriting_disk() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    store.open_remote_proxy(None, &context()).unwrap();
    let path = temp.path().join("sessions/_remote_proxies.json");
    let pristine: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    drop(store);

    let valid = serde_json::json!({
        "client_request_id": "request-1",
        "task_id": "task-1",
        "request_fingerprint": "a".repeat(64),
        "outcome_unknown": false,
    });
    let mut cases = Vec::new();
    for (field, value) in [
        ("client_request_id", serde_json::json!("")),
        ("task_id", serde_json::json!("")),
        ("client_request_id", serde_json::json!("a".repeat(129))),
        ("task_id", serde_json::json!("t".repeat(129))),
        ("client_request_id", serde_json::json!("bad space")),
        ("task_id", serde_json::json!("éclair")),
        ("task_id", serde_json::json!("bad/slash")),
        ("request_fingerprint", serde_json::json!("A".repeat(64))),
        ("request_fingerprint", serde_json::json!("g".repeat(64))),
        ("request_fingerprint", serde_json::json!("a".repeat(63))),
    ] {
        let mut malformed = valid.clone();
        malformed[field] = value;
        cases.push(vec![malformed]);
    }
    // An exact duplicate is not a second correlation slot. Neither a shared
    // client ID nor a shared task ID may silently win on migration.
    cases.push(vec![valid.clone(), valid.clone()]);
    let mut same_client = valid.clone();
    same_client["task_id"] = "task-2".into();
    cases.push(vec![valid.clone(), same_client]);
    let mut same_task = valid.clone();
    same_task["client_request_id"] = "request-2".into();
    cases.push(vec![valid.clone(), same_task]);
    let mut same_pair_other_fingerprint = valid.clone();
    same_pair_other_fingerprint["request_fingerprint"] = "b".repeat(64).into();
    cases.push(vec![valid.clone(), same_pair_other_fingerprint]);
    cases.push(
        (0..65)
            .map(|index| {
                serde_json::json!({
                    "client_request_id": format!("request-{index}"),
                    "task_id": format!("task-{index}"),
                    "request_fingerprint": format!("{index:064x}"),
                    "outcome_unknown": false,
                })
            })
            .collect(),
    );

    for version in [1, 2] {
        for pending in &cases {
            let mut file = pristine.clone();
            file["schema_version"] = version.into();
            file["records"][0]["schema_version"] = version.into();
            file["records"][0]["pending"] = serde_json::Value::Array(pending.clone());
            file["records"][0]
                .as_object_mut()
                .unwrap()
                .remove("retired_requests");
            if version == 1 {
                let record = file["records"][0].as_object_mut().unwrap();
                record.remove("projection");
                record.remove("retired_epochs");
            }
            let bytes = serde_json::to_vec(&file).unwrap();
            std::fs::write(&path, &bytes).unwrap();
            assert!(
                SessionStore::boot_at_test_dir(temp.path()).is_err(),
                "v{version} accepted invalid pending {pending:?}"
            );
            assert_eq!(std::fs::read(&path).unwrap(), bytes);
        }
    }
}

#[test]
fn pending_write_uses_protocol_field_rules_and_keeps_recovered_uncertainty() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
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
    store.open_remote_proxy(None, &context()).unwrap();
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
        assert!(SessionStore::boot_at_test_dir(temp.path()).is_err());
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
        assert!(SessionStore::boot_at_test_dir(temp.path()).is_err());
    }
    std::fs::write(&path, serde_json::to_vec(&pristine).unwrap()).unwrap();
    assert!(SessionStore::boot_at_test_dir(temp.path()).is_ok());
}

#[test]
fn journal_append_is_contiguous_and_epoch_change_requires_verified_snapshot() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
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
    let append = |entry| {
        store.append_remote_projection(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            entry,
        )
    };
    assert!(append(projection_entry(0, "zero")).is_err());
    assert!(append(projection_entry(2, "first gap")).is_err());
    assert!(
        store
            .read_remote_proxy_history(&opened.local_session_id)
            .unwrap()
            .projection
            .entries
            .is_empty()
    );
    append(projection_entry(1, "one")).unwrap();
    assert!(append(projection_entry(1, "one")).is_err());
    assert!(append(projection_entry(3, "gap")).is_err());
    let mut another_epoch = projection_entry(2, "wrong epoch");
    another_epoch.journal_epoch = "journal-2".into();
    assert!(append(another_epoch).is_err());
    append(projection_entry(2, "two")).unwrap();
    assert!(append(projection_entry(4, "no-content gap")).is_err());

    let mut epoch_two_first = projection_entry(1, "new epoch one");
    epoch_two_first.journal_epoch = "journal-2".into();
    let mut epoch_two_second = projection_entry(2, "new epoch two");
    epoch_two_second.journal_epoch = "journal-2".into();
    let snapshot = verified_snapshot(
        &opened,
        &live,
        AuthenticatedProjection {
            schema_version: 1,
            entries: vec![epoch_two_first.clone(), epoch_two_second.clone()],
        },
        AuthenticatedCursor {
            journal_epoch: "journal-2".into(),
            sequence: 2,
        },
    )
    .unwrap();
    store
        .replace_remote_projection_from_authenticated_snapshot(snapshot)
        .unwrap();
    assert!(append(projection_entry(3, "old epoch replay")).is_err());
    let view = store
        .read_remote_proxy_history(&opened.local_session_id)
        .unwrap();
    assert_eq!(
        view.projection.entries,
        vec![epoch_two_first, epoch_two_second]
    );
    assert_eq!(
        view.authenticated_cursor.as_ref().unwrap().journal_epoch,
        "journal-2"
    );
    assert_eq!(
        store
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .retired_epochs,
        vec!["journal-1"]
    );

    let mut old_snapshot = AuthenticatedProjection::default();
    old_snapshot.entries = vec![projection_entry(1, "one"), projection_entry(2, "two")];
    let old_cursor = AuthenticatedCursor {
        journal_epoch: "journal-1".into(),
        sequence: 2,
    };
    let old = verified_snapshot(&opened, &live, old_snapshot.clone(), old_cursor.clone()).unwrap();
    let file = temp.path().join("sessions/_remote_proxies.json");
    let before = std::fs::read(&file).unwrap();
    assert!(
        store
            .replace_remote_projection_from_authenticated_snapshot(old)
            .is_err()
    );
    assert_eq!(std::fs::read(&file).unwrap(), before);

    // A failed atomic persistence cannot publish a new epoch/cursor in memory.
    let mut epoch_three = projection_entry(1, "epoch three");
    epoch_three.journal_epoch = "journal-3".into();
    let epoch_three_snapshot = verified_snapshot(
        &opened,
        &live,
        AuthenticatedProjection {
            schema_version: 1,
            entries: vec![epoch_three],
        },
        AuthenticatedCursor {
            journal_epoch: "journal-3".into(),
            sequence: 1,
        },
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
            .replace_remote_projection_from_authenticated_snapshot(epoch_three_snapshot)
            .is_err()
    );
    store.remote_proxies.lock().path = original_path;
    assert_eq!(
        store
            .read_remote_proxy_history(&opened.local_session_id)
            .unwrap(),
        view
    );
    assert_eq!(std::fs::read(&file).unwrap(), before);

    let restarted = make_store(temp.path());
    assert_eq!(
        restarted
            .read_remote_proxy_history(&opened.local_session_id)
            .unwrap(),
        view
    );
    assert_eq!(
        restarted
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .retired_epochs,
        vec!["journal-1"]
    );
    // Simulate a future trusted rebind after restart: retired epochs remain a
    // durable independent guard, even when a live binding is again present.
    {
        let mut state = restarted.remote_proxies.lock();
        state.current_context = Some(live.clone());
        state.openings.insert(
            opened.local_session_id.clone(),
            super::binding::LiveRemoteBinding {
                context: live.clone(),
                confirmed: true,
            },
        );
    }
    let old_again = verified_snapshot(&opened, &live, old_snapshot, old_cursor).unwrap();
    assert!(
        restarted
            .replace_remote_projection_from_authenticated_snapshot(old_again)
            .is_err()
    );
    assert_eq!(std::fs::read(&file).unwrap(), before);
}

#[test]
fn snapshot_rejects_mixed_epoch_gap_and_cursor_mismatch_before_write() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
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
    let original = store
        .read_remote_proxy_history(&opened.local_session_id)
        .unwrap();
    let cursor = AuthenticatedCursor {
        journal_epoch: "journal-2".into(),
        sequence: 3,
    };
    let mut mixed = projection_entry(2, "mixed");
    mixed.journal_epoch = "journal-1".into();
    let new_entry = |sequence, body| {
        let mut entry = projection_entry(sequence, body);
        entry.journal_epoch = "journal-2".into();
        entry
    };
    for entries in [
        vec![new_entry(1, "a"), mixed, new_entry(3, "c")],
        vec![new_entry(1, "a"), new_entry(3, "c")],
        vec![new_entry(1, "a"), new_entry(2, "b")],
    ] {
        assert!(
            verified_snapshot(
                &opened,
                &live,
                AuthenticatedProjection {
                    schema_version: 1,
                    entries,
                },
                cursor.clone()
            )
            .is_err()
        );
    }
    assert_eq!(
        store
            .read_remote_proxy_history(&opened.local_session_id)
            .unwrap(),
        original
    );
}

fn pending_request(client: &str, task: &str) -> PendingRemoteRequest {
    PendingRemoteRequest {
        client_request_id: client.into(),
        task_id: task.into(),
        request_fingerprint: "a".repeat(64),
        dispatch_state: PendingDispatchState::PreparedNeverAttempted,
    }
}

#[test]
fn dispatch_barrier_and_terminal_retirement_are_durable_and_irreversible() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
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
    store
        .mark_remote_dispatch_attempt(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            "request-1",
            "task-1",
            &pending.request_fingerprint,
        )
        .unwrap();
    assert!(
        store
            .mark_remote_dispatch_attempt(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                "request-1",
                "task-1",
                &pending.request_fingerprint
            )
            .is_err()
    );
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
    store
        .retire_remote_pending_terminal(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            "b-session",
            "request-1",
            "task-1",
            &pending.request_fingerprint,
        )
        .unwrap();
    drop(store);
    let restarted = make_store(temp.path());
    let retired = &restarted
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap()
        .retired_requests[0];
    assert_eq!(retired.disposition, RetiredRequestDisposition::Terminal);
    assert_eq!(retired.request_fingerprint, pending.request_fingerprint);
    let another = restarted.open_remote_proxy(None, &live).unwrap();
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
fn never_attempted_abandon_is_atomic_and_retires_both_ids_across_records() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
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
    let another = restarted.open_remote_proxy(None, &live).unwrap();
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
fn v2_pending_migrates_to_possibly_attempted_without_releasing_ids() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let opened = store.open_remote_proxy(None, &context()).unwrap();
    let path = temp.path().join("sessions/_remote_proxies.json");
    drop(store);
    let mut file: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    file["schema_version"] = 2.into();
    file["records"][0]["schema_version"] = 2.into();
    file["records"][0]
        .as_object_mut()
        .unwrap()
        .remove("retired_requests");
    file["records"][0]["pending"] = serde_json::json!([{
        "client_request_id": "request-1",
        "task_id": "task-1",
        "request_fingerprint": "a".repeat(64),
        "outcome_unknown": false
    }]);
    std::fs::write(&path, serde_json::to_vec(&file).unwrap()).unwrap();
    let migrated = make_store(temp.path());
    let pending = &migrated
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap()
        .pending[0];
    assert_eq!(
        pending.dispatch_state,
        PendingDispatchState::AttemptMayHaveOccurred
    );
    assert!(
        migrated
            .abandon_unattempted_remote_pending(
                &opened.local_session_id,
                "request-1",
                "task-1",
                &"a".repeat(64)
            )
            .is_err()
    );
    let rewritten: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    assert_eq!(rewritten["schema_version"], 3);
    assert_eq!(
        rewritten["records"][0]["pending"][0]["dispatch_state"],
        "attempt_may_have_occurred"
    );
    assert!(
        rewritten["records"][0]["pending"][0]
            .get("outcome_unknown")
            .is_none()
    );
}

#[test]
fn v3_rejects_incomplete_or_conflicting_outbox_contract_without_disk_rewrite() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    store.open_remote_proxy(None, &context()).unwrap();
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
        assert!(SessionStore::boot_at_test_dir(temp.path()).is_err());
        assert_eq!(std::fs::read(&path).unwrap(), bytes);
    }
}

#[test]
fn terminal_retirement_rechecks_live_b_mapping_and_correlation_under_write_lock() {
    let temp = tempfile::tempdir().unwrap();
    let store = make_store(temp.path());
    let live = context();
    let opened = store.open_remote_proxy(None, &live).unwrap();
    store
        .confirm_remote_mapping(
            &opened.local_session_id,
            &live,
            &AuthenticatedMapping {
                remote_session_ref: opened.remote_session_ref.clone(),
                controlled_session_id: "b-session-1".into(),
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
    store
        .mark_remote_dispatch_attempt(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            "request-1",
            "task-1",
            &pending.request_fingerprint,
        )
        .unwrap();
    let path = temp.path().join("sessions/_remote_proxies.json");
    let before = std::fs::read(&path).unwrap();
    let retire = |context: &LiveRemoteContext,
                  remote_ref: &str,
                  b_session: &str,
                  client_id: &str,
                  task_id: &str,
                  fingerprint: &str| {
        store.retire_remote_pending_terminal(
            &opened.local_session_id,
            remote_ref,
            context,
            b_session,
            client_id,
            task_id,
            fingerprint,
        )
    };
    let mut stale_generation = live.clone();
    stale_generation.generation = "stale-generation".into();
    let mut wrong_lease = live.clone();
    wrong_lease.controller_lease_id = "wrong-lease".into();
    let mut wrong_connection = live.clone();
    wrong_connection.connection_id = "wrong-connection".into();
    for changed in [&stale_generation, &wrong_lease, &wrong_connection] {
        assert!(
            retire(
                changed,
                &opened.remote_session_ref,
                "b-session-1",
                "request-1",
                "task-1",
                &pending.request_fingerprint
            )
            .is_err()
        );
    }
    let wrong_fingerprint = "b".repeat(64);
    for (remote_ref, b_session, client_id, task_id, fingerprint) in [
        (
            "foreign-ref",
            "b-session-1",
            "request-1",
            "task-1",
            pending.request_fingerprint.as_str(),
        ),
        (
            opened.remote_session_ref.as_str(),
            "foreign-b-session",
            "request-1",
            "task-1",
            pending.request_fingerprint.as_str(),
        ),
        (
            opened.remote_session_ref.as_str(),
            "b-session-1",
            "foreign-request",
            "task-1",
            pending.request_fingerprint.as_str(),
        ),
        (
            opened.remote_session_ref.as_str(),
            "b-session-1",
            "request-1",
            "foreign-task",
            pending.request_fingerprint.as_str(),
        ),
        (
            opened.remote_session_ref.as_str(),
            "b-session-1",
            "request-1",
            "task-1",
            wrong_fingerprint.as_str(),
        ),
    ] {
        assert!(
            retire(
                &live,
                remote_ref,
                b_session,
                client_id,
                task_id,
                fingerprint
            )
            .is_err()
        );
    }
    assert_eq!(std::fs::read(&path).unwrap(), before);
    assert_eq!(
        store
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap()
            .pending
            .len(),
        1
    );

    // A read-only validation is not a grant: disconnect between validation
    // and write must make the atomic terminal operation fail closed.
    store
        .require_live_remote_mutation(&opened.local_session_id, &opened.remote_session_ref, &live)
        .unwrap();
    store.disconnect_remote_proxy(&opened.local_session_id);
    assert!(
        retire(
            &live,
            &opened.remote_session_ref,
            "b-session-1",
            "request-1",
            "task-1",
            &pending.request_fingerprint
        )
        .is_err()
    );
    assert_eq!(std::fs::read(&path).unwrap(), before);

    let mut rebound = live.clone();
    rebound.connection_id = "connection-2".into();
    rebound.lease_epoch = "epoch-2".into();
    rebound.generation = "generation-2".into();
    rebound.control_connected = true;
    rebound.encrypted_channel_connected = true;
    store
        .resume_remote_proxy(&opened.local_session_id, &rebound)
        .unwrap();
    store
        .confirm_remote_mapping(
            &opened.local_session_id,
            &rebound,
            &AuthenticatedMapping {
                remote_session_ref: opened.remote_session_ref.clone(),
                controlled_session_id: "b-session-1".into(),
            },
        )
        .unwrap();
    assert!(
        retire(
            &live,
            &opened.remote_session_ref,
            "b-session-1",
            "request-1",
            "task-1",
            &pending.request_fingerprint
        )
        .is_err()
    );
    assert_eq!(std::fs::read(&path).unwrap(), before);

    // Persistence failure rolls back the in-memory retirement as well.
    let original_path = {
        let mut state = store.remote_proxies.lock();
        std::mem::replace(
            &mut state.path,
            temp.path().join("missing-parent/remote.json"),
        )
    };
    assert!(
        retire(
            &rebound,
            &opened.remote_session_ref,
            "b-session-1",
            "request-1",
            "task-1",
            &pending.request_fingerprint
        )
        .is_err()
    );
    store.remote_proxies.lock().path = original_path;
    assert_eq!(std::fs::read(&path).unwrap(), before);
    let record = store
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap();
    assert_eq!(record.pending.len(), 1);
    assert!(record.retired_requests.is_empty());
    retire(
        &rebound,
        &opened.remote_session_ref,
        "b-session-1",
        "request-1",
        "task-1",
        &pending.request_fingerprint,
    )
    .unwrap();
    let record = store
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap();
    assert!(record.pending.is_empty());
    assert_eq!(record.retired_requests.len(), 1);
}
