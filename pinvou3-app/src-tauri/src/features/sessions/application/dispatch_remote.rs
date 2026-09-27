//! A-side remote task submission. Durable authority is the SessionStore v3
//! RemoteProxy record; the encrypted payload exists only in this process.

use std::collections::HashMap;
use std::sync::Arc;

use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::ports::{UseCaseError, UseCaseFuture};
use crate::features::sessions::SessionStore;
use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;
use crate::features::sessions::remote_proxy::outbox::{
    PreparedOutboxEntry, RemoteRequestSemantics, model_execution_digest, request_fingerprint,
};
use crate::features::sessions::remote_proxy::schema::{
    PendingDispatchState, PendingRemoteRequest, PendingReservation, valid_remote_identifier,
};

const MAX_EPHEMERAL_PACKAGES: usize = 256;

pub(crate) trait DispatchRemoteUseCase: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn dispatch(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response>;
}

/// The host prepares the sensitive DEV-15 wire payload and sends it only
/// after SessionStore confirms the durable dispatch-attempt barrier.
pub(crate) trait RemoteTaskDispatchPort: Send + Sync {
    fn prepare(&self, request: &RemoteDispatchRequest) -> Result<PreparedRemoteTask, UseCaseError>;
    fn send_encrypted(&self, payload: &Value) -> Result<(), UseCaseError>;

    /// Fault-injection seam; the production adapter uses the no-op default.
    fn after_persist(&self) -> Result<(), UseCaseError> {
        Ok(())
    }
}

/// One trusted prepare result binds the selected model profile to the exact
/// sensitive wire payload. Neither field is persisted as raw data; only the
/// profile ID and the package's non-secret route digest enter the fingerprint.
pub(crate) struct PreparedRemoteTask {
    pub(crate) model_profile_id: Option<String>,
    pub(crate) payload: Value,
}

pub(crate) struct RemoteDispatchRequest {
    pub(crate) task_id: String,
    pub(crate) client_request_id: String,
    pub(crate) live: LiveRemoteContext,
    pub(crate) semantics: RemoteRequestSemantics,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum RemoteDispatchState {
    Queued,
    PreparedNotSent,
    OutcomeUnknown,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct RemoteDispatchReport {
    pub(crate) retry_handle: RemoteRetryHandle,
    pub(crate) state: RemoteDispatchState,
}

/// Opaque correlation handle for replaying only the original in-process
/// payload. It carries no task semantics, credentials or temporary package.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct RemoteRetryHandle {
    pub(crate) task_id: String,
    pub(crate) client_request_id: String,
    pub(crate) request_fingerprint: String,
}

impl RemoteRetryHandle {
    fn report(&self, state: RemoteDispatchState) -> RemoteDispatchReport {
        RemoteDispatchReport {
            retry_handle: self.clone(),
            state,
        }
    }
}

/// DEV-30 can authenticate a B terminal event before handing it to this
/// storage-only reconciliation boundary. No Tauri event or projection is
/// emitted here, and callers cannot construct a verified result directly.
pub(crate) struct RemoteTerminalCandidate {
    pub(crate) local_session_id: String,
    pub(crate) remote_session_ref: String,
    pub(crate) controlled_session_id: String,
    pub(crate) task_id: String,
    pub(crate) client_request_id: String,
    pub(crate) request_fingerprint: String,
    pub(crate) authenticated_event_id: String,
    pub(crate) live: LiveRemoteContext,
}

pub(crate) trait RemoteTerminalVerificationPort: Send + Sync {
    fn verify_terminal(&self, candidate: &RemoteTerminalCandidate) -> Result<(), UseCaseError>;
}

pub(crate) struct VerifiedRemoteTerminal {
    candidate: RemoteTerminalCandidate,
}

impl VerifiedRemoteTerminal {
    pub(crate) fn verify(
        candidate: RemoteTerminalCandidate,
        verifier: &dyn RemoteTerminalVerificationPort,
    ) -> Result<Self, UseCaseError> {
        if candidate.authenticated_event_id.is_empty()
            || !valid_remote_identifier(&candidate.task_id)
            || !valid_remote_identifier(&candidate.client_request_id)
            || candidate.request_fingerprint.len() != 64
            || !candidate
                .request_fingerprint
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        {
            return Err(error("remote_terminal_invalid"));
        }
        verifier.verify_terminal(&candidate)?;
        Ok(Self { candidate })
    }

    /// Transfer the already verified terminal binding to a single projection
    /// or reconciliation consumer. The proof cannot be cloned or reconstructed
    /// from its fields, so this consumes it exactly once.
    pub(crate) fn into_candidate(self) -> RemoteTerminalCandidate {
        self.candidate
    }
}

type OutboxKey = (String, String, String);

pub(crate) struct RemoteDispatchService {
    store: SessionStore,
    port: Arc<dyn RemoteTaskDispatchPort>,
    /// Sensitive, strictly in-process cache; never a second durable ledger.
    ephemeral: Mutex<HashMap<OutboxKey, PreparedOutboxEntry>>,
}

impl RemoteDispatchService {
    pub(crate) fn new(store: SessionStore, port: Arc<dyn RemoteTaskDispatchPort>) -> Self {
        Self {
            store,
            port,
            ephemeral: Mutex::new(HashMap::new()),
        }
    }

