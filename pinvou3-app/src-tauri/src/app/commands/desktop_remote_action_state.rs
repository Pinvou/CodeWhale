//! Process-local action correlation and upload receipts.
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::relay_runtime::log_remote_action_event;
use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;
use parking_lot::Mutex;
use serde_json::{Value, json};
use sha2::{Digest as _, Sha256};
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};
use tokio::sync::oneshot;

const MAX_PENDING_ACTIONS: usize = 64;
pub(super) const MAX_UPLOAD_RECEIPTS: usize = 128;
const UPLOAD_RECEIPT_TTL: Duration = Duration::from_secs(10 * 60);
const MAX_ACTION_IDENTITIES: usize = 128;

struct ActionIdentity {
    request_id: String,
    live: LiveRemoteContext,
    busy: bool,
    attempt: Arc<()>,
}

#[derive(Debug)]
pub(crate) struct ActionIdentityError {
    pub(crate) code: &'static str,
    pub(crate) request_id: Option<String>,
}

impl From<&'static str> for ActionIdentityError {
    fn from(code: &'static str) -> Self {
        Self {
            code,
            request_id: None,
        }
    }
}

/// Native ownership survives WebView/bridge replacement. Dropping an awaiting
/// command proves no remote outcome: keep its identity until authenticated
/// settlement or an authoritative lease transition.
pub(crate) struct ActionIdentityReservation {
    state: Arc<DesktopRemoteActionState>,
    key: String,
    pub(crate) request_id: String,
    attempt: Arc<()>,
    was_new: bool,
    dispatched: AtomicBool,
}

impl ActionIdentityReservation {
    pub(crate) fn mark_dispatched(&self) {
        self.dispatched.store(true, Ordering::Release);
    }
    pub(crate) fn settle(&self, authoritative: bool) {
        let mut inner = self.state.inner.lock();
        if inner
            .identities
            .get(&self.key)
            .is_some_and(|entry| Arc::ptr_eq(&entry.attempt, &self.attempt))
        {
            if authoritative {
                inner.identities.remove(&self.key);
            } else if let Some(entry) = inner.identities.get_mut(&self.key) {
                entry.busy = false;
            }
        }
    }
}

impl Drop for ActionIdentityReservation {
    fn drop(&mut self) {
        self.settle(self.was_new && !self.dispatched.load(Ordering::Acquire));
    }
}

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
    identities: HashMap<String, ActionIdentity>,
}

#[derive(Default)]
pub(crate) struct DesktopRemoteActionState {
    pub(super) inner: Mutex<ActionInner>,
}

impl DesktopRemoteActionState {
    pub(crate) fn reserve_identity(
        self: &Arc<Self>,
        local_session_id: &str,
        remote_ref: &str,
        live: &LiveRemoteContext,
        operation: &str,
        body: &Value,
        proposed_id: &str,
    ) -> Result<ActionIdentityReservation, ActionIdentityError> {
        // Hash the exact canonical typed intent; never retain attachment bytes
        // or credentials in a second cache. Include task/chunk fields via body.
        let encoded = serde_json::to_vec(&json!([
            local_session_id,
            remote_ref,
            live.peer_id,
            live.connection_id,
            live.controller_lease_id,
            live.lease_epoch,
            live.relay_epoch,
            live.generation,
            operation,
            body
        ]))
        .map_err(|_| "remote_action_invalid")?;
        let key = Sha256::digest(&encoded)
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let mut inner = self.inner.lock();
        let attempt = Arc::new(());
        let was_new = !inner.identities.contains_key(&key);
        let request_id = if let Some(entry) = inner.identities.get_mut(&key) {
            if entry.busy {
                return Err(ActionIdentityError {
                    code: "remote_action_capacity",
                    request_id: Some(entry.request_id.clone()),
                });
            }
            if matches!(operation, "attachment_begin" | "attachment_chunk") {
                return Err(ActionIdentityError {
                    code: "result_unknown",
                    request_id: Some(entry.request_id.clone()),
                });
            }
            entry.busy = true;
            entry.attempt = Arc::clone(&attempt);
            entry.request_id.clone()
        } else {
            if inner.identities.len() >= MAX_ACTION_IDENTITIES {
                return Err("remote_action_capacity".into());
            }
            // An explicit ID cannot alias another unresolved intent.
            if inner
                .identities
                .values()
                .any(|entry| entry.request_id == proposed_id)
            {
                return Err("remote_action_invalid".into());
            }
            inner.identities.insert(
                key.clone(),
                ActionIdentity {
                    request_id: proposed_id.to_owned(),
                    live: live.clone(),
                    busy: true,
                    attempt: Arc::clone(&attempt),
                },
            );
            proposed_id.to_owned()
        };
        Ok(ActionIdentityReservation {
            state: Arc::clone(self),
            key,
            request_id,
            attempt,
            was_new,
            dispatched: AtomicBool::new(false),
        })
    }

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
        // Clearing waiters is not proof of a remote terminal outcome. A late
        // connect/disconnect cleanup may also run after a new owner exists.
        // Only purge_stale, called inside the current dispatch authority fence,
        // retires old identity records; never clear a new owner's unknown IDs.
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
        inner.identities.retain(|_, entry| {
            // A resume-boundary change alone must not mint a new remote action.
            entry.live.peer_id == live.peer_id
                && entry.live.connection_id == live.connection_id
                && entry.live.controller_lease_id == live.controller_lease_id
                && entry.live.lease_epoch == live.lease_epoch
                && entry.live.relay_epoch == live.relay_epoch
                && entry.live.generation == live.generation
        });
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

#[cfg(test)]
mod identity_tests {
    use super::*;

