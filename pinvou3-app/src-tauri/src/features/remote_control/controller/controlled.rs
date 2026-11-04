//! Windows controlled-device (B) lifecycle owner.
//!
//! The manager keeps only in-memory pairing and lease material. A monotonic
//! intent/generation pair gates every late network result. Preconnection has
//! separate, nonrenewable local deadlines; after authentication DEV-08's A
//! watchdog remains the sole source of B's liveness/resume safety deadline.

use std::path::PathBuf;
use std::sync::Arc;

use parking_lot::Mutex;
use serde::Serialize;
use uuid::Uuid;
use zeroize::{Zeroize, Zeroizing};

use super::crypto::{AuthenticatedRelayTrustConfig, authenticate_relay_liveness_attestation};
use super::journal::{JournalOwner, RemoteJournal};
use super::liveness::{
    ControllerLivenessWatchdog, LivenessWatchdogState, MAX_LIVENESS_ATTESTATION_AGE_MS,
};
use super::protocol::LeaseContext;
use super::relay_transport::ControllerTransportSender;
use super::resume::{ControllerResumeGate, ResumePhase};

/// A and B use different physical transports. Both must consume a real,
/// current-reader final proof; absence of either implementation fails closed.
pub(crate) trait ControlledTransportAuthority: Send + Sync {
    fn restore_authenticated_lease_after_resume(
        &self,
        context: &LeaseContext,
        activation_id: &str,
        request_id: &str,
    ) -> bool;
    fn is_stopped(&self) -> bool;
    fn stop(&self);
    /// Hold exact transport authority through a synchronous business mutation.
    fn with_business_authority(&self, expected: &LeaseContext, action: &mut dyn FnMut()) -> bool;
}

impl ControlledTransportAuthority for ControllerTransportSender {
    fn restore_authenticated_lease_after_resume(
        &self,
        context: &LeaseContext,
        activation_id: &str,
        request_id: &str,
    ) -> bool {
        ControllerTransportSender::restore_authenticated_lease_after_resume(
            self,
            context,
            activation_id,
            request_id,
        )
        .is_ok()
    }
    fn is_stopped(&self) -> bool {
        ControllerTransportSender::is_stopped(self)
    }
    fn stop(&self) {
        ControllerTransportSender::stop(self);
    }
    fn with_business_authority(&self, expected: &LeaseContext, action: &mut dyn FnMut()) -> bool {
        ControllerTransportSender::with_business_authority(self, expected, action)
    }
}

const MAX_DEVICE_NAME_CHARS: usize = 80;
const MAX_WORKSPACES: usize = 16;
const MAX_DISPLAY_NAME_CHARS: usize = 120;

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum ControlledStatusKind {
    Disabled,
    Registering,
    WaitingForPairing,
    PairingReserved,
    HandshakePending,
    Connected,
    Reconnecting,
    ResumeProvisional,
    ResumeActivatedPendingLiveness,
    RetiringGeneration,
    Stopping,
    Stopped,
    Error,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ControlledOperation {
    intent: u64,
    generation: String,
    registration: Option<ControlledTransportRegistration>,
}

impl ControlledOperation {
    pub(crate) fn for_lane(mut self, owner: &ControlledTransportOwner) -> Self {
        self.registration = Some(ControlledTransportRegistration {
            lease: owner.lease.clone(),
            transport: owner.transport.clone(),
        });
        self
    }

    pub(crate) fn generation(&self) -> &str {
        &self.generation
    }
}

pub(crate) const MAX_HANDSHAKE_TTL_MS: u64 = 20_000;

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
enum PreconnectPhase {
    Waiting,
    Handshake,
}

/// An immutable timer ticket. A late timer must match this exact operation and
/// preconnection phase, even if its operation has since become Connected.
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ControlledPreconnectDeadline {
    operation: ControlledOperation,
    phase: PreconnectPhase,
    deadline_b: u64,
}

impl ControlledPreconnectDeadline {
    pub(crate) fn deadline_b(&self) -> u64 {
        self.deadline_b
    }

    fn error_code(&self) -> &'static str {
        match self.phase {
            PreconnectPhase::Waiting => "desktop_remote_pairing_expired",
            PreconnectPhase::Handshake => "desktop_remote_handshake_timeout",
        }
    }

    fn matches_phase(&self, phase: ControlledStatusKind) -> bool {
        match self.phase {
            PreconnectPhase::Waiting => phase == ControlledStatusKind::WaitingForPairing,
            PreconnectPhase::Handshake => matches!(
                phase,
                ControlledStatusKind::PairingReserved | ControlledStatusKind::HandshakePending
            ),
        }
    }
}

#[derive(Clone)]
struct ControlledTransportRegistration {
    lease: LeaseContext,
    transport: Arc<dyn ControlledTransportAuthority>,
}

impl PartialEq for ControlledTransportRegistration {
    fn eq(&self, other: &Self) -> bool {
        self.lease == other.lease && Arc::ptr_eq(&self.transport, &other.transport)
    }
}

impl Eq for ControlledTransportRegistration {}

impl std::fmt::Debug for ControlledTransportRegistration {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("ControlledTransportRegistration([REDACTED])")
    }
}

/// Original authenticated worker registration. The Arc keeps the registered
/// transport allocation alive; equality is checked under the lifecycle lock,
/// independently of whether its socket is temporarily disconnected.
#[derive(Clone)]
pub(crate) struct ControlledTransportOwner {
    operation: ControlledOperation,
    lease: LeaseContext,
    transport: Arc<dyn ControlledTransportAuthority>,
}

impl ControlledTransportOwner {
    pub(crate) fn operation(&self) -> &ControlledOperation {
        &self.operation
    }

    pub(crate) fn lease(&self) -> &LeaseContext {
        &self.lease
    }
}

impl std::fmt::Debug for ControlledTransportOwner {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("ControlledTransportOwner([REDACTED])")
    }
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
    /// 验签 transcript 协商出的能力集合（E2E-04 解密业务帧的准入上下文）。
    pub(crate) negotiated_capabilities: Vec<String>,
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
    identity: super::super::platform::WorkspaceIdentity,
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
    status_intent: String,
    generation: Option<String>,
    registration_request_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pairing_code: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pairing_remaining_ms: Option<u64>,
    device_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    controller_device_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    connected_duration_ms: Option<u64>,
    workspace_count: usize,
    code_mode_allowed: bool,
    #[serde(skip_serializing)]
    watchdog_deadline_b: Option<u64>,
    watchdog_suspect: bool,
    error_code: Option<String>,
}

impl ControlledStatus {
    pub(crate) fn kind(&self) -> ControlledStatusKind {
        self.status
    }
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
            .field("pairing_remaining_ms", &self.pairing_remaining_ms)
            .field("device_name", &self.device_name)
            .field("controller_device_name", &self.controller_device_name)
            .field("connected_duration_ms", &self.connected_duration_ms)
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

/// E2E-06：B 端断线恢复所需的本地材料（令牌只以内存数组存在，Debug 脱敏）。
pub(crate) struct ControlledResumeMaterial {
    pub(crate) lease: LeaseContext,
    pub(crate) resume_token: [u8; 32],
    pub(crate) relay_deadline_identity_ms: u64,
}

impl std::fmt::Debug for ControlledResumeMaterial {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ControlledResumeMaterial")
            .field("lease", &self.lease)
            .field("resume_token", &"[REDACTED]")
            .field(
                "relay_deadline_identity_ms",
                &self.relay_deadline_identity_ms,
            )
            .finish()
    }
}

/// E2E-06：B 端恢复相位推进（worker 按 Relay/A 的帧顺序调用）。
#[derive(Debug, Clone, Copy)]
pub(crate) enum ControlledResumeStep<'a> {
    Provisional {
        activation_id: &'a str,
    },
    Confirmed {
        activation_id: &'a str,
    },
    ActivationAcknowledged {
        activation_id: &'a str,
    },
    PostActivationLiveness {
        activation_id: &'a str,
        liveness_epoch_b: &'a str,
    },
    ReadyAcknowledged {
        activation_id: &'a str,
    },
}

/// B 端恢复失败时投影的本地错误码（有界，UI 直接显示为失败终态）。
const RESUME_FAILED_CODE: &str = "desktop_remote_resume_invalid";

/// `reconnecting` 同时承载真实传输断线和本地活性可疑两种 UI 投影，但二者的
/// 恢复权限不同：前者必须走完整 resume，后者仍应允许当前认证通道提交存活证明。
#[derive(Debug, Clone, Copy, Eq, PartialEq)]
enum ReconnectReason {
    Transport,
    LivenessSuspect,
}

/// Only the lifecycle manager can construct or retain issuance authority.
/// Consumers receive the journal view and opaque owner handles separately.
pub(super) struct JournalAuthority {
    journal: Arc<RemoteJournal>,
}

impl JournalAuthority {
    pub(super) fn controls(&self, journal: &RemoteJournal) -> bool {
        std::ptr::eq(self.journal.as_ref(), journal)
    }
}

struct Inner {
    phase: ControlledStatusKind,
    intent: u64,
    generation: Option<String>,
    /// Terminal projection only; live admission reads `generation` instead.
    retired_generation: Option<String>,
    registration_request_id: Option<String>,
    device_name: Option<String>,
    pairing: Option<PairingState>,
    preconnect_deadline: Option<ControlledPreconnectDeadline>,
    lease_context: Option<LeaseContext>,
    journal_authority: JournalAuthority,
    journal_owner: Option<JournalOwner>,
    negotiated_capabilities: Vec<String>,
    liveness_epoch_a: Option<String>,
    controller_device_name: Option<String>,
    connected_at_b: Option<u64>,
    watchdog: Option<ControllerLivenessWatchdog>,
    resume_gate: Option<ControllerResumeGate>,
    reconnect_reason: Option<ReconnectReason>,
    last_observed_now_b: u64,
    transport: Option<Arc<dyn ControlledTransportAuthority>>,
    workspaces: Vec<WorkspaceAuthorization>,
    error_code: Option<String>,
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
            retired_generation: None,
            registration_request_id: None,
            device_name: None,
            pairing: None,
            preconnect_deadline: None,
            lease_context: None,
            journal_authority: JournalAuthority {
                journal: Arc::new(RemoteJournal::new(format!("boot-{}", Uuid::new_v4()))),
            },
            journal_owner: None,
            negotiated_capabilities: Vec::new(),
            liveness_epoch_a: None,
            controller_device_name: None,
            connected_at_b: None,
            watchdog: None,
            resume_gate: None,
            reconnect_reason: None,
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
    identity: super::super::platform::WorkspaceIdentity,
}

impl WorkspaceAuthorizationRequest {
    pub(crate) fn new(path: PathBuf, display_name: String, code_allowed: bool) -> Result<Self> {
        if !path.is_dir() {
            return Err(ControlledStateError::new("workspace_path_invalid"));
        }
        // Keep the directory identity next to the authorization. Resolving an
        // opaque workspace id for a code turn must prove that the path still
        // names the directory that B approved, not a replacement at the same
        // textual path.
        let identity = super::super::platform::workspace_identity(&path)
            .map_err(|_| ControlledStateError::new("workspace_identity_unavailable"))?;
        Ok(Self {
            path,
            display_name,
            code_allowed,
            identity,
        })
    }
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
        inner.retired_generation = None;
        let authorization = normalize_workspaces(workspaces)?;
        let operation = prepare_generation(&mut inner);
        inner.device_name = device_name.map(str::to_owned);
        inner.workspaces = authorization;
        inner.phase = ControlledStatusKind::Registering;
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

    pub(crate) fn current_operation(&self) -> Option<ControlledOperation> {
        current_operation(&self.inner.lock())
    }

    pub(crate) fn current_generation(&self) -> Option<String> {
        self.inner.lock().generation.clone()
    }

    #[cfg(test)]
    pub(crate) fn complete_registration(
        &self,
        operation: ControlledOperation,
        registration: ControlledRegistration,
        now_b: u64,
    ) -> Result<ControlledStatus> {
        self.complete_registration_at(operation, registration, || now_b)
    }

