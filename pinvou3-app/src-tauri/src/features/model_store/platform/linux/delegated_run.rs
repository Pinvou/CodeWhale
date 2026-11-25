//! Immutable UI hand-off receipt for an accepted cmsd parent whose store run
//! may not exist yet. This is a lookup binding, not another task scheduler.
use std::collections::BTreeMap;
use std::fs::OpenOptions;
use std::io::Read;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::PathBuf;

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::{config, execution_fence};
use crate::features::megacube::Client;

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Binding {
    task_id: String,
    run_id: String,
    card_id: String,
    store_kind: String,
    device_id: String,
    socket: PathBuf,
}

fn valid(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 160
        && id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"_.-".contains(&byte))
        && id != "."
        && id != ".."
}

fn path(run_id: &str) -> Result<PathBuf, String> {
    if !valid(run_id) {
        return Err("invalid_installation_reference".into());
    }
    Ok(config::data_dir()
        .join("delegated-runs")
        .join(format!("{run_id}.json")))
}

pub(super) fn save(
    client: &Client,
    task_id: &str,
    card_id: &str,
    store_kind: &str,
    device_id: &str,
) -> Result<(), String> {
    if !valid(task_id)
        || !valid(card_id)
        || !valid(device_id)
        || !["download", "install_local"].contains(&store_kind)
    {
        return Err("installation_admission_unconfirmed".into());
    }
    let run_id = format!("cmsd-{task_id}");
    let binding = Binding {
        task_id: task_id.into(),
        run_id: run_id.clone(),
        card_id: card_id.into(),
        store_kind: store_kind.into(),
        device_id: device_id.into(),
        socket: client
            .bound_socket()
            .canonicalize()
            .map_err(|_| "installation_admission_unconfirmed")?,
    };
    execution_fence::private_write(
        &path(&run_id)?,
        &serde_json::to_vec(&binding).map_err(|error| error.to_string())?,
    )
}

pub(super) struct Parent {
    pub client: Client,
    pub task_id: String,
    pub task: Value,
}

fn matches(binding: &Binding, task: &Value, device: &Value) -> bool {
    device["device_id"] == binding.device_id
        && task["id"] == binding.task_id
        && task["kind"] == "install"
        && task["card"] == binding.card_id
        && task["store_run_id"] == binding.run_id
}

fn binding(run_id: &str) -> Result<Option<Binding>, String> {
    let mut file = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path(run_id)?)
    {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err("installation_admission_unconfirmed".into()),
    };
    let metadata = file
        .metadata()
        .map_err(|_| "installation_admission_unconfirmed")?;
    // SAFETY: geteuid has no preconditions.
    let uid = unsafe { libc::geteuid() };
    if !metadata.is_file()
        || metadata.uid() != uid
        || metadata.mode() & 0o077 != 0
        || metadata.nlink() != 1
        || metadata.len() > 8192
    {
        return Err("installation_admission_unconfirmed".into());
    }
    let mut bytes = Vec::new();
    file.by_ref()
        .take(8193)
        .read_to_end(&mut bytes)
        .map_err(|_| "installation_admission_unconfirmed")?;
    if bytes.len() > 8192 {
        return Err("installation_admission_unconfirmed".into());
    }
    let binding: Binding =
        serde_json::from_slice(&bytes).map_err(|_| "installation_admission_unconfirmed")?;
    if binding.run_id != run_id
        || !valid(&binding.task_id)
        || !valid(&binding.card_id)
        || !valid(&binding.device_id)
        || !["download", "install_local"].contains(&binding.store_kind.as_str())
    {
        return Err("installation_admission_unconfirmed".into());
    }
    Ok(Some(binding))
}

pub(super) fn parent(run_id: &str) -> Result<Option<Parent>, String> {
    let Some(binding) = binding(run_id)? else {
        return Ok(None);
    };
    let client = Client::at_socket(binding.socket.clone())?;
    let device = client.device()?;
    let task = client.task(&binding.task_id)?;
    if !matches(&binding, &task, &device) {
        return Err(format!(
            "installation_admission_unconfirmed: cmsd_task_id={}",
            binding.task_id
        ));
    }
    Ok(Some(Parent {
        client,
        task_id: binding.task_id,
        task,
    }))
}

pub(super) fn known(run_id: &str) -> bool {
    matches!(binding(run_id), Ok(Some(_)))
}

pub(super) fn unconfirmed(from: usize) -> Value {
    // An observation failure cannot establish the outcome of an accepted job.
    // Keep the real cursor unchanged until the receiver log is available.
    json!({"lines":[],"next":from,"done":false,"ok":false,"cancelled":false,
        "awaiting_input":false,"result":null,"error":null,"backend_pending":false,
        "unconfirmed":true})
}

type DeviceTasks = Result<(Value, Vec<Value>), String>;

fn device_tasks(socket: &std::path::Path) -> DeviceTasks {
    let client = Client::at_socket(socket.to_path_buf())?;
    let device = client.device()?;
    let response = client.tasks()?;
    let tasks = response["tasks"]
        .as_array()
        .ok_or("installation_admission_unconfirmed")?
        .clone();
    Ok((device, tasks))
}

