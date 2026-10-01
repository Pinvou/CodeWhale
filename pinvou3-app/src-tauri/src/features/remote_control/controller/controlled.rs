//! Windows controlled-device (B) lifecycle owner.
//!
//! The manager keeps only in-memory pairing and lease material. A monotonic
//! intent/generation pair gates every late network result, while DEV-08's A
//! watchdog remains the sole source of B's local safety deadline.

use std::path::PathBuf;
use std::sync::Arc;

use parking_lot::Mutex;
use serde::Serialize;
use uuid::Uuid;
use zeroize::{Zeroize, Zeroizing};

use super::crypto::{AuthenticatedRelayTrustConfig, authenticate_relay_liveness_attestation};
use super::liveness::{
    ControllerLivenessWatchdog, LivenessWatchdogState, MAX_LIVENESS_ATTESTATION_AGE_MS,
};
use super::protocol::LeaseContext;
use super::relay_transport::ControllerTransportSender;
use super::resume::{ControllerResumeGate, ResumePhase};

const MAX_DEVICE_NAME_CHARS: usize = 80;
const MAX_WORKSPACES: usize = 16;
const MAX_DISPLAY_NAME_CHARS: usize = 120;

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum ControlledStatusKind {
    Disabled,
    Reserving,
    WaitingConnection,
    SafetyHandshake,
    Connected,
    Reconnecting,
    Refreshing,
    Stopped,
    Error,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ControlledOperation {
    intent: u64,
    generation: String,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ControlledStateError {
    pub(crate) code: &'static str,
}

impl ControlledStateError {
    const fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl std::fmt::Display for ControlledStateError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for ControlledStateError {}

type Result<T> = std::result::Result<T, ControlledStateError>;

#[derive(Clone)]
pub(crate) struct ControlledRegistration {
    pub(crate) pairing_code: String,
    pub(crate) sealed_code_b64: String,
    pub(crate) expires_at_ms: u64,
}

impl std::fmt::Debug for ControlledRegistration {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ControlledRegistration")
            .field("expires_at_ms", &self.expires_at_ms)
            .field("pairing_code", &"[REDACTED]")
            .field("sealed_code_b64", &"[REDACTED]")
            .finish()
    }
}

pub(crate) struct ControlledLease {
    pub(crate) context: LeaseContext,
    pub(crate) liveness_epoch_a: String,
    pub(crate) resume_deadline_b: u64,
    pub(crate) resume_token: [u8; 32],
    pub(crate) transport_key_fingerprint: [u8; 32],
    pub(crate) controller_device_name: String,
    pub(crate) connected_at_b: u64,
}

impl std::fmt::Debug for ControlledLease {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ControlledLease")
            .field("context", &self.context)
            .field("liveness_epoch_a", &self.liveness_epoch_a)
            .field("resume_deadline_b", &self.resume_deadline_b)
            .field("resume_token", &"[REDACTED]")
            .field("transport_key_fingerprint", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug)]
struct PairingState {
    registration_request_id: String,
    sealed_code_b64: String,
    pairing_code: Zeroizing<String>,
    expires_at_ms: u64,
}

impl std::ops::Drop for PairingState {
    fn drop(&mut self) {
        self.pairing_code.zeroize();
    }
}

#[derive(Clone)]
struct WorkspaceAuthorization {
    id: String,
    display_name: String,
    path: PathBuf,
    code_allowed: bool,
}

#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControlledWorkspaceInfo {
    pub(crate) workspace_id: String,
    pub(crate) display_name: String,
    pub(crate) code_allowed: bool,
}

#[derive(Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControlledStatus {
    status: ControlledStatusKind,
    generation: Option<String>,
    registration_request_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pairing_code: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pairing_expires_at_ms: Option<u64>,
    device_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    controller_device_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    connected_at_b: Option<u64>,
    workspace_count: usize,
    code_mode_allowed: bool,
    watchdog_deadline_b: Option<u64>,
    watchdog_suspect: bool,
    error_code: Option<&'static str>,
}

impl std::fmt::Debug for ControlledStatus {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ControlledStatus")
            .field("status", &self.status)
            .field("generation", &self.generation)
            .field("registration_request_id", &self.registration_request_id)
            .field(
                "pairing_code",
                &self.pairing_code.as_ref().map(|_| "[REDACTED]"),
            )
            .field("pairing_expires_at_ms", &self.pairing_expires_at_ms)
            .field("device_name", &self.device_name)
            .field("controller_device_name", &self.controller_device_name)
            .field("connected_at_b", &self.connected_at_b)
            .field("workspace_count", &self.workspace_count)
            .field("code_mode_allowed", &self.code_mode_allowed)
            .field("watchdog_deadline_b", &self.watchdog_deadline_b)
            .field("watchdog_suspect", &self.watchdog_suspect)
            .field("error_code", &self.error_code)
            .finish()
    }
}

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
pub(crate) struct ControlledCleanupScope {
    pub(crate) remote_tasks: bool,
    pub(crate) pending_permissions: bool,
    pub(crate) session_keys: bool,
    pub(crate) temporary_models: bool,
    pub(crate) workspace_grants: bool,
}

impl ControlledCleanupScope {
    const ALL: Self = Self {
        remote_tasks: true,
        pending_permissions: true,
        session_keys: true,
        temporary_models: true,
        workspace_grants: true,
    };
}

pub(crate) trait ControlledCleanupSink: Send + Sync {
    fn cleanup(&self, generation: &str, scope: ControlledCleanupScope);
}

struct Inner {
    phase: ControlledStatusKind,
    intent: u64,
    generation: Option<String>,
    registration_request_id: Option<String>,
    device_name: Option<String>,
    pairing: Option<PairingState>,
    lease_context: Option<LeaseContext>,
    liveness_epoch_a: Option<String>,
    controller_device_name: Option<String>,
    connected_at_b: Option<u64>,
    watchdog: Option<ControllerLivenessWatchdog>,
    resume_gate: Option<ControllerResumeGate>,
    last_observed_now_b: u64,
    transport: Option<ControllerTransportSender>,
    workspaces: Vec<WorkspaceAuthorization>,
    error_code: Option<&'static str>,
}

