//! 模型商店的 Linux 实现。
//!
//! 这些模块整体搬自 LMM（`Pinvou/pinvou-models-cube`，原 `src-tauri/src/core/`），
//! 那是一套在 H3C MegaCube / NVIDIA GB10 真机上跑了两个多月的实现。**搬而不是重写**
//! 是有意的：每个文件里的注释都记着某个坑的来龙去脉（取消等于失败、pty 必须关回显、
//! 静默超时要分档、容器以 root 写进挂载目录的文件删不掉……），重写必然丢掉一部分，
//! 而丢掉的那部分就是下次重新踩的坑。
//!
//! 模块职责：
//!
//! | 模块 | 职责 |
//! |---|---|
//! | `config` | 设置与路径（权重目录、暂存目录；商店地址是固定常量） |
//! | `db` | sqlite 卡片库：模型记录、状态机、脚本本地固化副本、API Key |
//! | `store_client` | 商店 API 客户端，按 sha256 内容寻址取脚本和附属文件 |
//! | `script_runner` | **最核心**：pty 执行卡片脚本，解析进度/交互提示，静默超时，取消 |
//! | `lifecycle` | 六个入口脚本的编排（下载/装引擎/部署/启用/停用/卸载） |
//! | `run_state` | 运行中任务注册表、日志缓冲、卡片级并发互斥、下载队列 |
//! | `run_log_store` | 运行日志落盘（`~/.pinvou3/model-store/logs/`）+ 脱敏 + 轮转 |
//! | `local_service` | 本地下载服务（service.sock）：cmsd 等本机入口经它委托商店执行下载 |
//! | `orphans` | 启动时回收上次没收干净的**脚本进程**（权重一个字节都不动） |
//! | `errors` | 把技术错误翻译成用户看得懂的话 |
//! | `disk` | 字节格式化与目录体积统计 |
//! | `import_scan` | 下载前去哪找已在本机的权重（可配的搜索根目录） |
//! | `lan_gateway` / `lan_store` / `lan_network` | 主动开启的模型级 LAN 认证代理、数据库适配及接口/原端口隔离证据 |
//!
//! 相对 LMM **刻意没有搬**的：`admin.rs`（上游 919 行，本地 HTTP 管理接口——那是 sidecar
//! 形态的产物，原生实现后前端直接走 Tauri 命令）、`updater.rs`（LMM 自己的 OTA，
//! 品悟有自己的更新机制）、`system_probe.rs`（硬件探测）。
//! `worker.rs`（双机从机控制）**已搬入**——下方 `pub mod worker;`，round-3 P3
//! 勘误：本行此前仍把它列在「没有搬」清单里，与本文件自己的模块声明矛盾。
//! LAN 共享由本模块自己的窄代理实现，不复用 sidecar 管理接口。

pub mod background;
pub mod boundary;
mod card_assets;
pub mod card_update;
pub mod cmsd_inventory;
pub mod config;
mod control_file;
pub mod db;
mod delegated_run;
pub mod deploy_profiles;

// 商店边界在 platform/mod.rs 经 `use linux as backend` 转发，必须先在本模块根
// re-export（boundary 是子模块，别名不透传子模块条目）。
pub(crate) use boundary::{
    ollama_base_url, ollama_base_url_checked, store_model_name, store_model_name_checked,
    store_weights_roots, store_weights_roots_checked, with_weight_removal_boundary,
};
pub mod disk;
pub mod errors;
pub mod execution_fence;
mod host_control;
pub mod import_scan;
mod incomplete;
mod lan_gateway;
mod lan_network;
mod lan_store;
pub mod lifecycle;
mod linked_models;
pub mod local_service;
mod log_page;
pub mod maintenance;
pub(crate) use linked_models::matches_current_model_route;
mod memory_policy;
pub mod native_card_compat;
mod native_process;
mod native_runtime;
pub mod orphan_dirs;
pub mod orphans;
pub mod proc_timeout;
pub mod run_log_store;
pub mod run_state;
mod runtime_guard;
pub mod script_runner;
pub mod service;
pub mod startup_updates;
pub mod store_client;
pub mod version_store;
pub mod weight_identity;
pub mod worker;

// 服务层的操作直接对外，签名与 `platform/unsupported.rs` 里的桩一一对应。
// 命令层因此不必分平台分支。
pub use lan_store::{lan_status, revoke_api_key, set_lan_sharing};
pub use service::{
    ModelStoreService, active_runs, answer_run, api_keys, cancel_run, check_card_update,
    create_api_key, delete_engine, dismiss_incomplete, export_run_log, follow_deployment_context,
    force_remove, installed_engines, library, list_cards, local_resources, memory_admission,
    model_detail, preview_incomplete, remove_orphan, retry_incomplete, reveal_run_logs_dir,
    run_log, run_startup_cleanup, run_startup_linked_sync, scan_orphans, set_storage_settings,
    start_card_scripts, start_card_update, start_deploy, start_download, start_lifecycle,
    start_version_action, startup_update_check, storage_settings, take_startup_cleanup, versions,
};

// 可见性契约：`installed_weight_paths_checked` 与 `installed_weight_candidate_names_checked`
// 是 crate 内部的设备守卫助手（service.rs 里 pub(crate)，非公开命令面），所以
// 不进上面的 `pub use`——pub 重导出 crate 可见项是 E0364（Linux CI 编译错误
// 2026-11-04）。下游 platform/mod.rs 与 model_store/mod.rs 的 `pub(crate) use`
// 两跳照常转发。
pub(crate) use service::{
    installed_import_markers_checked, installed_ownership_unconfirmable_checked,
    installed_weight_candidate_names_checked, installed_weight_filenames_checked,
    installed_weight_paths_checked, installed_weight_paths_for_removal_checked,
};

/// Linux 上模型商店总是可用的。
///
/// 刻意**不**在这里探测 docker 在不在、GPU 有没有：那些是「这张卡能不能装」的问题，
/// 该在真正安装时由卡片脚本回答（它比我们更清楚自己需要什么）。这里回答的是更前面
/// 一个问题——「这个界面该不该存在」。把两者混在一起的后果是：docker 没起来时侧栏
/// 入口整个消失，用户连一句解释都看不到。
pub(crate) fn available() -> bool {
    true
}

pub(crate) fn unavailable_reason() -> Option<String> {
    None
}
