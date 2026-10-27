//! The store's published current pointer is the only ordinary execution target.
//! History is a deny list, never a second discovery or fallback source.
use super::*;
use crate::app::{api_err, ApiErr};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::File;

const LIMIT: u64 = 1024 * 1024;

#[derive(Clone, Debug, Default)]
pub struct Snapshot {
    pub selected: BTreeMap<String, String>,
    pub archived: BTreeMap<String, String>,
    entries: BTreeMap<String, serde_json::Value>,
    cleanup_only: BTreeSet<String>,
    raw: serde_json::Value,
}

impl Snapshot {
    pub fn metadata(&self, card: &str) -> Option<serde_json::Value> {
        let (logical, _) = self.selected.iter().find(|(_, target)| *target == card)?;
        let selected = self.raw["selections"]
            .as_array()?
            .iter()
            .find(|row| row["logical_card"].as_str() == Some(logical))?;
        let revision = self.entries.get(card).and_then(|entry| {
            entry
                .get("deployment_revision")
                .or_else(|| entry.get("fingerprint"))
        });
        Some(
            serde_json::json!({"logical_card":logical,"version_id":selected["version_id"],
            "selected_version":selected["version_id"],"deployment_revision":revision}),
        )
    }
    pub fn visible(&self, physical: &str) -> bool {
        !self.archived.contains_key(physical)
    }

    pub fn routable(&self, physical: &str) -> bool {
        self.visible(physical)
            && (!self.selected.values().any(|card| card == physical)
                || self.entries.contains_key(physical))
    }

    /// A private maintenance owner pins a physical object. Ordinary lifecycle
    /// admission still requires a complete startable entry. Unknown current
    /// revisions and true history never gain cleanup authority from a directory.
    pub fn maintenance_target(&self, physical: &str) -> Result<bool, String> {
        if self.archived.contains_key(physical)
            || self
                .selected
                .get(physical)
                .is_some_and(|target| target != physical)
        {
            return Err("archived_model_version: 非当前物理版本不能取得清理预留".into());
        }
        if self.selected.values().any(|target| target == physical) {
            if self.cleanup_only.contains(physical) {
                return Ok(true);
            }
            if !self.entries.contains_key(physical) {
                return Err(
                    "store_cleanup_unconfirmed: 当前部署事实不可核实，未授予清理预留".into(),
                );
            }
        }
        Ok(false)
    }

    pub fn resolve(&self, requested: &str) -> Result<(String, Option<String>), String> {
        if let Some(card) = self.selected.get(requested) {
            return Ok((card.clone(), Some(requested.into())));
        }
        if let Some((logical, _)) = self.selected.iter().find(|(_, card)| *card == requested) {
            return Ok((requested.into(), Some(logical.clone())));
        }
        if self.archived.values().any(|logical| logical == requested) {
            return Err(
                "current_not_deployed: 请在模型商店完成当前版本部署，未回落历史目录".into(),
            );
        }
        if self.archived.contains_key(requested) {
            return Err(
                "archived_model_version: 历史版本已归档，不能通过普通接口启用或恢复".into(),
            );
        }
        Ok((requested.into(), None))
    }
}

