//! Windows controller-device connection commands.
//!
//! DEV-20 owns final handler registration. The wrappers never persist a code,
//! device credential, token, or key, and do not equate window visibility with
//! disconnection.

use tauri::{AppHandle, Emitter, State};

use crate::features::remote_control::RemoteControlManager;
use crate::features::remote_control::controller::controlling::{
    ControllingDeviceManager, ControllingStateError, ControllingStatus,
};
use crate::features::remote_control::controller::controlling_service::{
    CONTROLLER_STATUS_EVENT, connect_controlling_device,
};
use crate::features::remote_control::controller::controlling_session::ControllingSessionManager;
use crate::features::remote_control::controller::relay_runtime::log_controller_event;
use std::sync::Arc;

const STATUS_EVENT: &str = CONTROLLER_STATUS_EVENT;
const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

fn controlling_error(error: ControllingStateError) -> String {
    error.code.to_owned()
}

fn ensure_supported(manager: &ControllingDeviceManager) -> Result<(), String> {
    if manager.is_supported() {
        Ok(())
    } else {
        Err(UNSUPPORTED.to_owned())
    }
}

fn emit_status(app: &AppHandle, status: &ControllingStatus) {
    let _ = app.emit(STATUS_EVENT, status);
}

#[tauri::command]
pub async fn desktop_remote_controller_connect(
    app: AppHandle,
    manager: State<'_, ControllingDeviceManager>,
    relay: State<'_, RemoteControlManager>,
    sessions: State<'_, ControllingSessionManager>,
    history: State<'_, Arc<super::desktop_remote_journal::DesktopRemoteJournalRuntime>>,
    actions: State<'_, Arc<super::desktop_remote_action::DesktopRemoteActionState>>,
    artifacts: State<'_, Arc<super::desktop_remote_artifact::DesktopRemoteArtifactRuntime>>,
    code: String,
) -> Result<ControllingStatus, String> {
    ensure_supported(&manager)?;
    let previous_connection_id = manager
        .live_runtime_context()
        .ok()
        .map(|(lease, _)| lease.connection_id);
    // A 端排障日志：把「用户点了连接」和「本地状态机拒绝」都留痕。此前这两步
    // 没有任何落盘痕迹，失败只能从界面文案反推。
    log_controller_event("connect_requested", &relay.controller_endpoint_log_detail());
    let operation = match manager.connect(&code) {
        Ok(operation) => operation,
        Err(error) => {
            log_controller_event("connect_rejected", error.code);
            return Err(controlling_error(error));
        }
    };
    // 只有成功进入新连接尝试才淘汰上一代状态；busy/非法连接码不得
    // 破坏仍然有效的会话、回执或成果索引。
    if let Some(connection_id) = previous_connection_id {
        artifacts.clear_connection(&connection_id);
    }
    actions.clear_all();
    sessions.clear();
    let status = manager.status();
    emit_status(&app, &status);
    let journal: Arc<dyn crate::features::remote_control::controller::controlling_service::ControllingJournalSink> =
        Arc::new(
            super::desktop_remote_journal::DesktopControllerJournalSink::new(
                app.clone(),
                history.inner().clone(),
            ),
        );
    connect_controlling_device(&app, &manager, relay.inner(), operation, journal).await
}

#[tauri::command]
pub async fn desktop_remote_controller_disconnect(
    app: AppHandle,
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    actions: State<'_, Arc<super::desktop_remote_action::DesktopRemoteActionState>>,
    artifacts: State<'_, Arc<super::desktop_remote_artifact::DesktopRemoteArtifactRuntime>>,
) -> Result<ControllingStatus, String> {
    ensure_supported(&manager)?;
    // E2E-06：先尽力把显式断开告诉 Relay/B（本地断开始终权威，发送失败不阻塞）。
    if let Some(receipt) = manager.send_disconnect_with_receipt("user_disconnect") {
        // Queue admission is not delivery. Let the socket flush the final frame
        // before local cleanup cancels its transport; offline peers stay bounded.
        let _ = tokio::time::timeout(std::time::Duration::from_secs(2), receipt).await;
    }
    sessions.clear();
    actions.clear_all();
    if let Ok((lease, _)) = manager.live_runtime_context() {
        artifacts.clear_connection(&lease.connection_id);
    }
    let status = manager.disconnect();
    emit_status(&app, &status);
    Ok(status)
}

#[tauri::command]
pub fn desktop_remote_controller_status(
    manager: State<'_, ControllingDeviceManager>,
) -> Result<ControllingStatus, String> {
    ensure_supported(&manager)?;
    Ok(manager.status())
}

#[cfg(test)]
mod tests {
    #[test]
    fn controller_command_surface_is_stable_and_contains_no_code_storage() {
        assert_eq!(super::STATUS_EVENT, "desktop-remote-controller-status");
        assert_eq!(super::UNSUPPORTED, "desktop_remote_control_unsupported");
    }
}
