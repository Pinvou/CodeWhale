//! Atomic, idempotent SessionStore writes for authenticated A-side projections.
//! Caller authentication is mandatory; this layer rechecks current A/B binding
//! in the same lock as cursor changes and disk persistence.

use anyhow::{Result, bail};
use std::collections::HashSet;

use super::binding::{LiveRemoteContext, require_live_mapping_locked};
use super::persistence::persist_changed;
use super::schema::{
    AuthenticatedCursor, MAX_PROJECTION_BYTES, MAX_PROJECTION_ENTRIES, MAX_RETIRED_EPOCHS,
    MAX_RETIRED_REQUESTS, PendingDispatchState, PendingRemoteRequest, ProjectionEntry,
    ProjectionKind, RemoteProxyRecord, RetiredRemoteRequest, RetiredRequestDisposition,
    SnapshotCandidate, SnapshotVerificationPort, validate_snapshot_window,
};
use crate::features::sessions::SessionStore;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ProjectionWrite {
    Inserted,
    Unchanged,
}

pub(crate) enum TerminalPendingCorrelation {
    NoPending,
    Matched(PendingRemoteRequest),
}

fn require_next(record: &RemoteProxyRecord, entry: &ProjectionEntry) -> Result<()> {
    match &record.authenticated_cursor {
        None if entry.sequence == 1 => Ok(()),
        Some(previous)
            if previous.journal_epoch == entry.journal_epoch
                && previous.sequence.checked_add(1) == Some(entry.sequence) =>
        {
            Ok(())
        }
        _ => bail!("remote journal cursor is not the next event"),
    }
}

fn trim_projection_window(record: &mut RemoteProxyRecord) -> Result<()> {
    while record.projection.entries.len() > MAX_PROJECTION_ENTRIES
        || serde_json::to_vec(&record.projection)?.len() > MAX_PROJECTION_BYTES
    {
        if record.projection.entries.is_empty() {
            bail!("remote projection entry exceeds bounded window");
        }
        record.projection.entries.remove(0);
    }
    // The durable cursor remains the newest authenticated sequence. Removed
    // display text is never authority for replay/ACK or task id reuse.
    record.projection.validate()
}

fn retire_snapshot_terminals(
    record: &mut RemoteProxyRecord,
    projection: &super::schema::AuthenticatedProjection,
) -> Result<()> {
    let mut terminal_tasks = HashSet::new();
    for entry in &projection.entries {
        if entry.kind != ProjectionKind::Status
            || !matches!(
                entry.body.as_deref(),
                Some("succeeded" | "failed" | "cancelled" | "interrupted")
            )
        {
            continue;
        }
        let task_id = entry
            .task_id
            .as_deref()
            .ok_or_else(|| anyhow::anyhow!("remote snapshot terminal task missing"))?;
        if !terminal_tasks.insert(task_id) {
            bail!("remote snapshot contains duplicate terminal task");
        }
        let matches: Vec<_> = record
            .pending
            .iter()
            .enumerate()
            .filter(|(_, pending)| pending.task_id == task_id)
            .collect();
        if matches.len() > 1 {
            bail!("remote snapshot terminal correlation is ambiguous");
        }
        let Some((index, pending)) = matches.first() else {
            // B-local tasks and previously retired A tasks carry no live A
            // outbox record. Their authenticated status remains display-only.
            continue;
        };
        if pending.dispatch_state != PendingDispatchState::AttemptMayHaveOccurred
            || record.retired_requests.len() >= MAX_RETIRED_REQUESTS
        {
            bail!("remote snapshot terminal correlation or capacity mismatch");
        }
        let retired = RetiredRemoteRequest {
            client_request_id: pending.client_request_id.clone(),
            task_id: pending.task_id.clone(),
            request_fingerprint: pending.request_fingerprint.clone(),
            disposition: RetiredRequestDisposition::Terminal,
        };
        record.pending.remove(*index);
        record.retired_requests.push(retired);
    }
    Ok(())
}

