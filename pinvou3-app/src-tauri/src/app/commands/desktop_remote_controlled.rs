//! Windows controlled-device lifecycle commands.
//!
//! DEV-20 owns final handler registration. These wrappers keep the B UI on
//! the feature state machine and emit only its allowlisted, generation-bearing
//! projection; no pairing, lease, token, key, or host-path value is persisted.

use serde::Deserialize;
use tauri::{AppHandle, Emitter, State};

use crate::features::files::file_ingest::validate_browsable_path;
use crate::features::remote_control::RemoteControlManager;
use crate::features::remote_control::controller::controlled::{
    ControlledDeviceManager, ControlledStateError, ControlledStatus, ControlledWorkspaceInfo,
    WorkspaceAuthorizationRequest,
};
use crate::features::remote_control::controller::relay_runtime::{
    ControlledRelayWorker, controlled_monotonic_millis,
};

const STATUS_EVENT: &str = "desktop-remote-controlled-status";
const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
/// DEV-10 审核修复：B 本地 watchdog 必须有真实调用方。该间隔决定本地终止的
/// 检测粒度（预算本身由 DEV-08 的 30 秒单调时钟 deadline 决定）。
const WATCHDOG_POLL_INTERVAL_MS: u64 = 1_000;

/// 周期推进 B 端本地 watchdog。Tauri managed state 由 app 持有，这里只借用，
/// 因此不改变 `ControlledDeviceManager` 的所有权与 Drop 语义。
pub(crate) fn spawn_watchdog_reaper(app: AppHandle) {
    tauri::async_runtime::spawn(async move {
        let mut last_projection = String::new();
        loop {
            tokio::time::sleep(std::time::Duration::from_millis(WATCHDOG_POLL_INTERVAL_MS)).await;
            let status = {
                use tauri::Manager as _;
                app.state::<ControlledDeviceManager>()
                    .poll_watchdog(controlled_monotonic_millis())
            };
            // 仅在状态投影变化时广播，避免空闲时每秒刷事件。
            let projection = serde_json::to_string(&status).unwrap_or_default();
            if projection != last_projection {
                last_projection = projection;
                emit_status(&app, &status);
            }
        }
    });
}

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

/// 前端排障日志（仅用于定位「后端已出码、界面无码」这类投影问题）：
/// 把前端持有的被控状态写成一行有界文本，落到与 Rust 相同的日志文件里，
/// 支持人员可直接读取，不需要用户在界面上转述。
#[tauri::command]
pub fn desktop_remote_client_log(message: String) -> Result<(), String> {
    super::super::super::features::remote_control::controller::relay_runtime::log_controlled_event(
        "client", &message,
    );
    Ok(())
}

#[tauri::command]
pub async fn desktop_remote_controlled_start(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    relay: State<'_, RemoteControlManager>,
    device_name: Option<String>,
    workspaces: Vec<DesktopControlledWorkspaceInput>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    let workspaces = workspace_requests(workspaces)?;
    if let Err(error) = manager.start(device_name.as_deref(), workspaces) {
        // 早期拒绝（例如上一代仍在等待配对 → desktop_remote_control_busy）此前完全
        // 没有痕迹，UI 只会显示兜底文案；这里写入排障日志。
        super::super::super::features::remote_control::controller::relay_runtime::log_controlled_event(
            "start_rejected",
            error.code,
        );
        return Err(controlled_error(error));
    }
    let operation = manager
        .current_operation()
        .ok_or_else(|| "desktop_remote_generation_unavailable".to_owned())?;
    let done = match ControlledRelayWorker::start(
        app.clone(),
        relay.inner().clone(),
        operation.clone(),
        std::sync::Arc::new(
            super::desktop_remote_controller_business::DesktopControlledBusiness {
                app: app.clone(),
            },
        ),
    )
    .await
    {
        Ok(done) => done,
        Err(error) => {
            let bounded = error
                .chars()
                .filter(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-'))
                .take(64)
                .collect::<String>();
            super::super::super::features::remote_control::controller::relay_runtime::log_controlled_event(
                "start_failed",
                &bounded,
            );
            let status = manager
                .fail_operation(operation, bounded)
                .map_err(controlled_error)?;
            emit_status(&app, &status);
            return Err(error);
        }
    };
    match done.await {
        Ok(Ok(status)) => Ok(status),
        Ok(Err(error)) => {
            super::super::super::features::remote_control::controller::relay_runtime::log_controlled_event(
                "start_failed",
                &error,
            );
            Err(error)
        }
        Err(_) => {
            super::super::super::features::remote_control::controller::relay_runtime::log_controlled_event(
                "start_failed",
                "pairing_registration_failed",
            );
            Err("pairing_registration_failed".to_owned())
        }
    }
}

#[tauri::command]
pub fn desktop_remote_controlled_refresh(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    relay: State<'_, RemoteControlManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    if let Some(generation) = manager.current_generation() {
        relay.stop_controller_relay(&generation);
    }
    manager.begin_refresh().map_err(controlled_error)?;
    let status = manager.status();
    emit_status(&app, &status);
    Ok(status)
}

#[tauri::command]
pub fn desktop_remote_controlled_stop(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    relay: State<'_, RemoteControlManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    if let Some(generation) = manager.current_generation() {
        relay.stop_controller_relay(&generation);
    }
    let status = manager.stop();
    emit_status(&app, &status);
    Ok(status)
}

#[tauri::command]
pub fn desktop_remote_controlled_disconnect_now(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    relay: State<'_, RemoteControlManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    if let Some(generation) = manager.current_generation() {
        relay.stop_controller_relay(&generation);
    }
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
    let status = manager.poll_watchdog(controlled_monotonic_millis());
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
        assert!(controlled_monotonic_millis() <= controlled_monotonic_millis());
    }
}