    fn live() -> LiveRemoteContext {
        LiveRemoteContext {
            peer_id: "peer".into(),
            resume_boundary_id: "resume".into(),
            connection_id: "connection".into(),
            controller_lease_id: "lease".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay".into(),
            generation: "generation".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        }
    }

    fn reserve(
        state: &Arc<DesktopRemoteActionState>,
        live: &LiveRemoteContext,
        task: usize,
        id: &str,
    ) -> Result<ActionIdentityReservation, &'static str> {
        state.reserve_identity("local", "remote", live, "permission_decide",
            &json!({"task_id":format!("task-{task}"),"permission_request_id":"permission","decision":"allow"}), id).map_err(|error| error.code)
    }

    #[test]
    fn unresolved_identity_capacity_never_evicts_and_reuses_original_after_renderer_loss() {
        let state = Arc::new(DesktopRemoteActionState::default());
        for task in 0..MAX_ACTION_IDENTITIES {
            let entry = reserve(&state, &live(), task, &format!("request-{task}")).unwrap();
            entry.mark_dispatched();
            drop(entry); // command cancelled/renderer lost, no authenticated outcome
        }
        assert!(matches!(
            reserve(&state, &live(), 128, "overflow"),
            Err("remote_action_capacity")
        ));
        let retry = reserve(&state, &live(), 0, "new-renderer-request").unwrap();
        assert_eq!(retry.request_id, "request-0");
        retry.mark_dispatched();
        retry.settle(true);
        drop(retry);
        let next = reserve(&state, &live(), 128, "next").unwrap();
        assert_eq!(next.request_id, "next");
    }

    #[test]
    fn unsent_new_reservations_release_but_unknown_retries_and_late_owners_do_not() {
        let state = Arc::new(DesktopRemoteActionState::default());
        drop(reserve(&state, &live(), 0, "unsent").unwrap());
        assert!(state.inner.lock().identities.is_empty());
        let old = reserve(&state, &live(), 0, "first").unwrap();
        assert!(matches!(
            reserve(&state, &live(), 0, "duplicate"),
            Err("remote_action_capacity")
        ));
        old.mark_dispatched();
        old.settle(false);
        let retry = reserve(&state, &live(), 0, "second").unwrap();
        assert_eq!(retry.request_id, "first");
        drop(old); // must not clear the newer in-flight owner
        assert!(matches!(
            reserve(&state, &live(), 0, "third"),
            Err("remote_action_capacity")
        ));
        drop(retry); // local retry failed before dispatch; original remains unknown
        let retry = reserve(&state, &live(), 0, "fourth").unwrap();
        assert_eq!(retry.request_id, "first");
        retry.settle(true);
        let next = reserve(&state, &live(), 0, "fresh").unwrap();
        drop(retry); // old terminal guard cannot unlock a new same-intent attempt
        assert_eq!(next.request_id, "fresh");
        assert!(matches!(
            reserve(&state, &live(), 0, "fifth"),
            Err("remote_action_capacity")
        ));
    }

