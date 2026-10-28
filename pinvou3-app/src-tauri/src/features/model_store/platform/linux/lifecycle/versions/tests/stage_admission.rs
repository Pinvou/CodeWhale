//! Exercise the public staging entry, before any generation may be registered.
use super::*;
use crate::features::model_store::platform::linux::store_client::{StoreError, StoreSource};
use std::io::Write;
use std::net::TcpListener;
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, AtomicUsize, Ordering},
};

struct CardSource {
    card: StoreCardDetail,
    calls: Arc<AtomicUsize>,
}

impl StoreSource for CardSource {
    fn list_cards(&self) -> Result<Vec<StoreCardSummary>, StoreError> {
        panic!("stage_version must fetch the selected card, not the catalogue")
    }

    fn get_card(&self, card_id: &str) -> Result<StoreCardDetail, StoreError> {
        assert_eq!(card_id, self.card.summary.id);
        self.calls.fetch_add(1, Ordering::SeqCst);
        Ok(self.card.clone())
    }
}

struct AssetServer {
    origin: String,
    routes: Arc<Mutex<BTreeMap<String, String>>>,
    requests: Arc<Mutex<Vec<String>>>,
    stop: Arc<AtomicBool>,
    worker: Option<std::thread::JoinHandle<()>>,
}

impl AssetServer {
    fn new() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let origin = format!("https://{}", listener.local_addr().unwrap());
        let routes = Arc::new(Mutex::new(BTreeMap::<String, String>::new()));
        let requests = Arc::new(Mutex::new(Vec::new()));
        let stop = Arc::new(AtomicBool::new(false));
        let (responses, observed, done) = (routes.clone(), requests.clone(), stop.clone());
        let worker = std::thread::spawn(move || {
            while !done.load(Ordering::SeqCst) {
                let (mut stream, _) = match listener.accept() {
                    Ok(connection) => connection,
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        std::thread::sleep(Duration::from_millis(2));
                        continue;
                    }
                    Err(error) => panic!("loopback fixture accept failed: {error}"),
                };
                stream
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                let mut request = Vec::new();
                loop {
                    let mut buf = [0; 1024];
                    let read = stream.read(&mut buf).unwrap();
                    assert_ne!(read, 0, "fixture request ended before headers");
                    request.extend_from_slice(&buf[..read]);
                    if request.windows(4).any(|part| part == b"\r\n\r\n") {
                        break;
                    }
                    assert!(request.len() < 16384);
                }
                let request = String::from_utf8(request).unwrap();
                let first = request.lines().next().unwrap();
                let path = first
                    .strip_prefix("GET ")
                    .unwrap()
                    .split(' ')
                    .next()
                    .unwrap();
                observed.lock().unwrap().push(path.into());
                let body = responses.lock().unwrap().get(path).cloned();
                let (status, body) =
                    body.map_or((404, "unknown fixture path".into()), |body| (200, body));
                write!(
                    stream,
                    "HTTP/1.1 {status} Fixture\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                ).unwrap();
            }
        });
        Self {
            origin,
            routes,
            requests,
            stop,
            worker: Some(worker),
        }
    }

    fn publish(&self, card: &mut StoreCardDetail, package: &CardPackage) {
        for (entry, blob) in &package.scripts {
            let path = format!("/scripts/{entry}");
            self.routes
                .lock()
                .unwrap()
                .insert(path.clone(), blob.content.clone());
            card.scripts
                .insert(entry.clone(), format!("{}{path}", self.origin));
        }
        for (entry, blob) in &package.assets {
            let path = format!("/assets/{entry}");
            self.routes
                .lock()
                .unwrap()
                .insert(path.clone(), blob.content.clone());
            card.assets
                .insert(entry.clone(), format!("{}{path}", self.origin));
        }
    }

    fn requests(&self) -> Vec<String> {
        self.requests.lock().unwrap().clone()
    }
}

impl Drop for AssetServer {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        self.worker.take().unwrap().join().unwrap();
    }
}

