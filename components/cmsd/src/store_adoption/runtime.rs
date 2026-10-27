//! Bind an already running store-native process without replacing it. PID start
//! time, executable, weight file, model ID and port are checked together.
use super::*;
use crate::systemd::UnitState;

pub struct Binding {
    pub pid: u32,
    pub running: bool,
    pub boot: bool,
    ticks: u64,
    exe: String,
    weights: String,
    model_id: String,
    port: u16,
    unit: Option<String>,
}
fn path(app: &App, card: &str) -> Result<std::path::PathBuf, String> {
    if !safe_id(card) {
        return Err("invalid_card".into());
    }
    Ok(app
        .config
        .state_dir
        .join("store-runtime")
        .join(format!("{card}.json")))
}
fn ticks(root: &Path, pid: u32) -> Option<u64> {
    std::fs::read_to_string(root.join(pid.to_string()).join("stat"))
        .ok()?
        .rsplit_once(") ")?
        .1
        .split_whitespace()
        .nth(19)?
        .parse()
        .ok()
}

pub fn instance_key(app: &App, pid: u32) -> Option<(u32, u64)> {
    ticks(&app.config.proc_root, pid)
        .filter(|ticks| *ticks > 0)
        .map(|ticks| (pid, ticks))
}
fn validate(app: &App, binding: &Binding) -> Result<bool, String> {
    let process = app.config.proc_root.join(binding.pid.to_string());
    if ticks(&app.config.proc_root, binding.pid) != Some(binding.ticks) {
        return Ok(false);
    }
    let metadata = std::fs::metadata(&process).map_err(|e| e.to_string())?;
    if metadata.uid() != app.systemd.model_run_uid()? {
        return Err("store_runtime_account_changed".into());
    }
    let exe = std::fs::read_link(process.join("exe")).map_err(|e| e.to_string())?;
    if exe != Path::new(&binding.exe) {
        return Err("store_runtime_executable_changed".into());
    }
    let raw = std::fs::read(process.join("cmdline")).map_err(|e| e.to_string())?;
    let args: Vec<&str> = raw
        .split(|b| *b == 0)
        .filter_map(|b| std::str::from_utf8(b).ok())
        .collect();
    let arg = |name| args.windows(2).find(|w| w[0] == name).map(|w| w[1]);
    if !args.contains(&binding.weights.as_str())
        || arg("--model-id") != Some(binding.model_id.as_str())
        || arg("--port").and_then(|v| v.parse::<u16>().ok()) != Some(binding.port)
    {
        return Err("store_runtime_arguments_changed".into());
    }
    Ok(true)
}
pub fn discover(app: &App, card: &Card) -> Result<Option<Binding>, String> {
    let Some(deploy) = &card.deploy else {
        return Ok(None);
    };
    let expected_exe = card
        .engine_bin()
        .ok_or("native_engine_missing")?
        .canonicalize()
        .map_err(|e| e.to_string())?;
    let expected_weights = card
        .weights_path()
        .ok_or("native_weights_missing")?
        .canonicalize()
        .map_err(|e| e.to_string())?;
    let procs: Vec<_> =
        crate::engine::scan_engine_procs(&app.config.proc_root, &app.config.engine_root)
            .into_iter()
            .filter(|p| {
                p.port == Some(deploy.port)
                    && p.model_id.as_deref() == Some(deploy.model_id.as_str())
                    && p.sha8 == card.engine_sha8
            })
            .collect();
    if procs.len() > 1 {
        return Err("store_runtime_ambiguous".into());
    }
    let Some(process) = procs.first() else {
        // A stopped legacy store unit may still have a next-boot preference.
        // Capture and migrate that preference without starting it or changing
        // the shared template. Active but unmatched units are never claimed.
        let current = app.systemd.unit_name_for(&card.name);
        let legacy = format!("megacube-model@{}.service", card.name);
        let mut units = vec![current.clone()];
        if legacy != current {
            units.push(legacy);
        }
        let mut enabled = None;
        for unit in units {
            let state = app.systemd.show(&unit)?;
            if state.main_pid > 0 || !["inactive", "failed"].contains(&state.active_state.as_str())
            {
                return Err("store_runtime_unit_unverified".into());
            }
            let boot_enabled = state.boot_enabled.ok_or("store_runtime_boot_unknown")?;
            if boot_enabled {
                if enabled.is_some() {
                    return Err("store_runtime_ambiguous".into());
                }
                enabled = Some(unit);
            }
        }
        if std::net::TcpStream::connect_timeout(
            &std::net::SocketAddr::from(([127, 0, 0, 1], deploy.port)),
            std::time::Duration::from_millis(100),
        )
        .is_ok()
        {
            return Err("store_runtime_port_unverified".into());
        }
        return Ok(enabled.map(|unit| Binding {
            pid: 0,
            ticks: 0,
            running: false,
            boot: true,
            exe: expected_exe.to_string_lossy().into(),
            weights: expected_weights.to_string_lossy().into(),
            model_id: deploy.model_id.clone(),
            port: deploy.port,
            unit: Some(unit),
        }));
    };
    if process.start_time == 0 {
        return Err("store_runtime_unverified".into());
    }
    let group = std::fs::read_to_string(
        app.config
            .proc_root
            .join(process.pid.to_string())
            .join("cgroup"),
    )
    .map_err(|e| e.to_string())?;
    let unit = group
        .lines()
        .filter_map(|line| line.rsplit('/').next())
        .find(|name| name.ends_with(".service"))
        .map(str::to_owned);
    let mut binding = Binding {
        pid: process.pid,
        ticks: process.start_time,
        running: true,
        boot: false,
        exe: expected_exe.to_string_lossy().into(),
        weights: expected_weights.to_string_lossy().into(),
        model_id: deploy.model_id.clone(),
        port: deploy.port,
        unit,
    };
    if !validate(app, &binding)? {
        return Err("store_runtime_changed".into());
    }
    if let Some(unit) = &binding.unit {
        if !valid_store_unit(unit, &card.name) {
            return Err("store_runtime_foreign_unit".into());
        }
        let state = app.systemd.show(unit)?;
        if state.main_pid != binding.pid || !state.is_active() {
            return Err("store_runtime_unit_changed".into());
        }
        binding.boot = state.boot_enabled.ok_or("store_runtime_boot_unknown")?;
    }
    if !crate::ops::native_deployment_matches(app, card, deploy.port, binding.pid) {
        return Err(
            "native_instance_mismatch: 现有原生实例的完整部署身份未匹配，未获取控制权".into(),
        );
    }
    Ok(Some(binding))
}
fn valid_store_unit(unit: &str, card: &str) -> bool {
    let Some((prefix, instance)) = unit.split_once('@') else {
        return false;
    };
    (prefix == "megacube-model"
        || prefix
            .strip_prefix("megacube-model-")
            .is_some_and(|v| v.len() == 16 && v.bytes().all(|b| b.is_ascii_hexdigit())))
        && instance == format!("{card}.service")
}
pub fn save(app: &App, card: &str, value: &Binding) -> Result<(), String> {
    let file = path(app, card)?;
    crate::private_storage::directory(file.parent().unwrap(), true)?;
    let raw = serde_json::json!({"pid":value.pid,"ticks":value.ticks,"exe":value.exe,"weights":value.weights,"model_id":value.model_id,"port":value.port,"unit":value.unit,"boot":value.boot});
    crate::private_storage::publish(&file, raw.to_string().as_bytes(), true)
}
fn load(app: &App, card: &str) -> Result<Option<Binding>, String> {
    let Some(raw) = crate::private_storage::read_limit(&path(app, card)?, 8192)? else {
        return Ok(None);
    };
    let v: serde_json::Value = serde_json::from_slice(&raw).map_err(|_| "store_runtime_corrupt")?;
    let s = |name| {
        v[name]
            .as_str()
            .map(str::to_owned)
            .ok_or("store_runtime_corrupt".to_string())
    };
    let n = |name| v[name].as_u64().ok_or("store_runtime_corrupt".to_string());
    Ok(Some(Binding {
        pid: u32::try_from(n("pid")?).map_err(|_| "store_runtime_corrupt")?,
        ticks: n("ticks")?,
        exe: s("exe")?,
        weights: s("weights")?,
        model_id: s("model_id")?,
        port: u16::try_from(n("port")?).map_err(|_| "store_runtime_corrupt")?,
        unit: match v.get("unit") {
            Some(serde_json::Value::Null) => None,
            Some(serde_json::Value::String(unit)) => Some(unit.clone()),
            _ => return Err("store_runtime_corrupt".into()),
        },
        boot: v["boot"].as_bool().ok_or("store_runtime_corrupt")?,
        running: true,
    }))
}
const JOURNAL_SOURCE_LIMIT: u64 = 4096;

