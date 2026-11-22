use super::*;
use std::os::unix::net::UnixListener;
use std::path::Path;
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, Ordering},
};

struct Reply(u16, Value, Option<Duration>);
fn facts(profile: &Path, card: &str) -> Value {
    serde_json::json!({"models":[{"card":card,"managed":true,"model_id":"Model"}],
        "store_enrollment":1,"store_reader":{"program":std::env::current_exe().unwrap().canonicalize().unwrap(),"profile":profile}})
}
fn initial() -> Value {
    serde_json::json!({"models":[{"card":"old","managed":true}],"store_enrollment":1,"store_reader":null})
}
fn pending() -> Value {
    serde_json::json!({"error":{"code":"store_selection_unavailable","message":"snapshot pending"}})
}

fn fixture(
    mut respond: impl FnMut(usize, &str, &Path) -> Reply + Send + 'static,
    test: impl FnOnce(&Client, &Path, &Arc<Mutex<Vec<String>>>),
) {
    let _lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|e| e.into_inner());
    let _env =
        crate::platform::paths::tests::EnvVarGuard::capture(&["PINVOU3_HOME", "CMSD_SOCKET"]);
    let home = tempfile::tempdir().unwrap();
    let socket = home.path().join("cmsd.sock");
    // SAFETY: the shared project ENV_LOCK covers every request and server join.
    unsafe {
        std::env::set_var("PINVOU3_HOME", home.path());
        std::env::set_var("CMSD_SOCKET", &socket);
    }
    let listener = UnixListener::bind(&socket).unwrap();
    listener.set_nonblocking(true).unwrap();
    let stopping = Arc::new(AtomicBool::new(false));
    let stop = stopping.clone();
    let trace = Arc::new(Mutex::new(Vec::new()));
    let observed = trace.clone();
    let profile = home.path().to_path_buf();
    let worker = std::thread::spawn(move || {
        let mut index = 0;
        while !stop.load(Ordering::Acquire) {
            let (mut stream, _) = match listener.accept() {
                Ok(value) => value,
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    std::thread::sleep(Duration::from_millis(2));
                    continue;
                }
                Err(error) => panic!("fixture accept: {error}"),
            };
            stream
                .set_read_timeout(Some(Duration::from_secs(1)))
                .unwrap();
            let mut head = Vec::new();
            let mut byte = [0];
            while !head.ends_with(b"\r\n\r\n") {
                stream.read_exact(&mut byte).unwrap();
                head.push(byte[0]);
            }
            let head = String::from_utf8(head).unwrap();
            let request = head.lines().next().unwrap().to_string();
            let size = head
                .lines()
                .find_map(|line| {
                    line.strip_prefix("content-length: ")
                        .and_then(|v| v.parse::<usize>().ok())
                })
                .unwrap_or(0);
            let mut body = vec![0; size];
            stream.read_exact(&mut body).unwrap();
            if request.starts_with("POST /v1/store-enrollment ") {
                let body: Value = serde_json::from_slice(&body).unwrap();
                assert_eq!(body["profile"].as_str(), profile.to_str());
            }
            observed.lock().unwrap().push(request.clone());
            let Reply(status, value, delay) = respond(index, &request, &profile);
            index += 1;
            let body = value.to_string();
            if write!(
                stream,
                "HTTP/1.1 {status} Test\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
                body.len()
            )
            .is_err()
            {
                continue;
            }
            if let Some(delay) = delay {
                for byte in body.bytes() {
                    if stream.write_all(&[byte]).is_err() {
                        break;
                    }
                    std::thread::sleep(delay);
                }
            } else {
                let _ = stream.write_all(body.as_bytes());
            }
        }
    });
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        test(&Client { socket }, home.path(), &trace)
    }));
    stopping.store(true, Ordering::Release);
    worker.join().unwrap();
    if let Err(error) = result {
        std::panic::resume_unwind(error);
    }
}

