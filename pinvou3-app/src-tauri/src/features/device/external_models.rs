//! 外部模型注册表（设计 §4）：`~/.pinvou3/device/external-models.d/*.json`。
//!
//! 职责：
//! - 扫描注册表 + 探测 Ollama `/api/tags`，合并出"本机全部模型"视图；
//! - 登记（写注册表文件），**商店边界**：名字已由模型商店管理的一律拒绝；
//! - 移除（确定性守卫链，防误删）：确认名 → 商店边界 → 权重删除守卫 →
//!   Ollama 删除（失败只警告不阻断）→ 删注册表文件。
//!
//! 权重删除是全文件最危险的动作，所以守卫拆成**纯函数**
//! （`weights_deletion_guard`），输入只有 (家目录, 权重路径)，测试矩阵直接覆盖。

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::{human_probe_error, probe_client, OLLAMA_BASE, PROBE_TIMEOUT};

/// 允许登记的模型类别（与注册表 schema 一致）。
const MODEL_KINDS: &[&str] = &["ollama", "openai_endpoint", "llamacpp", "vllm", "other"];

#[derive(Debug, Clone, Serialize)]
pub struct ExternalModel {
    pub id: String,
    pub name: String,
    pub kind: String,
    pub base_url: Option<String>,
    pub model: Option<String>,
    pub weights_path: Option<String>,
    /// 由可达性探测：available | unreachable
    pub status: String,
    pub registered: bool,
    /// registry | ollama
    pub source: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct ExternalModelsView {
    pub models: Vec<ExternalModel>,
    pub warnings: Vec<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct RemoveOutcome {
    pub removed_registry: bool,
    pub removed_weights: bool,
    pub ollama_deleted: bool,
    pub warnings: Vec<String>,
}

/// 注册表文件 schema（设计 §4）。
#[derive(Debug, Clone, Deserialize, Serialize)]
struct RegistryRecord {
    #[serde(default = "default_schema")]
    #[allow(dead_code)]
    schema: u8,
    #[serde(default)]
    id: String,
    name: String,
    #[serde(default)]
    kind: String,
    #[serde(default)]
    base_url: Option<String>,
    #[serde(default)]
    model: Option<String>,
    #[serde(default)]
    weights_path: Option<String>,
    #[serde(default)]
    installed_at: Option<String>,
    #[serde(default)]
    note: Option<String>,
}

fn default_schema() -> u8 {
    1
}

// ---------------------------------------------------------------- 路径

/// 外部模型注册表目录：`~/.pinvou3/device/external-models.d/`。
pub(crate) fn registry_dir() -> PathBuf {
    crate::platform::paths::pinvou3_home()
        .join("device")
        .join("external-models.d")
}

/// 模型商店权重目录：`~/.pinvou3/model-store/`（商店边界，外部模型不得越界）。
fn model_store_dir() -> PathBuf {
    crate::platform::paths::pinvou3_home().join("model-store")
}

// ---------------------------------------------------------------- 视图

/// 本机全部外部模型：注册表 + Ollama 在线列表（去重已注册者）。
pub async fn get_external_models() -> Result<ExternalModelsView, String> {
    // 注册表目录扫描是阻塞文件 IO（read_dir + 逐文件读 + JSON 解析），而本函数
    // 由 async 命令处理器直接调用：注册文件多或磁盘慢时不能占住 async worker，
    // 整个扫描放 blocking 池。
    let dir = registry_dir();
    let (mut models, mut warnings) = tokio::task::spawn_blocking(move || read_registry(&dir))
        .await
        .map_err(|error| format!("注册表目录扫描失败：{error}"))?;
    dedupe_registry_by_id(&mut models, &mut warnings);

    match fetch_ollama_model_names().await {
        Ok(names) => {
            for model in &mut models {
                if model.kind == "ollama" {
                    model.status = ollama_status(model, &names);
                }
            }
            for name in names {
                let already_listed = models.iter().any(|model| {
                    model.kind == "ollama"
                        && (model.model.as_deref() == Some(name.as_str()) || model.name == name)
                });
                if !already_listed {
                    models.push(ExternalModel {
                        id: format!("ollama-{name}"),
                        name,
                        kind: "ollama".to_string(),
                        base_url: None,
                        model: None,
                        weights_path: None,
                        status: "available".to_string(),
                        registered: false,
                        source: "ollama".to_string(),
                    });
                }
            }
        }
        Err(reason) => {
            // Ollama 不可达不是错误：注册表照常返回，相关模型标 unreachable。
            warnings.push(format!("Ollama 服务不可达：{reason}"));
            for model in &mut models {
                if model.kind == "ollama" {
                    model.status = "unreachable".to_string();
                }
            }
        }
    }
    for model in &mut models {
        if model.kind != "ollama" {
            model.status =
                probe_status_for(&model.kind, model.base_url.as_deref(), model.weights_path.as_deref())
                    .await;
        }
    }
    models.sort_by(|a, b| a.name.to_lowercase().cmp(&b.name.to_lowercase()));
    Ok(ExternalModelsView { models, warnings })
}

fn ollama_status(model: &ExternalModel, names: &[String]) -> String {
    let expected = model.model.as_deref().unwrap_or(model.name.as_str());
    if names.iter().any(|name| name == expected) {
        "available".to_string()
    } else {
        "unreachable".to_string()
    }
}

/// 同一 id 出现在多个注册表文件（手工拷贝/恢复备份的常见产物）→ 保留第一份，
/// 其余计入警告。不静默吞掉，否则用户会疑惑"删了一条怎么还在"。
fn dedupe_registry_by_id(models: &mut Vec<ExternalModel>, warnings: &mut Vec<String>) {
    let mut seen: Vec<String> = Vec::new();
    models.retain(|model| {
        if !model.registered {
            return true;
        }
        if seen.contains(&model.id) {
            warnings.push(format!("注册表存在重复 id（{}），已忽略多余记录", model.id));
            return false;
        }
        seen.push(model.id.clone());
        true
    });
}

// ---------------------------------------------------------------- 登记

/// 登记外部模型：校验 → 商店边界 → 生成 id → 写注册表文件 → 返回视图记录。
pub async fn register_external_model(
    name: String,
    kind: String,
    base_url: Option<String>,
    model: Option<String>,
    weights_path: Option<String>,
) -> Result<ExternalModel, String> {
    let name = name.trim().to_string();
    if name.is_empty() {
        return Err("模型名不能为空".to_string());
    }
    if name.chars().count() > 200 {
        return Err("模型名过长（最多 200 字符）".to_string());
    }
    let kind = kind.trim().to_string();
    if !MODEL_KINDS.contains(&kind.as_str()) {
        return Err(format!(
            "不支持的模型类别“{kind}”，支持：{}",
            MODEL_KINDS.join(" / ")
        ));
    }
    let base_url = normalize_optional(&base_url);
    if let Some(url) = &base_url {
        validate_http_url(url)?;
    }
    if matches!(kind.as_str(), "openai_endpoint" | "vllm" | "llamacpp") && base_url.is_none() {
        return Err(format!("类别“{kind}”需要提供服务地址（base_url）"));
    }
    let model = normalize_optional(&model);
    let weights_path = normalize_optional(&weights_path);

    // 商店边界（纵深防御第二层）：商店管理的名字绝不能登记成外部模型。
    if let Some(store_name) = crate::features::model_store::store_model_name(&name) {
        return Err(format!(
            "“{name}”由模型商店管理（商店记录：{store_name}），不能登记为外部模型；请前往模型商店操作"
        ));
    }

    // 纯校验（名字/类别/URL/商店边界）已在上方完成；剩下的建目录 → 生成 id →
    // 写文件整段是阻塞文件 IO，放 blocking 池，避免占住 async worker。
    // JoinError 也映射进写失败的同一句人话报错。
    let dir = registry_dir();
    let id = tokio::task::spawn_blocking({
        let name = name.clone();
        let kind = kind.clone();
        let base_url = base_url.clone();
        let model = model.clone();
        let weights_path = weights_path.clone();
        move || -> Result<String, String> {
            std::fs::create_dir_all(&dir).map_err(|error| format!("注册表目录创建失败：{error}"))?;
            let id = fresh_record_id(&dir)?;
            let record = RegistryRecord {
                schema: 1,
                id: id.clone(),
                name,
                kind,
                base_url,
                model,
                weights_path,
                installed_at: Some(chrono::Utc::now().to_rfc3339()),
                note: None,
            };
            let json = serde_json::to_string_pretty(&record)
                .map_err(|error| format!("注册表序列化失败：{error}"))?;
            let path = dir.join(format!("{id}.json"));
            std::fs::write(&path, format!("{json}\n"))
                .map_err(|error| format!("注册表写入失败：{error}"))?;
            Ok(id)
        }
    })
    .await
    .map_err(|error| format!("注册表写入失败：{error}"))??;

    let status =
        probe_status_for(&kind, base_url.as_deref(), weights_path.as_deref()).await;
    Ok(ExternalModel {
        id,
        name,
        kind,
        base_url,
        model,
        weights_path,
        status,
        registered: true,
        source: "registry".to_string(),
    })
}

fn normalize_optional(raw: &Option<String>) -> Option<String> {
    raw.as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_string)
}

/// URL 可解析且是 http(s)。
fn validate_http_url(raw: &str) -> Result<(), String> {
    match reqwest::Url::parse(raw) {
        Ok(url) if matches!(url.scheme(), "http" | "https") => Ok(()),
        Ok(url) => Err(format!("服务地址必须是 http(s) 地址：{url}")),
        Err(error) => Err(format!("服务地址无法解析（{raw}）：{error}")),
    }
}

/// 生成未冲突的记录 id：`m-` + 8 位随机 hex。
fn fresh_record_id(dir: &Path) -> Result<String, String> {
    for _ in 0..16 {
        let uuid = uuid::Uuid::new_v4().simple().to_string();
        let id = format!("m-{}", &uuid[..8]);
        if !dir.join(format!("{id}.json")).exists() {
            return Ok(id);
        }
    }
    Err("注册表 id 生成冲突，请重试".to_string())
}

// ---------------------------------------------------------------- 移除

/// 移除外部模型（设计 §4 守卫链，顺序执行，任何一条不过返回人话 Err）。
pub async fn remove_external_model(
    id: String,
    delete_weights: bool,
    confirm_name: String,
) -> Result<RemoveOutcome, String> {
    let mut warnings = Vec::new();
    if !is_safe_id(&id) {
        // 非注册表形状的 id（含 Ollama 在线条目的展示 id）：不是本机登记的模型。
        return Err(format!(
            "“{id}”不是本机登记的外部模型；未注册模型请通过 AI 助手会话移除"
        ));
    }
    let record_path = registry_dir().join(format!("{id}.json"));
    let record = read_record(&record_path)?.ok_or_else(|| {
        format!("未找到该模型的注册记录（{id}），可能已经被移除；请刷新后重试")
    })?;

    // 守卫 1：确认名必须与记录一致（防误删）。
    if confirm_name.trim() != record.name {
        return Err(format!(
            "确认名不匹配：为防误删，请输入模型名“{}”以确认移除",
            record.name
        ));
    }

    // 守卫 2：商店边界——名字命中商店、或权重位于商店目录之下，一律拒绝。
    if let Some(store_name) = crate::features::model_store::store_model_name(&record.name) {
        return Err(format!(
            "“{store_name}”由模型商店管理，请在模型商店中操作，不能从这里移除"
        ));
    }
    if let Some(weights) = record.weights_path.as_deref() {
        if path_is_under(Path::new(weights), &model_store_dir()) {
            return Err(
                "该模型的权重位于模型商店目录（~/.pinvou3/model-store/）之下，由模型商店管理，请在模型商店中操作"
                    .to_string(),
            );
        }
    }

    // 守卫 3：权重目录删除（绝对路径 / 家目录之下 / 深度 ≥ 3 / 存在的目录）。
    // 守卫是纯路径计算，留在 async 侧；只有整目录删除进 blocking 池——大权重
    // 目录的 remove_dir_all 可能跑很久，不能占住 async worker。JoinError 与
    // 删除失败映射进同一句人话报错。
    let mut removed_weights = false;
    if delete_weights {
        match record.weights_path.as_deref() {
            None => warnings.push("该记录未登记权重路径，已跳过权重删除".to_string()),
            Some(weights) => {
                let target = weights_deletion_guard(
                    &crate::platform::paths::user_home_dir(),
                    Path::new(weights),
                )?;
                let display = target.display().to_string();
                tokio::task::spawn_blocking(move || std::fs::remove_dir_all(&target))
                    .await
                    .map_err(|error| format!("权重目录删除失败（{display}）：{error}"))?
                    .map_err(|error| format!("权重目录删除失败（{display}）：{error}"))?;
                removed_weights = true;
            }
        }
    }

    // 守卫 4：Ollama 模型顺带从 Ollama 里删；失败只警告，不阻断注册表清理。
    let mut ollama_deleted = false;
    if record.kind == "ollama" {
        // 记录缺 model 字段时回退按 name 删（登记端点类别名时二者可能不同）。
        let model_name = record.model.clone().unwrap_or_else(|| record.name.clone());
        match ollama_delete(&model_name).await {
            Ok(()) => ollama_deleted = true,
            Err(reason) => warnings.push(format!(
                "Ollama 删除失败（不影响本机移除，可手动处理）：{reason}"
            )),
        }
    }

    // 第 5 步：删注册表文件。
    let removed_registry = match std::fs::remove_file(&record_path) {
        Ok(()) => true,
        Err(error) => {
            warnings.push(format!("注册表文件删除失败：{error}"));
            false
        }
    };
    Ok(RemoveOutcome {
        removed_registry,
        removed_weights,
        ollama_deleted,
        warnings,
    })
}

/// 权重删除守卫（纯函数，测试矩阵直打）。全部通过时返回**规范化后**的目标目录，
/// 调用方只对该返回值执行 `remove_dir_all`——绝不删除入参原样路径。
/// 深度按"家目录之下的相对层级"计（≥ 3 = 至少是家目录的孙目录）：直接删家目录的
/// 一级子目录（往往是用户自己整理的目录）风险过高，必须再往深一层才放行。
fn weights_deletion_guard(home: &Path, weights_path: &Path) -> Result<PathBuf, String> {
    if !weights_path.is_absolute() {
        return Err(format!(
            "拒绝删除：权重路径必须是绝对路径（收到：{}）",
            weights_path.display()
        ));
    }
    let target = weights_path
        .canonicalize()
        .map_err(|_| format!("拒绝删除：权重目录不存在（{}）", weights_path.display()))?;
    if !target.is_dir() {
        return Err(format!("拒绝删除：权重路径不是目录（{}）", target.display()));
    }
    let home_canonical = home
        .canonicalize()
        .map_err(|_| "拒绝删除：无法解析家目录".to_string())?;
    if target == home_canonical {
        return Err("拒绝删除：目标路径就是家目录本身".to_string());
    }
    let relative = target
        .strip_prefix(&home_canonical)
        .map_err(|_| {
            format!(
                "拒绝删除：权重路径必须位于家目录（{}）之下",
                home_canonical.display()
            )
        })?
        .to_path_buf();
    let depth = relative.components().count();
    if depth < 3 {
        return Err(format!(
            "拒绝删除：路径层级过浅（家目录下仅 {depth} 层，要求至少 3 层），疑似误选目录"
        ));
    }
    Ok(target)
}

/// 路径包含判断：原样前缀与规范化前缀都试（商店目录可能尚未建出/含符号链接）。
fn path_is_under(path: &Path, dir: &Path) -> bool {
    if path.starts_with(dir) {
        return true;
    }
    let resolved = path.canonicalize().unwrap_or_else(|_| path.to_path_buf());
    if resolved.starts_with(dir) {
        return true;
    }
    dir.canonicalize()
        .map(|dir_canonical| resolved.starts_with(dir_canonical))
        .unwrap_or(false)
}

/// id 只允许字母数字、`-`、`_`（防路径拼接注入）。
fn is_safe_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 64
        && id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'_')
}

