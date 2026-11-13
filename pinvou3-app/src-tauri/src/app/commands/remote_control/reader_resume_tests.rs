//! Process-private Windows host driver for the feature's real reader regression.
use std::io::Read;
use std::time::{Duration, Instant};
use tauri::Manager as _;

#[test]
fn real_registration_task_exit_closes_start_refresh_and_preserves_successors() {
    const CHILD: &str = "PINVOU_READER_RESUME_TEST_CHILD";
    if std::env::var(CHILD).as_deref() != Ok("1") {
        run_reader_child(
            "real_registration_task_exit_closes_start_refresh_and_preserves_successors",
            false,
            false,
            25,
        );
        return;
    }
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(async {
        use crate::features::remote_control::controller::controlled::{ControlledDeviceManager, ControlledStatusKind};
        use crate::features::remote_control::controller::relay_runtime::ControlledRelayWorker;
        use crate::features::remote_control::RemoteControlManager;
        use futures_util::{SinkExt, StreamExt};
        use serde_json::{Value, json};
        use tokio_tungstenite::tungstenite::Message;
        let home = tempfile::tempdir().unwrap();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("ws://{}/relay", listener.local_addr().unwrap());
        // This process owns all runtime overrides and its ephemeral endpoint.
        unsafe {
            std::env::set_var("PINVOU3_HOME", home.path());
            std::env::set_var("PINVOU_REMOTE_RELAY_WS_URL", &url);
            std::env::set_var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL", &url);
        }
        let mut context = crate::build_tauri_context();
        context.config_mut().app.windows.clear();
        let app = tauri::Builder::default().any_thread()
            .manage(ControlledDeviceManager::default())
            .build(context).unwrap();
        let handle = app.handle().clone();
        let relay = RemoteControlManager::new(handle.clone());
        let manager = handle.state::<ControlledDeviceManager>();
        let mut socket = None;
        for refreshed in [false, true] {
            // No race / stopped / replacement start / replacement refresh.
            for interference in 0..4 {
                for while_waiting in [false, true] {
                    let initial = manager.start(None, Vec::new()).unwrap();
                    let operation = if refreshed {
                        let retiring = manager.begin_refresh().unwrap();
                        manager.complete_refresh_revoked(retiring).unwrap()
                    } else { initial };
                    let registration = ControlledRelayWorker::start(
                        handle.clone(), relay.clone(), operation.clone(),
                        std::sync::Arc::new(super::super::desktop_remote_controller_business::DesktopControlledBusiness { app:handle.clone() }),
                    ).await.unwrap();
                    if socket.is_none() {
                        let (stream, _) = listener.accept().await.unwrap();
                        let mut connected = tokio_tungstenite::accept_hdr_async(stream,
                            crate::features::remote_control::controller::test_support::ControllerReaderProtocol).await.unwrap();
                        let Message::Text(frame) = connected.next().await.unwrap().unwrap() else { panic!("endpoint register") };
                        let frame:Value=serde_json::from_str(&frame).unwrap();
                        assert_eq!(frame["type"], "desktop_endpoint_register");
                        connected.send(Message::Text(json!({"v":2,"type":"desktop_endpoint_registered","endpoint_id":frame["endpoint_id"],"web_client_connected":false}).to_string().into())).await.unwrap();
                        socket=Some(connected);
                    }
                    // Observe the actual production registration request before
                    // aborting its real task; no result or watchdog is fabricated.
                    loop {
                        match socket.as_mut().unwrap().next().await.unwrap().unwrap() {
                            Message::Text(text) => {
                                let frame:Value=serde_json::from_str(&text).unwrap();
                                if frame["type"]=="desktop_pairing_register" {
                                    assert_eq!(frame["remote_control_generation"], operation.generation());
                                    break;
                                }
                            },
                            Message::Ping(data) => socket.as_mut().unwrap().send(Message::Pong(data)).await.unwrap(),
                            _ => {},
                        }
                    }
                    assert_eq!(manager.status().kind(), ControlledStatusKind::Registering);
                    let abort = registration.abort_handle_for_test();
                    let mut emitted = None;
                    let mut replacement_channel = None;
                    {
                        let wait = super::super::desktop_remote_controlled::wait_controlled_registration(
                            &manager, operation.clone(), registration.wait(),
                            |generation| relay.stop_controller_relay(generation),
                            |status| emitted=Some(status.clone()),
                        );
                        tokio::pin!(wait);
                        if while_waiting {
                            std::future::poll_fn(|cx| {
                                assert!(std::future::Future::poll(wait.as_mut(),cx).is_pending());
                                std::task::Poll::Ready(())
                            }).await;
                        }
                        if interference != 0 {
                            manager.stop();
                            relay.stop_controller_relay(operation.generation());
                        }
                        if interference >= 2 {
                            manager.start(None, Vec::new()).unwrap();
                            if interference == 3 {
                                let old=manager.begin_refresh().unwrap();
                                manager.complete_refresh_revoked(old).unwrap();
                            }
                            let next = manager.current_operation().unwrap();
                            replacement_channel = Some(relay.start_controller_relay(next.generation()).await.unwrap());
                        }
                        abort.abort();
                        let error=tokio::time::timeout(Duration::from_secs(3),wait).await.unwrap().unwrap_err();
                        assert_eq!(error,"pairing_registration_failed");
                    }
                    if interference==0 {
                        assert_eq!(manager.status().kind(),ControlledStatusKind::Error);
                        assert!(emitted.is_some());
                    } else {
                        assert!(emitted.is_none());
                        assert_eq!(manager.status().kind(),if interference==1 {ControlledStatusKind::Stopped} else {ControlledStatusKind::Registering});
                    }
                    // A real lane can be installed again after the old task is
                    // joined. A stale command must not retire this new owner.
                    if interference<=1 { manager.start(None,Vec::new()).unwrap(); }
                    let next=manager.current_operation().unwrap();
                    let channel = if let Some(channel) = replacement_channel {
                        assert!(matches!(relay.start_controller_relay(next.generation()).await,
                            Err(code) if code=="desktop_controller_relay_busy"));
                        channel
                    } else {
                        relay.start_controller_relay(next.generation()).await.unwrap()
                    };
                    assert!(manager.is_pending_handshake(next.clone()));
                    manager.stop();
                    relay.stop_controller_relay(next.generation());
                    drop(channel);
                }
            }
        }
        relay.shutdown_loopback_test_endpoint();
        if let Some(mut socket)=socket {let _=socket.close(None).await;}
    });
}