fn journal_path(app: &App, card: &str) -> Result<std::path::PathBuf, String> {
    // Reuse the lifecycle binding's exact safe-card path validation.
    let binding = path(app, card)?;
    Ok(app
        .config
        .state_dir
        .join("native-journals")
        .join(binding.file_name().unwrap()))
}

/// Outer None means no receipt; Some(None) records a direct process without a
/// systemd journal. This diagnostic receipt is never consumed by control paths.
fn journal_unit(app: &App, card: &str) -> Result<Option<Option<String>>, String> {
    let Some(raw) = crate::private_storage::read_diagnostic_limit(
        &journal_path(app, card)?,
        JOURNAL_SOURCE_LIMIT,
    )?
    else {
        return Ok(None);
    };
    let value = Value::parse(std::str::from_utf8(&raw).map_err(|_| "native_journal_corrupt")?)
        .map_err(|_| "native_journal_corrupt")?;
    if value.as_obj().is_none_or(|value| value.len() != 3)
        || value.get("format").and_then(Value::as_str) != Some("cmsd-native-journal-v1")
        || value.get("card").and_then(Value::as_str) != Some(card)
    {
        return Err("native_journal_corrupt".into());
    }
    match value.get("unit") {
        Some(Value::Null) => Ok(Some(None)),
        Some(Value::Str(unit)) if valid_store_unit(unit, card) => Ok(Some(Some(unit.clone()))),
        _ => Err("native_journal_corrupt".into()),
    }
}

