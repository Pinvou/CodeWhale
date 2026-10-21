//! Desktop-independent host for the existing store executor. Same Unix API and
//! same card locks for the UI and cmsd; transport failure never starts a local job.
use std::io::{Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::process::{Command, Stdio};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{Value, json};

use super::{config, local_service::LocalService, service};

const MAINTAINED_UNINSTALL: &str = "model.uninstall_maintenance";
const RESERVED_UNINSTALL: &str = "model.uninstall_reservation";

/// One health request on the healthy path. Only an unavailable transport starts
/// recovery; a request whose bytes may have been sent is never replayed here.
pub fn call(method: &str, args: Value) -> Result<Value, String> {
    if method == "health" {
        return ui_call(method, args);
    }
    let mut health = match ui_call("health", json!({})) {
        Ok(health) => health,
        Err(error) if error.starts_with("store_offline:") => {
            ensure_host()?;
            ui_call("health", json!({}))?
        }
        Err(error) => return Err(error),
    };
    if check_health(&health).is_err() {
        if ["active", "log", "cancel", "answer"].contains(&method) {
            let host = controlled_host(&health)?;
            return result(
                request_bound(
                    "/v1/host/task",
                    json!({"host":host,"method":method,"args":args}),
                    Some(&host),
                )?
                .0,
            );
        }
        replace_idle_host(&health)?;
        health = ui_call("health", json!({}))?;
        check_health(&health)?;
    }
    let host = controlled_host(&health)?;
    result(request_bound(&format!("/v1/ui/{method}"), args, Some(&host))?.0)
}
fn result(value: Value) -> Result<Value, String> {
    value
        .get("result")
        .cloned()
        .ok_or("store_protocol: missing result".into())
}
fn ui_call(method: &str, args: Value) -> Result<Value, String> {
    let (value, peer) = request(&format!("/v1/ui/{method}"), args)?;
    let value = result(value)?;
    if method == "health" {
        let host: super::host_control::Identity = serde_json::from_value(value["host"].clone())
            .map_err(
                |_| "store_protocol: host.control.v1 required; unsupported development executor",
            )?;
        host.verify_peer(peer)?;
        if value["version"] != host.version {
            return Err("store_protocol: host version mismatch".into());
        }
    }
    Ok(value)
}
fn request(path: &str, args: Value) -> Result<(Value, u32), String> {
    request_bound(path, args, None)
}
fn request_bound(
    path: &str,
    args: Value,
    expected: Option<&super::host_control::Identity>,
) -> Result<(Value, u32), String> {
    use std::os::fd::AsRawFd;
    let socket = config::data_dir().join("service.sock");
    let mut stream =
        crate::platform::os::local_socket::connect_timeout(&socket, Duration::from_secs(2))
            .map_err(|e| format!("store_offline: {}: {e}", socket.display()))?;
    let mut peer = libc::ucred {
        pid: 0,
        uid: 0,
        gid: 0,
    };
    let mut size = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
    // SAFETY: the connected stream owns a live fd, and both output pointers
    // reference initialized storage whose capacity is supplied in `size`.
    let result = unsafe {
        libc::getsockopt(
            stream.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            (&mut peer as *mut libc::ucred).cast(),
            &mut size,
        )
    };
    // SAFETY: geteuid has no preconditions.
    let uid = unsafe { libc::geteuid() };
    if result != 0
        || size as usize != std::mem::size_of::<libc::ucred>()
        || peer.pid <= 0
        || peer.uid != uid
    {
        return Err("store_protocol: untrusted host peer".into());
    }
    // No request bytes have been sent. A host replaced since health must not
    // receive a business mutation under another executable version/generation.
    if let Some(expected) = expected {
        expected.verify_peer(peer.pid as u32)?;
    }
    stream
        .set_read_timeout(Some(Duration::from_secs(35)))
        .map_err(|e| e.to_string())?;
    stream
        .set_write_timeout(Some(Duration::from_secs(5)))
        .map_err(|e| e.to_string())?;
    let body = serde_json::to_vec(&args).map_err(|e| e.to_string())?;
    write!(
        stream,
        "POST {path} HTTP/1.1\r\nhost: store\r\nconnection: close\r\ncontent-length: {}\r\n\r\n",
        body.len()
    )
    .map_err(|e| e.to_string())?;
    stream.write_all(&body).map_err(|e| e.to_string())?;
    let mut bytes = Vec::new();
    stream
        .take(16 * 1024 * 1024)
        .read_to_end(&mut bytes)
        .map_err(|e| e.to_string())?;
    let text = String::from_utf8(bytes).map_err(|e| e.to_string())?;
    let (_, body) = text
        .split_once("\r\n\r\n")
        .ok_or("store_protocol: missing headers")?;
    let value: Value = serde_json::from_str(body).map_err(|e| e.to_string())?;
    if let Some(error) = value.get("error") {
        return Err(error
            .get("message")
            .and_then(Value::as_str)
            .unwrap_or("store_protocol")
            .into());
    }
    Ok((value, peer.pid as u32))
}
fn controlled_host(health: &Value) -> Result<super::host_control::Identity, String> {
    if health["service"] != "pinvou-model-store"
        || health["protocol"] != 1
        || !health["capabilities"].as_array().is_some_and(|values| {
            [
                super::host_control::CAPABILITY,
                RESERVED_UNINSTALL,
                "model.latest_installed.v1",
            ]
            .iter()
            .all(|required| values.iter().any(|value| value.as_str() == Some(required)))
        })
    {
        return Err(
            "store_protocol: host.control.v1 required; unsupported development executor".into(),
        );
    }
    serde_json::from_value(health["host"].clone())
        .map_err(|_| "store_protocol: invalid host identity".into())
}
pub fn ensure_host() -> Result<(), String> {
    match ui_call("health", json!({})) {
        Ok(health) if check_health(&health).is_ok() => Ok(()),
        Ok(health) => replace_idle_host(&health),
        Err(error) if error.starts_with("store_offline:") => start_host(),
        Err(error) => Err(error),
    }
}
fn replace_idle_host(health: &Value) -> Result<(), String> {
    let old = controlled_host(health)?;
    let current_version = semver::Version::parse(env!("CARGO_PKG_VERSION"))
        .map_err(|_| "store_protocol: invalid client version")?;
    let old_version =
        semver::Version::parse(&old.version).map_err(|_| "store_protocol: invalid host version")?;
    if old_version >= current_version {
        return Err(
            "store_protocol: executor is newer or incompatible; use the matching desktop".into(),
        );
    }
    let (receipt, peer) = request_bound(
        "/v1/host/retire",
        serde_json::to_value(&old).map_err(|e| e.to_string())?,
        Some(&old),
    )
    .map_err(|error| format!("store_upgrade_pending: original tasks retained; {error}"))?;
    if peer != old.pid
        || receipt["retiring"] != true
        || receipt["host"] != serde_json::to_value(&old).map_err(|e| e.to_string())?
    {
        return Err("store_protocol: host retirement unconfirmed".into());
    }
    for _ in 0..50 {
        match ui_call("health", json!({})) {
            Ok(current) if current["host"] != health["host"] => return check_health(&current),
            Err(error) if error.starts_with("store_offline:") => {
                // The listener is removed just before the lifetime lock drops.
                // Wait for that exact exclusion domain instead of racing exec.
                let lock = std::fs::OpenOptions::new()
                    .read(true)
                    .write(true)
                    .open(config::data_dir().join("host.lock"))
                    .map_err(|e| e.to_string())?;
                match lock.try_lock() {
                    Ok(()) => {
                        drop(lock);
                        return start_host();
                    }
                    Err(std::fs::TryLockError::WouldBlock) => {
                        std::thread::sleep(Duration::from_millis(100))
                    }
                    Err(error) => return Err(error.to_string()),
                }
            }
            Err(error) => return Err(error),
            _ => std::thread::sleep(Duration::from_millis(100)),
        }
    }
    Err("store_upgrade_pending: previous host has not released its socket".into())
}
fn start_host() -> Result<(), String> {
    let root = config::data_dir();
    std::fs::create_dir_all(&root).map_err(|e| e.to_string())?;
    let log = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .mode(0o600)
        .open(root.join("host.log"))
        .map_err(|e| e.to_string())?;
    // run_host still owns host.lock for its complete lifetime. A competing or
    // unhealthy live executor prevents this process from becoming a second one.
    let mut child = Command::new("setsid")
        .arg(std::env::current_exe().map_err(|e| e.to_string())?)
        .arg("--model-store-host")
        .stdin(Stdio::null())
        .stdout(log.try_clone().map_err(|e| e.to_string())?)
        .stderr(log)
        .spawn()
        .map_err(|e| e.to_string())?;
    std::thread::spawn(move || {
        let _ = child.wait();
    });
    for _ in 0..50 {
        match ui_call("health", json!({})) {
            Ok(health) => return check_health(&health),
            Err(error) if error.starts_with("store_offline:") => {
                std::thread::sleep(Duration::from_millis(100))
            }
            Err(error) => return Err(error),
        }
    }
    Err("store_offline: 后台模型商店启动失败，请查看 model-store/host.log".into())
}

fn check_health(value: &Value) -> Result<(), String> {
    if value["service"] == "pinvou-model-store"
        && value["protocol"] == 1
        && value["version"] == env!("CARGO_PKG_VERSION")
        && value["capabilities"].as_array().is_some_and(|values| {
            values
                .iter()
                .any(|value| value.as_str() == Some(super::host_control::CAPABILITY))
                && values
                    .iter()
                    .any(|value| value.as_str() == Some(RESERVED_UNINSTALL))
                && values
                    .iter()
                    .any(|value| value.as_str() == Some("model.latest_installed.v1"))
        })
    {
        Ok(())
    } else {
        Err("store_protocol: 后台商店协议不兼容，请先结束现有安装再更新后台服务".into())
    }
}

pub fn run_host() -> Result<(), String> {
    let root = config::data_dir();
    std::fs::create_dir_all(&root).map_err(|e| e.to_string())?;
    let lock = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .mode(0o600)
        .open(root.join("host.lock"))
        .map_err(|e| e.to_string())?;
    // Held for the host lifetime, including cleanup and socket binding. Losing
    // a startup race must not sweep the winning host's active script processes.
    lock.try_lock()
        .map_err(|e| format!("store_host_busy: {e}"))?;
    let service = Arc::new(service::ModelStoreService::new());
    service::run_startup_cleanup(&service);
    service.restore_runs()?;
    service::run_startup_refresh(&service);
    let _listener = LocalService::bind(Arc::clone(&service), root.join("service.sock"), false)
        .map_err(|e| e.to_string())?;
    eprintln!(
        "[model-store] background host ready pid={}",
        std::process::id()
    );
    while !service.host_control.should_exit() {
        std::thread::sleep(Duration::from_millis(50));
    }
    Ok(())
}

pub fn dispatch(
    service: &Arc<service::ModelStoreService>,
    method: &str,
    a: Value,
) -> Result<Value, String> {
    let string = |key: &str| {
        a.get(key)
            .and_then(Value::as_str)
            .map(str::to_string)
            .ok_or_else(|| format!("missing {key}"))
    };
    let optional = |key: &str| a.get(key).and_then(Value::as_str).map(str::to_string);
    match method {
        "health" => Ok(
            json!({"service":"pinvou-model-store", "protocol":1, "pid":std::process::id(), "version":env!("CARGO_PKG_VERSION"),"host":service.host_control.identity,"capabilities":[MAINTAINED_UNINSTALL,RESERVED_UNINSTALL,"model.install_retry.v1","model.latest_installed.v1",super::host_control::CAPABILITY]}),
        ),
        "library" => tauri::async_runtime::block_on(service::library(Arc::clone(service))),
        "incomplete_preview" => {
            tauri::async_runtime::block_on(service::preview_incomplete(service, string("run_id")?))
        }
        "incomplete_retry" => {
            service::retry_incomplete(service, string("run_id")?, string("expected_sha256")?)
        }
        "download" => service::start_download(
            service,
            string("card_id")?,
            a["test_mode"].as_bool().unwrap_or(false),
        ),
        "version_action" => service::start_version_action(
            service,
            string("card_id")?,
            string("action")?,
            string("version_id")?,
        ),
        "card_update" => {
            service::start_card_update(service, string("card_id")?, string("revision")?)
        }
        "deploy" => service::start_deploy(service, string("card_id")?, optional("profile_id")),
        "lifecycle" => service::start_lifecycle(
            service,
            string("card_id")?,
            string("action")?,
            a["purge_weights"].as_bool().unwrap_or(false),
        ),
        "cancel" => service::cancel_run(service, string("run_id")?),
        "answer" => service::answer_run(service, string("run_id")?, optional("answer")),
        "active" => service::active_runs(service, optional("card_id")),
        "cleanup" => service::take_startup_cleanup(service),
        "force_remove" => service::force_remove(
            service,
            string("card_id")?,
            a["confirm"].as_bool().unwrap_or(false),
        ),
        "log" => tauri::async_runtime::block_on(service::run_log_page(
            Arc::clone(service),
            string("run_id")?,
            a["from"].as_u64().unwrap_or(0) as usize,
            a["wait"].as_u64(),
            serde_json::from_value(a.clone()).map_err(|e| format!("invalid_log_page: {e}"))?,
        )),
        _ => Err("store_protocol: unsupported UI operation".into()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn incompatible_host_never_receives_the_requested_operation() {
        let _home = config::TempHome::new();
        let root = config::data_dir();
        let listener = std::os::unix::net::UnixListener::bind(root.join("service.sock")).unwrap();
        let worker = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut bytes = Vec::new();
            let mut byte = [0];
            while !bytes.ends_with(b"\r\n\r\n") {
                stream.read_exact(&mut byte).unwrap();
                bytes.push(byte[0]);
            }
            let response = json!({"result":{"service":"pinvou-model-store","protocol":1,"version":env!("CARGO_PKG_VERSION")}}).to_string();
            write!(
                stream,
                "HTTP/1.1 200 OK\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{}",
                response.len(),
                response
            )
            .unwrap();
            stream.flush().unwrap();
            // Drain the two-byte health request body before close so the peer
            // gets a complete response rather than an ECONNRESET.
            let mut body = [0; 2];
            stream.read_exact(&mut body).unwrap();
            String::from_utf8(bytes).unwrap()
        });
        let error = call("download", json!({"card_id":"must-not-run"})).unwrap_err();
        assert!(error.contains("store_protocol"), "{error}");
        assert!(worker.join().unwrap().starts_with("POST /v1/ui/health "));
    }

    #[test]
    fn host_health_requires_maintenance_capability_even_at_the_same_version() {
        let mut health = json!({"service":"pinvou-model-store","protocol":1,"version":env!("CARGO_PKG_VERSION")});
        assert!(check_health(&health).is_err());
        health["capabilities"] = json!([MAINTAINED_UNINSTALL]);
        assert!(check_health(&health).is_err());
        health["capabilities"] = json!([MAINTAINED_UNINSTALL, RESERVED_UNINSTALL]);
        assert!(check_health(&health).is_err());
        health["capabilities"] = json!([
            MAINTAINED_UNINSTALL,
            RESERVED_UNINSTALL,
            "model.latest_installed.v1",
            super::super::host_control::CAPABILITY
        ]);
        assert!(check_health(&health).is_ok());
        health["version"] = json!("older-build");
        assert!(check_health(&health).is_err());
    }
}
