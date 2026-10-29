//! Display-requiring Wry IPC → managed EnginePool → actual next provider turn.
//! The local server replaces only HTTP inference, never the pool or its mailbox.

use crate::{
    app::commands,
    features::{
        assistant::{engine::EngineTurnSignal, engine_pool::EnginePool},
        model_store::{
            ModelStoreService,
            ipc_fixture::{CARD_ID, Fixture, MODEL_ID, OTHER_MODEL_ID},
        },
        sessions::SessionStore,
    },
    platform::{app_events::AppEventBus, prefs::UserPrefs, test_support::EnvRestore},
};
use deepseek_tui::{
    AppMode,
    core::events::TurnOutcomeStatus,
    models::{ContentBlock, Message},
};
use serde_json::{Value, json};
use std::{
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    process::{Command, Stdio},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    thread,
    time::{Duration, Instant},
};
use tauri::{Listener, Manager};

const CHILD_ENV: &str = "PINVOU_MODEL_STORE_LIVE_POOL_CHILD";
const HISTORY_USER: &str = "history-before-model-store-enable";
const HISTORY_REPLY: &str = "reply-before-model-store-enable";
const NEXT_PROMPT: &str = "next-turn-after-identical-enable";
const NEXT_REPLY: &str = "loopback-reply-after-engine-rebuild";

struct ProviderServer {
    base_url: String,
    model_requests: Arc<AtomicUsize>,
    turns: Arc<Mutex<Vec<Value>>>,
    stop: Arc<AtomicBool>,
    worker: Option<thread::JoinHandle<()>>,
}

impl ProviderServer {
    fn new() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let base_url = format!("http://{}/v1", listener.local_addr().unwrap());
        let model_requests = Arc::new(AtomicUsize::new(0));
        let turns = Arc::new(Mutex::new(Vec::new()));
        let stop = Arc::new(AtomicBool::new(false));
        let (count, observed, done) = (model_requests.clone(), turns.clone(), stop.clone());
        let worker = thread::spawn(move || {
            while !done.load(Ordering::Relaxed) {
                match listener.accept() {
                    Ok((mut stream, _)) => {
                        stream
                            .set_read_timeout(Some(Duration::from_secs(2)))
                            .unwrap();
                        stream
                            .set_write_timeout(Some(Duration::from_secs(2)))
                            .unwrap();
                        let (first, body) = read_request(&mut stream);
                        if first.starts_with("GET /v1/models ") {
                            count.fetch_add(1, Ordering::Relaxed);
                            reply(
                                &mut stream,
                                "200 OK",
                                "application/json",
                                r#"{"data":[{"id":"wire","max_model_len":262144}]}"#,
                            );
                        } else if first.starts_with("POST /v1/chat/completions ") {
                            observed
                                .lock()
                                .unwrap()
                                .push(serde_json::from_slice(&body).unwrap());
                            let chunk = |delta: Value, finish: Value| {
                                json!({
                                    "id":"local-only-fixture", "object":"chat.completion.chunk",
                                    "created":1, "model":"wire",
                                    "choices":[{"index":0,"delta":delta,"finish_reason":finish}]
                                })
                            };
                            let body = format!(
                                "data: {}\n\ndata: {}\n\ndata: [DONE]\n\n",
                                chunk(
                                    json!({"role":"assistant","content":NEXT_REPLY}),
                                    Value::Null
                                ),
                                chunk(json!({}), json!("stop"))
                            );
                            reply(&mut stream, "200 OK", "text/event-stream", &body);
                        } else {
                            // The real pool probes local server kinds before spawning.
                            reply(&mut stream, "404 Not Found", "application/json", "{}");
                        }
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(2));
                    }
                    Err(error) => panic!("fixture accept failed: {error}"),
                }
            }
        });
        Self {
            base_url,
            model_requests,
            turns,
            stop,
            worker: Some(worker),
        }
    }
}

impl Drop for ProviderServer {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        self.worker.take().unwrap().join().unwrap();
    }
}

fn read_request(stream: &mut TcpStream) -> (String, Vec<u8>) {
    let mut bytes = Vec::new();
    let header_end = loop {
        let mut part = [0; 8192];
        let size = stream.read(&mut part).unwrap();
        assert!(size > 0, "HTTP request ended before headers");
        bytes.extend_from_slice(&part[..size]);
        assert!(bytes.len() <= 1024 * 1024, "fixture request exceeded limit");
        if let Some(index) = bytes.windows(4).position(|v| v == b"\r\n\r\n") {
            break index + 4;
        }
    };
    let headers = std::str::from_utf8(&bytes[..header_end]).unwrap();
    let first = headers.lines().next().unwrap().to_string();
    let content_length = headers
        .lines()
        .find_map(|line| {
            let (name, value) = line.split_once(':')?;
            name.eq_ignore_ascii_case("content-length")
                .then(|| value.trim().parse::<usize>().unwrap())
        })
        .unwrap_or(0);
    assert!(content_length <= 1024 * 1024);
    while bytes.len() - header_end < content_length {
        let mut part = [0; 8192];
        let size = stream.read(&mut part).unwrap();
        assert!(size > 0, "HTTP request ended before its body");
        bytes.extend_from_slice(&part[..size]);
    }
    (
        first,
        bytes[header_end..header_end + content_length].to_vec(),
    )
}

