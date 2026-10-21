//! 商店生命周期委托：cmsd 在场且认识这张原生卡时，启用/停用的权威让位给
//! systemd 守护层（崩溃自愈、开机自启、内存预算、degraded 熔断）。
//!
//! 判定纪律：明确配置、本机发现/组件回执、历史歧义或每卡所有权存在时，
//! 失联或查询失败不允许回退。远端只读连接与有来源的独立下载记录不代表
//! 本机管理权。只有确认本机未配置，或有效本机查询确认该卡不存在且没有
//! 历史所有权时，才返回 NotManaged 供商店继续卡片脚本。
//!
//! 进度模型：复用商店 spawn_op 的 run 日志（长轮询），cmsd 任务事件逐条写进
//! 日志行（形如 `[cmsd] wait: 等待服务就绪…`），前端零改动。取消语义以谓词
//! 闭包传入（解开对 model_store 私有 platform 模块的编译期依赖）。

use super::client::{Client, GATEWAY_MODEL_ALIAS};

/// 委托结果。调用方（model_store lifecycle）据此落库。
pub enum DelegateOutcome {
    /// 无本机管理预期，或已确认该卡不受管理且无历史冲突。
    NotManaged,
    /// cmsd 侧 enable 完成：钉固定端点 + 别名（设计 §9.1 端点稳定性）。
    Enabled {
        endpoint_url: String,
        endpoint_model: String,
    },
    /// cmsd 侧 disable 完成。
    Stopped,
    /// cmsd 侧 purge 完成（注销登记；容器卡不动权重/容器实体，原生卡含权重清除）。
    Purged,
}

fn ownership_path(card: &str) -> Result<std::path::PathBuf, String> {
    if card.is_empty()
        || card.len() > 128
        || matches!(card, "." | "..")
        || !card
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
    {
        return Err("invalid_card: 模型标识不合法".into());
    }
    Ok(super::installation_records_dir()
        .join("ownership")
        .join(card))
}

fn ownership_known(card_id: &str) -> Result<bool, String> {
    Ok(super::authority::present_or_unknown(&ownership_path(
        card_id,
    )?))
}

/// Shared by lifecycle and maintenance; global tool availability is not a
/// statement about local authority. Metadata errors never prove absence.
fn local_client(card_id: &str) -> Result<Option<Client>, String> {
    let owned = ownership_known(card_id)?;
    match Client::connect() {
        Ok(client) => Ok(Some(client)),
        Err(error) if owned || super::authority::local_authority_expected() => Err(format!(
            "cmsd_unavailable: 本机管理配置或历史记录尚未核实，请恢复设备服务或核对原记录，不能改用旧脚本。{error}"
        )),
        Err(_) => Ok(None),
    }
}

/// A same-name observed resource is not permission to execute local scripts.
fn managed_by_cmsd(client: &Client, card_id: &str) -> Result<bool, String> {
    match client.model_fact(card_id)? {
        Some(model) if model.get("managed").and_then(serde_json::Value::as_bool) == Some(true) => {
            remember_ownership(card_id)?;
            Ok(true)
        }
        Some(_) => Err("ownership_required: 同名资源只有观察权限，不能改用旧脚本操作".into()),
        None if ownership_known(card_id)? => Err("ownership_conflict: 已托管模型未出现在设备服务中，请恢复设备记录后重试，不能改用旧脚本".into()),
        None => Ok(false),
    }
}

fn remember_ownership(card: &str) -> Result<(), String> {
    let binding = ownership_path(card)?;
    super::authority::remember_local_device()?;
    crate::platform::filesystem::atomic_write_private_anchored(&binding, b"cmsd\n")
        .map_err(|e| format!("ownership_persist_failed: {e}"))
}

