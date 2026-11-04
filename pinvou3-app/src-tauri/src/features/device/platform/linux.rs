//! Linux 专属设备信息读取：DMI 产品名、os-release、hwmon 风扇/电压/电流/功率、
//! 网口 operstate、NVIDIA 内核驱动版本。全部只读，读取失败一律 None / 空集合
//! （graceful degrade，与 monitor 采样同一原则）。

use std::path::{Path, PathBuf};
use std::sync::OnceLock;

use super::ExtraSensor;

/// DMI 产品名：候选字段清单与读取收敛在 `platform::dmi`（能力位同一份清单），
/// 这里取第一个非空字段用于设备页展示。进程内 OnceLock 缓存（第九轮评审 P3，
/// 性能）：产品名是静态硬件事实，运行期不会变，而 `sample_snapshot` 每 2s 一次
/// 全量采样没必要反复扫 DMI 目录；读不到按 None 缓存，同样避免每拍重试一个
/// 必然失败的读取。缓存收在本文件（features/device 的平台适配层），不动
/// `platform/dmi.rs` 的共享读取实现。
pub(crate) fn dmi_product_name() -> Option<String> {
    static CACHE: OnceLock<Option<String>> = OnceLock::new();
    CACHE
        .get_or_init(crate::platform::dmi::dmi_first_nonempty)
        .clone()
}

