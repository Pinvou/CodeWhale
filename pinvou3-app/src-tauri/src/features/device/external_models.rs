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

use std::time::Duration;

use super::{PROBE_TIMEOUT, human_probe_error, probe_client};

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

/// 商店边界（路径臂）：权重路径位于**任一**商店权重根之下即命中。根列表由
/// `model_store::store_weights_roots()` 提供（默认 store 数据根 + 设置里可自定义
/// 的 `models_dir`）——设备侧不得自行推断默认值，否则商店改了权重根这里就会漂移。
fn weights_in_store(weights: &str) -> bool {
    path_under_any(
        Path::new(weights),
        &crate::features::model_store::store_weights_roots(),
    )
}

/// 路径包含判断（多根版，测试直打）：任一根命中即 true。
fn path_under_any(path: &Path, roots: &[PathBuf]) -> bool {
    roots.iter().any(|root| path_is_under(path, root))
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
    // 非 Ollama 模型并发探测：每个不可达端点要吃满 1.5s 超时，串行会让 N 个
    // 失效端点把整次刷新拖到 N×1.5s；join_all 并发后总耗时 ≈ 最慢的一个。
    let probe_indices: Vec<usize> = models
        .iter()
        .enumerate()
        .filter(|(_, model)| model.kind != "ollama")
        .map(|(index, _)| index)
        .collect();
    let probes = futures_util::future::join_all(probe_indices.iter().map(|&index| {
        let model = &models[index];
        probe_status_for(
            &model.kind,
            model.base_url.as_deref(),
            model.weights_path.as_deref(),
        )
    }))
    .await;
    for (&index, status) in probe_indices.iter().zip(probes) {
        models[index].status = status;
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
/// （去重主逻辑在 read_registry 的 resolve_duplicate_ids：优先保留文件名与 id
/// 一致的副本；本函数保留给直接构造模型的测试与未来非注册表来源复用。）
#[cfg(test)]
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
    // 字符串字段限长：read_record 对 >64KB 的文件整条拒绝，一个超长字段会登记
    // 出"写进去就永远读不出来"的僵尸记录（第四轮评审 P3）。上限给足宽
    // （4096 ≈ PATH_MAX，URL/标识远用不到这么多），正常输入无感。
    for (label, value) in [
        ("服务地址", base_url.as_deref()),
        ("模型标识", model.as_deref()),
        ("权重路径", weights_path.as_deref()),
    ] {
        if value.is_some_and(|value| value.chars().count() > 4096) {
            return Err(format!("{label}过长（最多 4096 字符）"));
        }
    }

    // Ollama 记录实际删除的目标名：登记端点类别名时 model ≠ name（移除同一口径），
    // 商店边界必须对**有效删除名**检查——只查 name 会漏掉"别名指向商店管理的
    // Ollama 模型"这条路（第三轮评审 P1）。
    let effective_ollama_name =
        (kind == "ollama").then(|| model.clone().unwrap_or_else(|| name.clone()));

    // 商店边界（纵深防御第二层）：商店管理的名字/有效删除名绝不能登记成外部模型。
    // 商店名单查询（sqlite connect）与权重根收集（读 settings）是阻塞 IO，先在
    // blocking 池里把三个边界事实一次查全，再走纯校验，不占 async worker。
    let (name_hit, model_hit, weights_hit) = tokio::task::spawn_blocking({
        let name = name.clone();
        let effective_ollama_name = effective_ollama_name.clone();
        let weights_path = weights_path.clone();
        move || {
            // 登记端维持宽松口径（查询出错按未命中），但 Ollama 有效删除名臂
            // 同样大小写不敏感：早一步给出"由模型商店管理"的明确报错，好过登记
            // 成一条之后永远移除不掉的记录。
            let name_hit = crate::features::model_store::store_model_name(&name);
            let model_hit = effective_ollama_name.as_deref().and_then(|name| {
                crate::features::model_store::store_model_name_checked(name, true)
                    .ok()
                    .flatten()
            });
            let weights_hit = weights_path
                .as_deref()
                .map(weights_in_store)
                .unwrap_or(false);
            (name_hit, model_hit, weights_hit)
        }
    })
    .await
    .map_err(|error| format!("商店边界查询失败：{error}"))?;
    if let Some(store_name) = name_hit {
        return Err(format!(
            "“{name}”由模型商店管理（商店记录：{store_name}），不能登记为外部模型；请前往模型商店操作"
        ));
    }
    if let Some(store_name) = model_hit {
        return Err(format!(
            "模型标识“{}”由模型商店管理（商店记录：{store_name}），不能登记为外部模型；请前往模型商店操作",
            effective_ollama_name.unwrap_or_default()
        ));
    }
    // 商店边界（路径臂）：权重位于任一商店权重根（含自定义 models_dir）之下，
    // 与名字臂同等拒绝——否则商店权重可被登记成外部模型后经设备管理删除。
    if weights_hit {
        return Err(format!(
            "“{}”位于模型商店目录之下，由模型商店管理，不能登记为外部模型；请前往模型商店操作",
            weights_path.unwrap_or_default()
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
            std::fs::create_dir_all(&dir)
                .map_err(|error| format!("注册表目录创建失败：{error}"))?;
            // 注册表记录含 base_url/weights_path，目录只留 owner 权限（对齐
            // model_store 的私密纪律；unix 专属实现收在 platform 适配层）。
            super::platform::harden_dir_owner_only(&dir);
            for _ in 0..16 {
                let id = fresh_record_id(&dir)?;
                let record = RegistryRecord {
                    schema: 1,
                    id: id.clone(),
                    name: name.clone(),
                    kind: kind.clone(),
                    base_url: base_url.clone(),
                    model: model.clone(),
                    weights_path: weights_path.clone(),
                    installed_at: Some(chrono::Utc::now().to_rfc3339()),
                    note: None,
                };
                let json = serde_json::to_string_pretty(&record)
                    .map_err(|error| format!("注册表序列化失败：{error}"))?;
                // temp 文件 + rename 原子落盘：进程崩溃不留半截 JSON（半截文件会在
                // 下次扫描时被跳过并告警，用户看到的是模型凭空消失）。tmp 用
                // create_new 独占创建（并发登记撞 id 时换一个重来，不静默覆盖先写
                // 记录），rename 前 sync，掉电不留空文件；文件 0600 只留 owner。
                let path = dir.join(format!("{id}.json"));
                let tmp = dir.join(format!("{id}.json.tmp"));
                let mut file = match std::fs::OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .open(&tmp)
                {
                    Ok(file) => file,
                    Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
                    Err(error) => return Err(format!("注册表写入失败：{error}")),
                };
                write_record_file(&mut file, json.as_bytes())?;
                std::fs::rename(&tmp, &path).map_err(|error| format!("注册表写入失败：{error}"))?;
                return Ok(id);
            }
            Err("注册表 id 生成冲突，请重试".to_string())
        }
    })
    .await
    .map_err(|error| format!("注册表写入失败：{error}"))??;

    let status = probe_status_for(&kind, base_url.as_deref(), weights_path.as_deref()).await;
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

/// 写入 + 落盘前 sync + 文件 0600（记录含 base_url/weights_path，只留 owner；
/// unix 专属实现收在 platform 适配层）。
fn write_record_file(file: &mut std::fs::File, bytes: &[u8]) -> Result<(), String> {
    use std::io::Write;
    super::platform::set_file_owner_only(file);
    file.write_all(bytes)
        .and_then(|_| file.write_all(b"\n"))
        .and_then(|_| file.sync_all())
        .map_err(|error| format!("注册表写入失败：{error}"))
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
    // 读注册记录：先按 `<id>.json` 直取；文件名与 id 不一致的存量记录（早期
    // 模板教 AI 用 `<slug>.json` 命名，第三轮评审 P1）有界扫描按**内容 id** 兜底
    // 命中——UI 展示的 id 就是内容 id，按它必须能找到文件，否则记录成僵尸。
    let dir = registry_dir();
    let loaded = tokio::task::spawn_blocking({
        let dir = dir.clone();
        let id = id.clone();
        move || load_record_for_removal(&dir, &id)
    })
    .await
    .map_err(|error| format!("注册表读取失败（{error}）"))??;
    let (record, record_path) = loaded
        .ok_or_else(|| format!("未找到该模型的注册记录（{id}），可能已经被移除；请刷新后重试"))?;

    // Ollama 记录实际删除的目标名（与登记端同一口径）：商店边界必须对它检查，
    // 否则"别名指向商店管理的 Ollama 模型"可绕过名字臂把商店模型从守护进程里
    // 删掉（第三轮评审 P1）。
    let ollama_model_name = (record.kind == "ollama")
        .then(|| record.model.clone().unwrap_or_else(|| record.name.clone()));

    // 守卫 1 + 守卫 2：确认名一致、名字（及 Ollama 有效删除名）不在商店名单、
    // 权重不在任一商店根之下。商店名单查询（sqlite connect）与权重根收集（读
    // settings）是阻塞 IO，先在 blocking 池里把边界事实一次查全，再走纯函数。
    // 移除路径 **fail-closed**（第四轮评审 P2）：查询出错按 Err 拒绝，不当未
    // 命中放行——瞬时 SQLITE_BUSY（恰在商店部署/删除高峰最常见）若放行，别名
    // 记录就能对着商店管理的 Ollama 模型执行真 DELETE。登记端维持宽松口径
    // （登记错最多产生一条之后删不掉的记录，移除错会真删商店模型）。
    let boundary = tokio::task::spawn_blocking({
        let record_name = record.name.clone();
        let weights_path = record.weights_path.clone();
        let ollama_model_name = ollama_model_name.clone();
        move || {
            let name_hit =
                crate::features::model_store::store_model_name_checked(&record_name, false)?;
            let ollama_model_hit = match &ollama_model_name {
                // Ollama 有效删除名按大小写不敏感对齐：daemon 标签归一小写，
                // BINARY 精确匹配会漏掉 "Qwen3:30B" → "qwen3:30b" 的别名（第四轮 P3）。
                Some(name) => crate::features::model_store::store_model_name_checked(name, true)?,
                None => None,
            };
            let weights_hit = weights_path
                .as_deref()
                .map(weights_in_store)
                .unwrap_or(false);
            Ok((name_hit, ollama_model_hit, weights_hit))
        }
    })
    .await
    .map_err(|error| format!("商店边界查询失败：{error}"))?
    .map_err(|error: String| {
        format!("{error}；为防误删商店管理的模型，本次移除已被拒绝，请稍后重试")
    })?;
    let (name_hit, ollama_model_hit, weights_hit) = boundary;
    removal_boundary_error(
        &record.name,
        &confirm_name,
        weights_hit,
        name_hit.as_deref(),
        ollama_model_hit.as_deref(),
    )?;

    // 守卫 3：权重目录删除（绝对路径 / 家目录之下 / 深度 ≥ 3 / 存在的目录 /
    // 不在应用自身数据树内）。守卫是纯路径计算，留在 async 侧；只有整目录删除
    // 进 blocking 池——大权重目录的 remove_dir_all 可能跑很久，不能占住 async
    // worker。JoinError 与删除失败映射进同一句人话报错。
    let mut removed_weights = false;
    if delete_weights {
        match record.weights_path.as_deref() {
            None => warnings.push("该记录未登记权重路径，已跳过权重删除".to_string()),
            // 权重目录已不存在（上次删除成功但记录文件没删掉等）：按已删除处理，
            // 不让重试整单报错卡死收尾。
            Some(weights) if !Path::new(weights).exists() => warnings.push(format!(
                "登记的权重目录不存在（可能已删除过），已跳过：{weights}"
            )),
            Some(weights) => {
                let home = crate::platform::paths::user_home_dir();
                let protected = crate::platform::paths::pinvou3_home();
                let weights = weights.to_string();
                let weights_for_task = weights.clone();
                tokio::task::spawn_blocking(move || -> Result<PathBuf, String> {
                    // 守卫（含 canonicalize）与整目录删除同一池执行：canonicalize
                    // 在陈旧 NFS/FUSE 挂载上可能阻塞数秒，不能占住 async worker
                    // （第四轮评审 P3；其余 IO 此前已全部入池）。
                    let target = weights_deletion_guard(
                        &home,
                        Some(&protected),
                        Path::new(&weights_for_task),
                    )?;
                    std::fs::remove_dir_all(&target).map_err(|error| {
                        format!("权重目录删除失败（{}）：{error}", target.display())
                    })?;
                    Ok(target)
                })
                .await
                .map_err(|error| format!("权重目录删除失败（{weights}）：{error}"))??;
                removed_weights = true;
            }
        }
    }

    // 守卫 4：Ollama 模型顺带从 Ollama 里删；失败只警告，不阻断注册表清理。
    let mut ollama_deleted = false;
    if let Some(model_name) = &ollama_model_name {
        match ollama_delete(model_name).await {
            Ok(()) => ollama_deleted = true,
            Err(reason) => warnings.push(format!(
                "Ollama 删除失败（不影响本机移除，可手动处理）：{reason}"
            )),
        }
    }

    // 第 5 步：删注册表文件（文件 IO，进 blocking 池，与读盘同一纪律）。
    let removed_registry = match tokio::task::spawn_blocking({
        let record_path = record_path.clone();
        move || std::fs::remove_file(&record_path)
    })
    .await
    {
        Ok(Ok(())) => true,
        Ok(Err(error)) => {
            warnings.push(format!("注册表文件删除失败：{error}"));
            false
        }
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

/// 守卫 1 + 守卫 2 的纯逻辑（测试矩阵直打）：确认名一致 → 名字未由商店管理 →
/// Ollama 有效删除名未由商店管理（别名记录不许把商店模型从守护进程里删掉）→
/// 权重不在任一商店权重根之下。商店名单/权重根的查询结果由调用方先在 blocking
/// 池算好传入，这里保持纯计算、可测试矩阵直打。
fn removal_boundary_error(
    record_name: &str,
    confirm_name: &str,
    weights_in_store: bool,
    store_name_hit: Option<&str>,
    ollama_model_store_hit: Option<&str>,
) -> Result<(), String> {
    // 双侧都 trim：登记端写入的名字已 trim，但 AI/手写的存量记录可能带空白，
    // 只 trim 输入侧会让这条记录怎么输都删不掉。
    if confirm_name.trim() != record_name.trim() {
        return Err(format!(
            "确认名不匹配：为防误删，请输入模型名“{record_name}”以确认移除"
        ));
    }
    if let Some(store_name) = store_name_hit {
        return Err(format!(
            "“{store_name}”由模型商店管理，请在模型商店中操作，不能从这里移除"
        ));
    }
    if let Some(store_name) = ollama_model_store_hit {
        return Err(format!(
            "该记录的 Ollama 模型“{store_name}”由模型商店管理，请在模型商店中操作，不能从这里移除"
        ));
    }
    if weights_in_store {
        return Err(
            "该模型的权重位于模型商店目录之下，由模型商店管理，请在模型商店中操作".to_string(),
        );
    }
    Ok(())
}

/// 权重删除守卫（纯函数，测试矩阵直打）。全部通过时返回**规范化后**的目标目录，
/// 调用方只对该返回值执行 `remove_dir_all`——绝不删除入参原样路径。
/// 深度按"家目录之下的相对层级"计（≥ 3 = 至少是家目录的孙目录）：直接删家目录的
/// 一级子目录（往往是用户自己整理的目录）风险过高，必须再往深一层才放行。
/// `protected_root`（应用自身数据根 `~/.pinvou3`）整体拒绝：注册表目录恰好 3 层、
/// bundle/skills、runtimes 等更深目录都在放行深度内，不显式排除就存在"删除权重"
/// 按钮抹掉应用配置/知识库的通道（第三轮评审 P2）。
pub(crate) fn weights_deletion_guard(
    home: &Path,
    protected_root: Option<&Path>,
    weights_path: &Path,
) -> Result<PathBuf, String> {
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
        return Err(format!(
            "拒绝删除：权重路径不是目录（{}）",
            target.display()
        ));
    }
    if let Some(protected) = protected_root {
        if let Ok(protected_canonical) = protected.canonicalize() {
            if target == protected_canonical || target.starts_with(&protected_canonical) {
                return Err(format!(
                    "拒绝删除：目标位于应用数据目录（{}）之内，请到设置或模型商店操作",
                    protected_canonical.display()
                ));
            }
        }
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

/// 移除时的记录加载：`<id>.json` 直取（内容 id 与文件名不一致的错位文件不认，
/// 除非内容 id 为空的存量记录）；缺失时有界扫描按**内容 id** 兜底（消化文件名
/// 与 id 不一致的存量记录）。返回命中的记录与其实际文件路径（第 5 步删的就是
/// 这个路径）。扫描对坏邻居文件（坏 JSON/超限）跳过而不是让整个移除失败——
/// 与 `read_registry` 的降级口径一致，否则一个损坏文件会吊死所有健康记录的
/// 移除（第四轮评审 P2）。
fn load_record_for_removal(
    dir: &Path,
    id: &str,
) -> Result<Option<(RegistryRecord, PathBuf)>, String> {
    let direct = dir.join(format!("{id}.json"));
    if let Some(record) = read_record(&direct)? {
        // 直取也要求内容 id 对得上（容忍存量空 id）：`a.json` 内容 id 是 `m-2`
        // 而 `m-1` 命中直取时，破坏性动作会用错那份记录的权重路径/模型名。
        let content_id = record.id.trim();
        if content_id == id || content_id.is_empty() {
            return Ok(Some((record, direct)));
        }
    }
    let Ok(entries) = std::fs::read_dir(dir) else {
        return Ok(None);
    };
    let mut paths: Vec<PathBuf> = entries
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .filter(|path| path.extension().and_then(|ext| ext.to_str()) == Some("json"))
        .collect();
    paths.sort();
    paths.truncate(MAX_REGISTRY_FILES);
    for path in paths {
        if path == direct {
            continue;
        }
        let Ok(Some(record)) = read_record(&path) else {
            continue;
        };
        if record.id.trim() == id {
            return Ok(Some((record, path)));
        }
    }
    Ok(None)
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

/// 单条注册表文件的大小上限：正常记录不到 1KB，64KB 已放宽两个数量级仍能兜住
/// 手滑粘贴的超大内容——超限按损坏处理，读进内存前先拒。
const MAX_RECORD_FILE_BYTES: u64 = 64 * 1024;
/// 注册表文件数上限：防目录被塞满后扫描与内存无界放大（本地写权限才可触发，纯防御）。
const MAX_REGISTRY_FILES: usize = 256;

/// 读单个注册表文件。文件缺失 → Ok(None)；JSON 损坏/字段缺失/超大小上限 → Err（上层进 warnings）。
/// 大小上限在 `Take` 有界读取处生效：metadata 检查与真正读取之间存在本地竞态，
/// 有界读保证任何情况下读进内存的字节都有上界。
fn read_record(path: &Path) -> Result<Option<RegistryRecord>, String> {
    use std::io::Read;
    if !path.is_file() {
        return Ok(None);
    }
    let file = std::fs::File::open(path).map_err(|error| format!("文件读取失败：{error}"))?;
    let mut text = String::new();
    file.take(MAX_RECORD_FILE_BYTES + 1)
        .read_to_string(&mut text)
        .map_err(|error| format!("文件读取失败：{error}"))?;
    if text.len() as u64 > MAX_RECORD_FILE_BYTES {
        return Err(format!("文件异常过大（上限 {MAX_RECORD_FILE_BYTES} 字节）"));
    }
    parse_record(&text).map(Some)
}

fn parse_record(text: &str) -> Result<RegistryRecord, String> {
    let value: Value =
        serde_json::from_str(text).map_err(|error| format!("JSON 无法解析：{error}"))?;
    let record: RegistryRecord = serde_json::from_value(value)
        .map_err(|error| format!("字段不符合注册表 schema：{error}"))?;
    if record.name.trim().is_empty() {
        return Err("缺少模型名（name）".to_string());
    }
    Ok(record)
}

fn read_registry(dir: &Path) -> (Vec<ExternalModel>, Vec<String>) {
    let mut warnings = Vec::new();
    let Ok(entries) = std::fs::read_dir(dir) else {
        // 目录不存在 = 还没有任何外部模型，属正常首跑。
        return (Vec::new(), warnings);
    };
    let mut paths: Vec<PathBuf> = entries
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .filter(|path| path.extension().and_then(|ext| ext.to_str()) == Some("json"))
        .collect();
    paths.sort();
    if paths.len() > MAX_REGISTRY_FILES {
        warnings.push(format!(
            "注册表文件数超过 {MAX_REGISTRY_FILES}，已忽略其余；请检查 {}",
            dir.display()
        ));
        paths.truncate(MAX_REGISTRY_FILES);
    }
    let mut loaded: Vec<(String, ExternalModel)> = Vec::new();
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
                let model = ExternalModel {
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
                };
                loaded.push((stem_of(&path), model));
            }
            Ok(None) => {}
            Err(reason) => warnings.push(format!(
                "跳过无法解析的注册表文件（{}）：{reason}",
                path.display()
            )),
        }
    }
    let models = resolve_duplicate_ids(loaded, &mut warnings);
    (models, warnings)
}

fn stem_of(path: &Path) -> String {
    path.file_stem()
        .and_then(|stem| stem.to_str())
        .unwrap_or_default()
        .to_string()
}

/// 同一 id 出现在多个注册表文件（手工拷贝/恢复备份/早期 slug 文件名的产物）：
/// 优先保留**文件名与 id 一致**的那份——移除按 `<id>.json` 直取，保留别的副本会
/// 出现"删了一条怎么还在"（第三轮评审 P3）；没有一致文件名时保留第一份，其余进
/// 警告，绝不静默吞掉。
fn resolve_duplicate_ids(
    loaded: Vec<(String, ExternalModel)>,
    warnings: &mut Vec<String>,
) -> Vec<ExternalModel> {
    let mut models: Vec<ExternalModel> = Vec::new();
    // (id, 在 models 里的下标, 已保留副本的文件 stem)：换副本时要同步更新 stem。
    let mut seen: Vec<(String, usize, String)> = Vec::new();
    for (stem, model) in loaded {
        let hit = seen
            .iter()
            .enumerate()
            .find(|(_, (existing_id, _, _))| *existing_id == model.id)
            .map(|(position, (_, index, kept_stem))| (position, *index, kept_stem.clone()));
        match hit {
            None => {
                seen.push((model.id.clone(), models.len(), stem));
                models.push(model);
            }
            Some((position, index, kept_stem)) => {
                if kept_stem != model.id && stem == model.id {
                    // 换成文件名与 id 一致的副本（移除才能直取）。
                    let id = model.id.clone();
                    seen[position].2 = stem;
                    models[index] = model;
                    warnings.push(format!(
                        "注册表存在重复 id（{}），已采用文件名与 id 一致的记录",
                        id
                    ));
                } else {
                    warnings.push(format!("注册表存在重复 id（{}），已忽略多余记录", model.id));
                }
            }
        }
    }
    models
}

// ---------------------------------------------------------------- 探测

/// Ollama `/api/tags` 的模型名列表（Ollama 不可达时 Err）。
pub(crate) async fn fetch_ollama_model_names() -> Result<Vec<String>, String> {
    let base = super::ollama_base().await;
    let client = probe_client().ok_or_else(|| "HTTP 客户端初始化失败".to_string())?;
    let mut response = client
        .get(format!("{base}/api/tags"))
        .timeout(PROBE_TIMEOUT)
        .send()
        .await
        .map_err(human_probe_error)?;
    if !response.status().is_success() {
        return Err(format!(
            "服务响应异常（HTTP {}）",
            response.status().as_u16()
        ));
    }
    // 响应体上限 1MB：正常 /api/tags 是几 KB 的 JSON；回环上的失控进程也可能
    // 假扮 Ollama 返回巨量内容。上限必须在**读取**处生效——先查 content-length
    //（有头即拒），再按 chunk 限量聚合；无界 text() 读完整型后才比对长度等于
    // 没防（第三轮评审 P2）。
    const MAX_TAGS_BYTES: usize = 1024 * 1024;
    if let Some(length) = response.content_length() {
        if length as usize > MAX_TAGS_BYTES {
            return Err(format!("Ollama 响应过大（{length} 字节），拒绝解析"));
        }
    }
    let mut body: Vec<u8> = Vec::new();
    loop {
        let chunk = response
            .chunk()
            .await
            .map_err(|error| format!("Ollama 返回内容无法读取：{error}"))?;
        let Some(chunk) = chunk else { break };
        if body.len() + chunk.len() > MAX_TAGS_BYTES {
            return Err(format!(
                "Ollama 响应过大（超过 {MAX_TAGS_BYTES} 字节），拒绝解析"
            ));
        }
        body.extend_from_slice(&chunk);
    }
    let text =
        String::from_utf8(body).map_err(|_| "Ollama 返回内容无法解析（非 UTF-8）".to_string())?;
    let value: Value =
        serde_json::from_str(&text).map_err(|error| format!("Ollama 返回内容无法解析：{error}"))?;
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

/// Ollama 删除的独立预算：删几十 GB blob 的守护进程侧耗时不可控，沿用探测的
/// 1.5s 会把"实际已删/将删"误报成"删除失败可手动处理"（第三轮评审 P3）。
const OLLAMA_DELETE_TIMEOUT: Duration = Duration::from_secs(30);

/// Ollama 删除：`DELETE /api/delete {"name": model}`。
async fn ollama_delete(model: &str) -> Result<(), String> {
    let base = super::ollama_base().await;
    let client = probe_client().ok_or_else(|| "HTTP 客户端初始化失败".to_string())?;
    let response = client
        .delete(format!("{base}/api/delete"))
        .json(&serde_json::json!({ "name": model }))
        .timeout(OLLAMA_DELETE_TIMEOUT)
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

/// 非 Ollama 模型的可达性：端点类探 `{base_url}/models`，权重类看目录是否存在。
async fn probe_status_for(
    kind: &str,
    base_url: Option<&str>,
    weights_path: Option<&str>,
) -> String {
    match kind {
        "openai_endpoint" | "vllm" | "llamacpp" => match base_url.map(endpoint_models_url) {
            Some(url) => match super::http_get_success(&url).await {
                Ok(()) => "available".to_string(),
                Err(_) => "unreachable".to_string(),
            },
            None => "unreachable".to_string(),
        },
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
        let record = parse_record(r#"{"name": "tiny", "kind": "other"}"#).expect("minimal record");
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
        let target = weights_deletion_guard(home.path(), None, &weights).expect("guard must pass");
        assert_eq!(
            target,
            weights.canonicalize().expect("weights canonicalize")
        );
    }

    #[test]
    fn weights_deletion_guard_rejects_the_app_data_tree() {
        // ~/.pinvou3 整树拒绝：注册表目录恰好 3 层、bundle/skills 等更深目录都在
        // 放行深度内，不显式排除就有"删除权重"抹掉应用数据的通道。
        let home = TempDir::new("guard-appdata");
        let app_root = home.path().join(".pinvou3");
        let registry = app_root.join("device").join("external-models.d");
        std::fs::create_dir_all(&registry).expect("create registry dir");
        let deep_app = app_root.join("bundle").join("skills").join("device-doctor");
        std::fs::create_dir_all(&deep_app).expect("create deep app dir");
        assert!(weights_deletion_guard(home.path(), Some(&app_root), &registry).is_err());
        assert!(weights_deletion_guard(home.path(), Some(&app_root), &deep_app).is_err());
        // 保护根本身同样拒绝。
        assert!(weights_deletion_guard(home.path(), Some(&app_root), &app_root).is_err());
        // 同深度、保护根之外的家目录仍放行。
        let weights = home.path().join("ai").join("weights").join("qwen3");
        std::fs::create_dir_all(&weights).expect("create weights");
        assert!(weights_deletion_guard(home.path(), Some(&app_root), &weights).is_ok());
    }

    #[test]
    fn weights_deletion_guard_rejects_the_guard_matrix() {
        let home = TempDir::new("guard-matrix");
        let deep = home.path().join("a").join("b").join("c");
        std::fs::create_dir_all(&deep).expect("create weights dir");

        // 相对路径。
        let relative = Path::new("relative/weights/dir");
        assert!(weights_deletion_guard(home.path(), None, relative).is_err());
        // 不存在的目录。
        assert!(
            weights_deletion_guard(home.path(), None, &home.path().join("no/such/dir")).is_err()
        );
        // 普通文件（不是目录）。
        let file = home.path().join("file.txt");
        std::fs::write(&file, b"x").expect("write file");
        assert!(weights_deletion_guard(home.path(), None, &file).is_err());
        // 家目录本身。
        assert!(weights_deletion_guard(home.path(), None, home.path()).is_err());
        // 家目录之外。
        let outside = TempDir::new("guard-outside");
        let outside_weights = outside.path().join("x").join("y").join("z");
        std::fs::create_dir_all(&outside_weights).expect("create outside dir");
        assert!(weights_deletion_guard(home.path(), None, &outside_weights).is_err());
    }

    #[test]
    fn weights_deletion_guard_rejects_shallow_dirs_under_home() {
        // 相对深度 1/2（家目录的一级、二级子目录）必须拒绝。
        let home = TempDir::new("guard-depth");
        let child = home.path().join("weights-dir");
        std::fs::create_dir_all(&child).expect("create child");
        assert!(weights_deletion_guard(home.path(), None, &child).is_err());
        let grandchild = child.join("sub");
        std::fs::create_dir_all(&grandchild).expect("create grandchild");
        assert!(weights_deletion_guard(home.path(), None, &grandchild).is_err());
        // 孙目录（深度 3）放行。
        let great = grandchild.join("blobs");
        std::fs::create_dir_all(&great).expect("create great");
        assert!(weights_deletion_guard(home.path(), None, &great).is_ok());
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
        let dotted = store
            .path()
            .join("models")
            .join("..")
            .join("models")
            .join("qwen3");
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
        assert_eq!(
            ollama_status(&model, &["qwen3:30b".to_string()]),
            "available"
        );
        assert_eq!(
            ollama_status(&model, &["other:8b".to_string()]),
            "unreachable"
        );
        model.model = None;
        // 无 model 字段时回退按 name 匹配。
        assert_eq!(
            ollama_status(&model, &["qwen3-30b".to_string()]),
            "available"
        );
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
    #[test]
    fn removal_boundary_rejects_confirm_mismatch_store_names_and_store_weights() {
        // 双侧都 trim：存量记录名带空白时确认输入 trim 后仍应匹配（Ok 而非 Err）。
        assert!(
            removal_boundary_error(" qwen3 ", "qwen3", false, None, None).is_ok(),
            "trailing-space record names must stay removable"
        );
        let error = removal_boundary_error("qwen3", " qwen3x ", false, None, None).unwrap_err();
        assert!(error.contains("确认名"));
        // 守卫 2 名字臂：名字命中商店名单。
        let error =
            removal_boundary_error("qwen3", "qwen3", false, Some("Qwen3-30B"), None).unwrap_err();
        assert!(error.contains("模型商店"));
        // 守卫 2 model 臂：别名记录的 Ollama 有效删除名命中商店名单（第三轮
        // 评审 P1）——名字臂拦不住它，必须单独立臂。
        let error = removal_boundary_error("my-alias", "my-alias", false, None, Some("qwen3:30b"))
            .unwrap_err();
        assert!(error.contains("模型商店"));
        // 守卫 2 路径臂：权重位于任一商店根之下（别名躲过两个名字臂时仍被拦）。
        let error = removal_boundary_error("qwen3", "qwen3", true, None, None).unwrap_err();
        assert!(error.contains("模型商店"));
        // 四道全过 → Ok。
        assert!(removal_boundary_error("qwen3", " qwen3 ", false, None, None).is_ok());
    }

    #[test]
    fn load_record_for_removal_finds_slug_named_records_by_content_id() {
        // 早期模板教 AI 用 <slug>.json 命名：UI 展示的内容 id 直取必失败，
        // 必须有界扫描按内容 id 兜底，且返回实际文件路径（删除用它）。
        let dir = TempDir::new("removal-lookup");
        let slug_path = dir.path().join("qwen3-8b.json");
        std::fs::write(
            &slug_path,
            r#"{"id": "m-ab12cd34", "name": "qwen3-8b", "kind": "ollama"}"#,
        )
        .expect("write slug record");
        let (record, path) = load_record_for_removal(dir.path(), "m-ab12cd34")
            .expect("lookup ok")
            .expect("content id must find the slug-named file");
        assert_eq!(record.id, "m-ab12cd34");
        assert_eq!(path, slug_path);
        // 直取命中照常。
        std::fs::write(
            dir.path().join("m-ab12cd34.json"),
            r#"{"id": "m-ab12cd34", "name": "qwen3-8b", "kind": "ollama"}"#,
        )
        .expect("write canonical record");
        let (_, path) = load_record_for_removal(dir.path(), "m-ab12cd34")
            .expect("lookup ok")
            .expect("direct hit");
        assert_eq!(path, dir.path().join("m-ab12cd34.json"));
        // 未登记的 id → None。
        assert!(
            load_record_for_removal(dir.path(), "m-00000000")
                .expect("lookup ok")
                .is_none()
        );
    }

    #[test]
    fn resolve_duplicate_ids_prefers_the_canonical_filename() {
        // 重复 id 时保留文件名与 id 一致的副本（移除按 <id>.json 直取）。
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
        let loaded = vec![
            ("qwen3-8b".to_string(), make("m-1")),
            ("m-1".to_string(), make("m-1")),
        ];
        let mut warnings = Vec::new();
        let models = resolve_duplicate_ids(loaded, &mut warnings);
        assert_eq!(models.len(), 1);
        assert!(warnings[0].contains("文件名与 id 一致"));
        // 无一致文件名时保留第一份，多余副本进警告。
        let loaded = vec![
            ("other".to_string(), make("m-2")),
            ("another".to_string(), make("m-2")),
        ];
        let mut warnings = Vec::new();
        let models = resolve_duplicate_ids(loaded, &mut warnings);
        assert_eq!(models.len(), 1);
        assert!(warnings[0].contains("已忽略多余记录"));
    }

    #[test]
    fn path_under_any_hits_any_root_raw_or_canonical() {
        let root_a = TempDir::new("roots-a");
        let root_b = TempDir::new("roots-b");
        let inside_b = root_b.path().join("models").join("qwen3");
        std::fs::create_dir_all(&inside_b).expect("create inside");
        let roots = vec![root_a.path().to_path_buf(), root_b.path().to_path_buf()];
        assert!(path_under_any(&inside_b, &roots));
        // `..` 拼接规范化后仍命中；根之外不命中。
        let dotted = root_b
            .path()
            .join("models")
            .join("..")
            .join("models")
            .join("qwen3");
        assert!(path_under_any(&dotted, &roots));
        let outside = TempDir::new("roots-outside");
        assert!(!path_under_any(outside.path(), &roots));
    }

    #[test]
    fn read_registry_skips_oversized_record_files() {
        let dir = TempDir::new("registry-big");
        std::fs::write(
            dir.path().join("m-ok.json"),
            r#"{"id": "m-ok", "name": "ok"}"#,
        )
        .expect("write ok record");
        let blob = format!(
            "{{\"name\": \"big\", \"note\": \"{}\"}}",
            "x".repeat(70 * 1024)
        );
        std::fs::write(dir.path().join("m-big.json"), blob).expect("write big record");
        let (models, warnings) = read_registry(dir.path());
        assert_eq!(models.len(), 1);
        assert_eq!(models[0].id, "m-ok");
        assert!(warnings.iter().any(|warning| warning.contains("异常过大")));
    }

    #[test]
    fn read_registry_caps_file_count() {
        let dir = TempDir::new("registry-many");
        for i in 0..(MAX_REGISTRY_FILES + 1) {
            std::fs::write(
                dir.path().join(format!("m-{i:04}.json")),
                format!("{{\"id\": \"m-{i:04}\", \"name\": \"n{i}\"}}"),
            )
            .expect("write record");
        }
        let (models, warnings) = read_registry(dir.path());
        assert_eq!(models.len(), MAX_REGISTRY_FILES);
        assert!(warnings.iter().any(|warning| warning.contains("超过")));
    }

    #[test]
    fn removal_scan_skips_corrupt_siblings_instead_of_failing() {
        // 一个坏邻居（坏 JSON/超限）不得吊死健康记录的移除：扫描口径必须与
        // read_registry 的"坏文件跳过进警告"一致（第四轮评审 P2）。
        let dir = TempDir::new("removal-bad-sibling");
        // 排序在目标之前与之后各放一个坏文件，证明不是"先坏就断"。
        std::fs::write(dir.path().join("m-aaaa1111.json"), "{ broken").expect("bad early");
        let target_path = dir.path().join("m-target000.json");
        std::fs::write(
            &target_path,
            r#"{"id": "m-target000", "name": "healthy", "kind": "ollama"}"#,
        )
        .expect("write healthy record");
        let blob = format!(
            "{{\"name\": \"big\", \"note\": \"{}\"}}",
            "x".repeat(70 * 1024)
        );
        std::fs::write(dir.path().join("m-zzzz9999.json"), blob).expect("bad late");
        let (record, path) = load_record_for_removal(dir.path(), "m-target000")
            .expect("lookup must succeed past corrupt siblings")
            .expect("healthy record must be found");
        assert_eq!(record.id, "m-target000");
        assert_eq!(path, target_path);
    }

    #[test]
    fn removal_direct_hit_requires_matching_content_id() {
        // `m-1.json` 内容 id 是 `m-2` 的错位文件：直取不认（否则破坏性动作会用
        // 错那份记录的权重路径/模型名），落到按内容 id 的兜底扫描（第四轮 P3）。
        let dir = TempDir::new("removal-misfiled");
        let misfiled = dir.path().join("m-11111111.json");
        std::fs::write(
            &misfiled,
            r#"{"id": "m-22222222", "name": "misfiled", "kind": "other"}"#,
        )
        .expect("write misfiled record");
        // 按 m-1 找：直取内容 id 不符 → 扫描也找不到内容 id 为 m-1 的文件 → None。
        assert!(
            load_record_for_removal(dir.path(), "m-11111111")
                .expect("lookup ok")
                .is_none()
        );
        // 按 m-2 找：兜底扫描按内容 id 命中错位文件本身。
        let (record, path) = load_record_for_removal(dir.path(), "m-22222222")
            .expect("lookup ok")
            .expect("content id finds the misfiled file");
        assert_eq!(record.name, "misfiled");
        assert_eq!(path, misfiled);
        // 容忍存量空 id：文件名对得上即放行。
        std::fs::write(
            dir.path().join("m-33333333.json"),
            r#"{"id": "", "name": "legacy-empty-id", "kind": "other"}"#,
        )
        .expect("write empty-id record");
        let (record, _) = load_record_for_removal(dir.path(), "m-33333333")
            .expect("lookup ok")
            .expect("empty content id tolerates the filename contract");
        assert_eq!(record.name, "legacy-empty-id");
    }

    #[tokio::test]
    async fn register_rejects_oversized_string_fields_before_touching_the_store() {
        // 超长字段会登记出 read_record 永远拒绝的僵尸记录（>64KB 整条拒读，
        // 第四轮 P3）；登记端必须在触到商店边界之前就拒绝。错误返回发生在
        // 商店查询 spawn 之前，本测试不需要临时 HOME。
        let huge = "x".repeat(5000);
        let error = register_external_model(
            "caps-test".to_string(),
            "llamacpp".to_string(),
            Some("http://127.0.0.1:8080/v1".to_string()),
            None,
            Some(huge),
        )
        .await
        .expect_err("oversized weights_path must be rejected");
        assert!(error.contains("过长"), "unexpected error: {error}");
        let huge_url = "http://127.0.0.1:8080/".to_string() + &"x".repeat(5000);
        let error = register_external_model(
            "caps-test".to_string(),
            "openai_endpoint".to_string(),
            Some(huge_url),
            None,
            None,
        )
        .await
        .expect_err("oversized base_url must be rejected");
        assert!(error.contains("过长"), "unexpected error: {error}");
    }
}