fn parse(value: &serde_json::Value) -> Result<Snapshot, String> {
    if value["selection_policy"] != "latest-installed-v1" {
        return Err(
            "store_selection_upgrade_required: 请更新 Pinvou 商店读取器，不能回落旧目录".into(),
        );
    }
    let mut snapshot = Snapshot::default();
    let rows = |name: &str| {
        value[name]
            .as_array()
            .filter(|rows| rows.len() <= if name == "archived_models" { 4096 } else { 512 })
            .ok_or_else(|| format!("invalid_store_selection: {name}"))
    };
    let identity = |row: &serde_json::Value, name: &str| {
        row[name]
            .as_str()
            .filter(|value| safe_id(value))
            .map(str::to_owned)
            .ok_or_else(|| format!("invalid_store_selection: {name}"))
    };
    let mut physical = BTreeSet::new();
    for row in rows("selections")? {
        let logical = identity(row, "logical_card")?;
        let card = identity(row, "card")?;
        identity(row, "version_id")?;
        if snapshot.selected.insert(logical, card.clone()).is_some() || !physical.insert(card) {
            return Err("invalid_store_selection: duplicate current target".into());
        }
    }
    if snapshot
        .selected
        .iter()
        .any(|(logical, card)| logical != card && physical.contains(logical))
    {
        return Err("invalid_store_selection: logical alias is another current target".into());
    }
    for row in rows("archived_models")? {
        let logical = identity(row, "logical_card")?;
        let card = identity(row, "card")?;
        identity(row, "version_id")?;
        if physical.contains(&card) {
            return Err("invalid_store_selection: current target also archived".into());
        }
        if snapshot
            .archived
            .insert(card, logical.clone())
            .is_some_and(|old| old != logical)
        {
            return Err("invalid_store_selection: conflicting archive ownership".into());
        }
    }
    // An authoritative logical alias cannot simultaneously be an old physical
    // candidate, including after a legacy history row has been cleaned up.
    for (logical, current) in &snapshot.selected {
        if logical != current && !physical.contains(logical) {
            snapshot
                .archived
                .entry(logical.clone())
                .or_insert_with(|| logical.clone());
        }
    }
    for entry in rows("models")? {
        let logical = identity(entry, "logical_card")?;
        let card = identity(entry, "card")?;
        if snapshot.selected.get(&logical) != Some(&card)
            || value["selections"]
                .as_array()
                .unwrap()
                .iter()
                .find(|row| row["logical_card"].as_str() == Some(&logical))
                .is_none_or(|row| row["version_id"] != entry["version_id"])
            || snapshot.entries.insert(card, entry.clone()).is_some()
        {
            return Err("invalid_store_selection: installation is not current".into());
        }
    }
    // `skipped` is part of the existing inventory contract. Only this positive
    // current-download result distinguishes cleanup from corrupt/deleting or
    // otherwise unknown current records. Older snapshots remain fail-closed.
    let skipped = value
        .get("skipped")
        .cloned()
        .unwrap_or_else(|| serde_json::json!([]));
    let skipped_rows = skipped
        .as_array()
        .filter(|rows| rows.len() <= 512)
        .ok_or("invalid_store_selection: skipped")?;
    for row in skipped_rows {
        if row["reason"] != "current_not_deployed" {
            continue;
        }
        let logical = identity(row, "card")?;
        let physical = snapshot
            .selected
            .get(&logical)
            .ok_or("invalid_store_cleanup_target")?;
        if snapshot.entries.contains_key(physical) {
            return Err("invalid_store_cleanup_target".into());
        }
        snapshot.cleanup_only.insert(physical.clone());
    }
    let archives:Vec<_>=snapshot.archived.iter().map(|(card,logical)| {
        rows("archived_models").unwrap().iter().find(|row|row["card"].as_str()==Some(card)).cloned()
            .unwrap_or_else(||serde_json::json!({"card":card,"logical_card":logical,"version_id":"retired"}))
    }).collect();
    snapshot.raw = serde_json::json!({
        "selection_policy":"latest-installed-v1",
        "selections":value["selections"],
        "archived_models":archives,
        "models":value["models"],
        "skipped":skipped
    });
    Ok(snapshot)
}

pub fn cached(app: &App) -> Result<Snapshot, String> {
    let path = app.config.state_dir.join("store-selection.json");
    match trusted_file(&path, LIMIT) {
        Ok(raw) => parse(&serde_json::from_slice(&raw).map_err(|_| "invalid_store_selection")?),
        Err(_)
            if std::fs::symlink_metadata(&path)
                .is_err_and(|error| error.kind() == std::io::ErrorKind::NotFound)
                && reader(app)?.is_none()
                && std::fs::symlink_metadata(app.config.state_dir.join("store-selection-v1"))
                    .is_err_and(|error| error.kind() == std::io::ErrorKind::NotFound) =>
        {
            Ok(Snapshot::default())
        }
        Err(error) => Err(format!("store_selection_unavailable: {error}")),
    }
}

