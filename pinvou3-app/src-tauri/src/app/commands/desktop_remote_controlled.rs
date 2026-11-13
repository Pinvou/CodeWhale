//! Windows controlled-device lifecycle commands.
//!
//! DEV-20 owns final handler registration. These wrappers keep the B UI on
//! the feature state machine and emit only its allowlisted, generation-bearing
//! projection; no pairing, lease, token, key, or host-path value is persisted.

use super::desktop_remote_state::DesktopRemoteState as State;
use serde::Deserialize;
use std::sync::Arc;
use tauri::{AppHandle, Emitter};

use crate::features::behavior_telemetry::{RemoteControlEvent, RemoteStage, track_remote_control};
use crate::features::files::file_ingest::validate_browsable_path;
use crate::features::remote_control::RemoteControlManager;
#[cfg(test)]
pub(crate) use crate::features::remote_control::controller::clock::monotonic_millis;
use crate::features::remote_control::controller::controlled::{
    ControlledDeviceManager, ControlledOperation, ControlledStateError, ControlledStatus,
    ControlledWorkspaceInfo, WorkspaceAuthorizationRequest,
};
use crate::features::remote_control::controller::relay_runtime::{
    ControlledRelayWorker, controlled_monotonic_millis, log_controlled_event,
};

const STATUS_EVENT: &str = "desktop-remote-controlled-status";
const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
/// DEV-10 审核修复：B 本地 watchdog 必须有真实调用方。该间隔决定本地终止的
/// 检测粒度（预算本身由 DEV-08 的 30 秒单调时钟 deadline 决定）。
const WATCHDOG_POLL_INTERVAL_MS: u64 = 1_000;

