//! Read-only runtime proof for an uninstall reservation. A reservation never
//! grants permission to stop or reconfigure a previously unowned resource.
use crate::app::{api_err, ApiErr, App};
use crate::json::Value;
use crate::store::ModelState;

pub(super) fn reference(
    app: &App,
    card: &str,
    model: Option<&ModelState>,
) -> Result<Value, ApiErr> {
    let mut out = Value::obj();
    let owned = model.is_some_and(|m| m.lifecycle_owned);
    let declaration = if owned {
        None
    } else {
        Some(crate::ops::find_card(app, card)?)
    };
    let kind = model
        .map(|m| m.engine_type.as_str())
        .or_else(|| declaration.as_ref().map(|c| c.engine_kind.as_str()))
        .ok_or_else(|| api_err(409, "state_unknown", "不能核实卡片运行时"))?;
    if declaration
        .as_ref()
        .is_some_and(|c| c.engine_kind.as_str() != kind)
    {
        return Err(api_err(
            409,
            "configuration_changed",
            "卡片与发现的运行时类型不一致",
        ));
    }
    out.set("engine_type", kind.into());
    out.set("card", card.into());
    if kind == "container" {
        let observed = model.and_then(|m| m.container_name.as_deref());
        let declared = declaration
            .as_ref()
            .and_then(|c| c.container_name.as_deref());
        if observed.is_some() && declared.is_some() && observed != declared {
            return Err(api_err(
                409,
                "configuration_changed",
                "卡片与发现的容器引用不一致",
            ));
        }
        let name = observed
            .or(declared)
            .ok_or_else(|| api_err(409, "state_unknown", "缺少容器引用"))?;
        if name.is_empty()
            || name.len() > 128
            || !name.as_bytes()[0].is_ascii_alphanumeric()
            || !name
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
        {
            return Err(api_err(409, "state_unknown", "容器引用不合法"));
        }
        out.set("container_name", name.into());
    } else if kind == "native" {
        out.set("unit", app.systemd.unit_name_for(card).into());
        out.set(
            "legacy_unit",
            format!("megacube-model@{card}.service").into(),
        );
        // Keep a complete read-only proof after registration/card deletion.
        // A missing adoption binding does not prove that store scripts stopped.
        let native = declaration
            .clone()
            .or_else(|| crate::ops::find_card_raw(app, card).ok());
        let deploy = native.as_ref().and_then(|card| card.deploy.as_ref());
        let port = model
            .map(|model| model.port)
            .filter(|port| *port > 0)
            .or_else(|| deploy.map(|deploy| deploy.port))
            .unwrap_or(0);
        out.set("native_port", (port as u64).into());
        out.set(
            "native_host",
            deploy
                .map(|deploy| deploy.host.as_str())
                .unwrap_or("127.0.0.1")
                .into(),
        );
        out.set(
            "native_model_id",
            model
                .map(|model| model.model_id.as_str())
                .or_else(|| native.as_ref().map(|card| card.model_id.as_str()))
                .unwrap_or("")
                .into(),
        );
        out.set(
            "native_engine_sha8",
            model
                .map(|model| model.engine_sha8.as_str())
                .or_else(|| native.as_ref().map(|card| card.engine_sha8.as_str()))
                .unwrap_or("")
                .into(),
        );
        if let Some(weights) = native.as_ref().and_then(|card| card.weights_path()) {
            out.set(
                "native_weights",
                weights.to_string_lossy().into_owned().into(),
            );
        }
        out.set(
            "native_uid",
            (app.systemd
                .model_run_uid()
                .map_err(|_| api_err(409, "state_unknown", "原生模型执行账号不可核实"))?
                as u64)
                .into(),
        );
    } else {
        return Err(api_err(
            409,
            "unsupported_runtime",
            "此运行时不能取得卸载预留",
        ));
    }
    Ok(out)
}