#[derive(Default)]
pub(crate) struct ControlledDeviceManager {
    inner: Mutex<Inner>,
    cleanup_sinks: Mutex<Vec<Arc<dyn ControlledCleanupSink>>>,
}

impl Default for Inner {
    fn default() -> Self {
        Self {
            phase: ControlledStatusKind::Disabled,
            intent: 0,
            generation: None,
            registration_request_id: None,
            device_name: None,
            pairing: None,
            lease_context: None,
            liveness_epoch_a: None,
            controller_device_name: None,
            connected_at_b: None,
            watchdog: None,
            resume_gate: None,
            last_observed_now_b: 0,
            transport: None,
            workspaces: Vec::new(),
            error_code: None,
        }
    }
}

pub(crate) struct WorkspaceAuthorizationRequest {
    pub(crate) path: PathBuf,
    pub(crate) display_name: String,
    pub(crate) code_allowed: bool,
}

impl ControlledDeviceManager {
    pub(crate) fn is_supported(&self) -> bool {
        super::super::platform::desktop_control_supported()
    }

    pub(crate) fn start(
        &self,
        device_name: Option<&str>,
        workspaces: Vec<WorkspaceAuthorizationRequest>,
    ) -> Result<ControlledOperation> {
        let mut inner = self.inner.lock();
        if !matches!(
            inner.phase,
            ControlledStatusKind::Disabled
                | ControlledStatusKind::Stopped
                | ControlledStatusKind::Error
        ) {
            return Err(ControlledStateError::new("desktop_remote_control_busy"));
        }
        let device_name = device_name.map(str::trim).filter(|value| !value.is_empty());
        if let Some(name) = device_name {
            if name.chars().count() > MAX_DEVICE_NAME_CHARS {
                return Err(ControlledStateError::new("device_name_invalid"));
            }
        }
        clear_sensitive(&mut inner, 0, true);
        let authorization = normalize_workspaces(workspaces)?;
        let operation = prepare_generation(&mut inner);
        inner.device_name = device_name.map(str::to_owned);
        inner.workspaces = authorization;
        inner.phase = ControlledStatusKind::Reserving;
        inner.error_code = None;
        Ok(operation)
    }

    pub(crate) fn registration_request_id(&self, operation: ControlledOperation) -> Result<String> {
        let inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        inner
            .registration_request_id
            .clone()
            .ok_or_else(|| ControlledStateError::new("registration_request_unavailable"))
    }

