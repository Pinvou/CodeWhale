//! cmsd unix socket 客户端（std UnixStream + 极简 HTTP/1.1，不引新依赖）。
//!
//! 同步阻塞：调用方都在非 async 上下文（命令层 spawn_blocking / 商店 spawn_op
//! 线程），单次请求最坏 2s 连接 + 5s IO 超时。

// architecture-guard: allow-target-cfg -- cmsd 是 MegaCube 本机 Linux 守护进程，
// unix socket 只在该平台存在；非 unix 平台显式返回 unsupported（client 单测覆盖
// 解析与判定形状，commands::cms 层有 socket_missing 门控测试）。

use serde_json::Value;
use std::io::{Read, Write};
use std::path::PathBuf;
use std::time::Duration;

pub const DEFAULT_SOCKET_PATH: &str = "/run/megacube/cmsd.sock";
/// 设备级默认别名：网关改写为活跃模型的真实 id；具体卡片连接使用各自已确认的模型 id。
pub const GATEWAY_MODEL_ALIAS: &str = "megacube-default";

const CONNECT_TIMEOUT: Duration = Duration::from_secs(2);
const IO_TIMEOUT: Duration = Duration::from_secs(5);

/// socket 路径：`CMSD_SOCKET` 覆盖 > 生产默认。
pub fn socket_path() -> PathBuf {
    if let Some(value) = std::env::var_os("CMSD_SOCKET") {
        if !value.is_empty() {
            return PathBuf::from(value);
        }
    }
    let system = PathBuf::from(DEFAULT_SOCKET_PATH);
    if system.exists() {
        return system;
    }
    if let Some(runtime) = std::env::var_os("XDG_RUNTIME_DIR") {
        return PathBuf::from(runtime).join("megacube/cmsd.sock");
    }
    std::env::var_os("XDG_STATE_HOME")
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| crate::platform::paths::user_home_dir().join(".local/state"))
        .join("megacube/cmsd.sock")
}

#[cfg(test)]
fn resolve_socket_path(env_val: Option<String>) -> PathBuf {
    env_val
        .filter(|s| !s.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(DEFAULT_SOCKET_PATH))
}

/// 能力门控：socket 存在且 /v1/status 应答 service=="cmsd"。任何一步失败都算没有。
pub fn available() -> bool {
    Client::connect()
        .and_then(|c| c.status())
        .map(|v| v.get("service").and_then(|s| s.as_str()) == Some("cmsd"))
        .unwrap_or(false)
}

pub struct Client {
    socket: PathBuf,
}

impl Client {
    pub(crate) fn bound_socket(&self) -> &std::path::Path {
        &self.socket
    }
    pub(crate) fn at_socket(socket: PathBuf) -> Result<Client, String> {
        if !socket.is_absolute() || !socket.exists() {
            return Err("cmsd_unavailable: 请恢复事务原先连接的设备服务".into());
        }
        Ok(Client { socket })
    }

    pub fn connect() -> Result<Client, String> {
        let socket = socket_path();
        if !socket.exists() {
            return Err(format!(
                "socket_missing: 未检测到 MegaCube 管理面（{} 不存在）",
                socket.display()
            ));
        }
        Ok(Client { socket })
    }

    fn roundtrip(&self, method: &str, path: &str) -> Result<Value, String> {
        self.roundtrip_body(method, path, if method == "POST" { "{}" } else { "" })
    }

