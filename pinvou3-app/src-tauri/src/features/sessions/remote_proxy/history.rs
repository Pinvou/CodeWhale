//! RemoteProxy history deletion is a durable UI soft-hide, never record
//! removal: pending requests and permanent retired-ID tombstones remain owned
//! by SessionStore even after the user removes an item from history.

use anyhow::{Result, bail};

use super::persistence::persist_changed;
use super::schema::PendingDispatchState;
use crate::features::sessions::SessionStore;

impl SessionStore {
    pub(crate) fn hide_remote_proxy_history(&self, local_session_id: &str) -> Result<()> {
        let mut state = self.remote_proxies.lock();
        state.require_available()?;
        if state.archives.contains(local_session_id)? {
            return state.archives.hide(local_session_id);
        }
        let mut record = state
            .records
            .get(local_session_id)
            .ok_or_else(|| anyhow::anyhow!("remote_proxy_not_found"))?
            .clone();
        if record.local_session_id != local_session_id || !local_session_id.starts_with("remote-") {
            bail!("remote_proxy_not_found");
        }
        if record
            .pending
            .iter()
            .any(|pending| pending.dispatch_state == PendingDispatchState::PreparedNeverAttempted)
        {
            bail!("remote_unattempted_pending_exists");
        }
        if record.ui.hidden {
            return Ok(());
        }
        record.ui.hidden = true;
        persist_changed(&mut state, local_session_id, record)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;
    use crate::features::sessions::remote_proxy::schema::{
        RetiredRemoteRequest, RetiredRequestDisposition,
    };

    #[test]
    fn soft_hide_preserves_remote_record_and_permanent_request_tombstones() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = LiveRemoteContext {
            peer_id: "peer-1".into(),
            resume_boundary_id: "resume-1".into(),
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "epoch-1".into(),
            relay_epoch: "relay-1".into(),
            generation: "generation-1".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        };
        let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
        {
            let mut state = store.remote_proxies.lock();
            let mut record = state.records[&opened.local_session_id].clone();
            record.retired_requests.push(RetiredRemoteRequest {
                client_request_id: "retired-request-1".into(),
                task_id: "retired-task-1".into(),
                request_fingerprint: "a".repeat(64),
                disposition: RetiredRequestDisposition::Terminal,
            });
            persist_changed(&mut state, &opened.local_session_id, record).unwrap();
        }
        store
            .hide_remote_proxy_history(&opened.local_session_id)
            .unwrap();
        store
            .hide_remote_proxy_history(&opened.local_session_id)
            .unwrap();
        let record = store
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap();
        assert!(record.ui.hidden);
        assert_eq!(record.retired_requests.len(), 1);
        assert!(store.hide_remote_proxy_history("ordinary-session").is_err());
        drop(store);
        let restarted = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let record = restarted
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap();
        assert!(record.ui.hidden);
        assert_eq!(record.retired_requests[0].task_id, "retired-task-1");
    }
}