    pub(crate) fn complete_registration_at(
        &self,
        operation: ControlledOperation,
        registration: ControlledRegistration,
        now: impl FnOnce() -> u64,
    ) -> Result<ControlledStatus> {
        validate_pairing_code(&registration.pairing_code)?;
        if registration.sealed_code_b64.is_empty() {
            return Err(ControlledStateError::new("sealed_code_invalid"));
        }
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::Registering {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        let now_b = now();
        if now_b >= registration.expires_at_ms {
            return Err(ControlledStateError::new("pairing_code_expired"));
        }
        let registration_request_id = inner
            .registration_request_id
            .clone()
            .ok_or_else(|| ControlledStateError::new("registration_request_unavailable"))?;
        inner.preconnect_deadline = Some(ControlledPreconnectDeadline {
            operation,
            phase: PreconnectPhase::Waiting,
            deadline_b: registration.expires_at_ms,
        });
        inner.pairing = Some(PairingState {
            registration_request_id,
            sealed_code_b64: registration.sealed_code_b64,
            pairing_code: Zeroizing::new(registration.pairing_code),
            expires_at_ms: registration.expires_at_ms,
        });
        inner.phase = ControlledStatusKind::WaitingForPairing;
        inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
        Ok(status_locked(&inner))
    }

    /// Only after the Relay-signed reservation transcript was verified.
    pub(crate) fn mark_pairing_reserved_at(
        &self,
        operation: ControlledOperation,
        handshake_deadline_b: u64,
        now: impl FnOnce() -> u64,
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::WaitingForPairing {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        let now_b = now();
        require_preconnect_live(&inner, now_b)?;
        if handshake_deadline_b <= now_b {
            return Err(ControlledStateError::new(
                "desktop_remote_handshake_timeout",
            ));
        }
        if handshake_deadline_b - now_b > MAX_HANDSHAKE_TTL_MS {
            return Err(ControlledStateError::new(
                "desktop_remote_handshake_deadline_invalid",
            ));
        }
        if let Some(pairing) = inner.pairing.as_mut() {
            pairing.pairing_code.zeroize();
        }
        inner.pairing = None;
        inner.preconnect_deadline = Some(ControlledPreconnectDeadline {
            operation,
            phase: PreconnectPhase::Handshake,
            deadline_b: handshake_deadline_b,
        });
        inner.phase = ControlledStatusKind::PairingReserved;
        inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
        Ok(status_locked(&inner))
    }

    #[cfg(test)]
    pub(crate) fn mark_pairing_reserved(
        &self,
        operation: ControlledOperation,
    ) -> Result<ControlledStatus> {
        let now_b = self.inner.lock().last_observed_now_b;
        let deadline = now_b
            .checked_add(MAX_HANDSHAKE_TTL_MS)
            .ok_or_else(|| ControlledStateError::new("desktop_remote_handshake_timeout"))?;
        self.mark_pairing_reserved_at(operation, deadline, || now_b)
    }

    /// The authenticated receipt commitment has been queued to Relay.
    pub(crate) fn mark_handshake_pending_at(
        &self,
        operation: ControlledOperation,
        now: impl FnOnce() -> u64,
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::PairingReserved {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        let now_b = now();
        require_preconnect_live(&inner, now_b)?;
        inner.phase = ControlledStatusKind::HandshakePending;
        inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
        Ok(status_locked(&inner))
    }

    #[cfg(test)]
    pub(crate) fn mark_handshake_pending(
        &self,
        operation: ControlledOperation,
    ) -> Result<ControlledStatus> {
        let now_b = self.inner.lock().last_observed_now_b;
        self.mark_handshake_pending_at(operation, || now_b)
    }

    pub(crate) fn preconnect_deadline(
        &self,
        operation: &ControlledOperation,
    ) -> Result<Option<ControlledPreconnectDeadline>> {
        let inner = self.inner.lock();
        ensure_operation(&inner, operation)?;
        Ok(inner.preconnect_deadline.clone())
    }

    pub(crate) fn expire_preconnect(
        &self,
        ticket: &ControlledPreconnectDeadline,
        now: impl FnOnce() -> u64,
    ) -> Result<Option<ControlledStatus>> {
        let status;
        {
            let mut inner = self.inner.lock();
            ensure_operation(&inner, &ticket.operation)?;
            if inner.preconnect_deadline.as_ref() != Some(ticket)
                || !ticket.matches_phase(inner.phase)
            {
                return Ok(None);
            }
            let now_b = now();
            if now_b < ticket.deadline_b {
                return Ok(None);
            }
            let retired = inner.generation.clone();
            clear_sensitive(&mut inner, now_b, true);
            inner.phase = ControlledStatusKind::Error;
            inner.error_code = Some(ticket.error_code().to_owned());
            status = terminal_status_with_retired_generation(&inner, retired);
        }
        self.notify_cleanup(ticket.operation.generation());
        super::relay_runtime::log_controlled_event("preconnect_expired", ticket.error_code());
        Ok(Some(status))
    }

    // Preserve the original late-liveness red regression's fixture setup.
    // Production uses the two independently checked lifecycle transitions.
    #[cfg(test)]
    fn mark_claimed(&self, operation: ControlledOperation) -> Result<ControlledStatus> {
        self.mark_pairing_reserved(operation.clone())?;
        self.mark_handshake_pending(operation)
    }

    #[cfg(test)]
    pub(crate) fn complete_connected(
        &self,
        operation: ControlledOperation,
        lease: ControlledLease,
    ) -> Result<ControlledStatus> {
        let now_b = lease.connected_at_b;
        self.complete_connected_inner(operation, lease, || now_b, || Ok(None))
            .map(|(status, _)| status)
    }

    /// Final admission samples the local clock after acquiring the lifecycle
    /// owner. The synchronous install may take R/lane/outbound locks, never C.
    /// Validate before every shared install and commit Connected in one C lock.
    /// The callback may take R/S/lane/outbound locks, never re-enter this owner.
    pub(crate) fn complete_connected_with_install(
        &self,
        operation: ControlledOperation,
        lease: ControlledLease,
        now: impl FnOnce() -> u64,
        install: impl FnOnce() -> Result<Arc<dyn ControlledTransportAuthority>>,
    ) -> Result<(ControlledStatus, ControlledTransportOwner)> {
        let (status, owner) =
            self.complete_connected_inner(operation, lease, now, || install().map(Some))?;
        owner
            .map(|owner| (status, owner))
            .ok_or_else(|| ControlledStateError::new("desktop_remote_transport_unavailable"))
    }

    fn complete_connected_inner(
        &self,
        operation: ControlledOperation,
        lease: ControlledLease,
        now: impl FnOnce() -> u64,
        install: impl FnOnce() -> Result<Option<Arc<dyn ControlledTransportAuthority>>>,
    ) -> Result<(ControlledStatus, Option<ControlledTransportOwner>)> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::HandshakePending {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        let now_b = now();
        require_preconnect_live(&inner, now_b)?;
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
        let journal_owner = inner
            .journal_authority
            .journal
            .activate_owner(&inner.journal_authority, &operation.generation)
            .map_err(|_| ControlledStateError::new("desktop_remote_phase_invalid"))?;
        inner.journal_owner = Some(journal_owner);
        if let Some(transport) = install()? {
            if let Some(previous) = inner.transport.replace(transport) {
                previous.stop();
            }
        }
        let owner = inner
            .transport
            .as_ref()
            .map(|transport| ControlledTransportOwner {
                operation,
                lease: lease.context.clone(),
                transport: transport.clone(),
            });
        inner.lease_context = Some(lease.context);
        inner.negotiated_capabilities = lease.negotiated_capabilities;
        inner.liveness_epoch_a = Some(lease.liveness_epoch_a);
        inner.controller_device_name = Some(lease.controller_device_name);
        inner.connected_at_b = Some(lease.connected_at_b);
        inner.last_observed_now_b = inner
            .last_observed_now_b
            .max(lease.connected_at_b)
            .max(now_b);
        inner.watchdog = Some(watchdog);
        inner.resume_gate = Some(resume_gate);
        inner.reconnect_reason = None;
        inner.pairing = None;
        inner.preconnect_deadline = None;
        inner.phase = ControlledStatusKind::Connected;
        Ok((status_locked(&inner), owner))
    }

    pub(crate) fn with_transport_owner<T>(
        &self,
        owner: &ControlledTransportOwner,
        action: impl FnOnce() -> T,
    ) -> Result<T> {
        let inner = self.inner.lock();
        ensure_transport_owner(&inner, owner)?;
        Ok(action())
    }

    /// Test fixtures capture the already-installed real registration before
    /// disconnect/replacement; this mints no reader or authentication proof.
    #[cfg(test)]
    pub(super) fn transport_owner_for_test(
        &self,
        operation: &ControlledOperation,
    ) -> Result<ControlledTransportOwner> {
        let inner = self.inner.lock();
        ensure_operation(&inner, operation)?;
        Ok(ControlledTransportOwner {
            operation: operation.clone(),
            lease: inner
                .lease_context
                .clone()
                .ok_or_else(|| ControlledStateError::new("desktop_remote_transport_unavailable"))?,
            transport: inner
                .transport
                .clone()
                .ok_or_else(|| ControlledStateError::new("desktop_remote_transport_unavailable"))?,
        })
    }

    #[cfg(test)]
    pub(super) fn lifecycle_lock_held_for_test(&self) -> bool {
        self.inner.try_lock().is_none()
    }

    #[cfg(test)]
    pub(crate) fn replace_authenticated_transport<T: ControlledTransportAuthority + 'static>(
        &self,
        operation: ControlledOperation,
        transport: T,
    ) -> Result<()> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if let Some(previous) = inner.transport.replace(Arc::new(transport)) {
            previous.stop();
        }
        Ok(())
    }

    #[cfg(test)]
    pub(crate) fn replace_transport<T: ControlledTransportAuthority + 'static>(
        &self,
        transport: T,
    ) {
        let mut inner = self.inner.lock();
        if let Some(previous) = inner.transport.replace(Arc::new(transport)) {
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

    #[cfg(test)]
    pub(crate) fn mark_transport_disconnected(&self, now_b: u64) -> ControlledStatus {
        self.mark_transport_disconnected_inner(None, now_b, || {})
            .expect("unscoped state fixture has no owner rejection")
    }

    pub(crate) fn mark_owned_transport_disconnected(
        &self,
        owner: &ControlledTransportOwner,
        now_b: u64,
        suspend: impl FnOnce(),
    ) -> Result<ControlledStatus> {
        self.mark_transport_disconnected_inner(Some(owner), now_b, suspend)
    }

    fn mark_transport_disconnected_inner(
        &self,
        owner: Option<&ControlledTransportOwner>,
        now_b: u64,
        suspend: impl FnOnce(),
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        if let Some(owner) = owner {
            ensure_transport_owner(&inner, owner)?;
        }
        suspend();
        inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
        if (inner.phase == ControlledStatusKind::Connected
            || (inner.phase == ControlledStatusKind::Reconnecting
                && inner.reconnect_reason == Some(ReconnectReason::LivenessSuspect)))
            && inner
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
                inner.reconnect_reason = Some(ReconnectReason::Transport);
            } else {
                let generation = inner.generation.clone().unwrap_or_default();
                clear_sensitive(&mut inner, now_b, true);
                inner.device_name = None;
                inner.phase = ControlledStatusKind::Error;
                inner.error_code = Some(RESUME_FAILED_CODE.to_owned());
                let status = status_locked(&inner);
                drop(inner);
                self.notify_cleanup(&generation);
                return Ok(status);
            }
        } else if matches!(
            inner.phase,
            ControlledStatusKind::ResumeProvisional
                | ControlledStatusKind::ResumeActivatedPendingLiveness
        ) {
            let generation = inner.generation.clone().unwrap_or_default();
            clear_sensitive(&mut inner, now_b, true);
            inner.device_name = None;
            inner.phase = ControlledStatusKind::Error;
            inner.error_code = Some("desktop_remote_resume_invalid".to_owned());
            inner.reconnect_reason = None;
            let status = status_locked(&inner);
            drop(inner);
            self.notify_cleanup(&generation);
            return Ok(status);
        }
        let status = status_locked(&inner);
        drop(inner);
        Ok(status)
    }

    #[cfg(test)]
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

    /// 本地未成功发出或已超时的挑战必须从 watchdog 一并撤销，否则 worker 的
    /// 轻量状态虽已 reset，watchdog 内仍会累积到容量上限。
    #[cfg(test)]
    pub(crate) fn cancel_liveness_challenge(&self, challenge_id: &str) {
        let mut inner = self.inner.lock();
        if let Some(watchdog) = inner.watchdog.as_mut() {
            watchdog.cancel_challenge(challenge_id);
        }
    }

    pub(crate) fn cancel_owned_liveness_challenge(
        &self,
        owner: &ControlledTransportOwner,
        challenge_id: &str,
    ) -> Result<()> {
        let mut inner = self.inner.lock();
        ensure_transport_owner(&inner, owner)?;
        if let Some(watchdog) = inner.watchdog.as_mut() {
            watchdog.cancel_challenge(challenge_id);
        }
        Ok(())
    }

    /// E2E-06：是否需要向对端/Relay 索取存活证据（watchdog 政策）。
    pub(crate) fn liveness_evidence_needed(&self, now_b: u64) -> bool {
        let inner = self.inner.lock();
        inner
            .watchdog
            .as_ref()
            .is_some_and(|watchdog| watchdog.needs_evidence(now_b))
    }

    /// E2E-06：开启一次存活挑战（生成 + 登记）并返回 (challenge_id, challenge_b64)。
    #[cfg(test)]
    pub(crate) fn begin_liveness_challenge(&self, now_b: u64) -> Result<(String, String)> {
        self.begin_liveness_challenge_inner(None, now_b)
            .map(|(_, challenge_id, challenge_b64)| (challenge_id, challenge_b64))
    }

    /// The captured worker owns both this watchdog mutation and the returned
    /// lease. Do not check an owner, unlock, then register in the current one.
    pub(crate) fn begin_owned_liveness_challenge(
        &self,
        owner: &ControlledTransportOwner,
        now_b: u64,
    ) -> Result<(LeaseContext, String, String)> {
        self.begin_liveness_challenge_inner(Some(owner), now_b)
    }