    fn roundtrip_body(&self, method: &str, path: &str, body: &str) -> Result<Value, String> {
        #[cfg(unix)]
        {
            let mut stream = super::platform::connect_timeout(&self.socket, CONNECT_TIMEOUT)
                .map_err(|e| format!("connect_failed: {e}"))?;
            if method == "POST" {
                // Persist before any management request can be accepted, even
                // when its response is lost and no model fact was read first.
                super::authority::remember_local_device()?;
            }
            stream
                .set_read_timeout(Some(IO_TIMEOUT))
                .map_err(|e| format!("io_failed: {e}"))?;
            stream
                .set_write_timeout(Some(IO_TIMEOUT))
                .map_err(|e| format!("io_failed: {e}"))?;
            let request = format!(
                "{method} {path} HTTP/1.1\r\nhost: localhost\r\nconnection: close\r\ncontent-length: {}\r\n\r\n{body}",
                body.len()
            );
            stream
                .write_all(request.as_bytes())
                .map_err(|e| format!("io_failed: {e}"))?;
            let mut buf = Vec::new();
            stream
                .read_to_end(&mut buf)
                .map_err(|e| format!("io_failed: {e}"))?;
            parse_http_response(&buf)
        }
        #[cfg(not(unix))]
        {
            let _ = (method, path, body);
            Err("unsupported_platform: MegaCube 管理面（cmsd）只在 Linux 设备上存在".to_string())
        }
    }

    pub fn status(&self) -> Result<Value, String> {
        let value = self.roundtrip("GET", "/v1/status")?;
        if value.get("service").and_then(Value::as_str) == Some("cmsd") {
            super::authority::remember_local_device()?;
        }
        Ok(value)
    }

    pub fn models(&self) -> Result<Value, String> {
        let value = self.roundtrip("GET", "/v1/models")?;
        if value.get("models").is_some_and(Value::is_array)
            && value.get("error").is_none_or(Value::is_null)
            && value
                .get("http_status")
                .and_then(Value::as_u64)
                .is_none_or(|status| status == 200)
        {
            // Observing a device grants no lifecycle authority, but must be
            // remembered before an accepted adoption can outlive this client.
            super::authority::remember_local_device()?;
            if value["store_enrollment"] == 1 {
                let program = std::env::current_exe()
                    .and_then(|p| p.canonicalize())
                    .map_err(|e| e.to_string())?;
                let profile = crate::platform::paths::pinvou3_home();
                if value["store_reader"]["program"]
                    .as_str()
                    .is_none_or(|p| !std::path::Path::new(p).is_file())
                    || value["store_reader"]["profile"].as_str() != profile.to_str()
                {
                    // Existing facts remain readable if enrollment is blocked
                    // by maintenance or an administrator's explicit binding.
                    // Ownership itself is granted later by cmsd after checking
                    // completed standard store records and actual resources.
                    let _ = self.roundtrip_body(
                        "POST",
                        "/v1/store-enrollment",
                        &serde_json::json!({"program":program,"profile":profile}).to_string(),
                    );
                }
            }
        }
        Ok(value)
    }

    pub(crate) fn model_fact(&self, card: &str) -> Result<Option<Value>, String> {
        card_fact_from_response(&self.models()?, card)
    }

    pub(crate) fn supports(&self, capability: &str) -> Result<bool, String> {
        let device = self.device()?;
        if device.get("error").is_some_and(|error| !error.is_null())
            || device
                .get("http_status")
                .and_then(Value::as_u64)
                .is_some_and(|status| status != 200)
        {
            return Err("cmsd_unavailable: 无法核实设备能力，请先恢复设备服务".into());
        }
        match device.get("capabilities") {
            None => Ok(false),
            Some(value) => {
                let capabilities = value
                    .as_array()
                    .ok_or("invalid_response: 设备能力列表无效")?;
                if capabilities.iter().any(|value| !value.is_string()) {
                    return Err("invalid_response: 设备能力列表不完整".into());
                }
                Ok(capabilities
                    .iter()
                    .any(|value| value.as_str() == Some(capability)))
            }
        }
    }

    pub(crate) fn device(&self) -> Result<Value, String> {
        self.roundtrip("GET", "/v1/device")
    }

    pub fn engines(&self) -> Result<Value, String> {
        self.roundtrip("GET", "/v1/engines")
    }

    pub fn capacity(&self) -> Result<Value, String> {
        self.roundtrip("GET", "/v1/capacity")
    }

    pub fn task(&self, task_id: &str) -> Result<Value, String> {
        self.roundtrip("GET", &format!("/v1/tasks/{task_id}"))
    }

