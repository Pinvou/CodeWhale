//! MegaCube 设备管理的后端采样（设计 §2）。
//!
//! 与 `features/monitor` 互补：monitor 服务所有平台的"运行状态"卡片，本模块只为
//! MegaCube 一体机提供更细的设备视图——传感器（温度/风扇/电压）、磁盘、网络速率、
//! 进程占用、GPU 扩展信息（驱动/CUDA/计算进程）。
//!
//! 同一采样原则：**任何子项失败都 graceful degrade**（None / 空集合 / 默认值），
//! 不让单点读取失败拖垮整个快照。
//!
//! 状态所有权：
//! - 进程 CPU% 必须在**同一个** `sysinfo::System` 实例的两次刷新之间求差 → 持久
//!   实例放 `DeviceState`（lib.rs manage），不能每次新建。
//! - 网络速率同理：`sysinfo::Networks` 的 `received()/transmitted()` 是相对上一次
//!   刷新的增量 → 实例持久 + `last_network_poll` 记录刷新时刻，速率 = 增量 / 间隔。
//! - 温度组件（`Components`）与磁盘（`Disks`）无跨采样状态，每次新建。
//!
//! 平台差异（DMI / os-release / hwmon 风扇电压 / operstate / NVIDIA 内核驱动版本）
//! 全部收在 `platform/`，本文件**零 cfg**；外部模型注册表见 `external_models.rs`，
//! 故障排查提示词见 `doctor.rs`。

mod doctor;
mod external_models;
mod platform;

pub use doctor::{DoctorKind, build_doctor_prompt};
pub use external_models::{
    ExternalModel, ExternalModelsView, RemoveOutcome, get_external_models, register_external_model,
    remove_external_model,
};

use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

use parking_lot::Mutex;
use serde::Serialize;
use sysinfo::{Components, Disks, Networks, ProcessRefreshKind, ProcessesToUpdate, System};

// GPU 探测与 monitor 同一套预算与候选：nvidia-smi 在驱动/CUDA 争用下可能挂死。
use crate::features::monitor::GPU_PROBE_TIMEOUT;

/// 进程名展示上限（设计 §2.2：name 截断 40 字符）。
const PROCESS_NAME_MAX_CHARS: usize = 40;
/// top CPU / top 内存各取前 8。
const TOP_N: usize = 8;
/// GPU 扩展快照缓存窗口：2s 前端轮询不必每次都拉起 nvidia-smi（与 monitor 同款）。
const GPU_EXT_CACHE_TTL: Duration = Duration::from_secs(3);

// ---------------------------------------------------------------- 数据契约

/// 设备管理页完整快照（前端 2s 轮询）。字段 snake_case 直出。
#[derive(Debug, Clone, Serialize)]
pub struct DeviceSnapshot {
    pub generated_at_ms: u64,
    pub os: OsInfo,
    /// 空 → 前端显示"未读到传感器"空态。
    pub sensors: Vec<SensorGroup>,
    pub disks: Vec<DiskSnapshot>,
    /// 含本次与上次采样间的速率（首次采样速率为 0）。
    pub network: Vec<NetIfaceSnapshot>,
    pub processes: ProcessSummary,
    /// 无 NVIDIA 时 None → 前端隐藏 GPU 扩展区块。
    pub gpu_ext: Option<GpuExtSnapshot>,
}

#[derive(Debug, Clone, Serialize)]
pub struct OsInfo {
    pub product_name: Option<String>,
    pub pretty_name: Option<String>,
    pub kernel: String,
    pub arch: String,
}

/// 按 chip（hwmon `name` / 温度组件标签首段）分组的传感器。
#[derive(Debug, Clone, Serialize)]
pub struct SensorGroup {
    pub chip: String,
    pub entries: Vec<SensorEntry>,
}

#[derive(Debug, Clone, Serialize)]
pub struct SensorEntry {
    /// temperature | fan | voltage | current | power
    pub kind: String,
    pub name: String,
    /// 归一化：°C / RPM / V / A / W
    pub value: f64,
    pub critical: Option<f64>,
    pub max: Option<f64>,
}

