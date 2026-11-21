//! Automatic ownership handoff from completed Pinvou store installations.
//! Store's read-only SQLite helper is independent of the UI and store host
//! versions. No shell scripts, downloads, model start or default switch run.
pub(crate) mod runtime;
pub(crate) mod selection;
pub use runtime::{native_state, stop_native, try_running_native};

use crate::app::{log_line, App};
use crate::cards::{Card, EngineKind};
use crate::json::Value;
use crate::store::ModelStatus;
use std::io::Read;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::Path;
use std::sync::Arc;

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
fn safe_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && !matches!(value, "." | "..")
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
}
fn trusted_file(path: &Path, max: u64) -> Result<Vec<u8>, String> {
    for ancestor in path.ancestors() {
        if std::fs::symlink_metadata(ancestor).is_ok_and(|m| m.file_type().is_symlink()) {
            return Err("linked_store_record".into());
        }
    }
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|e| e.to_string())?;
    let meta = file.metadata().map_err(|e| e.to_string())?;
    if !meta.is_file()
        || meta.uid() != unsafe { libc::geteuid() }
        || (meta.mode() & 0o002 != 0
            || (meta.mode() & 0o020 != 0 && meta.gid() != unsafe { libc::getegid() }))
        || meta.len() > max
    {
        return Err("untrusted_store_record".into());
    }
    let mut bytes = Vec::new();
    Read::by_ref(&mut file)
        .take(max + 1)
        .read_to_end(&mut bytes)
        .map_err(|e| e.to_string())?;
    if bytes.len() as u64 > max {
        return Err("store_record_too_large".into());
    }
    Ok(bytes)
}
fn reader(app: &App) -> Result<Option<(std::path::PathBuf, std::path::PathBuf)>, String> {
    match (
        &app.config.store_inventory_program,
        &app.config.store_inventory_profile,
    ) {
        (Some(program), Some(profile)) => return Ok(Some((program.clone(), profile.clone()))),
        (None, None) => {}
        _ => return Err("invalid_store_inventory_configuration".into()),
    }
    let Some(raw) =
        crate::private_storage::read_limit(&app.config.state_dir.join("store-reader.json"), 8192)?
    else {
        return Ok(None);
    };
    let v: serde_json::Value = serde_json::from_slice(&raw).map_err(|_| "invalid_store_reader")?;
    if v["format"] != "pinvou-store-reader-v1" {
        return Err("invalid_store_reader".into());
    }
    Ok(Some((
        v["program"].as_str().ok_or("invalid_store_reader")?.into(),
        v["profile"].as_str().ok_or("invalid_store_reader")?.into(),
    )))
}
fn inventory(app: &App, program: &Path, profile: &Path) -> Result<serde_json::Value, String> {
    if !program.is_absolute() || !profile.is_absolute() {
        return Err("invalid_store_inventory_configuration".into());
    }
    let metadata = std::fs::symlink_metadata(program).map_err(|e| e.to_string())?;
    let uid = unsafe { libc::geteuid() };
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || ![0, uid].contains(&metadata.uid())
        || (metadata.mode() & 0o002 != 0
            || (metadata.mode() & 0o020 != 0 && metadata.gid() != unsafe { libc::getegid() }))
    {
        return Err("untrusted_store_inventory_program".into());
    }
    let output = crate::process::output(
        std::process::Command::new(program)
            .arg("--cmsd-store-inventory")
            .env("PINVOU3_HOME", profile),
        std::time::Duration::from_secs(8),
    )
    .map_err(|e| e.to_string())?;
    if !output.status.success() {
        return Err(format!(
            "store_inventory_failed: {}",
            String::from_utf8_lossy(&output.stderr)
                .chars()
                .take(512)
                .collect::<String>()
        ));
    }
    if output.stdout.len() > 1024 * 1024 {
        return Err("store_inventory_limit_exceeded".into());
    }
    let value: serde_json::Value =
        serde_json::from_slice(&output.stdout).map_err(|_| "invalid_store_inventory_json")?;
    if value["format"] != "pinvou-installed-models-v1"
        || value["cards_root"].as_str().map(Path::new) != Some(app.config.cards_dir.as_path())
        || value["models"].as_array().is_none_or(|v| v.len() > 512)
        || value["skipped"].as_array().is_none_or(|v| v.len() > 512)
    {
        return Err("store_inventory_binding_mismatch".into());
    }
    Ok(value)
}
fn verify_entry(app: &App, entry: &serde_json::Value) -> Result<Card, String> {
    entry["logical_card"]
        .as_str()
        .filter(|v| safe_id(v))
        .ok_or("invalid_store_logical_card")?;
    let card = entry["card"]
        .as_str()
        .filter(|v| safe_id(v))
        .ok_or("invalid_store_card")?;
    if !matches!(entry["origin"].as_str(), Some("store" | "local"))
        || !matches!(
            entry["store_status"].as_str(),
            Some("downloaded" | "pending" | "deployed")
        )
        || entry["downloaded_at"]
            .as_str()
            .is_none_or(|v| v.trim().is_empty())
    {
        return Err("not_completed_store_installation".into());
    }
    let files = entry["files"]
        .as_object()
        .ok_or("invalid_store_card_files")?;
    if (!files.contains_key("common.sh") && !files.contains_key("install_engine.sh"))
        || files.len() > 1041
    {
        return Err("invalid_store_card_files".into());
    }
    for (name, hash) in files {
        // The authoritative recipe includes bounded source assets as well as
        // launch/deploy scripts. Hash each safe relative path, never execute it.
        if name.is_empty()
            || name.len() > 1024
            || name.chars().any(|c| c.is_control() || c == '\\')
            || name
                .split('/')
                .any(|part| part.is_empty() || matches!(part, "." | ".."))
            || !Path::new(name)
                .components()
                .all(|part| matches!(part, std::path::Component::Normal(_)))
        {
            return Err("invalid_store_card_file".into());
        }
        let bytes = trusted_file(&app.config.cards_dir.join(card).join(name), 256 * 1024)?;
        if hash.as_str()
            != Some(hex(ring::digest::digest(&ring::digest::SHA256, &bytes).as_ref()).as_str())
        {
            return Err("store_card_changed_since_inventory".into());
        }
    }
    crate::ops::find_card_raw(app, card).map_err(|(_, code, _)| code)
}