    fn begin_liveness_challenge_inner(
        &self,
        owner: Option<&ControlledTransportOwner>,
        now_b: u64,
    ) -> Result<(LeaseContext, String, String)> {
        let (challenge_id, challenge_b64) = super::liveness::new_liveness_challenge()
            .map_err(|_| ControlledStateError::new("liveness_challenge_invalid"))?;
        let mut inner = self.inner.lock();
        if let Some(owner) = owner {
            ensure_transport_owner(&inner, owner)?;
        }
        ensure_connected_evidence(&inner)?;
        let context = inner
            .lease_context
            .clone()
            .ok_or_else(|| ControlledStateError::new("lease_unavailable"))?;
        let Some(watchdog) = inner.watchdog.as_mut() else {
            return Err(ControlledStateError::new("watchdog_unavailable"));
        };
        watchdog
            .register_challenge(challenge_id.clone(), challenge_b64.clone(), now_b)
            .map_err(|_| ControlledStateError::new("liveness_challenge_invalid"))?;
        Ok((context, challenge_id, challenge_b64))
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
            ControlledStatusKind::Registering
                | ControlledStatusKind::WaitingForPairing
                | ControlledStatusKind::PairingReserved
                | ControlledStatusKind::HandshakePending
                | ControlledStatusKind::Connected
                | ControlledStatusKind::Reconnecting
                | ControlledStatusKind::ResumeProvisional
                | ControlledStatusKind::ResumeActivatedPendingLiveness
        ) {
            return Err(ControlledStateError::new("desktop_remote_refresh_invalid"));
        }
        revoke_journal_owner(&mut inner);
        inner.intent += 1;
        if let Some(pairing) = inner.pairing.as_mut() {
            pairing.pairing_code.zeroize();
        }
        inner.pairing = None;
        inner.preconnect_deadline = None;
        inner.phase = ControlledStatusKind::RetiringGeneration;
        inner.error_code = None;
        current_operation(&inner).ok_or_else(|| ControlledStateError::new("generation_unavailable"))
    }

    pub(crate) fn complete_refresh_revoked(
        &self,
        operation: ControlledOperation,
    ) -> Result<ControlledOperation> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::RetiringGeneration {
            return Err(ControlledStateError::new("desktop_remote_refresh_invalid"));
        }
        let retired_generation = inner.generation.clone().unwrap_or_default();
        clear_sensitive(&mut inner, 0, false);
        let next = prepare_generation(&mut inner);
        inner.phase = ControlledStatusKind::Registering;
        drop(inner);
        self.notify_cleanup(&retired_generation);
        Ok(next)
    }

    pub(crate) fn fail_refresh(&self, operation: ControlledOperation) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::RetiringGeneration {
            return Err(ControlledStateError::new("desktop_remote_refresh_invalid"));
        }
        let retired_generation = inner.generation.clone();
        let generation = retired_generation.clone().unwrap_or_default();
        clear_sensitive(&mut inner, u64::MAX, true);
        inner.device_name = None;
        inner.phase = ControlledStatusKind::Error;
        inner.error_code = Some("desktop_remote_refresh_failed".to_owned());
        let status = terminal_status_with_retired_generation(&inner, retired_generation);
        drop(inner);
        self.notify_cleanup(&generation);
        Ok(status)
    }

    pub(crate) fn fail_operation(
        &self,
        operation: ControlledOperation,
        error_code: String,
    ) -> Result<ControlledStatus> {
        self.fail_operation_with_retired_lease(operation, error_code)
            .map(|(status, _)| status)
    }

    /// A failed preconnection install cannot clear an already Connected lane
    /// or a successor operation while its error is returning to the worker.
    pub(crate) fn fail_preconnect_with_cleanup(
        &self,
        operation: ControlledOperation,
        error_code: String,
        cleanup: impl FnOnce(),
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if !inner
            .preconnect_deadline
            .as_ref()
            .is_some_and(|ticket| ticket.matches_phase(inner.phase))
        {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        cleanup();
        let retired_generation = inner.generation.clone();
        clear_sensitive(&mut inner, u64::MAX, true);
        inner.device_name = None;
        inner.phase = ControlledStatusKind::Error;
        inner.error_code = Some(error_code);
        let status = terminal_status_with_retired_generation(&inner, retired_generation);
        drop(inner);
        self.notify_cleanup(operation.generation());
        Ok(status)
    }

    /// Capture cleanup identity under the same lock as retiring its operation.
    pub(crate) fn fail_operation_with_retired_lease(
        &self,
        operation: ControlledOperation,
        error_code: String,
    ) -> Result<(ControlledStatus, Option<LeaseContext>)> {
        self.fail_operation_with_cleanup_inner(operation, None, error_code, || {})
    }

    pub(crate) fn fail_owned_transport(
        &self,
        owner: &ControlledTransportOwner,
        error_code: String,
    ) -> Result<ControlledStatus> {
        self.fail_operation_with_cleanup_inner(
            owner.operation.clone(),
            Some(owner),
            error_code,
            || {},
        )
        .map(|(status, _)| status)
    }

    fn fail_operation_with_cleanup_inner(
        &self,
        operation: ControlledOperation,
        owner: Option<&ControlledTransportOwner>,
        error_code: String,
        cleanup: impl FnOnce(),
    ) -> Result<(ControlledStatus, Option<LeaseContext>)> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if let Some(owner) = owner {
            ensure_transport_owner(&inner, owner)?;
        }
        cleanup();
        let retired_lease = inner.lease_context.clone();
        let retired_generation = inner.generation.clone();
        let generation = retired_generation.clone().unwrap_or_default();
        clear_sensitive(&mut inner, u64::MAX, true);
        inner.device_name = None;
        inner.phase = ControlledStatusKind::Error;
        inner.error_code = Some(error_code);
        let status = terminal_status_with_retired_generation(&inner, retired_generation);
        drop(inner);
        self.notify_cleanup(&generation);
        Ok((status, retired_lease))
    }

    #[cfg(test)]
    pub(crate) fn complete_resumed(
        &self,
        operation: ControlledOperation,
        activation_id: &str,
        resume_request_id: &str,
        now_b: u64,
        liveness_epoch_a: impl Into<String>,
        resume_token: [u8; 32],
        transport_key_fingerprint: [u8; 32],
        relay_resume_deadline_ms: u64,
    ) -> Result<ControlledStatus> {
        self.complete_resumed_inner(
            operation,
            None,
            activation_id,
            resume_request_id,
            now_b,
            liveness_epoch_a.into(),
            resume_token,
            transport_key_fingerprint,
            relay_resume_deadline_ms,
            || {},
        )
    }

    pub(crate) fn complete_owned_resume(
        &self,
        owner: &ControlledTransportOwner,
        activation_id: &str,
        resume_request_id: &str,
        now_b: u64,
        liveness_epoch_a: impl Into<String>,
        resume_token: [u8; 32],
        transport_key_fingerprint: [u8; 32],
        relay_resume_deadline_ms: u64,
        activate: impl FnOnce(),
    ) -> Result<ControlledStatus> {
        self.complete_resumed_inner(
            owner.operation.clone(),
            Some(owner),
            activation_id,
            resume_request_id,
            now_b,
            liveness_epoch_a.into(),
            resume_token,
            transport_key_fingerprint,
            relay_resume_deadline_ms,
            activate,
        )
    }

    fn complete_resumed_inner(
        &self,
        operation: ControlledOperation,
        owner: Option<&ControlledTransportOwner>,
        activation_id: &str,
        resume_request_id: &str,
        now_b: u64,
        liveness_epoch_a: String,
        resume_token: [u8; 32],
        transport_key_fingerprint: [u8; 32],
        relay_resume_deadline_ms: u64,
        activate: impl FnOnce(),
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if let Some(owner) = owner {
            ensure_transport_owner(&inner, owner)?;
        }
        if inner.phase != ControlledStatusKind::ResumeActivatedPendingLiveness {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        let Some(context) = inner.lease_context.clone() else {
            return Err(ControlledStateError::new("lease_unavailable"));
        };
        let epoch = liveness_epoch_a;
        let next_watchdog =
            ControllerLivenessWatchdog::authenticated_cycle(epoch.clone(), now_b).ok();
        // E2E-06：先用完整的 `controller_resumed` 让旧 gate 消费旧材料；任何
        // 相位、activation 或轮换材料不符都必须终止候选，绝不产生半恢复状态。
        let watchdog_deadline = inner
            .watchdog
            .as_ref()
            .and_then(|watchdog| watchdog.deadline_b());
        let accepted = relay_resume_deadline_ms > 0
            && next_watchdog.is_some()
            && match inner.resume_gate.as_mut() {
                Some(gate) => gate
                    .accept_resumed(
                        activation_id,
                        epoch.clone(),
                        resume_token,
                        transport_key_fingerprint,
                        now_b,
                        watchdog_deadline,
                    )
                    .is_ok(),
                None => false,
            };
        let installed = accepted
            && inner.transport.as_ref().is_some_and(|transport| {
                transport.restore_authenticated_lease_after_resume(
                    &context,
                    activation_id,
                    resume_request_id,
                )
            });
        if !installed {
            let generation = inner.generation.clone().unwrap_or_default();
            clear_sensitive(&mut inner, u64::MAX, true);
            inner.device_name = None;
            inner.phase = ControlledStatusKind::Error;
            inner.error_code = Some(RESUME_FAILED_CODE.to_owned());
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
        inner.reconnect_reason = None;
        inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
        // Restore proof, phase and business activation share the final C
        // commit. The activation callback must not re-enter the manager.
        activate();
        Ok(status_locked(&inner))
    }

    /// E2E-06：断线（`desktop_relay_connection{connected:false}`）后取恢复材料并让
    /// 本端 gate 进入 `Prepared`；相位不符或 gate 已终止时失败关闭并清理本代。
    #[cfg(test)]
    pub(crate) fn begin_resume(
        &self,
        operation: ControlledOperation,
        now_b: u64,
    ) -> Result<ControlledResumeMaterial> {
        self.begin_resume_inner(operation, None, now_b)
    }

    pub(crate) fn begin_owned_resume(
        &self,
        owner: &ControlledTransportOwner,
        now_b: u64,
    ) -> Result<ControlledResumeMaterial> {
        self.begin_resume_inner(owner.operation.clone(), Some(owner), now_b)
    }

    fn begin_resume_inner(
        &self,
        operation: ControlledOperation,
        owner: Option<&ControlledTransportOwner>,
        now_b: u64,
    ) -> Result<ControlledResumeMaterial> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if let Some(owner) = owner {
            ensure_transport_owner(&inner, owner)?;
        }
        if inner.phase != ControlledStatusKind::Reconnecting {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        let Some(context) = inner.lease_context.clone() else {
            return Err(ControlledStateError::new("lease_unavailable"));
        };
        let deadline = inner
            .watchdog
            .as_ref()
            .and_then(|watchdog| watchdog.deadline_b());
        let (prepared, relay_deadline_identity_ms, resume_token) = match inner.resume_gate.as_mut()
        {
            Some(gate) => {
                let prepared = gate.prepare(now_b, deadline).is_ok();
                (
                    prepared,
                    gate.relay_deadline_identity_ms(),
                    gate.current_token(),
                )
            }
            None => (false, 0, [0_u8; 32]),
        };
        if !prepared {
            let generation = inner.generation.clone().unwrap_or_default();
            clear_sensitive(&mut inner, u64::MAX, true);
            inner.device_name = None;
            inner.phase = ControlledStatusKind::Error;
            inner.error_code = Some(RESUME_FAILED_CODE.to_owned());
            drop(inner);
            self.notify_cleanup(&generation);
            return Err(ControlledStateError::new("desktop_remote_resume_failed"));
        }
        Ok(ControlledResumeMaterial {
            lease: context,
            resume_token,
            relay_deadline_identity_ms,
        })
    }

    /// E2E-06：按帧顺序推进 B 端恢复相位；任一步失败即终止候选并清理本代。
    #[cfg(test)]
    pub(crate) fn advance_resume(
        &self,
        operation: ControlledOperation,
        step: ControlledResumeStep<'_>,
        now_b: u64,
    ) -> Result<ControlledStatus> {
        self.advance_resume_inner(operation, None, step, now_b)
    }

    pub(crate) fn advance_owned_resume(
        &self,
        owner: &ControlledTransportOwner,
        step: ControlledResumeStep<'_>,
        now_b: u64,
    ) -> Result<ControlledStatus> {
        self.advance_resume_inner(owner.operation.clone(), Some(owner), step, now_b)
    }

    fn advance_resume_inner(
        &self,
        operation: ControlledOperation,
        owner: Option<&ControlledTransportOwner>,
        step: ControlledResumeStep<'_>,
        now_b: u64,
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if let Some(owner) = owner {
            ensure_transport_owner(&inner, owner)?;
        }
        if !matches!(
            inner.phase,
            ControlledStatusKind::Reconnecting
                | ControlledStatusKind::ResumeProvisional
                | ControlledStatusKind::ResumeActivatedPendingLiveness
        ) {
            return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
        }
        let deadline = inner
            .watchdog
            .as_ref()
            .and_then(|watchdog| watchdog.deadline_b());
        let advanced = match inner.resume_gate.as_mut() {
            Some(gate) => match step {
                ControlledResumeStep::Provisional { activation_id } => gate
                    .accept_provisional(activation_id, now_b, deadline)
                    .is_ok(),
                ControlledResumeStep::Confirmed { activation_id } => {
                    gate.confirm(activation_id, now_b, deadline).is_ok()
                }
                ControlledResumeStep::ActivationAcknowledged { activation_id } => gate
                    .acknowledge_activation(activation_id, now_b, deadline)
                    .is_ok(),
                ControlledResumeStep::PostActivationLiveness {
                    activation_id,
                    liveness_epoch_b,
                } => gate
                    .send_post_activation_liveness(activation_id, liveness_epoch_b, now_b, deadline)
                    .is_ok(),
                ControlledResumeStep::ReadyAcknowledged { activation_id } => gate
                    .acknowledge_ready(activation_id, now_b, deadline)
                    .is_ok(),
            },
            None => false,
        };
        if !advanced {
            let generation = inner.generation.clone().unwrap_or_default();
            clear_sensitive(&mut inner, u64::MAX, true);
            inner.device_name = None;
            inner.phase = ControlledStatusKind::Error;
            inner.error_code = Some(RESUME_FAILED_CODE.to_owned());
            drop(inner);
            self.notify_cleanup(&generation);
            return Err(ControlledStateError::new("desktop_remote_resume_failed"));
        }
        inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
        inner.phase = match step {
            ControlledResumeStep::Provisional { .. } => ControlledStatusKind::ResumeProvisional,
            ControlledResumeStep::PostActivationLiveness { .. } => {
                ControlledStatusKind::ResumeActivatedPendingLiveness
            }
            _ => inner.phase,
        };
        Ok(status_locked(&inner))
    }

    pub(crate) fn stop(&self) -> ControlledStatus {
        let _ = self.begin_stop();
        self.finish_stop()
    }

    /// Invalidate local admission and credentials before Relay revocation.
    pub(crate) fn begin_stop(&self) -> ControlledStatus {
        self.terminate_local(
            "desktop_remote_stopped",
            ControlledStatusKind::Stopping,
            true,
        )
    }

    pub(crate) fn finish_stop(&self) -> ControlledStatus {
        let mut inner = self.inner.lock();
        if inner.phase == ControlledStatusKind::Stopping {
            inner.phase = ControlledStatusKind::Stopped;
        }
        status_locked(&inner)
    }

    /// E2E-06：A 端显式断开（`controller_disconnect`）到达时终结当前租约：清空
    /// 租约/会话密钥/远端任务与凭证，**保留**用户工作区授权，投影为停止态
    /// （A 需要重新让对方刷新连接码后再连）。只有已连接的代际可以接受该帧。
    /// E2E-06：当前生效的 A 端 liveness epoch（恢复完成后会轮换成 Relay 下发的值）。
    pub(crate) fn live_liveness_epoch(&self) -> Option<String> {
        let inner = self.inner.lock();
        if inner.phase != ControlledStatusKind::Connected {
            return None;
        }
        inner.liveness_epoch_a.clone()
    }

    pub(crate) fn accept_remote_disconnect_for(
        &self,
        operation: &ControlledOperation,
        expected: &LeaseContext,
    ) -> Result<ControlledStatus> {
        let status = {
            let mut inner = self.inner.lock();
            ensure_operation(&inner, operation)?;
            if inner.lease_context.as_ref() != Some(expected)
                || !matches!(
                    inner.phase,
                    ControlledStatusKind::Connected | ControlledStatusKind::Reconnecting
                )
            {
                return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
            }
            let retired_generation = inner.generation.clone();
            clear_sensitive(&mut inner, u64::MAX, false);
            inner.device_name = None;
            inner.phase = ControlledStatusKind::Stopped;
            inner.error_code = None;
            terminal_status_with_retired_generation(&inner, retired_generation)
        };
        // Host hooks may re-enter or start a successor. Never call them under
        // the state lock or clear resources by reading the successor identity.
        self.notify_cleanup(operation.generation());
        Ok(status)
    }

    pub(crate) fn disconnect_now(&self) -> ControlledStatus {
        self.terminate_local(
            "desktop_remote_disconnected",
            ControlledStatusKind::Stopped,
            true,
        )
    }

    pub(crate) fn poll_watchdog(&self, now_b: u64) -> Result<ControlledStatus> {
        self.poll_watchdog_inner(None, now_b)
    }

    pub(crate) fn poll_watchdog_for(
        &self,
        operation: &ControlledOperation,
        now_b: u64,
    ) -> Result<ControlledStatus> {
        self.poll_watchdog_inner(Some(operation), now_b)
    }

    fn poll_watchdog_inner(
        &self,
        operation: Option<&ControlledOperation>,
        now_b: u64,
    ) -> Result<ControlledStatus> {
        let generation;
        let expired;
        let expired_preconnect;
        let status;
        {
            let mut inner = self.inner.lock();
            if let Some(operation) = operation {
                ensure_operation(&inner, operation)?;
            }
            let retired_generation = inner.generation.clone();
            generation = retired_generation.clone().unwrap_or_default();
            inner.last_observed_now_b = inner.last_observed_now_b.max(now_b);
            let watchdog_state = inner
                .watchdog
                .as_ref()
                .map(|watchdog| watchdog.state(now_b));
            expired_preconnect = inner
                .preconnect_deadline
                .as_ref()
                .filter(|ticket| ticket.matches_phase(inner.phase) && now_b >= ticket.deadline_b)
                .map(ControlledPreconnectDeadline::error_code);
            if let Some(code) = expired_preconnect {
                clear_sensitive(&mut inner, now_b, true);
                inner.phase = ControlledStatusKind::Error;
                inner.error_code = Some(code.to_owned());
                expired = true;
            } else if watchdog_state == Some(LivenessWatchdogState::Expired) {
                clear_sensitive(&mut inner, now_b, true);
                inner.phase = ControlledStatusKind::Error;
                inner.error_code = Some("desktop_remote_watchdog_expired".to_owned());
                expired = true;
            } else {
                expired = false;
                if inner.phase == ControlledStatusKind::Connected
                    && watchdog_state == Some(LivenessWatchdogState::Suspect)
                {
                    // 仅 A 的有效存活证明暂时缺失，认证 transport 并没有断开。
                    // 保留 resume gate 的 Active 状态，让迟到的直接证明/attestation
                    // 可以恢复；真实 socket 断线会由 mark_transport_disconnected 将
                    // 原因升级为 Transport 并启动完整 resume。
                    inner.phase = ControlledStatusKind::Reconnecting;
                    inner.reconnect_reason = Some(ReconnectReason::LivenessSuspect);
                }
            }
            status = if expired {
                terminal_status_with_retired_generation(&inner, retired_generation)
            } else {
                status_locked(&inner)
            };
        }
        if expired {
            self.notify_cleanup(&generation);
        }
        if let Some(code) = expired_preconnect {
            super::relay_runtime::log_controlled_event("preconnect_expired", code);
        }
        Ok(status)
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

    /// Resolve an opaque capability back to the B-local directory while the
    /// controlled lease is live. The path never leaves this process.
    pub(crate) fn resolve_code_workspace(&self, workspace_id: &str) -> Result<PathBuf> {
        let inner = self.inner.lock();
        if inner.phase != ControlledStatusKind::Connected {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        }
        resolve_workspace_locked(&inner, workspace_id)
    }

    /// An already admitted task may finish during transient transport recovery.
    /// Its caller first checks the durable task and generation-scoped execution
    /// grant; this path never authorizes a new task or a replacement lease.
    pub(crate) fn resolve_admitted_code_workspace(
        &self,
        expected: &LeaseContext,
        workspace_id: &str,
    ) -> Result<PathBuf> {
        let inner = self.inner.lock();
        if inner.lease_context.as_ref() != Some(expected)
            || !matches!(
                inner.phase,
                ControlledStatusKind::Connected
                    | ControlledStatusKind::Reconnecting
                    | ControlledStatusKind::ResumeProvisional
                    | ControlledStatusKind::ResumeActivatedPendingLiveness
            )
        {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        }
        resolve_workspace_locked(&inner, workspace_id)
    }

    #[cfg(test)]
    pub(crate) fn resume_phase_for_test(&self) -> Option<ResumePhase> {
        self.inner
            .lock()
            .resume_gate
            .as_ref()
            .map(|gate| gate.phase())
    }

    #[cfg(test)]
    pub(crate) fn watchdog_deadline_for_test(&self) -> Option<u64> {
        self.inner
            .lock()
            .watchdog
            .as_ref()
            .and_then(|watchdog| watchdog.deadline_b())
    }

    #[cfg(test)]
    pub(crate) fn retained_liveness_epoch_for_test(&self) -> Option<String> {
        self.inner.lock().liveness_epoch_a.clone()
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

    /// 本次代际是否仍处于注册/握手的待决阶段。在途 worker 的失败收尾据此判断
    /// 能否投影 error：用户已显式停止/断开、或本代已被刷新取代时不得覆盖。
    pub(crate) fn is_pending_handshake(&self, operation: ControlledOperation) -> bool {
        let inner = self.inner.lock();
        ensure_operation(&inner, &operation).is_ok()
            && matches!(
                inner.phase,
                ControlledStatusKind::Registering
                    | ControlledStatusKind::WaitingForPairing
                    | ControlledStatusKind::PairingReserved
                    | ControlledStatusKind::HandshakePending
            )
    }

    /// 活租约上下文 + 协商能力：B 端解密/校验业务帧与构造回包的准入依据。
    pub(crate) fn live_runtime_context(&self) -> Result<(LeaseContext, Vec<String>)> {
        let inner = self.inner.lock();
        if inner.phase != ControlledStatusKind::Connected {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        }
        let context = inner
            .lease_context
            .clone()
            .ok_or_else(|| ControlledStateError::new("desktop_remote_not_connected"))?;
        Ok((context, inner.negotiated_capabilities.clone()))
    }

    /// B business actions are admitted only while this exact authenticated
    /// generation remains connected. Stop/refresh cannot race the mutation.
    pub(crate) fn with_current_business_lease<T>(
        &self,
        expected: &LeaseContext,
        action: impl FnOnce() -> T,
    ) -> Result<T> {
        self.with_current_business_inner(expected, |_| action())
    }

    fn with_current_business_inner<T>(
        &self,
        expected: &LeaseContext,
        action: impl FnOnce(&Inner) -> T,
    ) -> Result<T> {
        let inner = self.inner.lock();
        if inner.phase != ControlledStatusKind::Connected
            || inner.lease_context.as_ref() != Some(expected)
        {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        }
        let Some(transport) = inner.transport.as_ref() else {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        };
        let mut action = Some(action);
        let mut result = None;
        if !transport.with_business_authority(expected, &mut || {
            result = action.take().map(|action| action(&inner));
        }) {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        }
        result.ok_or_else(|| ControlledStateError::new("desktop_remote_not_connected"))
    }

    pub(crate) fn journal(&self) -> Arc<RemoteJournal> {
        Arc::clone(&self.inner.lock().journal_authority.journal)
    }

    /// Capture only an owner already issued to this exact authenticated lease.
    /// The journal revalidates it at the final operation after this lock exits.
    pub(crate) fn business_journal_owner(&self, expected: &LeaseContext) -> Result<JournalOwner> {
        self.with_current_business_inner(expected, |inner| inner.journal_owner.clone())?
            .ok_or_else(|| ControlledStateError::new("desktop_remote_not_connected"))
    }

    /// Accepted local work keeps recording through same-generation reconnect.
    /// Lease and owner must be captured together, before any mapping lookup.
    pub(crate) fn local_journal_context(&self) -> Result<(LeaseContext, JournalOwner)> {
        let inner = self.inner.lock();
        let lease = local_cancel_lease(&inner)?;
        let owner = inner
            .journal_owner
            .clone()
            .ok_or_else(|| ControlledStateError::new("desktop_remote_not_connected"))?;
        Ok((lease.clone(), owner))
    }

    /// Response construction pins the B owner but is not business admission.
    /// Each send separately checks exact socket/lease authority. Holding the
    /// transport mutation gate here would deadlock when send re-enters it.
    pub(crate) fn with_current_response_lease<T>(
        &self,
        expected: &LeaseContext,
        action: impl FnOnce() -> T,
    ) -> Result<T> {
        let inner = self.inner.lock();
        if inner.phase != ControlledStatusKind::Connected
            || inner.lease_context.as_ref() != Some(expected)
        {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        }
        Ok(action())
    }

    /// Hold the B lifecycle lock through a narrow ledger admission. The
    /// closure must not emit callbacks that re-enter this manager.
    pub(crate) fn with_local_cancel_lease<T>(
        &self,
        admit: impl FnOnce(&LeaseContext) -> T,
    ) -> Result<T> {
        let inner = self.inner.lock();
        let lease = local_cancel_lease(&inner)?;
        Ok(admit(lease))
    }

    /// 存活证据专用上下文：活性可疑时业务仍关闭，但允许当前认证通道继续完成
    /// challenge/attestation。真实 transport 断线绝不从这里取得可发送上下文。
    pub(crate) fn liveness_runtime_context(&self) -> Result<(LeaseContext, Vec<String>)> {
        let inner = self.inner.lock();
        let allowed = inner.phase == ControlledStatusKind::Connected
            || (inner.phase == ControlledStatusKind::Reconnecting
                && inner.reconnect_reason == Some(ReconnectReason::LivenessSuspect));
        if !allowed {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        }
        let context = inner
            .lease_context
            .clone()
            .ok_or_else(|| ControlledStateError::new("desktop_remote_not_connected"))?;
        Ok((context, inner.negotiated_capabilities.clone()))
    }

    /// Keep the original worker and complete lease bound through control enqueue.
    /// Only liveness-suspect is allowed; a transport-disconnected/resuming worker
    /// cannot manufacture fresh role evidence. The action must not re-enter us.
    pub(crate) fn with_owned_liveness_lease<T>(
        &self,
        operation: &ControlledOperation,
        expected: &LeaseContext,
        action: impl FnOnce() -> T,
    ) -> Result<T> {
        let inner = self.inner.lock();
        ensure_operation(&inner, operation)?;
        let allowed = inner.phase == ControlledStatusKind::Connected
            || (inner.phase == ControlledStatusKind::Reconnecting
                && inner.reconnect_reason == Some(ReconnectReason::LivenessSuspect));
        if !allowed
            || inner.lease_context.as_ref() != Some(expected)
            || !inner
                .negotiated_capabilities
                .iter()
                .any(|cap| cap == "role_liveness_v1")
        {
            return Err(ControlledStateError::new("desktop_remote_not_connected"));
        }
        Ok(action())
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
        let status;
        {
            let mut inner = self.inner.lock();
            let retired_generation = inner.generation.clone();
            generation = retired_generation.clone().unwrap_or_default();
            clear_sensitive(&mut inner, u64::MAX, clear_workspaces);
            inner.device_name = None;
            inner.phase = phase;
            inner.error_code =
                (phase == ControlledStatusKind::Error).then(|| error_code.to_owned());
            status = terminal_status_with_retired_generation(&inner, retired_generation);
        }
        self.notify_cleanup(&generation);
        status
    }

    fn notify_cleanup(&self, generation: &str) {
        let sinks = self.cleanup_sinks.lock().clone();
        for sink in sinks {
            sink.cleanup(generation, ControlledCleanupScope::ALL);
        }
    }
}

fn resolve_workspace_locked(inner: &Inner, workspace_id: &str) -> Result<PathBuf> {
    let workspace = inner
        .workspaces
        .iter()
        .find(|workspace| workspace.id == workspace_id && workspace.code_allowed)
        .ok_or_else(|| ControlledStateError::new("workspace_not_authorized"))?;
    if !workspace.path.is_dir()
        || super::super::platform::workspace_identity(&workspace.path)
            .map(|identity| identity != workspace.identity)
            .unwrap_or(true)
    {
        return Err(ControlledStateError::new("workspace_authorization_stale"));
    }
    Ok(workspace.path.clone())
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
        registration: None,
    }
}

fn current_operation(inner: &Inner) -> Option<ControlledOperation> {
    inner
        .generation
        .clone()
        .map(|generation| ControlledOperation {
            intent: inner.intent,
            generation,
            registration: None,
        })
}

fn ensure_operation(inner: &Inner, operation: &ControlledOperation) -> Result<()> {
    if inner.intent != operation.intent
        || inner.generation.as_deref() != Some(operation.generation.as_str())
    {
        return Err(ControlledStateError::new("desktop_remote_stale_result"));
    }
    if let Some(registration) = operation.registration.as_ref() {
        if inner.lease_context.as_ref() != Some(&registration.lease)
            || !inner
                .transport
                .as_ref()
                .is_some_and(|transport| Arc::ptr_eq(transport, &registration.transport))
        {
            return Err(ControlledStateError::new("desktop_remote_stale_transport"));
        }
    }
    Ok(())
}

fn ensure_transport_owner(inner: &Inner, owner: &ControlledTransportOwner) -> Result<()> {
    ensure_operation(inner, &owner.operation)?;
    if inner.lease_context.as_ref() != Some(&owner.lease)
        || !inner
            .transport
            .as_ref()
            .is_some_and(|transport| Arc::ptr_eq(transport, &owner.transport))
    {
        return Err(ControlledStateError::new("desktop_remote_stale_transport"));
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
            identity: request.identity,
            path: request.path,
            code_allowed: request.code_allowed,
        });
    }
    Ok(output)
}

