//! A-only ingress ownership, independent of the transport's outbound budgets.
//!
//! Control and business retain their own FIFO order. A disconnect freezes
//! business delivery, preserving accepted journal tails for the same lease's
//! authenticated resume. Its fixed control prefix cannot grow on reconnect.

use std::collections::VecDeque;
use std::sync::{Arc, OnceLock, Weak};

use parking_lot::Mutex;
use tokio::sync::{Notify, OwnedSemaphorePermit, Semaphore};

use super::lane_event::{ControllerLaneEvent, ControllerLaneFailure};
use super::relay_transport::{
    ControllerDisconnectContext, ControllerTransportEvent, ControllerTransportSender, FramePriority,
};

#[derive(Clone, Copy)]
pub(super) struct InboundLimits {
    pub(super) control_items: usize,
    pub(super) business_items: usize,
    pub(super) total_bytes: usize,
    pub(super) control_reserved_bytes: usize,
}

impl Default for InboundLimits {
    fn default() -> Self {
        Self {
            control_items: 64,
            business_items: 256,
            total_bytes: 8 * 1024 * 1024,
            control_reserved_bytes: 128 * 1024,
        }
    }
}

/// Ownership stays with the Port until its serial consumer asks for another
/// event. Only the original String allocation is charged, not parsed/crypto
/// temporaries, the reader's current frame, or the process's entire memory.
pub(super) struct InboundCharge {
    _item: OwnedSemaphorePermit,
    _total: OwnedSemaphorePermit,
    _business: Option<OwnedSemaphorePermit>,
}

pub(super) struct InboundDelivery {
    pub(super) event: ControllerLaneEvent,
    pub(super) charge: Option<InboundCharge>,
}

struct QueuedFrame {
    json: String,
    charge: InboundCharge,
}

impl QueuedFrame {
    fn into_delivery(self) -> InboundDelivery {
        InboundDelivery {
            event: ControllerLaneEvent::Frame(self.json),
            charge: Some(self.charge),
        }
    }
}

/// Delivery keeps each accepted disconnect's owner context distinct so the
/// consumer can filter stale transports. The bound keeps a disconnect flood
/// constant-space: the oldest undelivered context is dropped, never the
/// finite control prefix accounting of the survivors.
const PENDING_DISCONNECT_CAP: usize = 8;

#[derive(Default)]
struct QueueState {
    control: VecDeque<QueuedFrame>,
    business: VecDeque<QueuedFrame>,
    // Disconnects accepted but not yet delivered, in arrival order. Each
    // records how many queued controls precede it: the first pending
    // disconnect freezes that finite control prefix, and a repeated
    // disconnect never moves an already counted prefix forward. Contexts
    // stay distinct so the consumer can filter stale transport owners.
    pending_disconnects: VecDeque<(usize, ControllerDisconnectContext)>,
    business_paused: bool,
    failure: Option<ControllerLaneFailure>,
    receiver_closed: bool,
}

struct State {
    queue: Mutex<QueueState>,
    ready: Notify,
    control_items: Arc<Semaphore>,
    business_items: Arc<Semaphore>,
    total_bytes: Arc<Semaphore>,
    business_bytes: Arc<Semaphore>,
    transport: OnceLock<ControllerTransportSender>,
}

impl State {
    fn fail(&self, reason: ControllerLaneFailure) {
        {
            let mut queue = self.queue.lock();
            queue.failure.get_or_insert(reason);
            queue.control.clear();
            queue.business.clear();
        }
        self.ready.notify_one();
        // Never take a transport/manager lock while holding the inbox lock.
        // request_stop also avoids waiting for a consumer's lease read guard.
        if let Some(transport) = self.transport.get() {
            transport.request_stop();
        }
    }

    fn charge(
        &self,
        priority: FramePriority,
        bytes: usize,
    ) -> Result<InboundCharge, ControllerLaneFailure> {
        let items = match priority {
            FramePriority::Control => &self.control_items,
            FramePriority::Business => &self.business_items,
        };
        let item = items
            .clone()
            .try_acquire_owned()
            .map_err(|_| ControllerLaneFailure::ItemCapacity)?;
        let bytes = u32::try_from(bytes).map_err(|_| ControllerLaneFailure::ByteBudget)?;
        let total = self
            .total_bytes
            .clone()
            .try_acquire_many_owned(bytes)
            .map_err(|_| ControllerLaneFailure::ByteBudget)?;
        let business = if priority == FramePriority::Business {
            Some(
                self.business_bytes
                    .clone()
                    .try_acquire_many_owned(bytes)
                    .map_err(|_| ControllerLaneFailure::ByteBudget)?,
            )
        } else {
            None
        };
        Ok(InboundCharge {
            _item: item,
            _total: total,
            _business: business,
        })
    }
}