impl SessionStore {
    /// Correlation for an authenticated B terminal is sourced from the
    /// persisted A outbox, never from caller-supplied wire fields. The writer
    /// repeats these checks while retiring under its own transaction lock.
    pub(crate) fn pending_terminal_for_live_mapping(
        &self,
        local_session_id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        controlled_session_id: &str,
        task_id: &str,
    ) -> Result<TerminalPendingCorrelation> {
        let state = self.remote_proxies.lock();
        require_live_mapping_locked(
            &state,
            local_session_id,
            remote_ref,
            context,
            controlled_session_id,
        )?;
        let mut matches = state
            .records
            .get(local_session_id)
            .expect("checked")
            .pending
            .iter()
            .filter(|pending| pending.task_id == task_id);
        let Some(pending) = matches.next() else {
            return Ok(TerminalPendingCorrelation::NoPending);
        };
        if matches.next().is_some()
            || pending.dispatch_state != PendingDispatchState::AttemptMayHaveOccurred
        {
            bail!("remote terminal pending correlation is ambiguous or unattempted");
        }
        Ok(TerminalPendingCorrelation::Matched(pending.clone()))
    }

    /// Read-only capability gate: validate the exact current A/B binding in
    /// one lock acquisition, including connection and encrypted channel.
    pub(crate) fn require_live_remote_projection_mapping(
        &self,
        local_session_id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        controlled_session_id: &str,
    ) -> Result<()> {
        let state = self.remote_proxies.lock();
        require_live_mapping_locked(
            &state,
            local_session_id,
            remote_ref,
            context,
            controlled_session_id,
        )
    }

    /// One event key has one immutable content. Repeated authenticated delivery
    /// is a no-op, not another disk write or UI notification.
    pub(crate) fn project_authenticated_remote_entry(
        &self,
        local_session_id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        controlled_session_id: &str,
        entry: ProjectionEntry,
    ) -> Result<ProjectionWrite> {
        entry.validate()?;
        let mut state = self.remote_proxies.lock();
        require_live_mapping_locked(
            &state,
            local_session_id,
            remote_ref,
            context,
            controlled_session_id,
        )?;
        let mut record = state
            .records
            .get(local_session_id)
            .expect("checked")
            .clone();
        if let Some(previous) = record.projection.entries.iter().find(|old| {
            (old.journal_epoch == entry.journal_epoch && old.sequence == entry.sequence)
                || old.event_id == entry.event_id
        }) {
            if previous == &entry {
                return Ok(ProjectionWrite::Unchanged);
            }
            bail!("remote journal event key or id was reused with different content");
        }
        require_next(&record, &entry)?;
        if record.retired_epochs.contains(&entry.journal_epoch) {
            bail!("retired journal epoch cannot be replayed");
        }
        record.authenticated_cursor = Some(AuthenticatedCursor {
            journal_epoch: entry.journal_epoch.clone(),
            sequence: entry.sequence,
        });
        record.projection.entries.push(entry);
        trim_projection_window(&mut record)?;
        persist_changed(&mut state, local_session_id, record)?;
        Ok(ProjectionWrite::Inserted)
    }

