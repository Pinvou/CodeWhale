//! A-side pending action receipts. Only an authenticated controller frame can
//! settle a request or attest that B completed an attachment upload. This is
//! process-local correlation, never a second SessionStore or transfer ledger.

use serde::Deserialize;
use serde_json::{Value, json};
use std::sync::Arc;
use tauri::{AppHandle, Manager as _};
#[cfg(test)]
use tokio::sync::oneshot;

use crate::features::behavior_telemetry::{RemoteControlEvent, RemoteStage};
use crate::features::remote_control::controller::controlling::ControllingDeviceManager;
use crate::features::remote_control::controller::controlling_session::ControllingSessionManager;
use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
#[cfg(test)]
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::protocol::{
    RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
};
use crate::features::remote_control::controller::relay_runtime::log_remote_action_event;
use crate::features::sessions::SessionStore;
use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;

#[path = "desktop_remote_action_state.rs"]
mod action_state;
pub(crate) use action_state::DesktopRemoteActionState;
#[cfg(test)]
use action_state::MAX_UPLOAD_RECEIPTS;

use super::desktop_remote_task::{lease_matches_remote_binding, with_current_dispatch_binding};

/// UI action parameters only. No lease, session wire ID, capability, key or
/// controller-supplied protocol envelope is accepted from WebView.
#[derive(Deserialize)]
#[serde(
    tag = "operation",
    rename_all = "snake_case",
    rename_all_fields = "camelCase",
    deny_unknown_fields
)]
pub enum RemoteActionIntent {
    PermissionList {
        task_id: String,
    },
    PermissionDecide {
        task_id: String,
        permission_request_id: String,
        decision: String,
    },
    AttachmentBegin {
        task_id: String,
        file_name: String,
        mime_type: String,
        total_bytes: u64,
        sha256: String,
    },
    AttachmentChunk {
        task_id: String,
        transfer_id: String,
        chunk_index: u64,
        chunk_b64: String,
        chunk_bytes: u64,
    },
    AttachmentAbort {
        task_id: String,
        transfer_id: String,
    },
    ArtifactIndex,
    ArtifactPreview {
        artifact_id: String,
    },
    ArtifactDownloadBegin {
        task_id: String,
        artifact_id: String,
    },
    ArtifactDownloadChunk {
        task_id: String,
        artifact_id: String,
        transfer_id: String,
        chunk_index: u64,
    },
    ArtifactDownloadAbort {
        task_id: String,
        artifact_id: String,
        transfer_id: String,
    },
}

impl RemoteActionIntent {
    fn operation(&self) -> &'static str {
        match self {
            Self::PermissionList { .. } => "permission_list",
            Self::PermissionDecide { .. } => "permission_decide",
            Self::AttachmentBegin { .. } => "attachment_begin",
            Self::AttachmentChunk { .. } => "attachment_chunk",
            Self::AttachmentAbort { .. } => "attachment_abort",
            Self::ArtifactIndex => "artifact_index",
            Self::ArtifactPreview { .. } => "artifact_preview",
            Self::ArtifactDownloadBegin { .. } => "artifact_download_begin",
            Self::ArtifactDownloadChunk { .. } => "artifact_download_chunk",
            Self::ArtifactDownloadAbort { .. } => "artifact_download_abort",
        }
    }

    fn task_id(&self) -> Option<&str> {
        match self {
            Self::PermissionList { task_id }
            | Self::PermissionDecide { task_id, .. }
            | Self::AttachmentBegin { task_id, .. }
            | Self::AttachmentChunk { task_id, .. }
            | Self::AttachmentAbort { task_id, .. }
            | Self::ArtifactDownloadBegin { task_id, .. }
            | Self::ArtifactDownloadChunk { task_id, .. }
            | Self::ArtifactDownloadAbort { task_id, .. } => Some(task_id),
            Self::ArtifactIndex | Self::ArtifactPreview { .. } => None,
        }
    }

    fn transfer_id(&self) -> Option<&str> {
        match self {
            Self::AttachmentChunk { transfer_id, .. }
            | Self::AttachmentAbort { transfer_id, .. }
            | Self::ArtifactDownloadChunk { transfer_id, .. }
            | Self::ArtifactDownloadAbort { transfer_id, .. } => Some(transfer_id),
            _ => None,
        }
    }

    fn body(&self) -> Result<Value, String> {
        let mut body = serde_json::to_value(self)
            .map_err(|_| "remote_action_invalid".to_owned())?["body"]
            .clone();
        for field in ["total_bytes", "chunk_index", "chunk_bytes"] {
            if let Some(number) = body.get(field).and_then(Value::as_u64) {
                body[field] = Value::String(number.to_string());
            }
        }
        Ok(body)
    }
}