fn ensure_connected_evidence(inner: &Inner) -> Result<()> {
    if (inner.phase == ControlledStatusKind::Connected
        || (inner.phase == ControlledStatusKind::Reconnecting
            && inner.reconnect_reason == Some(ReconnectReason::LivenessSuspect)))
        && inner
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
        && inner.reconnect_reason == Some(ReconnectReason::LivenessSuspect)
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
        inner.reconnect_reason = None;
    }
}

fn local_cancel_lease(inner: &Inner) -> Result<&LeaseContext> {
    if !matches!(
        inner.phase,
        ControlledStatusKind::Connected
            | ControlledStatusKind::Reconnecting
            | ControlledStatusKind::ResumeProvisional
            | ControlledStatusKind::ResumeActivatedPendingLiveness
    ) {
        return Err(ControlledStateError::new("desktop_remote_not_connected"));
    }
    inner
        .lease_context
        .as_ref()
        .ok_or_else(|| ControlledStateError::new("desktop_remote_not_connected"))
}

fn revoke_journal_owner(inner: &mut Inner) {
    if let Some(owner) = inner.journal_owner.take() {
        inner
            .journal_authority
            .journal
            .revoke_owner(&inner.journal_authority, &owner);
    }
}

fn require_preconnect_live(inner: &Inner, now_b: u64) -> Result<()> {
    let ticket = inner
        .preconnect_deadline
        .as_ref()
        .filter(|ticket| ticket.matches_phase(inner.phase))
        .ok_or_else(|| ControlledStateError::new("desktop_remote_phase_invalid"))?;
    ensure_operation(inner, &ticket.operation)?;
    if now_b >= ticket.deadline_b {
        return Err(ControlledStateError::new(ticket.error_code()));
    }
    Ok(())
}