#[test]
fn real_reader_manager_worker_resume_round_trip() {
    run_isolated_reader(false, false, false, None);
}

#[test]
fn real_reader_resume_prepare_survives_failed_write() {
    run_isolated_reader(false, false, false, Some("prepare_write"));
}

#[test]
fn real_reader_resume_prepare_survives_legacy_socket_epoch() {
    run_isolated_reader(false, false, false, Some("prepare_legacy"));
}

#[test]
fn real_reader_endpoint_retirement_closes_waiting_and_connected_workers() {
    run_isolated_reader(true, false, false, None);
}

#[test]
fn real_reader_preconnection_deadlines_bound_silence_and_self_echo() {
    run_isolated_reader(false, true, false, None);
}

#[test]
fn real_reader_old_received_disconnect_cannot_mutate_successor() {
    run_isolated_reader(false, false, false, Some("disconnect"));
}

#[test]
fn real_reader_old_received_joined_cannot_install_or_clear_successor() {
    run_isolated_reader(false, false, false, Some("joined"));
}

#[test]
fn real_reader_old_tick_challenge_cannot_mask_successor_suspicion() {
    run_isolated_reader(false, false, false, Some("challenge"));
}

#[test]
fn real_reader_resume_activation_holds_lifecycle_owner_commit() {
    run_isolated_reader(false, false, false, Some("activation"));
}

#[test]
fn real_reader_journal_owner_rejects_late_replay_after_fifo_rotation() {
    const CHILD: &str = "PINVOU_READER_RESUME_TEST_CHILD";
    if std::env::var(CHILD).as_deref() != Ok("1") {
        run_reader_child(
            "real_reader_journal_owner_rejects_late_replay_after_fifo_rotation",
            false,
            false,
            15,
        );
        return;
    }
    // The real business poll pauses synchronously outside M/J; other runtime
    // threads must remain free to rotate the manager and authenticate a peer.
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(4)
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(async {
        tokio::time::timeout(Duration::from_secs(60),
            crate::features::remote_control::controller::relay_runtime::reader_resume_tests::journal_owner_tests::journal_owner_round_trip(
                |probe| super::super::desktop_remote_task::journal_owner_tests::host(Some(probe)),
                |handle,lease,remote_ref| {
                    use tauri::Manager as _;
                    handle.state::<std::sync::Arc<super::super::desktop_remote_task::DesktopRemoteTaskRuntime>>()
                        .service().ensure_unclaimed_remote_session(remote_ref,"Journal owner regression",&lease.remote_control_generation).unwrap()
                }
            )).await.unwrap();
    });
}