impl serde::Serialize for RemoteActionIntent {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        // The same tagged shape is used only as a bounded source for the
        // server-built body; it is never accepted as a wire envelope.
        let (operation, body) = match self {
            Self::PermissionList { task_id } => (self.operation(), json!({"task_id":task_id})),
            Self::PermissionDecide {
                task_id,
                permission_request_id,
                decision,
            } => (
                self.operation(),
                json!({"task_id":task_id,"permission_request_id":permission_request_id,"decision":decision}),
            ),
            Self::AttachmentBegin {
                task_id,
                file_name,
                mime_type,
                total_bytes,
                sha256,
            } => (
                self.operation(),
                json!({"task_id":task_id,"file_name":file_name,"mime_type":mime_type,"total_bytes":total_bytes,"sha256":sha256}),
            ),
            Self::AttachmentChunk {
                task_id,
                transfer_id,
                chunk_index,
                chunk_b64,
                chunk_bytes,
            } => (
                self.operation(),
                json!({"task_id":task_id,"transfer_id":transfer_id,"chunk_index":chunk_index,"chunk_b64":chunk_b64,"chunk_bytes":chunk_bytes}),
            ),
            Self::AttachmentAbort {
                task_id,
                transfer_id,
            } => (
                self.operation(),
                json!({"task_id":task_id,"transfer_id":transfer_id}),
            ),
            Self::ArtifactIndex => (self.operation(), json!({})),
            Self::ArtifactPreview { artifact_id } => {
                (self.operation(), json!({"artifact_id":artifact_id}))
            }
            Self::ArtifactDownloadBegin {
                task_id,
                artifact_id,
            } => (
                self.operation(),
                json!({"task_id":task_id,"artifact_id":artifact_id}),
            ),
            Self::ArtifactDownloadChunk {
                task_id,
                artifact_id,
                transfer_id,
                chunk_index,
            } => (
                self.operation(),
                json!({"task_id":task_id,"artifact_id":artifact_id,"transfer_id":transfer_id,"chunk_index":chunk_index}),
            ),
            Self::ArtifactDownloadAbort {
                task_id,
                artifact_id,
                transfer_id,
            } => (
                self.operation(),
                json!({"task_id":task_id,"artifact_id":artifact_id,"transfer_id":transfer_id}),
            ),
        };
        json!({"operation":operation,"body":body}).serialize(serializer)
    }
}

#[derive(Debug, serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RemoteActionError {
    code: String,
    request_id: String,
    authoritative: bool,
}

