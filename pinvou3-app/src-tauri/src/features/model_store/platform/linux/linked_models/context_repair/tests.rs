use super::*;
use crate::features::model_store::platform::linux::{config, script_runner, service, store_client};
use std::{
    io::{Read, Write},
    net::TcpListener,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    thread,
};

const CARD: &str = "qwen38-27b-nvfp4-tachyon";

struct Server {
    base: String,
    response: Arc<Mutex<(u16, String)>>,
    requests: Arc<AtomicUsize>,
    before_reply: Arc<Mutex<Option<ResponseGate>>>,
    stop: Arc<AtomicBool>,
    worker: Option<thread::JoinHandle<()>>,
}
struct ResponseGate {
    entered: std::sync::mpsc::SyncSender<()>,
    release: std::sync::mpsc::Receiver<()>,
}
impl Server {
    fn new() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let base = format!("http://{}/v1", listener.local_addr().unwrap());
        let response = Arc::new(Mutex::new((200, String::new())));
        let requests = Arc::new(AtomicUsize::new(0));
        let before_reply = Arc::new(Mutex::new(Option::<ResponseGate>::None));
        let stop = Arc::new(AtomicBool::new(false));
        let (reply, count, done) = (response.clone(), requests.clone(), stop.clone());
        let gate = before_reply.clone();
        let worker = thread::spawn(move || {
            while !done.load(Ordering::Relaxed) {
                let Ok((mut stream, _)) = listener.accept() else {
                    thread::sleep(Duration::from_millis(2));
                    continue;
                };
                stream
                    .set_read_timeout(Some(Duration::from_secs(1)))
                    .unwrap();
                let mut buf = [0; 8192];
                let n = stream.read(&mut buf).unwrap_or(0);
                assert!(String::from_utf8_lossy(&buf[..n]).starts_with("GET /v1/models "));
                count.fetch_add(1, Ordering::Relaxed);
                if let Some(gate) = gate.lock().unwrap().take() {
                    let _ = gate.entered.send(());
                    if gate.release.recv_timeout(Duration::from_secs(3)).is_err() {
                        continue;
                    }
                }
                let (status, body) = reply.lock().unwrap().clone();
                let _ = write!(
                    stream,
                    "HTTP/1.1 {status} Test\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                );
            }
        });
        let server = Self {
            base,
            response,
            requests,
            before_reply,
            stop,
            worker: Some(worker),
        };
        server.window(262144);
        server
    }
    fn window(&self, window: u32) {
        self.reply(
            200,
            serde_json::json!({"data":[{"id":"wire", "max_model_len":window}]}),
        );
    }
    fn reply(&self, status: u16, body: serde_json::Value) {
        *self.response.lock().unwrap() = (status, body.to_string());
    }
    fn count(&self) -> usize {
        self.requests.load(Ordering::Relaxed)
    }
    fn hold_next_response(
        &self,
    ) -> (
        std::sync::mpsc::Receiver<()>,
        std::sync::mpsc::SyncSender<()>,
    ) {
        let (entered, incoming) = std::sync::mpsc::sync_channel(1);
        let (release, resume) = std::sync::mpsc::sync_channel(1);
        *self.before_reply.lock().unwrap() = Some(ResponseGate {
            entered,
            release: resume,
        });
        (incoming, release)
    }
}
impl Drop for Server {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        self.worker.take().unwrap().join().unwrap();
    }
}

fn setup(card: &str, base: &str) -> SavedModel {
    db::init_db().unwrap();
    db::add_store_model(db::AddStoreModelParams {
        card_id: card,
        model_name: "wire",
        card_name: "Verified local model",
        org: None,
        description: None,
        engine_name: "native",
        engine_version: None,
        size_bytes: 1,
        quantization: None,
        source_repo_id: None,
        source_filename: None,
        scripts: &Default::default(),
        status: "deployed",
        profiles: &[],
        deploy_topology: store_client::DeployTopology::Single,
        card_source: "store",
    })
    .unwrap();
    db::set_endpoint_info(card, Some(base), Some("wire")).unwrap();
    db::set_running(card, true).unwrap();
    let row: SavedModel = serde_json::from_value(serde_json::json!({
        "id":"stable-id", "name":"My label", "alias":"My alias",
        "preset":"openai_compatible", "provider_kind":"custom", "model":"wire",
        "base_url":base, "model_store_card_id":card, "context_window_tokens":32768,
        "max_output_tokens":4096, "reasoning_effort":"high", "vision_model_id":"vision"
    }))
    .unwrap();
    UserPrefs::update_transaction(|prefs| {
        prefs.advanced.saved_models = vec![row.clone()];
        prefs.advanced.active_model_id = Some(row.id.clone());
        Ok(())
    })
    .unwrap();
    row
}
fn window() -> Option<u32> {
    UserPrefs::load()
        .model_by_id("stable-id")
        .unwrap()
        .context_window_tokens
}
fn receipts() -> usize {
    UserPrefs::load().advanced.model_store_context_repairs.len()
}

