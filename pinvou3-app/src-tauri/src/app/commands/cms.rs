//! MegaCube 管理能力（cmsd unix socket）只读查询命令（命令薄壳）。
//!
//! socket 客户端已下沉到 `features/megacube`（商店生命周期委托与 MCP 条件注册
//! 复用它）；本文件只保留：能力门控（socket 缺失返回 unavailable 而非报错）、
//! 应答形状封装、Tauri 命令协议。socket 路径：`CMSD_SOCKET` 覆盖，生产默认
//! `/run/megacube/cmsd.sock`（决策 D7）。

use super::prelude::*;
use crate::features::megacube::Client;

/// 只读查询的统一应答形状（与 model_store 的 StoreAvailability 同款惯例）。
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CmsQueryResult {
    pub available: bool,
    pub detected: bool,
    /// 不可用时给调用方看的机器可读原因（socket_missing / connect_failed / ...）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    /// 可用时透传 cmsd 的 JSON 应答。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub data: Option<serde_json::Value>,
}

impl CmsQueryResult {
    fn ok(data: serde_json::Value) -> Self {
        CmsQueryResult {
            available: true,
            detected: true,
            reason: None,
            data: Some(data),
        }
    }

    fn unavailable(reason: impl Into<String>) -> Self {
        CmsQueryResult {
            available: false,
            detected: crate::features::megacube::tooling_available(),
            reason: Some(reason.into()),
            data: None,
        }
    }
}

async fn query(path: &'static str) -> CmsQueryResult {
    let result = tauri::async_runtime::spawn_blocking(move || {
        let client = Client::connect()?;
        match path {
            "/v1/status" => client.status(),
            "/v1/models" => client.models(),
            "/v1/engines" => client.engines(),
            "/v1/capacity" => client.capacity(),
            "/v1/tasks" => client.tasks(),
            other => Err(format!("unknown_path: {other}")),
        }
    })
    .await;
    match result {
        Ok(Ok(data)) if data.get("error").is_some() => {
            CmsQueryResult::unavailable(data["error"].to_string())
        }
        Ok(Ok(data))
            if path == "/v1/status"
                && data.get("service").and_then(serde_json::Value::as_str) != Some("cmsd") =>
        {
            CmsQueryResult::unavailable("invalid_service")
        }
        Ok(Ok(data)) => CmsQueryResult::ok(data),
        Ok(Err(reason)) => CmsQueryResult::unavailable(reason),
        Err(e) => CmsQueryResult::unavailable(format!("join_failed: {e}")),
    }
}

/// MegaCube 管理面状态（模型/引擎/活跃指针/降级列表概览）。
#[tauri::command]
pub async fn cms_status() -> Result<CmsQueryResult, String> {
    Ok(query("/v1/status").await)
}

/// MegaCube 已登记模型清单（含健康、重启计数、degraded 原因）。
#[tauri::command]
pub async fn cms_models() -> Result<CmsQueryResult, String> {
    Ok(query("/v1/models").await)
}

/// MegaCube 引擎注册表（容器+原生统一视图，含野生收编实例）。
#[tauri::command]
pub async fn cms_engines() -> Result<CmsQueryResult, String> {
    Ok(query("/v1/engines").await)
}

/// MegaCube 内存容量（总量/可用/安全边际与常驻模型实测占用）。
#[tauri::command]
pub async fn cms_capacity() -> Result<CmsQueryResult, String> {
    Ok(query("/v1/capacity").await)
}

/// MegaCube 任务列表（维护模式窗口只消费 switch 换胎任务；启停任务由发起方自行呈现）。
#[tauri::command]
pub async fn cms_tasks() -> Result<CmsQueryResult, String> {
    Ok(query("/v1/tasks").await)
}

/// Retain task execution errors as facts; only transport/error envelopes fail observation.
fn task_observation(task_id: &str, value: serde_json::Value) -> Result<serde_json::Value, String> {
    if value.get("id").and_then(serde_json::Value::as_str) != Some(task_id)
        || !["kind", "card", "status"]
            .iter()
            .all(|key| value.get(key).is_some_and(serde_json::Value::is_string))
        || value
            .get("error")
            .is_some_and(|error| !error.is_null() && !error.is_string())
        || value
            .get("http_status")
            .and_then(serde_json::Value::as_u64)
            .is_some_and(|status| status != 200)
    {
        return Err("task_observation_unconfirmed".to_string());
    }
    Ok(value)
}

