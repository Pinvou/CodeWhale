use anyhow::{Result, bail};
use uuid::Uuid;

use super::schema::{
    AuthenticatedProjection, MAX_REMOTE_PROXY_RECORDS, REMOTE_PROXY_SCHEMA_VERSION,
    RemoteProxyRecord, RemoteUiMetadata,
};
use super::{RemoteProxyState, migration::persist_changed};
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
    pub(crate) fn open_remote_proxy(
        &self,
        existing_id: Option<&str>,
        context: &LiveRemoteContext,
    ) -> Result<RemoteOpen> {
        if !context.valid() || !context.connected() {
            bail!("remote controller channel is not connected");
        }
        let mut state = self.remote_proxies.lock();
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
            return Ok(RemoteOpen {
                local_session_id: id.to_owned(),
                remote_session_ref: record.remote_session_ref.clone(),
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
        persist_changed(&mut state, &id, record)?;
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
        Ok(RemoteOpen {
            local_session_id: id,
            remote_session_ref: remote_ref,
        })
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

    /// A short resume can retain the same ref only inside the same boundary;
    /// the changed lease requires a fresh B mapping confirmation.
    pub(crate) fn resume_remote_proxy(&self, id: &str, context: &LiveRemoteContext) -> Result<()> {
        if !context.valid() || !context.connected() {
            bail!("remote controller channel is not connected");
        }
        let mut state = self.remote_proxies.lock();
        let record = state
            .records
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("unknown RemoteProxy"))?;
        if record.peer_id != context.peer_id {
            bail!("remote pairing mismatch");
        }
        let opening = state
            .openings
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("remote resume boundary expired"))?;
        if opening.context.resume_boundary_id != context.resume_boundary_id {
            bail!("remote resume boundary changed");
        }
        state.current_context = Some(context.clone());
        let opening = state.openings.get_mut(id).expect("checked opening");
        opening.context = context.clone();
        opening.confirmed = false;
        Ok(())
    }

    pub(crate) fn disconnect_remote_proxy(&self, id: &str) {
        let mut state = self.remote_proxies.lock();
        if state.openings.contains_key(id) {
            state.current_context = None;
            for opening in state.openings.values_mut() {
                opening.context.control_connected = false;
                opening.context.encrypted_channel_connected = false;
                opening.confirmed = false;
            }
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