/// Only trusted binding retirement and this daemon's admitted native start call
/// this writer. Persist before retiring lifecycle evidence or starting a new unit.
pub fn record_native_unit(app: &App, card: &str, unit: Option<&str>) -> Result<(), String> {
    if unit.is_some_and(|unit| !valid_store_unit(unit, card)) {
        return Err("store_runtime_foreign_unit".into());
    }
    let file = journal_path(app, card)?;
    crate::private_storage::directory(file.parent().unwrap(), true)?;
    let mut value = Value::obj();
    value.set("format", "cmsd-native-journal-v1".into());
    value.set("card", card.into());
    value.set("unit", unit.map(Value::from).unwrap_or(Value::Null));
    crate::private_storage::publish_diagnostic_limit(
        &file,
        value.to_string().as_bytes(),
        JOURNAL_SOURCE_LIMIT,
    )
}

/// Purge retires only diagnostic provenance, before forgetting registration.
/// Failure retains the lifecycle record so explicit recovery can retry cleanup.
pub fn retire_native_unit(app: &App, card: &str) -> Result<(), String> {
    crate::private_storage::retire_diagnostic(&journal_path(app, card)?)
}

/// Read-only provenance: prefer the existing trusted binding, then its retained
/// journal source. Stopped/exited PIDs do not lose historical journals. No source
/// is permission to restore or stop a runtime, nor a reason to scan other units.
pub fn native_unit_name(app: &App, card: &str) -> Result<Option<String>, String> {
    if let Some(binding) = load(app, card)? {
        if binding
            .unit
            .as_deref()
            .is_some_and(|unit| !valid_store_unit(unit, card))
        {
            return Err("store_runtime_foreign_unit".into());
        }
        return Ok(binding.unit);
    }
    Ok(journal_unit(app, card)?.unwrap_or_else(|| Some(app.systemd.unit_name_for(card))))
}

pub fn native_state(app: &App, card: &str) -> Result<Option<UnitState>, String> {
    let Some(binding) = load(app, card)? else {
        return Ok(None);
    };
    let active = validate(app, &binding)?;
    Ok(Some(UnitState {
        active_state: if active { "active" } else { "inactive" }.into(),
        sub_state: if active { "running" } else { "dead" }.into(),
        main_pid: if active { binding.pid } else { 0 },
        boot_enabled: Some(false),
        checked_at: crate::timeutil::now_epoch(),
        ..Default::default()
    }))
}
/// A vanished process is not proof that its bound unit cannot restart. This
/// check never changes policy or signals a replacement generation.
fn require_unit_quiescent(app: &App, unit: &str, state: &UnitState) -> Result<(), String> {
    if state.main_pid != 0
        || !["inactive", "failed"].contains(&state.active_state.as_str())
        || state.boot_enabled != Some(false)
        || (!state.not_found && !app.systemd.restart_is_disabled(unit)?)
    {
        return Err("store_runtime_recovery_unverified".into());
    }
    Ok(())
}

