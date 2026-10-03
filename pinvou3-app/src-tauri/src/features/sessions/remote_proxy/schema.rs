use anyhow::{Result, bail};
use serde::{Deserialize, Serialize};
use std::collections::HashSet;

use super::{
    binding::{LiveRemoteContext, require_live_locked},
    persistence::persist_changed,
};
use crate::features::sessions::SessionStore;
use crate::features::sessions::validators::validate_session_id;

pub(super) const REMOTE_PROXY_SCHEMA_VERSION: u32 = 3;
pub(super) const MAX_PENDING_REQUESTS: usize = 64;
pub(super) const MAX_RETIRED_REQUESTS: usize = 4096;
pub(crate) const MAX_PROJECTION_ENTRIES: usize = 512;
pub(super) const MAX_PROJECTION_BYTES: usize = 1024 * 1024;
pub(super) const MAX_PROJECTION_BODY_BYTES: usize = 16 * 1024;
pub(super) const MAX_PROJECTION_ATTACHMENTS: usize = 16;
pub(super) const MAX_REMOTE_PROXY_FILE_BYTES: usize = 16 * 1024 * 1024;
pub(super) const MAX_REMOTE_PROXY_RECORDS: usize = 256;
pub(super) const MAX_RETIRED_EPOCHS: usize = 64;
// Matches controller/task.rs::validate_identifier, not protocol.rs's stricter
// first-character grammar. Keep this local: sessions must not depend on the
// remote_control feature. The app-layer contract test compares both sides.
const MAX_REMOTE_ID_CHARS: usize = 128;

fn projection_schema_version() -> u32 {
    1
}

/// A-owned UI state and an authenticated, read-only cache of B history.
/// `remote_session_ref` and `controlled_session_id` are historical join keys,
/// never credentials. Live authority exists only in the in-process binding.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RemoteProxyRecord {
    pub(crate) schema_version: u32,
    pub(crate) local_session_id: String,
    pub(crate) peer_id: String,
    pub(crate) remote_session_ref: String,
    pub(crate) controlled_session_id: Option<String>,
    pub(crate) ui: RemoteUiMetadata,
    pub(crate) authenticated_cursor: Option<AuthenticatedCursor>,
    /// Never evicted. Once the bounded list is full, a new local Session is
    /// required rather than forgetting an epoch and permitting rollback.
    pub(crate) retired_epochs: Vec<String>,
    /// Versioned, bounded cache of B-authenticated content. Never B authority.
    pub(crate) projection: AuthenticatedProjection,
    pub(crate) pending: Vec<PendingRemoteRequest>,
    /// Permanent across this SessionStore. Capacity exhaustion fails closed;
    /// no identifier may be recycled by evicting an older tombstone.
    pub(crate) retired_requests: Vec<RetiredRemoteRequest>,
}

