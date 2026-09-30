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

use super::liveness::{
    ControllerLivenessWatchdog, LivenessWatchdogState, MAX_LIVENESS_ATTESTATION_AGE_MS,
};
use super::protocol::LeaseContext;
use super::relay_transport::ControllerTransportSender;
use super::resume::ControllerResumeGate;

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

impl ControlledOperation {
    pub(crate) fn generation(&self) -> &str {
        &self.generation
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
    error_code: Option<String>,
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

/// E2E-06：B 端断线恢复所需的本地材料（令牌只以内存数组存在，Debug 脱敏）。
pub(crate) struct ControlledResumeMaterial {
    pub(crate) lease: LeaseContext,
    pub(crate) resume_token: [u8; 32],
    pub(crate) original_deadline_b: u64,
}

impl std::fmt::Debug for ControlledResumeMaterial {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ControlledResumeMaterial")
            .field("lease", &self.lease)
            .field("resume_token", &"[REDACTED]")
            .field("original_deadline_b", &self.original_deadline_b)
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

struct Inner {
    phase: ControlledStatusKind,
    intent: u64,
    generation: Option<String>,
    registration_request_id: Option<String>,
    device_name: Option<String>,
    pairing: Option<PairingState>,
    lease_context: Option<LeaseContext>,
    negotiated_capabilities: Vec<String>,
    liveness_epoch_a: Option<String>,
    controller_device_name: Option<String>,
    connected_at_b: Option<u64>,
    watchdog: Option<ControllerLivenessWatchdog>,
    resume_gate: Option<ControllerResumeGate>,
    reconnect_reason: Option<ReconnectReason>,
    last_observed_now_b: u64,
    transport: Option<ControllerTransportSender>,
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
            registration_request_id: None,
            device_name: None,
            pairing: None,
            lease_context: None,
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

    pub(crate) fn current_operation(&self) -> Option<ControlledOperation> {
        current_operation(&self.inner.lock())
    }

    pub(crate) fn current_generation(&self) -> Option<String> {
        self.inner.lock().generation.clone()
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
        let watchdog = ControllerLivenessWatchdog::new(lease.liveness_epoch_a.clone())
            .map_err(|_| ControlledStateError::new("liveness_epoch_invalid"))?;
        let resume_gate = ControllerResumeGate::new(
            super::relay_transport::ControllerRole::ControlledB,
            lease.context.clone(),
            lease.resume_token,
            lease.transport_key_fingerprint,
            lease.resume_deadline_b,
        );
        inner.lease_context = Some(lease.context);
        inner.negotiated_capabilities = lease.negotiated_capabilities;
        inner.liveness_epoch_a = Some(lease.liveness_epoch_a);
        inner.controller_device_name = Some(lease.controller_device_name);
        inner.connected_at_b = Some(lease.connected_at_b);
        inner.watchdog = Some(watchdog);
        inner.resume_gate = Some(resume_gate);
        inner.reconnect_reason = None;
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
        if inner.phase == ControlledStatusKind::Connected
            || (inner.phase == ControlledStatusKind::Reconnecting
                && inner.reconnect_reason == Some(ReconnectReason::LivenessSuspect))
        {
            let deadline = inner
                .watchdog
                .as_ref()
                .and_then(|watchdog| watchdog.deadline_b());
            if let Some(gate) = inner.resume_gate.as_mut() {
                if gate.disconnect(now_b, deadline).is_err() {
                    inner.phase = ControlledStatusKind::Error;
                    inner.error_code = Some("desktop_remote_resume_invalid".to_owned());
                    inner.reconnect_reason = None;
                } else {
                    inner.phase = ControlledStatusKind::Reconnecting;
                    inner.reconnect_reason = Some(ReconnectReason::Transport);
                }
            } else {
                inner.phase = ControlledStatusKind::Reconnecting;
                inner.reconnect_reason = Some(ReconnectReason::Transport);
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
        let Some(watchdog) = inner.watchdog.as_mut() else {
            return Err(ControlledStateError::new("watchdog_unavailable"));
        };
        watchdog
            .register_challenge(challenge_id, challenge_b64, sent_at_b)
            .map_err(|_| ControlledStateError::new("liveness_challenge_invalid"))
    }

    /// 本地未成功发出或已超时的挑战必须从 watchdog 一并撤销，否则 worker 的
    /// 轻量状态虽已 reset，watchdog 内仍会累积到容量上限。
    pub(crate) fn cancel_liveness_challenge(&self, challenge_id: &str) {
        let mut inner = self.inner.lock();
        if let Some(watchdog) = inner.watchdog.as_mut() {
            watchdog.cancel_challenge(challenge_id);
        }
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
    pub(crate) fn begin_liveness_challenge(&self, now_b: u64) -> Result<(String, String)> {
        let (challenge_id, challenge_b64) = super::liveness::new_liveness_challenge()
            .map_err(|_| ControlledStateError::new("liveness_challenge_invalid"))?;
        {
            let mut inner = self.inner.lock();
            let Some(watchdog) = inner.watchdog.as_mut() else {
                return Err(ControlledStateError::new("watchdog_unavailable"));
            };
            watchdog
                .register_challenge(challenge_id.clone(), challenge_b64.clone(), now_b)
                .map_err(|_| ControlledStateError::new("liveness_challenge_invalid"))?;
        }
        Ok((challenge_id, challenge_b64))
    }

    pub(crate) fn accept_direct_liveness(&self, json: &str, received_at_b: u64) -> Result<()> {
        let mut inner = self.inner.lock();
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
        recover_liveness_suspect(&mut inner);
        Ok(())
    }

    pub(crate) fn accept_liveness_attestation(
        &self,
        json: &str,
        request_sent_at_b: u64,
        received_at_b: u64,
    ) -> Result<()> {
        let mut inner = self.inner.lock();
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
        recover_liveness_suspect(&mut inner);
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
        inner.error_code = Some("desktop_remote_refresh_failed".to_owned());
        drop(inner);
        self.notify_cleanup(&generation);
        Ok(self.status())
    }

    pub(crate) fn fail_operation(
        &self,
        operation: ControlledOperation,
        error_code: String,
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
        activation_id: &str,
        now_b: u64,
        liveness_epoch_a: impl Into<String>,
        resume_token: [u8; 32],
        transport_key_fingerprint: [u8; 32],
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
        // E2E-06：先用完整的 `controller_resumed` 让旧 gate 消费旧材料；任何
        // 相位、activation 或轮换材料不符都必须终止候选，绝不产生半恢复状态。
        let watchdog_deadline = inner
            .watchdog
            .as_ref()
            .and_then(|watchdog| watchdog.deadline_b());
        let accepted = match inner.resume_gate.as_mut() {
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
        if !accepted {
            let generation = inner.generation.clone().unwrap_or_default();
            clear_sensitive(&mut inner, u64::MAX, true);
            inner.device_name = None;
            inner.phase = ControlledStatusKind::Error;
            inner.error_code = Some(RESUME_FAILED_CODE.to_owned());
            drop(inner);
            self.notify_cleanup(&generation);
            return Err(ControlledStateError::new("desktop_remote_resume_failed"));
        }
        let deadline = now_b.saturating_add(30_000);
        // Replacing the gate with the already-authenticated final material is
        // the single transition that cancels the old watchdog.
        inner.resume_gate = Some(ControllerResumeGate::new(
            super::relay_transport::ControllerRole::ControlledB,
            context,
            resume_token,
            transport_key_fingerprint,
            deadline,
        ));
        inner.watchdog = Some(
            ControllerLivenessWatchdog::new(epoch.clone())
                .map_err(|_| ControlledStateError::new("liveness_epoch_invalid"))?,
        );
        inner.liveness_epoch_a = Some(epoch);
        inner.phase = ControlledStatusKind::Connected;
        inner.reconnect_reason = None;
        Ok(status_locked(&inner))
    }

    /// E2E-06：断线（`desktop_relay_connection{connected:false}`）后取恢复材料并让
    /// 本端 gate 进入 `Prepared`；相位不符或 gate 已终止时失败关闭并清理本代。
    pub(crate) fn begin_resume(
        &self,
        operation: ControlledOperation,
        now_b: u64,
    ) -> Result<ControlledResumeMaterial> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
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
        let (prepared, original_deadline_b, resume_token) = match inner.resume_gate.as_mut() {
            Some(gate) => {
                let prepared = gate.prepare(now_b, deadline).is_ok();
                (prepared, gate.original_deadline_b(), gate.current_token())
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
            original_deadline_b,
        })
    }

    /// E2E-06：按帧顺序推进 B 端恢复相位；任一步失败即终止候选并清理本代。
    pub(crate) fn advance_resume(
        &self,
        operation: ControlledOperation,
        step: ControlledResumeStep<'_>,
        now_b: u64,
    ) -> Result<ControlledStatus> {
        let mut inner = self.inner.lock();
        ensure_operation(&inner, &operation)?;
        if inner.phase != ControlledStatusKind::Reconnecting {
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
        Ok(status_locked(&inner))
    }

    pub(crate) fn stop(&self) -> ControlledStatus {
        self.terminate_local(
            "desktop_remote_stopped",
            ControlledStatusKind::Stopped,
            true,
        )
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

    pub(crate) fn accept_remote_disconnect(&self) -> Result<ControlledStatus> {
        {
            let inner = self.inner.lock();
            if inner.lease_context.is_none()
                || !matches!(
                    inner.phase,
                    ControlledStatusKind::Connected | ControlledStatusKind::Reconnecting
                )
            {
                return Err(ControlledStateError::new("desktop_remote_phase_invalid"));
            }
        }
        Ok(self.terminate_local(
            "desktop_remote_controller_disconnected",
            ControlledStatusKind::Stopped,
            false,
        ))
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

    /// 本次代际是否仍处于注册/握手的待决阶段。在途 worker 的失败收尾据此判断
    /// 能否投影 error：用户已显式停止/断开、或本代已被刷新取代时不得覆盖。
    pub(crate) fn is_pending_handshake(&self, operation: ControlledOperation) -> bool {
        let inner = self.inner.lock();
        ensure_operation(&inner, &operation).is_ok()
            && matches!(
                inner.phase,
                ControlledStatusKind::Reserving
                    | ControlledStatusKind::WaitingConnection
                    | ControlledStatusKind::SafetyHandshake
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
            inner.error_code =
                (phase == ControlledStatusKind::Error).then(|| error_code.to_owned());
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
    inner.reconnect_reason = None;
    inner.pairing = None;
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

fn recover_liveness_suspect(inner: &mut Inner) {
    if inner.phase == ControlledStatusKind::Reconnecting
        && inner.reconnect_reason == Some(ReconnectReason::LivenessSuspect)
    {
        inner.phase = ControlledStatusKind::Connected;
        inner.reconnect_reason = None;
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
        error_code: inner.error_code.clone(),
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
        assert_eq!(connected.watchdog_deadline_b, None);
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

    /// E2E-06：B 端恢复相位必须按冻结顺序推进（provisional → confirmed →
    /// activation_ack → post_activation_liveness → ready_ack → resumed），顺序错乱
    /// 或材料复用失败关闭并清理本代。
    #[test]
    fn resume_sequence_advances_the_gate_and_rotates_material() {
        let manager = ControlledDeviceManager::default();
        let operation = manager.start(None, Vec::new()).unwrap();
        register(&manager, &operation);
        manager.mark_claimed(operation.clone()).unwrap();
        manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        assert_eq!(
            manager.mark_transport_disconnected(5_000).status,
            ControlledStatusKind::Reconnecting
        );
        let material = manager.begin_resume(operation.clone(), 6_000).unwrap();
        assert_eq!(material.original_deadline_b, 30_000);
        assert!(format!("{material:?}").contains("[REDACTED]"));
        assert_eq!(material.resume_token, [1; 32]);

        let activation = "activation-0001";
        for (step, now_b) in [
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
                .advance_resume(operation.clone(), step, now_b)
                .unwrap();
        }
        let resumed = manager
            .complete_resumed(
                operation.clone(),
                activation,
                12_000,
                "live-a-2",
                [3; 32],
                [4; 32],
            )
            .unwrap();
        assert_eq!(resumed.status, ControlledStatusKind::Connected);
        assert_eq!(manager.live_liveness_epoch(), Some("live-a-2".to_owned()));

        // 第二次断线后相位错乱（未 provisional 就 confirm）必须失败关闭。
        manager.mark_transport_disconnected(13_000);
        manager.begin_resume(operation.clone(), 14_000).unwrap();
        assert_eq!(
            manager
                .advance_resume(
                    operation,
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
            manager.status().error_code.as_deref(),
            Some("desktop_remote_resume_invalid")
        );
        assert!(manager.live_lease().is_err());
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
            manager.poll_watchdog(17_000).status,
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
        assert_eq!(
            status.error_code.as_deref(),
            Some("desktop_remote_refresh_failed")
        );
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
                }],
            )
            .unwrap();
        assert_eq!(
            manager.accept_remote_disconnect().unwrap_err().code,
            "desktop_remote_phase_invalid",
            "尚未连接时不得接受远端断开"
        );
        register(&manager, &operation);
        manager.mark_claimed(operation.clone()).unwrap();
        manager
            .complete_connected(operation.clone(), lease(&operation.generation))
            .unwrap();
        let status = manager.accept_remote_disconnect().unwrap();
        assert_eq!(status.status, ControlledStatusKind::Stopped);
        assert_eq!(status.workspace_count, 1, "用户工作区授权必须保留");
        assert_eq!(status.controller_device_name, None);
        assert_eq!(status.pairing_code, None, "远端断开后不得残留连接码");
        assert_eq!(
            manager.live_lease().unwrap_err().code,
            "desktop_remote_not_connected"
        );
        assert_eq!(
            manager.accept_remote_disconnect().unwrap_err().code,
            "desktop_remote_phase_invalid"
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
        manager.mark_claimed(operation.clone()).unwrap();
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