#[test]
fn verified_legacy_32k_is_corrected_once_and_survives_reload_and_later_user_edits() {
    let _home = config::TempHome::new();
    let server = Server::new();
    let mut expected = setup(CARD, &server.base);
    assert_eq!(super::super::sync(None).unwrap(), ["stable-id"]);
    expected.context_window_tokens = Some(262144);
    assert_eq!(UserPrefs::load().model_by_id("stable-id"), Some(&expected));
    assert_eq!(
        UserPrefs::load().advanced.active_model_id.as_deref(),
        Some("stable-id")
    );
    assert_eq!(receipts(), 1);
    assert_eq!(server.count(), 1);
    UserPrefs::update_transaction(|prefs| {
        prefs.advanced.saved_models[0].context_window_tokens = Some(32768);
        Ok(())
    })
    .unwrap();
    assert!(super::super::sync(None).unwrap().is_empty());
    assert_eq!(window(), Some(32768));
    assert_eq!(server.count(), 1); // persisted receipt, no repeat probe after restart
    UserPrefs::update_transaction(|prefs| {
        prefs.remove_model("stable-id");
        Ok(())
    })
    .unwrap();
    assert_eq!(receipts(), 0);
}

#[test]
fn every_verified_card_repairs_legacy_32k_and_persists_its_own_receipt() {
    for (card, profile) in [
        ("qwen38-27b-sglang0519", None),
        ("qwen38-27b-nvfp4-tachyon", None),
        ("ornith15-35b-a3b-nvfp4", None),
        ("ornith-15-9b-nvfp4", None),
        ("qwen38-27b-nvfp4-mtp", None),
        ("qwen38-27b-nvfp4-mtp", Some("production-sampling")),
    ] {
        // Keep the HOME/config lock for the full SQLite, settings and HTTP lifecycle.
        let _home = config::TempHome::new();
        let server = Server::new();
        let mut expected = setup(card, &server.base);
        db::connect()
            .unwrap()
            .execute(
                "UPDATE store_models SET active_profile_id=?1 WHERE card_id=?2",
                rusqlite::params![profile, card],
            )
            .unwrap();
        assert_eq!(window(), Some(32768), "{card} / {profile:?}");
        assert_eq!(receipts(), 0, "{card} / {profile:?}");
        assert_eq!(server.count(), 0, "{card} / {profile:?}");

        // Exercise real route discovery, /models probing and the settings transaction.
        assert_eq!(
            super::super::sync(None).unwrap(),
            ["stable-id"],
            "{card} / {profile:?}"
        );
        expected.context_window_tokens = Some(262144);
        let persisted = UserPrefs::load();
        assert_eq!(
            persisted.model_by_id("stable-id"),
            Some(&expected),
            "{card} / {profile:?}"
        );
        assert_eq!(
            persisted.advanced.active_model_id.as_deref(),
            Some("stable-id"),
            "{card} / {profile:?}"
        );
        let receipt = format!("verified-256k-v1/{card}");
        assert_eq!(
            persisted
                .advanced
                .model_store_context_repairs
                .get("stable-id"),
            Some(&receipt),
            "{card} / {profile:?}"
        );
        assert_eq!(receipts(), 1, "{card} / {profile:?}");
        assert_eq!(server.count(), 1, "{card} / {profile:?}");

        // A later user edit survives a reload: each card's persisted receipt is honored.
        UserPrefs::update_transaction(|prefs| {
            prefs.advanced.saved_models[0].context_window_tokens = Some(32768);
            Ok(())
        })
        .unwrap();
        assert!(
            super::super::sync(None).unwrap().is_empty(),
            "{card} / {profile:?}"
        );
        assert_eq!(window(), Some(32768), "{card} / {profile:?}");
        assert_eq!(receipts(), 1, "{card} / {profile:?}");
        assert_eq!(server.count(), 1, "{card} / {profile:?}");
    }
}