// ---------------------------------------------------------------- 注册表 IO

/// 读单个注册表文件。文件缺失 → Ok(None)；JSON 损坏/字段缺失 → Err（上层进 warnings）。
fn read_record(path: &Path) -> Result<Option<RegistryRecord>, String> {
    if !path.is_file() {
        return Ok(None);
    }
    let text = std::fs::read_to_string(path)
        .map_err(|error| format!("文件读取失败：{error}"))?;
    parse_record(&text).map(Some)
}

fn parse_record(text: &str) -> Result<RegistryRecord, String> {
    let value: Value = serde_json::from_str(text).map_err(|error| format!("JSON 无法解析：{error}"))?;
    let record: RegistryRecord = serde_json::from_value(value)
        .map_err(|error| format!("字段不符合注册表 schema：{error}"))?;
    if record.name.trim().is_empty() {
        return Err("缺少模型名（name）".to_string());
    }
    Ok(record)
}

fn read_registry(dir: &Path) -> (Vec<ExternalModel>, Vec<String>) {
    let mut models = Vec::new();
    let mut warnings = Vec::new();
    let Ok(entries) = std::fs::read_dir(dir) else {
        // 目录不存在 = 还没有任何外部模型，属正常首跑。
        return (models, warnings);
    };
    let mut paths: Vec<PathBuf> = entries
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .filter(|path| path.extension().and_then(|ext| ext.to_str()) == Some("json"))
        .collect();
    paths.sort();
    for path in paths {
        match read_record(&path) {
            Ok(Some(record)) => {
                let id = if record.id.trim().is_empty() {
                    path.file_stem()
                        .and_then(|stem| stem.to_str())
                        .unwrap_or("m-unknown")
                        .to_string()
                } else {
                    record.id.clone()
                };
                models.push(ExternalModel {
                    id,
                    name: record.name,
                    kind: if record.kind.is_empty() {
                        "other".to_string()
                    } else {
                        record.kind
                    },
                    base_url: record.base_url,
                    model: record.model,
                    weights_path: record.weights_path,
                    status: "unreachable".to_string(),
                    registered: true,
                    source: "registry".to_string(),
                });
            }
            Ok(None) => {}
            Err(reason) => warnings.push(format!(
                "跳过无法解析的注册表文件（{}）：{reason}",
                path.display()
            )),
        }
    }
    (models, warnings)
}