/// Reattach accepted hand-offs after the UI reopens before the store receiver
/// has created its run. Unknown queries retain the accepted reference; they do
/// not assert the installer is running or grant cancellation without recheck.
pub(super) fn pending(card_id: Option<&str>) -> (Vec<Value>, Option<String>) {
    let directory = config::data_dir().join("delegated-runs");
    let entries = match std::fs::read_dir(directory) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return (Vec::new(), None),
        Err(_) => {
            return (
                Vec::new(),
                Some("installation_admission_unconfirmed".into()),
            );
        }
    };
    let mut values = Vec::new();
    let mut error = None;
    // This cache lives only for this read. Reopening many receipts on one
    // unavailable device must not multiply its network timeout per receipt.
    // Mutations and log queries still use parent() and fresh task details.
    let mut snapshots: BTreeMap<PathBuf, DeviceTasks> = BTreeMap::new();
    for entry in entries {
        let Ok(entry) = entry else {
            error = Some("installation_admission_unconfirmed".into());
            continue;
        };
        let path = entry.path();
        if path.extension().and_then(|extension| extension.to_str()) != Some("json") {
            continue;
        }
        let Some(run_id) = path.file_stem().and_then(|name| name.to_str()) else {
            continue;
        };
        let record = match binding(run_id) {
            Ok(Some(record)) => record,
            Ok(None) => continue,
            Err(reason) => {
                error = Some(reason);
                continue;
            }
        };
        if card_id.is_some_and(|card| card != record.card_id)
            || config::data_dir()
                .join("runs")
                .join(format!("{run_id}.json"))
                .exists()
        {
            continue;
        }
        // Avoid unbounded historical device fanout in a single UI query.
        // Other receipts remain explicitly unconfirmed.
        if !snapshots.contains_key(&record.socket) && snapshots.len() < 2 {
            snapshots.insert(record.socket.clone(), device_tasks(&record.socket));
        }
        let observed = match snapshots.get(&record.socket) {
            Some(Ok((device, tasks))) => tasks.iter().find(|task| matches(&record, task, device)),
            _ => None,
        };
        let unconfirmed = match observed.and_then(|task| task["status"].as_str()) {
            Some("running" | "queued") => false,
            Some("failed" | "cancelled") => continue,
            _ => {
                // A success with no store receipt still needs reconciliation.
                error = Some("installation_admission_unconfirmed".into());
                true
            }
        };
        // Completed/materialized history does not consume pending capacity.
        // Bound only actual unresolved hand-offs, after every positive check.
        if values.len() >= 4096 {
            error = Some("installation_index_limit".into());
            continue;
        }
        values.push(
            json!({"card_id":record.card_id,"run_id":run_id,"kind":record.store_kind,
            "label":"accepted installation","backend_pending":true,"unconfirmed":unconfirmed}),
        );
    }
    (values, error)
}

