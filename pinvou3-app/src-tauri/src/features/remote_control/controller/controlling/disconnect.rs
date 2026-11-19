//! Commit local disconnect before any receipt wait. The returned scope owns
//! only the retired socket; dropping/cancelling it cannot stop a new socket.
use super::super::controlling_session::ControllingSessionManager;
use super::*;
use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;

pub(crate) struct ControllingDisconnect {
    transport: Option<ControllerTransportSender>,
    receipt: Option<tokio::sync::oneshot::Receiver<()>>,
    projection: ControllerDisconnectProjection,
}

impl ControllingDisconnect {
    pub(crate) fn projection(&self) -> ControllerDisconnectProjection {
        self.projection.clone()
    }

    pub(crate) async fn flush(mut self) {
        if let Some(receipt) = self.receipt.take() {
            let _ = tokio::time::timeout(std::time::Duration::from_secs(2), receipt).await;
        }
        // Drop stops precisely the captured socket, including cancellation.
    }
}

impl Drop for ControllingDisconnect {
    fn drop(&mut self) {
        if let Some(transport) = &self.transport {
            transport.stop();
        }
    }
}

impl ControllingDeviceManager {
    /// Lock order is intents -> manager -> business. Cleanup is synchronous,
    /// never re-enters either manager, and never publishes callbacks.
    /// No guard crosses await.
    pub(crate) fn disconnect_business(
        &self,
        sessions: &ControllingSessionManager,
        business: &ControllingLaneState,
        cleanup: impl FnOnce(Option<&LeaseContext>),
    ) -> ControllingDisconnect {
        sessions.clear_with_owner(|clear_sessions| {
            let mut inner = self.inner.lock();
            let retired = inner
                .lease
                .as_ref()
                .map(|lease| lease.context.clone())
                .or_else(|| inner.retired_lease.clone());
            let receipt = if inner.phase == ControllingStatusKind::Connected {
                retired.as_ref().and_then(|lease| {
                    let frame = build_controller_disconnect(
                        lease,
                        &format!("request-disconnect-{}", Uuid::new_v4().simple()),
                        "user_disconnect",
                    )
                    .ok()?;
                    inner
                        .transport
                        .as_ref()?
                        .send_control_with_receipt(&frame)
                        .ok()
                })
            } else {
                None
            };
            let transport = inner.transport.take();
            if let Some(transport) = &transport {
                inner.retired_transport_owner = Some(transport.owner());
            }
            // Preserve the existing retired lease identity but defer only socket
            // shutdown until its already-queued final frame can flush.
            clear_live_context(&mut inner);
            business.clear();
            inner.phase = ControllingStatusKind::Disconnected;
            inner.error_code = None;
            let disconnect = ControllingDisconnect {
                transport,
                receipt,
                projection: ControllerDisconnectProjection {
                    intent: inner.intent,
                    status: status_locked(&inner),
                },
            };
            if let Some(lease) = &retired {
                clear_sessions(&lease.connection_id);
            }
            cleanup(retired.as_ref());
            disconnect
        })
    }
}
