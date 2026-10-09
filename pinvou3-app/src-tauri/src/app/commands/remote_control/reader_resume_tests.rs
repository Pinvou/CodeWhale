//! Process-private Windows host driver for the feature's real reader regression.
use std::io::Read;
use std::time::{Duration, Instant};

#[test]
fn real_reader_manager_worker_resume_round_trip() {
    const CHILD: &str = "PINVOU_READER_RESUME_TEST_CHILD";
    if std::env::var(CHILD).as_deref() == Ok("1") {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            tokio::time::timeout(Duration::from_secs(15),
                crate::features::remote_control::controller::relay_runtime::reader_resume_tests::resume_round_trip(|| {
                    let mut context = crate::build_tauri_context();
                    context.config_mut().app.windows.clear();
                    let app = tauri::Builder::default().any_thread()
                        .manage(crate::features::remote_control::controller::controlled::ControlledDeviceManager::default())
                        .manage(crate::features::remote_control::controller::encrypted_lane::ControlledLaneState::default())
                        .manage(crate::features::remote_control::controller::relay_runtime::ControlledOutboundState::default())
                        .build(context).unwrap();
                    (app.handle().clone(), app)
                }))
                .await
                .unwrap();
        });
        return;
    }
    // A separate process owns Tauri's global runtime and path/env overrides.
    // Never alter the test runner's home or share its live managed states.
    let module = module_path!().split_once("::").unwrap().1;
    let filter = format!("{module}::real_reader_manager_worker_resume_round_trip");
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