fn clear_sensitive(inner: &mut Inner, now_b: u64, clear_workspaces: bool) {
    revoke_journal_owner(inner);
    if let Some(generation) = inner.generation.as_ref() {
        inner.retired_generation = Some(generation.clone());
    }
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
    inner.reconnect_reason = None;
    inner.pairing = None;
    inner.preconnect_deadline = None;
    inner.lease_context = None;
    inner.negotiated_capabilities.clear();
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
        status_intent: inner.intent.to_string(),
        generation: inner.generation.clone().or_else(|| {
            matches!(
                inner.phase,
                ControlledStatusKind::Stopping
                    | ControlledStatusKind::Stopped
                    | ControlledStatusKind::Error
            )
            .then(|| inner.retired_generation.clone())
            .flatten()
        }),
        registration_request_id: inner.registration_request_id.clone(),
        pairing_code: inner
            .pairing
            .as_ref()
            .filter(|_| inner.phase == ControlledStatusKind::WaitingForPairing)
            .map(|pairing| pairing.pairing_code.to_string()),
        pairing_remaining_ms: inner
            .pairing
            .as_ref()
            .filter(|_| inner.phase == ControlledStatusKind::WaitingForPairing)
            .map(|pairing| {
                pairing
                    .expires_at_ms
                    .saturating_sub(inner.last_observed_now_b)
            }),
        device_name: inner.device_name.clone(),
        controller_device_name: inner.controller_device_name.clone(),
        connected_duration_ms: matches!(
            inner.phase,
            ControlledStatusKind::Connected
                | ControlledStatusKind::Reconnecting
                | ControlledStatusKind::ResumeProvisional
                | ControlledStatusKind::ResumeActivatedPendingLiveness
        )
        .then(|| {
            inner
                .connected_at_b
                .map(|connected_at_b| inner.last_observed_now_b.saturating_sub(connected_at_b))
        })
        .flatten(),
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
        error_code: inner.error_code.clone(),
    }
}

