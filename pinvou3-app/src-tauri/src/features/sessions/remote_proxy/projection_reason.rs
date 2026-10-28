use super::schema::{
    MAX_RETIRED_EPOCHS, MAX_RETIRED_REQUESTS, PendingDispatchState, ProjectionEntry,
    ProjectionKind, RemoteProxyRecord, RetiredRemoteRequest, RetiredRequestDisposition,
    SnapshotCandidate, validate_snapshot_window,
};
use anyhow::{Result, bail};
use std::collections::HashSet;

// Upgrade a retained entry only with an authenticated closed-set reason for
// the identical event. A legacy replay may omit an already recorded reason;
// it must never erase it. No other event field may change.
pub(super) fn merge_entry_reason(
    old: &ProjectionEntry,
    incoming: &ProjectionEntry,
) -> Result<ProjectionEntry> {
    let mut old_without_reason = old.clone();
    let mut incoming_without_reason = incoming.clone();
    old_without_reason.terminal_reason = None;
    incoming_without_reason.terminal_reason = None;
    if old_without_reason != incoming_without_reason
        || matches!((&old.terminal_reason, &incoming.terminal_reason),
            (Some(left), Some(right)) if left != right)
    {
        bail!("remote journal event key or id was reused with different content");
    }
    let mut merged = incoming.clone();
    merged.terminal_reason = old
        .terminal_reason
        .clone()
        .or_else(|| incoming.terminal_reason.clone());
    Ok(merged)
}

