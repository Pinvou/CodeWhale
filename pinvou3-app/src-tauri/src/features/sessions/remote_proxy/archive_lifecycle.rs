//! Host-fenced archive lifecycle and active-registry cleanup.
use super::schema::RemoteProxyListItem;
use anyhow::{Result, bail};

impl super::super::SessionStore {
    /// Expose only an allowlisted category; the cause may contain paths or
    /// cache contents and must not cross the WebView boundary.
    pub(crate) fn remote_proxy_load_failure_code(&self) -> Option<&'static str> {
        use super::RemoteProxyLoadFailure;
        match self.remote_proxies.lock().load_failure {
            None => None,
            Some(RemoteProxyLoadFailure::Unreadable) => Some("remote_proxy_unreadable"),
            Some(RemoteProxyLoadFailure::Oversized) => Some("remote_proxy_oversized"),
            Some(RemoteProxyLoadFailure::Malformed) => Some("remote_proxy_malformed"),
            Some(RemoteProxyLoadFailure::UnsupportedVersion) => {
                Some("remote_proxy_version_unsupported")
            }
            Some(RemoteProxyLoadFailure::InvalidRecords) => Some("remote_proxy_invalid_records"),
            Some(RemoteProxyLoadFailure::ArchiveUnavailable) => {
                Some("remote_proxy_archive_unavailable")
            }
        }
    }

    /// Caller holds the controller lifecycle owner while supplying its current
    /// authenticated identity (including resumable/disconnected leases).
    pub(crate) fn archive_remote_proxy(
        &self,
        id: &str,
        live: Option<&super::binding::LiveRemoteContext>,
    ) -> Result<bool> {
        self.seal_remote_proxy_history(id, live, false)
    }

    /// Irreversible, explicitly confirmed exit. Pending records remain frozen
    /// facts; this neither cancels remote execution nor invents a result.
    pub(crate) fn quarantine_remote_proxy(
        &self,
        id: &str,
        live: Option<&super::binding::LiveRemoteContext>,
        confirmed: bool,
    ) -> Result<bool> {
        if !confirmed {
            bail!("remote_proxy_quarantine_confirmation_required");
        }
        self.seal_remote_proxy_history(id, live, true)
    }

    fn seal_remote_proxy_history(
        &self,
        id: &str,
        live: Option<&super::binding::LiveRemoteContext>,
        quarantine: bool,
    ) -> Result<bool> {
        let mut state = self.remote_proxies.lock();
        state.require_available()?;
        if state.archives.contains(id)? {
            let snapshot = state
                .archives
                .read_snapshot(id)?
                .ok_or_else(|| anyhow::anyhow!("remote_proxy_archive_unavailable"))?;
            if snapshot.is_quarantined() != quarantine {
                bail!("remote_proxy_archive_conflict");
            }
            if let Some(record) = state.records.get(id) {
                let mut archived = snapshot.into_record();
                // Another cooperating instance may have sealed this exact
                // residue after our active JSON was loaded. Hidden is a separate
                // archive UI flag; every immutable field must still match.
                archived.ui.hidden = record.ui.hidden;
                if archived != *record {
                    bail!("remote_proxy_archive_conflict");
                }
                state.records.remove(id);
                state.openings.remove(id);
            }
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
        if !quarantine && !record.pending.is_empty() {
            bail!("remote_proxy_archive_pending");
        }
        state
            .archives
            .require_active_records(state.records.values())?;
        // The archive anchor has its own bounded storage, so even a completely
        // full legacy active JSON can seal without first growing that JSON.
        let sealed = if quarantine {
            state.archives.quarantine(&record)
        } else {
            state.archives.seal(&record)
        };
        if let Err(error) = sealed {
            // A proven rejection before intent is retryable. Ambiguous I/O
            // still disables remote mutations until strict startup recovery.
            if error.requires_quarantine() {
                state.load_failed = true;
            }
            return Err(error.into_error());
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
