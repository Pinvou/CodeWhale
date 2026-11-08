//! Process-local action correlation and upload receipts.
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::relay_runtime::log_remote_action_event;
use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;
use parking_lot::Mutex;
use serde_json::{Value, json};
use std::collections::{HashMap, HashSet};
use std::time::{Duration, Instant};
use tokio::sync::oneshot;

const MAX_PENDING_ACTIONS: usize = 64;
pub(super) const MAX_UPLOAD_RECEIPTS: usize = 128;
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

pub(super) struct CompletedUpload {
    local_session_id: String,
    remote_ref: String,
    live: LiveRemoteContext,
    task_id: String,
    complete: bool,
    invalid: bool,
    touched_at: Instant,
}

#[derive(Default)]
pub(super) struct ActionInner {
    pending: HashMap<String, PendingAction>,
    pub(super) uploads: HashMap<String, CompletedUpload>,
}

#[derive(Default)]
pub(crate) struct DesktopRemoteActionState {
    pub(super) inner: Mutex<ActionInner>,
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
        let Some(pending) = inner.pending.remove(request_id) else {
            return false;
        };
        let ok = payload.get("ok").and_then(Value::as_bool) == Some(true);
        let body = payload.get("body").cloned().unwrap_or(Value::Null);
        if ok && pending.operation == "attachment_begin" {
            let (Some(task_id), Some(transfer_id)) = (
                pending.task_id.as_deref(),
                body.get("transfer_id")
                    .and_then(Value::as_str)
                    .filter(|id| !id.is_empty()),
            ) else {
                let _ = pending.reply.send(Err("result_unknown".into()));
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
                        invalid: false,
                        touched_at: Instant::now(),
                    },
                );
            } else {
                // B has already accepted begin, but we cannot safely retain its
                // handle. The remote outcome must remain fenced.
                let _ = pending.reply.send(Err("result_unknown".into()));
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
                // 与 !ok 路径对称：绑定不匹配只拒绝本次回执，不得驱逐另一个
                // task 名下的存证。
                let _ = pending
                    .reply
                    .send(Err("remote_attachment_binding_mismatch".into()));
                return true;
            }
            let Some(upload) = inner.uploads.get_mut(transfer_id) else {
                let _ = pending
                    .reply
                    .send(Err("remote_attachment_binding_mismatch".into()));
                return true;
            };
            if upload.invalid {
                let _ = pending
                    .reply
                    .send(Err("remote_attachment_unconfirmed".into()));
                return true;
            }
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
                if bound && pending.operation == "attachment_chunk" {
                    // Retain only the authenticated binding for cleanup. An
                    // out-of-order successful reply cannot re-attest this upload.
                    if let Some(upload) = inner.uploads.get_mut(transfer_id) {
                        upload.complete = false;
                        upload.invalid = true;
                    }
                } else if bound && pending.operation == "attachment_abort" {
                    inner.uploads.remove(transfer_id);
                    // Only a correlated, authenticated B reply proves the handle
                    // is logically absent. Physical cleanup may still be pending.
                    if payload.get("error_code").and_then(Value::as_str) == Some("transfer_unknown")
                    {
                        let _ = pending
                            .reply
                            .send(Ok(json!({"aborted": false, "already_absent": true})));
                        return true;
                    }
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
        log_remote_action_event(
            "controller",
            "remote_action_result_received",
            &pending.operation,
            result.as_ref().map(|_| "ok").unwrap_or_else(|code| code),
        );
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
                || upload.invalid
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
