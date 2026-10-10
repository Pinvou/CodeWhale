//! Linux 专属设备信息读取：DMI 产品名、os-release、hwmon 风扇/电压/电流/功率、
//! 网口 operstate、NVIDIA 内核驱动版本。全部只读，读取失败一律 None / 空集合
//! （graceful degrade，与 monitor 采样同一原则）。

use std::path::{Path, PathBuf};

use super::ExtraSensor;

/// DMI 产品名：候选字段清单与读取收敛在 `platform::dmi`（能力位同一份清单），
/// 这里取第一个非空字段用于设备页展示。
pub(crate) fn dmi_product_name() -> Option<String> {
    crate::platform::dmi::dmi_first_nonempty()
}

/// `/etc/os-release` 的 `PRETTY_NAME="..."`（引号可省略）；`/etc` 没有时按规范
/// 回退 `/usr/lib/os-release`（usrmerge/部分容器镜像里 /etc 不必有这份文件）。
pub(crate) fn os_pretty_name() -> Option<String> {
    std::fs::read_to_string("/etc/os-release")
        .or_else(|_| std::fs::read_to_string("/usr/lib/os-release"))
        .ok()
        .and_then(|text| parse_os_release_pretty_name(&text))
}

fn parse_os_release_pretty_name(text: &str) -> Option<String> {
    for line in text.lines() {
        if let Some(("PRETTY_NAME", value)) =
            line.split_once('=').map(|(k, v)| (k.trim(), v.trim()))
        {
            let value = value.trim_matches('"').trim_matches('\'').trim();
            if !value.is_empty() {
                return Some(value.to_string());
            }
        }
    }
    None
}

/// 网口是否 up：读 `/sys/class/net/<name>/operstate`。读不到返回 None（调用方
/// 默认 true）。**"unknown" 不算 down**——回环口恒为 unknown，tun/wg 等 VPN 口
/// 也多为 unknown，判成 down 会给用户持续的假"断开"信号；只有内核明确报告
/// down 家族（down/lowerLayerDown/notPresent）或 dormant（无链路）才判断开。
pub(crate) fn iface_operstate_up(name: &str) -> Option<bool> {
    let text =
        std::fs::read_to_string(Path::new("/sys/class/net").join(name).join("operstate")).ok()?;
    Some(!matches!(
        text.trim(),
        "down" | "lowerlayerdown" | "notpresent" | "dormant"
    ))
}

/// 网口是否为无线网卡：存在 `wireless` 子目录（/proc/net/wireless 的旧式暴露，
/// 非 `wl` 前缀命名的网卡也能识别）。
pub(crate) fn iface_has_wireless(name: &str) -> bool {
    Path::new("/sys/class/net")
        .join(name)
        .join("wireless")
        .exists()
}

/// CUDA toolkit 版本：`/usr/local/cuda*/version.json` 的 `cuda.version`。多套
/// toolkit 并存时按 (major, minor) **数值**取最新——字典序会把 "cuda-12.10" 排到
/// "cuda-12.2" 之后，数值序才符合"最新"直觉。
pub(crate) fn cuda_toolkit_version() -> Option<String> {
    let mut dirs: Vec<PathBuf> = std::fs::read_dir("/usr/local")
        .ok()?
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .filter(|path| {
            path.file_name()
                .and_then(|name| name.to_str())
                .is_some_and(|name| name.starts_with("cuda"))
        })
        .collect();
    dirs.sort_by_key(|dir| cuda_dir_sort_key(dir));
    dirs.into_iter().rev().find_map(|dir| {
        let text = std::fs::read_to_string(dir.join("version.json")).ok()?;
        crate::features::device::parse_cuda_toolkit_version(&text)
    })
}

