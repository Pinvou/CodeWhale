//! Actual A keeper/manager/crypto/reader-dispatch and production status consumer.
//! Relay delivery and Tauri/telemetry I/O are controlled ports; this is not a
//! two-machine handshake, WebView, or telemetry-upload E2E.
use super::super::test_support::{pairing_fixture, role_session_keys};
use super::*;
use std::collections::VecDeque;
use std::future::Future;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Mutex, mpsc as sync_mpsc};

type Lifecycle = (
    RemoteStage,
    RemoteOutcome,
    RemoteReason,
    Option<&'static str>,
);

// Bound an accidental production callback double-lock without hanging the lib
// runner. On timeout the thread owns only this fixture and is detached; no global
// environment, personal data, or shared database/lock is held by these tests.
fn bounded(test: impl FnOnce() + Send + 'static) {
    let (done, result) = sync_mpsc::channel();
    let worker = std::thread::spawn(move || {
        let _ = done.send(std::panic::catch_unwind(std::panic::AssertUnwindSafe(test)));
    });
    match result
        .recv_timeout(Duration::from_secs(15))
        .expect("keeper callback must finish")
    {
        Ok(()) => worker.join().unwrap(),
        Err(panic) => std::panic::resume_unwind(panic),
    }
}

fn runtime(test: impl Future<Output = ()>) {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(test);
}

struct Fixture {
    manager: Arc<ControllingDeviceManager>,
    operation: ControllerOperation,
    business: Arc<ControllingLaneState>,
    sender: ControllerTransportSender,
    lease: ControllerLease,
}

impl Fixture {
    fn new() -> Self {
        let manager = Arc::new(ControllingDeviceManager::default());
        let operation = manager.connect("2345678").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        let lease = ControllerLease {
            context: LeaseContext {
                connection_id: "connection-0001".into(),
                controller_lease_id: "lease-0001".into(),
                lease_epoch: "1".into(),
                relay_epoch: "relay-epoch-0001".into(),
                remote_control_generation: "generation-0001".into(),
            },
            peer_endpoint_id: "endpoint-0001".into(),
            reservation_id: "reservation-0001".into(),
            device_display_name: "Studio PC".into(),
            liveness_epoch_a: "live-a-1".into(),
            liveness_epoch_b: "live-b-1".into(),
            resume_deadline_b: 30_000,
            resume_token: [7; 32],
            transport_key_fingerprint: [9; 32],
            negotiated_capabilities: vec![
                "encrypted_frames_v1".into(),
                "pairing_v1".into(),
                "resume_v1".into(),
                "task_v1".into(),
            ],
        };
        manager
            .complete_connected(operation.clone(), lease.clone())
            .unwrap();
        let (context, capabilities) = manager.live_runtime_context().unwrap();
        let sender = super::super::test_support::resume_reader_transport(
            ControllerRole::ControllerA,
            context,
            capabilities,
        );
        manager.replace_transport(sender.clone());
        let business = Arc::new(ControllingLaneState::default());
        let fixture = pairing_fixture("relay-key-projection");
        let session = role_session_keys(
            HandshakeRole::A,
            fixture.a_keys,
            fixture.b_keys.public_key().to_bytes(),
            &fixture.reservation,
        );
        business
            .install(HandshakeRole::A, &lease.context, session)
            .unwrap();
        Self {
            manager,
            operation,
            business,
            sender,
            lease,
        }
    }