/// Query one previously accepted task without relying on the recent-task index.
#[tauri::command]
pub async fn cms_task(task_id: String) -> Result<CmsQueryResult, String> {
    crate::features::megacube::validate_card_id(&task_id)
        .map_err(|_| "invalid_task".to_string())?;
    let result = tauri::async_runtime::spawn_blocking(move || {
        task_observation(&task_id, Client::connect()?.task(&task_id)?)
    })
    .await;
    Ok(match result {
        Ok(Ok(value)) => CmsQueryResult::ok(value),
        Ok(Err(error)) => CmsQueryResult::unavailable(error),
        Err(error) => CmsQueryResult::unavailable(format!("join_failed: {error}")),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn task_details_preserve_execution_error_null_string_and_cancelled_facts() {
        let cases: serde_json::Value = serde_json::from_str(include_str!(
            "../../../../tests/fixtures/cmsd-switch-task-details.json"
        ))
        .unwrap();
        for name in ["succeeded", "failed", "cancelled"] {
            let original = cases[name]["body"].clone();
            let observed = task_observation("t-old-switch", original.clone()).unwrap();
            assert_eq!(observed, original);
            assert!(CmsQueryResult::ok(observed).available);
        }
        assert!(task_observation("t-old-switch", cases["not_found"]["body"].clone()).is_err());
        let mut rejected = cases["succeeded"]["body"].clone();
        rejected["http_status"] = serde_json::json!(404);
        assert!(task_observation("t-old-switch", rejected).is_err());
        let wrong = cases["succeeded"]["body"].clone();
        assert!(task_observation("different-task", wrong).is_err());
    }

    #[tokio::test]
    async fn task_detail_rejects_unsafe_ids_before_connecting() {
        for id in ["", ".", "..", "a/b", "a?b", "a\nb", &"a".repeat(129)] {
            assert!(matches!(cms_task(id.into()).await, Err(error) if error == "invalid_task"));
        }
    }

    #[test]
    fn unavailable_shape() {
        let r = CmsQueryResult::unavailable("socket_missing: x");
        assert!(!r.available);
        assert_eq!(r.reason.as_deref(), Some("socket_missing: x"));
        assert!(r.data.is_none());
        let json = serde_json::to_value(&r).unwrap();
        assert_eq!(json["available"], false);
        assert!(json.get("data").is_none()); // skip_serializing_if 生效
    }

    #[test]
    fn ok_shape() {
        let r = CmsQueryResult::ok(serde_json::json!({"models": []}));
        assert!(r.available);
        let json = serde_json::to_value(&r).unwrap();
        assert_eq!(json["data"]["models"], serde_json::json!([]));
        assert!(json.get("reason").is_none());
    }

    #[tokio::test]
    async fn missing_socket_returns_unavailable() {
        // 指向一个必不存在的路径（socket_path 解析在 features::megacube 单测覆盖）；
        // 环境变量读写全程持 ENV_LOCK（项目既有并发隔离惯例）。
        let _lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let key = "CMSD_SOCKET";
        let saved = std::env::var(key).ok();
        // SAFETY: 同上——调用方持有 ENV_LOCK，串行化环境变量读写。
        unsafe {
            std::env::set_var(key, "/tmp/cmsd-definitely-not-here.sock");
        }
        let r = query("/v1/status").await;
        // SAFETY: 同上。
        match saved {
            // SAFETY: ENV_LOCK is held; env writes are serialized in-process.
            Some(v) => unsafe { std::env::set_var(key, v) },
            // SAFETY: ENV_LOCK is held; env writes are serialized in-process.
            None => unsafe { std::env::remove_var(key) },
        }
        assert!(!r.available);
        assert!(
            r.reason
                .as_deref()
                .unwrap_or("")
                .starts_with("socket_missing"),
            "{:?}",
            r.reason
        );
    }

    #[tokio::test]
    async fn dotted_card_ids_pass_the_desktop_gate_and_unsafe_ids_are_rejected() {
        let _lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let _environment = crate::platform::paths::tests::EnvVarGuard::capture(&["CMSD_SOCKET"]);
        let home = tempfile::tempdir().unwrap();
        // SAFETY: the shared environment lock covers the whole test.
        unsafe {
            std::env::set_var("CMSD_SOCKET", home.path().join("missing.sock"));
        }
        let error = cms_model_action("qwen3.5".into(), "enable".into())
            .await
            .unwrap_err();
        assert!(
            error.starts_with("socket_missing:"),
            "valid card was rejected before connecting: {error}"
        );
        for card in ["", ".", "..", "a/b", "a?b", "a\nb", &"a".repeat(129)] {
            assert_eq!(
                cms_model_action(card.into(), "enable".into())
                    .await
                    .unwrap_err(),
                "invalid_card"
            );
        }
    }

    /// V7 真机验收路径：设了 CMSD_SOCKET 才跑（指向 dev cmsd），否则跳过。
    #[tokio::test]
    async fn live_socket_matches_cmsd() {
        if std::env::var("CMSD_SOCKET").is_err() {
            eprintln!("CMSD_SOCKET 未设置，跳过真机验收路径");
            return;
        }
        let status = query("/v1/status").await;
        assert!(status.available, "{:?}", status.reason);
        assert_eq!(status.data.as_ref().unwrap()["service"], "cmsd");
        let models = query("/v1/models").await;
        assert!(models.available, "{:?}", models.reason);
        assert!(models.data.unwrap()["models"].is_array());
        let engines = query("/v1/engines").await;
        assert!(engines.available, "{:?}", engines.reason);
        let capacity = query("/v1/capacity").await;
        assert!(capacity.available, "{:?}", capacity.reason);
        assert!(capacity.data.unwrap()["memory"]["total_gib"].is_number());
    }
}

/// Direct rescue actions share cmsd's lifecycle and budget checks.
#[tauri::command]
pub async fn cms_model_action(card: String, action: String) -> Result<serde_json::Value, String> {
    crate::features::megacube::validate_card_id(&card)?;
    if ![
        "enable",
        "disable",
        "set-default",
        "enable-boot",
        "disable-boot",
    ]
    .contains(&action.as_str())
    {
        return Err("unsupported_action".into());
    }
    tauri::async_runtime::spawn_blocking(move || Client::connect()?.model_action(&card, &action))
        .await
        .map_err(|e| e.to_string())?
}

#[tauri::command]
pub async fn cms_cancel_task(task_id: String) -> Result<serde_json::Value, String> {
    if task_id.is_empty()
        || !task_id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b))
    {
        return Err("invalid_task".into());
    }
    tauri::async_runtime::spawn_blocking(move || Client::connect()?.cancel_task(&task_id))
        .await
        .map_err(|e| e.to_string())?
}