/// 目录名 → (major, minor) 排序键：`cuda-12.10` → (12, 10)；`cuda`、解析失败
/// 一律 (0, 0)（排最前，被任何带版本号的候选盖过）。
fn cuda_dir_sort_key(dir: &Path) -> (u32, u32) {
    let Some(name) = dir.file_name().and_then(|name| name.to_str()) else {
        return (0, 0);
    };
    let Some(suffix) = name.strip_prefix("cuda-") else {
        return (0, 0);
    };
    let mut parts = suffix.split('.');
    let major = parts.next().and_then(|part| part.parse().ok()).unwrap_or(0);
    let minor = parts.next().and_then(|part| part.parse().ok()).unwrap_or(0);
    (major, minor)
}

/// NVIDIA 内核驱动版本：`/proc/driver/nvidia/version` 首行。nvidia-smi 不可用时
/// 的兜底（GPU 扩展快照用它判断"本机到底有没有 NVIDIA"）。
pub(crate) fn nvidia_driver_version() -> Option<String> {
    let text = std::fs::read_to_string("/proc/driver/nvidia/version").ok()?;
    let line = text.lines().next()?;
    parse_nvidia_version_line(line)
}

/// 解析形如
/// `NVRM version: NVIDIA UNIX Open Kernel Module for aarch64  540.4.0  Release Build ...`
/// 的行：定位 `NVIDIA UNIX` 之后，取第一个形如 `N.N...` 的纯数字段号。
fn parse_nvidia_version_line(line: &str) -> Option<String> {
    let rest = line.split("NVIDIA UNIX").nth(1)?;
    rest.split_whitespace()
        .find(|token| looks_like_version(token))
        .map(|token| token.to_string())
}

fn looks_like_version(token: &str) -> bool {
    let mut segments = token.split('.');
    let first = segments.next().unwrap_or("");
    if first.is_empty() || !first.bytes().all(|b| b.is_ascii_digit()) {
        return false;
    }
    let mut count = 1usize;
    for segment in segments {
        count += 1;
        if segment.is_empty() || !segment.bytes().all(|b| b.is_ascii_digit()) {
            return false;
        }
    }
    count >= 2
}

/// 注册表目录只留 owner 权限（0700）：外部模型记录含 base_url/weights_path，
/// 同机其他用户不该读到。
pub(crate) fn harden_dir_owner_only(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700));
}

/// 注册表文件只留 owner 权限（0600），rename 落盘前调用。
pub(crate) fn set_file_owner_only(file: &std::fs::File) {
    use std::os::unix::fs::PermissionsExt;
    let _ = file.set_permissions(std::fs::Permissions::from_mode(0o600));
}

/// hwmon 风扇/电压/电流/功率采样（温度交给 sysinfo Components，这里跳过温度通道）。
/// chip 取 hwmon 的 `name` 文件；label 取 `<前缀>N_label`，否则 `<chip> <通道号>`。
pub(crate) fn extra_sensors() -> Vec<ExtraSensor> {
    let mut out = Vec::new();
    let Ok(entries) = std::fs::read_dir("/sys/class/hwmon") else {
        return out;
    };
    let mut hwmon_dirs: Vec<PathBuf> = entries
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .collect();
    hwmon_dirs.sort();
    for dir in hwmon_dirs {
        collect_hwmon_chip(&dir, &mut out);
    }
    out
}

fn collect_hwmon_chip(dir: &Path, out: &mut Vec<ExtraSensor>) {
    let Some(chip) = read_trimmed_non_empty(&dir.join("name")) else {
        return;
    };
    let Ok(files) = std::fs::read_dir(dir) else {
        return;
    };
    let mut file_names: Vec<String> = files
        .filter_map(|entry| entry.ok())
        .filter_map(|entry| entry.file_name().into_string().ok())
        .collect();
    file_names.sort();
    for file_name in file_names {
        let Some((kind, hwmon_prefix, channel)) = parse_hwmon_input(&file_name) else {
            continue;
        };
        let Some(raw) = read_f64(&dir.join(&file_name)) else {
            continue;
        };
        let label = read_trimmed_non_empty(&dir.join(format!("{hwmon_prefix}{channel}_label")))
            .unwrap_or_else(|| format!("{chip} {channel}"));
        let factor = sensor_unit_factor(kind);
        let critical =
            read_f64(&dir.join(format!("{hwmon_prefix}{channel}_crit"))).map(|raw| raw * factor);
        let max =
            read_f64(&dir.join(format!("{hwmon_prefix}{channel}_max"))).map(|raw| raw * factor);
        out.push((
            chip.clone(),
            kind.to_string(),
            label,
            raw * factor,
            critical,
            max,
        ));
    }
}