#[test]
fn real_relay_manager_routes_controller_lane_outage_signal() {
    const CHILD: &str = "PINVOU_READER_RESUME_TEST_CHILD";
    if std::env::var(CHILD).as_deref() == Ok("1") {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            tokio::time::timeout(Duration::from_secs(15),
                crate::features::remote_control::controller::relay_runtime::reader_resume_tests::controller_lane_outage_round_trip(|| {
                    let mut context = crate::build_tauri_context();
                    context.config_mut().app.windows.clear();
                    let app = tauri::Builder::default().any_thread()
                        .manage(crate::features::remote_control::controller::controlled::ControlledDeviceManager::default())
                        .manage(crate::features::remote_control::controller::encrypted_lane::ControlledLaneState::default())
                        .manage(crate::features::remote_control::controller::relay_runtime::ControlledOutboundState::default())
                        .build(context).unwrap();
                    let relay = crate::features::remote_control::RemoteControlManager::new(
                        app.handle().clone(),
                    );
                    (relay, app)
                }))
                .await
                .unwrap();
        });
        return;
    }
    // A separate process owns Tauri's global runtime and path/env overrides.
    // Never alter the test runner's home or share its live managed states.
    let module = module_path!().split_once("::").unwrap().1;
    let filter = format!("{module}::real_relay_manager_routes_controller_lane_outage_signal");
    let child = std::process::Command::new(std::env::current_exe().unwrap())
        .args(["--exact", &filter, "--nocapture"])
        .env(CHILD, "1")
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .unwrap();
    let (status, output, errors) = collect_reader_child(child, Duration::from_secs(25));
    assert!(
        status.success(),
        "isolated regression failed: {output}\n{errors}"
    );
    assert!(
        output.contains("test result: ok. 1 passed;"),
        "zero-match child is not evidence"
    );
}

#[test]
fn real_worker_final_admission_preserves_timeout_and_signed_deadline() {
    run_isolated_reader(false, false, true, None);
}

fn run_isolated_reader(
    retire_endpoint: bool,
    preconnect: bool,
    final_admission: bool,
    owner_case: Option<&str>,
) {
    const CHILD: &str = "PINVOU_READER_RESUME_TEST_CHILD";
    // These two regressions exercise real twenty-second local deadlines twice;
    // they never shorten the production budget through a test-only branch.
    let child_budget = if preconnect || final_admission {
        65
    } else {
        15
    };
    if std::env::var(CHILD).as_deref() == Ok("1") {
        if let Some(case) = owner_case {
            unsafe {
                std::env::set_var("PINVOU_READER_OWNER_CASE", case);
            }
        } else {
            unsafe {
                std::env::remove_var("PINVOU_READER_OWNER_CASE");
            }
        }
        let concurrent_race = matches!(
            owner_case,
            Some("disconnect") | Some("joined") | Some("challenge")
        );
        let runtime = if concurrent_race {
            tokio::runtime::Builder::new_multi_thread()
                .worker_threads(2)
                .enable_all()
                .build()
                .unwrap()
        } else {
            // The activation lock probe has no other C-lock user on this
            // runtime: a dropped commit lock cannot look held by another task.
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap()
        };
        runtime.block_on(async {
            tokio::time::timeout(Duration::from_secs(if concurrent_race { 40 } else { child_budget }),
                crate::features::remote_control::controller::relay_runtime::reader_resume_tests::resume_round_trip(|| {
                    let mut context = crate::build_tauri_context();
                    context.config_mut().app.windows.clear();
                    let app = tauri::Builder::default().any_thread()
                        .manage(crate::features::remote_control::controller::controlled::ControlledDeviceManager::default())
                        .manage(crate::features::remote_control::controller::encrypted_lane::ControlledLaneState::default())
                        .manage(crate::features::remote_control::controller::relay_runtime::ControlledOutboundState::default())
                        .build(context).unwrap();
                    if final_admission {
                        let handle = app.handle().clone();
                        handle.manage(crate::features::remote_control::RemoteControlManager::new(handle.clone()));
                        let cleanup: std::sync::Arc<dyn crate::features::remote_control::controller::controlled::ControlledCleanupSink> = std::sync::Arc::new(super::super::desktop_remote_controller_business::DesktopControlledCleanup::new(handle.clone()));
                        handle.state::<crate::features::remote_control::controller::controlled::ControlledDeviceManager>()
                            .register_cleanup_sink(cleanup.clone());
                        handle.manage(cleanup);
                    }
                    (app.handle().clone(), app)
                }, retire_endpoint))
                .await
                .unwrap();
        });
        return;
    }
    // A separate process owns Tauri's global runtime and path/env overrides.
    // Never alter the test runner's home or share its live managed states.
    let name = match owner_case {
        Some("disconnect") => "real_reader_old_received_disconnect_cannot_mutate_successor",
        Some("joined") => "real_reader_old_received_joined_cannot_install_or_clear_successor",
        Some("activation") => "real_reader_resume_activation_holds_lifecycle_owner_commit",
        Some("prepare_write") => "real_reader_resume_prepare_survives_failed_write",
        Some("prepare_legacy") => "real_reader_resume_prepare_survives_legacy_socket_epoch",
        Some("challenge") => "real_reader_old_tick_challenge_cannot_mask_successor_suspicion",
        Some(other) => panic!("unknown isolated ownership case: {other}"),
        None if final_admission => {
            "real_worker_final_admission_preserves_timeout_and_signed_deadline"
        }
        None if preconnect => "real_reader_preconnection_deadlines_bound_silence_and_self_echo",
        None if retire_endpoint => {
            "real_reader_endpoint_retirement_closes_waiting_and_connected_workers"
        }
        None => "real_reader_manager_worker_resume_round_trip",
    };
    run_reader_child(name, preconnect, final_admission, child_budget);
}