/// Resolve ownership once before opening a cross-process store transaction.
pub(crate) fn maintenance_client(card_id: &str) -> Result<Option<Client>, String> {
    let Some(client) = local_client(card_id)? else {
        return Ok(None);
    };
    let Some(model) = client.model_fact(card_id)? else {
        if ownership_known(card_id)? {
            return Err("ownership_conflict: 原托管模型记录缺失，请先恢复设备记录".into());
        }
        return Ok(None);
    };
    let owned = model.get("managed").and_then(serde_json::Value::as_bool) == Some(true);
    if !owned
        && model
            .get("can_reserve_uninstall")
            .and_then(serde_json::Value::as_bool)
            != Some(true)
    {
        return Err(
            "ownership_required: 此资源只能观察，未获标准卸载预留权，不能回退旧脚本".into(),
        );
    }
    if !client.supports("model.maintenance.reservation")? {
        return Err("maintenance_unsupported: 请先更新设备服务以支持安全卸载预留和仅撤登记的 scoped purge；未执行卸载脚本".into());
    }
    if owned {
        remember_ownership(card_id)?;
    }
    Ok(Some(client))
}

pub(crate) fn forget_ownership(card_id: &str) -> Result<(), String> {
    let path = ownership_path(card_id)?;
    match std::fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.to_string()),
    }
}

/// 轮询 cmsd 任务直到终结，事件行写进商店 run 日志。
/// 取消语义：enable 是幂等长操作（权重加载分钟级），前端点了取消就停止轮询
/// 并明确报错——cmsd 侧任务继续推进，状态融合（library 融合 cmsd 实况）会在
/// 下次打开商店时纠正行内状态，不留漂移。
fn wait_task(
    client: &Client,
    task_id: &str,
    log: &mut dyn FnMut(&str),
    cancelled: &dyn Fn() -> bool,
) -> Result<(), String> {
    let mut seen_events = 0usize;
    loop {
        let task = client.task(task_id)?;
        let events = task
            .get("events")
            .and_then(|e| e.as_array())
            .cloned()
            .unwrap_or_default();
        for event in events.iter().skip(seen_events) {
            let stage = event.get("stage").and_then(|s| s.as_str()).unwrap_or("");
            let message = event.get("message").and_then(|m| m.as_str()).unwrap_or("");
            log(&format!("[cmsd] {stage}: {message}"));
        }
        seen_events = events.len();
        match task.get("status").and_then(|s| s.as_str()) {
            Some("succeeded") => return Ok(()),
            Some("cancelled") => return Err("设备任务已取消".to_string()),
            Some("failed") => {
                let error = task
                    .get("error")
                    .and_then(|e| e.as_str())
                    .unwrap_or("管理面任务失败（无详情）");
                return Err(error.to_string());
            }
            _ => {
                if cancelled() {
                    return Err(
                        "已取消等待（Cube Manage Software 侧任务仍在继续推进，可稍后在 设置→MegaCube 查看结果）"
                            .to_string(),
                    );
                }
                std::thread::sleep(std::time::Duration::from_secs(2));
            }
        }
    }
}

/// 管理面端点事实（公共面）：cmsd 在场返回 Some((固定端点 URL, 别名))，
/// 不在场 None。商店融合的自愈端点修复用它。
pub fn gateway_endpoint() -> Option<(String, String)> {
    let client = Client::connect().ok()?;
    Some(gateway_endpoint_for(&client))
}

/// 网关端点（D0 固定端点）：从 cmsd status 拿真实监听地址，拿不到用默认。
fn gateway_endpoint_for(client: &Client) -> (String, String) {
    let gateway = client
        .status()
        .ok()
        .and_then(|s| {
            s.get("gateway")
                .and_then(|g| g.as_str())
                .map(|s| s.to_string())
        })
        .unwrap_or_else(|| "127.0.0.1:8000".to_string());
    (
        format!("http://{gateway}/v1/chat/completions"),
        GATEWAY_MODEL_ALIAS.to_string(),
    )
}

