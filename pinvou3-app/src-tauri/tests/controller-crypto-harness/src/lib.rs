//! Resource-bounded tests for controller authentication and local safety state.
//!
//! Actual production modules are compiled below without linking the full
//! Tauri/CodeWhale graph. The actual controller and legacy Relay transports
//! are compiled, including their loopback WebSocket and queue admission tests.
//! The platform capability projection is also a test double, not OS coverage.
//! Transfer tests compile the actual byte-budget and retry-evidence core; they
//! cover the upload identity port, but not real staging files, command
//! admission or production transfer wiring.
//! The full product compile and native transport tests remain separate gates.

#![allow(dead_code)]

#[path = "../../../src/features/remote_control/controller/protocol.rs"]
mod protocol;

#[path = "../../../src/features/remote_control/controller/crypto.rs"]
mod crypto;

#[path = "../../../src/features/remote_control/controller/liveness.rs"]
pub(crate) mod liveness;

#[cfg(test)]
#[path = "../../../src/features/remote_control/controller/resume.rs"]
mod resume;

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
mod features {
    pub(crate) mod remote_control {
        pub(crate) mod controller {
            pub(crate) use crate::state::{controlled, controlling};
            pub(crate) use crate::transfer;
            pub(crate) use crate::{liveness, protocol};
        }
        pub(crate) use crate::legacy_relay::relay_client;
    }
}

mod platform {
    pub(crate) fn desktop_control_supported() -> bool {
        cfg!(target_os = "windows")
    }
}
