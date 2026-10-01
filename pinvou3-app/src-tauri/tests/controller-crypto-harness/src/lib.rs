//! Resource-bounded tests for controller authentication and local safety state.
//!
//! Actual production modules are compiled below without linking the full
//! Tauri/CodeWhale graph. Transport is deliberately out of scope: its test
//! double panics on use so this harness cannot claim network/runtime coverage.
//! The platform capability projection is also a test double, not OS coverage.
//! Transfer tests compile the actual byte-budget and retry-evidence core; they
//! do not cover staging files, command admission or production transfer wiring.
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

#[cfg(test)]
mod transfer;

mod state;

#[cfg(test)]
mod features {
    pub(crate) mod remote_control {
        pub(crate) mod controller {
            pub(crate) use crate::{liveness, protocol};
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

    pub(crate) struct ControllerTransportSender;

    impl ControllerTransportSender {
        pub(crate) fn stop(&self) {
            panic!("transport is outside this local-state harness");
        }

        pub(crate) fn is_stopped(&self) -> bool {
            panic!("transport is outside this local-state harness");
        }
    }
}