fn resource_lock(profile: &Path) -> Result<File, String> {
    let path = profile.join("model-store/version-operations.lock");
    for part in path.ancestors() {
        if std::fs::symlink_metadata(part).is_ok_and(|m| m.file_type().is_symlink()) {
            return Err("store_selection_linked_lock".into());
        }
    }
    let file = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .mode(0o600)
        .open(path)
        .map_err(|e| format!("store_selection_lock_unavailable: {e}"))?;
    let metadata = file.metadata().map_err(|e| e.to_string())?;
    if !metadata.is_file()
        || metadata.uid() != unsafe { libc::geteuid() }
        || metadata.mode() & 0o022 != 0
    {
        return Err("store_selection_untrusted_lock".into());
    }
    file.try_lock_shared()
        .map_err(|_| "store_version_busy: 模型版本正在发布或清理，请稍后重试")?;
    Ok(file)
}

pub fn publish(app: &Arc<App>, inventory: &serde_json::Value) -> Result<Snapshot, String> {
    let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let mut merged = inventory.clone();
    let incoming = parse(inventory)?;
    let path = app.config.state_dir.join("store-selection.json");
    if std::fs::symlink_metadata(&path).is_ok() {
        let old = cached(app)?;
        let mut archives = merged["archived_models"]
            .as_array()
            .ok_or("invalid_store_archives")?
            .clone();
        for (card, logical) in old
            .archived
            .iter()
            .chain(old.selected.iter().map(|(logical, card)| (card, logical)))
        {
            if incoming.selected.values().any(|current| current == card)
                || archives
                    .iter()
                    .any(|row| row["card"].as_str() == Some(card))
            {
                continue;
            }
            let version = old
                .metadata(card)
                .and_then(|data| data["version_id"].as_str().map(str::to_owned))
                .or_else(|| {
                    old.raw["archived_models"]
                        .as_array()?
                        .iter()
                        .find(|row| row["card"].as_str() == Some(card))?["version_id"]
                        .as_str()
                        .map(str::to_owned)
                })
                .unwrap_or_else(|| "retired".into());
            archives
                .push(serde_json::json!({"card":card,"logical_card":logical,"version_id":version}));
        }
        merged["archived_models"] = archives.into();
    } else if std::fs::symlink_metadata(app.config.state_dir.join("store-selection-v1")).is_ok() {
        return Err("store_selection_missing: 请恢复匹配的选择台账备份，未重新发现旧目录".into());
    }
    let snapshot = parse(&merged)?;
    let raw = serde_json::to_vec(&snapshot.raw).map_err(|e| e.to_string())?;
    if trusted_file(&path, LIMIT).ok().as_deref() != Some(raw.as_slice()) {
        crate::lifecycle_revision::reserve(app, &mut store)
            .map_err(|(_, code, message)| format!("{code}: {message}"))?;
        crate::private_storage::secure_state(&app.config.state_dir)?;
        crate::private_storage::publish_limit(&path, &raw, true, LIMIT)?;
        store.save_if_dirty().map_err(|e| e.to_string())?;
        app.store_adoption_requested
            .store(true, std::sync::atomic::Ordering::Release);
    }
    let marker = app.config.state_dir.join("store-selection-v1");
    match trusted_file(&marker, 64) {
        Ok(bytes) if bytes == b"latest-installed-v1\n" => {}
        Err(_)
            if std::fs::symlink_metadata(&marker)
                .is_err_and(|error| error.kind() == std::io::ErrorKind::NotFound) =>
        {
            crate::private_storage::publish(&marker, b"latest-installed-v1\n", false)?
        }
        _ => return Err("store_selection_invalid_marker".into()),
    }
    Ok(snapshot)
}

/// Reconciliation and admission share the same store writer fence.
pub fn read_inventory(app: &App) -> Result<Option<(File, serde_json::Value)>, String> {
    let Some((program, profile)) = reader(app)? else {
        return Ok(None);
    };
    let lock = resource_lock(&profile)?;
    let value = inventory(app, &program, &profile)?;
    parse(&value)?;
    Ok(Some((lock, value)))
}

pub struct Guard {
    pub card: String,
    pub logical: Option<String>,
    requested: String,
    snapshot: Snapshot,
    _resources: Option<File>,
}

