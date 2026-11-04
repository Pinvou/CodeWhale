//! 设备管理（MegaCube）的 Tauri 命令层（设计 §2.3）。
//!
//! 与 `model_store.rs` 同一纪律：这层**只做搬运**——取 managed state、转发参数、
//! 把 `features::device` 的结果原样回给前端。唯一多出来的一件事是
//! `device_doctor_prompt` 的上下文组装：把设备快照 + 运行状态 + 服务探测拼成
//! 一份 JSON，交给提示词模板注入。
//!
//! 依赖方向合规：`app/commands → features::device / features::monitor`，features
//! 内部零 `#[tauri::command]`、零 `crate::app::*` 引用（架构守卫）。

use serde_json::{Value, json};
use tauri::State;

use crate::features::device::{
    self, DeviceSnapshot, DeviceState, ExternalModel, ExternalModelsView, RemoveOutcome,
    ServiceStatus,
};
use crate::features::monitor::{CpuSnapshot, GpuSnapshot, MonitorState, RamSnapshot};

/// 设备命令族的能力门禁（第二十轮评审 P2）：`megacubeDevice` 能力位此前只是
/// 前端入口开关，渲染层（含被攻破的网页层）可以直接 invoke 任意命令——读面是
/// 全量设备遥测，破坏面是"家目录深度 ≥3 目录删除 + daemon DELETE"。命令层
/// 显式拒非 MegaCube 平台，与 `desktop_remote_control` 的 `ensure_*_enabled`
/// fail-closed 先例同口径；开发/自动化经 `PINVOU3_MEGACUBE_DEVICE=1` 逃生
/// （ OnceLock 进程缓存，2s 轮询零开销）。
fn ensure_device_supported() -> Result<(), String> {
    if crate::platform::capabilities::is_megacube_device() {
        Ok(())
    } else {
        Err("设备管理仅支持 MegaCube 一体机".to_string())
    }
}

/// 设备管理页 2s 轮询的完整快照。
#[tauri::command]
pub async fn get_device_snapshot(device: State<'_, DeviceState>) -> Result<DeviceSnapshot, String> {
    ensure_device_supported()?;
    Ok(device::sample_snapshot(&device).await)
}

/// 模型服务可达性（慢轮询 10s / 手动刷新；docker 探测带 3s 子进程预算）。
#[tauri::command]
pub async fn get_device_services() -> Result<Vec<ServiceStatus>, String> {
    ensure_device_supported()?;
    Ok(device::probe_services().await)
}

/// 本机全部外部模型（注册表 + Ollama 在线列表；坏注册表文件跳过进 warnings）。
#[tauri::command]
pub async fn get_external_models() -> Result<ExternalModelsView, String> {
    ensure_device_supported()?;
    device::get_external_models().await
}

/// 登记外部模型（写注册表；商店管理的名字会被拒绝）。
#[tauri::command]
pub async fn device_register_external_model(
    name: String,
    kind: String,
    base_url: Option<String>,
    model: Option<String>,
    weights_path: Option<String>,
) -> Result<ExternalModel, String> {
    ensure_device_supported()?;
    device::register_external_model(name, kind, base_url, model, weights_path).await
}

/// 移除外部模型（守卫链：确认名 → 商店边界 → 守护进程绑定 → 权重删除守卫 →
/// Ollama 删除 → 删记录）。`expected_ollama_base` 是确认面板对准的守护进程
/// 基地址（列表行自带的那一次解析），ollama 记录在删除前绑定校验（2026-11-04
/// 评审 P3）；非 ollama 记录传 null。
#[tauri::command]
pub async fn device_remove_external_model(
    id: String,
    delete_weights: bool,
    confirm_name: String,
    expected_ollama_base: Option<String>,
) -> Result<RemoveOutcome, String> {
    ensure_device_supported()?;
    device::remove_external_model(id, delete_weights, confirm_name, expected_ollama_base).await
}

/// 组装 AI 故障排查提示词（设计 §5）。上下文由这里实时采样拼装，保证模型拿到
/// 真实设备数据而不是瞎猜；模板与校验都在 `features::device::doctor`。
#[tauri::command]
pub async fn device_doctor_prompt(
    kind: String,
    question: Option<String>,
    device: State<'_, DeviceState>,
    monitor: State<'_, MonitorState>,
) -> Result<String, String> {
    ensure_device_supported()?;
    let parsed_kind = device::DoctorKind::parse(&kind)?;
    let context = build_doctor_context(&device, &monitor).await;
    device::build_doctor_prompt(parsed_kind, question.as_deref(), &context)
}