#[derive(Debug, Clone, Serialize)]
pub struct DiskSnapshot {
    pub name: Option<String>,
    pub mount_point: String,
    pub fs_type: String,
    /// ssd | hdd | unknown
    pub kind: String,
    pub total_bytes: u64,
    pub available_bytes: u64,
    pub is_removable: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct NetIfaceSnapshot {
    pub name: String,
    /// ethernet | wifi | loopback | bridge | virtual | other
    pub iface_type: String,
    pub is_up: bool,
    pub mac: Option<String>,
    pub rx_bytes: u64,
    pub tx_bytes: u64,
    /// 速率单位是**字节/秒**（sysinfo received() 为字节增量 / 秒）。字段名不用
    /// 惯例缩写 "bps"（bits/s），避免后来者按 8 倍误读。
    pub rx_bytes_per_sec: f64,
    pub tx_bytes_per_sec: f64,
}

#[derive(Debug, Clone, Serialize)]
pub struct ProcessSummary {
    pub top_cpu: Vec<ProcInfo>,
    pub top_memory: Vec<ProcInfo>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ProcInfo {
    pub pid: u32,
    pub name: String,
    pub cpu_pct: f32,
    pub memory_bytes: u64,
}

#[derive(Debug, Clone, Serialize)]
pub struct GpuExtSnapshot {
    pub driver_version: Option<String>,
    pub cuda_version: Option<String>,
    pub compute_processes: Vec<GpuProcInfo>,
}

#[derive(Debug, Clone, Serialize)]
pub struct GpuProcInfo {
    pub pid: u32,
    pub name: String,
    pub used_mib: u64,
}

/// 模型服务探测结果（独立命令，慢轮询；不进 2s 快照）。
#[derive(Debug, Clone, Serialize)]
pub struct ServiceStatus {
    /// ollama | vllm | lmstudio | docker
    pub key: String,
    pub running: bool,
    /// 人话补充：running=false 时是失败原因（超时/拒绝连接/命令不存在）；
    /// docker running 时是版本号。
    pub detail: Option<String>,
}

// ---------------------------------------------------------------- DeviceState

/// 设备采样状态：只持有**必须跨采样存续**的 sysinfo 实例与上次网络刷新时刻。
/// 内层装 `Arc` 是为了把整段全量采样放进 blocking 池（spawn_blocking 要求
/// 'static 闭包），快照命令拿 `&DeviceState` 即可克隆指针入池。
#[derive(Debug, Clone)]
pub struct DeviceState(Arc<DeviceStateInner>);

#[derive(Debug)]
struct DeviceStateInner {
    system: Mutex<System>,
    networks: Mutex<Networks>,
    last_network_poll: Mutex<Option<Instant>>,
}

impl DeviceState {
    pub fn new() -> Self {
        // Linux 上 sysinfo 默认会缓存接近一半 FD 上限的 /proc stat 句柄；ACP
        // ledger（platform/process_resources.rs）以全局 `set_open_files_limit(0)`
        // 换取有界描述符占用。这是进程级开关，这里显式表达同一立场：本模块的
        // CPU% 只依赖两次刷新的计数值差，不需要 FD 缓存；谁先初始化谁生效，
        // 与其让"设备页先采样"悄悄把全局策略定为缓存，不如两边一致关掉。
        let _ = sysinfo::set_open_files_limit(0);
        Self(Arc::new(DeviceStateInner {
            system: Mutex::new(System::new()),
            networks: Mutex::new(Networks::new()),
            last_network_poll: Mutex::new(None),
        }))
    }
}

impl Default for DeviceState {
    fn default() -> Self {
        Self::new()
    }
}

// ---------------------------------------------------------------- 采样

/// 完整设备快照。全量采样（refresh_processes / Disks / Components）与 GPU 扩展
/// （nvidia-smi 子进程）都放 blocking 池并并发执行：进程表在慢盘/大进程数时可能
/// 超过毫秒级，2s 轮询不能周期性占住 async worker（与 ACP ledger 的
/// spawn_blocking 先例同一取舍）。
pub async fn sample_snapshot(state: &DeviceState) -> DeviceSnapshot {
    let base_task = {
        let inner = Arc::clone(&state.0);
        tokio::task::spawn_blocking(move || sample_without_gpu(&inner))
    };
    let gpu_task = tokio::task::spawn_blocking(gpu_ext_cached);
    let (base, gpu_ext) = tokio::join!(base_task, gpu_task);
    // 采样闭包 panic（只可能是 bug）时恢复同步版的 panic 语义，不静默降级成空快照；
    // GPU 侧 JoinError 照旧降级 None（沿用既有口径）。
    let mut snapshot = match base {
        Ok(snapshot) => snapshot,
        Err(join_error) => std::panic::resume_unwind(join_error.into_panic()),
    };
    snapshot.gpu_ext = gpu_ext.unwrap_or(None);
    snapshot
}

fn sample_without_gpu(state: &DeviceStateInner) -> DeviceSnapshot {
    let os = OsInfo {
        product_name: platform::dmi_product_name(),
        pretty_name: platform::os_pretty_name(),
        kernel: System::kernel_version().unwrap_or_default(),
        arch: std::env::consts::ARCH.to_string(),
    };
    let sensors = collect_sensors();
    let disks = collect_disks();
    let network = collect_network(state);
    let processes = collect_processes(state);
    DeviceSnapshot {
        generated_at_ms: now_ms(),
        os,
        sensors,
        disks,
        network,
        processes,
        gpu_ext: None,
    }
}

/// 温度走 sysinfo Components（Linux 上即 hwmon 温度通道），风扇/电压/电流/功率
/// 走 platform::extra_sensors()，按 chip 合并分组。
fn collect_sensors() -> Vec<SensorGroup> {
    let mut groups: Vec<SensorGroup> = Vec::new();
    let components = Components::new_with_refreshed_list();
    for component in components.list() {
        // 读失败（None）整条跳过，不伪造 0°C——对分不清真假的用户，编造的 0°C
        // 比缺数据更糟；与 extra_sensors 读失败即跳过的口径一致。
        let Some(celsius) = component.temperature() else {
            continue;
        };
        let (chip, name) = split_component_label(component.label());
        push_sensor(
            &mut groups,
            chip,
            "temperature",
            name,
            f64::from(celsius),
            component.critical().map(f64::from),
            component.max().map(f64::from),
        );
    }
    for (chip, kind, name, value, critical, max) in platform::extra_sensors() {
        push_sensor(&mut groups, chip, &kind, name, value, critical, max);
    }
    groups
}

fn push_sensor(
    groups: &mut Vec<SensorGroup>,
    chip: String,
    kind: &str,
    name: String,
    value: f64,
    critical: Option<f64>,
    max: Option<f64>,
) {
    let entry = SensorEntry {
        kind: kind.to_string(),
        name,
        value,
        critical,
        max,
    };
    if let Some(group) = groups.iter_mut().find(|group| group.chip == chip) {
        group.entries.push(entry);
        return;
    }
    groups.push(SensorGroup {
        chip,
        entries: vec![entry],
    });
}

/// sysinfo 组件标签形如 `"{name} {label} {device_model}"`（Linux 即 hwmon 的
/// `name` + 可选 label）。首段当 chip、其余当条目名，正好与 hwmon 风扇/电压
/// 分组的 chip 命名对齐；只有一段时 chip 与 name 同值。
fn split_component_label(label: &str) -> (String, String) {
    let label = label.trim();
    match label.split_once(' ') {
        Some((chip, rest)) if !chip.is_empty() && !rest.trim().is_empty() => {
            (chip.to_string(), rest.trim().to_string())
        }
        _ => (label.to_string(), label.to_string()),
    }
}

fn collect_disks() -> Vec<DiskSnapshot> {
    let disks = Disks::new_with_refreshed_list();
    let mut list: Vec<DiskSnapshot> = disks
        .list()
        .iter()
        .map(disk_snapshot)
        .filter(keep_disk)
        .collect();
    sort_disks(&mut list);
    list
}

fn disk_snapshot(disk: &sysinfo::Disk) -> DiskSnapshot {
    let name = disk.name().to_string_lossy().trim().to_string();
    DiskSnapshot {
        name: if name.is_empty() { None } else { Some(name) },
        mount_point: disk.mount_point().to_string_lossy().to_string(),
        fs_type: disk.file_system().to_string_lossy().to_string(),
        kind: match disk.kind() {
            sysinfo::DiskKind::SSD => "ssd",
            sysinfo::DiskKind::HDD => "hdd",
            sysinfo::DiskKind::Unknown(_) => "unknown",
        }
        .to_string(),
        total_bytes: disk.total_space(),
        available_bytes: disk.available_space(),
        is_removable: disk.is_removable(),
    }
}

/// 过滤噪音挂载：squashfs（snap 的只读 squash 包）与 snap 挂载点；`/boot/efi`
/// 保留（用户关心 EFI 分区占用），只是排序垫底。
fn keep_disk(disk: &DiskSnapshot) -> bool {
    if disk.fs_type.eq_ignore_ascii_case("squashfs") {
        return false;
    }
    if disk.mount_point.replace('\\', "/").starts_with("/snap/") {
        return false;
    }
    true
}

/// 排序：根挂载最前 → 其余按总量降序 → `/boot/efi` 垫底。
fn sort_disks(list: &mut [DiskSnapshot]) {
    list.sort_by(|a, b| {
        disk_sort_key(&a.mount_point)
            .cmp(&disk_sort_key(&b.mount_point))
            .then_with(|| b.total_bytes.cmp(&a.total_bytes))
    });
}

fn disk_sort_key(mount_point: &str) -> u8 {
    match mount_point {
        "/" => 0,
        "/boot/efi" => 2,
        _ => 1,
    }
}

/// 网络接口：持久 `Networks` 实例刷新一次，`received()/transmitted()` 为相对
/// 上次刷新的增量，除以真实间隔秒数得速率；首次采样（无上次时刻）速率记 0。
fn collect_network(state: &DeviceStateInner) -> Vec<NetIfaceSnapshot> {
    let mut networks = state.networks.lock();
    let mut last_poll = state.last_network_poll.lock();
    let now = Instant::now();
    let elapsed_secs = last_poll.map(|at| now.duration_since(at).as_secs_f64());
    networks.refresh(true);
    *last_poll = Some(now);

    let mut list: Vec<NetIfaceSnapshot> = networks
        .list()
        .iter()
        .map(|(name, data)| {
            let (rx_bytes_per_sec, tx_bytes_per_sec) = match elapsed_secs {
                Some(secs) if secs > 0.0 => (
                    data.received() as f64 / secs,
                    data.transmitted() as f64 / secs,
                ),
                _ => (0.0, 0.0),
            };
            let mac = data.mac_address().to_string();
            NetIfaceSnapshot {
                name: name.clone(),
                iface_type: classify_iface(name, platform::iface_has_wireless(name)).to_string(),
                is_up: platform::iface_operstate_up(name).unwrap_or(true),
                mac: if mac == "00:00:00:00:00:00" {
                    None
                } else {
                    Some(mac)
                },
                rx_bytes: data.total_received(),
                tx_bytes: data.total_transmitted(),
                rx_bytes_per_sec,
                tx_bytes_per_sec,
            }
        })
        .collect();
    list.sort_by(|a, b| a.name.cmp(&b.name));
    list
}

/// 接口类型分类（设计 §2.2 顺序）：lo → 回环；wireless 子目录 → wifi；
/// br-/virbr → 网桥；docker/veth → 虚拟；enp/eth/enx → 以太网；wl → wifi；
/// tun/tap/wg 等一律 other（VPN 归 other）。
fn classify_iface(name: &str, has_wireless_dir: bool) -> &'static str {
    if name == "lo" {
        return "loopback";
    }
    if has_wireless_dir {
        return "wifi";
    }
    if name.starts_with("br-") || name.starts_with("virbr") {
        return "bridge";
    }
    if name.starts_with("docker") || name.starts_with("veth") {
        return "virtual";
    }
    if name.starts_with("enp") || name.starts_with("eth") || name.starts_with("enx") {
        return "ethernet";
    }
    if name.starts_with("wl") {
        return "wifi";
    }
    "other"
}

/// 进程 top 列表：CPU% 依赖同一个 System 实例两次刷新的差值（DeviceState 持久
/// 实例保证这一点）；首拍全部为 0 属预期，第二拍起出真实值。
fn collect_processes(state: &DeviceStateInner) -> ProcessSummary {
    let mut system = state.system.lock();
    // 只需要 CPU%+内存：显式收窄刷新 kind。sysinfo 默认 kind 还会带 disk_usage、
    // exe、tasks——每个进程每拍多读 /proc/<pid>/io、exe 与 task/ 目录，数百进程
    // 的机器上是无谓放大。内存字段来自 refresh_processes 的 with_memory，本模块
    // 不消费系统级内存，refresh_memory() 是死代码（评审确认），不调用。
    system.refresh_processes_specifics(
        ProcessesToUpdate::All,
        true,
        ProcessRefreshKind::nothing().with_cpu().with_memory(),
    );
    let procs: Vec<ProcInfo> = system
        .processes()
        .values()
        .map(|process| ProcInfo {
            pid: process.pid().as_u32(),
            name: truncate_name(&process.name().to_string_lossy()),
            cpu_pct: process.cpu_usage(),
            memory_bytes: process.memory(),
        })
        .collect();
    let mut top_cpu = procs.clone();
    top_cpu.sort_by(|a, b| {
        b.cpu_pct
            .partial_cmp(&a.cpu_pct)
            .unwrap_or(std::cmp::Ordering::Equal)
            .then_with(|| b.memory_bytes.cmp(&a.memory_bytes))
    });
    top_cpu.truncate(TOP_N);
    let mut top_memory = procs;
    // 等值内存追加 name/pid 决胜键：跨拍顺序漂移会让列表行反复跳动（第三轮评审 P3）。
    top_memory.sort_by(|a, b| {
        b.memory_bytes
            .cmp(&a.memory_bytes)
            .then_with(|| a.name.cmp(&b.name))
            .then_with(|| a.pid.cmp(&b.pid))
    });
    top_memory.truncate(TOP_N);
    ProcessSummary {
        top_cpu,
        top_memory,
    }
}

/// 按字符（而非字节）截断到 40，避免切开中文/emoji。
fn truncate_name(raw: &str) -> String {
    raw.chars().take(PROCESS_NAME_MAX_CHARS).collect()
}

// ---------------------------------------------------------------- GPU 扩展

/// GPU 扩展快照（3s 缓存，monitor::gpu_snapshot 同款结构）。nvidia-smi 探测
/// 失败/超时按"本机无 NVIDIA"降级返回 None。
fn gpu_ext_cached() -> Option<GpuExtSnapshot> {
    static CACHE: OnceLock<Mutex<GpuExtCache>> = OnceLock::new();
    let cache = CACHE.get_or_init(|| Mutex::new(GpuExtCache::default()));
    let mut guard = cache.lock();
    if guard
        .sampled_at
        .is_some_and(|sampled_at| sampled_at.elapsed() < GPU_EXT_CACHE_TTL)
    {
        return guard.value.clone();
    }
    let value = gpu_ext_snapshot();
    guard.sampled_at = Some(Instant::now());
    // 采样失败（瞬时超时/驱动忙）保留上一份好值、只刷新时间戳，与 monitor
    // TtlSnapshotCache 的失败保旧同款——否则一次抖动会让 compute 列表/驱动徽章
    // 在页面上闪没（第三轮评审 P3）。
    if value.is_some() || guard.value.is_none() {
        guard.value = value;
    }
    guard.value.clone()
}

#[derive(Default)]
struct GpuExtCache {
    sampled_at: Option<Instant>,
    value: Option<GpuExtSnapshot>,
}

/// 驱动与 CUDA 版本：一次裸跑 `nvidia-smi` 同时取表头里的 `Driver Version:` 与
/// `CUDA Version:`（原先驱动查询+CUDA 兜底要两次调用；同一挂死的二进制按 3 个
/// 候选路径重试会把最坏预算三重放大到 60-90s，第三轮评审 P2）。裸跑失败退
/// `/proc` 的驱动版本；两者皆缺 → 视为本机无 NVIDIA，整块返回 None。
fn gpu_ext_snapshot() -> Option<GpuExtSnapshot> {
    let smi_output = run_nvidia_smi(&[]);
    let driver_version = smi_output
        .as_deref()
        .and_then(parse_nvidia_smi_driver_version)
        .or_else(platform::nvidia_driver_version)?;
    Some(GpuExtSnapshot {
        driver_version: Some(driver_version),
        cuda_version: platform::cuda_toolkit_version().or_else(|| {
            smi_output
                .as_deref()
                .and_then(parse_nvidia_smi_cuda_version)
        }),
        compute_processes: nvidia_compute_processes(),
    })
}

/// 跑一次 nvidia-smi，返回 stdout（成功时）。候选路径与超时预算复用 monitor。
fn run_nvidia_smi(args: &[&str]) -> Option<String> {
    crate::platform::os::nvidia_smi_candidates()
        .into_iter()
        .find_map(|candidate| {
            let mut command = crate::platform::process::HiddenCommand::new(candidate);
            command.args(args);
            crate::platform::process::output_with_timeout(command, GPU_PROBE_TIMEOUT)
                .ok()
                .filter(|output| output.status.success())
                .and_then(|output| String::from_utf8(output.stdout).ok())
        })
}

pub(crate) fn parse_cuda_toolkit_version(json_text: &str) -> Option<String> {
    let value: serde_json::Value = serde_json::from_str(json_text).ok()?;
    value
        .get("cuda")?
        .get("version")?
        .as_str()
        .map(|version| version.to_string())
        .filter(|version| !version.is_empty())
}

/// 表头键值对的公共解析：定位 `<Key>:` 取首个空白分隔 token（表头列以 `|` 收边）。
fn parse_nvidia_smi_header_value(text: &str, key: &str) -> Option<String> {
    let rest = text.split(key).nth(1)?;
    let token = rest.split_whitespace().next()?;
    if token.ends_with('|') {
        return Some(token.trim_end_matches('|').to_string()).filter(|s| !s.is_empty());
    }
    Some(token.to_string())
}

/// 兜底：`nvidia-smi` 表头的 `CUDA Version: N.N`。
fn parse_nvidia_smi_cuda_version(text: &str) -> Option<String> {
    parse_nvidia_smi_header_value(text, "CUDA Version:")
}

fn parse_nvidia_smi_driver_version(text: &str) -> Option<String> {
    parse_nvidia_smi_header_value(text, "Driver Version:")
}

/// 计算进程：`--query-compute-apps=pid,process_name,used_memory`。
fn nvidia_compute_processes() -> Vec<GpuProcInfo> {
    let Some(out) = run_nvidia_smi(&[
        "--query-compute-apps=pid,process_name,used_memory",
        "--format=csv,noheader,nounits",
    ]) else {
        return Vec::new();
    };
    out.lines().filter_map(parse_compute_app_line).collect()
}

fn parse_compute_app_line(line: &str) -> Option<GpuProcInfo> {
    let parts: Vec<&str> = line.split(',').map(|part| part.trim()).collect();
    if parts.len() < 3 {
        return None;
    }
    // CSV 无引号转义：进程名含逗号时不能按固定下标取（名字会被截断、内存落 0），
    // 首段是 pid、末段是内存、中间整段归名字（第三轮评审 P3）。
    let used_mib = parts[parts.len() - 1].parse().unwrap_or(0);
    let name = parts[1..parts.len() - 1].join(",");
    Some(GpuProcInfo {
        pid: parts[0].parse().ok()?,
        // unified-memory 设备 used_memory 可能是 [N/A]：按 0 处理，不丢整行。
        used_mib,
        name: truncate_name(&name),
    })
}

// ---------------------------------------------------------------- 服务探测

/// 模型服务可达性：ollama / vllm / lmstudio 走 HTTP 端口探测（1.5s 超时），
/// docker 走 CLI（3s 超时）。失败给一句人话 detail。四路并发：docker 的
/// spawn_blocking 提到 join 之前，否则总延迟 = HTTP 探测 + docker 串行和而非
/// "最慢一路"（第三轮评审 P3）。
pub async fn probe_services() -> Vec<ServiceStatus> {
    let docker_task = tokio::task::spawn_blocking(probe_docker_service);
    let (ollama, vllm, lmstudio, docker) = tokio::join!(
        probe_ollama_service(),
        probe_openai_ports("vllm", &[8000, 8001, 8002],),
        probe_openai_ports("lmstudio", &[1234]),
        async {
            docker_task.await.unwrap_or_else(|_| ServiceStatus {
                key: "docker".to_string(),
                running: false,
                detail: Some("docker 探测任务失败".to_string()),
            })
        },
    );
    vec![ollama, vllm, lmstudio, docker]
}

/// ollama → `GET <ollama_base>/api/tags`（1.5s 超时）。基地址与商店设置
/// （config.json 的 ollama_host）对齐——改配过的机器上探测、标签、删除必须
/// 指向同一守护进程。
async fn probe_ollama_service() -> ServiceStatus {
    let base = ollama_base().await;
    match http_get_success(&format!("{base}/api/tags")).await {
        Ok(()) => ServiceStatus {
            key: "ollama".to_string(),
            running: true,
            detail: None,
        },
        Err(reason) => ServiceStatus {
            key: "ollama".to_string(),
            running: false,
            detail: Some(reason),
        },
    }
}

/// vllm / lmstudio → 对候选端口逐个探测 `/v1/models`，任一 200 即 running。
async fn probe_openai_ports(key: &str, ports: &[u16]) -> ServiceStatus {
    for port in ports {
        if http_get_success(&format!("http://127.0.0.1:{port}/v1/models"))
            .await
            .is_ok()
        {
            return ServiceStatus {
                key: key.to_string(),
                running: true,
                detail: None,
            };
        }
    }
    ServiceStatus {
        key: key.to_string(),
        running: false,
        detail: Some(format!(
            "未在端口 {} 探测到服务（连接被拒绝或超时）",
            ports
                .iter()
                .map(|port| port.to_string())
                .collect::<Vec<_>>()
                .join("/")
        )),
    }
}

/// docker → `docker info --format {{.ServerVersion}}`（3s 超时，失败即 not running）。
fn probe_docker_service() -> ServiceStatus {
    let mut command = crate::platform::process::HiddenCommand::new("docker");
    command.args(["info", "--format", "{{.ServerVersion}}"]);
    match crate::platform::process::output_with_timeout(command, Duration::from_secs(3)) {
        Ok(output) if output.status.success() => {
            // stdout 全量进 detail 有整段涌入上下文/UI 的通道，截到 64 字符足够装版本号。
            let version: String = String::from_utf8_lossy(&output.stdout)
                .trim()
                .chars()
                .take(64)
                .collect();
            let detail = if version.is_empty() {
                None
            } else {
                Some(version)
            };
            ServiceStatus {
                key: "docker".to_string(),
                running: true,
                detail,
            }
        }
        Ok(output) => ServiceStatus {
            key: "docker".to_string(),
            running: false,
            detail: Some(format!(
                "docker 命令返回异常（exit {}），守护进程可能未运行",
                output.status.code().unwrap_or(-1)
            )),
        },
        Err(error) => ServiceStatus {
            key: "docker".to_string(),
            running: false,
            detail: Some(format!("docker 命令不可用：{error}")),
        },
    }
}

// ---------------------------------------------------------------- 共用探针

pub(crate) const OLLAMA_BASE: &str = "http://127.0.0.1:11434";
pub(crate) const PROBE_TIMEOUT: Duration = Duration::from_millis(1500);

/// 本机 Ollama 守护进程基地址：商店设置里的 `ollama_host`（config.json 可改）
/// 优先，未配置时用出厂默认。服务探测、外部模型标签与删除必须与商店的 ollama
/// 生命周期指向**同一**守护进程，否则改配过的机器上设备页会读写另一个实例
/// （第三轮评审 P3 对齐项）。settings 读取是阻塞文件 IO，走 blocking 池。
pub(crate) async fn ollama_base() -> String {
    tokio::task::spawn_blocking(crate::features::model_store::ollama_base_url)
        .await
        .unwrap_or_else(|_| OLLAMA_BASE.to_string())
}

/// 短超时探测客户端（OnceLock 共享；构建失败降级为每次现建，仍失败返回 None）。
/// 显式禁用代理：客户端只访问 127.0.0.1，reqwest 0.13 的 `system-proxy` feature
/// 会吃 `http_proxy` 环境变量且对 loopback 无隐式豁免——宿主设了代理而 no_proxy
/// 没盖住 127.0.0.1 时，三个服务探测会全部改走代理然后假报"连接被拒绝"
/// （第三轮评审 P2）。
pub(crate) fn probe_client() -> Option<&'static reqwest::Client> {
    static CLIENT: OnceLock<Option<reqwest::Client>> = OnceLock::new();
    CLIENT
        .get_or_init(|| reqwest::Client::builder().no_proxy().build().ok())
        .as_ref()
}