/// Terminal status may retain the retired generation for late-event filtering;
/// live admission reads only the cleared active generation and lease.
fn terminal_status_with_retired_generation(
    inner: &Inner,
    retired_generation: Option<String>,
) -> ControlledStatus {
    let mut status = status_locked(inner);
    if matches!(
        status.status,
        ControlledStatusKind::Stopping
            | ControlledStatusKind::Stopped
            | ControlledStatusKind::Error
    ) {
        status.generation = retired_generation.or_else(|| inner.retired_generation.clone());
    }
    status
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
#[path = "controlled/journal_owner_tests.rs"]
mod journal_owner_tests;

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicUsize, Ordering};

    use super::*;

    #[test]
    fn same_operation_replaced_transport_rejects_the_old_owner() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
        let material = lease(operation.generation());
        let context = material.context.clone();
        let capabilities = material.negotiated_capabilities.clone();
        let sender = super::super::test_support::resume_reader_transport(
            super::super::relay_transport::ControllerRole::ControlledB,
            context.clone(),
            capabilities.clone(),
        );
        let now_b = material.connected_at_b;
        let (_, old) = manager
            .complete_connected_with_install(
                operation.clone(),
                material,
                || now_b,
                || Ok(Arc::new(sender)),
            )
            .unwrap();
        let calls = AtomicUsize::new(0);
        manager
            .with_transport_owner(&old, || calls.fetch_add(1, Ordering::SeqCst))
            .unwrap();
        let successor = super::super::test_support::resume_reader_transport(
            super::super::relay_transport::ControllerRole::ControlledB,
            context.clone(),
            capabilities,
        );
        manager.replace_transport(successor);
        // No new intent, generation or lease: only the transport Arc differs.
        assert_eq!(manager.current_operation(), Some(operation.clone()));
        assert_eq!(manager.live_lease().unwrap(), context);
        assert_eq!(
            manager
                .with_transport_owner(&old, || calls.fetch_add(1, Ordering::SeqCst))
                .unwrap_err()
                .code,
            "desktop_remote_stale_transport"
        );
        let current = manager.transport_owner_for_test(&operation).unwrap();
        manager
            .with_transport_owner(&current, || calls.fetch_add(1, Ordering::SeqCst))
            .unwrap();
        assert_eq!(calls.load(Ordering::SeqCst), 2);
        assert_eq!(manager.status().kind(), ControlledStatusKind::Connected);
    }

    #[test]
    fn replaced_transport_cannot_disconnect_or_resume_the_successor() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
        let material = lease(operation.generation());
        let context = material.context.clone();
        let capabilities = material.negotiated_capabilities.clone();
        let sender = super::super::test_support::resume_reader_transport(
            super::super::relay_transport::ControllerRole::ControlledB,
            context.clone(),
            capabilities.clone(),
        );
        let now_b = material.connected_at_b;
        let (_, old) = manager
            .complete_connected_with_install(
                operation.clone(),
                material,
                || now_b,
                || Ok(Arc::new(sender)),
            )
            .unwrap();
        let successor = super::super::test_support::resume_reader_transport(
            super::super::relay_transport::ControllerRole::ControlledB,
            context.clone(),
            capabilities,
        );
        manager.replace_transport(successor.clone());
        let current = manager.transport_owner_for_test(&operation).unwrap();
        assert_eq!(manager.current_operation(), Some(operation.clone()));
        assert_eq!(manager.live_lease().unwrap(), context);
        let effects = AtomicUsize::new(0);
        assert_eq!(
            manager
                .mark_owned_transport_disconnected(&old, 5_000, || {
                    effects.fetch_add(1, Ordering::SeqCst);
                })
                .unwrap_err()
                .code,
            "desktop_remote_stale_transport"
        );
        assert_eq!(effects.load(Ordering::SeqCst), 0);
        assert_eq!(manager.status().kind(), ControlledStatusKind::Connected);
        assert_eq!(manager.live_lease().unwrap(), context);
        super::super::relay_transport::disconnect_test_transport(&successor);
        manager
            .mark_owned_transport_disconnected(&current, 5_000, || {
                effects.fetch_add(1, Ordering::SeqCst);
            })
            .unwrap();
        assert_eq!(effects.load(Ordering::SeqCst), 1);
        assert_eq!(manager.status().kind(), ControlledStatusKind::Reconnecting);
        assert_eq!(
            manager.begin_owned_resume(&old, 6_000).unwrap_err().code,
            "desktop_remote_stale_transport"
        );
        assert_eq!(manager.status().kind(), ControlledStatusKind::Reconnecting);
        manager.begin_owned_resume(&current, 6_000).unwrap();
        let activation = "activation-0001";
        for (step, now) in [
            (
                ControlledResumeStep::Provisional {
                    activation_id: activation,
                },
                7_000,
            ),
            (
                ControlledResumeStep::Confirmed {
                    activation_id: activation,
                },
                8_000,
            ),
            (
                ControlledResumeStep::ActivationAcknowledged {
                    activation_id: activation,
                },
                9_000,
            ),
            (
                ControlledResumeStep::PostActivationLiveness {
                    activation_id: activation,
                    liveness_epoch_b: "live-b-2",
                },
                10_000,
            ),
            (
                ControlledResumeStep::ReadyAcknowledged {
                    activation_id: activation,
                },
                11_000,
            ),
        ] {
            manager.advance_owned_resume(&current, step, now).unwrap();
        }
        super::super::relay_transport::dispatch_test_transport_frame(
            &successor,
            &super::super::test_support::resume_final_frame(
                &context,
                activation,
                "request-resume-final",
            ),
        );
        let finish = |owner: &ControlledTransportOwner| {
            manager.complete_owned_resume(
                owner,
                activation,
                "request-resume-final",
                12_000,
                "live:a-2",
                [3; 32],
                [4; 32],
                1_700_000_030_000,
                || {
                    effects.fetch_add(1, Ordering::SeqCst);
                },
            )
        };
        assert_eq!(
            finish(&old).unwrap_err().code,
            "desktop_remote_stale_transport"
        );
        assert_eq!(effects.load(Ordering::SeqCst), 1);
        assert_eq!(
            manager.status().kind(),
            ControlledStatusKind::ResumeActivatedPendingLiveness
        );
        finish(&current).unwrap();
        assert_eq!(effects.load(Ordering::SeqCst), 2);
        assert_eq!(manager.status().kind(), ControlledStatusKind::Connected);
    }

    #[test]
    fn stale_connected_install_never_runs_its_shared_side_effect() {
        let manager = ControlledDeviceManager::default();
        let old = manager.start(None, Vec::new()).unwrap();
        register(&manager, &old);
        enter_handshake(&manager, &old);
        manager.stop();
        let current = manager.start(None, Vec::new()).unwrap();
        register(&manager, &current);
        enter_handshake(&manager, &current);
        let calls = AtomicUsize::new(0);
        let stale = lease(old.generation());
        let now_b = stale.connected_at_b;
        let result = manager.complete_connected_with_install(
            old.clone(),
            stale,
            || now_b,
            || {
                calls.fetch_add(1, Ordering::SeqCst);
                Err(ControlledStateError::new("install_must_not_run"))
            },
        );
        assert_eq!(result.err().unwrap().code, "desktop_remote_stale_result");
        assert_eq!(calls.load(Ordering::SeqCst), 0);
        assert_eq!(manager.current_operation(), Some(current.clone()));
        assert_eq!(
            manager.status().kind(),
            ControlledStatusKind::HandshakePending
        );
        let material = lease(current.generation());
        let sender = super::super::test_support::resume_reader_transport(
            super::super::relay_transport::ControllerRole::ControlledB,
            material.context.clone(),
            material.negotiated_capabilities.clone(),
        );
        let now_b = material.connected_at_b;
        manager
            .complete_connected_with_install(
                current,
                material,
                || now_b,
                || {
                    calls.fetch_add(1, Ordering::SeqCst);
                    Ok(Arc::new(sender))
                },
            )
            .unwrap();
        assert_eq!(calls.load(Ordering::SeqCst), 1);
        assert_eq!(manager.status().kind(), ControlledStatusKind::Connected);
    }

    #[test]
    fn old_evidence_owner_cannot_register_or_cancel_successor_challenge() {
        let manager = ControlledDeviceManager::default();
        let connect = || {
            let operation = manager.start(None, Vec::new()).unwrap();
            register(&manager, &operation);
            enter_handshake(&manager, &operation);
            let mut material = lease(operation.generation());
            material.connected_at_b = 0;
            let sender = super::super::test_support::resume_reader_transport(
                super::super::relay_transport::ControllerRole::ControlledB,
                material.context.clone(),
                material.negotiated_capabilities.clone(),
            );
            let now_b = material.connected_at_b;
            let (_, owner) = manager
                .complete_connected_with_install(
                    operation.clone(),
                    material,
                    || now_b,
                    || Ok(Arc::new(sender)),
                )
                .unwrap();
            (operation, owner)
        };
        let (_, old) = connect();
        manager.stop();
        let (operation, current) = connect();
        let deadline = manager.watchdog_deadline_for_test();
        assert_eq!(deadline, Some(30_000));
        assert_eq!(
            manager
                .begin_owned_liveness_challenge(&old, 14_500)
                .unwrap_err()
                .code,
            "desktop_remote_stale_result",
        );
        assert_eq!(
            manager
                .poll_watchdog_for(&operation, 15_000)
                .unwrap()
                .kind(),
            ControlledStatusKind::Reconnecting
        );
        let (lease, challenge_id, _) = manager
            .begin_owned_liveness_challenge(&current, 16_000)
            .unwrap();
        assert_eq!(&lease, current.lease());
        assert_eq!(
            manager
                .inner
                .lock()
                .watchdog
                .as_ref()
                .unwrap()
                .state(16_000),
            LivenessWatchdogState::Healthy
        );
        assert_eq!(
            manager
                .cancel_owned_liveness_challenge(&old, &challenge_id)
                .unwrap_err()
                .code,
            "desktop_remote_stale_result",
        );
        assert_eq!(
            manager
                .inner
                .lock()
                .watchdog
                .as_ref()
                .unwrap()
                .state(16_000),
            LivenessWatchdogState::Healthy,
            "old cancellation removed the successor's genuine pending challenge"
        );
        manager
            .cancel_owned_liveness_challenge(&current, &challenge_id)
            .unwrap();
        assert_eq!(
            manager
                .inner
                .lock()
                .watchdog
                .as_ref()
                .unwrap()
                .state(16_000),
            LivenessWatchdogState::Suspect
        );
        assert_eq!(manager.watchdog_deadline_for_test(), deadline);
    }

    #[test]
    fn serialized_status_matches_the_shared_bridge_fixture() {
        let fixtures: serde_json::Value = serde_json::from_str(include_str!(
            "../../../../../tests/fixtures/desktop_remote_status.json"
        ))
        .unwrap();
        let manager = ControlledDeviceManager::default();
        let operation = manager
            .start(
                Some("Studio PC"),
                vec![WorkspaceAuthorizationRequest {
                    path: PathBuf::from("workspace-fixture"),
                    display_name: "Project".into(),
                    code_allowed: true,
                    identity: super::super::super::platform::test_workspace_identity(6),
                }],
            )
            .unwrap();
        let mut waiting = manager
            .complete_registration(
                operation.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: 600_000,
                },
                1_000,
            )
            .unwrap();
        // Normalize only random correlation IDs; every other field comes from
        // the production transition rather than a hand-constructed DTO.
        waiting.generation = Some("generation-0001".into());
        waiting.registration_request_id = Some("registration-0001".into());
        assert_eq!(
            serde_json::to_value(&waiting).unwrap(),
            fixtures["controlledWaiting"]
        );
        let mut failed = manager
            .fail_operation(operation.clone(), "endpoint_offline".into())
            .unwrap();
        assert_eq!(
            failed.generation.as_deref(),
            Some(operation.generation.as_str())
        );
        assert!(manager.inner.lock().generation.is_none());
        assert_eq!(
            manager
                .complete_registration(
                    operation,
                    ControlledRegistration {
                        pairing_code: "2345678".into(),
                        sealed_code_b64: "sealed".into(),
                        expires_at_ms: 600_000,
                    },
                    1_000
                )
                .unwrap_err()
                .code,
            "desktop_remote_stale_result"
        );
        failed.generation = Some("generation-0001".into());
        assert_eq!(
            serde_json::to_value(failed).unwrap(),
            fixtures["controlledError"]
        );
    }

    #[derive(Default)]
    struct CleanupCounter(AtomicUsize);

    impl ControlledCleanupSink for CleanupCounter {
        fn cleanup(&self, _generation: &str, scope: ControlledCleanupScope) {
            assert!(scope.remote_tasks && scope.session_keys && scope.temporary_models);
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    pub(super) fn lease(generation: &str) -> ControlledLease {
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
            negotiated_capabilities: vec![
                "encrypted_frames_v1".into(),
                "pairing_v1".into(),
                "task_v1".into(),
            ],
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

    pub(super) fn register(manager: &ControlledDeviceManager, operation: &ControlledOperation) {
        manager
            .complete_registration(
                operation.clone(),
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    // A successor registration issues a fresh pairing code; keep
                    // the expiry above every fixture clock advance (tests move
                    // last_observed_now_b up to 33_000) so the merged #2146
                    // preconnect deadline check sees a live ticket.
                    expires_at_ms: 90_000,
                },
                1_000,
            )
            .unwrap();
    }

    pub(super) fn enter_handshake(
        manager: &ControlledDeviceManager,
        operation: &ControlledOperation,
    ) {
        manager.mark_pairing_reserved(operation.clone()).unwrap();
        manager.mark_handshake_pending(operation.clone()).unwrap();
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
    fn missing_registration_id_fails_without_installing_pairing_or_advancing_state() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(Some("B"), Vec::new()).unwrap();
        manager.inner.lock().registration_request_id = None;
        let before = manager.status();
        let observed_before = manager.inner.lock().last_observed_now_b;
        let error = manager
            .complete_registration(
                operation,
                ControlledRegistration {
                    pairing_code: "2345678".into(),
                    sealed_code_b64: "sealed".into(),
                    expires_at_ms: 30_000,
                },
                1_000,
            )
            .unwrap_err();
        assert_eq!(error.code, "registration_request_unavailable");
        assert_eq!(manager.status(), before);
        let inner = manager.inner.lock();
        assert!(inner.pairing.is_none());
        assert!(inner.preconnect_deadline.is_none());
        assert_eq!(inner.last_observed_now_b, observed_before);
    }

    #[test]
    fn connected_without_transport_cannot_execute_business_mutation() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        register(&manager, &operation);
        manager.mark_claimed(operation.clone()).unwrap();
        let context = lease(operation.generation()).context;
        manager
            .complete_connected(operation.clone(), lease(operation.generation()))
            .unwrap();
        let mut calls = 0;
        let result = manager.with_current_business_lease(&context, || calls += 1);
        assert_eq!(result.unwrap_err().code, "desktop_remote_not_connected");
        assert_eq!(calls, 0);
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
                    identity: super::super::super::platform::test_workspace_identity(1),
                }],
            )
            .unwrap();
        assert_eq!(
            manager.registration_request_id(operation.clone()).unwrap(),
            manager.registration_request_id(operation.clone()).unwrap()
        );
        assert_eq!(manager.status().status, ControlledStatusKind::Registering);
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
        assert_eq!(status.status, ControlledStatusKind::WaitingForPairing);
        assert_eq!(status.pairing_code.as_deref(), Some("2345678"));
        assert_eq!(status.pairing_remaining_ms, Some(29_000));
        assert_eq!(status.workspace_count, 1);
        assert!(status.code_mode_allowed);
        assert_eq!(status.device_name.as_deref(), Some("Studio PC"));

        let claimed = manager.mark_pairing_reserved(operation.clone()).unwrap();
        assert_eq!(claimed.status, ControlledStatusKind::PairingReserved);
        let handshake = manager.mark_handshake_pending(operation.clone()).unwrap();
        assert_eq!(handshake.status, ControlledStatusKind::HandshakePending);
        assert_eq!(claimed.pairing_code, None);
        let connected = manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        assert_eq!(connected.status, ControlledStatusKind::Connected);
        assert_eq!(
            connected.controller_device_name.as_deref(),
            Some("Controller PC")
        );
        assert_eq!(connected.connected_duration_ms, Some(0));
        assert_eq!(
            serde_json::to_value(&connected).unwrap()["controllerDeviceName"],
            "Controller PC"
        );
        assert_eq!(connected.watchdog_deadline_b, Some(33_000));
        let projected = manager.workspaces();
        assert_eq!(projected.len(), 1);
        assert_eq!(projected[0].display_name, "Projects");
        assert!(!format!("{projected:?}").contains("projects"));
    }

    #[test]
    fn empty_workspace_start_can_pair_but_never_advertises_or_resolves_code_scope() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        assert_eq!(manager.status().status, ControlledStatusKind::Registering);
        assert_eq!(manager.status().workspace_count, 0);
        assert!(!manager.status().code_mode_allowed);
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
        let connected = manager
            .complete_connected(operation.clone(), lease(operation.generation()))
            .unwrap();
        assert_eq!(connected.status, ControlledStatusKind::Connected);
        assert_eq!(connected.workspace_count, 0);
        assert!(!connected.code_mode_allowed);
        assert!(manager.workspaces().is_empty());
        assert_eq!(
            manager
                .resolve_code_workspace("ungranted")
                .unwrap_err()
                .code,
            "workspace_not_authorized"
        );
    }

    #[test]
    fn opaque_workspace_resolves_only_while_the_b_authorized_directory_identity_is_live() {
        let workspace = tempfile::tempdir().unwrap();
        let authorized_path = workspace.path().to_path_buf();
        let manager = ControlledDeviceManager::default();
        let operation = manager
            .start(
                Some("Studio PC"),
                vec![
                    WorkspaceAuthorizationRequest::new(
                        authorized_path.clone(),
                        "Projects".into(),
                        true,
                    )
                    .unwrap(),
                ],
            )
            .unwrap();
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
        manager
            .complete_connected(operation.clone(), lease(operation.generation()))
            .unwrap();
        let workspace_id = manager.workspaces().remove(0).workspace_id;
        assert_eq!(
            manager.resolve_code_workspace(&workspace_id).unwrap(),
            authorized_path
        );
        std::fs::remove_dir_all(&authorized_path).unwrap();
        assert_eq!(
            manager
                .resolve_code_workspace(&workspace_id)
                .unwrap_err()
                .code,
            "workspace_authorization_stale"
        );
    }

    #[test]
    fn admitted_workspace_survives_transient_loss_but_not_another_lease_or_stop() {
        let directory = tempfile::tempdir().unwrap();
        let manager = ControlledDeviceManager::default();
        let operation = manager
            .start(
                None,
                vec![
                    WorkspaceAuthorizationRequest::new(
                        directory.path().to_path_buf(),
                        "Project".into(),
                        true,
                    )
                    .unwrap(),
                ],
            )
            .unwrap();
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
        manager
            .complete_connected(operation.clone(), lease(operation.generation()))
            .unwrap();
        let context = manager.live_lease().unwrap();
        let workspace_id = manager.workspaces().remove(0).workspace_id;
        for field in 0..5 {
            let mut wrong = context.clone();
            match field {
                0 => wrong.connection_id.push_str("-other"),
                1 => wrong.controller_lease_id.push_str("-other"),
                2 => wrong.lease_epoch = "2".into(),
                3 => wrong.relay_epoch.push_str("-other"),
                _ => wrong.remote_control_generation.push_str("-other"),
            }
            assert!(
                manager
                    .resolve_admitted_code_workspace(&wrong, &workspace_id)
                    .is_err()
            );
        }
        manager.mark_transport_disconnected(4_000);
        assert!(manager.resolve_code_workspace(&workspace_id).is_err());
        assert_eq!(
            manager
                .resolve_admitted_code_workspace(&context, &workspace_id)
                .unwrap(),
            directory.path()
        );
        manager.stop();
        assert!(
            manager
                .resolve_admitted_code_workspace(&context, &workspace_id)
                .is_err()
        );
    }

    #[test]
    fn connected_duration_is_a_monotonic_snapshot_without_exposing_clock_origin() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(Some("Device"), Vec::new()).unwrap();
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
        manager
            .complete_connected(operation.clone(), lease(operation.generation()))
            .unwrap();
        manager.inner.lock().last_observed_now_b = 4_500;
        let projected = serde_json::to_value(manager.status()).unwrap();
        assert_eq!(projected["connectedDurationMs"], 1_500);
        assert!(projected.get("connectedAtB").is_none());
        assert!(projected.get("watchdogDeadlineB").is_none());
        let stopped = serde_json::to_value(manager.stop()).unwrap();
        assert!(stopped.get("connectedDurationMs").is_none());
    }

    #[test]
    fn stale_results_cannot_replace_a_new_generation() {
        let manager = ControlledDeviceManager::default();
        let old = manager.start(Some("B"), Vec::new()).unwrap();
        let next = manager.begin_refresh().unwrap();
        assert_eq!(old.generation, next.generation);
        assert_eq!(next.intent, old.intent + 1);
        assert_eq!(
            manager.status().status,
            ControlledStatusKind::RetiringGeneration
        );
        let new_operation = manager.complete_refresh_revoked(next).unwrap();
        assert_ne!(new_operation.generation, old.generation);
        assert_eq!(manager.status().status, ControlledStatusKind::Registering);
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
                    identity: super::super::super::platform::test_workspace_identity(2),
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
        assert_eq!(
            manager.status().status,
            ControlledStatusKind::RetiringGeneration
        );
        manager.complete_refresh_revoked(refresh).unwrap();
        assert_eq!(manager.status().status, ControlledStatusKind::Registering);
        assert_eq!(manager.status().workspace_count, 1);
        assert!(!manager.status().code_mode_allowed);
    }

    /// E2E-06：B 端恢复相位必须按冻结顺序推进（provisional → confirmed →
    /// activation_ack → post_activation_liveness → ready_ack → resumed），顺序错乱
    /// 或材料复用失败关闭并清理本代。
    #[test]
    fn resume_sequence_advances_the_gate_and_rotates_material() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
        manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        let journal = manager.journal();
        let (_, journal_owner) = manager.local_journal_context().unwrap();
        journal
            .append_for_owner(
                &journal_owner,
                "before-resume",
                super::super::journal::JournalKind::UserMessage,
                "b-session",
                None,
                serde_json::json!({"text":"keep through resume"}),
            )
            .unwrap();
        let before_journal = journal
            .snapshot_for_owner(&journal_owner, "b-session")
            .unwrap();
        let current = lease(&operation.generation);
        let sender = super::super::test_support::resume_reader_transport(
            super::super::relay_transport::ControllerRole::ControlledB,
            current.context.clone(),
            current.negotiated_capabilities,
        );
        manager
            .replace_authenticated_transport(operation.clone(), sender.clone())
            .unwrap();
        super::super::relay_transport::disconnect_test_transport(&sender);
        assert_eq!(
            manager.mark_transport_disconnected(5_000).status,
            ControlledStatusKind::Reconnecting
        );
        let material = manager.begin_resume(operation.clone(), 6_000).unwrap();
        assert_eq!(material.relay_deadline_identity_ms, 30_000);
        assert!(format!("{material:?}").contains("[REDACTED]"));
        assert_eq!(material.resume_token, [1; 32]);

        let activation = "activation-0001";
        for (step, now_b, expected) in [
            (
                ControlledResumeStep::Provisional {
                    activation_id: activation,
                },
                7_000,
                ControlledStatusKind::ResumeProvisional,
            ),
            (
                ControlledResumeStep::Confirmed {
                    activation_id: activation,
                },
                8_000,
                ControlledStatusKind::ResumeProvisional,
            ),
            (
                ControlledResumeStep::ActivationAcknowledged {
                    activation_id: activation,
                },
                9_000,
                ControlledStatusKind::ResumeProvisional,
            ),
            (
                ControlledResumeStep::PostActivationLiveness {
                    activation_id: activation,
                    liveness_epoch_b: "live-b-2",
                },
                10_000,
                ControlledStatusKind::ResumeActivatedPendingLiveness,
            ),
            (
                ControlledResumeStep::ReadyAcknowledged {
                    activation_id: activation,
                },
                11_000,
                ControlledStatusKind::ResumeActivatedPendingLiveness,
            ),
        ] {
            let status = manager
                .advance_resume(operation.clone(), step, now_b)
                .unwrap();
            assert_eq!(status.status, expected);
            assert_eq!(manager.status().status, expected);
            assert_eq!(
                manager
                    .with_local_cancel_lease(|lease| lease.remote_control_generation.clone())
                    .unwrap(),
                operation.generation
            );
        }
        super::super::relay_transport::dispatch_test_transport_frame(
            &sender,
            &super::super::test_support::resume_final_frame(
                &current.context,
                activation,
                "request-resume-final",
            ),
        );
        let resumed = manager
            .complete_resumed(
                operation.clone(),
                activation,
                "request-resume-final",
                12_000,
                "live:a-2",
                [3; 32],
                [4; 32],
                1_700_000_030_000,
            )
            .unwrap();
        assert_eq!(resumed.status, ControlledStatusKind::Connected);
        let (_, resumed_owner) = manager.local_journal_context().unwrap();
        assert_eq!(
            journal
                .snapshot_for_owner(&journal_owner, "b-session")
                .unwrap(),
            before_journal
        );
        assert_eq!(
            journal
                .snapshot_for_owner(&resumed_owner, "b-session")
                .unwrap(),
            before_journal
        );
        journal
            .append_for_owner(
                &resumed_owner,
                "after-resume",
                super::super::journal::JournalKind::UserMessage,
                "b-session",
                None,
                serde_json::json!({"text":"next accepted local event"}),
            )
            .unwrap();

        assert_eq!(manager.live_liveness_epoch(), Some("live:a-2".to_owned()));

        // 第二次断线后相位错乱（未 provisional 就 confirm）必须失败关闭。
        manager.mark_transport_disconnected(13_000);
        manager.begin_resume(operation.clone(), 14_000).unwrap();
        assert_eq!(
            manager
                .advance_resume(
                    operation.clone(),
                    ControlledResumeStep::Confirmed {
                        activation_id: "activation-0002",
                    },
                    15_000,
                )
                .unwrap_err()
                .code,
            "desktop_remote_resume_failed"
        );
        assert_eq!(manager.status().status, ControlledStatusKind::Error);
        assert_eq!(
            manager.status().generation.as_deref(),
            Some(operation.generation.as_str())
        );
        assert_eq!(
            manager.status().error_code.as_deref(),
            Some("desktop_remote_resume_invalid")
        );
        assert!(manager.live_lease().is_err());
        assert!(
            journal
                .snapshot_for_owner(&journal_owner, "b-session")
                .is_err()
        );
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
            manager.poll_watchdog(17_000).unwrap().status,
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
        assert!(manager.begin_liveness_challenge(20_000).is_err());
        manager.mark_transport_disconnected(25_000);
        assert_eq!(manager.status().watchdog_deadline_b, Some(46_000));
        assert_eq!(
            manager.poll_watchdog(46_000).unwrap().status,
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
    fn controlled_final_without_transport_fails_closed_even_after_valid_gate() {
        for case in ["valid", "expired", "activation", "token", "key", "no-ready"] {
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
            manager.begin_resume(operation.clone(), 6_000).unwrap();
            for (now, step) in [
                (
                    7_000,
                    ControlledResumeStep::Provisional {
                        activation_id: "activation-0001",
                    },
                ),
                (
                    8_000,
                    ControlledResumeStep::Confirmed {
                        activation_id: "activation-0001",
                    },
                ),
                (
                    9_000,
                    ControlledResumeStep::ActivationAcknowledged {
                        activation_id: "activation-0001",
                    },
                ),
                (
                    10_000,
                    ControlledResumeStep::PostActivationLiveness {
                        activation_id: "activation-0001",
                        liveness_epoch_b: "live-b-2",
                    },
                ),
                (
                    11_000,
                    ControlledResumeStep::ReadyAcknowledged {
                        activation_id: "activation-0001",
                    },
                ),
            ] {
                if case == "no-ready" && now == 11_000 {
                    continue;
                }
                manager
                    .advance_resume(operation.clone(), step, now)
                    .unwrap();
            }
            let result = manager.complete_resumed(
                operation.clone(),
                if case == "activation" {
                    "activation-wrong"
                } else {
                    "activation-0001"
                },
                "request-resume-0001",
                if case == "expired" { 33_000 } else { 12_000 },
                "live-a-2",
                if case == "token" { [1; 32] } else { [3; 32] },
                if case == "key" { [2; 32] } else { [4; 32] },
                60_000,
            );
            assert_eq!(
                result.unwrap_err().code,
                "desktop_remote_resume_failed",
                "{case}"
            );
            assert_eq!(manager.status().status, ControlledStatusKind::Error);
            assert!(manager.inner.lock().watchdog.is_none());
            assert!(manager.inner.lock().lease_context.is_none());
            assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
        }
    }

    #[test]
    fn invalid_resumed_cycle_metadata_cleans_generation_and_notifies_once() {
        for (deadline, epoch, now) in [
            (0, "live-a-2", 12_000),
            (60_000, "", 12_000),
            (60_000, "invalid epoch", 12_000),
            (60_000, "live~a-2", 12_000),
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
            manager.begin_resume(operation.clone(), 6_000).unwrap();
            let activation = "activation-0001";
            for (step, timestamp) in [
                (
                    ControlledResumeStep::Provisional {
                        activation_id: activation,
                    },
                    7_000,
                ),
                (
                    ControlledResumeStep::Confirmed {
                        activation_id: activation,
                    },
                    8_000,
                ),
                (
                    ControlledResumeStep::ActivationAcknowledged {
                        activation_id: activation,
                    },
                    9_000,
                ),
                (
                    ControlledResumeStep::PostActivationLiveness {
                        activation_id: activation,
                        liveness_epoch_b: "live-b-2",
                    },
                    10_000,
                ),
                (
                    ControlledResumeStep::ReadyAcknowledged {
                        activation_id: activation,
                    },
                    11_000,
                ),
            ] {
                manager
                    .advance_resume(operation.clone(), step, timestamp)
                    .unwrap();
            }
            assert_eq!(
                manager
                    .complete_resumed(
                        operation,
                        activation,
                        "request-resume-final",
                        now,
                        epoch,
                        [3; 32],
                        [4; 32],
                        deadline
                    )
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
    fn second_transport_loss_in_resume_phases_cleans_once_without_refreezing() {
        for activated in [false, true] {
            let cleanup = Arc::new(CleanupCounter::default());
            let manager = ControlledDeviceManager::default();
            manager.register_cleanup_sink(cleanup.clone());
            let operation = manager.start(None, Vec::new()).unwrap();
            register(&manager, &operation);
            enter_handshake(&manager, &operation);
            manager
                .complete_connected(operation.clone(), lease(&operation.generation))
                .unwrap();
            assert_eq!(
                manager.mark_transport_disconnected(5_000).status,
                ControlledStatusKind::Reconnecting
            );
            manager.begin_resume(operation.clone(), 6_000).unwrap();
            let activation = "activation-second-close";
            manager
                .advance_resume(
                    operation.clone(),
                    ControlledResumeStep::Provisional {
                        activation_id: activation,
                    },
                    7_000,
                )
                .unwrap();
            if activated {
                for (step, now) in [
                    (
                        ControlledResumeStep::Confirmed {
                            activation_id: activation,
                        },
                        8_000,
                    ),
                    (
                        ControlledResumeStep::ActivationAcknowledged {
                            activation_id: activation,
                        },
                        9_000,
                    ),
                    (
                        ControlledResumeStep::PostActivationLiveness {
                            activation_id: activation,
                            liveness_epoch_b: "live-b-2",
                        },
                        10_000,
                    ),
                ] {
                    manager
                        .advance_resume(operation.clone(), step, now)
                        .unwrap();
                }
            }
            assert_eq!(
                manager.status().status,
                if activated {
                    ControlledStatusKind::ResumeActivatedPendingLiveness
                } else {
                    ControlledStatusKind::ResumeProvisional
                }
            );
            assert_eq!(manager.status().watchdog_deadline_b, Some(33_000));
            assert_eq!(cleanup.0.load(Ordering::SeqCst), 0);
            let failed = manager.mark_transport_disconnected(12_000);
            assert_eq!(failed.status, ControlledStatusKind::Error);
            assert_eq!(
                failed.error_code.as_deref(),
                Some("desktop_remote_resume_invalid")
            );
            assert!(manager.live_lease().is_err());
            assert!(manager.live_runtime_context().is_err());
            assert!(manager.with_local_cancel_lease(|_| ()).is_err());
            assert_eq!(manager.live_liveness_epoch(), None);
            {
                let inner = manager.inner.lock();
                assert!(inner.generation.is_none());
                assert!(inner.lease_context.is_none());
                assert!(inner.resume_gate.is_none());
                assert!(inner.watchdog.is_none());
                assert!(inner.liveness_epoch_a.is_none());
            }
            assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
            for now in [13_000, 34_000] {
                assert_eq!(
                    manager.mark_transport_disconnected(now).status,
                    ControlledStatusKind::Error
                );
                assert_eq!(manager.status().watchdog_deadline_b, None);
                assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
            }
            assert!(manager.begin_resume(operation, 35_000).is_err());
            assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
        }
    }

    fn pending_permission_for_test(
        authority: &super::super::permission::PermissionAuthority,
        context: &LeaseContext,
        task_id: &str,
    ) -> String {
        authority
            .request(
                context,
                task_id,
                super::super::permission::PermissionRequest {
                    operation_type: "file_write".into(),
                    risk_level: "medium".into(),
                    target_scope: "workspace/report.md".into(),
                    preview: "write report".into(),
                    expires_at_ms: 60_000,
                },
                1_000,
            )
            .unwrap()
            .permission_request_id
    }

    #[test]
    fn retirement_denies_pre_admission_permissions_but_keeps_resume_and_dispatch_history() {
        use super::super::permission::{
            DecisionAuthority, PermissionAuthority, PermissionDecision,
        };
        for terminal in ["stop", "remote_disconnect", "watchdog"] {
            let manager = ControlledDeviceManager::default();
            let authority = Arc::new(PermissionAuthority::with_clock(Arc::new(|| 1_000)));
            authority.register_controlled_cleanup(&manager);
            let operation = manager.start(None, Vec::new()).unwrap();
            register(&manager, &operation);
            enter_handshake(&manager, &operation);
            manager
                .complete_connected(operation.clone(), lease(operation.generation()))
                .unwrap();
            let context = manager.live_lease().unwrap();
            // No task service admission exists for this pending record.
            let pending = pending_permission_for_test(&authority, &context, "pre-admission-task");
            let mut other = context.clone();
            other.remote_control_generation.push_str("-other");
            let unrelated = pending_permission_for_test(&authority, &other, "other-task");
            let committed = pending_permission_for_test(&authority, &context, "committed-task");
            let awaiting =
                pending_permission_for_test(&authority, &context, "awaiting-dispatch-task");
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &context,
                    "awaiting-dispatch-task",
                    &awaiting,
                    PermissionDecision::Allow,
                    1_000,
                )
                .unwrap();
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &context,
                    "committed-task",
                    &committed,
                    PermissionDecision::Allow,
                    1_000,
                )
                .unwrap();
            authority.commit_dispatch(&committed, 1_000).unwrap();
            let committed_before =
                serde_json::to_value(authority.view(&committed).unwrap()).unwrap();
            manager.mark_transport_disconnected(4_000);
            assert_eq!(authority.view(&pending).unwrap().state, "pending");
            match terminal {
                "stop" => {
                    manager.stop();
                }
                "remote_disconnect" => {
                    manager
                        .accept_remote_disconnect_for(&operation, &context)
                        .unwrap();
                }
                _ => {
                    let deadline = manager.watchdog_deadline_for_test().unwrap();
                    let status = manager.poll_watchdog(deadline).unwrap();
                    assert_eq!(status.status, ControlledStatusKind::Error);
                    assert_eq!(
                        status.error_code.as_deref(),
                        Some("desktop_remote_watchdog_expired")
                    );
                }
            }
            assert_eq!(
                authority.view(&pending).unwrap().state,
                "denied",
                "{terminal}"
            );
            assert_eq!(authority.view(&unrelated).unwrap().state, "pending");
            assert_eq!(authority.view(&awaiting).unwrap().state, "denied");
            assert_eq!(
                serde_json::to_value(authority.view(&committed).unwrap()).unwrap(),
                committed_before
            );
            manager.stop();
            assert_eq!(authority.view(&pending).unwrap().state, "denied");
        }
    }

    #[test]
    fn delayed_permission_retirement_keeps_a_successor_with_reused_connection_fields() {
        use super::super::permission::PermissionAuthority;
        struct PausedCleanup {
            old: String,
            entered: std::sync::mpsc::Sender<()>,
            release: Mutex<std::sync::mpsc::Receiver<()>>,
        }
        impl ControlledCleanupSink for PausedCleanup {
            fn cleanup(&self, generation: &str, _: ControlledCleanupScope) {
                if generation == self.old {
                    self.entered.send(()).unwrap();
                    self.release
                        .lock()
                        .recv_timeout(std::time::Duration::from_secs(5))
                        .unwrap();
                }
            }
        }
        let manager = Arc::new(ControlledDeviceManager::default());
        let old = manager.start(None, Vec::new()).unwrap();
        register(&manager, &old);
        enter_handshake(&manager, &old);
        manager
            .complete_connected(old.clone(), lease(old.generation()))
            .unwrap();
        let context = manager.live_lease().unwrap();
        let (entered_tx, entered_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        manager.register_cleanup_sink(Arc::new(PausedCleanup {
            old: old.generation().into(),
            entered: entered_tx,
            release: Mutex::new(release_rx),
        }));
        let authority = Arc::new(PermissionAuthority::with_clock(Arc::new(|| 1_000)));
        authority.register_controlled_cleanup(&manager);
        let pending = pending_permission_for_test(&authority, &context, "old-unadmitted-task");
        let retiring = manager.clone();
        let thread =
            std::thread::spawn(move || retiring.accept_remote_disconnect_for(&old, &context));
        entered_rx
            .recv_timeout(std::time::Duration::from_secs(2))
            .unwrap();
        let successor = manager.start(None, Vec::new()).unwrap();
        register(&manager, &successor);
        enter_handshake(&manager, &successor);
        manager
            .complete_connected(successor.clone(), lease(successor.generation()))
            .unwrap();
        let fresh = pending_permission_for_test(
            &authority,
            &manager.live_lease().unwrap(),
            "new-unadmitted-task",
        );
        release_tx.send(()).unwrap();
        thread.join().unwrap().unwrap();
        assert_eq!(authority.view(&pending).unwrap().state, "denied");
        assert_eq!(authority.view(&fresh).unwrap().state, "pending");
        assert_eq!(
            manager.current_generation().as_deref(),
            Some(successor.generation())
        );
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
                    identity: super::super::super::platform::test_workspace_identity(3),
                }],
            )
            .unwrap();
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
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
            manager.poll_watchdog(17_000).unwrap().status,
            ControlledStatusKind::Reconnecting
        );
        let status = manager.poll_watchdog(31_000).unwrap();
        assert_eq!(
            status.generation.as_deref(),
            Some(operation.generation.as_str())
        );
        assert!(manager.inner.lock().generation.is_none());
        assert_eq!(status.status, ControlledStatusKind::Error);
        assert_eq!(
            status.error_code.as_deref(),
            Some("desktop_remote_watchdog_expired")
        );
        assert_eq!(status.workspace_count, 0);
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn late_valid_liveness_recovers_suspect_without_corrupting_resume_gate() {
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
            manager.poll_watchdog(17_000).unwrap().status,
            ControlledStatusKind::Reconnecting
        );
        manager
            .register_liveness_challenge("challenge-0002", "AQEBAQEBAQEBAQEBAQEBAQ==", 17_000)
            .unwrap();
        let second = serde_json::json!({
            "v": 1,
            "type": "controller_liveness_a",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "1",
            "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": operation.generation.clone(),
            "role": "A",
            "liveness_epoch_a": "live-a-1",
            "heartbeat_sequence_a": "2",
            "challenge_id": "challenge-0002",
            "challenge_b64": "AQEBAQEBAQEBAQEBAQEBAQ=="
        })
        .to_string();
        manager.accept_direct_liveness(&second, 18_000).unwrap();
        assert_eq!(manager.status().status, ControlledStatusKind::Connected);

        // 活性可疑不应提前消费 resume gate；随后发生真实断线时仍能启动完整恢复。
        assert_eq!(
            manager.mark_transport_disconnected(19_000).status,
            ControlledStatusKind::Reconnecting
        );
        manager.begin_resume(operation, 20_000).unwrap();
    }

    #[test]
    fn stop_and_disconnect_clear_all_sensitive_context_without_network() {
        let cleanup = Arc::new(CleanupCounter::default());
        let manager = ControlledDeviceManager::default();
        manager.register_cleanup_sink(cleanup.clone());
        let operation = manager.start(Some("B"), Vec::new()).unwrap();
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
        let stopping = manager.begin_stop();
        let cleanup_after_stop = cleanup.0.load(Ordering::SeqCst);
        assert_eq!(
            manager
                .fail_operation(operation.clone(), "desktop_remote_late_failure".into())
                .unwrap_err()
                .code,
            "desktop_remote_stale_result"
        );
        assert_eq!(manager.status(), stopping);
        assert_eq!(cleanup.0.load(Ordering::SeqCst), cleanup_after_stop);
        let stopped = manager.finish_stop();
        assert_eq!(stopped.status, ControlledStatusKind::Stopped);
        assert_eq!(stopped.pairing_code, None);
        assert_eq!(stopped.generation, Some(operation.generation.clone()));
        assert_eq!(manager.status().generation, stopped.generation);
        assert!(manager.inner.lock().generation.is_none());
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
        assert_eq!(
            manager
                .fail_operation(operation.clone(), "desktop_remote_late_failure".into())
                .unwrap_err()
                .code,
            "desktop_remote_stale_result"
        );
        assert_eq!(manager.status(), stopped);
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);

        let second = manager.start(Some("B"), Vec::new()).unwrap();
        register(&manager, &second);
        enter_handshake(&manager, &second);
        manager
            .complete_connected(second.clone(), lease(&second.generation))
            .unwrap();
        let disconnected = manager.disconnect_now();
        assert_eq!(disconnected.status, ControlledStatusKind::Stopped);
        assert_eq!(disconnected.generation, Some(second.generation.clone()));
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 2);
        assert_eq!(
            manager
                .fail_operation(second, "desktop_remote_late_failure".into())
                .unwrap_err()
                .code,
            "desktop_remote_stale_result"
        );
        assert_eq!(manager.status(), disconnected);
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 2);
    }

    #[test]
    fn stale_worker_poll_cannot_expire_or_observe_a_replacement_operation() {
        let manager = ControlledDeviceManager::default();
        let old = manager.start(None, Vec::new()).unwrap();
        manager.stop();
        let current = manager.start(None, Vec::new()).unwrap();
        register(&manager, &current);
        enter_handshake(&manager, &current);
        manager
            .complete_connected(current.clone(), lease(&current.generation))
            .unwrap();
        let before = manager.status();
        assert_eq!(
            manager.poll_watchdog_for(&old, u64::MAX).unwrap_err().code,
            "desktop_remote_stale_result"
        );
        assert_eq!(manager.status(), before);
        let expired = manager.poll_watchdog_for(&current, u64::MAX).unwrap();
        assert_eq!(expired.kind(), ControlledStatusKind::Error);
        assert_eq!(
            expired.error_code.as_deref(),
            Some("desktop_remote_watchdog_expired")
        );
        assert!(manager.live_lease().is_err());
    }

    #[test]
    fn terminal_errors_keep_only_retired_identity_and_new_start_clears_it() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        let failed = manager
            .fail_operation(operation.clone(), "desktop_remote_test_error".into())
            .unwrap();
        assert_eq!(failed.status, ControlledStatusKind::Error);
        assert_eq!(
            failed.generation.as_deref(),
            Some(operation.generation.as_str())
        );
        assert_eq!(manager.status().generation, failed.generation);
        assert!(manager.live_lease().is_err());
        let next = manager.start(None, Vec::new()).unwrap();
        assert_ne!(next.generation, operation.generation);
        assert_eq!(
            manager.status().generation.as_deref(),
            Some(next.generation.as_str())
        );
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
                    identity: super::super::super::platform::test_workspace_identity(4),
                }],
            )
            .unwrap();
        let refresh = manager.begin_refresh().unwrap();
        let status = manager.fail_refresh(refresh).unwrap();
        assert_eq!(
            status.generation.as_deref(),
            Some(operation.generation.as_str())
        );
        assert!(manager.inner.lock().generation.is_none());
        assert_eq!(status.status, ControlledStatusKind::Error);
        assert_eq!(
            status.error_code.as_deref(),
            Some("desktop_remote_refresh_failed")
        );
        assert_eq!(status.workspace_count, 0);
        assert_eq!(cleanup.0.load(Ordering::SeqCst), 1);
        assert!(manager.workspaces().is_empty());
        assert_eq!(operation.intent, 1);
    }

    #[test]
    fn non_windows_platforms_do_not_claim_supported_and_pairing_code_grammar_is_closed() {
        for code in ["1234567", "0123456", "234567", "234567I", "2345670"] {
            assert!(validate_pairing_code(code).is_err(), "{code}");
        }
        assert!(validate_pairing_code("2345678").is_ok());
    }

    /// E2E-06：A 端显式断开终结租约但保留用户工作区授权，且只有已连接的代际
    /// 可以接受该帧；未连接重复断开失败关闭。
    #[test]
    fn remote_disconnect_terminates_the_lease_and_keeps_workspaces() {
        let manager = ControlledDeviceManager::default();
        let operation = manager
            .start(
                Some("Studio PC"),
                vec![WorkspaceAuthorizationRequest {
                    path: PathBuf::from(r"D:\projects"),
                    display_name: "Projects".into(),
                    code_allowed: true,
                    identity: super::super::super::platform::test_workspace_identity(5),
                }],
            )
            .unwrap();
        assert_eq!(
            manager
                .accept_remote_disconnect_for(&operation, &lease(&operation.generation).context)
                .unwrap_err()
                .code,
            "desktop_remote_phase_invalid",
            "尚未连接时不得接受远端断开"
        );
        register(&manager, &operation);
        enter_handshake(&manager, &operation);
        manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        let status = manager
            .accept_remote_disconnect_for(&operation, &lease(&operation.generation).context)
            .unwrap();
        assert_eq!(status.status, ControlledStatusKind::Stopped);
        assert_eq!(status.workspace_count, 1, "用户工作区授权必须保留");
        assert_eq!(status.controller_device_name, None);
        assert_eq!(status.pairing_code, None, "远端断开后不得残留连接码");
        assert_eq!(
            manager.live_lease().unwrap_err().code,
            "desktop_remote_not_connected"
        );
        assert_eq!(
            manager
                .accept_remote_disconnect_for(&operation, &lease(&operation.generation).context)
                .unwrap_err()
                .code,
            "desktop_remote_stale_result"
        );
    }

    #[test]
    fn delayed_remote_disconnect_cannot_terminate_a_successor_or_a_rotated_lease() {
        let manager = ControlledDeviceManager::default();
        let old = manager.start(None, Vec::new()).unwrap();
        register(&manager, &old);
        enter_handshake(&manager, &old);
        let old_lease = lease(old.generation()).context;
        manager
            .complete_connected(old.clone(), lease(old.generation()))
            .unwrap();
        // This is the deterministic window after an old worker validated its
        // frozen lease but before it asks the state owner to commit disconnect.
        manager.stop();
        let successor = manager.start(None, Vec::new()).unwrap();
        register(&manager, &successor);
        enter_handshake(&manager, &successor);
        let new_lease = lease(successor.generation()).context;
        manager
            .complete_connected(successor.clone(), lease(successor.generation()))
            .unwrap();
        let before = manager.status();
        assert_eq!(
            manager
                .accept_remote_disconnect_for(&old, &old_lease)
                .unwrap_err()
                .code,
            "desktop_remote_stale_result"
        );
        assert!(manager.status() == before);
        let mut foreign = new_lease.clone();
        foreign.controller_lease_id = "foreign-lease".into();
        assert_eq!(
            manager
                .accept_remote_disconnect_for(&successor, &foreign)
                .unwrap_err()
                .code,
            "desktop_remote_phase_invalid"
        );
        assert_eq!(manager.live_lease().unwrap(), new_lease);
    }

    #[test]
    fn remote_disconnect_cleanup_runs_unlocked_and_keeps_its_retired_identity() {
        struct PausedCleanup {
            retired: String,
            entered: std::sync::mpsc::Sender<String>,
            release: Mutex<std::sync::mpsc::Receiver<()>>,
        }
        impl ControlledCleanupSink for PausedCleanup {
            fn cleanup(&self, generation: &str, _: ControlledCleanupScope) {
                if generation == self.retired {
                    self.entered.send(generation.to_owned()).unwrap();
                    self.release
                        .lock()
                        .recv_timeout(std::time::Duration::from_secs(5))
                        .unwrap();
                }
            }
        }
        let manager = Arc::new(ControlledDeviceManager::default());
        let old = manager.start(None, Vec::new()).unwrap();
        register(&manager, &old);
        enter_handshake(&manager, &old);
        manager
            .complete_connected(old.clone(), lease(old.generation()))
            .unwrap();
        let (entered_tx, entered_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        manager.register_cleanup_sink(Arc::new(PausedCleanup {
            retired: old.generation().to_owned(),
            entered: entered_tx,
            release: Mutex::new(release_rx),
        }));
        let retiring = manager.clone();
        let expected = lease(old.generation()).context;
        let old_generation = old.generation().to_owned();
        let thread =
            std::thread::spawn(move || retiring.accept_remote_disconnect_for(&old, &expected));
        assert_eq!(
            entered_rx
                .recv_timeout(std::time::Duration::from_secs(2))
                .unwrap(),
            old_generation
        );
        // A successor can be installed while the old cleanup is paused. This
        // would deadlock if host callbacks still ran under the state owner lock.
        let successor = manager.start(None, Vec::new()).unwrap();
        register(&manager, &successor);
        enter_handshake(&manager, &successor);
        manager
            .complete_connected(successor.clone(), lease(successor.generation()))
            .unwrap();
        release_tx.send(()).unwrap();
        let retired_status = thread.join().unwrap().unwrap();
        assert_eq!(
            retired_status.generation.as_deref(),
            Some(old_generation.as_str())
        );
        assert_eq!(
            manager.current_generation().as_deref(),
            Some(successor.generation())
        );
        assert_eq!(
            manager.live_lease().unwrap(),
            lease(successor.generation()).context
        );
    }

    /// E2E-02：只有注册/握手待决的代际才允许在途 worker 把状态收尾为 error；
    /// 用户停止/断开（或已连接）之后不得覆盖既有投影。
    #[test]
    fn pending_handshake_is_the_only_owner_of_failure_projection() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(Some("Studio PC"), Vec::new()).unwrap();
        assert!(manager.is_pending_handshake(operation.clone()));
        register(&manager, &operation);
        assert!(manager.is_pending_handshake(operation.clone()));
        enter_handshake(&manager, &operation);
        assert!(manager.is_pending_handshake(operation.clone()));
        manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        assert!(
            !manager.is_pending_handshake(operation.clone()),
            "已连接后不得再投影握手失败"
        );

        manager.stop();
        assert!(
            !manager.is_pending_handshake(operation.clone()),
            "用户已停止时在途失败不得覆盖状态"
        );
        assert_eq!(
            manager
                .fail_operation(operation, "invalid_message".to_owned())
                .unwrap_err()
                .code,
            "desktop_remote_stale_result",
            "停止会清空 generation，在途结果被状态机判定为陈旧"
        );
    }
}

#[cfg(test)]
#[path = "controlled/preconnect_tests.rs"]
mod preconnect_tests;