#[test]
fn successful_enrollment_returns_fresh_facts_without_reenrolling() {
    fixture(
        |index, _, profile| match index {
            0 => Reply(200, initial(), None),
            1 => Reply(200, serde_json::json!({"registered":true}), None),
            2 | 3 => Reply(503, pending(), None),
            _ => Reply(200, facts(profile, "current--v2"), None),
        },
        |client, _, trace| {
            assert_eq!(client.models().unwrap()["models"][0]["card"], "current--v2");
            let trace = trace.lock().unwrap();
            assert_eq!(trace.len(), 5);
            assert_eq!(
                trace
                    .iter()
                    .filter(|line| line.starts_with("POST "))
                    .count(),
                1
            );
            assert_eq!(trace[1], "POST /v1/store-enrollment HTTP/1.1");
        },
    );
}

#[test]
fn rejected_registration_preserves_readable_facts() {
    fixture(
        |index, _, _| {
            if index == 0 {
                Reply(200, initial(), None)
            } else {
                Reply(
                    409,
                    serde_json::json!({"error":{"code":"component_maintenance"}}),
                    None,
                )
            }
        },
        |client, _, trace| {
            assert_eq!(client.models().unwrap(), initial());
            assert_eq!(trace.lock().unwrap().len(), 2);
        },
    );
}

#[test]
fn already_registered_reader_with_missing_first_snapshot_recovers() {
    fixture(
        |index, _, profile| match index {
            0 | 2 => Reply(503, pending(), None),
            1 => Reply(200, serde_json::json!({"registered":true}), None),
            _ => Reply(200, facts(profile, "current"), None),
        },
        |client, _, trace| {
            assert_eq!(client.models().unwrap()["models"][0]["card"], "current");
            assert_eq!(trace.lock().unwrap().len(), 4);
        },
    );
}

#[test]
fn a_fresh_response_for_another_profile_remains_unknown() {
    fixture(
        |index, _, profile| match index {
            0 => Reply(200, initial(), None),
            1 => Reply(200, serde_json::json!({"registered":true}), None),
            _ => Reply(200, facts(&profile.join("other"), "current"), None),
        },
        |client, _, trace| {
            assert!(client.models().unwrap_err().contains("binding changed"));
            assert_eq!(trace.lock().unwrap().len(), 3);
        },
    );
}

#[test]
fn unrelated_fresh_http_failure_is_not_retried() {
    fixture(
        |index, _, _| match index {
            0 => Reply(200, initial(), None),
            1 => Reply(200, serde_json::json!({"registered":true}), None),
            _ => Reply(
                503,
                serde_json::json!({"error":{"code":"another_failure"}}),
                None,
            ),
        },
        |client, _, trace| {
            let value = client.models().unwrap();
            assert_eq!(value["error"]["code"], "another_failure");
            assert_eq!(trace.lock().unwrap().len(), 3);
        },
    );
}

#[test]
fn readiness_polling_has_one_overall_deadline() {
    fixture(
        |index, _, _| match index {
            0 => Reply(200, initial(), None),
            1 => Reply(200, serde_json::json!({"registered":true}), None),
            _ => Reply(503, pending(), None),
        },
        |client, _, trace| {
            let start = Instant::now();
            assert!(
                client
                    .models_until(start + Duration::from_millis(180))
                    .is_err()
            );
            assert!(start.elapsed() < Duration::from_millis(700));
            assert_eq!(
                trace
                    .lock()
                    .unwrap()
                    .iter()
                    .filter(|line| line.starts_with("POST "))
                    .count(),
                1
            );
        },
    );
}

#[test]
fn trickle_reads_cannot_extend_the_overall_deadline() {
    fixture(
        |index, _, profile| match index {
            0 => Reply(200, initial(), None),
            1 => Reply(200, serde_json::json!({"registered":true}), None),
            _ => Reply(
                200,
                facts(profile, "current"),
                Some(Duration::from_millis(15)),
            ),
        },
        |client, _, trace| {
            let start = Instant::now();
            assert!(
                client
                    .models_until(start + Duration::from_millis(180))
                    .is_err()
            );
            assert!(start.elapsed() < Duration::from_millis(700));
            assert_eq!(trace.lock().unwrap().len(), 3);
        },
    );
}