#[tauri::command]
pub async fn desktop_remote_action_a(
    app: AppHandle,
    local_session_id: String,
    remote_ref: String,
    request_id: String,
    intent: RemoteActionIntent,
) -> Result<Value, RemoteActionError> {
    let operation = intent.operation();
    log_remote_action_event(
        "controller",
        "remote_action_requested",
        operation,
        "requested",
    );
    super::desktop_remote_state::ensure_ready(&app)
        .map_err(|code| {
            log_remote_action_event("controller", "remote_action_failed", operation, &code);
            code
        })
        .map_err(|code| RemoteActionError {
            code,
            request_id: request_id.clone(),
            authoritative: false,
        })?;
    let started = std::time::Instant::now();
    let mut metric = match &intent {
        RemoteActionIntent::AttachmentBegin { .. } => {
            Some((RemoteStage::Attachment, request_id.clone()))
        }
        RemoteActionIntent::ArtifactDownloadBegin { .. } => {
            Some((RemoteStage::Artifact, request_id.clone()))
        }
        RemoteActionIntent::AttachmentChunk { transfer_id, .. } => {
            Some((RemoteStage::Attachment, transfer_id.clone()))
        }
        RemoteActionIntent::ArtifactDownloadChunk { transfer_id, .. } => {
            Some((RemoteStage::Artifact, transfer_id.clone()))
        }
        _ => None,
    };
    let mut actual_request_id = request_id.clone();
    let mut authoritative = false;
    let result = async {
        let store = app.state::<SessionStore>();
        let manager = app.state::<ControllingDeviceManager>();
        let sessions = app.state::<ControllingSessionManager>();
        let state = app.state::<Arc<DesktopRemoteActionState>>();
        let live = store
            .remote_dispatch_live_context(&local_session_id, &remote_ref)
            .map_err(|_| "remote_binding_mismatch".to_owned())?;
        let (lease, capabilities) = manager
            .live_runtime_context()
            .map_err(|_| "controller_not_connected".to_owned())?;
        if !lease_matches_remote_binding(&lease, &live) {
            return Err("remote_binding_mismatch".to_owned());
        }
        state.purge_stale(&live);
        sessions
            .state(&remote_ref, &lease)
            .map_err(|_| "remote_session_unavailable".to_owned())?;
        // `remote_dispatch_live_context` already verified the current context,
        // remote ref, confirmed mapping, and presence of B's Session id under
        // the store lock. Registration atomically rechecks that authority via
        // `with_current_dispatch_binding` and `require_live_remote_mutation`.
        // A's remote/session id and B's controlled Session id are distinct
        // namespaces and must not be compared for equality.
        if request_id.is_empty()
            || request_id.len() > 128
            || !request_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
        {
            return Err("remote_action_invalid".to_owned());
        }
        let operation = operation.to_owned();
        let body = intent.body()?;
        let mut payload = json!({"v":1,"type":"remote_action_request",
        "connection_id":lease.connection_id,"controller_lease_id":lease.controller_lease_id,
        "lease_epoch":lease.lease_epoch,"relay_epoch":lease.relay_epoch,
        "remote_control_generation":lease.remote_control_generation,
        "request_id":request_id,"session_id":remote_ref,
        "operation":operation,"body":body});
        validate_controller_runtime_payload_json(
            &payload.to_string(),
            SenderRole::ControllerA,
            Some(&RuntimeValidationContext {
                lease: Some(lease.clone()),
                negotiated_capabilities: capabilities.clone(),
            }),
        )
        .map_err(|error| {
            log_remote_action_event(
                "controller",
                "remote_action_validation_failed",
                &operation,
                error.code,
            );
            "remote_action_invalid".to_owned()
        })?;
        // Authoritative native ownership outlives renderer stop/reload. Reserve
        // before any dispatch and reuse an unresolved intent's original wire ID.
        let identity = state
            .reserve_identity(
                &local_session_id,
                &remote_ref,
                &live,
                &operation,
                &body,
                &request_id,
            )
            .map_err(|failure| {
                if let Some(id) = failure.request_id {
                    actual_request_id = id;
                }
                failure.code.to_owned()
            })?;
        let request_id = identity.request_id.clone();
        actual_request_id = request_id.clone();
        if matches!(
            &intent,
            RemoteActionIntent::AttachmentBegin { .. }
                | RemoteActionIntent::ArtifactDownloadBegin { .. }
        ) {
            if let Some((_, id)) = &mut metric {
                *id = request_id.clone();
            }
        }
        payload["request_id"] = json!(request_id);
        let receiver = with_current_dispatch_binding(&app, &live, || {
            store
                .require_live_remote_mutation(&local_session_id, &remote_ref, &live)
                .map_err(|_| "remote_binding_mismatch".to_owned())?;
            state
                .register(
                    request_id.clone(),
                    local_session_id.clone(),
                    remote_ref.clone(),
                    live.clone(),
                    operation.clone(),
                    intent.task_id().map(str::to_owned),
                    intent.transfer_id().map(str::to_owned),
                )
                .map_err(str::to_owned)
        })
        .map_err(|error| error.code().to_owned())?;
        identity.mark_dispatched();
        let result = send_action_encrypted(&app, &live, &payload);
        if result.is_err() {
            state.detach_uncertain(&request_id);
            log_remote_action_event(
                "controller",
                "remote_action_failed",
                &operation,
                "remote_send_uncertain",
            );
            return Err("result_unknown".to_owned());
        }
        log_remote_action_event("controller", "remote_action_sent", &operation, "sent");
        match tokio::time::timeout(std::time::Duration::from_secs(15), receiver).await {
            Ok(Ok(result)) => {
                authoritative = !matches!(&result, Err(code) if code == "result_unknown");
                identity.settle(authoritative);
                result
            }
            _ => {
                state.detach_uncertain(&request_id);
                log_remote_action_event(
                    "controller",
                    "remote_action_timeout",
                    &operation,
                    "result_unknown",
                );
                Err("result_unknown".to_owned())
            }
        }
    }
    .await;
    match &result {
        Ok(_) => log_remote_action_event("controller", "remote_action_completed", operation, "ok"),
        Err(code) => log_remote_action_event("controller", "remote_action_failed", operation, code),
    }
    if let Some((stage, transfer_id)) = metric {
        let completed = match (stage, &result) {
            (RemoteStage::Attachment, Ok(body)) => {
                body.get("state").and_then(Value::as_str) == Some("completed")
            }
            (RemoteStage::Artifact, Ok(body)) => {
                body.get("completed").and_then(Value::as_bool) == Some(true)
            }
            (_, Ok(_)) => true,
            (_, Err(_)) => true,
        };
        if completed {
            let mut event = RemoteControlEvent::from_result(
                stage,
                &result,
                started.elapsed().as_millis() as u64,
            );
            if let Ok(body) = &result {
                if let Some(bytes) = body
                    .get("received_bytes")
                    .and_then(Value::as_str)
                    .and_then(|value| value.parse::<u64>().ok())
                {
                    event = event.size_bytes(bytes);
                }
            }
            crate::features::behavior_telemetry::track_remote_transfer(&app, event, &transfer_id);
        }
    }
    result.map_err(|code| RemoteActionError {
        code,
        request_id: actual_request_id,
        authoritative,
    })
}

