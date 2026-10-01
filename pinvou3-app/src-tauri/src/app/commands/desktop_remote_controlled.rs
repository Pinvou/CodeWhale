//! Windows controlled-device lifecycle commands.
//!
//! DEV-20 owns final handler registration. These wrappers keep the B UI on
//! the feature state machine and emit only its allowlisted, generation-bearing
//! projection; no pairing, lease, token, key, or host-path value is persisted.

use std::sync::OnceLock;
use std::time::Instant;

use serde::Deserialize;
use tauri::{AppHandle, Emitter, State};

use crate::features::files::file_ingest::validate_browsable_path;
use crate::features::remote_control::controller::controlled::{
    ControlledDeviceManager, ControlledStateError, ControlledStatus, ControlledWorkspaceInfo,
    WorkspaceAuthorizationRequest,
};

const STATUS_EVENT: &str = "desktop-remote-controlled-status";
const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DesktopControlledWorkspaceInput {
    path: String,
    display_name: String,
    code_allowed: bool,
}

fn controlled_error(error: ControlledStateError) -> String {
    error.code.to_owned()
}

fn ensure_supported(manager: &ControlledDeviceManager) -> Result<(), String> {
    if manager.is_supported() {
        Ok(())
    } else {
        Err(UNSUPPORTED.to_owned())
    }
}

fn monotonic_millis() -> u64 {
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

fn workspace_request(
    input: DesktopControlledWorkspaceInput,
) -> Result<WorkspaceAuthorizationRequest, String> {
    let path =
        validate_browsable_path(&input.path).map_err(|_| "workspace_path_invalid".to_owned())?;
    if !path.is_dir() {
        return Err("workspace_path_invalid".to_owned());
    }
    Ok(WorkspaceAuthorizationRequest {
        path,
        display_name: input.display_name,
        code_allowed: input.code_allowed,
    })
}

fn workspace_requests(
    inputs: Vec<DesktopControlledWorkspaceInput>,
) -> Result<Vec<WorkspaceAuthorizationRequest>, String> {
    inputs.into_iter().map(workspace_request).collect()
}

fn emit_status(app: &AppHandle, status: &ControlledStatus) {
    let _ = app.emit(STATUS_EVENT, status);
}

#[tauri::command]
pub fn desktop_remote_controlled_start(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    device_name: Option<String>,
    workspaces: Vec<DesktopControlledWorkspaceInput>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    let workspaces = workspace_requests(workspaces)?;
    manager
        .start(device_name.as_deref(), workspaces)
        .map_err(controlled_error)?;
    let status = manager.status();
    emit_status(&app, &status);
    Ok(status)
}

#[tauri::command]
pub fn desktop_remote_controlled_refresh(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    manager.begin_refresh().map_err(controlled_error)?;
    let status = manager.status();
    emit_status(&app, &status);
    Ok(status)
}

#[tauri::command]
pub fn desktop_remote_controlled_stop(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    let status = manager.stop();
    emit_status(&app, &status);
    Ok(status)
}

#[tauri::command]
pub fn desktop_remote_controlled_disconnect_now(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    let status = manager.disconnect_now();
    emit_status(&app, &status);
    Ok(status)
}

#[tauri::command]
pub fn desktop_remote_controlled_status(
    manager: State<'_, ControlledDeviceManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    Ok(manager.status())
}

#[tauri::command]
pub fn desktop_remote_controlled_poll_watchdog(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    let status = manager.poll_watchdog(monotonic_millis());
    emit_status(&app, &status);
    Ok(status)
}

#[tauri::command]
pub fn desktop_remote_controlled_authorize_workspace(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    workspace: DesktopControlledWorkspaceInput,
) -> Result<ControlledWorkspaceInfo, String> {
    ensure_supported(&manager)?;
    let request = workspace_request(workspace)?;
    let info = manager
        .authorize_workspace(request)
        .map_err(controlled_error)?;
    emit_status(&app, &manager.status());
    Ok(info)
}

#[tauri::command]
pub fn desktop_remote_controlled_list_workspaces(
    manager: State<'_, ControlledDeviceManager>,
) -> Result<Vec<ControlledWorkspaceInfo>, String> {
    ensure_supported(&manager)?;
    Ok(manager.workspaces())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn input(path: &std::path::Path) -> DesktopControlledWorkspaceInput {
        DesktopControlledWorkspaceInput {
            path: path.to_string_lossy().into_owned(),
            display_name: "Workspace".into(),
            code_allowed: true,
        }
    }

    #[test]
    fn workspace_command_accepts_only_a_real_browsable_directory() {
        // Exercise the production HOME policy without changing process-wide
        // environment or weakening validation on Linux/macOS.
        let directory = tempfile::Builder::new()
            .prefix(".pinvou3-controlled-workspace-test-")
            .tempdir_in(crate::platform::os::user_home_dir())
            .unwrap();
        let request = workspace_request(input(directory.path())).unwrap();
        assert_eq!(
            request.path.file_name(),
            directory.path().file_name(),
            "authorization must retain the selected directory identity"
        );
        assert!(request.code_allowed);

        let missing = directory.path().join("missing");
        let error = match workspace_request(input(&missing)) {
            Ok(_) => panic!("missing workspace path must fail closed"),
            Err(error) => error,
        };
        assert_eq!(error, "workspace_path_invalid");
    }

    #[test]
    fn command_surface_has_stable_events_and_monotonic_clock() {
        assert_eq!(STATUS_EVENT, "desktop-remote-controlled-status");
        assert_eq!(UNSUPPORTED, "desktop_remote_control_unsupported");
        assert!(monotonic_millis() <= monotonic_millis());
    }
}