/// 周期推进 B 端本地 watchdog。Tauri managed state 由 app 持有，这里只借用，
/// 因此不改变 `ControlledDeviceManager` 的所有权与 Drop 语义。
pub(crate) fn spawn_watchdog_reaper(app: AppHandle) {
    crate::features::remote_control::controller::relay_runtime::log_controlled_event(
        "lifecycle_checkpoint",
        "runtime_started",
    );
    tauri::async_runtime::spawn(async move {
        let mut last_projection = String::new();
        loop {
            tokio::time::sleep(std::time::Duration::from_millis(WATCHDOG_POLL_INTERVAL_MS)).await;
            let status = {
                use tauri::Manager as _;
                let manager = app.state::<ControlledDeviceManager>();
                let connection_id = manager
                    .live_runtime_context()
                    .ok()
                    .map(|(lease, _)| lease.connection_id);
                let Ok(status) = manager.poll_watchdog(controlled_monotonic_millis()) else {
                    crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                        "lifecycle_checkpoint", "watchdog_poll_failed",
                    );
                    continue;
                };
                if manager.live_runtime_context().is_err() {
                    if let Some(connection_id) = connection_id {
                        app.state::<Arc<super::desktop_remote_attachment::DesktopRemoteAttachmentRuntime>>()
                            .uploads_cleanup_for_connection(&connection_id);
                        app.state::<Arc<super::desktop_remote_artifact::DesktopRemoteArtifactRuntime>>()
                            .clear_connection(&connection_id);
                    }
                }
                status
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
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
    manager
        .is_supported()
        .then_some(())
        .ok_or_else(|| UNSUPPORTED.to_owned())
}

fn workspace_request(
    input: DesktopControlledWorkspaceInput,
) -> Result<WorkspaceAuthorizationRequest, String> {
    let path =
        validate_browsable_path(&input.path).map_err(|_| "workspace_path_invalid".to_owned())?;
    if !path.is_dir() {
        return Err("workspace_path_invalid".to_owned());
    }
    WorkspaceAuthorizationRequest::new(path, input.display_name, input.code_allowed)
        .map_err(controlled_error)
}

fn workspace_requests(
    inputs: Vec<DesktopControlledWorkspaceInput>,
) -> Result<Vec<WorkspaceAuthorizationRequest>, String> {
    inputs.into_iter().map(workspace_request).collect()
}

fn emit_status(app: &AppHandle, status: &ControlledStatus) {
    crate::features::remote_control::controller::relay_runtime::track_controlled_status_transition(
        app, status,
    );
    let _ = app.emit(STATUS_EVENT, status);
}

/// Start and refresh share the same operation-owned failure boundary. A worker
/// can fail before it is spawned (configuration, ownership or key setup), so
/// its asynchronous registration failure handler cannot close this state.
async fn start_controlled_worker<T>(
    manager: &ControlledDeviceManager,
    operation: ControlledOperation,
    start: impl std::future::Future<Output = Result<T, String>>,
    emit: impl FnOnce(&ControlledStatus),
) -> Result<T, String> {
    match start.await {
        Ok(done) => Ok(done),
        Err(error) => {
            let code =
                crate::features::remote_control::controller::protocol::CONTROLLER_ERROR_CODES
                    .iter()
                    .copied()
                    .find(|code| *code == error)
                    .unwrap_or("pairing_registration_failed");
            manager
                .fail_operation_with_projection(operation, code.to_owned(), emit)
                .map_err(controlled_error)?;
            log_controlled_event("start_failed", code);
            Err(code.to_owned())
        }
    }
}

/// A registration task can disappear without sending its normal result. Retire
/// only the operation captured by this command, including its relay lane; a
/// concurrent stop or replacement must retain its own state and projection.
pub(crate) async fn wait_controlled_registration(
    manager: &ControlledDeviceManager,
    operation: ControlledOperation,
    done: impl std::future::Future<
        Output = Result<Result<ControlledStatus, String>, tokio::sync::oneshot::error::RecvError>,
    >,
    retire: impl FnOnce(&str),
    emit: impl FnOnce(&ControlledStatus),
) -> Result<ControlledStatus, String> {
    match done.await {
        Ok(result) => result,
        Err(_) => {
            const ERROR: &str = "pairing_registration_failed";
            retire(operation.generation());
            let _ = manager.fail_operation_with_projection(operation, ERROR.to_owned(), emit);
            log_controlled_event("registration_failed", ERROR);
            Err(ERROR.to_owned())
        }
    }
}

/// Frontend projection diagnostics accept only semantic categories. The UI
/// cannot submit a code, generation, endpoint, device name or free text.
#[derive(Debug, Clone, Copy, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ClientRemoteEvent {
    ControlledStatus,
    ControlledResult,
    CapabilityHandshake,
}

impl ClientRemoteEvent {
    fn as_str(&self) -> &'static str {
        match self {
            Self::ControlledStatus => "controlled_status",
            Self::ControlledResult => "controlled_result",
            Self::CapabilityHandshake => "capability_handshake",
        }
    }
}

#[derive(Debug, Clone, Copy, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ClientRemoteDetail {
    Registering,
    WaitingForPairing,
    PairingReserved,
    HandshakePending,
    Connected,
    Reconnecting,
    Stopped,
    Error,
    Failed,
    Unknown,
}

impl ClientRemoteDetail {
    fn as_str(&self) -> &'static str {
        match self {
            Self::Registering => "registering",
            Self::WaitingForPairing => "waiting_for_pairing",
            Self::PairingReserved => "pairing_reserved",
            Self::HandshakePending => "handshake_pending",
            Self::Connected => "connected",
            Self::Reconnecting => "reconnecting",
            Self::Stopped => "stopped",
            Self::Error => "error",
            Self::Failed => "failed",
            Self::Unknown => "unknown",
        }
    }
}

fn validate_client_remote_event(
    event: ClientRemoteEvent,
    detail: ClientRemoteDetail,
) -> Result<(), String> {
    if matches!(event, ClientRemoteEvent::CapabilityHandshake)
        && !matches!(detail, ClientRemoteDetail::Failed)
    {
        return Err("remote_client_event_invalid".into());
    }
    Ok(())
}

