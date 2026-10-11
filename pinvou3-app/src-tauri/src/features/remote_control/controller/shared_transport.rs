//! Controller authority for B's shared desktop socket, not a second transport.
//!
//! Only the socket reader can mint receipts. The lifecycle owner consumes them
//! after authenticating the handshake/resume; queued old receipts cannot grant
//! authority on a replacement socket. The business gate permits reviewed narrow
//! synchronous mutations and non-reentrant telemetry only; no await or callback
//! into lifecycle/transport/desktop-endpoint owners occurs under this lock.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use parking_lot::Mutex;
use serde_json::Value;

use super::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_message_json,
    validate_controller_runtime_message_json,
};
use super::resume::resume_frame_matches_lease;

#[derive(Default)]
struct State {
    epoch: u64,
    connected: bool,
    generation: Option<String>,
    binding: u64,
    lease: Option<RuntimeValidationContext>,
    suspended: bool,
}

#[derive(Clone, Default)]
pub(crate) struct SharedControllerAuthority(Arc<Mutex<State>>);

struct ReceiptIdentity {
    epoch: u64,
    binding: u64,
    generation: String,
    kind: String,
    context: LeaseContext,
    activation_id: Option<String>,
    request_id: String,
    consumed: AtomicBool,
}

/// Internal queue metadata; never serialized or accepted from wire fields.
#[derive(Clone)]
pub struct ControllerReadReceipt {
    authority: SharedControllerAuthority,
    identity: Arc<ReceiptIdentity>,
}

impl std::fmt::Debug for ControllerReadReceipt {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("ControllerReadReceipt([REDACTED])")
    }
}

pub(crate) struct SharedSocket {
    authority: SharedControllerAuthority,
    epoch: u64,
}

impl Drop for SharedSocket {
    fn drop(&mut self) {
        self.invalidate();
    }
}

impl SharedSocket {
    pub(crate) fn invalidate(&self) {
        let mut state = self.authority.0.lock();
        if state.epoch == self.epoch {
            state.connected = false;
            state.suspended = true;
        }
    }
}

#[cfg(test)]
mod business_authority_tests {
    use super::*;

    fn fixture() -> (SharedControllerAuthority, LeaseContext) {
        let expected = LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        };
        // Only the synchronization gate is under test here. Reader-minted
        // receipt/handshake proof is exercised by the native loopback driver.
        let authority = SharedControllerAuthority::default();
        *authority.0.lock() = State {
            connected: true,
            generation: Some(expected.remote_control_generation.clone()),
            binding: 1,
            lease: Some(RuntimeValidationContext {
                lease: Some(expected.clone()),
                negotiated_capabilities: vec!["task_v1".into()],
            }),
            ..State::default()
        };
        (authority, expected)
    }

    #[test]
    fn retired_suspended_or_foreign_business_authority_never_runs_mutation() {
        let (authority, expected) = fixture();
        let mut calls = 0;
        assert!(
            authority.with_business_authority("generation-0001", 1, &expected, &mut || calls += 1)
        );
        let mut foreign = expected.clone();
        foreign.controller_lease_id = "lease-0002".into();
        assert!(
            !authority.with_business_authority("generation-0001", 1, &foreign, &mut || calls += 1)
        );
        assert!(
            !authority.with_business_authority("generation-0001", 2, &expected, &mut || calls += 1)
        );
        authority.0.lock().suspended = true;
        assert!(
            !authority.with_business_authority("generation-0001", 1, &expected, &mut || calls += 1)
        );
        authority.0.lock().suspended = false;
        authority.retire_owned("generation-0001", 1);
        assert!(
            !authority.with_business_authority("generation-0001", 1, &expected, &mut || calls += 1)
        );
        assert_eq!(calls, 1);
    }

    #[test]
    fn retirement_cannot_cross_an_admitted_synchronous_mutation() {
        let (authority, expected) = fixture();
        let worker_authority = authority.clone();
        let (entered_tx, entered_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let worker = std::thread::spawn(move || {
            worker_authority.with_business_authority("generation-0001", 1, &expected, &mut || {
                entered_tx.send(()).unwrap();
                release_rx
                    .recv_timeout(std::time::Duration::from_secs(3))
                    .unwrap();
            })
        });
        entered_rx
            .recv_timeout(std::time::Duration::from_secs(3))
            .unwrap();
        assert!(
            authority.0.try_lock().is_none(),
            "mutation must hold the retirement lock"
        );
        release_tx.send(()).unwrap();
        assert!(worker.join().unwrap());
        authority.retire_owned("generation-0001", 1);
        assert!(authority.binding("generation-0001").is_none());
    }
}