pub fn fence(app: &App, card: &str) -> Result<(), String> {
    let Some(binding) = load(app, card)? else {
        return Ok(());
    };
    if let Some(unit) = &binding.unit {
        let state = app.systemd.show(unit)?;
        if !valid_store_unit(unit, card) {
            return Err("store_runtime_unit_changed".into());
        }
        let boot_enabled = state.boot_enabled.ok_or("store_runtime_boot_unknown")?;
        if binding.pid > 0 && !validate(app, &binding)? {
            // Losing the observed process does not fence its old unit: it may
            // be waiting to restart or now contain an unverified generation.
            // Never change that generation's policy. Only independent proof
            // that the exact old unit cannot restart permits recovery ready.
            return require_unit_quiescent(app, unit, &state);
        }
        if state.main_pid != binding.pid
            || (binding.pid == 0 && !["inactive", "failed"].contains(&state.active_state.as_str()))
        {
            return Err("store_runtime_unit_changed".into());
        }
        let directory = app.systemd.unit_dir.join(format!("{unit}.d"));
        if crate::systemd::recovery_dropin(&directory)? {
            app.systemd.daemon_reload()?;
        }
        if boot_enabled {
            app.systemd.disable(unit)?;
        }
        if !app.systemd.restart_is_disabled(unit)? {
            return Err("store_runtime_restart_unverified".into());
        }
    }
    Ok(())
}
pub fn stop_native(app: &App, card: &str) -> Result<bool, String> {
    let Some(binding) = load(app, card)? else {
        return Ok(false);
    };
    if validate(app, &binding)? {
        if let Some(unit) = &binding.unit {
            if !valid_store_unit(unit, card) || app.systemd.show(unit)?.main_pid != binding.pid {
                return Err("store_runtime_unit_changed".into());
            }
            app.systemd.stop(unit)?;
        } else {
            if app.config.proc_root != Path::new("/proc") {
                return Err("cannot_signal_fixture_process".into());
            }
            let fd = unsafe { libc::syscall(libc::SYS_pidfd_open, binding.pid as libc::pid_t, 0) };
            if fd < 0 {
                return Err(std::io::Error::last_os_error().to_string());
            }
            use std::os::fd::FromRawFd;
            let handle = unsafe { std::os::fd::OwnedFd::from_raw_fd(fd as i32) };
            if validate(app, &binding)? {
                use std::os::fd::AsRawFd;
                if unsafe {
                    libc::syscall(
                        libc::SYS_pidfd_send_signal,
                        handle.as_raw_fd(),
                        libc::SIGTERM,
                        std::ptr::null::<libc::siginfo_t>(),
                        0,
                    )
                } < 0
                {
                    return Err(std::io::Error::last_os_error().to_string());
                }
            }
        }
        let until = std::time::Instant::now() + std::time::Duration::from_secs(120);
        while validate(app, &binding)? {
            if std::time::Instant::now() >= until {
                return Err("store_runtime_stop_timeout".into());
            }
            std::thread::sleep(std::time::Duration::from_millis(100));
        }
    }
    if let Some(unit) = &binding.unit {
        if !valid_store_unit(unit, card) {
            return Err("store_runtime_unit_changed".into());
        }
        let state = app.systemd.show(unit)?;
        require_unit_quiescent(app, unit, &state)?;
    }
    // Preserve the verified unit's journal before retiring lifecycle authority.
    // A failed write leaves the original binding available for recovery.
    record_native_unit(app, card, binding.unit.as_deref())?;
    std::fs::remove_file(path(app, card)?).map_err(|e| e.to_string())?;
    Ok(true)
}
pub fn try_running_native(app: &Arc<App>, card: &Card) -> Result<Option<String>, String> {
    if load(app, &card.name)?.is_none() {
        return Ok(None);
    }
    let Some(mut state) = native_state(app, &card.name)? else {
        return Ok(None);
    };
    if !state.is_active() {
        if let Some(replacement) = discover(app, card)? {
            save(app, &card.name, &replacement)?;
            fence(app, &card.name)?;
            state = native_state(app, &card.name)?.ok_or("store_runtime_unverified")?;
        }
    }
    if state.is_active() {
        let deploy = card.deploy.as_ref().ok_or("not_deployed")?;
        if !crate::ops::native_instance_matches(app, card, deploy.port, state.main_pid) {
            return Err("native_instance_mismatch: 当前原生实例的有效运行档与商店部署不同，未按同名端口认定成功".into());
        }
        let health = crate::engine::check_health(deploy.port, Some(&deploy.model_id));
        if health.ok {
            let key =
                instance_key(app, state.main_pid).ok_or("native_instance_generation_unverified")?;
            {
                let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
                live.health_instances.insert(deploy.port, key);
                live.health.insert(deploy.port, health);
            }
            crate::ops::upsert_model_state(app, &card.name, |m| {
                m.status = ModelStatus::Running;
                m.main_pid = Some(state.main_pid);
                m.port = deploy.port;
            });
            return Ok(Some(
                "Already running store model retained without restarting".into(),
            ));
        }
    }
    stop_native(app, &card.name)?;
    Ok(None)
}