#[tauri::command]
pub fn desktop_remote_client_event(
    event: ClientRemoteEvent,
    detail: ClientRemoteDetail,
) -> Result<(), String> {
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
    validate_client_remote_event(event, detail)?;
    super::super::super::features::remote_control::controller::relay_runtime::log_controlled_event(
        event.as_str(),
        detail.as_str(),
    );
    Ok(())
}

#[tauri::command]
pub async fn desktop_remote_controlled_permissions(
    manager: State<'_, ControlledDeviceManager>,
    policy: Option<crate::platform::prefs::RemoteTaskPermissionPrefs>,
) -> Result<crate::platform::prefs::RemoteTaskPermissionPrefs, String> {
    ensure_supported(&manager)?;
    tokio::task::spawn_blocking(move || match policy {
        Some(policy) => crate::features::remote_control::task_permissions::save(policy),
        None => Ok(crate::platform::prefs::UserPrefs::load().remote_task_permissions),
    })
    .await
    .map_err(|_| "remote_task_permissions_save_failed".to_owned())?
}

#[tauri::command]
pub async fn desktop_remote_controlled_start(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    relay: State<'_, RemoteControlManager>,
    device_name: Option<String>,
    workspaces: Vec<DesktopControlledWorkspaceInput>,
) -> Result<ControlledStatus, String> {
    crate::features::remote_control::controller::relay_runtime::log_controlled_event(
        "lifecycle_checkpoint",
        "start_requested",
    );
    let started = std::time::Instant::now();
    let result = async {
        ensure_supported(&manager)?;
        let workspaces = workspace_requests(workspaces)?;
        tokio::task::spawn_blocking(
            crate::features::remote_control::task_permissions::enable_defaults,
        )
        .await
        .map_err(|_| "remote_task_permissions_save_failed".to_owned())??;
        // Retain the exact operation returned under the start lock. Looking up
        // "current" again can substitute a concurrent stop/restart's new owner.
        let operation = match manager.start(device_name.as_deref(), workspaces) {
            Ok(operation) => operation,
            Err(error) => {
                log_controlled_event("start_rejected", error.code);
                return Err(controlled_error(error));
            }
        };
        let done = start_controlled_worker(
            &manager,
            operation.clone(),
            ControlledRelayWorker::start(
                app.clone(),
                relay.inner().clone(),
                operation.clone(),
                std::sync::Arc::new(
                    super::desktop_remote_controller_business::DesktopControlledBusiness {
                        app: app.clone(),
                    },
                ),
            ),
            |status| emit_status(&app, status),
        )
        .await?;
        match wait_controlled_registration(
            &manager,
            operation,
            done.wait(),
            |generation| relay.stop_controller_relay(generation),
            |status| emit_status(&app, status),
        )
        .await
        {
            Ok(status) => Ok(status),
            Err(error) => {
                log_controlled_event("start_failed", &error);
                Err(error)
            }
        }
    }
    .await;
    track_remote_control(
        &app,
        RemoteControlEvent::from_result(
            RemoteStage::Start,
            &result,
            started.elapsed().as_millis() as u64,
        )
        .role(true),
    );
    result
}