/// Enqueued controller frames are socket/generation scoped, unlike legacy v2.
pub(crate) struct ControllerOutboundScope {
    epoch: u64,
    generation: String,
    binding: u64,
    business: bool,
}

impl SharedControllerAuthority {
    pub(crate) fn open_socket(&self, controller_protocol: bool) -> Option<SharedSocket> {
        let mut state = self.0.lock();
        state.connected = false;
        state.suspended = true;
        state.epoch = state.epoch.checked_add(1)?;
        // Physical v2 connectivity never grants controller authority. The
        // negotiated mode is frozen with this socket epoch, not wire input.
        state.connected = controller_protocol;
        Some(SharedSocket {
            authority: self.clone(),
            epoch: state.epoch,
        })
    }

    pub(crate) fn shutdown(&self) {
        let mut state = self.0.lock();
        state.connected = false;
        state.suspended = true;
        state.lease = None;
        state.generation = None;
    }

    pub(crate) fn bind(&self, generation: &str) -> Option<u64> {
        let mut state = self.0.lock();
        if generation.is_empty() || state.generation.is_some() {
            return None;
        }
        let binding = state.binding.checked_add(1)?;
        state.binding = binding;
        state.generation = Some(generation.to_owned());
        state.lease = None;
        state.suspended = true;
        Some(binding)
    }

    pub(crate) fn retire(&self, generation: &str) {
        let mut state = self.0.lock();
        if state.generation.as_deref() == Some(generation) {
            state.generation = None;
            state.lease = None;
            state.suspended = true;
        }
    }

    pub(crate) fn owns(&self, generation: &str) -> bool {
        self.0.lock().generation.as_deref() == Some(generation)
    }

    pub(crate) fn binding(&self, generation: &str) -> Option<u64> {
        let state = self.0.lock();
        (state.generation.as_deref() == Some(generation)).then_some(state.binding)
    }

    /// Linearize a narrow synchronous mutation against socket invalidation,
    /// suspension and binding retirement. The callback must not call transport
    /// methods or the desktop endpoint manager, nor await or invoke reentrant
    /// lifecycle callbacks. Non-reentrant telemetry enqueue is allowed.
    pub(crate) fn with_business_authority(
        &self,
        generation: &str,
        binding: u64,
        expected: &LeaseContext,
        action: &mut dyn FnMut(),
    ) -> bool {
        let state = self.0.lock();
        if !state.connected
            || state.suspended
            || state.generation.as_deref() != Some(generation)
            || state.binding != binding
            || state
                .lease
                .as_ref()
                .and_then(|runtime| runtime.lease.as_ref())
                != Some(expected)
        {
            return false;
        }
        action();
        true
    }

    pub(crate) fn retire_owned(&self, generation: &str, binding: u64) {
        let mut state = self.0.lock();
        if state.generation.as_deref() == Some(generation) && state.binding == binding {
            state.generation = None;
            state.lease = None;
            state.suspended = true;
        }
    }

    pub(crate) fn install_joined(
        &self,
        receipt: ControllerReadReceipt,
        context: &LeaseContext,
        capabilities: Vec<String>,
    ) -> bool {
        let mut state = self.0.lock();
        if !Arc::ptr_eq(&self.0, &receipt.authority.0)
            || !state.connected
            || state.epoch != receipt.identity.epoch
            || state.binding != receipt.identity.binding
            || state.generation.as_deref() != Some(context.remote_control_generation.as_str())
            || receipt.identity.generation != context.remote_control_generation
            || receipt.identity.kind != "controller_joined"
            || state.lease.is_some()
            || &receipt.identity.context != context
            || receipt.identity.consumed.load(Ordering::Relaxed)
        {
            return false;
        }
        state.lease = Some(RuntimeValidationContext {
            lease: Some(context.clone()),
            negotiated_capabilities: capabilities,
        });
        receipt.identity.consumed.store(true, Ordering::Relaxed);
        state.suspended = false;
        true
    }

    pub(crate) fn restore(
        &self,
        receipt: ControllerReadReceipt,
        context: &LeaseContext,
        activation_id: &str,
        request_id: &str,
    ) -> bool {
        let mut state = self.0.lock();
        if !Arc::ptr_eq(&self.0, &receipt.authority.0)
            || !state.connected
            || !state.suspended
            || state.epoch != receipt.identity.epoch
            || state.binding != receipt.identity.binding
            || state.generation.as_deref() != Some(context.remote_control_generation.as_str())
            || receipt.identity.generation != context.remote_control_generation
            || receipt.identity.kind != "controller_resumed"
            || !state
                .lease
                .as_ref()
                .is_some_and(|lease| lease.lease.as_ref() == Some(context))
            || &receipt.identity.context != context
            || receipt.identity.activation_id.as_deref() != Some(activation_id)
            || receipt.identity.request_id != request_id
            || receipt.identity.consumed.load(Ordering::Relaxed)
        {
            return false;
        }
        receipt.identity.consumed.store(true, Ordering::Relaxed);
        state.suspended = false;
        true
    }

