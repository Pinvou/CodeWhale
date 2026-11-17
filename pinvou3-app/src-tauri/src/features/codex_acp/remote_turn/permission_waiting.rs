//! Task-state projection for the exact ACP approval owner.
use super::{RemotePermissionOwner, RemoteTaskService, RemoteTaskState};
use crate::features::codex_acp::RemoteOwnedPermission;
use crate::features::remote_control::controller::task::TaskLeaseBinding;

use super::{
    AcpPool, ControllerTaskEventSink, PermissionAuthority, REMOTE_PERMISSION_TTL_MS,
    permission_mint_projection,
};
use crate::features::remote_control::controller::permission::PermissionView;
use std::{collections::HashSet, sync::Arc};

// This seam keeps the polling state machine independent of Tauri. Production
// still enters the pool's runtime/turn-fenced operations unchanged.
#[async_trait::async_trait]
pub(super) trait PermissionPool: Send + Sync {
    async fn pending_remote_permissions(
        &self,
        owner: &RemotePermissionOwner,
    ) -> Vec<RemoteOwnedPermission>;
    async fn mint_remote_permission(
        &self,
        owner: &RemotePermissionOwner,
        callback: &str,
        authority: &PermissionAuthority,
        ledger: &crate::features::remote_control::controller::task::RemoteTaskLedger,
        ttl: u64,
    ) -> anyhow::Result<PermissionView>;
    async fn respond_remote_permission(
        &self,
        owner: &RemotePermissionOwner,
        approval: &str,
        allow: bool,
    ) -> anyhow::Result<()>;
}
#[async_trait::async_trait]
impl PermissionPool for AcpPool {
    async fn pending_remote_permissions(
        &self,
        owner: &RemotePermissionOwner,
    ) -> Vec<RemoteOwnedPermission> {
        AcpPool::pending_remote_permissions(self, owner).await
    }
    async fn mint_remote_permission(
        &self,
        owner: &RemotePermissionOwner,
        callback: &str,
        authority: &PermissionAuthority,
        ledger: &crate::features::remote_control::controller::task::RemoteTaskLedger,
        ttl: u64,
    ) -> anyhow::Result<PermissionView> {
        AcpPool::mint_remote_permission(self, owner, callback, authority, ledger, ttl).await
    }
    async fn respond_remote_permission(
        &self,
        owner: &RemotePermissionOwner,
        approval: &str,
        allow: bool,
    ) -> anyhow::Result<()> {
        AcpPool::respond_remote_permission(self, owner, approval, allow).await
    }
}

pub(super) async fn sync(
    pool: &impl PermissionPool,
    authority: &PermissionAuthority,
    tasks: &RemoteTaskService,
    owner: &RemotePermissionOwner,
    journal: &mut super::PermissionJournal,
    events: &Arc<dyn ControllerTaskEventSink>,
) -> Result<(), String> {
    let pending = pool.pending_remote_permissions(owner).await;
    let now =
        crate::features::remote_control::controller::relay_runtime::controlled_monotonic_millis();
    authority.sweep_expired(authority.now_ms());
    for request in pending {
        if let Some(approval_id) = journal.approval(&request.callback_id) {
            if request.approval_id.as_deref() != Some(approval_id) {
                return Err("permission_binding_stale".to_owned());
            }
            let view = authority
                .view_for_binding(&owner.lease, &owner.task_id, approval_id)
                .map_err(|error| error.code.to_owned())?;
            let decision = match view.state.as_str() {
                "allowed" => Some(true),
                "denied" | "expired" => Some(false),
                _ => None,
            };
            if let Some(allow) = decision {
                let resolution = pool
                    .respond_remote_permission(owner, approval_id, allow)
                    .await;
                if resolution.is_err()
                    && pool
                        .pending_remote_permissions(owner)
                        .await
                        .iter()
                        .any(|pending| pending.approval_id.as_deref() == Some(approval_id))
                {
                    return Err("permission_dispatch_failed".to_owned());
                }
            }
            continue;
        }
        let minted = pool
            .mint_remote_permission(
                owner,
                &request.callback_id,
                &authority,
                &tasks.ledger(),
                REMOTE_PERMISSION_TTL_MS,
            )
            .await;
        // No approval is minted during the brief journal resource gate.
        // Retry at the next bounded poll without retiring this runtime.
        let Some(view) = permission_mint_projection(minted)? else {
            continue;
        };
        let record = tasks
            .status(&owner.task_id)
            .map_err(|error| error.code.to_owned())?;
        if record.sequence != owner.task_sequence
            || record.session_id != owner.session_id
            || record.lease
                != crate::features::remote_control::controller::task::TaskLeaseBinding::from_context(
                    &owner.lease,
                )
        {
            return Err("permission_binding_stale".to_owned());
        }
        if record.state == RemoteTaskState::Running {
            tasks
                .enter_waiting_permission(&owner.task_id, owner.task_sequence, now)
                .map_err(|error| error.code.to_owned())?;
        } else if record.state != RemoteTaskState::WaitingPermission {
            return Err("task_state_conflict".to_owned());
        }
        journal.register(&request.callback_id, &view, events.as_ref())?;
    }

    // Responses in this very poll can remove entries. Observe the actual
    // pending set again rather than requiring another 200ms iteration.
    let remaining = pool.pending_remote_permissions(owner).await;
    let current = remaining
        .iter()
        .map(|request| request.callback_id.clone())
        .collect::<HashSet<_>>();
    if journal.project_missing(authority, tasks, events.as_ref(), &current)? > 0 {
        resume_when_clear(tasks, owner, &remaining)?;
    }
    Ok(())
}

pub(super) fn resume_when_clear(
    tasks: &RemoteTaskService,
    owner: &RemotePermissionOwner,
    remaining: &[RemoteOwnedPermission],
) -> Result<(), String> {
    if !remaining.is_empty() {
        return Ok(());
    }
    let record = tasks
        .status(&owner.task_id)
        .map_err(|error| error.code.to_owned())?;
    if record.sequence != owner.task_sequence
        || record.session_id != owner.session_id
        || record.lease != TaskLeaseBinding::from_context(&owner.lease)
    {
        return Err("permission_binding_stale".to_owned());
    }
    if record.state == RemoteTaskState::WaitingPermission {
        tasks.resume_running(
            &owner.task_id,
            owner.task_sequence,
            crate::features::remote_control::controller::relay_runtime::controlled_monotonic_millis(),
        ).map_err(|error| error.code.to_owned())?;
    }
    Ok(())
}

#[cfg(test)]
use std::collections::HashMap;
#[cfg(test)]
mod tests;
