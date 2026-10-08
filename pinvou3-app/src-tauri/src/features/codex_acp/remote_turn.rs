//! DEV-13 adapter: code-mode remote turns over the ACP pool.
//!
//! A remote code task never carries a host path. The A-side workspace field is
//! an opaque ID that must be registered here by the command layer *after* the
//! DEV-12 capability snapshot and the B workspace owner both accepted it. The
//! ACP session keeps running in its own preauthorized execution workspace, and
//! this adapter only submits the prompt text.

use std::collections::HashSet;
use std::sync::Arc;
use std::time::{Duration, Instant};

use async_trait::async_trait;
use parking_lot::Mutex;
use tokio_util::sync::CancellationToken;

use super::{AcpPool, AcpSession};
use crate::features::remote_control::controller::task::{RemoteCodeTurnPort, TASK_CANCELLED_CODE};
use crate::features::remote_control::controller::turn_budget::CANCEL_SETTLE_BUDGET;
use crate::features::remote_control::controller::turn_budget::{TURN_TIMEOUT_CODE, TurnBudget};

const POLL_INTERVAL: Duration = Duration::from_millis(200);
const BUSY_GRACE: Duration = Duration::from_secs(5);
const MAX_AUTHORIZED_REFS: usize = 64;

/// Captured synchronously by the ACP admission path. Never look up the
/// session's CURRENT turn/runtime when this old remote task later expires.
pub(super) struct OwnedAcpTurn {
    runtime: Arc<AcpSession>,
    turn_id: String,
}

impl OwnedAcpTurn {
    pub(super) fn new(runtime: Arc<AcpSession>, turn_id: String) -> Self {
        Self { runtime, turn_id }
    }

    fn is_active(&self) -> bool {
        self.runtime.bridge.current_turn_id().as_deref() == Some(self.turn_id.as_str())
    }

    async fn retire(&self, pool: &AcpPool, reason: &str) {
        let _ = tokio::time::timeout(
            CANCEL_SETTLE_BUDGET,
            pool.retire_remote_turn(self.runtime.clone(), &self.turn_id, reason),
        )
        .await;
    }
}

/// Bounded registry of opaque `(session, workspace)` authorizations. Entries
/// are written only by the command layer and dropped when the connection ends.
#[derive(Default)]
pub(crate) struct RemoteWorkspaceAuthorizations {
    entries: Mutex<HashSet<(String, String)>>,
}

impl RemoteWorkspaceAuthorizations {
    pub(crate) fn authorize(&self, session_id: &str, workspace_id: &str) -> bool {
        let mut entries = self.entries.lock();
        if entries.len() >= MAX_AUTHORIZED_REFS
            && !entries.contains(&(session_id.to_owned(), workspace_id.to_owned()))
        {
            return false;
        }
        entries.insert((session_id.to_owned(), workspace_id.to_owned()));
        true
    }

    pub(crate) fn is_authorized(&self, session_id: &str, workspace_id: &str) -> bool {
        self.entries
            .lock()
            .contains(&(session_id.to_owned(), workspace_id.to_owned()))
    }

    pub(crate) fn revoke_session(&self, session_id: &str) {
        self.entries
            .lock()
            .retain(|(candidate, _)| candidate != session_id);
    }

    pub(crate) fn clear(&self) {
        self.entries.lock().clear();
    }
}

pub(crate) struct AcpPoolCodeTurnPort {
    pool: Arc<AcpPool>,
    authorizations: Arc<RemoteWorkspaceAuthorizations>,
}

impl AcpPoolCodeTurnPort {
    pub(crate) fn new(
        pool: Arc<AcpPool>,
        authorizations: Arc<RemoteWorkspaceAuthorizations>,
    ) -> Self {
        Self {
            pool,
            authorizations,
        }
    }
}

#[async_trait]
impl RemoteCodeTurnPort for AcpPoolCodeTurnPort {
    async fn run_code_turn(
        &self,
        session_id: &str,
        workspace_id: &str,
        content: String,
        cancel: CancellationToken,
    ) -> std::result::Result<(), String> {
        let budget = TurnBudget::from_environment()?;
        if cancel.is_cancelled() {
            return Err(TASK_CANCELLED_CODE.into());
        }
        if !self.authorizations.is_authorized(session_id, workspace_id) {
            return Err("task_workspace_not_authorized".to_owned());
        }
        let mut owned = None;
        let submitted = budget
            .run(&cancel, async {
                owned = Some(
                    self.pool
                        .send_message_with_owner(session_id, content, Vec::new(), Vec::new())
                        .await
                        .map_err(|_| "task_engine_unavailable".to_owned())?,
                );
                Ok(())
            })
            .await;
        if let Err(error) = submitted {
            if let Some(owned) = owned {
                owned.retire(&self.pool, &error).await;
            }
            return Err(error);
        }
        let owned = owned.ok_or_else(|| "task_turn_not_started".to_owned())?;

        // The ACP prompt is submitted in the background, so wait for the busy
        // transition before trusting a "not busy" observation.
        let mut observed_busy = false;
        let started = Instant::now();
        let result = budget
            .run(&cancel, async {
                loop {
                    let busy = owned.is_active();
                    if busy {
                        observed_busy = true;
                    } else if observed_busy {
                        return Ok(());
                    } else if started.elapsed() >= BUSY_GRACE {
                        return Err("task_turn_not_started".to_owned());
                    }
                    tokio::select! {
                        _ = cancel.cancelled() => {
                            return Err(TASK_CANCELLED_CODE.to_owned());
                        }
                        _ = tokio::time::sleep(POLL_INTERVAL) => {}
                    }
                }
            })
            .await;
        if matches!(
            result.as_ref().err().map(String::as_str),
            Some(TASK_CANCELLED_CODE | TURN_TIMEOUT_CODE)
        ) {
            owned
                .retire(
                    &self.pool,
                    result
                        .as_ref()
                        .err()
                        .map(String::as_str)
                        .unwrap_or(TASK_CANCELLED_CODE),
                )
                .await;
        }
        result
    }
}

/// Poll interval exported for the wiring layer and tests.
pub(crate) const REMOTE_CODE_TURN_POLL_INTERVAL: Duration = POLL_INTERVAL;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn authorization_registry_is_bounded_and_session_scoped() {
        let registry = RemoteWorkspaceAuthorizations::default();
        assert!(registry.authorize("session-0001", "workspace-0001"));
        assert!(registry.is_authorized("session-0001", "workspace-0001"));
        assert!(!registry.is_authorized("session-0002", "workspace-0001"));
        assert!(!registry.is_authorized("session-0001", "workspace-0002"));

        registry.revoke_session("session-0001");
        assert!(!registry.is_authorized("session-0001", "workspace-0001"));
    }

    #[test]
    fn authorization_registry_rejects_growth_past_the_cap() {
        let registry = RemoteWorkspaceAuthorizations::default();
        for index in 0..MAX_AUTHORIZED_REFS {
            assert!(registry.authorize("session-0001", &format!("workspace-{index:04}")));
        }
        assert!(!registry.authorize("session-0001", "workspace-overflow"));
        // Re-authorizing an existing pair stays idempotent at the cap.
        assert!(registry.authorize("session-0001", "workspace-0000"));
        registry.clear();
        assert!(registry.authorize("session-0001", "workspace-overflow"));
    }
}
