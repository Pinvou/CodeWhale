//! A-side pending action receipts. Only an authenticated controller frame can
//! settle a request or attest that B completed an attachment upload. This is
//! process-local correlation, never a second SessionStore or transfer ledger.

use std::collections::{HashMap, HashSet};
use std::time::{Duration, Instant};

use parking_lot::Mutex;
use serde::Deserialize;
use serde_json::{Value, json};
use std::sync::Arc;
use tauri::{AppHandle, Manager as _};
use tokio::sync::oneshot;

use crate::features::behavior_telemetry::{RemoteControlEvent, RemoteStage, track_remote_control};
use crate::features::remote_control::controller::controlling::ControllingDeviceManager;
use crate::features::remote_control::controller::controlling_session::ControllingSessionManager;
use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
use crate::features::remote_control::controller::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
};
use crate::features::sessions::SessionStore;
use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;

use super::desktop_remote_task::{lease_matches_remote_binding, with_current_dispatch_binding};

const MAX_PENDING_ACTIONS: usize = 64;
const MAX_UPLOAD_RECEIPTS: usize = 128;
const UPLOAD_RECEIPT_TTL: Duration = Duration::from_secs(10 * 60);

struct PendingAction {
    local_session_id: String,
    remote_ref: String,
    live: LiveRemoteContext,
    operation: String,
    task_id: Option<String>,
    transfer_id: Option<String>,
    reply: oneshot::Sender<Result<Value, String>>,
}

struct CompletedUpload {
    local_session_id: String,
    remote_ref: String,
    live: LiveRemoteContext,
    task_id: String,
    complete: bool,
    touched_at: Instant,
}

#[derive(Default)]
struct ActionInner {
    pending: HashMap<String, PendingAction>,
    uploads: HashMap<String, CompletedUpload>,
}

#[derive(Default)]
pub(crate) struct DesktopRemoteActionState {
    inner: Mutex<ActionInner>,
}

impl DesktopRemoteActionState {
    fn purge_expired_uploads(inner: &mut ActionInner) {
        let now = Instant::now();
        inner.uploads.retain(|_, upload| {
            now.saturating_duration_since(upload.touched_at) <= UPLOAD_RECEIPT_TTL
        });
    }

    /// Connection transitions invalidate every process-local waiter and
    /// attachment attestation. Dropping senders wakes pending callers.
    pub(crate) fn clear_all(&self) {
        let mut inner = self.inner.lock();
        inner.pending.clear();
        inner.uploads.clear();
    }

    pub(crate) fn pending_binding(
        &self,
        request_id: &str,
    ) -> Option<(String, String, LiveRemoteContext)> {
        let mut inner = self.inner.lock();
        Self::purge_expired_uploads(&mut inner);
        inner.pending.get(request_id).map(|pending| {
            (
                pending.local_session_id.clone(),
                pending.remote_ref.clone(),
                pending.live.clone(),
            )
        })
    }

    /// A new authenticated generation cannot inherit pending replies or
    /// completed receipts from the previous one. Dropped senders wake waiters.
    pub(crate) fn purge_stale(&self, live: &LiveRemoteContext) {
        let mut inner = self.inner.lock();
        Self::purge_expired_uploads(&mut inner);
        inner.pending.retain(|_, pending| pending.live == *live);
        inner.uploads.retain(|_, upload| upload.live == *live);
    }

    pub(crate) fn retire_uploaded_receipts(
        &self,
        local_session_id: &str,
        remote_ref: &str,
        live: &LiveRemoteContext,
        task_id: &str,
        attachment_ids: &[String],
    ) {
        let mut inner = self.inner.lock();
        for id in attachment_ids {
            if inner.uploads.get(id).is_some_and(|upload| {
                upload.local_session_id == local_session_id
                    && upload.remote_ref == remote_ref
                    && upload.live == *live
                    && upload.task_id == task_id
            }) {
                inner.uploads.remove(id);
            }
        }
    }

    pub(crate) fn discard_task_receipts(&self, local_session_id: &str, task_id: &str) {
        let mut inner = self.inner.lock();
        inner.uploads.retain(|_, upload| {
            upload.local_session_id != local_session_id || upload.task_id != task_id
        });
        inner.pending.retain(|_, pending| {
            pending.local_session_id != local_session_id
                || pending.task_id.as_deref() != Some(task_id)
        });
    }