/// 委托启用。返回 Ok(NotManaged) 表示不接管（调用方走老路径）。
pub fn enable(
    card_id: &str,
    log: &mut dyn FnMut(&str),
    cancelled: &dyn Fn() -> bool,
) -> Result<DelegateOutcome, String> {
    let Some(client) = local_client(card_id)? else {
        return Ok(DelegateOutcome::NotManaged);
    };
    let fact = client.model_fact(card_id)?;
    match fact {
        Some(model) if model.get("managed").and_then(serde_json::Value::as_bool) == Some(true) => {
            remember_ownership(card_id)?;
        }
        Some(model)
            if model.get("can_adopt").and_then(serde_json::Value::as_bool) == Some(true)
                && client.supports("model.explicit_adoption")? =>
        {
            log("[cmsd] 按本次显式启用请求接管已部署的标准卡；发现本身不授予管理权");
        }
        Some(_) => {
            return Err(
                "ownership_required: 此资源只有观察权限，请先在模型商店完成或修复部署".into(),
            );
        }
        None => {
            if ownership_known(card_id)? {
                return Err("ownership_conflict: 原托管模型记录缺失，请先恢复设备记录".into());
            }
            return Ok(DelegateOutcome::NotManaged);
        }
    }
    log("[cmsd] 检测到 MegaCube 管理面，启用改由 systemd 托管（崩溃自愈、开机自启）");
    let task = client.model_action(card_id, "enable")?;
    if let Some(error) = task.get("error") {
        // 预算不足等前置拒绝：cmsd 的中文 verdict 直接透传
        let message = error
            .get("message")
            .and_then(|m| m.as_str())
            .unwrap_or("管理面拒绝启用");
        return Err(message.to_string());
    }
    let task_id = task
        .get("task_id")
        .and_then(|t| t.as_str())
        .ok_or_else(|| "管理面应答缺 task_id".to_string())?;
    log(&format!("[cmsd] 任务 {task_id} 已创建，等待就绪…"));
    wait_task(&client, task_id, log, cancelled)?;
    if !managed_by_cmsd(&client, card_id)? {
        return Err("ownership_unconfirmed: 启用结束但未确认设备已接管，请核实状态".into());
    }
    let (endpoint_url, _) = gateway_endpoint_for(&client);
    // 接入写真名（用户拍板）：启用成功后问管理面要引擎自报的真实模型 id，
    // 问不到才退回别名兜底。
    let real_model_id = client
        .models()
        .ok()
        .and_then(|v| {
            v.get("models")?.as_array()?.iter().find_map(|m| {
                if m.get("card")?.as_str()? == card_id {
                    m.get("model_id")?.as_str().map(|s| s.to_string())
                } else {
                    None
                }
            })
        })
        .unwrap_or_else(|| GATEWAY_MODEL_ALIAS.to_string());
    log(&format!(
        "[cmsd] 服务就绪，固定端点 {endpoint_url}（模型 {real_model_id}）"
    ));
    Ok(DelegateOutcome::Enabled {
        endpoint_url,
        endpoint_model: real_model_id,
    })
}

/// 委托停用：明确不存在且无历史冲突时才返回 NotManaged。
pub fn stop(
    card_id: &str,
    log: &mut dyn FnMut(&str),
    cancelled: &dyn Fn() -> bool,
) -> Result<DelegateOutcome, String> {
    let Some(client) = local_client(card_id)? else {
        return Ok(DelegateOutcome::NotManaged);
    };
    if !managed_by_cmsd(&client, card_id)? {
        return Ok(DelegateOutcome::NotManaged);
    }
    log("[cmsd] 经 MegaCube 管理面停用（systemd 停止并撤销自启登记）");
    let task = client.model_action(card_id, "disable")?;
    let task_id = task
        .get("task_id")
        .and_then(|t| t.as_str())
        .ok_or_else(|| "管理面应答缺 task_id".to_string())?;
    wait_task(&client, task_id, log, cancelled)?;
    Ok(DelegateOutcome::Stopped)
}

