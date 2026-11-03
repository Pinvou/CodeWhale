use super::archive_snapshot::ArchiveSnapshot;
use super::binding::{AuthenticatedMapping, LiveRemoteContext, RemoteOpen};
use super::schema::{
    MAX_PENDING_REQUESTS, MAX_REMOTE_PROXY_RECORDS, MAX_RETIRED_REQUESTS, PendingDispatchState,
    PendingRemoteRequest, REMOTE_PROXY_SCHEMA_VERSION, RemoteProxyCapacityError, RemoteProxyFile,
    RemoteProxyRecord, RetiredRemoteRequest, RetiredRequestDisposition, remote_proxy_capacity_code,
};
use crate::features::sessions::SessionStore;

fn context() -> LiveRemoteContext {
    LiveRemoteContext {
        peer_id: "quarantine-peer".into(),
        resume_boundary_id: "quarantine-boundary".into(),
        connection_id: "quarantine-connection".into(),
        controller_lease_id: "quarantine-lease".into(),
        lease_epoch: "quarantine-lease-epoch".into(),
        relay_epoch: "quarantine-relay-epoch".into(),
        generation: "quarantine-generation".into(),
        control_connected: true,
        encrypted_channel_connected: true,
    }
}

// New request admission requires B's authenticated mapping acknowledgement.
// Exercise the real confirmation producer, rather than overriding its guards.
fn confirmed_open(store: &SessionStore, context: &LiveRemoteContext) -> anyhow::Result<RemoteOpen> {
    let opened = store.test_commit_remote_proxy_open(None, context)?;
    store.confirm_remote_mapping(
        &opened.local_session_id,
        context,
        &AuthenticatedMapping {
            remote_session_ref: opened.remote_session_ref.clone(),
            controlled_session_id: format!("controlled-{}", opened.local_session_id),
        },
    )?;
    Ok(opened)
}

fn request(index: usize, attempted: bool) -> PendingRemoteRequest {
    PendingRemoteRequest {
        client_request_id: format!("pending-client-{index}"),
        task_id: format!("pending-task-{index}"),
        request_fingerprint: "a".repeat(64),
        reservation_fingerprint: None,
        dispatch_state: if attempted {
            PendingDispatchState::AttemptMayHaveOccurred
        } else {
            PendingDispatchState::PreparedNeverAttempted
        },
    }
}

/// The historical loader accepts independent R/P maxima. Seed a normal record
/// through its real producer, then load the explicitly supported legacy shape;
/// do not pretend current admission can produce R4096/P64.
fn load_legacy(
    root: &std::path::Path,
    retired: usize,
    pending: usize,
    active_count: usize,
) -> (SessionStore, RemoteProxyRecord) {
    let seed = SessionStore::boot_at_test_dir(root).unwrap();
    let opened = confirmed_open(&seed, &context()).unwrap();
    let mut record = seed
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap();
    record.ui.title = "Frozen historical conversation".into();
    record.retired_requests = (0..retired)
        .map(|index| RetiredRemoteRequest {
            client_request_id: format!("retired-client-{index}"),
            task_id: format!("retired-task-{index}"),
            request_fingerprint: "b".repeat(64),
            disposition: RetiredRequestDisposition::Terminal,
        })
        .collect();
    record.pending = (0..pending)
        .map(|index| request(index, index % 2 == 1))
        .collect();
    record.validate_stored().unwrap();
    let mut records = vec![record.clone()];
    for _ in 1..active_count {
        let mut other = record.clone();
        other.local_session_id = format!("remote-{}", uuid::Uuid::new_v4());
        other.remote_session_ref = format!("ref-{}", uuid::Uuid::new_v4());
        other.retired_requests.clear();
        other.pending.clear();
        records.push(other);
    }
    drop(seed);
    std::fs::write(
        root.join("sessions/_remote_proxies.json"),
        serde_json::to_vec(&RemoteProxyFile {
            schema_version: REMOTE_PROXY_SCHEMA_VERSION,
            records,
        })
        .unwrap(),
    )
    .unwrap();
    let restored = SessionStore::boot_at_test_dir(root).unwrap();
    assert!(!restored.remote_proxies.lock().load_failed);
    assert_eq!(
        restored.remote_proxy_snapshot(&record.local_session_id),
        Some(record.clone())
    );
    (restored, record)
}