#[tauri::command]
pub async fn desktop_remote_controlled_refresh(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    relay: State<'_, RemoteControlManager>,
    attachments: State<'_, Arc<super::desktop_remote_attachment::DesktopRemoteAttachmentRuntime>>,
    artifacts: State<'_, Arc<super::desktop_remote_artifact::DesktopRemoteArtifactRuntime>>,
) -> Result<ControlledStatus, String> {
    let checkpoint = |detail| {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "lifecycle_checkpoint",
            detail,
        );
    };
    checkpoint("refresh_requested");
    let started = std::time::Instant::now();
    let result = async {
        ensure_supported(&manager)?;
        let old_generation = manager.current_generation();
        let old_connection_id = manager
            .live_runtime_context()
            .ok()
            .map(|(lease, _)| lease.connection_id);
        let refresh = manager.begin_refresh().map_err(controlled_error)?;
        checkpoint("refresh_retiring");
        if let Some(connection_id) = old_connection_id {
            checkpoint("refresh_connection_cleanup_begin");
            attachments.uploads_cleanup_for_connection(&connection_id);
            artifacts.clear_connection(&connection_id);
            checkpoint("refresh_connection_cleanup_done");
        }
        emit_status(&app, &manager.status());
        if let Some(generation) = old_generation {
            checkpoint("refresh_relay_stop_begin");
            relay.stop_controller_relay(&generation);
            checkpoint("refresh_relay_stop_done");
        }
        checkpoint("refresh_state_rotation_begin");
        let operation = manager
            .complete_refresh_revoked_with_projection(refresh, |status| emit_status(&app, status))
            .map_err(controlled_error)?;
        checkpoint("refresh_state_rotation_done");
        checkpoint("refresh_worker_start_begin");
        let done = start_controlled_worker(
            &manager,
            operation.clone(),
            ControlledRelayWorker::start(
                app.clone(),
                relay.inner().clone(),
                operation.clone(),
                std::sync::Arc::new(
                    super::desktop_remote_controller_business::DesktopControlledBusiness {
                        app: app.clone(),
                    },
                ),
            ),
            |status| emit_status(&app, status),
        )
        .await?;
        checkpoint("refresh_worker_start_done");
        checkpoint("refresh_registration_wait_begin");
        match wait_controlled_registration(
            &manager,
            operation,
            done.wait(),
            |generation| relay.stop_controller_relay(generation),
            |status| emit_status(&app, status),
        )
        .await
        {
            Ok(status) => {
                checkpoint("refresh_registration_wait_done");
                emit_status(&app, &status);
                Ok(status)
            }
            Err(error) => {
                checkpoint("refresh_registration_wait_failed");
                Err(error)
            }
        }
    }
    .await;
    checkpoint(if result.is_ok() {
        "refresh_completed"
    } else {
        "refresh_failed"
    });
    track_remote_control(
        &app,
        RemoteControlEvent::from_result(
            RemoteStage::Refresh,
            &result,
            started.elapsed().as_millis() as u64,
        )
        .role(true),
    );
    result
}

#[tauri::command]
pub fn desktop_remote_controlled_stop(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    relay: State<'_, RemoteControlManager>,
    attachments: State<'_, Arc<super::desktop_remote_attachment::DesktopRemoteAttachmentRuntime>>,
    artifacts: State<'_, Arc<super::desktop_remote_artifact::DesktopRemoteArtifactRuntime>>,
) -> Result<ControlledStatus, String> {
    crate::features::remote_control::controller::relay_runtime::log_controlled_event(
        "lifecycle_checkpoint",
        "stop_requested",
    );
    let started = std::time::Instant::now();
    let result = (|| {
        ensure_supported(&manager)?;
        let generation = manager.current_generation();
        let connection_id = manager
            .live_runtime_context()
            .ok()
            .map(|(lease, _)| lease.connection_id);
        let stopping = manager.begin_stop();
        emit_status(&app, &stopping);
        if let Some(generation) = generation {
            relay.stop_controller_relay(&generation);
        }
        let stopped = manager.finish_stop();
        if let Some(connection_id) = connection_id {
            attachments.uploads_cleanup_for_connection(&connection_id);
            artifacts.clear_connection(&connection_id);
        }
        emit_status(&app, &stopped);
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "lifecycle_checkpoint",
            "stop_completed",
        );
        Ok(stopped)
    })();
    track_remote_control(
        &app,
        RemoteControlEvent::from_result(
            RemoteStage::Stop,
            &result,
            started.elapsed().as_millis() as u64,
        )
        .role(true),
    );
    result
}

