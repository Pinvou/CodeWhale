//! Session-owned, host-independent journal synchronization decisions.
//!
//! This service does not authenticate transport frames or maintain another
//! cursor. A caller presents a trusted live scope, projects through the
//! injected remote-control port, then confirms the persisted SessionStore
//! cursor before acknowledging B. `Duplicate` still needs the projection
//! port's content-equality check before an ACK is issued.

use super::ports::{UseCaseError, UseCaseFuture};
use crate::features::sessions::SessionStore;
use crate::features::sessions::remote_proxy::binding::{
    LiveRemoteContext, require_live_mapping_locked,
};
use crate::features::sessions::remote_proxy::schema::AuthenticatedCursor;

pub(crate) trait SyncRemoteJournalUseCase: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn sync_remote_journal(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response>;
}

fn invalid() -> UseCaseError {
    UseCaseError::new("remote_journal_sync_invalid", false)
}

fn bound_error() -> UseCaseError {
    UseCaseError::new("remote_journal_sync_binding_mismatch", false)
}

fn valid_epoch(epoch: &str) -> bool {
    !epoch.is_empty()
        && epoch.len() <= 128
        && epoch
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b':'))
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct SyncKey {
    local_session_id: String,
    remote_session_ref: String,
    controlled_session_id: String,
    live: LiveRemoteContext,
}

impl SyncKey {
    pub(crate) fn new(
        local_session_id: String,
        remote_session_ref: String,
        controlled_session_id: String,
        live: LiveRemoteContext,
    ) -> Result<Self, UseCaseError> {
        if local_session_id.is_empty()
            || remote_session_ref.is_empty()
            || controlled_session_id.is_empty()
        {
            return Err(invalid());
        }
        Ok(Self {
            local_session_id,
            remote_session_ref,
            controlled_session_id,
            live,
        })
    }

    pub(crate) fn local_session_id(&self) -> &str {
        &self.local_session_id
    }

    pub(crate) fn remote_session_ref(&self) -> &str {
        &self.remote_session_ref
    }

    pub(crate) fn controlled_session_id(&self) -> &str {
        &self.controlled_session_id
    }