#[derive(Debug, PartialEq, Eq)]
struct Preserved {
    record: serde_json::Value,
    keys: serde_json::Value,
    versions: serde_json::Value,
    files: BTreeMap<PathBuf, Option<Vec<u8>>>,
}

fn snapshot(mgr: &ModelManager, current: &db::StoreModelRecord) -> Preserved {
    fn collect(path: &Path, files: &mut BTreeMap<PathBuf, Option<Vec<u8>>>) {
        if !path.exists() {
            return;
        }
        if path.is_dir() {
            files.insert(path.into(), None);
            for child in fs::read_dir(path).unwrap() {
                collect(&child.unwrap().path(), files);
            }
        } else {
            files.insert(path.into(), Some(fs::read(path).unwrap()));
        }
    }
    let mut files = BTreeMap::new();
    for path in [
        PathBuf::from(&mgr.settings.models_dir),
        PathBuf::from(&mgr.settings.staging_dir),
        config::data_dir().join("cards"),
    ] {
        collect(&path, &mut files);
    }
    Preserved {
        record: serde_json::to_value(db::get_store_model(&current.card_id).unwrap().unwrap())
            .unwrap(),
        keys: serde_json::to_value(db::list_api_keys_for_card(&current.card_id).unwrap()).unwrap(),
        versions: serde_json::to_value(version_store::list(None).unwrap()).unwrap(),
        files,
    }
}

#[derive(Clone, Copy, Debug)]
enum Rejection {
    Inventory,
    Revision,
    EvidenceDigest,
    EvidenceMissing,
    IncompatibleSelection,
    LegacyDirectory,
}

fn reject_before_writes(rejection: Rejection) {
    let (_home, mut mgr, current, mut card, mut package, _) = fixture();
    db::add_api_key(
        &current.card_id,
        Some("preserved stage-admission credential"),
    )
    .unwrap();
    for (entry, script) in &mut package.scripts {
        *script = blob(&format!(
            "#!/bin/bash\nprintf '%s\\n' '{entry}' >> \"$LMM_STAGING_DIR/stage-admission-script-ran\"\n{}",
            script.content.strip_prefix("#!/bin/bash\n").unwrap()
        ));
    }
    card.script_sha256 = Some(
        package
            .scripts
            .iter()
            .map(|(key, value)| (key.clone(), value.sha256.clone()))
            .collect(),
    );
    let server = AssetServer::new();
    server.publish(&mut card, &package);
    let expected = match rejection {
        Rejection::Inventory => {
            card.script_sha256.as_mut().unwrap().remove("enable");
            "卡片更新包的入口或校验清单不完整，已保留原版本。"
        }
        Rejection::Revision => "线上卡片已再次更新，请重新检查。",
        Rejection::EvidenceDigest => {
            server
                .routes
                .lock()
                .unwrap()
                .insert("/assets/weights.sha256".into(), "modified evidence".into());
            "脚本完整性校验失败"
        }
        Rejection::EvidenceMissing => {
            card.assets.remove(weight_identity::MANIFEST);
            card.asset_sha256
                .as_mut()
                .unwrap()
                .remove(weight_identity::MANIFEST);
            "此卡片不支持当前版本的隔离升级，现有部署已保留。"
        }
        Rejection::IncompatibleSelection => {
            card.source_filename = Some("not-in-manifest.tachyon".into());
            "此卡片不支持当前版本的隔离升级，现有部署已保留。"
        }
        Rejection::LegacyDirectory => {
            fs::write(
                runtime_path(&current).unwrap().join(".deploy-env"),
                format!("MODEL_DIR=\"{}/missing-legacy\"\n", mgr.settings.models_dir),
            )
            .unwrap();
            "无法确认旧版部署的权重目录，请先修复旧版部署，确保可回退后再升级。"
        }
    };
    let mut revision = card_update::revision(&card_update::remote_manifest(&card));
    if matches!(rejection, Rejection::Revision) {
        revision = "f".repeat(64);
        assert_ne!(
            revision,
            card_update::revision(&card_update::remote_manifest(&card))
        );
    }
    let calls = Arc::new(AtomicUsize::new(0));
    mgr.store = Box::new(CardSource {
        card,
        calls: calls.clone(),
    });
    let _transport = script_runner::test_loopback_fetch::install(&server.origin);
    let before = snapshot(&mgr, &current);
    let error = mgr
        .stage_version(&current.card_id, &revision, &mut |_| {}, Default::default())
        .unwrap_err();
    if matches!(rejection, Rejection::EvidenceDigest) {
        assert!(error.to_string().starts_with(expected), "{error}");
    } else {
        assert_eq!(error.to_string(), expected, "{rejection:?}");
    }
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let requests = server.requests();
    if matches!(
        rejection,
        Rejection::Inventory | Rejection::Revision | Rejection::EvidenceMissing
    ) {
        assert!(requests.is_empty(), "{requests:?}");
    } else {
        assert_eq!(requests, ["/assets/weights.sha256"]);
    }
    assert_eq!(snapshot(&mgr, &current), before, "{rejection:?}");
    assert!(
        !Path::new(&mgr.settings.staging_dir)
            .join("stage-admission-script-ran")
            .exists()
    );
}