    fn lane(&self) -> Lane {
        let context = &self.lease.context;
        let frame = |kind: &str| {
            serde_json::json!({
                "v": 1, "type": kind, "activation_id": "activation-0001",
                "request_id": format!("request-resume-{kind}-0001"),
                "connection_id": context.connection_id,
                "controller_lease_id": context.controller_lease_id,
                "lease_epoch": context.lease_epoch,
                "relay_epoch": context.relay_epoch,
                "remote_control_generation": context.remote_control_generation,
            })
        };
        let mut provisional = frame("controller_resume_provisional");
        provisional["original_deadline_ms"] = "30000".into();
        let mut pending = frame("controller_resume_activation_pending");
        pending["original_deadline_ms"] = "30000".into();
        let mut liveness = frame("controller_post_activation_liveness_confirmed");
        liveness["liveness_epoch_b"] = "live-b-2".into();
        liveness["heartbeat_sequence_b"] = "1".into();
        let mut resumed = frame("controller_resumed");
        resumed["request_id"] = "request-resume-final-0001".into();
        resumed["liveness_epoch_a"] = "live-a-2".into();
        resumed["liveness_epoch_b"] = "live-b-2".into();
        resumed["resume_deadline_ms"] = "1700000030000".into();
        resumed["new_resume_token_b64"] = base64::engine::general_purpose::STANDARD
            .encode([3; 32])
            .into();
        Lane {
            events: VecDeque::from([
                ControllerLaneEvent::Disconnected(self.sender.disconnect_context()),
                ControllerLaneEvent::Frame(provisional.to_string()),
                ControllerLaneEvent::Frame(pending.to_string()),
                ControllerLaneEvent::Frame(liveness.to_string()),
                ControllerLaneEvent::Frame(resumed.to_string()),
            ]),
            sender: self.sender.clone(),
            authorize: true,
            before_activate: None,
            after_activate: None,
            resume_business: None,
            observe: None,
            final_after_first_event: None,
            first_event_at: None,
            delays: VecDeque::new(),
            hold_open: false,
            reader_dropped: 0,
        }
    }
}

struct Lane {
    events: VecDeque<ControllerLaneEvent>,
    sender: ControllerTransportSender,
    authorize: bool,
    before_activate: Option<Box<dyn Fn() + Send + Sync>>,
    after_activate: Option<Box<dyn Fn() + Send + Sync>>,
    resume_business: Option<Box<dyn Fn() -> Result<(), ControllerLaneFailure> + Send + Sync>>,
    observe: Option<Box<dyn Fn(&ControllerLaneEvent) + Send>>,
    final_after_first_event: Option<Duration>,
    first_event_at: Option<tokio::time::Instant>,
    delays: VecDeque<Duration>,
    hold_open: bool,
    reader_dropped: usize,
}

#[async_trait::async_trait]
impl ControllingLane for Lane {
    fn transport_sender(&self) -> Option<ControllerTransportSender> {
        Some(self.sender.clone())
    }

    async fn next_lane_event(&mut self) -> Option<ControllerLaneEvent> {
        loop {
            let Some(event) = self.events.pop_front() else {
                if self.hold_open {
                    std::future::pending::<()>().await;
                }
                return None;
            };
            if let Some(delay) = self.delays.pop_front()
                && !delay.is_zero()
            {
                tokio::time::sleep(delay).await;
            }
            let first_event_at = *self
                .first_event_at
                .get_or_insert_with(tokio::time::Instant::now);
            if let ControllerLaneEvent::Frame(text) = &event
                && serde_json::from_str::<Value>(text).unwrap()["type"] == "controller_resumed"
                && let Some(offset) = self.final_after_first_event.take()
            {
                tokio::time::sleep_until(first_event_at + offset).await;
            }
            if let Some(observe) = self.observe.as_ref() {
                observe(&event);
            }
            match &event {
                ControllerLaneEvent::Disconnected(_) => {
                    super::super::relay_transport::disconnect_test_transport(&self.sender);
                    return Some(event);
                }
                ControllerLaneEvent::Frame(text) => {
                    let frame: Value = serde_json::from_str(text).unwrap();
                    if !self.authorize && frame["type"] == "controller_resumed" {
                        // Explicit negative injection: final without reader proof
                        // must still fail closed in the actual resume handler.
                        return Some(event);
                    }
                    match super::super::relay_transport::read_test_transport_frame(
                        &self.sender,
                        &frame,
                    ) {
                        Some(ControllerTransportEvent::Frame { json, .. }) => {
                            return Some(ControllerLaneEvent::Frame(json));
                        }
                        Some(_) => panic!("frame reader delivered a non-frame event"),
                        None => {
                            self.reader_dropped += 1;
                        }
                    }
                }
            }
        }
    }

