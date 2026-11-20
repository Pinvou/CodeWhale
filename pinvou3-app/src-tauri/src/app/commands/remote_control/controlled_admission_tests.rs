//! Private Windows host adapter for real Controlled startup/retirement tests.
use crate::features::remote_control::RemoteControlManager;
use crate::features::remote_control::controller::controlled::{
    ControlledDeviceManager, ControlledOperation, ControlledStatus, ControlledStatusKind,
};
use crate::features::remote_control::controller::relay_runtime::{
    ControlledBusinessPort, ControlledRegistrationTask, ControlledRelayWorker,
};
use futures_util::{FutureExt, SinkExt, StreamExt};
use parking_lot::Mutex;
use serde_json::{Value, json};
use std::panic::AssertUnwindSafe;
use std::sync::Arc;
use std::thread::JoinHandle;
use std::time::Duration;
use tauri::{AppHandle, Manager as _};
use tokio_tungstenite::tungstenite::Message;

struct AdmissionFixture {
    app: AppHandle,
    relay: RemoteControlManager,
}

struct EndpointScope(RemoteControlManager);

impl Drop for EndpointScope {
    fn drop(&mut self) {
        self.0.shutdown_loopback_test_endpoint();
    }
}

#[derive(Clone, Default)]
struct RegistrationScopes(Arc<Mutex<Vec<ControlledRegistrationTask>>>);

impl RegistrationScopes {
    fn remember(&self, task: ControlledRegistrationTask) {
        self.0.lock().push(task);
    }

    async fn retire(&self) {
        let tasks = std::mem::take(&mut *self.0.lock());
        for task in tasks {
            task.abort_handle_for_test().abort();
            // No fixture sends a successful pairing response. wait() either
            // joins the aborted registration or receives failure after its
            // worker was dropped; it is not a successful serve-task join.
            let _ = task.wait().await;
        }
    }
}

struct WaitingStart<'a> {
    lifecycle: Option<parking_lot::MutexGuard<'a, ()>>,
    worker: Option<JoinHandle<Result<ControlledRegistrationTask, String>>>,
    stop: Option<JoinHandle<ControlledStatus>>,
    registrations: RegistrationScopes,
}

impl WaitingStart<'_> {
    fn finish(mut self) -> ControlledStatus {
        drop(self.lifecycle.take());
        let task = self.worker.take().unwrap().join().unwrap().unwrap();
        self.registrations.remember(task);
        self.stop.take().unwrap().join().unwrap()
    }
}

impl Drop for WaitingStart<'_> {
    fn drop(&mut self) {
        // Release L before joining a worker that legitimately holds C while
        // waiting for L, then join the Stop waiting for C. This order also
        // applies when the independent lock probe detects a regression.
        drop(self.lifecycle.take());
        if let Some(worker) = self.worker.take()
            && let Ok(Ok(task)) = worker.join()
        {
            self.registrations.remember(task);
        }
        if let Some(stop) = self.stop.take() {
            let _ = stop.join();
        }
    }
}

impl AdmissionFixture {
    async fn assert_retired_worker_rejected(
        &self,
        operation: ControlledOperation,
        business: Arc<dyn ControlledBusinessPort>,
        registrations: &RegistrationScopes,
    ) {
        match ControlledRelayWorker::start(
            self.app.clone(),
            self.relay.clone(),
            operation,
            business,
        )
        .await
        {
            Err(error) => assert_eq!(error, "desktop_remote_stale_result"),
            Ok(task) => {
                registrations.remember(task);
                panic!("a retired Controlled operation reached relay startup");
            }
        }
        self.relay.assert_controlled_start_absent_for_test();
    }

