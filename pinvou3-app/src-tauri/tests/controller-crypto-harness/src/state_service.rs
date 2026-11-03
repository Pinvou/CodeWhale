// Harness host for the actual private production dispatcher module.
fn lane_monotonic_millis() -> u64 {
    super::clock::monotonic_millis()
}
#[path = "../../../src/features/remote_control/controller/controlling_dispatch.rs"]
mod dispatch;
