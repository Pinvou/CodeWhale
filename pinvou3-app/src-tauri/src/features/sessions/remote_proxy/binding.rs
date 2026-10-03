use anyhow::{Result, bail};
use uuid::Uuid;

use super::schema::{
    AuthenticatedProjection, MAX_REMOTE_PROXY_RECORDS, REMOTE_PROXY_SCHEMA_VERSION,
    RemoteProxyRecord, RemoteUiMetadata,
};
use super::{RemoteProxyState, persistence::persist_changed};
use crate::features::sessions::SessionStore;

/// Supplied only by the authenticated controller boundary, never by UI input.
/// All fields are checked again for every state-changing operation.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct LiveRemoteContext {
    pub(crate) peer_id: String,
    pub(crate) resume_boundary_id: String,
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: String,
    pub(crate) relay_epoch: String,
    pub(crate) generation: String,
    pub(crate) control_connected: bool,
    pub(crate) encrypted_channel_connected: bool,
}

impl LiveRemoteContext {
    fn valid(&self) -> bool {
        [
            &self.peer_id,
            &self.resume_boundary_id,
            &self.connection_id,
            &self.controller_lease_id,
            &self.lease_epoch,
            &self.relay_epoch,
            &self.generation,
        ]
        .iter()
        .all(|value| !value.is_empty())
    }

    fn connected(&self) -> bool {
        self.control_connected && self.encrypted_channel_connected
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct RemoteOpen {
    pub(crate) local_session_id: String,
    pub(crate) remote_session_ref: String,
}

/// An unpersisted identity allocated inside Rust. Only commit creates a visible record.
pub(crate) struct PreparedRemoteOpen {
    pub(crate) open: RemoteOpen,
    context: LiveRemoteContext,
    existing: bool,
}

/// B's authenticated acknowledgement that this opaque reference maps to
/// exactly one B-owned Session. This is not a local execution authorization.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct AuthenticatedMapping {
    pub(crate) remote_session_ref: String,
    pub(crate) controlled_session_id: String,
}

#[derive(Clone)]
pub(super) struct LiveRemoteBinding {
    pub(super) context: LiveRemoteContext,
    pub(super) confirmed: bool,
}

impl SessionStore {
    /// An omitted id starts a new A-local Session. Retrying an id is permitted
    /// only inside the same in-process resume boundary. A restart or re-pair
    /// must create a new id/ref; persisted history cannot authorize a reopen.
    pub(crate) fn prepare_remote_proxy_open(
        &self,
        existing_id: Option<&str>,
        context: &LiveRemoteContext,
    ) -> Result<PreparedRemoteOpen> {
        if !context.valid() || !context.connected() {
            bail!("remote controller channel is not connected");
        }
        let state = self.remote_proxies.lock();
        state.require_available()?;
        if let Some(id) = existing_id {
            let record = state
                .records
                .get(id)
                .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?;
            let opening = state.openings.get(id).ok_or_else(|| {
                anyhow::anyhow!("remote session must be reopened with a new reference")
            })?;
            if state.current_context.as_ref() != Some(context)
                || record.peer_id != context.peer_id
                || opening.context.peer_id != context.peer_id
                || opening.context.resume_boundary_id != context.resume_boundary_id
                || opening.context != *context
            {
                bail!("remote session belongs to another pairing or resume boundary");
            }
            return Ok(PreparedRemoteOpen {
                open: RemoteOpen {
                    local_session_id: id.to_owned(),
                    remote_session_ref: record.remote_session_ref.clone(),
                },
                context: context.clone(),
                existing: true,
            });
        }
        if state.records.len() >= MAX_REMOTE_PROXY_RECORDS {
            bail!("RemoteProxy record capacity reached");
        }
        let id = format!("remote-{}", Uuid::new_v4().simple());
        let remote_ref = Uuid::new_v4().to_string();
        if self
            .manager
            .sessions_dir()
            .join(format!("{id}.json"))
            .exists()
        {
            bail!("RemoteProxy id collides with a local Session");
        }
        Ok(PreparedRemoteOpen {
            open: RemoteOpen {
                local_session_id: id,
                remote_session_ref: remote_ref,
            },
            context: context.clone(),
            existing: false,
        })
    }

