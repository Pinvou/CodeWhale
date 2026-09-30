//! Windows remote permission commands (DEV-16).
//!
//! B can only list, stop or disconnect; the only Allow/Deny entry is
//! `desktop_remote_permission_decide`, which requires the local A controller
//! authority and is rejected for any other caller. The existing Web ACP respond
//! commands are not a remote authority path: a remote task must go through this
//! module, and the ACP adapter refuses non-controller sources.

use std::sync::Arc;

use serde::Deserialize;
use serde_json::Value;
use tauri::State;

use crate::features::codex_acp::AcpPool;
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::permission::{
    DecisionAuthority, DecisionOutcome, DispatchCommit, PermissionAuthority, PermissionBinding,
    PermissionDecision, PermissionRequest, PermissionView,
};
use crate::features::remote_control::controller::task::RemoteTaskLedger;

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct DesktopRemotePermissionRequestInput {
    task_id: String,
    permission_request_id: String,
    operation_type: String,
    risk_level: String,
    target_scope: String,
    preview: String,
    expires_at_ms: u64,
}

fn monotonic_millis() -> u64 {
    use std::sync::OnceLock;
    use std::time::Instant;
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

fn ensure_supported(controlled: &ControlledDeviceManager) -> Result<(), String> {
    if controlled.is_supported() {
        Ok(())
    } else {
        Err(UNSUPPORTED.to_owned())
    }
}

#[tauri::command]
pub fn desktop_remote_permission_request(
    request: DesktopRemotePermissionRequestInput,
    controlled: State<'_, ControlledDeviceManager>,
    authority: State<'_, Arc<PermissionAuthority>>,
) -> Result<PermissionView, String> {
    ensure_supported(&controlled)?;
    let live = controlled
        .live_lease()
        .map_err(|error| error.code.to_owned())?;
    let binding = PermissionBinding::new(&live, &request.task_id, &request.permission_request_id)
        .map_err(|error| error.code.to_owned())?;
    authority
        .request(
            binding,
            PermissionRequest {
                operation_type: request.operation_type,
                risk_level: request.risk_level,
                target_scope: request.target_scope,
                preview: request.preview,
                expires_at_ms: request.expires_at_ms,
            },
            monotonic_millis(),
        )
        .map_err(|error| error.code.to_owned())
}

/// The only remote Allow/Deny entry point. The `authority` value is derived
/// server-side: a B-side caller cannot reach it, and the ledger is consulted to
/// make sure the request belongs to a live remote task.
#[tauri::command]
pub fn desktop_remote_permission_decide(
    task_id: String,
    permission_request_id: String,
    decision: String,
    controlled: State<'_, ControlledDeviceManager>,
    authority: State<'_, Arc<PermissionAuthority>>,
    ledger: State<'_, Arc<RemoteTaskLedger>>,
) -> Result<Value, String> {
    ensure_supported(&controlled)?;
    let live = controlled
        .live_lease()
        .map_err(|error| error.code.to_owned())?;
    let record = ledger
        .record(&task_id)
        .map_err(|error| error.code.to_owned())?;
    if record.is_terminal() {
        return Err("task_state_conflict".to_owned());
    }
    let decision = PermissionDecision::parse(&decision)
        .ok_or_else(|| "permission_decision_invalid".to_owned())?;
    let outcome = authority
        .decide(
            DecisionAuthority::ControllerA,
            &live,
            &task_id,
            &permission_request_id,
            decision,
            monotonic_millis(),
        )
        .map_err(|error| error.code.to_owned())?;

    // Atomic dispatch boundary: only an allow that wins the CAS may run.
    if matches!(
        outcome,
        DecisionOutcome::Decided {
            decision: PermissionDecision::Allow,
            ..
        }
    ) {
        match authority
            .commit_dispatch(&permission_request_id, monotonic_millis())
            .map_err(|error| error.code.to_owned())?
        {
            DispatchCommit::Committed => {}
            DispatchCommit::Rejected { code } => return Err(code.to_owned()),
        }
    }
    authority
        .view(&permission_request_id)
        .map(|view| serde_json::to_value(view).unwrap_or(Value::Null))
        .map_err(|error| error.code.to_owned())
}

/// B-side read-only view: no Allow/Approve control exists.
#[tauri::command]
pub fn desktop_remote_permission_list(
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
    Ok(authority.deny_all_for_task(&task_id, &reason, monotonic_millis()))
}

/// Narrow ACP adapter: the remote permission authority is the only responder
/// for a remote-sourced pending permission. A local/Web respond command must
/// never decide a remote request.
pub(crate) async fn respond_remote_permission_via_acp(
    acp_pool: &AcpPool,
    session_id: &str,
    tool_call_id: &str,
    option_id: &str,
) -> Result<(), String> {
    acp_pool
        .respond_permission(session_id, tool_call_id, option_id)
        .await
        .map_err(|_| "permission_dispatch_failed".to_owned())
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
        for authority_kind in [DecisionAuthority::ControlledB, DecisionAuthority::LocalOperator] {
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