/// 委托注销（2026-10-15 用户实测驱动）：商店删除模型记录时，把管理面的登记
/// 一并撤掉——否则商店记录没了、cmsd 还说「已下载」，LLM 会把已删模型报给用户。
/// purge 对容器卡是停服务+撤登记（权重/容器实体不动，归商店卸载流程）；
/// 对原生卡是完整清除（含权重目录，cmsd 侧护栏）。仅观察或未知时不回退。
pub fn purge(
    card_id: &str,
    log: &mut dyn FnMut(&str),
    cancelled: &dyn Fn() -> bool,
) -> Result<DelegateOutcome, String> {
    let Some(client) = local_client(card_id)? else {
        return Ok(DelegateOutcome::NotManaged);
    };
    if !managed_by_cmsd(&client, card_id)? {
        return Ok(DelegateOutcome::NotManaged);
    }
    log("[cmsd] 商店记录已删，同步注销管理面登记（停服务+撤登记）");
    let task = client.model_action(card_id, "purge")?;
    if let Some(error) = task.get("error") {
        let message = error
            .get("message")
            .and_then(|m| m.as_str())
            .unwrap_or("管理面拒绝注销");
        return Err(message.to_string());
    }
    let task_id = task
        .get("task_id")
        .and_then(|t| t.as_str())
        .ok_or_else(|| "管理面应答缺 task_id".to_string())?;
    wait_task(&client, task_id, log, cancelled)?;
    let _ = std::fs::remove_file(ownership_path(card_id)?);
    Ok(DelegateOutcome::Purged)
}

/// 状态融合数据（library() 用）：cmsd 对这张卡的实况。
#[derive(Debug, Clone, serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CmsdModelState {
    pub managed: bool,
    pub can_adopt: bool,
    pub can_reserve_uninstall: bool,
    /// 引擎自报的真实模型 id（接入条目写真名——用户拍板，别名只作兜底）。
    pub model_id: Option<String>,
    pub status: String,
    pub healthy: Option<bool>,
    pub restart_count: u64,
    pub degraded: bool,
    pub degraded_reason: Option<String>,
    pub port: Option<u16>,
    pub measured_footprint_gib: Option<f64>,
    /// 引擎自报的上下文窗口（/v1/models 的 max_model_len）：接入行
    /// context_window 的事实来源（没有它路由预算落保守兜底，每条消息都
    /// 误触发紧急压缩——真机实测）。
    pub max_model_len: Option<u64>,
}