fn reply(stream: &mut TcpStream, status: &str, content_type: &str, body: &str) {
    write!(stream,
        "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()).unwrap();
}

async fn invoke(window: &tauri::WebviewWindow, cmd: &str, args: Value) -> Value {
    // Tauri's test::get_ipc_response is MockRuntime-only. This is its actual
    // generic dispatcher entry, with the live app's key and a bounded response.
    let (sender, receiver) = tokio::sync::oneshot::channel();
    let webview: &tauri::Webview = window.as_ref();
    webview.clone().on_message(
        tauri::webview::InvokeRequest {
            cmd: cmd.into(),
            callback: tauri::ipc::CallbackFn(0),
            error: tauri::ipc::CallbackFn(1),
            url: window.url().unwrap(),
            body: tauri::ipc::InvokeBody::Json(args),
            headers: Default::default(),
            invoke_key: window.app_handle().invoke_key().to_owned(),
        },
        Box::new(move |_, _, response, _, _| {
            let _ = sender.send(response);
        }),
    );
    match tokio::time::timeout(Duration::from_secs(15), receiver)
        .await
        .unwrap()
        .unwrap()
    {
        tauri::ipc::InvokeResponse::Ok(body) => body.deserialize().unwrap(),
        tauri::ipc::InvokeResponse::Err(error) => panic!("{cmd} failed: {}", error.0),
    }
}

async fn enable(window: &tauri::WebviewWindow) {
    let started = invoke(
        window,
        "model_store_start_lifecycle",
        json!({"cardId":CARD_ID,"action":"enable","purgeWeights":false}),
    )
    .await;
    let deadline = Instant::now() + Duration::from_secs(15);
    let mut from = 0;
    loop {
        assert!(Instant::now() < deadline, "enable exceeded its deadline");
        let log = invoke(
            window,
            "model_store_run_log",
            json!({"runId":started["run_id"],"from":from,"wait":1}),
        )
        .await;
        if log["done"] == true {
            assert_eq!(log["ok"], true, "enable failed: {log}");
            return;
        }
        from = log["next"].as_u64().unwrap();
    }
}

fn message(role: &str, text: &str) -> Message {
    Message {
        role: role.into(),
        content: vec![ContentBlock::Text {
            text: text.into(),
            cache_control: None,
        }],
    }
}

async fn exercise(
    window: tauri::WebviewWindow,
    pool: EnginePool,
    store: SessionStore,
    server: Arc<ProviderServer>,
    desktop: Arc<Mutex<Vec<Value>>>,
    forwarded: Arc<Mutex<Vec<Value>>>,
) {
    let workspace = crate::platform::paths::pinvou3_home().join("isolated-workspace");
    std::fs::create_dir_all(&workspace).unwrap();
    let linked = store
        .create_new("wire".into(), Some(MODEL_ID.into()), workspace.clone())
        .unwrap();
    let other = store
        .create_new("wire".into(), Some(OTHER_MODEL_ID.into()), workspace)
        .unwrap();
    let sid = &linked.metadata.id;
    store
        .update_messages(
            sid,
            vec![
                message("user", HISTORY_USER),
                message("assistant", HISTORY_REPLY),
            ],
        )
        .unwrap();
    let initial = pool.get_or_spawn(sid).await.unwrap();
    assert!(
        initial
            .handle
            .tx_op
            .same_channel(&pool.get_or_spawn(sid).await.unwrap().handle.tx_op)
    );
    let unrelated = pool.get_or_spawn(&other.metadata.id).await.unwrap();
    let unrelated_settings =
        serde_json::to_value(UserPrefs::load().model_by_id(OTHER_MODEL_ID).unwrap()).unwrap();

    enable(&window).await;
    let first_prefs = serde_json::to_value(UserPrefs::load()).unwrap();
    assert_eq!(
        serde_json::to_value(UserPrefs::load().model_by_id(OTHER_MODEL_ID).unwrap()).unwrap(),
        unrelated_settings,
        "the independent route must not become a legacy store link",
    );
    let first = pool.get_or_spawn(sid).await.unwrap();
    assert!(!initial.handle.tx_op.same_channel(&first.handle.tx_op));
    assert_eq!(
        first
            .bridge
            .effective_model_owned()
            .unwrap()
            .context_window_tokens,
        Some(262144)
    );
    desktop.lock().unwrap().clear();
    forwarded.lock().unwrap().clear();

    // Config comparison cannot rescue a missing production revision mark here.
    let before_enable_gets = server.model_requests.load(Ordering::Relaxed);
    enable(&window).await;
    assert_eq!(
        serde_json::to_value(UserPrefs::load()).unwrap(),
        first_prefs
    );
    assert_eq!(
        server.model_requests.load(Ordering::Relaxed),
        before_enable_gets
    );
    let next = pool.get_or_spawn(sid).await.unwrap();
    assert!(
        !first.handle.tx_op.same_channel(&next.handle.tx_op),
        "an identical successful enable must rebuild the actual cached engine"
    );
    assert!(
        unrelated.handle.tx_op.same_channel(
            &pool
                .get_or_spawn(&other.metadata.id)
                .await
                .unwrap()
                .handle
                .tx_op
        ),
        "the unrelated session must retain its actual engine"
    );
    for events in [&desktop, &forwarded] {
        assert_eq!(
            *events.lock().unwrap(),
            vec![json!({"model_ids":[MODEL_ID]})]
        );
    }
    let mut turns = next.subscribe_turns();
    pool.send_user_message(sid, NEXT_PROMPT.into(), AppMode::Agent, true)
        .await
        .unwrap();
    tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            match turns.recv().await.unwrap() {
                EngineTurnSignal::Started { .. } => {}
                EngineTurnSignal::Terminal { status, error, .. } => {
                    assert_eq!(
                        status,
                        TurnOutcomeStatus::Completed,
                        "actual next turn failed: {error:?}"
                    );
                    assert_eq!(error, None);
                    break;
                }
                EngineTurnSignal::ForwarderStopped { error } => {
                    panic!("forwarder stopped: {error}")
                }
            }
        }
    })
    .await
    .expect("actual next turn exceeded its deadline");
    assert!(
        next.handle
            .tx_op
            .same_channel(&pool.handle_for(sid).await.unwrap().handle.tx_op)
    );
    let requests = server.turns.lock().unwrap().clone();
    assert_eq!(
        requests.len(),
        1,
        "one real provider request must complete the next turn"
    );
    assert_eq!(requests[0]["model"], "wire");
    let wire_history = requests[0]["messages"].to_string();
    for text in [HISTORY_USER, HISTORY_REPLY, NEXT_PROMPT] {
        assert!(
            wire_history.contains(text),
            "rebuilt engine omitted {text} from its real request"
        );
    }
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            let saved = serde_json::to_string(&store.load(sid).unwrap().messages).unwrap();
            if [HISTORY_USER, HISTORY_REPLY, NEXT_PROMPT, NEXT_REPLY]
                .iter()
                .all(|text| saved.contains(text))
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("real event forwarder did not persist history and next reply");
    pool.evict(sid).await;
    pool.evict(&other.metadata.id).await;
}