impl RemoteProxyRecord {
    /// The same durable invariant is checked before every write and after
    /// strictly decoding the current v3 record.
    pub(super) fn validate_stored(&self) -> Result<()> {
        validate_session_id(&self.local_session_id)?;
        if !self.local_session_id.starts_with("remote-")
            || self.schema_version != REMOTE_PROXY_SCHEMA_VERSION
            || self.peer_id.is_empty()
            || self.remote_session_ref.is_empty()
        {
            bail!("invalid RemoteProxy record");
        }
        self.ui.validate()?;
        self.projection.validate()?;
        validate_request_ids(&self.pending, &self.retired_requests)?;
        if self.retired_epochs.len() > MAX_RETIRED_EPOCHS {
            bail!("retired RemoteProxy journal epoch capacity exceeded");
        }
        let mut retired = HashSet::new();
        if self
            .retired_epochs
            .iter()
            .any(|epoch| epoch.is_empty() || epoch.len() > 128 || !retired.insert(epoch))
        {
            bail!("invalid retired RemoteProxy journal epoch");
        }
        if let Some(cursor) = &self.authenticated_cursor {
            if retired.contains(&cursor.journal_epoch) {
                bail!("current RemoteProxy epoch is retired");
            }
            validate_snapshot_window(&self.projection, cursor)?;
        } else if !self.projection.entries.is_empty() {
            bail!("RemoteProxy projection has no authenticated cursor");
        } else if !self.retired_epochs.is_empty() {
            bail!("retired RemoteProxy epochs require a current cursor");
        }
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct AuthenticatedProjection {
    pub(crate) schema_version: u32,
    pub(crate) entries: Vec<ProjectionEntry>,
}

impl Default for AuthenticatedProjection {
    fn default() -> Self {
        Self {
            schema_version: projection_schema_version(),
            entries: Vec::new(),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct ProjectionEntry {
    /// Stable B journal key. Replays with the same key must be byte-for-byte equal.
    pub(crate) journal_epoch: String,
    pub(crate) sequence: u64,
    pub(crate) event_id: String,
    pub(crate) kind: ProjectionKind,
    pub(crate) task_id: Option<String>,
    /// Display text only. DEV-30 must redact transport credentials and model
    /// secrets before invoking the authenticated projection writer; this
    /// schema deliberately has no credential/header/key-material fields.
    /// User-authored text is not keyword-filtered: discussing an API key is
    /// legitimate history and cannot itself prove a secret was persisted.
    pub(crate) body: Option<String>,
    pub(crate) attachments: Vec<ProjectionAttachment>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum ProjectionKind {
    UserMessage,
    AssistantMessage,
    Reasoning,
    Tool,
    Approval,
    Status,
    Error,
    Attachment,
    Artifact,
    SnapshotMarker,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct ProjectionAttachment {
    pub(crate) id: String,
    pub(crate) display_name: String,
    pub(crate) content_type: String,
    pub(crate) byte_length: u64,
    pub(crate) checksum_sha256: String,
}

impl ProjectionEntry {
    pub(super) fn validate(&self) -> Result<()> {
        if self.journal_epoch.is_empty()
            || self.journal_epoch.len() > 128
            || self.event_id.is_empty()
            || self.event_id.len() > 128
            || self
                .task_id
                .as_ref()
                .is_some_and(|value| value.is_empty() || value.len() > 128)
            || self
                .body
                .as_ref()
                .is_some_and(|value| value.len() > MAX_PROJECTION_BODY_BYTES)
            || self.attachments.len() > MAX_PROJECTION_ATTACHMENTS
        {
            bail!("invalid remote projection entry");
        }
        for attachment in &self.attachments {
            if attachment.id.is_empty()
                || attachment.id.len() > 128
                || attachment.display_name.is_empty()
                || attachment.display_name.len() > 256
                || attachment.content_type.is_empty()
                || attachment.content_type.len() > 128
                || attachment.checksum_sha256.len() != 64
                || !attachment
                    .checksum_sha256
                    .bytes()
                    .all(|byte| byte.is_ascii_hexdigit())
            {
                bail!("invalid remote projection attachment index");
            }
        }
        Ok(())
    }
}

impl AuthenticatedProjection {
    pub(super) fn validate(&self) -> Result<()> {
        if self.schema_version != projection_schema_version()
            || self.entries.len() > MAX_PROJECTION_ENTRIES
            || serde_json::to_vec(self)?.len() > MAX_PROJECTION_BYTES
        {
            bail!("unsupported or oversized remote projection");
        }
        let mut keys = std::collections::HashSet::new();
        for entry in &self.entries {
            entry.validate()?;
            if !keys.insert((&entry.journal_epoch, entry.sequence)) {
                bail!("duplicate remote projection key");
            }
        }
        Ok(())
    }
}

impl RemoteUiMetadata {
    pub(super) fn validate(&self) -> Result<()> {
        if self.title.chars().count() > 120
            || self.last_opened_at.as_ref().is_some_and(|value| {
                value.len() > 64 || chrono::DateTime::parse_from_rfc3339(value).is_err()
            })
        {
            bail!("invalid RemoteProxy UI metadata");
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RemoteUiMetadata {
    pub(crate) title: String,
    pub(crate) pinned: bool,
    pub(crate) hidden: bool,
    pub(crate) last_opened_at: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct RemoteProxyListItem {
    pub(crate) local_session_id: String,
    pub(crate) ui: RemoteUiMetadata,
    pub(crate) projected_entry_count: usize,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct RemoteOfflineView {
    pub(crate) local_session_id: String,
    pub(crate) ui: RemoteUiMetadata,
    pub(crate) projection: AuthenticatedProjection,
    pub(crate) authenticated_cursor: Option<AuthenticatedCursor>,
    pub(crate) pending_unknown_count: usize,
    /// Only never-attempted handles are discoverable after restart so the UI
    /// can offer explicit abandon. Attempted requests remain outcome-unknown.
    pub(crate) unattempted_pending: Vec<PendingRemoteRequest>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct AuthenticatedCursor {
    pub(crate) journal_epoch: String,
    pub(crate) sequence: u64,
}

/// Candidate snapshot and the verifier contract used by DEV-30's encrypted
/// channel adapter. A bool from a caller is not an authentication proof.
#[derive(Clone, Debug)]
pub(crate) struct SnapshotCandidate {
    pub(crate) projection: AuthenticatedProjection,
    pub(crate) cursor: AuthenticatedCursor,
}

pub(crate) trait SnapshotVerificationPort {
    fn verify_authenticated_snapshot(
        &self,
        local_session_id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        candidate: &SnapshotCandidate,
    ) -> Result<()>;
}

pub(super) fn validate_snapshot_window(
    projection: &AuthenticatedProjection,
    cursor: &AuthenticatedCursor,
) -> Result<()> {
    projection.validate()?;
    if cursor.journal_epoch.is_empty() || cursor.journal_epoch.len() > 128 {
        bail!("invalid authenticated snapshot epoch");
    }
    let mut previous = None;
    for entry in &projection.entries {
        if entry.sequence == 0
            || entry.journal_epoch != cursor.journal_epoch
            || previous.is_some_and(|sequence: u64| sequence.checked_add(1) != Some(entry.sequence))
        {
            bail!("snapshot projection is not contiguous in its authenticated epoch");
        }
        previous = Some(entry.sequence);
    }
    match previous {
        Some(last) if last == cursor.sequence => Ok(()),
        None if cursor.sequence == 0 => Ok(()),
        _ => bail!("snapshot projection does not end at its authenticated cursor"),
    }
}

/// Non-authoritative correlation only. No prompt, token, key, or wire payload.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PendingRemoteRequest {
    pub(crate) client_request_id: String,
    pub(crate) task_id: String,
    /// Lowercase SHA-256 of DEV-29's canonical, non-secret request semantics.
    /// This is not a digest of a randomized temporary model package.
    pub(crate) request_fingerprint: String,
    /// Durable pre-send barrier. Never move back to `NeverAttempted` after
    /// persistence, including when the transport reports an error.
    pub(crate) dispatch_state: PendingDispatchState,
    /// Original reserve handle, retained only for read-only outcome queries
    /// after authenticated attachment receipts finalized the fingerprint.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub(crate) reservation_fingerprint: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum PendingDispatchState {
    PreparedNeverAttempted,
    AttemptMayHaveOccurred,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RetiredRemoteRequest {
    pub(crate) client_request_id: String,
    pub(crate) task_id: String,
    pub(crate) request_fingerprint: String,
    pub(crate) disposition: RetiredRequestDisposition,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum RetiredRequestDisposition {
    AbandonedNeverAttempted,
    Terminal,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum PendingReservation {
    Inserted,
    Existing(PendingRemoteRequest),
}

pub(crate) fn valid_remote_identifier(text: &str) -> bool {
    !text.is_empty()
        && text.len() <= MAX_REMOTE_ID_CHARS
        && text
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"._:-".contains(&byte))
}

impl PendingRemoteRequest {
    fn validate(&self) -> Result<()> {
        if !valid_remote_identifier(&self.client_request_id)
            || !valid_remote_identifier(&self.task_id)
            || !valid_request_fingerprint(&self.request_fingerprint)
            || self
                .reservation_fingerprint
                .as_deref()
                .is_some_and(|value| !valid_request_fingerprint(value))
        {
            bail!("invalid remote pending correlation");
        }
        Ok(())
    }
}

pub(super) fn validate_request_ids(
    pending: &[PendingRemoteRequest],
    retired: &[RetiredRemoteRequest],
) -> Result<()> {
    if pending.len() > MAX_PENDING_REQUESTS || retired.len() > MAX_RETIRED_REQUESTS {
        bail!("remote pending capacity reached");
    }
    let mut client_ids = HashSet::new();
    let mut task_ids = HashSet::new();
    for item in pending {
        item.validate()?;
        if !client_ids.insert(&item.client_request_id) || !task_ids.insert(&item.task_id) {
            bail!("remote pending identifier conflict");
        }
    }
    for item in retired {
        if !valid_remote_identifier(&item.client_request_id)
            || !valid_remote_identifier(&item.task_id)
            || !valid_request_fingerprint(&item.request_fingerprint)
            || !client_ids.insert(&item.client_request_id)
            || !task_ids.insert(&item.task_id)
        {
            bail!("invalid or reused retired remote request identifier");
        }
    }
    Ok(())
}

fn valid_request_fingerprint(fingerprint: &str) -> bool {
    fingerprint.len() == 64
        && fingerprint
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct RemoteProxyFile {
    pub(super) schema_version: u32,
    pub(super) records: Vec<RemoteProxyRecord>,
}

impl SessionStore {
    /// Trusted, single-source list for DEV-31's history adapter. No historic
    /// remote reference or B mapping is exposed to the offline UI.
    pub(crate) fn list_remote_proxy_history(&self) -> Vec<RemoteProxyListItem> {
        let state = self.remote_proxies.lock();
        let mut list: Vec<_> = state
            .records
            .values()
            .map(|record| RemoteProxyListItem {
                local_session_id: record.local_session_id.clone(),
                ui: record.ui.clone(),
                projected_entry_count: record.projection.entries.len(),
            })
            .collect();
        list.sort_by(|left, right| {
            right
                .ui
                .pinned
                .cmp(&left.ui.pinned)
                .then_with(|| right.ui.last_opened_at.cmp(&left.ui.last_opened_at))
                .then_with(|| left.local_session_id.cmp(&right.local_session_id))
        });
        list
    }

    pub(crate) fn read_remote_proxy_history(&self, id: &str) -> Option<RemoteOfflineView> {
        let state = self.remote_proxies.lock();
        state.records.get(id).map(|record| RemoteOfflineView {
            local_session_id: record.local_session_id.clone(),
            ui: record.ui.clone(),
            projection: record.projection.clone(),
            authenticated_cursor: record.authenticated_cursor.clone(),
            pending_unknown_count: record
                .pending
                .iter()
                .filter(|item| item.dispatch_state == PendingDispatchState::AttemptMayHaveOccurred)
                .count(),
            unattempted_pending: record
                .pending
                .iter()
                .filter(|item| item.dispatch_state == PendingDispatchState::PreparedNeverAttempted)
                .cloned()
                .collect(),
        })
    }

    /// Correlation slot for DEV-29's prepare/persist/dispatch. No input body,
    /// temporary credential or claim of B acceptance is stored here.
    pub(crate) fn insert_remote_pending(
        &self,
        id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        pending: PendingRemoteRequest,
    ) -> Result<PendingReservation> {
        pending.validate()?;
        if pending.dispatch_state != PendingDispatchState::PreparedNeverAttempted {
            bail!("new remote pending request must be unattempted and known");
        }
        let mut state = self.remote_proxies.lock();
        require_live_locked(&state, id, remote_ref, context)?;
        let mut record = state
            .records
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?
            .clone();
        if let Some(existing) = record.pending.iter().find(|item| {
            item.client_request_id == pending.client_request_id || item.task_id == pending.task_id
        }) {
            // Returning the existing durable state must not cross the send
            // barrier or turn a possibly attempted request into a fresh one.
            if existing.client_request_id == pending.client_request_id
                && existing.task_id == pending.task_id
                && existing.request_fingerprint == pending.request_fingerprint
            {
                return Ok(PendingReservation::Existing(existing.clone()));
            }
            bail!("remote pending identifier conflict");
        }
        if record.retired_requests.iter().any(|item| {
            item.client_request_id == pending.client_request_id || item.task_id == pending.task_id
        }) {
            bail!("retired remote request identifier cannot be reused");
        }
        if record.pending.len() >= MAX_PENDING_REQUESTS {
            bail!("remote pending capacity reached");
        }
        record.pending.push(pending);
        validate_request_ids(&record.pending, &record.retired_requests)?;
        persist_changed(&mut state, id, record)?;
        Ok(PendingReservation::Inserted)
    }

    /// Finalize the fingerprint of a prepared request after authenticated B
    /// upload receipts have been bound to its in-process payload. The original
    /// IDs stay fixed; an attempted request can never change semantics.
    pub(crate) fn replace_prepared_remote_fingerprint(
        &self,
        id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        client_request_id: &str,
        task_id: &str,
        old_fingerprint: &str,
        new_fingerprint: &str,
    ) -> Result<()> {
        if !valid_request_fingerprint(old_fingerprint)
            || !valid_request_fingerprint(new_fingerprint)
        {
            bail!("invalid remote request fingerprint");
        }
        let mut state = self.remote_proxies.lock();
        require_live_locked(&state, id, remote_ref, context)?;
        let mut record = state
            .records
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?
            .clone();
        let pending = record
            .pending
            .iter_mut()
            .find(|item| item.client_request_id == client_request_id && item.task_id == task_id)
            .ok_or_else(|| anyhow::anyhow!("unknown remote pending request"))?;
        if pending.request_fingerprint != old_fingerprint
            || pending.dispatch_state != PendingDispatchState::PreparedNeverAttempted
        {
            bail!("remote request cannot change after its dispatch barrier");
        }
        if old_fingerprint == new_fingerprint {
            return Ok(());
        }
        if pending.reservation_fingerprint.is_none() {
            pending.reservation_fingerprint = Some(old_fingerprint.to_owned());
        }
        pending.request_fingerprint = new_fingerprint.to_owned();
        persist_changed(&mut state, id, record)
    }

    /// Persist the irreversible send barrier before any transport call. A
    /// failed persistence must stop dispatch; an ambiguous transport failure
    /// must never restore `NeverAttempted`.
    pub(crate) fn mark_remote_dispatch_attempt(
        &self,
        id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        client_request_id: &str,
        task_id: &str,
        fingerprint: &str,
    ) -> Result<()> {
        let mut state = self.remote_proxies.lock();
        require_live_locked(&state, id, remote_ref, context)?;
        let mut record = state
            .records
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?
            .clone();
        let pending = record
            .pending
            .iter_mut()
            .find(|item| item.client_request_id == client_request_id && item.task_id == task_id)
            .ok_or_else(|| anyhow::anyhow!("unknown remote pending request"))?;
        if pending.request_fingerprint != fingerprint
            || pending.dispatch_state != PendingDispatchState::PreparedNeverAttempted
        {
            bail!("remote request cannot cross the dispatch barrier");
        }
        pending.dispatch_state = PendingDispatchState::AttemptMayHaveOccurred;
        persist_changed(&mut state, id, record)
    }

    /// A cancel request is meaningful only for a task whose send barrier was
    /// durably crossed in this exact live RemoteProxy binding. Prepared tasks
    /// must be abandoned instead; an arbitrary UI task ID is never authority.
    pub(crate) fn require_attempted_remote_task(
        &self,
        id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
        task_id: &str,
    ) -> Result<()> {
        let state = self.remote_proxies.lock();
        require_live_locked(&state, id, remote_ref, context)?;
        let record = state
            .records
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?;
        if !record.pending.iter().any(|pending| {
            pending.task_id == task_id
                && pending.dispatch_state == PendingDispatchState::AttemptMayHaveOccurred
        }) {
            bail!("remote task is not currently attempted");
        }
        Ok(())
    }

    /// Only a persisted proof of `NeverAttempted` permits abandoning a lost
    /// ephemeral package. The two identifiers are retired atomically with
    /// pending removal, even when this process no longer has a live binding.
    pub(crate) fn abandon_unattempted_remote_pending(
        &self,
        id: &str,
        client_request_id: &str,
        task_id: &str,
        fingerprint: &str,
    ) -> Result<()> {
        let mut state = self.remote_proxies.lock();
        let mut record = state
            .records
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?
            .clone();
        let index = record
            .pending
            .iter()
            .position(|item| item.client_request_id == client_request_id && item.task_id == task_id)
            .ok_or_else(|| anyhow::anyhow!("unknown remote pending request"))?;
        let pending = &record.pending[index];
        if pending.request_fingerprint != fingerprint
            || pending.dispatch_state != PendingDispatchState::PreparedNeverAttempted
        {
            bail!("remote dispatch may have been attempted");
        }
        if record.retired_requests.len() >= MAX_RETIRED_REQUESTS {
            bail!("retired remote request capacity reached");
        }
        record.pending.remove(index);
        record.retired_requests.push(RetiredRemoteRequest {
            client_request_id: client_request_id.to_owned(),
            task_id: task_id.to_owned(),
            request_fingerprint: fingerprint.to_owned(),
            disposition: RetiredRequestDisposition::AbandonedNeverAttempted,
        });
        persist_changed(&mut state, id, record)
    }
}