#[test]
fn reader_preparation_finishes_before_exclusive_store_transaction() {
    let registered = Arc::new(AtomicBool::new(false));
    let ready = Arc::new(AtomicBool::new(false));
    fixture(
        move |_, request, profile| {
            if request.starts_with("POST ") {
                assert_eq!(request, "POST /v1/store-enrollment HTTP/1.1");
                registered.store(true, Ordering::Release);
                return Reply(200, serde_json::json!({"registered":true}), None);
            }
            if !registered.load(Ordering::Acquire) {
                return Reply(200, initial(), None);
            }
            if !ready.load(Ordering::Acquire) {
                // Same real shared/exclusive filesystem lock relationship used by
                // CMS inventory and ModelManager::uninstall, in this private profile.
                let file = std::fs::OpenOptions::new()
                    .read(true)
                    .write(true)
                    .open(profile.join("version-operations.lock"))
                    .unwrap();
                if file.try_lock_shared().is_err() {
                    return Reply(503, pending(), None);
                }
                ready.store(true, Ordering::Release);
            }
            Reply(200, facts(profile, "current"), None)
        },
        |client, profile, trace| {
            let lock = std::fs::OpenOptions::new()
                .read(true)
                .write(true)
                .create_new(true)
                .open(profile.join("version-operations.lock"))
                .unwrap();
            super::super::delegate::prepare_reader("current").unwrap();
            lock.try_lock().unwrap();
            assert!(client.model_fact("current").unwrap().is_some());
            assert_eq!(
                trace
                    .lock()
                    .unwrap()
                    .iter()
                    .filter(|line| line.starts_with("POST "))
                    .count(),
                1
            );
        },
    );
}

#[test]
fn first_enable_after_enrollment_sends_the_lifecycle_post_once() {
    fixture(
        |index, request, profile| {
            if index == 0 {
                return Reply(200, initial(), None);
            }
            if request.starts_with("POST /v1/store-enrollment ") {
                return Reply(200, serde_json::json!({"registered":true}), None);
            }
            if index == 2 {
                return Reply(503, pending(), None);
            }
            let value = match request {
                "GET /v1/models HTTP/1.1" => facts(profile, "current"),
                "POST /v1/models/current/enable HTTP/1.1" => {
                    serde_json::json!({"task_id":"first-task"})
                }
                "GET /v1/tasks/first-task HTTP/1.1" => {
                    serde_json::json!({"status":"succeeded","events":[]})
                }
                "GET /v1/status HTTP/1.1" => {
                    serde_json::json!({"service":"cmsd","gateway":"127.0.0.1:18081"})
                }
                _ => panic!("unexpected request: {request}"),
            };
            Reply(200, value, None)
        },
        |_, _, trace| {
            let result = super::super::delegate::enable("current", &mut |_| {}, &|| false).unwrap();
            let super::super::delegate::DelegateOutcome::Enabled { endpoint_model, .. } = result
            else {
                panic!("enable did not complete")
            };
            assert_eq!(endpoint_model, "Model");
            let trace = trace.lock().unwrap();
            assert_eq!(
                trace
                    .iter()
                    .filter(|line| line.starts_with("POST /v1/store-enrollment "))
                    .count(),
                1
            );
            assert_eq!(
                trace
                    .iter()
                    .filter(|line| line.starts_with("POST /v1/models/"))
                    .count(),
                1
            );
        },
    );
}

#[test]
fn a_contradictory_registration_ack_remains_unknown() {
    fixture(
        |index, _, _| {
            if index == 0 {
                Reply(200, initial(), None)
            } else {
                Reply(
                    500,
                    serde_json::json!({"registered":true,"error":{"code":"persist_failed"}}),
                    None,
                )
            }
        },
        |client, _, trace| {
            assert!(client.models().unwrap_err().contains("not confirmed"));
            assert_eq!(trace.lock().unwrap().len(), 2);
        },
    );
}