    #[test]
    fn canonical_intent_and_full_lease_own_identity_without_persisting_content() {
        let state = Arc::new(DesktopRemoteActionState::default());
        let original = live();
        let body = json!({"task_id":"task","transfer_id":"transfer","chunk_index":0,"chunk_b64":"c2VjcmV0"});
        let entry = state
            .reserve_identity(
                "local",
                "remote",
                &original,
                "artifact_download_chunk",
                &body,
                "first",
            )
            .unwrap();
        entry.mark_dispatched();
        drop(entry);
        let mut resumed = original.clone();
        resumed.resume_boundary_id = "resumed".into();
        state.purge_stale(&resumed);
        let entry = state
            .reserve_identity(
                "local",
                "remote",
                &resumed,
                "artifact_download_chunk",
                &body,
                "reload",
            )
            .unwrap();
        assert_eq!(entry.request_id, "first");
        drop(entry);
        let changed = json!({"task_id":"task","transfer_id":"transfer","chunk_index":0,"chunk_b64":"b3RoZXI="});
        assert!(matches!(
            state.reserve_identity(
                "local",
                "remote",
                &resumed,
                "artifact_download_chunk",
                &changed,
                "first"
            ),
            Err(ActionIdentityError {
                code: "remote_action_invalid",
                ..
            })
        ));
        let changed_entry = state
            .reserve_identity(
                "local",
                "remote",
                &resumed,
                "artifact_download_chunk",
                &changed,
                "other-content",
            )
            .unwrap();
        assert_eq!(changed_entry.request_id, "other-content");
        for key in state.inner.lock().identities.keys() {
            assert_eq!(key.len(), 64);
            assert!(!key.contains("secret"));
        }
        drop(changed_entry);
        for field in 0..5 {
            let mut next = original.clone();
            match field {
                0 => next.connection_id.push('2'),
                1 => next.controller_lease_id.push('2'),
                2 => next.lease_epoch.push('2'),
                3 => next.relay_epoch.push('2'),
                _ => next.generation.push('2'),
            }
            state.purge_stale(&next);
            assert!(state.inner.lock().identities.is_empty());
            let entry = state
                .reserve_identity(
                    "local",
                    "remote",
                    &next,
                    "artifact_download_chunk",
                    &body,
                    "next-lease",
                )
                .unwrap();
            assert_eq!(entry.request_id, "next-lease");
            drop(entry);
        }
    }