    fn send_resume_control(&self, _: Value) -> Result<(), String> {
        Ok(())
    }

    fn before_resume_activation(&self) {
        if let Some(hook) = self.before_activate.as_ref() {
            hook();
        }
    }

    fn after_resume_activation(&self) {
        if let Some(hook) = self.after_activate.as_ref() {
            hook();
        }
    }

    fn resume_business(&mut self) -> Result<(), ControllerLaneFailure> {
        self.resume_business.as_ref().map_or(Ok(()), |hook| hook())
    }
}

async fn serve(
    f: &Fixture,
    lane: &mut Lane,
    on_status: &mut (dyn FnMut(&ControllingStatus) + Send),
) {
    let sessions = ControllingSessionManager::default();
    let clock_start = tokio::time::Instant::now();
    let monotonic = || u64::try_from(clock_start.elapsed().as_millis()).unwrap();
    let mut sinks = ControllingLaneSinks {
        on_status,
        on_session: &mut |_| {},
        on_mapping: &mut |_, _, _| Ok(()),
        on_task: &mut |_, _| {},
        on_journal: &mut |_| Ok(()),
        on_snapshot: &mut |_| Ok(()),
        on_liveness: &mut |_| true,
    };
    serve_controlling_lane_with_evidence_context(
        lane,
        &f.manager,
        &sessions,
        &f.business,
        &mut sinks,
        Duration::from_secs(3600),
        PeerEvidenceContext {
            trust: None,
            monotonic: &monotonic,
            operation: f.operation.clone(),
        },
    )
    .await;
}

fn shared_fixture(status: &Value, name: &str) {
    let fixtures: Value = serde_json::from_str(include_str!(
        "../../../../../tests/fixtures/desktop_remote_status.json"
    ))
    .unwrap();
    // Raw ownership is checked by callers first. Only the random claim in this
    // separate clone is normalized to compare the cross-language shape.
    let mut normalized = status.clone();
    normalized["claimRequestId"] = "claim-0001".into();
    assert_eq!(normalized, fixtures[name]);
}

fn assert_raw(f: &Fixture, status: &Value) {
    assert_eq!(
        status["claimRequestId"].as_str(),
        Some(f.operation.claim_request_id())
    );
    assert!(!f.operation.claim_request_id().is_empty());
    let lease = &f.lease.context;
    for (key, value) in [
        ("connectionId", &lease.connection_id),
        ("controllerLeaseId", &lease.controller_lease_id),
        ("leaseEpoch", &lease.lease_epoch),
        ("relayEpoch", &lease.relay_epoch),
        ("remoteControlGeneration", &lease.remote_control_generation),
    ] {
        assert_eq!(status["lease"][key].as_str(), Some(value.as_str()));
    }
}

#[test]
fn connected_publication_waits_for_successful_inbound_resume() {
    bounded(|| {
        runtime(async {
            for fail_resume in [false, true] {
                let f = Fixture::new();
                let mut lane = f.lane();
                let resumed = Arc::new(AtomicBool::new(false));
                let states = Arc::new(Mutex::new(Vec::new()));
                let business = Arc::clone(&f.business);
                let lease = f.lease.context.clone();
                let release_seen = Arc::clone(&resumed);
                let before_release = Arc::clone(&states);
                lane.resume_business = Some(Box::new(move || {
                    business.with_active(&lease, || Ok(())).unwrap();
                    assert!(
                        before_release
                            .lock()
                            .unwrap()
                            .iter()
                            .all(|kind| { *kind != ControllingStatusKind::Connected })
                    );
                    assert!(!release_seen.swap(true, Ordering::SeqCst));
                    if fail_resume {
                        Err(ControllerLaneFailure::Closed)
                    } else {
                        Ok(())
                    }
                }));
                let mut sink = keeper_status_sink(
                    &f.manager,
                    &f.operation,
                    |_| {},
                    |status| {
                        if status.kind() == ControllingStatusKind::Connected {
                            assert!(resumed.load(Ordering::SeqCst));
                            assert!(!fail_resume);
                        }
                        states.lock().unwrap().push(status.kind());
                    },
                );
                serve(&f, &mut lane, &mut sink).await;
                assert!(resumed.load(Ordering::SeqCst));
                let connected = states
                    .lock()
                    .unwrap()
                    .contains(&ControllingStatusKind::Connected);
                assert_eq!(connected, !fail_resume);
                if fail_resume {
                    assert!(f.business.with_active(&f.lease.context, || Ok(())).is_err());
                    assert_ne!(f.manager.status().kind(), ControllingStatusKind::Connected);
                }
            }
        });
    });
}

