//! Pinvou Agent Tauri 后端入口。

mod app;
mod core;
pub mod features;
pub mod platform;

pub use app::commands::attachments::build_message_with_attachments;

use tauri::Manager;

use crate::app::commands;
use crate::features::{
    assistant::{engine_pool::EnginePool, platform::bridge},
    connectors::{connector_cli, eip, zhidao},
    files::file_watcher,
    knowledge,
    monitor::MonitorState,
    pet::{pet_window, selected_pet},
    remote_control::RemoteControlManager,
    remote_knowledge::RemoteKnowledgeService,
    scheduled::tasks as scheduled_tasks,
    sessions::SessionStore,
};
use crate::platform::{notifications, startup, telemetry};

const RELEASE_ENV_DEFAULTS: &[(&str, &str)] = &[
    // —— vLLM 后端：BASE_URL/MODEL/API_KEY 已在 bridge/mod.rs 有默认常量，
    // 这里只补 run-dev.sh 额外注入但 Rust 没默认的 ——
    // ⚠️ 不再注入 DEEPSEEK_PROVIDER：它会被 bridge.provider() 当成 env 覆盖
    //   （env 优先级高于 preset），在「添加模型」多 provider 方案下钉死路由——
    //   切到 kimi/openai/qwen 等仍被当 vllm，且设置页误报「环境变量已锁定 provider」。
    //   provider 现由 active_model.preset 决定（LocalVllm→vllm 默认仍成立）。
    ("DEEPSEEK_ALLOW_INSECURE_HTTP", "1"),
    ("DEEPSEEK_FORCE_HTTP1", "1"),
    // 不再注入 DEEPSEEK_MAX_OUTPUT_TOKENS：它会把所有模型（含云端）的输出上限
    // 钉死在 24576。云端模型应落到底座请求上限与厂商能力兜底；本地 vLLM 的
    // 24K 预算由 route_limits_for_model 的 is_local_vllm 分支显式携带。
    // 与 CodeWhale 的 stream_chunk_timeout 默认值保持一致。
    ("DEEPSEEK_STREAM_IDLE_TIMEOUT_SECS", "300"),
    // SSE 首响应头超时(open timeout):底座只认 env,默认 45s 是为云端调的。
    // 本地 GB10 大上下文 SubAgent 请求首 token TTFT 偶发 >45s → 误杀子 agent。
    // 280s 与
    // ~/.deepseek config 的 subagent api_timeout=300 对齐(步级超时须更大)。
    ("DEEPSEEK_STREAM_OPEN_TIMEOUT_SECS", "280"),
];

/// 为 release 安装包（.deb 双击启动场景）注入 run-dev.sh 里集中处理的运行时 env。
/// dev 启动走 run-dev.sh 已经 export 过的不会被覆盖（var_os().is_none() 守门）。
fn ensure_release_env() {
    use std::env;
    crate::platform::ui_cache::configure_runtime_environment();
    for (k, v) in RELEASE_ENV_DEFAULTS {
        if env::var_os(k).is_none() {
            env::set_var(k, v);
        }
    }

    // IT 内部 CLI 业务调用走 bin 里的包装脚本(`eip`/`zhidao` 等,内部注入 AGENT_*)——因为模型
    // shell 工具的环境是白名单消毒过的,AGENT_DEVICE_ID/CREDENTIALS_DIR/NON_INTERACTIVE
    // 直接继承会被过滤掉(见 eip.rs 顶注 + EIP 接入方案 §2.5)。PATH 在白名单内可继承,
    // 故把 `~/.pinvou3/bundle/skills/{eip,zhidao}/bin` 前插进程 PATH,模型即可像 lark-cli 一样
    // 直接调用。目录此刻可能尚未解包,但 PATH 含不存在目录无害。
    {
        let skills_dir = crate::platform::paths::bundle_skills_dir();
        if let Some(old) = env::var_os("PATH") {
            let mut dirs = vec![
                skills_dir.join("eip").join("bin"),
                skills_dir.join("zhidao").join("bin"),
            ];
            if let Some(connector_bin) = crate::platform::paths::managed_connector_bin_dir() {
                dirs.push(connector_bin);
            }
            if let Ok(prefix) = env::var("NPM_CONFIG_PREFIX") {
                dirs.push(std::path::Path::new(&prefix).join("bin"));
            }
            if let Some(home) = env::var_os("HOME") {
                let home = std::path::Path::new(&home);
                dirs.push(home.join(".npm-global").join("bin"));
                dirs.push(home.join(".local").join("bin"));
            }
            #[cfg(target_os = "macos")]
            {
                dirs.push(std::path::PathBuf::from("/opt/homebrew/bin"));
                dirs.push(std::path::PathBuf::from("/usr/local/bin"));
                dirs.push(std::path::PathBuf::from(
                    "/Applications/LibreOffice.app/Contents/MacOS",
                ));
            }
            dirs.extend(env::split_paths(&old));
            if let Ok(joined) = env::join_paths(dirs) {
                env::set_var("PATH", joined);
            }
        }
    }
}

/// 进程级选定并安装 rustls 的 CryptoProvider(aws-lc-rs)。
///
/// 幂等:`install_default` 返回 `Err` 表示已装过(本次或之前的 reqwest/tungstenite 调用),
/// 用 `drop` 吞掉即可,不会二次注册。装在 `run()` 最前,保证后续 reqwest / relay 的
/// `connect_async` 都用已确定的 provider,避开「aws-lc-rs + ring 同时启用时无参
/// ClientConfig::builder().expect() panic」(见 Cargo.toml rustls 注释)。
fn install_rustls_provider() {
    drop(rustls::crypto::aws_lc_rs::default_provider().install_default());
}

