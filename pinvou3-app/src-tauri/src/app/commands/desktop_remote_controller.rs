//! Windows controller-device connection commands.
//!
//! DEV-20 owns final handler registration. The wrappers never persist a code,
//! device credential, token, or key, and do not equate window visibility with
//! disconnection.

use super::desktop_remote_state::DesktopRemoteState as State;
use tauri::{AppHandle, Emitter};

use crate::features::behavior_telemetry::{RemoteControlEvent, RemoteStage, track_remote_control};
use crate::features::remote_control::RemoteControlManager;
use crate::features::remote_control::controller::controlling::{
    ControllingDeviceManager, ControllingStateError, ControllingStatus,
};
use crate::features::remote_control::controller::controlling_service::{
    CONTROLLER_STATUS_EVENT, connect_controlling_device,
};
use crate::features::remote_control::controller::controlling_session::ControllingSessionManager;
use crate::features::remote_control::controller::relay_runtime::log_controller_event;
use crate::features::remote_control::controller::transfer::download::ArtifactDownloads;
use std::sync::Arc;

const STATUS_EVENT: &str = CONTROLLER_STATUS_EVENT;
const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

fn controlling_error(error: ControllingStateError) -> String {
    error.code.to_owned()
}

fn ensure_supported(manager: &ControllingDeviceManager) -> Result<(), String> {
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
    manager
        .is_supported()
        .then_some(())
        .ok_or_else(|| UNSUPPORTED.to_owned())
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
    let started = std::time::Instant::now();
    let result = async {
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
    }.await;
    track_remote_control(
        &app,
        RemoteControlEvent::from_result(
            RemoteStage::Connection,
            &result,
            started.elapsed().as_millis() as u64,
        )
        .role(false),
    );
    result
}

#[tauri::command]
pub async fn desktop_remote_controller_disconnect(
    app: AppHandle,
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    actions: State<'_, Arc<super::desktop_remote_action::DesktopRemoteActionState>>,
    artifacts: State<'_, Arc<super::desktop_remote_artifact::DesktopRemoteArtifactRuntime>>,
) -> Result<ControllingStatus, String> {
    let started = std::time::Instant::now();
    let result = async {
        ensure_supported(&manager)?;
        Ok(disconnect_controller_state(
            &manager,
            &sessions,
            &actions,
            artifacts.downloads(),
            |status| emit_status(&app, status),
        )
        .await)
    }
    .await;
    track_remote_control(
        &app,
        RemoteControlEvent::from_result(
            RemoteStage::Disconnection,
            &result,
            started.elapsed().as_millis() as u64,
        )
        .role(false),
    );
    result
}