    pub(crate) fn complete_registration(
        &self,
        operation: ControlledOperation,
        registration: ControlledRegistration,
        now_b: u64,
    ) -> Result<ControlledStatus> {
        validate_pairing_code(&registration.pairing_code)?;
        if registration.sealed_code_b64.is_empty() {
            return Err(ControlledStateError::new("sealed_code_invalid"));
        }
        if now_b >= registration.expires_at_ms {
            return Err(ControlledStateError::new("pairing_code_expired"));
        }
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::Reserving {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        inner.pairing = Some(PairingState {
            registration_request_id: inner
                .registration_request_id
                .clone()
                .expect("generation reserves a registration ID"),
            sealed_code_b64: registration.sealed_code_b64,
            pairing_code: Zeroizing::new(registration.pairing_code),
            expires_at_ms: registration.expires_at_ms,
        });
        inner.phase = ControlledStatusKind::WaitingConnection;
        inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
        Ok(status_locked(&inner))
    }

    pub(crate) fn mark_claimed(&self, operation: ControlledOperation) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::WaitingConnection {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        if let Some(pairing) = inner.pairing.as_mut() {
            pairing.pairing_code.zeroize();
        }
        inner.pairing = None;
        inner.phase = ControlledStatusKind::SafetyHandshake;
        Ok(status_locked(&inner))
    }

    pub(crate) fn complete_connected(
        &self,
        operation: ControlledOperation,
        lease: ControlledLease,
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::SafetyHandshake {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        if lease.context.remote_control_generation != operation.generation {
            return Err(ControlledStateError::new(
                "desktop_remote_generation_mismatch",
            ));
        }
        let watchdog = ControllerLivenessWatchdog::authenticated_cycle(
            lease.liveness_epoch_a.clone(),
            lease.connected_at_b,
        )
        .map_err(|_| ControlledStateError::new("liveness_epoch_invalid"))?;
        let resume_gate = ControllerResumeGate::new(
            super::relay_transport::ControllerRole::ControlledB,
            lease.context.clone(),
            lease.resume_token,
            lease.transport_key_fingerprint,
            lease.resume_deadline_b,
        );
        inner.lease_context = Some(lease.context);
        inner.liveness_epoch_a = Some(lease.liveness_epoch_a);
        inner.controller_device_name = Some(lease.controller_device_name);
        inner.connected_at_b = Some(lease.connected_at_b);
        inner.watchdog = Some(watchdog);
        inner.resume_gate = Some(resume_gate);
        inner.pairing = None;
        inner.phase = ControlledStatusKind::Connected;
        Ok(status_locked(&inner))
    }

    pub(crate) fn replace_authenticated_transport(
        &self,
        operation: ControlledOperation,
        transport: ControllerTransportSender,
    ) -> Result<()> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if let Some(previous) = inner.transport.replace(transport) {
            previous.stop();
        }
        Ok(())
    }

    pub(crate) fn replace_transport(&self, transport: ControllerTransportSender) {
        let mut inner = self.inner.lock();
        if let Some(previous) = inner.transport.replace(transport) {
            previous.stop();
        }
    }

    pub(crate) fn is_running(&self) -> bool {
        self.inner
            .lock()
            .transport
            .as_ref()
            .is_some_and(|transport| !transport.is_stopped())
    }

    pub(crate) fn mark_transport_disconnected(&self, now_b: u64) -> ControlledStatus {
        let mut inner = self.inner.lock();
        inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
        if matches!(
            inner.phase,
            ControlledStatusKind::Connected | ControlledStatusKind::Reconnecting
        ) && inner
            .resume_gate
            .as_ref()
            .is_none_or(|gate| gate.phase() == ResumePhase::Connected)
        {
            let deadline = inner
                .watchdog
                .as_mut()
                .and_then(|watchdog| watchdog.freeze_deadline(now_b));
            if inner
                .resume_gate
                .as_mut()
                .is_some_and(|gate| gate.disconnect(now_b, deadline).is_ok())
            {
                inner.phase = ControlledStatusKind::Reconnecting;
            } else {
                let generation = inner.generation.clone().unwrap_or_default();
                clear_sensitive(&mut inner, now_b, true);
                inner.device_name = None;
                inner.phase = ControlledStatusKind::Error;
                inner.error_code = Some("desktop_remote_resume_invalid");
                drop(inner);
                self.notify_cleanup(&generation);
                return self.status();
            }
        }
        status_locked(&inner)
    }

    pub(crate) fn register_liveness_challenge(
        &self,
        challenge_id: impl Into<String>,
        challenge_b64: impl Into<String>,
        sent_at_b: u64,
    ) -> Result<()> {
        let mut inner = self.inner.lock();
        ensure_connected_evidence(&inner)?;
        let Some(watchdog) = inner.watchdog.as_mut() else {
            return Err(ControlledStateError::new("watchdog_unavailable"));
        };
        watchdog
            .register_challenge(challenge_id, challenge_b64, sent_at_b)
            .map_err(|_| ControlledStateError::new("liveness_challenge_invalid"))
    }

    pub(crate) fn accept_direct_liveness(&self, json: &str, received_at_b: u64) -> Result<()> {
        let mut inner = self.inner.lock();
        ensure_connected_evidence(&inner)?;
        let Some(context) = inner.lease_context.clone() else {
            return Err(ControlledStateError::new("lease_unavailable"));
        };
        let Some(watchdog) = inner.watchdog.as_mut() else {
            return Err(ControlledStateError::new("watchdog_unavailable"));
        };
        let _ = watchdog
            .accept_direct_json(json, &context, received_at_b)
            .map_err(|_| ControlledStateError::new("liveness_evidence_invalid"))?;
        inner.last_observed_now_b = inner.last_observed_now_b.max(received_at_b);
        recover_liveness_suspect(&mut inner, received_at_b);
        Ok(())
    }

    pub(crate) fn accept_liveness_attestation(
        &self,
        json: &str,
        trust: &AuthenticatedRelayTrustConfig,
        request_sent_at_b: u64,
        received_at_b: u64,
    ) -> Result<()> {
        authenticate_relay_liveness_attestation(json, trust)
            .map_err(|_| ControlledStateError::new("liveness_evidence_invalid"))?;
        let mut inner = self.inner.lock();
        ensure_connected_evidence(&inner)?;
        let Some(context) = inner.lease_context.clone() else {
            return Err(ControlledStateError::new("lease_unavailable"));
        };
        let Some(watchdog) = inner.watchdog.as_mut() else {
            return Err(ControlledStateError::new("watchdog_unavailable"));
        };
        if request_sent_at_b.saturating_add(MAX_LIVENESS_ATTESTATION_AGE_MS) < received_at_b {
            return Err(ControlledStateError::new("liveness_evidence_invalid"));
        }
        let _ = watchdog
            .accept_attestation_json(json, &context, request_sent_at_b, received_at_b)
            .map_err(|_| ControlledStateError::new("liveness_evidence_invalid"))?;
        inner.last_observed_now_b = inner.last_observed_now_b.max(received_at_b);
        recover_liveness_suspect(&mut inner, received_at_b);
        Ok(())
    }

    pub(crate) fn begin_refresh(&self) -> Result<ControlledOperation> {
        let mut inner = self.inner.lock();
        if !matches!(
            inner.phase,
            ControlledStatusKind::Reserving
                | ControlledStatusKind::WaitingConnection
                | ControlledStatusKind::SafetyHandshake
                | ControlledStatusKind::Connected
                | ControlledStatusKind::Reconnecting
        ) {
            return Err(ControlledStateError::new("desktop_remote_refresh_invalid"));
        }
        inner.intent += 1;
        if let Some(pairing) = inner.pairing.as_mut() {
            pairing.pairing_code.zeroize();
        }
        inner.pairing = None;
        inner.phase = ControlledStatusKind::Refreshing;
        inner.error_code = None;
        current_operation(&inner).ok_or_else(|| ControlledStateError::new("generation_unavailable"))
    }

    pub(crate) fn complete_refresh_revoked(
        &self,
        operation: ControlledOperation,
    ) -> Result<ControlledOperation> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::Refreshing {
            return Err(ControlledStateError::new("desktop_remote_refresh_invalid"));
        }
        let retired_generation = inner.generation.clone().unwrap_or_default();
        clear_sensitive(&mut inner, 0, false);
        let next = prepare_generation(&mut inner);
        inner.phase = ControlledStatusKind::Reserving;
        drop(inner);
        self.notify_cleanup(&retired_generation);
        Ok(next)
    }

