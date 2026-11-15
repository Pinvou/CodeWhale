//! Real MockRuntime IPC/service/persistence/fanout with the engine's dependency
//! predicate. The injected marker does not instantiate a Wry EnginePool.

use crate::{
    app::commands,
    features::{
        assistant::engine_pool::model_update_test_support::RevisionProbe,
        model_store::{
            ModelStoreService,
            ipc_fixture::{CARD_ID, Fixture, MODEL_ID, OTHER_MODEL_ID},
        },
    },
    platform::{app_events::AppEventBus, prefs::UserPrefs},
};
use serde_json::{Value, json};
use std::{
    io::{Read, Write},
    net::TcpListener,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    thread,
    time::{Duration, Instant},
};
use tauri::{Listener, Manager};

struct ModelsServer {
    base_url: String,
    requests: Arc<AtomicUsize>,
    stop: Arc<AtomicBool>,
    worker: Option<thread::JoinHandle<()>>,
}

impl ModelsServer {
    fn new() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let base_url = format!("http://{}/v1", listener.local_addr().unwrap());
        let requests = Arc::new(AtomicUsize::new(0));
        let stop = Arc::new(AtomicBool::new(false));
        let (count, done) = (requests.clone(), stop.clone());
        let worker = thread::spawn(move || {
            while !done.load(Ordering::Relaxed) {
                let Ok((mut stream, _)) = listener.accept() else {
                    thread::sleep(Duration::from_millis(2));
                    continue;
                };
                stream
                    .set_read_timeout(Some(Duration::from_secs(1)))
                    .unwrap();
                let mut request = [0; 8192];
                let size = stream.read(&mut request).unwrap_or(0);
                assert!(request[..size].starts_with(b"GET /v1/models "));
                count.fetch_add(1, Ordering::Relaxed);
                let body = r#"{"data":[{"id":"wire","max_model_len":262144}]}"#;
                write!(
                    stream,
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                )
                .unwrap();
            }
        });
        Self {
            base_url,
            requests,
            stop,
            worker: Some(worker),
        }
    }
}

impl Drop for ModelsServer {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        self.worker.take().unwrap().join().unwrap();
    }
}

fn invoke(
    window: &tauri::WebviewWindow<tauri::test::MockRuntime>,
    cmd: &str,
    args: Value,
) -> Value {
    tauri::test::get_ipc_response(
        window,
        tauri::webview::InvokeRequest {
            cmd: cmd.into(),
            callback: tauri::ipc::CallbackFn(0),
            error: tauri::ipc::CallbackFn(1),
            url: window.url().unwrap(),
            body: tauri::ipc::InvokeBody::Json(args),
            headers: Default::default(),
            invoke_key: tauri::test::INVOKE_KEY.to_owned(),
        },
    )
    .unwrap_or_else(|error| panic!("{cmd} failed: {error}"))
    .deserialize()
    .unwrap()
}

fn enable(window: &tauri::WebviewWindow<tauri::test::MockRuntime>) {
    let started = invoke(
        window,
        "model_store_start_lifecycle",
        json!({"cardId": CARD_ID, "action": "enable", "purgeWeights": false}),
    );
    let deadline = Instant::now() + Duration::from_secs(10);
    let mut from = 0;
    loop {
        assert!(Instant::now() < deadline, "enable did not finish");
        let log = invoke(
            window,
            "model_store_run_log",
            json!({"runId": started["run_id"], "from": from, "wait": 1}),
        );
        if log["done"] == true {
            assert_eq!(log["ok"], true, "enable failed: {log}");
            return;
        }
        from = log["next"].as_u64().unwrap();
    }
}