pub(super) fn stopped(
    app: &App,
    reference: &Value,
    require_no_autostart: bool,
) -> Result<(), ApiErr> {
    let stopped = match reference.get("engine_type").and_then(Value::as_str) {
        Some("container") => {
            let name = reference
                .get("container_name")
                .and_then(Value::as_str)
                .ok_or_else(|| api_err(409, "maintenance_corrupt", "维护记录缺容器引用"))?;
            match app
                .docker
                .inspect(name)
                .map_err(|_| api_err(409, "state_unknown", "容器状态不可核实，保持保护"))?
            {
                None => true,
                Some(c) => {
                    !c.running && c.pid == 0 && (!require_no_autostart || c.restart_policy == "no")
                }
            }
        }
        Some("native") => {
            if let Some(card) = reference.get("card").and_then(Value::as_str) {
                if crate::store_adoption::native_state(app, card)
                    .map_err(|_| api_err(409, "state_unknown", "商店原生进程不可核实，保持保护"))?
                    .is_some_and(|s| s.is_active() || s.main_pid != 0)
                {
                    return Err(api_err(
                        409,
                        "runtime_not_quiescent",
                        "商店模型进程仍在运行",
                    ));
                }
            }
            let unit = reference
                .get("unit")
                .and_then(Value::as_str)
                .ok_or_else(|| api_err(409, "maintenance_corrupt", "维护记录缺单元引用"))?;
            if app.systemd.card_from_unit(unit).is_none() || unit.contains('/') {
                return Err(api_err(409, "maintenance_corrupt", "单元引用不合法"));
            }
            let card = reference
                .get("card")
                .and_then(Value::as_str)
                .ok_or_else(|| api_err(409, "maintenance_corrupt", "维护记录缺原生卡引用"))?;
            let legacy = reference
                .get("legacy_unit")
                .and_then(Value::as_str)
                .filter(|legacy| *legacy == format!("megacube-model@{card}.service"))
                .ok_or_else(|| api_err(409, "state_unknown", "维护记录缺完整原生运行时证明"))?;
            native_quiescent(app, reference)?;
            let mut units = vec![unit];
            if legacy != unit {
                units.push(legacy);
            }
            for unit in units {
                let state = app
                    .systemd
                    .show(unit)
                    .map_err(|_| api_err(409, "state_unknown", "单元状态不可核实，保持保护"))?;
                if state.main_pid != 0
                    || !["inactive", "failed"].contains(&state.active_state.as_str())
                    || (require_no_autostart
                        && app
                            .systemd
                            .enabled_checked(unit)
                            .map_err(|_| api_err(409, "state_unknown", "不能核实单元自启登记"))?)
                {
                    return Err(api_err(
                        409,
                        "runtime_not_quiescent",
                        "原生模型单元仍活动或保有独立自启",
                    ));
                }
            }
            true
        }
        _ => {
            return Err(api_err(
                409,
                "maintenance_corrupt",
                "维护记录的运行时类型不合法",
            ))
        }
    };
    if stopped {
        Ok(())
    } else {
        Err(api_err(409, "runtime_not_quiescent", "服务仍在运行、过渡中或保有独立自启；请由原管理方停止并撤自启后重试，维护预留不授予停止权限"))
    }
}

const MAX_PROC_ENTRIES: usize = 16_384;