/// 解析 hwmon 的 `*_input` 文件名 → (归一化 kind, hwmon 前缀, 通道号)。
/// 只认 fan/in/curr/power；温度（temp）由 sysinfo Components 负责，跳过。
fn parse_hwmon_input(file_name: &str) -> Option<(&'static str, &'static str, u32)> {
    if file_name.starts_with("temp") {
        return None;
    }
    let stem = file_name.strip_suffix("_input")?;
    let split = stem.find(|c: char| c.is_ascii_digit())?;
    let (raw_prefix, channel) = stem.split_at(split);
    let channel: u32 = channel.parse().ok()?;
    // match 字面量 → kind/prefix 都是 'static，与入参生命周期解耦。
    let (kind, prefix) = match raw_prefix {
        "fan" => ("fan", "fan"),
        "curr" => ("current", "curr"),
        "in" => ("voltage", "in"),
        "power" => ("power", "power"),
        _ => return None,
    };
    Some((kind, prefix, channel))
}

/// hwmon 原始读数 → 设计 §2.2 的归一化单位：RPM 原样、mV→V、mA→A、µW→W。
fn sensor_unit_factor(kind: &str) -> f64 {
    match kind {
        "voltage" | "current" => 0.001,
        "power" => 0.000_001,
        _ => 1.0,
    }
}

fn read_trimmed_non_empty(path: &Path) -> Option<String> {
    let text = std::fs::read_to_string(path).ok()?;
    let trimmed = text.trim();
    if trimmed.is_empty() {
        None
    } else {
        Some(trimmed.to_string())
    }
}

fn read_f64(path: &Path) -> Option<f64> {
    std::fs::read_to_string(path)
        .ok()?
        .trim()
        .parse::<f64>()
        .ok()
        .filter(|value| value.is_finite())
}

#[cfg(test)]
mod tests {
    #[test]
    fn cuda_dir_sort_key_orders_numerically_not_lexicographically() {
        assert_eq!(
            cuda_dir_sort_key(Path::new("/usr/local/cuda-12.10")),
            (12, 10)
        );
        assert_eq!(
            cuda_dir_sort_key(Path::new("/usr/local/cuda-12.2")),
            (12, 2)
        );
        // 字典序会判 12.2 更大;数值序正确判 12.10 更新。
        assert!(
            cuda_dir_sort_key(Path::new("/usr/local/cuda-12.10"))
                > cuda_dir_sort_key(Path::new("/usr/local/cuda-12.2"))
        );
        assert_eq!(cuda_dir_sort_key(Path::new("/usr/local/cuda")), (0, 0));
    }

    use super::*;

    #[test]
    fn parse_os_release_extracts_quoted_pretty_name() {
        let text = "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\n";
        assert_eq!(
            parse_os_release_pretty_name(text),
            Some("Ubuntu 24.04.1 LTS".to_string())
        );
    }

    #[test]
    fn parse_os_release_tolerates_unquoted_and_whitespace() {
        assert_eq!(
            parse_os_release_pretty_name("PRETTY_NAME=Debian GNU/Python\n"),
            Some("Debian GNU/Python".to_string())
        );
        assert_eq!(
            parse_os_release_pretty_name("  PRETTY_NAME = \"Trimmed\"  \n"),
            Some("Trimmed".to_string())
        );
    }

