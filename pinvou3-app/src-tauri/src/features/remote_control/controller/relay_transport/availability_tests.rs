use super::tests::{control, lease};
use super::*;
use crate::features::remote_control::controller::protocol::CONTROLLER_SUBPROTOCOL;
use serde_json::json;
use std::panic::AssertUnwindSafe;
use std::sync::OnceLock;
use tokio_tungstenite::accept_hdr_async;

struct StopOnDrop(ControllerTransportSender);

impl Drop for StopOnDrop {
    fn drop(&mut self) {
        self.0.stop();
    }
}

fn channels() -> (ControllerTransportSender, ControllerTransportReceiver) {
    transport_channels(
        ControllerRole::ControlledB,
        "generation-0001",
        32,
        8,
        64 * 1024,
        1024,
    )
}

fn config() -> ControllerTransportConfig {
    ControllerTransportConfig::new(
        "ws://127.0.0.1:1",
        ControllerRole::ControlledB,
        "generation-0001",
    )
}

fn liveness(epoch: &str) -> Value {
    json!({"v":1,"type":"encrypted_controller_liveness_a", "connection_id":"connection-0001",
        "controller_lease_id":"lease-0001","lease_epoch":epoch,"ciphertext_b64":"AQID","ciphertext_bytes":"3"})
}

#[test]
fn bare_stale_and_error_admission_never_reset_runtime_backoff() {
    let (sender, _receiver) = channels();
    sender
        .install_authenticated_lease(lease("7", "generation-0001"))
        .unwrap();
    let events = parking_lot::Mutex::new(Vec::new());
    let sink = |event| events.lock().push(event);
    let bare = dispatch_inbound_evidence(
        &config(),
        &sender,
        &sink,
        &control("request-bare").to_string(),
    );
    assert!(
        bare.accepted,
        "a legal pre-authentication control remains deliverable"
    );
    assert!(bare.positive_lease.is_none());
    assert!(
        !dispatch_inbound_evidence(&config(), &sender, &sink, &liveness("6").to_string()).accepted
    );
    assert!(
        !dispatch_inbound_evidence(
            &config(),
            &sender,
            &sink,
            "{\"v\":1,\"v\":1,\"type\":\"error\"}"
        )
        .accepted
    );
    let error = json!({"v":1,"type":"error","request_id":"request-error","code":"controller_busy","message":"busy"});
    let error = dispatch_inbound_evidence(&config(), &sender, &sink, &error.to_string());
    assert!(error.accepted);
    assert!(error.positive_lease.is_none());
    let good = dispatch_inbound_evidence(&config(), &sender, &sink, &liveness("7").to_string());
    assert!(good.positive_lease.is_some());
    sender.stop();
    assert!(
        !dispatch_inbound_evidence(&config(), &sender, &sink, &liveness("7").to_string()).accepted
    );
    assert_eq!(events.lock().len(), 3);
}

#[test]
fn ingress_callback_stop_or_reinstall_cannot_credit_an_old_admission() {
    for stop in [false, true] {
        let (sender, _receiver) = channels();
        sender
            .install_authenticated_lease(lease("7", "generation-0001"))
            .unwrap();
        let original = sender.lease.read().clone().unwrap();
        let sink = |_: ControllerTransportEvent| {
            if stop {
                sender.stop();
            } else {
                sender
                    .install_authenticated_lease(lease("7", "generation-0001"))
                    .unwrap();
            }
        };
        let evidence =
            dispatch_inbound_evidence(&config(), &sender, &sink, &liveness("7").to_string());
        assert!(evidence.accepted);
        assert!(evidence.positive_lease.is_none());
        if !stop {
            assert!(!Arc::ptr_eq(
                &original,
                sender.lease.read().as_ref().unwrap()
            ));
        }
    }
}

#[tokio::test(start_paused = true)]
async fn stable_reset_requires_two_admissions_across_the_window_and_same_install() {
    let current = Arc::new(lease("7", "generation-0001"));
    let mut stable = StableAdmission::default();
    let start = tokio::time::Instant::now();
    let window = Duration::from_secs(5);
    assert!(!stable.observe(current.clone(), start, window));
    assert!(!stable.observe(
        current.clone(),
        start + window - Duration::from_millis(1),
        window
    ));
    assert!(stable.observe(current, start + window, window));
    // A freshly installed lease with equal public context is a new admission
    // identity, including after a successful resume, not inherited stability.
    let replacement = Arc::new(lease("7", "generation-0001"));
    assert!(!stable.observe(replacement.clone(), start + window, window));
    assert!(stable.observe(replacement, start + window * 2, window));
    let mut next_socket = StableAdmission::default();
    assert!(!next_socket.observe(
        Arc::new(lease("7", "generation-0001")),
        start + window * 3,
        window
    ));
}