    pub(crate) fn commit_prepared_remote_proxy_open(
        &self,
        prepared: PreparedRemoteOpen,
        context: &LiveRemoteContext,
    ) -> Result<RemoteOpen> {
        if !context.valid() || !context.connected() || prepared.context != *context {
            bail!("remote controller channel changed");
        }
        let mut state = self.remote_proxies.lock();
        state.require_available()?;
        let id = &prepared.open.local_session_id;
        let remote_ref = &prepared.open.remote_session_ref;
        if prepared.existing {
            let record = state
                .records
                .get(id)
                .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?;
            let opening = state
                .openings
                .get(id)
                .ok_or_else(|| anyhow::anyhow!("remote session is offline"))?;
            if state.current_context.as_ref() != Some(context)
                || record.remote_session_ref != *remote_ref
                || record.peer_id != context.peer_id
                || opening.context != *context
            {
                bail!("remote session binding changed");
            }
            return Ok(prepared.open);
        }
        if state.records.len() >= MAX_REMOTE_PROXY_RECORDS
            || state.records.contains_key(id)
            || state
                .records
                .values()
                .any(|record| record.remote_session_ref == *remote_ref)
            || self
                .manager
                .sessions_dir()
                .join(format!("{id}.json"))
                .exists()
        {
            bail!("RemoteProxy identity is no longer available");
        }
        let record = RemoteProxyRecord {
            schema_version: REMOTE_PROXY_SCHEMA_VERSION,
            local_session_id: id.clone(),
            peer_id: context.peer_id.clone(),
            remote_session_ref: remote_ref.clone(),
            controlled_session_id: None,
            ui: RemoteUiMetadata::default(),
            authenticated_cursor: None,
            retired_epochs: Vec::new(),
            projection: AuthenticatedProjection::default(),
            pending: Vec::new(),
            retired_requests: Vec::new(),
        };
        persist_changed(&mut state, id, record)?;
        if state.current_context.as_ref().is_some_and(|current| {
            current.peer_id != context.peer_id
                || current.resume_boundary_id != context.resume_boundary_id
                || current.connection_id != context.connection_id
                || current.controller_lease_id != context.controller_lease_id
                || current.lease_epoch != context.lease_epoch
                || current.relay_epoch != context.relay_epoch
                || current.generation != context.generation
        }) {
            state.openings.clear();
        }
        state.current_context = Some(context.clone());
        state.openings.insert(
            id.clone(),
            LiveRemoteBinding {
                context: context.clone(),
                confirmed: false,
            },
        );
        Ok(prepared.open)
    }

    /// Test setup exercises the same explicit prepare/commit contract used by
    /// the host-gated command; it is absent from production builds.
    #[cfg(test)]
    pub(crate) fn test_commit_remote_proxy_open(
        &self,
        existing_id: Option<&str>,
        context: &LiveRemoteContext,
    ) -> Result<RemoteOpen> {
        let prepared = self.prepare_remote_proxy_open(existing_id, context)?;
        self.commit_prepared_remote_proxy_open(prepared, context)
    }

    /// The caller must verify B's authenticated response before invoking this.
    pub(crate) fn confirm_remote_mapping(
        &self,
        id: &str,
        context: &LiveRemoteContext,
        mapping: &AuthenticatedMapping,
    ) -> Result<()> {
        if !context.valid() || !context.connected() || mapping.controlled_session_id.is_empty() {
            bail!("invalid live remote mapping");
        }
        let mut state = self.remote_proxies.lock();
        if state.current_context.as_ref() != Some(context) {
            bail!("remote mapping is not on the current live connection");
        }
        let opening = state
            .openings
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("no in-process remote open"))?;
        if opening.context != *context {
            bail!("remote connection, lease or generation changed");
        }
        let mut record = state
            .records
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?
            .clone();
        if record.peer_id != context.peer_id
            || record.remote_session_ref != mapping.remote_session_ref
        {
            bail!("remote session reference mismatch");
        }
        if record
            .controlled_session_id
            .as_ref()
            .is_some_and(|old| old != &mapping.controlled_session_id)
        {
            bail!("remote session reference remapped to a different B Session");
        }
        record.controlled_session_id = Some(mapping.controlled_session_id.clone());
        persist_changed(&mut state, id, record)?;
        state
            .openings
            .get_mut(id)
            .expect("checked opening")
            .confirmed = true;
        Ok(())
    }