#[test]
fn quarantine_legacy_capacity_matrix_preserves_facts_and_permanently_rejects_old_ids() {
    for (retired, pending) in [(4095, 1), (4095, 2), (4096, 0), (4096, 1), (4096, 64)] {
        let temp = tempfile::tempdir().unwrap();
        let (store, original) = load_legacy(temp.path(), retired, pending, 1);
        let id = &original.local_session_id;
        if pending == 0 {
            assert!(store.archive_remote_proxy(id, None).unwrap());
        } else {
            assert!(store.quarantine_remote_proxy(id, None, true).unwrap());
        }
        assert!(store.remote_proxy_snapshot(id).is_none());
        assert!(store.remote_proxies.lock().openings.get(id).is_none());
        let snapshot = store
            .remote_proxies
            .lock()
            .archives
            .read_snapshot(id)
            .unwrap()
            .unwrap();
        assert_eq!(snapshot.is_quarantined(), pending != 0);
        assert_eq!(snapshot.record().pending, original.pending);
        assert_eq!(snapshot.record(), &original);
        assert!(
            store
                .prepare_remote_proxy_open(Some(id), &context())
                .is_err()
        );
        assert!(
            store
                .require_live_remote_mutation(id, &original.remote_session_ref, &context())
                .is_err()
        );
        if let Some(old) = original.pending.first() {
            assert!(
                store
                    .abandon_unattempted_remote_pending(
                        id,
                        &old.client_request_id,
                        &old.task_id,
                        &old.request_fingerprint
                    )
                    .is_err()
            );
            assert!(
                store
                    .mark_remote_dispatch_attempt(
                        id,
                        &original.remote_session_ref,
                        &context(),
                        &old.client_request_id,
                        &old.task_id,
                        &old.request_fingerprint
                    )
                    .is_err()
            );
        }
        assert_eq!(
            store
                .remote_proxies
                .lock()
                .archives
                .read(id)
                .unwrap()
                .unwrap(),
            original
        );
        if let Some(attempted) = original
            .pending
            .iter()
            .find(|item| item.dispatch_state == PendingDispatchState::AttemptMayHaveOccurred)
        {
            assert!(
                store
                    .reject_authenticated_remote_request(
                        id,
                        &original.remote_session_ref,
                        &context(),
                        &attempted.task_id,
                        &attempted.client_request_id,
                        "remote_task_rejected",
                    )
                    .is_err(),
                "late authenticated rejection must not rewrite frozen unknown facts"
            );
            assert_eq!(
                store
                    .remote_proxies
                    .lock()
                    .archives
                    .read(id)
                    .unwrap()
                    .unwrap(),
                original
            );
        }
        drop(store);

        let restarted = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        assert!(restarted.remote_proxy_snapshot(id).is_none());
        let view = restarted.read_remote_proxy_history(id).unwrap();
        assert_eq!(view.quarantined, pending != 0);
        assert_eq!(view.pending_unknown_count, pending / 2);
        assert_eq!(
            view.frozen_prepared_count,
            if pending == 0 { 0 } else { pending.div_ceil(2) }
        );
        assert!(
            view.unattempted_pending.is_empty(),
            "frozen facts must not become abandon/resend handles"
        );
        assert!(
            restarted
                .prepare_remote_proxy_open(Some(id), &context())
                .is_err()
        );

        let next = confirmed_open(&restarted, &context()).unwrap();
        assert_ne!(next.local_session_id, *id);
        assert_ne!(next.remote_session_ref, original.remote_session_ref);
        let (old_client, old_task) = original
            .pending
            .first()
            .map(|item| (item.client_request_id.clone(), item.task_id.clone()))
            .unwrap_or_else(|| {
                (
                    original.retired_requests[0].client_request_id.clone(),
                    original.retired_requests[0].task_id.clone(),
                )
            });
        for (client, task) in [
            (old_client, "fresh-task".into()),
            ("fresh-client".into(), old_task),
        ] {
            let mut reused = request(999, false);
            reused.client_request_id = client;
            reused.task_id = task;
            assert!(
                restarted
                    .insert_remote_pending(
                        &next.local_session_id,
                        &next.remote_session_ref,
                        &context(),
                        reused
                    )
                    .is_err()
            );
            assert!(
                restarted
                    .remote_proxy_snapshot(&next.local_session_id)
                    .unwrap()
                    .pending
                    .is_empty()
            );
        }
        restarted
            .insert_remote_pending(
                &next.local_session_id,
                &next.remote_session_ref,
                &context(),
                request(1000, false),
            )
            .unwrap();
        assert_eq!(
            restarted
                .remote_proxy_snapshot(&next.local_session_id)
                .unwrap()
                .pending
                .len(),
            1
        );
        assert!(!restarted.remote_proxies.lock().load_failed);
        assert_eq!(
            restarted
                .remote_proxies
                .lock()
                .archives
                .read(id)
                .unwrap()
                .unwrap(),
            original
        );
    }
}