struct ProducerCore {
    state: Weak<State>,
}

impl Drop for ProducerCore {
    fn drop(&mut self) {
        if let Some(state) = self.state.upgrade() {
            state.fail(ControllerLaneFailure::Closed);
        }
    }
}

#[derive(Clone)]
pub(super) struct InboundSender {
    core: Arc<ProducerCore>,
}

impl InboundSender {
    pub(super) fn bind_transport(&self, transport: ControllerTransportSender) {
        let Some(state) = self.core.state.upgrade() else {
            transport.request_stop();
            return;
        };
        if state.transport.set(transport.clone()).is_err() {
            state.fail(ControllerLaneFailure::Closed);
            transport.request_stop();
            return;
        }
        // Either bind sees an earlier terminal, or that terminal sees this
        // exact sender in OnceLock. Neither ordering can miss cancellation.
        let queue = state.queue.lock();
        let closed = queue.failure.is_some() || queue.receiver_closed;
        drop(queue);
        if closed {
            transport.request_stop();
        }
    }

    pub(super) fn enqueue(&self, event: ControllerTransportEvent) {
        let Some(state) = self.core.state.upgrade() else {
            return;
        };
        let mut queue = state.queue.lock();
        if queue.failure.is_some() || queue.receiver_closed {
            return;
        }
        match event {
            ControllerTransportEvent::Frame { priority, json, .. } => {
                let charge = match state.charge(priority, json.capacity()) {
                    Ok(charge) => charge,
                    Err(reason) => {
                        // Commit terminal before unlocking; another producer
                        // cannot admit a frame in the stop-notification gap.
                        queue.failure = Some(reason);
                        queue.control.clear();
                        queue.business.clear();
                        drop(queue);
                        state.fail(reason);
                        return;
                    }
                };
                let frame = QueuedFrame { json, charge };
                match priority {
                    FramePriority::Control => queue.control.push_back(frame),
                    FramePriority::Business => queue.business.push_back(frame),
                }
            }
            ControllerTransportEvent::Disconnected { context, .. } => {
                queue.business_paused = true;
                if queue.pending_disconnects.len() >= PENDING_DISCONNECT_CAP {
                    queue.pending_disconnects.pop_front();
                }
                let preceding_controls = queue.control.len();
                queue
                    .pending_disconnects
                    .push_back((preceding_controls, context));
            }
            ControllerTransportEvent::Connected { .. }
            | ControllerTransportEvent::RttSample { .. } => return,
        }
        drop(queue);
        state.ready.notify_one();
    }
}

pub(super) struct InboundReceiver {
    state: Arc<State>,
    failure_delivered: bool,
}

impl InboundReceiver {
    #[cfg(test)]
    pub(super) fn remaining_bytes(&self) -> usize {
        self.state.total_bytes.available_permits()
    }

    /// Test observation used while the actual Port consumer is held behind
    /// its barrier; this does not replace admission or manufacture a failure.
    #[cfg(test)]
    pub(super) async fn wait_for_failure(&self) -> ControllerLaneFailure {
        loop {
            let notified = self.state.ready.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if let Some(reason) = self.failure() {
                return reason;
            }
            notified.await;
        }
    }

    pub(super) fn failure(&self) -> Option<ControllerLaneFailure> {
        self.state.queue.lock().failure
    }

    pub(super) fn disconnect_pending(&self) -> bool {
        !self.state.queue.lock().pending_disconnects.is_empty()
    }

    /// Called only after complete_resumed and business.activate succeed.
    pub(super) fn resume_business(&self) -> Result<(), ControllerLaneFailure> {
        {
            let mut queue = self.state.queue.lock();
            if let Some(failure) = queue.failure {
                return Err(failure);
            }
            // A newer socket close may have raced the successful resume.
            // Preserve its barrier; that close does not reopen this inbox.
            queue.business_paused = !queue.pending_disconnects.is_empty();
        }
        self.state.ready.notify_one();
        Ok(())
    }

