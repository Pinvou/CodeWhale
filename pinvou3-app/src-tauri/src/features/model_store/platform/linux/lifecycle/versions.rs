//! Isolated native weight generations, verified preparation, explicit switching and cleanup.
use super::super::{
    card_update,
    version_store::{self, Version},
    weight_identity,
};
use super::*;
use sha2::{Digest, Sha256};
use std::fs;
use std::io::Read;
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

fn now() -> String {
    chrono::Utc::now().to_rfc3339()
}
fn fail(message: impl Into<String>) -> ModelManagerError {
    ModelManagerError::new(message)
}

fn runtime_path(rec: &db::StoreModelRecord) -> Result<PathBuf, ModelManagerError> {
    let id = rec.runtime_card_id.as_deref().unwrap_or(&rec.card_id);
    Ok(config::data_dir()
        .join("cards")
        .join(config::safe_path_component(id).map_err(fail)?))
}

fn owned_directory(root: &Path, parts: &[&str]) -> Result<PathBuf, ModelManagerError> {
    let mut path = root.canonicalize().map_err(|e| fail(e.to_string()))?;
    let uid = fs::metadata("/proc/self")
        .map_err(|e| fail(e.to_string()))?
        .uid();
    for part in parts {
        path.push(config::safe_path_component(part).map_err(fail)?);
        match fs::DirBuilder::new().mode(0o700).create(&path) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {}
            Err(e) => return Err(fail(e.to_string())),
        }
        let meta = path.symlink_metadata().map_err(|e| fail(e.to_string()))?;
        if !meta.is_dir() || meta.uid() != uid || meta.mode() & 0o022 != 0 {
            return Err(fail("版本目录归属或权限不安全，已中止。"));
        }
    }
    Ok(path)
}

fn file_digest(
    path: &Path,
    control: &script_runner::RunControl,
) -> Result<String, ModelManagerError> {
    let mut file = fs::File::open(path).map_err(|e| fail(e.to_string()))?;
    let before = file.metadata().map_err(|e| fail(e.to_string()))?;
    let mut hash = Sha256::new();
    let mut buffer = vec![0; 1024 * 1024];
    loop {
        if control.cancelled() {
            return Err(fail("已取消"));
        }
        let n = file.read(&mut buffer).map_err(|e| fail(e.to_string()))?;
        if n == 0 {
            break;
        }
        hash.update(&buffer[..n]);
    }
    let after = file.metadata().map_err(|e| fail(e.to_string()))?;
    if (before.len(), before.mtime(), before.mtime_nsec())
        != (after.len(), after.mtime(), after.mtime_nsec())
    {
        return Err(fail("校验期间权重发生变化，请重试。"));
    }
    Ok(crate::platform::encoding::hex_lower(&hash.finalize()))
}

fn single_weight(rec: &db::StoreModelRecord) -> Result<(String, String), ModelManagerError> {
    let blob = rec
        .assets_content
        .get(weight_identity::MANIFEST)
        .ok_or_else(|| fail("此卡片缺少独立的权重清单，暂不支持版本化升级。"))?;
    let files = weight_identity::verified(blob, &blob.sha256).map_err(fail)?;
    if files.len() != 1 {
        return Err(fail("此版本暂不支持多文件权重的隔离升级；当前部署已保留。"));
    }
    files
        .into_iter()
        .next()
        .ok_or_else(|| fail("此卡片缺少可确认的单文件权重。"))
}

fn find_weight(root: &Path, name: &str) -> Result<PathBuf, ModelManagerError> {
    let canonical = root.canonicalize().map_err(|e| fail(e.to_string()))?;
    let mut stack = vec![(root.to_path_buf(), 0)];
    let mut found = Vec::new();
    let mut visited = 0;
    while let Some((dir, depth)) = stack.pop() {
        if depth > 5 || visited > 10000 {
            return Err(fail("权重目录过于复杂，无法可靠确认文件归属。"));
        }
        for entry in fs::read_dir(dir).map_err(|e| fail(e.to_string()))? {
            let entry = entry.map_err(|e| fail(e.to_string()))?;
            visited += 1;
            let kind = entry.file_type().map_err(|e| fail(e.to_string()))?;
            if kind.is_dir() {
                stack.push((entry.path(), depth + 1));
            } else if kind.is_file() && entry.path().ends_with(name) {
                let path = entry
                    .path()
                    .canonicalize()
                    .map_err(|e| fail(e.to_string()))?;
                if path.starts_with(&canonical) {
                    found.push(path);
                }
            }
        }
    }
    if found.len() != 1 {
        return Err(fail("无法唯一确认隔离目录中的权重文件，未切换部署。"));
    }
    found
        .pop()
        .ok_or_else(|| fail("无法唯一确认隔离目录中的权重文件，未切换部署。"))
}

