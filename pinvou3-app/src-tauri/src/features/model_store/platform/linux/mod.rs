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
//! | `config` | 设置与路径（权重目录、暂存目录、商店地址） |
//! | `db` | sqlite 卡片库：模型记录、状态机、脚本本地固化副本、API Key |
//! | `store_client` | 商店 API 客户端，按 sha256 内容寻址取脚本和附属文件 |
//! | `script_runner` | **最核心**：pty 执行卡片脚本，解析进度/交互提示，静默超时，取消 |
//! | `lifecycle` | 六个入口脚本的编排（下载/装引擎/部署/启用/停用/卸载） |
//! | `run_state` | 运行中任务注册表、日志缓冲、卡片级并发互斥、下载队列 |
//! | `orphans` | 启动时回收上次没收干净的脚本进程和残留权重 |
//! | `errors` | 把技术错误翻译成用户看得懂的话 |
//! | `disk` | 字节格式化与目录体积统计 |
//! | `import_scan` | 下载前去哪找已在本机的权重（可配的搜索根目录） |
//! | `test_store_data` | 内置测试商店的卡片数据（脚本正文编进二进制） |
//!
//! 相对 LMM **刻意没有搬**的：`admin.rs`（1880 行，本地 HTTP 管理接口——那是 sidecar
//! 形态的产物，原生实现后前端直接走 Tauri 命令）、`updater.rs`（LMM 自己的 OTA，
//! 品悟有自己的更新机制）、`gateway.rs` / `worker.rs` / `system_probe.rs`（分别是
//! 局域网暴露、双机从机控制、硬件探测，排在后面的阶段）。

pub mod config;
pub mod db;
pub mod disk;
pub mod errors;
pub mod import_scan;
pub mod lifecycle;
pub mod orphans;
pub mod run_state;
pub mod script_runner;
pub mod service;
pub mod store_client;
pub mod test_store_data;

// 服务层的操作直接对外，签名与 `platform/unsupported.rs` 里的桩一一对应。
// 命令层因此不必分平台分支。
pub use service::{
    answer_run, api_keys, cancel_run, create_api_key, disable_test_store, force_remove, library,
    list_cards, model_detail, run_log, set_storage_settings, start_deploy, start_download,
    start_lifecycle, storage_settings, test_store_state, unlock_test_store, ModelStoreService,
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