async fn disconnect_controller_state(
    manager: &ControllingDeviceManager,
    sessions: &ControllingSessionManager,
    actions: &super::desktop_remote_action::DesktopRemoteActionState,
    downloads: &ArtifactDownloads,
    publish: impl FnOnce(&ControllingStatus),
) -> ControllingStatus {
    let mut attempt = manager.begin_disconnect("user_disconnect");
    if let Some(receipt) = attempt.receipt.take() {
        // Queue admission is not delivery. Allow the final frame to flush,
        // while keeping local cleanup authoritative for offline peers.
        let _ = tokio::time::timeout(std::time::Duration::from_secs(2), receipt).await;
    }
    let (status, connection_id, applied) = manager.complete_disconnect(&attempt);
    if let Some(connection_id) = connection_id {
        sessions.clear_connection(&connection_id);
        actions.clear_connection(&connection_id);
        // A owns this download/index scope. B capture/publication keeps its
        // existing lifecycle owner.
        downloads.cleanup_for_connection(&connection_id);
    }
    if applied && !manager.publish_disconnect_if_current(&attempt, &status, || publish(&status)) {
        return manager.status();
    }
    status
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
    use super::*;
    use crate::features::remote_control::controller::controlling::ControllerLease;
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use crate::features::remote_control::controller::relay_transport::{
        AuthenticatedLease, ControllerRole, transport_channels,
    };
    use crate::features::remote_control::controller::transfer::TransferRegistry;
    use crate::features::remote_control::controller::transfer::download::{
        ArtifactIndex, MemoryArtifactSource,
    };
    use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;
    use serde_json::json;
    use sha2::{Digest, Sha256};
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn lease(connection_id: &str) -> ControllerLease {
        ControllerLease {
            context: LeaseContext {
                connection_id: connection_id.into(),
                controller_lease_id: "lease-0001".into(),
                lease_epoch: "1".into(),
                relay_epoch: "relay-epoch-0001".into(),
                remote_control_generation: "generation-0001".into(),
            },
            peer_endpoint_id: "endpoint-0001".into(),
            reservation_id: "reservation-0001".into(),
            device_display_name: "Test PC".into(),
            liveness_epoch_a: "live-a-1".into(),
            liveness_epoch_b: "live-b-1".into(),
            resume_deadline_b: 30_000,
            resume_token: [1; 32],
            transport_key_fingerprint: [2; 32],
            negotiated_capabilities: vec![
                "encrypted_frames_v1".into(),
                "pairing_v1".into(),
                "task_v1".into(),
            ],
        }
    }

    #[tokio::test]
    async fn concurrent_disconnect_waits_preserve_all_successor_resources_and_next_connect() {
        let manager = ControllingDeviceManager::default();
        let sessions = ControllingSessionManager::default();
        let actions = super::super::desktop_remote_action::DesktopRemoteActionState::default();
        let index = Arc::new(ArtifactIndex::default());
        let downloads =
            ArtifactDownloads::new(Arc::new(TransferRegistry::default()), Arc::clone(&index));
        let original = lease("connection-original");
        let successor = lease("connection-successor");
        let add_resources = |lease: &ControllerLease, artifact: &str| {
            let (session, _) = sessions
                .open_with_outcome(
                    None,
                    "Test session",
                    "chat",
                    &lease.context,
                    &lease.negotiated_capabilities,
                )
                .unwrap();
            let live = LiveRemoteContext {
                peer_id: "peer-0001".into(),
                resume_boundary_id: "resume-0001".into(),
                connection_id: lease.context.connection_id.clone(),
                controller_lease_id: lease.context.controller_lease_id.clone(),
                lease_epoch: lease.context.lease_epoch.clone(),
                relay_epoch: lease.context.relay_epoch.clone(),
                generation: lease.context.remote_control_generation.clone(),
                control_connected: true,
                encrypted_channel_connected: true,
            };
            let (_, reply) = actions
                .register(
                    artifact.into(),
                    "local-0001".into(),
                    session.session_id.clone(),
                    live,
                    "permission_list".into(),
                    Some("task-0001".into()),
                    None,
                )
                .unwrap();
            let bytes = b"artifact";
            let digest: String = Sha256::digest(bytes)
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect();
            index
                .register(
                    &lease.context,
                    &session.session_id,
                    "task-0001",
                    artifact,
                    "result.txt",
                    "text/plain",
                    &digest,
                    Arc::new(MemoryArtifactSource::new(bytes.to_vec())),
                )
                .unwrap();
            let handle = downloads
                .begin(
                    &lease.context,
                    &session.session_id,
                    "task-0001",
                    artifact,
                    1_000,
                )
                .unwrap();
            (session, reply, handle)
        };
        let old_operation = manager.connect("2345678").unwrap();
        manager.complete_reserved(old_operation.clone()).unwrap();
        manager
            .complete_connected(old_operation.clone(), original.clone())
            .unwrap();
        let (transport, receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            4,
            2,
            8192,
            2048,
        );
        transport
            .install_authenticated_lease(AuthenticatedLease {
                context: original.context.clone(),
                negotiated_capabilities: original.negotiated_capabilities.clone(),
            })
            .unwrap();
        let old_transport = transport.clone();
        manager.replace_transport(transport);
        let (old_session, mut old_reply, old_download) =
            add_resources(&original, "artifact-original");
        let published = AtomicUsize::new(0);
        let publish = |_: &ControllingStatus| {
            published.fetch_add(1, Ordering::SeqCst);
        };
        let mut first = Box::pin(disconnect_controller_state(
            &manager, &sessions, &actions, &downloads, publish,
        ));
        let mut second = Box::pin(disconnect_controller_state(
            &manager, &sessions, &actions, &downloads, publish,
        ));
        assert!(futures_util::poll!(first.as_mut()).is_pending());
        assert!(futures_util::poll!(second.as_mut()).is_pending());
        manager
            .fail_connection(old_operation, "endpoint_offline")
            .unwrap();
        let next_operation = manager.connect("3456789").unwrap();
        manager.complete_reserved(next_operation.clone()).unwrap();
        manager
            .complete_connected(next_operation, successor.clone())
            .unwrap();
        let (next_transport, _next_receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            4,
            2,
            8192,
            2048,
        );
        next_transport
            .install_authenticated_lease(AuthenticatedLease {
                context: successor.context.clone(),
                negotiated_capabilities: successor.negotiated_capabilities.clone(),
            })
            .unwrap();
        manager.replace_transport(next_transport.clone());
        let (next_session, mut next_reply, next_download) =
            add_resources(&successor, "artifact-successor");
        // Closing the controlled transport releases both real receipt waits.
        // Both normal command continuations now resume after the successor owns
        // sessions, pending actions, a live download and its artifact index.
        drop(receiver);
        assert_eq!(
            serde_json::to_value(first.await).unwrap()["lease"]["connectionId"],
            "connection-successor"
        );
        assert_eq!(
            serde_json::to_value(second.await).unwrap()["lease"]["connectionId"],
            "connection-successor"
        );
        assert_eq!(published.load(Ordering::SeqCst), 0);
        assert!(old_transport.is_stopped());
        assert!(!next_transport.is_stopped());
        assert!(manager.send_disconnect("transport_probe"));
        assert!(
            sessions
                .state(&old_session.session_ref, &original.context)
                .is_err()
        );
        assert!(
            sessions
                .state(&next_session.session_ref, &successor.context)
                .is_ok()
        );
        assert_eq!(
            old_reply.try_recv(),
            Err(tokio::sync::oneshot::error::TryRecvError::Closed)
        );
        assert_eq!(
            next_reply.try_recv(),
            Err(tokio::sync::oneshot::error::TryRecvError::Empty)
        );
        assert!(actions.settle_authenticated(
            &successor.context,
            &actions.pending_binding("artifact-successor").unwrap().2,
            &json!({"request_id":"artifact-successor", "session_id":next_session.session_id,
                "operation":"permission_list", "ok":true, "body":{"permissions":[]}}),
        ));
        assert!(next_reply.await.unwrap().is_ok());
        assert!(
            downloads
                .next_chunk_bound(
                    &original.context,
                    &old_session.session_id,
                    "task-0001",
                    "artifact-original",
                    &old_download.handle_id,
                    0,
                    1_001,
                )
                .is_err()
        );
        assert!(
            downloads
                .next_chunk_bound(
                    &successor.context,
                    &next_session.session_id,
                    "task-0001",
                    "artifact-successor",
                    &next_download.handle_id,
                    0,
                    1_001,
                )
                .unwrap()
                .is_some()
        );
        assert_eq!(index.len(), 1);
        disconnect_controller_state(&manager, &sessions, &actions, &downloads, publish).await;
        assert!(next_transport.is_stopped());
        assert_eq!(published.load(Ordering::SeqCst), 1);
        assert_eq!(index.len(), 0);
        let third_operation = manager.connect("456789A").unwrap();
        manager.complete_reserved(third_operation.clone()).unwrap();
        manager
            .complete_connected(third_operation, lease("connection-third"))
            .unwrap();
        assert_eq!(
            manager.live_lease().unwrap().connection_id,
            "connection-third"
        );
    }

    #[test]
    fn controller_command_surface_is_stable_and_contains_no_code_storage() {
        assert_eq!(super::STATUS_EVENT, "desktop-remote-controller-status");
        assert_eq!(super::UNSUPPORTED, "desktop_remote_control_unsupported");
    }
}