pub(super) fn log(task: &Value, from: usize) -> Result<Value, String> {
    let status = task["status"]
        .as_str()
        .ok_or("installation_admission_unconfirmed")?;
    let pending = matches!(status, "running" | "queued");
    if !pending && !matches!(status, "failed" | "cancelled") {
        return Err(
            "installation_result_unconfirmed: inspect device task and retained files".into(),
        );
    }
    Ok(
        json!({"lines":[],"next":from,"done":!pending,"ok":false,"cancelled":status=="cancelled",
        "awaiting_input":false,"result":null,"error":if pending {Value::Null} else {task.get("error").cloned().filter(|error|!error.is_null()).unwrap_or(json!(status))},
        "backend_pending":pending,"cmsd_task_id":task["id"]}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parent_binding_requires_exact_device_card_kind_and_run() {
        let binding = Binding {
            task_id: "task-fixture".into(),
            run_id: "cmsd-task-fixture".into(),
            card_id: "card-fixture".into(),
            store_kind: "install_local".into(),
            device_id: "dev-fixture".into(),
            socket: "/tmp/test.sock".into(),
        };
        let device = json!({"device_id":"dev-fixture"});
        let task = json!({"id":"task-fixture","kind":"install","card":"card-fixture","store_run_id":"cmsd-task-fixture","status":"running","error":null});
        assert!(matches(&binding, &task, &device));
        for key in ["id", "kind", "card", "store_run_id"] {
            let mut changed = task.clone();
            changed[key] = json!("foreign");
            assert!(!matches(&binding, &changed, &device));
        }
        assert!(!matches(
            &binding,
            &task,
            &json!({"device_id":"another-device"})
        ));
    }

    #[test]
    fn absent_store_log_is_pending_only_while_the_verified_parent_is_active() {
        let task = json!({"id":"task-fixture","status":"running","error":null});
        let pending = log(&task, 0).unwrap();
        assert_eq!(pending["done"], false);
        assert_eq!(pending["backend_pending"], true);
        assert!(
            pending["lines"].as_array().unwrap().is_empty(),
            "synthetic lines must not advance the real store cursor"
        );
        assert_eq!(pending["next"], 0);
        let cancelled = log(&json!({"id":"task-fixture","status":"cancelled"}), 0).unwrap();
        assert_eq!(cancelled["cancelled"], true);
        assert_eq!(cancelled["ok"], false);
        assert!(log(&json!({"id":"task-fixture","status":"succeeded"}), 0).is_err());
        let unknown = unconfirmed(8);
        assert_eq!(unknown["done"], false);
        assert_eq!(unknown["unconfirmed"], true);
        assert_eq!(unknown["next"], 8);
    }

    #[test]
    fn reopening_many_receipts_uses_one_device_and_task_list_lookup() {
        use std::io::{BufRead, BufReader, Write};
        use std::os::unix::net::UnixListener;

        let _home = config::TempHome::new();
        let socket = config::data_dir().join("pending-device.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        let client = Client::at_socket(socket).unwrap();
        let mut tasks = Vec::new();
        for i in 0..6 {
            let task_id = format!("task-{i}");
            let card_id = format!("card-{i}");
            save(
                &client,
                &task_id,
                &card_id,
                "install_local",
                "device-fixture",
            )
            .unwrap();
            tasks.push(json!({"id":task_id,"card":card_id,"kind":"install",
                "store_run_id":format!("cmsd-task-{i}"),"status":if i==5 {"cancelled"}else{"running"}}));
        }
        let worker = std::thread::spawn(move || {
            let mut requests = Vec::new();
            for response in [
                json!({"device_id":"device-fixture"}),
                json!({"tasks":tasks}),
            ] {
                let (mut stream, _) = listener.accept().unwrap();
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                let mut line = String::new();
                reader.read_line(&mut line).unwrap();
                requests.push(line.trim().to_string());
                loop {
                    line.clear();
                    reader.read_line(&mut line).unwrap();
                    if line == "\r\n" {
                        break;
                    }
                }
                let body = serde_json::to_string(&response).unwrap();
                write!(
                    stream,
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                )
                .unwrap();
            }
            requests
        });
        let (runs, error) = pending(None);
        assert!(error.is_none(), "{error:?}");
        assert_eq!(runs.len(), 5);
        assert!(runs.iter().all(|run| run["unconfirmed"] == false));
        assert_eq!(
            worker.join().unwrap(),
            ["GET /v1/device HTTP/1.1", "GET /v1/tasks HTTP/1.1"]
        );
    }

    #[test]
    fn materialized_history_does_not_hide_a_later_unmaterialized_parent() {
        use std::io::{BufRead, BufReader, Write};
        use std::os::unix::{fs::PermissionsExt, net::UnixListener};
        let _home = config::TempHome::new();
        let directory = config::data_dir().join("delegated-runs");
        std::fs::create_dir_all(&directory).unwrap();
        std::fs::create_dir_all(config::data_dir().join("runs")).unwrap();
        let socket = config::data_dir().join("pending-history.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        for i in 0..4097 {
            let task_id = format!("task-{i}");
            let run_id = format!("cmsd-{task_id}");
            let receipt = Binding {
                task_id,
                run_id: run_id.clone(),
                card_id: "card-fixture".into(),
                store_kind: "install_local".into(),
                device_id: "device-fixture".into(),
                socket: socket.clone(),
            };
            let file = path(&run_id).unwrap();
            std::fs::write(&file, serde_json::to_vec(&receipt).unwrap()).unwrap();
            std::fs::set_permissions(file, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        let paths: Vec<_> = std::fs::read_dir(&directory)
            .unwrap()
            .map(|entry| entry.unwrap().path())
            .collect();
        let pending_run = paths
            .last()
            .unwrap()
            .file_stem()
            .unwrap()
            .to_str()
            .unwrap()
            .to_string();
        for path in &paths[..4096] {
            std::fs::write(
                config::data_dir()
                    .join("runs")
                    .join(path.file_name().unwrap()),
                b"{\"outcome\":{\"Ok\":null}}",
            )
            .unwrap();
        }
        let task_id = pending_run.strip_prefix("cmsd-").unwrap().to_string();
        let task = json!({"id":task_id,"card":"card-fixture","kind":"install","store_run_id":pending_run,"status":"queued"});
        let worker = std::thread::spawn(move || {
            for response in [
                json!({"device_id":"device-fixture"}),
                json!({"tasks":[task]}),
            ] {
                let (mut stream, _) = listener.accept().unwrap();
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                let mut line = String::new();
                loop {
                    reader.read_line(&mut line).unwrap();
                    if line == "\r\n" {
                        break;
                    }
                    line.clear();
                }
                let body = response.to_string();
                write!(
                    stream,
                    "HTTP/1.1 200 OK\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                    body.len()
                )
                .unwrap();
            }
        });
        let (runs, error) = pending(None);
        assert!(error.is_none(), "{error:?}");
        assert_eq!(runs.len(), 1);
        assert_eq!(runs[0]["run_id"], pending_run);
        assert_eq!(runs[0]["unconfirmed"], false);
        assert!(
            binding(paths[0].file_stem().unwrap().to_str().unwrap())
                .unwrap()
                .is_some(),
            "history remains available by exact ID"
        );
        worker.join().unwrap();
    }
}