#[test]
fn same_route_enable_crosses_real_ipc_and_fanout_before_next_turn_rebuild() {
    let server = ModelsServer::new();
    let _fixture = Fixture::new(&server.base_url);
    let initial = UserPrefs::load();
    let linked = initial.model_by_id(MODEL_ID).unwrap().clone();
    let mut expected = linked.clone();
    expected.context_window_tokens = Some(262144);
    let unrelated = initial.model_by_id(OTHER_MODEL_ID).unwrap().clone();
    let probe = Arc::new(RevisionProbe::new());
    let before_linked = Arc::new(Mutex::new(probe.snapshot(linked)));
    let before_other = probe.snapshot(unrelated.clone());
    let desktop = Arc::new(Mutex::new(Vec::new()));
    let forwarded = Arc::new(Mutex::new(Vec::new()));
    let observe = {
        let (probe, before_linked) = (probe.clone(), before_linked.clone());
        Arc::new(move |payload: Value| {
            let saved = UserPrefs::load();
            let linked = saved.model_by_id(MODEL_ID).unwrap().clone();
            let stale = probe
                .snapshot(linked.clone())
                .requires_rebuild_from(&before_linked.lock().unwrap());
            (payload, linked.context_window_tokens, stale)
        })
    };
    let service = Arc::new(ModelStoreService::new());
    let app = tauri::test::mock_builder()
        .invoke_handler(tauri::generate_handler![
            commands::model_store::model_store_start_lifecycle,
            commands::model_store::model_store_run_log,
        ])
        .build(tauri::test::mock_context(tauri::test::noop_assets()))
        .unwrap();
    app.manage(service.clone());
    let marker = probe.marker();
    service.set_models_changed_handler(super::super::models_changed_handler(
        app.handle().clone(),
        move |ids| marker(ids),
    ));
    let (events, observer) = (desktop.clone(), observe.clone());
    app.listen("models:changed", move |event| {
        events
            .lock()
            .unwrap()
            .push(observer(serde_json::from_str(event.payload()).unwrap()));
    });
    let events = forwarded.clone();
    app.manage(AppEventBus::new(
        move |event, payload| {
            assert_eq!(event, "models:changed");
            events.lock().unwrap().push(observe(payload));
        },
        || true,
    ));
    let window = tauri::WebviewWindowBuilder::new(&app, "main", Default::default())
        .build()
        .unwrap();

    enable(&window);
    let first = UserPrefs::load();
    let ready = first.model_by_id(MODEL_ID).unwrap().clone();
    assert_eq!(ready, expected);
    assert_eq!(server.requests.load(Ordering::Relaxed), 1);
    assert_eq!(
        first.advanced.model_store_context_repairs.get(MODEL_ID),
        Some(&format!("verified-256k-v1/{CARD_ID}"))
    );
    let first_cached = probe.snapshot(ready.clone());
    *before_linked.lock().unwrap() = probe.snapshot(ready.clone());
    for events in [&desktop, &forwarded] {
        let mut observed = events.lock().unwrap();
        assert!(!observed.is_empty());
        assert!(observed.iter().all(|(payload, window, stale)| {
            payload == &json!({"model_ids": [MODEL_ID]}) && *window == Some(262144) && *stale
        }));
        observed.clear();
    }

    // No route, context or other saved-model field changes on the second enable.
    // Only the notification revision can make this exact cached model stale.
    enable(&window);
    let second = UserPrefs::load();
    assert_eq!(second.advanced.saved_models, first.advanced.saved_models);
    assert_eq!(
        serde_json::to_value(&second).unwrap(),
        serde_json::to_value(&first).unwrap()
    );
    assert_eq!(server.requests.load(Ordering::Relaxed), 1);
    let after_cached = probe.snapshot(second.model_by_id(MODEL_ID).unwrap().clone());
    assert!(after_cached.requires_rebuild_from(&first_cached));
    assert!(
        after_cached.requires_rebuild_from(&before_linked.lock().unwrap()),
        "previous prepared state must retain its original revision"
    );
    assert!(
        !probe
            .snapshot(unrelated.clone())
            .requires_rebuild_from(&before_other)
    );
    assert_eq!(second.model_by_id(OTHER_MODEL_ID), Some(&unrelated));
    assert_eq!(
        second.advanced.active_model_id.as_deref(),
        Some(OTHER_MODEL_ID)
    );
    for events in [&desktop, &forwarded] {
        let observed = events.lock().unwrap();
        assert_eq!(observed.len(), 1);
        assert_eq!(
            observed[0],
            (json!({"model_ids": [MODEL_ID]}), Some(262144), true)
        );
    }
}
