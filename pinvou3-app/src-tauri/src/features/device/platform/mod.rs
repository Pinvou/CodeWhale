//! 设备信息平台适配层。
//!
//! cfg(target_os) 按仓库约定只允许出现在 `/platform/` 路径下（见
//! `scripts/architecture-guard.py` 的 `target_cfg_allowed`），所以 Linux 专属的
//! sysfs/procfs 读取全部收在 `linux.rs`，其余平台走 `unsupported.rs` 的显式
//! 桩（None / 空集合，不静默借用其他平台实现）。
//!
//! 上层 `features/device/mod.rs` 只依赖本模块的跨平台入口，不写任何 cfg。

/// `extra_sensors()` 的条目形状：`(chip, kind, name, value, critical, max)`。
/// value/critical/max 已归一化：温度 °C / 风扇 RPM / 电压 V / 电流 A / 功率 W。
pub(crate) type ExtraSensor = (String, String, String, f64, Option<f64>, Option<f64>);

#[cfg(target_os = "linux")]
mod linux;
#[cfg(not(target_os = "linux"))]
mod unsupported;

#[cfg(target_os = "linux")]
pub(crate) use linux::{
    cuda_toolkit_version, dmi_product_name, extra_sensors, harden_dir_owner_only,
    iface_has_wireless, iface_operstate_up, nvidia_driver_version, os_pretty_name,
    set_file_owner_only,
};
#[cfg(not(target_os = "linux"))]
pub(crate) use unsupported::{
    cuda_toolkit_version, dmi_product_name, extra_sensors, harden_dir_owner_only,
    iface_has_wireless, iface_operstate_up, nvidia_driver_version, os_pretty_name,
    set_file_owner_only,
};
