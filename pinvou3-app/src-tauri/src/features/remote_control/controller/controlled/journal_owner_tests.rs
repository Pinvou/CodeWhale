use super::super::journal::JournalKind;
use super::super::task::{RemoteTaskEvent, RemoteTaskModelSource};
use super::tests::{enter_handshake, lease, register};
use super::*;
use serde_json::json;
use std::sync::mpsc;
use std::time::Duration;

fn connect(manager: &ControlledDeviceManager) -> (ControlledOperation, JournalOwner) {
    // A live generation cannot be started over: use its real stop transition
    // before issuing the successor, including while a stale consumer is paused.
    manager.stop();
    let operation = manager.start(None, Vec::new()).unwrap();
    register(manager, &operation);
    enter_handshake(manager, &operation);
    manager
        .complete_connected(operation.clone(), lease(operation.generation()))
        .unwrap();
    let (captured_lease, owner) = manager.local_journal_context().unwrap();
    assert_eq!(
        captured_lease.remote_control_generation,
        operation.generation()
    );
    (operation, owner)
}

fn append(journal: &RemoteJournal, owner: &JournalOwner, event: &str, session: &str) {
    journal
        .append_for_owner(
            owner,
            event,
            JournalKind::UserMessage,
            session,
            None,
            json!({"text":event}),
        )
        .unwrap();
}

#[test]
fn journal_owner_late_worker_after_66_actual_generations_cannot_change_current_state() {
    let manager = ControlledDeviceManager::default();
    let journal = manager.journal();
    let (old_operation, old_owner) = connect(&manager);
    append(&journal, &old_owner, "old-event", "b-session");
    let old_epoch = journal
        .snapshot_for_owner(&old_owner, "b-session")
        .unwrap()
        .epoch;
    let (entered_tx, entered_rx) = mpsc::sync_channel(1);
    let (release_tx, release_rx) = mpsc::sync_channel(1);
    let paused_journal = journal.clone();
    let paused_owner = old_owner.clone();
    let paused_epoch = old_epoch.clone();
    // Domain-level barrier after real manager capture, before the Journal
    // lock. The authenticated native host/worker chain is a separate gate.
    let worker = std::thread::spawn(move || {
        entered_tx.send(()).unwrap();
        release_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        paused_journal.replay_after_for_owner(&paused_owner, "b-session", &paused_epoch, 0)
    });
    entered_rx.recv_timeout(Duration::from_secs(2)).unwrap();
    let mut current_owner = old_owner.clone();
    for index in 0..66 {
        let (_, owner) = connect(&manager);
        append(&journal, &owner, &format!("successor-{index}"), "b-session");
        current_owner = owner;
    }
    append(&journal, &current_owner, "second-session", "b-second");
    let before = journal.owner_test_state(&current_owner);
    release_tx.send(()).unwrap();
    assert_eq!(
        worker.join().unwrap().unwrap_err().code,
        "journal_generation_stale"
    );
    assert!(
        journal
            .append_for_owner(
                &old_owner,
                "late",
                JournalKind::UserMessage,
                "b-session",
                None,
                json!({"text":"late"})
            )
            .is_err()
    );
    assert!(
        journal
            .append_terminal_for_owner(
                &old_owner,
                "late-terminal",
                "b-session",
                "old-task",
                "succeeded",
                json!({"status":"succeeded"})
            )
            .is_err()
    );
    let terminal = RemoteTaskEvent {
        task_id: "old-task".into(),
        session_id: "b-session".into(),
        generation: old_operation.generation().into(),
        model_source: RemoteTaskModelSource::ControlledDevice,
        status: "succeeded".into(),
        sequence: "1".into(),
        transition_sequence: 2,
        outcome_unknown: false,
        terminal_reason: None,
        result_digest: None,
    };
    assert!(
        journal
            .record_task_event_for_owner(&old_owner, &terminal)
            .is_none()
    );
    assert!(journal.snapshot_for_owner(&old_owner, "b-session").is_err());
    assert!(
        journal
            .acknowledge_for_owner(&old_owner, "b-session", &old_epoch, 1)
            .is_err()
    );
    let current = journal
        .snapshot_for_owner(&current_owner, "b-session")
        .unwrap();
    assert!(
        journal
            .require_current_event_for_owner(&old_owner, &current.events[0])
            .is_err()
    );
    assert_eq!(journal.owner_test_state(&current_owner), before);
    // Rejection must leave the next valid call and both current sessions usable.
    append(&journal, &current_owner, "next-valid", "b-session");
    assert_eq!(
        journal
            .snapshot_for_owner(&current_owner, "b-session")
            .unwrap()
            .sequence,
        2
    );
    assert_eq!(
        journal
            .snapshot_for_owner(&current_owner, "b-second")
            .unwrap()
            .sequence,
        1
    );
}

