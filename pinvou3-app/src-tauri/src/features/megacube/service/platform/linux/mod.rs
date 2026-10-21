mod activation;
mod binding;
mod bootstrap;
mod bundled;
mod config_migration;
pub use bundled::bundled_package;
mod operation_lock;
mod package;
mod registration;
mod selection_policy;
mod systemd;

use std::fs::{File, OpenOptions};
use std::io::Write;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct Record {
    unit: String,
    socket: PathBuf,
    config: PathBuf,
    state: PathBuf,
    binary: PathBuf,
    version: String,
    owned_unit: bool,
    managed: bool,
    installed: bool,
    original_unit: Option<String>,
    previous: Option<Box<Record>>,
    #[serde(default)]
    maintenance_owner: Option<String>,
    #[serde(default)]
    registration: Option<binding::Registration>,
    /// A first install may have published its exact owned unit before reload failed.
    #[serde(default)]
    pending_install: bool,
    #[serde(default)]
    bootstrap_import: Option<bootstrap::Import>,
    #[serde(default)]
    factory_import: Option<Value>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Job {
    id: String,
    action: String,
    package: Option<String>,
    status: String,
    stage: String,
    created_at: u64,
    finished_at: Option<u64>,
    pid: Option<u32>,
    start_ticks: Option<String>,
    boot_id: String,
    error: Option<String>,
    #[serde(default)]
    expected_hash: Option<String>,
    #[serde(default)]
    recovery: Option<activation::Recovery>,
}

fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}
fn root() -> PathBuf {
    crate::platform::paths::pinvou3_home().join("megacube/cmsd-component")
}

pub(super) fn private_directory(path: &Path) -> Result<(), String> {
    if !path.is_absolute() {
        return Err("invalid_component_root".into());
    }
    if let Ok(metadata) = std::fs::symlink_metadata(path) {
        if metadata.file_type().is_symlink() || !metadata.is_dir() {
            return Err("linked_component_directory".into());
        }
    }
    std::fs::create_dir_all(path).map_err(|e| e.to_string())?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| e.to_string())
}

fn write_bytes(path: &Path, bytes: &[u8]) -> Result<(), String> {
    let parent = path.parent().ok_or("invalid_component_path")?;
    std::fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    if std::fs::symlink_metadata(parent)
        .map_err(|e| e.to_string())?
        .file_type()
        .is_symlink()
    {
        return Err("linked_component_directory".into());
    }
    let mut temp = tempfile::NamedTempFile::new_in(parent).map_err(|e| e.to_string())?;
    temp.write_all(bytes).map_err(|e| e.to_string())?;
    temp.as_file().sync_all().map_err(|e| e.to_string())?;
    temp.persist(path).map_err(|e| e.to_string())?;
    File::open(parent)
        .and_then(|file| file.sync_all())
        .map_err(|e| e.to_string())
}

pub(super) fn write_json(path: &Path, value: &impl Serialize) -> Result<(), String> {
    write_bytes(
        path,
        &serde_json::to_vec_pretty(value).map_err(|e| e.to_string())?,
    )
}

fn read_json<T: for<'de> Deserialize<'de>>(path: &Path) -> Result<T, String> {
    let metadata = std::fs::symlink_metadata(path).map_err(|e| e.to_string())?;
    if !metadata.is_file() || metadata.file_type().is_symlink() || metadata.len() > 512 * 1024 {
        return Err("invalid_component_record".into());
    }
    serde_json::from_slice(&std::fs::read(path).map_err(|e| e.to_string())?)
        .map_err(|e| format!("invalid_component_record: {e}"))
}

fn lock() -> Result<operation_lock::Guard, String> {
    operation_lock::acquire(&root())
}

fn saved_record() -> Result<Option<Record>, String> {
    let path = root().join("service.json");
    if !path.exists() {
        return Ok(None);
    }
    let record: Record = read_json(&path)?;
    if !systemd::unit_valid(&record.unit)
        || configured_unit()?.is_some_and(|unit| unit != record.unit)
        || record.socket != crate::features::megacube::socket_path()
        || !record.state.is_absolute()
        || !record.config.is_absolute()
        || (record.managed && !record.binary.starts_with(root().join("releases")))
    {
        return Err(
            "service_configuration_changed: stored binding does not match this desktop".into(),
        );
    }
    Ok(Some(record))
}

fn configured_unit() -> Result<Option<String>, String> {
    let value = std::env::var("PINVOU3_CMSD_UNIT").ok();
    if value
        .as_deref()
        .is_some_and(|unit| !systemd::unit_valid(unit))
    {
        return Err("invalid_service_unit".into());
    }
    Ok(value)
}