    async fn observe_successor_registration(
        &self,
        listener: &tokio::net::TcpListener,
        operation: ControlledOperation,
        business: Arc<dyn ControlledBusinessPort>,
        registrations: &RegistrationScopes,
    ) {
        let task = ControlledRelayWorker::start(
            self.app.clone(),
            self.relay.clone(),
            operation.clone(),
            business,
        )
        .await
        .unwrap();
        registrations.remember(task);
        let (stream, _) = listener.accept().await.unwrap();
        let mut socket = tokio_tungstenite::accept_hdr_async(
            stream,
            crate::features::remote_control::controller::test_support::ControllerReaderProtocol,
        )
        .await
        .unwrap();
        let Message::Text(frame) = socket.next().await.unwrap().unwrap() else {
            panic!("missing production endpoint registration");
        };
        let frame: Value = serde_json::from_str(&frame).unwrap();
        assert_eq!(frame["type"], "desktop_endpoint_register");
        socket
            .send(Message::Text(
                json!({"v":2,"type":"desktop_endpoint_registered",
                    "endpoint_id":frame["endpoint_id"],"web_client_connected":false})
                .to_string()
                .into(),
            ))
            .await
            .unwrap();
        let Message::Text(frame) = socket.next().await.unwrap().unwrap() else {
            panic!("missing production pairing registration");
        };
        let frame: Value = serde_json::from_str(&frame).unwrap();
        assert_eq!(frame["type"], "desktop_pairing_register");
        assert_eq!(frame["remote_control_generation"], operation.generation());
        assert!(
            frame["registration_request_id"]
                .as_str()
                .is_some_and(|s| !s.is_empty())
        );
        assert!(
            frame["code_wrap_public_key_b64"]
                .as_str()
                .is_some_and(|s| !s.is_empty())
        );
        let controller = self.app.state::<ControlledDeviceManager>();
        assert_eq!(controller.current_operation(), Some(operation.clone()));
        controller.begin_stop();
        self.relay.stop_controller_relay(operation.generation());
        assert_eq!(
            controller.finish_stop().kind(),
            ControlledStatusKind::Stopped
        );
        registrations.retire().await;
        self.relay.shutdown_loopback_test_endpoint();
        let _ = socket.close(None).await;
    }
}

