//! App-owned real-host entry. Only the operation boundary is paused; startup,
//! reader dispatch, persistence, authority retirement and cleanup stay real.
use super::persistence::{pending_revocations_path, rpc_ledger_path};
use super::*;
use std::sync::mpsc;
use std::thread::JoinHandle;

struct PendingStart {
    release: Option<mpsc::Sender<()>>,
    thread: Option<JoinHandle<Result<ControllerRelayChannel, String>>>,
}

impl PendingStart {
    fn pause(manager: &RemoteControlManager, generation: &'static str) -> Self {
        let (installed, observed) = mpsc::channel();
        let (release, released) = mpsc::channel();
        *manager.controller_start_hook.lock() = Some(Box::new(move || {
            installed.send(()).unwrap();
            released.recv_timeout(Duration::from_secs(10)).unwrap();
        }));
        let worker = manager.clone();
        let thread = std::thread::spawn(move || {
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap()
                .block_on(worker.start_controller_relay(generation))
        });
        let pending = Self {
            release: Some(release),
            thread: Some(thread),
        };
        observed.recv_timeout(Duration::from_secs(10)).unwrap();
        pending
    }

    fn finish(mut self) -> Result<ControllerRelayChannel, String> {
        self.unpause();
        self.thread.take().unwrap().join().unwrap()
    }

    fn unpause(&mut self) {
        if let Some(release) = self.release.take() {
            let _ = release.send(());
        }
    }
}