#[test]
fn authenticated_resume_publishes_once_with_raw_identity_and_shared_serde_shape() {
    bounded(|| {
        runtime(async {
            let f = Fixture::new();
            let mut lane = f.lane();
            let duplicate = lane.events.back().unwrap().clone();
            lane.events.push_back(duplicate);
            let states = Arc::new(Mutex::new(Vec::new()));
            let telemetry = Arc::new(Mutex::new(Vec::new()));
            let observed = Arc::clone(&states);
            let first_final = AtomicBool::new(true);
            lane.observe = Some(Box::new(move |event| {
                if let ControllerLaneEvent::Frame(text) = event {
                    let frame: Value = serde_json::from_str(text).unwrap();
                    if frame["type"] != "controller_resumed"
                        || first_final.swap(false, Ordering::SeqCst)
                    {
                        assert!(
                            observed
                                .lock()
                                .unwrap()
                                .iter()
                                .all(|s: &Value| s["status"] != "connected")
                        );
                    }
                }
            }));
            let mut sink = keeper_status_sink(
                &f.manager,
                &f.operation,
                |event| {
                    telemetry
                        .lock()
                        .unwrap()
                        .push(event.lifecycle_fields_for_test())
                },
                |status| {
                    if status.kind() == ControllingStatusKind::Connected {
                        f.business
                            .with_active(&f.lease.context, || Ok(()))
                            .expect("Connected publication requires active business");
                    }
                    states
                        .lock()
                        .unwrap()
                        .push(serde_json::to_value(status).unwrap())
                },
            );
            serve(&f, &mut lane, &mut sink).await;
            assert_eq!(
                lane.reader_dropped, 1,
                "duplicate final must be rejected by the actual reader"
            );
            let states = states.lock().unwrap();
            assert_eq!(
                states
                    .iter()
                    .map(|s| s["status"].as_str().unwrap())
                    .collect::<Vec<_>>(),
                ["reconnecting", "connected"]
            );
            let query = serde_json::to_value(f.manager.status()).unwrap();
            assert_raw(&f, &states[0]);
            assert_raw(&f, &states[1]);
            assert_raw(&f, &query);
            assert_eq!(states[1], query);
            shared_fixture(&states[0], "controllerReconnecting");
            shared_fixture(&states[1], "controllerConnected");
            assert!(!f.business.is_suspended());
            f.business.with_active(&f.lease.context, || Ok(())).unwrap();
            assert_eq!(
                *telemetry.lock().unwrap(),
                [
                    (
                        RemoteStage::Disconnection,
                        RemoteOutcome::Ok,
                        RemoteReason::None,
                        Some("controller")
                    ),
                    (
                        RemoteStage::Resume,
                        RemoteOutcome::Ok,
                        RemoteReason::None,
                        Some("controller")
                    ),
                ]
            );
        })
    });
}

