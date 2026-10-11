//! Internal ACP responder port; not a renderer approval command.
//! A08 must bind the authenticated A decision to the exact pending ACP
//! session/tool call/option before invoking this port. A07 has no such ingress.

use crate::features::codex_acp::AcpPool;

/// Private responder port, reserved for the authenticated remote dispatcher.
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