    pub(crate) fn fail_refresh(&self, operation: ControlledOperation) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::Refreshing {
            return Err(ControlledStateError::new("desktop_remote_refresh_invalid"));
        }
        let generation = inner.generation.clone().unwrap_or_default();
        clear_sensitive(&mut inner, u64::MAX, true);
        inner.device_name = None;
        inner.phase = ControlledStatusKind::Error;
        inner.error_code = Some("desktop_remote_refresh_failed");
        drop(inner);
        self.notify_cleanup(&generation);
        Ok(self.status())
    }

    pub(crate) fn fail_operation(
        &self,
        operation: ControlledOperation,
        error_code: &'static str,
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        let generation = inner.generation.clone().unwrap_or_default();
        clear_sensitive(&mut inner, u64::MAX, true);
        inner.device_name = None;
        inner.phase = ControlledStatusKind::Error;
        inner.error_code = Some(error_code);
        drop(inner);
        self.notify_cleanup(&generation);
        Ok(self.status())
    }

    pub(crate) fn complete_resumed(
        &self,
        operation: ControlledOperation,
        liveness_epoch_a: impl Into<String>,
        resume_token: [u8; 32],
        transport_key_fingerprint: [u8; 32],
        now_b: u64,
        relay_resume_deadline_ms: u64,
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::Reconnecting {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        let Some(context) = inner.lease_context.clone() else {
            return Err(ControlledStateError::new("lease_unavailable"));
        };
        let epoch = liveness_epoch_a.into();
        let next_watchdog =
            ControllerLivenessWatchdog::authenticated_cycle(epoch.clone(), now_b).ok();
        if relay_resume_deadline_ms == 0 || next_watchdog.is_none() {
            let generation = inner.generation.clone().unwrap_or_default();
            clear_sensitive(&mut inner, u64::MAX, true);
            inner.device_name = None;
            inner.phase = ControlledStatusKind::Error;
            inner.error_code = Some("endpoint_offline");
            drop(inner);
            self.notify_cleanup(&generation);
            return Err(ControlledStateError::new("desktop_remote_resume_failed"));
        }
        // Replacing the gate with the already-authenticated final material is
        // the single transition that cancels the old watchdog.
        inner.resume_gate = Some(ControllerResumeGate::new(
            super::relay_transport::ControllerRole::ControlledB,
            context,
            resume_token,
            transport_key_fingerprint,
            relay_resume_deadline_ms,
        ));
        inner.watchdog = next_watchdog;
        inner.liveness_epoch_a = Some(epoch);
        inner.phase = ControlledStatusKind::Connected;
        Ok(status_locked(&inner))
    }

    pub(crate) fn stop(&self) -> ControlledStatus {
        self.terminate_local(
            "desktop_remote_stopped",
            ControlledStatusKind::Stopped,
            true,
        )
    }

    pub(crate) fn disconnect_now(&self) -> ControlledStatus {
        self.terminate_local(
            "desktop_remote_disconnected",
            ControlledStatusKind::Stopped,
            true,
        )
    }

    pub(crate) fn poll_watchdog(&self, now_b: u64) -> ControlledStatus {
        let generation;
        let expired;
        {
            let mut inner = self.inner.lock();
            generation = inner.generation.clone().unwrap_or_default();
            inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
            let watchdog_state = inner
                .watchdog
                .as_ref()
                .map(|watchdog| watchdog.state(now_b));
            if watchdog_state == Some(LivenessWatchdogState::Expired) {
                clear_sensitive(&mut inner, now_b, true);
                inner.phase = ControlledStatusKind::Error;
                inner.error_code = Some("desktop_remote_watchdog_expired");
                expired = true;
            } else {
                expired = false;
                if inner.phase == ControlledStatusKind::Connected
                    && watchdog_state == Some(LivenessWatchdogState::Suspect)
                {
                    inner.phase = ControlledStatusKind::Reconnecting;
                }
            }
        }
        if expired {
            self.notify_cleanup(&generation);
        }
        self.status()
    }

    pub(crate) fn authorize_workspace(
        &self,
        request: WorkspaceAuthorizationRequest,
    ) -> Result<ControlledWorkspaceInfo> {
        let display_name = request.display_name.trim();
        if display_name.is_empty() || display_name.chars().count() > MAX_DISPLAY_NAME_CHARS {
            return Err(ControlledStateError::new("workspace_display_name_invalid"));
        }
        let mut inner = self.inner.lock();
        if matches!(
            inner.phase,
            ControlledStatusKind::Disabled
                | ControlledStatusKind::Stopped
                | ControlledStatusKind::Error
        ) {
            return Err(ControlledStateError::new("desktop_remote_not_running"));
        }
        if inner.workspaces.len() >= MAX_WORKSPACES {
            return Err(ControlledStateError::new("workspace_capacity_exhausted"));
        }
        if inner
            .workspaces
            .iter()
            .any(|workspace: &WorkspaceAuthorization| workspace.path == request.path)
        {
            return Err(ControlledStateError::new("workspace_already_authorized"));
        }
        let workspace = WorkspaceAuthorization {
            id: format!("workspace_{}", Uuid::new_v4().simple()),
            display_name: display_name.to_owned(),
            path: request.path,
            code_allowed: request.code_allowed,
        };
        let projected = ControlledWorkspaceInfo {
            workspace_id: workspace.id.clone(),
            display_name: workspace.display_name.clone(),
            code_allowed: workspace.code_allowed,
        };
        inner.workspaces.push(workspace);
        Ok(projected)
    }

    pub(crate) fn workspaces(&self) -> Vec<ControlledWorkspaceInfo> {
        self.inner
            .lock()
            .workspaces
            .iter()
            .map(|workspace| ControlledWorkspaceInfo {
                workspace_id: workspace.id.clone(),
                display_name: workspace.display_name.clone(),
                code_allowed: workspace.code_allowed,
            })
            .collect()
    }

    pub(crate) fn status(&self) -> ControlledStatus {
        status_locked(&self.inner.lock())
    }

    pub(crate) fn live_lease(&self) -> Result<LeaseContext> {
        let inner = self.inner.lock();
        if inner.phase != ControlledStatusKind::Connected {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        }
        inner
            .lease_context
            .clone()
            .ok_or_else(|| ControlledStateError::new("desktop_remote_not_connected"))
    }

    pub(crate) fn register_cleanup_sink(&self, sink: Arc<dyn ControlledCleanupSink>) {
        self.cleanup_sinks.lock().push(sink);
    }

    fn terminate_local(
        &self,
        error_code: &'static str,
        phase: ControlledStatusKind,
        clear_workspaces: bool,
    ) -> ControlledStatus {
        let generation;
        {
            let mut inner = self.inner.lock();
            generation = inner.generation.clone().unwrap_or_default();
            clear_sensitive(&mut inner, u64::MAX, clear_workspaces);
            inner.device_name = None;
            inner.phase = phase;
            inner.error_code = (phase == ControlledStatusKind::Error).then_some(error_code);
        }
        self.notify_cleanup(&generation);
        self.status()
    }

    fn notify_cleanup(&self, generation: &str) {
        let sinks = self.cleanup_sinks.lock().clone();
        for sink in sinks {
            sink.cleanup(generation, ControlledCleanupScope::ALL);
        }
    }
}

