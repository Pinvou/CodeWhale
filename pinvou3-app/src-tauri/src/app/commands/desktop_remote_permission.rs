//! Windows remote permission commands (DEV-16).
//!
//! B can read pending approvals, stop or disconnect, but cannot approve. A decisions arrive exclusively through the
//! authenticated `remote_action_request` handler; no legacy Tauri decision or
//! request command remains. Local/Web ACP responders fail closed for requests
//! owned by a remote task.
//!
//! B mints opaque, non-reusable approval IDs. ACP callback IDs stay private;
//! remote decisions must echo the B-authoritative ID.

use std::sync::Arc;

use super::desktop_remote_state::DesktopRemoteState as State;

use crate::features::codex_acp::{AcpPool, RemotePermissionOwner};
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
#[cfg(test)]
use crate::features::remote_control::controller::permission::{
    DecisionAuthority, PermissionRequest,
};
use crate::features::remote_control::controller::permission::{
    PermissionAuthority, PermissionDecision, PermissionView,
};

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

#[cfg(test)]
pub use crate::features::remote_control::controller::permission::PermissionRequestInput as DesktopRemotePermissionRequestInput;

fn ensure_supported(controlled: &ControlledDeviceManager) -> Result<(), String> {
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
    controlled
        .is_supported()
        .then_some(())
        .ok_or_else(|| UNSUPPORTED.to_owned())
}

/// Sanitized local projection only. Decisions remain exclusive to remote A.
#[tauri::command]
pub(crate) fn desktop_remote_permission_list_b(
    task_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    authority: State<'_, Arc<PermissionAuthority>>,
) -> Result<Vec<PermissionView>, String> {
    ensure_supported(&controlled)?;
    Ok(authority.pending_for_task(&task_id))
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
    Ok(authority.deny_all_for_task(&task_id, &reason, authority.now_ms()))
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
            .any(|pending| pending.approval_id.as_deref() == Some(permission_request_id))
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
    fn request_input_cannot_supply_or_reuse_an_approval_id() {
        let input = serde_json::json!({
            "taskId": "task-0001", "operationType": "file_write", "riskLevel": "medium",
            "targetScope": "workspace-0001", "preview": "write", "expiresAtMs": 60_000
        });
        assert!(
            serde_json::from_value::<DesktopRemotePermissionRequestInput>(input.clone()).is_ok()
        );
        let mut injected = input;
        injected["permissionRequestId"] = serde_json::json!("old-request-id");
        assert!(serde_json::from_value::<DesktopRemotePermissionRequestInput>(injected).is_err());
    }

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
        let view = authority
            .request(
                &live,
                "task-0001",
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
                        &view.permission_request_id,
                        PermissionDecision::Allow,
                        1_100,
                    )
                    .unwrap_err()
                    .code,
                "permission_authority_invalid"
            );
        }
        assert_eq!(
            authority.view(&view.permission_request_id).unwrap().state,
            "pending"
        );
    }

    #[test]
    fn preview_sanitizer_is_reused_for_every_projection() {
        assert_eq!(sanitize_preview("token=abc"), "[REDACTED]");
        let raw_path = r"C:\tmp\x.txt";
        let sanitized = sanitize_preview(raw_path);
        assert_eq!(sanitized, "[REDACTED]");
        assert!(!sanitized.contains(raw_path));
        assert_eq!(sanitize_preview(&sanitized), sanitized);
        assert_eq!(sanitize_preview("[PATH]"), "[PATH]");
    }

    #[test]
    fn monotonic_clock_never_moves_backwards() {
        let authority = PermissionAuthority::default();
        let first = authority.now_ms();
        let second = authority.now_ms();
        assert!(second >= first);
    }
}
