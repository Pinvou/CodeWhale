// Keep the actual manager's parent-module boundary while compiling its local
// state tests independently of the full product graph.
pub(crate) use crate::{crypto, encrypted_lane, liveness, protocol, relay_transport, resume};

#[path = "../../../src/features/remote_control/controller/controlled.rs"]
pub(crate) mod controlled;

#[path = "../../../src/features/remote_control/controller/controlling.rs"]
pub(crate) mod controlling;

#[path = "../../../src/features/remote_control/controller/controlling_session.rs"]
mod controlling_session;

#[path = "../../../src/features/remote_control/controller/permission.rs"]
pub(crate) mod permission;

// Compile the real task producer and journal consumer together: a fabricated
// event sequence cannot prove that production transitions reach the journal.
#[path = "../../../src/features/remote_control/controller/task.rs"]
pub(crate) mod task;

#[path = "../../../src/features/remote_control/controller/journal.rs"]
pub(crate) mod journal;

pub(crate) use crate::transfer;