impl Drop for ControlledDeviceManager {
    fn drop(&mut self) {
        let generation;
        {
            let mut inner = self.inner.lock();
            generation = inner.generation.clone().unwrap_or_default();
            clear_sensitive(&mut inner, u64::MAX, true);
            inner.device_name = None;
            inner.phase = ControlledStatusKind::Stopped;
        }
        let sinks = std::mem::take(&mut *self.cleanup_sinks.lock());
        for sink in sinks {
            sink.cleanup(&generation, ControlledCleanupScope::ALL);
        }
    }
}

fn prepare_generation(inner: &mut Inner) -> ControlledOperation {
    inner.intent += 1;
    let generation = format!("generation_{}", Uuid::new_v4().simple());
    let registration = format!("registration_{}", Uuid::new_v4().simple());
    inner.generation = Some(generation.clone());
    inner.registration_request_id = Some(registration);
    ControlledOperation {
        intent: inner.intent,
        generation,
    }
}

fn current_operation(inner: &Inner) -> Option<ControlledOperation> {
    inner
        .generation
        .clone()
        .map(|generation| ControlledOperation {
            intent: inner.intent,
            generation,
        })
}

fn ensure_operation(inner: &Inner, operation: &ControlledOperation) -> Result<()> {
    if current_operation(inner).as_ref() != Some(operation) {
        return Err(ControlledStateError::new("desktop_remote_stale_result"));
    }
    Ok(())
}

fn normalize_workspaces(
    requests: Vec<WorkspaceAuthorizationRequest>,
) -> Result<Vec<WorkspaceAuthorization>> {
    if requests.len() > MAX_WORKSPACES {
        return Err(ControlledStateError::new("workspace_capacity_exhausted"));
    }
    let mut output = Vec::with_capacity(requests.len());
    for request in requests {
        let display_name = request.display_name.trim();
        if display_name.is_empty() || display_name.chars().count() > MAX_DISPLAY_NAME_CHARS {
            return Err(ControlledStateError::new("workspace_display_name_invalid"));
        }
        if output
            .iter()
            .any(|workspace: &WorkspaceAuthorization| workspace.path == request.path)
        {
            return Err(ControlledStateError::new("workspace_already_authorized"));
        }
        output.push(WorkspaceAuthorization {
            id: format!("workspace_{}", Uuid::new_v4().simple()),
            display_name: display_name.to_owned(),
            path: request.path,
            code_allowed: request.code_allowed,
        });
    }
    Ok(output)
}

fn ensure_connected_evidence(inner: &Inner) -> Result<()> {
    if matches!(
        inner.phase,
        ControlledStatusKind::Connected | ControlledStatusKind::Reconnecting
    ) && inner
        .resume_gate
        .as_ref()
        .is_some_and(|gate| gate.phase() == ResumePhase::Connected)
    {
        Ok(())
    } else {
        Err(ControlledStateError::new("liveness_evidence_invalid"))
    }
}

fn recover_liveness_suspect(inner: &mut Inner, now_b: u64) {
    if inner.phase == ControlledStatusKind::Reconnecting
        && inner
            .resume_gate
            .as_ref()
            .is_some_and(|gate| gate.phase() == ResumePhase::Connected)
        && inner
            .watchdog
            .as_ref()
            .is_some_and(|watchdog| watchdog.state(now_b) == LivenessWatchdogState::Healthy)
    {
        inner.phase = ControlledStatusKind::Connected;
    }
}

fn clear_sensitive(inner: &mut Inner, now_b: u64, clear_workspaces: bool) {
    if let Some(transport) = inner.transport.take() {
        transport.stop();
    }
    if let Some(mut watchdog) = inner.watchdog.take() {
        watchdog.shorten_deadline(now_b);
    }
    if let Some(gate) = inner.resume_gate.as_mut() {
        let _ = gate.terminate("desktop_remote_cleanup");
    }
    inner.resume_gate = None;
    inner.pairing = None;
    inner.lease_context = None;
    inner.liveness_epoch_a = None;
    inner.controller_device_name = None;
    inner.connected_at_b = None;
    inner.generation = None;
    inner.registration_request_id = None;
    if clear_workspaces {
        inner.workspaces.clear();
    }
}

fn status_locked(inner: &Inner) -> ControlledStatus {
    let watchdog_state = inner
        .watchdog
        .as_ref()
        .map(|watchdog| watchdog.state(inner.last_observed_now_b));
    ControlledStatus {
        status: inner.phase,
        generation: inner.generation.clone(),
        registration_request_id: inner.registration_request_id.clone(),
        pairing_code: inner
            .pairing
            .as_ref()
            .filter(|_| inner.phase == ControlledStatusKind::WaitingConnection)
            .map(|pairing| pairing.pairing_code.to_string()),
        pairing_expires_at_ms: inner
            .pairing
            .as_ref()
            .filter(|_| inner.phase == ControlledStatusKind::WaitingConnection)
            .map(|pairing| pairing.expires_at_ms),
        device_name: inner.device_name.clone(),
        controller_device_name: inner.controller_device_name.clone(),
        connected_at_b: inner.connected_at_b,
        workspace_count: inner.workspaces.len(),
        code_mode_allowed: inner
            .workspaces
            .iter()
            .any(|workspace| workspace.code_allowed),
        watchdog_deadline_b: inner
            .watchdog
            .as_ref()
            .and_then(|watchdog| watchdog.deadline_b()),
        watchdog_suspect: watchdog_state == Some(LivenessWatchdogState::Suspect),
        error_code: inner.error_code,
    }
}