    /// Verify the snapshot proof before entering the write lock, then check
    /// the exact current B mapping again and atomically replace the cache.
    /// Equal snapshots at the same cursor do not persist or notify twice.
    pub(crate) fn project_authenticated_remote_snapshot(
        &self,
        local_session_id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        controlled_session_id: &str,
        candidate: SnapshotCandidate,
        verifier: &dyn SnapshotVerificationPort,
    ) -> Result<ProjectionWrite> {
        validate_snapshot_window(&candidate.projection, &candidate.cursor)?;
        verifier.verify_authenticated_snapshot(
            local_session_id,
            remote_ref,
            context,
            &candidate,
        )?;
        let mut ids = HashSet::new();
        if candidate
            .projection
            .entries
            .iter()
            .any(|entry| !ids.insert(&entry.event_id))
        {
            bail!("duplicate remote snapshot event id");
        }
        let mut state = self.remote_proxies.lock();
        require_live_mapping_locked(
            &state,
            local_session_id,
            remote_ref,
            context,
            controlled_session_id,
        )?;
        let mut record = state
            .records
            .get(local_session_id)
            .expect("checked")
            .clone();
        if record
            .retired_epochs
            .contains(&candidate.cursor.journal_epoch)
        {
            bail!("retired journal epoch cannot be restored");
        }
        if let Some(previous) = &record.authenticated_cursor {
            if previous.journal_epoch == candidate.cursor.journal_epoch {
                if candidate.cursor.sequence < previous.sequence {
                    bail!("remote snapshot cursor moved backwards");
                }
                if candidate.cursor.sequence == previous.sequence {
                    if candidate.projection == record.projection {
                        return Ok(ProjectionWrite::Unchanged);
                    }
                    let existing = &record.projection.entries;
                    let incoming = &candidate.projection.entries;
                    // B's 256-KiB byte window may have trimmed more than A's
                    // count/byte window at the same authenticated cursor.
                    // Only a shorter, byte-identical suffix is a valid
                    // replacement; there must be a common cursor-tail event.
                    if incoming.is_empty()
                        || existing.is_empty()
                        || incoming.len() >= existing.len()
                        || incoming.last() != existing.last()
                        || !existing.ends_with(incoming)
                    {
                        bail!("same remote cursor has conflicting snapshot content");
                    }
                }
            } else {
                if record.retired_epochs.len() >= MAX_RETIRED_EPOCHS {
                    bail!("retired journal epoch capacity reached");
                }
                record.retired_epochs.push(previous.journal_epoch.clone());
            }
        }
        retire_snapshot_terminals(&mut record, &candidate.projection)?;
        record.projection = candidate.projection;
        record.authenticated_cursor = Some(candidate.cursor);
        persist_changed(&mut state, local_session_id, record)?;
        Ok(ProjectionWrite::Inserted)
    }

