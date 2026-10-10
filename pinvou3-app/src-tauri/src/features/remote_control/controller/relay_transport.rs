//! Bounded Relay transport shared by the controller and controlled-device domains.
//!
//! This layer owns only WebSocket lifecycle, queueing and lease-context admission.
//! Pairing, liveness, resume and task semantics remain in their dedicated modules.

use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use futures_util::{Sink, SinkExt, Stream, StreamExt};
use parking_lot::RwLock;
use serde::Serialize;
use serde_json::Value;
use tokio::sync::{OwnedSemaphorePermit, Semaphore, mpsc};
use tokio_tungstenite::{connect_async_with_config, tungstenite::Message};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

use super::protocol::{
    LeaseContext, MAX_CONTROL_MESSAGE_BYTES, MAX_ENCRYPTED_WIRE_BYTES, RuntimeValidationContext,
    SenderRole, is_valid_identifier, validate_controller_message_json,
    validate_controller_runtime_message_json,
};
use crate::features::remote_control::relay_client::{
    MAX_RELAY_FRAME_BYTES, ReconnectBackoff, controller_websocket_request, relay_websocket_config,
};

const CONTROL_QUEUE_CAPACITY: usize = 64;
const BUSINESS_QUEUE_CAPACITY: usize = 256;
const TOTAL_OUTBOUND_BYTE_BUDGET: usize = 8 * 1024 * 1024;
const CONTROL_RESERVED_BYTES: usize = 128 * 1024;
// Two unanswered transport heartbeat windows must leave time inside the
// independent 30s authenticated-evidence budget. Pongs never extend it.
const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(5);
const WRITE_TIMEOUT: Duration = Duration::from_secs(10);
const CLOSE_TIMEOUT: Duration = Duration::from_millis(250);
const MAX_CONTROLLER_INBOUND_TEXT_BYTES: usize = if MAX_ENCRYPTED_WIRE_BYTES < MAX_RELAY_FRAME_BYTES
{
    MAX_ENCRYPTED_WIRE_BYTES
} else {
    MAX_RELAY_FRAME_BYTES
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ControllerRole {
    ControllerA,
    ControlledB,
}

impl ControllerRole {
    fn sender_role(self) -> SenderRole {
        match self {
            Self::ControllerA => SenderRole::ControllerA,
            Self::ControlledB => SenderRole::ControlledB,
        }
    }

    fn peer_role(self) -> SenderRole {
        match self {
            Self::ControllerA => SenderRole::ControlledB,
            Self::ControlledB => SenderRole::ControllerA,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FramePriority {
    Control,
    Business,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct AuthenticatedLease {
    pub(crate) context: LeaseContext,
    pub(crate) negotiated_capabilities: Vec<String>,
}

pub(crate) trait TransportClock: Send + Sync {
    fn now_millis(&self) -> u64;
}

#[derive(Debug)]
pub(crate) struct MonotonicTransportClock {
    origin: Instant,
}

impl Default for MonotonicTransportClock {
    fn default() -> Self {
        Self {
            origin: Instant::now(),
        }
    }
}

impl TransportClock for MonotonicTransportClock {
    fn now_millis(&self) -> u64 {
        u64::try_from(self.origin.elapsed().as_millis()).unwrap_or(u64::MAX)
    }
}

#[derive(Clone)]
pub(crate) struct ControllerTransportConfig {
    pub(crate) relay_url: String,
    pub(crate) role: ControllerRole,
    /// E2E-02：`remote_control_generation` 由 B 拥有，A 只能在验签 transcript
    /// 之后才知道它。sender 与 transport 任务共享同一个单元格，因此受控的
    /// `bind_generation` 绑定结果对两侧同时生效。
    pub(crate) remote_control_generation: Arc<RwLock<String>>,
    pub(crate) clock: Arc<dyn TransportClock>,
}

impl ControllerTransportConfig {
    /// `remote_control_generation` 为空表示「尚未绑定」：只有 A 端在验签
    /// transcript 之后才允许通过 `bind_generation` 一次性补齐。
    pub(crate) fn new(
        relay_url: impl Into<String>,
        role: ControllerRole,
        remote_control_generation: impl Into<String>,
    ) -> Self {
        Self {
            relay_url: relay_url.into(),
            role,
            remote_control_generation: Arc::new(RwLock::new(remote_control_generation.into())),
            clock: Arc::new(MonotonicTransportClock::default()),
        }
    }

    /// 当前生效的 generation；未绑定时为空串。
    pub(crate) fn generation(&self) -> String {
        self.remote_control_generation.read().clone()
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum ControllerTransportEvent {
    Connected {
        remote_control_generation: String,
        lease_epoch: Option<String>,
    },
    Disconnected {
        remote_control_generation: String,
        lease_epoch: Option<String>,
        error: String,
    },
    Frame {
        priority: FramePriority,
        remote_control_generation: String,
        lease_epoch: Option<String>,
        json: String,
    },
    RttSample {
        remote_control_generation: String,
        millis: u64,
    },
}

pub(crate) trait ControllerEventSink: Send + Sync + 'static {
    fn emit(&self, event: ControllerTransportEvent);
}

impl<F> ControllerEventSink for F
where
    F: Fn(ControllerTransportEvent) + Send + Sync + 'static,
{
    fn emit(&self, event: ControllerTransportEvent) {
        self(event);
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum ControllerSendError {
    Serialization,
    Unauthenticated,
    InvalidFrame(String),
    FrameTooLarge {
        bytes: usize,
        limit: usize,
    },
    ByteBudgetExhausted,
    QueueFull(FramePriority),
    /// E2E-02：transport 的 `remote_control_generation` 已经被绑定过，或已被
    /// 已安装租约锁定，无法再次绑定。
    GenerationAlreadyBound,
    Closed,
}

impl fmt::Display for ControllerSendError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Serialization => formatter.write_str("controller frame serialization failed"),
            Self::Unauthenticated => formatter.write_str("controller lease is not authenticated"),
            Self::InvalidFrame(field) => write!(formatter, "invalid controller frame: {field}"),
            Self::FrameTooLarge { bytes, limit } => {
                write!(
                    formatter,
                    "controller frame is {bytes} bytes; limit is {limit}"
                )
            }
            Self::ByteBudgetExhausted => {
                formatter.write_str("controller outbound byte budget exhausted")
            }
            Self::QueueFull(priority) => write!(formatter, "controller {priority:?} queue is full"),
            Self::GenerationAlreadyBound => {
                formatter.write_str("controller transport generation is already bound")
            }
            Self::Closed => formatter.write_str("controller transport is closed"),
        }
    }
}

impl std::error::Error for ControllerSendError {}

enum WirePayload {
    Plain(String),
    Sensitive(Zeroizing<String>),
}

impl fmt::Debug for WirePayload {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Plain(text) => formatter
                .debug_struct("Plain")
                .field("bytes", &text.len())
                .finish(),
            Self::Sensitive(text) => formatter
                .debug_struct("Sensitive")
                .field("bytes", &text.len())
                .field("contents", &"[REDACTED]")
                .finish(),
        }
    }
}

impl WirePayload {
    fn as_str(&self) -> &str {
        match self {
            Self::Plain(text) => text,
            Self::Sensitive(text) => text.as_str(),
        }
    }
}

#[derive(Debug)]
struct QueuedFrame {
    flushed: Option<tokio::sync::oneshot::Sender<()>>,
    priority: FramePriority,
    transport_epoch: u64,
    lease_context: Option<LeaseContext>,
    resume_only: bool,
    payload: WirePayload,
    _total_permit: OwnedSemaphorePermit,
    _business_permit: Option<OwnedSemaphorePermit>,
}

struct ResumeInstallAuthority {
    activation_id: String,
    request_id: String,
    role: ControllerRole,
    transport_epoch: u64,
    context: LeaseContext,
}

fn is_resume_inbound(kind: Option<&str>) -> bool {
    matches!(
        kind,
        Some(
            "controller_resume_provisional"
                | "encrypted_resume_confirm"
                | "controller_resume_activation_pending"
                | "controller_post_activation_liveness_confirmed"
                | "controller_resume_ready_ack"
                | "controller_resumed"
        )
    )
}

impl QueuedFrame {
    fn text(&self) -> &str {
        self.payload.as_str()
    }
}

#[derive(Clone)]
pub(crate) struct ControllerTransportSender {
    role: ControllerRole,
    remote_control_generation: Arc<RwLock<String>>,
    control_tx: mpsc::Sender<QueuedFrame>,
    business_tx: mpsc::Sender<QueuedFrame>,
    total_budget: Arc<Semaphore>,
    business_budget: Arc<Semaphore>,
    lease: Arc<RwLock<Option<AuthenticatedLease>>>,
    // A disconnected lease is a resume-frame binding only, never business or TX authority.
    resume_lease: Arc<RwLock<Option<AuthenticatedLease>>>,
    resume_install: Arc<RwLock<Option<ResumeInstallAuthority>>>,
    transport_epoch: Arc<AtomicU64>,
    shutdown: CancellationToken,
}

impl ControllerTransportSender {
    /// E2E-02 受控绑定：A 端在验签 transcript 之后才有权把 transport 的
    /// generation 固定为 B 的 generation。只允许一次，且必须在安装租约之前；
    /// 空值、非法 id、重复绑定或已安装租约都失败关闭，绝不覆盖既有绑定。
    pub(crate) fn bind_generation(&self, generation: &str) -> Result<(), ControllerSendError> {
        if !is_valid_identifier(generation) {
            return Err(ControllerSendError::InvalidFrame(
                "remote_control_generation".into(),
            ));
        }
        if self.lease.read().is_some() {
            return Err(ControllerSendError::GenerationAlreadyBound);
        }
        let mut current = self.remote_control_generation.write();
        if !current.is_empty() {
            return Err(ControllerSendError::GenerationAlreadyBound);
        }
        *current = generation.to_owned();
        Ok(())
    }

    /// 当前生效的 generation；未绑定时为空串。
    pub(crate) fn generation(&self) -> String {
        self.remote_control_generation.read().clone()
    }

    pub(crate) fn install_authenticated_lease(
        &self,
        lease: AuthenticatedLease,
    ) -> Result<(), ControllerSendError> {
        let bound = self.generation();
        if bound.is_empty() || lease.context.remote_control_generation != bound {
            return Err(ControllerSendError::InvalidFrame(
                "remote_control_generation".into(),
            ));
        }
        let mut current = self.lease.write();
        if self.is_stopped() {
            return Err(ControllerSendError::Closed);
        }
        *current = Some(lease);
        *self.resume_lease.write() = None;
        *self.resume_install.write() = None;
        Ok(())
    }

    /// Consume the final frame's one-shot authority on the exact receiving
    /// socket epoch. A queued final from a subsequently closed socket cannot
    /// reopen TX, even if another socket has already reconnected.
    pub(crate) fn install_authenticated_lease_after_resume(
        &self,
        lease: AuthenticatedLease,
        activation_id: &str,
        request_id: &str,
    ) -> Result<(), ControllerSendError> {
        let bound = self.generation();
        if bound.is_empty() || lease.context.remote_control_generation != bound {
            return Err(ControllerSendError::InvalidFrame(
                "remote_control_generation".into(),
            ));
        }
        let mut current = self.lease.write();
        let mut saved = self.resume_lease.write();
        let mut authority = self.resume_install.write();
        if self.is_stopped() {
            return Err(ControllerSendError::Closed);
        }
        if current.is_some()
            || !saved
                .as_ref()
                .is_some_and(|old| old.context == lease.context)
            || !authority.as_ref().is_some_and(|proof| {
                proof.context == lease.context
                    && proof.activation_id == activation_id
                    && proof.request_id == request_id
                    && proof.role == self.role
                    && proof.transport_epoch == self.transport_epoch.load(Ordering::Acquire)
            })
        {
            return Err(ControllerSendError::Unauthenticated);
        }
        *current = Some(lease);
        *saved = None;
        *authority = None;
        Ok(())
    }

    fn record_resume_install(&self, frame: &Value, inbound_epoch: u64) -> bool {
        let current = self.lease.read();
        let saved = self.resume_lease.read();
        if self.is_stopped()
            || current.is_some()
            || inbound_epoch != self.transport_epoch.load(Ordering::Acquire)
            || !saved
                .as_ref()
                .is_some_and(|lease| inbound_context_matches(frame, Some(lease)))
        {
            return false;
        }
        let Some(activation_id) = frame.get("activation_id").and_then(Value::as_str) else {
            return false;
        };
        let Some(request_id) = frame.get("request_id").and_then(Value::as_str) else {
            return false;
        };
        let Some(saved) = saved.as_ref() else {
            return false;
        };
        *self.resume_install.write() = Some(ResumeInstallAuthority {
            activation_id: activation_id.to_owned(),
            transport_epoch: inbound_epoch,
            request_id: request_id.to_owned(),
            role: self.role,
            context: saved.context.clone(),
        });
        true
    }

    /// Restore only the saved lease (including its negotiated capabilities).
    /// The existing one-shot reader proof still authorizes installation.
    pub(crate) fn restore_authenticated_lease_after_resume(
        &self,
        context: &LeaseContext,
        activation_id: &str,
        request_id: &str,
    ) -> Result<(), ControllerSendError> {
        let saved = self
            .resume_lease
            .read()
            .clone()
            .filter(|saved| &saved.context == context)
            .ok_or(ControllerSendError::Unauthenticated)?;
        self.install_authenticated_lease_after_resume(saved, activation_id, request_id)
    }

    pub(crate) fn clear_authenticated_lease(&self) {
        let mut current = self.lease.write();
        *current = None;
        *self.resume_lease.write() = None;
        *self.resume_install.write() = None;
    }

    fn suspend_authenticated_lease(&self) {
        let mut current = self.lease.write();
        self.transport_epoch.fetch_add(1, Ordering::AcqRel);
        let mut resume = self.resume_lease.write();
        *self.resume_install.write() = None;
        if self.is_stopped() {
            *current = None;
            *resume = None;
        } else if let Some(previous) = current.take() {
            *resume = Some(previous);
        }
        // Repeated failed reconnects preserve the original binding, not a new lease.
    }

    pub(crate) fn authenticated_lease(&self) -> Option<AuthenticatedLease> {
        self.lease.read().clone()
    }

    pub(crate) fn try_send_control<T: Serialize>(
        &self,
        value: &T,
    ) -> Result<(), ControllerSendError> {
        let text = serde_json::to_string(value).map_err(|_| ControllerSendError::Serialization)?;
        enforce_frame_limit(text.len(), MAX_CONTROL_MESSAGE_BYTES)?;
        let validated = self.validate_control(&text)?;
        if validated.get("controller_lease_id").is_some() {
            return Err(ControllerSendError::Unauthenticated);
        }
        self.try_enqueue(FramePriority::Control, WirePayload::Plain(text), None)
    }

    pub(crate) fn try_send_authenticated_control<T: Serialize>(
        &self,
        value: &T,
    ) -> Result<(), ControllerSendError> {
        self.enqueue_authenticated_control(value, None)
    }

    pub(crate) fn send_control_with_receipt<T: Serialize>(
        &self,
        value: &T,
    ) -> Result<tokio::sync::oneshot::Receiver<()>, ControllerSendError> {
        let (sent, receipt) = tokio::sync::oneshot::channel();
        self.enqueue_authenticated_control(value, Some(sent))?;
        Ok(receipt)
    }

    fn enqueue_authenticated_control<T: Serialize>(
        &self,
        value: &T,
        flushed: Option<tokio::sync::oneshot::Sender<()>>,
    ) -> Result<(), ControllerSendError> {
        let text = serde_json::to_string(value).map_err(|_| ControllerSendError::Serialization)?;
        enforce_frame_limit(text.len(), MAX_CONTROL_MESSAGE_BYTES)?;
        let lease = self
            .lease
            .read()
            .clone()
            .ok_or(ControllerSendError::Unauthenticated)?;
        let lease_context = lease.context.clone();
        let context = RuntimeValidationContext {
            lease: Some(lease.context),
            negotiated_capabilities: lease.negotiated_capabilities,
        };
        let validated = validate_controller_runtime_message_json(
            &text,
            self.role.sender_role(),
            Some(&context),
        )
        .map_err(|error| ControllerSendError::InvalidFrame(error.field))?;
        if validated.get("type").and_then(Value::as_str) == Some("encrypted_frame") {
            return Err(ControllerSendError::InvalidFrame("message.type".into()));
        }
        self.try_enqueue_with_receipt(
            FramePriority::Control,
            WirePayload::Plain(text),
            Some(lease_context),
            flushed,
            false,
        )
    }

    /// Serialize credentials and other ephemeral handshake material through a
    /// redacted, zeroizing queue representation. Validation errors deliberately
    /// expose only the protocol field, never serialized contents.
    pub(crate) fn try_send_sensitive_control<T: Serialize>(
        &self,
        value: &T,
    ) -> Result<(), ControllerSendError> {
        let text = Zeroizing::new(
            serde_json::to_string(value).map_err(|_| ControllerSendError::Serialization)?,
        );
        enforce_frame_limit(text.len(), MAX_CONTROL_MESSAGE_BYTES)?;
        let validated = self.validate_control(text.as_str())?;
        if validated.get("controller_lease_id").is_some() {
            return Err(ControllerSendError::Unauthenticated);
        }
        self.try_enqueue(FramePriority::Control, WirePayload::Sensitive(text), None)
    }

    /// Resume starts without an installed runtime lease. Only its explicit
    /// handshake frames use the frozen recovery binding, never runtime authority.
    /// The resume state machine additionally binds the token and activation.
    pub(crate) fn try_send_sensitive_resume_control<T: Serialize>(
        &self,
        value: &T,
    ) -> Result<(), ControllerSendError> {
        let text = Zeroizing::new(
            serde_json::to_string(value).map_err(|_| ControllerSendError::Serialization)?,
        );
        enforce_frame_limit(text.len(), MAX_CONTROL_MESSAGE_BYTES)?;
        let validated = self.validate_control(text.as_str())?;
        if !matches!(
            validated.get("type").and_then(Value::as_str),
            Some(
                "controller_resume_prepare"
                    | "encrypted_resume_confirm"
                    | "controller_resume_activation_ack"
                    | "controller_post_activation_liveness"
                    | "controller_resume_ready_ack"
            )
        ) {
            return Err(ControllerSendError::InvalidFrame("message.type".into()));
        }
        let current = self.lease.read();
        let saved = self.resume_lease.read();
        if current.is_some() {
            return Err(ControllerSendError::Unauthenticated);
        }
        let lease = saved.as_ref().ok_or(ControllerSendError::Unauthenticated)?;
        if !inbound_context_matches(&validated, Some(lease)) {
            return Err(ControllerSendError::InvalidFrame("lease_context".into()));
        }
        self.try_enqueue_with_receipt(
            FramePriority::Control,
            WirePayload::Sensitive(text),
            Some(lease.context.clone()),
            None,
            true,
        )
    }

    pub(crate) fn try_send_business<T: Serialize>(
        &self,
        value: &T,
    ) -> Result<(), ControllerSendError> {
        let text = serde_json::to_string(value).map_err(|_| ControllerSendError::Serialization)?;
        let lease = self
            .lease
            .read()
            .clone()
            .ok_or(ControllerSendError::Unauthenticated)?;
        let lease_context = lease.context.clone();
        let context = RuntimeValidationContext {
            lease: Some(lease.context),
            negotiated_capabilities: lease.negotiated_capabilities,
        };
        let validated = validate_controller_runtime_message_json(
            &text,
            self.role.sender_role(),
            Some(&context),
        )
        .map_err(|error| ControllerSendError::InvalidFrame(error.field))?;
        if validated.get("type").and_then(Value::as_str) != Some("encrypted_frame") {
            return Err(ControllerSendError::InvalidFrame("message.type".into()));
        }
        self.try_enqueue(
            FramePriority::Business,
            WirePayload::Plain(text),
            Some(lease_context),
        )
    }

    pub(crate) fn stop(&self) {
        self.shutdown.cancel();
        self.clear_authenticated_lease();
    }

    pub(crate) fn is_stopped(&self) -> bool {
        self.shutdown.is_cancelled()
    }

    fn validate_control(&self, text: &str) -> Result<Value, ControllerSendError> {
        validate_controller_message_json(text, self.role.sender_role())
            .map_err(|error| ControllerSendError::InvalidFrame(error.field))
    }

    fn try_enqueue(
        &self,
        priority: FramePriority,
        payload: WirePayload,
        lease_context: Option<LeaseContext>,
    ) -> Result<(), ControllerSendError> {
        self.try_enqueue_with_receipt(priority, payload, lease_context, None, false)
    }

    fn try_enqueue_with_receipt(
        &self,
        priority: FramePriority,
        payload: WirePayload,
        lease_context: Option<LeaseContext>,
        flushed: Option<tokio::sync::oneshot::Sender<()>>,
        resume_only: bool,
    ) -> Result<(), ControllerSendError> {
        if self.shutdown.is_cancelled() {
            return Err(ControllerSendError::Closed);
        }
        let bytes = payload.as_str().len();
        let limit = match priority {
            FramePriority::Control => MAX_CONTROL_MESSAGE_BYTES,
            FramePriority::Business => MAX_RELAY_FRAME_BYTES,
        };
        if bytes > limit {
            return Err(ControllerSendError::FrameTooLarge { bytes, limit });
        }
        let permits = u32::try_from(bytes)
            .map_err(|_| ControllerSendError::FrameTooLarge { bytes, limit })?;
        let total_permit = self
            .total_budget
            .clone()
            .try_acquire_many_owned(permits)
            .map_err(|_| ControllerSendError::ByteBudgetExhausted)?;
        let business_permit = if priority == FramePriority::Business {
            Some(
                self.business_budget
                    .clone()
                    .try_acquire_many_owned(permits)
                    .map_err(|_| ControllerSendError::ByteBudgetExhausted)?,
            )
        } else {
            None
        };
        let frame = QueuedFrame {
            flushed,
            priority,
            transport_epoch: self.transport_epoch.load(Ordering::Acquire),
            lease_context,
            resume_only,
            payload,
            _total_permit: total_permit,
            _business_permit: business_permit,
        };
        let sender = match priority {
            FramePriority::Control => &self.control_tx,
            FramePriority::Business => &self.business_tx,
        };
        sender.try_send(frame).map_err(|error| match error {
            mpsc::error::TrySendError::Full(_) => ControllerSendError::QueueFull(priority),
            mpsc::error::TrySendError::Closed(_) => ControllerSendError::Closed,
        })
    }
}

pub(crate) struct ControllerTransportReceiver {
    control_rx: mpsc::Receiver<QueuedFrame>,
    business_rx: mpsc::Receiver<QueuedFrame>,
}

impl ControllerTransportReceiver {
    async fn recv_prioritized(&mut self) -> Option<QueuedFrame> {
        tokio::select! {
            biased;
            frame = self.control_rx.recv() => match frame {
                Some(frame) => Some(frame),
                None => self.business_rx.recv().await,
            },
            frame = self.business_rx.recv() => match frame {
                Some(frame) => Some(frame),
                None => self.control_rx.recv().await,
            },
        }
    }
}

/// 测试用便捷构造：新建一个独立 generation 单元格。生产调用必须走
/// `spawn_controller_transport`，否则 transport 任务与配置不会共享绑定结果。
#[cfg(test)]
pub(crate) fn transport_channels(
    role: ControllerRole,
    remote_control_generation: impl Into<String>,
    control_capacity: usize,
    business_capacity: usize,
    total_byte_budget: usize,
    control_reserved_bytes: usize,
) -> (ControllerTransportSender, ControllerTransportReceiver) {
    transport_channels_shared(
        role,
        Arc::new(RwLock::new(remote_control_generation.into())),
        control_capacity,
        business_capacity,
        total_byte_budget,
        control_reserved_bytes,
    )
}

fn transport_channels_shared(
    role: ControllerRole,
    remote_control_generation: Arc<RwLock<String>>,
    control_capacity: usize,
    business_capacity: usize,
    total_byte_budget: usize,
    control_reserved_bytes: usize,
) -> (ControllerTransportSender, ControllerTransportReceiver) {
    assert!(control_reserved_bytes < total_byte_budget);
    let (control_tx, control_rx) = mpsc::channel(control_capacity);
    let (business_tx, business_rx) = mpsc::channel(business_capacity);
    let total_budget = Arc::new(Semaphore::new(total_byte_budget));
    let business_budget = Arc::new(Semaphore::new(total_byte_budget - control_reserved_bytes));
    let shutdown = CancellationToken::new();
    (
        ControllerTransportSender {
            role,
            remote_control_generation,
            control_tx,
            business_tx,
            total_budget,
            business_budget,
            lease: Arc::new(RwLock::new(None)),
            resume_lease: Arc::new(RwLock::new(None)),
            resume_install: Arc::new(RwLock::new(None)),
            transport_epoch: Arc::new(AtomicU64::new(0)),
            shutdown,
        },
        ControllerTransportReceiver {
            control_rx,
            business_rx,
        },
    )
}

pub(crate) fn spawn_controller_transport(
    config: ControllerTransportConfig,
    sink: Arc<dyn ControllerEventSink>,
) -> ControllerTransportSender {
    let (sender, receiver) = transport_channels_shared(
        config.role,
        config.remote_control_generation.clone(),
        CONTROL_QUEUE_CAPACITY,
        BUSINESS_QUEUE_CAPACITY,
        TOTAL_OUTBOUND_BYTE_BUDGET,
        CONTROL_RESERVED_BYTES,
    );
    let task_sender = sender.clone();
    tauri::async_runtime::spawn(async move {
        run_transport(config, sink, task_sender, receiver).await;
    });
    sender
}

async fn run_transport(
    config: ControllerTransportConfig,
    sink: Arc<dyn ControllerEventSink>,
    sender: ControllerTransportSender,
    receiver: ControllerTransportReceiver,
) {
    run_transport_with_heartbeat(config, sink, sender, receiver, HEARTBEAT_INTERVAL).await;
}

async fn run_transport_with_heartbeat(
    config: ControllerTransportConfig,
    sink: Arc<dyn ControllerEventSink>,
    sender: ControllerTransportSender,
    mut receiver: ControllerTransportReceiver,
    heartbeat_period: Duration,
) {
    let mut liveness = ConnectedTransportLiveness::new(heartbeat_period);
    loop {
        let connection = async {
            let request = controller_websocket_request(&config.relay_url)?;
            connect_async_with_config(
                request,
                Some(relay_websocket_config(MAX_CONTROLLER_INBOUND_TEXT_BYTES)),
                false,
            )
            .await
        };
        tokio::pin!(connection);
        let connected = tokio::select! {
            biased;
            _ = sender.shutdown.cancelled() => return,
            result = &mut connection => result,
        };
        let (socket, _) = match connected {
            Ok(value) => value,
            Err(error) => {
                // Nothing was put on the wire. Keep queued pairing frames for
                // the next attempt instead of invalidating their epoch.
                sink.emit(ControllerTransportEvent::Disconnected {
                    remote_control_generation: config.generation(),
                    lease_epoch: current_lease_epoch(&sender),
                    error: format!("connect Relay: {error}"),
                });
                if !wait_reconnect(&sender.shutdown, &mut liveness.reconnect).await {
                    return;
                }
                continue;
            }
        };
        let (mut write, mut read) = socket.split();
        sink.emit(ControllerTransportEvent::Connected {
            remote_control_generation: config.generation(),
            lease_epoch: current_lease_epoch(&sender),
        });

        let disconnected = run_connected_transport(
            &config,
            sink.as_ref(),
            &sender,
            &mut receiver,
            &mut write,
            &mut read,
            &mut liveness,
        )
        .await;
        let Some(disconnected) = disconnected else {
            return;
        };
        emit_disconnected(&config, &sender, sink.as_ref(), disconnected);
        if !wait_reconnect(&sender.shutdown, &mut liveness.reconnect).await {
            return;
        }
    }
}

fn frame_is_task_submit(text: &str) -> bool {
    serde_json::from_str::<Value>(text)
        .ok()
        .is_some_and(|frame| {
            frame.get("type").and_then(Value::as_str) == Some("encrypted_frame")
                && frame.get("payload_kind").and_then(Value::as_str) == Some("task_submit")
        })
}

/// One absolute limit covers readiness, enqueue and flush; progress cannot
/// reset it. Cancellation drops the pending write rather than waiting on TCP.
async fn bounded_write<S>(
    write: &mut S,
    message: Message,
    shutdown: &CancellationToken,
    timeout: Duration,
) -> Result<(), String>
where
    S: Sink<Message> + Unpin,
    S::Error: fmt::Display,
{
    let deadline = tokio::time::Instant::now() + timeout;
    bounded_write_until(write, message, shutdown, deadline).await
}

async fn bounded_write_until<S>(
    write: &mut S,
    message: Message,
    shutdown: &CancellationToken,
    deadline: tokio::time::Instant,
) -> Result<(), String>
where
    S: Sink<Message> + Unpin,
    S::Error: fmt::Display,
{
    tokio::select! {
        biased;
        _ = shutdown.cancelled() => Err("controller_transport_stopped".into()),
        result = tokio::time::timeout_at(deadline, write.send(message)) => match result {
            Ok(Ok(())) => Ok(()),
            Ok(Err(error)) => Err(error.to_string()),
            Err(_) => Err("controller_transport_write_timeout".into()),
        },
    }
}

struct ConnectedTransportLiveness {
    heartbeat_period: Duration,
    reconnect: ReconnectBackoff,
}

impl ConnectedTransportLiveness {
    fn new(heartbeat_period: Duration) -> Self {
        Self {
            heartbeat_period,
            reconnect: ReconnectBackoff::default(),
        }
    }
}

/// Neither an in-flight write nor its progress may postpone inbound silence.
async fn bounded_connected_write<S>(
    write: &mut S,
    message: Message,
    shutdown: &CancellationToken,
    inbound_deadline: tokio::time::Instant,
) -> Result<(), String>
where
    S: Sink<Message> + Unpin,
    S::Error: fmt::Display,
{
    let write_deadline = tokio::time::Instant::now() + WRITE_TIMEOUT;
    let result = bounded_write_until(
        write,
        message,
        shutdown,
        write_deadline.min(inbound_deadline),
    )
    .await;
    if matches!(&result, Err(error) if error == "controller_transport_write_timeout")
        && inbound_deadline < write_deadline
    {
        return Err("Relay inbound silence deadline expired".to_owned());
    }
    result
}

async fn run_connected_transport<S, R, E>(
    config: &ControllerTransportConfig,
    sink: &dyn ControllerEventSink,
    sender: &ControllerTransportSender,
    receiver: &mut ControllerTransportReceiver,
    write: &mut S,
    read: &mut R,
    liveness: &mut ConnectedTransportLiveness,
) -> Option<String>
where
    S: Sink<Message> + Unpin,
    S::Error: fmt::Display,
    R: Stream<Item = Result<Message, E>> + Unpin,
    E: fmt::Display,
{
    let heartbeat_period = liveness.heartbeat_period;
    let mut last_inbound = tokio::time::Instant::now();
    let mut heartbeat = tokio::time::interval(heartbeat_period);
    heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    heartbeat.tick().await;
    let disconnected = loop {
        tokio::select! {
            biased;
            _ = sender.shutdown.cancelled() => {
                // Stop is authoritative. Close gets its own short budget,
                // never the already-cancelled token or an unbounded flush.
                let _ = bounded_write(write, Message::Close(None), &CancellationToken::new(), CLOSE_TIMEOUT).await;
                return None;
            }
            _ = tokio::time::sleep_until(last_inbound + heartbeat_period * 2) => {
                break "Relay inbound silence deadline expired".to_owned();
            }
            frame = receiver.recv_prioritized() => {
                let Some(frame) = frame else { return None; };
                if !frame_is_current(&sender, &frame) {
                    continue;
                }
                let result = bounded_connected_write(write, Message::Text(frame.text().to_owned().into()), &sender.shutdown, last_inbound + heartbeat_period * 2).await;
                if sender.is_stopped() { return None; }
                match result {
                    Ok(()) => {
                        if frame_is_task_submit(frame.text()) {
                            crate::features::remote_control::controller::relay_runtime::log_controller_event(
                                "task_submit_socket_written",
                                "sent",
                            );
                        }
                        if let Some(flushed) = frame.flushed {
                            let _ = flushed.send(());
                        }
                    }
                    Err(error) => {
                        if frame_is_task_submit(frame.text()) {
                            crate::features::remote_control::controller::relay_runtime::log_controller_event(
                                "task_submit_socket_write_failed",
                                "write_failed",
                            );
                        }
                        break format!("send Relay frame: {error}");
                    }
                }
            }
            message = read.next() => {
                let Some(message) = message else {
                    break "Relay connection closed".to_string();
                };
                last_inbound = tokio::time::Instant::now();
                match message {
                    Ok(Message::Text(text)) => {
                        if text.len() > MAX_CONTROLLER_INBOUND_TEXT_BYTES {
                            break format!(
                                "Relay frame exceeds {MAX_CONTROLLER_INBOUND_TEXT_BYTES} bytes"
                            );
                        }
                        // Only current positive controller admission resets backoff;
                        // transport pongs and bare handshakes are not admission.
                        if dispatch_inbound(config, sender, sink, text.as_str()) {
                            liveness.reconnect.reset();
                        }
                    }
                    Ok(Message::Ping(payload)) => {
                        if let Err(error) = bounded_connected_write(write, Message::Pong(payload), &sender.shutdown, last_inbound + heartbeat_period * 2).await {
                            if sender.is_stopped() { return None; }
                            break format!("send Relay pong: {error}");
                        }
                    }
                    Ok(Message::Pong(payload)) => {
                        if let Some(millis) = rtt_sample(config.clock.as_ref(), payload.as_ref()) {
                            sink.emit(ControllerTransportEvent::RttSample {
                                remote_control_generation: config.generation(),
                                millis,
                            });
                        }
                    }
                    Ok(Message::Close(_)) => break "Relay connection closed".to_string(),
                    Ok(_) => {}
                    Err(error) => break format!("read Relay frame: {error}"),
                }
            }
            _ = heartbeat.tick() => {
                let payload = config.clock.now_millis().to_be_bytes().to_vec();
                if let Err(error) = bounded_connected_write(write, Message::Ping(payload.into()), &sender.shutdown, last_inbound + heartbeat_period * 2).await {
                    if sender.is_stopped() { return None; }
                    break format!("send Relay heartbeat: {error}");
                }
            }
        }
    };
    Some(disconnected)
}

fn frame_is_current(sender: &ControllerTransportSender, frame: &QueuedFrame) -> bool {
    if frame.transport_epoch != sender.transport_epoch.load(Ordering::Acquire) {
        return false;
    }
    frame.lease_context.as_ref().is_none_or(|expected| {
        if frame.resume_only {
            let current = sender.lease.read();
            return current.is_none()
                && sender
                    .resume_lease
                    .read()
                    .as_ref()
                    .is_some_and(|actual| &actual.context == expected);
        }
        sender
            .lease
            .read()
            .as_ref()
            .is_some_and(|actual| &actual.context == expected)
    })
}

fn current_lease_epoch(sender: &ControllerTransportSender) -> Option<String> {
    sender
        .lease
        .read()
        .as_ref()
        .map(|lease| lease.context.lease_epoch.clone())
}

fn emit_disconnected(
    config: &ControllerTransportConfig,
    sender: &ControllerTransportSender,
    sink: &dyn ControllerEventSink,
    error: String,
) {
    let event = ControllerTransportEvent::Disconnected {
        remote_control_generation: config.generation(),
        lease_epoch: current_lease_epoch(sender),
        error,
    };
    sender.suspend_authenticated_lease();
    sink.emit(event);
}

async fn wait_reconnect(shutdown: &CancellationToken, backoff: &mut ReconnectBackoff) -> bool {
    let delay = backoff.next_delay();
    tokio::select! {
        biased;
        _ = shutdown.cancelled() => false,
        _ = tokio::time::sleep(delay) => true,
    }
}

fn dispatch_inbound(
    config: &ControllerTransportConfig,
    sender: &ControllerTransportSender,
    sink: &dyn ControllerEventSink,
    text: &str,
) -> bool {
    if sender.is_stopped() {
        return false;
    }
    let inbound_epoch = sender.transport_epoch.load(Ordering::Acquire);
    let Ok(shape) = serde_json::from_str::<Value>(text) else {
        return false;
    };
    if !generation_admits(&config.generation(), &shape) {
        return false;
    }
    let kind = shape.get("type").and_then(Value::as_str);
    let priority = if kind == Some("encrypted_frame") {
        FramePriority::Business
    } else {
        FramePriority::Control
    };
    let lease = sender.lease.read().clone();
    let resume_lease = if lease.is_none() && is_resume_inbound(kind) {
        sender.resume_lease.read().clone()
    } else {
        None
    };
    let validated = if priority == FramePriority::Business {
        let Some(lease) = lease.as_ref() else {
            return false;
        };
        let context = RuntimeValidationContext {
            lease: Some(lease.context.clone()),
            negotiated_capabilities: lease.negotiated_capabilities.clone(),
        };
        validate_controller_runtime_message_json(text, config.role.peer_role(), Some(&context))
            .is_ok()
    } else {
        validate_controller_message_json(text, SenderRole::Relay).is_ok()
            || validate_controller_message_json(text, config.role.peer_role()).is_ok()
    };
    let context_matches = inbound_context_matches(&shape, lease.as_ref())
        || (lease.is_none()
            && is_inbound_resume_frame(kind, config.role)
            && sender.resume_lease.read().as_ref().is_some_and(|saved| {
                super::resume::resume_frame_matches_lease(&saved.context, &shape)
            }));
    if !validated || !context_matches {
        return false;
    }
    if kind == Some("controller_resumed") && !sender.record_resume_install(&shape, inbound_epoch) {
        return false;
    }
    sink.emit(ControllerTransportEvent::Frame {
        priority,
        remote_control_generation: config.generation(),
        lease_epoch: lease
            .as_ref()
            .or(resume_lease.as_ref())
            .map(|value| value.context.lease_epoch.clone()),
        json: text.to_owned(),
    });
    kind != Some("error")
}

/// Test adapters exercise the real suspension and reader validation paths;
/// they never manufacture or directly install reader authority.
#[cfg(test)]
pub(super) fn disconnect_test_transport(sender: &ControllerTransportSender) {
    let config =
        ControllerTransportConfig::new("ws://127.0.0.1:1", sender.role, sender.generation());
    emit_disconnected(&config, sender, &|_| {}, "test socket closed".into());
}

#[cfg(test)]
pub(super) fn dispatch_test_transport_frame(sender: &ControllerTransportSender, frame: &Value) {
    let config =
        ControllerTransportConfig::new("ws://127.0.0.1:1", sender.role, sender.generation());
    dispatch_inbound(&config, sender, &|_| {}, &frame.to_string());
}

/// 未绑定 generation 的 transport（A 端 claim 之前）只按类型准入，绑定之后
/// 一律按 B 的 generation 硬门过滤；缺少 generation 字段的同生成期报文仍然
/// 允许通过，最终由各状态机自行校验绑定。
fn generation_admits(bound: &str, value: &Value) -> bool {
    bound.is_empty()
        || value
            .get("remote_control_generation")
            .and_then(Value::as_str)
            .is_none_or(|generation| generation == bound)
}

fn is_inbound_resume_frame(kind: Option<&str>, role: ControllerRole) -> bool {
    matches!(
        kind,
        Some(
            "controller_resume_provisional"
                | "encrypted_resume_confirm"
                | "controller_resume_activation_pending"
                | "controller_resumed"
        )
    ) || matches!(
        (kind, role),
        (
            Some("controller_post_activation_liveness_confirmed"),
            ControllerRole::ControllerA
        ) | (
            Some("controller_post_activation_liveness" | "controller_resume_ready_ack"),
            ControllerRole::ControlledB
        )
    )
}

fn inbound_context_matches(value: &Value, lease: Option<&AuthenticatedLease>) -> bool {
    let has_lease_context = value.get("controller_lease_id").is_some()
        || value.get("lease_epoch").is_some()
        || value.get("connection_id").is_some();
    if !has_lease_context || value.get("type").and_then(Value::as_str) == Some("controller_joined")
    {
        return true;
    }
    let Some(lease) = lease else {
        return false;
    };
    let context = &lease.context;
    if value.get("type").and_then(Value::as_str) == Some("encrypted_controller_liveness_a") {
        return [
            ("connection_id", context.connection_id.as_str()),
            ("controller_lease_id", context.controller_lease_id.as_str()),
            ("lease_epoch", context.lease_epoch.as_str()),
        ]
        .into_iter()
        .all(|(field, expected)| value.get(field).and_then(Value::as_str) == Some(expected));
    }
    [
        ("connection_id", context.connection_id.as_str()),
        ("controller_lease_id", context.controller_lease_id.as_str()),
        ("lease_epoch", context.lease_epoch.as_str()),
        ("relay_epoch", context.relay_epoch.as_str()),
        (
            "remote_control_generation",
            context.remote_control_generation.as_str(),
        ),
    ]
    .into_iter()
    .all(|(field, expected)| value.get(field).and_then(Value::as_str) == Some(expected))
}

fn rtt_sample(clock: &dyn TransportClock, payload: &[u8]) -> Option<u64> {
    let sent = u64::from_be_bytes(payload.try_into().ok()?);
    Some(clock.now_millis().saturating_sub(sent))
}

fn enforce_frame_limit(bytes: usize, limit: usize) -> Result<(), ControllerSendError> {
    if bytes > limit {
        Err(ControllerSendError::FrameTooLarge { bytes, limit })
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicU64, Ordering};

    use futures_util::{SinkExt, StreamExt};
    use serde_json::json;
    use tokio::net::TcpListener;
    use tokio_tungstenite::accept_hdr_async;
    use tokio_tungstenite::tungstenite::http::{HeaderValue, header::SEC_WEBSOCKET_PROTOCOL};

    use super::*;
    use crate::features::remote_control::controller::{
        controlled::ControlledDeviceManager, controlling::ControllingDeviceManager,
    };

    #[test]
    fn task_submit_write_telemetry_uses_encrypted_envelope_kind() {
        assert!(frame_is_task_submit(
            &json!({"type":"encrypted_frame", "payload_kind":"task_submit"}).to_string()
        ));
        for frame in [
            json!({"type":"encrypted_frame", "payload_kind":"task_status"}),
            json!({"type":"task_submit", "message_type":"task_submit"}),
            json!({"payload_kind":"task_submit"}),
        ] {
            assert!(!frame_is_task_submit(&frame.to_string()));
        }
        assert!(!frame_is_task_submit("not JSON"));
    }

    struct PendingSink {
        block_ready: bool,
        sent: usize,
    }

    struct ReceiptSink {
        flush_state: Arc<AtomicU64>,
        sent: Arc<AtomicU64>,
    }

    impl Sink<Message> for ReceiptSink {
        type Error = std::io::Error;

        fn poll_ready(
            self: std::pin::Pin<&mut Self>,
            _: &mut std::task::Context<'_>,
        ) -> std::task::Poll<Result<(), Self::Error>> {
            std::task::Poll::Ready(Ok(()))
        }
        fn start_send(self: std::pin::Pin<&mut Self>, _: Message) -> Result<(), Self::Error> {
            self.sent.fetch_add(1, Ordering::SeqCst);
            Ok(())
        }
        fn poll_flush(
            self: std::pin::Pin<&mut Self>,
            _: &mut std::task::Context<'_>,
        ) -> std::task::Poll<Result<(), Self::Error>> {
            match self.flush_state.load(Ordering::SeqCst) {
                1 => std::task::Poll::Ready(Ok(())),
                2 => std::task::Poll::Ready(Err(std::io::Error::other("test flush failure"))),
                _ => std::task::Poll::Pending,
            }
        }
        fn poll_close(
            self: std::pin::Pin<&mut Self>,
            _: &mut std::task::Context<'_>,
        ) -> std::task::Poll<Result<(), Self::Error>> {
            std::task::Poll::Ready(Ok(()))
        }
    }

    #[tokio::test(start_paused = true)]
    async fn flush_receipt_requires_success_and_is_never_sent_on_error_timeout_or_stop() {
        for case in ["success", "error", "timeout", "stop"] {
            let (sender, mut receiver) = transport_channels(
                ControllerRole::ControllerA,
                "generation-0001",
                2,
                1,
                8192,
                1024,
            );
            sender
                .install_authenticated_lease(lease("7", "generation-0001"))
                .unwrap();
            let mut receipt = sender
                .send_control_with_receipt(&json!({
                    "v":1, "type":"controller_liveness", "sender_role":"A",
                    "connection_id":"connection-0001", "controller_lease_id":"lease-0001",
                    "lease_epoch":"7", "relay_epoch":"relay-epoch-0001",
                    "remote_control_generation":"generation-0001",
                    "liveness_epoch":"liveness-0001", "heartbeat_sequence":"1",
                }))
                .unwrap();
            let flush_state = Arc::new(AtomicU64::new(0));
            let sent = Arc::new(AtomicU64::new(0));
            let mut write = ReceiptSink {
                flush_state: flush_state.clone(),
                sent: sent.clone(),
            };
            let mut read = futures_util::stream::pending::<Result<Message, std::io::Error>>();
            let config = ControllerTransportConfig::new(
                "ws://127.0.0.1:1",
                ControllerRole::ControllerA,
                "generation-0001",
            );
            let sink = |_: ControllerTransportEvent| {};
            let mut liveness = ConnectedTransportLiveness::new(Duration::from_secs(20));
            let connected = run_connected_transport(
                &config,
                &sink,
                &sender,
                &mut receiver,
                &mut write,
                &mut read,
                &mut liveness,
            );
            futures_util::pin_mut!(connected);
            assert!(futures_util::poll!(connected.as_mut()).is_pending());
            assert_eq!(sent.load(Ordering::SeqCst), 1);
            assert_eq!(
                receipt.try_recv(),
                Err(tokio::sync::oneshot::error::TryRecvError::Empty)
            );
            match case {
                "success" => {
                    flush_state.store(1, Ordering::SeqCst);
                    assert!(futures_util::poll!(connected.as_mut()).is_pending());
                    assert_eq!(receipt.try_recv(), Ok(()));
                    sender.stop();
                    assert!(connected.await.is_none());
                }
                "error" => {
                    flush_state.store(2, Ordering::SeqCst);
                    assert!(connected.await.unwrap().contains("test flush failure"));
                }
                "timeout" => {
                    tokio::time::advance(WRITE_TIMEOUT).await;
                    assert!(
                        connected
                            .await
                            .unwrap()
                            .contains("controller_transport_write_timeout")
                    );
                }
                _ => {
                    sender.stop();
                    assert!(connected.await.is_none());
                }
            }
            if case != "success" {
                assert_eq!(
                    receipt.try_recv(),
                    Err(tokio::sync::oneshot::error::TryRecvError::Closed),
                    "{case}"
                );
            }
            assert_eq!(sender.total_budget.available_permits(), 8192);
        }
    }

    impl Sink<Message> for PendingSink {
        type Error = std::io::Error;

        fn poll_ready(
            self: std::pin::Pin<&mut Self>,
            _: &mut std::task::Context<'_>,
        ) -> std::task::Poll<Result<(), Self::Error>> {
            if self.block_ready {
                std::task::Poll::Pending
            } else {
                std::task::Poll::Ready(Ok(()))
            }
        }
        fn start_send(mut self: std::pin::Pin<&mut Self>, _: Message) -> Result<(), Self::Error> {
            self.sent += 1;
            Ok(())
        }
        fn poll_flush(
            self: std::pin::Pin<&mut Self>,
            _: &mut std::task::Context<'_>,
        ) -> std::task::Poll<Result<(), Self::Error>> {
            std::task::Poll::Pending
        }
        fn poll_close(
            self: std::pin::Pin<&mut Self>,
            cx: &mut std::task::Context<'_>,
        ) -> std::task::Poll<Result<(), Self::Error>> {
            self.poll_flush(cx)
        }
    }

    #[tokio::test(start_paused = true)]
    async fn pending_ready_or_flush_cannot_wedge_frame_pong_or_heartbeat() {
        for route in ["frame", "pong", "heartbeat"] {
            for block_ready in [true, false] {
                let (sender, mut receiver) = transport_channels(
                    ControllerRole::ControllerA,
                    "generation-0001",
                    2,
                    2,
                    8192,
                    1024,
                );
                sender
                    .install_authenticated_lease(lease("7", "generation-0001"))
                    .unwrap();
                if route == "frame" {
                    sender
                        .try_send_control(&control("request-stalled"))
                        .unwrap();
                }
                let inbound = (route == "pong")
                    .then(|| Ok::<_, std::io::Error>(Message::Ping(vec![1].into())));
                let mut read =
                    futures_util::stream::iter(inbound).chain(futures_util::stream::pending());
                let mut write = PendingSink {
                    block_ready,
                    sent: 0,
                };
                let config = ControllerTransportConfig::new(
                    "wss://example.test/ws",
                    ControllerRole::ControllerA,
                    "generation-0001",
                );
                let before = tokio::time::Instant::now();
                // Isolate the inherited write budget; the combined production
                // silence/write minimum has its own regression below.
                let heartbeat_period = Duration::from_secs(20);
                let mut liveness = ConnectedTransportLiveness::new(heartbeat_period);
                let failure = tokio::time::timeout(
                    heartbeat_period + WRITE_TIMEOUT + Duration::from_secs(1),
                    run_connected_transport(
                        &config,
                        &|_| {},
                        &sender,
                        &mut receiver,
                        &mut write,
                        &mut read,
                        &mut liveness,
                    ),
                )
                .await
                .expect("connection loop must terminate")
                .expect("write timeout must disconnect");
                assert!(
                    failure.contains("controller_transport_write_timeout"),
                    "{route}: {failure}"
                );
                let expected = if route == "heartbeat" {
                    heartbeat_period + WRITE_TIMEOUT
                } else {
                    WRITE_TIMEOUT
                };
                assert_eq!(before.elapsed(), expected);
                assert_eq!(write.sent, usize::from(!block_ready));
                assert_eq!(sender.total_budget.available_permits(), 8192);
                emit_disconnected(&config, &sender, &|_| {}, failure);
                assert!(sender.authenticated_lease().is_none());
                assert!(sender.resume_lease.read().is_some());
            }
        }
    }

    #[tokio::test(start_paused = true)]
    async fn pending_writes_keep_the_earlier_absolute_inbound_silence_deadline() {
        for route in ["frame", "pong", "heartbeat"] {
            for block_ready in [true, false] {
                let (sender, mut receiver) = transport_channels(
                    ControllerRole::ControllerA,
                    "generation-0001",
                    2,
                    2,
                    8192,
                    1024,
                );
                sender
                    .install_authenticated_lease(lease("7", "generation-0001"))
                    .unwrap();
                if route == "frame" {
                    sender
                        .try_send_control(&control("request-stalled"))
                        .unwrap();
                }
                let inbound = (route == "pong")
                    .then(|| Ok::<_, std::io::Error>(Message::Ping(vec![1].into())));
                let mut read =
                    futures_util::stream::iter(inbound).chain(futures_util::stream::pending());
                let mut write = PendingSink {
                    block_ready,
                    sent: 0,
                };
                let config = ControllerTransportConfig::new(
                    "wss://example.test/ws",
                    ControllerRole::ControllerA,
                    "generation-0001",
                );
                let heartbeat_period = Duration::from_millis(100);
                let mut liveness = ConnectedTransportLiveness::new(heartbeat_period);
                let before = tokio::time::Instant::now();
                let failure = run_connected_transport(
                    &config,
                    &|_| {},
                    &sender,
                    &mut receiver,
                    &mut write,
                    &mut read,
                    &mut liveness,
                )
                .await
                .expect("silence must disconnect despite a pending write");
                assert!(
                    failure.contains("inbound silence deadline"),
                    "{route}: {failure}"
                );
                assert_eq!(before.elapsed(), heartbeat_period * 2);
                assert_eq!(write.sent, usize::from(!block_ready));
                assert_eq!(sender.total_budget.available_permits(), 8192);
                emit_disconnected(&config, &sender, &|_| {}, failure);
                assert!(sender.authenticated_lease().is_none());
                assert!(sender.resume_lease.read().is_some());
            }
        }
    }

    #[tokio::test(start_paused = true)]
    async fn stop_interrupts_each_inflight_write_and_bounds_idle_close() {
        for route in ["frame", "pong", "heartbeat", "close"] {
            let (sender, mut receiver) = transport_channels(
                ControllerRole::ControllerA,
                "generation-0001",
                2,
                2,
                8192,
                1024,
            );
            sender
                .install_authenticated_lease(lease("7", "generation-0001"))
                .unwrap();
            if route == "frame" {
                sender
                    .try_send_control(&control("request-stalled"))
                    .unwrap();
            }
            let inbound =
                (route == "pong").then(|| Ok::<_, std::io::Error>(Message::Ping(vec![1].into())));
            let mut read =
                futures_util::stream::iter(inbound).chain(futures_util::stream::pending());
            let mut write = PendingSink {
                block_ready: false,
                sent: 0,
            };
            let config = ControllerTransportConfig::new(
                "wss://example.test/ws",
                ControllerRole::ControllerA,
                "generation-0001",
            );
            let delay = if route == "heartbeat" {
                HEARTBEAT_INTERVAL + Duration::from_secs(1)
            } else {
                Duration::from_secs(1)
            };
            let before = tokio::time::Instant::now();
            let mut liveness = ConnectedTransportLiveness::new(HEARTBEAT_INTERVAL);
            let connected = run_connected_transport(
                &config,
                &|_| {},
                &sender,
                &mut receiver,
                &mut write,
                &mut read,
                &mut liveness,
            );
            let stopping = async {
                tokio::time::sleep(delay).await;
                sender.stop();
            };
            let (result, ()) =
                tokio::time::timeout(delay + CLOSE_TIMEOUT + Duration::from_secs(1), async {
                    tokio::join!(connected, stopping)
                })
                .await
                .expect("stop must not await an unbounded write");
            assert!(result.is_none(), "Stop must not authorize reconnect");
            assert!(sender.authenticated_lease().is_none());
            assert!(sender.resume_lease.read().is_none());
            assert_eq!(sender.total_budget.available_permits(), 8192);
            assert!(before.elapsed() <= delay + CLOSE_TIMEOUT);
            assert_eq!(write.sent, 1);
        }
    }

    #[test]
    fn controlling_resume_requires_frozen_gate_and_exact_reader_proof() {
        use crate::features::remote_control::controller::controlling::{
            ControllerLease, ResumeStep,
        };
        for case in [
            "valid",
            "expired",
            "activation",
            "token",
            "key",
            "no-reader",
            "request",
            "no-ready",
            "deadline-zero",
            "epoch-empty",
            "epoch-space",
            "epoch-tilde",
        ] {
            let manager = ControllingDeviceManager::default();
            let operation = manager.connect("234-4567").unwrap();
            manager.complete_reserved(operation.clone()).unwrap();
            let current = lease("7", "generation-0001");
            manager
                .complete_connected(
                    operation.clone(),
                    ControllerLease {
                        context: current.context.clone(),
                        peer_endpoint_id: "endpoint-0001".into(),
                        reservation_id: "reservation-0001".into(),
                        device_display_name: "B".into(),
                        liveness_epoch_a: "live-a-1".into(),
                        liveness_epoch_b: "live-b-1".into(),
                        negotiated_capabilities: current.negotiated_capabilities.clone(),
                        resume_deadline_b: 30_000,
                        resume_token: [1; 32],
                        transport_key_fingerprint: [2; 32],
                    },
                )
                .unwrap();
            let (sender, _receiver) = transport_channels(
                ControllerRole::ControllerA,
                "generation-0001",
                8,
                2,
                8192,
                1024,
            );
            sender.install_authenticated_lease(current.clone()).unwrap();
            manager.replace_transport(sender.clone());
            let config = ControllerTransportConfig::new(
                "wss://example.test/ws",
                ControllerRole::ControllerA,
                "generation-0001",
            );
            emit_disconnected(&config, &sender, &|_| {}, "lost".into());
            manager.mark_transport_disconnected(5_000, Some(30_000));
            manager.begin_resume(operation.clone(), 6_000).unwrap();
            for (at, step) in [
                (
                    7_000,
                    ResumeStep::Provisional {
                        activation_id: "activation-0001",
                    },
                ),
                (
                    8_000,
                    ResumeStep::Confirmed {
                        activation_id: "activation-0001",
                    },
                ),
                (
                    9_000,
                    ResumeStep::ActivationPending {
                        activation_id: "activation-0001",
                    },
                ),
                (
                    10_000,
                    ResumeStep::LivenessConfirmed {
                        activation_id: "activation-0001",
                    },
                ),
                (
                    11_000,
                    ResumeStep::ReadyAcknowledged {
                        activation_id: "activation-0001",
                    },
                ),
            ] {
                if case == "no-ready" && at == 11_000 {
                    continue;
                }
                manager.advance_resume(operation.clone(), step, at).unwrap();
            }
            if case != "no-reader" {
                let mut final_frame = resume_control("controller_resumed");
                final_frame["liveness_epoch_a"] = json!("live-a-2");
                final_frame["resume_deadline_ms"] = json!("60000");
                final_frame["new_resume_token_b64"] = json!(base64::Engine::encode(
                    &base64::engine::general_purpose::STANDARD,
                    [3; 32]
                ));
                dispatch_inbound(&config, &sender, &|_| {}, &final_frame.to_string());
            }
            let result = manager.complete_resumed(
                operation.clone(),
                if case == "activation" {
                    "activation-wrong"
                } else {
                    "activation-0001"
                },
                if case == "request" {
                    "request-wrong"
                } else {
                    "request-resume-0001"
                },
                if case == "expired" { 30_000 } else { 12_000 },
                (
                    match case {
                        "epoch-empty" => "",
                        "epoch-space" => "invalid epoch",
                        "epoch-tilde" => "live~a-2",
                        _ => "live-a-2",
                    },
                    "live-b-2",
                ),
                if case == "token" { [1; 32] } else { [3; 32] },
                if case == "key" { [2; 32] } else { [4; 32] },
                if case == "deadline-zero" { 0 } else { 60_000 },
            );
            if case == "valid" {
                assert_eq!(
                    serde_json::to_value(result.unwrap()).unwrap()["status"],
                    "connected"
                );
                assert_eq!(sender.authenticated_lease(), Some(current));
                assert!(
                    manager
                        .complete_resumed(
                            operation,
                            "activation-0001",
                            "request-resume-0001",
                            13_000,
                            ("live-a-2", "live-b-2"),
                            [3; 32],
                            [4; 32],
                            60_000
                        )
                        .is_err()
                );
            } else {
                assert_eq!(
                    result.unwrap_err().code,
                    "controller_resume_failed",
                    "{case}"
                );
                assert!(sender.is_stopped());
                assert!(sender.authenticated_lease().is_none());
                assert!(manager.live_lease().is_err());
            }
        }
    }

    #[test]
    fn controlled_resume_restores_only_the_exact_reader_authorized_lease() {
        use crate::features::remote_control::controller::controlled::{
            ControlledLease, ControlledRegistration, ControlledResumeStep, ControlledStatusKind,
        };
        for case in [
            "valid",
            "expired",
            "activation",
            "token",
            "key",
            "no-reader",
            "request",
            "no-ready",
            "deadline-zero",
            "epoch-empty",
            "epoch-space",
            "epoch-tilde",
            "stale-reader",
        ] {
            let manager = ControlledDeviceManager::default();
            let operation = manager.start(None, Vec::new()).unwrap();
            let status = serde_json::to_value(manager.status()).unwrap();
            let generation = status["generation"].as_str().unwrap().to_owned();
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
            manager.mark_pairing_reserved(operation.clone()).unwrap();
            manager.mark_handshake_pending(operation.clone()).unwrap();
            let current = lease("7", &generation);
            manager
                .complete_connected(
                    operation.clone(),
                    ControlledLease {
                        context: current.context.clone(),
                        negotiated_capabilities: current.negotiated_capabilities.clone(),
                        controller_device_name: "A".into(),
                        liveness_epoch_a: "live-a-1".into(),
                        resume_deadline_b: 30_000,
                        resume_token: [1; 32],
                        transport_key_fingerprint: [2; 32],
                        connected_at_b: 3_000,
                    },
                )
                .unwrap();
            let (sender, _receiver) =
                transport_channels(ControllerRole::ControlledB, &generation, 8, 2, 8192, 1024);
            sender.install_authenticated_lease(current.clone()).unwrap();
            manager
                .replace_authenticated_transport(operation.clone(), sender.clone())
                .unwrap();
            let config = ControllerTransportConfig::new(
                "wss://example.test/ws",
                ControllerRole::ControlledB,
                &generation,
            );
            emit_disconnected(&config, &sender, &|_| {}, "lost".into());
            manager.mark_transport_disconnected(5_000);
            manager.begin_resume(operation.clone(), 6_000).unwrap();
            for (at, step) in [
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
                if case == "no-ready" && at == 11_000 {
                    continue;
                }
                manager.advance_resume(operation.clone(), step, at).unwrap();
            }
            if case != "no-reader" {
                let mut final_frame = resume_control("controller_resumed");
                final_frame["remote_control_generation"] = json!(generation);
                final_frame["liveness_epoch_a"] = json!("live-a-2");
                final_frame["resume_deadline_ms"] = json!("60000");
                final_frame["new_resume_token_b64"] = json!(base64::Engine::encode(
                    &base64::engine::general_purpose::STANDARD,
                    [3; 32]
                ));
                dispatch_inbound(&config, &sender, &|_| {}, &final_frame.to_string());
            }
            if case == "stale-reader" {
                emit_disconnected(&config, &sender, &|_| {}, "lost-again".into());
            }
            let result = manager.complete_resumed(
                operation.clone(),
                if case == "activation" {
                    "activation-wrong"
                } else {
                    "activation-0001"
                },
                if case == "request" {
                    "request-wrong"
                } else {
                    "request-resume-0001"
                },
                if case == "expired" { 33_000 } else { 12_000 },
                match case {
                    "epoch-empty" => "",
                    "epoch-space" => "invalid epoch",
                    "epoch-tilde" => "live~a-2",
                    _ => "live-a-2",
                },
                if case == "token" { [1; 32] } else { [3; 32] },
                if case == "key" { [2; 32] } else { [4; 32] },
                if case == "deadline-zero" { 0 } else { 60_000 },
            );
            if case == "valid" {
                let resumed = serde_json::to_value(result.unwrap()).unwrap();
                assert_eq!(resumed["status"], "connected");
                assert!(resumed.get("watchdogDeadlineB").is_none());
                assert_eq!(sender.authenticated_lease(), Some(current));
                assert!(
                    manager
                        .complete_resumed(
                            operation,
                            "activation-0001",
                            "request-resume-0001",
                            13_000,
                            "live-a-2",
                            [3; 32],
                            [4; 32],
                            60_000
                        )
                        .is_err()
                );
                assert_eq!(
                    manager.poll_watchdog(41_999).unwrap().kind(),
                    ControlledStatusKind::Reconnecting
                );
                assert_eq!(
                    manager.poll_watchdog(42_000).unwrap().kind(),
                    ControlledStatusKind::Error
                );
            } else {
                assert_eq!(
                    result.unwrap_err().code,
                    "desktop_remote_resume_failed",
                    "{case}"
                );
                assert!(sender.is_stopped());
                assert!(sender.authenticated_lease().is_none());
                assert_eq!(
                    serde_json::to_value(manager.status()).unwrap()["status"],
                    "error"
                );
            }
        }
    }

    fn lease(epoch: &str, generation: &str) -> AuthenticatedLease {
        AuthenticatedLease {
            context: LeaseContext {
                connection_id: "connection-0001".into(),
                controller_lease_id: "lease-0001".into(),
                lease_epoch: epoch.into(),
                relay_epoch: "relay-epoch-0001".into(),
                remote_control_generation: generation.into(),
            },
            negotiated_capabilities: vec![
                "encrypted_frames_v1".into(),
                "role_liveness_v1".into(),
                "task_v1".into(),
            ],
        }
    }

    fn control(marker: &str) -> Value {
        json!({
            "v": 1,
            "type": "controller_capabilities",
            "request_id": marker,
            "client_role": "desktop_controller",
            "sender_role": "A",
            "protocol_version": 1,
            "capabilities": ["encrypted_frames_v1", "task_v1"]
        })
    }

    fn resume_final(context: &LeaseContext, activation_id: &str) -> Value {
        json!({
            "v": 1, "type": "controller_resumed", "request_id": "request-resume-final",
            "activation_id": activation_id,
            "connection_id": context.connection_id,
            "controller_lease_id": context.controller_lease_id,
            "lease_epoch": context.lease_epoch, "relay_epoch": context.relay_epoch,
            "remote_control_generation": context.remote_control_generation,
            "liveness_epoch_a": "live-a-2", "liveness_epoch_b": "live-b-2",
            "resume_deadline_ms": "60000",
            "new_resume_token_b64": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
        })
    }

    #[test]
    fn resume_install_is_activation_bound_one_shot_and_closed_by_next_socket_epoch() {
        for lose_socket in [false, true] {
            let expected = lease("1", "generation-0001");
            let (sender, _) = transport_channels(
                ControllerRole::ControllerA,
                "generation-0001",
                2,
                2,
                8192,
                1024,
            );
            sender
                .install_authenticated_lease(expected.clone())
                .unwrap();
            sender.suspend_authenticated_lease();
            let config = ControllerTransportConfig::new(
                "ws://127.0.0.1:1",
                ControllerRole::ControllerA,
                "generation-0001",
            );
            let events = Arc::new(std::sync::Mutex::new(Vec::new()));
            let capture = events.clone();
            let sink = move |event| capture.lock().unwrap().push(event);
            validate_controller_message_json(
                &resume_final(&expected.context, "activation-0001").to_string(),
                SenderRole::Relay,
            )
            .unwrap();
            dispatch_inbound(
                &config,
                &sender,
                &sink,
                &resume_final(&expected.context, "activation-0001").to_string(),
            );
            assert_eq!(
                events.lock().unwrap().len(),
                1,
                "final must really pass RX admission"
            );
            assert_eq!(
                sender.install_authenticated_lease_after_resume(
                    expected.clone(),
                    "activation-other",
                    "request-resume-final"
                ),
                Err(ControllerSendError::Unauthenticated)
            );
            if lose_socket {
                emit_disconnected(&config, &sender, &sink, "closed".into());
                assert_eq!(
                    sender.install_authenticated_lease_after_resume(
                        expected,
                        "activation-0001",
                        "request-resume-final"
                    ),
                    Err(ControllerSendError::Unauthenticated)
                );
                assert!(sender.authenticated_lease().is_none());
            } else {
                sender
                    .install_authenticated_lease_after_resume(
                        expected.clone(),
                        "activation-0001",
                        "request-resume-final",
                    )
                    .unwrap();
                assert!(sender.authenticated_lease().is_some());
                assert_eq!(
                    sender.install_authenticated_lease_after_resume(
                        expected,
                        "activation-0001",
                        "request-resume-final"
                    ),
                    Err(ControllerSendError::Unauthenticated)
                );
            }
        }
    }

    #[test]
    fn verified_resume_reinstalls_real_transport_only_at_final_gate_and_fails_closed() {
        use crate::features::remote_control::controller::controlling::{
            ControllerLease, ControllingStatusKind, ResumeStep,
        };
        for (generation, stopped, close_after_final) in [
            ("generation-0001", false, false),
            ("generation-replaced", false, false),
            ("generation-0001", true, false),
            ("generation-0001", false, true),
        ] {
            let manager = ControllingDeviceManager::default();
            let operation = manager.connect("234-4567").unwrap();
            manager.complete_reserved(operation.clone()).unwrap();
            let expected = lease("1", "generation-0001");
            manager
                .complete_connected(
                    operation.clone(),
                    ControllerLease {
                        context: expected.context.clone(),
                        peer_endpoint_id: "endpoint-0001".into(),
                        reservation_id: "reservation-0001".into(),
                        device_display_name: "remote-device".into(),
                        liveness_epoch_a: "live-a-1".into(),
                        liveness_epoch_b: "live-b-1".into(),
                        resume_deadline_b: 30_000,
                        resume_token: [1; 32],
                        transport_key_fingerprint: [2; 32],
                        negotiated_capabilities: expected.negotiated_capabilities.clone(),
                    },
                )
                .unwrap();
            let (sender, mut receiver) =
                transport_channels(ControllerRole::ControllerA, generation, 2, 2, 8192, 1024);
            if generation == "generation-0001" {
                sender
                    .install_authenticated_lease(expected.clone())
                    .unwrap();
            }
            manager.replace_transport(sender.clone());
            sender.suspend_authenticated_lease();
            if stopped {
                sender.stop();
            }
            manager.mark_transport_disconnected(1_000, Some(30_000));
            manager.begin_resume(operation.clone(), 2_000).unwrap();
            let activation = "activation-0001";
            let frame = json!({
                "v":1, "type":"encrypted_frame", "connection_id":"connection-0001",
                "controller_lease_id":"lease-0001", "lease_epoch":"1", "relay_epoch":"relay-epoch-0001",
                "remote_control_generation":"generation-0001", "frame_sequence":"1", "direction":"a_to_b",
                "payload_kind":"task_submit", "ciphertext_b64":"AQID", "ciphertext_bytes":"3"
            });
            for (step, at) in [
                (
                    ResumeStep::Provisional {
                        activation_id: activation,
                    },
                    3_000,
                ),
                (
                    ResumeStep::Confirmed {
                        activation_id: activation,
                    },
                    4_000,
                ),
                (
                    ResumeStep::ActivationPending {
                        activation_id: activation,
                    },
                    5_000,
                ),
                (
                    ResumeStep::LivenessConfirmed {
                        activation_id: activation,
                    },
                    6_000,
                ),
                (
                    ResumeStep::ReadyAcknowledged {
                        activation_id: activation,
                    },
                    7_000,
                ),
            ] {
                manager.advance_resume(operation.clone(), step, at).unwrap();
                assert!(sender.authenticated_lease().is_none());
                assert_eq!(
                    sender.try_send_business(&frame),
                    Err(ControllerSendError::Unauthenticated)
                );
                assert!(receiver.business_rx.try_recv().is_err());
            }
            let config = ControllerTransportConfig::new(
                "ws://127.0.0.1:1",
                ControllerRole::ControllerA,
                generation,
            );
            dispatch_inbound(
                &config,
                &sender,
                &|_| {},
                &resume_final(&expected.context, activation).to_string(),
            );
            if close_after_final {
                // Final Frame was already delivered, but the socket closed
                // before the manager consumed it from its event queue.
                emit_disconnected(&config, &sender, &|_| {}, "closed".into());
            }
            let result = manager.complete_resumed(
                operation,
                activation,
                "request-resume-final",
                8_000,
                ("live-a-2", "live-b-2"),
                [3; 32],
                [4; 32],
                60_000,
            );
            if generation != "generation-0001" || stopped || close_after_final {
                assert!(result.is_err());
                assert_eq!(manager.status().kind(), ControllingStatusKind::Error);
                assert!(manager.live_lease().is_err());
                assert!(sender.authenticated_lease().is_none());
                assert!(sender.is_stopped());
                continue;
            }
            result.unwrap();
            assert_eq!(
                sender.authenticated_lease().unwrap().context,
                expected.context
            );
            sender.try_send_business(&frame).unwrap();
            let queued = receiver.business_rx.try_recv().unwrap();
            assert_eq!(
                serde_json::from_str::<Value>(queued.payload.as_str()).unwrap(),
                frame
            );
            assert_eq!(queued.lease_context, Some(expected.context));
            sender.try_send_business(&frame).unwrap();
            sender.try_send_business(&frame).unwrap();
            assert_eq!(
                sender.try_send_business(&frame),
                Err(ControllerSendError::QueueFull(FramePriority::Business))
            );
        }
    }

    #[test]
    fn queue_full_releases_byte_budget_and_cancel_is_terminal() {
        let (sender, _receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            1,
            1,
            4096,
            512,
        );
        sender.try_send_control(&control("request-0001")).unwrap();
        let permits = sender.total_budget.available_permits();
        assert_eq!(
            sender.try_send_control(&control("request-0002")),
            Err(ControllerSendError::QueueFull(FramePriority::Control))
        );
        assert_eq!(sender.total_budget.available_permits(), permits);
        sender.stop();
        assert!(sender.is_stopped());
        assert_eq!(
            sender.try_send_control(&control("request-0003")),
            Err(ControllerSendError::Closed)
        );
    }

    #[test]
    fn controller_and_controlled_state_owners_stop_independently() {
        let controlled = ControlledDeviceManager::default();
        let controlling = ControllingDeviceManager::default();
        let (controlled_transport, _controlled_receiver) =
            transport_channels(ControllerRole::ControlledB, "generation-b", 1, 1, 4096, 512);
        let (controlling_transport, _controlling_receiver) =
            transport_channels(ControllerRole::ControllerA, "generation-a", 1, 1, 4096, 512);
        controlled.replace_transport(controlled_transport);
        controlling.replace_transport(controlling_transport);
        assert!(controlled.is_running());
        assert!(controlling.is_running());

        controlled.stop();
        assert!(!controlled.is_running());
        assert!(controlling.is_running());
        controlling.stop();
        assert!(!controlling.is_running());
    }

    #[test]
    fn sensitive_frame_has_a_hard_limit_and_redacted_debug_surface() {
        let (sender, mut receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            1,
            1,
            MAX_CONTROL_MESSAGE_BYTES * 2,
            1024,
        );
        let oversized = json!({
            "v": 1,
            "type": "controller_capabilities",
            "request_id": "request-0001",
            "client_role": "desktop_controller",
            "sender_role": "A",
            "protocol_version": 1,
            "capabilities": ["encrypted_frames_v1"],
            "padding": "x".repeat(MAX_CONTROL_MESSAGE_BYTES)
        });
        assert!(matches!(
            sender.try_send_sensitive_control(&oversized),
            Err(ControllerSendError::FrameTooLarge { .. })
        ));

        sender
            .try_send_sensitive_control(&control("secret-request"))
            .unwrap();
        let frame = receiver.control_rx.try_recv().unwrap();
        let debug = format!("{frame:?}");
        assert!(debug.contains("[REDACTED]"));
        assert!(!debug.contains("secret-request"));
    }

    #[tokio::test]
    async fn control_queue_wins_when_both_priorities_are_ready() {
        let (sender, mut receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            2,
            2,
            8192,
            1024,
        );
        sender
            .install_authenticated_lease(lease("7", "generation-0001"))
            .unwrap();
        let business = json!({
            "v": 1,
            "type": "encrypted_frame",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "7",
            "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": "generation-0001",
            "frame_sequence": "1",
            "direction": "a_to_b",
            "payload_kind": "task_submit",
            "ciphertext_b64": "AQID",
            "ciphertext_bytes": "3"
        });
        sender.try_send_business(&business).unwrap();
        sender.try_send_control(&control("request-0001")).unwrap();

        assert_eq!(
            receiver.recv_prioritized().await.unwrap().priority,
            FramePriority::Control
        );
        assert_eq!(
            receiver.recv_prioritized().await.unwrap().priority,
            FramePriority::Business
        );
    }

    #[test]
    fn business_frames_require_an_authenticated_current_lease() {
        let (sender, _receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            1,
            1,
            8192,
            1024,
        );
        assert_eq!(
            sender.try_send_business(&control("request-0001")),
            Err(ControllerSendError::Unauthenticated)
        );
        sender
            .install_authenticated_lease(lease("7", "generation-0001"))
            .unwrap();
        assert!(matches!(
            sender.try_send_business(&control("request-0001")),
            Err(ControllerSendError::InvalidFrame(_))
        ));
    }

    #[test]
    fn lease_install_is_bound_to_the_transport_generation() {
        let (sender, _receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-new",
            1,
            1,
            8192,
            1024,
        );
        assert_eq!(
            sender.install_authenticated_lease(lease("7", "generation-old")),
            Err(ControllerSendError::InvalidFrame(
                "remote_control_generation".into()
            ))
        );
        assert!(sender.authenticated_lease().is_none());
    }

    /// E2E-02 第 1 步：A 端在验签 transcript 之前不知道 B 的 generation，
    /// 因此未绑定的 transport 只能靠受控 `bind_generation` 一次性补齐；
    /// 空值/非法 id/重复绑定/已安装租约都必须失败关闭。
    #[test]
    fn generation_binding_is_controlled_and_one_shot() {
        let (sender, _receiver) = transport_channels_shared(
            ControllerRole::ControllerA,
            Arc::new(RwLock::new(String::new())),
            1,
            1,
            8192,
            1024,
        );
        assert_eq!(sender.generation(), "");
        // 未绑定前不得安装租约。
        assert_eq!(
            sender.install_authenticated_lease(lease("7", "generation-0001")),
            Err(ControllerSendError::InvalidFrame(
                "remote_control_generation".into()
            ))
        );
        for invalid in ["", " leading-space", "generation with space", "-dash-first"] {
            assert_eq!(
                sender.bind_generation(invalid),
                Err(ControllerSendError::InvalidFrame(
                    "remote_control_generation".into()
                )),
                "{invalid:?} must fail closed"
            );
        }
        sender.bind_generation("generation-0001").unwrap();
        assert_eq!(sender.generation(), "generation-0001");
        // 只允许绑定一次：重复绑定（即使同值）也必须拒绝。
        assert_eq!(
            sender.bind_generation("generation-0001"),
            Err(ControllerSendError::GenerationAlreadyBound)
        );
        assert_eq!(
            sender.bind_generation("generation-0002"),
            Err(ControllerSendError::GenerationAlreadyBound)
        );
        assert_eq!(sender.generation(), "generation-0001");

        // 绑定后可以用同一 generation 安装租约，旧 generation 仍被拒绝。
        sender
            .install_authenticated_lease(lease("7", "generation-0001"))
            .unwrap();
        assert!(sender.authenticated_lease().is_some());
        assert_eq!(
            sender.bind_generation("generation-0003"),
            Err(ControllerSendError::GenerationAlreadyBound)
        );
        assert_eq!(
            sender.install_authenticated_lease(lease("7", "generation-old")),
            Err(ControllerSendError::InvalidFrame(
                "remote_control_generation".into()
            ))
        );
        assert_eq!(
            sender
                .authenticated_lease()
                .map(|value| value.context.remote_control_generation),
            Some("generation-0001".to_owned())
        );
    }

    /// 共享单元格：sender 的绑定立刻对 transport 任务可见（同一 Arc）。
    #[test]
    fn generation_binding_is_shared_with_the_transport_task() {
        let config = ControllerTransportConfig::new(
            "wss://example.test/ws",
            ControllerRole::ControllerA,
            String::new(),
        );
        let (sender, _receiver) = transport_channels_shared(
            config.role,
            config.remote_control_generation.clone(),
            1,
            1,
            8192,
            1024,
        );
        assert_eq!(config.generation(), "");
        sender.bind_generation("generation-0001").unwrap();
        assert_eq!(config.generation(), "generation-0001");
    }

    /// 未绑定 generation 的 transport 只按类型准入（否则 A 端连
    /// `controller_pairing_reserved` 都收不到）；绑定后一律硬门过滤。
    #[test]
    fn unbound_transport_admits_frames_then_filters_by_bound_generation() {
        let frame = json!({
            "v": 1,
            "type": "controller_joined",
            "remote_control_generation": "generation-0001"
        });
        assert!(generation_admits("", &frame));
        assert!(generation_admits("generation-0001", &frame));
        assert!(!generation_admits("generation-0002", &frame));
        // 不带 generation 的同生成期报文仍由各状态机自行校验绑定。
        assert!(generation_admits(
            "generation-0001",
            &json!({ "v": 2, "type": "desktop_endpoint_registered" })
        ));
    }

    #[test]
    fn lease_scoped_control_cannot_use_the_preauthenticated_queue() {
        let (sender, mut receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            2,
            1,
            8192,
            1024,
        );
        let liveness = json!({
            "v": 1,
            "type": "controller_liveness",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "7",
            "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": "generation-0001",
            "sender_role": "A",
            "liveness_epoch": "liveness-0001",
            "heartbeat_sequence": "1"
        });
        assert_eq!(
            sender.try_send_control(&liveness),
            Err(ControllerSendError::Unauthenticated)
        );
        sender
            .install_authenticated_lease(lease("7", "generation-0001"))
            .unwrap();
        sender.try_send_authenticated_control(&liveness).unwrap();
        assert_eq!(
            receiver.control_rx.try_recv().unwrap().priority,
            FramePriority::Control
        );
    }

    #[test]
    fn disconnect_invalidates_queued_lease_frames_before_reconnect() {
        let (sender, mut receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            2,
            2,
            8192,
            1024,
        );
        sender
            .install_authenticated_lease(lease("7", "generation-0001"))
            .unwrap();
        let frame = json!({
            "v": 1,
            "type": "encrypted_frame",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "7",
            "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": "generation-0001",
            "frame_sequence": "1",
            "direction": "a_to_b",
            "payload_kind": "task_submit",
            "ciphertext_b64": "AQID",
            "ciphertext_bytes": "3"
        });
        sender.try_send_business(&frame).unwrap();
        let queued = receiver.business_rx.try_recv().unwrap();
        assert!(frame_is_current(&sender, &queued));
        let config = ControllerTransportConfig::new(
            "wss://example.test/ws",
            ControllerRole::ControllerA,
            "generation-0001",
        );
        let events = Arc::new(parking_lot::Mutex::new(Vec::new()));
        let sink_events = events.clone();
        emit_disconnected(
            &config,
            &sender,
            &move |event| sink_events.lock().push(event),
            "lost".into(),
        );
        assert!(sender.authenticated_lease().is_none());
        assert!(!frame_is_current(&sender, &queued));
        sender
            .install_authenticated_lease(lease("8", "generation-0001"))
            .unwrap();
        assert!(!frame_is_current(&sender, &queued));
        assert!(matches!(
            &events.lock()[0],
            ControllerTransportEvent::Disconnected { lease_epoch: Some(epoch), .. }
                if epoch == "7"
        ));
    }

    #[test]
    fn business_bytes_cannot_consume_the_reserved_control_budget() {
        let (sender, mut receiver) = transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            2,
            2,
            1024,
            256,
        );
        let payload = WirePayload::Plain("b".repeat(769));
        assert_eq!(
            sender.try_enqueue(FramePriority::Business, payload, None),
            Err(ControllerSendError::ByteBudgetExhausted)
        );
        sender.try_send_control(&control("request-0001")).unwrap();
        assert_eq!(
            receiver.control_rx.try_recv().unwrap().priority,
            FramePriority::Control
        );
    }

    #[test]
    fn stale_generation_or_lease_epoch_is_rejected_before_dispatch() {
        let current = lease("7", "generation-new");
        let mut stale_generation = json!({
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "7",
            "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": "generation-old"
        });
        assert!(!inbound_context_matches(&stale_generation, Some(&current)));
        stale_generation["remote_control_generation"] = json!("generation-new");
        stale_generation["lease_epoch"] = json!("6");
        assert!(!inbound_context_matches(&stale_generation, Some(&current)));
        stale_generation["lease_epoch"] = json!("7");
        assert!(inbound_context_matches(&stale_generation, Some(&current)));
    }

    #[test]
    fn direct_encrypted_liveness_uses_its_frozen_three_field_context() {
        let (sender, _receiver) = transport_channels(
            ControllerRole::ControlledB,
            "generation-0001",
            8,
            2,
            8192,
            1024,
        );
        sender
            .install_authenticated_lease(lease("7", "generation-0001"))
            .unwrap();
        let config = ControllerTransportConfig::new(
            "wss://example.test/ws",
            ControllerRole::ControlledB,
            "generation-0001",
        );
        let events = Arc::new(parking_lot::Mutex::new(Vec::new()));
        let sink_events = events.clone();
        let sink = move |event| sink_events.lock().push(event);
        let frame = json!({
            "v": 1, "type": "encrypted_controller_liveness_a",
            "connection_id": "connection-0001", "controller_lease_id": "lease-0001",
            "lease_epoch": "7", "ciphertext_b64": "AQID", "ciphertext_bytes": "3"
        });
        dispatch_inbound(&config, &sender, &sink, &frame.to_string());
        assert_eq!(events.lock().len(), 1);
        for field in ["connection_id", "controller_lease_id", "lease_epoch"] {
            let mut wrong = frame.clone();
            wrong[field] = json!("wrong-0001");
            dispatch_inbound(&config, &sender, &sink, &wrong.to_string());
        }
        assert_eq!(events.lock().len(), 1);
        let mut extra = frame.clone();
        extra["relay_epoch"] = json!("relay-epoch-0001");
        dispatch_inbound(&config, &sender, &sink, &extra.to_string());
        assert_eq!(events.lock().len(), 1);
        let full = resume_control("controller_resume_provisional");
        for field in [
            "connection_id",
            "controller_lease_id",
            "lease_epoch",
            "relay_epoch",
            "remote_control_generation",
        ] {
            let mut incomplete = full.clone();
            incomplete.as_object_mut().unwrap().remove(field);
            dispatch_inbound(&config, &sender, &sink, &incomplete.to_string());
        }
        assert_eq!(events.lock().len(), 1);
    }

    fn resume_control(kind: &str) -> Value {
        let mut frame = json!({
            "v": 1, "type": kind, "request_id": "request-resume-0001",
            "connection_id": "connection-0001", "controller_lease_id": "lease-0001",
            "lease_epoch": "7", "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": "generation-0001", "activation_id": "activation-0001"
        });
        match kind {
            "controller_resume_activation_ack"
            | "controller_resume_provisional"
            | "controller_resume_activation_pending" => {
                frame["original_deadline_ms"] = json!("31000");
            }
            "controller_post_activation_liveness"
            | "controller_post_activation_liveness_confirmed"
            | "controller_resume_ready_ack" => {
                frame["liveness_epoch_b"] = json!("liveness-b-new-0001");
                frame["heartbeat_sequence_b"] = json!("1");
                if kind == "controller_resume_ready_ack" {
                    frame["sender_role"] = json!("B");
                }
            }
            "encrypted_resume_confirm" => {
                frame["sender_role"] = json!("A");
                frame["ciphertext_b64"] = json!("AQID");
                frame["ciphertext_bytes"] = json!("3");
            }
            "controller_resumed" => {
                frame["liveness_epoch_a"] = json!("liveness-a-new-0001");
                frame["liveness_epoch_b"] = json!("liveness-b-new-0001");
                frame["new_resume_token_b64"] =
                    json!("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=");
                frame["resume_deadline_ms"] = json!("35000");
            }
            _ => panic!("test frame kind"),
        }
        frame
    }

    #[test]
    fn disconnected_transport_admits_only_context_bound_resume_controls() {
        let (sender, mut receiver) = transport_channels(
            ControllerRole::ControlledB,
            "generation-0001",
            8,
            2,
            8192,
            1024,
        );
        let config = ControllerTransportConfig::new(
            "wss://example.test/ws",
            ControllerRole::ControlledB,
            "generation-0001",
        );
        let ack = resume_control("controller_resume_activation_ack");
        assert!(sender.try_send_sensitive_resume_control(&ack).is_err());
        sender
            .install_authenticated_lease(lease("7", "generation-0001"))
            .unwrap();
        emit_disconnected(&config, &sender, &|_| {}, "lost".into());
        emit_disconnected(&config, &sender, &|_| {}, "retry-lost".into());
        assert!(sender.authenticated_lease().is_none());
        for kind in [
            "controller_resume_activation_ack",
            "controller_post_activation_liveness",
            "controller_resume_ready_ack",
        ] {
            sender
                .try_send_sensitive_resume_control(&resume_control(kind))
                .unwrap();
            let queued = receiver.control_rx.try_recv().unwrap();
            assert!(frame_is_current(&sender, &queued));
            assert!(format!("{queued:?}").contains("[REDACTED]"));
        }
        let events = Arc::new(parking_lot::Mutex::new(Vec::new()));
        let sink_events = events.clone();
        let sink = move |event| sink_events.lock().push(event);
        for kind in [
            "controller_resume_provisional",
            "controller_resume_activation_pending",
            "controller_resume_ready_ack",
            "encrypted_resume_confirm",
            "controller_resumed",
        ] {
            let mut incoming = resume_control(kind);
            if kind == "controller_resume_ready_ack" {
                incoming["sender_role"] = json!("A");
            }
            dispatch_inbound(&config, &sender, &sink, &incoming.to_string());
        }
        assert_eq!(events.lock().len(), 5);
        dispatch_inbound(
            &config,
            &sender,
            &sink,
            &resume_control("controller_post_activation_liveness_confirmed").to_string(),
        );
        assert_eq!(
            events.lock().len(),
            5,
            "A-only liveness confirmation is rejected by B"
        );
        let mut wrong = ack.clone();
        wrong["lease_epoch"] = json!("6");
        assert!(sender.try_send_sensitive_resume_control(&wrong).is_err());
        let mut wrong_inbound = resume_control("controller_resume_provisional");
        wrong_inbound["controller_lease_id"] = json!("lease-wrong-0001");
        dispatch_inbound(&config, &sender, &sink, &wrong_inbound.to_string());
        assert_eq!(events.lock().len(), 5);
        assert_eq!(
            sender.try_send_business(&control("request-business")),
            Err(ControllerSendError::Unauthenticated)
        );
        assert!(sender.try_send_authenticated_control(&ack).is_err());
        sender.try_send_sensitive_resume_control(&ack).unwrap();
        let queued = receiver.control_rx.try_recv().unwrap();
        sender
            .install_authenticated_lease(lease("7", "generation-0001"))
            .unwrap();
        assert!(!frame_is_current(&sender, &queued));
        assert!(sender.try_send_sensitive_resume_control(&ack).is_err());
        sender.stop();
        dispatch_inbound(
            &config,
            &sender,
            &sink,
            &resume_control("controller_resumed").to_string(),
        );
        assert_eq!(events.lock().len(), 5);
    }

    #[test]
    fn resume_final_authority_is_one_shot_and_bound_to_socket_epoch_and_request() {
        let (sender, _receiver) = transport_channels(
            ControllerRole::ControlledB,
            "generation-0001",
            8,
            2,
            8192,
            1024,
        );
        let config = ControllerTransportConfig::new(
            "wss://example.test/ws",
            ControllerRole::ControlledB,
            "generation-0001",
        );
        let current = lease("7", "generation-0001");
        let final_frame = resume_control("controller_resumed");
        sender.install_authenticated_lease(current.clone()).unwrap();
        emit_disconnected(&config, &sender, &|_| {}, "lost".into());
        dispatch_inbound(&config, &sender, &|_| {}, &final_frame.to_string());
        assert!(
            sender
                .install_authenticated_lease_after_resume(
                    current.clone(),
                    "wrong-activation",
                    "request-resume-0001"
                )
                .is_err()
        );
        assert!(
            sender
                .install_authenticated_lease_after_resume(
                    current.clone(),
                    "activation-0001",
                    "wrong-request"
                )
                .is_err()
        );
        emit_disconnected(&config, &sender, &|_| {}, "lost-again".into());
        assert!(
            sender
                .install_authenticated_lease_after_resume(
                    current.clone(),
                    "activation-0001",
                    "request-resume-0001"
                )
                .is_err()
        );
        assert!(!sender.record_resume_install(&final_frame, 1));
        dispatch_inbound(&config, &sender, &|_| {}, &final_frame.to_string());
        sender
            .install_authenticated_lease_after_resume(
                current.clone(),
                "activation-0001",
                "request-resume-0001",
            )
            .unwrap();
        assert!(
            sender
                .install_authenticated_lease_after_resume(
                    current.clone(),
                    "activation-0001",
                    "request-resume-0001"
                )
                .is_err()
        );
        assert_eq!(sender.authenticated_lease(), Some(current.clone()));
        emit_disconnected(&config, &sender, &|_| {}, "third-loss".into());
        dispatch_inbound(&config, &sender, &|_| {}, &final_frame.to_string());
        sender.clear_authenticated_lease();
        assert!(
            sender
                .install_authenticated_lease_after_resume(
                    current.clone(),
                    "activation-0001",
                    "request-resume-0001"
                )
                .is_err()
        );
        sender.stop();
        assert_eq!(
            sender.install_authenticated_lease_after_resume(
                current,
                "activation-0001",
                "request-resume-0001"
            ),
            Err(ControllerSendError::Closed)
        );
    }

    struct FakeClock(AtomicU64);

    impl TransportClock for FakeClock {
        fn now_millis(&self) -> u64 {
            self.0.load(Ordering::Relaxed)
        }
    }

    #[test]
    fn rtt_sampling_uses_injected_monotonic_clock() {
        let clock = FakeClock(AtomicU64::new(1_250));
        assert_eq!(rtt_sample(&clock, &1_000_u64.to_be_bytes()), Some(250));
        clock.0.store(900, Ordering::Relaxed);
        assert_eq!(rtt_sample(&clock, &1_000_u64.to_be_bytes()), Some(0));
        assert_eq!(rtt_sample(&clock, &[1, 2, 3]), None);
    }

    #[tokio::test]
    async fn reconnect_wait_is_cancel_aware() {
        let token = CancellationToken::new();
        token.cancel();
        let mut backoff = ReconnectBackoff::default();
        assert!(!wait_reconnect(&token, &mut backoff).await);
    }

    struct ControllerSubprotocolCallback;

    // The upstream Callback trait owns the handshake ErrorResponse ABI.
    // Implement that fixed interface directly; the test only accepts the
    // controller subprotocol and retains the assertion on the real request.
    impl tokio_tungstenite::tungstenite::handshake::server::Callback for ControllerSubprotocolCallback {
        fn on_request(
            self,
            request: &tokio_tungstenite::tungstenite::handshake::server::Request,
            mut response: tokio_tungstenite::tungstenite::handshake::server::Response,
        ) -> std::result::Result<
            tokio_tungstenite::tungstenite::handshake::server::Response,
            tokio_tungstenite::tungstenite::handshake::server::ErrorResponse,
        > {
            assert_eq!(
                request.headers().get(SEC_WEBSOCKET_PROTOCOL).unwrap(),
                super::super::protocol::CONTROLLER_SUBPROTOCOL
            );
            response.headers_mut().insert(
                SEC_WEBSOCKET_PROTOCOL,
                HeaderValue::from_static(super::super::protocol::CONTROLLER_SUBPROTOCOL),
            );
            Ok(response)
        }
    }

    #[test]
    fn only_positive_current_admission_is_a_backoff_reset_signal() {
        let config = ControllerTransportConfig::new(
            "ws://127.0.0.1:1",
            ControllerRole::ControlledB,
            "generation-0001",
        );
        let (sender, _) = transport_channels(
            ControllerRole::ControlledB,
            "generation-0001",
            2,
            2,
            8192,
            1024,
        );
        let sink = |_| {};
        assert!(!dispatch_inbound(&config, &sender, &sink, "invalid JSON"));
        let error = json!({ "v": 1, "type": "error", "request_id": "request-0001",
            "code": "controller_busy", "message": "busy" });
        validate_controller_message_json(&error.to_string(), SenderRole::Relay).unwrap();
        assert!(!dispatch_inbound(
            &config,
            &sender,
            &sink,
            &error.to_string()
        ));
        let mut invalid = control("request-0002");
        invalid["unknown"] = json!(true);
        assert!(!dispatch_inbound(
            &config,
            &sender,
            &sink,
            &invalid.to_string()
        ));
        assert!(dispatch_inbound(
            &config,
            &sender,
            &sink,
            &control("request-0003").to_string()
        ));
        sender.stop();
        assert!(!dispatch_inbound(
            &config,
            &sender,
            &sink,
            &control("request-0004").to_string()
        ));
    }

    #[tokio::test]
    async fn repeated_websocket_accept_close_keeps_exponential_backoff() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("ws://{}", listener.local_addr().unwrap());
        let mut server = tokio::spawn(async move {
            let mut accepted = Vec::new();
            for _ in 0..3 {
                let (stream, _) = listener.accept().await.unwrap();
                let mut socket = accept_hdr_async(stream, ControllerSubprotocolCallback)
                    .await
                    .unwrap();
                accepted.push(Instant::now());
                // A transport pong is life, but never controller admission.
                socket.send(Message::Pong(Vec::new().into())).await.unwrap();
                socket.send(Message::Close(None)).await.unwrap();
            }
            accepted
        });
        let sender = spawn_controller_transport(
            ControllerTransportConfig::new(url, ControllerRole::ControllerA, "generation-0001"),
            Arc::new(|_| {}),
        );
        let result = tokio::time::timeout(Duration::from_secs(8), &mut server).await;
        sender.stop();
        server.abort();
        let accepted = result.expect("three bounded attempts").unwrap();
        // Second failure is attempt 2: base500 *2 *jitter[.8,1.2].
        assert!(
            accepted[2].duration_since(accepted[1]) >= Duration::from_millis(790),
            "bare accept/close must not continually reset to the 400-600ms first delay"
        );
    }

    #[tokio::test]
    async fn silent_open_websocket_clears_installed_lease_before_reconnecting() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("ws://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let _socket = accept_hdr_async(stream, ControllerSubprotocolCallback)
                .await
                .unwrap();
            // Keep TCP open without reading/writing WS frames; no automatic pong.
            std::future::pending::<()>().await;
        });
        let config =
            ControllerTransportConfig::new(url, ControllerRole::ControllerA, "generation-0001");
        let (sender, receiver) = transport_channels_shared(
            config.role,
            config.remote_control_generation.clone(),
            2,
            2,
            8192,
            1024,
        );
        let (tx, mut rx) = mpsc::unbounded_channel();
        let task_sender = sender.clone();
        let worker = tokio::spawn(async move {
            run_transport_with_heartbeat(
                config,
                Arc::new(move |event| {
                    let _ = tx.send(event);
                }),
                task_sender,
                receiver,
                Duration::from_millis(20),
            )
            .await;
        });
        let result = tokio::time::timeout(Duration::from_secs(2), async {
            while !matches!(
                rx.recv().await.unwrap(),
                ControllerTransportEvent::Connected { .. }
            ) {}
            sender
                .install_authenticated_lease(lease("7", "generation-0001"))
                .unwrap();
            loop {
                if let ControllerTransportEvent::Disconnected { error, .. } =
                    rx.recv().await.unwrap()
                {
                    assert!(error.contains("inbound silence deadline"));
                    assert!(sender.authenticated_lease().is_none());
                    assert!(sender.resume_lease.read().is_some());
                    break;
                }
            }
        })
        .await;
        sender.stop();
        server.abort();
        worker.abort();
        result.expect("silent but open socket must trigger the actual disconnect path");
    }

    #[tokio::test]
    async fn websocket_reconnects_and_delivers_new_generation_frames() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("ws://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            let mut requests = Vec::new();
            for _ in 0..2 {
                let (stream, _) = listener.accept().await.unwrap();
                let mut socket = accept_hdr_async(stream, ControllerSubprotocolCallback)
                    .await
                    .unwrap();
                let message = socket.next().await.unwrap().unwrap();
                let Message::Text(text) = message else {
                    panic!("expected controller text frame");
                };
                requests.push(serde_json::from_str::<Value>(text.as_str()).unwrap());
                socket.send(Message::Close(None)).await.unwrap();
            }
            requests
        });
        let (event_tx, mut event_rx) = mpsc::unbounded_channel();
        let config =
            ControllerTransportConfig::new(url, ControllerRole::ControllerA, "generation-0001");
        let sender = spawn_controller_transport(
            config,
            Arc::new(move |event| {
                let _ = event_tx.send(event);
            }),
        );
        let result = tokio::time::timeout(Duration::from_secs(8), async {
            for index in 1..=2 {
                loop {
                    match event_rx.recv().await.unwrap() {
                        ControllerTransportEvent::Connected { .. } => break,
                        ControllerTransportEvent::Disconnected { .. } => {}
                        _ => {}
                    }
                }
                sender
                    .try_send_control(&control(&format!("request-000{index}")))
                    .unwrap();
                if index == 1 {
                    loop {
                        if matches!(
                            event_rx.recv().await.unwrap(),
                            ControllerTransportEvent::Disconnected { .. }
                        ) {
                            break;
                        }
                    }
                }
            }
            server.await.unwrap()
        })
        .await
        .expect("controller transport must reconnect after the first close");
        sender.stop();
        assert_eq!(result[0]["request_id"], "request-0001");
        assert_eq!(result[1]["request_id"], "request-0002");
    }

    #[tokio::test]
    async fn resume_provisional_crosses_real_websocket_after_disconnect_clears_lease() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("ws://{}", listener.local_addr().unwrap());
        let provisional = json!({
            "v": 1, "type": "controller_resume_provisional", "request_id": "request-resume-1",
            "connection_id": "connection-0001", "controller_lease_id": "lease-0001",
            "lease_epoch": "7", "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": "generation-0001", "activation_id": "activation-0001",
            "original_deadline_ms": "30000"
        });
        validate_controller_message_json(&provisional.to_string(), SenderRole::Relay).unwrap();
        let expected = provisional.clone();
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut socket = accept_hdr_async(stream, ControllerSubprotocolCallback)
                .await
                .unwrap();
            assert!(matches!(
                socket.next().await.unwrap().unwrap(),
                Message::Text(_)
            ));
            socket.send(Message::Close(None)).await.unwrap();
            let (stream, _) = listener.accept().await.unwrap();
            let mut socket = accept_hdr_async(stream, ControllerSubprotocolCallback)
                .await
                .unwrap();
            let Message::Text(prepare) = socket.next().await.unwrap().unwrap() else {
                panic!("expected resume prepare");
            };
            assert_eq!(
                serde_json::from_str::<Value>(prepare.as_str()).unwrap()["type"],
                "controller_resume_prepare"
            );
            let mut stale = provisional.clone();
            stale["remote_control_generation"] = json!("generation-stale");
            socket
                .send(Message::Text(stale.to_string().into()))
                .await
                .unwrap();
            let mut wrong_lease = provisional.clone();
            wrong_lease["controller_lease_id"] = json!("lease-other");
            socket
                .send(Message::Text(wrong_lease.to_string().into()))
                .await
                .unwrap();
            // Even during resume, unauthenticated encrypted business stays closed.
            socket.send(Message::Text(json!({
                "v":1, "type":"encrypted_frame", "connection_id":"connection-0001",
                "controller_lease_id":"lease-0001", "lease_epoch":"7", "relay_epoch":"relay-epoch-0001",
                "remote_control_generation":"generation-0001", "frame_sequence":"1", "direction":"b_to_a",
                "payload_kind":"task_status", "ciphertext_b64":"AQID", "ciphertext_bytes":"3"
            }).to_string().into())).await.unwrap();
            socket
                .send(Message::Text(provisional.to_string().into()))
                .await
                .unwrap();
            let _ = socket.next().await;
        });
        let (event_tx, mut event_rx) = mpsc::unbounded_channel();
        let sender = spawn_controller_transport(
            ControllerTransportConfig::new(url, ControllerRole::ControllerA, "generation-0001"),
            Arc::new(move |event| {
                let _ = event_tx.send(event);
            }),
        );
        let delivered = tokio::time::timeout(Duration::from_secs(8), async {
            while !matches!(
                event_rx.recv().await.unwrap(),
                ControllerTransportEvent::Connected { .. }
            ) {}
            sender
                .install_authenticated_lease(lease("7", "generation-0001"))
                .unwrap();
            sender
                .try_send_control(&control("request-initial-1"))
                .unwrap();
            while !matches!(
                event_rx.recv().await.unwrap(),
                ControllerTransportEvent::Disconnected { .. }
            ) {}
            assert!(sender.authenticated_lease().is_none());
            while !matches!(
                event_rx.recv().await.unwrap(),
                ControllerTransportEvent::Connected { .. }
            ) {}
            sender.try_send_sensitive_resume_control(&json!({
                "v":1, "type":"controller_resume_prepare", "request_id":"request-resume-1",
                "connection_id":"connection-0001", "controller_lease_id":"lease-0001",
                "lease_epoch":"7", "relay_epoch":"relay-epoch-0001",
                "remote_control_generation":"generation-0001", "sender_role":"A",
                "resume_token_b64":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=",
                "transport_challenge_b64":"AAAAAAAAAAAAAAAAAAAAAA==", "original_deadline_ms":"30000"
            })).unwrap();
            loop {
                if let ControllerTransportEvent::Frame { json, .. } = event_rx.recv().await.unwrap()
                {
                    return serde_json::from_str::<Value>(&json).unwrap();
                }
            }
        })
        .await;
        sender.stop();
        server.abort();
        assert_eq!(
            delivered.expect("valid resume must cross real transport admission"),
            expected
        );
    }
}
