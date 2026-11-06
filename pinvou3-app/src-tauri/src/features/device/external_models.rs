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

// architecture-guard: allow-target-cfg -- 生产代码零 target cfg；唯一 target
// cfg 在相交矩阵测试的符号链接别名用例上：std::os::unix::fs::symlink 在
// Windows 不存在，运行时 OS 判断保不住类型检查（projects/tests.rs round-19
// BLOCKER 1 同款结论），且 symlink 无特权创建仅 unix 可用。

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use std::time::Duration;

use super::{PROBE_TIMEOUT, human_probe_error, probe_client};
// 裸名补 `:latest` 的尺子与引擎预算/监控展示共用一份（features→core 是公约允许
// 的依赖方向）：模型身份判定全仓只能有一个口径，此前这里的私有副本按整串判
// `:`、core 按末段判，`host:port/ns/model` 形状会分叉（二十一轮评审 P3）。
use crate::core::model_endpoint::ollama_canonical_name;

/// 允许登记的模型类别（与注册表 schema 一致）。
const MODEL_KINDS: &[&str] = &["ollama", "openai_endpoint", "llamacpp", "vllm", "other"];

#[derive(Debug, Clone, Serialize)]
pub struct ExternalModel {
    pub id: String,
    pub name: String,
    pub kind: String,
    /// 非 ollama 行 = 登记的服务地址；kind == "ollama" 的行（daemon 行与注册
    /// 表行）恒为设置推导的守护进程基地址 + `/v1`——展示、链接与删除统一对准
    /// 后端实际管理的实例，不回显记录里手写的地址（第九轮评审 P2：前端
    /// linkTargetOf 硬编码 11434 的口径修正，移除链本就忽略 record.base_url）。
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

/// 商店边界（路径臂）：权重路径与**任一**商店权重根相交（位于其下、包含该根
/// 或两者相等）即命中。根列表由 `model_store::store_weights_roots()` 提供（默认
/// store 数据根 + 设置里可自定义的 `models_dir`）——设备侧不得自行推断默认值，
/// 否则商店改了权重根这里就会漂移。祖先方向（登记路径包含商店权重根）同样拒绝：
/// 放行的话登记能成功，移除时递归删除会把整棵商店权重根连根清掉（二十六轮
/// 评审 P1），登记端先拒绝比留一条永远删不掉的记录更好。
fn weights_in_store(weights: &str) -> bool {
    paths_intersect_any(
        Path::new(weights),
        &crate::features::model_store::store_weights_roots(),
    )
}

/// 路径包含判断（多根版，测试直打）：任一根命中即 true。
fn path_under_any(path: &Path, roots: &[PathBuf]) -> bool {
    roots.iter().any(|root| path_is_under(path, root))
}

/// 路径相交判断：相等、任一侧包含另一侧都算命中。解析纪律与 [`path_is_under`]
/// 相同（原样、解析后、双侧 canonical），等价于两个方向各做一次单向判断——
/// 复用 [`path_is_under`] 而不是另写解析，避免两套规范化口径漂移。
///
/// 删除语义下单向判断不够：`target.starts_with(root)` 只回答"目标在根之下"，
/// "目标是根的祖先"会放行，而 `remove_dir_all` 对祖先目标恰好是整棵连根删
/// （二十六轮评审 P1：外部权重登记 `$HOME/ai/shared/weights`，商店自定义
/// `models_dir` 在其内的 `store-managed` 子目录——初次检查与删除时复查都单向
/// 放行，商店管理的权重被连带清掉）。比较基于 `Path::starts_with` 的路径分量
/// 语义，不做字符串前缀匹配。
fn paths_intersect(a: &Path, b: &Path) -> bool {
    path_is_under(a, b) || path_is_under(b, a)
}

/// 路径相交判断（多根版，测试直打）：任一根命中即 true。
fn paths_intersect_any(path: &Path, roots: &[PathBuf]) -> bool {
    roots.iter().any(|root| paths_intersect(path, root))
}

/// 本机 Ollama 守护进程的**候选**模型权重根（纯函数，测试直打）：`OLLAMA_MODELS`
/// 迁移目录（Ollama 官方环境变量，应用 env 可见时）**并上**默认 `~/.ollama/models`。
/// 只取 env 根会让"env 指向未建目录/写错"的机器失去默认根保护——daemon 完全可以
/// 继续用默认根（第二十轮评审 P2）。注册表是 AI/手写共用的文件，weights_path 指
/// 进任一根意味着"删除权重"会连根清掉 daemon 的全部 blob——商店 ollama 卡片经
/// `ollama create` 导入的权重也住在里面（第八轮评审 P1：第七轮的守卫键在 kind
/// 字符串上，`kind:"other"` 的同路径记录仍可绕过）。顺序稳定；env 根与默认根相同
/// 时去重。env 不可见（systemd `Environment=`）的迁移根对本函数恒失明，由布局
/// 签名臂兜底。
fn ollama_model_roots(env_value: Option<&str>, home: &Path) -> Vec<PathBuf> {
    let mut roots = Vec::new();
    if let Some(trimmed) = env_value.map(str::trim).filter(|value| !value.is_empty()) {
        roots.push(PathBuf::from(trimmed));
    }
    let default_root = home.join(".ollama").join("models");
    if !roots.contains(&default_root) {
        roots.push(default_root);
    }
    roots
}

/// 登记端路径臂：weights_path（**任何 kind**）与任一 daemon 模型候选根相交
/// （位于其下、包含该根或两者相等）即拒绝。kind 只是自我声明的元数据，照抄它
/// 布防会让 "other"/"llamacpp" 记录带着同样的 weights_path 绕过全链
/// （`~/.ollama/models/blobs` 恰好家目录三层，深度臂放行）。祖先方向同样拒绝：
/// env 迁移根（如 `$HOME/ai/shared/models`）比登记路径更深时，单向判断放行，
/// 移除会连根删掉 daemon 的候选根（二十六轮评审 P1 同向缺口）。与
/// `weights_in_store` 同池执行（canonicalize 是文件系统调用）。
fn weights_under_ollama_root(weights: &str) -> bool {
    ollama_model_roots(
        std::env::var("OLLAMA_MODELS").ok().as_deref(),
        &crate::platform::paths::user_home_dir(),
    )
    .iter()
    .any(|root| paths_intersect(Path::new(weights), root))
}

/// daemon 模型库布局签名（第九轮评审 P2，第二十轮评审 P2 扩展，纯函数测试直打）：
/// 任一路径分量等于 `blobs` 或 `manifests` 即命中。`ollama_model_roots` 只能读到
/// **应用进程自己的**环境，daemon 由 systemd 管理（`Environment=OLLAMA_MODELS=…`）
/// 时迁移目录对应用不可见，根包含臂会整体失明；`models/blobs/sha256-…` 与
/// `models/manifests/…` 是 Ollama 权威存储的两套固定布局——此前签名只认 `blobs`，
/// 手写/AI 记录指向迁移根下 `manifests` 子树（家目录下恰 3 层）时全臂放行，整删
/// 会清掉 daemon 的全部模型清单。按原始路径分量判定，属布局签名而非 canonicalize
/// 的文件系统问题，宁拒勿删（误伤方向只是"恰好叫 blobs/manifests 的自管目录被拒"，
/// 与该臂既有取舍一致）。
fn path_has_ollama_layout_component(weights: &str) -> bool {
    Path::new(weights).components().any(|component| {
        component.as_os_str() == std::ffi::OsStr::new("blobs")
            || component.as_os_str() == std::ffi::OsStr::new("manifests")
    })
}

/// 布局签名的 canonical 复查（十九轮评审 P1）：原始分量不含 `blobs`/`manifests` 的
/// 路径，末分量可以是指向 daemon 模型库（含 systemd `OLLAMA_MODELS` 迁移根，
/// 根包含臂对它失明）的符号链接——canonical 目标才露出 `…/models/blobs`、
/// `…/models/manifests` 布局。目录不存在/不可解析时按 false（登记容许"先登记后
/// 建目录"；路径真身落在 daemon 布局时，移除端守卫对 canonical 的复查兜底拒绝）。
/// pub(crate) 供平台适配层的 symlink 测试直打（features 层不能写 cfg，见
/// platform/linux.rs）。
pub(crate) fn canonicalized_has_ollama_layout(weights: &str) -> bool {
    Path::new(weights)
        .canonicalize()
        .map(|canonical| path_has_ollama_layout_component(canonical.to_string_lossy().as_ref()))
        .unwrap_or(false)
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
                    let expected = model.model.as_deref().unwrap_or(model.name.as_str());
                    model.status = ollama_status(expected, &names);
                }
            }
            // 去重与 ollama_status 同一把尺子（push_ollama_daemon_rows 内注释）。
            push_ollama_daemon_rows(&mut models, &names);
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
    // ollama 行的展示/链接地址统一回填设置推导的 daemon 基地址 + `/v1`（第九
    // 轮评审 P2）：注册行自带的 base_url 一并覆盖、daemon 行不再落 None——
    // 展示、链接与删除都指向后端实际管理的同一实例（探测/删除走的就是
    // `ollama_base()`；daemon 不可达分支同样覆盖）。
    let ollama_link_base = super::ollama_base().await;
    let ollama_link_url = format!("{}/v1", ollama_link_base.trim_end_matches('/'));
    for model in &mut models {
        if model.kind == "ollama" {
            model.base_url = Some(ollama_link_url.clone());
        }
    }
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

/// Ollama 名字归一匹配：**只**把缺省 tag 补成 `:latest`，显式 tag 原样保留，
/// 大小写折叠交给比较侧。daemon 的 /api/tags 恒带 `:tag`（未显式给 tag 时
/// Ollama 默认补 `:latest`），注册表记录/AI 手写常只有裸名——原串比较会让无
/// tag 记录恒判 unreachable，且同一模型再以未登记 daemon 行重复出现（第九轮
/// 评审 P3）；但无条件去 tag 会把 `qwen3:8b` 与 `qwen3:30b` 也当成同一个——
/// tag 是模型身份，列表去重会把 daemon 真实存在的另一个 tag 当"重复行"隐藏，
/// 只装了 30b 却登记 8b 也会被误标 available（十轮评审 P2）。商店边界
/// （model_store boundary）的去 tag 是防误删的保守兜底，不是身份判断，
/// 两把尺子用途不同，不能互借。
fn ollama_name_matches(record_name: &str, daemon_name: &str) -> bool {
    ollama_canonical_name(record_name).eq_ignore_ascii_case(&ollama_canonical_name(daemon_name))
}

/// 列表/登记两侧共用的"业务身份"：同一类别下，记录代表的是哪个服务上的哪个
/// 模型。Ollama 记录的身份是有效删除名（`model` 优先，裸名补 `:latest`、大小写
/// 折叠——与 daemon 去重同一把尺子），端点不参与：列表/删除都走同一配置守护
/// 进程，且未登记 daemon 行 ↔ 其注册行的"翻转对"必须共享身份，armed/busy 才能
/// 穿越静默登记的 id 交换（二十一轮评审 P2）。其余类别以 `model`（端点上的
/// 模型标识）回退 `name`，并把 `base_url` 纳入身份第二元：不同端点上的同名同
/// 模型是设计文档定义的两个独立服务，登记查重、读取告警与前端 armed/busy 键
/// 都按端点区分，不再靠拒绝登记收窄合法配置（二十三轮评审 P2；外部评审同指）。
/// 登记端按它查重、读取端按它告警、前端 `rowIdentity` 按它做 armed/busy 键：
/// "别名记录 + daemon 同名行"此前会共享前端身份，一次 arm 同时打开两个确认
/// 面板（二十一轮评审 P2；外部评审同指）。
fn registry_identity(
    kind: &str,
    name: &str,
    model: Option<&str>,
    base_url: Option<&str>,
) -> (String, String) {
    let effective = model.unwrap_or(name);
    if kind == "ollama" {
        (
            ollama_canonical_name(effective).to_ascii_lowercase(),
            String::new(),
        )
    } else {
        (
            effective.to_string(),
            base_url.unwrap_or_default().to_string(),
        )
    }
}

/// 登记查重（纯函数，测试直打）：已有记录与新记录同 kind 且业务身份（非
/// Ollama 含端点）相同即冲突。
fn find_identity_conflict<'a>(
    existing: &'a [ExternalModel],
    kind: &str,
    name: &str,
    model: Option<&str>,
    base_url: Option<&str>,
) -> Option<&'a ExternalModel> {
    let identity = registry_identity(kind, name, model, base_url);
    existing.iter().find(|record| {
        record.registered
            && record.kind == kind
            && registry_identity(
                &record.kind,
                &record.name,
                record.model.as_deref(),
                record.base_url.as_deref(),
            ) == identity
    })
}

/// 读取端对手写/AI 产出的重复身份（不同 id、同 kind、同模型且非 Ollama 同
/// 端点）只告警不吞：移除按 id 直取仍各自可删，但用户要知道为什么列表里
/// "同一个模型出现两次"。端点独立的同名同模型记录是合法配置，不告警。
fn warn_duplicate_identities(models: &[ExternalModel], warnings: &mut Vec<String>) {
    let mut seen: Vec<(String, (String, String), &str)> = Vec::new();
    for model in models.iter().filter(|model| model.registered) {
        let identity = registry_identity(
            &model.kind,
            &model.name,
            model.model.as_deref(),
            model.base_url.as_deref(),
        );
        if let Some((kind, _, first_id)) = seen
            .iter()
            .find(|(kind, seen_identity, _)| *kind == model.kind && *seen_identity == identity)
        {
            let endpoint = if identity.1.is_empty() {
                String::new()
            } else {
                format!("，服务地址 {}", truncate_echo(&identity.1))
            };
            warnings.push(format!(
                "注册表存在同类别同模型的重复记录（{kind}：{}{endpoint}，id {first_id} 与 {}），建议移除多余的一条",
                truncate_echo(&identity.0),
                model.id
            ));
        } else {
            seen.push((model.kind.clone(), identity, model.id.as_str()));
        }
    }
}

fn ollama_status(expected: &str, names: &[String]) -> String {
    // 归一匹配走 ollama_name_matches（小写折叠 + 缺省 tag 补 latest、显式 tag
    // 保留）：记录里手写 "Qwen3:30B" 时精确比较会恒判 unreachable（五轮评审
    // P3）；裸名记录撞 daemon 的 `mymodel:latest` 是同一刀的另一半（九轮评审
    // P3）；显式 tag 是身份，不得跨 tag 冒充 available（十轮评审 P2）。
    if names.iter().any(|name| ollama_name_matches(expected, name)) {
        "available".to_string()
    } else {
        "unreachable".to_string()
    }
}