#[tokio::test(start_paused = true)]
async fn full_outbound_batch_yields_to_heartbeat_then_inbound_and_stop() {
    let (sender, mut receiver) = transport_channels(
        ControllerRole::ControllerA,
        "generation-0001",
        32,
        8,
        64 * 1024,
        1024,
    );
    for i in 0..(MAX_OUTBOUND_BATCH + 2) {
        sender
            .try_send_control(&control(&format!("request-{i:04}")))
            .unwrap();
    }
    let mut incoming = futures_util::stream::repeat_with(|| {
        Ok::<_, std::io::Error>(Message::Pong(Vec::new().into()))
    });
    let mut heartbeat = tokio::time::interval(Duration::from_secs(5));
    heartbeat.tick().await;
    let deadline = tokio::time::Instant::now() + Duration::from_secs(30);
    for _ in 0..MAX_OUTBOUND_BATCH {
        assert!(matches!(
            connected_poll::next_event(
                &sender.shutdown,
                &mut receiver,
                &mut incoming,
                &mut heartbeat,
                deadline,
                false
            )
            .await,
            ConnectedEvent::Outgoing(Some(_))
        ));
    }
    tokio::time::advance(Duration::from_secs(5)).await;
    assert!(matches!(
        connected_poll::next_event(
            &sender.shutdown,
            &mut receiver,
            &mut incoming,
            &mut heartbeat,
            deadline,
            true
        )
        .await,
        ConnectedEvent::Heartbeat
    ));
    assert!(matches!(
        connected_poll::next_event(
            &sender.shutdown,
            &mut receiver,
            &mut incoming,
            &mut heartbeat,
            deadline,
            true
        )
        .await,
        ConnectedEvent::Incoming(Some(Ok(_)))
    ));
    sender.stop();
    assert!(matches!(
        connected_poll::next_event(
            &sender.shutdown,
            &mut receiver,
            &mut incoming,
            &mut heartbeat,
            deadline,
            true
        )
        .await,
        ConnectedEvent::Shutdown
    ));
}

#[tokio::test]
async fn lifecycle_guard_retires_aborted_unpolled_task_but_not_replacement() {
    let actor = || {
        transport_channels(
            ControllerRole::ControllerA,
            "generation-0001",
            32,
            8,
            64 * 1024,
            1024,
        )
    };
    let (old, old_receiver) = actor();
    let (new, _new_receiver) = actor();
    old.install_authenticated_lease(lease("7", "generation-0001"))
        .unwrap();
    new.install_authenticated_lease(lease("7", "generation-0001"))
        .unwrap();
    let events = Arc::new(parking_lot::Mutex::new(Vec::new()));
    let sink_events = events.clone();
    let guard = task_lifecycle::TaskLifetimeGuard::new(
        old.clone(),
        Arc::new(move |event| sink_events.lock().push(event)),
    );
    let task = tokio::spawn(async move {
        let _guard = guard;
        let _receiver = old_receiver;
        std::future::pending::<()>().await;
    });
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    assert!(old.is_stopped());
    assert!(old.authenticated_lease().is_none());
    assert!(!new.is_stopped());
    assert!(new.authenticated_lease().is_some());
    assert_eq!(events.lock().len(), 1);
    let ControllerTransportEvent::Disconnected { context, .. } = &events.lock()[0] else {
        panic!("missing terminal disconnect")
    };
    assert_eq!(context.owner, old.owner());
    assert_ne!(context.owner, new.owner());
    assert_eq!(context.lease.as_ref().unwrap().lease_epoch, "7");
    assert_eq!(
        old.try_send_control(&control("request-after-stop")),
        Err(ControllerSendError::Closed)
    );
    new.stop();
}