/// A same-account desktop may deny ptrace access to an unrelated executable
/// (user systemd, ssh-agent, sandbox helpers). This is only a negative candidate
/// test: incomplete identity or any supported native launch marker stays unknown.
fn unrelated_process(process: &std::path::Path, reference: &Value) -> std::io::Result<bool> {
    use std::io::{Error, ErrorKind, Read};
    use std::os::unix::fs::OpenOptionsExt;
    let invalid = || Error::new(ErrorKind::InvalidData, "process identity incomplete");
    let read = |name: &str, limit: u64| -> std::io::Result<Vec<u8>> {
        let file = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC)
            .open(process.join(name))?;
        if !file.metadata()?.is_file() {
            return Err(invalid());
        }
        let mut bytes = Vec::new();
        file.take(limit + 1).read_to_end(&mut bytes)?;
        if bytes.len() as u64 > limit {
            return Err(invalid());
        }
        Ok(bytes)
    };
    let generation = || -> std::io::Result<(u32, u64, String)> {
        let bytes = read("stat", 16 * 1024)?;
        let text = std::str::from_utf8(&bytes).map_err(|_| invalid())?;
        let (head, fields) = text.rsplit_once(") ").ok_or_else(invalid)?;
        let (pid, comm) = head.split_once(" (").ok_or_else(invalid)?;
        let pid = pid.parse::<u32>().map_err(|_| invalid())?;
        if Some(pid) != process.file_name().and_then(|p| p.to_str()?.parse().ok()) {
            return Err(invalid());
        }
        let fields: Vec<_> = fields.split_whitespace().collect();
        // Ordinary R/S scheduling changes do not change the generation. A dead
        // or malformed observation cannot exclude an unreadable executable.
        if !fields.first().is_some_and(|state| {
            matches!(*state, "R" | "S" | "D" | "T" | "t" | "I" | "W" | "K" | "P")
        }) {
            return Err(invalid());
        }
        let ticks = fields
            .get(19)
            .and_then(|v| v.parse::<u64>().ok())
            .filter(|v| *v > 0)
            .ok_or_else(invalid)?;
        Ok((pid, ticks, comm.to_owned()))
    };
    let before = generation()?;
    let bytes = read("cmdline", 1024 * 1024)?;
    let text = std::str::from_utf8(bytes.strip_suffix(&[0]).ok_or_else(invalid)?)
        .map_err(|_| invalid())?;
    let args: Vec<_> = text.split('\0').take(16_385).collect();
    if args.first().is_none_or(|arg| arg.is_empty()) || args.len() > 16_384 {
        return Err(invalid());
    }
    let native = before.2 == "tachyon-serve"
        || args.iter().any(|arg| {
            let path = arg.strip_suffix(" (deleted)").unwrap_or(arg);
            std::path::Path::new(path)
                .file_name()
                .is_some_and(|name| name == "tachyon-serve")
                || matches!(
                    arg.split_once('=').map_or(*arg, |(name, _)| name),
                    "--host"
                        | "--port"
                        | "--model-id"
                        | "--max-context"
                        | "--max-concurrency"
                        | "--device-state-slots"
                        | "--host-state-slots"
                )
                || reference
                    .get("native_weights")
                    .and_then(Value::as_str)
                    .is_some_and(|weights| !weights.is_empty() && *arg == weights)
                || reference
                    .get("native_model_id")
                    .and_then(Value::as_str)
                    .is_some_and(|model| !model.is_empty() && *arg == model)
        });
    let after = generation()?;
    Ok(before == after && !native)
}