#[test]
fn production_consumer_counts_resume_and_second_disconnect_then_rejection() {
    bounded(|| {
        runtime(async {
            tokio::time::pause();
            let f = Fixture::new();
            let mut lane = f.lane();
            lane.events.push_back(ControllerLaneEvent::Disconnected(
                f.sender.disconnect_context(),
            ));
            // A genuine finite second resume timeout, rather than a fabricated
            // error packet which the production reader would reject.
            lane.hold_open = true;
            let mut events: Vec<Lifecycle> = Vec::new();
            let mut states = Vec::new();
            let mut sink = keeper_status_sink(
                &f.manager,
                &f.operation,
                |event| events.push(event.lifecycle_fields_for_test()),
                |status| states.push(status.kind()),
            );
            serve(&f, &mut lane, &mut sink).await;
            drop(sink);
            assert_eq!(
                states,
                [
                    ControllingStatusKind::Reconnecting,
                    ControllingStatusKind::Connected,
                    ControllingStatusKind::Reconnecting,
                    ControllingStatusKind::Error
                ]
            );
            assert_eq!(
                events,
                [
                    (
                        RemoteStage::Disconnection,
                        RemoteOutcome::Ok,
                        RemoteReason::None,
                        Some("controller")
                    ),
                    (
                        RemoteStage::Resume,
                        RemoteOutcome::Ok,
                        RemoteReason::None,
                        Some("controller")
                    ),
                    (
                        RemoteStage::Disconnection,
                        RemoteOutcome::Ok,
                        RemoteReason::None,
                        Some("controller")
                    ),
                    (
                        RemoteStage::Resume,
                        RemoteOutcome::Rejected,
                        RemoteReason::ResumeFailed,
                        Some("controller")
                    ),
                ]
            );
        })
    });
}

#[test]
fn unavailable_telemetry_does_not_block_resume_publication() {
    bounded(|| {
        runtime(async {
            let f = Fixture::new();
            let mut lane = f.lane();
            let mut states = Vec::new();
            let mut sink = keeper_status_sink(
                &f.manager,
                &f.operation,
                |_| {},
                |status| states.push(status.kind()),
            );
            serve(&f, &mut lane, &mut sink).await;
            drop(sink);
            assert_eq!(
                states,
                [
                    ControllingStatusKind::Reconnecting,
                    ControllingStatusKind::Connected
                ]
            );
        })
    });
}

#[test]
fn rejected_snapshot_keeps_previous_status_for_the_real_failure_transition() {
    bounded(|| {
        let f = Fixture::new();
        let stale_connected = f.manager.status();
        let reconnecting = f
            .manager
            .mark_owned_transport_disconnected(&f.operation, 0, Some(30_000), &f.business)
            .unwrap();
        let mut events = Vec::new();
        let mut states = Vec::new();
        let mut sink = keeper_status_sink(
            &f.manager,
            &f.operation,
            |event| events.push(event.lifecycle_fields_for_test()),
            |status| states.push(status.kind()),
        );
        sink(&reconnecting);
        // Direct adapter negative input: this does not assert that a production
        // keeper can queue an old callback across a second resume cycle.
        sink(&stale_connected);
        let failure = f
            .manager
            .fail_resume_connection(f.operation.clone(), PEER_UNAVAILABLE_ERROR)
            .unwrap();
        sink(&failure);
        drop(sink);
        assert_eq!(
            states,
            [
                ControllingStatusKind::Reconnecting,
                ControllingStatusKind::Error
            ]
        );
        assert_eq!(
            events,
            [
                (
                    RemoteStage::Disconnection,
                    RemoteOutcome::Ok,
                    RemoteReason::None,
                    Some("controller")
                ),
                (
                    RemoteStage::Resume,
                    RemoteOutcome::Rejected,
                    RemoteReason::ResumeFailed,
                    Some("controller")
                ),
            ]
        );
    });
}