#[tokio::test]
async fn real_spawn_panic_retires_sender_and_projects_one_owned_disconnect() {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let slot = Arc::new(OnceLock::<ControllerTransportSender>::new());
    let sink_slot = slot.clone();
    let terminal = Arc::new(tokio::sync::Notify::new());
    let signal = terminal.clone();
    let events = Arc::new(parking_lot::Mutex::new(Vec::new()));
    let sink_events = events.clone();
    let sender = spawn_controller_transport(
        ControllerTransportConfig::new(
            format!("ws://{}", listener.local_addr().unwrap()),
            ControllerRole::ControlledB,
            "generation-0001",
        ),
        Arc::new(move |event| {
            if matches!(event, ControllerTransportEvent::Connected { .. }) {
                sink_slot
                    .get()
                    .unwrap()
                    .install_authenticated_lease(lease("7", "generation-0001"))
                    .unwrap();
                panic!("injected controller transport task panic");
            }
            if matches!(event, ControllerTransportEvent::Disconnected { .. }) {
                sink_events.lock().push(event);
                signal.notify_one();
            }
        }),
    );
    let _stop = StopOnDrop(sender.clone());
    assert!(slot.set(sender.clone()).is_ok());
    let (stream, _) = tokio::time::timeout(Duration::from_secs(3), listener.accept())
        .await
        .unwrap()
        .unwrap();
    let _socket = tokio::time::timeout(Duration::from_secs(3), accept_hdr_async(
        stream,
        |_: &tokio_tungstenite::tungstenite::handshake::server::Request,
         mut response: tokio_tungstenite::tungstenite::handshake::server::Response| {
            response.headers_mut().insert(
                "Sec-WebSocket-Protocol",
                CONTROLLER_SUBPROTOCOL.parse().unwrap(),
            );
            Ok(response)
        },
    ))
    .await
    .unwrap()
    .unwrap();
    tokio::time::timeout(Duration::from_secs(3), terminal.notified())
        .await
        .unwrap();
    assert!(sender.is_stopped());
    assert!(sender.authenticated_lease().is_none());
    assert_eq!(events.lock().len(), 1);
    let ControllerTransportEvent::Disconnected { context, .. } = &events.lock()[0] else {
        panic!("missing terminal disconnect")
    };
    assert_eq!(context.owner, sender.owner());
    assert_eq!(context.lease.as_ref().unwrap().lease_epoch, "7");
}

#[test]
fn explicit_stop_suppresses_terminal_projection_and_sink_panic_cannot_skip_retirement() {
    let (sender, _receiver) = channels();
    let events = Arc::new(parking_lot::Mutex::new(Vec::new()));
    let copy = events.clone();
    let guard = task_lifecycle::TaskLifetimeGuard::new(
        sender.clone(),
        Arc::new(move |event| copy.lock().push(event)),
    );
    sender.stop();
    drop(guard);
    assert!(events.lock().is_empty());
    let (sender, _receiver) = channels();
    sender
        .install_authenticated_lease(lease("7", "generation-0001"))
        .unwrap();
    let guard = task_lifecycle::TaskLifetimeGuard::new(
        sender.clone(),
        Arc::new(|_| panic!("injected terminal sink panic")),
    );
    assert!(std::panic::catch_unwind(AssertUnwindSafe(|| drop(guard))).is_ok());
    assert!(sender.is_stopped());
    assert!(sender.authenticated_lease().is_none());
}

#[tokio::test]
async fn real_socket_invalid_traffic_and_unmatched_pongs_cannot_renew_silence() {
    for noise in [
        Message::Text("invalid JSON".into()),
        Message::Pong(Vec::new().into()),
        Message::Binary(vec![1, 2, 3].into()),
    ] {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut socket=accept_hdr_async(stream,|_: &tokio_tungstenite::tungstenite::handshake::server::Request,mut response:tokio_tungstenite::tungstenite::handshake::server::Response| {
                response.headers_mut().insert("Sec-WebSocket-Protocol",CONTROLLER_SUBPROTOCOL.parse().unwrap());Ok(response)
            }).await.unwrap();
            // Do not read/answer the client's probes. Keep actual inbound noise
            // ready until the controller enforces its absolute silence cutoff.
            while socket.send(noise.clone()).await.is_ok() {
                tokio::task::yield_now().await;
            }
        });
        let (sender, receiver) = channels();
        let _stop = StopOnDrop(sender.clone());
        let (finished, done) = tokio::sync::oneshot::channel();
        let finished = parking_lot::Mutex::new(Some(finished));
        let sink = Arc::new(move |event| {
            if let ControllerTransportEvent::Disconnected { error, .. } = event {
                if let Some(tx) = finished.lock().take() {
                    let _ = tx.send(error);
                }
            }
        });
        let task_sender = sender.clone();
        let mut task = tokio::spawn(async move {
            run_transport_with_heartbeat(
                ControllerTransportConfig::new(
                    format!("ws://{address}"),
                    ControllerRole::ControlledB,
                    "generation-0001",
                ),
                sink,
                task_sender,
                receiver,
                Duration::from_millis(25),
            )
            .await;
        });
        let result = tokio::time::timeout(Duration::from_secs(3), done).await;
        sender.stop();
        let ended = tokio::time::timeout(Duration::from_secs(1), &mut task).await;
        if ended.is_err() {
            task.abort();
            let _ = task.await;
        }
        server.abort();
        let _ = server.await;
        assert!(ended.unwrap().is_ok());
        assert!(
            result
                .unwrap()
                .unwrap()
                .contains("inbound silence deadline expired")
        );
    }
}