/// GET 一个 URL，2xx 即 Ok；非 2xx / 连接失败 / 超时 → 人话 Err。
pub(crate) async fn http_get_success(url: &str) -> Result<(), String> {
    let client = probe_client().ok_or_else(|| "HTTP 客户端初始化失败".to_string())?;
    let response = client
        .get(url)
        .timeout(PROBE_TIMEOUT)
        .send()
        .await
        .map_err(human_probe_error)?;
    if response.status().is_success() {
        Ok(())
    } else {
        Err(format!(
            "服务响应异常（HTTP {}）",
            response.status().as_u16()
        ))
    }
}

/// reqwest 错误 → 人话原因（超时 / 拒绝连接 / 其他）。
pub(crate) fn human_probe_error(error: reqwest::Error) -> String {
    if error.is_timeout() {
        "请求超时".to_string()
    } else if error.is_connect() {
        "连接被拒绝（服务可能未启动）".to_string()
    } else {
        error.to_string()
    }
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|duration| duration.as_millis() as u64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use sysinfo::Pid;

    #[test]
    fn split_component_label_splits_first_token_as_chip() {
        assert_eq!(
            split_component_label("coretemp Package id 0"),
            ("coretemp".to_string(), "Package id 0".to_string())
        );
    }

    #[test]
    fn split_component_label_keeps_single_token_labels() {
        assert_eq!(
            split_component_label("acpitz"),
            ("acpitz".to_string(), "acpitz".to_string())
        );
        assert_eq!(
            split_component_label("  nvme pcie4  "),
            ("nvme".to_string(), "pcie4".to_string())
        );
        assert_eq!(split_component_label(""), (String::new(), String::new()));
    }

    #[test]
    fn classify_iface_follows_design_order() {
        // lo 最优先，即使存在 wireless 子目录。
        assert_eq!(classify_iface("lo", true), "loopback");
        // wireless 子目录命中非 wl 命名网卡。
        assert_eq!(classify_iface("enp2s0", true), "wifi");
        assert_eq!(classify_iface("wlp3s0", false), "wifi");
        // 桥 / 虚拟 / 以太网。
        assert_eq!(classify_iface("br-1a2b3c", false), "bridge");
        assert_eq!(classify_iface("virbr0", false), "bridge");
        assert_eq!(classify_iface("docker0", false), "virtual");
        assert_eq!(classify_iface("veth8a2f1b0", false), "virtual");
        assert_eq!(classify_iface("enp1s0", false), "ethernet");
        assert_eq!(classify_iface("eth0", false), "ethernet");
        assert_eq!(classify_iface("enx00e04c680201", false), "ethernet");
        // VPN 与未知一律 other。
        assert_eq!(classify_iface("tun0", false), "other");
        assert_eq!(classify_iface("tap9", false), "other");
        assert_eq!(classify_iface("wg0", false), "other");
        assert_eq!(classify_iface("wwan0", false), "other");
    }

    #[test]
    fn disk_filters_drop_squashfs_and_snap_mounts() {
        let squash = DiskSnapshot {
            name: None,
            mount_point: "/snap/core22/1380".to_string(),
            fs_type: "squashfs".to_string(),
            kind: "unknown".to_string(),
            total_bytes: 1,
            available_bytes: 0,
            is_removable: false,
        };
        let overlay = DiskSnapshot {
            fs_type: "overlay".to_string(),
            mount_point: "/var/lib/docker/overlay2/x/merged".to_string(),
            ..squash.clone()
        };
        assert!(!keep_disk(&squash));
        // squashfs 类型即过滤，无论挂载点；非 squashfs 的普通挂载保留。
        let mut root = squash.clone();
        root.fs_type = "ext4".to_string();
        root.mount_point = "/".to_string();
        assert!(keep_disk(&root));
        assert!(keep_disk(&overlay));
    }

    #[test]
    fn disk_sort_puts_root_first_efi_last_then_total_desc() {
        let make = |mount: &str, total: u64| DiskSnapshot {
            name: None,
            mount_point: mount.to_string(),
            fs_type: "ext4".to_string(),
            kind: "ssd".to_string(),
            total_bytes: total,
            available_bytes: 0,
            is_removable: false,
        };
        let mut list = vec![
            make("/boot/efi", 1),
            make("/home", 500),
            make("/boot/efi", 1),
            make("/", 100),
            make("/data", 2000),
        ];
        sort_disks(&mut list);
        let mounts: Vec<&str> = list.iter().map(|d| d.mount_point.as_str()).collect();
        assert_eq!(
            mounts,
            vec!["/", "/data", "/home", "/boot/efi", "/boot/efi"]
        );
    }

    #[test]
    fn truncate_name_is_charwise_not_bytewise() {
        assert_eq!(truncate_name("short"), "short");
        let long_chinese: String = "模型进程".repeat(20); // 80 chars
        assert_eq!(truncate_name(&long_chinese).chars().count(), 40);
    }

    #[test]
    fn parse_compute_app_line_handles_na_memory_and_garbage() {
        let info = parse_compute_app_line("1234, /usr/bin/python3, 4096").expect("normal line");
        assert_eq!(info.pid, 1234);
        assert_eq!(info.used_mib, 4096);
        // GB10 unified-memory：used_memory 为 [N/A] → 按 0，行保留。
        let info = parse_compute_app_line("1234, python3, [N/A]").expect("N/A tolerated");
        assert_eq!(info.used_mib, 0);
        assert!(parse_compute_app_line("not,a,line").is_none());
        assert!(parse_compute_app_line("").is_none());
    }

    #[test]
    fn parse_cuda_toolkit_version_reads_cuda_field() {
        let json =
            r#"{"cuda":{"name":"CUDA SDK","version":"12.3.1"},"nvcc":{"version":"12.3.107"}}"#;
        assert_eq!(parse_cuda_toolkit_version(json), Some("12.3.1".to_string()));
        assert_eq!(parse_cuda_toolkit_version("not json"), None);
        assert_eq!(parse_cuda_toolkit_version(r#"{"other":1}"#), None);
    }

    #[test]
    fn parse_nvidia_smi_header_cuda_version() {
        // 表头竖线直接跟在版本号后。
        let header = "| NVIDIA-SMI 540.4.0    Driver Version: 540.4.0    CUDA Version: 12.3 |\n";
        assert_eq!(
            parse_nvidia_smi_cuda_version(header),
            Some("12.3".to_string())
        );
        assert_eq!(parse_nvidia_smi_cuda_version("no header"), None);
    }

    #[test]
    fn device_state_constructs_and_is_defaultable() {
        // sysinfo 实例在 new() 即创建；两拍之间必须是同一实例（CPU% 差值的前提）。
        let state = DeviceState::default();
        let first = collect_processes(&state.0);
        let second = collect_processes(&state.0);
        // 冒烟：结构可序列化，列表各自 ≤ 8。
        assert!(first.top_cpu.len() <= 8 && second.top_memory.len() <= 8);
        let serialized = serde_json::to_string(&second).expect("ProcessSummary serializes");
        assert!(serialized.contains("top_cpu"));
    }

    #[test]
    fn snapshot_serialization_smoke_has_contract_fields() {
        let snapshot = DeviceSnapshot {
            generated_at_ms: 1,
            os: OsInfo {
                product_name: Some("LinSeer MegaCube".to_string()),
                pretty_name: None,
                kernel: "6.0.0".to_string(),
                arch: "aarch64".to_string(),
            },
            sensors: vec![SensorGroup {
                chip: "coretemp".to_string(),
                entries: vec![SensorEntry {
                    kind: "temperature".to_string(),
                    name: "Package id 0".to_string(),
                    value: 45.0,
                    critical: Some(100.0),
                    max: Some(80.0),
                }],
            }],
            disks: Vec::new(),
            network: Vec::new(),
            processes: ProcessSummary {
                top_cpu: Vec::new(),
                top_memory: Vec::new(),
            },
            gpu_ext: None,
        };
        let value = serde_json::to_value(&snapshot).expect("serialize");
        // 设计 §2.2 契约字段名（前端按这些键取数）。
        for key in [
            "generated_at_ms",
            "os",
            "sensors",
            "disks",
            "network",
            "processes",
            "gpu_ext",
        ] {
            assert!(value.get(key).is_some(), "missing contract key: {key}");
        }
        assert_eq!(value["os"]["product_name"], "LinSeer MegaCube");
    }

    #[test]
    fn network_snapshot_smoke_on_first_poll_has_zero_rates() {
        // 首拍（last_network_poll 为 None）速率必须为 0，累计量可用。
        let state = DeviceState::new();
        let list = collect_network(&state.0);
        for iface in &list {
            assert_eq!(iface.rx_bytes_per_sec, 0.0);
            assert_eq!(iface.tx_bytes_per_sec, 0.0);
        }
    }

    #[test]
    fn pid_wrapper_maps_sysinfo_pid() {
        // ProcInfo.pid 是 u32：锁死 sysinfo Pid → u32 的转换路径（aarch64 上 Pid 为 i32）。
        let pid: Pid = Pid::from_u32(4242);
        assert_eq!(pid.as_u32(), 4242);
    }
}
