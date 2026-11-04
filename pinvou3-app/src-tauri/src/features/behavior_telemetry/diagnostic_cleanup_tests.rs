use super::*;
use diagnostics::enrich_lock_probe::{Point, with_hook};
use std::sync::mpsc::{Receiver, SyncSender, sync_channel};
use std::thread::{self, JoinHandle};

// Channels are rendezvous points, not timing guesses. The timeout only bounds
// an unexpected deadlock; consent transitions use the real collection API.
struct Paused<T> {
    entered: Receiver<()>,
    release: Option<SyncSender<()>>,
    worker: Option<JoinHandle<T>>,
}

impl<T: Send + 'static> Paused<T> {
    fn start(point: Point, run: impl FnOnce() -> T + Send + 'static) -> Self {
        let (entered_tx, entered) = sync_channel(0);
        let (release, release_rx) = sync_channel(0);
        let worker = thread::spawn(move || {
            with_hook(
                point,
                move || {
                    entered_tx.send(()).unwrap();
                    release_rx.recv_timeout(Duration::from_secs(30)).unwrap();
                },
                run,
            )
        });
        Self {
            entered,
            release: Some(release),
            worker: Some(worker),
        }
    }

    fn wait(&self) {
        self.entered.recv_timeout(Duration::from_secs(30)).unwrap();
    }

    fn resume(mut self) -> T {
        self.release.take().unwrap().send(()).unwrap();
        self.worker.take().unwrap().join().unwrap()
    }
}

impl<T> Drop for Paused<T> {
    fn drop(&mut self) {
        if let Some(release) = self.release.take() {
            let _ = release.send(());
        }
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

fn assert_empty_diagnostics(telemetry: &BehaviorTelemetry) {
    let memory = serde_json::to_value(&*telemetry.diagnostics.lock().unwrap()).unwrap();
    let disk: Value = serde_json::from_slice(
        &std::fs::read(paths::pinvou3_home().join("failure-diagnostics.json")).unwrap(),
    )
    .unwrap();
    assert!(
        [&memory, &disk]
            .into_iter()
            .all(|state| state["contexts"].as_object().unwrap().is_empty()
                && state["incidents"].as_array().unwrap().is_empty()),
        "memory={memory}; disk={disk}",
    );
}

#[tokio::test(flavor = "current_thread")]
async fn diagnostic_disable_before_lock_rejects_late_install_and_allows_next_generation() {
    let _home = TestHome::new();
    let (base, server) = mock_http(vec![
        (200, r#"{"collection_period_id":"installation_period"}"#),
        (200, r#"{"collection_period_id":"after_disable_period"}"#),
    ])
    .await;
    let runtime = test_runtime(&base);
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let install = super::install::guard(&telemetry, "feishu");
    let old_operation = install.operation_id.clone();
    let old_generation = install.authorization_generation.unwrap();
    let error = "connection timeout PRIVATE_INSTALL_DETAILS";
    let paused = Paused::start(Point::BeforeLock, move || {
        install.finish(Err::<Value, _>(error.into()))
    });
    paused.wait();
    let stopped = telemetry.set_user_collection_enabled(false).await.unwrap();
    assert!(!stopped.enabled);
    assert_empty_diagnostics(&telemetry);
    assert_eq!(paused.resume(), Err(error.into()));
    // Check local memory and the real file, not just upload/API filtering.
    assert_empty_diagnostics(&telemetry);
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 0);

    telemetry.set_user_collection_enabled(true).await.unwrap();
    let lease = telemetry.current_lease().unwrap();
    assert!(lease.generation > old_generation);
    assert_eq!(lease.collection_period_id, "after_disable_period");
    assert_eq!(
        super::install::guard(&telemetry, "feishu").finish(Ok(json!({"ok":true}))),
        Ok(json!({"ok":true}))
    );
    assert_eq!(
        super::install::guard(&telemetry, "feishu").finish(Err::<(), _>(error.into())),
        Err(error.into())
    );
    let outbox = telemetry.outbox.lock().unwrap();
    let terminals = outbox
        .records
        .iter()
        .filter(|r| r.event["event_name"] == "tool_install_completed")
        .collect::<Vec<_>>();
    assert_eq!(terminals.len(), 2);
    assert!(terminals.iter().any(|r| r.event["success"] == true));
    assert!(terminals.iter().any(|r| r.event["success"] == false));
    assert!(
        outbox
            .records
            .iter()
            .all(|r| r.generation == lease.generation
                && r.collection_period_id == lease.collection_period_id
                && r.event["operation_id"] != old_operation)
    );
    drop(outbox);
    let memory = serde_json::to_value(&*telemetry.diagnostics.lock().unwrap()).unwrap();
    let disk =
        std::fs::read_to_string(paths::pinvou3_home().join("failure-diagnostics.json")).unwrap();
    assert!(!memory.to_string().contains(&old_operation));
    assert!(!disk.contains(&old_operation));
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 2);
    assert!(requests[0].starts_with("POST /collection/end "));
    assert!(requests[1].starts_with("POST /collection/start "));
}

#[test]
fn diagnostic_observer_holding_lock_is_cleared_before_disable_returns() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let observer = telemetry.clone();
    // A generic task producer exercises the same shared boundary as installs.
    let paused = Paused::start(Point::Locked, move || {
        observer.track(diagnostic_failure(
            "generic_cleanup",
            "generic_cleanup_turn",
        ));
    });
    paused.wait();
    let stopper = telemetry.clone();
    let stopped = thread::spawn(move || {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
            .block_on(stopper.set_user_collection_enabled(false))
    });
    let deadline = std::time::Instant::now() + Duration::from_secs(30);
    while telemetry.collecting.load(Ordering::Acquire) {
        assert!(
            std::time::Instant::now() < deadline,
            "disable did not invalidate consent"
        );
        thread::yield_now();
    }
    paused.resume();
    assert!(!stopped.join().unwrap().unwrap().enabled);
    assert_empty_diagnostics(&telemetry);
    assert!(telemetry.outbox.lock().unwrap().records.is_empty());
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 0);
}

