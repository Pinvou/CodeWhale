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

use parking_lot::{Mutex, RwLock};
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
    /// 安装态 CUDA toolkit（version.json）优先于驱动表头的 `CUDA Version:`
    /// （驱动支持的上限）——卸载 toolkit 会让本字段的语义静默切换口径，两者
    /// 并列展示时按这个优先级解读（doctor 模板已指示模型对比两者，第七轮评审
    /// P3 补记字段口径）。取值限长 64 字符（与 docker 版本臂同一口径）。
    pub cuda_version: Option<String>,
    /// 计算进程列表：`Some(列表)` 是查询成功（空列表 = 确实在跑零个计算进程），
    /// `None` = 查询失败/不可用（nvidia-smi 调用失败），与“无进程"显式区分——
    /// 二者混同会让排查上下文把"查询失败"说成"没有计算进程"（十六轮评审）。
    pub compute_processes: Option<Vec<GpuProcInfo>>,
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
    /// 人话补充（中文，进 doctor 上下文与日志）：running=false 时是失败原因
    /// （超时/拒绝连接/命令不存在）；docker running 时是版本号。
    pub detail: Option<String>,
    /// 失败原因的稳定机器码（running=false 时有值）：`timeout` /
    /// `connection_refused` / `network_error` / `client_init_failed` /
    /// `http_status` / `not_detected` / `docker_exit` / `docker_unavailable` /
    /// `docker_task_failed`。前端据此映射三语 tooltip——此前只有中文 `detail`
    /// 直出，en/ja 用户在服务芯片上读到中文（二十一轮评审 P2，§4 三语公约）。
    pub reason: Option<&'static str>,
    /// 与 `reason` 配套的数字（`http_status` 的状态码 / `docker_exit` 的退出码）。
    pub code: Option<i64>,
}

impl ServiceStatus {
    fn running(key: &str, detail: Option<String>) -> Self {
        Self {
            key: key.to_string(),
            running: true,
            detail,
            reason: None,
            code: None,
        }
    }

    fn down(key: &str, detail: String, reason: &'static str, code: Option<i64>) -> Self {
        Self {
            key: key.to_string(),
            running: false,
            detail: Some(detail),
            reason: Some(reason),
            code,
        }
    }
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
    last_process_poll: Mutex<Option<Instant>>,
}