pub fn reconcile(app: &Arc<App>) {
    let result = match selection::read_inventory(app) {
        Ok(None) => return,
        Ok(Some((resources, value))) => {
            selection::publish(app, &value).map(|snapshot| (resources, value, snapshot))
        }
        Err(error) => Err(error),
    };
    let mut status = serde_json::json!({"phase":"ready","observed_at":crate::timeutil::now_epoch(),"imported":[],"skipped":[]});
    match result {
        Err(error) => {
            status["phase"] = "unavailable".into();
            status["error"] = error.into();
        }
        Ok((_resources, inventory, snapshot)) => {
            status["skipped"] = inventory["skipped"].clone();
            let mut bindings = serde_json::Map::new();
            for entry in inventory["models"].as_array().into_iter().flatten() {
                let enrolled = verify_entry(app, entry).and_then(|card| enroll(app, &card, entry));
                if enrolled.is_ok() {
                    bindings.insert(
                        entry["logical_card"].as_str().unwrap().into(),
                        entry["card"].clone(),
                    );
                }
                match enrolled {
                    Ok(true) => status["imported"].as_array_mut().unwrap().push(entry["card"].clone()),
                    Ok(false) => {},
                    Err(reason) => status["skipped"].as_array_mut().unwrap().push(serde_json::json!({"card":entry["logical_card"],"runtime_card":entry["card"],"reason":reason})),
                }
            }
            let file = app.config.state_dir.join("store-bindings.json");
            let raw = serde_json::to_vec(&bindings).unwrap();
            if !bindings.is_empty() {
                if let Err(error) = crate::private_storage::secure_state(&app.config.state_dir)
                    .and_then(|_| {
                        crate::private_storage::publish(
                            &app.config.state_dir.join("store-adoption-v1"),
                            b"cmsd-store-adoption-v1\n",
                            true,
                        )
                    })
                {
                    status["phase"] = "unavailable".into();
                    status["error"] = error.into();
                    *app.store_adoption_status
                        .lock()
                        .unwrap_or_else(|e| e.into_inner()) =
                        Value::parse(&status.to_string()).unwrap();
                    return;
                }
            }
            // r18 #9：与 selection.rs 的孪生写方同款字节比较守卫——30s
            // reconcile 对未变化的内容不重写（原为无条件 temp+双 fsync+
            // rename，2,880 writes/日怠速）。
            if trusted_file(&file, 1024 * 1024).ok().as_deref() != Some(raw.as_slice()) {
                if let Err(error) = crate::private_storage::secure_state(&app.config.state_dir)
                    .and_then(|_| crate::private_storage::publish(&file, &raw, true))
                {
                    status["phase"] = "unavailable".into();
                    status["error"] = error.into();
                }
            }
            if let Err(error) = selection::fence_archived(app, &snapshot) {
                status["phase"] = "unavailable".into();
                status["error"] = error.into();
            }
        }
    }
    if let Ok(value) = Value::parse(&status.to_string()) {
        *app.store_adoption_status
            .lock()
            .unwrap_or_else(|e| e.into_inner()) = value;
    }
}