    pub fn cancel_task(&self, id: &str) -> Result<Value, String> {
        self.roundtrip("POST", &format!("/v1/tasks/{id}/cancel"))
    }

    pub fn tasks(&self) -> Result<Value, String> {
        self.roundtrip("GET", "/v1/tasks")
    }

    /// Component UI only: durable admission gate, never a model-management fallback.
    pub fn component_maintenance(&self, action: &str, owner: &str) -> Result<Value, String> {
        if !["begin", "end"].contains(&action)
            || owner.is_empty()
            || owner.len() > 128
            || !owner
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b))
        {
            return Err("invalid_component_maintenance".into());
        }
        self.roundtrip_body(
            "POST",
            "/v1/component-maintenance",
            &serde_json::json!({"action":action,"owner":owner}).to_string(),
        )
    }

    /// POST /v1/models/{card}/{action}（enable/disable/purge），返回任务句柄 JSON。
    pub fn model_action(&self, card: &str, action: &str) -> Result<Value, String> {
        validate_maintenance(card, action)?;
        let fact = self.model_fact(card)?;
        let runtime = fact
            .as_ref()
            .and_then(|m| m["card"].as_str())
            .unwrap_or(card);
        self.roundtrip("POST", &format!("/v1/models/{runtime}/{action}"))
    }

    pub(crate) fn retry_install(
        &self,
        card: &str,
        original_run: &str,
        expected_preview: &str,
    ) -> Result<Value, String> {
        validate_maintenance(card, "retry")?;
        if original_run.is_empty()
            || original_run.len() > 160
            || !original_run
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b"_.-".contains(&byte))
            || expected_preview.len() != 64
            || !expected_preview
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        {
            return Err("invalid_installation_reference".into());
        }
        self.roundtrip_body("POST", &format!("/v1/models/{card}/install/retry"),
            &serde_json::json!({"retry_store_run_id":original_run,"expected_preview_sha256":expected_preview}).to_string())
    }

    /// Private desktop transaction credential; never accepted from an LLM tool.
    pub(crate) fn model_maintenance(
        &self,
        card: &str,
        action: &str,
        owner: &str,
    ) -> Result<Value, String> {
        validate_maintenance(card, owner)?;
        if !["begin", "end", "failed"].contains(&action) {
            return Err("invalid_model_maintenance_action".into());
        }
        self.roundtrip_body(
            "POST",
            &format!("/v1/models/{card}/maintenance"),
            &serde_json::json!({"action":action,"owner":owner}).to_string(),
        )
    }

    pub(crate) fn model_maintenance_state(&self, card: &str) -> Result<Value, String> {
        validate_maintenance(card, "query")?;
        self.roundtrip("GET", &format!("/v1/models/{card}/maintenance"))
    }

    pub(crate) fn maintained_action(
        &self,
        card: &str,
        action: &str,
        owner: &str,
    ) -> Result<Value, String> {
        validate_maintenance(card, owner)?;
        if !["disable", "purge"].contains(&action) {
            return Err("invalid_maintained_action".into());
        }
        self.roundtrip_body(
            "POST",
            &format!("/v1/models/{card}/{action}"),
            &serde_json::json!({"maintenance_owner":owner}).to_string(),
        )
    }

    /// Only a valid authoritative model list can prove a card is unmanaged.
    /// Transport, HTTP and schema failures must never grant the legacy executor ownership.
    pub fn manages_card(&self, card_id: &str) -> Result<bool, String> {
        let value = self.models()?;
        managed_card_from_response(&value, card_id)
    }
}

pub(crate) fn validate_card_id(card: &str) -> Result<(), String> {
    if card.is_empty()
        || card.len() > 128
        || matches!(card, "." | "..")
        || !card
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
    {
        return Err("invalid_card".into());
    }
    Ok(())
}

