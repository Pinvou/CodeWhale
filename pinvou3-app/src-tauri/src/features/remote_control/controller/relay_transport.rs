//! Bounded Relay transport shared by the controller and controlled-device domains.
//!
//! This layer owns only WebSocket lifecycle, queueing and lease-context admission.
//! Pairing, liveness, resume and task semantics remain in their dedicated modules.

use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use futures_util::{SinkExt, StreamExt};
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
    MAX_RELAY_FRAME_BYTES, ReconnectBackoff, relay_websocket_config,
};

const CONTROL_QUEUE_CAPACITY: usize = 64;
const BUSINESS_QUEUE_CAPACITY: usize = 256;
const TOTAL_OUTBOUND_BYTE_BUDGET: usize = 8 * 1024 * 1024;
const CONTROL_RESERVED_BYTES: usize = 128 * 1024;
const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(20);
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
    FrameTooLarge { bytes: usize, limit: usize },
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
    payload: WirePayload,
    _total_permit: OwnedSemaphorePermit,
    _business_permit: Option<OwnedSemaphorePermit>,
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
        *self.lease.write() = Some(lease);
        Ok(())
    }

    pub(crate) fn clear_authenticated_lease(&self) {
        *self.lease.write() = None;
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
    /// handshake frames may bypass lease admission; the resume state machine
    /// remains responsible for binding the old context and token to the peer.
    ///
    /// E2E-06：`controller_resume_ready_ack` 是同一族里的第三个出站帧
    /// （B 由 Relay 驱动确认，A 需要独立 ack 才能让 Relay 提交轮换）。
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
                    | "controller_resume_ready_ack"
            )
        ) {
            return Err(ControllerSendError::InvalidFrame("message.type".into()));
        }
        self.try_enqueue(FramePriority::Control, WirePayload::Sensitive(text), None)
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
        self.try_enqueue_with_receipt(priority, payload, lease_context, None)
    }

    fn try_enqueue_with_receipt(
        &self,
        priority: FramePriority,
        payload: WirePayload,
        lease_context: Option<LeaseContext>,
        flushed: Option<tokio::sync::oneshot::Sender<()>>,
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
    mut receiver: ControllerTransportReceiver,
) {
    let mut reconnect = ReconnectBackoff::default();
    loop {
        let connection = connect_async_with_config(
            &config.relay_url,
            Some(relay_websocket_config(MAX_CONTROLLER_INBOUND_TEXT_BYTES)),
            false,
        );
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
                if !wait_reconnect(&sender.shutdown, &mut reconnect).await {
                    return;
                }
                continue;
            }
        };
        let (mut write, mut read) = socket.split();
        reconnect.reset();
        sink.emit(ControllerTransportEvent::Connected {
            remote_control_generation: config.generation(),
            lease_epoch: current_lease_epoch(&sender),
        });

        let mut heartbeat = tokio::time::interval(HEARTBEAT_INTERVAL);
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        heartbeat.tick().await;
        let disconnected = loop {
            tokio::select! {
                biased;
                _ = sender.shutdown.cancelled() => {
                    let _ = write.send(Message::Close(None)).await;
                    return;
                }
                frame = receiver.recv_prioritized() => {
                    let Some(frame) = frame else { return; };
                    if !frame_is_current(&sender, &frame) {
                        continue;
                    }
                    let result = tokio::select! {
                        biased;
                        _ = sender.shutdown.cancelled() => return,
                        result = write.send(Message::Text(frame.text().to_owned().into())) => result,
                    };
                    match result {
                        Ok(()) => {
                            if let Some(flushed) = frame.flushed {
                                let _ = flushed.send(());
                            }
                        }
                        Err(error) => {
                            break format!("send Relay frame: {error}");
                        }
                    }
                }
                message = read.next() => {
                    let Some(message) = message else {
                        break "Relay connection closed".to_string();
                    };
                    match message {
                        Ok(Message::Text(text)) => {
                            if text.len() > MAX_CONTROLLER_INBOUND_TEXT_BYTES {
                                break format!(
                                    "Relay frame exceeds {MAX_CONTROLLER_INBOUND_TEXT_BYTES} bytes"
                                );
                            }
                            dispatch_inbound(&config, &sender, sink.as_ref(), text.as_str());
                        }
                        Ok(Message::Ping(payload)) => {
                            if let Err(error) = write.send(Message::Pong(payload)).await {
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
                    if let Err(error) = write.send(Message::Ping(payload.into())).await {
                        break format!("send Relay heartbeat: {error}");
                    }
                }
            }
        };
        emit_disconnected(&config, &sender, sink.as_ref(), disconnected);
        if !wait_reconnect(&sender.shutdown, &mut reconnect).await {
            return;
        }
    }
}

fn frame_is_current(sender: &ControllerTransportSender, frame: &QueuedFrame) -> bool {
    if frame.transport_epoch != sender.transport_epoch.load(Ordering::Acquire) {
        return false;
    }
    frame.lease_context.as_ref().is_none_or(|expected| {
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
    sender.transport_epoch.fetch_add(1, Ordering::AcqRel);
    sender.clear_authenticated_lease();
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
) {
    let Ok(shape) = serde_json::from_str::<Value>(text) else {
        return;
    };
    if !generation_admits(&config.generation(), &shape) {
        return;
    }
    let kind = shape.get("type").and_then(Value::as_str);
    let priority = if kind == Some("encrypted_frame") {
        FramePriority::Business
    } else {
        FramePriority::Control
    };
    let lease = sender.lease.read().clone();
    let validated = if priority == FramePriority::Business {
        let Some(lease) = lease.as_ref() else {
            return;
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
    if !validated || !inbound_context_matches(&shape, lease.as_ref()) {
        return;
    }
    sink.emit(ControllerTransportEvent::Frame {
        priority,
        remote_control_generation: config.generation(),
        lease_epoch: lease
            .as_ref()
            .map(|value| value.context.lease_epoch.clone()),
        json: text.to_owned(),
    });
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
    use tokio_tungstenite::accept_async;

    use super::*;
    use crate::features::remote_control::controller::{
        controlled::ControlledDeviceManager, controlling::ControllingDeviceManager,
    };

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

    #[tokio::test]
    async fn websocket_reconnects_and_delivers_new_generation_frames() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("ws://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            let mut requests = Vec::new();
            for _ in 0..2 {
                let (stream, _) = listener.accept().await.unwrap();
                let mut socket = accept_async(stream).await.unwrap();
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
}