/// `iframe[srcdoc]` 在 WebKitGTK 中会作为宿主 WebView 的 `about:srcdoc` 导航
/// 进入 Wry 的 navigation handler。这里只放行浏览器内部的两个空文档地址；
/// 主窗口和 iframe 的任意外部来源仍走初始 origin 限制。
#[cfg(any(target_os = "linux", test))]
fn allow_embedded_document_navigation(url: &tauri::Url, main_origin_initialized: bool) -> bool {
    main_origin_initialized && matches!(url.as_str(), "about:blank" | "about:srcdoc")
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    // 必须最先执行:进程级选定 rustls CryptoProvider。
    // 见 Cargo.toml 的 rustls/reqwest 注释——reqwest 0.13 自带 aws-lc-rs 但只「借用」
    // provider,不写入默认槽;而 oauth2(经 reqwest 0.12)把 rustls 0.23 的 `ring` feature
    // 也拉进图,使 rustls 同时启用 aws-lc-rs + ring 两个 provider。tokio-tungstenite 0.30
    // 的 TLS 连接器走无参 ClientConfig::builder(),两 provider 共存时会 panic。此处选定
    // aws-lc-rs(FIPS 可候选、性能优于 ring),在任何 TLS 连接之前装好。
    install_rustls_provider();
    ensure_release_env();
    startup::init();
    startup::mark("environment:ready");
    // 必须早于 Tauri Builder/WebView 创建：避免升级后 WebKit 复用旧 index.html，
    // 却在新包内找不到旧 CSS，退化成裸 HTML 页面。
    crate::platform::ui_cache::migrate_before_webview();
    let initial_navigation_reported =
        std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let main_navigation_origin = std::sync::Arc::new(std::sync::Mutex::new(None::<String>));
    let builder = tauri::Builder::default()
        // These no-op probe plugins bracket Tauri's own plugin initialization.
        // The main window is created by Tauri before the application setup hook,
        // so their lifecycle hooks expose time that was previously one opaque gap.
        .plugin({
            let initial_navigation_reported = initial_navigation_reported.clone();
            let main_navigation_origin = main_navigation_origin.clone();
            tauri::plugin::Builder::<_, ()>::new("startup-probe-runtime")
                .setup(|_app, _api| {
                    startup::mark("tauri:runtime_created");
                    Ok(())
                })
                .on_window_ready(|window| {
                    if window.label() == "main" {
                        startup::mark("tauri:main_window_ready");
                    }
                })
                .on_webview_ready(|webview| {
                    if webview.label() == "main" {
                        startup::mark("tauri:main_webview_ready");
                    }
                })
                .on_navigation(move |webview, url| {
                    use std::sync::atomic::Ordering;

                    if webview.label() != "main" {
                        return true;
                    }
                    #[cfg(target_os = "linux")]
                    if matches!(url.as_str(), "about:blank" | "about:srcdoc") {
                        let main_origin_initialized = main_navigation_origin
                            .lock()
                            .unwrap_or_else(std::sync::PoisonError::into_inner)
                            .is_some();
                        return allow_embedded_document_navigation(url, main_origin_initialized);
                    }
                    let origin = format!(
                        "{}://{}:{}",
                        url.scheme(),
                        url.host_str().unwrap_or_default(),
                        url.port_or_known_default()
                            .map_or_else(|| "-".to_string(), |port| port.to_string())
                    );
                    if !initial_navigation_reported.swap(true, Ordering::Relaxed) {
                        startup::mark_with_detail(
                            "rust",
                            "tauri:main_navigation",
                            &format!("scheme={}", url.scheme()),
                        );
                    }
                    let mut initial_origin = main_navigation_origin
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner);
                    match initial_origin.as_ref() {
                        None => {
                            *initial_origin = Some(origin);
                            true
                        }
                        Some(allowed) if allowed == &origin => true,
                        Some(_) => {
                            startup::mark_with_detail(
                                "rust",
                                "tauri:main_navigation_blocked",
                                &format!(
                                    "scheme={} host={} port={}",
                                    url.scheme(),
                                    url.host_str().unwrap_or_default(),
                                    url.port_or_known_default()
                                        .map_or_else(|| "-".to_string(), |port| port.to_string())
                                ),
                            );
                            false
                        }
                    }
                })
                .build()
        })
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            if let Some(window) = app.get_webview_window("main") {
                crate::platform::window_startup::activate_main_window(window);
            }
        }))
        .plugin(
            tauri::plugin::Builder::<_, ()>::new("startup-probe-single-instance")
                .setup(|_app, _api| {
                    startup::mark("tauri:plugin_single_instance_ready");
                    Ok(())
                })
                .build(),
        )
        .plugin(tauri_plugin_notification::init())
        .plugin(
            tauri::plugin::Builder::<_, ()>::new("startup-probe-notification")
                .setup(|_app, _api| {
                    startup::mark("tauri:plugin_notification_ready");
                    Ok(())
                })
                .build(),
        )
        .plugin(tauri_plugin_dialog::init())
        .plugin(
            tauri::plugin::Builder::<_, ()>::new("startup-probe-dialog")
                .setup(|_app, _api| {
                    startup::mark("tauri:plugin_dialog_ready");
                    Ok(())
                })
                .build(),
        )
        .on_page_load(|webview, payload| {
            startup::mark_with_detail(
                "rust",
                "webview:page_load",
                &format!("label={} event={:?}", webview.label(), payload.event()),
            );
        })
        .setup(|app| {
            startup::mark("setup:start");
            if let Ok(resource_dir) = app.path().resource_dir() {
                crate::platform::paths::set_runtime_resource_dir(resource_dir);
            }
            #[cfg(target_os = "macos")]
            features::updater::cleanup_stale_backup();
            if cfg!(debug_assertions) {
                app.handle().plugin(
                    tauri_plugin_log::Builder::default()
                        .level(log::LevelFilter::Info)
                        .build(),
                )?;
            }
            startup::mark("setup:plugins_ready");
            crate::platform::window_startup::arm_hidden_main_window_fallback(app.handle());

            // Linux webview(webkit2gtk)默认拒绝 getUserMedia,语音输入点麦克风会被拒。
            // 给 main 窗口 webview 挂 permission-request:只放行 UserMedia(麦克风/摄像头)
            // 请求,定位/通知等其余权限仍按默认拒绝。Windows/macOS 的 WebView2/WKWebView
            // 自带系统级麦克风授权,不走这条。
            #[cfg(target_os = "linux")]
            {
                use tauri::Manager;
                if let Some(window) = app.get_webview_window("main") {
                    let _ = window.with_webview(|webview| {
                        use webkit2gtk::glib::prelude::ObjectExt;
                        use webkit2gtk::{PermissionRequestExt, WebViewExt};
                        let wv = webview.inner();
                        wv.connect_permission_request(|_wv, req| {
                            if req.type_().name() == "WebKitUserMediaPermissionRequest" {
                                req.allow();
                                true
                            } else {
                                false
                            }
                        });
                    });
                }
            }

            // 必须早于 SessionStore boot：归档退役宿主并清除绑定，避免内存态与磁盘态打架。
            startup::mark("retired_features:start");
            if let Err(error) = crate::features::retirement::retire_removed_features() {
                eprintln!("[pinvou3-app] retired feature cleanup failed (will retry next boot): {error}");
                startup::mark_with_detail("rust", "retired_features:error", &error.to_string());
            }
            startup::mark("retired_features:done");

            // 多对话历史 store：用 ~/.pinvou3/sessions/ 隔离 deepseek-tui 全局目录。
            // 必须先 boot 这个，engine forwarder 需要它跟踪 active session 的 mode_state
            // 以便 TurnComplete 时判定是否 emit chat:plan_ready。
            startup::mark("session_store:start");
            let session_store = match SessionStore::boot_for_process_startup() {
                Ok(store) => {
                    store.load_session_models();
                    store.load_pinned_sessions();
                    store.load_hidden_sessions();
                    eprintln!("[pinvou3-app] session store ready");
                    Some(store)
                }
                Err(e) => {
                    eprintln!("[pinvou3-app] session store boot failed: {e:#}");
                    None
                }
            };
            startup::mark("session_store:done");
            if let Some(store) = session_store.clone() {
                app.handle().manage(store);
            }
            let remote_control_manager = RemoteControlManager::new(app.handle().clone());
            let remote_event_transport = remote_control_manager.clone();
            app.handle().manage(platform::app_events::AppEventBus::new(
                move |event, payload| remote_event_transport.forward_local_event(event, payload),
            ));
            app.handle().manage(remote_control_manager.clone());
            app.handle()
                .manage(features::behavior_telemetry::BehaviorTelemetry::new());
            // 匿名设备遥测：独立于 UI/Engine，失败只写日志；先 manage 再建 EnginePool，
            // 让每个 session forwarder 都能在 TurnStarted/TurnComplete 取到状态。
            match telemetry::TelemetryState::boot(env!("CARGO_PKG_VERSION")) {
                Ok(Some(state)) => {
                    app.handle().manage(state.clone());
                    state.spawn();
                    eprintln!("[pinvou3-app] device telemetry ready");
                }
                Ok(None) => eprintln!("[pinvou3-app] device telemetry disabled"),
                Err(error) => {
                    eprintln!("[pinvou3-app] device telemetry init failed: {error:#}")
                }
            }
            let team_relay_manager = crate::features::team_relay::TeamRelayManager::new()
                .map_err(std::io::Error::other)?;
            app.handle().manage(team_relay_manager);
            // 多 session 并发:存 EnginePool(lazy spawn,首条消息才为该 session 起 engine)。
            // boot bridge 在 pool::new 里做一次(写盘 / 设 env 只能一次)。
            let handle = app.handle().clone();
            let store_for_engine = session_store.unwrap_or_else(|| {
                // store boot 失败时退化用一份临时 store（让 engine 至少能起来）；
                // 实际使用 session 相关命令会失败,但聊天能跑
                SessionStore::boot_for_process_startup().expect("session store boot fallback")
            });
            // 原生代码会话的执行根解析需要共享 AcpPool 持有的 SessionAgentStore
            // （多实例各自读盘，只有这份 clone 与 AcpPool 同一份 Arc）。
            let (code_session_agents, acp_pool_for_capabilities) =
                match crate::features::codex_acp::AcpPool::new(handle.clone(), store_for_engine.clone())
                {
                    Ok(pool) => {
                        let agents = pool.agents().clone();
                        let capability_pool = pool.clone();
                        handle.manage(pool);
                        eprintln!("[pinvou3-app] Codex ACP pool ready (lazy spawn per session)");
                        (agents, capability_pool)
                    }
                    Err(error) => {
                        panic!("failed to init Codex ACP pool: {error:#}");
                    }
                };
            startup::mark("engine_pool:start");
            let tool_factory: crate::features::assistant::engine_pool::EngineToolFactory =
                std::sync::Arc::new(|app, session_id| {
                    vec![
                        std::sync::Arc::new(knowledge::KbSearchTool::new(
                            app.clone(),
                            session_id.to_string(),
                        )),
                        std::sync::Arc::new(knowledge::KbOpenSourceTool::new(
                            app.clone(),
                            session_id.to_string(),
                        )),
                    ]
                });
            let tool_policy: crate::features::assistant::engine_pool::ToolPolicy =
                std::sync::Arc::new(|app| {
                    let mut tools = crate::features::marketplace::disabled_tool_names();
                    let kb_usable = app
                        .try_state::<knowledge::KnowledgeService>()
                        .map(|service| service.has_indexed_content() && service.semantic_ready())
                        .unwrap_or(false)
                        || app
                            .try_state::<RemoteKnowledgeService>()
                            .map(|service| service.has_connections())
                            .unwrap_or(false);
                    if !kb_usable {
                        tools.push("kb_search".to_string());
                        tools.push("kb_open_source".to_string());
                    }
                    tools
                });
            match EnginePool::new_with_runtime_model_provider(
                handle.clone(),
                store_for_engine.clone(),
                tool_factory,
                tool_policy,
                std::sync::Arc::new(
                    crate::features::llmapi_hub::runtime_model::LlmApiRuntimeModelProvider,
                ),
            ) {
                Ok(mut pool) => {
                    // 两个根：执行根（engine cwd/shell）对绑了项目目录的原生代码会话
                    // 解析到项目目录；账本根（附件/审计/产物）恒为会话私有目录。
                    // 解析实现统一下沉在 SessionStore::session_roots，bridge 与
                    // SessionStore 注入同一份 resolver 闭包，两侧结果一致。
                    let execution_root_resolver: crate::features::sessions::ExecutionRootResolver =
                        std::sync::Arc::new({
                            let agents = code_session_agents.clone();
                            move |session_id: &str| agents.code_project_workspace(session_id)
                        });
                    pool.bridge
                        .set_execution_root_resolver(execution_root_resolver.clone());
                    store_for_engine.set_execution_root_resolver(execution_root_resolver);
                    pool.bridge.set_code_session_predicate(std::sync::Arc::new({
                        let agents = code_session_agents.clone();
                        move |session_id: &str| agents.is_code_session(session_id)
                    }));
                    pool.bridge
                        .set_external_acp_session_predicate(std::sync::Arc::new({
                            let acp_pool = acp_pool_for_capabilities.clone();
                            move |session_id: &str| acp_pool.is_acp(session_id)
                        }));
                    // sessions feature 自己的 code 判定（mode 默认值解析 + 仅 code
                    // 持久化），与 bridge 共用同一份 SessionAgentStore 闭包。
                    store_for_engine.set_code_session_predicate(std::sync::Arc::new({
                        let agents = code_session_agents.clone();
                        move |session_id: &str| agents.is_code_session(session_id)
                    }));
                    // 远程端正式支持代码会话之前，先过滤原生代码会话事件（与 Engine
                    // bridge 共用同一份 SessionAgentStore 判定）。
                    remote_control_manager.set_code_session_predicate(std::sync::Arc::new({
                        let agents = code_session_agents.clone();
                        move |session_id: &str| agents.is_code_session(session_id)
                    }));
                    let scheduled_state = tauri::async_runtime::block_on(
                        scheduled_tasks::ScheduledTaskState::boot_runtime(
                            &pool.bridge,
                            pool.clone(),
                            store_for_engine.clone(),
                        ),
                    );
                    match scheduled_state {
                        Ok(state) => {
                            handle.manage(state);
                            eprintln!("[pinvou3-app] scheduled tasks runtime ready");
                        }
                        Err(e) => {
                            eprintln!("[pinvou3-app] scheduled tasks runtime init failed: {e:#}");
                        }
                    }
                    handle.manage(pool);
                    eprintln!("[pinvou3-app] engine pool ready (lazy spawn per session)");
                    match remote_control_manager.resume() {
                        Ok(true) => eprintln!("[pinvou3-app] persistent Web access resumed"),
                        Ok(false) => {}
                        Err(error) => {
                            eprintln!("[pinvou3-app] persistent Web access resume failed: {error}")
                        }
                    }
                }
                Err(e) => {
                    eprintln!("[pinvou3-app] failed to init engine pool: {e:#}");
                }
            }
            startup::mark("engine_pool:done");

            // 技能双 scope 治理(skill-scope-governance):启动时
            //   1. 读一次 disabled_skills.json——触发旧数据迁移(裸数组 / 借道
            //      disabled_connectors.json 的 `skill:` 条目 → plain scope);
            //   2. 退役进程级全局 DISABLED_SKILLS(过滤职责移交组合目录,组合目录
            //      空 → 整个 `## Skills` 块不渲染,路径泄露面随之封闭)。
            // 组合目录的物化在 engine spawn 时按会话进行(build_engine_config 注入
            // skills_dir 指向 ~/.pinvou3/sessions/<sid>/skills/)。
            startup::mark("disabled_skills:start");
            let _ = crate::features::assistant::skill_materialization::load_disabled_skills();
            deepseek_tui::skills::set_disabled_skills(Vec::new());
            startup::mark("disabled_skills:done");

            // Monitor 按需采样：state 只持有 session_uptime，sample 由前端调
            // get_monitor_snapshot 时触发（监控页面 1s interval，离开页面停）。
            let monitor_state = MonitorState::new();
            app.handle().manage(monitor_state);
            app.handle()
                .manage(notifications::NotificationState::default());
            app.handle()
                .manage(pet_window::PetNavigationState::default());
            app.handle().manage(pet_window::PetReplyState::default());
            app.handle().manage(selected_pet::SelectedPetStore::load());

            // CLI 连接器连接编排状态(按连接器 id 存长驻子进程 PID + 取消标志),
            // 飞书 / 企微共用,供 *_connect_begin / *_cancel 用。
            app.handle().manage(connector_cli::ConnectorConn::default());

            // EIP 连接编排状态(SSO 登录轮询取消标志),供 eip_connect_begin/cancel 用。
            app.handle().manage(eip::EipConn::default());

            // 知道连接编排状态(SSO 登录取消标志),供 zhidao_connect_begin/cancel 用。
            app.handle().manage(zhidao::ZhidaoConn::default());
            // File watcher: 监听 ~/.pinvou3/sessions/ 树,新文件 emit artifact:disk
            file_watcher::spawn(app.handle().clone(), bridge::paths::sessions_root());
            startup::mark("file_watcher:spawned");

            // 本地知识底座 L0:全系统元数据索引(秒搜+去重)。这里只 manage,**不自动扫**——
            // 扫描改懒触发:由前端进入文件管理页时增量扫(不进页=零扫描),不常驻 watcher/周期
            // 重扫。文件管理是低频功能,不该长期占资源。
            // embedding 模型**不再随 deb 打包**(deb 瘦 ~559MB):改按需下载到
            // ~/.pinvou3/knowledge/models/bge-m3。setup 只打开数据库并以 embedder=None 注册服务；
            // React 首帧后再调用 kb_model_load_after_first_frame，通过 spawn_blocking 后台加载。
            // 模型没装/加载失败时维持完全门控，不阻断启动；下载完成仍可热加载。
            // 语音识别引擎随平台安装包打包,容错同 bge-m3 的资源布局,
            // 注入给 voice_asr 作为 ~/.pinvou3/asr/ 之外的回退查找目录。
            if let Some(asr_res) = app.path().resource_dir().ok().and_then(|res| {
                [
                    res.join("runtime").join("asr"),
                    res.join("resources").join("runtime").join("asr"),
                ]
                .into_iter()
                .find(|d| d.join(features::voice::engine_binary_name()).exists())
            }) {
                features::voice::set_bundled_engine_dir(asr_res);
            }

            startup::mark("knowledge_service:start");
            match knowledge::KnowledgeService::new(&knowledge::default_db_path()) {
                Ok(svc) => {
                    app.handle().manage(svc);
                    eprintln!("[pinvou3-app] knowledge service ready");
                }
                Err(e) => eprintln!("[pinvou3-app] knowledge service init failed: {e:#}"),
            }
            startup::mark("knowledge_service:done");

            match RemoteKnowledgeService::load(RemoteKnowledgeService::default_path()) {
                Ok(service) => {
                    app.handle().manage(service);
                    eprintln!("[pinvou3-app] remote knowledge service ready");
                }
                Err(error) => {
                    eprintln!("[pinvou3-app] remote knowledge service init failed: {error}")
                }
            }
            // 桌宠:settings.json 里 pet.enabled 为真时随主窗口一起拉起。
            pet_window::spawn_if_enabled(app.handle());

            startup::mark("setup:done");
            Ok(())
        })
        .on_window_event(|window, event| {
            // 主窗口销毁 → 一并关掉桌宠,否则只剩宠物窗口时 app 不退出。
            if window.label() == "main" {
                if let tauri::WindowEvent::Destroyed = event {
                    pet_window::close_with_main(window.app_handle());
                }
            }
        })
        .invoke_handler(tauri::generate_handler![
            commands::chat::chat,
            commands::behavior_telemetry::track_behavior_event,
            commands::assistant_response::export_assistant_response,
            commands::assistant_response::open_assistant_share_target,
            commands::startup::report_frontend_startup,
            commands::startup::reveal_startup_window,
            commands::connectors::refresh_connector_auth_gates,
            commands::connectors::feishu_ensure_cli,
            commands::connectors::feishu_status,
            commands::connectors::feishu_connect_begin,
            commands::connectors::feishu_cancel,
            commands::connectors::feishu_logout,
            commands::connectors::feishu_apply_skills,
            commands::connectors::set_feishu_enabled,
            commands::connectors::feishu_skills_state,
            commands::connectors::wecom_ensure_cli,
            commands::connectors::wecom_status,
            commands::connectors::wecom_connect_begin,
            commands::connectors::wecom_cancel,
            commands::connectors::wecom_logout,
            commands::connectors::wecom_apply_skills,
            commands::connectors::set_wecom_enabled,
            commands::connectors::wecom_skills_state,
            commands::connectors::dingtalk_ensure_cli,
            commands::connectors::dingtalk_status,
            commands::connectors::dingtalk_connect_begin,
            commands::connectors::dingtalk_cancel,
            commands::connectors::dingtalk_logout,
            commands::connectors::dingtalk_apply_skills,
            commands::connectors::set_dingtalk_enabled,
            commands::connectors::dingtalk_skills_state,
            commands::connectors::eip_status,
            commands::connectors::eip_connect_begin,
            commands::connectors::eip_cancel,
            commands::connectors::eip_logout,
            commands::connectors::zhidao_status,
            commands::connectors::zhidao_connect_begin,
            commands::connectors::zhidao_cancel,
            commands::connectors::zhidao_logout,
            commands::connectors::tmeet_ensure_cli,
            commands::connectors::tmeet_status,
            commands::connectors::tmeet_connect_begin,
            commands::connectors::tmeet_cancel,
            commands::connectors::tmeet_logout,
            commands::connectors::tmeet_apply_skills,
            commands::connectors::set_tmeet_enabled,
            commands::connectors::tmeet_skills_state,
            commands::connectors::ima_status,
            commands::connectors::ima_connect,
            commands::connectors::ima_logout,
            commands::settings::get_settings,
            commands::runtime::get_platform_capabilities,
            commands::team_relay::team_relay_status,
            commands::team_relay::team_relay_verify_beta_access,
            commands::team_relay::team_relay_register_account,
            commands::team_relay::team_relay_account,
            commands::team_relay::team_relay_claim,
            commands::team_relay::team_relay_create_first_team,
            commands::team_relay::team_relay_bootstrap,
            commands::team_relay::team_relay_update_profile,
            commands::team_relay::team_relay_upload_avatar,
            commands::team_relay::team_relay_remove_avatar,
            commands::team_relay::team_relay_avatar_data_url,
            commands::team_relay::team_relay_create_team,
            commands::team_relay::team_relay_join_team,
            commands::team_relay::team_relay_switch_team,
            commands::team_relay::team_relay_create_invite,
            commands::team_relay::team_relay_get_join_password,
            commands::team_relay::team_relay_rotate_join_password,
            commands::team_relay::team_relay_set_join_password_visibility,
            commands::team_relay::team_relay_disable_join_password,
            commands::team_relay::team_relay_remove_member,
            commands::team_relay::team_relay_create_thread,
            commands::team_relay::team_relay_create_action,
            commands::team_relay::team_relay_set_thread_preference,
            commands::team_relay::team_relay_mark_thread_read,
            commands::team_relay::team_relay_upload_attachment,
            commands::team_relay::team_relay_download_attachment,
            commands::team_relay::team_relay_task_workspace_status,
            commands::team_relay::team_relay_prepare_task_workspace,
            commands::team_relay::team_relay_reveal_prepared_material,
            commands::team_relay::team_relay_set_task_agent_session,
            commands::team_relay::team_relay_disconnect,
            commands::settings::submit_feedback,
            commands::settings::get_effective_model_config,
            commands::settings::update_settings,
            commands::settings::update_search_settings,
            commands::settings::save_settings_and_restart,
            commands::settings::save_search_settings_and_restart,
            commands::sessions::clear_session,
            commands::monitor::get_monitor_snapshot,
            commands::monitor::get_backend_status,
            commands::llmapi::ensure_llmapi_binding,
            commands::llmapi::login_llmapi_user,
            commands::llmapi::save_llmapi_user_session,
            commands::llmapi::get_llmapi_status,
            commands::llmapi::get_llmapi_models,
            commands::llmapi::set_llmapi_default_model,
            commands::llmapi::retry_llmapi_provisioning,
            commands::llmapi::set_llmapi_user_enabled,
            commands::llmapi::get_llmapi_admin_overview,
            commands::monitor::discover_local_vllm,
            commands::local_llm::detect_local_vllm_setup,
            commands::local_llm::bootstrap_local_vllm,
            commands::local_llm::decline_local_vllm_setup,
            commands::settings::list_models,
            commands::settings::reveal_model_api_key,
            commands::settings::save_model,
            commands::settings::delete_model,
            commands::settings::set_active_model,
            commands::settings::set_session_model,
            commands::settings::get_session_model_id,
            commands::settings::get_image_input_capability,
            commands::codex::get_codex_acp_status,
            commands::codex::list_acp_agents,
            commands::codex::get_acp_agent_status,
            commands::codex::prepare_codex_acp,
            commands::codex::install_codex_homebrew,
            commands::codex::install_acp_agent,
            commands::codex::login_codex_acp,
            commands::codex::login_acp_agent,
            commands::codex::switch_acp_agent_account,
            commands::codex::open_codex_login_url,
            commands::codex::open_acp_agent_login_url,
            commands::codex::submit_acp_agent_login_code,
            commands::codex::get_codex_acp_session_info,
            commands::codex::set_codex_acp_model,
            commands::codex::set_codex_acp_mode,
            commands::codex::set_codex_acp_config_option,
            commands::codex::codex_acp_prompt,
            commands::codex::cancel_codex_acp,
            commands::codex::get_codex_acp_timeline,
            commands::codex::get_codex_acp_pending_permissions,
            commands::codex::respond_codex_acp_permission,
            commands::codex::get_codex_acp_pending_elicitations,
            commands::codex::respond_codex_acp_elicitation,
            commands::acp_providers::list_acp_providers,
            commands::acp_providers::save_acp_provider,
            commands::acp_providers::delete_acp_provider,
            commands::acp_providers::switch_acp_provider,
            commands::acp_providers::switch_acp_provider_official,
            commands::acp_providers::uninstall_acp_agent,
            commands::acp_providers::cancel_acp_agent_install,
            commands::acp_providers::logout_acp_agent,
            commands::acp_providers::get_acp_provider_key,
            commands::acp_providers::export_acp_providers,
            commands::acp_providers::import_acp_providers,
            commands::acp_providers::probe_acp_agent_models,
            commands::acp_providers::set_codex_acp_session_provider,
            commands::codex::list_codex_acp_sessions,
            commands::codex::create_codex_acp_session,
            commands::codex::list_codex_workspace,
            commands::codex::search_codex_workspace,
            commands::codex::preview_codex_workspace_file,
            commands::codex::open_codex_workspace_resource,
            commands::codex::get_codex_workspace_changes,
            commands::codex::get_codex_workspace_diff,
            commands::codex::open_codex_workspace_file,
            commands::codex::reveal_codex_workspace_file,
            commands::codex::open_code_reader,
            commands::codex::take_code_reader_pending,
            commands::settings::test_model_connection,
            commands::settings::test_image_input_capability,
            commands::settings::test_search_provider,
            commands::voice::transcribe_voice_audio,
            commands::voice::reset_microphone_permission,
            commands::voice::voice_asr_status,
            commands::voice::install_voice_asr,
            commands::voice::cancel_voice_asr,
            commands::sessions::list_sessions,
            commands::sessions::create_session,
            commands::sessions::load_session,
            commands::sessions::delete_session,
            commands::sessions::rename_session,
            commands::sessions::set_session_pinned,
            commands::sessions::list_archived_sessions,
            commands::sessions::set_session_archived,
            commands::timeline::get_session_timeline,
            commands::timeline::get_session_stats,
            commands::scheduled::list_scheduled_tasks,
            commands::scheduled::read_scheduled_task,
            commands::scheduled::list_scheduled_task_runs,
            commands::scheduled::list_scheduled_runs,
            commands::scheduled::create_scheduled_task,
            commands::scheduled::update_scheduled_task,
            commands::scheduled::pause_scheduled_task,
            commands::scheduled::resume_scheduled_task,
            commands::scheduled::set_scheduled_task_pinned,
            commands::scheduled::delete_scheduled_task,
            commands::scheduled::run_scheduled_task_now,
            commands::scheduled::mark_scheduled_run_viewed,
            commands::scheduled::scheduled_task_chat_prompt,
            commands::sessions::get_active_session,
            commands::sessions::save_session_messages,
            commands::sessions::save_session_artifacts,
            commands::sessions::save_session_pinvou_scene_events,
            commands::sessions::get_session_pinvou_scene_events,
            commands::sessions::list_workspace_files,
            commands::runtime::cancel_generation,
            commands::runtime::list_shell_tasks,
            commands::runtime::cancel_shell_task,
            commands::remote_control::web_access_enable,
            commands::remote_control::web_access_disable,
            commands::remote_control::web_access_status,
            commands::remote_control::web_access_rotate,
            commands::remote_control::web_access_relay_settings,
            commands::remote_control::web_access_set_relay,
            commands::remote_control::web_access_reset_relay,
            commands::remote_control::web_access_bridge_ready,
            commands::remote_control::web_access_rpc_begin,
            commands::remote_control::web_access_rpc_respond,
            commands::remote_control::web_access_publish_event,
            commands::remote_control::web_access_list_host_files,
            commands::remote_control::web_access_create_session,
            commands::remote_control::web_access_create_session_and_chat,
            commands::remote_control::web_access_load_session_chunk,
            commands::remote_control::web_access_ingest_file,
            commands::remote_control::web_access_upload_attachment_chunk,
            commands::remote_control::web_access_abort_attachment_upload,
            commands::remote_control::web_access_discard_attachment,
            commands::remote_control::web_access_read_conversation_attachment_chunk,
            commands::remote_control::web_access_chat,
            commands::remote_control::web_access_save_session_messages_chunk,
            commands::remote_control::web_access_transcribe_voice_audio,
            commands::remote_control::web_access_read_artifact_chunk,
            commands::remote_control::web_access_update_settings,
            commands::remote_control::web_access_artifact_info,
            commands::remote_control::web_access_read_artifact_text,
            commands::remote_control::web_access_write_artifact_text,
            commands::remote_control::web_access_read_artifact_image_b64,
            commands::remote_control::web_access_read_artifact_thumbnail,
            commands::remote_control::web_access_render_artifact_visual,
            commands::connectors::set_disabled_connectors,
            commands::connectors::get_disabled_connectors,
            commands::connectors::set_disabled_skills,
            commands::connectors::get_disabled_skills,
            commands::connectors::set_project_skills_enabled,
            commands::connectors::get_project_skills_enabled,
            commands::memory::get_memory_profile,
            commands::memory::update_memory_profile,
            commands::memory::clear_memory_profile,
            commands::memory::get_memory_overview,
            commands::memory::list_pending_memory,
            commands::memory::suggest_memory,
            commands::memory::confirm_pending_memory,
            commands::memory::ignore_pending_memory,
            commands::memory::never_pending_memory,
            commands::memory::list_recent_work_memory,
            commands::memory::upsert_recent_work_memory,
            commands::memory::archive_recent_work_memory,
            commands::memory::delete_memory_preference,
            commands::memory::update_memory_preference,
            commands::memory::update_work_context_memory,
            commands::memory::delete_work_context_memory,
            commands::memory::update_timed_memory,
            commands::memory::delete_timed_memory,
            commands::memory::edit_last_turn,
            commands::artifacts::read_artifact_text,
            commands::artifacts::write_artifact_text,
            commands::artifacts::list_deliverable_index,
            commands::artifacts::artifact_info,
            commands::artifacts::render_artifact_visual,
            commands::artifacts::read_artifact_image_b64,
            commands::artifacts::read_artifact_thumbnail,
            commands::artifacts::open_in_system,
            commands::artifacts::open_containing_folder,
            commands::artifacts::reveal_session_folder,
            commands::artifacts::open_scheduled_task_folder,
            commands::artifacts::open_artifact_window,
            commands::pet::open_detached_window,
            commands::pet::begin_detach_drag,
            commands::pet::set_pet_enabled,
            commands::pet::get_pet_scale,
            commands::pet::set_pet_scale,
            commands::pet::set_pet_activity_visible,
            commands::pet::save_pet_position,
            commands::pet::save_pet_vertical_alignment,
            commands::pet::open_main_from_pet,
            commands::pet::take_pet_navigation,
            commands::pet::queue_pet_reply,
            commands::pet::take_pet_reply,
            commands::pet::get_selected_pet,
            commands::pet::set_selected_pet,
            commands::artifacts::open_external_url,
            commands::artifacts::open_user_external_url,
            commands::files::ingest_file,
            commands::files::ingest_dropped_file_chunk,
            commands::files::cancel_dropped_file_upload,
            commands::files::discard_dropped_attachment,
            commands::files::resolve_conversation_attachment,
            commands::files::open_conversation_attachment,
            commands::files::reveal_conversation_attachment,
            commands::files::detect_system_tools,
            commands::files::save_paste_image,
            commands::interaction::compact_now,
            commands::interaction::get_mode_state,
            commands::interaction::get_code_permission_prefs,
            commands::interaction::confirm_code_yolo,
            commands::interaction::get_mode_defaults,
            commands::interaction::set_mode_default,
            commands::interaction::set_plan_mode_next,
            commands::interaction::exit_plan_to_yolo,
            commands::interaction::set_multi_agent_mode,
            commands::interaction::accept_plan,
            commands::interaction::discard_plan,
            commands::interaction::read_skill_body,
            // 多智能体执行记录投影。
            commands::multiagent::list_subagent_transcripts,
            commands::multiagent::read_subagent_transcript,
            commands::interaction::submit_user_input,
            commands::interaction::cancel_user_input,
            commands::interaction::get_pending_user_inputs,
            commands::interaction::restart_engine,
            commands::interaction::summon_pinvou,
            commands::personas::save_session_pinvou_reviews,
            commands::personas::get_session_pinvou_reviews,
            commands::interaction::get_super_permission_status,
            commands::interaction::set_super_permission,
            commands::personas::list_personas,
            commands::personas::read_persona_body,
            commands::personas::equip_persona,
            commands::personas::unequip_persona,
            commands::personas::get_active_persona,
            commands::personas::create_persona,
            commands::personas::update_persona,
            commands::personas::delete_persona,
            commands::personas::save_session_persona_events,
            commands::personas::get_session_persona_events,
            commands::updater::get_app_version,
            commands::updater::check_for_update,
            commands::updater::download_update,
            commands::updater::install_update,
            commands::updater::restart_app,
            commands::updater::cancel_download,
            commands::updater::report_pending_update_result,
            commands::dependencies::check_dependencies,
            commands::dependencies::install_dependencies,
            commands::marketplace::list_marketplace_tools,
            commands::marketplace::install_marketplace_tool,
            commands::marketplace::get_marketplace_tool_auth_status,
            commands::marketplace::start_marketplace_tool_oauth_login,
            commands::marketplace::cancel_marketplace_tool_oauth_login,
            commands::marketplace::uninstall_marketplace_tool,
            commands::artifacts::detect_obsidian,
            commands::knowledge::kb_start_scan,
            commands::knowledge::kb_scan_status,
            commands::knowledge::kb_cancel_scan,
            commands::knowledge::kb_search,
            commands::knowledge::kb_stats,
            commands::knowledge::kb_type_counts,
            commands::knowledge::kb_collection_list,
            commands::knowledge::kb_collection_create,
            commands::knowledge::kb_collection_update,
            commands::knowledge::kb_collection_delete,
            commands::knowledge::kb_collection_add_sources,
            commands::knowledge::kb_index_status,
            commands::knowledge::kb_index_cancel,
            commands::knowledge::kb_index_failed_files,
            commands::knowledge::kb_index_resume,
            commands::knowledge::kb_index_retry_file,
            commands::knowledge::kb_documents,
            commands::knowledge::kb_remove_document,
            commands::knowledge::kb_embed_info,
            commands::knowledge::kb_model_status,
            commands::knowledge::kb_model_load_after_first_frame,
            commands::knowledge::kb_model_download,
            commands::knowledge::kb_model_cancel,
            commands::knowledge::session_mount_collection,
            commands::knowledge::session_set_mounted_collections,
            commands::knowledge::session_add_mounted_collection,
            commands::knowledge::session_set_mounted_collection_enabled,
            commands::knowledge::session_remove_mounted_collection,
            commands::knowledge::session_unmount_collection,
            commands::knowledge::session_mounted_collection,
            commands::knowledge::session_mounted_collections,
            commands::knowledge::session_mounted_collections_snapshot,
            commands::remote_knowledge::remote_kb_connections,
            commands::remote_knowledge::remote_kb_request_join,
            commands::remote_knowledge::remote_kb_probe_private_endpoint,
            commands::remote_knowledge::remote_kb_request_join_confirmed,
            commands::remote_knowledge::remote_kb_connection_identity,
            commands::remote_knowledge::remote_kb_pending_joins,
            commands::remote_knowledge::remote_kb_refresh_join,
            commands::remote_knowledge::remote_kb_cancel_join,
            commands::remote_knowledge::remote_kb_create_share,
            commands::remote_knowledge::remote_kb_shares,
            commands::remote_knowledge::remote_kb_stop_share,
            commands::remote_knowledge::remote_kb_join_requests,
            commands::remote_knowledge::remote_kb_approve_join_request,
            commands::remote_knowledge::remote_kb_reject_join_request,
            commands::remote_knowledge::remote_kb_model_status,
            commands::remote_knowledge::remote_kb_download_model,
            commands::remote_knowledge::remote_kb_devices,
            commands::remote_knowledge::remote_kb_update_device,
            commands::remote_knowledge::remote_kb_remove_device,
            commands::remote_knowledge::remote_kb_trashed_collections,
            commands::remote_knowledge::remote_kb_trashed_documents,
            commands::remote_knowledge::remote_kb_permanently_delete_collection,
            commands::remote_knowledge::remote_kb_permanently_delete_document,
            commands::shared_knowledge_host::shared_kb_host_status,
            commands::shared_knowledge_host::shared_kb_host_lan_endpoints,
            commands::shared_knowledge_host::shared_kb_discover_nearby,
            commands::shared_knowledge_host::shared_kb_host_install,
            commands::shared_knowledge_host::shared_kb_host_upgrade,
            commands::shared_knowledge_host::shared_kb_host_reconnect,
            commands::shared_knowledge_host::shared_kb_host_set_owner_device,
            commands::shared_knowledge_host::shared_kb_host_remove,
            commands::shared_knowledge_host::shared_kb_host_backup,
            commands::shared_knowledge_host::shared_kb_host_restore,
            commands::remote_knowledge::remote_kb_remove_connection,
            commands::remote_knowledge::remote_kb_collections,
            commands::remote_knowledge::remote_kb_create_collection,
            commands::remote_knowledge::remote_kb_update_collection,
            commands::remote_knowledge::remote_kb_delete_collection,
            commands::remote_knowledge::remote_kb_restore_collection,
            commands::remote_knowledge::remote_kb_documents,
            commands::remote_knowledge::remote_kb_document_statuses,
            commands::remote_knowledge::remote_kb_discover_folder_files,
            commands::remote_knowledge::remote_kb_upload_files,
            commands::remote_knowledge::remote_kb_replace_document,
            commands::remote_knowledge::remote_kb_delete_document,
            commands::remote_knowledge::remote_kb_restore_document,
            commands::remote_knowledge::remote_kb_download_document,
            commands::remote_knowledge::remote_kb_search,
            commands::remote_knowledge::session_mounted_remote_collections,
            commands::remote_knowledge::session_add_mounted_remote_collection,
            commands::remote_knowledge::session_set_mounted_remote_collection_enabled,
            commands::remote_knowledge::session_remove_mounted_remote_collection,
            commands::marketplace::list_marketplace_skills,
            commands::marketplace::install_marketplace_skill,
            commands::marketplace::import_skill_package,
            commands::marketplace::import_skill_package_bytes,
            commands::marketplace::uninstall_marketplace_skill,
            commands::marketplace::bundle_readiness,
            commands::files::verify_upload,
        ]);

    startup::mark("tauri:builder_configured");
    startup::mark("tauri:context:start");
    let context = tauri::generate_context!();
    startup::mark("tauri:context:done");
    // Keep the historical marker so old and new startup runs remain comparable.
    startup::mark("tauri:run_enter");
    startup::mark("tauri:build:start");
    let app = builder
        .build(context)
        .expect("error while building tauri application");
    startup::mark("tauri:build:done");
    startup::mark("tauri:event_loop:run_enter");
    let mut resumed_reported = false;
    app.run(move |_app, event| match event {
        tauri::RunEvent::Ready => startup::mark("tauri:event_loop:ready"),
        tauri::RunEvent::Resumed if !resumed_reported => {
            resumed_reported = true;
            startup::mark("tauri:event_loop:first_resumed");
        }
        _ => {}
    });
    startup::mark("process:exit");
}

