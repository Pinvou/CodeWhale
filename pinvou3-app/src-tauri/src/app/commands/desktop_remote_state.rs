//! Desktop-only IPC readiness and state extraction. Never replace failed
//! recovery with an empty writable ledger/history or affect Legacy Web state.

use tauri::ipc::{CommandArg, CommandItem, InvokeError};
use tauri::{Manager, Runtime, State};

pub(crate) const UNAVAILABLE: &str = "desktop_remote_control_unavailable";

/// Managed by the composition root only after the entire domain boot succeeds.
pub(crate) struct DesktopRemoteReady;

pub(crate) fn is_ready<R: Runtime>(app: &tauri::AppHandle<R>) -> bool {
    app.try_state::<DesktopRemoteReady>().is_some()
}

/// AppHandle-only entry points must check before retrieving domain states.
pub(crate) fn ensure_ready<R: Runtime>(app: &tauri::AppHandle<R>) -> Result<(), String> {
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
    if is_ready(app) {
        Ok(())
    } else {
        Err(UNAVAILABLE.to_owned())
    }
}

pub(crate) struct DesktopRemoteState<'a, T: Send + Sync + 'static>(State<'a, T>);

impl<'a, T: Send + Sync + 'static> DesktopRemoteState<'a, T> {
    pub(crate) fn inner(&self) -> &'a T {
        self.0.inner()
    }
}

impl<T: Send + Sync + 'static> std::ops::Deref for DesktopRemoteState<'_, T> {
    type Target = T;

    fn deref(&self) -> &T {
        self.0.inner()
    }
}