    #[cfg(test)]
    pub(crate) fn test_expire_remote_binding(&self, id: &str) {
        let mut state = self.remote_proxies.lock();
        if state.openings.contains_key(id) {
            state.current_context = None;
            state.openings.clear();
        }
    }

    pub(crate) fn require_live_remote_mutation(
        &self,
        id: &str,
        remote_ref: &str,
        context: &LiveRemoteContext,
    ) -> Result<()> {
        let state = self.remote_proxies.lock();
        require_live_locked(&state, id, remote_ref, context)
    }

    pub(crate) fn remote_proxy_snapshot(&self, id: &str) -> Option<RemoteProxyRecord> {
        self.remote_proxies.lock().records.get(id).cloned()
    }

    /// Lookup is not admission. The caller must still use confirm_remote_mapping,
    /// which rechecks the current live opening under the same store lock as write.
    pub(crate) fn remote_proxy_local_id_for_ref(&self, remote_ref: &str) -> Option<String> {
        self.remote_proxies
            .lock()
            .records
            .values()
            .find(|record| record.remote_session_ref == remote_ref)
            .map(|record| record.local_session_id.clone())
    }

    pub(crate) fn current_remote_proxy_id_for_ref(
        &self,
        remote_ref: &str,
        context: &LiveRemoteContext,
    ) -> Result<String> {
        let state = self.remote_proxies.lock();
        if state.current_context.as_ref() != Some(context) {
            bail!("remote session connection changed");
        }
        let record = state
            .records
            .values()
            .find(|record| record.remote_session_ref == remote_ref)
            .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy reference"))?;
        let opening = state
            .openings
            .get(&record.local_session_id)
            .ok_or_else(|| anyhow::anyhow!("remote session is offline"))?;
        if record.peer_id != context.peer_id || opening.context != *context {
            bail!("remote session binding changed");
        }
        Ok(record.local_session_id.clone())
    }
}

pub(super) fn require_live_locked(
    state: &RemoteProxyState,
    id: &str,
    remote_ref: &str,
    context: &LiveRemoteContext,
) -> Result<()> {
    let record = state
        .records
        .get(id)
        .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?;
    let opening = state
        .openings
        .get(id)
        .ok_or_else(|| anyhow::anyhow!("remote session is offline"))?;
    if state.current_context.as_ref() != Some(context)
        || record.remote_session_ref != remote_ref
        || record.peer_id != context.peer_id
        || record.controlled_session_id.is_none()
        || !opening.confirmed
        || opening.context != *context
        || !context.valid()
        || !context.connected()
    {
        bail!("remote session has no matching live binding");
    }
    Ok(())
}

/// Terminal correlation must bind the exact B-owned Session in the same lock
/// acquisition as the current A lease/opening check. A prior read-only check
/// is not authority after disconnect, resume, or re-pair.
pub(crate) fn require_live_mapping_locked(
    state: &RemoteProxyState,
    id: &str,
    remote_ref: &str,
    context: &LiveRemoteContext,
    controlled_session_id: &str,
) -> Result<()> {
    require_live_locked(state, id, remote_ref, context)?;
    if controlled_session_id.is_empty()
        || state
            .records
            .get(id)
            .and_then(|record| record.controlled_session_id.as_deref())
            != Some(controlled_session_id)
    {
        bail!("remote terminal B Session mapping mismatch");
    }
    Ok(())
}