#[cfg(test)]
mod tool_allowlist_contract {
    use crate::features::assistant::tool_policy::{
        is_pinvou3_allowed, PINVOU3_ALLOWED_TOOLS, PINVOU3_ALWAYS_LOADED_TOOLS,
    };

    /// Pinvou 只允许产品需要的 canonical 工具家族；动态 MCP 工具限制在标准命名空间。
    #[test]
    fn pinvou3_allowlist_uses_canonical_families_and_dynamic_mcp_namespace() {
        for core in [
            "Bash",
            "File",
            "Git",
            "Web",
            "agent",
            "load_skill",
            "todo_write",
            "tool_search",
            "request_user_input",
            "revert_turn",
            "kb_search",
            "kb_open_source",
            "mcp_weather_get_weather",
        ] {
            assert!(is_pinvou3_allowed(core), "核心工具 {core} 应在白名单");
        }

        for excluded in [
            "Run",
            "tasks",
            "automation",
            "github",
            "rlm",
            // work_update 是 v0.9.5 隐藏的 replay 别名，模型目录里只有 canonical
            // `todo_write`；别名写进白名单是死条目，且会让进度工具整体不可见。
            "work_update",
            "checklist_write",
            "update_plan",
        ] {
            assert!(
                !is_pinvou3_allowed(excluded),
                "非 Pinvou 工具家族 {excluded} 不应进入白名单"
            );
        }

        assert_eq!(
            PINVOU3_ALWAYS_LOADED_TOOLS,
            &["request_user_input", "image_analyze"]
        );
        assert!(PINVOU3_ALLOWED_TOOLS.contains(&"mcp_*"));
    }
}