/// 故障排查上下文（设计 §5 第 4 节）：设备快照（传感器/磁盘/网络/进程/GPU 扩展）
/// + 运行状态（CPU/GPU/内存）+ 服务可达性 + 当前活动模型健康。键名英文 snake_case。
///
/// **逐字段白名单**（十四轮评审）：上下文随提示词送往（可能是云端的）模型后端，
/// 每一节都按字段显式挑明——快照结构体未来新增字段不会自动进上下文，必须在此
/// 显式加入才外发。安全要点"白名单字段，不含密钥"由此落地（此前是"全结构体
/// −事后脱敏"，新增字段默认外泄）。MAC 地址在白名单外（结构性剔除；脱敏层
/// 的剔除臂保留兜底）。
async fn build_doctor_context(device: &DeviceState, monitor: &MonitorState) -> Value {
    // 三路来源互不依赖，并发采样。串行时 vLLM 三端口逐个 1.5s 超时 + docker 3s，
    // 最坏 8-12s 用户点排查卡才开出会话（评审 P2）；并发后总耗时 ≈ 最慢一路
    // （vLLM 端口并发后为单端口 1.5s，十四轮评审）。上面 8-12s 不含 nvidia-smi
    // 挂死的最坏路径：GPU 扩展最坏 2 次调用 × 3 候选 × 10s ≈ 60s，且
    // sample_snapshot 与 GPU 扩展同一 join，nvidia-smi 挂死时排查卡等待预算
    // 一并拉长（设备页注释已声明为后续架构项，十九轮评审补记）。
    let (device_snapshot, monitor_snapshot, services) = tokio::join!(
        device::sample_snapshot(device),
        crate::features::monitor::sample_all(monitor),
        device::probe_services(),
    );
    let active_model = monitor_snapshot.vllm.as_ref().map(active_model_projection);
    finish_doctor_context(
        &device_snapshot,
        monitor_snapshot.gpu.as_ref(),
        monitor_snapshot.cpu.as_ref(),
        monitor_snapshot.ram.as_ref(),
        &services,
        active_model,
        &crate::platform::paths::user_home_dir().to_string_lossy(),
    )
}

/// 拼装 + 脱敏的组合（纯函数，测试直打）。两个零件各有直打测试，但"拼装之后
/// 必须脱敏"这条接线此前零覆盖——删掉脱敏调用全套测试照绿，MAC/家目录/用户名
/// 会静默外发云端模型（二十一轮评审 P2 变异实证）。命令路径只许经由本函数。
fn finish_doctor_context(
    device: &DeviceSnapshot,
    gpu: Option<&GpuSnapshot>,
    cpu: Option<&CpuSnapshot>,
    ram: Option<&RamSnapshot>,
    services: &[ServiceStatus],
    active_model: Option<Value>,
    home: &str,
) -> Value {
    let mut context = assemble_doctor_context(device, gpu, cpu, ram, services, active_model);
    mask_context_privacy(&mut context, home);
    context
}

/// `active_model` 的字段投影（纯函数，测试直打）：TASK_SERVICES 模板按
/// `active_model.health_status` 等键取数，投影键名笔误会让模型拿到 null 并如实
/// 回答"上下文里没有"——顶层键有拼装契约测试盯着，这一层内联 json! 时漏了同一刀
/// （五轮评审 P3），抽出后键集可直接断言。
fn active_model_projection(model: &crate::features::monitor::VllmSnapshot) -> Value {
    json!({
        "model_id": model.model_id,
        "provider": model.provider,
        "model": model.model,
        "configured_model": model.configured_model,
        "target_kind": model.target_kind,
        "health_status": model.health_status,
        "max_model_len": model.max_model_len,
    })
}