/// 把 daemon 在线列表按身份去重后并进视图（纯函数，测试直打）：已注册记录
/// 命中过的名字不再重复成未登记行；没命中过的名字——包括同 base 名的另一
/// 个显式 tag——必须以未登记行现身，否则用户看不到 daemon 上真实存在的模型
/// （十轮评审 P2）。匹配用与 ollama_status 完全相同的**有效身份**（显式
/// model 优先，缺失才回退展示 name，十一轮评审 P2）：name 只是展示别名，
/// 登记了显式 model 仍拿 name 兜底，会把 daemon 真实存在的另一个模型当
/// “重复行”隐藏。
fn push_ollama_daemon_rows(models: &mut Vec<ExternalModel>, names: &[String]) {
    for name in names {
        let already_listed = models.iter().any(|model| {
            if model.kind != "ollama" {
                return false;
            }
            let identity = model.model.as_deref().unwrap_or(model.name.as_str());
            ollama_name_matches(identity, name)
        });
        if !already_listed {
            models.push(ExternalModel {
                id: format!("ollama-{name}"),
                name: name.clone(),
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
    // 控制字符/换行：JSON 转义能保证文件合法，但换行名会击穿 warnings、错误
    // 文案与 UI 行展示（五轮评审 P3）。
    if name.chars().any(char::is_control) {
        return Err("模型名包含控制字符或换行，请改用普通文本".to_string());
    }
    let kind = kind.trim().to_string();
    if !MODEL_KINDS.contains(&kind.as_str()) {
        // kind 是前端/AI 原样传来的任意字符串：回显限长（与 doctor.rs 同一纪律），
        // 构造请求不该把超长输入逐字搬进错误与日志（第八轮评审 P3）。
        return Err(format!(
            "不支持的模型类别“{}”，支持：{}",
            truncate_echo(&kind),
            MODEL_KINDS.join(" / ")
        ));
    }
    let base_url = normalize_optional(&base_url);
    if let Some(url) = &base_url {
        validate_http_url(url)?;
    }
    // Ollama 记录由**本机**守护进程托管：探测/标签/删除全部走设置里的
    // `ollama_base()`（与商店同一实例），record.base_url 不参与任何生命周期。
    // 放行非回环地址会造出"展示指向远程、删除却打本机"的记录——别名命中本机
    // 同名 tag 时会把本机模型真删掉（五轮评审 P3）。与前端 isLocalGatewayUrl、
    // model_store lifecycle 的 endpoint_url_is_loopback 同一把回环尺子。
    if kind == "ollama" {
        if let Some(url) = &base_url {
            if !endpoint_url_is_loopback(url) {
                return Err(
                    "Ollama 模型由本机守护进程托管，不接受指向远程主机的服务地址".to_string(),
                );
            }
        }
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
        let Some(value) = value else { continue };
        if value.chars().count() > 4096 {
            return Err(format!("{label}过长（最多 4096 字符）"));
        }
        // 控制字符与 name 同一纪律（round-22 review P3）：换行的 model 标识会
        // 流进身份查重告警与 UI 行展示，URL/路径里的控制字符只会制造后续失败。
        if value.chars().any(char::is_control) {
            return Err(format!("{label}包含控制字符或换行，请改用普通文本"));
        }
    }
    // 权重路径必须是绝对路径（与移除守卫第一臂、model_store 权重根同一把尺子）：
    // 相对路径此前能登记成功，之后勾"删除权重"的移除必整单失败；且状态探测与
    // 商店边界都按进程 CWD 解析，同一输入随启动目录得到不同判定（二十一轮评审 P2）。
    if let Some(weights) = weights_path.as_deref() {
        if !Path::new(weights).is_absolute() {
            return Err(format!(
                "权重路径必须是绝对路径（收到：{}）",
                truncate_path_echo(weights)
            ));
        }
    }
    // Ollama 的权重字节全部由本机守护进程管理（默认 ~/.ollama，受 OLLAMA_MODELS
    // 迁移），不在任何商店权重根/应用数据根之内——登记 weights_path 的 ollama
    // 记录能让守卫链的名字臂/路径臂全部落空后，对着 `~/.ollama/models/blobs`
    // （家目录下恰好 3 层，深度臂放行）执行整目录删除，把已下载的全部 blob（含
    // 商店部署模型的权重）连根删掉（第七轮评审 P2）。UI 与 doctor 安装模板本
    // 来就不产这个字段，这里把契约落成硬校验；移除端对存量记录同步跳过权重删除。
    if kind == "ollama" && weights_path.is_some() {
        return Err("Ollama 模型的权重由本机守护进程统一管理，weights_path 必须留空".to_string());
    }
    // blob/manifests 布局签名臂（第九轮评审 P2，第二十轮评审 P2 扩展）：kind 只是
    // 自我声明的元数据，"other" 记录带 `…/models/blobs`（或 `…/models/manifests`）
    // 权重路径时，根包含臂对 systemd 迁移目录失明、深度臂放行——任何 kind 的路径
    // 含 daemon 布局分量一律拒绝登记，错误说明权重由 daemon 统一管理、不能登记来删。
    if weights_path
        .as_deref()
        .is_some_and(path_has_ollama_layout_component)
    {
        return Err(format!(
            "“{}”指向 Ollama 守护进程的模型库布局（models/blobs、models/manifests），权重由守护进程统一管理，不能登记为权重路径",
            truncate_path_echo(weights_path.unwrap_or_default().as_str())
        ));
    }

    // Ollama 记录实际删除的目标名：登记端点类别名时 model ≠ name（移除同一口径），
    // 商店边界必须对**有效删除名**检查——只查 name 会漏掉"别名指向商店管理的
    // Ollama 模型"这条路（第三轮评审 P1）。
    let effective_ollama_name =
        (kind == "ollama").then(|| model.clone().unwrap_or_else(|| name.clone()));

    // 商店边界（纵深防御第二层）：商店管理的名字/有效删除名绝不能登记成外部模型。
    // 商店名单查询（sqlite connect）与权重根收集（读 settings）、daemon 模型目录
    // 判定（canonicalize）都是阻塞 IO，先在 blocking 池里把四个边界事实一次查全，
    // 再走纯校验，不占 async worker。
    let (name_hit, model_hit, weights_hit, ollama_root_hit, canonical_blob_hit) =
        tokio::task::spawn_blocking({
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
                let ollama_root_hit = weights_path
                    .as_deref()
                    .map(weights_under_ollama_root)
                    .unwrap_or(false);
                // 上面的签名臂按原始分量判定；末分量是指向 daemon blob 存储的
                // 符号链接时，canonical 目标才露出 blobs 布局（systemd
                // OLLAMA_MODELS 迁移根对环境臂失明，正是签名臂要兜底的场景）——
                // 登记端同样对 canonical 复查，不然这种记录今天就能登记成功
                // （十九轮评审 P1）。canonicalize 与上面两臂同类阻塞 IO，同池。
                let canonical_blob_hit = weights_path
                    .as_deref()
                    .map(canonicalized_has_ollama_layout)
                    .unwrap_or(false);
                (
                    name_hit,
                    model_hit,
                    weights_hit,
                    ollama_root_hit,
                    canonical_blob_hit,
                )
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
    // 商店边界（路径臂）：权重与任一商店权重根（含自定义 models_dir）相交
    // （位于其下或包含该根），与名字臂同等拒绝——否则商店权重可被登记成外部
    // 模型后经设备管理删除；祖先方向放行的话，移除时递归删除会把商店权重根
    // 连根清掉（二十六轮评审 P1）。回显限长：手写/存量记录的 weights_path
    // 只受 64KB 记录上限约束，超长路径不该逐字搬进错误与日志（第二十轮评审 P3）。
    if weights_hit {
        return Err(format!(
            "“{}”与模型商店目录相交（位于商店目录之下，或包含商店权重根），由模型商店管理，不能登记为外部模型；请前往模型商店操作",
            truncate_path_echo(weights_path.unwrap_or_default().as_str())
        ));
    }
    // daemon 模型目录对**任何 kind** 都是登记禁区（第八轮评审 P1）：`~/.ollama`
    // 不在商店权重根/应用数据根之内，上面的 kind=="ollama" 硬校验只覆盖一种
    // 自我声明的类别——"other" 记录带同路径 weights_path 时，登记放行、移除端
    // 深度臂放行，整目录删除会连根清掉全部 blob（含商店 ollama 卡片的权重）。
    if ollama_root_hit {
        return Err(format!(
            "“{}”与本机 Ollama 守护进程的模型目录相交，权重由守护进程统一管理，不能登记为权重路径",
            truncate_path_echo(weights_path.unwrap_or_default().as_str())
        ));
    }
    // canonical 签名复查（十九轮评审 P1）：原始分量不含 daemon 布局分量、末分量
    // 指向 daemon 模型库的符号链接，在这里与移除端守卫同一口径拒绝。
    if canonical_blob_hit {
        return Err(format!(
            "“{}”解析后指向 Ollama 守护进程的模型库布局（models/blobs、models/manifests），权重由守护进程统一管理，不能登记为权重路径",
            truncate_path_echo(weights_path.unwrap_or_default().as_str())
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
        move || {
            publish_registry_entry(
                &dir,
                &name,
                &kind,
                base_url.as_deref(),
                model.as_deref(),
                weights_path.as_deref(),
            )
        }
    })
    .await
    .map_err(|error| format!("注册表写入失败：{error}"))??;

    // 登记响应里的状态如实探测：ollama 臂原先硬编码 unreachable，刚装好的本机
    // 模型即使守护进程在跑也显示"未运行"直到下次刷新（五轮评审 P3）。
    let status = if kind == "ollama" {
        let expected = effective_ollama_name.unwrap_or_else(|| name.clone());
        match fetch_ollama_model_names().await {
            Ok(names) => ollama_status(&expected, &names),
            Err(_) => "unreachable".to_string(),
        }
    } else {
        probe_status_for(&kind, base_url.as_deref(), weights_path.as_deref()).await
    };
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

/// 注册表发布的进程级互斥（见 [`publish_registry_entry`]）：容量检查、身份
/// 查重、随机 id 落盘共用一锁串行。锁中毒时直取内层守卫——登记写盘本就是
/// 可失败的幂等操作，一条注册线程的 panic 不应把后续全部登记打成“锁中毒”。
static REGISTRY_PUBLISH_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// 登记发布临界区（三十五轮评审 P2）：目录准备 → 容量检查 → 读注册表 → 同
/// kind 同业务身份查重 → 随机 id 不覆盖落盘，整段在 [`REGISTRY_PUBLISH_LOCK`]
/// 内串行。此前读取与发布之间无保护：两路同 (kind, effective model, endpoint)
/// 的并发登记都能读到尚无该身份的旧集合、各自通过查重后以不同随机 id 先后
/// 落盘——no-replace 只保护文件 id，不保护业务身份；读取端对存量重复只告警
/// 不吞，两行随后共享同一 armed/busy 身份键，一次 arm 可同时展开两行确认
/// 面板，查重的存在理由被并发路径绕过。应用内的写入只有本函数这一条路（单
/// 实例插件约束下进程内互斥即可覆盖渲染层与 AI 通道的重试/重挂载并发；CLI
/// 的 models 子命令走 model_store，不经本注册表）。应用外确实存在写者：
/// device-doctor 技能教示的 `mkdir -p` + `cat >` shell 直写不经过本锁，也
/// 不走登记校验——那属于注册表自我声明的「手写记录」模型（读取端告警收敛、
/// 容量截断、移除端 fail-closed 重校验兜底），不是本锁的保护对象（36 轮
/// 评审 P2 更正：原注释声称「跨进程不存在写入方」，与技能教示矛盾）。
fn publish_registry_entry(
    dir: &Path,
    name: &str,
    kind: &str,
    base_url: Option<&str>,
    model: Option<&str>,
    weights_path: Option<&str>,
) -> Result<String, String> {
    let _publish_guard = REGISTRY_PUBLISH_LOCK
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    #[cfg(test)]
    section_test_pause(&dir.to_string_lossy());
    std::fs::create_dir_all(dir).map_err(|error| format!("注册表目录创建失败：{error}"))?;
    // 注册表记录含 base_url/weights_path，目录只留 owner 权限（对齐
    // model_store 的私密纪律；unix 专属实现收在 platform 适配层）。
    // 目录被换成符号链接时不收敛：harden 跟随链接会把目标目录 chmod
    // 成 0700（与 read_registry 扫描同一纪律，round-22 review P2）。
    // 记录文件本身由 create_private_file_once 直接按 0600 落盘。
    let dir_is_regular = std::fs::symlink_metadata(dir)
        .map(|meta| !meta.file_type().is_symlink())
        .unwrap_or(false);
    if dir_is_regular {
        super::platform::harden_dir_owner_only(dir);
    }
    // 数量上限在写入处也生效：此前只在读取侧截断，目录塞满后新登记仍会
    // 成功、并把排序靠后的既有记录挤出可见范围（第七轮评审 P3）。
    ensure_registry_has_capacity(dir)?;
    // 同 kind 同业务身份查重（二十一轮评审 P2；二十三轮评审 P2 纳入
    // 端点）：注册表不强制 name 唯一，两条同身份记录会让前端 armed/busy
    // 键碰撞（一次 arm 打开两个确认面板）；不同端点的同名同模型是独立
    // 服务，合法共存。读取端对存量重复只告警不吞，这里对新登记直接拒绝。
    // 读取必须在发布锁内：查重看的是锁内当下集合，不是进入临界区前的旧快照。
    let (existing_models, _) = read_registry(dir);
    if let Some(conflict) = find_identity_conflict(&existing_models, kind, name, model, base_url) {
        return Err(format!(
            "已存在同类别同模型且同服务地址的外部模型记录（{}，id {}），请先移除它，或改用其他名字/模型标识/服务地址",
            truncate_echo(&conflict.name),
            conflict.id
        ));
    }
    persist_new_record(dir, random_record_id, |id| RegistryRecord {
        schema: 1,
        id: id.to_string(),
        name: name.to_string(),
        kind: kind.to_string(),
        base_url: base_url.map(str::to_string),
        model: model.map(str::to_string),
        weights_path: weights_path.map(str::to_string),
        installed_at: Some(chrono::Utc::now().to_rfc3339()),
        note: None,
    })
}

/// 测试专用暂停点（仅测试构建；见 [`publish_registry_entry`]）：受控交错回归
/// 先按**目标目录**置闸，匹配该目录的发布线程在此暂停并通知“已到达”，直到
/// 测试放行。用它在锁内强制出一个确定性窗口：第二个同身份请求必须被挡在
/// [`REGISTRY_PUBLISH_LOCK`] 上，放行后凭锁内重读拒绝——删除该暂停点或把
/// 查重挪出锁，回归即红。闸门按目录匹配是关键：闸是进程级的，同测试二进制
/// 里 `platform/linux.rs` 的登记 e2e 用例与竞态用例并行跑，若首到者无条件
/// 暂停，e2e 线程会偷走竞态用例布下的闸（wait 提前返回、try_lock 断言空转、
/// 放行后 A/B 争锁时序不定），竞态回归退化为 flake（第 36 轮评审 P2）。
#[cfg(test)]
fn section_test_pause(key: &str) {
    let mut state = SECTION_TEST_PAUSE.lock().expect("section pause lock");
    if !state.armed || state.armed_key.as_deref() != Some(key) {
        return;
    }
    state.armed = false;
    state.arrived = true;
    SECTION_TEST_PAUSE_CV.notify_all();
    while !state.release {
        state = SECTION_TEST_PAUSE_CV
            .wait(state)
            .expect("section pause condvar");
    }
}

#[cfg(test)]
struct SectionPauseState {
    armed: bool,
    armed_key: Option<String>,
    arrived: bool,
    release: bool,
}

#[cfg(test)]
static SECTION_TEST_PAUSE: std::sync::Mutex<SectionPauseState> =
    std::sync::Mutex::new(SectionPauseState {
        armed: false,
        armed_key: None,
        arrived: false,
        release: false,
    });

#[cfg(test)]
static SECTION_TEST_PAUSE_CV: std::sync::Condvar = std::sync::Condvar::new();

fn normalize_optional(raw: &Option<String>) -> Option<String> {
    raw.as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_string)
}

/// 回环判定：`IpAddr::is_loopback()`（整个 127.0.0.0/8 与 ::1，不止字面量）+
/// `localhost`（剥一个 FQDN 尾点）。与 model_store lifecycle 的
/// `endpoint_url_is_loopback`、前端 pinvou-model-link 的 `isLocalGatewayUrl`
/// 同判据——"各自一份 + 注释互指"是仓库既有惯例（三处必须同一把尺子）。
fn endpoint_url_is_loopback(url: &str) -> bool {
    reqwest::Url::parse(url)
        .ok()
        .and_then(|parsed| parsed.host_str().map(str::to_string))
        .is_some_and(|host| {
            // IPv6 的 host_str 带方括号（`[::1]`），先剥掉再比较——剥括号与剥尾点
            // 必须**分两步**：链成 `strip_suffix('.').unwrap_or(&host)` 时，无尾点
            // 的 IPv6 主机会经 unwrap 回到带括号原串，parse 失败被误判为非回环，
            // `http://[::1]/…` 的合法本机地址被拒（第二十轮评审 P1 修，与
            // model_store 孪生实现对齐）。
            let host = host.trim_start_matches('[').trim_end_matches(']');
            // 末尾剥**一个**点（合法 FQDN 至多一个，`localhost..` 不是合法写法）。
            let host = host.strip_suffix('.').unwrap_or(host);
            host.eq_ignore_ascii_case("localhost")
                || host
                    .parse::<std::net::IpAddr>()
                    .is_ok_and(|address| address.is_loopback())
        })
}

/// URL 可解析且是 http(s)。错误回显限长：解析失败分支发生在 4096 长度上限
/// 之前，构造的超长输入不该逐字进错误与日志（第八轮评审 P3）。
fn validate_http_url(raw: &str) -> Result<(), String> {
    match reqwest::Url::parse(raw) {
        Ok(url) if matches!(url.scheme(), "http" | "https") => Ok(()),
        Ok(url) => Err(format!(
            "服务地址必须是 http(s) 地址：{}",
            truncate_echo(url.as_ref())
        )),
        Err(error) => Err(format!(
            "服务地址无法解析（{}）：{}",
            truncate_echo(raw),
            truncate_echo(&error.to_string())
        )),
    }
}

/// 随机记录 id：`m-` + 8 位随机十六进制（命名约定见 doctor 模板第 6 步）。
fn random_record_id() -> String {
    let uuid = uuid::Uuid::new_v4().simple().to_string();
    format!("m-{}", &uuid[..8])
}

/// 写入侧数量上限（纯 IO，测试直打，round-22 评审 P3 补钉）：目录塞满后新登记
/// 必须在写入口拒绝——此前该臂零覆盖，删掉比较或放行超额登记全套测试照绿，
/// 第 257 条会登记成功却在读取侧被截断成 UI 不可见的隐形记录。
fn ensure_registry_has_capacity(dir: &Path) -> Result<(), String> {
    let existing = std::fs::read_dir(dir)
        .map_err(|error| format!("注册表目录读取失败：{error}"))?
        .filter_map(|entry| entry.ok())
        .filter(|entry| entry.path().extension().and_then(|ext| ext.to_str()) == Some("json"))
        .count();
    if existing >= MAX_REGISTRY_FILES {
        return Err(format!(
            "注册表已满（上限 {MAX_REGISTRY_FILES} 条），请先移除不需要的模型"
        ));
    }
    Ok(())
}

/// 落盘一条新注册记录：生成 id → 序列化 → **不覆盖式发布，id 撞车换新 id 重试
/// （上限 16 次）。此前是"存在性检查 + rename 覆盖写"两步，检查与写之间是
/// TOCTOU 窗口：两个应用实例并发登记撞上同一 id（2⁻³²）时，后写者把先写者
/// 刚落盘的记录整个静默覆盖。现改用底座 `create_private_file_once
/// （rename NOREPLACE / hard_link 兜底，OS 级原子不覆盖，temp 即 0600、失败
/// 清理临时文件、写后与发布后都有 fsync——私密性与原子性与 atomic_write_private
/// 同一纪律），碰撞由 `AlreadyExists` 报出，输家用新 id 重试，任何时序下都
/// 不会吞掉对方的记录。序列化在重试内重复：id 在记录体里，换 id 必须重序列化。
fn persist_new_record(
    dir: &Path,
    mut next_id: impl FnMut() -> String,
    build: impl Fn(&str) -> RegistryRecord,
) -> Result<String, String> {
    for _ in 0..16 {
        let id = next_id();
        let record = build(&id);
        let json = serde_json::to_string_pretty(&record)
            .map_err(|error| format!("注册表序列化失败：{error}"))?;
        let path = dir.join(format!("{id}.json"));
        match crate::platform::filesystem::create_private_file_once(
            &path,
            format!("{json}\n").as_bytes(),
        ) {
            Ok(()) => return Ok(id),
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(error) => return Err(format!("注册表写入失败：{error}")),
        }
    }
    Err("注册表 id 生成冲突，请重试".to_string())
}

// ---------------------------------------------------------------- 移除

/// 移除外部模型（设计 §4 守卫链，顺序执行，任何一条不过返回人话 Err）。
///
/// 与登记发布锁的关系（36 轮评审备案的取舍）：移除刻意不取
/// [`REGISTRY_PUBLISH_LOCK`]——它只在锁内 `read_registry` 的快照上做守卫判定，
/// 随后按文件 id 删。登记读到旧快照（含本条）后、移除落盘前完成，登记会以新
/// id 复活同一业务身份（反之登记在先则被拒绝）；这是两个合法用户意图的
/// last-writer-wins，读取端对存量重复只告警，不构成破坏路径，故不为此加锁
/// 拓宽临界区。
///
/// `expected_ollama_base`（2026-11-04 评审 P3）：ollama 记录的确认面板只带
/// 模型标识与名称，守护进程地址此前在删除时刻才解析——列表之后、确认之前
/// 这段窗口里改过商店设置的 `ollama_host`，DELETE 会改投新实例，同名模型
/// 即被误删。前端把**列表行自带的基地址**（与展示/链接同一解析）随确认传入，
/// 这里绑定：不一致整单拒绝（要求刷新后重新确认），一致则全程使用**这一
/// 次解析值**执行删除（守卫 4 不再二次解析，收窄检查与删除之间的改配窗口）。
/// settings 读取失败维持三十二轮评审口径：不硬拦注册表清理，守卫 4 再解析
/// 一次并把失败收进警告。
pub async fn remove_external_model(
    id: String,
    delete_weights: bool,
    confirm_name: String,
    expected_ollama_base: Option<String>,
) -> Result<RemoveOutcome, String> {
    let mut warnings = Vec::new();
    if !is_safe_id(&id) {
        // 非注册表形状的 id（含 Ollama 在线条目的展示 id）：不是本机登记的模型。
        // is_safe_id 恰在 id 超长/含杂字符时失败——回显必须限长，否则构造请求
        // 的超长输入被逐字搬进错误与日志（第八轮评审 P3，与 doctor.rs 同一纪律）。
        return Err(format!(
            "“{}”不是本机登记的外部模型；未注册模型请通过 AI 助手会话移除",
            truncate_echo(&id)
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
        // Ollama 记录的权重从不由这里删除（守卫 3 对 ollama 一律跳过目录删除），
        // 存量手写记录恰把 weights_path 写在商店根之下时，路径臂仍整单拒绝会让
        // 这条记录永远移除不掉、错误还指向管不到注册表的商店（二十一轮评审 P3）。
        let weights_path = (record.kind != "ollama")
            .then(|| record.weights_path.clone())
            .flatten();
        let ollama_model_name = ollama_model_name.clone();
        let record_id = record.id.clone();
        move || {
            let name_hit =
                crate::features::model_store::store_model_name_checked(&record_name, false)?;
            let ollama_model_hit = match &ollama_model_name {
                // Ollama 有效删除名按大小写不敏感对齐：daemon 标签归一小写，
                // BINARY 精确匹配会漏掉 "Qwen3:30B" → "qwen3:30b" 的别名（第四轮 P3）。
                Some(name) => crate::features::model_store::store_model_name_checked(name, true)?,
                None => None,
            };
            let weights_hit = match weights_path.as_deref() {
                // 权重根来自 settings（models_dir 可配）：settings 读取失败按
                // Err 拒绝——静默回退默认根会让自定义根的保护在这一拍失效，
                // 深度守卫照常放行（五轮评审 P3，与名字臂 fail-closed 同口径；
                // 登记端维持宽松版 weights_in_store）。
                Some(weights) => {
                    let roots = crate::features::model_store::store_weights_roots_checked()?;
                    paths_intersect_any(Path::new(weights), &roots)
                }
                None => false,
            };
            // 存量引用臂（2026-11-04 评审 P2）：目录级保护超集覆盖不到的两类
            // 存量——升级前已移除的扫描目录（历史为空）与被 16 条上限淘汰的
            // 历史根——由"在册卡片实际引用的权重路径"兜底。fail-closed 与
            // 名单/权重根查询同口径。
            let installed_refs = crate::features::model_store::installed_weight_paths_checked()?;
            let installed_weight_hit = match weights_path.as_deref() {
                Some(weights) => paths_intersect_any(Path::new(weights), &installed_refs),
                None => false,
            };
            // 零痕迹旧根名字臂（2026-11-04 评审 P2 zhuowp 场景）：路径臂按
            // 「已知根 × 约定名」恢复引用，覆盖不了旧版本自定义权重根不留痕的
            // 存量——旧版本保存存储设置时换根并移除扫描目录，旧根不在任何
            // 历史/内置根中，升级后零痕迹。这里按名字 fail-closed 兜底：约定
            // 名集合只依赖在册记录（覆盖零痕迹旧根的原因），目标叶名或一级
            // 子目录名命中即拒绝。fail-closed 与名单/权重根查询同口径。
            let candidate_names =
                crate::features::model_store::installed_weight_candidate_names_checked()?;
            let installed_name_hit = match weights_path.as_deref() {
                Some(weights) => target_matches_candidate_names(Path::new(weights), &candidate_names),
                None => false,
            };
            // 自取名原目录标记臂（2026-11-05 评审 P2 zhuowp 场景 II）：官方
            // 下载脚本按标记文件（model_index.json 等）在扫描根里发现原始模型
            // 目录再软链——原目录名是用户自取的，名字臂与路径臂都匹配不上。
            // 目标子树内任意深度出现标记文件 + 本机仍有在册卡片 = 无法确认
            // 它不是在册卡片的就地导入原目录，fail-closed 拦截。误拦面与解除
            // 方式见 installed_import_markers_checked 的文档。
            let import_markers =
                crate::features::model_store::installed_import_markers_checked()?;
            let installed_marker_hit = match weights_path.as_deref() {
                Some(weights) => target_contains_import_marker(Path::new(weights), &import_markers),
                None => false,
            };
            // 登记权重文件名臂（2026-11-06 评审 P2 zhuowp 场景 III）：目录改名
            // 且无标记时，记录里的权重文件名是唯一存活的引用数据，目标子树内
            // 命中即 fail-closed（见 target_contains_registered_weight_file）。
            let weight_filenames =
                crate::features::model_store::installed_weight_filenames_checked()?;
            let installed_filename_hit = match weights_path.as_deref() {
                Some(weights) => {
                    target_contains_registered_weight_file(Path::new(weights), &weight_filenames)
                }
                None => false,
            };
            // 同仓注册表交叉引用臂（2026-11-07 评审 P2 zhuowp，review
            // 5801985394）：按其余登记记录的 weights_path 做归属兜底，读取失败
            // 按 Err 上抛（与商店查询 fail-closed 同口径）。
            let registry_weights = other_registry_weights_checked(&registry_dir(), &record_id)?;
            let registry_weights_hit = match weights_path.as_deref() {
                Some(weights) => paths_intersect_any(Path::new(weights), &registry_weights),
                None => false,
            };
            Ok((
                name_hit,
                ollama_model_hit,
                weights_hit,
                installed_weight_hit,
                installed_name_hit,
                installed_marker_hit,
                installed_filename_hit,
                registry_weights_hit,
            ))
        }
    })
    .await
    .map_err(|error| format!("商店边界查询失败：{error}"))?
    .map_err(|error: String| {
        // 商店库查询失败（含永久损坏）在"请稍后重试"之外同样给手工删除出路
        // （round-22 review P3，与命中商店名单的拒绝同一句尾）：永久损坏时
        // "稍后重试"是死路，记录会从 UI 永远删不掉。
        format!("{error}；为防误删商店管理的模型，本次移除已被拒绝，请稍后重试{STORE_BOUNDARY_ESCAPE_HINT}")
    })?;
    let (
        name_hit,
        ollama_model_hit,
        weights_hit,
        installed_weight_hit,
        installed_name_hit,
        installed_marker_hit,
        installed_filename_hit,
        registry_weights_hit,
    ) = boundary;
    removal_boundary_error(
        &record.name,
        &confirm_name,
        weights_hit,
        installed_weight_hit,
        installed_name_hit,
        installed_marker_hit,
        installed_filename_hit,
        registry_weights_hit,
        name_hit.as_deref(),
        ollama_model_hit.as_deref(),
    )?;

    // 守卫 2.5（2026-11-04 评审 P3）：ollama 记录在**任何破坏性动作之前**绑定
    // 守护进程实例。解析与比对集中在这一处：一致则把这一次的解析值带给守卫 4
    // （删除不再二次解析），不一致整单拒绝——注册表文件原样保留，用户刷新
    // 设备页后对当前视图重新确认。
    let ollama_bound_base = match ollama_model_name.as_deref() {
        Some(_) => match super::ollama_base_checked().await {
            Ok(current) => {
                ensure_ollama_base_binding(expected_ollama_base.as_deref(), &current)?;
                Some(current)
            }
            // settings 读不出来时无法比对，维持三十二轮评审口径：此处不硬拦
            // （注册表清理不该被瞬时 IO 卡死），守卫 4 再解析一次，失败按警告
            // 收尾、DELETE 不改投默认实例。
            Err(_) => None,
        },
        // 非 ollama 记录不涉及守护进程，绑定参数不参与（前端对这类行传 null）。
        None => None,
    };

    // 守卫 3：权重目录删除（绝对路径 / 家目录之下 / 深度 ≥ 3 / 存在的目录 /
    // 不在应用自身数据树内）。守卫是纯路径计算，留在 async 侧；只有整目录删除
    // 进 blocking 池——大权重目录的 remove_dir_all 可能跑很久，不能占住 async
    // worker。JoinError 与删除失败映射进同一句人话报错。
    let mut removed_weights = false;
    if delete_weights {
        if record.kind == "ollama" {
            // 契约收紧的存量兜底（与登记端"weights_path 必须留空"校验同一条
            // 约束）：早期模板/AI 手写的 ollama 记录可能登记了权重路径，移除
            // 一律不执行目录删除，只如实警告（第七轮评审 P2）。模型本身仍从
            // 守护进程删除（守卫 4），权重随 daemon 的模型删除一并回收。
            if record.weights_path.is_some() {
                warnings.push(
                    "Ollama 记录不执行权重目录删除：权重由 Ollama 守护进程统一管理，随模型删除一并回收"
                        .to_string(),
                );
            }
        } else {
            match record.weights_path.as_deref() {
                None => warnings.push("该记录未登记权重路径，已跳过权重删除".to_string()),
                Some(weights) => {
                    let home = crate::platform::paths::user_home_dir();
                    let protected = crate::platform::paths::pinvou3_home();
                    let weights = weights.to_string();
                    let weights_for_task = weights.clone();
                    let record_id_for_task = record.id.clone();
                    let removed =
                        tokio::task::spawn_blocking(move || -> Result<Option<PathBuf>, String> {
                            // 存在性预检与守卫/删除同一池：stat 与 canonicalize
                            // 同类，陈旧 NFS/FUSE 挂载上可能阻塞数秒，不能占住
                            // async worker（第七轮评审 P2；原先 exists() 留在
                            // async 侧，与本文件"文件 IO 全入池"的纪律相悖）。
                            // 预检必须区分"确认不存在"与"读不了"：Path::exists()
                            // 把权限/IO 错误折叠成 false，仍存在的目录会被按
                            // "已删除"收敛并删掉登记，丢失重试入口（round-29
                            // review P2；与 read_registry 的同族修法同口径）。
                            // 仅确认 NotFound 走幂等收尾，其余错误按失败返回、
                            // 登记保留。
                            if !weights_dir_presence(Path::new(&weights_for_task)).map_err(
                                |error| {
                                    format!(
                                        "权重目录存在性检查失败（{}）：{error}；为防误删已拒绝并保留登记",
                                        truncate_path_echo(&weights_for_task)
                                    )
                                },
                            )? {
                                return Ok(None);
                            }
                            // 守卫（含 canonicalize）与整目录删除同一池执行：
                            // canonicalize 在陈旧 NFS/FUSE 挂载上可能阻塞数秒，
                            // 不能占住 async worker（第四轮评审 P3）。
                            // daemon 模型候选根 = env 迁移根 + 默认根并集：env
                            // 写错/指向未建目录时默认根仍要受保护（第二十轮评审 P2）。
                            let ollama_roots = ollama_model_roots(
                                std::env::var("OLLAMA_MODELS").ok().as_deref(),
                                &home,
                            );
                            let target = weights_deletion_guard(
                                &home,
                                Some(&protected),
                                &ollama_roots,
                                Path::new(&weights_for_task),
                            )?;
                            // canonicalize（守卫）与 remove_dir_all（删除）之间存在窗口：
                            // 本地进程可把路径的中间分量换成符号链接让删除落到别处。
                            // 删除前重新解析并比对，收窄最后一跳。剩余的亚毫秒级精确
                            // 竞速不在承诺内——能写 HOME 的本地进程本就可以直接 rm -rf，
                            // 守卫防的是"守卫时刻判断正确、删除时刻路径已被换走"的
                            // 构造场景（五轮评审对抗实测）。
                            let recheck = target.canonicalize().map_err(|error| {
                                format!(
                                    "拒绝删除：目标目录在删除前变得不可解析（{}）：{error}",
                                    truncate_path_echo(&target.to_string_lossy())
                                )
                            })?;
                            ensure_recheck_unchanged(&target, &recheck)?;
                            // 商店权重根删除时刻复查（第九轮评审 P3）：边界臂的
                            // 根集合只在守卫链早段查过一次，与上方 ollama 根同一
                            // 模式在闭包内重推导；查询失败按拒绝（与边界臂
                            // fail-closed 同口径）。canonicalize（守卫）→ 删除
                            // 之间剩余的同用户竞速窗口沿用上方免责：能写
                            // settings/HOME 的本地进程本就可以直接 rm -rf，这里
                            // 收窄的是"守卫时刻判断正确、删除时刻目录已被换进
                            // 商店根"的构造场景。
                            let store_roots =
                                crate::features::model_store::store_weights_roots_checked()
                                    .map_err(|error| {
                                        format!(
                                            "拒绝删除：商店权重根读取失败（{error}），为防误删已拒绝"
                                        )
                                    })?;
                            ensure_recheck_outside_store_roots(&recheck, &store_roots)?;
                            // 存量引用删除时刻复查（2026-11-04 评审 P2）：与商店
                            // 根复查同一模式重推导，查询失败按拒绝（fail-closed
                            // 同口径）。
                            let installed_refs =
                                crate::features::model_store::installed_weight_paths_checked()
                                    .map_err(|error| {
                                        format!(
                                            "拒绝删除：已安装引用读取失败（{error}），为防误删已拒绝"
                                        )
                                    })?;
                            ensure_recheck_outside_installed_refs(&recheck, &installed_refs)?;
                            // 零痕迹旧根名字删除时刻复查（2026-11-04 评审 P2
                            // zhuowp 场景）：与边界闭包的名字判定镜像同一口径，
                            // 查询失败按拒绝（fail-closed 同口径）。
                            let candidate_names =
                                crate::features::model_store::installed_weight_candidate_names_checked()
                                    .map_err(|error| {
                                        format!(
                                            "拒绝删除：在册卡片约定名读取失败（{error}），为防误删已拒绝"
                                        )
                                    })?;
                            ensure_recheck_not_candidate_named(&recheck, &candidate_names)?;
                            // 自取名原目录标记删除时刻复查（2026-11-05 评审 P2
                            // zhuowp 场景 II）：与边界闭包的标记判定镜像同一
                            // 口径，查询失败按拒绝（fail-closed 同口径）。
                            let import_markers =
                                crate::features::model_store::installed_import_markers_checked()
                                    .map_err(|error| {
                                        format!(
                                            "拒绝删除：在册卡片导入标记读取失败（{error}），为防误删已拒绝"
                                        )
                                    })?;
                            ensure_recheck_free_of_import_markers(&recheck, &import_markers)?;
                            // 登记权重文件名删除时刻复查（2026-11-06 评审 P2
                            // zhuowp 场景 III）：与标记复查镜像同一口径，查询
                            // 失败按拒绝（fail-closed 同口径）。
                            let weight_filenames =
                                crate::features::model_store::installed_weight_filenames_checked()
                                    .map_err(|error| {
                                        format!(
                                            "拒绝删除：在册卡片权重文件名读取失败（{error}），为防误删已拒绝"
                                        )
                                    })?;
                            ensure_recheck_free_of_registered_weight_files(
                                &recheck,
                                &weight_filenames,
                            )?;
                            // 同仓注册表交叉引用删除时刻复查（2026-11-07 评审 P2
                            // zhuowp，review 5801985394）：与商店根复查同一模式
                            // 重推导（守卫后到删除前注册表可能被并发改写），读取
                            // 失败按拒绝（fail-closed 同口径）。
                            let registry_weights =
                                other_registry_weights_checked(&registry_dir(), &record_id_for_task)
                                    .map_err(|error| {
                                        format!(
                                            "拒绝删除：注册表交叉引用读取失败（{error}），为防误删已拒绝"
                                        )
                                    })?;
                            ensure_recheck_outside_registry_weights(&recheck, &registry_weights)?;
                            // 并发移除兜底：另一窗口/通道已把目录删掉时
                            // remove_dir_all 报 NotFound——与存在性预检的
                            // Ok(None) 同口径按"已删除"收敛，不报误导性的
                            // "删除失败"（第二十轮评审 P3；与 ollama 404 幂等
                            // 同一语义）。
                            match std::fs::remove_dir_all(&recheck) {
                                Ok(()) => Ok(Some(recheck)),
                                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                                    Ok(None)
                                }
                                Err(error) => Err(format!(
                                    "权重目录删除失败（{}）：{error}",
                                    truncate_path_echo(&recheck.to_string_lossy())
                                )),
                            }
                        })
                        .await
                        .map_err(|error| format!("权重目录删除失败（{}）：{error}", truncate_path_echo(&weights)))??;
                    match removed {
                        // 权重目录已不存在（上次删除成功但记录文件没删掉等，含并发
                        // 移除的 NotFound 兜底）：按已删除处理，不让重试整单报错
                        // 卡死收尾。回显限长（第二十轮评审 P3）。
                        None => warnings.push(format!(
                            "登记的权重目录不存在（可能已删除过），已跳过：{}",
                            truncate_path_echo(&weights)
                        )),
                        Some(_) => removed_weights = true,
                    }
                }
            }
        }
    }

    // 守卫 4：Ollama 模型顺带从 Ollama 里删；失败只警告，不阻断注册表清理。
    // 基地址走可失败解析（三十二轮评审 P2）：settings 恰在此刻读不出来/被隔离
    // 时按 Err 收进警告，DELETE 不改投默认实例——宁可留下可手动处理的原实例
    // 模型，不能删错守护进程上的同名对象。守卫 2.5 绑定通过时沿用**那一次**
    // 的解析值（2026-11-04 评审 P3：二次解析会在检查与删除之间重开改配窗口）。
    let mut ollama_deleted = false;
    if let Some(model_name) = &ollama_model_name {
        let delete_result = match ollama_bound_base.as_deref() {
            Some(base) => ollama_delete_against(base, model_name).await,
            None => ollama_delete(model_name).await,
        };
        match delete_result {
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
        // 并发双通道（设备页 + AI 技能）对同一 id 各发一次移除时，后到者的
        // remove_file 得 NotFound——文件确实已不在，终态正确，与权重臂/ollama
        // 404 同口径按"已删除"收敛，不再误报"删除失败"（二十一轮评审 P3）。
        Ok(Err(error)) if error.kind() == std::io::ErrorKind::NotFound => {
            warnings.push("注册表文件已不存在（可能已被另一次移除删掉），按已删除处理".to_string());
            true
        }
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
/// 权重不在任一商店权重根之下 → 与在册卡片引用路径不相交 → 目录名不在零痕迹
/// 旧根名字臂命中 → 目录（含一级子目录）不含在册卡片就地导入标记文件。商店
/// 名单/权重根/引用路径/约定名/标记文件的查询结果由调用方先在 blocking 池
/// 算好传入，这里保持纯计算、可测试矩阵直打。
fn removal_boundary_error(
    record_name: &str,
    confirm_name: &str,
    weights_in_store: bool,
    installed_weight_hit: bool,
    installed_name_hit: bool,
    installed_marker_hit: bool,
    installed_filename_hit: bool,
    registry_weights_hit: bool,
    store_name_hit: Option<&str>,
    ollama_model_store_hit: Option<&str>,
) -> Result<(), String> {
    // 双侧都 trim：登记端写入的名字已 trim，但 AI/手写的存量记录可能带空白，
    // 只 trim 输入侧会让这条记录怎么输都删不掉。
    if confirm_name.trim() != record_name.trim() {
        // 名字回显限长（第九轮评审 P3）：record_name 来自手写注册表文件，超长
        // 名字不该逐字搬进错误与日志——与本文件其余回显同一 truncate_echo 纪律。
        return Err(format!(
            "确认名不匹配：为防误删，请输入模型名“{}”以确认移除",
            truncate_echo(record_name)
        ));
    }
    if let Some(store_name) = store_name_hit {
        return Err(format!(
            "“{store_name}”由模型商店管理，请在模型商店中操作，不能从这里移除{STORE_BOUNDARY_ESCAPE_HINT}"
        ));
    }
    if let Some(store_name) = ollama_model_store_hit {
        return Err(format!(
            "该记录的 Ollama 模型“{store_name}”由模型商店管理，请在模型商店中操作，不能从这里移除{STORE_BOUNDARY_ESCAPE_HINT}"
        ));
    }
    if weights_in_store {
        // 相交两个方向共用一句：祖先方向（登记路径包含商店权重根）的删除同样
        // 由商店边界拦下——递归删除会连根清掉商店目录（二十六轮评审 P1）。
        return Err(format!(
            "该模型的权重目录与模型商店目录相交（位于商店目录之下，或包含商店权重根），由模型商店管理，请在模型商店中操作{STORE_BOUNDARY_ESCAPE_HINT}"
        ));
    }
    if installed_weight_hit {
        // 存量引用臂（2026-11-04 评审 P2）：就地复用的卡片权重可能落在已从
        // 设置移除的扫描目录或被历史淘汰的旧根里——目录级超集覆盖不到，由
        // 在册卡片引用路径兜底。
        return Err(format!(
            "该模型的权重目录与已安装商店模型引用的权重路径相交（就地复用），删除会破坏仍在使用的商店模型，请在模型商店中操作{STORE_BOUNDARY_ESCAPE_HINT}"
        ));
    }
    if installed_name_hit {
        // 零痕迹旧根名字臂（2026-11-04 评审 P2 zhuowp 场景）：旧版本允许在
        // 保存存储设置时换权重根并移除扫描目录且不留任何历史，那样的旧自定义
        // 根升级后不在任何已知商店根之下，路径臂恢复不了它的引用——目录名与
        // 在册卡片约定同名时无法确认归属，拦住删除（宁可误拒，不误删）。
        return Err(format!(
            "该目录名与在册商店卡片的权重目录约定同名，且不在任何已知商店根之下，无法确认它不是仍被引用的原始权重（旧版本自定义权重根不留痕），删除可能破坏仍在使用的商店模型，请在模型商店中操作{STORE_BOUNDARY_ESCAPE_HINT}"
        ));
    }
    if installed_marker_hit {
        // 自取名原目录标记臂（2026-11-05 评审 P2 zhuowp 场景 II）：官方卡片
        // 下载脚本按标记文件发现原始模型目录再软链，原目录名用户自取——上面
        // 名字臂/路径臂对它失明。标记文件 + 在册卡片同时成立即说明归属无法
        // 确认，宁可误拒不误删。误拦面（用户自备 diffusers 目录在装卡期间同
        // 样命中）与解除方式（卸载卡片后本臂自动关闭）如实写进文案。
        return Err(format!(
            "该目录（或其一级子目录）含有商店卡片下载脚本就地发现模型所用的标记文件（model_index.json / model.safetensors.index.json），且本机仍有在册商店卡片——无法确认它不是在册卡片软链引用的原始权重目录（旧版本自定义权重根不留痕），删除可能破坏仍在使用的商店模型。请先在模型商店卸载对应卡片（本臂随之放行），或确认该目录与商店无关后移除本记录{STORE_BOUNDARY_ESCAPE_HINT}"
        ));
    }
    if installed_filename_hit {
        // 登记权重文件名臂（2026-11-06 评审 P2 zhuowp 场景 III，review
        // 5797725367）：目录改名且无标记时，记录里的权重文件名是唯一存活的
        // 引用数据——目标子树内出现登记文件名即说明它可能是在册卡片的权重
        // 目录（或其祖先），路径/名字/标记三臂对这类卡片失明。误拦面与解除
        // 方式随文案如实告知（与标记臂同一取舍方向）。
        return Err(format!(
            "该目录子树内含有在册商店卡片登记的权重文件，无法确认它不是仍在使用的商店权重目录（权重目录名可被改名，文件名是卡片运行的必需组件），删除可能破坏仍在使用的商店模型。请先在模型商店卸载对应卡片（本臂随之放行），或确认该目录与商店无关后移除本记录{STORE_BOUNDARY_ESCAPE_HINT}"
        ));
    }
    if registry_weights_hit {
        // 同仓注册表交叉引用臂（2026-11-07 评审 P2 zhuowp，review
        // 5801985394）：商店四臂的在册引用集合全部来自商店库——外部登记与
        // 受支持历史迁移写进本仓注册表的其他记录从不被咨询，其余记录的
        // weights_path 落在删除目标之内（例如以目标为权重父目录登记）时，
        // 整树删除会连带毁掉它们而四臂全放行。按其余记录的登记权重路径做
        // 归属兜底（自身记录排除），宁可误拒不误删；解除方式随文案告知。
        return Err(format!(
            "该模型的权重目录与其他外部登记模型引用的权重路径相交，删除会破坏仍被引用的外部登记权重。请先移除引用该目录的其他登记记录（本臂随之放行）{STORE_BOUNDARY_ESCAPE_HINT}"
        ));
    }
    Ok(())
}

/// 商店边界拒绝的出路提示：商店管不到注册表目录，被拒的记录（登记后商店恰好部署
/// 了同名模型、手写记录把权重放进商店根等）此前是死胡同——与"记录损坏"分支给
/// 手工删除出路同一口径（二十一轮评审 P3）。
const STORE_BOUNDARY_ESCAPE_HINT: &str = "；若确认该登记记录本身不应由商店管理，可手工删除 ~/.pinvou3/device/external-models.d/<id>.json";

/// 守卫 2.5 的纯逻辑（测试矩阵直打）：确认面板带来的守护进程基地址与当前解析
/// 值必须一致，否则整单拒绝。展示端点恒为基地址 + `/v1`（`ExternalModel
/// base_url` 契约），归一化剥掉这一层与尾斜杠后做精确比对——比对的单位是
/// 「实例」，scheme/host/端口任何一个分量变了都是换实例（哪怕新实例恰好也有
/// 同名模型，删除也不该改投它）。expected 缺失（旧调用方/存量行无 base_url）
/// 一律拒绝：绑定是 ollama 记录移除的必要输入，缺了就要求刷新设备页重来，
/// 不给"当没传"的静默放行口子。
pub(crate) fn ensure_ollama_base_binding(
    expected: Option<&str>,
    current: &str,
) -> Result<(), String> {
    let Some(expected) = expected else {
        return Err(
            "移除确认缺少守护进程地址绑定，为防误删已拒绝；请刷新设备页后重新确认".to_string(),
        );
    };
    let normalize = |raw: &str| -> String {
        // 先剥尾斜杠再剥一层 "/v1"、再剥尾斜杠——"…/v1/"（展示端点带的
        // 尾斜杠形状）才能归一到裸基地址；裸基地址本身不受益。
        let trimmed = raw.trim().trim_end_matches('/');
        let without_v1 = trimmed.strip_suffix("/v1").unwrap_or(trimmed);
        without_v1.trim_end_matches('/').to_string()
    };
    let (expected, current) = (normalize(expected), normalize(current));
    if expected != current {
        // 回显限长（与本文件其余回显同一纪律）：地址来自前端载荷/设置文件。
        return Err(format!(
            "Ollama 守护进程配置已变化（确认时 {}，当前 {}），为防误删已拒绝；请刷新设备页后重新确认",
            truncate_echo(&expected),
            truncate_echo(&current),
        ));
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
/// `ollama_roots`（daemon 模型候选根，env 迁移根 + 默认根并集）同理整体拒绝：目录
/// 恰好家目录三层被深度臂放行，而 blob/manifests 全删等于抹掉 daemon 托管的一切
/// 权重——商店 ollama 卡片的也在内。守卫按**路径**布防、与记录自述的 kind 无关
/// （第八轮评审 P1：第七轮按 kind 字符串布防的版本可被 `kind:"other"` 绕过）。
pub(crate) fn weights_deletion_guard(
    home: &Path,
    protected_root: Option<&Path>,
    ollama_roots: &[PathBuf],
    weights_path: &Path,
) -> Result<PathBuf, String> {
    if !weights_path.is_absolute() {
        // 回显限长：注册表是 AI/手写共用的文件，weights_path 只受 64KB 记录上限
        // 约束，构造的超长路径不该逐字搬进错误与日志（第二十轮评审 P3，与
        // truncate_echo 同一纪律；路径给 128 字符以保留可诊断性）。
        return Err(format!(
            "拒绝删除：权重路径必须是绝对路径（收到：{}）",
            truncate_path_echo(&weights_path.to_string_lossy())
        ));
    }
    // blob/manifests 布局签名臂（第九轮评审 P2）：与 ollama 根臂同口径硬拒——
    // OLLAMA_MODELS 只能读到应用自己的环境，daemon 的 systemd 环境不可见，
    // blob/manifests 目录布局（models/blobs/sha256-…、models/manifests/…）是权威
    // 签名，宁拒勿删。按原始路径分量判定，不依赖根目录推导。
    if path_has_ollama_layout_component(weights_path.to_string_lossy().as_ref()) {
        return Err(
            "拒绝删除：权重路径指向 Ollama 守护进程的模型库布局（models/blobs、models/manifests），权重由守护进程统一管理"
                .to_string(),
        );
    }
    let target = weights_path.canonicalize().map_err(|_| {
        format!(
            "拒绝删除：权重目录不存在（{}）",
            truncate_path_echo(&weights_path.to_string_lossy())
        )
    })?;
    if !target.is_dir() {
        return Err(format!(
            "拒绝删除：权重路径不是目录（{}）",
            truncate_path_echo(&target.to_string_lossy())
        ));
    }
    // 签名复查（十九轮评审 P1）：上面的签名臂按**原始**路径分量判定，而
    // weights_path 的原始分量可以不含 daemon 布局分量、末分量是指向 daemon 模型库
    // （含 systemd `OLLAMA_MODELS` 迁移根——根包含臂对它失明，正是签名臂要兜底
    // 的场景）的符号链接；此时 canonical 目标才露出 `…/models/blobs`、
    // `…/models/manifests` 布局，根臂落空、深度恰 3 层放行。凡是在原始路径上做的
    // 判定，解析后都要复查一次；误伤方向只是"用户自管的目录恰好叫 blobs/manifests
    // 被拒"，与该臂既有取舍一致。登记端对 canonical 的同款复查见
    // register_external_model。
    if path_has_ollama_layout_component(target.to_string_lossy().as_ref()) {
        return Err(
            "拒绝删除：权重路径解析后指向 Ollama 守护进程的模型库布局（models/blobs、models/manifests），权重由守护进程统一管理"
                .to_string(),
        );
    }
    if let Some(protected) = protected_root {
        // 解析失败不能跳过保护：注册表目录就在保护根内，正常机器上必可解析，
        // 解析不了说明环境异常——按拒绝处理（与"无法解析家目录"臂同口径，
        // 五轮评审 P3；原先的 `if let Ok` 会静默放行整棵应用树）。
        let protected_canonical = protected.canonicalize().map_err(|_| {
            format!(
                "拒绝删除：无法解析应用数据目录（{}），为防误删已拒绝",
                protected.display()
            )
        })?;
        // 相交两方向都拒绝（二十六轮评审 P1）：目标在根之下覆盖了既有臂，祖先
        // 方向（目标包含保护根）此前会放行——不过该方向对应用数据根实际到不了
        // （目标必须位于家目录之下且家目录本身已被拒），这里补齐是防御纵深，与
        // 商店/Ollama 根的相交臂同口径。
        if target == protected_canonical
            || target.starts_with(&protected_canonical)
            || protected_canonical.starts_with(&target)
        {
            return Err(format!(
                "拒绝删除：目标目录与应用数据目录（{}）相交，请到设置或模型商店操作",
                protected_canonical.display()
            ));
        }
    }
    // daemon 模型候选根逐根比对（第二十轮评审 P2：env 迁移根 + 默认根并集，
    // env 失真时默认根不再失守）。根不存在（未装 Ollama / env 指向未建目录）时
    // canonicalize 失败：跳过的只是"目标在根之下"方向，根不存在时祖先方向同样
    // 不可能成立（目标存在而根不存在，互不包含），跳过该根是安全的；可解析根
    // 与保护根同口径，相交两方向都拒绝（二十六轮评审 P1：env 迁移根比目标
    // 更深时，单向判断放行祖先目标，递归删除连根清掉 daemon 候选根）。
    for root in ollama_roots {
        if let Ok(root_canonical) = root.canonicalize() {
            if target == root_canonical
                || target.starts_with(&root_canonical)
                || root_canonical.starts_with(&target)
            {
                return Err(format!(
                    "拒绝删除：目标目录与 Ollama 守护进程的模型目录（{}）相交，权重由守护进程统一管理",
                    root_canonical.display()
                ));
            }
        }
    }
    let home_canonical = home
        .canonicalize()
        .map_err(|_| "拒绝删除：无法解析家目录".to_string())?;
    // 退化家目录 fail-closed（第九轮评审 P3）：canonicalized home 是文件系统根
    // `/`（或空）时，下面的 containment 臂对一切绝对路径恒真、形同虚设——解析
    // 成这种形状说明环境异常（HOME=/ 或未设的兜底），直接拒绝而不是放行。
    if home_canonical.as_os_str().is_empty() || home_canonical.parent().is_none() {
        return Err("拒绝删除：家目录解析异常（家目录为文件系统根），为防误删已拒绝".to_string());
    }
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
    // 直取命中损坏文件时把原样 Err 往上抛会让用户面对一句 JSON 报错而不知所措
    // ——fail-closed 是对的（无法核对确认名/权重路径就绝不能删），但报错要给出
    // 可执行的出路（第七轮评审 P3）。IO 读取失败（EACCES 等）≠ 内容损坏：
    // 一律称"损坏"会把用户引向检查 JSON 的死路（round-22 review P3），按成因
    // 分叉措辞，出路相同（手工删除该文件）。
    let direct_record = read_record(&direct).map_err(|error| {
        if error.starts_with("文件读取失败") {
            format!(
                "注册表记录无法读取（权限异常？），为防误删已拒绝移除（{}）：{error}；请恢复可读后重试，或手工删除该文件",
                direct.display()
            )
        } else {
            format!(
                "注册表记录损坏，为防误删已拒绝移除（{}）：{error}；请确认后手工删除该文件",
                direct.display()
            )
        }
    })?;
    if let Some(record) = direct_record {
        // 直取也要求内容 id 对得上（容忍存量空 id）：`a.json` 内容 id 是 `m-2`
        // 而 `m-1` 命中直取时，破坏性动作会用错那份记录的权重路径/模型名。
        let content_id = record.id.trim();
        if content_id == id || content_id.is_empty() {
            return Ok(Some((record, direct)));
        }
    }
    let entries = match std::fs::read_dir(dir) {
        Ok(entries) => entries,
        // 目录读不了（权限/挂载异常）≠ 记录不存在：折叠成 Ok(None) 会报"可能
        // 已经被移除，请刷新后重试"，把用户引向无解的重试死路（round-22 review
        // P3）。fail-closed 不变——无法核对确认名/权重路径就绝不能删——但成因
        // 如实、出路给出。
        Err(error) => {
            return Err(format!(
                "注册表目录无法读取，为防误删已拒绝移除（{}）：{error}；请恢复目录可读后重试，或手工删除该记录文件",
                dir.display()
            ));
        }
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

/// 错误回显限长（32 字符，与 doctor.rs 的 kind 回显同一纪律）：错误串会进
/// toast/日志，构造请求的超长输入不该逐字搬运。
fn truncate_echo(value: &str) -> String {
    value.chars().take(32).collect()
}

/// 路径回显限长（128 字符）：注册表是 AI/手写共用的文件，weights_path 只受
/// 64KB 记录上限约束（不像登记命令入口有 4096 校验），超长路径不该逐字搬进
/// 错误与日志（第二十轮评审 P3）。与 truncate_echo 同一纪律；路径给 4 倍长度
/// 以保留可诊断性。
fn truncate_path_echo(value: &str) -> String {
    value.chars().take(128).collect()
}

/// 删除前 canonical 复查的纯逻辑（测试直打，第二十轮评审 P3）：守卫返回的
/// canonical 目标与删除时刻重新解析的结果必须逐字节一致，否则目录在守卫后被
/// 换走（symlink 交换/改名），拒绝重试。
fn ensure_recheck_unchanged(target: &Path, recheck: &Path) -> Result<(), String> {
    if recheck == target {
        return Ok(());
    }
    Err(format!(
        "拒绝删除：目标目录在守卫后被改动（{}），请刷新后重试",
        truncate_path_echo(&target.to_string_lossy())
    ))
}

/// 删除时刻商店权重根复查的纯逻辑（测试直打，第二十轮评审 P3）：守卫通过到
/// 真正删除之间，目录可能已被移进任一商店权重根、或目录本就是商店权重根的
/// 祖先（递归删除连根清掉根内商店权重，二十六轮评审 P1）——相交即拒绝，
/// 由模型商店管理。
fn ensure_recheck_outside_store_roots(
    recheck: &Path,
    store_roots: &[PathBuf],
) -> Result<(), String> {
    if paths_intersect_any(recheck, store_roots) {
        return Err(
            "拒绝删除：目标目录与模型商店目录相交（位于商店目录之下，或包含商店权重根），由模型商店管理，请在模型商店中操作".to_string(),
        );
    }
    Ok(())
}

/// 删除时刻的存量引用复查（纯函数，测试直打）：目标与任一在册卡片引用路径
/// 相交即拒绝。
fn ensure_recheck_outside_installed_refs(
    recheck: &Path,
    installed_refs: &[PathBuf],
) -> Result<(), String> {
    if paths_intersect_any(recheck, installed_refs) {
        return Err(
            "拒绝删除：目标目录与已安装商店模型引用的权重路径相交（就地复用），由模型商店管理，请在模型商店中操作".to_string(),
        );
    }
    Ok(())
}

/// 其余外部注册表记录的登记权重路径集合（同仓交叉引用臂数据面，可失败版；
/// 2026-11-07 评审 P2 zhuowp，review 5801985394）：商店四臂（引用路径/约定
/// 名/导入标记/权重文件名）的在册引用集合全部来自商店库，本仓注册表的其他
/// 记录——外部登记与受支持历史迁移写入的——从不被咨询；其 weights_path 落
/// 在删除目标之内时，整树递归删除会连带毁掉它们而四臂全放行。目录读不了
/// （权限/挂载异常）按 Err 上抛——删除守卫与商店查询同 fail-closed 口径，
/// 不得沿 `read_registry` 的视图级降级静默放行；NotFound（还没有任何外部
/// 登记）按空集合放行；坏邻居文件（坏 JSON/超限）沿既有容错跳过；自身记录
/// 按**内容 id** 排除（删除自身的权重正是本次请求）。
fn other_registry_weights_checked(dir: &Path, exclude_id: &str) -> Result<Vec<PathBuf>, String> {
    let entries = match std::fs::read_dir(dir) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(error) => {
            return Err(format!(
                "注册表目录无法读取（{}）：{error}",
                truncate_path_echo(&dir.to_string_lossy())
            ));
        }
    };
    let mut paths: Vec<PathBuf> = entries
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .filter(|path| path.extension().and_then(|ext| ext.to_str()) == Some("json"))
        .collect();
    paths.sort();
    if paths.len() > MAX_REGISTRY_FILES {
        paths.truncate(MAX_REGISTRY_FILES);
    }
    let mut out: Vec<PathBuf> = Vec::new();
    for path in paths {
        let Ok(Some(record)) = read_record(&path) else {
            continue;
        };
        let id = if record.id.trim().is_empty() {
            path.file_stem()
                .and_then(|stem| stem.to_str())
                .unwrap_or("m-unknown")
                .to_string()
        } else {
            record.id.clone()
        };
        if id == exclude_id {
            continue;
        }
        let Some(weights) = record.weights_path.as_deref() else {
            continue;
        };
        if weights.trim().is_empty() {
            continue;
        }
        let candidate = PathBuf::from(weights);
        // canonical 解析软链对齐真实落点，失败回退原路径（与商店
        // installed_weight_paths_checked 同一把尺）。
        let resolved = candidate.canonicalize().unwrap_or(candidate);
        if !out.contains(&resolved) {
            out.push(resolved);
        }
    }
    Ok(out)
}

/// 同仓注册表交叉引用删除时刻复查（纯函数，测试直打）：目标与任一**其他**
/// 外部登记记录的登记权重路径相交即拒绝（相交两方向：目标在登记路径之内
/// 连带毁掉该权重；目标包含登记路径则递归删除连根清掉）。
fn ensure_recheck_outside_registry_weights(
    recheck: &Path,
    registry_weights: &[PathBuf],
) -> Result<(), String> {
    if paths_intersect_any(recheck, registry_weights) {
        return Err(
            "拒绝删除：目标目录与其他外部登记模型引用的权重路径相交，删除会破坏仍被引用的外部登记权重，请先移除引用该目录的登记记录".to_string(),
        );
    }
    Ok(())
}

/// 零痕迹旧根名字臂的判定（边界闭包与删除时刻复查共用；名字比对是纯计算，
/// 「一级子目录存在性」是一次 stat，调用方都在 blocking 池）：删除目标**自身
/// 的叶目录名**命中在册卡片约定名（直接删权重目录），或目标之下**恰好一层**
/// 存在约定名子目录（删其父目录连带权重），即视为无法排除是仍被引用的原始
/// 权重。卡片脚本按约定把权重放在 `$ROOT/<name>` 一层（import_scan 候选约定，
/// 与 model_store 两臂约定名同一口径），所以两查恰好覆盖「叶删」与「父删」；
/// 更深的祖先（约定名的祖父目录）不含约定名分量、两查都落空——按「宁可漏」
/// 边界如实放行，不向上扩散（评审处方：在无法确认归属时拦住删除，而不是把
/// 任意祖先目录都当可疑）。
fn target_matches_candidate_names(target: &Path, names: &[String]) -> bool {
    if names.is_empty() {
        return false;
    }
    let named = |name: &str| names.iter().any(|candidate| candidate == name);
    // 叶删：目标就是权重目录本身（<零痕迹旧根>/<约定名>）。
    if target
        .file_name()
        .and_then(|name| name.to_str())
        .is_some_and(named)
    {
        return true;
    }
    // 父删：目标是权重目录的父目录，其下一级就是约定名目录。
    names.iter().any(|name| target.join(name).exists())
}

/// 自取名原目录标记判定（存在性探测，测试直打）：目标子树内**任意深度**是否
/// 存在官方卡片下载脚本「就地发现」所用的标记文件。
///
/// 深度不设上限（2026-11-05 评审两轮 P2 的结论）：脚本的
/// `find -maxdepth 4` 是从**它当时配置的扫描根**起算的——扫描根列表可配置、
/// 可能在旧版本被更换且不留历史，守卫无法知道那次发现的起算根在哪里；
/// 待删目标相对那个根可以是任意深度（例如内层扫描根 `T/versions/release`、
/// 外层删除目标 `T`，标记在 `T/versions/release/assets/models/checkpoint`）。
/// 因此「目标子树内存在标记文件」= 无法确认它不是在册卡片的就地导入原目录
/// → 拦截。删除路径 `remove_dir_all` 是整树递归，标记在子树任何深度都会被
/// 一并毁掉。误拦面（装卡期间删除任何含 diffusers 标记的目录都会命中）与
/// 解除方式（卸载在册卡片后本臂自动关闭）随拦截如实告知用户。
///
/// 标记按 `is_file()` 判定（与脚本的 `find -type f` 同口径），同名目录不算；
/// 单分支读不了（read_dir Err）按该分支未命中放行——目录不存在时后续删除
/// 路径本就会按"已删除"收敛，读不了时 remove_dir_all 也会失败，都不产生
/// 破坏窗口。标记集合为空（没有在册卡片）恒不命中。走查半径以删除目标自身
/// 的子树为界（删除的物理范围就是它），不是无界全盘扫描。
fn target_contains_import_marker(target: &Path, markers: &[String]) -> bool {
    subtree_contains_any_named_file(target, markers)
}

/// 子树内文件名命中走查（[`target_contains_import_marker`] 与登记权重文件名
/// 臂共用的半径与口径）：以删除目标的子树为界（删除的物理范围就是它），任意
/// 深度出现名单内的**文件**即命中；read_dir 失败的分支按未命中放行（与标记
/// 臂同一理由：目录不存在时删除路径本就按"已删除"收敛，读不了时
/// remove_dir_all 同样失败，都不产生破坏窗口）。
fn subtree_contains_any_named_file(target: &Path, names: &[String]) -> bool {
    if names.is_empty() {
        return false;
    }
    let has_named = |dir: &Path| names.iter().any(|m| dir.join(m).is_file());
    let mut stack = vec![target.to_path_buf()];
    while let Some(dir) = stack.pop() {
        if has_named(&dir) {
            return true;
        }
        let Ok(entries) = std::fs::read_dir(&dir) else {
            continue;
        };
        for entry in entries.flatten() {
            if entry.file_type().map(|t| t.is_dir()).unwrap_or(false) {
                stack.push(entry.path());
            }
        }
    }
    false
}

/// 登记权重文件名臂（2026-11-06 评审 P2 zhuowp 场景 III，review 5797725367）：
/// 路径臂/名字臂/标记臂共同的盲区——权重目录名被改掉、目录内又没有标记文件，
/// 但在册记录仍存有权重文件名（source_filename）。目录名可以改，权重文件名
/// 是卡片运行的必需组件、不会跟着改：删除目标子树内出现登记文件名 = 无法
/// 确认不是在册卡片的权重，fail-closed 拦截（与标记臂同一走查半径与误拦
/// 方向；集合为空=没有可恢复文件名的在册卡片，恒不命中）。
fn target_contains_registered_weight_file(target: &Path, filenames: &[String]) -> bool {
    subtree_contains_any_named_file(target, filenames)
}

/// 删除时刻的自取名原目录标记复查（纯函数 + 存在性探测，测试直打）：与边界
/// 闭包的标记判定镜像同一口径（[`target_contains_import_marker`]），收窄
/// 「守卫时刻判断正确、删除时刻目录已被换成原目录形状」的窗口。
fn ensure_recheck_free_of_import_markers(recheck: &Path, markers: &[String]) -> Result<(), String> {
    if target_contains_import_marker(recheck, markers) {
        return Err(
            "拒绝删除：目标目录子树内含有商店卡片就地发现模型所用的标记文件，无法确认它不是在册卡片软链引用的原始权重目录，为防误删已拒绝".to_string(),
        );
    }
    Ok(())
}

/// 删除时刻的登记权重文件名复查（纯函数 + 存在性探测，测试直打）：与边界
/// 闭包的文件名判定镜像同一口径（[`target_contains_registered_weight_file`]），
/// 收窄「守卫时刻判断正确、删除时刻目录已被换成含登记权重文件的形状」的窗口。
fn ensure_recheck_free_of_registered_weight_files(
    recheck: &Path,
    filenames: &[String],
) -> Result<(), String> {
    if target_contains_registered_weight_file(recheck, filenames) {
        return Err(
            "拒绝删除：目标目录子树内含有在册商店卡片登记的权重文件，无法确认它不是仍在使用的商店权重目录，为防误删已拒绝".to_string(),
        );
    }
    Ok(())
}

/// 删除时刻的零痕迹旧根名字复查（纯函数 + 存在性探测，测试直打）：与边界
/// 闭包的名字判定镜像同一口径（[`target_matches_candidate_names`]），收窄
/// 「守卫时刻判断正确、删除时刻目录已被换成零痕迹旧根形状」的窗口。
fn ensure_recheck_not_candidate_named(
    recheck: &Path,
    candidate_names: &[String],
) -> Result<(), String> {
    if target_matches_candidate_names(recheck, candidate_names) {
        return Err(
            "拒绝删除：目标目录名与在册商店卡片的权重目录约定同名，且不在任何已知商店根之下，无法确认它不是仍被引用的原始权重（旧版本自定义权重根不留痕），为防误删已拒绝".to_string(),
        );
    }
    Ok(())
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
    // 写路径恒写 schema:1；更高版本意味着字段语义变化，按 v1 规则给缺省值
    // 静默误读比直接拒绝危险——注册表是 AI/手写共用文件（十二轮评审 P3）。
    // 未知**新增**字段 serde 天然容忍，不受此限。
    if record.schema > 1 {
        return Err(format!(
            "注册表记录 schema 版本过新（{0} > 1），请升级应用后再试",
            record.schema
        ));
    }
    Ok(record)
}

pub(crate) fn read_registry(dir: &Path) -> (Vec<ExternalModel>, Vec<String>) {
    let mut warnings = Vec::new();
    let entries = match std::fs::read_dir(dir) {
        Ok(entries) => entries,
        // 目录不存在 = 还没有任何外部模型，属正常首跑。
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return (Vec::new(), warnings);
        }
        // 目录存在却读不了（权限/挂载异常）：按空列表静默返回会让用户面对
        // "模型全消失"的假象（第九轮评审 P3）——推一条警告进 warnings 通道，
        // 视图如实呈现。
        Err(error) => {
            warnings.push(format!("注册表目录无法读取（{}）：{error}", dir.display()));
            return (Vec::new(), warnings);
        }
    };
    let mut paths: Vec<PathBuf> = entries
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .filter(|path| path.extension().and_then(|ext| ext.to_str()) == Some("json"))
        .collect();
    paths.sort();
    // 扫描顺带重收敛权限（第七轮评审 P3）：doctor 安装模板教 AI 用 mkdir+cat
    // 自写注册表文件，chmod 被跳过时目录/记录（含 base_url/weights_path）会按
    // 进程 umask 留下组/其他可读位。写入端 atomic_write_private 的 open 时 0600
    // 仍是主防线，这里对既有文件做尽力补偿；本函数整个跑在 blocking 池里，
    // 权限调用失败也不影响读取本身。硬收敛必须排在截断**之前**、覆盖全部发现
    // 的 .json 文件（第九轮评审 P3：原先只收敛截断后保留的条目，被
    // MAX_REGISTRY_FILES 挤出的文件永远得不到收敛）。
    // 目录本身与下方文件条目同一纪律（round-22 review P2）：harden 的
    // set_permissions 跟随符号链接，注册表目录被换成指向用户其他目录的链接时，
    // 每次扫描都会把**目标目录** chmod 成 0700（group/other 位被无声收窄）。
    // symlink_metadata 不跟随——是链接就跳过收敛（记录读取照常跟随）。
    let dir_is_regular = std::fs::symlink_metadata(dir)
        .map(|meta| !meta.file_type().is_symlink())
        .unwrap_or(false);
    if dir_is_regular {
        super::platform::harden_dir_owner_only(dir);
    }
    for path in &paths {
        // 符号链接条目跳过收敛：harden 的 set_permissions 跟随链接，会把链接
        // **目标**（用户自己的任意文件/目录）chmod 成 0600——目录目标会因此
        // 丢掉 x 位无法进入，且成功路径静默无提示（十九轮评审 P2）。链接记录
        // 的读取不受影响（read_record 跟随读），只是不对其目标执行 chmod。
        let is_regular = path
            .symlink_metadata()
            .map(|meta| !meta.file_type().is_symlink())
            .unwrap_or(false);
        if is_regular {
            super::platform::harden_file_owner_only(path);
        }
    }
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
                // 内容 id 形状不合法（含 :/超长/控制字符）：照常展示，但移除被
                // is_safe_id 一票挡死——不说破的话用户面对的是"永远删不掉"的
                // 僵尸记录（五轮评审 P3）。
                if !record.id.trim().is_empty() && !is_safe_id(record.id.trim()) {
                    warnings.push(format!(
                        "注册表记录 id 形状不合法（{}），无法从页面移除，请手工处理文件",
                        path.display()
                    ));
                }
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
    warn_duplicate_identities(&models, &mut warnings);
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
            // 不串化原始 error：reqwest 的 Display 会追加完整 URL（自定义
            // ollama_host 可带 userinfo/内网主机名），而这条 Err 会进 warnings/
            // UI——与 human_probe_error 刻意不透传原始错误同一纪律（第六轮
            // 评审 P3）。
            .map_err(|_| "Ollama 返回内容无法读取".to_string())?;
        let Some(chunk) = chunk else { break };
        append_chunk_bounded(&mut body, &chunk, MAX_TAGS_BYTES)?;
    }
    let text =
        String::from_utf8(body).map_err(|_| "Ollama 返回内容无法解析（非 UTF-8）".to_string())?;
    let value: Value =
        serde_json::from_str(&text).map_err(|error| format!("Ollama 返回内容无法解析：{error}"))?;
    Ok(parse_ollama_tag_names(&value))
}

/// 分块聚合的有界读（纯函数，测试直打）：累计超过上限即拒绝，不把这一块追加进
/// 去。此前内联在读取循环里，死端口 e2e 连接层就失败、永不进入循环，把比较改成
/// `if false` 全套测试照绿（二十一轮评审 P2 变异实证）。
fn append_chunk_bounded(body: &mut Vec<u8>, chunk: &[u8], max_bytes: usize) -> Result<(), String> {
    if body.len() + chunk.len() > max_bytes {
        return Err(format!(
            "Ollama 响应过大（超过 {max_bytes} 字节），拒绝解析"
        ));
    }
    body.extend_from_slice(chunk);
    Ok(())
}

/// 从已解析的 /api/tags JSON 里取模型名。字节数有 1MB 上界但**条目数**没有——
/// 假扮 Ollama 的本地进程用最小 JSON 对象可以塞进 ~8 万个名字，而每个名字都会
/// 在 async 侧经过一次 O(列表) 的去重扫描并变成 UI 一行（第七轮评审 P3）。上限
/// 给到 1024：真实机器上的 tag 数远用不到，超限部分直接截断（该响应本来就不可
/// 信，不值得为它保留完整列表）。
fn parse_ollama_tag_names(value: &Value) -> Vec<String> {
    const MAX_TAG_ENTRIES: usize = 1024;
    value
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
                .take(MAX_TAG_ENTRIES)
                .collect()
        })
        .unwrap_or_default()
}

/// Ollama 删除的独立预算：删几十 GB blob 的守护进程侧耗时不可控，沿用探测的
/// 1.5s 会把"实际已删/将删"误报成"删除失败可手动处理"（第三轮评审 P3）。
const OLLAMA_DELETE_TIMEOUT: Duration = Duration::from_secs(30);

/// Ollama 删除状态码的纯逻辑（测试直打，第二十轮评审 P3）：2xx 与 404 按成功
/// 处理，其余（含无法再跟随的终态 3xx，如 304）报"服务响应异常"——此前
/// `status < 400` 把 1xx/3xx 也当成功，与本注释自相矛盾（二十一轮评审 P2，外部
/// 评审同指）。
fn interpret_ollama_delete_status(status: u16) -> Result<(), String> {
    if (200..300).contains(&status) {
        return Ok(());
    }
    if status == reqwest::StatusCode::NOT_FOUND.as_u16() {
        // 404 = 模型已不存在（重复移除/上次删除成功但警告过）：按幂等成功处理，
        // 不再报"服务响应异常（HTTP 404）"诱导用户去手动清理已经不存在的东西
        // （十九轮评审 P3）。连接层失败（守护进程整个不在）走 human_probe_error，
        // 不会落到这里。
        return Ok(());
    }
    Err(format!("服务响应异常（HTTP {status}）"))
}

/// 删除请求的构造（纯函数，测试直打，round-22 评审 P3）：此前 DELETE 方法、
/// `/api/delete` 路径、`{"name": model}` 请求体与 30s 预算全部零覆盖——改方法、
/// 改端点、把预算退回探测的 1.5s 都会让全套测试照绿，删除失败只会以"可手动
/// 处理"的警告收场而不惊动任何人。
fn ollama_delete_request(
    client: &reqwest::Client,
    base: &str,
    model: &str,
) -> reqwest::RequestBuilder {
    client
        .delete(format!("{base}/api/delete"))
        .json(&serde_json::json!({ "name": model }))
        .timeout(OLLAMA_DELETE_TIMEOUT)
}

/// Ollama 删除：`DELETE /api/delete {"name": model}`。基地址走**可失败**解析
/// （三十二轮评审 P2）：非默认 `ollama_host` 的机器上，列表刚从原实例读出、
/// 确认移除前 settings 恰变得不可读/损坏被隔离时，宽松回退会把 DELETE 改投
/// 默认 11434——默认实例上恰有同名非商店模型就会被真删，原实例对象原封不动。
/// fail-closed 拒绝后由守卫 4 的既有警告通道给出人话原因，恢复配置即可重试；
/// 破坏性目标未知时宁可少删，不能删错实例。
async fn ollama_delete(model: &str) -> Result<(), String> {
    let base = super::ollama_base_checked().await?;
    ollama_delete_against(&base, model).await
}

/// 对**已解析**基地址执行删除（发送与状态判读，测试直打）：删什么、发到哪、
/// 怎么判读全部由参数决定——解析失败在调用方 `?` 处短路，不会构造任何请求。
async fn ollama_delete_against(base: &str, model: &str) -> Result<(), String> {
    let client = probe_client().ok_or_else(|| "HTTP 客户端初始化失败".to_string())?;
    let response = ollama_delete_request(&client, base, model)
        .send()
        .await
        .map_err(human_probe_error)?;
    interpret_ollama_delete_status(response.status().as_u16())
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
            Some(weights) if weights_dir_present(weights).await => "available".to_string(),
            _ => "unreachable".to_string(),
        },
    }
}

/// 权重目录存在性探测：stat 与 canonicalize 同类，陈旧 NFS/FUSE 挂载上可能
/// 阻塞数秒——本函数由 `get_external_models` 的 join_all 与登记回显直接 await，
/// 必须与本文件其余文件 IO 同口径入 blocking 池，不能占住 async worker
/// （第七轮评审 P2）。JoinError 按"不可达"降级，不放大成整次刷新失败。
async fn weights_dir_present(path: &str) -> bool {
    let path = path.to_string();
    tokio::task::spawn_blocking(move || Path::new(&path).is_dir())
        .await
        .unwrap_or(false)
}

/// 权重目录存在性三分类：`Ok(true)`=存在可继续删除；`Ok(false)`=确认
/// NotFound（幂等收尾臂）；`Err`=读不了（权限/IO/挂载异常）。移除臂的
/// 存在性预检必须走本函数而不是 `Path::exists()`：后者把 `Err` 折叠成
/// false，仍存在的目录会被按"已删除"收敛并删掉登记，丢失重试入口
/// （round-29 review P2；与 `read_registry` 的同族修法同口径）。
fn weights_dir_presence(path: &Path) -> Result<bool, std::io::Error> {
    match std::fs::metadata(path) {
        Ok(_) => Ok(true),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(error) => Err(error),
    }
}

/// `http://host:port/v1` → `http://host:port/v1/models`（已带 /models 则原样）。
/// 带 query/fragment 的基地址（`…/v1?x=1`）把 `/models` 插到 `?`/`#` 之前：
/// 按整串追加会产出 `…/v1?x=1/models`，探测永远 404（第九轮评审 P3）。
fn endpoint_models_url(base_url: &str) -> String {
    let cut = base_url
        .find(|c: char| c == '?' || c == '#')
        .unwrap_or(base_url.len());
    let (base, suffix) = base_url.split_at(cut);
    let trimmed = base.trim_end_matches('/');
    if trimmed.ends_with("/models") {
        format!("{trimmed}{suffix}")
    } else {
        format!("{trimmed}/models{suffix}")
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

    // 守卫 2.5 绑定矩阵（2026-11-04 评审 P3）：展示端点 = 基地址 + "/v1" 的
    // 归一层必须剥掉再比对；任何 host/端口/scheme 分量变化都是换实例，整单
    // 拒绝；expected 缺失不给静默放行。
    #[test]
    fn daemon_binding_accepts_same_instance_in_display_shapes() {
        let base = "http://127.0.0.1:11434";
        assert!(ensure_ollama_base_binding(Some(base), base).is_ok());
        // 展示形状（列表行自带 /v1、可能带尾斜杠）与当前裸基地址同实例。
        assert!(ensure_ollama_base_binding(Some("http://127.0.0.1:11434/v1"), base).is_ok());
        assert!(ensure_ollama_base_binding(Some("http://127.0.0.1:11434/v1/"), base).is_ok());
        // 首尾空白是前端载荷的正常噪声。
        assert!(ensure_ollama_base_binding(Some("  http://127.0.0.1:11434/v1  "), base).is_ok());
    }

    #[test]
    fn daemon_binding_rejects_any_instance_component_change() {
        let base = "http://127.0.0.1:11434";
        // 端口变了 = 新实例（哪怕新实例恰好有同名模型，也不该改投）。
        let port = ensure_ollama_base_binding(Some("http://127.0.0.1:11500/v1"), base)
            .expect_err("port change must be refused");
        assert!(
            port.contains("已变化") && port.contains("刷新"),
            "got: {port}"
        );
        // host 变了同理。
        assert!(ensure_ollama_base_binding(Some("http://127.0.0.2/v1"), base).is_err());
        // scheme 变了同理。
        assert!(ensure_ollama_base_binding(Some("https://127.0.0.1:11434"), base).is_err());
        // 路径分量不同（不只是 /v1 归一层）也是不同实例。
        assert!(ensure_ollama_base_binding(Some("http://127.0.0.1:11434/v2"), base).is_err());
        // "/v1" 只剥一层：裸基地址恰好以别的段结尾不受益。
        assert!(ensure_ollama_base_binding(Some("http://127.0.0.1:11434/ollama"), base).is_err());
    }

    #[test]
    fn installed_refs_recheck_rejects_intersections_and_passes_disjoint_targets() {
        let refs = vec![
            PathBuf::from("/home/u/legacy-store/hf/qwen-weights"),
            PathBuf::from("/home/u/data/llm/qwen"),
        ];
        // 相等即命中。
        assert!(
            ensure_recheck_outside_installed_refs(
                Path::new("/home/u/legacy-store/hf/qwen-weights"),
                &refs
            )
            .is_err()
        );
        // 目标在引用之下（删子目录同样破坏该引用）。
        assert!(
            ensure_recheck_outside_installed_refs(
                Path::new("/home/u/legacy-store/hf/qwen-weights/blobs/sha256-x"),
                &refs
            )
            .is_err()
        );
        // 祖先方向：目标包含引用 = 连根删除，同样拒绝。
        assert!(
            ensure_recheck_outside_installed_refs(Path::new("/home/u/legacy-store"), &refs)
                .is_err()
        );
        // 与全部引用都不相交 → 放行。
        assert!(
            ensure_recheck_outside_installed_refs(Path::new("/home/u/own-models/unrelated"), &refs)
                .is_ok()
        );
        // 空引用集合（无在册卡片/非商店平台）恒放行。
        assert!(
            ensure_recheck_outside_installed_refs(
                Path::new("/home/u/legacy-store/hf/qwen-weights"),
                &[]
            )
            .is_ok()
        );
    }

    // 零痕迹旧根名字臂矩阵（2026-11-04 评审 P2 zhuowp 场景）：约定形状恰是
    // $ROOT/<name> 一层——叶删（目标自身名命中）与父删（一级子目录命中）都
    // 必须拦；不相关名放行（该臂不误伤无关目录）；空名字集恒放行（无在册
    // 卡片/非商店平台）。删除时刻复查与边界闭包共用同一判定，顺带钉其文案。
    #[test]
    fn candidate_name_arm_hits_leaf_and_first_level_child_only() {
        let names = vec!["Repo".to_string(), "my-model".to_string()];
        let dir = TempDir::new("legacy-root-names");
        // 叶删：目标就是约定名目录（名字比对纯计算，无需盘上存在）。
        assert!(target_matches_candidate_names(
            &dir.path().join("Repo"),
            &names
        ));
        assert!(target_matches_candidate_names(
            &dir.path().join("my-model"),
            &names
        ));
        // 父删：目标之下恰好一层存在约定名子目录（需要真实盘上目录）。
        std::fs::create_dir_all(dir.path().join("legacy-custom").join("Repo"))
            .expect("create first-level candidate");
        assert!(target_matches_candidate_names(
            &dir.path().join("legacy-custom"),
            &names
        ));
        // 更深祖先：约定名在两级之下，两查都落空——「宁可漏」边界如实放行。
        std::fs::create_dir_all(dir.path().join("deep").join("mid").join("Repo"))
            .expect("create deeper candidate");
        assert!(
            !target_matches_candidate_names(&dir.path().join("deep"), &names),
            "ancestors beyond one level must stay outside the arm's reach"
        );
        // 不相关名放行：叶名与一级子目录名都不在约定名集合。
        let unrelated = dir.path().join("unrelated-root");
        std::fs::create_dir_all(unrelated.join("user-stuff")).expect("create unrelated tree");
        assert!(!target_matches_candidate_names(&unrelated, &names));
        // 大小写敏感：约定名比较按原串，不折叠（与 safe_path_component 保留
        // 原样一致；大小写变形的目录不是约定形状）。
        assert!(!target_matches_candidate_names(
            &dir.path().join("repo"),
            &names
        ));
        // 空名字集（无在册卡片/非商店平台）恒放行。
        assert!(!target_matches_candidate_names(
            &dir.path().join("Repo"),
            &[]
        ));
        // 删除时刻复查：命中拒绝且文案点名 fail-closed 语义，不相关名通过。
        let error = ensure_recheck_not_candidate_named(&dir.path().join("Repo"), &names)
            .expect_err("leaf hit must be refused at deletion time");
        assert!(
            error.contains("约定同名") && error.contains("为防误删已拒绝"),
            "got: {error}"
        );
        assert!(ensure_recheck_not_candidate_named(&unrelated, &names).is_ok());
    }

    // 2026-11-05 评审 P2 zhuowp 场景 II：官方卡片下载脚本按标记文件
    // （model_index.json / model.safetensors.index.json）就地发现用户自取名
    // 的原始模型目录再软链——名字臂/路径臂对这种原目录失明。目标含标记文件
    // 且仍有在册卡片时必须拦（归属无法确认），删光在册卡片后自动放行。
    #[test]
    fn import_marker_detection_covers_whole_subtree() {
        let dir = TempDir::new("import-markers");
        let root = dir.path();

        // 叶删：目标就是原目录（Z-Image-Turbo 官方脚本的发现层）。
        let leaf = root.join("my-zimage-ckpt");
        std::fs::create_dir_all(&leaf).unwrap();
        std::fs::write(leaf.join("model_index.json"), "{}").unwrap();
        let markers = [
            "model_index.json".to_string(),
            "model.safetensors.index.json".to_string(),
        ];
        assert!(target_contains_import_marker(&leaf, &markers));

        // 叶删（第二个标记）：内置 ornith15/qwen38 卡的发现层。
        let sharded = root.join("qwen-weights");
        std::fs::create_dir_all(&sharded).unwrap();
        std::fs::write(sharded.join("model.safetensors.index.json"), "{}").unwrap();
        assert!(target_contains_import_marker(&sharded, &markers));

        // 父删：目标是原目录的父目录，一级子目录含标记。
        let parent = root.join("downloads-bundle");
        std::fs::create_dir_all(parent.join("inside-model")).unwrap();
        std::fs::write(parent.join("inside-model").join("model_index.json"), "{}").unwrap();
        assert!(target_contains_import_marker(&parent, &markers));

        // 2026-11-05 评审 P2（zhuowp 5783007585）的深层专用容器：任意名原
        // 目录可落在删除目标的深层——整树递归删除不得漏掉仍被软链引用的
        // 深层权重源。
        let container = root.join("assets").join("bundles").join("custom");
        let checkpoint = container.join("releases").join("checkpoint");
        std::fs::create_dir_all(&checkpoint).unwrap();
        std::fs::write(checkpoint.join("model_index.json"), "{}").unwrap();
        assert!(target_contains_import_marker(&container, &markers));

        // 2026-11-05 评审 P2（zhuowp 5783998563）起点谬误组合：内层扫描根
        // R=T/versions/release（脚本 find -maxdepth 4 以 R 起算，S 相对 R
        // 恰为第 4 层文件），外层删除目标 T——S 相对 T 是第 5 层，任何以
        // 目标起算的深度上限都会漏。全子树走查必须命中。
        let t = root.join("ai").join("private").join("bundle");
        let s = t
            .join("versions")
            .join("release")
            .join("assets")
            .join("models")
            .join("checkpoint");
        std::fs::create_dir_all(&s).unwrap();
        std::fs::write(s.join("model_index.json"), "{}").unwrap();
        assert!(target_contains_import_marker(&t, &markers));

        // 更深（第 6 层）同样命中：深度上限与脚本起点不同源，一并不设。
        let deeper = s.join("sub").join("weights");
        std::fs::create_dir_all(&deeper).unwrap();
        std::fs::write(deeper.join("model_index.json"), "{}").unwrap();
        assert!(target_contains_import_marker(&t, &markers));

        // 深层分支读不了：该分支按未命中放行（其余分支照常）。
        let unreadable = root.join("unreadable").join("sub");
        std::fs::create_dir_all(&unreadable).unwrap();
        std::fs::write(unreadable.join("model_index.json"), "{}").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let unreadable_root = root.join("unreadable");
            let mut locked = std::fs::metadata(&unreadable_root).unwrap().permissions();
            locked.set_mode(0o000);
            std::fs::set_permissions(&unreadable_root, locked).unwrap();
            let hit = target_contains_import_marker(&unreadable_root, &markers);
            let mut restored = std::fs::metadata(&unreadable_root).unwrap().permissions();
            restored.set_mode(0o755);
            std::fs::set_permissions(&unreadable_root, restored).unwrap();
            assert!(!hit, "unreadable branch counts as a miss");
        }

        // 标记必须是文件（与脚本 find -type f 同口径）：同名目录不算。
        let dir_marker = root.join("dir-marker");
        std::fs::create_dir_all(dir_marker.join("model_index.json")).unwrap();
        assert!(!target_contains_import_marker(&dir_marker, &markers));

        // 空集合（没有在册卡片）恒不命中——卸载卡片后本臂关闭。
        assert!(!target_contains_import_marker(&leaf, &[]));
        // 目标不存在 → 未命中（删除路径随后按"已删除"收敛）。
        assert!(!target_contains_import_marker(
            &root.join("absent"),
            &markers
        ));
    }

    #[test]
    fn import_marker_recheck_mirrors_boundary_verdict() {
        let dir = TempDir::new("import-marker-recheck");
        let markers = ["model_index.json".to_string()];
        // 叶删与父删形态在删除时刻复查同样拒绝，文案点名 fail-closed 语义。
        let leaf = dir.path().join("orig");
        std::fs::create_dir_all(&leaf).unwrap();
        std::fs::write(leaf.join("model_index.json"), "{}").unwrap();
        let error = ensure_recheck_free_of_import_markers(&leaf, &markers)
            .expect_err("leaf marker must be refused at deletion time");
        assert!(
            error.contains("标记文件") && error.contains("为防误删已拒绝"),
            "got: {error}"
        );
        let parent = dir.path().join("bundle");
        std::fs::create_dir_all(parent.join("child")).unwrap();
        std::fs::write(parent.join("child").join("model_index.json"), "{}").unwrap();
        assert!(ensure_recheck_free_of_import_markers(&parent, &markers).is_err());
        // 干净目录与空集合都放行。
        let clean = dir.path().join("clean");
        std::fs::create_dir_all(&clean).unwrap();
        assert!(ensure_recheck_free_of_import_markers(&clean, &markers).is_ok());
        assert!(ensure_recheck_free_of_import_markers(&leaf, &[]).is_ok());
    }

    #[test]
    fn registered_weight_file_detection_covers_whole_subtree() {
        let dir = TempDir::new("weight-filenames");
        let root = dir.path();
        let filenames = ["model-Q4_K_M.gguf".to_string()];

        // CR 5797725367 场景：权重目录被改名（my-ckpt 不匹配任何约定名）、
        // 目录内无标记文件——登记的权重文件名是唯一存活引用数据。
        let renamed = root.join("my-ckpt");
        std::fs::create_dir_all(&renamed).unwrap();
        std::fs::write(renamed.join("model-Q4_K_M.gguf"), "weights").unwrap();
        assert!(target_contains_registered_weight_file(&renamed, &filenames));

        // 祖先删：目标是改名目录的父目录，深层子树内的权重文件必须命中
        // （与标记臂同一走查半径=删除物理范围，任意深度不设上限）。
        let bundle = root.join("release-bundle");
        let deep = bundle.join("versions").join("r1").join("weights");
        std::fs::create_dir_all(&deep).unwrap();
        std::fs::write(deep.join("model-Q4_K_M.gguf"), "weights").unwrap();
        assert!(target_contains_registered_weight_file(&bundle, &filenames));

        // 通用走查口径镜像标记臂：目标不存在不命中；同名目录不算（find -type f
        // 同口径）；名单外文件不算；空集合恒不命中。
        assert!(!target_contains_registered_weight_file(
            &root.join("absent"),
            &filenames
        ));
        let dir_named = root.join("dir-named");
        std::fs::create_dir_all(dir_named.join("model-Q4_K_M.gguf")).unwrap();
        assert!(!target_contains_registered_weight_file(
            &dir_named, &filenames
        ));
        let other = root.join("other");
        std::fs::create_dir_all(&other).unwrap();
        std::fs::write(other.join("unrelated.bin"), "x").unwrap();
        assert!(!target_contains_registered_weight_file(&other, &filenames));
        assert!(!target_contains_registered_weight_file(&renamed, &[]));
    }

    #[test]
    fn registered_weight_file_recheck_mirrors_boundary_verdict() {
        let dir = TempDir::new("weight-filename-recheck");
        let filenames = ["model-Q4_K_M.gguf".to_string()];
        let leaf = dir.path().join("renamed-ckpt");
        std::fs::create_dir_all(&leaf).unwrap();
        std::fs::write(leaf.join("model-Q4_K_M.gguf"), "weights").unwrap();
        let error = ensure_recheck_free_of_registered_weight_files(&leaf, &filenames)
            .expect_err("registered weight file must be refused at deletion time");
        assert!(
            error.contains("权重文件") && error.contains("为防误删已拒绝"),
            "got: {error}"
        );
        let parent = dir.path().join("bundle");
        std::fs::create_dir_all(parent.join("child")).unwrap();
        std::fs::write(parent.join("child").join("model-Q4_K_M.gguf"), "w").unwrap();
        assert!(ensure_recheck_free_of_registered_weight_files(&parent, &filenames).is_err());
        let clean = dir.path().join("clean");
        std::fs::create_dir_all(&clean).unwrap();
        assert!(ensure_recheck_free_of_registered_weight_files(&clean, &filenames).is_ok());
        assert!(ensure_recheck_free_of_registered_weight_files(&leaf, &[]).is_ok());
    }

    #[test]
    fn daemon_binding_refuses_missing_binding_instead_of_silent_passthrough() {
        // 旧调用方/存量行没带绑定：拒绝并要求刷新，绝不静默当"无绑定"放行
        // ——那正是评审洞的形状（删除时刻重新解析、改投他实例）。
        let error = ensure_ollama_base_binding(None, "http://127.0.0.1:11434")
            .expect_err("missing binding must be refused");
        assert!(error.contains("缺少守护进程地址绑定"), "got: {error}");
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
        // schema 版本过新：按 v1 默认值静默误读比拒绝危险（十二轮评审 P3）。
        assert!(parse_record(r#"{"name": "x", "schema": 2}"#).is_err());
        // 缺 name（schema 必填）。
        assert!(parse_record(r#"{"id": "m-1"}"#).is_err());
        assert!(parse_record(r#"{"name": ""}"#).is_err());
        assert!(parse_record(r#"[]"#).is_err());
    }

    #[test]
    fn weights_deletion_guard_accepts_deep_absolute_dir_under_home() {
        let home = TempDir::new("guard-home");
        let weights = home.path().join("ai").join("weights").join("qwen3");
        std::fs::create_dir_all(&weights).expect("create weights dir");
        let target =
            weights_deletion_guard(home.path(), None, &[], &weights).expect("guard must pass");
        assert_eq!(
            target,
            weights.canonicalize().expect("weights canonicalize")
        );
    }

    #[test]
    fn weights_deletion_guard_rejects_the_ollama_models_tree() {
        // daemon 模型目录整体拒绝（第八轮评审 P1）：blob 全删等于抹掉 daemon
        // 托管的一切权重（商店 ollama 卡片经 ollama create 导入的也在内），
        // 与记录自述的 kind 无关——`~/.ollama/models/blobs` 恰好家目录三层，
        // 深度臂本会放行。
        let home = TempDir::new("guard-ollama");
        let root = home.path().join(".ollama").join("models");
        let blobs = root.join("blobs");
        std::fs::create_dir_all(&blobs).expect("create blobs dir");
        assert!(
            weights_deletion_guard(home.path(), None, std::slice::from_ref(&root), &blobs).is_err()
        );
        // 根本身同样拒绝。
        assert!(
            weights_deletion_guard(home.path(), None, std::slice::from_ref(&root), &root).is_err()
        );
        // 守卫之外的家目录深目录不受牵连。
        let weights = home.path().join("ai").join("weights").join("qwen3");
        std::fs::create_dir_all(&weights).expect("create weights");
        assert!(
            weights_deletion_guard(home.path(), None, std::slice::from_ref(&root), &weights)
                .is_ok()
        );
        // 根不存在（未装 Ollama）时跳过该臂：目标存在而根不存在，两个方向的
        // 包含都不可能成立。
        let missing = home.path().join("no-ollama").join("models");
        assert!(
            weights_deletion_guard(home.path(), None, std::slice::from_ref(&missing), &weights)
                .is_ok()
        );
        // 祖先方向（二十六轮评审 P1）：目标是候选根的祖先必须拒绝——env 迁移
        // 根比目标更深时（如 OLLAMA_MODELS 指向权重目录内的 models/），单向
        // 判断放行，递归删除连根清掉 daemon 候选根。祖先深度 ≥ 3 家目录层，
        // 深度臂不再兜底。
        let deep_env_root = weights.join("daemon-models");
        std::fs::create_dir_all(&deep_env_root).expect("create env root");
        assert!(
            weights_deletion_guard(
                home.path(),
                None,
                std::slice::from_ref(&deep_env_root),
                &weights
            )
            .is_err()
        );
    }

    #[test]
    fn ollama_model_roots_merge_env_migration_with_default() {
        let home = Path::new("/home/u");
        // 默认：~/.ollama/models 单根。
        assert_eq!(
            ollama_model_roots(None, home),
            vec![home.join(".ollama").join("models")]
        );
        // OLLAMA_MODELS 迁移目录（trim 容忍 env 文件里的空白）与默认根**并集**：
        // env 写错/指向未建目录时默认根仍在保护集内（第二十轮评审 P2）。
        assert_eq!(
            ollama_model_roots(Some(" /mnt/ollama-models "), home),
            vec![
                PathBuf::from("/mnt/ollama-models"),
                home.join(".ollama").join("models")
            ]
        );
        // 空白值视同未设。
        assert_eq!(
            ollama_model_roots(Some("   "), home),
            vec![home.join(".ollama").join("models")]
        );
        // env 恰为默认根时去重。
        assert_eq!(
            ollama_model_roots(Some("/home/u/.ollama/models"), home),
            vec![home.join(".ollama").join("models")]
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
        assert!(weights_deletion_guard(home.path(), Some(&app_root), &[], &registry).is_err());
        assert!(weights_deletion_guard(home.path(), Some(&app_root), &[], &deep_app).is_err());
        // 保护根本身同样拒绝。
        assert!(weights_deletion_guard(home.path(), Some(&app_root), &[], &app_root).is_err());
        // 祖先方向（二十六轮评审 P1）：目标是保护根的祖先同样拒绝——实际到
        // 不了（目标是家目录本身，后被家目录臂拒绝），这里钉住纵深，与商店/
        // Ollama 根的相交臂同口径。
        assert!(weights_deletion_guard(home.path(), Some(&app_root), &[], home.path()).is_err());
        // 同深度、保护根之外的家目录仍放行。
        let weights = home.path().join("ai").join("weights").join("qwen3");
        std::fs::create_dir_all(&weights).expect("create weights");
        assert!(weights_deletion_guard(home.path(), Some(&app_root), &[], &weights).is_ok());
    }

    #[test]
    fn weights_deletion_guard_rejects_the_guard_matrix() {
        let home = TempDir::new("guard-matrix");
        let deep = home.path().join("a").join("b").join("c");
        std::fs::create_dir_all(&deep).expect("create weights dir");

        // 相对路径。
        let relative = Path::new("relative/weights/dir");
        assert!(weights_deletion_guard(home.path(), None, &[], relative).is_err());
        // 不存在的目录。
        assert!(
            weights_deletion_guard(home.path(), None, &[], &home.path().join("no/such/dir"))
                .is_err()
        );
        // 普通文件（不是目录）。
        let file = home.path().join("file.txt");
        std::fs::write(&file, b"x").expect("write file");
        assert!(weights_deletion_guard(home.path(), None, &[], &file).is_err());
        // 家目录本身。
        assert!(weights_deletion_guard(home.path(), None, &[], home.path()).is_err());
        // 家目录之外。
        let outside = TempDir::new("guard-outside");
        let outside_weights = outside.path().join("x").join("y").join("z");
        std::fs::create_dir_all(&outside_weights).expect("create outside dir");
        assert!(weights_deletion_guard(home.path(), None, &[], &outside_weights).is_err());
    }

    #[test]
    fn weights_deletion_guard_rejects_shallow_dirs_under_home() {
        // 相对深度 1/2（家目录的一级、二级子目录）必须拒绝。
        let home = TempDir::new("guard-depth");
        let child = home.path().join("weights-dir");
        std::fs::create_dir_all(&child).expect("create child");
        assert!(weights_deletion_guard(home.path(), None, &[], &child).is_err());
        let grandchild = child.join("sub");
        std::fs::create_dir_all(&grandchild).expect("create grandchild");
        assert!(weights_deletion_guard(home.path(), None, &[], &grandchild).is_err());
        // 孙目录（深度 3）放行。名字避开 "blobs" 分量——它是 blob 布局签名臂
        // 的硬拒签名（第九轮评审 P2），无论深度一律拒绝。
        let great = grandchild.join("qwen3");
        std::fs::create_dir_all(&great).expect("create great");
        assert!(weights_deletion_guard(home.path(), None, &[], &great).is_ok());
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
        // 有效删除名由调用方解析（model 字段缺省回退 name），本函数只做归一匹配。
        assert_eq!(
            ollama_status("qwen3:30b", &["qwen3:30b".to_string()]),
            "available"
        );
        assert_eq!(
            ollama_status("qwen3:30b", &["other:8b".to_string()]),
            "unreachable"
        );
        // daemon 标签按小写归一：记录里手写大小写变体不再恒判 unreachable
        // （五轮评审 P3，与商店边界 Ollama 臂的 NOCASE 同一事实）。
        assert_eq!(
            ollama_status("Qwen3:30B", &["qwen3:30b".to_string()]),
            "available"
        );
        // 无 model 字段时由调用方回退按 name 传入。
        assert_eq!(
            ollama_status("qwen3-30b", &["qwen3-30b".to_string()]),
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
    fn registry_identity_folds_ollama_tags_and_case_but_keeps_other_kinds_exact() {
        // Ollama：有效删除名（model 优先）、裸名补 :latest、大小写折叠——与
        // daemon 去重同一把尺子；别名记录 {name:foo:latest, model:bar} 的身份是 bar。
        assert_eq!(
            registry_identity("ollama", "qwen3", None, None),
            ("qwen3:latest".to_string(), String::new())
        );
        assert_eq!(
            registry_identity("ollama", "Qwen3:Latest", None, None).0,
            "qwen3:latest"
        );
        assert_eq!(
            registry_identity("ollama", "foo:latest", Some("bar"), None).0,
            "bar:latest"
        );
        // 端点不参与 Ollama 身份（翻转对连续性）：第二元恒空。
        assert_eq!(
            registry_identity("ollama", "qwen3", None, Some("http://127.0.0.1:11434/v1")).1,
            ""
        );
        // 其余类别：model 回退 name，原样（端点模型标识大小写有意义），端点
        // 参与身份。
        assert_eq!(
            registry_identity(
                "openai_endpoint",
                "qwen3",
                None,
                Some("http://127.0.0.1:8000/v1")
            ),
            ("qwen3".to_string(), "http://127.0.0.1:8000/v1".to_string())
        );
        assert_eq!(
            registry_identity("openai_endpoint", "qwen3", Some("Qwen3-8B"), None).0,
            "Qwen3-8B"
        );
        assert_ne!(
            registry_identity("other", "Qwen3", None, None),
            registry_identity("other", "qwen3", None, None)
        );
        // 同名同模型、不同端点是两个身份（设计文档：独立服务）。
        assert_ne!(
            registry_identity(
                "openai_endpoint",
                "qwen3",
                None,
                Some("http://127.0.0.1:8000/v1")
            ),
            registry_identity(
                "openai_endpoint",
                "qwen3",
                None,
                Some("http://127.0.0.1:8001/v1")
            )
        );
        // 端点参与身份用原串精确比较（登记端已 trim）：毗邻删除守卫的身份链
        // 取"宁分勿合"——`http://host:80` 与 `http://host` 至多产出两条可独立
        // 移除的记录，不会误合；前端 findLinkedRowsForEndpoint 的 WHATWG 尺子
        // 折叠默认端口是品悟配置行清理域的既有契约，两把尺子用途不同不互借。
        assert_ne!(
            registry_identity(
                "openai_endpoint",
                "qwen3",
                None,
                Some("http://127.0.0.1:8000")
            ),
            registry_identity("openai_endpoint", "qwen3", None, Some("http://127.0.0.1"))
        );
    }

    #[test]
    fn identity_conflict_detection_matches_same_kind_only_and_skips_daemon_rows() {
        let make = |id: &str,
                    kind: &str,
                    name: &str,
                    model: Option<&str>,
                    base_url: Option<&str>,
                    registered: bool| {
            ExternalModel {
                id: id.to_string(),
                name: name.to_string(),
                kind: kind.to_string(),
                base_url: base_url.map(str::to_string),
                model: model.map(str::to_string),
                weights_path: None,
                status: "unreachable".to_string(),
                registered,
                source: if registered { "registry" } else { "ollama" }.to_string(),
            }
        };
        let existing = vec![
            make(
                "m-1",
                "openai_endpoint",
                "qwen3",
                None,
                Some("http://127.0.0.1:8000/v1"),
                true,
            ),
            make("m-2", "ollama", "foo:latest", Some("bar"), None, true),
            make(
                "ollama-baz:latest",
                "ollama",
                "baz:latest",
                None,
                None,
                false,
            ),
        ];
        // 同 kind + 同模型 + 同端点 → 冲突。
        let hit = find_identity_conflict(
            &existing,
            "openai_endpoint",
            "qwen3",
            None,
            Some("http://127.0.0.1:8000/v1"),
        )
        .expect("same kind + model + endpoint must conflict");
        assert_eq!(hit.id, "m-1");
        // 同 kind 同模型、不同端点是设计文档定义的两个独立服务 → 不冲突
        // （二十三轮评审 P2：不能靠拒绝登记收窄合法配置）。
        assert!(
            find_identity_conflict(
                &existing,
                "openai_endpoint",
                "qwen3",
                None,
                Some("http://127.0.0.1:8001/v1")
            )
            .is_none()
        );
        // 端点带 query 的第三种身份，谁也不匹配（与前端 query 参与身份同口径）。
        assert!(
            find_identity_conflict(
                &existing,
                "openai_endpoint",
                "qwen3",
                None,
                Some("http://127.0.0.1:8000/v1?tenant=a")
            )
            .is_none()
        );
        // 同名不同类别不冲突；同名不同 model（不同端点模型）不冲突。
        assert!(find_identity_conflict(&existing, "other", "qwen3", None, None).is_none());
        assert!(
            find_identity_conflict(
                &existing,
                "openai_endpoint",
                "qwen3",
                Some("qwen3-8b"),
                Some("http://127.0.0.1:8000/v1")
            )
            .is_none()
        );
        // Ollama 按有效删除名，端点不参与：登记 "BAR" 与别名记录的 model bar
        // 冲突（传入任何端点都一样）；登记 "foo:latest"（身份 foo:latest）与
        // 别名记录（身份 bar）不冲突。
        assert!(
            find_identity_conflict(
                &existing,
                "ollama",
                "BAR",
                None,
                Some("http://elsewhere:11434/v1")
            )
            .is_some()
        );
        assert!(find_identity_conflict(&existing, "ollama", "foo:latest", None, None).is_none());
        // daemon 行不是注册表记录，登记它（静默登记路径）不算冲突。
        assert!(find_identity_conflict(&existing, "ollama", "baz", None, None).is_none());
    }

    #[test]
    fn read_registry_warns_on_duplicate_identity_but_keeps_both_records() {
        let dir = TempDir::new("registry-dup-identity");
        std::fs::write(
            dir.path().join("m-a.json"),
            r#"{"id": "m-a", "name": "qwen3", "kind": "openai_endpoint", "base_url": "http://127.0.0.1:8000/v1"}"#,
        )
        .expect("write a");
        std::fs::write(
            dir.path().join("m-b.json"),
            r#"{"id": "m-b", "name": "qwen3", "kind": "openai_endpoint", "base_url": "http://127.0.0.1:8001/v1"}"#,
        )
        .expect("write b");
        std::fs::write(
            dir.path().join("m-c.json"),
            r#"{"id": "m-c", "name": "qwen3", "kind": "other"}"#,
        )
        .expect("write c");
        // 与 m-a 同名同模型且同端点的手写重复（m-d）。
        std::fs::write(
            dir.path().join("m-d.json"),
            r#"{"id": "m-d", "name": "qwen3", "kind": "openai_endpoint", "base_url": "http://127.0.0.1:8000/v1"}"#,
        )
        .expect("write d");
        let (models, warnings) = read_registry(dir.path());
        // 四条都保留（移除按 id 直取各自可删）。m-b 与 m-a 端点独立（设计文档：
        // 独立服务）是合法配置不告警；m-d 与 m-a 同端点同模型只告警一次；不同
        // 类别（m-c）不算。
        assert_eq!(models.len(), 4);
        assert_eq!(warnings.len(), 1, "unexpected warnings: {warnings:?}");
        assert!(
            warnings[0].contains("m-a") && warnings[0].contains("m-d"),
            "{}",
            warnings[0]
        );
        assert!(warnings[0].contains("openai_endpoint"));
        assert!(
            warnings[0].contains("8000"),
            "端点重复的告警应回显服务地址: {}",
            warnings[0]
        );
    }

    #[test]
    fn append_chunk_bounded_rejects_overflow_without_appending() {
        let mut body = Vec::new();
        assert!(append_chunk_bounded(&mut body, &[1, 2, 3], 5).is_ok());
        // 恰到上限放行。
        assert!(append_chunk_bounded(&mut body, &[4, 5], 5).is_ok());
        assert_eq!(body, vec![1, 2, 3, 4, 5]);
        // 再多一字节即拒绝，且被拒的块不追加。
        let error = append_chunk_bounded(&mut body, &[6], 5).unwrap_err();
        assert!(error.contains("响应过大"), "unexpected error: {error}");
        assert_eq!(body.len(), 5);
        // 单块超限同样拒绝。
        let mut fresh = Vec::new();
        assert!(append_chunk_bounded(&mut fresh, &[0; 6], 5).is_err());
        assert!(fresh.is_empty());
    }
    #[test]
    fn removal_boundary_rejects_confirm_mismatch_store_names_and_store_weights() {
        // 双侧都 trim：存量记录名带空白时确认输入 trim 后仍应匹配（Ok 而非 Err）。
        assert!(
            removal_boundary_error(
                " qwen3 ", "qwen3", false, false, false, false, false, false, None, None
            )
            .is_ok(),
            "trailing-space record names must stay removable"
        );
        let error = removal_boundary_error(
            "qwen3", " qwen3x ", false, false, false, false, false, false, None, None,
        )
        .unwrap_err();
        assert!(error.contains("确认名"));
        // 守卫 2 名字臂：名字命中商店名单。
        let error = removal_boundary_error(
            "qwen3",
            "qwen3",
            false,
            false,
            false,
            false,
            false,
            false,
            Some("Qwen3-30B"),
            None,
        )
        .unwrap_err();
        assert!(error.contains("模型商店"));
        // 守卫 2 model 臂：别名记录的 Ollama 有效删除名命中商店名单（第三轮
        // 评审 P1）——名字臂拦不住它，必须单独立臂。
        let error = removal_boundary_error(
            "my-alias",
            "my-alias",
            false,
            false,
            false,
            false,
            false,
            false,
            None,
            Some("qwen3:30b"),
        )
        .unwrap_err();
        assert!(error.contains("模型商店"));
        // 守卫 2 路径臂：权重位于任一商店根之下（别名躲过两个名字臂时仍被拦）。
        let error = removal_boundary_error(
            "qwen3", "qwen3", true, false, false, false, false, false, None, None,
        )
        .unwrap_err();
        assert!(error.contains("模型商店"));
        // 存量引用臂：与在册卡片引用路径相交（2026-11-04 评审 P2）。
        let error = removal_boundary_error(
            "qwen3", "qwen3", false, true, false, false, false, false, None, None,
        )
        .unwrap_err();
        assert!(error.contains("已安装商店模型引用"));
        // 零痕迹旧根名字臂：目录名与在册约定同名（2026-11-04 评审 P2 zhuowp
        // 场景），前四臂全空时仍被拦，且报错来自本臂而非前面的臂。
        let error = removal_boundary_error(
            "qwen3", "qwen3", false, false, true, false, false, false, None, None,
        )
        .unwrap_err();
        assert!(
            error.contains("约定同名") && error.contains("不留痕"),
            "rejection must come from the legacy-root name arm, got: {error}"
        );
        // 自取名原目录标记臂（2026-11-05 评审 P2 zhuowp 场景 II）：前五臂全空、
        // 目标含官方脚本就地发现的标记文件且仍有在册卡片 → 拦，且报错来自本臂。
        let error = removal_boundary_error(
            "qwen3", "qwen3", false, false, false, true, false, false, None, None,
        )
        .unwrap_err();
        assert!(
            error.contains("标记文件") && error.contains("无法确认"),
            "rejection must come from the import-marker arm, got: {error}"
        );
        // 登记权重文件名臂（2026-11-06 评审 P2 zhuowp 场景 III）：目录改名且
        // 无标记时记录里的权重文件名是唯一存活引用，第七臂命中即拒。
        let error = removal_boundary_error(
            "qwen3", "qwen3", false, false, false, false, true, false, None, None,
        )
        .unwrap_err();
        assert!(
            error.contains("权重文件") && error.contains("无法确认"),
            "rejection must come from the registered-filename arm, got: {error}"
        );
        // 七道全过 → Ok。
        assert!(
            removal_boundary_error(
                " qwen3 ", " qwen3 ", false, false, false, false, false, false, None, None
            )
            .is_ok()
        );
    }

    #[test]
    fn removal_boundary_rejects_cross_referenced_registry_weights() {
        // 同仓注册表交叉引用臂（2026-11-07 评审 P2 zhuowp，review 5801985394）：
        // 商店四臂对**本仓注册表**其他记录的权重失明——其余记录的 weights_path
        // 落在目标之内时必须由本臂拒绝，且报错来自本臂而非前面的臂。
        let error = removal_boundary_error(
            "qwen3", "qwen3", false, false, false, false, false, true, None, None,
        )
        .unwrap_err();
        assert!(
            error.contains("其他外部登记模型") && error.contains("先移除"),
            "rejection must come from the registry cross-reference arm, got: {error}"
        );
        // 自身记录排除后不相交：本臂放行（其余臂不在此测试职责内）。
        assert!(
            removal_boundary_error(
                "qwen3", "qwen3", false, false, false, false, false, false, None, None
            )
            .is_ok()
        );
    }

    #[test]
    fn ensure_recheck_outside_registry_weights_rejects_both_directions() {
        // 交集两方向都拒：目标在登记路径之内（连带毁掉该权重）与目标包含登记
        // 路径（递归删除连根清掉），与商店根复查同一口径。
        let others = vec![PathBuf::from("/home/u/ai-weights/qwen3")];
        assert!(
            ensure_recheck_outside_registry_weights(Path::new("/home/u/ai-weights"), &others)
                .is_err()
        );
        assert!(
            ensure_recheck_outside_registry_weights(
                Path::new("/home/u/ai-weights/qwen3"),
                &[PathBuf::from("/home/u/ai-weights")]
            )
            .is_err()
        );
        // 不相交与空集合（没有其他登记）放行。
        assert!(
            ensure_recheck_outside_registry_weights(Path::new("/home/u/other"), &others).is_ok()
        );
        assert!(
            ensure_recheck_outside_registry_weights(Path::new("/home/u/ai-weights"), &[]).is_ok()
        );
    }

    #[test]
    fn other_registry_weights_checked_excludes_self_and_unregistered() {
        let dir = TempDir::new("registry-cross");
        // 自身记录（被移除者）、未登记权重与空白权重的记录跳过；其余纳入。
        std::fs::write(
            dir.path().join("m-self.json"),
            r#"{"id": "m-self", "name": "self", "kind": "other", "weights_path": "/home/u/ai-weights"}"#,
        )
        .expect("write self record");
        std::fs::write(
            dir.path().join("m-other.json"),
            r#"{"id": "m-other", "name": "other", "kind": "other", "weights_path": "/home/u/ai-weights/qwen3"}"#,
        )
        .expect("write other record");
        std::fs::write(
            dir.path().join("m-bare.json"),
            r#"{"id": "m-bare", "name": "bare", "kind": "other"}"#,
        )
        .expect("write bare record");
        std::fs::write(dir.path().join("m-bad.json"), "{ broken").expect("write bad record");
        let weights = other_registry_weights_checked(dir.path(), "m-self")
            .expect("degraded neighbor files must not hard-fail the read");
        assert_eq!(
            weights,
            vec![PathBuf::from("/home/u/ai-weights/qwen3")],
            "only the other registered record's weights path must survive"
        );
        // 目录不存在（还没有任何外部登记）：空集合放行，不是失败。
        let missing = TempDir::new("registry-cross-none").path().join("absent");
        assert!(
            other_registry_weights_checked(&missing, "m-self")
                .expect("missing registry dir is an empty registry, not a failure")
                .is_empty()
        );
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
    fn paths_intersect_covers_equal_descendant_ancestor_sibling_and_alias() {
        // 删除保护的方向臂（二十六轮评审 P1）：单向 `target 在根之下`放行
        // "目标是保护根祖先"的场景——递归删除连根清掉根内的商店权重。相交
        // 判定必须在两个方向都命中，矩阵覆盖评审点名的五类形状。
        let root = TempDir::new("intersect-root");
        let store_managed = root.path().join("store-managed");
        std::fs::create_dir_all(&store_managed).expect("create store root inside target");
        // 后代（目标在根之下，既有单向臂）。
        assert!(paths_intersect(&store_managed, root.path()));
        // 相等（既有单向臂本就覆盖，钉住防止重构回退）。
        assert!(paths_intersect(root.path(), root.path()));
        // 祖先（目标是根的祖先——此前放行，修复后必须拒绝）。
        assert!(paths_intersect(root.path(), &store_managed));
        // 无关兄弟（root 的兄弟目录）不命中。
        let sibling_parent = TempDir::new("intersect-sibling-parent");
        let sibling = sibling_parent.path().join("root");
        std::fs::create_dir_all(&sibling).expect("create sibling");
        assert!(!paths_intersect(&sibling, root.path()));
        // canonical 别名：经由符号链接指到根的路径与根相交（symlink 创建是
        // unix 专属 API；Windows 侧的相交语义由非 symlink 用例覆盖）。
        #[cfg(unix)]
        {
            let alias_dir = TempDir::new("intersect-alias");
            let link = alias_dir.path().join("link");
            std::os::unix::fs::symlink(root.path(), &link).expect("create symlink");
            assert!(paths_intersect(&link, root.path()));
            // 符号链接落在根一侧：目标在真实根之下、根以别名形式给出，仍命中。
            let child = store_managed.join("qwen3");
            std::fs::create_dir_all(&child).expect("create child");
            assert!(paths_intersect(&child, &link));
        }
        // 路径分量比较不是字符串前缀：`root-evil` 与 `root` 共享字符串前缀
        // 但互不相交。
        let evil = sibling_parent.path().join("root-evil");
        std::fs::create_dir_all(&evil).expect("create evil");
        assert!(!paths_intersect(&evil, root.path()));
    }

    #[test]
    fn paths_intersect_any_hits_when_any_root_intersects() {
        let root_a = TempDir::new("intersect-any-a");
        let root_b = TempDir::new("intersect-any-b");
        let roots = vec![root_a.path().to_path_buf(), root_b.path().join("inner")];
        // 祖先方向多根：目标是 root_b 的祖先。
        assert!(paths_intersect_any(root_b.path(), &roots));
        // 与两根都不相交的路径不命中。
        let outside = TempDir::new("intersect-any-outside");
        assert!(!paths_intersect_any(outside.path(), &roots));
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

    /// 最小合法注册记录（persist_new_record 测试用）。
    fn minimal_record(id: &str) -> RegistryRecord {
        RegistryRecord {
            schema: 1,
            id: id.to_string(),
            name: "tiny".to_string(),
            kind: "other".to_string(),
            base_url: None,
            model: None,
            weights_path: None,
            installed_at: None,
            note: None,
        }
    }

    #[test]
    fn persist_new_record_retries_with_fresh_id_and_spares_the_rival_record() {
        // 撞车 id 的重试语义：占位文件是双实例并发登记里先落盘的对手记录。
        // 发布永不覆盖既有文件，撞车必须换新 id 成功，对手记录逐字节幸存
        // （顺序用例只钉语义；新旧实现的判别由下方并发压力测试承担）。
        let dir = TempDir::new("register-collision");
        let taken = dir.path().join("m-deadbeef.json");
        std::fs::write(&taken, "{\"id\": \"m-deadbeef\", \"name\": \"rival\"}\n")
            .expect("seed rival record");
        let mut first_call = true;
        let id = persist_new_record(
            dir.path(),
            || {
                if first_call {
                    first_call = false;
                    "m-deadbeef".to_string()
                } else {
                    "m-cafebabe".to_string()
                }
            },
            minimal_record,
        )
        .expect("must succeed with the retry id");
        assert_eq!(id, "m-cafebabe");
        assert_eq!(
            std::fs::read_to_string(&taken).expect("rival record must survive"),
            "{\"id\": \"m-deadbeef\", \"name\": \"rival\"}\n"
        );
        let written =
            std::fs::read_to_string(dir.path().join("m-cafebabe.json")).expect("new record");
        assert!(written.contains("\"m-cafebabe"));
        assert!(written.ends_with('\n'), "尾随换行与原落盘格式一致");
    }

    #[test]
    fn persist_new_record_fails_closed_when_every_generated_id_is_taken() {
        let dir = TempDir::new("register-exhaust");
        let taken = dir.path().join("m-00000000.json");
        std::fs::write(&taken, "keep\n").expect("seed record");
        let error = persist_new_record(dir.path(), || "m-00000000".to_string(), minimal_record)
            .expect_err("exhausted ids must surface the conflict");
        assert!(error.contains("冲突"), "unexpected error: {error}");
        assert_eq!(
            std::fs::read_to_string(&taken).expect("existing record must be intact"),
            "keep\n"
        );
    }

    #[test]
    fn persist_new_record_never_clobbers_under_concurrent_id_pressure() {
        // 并发压力下的不变量：发布不覆盖。id 生成器只在小空间里抽（碰撞
        // 频繁），8 线程并发登记——每一次成功都必须独占一个 id：任何"检查
        // 后覆盖写"的回归都会让输家盖掉赢家的记录，这里以重复 id /
        // 内容与文件名不符的形式暴露；不覆盖式发布则全部成功且互不相同。
        let dir = TempDir::new("register-race");
        let dir_path = dir.path().to_path_buf();
        let handles: Vec<_> = (0..8)
            .map(|_| {
                let dir_path = dir_path.clone();
                std::thread::spawn(move || {
                    let mut registered = Vec::new();
                    for _ in 0..4 {
                        let id = persist_new_record(
                            &dir_path,
                            || format!("m-{:x}", uuid::Uuid::new_v4().as_u128() % 64),
                            minimal_record,
                        )
                        .expect("registration must succeed within retry budget");
                        registered.push(id);
                    }
                    registered
                })
            })
            .collect();
        let mut all_ids = Vec::new();
        for handle in handles {
            all_ids.extend(handle.join().expect("worker must not panic"));
        }
        let total = all_ids.len();
        all_ids.sort();
        all_ids.dedup();
        assert_eq!(
            all_ids.len(),
            total,
            "并发登记不得产生重复 id（覆盖即丢失）"
        );
        let mut files = 0;
        for entry in std::fs::read_dir(&dir_path).expect("read registry dir") {
            let entry = entry.expect("dir entry");
            let file_id = entry
                .file_name()
                .to_string_lossy()
                .trim_end_matches(".json")
                .to_string();
            let content = std::fs::read_to_string(entry.path()).expect("read record");
            assert!(
                content.contains(&format!("\"{file_id}")),
                "记录内容 id 必须与文件名一致：{file_id}"
            );
            files += 1;
        }
        assert_eq!(files, total, "落盘文件数必须等于成功登记数");
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

    #[test]
    fn weights_dir_presence_keeps_io_faults_distinct_from_not_found() {
        // 真 NotFound：幂等收尾臂（上次删除成功、登记残留的对照面）。
        let dir = TempDir::new("presence-missing");
        assert!(matches!(
            weights_dir_presence(&dir.path().join("gone")),
            Ok(false)
        ));
        // 存在：继续删除臂。
        assert!(matches!(weights_dir_presence(dir.path()), Ok(true)));

        // 权限故障：父目录失去搜索权限时 stat 报 PermissionDenied——旧的
        // Path::exists() 把它折叠成 false（仍存在的目录按"已删除"收敛并删
        // 掉登记），新分类必须原样上抛让移除失败、登记保留（round-29
        // review P2）。
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let parent = TempDir::new("presence-denied");
            let child = parent.path().join("weights");
            std::fs::create_dir_all(&child).expect("create weights dir");
            std::fs::set_permissions(parent.path(), std::fs::Permissions::from_mode(0o000))
                .expect("chmod parent");
            let classification = weights_dir_presence(&child);
            let folded_exists = Path::new(&child).exists();
            std::fs::set_permissions(parent.path(), std::fs::Permissions::from_mode(0o755))
                .expect("restore parent permissions");
            if folded_exists {
                // root 越过权限位（容器内以 root 跑测试时 000 模式仍可 stat），
                // 该环境构造不出 PermissionDenied：退而断言分类至少没有折叠
                // 成 NotFound。
                assert!(
                    matches!(classification, Ok(true)),
                    "root bypasses mode bits; classification must not fold to NotFound"
                );
            } else {
                match classification {
                    Err(error) => {
                        assert_eq!(error.kind(), std::io::ErrorKind::PermissionDenied);
                    }
                    Ok(exists) => {
                        panic!("permission fault must not classify as exists={exists}")
                    }
                }
            }
        }
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

    // round-22 评审 P3：控制字符校验此前只覆盖 name——换行的 model 标识会流进
    // 身份查重告警与 UI 行展示。校验发生在商店查询 spawn 之前，无需临时 HOME。
    // base_url 臂不再单测：trim + WHATWG 解析会先剥掉 \t\n\r，同一拒绝臂保留作
    // 纵深（其余控制字符大多被 validate_http_url 先行拒绝）。
    #[tokio::test]
    async fn register_rejects_control_chars_in_optional_fields() {
        let base = "http://127.0.0.1:8080/v1".to_string();
        let error = register_external_model(
            "ctrl-test".to_string(),
            "openai_endpoint".to_string(),
            Some(base.clone()),
            Some("qwen3\n:8b".to_string()),
            None,
        )
        .await
        .expect_err("model control chars must be rejected");
        assert!(error.contains("控制字符"), "unexpected error: {error}");
        let error = register_external_model(
            "ctrl-test".to_string(),
            "llamacpp".to_string(),
            Some(base),
            None,
            // 首尾控制字符会被 normalize_optional 的 trim 先剥掉：用内嵌换行
            // 直打控制字符臂本身。
            Some("/home/u/we\nights/xx".to_string()),
        )
        .await
        .expect_err("weights_path control chars must be rejected");
        assert!(error.contains("控制字符"), "unexpected error: {error}");
    }

    #[tokio::test]
    async fn register_rejects_ollama_records_with_weights_path() {
        // Ollama 权重由守护进程统一管理（~/.ollama 不在任何保护根内，blobs 目录
        // 深度恰过守卫链）：登记带 weights_path 的 ollama 记录会造出能删光全部
        // 已下载 blob 的记录（第七轮评审 P2）。校验发生在商店边界查询之前，
        // 本测试不需要临时 HOME。夹具必须从 temp_dir 派生**本平台**绝对路径：
        // `/home/u/...` 在 Windows 上不带盘符，登记会先撞"权重路径必须是绝对
        // 路径"臂（:513），到不了本测试要钉的 Ollama weights 拒绝臂（:526），
        // 断言必红（三十二轮评审 P2）。相对路径的反控由
        // register_rejects_relative_weights_path_before_touching_the_store 承担。
        let fake_blobs = std::env::temp_dir()
            .join("pinvou3-ollama-fixture")
            .join("models")
            .join("blobs");
        let error = register_external_model(
            "cleanup".to_string(),
            "ollama".to_string(),
            None,
            None,
            Some(fake_blobs.to_string_lossy().into_owned()),
        )
        .await
        .expect_err("ollama record with weights_path must be rejected");
        assert!(
            error.contains("weights_path 必须留空"),
            "unexpected error: {error}"
        );
    }

    #[tokio::test]
    async fn register_rejects_relative_weights_path_before_touching_the_store() {
        // 与移除守卫第一臂同一把尺子：相对路径登记成功后勾"删除权重"必整单失败，
        // 且探测/边界按进程 CWD 解析（二十一轮评审 P2）。校验在商店边界之前，
        // 不需要临时 HOME。
        let error = register_external_model(
            "rel".to_string(),
            "other".to_string(),
            None,
            None,
            Some("relative/dir".to_string()),
        )
        .await
        .expect_err("relative weights_path must be rejected");
        assert!(error.contains("绝对路径"), "unexpected error: {error}");
        assert!(
            error.contains("relative/dir"),
            "echo must name the path: {error}"
        );
    }

    #[test]
    fn parse_ollama_tag_names_caps_entries_and_skips_nameless() {
        let many: Vec<String> = (0..1300).map(|index| format!("m{index}")).collect();
        let payload = serde_json::json!({ "models": many.iter().map(|n| serde_json::json!({"name": n})).collect::<Vec<_>>() });
        let names = parse_ollama_tag_names(&payload);
        assert_eq!(names.len(), 1024, "entry cap must truncate the parsed list");
        assert_eq!(names[0], "m0");
        // 缺 name 的条目跳过；models 缺失/非数组 → 空列表。
        let mixed =
            serde_json::json!({ "models": [{"name": "a"}, {}, {"other": 1}, {"name": "b"}] });
        assert_eq!(
            parse_ollama_tag_names(&mixed),
            vec!["a".to_string(), "b".to_string()]
        );
        assert!(parse_ollama_tag_names(&serde_json::json!({})).is_empty());
    }

    #[test]
    fn path_has_ollama_layout_component_matches_daemon_layouts() {
        // 布局签名按原始路径分量判定（第九轮评审 P2；第二十轮评审 P2 扩展
        // manifests）：默认根与 OLLAMA_MODELS 迁移根下的 blobs、manifests 都
        // 命中，分量不在末尾也命中。
        assert!(path_has_ollama_layout_component(
            "/home/u/.ollama/models/blobs"
        ));
        assert!(path_has_ollama_layout_component("/mnt/ollama-models/blobs"));
        assert!(path_has_ollama_layout_component("/data/blobs/sha256-abc"));
        assert!(path_has_ollama_layout_component(
            "/home/u/.ollama/models/manifests"
        ));
        assert!(path_has_ollama_layout_component(
            "/home/u/ollama/models/manifests/sha256-abc"
        ));
        // 普通权重目录与 "blobs"/"manifests" 前缀的名字不误伤。
        assert!(!path_has_ollama_layout_component(
            "/home/u/ai/weights/qwen3"
        ));
        assert!(!path_has_ollama_layout_component(
            "/home/u/blobstore/weights"
        ));
        assert!(!path_has_ollama_layout_component(
            "/home/u/manifests-docs/notes"
        ));
        assert!(!path_has_ollama_layout_component("relative/weights"));
    }

    #[test]
    fn weights_deletion_guard_rejects_blob_layout_paths_without_needing_the_root() {
        // blobs 分量签名臂（第九轮评审 P2）：ollama 根臂依赖环境推导，systemd
        // 迁移目录对应用失明；签名臂不依赖根——深度臂本会放行的 blobs 目录
        // 硬拒，同深度、无 blobs 分量的目录不受牵连（对照）。
        let home = TempDir::new("guard-blobs");
        let migrated = home.path().join("mnt").join("ollama").join("blobs");
        std::fs::create_dir_all(&migrated).expect("create migrated blobs");
        let error = weights_deletion_guard(home.path(), None, &[], &migrated).unwrap_err();
        assert!(error.contains("blob"), "unexpected error: {error}");
        // 同深度、无 blobs 分量的目录照常放行。
        let normal = home.path().join("mnt").join("ollama").join("models");
        std::fs::create_dir_all(&normal).expect("create normal");
        assert!(weights_deletion_guard(home.path(), None, &[], &normal).is_ok());
    }

    #[test]
    fn weights_deletion_guard_rejects_manifests_layout_without_needing_the_root() {
        // manifests 分量签名臂（第二十轮评审 P2）：systemd `OLLAMA_MODELS` 迁移根
        // 对根包含臂失明时，手写/AI 记录指向 manifests 子树（家目录下恰 3 层、无
        // blobs 分量）此前全臂放行，整删会清掉 daemon 的全部模型清单——签名臂
        // 扩展后硬拒，同深度普通目录不受牵连（对照）。
        let home = TempDir::new("guard-manifests");
        let manifests = home.path().join("mnt").join("ollama").join("manifests");
        std::fs::create_dir_all(&manifests).expect("create manifests");
        let error = weights_deletion_guard(home.path(), None, &[], &manifests).unwrap_err();
        assert!(error.contains("manifests"), "unexpected error: {error}");
        let normal = home.path().join("mnt").join("ollama").join("models");
        std::fs::create_dir_all(&normal).expect("create normal");
        assert!(weights_deletion_guard(home.path(), None, &[], &normal).is_ok());
    }

    #[test]
    fn weights_deletion_guard_protects_the_default_root_even_when_env_root_is_unresolvable() {
        // 候选根并集（第二十轮评审 P2）：env 迁移根写错/指向未建目录时，daemon
        // 完全可以继续用默认根——默认根不得失守。旧实现（单根、`if let Ok` 跳过）
        // 在该场景放行整删默认根下的全部权重。
        let home = TempDir::new("guard-roots-merge");
        let default_root = home.path().join(".ollama").join("models");
        let qwen = default_root.join("qwen3");
        std::fs::create_dir_all(&qwen).expect("create default root weights");
        let unresolvable_env_root = home.path().join("env-migration").join("models");
        let roots = vec![unresolvable_env_root, default_root.clone()];
        let error = weights_deletion_guard(home.path(), None, &roots, &qwen).unwrap_err();
        assert!(error.contains("Ollama"), "unexpected error: {error}");
        // 候选根全不可解析（两处都不存在）时跳过该臂：目标存在而根不存在，
        // "目标在根之下"不可能成立（与既有单根口径一致）。
        let weights = home.path().join("ai").join("weights").join("qwen3");
        std::fs::create_dir_all(&weights).expect("create weights");
        assert!(weights_deletion_guard(home.path(), None, &roots, &weights).is_ok());
    }

    #[test]
    fn guard_errors_echo_long_paths_truncated() {
        // 回显限长（第二十轮评审 P3）：注册表是手写/AI 共用的文件，weights_path
        // 只受 64KB 记录上限约束——超长路径不得逐字搬进错误与日志。
        let home = TempDir::new("guard-echo-cap");
        let long_relative = format!("a/{}", "b".repeat(300));
        let error =
            weights_deletion_guard(home.path(), None, &[], Path::new(&long_relative)).unwrap_err();
        assert!(
            !error.contains(&"b".repeat(129)),
            "echo must be capped, got: {error}"
        );
        assert!(
            error.contains(&"b".repeat(64)),
            "echo must stay diagnosable: {error}"
        );
    }

    #[test]
    fn interpret_ollama_delete_status_is_idempotent_on_404() {
        // 404 幂等臂（十九轮评审 P3 修复、第二十轮评审 P1 补测）：删除实现回退成
        // 404→Err 时本测试必红。连接层失败走 human_probe_error，不会落到这里。
        assert!(interpret_ollama_delete_status(200).is_ok());
        assert!(interpret_ollama_delete_status(201).is_ok());
        assert!(interpret_ollama_delete_status(204).is_ok());
        assert!(interpret_ollama_delete_status(404).is_ok());
        // 终态 3xx（跟随不了的 304 / 无 Location 的 301）不是"已删除"：此前
        // `< 400` 把它们当成功会产生假完成提示（二十一轮评审 P2）。
        let error = interpret_ollama_delete_status(304).unwrap_err();
        assert!(error.contains("HTTP 304"), "unexpected error: {error}");
        assert!(interpret_ollama_delete_status(301).is_err());
        assert!(interpret_ollama_delete_status(100).is_err());
        let error = interpret_ollama_delete_status(400).unwrap_err();
        assert!(error.contains("HTTP 400"), "unexpected error: {error}");
        assert!(interpret_ollama_delete_status(500).is_err());
        assert!(interpret_ollama_delete_status(503).is_err());
    }

    #[test]
    fn ensure_recheck_unchanged_rejects_swapped_targets() {
        // 删除前 canonical 复查臂（五轮补的防线、第二十轮评审 P3 抽纯函数补测）：
        // 守卫后目录被换走必须拒绝。抽成纯函数前该臂零测试触达（摘除全绿）。
        let target = Path::new("/tmp/recheck-a");
        assert!(ensure_recheck_unchanged(target, target).is_ok());
        let error = ensure_recheck_unchanged(target, Path::new("/tmp/recheck-b")).unwrap_err();
        assert!(error.contains("守卫后被改动"), "unexpected error: {error}");
    }

    #[test]
    fn ensure_recheck_outside_store_roots_rejects_targets_moved_into_store() {
        // 删除时刻商店根复查臂（九轮补的防线、第二十轮评审 P3 抽纯函数补测）：
        // 守卫后目录被移进任一商店权重根必须拒绝。祖先方向同样拒绝（二十六轮
        // 评审 P1）：目标是商店权重根的祖先时，`remove_dir_all` 会把根连根清掉。
        let root = PathBuf::from("/home/u/.pinvou3/model-store");
        let inside = root.join("weights").join("qwen3");
        assert!(ensure_recheck_outside_store_roots(&inside, std::slice::from_ref(&root)).is_err());
        let ancestor = PathBuf::from("/home/u/ai/weights");
        let store_custom = ancestor.join("store-managed");
        let error =
            ensure_recheck_outside_store_roots(&ancestor, std::slice::from_ref(&store_custom))
                .unwrap_err();
        assert!(error.contains("相交"), "unexpected error: {error}");
        let outside = PathBuf::from("/home/u/ai/weights/qwen3");
        assert!(ensure_recheck_outside_store_roots(&outside, &[root]).is_ok());
        // 兄弟目录（商店根的兄弟）放行。
        let sibling_of_store = ancestor.join("other-weights");
        assert!(ensure_recheck_outside_store_roots(&sibling_of_store, &[store_custom]).is_ok());
    }

    #[test]
    fn endpoint_url_is_loopback_matches_the_model_store_ruler() {
        // 与 model_store lifecycle 的 endpoint_url_is_loopback 同一把回环尺子
        // （注释互指的双实现）：device 侧副本此前零测试（第二十轮评审 P1），
        // 登记端"拒绝指向远程 daemon 的 ollama 记录"的臂回退成放行时本测试必红。
        // 用例矩阵照抄 lifecycle::only_loopback_endpoints_are_accepted。
        for good in [
            "http://127.0.0.1:30001/v1",
            "http://127.0.0.1:30001/v1/chat/completions",
            "http://127.0.0.2:8000/v1",
            "http://127.255.255.254:8000/v1",
            "http://localhost:11434/v1",
            "http://LocalHost:11434/v1",
            "http://localhost.:11434/v1",
            "http://[::1]:8000/v1",
            "http://[0:0:0:0:0:0:0:1]:8000/v1",
            "https://127.0.0.1:8443/v1",
        ] {
            assert!(endpoint_url_is_loopback(good), "{good} 是本机地址，该放行");
        }
        for bad in [
            "http://203.0.113.7/v1",
            "https://evil.example.com/v1",
            "http://192.168.1.10:8000/v1",
            "http://10.0.0.5:8000/v1",
            "http://localhost.evil.example.com/v1",
            "http://127.0.0.1.evil.example.com/v1",
            "http://0.0.0.0:8000/v1",
            "http://[::ffff:127.0.0.1]:8000/v1",
            "http://localhost../v1",
            "not a url",
            "",
        ] {
            assert!(
                !endpoint_url_is_loopback(bad),
                "{bad} 不指向本机，落库就等于把用户的对话导去别处"
            );
        }
    }

    #[test]
    fn weights_deletion_guard_fails_closed_when_the_protected_root_cannot_be_resolved() {
        // 保护根 canonicalize 失败臂（十九轮评审：此前零测试触达，退回
        // `if let Ok` 静默放行整棵应用树时现有套件照绿）。根不存在 → 必须
        // 整单拒绝并给出可执行出路，而不是跳过保护。
        let home = TempDir::new("guard-protect-unresolvable");
        let weights = home.path().join("deep").join("a").join("b");
        std::fs::create_dir_all(&weights).expect("create weights dir");
        let missing_root = home.path().join("no-such-protected-root");
        let error =
            weights_deletion_guard(home.path(), Some(&missing_root), &[], &weights).unwrap_err();
        assert!(
            error.contains("无法解析应用数据目录"),
            "unexpected error: {error}"
        );
    }

    // canonical blobs 复查的两枚符号链接用例（十九轮评审 P1：canonical 目标
    // 复查签名/登记端 canonical 助手）按架构约定落平台适配层——features 层
    // 不能写 cfg，见 platform/linux.rs 的 tests 模块。

    #[test]
    fn weights_deletion_guard_refuses_degenerate_home_root() {
        // canonicalized home 为 `/` 时 containment 臂对一切绝对路径恒真（第九
        // 轮评审 P3）：守卫必须直接拒绝，而不是让层级/包含臂全部落空。
        let deep = TempDir::new("guard-root-home");
        let weights = deep.path().join("a").join("b").join("c");
        std::fs::create_dir_all(&weights).expect("create weights dir");
        let error = weights_deletion_guard(Path::new("/"), None, &[], &weights).unwrap_err();
        assert!(error.contains("家目录"), "unexpected error: {error}");
    }

    #[tokio::test]
    async fn register_rejects_blob_layout_weights_path_for_any_kind() {
        // blob 布局签名臂（第九轮评审 P2）：kind:"other" 记录带迁移目录下的
        // blobs 路径时，环境臂对 systemd 的 OLLAMA_MODELS 失明——登记端按
        // 签名拒绝。校验发生在商店边界查询之前，本测试不需要临时 HOME。
        // 夹具从 temp_dir 派生**本平台**绝对路径（与
        // register_rejects_ollama_records_with_weights_path 同款修法，三十五轮
        // 评审 P2）：硬编码 `/mnt/...` 在 Windows 上 is_absolute()==false，会
        // 打错臂落到"必须是绝对路径"拒绝上——那句错误回显里恰好带路径原文的
        // "blobs" 字样，contains("blob") 侥幸放行，布局签名臂实际零覆盖。断言
        // 也改钉布局臂独有的文案，错臂不再能被路径回显侥幸满足。
        let fake_blobs = std::env::temp_dir()
            .join("pinvou3-blob-fixture")
            .join("models")
            .join("blobs");
        let error = register_external_model(
            "blob-layout".to_string(),
            "other".to_string(),
            None,
            None,
            Some(fake_blobs.to_string_lossy().into_owned()),
        )
        .await
        .expect_err("blob-layout weights_path must be rejected");
        assert!(
            error.contains("守护进程的模型库布局"),
            "unexpected error: {error}"
        );
    }

    // 三十五轮评审 P2 受控交错回归：登记发布临界区（容量/查重/落盘）必须被
    // REGISTRY_PUBLISH_LOCK 串行化。闸门把 A 强制停在锁内，证明 B 的同身份
    // 请求拿不到锁、放行后凭**锁内重读**拒绝——把查重挪出锁或删掉锁，本测试
    // 必红。两个用例共享全局闸门/互斥，用测试级互斥串行化，避免彼此踩闸门；
    // 闸门按目标目录匹配（见 section_test_pause），同二进制里其他登记用例
    // 偷不走闸（36 轮评审 P2）。
    static PUBLISH_RACE_TEST_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

    fn arm_publish_section_pause(key: &str) {
        let mut state = SECTION_TEST_PAUSE.lock().expect("pause lock");
        state.armed = true;
        state.armed_key = Some(key.to_string());
        state.arrived = false;
        state.release = false;
    }

    fn wait_publish_section_arrival() {
        let mut state = SECTION_TEST_PAUSE.lock().expect("pause lock");
        while !state.arrived {
            state = SECTION_TEST_PAUSE_CV.wait(state).expect("pause cv");
        }
    }

    fn release_publish_section_pause() {
        let mut state = SECTION_TEST_PAUSE.lock().expect("pause lock");
        state.release = true;
        SECTION_TEST_PAUSE_CV.notify_all();
    }

    #[test]
    fn concurrent_same_identity_registrations_are_rejected_by_the_publish_section() {
        let _serial = PUBLISH_RACE_TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let dir = TempDir::new("registry-publish-race");
        arm_publish_section_pause(&dir.path().to_string_lossy());
        let dir_a = dir.path().to_path_buf();
        let a = std::thread::spawn(move || {
            publish_registry_entry(
                &dir_a,
                "svc",
                "openai_endpoint",
                Some("http://127.0.0.1:8080/v1"),
                Some("qwen3:8b"),
                None,
            )
        });
        wait_publish_section_arrival();
        // A 在临界区内持锁：任何并发entrant此刻都拿不到锁——确定性证明串行化。
        assert!(
            REGISTRY_PUBLISH_LOCK.try_lock().is_err(),
            "the publish section must be held while a registration is inside it"
        );
        let dir_b = dir.path().to_path_buf();
        let b = std::thread::spawn(move || {
            publish_registry_entry(
                &dir_b,
                "svc",
                "openai_endpoint",
                Some("http://127.0.0.1:8080/v1"),
                Some("qwen3:8b"),
                None,
            )
        });
        release_publish_section_pause();
        let a_id = a
            .join()
            .expect("A must not panic")
            .expect("A must publish the first record");
        let error = b
            .join()
            .expect("B must not panic")
            .expect_err("the same business identity must be rejected");
        assert!(
            error.contains("已存在同类别同模型"),
            "unexpected error: {error}"
        );
        let (models, _) = read_registry(dir.path());
        assert_eq!(models.len(), 1, "no duplicate identity row may exist");
        assert_eq!(models[0].id, a_id, "the surviving row is A's record");
    }

    #[test]
    fn concurrent_different_identity_registrations_both_publish() {
        let _serial = PUBLISH_RACE_TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let dir = TempDir::new("registry-publish-parallel");
        arm_publish_section_pause(&dir.path().to_string_lossy());
        let dir_a = dir.path().to_path_buf();
        let a = std::thread::spawn(move || {
            publish_registry_entry(
                &dir_a,
                "svc",
                "openai_endpoint",
                Some("http://127.0.0.1:8080/v1"),
                Some("qwen3:8b"),
                None,
            )
        });
        wait_publish_section_arrival();
        // 不同端点 = 设计文档定义的独立服务，并发登记合法共存——锁只串行化
        // 临界区，不收窄合法配置。
        let dir_b = dir.path().to_path_buf();
        let b = std::thread::spawn(move || {
            publish_registry_entry(
                &dir_b,
                "svc",
                "openai_endpoint",
                Some("http://127.0.0.1:8081/v1"),
                Some("qwen3:8b"),
                None,
            )
        });
        release_publish_section_pause();
        a.join().expect("A must not panic").expect("A must publish");
        b.join().expect("B must not panic").expect("B must publish");
        let (models, _) = read_registry(dir.path());
        assert_eq!(models.len(), 2, "both independent services must coexist");
        let mut endpoints = models
            .iter()
            .map(|model| {
                model
                    .base_url
                    .clone()
                    .expect("registered endpoints present")
            })
            .collect::<Vec<_>>();
        endpoints.sort_unstable();
        assert_eq!(
            endpoints,
            vec![
                "http://127.0.0.1:8080/v1".to_string(),
                "http://127.0.0.1:8081/v1".to_string()
            ],
        );
    }

    #[test]
    fn ollama_name_matches_defaults_to_latest_and_keeps_explicit_tags() {
        // 裸名记录 ↔ daemon 默认 tag（两个方向都要命中，第九轮评审 P3）。
        assert!(ollama_name_matches("mymodel", "mymodel:latest"));
        assert!(ollama_name_matches("mymodel:latest", "mymodel"));
        // 大小写折叠仍在（记录里手写 "Qwen3:30B"，五轮评审 P3）。
        assert!(ollama_name_matches("MyModel", "mymodel:latest"));
        assert!(ollama_name_matches("Qwen3:30B", "qwen3:30b"));
        // 显式 tag 是身份：不同 tag 互不命中，裸名也不得冒充显式 tag——
        // 无条件去 tag 的旧口径会把 qwen3:8b 与 qwen3:30b 判成同一模型
        // （十轮评审 P2）。
        assert!(!ollama_name_matches("qwen3:8b", "qwen3:30b"));
        assert!(!ollama_name_matches("Qwen3:30B", "qwen3"));
        assert!(!ollama_name_matches("mymodel", "mymodel:8b"));
        assert!(!ollama_name_matches("mymodel:8b", "mymodel"));
        // 不同 base 名不得互相命中。
        assert!(!ollama_name_matches("mymodel", "othermodel:latest"));
        assert!(!ollama_name_matches("mymodel:latest", "mymodel2"));
    }

    #[test]
    fn daemon_rows_keep_every_explicit_tag_and_status_never_impersonates() {
        let make = |name: &str, model: &str| ExternalModel {
            id: format!("reg-{name}"),
            name: name.to_string(),
            kind: "ollama".to_string(),
            base_url: None,
            model: Some(model.to_string()),
            weights_path: None,
            status: "unreachable".to_string(),
            registered: true,
            source: "registry".to_string(),
        };
        // 注册了 qwen3:8b，daemon 同时挂着 8b 与 30b：8b 不重复成未登记行，
        // 30b 是另一个模型，必须以未登记行现身（无条件去 tag 会把它隐藏）。
        let mut models = vec![make("qwen3 8b", "qwen3:8b")];
        push_ollama_daemon_rows(
            &mut models,
            &["qwen3:8b".to_string(), "qwen3:30b".to_string()],
        );
        assert_eq!(models.len(), 2);
        assert_eq!(models[1].name, "qwen3:30b");
        assert_eq!(models[1].source, "ollama");
        assert!(!models[1].registered);

        // 只装了 30b 却登记 8b：8b 不得冒充 available（链接/删除发的是完整
        // 8b 名，daemon 会 404）；补上 8b 后恢复 available。
        assert_eq!(
            ollama_status("qwen3:8b", &["qwen3:30b".to_string()]),
            "unreachable"
        );
        assert_eq!(
            ollama_status(
                "qwen3:8b",
                &["qwen3:30b".to_string(), "qwen3:8b".to_string()]
            ),
            "available"
        );
    }

    #[test]
    fn daemon_rows_dedup_by_effective_model_not_display_name() {
        let make = |name: &str, model: Option<&str>| ExternalModel {
            id: format!("reg-{}", name.replace([':', ' '], "-")),
            name: name.to_string(),
            kind: "ollama".to_string(),
            base_url: None,
            model: model.map(str::to_string),
            weights_path: None,
            status: "unreachable".to_string(),
            registered: true,
            source: "registry".to_string(),
        };
        // 展示名 qwen3:30b、显式 model qwen3:8b（十一轮评审 P2 的负控）：
        // daemon 只挂着真实的 30b 时，去重不得拿展示名命中把它当“重复行”
        // 隐藏——状态链按 model 判 8b unreachable，去重必须同一把有效身份。
        let mut models = vec![make("qwen3:30b", Some("qwen3:8b"))];
        push_ollama_daemon_rows(&mut models, &["qwen3:30b".to_string()]);
        assert_eq!(models.len(), 2);
        assert_eq!(models[1].name, "qwen3:30b");
        assert_eq!(models[1].source, "ollama");
        assert!(!models[1].registered);

        // daemon 真挂着显式 model（8b）时仍按 model 去重，不重复成未登记行。
        let mut models = vec![make("qwen3:30b", Some("qwen3:8b"))];
        push_ollama_daemon_rows(&mut models, &["qwen3:8b".to_string()]);
        assert_eq!(models.len(), 1);

        // model 缺失才回退展示 name（登记裸名的既有口径不变）。
        let mut models = vec![make("qwen3:30b", None)];
        push_ollama_daemon_rows(&mut models, &["qwen3:30b".to_string()]);
        assert_eq!(models.len(), 1);
    }

    #[test]
    fn endpoint_models_url_inserts_models_before_query_or_fragment() {
        // 带 query/fragment 的基地址：/models 必须插在 `?`/`#` 之前，否则探测
        // 永远 404（第九轮评审 P3）。
        assert_eq!(
            endpoint_models_url("http://h:8080/v1?x=1"),
            "http://h:8080/v1/models?x=1"
        );
        assert_eq!(
            endpoint_models_url("http://h:8080/v1#frag"),
            "http://h:8080/v1/models#frag"
        );
        // 已带 /models 的 query 形态原样保留。
        assert_eq!(
            endpoint_models_url("http://h:8080/v1/models?x=1"),
            "http://h:8080/v1/models?x=1"
        );
    }

    #[test]
    fn read_registry_warns_when_existing_dir_cannot_be_read() {
        // 目录存在但 read_dir 失败：这里用"路径是普通文件"稳定制造非 NotFound
        // 错误——按空列表静默返回会让"模型全消失"，必须进 warnings（第九轮
        // 评审 P3）。NotFound（首跑无目录）仍是无警告的空列表。
        let host = TempDir::new("registry-notadir");
        let not_a_dir = host.path().join("plain.txt");
        std::fs::write(&not_a_dir, b"x").expect("write file");
        let (models, warnings) = read_registry(&not_a_dir);
        assert!(models.is_empty());
        assert!(
            warnings.iter().any(|warning| warning.contains("无法读取")),
            "unreadable dir must surface a warning, got: {warnings:?}"
        );
        // 首跑（目录不存在）零警告。
        let (models, warnings) = read_registry(&host.path().join("missing"));
        assert!(models.is_empty() && warnings.is_empty());
    }

    #[test]
    fn confirm_mismatch_echo_is_truncated() {
        // 确认名回显限长（第九轮评审 P3）：record_name 来自手写文件，超长名字
        // 不逐字进错误与日志。
        let long_name = "长".repeat(100);
        let error = removal_boundary_error(
            &long_name, "mismatch", false, false, false, false, false, false, None, None,
        )
        .unwrap_err();
        assert!(error.contains(&"长".repeat(32)));
        assert!(!error.contains(&"长".repeat(33)), "echo must be truncated");
    }

    // round-22 评审 P3：64 字节边界无钉——`<= 64` 放宽成 `<= 65` 全套照绿。
    #[test]
    fn is_safe_id_boundary_at_64_bytes() {
        assert!(is_safe_id(&"a".repeat(64)), "64 bytes must stay valid");
        assert!(!is_safe_id(&"a".repeat(65)), "65 bytes must be rejected");
        assert!(!is_safe_id(""));
    }

    // round-22 评审 P3：写入侧数量上限此前零覆盖（读侧截断有钉，写侧拒绝臂
    // 没有）——删掉比较或放行超额登记全套照绿，第 257 条会成为读取侧不可见的
    // 隐形记录。恰好 256 拒、255 放，钉死等值边界。
    #[test]
    fn ensure_registry_has_capacity_rejects_at_the_cap() {
        let dir = TempDir::new("capacity");
        for index in 0..255 {
            std::fs::write(dir.path().join(format!("m-fill{index:03}.json")), "{}")
                .expect("write filler record");
        }
        ensure_registry_has_capacity(dir.path()).expect("255 records must leave room");
        std::fs::write(dir.path().join("m-full.json"), "{}").expect("write 256th record");
        let error = ensure_registry_has_capacity(dir.path()).unwrap_err();
        assert!(error.contains("注册表已满"), "unexpected error: {error}");
    }

    // round-22 评审 P3：删除接线（方法/路径/请求体/预算）此前零覆盖。
    #[test]
    fn ollama_delete_request_pins_method_path_body_and_budget() {
        let client = reqwest::Client::new();
        let request = ollama_delete_request(&client, "http://127.0.0.1:11434", "qwen3:8b")
            .build()
            .expect("build request");
        assert_eq!(request.method(), reqwest::Method::DELETE);
        assert_eq!(request.url().as_str(), "http://127.0.0.1:11434/api/delete");
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .expect("json body");
        let parsed: serde_json::Value = serde_json::from_slice(body).expect("parse body");
        assert_eq!(parsed, serde_json::json!({ "name": "qwen3:8b" }));
        assert_eq!(
            request.timeout(),
            Some(&OLLAMA_DELETE_TIMEOUT),
            "delete must keep its dedicated 30s budget"
        );
    }

    // 三十二轮评审 P2 正控臂：删除按**解析出的**基地址上线——请求只能打到传入
    // 的这一个端点（默认 11434 上结构性地零 DELETE：base 是唯一入参，解析失败
    // 在 `ollama_delete` 的 `?` 处短路、连请求都不构造；读取失败的 fail-closed
    // 反例由 model_store 的 ollama_base_url_checked 测试钉住）。回归钉法：把
    // 发送臂改回宽松回退、或旁路 base 改投别处，本测试即红。
    #[tokio::test]
    async fn ollama_delete_against_hits_only_the_resolved_base() {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        use tokio::net::TcpListener;

        let listener = TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind ephemeral listener");
        let addr = listener.local_addr().expect("listener addr");
        // 刻意用不存在的模型名：万一回归把请求投到开发机上真实运行的默认
        // daemon，也只得到 404（幂等成功），不会真删任何模型。
        let model = "pinvou3-delete-binding-probe-nonexistent";

        let server = tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.expect("delete must connect");
            let mut buf = [0u8; 1024];
            let read = socket.read(&mut buf).await.expect("read delete request");
            let request = String::from_utf8_lossy(&buf[..read]).to_string();
            socket
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
                .await
                .ok();
            request
        });

        let base = format!("http://{addr}");
        let outcome = ollama_delete_against(&base, model).await;
        let request = server.await.expect("server task");
        assert!(
            request.starts_with("DELETE /api/delete"),
            "request line must target the resolved base: {request}"
        );
        assert!(
            request.contains(model),
            "delete body must carry the effective model name: {request}"
        );
        assert!(outcome.is_ok(), "200 must resolve as deleted: {outcome:?}");
    }
}
