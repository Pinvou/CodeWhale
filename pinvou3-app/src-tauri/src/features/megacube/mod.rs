//! MegaCube 本机管理与只读设备客户端。
//!
//! 落位说明：socket 客户端是「品悟 ⇆ 设备管理面」的领域能力，不是 OS 原语，
//! 故放 features/megacube 而非 platform/。消费方：
//! - `app/commands/cms.rs`（Tauri 命令薄壳，协议不变）
//! - `features/model_store`（委托启停 + 状态融合，feature→feature 单向依赖）
//! - `features/runtime_bundle`（megacube-admin MCP 条件注册）
//!
//! 本机模型动作须核实服务与每卡管理权；本机配置或历史不明时不回退旧脚本。
//! 工具注册独立判断：本机配置或安装记录可保留下载工具，
//! 不因 cmsd 暂停而丢失安装能力，也不因此取得模型管理权。

mod authority;
mod client;
pub mod delegate;
pub mod device;
mod platform;
pub mod service;

// 公共面：Client 供命令层；delegate 供 model_store；socket_path 供
// runtime_bundle 的 megacube-admin 条件注册。
pub(crate) use client::validate_card_id;
pub use client::{Client, socket_path};

pub fn installation_records_dir() -> std::path::PathBuf {
    crate::platform::paths::pinvou3_home()
        .join("megacube")
        .join("installations")
}

pub fn tooling_available() -> bool {
    authority::local_authority_expected()
        || authority::present_or_unknown(&installation_records_dir())
}

pub(crate) fn local_authority_expected() -> bool {
    authority::local_authority_expected()
}
