//! 设备信息平台适配层。
//!
//! cfg(target_os) 按仓库约定只允许出现在 `/platform/` 路径下（见
//! `scripts/architecture-guard.py` 的 `target_cfg_allowed`），所以 Linux 专属的
//! sysfs/procfs 读取全部收在 `linux.rs`，其余平台走 `unsupported.rs` 的显式
//! 桩（None / 空集合，不静默借用其他平台实现）。
//!
//! 权限收敛（`owner_only.rs`）是纯 `std::os::unix`：Linux 与 macOS 共用同一份
//! 实现（`cfg(unix)`），只有非 unix 平台走 unsupported 的空桩——此前 macOS 被
//! `cfg(not(target_os = "linux"))` 桩误伤，能加固的平台被静默放弃加固（第四轮
//! 评审 P2）。
//!
//! 上层 `features/device/mod.rs` 只依赖本模块的跨平台入口，不写任何 cfg。

/// `extra_sensors()` 的条目形状：`(chip, kind, name, value, critical, max)`。
/// value/critical/max 已归一化：温度 °C / 风扇 RPM / 电压 V / 电流 A / 功率 W。
pub(crate) type ExtraSensor = (String, String, String, f64, Option<f64>, Option<f64>);

#[cfg(target_os = "linux")]
mod linux;
#[cfg(unix)]
mod owner_only;
#[cfg(not(target_os = "linux"))]
mod unsupported;

#[cfg(target_os = "linux")]
pub(crate) use linux::{
    cuda_toolkit_version, dmi_product_name, extra_sensors, iface_has_wireless, iface_operstate_up,
    nvidia_driver_version, os_pretty_name,
};
#[cfg(not(target_os = "linux"))]
pub(crate) use unsupported::{
    cuda_toolkit_version, dmi_product_name, extra_sensors, iface_has_wireless, iface_operstate_up,
    nvidia_driver_version, os_pretty_name,
};
// 权限收敛按 unix/not(unix) 二分，而不是按目标 OS——macOS 同样享受 0700。
// 写入侧的文件级 0600 与落盘 fsync 由 platform::atomic_write_private 在底座
// 完成（第六轮评审 P2 起登记写入复用共享原子写）；这里提供目录级收敛，以及
// 读取路径对存量文件的重收敛（doctor 模板教 AI 自写注册表文件，chmod 可能被
// 跳过——第七轮评审 P3）。
#[cfg(unix)]
pub(crate) use owner_only::{harden_dir_owner_only, harden_file_owner_only};
#[cfg(not(unix))]
pub(crate) use unsupported::{harden_dir_owner_only, harden_file_owner_only};