    /// Atomically retire correlation IDs only after a separately authenticated
    /// B terminal event is bound to the exact current A/B session and attempt.
    pub(crate) fn reconcile_terminal(
        &self,
        verified: VerifiedRemoteTerminal,
    ) -> Result<(), UseCaseError> {
        let terminal = verified.into_candidate();
        let live = self
            .store
            .remote_dispatch_live_context(&terminal.local_session_id, &terminal.remote_session_ref)
            .map_err(|_| error("remote_binding_mismatch"))?;
        if live != terminal.live {
            return Err(error("remote_binding_mismatch"));
        }
        let record = self
            .store
            .remote_proxy_snapshot(&terminal.local_session_id)
            .ok_or_else(|| error("remote_proxy_not_found"))?;
        if record.remote_session_ref != terminal.remote_session_ref
            || record.controlled_session_id.as_deref()
                != Some(terminal.controlled_session_id.as_str())
            || record.peer_id != live.peer_id
        {
            return Err(error("remote_terminal_mapping_mismatch"));
        }
        let pending = record
            .pending
            .iter()
            .find(|item| {
                item.task_id == terminal.task_id
                    && item.client_request_id == terminal.client_request_id
            })
            .ok_or_else(|| error("remote_pending_not_found"))?;
        if pending.request_fingerprint != terminal.request_fingerprint
            || pending.dispatch_state != PendingDispatchState::AttemptMayHaveOccurred
        {
            return Err(error("remote_terminal_correlation_mismatch"));
        }
        self.store
            .retire_remote_pending_terminal(
                &terminal.local_session_id,
                &terminal.remote_session_ref,
                &terminal.live,
                &terminal.controlled_session_id,
                &terminal.client_request_id,
                &terminal.task_id,
                &terminal.request_fingerprint,
            )
            .map_err(|_| error("remote_terminal_persist_failed"))
    }

    pub(crate) fn dispatch_new(
        &self,
        mut request: RemoteDispatchRequest,
    ) -> Result<RemoteDispatchReport, UseCaseError> {
        if !valid_remote_identifier(&request.task_id)
            || !valid_remote_identifier(&request.client_request_id)
        {
            return Err(error("remote_request_id_invalid"));
        }
        let semantics = &request.semantics;
        if semantics.text.trim().is_empty()
            || semantics.text.trim() != semantics.text
            || semantics.local_session_id.is_empty()
            || semantics.remote_session_ref.is_empty()
            || semantics.session_id.is_empty()
            || semantics.mode.is_empty()
            || semantics.model_source.is_empty()
            || semantics.input_digest.len() != 64
            || match semantics.model_source.as_str() {
                "controller_temporary" => semantics
                    .model_profile_id
                    .as_deref()
                    .is_none_or(|id| !valid_remote_identifier(id)),
                "controlled_device" => semantics.model_profile_id.is_some(),
                _ => true,
            }
            || !semantics.model_execution_digest.is_empty()
        {
            return Err(error("remote_request_invalid"));
        }
        // Serializes same-ID calls in this service. SessionStore independently
        // serializes durable mutations across all service instances.
        let mut ephemeral = self.ephemeral.lock();
        let record = self
            .store
            .remote_proxy_snapshot(&semantics.local_session_id)
            .ok_or_else(|| error("remote_proxy_not_found"))?;
        if record.remote_session_ref != semantics.remote_session_ref
            || record.controlled_session_id.as_deref() != Some(&semantics.controlled_session_id)
            || record.peer_id != request.live.peer_id
        {
            return Err(error("remote_binding_mismatch"));
        }
        let trusted_live = self
            .store
            .remote_dispatch_live_context(
                &semantics.local_session_id,
                &semantics.remote_session_ref,
            )
            .map_err(|_| error("remote_binding_mismatch"))?;
        if trusted_live != request.live {
            return Err(error("remote_binding_mismatch"));
        }
        let key = (
            semantics.local_session_id.clone(),
            request.client_request_id.clone(),
            request.task_id.clone(),
        );
        if record.retired_requests.iter().any(|item| {
            item.client_request_id == request.client_request_id || item.task_id == request.task_id
        }) {
            ephemeral.remove(&key);
            return Err(error("remote_request_id_retired"));
        }
        let existing = record.pending.iter().find(|item| {
            item.client_request_id == request.client_request_id || item.task_id == request.task_id
        });
        if existing.is_some() {
            return Err(error("remote_request_id_exists"));
        }
        if ephemeral.len() >= MAX_EPHEMERAL_PACKAGES {
            return Err(error("remote_outbox_capacity"));
        }
        // DEV-15 performs the only package build. Nothing can be persisted or
        // sent until the exact returned wire payload is validated.
        let prepared = self.port.prepare(&request)?;
        if prepared.model_profile_id != request.semantics.model_profile_id {
            return Err(error("remote_model_profile_mismatch"));
        }
        request.semantics.model_profile_id = prepared.model_profile_id;
        let payload = prepared.payload;
        validate_prepared_payload(&payload, &request)?;
        request.semantics.model_execution_digest =
            model_execution_digest(payload.get("temporary_model_package"))
                .map_err(|_| error("remote_model_semantics_invalid"))?;
        let semantics = &request.semantics;
        let fingerprint = request_fingerprint(&request.semantics, &request.live)
            .map_err(|_| error("remote_fingerprint_failed"))?;
        let handle = RemoteRetryHandle {
            task_id: request.task_id.clone(),
            client_request_id: request.client_request_id.clone(),
            request_fingerprint: fingerprint.clone(),
        };
        let pending = PendingRemoteRequest {
            client_request_id: handle.client_request_id.clone(),
            task_id: handle.task_id.clone(),
            request_fingerprint: fingerprint.clone(),
            dispatch_state: PendingDispatchState::PreparedNeverAttempted,
        };
        match self
            .store
            .insert_remote_pending(
                &semantics.local_session_id,
                &semantics.remote_session_ref,
                &request.live,
                pending,
            )
            .map_err(|_| error("remote_pending_persist_failed"))?
        {
            PendingReservation::Inserted => {
                ephemeral.insert(
                    key,
                    PreparedOutboxEntry {
                        fingerprint,
                        context: request.live.clone(),
                        wire_payload: payload,
                    },
                );
            }
            PendingReservation::Existing(_) => return Err(error("remote_request_id_exists")),
        }
        self.advance_prepared(
            &semantics.local_session_id,
            &semantics.remote_session_ref,
            &request.live,
            &handle,
            &mut ephemeral,
        )
    }

