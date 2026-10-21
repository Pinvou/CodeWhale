use std::collections::BTreeMap;
use std::os::fd::AsRawFd;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

use super::Record;

pub fn unit_valid(name: &str) -> bool {
    name.len() <= 90
        && name.ends_with(".service")
        && (name == "cmsd.service"
            || name == "pinvou-cmsd.service"
            || name.starts_with("cmsd-")
            || name.starts_with("pinvou-cmsd-"))
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_.".contains(&b))
        && !name.contains("..")
}

pub fn run(program: &str, args: &[&str]) -> Result<String, String> {
    let mut command = Command::new(program);
    command.args(args).env("LC_ALL", "C");
    let output = crate::platform::process::output_with_timeout(command, Duration::from_secs(35))?;
    if !output.status.success() {
        return Err(format!(
            "service_command_failed: {program}: {}",
            String::from_utf8_lossy(&output.stderr)
                .chars()
                .take(1000)
                .collect::<String>()
        ));
    }
    Ok(String::from_utf8_lossy(&output.stdout).into_owned())
}

pub fn control(action: &str, unit: &str) -> Result<(), String> {
    if !unit_valid(unit)
        || ![
            "start",
            "stop",
            "restart",
            "enable",
            "disable",
            "reset-failed",
        ]
        .contains(&action)
    {
        return Err("invalid_service_action".into());
    }
    run("systemctl", &["--user", action, unit]).map(|_| ())
}

/// Reset only the exact current registration; a saved unit name alone must not
/// authorize control after an executable, configuration or peer change.
pub(super) fn reset_failed_verified(record: &Record) -> Result<(), String> {
    let mut current = record.clone();
    super::binding::verify(&mut current)?;
    control("reset-failed", &current.unit)
}

/// An inactive unit can retain start-limit counters, while a fresh/collected
/// unit may reject reset-failed. Only that control failure can be tolerated:
/// reverify the complete binding before starting and retain both diagnostics if
/// activation also fails. The strict reset helper remains for stopped cleanup.
pub(super) fn start_verified(record: &Record) -> Result<(), String> {
    let mut current = record.clone();
    super::binding::verify(&mut current)?;
    let reset_error = control("reset-failed", &current.unit).err();
    super::binding::verify(&mut current)?;
    match control("start", &current.unit) {
        Ok(()) => Ok(()),
        Err(start) => Err(match reset_error {
            Some(reset) => format!("{start}; reset_failed_before_start={reset}"),
            None => start,
        }),
    }
}

/// Persistent disable does not remove links created with --runtime. Clear both
/// before removing an owned unit, otherwise the user manager retains a dangling
/// default.target dependency on an uninstalled component.
pub fn disable_all(unit: &str) -> Result<(), String> {
    control("disable", unit)?;
    run("systemctl", &["--user", "--runtime", "disable", unit]).map(|_| ())
}

pub fn show(unit: &str) -> Result<BTreeMap<String, String>, String> {
    if !unit_valid(unit) {
        return Err("invalid_service_unit".into());
    }
    let text = run(
        "systemctl",
        &[
            "--user",
            "show",
            unit,
            "--no-pager",
            "--property=LoadState,ActiveState,SubState,MainPID,FragmentPath,UnitFileState,DropInPaths,NeedDaemonReload",
        ],
    )?;
    Ok(text
        .lines()
        .filter_map(|line| {
            line.split_once('=')
                .map(|(k, v)| (k.to_owned(), v.to_owned()))
        })
        .collect())
}

/// An enabled user unit starts at boot without a login only with lingering.
pub fn linger_enabled() -> Option<bool> {
    // SAFETY: geteuid has no preconditions and does not change the process identity.
    let uid = unsafe { libc::geteuid() }.to_string();
    let mut command = Command::new("loginctl");
    command
        .args(["show-user", &uid, "--property=Linger", "--value"])
        .env("LC_ALL", "C");
    let output =
        crate::platform::process::output_with_timeout(command, Duration::from_secs(2)).ok()?;
    if !output.status.success() {
        return None;
    }
    match String::from_utf8_lossy(&output.stdout).trim() {
        "yes" => Some(true),
        "no" => Some(false),
        _ => None,
    }
}

pub fn peer_pid(path: &Path) -> Result<u32, String> {
    socket_peer(path)?.ok_or_else(|| "socket_unavailable".into())
}

/// A stopped/hung server can leave its listen queue full. A blocking connect
/// would make even OS-only rescue hang behind that unhealthy API.
pub(super) fn socket_peer(path: &Path) -> Result<Option<u32>, String> {
    let stream = match crate::features::megacube::platform::try_connect(path) {
        Ok(stream) => stream,
        Err(error) => {
            return match error.raw_os_error() {
                Some(libc::ENOENT | libc::ECONNREFUSED) => Ok(None),
                Some(libc::EAGAIN | libc::EINPROGRESS) => Err("socket_busy".into()),
                _ => Err("socket_identity_unavailable".into()),
            };
        }
    };
    stream_peer(&stream).map(Some)
}