#[test]
fn quarantine_requires_each_confirmation_and_rejects_a_live_owner_before_sealing() {
    let temp = tempfile::tempdir().unwrap();
    let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
    let live = context();
    let opened = confirmed_open(&store, &live).unwrap();
    store
        .insert_remote_pending(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &live,
            request(0, false),
        )
        .unwrap();
    let original = store
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap();
    let bytes = std::fs::read(temp.path().join("sessions/_remote_proxies.json")).unwrap();
    assert_eq!(
        store
            .quarantine_remote_proxy(&opened.local_session_id, None, false)
            .unwrap_err()
            .to_string(),
        "remote_proxy_quarantine_confirmation_required"
    );
    assert_eq!(
        store
            .quarantine_remote_proxy(&opened.local_session_id, Some(&live), true)
            .unwrap_err()
            .to_string(),
        "remote_proxy_archive_online"
    );
    assert_eq!(
        store
            .archive_remote_proxy(&opened.local_session_id, None)
            .unwrap_err()
            .to_string(),
        "remote_proxy_archive_pending"
    );
    assert_eq!(
        store.remote_proxy_snapshot(&opened.local_session_id),
        Some(original)
    );
    assert_eq!(
        std::fs::read(temp.path().join("sessions/_remote_proxies.json")).unwrap(),
        bytes
    );
    assert!(
        !store
            .is_archived_remote_proxy(&opened.local_session_id)
            .unwrap()
    );
    assert!(
        store
            .quarantine_remote_proxy(&opened.local_session_id, None, true)
            .unwrap()
    );
    assert_eq!(
        store
            .quarantine_remote_proxy(&opened.local_session_id, None, false)
            .unwrap_err()
            .to_string(),
        "remote_proxy_quarantine_confirmation_required"
    );
}

#[test]
fn quarantine_frees_a_full_active_registry_without_resurrecting_the_old_identity() {
    let temp = tempfile::tempdir().unwrap();
    let (store, original) = load_legacy(temp.path(), 4096, 1, MAX_REMOTE_PROXY_RECORDS);
    assert!(confirmed_open(&store, &context()).is_err());
    assert_eq!(
        store.remote_proxies.lock().records.len(),
        MAX_REMOTE_PROXY_RECORDS
    );
    assert!(
        store
            .quarantine_remote_proxy(&original.local_session_id, None, true)
            .unwrap()
    );
    assert_eq!(
        store.remote_proxies.lock().records.len(),
        MAX_REMOTE_PROXY_RECORDS - 1
    );
    let next = confirmed_open(&store, &context()).unwrap();
    assert_ne!(next.local_session_id, original.local_session_id);
    assert_eq!(
        store.remote_proxies.lock().records.len(),
        MAX_REMOTE_PROXY_RECORDS
    );
    drop(store);
    let restarted = SessionStore::boot_at_test_dir(temp.path()).unwrap();
    assert!(!restarted.remote_proxies.lock().load_failed);
    assert_eq!(
        restarted.remote_proxies.lock().records.len(),
        MAX_REMOTE_PROXY_RECORDS
    );
    assert!(
        restarted
            .read_remote_proxy_history(&original.local_session_id)
            .unwrap()
            .quarantined
    );
    assert!(
        restarted
            .prepare_remote_proxy_open(Some(&original.local_session_id), &context())
            .is_err()
    );
}

