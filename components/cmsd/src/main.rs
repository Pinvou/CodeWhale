//! cmsd：Cube Manage Software 守护进程。
//!
//! MegaCube 模型服务管理面：unix socket API（D1 最小面）+ 薄网关（D0）+
//! systemd 守护层（D3/D4）+ 状态库与对账（T1）。

mod api;
mod app;
mod capacity;
mod cards;
mod component;
mod config;
mod container_runtime;
mod device;
mod diagnostics;
mod docker;
mod docker_guard;
mod engine;
mod gateway;
mod http;
mod install;
mod json;
mod lifecycle_revision;
mod limits;
mod model_maintenance;
mod network;
mod ops;
mod peer;
mod private_storage;
mod process;
mod recovery;
mod redact;
mod shop;
mod startup;
mod store;
mod store_adoption;
mod supervisor;
mod systemd;
mod timeutil;

use app::{log_line, App};
use config::Config;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::UnixListener;
use std::path::Path;
use std::sync::Arc;

fn main() {
    // Component inspection must finish before reading config or opening state,
    // sockets and model services. Installers can validate an offline artifact.
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.first().map(String::as_str) == Some("--docker-recovery-guard") {
        if let Err(message) = docker_guard::run(&args[1..]) {
            eprintln!("cmsd Docker guard: {message}");
        }
        std::process::exit(1);
    }
    if args == ["--version"] {
        println!("cmsd {}", env!("CARGO_PKG_VERSION"));
        return;
    }
    if args == ["--component-info"] {
        let mut info = crate::json::Value::obj();
        info.set("component", "cmsd".into());
        info.set("version", env!("CARGO_PKG_VERSION").into());
        info.set("os", std::env::consts::OS.into());
        info.set("architecture", std::env::consts::ARCH.into());
        info.set("update_contract", 1u64.into());
        info.set("authority_scope", "local-v1".into());
        info.set("native_unit_namespace", 1u64.into());
        info.set("native_profile_budget", 1u64.into());
        info.set("model_boot_policy", 1u64.into());
        info.set("store_adoption", 1u64.into());
        info.set("store_selection", 1u64.into());
        info.set("native_profile_from_card", 1u64.into());
        info.set("state_format", "cmsd-json-v1".into());
        info.set("api", "v1".into());
        info.set(
            "build_revision",
            option_env!("CMSD_BUILD_REVISION")
                .unwrap_or("development")
                .into(),
        );
        println!("{info}");
        return;
    }
    let check_config = args == ["--check-config"];
    let diagnose = args == ["--diagnose"];
    if !args.is_empty() && !check_config && !diagnose {
        eprintln!("usage: cmsd [--version|--component-info|--check-config|--diagnose]");
        std::process::exit(2);
    }
    let config = match Config::load() {
        Ok(config) => config,
        Err(error) => {
            eprintln!("cmsd 配置无效，未打开状态或监听：{error}");
            std::process::exit(2);
        }
    };
    // A diagnostic client only sends fixed local GETs. Never open the daemon
    // state or require a model execution account just to inspect a system unit.
    if diagnose {
        let (report, code) = diagnostics::run(&config);
        println!("{report}");
        std::process::exit(code);
    }
    if config.systemd_mode == config::SystemdMode::System
        && std::env::var("CMSD_RUN_USER")
            .unwrap_or_default()
            .is_empty()
    {
        eprintln!(
            "system 模式必须显式配置 CMSD_RUN_USER 及桌面用户资源路径，不能以 root HOME 推断"
        );
        std::process::exit(2);
    }
    if check_config {
        println!(
            "{}",
            serde_json::json!({"configuration_valid":true,"read_only":true,
            "systemd_mode":config.systemd_mode.as_str(),"native_unit_namespace":config.native_unit_namespace,
            "state_dir":config.state_dir,"socket_path":config.socket_path,
            "store_socket_path":config.store_socket_path,"unit_dir":config.unit_dir,"cards_dir":config.cards_dir,
            "models_dir":config.models_dir,"remote_pairing":false,
            "runtime_access_verified":false,"note":"Configuration only; no state, locks, listeners, model commands or trust changes"})
        );
        return;
    }
    if let Err(e) = startup::check_supported_state(&config.state_dir) {
        eprintln!("状态包含本单机版本不支持的控制授权，请使用完整兼容组件：{e}");
        std::process::exit(2);
    }
    if let Err(e) = private_storage::secure_state(&config.state_dir) {
        eprintln!("状态目录无法安全使用 {}：{e}", config.state_dir.display());
        std::process::exit(1);
    }
    let _state_lock = match startup::lock(&config.state_dir.join("daemon.lock")) {
        Ok(lock) => lock,
        Err(error) => {
            eprintln!("状态目录已有实例或锁不可核实，保留原状态：{error}");
            std::process::exit(1);
        }
    };
    if let Err(e) = std::fs::create_dir_all(config.env_dir()) {
        eprintln!("创建 env 目录失败：{e}");
        std::process::exit(1);
    }

    // API 监听（unix socket，线程每连接）
    let socket_path = config.socket_path.clone();
    if let Some(dir) = Path::new(&socket_path).parent() {
        if let Err(e) = std::fs::create_dir_all(dir) {
            log_line(&format!("创建 socket 目录 {} 失败：{e}", dir.display()));
            std::process::exit(1);
        }
    }
    let _socket_claim = match startup::socket_claim(&socket_path) {
        Ok(lock) => lock,
        Err(error) => {
            eprintln!("socket 已被声明或其锁不可核实，保留原路径：{error}");
            std::process::exit(1);
        }
    };
    if let Err(error) = startup::prepare_socket(&socket_path) {
        eprintln!("socket {} 不能安全使用：{error}", socket_path.display());
        std::process::exit(1);
    }
    let listener = match UnixListener::bind(&socket_path) {
        Ok(l) => l,
        Err(e) => {
            log_line(&format!("绑定 socket {} 失败：{e}", socket_path.display()));
            std::process::exit(1);
        }
    };
    let store = match store::StateStore::try_load(&config.state_file()) {
        Ok(store) => store,
        Err(e) => {
            eprintln!(
                "状态库 {} 无法安全加载：{e}；保留原文件，请从备份恢复后重试",
                config.state_file().display()
            );
            std::process::exit(1);
        }
    };
    let run_user = std::env::var("CMSD_RUN_USER")
        .or_else(|_| std::env::var("USER"))
        .unwrap_or_default();
    let systemd = systemd::Systemd::new(
        config.systemd_mode,
        config.unit_dir.clone(),
        config.env_dir(),
        run_user,
    )
    .with_state_namespace(config.native_unit_namespace == "state");
    if let Err(error) = systemd.claim_namespace(
        &config.state_dir,
        store
            .state
            .models
            .values()
            .any(|model| model.engine_type == "native" && model.lifecycle_owned),
    ) {
        eprintln!("cmsd model unit namespace refused: {error}");
        std::process::exit(1);
    }
    // 模板单元就位（D3）。权限不够（生产需要 root）时降级为警告——
    // enable 时会再试并以明确错误失败。
    match systemd.write_template() {
        Ok(true) => {
            log_line("模板单元已生成/更新");
            if let Err(e) = systemd.daemon_reload() {
                log_line(&format!("daemon-reload 失败：{e}"));
            }
        }
        Ok(false) => {}
        Err(e) => log_line(&format!("生成模板单元失败（{e}），enable 时会重试")),
    }

    let app = Arc::new(App::new(config, store, systemd));

    if let Err((_, code, message)) = device::inventory(&app) {
        eprintln!("{code}: {message}");
        std::process::exit(1);
    }
    if let Err((_, code, message)) = lifecycle_revision::initialize(&app) {
        if code == "identity_busy" {
            // As with the preceding device observation, retry startup before
            // starting reconciliation, recovery or request acceptance with pending state.
            eprintln!("{code}: {message}");
            std::process::exit(1);
        }
        log_line(&format!(
            "控制版本不可用：{code}: {message}；模型变更已封锁，本机只读与救援接口继续可用"
        ));
    }
    // Confirm the current store pointer and fence archives before boot recovery.
    store_adoption::reconcile(&app);
    recovery::migrate(&app);
    // T1 启动对账：不信状态库，先核现实
    let notes = supervisor::reconcile_once(&app);
    if let Err((_, code, message)) = model_maintenance::recover_receipts(&app) {
        log_line(&format!("模型维护恢复需人工核查：{code}: {message}"));
    }
    log_line(&format!(
        "启动对账完成：{notes}（{} 模式，socket {}）",
        app.config.systemd_mode.as_str(),
        app.config.socket_path.display()
    ));

    install::recover(&app);
    recovery::tick(&app);

    // 守护线程：单元观测、野生收编、健康探测
    {
        let app2 = Arc::clone(&app);
        std::thread::spawn(move || supervisor::run(app2));
    }
    // D0 薄网关线程
    {
        let app2 = Arc::clone(&app);
        std::thread::spawn(move || gateway::run(app2));
    }

    // 本机访问控制 = Unix 文件权限（设计 §6.3）：owner+group 可写
    let _ = std::fs::set_permissions(&socket_path, std::fs::Permissions::from_mode(0o660));
    log_line(&format!(
        "cmsd {} 管理API就绪：socket {}，网关配置 {}（实际监听见gateway_status）",
        env!("CARGO_PKG_VERSION"),
        socket_path.display(),
        app.config.gateway_listen
    ));

    for stream in listener.incoming() {
        match stream {
            Ok(s) => {
                let Some(permit) = app.connections.acquire() else {
                    let mut s = s;
                    let _ = s.set_write_timeout(Some(std::time::Duration::from_secs(1)));
                    let _ = http::write_response(&mut s, "503 Service Unavailable", "application/json",
                        b"{\"error\":{\"code\":\"connection_limit\",\"message\":\"device API busy\"}}", &[]);
                    continue;
                };
                let app2 = Arc::clone(&app);
                std::thread::spawn(move || {
                    let _permit = permit;
                    api::handle_conn(&app2, s);
                });
            }
            Err(e) => log_line(&format!("accept 失败：{e}")),
        }
    }
}