// ---------------------------------------------------------------- 探测

/// Ollama `/api/tags` 的模型名列表（Ollama 不可达时 Err）。
pub(crate) async fn fetch_ollama_model_names() -> Result<Vec<String>, String> {
    let client = probe_client().ok_or_else(|| "HTTP 客户端初始化失败".to_string())?;
    let response = client
        .get(format!("{OLLAMA_BASE}/api/tags"))
        .timeout(PROBE_TIMEOUT)
        .send()
        .await
        .map_err(human_probe_error)?;
    if !response.status().is_success() {
        return Err(format!("服务响应异常（HTTP {}）", response.status().as_u16()));
    }
    let value: Value = response
        .json()
        .await
        .map_err(|error| format!("Ollama 返回内容无法解析：{error}"))?;
    Ok(value
        .get("models")
        .and_then(Value::as_array)
        .map(|models| {
            models
                .iter()
                .filter_map(|model| {
                    model
                        .get("name")
                        .and_then(Value::as_str)
                        .map(str::to_string)
                })
                .collect()
        })
        .unwrap_or_default())
}

/// Ollama 删除：`DELETE /api/delete {"name": model}`。
async fn ollama_delete(model: &str) -> Result<(), String> {
    let client = probe_client().ok_or_else(|| "HTTP 客户端初始化失败".to_string())?;
    let response = client
        .delete(format!("{OLLAMA_BASE}/api/delete"))
        .json(&serde_json::json!({ "name": model }))
        .timeout(PROBE_TIMEOUT)
        .send()
        .await
        .map_err(human_probe_error)?;
    if response.status().is_success() {
        Ok(())
    } else {
        Err(format!("服务响应异常（HTTP {}）", response.status().as_u16()))
    }
}