    /// The outer use case must consume DEV-29's VerifiedRemoteTerminal before
    /// invoking this writer. This raw correlation stays private to the
    /// sessions feature; the final write still checks every binding and the
    /// attempted pending record under one mutex before persisting once.
    pub(in crate::features::sessions) fn project_authenticated_remote_terminal(
        &self,
        local_session_id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        controlled_session_id: &str,
        client_request_id: &str,
        task_id: &str,
        fingerprint: &str,
        entry: ProjectionEntry,
    ) -> Result<ProjectionWrite> {
        entry.validate()?;
        if entry.task_id.as_deref() != Some(task_id) || entry.kind != ProjectionKind::Status {
            bail!("terminal task does not match journal entry");
        }
        let mut state = self.remote_proxies.lock();
        require_live_mapping_locked(
            &state,
            local_session_id,
            remote_ref,
            context,
            controlled_session_id,
        )?;
        let mut record = state
            .records
            .get(local_session_id)
            .expect("checked")
            .clone();
        if let Some(previous) = record.projection.entries.iter().find(|old| {
            (old.journal_epoch == entry.journal_epoch && old.sequence == entry.sequence)
                || old.event_id == entry.event_id
        }) {
            let retired = record.retired_requests.iter().any(|item| {
                item.client_request_id == client_request_id
                    && item.task_id == task_id
                    && item.request_fingerprint == fingerprint
                    && item.disposition == RetiredRequestDisposition::Terminal
            });
            if previous == &entry && retired {
                return Ok(ProjectionWrite::Unchanged);
            }
            bail!("terminal event conflicts with prior projection or retirement");
        }
        require_next(&record, &entry)?;
        if record.retired_epochs.contains(&entry.journal_epoch) {
            bail!("retired journal epoch cannot be replayed");
        }
        let index = record
            .pending
            .iter()
            .position(|item| item.client_request_id == client_request_id && item.task_id == task_id)
            .ok_or_else(|| anyhow::anyhow!("unknown remote pending request"))?;
        let pending = &record.pending[index];
        if pending.request_fingerprint != fingerprint
            || pending.dispatch_state != PendingDispatchState::AttemptMayHaveOccurred
            || record.retired_requests.len() >= MAX_RETIRED_REQUESTS
        {
            bail!("terminal correlation or capacity mismatch");
        }
        record.pending.remove(index);
        record.retired_requests.push(RetiredRemoteRequest {
            client_request_id: client_request_id.to_owned(),
            task_id: task_id.to_owned(),
            request_fingerprint: fingerprint.to_owned(),
            disposition: RetiredRequestDisposition::Terminal,
        });
        record.authenticated_cursor = Some(AuthenticatedCursor {
            journal_epoch: entry.journal_epoch.clone(),
            sequence: entry.sequence,
        });
        record.projection.entries.push(entry);
        trim_projection_window(&mut record)?;
        persist_changed(&mut state, local_session_id, record)?;
        Ok(ProjectionWrite::Inserted)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::sessions::remote_proxy::binding::AuthenticatedMapping;
    use crate::features::sessions::remote_proxy::schema::{
        AuthenticatedProjection, PendingRemoteRequest, ProjectionKind,
    };

    fn live() -> LiveRemoteContext {
        LiveRemoteContext {
            peer_id: "peer-1".into(),
            resume_boundary_id: "resume-1".into(),
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "epoch-1".into(),
            relay_epoch: "relay-1".into(),
            generation: "generation-1".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        }
    }

    fn opened(store: &SessionStore, live: &LiveRemoteContext) -> (String, String) {
        let open = store.test_commit_remote_proxy_open(None, live).unwrap();
        store
            .confirm_remote_mapping(
                &open.local_session_id,
                live,
                &AuthenticatedMapping {
                    remote_session_ref: open.remote_session_ref.clone(),
                    controlled_session_id: "b-session-1".into(),
                },
            )
            .unwrap();
        (open.local_session_id, open.remote_session_ref)
    }

    fn entry(sequence: u64, body: &str) -> ProjectionEntry {
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

    struct AcceptSnapshot;
    impl SnapshotVerificationPort for AcceptSnapshot {
        fn verify_authenticated_snapshot(
            &self,
            _: &str,
            _: &str,
            _: &LiveRemoteContext,
            _: &SnapshotCandidate,
        ) -> Result<()> {
            Ok(())
        }
    }

    #[test]
    fn event_projection_is_idempotent_and_checks_current_b_binding() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let (local, remote_ref) = opened(&store, &live);
        let first = entry(1, "answer");
        assert_eq!(
            store
                .project_authenticated_remote_entry(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    first.clone(),
                )
                .unwrap(),
            ProjectionWrite::Inserted
        );
        assert_eq!(
            store
                .project_authenticated_remote_entry(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    first.clone(),
                )
                .unwrap(),
            ProjectionWrite::Unchanged
        );
        assert!(
            store
                .project_authenticated_remote_entry(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    entry(1, "tampered"),
                )
                .is_err()
        );
        assert!(
            store
                .project_authenticated_remote_entry(
                    &local,
                    &remote_ref,
                    &live,
                    "other-b",
                    entry(2, "late")
                )
                .is_err()
        );
        store.test_expire_remote_binding(&local);
        assert!(
            store
                .project_authenticated_remote_entry(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    entry(2, "late"),
                )
                .is_err()
        );
        assert_eq!(
            store
                .remote_proxy_snapshot(&local)
                .unwrap()
                .projection
                .entries,
            vec![first]
        );
    }

    #[test]
    fn snapshot_projection_is_idempotent_and_rejects_tampering() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let (local, remote_ref) = opened(&store, &live);
        let snapshot = SnapshotCandidate {
            projection: AuthenticatedProjection {
                schema_version: 1,
                entries: vec![entry(1, "answer")],
            },
            cursor: AuthenticatedCursor {
                journal_epoch: "journal-1".into(),
                sequence: 1,
            },
        };
        assert_eq!(
            store
                .project_authenticated_remote_snapshot(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    snapshot.clone(),
                    &AcceptSnapshot,
                )
                .unwrap(),
            ProjectionWrite::Inserted
        );
        assert_eq!(
            store
                .project_authenticated_remote_snapshot(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    snapshot.clone(),
                    &AcceptSnapshot,
                )
                .unwrap(),
            ProjectionWrite::Unchanged
        );
        let mut changed = snapshot;
        changed.projection.entries[0].body = Some("tampered".into());
        assert!(
            store
                .project_authenticated_remote_snapshot(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    changed,
                    &AcceptSnapshot,
                )
                .is_err()
        );
        assert!(
            store
                .project_authenticated_remote_snapshot(
                    &local,
                    &remote_ref,
                    &live,
                    "other-b",
                    SnapshotCandidate {
                        projection: AuthenticatedProjection::default(),
                        cursor: AuthenticatedCursor {
                            journal_epoch: "journal-2".into(),
                            sequence: 0,
                        },
                    },
                    &AcceptSnapshot,
                )
                .is_err()
        );
    }