/// 上下文拼装（纯函数，测试直打键名契约）：提示词模板与前端都按这些键取数，
/// 拼装与采样解耦后键名漂移才有测试可拦（第三轮评审：原"契约测试"自造字面量
/// 自证，测不到真实函数）。入参收**类型化快照**而非序列化后的 Value——白名单
/// 由编译期形状保证，结构体加字段时这里不挑明就不外发（十四轮评审）。
fn assemble_doctor_context(
    device: &DeviceSnapshot,
    gpu: Option<&GpuSnapshot>,
    cpu: Option<&CpuSnapshot>,
    ram: Option<&RamSnapshot>,
    services: &[ServiceStatus],
    active_model: Option<Value>,
) -> Value {
    json!({
        "device": doctor_device_projection(device),
        "gpu": gpu.map(doctor_gpu_projection),
        "cpu": cpu.map(doctor_cpu_projection),
        "ram": ram.map(doctor_ram_projection),
        "services": services.iter().map(doctor_service_projection).collect::<Vec<_>>(),
        "active_model": active_model,
    })
}

/// 设备快照投影（白名单逐字段）：传感器/磁盘/网络/进程/GPU 扩展全量条目都在
/// 诊断面内，但**逐字段挑明**——网络条目不含 `mac`（稳定硬件指纹，原脱敏层
/// 剔除臂保留兜底），挂载点/进程路径等文本仍靠脱敏层改写家目录前缀。
fn doctor_device_projection(device: &DeviceSnapshot) -> Value {
    json!({
        "generated_at_ms": device.generated_at_ms,
        "os": {
            "product_name": device.os.product_name,
            "pretty_name": device.os.pretty_name,
            "kernel": device.os.kernel,
            "arch": device.os.arch,
        },
        "sensors": device
            .sensors
            .iter()
            .map(|group| {
                json!({
                    "chip": group.chip,
                    "entries": group
                        .entries
                        .iter()
                        .map(|entry| {
                            json!({
                                "kind": entry.kind,
                                "name": entry.name,
                                "value": entry.value,
                                "critical": entry.critical,
                                "max": entry.max,
                            })
                        })
                        .collect::<Vec<_>>(),
                })
            })
            .collect::<Vec<_>>(),
        "disks": device
            .disks
            .iter()
            .map(|disk| {
                json!({
                    "name": disk.name,
                    "mount_point": disk.mount_point,
                    "fs_type": disk.fs_type,
                    "kind": disk.kind,
                    "total_bytes": disk.total_bytes,
                    "available_bytes": disk.available_bytes,
                    "is_removable": disk.is_removable,
                })
            })
            .collect::<Vec<_>>(),
        "network": device
            .network
            .iter()
            .map(|iface| {
                json!({
                    "name": iface.name,
                    "iface_type": iface.iface_type,
                    "is_up": iface.is_up,
                    "rx_bytes": iface.rx_bytes,
                    "tx_bytes": iface.tx_bytes,
                    "rx_bytes_per_sec": iface.rx_bytes_per_sec,
                    "tx_bytes_per_sec": iface.tx_bytes_per_sec,
                })
            })
            .collect::<Vec<_>>(),
        "processes": {
            "top_cpu": device
                .processes
                .top_cpu
                .iter()
                .map(doctor_proc_projection)
                .collect::<Vec<_>>(),
            "top_memory": device
                .processes
                .top_memory
                .iter()
                .map(doctor_proc_projection)
                .collect::<Vec<_>>(),
        },
        "gpu_ext": device.gpu_ext.as_ref().map(|ext| {
            json!({
                "driver_version": ext.driver_version,
                "cuda_version": ext.cuda_version,
                // None（查询失败）序列化为 null：排查上下文里 null = 不可知，
                // 模型不应读成“没有计算进程”（十六轮评审）；Some(空) = 真的零进程。
                // GpuProcInfo 自身的序列化键集恰为 pid/name/used_mib，直出即可。
                "compute_processes": ext.compute_processes,
            })
        }),
    })
}

fn doctor_proc_projection(proc: &crate::features::device::ProcInfo) -> Value {
    json!({
        "pid": proc.pid,
        "name": proc.name,
        "cpu_pct": proc.cpu_pct,
        "memory_bytes": proc.memory_bytes,
    })
}