#[test]
fn quarantine_cleanup_io_failure_keeps_a_permanent_seal_and_recovers_on_the_next_write() {
    let temp = tempfile::tempdir().unwrap();
    let (store, original) = load_legacy(temp.path(), 4096, 2, 1);
    let path = temp.path().join("sessions/_remote_proxies.json");
    let saved = temp
        .path()
        .join("sessions/_remote_proxies.cleanup-residue.json");
    let before = std::fs::read(&path).unwrap();
    std::fs::rename(&path, &saved).unwrap();
    std::fs::create_dir(&path).unwrap();
    assert!(
        !store
            .quarantine_remote_proxy(&original.local_session_id, None, true)
            .unwrap()
    );
    assert!(
        store
            .remote_proxy_snapshot(&original.local_session_id)
            .is_none()
    );
    assert!(!store.remote_proxies.lock().load_failed);
    assert_eq!(
        store
            .remote_proxies
            .lock()
            .archives
            .read(&original.local_session_id)
            .unwrap()
            .unwrap(),
        original
    );
    assert!(
        store
            .prepare_remote_proxy_open(Some(&original.local_session_id), &context())
            .is_err()
    );
    // Remove only our exact empty blocker and restore the original residue.
    // The production seal, SQLite data and watermark are never rolled back.
    std::fs::remove_dir(&path).unwrap();
    std::fs::rename(&saved, &path).unwrap();
    assert_eq!(std::fs::read(&path).unwrap(), before);
    let successor = confirmed_open(&store, &context()).unwrap();
    assert_ne!(successor.local_session_id, original.local_session_id);
    let cleaned: RemoteProxyFile = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    assert!(
        cleaned
            .records
            .iter()
            .all(|record| record.local_session_id != original.local_session_id)
    );
    drop(store);
    let restarted = SessionStore::boot_at_test_dir(temp.path()).unwrap();
    assert!(!restarted.remote_proxies.lock().load_failed);
    assert_eq!(
        restarted
            .remote_proxies
            .lock()
            .archives
            .read(&original.local_session_id)
            .unwrap()
            .unwrap(),
        original
    );
    assert!(
        restarted
            .prepare_remote_proxy_open(Some(&original.local_session_id), &context())
            .is_err()
    );
    assert!(
        restarted
            .remote_proxy_snapshot(&successor.local_session_id)
            .is_some()
    );
}

#[test]
fn quarantine_envelope_has_independent_headroom_for_an_exact_full_legacy_file() {
    let temp = tempfile::tempdir().unwrap();
    let (store, mut original) = load_legacy(temp.path(), 4096, 1, 1);
    drop(store);
    let mut file = RemoteProxyFile {
        schema_version: REMOTE_PROXY_SCHEMA_VERSION,
        records: vec![original.clone()],
    };
    let initial = serde_json::to_vec(&file).unwrap().len();
    let maximum = super::schema::MAX_REMOTE_PROXY_FILE_BYTES;
    original.peer_id.push_str(&"p".repeat(maximum - initial));
    file.records[0] = original.clone();
    let bytes = serde_json::to_vec(&file).unwrap();
    assert_eq!(bytes.len(), maximum);
    std::fs::write(temp.path().join("sessions/_remote_proxies.json"), bytes).unwrap();
    let loaded = SessionStore::boot_at_test_dir(temp.path()).unwrap();
    assert!(!loaded.remote_proxies.lock().load_failed);
    assert!(
        loaded
            .quarantine_remote_proxy(&original.local_session_id, None, true)
            .unwrap()
    );
    assert_eq!(
        loaded
            .remote_proxies
            .lock()
            .archives
            .read(&original.local_session_id)
            .unwrap()
            .unwrap(),
        original
    );
    drop(loaded);
    let restarted = SessionStore::boot_at_test_dir(temp.path()).unwrap();
    assert!(!restarted.remote_proxies.lock().load_failed);
    assert!(
        restarted
            .read_remote_proxy_history(&original.local_session_id)
            .unwrap()
            .quarantined
    );
    assert!(
        restarted
            .prepare_remote_proxy_open(Some(&original.local_session_id), &context())
            .is_err()
    );
    assert!(confirmed_open(&restarted, &context()).is_ok());
}

