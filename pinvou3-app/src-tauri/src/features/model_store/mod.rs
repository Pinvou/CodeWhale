//! 模型商店：在本机下载、部署、启用模型，让品悟直接用上，全程不碰 API Key。
//!
//! # 它在做什么
//!
//! 从模型商店取**卡片**，按卡片里的六个 shell 脚本装引擎、下权重、部署、启用，
//! 本机跑起一个 OpenAI 兼容服务，品悟直接连 `127.0.0.1` 用它对话。
//!
//! ```text
//!  商店服务器                        本机
//! ┌──────────────┐          ┌──────────────────────────────┐
//! │ card.json    │  HTTPS   │ 卡片库(sqlite)               │
//! │ 六个脚本     │ ───────► │ 脚本执行器(pty) → docker 容器│
//! │ 附属文件     │ 内容寻址 │   + 权重目录(几十~上百 GB)   │
//! └──────────────┘          │      ↓                       │
//!                           │ http://127.0.0.1:<port>/v1   │
//!                           └──────────────────────────────┘
//! ```
//!
//! # 关键认知
//!
//! **模型商店本身不懂任何模型。** 它是一个脚本编排器 + 状态机。怎么装 vLLM、怎么起
//! SGLang、权重放哪，全写在卡片作者交付的脚本里。这个模块只负责：按顺序跑、把过程
//! 如实显示给用户、把结果记进库、出错时说人话。
//!
//! 这条边界是整个设计的地基——它让「上架一张新卡」不需要改客户端。
//!
//! # 状态机
//!
//! ```text
//!   可下载 ──下载──► 已下载 ──部署──► 已部署 ──启用──► 运行中
//!               ▲        │              │              │
//!               │        │              └──── 停用 ◄───┘
//!               └─卸载───┴────────────────────┘
//!   已下载 ──删除──► 可下载
//! ```
//!
//! **卸载和删除是两个动作**，区别不只是权重删不删，更是卡片留不留在库里：
//! 卸载后权重留在盘上、卡片退回「已下载」，下次点部署直接用；删除才真删权重并
//! 把卡片移出库。两件事必须同进同退——权重留着而记录没了，那几十 GB 就没有任何
//! 界面管得到了（真机上出过 67GB 的孤儿）。
//!
//! # 平台
//!
//! 只在 Linux 上真正编译，见 `platform/mod.rs` 的说明。

// pub(crate) 而非私有：features/device 的移除 fail-closed 端到端测试（住在
// device/platform/linux.rs——linux 门控区）需要经由本路径拿 linux::config::TempHome
// 做 HOME 隔离。测试依赖方向是 device→model_store（与运行时同向，不构成环）。
pub(crate) mod platform;

/// 非 Linux 平台上给用户看的原因。放在这里而不是桩文件里，是为了让
/// 「为什么没有这个功能」只有一处定义。
pub(crate) const UNSUPPORTED_REASON: &str = "模型商店只在 Linux 设备上可用：它需要 docker 起推理引擎、需要 pty 跑卡片脚本、\
     装引擎时需要提权，这些前提在 Windows 和 macOS 上都不成立。";

/// 这台机器上有没有模型商店。
///
/// 前端拿它决定侧栏入口渲不渲染——非 Linux 上整个不渲染，而不是渲染出来再报错。
#[derive(Debug, Clone, serde::Serialize)]
pub struct StoreAvailability {
    pub available: bool,
    /// 不可用时给用户看的话；可用时为 `None`。
    pub reason: Option<String>,
}

pub fn availability() -> StoreAvailability {
    StoreAvailability {
        available: platform::backend::available(),
        reason: platform::backend::unavailable_reason(),
    }
}

/// 进程内的模型商店状态，作为 Tauri managed state 注册一次。
///
/// 必须**全进程唯一**：它持有卡片级互斥表和下载队列，那是「同一张卡不许被两处
/// 同时操作」的唯一依据。各持一份等于没有约束——真机上踩过，两个 `hf download`
/// 抢同一批文件的锁，速度从 2.2MB/s 掉到 569KB/s。
pub type ModelStoreService = platform::backend::ModelStoreService;

// 操作一律透传到平台后端。这一层不加逻辑，只是把「Linux 真做 / 其他平台回一句
// 解释」这件事收在 platform 里，命令层不必分支。
pub use platform::backend::{
    active_runs, answer_run, api_keys, cancel_run, create_api_key, disable_test_store,
    force_remove, library, list_cards, model_detail, run_log, run_startup_cleanup,
    run_startup_refresh, set_storage_settings, start_deploy, start_download, start_lifecycle,
    storage_settings, take_startup_cleanup, test_store_state, unlock_test_store,
};

// 商店边界查询：供其他 feature（设备管理的外部模型登记/移除）判断某个名字
// 是否已被商店接管。见 platform::store_model_name 的说明。
pub(crate) use platform::store_model_name;
// 可失败版（移除路径 fail-closed）：Err = 商店库查询本身失败，移除端必须拒绝
// 操作而不是当未命中放行。见 platform::store_model_name_checked 的说明。
pub(crate) use platform::store_model_name_checked;
pub(crate) use platform::store_weights_roots;
// 可失败版：settings 读取失败时 Err。设备移除路径用（fail-closed，见
// platform::store_weights_roots_checked 的说明）。
pub(crate) use platform::store_weights_roots_checked;
// 已安装卡片实际引用的权重路径（可失败）：设备移除守卫的存量引用保护臂。
// 见 platform::installed_weight_paths_checked 的说明。
pub(crate) use platform::installed_weight_paths_checked;
// 在册卡片权重目录的约定名集合（可失败）：设备移除守卫的零痕迹旧根名字臂
// （旧版本自定义权重根不留痕，路径臂恢复不了，按名字 fail-closed 拦截）。
// 见 platform::installed_weight_candidate_names_checked 的说明。
pub(crate) use platform::installed_weight_candidate_names_checked;
// 本机 Ollama 守护进程基地址：设备管理的外部模型探测/标签/删除与商店生命周期
// 对齐到同一实例。见 platform::ollama_base_url 的说明。
pub(crate) use platform::ollama_base_url;
// 可失败版：settings 读取失败时 Err。破坏性的删除调用用（fail-closed，见
// platform::ollama_base_url_checked 的说明）。
pub(crate) use platform::ollama_base_url_checked;