pub(super) fn legacy_resource_dir(
    rec: &db::StoreModelRecord,
    configured_root: &Path,
) -> Option<PathBuf> {
    let allowed = rec
        .models_root
        .as_deref()
        .map(Path::new)
        .unwrap_or(configured_root)
        .canonicalize()
        .ok()?;
    if let Some(runtime) = runtime_path(rec).ok() {
        if let Ok(env) = fs::read_to_string(runtime.join(".deploy-env")) {
            if let Some(value) = env
                .lines()
                .find_map(|line| line.strip_prefix("MODEL_DIR=\"")?.strip_suffix('"'))
            {
                if let Ok(path) = PathBuf::from(value).canonicalize() {
                    if path != allowed && path.starts_with(&allowed) {
                        return Some(path);
                    }
                }
            }
        }
    }
    rec.runtime_card_id
        .as_ref()
        .and_then(|_| rec.models_root.as_ref())
        .and_then(|root| PathBuf::from(root).canonicalize().ok())
}

pub(super) fn managed_storage(rec: &db::StoreModelRecord) -> Result<bool, ModelManagerError> {
    Ok(
        version_store::get(&rec.card_id, &version_store::active_id(rec))
            .map_err(fail)?
            .is_some_and(|v| v.managed),
    )
}

pub(super) fn ensure_not_deleting(rec: &db::StoreModelRecord) -> Result<(), ModelManagerError> {
    if version_store::get(&rec.card_id, &version_store::active_id(rec))
        .map_err(fail)?
        .is_some_and(|v| v.state == "deleting")
    {
        return Err(fail("当前版本的彻底删除尚未完成，请重试删除模型。"));
    }
    Ok(())
}

