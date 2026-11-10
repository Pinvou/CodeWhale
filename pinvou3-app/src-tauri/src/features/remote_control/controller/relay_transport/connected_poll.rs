//! Bounded outbound bursts preserve timely inbound, heartbeat and Stop polls.
use super::*;

pub(super) enum ConnectedEvent<E> {
    Shutdown,
    Silence,
    Outgoing(Option<QueuedFrame>),
    Incoming(Option<Result<Message, E>>),
    Heartbeat,
}

pub(super) async fn next_event<R, E>(
    shutdown: &CancellationToken,
    receiver: &mut ControllerTransportReceiver,
    read: &mut R,
    heartbeat: &mut tokio::time::Interval,
    deadline: tokio::time::Instant,
    outbound_limit_reached: bool,
) -> ConnectedEvent<E>
where
    R: Stream<Item = Result<Message, E>> + Unpin,
{
    if outbound_limit_reached {
        tokio::select! {
            biased;
            _ = shutdown.cancelled() => ConnectedEvent::Shutdown,
            _ = tokio::time::sleep_until(deadline) => ConnectedEvent::Silence,
            _ = heartbeat.tick() => ConnectedEvent::Heartbeat,
            message = read.next() => ConnectedEvent::Incoming(message),
            frame = receiver.recv_prioritized() => ConnectedEvent::Outgoing(frame),
        }
    } else {
        tokio::select! {
            biased;
            _ = shutdown.cancelled() => ConnectedEvent::Shutdown,
            _ = tokio::time::sleep_until(deadline) => ConnectedEvent::Silence,
            frame = receiver.recv_prioritized() => ConnectedEvent::Outgoing(frame),
            message = read.next() => ConnectedEvent::Incoming(message),
            _ = heartbeat.tick() => ConnectedEvent::Heartbeat,
        }
    }
}