    pub(super) async fn recv(&mut self) -> Option<InboundDelivery> {
        if self.failure_delivered {
            return None;
        }
        loop {
            let notified = self.state.ready.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            {
                let mut queue = self.state.queue.lock();
                // A disconnect accepted before the failure still reaches the
                // consumer first, in arrival order: its resume attempt fails
                // closed on the stopped transport. Producers cannot enqueue
                // behind a terminal, so nothing newer can precede it.
                loop {
                    match queue.pending_disconnects.front().map(|(before, _)| *before) {
                        Some(before) if before > 0 => {}
                        _ => break,
                    }
                    let Some(frame) = queue.control.pop_front() else {
                        break;
                    };
                    if let Some((before, _)) = queue.pending_disconnects.front_mut() {
                        *before -= 1;
                    }
                    return Some(frame.into_delivery());
                }
                if let Some((_, context)) = queue.pending_disconnects.pop_front() {
                    return Some(InboundDelivery {
                        event: ControllerLaneEvent::Disconnected(context),
                        charge: None,
                    });
                }
                if let Some(failure) = queue.failure {
                    self.failure_delivered = true;
                    return Some(InboundDelivery {
                        event: ControllerLaneEvent::Failed(failure),
                        charge: None,
                    });
                }
                if let Some(frame) = queue.control.pop_front() {
                    return Some(frame.into_delivery());
                }
                if !queue.business_paused {
                    if let Some(frame) = queue.business.pop_front() {
                        return Some(frame.into_delivery());
                    }
                }
            }
            notified.await;
        }
    }
}

impl Drop for InboundReceiver {
    fn drop(&mut self) {
        {
            let mut queue = self.state.queue.lock();
            queue.receiver_closed = true;
        }
        self.state.fail(ControllerLaneFailure::Closed);
    }
}