#[tauri::command]
pub fn desktop_remote_controlled_disconnect_now(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
    relay: State<'_, RemoteControlManager>,
    attachments: State<'_, Arc<super::desktop_remote_attachment::DesktopRemoteAttachmentRuntime>>,
    artifacts: State<'_, Arc<super::desktop_remote_artifact::DesktopRemoteArtifactRuntime>>,
) -> Result<ControlledStatus, String> {
    crate::features::remote_control::controller::relay_runtime::log_controlled_event(
        "lifecycle_checkpoint",
        "disconnect_requested",
    );
    ensure_supported(&manager)?;
    let connection_id = manager
        .live_runtime_context()
        .ok()
        .map(|(lease, _)| lease.connection_id);
    if let Some(generation) = manager.current_generation() {
        relay.stop_controller_relay(&generation);
    }
    let status = manager.disconnect_now();
    if let Some(connection_id) = connection_id {
        attachments.uploads_cleanup_for_connection(&connection_id);
        artifacts.clear_connection(&connection_id);
    }
    emit_status(&app, &status);
    crate::features::remote_control::controller::relay_runtime::log_controlled_event(
        "lifecycle_checkpoint",
        "disconnect_completed",
    );
    Ok(status)
}

#[tauri::command]
pub fn desktop_remote_controlled_status(
    manager: State<'_, ControlledDeviceManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    manager
        .poll_watchdog(controlled_monotonic_millis())
        .map_err(|error| error.code.to_owned())
}

