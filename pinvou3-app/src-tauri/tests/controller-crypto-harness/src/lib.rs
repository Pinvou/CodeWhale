//! Resource-bounded tests for controller authentication and local safety state.
//!
//! Actual production modules are compiled below without linking the full
//! Tauri/CodeWhale graph. The actual controller and legacy Relay transports
//! are compiled, including their loopback WebSocket and queue admission tests.
//! Capability detection, telemetry and shell path normalization are host seams.
//! This is not complete OS/Tauri coverage. Task,
//! journal, byte-budget, transfer lifecycle, anchored private filesystem and
//! real staging tests compile their actual production modules. The periodic
//! reaper is exercised with a controlled clock and real Tokio interval.
//! Native command registration/startup is not linked by this harness.
//! The full product compile and native transport tests remain separate gates.

#![cfg(test)]
#![allow(dead_code)]

#[path = "../../../src/features/remote_control/controller/protocol.rs"]
mod protocol;

#[path = "../../../src/features/remote_control/controller/crypto.rs"]
mod crypto;

#[path = "../../../src/features/remote_control/controller/encrypted_lane.rs"]
mod encrypted_lane;

#[path = "../../../src/features/remote_control/controller/liveness.rs"]
pub(crate) mod liveness;

#[cfg(test)]
#[path = "../../../src/features/remote_control/controller/resume.rs"]
mod resume;

#[path = "../../../src/features/remote_control/controller/recovery_budget.rs"]
mod recovery_budget;

#[path = "../../../src/features/remote_control/controller/shared_transport.rs"]
mod shared_transport;

#[cfg(test)]
mod transfer;

#[cfg(test)]
mod state;

// Actual ACP instance fence and callback identity helpers; this is not a
// provider/child-process/EventBridge integration test.
#[cfg(test)]
#[path = "../../../src/features/codex_acp/lifecycle.rs"]
mod acp_lifecycle;

#[cfg(test)]
#[path = "../../../src/features/remote_control/controller/test_support.rs"]
mod test_support;

#[path = "../../../src/features/remote_control/protocol.rs"]
mod legacy_protocol;

#[cfg(test)]
mod legacy_relay;

#[cfg(test)]
#[path = "../../../src/features/remote_control/controller/relay_transport.rs"]
mod relay_transport;

// Explicit host seam: the product's spawn calls use Tokio here. This does not
// exercise Tauri initialization, commands, TLS deployment or CodeWhale wiring.
extern crate self as tauri;
pub mod async_runtime {
    pub fn spawn<F>(future: F) -> tokio::task::JoinHandle<F::Output>
    where
        F: std::future::Future + Send + 'static,
        F::Output: Send + 'static,
    {
        tokio::spawn(future)
    }
}

#[cfg(test)]
#[path = "../../../src/features/files/published_attachment.rs"]
mod published_attachment;

#[path = "../../../src/features/files/limits.rs"]
mod file_limits;

mod features {
    pub(crate) mod sessions {
        pub(crate) mod validators {
            // Explicit host seam: native tests cover SessionStore validation.
            pub(crate) fn validate_session_id(id: &str) -> anyhow::Result<()> {
                anyhow::ensure!(!id.is_empty() && id.bytes().all(|byte|
                    byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'_'));
                Ok(())
            }
        }
    }
    pub(crate) mod files {
        pub(crate) mod file_ingest {
            pub(crate) use crate::published_attachment::*;
            pub(crate) use crate::file_limits::MAX_FILE_BYTES;
        }
    }
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
            pub(crate) use crate::state::{controlled, controlling, task};
            pub(crate) mod relay_runtime {
                pub(crate) fn log_controlled_event(_event: &str, _outcome: &str) {}
                pub(crate) fn log_controller_event(_event: &str, _outcome: &str) {}
            }
            pub(crate) use crate::transfer;
            pub(crate) use crate::{
                liveness, protocol, relay_transport, resume, shared_transport, test_support,
            };
        }
        pub(crate) use crate::legacy_relay::relay_client;
    }
}

mod platform;
mod remote_archive;

pub(crate) mod relay_runtime {
    pub(crate) fn log_controlled_event(_event: &str, _outcome: &str) {}
    pub(crate) fn log_controller_event(_event: &str, _outcome: &str) {}
}
