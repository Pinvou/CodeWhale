//! Process-private Windows host driver for the feature's real reader regression.
use std::io::Read;
use std::time::{Duration, Instant};
use tauri::Manager as _;

#[test]
fn real_reader_manager_worker_resume_round_trip() {
    run_isolated_reader(false, false, false, None);
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
    let mut child = std::process::Command::new(std::env::current_exe().unwrap())
        .args(["--exact", &filter, "--nocapture"])
        .env(CHILD, "1")
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(25);
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
    let mut output = String::new();
    child
        .stdout
        .take()
        .unwrap()
        .read_to_string(&mut output)
        .unwrap();
    let mut errors = String::new();
    child
        .stderr
        .take()
        .unwrap()
        .read_to_string(&mut errors)
        .unwrap();
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
    let module = module_path!().split_once("::").unwrap().1;
    let name = match owner_case {
        Some("disconnect") => "real_reader_old_received_disconnect_cannot_mutate_successor",
        Some("joined") => "real_reader_old_received_joined_cannot_install_or_clear_successor",
        Some("activation") => "real_reader_resume_activation_holds_lifecycle_owner_commit",
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
    let filter = format!("{module}::{name}");
    let mut child = std::process::Command::new(std::env::current_exe().unwrap())
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
    let child_wait = if owner_case.is_some() {
        50
    } else {
        child_budget + 10
    };
    let deadline = Instant::now() + Duration::from_secs(child_wait);
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
    let mut output = String::new();
    child
        .stdout
        .take()
        .unwrap()
        .read_to_string(&mut output)
        .unwrap();
    let mut errors = String::new();
    child
        .stderr
        .take()
        .unwrap()
        .read_to_string(&mut errors)
        .unwrap();
    assert!(
        status.success(),
        "isolated regression failed: {output}\n{errors}"
    );
    assert!(
        output.contains("test result: ok. 1 passed;"),
        "zero-match child is not evidence"
    );
}
