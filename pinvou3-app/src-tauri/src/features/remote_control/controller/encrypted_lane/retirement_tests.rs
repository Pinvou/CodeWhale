use super::*;
use crate::features::remote_control::controller::test_support::{
    lease_context, paired_session_keys, pairing_fixture,
};

fn material() -> (super::super::protocol::LeaseContext, SessionKeys) {
    let fixture = pairing_fixture("retirement-identity");
    let lease = lease_context(&fixture.reservation);
    let (keys, _) = paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
    (lease, keys)
}

#[test]
fn terminal_guard_retires_keys_but_suspend_and_resume_do_not() {
    let state = ControllingLaneState::default();
    let (lease, keys) = material();
    state.install(ControllerRole::A, &lease, keys).unwrap();
    let retirement = state.retire_on_exit();
    state.suspend();
    assert!(state.is_installed());
    state.activate();
    assert!(state.is_installed());
    drop(retirement);
    assert!(!state.is_installed());
    assert!(!state.is_suspended());
    assert_eq!(
        state.seal_liveness(&json!({}), &[]),
        Err(LaneFailure::Invalid)
    );
}

#[test]
fn old_guard_preserves_even_same_lease_replacement_and_its_suspension() {
    let state = ControllingLaneState::default();
    let (lease, keys) = material();
    state.install(ControllerRole::A, &lease, keys).unwrap();
    let old = state.retire_on_exit();
    let (_, replacement_keys) = material();
    // Store-level replacement intentionally keeps the complete lease tuple.
    // This tests instance identity, not a new wire/handshake capability.
    state
        .install(ControllerRole::A, &lease, replacement_keys)
        .unwrap();
    state.suspend();
    drop(old);
    assert!(state.is_installed());
    assert!(state.is_suspended());
    drop(state.retire_on_exit());
    assert!(!state.is_installed());
}

#[test]
fn unpolled_handoff_future_still_owns_retirement() {
    let state = ControllingLaneState::default();
    let (lease, keys) = material();
    state.install(ControllerRole::A, &lease, keys).unwrap();
    let retirement = state.retire_on_exit();
    let future = async move {
        let _retirement = retirement;
        std::future::pending::<()>().await;
    };
    drop(future);
    assert!(!state.is_installed());
}

#[tokio::test]
async fn cancelled_polled_handoff_retires_without_waiting_for_next_tick() {
    let state = ControllingLaneState::default();
    let (lease, keys) = material();
    state.install(ControllerRole::A, &lease, keys).unwrap();
    let retirement = state.retire_on_exit();
    let (entered, observed) = tokio::sync::oneshot::channel();
    let worker = tokio::spawn(async move {
        let _retirement = retirement;
        entered.send(()).unwrap();
        std::future::pending::<()>().await;
    });
    observed.await.unwrap();
    assert!(state.is_installed());
    worker.abort();
    assert!(worker.await.unwrap_err().is_cancelled());
    assert!(!state.is_installed());
}

#[test]
fn empty_and_repeated_old_cleanup_cannot_retire_a_future_installation() {
    let state = ControllingLaneState::default();
    let empty = state.retire_on_exit();
    let (lease, keys) = material();
    state.install(ControllerRole::A, &lease, keys).unwrap();
    drop(empty);
    assert!(state.is_installed());
    let old1 = state.retire_on_exit();
    let old2 = state.retire_on_exit();
    drop(old1);
    let (_, keys) = material();
    state.install(ControllerRole::A, &lease, keys).unwrap();
    drop(old2);
    assert!(state.is_installed());
}