fn locate() -> Result<Option<Record>, String> {
    if config_migration::pending() || registration::pending() {
        let _guard = lock()?;
        recover_locked()?;
    }
    if let Some(mut record) = saved_record()? {
        binding::verify(&mut record)?;
        return Ok(Some(record));
    }
    let candidates = configured_unit()?
        .map(|v| vec![v])
        .unwrap_or_else(|| vec!["cmsd.service".into(), "pinvou-cmsd.service".into()]);
    let socket = crate::features::megacube::socket_path();
    let mut loaded = false;
    let mut binding_error =
        "inspect the service account, unit and socket before control".to_owned();
    for unit in candidates {
        let properties = systemd::show(&unit)?;
        if properties
            .get("LoadState")
            .is_some_and(|v| v != "not-found")
        {
            loaded = true;
            match systemd::bind_live(&unit, &socket) {
                Ok(mut record) => {
                    record.registration = Some(binding::capture(&record)?);
                    return Ok(Some(record));
                }
                Err(error) => binding_error = format!("{unit}: {error}"),
            }
        }
    }
    if loaded || socket.exists() {
        return Err(format!("unverified_service_binding: {binding_error}"));
    }
    Ok(None)
}

fn public_job(mut job: Job) -> Job {
    if job.status == "running"
        && (job.pid.is_none()
            || job.start_ticks.is_none()
            || job.boot_id != boot_id()
            || job.pid.and_then(process_ticks) != job.start_ticks)
    {
        job.status = "interrupted".into();
        job.error =
            Some("component_interrupted: inspect logs and restore the previous component".into());
    } else if job.status == "queued" && now().saturating_sub(job.created_at) > 20 {
        job.status = "interrupted".into();
        job.error = Some("component_worker_not_started".into());
    }
    job.package = None;
    job
}

fn current_job() -> Result<Option<Job>, String> {
    let path = root().join("operation.json");
    if path.exists() {
        read_json::<Job>(&path).map(public_job).map(Some)
    } else {
        Ok(None)
    }
}

pub fn status() -> Result<Value, String> {
    let operation = current_job()?;
    let record = match locate() {
        Ok(record) => record,
        Err(reason) => {
            return Ok(
                json!({"supported":true,"installed":false,"scope":"unverified","reason":reason,
            "socketPath":crate::features::megacube::socket_path(),"operation":operation}),
            );
        }
    };
    // Record a verified component binding without changing service/model
    // ownership. This keeps rescue controls usable after the daemon goes down.
    if let Some(binding) = record.as_ref() {
        if !root().join("service.json").exists() {
            if let Ok(_guard) = lock() {
                if !root().join("service.json").exists() {
                    persist(binding)?;
                }
            }
        }
    }
    let unit = record
        .as_ref()
        .map(|r| r.unit.clone())
        .unwrap_or(configured_unit()?.unwrap_or_else(|| "pinvou-cmsd.service".into()));
    let properties = systemd::show(&unit)?;
    let active = properties
        .get("ActiveState")
        .map(String::as_str)
        .unwrap_or("inactive");
    let installed = record.as_ref().is_some_and(|r| r.installed)
        && properties
            .get("LoadState")
            .is_some_and(|v| v != "not-found");
    let enabled = match properties.get("UnitFileState").map(String::as_str) {
        Some("enabled") => Some(true),
        Some("disabled" | "enabled-runtime") => Some(false),
        _ => None,
    };
    let managed = record.as_ref().is_some_and(|r| r.managed);
    let owned = record.as_ref().is_some_and(|r| r.owned_unit);
    let imported = record
        .as_ref()
        .and_then(|record| record.bootstrap_import.as_ref());
    let awaiting_import = imported.is_none() && record.as_ref().is_some_and(bootstrap::has_journal);
    let (candidate, import_error) = if awaiting_import {
        match bootstrap::inspect(record.as_ref().ok_or("service_not_installed")?) {
            Ok(candidate) => (candidate.map(|proof| proof.public()), None),
            Err(error) => (None, Some(error)),
        }
    } else {
        (None, None)
    };
    Ok(
        json!({"supported":true,"installed":installed,"scope":"user","unitName":unit,
        "activeState":active,"bootEnabled":enabled,"lingerEnabled":systemd::linger_enabled(),"managedInstallation":managed,"ownedUnit":owned,
        "version":record.as_ref().map(|r|r.version.clone()),"socketPath":crate::features::megacube::socket_path(),
        "canInstall":!installed&&record.is_none(),"canStart":installed&&active!="active",
        "canStop":installed&&active!="inactive","canUpdate":!record.as_ref().is_some_and(|r|r.pending_install)&&!awaiting_import&&(installed||owned)&&(owned||managed||record.as_ref().is_some_and(|r|r.original_unit.is_some())),
        "canImportBootstrap":installed&&candidate.is_some(),"bootstrapCandidate":candidate,
        "bootstrapImported":imported.map(bootstrap::Import::public),"bootstrapImportError":import_error,
        "canUninstall":installed&&owned,"canRollback":!awaiting_import&&root().join("backup.json").exists(),
        "canRestore":record.as_ref().is_some_and(|r|!r.installed&&r.owned_unit),
        "canResumeMaintenance":(record.as_ref().is_some_and(|r|r.maintenance_owner.is_some())||operation.as_ref().is_some_and(|job|job.status=="interrupted"))&&active=="active",
        "operation":operation,"reason":if installed&&!managed&&imported.is_none() {Some("external_installation")}else{None}}),
    )
}

