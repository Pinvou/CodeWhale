// Keep the actual manager's parent-module boundary while compiling its local
// state tests independently of the full product graph.
pub(crate) use crate::{crypto, liveness, protocol, relay_transport, resume};

#[path = "../../../src/features/remote_control/controller/controlled.rs"]
mod controlled;

#[path = "../../../src/features/remote_control/controller/controlling.rs"]
mod controlling;

#[path = "../../../src/features/remote_control/controller/controlling_session.rs"]
mod controlling_session;

#[path = "../../../src/features/remote_control/controller/permission.rs"]
mod permission;

// Only the narrow hook trait is doubled; permission authority and all its
// tests are actual production code. This does not cover task-ledger wiring.
mod task {
    pub(crate) trait RemoteTaskPermissionScope: Send + Sync {
        fn deny_pending_for_task(&self, task_id: &str) -> usize;
    }
}
