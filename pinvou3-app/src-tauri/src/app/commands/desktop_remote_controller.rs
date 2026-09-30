//! Windows controller-device connection commands.
//!
//! DEV-20 owns final handler registration. The wrappers never persist a code,
//! device credential, token, or key, and do not equate window visibility with
//! disconnection.

use serde::Deserialize;
use tauri::{AppHandle, Emitter, State};

use crate::features::remote_control::controller::controlling::{
    ControllingDeviceManager, ControllingLeaseInfo, ControllingStateError, ControllingStatus,
    RemoteHistoryEntry,
};
use crate::features::remote_control::controller::controlling_session::ControllingSessionManager;
use crate::features::remote_control::controller::controlling_service::{
    CONTROLLER_STATUS_EVENT, connect_controlling_device,
};
use crate::features::remote_control::RemoteControlManager;
use std::sync::Arc;

const STATUS_EVENT: &str = CONTROLLER_STATUS_EVENT;
const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RemoteHistoryCreateRequest {
    title: String,
    b_session_reference: Option<String>,
}

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
    history: State<'_, Arc<super::desktop_remote_history::DesktopRemoteHistoryRuntime>>,
    code: String,
) -> Result<ControllingStatus, String> {
    ensure_supported(&manager)?;
    // 新连接不得复用上一代租赁的会话意图（它们的 lease 绑定必然失效）。
    sessions.clear();
    let operation = manager.connect(&code).map_err(controlling_error)?;
    let status = manager.status();
    emit_status(&app, &status);
    let journal: Arc<dyn crate::features::remote_control::controller::controlling_service::ControllingJournalSink> =
        Arc::new(
            super::desktop_remote_history::DesktopControllerJournalSink::new(
                app.clone(),
                history.inner().clone(),
            ),
        );
    connect_controlling_device(&app, &manager, relay.inner(), operation, journal).await
}

#[tauri::command]
pub fn desktop_remote_controller_disconnect(
    app: AppHandle,
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
) -> Result<ControllingStatus, String> {
    ensure_supported(&manager)?;
    sessions.clear();
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

#[tauri::command]
pub fn desktop_remote_controller_live_context(
    manager: State<'_, ControllingDeviceManager>,
) -> Result<ControllingLeaseInfo, String> {
    ensure_supported(&manager)?;
    manager
        .live_lease()
        .map(ControllingLeaseInfo::from)
        .map_err(controlling_error)
}

#[tauri::command]
pub fn desktop_remote_controller_create_history(
    manager: State<'_, ControllingDeviceManager>,
    request: RemoteHistoryCreateRequest,
) -> Result<RemoteHistoryEntry, String> {
    ensure_supported(&manager)?;
    manager
        .create_history(&request.title, request.b_session_reference.as_deref())
        .map_err(controlling_error)
}

#[tauri::command]
pub fn desktop_remote_controller_list_history(
    manager: State<'_, ControllingDeviceManager>,
) -> Result<Vec<RemoteHistoryEntry>, String> {
    ensure_supported(&manager)?;
    Ok(manager.history())
}

#[cfg(test)]
mod tests {
    #[test]
    fn controller_command_surface_is_stable_and_contains_no_code_storage() {
        assert_eq!(super::STATUS_EVENT, "desktop-remote-controller-status");
        assert_eq!(super::UNSUPPORTED, "desktop_remote_control_unsupported");
    }
}
