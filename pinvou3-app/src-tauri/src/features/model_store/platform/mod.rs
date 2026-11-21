//! 平台适配层：模型商店的真实实现按目标平台二选一。
//!
//! cfg 收在这里而不是散在 feature 里，是仓库的硬约定——架构守卫只放行
//! `/platform/` 下的平台条件编译（见 `scripts/architecture-guard.py` 的
//! `target_cfg_allowed`）。同形状的先例：
//! `features/shared_knowledge_host/platform/mod.rs`。
//!
//! **为什么模型商店是纯 Linux 功能**：整条链路要 docker（起推理引擎容器）、
//! pty（跑卡片脚本并解析交互提示）、pkexec（装引擎时提权），目标设备是
//! H3C MegaCube / NVIDIA GB10（ARM64 Linux）。Windows 和 macOS 上这些前提
//! 一个都不成立，所以那边只给一个 `available: false` 的桩，侧栏入口整个不渲染。

// 两边都必须是 `pub mod`，不能一边 pub 一边私有。
//
// 下面那两行是**模块级 re-export**（`use <模块> as backend`），rustc 对非 pub 模块
// 的模块级 re-export 无条件报 E0365。写成私有 `mod unsupported;` 时，Linux 上根本
// 编不到这一支所以一直没露头，而 Windows/macOS 一编就是 E0365 加 20 处级联 E0603。
//
// 提成 pub 不会让任何名字逃出 crate：`platform` 和 `model_store` 这两层本身都是私有的。
#[cfg(target_os = "linux")]
pub mod linux;
#[cfg(not(target_os = "linux"))]
pub mod unsupported;

#[cfg(target_os = "linux")]
pub(crate) use linux as backend;
#[cfg(not(target_os = "linux"))]
pub(crate) use unsupported as backend;

// 商店边界（features/device 的外部模型登记/移除消费）。与文件内其余平台差异
// 同一约定：具体实现住 linux/（boundary.rs）与 unsupported/，这里只做无 cfg 的
// 转发——第四轮评审前这里曾内联两套 #[cfg] fn，与模块级约定并存成第二套写法。
pub(crate) use backend::{
    installed_import_markers_checked, installed_ownership_unconfirmable_checked,
    installed_weight_candidate_names_checked, installed_weight_filenames_checked,
    installed_weight_paths_checked, installed_weight_paths_for_removal_checked, ollama_base_url,
    ollama_base_url_checked, store_model_name, store_model_name_checked, store_weights_roots,
    store_weights_roots_checked, with_weight_removal_boundary,
};

// lib.rs 启动路径直接调用的后台对账（r36 P2-1）：与上面边界函数同约定走
// backend 转发，非 Linux 是 no-op——启动同步没有用户面，不适用「回不支持」话术。
pub(crate) use backend::linked_models_startup_sync;
