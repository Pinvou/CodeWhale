//! Durable component-maintenance gate. Setting the gate and accepting model
//! tasks share the store mutex; a check-then-stop race cannot admit a switch.
use crate::app::{api_err, ApiErr, App};
use crate::json::Value;
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;

pub fn check_admission(app: &App) -> Result<(), ApiErr> {
    match std::fs::symlink_metadata(app.config.state_dir.join("component-maintenance.json")) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        _ => Err(api_err(
            409,
            "component_maintenance",
            "cmsd 正在维护，暂不接受新的模型变更；请在品悟的设备服务入口恢复维护状态",
        )),
    }
}

pub fn change(app: &App, body: &[u8]) -> Result<Value, ApiErr> {
    let text =
        std::str::from_utf8(body).map_err(|_| api_err(400, "invalid_json", "维护请求格式错误"))?;
    let request =
        Value::parse(text).map_err(|_| api_err(400, "invalid_json", "维护请求格式错误"))?;
    let owner = request.get("owner").and_then(Value::as_str).unwrap_or("");
    if owner.is_empty()
        || owner.len() > 128
        || !owner
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b))
    {
        return Err(api_err(
            400,
            "invalid_maintenance_owner",
            "维护操作标识不合法",
        ));
    }
    let action = request.get("action").and_then(Value::as_str).unwrap_or("");
    if !["begin", "end"].contains(&action) {
        return Err(api_err(400, "invalid_action", "维护动作不支持"));
    }
    let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    change_locked(app, &guard, owner, action)
}