impl Drop for PendingStart {
    fn drop(&mut self) {
        // Assertion failures still release/join the exact test-owned thread.
        self.unpause();
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

struct PendingReader {
    release: Option<mpsc::Sender<()>>,
    thread: Option<JoinHandle<()>>,
}

impl PendingReader {
    fn pause(manager: &RemoteControlManager, endpoint_id: &str, reader: &RelaySender) -> Self {
        let (retired, observed) = mpsc::channel();
        let (release, released) = mpsc::channel();
        *manager.reader_retired_hook.lock() = Some(Box::new(move || {
            retired.send(()).unwrap();
            released.recv_timeout(Duration::from_secs(10)).unwrap();
        }));
        let manager = manager.clone();
        let endpoint_id = endpoint_id.to_owned();
        let reader = reader.clone();
        let thread = std::thread::spawn(move || {
            manager.handle_relay_message(
                json!({"v":2,"type":"desktop_endpoint_revoked","endpoint_id":endpoint_id}),
                &reader,
            )
        });
        let pending = Self {
            release: Some(release),
            thread: Some(thread),
        };
        observed.recv_timeout(Duration::from_secs(10)).unwrap();
        pending
    }

    fn finish(mut self) {
        self.unpause();
        self.thread.take().unwrap().join().unwrap();
    }

    fn unpause(&mut self) {
        if let Some(release) = self.release.take() {
            let _ = release.send(());
        }
    }
}

impl Drop for PendingReader {
    fn drop(&mut self) {
        self.unpause();
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

struct CleanupTransition<T> {
    cleanup: Option<PendingReader>,
    transition: Option<JoinHandle<T>>,
}

impl<T> CleanupTransition<T> {
    fn finish(mut self) -> T {
        self.cleanup.take().unwrap().finish();
        self.transition.take().unwrap().join().unwrap()
    }
}

impl<T> Drop for CleanupTransition<T> {
    fn drop(&mut self) {
        drop(self.cleanup.take());
        if let Some(thread) = self.transition.take() {
            let _ = thread.join();
        }
    }
}

struct EndpointGuard(RemoteControlManager);
impl Drop for EndpointGuard {
    fn drop(&mut self) {
        self.0.shutdown_loopback_test_endpoint();
    }
}

struct PendingTransition<T> {
    start: Option<PendingStart>,
    transition: Option<JoinHandle<T>>,
}

impl<T> PendingTransition<T> {
    fn finish(mut self) -> (Result<ControllerRelayChannel, String>, T) {
        let started = self.start.take().unwrap().finish();
        let transitioned = self.transition.take().unwrap().join().unwrap();
        (started, transitioned)
    }
}

impl<T> Drop for PendingTransition<T> {
    fn drop(&mut self) {
        // Unpause/join startup before joining a transition waiting for its lock.
        drop(self.start.take());
        if let Some(thread) = self.transition.take() {
            let _ = thread.join();
        }
    }
}

impl RemoteControlManager {
    pub(crate) async fn verify_controller_start_fence<T>(
        build_host: impl FnOnce() -> (Self, T),
        case: &str,
    ) {
        let directory = tempfile::tempdir().unwrap();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("ws://{}/relay", listener.local_addr().unwrap());
        // The app driver invokes only this exact test in a private child.
        unsafe {
            std::env::set_var("PINVOU3_HOME", directory.path());
            std::env::set_var("PINVOU_REMOTE_RELAY_WS_URL", &url);
            std::env::set_var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL", &url);
        }
        let (manager, _host) = build_host();
        let _endpoint = EndpointGuard(manager.clone());
        match case {
            "lane-stop" => {
                let pending = PendingStart::pause(&manager, "controller-start-lane-stop");
                // This narrow operation is inner-only and can retire a pending
                // sender even while startup holds lifecycle. The owner fence
                // must reject before minting/persisting config or spawning a socket.
                manager.stop_controller_relay("controller-start-lane-stop");
                assert_eq!(
                    pending.finish().err().as_deref(),
                    Some("desktop_controller_generation_stale")
                );
                assert!(load_config().unwrap().is_none());
                assert!(manager.inner.lock().endpoint.is_none());
                let next = manager
                    .start_controller_relay("controller-start-next")
                    .await
                    .unwrap();
                assert!(manager.inner.lock().endpoint.is_some());
                assert!(load_config().unwrap().is_some());
                drop(next);
            }
            "reader-revocation" => {
                let previous = manager.start(false).unwrap();
                let reader = manager
                    .inner
                    .lock()
                    .endpoint
                    .as_ref()
                    .unwrap()
                    .sender
                    .clone();
                let pending = PendingStart::pause(&manager, "controller-start-revoked");
                assert!(manager.lifecycle.try_lock().is_none());
                let (attempt, observed) = mpsc::channel();
                let retiring = manager.clone();
                let revoke = std::thread::spawn(move || {
                    attempt.send(()).unwrap();
                    retiring.handle_relay_message(
                        json!({"v":2,"type":"desktop_endpoint_revoked","endpoint_id":previous.endpoint_id}), &reader,
                    );
                });
                let transition = PendingTransition {
                    start: Some(pending),
                    transition: Some(revoke),
                };
                observed.recv_timeout(Duration::from_secs(10)).unwrap();
                // Startup linearizes first, then the reader retires that exact
                // endpoint and its files. Never synchronously wait for a reader
                // while retaining the deliberately paused lifecycle owner.
                let (started, ()) = transition.finish();
                let old = started.unwrap();
                assert!(load_config().unwrap().is_none());
                assert!(manager.inner.lock().endpoint.is_none());
                let next = manager
                    .start_controller_relay("controller-start-after-revoke")
                    .await
                    .unwrap();
                let binding = next.binding;
                drop(old);
                assert_eq!(manager.inner.lock().controller_binding, Some(binding));
                drop(next);
            }
            "cleanup-refresh" => {
                let previous = manager.start(false).unwrap();
                let reader = manager
                    .inner
                    .lock()
                    .endpoint
                    .as_ref()
                    .unwrap()
                    .sender
                    .clone();
                let cleanup = PendingReader::pause(&manager, &previous.endpoint_id, &reader);
                assert!(
                    manager.lifecycle.try_lock().is_none(),
                    "retirement and both unlinks must share the lifecycle commit"
                );
                assert!(
                    manager.inner.try_lock().is_some(),
                    "no inner guard is retained by the reader hook"
                );
                assert!(
                    config_path().exists(),
                    "barrier must precede actual file removal"
                );
                let (attempt, observed) = mpsc::channel();
                let refreshing = manager.clone();
                let refresh = std::thread::spawn(move || {
                    attempt.send(()).unwrap();
                    refreshing.refresh()
                });
                let transition = CleanupTransition {
                    cleanup: Some(cleanup),
                    transition: Some(refresh),
                };
                observed.recv_timeout(Duration::from_secs(10)).unwrap();
                let refreshed = transition.finish().unwrap();
                assert_ne!(refreshed.endpoint_id, previous.endpoint_id);
                assert_eq!(
                    load_config().unwrap().unwrap().endpoint_id,
                    refreshed.endpoint_id
                );
                let ledger: RpcLedger =
                    serde_json::from_slice(&std::fs::read(rpc_ledger_path()).unwrap()).unwrap();
                assert_eq!(
                    ledger.endpoint_id, refreshed.endpoint_id,
                    "old reader must not unlink the successor ledger"
                );
                let next = manager
                    .start_controller_relay("controller-start-cleanup-next")
                    .await
                    .unwrap();
                assert_eq!(
                    manager.controller_endpoint_id().unwrap(),
                    refreshed.endpoint_id
                );
                drop(next);
            }
            "stop" => {
                let pending = PendingStart::pause(&manager, "controller-start-stop");
                assert!(
                    manager.lifecycle.try_lock().is_none(),
                    "stop cannot overtake installed startup"
                );
                let (attempt, observed) = mpsc::channel();
                let stopping = manager.clone();
                let stop = std::thread::spawn(move || {
                    attempt.send(()).unwrap();
                    stopping.stop_current()
                });
                let transition = PendingTransition {
                    start: Some(pending),
                    transition: Some(stop),
                };
                observed.recv_timeout(Duration::from_secs(10)).unwrap();
                let (started, stopped) = transition.finish();
                let channel = started.unwrap();
                stopped.unwrap();
                assert!(load_config().unwrap().is_none());
                assert!(manager.inner.lock().endpoint.is_none());
                assert!(manager.inner.lock().controller_inbound.is_none());
                assert!(
                    !load_pending_revocations_for_replay()
                        .unwrap()
                        .entries
                        .is_empty(),
                    "stop must retain durable revocation evidence"
                );
                drop(channel);
            }
            "refresh" => {
                let previous = manager.start(false).unwrap();
                let pending = PendingStart::pause(&manager, "controller-start-refresh");
                assert!(manager.lifecycle.try_lock().is_none());
                let (attempt, observed) = mpsc::channel();
                let refreshing = manager.clone();
                let refresh = std::thread::spawn(move || {
                    attempt.send(()).unwrap();
                    refreshing.refresh()
                });
                let transition = PendingTransition {
                    start: Some(pending),
                    transition: Some(refresh),
                };
                observed.recv_timeout(Duration::from_secs(10)).unwrap();
                let (started, refreshed) = transition.finish();
                let old = started.unwrap();
                let refreshed = refreshed.unwrap();
                assert_ne!(refreshed.endpoint_id, previous.endpoint_id);
                assert_eq!(
                    load_config().unwrap().unwrap().endpoint_id,
                    refreshed.endpoint_id
                );
                let next = manager
                    .start_controller_relay("controller-start-new")
                    .await
                    .unwrap();
                let binding = next.binding;
                drop(old);
                assert_eq!(manager.inner.lock().controller_binding, Some(binding));
                assert_eq!(
                    manager.controller_endpoint_id().unwrap(),
                    refreshed.endpoint_id
                );
                drop(next);
            }
            "failed-start" => {
                let previous = manager.start(false).unwrap();
                let owner = manager
                    .inner
                    .lock()
                    .endpoint
                    .as_ref()
                    .unwrap()
                    .sender
                    .clone();
                let bytes = std::fs::read(config_path()).unwrap();
                let unrelated = fresh_config(false);
                queue_pending_revocation_for_removal(&unrelated).unwrap();
                let revocations = std::fs::read(pending_revocations_path()).unwrap();
                std::fs::write(config_path(), b"invalid-json").unwrap();
                assert!(
                    manager
                        .start_controller_relay("controller-start-failed")
                        .await
                        .is_err()
                );
                let inner = manager.inner.lock();
                assert!(inner.controller_inbound.is_none());
                let endpoint = inner.endpoint.as_ref().unwrap();
                assert!(endpoint.sender.same_channel(&owner));
                assert_eq!(endpoint.config.endpoint_id, previous.endpoint_id);
                drop(inner);
                assert_eq!(
                    std::fs::read(pending_revocations_path()).unwrap(),
                    revocations
                );
                std::fs::write(config_path(), bytes).unwrap();
                let next = manager
                    .start_controller_relay("controller-start-recovered")
                    .await
                    .unwrap();
                assert_eq!(
                    manager.controller_endpoint_id().unwrap(),
                    previous.endpoint_id
                );
                drop(next);
            }
            "late-drop" => {
                let old = manager
                    .start_controller_relay("controller-start-reused")
                    .await
                    .unwrap();
                let endpoint_id = manager.controller_endpoint_id().unwrap();
                manager.stop_controller_relay("controller-start-reused");
                let next = manager
                    .start_controller_relay("controller-start-reused")
                    .await
                    .unwrap();
                assert_ne!(old.binding, next.binding);
                let binding = next.binding;
                drop(old);
                assert_eq!(manager.inner.lock().controller_binding, Some(binding));
                assert_eq!(
                    manager.start(false).unwrap().endpoint_id,
                    endpoint_id,
                    "legacy endpoint remains shared"
                );
                drop(next);
            }
            _ => panic!("unknown controller-start case"),
        }
    }
}
