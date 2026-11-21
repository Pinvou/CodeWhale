use super::*;
use std::time::Duration;

fn scope() -> InputTurnGuard {
    let guard = RemoteInputTurn::register(
        &uuid::Uuid::new_v4().to_string(),
        &uuid::Uuid::new_v4().to_string(),
        1,
        Arc::new(RemoteTurnOwner::default()),
        CancellationToken::new(),
        TurnBudget::for_test(Duration::from_secs(60)),
    )
    .unwrap();
    bind_engine_turn(&guard.0.session_id, &guard.0.task_id, 1, "turn-native").unwrap();
    guard
}
fn questions() -> Value {
    json!([{"id":"__proto__","header":"Pick","question":"Choose a report","multi_select":true,
        "options":[{"label":"A","description":"First"},{"label":"B","description":"Second"}]}])
}
fn record(turn: &RemoteInputTurn, tool: &str) -> String {
    let engine_turn = turn.state.lock().engine_turn_id.clone().unwrap();
    record_for_turn(
        &turn.session_id,
        turn.generation,
        &engine_turn,
        tool,
        questions(),
    )
    .unwrap();
    turn.pending()
        .unwrap()
        .into_iter()
        .find(|view| {
            view.input_id
                == turn
                    .state
                    .lock()
                    .questions
                    .iter()
                    .find(|q| q.tool_id == tool)
                    .unwrap()
                    .input_id
        })
        .unwrap()
        .input_id
}
fn answer() -> Vec<InputAnswer> {
    vec![InputAnswer {
        question_id: "0".into(),
        option_ids: vec!["0".into(), "1".into()],
        custom_text: None,
    }]
}

#[test]
fn producer_questions_keep_opaque_ids_and_exact_native_multi_answers_once() {
    let guard = scope();
    let turn = &guard.0;
    let id = record(turn, "call-1");
    let view = turn.pending().unwrap();
    assert_eq!(view[0].questions[0].id, "0");
    assert_eq!(view[0].questions[0].options[1].value, "1");
    assert!(!serde_json::to_string(&view).unwrap().contains("__proto__"));
    assert!(valid_requests(&view));
    let fixture: Value = serde_json::from_str(include_str!(
        "../../../../../remote-control-relay/test/fixtures/controller-user-input.json"
    ))
    .unwrap();
    let mut normalized = serde_json::to_value(&view).unwrap();
    normalized[0]["input_id"] = fixture["requests"][0]["input_id"].clone();
    assert_eq!(
        normalized, fixture["requests"],
        "committed browser DTO matches the actual native producer except its fresh nonce"
    );
    if let Ok(path) = std::env::var("PINVOU_REMOTE_USER_INPUT_PROJECTION_FILE") {
        std::fs::write(path, serde_json::to_vec(&json!({"requests":view})).unwrap()).unwrap();
    }
    turn.respond(&id, false, &answer(), |tool, response| {
        assert_eq!(tool, "call-1");
        let response = response.unwrap();
        assert_eq!(response.answers.len(), 2);
        assert_eq!(response.answers[0].id, "__proto__");
        assert_eq!(response.answers[0].value, "A");
        assert_eq!(response.answers[1].value, "B");
        Ok(())
    })
    .unwrap();
    assert!(turn.pending().unwrap().is_empty());
    turn.respond(&id, false, &answer(), |_, _| {
        panic!("replay cannot enqueue twice")
    })
    .unwrap();
    assert_eq!(
        turn.respond(&id, true, &[], |_, _| panic!()).unwrap_err(),
        "user_input_already_answered"
    );
    record_for_turn(&turn.session_id, 1, "turn-native", "call-1", questions()).unwrap();
    assert!(
        turn.pending().unwrap().is_empty(),
        "callback redelivery cannot reopen a consumed question"
    );
}

#[test]
fn cancellation_closing_and_successor_reject_old_nonce() {
    let guard = scope();
    let old = guard.0.clone();
    let id = record(&old, "reused-call");
    let task = old.task_id.clone();
    let owner = old.owner.clone();
    drop(guard);
    assert_eq!(
        old.respond(&id, false, &answer(), |_, _| panic!())
            .unwrap_err(),
        "user_input_stale"
    );
    let next = RemoteInputTurn::register(
        &old.session_id,
        &task,
        1,
        Arc::new(RemoteTurnOwner::default()),
        CancellationToken::new(),
        TurnBudget::for_test(Duration::from_secs(60)),
    )
    .unwrap();
    bind_engine_turn(&old.session_id, &task, 1, "turn-next").unwrap();
    assert!(
        record_for_turn(
            &old.session_id,
            1,
            "turn-native",
            "reused-call",
            questions()
        )
        .is_err(),
        "old source events cannot bind to a successor that reused its numeric generation"
    );
    let next_id = record(&next.0, "reused-call");
    assert_ne!(id, next_id);
    assert!(RemoteInputTurn::for_owner(&old.session_id, &owner).is_err());
    assert!(RemoteInputTurn::for_task("other-session", &task).is_err());
    assert_eq!(
        next.0
            .respond(&id, false, &answer(), |_, _| panic!())
            .unwrap_err(),
        "user_input_stale"
    );
    assert_eq!(next.0.pending().unwrap().len(), 1);
    next.0.cancel.cancel();
    assert_eq!(
        next.0
            .respond(&next_id, false, &answer(), |_, _| panic!())
            .unwrap_err(),
        "user_input_stale"
    );
}