impl<'a, 'de: 'a, T: Send + Sync + 'static, R: Runtime> CommandArg<'de, R>
    for DesktopRemoteState<'a, T>
{
    fn from_command(command: CommandItem<'de, R>) -> Result<Self, InvokeError> {
        if !crate::platform::capabilities::current().desktop_remote_control_supported {
            return Err(InvokeError("desktop_remote_control_unsupported".into()));
        }
        let states = command.message.state_ref();
        if states.try_get::<DesktopRemoteReady>().is_none() {
            return Err(InvokeError(UNAVAILABLE.into()));
        }
        states
            .try_get::<T>()
            .map(Self)
            .ok_or_else(|| InvokeError(UNAVAILABLE.into()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::app::commands;

    #[tauri::command]
    fn framework_missing_state_probe(_state: tauri::State<'_, ()>) -> bool {
        true
    }

    #[test]
    fn upstream_state_command_arg_rejects_without_calling_panicking_get() {
        let app = tauri::test::mock_builder()
            .invoke_handler(tauri::generate_handler![
                commands::desktop_remote_state::tests::framework_missing_state_probe
            ])
            .build(tauri::test::mock_context(tauri::test::noop_assets()))
            .unwrap();
        let window = tauri::WebviewWindowBuilder::new(&app, "main", Default::default())
            .build()
            .unwrap();
        let response = tauri::test::get_ipc_response(
            &window,
            tauri::webview::InvokeRequest {
                cmd: "framework_missing_state_probe".into(),
                callback: tauri::ipc::CallbackFn(0),
                error: tauri::ipc::CallbackFn(1),
                url: window.url().unwrap(),
                body: Default::default(),
                headers: Default::default(),
                invoke_key: tauri::test::INVOKE_KEY.to_owned(),
            },
        );
        assert!(
            response
                .unwrap_err()
                .as_str()
                .unwrap()
                .contains("state not managed")
        );
    }

    #[test]
    fn actual_ipc_rejects_missing_and_partial_boot_without_panicking() {
        let app = tauri::test::mock_builder()
            .invoke_handler(tauri::generate_handler![
                commands::desktop_remote_controlled::desktop_remote_controlled_status,
                commands::desktop_remote_controlled::desktop_remote_controlled_list_workspaces,
                commands::desktop_remote_controller::desktop_remote_controller_status,
                commands::desktop_remote_proxy::desktop_remote_proxy_open_live,
                commands::desktop_remote_proxy::desktop_remote_proxy_history_list,
                commands::desktop_remote_proxy::desktop_remote_proxy_history_read,
                commands::desktop_remote_proxy::desktop_remote_proxy_history_delete,
                commands::desktop_remote_model::desktop_remote_model_list_exportable,
                commands::desktop_remote_permission::desktop_remote_permission_list_b,
                commands::desktop_remote_permission::desktop_remote_permission_deny_task,
                commands::desktop_remote_capability::desktop_remote_controller_capability_snapshot,
                commands::desktop_remote_capability::desktop_remote_controller_capability_send,
                commands::desktop_remote_capabilities::desktop_remote_controlled_capabilities_snapshot,
            ])
            .build(tauri::test::mock_context(tauri::test::noop_assets()))
            .unwrap();
        let window = tauri::WebviewWindowBuilder::new(&app, "main", Default::default())
            .build()
            .unwrap();
        assert!(!is_ready(app.handle()));
        let payload = serde_json::json!({"taskId":"fixture-task", "epoch":"fixture",
            "sequence":0, "remoteSessionRef":"fixture-session", "reason":"fixture",
            "localSessionId":"fixture-session", "remoteRef":"fixture-ref", "sessionId":"fixture-session",
            "request":{"title":"Report","bSessionReference":null}});
        let expected_error =
            if crate::platform::capabilities::current().desktop_remote_control_supported {
                UNAVAILABLE
            } else {
                "desktop_remote_control_unsupported"
            };
        for partial in [false, true] {
            if partial {
                // Even an incorrectly published marker must not turn missing
                // runtime dependencies into panic or writable defaults.
                app.manage(DesktopRemoteReady);
            }
            for name in [
                "controlled_status",
                "controlled_list_workspaces",
                "controller_status",
                "proxy_open_live",
                "proxy_history_list",
                "proxy_history_read",
                "proxy_history_delete",
                "model_list_exportable",
                "permission_list_b",
                "permission_deny_task",
                "controller_capability_snapshot",
                "controller_capability_send",
                "controlled_capabilities_snapshot",
            ] {
                tauri::test::assert_ipc_response(
                    &window,
                    tauri::webview::InvokeRequest {
                        cmd: format!("desktop_remote_{name}"),
                        callback: tauri::ipc::CallbackFn(0),
                        error: tauri::ipc::CallbackFn(1),
                        url: window.url().unwrap(),
                        body: tauri::ipc::InvokeBody::Json(payload.clone()),
                        headers: Default::default(),
                        invoke_key: tauri::test::INVOKE_KEY.to_owned(),
                    },
                    Err::<String, String>(expected_error.to_owned()),
                );
            }
        }
        // Exercise the same macro/IPC extractor with a real managed manager,
        // not a JS mock or fabricated success payload.
        let manager = crate::features::remote_control::controller::controlled::ControlledDeviceManager::default();
        let expected_status = serde_json::to_value(manager.status()).unwrap();
        app.manage(manager);
        let response = tauri::test::get_ipc_response(
            &window,
            tauri::webview::InvokeRequest {
                cmd: "desktop_remote_controlled_status".into(),
                callback: tauri::ipc::CallbackFn(0),
                error: tauri::ipc::CallbackFn(1),
                url: window.url().unwrap(),
                body: Default::default(),
                headers: Default::default(),
                invoke_key: tauri::test::INVOKE_KEY.to_owned(),
            },
        );
        if expected_error == UNAVAILABLE {
            assert_eq!(
                response
                    .unwrap()
                    .deserialize::<serde_json::Value>()
                    .unwrap(),
                expected_status
            );
        } else {
            assert_eq!(response.unwrap_err(), serde_json::json!(expected_error));
        }
    }

    #[test]
    fn controlled_capability_ipc_requires_every_dependency_before_domain_access() {
        use crate::features::remote_control::controller::capabilities::CapabilitySnapshotManager;
        use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
        let app = tauri::test::mock_builder()
            .invoke_handler(tauri::generate_handler![
                commands::desktop_remote_capabilities::desktop_remote_controlled_capabilities_snapshot
            ])
            .build(tauri::test::mock_context(tauri::test::noop_assets()))
            .unwrap();
        let window = tauri::WebviewWindowBuilder::new(&app, "main", Default::default())
            .build()
            .unwrap();
        app.manage(DesktopRemoteReady);
        let expected = if crate::platform::capabilities::current().desktop_remote_control_supported
        {
            UNAVAILABLE
        } else {
            "desktop_remote_control_unsupported"
        };
        for installed in 0..3 {
            if installed == 1 {
                app.manage(ControlledDeviceManager::default());
            } else if installed == 2 {
                app.manage(CapabilitySnapshotManager::default());
            }
            // The real controlled manager is stopped. A prematurely executed
            // body would return not_connected instead of detecting the missing
            // snapshot manager/runtime at the IPC extraction boundary.
            tauri::test::assert_ipc_response(
                &window,
                tauri::webview::InvokeRequest {
                    cmd: "desktop_remote_controlled_capabilities_snapshot".into(),
                    callback: tauri::ipc::CallbackFn(0),
                    error: tauri::ipc::CallbackFn(1),
                    url: window.url().unwrap(),
                    body: tauri::ipc::InvokeBody::Json(
                        serde_json::json!({"sessionId":"fixture-session"}),
                    ),
                    headers: Default::default(),
                    invoke_key: tauri::test::INVOKE_KEY.to_owned(),
                },
                Err::<String, String>(expected.to_owned()),
            );
        }
    }
}