#[test]
fn explicit_32k_during_a_successful_probe_survives_with_or_without_a_legacy_card_link() {
    for already_linked in [false, true] {
        let _home = config::TempHome::new();
        let server = Server::new();
        setup(CARD, &server.base);
        if !already_linked {
            UserPrefs::update_transaction(|prefs| {
                prefs.advanced.saved_models[0].model_store_card_id = None;
                Ok(())
            })
            .unwrap();
        }
        let mut preview = UserPrefs::load();
        let routes = super::super::routes().unwrap();
        super::super::reconcile(&mut preview.advanced.saved_models, &routes, None);
        let (entered, release) = server.hold_next_response();
        let probing = thread::spawn(move || probe(&preview, &routes, None));
        entered.recv_timeout(Duration::from_secs(3)).unwrap();
        // The command/CLI tests verify the production writers of this receipt.
        // Here the same real settings transaction commits while HTTP is held.
        UserPrefs::update_transaction(|prefs| {
            prefs.advanced.saved_models[0].context_window_tokens = Some(32768);
            prefs.record_explicit_model_context_selection("stable-id");
            Ok(())
        })
        .unwrap();
        release.send(()).unwrap();
        let proofs = probing.join().unwrap();
        assert_eq!(
            proofs.len(),
            1,
            "the HTTP probe must succeed with a valid 256K proof, never pass via timeout"
        );
        UserPrefs::update_transaction(|prefs| {
            let routes = super::super::routes()?;
            super::super::reconcile(&mut prefs.advanced.saved_models, &routes, None);
            apply(prefs, &routes, &proofs, None);
            Ok(())
        })
        .unwrap();
        let saved = UserPrefs::load();
        assert_eq!(window(), Some(32768), "the freshly confirmed 32K must win");
        assert!(saved.has_explicit_model_context_selection("stable-id"));
        assert_eq!(
            saved
                .model_by_id("stable-id")
                .unwrap()
                .model_store_card_id
                .as_deref(),
            Some(CARD),
            "route association must still complete independently"
        );
        assert!(super::super::sync(None).unwrap().is_empty());
        assert_eq!(
            server.count(),
            1,
            "a saved user choice must suppress later probes"
        );
    }
}

#[test]
fn old_failed_wrong_or_ambiguous_model_probes_retry_without_marking_repair_complete() {
    let _home = config::TempHome::new();
    let server = Server::new();
    setup(CARD, &server.base);
    for (status, data) in [
        (
            200,
            serde_json::json!({"data":[{"id":"wire", "max_model_len":32768}]}),
        ),
        (401, serde_json::json!({"error":"unauthorized"})),
        (
            200,
            serde_json::json!({"data":[{"id":"another", "max_model_len":262144}]}),
        ),
        (200, serde_json::json!({"data":[{"id":"wire"}]})),
        (
            200,
            serde_json::json!({"data":[{"id":"wire", "max_model_len":262144},{"id":"wire", "max_model_len":262144}]}),
        ),
    ] {
        server.reply(status, data);
        assert!(super::super::sync(None).unwrap().is_empty());
        assert_eq!(window(), Some(32768));
        assert_eq!(receipts(), 0);
    }
    server.window(262144);
    assert_eq!(super::super::sync(None).unwrap(), ["stable-id"]);
    assert_eq!(window(), Some(262144));
    assert_eq!(receipts(), 1);
}

#[test]
fn stopped_pending_outdated_script_and_benchmark_deployments_wait_for_actual_update() {
    let _home = config::TempHome::new();
    let server = Server::new();
    let card = "qwen38-27b-nvfp4-mtp";
    setup(card, &server.base);
    let conn = db::connect().unwrap();
    for update in [
        "running=0",
        "running=1,card_update_pending=1",
        "card_update_pending=0,active_profile_id='benchmark'",
        "active_profile_id='production-sampling',deployed_script_sha256='old',scripts_content_json='{\"deploy\":{\"content\":\"new\",\"sha256\":\"new\"}}'",
    ] {
        conn.execute(
            &format!("UPDATE store_models SET {update} WHERE card_id=?1"),
            [card],
        )
        .unwrap();
        assert!(super::super::sync(None).unwrap().is_empty());
        assert_eq!(window(), Some(32768));
        assert_eq!(receipts(), 0);
        assert_eq!(server.count(), 0);
    }
    conn.execute(
        "UPDATE store_models SET deployed_script_sha256='new' WHERE card_id=?1",
        [card],
    )
    .unwrap();
    assert_eq!(super::super::sync(None).unwrap(), ["stable-id"]);
    assert_eq!(window(), Some(262144));
}

#[test]
fn unassociated_detached_unknown_and_other_limits_are_not_promoted() {
    let _home = config::TempHome::new();
    let server = Server::new();
    setup(CARD, &server.base);
    UserPrefs::update_transaction(|prefs| {
        prefs.advanced.saved_models[0].model_store_link_detached = true;
        Ok(())
    })
    .unwrap();
    assert!(super::super::sync(None).unwrap().is_empty());
    assert_eq!(receipts(), 0);
    UserPrefs::update_transaction(|prefs| {
        let row = &mut prefs.advanced.saved_models[0];
        row.model_store_link_detached = false;
        row.context_window_tokens = Some(65536);
        Ok(())
    })
    .unwrap();
    assert_eq!(super::super::sync(None).unwrap(), ["stable-id"]);
    assert_eq!(window(), Some(65536));
    assert_eq!(server.count(), 0);
    let mut rec = db::get_store_model(CARD).unwrap().unwrap();
    rec.card_id = "unverified-card".into();
    assert!(deployment_stamp(&rec).is_none());
    rec.card_id = CARD.into();
    rec.active_profile_id = Some("custom-profile".into());
    assert!(deployment_stamp(&rec).is_none());
}