    pub(crate) fn register(
        &self,
        request_id: String,
        local_session_id: String,
        remote_ref: String,
        live: LiveRemoteContext,
        operation: String,
        task_id: Option<String>,
        transfer_id: Option<String>,
    ) -> Result<oneshot::Receiver<Result<Value, String>>, &'static str> {
        let mut inner = self.inner.lock();
        Self::purge_expired_uploads(&mut inner);
        if inner.pending.len() >= MAX_PENDING_ACTIONS || inner.pending.contains_key(&request_id) {
            return Err("remote_action_capacity");
        }
        let (reply, receiver) = oneshot::channel();
        inner.pending.insert(
            request_id,
            PendingAction {
                local_session_id,
                remote_ref,
                live,
                operation,
                task_id,
                transfer_id,
                reply,
            },
        );
        Ok(receiver)
    }

    /// Stop waiting after an uncertain send/timeout without changing any
    /// transfer receipt. The peer may already have completed the action; a
    /// retry must therefore reuse the same request id and reconcile against
    /// B's idempotency cache rather than treating the action as failed.
    pub(crate) fn detach_uncertain(&self, request_id: &str) {
        self.inner.lock().pending.remove(request_id);
    }

    /// Called only from the AEAD-opened A inbound branch. The caller still
    /// checks current manager/lane/SessionStore before passing the frame here.
    pub(crate) fn settle_authenticated(
        &self,
        lease: &LeaseContext,
        current_live: &LiveRemoteContext,
        payload: &Value,
    ) -> bool {
        let Some(request_id) = payload.get("request_id").and_then(Value::as_str) else {
            return false;
        };
        let mut inner = self.inner.lock();
        Self::purge_expired_uploads(&mut inner);
        let Some(pending) = inner.pending.get(request_id) else {
            return false;
        };
        if pending.live != *current_live
            || !same_lease(lease, &pending.live)
            || payload.get("session_id").and_then(Value::as_str)
                != Some(pending.remote_ref.as_str())
            || payload.get("operation").and_then(Value::as_str) != Some(pending.operation.as_str())
        {
            return false;
        }
        let pending = inner
            .pending
            .remove(request_id)
            .expect("matched pending action");
        let ok = payload.get("ok").and_then(Value::as_bool) == Some(true);
        let body = payload.get("body").cloned().unwrap_or(Value::Null);
        if ok && pending.operation == "attachment_begin" {
            let (Some(task_id), Some(transfer_id)) = (
                pending.task_id.as_deref(),
                body.get("transfer_id").and_then(Value::as_str),
            ) else {
                let _ = pending
                    .reply
                    .send(Err("remote_attachment_unconfirmed".into()));
                return true;
            };
            if inner.uploads.len() < MAX_UPLOAD_RECEIPTS && !inner.uploads.contains_key(transfer_id)
            {
                inner.uploads.insert(
                    transfer_id.to_owned(),
                    CompletedUpload {
                        local_session_id: pending.local_session_id.clone(),
                        remote_ref: pending.remote_ref.clone(),
                        live: pending.live.clone(),
                        task_id: task_id.to_owned(),
                        complete: false,
                        touched_at: Instant::now(),
                    },
                );
            } else {
                let _ = pending.reply.send(Err("remote_upload_capacity".into()));
                return true;
            }
        }
        if ok && pending.operation == "attachment_chunk" {
            let Some(transfer_id) = pending.transfer_id.as_deref() else {
                let _ = pending
                    .reply
                    .send(Err("remote_attachment_unconfirmed".into()));
                return true;
            };
            let bound = inner.uploads.get(transfer_id).is_some_and(|upload| {
                upload.local_session_id == pending.local_session_id
                    && upload.remote_ref == pending.remote_ref
                    && upload.live == pending.live
                    && Some(upload.task_id.as_str()) == pending.task_id.as_deref()
            });
            if !bound {
                inner.uploads.remove(transfer_id);
                let _ = pending
                    .reply
                    .send(Err("remote_attachment_binding_mismatch".into()));
                return true;
            }
            let upload = inner
                .uploads
                .get_mut(transfer_id)
                .expect("binding checked while holding the same lock");
            if body.get("state").and_then(Value::as_str) == Some("completed") {
                upload.complete = true;
            }
            upload.touched_at = Instant::now();
        }
        if ok && pending.operation == "attachment_abort" {
            let Some(transfer_id) = pending.transfer_id.as_deref() else {
                let _ = pending
                    .reply
                    .send(Err("remote_attachment_unconfirmed".into()));
                return true;
            };
            let bound = inner.uploads.get(transfer_id).is_some_and(|upload| {
                upload.local_session_id == pending.local_session_id
                    && upload.remote_ref == pending.remote_ref
                    && upload.live == pending.live
                    && Some(upload.task_id.as_str()) == pending.task_id.as_deref()
            });
            if !bound {
                let _ = pending
                    .reply
                    .send(Err("remote_attachment_binding_mismatch".into()));
                return true;
            }
            inner.uploads.remove(transfer_id);
        }
        if !ok
            && matches!(
                pending.operation.as_str(),
                "attachment_chunk" | "attachment_abort"
            )
        {
            if let Some(transfer_id) = pending.transfer_id.as_deref() {
                let bound = inner.uploads.get(transfer_id).is_some_and(|upload| {
                    upload.local_session_id == pending.local_session_id
                        && upload.remote_ref == pending.remote_ref
                        && upload.live == pending.live
                        && Some(upload.task_id.as_str()) == pending.task_id.as_deref()
                });
                if bound {
                    inner.uploads.remove(transfer_id);
                }
            }
        }
        let result = if ok {
            Ok(body)
        } else {
            Err(payload
                .get("error_code")
                .and_then(Value::as_str)
                .unwrap_or("remote_action_failed")
                .to_owned())
        };
        let _ = pending.reply.send(result);
        true
    }

    pub(crate) fn validate_completed_uploads(
        &self,
        local_session_id: &str,
        remote_ref: &str,
        live: &LiveRemoteContext,
        task_id: &str,
        attachment_ids: &[String],
    ) -> Result<(), &'static str> {
        if attachment_ids.len() > 32 {
            return Err("remote_attachment_count_invalid");
        }
        let mut inner = self.inner.lock();
        Self::purge_expired_uploads(&mut inner);
        let mut unique = HashSet::new();
        for id in attachment_ids {
            if !unique.insert(id) {
                return Err("remote_attachment_duplicate");
            }
            let upload = inner
                .uploads
                .get(id)
                .ok_or("remote_attachment_unconfirmed")?;
            if !upload.complete
                || upload.local_session_id != local_session_id
                || upload.remote_ref != remote_ref
                || upload.live != *live
                || upload.task_id != task_id
            {
                return Err("remote_attachment_binding_mismatch");
            }
        }
        Ok(())
    }
}