#[tokio::test(start_paused = true)]
async fn deadline_expires_without_refresh_from_reads_or_answers() {
    let guard = scope();
    let id = record(&guard.0, "call");
    tokio::time::advance(Duration::from_secs(59)).await;
    assert_eq!(guard.0.pending().unwrap().len(), 1);
    tokio::time::advance(Duration::from_secs(1)).await;
    assert!(guard.0.pending().is_err());
    assert_eq!(
        guard
            .0
            .respond(&id, false, &answer(), |_, _| panic!())
            .unwrap_err(),
        "user_input_stale"
    );
}

#[test]
fn failed_enqueue_is_retryable_and_local_cancellation_closes_a_question() {
    let guard = scope();
    let turn = &guard.0;
    let id = record(turn, "call");
    assert_eq!(
        turn.respond(&id, false, &answer(), |_, _| Err("user_input_busy".into()))
            .unwrap_err(),
        "user_input_busy"
    );
    assert_eq!(turn.pending().unwrap().len(), 1);
    turn.respond_local("call", None, |tool, response| {
        assert_eq!(tool, "call");
        assert!(response.is_none());
        Ok(())
    })
    .unwrap();
    assert!(turn.pending().unwrap().is_empty());
    assert_eq!(
        turn.respond(&id, false, &answer(), |_, _| panic!())
            .unwrap_err(),
        "user_input_already_answered"
    );
    turn.respond_local("call", None, |_, _| {
        panic!("local replay cannot enqueue twice")
    })
    .unwrap();
    let next = record(turn, "next-call");
    turn.respond(&next, true, &[], |_, response| {
        assert!(response.is_none());
        Ok(())
    })
    .unwrap();
}

#[test]
fn unknown_questions_options_duplicates_and_conflicting_answers_have_no_effect() {
    let guard = scope();
    let turn = &guard.0;
    let id = record(turn, "call");
    for answers in [
        vec![],
        vec![InputAnswer {
            question_id: "2".into(),
            option_ids: vec!["0".into()],
            custom_text: None,
        }],
        vec![InputAnswer {
            question_id: "0".into(),
            option_ids: vec!["3".into()],
            custom_text: None,
        }],
        vec![InputAnswer {
            question_id: "0".into(),
            option_ids: vec!["0".into(), "0".into()],
            custom_text: None,
        }],
    ] {
        assert_eq!(
            turn.respond(&id, false, &answers, |_, _| panic!())
                .unwrap_err(),
            "user_input_invalid"
        );
    }
    assert!(turn.respond(&id, true, &answer(), |_, _| panic!()).is_err());
    assert_eq!(turn.pending().unwrap().len(), 1);
    let custom = vec![InputAnswer {
        question_id: "0".into(),
        option_ids: vec![],
        custom_text: Some("Custom choice".into()),
    }];
    turn.respond(&id, false, &custom, |_, response| {
        assert_eq!(response.unwrap().answers[0].value, "Custom choice");
        Ok(())
    })
    .unwrap();
}

#[test]
fn pending_capacity_and_retained_identity_do_not_block_the_next_question() {
    let guard = scope();
    let turn = &guard.0;
    for n in 0..8 {
        record(turn, &format!("call-{n}"));
    }
    assert_eq!(turn.pending().unwrap().len(), 8);
    assert_eq!(
        turn.record("overflow", questions()).unwrap_err(),
        "user_input_unavailable"
    );
    let first = turn.pending().unwrap()[0].input_id.clone();
    turn.respond(&first, true, &[], |_, _| Ok(())).unwrap();
    record(turn, "next");
    assert_eq!(turn.pending().unwrap().len(), 8);
    assert!(
        turn.state
            .lock()
            .questions
            .iter()
            .filter(|q| q.receipt.is_some())
            .all(|q| q.raw.is_none() && q.view.is_none())
    );
}

#[test]
fn producer_rejects_oversized_or_ambiguous_questions_without_registering() {
    let guard = scope();
    let turn = &guard.0;
    let mut oversized = questions();
    oversized[0]["question"] = json!("x".repeat(2049));
    assert!(turn.record("long", oversized).is_err());
    let q = questions()[0].clone();
    assert!(turn.record("duplicate-ids", json!([q.clone(), q])).is_err());
    assert!(turn.pending().unwrap().is_empty());
    record(turn, "valid-after-errors");
    finish_question(
        &turn.session_id,
        turn.generation,
        "turn-native",
        "valid-after-errors",
    );
    assert!(turn.pending().unwrap().is_empty());
}

#[test]
fn concurrent_a_answer_and_b_cancel_have_one_mailbox_winner() {
    use std::sync::{
        Barrier,
        atomic::{AtomicUsize, Ordering},
    };
    let guard = scope();
    let id = record(&guard.0, "call");
    let barrier = Arc::new(Barrier::new(3));
    let calls = Arc::new(AtomicUsize::new(0));
    let a = {
        let turn = guard.0.clone();
        let barrier = barrier.clone();
        let calls = calls.clone();
        std::thread::spawn(move || {
            barrier.wait();
            turn.respond(&id, false, &answer(), |_, _| {
                calls.fetch_add(1, Ordering::SeqCst);
                Ok(())
            })
        })
    };
    let b = {
        let turn = guard.0.clone();
        let barrier = barrier.clone();
        let calls = calls.clone();
        std::thread::spawn(move || {
            barrier.wait();
            turn.respond_local("call", None, |_, _| {
                calls.fetch_add(1, Ordering::SeqCst);
                Ok(())
            })
        })
    };
    barrier.wait();
    let results = [a.join().unwrap(), b.join().unwrap()];
    assert_eq!(results.iter().filter(|r| r.is_ok()).count(), 1);
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert!(guard.0.pending().unwrap().is_empty());
    record(&guard.0, "next-call");
    assert_eq!(
        guard.0.pending().unwrap().len(),
        1,
        "the next question remains usable after the race"
    );
}