pub(super) fn prepare_snapshot(
    record: &mut RemoteProxyRecord,
    candidate: &mut SnapshotCandidate,
) -> Result<(bool, bool)> {
    let same_cursor = record.authenticated_cursor.as_ref() == Some(&candidate.cursor);
    if let Some(previous) = &record.authenticated_cursor {
        if previous.journal_epoch == candidate.cursor.journal_epoch {
            if candidate.cursor.sequence < previous.sequence {
                bail!("remote snapshot cursor moved backwards");
            }
            // Authentication proves the sender, not immutability of the
            // retained history. A forward snapshot must obey the same
            // event-key/id rule as a single-entry write before retiring
            // pending requests or replacing any persisted state.
            for incoming in &mut candidate.projection.entries {
                if let Some(existing) = record.projection.entries.iter().find(|existing| {
                    (existing.journal_epoch == incoming.journal_epoch
                        && existing.sequence == incoming.sequence)
                        || existing.event_id == incoming.event_id
                }) {
                    *incoming = merge_entry_reason(existing, incoming)?;
                }
            }
            if candidate.cursor.sequence == previous.sequence {
                if candidate.projection == record.projection {
                    return Ok((true, same_cursor));
                }
                let existing = &record.projection.entries;
                let incoming = &candidate.projection.entries;
                // B's 256-KiB byte window may have trimmed more than A's
                // count/byte window at the same authenticated cursor.
                // Only a shorter, byte-identical suffix is a valid
                // replacement; there must be a common cursor-tail event.
                if incoming.is_empty()
                    || existing.is_empty()
                    || incoming.len() > existing.len()
                    || !existing[existing.len() - incoming.len()..]
                        .iter()
                        .zip(incoming)
                        .all(|(old, new)| {
                            merge_entry_reason(old, new).is_ok_and(|merged| merged == *new)
                        })
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
    validate_snapshot_window(&candidate.projection, &candidate.cursor)?;
    Ok((false, same_cursor))
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

pub(super) fn apply_snapshot(
    record: &mut RemoteProxyRecord,
    mut candidate: SnapshotCandidate,
) -> Result<bool> {
    let (unchanged, same_cursor) = prepare_snapshot(record, &mut candidate)?;
    if unchanged {
        return Ok(false);
    }
    if !same_cursor {
        retire_snapshot_terminals(record, &candidate.projection)?;
    }
    record.projection = candidate.projection;
    record.authenticated_cursor = Some(candidate.cursor);
    Ok(true)
}

#[cfg(test)]
mod tests {
    use super::super::schema::ProjectionKind;
    use super::*;

    fn failed() -> ProjectionEntry {
        ProjectionEntry {
            journal_epoch: "epoch".into(),
            sequence: 1,
            event_id: "event".into(),
            kind: ProjectionKind::Status,
            task_id: Some("task".into()),
            body: Some("failed".into()),
            terminal_reason: None,
            attachments: vec![],
        }
    }

    #[test]
    fn authenticated_reason_enriches_and_legacy_replay_never_erases() {
        let old = failed();
        for reason in ["task_generation_stale", "task_attachments_invalid"] {
            let mut incoming = old.clone();
            incoming.terminal_reason = Some(reason.into());
            incoming.validate().unwrap();
            let enriched = merge_entry_reason(&old, &incoming).unwrap();
            assert_eq!(enriched, incoming);
            assert_eq!(merge_entry_reason(&enriched, &old).unwrap(), enriched);
            assert_eq!(enriched.sequence, old.sequence);
            assert_eq!(enriched.event_id, old.event_id);
        }
    }

    #[test]
    fn conflicting_reasons_or_changed_event_are_rejected() {
        let mut old = failed();
        old.terminal_reason = Some("task_generation_stale".into());
        let mut conflict = old.clone();
        conflict.terminal_reason = Some("task_attachments_invalid".into());
        assert!(merge_entry_reason(&old, &conflict).is_err());
        conflict = old.clone();
        conflict.task_id = Some("other-task".into());
        assert!(merge_entry_reason(&old, &conflict).is_err());
        conflict = old.clone();
        conflict.event_id = "other-event".into();
        assert!(merge_entry_reason(&old, &conflict).is_err());
    }

    #[test]
    fn same_cursor_enriches_only_identical_history_and_preserves_pending() {
        let old = failed();
        let mut record: RemoteProxyRecord = serde_json::from_value(serde_json::json!({
            "schema_version":3,"local_session_id":"remote-test","peer_id":"peer",
            "remote_session_ref":"remote-ref","controlled_session_id":null,
            "ui":{"title":"test","pinned":false,"hidden":false,"last_opened_at":null},
            "authenticated_cursor":{"journal_epoch":"epoch","sequence":1},"retired_epochs":[],
            "projection":{"schema_version":1,"entries":[old]},
            "pending":[{"client_request_id":"new-request","task_id":"new-task","request_fingerprint":"a".repeat(64),"dispatch_state":"attempt_may_have_occurred"}],
            "retired_requests":[]
        })).unwrap();
        record.validate_stored().unwrap();
        let pending = record.pending.clone();
        let mut projection = record.projection.clone();
        projection.entries[0].terminal_reason = Some("task_generation_stale".into());
        let mut candidate = SnapshotCandidate {
            projection,
            cursor: record.authenticated_cursor.clone().unwrap(),
        };
        assert_eq!(
            prepare_snapshot(&mut record, &mut candidate).unwrap(),
            (false, true)
        );
        assert_eq!(record.pending, pending);
        assert_eq!(
            candidate.projection.entries[0].terminal_reason.as_deref(),
            Some("task_generation_stale")
        );
        record.projection = candidate.projection.clone();
        assert_eq!(
            prepare_snapshot(&mut record, &mut candidate).unwrap(),
            (true, true)
        );
        candidate.projection.entries[0].body = Some("changed".into());
        assert!(prepare_snapshot(&mut record, &mut candidate).is_err());
        assert_eq!(record.pending, pending);
    }

    #[test]
    fn same_cursor_does_not_retire_later_pending_but_forward_cursor_does() {
        let old = failed();
        let mut record: RemoteProxyRecord = serde_json::from_value(serde_json::json!({
            "schema_version":3,"local_session_id":"remote-test","peer_id":"peer",
            "remote_session_ref":"remote-ref","controlled_session_id":null,
            "ui":{"title":"test","pinned":false,"hidden":false,"last_opened_at":null},
            "authenticated_cursor":{"journal_epoch":"epoch","sequence":1},"retired_epochs":[],
            "projection":{"schema_version":1,"entries":[old]},
            "pending":[{"client_request_id":"new-request","task_id":"task","request_fingerprint":"a".repeat(64),"dispatch_state":"attempt_may_have_occurred"}],
            "retired_requests":[]
        })).unwrap();
        record.validate_stored().unwrap();
        let mut same = SnapshotCandidate {
            projection: record.projection.clone(),
            cursor: record.authenticated_cursor.clone().unwrap(),
        };
        same.projection.entries[0].terminal_reason = Some("task_generation_stale".into());
        assert!(apply_snapshot(&mut record, same).unwrap());
        assert_eq!(record.pending.len(), 1);
        assert!(record.retired_requests.is_empty());
        let mut forward = SnapshotCandidate {
            projection: record.projection.clone(),
            cursor: record.authenticated_cursor.clone().unwrap(),
        };
        let mut next = failed();
        next.sequence = 2;
        next.event_id = "next-event".into();
        forward.projection.entries = vec![next];
        forward.cursor.sequence = 2;
        assert!(apply_snapshot(&mut record, forward).unwrap());
        assert!(record.pending.is_empty());
        assert_eq!(record.retired_requests.len(), 1);
        assert_eq!(record.authenticated_cursor.unwrap().sequence, 2);
    }
}