pub fn inspect_package(path: &str) -> Result<Value, String> {
    let (manifest, _) = package::verify(Path::new(path))?;
    Ok(
        json!({"manifest":manifest,"publisherVerified":false,"source":"operator-selected-offline-package"}),
    )
}

pub fn logs() -> Result<Value, String> {
    let record = locate()?.ok_or("service_not_installed")?;
    let output = systemd::run(
        "journalctl",
        &[
            "--user",
            "--unit",
            &record.unit,
            "--lines=80",
            "--no-pager",
            "--output=short-iso",
        ],
    )?;
    let lines = output
        .chars()
        .rev()
        .take(24000)
        .collect::<String>()
        .chars()
        .rev()
        .collect::<String>();
    Ok(json!({"unitName":record.unit,"lines":lines}))
}

fn boot_id() -> String {
    std::fs::read_to_string("/proc/sys/kernel/random/boot_id")
        .unwrap_or_default()
        .trim()
        .to_owned()
}
fn process_ticks(pid: u32) -> Option<String> {
    std::fs::read_to_string(format!("/proc/{pid}/stat"))
        .ok()?
        .rsplit_once(") ")?
        .1
        .split_whitespace()
        .nth(19)
        .map(str::to_owned)
}

pub fn submit(
    action: &str,
    package: Option<String>,
    expected_hash: Option<String>,
) -> Result<Value, String> {
    if ![
        "install",
        "start",
        "stop",
        "rescue_stop",
        "restart",
        "update",
        "rollback",
        "restore",
        "resume",
        "uninstall",
        "enable_boot",
        "disable_boot",
        "import_bootstrap",
    ]
    .contains(&action)
    {
        return Err("unsupported_component_action".into());
    }
    let _lock = lock()?;
    recover_locked()?;
    if action == "import_bootstrap"
        && !expected_hash.as_deref().is_some_and(|hash| {
            hash.len() == 64
                && hash
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        })
    {
        return Err(if action == "import_bootstrap" {
            "bootstrap_preview_required"
        } else {
            "component_preview_required"
        }
        .into());
    }
    if let Some(job) = current_job()? {
        if ["queued", "running"].contains(&job.status.as_str()) {
            return Err("component_busy".into());
        }
        if job.status == "interrupted" {
            // Preserve the interrupted outcome in history before another
            // operation replaces the current receipt. Keep the private package
            // path in the journal; public views redact it separately.
            let mut archived: Job = read_json(&root().join("operation.json"))?;
            archived.status = "interrupted".into();
            archived.error = job.error;
            archived.finished_at = Some(now());
            save_job(&archived)?;
            if !["rollback", "restore", "start", "resume", "rescue_stop"].contains(&action) {
                return Err("component_recovery_required".into());
            }
        }
    }
    if ["install", "update"].contains(&action) {
        let inspected = inspect_package(package.as_deref().ok_or("package_required")?)?;
        if inspected["manifest"]["sha256"].as_str() != expected_hash.as_deref() {
            return Err("package_changed_since_preview".into());
        }
    }
    private_directory(&root().join("jobs"))?;
    let id = format!(
        "component-{}",
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos()
    );
    let mut job = Job {
        id: id.clone(),
        action: action.into(),
        package,
        status: "queued".into(),
        stage: "queued".into(),
        created_at: now(),
        finished_at: None,
        pid: None,
        start_ticks: None,
        boot_id: boot_id(),
        error: None,
        expected_hash,
        recovery: None,
    };
    write_json(&root().join("jobs").join(format!("{id}.json")), &job)?;
    write_json(&root().join("operation.json"), &job)?;
    let mut command = Command::new(std::env::current_exe().map_err(|e| e.to_string())?);
    command
        .arg("--cmsd-component-worker")
        .arg(&id)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    // SAFETY: the child is single-threaded between fork and exec; setsid is async-signal-safe.
    unsafe {
        command.pre_exec(|| {
            if libc::setsid() < 0 {
                Err(std::io::Error::last_os_error())
            } else {
                Ok(())
            }
        });
    }
    match command.spawn() {
        Ok(_) => Ok(json!({"operationId":id,"status":"queued"})),
        Err(error) => {
            job.status = "failed".into();
            job.error = Some(format!("component_worker_failed: {error}"));
            save_job(&job)?;
            Err("component_worker_failed".into())
        }
    }
}

