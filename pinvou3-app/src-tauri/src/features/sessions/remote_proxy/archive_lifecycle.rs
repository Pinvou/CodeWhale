//! Host-fenced archive lifecycle and active-registry cleanup.
use super::schema::RemoteProxyListItem;
use anyhow::{Result, bail};

impl super::super::SessionStore {
    /// Caller holds the controller lifecycle owner while supplying its current
    /// authenticated identity (including resumable/disconnected leases).
    pub(crate) fn archive_remote_proxy(
        &self,
        id: &str,
        live: Option<&super::binding::LiveRemoteContext>,
    ) -> Result<bool> {
        let mut state = self.remote_proxies.lock();
        state.require_available()?;
        if state.archives.contains(id)? {
            return Ok(state.persist().is_ok());
        }
        let record = state
            .records
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("remote_proxy_not_found"))?
            .clone();
        if live.is_some_and(|live| {
            state
                .openings
                .get(id)
                .is_some_and(|open| &open.context == live)
        }) {
            bail!("remote_proxy_archive_online");
        }
        if !record.pending.is_empty() {
            bail!("remote_proxy_archive_pending");
        }
        state
            .archives
            .require_active_records(state.records.values())?;
        // The archive anchor has its own bounded storage, so even a completely
        // full legacy active JSON can seal without first growing that JSON.
        if let Err(error) = state.archives.seal(&record) {
            // A durable intent may already exist. Never restore execution
            // authority after ambiguous I/O; startup recovers that exact seal.
            state.load_failed = true;
            return Err(error);
        }
        // The archive transaction is now permanent. A stale active JSON copy
        // cannot authorize a reopen, even if cleanup fails or the process exits.
        state.records.remove(id);
        state.openings.remove(id);
        let cleaned = state.persist().is_ok();
        Ok(cleaned)
    }

    pub(crate) fn list_remote_proxy_archives(
        &self,
        before: Option<i64>,
    ) -> Result<Vec<(i64, RemoteProxyListItem)>> {
        let state = self.remote_proxies.lock();
        state.require_available()?;
        state.archives.list(before)
    }

    pub(crate) fn is_archived_remote_proxy(&self, id: &str) -> Result<bool> {
        let state = self.remote_proxies.lock();
        state.require_available()?;
        state.archives.contains(id)
    }
}