impl DeviceState {
    pub fn new() -> Self {
        // Linux 上 sysinfo 默认会缓存接近一半 FD 上限的 /proc stat 句柄；ACP
        // ledger（platform/process_resources.rs）以全局 `set_open_files_limit(0)`
        // 换取有界描述符占用。这是进程级全局开关，且 sysinfo 的语义是
        // last-writer-wins（每次调用都会覆盖全局值，并非"谁先初始化谁生效"——
        // 第九轮评审修正旧注释）；今天两处调用点（本处与 process_resources.rs）
        // 都写 0，行为因此一致：本模块的 CPU% 只依赖两次刷新的计数值差，不需要
        // FD 缓存。任一侧将来想改值，必须两处同步决策，不能依赖初始化顺序。
        let _ = sysinfo::set_open_files_limit(0);
        Self(Arc::new(DeviceStateInner {
            system: Mutex::new(System::new()),
            networks: Mutex::new(Networks::new()),
            last_network_poll: Mutex::new(None),
            last_process_poll: Mutex::new(None),
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

/// sysinfo 0.36 的 `Component::max()` 语义：hwmon `tempN_highest`（开机以来的
/// **历史峰值**），且芯片没有 highest 文件时**回退为当前读数**（上游
/// fill_component 的 "input" 臂：`max.is_none() => max = temperature`）；真实
/// 阈值 `tempN_max` 上游未采用（源码注释掉）。把回退值当阈值用会让下游 ±10°
/// 告警带在无 highest 的芯片上恒 warn——只保留严格高于当前读数的历史峰值
/// （第七轮评审 P2）；纯函数便于测试直打。
fn component_peak_above_current(celsius: f64, max: Option<f64>) -> Option<f64> {
    max.filter(|max| *max > celsius)
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
            component_peak_above_current(celsius.into(), component.max().map(f64::from)),
        );
    }
    for (chip, kind, name, value, critical, max) in platform::extra_sensors() {
        push_sensor(&mut groups, chip, &kind, name, value, critical, max);
    }
    sort_sensors(&mut groups);
    groups
}

/// 传感器输出排序（组按 chip、组内按 kind+name）。sysinfo 0.36.1 的 Components
/// 在 Linux 上由 HashMap 聚合（component.rs `from_hwmon`），每次
/// `new_with_refreshed_list()` 迭代序都不同——不排序的话组序/条目序逐拍漂移，
/// 而 bridge 的显示等价抑制对数组是逐元素比较，会把纯顺序抖动当成真实变化，
/// 设备页每 2s 重渲染一次、卡片可见跳动（第四轮评审 P2）。组数极小，排序开销
/// 可忽略；磁盘/网络/进程列表本就各自确定性排序，这里补齐最后一个缺口。
fn sort_sensors(groups: &mut [SensorGroup]) {
    groups.sort_by(|a, b| a.chip.cmp(&b.chip));
    for group in groups {
        // value 只作决胜键：同 chip 下重复 hwmon label（键完全相同而值不同）在
        // stable sort 里会退化回 HashMap 迭代序，把值漂移误报成列表变化
        // （第六轮评审 P3）。
        group.entries.sort_by(|a, b| {
            a.kind
                .cmp(&b.kind)
                .then_with(|| a.name.cmp(&b.name))
                .then_with(|| {
                    a.value
                        .partial_cmp(&b.value)
                        .unwrap_or(std::cmp::Ordering::Equal)
                })
        });
    }
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

/// 过滤噪音挂载：squashfs（snap 的只读 squash 包）与 snap 挂载点；overlay
/// （docker/containerd/podman 的容器叠层，每容器一份、容量复制自根文件系统，
/// 模型服务工作流一跑容器就会刷出一排同名假磁盘——第四轮评审 P3；Live USB 的
/// overlay 根挂载是仅有的误伤面，不在本功能的目标形态内）；`/boot/efi` 保留
/// （用户关心 EFI 分区占用），只是排序垫底。
fn keep_disk(disk: &DiskSnapshot) -> bool {
    if disk.fs_type.eq_ignore_ascii_case("squashfs") {
        return false;
    }
    if disk.fs_type.eq_ignore_ascii_case("overlay") {
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
                Some(secs) => (
                    rate_from_delta(data.received(), secs),
                    rate_from_delta(data.transmitted(), secs),
                ),
                None => (0.0, 0.0),
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

/// 速率 = 字节增量 / 秒；间隔非正（时钟回拨/同拍重入）按 0，绝不出负速率或
/// 除零。抽成纯函数以便把方向（增量 ÷ 秒）钉进测试——算反了页面上显示的
/// 就是恒错数字（第四轮评审：正时长分支此前零覆盖）。
fn rate_from_delta(delta_bytes: u64, secs: f64) -> f64 {
    if secs > 0.0 {
        delta_bytes as f64 / secs
    } else {
        0.0
    }
}

/// 接口类型分类（设计 §2.2 顺序）：lo → 回环；wireless 子目录 → wifi；
/// br-/virbr/裸 br+数字（OVS/手工网桥惯例，如 br0）→ 网桥；docker/veth → 虚拟；
/// enp/eth/enx → 以太网；wl → wifi；tun/tap/wg 等一律 other（VPN 归 other）。
fn classify_iface(name: &str, has_wireless_dir: bool) -> &'static str {
    if name == "lo" {
        return "loopback";
    }
    if has_wireless_dir {
        return "wifi";
    }
    if name.starts_with("br-") || name.starts_with("virbr") || is_bare_bridge_name(name) {
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

/// `br0`/`br10` 这类"br+纯数字"命名（OVS/手工网桥的常见惯例）；`brctl` 等非
/// 数字后缀不命中（那是工具名形状，不是网桥命名）。
fn is_bare_bridge_name(name: &str) -> bool {
    name.strip_prefix("br")
        .is_some_and(|rest| !rest.is_empty() && rest.bytes().all(|byte| byte.is_ascii_digit()))
}

/// 距上次进程刷新不足 [`sysinfo::MINIMUM_CPU_UPDATE_INTERVAL`] 时跳过重刷新：
/// sysinfo 的进程 CPU% 依赖 /proc/stat 的全局 CPU 时间差，而该文件在窗口内不
/// 重读——此时 per-process utime/stime 照常前进、全局差值却为 0，cpu_usage 会
/// 算出 inf 并被封顶成"核数×100"的荒谬值。设备页 2s 轮询与 AI 排查上下文共用
/// 同一个 [`DeviceState`]，两次采样撞进窗口时后到的一拍就会呈现假 CPU%
/// （十二轮评审 P2）。窗口内沿用上一次的进程表：CPU% 仍是上次真实差值，内存
/// 最多陈旧 <200ms。纯函数便于测试直打（进程级 OnceLock/实例缓存没法安全变换
/// 时钟，与 megacube_device_decision 同款拆法）。
fn should_refresh_processes(last: Option<Instant>, now: Instant) -> bool {
    match last {
        None => true,
        Some(last) => now.duration_since(last) >= sysinfo::MINIMUM_CPU_UPDATE_INTERVAL,
    }
}

/// 进程 top 列表：CPU% 依赖同一个 System 实例两次刷新的差值（DeviceState 持久
/// 实例保证这一点）；首拍全部为 0 属预期，第二拍起出真实值。
fn collect_processes(state: &DeviceStateInner) -> ProcessSummary {
    let mut system = state.system.lock();
    // 只需要 CPU%+内存：显式收窄刷新 kind。sysinfo 默认 kind 还会带 disk_usage、
    // exe、tasks——每个进程每拍多读 /proc/<pid>/io、exe 与 task/ 目录，数百进程
    // 的机器上是无谓放大。内存字段来自 refresh_processes 的 with_memory，本模块
    // 不消费系统级内存，refresh_memory() 是死代码（评审确认），不调用。
    let now = Instant::now();
    if should_refresh_processes(*state.last_process_poll.lock(), now) {
        system.refresh_processes_specifics(
            ProcessesToUpdate::All,
            true,
            ProcessRefreshKind::nothing().with_cpu().with_memory(),
        );
        // 记录时刻必须在 refresh **返回之后**：sysinfo 的内部窗口以刷新结束
        // 时刻为基准（先遍历 /proc 全表、末尾才置自己的 last_update）。若记
        // 刷新前的时刻，上一拍遍历耗时 ≥200ms 时下一拍会同时满足本守卫、又
        // 落不进 sysinfo 内部窗口——全局 /proc/stat 不重读而 per-process 时间
        // 照常前进，整表 CPU% 被封顶成核数×100（十九轮评审 P2）。
        *state.last_process_poll.lock() = Some(Instant::now());
    }
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
    // 等值 CPU/内存追加 name/pid 决胜键，与 top_memory 同口径：等值键回退到
    // HashMap 迭代序会让前 8 名的成员跨拍反复跳动（top_memory 已修，第三轮
    // 评审 P3；top_cpu 漏了同一刀，第四轮评审 P3 补齐）。
    top_cpu.sort_by(|a, b| {
        b.cpu_pct
            .partial_cmp(&a.cpu_pct)
            .unwrap_or(std::cmp::Ordering::Equal)
            .then_with(|| b.memory_bytes.cmp(&a.memory_bytes))
            .then_with(|| a.name.cmp(&b.name))
            .then_with(|| a.pid.cmp(&b.pid))
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
///
/// 并发口径（第五轮评审）：探测锁（`SAMPLER`）在子进程探测期间持有——一次刷新 =
/// 表头 + 计算进程两次 nvidia-smi，每次至多 3 个候选路径 × 10s 超时，最坏 ~60s。
/// 与 monitor「挂起期间缓存互斥量被持有」不同的是，这里**绝不排队等锁**：
/// `try_lock` 抢不到（上一轮探测仍在飞）直接返回 `LAST` 里的最后已知值——否则
/// 挂死的 nvidia-smi 会把整份设备快照（温度/磁盘/网络/进程均已采好）在
/// `sample_snapshot` 的 `join!` 上冻结同样长的时间，而不是只让 GPU 区块降级。
/// 首拍就被并发踩上时 `LAST` 为空，返回 None（与"本机无 NVIDIA"同形，探测结束
/// 后下一拍自愈）。有界且自愈：探测内部超时会 kill 子进程。
///
/// 边界要说清（第六轮评审 P2）：不排队保护的是**并发调用方**（设备页 2s 轮询
/// 与 doctor 上下文并发时，拿不到锁的一路立即取旧值返回）。若轮询自己是拿锁
/// 的一路，本拍仍要等探测完成——前端 `pollDevice` 的 in-flight 守卫是串行的，
/// 期间不会再发新拍。即挂死场景下设备页表现为「最长一拍 ~60s 停更、GPU 区块
/// 沿用旧值、超时后自愈」，而不是永不冻结。彻底解耦需把探测搬到独立后台
/// 任务，属后续架构项。
fn gpu_ext_cached() -> Option<GpuExtSnapshot> {
    static SAMPLER: OnceLock<Mutex<GpuExtCache>> = OnceLock::new();
    static LAST: OnceLock<RwLock<Option<GpuExtSnapshot>>> = OnceLock::new();
    gpu_ext_cached_with(
        SAMPLER.get_or_init(|| Mutex::new(GpuExtCache::default())),
        LAST.get_or_init(|| RwLock::new(None)),
        gpu_ext_snapshot,
    )
}

/// 并发口径的可测形态（锁与最后已知值由调用方持有）：抢到探测锁走缓存刷新并
/// 发布到 `last`；抢不到（上一轮探测在飞）立即返回 `last`，**不排队、不采样**。
/// 此前整段锁在函数级 static 后面，"不排队返回最后已知值"这条核心并发契约把
/// 失败臂改成 `None => None` 全套测试照绿（二十一轮评审 P2 变异实证）。
fn gpu_ext_cached_with(
    sampler: &Mutex<GpuExtCache>,
    last: &RwLock<Option<GpuExtSnapshot>>,
    sample: impl Fn() -> Option<GpuExtSnapshot>,
) -> Option<GpuExtSnapshot> {
    match sampler.try_lock() {
        Some(mut guard) => {
            // TTL 锚定在采样**完成**时刻（与 monitor TtlSnapshotCache 同口径，
            // 第二十轮评审 P2）：锚在开始时刻时，nvidia-smi 探测落入 ≥1s 的慢带
            // 会让 3s TTL 在下一拍（2s 轮询）恒判过期、每拍重拉子进程，缓存
            // "不必每拍都拉 nvidia-smi"的设计目的在该耗时带完全落空。
            let value = refresh_gpu_ext_cache(&mut guard, Instant::now(), || {
                let value = sample();
                (value, Instant::now())
            });
            *last.write() = value.clone();
            value
        }
        // 探测在飞：返回最后已知值，不排队（见上方并发口径）。
        None => last.read().clone(),
    }
}

/// 缓存刷新的纯逻辑（测试直打）：TTL 内直接返回旧值不触发采样；过期后采样，
/// 采样成功用新值、失败保留旧值只刷时间戳（monitor TtlSnapshotCache 的失败
/// 保旧同款——一次抖动不该让 compute 列表/驱动徽章在页面上闪没，第三轮评审
/// P3）；首拍就失败保持 None。抽成纯函数是因为原先这条最关键的保旧分支锁在
/// OnceLock + 真子进程后面，零覆盖（第四轮评审 P3）。`sample` 返回
/// `(值, 完成时刻)`：时间戳取**完成**时刻而非进入本函数的时刻（第二十轮评审
/// P2），慢探测不再把 TTL 窗口从内侧啃掉。
fn refresh_gpu_ext_cache(
    cache: &mut GpuExtCache,
    now: Instant,
    sample: impl FnOnce() -> (Option<GpuExtSnapshot>, Instant),
) -> Option<GpuExtSnapshot> {
    if cache
        .sampled_at
        .is_some_and(|sampled_at| now.duration_since(sampled_at) < GPU_EXT_CACHE_TTL)
    {
        return cache.value.clone();
    }
    let (value, completed_at) = sample();
    cache.sampled_at = Some(completed_at);
    if value.is_some() || cache.value.is_none() {
        cache.value = value;
    }
    cache.value.clone()
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
        // 版本字符串限长 64（与 docker 版本臂同一口径）：来源是 root 所有权
        // 文件/子进程 stdout 的不可信环境文本，超长值会原样进每拍快照 JSON
        // 与 doctor 上下文（第七轮评审 P3）。
        driver_version: Some(capped_version_text(driver_version)),
        cuda_version: platform::cuda_toolkit_version()
            .or_else(|| {
                smi_output
                    .as_deref()
                    .and_then(parse_nvidia_smi_cuda_version)
            })
            .map(capped_version_text),
        compute_processes: nvidia_compute_processes(),
    })
}

/// 不可信来源的版本文本限长 64 字符：足够装任何真实版本号，超长只可能是
/// 环境异常，不值得原样透传。
fn capped_version_text(version: String) -> String {
    version.chars().take(64).collect()
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
/// 驱动不支持对应查询时表头就是 `CUDA Version: N/A`——按"字段缺失"处理（None），
/// 让 `gpu_ext_snapshot` 的回退链继续走，不把字面 "N/A" 当版本号展示（五轮评审 P3）。
fn parse_nvidia_smi_header_value(text: &str, key: &str) -> Option<String> {
    let rest = text.split(key).nth(1)?;
    let token = rest.split_whitespace().next()?;
    let value = if token.ends_with('|') {
        token.trim_end_matches('|')
    } else {
        token
    };
    if value.is_empty() || value.eq_ignore_ascii_case("N/A") {
        return None;
    }
    Some(value.to_string())
}

/// 兜底：`nvidia-smi` 表头的 `CUDA Version: N.N`。
fn parse_nvidia_smi_cuda_version(text: &str) -> Option<String> {
    parse_nvidia_smi_header_value(text, "CUDA Version:")
}

fn parse_nvidia_smi_driver_version(text: &str) -> Option<String> {
    parse_nvidia_smi_header_value(text, "Driver Version:")
}

/// 计算进程：`--query-compute-apps=pid,process_name,used_memory`。按 pid 排序：
/// 传感器/磁盘/网络/进程列表都是确定性排序（见 sort_sensors 的理由），计算进程
/// 列表沿用 nvidia-smi 输出序会在条目重排时击穿 bridge 的显示等价抑制
/// （第六轮评审 P3）；pid 唯一，单键即全序。命令失败返回 None（查询不可知），
/// 不与"成功且零进程"混同（十六轮评审：部分失败曾与"无进程"不可区分）。
fn nvidia_compute_processes() -> Option<Vec<GpuProcInfo>> {
    fold_compute_apps_output(
        run_nvidia_smi(&[
            "--query-compute-apps=pid,process_name,used_memory",
            "--format=csv,noheader,nounits",
        ])
        .as_deref(),
    )
}

/// 纯函数折叠：nvidia-smi 输出 → 计算进程列表。输入 None（命令失败）原样传
/// None；Some(文本) 逐行解析、坏行丢弃、按 pid 排序。测试直打 None/空/正常
/// 三态，钉住"查询失败 ≠ 无进程"的契约。
fn fold_compute_apps_output(out: Option<&str>) -> Option<Vec<GpuProcInfo>> {
    let out = out?;
    let mut list: Vec<GpuProcInfo> = out.lines().filter_map(parse_compute_app_line).collect();
    list.sort_by(|a, b| a.pid.cmp(&b.pid));
    Some(list)
}

fn parse_compute_app_line(line: &str) -> Option<GpuProcInfo> {
    let parts: Vec<&str> = line.split(',').map(|part| part.trim()).collect();
    if parts.len() < 3 {
        return None;
    }
    // CSV 对含逗号字段按惯例加双引号包裹（`1234, "my,proc", 4096`）：不能按
    // 固定下标取（名字会被截断、内存落 0），首段是 pid、末段是内存、中间整段
    // 归名字（第三轮评审 P3），再剥掉 CSV 引号包裹（第六轮评审 P3：原先名字
    // 带字面 `"` 展示）。
    let used_mib = parts[parts.len() - 1].parse().unwrap_or(0);
    let joined = parts[1..parts.len() - 1].join(",");
    let name = joined.trim_matches('"');
    Some(GpuProcInfo {
        pid: parts[0].parse().ok()?,
        // unified-memory 设备 used_memory 可能是 [N/A]：按 0 处理，不丢整行。
        used_mib,
        name: truncate_name(name),
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
            docker_task.await.unwrap_or_else(|_| {
                ServiceStatus::down(
                    "docker",
                    "docker 探测任务失败".to_string(),
                    "docker_task_failed",
                    None,
                )
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
        Ok(()) => ServiceStatus::running("ollama", None),
        Err(failure) => {
            let (reason, code) = failure.reason_code();
            ServiceStatus::down("ollama", failure.message(), reason, code)
        }
    }
}

/// vllm / lmstudio → 对候选端口**并发**探测 `/v1/models`，任一 200 即 running。
/// 串行时 vLLM 三个候选端口逐个 1.5s 超时最坏 4.5s，是 services 慢轮询与
/// doctor 上下文里最慢的一路；并发后总预算 = 单端口超时（十四轮评审）。
/// 结果折叠保持串行时代的确定语义，抽纯函数直打（[`fold_openai_port_probes`]）。
async fn probe_openai_ports(key: &str, ports: &[u16]) -> ServiceStatus {
    // join_all 保序：结果下标与 ports 一一对应，折叠才能按端口序取首个 HTTP
    // 状态错误（与旧串行首见语义一致）。
    let probes = futures_util::future::join_all(ports.iter().map(|&port| async move {
        http_get_success(&format!("http://127.0.0.1:{port}/v1/models")).await
    }))
    .await;
    fold_openai_port_probes(key, ports, probes)
}

/// 端口探测结果的确定性折叠（纯函数，测试直打）：端口被占用但对不上协议的
/// 服务会回 HTTP 4xx——这时"连接被拒绝或超时"是误导，把最具体的 HTTP 状态
/// 透给用户（第四轮评审 P3），多端口并发后按**端口序**取首个状态错误；可判
/// 别分支见 [`ProbeFailure`]——字符串前缀判断会被措辞改动静默击穿（五轮评审
/// P3）。全为传输层失败才给"未探测到"提示。
fn fold_openai_port_probes(
    key: &str,
    ports: &[u16],
    probes: Vec<Result<(), ProbeFailure>>,
) -> ServiceStatus {
    let mut http_error: Option<u16> = None;
    for result in probes {
        match result {
            Ok(()) => return ServiceStatus::running(key, None),
            Err(ProbeFailure::HttpStatus(status)) => {
                if http_error.is_none() {
                    http_error = Some(status);
                }
            }
            Err(ProbeFailure::Transport(_)) => {}
        }
    }
    match http_error {
        Some(status) => ServiceStatus::down(
            key,
            ProbeFailure::HttpStatus(status).message(),
            "http_status",
            Some(i64::from(status)),
        ),
        None => ServiceStatus::down(
            key,
            format!(
                "未在端口 {} 探测到服务（连接被拒绝或超时）",
                ports
                    .iter()
                    .map(|port| port.to_string())
                    .collect::<Vec<_>>()
                    .join("/")
            ),
            "not_detected",
            None,
        ),
    }
}

/// docker → `docker info --format {{.ServerVersion}}`（3s 超时，失败即 not running）。
/// 候选路径与 nvidia-smi 同一模式（`platform::os::docker_candidates`）：桌面入口
/// 启动的应用 PATH 可能不含 docker，裸名探测会把"装了但不在 PATH"误报成"守护
/// 进程未运行"（二十一轮评审 P3）。候选按序尝试，首个能拉起的进程决定结果；
/// 全部拉不起时报最后一个候选的错误。
fn probe_docker_service() -> ServiceStatus {
    let mut last_error: Option<String> = None;
    for candidate in crate::platform::os::docker_candidates() {
        let mut command = crate::platform::process::HiddenCommand::new(candidate);
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
                return ServiceStatus::running("docker", detail);
            }
            Ok(output) => {
                // 命令拉起了但返回非零：守护进程未运行等，不再试其它候选（同一
                // 个 docker 二进制换路径不会改变结论）。
                let exit = output.status.code().unwrap_or(-1);
                return ServiceStatus::down(
                    "docker",
                    format!("docker 命令返回异常（exit {exit}），守护进程可能未运行"),
                    "docker_exit",
                    Some(i64::from(exit)),
                );
            }
            Err(error) => {
                // detail 会进 UI tooltip 与 doctor 上下文：platform 层的超时/超限
                // 错误串内嵌截断到 4000 字符的子进程输出，与成功臂版本号同一 64
                // 字符纪律限长（十九轮评审 P3）。
                last_error = Some(error.to_string().chars().take(64).collect());
            }
        }
    }
    ServiceStatus::down(
        "docker",
        format!(
            "docker 命令不可用：{}",
            last_error.unwrap_or_else(|| "无候选路径".to_string())
        ),
        "docker_unavailable",
        None,
    )
}

// ---------------------------------------------------------------- 共用探针

const OLLAMA_BASE: &str = "http://127.0.0.1:11434";
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

/// [`ollama_base`](self) 的可失败版：settings 读取失败（隔离/损坏/IO）时 `Err`
/// 而不是静默回退默认地址。**破坏性的删除调用必须走本函数**——非默认
/// `ollama_host` 的机器上，读取失败静默回退会把 DELETE 改投默认 11434，可能
/// 删掉默认实例上的同名非商店模型（三十二轮评审 P2）。只读的探测/展示/标签
/// 消费方继续用宽松版。JoinError 按 Err 传播，不像宽松版按默认地址降级。
pub(crate) async fn ollama_base_checked() -> Result<String, String> {
    match tokio::task::spawn_blocking(crate::features::model_store::ollama_base_url_checked).await {
        Ok(inner) => inner,
        Err(error) => Err(format!("Ollama 守护进程地址解析失败（{error}）")),
    }
}

/// 短超时探测客户端（OnceLock 共享）。构建失败（TLS 后端初始化失败等持久性
/// 故障，极罕见）按 None **负缓存**：进程内不再重试构建，需重启恢复——Client
/// 构建失败不是暂态抖动，负缓存避免每次探测重付构建成本（注释曾承诺"失败降级
/// 为每次现建"但实现从未如此，第二十轮评审 P3 订正注释与实现对齐）。
/// 显式禁用代理：默认目标 127.0.0.1（ollama 还可能指向自定义 `ollama_host`，
/// 见 `ollama_base()`），reqwest 0.13 的 `system-proxy` feature 会吃
/// `http_proxy` 环境变量且对 loopback 无隐式豁免——宿主设了代理而 no_proxy
/// 没盖住目标时，服务探测会改走代理然后假报"连接被拒绝"（第三轮评审 P2；
/// 口径校准：自定义 host 在代理后可达与否不在本客户端的保证范围内，第六轮
/// 评审 P3）。
pub(crate) fn probe_client() -> Option<&'static reqwest::Client> {
    static CLIENT: OnceLock<Option<reqwest::Client>> = OnceLock::new();
    CLIENT
        .get_or_init(|| reqwest::Client::builder().no_proxy().build().ok())
        .as_ref()
}

/// 探测失败的可判别原因：HTTP 状态透传 vs 传输层失败。字符串化延迟到展示层——
/// 原先用中文文案前缀 `starts_with("服务响应异常")` 做程序判断，措辞一改，
/// "最具体 HTTP 状态"臂就会静默退化（五轮评审 P3）。
pub(crate) enum ProbeFailure {
    /// 服务有响应但状态码非 2xx。
    HttpStatus(u16),
    /// 连接失败/超时/客户端初始化失败（可判别原因，字符串化延迟到展示层）。
    Transport(TransportReason),
}

impl ProbeFailure {
    pub(crate) fn message(&self) -> String {
        match self {
            ProbeFailure::HttpStatus(status) => format!("服务响应异常（HTTP {status}）"),
            ProbeFailure::Transport(reason) => reason.message().to_string(),
        }
    }

    /// 稳定机器码（`ServiceStatus.reason` / `.code`），供前端三语映射。
    pub(crate) fn reason_code(&self) -> (&'static str, Option<i64>) {
        match self {
            ProbeFailure::HttpStatus(status) => ("http_status", Some(i64::from(*status))),
            ProbeFailure::Transport(reason) => (reason.code(), None),
        }
    }
}

/// 传输层失败的可判别原因。`message()` 是进 doctor 上下文/日志的中文人话，
/// `code()` 是前端 i18n 映射用的机器码——两者同源，措辞改动不会击穿判别。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum TransportReason {
    Timeout,
    ConnectionRefused,
    Network,
    ClientInit,
}

impl TransportReason {
    pub(crate) fn code(self) -> &'static str {
        match self {
            TransportReason::Timeout => "timeout",
            TransportReason::ConnectionRefused => "connection_refused",
            TransportReason::Network => "network_error",
            TransportReason::ClientInit => "client_init_failed",
        }
    }

    pub(crate) fn message(self) -> &'static str {
        match self {
            TransportReason::Timeout => "请求超时",
            TransportReason::ConnectionRefused => "连接被拒绝（服务可能未启动）",
            TransportReason::Network => "网络异常，无法访问服务",
            TransportReason::ClientInit => "HTTP 客户端初始化失败",
        }
    }
}

/// GET 一个 URL，2xx 即 Ok；非 2xx / 连接失败 / 超时 → 可判别的 [`ProbeFailure`]。
pub(crate) async fn http_get_success(url: &str) -> Result<(), ProbeFailure> {
    let client = probe_client().ok_or(ProbeFailure::Transport(TransportReason::ClientInit))?;
    let response = client
        .get(url)
        .timeout(PROBE_TIMEOUT)
        .send()
        .await
        .map_err(|error| ProbeFailure::Transport(probe_transport_reason(&error)))?;
    if response.status().is_success() {
        Ok(())
    } else {
        Err(ProbeFailure::HttpStatus(response.status().as_u16()))
    }
}

/// reqwest 错误 → 可判别原因（超时 / 拒绝连接 / 其他）。**不得**串化原始错误：
/// reqwest 的 Display 会追加 " for url (<完整 URL>)"，而 URL 来自用户可配置的
/// ollama_host（可能带 userinfo/内网主机名），detail 会进排查提示词上下文送往
/// （可能是云端的）模型——doctor 契约是"上下文刻意不含端点地址"（doctor.rs
/// TASK_SERVICES），第四轮评审 P2 堵住这条旁路。
pub(crate) fn probe_transport_reason(error: &reqwest::Error) -> TransportReason {
    if error.is_timeout() {
        TransportReason::Timeout
    } else if error.is_connect() {
        TransportReason::ConnectionRefused
    } else {
        TransportReason::Network
    }
}

/// reqwest 错误 → 人话原因（external_models 的 Err 字串口径）。
pub(crate) fn human_probe_error(error: reqwest::Error) -> String {
    probe_transport_reason(&error).message().to_string()
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

    #[test]
    fn fold_openai_port_probes_keeps_serial_era_semantics() {
        // 任一 200 → running（端口序无关，先到先判）。
        let ok = fold_openai_port_probes(
            "vllm",
            &[8000, 8001],
            vec![
                Err(ProbeFailure::Transport(TransportReason::Timeout)),
                Ok(()),
            ],
        );
        assert!(ok.running);
        assert_eq!(ok.detail, None);
        assert_eq!((ok.reason, ok.code), (None, None));
        // 多端口并发后仍按**端口序**取首个 HTTP 状态错误（旧串行首见语义）：
        // 8000 的 404 优先于 8001 的 503。
        let both_http = fold_openai_port_probes(
            "vllm",
            &[8000, 8001],
            vec![
                Err(ProbeFailure::HttpStatus(404)),
                Err(ProbeFailure::HttpStatus(503)),
            ],
        );
        assert!(!both_http.running);
        assert_eq!(
            both_http.detail.as_deref(),
            Some("服务响应异常（HTTP 404）")
        );
        // 机器码与数字随首个状态错误（前端三语 tooltip 据此映射，二十一轮评审 P2）。
        assert_eq!(
            (both_http.reason, both_http.code),
            (Some("http_status"), Some(404))
        );
        // 首端口传输层失败不吞掉后面端口的 HTTP 状态。
        let mixed = fold_openai_port_probes(
            "vllm",
            &[8000, 8001, 8002],
            vec![
                Err(ProbeFailure::Transport(TransportReason::ConnectionRefused)),
                Err(ProbeFailure::HttpStatus(403)),
                Err(ProbeFailure::Transport(TransportReason::Timeout)),
            ],
        );
        assert!(!mixed.running);
        assert_eq!(mixed.detail.as_deref(), Some("服务响应异常（HTTP 403）"));
        // 全为传输层失败 → 人话"未探测到"，端口清单随入参。
        let transports = fold_openai_port_probes(
            "vllm",
            &[8000, 8001, 8002],
            vec![
                Err(ProbeFailure::Transport(TransportReason::Network)),
                Err(ProbeFailure::Transport(TransportReason::Timeout)),
                Err(ProbeFailure::Transport(TransportReason::ConnectionRefused)),
            ],
        );
        assert!(!transports.running);
        assert_eq!(
            transports.detail.as_deref(),
            Some("未在端口 8000/8001/8002 探测到服务（连接被拒绝或超时）")
        );
        assert_eq!(
            (transports.reason, transports.code),
            (Some("not_detected"), None)
        );
    }

    #[test]
    fn probe_failure_reason_codes_are_stable_and_distinct() {
        // 机器码是前端 i18n 映射的契约面：每个传输层原因各有一码，与中文人话同源。
        let codes: Vec<&str> = [
            TransportReason::Timeout,
            TransportReason::ConnectionRefused,
            TransportReason::Network,
            TransportReason::ClientInit,
        ]
        .into_iter()
        .map(TransportReason::code)
        .collect();
        assert_eq!(
            codes,
            vec![
                "timeout",
                "connection_refused",
                "network_error",
                "client_init_failed"
            ]
        );
        assert_eq!(
            ProbeFailure::Transport(TransportReason::Timeout).reason_code(),
            ("timeout", None)
        );
        assert_eq!(
            ProbeFailure::HttpStatus(503).reason_code(),
            ("http_status", Some(503))
        );
        assert_eq!(
            ProbeFailure::Transport(TransportReason::ConnectionRefused).message(),
            "连接被拒绝（服务可能未启动）"
        );
    }

    #[test]
    fn component_peak_above_current_drops_sysinfo_fallback_and_stale_peaks() {
        // sysinfo 0.36 在芯片没有 tempN_highest 时把 max 回退为**当前读数**：
        // 该值当阈值用会让 ±10° 告警带恒 warn（第七轮评审 P2）——必须丢弃。
        assert_eq!(component_peak_above_current(45.0, Some(45.0)), None);
        assert_eq!(component_peak_above_current(45.0, Some(44.5)), None);
        // None（无 highest 也读不到当前值之外的分支）原样透传。
        assert_eq!(component_peak_above_current(45.0, None), None);
        // 严格高于当前读数的历史峰值保留（±10° 带的有效输入）。
        assert_eq!(component_peak_above_current(45.0, Some(72.0)), Some(72.0));
    }

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
        // 裸 br+数字（OVS/手工网桥惯例）；非数字后缀不命中。
        assert_eq!(classify_iface("br0", false), "bridge");
        assert_eq!(classify_iface("br10", false), "bridge");
        assert_eq!(classify_iface("brctl", false), "other");
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
    fn disk_filters_drop_squashfs_snap_and_container_overlay_mounts() {
        let make = |mount_point: &str, fs_type: &str| DiskSnapshot {
            name: None,
            mount_point: mount_point.to_string(),
            fs_type: fs_type.to_string(),
            kind: "unknown".to_string(),
            total_bytes: 1,
            available_bytes: 0,
            is_removable: false,
        };
        let squash = make("/snap/core22/1380", "squashfs");
        // 容器叠层（docker/containerd/podman）过滤：每容器一份、容量复制根 fs，
        // 跑一次模型容器就会刷出一排同名假磁盘（第四轮评审 P3）。
        let overlay = make("/var/lib/docker/overlay2/x/merged", "overlay");
        assert!(!keep_disk(&squash));
        assert!(!keep_disk(&overlay));
        // squashfs 类型即过滤，无论挂载点；非 squashfs/overlay 的普通挂载保留。
        assert!(!keep_disk(&make("/any/where", "squashfs")));
        assert!(keep_disk(&make("/", "ext4")));
        assert!(keep_disk(&make("/home", "btrfs")));
    }

    #[test]
    fn rate_math_is_delta_over_positive_elapsed_only() {
        // 方向钉死：增量 ÷ 秒。算反（秒 ÷ 增量、或增量当分母）页面上就是恒错数字。
        assert_eq!(rate_from_delta(1500, 2.0), 750.0);
        assert_eq!(rate_from_delta(0, 2.0), 0.0);
        // 非正间隔（时钟回拨/同拍重入）按 0，绝不出除零或负速率。
        assert_eq!(rate_from_delta(1500, 0.0), 0.0);
        assert_eq!(rate_from_delta(1500, -1.0), 0.0);
    }

    #[test]
    fn process_refresh_skips_within_sysinfo_cpu_update_window() {
        // 窗口内（<200ms）跳过重刷新：否则 /proc/stat 不重读而 per-process
        // 时间前进，CPU% 变 inf 封顶成核数×100（十二轮评审 P2）。
        let now = Instant::now();
        assert!(should_refresh_processes(None, now), "首拍必须刷新");
        assert!(
            !should_refresh_processes(Some(now), now),
            "同刻二次采样必须沿用上次进程表"
        );
        let just_under = now
            .checked_sub(sysinfo::MINIMUM_CPU_UPDATE_INTERVAL)
            .expect("clock permits subtracting 200ms")
            + std::time::Duration::from_millis(1);
        assert!(
            !should_refresh_processes(Some(just_under), now),
            "窗口边缘内侧仍须跳过"
        );
        let just_over = now
            .checked_sub(sysinfo::MINIMUM_CPU_UPDATE_INTERVAL)
            .expect("clock permits subtracting 200ms")
            - std::time::Duration::from_millis(1);
        assert!(
            should_refresh_processes(Some(just_over), now),
            "窗口外侧必须刷新"
        );
    }

    #[test]
    fn sensor_groups_sort_deterministically_by_chip_kind_name() {
        // sysinfo Components 的 HashMap 迭代序逐拍漂移；不排序会击穿 bridge 的
        // 显示等价抑制（第四轮评审 P2）。这里直打排序口径。
        let mut groups = vec![
            SensorGroup {
                chip: "nvme".to_string(),
                entries: vec![SensorEntry {
                    kind: "temperature".to_string(),
                    name: "Composite".to_string(),
                    value: 45.0,
                    critical: None,
                    max: None,
                }],
            },
            SensorGroup {
                chip: "acpitz".to_string(),
                entries: vec![
                    SensorEntry {
                        kind: "temperature".to_string(),
                        name: "thermal_zone".to_string(),
                        value: 79.4,
                        critical: None,
                        max: None,
                    },
                    SensorEntry {
                        kind: "fan".to_string(),
                        name: "fan1".to_string(),
                        value: 1200.0,
                        critical: None,
                        max: None,
                    },
                ],
            },
        ];
        sort_sensors(&mut groups);
        let chips: Vec<&str> = groups.iter().map(|group| group.chip.as_str()).collect();
        assert_eq!(chips, vec!["acpitz", "nvme"], "groups must sort by chip");
        let kinds: Vec<&str> = groups[0]
            .entries
            .iter()
            .map(|entry| entry.kind.as_str())
            .collect();
        assert_eq!(
            kinds,
            vec!["fan", "temperature"],
            "entries must sort by kind then name"
        );
    }

    #[test]
    fn gpu_ext_cached_returns_last_known_value_without_queueing_while_probe_in_flight() {
        // 并发口径（第五轮评审）的直打：探测锁被别人持有时，调用方立即拿到
        // `last` 的最后已知值，**既不排队也不采样**——采样闭包里 panic 作负控。
        let sampler = Mutex::new(GpuExtCache::default());
        let last = RwLock::new(Some(GpuExtSnapshot {
            driver_version: Some("580.02".to_string()),
            cuda_version: None,
            compute_processes: Some(Vec::new()),
        }));
        let held = sampler.lock();
        let value = gpu_ext_cached_with(&sampler, &last, || {
            panic!("must not sample while the probe lock is held")
        });
        assert_eq!(
            value
                .as_ref()
                .and_then(|value| value.driver_version.as_deref()),
            Some("580.02"),
            "contended call must return the last known value"
        );
        drop(held);
        // 锁释放后抢到锁的一路正常采样并发布到 last。
        let value = gpu_ext_cached_with(&sampler, &last, || {
            Some(GpuExtSnapshot {
                driver_version: Some("590.00".to_string()),
                cuda_version: None,
                compute_processes: None,
            })
        });
        assert_eq!(
            value
                .as_ref()
                .and_then(|value| value.driver_version.as_deref()),
            Some("590.00")
        );
        assert_eq!(
            last.read()
                .as_ref()
                .and_then(|value| value.driver_version.as_deref()),
            Some("590.00"),
            "a successful sample must be published as the new last-known value"
        );
    }

    #[test]
    fn gpu_ext_cache_keeps_old_value_on_failed_refresh_and_honors_ttl() {
        use std::cell::Cell;
        let make_value = |driver: &str| {
            Some(GpuExtSnapshot {
                driver_version: Some(driver.to_string()),
                cuda_version: None,
                compute_processes: Some(Vec::new()),
            })
        };
        let start = Instant::now();
        let mut cache = GpuExtCache::default();
        let calls = Cell::new(0usize);
        // 首拍成功落值。
        let first = refresh_gpu_ext_cache(&mut cache, start, || {
            calls.set(calls.get() + 1);
            (make_value("580.02"), start)
        });
        assert_eq!(
            first
                .as_ref()
                .and_then(|value| value.driver_version.as_deref()),
            Some("580.02")
        );
        assert_eq!(calls.get(), 1);
        // TTL 内：不触发采样，返回旧值。
        let cached = refresh_gpu_ext_cache(
            &mut cache,
            start + GPU_EXT_CACHE_TTL - Duration::from_millis(1),
            || {
                calls.set(calls.get() + 1);
                (make_value("999"), start)
            },
        );
        assert_eq!(
            cached
                .as_ref()
                .and_then(|value| value.driver_version.as_deref()),
            Some("580.02")
        );
        assert_eq!(calls.get(), 1, "TTL hit must not re-sample");
        // 过期 + 采样失败（瞬时超时/驱动忙）：保留旧值，只刷时间戳（第三轮 P3）。
        let kept = refresh_gpu_ext_cache(&mut cache, start + GPU_EXT_CACHE_TTL, || {
            calls.set(calls.get() + 1);
            (None, start + GPU_EXT_CACHE_TTL)
        });
        assert_eq!(
            kept.as_ref()
                .and_then(|value| value.driver_version.as_deref()),
            Some("580.02")
        );
        assert_eq!(calls.get(), 2);
        // 失败后的 TTL 窗口内不再重复打子进程。
        let _ = refresh_gpu_ext_cache(
            &mut cache,
            start + GPU_EXT_CACHE_TTL + Duration::from_millis(1),
            || {
                calls.set(calls.get() + 1);
                (None, start + GPU_EXT_CACHE_TTL)
            },
        );
        assert_eq!(calls.get(), 2, "kept-old refresh must still be TTL-cached");
        // 过期后采样恢复 → 换上新值。
        let recovered = refresh_gpu_ext_cache(&mut cache, start + 2 * GPU_EXT_CACHE_TTL, || {
            calls.set(calls.get() + 1);
            (make_value("590.01"), start + 2 * GPU_EXT_CACHE_TTL)
        });
        assert_eq!(
            recovered
                .as_ref()
                .and_then(|value| value.driver_version.as_deref()),
            Some("590.01")
        );
        // 首拍即失败：保持 None（“本机无 NVIDIA”降级）。
        let mut fresh = GpuExtCache::default();
        let none = refresh_gpu_ext_cache(&mut fresh, start, || (None, start));
        assert!(none.is_none());
    }

    #[test]
    fn gpu_ext_cache_ttl_is_anchored_at_sampling_completion_not_start() {
        // TTL 锚定（第二十轮评审 P2）：慢探测（完成时刻比进入时刻晚 ≥1s）不得把
        // TTL 窗口从内侧啃掉——旧实现把 sampled_at 记成进入时刻，探测耗时 ≥1s 时
        // 下一拍（2s 轮询）恒判过期、每拍重拉 nvidia-smi。
        let make_value = |driver: &str| {
            Some(GpuExtSnapshot {
                driver_version: Some(driver.to_string()),
                cuda_version: None,
                compute_processes: Some(Vec::new()),
            })
        };
        let start = Instant::now();
        let probe_took = Duration::from_millis(1500);
        let mut cache = GpuExtCache::default();
        let first = refresh_gpu_ext_cache(&mut cache, start, || {
            (make_value("580.02"), start + probe_took)
        });
        assert_eq!(
            first
                .as_ref()
                .and_then(|value| value.driver_version.as_deref()),
            Some("580.02")
        );
        // 下一拍观测窗必须落在「旧锚定已过期、完成锚定仍有效」的判别区间
        // (start+3s, start+4.5s)：观测早于 start+3s 时旧的进入时刻锚定同样
        // 未过期，测试区分不了两种锚定（二十三轮评审建议）。取 start+4s：
        // 完成时刻 start+1.5s，完成锚定余 2.5s 有效——正确实现返回缓存值，
        // 旧锚定此时已过期、会触发采样函数 panic。
        let cached = refresh_gpu_ext_cache(&mut cache, start + Duration::from_secs(4), || {
            panic!("TTL anchored at completion must not re-sample within the window")
        });
        assert_eq!(
            cached
                .as_ref()
                .and_then(|value| value.driver_version.as_deref()),
            Some("580.02")
        );
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
        // CSV 无引号转义：名字含逗号时中间整段归名字、末段仍是内存（第三轮 P3
        // 修复所依赖的路径，此前恰好没测）。
        let info = parse_compute_app_line("1234, /usr/bin/my,proc, 4096").expect("comma name");
        assert_eq!(info.pid, 1234);
        assert_eq!(info.name, "/usr/bin/my,proc");
        assert_eq!(info.used_mib, 4096);
        // nvidia-smi 对含逗号字段的标准 CSV 引号包裹形态：剥引号后名字不带引号
        // （此前剥引号分支零覆盖，删掉 trim_matches 全绿——二十一轮评审变异实证）。
        let info = parse_compute_app_line("1234, \"/usr/bin/my,proc\", 4096").expect("quoted name");
        assert_eq!(info.name, "/usr/bin/my,proc");
        assert!(parse_compute_app_line("not,a,line").is_none());
        assert!(parse_compute_app_line("").is_none());
    }

    #[test]
    fn fold_compute_apps_output_keeps_failed_query_distinct_from_no_processes() {
        // “查询失败 ≠ 无进程”契约（十六轮评审）：命令失败（None）必须原样
        // 传递，不得折成空列表冒充"没在跑任何计算进程"。
        assert!(fold_compute_apps_output(None).is_none());
        // 成功且零进程：空输出（甚至只有换行）→ Some(空列表)。
        let empty = fold_compute_apps_output(Some("")).expect("成功输出不得是 None");
        assert!(empty.is_empty(), "空输出 = 查询成功且零进程，必须是空列表");
        // 正常输出：解析 + 坏行丢弃 + pid 排序。
        let procs = fold_compute_apps_output(Some(
            "9, /usr/bin/sortd, 8\n1234, /usr/bin/python3, 4096\ngarbage\n",
        ))
        .expect("some output must parse");
        let pids: Vec<u32> = procs.iter().map(|proc| proc.pid).collect();
        assert_eq!(pids, vec![9, 1234]);
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
    fn parse_nvidia_smi_header_driver_version() {
        // driver_version 是 gpu_ext 块的存在门（None → 整块 None），必须与 cuda
        // 版本同等级钉死（第四轮评审 P3：此前只有 CUDA 一侧有测试）。
        let header = "| NVIDIA-SMI 540.4.0    Driver Version: 540.4.0    CUDA Version: 12.3 |\n";
        assert_eq!(
            parse_nvidia_smi_driver_version(header),
            Some("540.4.0".to_string())
        );
        // 竖线紧贴版本号（表头列收边）：trim_end_matches('|') 分支。
        let tight = "Driver Version: 550.54.14|   CUDA Version: N/A";
        assert_eq!(
            parse_nvidia_smi_driver_version(tight),
            Some("550.54.14".to_string())
        );
        assert_eq!(parse_nvidia_smi_driver_version("no header"), None);
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
        // 全量填充的构造快照（非字符串字面量自证）：前端按这些键取数，serde
        // rename 掉任一**内层**字段时，Rust 测试与两套 JS 套件可能照样全绿而
        // 出厂应用把 undefined 渲染出来（第九轮评审 P1）——每类元素的内层键集
        // 在这里钉死。
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
            disks: vec![DiskSnapshot {
                name: Some("nvme0n1".to_string()),
                mount_point: "/".to_string(),
                fs_type: "ext4".to_string(),
                kind: "ssd".to_string(),
                total_bytes: 1000,
                available_bytes: 500,
                is_removable: false,
            }],
            network: vec![NetIfaceSnapshot {
                name: "eth0".to_string(),
                iface_type: "ethernet".to_string(),
                is_up: true,
                mac: Some("aa:bb:cc:dd:ee:ff".to_string()),
                rx_bytes: 10,
                tx_bytes: 20,
                rx_bytes_per_sec: 1.5,
                tx_bytes_per_sec: 2.5,
            }],
            processes: ProcessSummary {
                top_cpu: vec![ProcInfo {
                    pid: 7,
                    name: "cpu-hog".to_string(),
                    cpu_pct: 90.5,
                    memory_bytes: 1024,
                }],
                top_memory: vec![ProcInfo {
                    pid: 9,
                    name: "mem-hog".to_string(),
                    cpu_pct: 0.5,
                    memory_bytes: 8192,
                }],
            },
            gpu_ext: Some(GpuExtSnapshot {
                driver_version: Some("580.02".to_string()),
                cuda_version: Some("12.3".to_string()),
                compute_processes: Some(vec![GpuProcInfo {
                    pid: 7,
                    name: "python3".to_string(),
                    used_mib: 64,
                }]),
            }),
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
        assert_eq!(value["generated_at_ms"], 1);
        // os 子键。
        for key in ["product_name", "pretty_name", "kernel", "arch"] {
            assert!(value["os"].get(key).is_some(), "missing os key: {key}");
        }
        assert_eq!(value["os"]["product_name"], "LinSeer MegaCube");
        assert_eq!(value["os"]["kernel"], "6.0.0");
        // sensors：组键 + 条目内层键。
        for key in ["chip", "entries"] {
            assert!(
                value["sensors"][0].get(key).is_some(),
                "missing sensor group key: {key}"
            );
        }
        assert_eq!(value["sensors"][0]["chip"], "coretemp");
        for key in ["kind", "name", "value", "critical", "max"] {
            assert!(
                value["sensors"][0]["entries"][0].get(key).is_some(),
                "missing sensor entry key: {key}"
            );
        }
        assert_eq!(value["sensors"][0]["entries"][0]["critical"], 100.0);
        // disks。
        for key in [
            "name",
            "mount_point",
            "fs_type",
            "kind",
            "total_bytes",
            "available_bytes",
            "is_removable",
        ] {
            assert!(
                value["disks"][0].get(key).is_some(),
                "missing disk key: {key}"
            );
        }
        assert_eq!(value["disks"][0]["mount_point"], "/");
        // network。
        for key in [
            "name",
            "iface_type",
            "is_up",
            "mac",
            "rx_bytes",
            "tx_bytes",
            "rx_bytes_per_sec",
            "tx_bytes_per_sec",
        ] {
            assert!(
                value["network"][0].get(key).is_some(),
                "missing network key: {key}"
            );
        }
        assert_eq!(value["network"][0]["rx_bytes_per_sec"], 1.5);
        // processes：top_cpu / top_memory 同一 ProcInfo 键集。
        for list in ["top_cpu", "top_memory"] {
            assert!(value["processes"].get(list).is_some(), "missing: {list}");
            for key in ["pid", "name", "cpu_pct", "memory_bytes"] {
                assert!(
                    value["processes"][list][0].get(key).is_some(),
                    "missing process key in {list}: {key}"
                );
            }
        }
        assert_eq!(value["processes"]["top_memory"][0]["memory_bytes"], 8192);
        // gpu_ext：版本字段 + 计算进程内层键。
        for key in ["driver_version", "cuda_version", "compute_processes"] {
            assert!(
                value["gpu_ext"].get(key).is_some(),
                "missing gpu_ext key: {key}"
            );
        }
        assert_eq!(value["gpu_ext"]["driver_version"], "580.02");
        for key in ["pid", "name", "used_mib"] {
            assert!(
                value["gpu_ext"]["compute_processes"][0].get(key).is_some(),
                "missing gpu process key: {key}"
            );
        }
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

    /// 表头 "N/A"（驱动不支持对应查询）按字段缺失处理，不把字面 "N/A" 当版本号。
    #[test]
    fn nvidia_smi_header_na_is_treated_as_missing() {
        let header = "CUDA Version: N/A | Driver Version: 580.02 |";
        assert_eq!(parse_nvidia_smi_cuda_version(header), None);
        assert_eq!(
            parse_nvidia_smi_driver_version(header),
            Some("580.02".to_string())
        );
        // 紧贴收边竖线的 N/A 同样不吃。
        assert_eq!(parse_nvidia_smi_cuda_version("CUDA Version: N/A|"), None);
    }
}
