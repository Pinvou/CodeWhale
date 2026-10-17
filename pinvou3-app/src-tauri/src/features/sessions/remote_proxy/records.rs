//! SessionStore registry operations over validated RemoteProxy records.
use super::binding::{LiveRemoteContext, require_live_locked};
use super::persistence::persist_changed;
use super::schema::*;
use crate::features::sessions::SessionStore;
use anyhow::{Result, bail};

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
        let archived = state.archives.read(id).ok().flatten();
        state
            .records
            .get(id)
            .or(archived.as_ref())
            .map(|record| RemoteOfflineView {
                local_session_id: record.local_session_id.clone(),
                ui: record.ui.clone(),
                projection: record.projection.clone(),
                authenticated_cursor: record.authenticated_cursor.clone(),
                pending_unknown_count: record
                    .pending
                    .iter()
                    .filter(|item| {
                        item.dispatch_state == PendingDispatchState::AttemptMayHaveOccurred
                    })
                    .count(),
                unattempted_pending: record
                    .pending
                    .iter()
                    .filter(|item| {
                        item.dispatch_state == PendingDispatchState::PreparedNeverAttempted
                    })
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
        if record.pending.len() >= MAX_PENDING_REQUESTS
            || record.retired_requests.len() + record.pending.len() >= MAX_RETIRED_REQUESTS
        {
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