impl Guard {
    pub fn pin_task(&self, app: &App, task: &str) -> Result<(), ApiErr> {
        let mut data = self
            .snapshot
            .metadata(&self.card)
            .unwrap_or_else(|| serde_json::json!({}));
        data["requested_card"] = self.requested.clone().into();
        data["card"] = self.card.clone().into();
        let data = Value::parse(&data.to_string())
            .map_err(|_| api_err(500, "selection_persist_failed", "部署选择编码失败"))?;
        let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let record = store
            .state
            .tasks
            .get_mut(task)
            .ok_or_else(|| api_err(500, "task_missing", "设备任务丢失"))?;
        record.selection = Some(data);
        store.mark_dirty();
        let result = store
            .save_if_dirty()
            .map_err(|error| api_err(500, "selection_persist_failed", &error.to_string()));
        drop(store);
        if let Err((_, _, message)) = &result {
            app.task_finish(task, Err(message.clone()));
            app.save_store();
        }
        result
    }
    pub fn verify(&self, app: &App) -> Result<(), String> {
        if let Some(logical) = &self.logical {
            let now = cached(app)?;
            if now.selected.get(logical) != Some(&self.card)
                || now.entries.get(&self.card) != self.snapshot.entries.get(&self.card)
            {
                return Err("store_selection_changed: 当前版本已变化，请重新查询后重试".into());
            }
            let entry = self
                .snapshot
                .entries
                .get(&self.card)
                .ok_or("current_not_deployed: 当前卡片需要在模型商店重新部署，未启动旧版")?;
            verify_entry(app, entry)?;
        }
        Ok(())
    }

    /// A logical enable performs the old-to-current handoff within its durable task.
    /// A history receipt by itself never grants stop authority.
    pub fn retire_related(&self, app: &Arc<App>, task: &str) -> Result<(), String> {
        let Some(logical) = &self.logical else {
            return Ok(());
        };
        for (old, owner) in &self.snapshot.archived {
            if owner != logical {
                continue;
            }
            let model = app
                .store
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .state
                .models
                .get(old)
                .cloned();
            if let Some(model) = model {
                if !model.lifecycle_owned {
                    if matches!(model.status, ModelStatus::Running | ModelStatus::Starting) {
                        return Err(
                            "archived_runtime_unowned: 历史实例未获停止权，请先核实归属".into()
                        );
                    }
                    continue;
                }
                remember_boot(app, logical, old)?;
                let archived = crate::recovery::view(app, old)
                    .map_err(|e| e.2)?
                    .is_some_and(|r| r.get("phase").and_then(Value::as_str) == Some("archived"));
                if !archived || model.status != ModelStatus::Stopped || model.main_pid.is_some() {
                    app.task_event(
                        task,
                        "archive_old",
                        &format!("隔离历史版本 {old}，只启动商店当前版本"),
                    );
                    crate::ops::stop_archived(app, task, old)?;
                    crate::recovery::archive(app, &model).map_err(|e| e.2)?;
                }
                // An archived version has no start authority until the store
                // explicitly selects it again. Release its stopped reservation.
                let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
                let before = store.state.ports.len();
                store.state.ports.retain(|_, owner| owner != old);
                if store.state.ports.len() != before {
                    store.mark_dirty();
                }
            }
        }
        Ok(())
    }

    pub fn inherit_boot(&self, app: &Arc<App>) -> Result<(), String> {
        if let Some(logical) = &self.logical {
            if let Some(enabled) = boot_preference(app, logical)? {
                crate::recovery::inherit_boot(app, &self.card, enabled).map_err(|e| e.2)?;
            }
        }
        Ok(())
    }

    pub fn disarm_boot(&self, app: &App) -> Result<(), String> {
        if let Some(logical) = &self.logical {
            let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
            save_boot(app, logical, false)?;
        }
        Ok(())
    }
}

pub fn admit(app: &Arc<App>, requested: &str) -> Result<Guard, ApiErr> {
    admit_inner(app, requested, true)
}

pub fn admit_stop(app: &Arc<App>, requested: &str) -> Result<Guard, ApiErr> {
    admit_inner(app, requested, false)
}