/// Inspect this account's native engines even before store adoption has written
/// a binding. Neither this scan nor the port probe grants lifecycle ownership.
fn native_quiescent(app: &App, reference: &Value) -> Result<(), ApiErr> {
    use std::io::Read;
    use std::os::unix::fs::MetadataExt;
    let unknown = || api_err(409, "state_unknown", "原生进程或监听状态不可核实，保持保护");
    let active = || api_err(409, "runtime_not_quiescent", "原生模型进程或声明端口仍活动");
    let uid = reference
        .get("native_uid")
        .and_then(Value::as_u64)
        .and_then(|uid| u32::try_from(uid).ok())
        .ok_or_else(unknown)?;
    if app.systemd.model_run_uid().map_err(|_| unknown())? != uid {
        return Err(unknown());
    }
    let port = reference
        .get("native_port")
        .and_then(Value::as_u64)
        .and_then(|port| u16::try_from(port).ok())
        .ok_or_else(unknown)?;
    let model_id = reference
        .get("native_model_id")
        .and_then(Value::as_str)
        .ok_or_else(unknown)?;
    let sha8 = reference
        .get("native_engine_sha8")
        .and_then(Value::as_str)
        .ok_or_else(unknown)?;
    let weights = reference.get("native_weights").and_then(Value::as_str);
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
    for (observed, entry) in std::fs::read_dir(&app.config.proc_root)
        .map_err(|_| unknown())?
        .enumerate()
    {
        // A large process table must not keep maintenance observation pending.
        // Exhausting either bound is unknown, never proof of quiescence.
        if observed >= MAX_PROC_ENTRIES || std::time::Instant::now() >= deadline {
            return Err(unknown());
        }
        let entry = entry.map_err(|_| unknown())?;
        if entry
            .file_name()
            .to_str()
            .is_none_or(|name| name.parse::<u32>().is_err())
        {
            continue;
        }
        let metadata = match entry.metadata() {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(_) => return Err(unknown()),
        };
        if metadata.uid() != uid {
            continue;
        }
        let exe = match std::fs::read_link(entry.path().join("exe")) {
            Ok(exe) => exe,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) if error.kind() == std::io::ErrorKind::PermissionDenied => {
                if unrelated_process(&entry.path(), reference).unwrap_or(false) {
                    continue;
                }
                return Err(unknown());
            }
            Err(_) => return Err(unknown()),
        };
        let executable = exe.to_string_lossy();
        let exe =
            std::path::Path::new(executable.strip_suffix(" (deleted)").unwrap_or(&executable));
        if exe.file_name().is_none_or(|name| name != "tachyon-serve") {
            continue;
        }
        let mut bytes = Vec::new();
        std::fs::File::open(entry.path().join("cmdline"))
            .and_then(|file| file.take(1024 * 1024 + 1).read_to_end(&mut bytes))
            .map_err(|_| unknown())?;
        if bytes.is_empty() || bytes.len() > 1024 * 1024 {
            return Err(unknown());
        }
        let text = std::str::from_utf8(&bytes).map_err(|_| unknown())?;
        let args: Vec<_> = text.split('\0').filter(|arg| !arg.is_empty()).collect();
        let arg = |name: &str| {
            args.iter().enumerate().find_map(|(index, arg)| {
                arg.strip_prefix(name)
                    .and_then(|tail| tail.strip_prefix('='))
                    .or_else(|| {
                        (*arg == name)
                            .then(|| args.get(index + 1).copied())
                            .flatten()
                    })
            })
        };
        let same_engine = exe
            .parent()
            .and_then(|path| path.file_name())
            .is_some_and(|name| name == sha8);
        if (port > 0 && arg("--port").and_then(|value| value.parse::<u16>().ok()) == Some(port))
            || (!model_id.is_empty() && same_engine && arg("--model-id") == Some(model_id))
            || weights.is_some_and(|weights| args.contains(&weights))
        {
            return Err(active());
        }
    }
    if port > 0 {
        let host = reference
            .get("native_host")
            .and_then(Value::as_str)
            .and_then(|host| host.parse::<std::net::IpAddr>().ok())
            .ok_or_else(unknown)?;
        let host = match host {
            std::net::IpAddr::V4(ip) if ip.is_unspecified() => std::net::Ipv4Addr::LOCALHOST.into(),
            std::net::IpAddr::V6(ip) if ip.is_unspecified() => std::net::Ipv6Addr::LOCALHOST.into(),
            host => host,
        };
        match std::net::TcpStream::connect_timeout(
            &std::net::SocketAddr::new(host, port),
            std::time::Duration::from_millis(100),
        ) {
            Ok(_) => return Err(active()),
            Err(error) if error.kind() == std::io::ErrorKind::ConnectionRefused => {}
            Err(_) => return Err(unknown()),
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests;