fn validate_maintenance(card: &str, owner: &str) -> Result<(), String> {
    if validate_card_id(card).is_err()
        || owner.is_empty()
        || owner.len() > 128
        || !owner
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b))
    {
        return Err("invalid_model_maintenance".into());
    }
    Ok(())
}

pub(super) fn managed_card_from_response(value: &Value, card_id: &str) -> Result<bool, String> {
    match card_fact_from_response(value, card_id)? {
        Some(model) if model.get("managed").and_then(Value::as_bool) == Some(true) => Ok(true),
        Some(_) => Err("ownership_required: 此资源仅被发现，未授予管理权。请在设备页显式接管并启用，不能回退旧脚本。".into()),
        None => Ok(false),
    }
}

fn card_fact_from_response(value: &Value, card_id: &str) -> Result<Option<Value>, String> {
    if value.get("error").is_some_and(|error| !error.is_null())
        || value
            .get("http_status")
            .and_then(Value::as_u64)
            .is_some_and(|status| status != 200)
    {
        return Err("cmsd_unavailable: 管理权查询失败，不能回退旧脚本".into());
    }
    let models = value
        .get("models")
        .and_then(Value::as_array)
        .ok_or_else(|| "cmsd_unavailable: 无法核实模型管理权，请恢复设备服务后重试".to_string())?;
    if models.iter().any(|model| {
        model
            .get("card")
            .and_then(Value::as_str)
            .is_none_or(str::is_empty)
            || model.get("managed").and_then(Value::as_bool).is_none()
    }) {
        return Err("invalid_response: 模型清单不完整，无法核实管理权".into());
    }
    let bindings = store_bindings(value)?;
    let runtime = bindings.get(card_id).map(String::as_str).unwrap_or(card_id);
    for model in models {
        if model.get("card").and_then(Value::as_str) == Some(runtime) {
            return Ok(Some(model.clone()));
        }
    }
    Ok(None)
}

/// Store aliases bind a logical store row to its installed runtime version.
/// Reject a malformed map instead of choosing an older card with the same name.
pub(super) fn store_bindings(
    value: &Value,
) -> Result<std::collections::BTreeMap<String, String>, String> {
    let Some(bindings) = value.get("store_bindings") else {
        return Ok(Default::default());
    };
    let bindings = bindings
        .as_object()
        .filter(|m| m.len() <= 512)
        .ok_or("invalid_response: store_bindings")?;
    let models = value["models"]
        .as_array()
        .ok_or("invalid_response: models")?;
    let safe = |v: &str| {
        !v.is_empty()
            && v.len() <= 128
            && !matches!(v, "." | "..")
            && v.bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
    };
    let mut out = std::collections::BTreeMap::new();
    for (logical, target) in bindings {
        let target = target
            .as_str()
            .filter(|v| safe(v))
            .ok_or("invalid_response: store_binding")?;
        if !safe(logical)
            || models
                .iter()
                .filter(|m| m["card"].as_str() == Some(target) && m["managed"] == true)
                .count()
                != 1
        {
            return Err("invalid_response: store_binding_target".into());
        }
        out.insert(logical.clone(), target.to_string());
    }
    Ok(out)
}