pub(super) fn inbound_channel(limits: InboundLimits) -> (InboundSender, InboundReceiver) {
    assert!(limits.control_items > 0 && limits.business_items > 0);
    assert!(limits.control_reserved_bytes < limits.total_bytes);
    let state = Arc::new(State {
        queue: Mutex::new(QueueState::default()),
        ready: Notify::new(),
        control_items: Arc::new(Semaphore::new(limits.control_items)),
        business_items: Arc::new(Semaphore::new(limits.business_items)),
        total_bytes: Arc::new(Semaphore::new(limits.total_bytes)),
        business_bytes: Arc::new(Semaphore::new(
            limits.total_bytes - limits.control_reserved_bytes,
        )),
        transport: OnceLock::new(),
    });
    (
        InboundSender {
            core: Arc::new(ProducerCore {
                state: Arc::downgrade(&state),
            }),
        },
        InboundReceiver {
            state,
            failure_delivered: false,
        },
    )
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::super::protocol::{MAX_CONTROL_MESSAGE_BYTES, MAX_ENCRYPTED_WIRE_BYTES};
    use super::super::relay_transport::{ControllerRole, transport_channels};
    use super::*;

    fn limits() -> InboundLimits {
        InboundLimits {
            control_items: 8,
            business_items: 8,
            total_bytes: 100,
            control_reserved_bytes: 40,
        }
    }

    fn frame(priority: FramePriority, byte: u8, bytes: usize) -> ControllerTransportEvent {
        ControllerTransportEvent::Frame {
            priority,
            remote_control_generation: "generation-test".into(),
            lease_epoch: Some("1".into()),
            json: String::from_utf8(vec![byte; bytes]).unwrap(),
        }
    }

    fn disconnected(context: ControllerDisconnectContext) -> ControllerTransportEvent {
        ControllerTransportEvent::Disconnected {
            remote_control_generation: "generation-test".into(),
            lease_epoch: Some("1".into()),
            context,
            error: "fake-secret-is-never-forwarded".into(),
        }
    }

    /// Each call allocates a fresh owner Arc, so two contexts never compare
    /// equal even though no real socket is involved.
    fn context_for(tag: &str) -> ControllerDisconnectContext {
        let (sender, _outbound) =
            transport_channels(ControllerRole::ControllerA, tag, 2, 2, 100, 40);
        sender.disconnect_context()
    }

    async fn next(receiver: &mut InboundReceiver) -> InboundDelivery {
        tokio::time::timeout(Duration::from_secs(5), receiver.recv())
            .await
            .unwrap()
            .unwrap()
    }

    #[test]
    fn default_budget_accepts_maximum_legal_frames_and_reserves_control() {
        let limits = InboundLimits::default();
        assert!(limits.total_bytes - limits.control_reserved_bytes >= MAX_ENCRYPTED_WIRE_BYTES);
        assert!(limits.control_reserved_bytes >= 2 * MAX_CONTROL_MESSAGE_BYTES);
    }

    #[tokio::test]
    async fn item_capacity_is_independent_of_bytes_and_terminal_cannot_be_lost() {
        let (sender, mut receiver) = inbound_channel(InboundLimits {
            business_items: 2,
            ..limits()
        });
        sender.enqueue(frame(FramePriority::Business, b'a', 1));
        sender.enqueue(frame(FramePriority::Business, b'b', 1));
        assert_eq!(receiver.state.business_items.available_permits(), 0);
        assert_eq!(receiver.state.total_bytes.available_permits(), 98);
        sender.enqueue(frame(FramePriority::Business, b'c', 1));
        assert_eq!(
            receiver.failure(),
            Some(ControllerLaneFailure::ItemCapacity)
        );
        assert_eq!(receiver.state.total_bytes.available_permits(), 100);
        let failure = next(&mut receiver).await;
        assert_eq!(
            failure.event,
            ControllerLaneEvent::Failed(ControllerLaneFailure::ItemCapacity)
        );
        sender.enqueue(frame(FramePriority::Control, b'd', 1));
        drop(sender);
        assert_eq!(
            receiver.failure(),
            Some(ControllerLaneFailure::ItemCapacity)
        );
        assert!(receiver.recv().await.is_none());
    }

    #[tokio::test]
    async fn total_bytes_boundary_and_business_reservation_are_distinct() {
        let (sender, mut receiver) = inbound_channel(limits());
        sender.enqueue(frame(FramePriority::Business, b'b', 60));
        sender.enqueue(frame(FramePriority::Control, b'c', 40));
        assert_eq!(receiver.state.total_bytes.available_permits(), 0);
        sender.enqueue(frame(FramePriority::Control, b'x', 1));
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Failed(ControllerLaneFailure::ByteBudget)
        );
        assert_eq!(receiver.state.total_bytes.available_permits(), 100);
        assert_eq!(receiver.state.business_bytes.available_permits(), 60);

        let (sender, mut receiver) = inbound_channel(limits());
        sender.enqueue(frame(FramePriority::Business, b'b', 61));
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Failed(ControllerLaneFailure::ByteBudget)
        );
        assert_eq!(receiver.state.total_bytes.available_permits(), 100);
        assert_eq!(receiver.state.business_items.available_permits(), 8);
    }

    #[tokio::test]
    async fn allocated_capacity_is_charged_instead_of_only_string_length() {
        let (sender, mut receiver) = inbound_channel(limits());
        let mut json = String::with_capacity(101);
        json.push('x');
        sender.enqueue(ControllerTransportEvent::Frame {
            priority: FramePriority::Control,
            remote_control_generation: "generation-test".into(),
            lease_epoch: Some("1".into()),
            json,
        });
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Failed(ControllerLaneFailure::ByteBudget)
        );
    }

    #[tokio::test]
    async fn business_exhaustion_preserves_control_priority_and_both_fifos() {
        let (sender, mut receiver) = inbound_channel(limits());
        sender.enqueue(frame(FramePriority::Business, b'a', 30));
        sender.enqueue(frame(FramePriority::Business, b'b', 30));
        sender.enqueue(frame(FramePriority::Control, b'c', 20));
        sender.enqueue(frame(FramePriority::Control, b'd', 20));
        for (byte, bytes) in [(b'c', 20), (b'd', 20), (b'a', 30), (b'b', 30)] {
            let delivery = next(&mut receiver).await;
            assert_eq!(
                delivery.event,
                ControllerLaneEvent::Frame(String::from_utf8(vec![byte; bytes]).unwrap())
            );
            drop(delivery);
        }
        assert_eq!(receiver.state.total_bytes.available_permits(), 100);
    }

    #[tokio::test]
    async fn control_flood_is_bounded_and_first_failure_wins() {
        let (sender, mut receiver) = inbound_channel(InboundLimits {
            control_items: 2,
            ..limits()
        });
        for _ in 0..3 {
            sender.enqueue(frame(FramePriority::Control, b'c', 1));
        }
        sender.enqueue(disconnected(context_for("flood")));
        drop(sender);
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Failed(ControllerLaneFailure::ItemCapacity)
        );
        assert_eq!(receiver.state.control_items.available_permits(), 2);
    }

    #[tokio::test]
    async fn in_flight_charge_survives_dequeue_and_is_released_on_drop() {
        let (sender, mut receiver) = inbound_channel(limits());
        sender.enqueue(frame(FramePriority::Business, b'a', 60));
        let in_flight = next(&mut receiver).await;
        assert_eq!(receiver.state.business_bytes.available_permits(), 0);
        sender.enqueue(frame(FramePriority::Control, b'c', 40));
        sender.enqueue(frame(FramePriority::Control, b'x', 1));
        assert_eq!(receiver.failure(), Some(ControllerLaneFailure::ByteBudget));
        // Terminal clears queued controls, but cannot reclaim the consumer's
        // still-owned raw frame by pretending it has already finished.
        assert_eq!(receiver.state.total_bytes.available_permits(), 40);
        drop(in_flight);
        assert_eq!(receiver.state.total_bytes.available_permits(), 100);
    }

    #[tokio::test]
    async fn disconnect_has_fixed_control_prefix_and_preserves_business_until_resume() {
        let (sender, mut receiver) = inbound_channel(limits());
        let first = context_for("first-disconnect");
        let second = context_for("second-disconnect");
        sender.enqueue(frame(FramePriority::Business, b'b', 10));
        sender.enqueue(frame(FramePriority::Control, b'a', 10));
        sender.enqueue(disconnected(first.clone()));
        sender.enqueue(frame(FramePriority::Control, b'c', 10));
        sender.enqueue(disconnected(second.clone()));
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Frame("a".repeat(10))
        );
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Disconnected(first)
        );
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Frame("c".repeat(10))
        );
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Disconnected(second)
        );
        let mut waiting = Box::pin(receiver.recv());
        assert!(futures_util::poll!(&mut waiting).is_pending());
        drop(waiting);
        // No subsequent B business is needed to make its already accepted
        // tail observable once the real resume consumer opens the gate.
        receiver.resume_business().unwrap();
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Frame("b".repeat(10))
        );
    }

    #[tokio::test]
    async fn cancelling_pending_receive_keeps_frames_and_last_producer_wakes_closed() {
        let (sender, mut receiver) = inbound_channel(limits());
        let mut waiting = Box::pin(receiver.recv());
        assert!(futures_util::poll!(&mut waiting).is_pending());
        drop(waiting);
        sender.enqueue(frame(FramePriority::Business, b'b', 1));
        assert_eq!(
            next(&mut receiver).await.event,
            ControllerLaneEvent::Frame("b".into())
        );
        let mut waiting = Box::pin(receiver.recv());
        assert!(futures_util::poll!(&mut waiting).is_pending());
        drop(sender);
        let terminal = tokio::time::timeout(Duration::from_secs(5), waiting)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(
            terminal.event,
            ControllerLaneEvent::Failed(ControllerLaneFailure::Closed)
        );
    }

    #[tokio::test]
    async fn fatal_and_transport_binding_cover_both_orders_and_receiver_drop() {
        for failure_first in [false, true] {
            let (sender, mut receiver) = inbound_channel(limits());
            let (transport, _outbound) = transport_channels(
                ControllerRole::ControllerA,
                "generation-test",
                2,
                2,
                100,
                40,
            );
            if !failure_first {
                sender.bind_transport(transport.clone());
            }
            sender.enqueue(frame(FramePriority::Business, b'x', 61));
            if failure_first {
                sender.bind_transport(transport.clone());
            }
            assert!(transport.is_stopped());
            assert_eq!(
                next(&mut receiver).await.event,
                ControllerLaneEvent::Failed(ControllerLaneFailure::ByteBudget)
            );
        }
        let (sender, receiver) = inbound_channel(limits());
        drop(receiver);
        let (transport, _outbound) = transport_channels(
            ControllerRole::ControllerA,
            "generation-test",
            2,
            2,
            100,
            40,
        );
        sender.bind_transport(transport.clone());
        assert!(transport.is_stopped());
    }

    #[test]
    fn receiver_drop_releases_queues_and_stops_only_its_bound_transport() {
        let (sender, receiver) = inbound_channel(limits());
        let total = receiver.state.total_bytes.clone();
        let (old, _old_outbound) = transport_channels(
            ControllerRole::ControllerA,
            "generation-test",
            2,
            2,
            100,
            40,
        );
        let (new, _new_outbound) = transport_channels(
            ControllerRole::ControllerA,
            "generation-test",
            2,
            2,
            100,
            40,
        );
        sender.bind_transport(old.clone());
        sender.enqueue(frame(FramePriority::Business, b'x', 60));
        assert_eq!(total.available_permits(), 40);
        drop(receiver);
        assert_eq!(total.available_permits(), 100);
        sender.enqueue(frame(FramePriority::Business, b'x', 60));
        drop(sender);
        assert!(old.is_stopped());
        assert!(!new.is_stopped());
    }
}