    pub(crate) fn live(&self) -> &LiveRemoteContext {
        &self.live
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct JournalCursor {
    epoch: String,
    sequence: u64,
}

impl JournalCursor {
    pub(crate) fn new(epoch: String, sequence: u64) -> Result<Self, UseCaseError> {
        if !valid_epoch(&epoch) {
            return Err(invalid());
        }
        Ok(Self { epoch, sequence })
    }

    pub(crate) fn epoch(&self) -> &str {
        &self.epoch
    }

    pub(crate) fn sequence(&self) -> u64 {
        self.sequence
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct IncomingMeta {
    cursor: JournalCursor,
}

impl IncomingMeta {
    pub(crate) fn new(epoch: String, sequence: u64) -> Result<Self, UseCaseError> {
        Ok(Self {
            cursor: JournalCursor::new(epoch, sequence)?,
        })
    }

    pub(crate) fn cursor(&self) -> &JournalCursor {
        &self.cursor
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum SyncDecision {
    Apply,
    Duplicate,
    Replay {
        after: JournalCursor,
    },
    SnapshotRequired,
    /// The epoch is durably retired, or a snapshot is behind the current base.
    Stale,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SyncKind {
    Event,
    Snapshot,
}

pub(crate) struct SyncRequest {
    pub(crate) key: SyncKey,
    pub(crate) incoming: IncomingMeta,
    pub(crate) kind: SyncKind,
}

/// This receipt is only created from the durable SessionStore cursor after
/// the projection port returned success. It is not a transport proof and the
/// ACK sender must still check that its live lease is current.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ConfirmedJournalReceipt {
    scope: SyncKey,
    target: JournalCursor,
    persisted: JournalCursor,
}

impl ConfirmedJournalReceipt {
    pub(crate) fn scope(&self) -> &SyncKey {
        &self.scope
    }

    pub(crate) fn target(&self) -> &JournalCursor {
        &self.target
    }

    pub(crate) fn persisted(&self) -> &JournalCursor {
        &self.persisted
    }
}

pub(crate) struct RemoteJournalSyncService {
    store: SessionStore,
}

struct BoundRemoteJournalState {
    cursor: Option<AuthenticatedCursor>,
    retired_epochs: Vec<String>,
}

impl SessionStore {
    /// Binding and durable cursor are observed under the same SessionStore
    /// mutex. The returned values are only a read-only plan, never authority
    /// for a later write: the projection writer checks the binding again.
    fn read_bound_remote_journal_state(
        &self,
        key: &SyncKey,
    ) -> Result<BoundRemoteJournalState, UseCaseError> {
        let state = self.remote_proxies.lock();
        require_live_mapping_locked(
            &state,
            &key.local_session_id,
            &key.remote_session_ref,
            &key.live,
            &key.controlled_session_id,
        )
        .map_err(|_| bound_error())?;
        let record = state.records.get(&key.local_session_id).expect("checked");
        Ok(BoundRemoteJournalState {
            cursor: record.authenticated_cursor.clone(),
            retired_epochs: record.retired_epochs.clone(),
        })
    }
}

impl RemoteJournalSyncService {
    pub(crate) fn new(store: SessionStore) -> Self {
        Self { store }
    }

    fn read_bound(&self, key: &SyncKey) -> Result<BoundRemoteJournalState, UseCaseError> {
        self.store.read_bound_remote_journal_state(key)
    }

    fn current_cursor(state: &BoundRemoteJournalState) -> Option<JournalCursor> {
        state.cursor.as_ref().map(|cursor| JournalCursor {
            epoch: cursor.journal_epoch.clone(),
            sequence: cursor.sequence,
        })
    }

    /// Plan a single event. The first event is sequence 1; any missing prefix
    /// requests replay from sequence 0 of that incoming epoch.
    pub(crate) fn plan_event(
        &self,
        key: &SyncKey,
        incoming: &IncomingMeta,
    ) -> Result<SyncDecision, UseCaseError> {
        if incoming.cursor.sequence == 0 {
            return Err(invalid());
        }
        let state = self.read_bound(key)?;
        if state.retired_epochs.contains(&incoming.cursor.epoch) {
            return Ok(SyncDecision::Stale);
        }
        Ok(match Self::current_cursor(&state) {
            None if incoming.cursor.sequence == 1 => SyncDecision::Apply,
            None => SyncDecision::Replay {
                after: JournalCursor {
                    epoch: incoming.cursor.epoch.clone(),
                    sequence: 0,
                },
            },
            Some(current) if current.epoch != incoming.cursor.epoch => {
                SyncDecision::SnapshotRequired
            }
            Some(current) if incoming.cursor.sequence <= current.sequence => {
                SyncDecision::Duplicate
            }
            Some(current) if current.sequence.checked_add(1) == Some(incoming.cursor.sequence) => {
                SyncDecision::Apply
            }
            Some(current) => SyncDecision::Replay { after: current },
        })
    }

    /// A different, non-retired epoch can enter only through an authenticated
    /// snapshot. The projection writer enforces its own content and cursor
    /// invariants again under the write lock.
    pub(crate) fn plan_snapshot(
        &self,
        key: &SyncKey,
        incoming: &IncomingMeta,
    ) -> Result<SyncDecision, UseCaseError> {
        let state = self.read_bound(key)?;
        if state.retired_epochs.contains(&incoming.cursor.epoch) {
            return Ok(SyncDecision::Stale);
        }
        Ok(match Self::current_cursor(&state) {
            Some(current) if current.epoch == incoming.cursor.epoch => {
                match incoming.cursor.sequence.cmp(&current.sequence) {
                    std::cmp::Ordering::Less => SyncDecision::Stale,
                    std::cmp::Ordering::Equal => SyncDecision::Duplicate,
                    std::cmp::Ordering::Greater => SyncDecision::Apply,
                }
            }
            _ => SyncDecision::Apply,
        })
    }

    /// Re-read the only durable cursor with exact live A/B binding validation.
    pub(crate) fn confirmed_cursor(
        &self,
        key: &SyncKey,
    ) -> Result<Option<JournalCursor>, UseCaseError> {
        Ok(Self::current_cursor(&self.read_bound(key)?))
    }

    /// No ACK may be formed until the port has returned success and this
    /// read verifies the target was actually persisted in the current epoch.
    pub(crate) fn confirm_target(
        &self,
        key: &SyncKey,
        target: &JournalCursor,
    ) -> Result<ConfirmedJournalReceipt, UseCaseError> {
        let persisted = self
            .confirmed_cursor(key)?
            .filter(|cursor| cursor.epoch == target.epoch && cursor.sequence >= target.sequence)
            .ok_or_else(|| UseCaseError::new("remote_journal_sync_unconfirmed", true))?;
        Ok(ConfirmedJournalReceipt {
            scope: key.clone(),
            target: target.clone(),
            persisted,
        })
    }
}

impl SyncRemoteJournalUseCase for RemoteJournalSyncService {
    type Request = SyncRequest;
    type Response = SyncDecision;

    fn sync_remote_journal(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response> {
        Box::pin(async move {
            match request.kind {
                SyncKind::Event => self.plan_event(&request.key, &request.incoming),
                SyncKind::Snapshot => self.plan_snapshot(&request.key, &request.incoming),
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::sessions::remote_proxy::binding::AuthenticatedMapping;
    use crate::features::sessions::remote_proxy::schema::{
        AuthenticatedProjection, ProjectionEntry, ProjectionKind, SnapshotCandidate,
        SnapshotVerificationPort,
    };

    fn live() -> LiveRemoteContext {
        LiveRemoteContext {
            peer_id: "peer-1".into(),
            resume_boundary_id: "resume-1".into(),
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "lease-epoch-1".into(),
            relay_epoch: "relay-1".into(),
            generation: "generation-1".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        }
    }

    fn opened(store: &SessionStore) -> SyncKey {
        let live = live();
        let open = store.open_remote_proxy(None, &live).unwrap();
        store
            .confirm_remote_mapping(
                &open.local_session_id,
                &live,
                &AuthenticatedMapping {
                    remote_session_ref: open.remote_session_ref.clone(),
                    controlled_session_id: "b-session-1".into(),
                },
            )
            .unwrap();
        SyncKey::new(
            open.local_session_id,
            open.remote_session_ref,
            "b-session-1".into(),
            live,
        )
        .unwrap()
    }

    fn event(epoch: &str, sequence: u64) -> IncomingMeta {
        IncomingMeta::new(epoch.into(), sequence).unwrap()
    }

    fn persisted_entry(epoch: &str, sequence: u64) -> ProjectionEntry {
        ProjectionEntry {
            journal_epoch: epoch.into(),
            sequence,
            event_id: format!("event-{epoch}-{sequence}"),
            kind: ProjectionKind::AssistantMessage,
            task_id: Some("task-1".into()),
            body: Some("answer".into()),
            attachments: Vec::new(),
        }
    }

    fn project(store: &SessionStore, key: &SyncKey, epoch: &str, sequence: u64) {
        store
            .project_authenticated_remote_entry(
                key.local_session_id(),
                key.remote_session_ref(),
                key.live(),
                key.controlled_session_id(),
                persisted_entry(epoch, sequence),
            )
            .unwrap();
    }

    struct AcceptSnapshot;
    impl SnapshotVerificationPort for AcceptSnapshot {
        fn verify_authenticated_snapshot(
            &self,
            _: &str,
            _: &str,
            _: &LiveRemoteContext,
            _: &SnapshotCandidate,
        ) -> anyhow::Result<()> {
            Ok(())
        }
    }

    #[test]
    fn first_event_must_be_one_and_missing_prefix_replays_from_zero() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let key = opened(&store);
        let sync = RemoteJournalSyncService::new(store);
        assert_eq!(
            sync.plan_event(&key, &event("journal-1", 1)).unwrap(),
            SyncDecision::Apply
        );
        assert_eq!(
            sync.plan_event(&key, &event("journal-1", 3)).unwrap(),
            SyncDecision::Replay {
                after: JournalCursor::new("journal-1".into(), 0).unwrap()
            }
        );
        assert_eq!(
            sync.plan_event(&key, &event("journal-1", 0))
                .unwrap_err()
                .code(),
            "remote_journal_sync_invalid"
        );
        assert!(IncomingMeta::new("".into(), 1).is_err());
    }

    #[tokio::test]
    async fn duplicate_next_and_gap_use_the_persisted_cursor() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let key = opened(&store);
        let sync = RemoteJournalSyncService::new(store.clone());
        project(&store, &key, "journal-1", 1);
        assert_eq!(
            sync.plan_event(&key, &event("journal-1", 1)).unwrap(),
            SyncDecision::Duplicate
        );
        assert_eq!(
            sync.plan_event(&key, &event("journal-1", 2)).unwrap(),
            SyncDecision::Apply
        );
        assert_eq!(
            sync.sync_remote_journal(SyncRequest {
                key: key.clone(),
                incoming: event("journal-1", 4),
                kind: SyncKind::Event,
            })
            .await
            .unwrap(),
            SyncDecision::Replay {
                after: JournalCursor::new("journal-1".into(), 1).unwrap()
            }
        );
        assert_eq!(
            sync.confirmed_cursor(&key).unwrap(),
            Some(JournalCursor::new("journal-1".into(), 1).unwrap())
        );
    }

    #[test]
    fn epoch_change_requires_snapshot_and_retired_epoch_is_stale() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let key = opened(&store);
        let sync = RemoteJournalSyncService::new(store.clone());
        project(&store, &key, "journal-1", 1);
        assert_eq!(
            sync.plan_event(&key, &event("journal-2", 1)).unwrap(),
            SyncDecision::SnapshotRequired
        );
        assert_eq!(
            sync.plan_snapshot(&key, &event("journal-2", 0)).unwrap(),
            SyncDecision::Apply
        );
        store
            .project_authenticated_remote_snapshot(
                key.local_session_id(),
                key.remote_session_ref(),
                key.live(),
                key.controlled_session_id(),
                SnapshotCandidate {
                    projection: AuthenticatedProjection::default(),
                    cursor: AuthenticatedCursor {
                        journal_epoch: "journal-2".into(),
                        sequence: 0,
                    },
                },
                &AcceptSnapshot,
            )
            .unwrap();
        assert_eq!(
            sync.plan_event(&key, &event("journal-1", 2)).unwrap(),
            SyncDecision::Stale
        );
        assert_eq!(
            sync.plan_snapshot(&key, &event("journal-1", 1)).unwrap(),
            SyncDecision::Stale
        );
        assert_eq!(
            sync.plan_event(&key, &event("journal-2", 1)).unwrap(),
            SyncDecision::Apply
        );
        assert_eq!(
            sync.plan_snapshot(&key, &event("journal-2", 0)).unwrap(),
            SyncDecision::Duplicate
        );
    }

    #[test]
    fn wrong_binding_and_disconnect_cannot_read_or_confirm() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let key = opened(&store);
        let sync = RemoteJournalSyncService::new(store.clone());
        for wrong in [
            SyncKey::new(
                key.local_session_id().into(),
                "wrong-ref".into(),
                key.controlled_session_id().into(),
                key.live().clone(),
            )
            .unwrap(),
            SyncKey::new(
                key.local_session_id().into(),
                key.remote_session_ref().into(),
                "other-b".into(),
                key.live().clone(),
            )
            .unwrap(),
        ] {
            assert_eq!(
                sync.plan_event(&wrong, &event("journal-1", 1))
                    .unwrap_err()
                    .code(),
                "remote_journal_sync_binding_mismatch"
            );
            assert!(sync.confirmed_cursor(&wrong).is_err());
        }
        let mut old_lease = key.live().clone();
        old_lease.generation = "old-generation".into();
        let wrong_lease = SyncKey::new(
            key.local_session_id().into(),
            key.remote_session_ref().into(),
            key.controlled_session_id().into(),
            old_lease,
        )
        .unwrap();
        assert!(
            sync.plan_snapshot(&wrong_lease, &event("journal-1", 0))
                .is_err()
        );
        store.disconnect_remote_proxy(key.local_session_id());
        assert!(sync.plan_event(&key, &event("journal-1", 1)).is_err());
        assert!(
            sync.confirm_target(&key, &JournalCursor::new("journal-1".into(), 1).unwrap())
                .is_err()
        );
    }

    #[test]
    fn only_a_durable_projection_can_produce_a_confirmation_receipt() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let key = opened(&store);
        let sync = RemoteJournalSyncService::new(store.clone());
        let first = JournalCursor::new("journal-1".into(), 1).unwrap();
        assert_eq!(
            sync.confirm_target(&key, &first).unwrap_err().code(),
            "remote_journal_sync_unconfirmed"
        );
        project(&store, &key, "journal-1", 1);
        let receipt = sync.confirm_target(&key, &first).unwrap();
        assert_eq!(receipt.scope(), &key);
        assert_eq!(receipt.target(), &first);
        assert_eq!(receipt.persisted(), &first);
        let second = JournalCursor::new("journal-1".into(), 2).unwrap();
        assert!(sync.confirm_target(&key, &second).is_err());
        project(&store, &key, "journal-1", 2);
        assert_eq!(
            sync.confirm_target(&key, &first).unwrap().persisted(),
            &second
        );
        store.disconnect_remote_proxy(key.local_session_id());
        assert_eq!(
            sync.confirm_target(&key, &second).unwrap_err().code(),
            "remote_journal_sync_binding_mismatch"
        );
    }
}
