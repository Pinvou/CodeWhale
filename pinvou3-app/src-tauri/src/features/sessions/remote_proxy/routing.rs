use std::sync::Arc;

use anyhow::{Result, bail};

use super::binding::LiveRemoteContext;
use crate::features::sessions::{SessionKind, SessionStore};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum LocalBackendBinding {
    Engine,
    Acp,
}

/// Installed by the app composition root from its durable ACP binding store.
/// Never supplied by the UI or inferred from the selected page/active Session.
pub(crate) type LocalBackendResolver =
    Arc<dyn Fn(&str) -> Result<LocalBackendBinding> + Send + Sync>;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SessionRoute {
    Engine,
    Acp,
    RemoteReadOnly,
    RemoteControl,
}

impl SessionStore {
    pub(crate) fn set_local_backend_resolver(&self, resolver: LocalBackendResolver) {
        *self.local_backend_resolver.write() = Some(resolver);
    }

    /// A remote route is never an Engine/Acp route, even when disconnected.
    /// `remote_ref`/`context` are required for every remote mutation.
    pub(crate) fn route_session(
        &self,
        id: &str,
        mutation: bool,
        remote_ref: Option<&str>,
        context: Option<&LiveRemoteContext>,
    ) -> Result<SessionRoute> {
        match self.session_kind(id)? {
            SessionKind::RemoteProxy => {
                if !mutation {
                    return Ok(SessionRoute::RemoteReadOnly);
                }
                let remote_ref =
                    remote_ref.ok_or_else(|| anyhow::anyhow!("missing remote reference"))?;
                let context =
                    context.ok_or_else(|| anyhow::anyhow!("missing live remote context"))?;
                self.require_live_remote_mutation(id, remote_ref, context)?;
                Ok(SessionRoute::RemoteControl)
            }
            SessionKind::ScheduledRun => {
                bail!("scheduled-run Session is not a conversational backend")
            }
            SessionKind::Chat => {
                if remote_ref.is_some() || context.is_some() {
                    bail!("remote binding cannot be applied to a local Session");
                }
                // A missing injection cannot guess that a durable ACP Session
                // belongs to EnginePool. Fail closed until the trusted store is ready.
                let resolver = self
                    .local_backend_resolver
                    .read()
                    .clone()
                    .ok_or_else(|| anyhow::anyhow!("local backend binding is unavailable"))?;
                match resolver(id)? {
                    LocalBackendBinding::Engine => Ok(SessionRoute::Engine),
                    LocalBackendBinding::Acp => Ok(SessionRoute::Acp),
                }
            }
        }
    }
}