/// 非 Ollama 模型的可达性：端点类探 `{base_url}/models`，权重类看目录是否存在。
async fn probe_status_for(
    kind: &str,
    base_url: Option<&str>,
    weights_path: Option<&str>,
) -> String {
    match kind {
        "openai_endpoint" | "vllm" | "llamacpp" => {
            match base_url.map(endpoint_models_url) {
                Some(url) => match super::http_get_success(&url).await {
                    Ok(()) => "available".to_string(),
                    Err(_) => "unreachable".to_string(),
                },
                None => "unreachable".to_string(),
            }
        }
        "ollama" => "unreachable".to_string(),
        _ => match weights_path {
            Some(weights) if Path::new(weights).is_dir() => "available".to_string(),
            _ => "unreachable".to_string(),
        },
    }
}

/// `http://host:port/v1` → `http://host:port/v1/models`（已带 /models 则原样）。
fn endpoint_models_url(base_url: &str) -> String {
    let trimmed = base_url.trim_end_matches('/');
    if trimmed.ends_with("/models") {
        trimmed.to_string()
    } else {
        format!("{trimmed}/models")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 测试专用的临时目录（进程内唯一），Drop 由调用方显式清理。
    struct TempDir(PathBuf);

    impl TempDir {
        fn new(label: &str) -> Self {
            let dir = std::env::temp_dir().join(format!(
                "pinvou3-device-{label}-{}-{}",
                std::process::id(),
                uuid::Uuid::new_v4().simple()
            ));
            std::fs::create_dir_all(&dir).expect("create temp dir");
            TempDir(dir)
        }

        fn path(&self) -> &Path {
            &self.0
        }
    }

    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    fn parse_record_reads_full_schema() {
        let text = r#"{
            "schema": 1, "id": "m-ab12cd34", "name": "qwen3-30b", "kind": "ollama",
            "base_url": "http://127.0.0.1:11434/v1", "model": "qwen3:30b",
            "weights_path": "/home/u/.ollama/models/qwen3",
            "installed_at": "2026-10-09T00:00:00+00:00", "note": ""
        }"#;
        let record = parse_record(text).expect("valid record");
        assert_eq!(record.id, "m-ab12cd34");
        assert_eq!(record.name, "qwen3-30b");
        assert_eq!(record.kind, "ollama");
        assert_eq!(record.model.as_deref(), Some("qwen3:30b"));
    }

    #[test]
    fn parse_record_defaults_optional_fields() {
        let record =
            parse_record(r#"{"name": "tiny", "kind": "other"}"#).expect("minimal record");
        assert_eq!(record.id, "");
        assert_eq!(record.base_url, None);
        assert_eq!(record.weights_path, None);
    }

    #[test]
    fn parse_record_rejects_broken_files() {
        assert!(parse_record("not json at all").is_err());
        // 缺 name（schema 必填）。
        assert!(parse_record(r#"{"id": "m-1"}"#).is_err());
        assert!(parse_record(r#"{"name": ""}"#).is_err());
        assert!(parse_record(r#"[]"#).is_err());
    }

    #[test]
    fn weights_deletion_guard_accepts_deep_absolute_dir_under_home() {
        let home = TempDir::new("guard-home");
        let weights = home.path().join(".ollama").join("models").join("qwen3");
        std::fs::create_dir_all(&weights).expect("create weights dir");
        let target =
            weights_deletion_guard(home.path(), &weights).expect("guard must pass");
        assert_eq!(
            target,
            weights.canonicalize().expect("weights canonicalize")
        );
    }

    #[test]
    fn weights_deletion_guard_rejects_the_guard_matrix() {
        let home = TempDir::new("guard-matrix");
        let deep = home.path().join("a").join("b").join("c");
        std::fs::create_dir_all(&deep).expect("create weights dir");

        // 相对路径。
        let relative = Path::new("relative/weights/dir");
        assert!(weights_deletion_guard(home.path(), relative).is_err());
        // 不存在的目录。
        assert!(weights_deletion_guard(home.path(), &home.path().join("no/such/dir")).is_err());
        // 普通文件（不是目录）。
        let file = home.path().join("file.txt");
        std::fs::write(&file, b"x").expect("write file");
        assert!(weights_deletion_guard(home.path(), &file).is_err());
        // 家目录本身。
        assert!(weights_deletion_guard(home.path(), home.path()).is_err());
        // 家目录之外。
        let outside = TempDir::new("guard-outside");
        let outside_weights = outside.path().join("x").join("y").join("z");
        std::fs::create_dir_all(&outside_weights).expect("create outside dir");
        assert!(weights_deletion_guard(home.path(), &outside_weights).is_err());
    }

    #[test]
    fn weights_deletion_guard_rejects_shallow_dirs_under_home() {
        // 相对深度 1/2（家目录的一级、二级子目录）必须拒绝。
        let home = TempDir::new("guard-depth");
        let child = home.path().join("weights-dir");
        std::fs::create_dir_all(&child).expect("create child");
        assert!(weights_deletion_guard(home.path(), &child).is_err());
        let grandchild = child.join("sub");
        std::fs::create_dir_all(&grandchild).expect("create grandchild");
        assert!(weights_deletion_guard(home.path(), &grandchild).is_err());
        // 孙目录（深度 3）放行。
        let great = grandchild.join("blobs");
        std::fs::create_dir_all(&great).expect("create great");
        assert!(weights_deletion_guard(home.path(), &great).is_ok());
    }

    #[test]
    fn path_is_under_covers_raw_and_canonical_forms() {
        let store = TempDir::new("store-dir");
        let inside = store.path().join("models").join("qwen3");
        std::fs::create_dir_all(&inside).expect("create inside");
        let outside = TempDir::new("store-outside");
        // 原样前缀。
        assert!(path_is_under(&inside, store.path()));
        // 规范化后命中（传入 .. 拼接路径）。
        let dotted = store.path().join("models").join("..").join("models").join("qwen3");
        assert!(path_is_under(&dotted, store.path()));
        assert!(!path_is_under(outside.path(), store.path()));
    }

    #[test]
    fn safe_id_rejects_path_traversal_and_empty() {
        assert!(is_safe_id("m-ab12cd34"));
        assert!(is_safe_id("ollama-qwen3_30b"));
        // 冒号/斜杠/点段/中文一律拒绝（id 直接拼进文件名）。
        assert!(!is_safe_id("ollama-qwen3:30b"));
        assert!(!is_safe_id("../../etc/passwd"));
        assert!(!is_safe_id("a/b"));
        assert!(!is_safe_id(""));
        assert!(!is_safe_id("白名单以外的中文"));
    }

    #[test]
    fn url_validation_requires_http_s() {
        assert!(validate_http_url("http://127.0.0.1:11434/v1").is_ok());
        assert!(validate_http_url("https://api.example.com/v1").is_ok());
        assert!(validate_http_url("ftp://127.0.0.1:21").is_err());
        assert!(validate_http_url("file:///etc/passwd").is_err());
        assert!(validate_http_url("not a url").is_err());
    }

    #[test]
    fn endpoint_models_url_appends_models_once() {
        assert_eq!(
            endpoint_models_url("http://127.0.0.1:8080/v1"),
            "http://127.0.0.1:8080/v1/models"
        );
        assert_eq!(
            endpoint_models_url("http://127.0.0.1:8080/v1/"),
            "http://127.0.0.1:8080/v1/models"
        );
        assert_eq!(
            endpoint_models_url("http://127.0.0.1:8080/v1/models"),
            "http://127.0.0.1:8080/v1/models"
        );
    }

    #[test]
    fn dedupe_keeps_first_record_and_warns() {
        let make = |id: &str| ExternalModel {
            id: id.to_string(),
            name: "same".to_string(),
            kind: "other".to_string(),
            base_url: None,
            model: None,
            weights_path: None,
            status: "unreachable".to_string(),
            registered: true,
            source: "registry".to_string(),
        };
        let mut models = vec![make("m-1"), make("m-2"), make("m-1")];
        // 一条 ollama 在线条目（registered=false）不参与去重。
        models.push(ExternalModel {
            registered: false,
            source: "ollama".to_string(),
            ..make("m-1")
        });
        let mut warnings = Vec::new();
        dedupe_registry_by_id(&mut models, &mut warnings);
        assert_eq!(models.len(), 3);
        assert_eq!(models[0].id, "m-1");
        assert_eq!(warnings.len(), 1);
    }

    #[test]
    fn ollama_status_matches_model_then_name() {
        let mut model = ExternalModel {
            id: "m-1".to_string(),
            name: "qwen3-30b".to_string(),
            kind: "ollama".to_string(),
            base_url: None,
            model: Some("qwen3:30b".to_string()),
            weights_path: None,
            status: String::new(),
            registered: true,
            source: "registry".to_string(),
        };
        assert_eq!(ollama_status(&model, &["qwen3:30b".to_string()]), "available");
        assert_eq!(ollama_status(&model, &["other:8b".to_string()]), "unreachable");
        model.model = None;
        // 无 model 字段时回退按 name 匹配。
        assert_eq!(ollama_status(&model, &["qwen3-30b".to_string()]), "available");
    }

    #[test]
    fn read_registry_skips_bad_files_into_warnings() {
        let dir = TempDir::new("registry");
        std::fs::write(
            dir.path().join("m-good.json"),
            r#"{"id": "m-good", "name": "good", "kind": "other"}"#,
        )
        .expect("write good record");
        std::fs::write(dir.path().join("m-bad.json"), "{ broken").expect("write bad record");
        std::fs::write(dir.path().join("notes.txt"), "ignore me").expect("write non-json");
        let (models, warnings) = read_registry(dir.path());
        assert_eq!(models.len(), 1);
        assert_eq!(models[0].id, "m-good");
        assert_eq!(warnings.len(), 1);
        assert!(warnings[0].contains("m-bad.json"));
    }
}