/// Component controls remain desktop commands, independent of LLM/MCP and of
/// the daemon that can be stopped or absent. Mutations return durable job IDs.
#[tauri::command]
pub async fn cms_service_status() -> Result<serde_json::Value, String> {
    tauri::async_runtime::spawn_blocking(crate::features::megacube::service::status)
        .await
        .map_err(|error| error.to_string())?
}

#[tauri::command]
pub async fn cms_service_action(
    action: String,
    package_path: Option<String>,
    expected_sha256: Option<String>,
) -> Result<serde_json::Value, String> {
    tauri::async_runtime::spawn_blocking(move || {
        crate::features::megacube::service::submit(&action, package_path, expected_sha256)
    })
    .await
    .map_err(|error| error.to_string())?
}

#[tauri::command]
pub async fn cms_service_bundled_package() -> Result<serde_json::Value, String> {
    tauri::async_runtime::spawn_blocking(crate::features::megacube::service::bundled_package)
        .await
        .map_err(|error| error.to_string())?
}

#[tauri::command]
pub async fn cms_service_inspect_package(
    package_path: String,
) -> Result<serde_json::Value, String> {
    tauri::async_runtime::spawn_blocking(move || {
        crate::features::megacube::service::inspect_package(&package_path)
    })
    .await
    .map_err(|error| error.to_string())?
}

#[tauri::command]
pub async fn cms_service_logs() -> Result<serde_json::Value, String> {
    tauri::async_runtime::spawn_blocking(crate::features::megacube::service::logs)
        .await
        .map_err(|error| error.to_string())?
}

/// Read-only provisioning discovery; full payload hashing is an explicit worker job.
#[tauri::command]
pub async fn cms_device_status(inspect_running: Option<bool>) -> Result<serde_json::Value, String> {
    tauri::async_runtime::spawn_blocking(move || {
        crate::features::megacube::device::status(inspect_running.unwrap_or(false))
    })
    .await
    .map_err(|error| error.to_string())?
}