#[test]
fn first_frozen_deadline_wins_over_final_and_repeated_disconnects() {
    bounded(|| {
        runtime(async {
            tokio::time::pause();
            for final_offset in [30, 50] {
                let f = Fixture::new();
                let mut lane = f.lane();
                lane.events.insert(
                    1,
                    ControllerLaneEvent::Disconnected(f.sender.disconnect_context()),
                );
                lane.events.insert(
                    4,
                    ControllerLaneEvent::Disconnected(f.sender.disconnect_context()),
                );
                // Anchor after the initial heartbeat tick: the first lane event
                // follows budget creation in the same paused, synchronous poll.
                // Final shares this absolute origin, avoiding accumulated sleeps.
                lane.delays = [0, 5, 0, 0, 5, 0, 0].map(Duration::from_secs).into();
                lane.final_after_first_event = Some(Duration::from_secs(final_offset));
                let disconnects = Arc::new(Mutex::new(Vec::new()));
                let seen = Arc::clone(&disconnects);
                lane.observe = Some(Box::new(move |event| {
                    if matches!(event, ControllerLaneEvent::Disconnected(_)) {
                        seen.lock().unwrap().push(tokio::time::Instant::now());
                    }
                }));
                let mut events = Vec::new();
                let mut states = Vec::new();
                let mut sink = keeper_status_sink(
                    &f.manager,
                    &f.operation,
                    |event| events.push(event.lifecycle_fields_for_test()),
                    |status| states.push(status.kind()),
                );
                serve(&f, &mut lane, &mut sink).await;
                drop(sink);
                let origin = lane.first_event_at.unwrap();
                let elapsed = origin.elapsed();
                // Tokio's locked timer driver rounds up to the next millisecond
                // (runtime/time/source.rs::deadline_to_tick). This is scheduler
                // precision, not an extension of the product's 30-second budget.
                assert!(
                    (Duration::from_secs(30)..=Duration::from_millis(30_001)).contains(&elapsed),
                    "frozen cutoff must not move: {elapsed:?}"
                );
                let disconnects = disconnects.lock().unwrap();
                assert_eq!(disconnects.len(), 3);
                assert_eq!(disconnects[0], origin);
                for (index, seconds) in [(1, 5), (2, 10)] {
                    let expected = Duration::from_secs(seconds);
                    let rounding = Duration::from_millis(u64::try_from(index).unwrap());
                    assert!(
                        (expected..=expected + rounding).contains(&(disconnects[index] - origin))
                    );
                }
                assert_eq!(
                    states,
                    [
                        ControllingStatusKind::Reconnecting,
                        ControllingStatusKind::Error
                    ]
                );
                assert_eq!(
                    events,
                    [
                        (
                            RemoteStage::Disconnection,
                            RemoteOutcome::Ok,
                            RemoteReason::None,
                            Some("controller")
                        ),
                        (
                            RemoteStage::Resume,
                            RemoteOutcome::Rejected,
                            RemoteReason::ResumeFailed,
                            Some("controller")
                        ),
                    ]
                );
                assert!(f.manager.live_lease().is_err());
                assert!(!f.business.is_installed());
            }
        })
    });
}

#[test]
fn missing_reader_or_b_terminal_gate_never_publishes_success() {
    bounded(|| {
        runtime(async {
            for missing_reader in [true, false] {
                let f = Fixture::new();
                let mut lane = f.lane();
                if missing_reader {
                    lane.authorize = false;
                } else {
                    lane.events.remove(3);
                }
                let mut states = Vec::new();
                let mut events = Vec::new();
                let mut sink = keeper_status_sink(
                    &f.manager,
                    &f.operation,
                    |event| events.push(event.lifecycle_fields_for_test()),
                    |status| states.push(status.kind()),
                );
                serve(&f, &mut lane, &mut sink).await;
                drop(sink);
                assert!(!states.contains(&ControllingStatusKind::Connected));
                assert!(
                    events
                        .iter()
                        .all(|(stage, outcome, _, _)| !(*stage == RemoteStage::Resume
                            && *outcome == RemoteOutcome::Ok))
                );
                assert!(f.business.is_suspended() || !f.business.is_installed());
            }
        })
    });
}

// A zero-capacity rendezvous fixes the exact interleaving. Both directions have
// timeouts and the outer product callback execution is bounded as well.
fn replacement_rendezvous(
    f: &Fixture,
    replace: bool,
) -> (Box<dyn Fn() + Send + Sync>, std::thread::JoinHandle<()>) {
    let manager = Arc::clone(&f.manager);
    let lease = f.lease.clone();
    let business = Arc::clone(&f.business);
    let (arrive, reached) = sync_mpsc::sync_channel(0);
    let (released, release) = sync_mpsc::sync_channel(0);
    let worker = std::thread::spawn(move || {
        reached.recv_timeout(Duration::from_secs(5)).unwrap();
        manager.disconnect();
        if replace {
            let operation = manager.connect("2345678").unwrap();
            manager.complete_reserved(operation.clone()).unwrap();
            manager.complete_connected(operation, lease).unwrap();
            // A replacement with the same wire lease still owns new key material.
            // A stale clear/activate must leave this actual new lane intact.
            let next = pairing_fixture("relay-key-projection-replacement");
            let session = role_session_keys(
                HandshakeRole::A,
                next.a_keys,
                next.b_keys.public_key().to_bytes(),
                &next.reservation,
            );
            business
                .install(HandshakeRole::A, &manager.live_lease().unwrap(), session)
                .unwrap();
        }
        released.send(()).unwrap();
    });
    let release = Mutex::new(release);
    (
        Box::new(move || {
            arrive.send(()).unwrap();
            release
                .lock()
                .unwrap()
                .recv_timeout(Duration::from_secs(5))
                .unwrap();
        }),
        worker,
    )
}