fn same_lease(lease: &LeaseContext, live: &LiveRemoteContext) -> bool {
    lease.connection_id == live.connection_id
        && lease.controller_lease_id == live.controller_lease_id
        && lease.lease_epoch == live.lease_epoch
        && lease.relay_epoch == live.relay_epoch
        && lease.remote_control_generation == live.generation
}

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

    fn body(&self) -> Value {
        let mut body =
            serde_json::to_value(self).expect("action intent serializes")["body"].clone();
        for field in ["total_bytes", "chunk_index", "chunk_bytes"] {
            if let Some(number) = body.get(field).and_then(Value::as_u64) {
                body[field] = Value::String(number.to_string());
            }
        }
        body
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

#[tauri::command]
pub async fn desktop_remote_action_a(
    app: AppHandle,
    local_session_id: String,
    remote_ref: String,
    request_id: String,
    intent: RemoteActionIntent,
) -> Result<Value, String> {
    crate::platform::capabilities::ensure_desktop_remote_control_enabled()?;
    let started = std::time::Instant::now();
    let metric = match &intent {
        RemoteActionIntent::AttachmentChunk { .. } => Some(RemoteStage::Attachment),
        RemoteActionIntent::ArtifactDownloadChunk { .. } => Some(RemoteStage::Artifact),
        _ => None,
    };
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
        let session = sessions
            .state(&remote_ref, &lease)
            .map_err(|_| "remote_session_unavailable".to_owned())?;
        let record = store
            .remote_proxy_snapshot(&local_session_id)
            .ok_or_else(|| "remote_proxy_not_found".to_owned())?;
        if record.controlled_session_id.as_deref() != Some(session.session_id.as_str()) {
            return Err("remote_mapping_unconfirmed".to_owned());
        }
        if request_id.is_empty()
            || request_id.len() > 128
            || !request_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
        {
            return Err("remote_action_invalid".to_owned());
        }
        let operation = intent.operation().to_owned();
        let payload = json!({"v":1,"type":"remote_action_request",
        "connection_id":lease.connection_id,"controller_lease_id":lease.controller_lease_id,
        "lease_epoch":lease.lease_epoch,"relay_epoch":lease.relay_epoch,
        "remote_control_generation":lease.remote_control_generation,
        "request_id":request_id,"session_id":remote_ref,
        "operation":operation,"body":intent.body()});
        validate_controller_runtime_payload_json(
            &payload.to_string(),
            SenderRole::ControllerA,
            Some(&RuntimeValidationContext {
                lease: Some(lease.clone()),
                negotiated_capabilities: capabilities.clone(),
            }),
        )
        .map_err(|_| "remote_action_invalid".to_owned())?;
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
        let result = send_action_encrypted(&app, &live, &payload);
        if result.is_err() {
            state.detach_uncertain(&request_id);
            return Err("result_unknown".to_owned());
        }
        match tokio::time::timeout(std::time::Duration::from_secs(15), receiver).await {
            Ok(Ok(result)) => result,
            _ => {
                state.detach_uncertain(&request_id);
                Err("result_unknown".to_owned())
            }
        }
    }
    .await;
    if let Some(stage) = metric {
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
            track_remote_control(&app, event);
        }
    }
    result
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
            Err("remote_attachment_unconfirmed")
        );
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