    #[test]
    fn old_authority_and_late_cleanup_cannot_retire_new_unknown_identity() {
        use crate::features::remote_control::controller::{
            controlling::{ControllerLease, ControllingDeviceManager},
            crypto::ControllerRole,
            encrypted_lane::ControllingLaneState,
            test_support::{
                bind_pairing_generation, lease_context, paired_session_keys, pairing_fixture,
            },
        };
        fn activate(
            manager: &ControllingDeviceManager,
            lane: &ControllingLaneState,
            generation: &str,
        ) -> LiveRemoteContext {
            let mut fixture = pairing_fixture("identity-owner-fence");
            bind_pairing_generation(&mut fixture, generation);
            let context = lease_context(&fixture.reservation);
            let (keys, _) =
                paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
            let operation = manager.connect("2345678").unwrap();
            manager.complete_reserved(operation.clone()).unwrap();
            manager
                .complete_connected(
                    operation,
                    ControllerLease {
                        context: context.clone(),
                        peer_endpoint_id: "endpoint-0001".into(),
                        reservation_id: "reservation-0001".into(),
                        device_display_name: "test".into(),
                        liveness_epoch_a: "live-a-1".into(),
                        liveness_epoch_b: "live-b-1".into(),
                        resume_deadline_b: 30000,
                        resume_token: [1; 32],
                        transport_key_fingerprint: [2; 32],
                        negotiated_capabilities: vec![
                            "encrypted_frames_v1".into(),
                            "pairing_v1".into(),
                            "task_v1".into(),
                        ],
                    },
                )
                .unwrap();
            lane.install(ControllerRole::A, &context, keys).unwrap();
            let identity = manager.authenticated_remote_proxy_identity().unwrap();
            LiveRemoteContext {
                peer_id: identity.1,
                resume_boundary_id: identity.2,
                connection_id: context.connection_id,
                controller_lease_id: context.controller_lease_id,
                lease_epoch: context.lease_epoch,
                relay_epoch: context.relay_epoch,
                generation: context.remote_control_generation,
                control_connected: true,
                encrypted_channel_connected: true,
            }
        }
        let state = Arc::new(DesktopRemoteActionState::default());
        let manager = ControllingDeviceManager::default();
        let lane = ControllingLaneState::default();
        let old = activate(&manager, &lane, "old-generation");
        let expected = manager.authenticated_remote_proxy_identity().unwrap();
        let (release, wait) = std::sync::mpsc::sync_channel(0);
        let ran_old_mutation = AtomicBool::new(false);
        std::thread::scope(|scope| {
            let old_manager = &manager;
            let old_lane = &lane;
            let old_state = &state;
            let old_ran = &ran_old_mutation;
            let old_call = scope.spawn(move || {
                wait.recv_timeout(Duration::from_secs(10)).unwrap();
                old_manager.with_current_authenticated_remote_proxy_identity(
                    old_lane,
                    &expected,
                    || {
                        old_ran.store(true, Ordering::SeqCst);
                        old_state.purge_stale(&old);
                        Ok(())
                    },
                )
            });
            manager.disconnect();
            let current = activate(&manager, &lane, "new-generation");
            let identity = manager.authenticated_remote_proxy_identity().unwrap();
            manager
                .with_current_authenticated_remote_proxy_identity(&lane, &identity, || {
                    state.purge_stale(&current);
                    let request = reserve(&state, &current, 0, "new-owner-original").unwrap();
                    request.mark_dispatched();
                    drop(request);
                    Ok(())
                })
                .unwrap();
            release.send(()).unwrap();
            assert!(old_call.join().unwrap().is_err());
            assert!(!ran_old_mutation.load(Ordering::SeqCst));
            // A delayed connect cleanup is allowed to drop waiters, never this
            // new owner's result-unknown identity.
            state.clear_all();
            let retry = reserve(&state, &current, 0, "new-renderer-candidate").unwrap();
            assert_eq!(retry.request_id, "new-owner-original");
        });
    }

    #[test]
    fn unknown_attachment_remains_fenced_until_native_authority_changes() {
        let state = Arc::new(DesktopRemoteActionState::default());
        let body = json!({"task_id":"task","file_name":"a.txt"});
        let entry = state
            .reserve_identity(
                "local",
                "remote",
                &live(),
                "attachment_begin",
                &body,
                "first",
            )
            .unwrap();
        entry.mark_dispatched();
        drop(entry);
        assert!(matches!(
            state.reserve_identity(
                "local",
                "remote",
                &live(),
                "attachment_begin",
                &body,
                "fresh"
            ),
            Err(ActionIdentityError { code: "result_unknown", request_id: Some(ref id) }) if id == "first"
        ));
        // Cleanup remains reconcilable; it cannot create another attachment.
        let abort = state
            .reserve_identity(
                "local",
                "remote",
                &live(),
                "attachment_abort",
                &json!({"task_id":"task","transfer_id":"transfer"}),
                "abort-original",
            )
            .unwrap();
        abort.mark_dispatched();
        drop(abort);
        let retry = state
            .reserve_identity(
                "local",
                "remote",
                &live(),
                "attachment_abort",
                &json!({"task_id":"task","transfer_id":"transfer"}),
                "abort-new-renderer",
            )
            .unwrap();
        assert_eq!(retry.request_id, "abort-original");
        retry.settle(true);
        drop(retry);
        state.clear_all();
        assert!(matches!(
            state.reserve_identity(
                "local",
                "remote",
                &live(),
                "attachment_begin",
                &body,
                "after-waiter-reset"
            ),
            Err(ActionIdentityError {
                code: "result_unknown",
                ..
            })
        ));
        let mut next = live();
        next.generation = "next-generation".into();
        state.purge_stale(&next);
        let entry = state
            .reserve_identity("local", "remote", &next, "attachment_begin", &body, "fresh")
            .unwrap();
        assert_eq!(entry.request_id, "fresh");
    }
}