#[cfg(test)]
mod uid_tests {
    use super::*;
    #[test]
    fn stopped_legacy_unit_is_not_duplicated_but_distinct_enabled_units_are_ambiguous() {
        use std::os::unix::fs::PermissionsExt;
        let root = crate::docker::fixtures::tmpdir("store-stopped-unit-candidates");
        let mut app = crate::docker::fixtures::make_app(&root, 128, 100);
        let config = &mut Arc::get_mut(&mut app).unwrap().config;
        config.proc_root = root.join("proc");
        std::fs::create_dir_all(&config.proc_root).unwrap();
        let engine = config.engine_root.join("01234567");
        std::fs::create_dir_all(&engine).unwrap();
        std::fs::write(engine.join("tachyon-serve"), b"engine").unwrap();
        let weights = root.join("weights");
        std::fs::create_dir_all(&weights).unwrap();
        std::fs::write(weights.join("model.tachyon"), b"model").unwrap();
        let directory = config.cards_dir.join("native");
        std::fs::create_dir_all(&directory).unwrap();
        std::fs::write(
            directory.join("common.sh"),
            "ENGINE_BIN_SHA256=01234567\nMODEL_ID=Native\nARTIFACT=model.tachyon\nTOTAL_BYTES=5\n",
        )
        .unwrap();
        let port = std::net::TcpListener::bind("127.0.0.1:0")
            .unwrap()
            .local_addr()
            .unwrap()
            .port();
        std::fs::write(directory.join(".deploy-env"), format!("PORT={port}\nHOST=127.0.0.1\nMODEL_DIR={}\nENGINE_DIR={}\nARTIFACT=model.tachyon\nMODEL_ID=Native\nMAX_CONCURRENCY=1\n", weights.display(), engine.display())).unwrap();
        let cli = root.join("systemctl");
        let calls = root.join("systemctl.calls");
        std::fs::write(&cli, format!("#!/bin/sh\nprintf '%s\\n' \"$*\" >> '{}'\nprintf 'ActiveState=inactive\\nSubState=dead\\nMainPID=0\\nLoadState=loaded\\nUnitFileState=enabled\\n'\n", calls.display())).unwrap();
        std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
        Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
            app.config.systemd_mode,
            app.config.unit_dir.clone(),
            app.config.env_dir(),
            "test".into(),
        )
        .with_test_cli(&cli);
        let card = crate::ops::find_card(&app, "native").unwrap();
        let binding = discover(&app, &card).unwrap().unwrap();
        assert!(binding.boot);
        assert!(!binding.running);
        assert_eq!(
            binding.unit.as_deref(),
            Some("megacube-model@native.service")
        );
        assert_eq!(std::fs::read_to_string(&calls).unwrap().lines().count(), 1);
        std::fs::write(&calls, "").unwrap();
        Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
            app.config.systemd_mode,
            app.config.unit_dir.clone(),
            app.config.env_dir(),
            "test".into(),
        )
        .with_state_namespace(true)
        .with_test_cli(&cli);
        assert_eq!(
            discover(&app, &card).err().as_deref(),
            Some("store_runtime_ambiguous")
        );
        assert_eq!(std::fs::read_to_string(&calls).unwrap().lines().count(), 2);
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn adopted_process_uid_uses_the_model_execution_account() {
        let root = crate::docker::fixtures::tmpdir("store-binding-execution-uid");
        let mut app = crate::docker::fixtures::make_app(&root, 128, 100);
        let config = &mut Arc::get_mut(&mut app).unwrap().config;
        config.proc_root = root.join("proc");
        let process = config.proc_root.join("412");
        std::fs::create_dir_all(&process).unwrap();
        std::fs::write(
            process.join("stat"),
            format!("412 (tachyon-serve) {} 99", vec!["0"; 19].join(" ")),
        )
        .unwrap();
        std::os::unix::fs::symlink("/fixture/tachyon-serve", process.join("exe")).unwrap();
        std::fs::write(
            process.join("cmdline"),
            b"tachyon-serve\0/weights/model.tachyon\0--model-id\0Native\0--port\x0018199\0",
        )
        .unwrap();
        let binding = Binding {
            pid: 412,
            ticks: 99,
            running: true,
            boot: false,
            exe: "/fixture/tachyon-serve".into(),
            weights: "/weights/model.tachyon".into(),
            model_id: "Native".into(),
            port: 18199,
            unit: None,
        };
        let systemd = &mut Arc::get_mut(&mut app).unwrap().systemd;
        systemd.mode = crate::config::SystemdMode::System;
        systemd.run_user = unsafe { libc::geteuid() }.to_string();
        assert!(validate(&app, &binding).unwrap());
        Arc::get_mut(&mut app).unwrap().systemd.run_user =
            "cmsd-nonexistent-execution-account".into();
        assert!(validate(&app, &binding).is_err());
        if unsafe { libc::geteuid() } != 0 {
            Arc::get_mut(&mut app).unwrap().systemd.run_user = "root".into();
            assert_eq!(
                validate(&app, &binding).unwrap_err(),
                "store_runtime_account_changed"
            );
        }
    }
}