/// `/etc/os-release` 的 `PRETTY_NAME="..."`（引号可省略）；`/etc` 没有时按规范
/// 回退 `/usr/lib/os-release`（usrmerge/部分容器镜像里 /etc 不必有这份文件）。
/// 同 DMI 产品名：进程内 OnceLock 缓存静态 OS 事实，不随 2s 采样反复读盘
/// （第九轮评审 P3）。解析逻辑仍是纯函数，测试直打不受缓存影响。
pub(crate) fn os_pretty_name() -> Option<String> {
    static CACHE: OnceLock<Option<String>> = OnceLock::new();
    CACHE
        .get_or_init(|| {
            std::fs::read_to_string("/etc/os-release")
                .or_else(|_| std::fs::read_to_string("/usr/lib/os-release"))
                .ok()
                .and_then(|text| parse_os_release_pretty_name(&text))
        })
        .clone()
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

/// operstate 文本 → 是否 up 的判定（纯函数，测试直打；读取见
/// [`iface_operstate_up`]）。**"unknown" 不算 down**——回环口恒为 unknown，
/// tun/wg 等 VPN 口也多为 unknown，判成 down 会给用户持续的假"断开"信号；
/// 只有内核明确报告 down 家族（down/lowerLayerDown/notPresent）或 dormant
/// （无链路）才判断开。
fn operstate_text_is_up(text: &str) -> bool {
    !matches!(
        text.trim(),
        "down" | "lowerlayerdown" | "notpresent" | "dormant"
    )
}

/// 网口是否 up：读 `/sys/class/net/<name>/operstate`。读不到返回 None（调用方
/// 默认 true）。
pub(crate) fn iface_operstate_up(name: &str) -> Option<bool> {
    let text =
        std::fs::read_to_string(Path::new("/sys/class/net").join(name).join("operstate")).ok()?;
    Some(operstate_text_is_up(&text))
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
        use std::io::Read;
        let file = std::fs::File::open(dir.join("version.json")).ok()?;
        let mut text = String::new();
        // version.json 是 root 所有的小文件；有界读取防异常环境下的巨文件占内存
        // （第七轮评审 P3），64KB 对真实 version.json 已放宽三个数量级。
        file.take(64 * 1024).read_to_string(&mut text).ok()?;
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

    // 符号链接逃逸对抗面：家目录之下、中间组件指向家目录之外的符号链接，
    // canonicalize 后 strip_prefix(home) 必须失败。守卫本体在
    // external_models.rs（features 层不能写 cfg），这个 unix 专属测试按架构
    // 约定落在平台适配层（第四轮评审测试盲区；曾因 #[cfg(unix)] 留在
    // features 层被架构守卫拦下）。
    #[test]
    fn weights_deletion_guard_rejects_symlink_escape_under_home() {
        use crate::features::device::external_models::weights_deletion_guard;
        let home = std::env::temp_dir().join(format!("guard-symlink-{}", std::process::id()));
        let outside =
            std::env::temp_dir().join(format!("guard-symlink-out-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&home);
        let _ = std::fs::remove_dir_all(&outside);
        std::fs::create_dir_all(&home).expect("create temp home");
        let outside_weights = outside.join("b").join("c");
        std::fs::create_dir_all(&outside_weights).expect("create outside weights");
        let link = home.join("a");
        std::os::unix::fs::symlink(&outside, &link).expect("create symlink");
        let smuggled = link.join("b").join("c");
        let result = weights_deletion_guard(&home, None, &[], &smuggled);
        let _ = std::fs::remove_dir_all(&home);
        let _ = std::fs::remove_dir_all(&outside);
        assert!(
            result.is_err(),
            "symlink escape must be rejected, got {:?}",
            result
        );
    }

    // 二十一轮评审 P2（变异实证的缺口）：read_registry 扫描时对**符号链接条目**
    // 跳过权限收敛（十九轮评审 P2 修复）——harden 的 set_permissions 会跟随链接把
    // 用户自己的目标文件 chmod 成 0600。此前该臂零覆盖：改成无条件收敛全套照绿。
    // 符号链接 + PermissionsExt 是 unix 专属，故与其它 symlink 用例同住本文件。
    #[test]
    fn read_registry_hardens_regular_records_but_never_chmods_symlink_targets() {
        use crate::features::device::external_models::read_registry;
        use std::os::unix::fs::PermissionsExt;
        let base = std::env::temp_dir().join(format!("registry-symlink-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        let dir = base.join("registry");
        let outside = base.join("outside");
        std::fs::create_dir_all(&dir).expect("create registry dir");
        std::fs::create_dir_all(&outside).expect("create outside dir");
        let regular = dir.join("m-a.json");
        std::fs::write(&regular, r#"{"id": "m-a", "name": "a", "kind": "other"}"#)
            .expect("write regular record");
        std::fs::set_permissions(&regular, std::fs::Permissions::from_mode(0o644))
            .expect("loosen regular record");
        let target = outside.join("user-file.json");
        std::fs::write(&target, r#"{"id": "m-b", "name": "b", "kind": "other"}"#)
            .expect("write symlink target");
        std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o644))
            .expect("loosen symlink target");
        std::os::unix::fs::symlink(&target, dir.join("m-b.json")).expect("create symlink record");
        let (models, _warnings) = read_registry(&dir);
        let regular_mode = std::fs::metadata(&regular)
            .expect("regular metadata")
            .permissions()
            .mode()
            & 0o777;
        let target_mode = std::fs::metadata(&target)
            .expect("target metadata")
            .permissions()
            .mode()
            & 0o777;
        let _ = std::fs::remove_dir_all(&base);
        assert_eq!(models.len(), 2, "symlinked record must still be readable");
        assert_eq!(
            regular_mode, 0o600,
            "regular record must be hardened to 0600"
        );
        assert_eq!(
            target_mode, 0o644,
            "symlink target (user file) must not be chmod'ed"
        );
    }

    // round-22 评审 P2：目录**本体**与文件条目同一纪律——注册表目录被换成指向
    // 用户其他目录的符号链接时，read_registry 的目录收敛不得跟随（harden 跟随
    // 链接会把目标目录 chmod 成 0700，group/other 位被无声收窄）。记录读取与
    // 文件条目收敛照常。
    #[test]
    fn read_registry_never_chmods_a_symlinked_registry_dir_target() {
        use crate::features::device::external_models::read_registry;
        use std::os::unix::fs::PermissionsExt;
        let base =
            std::env::temp_dir().join(format!("registry-dir-symlink-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        let outside = base.join("outside");
        std::fs::create_dir_all(&outside).expect("create target dir");
        std::fs::set_permissions(&outside, std::fs::Permissions::from_mode(0o755))
            .expect("loosen target dir");
        let record = outside.join("m-a.json");
        std::fs::write(&record, r#"{"id": "m-a", "name": "a", "kind": "other"}"#)
            .expect("write record");
        std::fs::set_permissions(&record, std::fs::Permissions::from_mode(0o644))
            .expect("loosen record");
        let link = base.join("registry");
        std::os::unix::fs::symlink(&outside, &link).expect("create registry dir symlink");
        let (models, _warnings) = read_registry(&link);
        let outside_mode = std::fs::metadata(&outside)
            .expect("target dir metadata")
            .permissions()
            .mode()
            & 0o777;
        let record_mode = std::fs::metadata(&record)
            .expect("record metadata")
            .permissions()
            .mode()
            & 0o777;
        let _ = std::fs::remove_dir_all(&base);
        assert_eq!(
            models.len(),
            1,
            "records behind a symlinked registry dir must stay readable"
        );
        assert_eq!(
            outside_mode, 0o755,
            "symlinked registry dir target must not be chmod'ed"
        );
        assert_eq!(record_mode, 0o600, "regular records are still hardened");
    }

    // 十九轮评审 P1：原始分量不含 blobs、末分量指向 daemon blob 存储的符号
    // 链接——canonical 目标才露出 blobs 布局，而环境臂对 systemd
    // OLLAMA_MODELS 迁移根（此处 decoy_root 是应用推导出的、与 daemon 真身
    // 无关的根）失明、深度恰 3 层放行。守卫必须对 canonical 目标复查签名
    // （修复前该用例 APPROVED，整删 daemon blobs；摘除复查变异核验必红）。
    // 与上一条同款落在平台适配层：features 层不能写 cfg。
    #[test]
    fn weights_deletion_guard_rechecks_blob_signature_on_the_canonicalized_target() {
        use crate::features::device::external_models::weights_deletion_guard;
        let home = std::env::temp_dir().join(format!("guard-canon-blobs-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&home);
        let decoy_root = home.join("mnt").join("ollama-models");
        std::fs::create_dir_all(&decoy_root).expect("create decoy root");
        let blobs = home.join(".ollama").join("models").join("blobs");
        std::fs::create_dir_all(&blobs).expect("create daemon blobs");
        let link_dir = home.join("deep").join("a").join("b");
        std::fs::create_dir_all(&link_dir).expect("create link dir");
        let smuggled = link_dir.join("weights");
        std::os::unix::fs::symlink(&blobs, &smuggled).expect("create symlink");
        let error =
            weights_deletion_guard(&home, None, std::slice::from_ref(&decoy_root), &smuggled)
                .expect_err("canonical blob layout must be re-checked");
        assert!(error.contains("blob"), "unexpected error: {error}");
        // 对照：同一形状、指向普通目录的符号链接照常放行（签名复查不误伤；
        // canonical 目标自身仍须过深度臂，故给足 3 层）。
        let plain = home.join("mnt").join("store").join("plain-weights");
        std::fs::create_dir_all(&plain).expect("create plain weights");
        let plain_link = link_dir.join("plain");
        std::os::unix::fs::symlink(&plain, &plain_link).expect("create plain symlink");
        assert!(
            weights_deletion_guard(&home, None, std::slice::from_ref(&decoy_root), &plain_link)
                .is_ok(),
            "plain symlinked weights must still pass"
        );
        let _ = std::fs::remove_dir_all(&home);
    }

    // 登记端 canonical 签名复查（十九轮评审 P1；第二十轮评审 P2 扩展
    // manifests）：原始路径无 daemon 布局分量、末分量指向 blob/manifests 存储
    // 的符号链接必须命中；不存在的路径按 false（登记容许先登记后建目录，
    // 移除端守卫兜底）。
    #[test]
    fn canonicalized_has_ollama_layout_follows_the_final_symlink() {
        use crate::features::device::external_models::canonicalized_has_ollama_layout;
        let home =
            std::env::temp_dir().join(format!("register-canon-blobs-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&home);
        let blobs = home.join("migrated").join("models").join("blobs");
        std::fs::create_dir_all(&blobs).expect("create blobs");
        let smuggled = home.join("weights-link");
        std::os::unix::fs::symlink(&blobs, &smuggled).expect("create symlink");
        assert!(canonicalized_has_ollama_layout(
            smuggled.to_string_lossy().as_ref()
        ));
        let manifests = home.join("migrated2").join("models").join("manifests");
        std::fs::create_dir_all(&manifests).expect("create manifests");
        let smuggled_manifests = home.join("manifests-link");
        std::os::unix::fs::symlink(&manifests, &smuggled_manifests).expect("create symlink");
        assert!(canonicalized_has_ollama_layout(
            smuggled_manifests.to_string_lossy().as_ref()
        ));
        assert!(!canonicalized_has_ollama_layout(
            home.join("not-created-yet").to_string_lossy().as_ref()
        ));
        let _ = std::fs::remove_dir_all(&home);
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

    /// 把 Ollama 守护进程地址指到必拒的死端口：e2e 会触发对 daemon 的真 HTTP
    /// （移除守卫 4 的 DELETE、列表的 GET /api/tags），TempHome 只隔离 HOME，
    /// daemon 地址不随 HOME 走——不注入的话，开着 Ollama 的开发机上跑测试会对
    /// 127.0.0.1:11434 发真请求（同名字模型会被真删，第八轮评审 P2）。
    /// 127.0.0.1:1 无监听，连接立即被拒，不吞超时预算。
    fn point_ollama_at_dead_port() {
        let dir = crate::features::model_store::platform::linux::config::config_dir();
        std::fs::create_dir_all(&dir).expect("config dir");
        std::fs::write(
            dir.join("config.json"),
            r#"{"ollama_host": "http://127.0.0.1:1"}"#,
        )
        .expect("write config.json");
    }

    /// 移除路径 fail-closed 的**端到端接线**：商店库损坏时设备侧
    /// `remove_external_model` 必须整单拒绝、注册记录原样保留。helper 自身的
    /// Err 语义已由 model_store boundary.rs 的
    /// `checked_reports_query_failure_instead_of_miss` 直打，这里钉的是调用点的
    /// `?` 传播——把调用点退回宽松 `.ok().flatten()`，本测试必须变红（五轮评审
    /// P3）。住在 linux 门控区是因为 TempHome 随 linux 模块编译，且依赖方向
    /// 保持 device→model_store 不构成环。
    #[test]
    fn device_removal_is_fail_closed_on_corrupt_store_db() {
        use crate::features::device::{
            get_external_models, register_external_model, remove_external_model,
        };
        use crate::features::model_store::platform::linux::config;
        let _ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async move {
            // register 返回完整视图记录，id 从中取（E0308 教训：linux 门控代码
            // 本地编译不到，签名误读只能靠 CI 暴露）。
            let registered = register_external_model(
                "guard-e2e-model".to_string(),
                "other".to_string(),
                None,
                None,
                None,
            )
            .await
            .expect("register external model");
            let id = registered.id;
            // 登记成功后把商店库覆写成垃圾字节：connect 能打开、查询必然失败。
            std::fs::create_dir_all(config::data_dir()).expect("store data dir");
            std::fs::write(
                config::data_dir().join("models.db"),
                b"not a sqlite db at all",
            )
            .expect("corrupt db");
            let result =
                remove_external_model(id.clone(), false, "guard-e2e-model".to_string(), None).await;
            let error = result.expect_err("corrupt store db must reject the removal");
            assert!(
                error.contains("已被拒绝"),
                "rejection must say so, got: {error}"
            );
            // 记录必须原样保留（经列表接口读回，而不是直查文件——registry_dir
            // 不跨 feature re-export）。
            let view = get_external_models().await.expect("list external models");
            assert!(
                view.models.iter().any(|model| model.id == id),
                "registry record must survive the rejected removal"
            );
        });
    }

    /// 移除端对 ollama 记录**跳过权重删除**的端到端接线（第七轮评审 P2）：
    /// 登记端已硬校验拒绝 ollama+weights_path（external_models 直打测试），这里
    /// 钉存量兜底——早期模板/AI 手写的 ollama 记录可能登记了权重路径，而
    /// `~/.ollama/models/blobs`（家目录下恰 3 层）能过守卫链的深度臂，不能让
    /// "删除外部模型"把守护进程的全部 blob 连商店部署模型的权重一起删光。
    /// 住在 linux 门控区：TempHome/db 初始化随 linux 模块编译。
    #[test]
    fn ollama_removal_skips_weight_deletion_even_with_registered_path() {
        use crate::features::device::external_models::registry_dir;
        use crate::features::device::remove_external_model;
        use crate::features::model_store::platform::linux::{config, db};
        let _ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        db::init_db().expect("init store db");
        // 权重目录放在家目录下第 3 层（守卫链深度臂本会放行的形状）。
        let weights = crate::platform::paths::user_home_dir()
            .join("ollama-weights")
            .join("deep")
            .join("blobs");
        std::fs::create_dir_all(&weights).expect("create weights dir");
        std::fs::write(weights.join("sha256-x"), b"blob bytes").expect("write blob");
        let dir = registry_dir();
        std::fs::create_dir_all(&dir).expect("registry dir");
        std::fs::write(
            dir.join("m-ollama01.json"),
            format!(
                r#"{{"schema":1,"id":"m-ollama01","name":"cleanup","kind":"ollama","model":"cleanup:latest","weights_path":{}}}"#,
                serde_json::to_string(&weights.to_string_lossy()).expect("json string")
            ),
        )
        .expect("write legacy ollama record");
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async move {
            let outcome = remove_external_model(
                "m-ollama01".to_string(),
                true,
                "cleanup".to_string(),
                // 守护进程绑定（2026-11-04 评审 P3）：确认面板带来的地址必须与
                // point_ollama_at_dead_port 写入的设置同源，否则守卫 2.5 整单拒绝。
                Some("http://127.0.0.1:1".to_string()),
            )
            .await
            .expect("removal must succeed");
            assert!(
                !outcome.removed_weights,
                "ollama weights must not be touched"
            );
            assert!(
                outcome
                    .warnings
                    .iter()
                    .any(|warning| warning.contains("不执行权重目录删除")),
                "skip must be disclosed, got: {:?}",
                outcome.warnings
            );
            assert!(outcome.removed_registry, "registry file should be removed");
            assert!(weights.exists(), "weight dir must survive");
        });
    }

    /// 非 ollama 记录 + `delete_weights=true` 的**端到端删除接线**（十二轮评审
    /// P2）：此前唯一的真删路径——守卫通过 → recheck 复核 → 删除时刻商店根复查
    /// → `remove_dir_all`——整段没有任何测试执行过，把 recheck 比对或复查整段
    /// 删掉、或改删原始路径，当时全部测试照绿。这里钉住"权重目录真的被删、
    /// 注册记录随之消失"；纯函数臂（深度/保护根/blob 签名）由 external_models
    /// 与上方符号链接逃逸测试直打。
    #[test]
    fn non_ollama_removal_deletes_guard_approved_weights_end_to_end() {
        use crate::features::device::{
            external_models::registry_dir, get_external_models, remove_external_model,
        };
        use crate::features::model_store::platform::linux::{config, db};
        use crate::platform::paths::user_home_dir;
        let _ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        db::init_db().expect("init store db");
        // 家目录下第 3 层（深度臂放行的最浅形状），且不在任何商店权重根之下。
        let weights = user_home_dir()
            .join("wiring-e2e")
            .join("deep")
            .join("models");
        std::fs::create_dir_all(&weights).expect("create weights dir");
        std::fs::write(weights.join("weight.bin"), b"weights").expect("write weight");
        let dir = registry_dir();
        std::fs::create_dir_all(&dir).expect("registry dir");
        std::fs::write(
            dir.join("m-wiring01.json"),
            format!(
                r#"{{"schema":1,"id":"m-wiring01","name":"wiring-e2e","kind":"other","weights_path":{}}}"#,
                serde_json::to_string(&weights.to_string_lossy()).expect("json string")
            ),
        )
        .expect("write registry record");
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async move {
            let outcome = remove_external_model(
                "m-wiring01".to_string(),
                true,
                "wiring-e2e".to_string(),
                None,
            )
            .await
            .expect("removal must succeed");
            assert!(
                outcome.removed_weights,
                "guard-approved weights must be deleted"
            );
            assert!(outcome.removed_registry, "registry file should be removed");
            assert!(
                !weights.exists(),
                "weight dir must be gone after delete_weights=true"
            );
            let view = get_external_models().await.expect("list external models");
            assert!(
                view.models.iter().all(|model| model.id != "m-wiring01"),
                "registry record must disappear from the listing"
            );
        });
    }

    /// 2026-11-04 评审 P2 的**完整状态转换**：扫描目录里的权重被卡片就地复用
    /// （宿主无从枚举引用，保护取保守超集）→ 用户从存储设置移除该扫描目录 →
    /// 设备管理对指向其中权重的外部记录请求「删除权重」。不留痕的旧形态在
    /// 最后一跳整单放行（评审者以隔离探针实证）；留痕后目录经
    /// `import_scan_dirs_history` 仍属商店权重根，删除被拒、目录与记录原样
    /// 保留。设置层留痕/根集合联动由 model_store 侧单测直打，这里钉设备侧
    /// 全链路。
    #[test]
    fn removing_scan_dir_shields_reused_weights_from_device_deletion() {
        use crate::features::device::{
            external_models::registry_dir, register_external_model, remove_external_model,
        };
        use crate::features::model_store::platform::linux::{config, db};
        use crate::platform::paths::user_home_dir;
        let ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        db::init_db().expect("init store db");
        // 权重落在家目录三层（深度臂本会放行的形状），先以普通外部模型登记
        // ——此刻目录还不是扫描根，登记合法。
        let scan_root = ctx.path().join("imported");
        let weights = user_home_dir().join("imported").join("hf").join("qwen");
        std::fs::create_dir_all(&weights).expect("create weights dir");
        std::fs::write(weights.join("weight.bin"), b"weights").expect("write weight");
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async {
            let registered = register_external_model(
                "reused-model".to_string(),
                "other".to_string(),
                None,
                None,
                Some(weights.to_string_lossy().into_owned()),
            )
            .await
            .expect("register before the dir becomes a scan root");

            // 挂为扫描目录（卡片会就地复用其中的既有权重），随后从设置移除
            // ——正是评审点名的状态转换。留痕后它仍在保护集合里。
            crate::features::model_store::set_storage_settings(
                None,
                Some(vec![scan_root.to_string_lossy().into_owned()]),
            )
            .expect("add scan dir");
            crate::features::model_store::set_storage_settings(None, Some(Vec::new()))
                .expect("remove scan dir");

            let error = remove_external_model(
                registered.id.clone(),
                true,
                "reused-model".to_string(),
                None,
            )
            .await
            .expect_err("weights in a removed scan dir must stay protected");
            assert!(
                error.contains("模型商店"),
                "rejection must say the weights are store-managed, got: {error}"
            );
            assert!(weights.exists(), "weight dir must survive the rejection");
            let dir = registry_dir();
            assert!(
                dir.join(format!("{}.json", registered.id)).exists(),
                "registry record must survive the rejection"
            );
        });
    }

    /// 2026-11-04 评审 P2（5765748232）场景 a——**升级空历史**：修复上线前
    /// 已被从扫描列表移除的目录在存量配置里不进 import_scan_dirs_history，
    /// 目录级超集覆盖不到；已安装卡片的就地复用经约定软链（models_dir/<HF
    /// 仓库名> → 原位）恢复实际引用路径，删除守卫据此拦截。
    #[test]
    fn upgrade_with_empty_history_still_shields_reused_weights() {
        use crate::features::device::{external_models::registry_dir, remove_external_model};
        use crate::features::model_store::platform::linux::{config, db};
        use crate::platform::paths::user_home_dir;
        let _ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        db::init_db().expect("init store db");
        // 原位权重：家目录三层（深度臂本会放行），不在任何现役根之下。
        let weights = user_home_dir()
            .join("legacy-store")
            .join("hf")
            .join("qwen-weights");
        std::fs::create_dir_all(&weights).expect("create original weights");
        std::fs::write(weights.join("weight.bin"), b"weights").expect("write weight");
        // 约定软链：models_dir/<HF 仓库名> → 原位（就地复用不搬家的形态）。
        let models_dir = config::try_load_settings()
            .expect("load default settings")
            .models_path();
        #[cfg(unix)]
        std::os::unix::fs::symlink(&weights, models_dir.join("qwen-weights"))
            .expect("create reuse symlink");
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-upg",
            model_name: "upg-model",
            card_name: "c-upg",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("org/qwen-weights"),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert downloaded model");
        let dir = registry_dir();
        std::fs::create_dir_all(&dir).expect("registry dir");
        // 注册名刻意与在册 model_name（"upg-model"）不同：同名会让守卫 2 的
        // 商店名单名字臂先命中（"由模型商店管理"），本用例要钉的是其后的
        // 存量引用臂（f3e00ff90 初版同名，该提交起 CI 未跑过 Linux 测试）。
        std::fs::write(
            dir.join("m-upg01.json"),
            format!(
                r#"{{"schema":1,"id":"m-upg01","name":"upg-ext","kind":"other","weights_path":{}}}"#,
                serde_json::to_string(&weights.to_string_lossy()).expect("json string")
            ),
        )
        .expect("write registry record");
        // 不触碰任何设置 → import_scan_dirs_history 保持空（存量升级形态）。
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async move {
            let error =
                remove_external_model("m-upg01".to_string(), true, "upg-ext".to_string(), None)
                    .await
                    .expect_err("empty-history upgrade must still shield reused weights");
            assert!(
                error.contains("已安装商店模型引用") && error.contains("删除会破坏仍在使用"),
                "rejection must name the installed-reference arm, got: {error}"
            );
            assert!(weights.exists(), "referenced weights must survive");
            assert!(
                dir.join("m-upg01.json").exists(),
                "registry record must survive the rejection"
            );
        });
    }

    /// 2026-11-04 评审 P2（5765748232）场景 b——**历史到顶仍存在引用**：扫描
    /// 目录历史被 17 次交换挤出最老端后，目录级保护对该根脱出；在册卡片经
    /// 约定软链恢复的引用路径继续拦住删除。
    #[test]
    fn evicted_history_root_keeps_referenced_weights_shielded() {
        use crate::features::device::{external_models::registry_dir, remove_external_model};
        use crate::features::model_store::platform::linux::{config, db};
        use crate::platform::paths::user_home_dir;
        let ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        db::init_db().expect("init store db");
        // 被淘汰的最老扫描根下的原位权重（家目录四层）。
        let scan_root = ctx.path().join("scan-000");
        let weights = user_home_dir()
            .join("scan-000")
            .join("legacy")
            .join("hf")
            .join("qwen-weights");
        std::fs::create_dir_all(&weights).expect("create original weights");
        std::fs::write(weights.join("weight.bin"), b"weights").expect("write weight");
        let models_dir = config::try_load_settings()
            .expect("load default settings")
            .models_path();
        #[cfg(unix)]
        std::os::unix::fs::symlink(&weights, models_dir.join("qwen-weights"))
            .expect("create reuse symlink");
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-evict",
            model_name: "evict-model",
            card_name: "c-evict",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("org/qwen-weights"),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert downloaded model");
        let dir = registry_dir();
        std::fs::create_dir_all(&dir).expect("registry dir");
        // 注册名与在册 model_name（"evict-model"）刻意不同：同名字臂先命中，
        // 钉不住本用例目标的存量引用臂（与 upgrade 用例同一更正）。
        std::fs::write(
            dir.join("m-evict01.json"),
            format!(
                r#"{{"schema":1,"id":"m-evict01","name":"evict-ext","kind":"other","weights_path":{}}}"#,
                serde_json::to_string(&weights.to_string_lossy()).expect("json string")
            ),
        )
        .expect("write registry record");
        // 把扫描目录历史顶到上限并挤出 scan-000（与 service 层淘汰测试同一
        // 交换序列，MAX_IMPORT_SCAN_DIRS_HISTORY + 1 次）。
        for index in 0..=(config::MAX_IMPORT_SCAN_DIRS_HISTORY + 1) {
            let swap = ctx.path().join(format!("scan-{index:03}"));
            std::fs::create_dir_all(&swap).expect("create swap dir");
            crate::features::model_store::set_storage_settings(
                None,
                Some(vec![swap.to_string_lossy().into_owned()]),
            )
            .expect("swap scan dir");
        }
        let settings = config::load_settings();
        assert!(
            !settings
                .import_scan_dirs_history
                .contains(&scan_root.to_string_lossy().into_owned()),
            "precondition: the oldest root must have been evicted, got {:?}",
            settings.import_scan_dirs_history
        );
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async move {
            let error =
                remove_external_model("m-evict01".to_string(), true, "evict-ext".to_string(), None)
                    .await
                    .expect_err("evicted root must not lift installed-reference protection");
            assert!(
                error.contains("已安装商店模型引用"),
                "rejection must come from the installed-reference arm, got: {error}"
            );
            assert!(weights.exists(), "referenced weights must survive");
        });
    }

    /// 2026-11-04 评审 P2（5768052838）zhuowp 点名的存量场景——**升级前自定义
    /// 权重根零痕迹**：旧版本用自定义权重根 X 安装卡片，之后仍在旧版本的保存中
    /// 换根到 Y 并移除扫描目录——旧版本无任何留痕机制，升级后 settings 的两类
    /// history 全空、models_dir=Y，X 不在任何历史/内置根中。路径臂（已知根 ×
    /// 约定名）恢复不了 X 上的引用，由名字臂兜底：删除 X 本身（一级子目录命中）
    /// 与删除 X/<Repo>（叶名命中）都被拒绝、卡片与注册记录存活；不相关名目录
    /// 照常放行（负验证，钉「宁可漏」边界不误伤）。
    #[test]
    fn untraced_legacy_custom_root_is_shielded_by_candidate_names() {
        use crate::features::device::{external_models::registry_dir, remove_external_model};
        use crate::features::model_store::platform::linux::{config, db};
        use crate::platform::paths::user_home_dir;
        let ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        db::init_db().expect("init store db");
        // 手写存量升级形态的 settings：models_dir=Y + ollama_host 合并写（覆盖
        // point_ollama_at_dead_port 的单键文件），两条 history 均不存在。
        let models_dir_y = ctx.path().join("weights-y");
        std::fs::create_dir_all(&models_dir_y).expect("create models_dir Y");
        std::fs::write(
            config::config_dir().join("config.json"),
            format!(
                r#"{{"ollama_host": "http://127.0.0.1:1", "models_dir": "{}"}}"#,
                models_dir_y.to_string_lossy()
            ),
        )
        .expect("write pre-upgrade settings");
        let settings = config::load_settings();
        assert!(
            settings.models_dir_history.is_empty() && settings.import_scan_dirs_history.is_empty(),
            "precondition: pre-upgrade settings carry no history, got {:?} / {:?}",
            settings.models_dir_history,
            settings.import_scan_dirs_history
        );
        // 零痕迹旧根 X 及其上的卡片权重（约定形状恰一层：<X>/<Repo>）。
        let legacy_root = user_home_dir().join("legacy-custom");
        let weights = legacy_root.join("Repo");
        std::fs::create_dir_all(&weights).expect("create legacy weights");
        std::fs::write(weights.join("weight.bin"), b"weights").expect("write weight");
        // 在册卡片：约定名两臂 = "Repo"（source_repo_id 末段）与 "zero-model"。
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-zero",
            model_name: "zero-model",
            card_name: "c-zero",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("org/Repo"),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert downloaded model");
        // 注册记录名与在册 model_name 刻意不同：同名会让守卫 2 名字臂先命中，
        // 钉不住本用例的名字臂（与 upgrade/evict 用例同一更正）。
        let dir = registry_dir();
        std::fs::create_dir_all(&dir).expect("registry dir");
        let write_record = |id: &str, name: &str, path: &std::path::Path| {
            std::fs::write(
                dir.join(format!("{id}.json")),
                format!(
                    r#"{{"schema":1,"id":"{id}","name":"{name}","kind":"other","weights_path":{}}}"#,
                    serde_json::to_string(&path.to_string_lossy()).expect("json string")
                ),
            )
            .expect("write registry record");
        };
        write_record("m-zero1", "legacy-ext-leaf", &weights);
        write_record("m-zero2", "legacy-ext-root", &legacy_root);
        // 负验证对照：不相关名的家目录三层权重目录（深度臂放行的形状），登记
        // 名/叶名/一级子名都不命中约定名集合。
        let unrelated = user_home_dir()
            .join("unrelated-place")
            .join("deep")
            .join("store-free");
        std::fs::create_dir_all(&unrelated).expect("create unrelated weights");
        std::fs::write(unrelated.join("weight.bin"), b"weights").expect("write unrelated weight");
        write_record("m-free01", "legacy-ext-free", &unrelated);
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async move {
            // 叶删：删除 <X>/<Repo>（记录 m-zero1）→ 名字臂叶名命中拒绝。
            let leaf_error = remove_external_model(
                "m-zero1".to_string(),
                true,
                "legacy-ext-leaf".to_string(),
                None,
            )
            .await
            .expect_err("untraced legacy root weights must stay shielded");
            assert!(
                leaf_error.contains("约定同名") && leaf_error.contains("不留痕"),
                "leaf deletion must be refused by the legacy-root name arm, got: {leaf_error}"
            );
            // 父删：删除 <X> 本身（记录 m-zero2）→ 一级子目录名命中拒绝。
            let root_error = remove_external_model(
                "m-zero2".to_string(),
                true,
                "legacy-ext-root".to_string(),
                None,
            )
            .await
            .expect_err("the untraced root itself must stay shielded too");
            assert!(
                root_error.contains("约定同名"),
                "parent deletion must be refused by the legacy-root name arm, got: {root_error}"
            );
            // 卡片、权重目录与注册记录全部存活。
            assert!(
                db::get_store_model("c-zero")
                    .expect("query store model")
                    .is_some(),
                "the in-store card must survive the rejections"
            );
            assert!(weights.exists(), "referenced weights must survive");
            assert!(legacy_root.exists(), "the legacy root must survive");
            assert!(dir.join("m-zero1.json").exists());
            assert!(dir.join("m-zero2.json").exists());
            // 负验证：不相关名目录照常放行（整单删除成功），钉住新臂不误伤。
            let outcome = remove_external_model(
                "m-free01".to_string(),
                true,
                "legacy-ext-free".to_string(),
                None,
            )
            .await
            .expect("unrelated-name weights must delete normally");
            assert!(
                outcome.removed_weights,
                "unrelated-name weights must be deleted"
            );
            assert!(outcome.removed_registry);
            assert!(
                !unrelated.exists(),
                "unrelated-name weights must be gone after delete_weights=true"
            );
        });
    }

    /// 2026-11-04 评审 P3 的端到端：确认面板带来的守护进程地址与当前设置解析
    /// 值不一致时（列表之后、确认之前改过 `ollama_host`），整单拒绝且记录
    /// 原样保留——DELETE 绝不改投新实例；用**当前**地址重新确认则正常走完
    /// （daemon 不可达按既有口径收进警告，注册表清理不阻断）。
    #[test]
    fn stale_daemon_confirmation_is_refused_not_redirected() {
        use crate::features::device::{
            external_models::registry_dir, get_external_models, remove_external_model,
        };
        use crate::features::model_store::platform::linux::{config, db};
        let _ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        db::init_db().expect("init store db");
        let dir = registry_dir();
        std::fs::create_dir_all(&dir).expect("registry dir");
        std::fs::write(
            dir.join("m-stale01.json"),
            r#"{"schema":1,"id":"m-stale01","name":"stale-model","kind":"ollama","model":"stale-model:latest"}"#,
        )
        .expect("write ollama record");
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async move {
            // 列表时刻解析出的地址（改配前的确认面板所见）：出厂默认。
            let view = get_external_models().await.expect("list external models");
            let row = view
                .models
                .iter()
                .find(|model| model.id == "m-stale01")
                .expect("listed ollama row");
            let listed_base = row.base_url.clone().expect("ollama rows carry base_url");

            // 中间改配（设置里的 ollama_host 换实例：dead port → 另一死端口）。
            std::fs::write(
                config::config_dir().join("config.json"),
                r#"{"ollama_host": "http://127.0.0.1:2"}"#,
            )
            .expect("switch daemon config");

            // 陈旧确认：按改配前的地址请求移除 → 整单拒绝。
            let error = remove_external_model(
                "m-stale01".to_string(),
                true,
                "stale-model".to_string(),
                Some(listed_base),
            )
            .await
            .expect_err("stale confirmation must be refused");
            assert!(
                error.contains("已变化") && error.contains("刷新"),
                "rejection must demand a refresh, got: {error}"
            );
            assert!(
                dir.join("m-stale01.json").exists(),
                "registry record must survive the refusal"
            );

            // 用当前地址重新确认 → 正常走完；DELETE 发往当前实例（死端口，
            // 失败按警告收尾），注册表清理完成。
            let outcome = remove_external_model(
                "m-stale01".to_string(),
                true,
                "stale-model".to_string(),
                Some("http://127.0.0.1:2/v1".to_string()),
            )
            .await
            .expect("re-confirmation with the current base must succeed");
            assert!(!outcome.ollama_deleted, "dead daemon cannot be deleted");
            assert!(
                outcome
                    .warnings
                    .iter()
                    .any(|warning| warning.contains("Ollama 删除失败")),
                "daemon failure must be disclosed, got: {:?}",
                outcome.warnings
            );
            assert!(outcome.removed_registry, "registry file should be removed");
        });
    }

    /// 商店权重根**内置导入扫描根臂**的端到端接线（十二轮评审 P1）：
    /// `LMM_IMPORT_DIRS` 给脚本下发的是含 `~/Downloads` 等内置根的同一张列表，
    /// 卡片会就地复用其中的既有权重——这些目录下的权重同属"由模型商店管理"，
    /// 登记端必须拒绝、移除端必须拒删。此前 boundary 只收 settings 里的
    /// import_scan_dirs，内置根裸奔：别名记录指向内置根下三层目录即可真删
    /// 商店复用的权重。
    #[test]
    fn builtin_scan_root_weights_are_store_managed_end_to_end() {
        use crate::features::device::{
            external_models::registry_dir, register_external_model, remove_external_model,
        };
        use crate::features::model_store::platform::linux::{config, db};
        use crate::platform::paths::user_home_dir;
        let _ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        db::init_db().expect("init store db");
        // ~/Downloads/<a>/<b>：家目录三层（深度臂本会放行），落在内置扫描根
        // ~/Downloads 之下（本测试钉的新臂）。
        let weights = user_home_dir().join("Downloads").join("hf").join("qwen");
        std::fs::create_dir_all(&weights).expect("create weights dir");
        std::fs::write(weights.join("weight.bin"), b"weights").expect("write weight");

        // 登记端：权重在内置扫描根之下 → 必须按商店管理拒绝。
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async {
            let error = register_external_model(
                "dl-model".to_string(),
                "other".to_string(),
                Some("http://127.0.0.1:1".to_string()),
                None,
                Some(weights.to_string_lossy().into_owned()),
            )
            .await
            .expect_err("weights under a builtin scan root must be rejected");
            assert!(
                error.contains("模型商店"),
                "rejection must say the weights are store-managed, got: {error}"
            );
        });

        // 移除端：手写存量记录（登记端修复前/AI 手写可能存在）同样必须拒删，
        // 且目录与记录都原样保留。
        let dir = registry_dir();
        std::fs::create_dir_all(&dir).expect("registry dir");
        std::fs::write(
            dir.join("m-dlroot01.json"),
            format!(
                r#"{{"schema":1,"id":"m-dlroot01","name":"dl-model","kind":"other","weights_path":{}}}"#,
                serde_json::to_string(&weights.to_string_lossy()).expect("json string")
            ),
        )
        .expect("write legacy record");
        runtime.block_on(async move {
            let error =
                remove_external_model("m-dlroot01".to_string(), true, "dl-model".to_string(), None)
                    .await
                    .expect_err("removal must be rejected for builtin-scan-root weights");
            assert!(
                error.contains("模型商店"),
                "rejection must say the weights are store-managed, got: {error}"
            );
            assert!(weights.exists(), "weight dir must survive the rejection");
            assert!(
                dir.join("m-dlroot01.json").exists(),
                "registry record must survive the rejection"
            );
        });
    }

    /// operstate 判定表：unknown（回环/VPN 口）必须算 up，down 家族与 dormant
    /// 判 down。此前这段 matches! 长在 /sys 读取里零覆盖——把它改成 "down" 会
    /// 让 lo/tun 口持续假"断开"而测试全绿（五轮评审 P3）。
    #[test]
    fn operstate_text_unknown_is_up_and_down_family_is_down() {
        assert!(operstate_text_is_up("unknown"));
        assert!(operstate_text_is_up("up"));
        assert!(operstate_text_is_up("up\n"));
        assert!(!operstate_text_is_up("down"));
        assert!(!operstate_text_is_up("lowerlayerdown"));
        assert!(!operstate_text_is_up("notpresent"));
        assert!(!operstate_text_is_up("dormant"));
        assert!(!operstate_text_is_up(" down "));
    }

    /// 登记端 daemon 模型目录路径臂的**端到端接线**（第八轮评审 P1）：任何 kind
    /// 的 weights_path 落在 `~/.ollama/models` 之内都必须拒绝——`kind:"other"`
    /// 不受 ollama 硬校验覆盖，而 blobs 恰好家目录三层能过移除端的深度臂，堵不住
    /// 登记就是"登记成功 → 移除连根删光全部 blob"。守卫矩阵/纯函数臂由
    /// external_models 的 `weights_deletion_guard_rejects_the_ollama_models_tree`
    /// 直打，这里钉的是命令全链。
    #[test]
    fn register_rejects_weights_path_under_ollama_models_root() {
        use crate::features::device::register_external_model;
        use crate::features::model_store::platform::linux::config;
        let _ctx = config::TempHome::new();
        let blobs = crate::platform::paths::user_home_dir()
            .join(".ollama")
            .join("models")
            .join("blobs");
        std::fs::create_dir_all(&blobs).expect("create blobs dir");
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async move {
            let error = register_external_model(
                "blob-killer".to_string(),
                "other".to_string(),
                None,
                None,
                Some(blobs.to_string_lossy().to_string()),
            )
            .await
            .expect_err("weights under the ollama root must be rejected");
            assert!(
                error.contains("Ollama"),
                "rejection must name the daemon directory, got: {error}"
            );
        });
    }

    /// 视图装配对 ollama 行统一回填设置推导的 daemon 基地址 + `/v1`（第九轮
    /// 评审 P2）：前端 linkTargetOf 曾硬编码 11434、注册行回显自带 base_url，
    /// 而后端探测/删除全部走 `ollama_base()`——展示、链接与删除必须对准同一
    /// 实例（注册行自带的 11434 地址被覆盖）。daemon 不可达分支同样要覆盖
    /// （死端口配置即触发 warnings 路径）。
    #[test]
    fn external_models_view_pins_ollama_base_url_to_settings_daemon() {
        use crate::features::device::external_models::registry_dir;
        use crate::features::device::get_external_models;
        use crate::features::model_store::platform::linux::config;
        let _ctx = config::TempHome::new();
        point_ollama_at_dead_port();
        let dir = registry_dir();
        std::fs::create_dir_all(&dir).expect("registry dir");
        // 存量记录自带 base_url（早期模板/前端写的 11434），视图装配必须覆盖。
        std::fs::write(
            dir.join("m-legacy01.json"),
            r#"{"schema":1,"id":"m-legacy01","name":"legacy-ollama","kind":"ollama","base_url":"http://127.0.0.1:11434/v1","model":"legacy:latest"}"#,
        )
        .expect("write legacy ollama record");
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        runtime.block_on(async move {
            let view = get_external_models().await.expect("list external models");
            let ollama_rows: Vec<_> = view
                .models
                .iter()
                .filter(|model| model.kind == "ollama")
                .collect();
            assert!(
                !ollama_rows.is_empty(),
                "registry ollama row must be listed"
            );
            for model in ollama_rows {
                assert_eq!(
                    model.base_url.as_deref(),
                    Some("http://127.0.0.1:1/v1"),
                    "ollama rows must carry the settings-derived daemon base + /v1"
                );
            }
        });
    }
}