fn run_reader_child(name: &str, preconnect: bool, final_admission: bool, child_budget: u64) {
    const CHILD: &str = "PINVOU_READER_RESUME_TEST_CHILD";
    // A separate process owns Tauri's global runtime and path/env overrides.
    // Never alter the test runner's home or share its live managed states.
    let module = module_path!().split_once("::").unwrap().1;
    let filter = format!("{module}::{name}");
    let child = std::process::Command::new(std::env::current_exe().unwrap())
        .args(["--exact", &filter, "--nocapture"])
        .env(CHILD, "1")
        .env(
            "PINVOU_READER_PRECONNECT_SCENARIO",
            if preconnect { "1" } else { "0" },
        )
        .env(
            "PINVOU_READER_FINAL_ADMISSION_SCENARIO",
            if final_admission { "1" } else { "0" },
        )
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .unwrap();
    let (status, output, errors) = collect_reader_child(
        child,
        Duration::from_secs(
            if name == "real_reader_journal_owner_rejects_late_replay_after_fifo_rotation" {
                70
            } else if name.starts_with("real_reader_old_")
                || name == "real_reader_resume_activation_holds_lifecycle_owner_commit"
            {
                50
            } else {
                child_budget + 10
            },
        ),
    );
    assert!(
        status.success(),
        "isolated regression failed: {output}\n{errors}"
    );
    assert!(
        output.contains("test result: ok. 1 passed;"),
        "zero-match child is not evidence"
    );
}

fn collect_reader_child(
    mut child: std::process::Child,
    budget: Duration,
) -> (std::process::ExitStatus, String, String) {
    let deadline = Instant::now() + budget;
    // Drain both streams while the child is running. Waiting for exit first
    // can fill a pipe and prevent the child from ever reaching its exit.
    let mut stdout = child.stdout.take().unwrap();
    let mut stderr = child.stderr.take().unwrap();
    let (stdout_tx, stdout_rx) = std::sync::mpsc::sync_channel(1);
    let (stderr_tx, stderr_rx) = std::sync::mpsc::sync_channel(1);
    std::thread::spawn(move || {
        let mut output = String::new();
        let _ = stdout_tx.send(stdout.read_to_string(&mut output).map(|_| output));
    });
    std::thread::spawn(move || {
        let mut errors = String::new();
        let _ = stderr_tx.send(stderr.read_to_string(&mut errors).map(|_| errors));
    });
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        if Instant::now() >= deadline {
            // Only the exact child just created by this test is terminated.
            child.kill().unwrap();
            child.wait().unwrap();
            panic!("isolated reader/worker regression exceeded its watchdog");
        }
        std::thread::sleep(Duration::from_millis(10));
    };
    let output = stdout_rx
        .recv_timeout(deadline.saturating_duration_since(Instant::now()))
        .unwrap_or_else(|error| panic!("isolated reader stdout collection failed: {error}"))
        .unwrap();
    let errors = stderr_rx
        .recv_timeout(deadline.saturating_duration_since(Instant::now()))
        .unwrap_or_else(|error| panic!("isolated reader stderr collection failed: {error}"))
        .unwrap();
    (status, output, errors)
}