#[cfg(test)]
mod recovery_fence_tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;
    #[test]
    fn stale_bound_unit_matrix_preserves_unknown_and_accepts_proven_stopped() {
        let cases = [
            (
                "waiting-restart",
                "activating",
                0,
                "enabled",
                false,
                "on-failure",
                false,
                false,
            ),
            (
                "replacement-active",
                "active",
                999,
                "enabled",
                false,
                "always",
                false,
                false,
            ),
            (
                "deactivating",
                "deactivating",
                0,
                "disabled",
                false,
                "no",
                false,
                false,
            ),
            (
                "inactive-boot-enabled",
                "inactive",
                0,
                "enabled",
                false,
                "no",
                false,
                false,
            ),
            (
                "inactive-independent-restart",
                "inactive",
                0,
                "disabled",
                false,
                "on-failure",
                false,
                false,
            ),
            (
                "inactive-boot-unknown",
                "inactive",
                0,
                "bad",
                false,
                "no",
                false,
                false,
            ),
            (
                "inactive-proven",
                "inactive",
                0,
                "disabled",
                false,
                "no",
                true,
                false,
            ),
            (
                "failed-proven",
                "failed",
                0,
                "disabled",
                false,
                "no",
                true,
                false,
            ),
            (
                "missing-unit",
                "inactive",
                0,
                "not-found",
                true,
                "bad",
                true,
                false,
            ),
            (
                "stopped-binding-pending",
                "activating",
                0,
                "enabled",
                false,
                "always",
                false,
                true,
            ),
            (
                "stopped-binding-boot-unknown",
                "inactive",
                0,
                "bad",
                false,
                "no",
                false,
                true,
            ),
            (
                "stopped-binding-boot-off",
                "inactive",
                0,
                "disabled",
                false,
                "no",
                true,
                true,
            ),
            (
                "stopped-binding-retained",
                "inactive",
                0,
                "enabled",
                false,
                "no",
                true,
                true,
            ),
        ];
        for (tag, state, main_pid, boot, missing, restart, success, stopped_binding) in cases {
            let root = crate::docker::fixtures::tmpdir(&format!("native-fence-{tag}"));
            let mut app = crate::docker::fixtures::make_app(&root, 128, 100);
            Arc::get_mut(&mut app).unwrap().config.proc_root = root.join("proc");
            std::fs::create_dir_all(&app.config.proc_root).unwrap();
            let unit = "megacube-model@native.service";
            let cli = root.join("systemctl");
            let calls = root.join("calls");
            std::fs::write(
                &cli,
                format!(
                    r#"#!/bin/sh
printf '%s\n' "$*" >> '{}'
case "$*" in
  *'--property=Restart --value'*) printf '{}\n';;
  *'show'*) printf 'ActiveState={}\nSubState=dead\nMainPID={}\nLoadState={}\nUnitFileState={}\n';;
esac
exit 0
"#,
                    calls.display(),
                    restart,
                    state,
                    main_pid,
                    if missing { "not-found" } else { "loaded" },
                    boot
                ),
            )
            .unwrap();
            std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
            Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
                app.config.systemd_mode,
                app.config.unit_dir.clone(),
                app.config.env_dir(),
                "test".into(),
            )
            .with_state_namespace(true)
            .with_test_cli(&cli);
            save(
                &app,
                "native",
                &Binding {
                    pid: if stopped_binding { 0 } else { 412 },
                    ticks: 99,
                    running: !stopped_binding,
                    boot: true,
                    exe: "/fixture/tachyon-serve".into(),
                    weights: "/fixture/model.tachyon".into(),
                    model_id: "Native".into(),
                    port: 18199,
                    unit: Some(unit.into()),
                },
            )
            .unwrap();
            let before = std::fs::read(path(&app, "native").unwrap()).unwrap();
            if !stopped_binding || boot != "enabled" {
                let stop_success = success;
                let stop_result = stop_native(&app, "native");
                assert_eq!(
                    stop_result.is_ok(),
                    stop_success,
                    "{tag}: stop {stop_result:?}"
                );
                if stop_success {
                    assert!(
                        matches!(std::fs::symlink_metadata(path(&app,"native").unwrap()),Err(error) if error.kind()==std::io::ErrorKind::NotFound)
                    );
                    save(
                        &app,
                        "native",
                        &Binding {
                            pid: if stopped_binding { 0 } else { 412 },
                            ticks: 99,
                            running: !stopped_binding,
                            boot: true,
                            exe: "/fixture/tachyon-serve".into(),
                            weights: "/fixture/model.tachyon".into(),
                            model_id: "Native".into(),
                            port: 18199,
                            unit: Some(unit.into()),
                        },
                    )
                    .unwrap();
                } else {
                    assert_eq!(
                        before,
                        std::fs::read(path(&app, "native").unwrap()).unwrap(),
                        "{tag}: failed stop discarded binding"
                    );
                }
                println!("STOP MATRIX {tag}: {stop_result:?}");
            }
            let result = fence(&app, "native");
            assert_eq!(result.is_ok(), success, "{tag}: {result:?}");
            assert_eq!(
                before,
                std::fs::read(path(&app, "native").unwrap()).unwrap(),
                "{tag}: binding changed"
            );
            let log = std::fs::read_to_string(&calls).unwrap();
            assert!(!log.contains(" stop "), "{tag}: stopped a generation");
            if !success || !stopped_binding {
                assert!(
                    !log.contains("disable"),
                    "{tag}: changed unknown boot policy"
                );
                assert!(
                    !app.systemd.unit_dir.join(format!("{unit}.d")).exists(),
                    "{tag}: wrote unknown policy"
                );
            }
            println!("FENCE MATRIX {tag}: {result:?}");
            std::fs::remove_dir_all(root).unwrap();
        }
    }
    #[test]
    fn stopped_discovery_must_not_hide_transitions_or_unknown_boot_policy() {
        for (tag, state, boot, success) in [
            ("pending-restart", "activating", "disabled", false),
            ("stopping", "deactivating", "disabled", false),
            ("unknown-boot", "inactive", "bad", false),
            ("ordinary-stopped", "inactive", "disabled", true),
            ("ordinary-failed", "failed", "disabled", true),
        ] {
            let root = crate::docker::fixtures::tmpdir(&format!("native-discover-{tag}"));
            let mut app = crate::docker::fixtures::make_app(&root, 128, 100);
            Arc::get_mut(&mut app).unwrap().config.proc_root = root.join("proc");
            std::fs::create_dir_all(&app.config.proc_root).unwrap();
            let engine = root.join("engine");
            std::fs::create_dir_all(&engine).unwrap();
            std::fs::write(engine.join("tachyon-serve"), b"engine").unwrap();
            let weights = root.join("weights");
            std::fs::create_dir_all(&weights).unwrap();
            std::fs::write(weights.join("model.tachyon"), b"model").unwrap();
            let dir = app.config.cards_dir.join("native");
            std::fs::create_dir_all(&dir).unwrap();
            std::fs::write(dir.join("common.sh"),"ENGINE_BIN_SHA256=01234567\nMODEL_ID=Native\nARTIFACT=model.tachyon\nTOTAL_BYTES=5\n").unwrap();
            let port = std::net::TcpListener::bind("127.0.0.1:0")
                .unwrap()
                .local_addr()
                .unwrap()
                .port();
            std::fs::write(dir.join(".deploy-env"),format!("PORT={port}\nHOST=127.0.0.1\nMODEL_DIR={}\nENGINE_DIR={}\nARTIFACT=model.tachyon\nMODEL_ID=Native\nMAX_CONCURRENCY=1\n",weights.display(),engine.display())).unwrap();
            let cli = root.join("systemctl");
            std::fs::write(&cli,format!("#!/bin/sh\nprintf 'ActiveState={state}\\nSubState=dead\\nMainPID=0\\nLoadState=loaded\\nUnitFileState={boot}\\n'\n")).unwrap();
            std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
            Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
                app.config.systemd_mode,
                app.config.unit_dir.clone(),
                app.config.env_dir(),
                "test".into(),
            )
            .with_state_namespace(true)
            .with_test_cli(&cli);
            let card = crate::ops::find_card_raw(&app, "native").unwrap();
            let result = discover(&app, &card);
            assert_eq!(result.is_ok(), success, "{tag}");
            println!(
                "DISCOVER MATRIX {tag}: {}",
                match result {
                    Ok(None) => "absent".to_owned(),
                    Ok(Some(_)) => "bound".to_owned(),
                    Err(e) => e,
                }
            );
            std::fs::remove_dir_all(root).unwrap();
        }
    }
}

