//! DeepSeek-TUI library — pinvou-platform 复用的底层能力。

use rust_i18n::i18n;
i18n!("locales", fallback = ["en"]);

#[cfg(test)]
pub mod test_support;

pub mod artifacts;
pub mod audit;
pub mod auto_reasoning;
pub mod automation_manager;
pub mod child_env;
pub mod client;
pub mod codex_model_cache;
pub mod command_safety;
pub mod commands;
pub mod compaction;
pub mod composer_history;
pub mod composer_stash;
pub mod config;
pub mod config_persistence; // [pinvou3-fork C1] v0.8.57 上游新增,facade 同步
pub mod config_ui;
pub mod context_budget; // [pinvou3-fork C1] v0.8.65 上游新增(context 预算),facade 同步
pub mod context_report; // [pinvou3-fork C1] v0.8.60 上游新增(context 用量报告),facade 同步
pub mod core;
pub mod cost_status;
pub mod deepseek_theme;
pub mod dependencies;
pub mod error_taxonomy;
pub mod eval;
pub mod execpolicy;
pub mod fast_hash;
pub mod features;
pub mod fleet; // [pinvou3-fork C1] v0.8.60 上游新增(Agent Fleet 工作者运行时),facade 同步
pub mod goal_loop; // [pinvou3-fork C1] v0.8.65 上游新增(goal loop 运行时),facade 同步
pub mod hashing;
pub mod hooks;
pub mod llm_client;
pub mod llm_response_cache; // [pinvou3-fork C1] v0.8.57 上游新增,facade 同步
pub mod localization;
pub mod logging;
pub mod lsp;
pub mod mcp;
pub mod mcp_server;
pub mod memory;
pub mod model_catalog; // [pinvou3-fork C1] v0.8.65 上游新增(模型目录),facade 同步
pub mod model_context;
pub mod model_inventory; // [pinvou3-fork C1] v0.8.60 上游新增(模型清单),facade 同步
pub mod model_profile; // [pinvou3-fork C1] v0.8.65 上游新增(模型档案),facade 同步
pub mod model_registry; // [pinvou3-fork C1] v0.8.65 上游新增(模型注册表),facade 同步
pub mod model_routing; // [pinvou3-fork C1] v0.8.57 上游新增(模型解析迁出 models.rs),facade 同步
pub mod models;
pub mod models_dev_live;
pub mod network_policy;
pub mod oauth; // [pinvou3-fork C1] v0.8.57 上游新增(openai-codex provider),facade 同步
pub mod palette;
pub mod plugins;
pub mod prefix_cache;
pub mod pricing;
pub mod project_context;
pub mod project_context_cache; // [pinvou3-fork C1] v0.8.57 上游新增(跨会话上下文缓存),facade 同步
// [pinvou3-fork C1] prompt_persist 于 v0.8.60 被上游删除(prompt 持久化重构),facade 同步移除孤儿声明
pub mod prompt_zones;
pub mod prompts;
pub mod provider_lake;
pub mod provider_readiness;
pub mod purge;
pub mod regex_cache;
pub mod remote_setup; // [pinvou3-fork C1] v0.8.65 上游新增(远程会话搭建),facade 同步
pub mod repl;
pub mod repo_law;
pub mod request_tuning; // [pinvou3-fork C1] v0.8.65 上游新增(请求调参),facade 同步
pub mod resource_telemetry; // [pinvou3-fork C1] v0.8.65 上游新增(资源遥测),facade 同步
pub mod retry_status;
pub mod rlm;
pub mod route_billing;
pub mod route_budget; // [pinvou3-fork C1] v0.8.65 上游新增(路由预算),facade 同步
pub mod route_runtime; // [pinvou3-fork C1] v0.8.65 上游新增(路由运行时),facade 同步
pub mod runtime_api;
pub mod runtime_log;
pub mod runtime_threads;
pub mod sandbox;
pub mod scorecard;
pub mod seam_manager;
#[allow(dead_code)]
pub mod session_diagnostics;
pub mod session_manager;
pub mod settings;
pub mod shell_dispatcher;
pub mod skill_state;
pub mod skills;
pub mod slop_ledger;
pub mod snapshot;
pub mod startup_trace;
pub mod task_manager;
pub mod tls; // [pinvou3-fork C1] v0.8.57 上游新增(insecure_skip_tls_verify),facade 同步
pub mod tool_output_receipts;
pub mod tools;
pub mod tui;
pub mod utils;
pub mod vision;
pub mod worker_profile; // [pinvou3-fork C1] v0.8.65 上游新增(Fleet 工作者档案),facade 同步
pub mod working_set;
pub mod workspace_discovery;
pub mod workspace_trust;
pub mod xai_oauth;
