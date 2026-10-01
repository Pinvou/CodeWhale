//! Resource-bounded tests for controller authentication and local safety state.
//!
//! Actual production modules are compiled below without linking the full
//! Tauri/CodeWhale graph. Transport is deliberately out of scope: its test
//! double models only installed/closed admission for local manager tests; it
//! cannot claim protocol queue, network, or runtime transport coverage.
//! The platform capability projection is also a test double, not OS coverage.
//! Endpoint profile lookup supplies a non-production public-key fixture; actual
//! persisted/environment-selected deployment profiles are outside this harness.
//! The full product compile and native transport tests remain separate gates.

#![allow(dead_code)]

#[path = "../../../src/features/remote_control/controller/protocol.rs"]
mod protocol;

#[path = "../../../src/features/remote_control/controller/crypto.rs"]
mod crypto;

#[path = "../../../src/features/remote_control/controller/liveness.rs"]
pub(crate) mod liveness;

#[path = "../../../src/features/remote_control/controller/resume.rs"]
mod resume;

#[path = "../../../src/features/remote_control/controller/recovery_budget.rs"]
mod recovery_budget;

mod state;

#[cfg(test)]
#[path = "../../../src/features/remote_control/controller/test_support.rs"]
mod test_support;

mod features {
    pub(crate) mod remote_control {
        pub(crate) mod manager {
            pub(crate) struct HarnessRelayTrustProfile {
                pub(crate) signing_key_id: &'static str,
                pub(crate) signing_public_key_b64: &'static str,
            }

            // Non-production public key fixture: this adapter tests the crypto
            // loader's fallback shape, not deployment profile selection.
            pub(crate) fn active_endpoint_profile() -> HarnessRelayTrustProfile {
                HarnessRelayTrustProfile {
                    signing_key_id: "relay-harness-default-key",
                    signing_public_key_b64: "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=",
                }
            }
        }

        pub(crate) mod controller {
            pub(crate) use crate::liveness;
        }
    }
}

mod platform {
    pub(crate) fn desktop_control_supported() -> bool {
        cfg!(target_os = "windows")
    }
}

mod relay_transport {
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub(crate) enum ControllerRole {
        ControllerA,
        ControlledB,
    }

    // Admission-only test double for manager lifecycle tests. It does not
    // validate frames, enqueue bytes, start workers or communicate over a socket.
    pub(crate) struct ControllerTransportSender {
        authenticated: std::sync::atomic::AtomicBool,
        stopped: std::sync::atomic::AtomicBool,
    }

    pub(crate) struct AuthenticatedLease {
        pub(crate) context: crate::protocol::LeaseContext,
        pub(crate) negotiated_capabilities: Vec<String>,
    }

    #[cfg(test)]
    pub(crate) fn transport_channels(
        _role: ControllerRole,
        _generation: &str,
        _control_capacity: usize,
        _business_capacity: usize,
        _total_byte_budget: usize,
        _reserved_control_bytes: usize,
    ) -> (ControllerTransportSender, ()) {
        (
            ControllerTransportSender {
                authenticated: std::sync::atomic::AtomicBool::new(false),
                stopped: std::sync::atomic::AtomicBool::new(false),
            },
            (),
        )
    }

    #[derive(Debug)]
    pub(crate) enum ControllerSendError {
        Unauthenticated,
    }

    impl ControllerTransportSender {
        pub(crate) fn try_send_sensitive_resume_control<T: serde::Serialize>(
            &self,
            _frame: &T,
        ) -> Result<(), ControllerSendError> {
            panic!("resume transport is outside this local-state harness");
        }

        #[cfg(test)]
        pub(crate) fn install_authenticated_lease(
            &self,
            _lease: AuthenticatedLease,
        ) -> Result<(), ControllerSendError> {
            self.authenticated
                .store(true, std::sync::atomic::Ordering::SeqCst);
            Ok(())
        }

        pub(crate) fn try_send_authenticated_control(
            &self,
            _frame: &serde_json::Value,
        ) -> Result<(), ControllerSendError> {
            if self.is_stopped() || !self.authenticated.load(std::sync::atomic::Ordering::SeqCst) {
                return Err(ControllerSendError::Unauthenticated);
            }
            Ok(())
        }

        pub(crate) fn try_send_business(
            &self,
            _frame: &serde_json::Value,
        ) -> Result<(), ControllerSendError> {
            panic!("transport is outside this local-state harness");
        }

        pub(crate) fn stop(&self) {
            self.stopped
                .store(true, std::sync::atomic::Ordering::SeqCst);
        }

        pub(crate) fn is_stopped(&self) -> bool {
            self.stopped.load(std::sync::atomic::Ordering::SeqCst)
        }
    }
}
