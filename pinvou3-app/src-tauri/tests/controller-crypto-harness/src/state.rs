// Keep the actual manager's parent-module boundary while compiling its local
// state tests independently of the full product graph.
pub(crate) use crate::{crypto, liveness, protocol, relay_transport, resume};

#[path = "../../../src/features/remote_control/controller/clock.rs"]
pub(crate) mod clock;

#[path = "../../../src/features/remote_control/controller/controlled.rs"]
pub(crate) mod controlled;

#[path = "../../../src/features/remote_control/controller/controlling.rs"]
pub(crate) mod controlling;

#[path = "../../../src/features/remote_control/controller/permission.rs"]
mod permission;

// Compile the real task producer and journal consumer together: a fabricated
// event sequence cannot prove that production transitions reach the journal.
#[path = "../../../src/features/remote_control/controller/task.rs"]
pub(crate) mod task;

#[path = "../../../src/features/remote_control/controller/journal.rs"]
pub(crate) mod journal;

#[path = "../../../src/features/remote_control/controller/redaction.rs"]
pub(crate) mod redaction;

#[path = "../../../src/features/remote_control/controller/capabilities.rs"]
pub(crate) mod capabilities;

#[path = "../../../src/features/remote_control/controller/turn_budget.rs"]
pub(crate) mod turn_budget;