    #[cfg(test)]
    pub(crate) fn outbound_scope(
        &self,
        frame: &Value,
    ) -> Result<Option<ControllerOutboundScope>, ()> {
        self.outbound_scope_owned(frame, None)
    }

    pub(crate) fn outbound_scope_owned(
        &self,
        frame: &Value,
        expected: Option<(&str, u64)>,
    ) -> Result<Option<ControllerOutboundScope>, ()> {
        let state = self.0.lock();
        if expected.is_some_and(|(generation, binding)| {
            state.generation.as_deref() != Some(generation)
                || state.binding != binding
                || frame
                    .get("remote_control_generation")
                    .is_some_and(|value| value.as_str() != Some(generation))
                || validate_controller_message_json(&frame.to_string(), SenderRole::ControlledB)
                    .is_err()
        }) {
            return Err(());
        }
        let business = frame["type"].as_str() == Some("encrypted_frame");
        let Some(generation) = state.generation.as_ref() else {
            // The standalone legacy transport has no controller lane. Do not
            // change v2 behavior; encrypted controller business is fail closed.
            return if business { Err(()) } else { Ok(None) };
        };
        // B freezes its gate immediately at disconnect and queues prepare
        // before the reconnect backoff finishes. Only this strict, saved-lease
        // prepare may target exactly the next socket; arbitrary old control or
        // business frames must never be retargeted/replayed on reconnect.
        let next_socket_prepare = !state.connected
            && frame["type"].as_str() == Some("controller_resume_prepare")
            && state.suspended
            && state
                .lease
                .as_ref()
                .and_then(|lease| lease.lease.as_ref())
                .is_some_and(|lease| resume_frame_matches_lease(lease, frame))
            && validate_controller_message_json(&frame.to_string(), SenderRole::ControlledB)
                .is_ok();
        if (!state.connected && !next_socket_prepare)
            || (business
                && (state.suspended
                    || validate_controller_runtime_message_json(
                        &frame.to_string(),
                        SenderRole::ControlledB,
                        state.lease.as_ref(),
                    )
                    .is_err()))
        {
            return Err(());
        }
        Ok(Some(ControllerOutboundScope {
            epoch: if next_socket_prepare {
                state.epoch.checked_add(1).ok_or(())?
            } else {
                state.epoch
            },
            generation: generation.clone(),
            binding: state.binding,
            business,
        }))
    }

    pub(crate) fn frame_is_current(&self, scope: &ControllerOutboundScope) -> bool {
        let state = self.0.lock();
        state.connected
            && state.epoch == scope.epoch
            && state.generation.as_deref() == Some(scope.generation.as_str())
            && state.binding == scope.binding
            && (!scope.business || (!state.suspended && state.lease.is_some()))
    }
}

impl SharedSocket {
    pub(crate) fn receipt(&self, frame: &Value) -> Option<ControllerReadReceipt> {
        let kind = frame["type"].as_str()?;
        if !matches!(kind, "controller_joined" | "controller_resumed")
            || validate_controller_message_json(&frame.to_string(), SenderRole::Relay).is_err()
        {
            return None;
        }
        let state = self.authority.0.lock();
        let generation = state.generation.as_ref()?;
        if !state.connected
            || state.epoch != self.epoch
            || frame["remote_control_generation"].as_str() != Some(generation.as_str())
        {
            return None;
        }
        if kind == "controller_resumed"
            && (!state.suspended
                || !state
                    .lease
                    .as_ref()
                    .and_then(|lease| lease.lease.as_ref())
                    .is_some_and(|lease| resume_frame_matches_lease(lease, frame)))
        {
            return None;
        }
        let identity = Arc::new(ReceiptIdentity {
            epoch: self.epoch,
            binding: state.binding,
            generation: generation.clone(),
            kind: kind.to_owned(),
            context: LeaseContext {
                connection_id: frame["connection_id"].as_str()?.to_owned(),
                controller_lease_id: frame["controller_lease_id"].as_str()?.to_owned(),
                lease_epoch: frame["lease_epoch"].as_str()?.to_owned(),
                relay_epoch: frame["relay_epoch"].as_str()?.to_owned(),
                remote_control_generation: generation.clone(),
            },
            activation_id: frame["activation_id"].as_str().map(str::to_owned),
            request_id: frame["request_id"].as_str()?.to_owned(),
            consumed: AtomicBool::new(false),
        });
        Some(ControllerReadReceipt {
            authority: self.authority.clone(),
            identity,
        })
    }
}