fn run_child() {
    assert!(
        std::env::var_os("DISPLAY").is_some(),
        "this explicit Wry test requires a Linux display"
    );
    let server = Arc::new(ProviderServer::new());
    // A second live endpoint avoids satisfying the intentional legacy-link
    // migration contract for an unlinked config with an identical route.
    let unrelated_server = ProviderServer::new();
    let _fixture = Fixture::new(&server.base_url);
    let _env = EnvRestore::capture(&[
        "CODEWHALE_HOME",
        "PINVOU3_TEST_KEYRING_FILE_FALLBACK",
        "DEEPSEEK_API_KEY",
        "DEEPSEEK_BASE_URL",
        "DEEPSEEK_MODEL",
        "DEEPSEEK_PROVIDER",
        "OPENAI_API_KEY",
        "ANTHROPIC_API_KEY",
    ]);
    // SAFETY: Fixture holds the existing environment lock for this fresh child.
    unsafe {
        std::env::set_var(
            "CODEWHALE_HOME",
            crate::platform::paths::pinvou3_home().join("foundation"),
        );
        std::env::set_var("PINVOU3_TEST_KEYRING_FILE_FALLBACK", "1");
        for name in [
            "DEEPSEEK_API_KEY",
            "DEEPSEEK_BASE_URL",
            "DEEPSEEK_MODEL",
            "DEEPSEEK_PROVIDER",
            "OPENAI_API_KEY",
            "ANTHROPIC_API_KEY",
        ] {
            std::env::remove_var(name);
        }
    }
    UserPrefs::update_transaction(|prefs| {
        prefs.notifications.enabled = false;
        prefs
            .advanced
            .saved_models
            .iter_mut()
            .find(|m| m.id == OTHER_MODEL_ID)
            .unwrap()
            .base_url = unrelated_server.base_url.clone();
        Ok(())
    })
    .unwrap();
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let _entered = runtime.enter();
    let app = tauri::Builder::default()
        .any_thread()
        .invoke_handler(tauri::generate_handler![
            commands::model_store::model_store_start_lifecycle,
            commands::model_store::model_store_run_log
        ])
        .build(tauri::test::mock_context(tauri::test::noop_assets()))
        .unwrap();
    let store = SessionStore::boot().unwrap();
    let pool = EnginePool::new_with_dependencies(
        app.handle().clone(),
        store.clone(),
        Arc::new(|_, _| vec![]),
        Arc::new(|_| vec![]),
    )
    .unwrap();
    app.manage(pool.clone());
    let service = Arc::new(ModelStoreService::new());
    service.set_models_changed_handler(super::super::live_models_changed_handler(
        app.handle().clone(),
    ));
    app.manage(service);
    let desktop = Arc::new(Mutex::new(Vec::new()));
    let forwarded = Arc::new(Mutex::new(Vec::new()));
    let observed = desktop.clone();
    app.listen("models:changed", move |event| {
        observed
            .lock()
            .unwrap()
            .push(serde_json::from_str(event.payload()).unwrap())
    });
    let observed = forwarded.clone();
    app.manage(AppEventBus::new(
        move |event, payload| {
            if event == "models:changed" {
                observed.lock().unwrap().push(payload);
            }
        },
        || true,
    ));
    let window =
        tauri::WebviewWindowBuilder::new(&app, "model-store-live-pool", Default::default())
            .data_directory(crate::platform::paths::pinvou3_home().join("wry-webview-data"))
            .visible(false)
            .build()
            .unwrap();
    let exit = app.handle().clone();
    let (sender, receiver) = std::sync::mpsc::sync_channel(1);
    let scenario = runtime.spawn(exercise(
        window,
        pool,
        store,
        server.clone(),
        desktop,
        forwarded,
    ));
    runtime.spawn(async move {
        let result = tokio::time::timeout(Duration::from_secs(70), scenario)
            .await
            .map_err(|e| e.to_string())
            .and_then(|r| r.map_err(|e| e.to_string()));
        let _ = sender.send(result);
        exit.exit(0);
    });
    assert_eq!(app.run_return(|_, _| {}), 0);
    receiver
        .recv_timeout(Duration::from_secs(5))
        .unwrap()
        .unwrap();
}