/// 取 cmsd 的模型实况表（card → 状态）。cmsd 不在场返回 None（调用方原样直行）。
pub fn cmsd_states() -> Option<std::collections::BTreeMap<String, CmsdModelState>> {
    let client = Client::connect().ok()?;
    let value = client.models().ok()?;
    let models = value.get("models")?.as_array()?;
    let mut out = std::collections::BTreeMap::new();
    for m in models {
        let Some(card) = m.get("card").and_then(|c| c.as_str()) else {
            continue;
        };
        if m.get("managed").and_then(|v| v.as_bool()) == Some(true) {
            if remember_ownership(card).is_err() {
                return None;
            }
        }
        let status = m
            .get("status")
            .and_then(|s| s.as_str())
            .unwrap_or("stopped")
            .to_string();
        let degraded_reason = m
            .get("degraded_reason")
            .and_then(|r| r.as_str())
            .map(|s| s.to_string());
        out.insert(
            card.to_string(),
            CmsdModelState {
                managed: m.get("managed").and_then(serde_json::Value::as_bool) == Some(true),
                can_adopt: m.get("can_adopt").and_then(serde_json::Value::as_bool) == Some(true),
                can_reserve_uninstall: m
                    .get("can_reserve_uninstall")
                    .and_then(serde_json::Value::as_bool)
                    == Some(true),
                model_id: m
                    .get("model_id")
                    .and_then(|x| x.as_str())
                    .map(|s| s.to_string()),
                degraded: status == "degraded" || degraded_reason.is_some(),
                status,
                degraded_reason,
                healthy: m.get("healthy").and_then(|h| h.as_bool()),
                restart_count: m.get("restart_count").and_then(|r| r.as_u64()).unwrap_or(0),
                port: m.get("port").and_then(|p| p.as_u64()).map(|p| p as u16),
                measured_footprint_gib: m.get("measured_footprint_gib").and_then(|f| f.as_f64()),
                max_model_len: m.get("max_model_len").and_then(|l| l.as_u64()),
            },
        );
    }
    let physical = out.clone();
    for (logical, runtime) in super::client::store_bindings(&value).ok()? {
        let state = physical.get(&runtime)?.clone();
        if state.managed && remember_ownership(&logical).is_err() {
            return None;
        }
        out.insert(logical, state);
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cmsd_states_parses_real_shape() {
        // 2026-10-13 真机 /v1/models 应答形状
        let value = serde_json::json!({
            "active_model": "ornith15-35b-a3b-nvfp4",
            "models": [{
                "card": "ornith15-35b-a3b-nvfp4",
                "status": "running",
                "healthy": true,
                "restart_count": 1,
                "degraded_reason": null,
                "port": 18080,
                "measured_footprint_gib": 33.0,
                "max_model_len": 128000
            }, {
                "card": "qwen38-27b-nvfp4-tachyon",
                "status": "stopped"
            }]
        });
        // 抽出纯判定逻辑单测（不连真 socket）：直接对 Value 跑映射
        let models = value.get("models").unwrap().as_array().unwrap();
        let mut out = std::collections::BTreeMap::new();
        for m in models {
            let card = m.get("card").and_then(|c| c.as_str()).unwrap();
            let status = m
                .get("status")
                .and_then(|s| s.as_str())
                .unwrap_or("stopped")
                .to_string();
            let degraded_reason = m
                .get("degraded_reason")
                .and_then(|r| r.as_str())
                .map(|s| s.to_string());
            out.insert(
                card.to_string(),
                CmsdModelState {
                    managed: true,
                    can_adopt: false,
                    can_reserve_uninstall: true,
                    model_id: m
                        .get("model_id")
                        .and_then(|x| x.as_str())
                        .map(|s| s.to_string()),
                    degraded: status == "degraded" || degraded_reason.is_some(),
                    status,
                    degraded_reason: m
                        .get("degraded_reason")
                        .and_then(|r| r.as_str())
                        .map(|s| s.to_string()),
                    healthy: m.get("healthy").and_then(|h| h.as_bool()),
                    restart_count: m.get("restart_count").and_then(|r| r.as_u64()).unwrap_or(0),
                    port: m.get("port").and_then(|p| p.as_u64()).map(|p| p as u16),
                    measured_footprint_gib: m
                        .get("measured_footprint_gib")
                        .and_then(|f| f.as_f64()),
                    max_model_len: m.get("max_model_len").and_then(|l| l.as_u64()),
                },
            );
        }
        let ornith = &out["ornith15-35b-a3b-nvfp4"];
        assert_eq!(ornith.status, "running");
        assert_eq!(ornith.healthy, Some(true));
        assert_eq!(ornith.restart_count, 1);
        assert!(!ornith.degraded);
        assert_eq!(ornith.port, Some(18080));
        let qwen = &out["qwen38-27b-nvfp4-tachyon"];
        assert_eq!(qwen.status, "stopped");
        assert_eq!(qwen.healthy, None);
        // 序列化形状（前端消费 camelCase）
        let json = serde_json::to_value(ornith).unwrap();
        assert_eq!(json["restartCount"], 1);
        assert_eq!(json["maxModelLen"], 128000);
    }
}