    /// Retry has no task semantics. It can only replay the original cached
    /// bytes, after matching a durable pending fingerprint and current binding.
    pub(crate) fn dispatch_retry(
        &self,
        local_session_id: &str,
        remote_session_ref: &str,
        handle: RemoteRetryHandle,
    ) -> Result<RemoteDispatchReport, UseCaseError> {
        if !valid_remote_identifier(&handle.task_id)
            || !valid_remote_identifier(&handle.client_request_id)
            || !valid_fingerprint(&handle.request_fingerprint)
        {
            return Err(error("remote_retry_handle_invalid"));
        }
        let mut ephemeral = self.ephemeral.lock();
        let record = self
            .store
            .remote_proxy_snapshot(local_session_id)
            .ok_or_else(|| error("remote_proxy_not_found"))?;
        if record.remote_session_ref != remote_session_ref {
            return Err(error("remote_binding_mismatch"));
        }
        let live = self
            .store
            .remote_dispatch_live_context(local_session_id, remote_session_ref)
            .map_err(|_| error("remote_binding_mismatch"))?;
        if record.peer_id != live.peer_id {
            return Err(error("remote_binding_mismatch"));
        }
        let key = (
            local_session_id.to_owned(),
            handle.client_request_id.clone(),
            handle.task_id.clone(),
        );
        if record.retired_requests.iter().any(|item| {
            item.client_request_id == handle.client_request_id || item.task_id == handle.task_id
        }) {
            ephemeral.remove(&key);
            return Err(error("remote_request_id_retired"));
        }
        let pending = record
            .pending
            .iter()
            .find(|item| {
                item.client_request_id == handle.client_request_id || item.task_id == handle.task_id
            })
            .ok_or_else(|| error("remote_retry_not_pending"))?;
        if pending.client_request_id != handle.client_request_id
            || pending.task_id != handle.task_id
            || pending.request_fingerprint != handle.request_fingerprint
        {
            return Err(error("remote_request_id_conflict"));
        }
        if pending.dispatch_state == PendingDispatchState::AttemptMayHaveOccurred {
            ephemeral.remove(&key);
            return Ok(handle.report(RemoteDispatchState::OutcomeUnknown));
        }
        if !ephemeral.get(&key).is_some_and(|cached| {
            cached.context == live && cached.fingerprint == handle.request_fingerprint
        }) {
            return Err(error("remote_payload_unavailable"));
        }
        self.advance_prepared(
            local_session_id,
            remote_session_ref,
            &live,
            &handle,
            &mut ephemeral,
        )
    }

    fn advance_prepared(
        &self,
        local_session_id: &str,
        remote_session_ref: &str,
        live: &LiveRemoteContext,
        handle: &RemoteRetryHandle,
        ephemeral: &mut HashMap<OutboxKey, PreparedOutboxEntry>,
    ) -> Result<RemoteDispatchReport, UseCaseError> {
        if self.port.after_persist().is_err() {
            return Ok(handle.report(RemoteDispatchState::PreparedNotSent));
        }
        if self
            .store
            .mark_remote_dispatch_attempt(
                local_session_id,
                remote_session_ref,
                live,
                &handle.client_request_id,
                &handle.task_id,
                &handle.request_fingerprint,
            )
            .is_err()
        {
            return Ok(handle.report(RemoteDispatchState::PreparedNotSent));
        }
        // No subsequent error can justify an automatic resend. Moving out of
        // the cache also ensures secrets are not retained for retry.
        let payload = ephemeral
            .remove(&(
                local_session_id.to_owned(),
                handle.client_request_id.clone(),
                handle.task_id.clone(),
            ))
            .expect("prepared entry after barrier")
            .wire_payload;
        let result = self.port.send_encrypted(&payload);
        drop(payload);
        Ok(handle.report(if result.is_ok() {
            RemoteDispatchState::Queued
        } else {
            RemoteDispatchState::OutcomeUnknown
        }))
    }