fn valid_resource_marker(
    root: &Path,
    expected: &str,
    deleting: bool,
) -> Result<bool, ModelManagerError> {
    let marker = root.join(".pinvou-version.json");
    Ok(match marker.symlink_metadata() {
        Ok(meta) => {
            meta.is_file()
                && meta.nlink() == 1
                && fs::read_to_string(marker).ok().as_deref() == Some(expected)
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound && deleting => fs::read_dir(root)
            .map_err(|e| fail(e.to_string()))?
            .next()
            .is_none(),
        _ => false,
    })
}

/// Downloaded legacy cards have no deploy-env yet. Their missing runtime path
/// does not prove that an otherwise matching shared weight is unreferenced.
fn unresolved_weight_reference(other: &db::StoreModelRecord, owner: &db::StoreModelRecord) -> bool {
    if owner
        .source_repo_id
        .as_deref()
        .is_some_and(|repo| !repo.is_empty())
        && other.source_repo_id == owner.source_repo_id
        && (other.source_filename == owner.source_filename
            || other.source_filename.as_deref().is_none_or(str::is_empty)
            || owner.source_filename.as_deref().is_none_or(str::is_empty))
    {
        return true;
    }
    let inventory = |rec: &db::StoreModelRecord| {
        let blob = rec.assets_content.get(weight_identity::MANIFEST)?;
        weight_identity::verified(blob, &blob.sha256).ok()
    };
    match (inventory(other), inventory(owner)) {
        (Some(other), Some(owner)) => owner
            .iter()
            .any(|(name, hash)| other.get(name) == Some(hash)),
        _ => false,
    }
}

/// Check references before scripts can remove deployment state, then repeat
/// after the service stops before deleting any host-managed files.
pub(super) fn current_purge_preflight(
    rec: &db::StoreModelRecord,
    configured_root: &Path,
) -> Result<Option<(Version, PathBuf, PathBuf)>, ModelManagerError> {
    let id = version_store::active_id(rec);
    let version = version_store::get(&rec.card_id, &id).map_err(fail)?;
    let managed = version.as_ref().is_some_and(|v| v.managed);
    let root = if managed {
        Some(PathBuf::from(
            rec.models_root
                .as_ref()
                .ok_or_else(|| fail("缺少版本存储目录"))?,
        ))
    } else {
        legacy_resource_dir(rec, configured_root)
    };
    let runtime = runtime_path(rec)?;
    let possible_legacy_reference = |other: &db::StoreModelRecord| {
        rec.source_repo_id
            .as_deref()
            .is_some_and(|repo| !repo.is_empty())
            && rec.source_repo_id == other.source_repo_id
            || unresolved_weight_reference(rec, other)
    };
    let overlaps = |other: Option<&Path>, other_record: Option<&db::StoreModelRecord>| match (
        root.as_deref(),
        other,
    ) {
        (Some(root), Some(other)) => root.starts_with(other) || other.starts_with(root),
        _ => !managed && other_record.is_some_and(possible_legacy_reference),
    };
    let other_versions = version_store::list(None).map_err(fail)?;
    if other_versions.iter().any(|v| {
        !(v.card_id == rec.card_id && v.version_id == id)
            && (config::data_dir().join("cards").join(&v.runtime_card_id) == runtime
                || overlaps(v.resource_dir.as_deref().map(Path::new), v.record.as_ref()))
    }) {
        return Err(fail("此版本的存储仍被引用，已保留资源和记录。"));
    }
    for other in db::list_store_models()
        .map_err(|e| fail(e.to_string()))?
        .iter()
        .filter(|r| r.card_id != rec.card_id)
    {
        if runtime_path(other).ok().as_ref() == Some(&runtime)
            || overlaps(
                legacy_resource_dir(other, configured_root).as_deref(),
                Some(other),
            )
        {
            return Err(fail("此版本的存储仍被其他模型引用，已保留资源和记录。"));
        }
    }
    if !managed {
        return Ok(None);
    }
    let version = version.unwrap();
    let root = root.unwrap();
    if root.exists() {
        let expected = serde_json::json!({"card_id":rec.card_id,"version_id":id}).to_string();
        if !root
            .symlink_metadata()
            .map_err(|e| fail(e.to_string()))?
            .is_dir()
            || !valid_resource_marker(&root, &expected, version.state == "deleting")?
        {
            return Err(fail("版本目录归属不符，已保留资源和记录。"));
        }
    }
    Ok(Some((version, root, runtime)))
}

/// Called after the card has removed its service with weight deletion disabled.
pub(super) fn purge_current_storage(
    rec: &db::StoreModelRecord,
    configured_root: &Path,
) -> Result<(), ModelManagerError> {
    let Some((mut version, root, runtime)) = current_purge_preflight(rec, configured_root)? else {
        return Ok(());
    };
    ensure_unused(&runtime, Some(&root))?;
    version.state = "deleting".into();
    version_store::put(&version).map_err(fail)?;
    if root.exists() {
        remove_managed_resource(&root)?;
    }
    if runtime.exists() {
        fs::remove_dir_all(runtime).map_err(|e| fail(e.to_string()))?;
    }
    Ok(())
}

fn ensure_unused(runtime: &Path, resource: Option<&Path>) -> Result<(), ModelManagerError> {
    // Read only; neither PID files nor discovered processes authorize signals.
    let uid = fs::metadata("/proc/self")
        .map_err(|e| fail(e.to_string()))?
        .uid();
    let deadline = std::time::Instant::now() + Duration::from_secs(5);
    for entry in fs::read_dir("/proc").map_err(|e| fail(e.to_string()))? {
        if std::time::Instant::now() > deadline {
            return Err(fail("进程检查超时，未删除或切换任何资源。"));
        }
        let entry = entry.map_err(|e| fail(e.to_string()))?;
        if entry.file_name().to_string_lossy().parse::<u32>().is_err() {
            continue;
        }
        if entry.metadata().map(|m| m.uid() != uid).unwrap_or(true) {
            continue;
        }
        if fs::read_link(entry.path().join("cwd")).ok().as_deref() == Some(runtime) {
            return Err(fail("此版本仍有运行进程，请先停用。"));
        }
        if let Some(resource) = resource {
            let Ok(file) = fs::File::open(entry.path().join("cmdline")) else {
                continue;
            };
            let mut bytes = Vec::new();
            if file.take(65537).read_to_end(&mut bytes).is_err() {
                continue;
            }
            for arg in bytes.split(|b| *b == 0).skip(1) {
                let Ok(text) = std::str::from_utf8(arg) else {
                    continue;
                };
                let path = Path::new(text);
                if path.is_absolute() && path.starts_with(resource) {
                    return Err(fail("此版本的权重仍被运行进程使用，未删除或切换资源。"));
                }
            }
        }
    }
    Ok(())
}

impl ModelManager {
    pub(super) fn remote_weight_evidence(
        &self,
        rec: &db::StoreModelRecord,
        card: &StoreCardDetail,
        control: Option<&script_runner::RunControl>,
    ) -> Result<Option<db::ScriptBlob>, ModelManagerError> {
        let Some(url) = card.assets.get(weight_identity::MANIFEST) else {
            return Ok(None);
        };
        let hash = card
            .asset_sha256
            .as_ref()
            .and_then(|h| h.get(weight_identity::MANIFEST))
            .ok_or_else(|| fail("卡片权重清单缺少摘要，已保留原版本。"))?;
        if let Some(blob) = rec.assets_content.get(weight_identity::MANIFEST) {
            if weight_identity::verified(blob, hash).is_ok() {
                return Ok(Some(blob.clone()));
            }
        }
        if control.is_some_and(|c| c.cancelled()) {
            return Err(fail("已取消"));
        }
        let content = script_runner::fetch_script_content(
            url,
            Some(hash),
            script_runner::FetchPolicy {
                may_run_as_root: true,
                allow_local_path: false,
            },
        )?;
        Ok(Some(db::ScriptBlob {
            sha256: script_runner::sha256_hex(&content),
            content,
        }))
    }

    pub fn versions(&self, card_id: &str) -> Result<serde_json::Value, ModelManagerError> {
        let current = self.get_record(card_id)?;
        version_store::recover_interrupted(card_id).map_err(fail)?;
        let mut versions = version_store::list(Some(card_id)).map_err(fail)?;
        let id = version_store::active_id(&current);
        let deleting = versions
            .iter()
            .any(|v| v.version_id == id && v.state == "deleting");
        versions.retain(|v| v.version_id != id);
        versions.insert(
            0,
            Version {
                card_id: card_id.into(),
                version_id: id.clone(),
                state: if deleting { "deleting" } else { "current" }.into(),
                models_root: current.models_root.clone(),
                runtime_card_id: current
                    .runtime_card_id
                    .clone()
                    .unwrap_or_else(|| card_id.into()),
                resource_dir: legacy_resource_dir(&current, &self.settings.models_path())
                    .map(|p| p.to_string_lossy().into_owned()),
                managed: managed_storage(&current)?,
                created_at: current.downloaded_at.clone(),
                record: Some(current),
                error: None,
            },
        );
        Ok(serde_json::json!(versions.iter().map(|v|serde_json::json!({
            "version_id":v.version_id,"state":v.state,"created_at":v.created_at,"error":v.error,
            "engine_version":v.record.as_ref().and_then(|r|r.engine_version.clone()),
            "filename":v.record.as_ref().and_then(|r|r.source_filename.clone()),
            "size_bytes":v.models_root.as_ref().filter(|_|v.managed).or(v.resource_dir.as_ref()).map(|p|super::super::disk::dir_size_bytes(Path::new(p))).unwrap_or_else(||v.record.as_ref().map(|r|r.size_bytes.max(0) as u64).unwrap_or(0)),
            "can_switch":(["ready","history"].contains(&v.state.as_str())),
            "can_delete":v.version_id != id && v.state!="current" && v.state!="downloading",
            "can_retry":v.state=="failed",
        })).collect::<Vec<_>>()))
    }

    pub fn stage_version(
        &self,
        card_id: &str,
        revision: &str,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<serde_json::Value, ModelManagerError> {
        let _resources = version_store::resource_operation_lock(false).map_err(fail)?;
        let current = self.get_record(card_id)?;
        ensure_not_deleting(&current)?;
        let card = self.store.get_card(card_id)?;
        card_update::validate_inventory(
            &card.scripts,
            card.script_sha256.as_ref().unwrap_or(&BTreeMap::new()),
            &card.assets,
            card.asset_sha256.as_ref().unwrap_or(&BTreeMap::new()),
        )
        .map_err(fail)?;
        if card_update::revision(&card_update::remote_manifest(&card)) != revision {
            return Err(fail("线上卡片已再次更新，请重新检查。"));
        }
        let evidence = self.remote_weight_evidence(&current, &card, Some(&control))?;
        let assessment = card_update::compatibility(&current, &card, evidence.as_ref());
        if !assessment.can_stage {
            return Err(fail("此卡片不支持当前版本的隔离升级，现有部署已保留。"));
        }
        if legacy_resource_dir(&current, &self.settings.models_path()).is_none() {
            return Err(fail(
                "无法确认旧版部署的权重目录，请先修复旧版部署，确保可回退后再升级。",
            ));
        }
        let pkg = self.fetch_and_store_scripts(&card, Some(&control))?;
        self.stage_verified(&current, &card, revision, pkg, on_progress, control)
    }

    fn stage_verified(
        &self,
        current: &db::StoreModelRecord,
        card: &StoreCardDetail,
        revision: &str,
        pkg: CardPackage,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<serde_json::Value, ModelManagerError> {
        let card_id = current.card_id.as_str();
        let blob = pkg
            .assets
            .get(weight_identity::MANIFEST)
            .ok_or_else(|| fail("缺少权重校验依据"))?;
        let expected = card
            .asset_sha256
            .as_ref()
            .and_then(|h| h.get(weight_identity::MANIFEST))
            .ok_or_else(|| fail("缺少权重摘要"))?;
        let files = weight_identity::verified(blob, expected).map_err(fail)?;
        if files.len() != 1 {
            return Err(fail("暂不支持多文件隔离升级"));
        }
        if let Some(v) = version_store::get(card_id, revision).map_err(fail)? {
            if ["ready", "current", "history"].contains(&v.state.as_str()) {
                return Ok(serde_json::json!({"version_id":revision}));
            }
        }
        let space = self.check_can_download(card.summary.size_bytes);
        if !space.ok {
            return Err(fail(space.reason));
        }
        let safe = config::safe_path_component(card_id).map_err(fail)?;
        // Full revision IDs are validated before any path construction.
        if revision.len() != 64 || !revision.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Err(fail("无效版本标识"));
        }
        let models_root = owned_directory(
            &self.settings.models_path(),
            &[".versions", &safe, revision],
        )?;
        let storage_id = format!("{safe}--{}", &revision[..32]);
        let cards = config::data_dir().join("cards");
        fs::create_dir_all(&cards).map_err(|e| fail(e.to_string()))?;
        owned_directory(&cards, &[&storage_id])?;
        ensure_unused(
            &config::data_dir().join("cards").join(&storage_id),
            Some(&models_root),
        )?;
        let marker = serde_json::json!({"card_id":card_id,"version_id":revision}).to_string();
        let marker_path = models_root.join(".pinvou-version.json");
        match fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&marker_path)
        {
            Ok(mut file) => {
                use std::io::Write;
                file.write_all(marker.as_bytes())
                    .map_err(|e| fail(e.to_string()))?;
            }
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
                let meta = marker_path
                    .symlink_metadata()
                    .map_err(|e| fail(e.to_string()))?;
                if !meta.is_file()
                    || meta.nlink() != 1
                    || fs::read_to_string(&marker_path).ok().as_deref() != Some(&marker)
                {
                    return Err(fail("版本目录归属不符"));
                }
            }
            Err(e) => return Err(fail(e.to_string())),
        }
        let mut version = Version {
            card_id: card_id.into(),
            version_id: revision.into(),
            state: "downloading".into(),
            record: None,
            models_root: Some(models_root.to_string_lossy().into_owned()),
            runtime_card_id: storage_id.clone(),
            resource_dir: Some(models_root.to_string_lossy().into_owned()),
            managed: true,
            created_at: now(),
            error: None,
        };
        version_store::put(&version).map_err(fail)?;
        let result = (|| {
            on_progress("下载并校验新版卡片；当前部署与旧版权重保持可用。");
            let source = ScriptsSource::Local {
                blobs: pkg.scripts.clone(),
                assets: pkg.assets.clone(),
                profiles: card.profiles.clone(),
            };
            let mut env = self.env_for_card(&card, &current.model_name);
            env.insert(
                "LMM_MODELS_DIR".into(),
                models_root.to_string_lossy().into_owned(),
            );
            env.insert("LMM_STORAGE_CARD_ID".into(), storage_id.clone());
            let probe = self.engine_probe(&source, card_id, &env);
            self.install_engine(&source, &env, on_progress, control.clone(), probe.need_root)?;
            db::mark_engine_installed(
                &card.summary.engine_name,
                card.summary.engine_version.as_deref(),
                None,
                source.script_hash("install_engine").as_deref(),
            )
            .map_err(|e| fail(e.to_string()))?;
            self.run_card_script_with_retry(
                &source,
                "download_model",
                &env,
                on_progress,
                control.clone(),
                false,
            )?;
            on_progress("独立校验新版权重 SHA-256；通过后才允许切换。");
            for (name, hash) in &files {
                let path = find_weight(&models_root, name)?;
                if file_digest(&path, &control)? != *hash {
                    return Err(fail("新版权重完整性校验失败，当前部署未改变。"));
                }
            }
            let mut rec = (*current).clone();
            rec.card_name = card.summary.name.clone();
            rec.org = card.summary.org.clone();
            rec.description = card.summary.description.clone();
            rec.engine_name = card.summary.engine_name.clone();
            rec.engine_version = card.summary.engine_version.clone();
            rec.size_bytes =
                i64::try_from(card.summary.size_bytes).map_err(|e| fail(e.to_string()))?;
            rec.quantization = card.summary.quantization.clone();
            rec.source_repo_id = card.source_repo_id.clone();
            rec.source_filename = card.source_filename.clone();
            rec.deploy_topology = card.summary.deploy_topology;
            rec.profiles = card.profiles.clone();
            rec.active_profile_id = None;
            rec.scripts = card
                .scripts
                .iter()
                .map(|(k, v)| (k.clone(), Some(v.clone())))
                .collect();
            rec.scripts_content = pkg.scripts;
            rec.assets_content = pkg.assets;
            rec.scripts_fetched_at = Some(now());
            rec.status = "downloaded".into();
            rec.running = false;
            rec.downloaded_at = now();
            rec.deployed_at = None;
            rec.endpoint_url = None;
            rec.endpoint_model = None;
            rec.container_name = None;
            rec.deployed_script_sha256 = None;
            rec.card_update_pending = false;
            rec.models_root = version.models_root.clone();
            rec.runtime_card_id = Some(storage_id);
            rec.active_version_id = Some(revision.into());
            version.record = Some(rec);
            version.state = "ready".into();
            version.error = None;
            Ok(())
        })();
        if let Err(error) = result {
            version.state = "failed".into();
            version.error = Some(error.to_string());
            version_store::put(&version).map_err(fail)?;
            return Err(error);
        }
        version_store::put(&version).map_err(fail)?;
        on_progress("新版已验证，当前部署未切换；停用当前模型后，可在历史版本中切换。");
        Ok(serde_json::json!({"version_id":revision}))
    }

    pub fn switch_version(
        &self,
        card_id: &str,
        id: &str,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<serde_json::Value, ModelManagerError> {
        let _resources = version_store::resource_operation_lock(true).map_err(fail)?;
        let current = self.get_record(card_id)?;
        ensure_not_deleting(&current)?;
        if current.running {
            return Err(fail("请先停用当前模型，再切换版本。"));
        }
        let managed = managed_storage(&current)?;
        let resource = if managed {
            current.models_root.as_ref().map(PathBuf::from)
        } else {
            legacy_resource_dir(&current, &self.settings.models_path())
        };
        if resource.is_none() {
            return Err(fail(
                "无法确认当前版本的权重目录，不能保证回退；请先修复当前部署，未切换版本。",
            ));
        }
        ensure_unused(&runtime_path(&current)?, resource.as_deref())?;
        let mut target = version_store::get(card_id, id)
            .map_err(fail)?
            .ok_or_else(|| fail("目标版本不存在"))?;
        if !["ready", "history"].contains(&target.state.as_str()) {
            return Err(fail("目标版本尚未验证完成"));
        }
        if target.state == "ready" {
            // A new upgrade promises a usable rollback. A history target is
            // also a recovery route and remains usable if the current file broke.
            on_progress("校验当前版本权重，确认升级后仍可回退。");
            let (name, hash) = single_weight(&current)?;
            let old_file = find_weight(resource.as_deref().unwrap(), &name).map_err(|_| {
                fail("当前权重缺失，不能保证升级后回退；请先修复当前部署，未切换版本。")
            })?;
            if file_digest(&old_file, &control)? != hash {
                return Err(fail(
                    "当前权重校验失败，不能保证升级后回退；请先修复当前部署，未切换版本。",
                ));
            }
        }
        let rec = target
            .record
            .as_mut()
            .ok_or_else(|| fail("目标版本缺少部署记录"))?;
        for blob in rec
            .scripts_content
            .values()
            .chain(rec.assets_content.values())
        {
            if !script_runner::sha256_hex(&blob.content).eq_ignore_ascii_case(&blob.sha256) {
                return Err(fail("目标版本的本地卡片校验失败，未切换部署。"));
            }
        }
        let (name, hash) = single_weight(rec)?;
        let root = legacy_resource_dir(rec, &self.settings.models_path())
            .ok_or_else(|| fail("无法确认目标权重目录"))?;
        ensure_unused(&runtime_path(rec)?, Some(&root))?;
        on_progress("校验目标版本权重；当前卡片记录保持不变。");
        if file_digest(&find_weight(&root, &name)?, &control)? != hash {
            return Err(fail("目标权重校验失败，未切换版本。"));
        }
        let source = self.local_scripts_source(rec)?;
        let selected_profile = super::super::deploy_profiles::selected_id(
            card_id,
            source.profiles(),
            rec.active_profile_id.as_deref(),
        )
        .map(str::to_owned);
        let mut env = self.env_for_record(rec);
        if let Some(profile) = &selected_profile {
            env.insert("LMM_DEPLOY_PROFILE".into(), profile.clone());
        }
        let probe = self.engine_probe(&source, card_id, &env);
        if !probe.installed {
            self.install_engine(&source, &env, on_progress, control.clone(), probe.need_root)?;
        }
        self.run_card_script(&source, "deploy", &env, on_progress, control.clone(), false)?;
        if control.cancelled() {
            return Err(fail("已取消"));
        }
        rec.status = "deployed".into();
        rec.deployed_at = Some(now());
        rec.card_update_pending = false;
        rec.running = false;
        rec.deployed_script_sha256 = source.script_hash("deploy");
        rec.active_profile_id = selected_profile;
        let mut old = Version {
            card_id: card_id.into(),
            version_id: version_store::active_id(&current),
            state: "history".into(),
            models_root: current.models_root.clone(),
            runtime_card_id: current
                .runtime_card_id
                .clone()
                .unwrap_or_else(|| card_id.into()),
            resource_dir: resource.map(|p| p.to_string_lossy().into_owned()),
            managed,
            created_at: current.downloaded_at.clone(),
            record: Some(current.clone()),
            error: None,
        };
        old.models_root = Some(
            current
                .models_root
                .clone()
                .unwrap_or_else(|| self.settings.models_path().to_string_lossy().into_owned()),
        );
        if let Some(record) = old.record.as_mut() {
            record.models_root = old.models_root.clone();
        }
        version_store::activate(&current, &old, &target).map_err(fail)?;
        on_progress("版本已切换并部署；旧版已归档，可以回退或清理。请启用模型。");
        Ok(serde_json::json!({"switched":true}))
    }

    pub fn delete_version(
        &self,
        card_id: &str,
        id: &str,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<serde_json::Value, ModelManagerError> {
        let _resources = version_store::resource_operation_lock(true).map_err(fail)?;
        let current = self.get_record(card_id)?;
        if version_store::active_id(&current) == id {
            return Err(fail("当前版本不能从历史版本中删除。"));
        }
        let Some(mut version) = version_store::get(card_id, id).map_err(fail)? else {
            return Ok(
                serde_json::json!({"freed_bytes":0,"remaining_paths":[],"already_removed":true}),
            );
        };
        if ["current", "downloading"].contains(&version.state.as_str()) {
            return Err(fail("此版本正在使用，不能清理。"));
        }
        let runtime = config::data_dir()
            .join("cards")
            .join(config::safe_path_component(&version.runtime_card_id).map_err(fail)?);
        let resource = version
            .resource_dir
            .as_ref()
            .map(PathBuf::from)
            .ok_or_else(|| fail("无法确认旧版权重归属，未删除资源。"))?;
        ensure_unused(&runtime, Some(&resource))?;
        let others = version_store::list(None).map_err(fail)?;
        let active = db::list_store_models().map_err(|e| fail(e.to_string()))?;
        let shared_runtime = active
            .iter()
            .any(|r| runtime_path(r).ok().as_ref() == Some(&runtime))
            || others.iter().any(|v| {
                !(v.card_id == card_id && v.version_id == id)
                    && v.runtime_card_id == version.runtime_card_id
            });
        let shared_resource = active.iter().any(|other| {
            if let Some(path) = legacy_resource_dir(other, &self.settings.models_path()) {
                resource.starts_with(&path) || path.starts_with(&resource)
            } else {
                !version.managed
                    && version
                        .record
                        .as_ref()
                        .is_some_and(|owner| unresolved_weight_reference(other, owner))
            }
        }) || others
            .iter()
            .filter(|v| !(v.card_id == card_id && v.version_id == id))
            .filter_map(|v| v.resource_dir.as_ref())
            .map(PathBuf::from)
            .any(|other| resource.starts_with(&other) || other.starts_with(&resource));
        if shared_resource || shared_runtime {
            return Err(fail(
                "此版本的权重或运行目录仍被其他版本引用，未删除任何资源。",
            ));
        }
        if runtime.exists()
            && !runtime
                .symlink_metadata()
                .map_err(|e| fail(e.to_string()))?
                .is_dir()
        {
            return Err(fail("运行目录归属异常，未删除资源。"));
        }
        let mut freed = 0u64;
        let mut legacy_file = None;
        let mut remaining = Vec::new();
        if resource.exists() {
            if !resource
                .symlink_metadata()
                .map_err(|e| fail(e.to_string()))?
                .is_dir()
            {
                return Err(fail("拒绝清理链接或非目录资源。"));
            }
            if version.managed {
                let expected = serde_json::json!({"card_id":card_id,"version_id":id}).to_string();
                // A crash between removing the last marker and the empty root
                // may leave only that directory. Never accept unknown contents.
                if !valid_resource_marker(&resource, &expected, version.state == "deleting")? {
                    return Err(fail("版本目录归属校验失败，未删除资源。"));
                }
                freed = allocated_exclusive(&resource)?;
            } else if let Some(rec) = version.record.as_ref() {
                let (name, hash) = single_weight(rec)?;
                let file = resource.join(name);
                let metadata = match file.symlink_metadata() {
                    Ok(metadata) => Some(metadata),
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => None,
                    Err(error) => return Err(fail(error.to_string())),
                };
                if let Some(meta) = metadata {
                    if !meta.is_file() {
                        return Err(fail("旧版权重不是普通文件，未删除资源。"));
                    }
                    if !file
                        .canonicalize()
                        .map_err(|e| fail(e.to_string()))?
                        .starts_with(resource.canonicalize().map_err(|e| fail(e.to_string()))?)
                    {
                        return Err(fail("旧版权重路径越界"));
                    }
                    if file_digest(&file, &control)? != hash {
                        return Err(fail("旧版权重已变化，未删除资源。"));
                    }
                    if meta.nlink() == 1 {
                        freed = meta.blocks() * 512;
                    }
                    legacy_file = Some(file);
                }
                remaining.push(resource.to_string_lossy().into_owned());
            } else {
                return Err(fail("旧版缺少文件清单，未删除资源。"));
            }
        }
        if control.cancelled() {
            return Err(fail("已取消"));
        }
        // Commit point: after this marker cleanup is resumable, never reported as
        // a rollbackable history item whose weights might already be gone.
        version.state = "deleting".into();
        version_store::put(&version).map_err(fail)?;
        if version.managed && resource.exists() {
            remove_managed_resource(&resource)?;
        }
        if let Some(file) = legacy_file {
            fs::remove_file(file).map_err(|e| fail(e.to_string()))?;
        }
        if runtime.exists() {
            fs::remove_dir_all(&runtime).map_err(|e| fail(e.to_string()))?;
        }
        version_store::remove(card_id, id).map_err(fail)?;
        on_progress("选中的历史版本已清理，共享引擎保留。统计只计入实际删除的独占文件块。");
        Ok(serde_json::json!({"freed_bytes":freed,"remaining_paths":remaining}))
    }
}

/// Preserve the ownership marker through any partial recursive deletion. Only
/// remove it once the tree is empty so permission/IO failures remain retryable.
fn remove_managed_resource(root: &Path) -> Result<(), ModelManagerError> {
    let marker = root.join(".pinvou-version.json");
    for entry in fs::read_dir(root).map_err(|e| fail(e.to_string()))? {
        let entry = entry.map_err(|e| fail(e.to_string()))?;
        if entry.path() == marker {
            continue;
        }
        if entry.file_type().map_err(|e| fail(e.to_string()))?.is_dir() {
            fs::remove_dir_all(entry.path()).map_err(|e| fail(e.to_string()))?;
        } else {
            fs::remove_file(entry.path()).map_err(|e| fail(e.to_string()))?;
        }
    }
    match fs::remove_file(marker) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(fail(e.to_string())),
    }
    fs::remove_dir(root).map_err(|e| fail(e.to_string()))
}

fn allocated_exclusive(path: &Path) -> Result<u64, ModelManagerError> {
    let mut total = 0;
    for entry in fs::read_dir(path).map_err(|e| fail(e.to_string()))? {
        let entry = entry.map_err(|e| fail(e.to_string()))?;
        let meta = entry
            .path()
            .symlink_metadata()
            .map_err(|e| fail(e.to_string()))?;
        if meta.is_dir() {
            total += allocated_exclusive(&entry.path())?;
        } else if meta.is_file() && meta.nlink() == 1 {
            total += meta.blocks() * 512;
        }
    }
    Ok(total)
}

#[cfg(test)]
mod tests;