#[test]
#[ignore = "requires real Linux Wry/WebKit and DISPLAY; run explicitly with --ignored --exact"]
fn identical_enable_rebuilds_real_pool_and_next_turn_preserves_history() {
    if std::env::var(CHILD_ENV).as_deref() == Ok("1") {
        run_child();
        return;
    }
    // Process-private GTK/runtime and environment: neither globals nor real user
    // data are shared with the ordinary lib-test process.
    let module = module_path!().split_once("::").unwrap().1;
    let filter =
        format!("{module}::identical_enable_rebuilds_real_pool_and_next_turn_preserves_history");
    let output = tempfile::NamedTempFile::new().unwrap();
    let errors = tempfile::NamedTempFile::new().unwrap();
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args(["--ignored", "--exact", &filter, "--nocapture"])
        .env(CHILD_ENV, "1")
        // The real reqwest clients obey system proxies; this isolated loopback
        // fixture must never send provider traffic to an inherited proxy.
        .env("NO_PROXY", "*")
        .env("no_proxy", "*")
        .env_remove("HTTP_PROXY")
        .env_remove("http_proxy")
        .env_remove("HTTPS_PROXY")
        .env_remove("https_proxy")
        .env_remove("ALL_PROXY")
        .env_remove("all_proxy")
        .stdout(Stdio::from(output.reopen().unwrap()))
        .stderr(Stdio::from(errors.reopen().unwrap()))
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(90);
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            panic!(
                "real Wry child timed out: {}\n{}",
                std::fs::read_to_string(output.path()).unwrap(),
                std::fs::read_to_string(errors.path()).unwrap()
            );
        }
        thread::sleep(Duration::from_millis(10));
    };
    let stdout = std::fs::read_to_string(output.path()).unwrap();
    let stderr = std::fs::read_to_string(errors.path()).unwrap();
    assert!(
        status.success(),
        "real Wry child failed: {stdout}\n{stderr}"
    );
    assert!(
        stdout.contains("test result: ok. 1 passed;"),
        "zero-match child is not evidence: {stdout}\n{stderr}"
    );
}