fn enroll(app: &Arc<App>, card: &Card, entry: &serde_json::Value) -> Result<bool, String> {
    if app
        .store
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .state
        .models
        .get(&card.name)
        .is_some_and(|m| m.lifecycle_owned)
    {
        return Ok(false);
    }
    // Same admission and maintenance lock as interactive changes. Repeated
    // scans never create tasks or rewrite policy for already managed records.
    let task = app
        .task_create_exclusive("adopt-store", &card.name)
        .map_err(|(_, code, _)| code)?;
    let result = (|| -> Result<String, String> {
        let current = verify_entry(app, entry)?;
        let card = &current;
        if (card.deploy.is_some() && !card.weights_complete())
            || card.docker_weights_complete() == Some(false)
        {
            return Err("store_weights_incomplete".into());
        }
        let native = if card.engine_kind == EngineKind::Native {
            runtime::discover(app, card)?
        } else {
            None
        };
        let container = if card.engine_kind == EngineKind::Container {
            match card.container_name.as_deref() {
                Some(name) => app.docker.inspect(name)?,
                None => None,
            }
        } else {
            None
        };
        if let Some(c) = &container {
            crate::container_runtime::check(card, None, c)
                .map_err(|_| "store_container_binding_mismatch".to_owned())?;
        }
        if card.engine_kind == EngineKind::Native
            && card.deploy.is_some()
            && app.systemd.write_template().map_err(|e| e.to_string())?
        {
            app.systemd.daemon_reload()?;
        }
        if let Some(binding) = &native {
            runtime::save(app, &card.name, binding)?;
        }
        crate::private_storage::secure_state(&app.config.state_dir)?;
        crate::private_storage::publish(
            &app.config.state_dir.join("store-adoption-v1"),
            b"cmsd-store-adoption-v1\n",
            true,
        )?;
        let running = native.as_ref().is_some_and(|n| n.running)
            || container.as_ref().is_some_and(|c| c.running);
        let boot = native.as_ref().is_some_and(|n| n.boot)
            || container.as_ref().is_some_and(|c| c.is_autostart());
        crate::ops::upsert_model_state(app, &card.name, |model| {
            model.status = if running {
                ModelStatus::Running
            } else {
                ModelStatus::Stopped
            };
            model.main_pid = native
                .as_ref()
                .map(|n| n.pid)
                .or_else(|| container.as_ref().filter(|c| c.running).map(|c| c.pid));
            if let Some(deploy) = &card.docker_deploy {
                // A proven published port may be any matching binding, not the
                // first observation saved by earlier read-only discovery.
                model.port = deploy.port;
            }
            if let Some(c) = &container {
                model.container_name = Some(c.name.clone());
                model.image = Some(c.image.clone());
            }
        });
        let model = app
            .store
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .state
            .models[&card.name]
            .clone();
        crate::recovery::adopt_store(app, &model, running, boot)
            .map_err(|(_, code, message)| format!("{code}: {message}"))?;
        let source = serde_json::to_vec(entry).map_err(|e| e.to_string())?;
        let source_dir = app.config.state_dir.join("store-installed");
        crate::private_storage::directory(&source_dir, true)?;
        crate::private_storage::publish_limit(
            &source_dir.join(format!("{}.json", card.name)),
            &source,
            true,
            1024 * 1024,
        )?;
        crate::ops::upsert_model_state(app, &card.name, |model| model.lifecycle_owned = true);
        if let Some(binding) = native.as_ref().filter(|n| n.pid > 0) {
            let mut store = app.store.lock().unwrap_or_else(|e| e.into_inner());
            store
                .state
                .externals
                .retain(|_, sighting| sighting.pid != binding.pid);
            store.mark_dirty();
        }
        Ok("Model-store installation enrolled; runtime and default selection preserved".into())
    })();
    app.task_finish(&task, result.clone());
    app.save_store();
    result.map(|_| {
        log_line(&format!("商店模型自动纳管：{}", card.name));
        true
    })
}