#[cfg(test)]
mod navigation_policy_tests {
    #[test]
    fn embedded_srcdoc_navigation_is_allowed_without_broadening_schemes() {
        for allowed in ["about:blank", "about:srcdoc"] {
            let url = tauri::Url::parse(allowed).unwrap();
            assert!(super::allow_embedded_document_navigation(&url, true));
            assert!(
                !super::allow_embedded_document_navigation(&url, false),
                "embedded documents must not become the initial main origin"
            );
        }
        for blocked in [
            "about:config",
            "about:blank?next=https://example.com",
            "data:text/html,hello",
            "https://example.com/",
            "file:///etc/passwd",
        ] {
            let url = tauri::Url::parse(blocked).unwrap();
            assert!(
                !super::allow_embedded_document_navigation(&url, true),
                "must not classify as embedded document: {blocked}"
            );
        }
    }
}

#[cfg(test)]
mod release_env_defaults_guard {
    /// 全量快照进程环境，保证调用真实 `ensure_release_env` 后完整还原 PATH、UI 环境
    /// 及测试原有变量；使用 OsString 兼容 POSIX 非 UTF-8 环境变量。
    struct EnvSnapshot(std::collections::HashMap<std::ffi::OsString, Option<std::ffi::OsString>>);

    impl EnvSnapshot {
        fn take() -> Self {
            let mut map = std::collections::HashMap::new();
            for (key, value) in std::env::vars_os() {
                map.insert(key, Some(value));
            }
            for key in ["DEEPSEEK_MAX_OUTPUT_TOKENS", "PINVOU3_MAX_OUTPUT_TOKENS"] {
                map.entry(std::ffi::OsString::from(key)).or_insert(None);
            }
            Self(map)
        }
    }