    #[test]
    fn parse_os_release_returns_none_when_absent_or_empty() {
        assert_eq!(parse_os_release_pretty_name("ID=ubuntu\n"), None);
        assert_eq!(parse_os_release_pretty_name("PRETTY_NAME=\"\"\n"), None);
        assert_eq!(parse_os_release_pretty_name(""), None);
    }

    #[test]
    fn parse_nvidia_version_extracts_driver_number() {
        // GB10 (open kernel module) 实测格式。
        let line = "NVRM version: NVIDIA UNIX Open Kernel Module for aarch64  540.4.0  \
                    Release Build  (buildbrain@mobile-381-e3d37da383e7)";
        assert_eq!(parse_nvidia_version_line(line), Some("540.4.0".to_string()));
        // 专有模块老格式同样命中。
        let line = "NVRM version: NVIDIA UNIX x86_64 Kernel Module  550.54.14  Fri Feb 23";
        assert_eq!(
            parse_nvidia_version_line(line),
            Some("550.54.14".to_string())
        );
    }

    #[test]
    fn parse_nvidia_version_rejects_unrelated_lines() {
        assert_eq!(parse_nvidia_version_line("NVRM: no NVIDIA UNIX here"), None);
        assert_eq!(parse_nvidia_version_line(""), None);
    }

    #[test]
    fn parse_hwmon_input_recognizes_the_four_supported_kinds() {
        assert_eq!(parse_hwmon_input("fan1_input"), Some(("fan", "fan", 1)));
        assert_eq!(parse_hwmon_input("in0_input"), Some(("voltage", "in", 0)));
        assert_eq!(
            parse_hwmon_input("curr12_input"),
            Some(("current", "curr", 12))
        );
        assert_eq!(
            parse_hwmon_input("power1_input"),
            Some(("power", "power", 1))
        );
    }

    #[test]
    fn parse_hwmon_input_skips_temperature_and_unknown_entries() {
        // 温度通道由 sysinfo Components 负责，这里必须跳过。
        assert_eq!(parse_hwmon_input("temp1_input"), None);
        assert_eq!(parse_hwmon_input("humidity1_input"), None);
        assert_eq!(parse_hwmon_input("fan1_label"), None);
        assert_eq!(parse_hwmon_input("name"), None);
        assert_eq!(parse_hwmon_input("power1_average"), None);
        assert_eq!(parse_hwmon_input("in0_max"), None);
        assert_eq!(parse_hwmon_input("curr1_crit"), None);
        assert_eq!(parse_hwmon_input("fan_input"), None);
    }

    #[test]
    fn sensor_unit_factors_normalize_to_si_units() {
        // mV→V、mA→A、µW→W、RPM 原样。
        assert_eq!(sensor_unit_factor("voltage"), 0.001);
        assert_eq!(sensor_unit_factor("current"), 0.001);
        assert_eq!(sensor_unit_factor("power"), 0.000_001);
        assert_eq!(sensor_unit_factor("fan"), 1.0);
        // 浮点乘法有表示误差，按容差断言换算结果。
        assert!((3_300.0 * sensor_unit_factor("voltage") - 3.3).abs() < 1e-9);
        assert!((1_500_000_000.0 * sensor_unit_factor("power") - 1_500.0).abs() < 1e-6);
        assert_eq!(4_200.0 * sensor_unit_factor("fan"), 4_200.0);
    }

    #[test]
    fn extra_sensors_reads_the_host_hwmon_tree_without_panicking() {
        // 集成冒烟：真机上跑通即可；无 hwmon（或全为温度通道）时为空也合法。
        let sensors = extra_sensors();
        for (chip, kind, name, value, _critical, _max) in &sensors {
            assert!(!chip.is_empty());
            assert!(matches!(
                kind.as_str(),
                "fan" | "voltage" | "current" | "power"
            ));
            assert!(!name.is_empty());
            assert!(value.is_finite());
        }
    }
}
