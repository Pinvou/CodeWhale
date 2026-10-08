// Keep the actual manager's parent-module boundary while compiling its local
// state tests independently of the full product graph.
pub(crate) use crate::{crypto, liveness, protocol, relay_transport, resume};

#[path = "../../../src/features/remote_control/controller/controlled.rs"]
pub(crate) mod controlled;

#[path = "../../../src/features/remote_control/controller/controlling.rs"]
pub(crate) mod controlling;

#[path = "../../../src/features/remote_control/controller/capabilities.rs"]
pub(crate) mod capabilities;

#[path = "../../../src/features/remote_control/controller/turn_budget.rs"]
pub(crate) mod turn_budget;
