//! Windows remote permission commands (DEV-16).
//!
//! B can only stop or disconnect. A decisions arrive exclusively through the
//! authenticated `remote_action_request` handler; no legacy Tauri decision or
//! request command remains. Local/Web ACP responders fail closed for requests
//! owned by a remote task.

use std::sync::Arc;

use tauri::State;

use crate::features::codex_acp::{AcpPool, RemotePermissionOwner};
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
#[cfg(test)]
use crate::features::remote_control::controller::permission::{
    DecisionAuthority, PermissionBinding, PermissionRequest,
};
use crate::features::remote_control::controller::permission::{
    PermissionAuthority, PermissionDecision,
};

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

fn monotonic_millis() -> u64 {
    use std::sync::OnceLock;
    use std::time::Instant;
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

fn ensure_supported(controlled: &ControlledDeviceManager) -> Result<(), String> {
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
    controlled
        .is_supported()
        .then_some(())
        .ok_or_else(|| UNSUPPORTED.to_owned())
}

/// Called by every B-side terminal path (stop, refresh, disconnect, cancel).
#[tauri::command]
pub fn desktop_remote_permission_deny_task(
    task_id: String,
    reason: String,
    controlled: State<'_, ControlledDeviceManager>,
    authority: State<'_, Arc<PermissionAuthority>>,
) -> Result<usize, String> {
    ensure_supported(&controlled)?;
    Ok(authority.deny_all_for_task(&task_id, &reason, monotonic_millis()))
}

/// Narrow ACP adapter: the remote permission authority is the only responder
/// for a remote-sourced pending permission. A local/Web respond command must
/// never decide a remote request.
pub(crate) async fn respond_remote_permission_via_acp(
    acp_pool: &AcpPool,
    owner: &RemotePermissionOwner,
    permission_request_id: &str,
    decision: PermissionDecision,
) -> Result<(), String> {
    let result = acp_pool
        .respond_remote_permission(
            owner,
            permission_request_id,
            decision == PermissionDecision::Allow,
        )
        .await;
    if result.is_ok() {
        return Ok(());
    }
    // The ACP adapter may observe the authority decision and win the response
    // race. Treat that as the same successful resolution only while the exact
    // owner is still current and the exact request is no longer pending.
    if acp_pool.has_remote_permission_owner(owner)
        && !acp_pool
            .pending_remote_permissions(owner)
            .await
            .iter()
            .any(|pending| pending.permission_request_id == permission_request_id)
    {
        return Ok(());
    }
    Err("permission_dispatch_failed".to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::permission::sanitize_preview;

    #[test]
    fn command_surface_has_no_b_side_allow_entry() {
        assert_eq!(UNSUPPORTED, "desktop_remote_control_unsupported");
        // Behaviorally: the decision path is unreachable without a
        // server-derived A authority, even for a caller that owns a valid
        // pending request.
        let authority = PermissionAuthority::default();
        let live = crate::features::remote_control::controller::protocol::LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        };
        let binding = PermissionBinding::new(&live, "task-0001", "permission-0001").unwrap();
        authority
            .request(
                binding,
                PermissionRequest {
                    operation_type: "shell_command".into(),
                    risk_level: "high".into(),
                    target_scope: "workspace-0001".into(),
                    preview: "run the tests".into(),
                    expires_at_ms: 61_000,
                },
                1_000,
            )
            .unwrap();
        for authority_kind in [
            DecisionAuthority::ControlledB,
            DecisionAuthority::LocalOperator,
        ] {
            assert_eq!(
                authority
                    .decide(
                        authority_kind,
                        &live,
                        "task-0001",
                        "permission-0001",
                        PermissionDecision::Allow,
                        1_100,
                    )
                    .unwrap_err()
                    .code,
                "permission_authority_invalid"
            );
        }
        assert_eq!(authority.view("permission-0001").unwrap().state, "pending");
    }

    #[test]
    fn preview_sanitizer_is_reused_for_every_projection() {
        assert_eq!(sanitize_preview("token=abc"), "[REDACTED]");
        assert_eq!(sanitize_preview(r"C:\tmp\x.txt"), "[PATH]");
    }

    #[test]
    fn monotonic_clock_never_moves_backwards() {
        let first = monotonic_millis();
        let second = monotonic_millis();
        assert!(second >= first);
    }
}
