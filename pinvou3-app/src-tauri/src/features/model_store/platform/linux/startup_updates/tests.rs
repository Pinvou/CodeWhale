use super::super::{
    script_runner,
    store_client::{DeployTopology, StoreCardDetail, StoreCardSummary, StoreError},
};
use super::*;
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex, atomic::AtomicUsize},
};

fn install(id: &str, source: &str) -> StoreCardDetail {
    db::add_store_model(db::AddStoreModelParams {
        card_id: id,
        model_name: id,
        card_name: "Installed",
        org: None,
        description: None,
        engine_name: "native",
        engine_version: None,
        size_bytes: 11,
        quantization: None,
        source_repo_id: None,
        source_filename: None,
        scripts: &Default::default(),
        status: "deployed",
        profiles: &[],
        deploy_topology: DeployTopology::Single,
        card_source: source,
    })
    .unwrap();
    let content = "#!/bin/bash\nexit 99 # startup must never execute this\n";
    let script = db::ScriptBlob {
        content: content.into(),
        sha256: script_runner::sha256_hex(content),
    };
    let scripts: BTreeMap<_, _> = [
        "install_engine",
        "download_model",
        "deploy",
        "enable",
        "stop",
        "uninstall",
    ]
    .into_iter()
    .map(|k| (k.into(), script.clone()))
    .collect();
    let asset = db::ScriptBlob {
        content: "cached source only".into(),
        sha256: script_runner::sha256_hex("cached source only"),
    };
    let assets = BTreeMap::from([("controller.py".into(), asset)]);
    db::set_scripts_content(id, &scripts, &assets).unwrap();
    serde_json::from_value(serde_json::json!({
        "id":id,"name":"Installed","engine_name":"native","size_bytes":11,
        "scripts":scripts.keys().map(|k|(k.clone(),format!("https://example.test/{k}"))).collect::<BTreeMap<_,_>>(),
        "script_sha256":scripts.iter().map(|(k,v)|(k.clone(),v.sha256.clone())).collect::<BTreeMap<_,_>>(),
        "assets":{"controller.py":"https://example.test/controller.py"},
        "asset_sha256":assets.iter().map(|(k,v)|(k.clone(),v.sha256.clone())).collect::<BTreeMap<_,_>>()
    })).unwrap()
}

struct Cards {
    cards: BTreeMap<String, StoreCardDetail>,
    calls: Mutex<Vec<String>>,
    mutate: bool,
}
impl StoreSource for Cards {
    fn list_cards(&self) -> Result<Vec<StoreCardSummary>, StoreError> {
        panic!("startup checks installed cards only")
    }
    fn get_card(&self, id: &str) -> Result<StoreCardDetail, StoreError> {
        self.calls.lock().unwrap().push(id.into());
        if self.mutate {
            db::connect()
                .unwrap()
                .execute(
                    "UPDATE store_models SET card_name='Changed concurrently' WHERE card_id=?1",
                    [id],
                )
                .unwrap();
        }
        self.cards
            .get(id)
            .cloned()
            .ok_or_else(|| StoreError::new("unavailable"))
    }
}

#[test]
fn installed_metadata_only_detects_asset_changes_without_changing_deployments() {
    let _home = config::TempHome::new();
    db::init_db().unwrap();
    let a = install("a", "store");
    let mut b = install("b", "store");
    install("builtin", "builtin");
    let before = db::startup_card_manifest("b").unwrap().unwrap();
    let full = db::get_store_model("b").unwrap().unwrap();
    assert_eq!(before, card_update::notice_manifest(&full));
    assert_eq!(before, card_update::local_manifest(&full));
    b.asset_sha256
        .as_mut()
        .unwrap()
        .insert("controller.py".into(), "f".repeat(64));
    let store = Cards {
        cards: BTreeMap::from([("a".into(), a), ("b".into(), b)]),
        calls: Mutex::new(vec![]),
        mutate: false,
    };
    let report = check_cards(
        &store,
        &db::startup_card_ids().unwrap(),
        Duration::from_secs(2),
    );
    assert_eq!(report.checked, 2);
    assert_eq!(report.updates.len(), 1);
    assert_eq!(report.updates[0].card_id, "b");
    assert_eq!(*store.calls.lock().unwrap(), ["a", "b"]);
    assert_eq!(db::startup_card_manifest("b").unwrap().unwrap(), before);
    assert!(
        !serde_json::to_string(&report)
            .unwrap()
            .contains("startup must never execute")
    );
}

#[test]
fn failures_budget_and_concurrent_changes_do_not_create_false_reminders() {
    let _home = config::TempHome::new();
    db::init_db().unwrap();
    install("a", "store");
    let b = install("b", "store");
    let store = Cards {
        cards: BTreeMap::from([("b".into(), b)]),
        calls: Mutex::new(vec![]),
        mutate: false,
    };
    let ids = db::startup_card_ids().unwrap();
    let report = check_cards(&store, &ids, Duration::from_secs(2));
    assert_eq!(report.checked, 1);
    assert_eq!(report.skipped, 1);
    assert!(report.updates.is_empty());
    let report = check_cards(&store, &ids, Duration::ZERO);
    assert_eq!(report.skipped, 2);
    let mut store = store;
    store.mutate = true;
    let report = check_cards(&store, &["b".into()], Duration::from_secs(2));
    assert_eq!(report.skipped, 1);
    assert!(report.updates.is_empty());
}

#[tokio::test]
async fn unavailable_features_do_not_start_a_job_or_consume_the_startup_attempt() {
    let state = StartupUpdates::default();
    let report = state.once(false, || panic!("must not run")).await;
    assert!(report.updates.is_empty());
    assert_eq!(
        state
            .once(true, || Report {
                checked: 1,
                ..Default::default()
            })
            .await
            .checked,
        1
    );
    assert_eq!(
        state.once(true, || panic!("must not retry")).await.checked,
        1
    );
}

#[tokio::test]
async fn concurrent_and_cancelled_callers_share_one_background_pass() {
    let state = Arc::new(StartupUpdates::default());
    let count = Arc::new(AtomicUsize::new(0));
    let (started, ready) = tokio::sync::oneshot::channel();
    let (release, gate) = std::sync::mpsc::channel();
    let owner = Arc::clone(&state);
    let probe = Arc::clone(&count);
    let first = tokio::spawn(async move {
        owner
            .once(true, move || {
                probe.fetch_add(1, Ordering::SeqCst);
                let _ = started.send(());
                gate.recv().unwrap();
                Report {
                    checked: 3,
                    ..Default::default()
                }
            })
            .await
    });
    ready.await.unwrap();
    first.abort();
    release.send(()).unwrap();
    let result = state
        .once(true, || {
            panic!("caller cancellation must not restart detection")
        })
        .await;
    assert_eq!(result.checked, 3);
    assert_eq!(count.load(Ordering::SeqCst), 1);
}