/// Installation remains the store's business. A known logical store card must
/// fetch its current package instead of replaying a historical natural folder.
/// Release the reader lock before the receiver takes its publication lock.
pub fn installation_route(
    app: &Arc<App>,
    requested: &str,
    local: bool,
) -> Result<(String, bool), ApiErr> {
    if !safe_id(requested) {
        return Err(api_err(400, "invalid_card", "模型标识无效"));
    }
    let snapshot =
        match read_inventory(app).map_err(|e| api_err(409, "store_selection_unavailable", &e))? {
            Some((_lock, inventory)) => publish(app, &inventory)
                .map_err(|e| api_err(409, "store_selection_unavailable", &e))?,
            None => cached(app).map_err(|e| api_err(409, "store_selection_unavailable", &e))?,
        };
    let logical = snapshot
        .selected
        .get_key_value(requested)
        .map(|(logical, _)| logical)
        .or_else(|| {
            snapshot
                .selected
                .iter()
                .find(|(_, card)| *card == requested)
                .map(|(logical, _)| logical)
        })
        .or_else(|| {
            snapshot
                .archived
                .values()
                .find(|logical| logical.as_str() == requested)
        });
    if let Some(logical) = logical {
        let current = snapshot.selected.get(logical);
        let local_current = current
            .and_then(|card| snapshot.entries.get(card))
            .is_some_and(|entry| entry["origin"] == "local");
        if local_current && current.is_some_and(|card| card != logical) {
            return Err(api_err(
                409,
                "store_local_package_requires_review",
                "请在模型商店处理当前本地安装包，未重放历史目录",
            ));
        }
        return Ok((logical.clone(), local_current && local));
    }
    if snapshot.archived.contains_key(requested) {
        return Err(api_err(
            409,
            "archived_model_version",
            "历史版本已归档，不能重新安装为当前版本",
        ));
    }
    Ok((requested.into(), local))
}

fn admit_inner(app: &Arc<App>, requested: &str, require_ready: bool) -> Result<Guard, ApiErr> {
    if !safe_id(requested) {
        return Err(api_err(400, "invalid_card", "模型标识无效"));
    }
    let failure = |message: String| {
        let prefix = message.split(':').next().unwrap_or("");
        let code = if [
            "current_not_deployed",
            "archived_model_version",
            "store_version_busy",
            "store_selection_upgrade_required",
            "store_selection_changed",
        ]
        .contains(&prefix)
        {
            prefix
        } else {
            "store_selection_unavailable"
        };
        api_err(409, code, &message)
    };
    let (resources, snapshot) = match read_inventory(app).map_err(failure)? {
        Some((lock, value)) => (Some(lock), publish(app, &value).map_err(failure)?),
        None => (None, cached(app).map_err(failure)?),
    };
    if resources.is_none() && !snapshot.selected.is_empty() {
        return Err(failure("商店当前选择未能重新核实，未回落旧目录".into()));
    }
    let (card, logical) = snapshot.resolve(requested).map_err(failure)?;
    let guard = Guard {
        card,
        logical,
        requested: requested.into(),
        snapshot,
        _resources: resources,
    };
    if require_ready {
        guard.verify(app).map_err(failure)?;
    }
    Ok(guard)
}

pub fn ensure_current(app: &App, physical: &str) -> Result<(), String> {
    let snapshot = cached(app)?;
    if !snapshot.routable(physical) {
        return Err("archived_or_unpublished_version: 仅允许商店当前已部署版本，未回落旧版".into());
    }
    if let Some(entry) = snapshot.entries.get(physical) {
        verify_entry(app, entry)?;
    }
    Ok(())
}

/// Only first container creation consumes LMM_MODELS_DIR. A published store
/// deployment pins that value independently of later daemon/store settings.
/// Existing runtimes need no script replay; standalone cards retain config use.
pub(crate) fn creation_models_root(
    app: &App,
    physical: &str,
) -> Result<Option<std::path::PathBuf>, String> {
    let snapshot = cached(app)?;
    if !snapshot.routable(physical) {
        return Err("archived_or_unpublished_version: 未核实商店当前部署，未执行首建脚本".into());
    }
    let Some(entry) = snapshot.entries.get(physical) else {
        return Ok(None);
    };
    verify_entry(app, entry)?;
    let unknown = || {
        "store_models_root_unknown: 当前商店部署没有已核实的下载根目录，请恢复匹配的商店目录记录后重试；未猜测默认目录，也未执行首建脚本".to_owned()
    };
    let root = entry
        .get("models_root")
        .and_then(serde_json::Value::as_str)
        .filter(|root| !root.is_empty() && !root.chars().any(char::is_control))
        .map(std::path::PathBuf::from)
        .filter(|root| root.is_absolute())
        .ok_or_else(unknown)?;
    // New download receipts persist canonical roots. Replaced links, missing
    // paths and permissions are unknown; never create/repair a root here.
    if root.canonicalize().ok().as_ref() != Some(&root)
        || !std::fs::metadata(&root).is_ok_and(|metadata| metadata.is_dir())
    {
        return Err("store_models_root_changed: 商店固定下载根目录缺失、已变化或无法核实；保留原部署，未执行首建脚本".into());
    }
    Ok(Some(root))
}