#[test]
fn disconnect_or_new_owner_before_activation_cannot_complete_old_resume() {
    bounded(|| {
        runtime(async {
            for replace in [false, true] {
                let f = Fixture::new();
                let mut lane = f.lane();
                let (hook, worker) = replacement_rendezvous(&f, replace);
                lane.before_activate = Some(hook);
                let activated = Arc::new(AtomicBool::new(false));
                let seen = Arc::clone(&activated);
                lane.after_activate = Some(Box::new(move || {
                    seen.store(true, Ordering::SeqCst);
                }));
                let mut states = Vec::new();
                let mut events = Vec::new();
                let mut sink = keeper_status_sink(
                    &f.manager,
                    &f.operation,
                    |event| events.push(event.lifecycle_fields_for_test()),
                    |status| states.push(status.kind()),
                );
                serve(&f, &mut lane, &mut sink).await;
                drop(sink);
                worker.join().unwrap();
                assert_eq!(states, [ControllingStatusKind::Reconnecting]);
                assert_eq!(events.len(), 1);
                assert!(
                    !activated.load(Ordering::SeqCst),
                    "old activation cannot complete"
                );
                if replace {
                    assert!(f.business.is_installed());
                    assert!(!f.business.is_suspended());
                } else {
                    assert!(!f.business.is_installed() || f.business.is_suspended());
                }
                assert_eq!(
                    f.manager.status().kind(),
                    if replace {
                        ControllingStatusKind::Connected
                    } else {
                        ControllingStatusKind::Disconnected
                    }
                );
                if replace {
                    assert_ne!(f.manager.current_operation().unwrap(), f.operation);
                }
            }
        })
    });
}

#[test]
fn disconnect_or_new_owner_after_activation_is_rejected_by_production_sink() {
    bounded(|| {
        runtime(async {
            for replace in [false, true] {
                let f = Fixture::new();
                let mut lane = f.lane();
                let (hook, worker) = replacement_rendezvous(&f, replace);
                let business = Arc::clone(&f.business);
                lane.after_activate = Some(Box::new(move || {
                    assert!(!business.is_suspended());
                    hook();
                }));
                let mut states = Vec::new();
                let mut events = Vec::new();
                let mut production = keeper_status_sink(
                    &f.manager,
                    &f.operation,
                    |event| events.push(event.lifecycle_fields_for_test()),
                    |status| states.push(status.kind()),
                );
                serve(&f, &mut lane, &mut production).await;
                drop(production);
                worker.join().unwrap();
                assert_eq!(states, [ControllingStatusKind::Reconnecting]);
                assert_eq!(events.len(), 1);
                let current = f.manager.status();
                assert_eq!(
                    current.kind(),
                    if replace {
                        ControllingStatusKind::Connected
                    } else {
                        ControllingStatusKind::Disconnected
                    }
                );
                if replace {
                    let next_operation = f.manager.current_operation().unwrap();
                    let mut next = Vec::new();
                    let mut next_sink = keeper_status_sink(
                        &f.manager,
                        &next_operation,
                        |_| {},
                        |s| next.push(s.kind()),
                    );
                    next_sink(&current);
                    drop(next_sink);
                    assert_eq!(next, [ControllingStatusKind::Connected]);
                }
            }
        })
    });
}
