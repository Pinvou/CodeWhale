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
use crate::features::monitor::MonitorState;

/// 设备管理页 2s 轮询的完整快照。
#[tauri::command]
pub async fn get_device_snapshot(device: State<'_, DeviceState>) -> Result<DeviceSnapshot, String> {
    Ok(device::sample_snapshot(&device).await)
}

/// 模型服务可达性（慢轮询 10s / 手动刷新；docker 探测带 3s 子进程预算）。
#[tauri::command]
pub async fn get_device_services() -> Result<Vec<ServiceStatus>, String> {
    Ok(device::probe_services().await)
}

/// 本机全部外部模型（注册表 + Ollama 在线列表；坏注册表文件跳过进 warnings）。
#[tauri::command]
pub async fn get_external_models() -> Result<ExternalModelsView, String> {
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
    device::register_external_model(name, kind, base_url, model, weights_path).await
}

/// 移除外部模型（守卫链：确认名 → 商店边界 → 权重删除守卫 → Ollama 删除 → 删记录）。
#[tauri::command]
pub async fn device_remove_external_model(
    id: String,
    delete_weights: bool,
    confirm_name: String,
) -> Result<RemoveOutcome, String> {
    device::remove_external_model(id, delete_weights, confirm_name).await
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
    let parsed_kind = device::DoctorKind::parse(&kind)?;
    let context = build_doctor_context(&device, &monitor).await;
    device::build_doctor_prompt(parsed_kind, question.as_deref(), &context)
}

/// 故障排查上下文（设计 §5 第 4 节）：设备快照（传感器/磁盘/网络/进程/GPU 扩展）
/// + 运行状态（CPU/GPU/内存）+ 服务可达性 + 当前活动模型健康。键名英文 snake_case。
async fn build_doctor_context(device: &DeviceState, monitor: &MonitorState) -> Value {
    // 三路来源互不依赖，并发采样。串行时 vLLM 三端口逐个 1.5s 超时 + docker 3s，
    // 最坏 8-12s 用户点排查卡才开出会话（评审 P2）；并发后总耗时 ≈ 最慢一路。
    let (device_snapshot, monitor_snapshot, services) = tokio::join!(
        device::sample_snapshot(device),
        crate::features::monitor::sample_all(monitor),
        device::probe_services(),
    );
    let active_model = monitor_snapshot.vllm.as_ref().map(active_model_projection);
    let mut context = assemble_doctor_context(
        json!(device_snapshot),
        json!(monitor_snapshot.gpu),
        json!(monitor_snapshot.cpu),
        json!(monitor_snapshot.ram),
        json!(services),
        active_model,
    );
    mask_context_privacy(
        &mut context,
        &crate::platform::paths::user_home_dir().to_string_lossy(),
    );
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
/// 自证，测不到真实函数）。
fn assemble_doctor_context(
    device: Value,
    gpu: Value,
    cpu: Value,
    ram: Value,
    services: Value,
    active_model: Option<Value>,
) -> Value {
    json!({
        "device": device,
        "gpu": gpu,
        "cpu": cpu,
        "ram": ram,
        "services": services,
        "active_model": active_model,
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

    #[test]
    fn doctor_context_assembles_the_documented_contract_keys() {
        // 键名契约直打真实拼装函数（设计 §5 第 4 节）：前端与提示词模板都按这些
        // 键取数。此前这里用 json! 自造字面量自证，测不到 build_doctor_context。
        let context = assemble_doctor_context(
            json!({"os": {"arch": "aarch64"}}),
            json!(null),
            json!(null),
            json!(null),
            json!([{"key": "ollama", "running": false}]),
            Some(json!({"model_id": "m", "health_status": "healthy"})),
        );
        for key in ["device", "gpu", "cpu", "ram", "services", "active_model"] {
            assert!(context.get(key).is_some(), "missing context key: {key}");
        }
        assert_eq!(context["device"]["os"]["arch"], "aarch64");
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
            health_status: "verified".to_string(),
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
}