fn doctor_gpu_projection(gpu: &GpuSnapshot) -> Value {
    // 统一内存架构（GB10——本产品的目标硬件）上 nvidia-smi 对显存/利用率返回
    // [N/A]，monitor 解析层降级为 0（UI 以 vram_total_mib==0 切换显示形态）。
    // doctor 上下文的契约是"null=不可知，不得读成零"（与 compute_processes 同一
    // 教义）：否则目标硬件上每一份排查上下文都是"显存 0MiB / 利用率 0%"，模型
    // 会据此拒绝推荐本地模型或误诊驱动/显存故障（第二十轮评审 P2）。真实读数
    // 原样透传；unified_memory_na 显式告知降级原因。
    let unified_memory_na = gpu.vram_total_mib == 0;
    let (vram_used, vram_total, utilization) = if unified_memory_na {
        (None, None, None)
    } else {
        (
            Some(gpu.vram_used_mib),
            Some(gpu.vram_total_mib),
            Some(gpu.utilization_pct),
        )
    };
    json!({
        "name": gpu.name,
        "vram_used_mib": vram_used,
        "vram_total_mib": vram_total,
        "utilization_pct": utilization,
        "processor_utilization_pct": gpu.processor_utilization_pct,
        "shared_memory_used_mib": gpu.shared_memory_used_mib,
        "temperature_c": gpu.temperature_c,
        "power_w": gpu.power_w,
        "unified_memory_na": unified_memory_na,
    })
}

fn doctor_cpu_projection(cpu: &CpuSnapshot) -> Value {
    json!({
        "name": cpu.name,
        "total_usage_pct": cpu.total_usage_pct,
    })
}

fn doctor_ram_projection(ram: &RamSnapshot) -> Value {
    json!({
        "total_kib": ram.total_kib,
        "used_kib": ram.used_kib,
        "swap_total_kib": ram.swap_total_kib,
        "swap_used_kib": ram.swap_used_kib,
    })
}

fn doctor_service_projection(service: &ServiceStatus) -> Value {
    json!({
        "key": service.key,
        "running": service.running,
        "detail": service.detail,
    })
}

/// 隐私收敛：上下文随提示词送往（可能是云端的）模型后端。
/// ① 网卡 MAC 是稳定硬件指纹，整字段剔除；
/// ② 字符串值里的家目录前缀改写为 `~`（带路径分量边界，`/home/u2` 不吃
///    `/home/u` 的前缀）、`/media/<用户名>/` 自动挂载段的用户名改写为 `~`——
///    GPU 计算进程名是 nvidia-smi 上报的完整可执行路径，常含家目录，是"家目录
///    脱敏"声明最容易被漏掉的旁路（第三轮评审 P2）；
/// ③ 反引号统一替换为单引号：上下文嵌在 ```json 围栏里，值内的反引号序列会让
///    围栏在视觉上提前闭合，把剩余 JSON 误读为模板正文。
/// 剩余已知取舍：进程名/芯片标签等环境可控文本仍在（prctl 即可伪造，"需系统级
/// 权限才可伪造"的旧论断不成立），注入面由 doctor.rs 上下文头的"不可信输入"
/// 注记防守；本函数只收敛隐私与围栏破坏两个确定面。
fn mask_context_privacy(context: &mut Value, home: &str) {
    if let Some(ifaces) = context
        .get_mut("device")
        .and_then(|device| device.get_mut("network"))
        .and_then(Value::as_array_mut)
    {
        for iface in ifaces {
            if let Some(object) = iface.as_object_mut() {
                object.remove("mac");
            }
        }
    }
    let username = std::path::Path::new(home)
        .file_name()
        .map(|name| name.to_string_lossy().to_string())
        .unwrap_or_default();
    sanitize_context_strings(context, home, &username);
}

/// 递归改写所有字符串值：家目录前缀与 `/media/<用户名>/` 段脱敏 + 反引号清洗。
fn sanitize_context_strings(value: &mut Value, home: &str, username: &str) {
    match value {
        Value::String(text) => {
            let mut masked = mask_path_text(text, home, username);
            if masked.contains('`') {
                masked = masked.replace('`', "'");
            }
            *value = Value::String(masked);
        }
        Value::Array(items) => {
            for item in items {
                sanitize_context_strings(item, home, username);
            }
        }
        Value::Object(map) => {
            for item in map.values_mut() {
                sanitize_context_strings(item, home, username);
            }
        }
        _ => {}
    }
}