#[tokio::test(flavor = "current_thread")]
async fn diagnostic_period_change_before_lock_retries_once_in_renewed_period() {
    let _home = TestHome::new();
    let runtime = test_runtime("http://localhost:1");
    let telemetry = test_telemetry(runtime.clone(), heartbeat_state(&runtime));
    let install = super::install::guard(&telemetry, "feishu");
    let operation = install.operation_id.clone();
    let generation = install.authorization_generation.unwrap();
    let error = "connection timeout PRIVATE_INSTALL_DETAILS";
    let paused = Paused::start(Point::BeforeLock, move || {
        install.finish(Err::<Value, _>(error.into()))
    });
    paused.wait();
    telemetry
        .credential_renewal_in_progress
        .store(true, Ordering::Release);
    telemetry.collecting.store(false, Ordering::Release);
    {
        let mut state = telemetry.state.lock().await;
        let state = state.as_mut().unwrap();
        state.device_token = Some("renewed_install_token".into());
        state.control.collection_period_id = Some("renewed_cleanup_period".into());
        state.credential_renewal_checkpoint = Some(CollectionLease {
            generation,
            collection_period_id: "renewed_cleanup_period".into(),
        });
        persist_state(state).unwrap();
        // The fixture supplies registration facts; the real checkpoint path
        // rebinds/persists children and reopens collection under the same consent.
        telemetry
            .finish_credential_renewal_checkpoint(state)
            .unwrap();
    }
    assert_eq!(paused.resume(), Err(error.into()));
    // The coordinator immediately compensates a failed old-period publication
    // if the same consent already has a usable renewed lease. It must publish
    // exactly once under that lease, without retaining any old-period witness.
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 0);
    let event_id = {
        let outbox = telemetry.outbox.lock().unwrap();
        assert_eq!(outbox.records.len(), 1);
        assert_eq!(outbox.records[0].event["operation_id"], operation);
        assert_eq!(
            outbox.records[0].event["diagnostic_code"],
            "request_timeout"
        );
        assert_eq!(
            outbox.records[0].collection_period_id,
            "renewed_cleanup_period"
        );
        assert_eq!(outbox.records[0].generation, generation);
        outbox.records[0].event["event_id"].clone()
    };
    let memory = serde_json::to_value(&*telemetry.diagnostics.lock().unwrap()).unwrap();
    let disk: Value = serde_json::from_slice(
        &std::fs::read(paths::pinvou3_home().join("failure-diagnostics.json")).unwrap(),
    )
    .unwrap();
    for state in [&memory, &disk] {
        let contexts = state["contexts"].as_object().unwrap();
        let incidents = state["incidents"].as_array().unwrap();
        assert_eq!(contexts.len(), 1);
        assert_eq!(incidents.len(), 1);
        for material in contexts.values().chain(incidents.iter()) {
            assert_eq!(material["lease"]["generation"], generation);
            assert_eq!(
                material["lease"]["collection_period_id"],
                "renewed_cleanup_period"
            );
        }
        assert_eq!(incidents[0]["event"]["event_id"], event_id);
        assert_eq!(
            incidents[0]["event"]["diagnostic_origin_period_id"],
            "renewed_cleanup_period"
        );
    }
    telemetry
        .credential_renewal_in_progress
        .store(false, Ordering::Release);
    telemetry.flush_tool_install_terminals(generation);
    let outbox = telemetry.outbox.lock().unwrap();
    assert_eq!(outbox.records.len(), 1);
    assert_eq!(outbox.records[0].event["event_id"], event_id);
    assert_eq!(telemetry.tool_install_terminals.pending_len(), 0);
}
