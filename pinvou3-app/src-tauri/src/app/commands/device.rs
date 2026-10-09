//! 设备管理（MegaCube）的 Tauri 命令层（设计 §2.3）。
//!
//! 与 `model_store.rs` 同一纪律：这层**只做搬运**——取 managed state、转发参数、
//! 把 `features::device` 的结果原样回给前端。唯一多出来的一件事是
//! `device_doctor_prompt` 的上下文组装：把设备快照 + 运行状态 + 服务探测拼成
//! 一份 JSON，交给提示词模板注入。
//!
//! 依赖方向合规：`app/commands → features::device / features::monitor`，features
//! 内部零 `#[tauri::command]`、零 `crate::app::*` 引用（架构守卫）。

use serde_json::{json, Value};
use tauri::State;

use crate::features::device::{
    self, DeviceState, DeviceSnapshot, ExternalModel, ExternalModelsView, RemoveOutcome,
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
    let device_snapshot = device::sample_snapshot(device).await;
    let monitor_snapshot = crate::features::monitor::sample_all(monitor).await;
    let services = device::probe_services().await;
    let active_model = monitor_snapshot.vllm.as_ref().map(|model| {
        json!({
            "model_id": model.model_id,
            "provider": model.provider,
            "model": model.model,
            "configured_model": model.configured_model,
            "target_kind": model.target_kind,
            "health_status": model.health_status,
            "max_model_len": model.max_model_len,
        })
    });
    json!({
        "device": device_snapshot,
        "gpu": monitor_snapshot.gpu,
        "cpu": monitor_snapshot.cpu,
        "ram": monitor_snapshot.ram,
        "services": services,
        "active_model": active_model,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn doctor_context_json_has_snake_case_contract_keys() {
        // 上下文键名契约：前端与提示词模板都按这些键取数（设计 §5 第 4 节）。
        let value = json!({
            "device": null, "gpu": null, "cpu": null, "ram": null,
            "services": [], "active_model": null
        });
        for key in ["device", "gpu", "cpu", "ram", "services", "active_model"] {
            assert!(value.get(key).is_some(), "missing context key: {key}");
        }
    }
}