    impl Drop for EnvSnapshot {
        fn drop(&mut self) {
            for (key, value) in &self.0 {
                match value {
                    Some(value) => std::env::set_var(key, value),
                    None => std::env::remove_var(key),
                }
            }
            let keep: std::collections::HashSet<&std::ffi::OsString> = self.0.keys().collect();
            let stale: Vec<std::ffi::OsString> = std::env::vars_os()
                .map(|(key, _)| key)
                .filter(|key| !keep.contains(key))
                .collect();
            for key in stale {
                std::env::remove_var(key);
            }
        }
    }

    /// Community #210 守卫：release 启动路径不得重新注入全局输出上限。
    #[test]
    fn release_env_defaults_must_not_pin_global_output_cap() {
        let _lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let _snapshot = EnvSnapshot::take();

        assert!(
            !super::RELEASE_ENV_DEFAULTS
                .iter()
                .any(|(key, _)| *key == "DEEPSEEK_MAX_OUTPUT_TOKENS"),
            "RELEASE_ENV_DEFAULTS 不得包含 DEEPSEEK_MAX_OUTPUT_TOKENS"
        );
        assert!(
            !super::RELEASE_ENV_DEFAULTS
                .iter()
                .any(|(key, _)| *key == "PINVOU3_MAX_OUTPUT_TOKENS"),
            "RELEASE_ENV_DEFAULTS 不得包含 PINVOU3_MAX_OUTPUT_TOKENS"
        );

        std::env::remove_var("DEEPSEEK_MAX_OUTPUT_TOKENS");
        std::env::remove_var("PINVOU3_MAX_OUTPUT_TOKENS");
        super::ensure_release_env();
        assert!(
            std::env::var_os("DEEPSEEK_MAX_OUTPUT_TOKENS").is_none(),
            "ensure_release_env 不得重新注入 DEEPSEEK_MAX_OUTPUT_TOKENS"
        );
        assert!(
            std::env::var_os("PINVOU3_MAX_OUTPUT_TOKENS").is_none(),
            "ensure_release_env 不得重新注入 PINVOU3_MAX_OUTPUT_TOKENS"
        );
    }
}