#[test]
fn reader_child_drains_both_large_output_streams() {
    const PROBE: &str = "PINVOU_READER_PIPE_PROBE_CHILD";
    const SIZE: usize = 1024 * 1024;
    if std::env::var(PROBE).as_deref() == Ok("1") {
        use std::io::Write;
        std::io::stdout().write_all(&vec![b'o'; SIZE]).unwrap();
        std::io::stderr().write_all(&vec![b'e'; SIZE]).unwrap();
        return;
    }
    let module = module_path!().split_once("::").unwrap().1;
    let filter = format!("{module}::reader_child_drains_both_large_output_streams");
    let child = std::process::Command::new(std::env::current_exe().unwrap())
        .args(["--exact", &filter, "--nocapture"])
        .env(PROBE, "1")
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .unwrap();
    let (status, output, errors) = collect_reader_child(child, Duration::from_secs(15));
    assert!(status.success());
    assert!(output.contains(&"o".repeat(SIZE)));
    assert!(errors.contains(&"e".repeat(SIZE)));
    assert!(output.contains("test result: ok. 1 passed;"));
}

#[test]
fn reader_child_watchdog_terminates_a_stalled_child() {
    const PROBE: &str = "PINVOU_READER_STALL_PROBE_CHILD";
    if let Ok(address) = std::env::var(PROBE) {
        use std::io::Write;
        let mut ready = std::net::TcpStream::connect(address).unwrap();
        ready.write_all(&std::process::id().to_le_bytes()).unwrap();
        std::thread::sleep(Duration::from_secs(20));
        return;
    }
    let module = module_path!().split_once("::").unwrap().1;
    let filter = format!("{module}::reader_child_watchdog_terminates_a_stalled_child");
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let mut child = std::process::Command::new(std::env::current_exe().unwrap())
        .args(["--exact", &filter, "--nocapture"])
        .env(PROBE, listener.local_addr().unwrap().to_string())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .unwrap();
    let ready_deadline = Instant::now() + Duration::from_secs(15);
    let ready_result = (|| -> Result<(), String> {
        let mut stream = None;
        let mut pid = [0_u8; 4];
        let mut received = 0;
        loop {
            if Instant::now() >= ready_deadline {
                return Err("readiness deadline expired".into());
            }
            if child.try_wait().map_err(|e| e.to_string())?.is_some() {
                return Err("child exited before readiness".into());
            }
            if stream.is_none() {
                match listener.accept() {
                    Ok((ready, _)) => {
                        ready.set_nonblocking(true).map_err(|e| e.to_string())?;
                        stream = Some(ready);
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                    Err(error) => return Err(error.to_string()),
                }
            }
            if let Some(ready) = stream.as_mut() {
                match ready.read(&mut pid[received..]) {
                    Ok(0) => return Err("readiness connection closed".into()),
                    Ok(count) => {
                        received += count;
                        if received == pid.len() {
                            return if Instant::now() >= ready_deadline {
                                Err("readiness deadline expired".into())
                            } else if u32::from_le_bytes(pid) == child.id() {
                                Ok(())
                            } else {
                                Err("readiness identity invalid".into())
                            };
                        }
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                    Err(error) => return Err(error.to_string()),
                }
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    })();
    if let Err(error) = ready_result {
        let kill = child.kill();
        let wait = child.wait();
        panic!("stall probe readiness failed: {error}; kill={kill:?}; wait={wait:?}");
    }
    let started = Instant::now();
    let result = std::panic::catch_unwind(|| collect_reader_child(child, Duration::from_secs(1)));
    let failure = result.expect_err("a stalled child must not be reported as success");
    assert_eq!(
        failure.downcast_ref::<&str>(),
        Some(&"isolated reader/worker regression exceeded its watchdog")
    );
    assert!(started.elapsed() >= Duration::from_secs(1));
    assert!(started.elapsed() < Duration::from_secs(10));
}