    /// Only a genuinely lost ephemeral payload may take this path. A durable
    /// `PreparedNeverAttempted` record is the proof that sending never began.
    pub(crate) fn abandon_lost_payload(
        &self,
        local_session_id: &str,
        client_request_id: &str,
        task_id: &str,
    ) -> Result<(), UseCaseError> {
        let key = (
            local_session_id.to_owned(),
            client_request_id.to_owned(),
            task_id.to_owned(),
        );
        let ephemeral = self.ephemeral.lock();
        if ephemeral.contains_key(&key) {
            return Err(error("remote_payload_still_available"));
        }
        let record = self
            .store
            .remote_proxy_snapshot(local_session_id)
            .ok_or_else(|| error("remote_proxy_not_found"))?;
        let pending = record
            .pending
            .iter()
            .find(|item| item.client_request_id == client_request_id && item.task_id == task_id)
            .ok_or_else(|| error("remote_pending_not_found"))?;
        if pending.dispatch_state != PendingDispatchState::PreparedNeverAttempted {
            return Err(error("remote_outcome_unknown"));
        }
        self.store
            .abandon_unattempted_remote_pending(
                local_session_id,
                client_request_id,
                task_id,
                &pending.request_fingerprint,
            )
            .map_err(|_| error("remote_pending_abandon_failed"))
    }
}

fn error(code: &'static str) -> UseCaseError {
    UseCaseError::new(code, false)
}

fn valid_fingerprint(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn validate_prepared_payload(
    payload: &Value,
    request: &RemoteDispatchRequest,
) -> Result<(), UseCaseError> {
    let semantics = &request.semantics;
    let live = &request.live;
    let input = payload
        .get("input")
        .ok_or_else(|| error("remote_prepare_mismatch"))?;
    let revision = semantics.capability_revision.to_string();
    let matches = payload.get("v") == Some(&Value::from(1))
        && payload.get("type").and_then(Value::as_str) == Some("task_submit")
        && payload
            .get("request_id")
            .and_then(Value::as_str)
            .is_some_and(valid_remote_identifier)
        && payload.get("task_id").and_then(Value::as_str) == Some(request.task_id.as_str())
        && payload.get("client_request_id").and_then(Value::as_str)
            == Some(request.client_request_id.as_str())
        && payload.get("session_id").and_then(Value::as_str) == Some(semantics.session_id.as_str())
        && payload.get("input_digest").and_then(Value::as_str)
            == Some(semantics.input_digest.as_str())
        && payload.get("capability_revision").and_then(Value::as_str) == Some(revision.as_str())
        && payload.get("mode").and_then(Value::as_str) == Some(semantics.mode.as_str())
        && payload.get("model_source").and_then(Value::as_str)
            == Some(semantics.model_source.as_str())
        && payload.get("connection_id").and_then(Value::as_str)
            == Some(live.connection_id.as_str())
        && payload.get("controller_lease_id").and_then(Value::as_str)
            == Some(live.controller_lease_id.as_str())
        && payload.get("lease_epoch").and_then(Value::as_str) == Some(live.lease_epoch.as_str())
        && payload.get("relay_epoch").and_then(Value::as_str) == Some(live.relay_epoch.as_str())
        && payload
            .get("remote_control_generation")
            .and_then(Value::as_str)
            == Some(live.generation.as_str())
        && input.get("text").and_then(Value::as_str) == Some(semantics.text.as_str())
        && input.get("attachment_ids") == Some(&serde_json::json!(semantics.attachment_ids))
        && input.get("skill_ids") == Some(&serde_json::json!(semantics.skill_ids));
    if !matches {
        return Err(error("remote_prepare_mismatch"));
    }
    let package = payload.get("temporary_model_package");
    let valid_model_binding = match semantics.model_source.as_str() {
        "controlled_device" => package.is_none() && semantics.model_profile_id.is_none(),
        "controller_temporary" => {
            package.is_some()
                && semantics
                    .model_profile_id
                    .as_deref()
                    .is_some_and(|id| !id.is_empty() && id.trim() == id)
        }
        _ => false,
    };
    if !valid_model_binding {
        return Err(error("remote_prepare_mismatch"));
    }
    if package.is_some_and(|package| {
        let binding = package.get("binding");
        binding
            .and_then(|binding| binding.get("task_id"))
            .and_then(Value::as_str)
            != Some(request.task_id.as_str())
            || binding
                .and_then(|binding| binding.get("connection_id"))
                .and_then(Value::as_str)
                != Some(live.connection_id.as_str())
            || binding
                .and_then(|binding| binding.get("controller_lease_id"))
                .and_then(Value::as_str)
                != Some(live.controller_lease_id.as_str())
            || binding
                .and_then(|binding| binding.get("lease_epoch"))
                .and_then(Value::as_str)
                != Some(live.lease_epoch.as_str())
    }) {
        return Err(error("remote_prepare_mismatch"));
    }
    Ok(())
}

impl DispatchRemoteUseCase for RemoteDispatchService {
    type Request = RemoteDispatchRequest;
    type Response = RemoteDispatchReport;

    fn dispatch(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response> {
        Box::pin(async move { self.dispatch_new(request) })
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    use super::*;
    use crate::features::sessions::remote_proxy::binding::AuthenticatedMapping;

    struct FakePort {
        store: SessionStore,
        local_id: String,
        prepare_count: AtomicUsize,
        package_build_count: AtomicUsize,
        send_count: AtomicUsize,
        credential_available: AtomicBool,
        fail_prepare: AtomicBool,
        fail_after_persist: AtomicBool,
        lose_ack: AtomicBool,
        model_route: Mutex<Value>,
        sent_route: Mutex<Option<Value>>,
        tamper_field: Mutex<Option<&'static str>>,
    }

    impl FakePort {
        fn new(store: SessionStore, local_id: String) -> Self {
            Self {
                store,
                local_id,
                prepare_count: AtomicUsize::new(0),
                package_build_count: AtomicUsize::new(0),
                send_count: AtomicUsize::new(0),
                credential_available: AtomicBool::new(true),
                fail_prepare: AtomicBool::new(false),
                fail_after_persist: AtomicBool::new(false),
                lose_ack: AtomicBool::new(false),
                model_route: Mutex::new(serde_json::json!({
                    "preset": "openai", "wire_api": "chat_completions",
                    "base_url": "https://example.test/v1", "model": "model-a",
                    "display_name": "Model A", "image_capability_override": "enabled"
                })),
                sent_route: Mutex::new(None),
                tamper_field: Mutex::new(None),
            }
        }

        fn package(&self, request: &RemoteDispatchRequest) -> Value {
            self.package_build_count.fetch_add(1, Ordering::SeqCst);
            serde_json::json!({
                "schema": "temporary_model_package/v1",
                "package_id": "random-package-id",
                "binding": {
                    "task_id": request.task_id,
                    "connection_id": request.live.connection_id,
                    "controller_lease_id": request.live.controller_lease_id,
                    "lease_epoch": request.live.lease_epoch,
                },
                "primary": self.model_route.lock().clone(),
                "primary_credential": {"kind": "api_key", "secret": "sk-never-persist-this"}
            })
        }
    }

    impl RemoteTaskDispatchPort for FakePort {
        fn prepare(
            &self,
            request: &RemoteDispatchRequest,
        ) -> Result<PreparedRemoteTask, UseCaseError> {
            self.prepare_count.fetch_add(1, Ordering::SeqCst);
            if self.fail_prepare.load(Ordering::SeqCst)
                || !self.credential_available.load(Ordering::SeqCst)
            {
                return Err(error("prepare_failed"));
            }
            let mut payload = serde_json::json!({
                "v": 1,
                "type": "task_submit",
                "request_id": "request-wire-1",
                "connection_id": request.live.connection_id,
                "controller_lease_id": request.live.controller_lease_id,
                "lease_epoch": request.live.lease_epoch,
                "relay_epoch": request.live.relay_epoch,
                "remote_control_generation": request.live.generation,
                "task_id": request.task_id,
                "client_request_id": request.client_request_id,
                "session_id": request.semantics.session_id,
                "capability_revision": request.semantics.capability_revision.to_string(),
                "model_source": request.semantics.model_source,
                "mode": request.semantics.mode,
                "input_digest": request.semantics.input_digest,
                "input": {
                    "text": request.semantics.text,
                    "attachment_ids": request.semantics.attachment_ids,
                    "skill_ids": request.semantics.skill_ids,
                },
            });
            let model_profile_id = if request.semantics.model_source == "controller_temporary" {
                payload["temporary_model_package"] = self.package(request);
                Some("model-1".into())
            } else {
                None
            };
            if let Some(field) = *self.tamper_field.lock() {
                payload[field] = "forged".into();
            }
            Ok(PreparedRemoteTask {
                model_profile_id,
                payload,
            })
        }

        fn send_encrypted(&self, payload: &Value) -> Result<(), UseCaseError> {
            let record = self.store.remote_proxy_snapshot(&self.local_id).unwrap();
            let pending = &record.pending[0];
            assert_eq!(
                pending.dispatch_state,
                PendingDispatchState::AttemptMayHaveOccurred
            );
            assert_eq!(payload["task_id"], pending.task_id);
            let durable =
                std::fs::read_to_string(self.store.remote_proxies.lock().path.clone()).unwrap();
            assert!(!durable.contains("sk-never-persist-this"));
            assert!(!durable.contains("temporary_model_package"));
            *self.sent_route.lock() = payload.pointer("/temporary_model_package/primary").cloned();
            self.send_count.fetch_add(1, Ordering::SeqCst);
            if self.lose_ack.load(Ordering::SeqCst) {
                Err(error("ack_lost"))
            } else {
                Ok(())
            }
        }

        fn after_persist(&self) -> Result<(), UseCaseError> {
            if self.fail_after_persist.load(Ordering::SeqCst) {
                Err(error("injected_after_persist"))
            } else {
                Ok(())
            }
        }
    }

    fn live() -> LiveRemoteContext {
        LiveRemoteContext {
            peer_id: "peer-1".into(),
            resume_boundary_id: "resume-1".into(),
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "epoch-1".into(),
            relay_epoch: "relay-1".into(),
            generation: "generation-1".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        }
    }

    fn setup() -> (tempfile::TempDir, SessionStore, RemoteDispatchRequest) {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let opened = store.open_remote_proxy(None, &live).unwrap();
        store
            .confirm_remote_mapping(
                &opened.local_session_id,
                &live,
                &AuthenticatedMapping {
                    remote_session_ref: opened.remote_session_ref.clone(),
                    controlled_session_id: "b-session-1".into(),
                },
            )
            .unwrap();
        let request = RemoteDispatchRequest {
            task_id: "task-1".into(),
            client_request_id: "request-1".into(),
            live,
            semantics: RemoteRequestSemantics {
                local_session_id: opened.local_session_id,
                remote_session_ref: opened.remote_session_ref,
                controlled_session_id: "b-session-1".into(),
                session_id: "wire-session-1".into(),
                capability_revision: 3,
                mode: "chat".into(),
                model_source: "controller_temporary".into(),
                model_profile_id: Some("model-1".into()),
                model_execution_digest: String::new(),
                text: "hello".into(),
                input_digest: "a".repeat(64),
                attachment_ids: Vec::new(),
                skill_ids: Vec::new(),
                workspace_ids: Vec::new(),
            },
        };
        (temp, store, request)
    }

    fn same_request(request: &RemoteDispatchRequest) -> RemoteDispatchRequest {
        RemoteDispatchRequest {
            task_id: request.task_id.clone(),
            client_request_id: request.client_request_id.clone(),
            live: request.live.clone(),
            semantics: request.semantics.clone(),
        }
    }

    #[test]
    fn selected_profile_mismatch_fails_before_outbox_or_send() {
        let (_temp, store, mut request) = setup();
        let local_id = request.semantics.local_session_id.clone();
        let port = Arc::new(FakePort::new(store.clone(), local_id.clone()));
        let service = RemoteDispatchService::new(store.clone(), port.clone());
        request.semantics.model_profile_id = Some("different-model".into());
        let error = service.dispatch_new(request).err().unwrap();
        assert_eq!(error.code(), "remote_model_profile_mismatch");
        assert!(
            store
                .remote_proxy_snapshot(&local_id)
                .unwrap()
                .pending
                .is_empty()
        );
        assert_eq!(port.prepare_count.load(Ordering::SeqCst), 1);
        assert_eq!(port.send_count.load(Ordering::SeqCst), 0);
    }

    fn handle(store: &SessionStore, request: &RemoteDispatchRequest) -> RemoteRetryHandle {
        let record = store
            .remote_proxy_snapshot(&request.semantics.local_session_id)
            .unwrap();
        let pending = record
            .pending
            .iter()
            .find(|item| {
                item.task_id == request.task_id
                    && item.client_request_id == request.client_request_id
            })
            .unwrap();
        RemoteRetryHandle {
            task_id: request.task_id.clone(),
            client_request_id: request.client_request_id.clone(),
            request_fingerprint: pending.request_fingerprint.clone(),
        }
    }

    fn retry(
        service: &RemoteDispatchService,
        store: &SessionStore,
        request: &RemoteDispatchRequest,
    ) -> Result<RemoteDispatchReport, UseCaseError> {
        service.dispatch_retry(
            &request.semantics.local_session_id,
            &request.semantics.remote_session_ref,
            handle(store, request),
        )
    }

    #[test]
    fn prepare_and_persist_failures_never_send_or_leave_false_pending() {
        let (temp, store, request) = setup();
        let port = Arc::new(FakePort::new(
            store.clone(),
            request.semantics.local_session_id.clone(),
        ));
        let service = RemoteDispatchService::new(store.clone(), port.clone());
        port.fail_prepare.store(true, Ordering::SeqCst);
        assert_eq!(
            service
                .dispatch_new(same_request(&request))
                .unwrap_err()
                .code(),
            "prepare_failed"
        );
        assert!(
            store
                .remote_proxy_snapshot(&request.semantics.local_session_id)
                .unwrap()
                .pending
                .is_empty()
        );
        port.fail_prepare.store(false, Ordering::SeqCst);
        let original_path = {
            let mut state = store.remote_proxies.lock();
            std::mem::replace(
                &mut state.path,
                temp.path().join("missing-parent/remote.json"),
            )
        };
        assert_eq!(
            service
                .dispatch_new(same_request(&request))
                .unwrap_err()
                .code(),
            "remote_pending_persist_failed"
        );
        store.remote_proxies.lock().path = original_path;
        assert!(
            store
                .remote_proxy_snapshot(&request.semantics.local_session_id)
                .unwrap()
                .pending
                .is_empty()
        );
        assert_eq!(port.send_count.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn same_process_pre_barrier_retry_reuses_original_package_and_ids() {
        let (_temp, store, request) = setup();
        let port = Arc::new(FakePort::new(
            store.clone(),
            request.semantics.local_session_id.clone(),
        ));
        let service = RemoteDispatchService::new(store.clone(), port.clone());
        port.fail_after_persist.store(true, Ordering::SeqCst);
        let first = service.dispatch_new(same_request(&request)).unwrap();
        assert_eq!(first.state, RemoteDispatchState::PreparedNotSent);
        assert_eq!(first.retry_handle, handle(&store, &request));
        let actual_digest = {
            let ephemeral = service.ephemeral.lock();
            let cached = ephemeral
                .get(&(
                    request.semantics.local_session_id.clone(),
                    request.client_request_id.clone(),
                    request.task_id.clone(),
                ))
                .unwrap();
            assert_eq!(
                cached.wire_payload["temporary_model_package"]["primary"]["model"],
                "model-a"
            );
            model_execution_digest(cached.wire_payload.get("temporary_model_package")).unwrap()
        };
        let mut expected_semantics = request.semantics.clone();
        expected_semantics.model_profile_id = Some("model-1".into());
        expected_semantics.model_execution_digest = actual_digest;
        assert_eq!(
            first.retry_handle.request_fingerprint,
            request_fingerprint(&expected_semantics, &request.live).unwrap()
        );
        assert_eq!(port.prepare_count.load(Ordering::SeqCst), 1);
        assert_eq!(port.package_build_count.load(Ordering::SeqCst), 1);
        assert_eq!(port.send_count.load(Ordering::SeqCst), 0);
        let mut wrong = first.retry_handle.clone();
        wrong.request_fingerprint = "0".repeat(64);
        assert_eq!(
            service
                .dispatch_retry(
                    &request.semantics.local_session_id,
                    &request.semantics.remote_session_ref,
                    wrong,
                )
                .unwrap_err()
                .code(),
            "remote_request_id_conflict"
        );
        port.fail_after_persist.store(false, Ordering::SeqCst);
        // The original payload is still cached, while its credential source
        // is gone. A retry must not build another DEV-15 package.
        port.credential_available.store(false, Ordering::SeqCst);
        assert_eq!(
            retry(&service, &store, &request).unwrap().state,
            RemoteDispatchState::Queued
        );
        assert_eq!(port.prepare_count.load(Ordering::SeqCst), 1);
        assert_eq!(port.package_build_count.load(Ordering::SeqCst), 1);
        assert_eq!(port.send_count.load(Ordering::SeqCst), 1);
        *port.model_route.lock() = serde_json::json!({
            "preset": "openai", "wire_api": "chat_completions",
            "base_url": "https://changed.test/v1", "model": "model-b",
            "display_name": "Model B", "image_capability_override": "enabled"
        });
        assert_eq!(
            retry(&service, &store, &request).unwrap().state,
            RemoteDispatchState::OutcomeUnknown
        );
        assert_eq!(port.send_count.load(Ordering::SeqCst), 1);
        assert_eq!(port.package_build_count.load(Ordering::SeqCst), 1);
        let mut wrong_after_attempt = first.retry_handle;
        wrong_after_attempt.request_fingerprint = "f".repeat(64);
        assert_eq!(
            service
                .dispatch_retry(
                    &request.semantics.local_session_id,
                    &request.semantics.remote_session_ref,
                    wrong_after_attempt,
                )
                .unwrap_err()
                .code(),
            "remote_request_id_conflict"
        );
    }

    #[test]
    fn controlled_device_new_has_no_profile_or_temporary_package() {
        let (_temp, store, mut request) = setup();
        request.semantics.model_source = "controlled_device".into();
        request.semantics.model_profile_id = None;
        let port = Arc::new(FakePort::new(
            store.clone(),
            request.semantics.local_session_id.clone(),
        ));
        let service = RemoteDispatchService::new(store.clone(), port.clone());
        port.fail_after_persist.store(true, Ordering::SeqCst);
        let report = service.dispatch_new(same_request(&request)).unwrap();
        assert_eq!(report.state, RemoteDispatchState::PreparedNotSent);
        assert_eq!(port.package_build_count.load(Ordering::SeqCst), 0);
        let cached = service.ephemeral.lock();
        let payload = &cached
            .get(&(
                request.semantics.local_session_id.clone(),
                request.client_request_id.clone(),
                request.task_id.clone(),
            ))
            .unwrap()
            .wire_payload;
        assert!(payload.get("temporary_model_package").is_none());
        let mut expected = request.semantics.clone();
        expected.model_execution_digest = model_execution_digest(None).unwrap();
        assert_eq!(expected.model_profile_id, None);
        assert_eq!(
            report.retry_handle.request_fingerprint,
            request_fingerprint(&expected, &request.live).unwrap()
        );
    }

    #[test]
    fn lost_ack_and_restart_never_auto_resend_or_claim_terminal_result() {
        let (temp, store, request) = setup();
        let port = Arc::new(FakePort::new(
            store.clone(),
            request.semantics.local_session_id.clone(),
        ));
        let service = RemoteDispatchService::new(store.clone(), port.clone());
        port.lose_ack.store(true, Ordering::SeqCst);
        let first = service.dispatch_new(same_request(&request)).unwrap();
        assert_eq!(first.state, RemoteDispatchState::OutcomeUnknown);
        assert_eq!(port.send_count.load(Ordering::SeqCst), 1);
        assert!(
            service
                .abandon_lost_payload(
                    &request.semantics.local_session_id,
                    &request.client_request_id,
                    &request.task_id
                )
                .is_err()
        );
        drop(service);
        drop(store);
        let restarted = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let restarted_port = Arc::new(FakePort::new(
            restarted.clone(),
            request.semantics.local_session_id.clone(),
        ));
        let service = RemoteDispatchService::new(restarted.clone(), restarted_port.clone());
        assert_eq!(restarted_port.send_count.load(Ordering::SeqCst), 0);
        assert_eq!(
            service
                .dispatch_retry(
                    &request.semantics.local_session_id,
                    &request.semantics.remote_session_ref,
                    first.retry_handle,
                )
                .unwrap()
                .state,
            RemoteDispatchState::OutcomeUnknown
        );
        assert_eq!(restarted_port.send_count.load(Ordering::SeqCst), 0);
        let pending = &restarted
            .remote_proxy_snapshot(&request.semantics.local_session_id)
            .unwrap()
            .pending[0];
        assert_eq!(
            pending.dispatch_state,
            PendingDispatchState::AttemptMayHaveOccurred
        );
        assert!(
            restarted
                .read_remote_proxy_history(&request.semantics.local_session_id)
                .unwrap()
                .projection
                .entries
                .is_empty()
        );
        assert!(
            service
                .abandon_lost_payload(
                    &request.semantics.local_session_id,
                    &request.client_request_id,
                    &request.task_id
                )
                .is_err()
        );
    }

    #[test]
    fn lost_pre_barrier_payload_can_only_be_retired_then_replaced_with_fresh_ids() {
        let (_temp, store, request) = setup();
        let port = Arc::new(FakePort::new(
            store.clone(),
            request.semantics.local_session_id.clone(),
        ));
        let service = RemoteDispatchService::new(store.clone(), port.clone());
        port.fail_after_persist.store(true, Ordering::SeqCst);
        assert_eq!(
            service.dispatch_new(same_request(&request)).unwrap().state,
            RemoteDispatchState::PreparedNotSent
        );
        assert!(
            service
                .abandon_lost_payload(
                    &request.semantics.local_session_id,
                    &request.client_request_id,
                    &request.task_id
                )
                .is_err()
        );
        drop(service);
        let fresh_service = RemoteDispatchService::new(store.clone(), port.clone());
        assert_eq!(
            retry(&fresh_service, &store, &request).unwrap_err().code(),
            "remote_payload_unavailable"
        );
        fresh_service
            .abandon_lost_payload(
                &request.semantics.local_session_id,
                &request.client_request_id,
                &request.task_id,
            )
            .unwrap();
        assert_eq!(
            fresh_service
                .dispatch_new(same_request(&request))
                .unwrap_err()
                .code(),
            "remote_request_id_retired"
        );
        let mut one_old_id = same_request(&request);
        one_old_id.task_id = "task-2".into();
        assert_eq!(
            fresh_service.dispatch_new(one_old_id).unwrap_err().code(),
            "remote_request_id_retired"
        );
        let mut fresh = same_request(&request);
        fresh.task_id = "task-2".into();
        fresh.client_request_id = "request-2".into();
        port.fail_after_persist.store(false, Ordering::SeqCst);
        assert_eq!(
            fresh_service.dispatch_new(fresh).unwrap().state,
            RemoteDispatchState::Queued
        );
        assert_eq!(port.send_count.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn forged_prepare_fields_fail_before_pending_persist_or_send() {
        for field in [
            "task_id",
            "client_request_id",
            "session_id",
            "input_digest",
            "connection_id",
            "capability_revision",
            "model_source",
        ] {
            let (_temp, store, request) = setup();
            let port = Arc::new(FakePort::new(
                store.clone(),
                request.semantics.local_session_id.clone(),
            ));
            *port.tamper_field.lock() = Some(field);
            let service = RemoteDispatchService::new(store.clone(), port.clone());
            assert_eq!(
                service
                    .dispatch_new(same_request(&request))
                    .unwrap_err()
                    .code(),
                "remote_prepare_mismatch",
                "field {field}"
            );
            assert!(
                store
                    .remote_proxy_snapshot(&request.semantics.local_session_id)
                    .unwrap()
                    .pending
                    .is_empty(),
                "field {field}"
            );
            assert_eq!(port.send_count.load(Ordering::SeqCst), 0, "field {field}");
        }
    }

    #[test]
    fn changed_route_does_not_replace_the_original_prepared_payload() {
        let (_temp, store, request) = setup();
        let port = Arc::new(FakePort::new(
            store.clone(),
            request.semantics.local_session_id.clone(),
        ));
        let service = RemoteDispatchService::new(store.clone(), port.clone());
        port.fail_after_persist.store(true, Ordering::SeqCst);
        assert_eq!(
            service.dispatch_new(same_request(&request)).unwrap().state,
            RemoteDispatchState::PreparedNotSent
        );
        port.model_route.lock()["base_url"] = "https://changed.example/v1".into();
        port.credential_available.store(false, Ordering::SeqCst);
        port.fail_after_persist.store(false, Ordering::SeqCst);
        assert_eq!(
            retry(&service, &store, &request).unwrap().state,
            RemoteDispatchState::Queued
        );
        assert_eq!(port.send_count.load(Ordering::SeqCst), 1);
        assert_eq!(port.package_build_count.load(Ordering::SeqCst), 1);
        assert_eq!(
            port.sent_route.lock().as_ref().unwrap()["base_url"],
            "https://example.test/v1"
        );
    }

    struct AcceptTerminal;

    impl RemoteTerminalVerificationPort for AcceptTerminal {
        fn verify_terminal(&self, candidate: &RemoteTerminalCandidate) -> Result<(), UseCaseError> {
            if candidate.authenticated_event_id == "authenticated-event-1" {
                Ok(())
            } else {
                Err(error("terminal_not_authenticated"))
            }
        }
    }

    fn terminal_candidate(
        request: &RemoteDispatchRequest,
        fingerprint: String,
    ) -> RemoteTerminalCandidate {
        RemoteTerminalCandidate {
            local_session_id: request.semantics.local_session_id.clone(),
            remote_session_ref: request.semantics.remote_session_ref.clone(),
            controlled_session_id: request.semantics.controlled_session_id.clone(),
            task_id: request.task_id.clone(),
            client_request_id: request.client_request_id.clone(),
            request_fingerprint: fingerprint,
            authenticated_event_id: "authenticated-event-1".into(),
            live: request.live.clone(),
        }
    }

    #[test]
    fn terminal_proof_only_exposes_the_original_candidate_by_consuming_it() {
        struct CountingVerifier(AtomicUsize);
        impl RemoteTerminalVerificationPort for CountingVerifier {
            fn verify_terminal(
                &self,
                candidate: &RemoteTerminalCandidate,
            ) -> Result<(), UseCaseError> {
                self.0.fetch_add(1, Ordering::SeqCst);
                if candidate.authenticated_event_id == "authenticated-event-1" {
                    Ok(())
                } else {
                    Err(error("terminal_not_authenticated"))
                }
            }
        }

        let (_temp, _store, request) = setup();
        let verifier = CountingVerifier(AtomicUsize::new(0));
        let fingerprint = "a".repeat(64);
        let mut malformed = terminal_candidate(&request, fingerprint.clone());
        malformed.request_fingerprint = "not-a-digest".into();
        assert_eq!(
            VerifiedRemoteTerminal::verify(malformed, &verifier)
                .err()
                .unwrap()
                .code(),
            "remote_terminal_invalid"
        );
        assert_eq!(verifier.0.load(Ordering::SeqCst), 0);

        let mut unauthenticated = terminal_candidate(&request, fingerprint.clone());
        unauthenticated.authenticated_event_id = "unverified".into();
        assert_eq!(
            VerifiedRemoteTerminal::verify(unauthenticated, &verifier)
                .err()
                .unwrap()
                .code(),
            "terminal_not_authenticated"
        );
        assert_eq!(verifier.0.load(Ordering::SeqCst), 1);

        let proof: VerifiedRemoteTerminal = VerifiedRemoteTerminal::verify(
            terminal_candidate(&request, fingerprint.clone()),
            &verifier,
        )
        .unwrap();
        let recovered: RemoteTerminalCandidate = proof.into_candidate();
        assert_eq!(verifier.0.load(Ordering::SeqCst), 2);
        assert_eq!(
            recovered.local_session_id,
            request.semantics.local_session_id
        );
        assert_eq!(
            recovered.remote_session_ref,
            request.semantics.remote_session_ref
        );
        assert_eq!(
            recovered.controlled_session_id,
            request.semantics.controlled_session_id
        );
        assert_eq!(recovered.task_id, request.task_id);
        assert_eq!(recovered.client_request_id, request.client_request_id);
        assert_eq!(recovered.request_fingerprint, fingerprint);
        assert_eq!(recovered.authenticated_event_id, "authenticated-event-1");
        assert_eq!(recovered.live, request.live);
    }

    #[test]
    fn authenticated_terminal_reconciles_exact_binding_only_and_retires_ids() {
        let (_temp, store, request) = setup();
        let port = Arc::new(FakePort::new(
            store.clone(),
            request.semantics.local_session_id.clone(),
        ));
        let service = RemoteDispatchService::new(store.clone(), port);
        assert_eq!(
            service.dispatch_new(same_request(&request)).unwrap().state,
            RemoteDispatchState::Queued
        );
        let fingerprint = store
            .remote_proxy_snapshot(&request.semantics.local_session_id)
            .unwrap()
            .pending[0]
            .request_fingerprint
            .clone();
        let mut bad = terminal_candidate(&request, fingerprint.clone());
        bad.controlled_session_id = "forged-b-session".into();
        assert_eq!(
            service
                .reconcile_terminal(VerifiedRemoteTerminal::verify(bad, &AcceptTerminal).unwrap())
                .unwrap_err()
                .code(),
            "remote_terminal_mapping_mismatch"
        );
        let mut bad = terminal_candidate(&request, fingerprint.clone());
        bad.live.generation = "old-generation".into();
        assert_eq!(
            service
                .reconcile_terminal(VerifiedRemoteTerminal::verify(bad, &AcceptTerminal).unwrap())
                .unwrap_err()
                .code(),
            "remote_binding_mismatch"
        );
        let mut bad = terminal_candidate(&request, fingerprint.clone());
        bad.authenticated_event_id = "unverified".into();
        assert_eq!(
            VerifiedRemoteTerminal::verify(bad, &AcceptTerminal)
                .err()
                .unwrap()
                .code(),
            "terminal_not_authenticated"
        );
        service
            .reconcile_terminal(
                VerifiedRemoteTerminal::verify(
                    terminal_candidate(&request, fingerprint),
                    &AcceptTerminal,
                )
                .unwrap(),
            )
            .unwrap();
        let record = store
            .remote_proxy_snapshot(&request.semantics.local_session_id)
            .unwrap();
        assert!(record.pending.is_empty());
        assert_eq!(record.retired_requests.len(), 1);
        assert_eq!(
            service
                .dispatch_new(same_request(&request))
                .unwrap_err()
                .code(),
            "remote_request_id_retired"
        );
    }
}