#[test]
fn completion_endpoint_uses_the_canonical_models_probe_path() {
    let _home = config::TempHome::new();
    let server = Server::new();
    setup(CARD, &server.base);
    let mut prefs = UserPrefs::load();
    prefs.advanced.saved_models[0]
        .base_url
        .push_str("/chat/completions/");
    let routes = super::super::routes().unwrap();
    // Server rejects any path other than /v1/models. A saved completions URL
    // is accepted by the shared route grammar and must use the same base here.
    let proofs = probe(&prefs, &routes, None);
    assert_eq!(proofs.len(), 1);
    assert_eq!(server.count(), 1);
    assert_eq!(apply(&mut prefs, &routes, &proofs, None), ["stable-id"]);
    assert_eq!(
        prefs.advanced.saved_models[0].context_window_tokens,
        Some(262144)
    );
}

#[test]
fn missing_deployment_stamp_never_probes_or_records_a_repair() {
    let _home = config::TempHome::new();
    let server = Server::new();
    setup(CARD, &server.base);
    let mut prefs = UserPrefs::load();
    let mut routes = super::super::routes().unwrap();
    routes[0].context_repair_stamp = None;
    let proofs = probe(&prefs, &routes, None);
    assert!(proofs.is_empty());
    assert_eq!(server.count(), 0);
    assert!(apply(&mut prefs, &routes, &proofs, None).is_empty());
    assert_eq!(
        prefs.advanced.saved_models[0].context_window_tokens,
        Some(32768)
    );
    assert!(prefs.advanced.model_store_context_repairs.is_empty());
}

#[test]
fn probe_cannot_be_applied_to_a_different_deployment_at_commit() {
    let _home = config::TempHome::new();
    let server = Server::new();
    setup(CARD, &server.base);
    let mut prefs = UserPrefs::load();
    let routes = super::super::routes().unwrap();
    let proofs = probe(&prefs, &routes, None);
    assert_eq!(proofs.len(), 1);
    db::set_container_name(CARD, Some("new-deployment")).unwrap();
    assert!(apply(&mut prefs, &super::super::routes().unwrap(), &proofs, None).is_empty());
    assert_eq!(
        prefs.advanced.saved_models[0].context_window_tokens,
        Some(32768)
    );
    assert!(prefs.advanced.model_store_context_repairs.is_empty());
}

#[test]
fn successful_enable_repairs_same_route_and_notifies_only_after_persistence() {
    let _home = config::TempHome::new();
    let server = Server::new();
    setup(CARD, &server.base);
    let service = Arc::new(service::ModelStoreService::new());
    let observed = Arc::new(Mutex::new(Vec::new()));
    let observation = observed.clone();
    service.set_models_changed_handler(Arc::new(move |ids| {
        observation.lock().unwrap().push((ids.to_vec(), window()));
    }));
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let run = |script: &str| {
        let scripts = std::collections::BTreeMap::from([(
            "enable".into(),
            db::ScriptBlob {
                content: script.into(),
                sha256: script_runner::sha256_hex(script),
            },
        )]);
        db::set_scripts_content(CARD, &scripts, &Default::default()).unwrap();
        let started =
            service::start_lifecycle(&service, CARD.into(), "enable".into(), false).unwrap();
        let run_id = started["run_id"].as_str().unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        let mut from = 0;
        loop {
            let result = runtime
                .block_on(service::run_log(
                    Arc::clone(&service),
                    run_id.into(),
                    from,
                    Some(1),
                ))
                .unwrap();
            if result["done"] == true {
                break result["ok"] == true;
            }
            from = result["next"].as_u64().unwrap() as usize;
            assert!(Instant::now() < deadline, "enable did not finish");
        }
    };
    assert!(!run("#!/bin/sh\nexit 1\n"));
    assert_eq!(window(), Some(32768));
    assert_eq!(receipts(), 0);
    assert_eq!(server.count(), 0);
    assert!(run(&format!(
        "#!/bin/sh\necho LMM_ENDPOINT_URL={}/chat/completions\necho LMM_ENDPOINT_MODEL=wire\n",
        server.base
    )));
    assert_eq!(window(), Some(262144));
    assert_eq!(receipts(), 1);
    assert!(
        observed
            .lock()
            .unwrap()
            .iter()
            .any(|(ids, tokens)| ids == &["stable-id"] && *tokens == Some(262144))
    );
}