/// 路径脱敏（纯函数）：家目录前缀（要求分量边界：恰好相等或后随 `/`，避免
/// `/home/u2` 吃到 `/home/u` 的前缀）→ `~`；`/media/<用户名>/`（Ubuntu 等对
/// 可移动介质的自动挂载点，卷标还是插盘人可控的自由文本）→ `/media/~/`。
///
/// 前缀全部按 `/` 分隔符书写：目标平台是 Linux（MegaCube）；macOS 走
/// `PINVOU3_MEGACUBE_DEVICE=1` 强开能力位的开发路径时，Windows 风格反斜杠
/// 路径不会被脱敏——那是仅在开发机上有意义的组合，不追求覆盖（第七轮评审
/// P3 记录该假设）。
fn mask_path_text(text: &str, home: &str, username: &str) -> String {
    // HOME 被设为**空串**（未设时平台层有 /tmp 兜底，不在此列）时，下面的
    // `"/"` 前缀臂会把一切绝对路径改写成 `~/…`，脱敏反而毁掉全部路径信息——
    // 家目录未知时跳过前缀脱敏，好过全面误伤（第二十轮评审 P3）。
    if home.is_empty() {
        return text.to_string();
    }
    if text == home {
        return "~".to_string();
    }
    let home_slash = format!("{home}/");
    if let Some(rest) = text.strip_prefix(&home_slash) {
        return format!("~/{rest}");
    }
    if !username.is_empty() {
        // 两个惯例自动挂载位：/media/<user>（Ubuntu 桌面）与 /run/media/<user>
        // （systemd/udisks2，Fedora 系与多数现代发行版），卷标都是插盘人可控的
        // 自由文本，用户名一并脱敏（第四轮评审 P3 补 /run 臂）。
        for media_root in ["/media", "/run/media"] {
            let media_prefix = format!("{media_root}/{username}/");
            if let Some(rest) = text.strip_prefix(&media_prefix) {
                return format!("{media_root}/~/{rest}");
            }
        }
    }
    text.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 全字段填充的快照夹具：白名单测试用它钉"哪些字段获准进上下文"。
    fn fixture_snapshot() -> DeviceSnapshot {
        DeviceSnapshot {
            generated_at_ms: 1,
            os: crate::features::device::OsInfo {
                product_name: Some("Ubuntu".into()),
                pretty_name: Some("Ubuntu 24.04".into()),
                kernel: "6.11".into(),
                arch: "aarch64".into(),
            },
            sensors: vec![crate::features::device::SensorGroup {
                chip: "coretemp".into(),
                entries: vec![crate::features::device::SensorEntry {
                    kind: "temperature".into(),
                    name: "temp1".into(),
                    value: 45.0,
                    critical: Some(100.0),
                    max: Some(46.0),
                }],
            }],
            disks: vec![crate::features::device::DiskSnapshot {
                name: Some("nvme0n1".into()),
                mount_point: "/home/u/data".into(),
                fs_type: "ext4".into(),
                kind: "ssd".into(),
                total_bytes: 1,
                available_bytes: 2,
                is_removable: false,
            }],
            network: vec![crate::features::device::NetIfaceSnapshot {
                name: "eth0".into(),
                iface_type: "ethernet".into(),
                is_up: true,
                mac: Some("aa:bb:cc:dd:ee:ff".into()),
                rx_bytes: 3,
                tx_bytes: 4,
                rx_bytes_per_sec: 5.0,
                tx_bytes_per_sec: 6.0,
            }],
            processes: crate::features::device::ProcessSummary {
                top_cpu: vec![crate::features::device::ProcInfo {
                    pid: 7,
                    name: "ollama".into(),
                    cpu_pct: 12.5,
                    memory_bytes: 8,
                }],
                top_memory: Vec::new(),
            },
            gpu_ext: Some(crate::features::device::GpuExtSnapshot {
                driver_version: Some("570.1".into()),
                cuda_version: Some("13.0".into()),
                compute_processes: Some(vec![crate::features::device::GpuProcInfo {
                    pid: 9,
                    name: "/home/u/bin/python3".into(),
                    used_mib: 64,
                }]),
            }),
        }
    }

    #[test]
    fn doctor_context_assembles_the_documented_contract_keys() {
        // 键名契约直打真实拼装函数（设计 §5 第 4 节）：前端与提示词模板都按这些
        // 键取数。此前这里用 json! 自造字面量自证，测不到真实函数。
        let gpu = crate::features::monitor::GpuSnapshot {
            name: "GPU".into(),
            vram_used_mib: 1,
            vram_total_mib: 2,
            utilization_pct: 3,
            processor_utilization_pct: None,
            shared_memory_used_mib: None,
            temperature_c: Some(45),
            power_w: Some(6.5),
        };
        let cpu = crate::features::monitor::CpuSnapshot {
            name: "CPU".into(),
            total_usage_pct: Some(11.0),
        };
        let ram = crate::features::monitor::RamSnapshot {
            total_kib: 10,
            used_kib: 4,
            swap_total_kib: 2,
            swap_used_kib: 0,
        };
        let context = assemble_doctor_context(
            &fixture_snapshot(),
            Some(&gpu),
            Some(&cpu),
            Some(&ram),
            &[ServiceStatus {
                key: "ollama".into(),
                running: false,
                detail: Some("连接被拒绝".into()),
                reason: Some("connection_refused"),
                code: None,
            }],
            Some(json!({"model_id": "m", "health_status": "healthy"})),
        );
        for key in ["device", "gpu", "cpu", "ram", "services", "active_model"] {
            assert!(context.get(key).is_some(), "missing context key: {key}");
        }
        assert_eq!(context["device"]["os"]["arch"], "aarch64");
        assert_eq!(context["gpu"]["vram_total_mib"], 2);
        assert_eq!(context["cpu"]["total_usage_pct"], 11.0);
        assert_eq!(context["ram"]["swap_used_kib"], 0);
        assert_eq!(context["services"][0]["key"], "ollama");
    }

    #[test]
    fn doctor_context_is_an_explicit_field_whitelist() {
        // 白名单钉面（十四轮评审）：上下文送往可能是云端的模型后端，每一节只
        // 放显式挑明的字段——结构体未来加字段时本测试不改就照绿（新字段不外
        // 泄），挑进白名单则必须在这里同步钉面。网络条目不得携带 mac（结构性
        // 剔除，脱敏层剔除臂保留兜底）。键序按 BTreeMap 字典序断言。
        fn keys_sorted(value: &serde_json::Value) -> Vec<String> {
            let mut keys: Vec<String> = value
                .as_object()
                .expect("expected a JSON object")
                .keys()
                .cloned()
                .collect();
            keys.sort();
            keys
        }
        let expected = |items: &[&str]| -> Vec<String> {
            let mut keys: Vec<String> = items.iter().map(|key| key.to_string()).collect();
            keys.sort();
            keys
        };
        let context = assemble_doctor_context(&fixture_snapshot(), None, None, None, &[], None);
        let device = &context["device"];
        assert_eq!(
            keys_sorted(&device["network"][0]),
            expected(&[
                "name",
                "iface_type",
                "is_up",
                "rx_bytes",
                "tx_bytes",
                "rx_bytes_per_sec",
                "tx_bytes_per_sec",
            ]),
            "network whitelist drifted"
        );
        assert!(device["network"][0].get("mac").is_none());
        assert_eq!(
            keys_sorted(&device["os"]),
            expected(&["product_name", "pretty_name", "kernel", "arch"])
        );
        assert_eq!(
            keys_sorted(&device["disks"][0]),
            expected(&[
                "name",
                "mount_point",
                "fs_type",
                "kind",
                "total_bytes",
                "available_bytes",
                "is_removable",
            ])
        );
        assert_eq!(
            keys_sorted(&device["gpu_ext"]),
            expected(&["driver_version", "cuda_version", "compute_processes"])
        );
        assert_eq!(
            keys_sorted(&device["gpu_ext"]["compute_processes"][0]),
            expected(&["pid", "name", "used_mib"])
        );

        // 计算进程查询失败（None）→ null（不可知），不折成 [] 冒充“零进程”
        // （十六轮评审：部分失败曾与“无进程”不可区分）。
        let mut no_gpu_procs = fixture_snapshot();
        if let Some(gpu_ext) = no_gpu_procs.gpu_ext.as_mut() {
            gpu_ext.compute_processes = None;
        }
        let degraded = assemble_doctor_context(&no_gpu_procs, None, None, None, &[], None);
        assert_eq!(
            degraded["device"]["gpu_ext"]["compute_processes"],
            Value::Null,
            "查询失败必须序列化为 null，不得读成“无计算进程”。",
        );
        assert_eq!(
            keys_sorted(&device["processes"]["top_cpu"][0]),
            expected(&["pid", "name", "cpu_pct", "memory_bytes"])
        );
        // 采样缺 GPU/CPU/RAM 时保持 null（与"全结构体直出"时代的 null 同形，
        // 前端与模板都按缺省解读）。
        assert!(context["gpu"].is_null());
        assert!(context["cpu"].is_null());
        assert!(context["ram"].is_null());
        assert!(context["active_model"].is_null());
    }

    #[test]
    fn active_model_projection_exposes_the_documented_keys() {
        // TASK_SERVICES 模板按 active_model.health_status 等键取数；投影键名
        // 漂移只会让模型拿到 null 并如实回答"上下文里没有"——键集钉在这里
        // （五轮评审 P3：内层投影是顶层键契约测试漏掉的同一刀）。
        let snapshot = crate::features::monitor::VllmSnapshot {
            status: crate::features::monitor::VllmStatus::Ready,
            model_id: Some("preset-1".to_string()),
            provider: "vllm".to_string(),
            model: Some("qwen3".to_string()),
            configured_model: Some("qwen3".to_string()),
            target_kind: "local".to_string(),
            metrics_applicable: true,
            health_status: crate::features::monitor::HealthStatus::Verified,
            diagnostic: None,
            metric_diagnostics: Vec::new(),
            max_model_len: Some(32768),
            num_requests_running: None,
            num_requests_waiting: None,
            prefix_cache_hit_pct: None,
            prefix_cache_hits: None,
            prefix_cache_queries: None,
            ttft_sum_s: None,
            ttft_count: None,
            tpot_sum_s: None,
            tpot_count: None,
            generation_tokens_total: None,
            prompt_tokens_total: None,
        };
        let projection = active_model_projection(&snapshot);
        for key in [
            "model_id",
            "provider",
            "model",
            "configured_model",
            "target_kind",
            "health_status",
            "max_model_len",
        ] {
            assert!(
                projection.get(key).is_some(),
                "missing projection key: {key}"
            );
        }
        assert_eq!(projection["health_status"], "verified");
        assert_eq!(projection["max_model_len"], 32768);
    }

    #[test]
    fn finish_doctor_context_masks_through_the_real_command_path() {
        // 走命令真实走的组合路径（拼装 → 脱敏），而不是对自造 JSON 直打脱敏：
        // 家目录前缀改写、GPU 进程完整路径改写、MAC 结构性不在白名单内。
        let context =
            finish_doctor_context(&fixture_snapshot(), None, None, None, &[], None, "/home/u");
        let disks = context["device"]["disks"].as_array().expect("disks");
        assert_eq!(disks[0]["mount_point"], "~/data");
        let procs = context["device"]["gpu_ext"]["compute_processes"]
            .as_array()
            .expect("compute processes");
        assert_eq!(procs[0]["name"], "~/bin/python3");
        let network = context["device"]["network"].as_array().expect("network");
        assert!(network.iter().all(|iface| iface.get("mac").is_none()));
        // 序列化后的整份文本里不得再出现家目录原文。
        let text = serde_json::to_string(&context).expect("serialize");
        assert!(!text.contains("/home/u/"), "home prefix leaked: {text}");
    }

    #[test]
    fn doctor_context_masks_mac_and_home_prefix() {
        let mut context = json!({
            "device": {
                "network": [
                    { "name": "eth0", "mac": "aa:bb:cc:dd:ee:ff" },
                    { "name": "lo", "mac": null }
                ],
                "disks": [
                    { "mount_point": "/home/zhangsan/usb", "total_bytes": 1 },
                    { "mount_point": "/mnt/data", "total_bytes": 2 }
                ],
                "gpu_ext": {
                    "compute_processes": [
                        { "pid": 7, "name": "/home/zhangsan/miniconda3/bin/python3", "used_mib": 64 }
                    ]
                }
            }
        });
        mask_context_privacy(&mut context, "/home/zhangsan");
        let network = context["device"]["network"].as_array().unwrap();
        assert!(network[0].get("mac").is_none(), "MAC 必须整字段剔除");
        assert!(network[1].get("mac").is_none());
        let disks = context["device"]["disks"].as_array().unwrap();
        assert_eq!(disks[0]["mount_point"], "~/usb");
        // 家目录之外的挂载点保持原样。
        assert_eq!(disks[1]["mount_point"], "/mnt/data");
        // GPU 计算进程完整路径同样脱敏（用户名不随家目录外泄）。
        let procs = context["device"]["gpu_ext"]["compute_processes"]
            .as_array()
            .unwrap();
        assert_eq!(procs[0]["name"], "~/miniconda3/bin/python3");
    }

    #[test]
    fn mask_path_text_respects_component_boundaries_and_media_users() {
        // 分量边界：/home/u2 不能吃 /home/u 的前缀（旧实现会改写成 ~2/...）。
        assert_eq!(
            mask_path_text("/home/u2/data", "/home/u", "u"),
            "/home/u2/data"
        );
        assert_eq!(mask_path_text("/home/u", "/home/u", "u"), "~");
        assert_eq!(mask_path_text("/home/u/data", "/home/u", "u"), "~/data");
        // /media/<用户名>/ 与 /run/media/<用户名>/ 自动挂载段：用户名改写为 ~，
        // 卷标原样保留（第四轮评审 P3 补 systemd 的 /run/media 惯例位）。
        assert_eq!(
            mask_path_text("/media/zhangsan/USB", "/home/zhangsan", "zhangsan"),
            "/media/~/USB"
        );
        assert_eq!(
            mask_path_text("/run/media/zhangsan/USB", "/home/zhangsan", "zhangsan"),
            "/run/media/~/USB"
        );
        // 别的用户的 /media 段不动。
        assert_eq!(
            mask_path_text("/media/other/x", "/home/zhangsan", "zhangsan"),
            "/media/other/x"
        );
        assert_eq!(
            mask_path_text("/run/media/other/x", "/home/zhangsan", "zhangsan"),
            "/run/media/other/x"
        );
    }

    #[test]
    fn context_strings_strip_backticks_to_protect_the_json_fence() {
        let mut context = json!({
            "device": {
                "disks": [
                    { "mount_point": "/media/u/```evil```" }
                ]
            }
        });
        mask_context_privacy(&mut context, "/home/u");
        assert_eq!(
            context["device"]["disks"][0]["mount_point"],
            "/media/~/'''evil'''"
        );
    }

    #[test]
    fn mask_path_text_skips_masking_when_home_is_empty() {
        // HOME 为空串（env 置空）时跳过前缀脱敏：`"/"` 前缀臂会把一切绝对路径
        // 改写成 ~/…，脱敏反而毁掉路径信息（第二十轮评审 P3）。
        assert_eq!(mask_path_text("/mnt/data", "", ""), "/mnt/data");
        assert_eq!(mask_path_text("/media/u/usb", "", "u"), "/media/u/usb");
    }

    #[test]
    fn doctor_gpu_projection_maps_unified_memory_na_to_null() {
        // GB10（目标硬件）上 nvidia-smi 对显存/利用率返 [N/A]，monitor 解析层
        // 降级为 0——doctor 上下文必须按"null=不可知"教义外发，否则模型把
        // "显存 0MiB"当真实读数（第二十轮评审 P2）。温度/功耗本来就是 .ok()
        // 语义（N/A→null），不受影响。
        let na_gpu = GpuSnapshot {
            name: "NVIDIA GB10".to_string(),
            vram_used_mib: 0,
            vram_total_mib: 0,
            utilization_pct: 0,
            processor_utilization_pct: None,
            shared_memory_used_mib: None,
            temperature_c: Some(52),
            power_w: Some(31.0),
        };
        let projected = doctor_gpu_projection(&na_gpu);
        assert_eq!(projected["vram_used_mib"], Value::Null);
        assert_eq!(projected["vram_total_mib"], Value::Null);
        assert_eq!(projected["utilization_pct"], Value::Null);
        assert_eq!(projected["unified_memory_na"], Value::Bool(true));
        assert_eq!(projected["temperature_c"], json!(52));
        // 真实读数（vram_total > 0）原样透传，unified_memory_na=false。
        let real_gpu = GpuSnapshot {
            vram_used_mib: 1024,
            vram_total_mib: 8192,
            utilization_pct: 37,
            temperature_c: None,
            power_w: None,
            ..na_gpu
        };
        let projected = doctor_gpu_projection(&real_gpu);
        assert_eq!(projected["vram_used_mib"], json!(1024));
        assert_eq!(projected["vram_total_mib"], json!(8192));
        assert_eq!(projected["utilization_pct"], json!(37));
        assert_eq!(projected["unified_memory_na"], Value::Bool(false));
    }
}