fn send_action_encrypted(
    app: &AppHandle,
    live: &LiveRemoteContext,
    payload: &Value,
) -> Result<(), String> {
    let manager = app.state::<ControllingDeviceManager>();
    let lane = app.state::<ControllingLaneState>();
    let identity = manager
        .authenticated_remote_proxy_identity()
        .map_err(|_| "remote_binding_mismatch".to_owned())?;
    if !lease_matches_remote_binding(&identity.0, live)
        || identity.1 != live.peer_id
        || identity.2 != live.resume_boundary_id
    {
        return Err("remote_binding_mismatch".to_owned());
    }
    let (lease, capabilities) = manager
        .live_runtime_context()
        .map_err(|_| "controller_not_connected".to_owned())?;
    if lease != identity.0 {
        return Err("remote_binding_mismatch".to_owned());
    }
    validate_controller_runtime_payload_json(
        &payload.to_string(),
        SenderRole::ControllerA,
        Some(&RuntimeValidationContext {
            lease: Some(lease),
            negotiated_capabilities: capabilities.clone(),
        }),
    )
    .map_err(|_| "remote_action_invalid".to_owned())?;
    let frame = lane
        .seal("remote_action_request", payload, &capabilities)
        .map_err(|_| "remote_encrypt_failed".to_owned())?;
    manager
        .send_authenticated_business_for_identity(&identity, &frame)
        .map_err(|_| "remote_send_uncertain".to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::sync::oneshot::error::TryRecvError;

    #[test]
    fn fallible_intent_body_preserves_the_typed_wire_shape() {
        for intent in [
            RemoteActionIntent::PermissionList {
                task_id: "task-1".into(),
            },
            RemoteActionIntent::PermissionDecide {
                task_id: "task-1".into(),
                permission_request_id: "permission-1".into(),
                decision: "deny".into(),
            },
            RemoteActionIntent::AttachmentBegin {
                task_id: "task-1".into(),
                file_name: "report.txt".into(),
                mime_type: "text/plain".into(),
                total_bytes: u64::MAX,
                sha256: "a".repeat(64),
            },
            RemoteActionIntent::AttachmentChunk {
                task_id: "task-1".into(),
                transfer_id: "transfer-1".into(),
                chunk_index: u64::MAX,
                chunk_b64: "YWJj".into(),
                chunk_bytes: 3,
            },
            RemoteActionIntent::AttachmentAbort {
                task_id: "task-1".into(),
                transfer_id: "transfer-1".into(),
            },
            RemoteActionIntent::ArtifactIndex,
            RemoteActionIntent::ArtifactPreview {
                artifact_id: "artifact-1".into(),
            },
            RemoteActionIntent::ArtifactDownloadBegin {
                task_id: "task-1".into(),
                artifact_id: "artifact-1".into(),
            },
            RemoteActionIntent::ArtifactDownloadChunk {
                task_id: "task-1".into(),
                artifact_id: "artifact-1".into(),
                transfer_id: "transfer-1".into(),
                chunk_index: u64::MAX,
            },
            RemoteActionIntent::ArtifactDownloadAbort {
                task_id: "task-1".into(),
                artifact_id: "artifact-1".into(),
                transfer_id: "transfer-1".into(),
            },
        ] {
            let serialized = serde_json::to_value(&intent).unwrap();
            let mut expected = serialized["body"].clone();
            for field in ["total_bytes", "chunk_index", "chunk_bytes"] {
                if let Some(number) = expected.get(field).and_then(Value::as_u64) {
                    expected[field] = Value::String(number.to_string());
                }
            }
            assert_eq!(intent.body().unwrap(), expected);
            assert_eq!(serialized["operation"], intent.operation());
        }
    }

    fn live() -> LiveRemoteContext {
        LiveRemoteContext {
            peer_id: "peer-0001".into(),
            resume_boundary_id: "resume-0001".into(),
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-0001".into(),
            generation: "generation-0001".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        }
    }

    fn lease() -> LeaseContext {
        let live = live();
        LeaseContext {
            connection_id: live.connection_id,
            controller_lease_id: live.controller_lease_id,
            lease_epoch: live.lease_epoch,
            relay_epoch: live.relay_epoch,
            remote_control_generation: live.generation,
        }
    }

    fn register(
        state: &DesktopRemoteActionState,
        request_id: &str,
        operation: &str,
        task_id: Option<&str>,
        transfer_id: Option<&str>,
    ) -> oneshot::Receiver<Result<Value, String>> {
        state
            .register(
                request_id.into(),
                "local-0001".into(),
                "remote-0001".into(),
                live(),
                operation.into(),
                task_id.map(str::to_owned),
                transfer_id.map(str::to_owned),
            )
            .unwrap()
    }

    fn settle(state: &DesktopRemoteActionState, payload: Value) {
        assert!(state.settle_authenticated(&lease(), &live(), &payload));
    }

    #[test]
    fn accepted_begin_without_transfer_id_is_unknown() {
        for body in [
            json!({}),
            json!({"transfer_id": ""}),
            json!({"transfer_id": 42}),
        ] {
            let state = DesktopRemoteActionState::default();
            let mut reply = register(
                &state,
                "request-missing-id",
                "attachment_begin",
                Some("task-1"),
                None,
            );
            settle(
                &state,
                json!({"request_id":"request-missing-id", "session_id":"remote-0001", "operation":"attachment_begin", "ok":true, "body":body}),
            );
            assert_eq!(reply.try_recv().unwrap(), Err("result_unknown".into()));
        }
    }

    #[test]
    fn clear_all_drops_pending_waiters() {
        let state = DesktopRemoteActionState::default();
        let mut receiver = register(&state, "request-1", "permission_list", Some("task-1"), None);
        state.clear_all();
        assert_eq!(receiver.try_recv(), Err(TryRecvError::Closed));
    }

    #[test]
    fn failed_chunk_retires_only_the_exact_upload_attestation() {
        let state = DesktopRemoteActionState::default();
        let mut begin = register(
            &state,
            "request-1",
            "attachment_begin",
            Some("task-1"),
            None,
        );
        settle(
            &state,
            json!({
                "request_id":"request-1","session_id":"remote-0001",
                "operation":"attachment_begin","ok":true,
                "body":{"transfer_id":"transfer-1"}
            }),
        );
        assert!(begin.try_recv().unwrap().is_ok());

        let mut chunk = register(
            &state,
            "request-2",
            "attachment_chunk",
            Some("task-1"),
            Some("transfer-1"),
        );
        settle(
            &state,
            json!({
                "request_id":"request-2","session_id":"remote-0001",
                "operation":"attachment_chunk","ok":false,
                "error_code":"remote_chunk_rejected","body":{}
            }),
        );
        assert_eq!(
            chunk.try_recv().unwrap(),
            Err("remote_chunk_rejected".into())
        );
        assert_eq!(
            state.validate_completed_uploads(
                "local-0001",
                "remote-0001",
                &live(),
                "task-1",
                &["transfer-1".into()],
            ),
            Err("remote_attachment_binding_mismatch")
        );
    }

    #[test]
    fn failed_chunk_retains_only_cleanup_binding_and_late_success_cannot_revive_it() {
        let state = DesktopRemoteActionState::default();
        let mut begin = register(
            &state,
            "request-1",
            "attachment_begin",
            Some("task-1"),
            None,
        );
        settle(
            &state,
            json!({"request_id":"request-1","session_id":"remote-0001","operation":"attachment_begin","ok":true,"body":{"transfer_id":"transfer-1"}}),
        );
        assert!(begin.try_recv().unwrap().is_ok());
        let mut failed = register(
            &state,
            "request-2",
            "attachment_chunk",
            Some("task-1"),
            Some("transfer-1"),
        );
        let mut late = register(
            &state,
            "request-3",
            "attachment_chunk",
            Some("task-1"),
            Some("transfer-1"),
        );
        settle(
            &state,
            json!({"request_id":"request-2","session_id":"remote-0001","operation":"attachment_chunk","ok":false,"error_code":"transfer_staging_failed","body":{}}),
        );
        assert!(failed.try_recv().unwrap().is_err());
        settle(
            &state,
            json!({"request_id":"request-3","session_id":"remote-0001","operation":"attachment_chunk","ok":true,"body":{"state":"completed"}}),
        );
        assert_eq!(
            late.try_recv().unwrap(),
            Err("remote_attachment_unconfirmed".into())
        );
        assert!(
            state
                .validate_completed_uploads(
                    "local-0001",
                    "remote-0001",
                    &live(),
                    "task-1",
                    &["transfer-1".into()]
                )
                .is_err()
        );
        let mut abort = register(
            &state,
            "request-4",
            "attachment_abort",
            Some("task-1"),
            Some("transfer-1"),
        );
        settle(
            &state,
            json!({"request_id":"request-4","session_id":"remote-0001","operation":"attachment_abort","ok":false,"error_code":"transfer_unknown","body":{}}),
        );
        assert_eq!(
            abort.try_recv().unwrap().unwrap(),
            json!({"aborted": false, "already_absent": true})
        );
        assert!(!state.inner.lock().uploads.contains_key("transfer-1"));
    }

    #[test]
    fn accepted_begin_without_receipt_capacity_is_unknown_not_a_definite_failure() {
        let state = DesktopRemoteActionState::default();
        for index in 0..MAX_UPLOAD_RECEIPTS {
            let request = format!("capacity-request-{index}");
            let transfer = format!("capacity-transfer-{index}");
            let mut begin = register(&state, &request, "attachment_begin", Some("task-1"), None);
            settle(
                &state,
                json!({"request_id":request,"session_id":"remote-0001","operation":"attachment_begin","ok":true,"body":{"transfer_id":transfer}}),
            );
            assert!(begin.try_recv().unwrap().is_ok());
        }
        let mut begin = register(
            &state,
            "capacity-overflow",
            "attachment_begin",
            Some("task-1"),
            None,
        );
        settle(
            &state,
            json!({"request_id":"capacity-overflow","session_id":"remote-0001","operation":"attachment_begin","ok":true,"body":{"transfer_id":"overflow-transfer"}}),
        );
        assert_eq!(begin.try_recv().unwrap(), Err("result_unknown".into()));
    }

    #[test]
    fn completed_upload_cannot_cross_task_binding() {
        let state = DesktopRemoteActionState::default();
        let mut begin = register(
            &state,
            "request-1",
            "attachment_begin",
            Some("task-1"),
            None,
        );
        settle(
            &state,
            json!({
                "request_id":"request-1","session_id":"remote-0001",
                "operation":"attachment_begin","ok":true,
                "body":{"transfer_id":"transfer-1"}
            }),
        );
        assert!(begin.try_recv().unwrap().is_ok());

        let mut chunk = register(
            &state,
            "request-2",
            "attachment_chunk",
            Some("task-2"),
            Some("transfer-1"),
        );
        settle(
            &state,
            json!({
                "request_id":"request-2","session_id":"remote-0001",
                "operation":"attachment_chunk","ok":true,
                "body":{"state":"completed"}
            }),
        );
        assert_eq!(
            chunk.try_recv().unwrap(),
            Err("remote_attachment_binding_mismatch".into())
        );
        // 绑定不匹配的 ok 回执不得驱逐另一个 task 的存证（与 !ok 路径对称）：
        // task-1 仍可用同一 transfer 完成上传。
        assert!(state.inner.lock().uploads.contains_key("transfer-1"));
        let mut retry = register(
            &state,
            "request-3",
            "attachment_chunk",
            Some("task-1"),
            Some("transfer-1"),
        );
        settle(
            &state,
            json!({
                "request_id":"request-3","session_id":"remote-0001",
                "operation":"attachment_chunk","ok":true,
                "body":{"state":"completed"}
            }),
        );
        assert!(retry.try_recv().unwrap().is_ok());
        assert!(
            state
                .validate_completed_uploads(
                    "local-0001",
                    "remote-0001",
                    &live(),
                    "task-1",
                    &["transfer-1".into()],
                )
                .is_ok()
        );
    }

    #[test]
    fn uncertain_chunk_keeps_its_upload_attestation_for_same_id_retry() {
        let state = DesktopRemoteActionState::default();
        let mut begin = register(
            &state,
            "request-1",
            "attachment_begin",
            Some("task-1"),
            None,
        );
        settle(
            &state,
            json!({
                "request_id":"request-1","session_id":"remote-0001",
                "operation":"attachment_begin","ok":true,
                "body":{"transfer_id":"transfer-1"}
            }),
        );
        assert!(begin.try_recv().unwrap().is_ok());
        let _chunk = register(
            &state,
            "request-2",
            "attachment_chunk",
            Some("task-1"),
            Some("transfer-1"),
        );
        state.detach_uncertain("request-2");
        assert!(state.inner.lock().uploads.contains_key("transfer-1"));
        assert_eq!(
            state.validate_completed_uploads(
                "local-0001",
                "remote-0001",
                &live(),
                "foreign-task",
                &["transfer-1".into()]
            ),
            Err("remote_attachment_binding_mismatch")
        );
        assert_eq!(
            state.validate_completed_uploads(
                "local-0001",
                "remote-0001",
                &live(),
                "task-1",
                &["transfer-1".into()],
            ),
            Err("remote_attachment_binding_mismatch")
        );
        let mut retry = register(
            &state,
            "request-2",
            "attachment_chunk",
            Some("task-1"),
            Some("transfer-1"),
        );
        settle(
            &state,
            json!({"request_id":"request-2", "session_id":"remote-0001", "operation":"attachment_chunk", "ok":true, "body":{"state":"completed"}}),
        );
        assert!(retry.try_recv().unwrap().is_ok());
        assert!(
            state
                .validate_completed_uploads(
                    "local-0001",
                    "remote-0001",
                    &live(),
                    "task-1",
                    &["transfer-1".into()]
                )
                .is_ok()
        );
    }

    #[test]
    fn nested_webview_intent_is_strictly_camel_case() {
        assert!(matches!(
            serde_json::from_value::<RemoteActionIntent>(json!({
                "operation": "permission_decide",
                "taskId": "task-1",
                "permissionRequestId": "permission-1",
                "decision": "allow"
            }))
            .unwrap(),
            RemoteActionIntent::PermissionDecide { .. }
        ));
        assert!(
            serde_json::from_value::<RemoteActionIntent>(json!({
                "operation": "permission_decide",
                "task_id": "task-1",
                "permission_request_id": "permission-1",
                "decision": "allow"
            }))
            .is_err()
        );
    }
}