/// Called while the model-admission Store mutex is held.
pub(crate) fn change_locked(
    app: &App,
    guard: &crate::store::StateStore,
    owner: &str,
    action: &str,
) -> Result<Value, ApiErr> {
    let path = app.config.state_dir.join("component-maintenance.json");
    let existing = crate::private_storage::read_control(&path, 16 * 1024)
        .map_err(|message| api_err(409, "maintenance_read_failed", &message))?
        .map(|bytes| {
            std::str::from_utf8(&bytes)
                .ok()
                .and_then(|text| Value::parse(text).ok())
                .ok_or_else(|| {
                    api_err(
                        409,
                        "maintenance_corrupt",
                        "维护记录损坏，请从组件恢复入口核查",
                    )
                })
        })
        .transpose()?;
    if existing
        .as_ref()
        .is_some_and(|v| v.get("owner").and_then(Value::as_str) != Some(owner))
    {
        return Err(api_err(
            409,
            "maintenance_owned",
            "其他组件维护操作尚未完成，请先恢复原操作",
        ));
    }
    if action == "begin" {
        if crate::model_maintenance::any_active(app)? {
            return Err(api_err(
                409,
                "model_maintenance",
                "模型卸载或恢复事务未结束，暂不能维护cmsd组件",
            ));
        }
        if let Some(task) = guard
            .state
            .tasks
            .values()
            .find(|task| !task.status.is_terminal())
        {
            return Err(api_err(
                409,
                "task_in_progress",
                &format!(
                    "cmsd 任务 {}（{}）仍在进行，暂不能维护服务",
                    task.id, task.kind
                ),
            ));
        }
        if existing.is_none() {
            let mut value = Value::obj();
            value.set("owner", owner.into());
            value.set("created_at", crate::timeutil::now_epoch().into());
            let stamp = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos();
            let temp = app
                .config
                .state_dir
                .join(format!(".component-maintenance-{stamp}.tmp"));
            let result = (|| -> std::io::Result<()> {
                let mut file = std::fs::OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .mode(0o600)
                    .open(&temp)?;
                file.write_all(value.to_string().as_bytes())?;
                file.sync_all()?;
                std::fs::rename(&temp, &path)?;
                std::fs::File::open(&app.config.state_dir)?.sync_all()
            })();
            if let Err(e) = result {
                return Err(api_err(500, "maintenance_persist_failed", &e.to_string()));
            }
        }
    } else if existing.is_some() {
        std::fs::remove_file(&path)
            .map_err(|e| api_err(500, "maintenance_release_failed", &e.to_string()))?;
        std::fs::File::open(&app.config.state_dir)
            .and_then(|dir| dir.sync_all())
            .map_err(|e| api_err(500, "maintenance_release_failed", &e.to_string()))?;
    }
    let mut out = Value::obj();
    out.set("maintenance", (action == "begin").into());
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::docker::fixtures as fx;

    fn request(owner: &str, action: &str) -> Vec<u8> {
        format!("{{\"owner\":\"{owner}\",\"action\":\"{action}\"}}").into_bytes()
    }

    #[test]
    fn damaged_receipts_return_without_holding_store_or_releasing_the_gate() {
        use std::os::unix::fs::{MetadataExt, PermissionsExt};
        for kind in [
            "corrupt",
            "dangling",
            "link",
            "fifo",
            "oversized",
            "directory",
            "writable",
        ] {
            let root = fx::tmpdir(&format!("component-receipt-{kind}"));
            let app = fx::make_app(&root, 128, 100);
            crate::private_storage::secure_state(&app.config.state_dir).unwrap();
            let path = app.config.state_dir.join("component-maintenance.json");
            let outside = root.join("outside");
            std::fs::write(&outside, b"preserve").unwrap();
            match kind {
                "dangling" => std::os::unix::fs::symlink(root.join("missing"), &path).unwrap(),
                "link" => std::os::unix::fs::symlink(&outside, &path).unwrap(),
                "fifo" => {
                    use std::os::unix::ffi::OsStrExt;
                    let name = std::ffi::CString::new(path.as_os_str().as_bytes()).unwrap();
                    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
                }
                "oversized" => std::fs::write(&path, vec![b'x'; 16 * 1024 + 1]).unwrap(),
                "directory" => std::fs::create_dir(&path).unwrap(),
                "writable" => {
                    std::fs::write(&path, b"{\"owner\":\"op-a\"}").unwrap();
                    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o666))
                        .unwrap();
                }
                _ => std::fs::write(&path, b"corrupt").unwrap(),
            }
            let inode = std::fs::symlink_metadata(&path).unwrap().ino();
            for action in ["begin", "end"] {
                let (sender, receiver) = std::sync::mpsc::channel();
                let app2 = app.clone();
                std::thread::spawn(move || {
                    sender
                        .send(change(&app2, &request("op-a", action)))
                        .unwrap();
                });
                let result = receiver
                    .recv_timeout(std::time::Duration::from_secs(1))
                    .expect("control receipt I/O must be bounded");
                assert!(result.is_err(), "{kind}/{action}");
                assert!(app.store.try_lock().is_ok(), "{kind}/{action}");
                assert!(check_admission(&app).is_err(), "{kind}/{action}");
                assert_eq!(std::fs::symlink_metadata(&path).unwrap().ino(), inode);
            }
            assert_eq!(std::fs::read(outside).unwrap(), b"preserve");
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn absent_end_is_idempotent_and_wrong_owner_cannot_release_normal_receipts() {
        let root = fx::tmpdir("component-receipt-normal");
        let app = fx::make_app(&root, 128, 100);
        crate::private_storage::secure_state(&app.config.state_dir).unwrap();
        change(&app, &request("op-a", "end")).unwrap();
        change(&app, &request("op-a", "begin")).unwrap();
        change(&app, &request("op-a", "begin")).unwrap();
        assert_eq!(
            change(&app, &request("op-b", "end")).unwrap_err().1,
            "maintenance_owned"
        );
        assert!(check_admission(&app).is_err());
        change(&app, &request("op-a", "end")).unwrap();
        change(&app, &request("op-a", "end")).unwrap();
        assert!(check_admission(&app).is_ok());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn maintenance_blocks_all_admission_and_survives_reload() {
        let root = fx::tmpdir("component-maintenance");
        let app = fx::make_app(&root, 128, 100);
        std::fs::create_dir_all(&app.config.state_dir).unwrap();
        change(&app, &request("op-a", "begin")).unwrap();
        assert_eq!(
            app.task_create_exclusive("switch", "card-a").unwrap_err().1,
            "component_maintenance"
        );
        assert_eq!(
            app.task_create_install("card-a").unwrap_err().1,
            "component_maintenance"
        );
        let reloaded = fx::make_app(&root, 128, 100);
        assert!(check_admission(&reloaded).is_err());
        assert_eq!(
            change(&reloaded, &request("op-b", "end")).unwrap_err().1,
            "maintenance_owned"
        );
        change(&reloaded, &request("op-a", "end")).unwrap();
        assert!(reloaded.task_create_exclusive("enable", "card-a").is_ok());
    }

    #[test]
    fn accepted_switch_prevents_maintenance_and_owner_retry_is_idempotent() {
        let root = fx::tmpdir("component-maintenance-task");
        let app = fx::make_app(&root, 128, 100);
        let task = app.task_create_exclusive("switch", "card-a").unwrap();
        assert_eq!(
            change(&app, &request("op-a", "begin")).unwrap_err().1,
            "task_in_progress"
        );
        assert!(check_admission(&app).is_ok());
        app.task_finish(&task, Ok("done".into()));
        change(&app, &request("op-a", "begin")).unwrap();
        change(&app, &request("op-a", "begin")).unwrap();
    }
}