pub(super) async fn run(case: &str) {
    let directory = tempfile::tempdir().unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("ws://{}/relay", listener.local_addr().unwrap());
    // Only the exact private child executes this entry, before host setup.
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("PINVOU_REMOTE_RELAY_WS_URL", &url);
        std::env::set_var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL", &url);
    }
    let mut context = crate::build_tauri_context();
    context.config_mut().app.windows.clear();
    let host = tauri::Builder::default().any_thread()
            .manage(ControlledDeviceManager::default())
            .manage(crate::features::remote_control::controller::encrypted_lane::ControlledLaneState::default())
            .manage(crate::features::remote_control::controller::relay_runtime::ControlledOutboundState::default())
            .build(context).unwrap();
    let app = host.handle().clone();
    let relay = RemoteControlManager::new(app.clone());
    assert!(app.manage(relay.clone()));
    app.state::<ControlledDeviceManager>()
        .register_cleanup_sink(Arc::new(
            crate::app::commands::desktop_remote_controller_business::DesktopControlledCleanup::new(
                app.clone(),
            ),
        ));
    let fixture = AdmissionFixture {
        app,
        relay: relay.clone(),
    };
    let _endpoint = EndpointScope(relay.clone());
    let controller = fixture.app.state::<ControlledDeviceManager>();
    let old = controller.start(None, Vec::new()).unwrap();
    let business: Arc<dyn ControlledBusinessPort> = Arc::new(
        crate::app::commands::desktop_remote_controller_business::DesktopControlledBusiness {
            app: fixture.app.clone(),
        },
    );
    let registrations = RegistrationScopes::default();
    // Preserve the original assertion panic, but first abort and await
    // every worker this fixture actually created. No blocking join relies
    // on the host's own async runtime making progress during unwinding.
    let outcome = AssertUnwindSafe(async {
        let next = match case {
            "stop-first" => {
                controller.begin_stop();
                relay.stop_controller_relay(old.generation());
                assert_eq!(
                    controller.finish_stop().kind(),
                    ControlledStatusKind::Stopped
                );
                fixture
                    .assert_retired_worker_rejected(old, business.clone(), &registrations)
                    .await;
                assert_eq!(controller.status().kind(), ControlledStatusKind::Stopped);
                controller.start(None, Vec::new()).unwrap()
            }
            "refresh-first" => {
                let retiring = controller.begin_refresh().unwrap();
                assert_eq!(retiring.generation(), old.generation());
                let next = controller.complete_refresh_revoked(retiring).unwrap();
                fixture
                    .assert_retired_worker_rejected(old, business.clone(), &registrations)
                    .await;
                assert_eq!(controller.current_operation(), Some(next.clone()));
                assert_eq!(
                    controller.status().kind(),
                    ControlledStatusKind::Registering
                );
                next
            }
            "admission-first" => {
                let lifecycle_mutex = relay.controller_lifecycle_for_test();
                let lifecycle = lifecycle_mutex.lock();
                let worker_app = fixture.app.clone();
                let worker_relay = relay.clone();
                let worker_operation = old.clone();
                let worker_business = business.clone();
                let worker = std::thread::spawn(move || {
                    tokio::runtime::Builder::new_current_thread()
                        .enable_all()
                        .build()
                        .unwrap()
                        .block_on(ControlledRelayWorker::start(
                            worker_app,
                            worker_relay,
                            worker_operation,
                            worker_business,
                        ))
                });
                let mut pending = WaitingStart {
                    lifecycle: Some(lifecycle),
                    worker: Some(worker),
                    stop: None,
                    registrations: registrations.clone(),
                };
                let probe_app = fixture.app.clone();
                let holds_admission = std::thread::spawn(move || {
                    let controller = probe_app.state::<ControlledDeviceManager>();
                    let deadline = std::time::Instant::now() + Duration::from_secs(10);
                    while std::time::Instant::now() < deadline {
                        if !controller.registration_start_guard_available_for_test() {
                            return true;
                        }
                        std::thread::yield_now();
                    }
                    false
                })
                .join()
                .unwrap();
                assert!(holds_admission, "worker must own C while waiting for L");
                relay.assert_controlled_start_absent_for_test();
                let stop_app = fixture.app.clone();
                let stop_relay = relay.clone();
                let stop_generation = old.generation().to_owned();
                pending.stop = Some(std::thread::spawn(move || {
                    let controller = stop_app.state::<ControlledDeviceManager>();
                    controller.begin_stop();
                    stop_relay.stop_controller_relay(&stop_generation);
                    controller.finish_stop()
                }));
                let stopped = pending.finish();
                assert_eq!(stopped.kind(), ControlledStatusKind::Stopped);
                relay.assert_controller_lane_absent_for_test();
                let endpoint_id = relay.controller_endpoint_id().unwrap();
                assert_eq!(
                    relay.start(false).unwrap().endpoint_id,
                    endpoint_id,
                    "a committed Legacy endpoint remains shared after narrow Stop"
                );
                registrations.retire().await;
                controller.start(None, Vec::new()).unwrap()
            }
            "setup-error" => {
                let path = relay.invalidate_start_config_for_test();
                match ControlledRelayWorker::start(
                    fixture.app.clone(),
                    relay.clone(),
                    old.clone(),
                    business.clone(),
                )
                .await
                {
                    Err(error) => assert!(!error.is_empty()),
                    Ok(task) => {
                        registrations.remember(task);
                        panic!("invalid config unexpectedly started a worker");
                    }
                }
                assert!(controller.registration_start_guard_available_for_test());
                relay.assert_controller_lane_absent_for_test();
                controller.stop();
                std::fs::remove_file(path).unwrap();
                controller.start(None, Vec::new()).unwrap()
            }
            _ => panic!("unknown Controlled admission case"),
        };
        fixture
            .observe_successor_registration(&listener, next, business, &registrations)
            .await;
    })
    .catch_unwind()
    .await;
    registrations.retire().await;
    if let Err(panic) = outcome {
        std::panic::resume_unwind(panic);
    }
}