#[tauri::command]
pub fn desktop_remote_controlled_poll_watchdog(
    app: AppHandle,
    manager: State<'_, ControlledDeviceManager>,
) -> Result<ControlledStatus, String> {
    ensure_supported(&manager)?;
    let status = manager
        .poll_watchdog(controlled_monotonic_millis())
        .map_err(|error| error.code.to_owned())?;
    emit_status(&app, &status);
    Ok(status)
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

    #[tokio::test]
    async fn closed_registration_retires_start_and_refresh_and_allows_next_start() {
        use crate::features::remote_control::controller::controlled::ControlledStatusKind;
        for refresh in [false, true] {
            for while_waiting in [false, true] {
                let manager = ControlledDeviceManager::default();
                let initial = manager.start(Some("B"), Vec::new()).unwrap();
                let operation = if refresh {
                    let retiring = manager.begin_refresh().unwrap();
                    manager.complete_refresh_revoked(retiring).unwrap()
                } else {
                    initial
                };
                let (sender, done) = tokio::sync::oneshot::channel();
                let mut retired = None;
                let mut emitted = None;
                {
                    let wait = wait_controlled_registration(
                        &manager,
                        operation.clone(),
                        done,
                        |generation| retired = Some(generation.to_owned()),
                        |status| emitted = Some(status.clone()),
                    );
                    tokio::pin!(wait);
                    if while_waiting {
                        // Establish an actual pending receiver without sleeping.
                        std::future::poll_fn(|cx| {
                            assert!(std::future::Future::poll(wait.as_mut(), cx).is_pending());
                            std::task::Poll::Ready(())
                        })
                        .await;
                    }
                    drop(sender);
                    assert_eq!(wait.await.unwrap_err(), "pairing_registration_failed");
                }
                assert_eq!(retired.as_deref(), Some(operation.generation()));
                let status = emitted.unwrap();
                assert_eq!(status.kind(), ControlledStatusKind::Error);
                assert_eq!(
                    serde_json::to_value(status).unwrap()["generation"],
                    operation.generation()
                );
                assert!(!manager.is_pending_handshake(operation));
                assert!(manager.start(Some("next"), Vec::new()).is_ok());
            }
        }
    }

    #[tokio::test]
    async fn closed_old_registration_preserves_stop_and_replacement() {
        use crate::features::remote_control::controller::controlled::ControlledStatusKind;
        for replacement in [false, true] {
            let manager = ControlledDeviceManager::default();
            let old = manager.start(None, Vec::new()).unwrap();
            let (sender, done) = tokio::sync::oneshot::channel();
            manager.stop();
            let next = replacement.then(|| manager.start(None, Vec::new()).unwrap());
            let before = serde_json::to_value(manager.status()).unwrap();
            drop(sender);
            assert_eq!(
                wait_controlled_registration(
                    &manager,
                    old.clone(),
                    done,
                    |generation| assert_eq!(generation, old.generation()),
                    |_| panic!("an obsolete registration must not publish over its successor"),
                )
                .await
                .unwrap_err(),
                "pairing_registration_failed"
            );
            assert_eq!(serde_json::to_value(manager.status()).unwrap(), before);
            assert_eq!(
                manager.status().kind(),
                if replacement {
                    ControlledStatusKind::Registering
                } else {
                    ControlledStatusKind::Stopped
                }
            );
            if let Some(next) = next {
                assert!(manager.is_pending_handshake(next));
            }
        }
    }

    #[tokio::test]
    async fn normal_registration_results_do_not_enter_abnormal_cleanup() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        for result in [Ok(manager.status()), Err("endpoint_offline".to_owned())] {
            let expected = serde_json::to_value(&result).unwrap();
            let (sender, done) = tokio::sync::oneshot::channel();
            sender.send(result).unwrap();
            let actual = wait_controlled_registration(
                &manager,
                operation.clone(),
                done,
                |_| panic!("normal results retain the existing worker lifecycle"),
                |_| panic!("normal results are already projected by their owner"),
            )
            .await;
            assert_eq!(serde_json::to_value(actual).unwrap(), expected);
        }
    }

    #[tokio::test]
    async fn start_entry_retains_its_original_operation_across_a_concurrent_restart() {
        use crate::features::remote_control::controller::controlled::ControlledStatusKind;

        // Bind the real command entry to the state-machine result, not a later
        // ambient lookup. This wiring guard fails on the old two-lock entry.
        let source = include_str!("desktop_remote_controlled.rs");
        let entry = source
            .split("pub async fn desktop_remote_controlled_start(")
            .nth(1)
            .unwrap()
            .split("pub async fn desktop_remote_controlled_refresh(")
            .next()
            .unwrap();
        assert!(entry.contains("let operation = match manager.start("));
        assert!(entry.contains("Ok(operation) => operation"));
        assert!(!entry.contains(".current_operation()"));

        // Exercise the production manager and failure helper at the exact race
        // boundary: an old start pauses before the worker, then B stops/restarts.
        let manager = ControlledDeviceManager::default();
        let original = manager.start(Some("B"), Vec::new()).unwrap();
        manager.stop();
        let current = manager.start(Some("B"), Vec::new()).unwrap();
        assert_ne!(original.generation(), current.generation());
        let before = serde_json::to_value(manager.status()).unwrap();
        let error = start_controlled_worker::<()>(
            &manager,
            original,
            async { Err("pairing_key_invalid".to_owned()) },
            |_| panic!("the old command must not emit over the replacement"),
        )
        .await
        .unwrap_err();
        assert_eq!(error, "desktop_remote_stale_result");
        assert_eq!(manager.status().kind(), ControlledStatusKind::Registering);
        assert_eq!(serde_json::to_value(manager.status()).unwrap(), before);
        assert!(manager.is_pending_handshake(current));
    }

    #[tokio::test]
    async fn worker_start_failure_closes_initial_and_refreshed_operations() {
        use crate::features::remote_control::controller::controlled::ControlledStatusKind;
        for refresh in [false, true] {
            let manager = ControlledDeviceManager::default();
            let initial = manager.start(Some("B"), Vec::new()).unwrap();
            let operation = if refresh {
                let retiring = manager.begin_refresh().unwrap();
                manager.complete_refresh_revoked(retiring).unwrap()
            } else {
                initial
            };
            let mut emitted = None;
            let error = start_controlled_worker::<()>(
                &manager,
                operation.clone(),
                async { Err("configuration contains private host details".to_owned()) },
                |status| emitted = Some(status.clone()),
            )
            .await
            .unwrap_err();
            assert_eq!(error, "pairing_registration_failed");
            let status = emitted.expect("entry failure must publish terminal status");
            assert_eq!(status.kind(), ControlledStatusKind::Error);
            let projection = serde_json::to_value(&status).unwrap();
            assert_eq!(projection["errorCode"], error);
            assert_eq!(projection["generation"], operation.generation());
            assert!(!manager.is_pending_handshake(operation));
            assert!(manager.live_runtime_context().is_err());
        }
    }

    #[tokio::test]
    async fn worker_start_success_preserves_registration_and_returns_receiver() {
        use crate::features::remote_control::controller::controlled::ControlledStatusKind;
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(Some("B"), Vec::new()).unwrap();
        let result = start_controlled_worker(&manager, operation, async { Ok(17) }, |_| {
            panic!("success must not publish a failure")
        })
        .await
        .unwrap();
        assert_eq!(result, 17);
        assert_eq!(manager.status().kind(), ControlledStatusKind::Registering);
    }

    #[tokio::test]
    async fn late_worker_start_failure_cannot_replace_stop_or_new_refresh() {
        use crate::features::remote_control::controller::controlled::ControlledStatusKind;
        for refresh in [false, true] {
            let manager = ControlledDeviceManager::default();
            let operation = manager.start(Some("B"), Vec::new()).unwrap();
            let error = start_controlled_worker::<()>(
                &manager,
                operation,
                async {
                    if refresh {
                        let retiring = manager.begin_refresh().unwrap();
                        manager.complete_refresh_revoked(retiring).unwrap();
                    } else {
                        manager.stop();
                    }
                    Err("endpoint_offline".to_owned())
                },
                |_| panic!("stale operation must not emit over current state"),
            )
            .await
            .unwrap_err();
            assert_eq!(error, "desktop_remote_stale_result");
            assert_eq!(
                manager.status().kind(),
                if refresh {
                    ControlledStatusKind::Registering
                } else {
                    ControlledStatusKind::Stopped
                }
            );
            assert!(serde_json::to_value(manager.status()).unwrap()["errorCode"].is_null());
        }
    }

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
    fn client_diagnostics_reject_free_text_and_identity_fields() {
        assert!(serde_json::from_str::<ClientRemoteEvent>("\"controlled_status\"").is_ok());
        assert!(serde_json::from_str::<ClientRemoteDetail>("\"connected\"").is_ok());
        for value in ["\"generation123\"", "\"AB12CD3\"", "\"deviceAlice\""] {
            assert!(serde_json::from_str::<ClientRemoteEvent>(value).is_err());
            assert!(serde_json::from_str::<ClientRemoteDetail>(value).is_err());
        }
        assert_eq!(
            validate_client_remote_event(
                ClientRemoteEvent::CapabilityHandshake,
                ClientRemoteDetail::Connected
            )
            .unwrap_err(),
            "remote_client_event_invalid"
        );
        assert_eq!(
            desktop_remote_client_event(
                ClientRemoteEvent::CapabilityHandshake,
                ClientRemoteDetail::Connected
            )
            .unwrap_err(),
            if crate::platform::capabilities::ensure_desktop_remote_control_enabled().is_ok() {
                "remote_client_event_invalid"
            } else {
                UNSUPPORTED
            }
        );
    }

    #[test]
    fn command_surface_has_stable_events_and_monotonic_clock() {
        assert_eq!(STATUS_EVENT, "desktop-remote-controlled-status");
        assert_eq!(UNSUPPORTED, "desktop_remote_control_unsupported");
        assert!(controlled_monotonic_millis() <= controlled_monotonic_millis());
    }
}
