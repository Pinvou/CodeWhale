//! Process-private Windows host driver for the feature's real reader regression.
use std::io::Read;
use std::time::{Duration, Instant};

#[test]
fn real_reader_manager_worker_resume_round_trip() {
    run_isolated_reader(false, None);
}

#[test]
fn real_reader_endpoint_retirement_closes_waiting_and_connected_workers() {
    run_isolated_reader(true, None);
}

#[test]
fn real_reader_old_received_disconnect_cannot_mutate_successor() {
    run_isolated_reader(false, Some("disconnect"));
}

#[test]
fn real_reader_old_received_joined_cannot_install_or_clear_successor() {
    run_isolated_reader(false, Some("joined"));
}

#[test]
fn real_reader_old_tick_challenge_cannot_mask_successor_suspicion() {
    run_isolated_reader(false, Some("challenge"));
}

#[test]
fn real_reader_resume_activation_holds_lifecycle_owner_commit() {
    run_isolated_reader(false, Some("activation"));
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

fn run_isolated_reader(retire_endpoint: bool, owner_case: Option<&str>) {
    const CHILD: &str = "PINVOU_READER_RESUME_TEST_CHILD";
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
            tokio::time::timeout(Duration::from_secs(if concurrent_race { 40 } else { 15 }),
                crate::features::remote_control::controller::relay_runtime::reader_resume_tests::resume_round_trip(|| {
                    let mut context = crate::build_tauri_context();
                    context.config_mut().app.windows.clear();
                    let app = tauri::Builder::default().any_thread()
                        .manage(crate::features::remote_control::controller::controlled::ControlledDeviceManager::default())
                        .manage(crate::features::remote_control::controller::encrypted_lane::ControlledLaneState::default())
                        .manage(crate::features::remote_control::controller::relay_runtime::ControlledOutboundState::default())
                        .build(context).unwrap();
                    (app.handle().clone(), app)
                }, retire_endpoint))
                .await
                .unwrap();
        });
        return;
    }
    // A separate process owns Tauri's global runtime and path/env overrides.
    // Never alter the test runner's home or share its live managed states.
    let module = module_path!().split_once("::").unwrap().1;
    let name = match owner_case {
        Some("disconnect") => "real_reader_old_received_disconnect_cannot_mutate_successor",
        Some("joined") => "real_reader_old_received_joined_cannot_install_or_clear_successor",
        Some("activation") => "real_reader_resume_activation_holds_lifecycle_owner_commit",
        Some("challenge") => "real_reader_old_tick_challenge_cannot_mask_successor_suspicion",
        Some(other) => panic!("unknown isolated ownership case: {other}"),
        None if retire_endpoint => {
            "real_reader_endpoint_retirement_closes_waiting_and_connected_workers"
        }
        None => "real_reader_manager_worker_resume_round_trip",
    };
    let filter = format!("{module}::{name}");
    let child = std::process::Command::new(std::env::current_exe().unwrap())
        .args(["--exact", &filter, "--nocapture"])
        .env(CHILD, "1")
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .unwrap();
    let (status, output, errors) = collect_reader_child(
        child,
        Duration::from_secs(if owner_case.is_some() { 50 } else { 25 }),
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
