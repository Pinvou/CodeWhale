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

/// 跨平台的商店边界查询：按模型名查商店库，命中返回商店里的规范名。
/// 设备管理（features/device）的外部模型登记/移除用它拒绝"把商店模型当成
/// 外部模型"的越界操作。Linux 查 sqlite；其他平台没有商店，恒为 None。
/// 查询出错按未命中处理——商店库打不开时商店自身也不可用，不存在"误接管"。
#[cfg(target_os = "linux")]
pub(crate) fn store_model_name(model_name: &str) -> Option<String> {
    linux::db::get_store_model_by_name(model_name)
        .ok()
        .flatten()
        .map(|record| record.model_name)
}

#[cfg(not(target_os = "linux"))]
pub(crate) fn store_model_name(_model_name: &str) -> Option<String> {
    None
}