/// 解析 HTTP/1.1 响应（cmsd 应答带 content-length 且 connection: close）。
/// 纯函数，与传输无关，便于单测。
pub fn parse_http_response(bytes: &[u8]) -> Result<Value, String> {
    let text = String::from_utf8_lossy(bytes);
    let (head, body) = text
        .split_once("\r\n\r\n")
        .ok_or_else(|| "invalid_response: 响应缺少头部终止".to_string())?;
    let status: u16 = head
        .split_whitespace()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .ok_or_else(|| "invalid_response: 状态行无法解析".to_string())?;
    let body = body.trim();
    let parsed: Value =
        serde_json::from_str(body).map_err(|e| format!("invalid_response: 应答体非 JSON：{e}"))?;
    if status != 200 {
        // cmsd 的错误应答也是 JSON，带上 HTTP 状态一起透传
        let mut out = parsed;
        out["http_status"] = Value::from(status);
        return Ok(out);
    }
    Ok(parsed)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn store_runtime_alias_wins_over_an_old_unmanaged_base_card() {
        let response = serde_json::json!({"models":[{"card":"model","managed":false},{"card":"model--v2","managed":true}],"store_bindings":{"model":"model--v2"}});
        assert_eq!(
            card_fact_from_response(&response, "model")
                .unwrap()
                .unwrap()["card"],
            "model--v2"
        );
        assert!(managed_card_from_response(&response, "model").unwrap());
        for invalid in [
            serde_json::json!(null),
            serde_json::json!({"model":"absent"}),
            serde_json::json!({"model":"../escape"}),
        ] {
            let mut value = response.clone();
            value["store_bindings"] = invalid;
            assert!(card_fact_from_response(&value, "model").is_err());
        }
    }

    #[test]
    fn socket_path_env_override_wins() {
        assert_eq!(
            resolve_socket_path(Some("/tmp/x.sock".to_string())),
            PathBuf::from("/tmp/x.sock")
        );
        assert_eq!(
            resolve_socket_path(None),
            PathBuf::from(DEFAULT_SOCKET_PATH)
        );
        assert_eq!(
            resolve_socket_path(Some(String::new())),
            PathBuf::from(DEFAULT_SOCKET_PATH)
        );
    }

    #[test]
    fn parses_200_json() {
        let resp = b"HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: 17\r\n\r\n{\"service\":\"cmsd\"}";
        let v = parse_http_response(resp).unwrap();
        assert_eq!(v["service"], "cmsd");
    }

    #[test]
    fn passes_through_error_json_with_status() {
        let resp = b"HTTP/1.1 404 Not Found\r\ncontent-length: 31\r\n\r\n{\"error\":{\"code\":\"unknown_endpoint\"}}";
        let v = parse_http_response(resp).unwrap();
        assert_eq!(v["error"]["code"], "unknown_endpoint");
        assert_eq!(v["http_status"], 404);
    }

    #[test]
    fn rejects_garbage() {
        assert!(parse_http_response(b"not http").is_err());
        assert!(parse_http_response(b"HTTP/1.1 200 OK\r\n\r\nnot json").is_err());
    }

    #[test]
    fn manages_card_matching() {
        // 纯判定逻辑抽出单测：对 /v1/models 应答形状做匹配
        let client_body = serde_json::json!({
            "models": [
                {"card": "ornith15-35b-a3b-nvfp4", "managed": true},
                {"card": "qwen38-27b-sglang0519", "managed": false},
            ]
        });
        let found = |card: &str| managed_card_from_response(&client_body, card).unwrap();
        assert!(found("ornith15-35b-a3b-nvfp4"));
        assert!(
            managed_card_from_response(&client_body, "qwen38-27b-sglang0519")
                .unwrap_err()
                .contains("ownership_required")
        );
        assert!(!found("no-such-card"));
        assert!(
            managed_card_from_response(&serde_json::json!({"error":{"code":"offline"}}), "x")
                .is_err()
        );
        assert!(
            managed_card_from_response(&serde_json::json!({"models":[{"card":"x"}]}), "x").is_err()
        );
        for response in [
            serde_json::json!({"models":[],"http_status":503}),
            serde_json::json!({"models":[],"error":{"code":"offline"}}),
            serde_json::json!({"models":[null]}),
        ] {
            assert!(
                managed_card_from_response(&response, "absent").is_err(),
                "failed/malformed query cannot prove the card is unmanaged"
            );
        }
    }

    #[test]
    fn maintenance_identifiers_match_device_path_contract() {
        assert!(validate_maintenance("Qwen3.8-model_v1", "private-owner_1").is_ok());
        for card in [".", "..", "../model", "model/enable", "model?owner=x", ""] {
            assert!(validate_maintenance(card, "private-owner").is_err());
        }
        assert!(validate_maintenance(&"m".repeat(129), "private-owner").is_err());
        assert!(validate_maintenance("model", "owner.with.dot").is_err());
    }
}