#[test]
fn journal_owner_never_used_old_handle_cannot_initialize_successor_payload() {
    let manager = ControlledDeviceManager::default();
    let journal = manager.journal();
    let (_, old_owner) = connect(&manager);
    let (_, current) = connect(&manager);
    let before = journal.owner_test_state(&current);
    assert!(journal.snapshot_for_owner(&old_owner, "b-session").is_err());
    assert!(
        journal
            .append_for_owner(
                &old_owner,
                "late",
                JournalKind::UserMessage,
                "b-session",
                None,
                json!({"text":"late"})
            )
            .is_err()
    );
    assert_eq!(journal.owner_test_state(&current), before);
    append(&journal, &current, "first-current", "b-session");
    assert_eq!(
        journal
            .snapshot_for_owner(&current, "b-session")
            .unwrap()
            .sequence,
        1
    );
}

#[test]
fn journal_owner_refresh_and_stop_revoke_before_cleanup_or_successor_use() {
    for refresh in [true, false] {
        let manager = ControlledDeviceManager::default();
        let journal = manager.journal();
        let (_, old_owner) = connect(&manager);
        append(&journal, &old_owner, "old", "b-session");
        if refresh {
            manager.begin_refresh().unwrap();
            assert_eq!(
                manager.status().status,
                ControlledStatusKind::RetiringGeneration
            );
        } else {
            manager.stop();
        }
        let before = journal.owner_test_state(&old_owner);
        assert!(manager.local_journal_context().is_err());
        assert!(journal.snapshot_for_owner(&old_owner, "b-session").is_err());
        assert_eq!(journal.owner_test_state(&old_owner), before);
        let (_, successor) = connect(&manager);
        append(&journal, &successor, "new", "b-session");
        assert!(journal.snapshot_for_owner(&old_owner, "b-session").is_err());
        assert_eq!(
            journal
                .snapshot_for_owner(&successor, "b-session")
                .unwrap()
                .sequence,
            1
        );
    }
}

#[test]
fn journal_owner_drop_revokes_even_while_a_consumer_retains_the_view() {
    let (journal, owner) = {
        let manager = ControlledDeviceManager::default();
        let (_, owner) = connect(&manager);
        let journal = manager.journal();
        append(&journal, &owner, "before-drop", "b-session");
        (journal, owner)
    };
    let before = journal.owner_test_state(&owner);
    assert!(journal.snapshot_for_owner(&owner, "b-session").is_err());
    assert_eq!(journal.owner_test_state(&owner), before);
}

#[test]
fn journal_owner_different_managers_with_identical_lease_text_remain_isolated() {
    let first = ControlledDeviceManager::default();
    let (operation, first_owner) = connect(&first);
    let second = ControlledDeviceManager::default();
    let mut duplicate = second.start(None, Vec::new()).unwrap();
    // Force the normally-random generation equal solely to test that text is
    // insufficient. Both owners still come through the real manager transition.
    duplicate.generation = operation.generation().into();
    second.inner.lock().generation = Some(duplicate.generation.clone());
    register(&second, &duplicate);
    enter_handshake(&second, &duplicate);
    second
        .complete_connected(duplicate.clone(), lease(duplicate.generation()))
        .unwrap();
    let (_, second_owner) = second.local_journal_context().unwrap();
    assert_eq!(first.live_lease().unwrap(), second.live_lease().unwrap());
    let journal = second.journal();
    append(&journal, &second_owner, "second-owner", "b-session");
    let before = journal.owner_test_state(&second_owner);
    assert!(
        journal
            .snapshot_for_owner(&first_owner, "b-session")
            .is_err()
    );
    assert_eq!(journal.owner_test_state(&second_owner), before);
    append(&journal, &second_owner, "next-valid", "b-session");
    assert_eq!(
        journal
            .snapshot_for_owner(&second_owner, "b-session")
            .unwrap()
            .sequence,
        2
    );
}