pub(super) fn stream_peer(stream: &std::os::unix::net::UnixStream) -> Result<u32, String> {
    // SAFETY: all-zero ucred contains valid integer fields for getsockopt output.
    let mut credential: libc::ucred = unsafe { std::mem::zeroed() };
    let mut size = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
    // SAFETY: credential and size are valid writable buffers, socket is owned.
    let result = unsafe {
        libc::getsockopt(
            stream.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            (&mut credential as *mut libc::ucred).cast(),
            &mut size,
        )
    };
    if result != 0 || credential.pid <= 0 {
        return Err("socket_identity_unavailable".into());
    }
    // SAFETY: geteuid has no preconditions.
    if credential.uid != unsafe { libc::geteuid() } {
        return Err("system_or_foreign_service: privileged installer required".into());
    }
    Ok(credential.pid as u32)
}

pub fn bind_live(unit: &str, socket: &Path) -> Result<Record, String> {
    let mut record = bind_process(unit, socket, true)?;
    let status = crate::features::megacube::Client::connect()?.status()?;
    record.version = status
        .get("version")
        .and_then(serde_json::Value::as_str)
        .unwrap_or("unknown")
        .to_owned();
    Ok(record)
}

/// Saved registration evidence permits rescue of an unhealthy daemon. Identity
/// comes from the user manager and /proc; an API timeout is not foreign ownership.
pub(super) fn bind_process(
    unit: &str,
    socket: &Path,
    require_peer: bool,
) -> Result<Record, String> {
    let properties = show(unit)?;
    let pid = properties
        .get("MainPID")
        .and_then(|v| v.parse::<u32>().ok())
        .unwrap_or(0);
    let peer = match socket_peer(socket) {
        Err(error) if !require_peer && error == "socket_busy" => None,
        result => result?,
    };
    if pid == 0 || peer.is_some_and(|peer| peer != pid) || (require_peer && peer != Some(pid)) {
        return Err("service_socket_mismatch".into());
    }
    let binary = std::fs::read_link(format!("/proc/{pid}/exe")).map_err(|e| e.to_string())?;
    let command = std::fs::read(format!("/proc/{pid}/cmdline")).map_err(|e| e.to_string())?;
    if command
        .split(|b| *b == 0)
        .filter(|arg| !arg.is_empty())
        .count()
        != 1
    {
        return Err(
            "custom_service_arguments: inspect the existing installation before migration".into(),
        );
    }
    // Read only cmsd path keys; never return or log the complete environment.
    let raw = std::fs::read(format!("/proc/{pid}/environ")).map_err(|e| e.to_string())?;
    let env: BTreeMap<String, String> = raw
        .split(|b| *b == 0)
        .filter_map(|entry| {
            let value = std::str::from_utf8(entry).ok()?;
            let (key, value) = value.split_once('=')?;
            [
                "CMSD_CONFIG",
                "CMSD_STATE_DIR",
                "XDG_STATE_HOME",
                "XDG_CONFIG_HOME",
            ]
            .contains(&key)
            .then(|| (key.to_owned(), value.to_owned()))
        })
        .collect();
    let home = crate::platform::paths::user_home_dir();
    let config = env
        .get("CMSD_CONFIG")
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            env.get("XDG_CONFIG_HOME")
                .map(PathBuf::from)
                .unwrap_or_else(|| home.join(".config"))
                .join("megacube/cmsd.toml")
        });
    let config_value = if config.exists() {
        parse_config_document(&std::fs::read_to_string(&config).map_err(|e| e.to_string())?)?
    } else {
        toml::Value::Table(Default::default())
    };
    let state = env
        .get("CMSD_STATE_DIR")
        .map(PathBuf::from)
        .or_else(|| {
            config_value
                .get("state_dir")
                .and_then(toml::Value::as_str)
                .map(PathBuf::from)
        })
        .unwrap_or_else(|| {
            env.get("XDG_STATE_HOME")
                .map(PathBuf::from)
                .unwrap_or_else(|| home.join(".local/state"))
                .join("megacube")
        });
    let original_unit = properties
        .get("FragmentPath")
        .filter(|path| Path::new(path.as_str()) == unit_directory().join(unit))
        .and_then(|path| std::fs::read_to_string(path).ok());
    Ok(Record {
        unit: unit.to_owned(),
        socket: socket.to_owned(),
        config,
        state,
        binary,
        version: String::new(),
        owned_unit: false,
        managed: false,
        installed: true,
        original_unit,
        previous: None,
        maintenance_owner: None,
        registration: None,
        pending_install: false,
        bootstrap_import: None,
        factory_import: None,
    })
}

fn parse_config_document(text: &str) -> Result<toml::Value, String> {
    // toml 1.x Value::from_str parses a scalar value, not a TOML document.
    // Existing service configuration is a document with top-level path keys.
    toml::from_str(text).map_err(|e| format!("invalid_service_config: {e}"))
}

pub fn unit_directory() -> PathBuf {
    std::env::var_os("XDG_CONFIG_HOME")
        .filter(|v| !v.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| crate::platform::paths::user_home_dir().join(".config"))
        .join("systemd/user")
}

