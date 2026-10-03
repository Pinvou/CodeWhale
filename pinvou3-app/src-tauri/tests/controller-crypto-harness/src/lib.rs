//! Resource-bounded tests for controller authentication and local safety state.
//!
//! Actual production modules are compiled below without linking the full
//! Tauri/CodeWhale graph. Transport is deliberately out of scope: its test
//! double models only installed/closed admission for local manager tests; it
//! cannot claim protocol queue, network, or runtime transport coverage.
//! Workspace identity uses the production OS adapter below. This is not
//! end-to-end filesystem or packaged application coverage.
//! Endpoint profile lookup supplies a non-production public-key fixture; actual
//! persisted/environment-selected deployment profiles are outside this harness.
//! Transfer tests compile the actual byte-budget and retry-evidence core; they
//! do not cover staging files, command admission or production transfer wiring.
//! The full product compile and native transport tests remain separate gates.

#![allow(dead_code)]

#[path = "../../../src/features/remote_control/controller/protocol.rs"]
mod protocol;

#[path = "../../../src/features/remote_control/controller/crypto.rs"]
mod crypto;

#[path = "../../../src/features/remote_control/controller/encrypted_lane.rs"]
mod encrypted_lane;

#[path = "../../../src/features/remote_control/controller/liveness.rs"]
pub(crate) mod liveness;

#[path = "../../../src/features/remote_control/controller/resume.rs"]
mod resume;

#[path = "../../../src/features/remote_control/controller/recovery_budget.rs"]
mod recovery_budget;

#[cfg(test)]
mod transfer;

mod state;

#[cfg(test)]
#[path = "../../../src/features/remote_control/controller/test_support.rs"]
mod test_support;

mod features {
    pub(crate) mod remote_control {
        mod platform {
            include!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../src/features/remote_control/platform/mod.rs"
            ));
        }

        #[derive(Debug, Clone, PartialEq, Eq)]
        pub(crate) struct HarnessWorkspaceIdentity(platform::WorkspaceIdentity);

        pub(crate) fn workspace_identity(
            path: &std::path::Path,
        ) -> std::io::Result<HarnessWorkspaceIdentity> {
            platform::workspace_identity(path).map(HarnessWorkspaceIdentity)
        }

        #[cfg(test)]
        pub(crate) fn test_workspace_identity(seed: u64) -> HarnessWorkspaceIdentity {
            HarnessWorkspaceIdentity(platform::test_workspace_identity(seed))
        }

        pub(crate) fn desktop_control_supported() -> bool {
            platform::desktop_control_supported()
        }

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
            #[cfg(test)]
            pub(crate) use crate::{liveness, protocol, test_support};
        }
    }
}

