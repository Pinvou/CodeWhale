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

#[cfg(target_os = "linux")]
pub mod linux;
#[cfg(not(target_os = "linux"))]
mod unsupported;

#[cfg(target_os = "linux")]
pub(crate) use linux as backend;
#[cfg(not(target_os = "linux"))]
pub(crate) use unsupported as backend;