    #[test]
    fn same_cursor_accepts_only_exact_shorter_byte_window_suffix() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let (local, remote_ref) = opened(&store, &live);
        let body = "x".repeat(8 * 1024);
        for sequence in 1..=40 {
            store
                .project_authenticated_remote_entry(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    entry(sequence, &body),
                )
                .unwrap();
        }
        let before = store.remote_proxy_snapshot(&local).unwrap();
        assert_eq!(before.projection.entries.len(), 40);
        // The B journal's 256-KiB budget keeps a shorter authenticated tail.
        let shorter = SnapshotCandidate {
            projection: AuthenticatedProjection {
                schema_version: 1,
                entries: before.projection.entries[12..].to_vec(),
            },
            cursor: AuthenticatedCursor {
                journal_epoch: "journal-1".into(),
                sequence: 40,
            },
        };
        let path = temp.path().join("sessions/_remote_proxies.json");
        let disk_before = std::fs::read(&path).unwrap();
        let mut tampered = shorter.clone();
        tampered.projection.entries[0].body = Some("tampered".into());
        assert!(
            store
                .project_authenticated_remote_snapshot(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    tampered,
                    &AcceptSnapshot,
                )
                .is_err()
        );
        assert_eq!(std::fs::read(&path).unwrap(), disk_before);
        assert_eq!(store.remote_proxy_snapshot(&local).unwrap(), before);
        assert_eq!(
            store
                .project_authenticated_remote_snapshot(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    shorter,
                    &AcceptSnapshot,
                )
                .unwrap(),
            ProjectionWrite::Inserted
        );
        let after = store.remote_proxy_snapshot(&local).unwrap();
        assert_eq!(after.projection.entries.len(), 28);
        assert_eq!(after.projection.entries.first().unwrap().sequence, 13);
        assert_eq!(after.authenticated_cursor.unwrap().sequence, 40);
    }

    #[test]
    fn terminal_projection_and_retirement_commit_or_roll_back_together() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let (local, remote_ref) = opened(&store, &live);
        let fingerprint = "a".repeat(64);
        store
            .insert_remote_pending(
                &local,
                &remote_ref,
                &live,
                PendingRemoteRequest {
                    client_request_id: "request-1".into(),
                    task_id: "task-1".into(),
                    request_fingerprint: fingerprint.clone(),
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                    reservation_fingerprint: None,
                },
            )
            .unwrap();
        store
            .mark_remote_dispatch_attempt(
                &local,
                &remote_ref,
                &live,
                "request-1",
                "task-1",
                &fingerprint,
            )
            .unwrap();
        let terminal = ProjectionEntry {
            kind: ProjectionKind::Status,
            body: Some("succeeded".into()),
            ..entry(1, "ignored")
        };
        let path = temp.path().join("sessions/_remote_proxies.json");
        let before = std::fs::read(&path).unwrap();
        let original_path = {
            let mut state = store.remote_proxies.lock();
            std::mem::replace(
                &mut state.path,
                temp.path().join("missing-parent/remote.json"),
            )
        };
        assert!(
            store
                .project_authenticated_remote_terminal(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    "request-1",
                    "task-1",
                    &fingerprint,
                    terminal.clone(),
                )
                .is_err()
        );
        store.remote_proxies.lock().path = original_path;
        assert_eq!(std::fs::read(&path).unwrap(), before);
        let record = store.remote_proxy_snapshot(&local).unwrap();
        assert_eq!(record.pending.len(), 1);
        assert!(record.projection.entries.is_empty());
        assert_eq!(
            store
                .project_authenticated_remote_terminal(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    "request-1",
                    "task-1",
                    &fingerprint,
                    terminal.clone(),
                )
                .unwrap(),
            ProjectionWrite::Inserted
        );
        assert_eq!(
            store
                .project_authenticated_remote_terminal(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    "request-1",
                    "task-1",
                    &fingerprint,
                    terminal,
                )
                .unwrap(),
            ProjectionWrite::Unchanged
        );
        let record = store.remote_proxy_snapshot(&local).unwrap();
        assert!(record.pending.is_empty());
        assert_eq!(record.retired_requests.len(), 1);
        assert_eq!(record.projection.entries.len(), 1);
    }

    #[test]
    fn authenticated_projection_rolls_at_513_without_losing_cursor() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let (local, remote_ref) = opened(&store, &live);
        for sequence in 1..=513 {
            store
                .project_authenticated_remote_entry(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    entry(sequence, "short"),
                )
                .unwrap();
        }
        let record = store.remote_proxy_snapshot(&local).unwrap();
        assert_eq!(record.projection.entries.len(), 512);
        assert_eq!(record.projection.entries.first().unwrap().sequence, 2);
        assert_eq!(record.projection.entries.last().unwrap().sequence, 513);
        assert_eq!(record.authenticated_cursor.unwrap().sequence, 513);
    }

    #[test]
    fn snapshot_terminal_retirement_and_cursor_are_one_persist() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let (local, remote_ref) = opened(&store, &live);
        let fingerprint = "b".repeat(64);
        store
            .insert_remote_pending(
                &local,
                &remote_ref,
                &live,
                PendingRemoteRequest {
                    client_request_id: "request-2".into(),
                    task_id: "task-1".into(),
                    request_fingerprint: fingerprint.clone(),
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                    reservation_fingerprint: None,
                },
            )
            .unwrap();
        store
            .mark_remote_dispatch_attempt(
                &local,
                &remote_ref,
                &live,
                "request-2",
                "task-1",
                &fingerprint,
            )
            .unwrap();
        let terminal = ProjectionEntry {
            kind: ProjectionKind::Status,
            body: Some("succeeded".into()),
            ..entry(1, "ignored")
        };
        let snapshot = SnapshotCandidate {
            projection: AuthenticatedProjection {
                schema_version: 1,
                entries: vec![terminal],
            },
            cursor: AuthenticatedCursor {
                journal_epoch: "journal-1".into(),
                sequence: 1,
            },
        };
        let path = temp.path().join("sessions/_remote_proxies.json");
        let before = std::fs::read(&path).unwrap();
        let original_path = {
            let mut state = store.remote_proxies.lock();
            std::mem::replace(
                &mut state.path,
                temp.path().join("missing-parent/remote.json"),
            )
        };
        assert!(
            store
                .project_authenticated_remote_snapshot(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    snapshot.clone(),
                    &AcceptSnapshot,
                )
                .is_err()
        );
        store.remote_proxies.lock().path = original_path;
        assert_eq!(std::fs::read(&path).unwrap(), before);
        let record = store.remote_proxy_snapshot(&local).unwrap();
        assert_eq!(record.pending.len(), 1);
        assert!(record.authenticated_cursor.is_none());
        assert_eq!(
            store
                .project_authenticated_remote_snapshot(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    snapshot,
                    &AcceptSnapshot,
                )
                .unwrap(),
            ProjectionWrite::Inserted
        );
        let record = store.remote_proxy_snapshot(&local).unwrap();
        assert!(record.pending.is_empty());
        assert_eq!(record.retired_requests.len(), 1);
        assert_eq!(record.authenticated_cursor.unwrap().sequence, 1);
    }

    #[test]
    fn snapshot_unknown_keeps_pending_and_b_local_terminal_is_display_only() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let (local, remote_ref) = opened(&store, &live);
        let fingerprint = "c".repeat(64);
        store
            .insert_remote_pending(
                &local,
                &remote_ref,
                &live,
                PendingRemoteRequest {
                    client_request_id: "request-3".into(),
                    task_id: "task-1".into(),
                    request_fingerprint: fingerprint.clone(),
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                    reservation_fingerprint: None,
                },
            )
            .unwrap();
        store
            .mark_remote_dispatch_attempt(
                &local,
                &remote_ref,
                &live,
                "request-3",
                "task-1",
                &fingerprint,
            )
            .unwrap();
        let unknown = ProjectionEntry {
            kind: ProjectionKind::Status,
            body: Some("result_unknown".into()),
            ..entry(1, "ignored")
        };
        let b_local = ProjectionEntry {
            kind: ProjectionKind::Status,
            task_id: Some("b-task-1".into()),
            body: Some("succeeded".into()),
            ..entry(2, "ignored")
        };
        store
            .project_authenticated_remote_snapshot(
                &local,
                &remote_ref,
                &live,
                "b-session-1",
                SnapshotCandidate {
                    projection: AuthenticatedProjection {
                        schema_version: 1,
                        entries: vec![unknown, b_local],
                    },
                    cursor: AuthenticatedCursor {
                        journal_epoch: "journal-1".into(),
                        sequence: 2,
                    },
                },
                &AcceptSnapshot,
            )
            .unwrap();
        let record = store.remote_proxy_snapshot(&local).unwrap();
        assert_eq!(record.pending.len(), 1);
        assert!(record.retired_requests.is_empty());
        assert_eq!(record.authenticated_cursor.unwrap().sequence, 2);
    }

    #[test]
    fn snapshot_unattempted_terminal_fails_without_advancing_cursor() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let (local, remote_ref) = opened(&store, &live);
        store
            .insert_remote_pending(
                &local,
                &remote_ref,
                &live,
                PendingRemoteRequest {
                    client_request_id: "request-4".into(),
                    task_id: "task-1".into(),
                    request_fingerprint: "d".repeat(64),
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                    reservation_fingerprint: None,
                },
            )
            .unwrap();
        let terminal = ProjectionEntry {
            kind: ProjectionKind::Status,
            body: Some("failed".into()),
            ..entry(1, "ignored")
        };
        assert!(
            store
                .project_authenticated_remote_snapshot(
                    &local,
                    &remote_ref,
                    &live,
                    "b-session-1",
                    SnapshotCandidate {
                        projection: AuthenticatedProjection {
                            schema_version: 1,
                            entries: vec![terminal]
                        },
                        cursor: AuthenticatedCursor {
                            journal_epoch: "journal-1".into(),
                            sequence: 1
                        },
                    },
                    &AcceptSnapshot,
                )
                .is_err()
        );
        let record = store.remote_proxy_snapshot(&local).unwrap();
        assert!(record.authenticated_cursor.is_none());
        assert_eq!(record.pending.len(), 1);
    }
}