mod platform {
    pub(crate) use crate::features::remote_control::HarnessWorkspaceIdentity as WorkspaceIdentity;
    #[cfg(test)]
    pub(crate) use crate::features::remote_control::test_workspace_identity;
    pub(crate) use crate::features::remote_control::workspace_identity;
    pub(crate) fn desktop_control_supported() -> bool {
        crate::features::remote_control::desktop_control_supported()
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
    #[derive(Clone)]
    pub(crate) struct ControllerTransportSender {
        generation: String,
        authenticated: std::sync::Arc<std::sync::atomic::AtomicBool>,
        stopped: std::sync::Arc<std::sync::atomic::AtomicBool>,
    }

    pub(crate) struct AuthenticatedLease {
        pub(crate) context: crate::protocol::LeaseContext,
        pub(crate) negotiated_capabilities: Vec<String>,
    }

    #[cfg(test)]
    pub(crate) fn transport_channels(
        _role: ControllerRole,
        generation: &str,
        _control_capacity: usize,
        _business_capacity: usize,
        _total_byte_budget: usize,
        _reserved_control_bytes: usize,
    ) -> (ControllerTransportSender, ()) {
        (
            ControllerTransportSender {
                generation: generation.to_owned(),
                authenticated: std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false)),
                stopped: std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false)),
            },
            (),
        )
    }

    #[derive(Debug, Eq, PartialEq)]
    pub(crate) enum ControllerSendError {
        Unauthenticated,
        InvalidFrame(&'static str),
    }

    impl ControllerTransportSender {
        pub(crate) fn send_control_with_receipt<T: serde::Serialize>(
            &self,
            _frame: &T,
        ) -> Result<tokio::sync::oneshot::Receiver<()>, ControllerSendError> {
            if self.is_stopped() || !self.authenticated.load(std::sync::atomic::Ordering::SeqCst) {
                return Err(ControllerSendError::Unauthenticated);
            }
            // Admission-only: no delivery receipt is produced by this double.
            let (_sender, receiver) = tokio::sync::oneshot::channel();
            Ok(receiver)
        }

        pub(crate) fn try_send_sensitive_resume_control<T: serde::Serialize>(
            &self,
            _frame: &T,
        ) -> Result<(), ControllerSendError> {
            panic!("resume transport is outside this local-state harness");
        }

        pub(crate) fn install_authenticated_lease_after_resume(
            &self,
            _lease: AuthenticatedLease,
            _activation_id: &str,
        ) -> Result<(), ControllerSendError> {
            // This double has no socket epoch or one-shot final-frame proof.
            // It must never pretend that authenticated resume was validated.
            Err(ControllerSendError::Unauthenticated)
        }

        #[cfg(test)]
        pub(crate) fn install_authenticated_lease(
            &self,
            lease: AuthenticatedLease,
        ) -> Result<(), ControllerSendError> {
            if self.generation.is_empty()
                || lease.context.remote_control_generation != self.generation
            {
                return Err(ControllerSendError::InvalidFrame(
                    "remote_control_generation",
                ));
            }
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
            frame: &serde_json::Value,
        ) -> Result<(), ControllerSendError> {
            if self.is_stopped() || !self.authenticated.load(std::sync::atomic::Ordering::SeqCst) {
                return Err(ControllerSendError::Unauthenticated);
            }
            // Only malformed-empty rejection is modeled for the identity
            // guard test. A valid frame cannot claim any transport coverage.
            if frame.as_object().is_some_and(|fields| fields.is_empty()) {
                return Err(ControllerSendError::InvalidFrame("invalid_message"));
            }
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

    #[cfg(test)]
    mod tests {
        use super::*;

        #[test]
        fn admission_double_rejects_replaced_generation_without_installing_a_lease() {
            let (sender, _) =
                transport_channels(ControllerRole::ControllerA, "generation-1", 1, 1, 4096, 512);
            for (generation, expected) in [
                (
                    "generation-old",
                    Err(ControllerSendError::InvalidFrame(
                        "remote_control_generation",
                    )),
                ),
                ("generation-1", Ok(())),
            ] {
                let lease = AuthenticatedLease {
                    context: crate::protocol::LeaseContext {
                        connection_id: "connection-1".into(),
                        controller_lease_id: "lease-1".into(),
                        lease_epoch: "1".into(),
                        relay_epoch: "relay-1".into(),
                        remote_control_generation: generation.into(),
                    },
                    negotiated_capabilities: Vec::new(),
                };
                let installed = expected.is_ok();
                assert_eq!(sender.install_authenticated_lease(lease), expected);
                assert_eq!(
                    sender
                        .authenticated
                        .load(std::sync::atomic::Ordering::SeqCst),
                    installed
                );
            }
        }

        #[test]
        fn resume_install_double_never_grants_transport_authority() {
            for stopped in [false, true] {
                let (sender, _) = transport_channels(
                    ControllerRole::ControllerA,
                    "generation-harness",
                    1,
                    1,
                    1024,
                    512,
                );
                if stopped {
                    sender.stop();
                }
                let lease = AuthenticatedLease {
                    context: crate::protocol::LeaseContext {
                        connection_id: "connection-harness".into(),
                        controller_lease_id: "lease-harness".into(),
                        lease_epoch: "lease-epoch-harness".into(),
                        relay_epoch: "relay-epoch-harness".into(),
                        remote_control_generation: "generation-harness".into(),
                    },
                    negotiated_capabilities: Vec::new(),
                };
                assert_eq!(
                    sender.install_authenticated_lease_after_resume(lease, "activation-harness"),
                    Err(ControllerSendError::Unauthenticated)
                );
                assert!(
                    !sender
                        .authenticated
                        .load(std::sync::atomic::Ordering::SeqCst)
                );
                assert_eq!(
                    sender.try_send_authenticated_control(&serde_json::json!({})),
                    Err(ControllerSendError::Unauthenticated)
                );
            }
        }
    }
}
