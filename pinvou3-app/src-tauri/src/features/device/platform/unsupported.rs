//! 非 Linux 平台的设备信息桩。
//!
//! 与仓库公约一致：不支持的能力显式返回 unsupported（None / 空集合），
//! 不静默借用其他平台的实现。MegaCube 设备管理本身只面向 Linux 一体机，
//! 非 Linux 平台上采样降级为：os 信息缺产品名、传感器只有 sysinfo 温度。

use std::path::Path;

use super::ExtraSensor;

pub(crate) fn dmi_product_name() -> Option<String> {
    None
}

pub(crate) fn os_pretty_name() -> Option<String> {
    None
}

pub(crate) fn extra_sensors() -> Vec<ExtraSensor> {
    Vec::new()
}

/// 非 Linux 上读不到 operstate → None（调用方按默认 up 处理）。
pub(crate) fn iface_operstate_up(_name: &str) -> Option<bool> {
    None
}

pub(crate) fn iface_has_wireless(_name: &str) -> bool {
    false
}

/// 非 Linux 平台不探测 CUDA toolkit（`/usr/local` 是 Linux 惯例路径）。
pub(crate) fn cuda_toolkit_version() -> Option<String> {
    None
}

pub(crate) fn nvidia_driver_version() -> Option<String> {
    None
}

// 引用 Path 保持签名对称（linux 侧实现用到），避免 unused-import 类告警差异。
const _: Option<&Path> = None;
