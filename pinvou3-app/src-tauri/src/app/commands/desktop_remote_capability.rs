//! A-only, allowlisted capability read and authenticated request send.

use super::desktop_remote_state::DesktopRemoteState as State;
use serde::Serialize;

use crate::features::remote_control::controller::controlling::ControllingDeviceManager;
use crate::features::remote_control::controller::controlling_session::{
    ControllerCapabilityProjection, ControllingIntentProjection, ControllingSessionManager,
};
use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
use crate::features::remote_control::controller::relay_transport::ControllerSendError;

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CapabilitySendAck {
    request_id: String,
    session_id: String,
}

impl From<ControllingIntentProjection> for CapabilitySendAck {
    fn from(intent: ControllingIntentProjection) -> Self {
        Self {
            request_id: intent.request_id,
            session_id: intent.session_id,
        }
    }
}

fn ensure_supported(manager: &ControllingDeviceManager) -> Result<(), String> {
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
    manager
        .is_supported()
        .then_some(())
        .ok_or_else(|| "desktop_remote_control_unsupported".to_owned())
}

fn send_error_code(error: ControllerSendError) -> String {
    match error {
        ControllerSendError::Unauthenticated => "controller_not_connected",
        ControllerSendError::FrameTooLarge { .. }
        | ControllerSendError::InvalidFrame(_)
        | ControllerSendError::Serialization => "capability_snapshot_unavailable",
        ControllerSendError::ByteBudgetExhausted
        | ControllerSendError::QueueFull(_)
        | ControllerSendError::GenerationAlreadyBound
        | ControllerSendError::Closed => "controller_lane_unavailable",
    }
    .to_owned()
}

#[tauri::command]
pub fn desktop_remote_controller_capability_snapshot(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    remote_ref: String,
) -> Result<ControllerCapabilityProjection, String> {
    ensure_supported(&manager)?;
    let (lease, _) = manager
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    let projection = sessions
        .capability_projection(&remote_ref, &lease)
        .map_err(|error| error.code.to_owned())?;
    let (still_live, _) = manager
        .live_runtime_context()
        .map_err(|error| error.code.to_owned())?;
    if still_live != lease {
        return Err("session_context_stale".into());
    }
    Ok(projection)
}

#[tauri::command]
pub fn desktop_remote_controller_capability_send(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    business: State<'_, ControllingLaneState>,
    remote_ref: String,
) -> Result<CapabilitySendAck, String> {
    ensure_supported(&manager)?;
    // Capture the operation before the lease: any replacement between the
    // reads is rejected by the final owner guard inside synchronous enqueue.
    let operation = manager
        .current_operation()
        .ok_or_else(|| "controller_not_connected".to_owned())?;
    let expected = manager
        .authenticated_remote_proxy_identity()
        .map_err(|error| error.code.to_owned())?;
    let intent = sessions
        .capability_request_with_send(&remote_ref, &expected.0, |payload| {
            manager.send_capability_for_owner(
                &operation,
                &expected,
                &business,
                payload,
                send_error_code,
            )
        })
        .map_err(|error| error.into_code())?;
    Ok(intent.into())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn capability_send_errors_do_not_report_task_input_failure() {
        for error in [
            ControllerSendError::FrameTooLarge { bytes: 2, limit: 1 },
            ControllerSendError::InvalidFrame("test".into()),
            ControllerSendError::Serialization,
        ] {
            assert_eq!(send_error_code(error), "capability_snapshot_unavailable");
        }
        assert_eq!(
            send_error_code(ControllerSendError::Unauthenticated),
            "controller_not_connected"
        );
        assert_eq!(
            send_error_code(ControllerSendError::ByteBudgetExhausted),
            "controller_lane_unavailable"
        );
    }

    #[test]
    fn a_capability_commands_have_one_handler_each() {
        let root = include_str!("../../lib.rs");
        for command in [
            "commands::desktop_remote_capability::desktop_remote_controller_capability_snapshot,",
            "commands::desktop_remote_capability::desktop_remote_controller_capability_send,",
        ] {
            assert_eq!(root.matches(command).count(), 1);
        }
    }

    #[test]
    fn send_ack_never_exposes_wire_payload() {
        let intent = ControllingIntentProjection {
            request_id: "request-1".into(),
            session_id: "session-1".into(),
            task_id: None,
            client_request_id: None,
            payload: json!({"secret":"never-export"}),
        };
        let ack = serde_json::to_value(CapabilitySendAck::from(intent)).unwrap();
        assert_eq!(
            ack,
            json!({"requestId":"request-1","sessionId":"session-1"})
        );
        assert!(!ack.to_string().contains("never-export"));
    }
}