#[cfg(test)]
mod unit_projection_tests {
    use super::*;

    fn fixture(tag: &str) -> (std::path::PathBuf, Arc<App>) {
        let root = crate::docker::fixtures::tmpdir(tag);
        let mut app = crate::docker::fixtures::make_app(&root, 128, 100);
        Arc::get_mut(&mut app).unwrap().config.proc_root = root.join("proc");
        Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
            app.config.systemd_mode,
            app.config.unit_dir.clone(),
            app.config.env_dir(),
            "test".into(),
        )
        .with_state_namespace(true);
        (root, app)
    }

    fn binding(unit: Option<&str>, pid: u32) -> Binding {
        Binding {
            pid,
            ticks: 99,
            running: pid > 0,
            boot: false,
            exe: "/fixture/tachyon-serve".into(),
            weights: "/weights/model.tachyon".into(),
            model_id: "Native".into(),
            port: 18199,
            unit: unit.map(str::to_owned),
        }
    }

    #[test]
    fn journal_unit_uses_retained_supported_binding_and_keeps_standalone_and_process_distinct() {
        let (root, app) = fixture("native-journal-provenance");
        assert_eq!(
            native_unit_name(&app, "native").unwrap(),
            Some(app.systemd.unit_name_for("native"))
        );
        for unit in [
            "megacube-model@native.service",
            "megacube-model-0123456789abcdef@native.service",
        ] {
            // Stopped bindings are also saved by read-only discovery. Retain
            // their journal provenance without a backend query or fresh start.
            save(&app, "native", &binding(Some(unit), 0)).unwrap();
            let before = std::fs::read(path(&app, "native").unwrap()).unwrap();
            assert_eq!(
                native_unit_name(&app, "native").unwrap().as_deref(),
                Some(unit)
            );
            assert_eq!(
                std::fs::read(path(&app, "native").unwrap()).unwrap(),
                before
            );
        }
        for unit in [
            "ssh.service",
            "megacube-model@other.service",
            "megacube-model-bad@native.service",
            "../megacube-model@native.service",
        ] {
            save(&app, "native", &binding(Some(unit), 0)).unwrap();
            assert_eq!(
                native_unit_name(&app, "native").unwrap_err(),
                "store_runtime_foreign_unit"
            );
        }
        save(&app, "native", &binding(None, 0)).unwrap();
        assert_eq!(native_unit_name(&app, "native").unwrap(), None);
        std::fs::write(path(&app, "native").unwrap(), b"broken").unwrap();
        assert!(native_unit_name(&app, "native").is_err());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn exited_binding_keeps_history_and_malformed_unit_is_unknown() {
        let (root, app) = fixture("native-journal-exited-binding");
        let unit = "megacube-model@native.service";
        save(&app, "native", &binding(Some(unit), 412)).unwrap();
        // No fixture process or systemctl exists. A dead/reused PID is not a
        // reason to lose the adopted unit's historical journal.
        assert!(!app.config.proc_root.exists());
        assert_eq!(
            native_unit_name(&app, "native").unwrap().as_deref(),
            Some(unit)
        );
        let file = path(&app, "native").unwrap();
        let original = std::fs::read(&file).unwrap();
        for value in [serde_json::json!(42), serde_json::json!({})] {
            let mut record: serde_json::Value = serde_json::from_slice(&original).unwrap();
            record["unit"] = value;
            std::fs::write(&file, record.to_string()).unwrap();
            assert_eq!(
                native_unit_name(&app, "native").unwrap_err(),
                "store_runtime_corrupt"
            );
        }
        let mut record: serde_json::Value = serde_json::from_slice(&original).unwrap();
        record.as_object_mut().unwrap().remove("unit");
        std::fs::write(&file, record.to_string()).unwrap();
        assert_eq!(
            native_unit_name(&app, "native").unwrap_err(),
            "store_runtime_corrupt"
        );
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn stopped_binding_keeps_journal_then_current_start_and_purge_retire_it() {
        use std::os::unix::fs::PermissionsExt;
        let (root, mut app) = fixture("native-journal-stop-restart-purge");
        let cli = root.join("systemctl");
        std::fs::write(&cli, "#!/bin/sh\nprintf 'ActiveState=inactive\nSubState=dead\nMainPID=0\nLoadState=not-found\nUnitFileState=disabled\n'\n").unwrap();
        std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
        Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
            app.config.systemd_mode,
            app.config.unit_dir.clone(),
            app.config.env_dir(),
            "test".into(),
        )
        .with_state_namespace(true)
        .with_test_cli(&cli);
        let legacy = "megacube-model@native.service";
        save(&app, "native", &binding(Some(legacy), 412)).unwrap();
        assert!(stop_native(&app, "native").unwrap());
        assert!(!path(&app, "native").unwrap().exists());
        assert_eq!(
            native_unit_name(&app, "native").unwrap().as_deref(),
            Some(legacy)
        );
        assert!(
            native_state(&app, "native").unwrap().is_none(),
            "diagnostic receipt cannot restore lifecycle binding"
        );
        let current = app.systemd.unit_name_for("native");
        record_native_unit(&app, "native", Some(&current)).unwrap();
        assert_eq!(
            native_unit_name(&app, "native").unwrap(),
            Some(current.clone())
        );
        retire_native_unit(&app, "native").unwrap();
        assert!(!journal_path(&app, "native").unwrap().exists());
        assert_eq!(native_unit_name(&app, "native").unwrap(), Some(current));
        save(&app, "native", &binding(None, 412)).unwrap();
        assert!(stop_native(&app, "native").unwrap());
        assert_eq!(native_unit_name(&app, "native").unwrap(), None);
        retire_native_unit(&app, "native").unwrap();
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn diagnostic_write_failure_keeps_lifecycle_binding_and_bad_receipt_stays_unknown() {
        use std::os::unix::fs::PermissionsExt;
        let (root, app) = fixture("native-journal-write-failure");
        save(&app, "native", &binding(None, 412)).unwrap();
        let binding_file = path(&app, "native").unwrap();
        let before = std::fs::read(&binding_file).unwrap();
        let source = journal_path(&app, "native").unwrap();
        crate::private_storage::directory(source.parent().unwrap(), true).unwrap();
        std::fs::create_dir(&source).unwrap();
        assert!(stop_native(&app, "native").is_err());
        assert_eq!(std::fs::read(&binding_file).unwrap(), before);
        std::fs::remove_dir(&source).unwrap();
        std::fs::remove_file(&binding_file).unwrap();
        std::fs::write(&source, b"broken").unwrap();
        std::fs::set_permissions(&source, std::fs::Permissions::from_mode(0o600)).unwrap();
        assert!(native_unit_name(&app, "native").is_err());
        retire_native_unit(&app, "native").unwrap();
        assert!(!source.exists());
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn public_disable_replaces_corrupt_private_source_and_retires_exited_binding() {
        use std::os::unix::fs::PermissionsExt;
        for raw in [
            b"broken".to_vec(),
            vec![b'x'; 4097],
            br#"{"format":"cmsd-native-journal-v1","card":"other","unit":"ssh.service"}"#.to_vec(),
        ] {
            let (root, mut app) = fixture("native-journal-public-disable");
            let cli = root.join("systemctl");
            std::fs::write(&cli, "#!/bin/sh\nprintf 'ActiveState=inactive\nSubState=dead\nMainPID=0\nLoadState=not-found\nUnitFileState=disabled\n'\n").unwrap();
            std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
            Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
                app.config.systemd_mode,
                app.config.unit_dir.clone(),
                app.config.env_dir(),
                "test".into(),
            )
            .with_state_namespace(true)
            .with_test_cli(&cli);
            app.systemd.write_template().unwrap();
            crate::ops::upsert_model_state(&app, "native", |model| model.lifecycle_owned = true);
            save(&app, "native", &binding(None, 412)).unwrap();
            record_native_unit(&app, "native", None).unwrap();
            let source = journal_path(&app, "native").unwrap();
            std::fs::write(&source, raw).unwrap();
            let task = crate::ops::disable(&app, "native").unwrap();
            let outcome = crate::docker::fixtures::wait_terminal(&app, &task, 5);
            assert_eq!(
                outcome.status,
                crate::store::TaskStatus::Succeeded,
                "{:?}",
                outcome.error
            );
            assert!(!path(&app, "native").unwrap().exists());
            assert_eq!(native_unit_name(&app, "native").unwrap(), None);
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn stop_keeps_binding_when_diagnostic_path_is_linked_or_nonprivate() {
        use std::os::unix::fs::PermissionsExt;
        for linked in [true, false] {
            let (root, app) = fixture("native-journal-stop-untrusted");
            save(&app, "native", &binding(None, 412)).unwrap();
            let binding_file = path(&app, "native").unwrap();
            let before = std::fs::read(&binding_file).unwrap();
            let source = journal_path(&app, "native").unwrap();
            crate::private_storage::directory(source.parent().unwrap(), true).unwrap();
            let target = root.join("original");
            std::fs::write(&target, b"preserve").unwrap();
            if linked {
                std::os::unix::fs::symlink(&target, &source).unwrap();
            } else {
                std::fs::write(&source, b"preserve").unwrap();
                std::fs::set_permissions(&source, std::fs::Permissions::from_mode(0o644)).unwrap();
            }
            assert!(stop_native(&app, "native").is_err());
            assert_eq!(std::fs::read(&binding_file).unwrap(), before);
            assert_eq!(std::fs::read(&target).unwrap(), b"preserve");
            assert!(std::fs::symlink_metadata(source).is_ok());
            std::fs::remove_dir_all(root).unwrap();
        }
    }
}