fn save_job(job: &Job) -> Result<(), String> {
    write_json(&root().join("jobs").join(format!("{}.json", job.id)), job)?;
    write_json(&root().join("operation.json"), job)
}

fn stage(job: &mut Job, value: &str) -> Result<(), String> {
    job.stage = value.into();
    save_job(job)
}

pub fn run_worker(id: &str) -> Result<(), String> {
    if !id.starts_with("component-") || !id.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
    {
        return Err("invalid_component_job".into());
    }
    let started = std::time::Instant::now();
    let _lock = loop {
        match lock() {
            Ok(lock) => break lock,
            Err(error)
                if started.elapsed() < std::time::Duration::from_secs(5)
                    && error.starts_with("component_busy") =>
            {
                std::thread::sleep(std::time::Duration::from_millis(50))
            }
            Err(error) => return Err(error),
        }
    };
    recover_locked()?;
    let mut job: Job = read_json(&root().join("jobs").join(format!("{id}.json")))?;
    if job.id != id || job.status != "queued" {
        return Err("component_job_not_queued".into());
    }
    job.status = "running".into();
    job.pid = Some(std::process::id());
    job.start_ticks = process_ticks(std::process::id());
    save_job(&job)?;
    let result = perform(&mut job);
    job.finished_at = Some(now());
    job.status = if result.is_ok() {
        "succeeded"
    } else {
        "failed"
    }
    .into();
    job.error = result.as_ref().err().cloned();
    save_job(&job)?;
    result
}

fn persist(record: &Record) -> Result<(), String> {
    write_json(&root().join("service.json"), record)
}

fn state_has_pending(record: &Record) -> Result<bool, String> {
    let path = record.state.join("state.json");
    if !path.exists() {
        return Ok(false);
    }
    let metadata = std::fs::symlink_metadata(&path).map_err(|e| e.to_string())?;
    if metadata.file_type().is_symlink() || metadata.len() > 16 * 1024 * 1024 {
        return Err("task_state_unreadable".into());
    }
    let state: Value = serde_json::from_slice(&std::fs::read(&path).map_err(|e| e.to_string())?)
        .map_err(|_| "task_state_unreadable")?;
    let tasks = state
        .get("tasks")
        .and_then(Value::as_object)
        .ok_or("task_state_unreadable")?;
    Ok(tasks.values().any(|task| {
        !matches!(
            task.get("status").and_then(Value::as_str),
            Some("succeeded" | "failed" | "cancelled")
        )
    }))
}

fn begin_maintenance(record: &mut Record, job: &Job) -> Result<(), String> {
    let properties = systemd::show(&record.unit)?;
    let active = properties
        .get("ActiveState")
        .is_some_and(|v| v == "active" || v == "activating");
    if !active {
        private_directory(&record.state)?;
        let daemon_lock = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW)
            .open(record.state.join("daemon.lock"))
            .map_err(|e| e.to_string())?;
        daemon_lock
            .try_lock()
            .map_err(|_| "service_still_running")?;
        if state_has_pending(record)? {
            return Err(
                "task_in_progress: start Cube Manage Software and reconcile its persistent tasks first".into(),
            );
        }
        let owner = record
            .maintenance_owner
            .clone()
            .unwrap_or_else(|| job.id.clone());
        let path = record.state.join("component-maintenance.json");
        if path.exists()
            && read_json::<Value>(&path)?
                .get("owner")
                .and_then(Value::as_str)
                != Some(owner.as_str())
        {
            return Err("maintenance_owned".into());
        }
        record.maintenance_owner = Some(owner.clone());
        persist(record)?;
        write_json(&path, &json!({"owner":owner,"created_at":now()}))?;
        return Ok(());
    }
    let pid = properties
        .get("MainPID")
        .and_then(|v| v.parse::<u32>().ok())
        .unwrap_or(0);
    if pid == 0 || systemd::peer_pid(&record.socket)? != pid {
        return Err("service_socket_mismatch".into());
    }
    let owner = record
        .maintenance_owner
        .clone()
        .unwrap_or_else(|| job.id.clone());
    record.maintenance_owner = Some(owner.clone());
    persist(record)?;
    let result =
        crate::features::megacube::Client::connect()?.component_maintenance("begin", &owner)?;
    if result.get("maintenance").and_then(Value::as_bool) != Some(true) {
        return Err(format!("maintenance_not_acquired: {result}"));
    }
    Ok(())
}