#[test]
fn public_stage_admission_rejects_incomplete_inventory() {
    reject_before_writes(Rejection::Inventory);
}

#[test]
fn public_stage_admission_rejects_changed_revision() {
    reject_before_writes(Rejection::Revision);
}

#[test]
fn public_stage_admission_verifies_remote_weight_evidence_before_compatibility() {
    reject_before_writes(Rejection::EvidenceDigest);
}

#[test]
fn public_stage_admission_rejects_missing_weight_evidence() {
    reject_before_writes(Rejection::EvidenceMissing);
}

#[test]
fn public_stage_admission_rejects_incompatible_artifact_selection() {
    reject_before_writes(Rejection::IncompatibleSelection);
}

#[test]
fn public_stage_admission_requires_resolvable_legacy_resource() {
    reject_before_writes(Rejection::LegacyDirectory);
}

#[test]
fn public_stage_admission_positive_control_fetches_verifies_and_runs_download() {
    let (_home, mut mgr, current, mut card, package, _) = fixture();
    db::add_api_key(
        &current.card_id,
        Some("preserved stage-admission credential"),
    )
    .unwrap();
    let server = AssetServer::new();
    server.publish(&mut card, &package);
    let revision = card_update::revision(&card_update::remote_manifest(&card));
    let calls = Arc::new(AtomicUsize::new(0));
    mgr.store = Box::new(CardSource {
        card,
        calls: calls.clone(),
    });
    let _transport = script_runner::test_loopback_fetch::install(&server.origin);
    let before = snapshot(&mgr, &current);
    let result = mgr
        .stage_version(&current.card_id, &revision, &mut |_| {}, Default::default())
        .unwrap();
    assert_eq!(result["version_id"], revision);
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let staged = version_store::get(&current.card_id, &revision)
        .unwrap()
        .unwrap();
    assert_eq!(staged.state, "ready");
    let root = Path::new(staged.models_root.as_deref().unwrap());
    assert_eq!(
        fs::read(root.join("new/new.tachyon")).unwrap(),
        b"new-weights"
    );
    assert!(
        config::data_dir()
            .join("cards")
            .join(&staged.runtime_card_id)
            .join("download_model.sh")
            .is_file()
    );
    let after = snapshot(&mgr, &current);
    assert_eq!(after.record, before.record);
    assert_eq!(after.keys, before.keys);
    for (path, content) in before.files {
        assert_eq!(after.files.get(&path), Some(&content), "{}", path.display());
    }
    let mut observed = server.requests();
    observed.sort();
    let mut expected: Vec<_> = package
        .scripts
        .keys()
        .map(|name| format!("/scripts/{name}"))
        .chain(package.assets.keys().map(|name| format!("/assets/{name}")))
        .collect();
    expected.push("/assets/weights.sha256".into());
    expected.sort();
    assert_eq!(observed, expected);
}