fn boot_map(app: &App) -> Result<serde_json::Map<String, serde_json::Value>, String> {
    let path = app.config.state_dir.join("logical-model-boot.json");
    let Some(raw) = crate::private_storage::read_control(&path, 128 * 1024)? else {
        return Ok(Default::default());
    };
    let value: serde_json::Value =
        serde_json::from_slice(&raw).map_err(|_| "invalid_logical_boot")?;
    let map = value
        .as_object()
        .filter(|v| v.len() <= 512)
        .ok_or("invalid_logical_boot")?;
    if map
        .iter()
        .any(|(key, value)| !safe_id(key) || !value.is_boolean())
    {
        return Err("invalid_logical_boot".into());
    }
    Ok(map.clone())
}

pub fn boot_preference(app: &App, logical: &str) -> Result<Option<bool>, String> {
    Ok(boot_map(app)?
        .get(logical)
        .and_then(serde_json::Value::as_bool))
}

/// Caller serializes this update through the app store mutex.
pub fn save_boot(app: &App, logical: &str, enabled: bool) -> Result<(), String> {
    let mut map = boot_map(app)?;
    if map.get(logical).and_then(serde_json::Value::as_bool) == Some(enabled) {
        return Ok(());
    }
    map.insert(logical.into(), enabled.into());
    crate::private_storage::publish(
        &app.config.state_dir.join("logical-model-boot.json"),
        &serde_json::to_vec(&map).map_err(|e| e.to_string())?,
        true,
    )
}

/// Internal physical stops during archive migration preserve the logical
/// preference. Explicit stop/switch of a current instance clears that intent.
pub fn disarm_current_boot(app: &App, physical: &str) -> Result<(), String> {
    let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    disarm_current_boot_locked(app, physical)
}

/// Caller already holds App.store, including external-stop reconciliation.
pub fn disarm_current_boot_locked(app: &App, physical: &str) -> Result<(), String> {
    let snapshot = cached(app)?;
    if let Some((logical, _)) = snapshot
        .selected
        .iter()
        .find(|(_, card)| card.as_str() == physical)
    {
        save_boot(app, logical, false)?;
    }
    Ok(())
}

pub fn restore_current_boot(app: &Arc<App>, physical: &str, enabled: bool) -> Result<(), String> {
    let snapshot = cached(app)?;
    if let Some((logical, _)) = snapshot
        .selected
        .iter()
        .find(|(_, card)| card.as_str() == physical)
    {
        {
            let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
            save_boot(app, logical, enabled)?;
        }
        crate::recovery::inherit_boot(app, physical, enabled).map_err(|e| e.2)?;
    }
    Ok(())
}

fn remember_boot(app: &App, logical: &str, old: &str) -> Result<(), String> {
    let _store = app.store.lock().unwrap_or_else(|e| e.into_inner());
    if boot_preference(app, logical)?.is_none() {
        if let Some(record) = crate::recovery::view(app, old).map_err(|e| e.2)? {
            let enabled = record
                .get("archived_previous_boot")
                .and_then(Value::as_bool)
                .unwrap_or_else(|| crate::recovery::boot_enabled(&record));
            save_boot(app, logical, enabled)?;
        }
    }
    Ok(())
}

pub fn fence_archived(app: &Arc<App>, snapshot: &Snapshot) -> Result<(), String> {
    for (card, logical) in &snapshot.archived {
        if app.task_running_for(card).is_some() {
            continue;
        }
        let model = app
            .store
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .state
            .models
            .get(card)
            .filter(|m| m.lifecycle_owned)
            .cloned();
        if let Some(model) = model {
            remember_boot(app, logical, card)?;
            crate::recovery::archive(app, &model).map_err(|e| e.2)?;
        }
    }
    for (logical, card) in &snapshot.selected {
        if let Some(enabled) = boot_preference(app, logical)? {
            crate::recovery::inherit_boot(app, card, enabled).map_err(|e| e.2)?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests;