fn end_maintenance(record: &mut Record) -> Result<(), String> {
    if let Some(owner) = record.maintenance_owner.clone() {
        let result =
            crate::features::megacube::Client::connect()?.component_maintenance("end", &owner)?;
        if result.get("maintenance").and_then(Value::as_bool) != Some(false) {
            return Err(format!("maintenance_not_released: {result}"));
        }
        record.maintenance_owner = None;
        persist(record)?;
    }
    Ok(())
}

fn end_stopped_maintenance(record: &mut Record) -> Result<(), String> {
    let Some(owner) = record.maintenance_owner.clone() else {
        return Ok(());
    };
    let daemon_lock = OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW)
        .open(record.state.join("daemon.lock"))
        .map_err(|e| e.to_string())?;
    daemon_lock
        .try_lock()
        .map_err(|_| "service_still_running")?;
    let path = record.state.join("component-maintenance.json");
    if path.exists() {
        let value: Value = read_json(&path)?;
        if value.get("owner").and_then(Value::as_str) != Some(owner.as_str()) {
            return Err("maintenance_owned".into());
        }
        std::fs::remove_file(path).map_err(|e| e.to_string())?;
    }
    record.maintenance_owner = None;
    persist(record)
}

fn archive_state(record: &Record, job: &Job) -> Result<(), String> {
    let directory = root().join("backups").join(&job.id);
    private_directory(&directory)?;
    for (name, path) in [
        ("cmsd.toml", record.config.clone()),
        ("state.json", record.state.join("state.json")),
        (
            "device-identity.json",
            record.state.join("device-identity.json"),
        ),
    ] {
        if path.exists() {
            let metadata = std::fs::symlink_metadata(&path).map_err(|e| e.to_string())?;
            if metadata.file_type().is_symlink() || metadata.len() > 16 * 1024 * 1024 {
                return Err("component_backup_refused".into());
            }
            write_bytes(
                &directory.join(name),
                &std::fs::read(path).map_err(|e| e.to_string())?,
            )?;
        }
    }
    write_json(&directory.join("service.json"), record)
}

fn recover_locked() -> Result<(), String> {
    config_migration::recover_locked()?;
    registration::recover_locked()
}

fn write_registration(record: &mut Record) -> Result<(), String> {
    registration::write(record)
}

fn new_record() -> Result<Record, String> {
    let socket = crate::features::megacube::socket_path();
    if socket.exists() {
        return Err(
            "socket_in_use: do not install over an existing or stale unverified service".into(),
        );
    }
    let gateway = std::env::var("PINVOU3_CMSD_GATEWAY").unwrap_or_else(|_| "127.0.0.1:8000".into());
    let address: std::net::SocketAddr = gateway.parse().map_err(|_| "invalid_gateway_address")?;
    if !address.ip().is_loopback() || address.port() == 0 {
        return Err("invalid_gateway_address".into());
    }
    let _probe = std::net::TcpListener::bind(address).map_err(|_| "gateway_in_use")?;
    let unit = configured_unit()?.unwrap_or_else(|| "pinvou-cmsd.service".into());
    if std::fs::symlink_metadata(systemd::unit_directory().join(&unit)).is_ok()
        || std::fs::symlink_metadata(systemd::unit_directory().join(format!("{unit}.d"))).is_ok()
    {
        return Err("service_registration_exists".into());
    }
    let record = Record {
        unit,
        socket,
        config: root().join("cmsd.toml"),
        state: root().join("state"),
        binary: PathBuf::new(),
        version: String::new(),
        owned_unit: true,
        managed: true,
        installed: false,
        original_unit: None,
        previous: None,
        maintenance_owner: None,
        registration: None,
        pending_install: false,
        bootstrap_import: None,
        factory_import: None,
    };
    let home = crate::platform::paths::pinvou3_home();
    let mut config = toml::map::Map::new();
    for (key, path) in [
        ("socket_path", record.socket.clone()),
        ("state_dir", record.state.clone()),
        ("cards_dir", home.join("model-store/cards")),
        ("models_dir", home.join("model-store/models")),
        ("staging_dir", home.join("model-store/staging")),
        ("store_socket_path", home.join("model-store/service.sock")),
        ("unit_dir", systemd::unit_directory()),
    ] {
        config.insert(
            key.into(),
            toml::Value::String(path.to_string_lossy().into_owned()),
        );
    }
    config.insert("gateway_listen".into(), toml::Value::String(gateway));
    config.insert("systemd_mode".into(), toml::Value::String("user".into()));
    config.insert(
        "native_unit_namespace".into(),
        toml::Value::String("state".into()),
    );
    config.insert(
        "require_native_profile_budget".into(),
        toml::Value::String("v1".into()),
    );
    config.insert(
        "require_store_selection".into(),
        toml::Value::String("v1".into()),
    );
    private_directory(&record.state)?;
    write_bytes(
        &record.config,
        toml::to_string(&config)
            .map_err(|e| e.to_string())?
            .as_bytes(),
    )?;
    Ok(record)
}

