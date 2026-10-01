// Keep the actual manager's parent-module boundary while compiling its local
// state tests independently of the full product graph.
pub(crate) use crate::{crypto, liveness, protocol, relay_transport, resume};

#[path = "../../../src/features/remote_control/controller/controlled.rs"]
mod controlled;

#[path = "../../../src/features/remote_control/controller/controlling.rs"]
mod controlling;