fn validate_pairing_code(code: &str) -> Result<()> {
    let valid = code.len() == 7
        && code.bytes().all(|byte| {
            byte.is_ascii_digit() && !matches!(byte, b'0' | b'1' | b'I' | b'O')
                || matches!(byte, b'A'..=b'H' | b'J'..=b'N' | b'P'..=b'Z')
        });
    if !valid {
        return Err(ControlledStateError::new("pairing_code_invalid"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicUsize, Ordering};

    use super::*;

    #[derive(Default)]
    struct CleanupCounter(AtomicUsize);

    impl ControlledCleanupSink for CleanupCounter {
        fn cleanup(&self, _generation: &str, scope: ControlledCleanupScope) {
            assert!(scope.remote_tasks && scope.session_keys && scope.temporary_models);
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    fn lease(generation: &str) -> ControlledLease {
        ControlledLease {
            context: LeaseContext {
                connection_id: "connection-0001".into(),
                controller_lease_id: "lease-0001".into(),
                lease_epoch: "1".into(),
                relay_epoch: "relay-epoch-0001".into(),
                remote_control_generation: generation.into(),
            },
            liveness_epoch_a: "live-a-1".into(),
            resume_deadline_b: 30_000,
            resume_token: [1; 32],
            transport_key_fingerprint: [2; 32],
            controller_device_name: "Controller PC".into(),
            connected_at_b: 3_000,
        }
    }

    fn direct_liveness(generation: &str) -> String {
        serde_json::json!({
            "v": 1,
            "type": "controller_liveness_a",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "1",
            "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": generation,
            "role": "A",
            "liveness_epoch_a": "live-a-1",
            "heartbeat_sequence_a": "1",
            "challenge_id": "challenge-0001",
            "challenge_b64": "AAAAAAAAAAAAAAAAAAAAAA=="
        })
        .to_string()
    }

    fn register(manager: &ControlledDeviceManager, operation: &ControlledOperation) {
        manager
            .complete_registration(
                operation.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: 30_000,
                },
                1_000,
            )
            .unwrap();
    }

    #[test]
    fn attestation_authentication_precedes_every_watchdog_state_change() {
        use super::super::protocol::build_liveness_attestation_preimage;
        use base64::Engine as _;
        use ring::signature::{Ed25519KeyPair, KeyPair};

        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        register(&manager, &operation);
        manager.mark_claimed(operation.clone()).unwrap();
        manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        let nonce = "AAAAAAAAAAAAAAAAAAAAAA==";
        manager
            .register_liveness_challenge("challenge-0001", nonce, 4_000)
            .unwrap();
        let prior_observed = manager.inner.lock().last_observed_now_b;

        let key = Ed25519KeyPair::from_seed_unchecked(&[7; 32]).unwrap();
        let public_key: [u8; 32] = key.public_key().as_ref().try_into().unwrap();
        let trust = AuthenticatedRelayTrustConfig::from_test_enrollment([(
            "relay-key-test-1".into(),
            public_key,
        )]);
        let mut proof = serde_json::json!({
            "v": 1, "type": "controller_liveness_attestation_a", "request_id": "request-0001",
            "connection_id": "connection-0001", "controller_lease_id": "lease-0001",
            "lease_epoch": "1", "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": operation.generation, "liveness_epoch_a": "live-a-1",
            "heartbeat_sequence_a": "1", "attestation_sequence": "1",
            "challenge_id": "challenge-0001", "challenge_b64": nonce,
            "observed_age_ms": "1000", "remaining_ttl_ms": "29000", "issued_at_relay": "5000",
            "relay_signing_key_id": "relay-key-test-1", "signature_algorithm": "ed25519"
        });
        proof["relay_signature_b64"] = base64::engine::general_purpose::STANDARD
            .encode(
                key.sign(&build_liveness_attestation_preimage(&proof).unwrap())
                    .as_ref(),
            )
            .into();
        for (field, replacement) in [
            ("relay_signature_b64", format!("{}==", "A".repeat(86))),
            ("relay_signing_key_id", "unknown-key".to_owned()),
            ("remaining_ttl_ms", "28000".to_owned()),
            ("signature_algorithm", "unsupported".to_owned()),
        ] {
            let mut changed = proof.clone();
            changed[field] = replacement.into();
            assert_eq!(
                manager
                    .accept_liveness_attestation(&changed.to_string(), &trust, 4_000, 5_000)
                    .unwrap_err()
                    .code,
                "liveness_evidence_invalid"
            );
            assert_eq!(manager.status().watchdog_deadline_b, Some(33_000));
            let inner = manager.inner.lock();
            assert_eq!(inner.last_observed_now_b, prior_observed);
        }
        // The same valid proof must still install evidence: failed signatures
        // neither consumed its challenge nor advanced the attestation sequence.
        manager
            .accept_liveness_attestation(&proof.to_string(), &trust, 4_000, 5_000)
            .unwrap();
        assert!(manager.status().watchdog_deadline_b.is_some());
    }

    #[test]
    fn start_registration_claim_and_connection_form_one_generation() {
        let manager = ControlledDeviceManager::default();
        let operation = manager
            .start(
                Some("  Studio PC  "),
                vec![WorkspaceAuthorizationRequest {
                    path: PathBuf::from(r"D:\projects"),
                    display_name: "Projects".into(),
                    code_allowed: true,
                }],
            )
            .unwrap();
        assert_eq!(
            manager.registration_request_id(operation.clone()).unwrap(),
            manager.registration_request_id(operation.clone()).unwrap()
        );
        assert_eq!(manager.status().status, ControlledStatusKind::Reserving);
        let status = manager
            .complete_registration(
                operation.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: 30_000,
                },
                1_000,
            )
            .unwrap();
        assert_eq!(status.status, ControlledStatusKind::WaitingConnection);
        assert_eq!(status.pairing_code.as_deref(), Some("2345678"));
        assert_eq!(status.pairing_expires_at_ms, Some(30_000));
        assert_eq!(status.workspace_count, 1);
        assert!(status.code_mode_allowed);
        assert_eq!(status.device_name.as_deref(), Some("Studio PC"));

        let claimed = manager.mark_claimed(operation.clone()).unwrap();
        assert_eq!(claimed.status, ControlledStatusKind::SafetyHandshake);
        assert_eq!(claimed.pairing_code, None);
        let connected = manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        assert_eq!(connected.status, ControlledStatusKind::Connected);
        assert_eq!(
            connected.controller_device_name.as_deref(),
            Some("Controller PC")
        );
        assert_eq!(connected.connected_at_b, Some(3_000));
        assert_eq!(connected.watchdog_deadline_b, Some(33_000));
        let projected = manager.workspaces();
        assert_eq!(projected.len(), 1);
        assert_eq!(projected[0].display_name, "Projects");
        assert!(!format!("{projected:?}").contains("projects"));
    }

    #[test]
    fn stale_results_cannot_replace_a_new_generation() {
        let manager = ControlledDeviceManager::default();
        let old = manager.start(Some("B"), Vec::new()).unwrap();
        let next = manager.begin_refresh().unwrap();
        assert_eq!(old.generation, next.generation);
        assert_eq!(next.intent, old.intent + 1);
        assert_eq!(manager.status().status, ControlledStatusKind::Refreshing);
        let new_operation = manager.complete_refresh_revoked(next).unwrap();
        assert_ne!(new_operation.generation, old.generation);
        assert_eq!(manager.status().status, ControlledStatusKind::Reserving);
        assert_eq!(
            manager
                .complete_registration(
                    old,
                    ControlledRegistration {
                        pairing_code: "2345678".into(),
                        sealed_code_b64: "sealed".into(),
                        expires_at_ms: 30_000,
                    },
                    1_000,
                )
                .unwrap_err()
                .code,
            "desktop_remote_stale_result"
        );
    }

    #[test]
    fn refresh_revocation_replaces_pairing_and_preserves_user_workspaces() {
        let manager = ControlledDeviceManager::default();
        let first = manager
            .start(
                None,
                vec![WorkspaceAuthorizationRequest {
                    path: PathBuf::from(r"D:\reports"),
                    display_name: "Reports".into(),
                    code_allowed: false,
                }],
            )
            .unwrap();
        manager
            .complete_registration(
                first,
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: 30_000,
                },
                1_000,
            )
            .unwrap();
        let refresh = manager.begin_refresh().unwrap();
        assert_eq!(manager.status().pairing_code, None);
        assert_eq!(manager.status().status, ControlledStatusKind::Refreshing);
        manager.complete_refresh_revoked(refresh).unwrap();
        assert_eq!(manager.status().status, ControlledStatusKind::Reserving);
        assert_eq!(manager.status().workspace_count, 1);
        assert!(!manager.status().code_mode_allowed);
    }

    #[test]
    fn late_valid_evidence_recovers_suspect_then_real_close_freezes_without_extension() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        register(&manager, &operation);
        manager.mark_claimed(operation.clone()).unwrap();
        manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        manager
            .register_liveness_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 1_000)
            .unwrap();
        manager
            .accept_direct_liveness(&direct_liveness(&operation.generation), 2_000)
            .unwrap();
        assert_eq!(
            manager.poll_watchdog(17_000).status,
            ControlledStatusKind::Reconnecting
        );
        let mut proof: serde_json::Value =
            serde_json::from_str(&direct_liveness(&operation.generation)).unwrap();
        proof["challenge_id"] = "challenge-0002".into();
        proof["challenge_b64"] = "AQEBAQEBAQEBAQEBAQEBAQ==".into();
        proof["heartbeat_sequence_a"] = "2".into();
        manager
            .register_liveness_challenge("challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ==", 17_000)
            .unwrap();
        manager
            .accept_direct_liveness(&proof.to_string(), 18_000)
            .unwrap();
        assert_eq!(manager.status().status, ControlledStatusKind::Connected);
        assert_eq!(manager.status().watchdog_deadline_b, Some(46_000));
        assert_eq!(
            manager.mark_transport_disconnected(19_000).status,
            ControlledStatusKind::Reconnecting
        );
        assert_eq!(
            manager
                .inner
                .lock()
                .resume_gate
                .as_ref()
                .unwrap()
                .frozen_deadline_b(),
            Some(46_000)
        );
        assert!(
            manager
                .register_liveness_challenge("challenge-0003", "AgICAgICAgICAgICAgICAg==", 20_000)
                .is_err()
        );
        assert!(
            manager
                .accept_direct_liveness(&proof.to_string(), 20_000)
                .is_err()
        );
        manager.mark_transport_disconnected(25_000);
        assert_eq!(manager.status().watchdog_deadline_b, Some(46_000));
        assert_eq!(
            manager.poll_watchdog(46_000).status,
            ControlledStatusKind::Error
        );
    }

    #[test]
    fn missing_or_expired_evidence_on_close_cleans_sensitive_state_immediately() {
        for missing in [false, true] {
            let cleanup = Arc::new(CleanupCounter::default());
            let manager = ControlledDeviceManager::default();
            manager.register_cleanup_sink(cleanup.clone());
            let operation = manager.start(None, Vec::new()).unwrap();
            register(&manager, &operation);
            manager.mark_claimed(operation.clone()).unwrap();
            manager
                .complete_connected(operation.clone(), lease(&operation.generation))
                .unwrap();
            if missing {
                manager.inner.lock().watchdog = None;
            }
            assert_eq!(
                manager.mark_transport_disconnected(33_000).status,
                ControlledStatusKind::Error
            );
            assert!(manager.inner.lock().lease_context.is_none());
            assert!(manager.inner.lock().resume_gate.is_none());
            assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
        }
    }

    #[test]
    fn invalid_resumed_cycle_metadata_cleans_generation_and_notifies_once() {
        for (deadline, epoch, now) in [
            (0, "live-a-2", 7_000),
            (60_000, "", 7_000),
            (60_000, "invalid epoch", 7_000),
            (60_000, "live~a-2", 7_000),
            (60_000, "live-a-2", u64::MAX),
        ] {
            let cleanup = Arc::new(CleanupCounter::default());
            let manager = ControlledDeviceManager::default();
            manager.register_cleanup_sink(cleanup.clone());
            let operation = manager.start(None, Vec::new()).unwrap();
            register(&manager, &operation);
            manager.mark_claimed(operation.clone()).unwrap();
            manager
                .complete_connected(operation.clone(), lease(&operation.generation))
                .unwrap();
            manager.mark_transport_disconnected(5_000);
            assert_eq!(
                manager
                    .complete_resumed(operation, epoch, [3; 32], [4; 32], now, deadline)
                    .unwrap_err()
                    .code,
                "desktop_remote_resume_failed"
            );
            assert_eq!(manager.status().status, ControlledStatusKind::Error);
            let inner = manager.inner.lock();
            assert!(inner.lease_context.is_none());
            assert!(inner.resume_gate.is_none());
            assert!(inner.watchdog.is_none());
            assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
        }
    }

    #[test]
    fn watchdog_blackhole_reconnects_then_fails_locally_without_relay() {
        let cleanup = Arc::new(CleanupCounter::default());
        let manager = ControlledDeviceManager::default();
        manager.register_cleanup_sink(cleanup.clone());
        let operation = manager
            .start(
                None,
                vec![WorkspaceAuthorizationRequest {
                    path: PathBuf::from(r"D:\private"),
                    display_name: "Private".into(),
                    code_allowed: true,
                }],
            )
            .unwrap();
        register(&manager, &operation);
        manager.mark_claimed(operation.clone()).unwrap();
        manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        manager
            .register_liveness_challenge("challenge-0001", "AAAAAAAAAAAAAAAAAAAAAA==", 1_000)
            .unwrap();
        manager
            .accept_direct_liveness(&direct_liveness(&operation.generation), 2_000)
            .unwrap();
        assert!(manager.status().watchdog_deadline_b.is_some());
        assert!(!manager.status().watchdog_suspect);
        assert_eq!(
            manager.poll_watchdog(17_000).status,
            ControlledStatusKind::Reconnecting
        );
        let status = manager.poll_watchdog(31_000);
        assert_eq!(status.status, ControlledStatusKind::Error);
        assert_eq!(status.error_code, Some("desktop_remote_watchdog_expired"));
        assert_eq!(status.workspace_count, 0);
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn stop_and_disconnect_clear_all_sensitive_context_without_network() {
        let cleanup = Arc::new(CleanupCounter::default());
        let manager = ControlledDeviceManager::default();
        manager.register_cleanup_sink(cleanup.clone());
        let operation = manager.start(Some("B"), Vec::new()).unwrap();
        manager
            .complete_registration(
                operation,
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: 30_000,
                },
                1_000,
            )
            .unwrap();
        let stopped = manager.stop();
        assert_eq!(stopped.status, ControlledStatusKind::Stopped);
        assert_eq!(stopped.pairing_code, None);
        assert_eq!(stopped.generation, None);
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);

        let second = manager.start(Some("B"), Vec::new()).unwrap();
        register(&manager, &second);
        manager.mark_claimed(second.clone()).unwrap();
        manager
            .complete_connected(second.clone(), lease(&second.generation))
            .unwrap();
        let disconnected = manager.disconnect_now();
        assert_eq!(disconnected.status, ControlledStatusKind::Stopped);
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 2);
    }

    #[test]
    fn failed_revocation_fails_closed_and_rejects_workspace_paths_in_projection() {
        let cleanup = Arc::new(CleanupCounter::default());
        let manager = ControlledDeviceManager::default();
        manager.register_cleanup_sink(cleanup.clone());
        let operation = manager
            .start(
                None,
                vec![WorkspaceAuthorizationRequest {
                    path: PathBuf::from(r"D:\secret"),
                    display_name: "Secret".into(),
                    code_allowed: true,
                }],
            )
            .unwrap();
        let refresh = manager.begin_refresh().unwrap();
        let status = manager.fail_refresh(refresh).unwrap();
        assert_eq!(status.status, ControlledStatusKind::Error);
        assert_eq!(status.error_code, Some("desktop_remote_refresh_failed"));
        assert_eq!(status.workspace_count, 0);
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
        assert_eq!(
            manager
                .authorize_workspace(WorkspaceAuthorizationRequest {
                    path: PathBuf::from(r"D:\other"),
                    display_name: "Other".into(),
                    code_allowed: false,
                })
                .unwrap_err()
                .code,
            "desktop_remote_not_running"
        );
        assert_eq!(operation.intent, 1);
    }

    #[test]
    fn non_windows_platforms_do_not_claim_supported_and_pairing_code_grammar_is_closed() {
        for code in ["1234567", "0123456", "234567", "234567I", "2345670"] {
            assert!(validate_pairing_code(code).is_err(), "{code}");
        }
        assert!(validate_pairing_code("2345678").is_ok());
    }
}