struct ConfigReceipts {
    current: Record,
    intent: config_migration::Intent,
}

/// Preflight every recovery receipt before changing the shared configuration.
/// Preparing this plan does not publish it or assert that the migration ran.
fn prepare_config_receipts(
    record: &Record,
    config: &selection_policy::PreparedConfig,
) -> Result<Option<ConfigReceipts>, String> {
    if !config.needs_update() {
        return Ok(None);
    }
    let current = config.plan_receipt(record)?;
    let path = root().join("backup.json");
    let backup_before = match std::fs::symlink_metadata(&path) {
        Ok(_) => {
            let backup: Record = read_json(&path)?;
            if !binding::same_target(&backup, record) {
                return Err("rollback_binding_mismatch".into());
            }
            Some(backup)
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => None,
        Err(error) => return Err(error.to_string()),
    };
    let backup_after = backup_before
        .as_ref()
        .map(|backup| config.plan_receipt(backup))
        .transpose()?;
    let (old, new) = config.revision_pair();
    let intent =
        config_migration::Intent::prepare(record, &current, backup_before, backup_after, old, new)?;
    Ok(Some(ConfigReceipts { current, intent }))
}

fn perform(job: &mut Job) -> Result<(), String> {
    stage(job, "checking")?;
    let existing = locate()?;
    if job.action == "install" && existing.is_some() {
        return Err("service_already_installed: use restore or update".into());
    }
    if existing
        .as_ref()
        .is_some_and(|record| !record.installed && !(job.action == "update" && record.owned_unit))
        && !["restore", "rollback"].contains(&job.action.as_str())
    {
        return Err("service_not_installed".into());
    }
    let mut record = match existing {
        Some(record) => record,
        None if job.action == "install" => new_record()?,
        None => return Err("service_not_installed".into()),
    };
    if ["update", "rollback", "restore"].contains(&job.action.as_str())
        && record.bootstrap_import.is_none()
        && bootstrap::has_journal(&record)
    {
        return Err("bootstrap_import_required: explicitly import the verified bootstrap installation first".into());
    }
    if record.pending_install && !["restore", "install"].contains(&job.action.as_str()) {
        return Err(
            "component_install_recovery_required: restore the verified first-install candidate"
                .into(),
        );
    }
    if job.action != "install" {
        persist(&record)?;
    }
    match job.action.as_str() {
        "import_bootstrap" => {
            if record.bootstrap_import.is_none() {
                let proof =
                    bootstrap::inspect(&record)?.ok_or("bootstrap_provenance_unverified")?;
                if job.expected_hash.as_deref() != Some(proof.sha256()) {
                    return Err("bootstrap_changed_since_preview".into());
                }
                record.bootstrap_import = Some(proof.complete());
                persist(&record)?;
            }
        }
        "start" => {
            verify_cached(&record)?;
            let config = selection_policy::prepare_config(&record.config)?;
            let migration = prepare_config_receipts(&record, &config)?;
            if let Some(migration) = &migration {
                migration.intent.begin()?;
            }
            if selection_policy::apply_config(&record.config, &config)? {
                let migration = migration.ok_or("component_config_migration_unprepared")?;
                record = migration.current;
                migration.intent.commit_locked()?;
            }
            write_store_reader(&record)?;
            systemd::start_verified(&record)?;
            systemd::wait_ready(&record)?;
            end_maintenance(&mut record)?;
        }
        "resume" => {
            verify_cached(&record)?;
            end_maintenance(&mut record)?;
        }
        "stop" | "restart" | "update" | "uninstall" => {
            if job.action == "uninstall" && !record.owned_unit {
                return Err(
                    "external_installation: only the Pinvou-owned service can be uninstalled here"
                        .into(),
                );
            }
            if job.action == "update" {
                return replace_package(job, &mut record);
            }
            if job.action == "restart" {
                verify_cached(&record)?;
            }
            begin_maintenance(&mut record, job)?;
            stage(job, "stopping")?;
            systemd::control("stop", &record.unit)?;
            if job.action == "restart" {
                systemd::start_verified(&record)?;
                systemd::wait_ready(&record)?;
                end_maintenance(&mut record)?;
            }
            if job.action == "uninstall" {
                archive_state(&record, job)?;
                write_json(&root().join("backup.json"), &record)?;
                systemd::disable_all(&record.unit)?;
                registration::remove(&mut record)?;
            }
        }
        "rescue_stop" => {
            stage(job, "stopping")?;
            systemd::control("stop", &record.unit)?;
        }
        "install" => return replace_package(job, &mut record),
        "rollback" | "restore" => {
            let previous: Record = if job.action == "rollback" {
                read_json(&root().join("backup.json"))?
            } else {
                record.clone()
            };
            activation::switch_to_saved(job, &mut record, previous)?;
        }
        "enable_boot" => {
            verify_cached(&record)?;
            systemd::control("enable", &record.unit)?;
        }
        "disable_boot" => systemd::disable_all(&record.unit)?,
        _ => return Err("unsupported_component_action".into()),
    }
    stage(job, "done")
}

fn verify_cached(record: &Record) -> Result<(), String> {
    if record.managed {
        let parent = record.binary.parent().ok_or("invalid_component_record")?;
        let hash = parent
            .file_name()
            .and_then(|name| name.to_str())
            .ok_or("invalid_component_record")?;
        if !parent.starts_with(root().join("releases"))
            || hash.len() != 64
            || package::hash(&record.binary)? != hash
        {
            return Err("cached_package_changed".into());
        }
    }
    let mut command = Command::new(&record.binary);
    command.arg("--component-info");
    let output =
        crate::platform::process::output_with_timeout(command, std::time::Duration::from_secs(4))?;
    let info: Value = serde_json::from_slice(&output.stdout).map_err(|_| "package_probe_failed")?;
    if !output.status.success() {
        return Err("package_probe_failed".into());
    }
    if store_adoption_required(record)? && info["store_adoption"] != 1 {
        return Err("component_store_adoption_unsupported".into());
    }
    selection_policy::verify_component(&info)?;

    Ok(())
}

fn state_namespace_required(record: &Record) -> Result<bool, String> {
    let config: toml::Value = toml::from_str(
        &std::fs::read_to_string(&record.config).map_err(|_| "unverified_service_config")?,
    )
    .map_err(|_| "unverified_service_config")?;
    match config
        .get("native_unit_namespace")
        .and_then(toml::Value::as_str)
    {
        Some("state") => Ok(true),
        Some("legacy") | None => Ok(false),
        _ => Err("unverified_service_config".into()),
    }
}

fn store_adoption_required(record: &Record) -> Result<bool, String> {
    match std::fs::symlink_metadata(record.state.join("store-adoption-v1")) {
        Ok(_) => Ok(true),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(_) => Err("component_state_unreadable".into()),
    }
}

// This private receipt opts this cmsd installation into completed model-store
// enrollment. cmsd invokes only the headless read-only inventory command.
fn write_store_reader(record: &Record) -> Result<(), String> {
    private_directory(&record.state)?;
    write_json(
        &record.state.join("store-reader.json"),
        &json!({
            "format":"pinvou-store-reader-v1",
            "program":std::env::current_exe().and_then(|p| p.canonicalize()).map_err(|e| e.to_string())?,
            "profile":crate::platform::paths::pinvou3_home(),
        }),
    )
}

fn model_boot_policy_required(record: &Record) -> Result<bool, String> {
    match std::fs::symlink_metadata(record.state.join("model-boot-policy-v1")) {
        Ok(_) => Ok(true),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(_) => Err("component_state_unreadable".into()),
    }
}

fn profile_budget_required(record: &Record) -> Result<bool, String> {
    let config: toml::Value = toml::from_str(
        &std::fs::read_to_string(&record.config).map_err(|_| "unverified_service_config")?,
    )
    .map_err(|_| "unverified_service_config")?;
    match config.get("require_native_profile_budget") {
        None => Ok(false),
        Some(value) if value.as_str() == Some("v1") => Ok(true),
        _ => Err("unverified_service_config".into()),
    }
}

fn replace_package(job: &mut Job, record: &mut Record) -> Result<(), String> {
    if job.action == "update" && record.bootstrap_import.is_none() && bootstrap::has_journal(record)
    {
        return Err("bootstrap_import_required: explicitly import the verified bootstrap installation first".into());
    }
    if job.action == "update" && !record.owned_unit && record.original_unit.is_none() {
        return Err(
            "external_installation: no restorable user unit; service was not stopped".into(),
        );
    }
    stage(job, "verifying_package")?;
    let (manifest, binary) = package::stage(
        Path::new(job.package.as_deref().ok_or("package_required")?),
        &root().join("releases"),
        job.expected_hash
            .as_deref()
            .ok_or("package_preview_required")?,
        state_namespace_required(record)?,
        profile_budget_required(record)?,
        model_boot_policy_required(record)?,
        store_adoption_required(record)?,
        &record.state,
    )?;
    let config_update = selection_policy::prepare_config(&record.config)?;
    // Reject damaged recovery evidence before stopping the current service.
    if config_update.needs_update() {
        config_update.plan_receipt(record)?;
    }
    if job.action == "update"
        && record.installed
        && record.binary == binary
        && !config_update.needs_update()
    {
        return stage(job, "done");
    }
    let was_running = systemd::show(&record.unit)?
        .get("ActiveState")
        .is_some_and(|value| value == "active");
    if job.action == "update" {
        if let (Ok(old), Ok(new)) = (
            semver::Version::parse(&record.version),
            semver::Version::parse(&manifest.version),
        ) {
            if new < old {
                return Err("package_downgrade: use a verified rollback instead".into());
            }
        }
    }
    // Persist a real candidate identity before the first side effect. A failed
    // registration remains an explicitly recoverable, uninstalled preparation.
    if job.action == "install" {
        record.binary = binary.clone();
        record.version = manifest.version.clone();
        record.pending_install = true;
        record.registration = Some(binding::prepare_install(record)?);
    }
    begin_maintenance(record, job)?;
    let mut before = record.clone();
    before.previous = None;
    if job.action == "update" {
        write_json(&root().join("backup.json"), &before)?;
    }
    stage(job, "stopping")?;
    if job.action == "update" && record.installed {
        systemd::control("stop", &record.unit)?;
    }
    if job.action == "update" {
        archive_state(record, job)?;
    }
    let migration = prepare_config_receipts(record, &config_update)?;
    record.previous = if job.action == "update" {
        Some(Box::new(before.clone()))
    } else {
        None
    };
    record.binary = binary;
    record.version = manifest.version;
    record.managed = true;
    record.installed = true;
    let mut published = before.clone();
    let result = (|| {
        stage(job, "installing")?;
        if let Some(migration) = &migration {
            migration.intent.begin()?;
        }
        if selection_policy::apply_config(&record.config, &config_update)? {
            let migration = migration.ok_or("component_config_migration_unprepared")?;
            // Update in-memory compensation evidence before fallible persistence.
            // The config bytes changed successfully; all other identities stay exact.
            published = migration.current.clone();
            before = migration.current;
            before.previous = None;
            if job.action == "update" {
                record.previous = Some(Box::new(before.clone()));
            }
            migration.intent.commit_locked()?;
        }
        write_registration(record)?;
        published = record.clone();
        persist(record)?;
        write_store_reader(record)?;
        if job.action == "install" {
            systemd::control("enable", &record.unit)?;
        }
        if job.action == "install" || was_running || !before.installed {
            stage(job, "starting")?;
            systemd::start_verified(record)?;
            systemd::wait_ready(record)?;
            end_maintenance(record)?;
        } else {
            end_stopped_maintenance(record)?;
        }
        stage(job, "done")
    })();
    if let Err(error) = result {
        if job.action == "update" {
            config_migration::recover_locked().map_err(|recovery| {
                format!("update_failed: {error}; config_receipt_recovery={recovery}")
            })?;
            let restore = activation::compensate(job, before, &mut published, was_running);
            return Err(format!(
                "update_failed: {error}; rollback={}",
                restore.err().unwrap_or_else(|| "succeeded".into())
            ));
        }
        binding::verify(&mut published)?;
        if published.installed {
            let _ = systemd::control("stop", &published.unit);
        }
        return Err(format!("install_failed: {error}; component data retained"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn pid_reuse_or_missing_worker_is_not_reported_as_running() {
        let job = Job {
            id: "component-test".into(),
            action: "update".into(),
            package: Some("/private/package".into()),
            status: "running".into(),
            stage: "stopping".into(),
            created_at: now(),
            finished_at: None,
            pid: Some(std::process::id()),
            start_ticks: Some("wrong-start-time".into()),
            boot_id: boot_id(),
            error: None,
            expected_hash: None,
            recovery: None,
        };
        let view = public_job(job);
        assert_eq!(view.status, "interrupted");
        assert!(view.package.is_none());
    }

    #[test]
    fn pending_durable_model_task_prevents_offline_component_mutation() {
        let directory = tempfile::tempdir().unwrap();
        let record = Record {
            unit: "pinvou-cmsd-test.service".into(),
            socket: directory.path().join("cmsd.sock"),
            config: directory.path().join("cmsd.toml"),
            state: directory.path().to_owned(),
            binary: directory.path().join("cmsd"),
            version: "0.1.0".into(),
            owned_unit: true,
            managed: true,
            installed: true,
            original_unit: None,
            previous: None,
            maintenance_owner: None,
            registration: None,
            pending_install: false,
            bootstrap_import: None,
            factory_import: None,
        };
        let path = directory.path().join("state.json");
        std::fs::write(
            &path,
            r#"{"tasks":{"t1":{"kind":"switch","status":"running"}}}"#,
        )
        .unwrap();
        assert!(state_has_pending(&record).unwrap());
        std::fs::write(&path, r#"{"tasks":{"t1":{"status":"succeeded"}}}"#).unwrap();
        assert!(!state_has_pending(&record).unwrap());
        std::fs::write(&path, b"corrupt").unwrap();
        assert!(state_has_pending(&record).is_err());
    }
}