pub fn quoted(value: &Path) -> Result<String, String> {
    let text = value.to_str().ok_or("invalid_service_path")?;
    if !value.is_absolute() || text.chars().any(char::is_control) {
        return Err("invalid_service_path".into());
    }
    Ok(format!(
        "\"{}\"",
        text.replace('\\', "\\\\")
            .replace('"', "\\\"")
            .replace('%', "%%")
    ))
}

pub fn unit_text(record: &Record) -> Result<String, String> {
    render_unit(record, true)
}

pub(super) fn legacy_unit_text(record: &Record) -> Result<String, String> {
    render_unit(record, false)
}

fn render_unit(record: &Record, clear_environment: bool) -> Result<String, String> {
    // Configuration files remain authoritative. Inherited manager variables
    // must not redirect storage or enable a remote listener for another profile.
    let boundary = if clear_environment {
        "UnsetEnvironment=CMSD_SOCKET CMSD_STATE_DIR CMSD_GATEWAY_LISTEN CMSD_SYSTEMD_MODE CMSD_CARDS_DIR CMSD_ENGINE_ROOT CMSD_UNIT_DIR CMSD_POOL_START CMSD_POOL_END CMSD_PROC_ROOT CMSD_MEMINFO CMSD_DEBUG_AVAILABLE_MIB CMSD_READY_TIMEOUT_SECS CMSD_MODELS_DIR CMSD_STAGING_DIR CMSD_HF_ENDPOINT CMSD_DOCKER CMSD_NVIDIA_SMI CMSD_STORE_SOCKET CMSD_STORE_POLL_MS CMSD_CONTROL_LISTEN PINVOU3_HOME\n"
    } else {
        ""
    };
    Ok(format!(
        "[Unit]\nDescription=Pinvou managed Cube Manage Software device service\n[Service]\nType=simple\nExecStart={}\nEnvironment=CMSD_CONFIG={}\n{boundary}UMask=0077\nRestart=on-failure\nRestartSec=2\n[Install]\nWantedBy=default.target\n",
        quoted(&record.binary)?.replace('$', "$$"),
        quoted(&record.config)?
    ))
}

pub fn wait_ready(record: &Record) -> Result<(), String> {
    let start = std::time::Instant::now();
    loop {
        if let Ok(properties) = show(&record.unit) {
            let pid = properties
                .get("MainPID")
                .and_then(|v| v.parse::<u32>().ok())
                .unwrap_or(0);
            if pid > 0
                && bind_live(&record.unit, &record.socket).is_ok_and(|live| {
                    live.binary == record.binary
                        && live.config == record.config
                        && live.state == record.state
                })
            {
                return Ok(());
            }
        }
        if start.elapsed() > Duration::from_secs(12) {
            return Err(
                "service_not_ready: check Cube Manage Software logs; no model fallback was performed".into(),
            );
        }
        std::thread::sleep(Duration::from_millis(200));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn full_listen_queue_never_blocks_the_identity_probe() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("busy.sock");
        assert_eq!(socket_peer(&path).unwrap(), None);
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        // SAFETY: listener is a live AF_UNIX listening socket.
        assert_eq!(unsafe { libc::listen(listener.as_raw_fd(), 1) }, 0);
        assert_eq!(socket_peer(&path).unwrap(), Some(std::process::id()));
        let mut busy = false;
        for _ in 0..8 {
            match socket_peer(&path) {
                Ok(Some(pid)) => assert_eq!(pid, std::process::id()),
                Err(error) => {
                    assert_eq!(error, "socket_busy");
                    busy = true;
                    break;
                }
                other => panic!("unexpected probe result: {other:?}"),
            }
        }
        assert!(busy, "small unattended listen queue must fill");
    }

    #[test]
    fn existing_service_configuration_parses_as_a_document() {
        let value = parse_config_document(
            "state_dir = \"/tmp/existing state\"\nsocket_path = \"/tmp/existing.sock\"\n[extra]\nenabled = true\n",
        ).unwrap();
        assert_eq!(value["state_dir"].as_str(), Some("/tmp/existing state"));
        assert_eq!(value["socket_path"].as_str(), Some("/tmp/existing.sock"));
        assert!(parse_config_document("state_dir = [unfinished").is_err());
    }

    #[test]
    fn service_units_and_paths_do_not_accept_shell_or_unit_injection() {
        for value in ["cmsd.service", "pinvou-cmsd-qa-20261017.service"] {
            assert!(unit_valid(value));
        }
        for value in [
            "ssh.service",
            "cmsd-x;touch.service",
            "../cmsd.service",
            "cmsd-../other.service",
            "cmsd-x\n.service",
        ] {
            assert!(!unit_valid(value));
        }
        assert!(quoted(Path::new("relative")).is_err());
        assert!(quoted(Path::new("/tmp/a\nb")).is_err());
        assert_eq!(
            quoted(Path::new("/tmp/a b%q\"c")).unwrap(),
            "\"/tmp/a b%%q\\\"c\""
        );
    }
}