/// Aliases are receipts from the current store inventory, never guessed from
/// directory names. Unknown/corrupt bindings fail closed at the client.
pub fn bindings(app: &App) -> Value {
    let result = (|| -> Result<Value, String> {
        let Some(raw) = crate::private_storage::read_limit(
            &app.config.state_dir.join("store-bindings.json"),
            128 * 1024,
        )?
        else {
            return Ok(Value::obj());
        };
        let parsed: serde_json::Value =
            serde_json::from_slice(&raw).map_err(|_| "invalid_store_bindings")?;
        let map = parsed
            .as_object()
            .filter(|m| m.len() <= 512)
            .ok_or("invalid_store_bindings")?;
        let mut out = Value::obj();
        let store = app.store.lock().unwrap_or_else(|e| e.into_inner());
        for (logical, runtime) in map {
            let runtime = runtime
                .as_str()
                .filter(|v| safe_id(v))
                .ok_or("invalid_store_bindings")?;
            if !safe_id(logical) {
                return Err("invalid_store_bindings".into());
            }
            if store
                .state
                .models
                .get(runtime)
                .is_some_and(|m| m.lifecycle_owned)
            {
                out.set(logical, runtime.into());
            }
        }
        Ok(out)
    })();
    result.unwrap_or(Value::Null)
}

#[cfg(test)]
mod tests;

/// A same-account desktop may register its read-only reader with independently
/// installed user cmsd. The cards directory must already be bound to this
/// profile; this endpoint cannot redirect a daemon to another user's store.
pub fn register_reader(
    app: &App,
    body: &[u8],
    caller: crate::peer::LocalPeer,
) -> Result<Value, crate::app::ApiErr> {
    use crate::app::api_err;
    if caller.uid != unsafe { libc::geteuid() } || unsafe { libc::geteuid() } == 0 {
        return Err(api_err(
            403,
            "store_reader_account_mismatch",
            "仅同账号的用户服务可登记商店读取器",
        ));
    }
    let value: serde_json::Value = serde_json::from_slice(body)
        .map_err(|_| api_err(400, "invalid_store_reader", "读取器配置不合法"))?;
    let program = value["program"]
        .as_str()
        .map(std::path::PathBuf::from)
        .ok_or_else(|| api_err(400, "invalid_store_reader", "缺少读取程序"))?;
    let profile = value["profile"]
        .as_str()
        .map(std::path::PathBuf::from)
        .ok_or_else(|| api_err(400, "invalid_store_reader", "缺少商店账号目录"))?;
    let check = (|| -> Result<(), String> {
        if !program.is_absolute()
            || !profile.is_absolute()
            || profile.join("model-store/cards") != app.config.cards_dir
        {
            return Err("store_reader_profile_mismatch".into());
        }
        let metadata = std::fs::symlink_metadata(&program).map_err(|e| e.to_string())?;
        if !metadata.is_file()
            || metadata.file_type().is_symlink()
            || ![0, caller.uid].contains(&metadata.uid())
            || metadata.mode() & 0o002 != 0
            || (metadata.mode() & 0o020 != 0 && metadata.gid() != unsafe { libc::getegid() })
        {
            return Err("untrusted_store_inventory_program".into());
        }
        if app.config.store_inventory_program.is_some() {
            return Err("explicit_store_reader_configuration".into());
        }
        Ok(())
    })();
    check.map_err(|e| api_err(409, &e, "未更改既有商店绑定"))?;
    let _guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    crate::component::check_admission(app)?;
    let file = app.config.state_dir.join("store-reader.json");
    let desired =
        serde_json::json!({"format":"pinvou-store-reader-v1","program":program,"profile":profile});
    let existing = crate::private_storage::read_limit(&file, 8192)
        .map_err(|e| api_err(409, "invalid_store_reader", &e))?;
    if existing
        .as_ref()
        .and_then(|raw| serde_json::from_slice::<serde_json::Value>(raw).ok())
        .as_ref()
        != Some(&desired)
    {
        crate::private_storage::secure_state(&app.config.state_dir)
            .and_then(|_| {
                crate::private_storage::publish(&file, desired.to_string().as_bytes(), true)
            })
            .map_err(|e| api_err(500, "store_reader_persist_failed", &e))?;
    }
    // An idempotent registration may be recovering the first missing snapshot.
    // Wake the reader even when its binding bytes are already unchanged.
    app.store_adoption_requested
        .store(true, std::sync::atomic::Ordering::Release);
    let mut response = Value::obj();
    response.set("registered", true.into());
    Ok(response)
}

pub fn reader_view(app: &App) -> Value {
    match reader(app) {
        Ok(Some((program, profile))) => {
            let mut value = Value::obj();
            value.set("program", program.to_string_lossy().into_owned().into());
            value.set("profile", profile.to_string_lossy().into_owned().into());
            value
        }
        _ => Value::Null,
    }
}