#[test]
fn capacity_reasons_are_typed_and_distinguish_retirement_budget_pending_limit_and_io() {
    for (retired, pending, expected) in [
        (
            0,
            MAX_PENDING_REQUESTS,
            RemoteProxyCapacityError::PendingLimit,
        ),
        (
            MAX_RETIRED_REQUESTS,
            0,
            RemoteProxyCapacityError::RetirementBudget,
        ),
        (
            MAX_RETIRED_REQUESTS - MAX_PENDING_REQUESTS,
            MAX_PENDING_REQUESTS,
            RemoteProxyCapacityError::RetirementBudget,
        ),
    ] {
        let temp = tempfile::tempdir().unwrap();
        let (store, original) = load_legacy(temp.path(), retired, pending, 1);
        // Loaded history has no authority. This assertion concerns the real
        // admission gate, so explicitly install a fresh live test opening.
        let rebound = confirmed_open(&store, &context()).unwrap();
        {
            let mut state = store.remote_proxies.lock();
            let mut record = state.records[&rebound.local_session_id].clone();
            record.pending = original.pending.clone();
            record.retired_requests = original.retired_requests.clone();
            state.records.remove(&original.local_session_id);
            state
                .records
                .insert(rebound.local_session_id.clone(), record);
            state.persist().unwrap();
        }
        let before = store
            .remote_proxy_snapshot(&rebound.local_session_id)
            .unwrap();
        let failure = store
            .insert_remote_pending(
                &rebound.local_session_id,
                &rebound.remote_session_ref,
                &context(),
                request(9000, false),
            )
            .unwrap_err();
        assert_eq!(remote_proxy_capacity_code(&failure), Some(expected.code()));
        assert_eq!(
            store.remote_proxy_snapshot(&rebound.local_session_id),
            Some(before)
        );
    }
    assert_eq!(
        remote_proxy_capacity_code(&anyhow::anyhow!("remote_retirement_budget_exhausted")),
        None,
        "arbitrary error text must not masquerade as a typed capacity refusal"
    );
    assert_eq!(
        remote_proxy_capacity_code(
            &std::io::Error::from(std::io::ErrorKind::PermissionDenied).into()
        ),
        None
    );
}

#[test]
fn quarantine_format_is_explicit_preserves_pending_and_keeps_legacy_archives_unchanged() {
    let temp = tempfile::tempdir().unwrap();
    let (_store, mut record) = load_legacy(temp.path(), 1, 1, 1);
    assert!(
        ArchiveSnapshot::Archived(record.clone())
            .validate()
            .is_err()
    );
    let quarantine = ArchiveSnapshot::quarantined(record.clone());
    quarantine.validate().unwrap();
    let bytes = serde_json::to_vec(&quarantine).unwrap();
    assert!(serde_json::from_slice::<RemoteProxyRecord>(&bytes).is_err());
    let roundtrip: ArchiveSnapshot = serde_json::from_slice(&bytes).unwrap();
    assert!(roundtrip.is_quarantined());
    assert_eq!(roundtrip.record(), &record);
    let mut future: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    future["quarantine_schema_version"] = 2.into();
    assert!(
        serde_json::from_value::<ArchiveSnapshot>(future)
            .unwrap()
            .validate()
            .is_err()
    );
    record.pending.clear();
    let plain = serde_json::to_vec(&record).unwrap();
    assert_eq!(
        serde_json::to_vec(&ArchiveSnapshot::Archived(record.clone())).unwrap(),
        plain
    );
    let old: ArchiveSnapshot = serde_json::from_slice(&plain).unwrap();
    old.validate().unwrap();
    assert!(!old.is_quarantined());
    assert_eq!(old.record(), &record);
}
