//! 卡片发现：扫描卡片目录，解析 common.sh 关键常量与 .deploy-env。
//!
//! 原生驭光卡指纹：common.sh 含 ENGINE_BIN_SHA256；docker 容器卡指纹：含
//! IMAGE + CONTAINER_NAME（无 ENGINE_BIN_SHA256）。解析方式是逐行文本提取，
//! 不执行 shell。

use std::path::{Path, PathBuf};

/// 引擎形态：原生（驭光 systemd 托管）/ 容器（docker 托管）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EngineKind {
    Native,
    Container,
}

impl EngineKind {
    pub fn as_str(&self) -> &'static str {
        match self {
            EngineKind::Native => "native",
            EngineKind::Container => "container",
        }
    }
}

#[derive(Debug, Clone)]
pub struct Card {
    /// 目录名，即卡片 id（unit 实例名、API 路径段都用它）。
    pub name: String,
    pub model_id: String,
    pub total_bytes: u64,
    pub engine_sha8: String,
    pub engine_kind: EngineKind,
    /// 容器卡：镜像名（common.sh 的 IMAGE）。
    pub image: Option<String>,
    /// 容器卡：容器名（common.sh 的 CONTAINER_NAME，容器→卡映射的第一锚点）。
    pub container_name: Option<String>,
    /// 原生卡的部署信息（卡根 .deploy-env）。
    pub deploy: Option<DeployEnv>,
    /// 容器卡的部署信息（vendor/.deploy-env，回落卡根 .deploy-env）。
    pub docker_deploy: Option<DockerDeploy>,
    /// 容器卡权重清单（common.sh 的 LMM_BIG_FILES / LMM_SMALL_FILES）。
    pub big_files: Vec<(String, u64)>,
    pub small_files: Vec<String>,
    /// compose 型卡：权重文件名清单（download_model.sh 内嵌的 HF LFS sha256
    /// 清单里提取的文件名列）与清单自身哈希（LMM_WEIGHTS_MANIFEST_SHA）。
    pub weight_files: Vec<String>,
    pub weights_manifest_sha: Option<String>,
    /// enable.sh 提取的字面量 serve 旗标（无 enable.sh 的卡为 None）。
    pub serve_flags: Option<String>,
    /// Optional native profile reservation. Invalid declarations remain visible
    /// as infinity and fail admission; never silently fall back to a smaller estimate.
    pub native_reserved_gib: Option<f64>,
}

/// 容器卡的部署信息（vendor/deploy.sh 写出的 KEY="VALUE" 行集）。
#[derive(Debug, Clone)]
pub struct DockerDeploy {
    pub image: String,
    pub container_name: String,
    /// 宿主端口（桥接卡 = -p PORT:容器内端口；compose host 网络卡 = .env PORT）。
    pub port: u16,
    pub model_dir: String,
    /// vendor/start.sh（或 .env）的 MEM_FRACTION_STATIC 默认值（静态预留预算口径）。
    pub mem_fraction_static: Option<f64>,
    /// compose 型卡（mtp/vllm 一代）：vendor 目录（install_engine.sh 的
    /// VENDOR_DIR 展开 $HOME）；sglang0519 一代为 None。
    pub vendor_dir: Option<PathBuf>,
    /// compose 型卡：HOST_BIND（network_mode: host，服务直接绑宿主该地址；
    /// 健康探测与网关代理目标是 127.0.0.1:PORT，与桥接映射容器不同）。
    pub host_bind: Option<String>,
    /// Explicit compose deployment values that docker start cannot update.
    pub runtime_flags: std::collections::BTreeMap<String, String>,
}

#[derive(Debug, Clone)]
pub struct DeployEnv {
    pub port: u16,
    pub host: String,
    pub model_dir: String,
    pub engine_dir: String,
    pub artifact: String,
    pub model_id: String,
    pub max_concurrency: u32,
}

impl Card {
    /// 权重文件完整路径（优先 .deploy-env 的 MODEL_DIR，与卡片脚本口径一致）。
    pub fn weights_path(&self) -> Option<PathBuf> {
        self.deploy
            .as_ref()
            .map(|d| PathBuf::from(&d.model_dir).join(&d.artifact))
    }

    pub fn engine_dir(&self) -> Option<PathBuf> {
        self.deploy.as_ref().map(|d| PathBuf::from(&d.engine_dir))
    }

    /// enable.sh 前置：引擎二进制存在且可执行。
    pub fn engine_bin(&self) -> Option<PathBuf> {
        self.engine_dir().map(|d| d.join("tachyon-serve"))
    }

    /// enable.sh 前置：权重文件存在且字节数精确匹配。
    pub fn weights_complete(&self) -> bool {
        self.weights_path()
            .and_then(|p| std::fs::metadata(&p).ok())
            .map(|m| m.len() == self.total_bytes)
            .unwrap_or(false)
    }

    /// 容器卡权重完整性（按卡的代际分口径；未部署/无清单的卡给 None 而不是误判 false）。
    pub fn docker_weights_complete(&self) -> Option<bool> {
        let deploy = self.docker_deploy.as_ref()?;
        if !self.big_files.is_empty() {
            // sglang0519 一代（口径对齐 common.sh lmm_model_complete：
            // 大文件核字节数、小文件查存在）
            let dir = Path::new(&deploy.model_dir);
            let big_ok = self.big_files.iter().all(|(name, want)| {
                std::fs::metadata(dir.join(name))
                    .map(|m| m.len() == *want)
                    .unwrap_or(false)
            });
            let small_ok = self.small_files.iter().all(|f| dir.join(f).is_file());
            return Some(big_ok && small_ok);
        }
        if deploy.vendor_dir.is_some() {
            // compose 一代（HF 快照目录）：见 compose_weights_complete 的口径注释
            return compose_weights_complete(self, &deploy.model_dir);
        }
        None
    }
}

/// 从 common.sh 文本提取 `KEY="value"` 或 `KEY="${VAR:-default}"` 的值。
pub fn native_environment(
    card_dir: &Path,
) -> Result<std::collections::BTreeMap<String, String>, String> {
    let mut result = std::collections::BTreeMap::new();
    for filename in ["common.sh", "enable.sh"] {
        let text = match std::fs::read_to_string(card_dir.join(filename)) {
            Ok(v) if v.len() <= 256 * 1024 => v,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => continue,
            _ => return Err("native_runtime_profile_unsupported: 卡片运行档不可读或过大".into()),
        };
        for line in text
            .lines()
            .filter_map(|l| l.trim().strip_prefix("export "))
        {
            for token in line.split_whitespace() {
                if token.starts_with('#') {
                    break;
                }
                let Some((key, value)) = token.split_once('=') else {
                    continue;
                };
                if !["TACHYON_PROJ_W4A4", "TACHYON_W4A4_SCHED"].contains(&key) {
                    continue;
                }
                let value = value.trim_matches(['\'', '"']);
                let valid = match key {
                    "TACHYON_PROJ_W4A4" => ["0", "1"].contains(&value),
                    _ => ["0", "1", "2", "3"].contains(&value),
                };
                if !valid {
                    return Err(format!(
                        "native_runtime_profile_unsupported: {key} 必须使用已支持的字面量档位"
                    ));
                }
                result.insert(key.to_owned(), value.to_owned());
            }
        }
    }
    Ok(result)
}

/// Strip exactly one data delimiter pair; paths remain literal, without shell decoding.
fn literal_assignment_value(value: &str) -> &str {
    let value = value.trim();
    value
        .strip_prefix('"')
        .and_then(|value| value.strip_suffix('"'))
        .unwrap_or(value)
}

/// Extract simple common/deployment assignments without executing shell.
fn extract_sh_value(text: &str, key: &str) -> Option<String> {
    for line in text.lines() {
        let line = line.trim();
        // 形如 KEY="..." 或 KEY="${VAR:-...}"（ TOTAL_BYTES 的形式）
        let Some(rest) = line.strip_prefix(key) else {
            continue;
        };
        let Some(rest) = rest.strip_prefix('=') else {
            continue;
        };
        let rest = literal_assignment_value(rest);
        // ${LMM_MODEL_SIZE_BYTES:-22892664832} → 22892664832
        if let Some(inner) = rest.strip_prefix("${").and_then(|r| r.strip_suffix('}')) {
            match inner.split_once(":-") {
                Some((_, default)) => return Some(default.to_string()),
                None => return Some(inner.to_string()),
            }
        }
        return Some(rest.to_string());
    }
    None
}

/// 解析 .deploy-env：deploy.sh 写出的 `KEY="VALUE"` 行集。
pub fn parse_deploy_env(text: &str) -> Option<DeployEnv> {
    let map = parse_kv_lines(text);
    let get = |k: &str| map.get(k).cloned();
    Some(DeployEnv {
        port: get("PORT")?.parse().ok()?,
        host: get("HOST")?,
        model_dir: get("MODEL_DIR")?,
        engine_dir: get("ENGINE_DIR")?,
        artifact: get("ARTIFACT")?,
        model_id: get("MODEL_ID")?,
        max_concurrency: get("MAX_CONCURRENCY")
            .and_then(|v| v.parse().ok())
            .unwrap_or(1),
    })
}

/// `KEY="VALUE"` 行集 → 映射（.deploy-env / vendor/.deploy-env 共用）。
fn parse_kv_lines(text: &str) -> std::collections::BTreeMap<String, String> {
    let mut map = std::collections::BTreeMap::new();
    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        if let Some((k, v)) = line.split_once('=') {
            map.insert(
                k.trim().to_string(),
                literal_assignment_value(v).to_string(),
            );
        }
    }
    map
}

/// 解析 bash 数组（LMM_BIG_FILES / LMM_SMALL_FILES 的多行括号形式，单行形式也兼容）。
fn extract_sh_array(text: &str, key: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut in_block = false;
    for line in text.lines() {
        let t = line.trim();
        if !in_block {
            if t.starts_with(key) && t.contains("=(") {
                in_block = true;
                // 单行形式：KEY=("a" "b")
                if let Some((_, rest)) = t.split_once('(') {
                    if let Some(inner) = rest.strip_suffix(')') {
                        out.extend(split_array_words(inner));
                        return out;
                    }
                }
            }
            continue;
        }
        if t.starts_with(')') {
            break;
        }
        out.extend(split_array_words(t));
    }
    out
}

fn split_array_words(s: &str) -> Vec<String> {
    s.split_whitespace()
        .map(|w| w.trim_matches('"').to_string())
        .filter(|w| !w.is_empty() && !w.starts_with('#'))
        .collect()
}

/// 容器卡的部署信息：vendor/.deploy-env（sglang 卡范式）回落卡根 .deploy-env。
fn parse_docker_deploy(dir: &Path) -> Option<DockerDeploy> {
    let text = std::fs::read_to_string(dir.join("vendor/.deploy-env"))
        .ok()
        .or_else(|| std::fs::read_to_string(dir.join(".deploy-env")).ok())?;
    let map = parse_kv_lines(&text);
    let get = |k: &str| map.get(k).cloned();
    Some(DockerDeploy {
        image: get("IMAGE")?,
        container_name: get("CONTAINER_NAME")?,
        port: get("PORT")?.parse().ok()?,
        model_dir: get("MODEL_DIR")?,
        mem_fraction_static: None, // 由 scan_cards 从 vendor/start.sh 补
        vendor_dir: None,
        host_bind: None,
        runtime_flags: Default::default(),
    })
}

// ---------------------------------------------------------------------------
// compose 型容器卡（mtp/vllm 一代，无 common.sh）
// ---------------------------------------------------------------------------

/// 展开值里的 $HOME/${HOME}（compose 卡的 VENDOR_DIR / .env.example 用）。
/// 还有其它 $ 变量就原样留着（后面读文件自然失败，按「未部署」处理，不猜）。
fn expand_home(v: &str) -> String {
    let home = std::env::var("HOME").unwrap_or_default();
    // $TARGET_HOME（compose 一代卡：${PKEXEC_UID:-$(id -u)} 对应用户的家目录；
    // cmsd 永不 pkexec/root，TARGET_UID 恒为当前用户，等价于 $HOME）
    v.replace("${TARGET_HOME}", &home)
        .replace("$TARGET_HOME", &home)
        .replace("${HOME}", &home)
        .replace("$HOME", &home)
}

/// KEY=VALUE 行集（首个出现者胜）：安装前兜底解析 install_engine.sh 内嵌的
/// .env.example heredoc——两个 profile 的同名字段（如 MEM_FRACTION_STATIC）
/// 取先出现的 benchmark 档，与卡根 deploy.sh 的默认 profile 一致。
fn first_wins_kv_lines(text: &str) -> std::collections::BTreeMap<String, String> {
    let mut map = std::collections::BTreeMap::new();
    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        if let Some((k, v)) = line.split_once('=') {
            let k = k.trim();
            if k.chars()
                .all(|c| c.is_ascii_uppercase() || c.is_ascii_digit() || c == '_')
            {
                map.entry(k.to_string())
                    .or_insert_with(|| literal_assignment_value(v).to_string());
            }
        }
    }
    map
}

/// 从 download_model.sh 提取内嵌权重清单的文件名列（`<64位hex>  <文件名>` 行）。
/// 这代卡把 HF LFS 清单（sha256 + 文件名）heredoc 在脚本里，对齐它而不是自己编。
fn extract_manifest_files(text: &str) -> Vec<String> {
    let mut out = Vec::new();
    for line in text.lines() {
        let t = line.trim();
        let mut parts = t.split_whitespace();
        let (Some(sha), Some(name)) = (parts.next(), parts.next()) else {
            continue;
        };
        if sha.len() == 64 && sha.chars().all(|c| c.is_ascii_hexdigit()) && !name.is_empty() {
            out.push(name.to_string());
        }
    }
    out.sort();
    out.dedup();
    out
}

/// compose 一代卡的权重完整性口径（HF 快照目录，非单文件）：
/// - download_model.sh 内嵌了 sha256 清单时：清单文件名齐 + 卡片自己的校验标记
///   （.lmm-weights-verified 内容 == LMM_WEIGHTS_MANIFEST_SHA）才算完整；
/// - 没内嵌清单时：对齐 vendor scripts/common.sh 的 model_complete（config.json
///   + 至少一个 *.safetensors）。
///
/// 同名目录误认防线（真实案例：本机 Qwen3.8-27B-NVFP4/ 同时是另一张卡的
/// unsloth 单文件快照+软链，而这代卡要的是 RadixArk 三分片）：宁可判不完整
/// 让 download_model.sh 幂等重下/续传，不可把别家快照认成这份权重。
fn compose_weights_complete(card: &Card, model_dir: &str) -> Option<bool> {
    let dir = Path::new(model_dir);
    if !dir.is_dir() {
        return Some(false);
    }
    if !card.weight_files.is_empty() {
        let files_ok = card.weight_files.iter().all(|f| dir.join(f).is_file());
        if !files_ok {
            return Some(false);
        }
        return match &card.weights_manifest_sha {
            Some(sha) => {
                let mark = std::fs::read_to_string(dir.join(".lmm-weights-verified")).ok();
                Some(mark.map(|m| m.trim() == sha.trim()).unwrap_or(false))
            }
            // 清单在但没声明哈希：文件齐即算（没有可核对的标记）
            None => Some(true),
        };
    }
    // vendor model_complete 兜底：config.json + 至少一个 *.safetensors
    let has_config = dir.join("config.json").is_file();
    let has_safetensors = std::fs::read_dir(dir)
        .map(|entries| {
            entries.flatten().any(|e| {
                e.file_name()
                    .to_str()
                    .map(|n| n.ends_with(".safetensors"))
                    .unwrap_or(false)
            })
        })
        .unwrap_or(false);
    Some(has_config && has_safetensors)
}

/// compose 型容器卡识别（无 common.sh）：install_engine.sh 声明 VENDOR_DIR 且
/// 内嵌 compose.yaml 即认为是这代卡。部署信息链：vendor/.env（deploy 产物，
/// 最准）→ vendor/.env.example（install_engine 产物）→ install_engine.sh 内嵌
/// heredoc 的 KEY=VALUE 行（安装前兜底，只填展示/映射字段，不算「已部署」）。
fn scan_compose_card(dir: &Path, name: String) -> Option<Card> {
    let text = std::fs::read_to_string(dir.join("install_engine.sh")).ok()?;
    if !text.contains("VENDOR_DIR=") || !text.contains("compose.yaml") {
        return None;
    }
    let vendor_dir = extract_sh_value(&text, "VENDOR_DIR").map(|v| PathBuf::from(expand_home(&v)));
    let env_text = vendor_dir.as_ref().and_then(|vd| {
        std::fs::read_to_string(vd.join(".env"))
            .ok()
            .or_else(|| std::fs::read_to_string(vd.join(".env.example")).ok())
    });
    let deployed = env_text.is_some();
    let map = match env_text {
        Some(t) => parse_kv_lines(&t),
        None => first_wins_kv_lines(&text),
    };
    let get = |k: &str| map.get(k).map(|v| expand_home(v));
    let image = get("IMAGE");
    let container_name = get("CONTAINER_NAME");
    let docker_deploy = if deployed {
        Some(DockerDeploy {
            image: image.clone()?,
            container_name: container_name.clone()?,
            port: get("PORT")?.parse().ok()?,
            model_dir: get("MODEL_PATH")?,
            mem_fraction_static: get("MEM_FRACTION_STATIC").and_then(|v| v.parse().ok()),
            vendor_dir: vendor_dir.clone(),
            host_bind: get("HOST_BIND"),
            runtime_flags: [
                ("CONTEXT_LENGTH", "--context-length"),
                ("SERVED_MODEL_NAME", "--served-model-name"),
                ("MAX_RUNNING_REQUESTS", "--max-running-requests"),
                ("KV_CACHE_DTYPE", "--kv-cache-dtype"),
            ]
            .into_iter()
            .filter_map(|(key, flag)| {
                get(key)
                    .filter(|v| !v.is_empty() && !v.contains('$'))
                    .map(|value| (flag.to_string(), value))
            })
            .collect(),
        })
    } else {
        None
    };
    // 权重清单：download_model.sh 内嵌的 sha256 清单文件名 + 清单自身哈希
    let dl = std::fs::read_to_string(dir.join("download_model.sh")).unwrap_or_default();
    let weight_files = extract_manifest_files(&dl);
    let weights_manifest_sha = extract_sh_value(&dl, "LMM_WEIGHTS_MANIFEST_SHA");
    // card.json（MCP 物化时随卡写入的商店/仓库元数据）：size_bytes 是这代卡
    // 唯一可靠的体积口径（install 进度分母 LMM_MODEL_SIZE_BYTES、「权重+3.5GiB」
    // 预算兜底、磁盘不足摘要、GiB 展示）；没有它维持 0 的保守行为。
    let total_bytes = std::fs::read_to_string(dir.join("card.json"))
        .ok()
        .and_then(|t| crate::json::Value::parse(&t).ok())
        .and_then(|v| v.get("size_bytes").and_then(|x| x.as_u64()))
        .unwrap_or(0);
    Some(Card {
        name,
        // 这代卡不带 MODEL_ID：enable 后从 /v1/models 现场问（健康探测回填路径）
        model_id: String::new(),
        total_bytes,
        engine_sha8: String::new(),
        engine_kind: EngineKind::Container,
        image,
        container_name,
        deploy: None,
        docker_deploy,
        big_files: Vec::new(),
        small_files: Vec::new(),
        weight_files,
        weights_manifest_sha,
        serve_flags: None,
        native_reserved_gib: None,
    })
}

pub fn scan_cards(cards_dir: &Path) -> Vec<Card> {
    let mut out = Vec::new();
    let Ok(entries) = std::fs::read_dir(cards_dir) else {
        return out;
    };
    for entry in entries.flatten() {
        let dir = entry.path();
        if !dir.is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        let common = dir.join("common.sh");
        let Ok(text) = std::fs::read_to_string(&common) else {
            // 无 common.sh：compose 型容器卡（mtp/vllm 一代）
            if let Some(card) = scan_compose_card(&dir, name) {
                out.push(card);
            }
            continue;
        };
        if let Some(sha) = extract_sh_value(&text, "ENGINE_BIN_SHA256") {
            // 原生驭光卡的指纹：内容寻址引擎声明
            let (Some(model_id), Some(_artifact)) = (
                extract_sh_value(&text, "MODEL_ID"),
                extract_sh_value(&text, "ARTIFACT"),
            ) else {
                continue;
            };
            let total_bytes = extract_sh_value(&text, "TOTAL_BYTES")
                .and_then(|v| v.parse::<u64>().ok())
                .unwrap_or(0);
            let deploy = std::fs::read_to_string(dir.join(".deploy-env"))
                .ok()
                .and_then(|t| parse_deploy_env(&t));
            let serve_flags = std::fs::read_to_string(dir.join("enable.sh"))
                .ok()
                .and_then(|t| extract_serve_flags(&t));
            out.push(Card {
                name,
                model_id,
                total_bytes,
                engine_sha8: sha[..8.min(sha.len())].to_string(),
                engine_kind: EngineKind::Native,
                image: None,
                container_name: None,
                deploy,
                docker_deploy: None,
                big_files: Vec::new(),
                small_files: Vec::new(),
                weight_files: Vec::new(),
                weights_manifest_sha: None,
                serve_flags,
                native_reserved_gib: extract_sh_value(&text, "CMSD_RESERVED_GIB").map(|value| {
                    value
                        .parse::<f64>()
                        .ok()
                        .filter(|number| number.is_finite() && *number > 0.0)
                        .unwrap_or(f64::INFINITY)
                }),
            });
            continue;
        }
        // docker 容器卡的指纹：镜像 + 容器名（无内容寻址引擎）
        let (Some(image), Some(container_name)) = (
            extract_sh_value(&text, "IMAGE"),
            extract_sh_value(&text, "CONTAINER_NAME"),
        ) else {
            continue;
        };
        let big_files: Vec<(String, u64)> = extract_sh_array(&text, "LMM_BIG_FILES")
            .iter()
            .filter_map(|entry| {
                let (name, bytes) = entry.split_once(':')?;
                Some((name.to_string(), bytes.parse::<u64>().ok()?))
            })
            .collect();
        let small_files = extract_sh_array(&text, "LMM_SMALL_FILES");
        let total_bytes = big_files.iter().map(|(_, b)| *b).sum();
        let mut docker_deploy = parse_docker_deploy(&dir);
        // 静态预留口径：vendor/start.sh 的 MEM_FRACTION_STATIC 默认值
        // （${MEM_FRACTION_STATIC:-0.60} → 0.60，extract_sh_value 已处理 :-）
        let mem_fraction = std::fs::read_to_string(dir.join("vendor/start.sh"))
            .ok()
            .or_else(|| std::fs::read_to_string(dir.join("start.sh")).ok())
            .and_then(|t| extract_sh_value(&t, "MEM_FRACTION_STATIC"))
            .and_then(|v| v.parse::<f64>().ok());
        if let Some(d) = docker_deploy.as_mut() {
            d.mem_fraction_static = mem_fraction;
        }
        out.push(Card {
            name,
            model_id: extract_sh_value(&text, "MODEL_ID").unwrap_or_default(),
            total_bytes,
            engine_sha8: String::new(),
            engine_kind: EngineKind::Container,
            image: Some(image),
            container_name: Some(container_name),
            deploy: None,
            docker_deploy,
            big_files,
            small_files,
            weight_files: Vec::new(),
            weights_manifest_sha: None,
            serve_flags: None,
            native_reserved_gib: None,
        });
    }
    out.sort_by(|a, b| a.name.cmp(&b.name));
    out
}

/// 从 enable.sh 的 tachyon-serve 启动命令里提取字面量旗标（D3：参数沿用卡片
/// enable.sh 的生产配置）。含变量（$）的参数由 env 文件注入，这里只收
/// `--flag value` 中 value 为字面量的对和孤立开关。
/// 例如 qwen 卡提取出 `--kv-dtype fp8 --spec dflash2 --draft-tokens 7 --lm-head-draft`。
pub fn extract_serve_flags(enable_sh: &str) -> Option<String> {
    // 拼起续行，找 tachyon-serve 调用段
    let joined: String = enable_sh
        .lines()
        .map(|l| l.trim_end())
        .collect::<Vec<_>>()
        .join(" ")
        .replace("\\ ", " ")
        .replace('\\', " ");
    let after_bin = joined.find("tachyon-serve")? + "tachyon-serve".len();
    // 跳过引擎路径 token 的残余（其引号可能未配对），从下一个空白后正式开始
    let start = joined[after_bin..]
        .find(char::is_whitespace)
        .map(|p| after_bin + p)?;
    // 命令到 & 或行尾关键标记结束（nohup ... >> log 2>&1 &）
    let segment: String = joined[start..]
        .split(">>")
        .next()
        .unwrap_or("")
        .split(" < /dev/null")
        .next()
        .unwrap_or("")
        .trim()
        .to_string();
    // 简单分词（尊重双引号）
    let mut tokens: Vec<String> = Vec::new();
    let mut cur = String::new();
    let mut in_quote = false;
    for ch in segment.chars() {
        match ch {
            '"' => in_quote = !in_quote,
            c if c.is_whitespace() && !in_quote => {
                if !cur.is_empty() {
                    tokens.push(std::mem::take(&mut cur));
                }
            }
            c => cur.push(c),
        }
    }
    if !cur.is_empty() {
        tokens.push(cur);
    }
    let mut flags: Vec<String> = Vec::new();
    let mut i = 0;
    while i < tokens.len() {
        let t = &tokens[i];
        if t.starts_with("--") {
            let next = tokens.get(i + 1);
            match next {
                // --flag 字面量值（不含 $ 不以 -- 开头）→ 收一对
                Some(v) if !v.starts_with("--") && !v.contains('$') => {
                    flags.push(t.clone());
                    flags.push(v.clone());
                    i += 2;
                }
                // 值含变量（由 env 文件提供）→ 整对跳过
                Some(v) if !v.starts_with("--") => {
                    let _ = v;
                    i += 2;
                }
                // 孤立开关
                _ => {
                    flags.push(t.clone());
                    i += 1;
                }
            }
        } else {
            i += 1;
        }
    }
    if flags.is_empty() {
        None
    } else {
        Some(flags.join(" "))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_card_keeps_profile_without_a_second_lifecycle_executor() {
        let root = std::env::temp_dir().join(format!("cmsd-native-card-{}", std::process::id()));
        let directory = root.join("ornith15-35b-a3b-nvfp4");
        std::fs::create_dir_all(&directory).unwrap();
        std::fs::write(
            directory.join("common.sh"),
            include_str!("../tests/fixtures/native-card/common.sh"),
        )
        .unwrap();
        std::fs::write(
            directory.join("enable.sh"),
            include_str!("../tests/fixtures/native-card/enable.sh"),
        )
        .unwrap();
        std::fs::write(directory.join(".deploy-env"), DEPLOY).unwrap();
        let cards = scan_cards(&root);
        assert_eq!(cards.len(), 1);
        assert!(matches!(cards[0].engine_kind, EngineKind::Native));
        assert_eq!(cards[0].total_bytes, 22_892_664_832);
        assert_eq!(cards[0].engine_sha8, "0f8a4a7c");
        assert_eq!(cards[0].native_reserved_gib, Some(48.0));
        assert!(cards[0].deploy.is_some());
        let flags = cards[0].serve_flags.as_deref().unwrap();
        assert!(flags.contains("--max-context 262144"));
        assert!(
            flags.contains("--spec mtp --draft-tokens 1 --lm-head-draft --vision --no-thinking")
        );
        assert!(!flags.contains("$"));
        let environment = native_environment(&directory).unwrap();
        assert_eq!(
            environment.get("TACHYON_W4A4_SCHED").map(String::as_str),
            Some("3")
        );
        for bad in ["NaN", "inf", "0", "-1", "$(touch forbidden)"] {
            let text = include_str!("../tests/fixtures/native-card/common.sh").replace(
                "CMSD_RESERVED_GIB=\"48\"",
                &format!("CMSD_RESERVED_GIB=\"{bad}\""),
            );
            std::fs::write(directory.join("common.sh"), text).unwrap();
            assert!(scan_cards(&root)[0]
                .native_reserved_gib
                .unwrap()
                .is_infinite());
        }
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn native_runtime_tuning_is_preserved_without_evaluating_shell() {
        let directory =
            std::env::temp_dir().join(format!("cmsd-native-env-{}", std::process::id()));
        std::fs::create_dir_all(&directory).unwrap();
        std::fs::write(
            directory.join("enable.sh"),
            "export TACHYON_PROJ_W4A4=1 TACHYON_W4A4_SCHED=3\nexport LD_PRELOAD=untrusted\n",
        )
        .unwrap();
        let environment = native_environment(&directory).unwrap();
        assert_eq!(environment.len(), 2);
        assert_eq!(
            environment.get("TACHYON_PROJ_W4A4").map(String::as_str),
            Some("1")
        );
        std::fs::write(
            directory.join("enable.sh"),
            "export TACHYON_PROJ_W4A4=$(touch /never-execute)\n",
        )
        .unwrap();
        assert!(native_environment(&directory).is_err());
        std::fs::remove_dir_all(directory).unwrap();
    }

    const COMMON: &str = r#"
MODEL_ID="Ornith-1.5-35B-A3B-NVFP4"
export PORT="${PORT:-18080}"
ARTIFACT="ornith15-35b-a3b-nvfp4.tachyon"
TOTAL_BYTES="${LMM_MODEL_SIZE_BYTES:-22892664832}"
ENGINE_BIN_SHA256="0f8a4a7cec88304b7dbd8d8e46c2c70e938b77c31ae7185060ab770b52343c35"
"#;

    const DEPLOY: &str = r#"PORT="18080"
HOST="127.0.0.1"
MODEL_DIR="/home/test/.pinvou3/model-store/models/Ornith-1.5-35B-A3B-NVFP4-Tachyon"
ENGINE_DIR="/home/test/.local/share/local-model-manager/engines/tachyon-serve/0f8a4a7c"
ARTIFACT="ornith15-35b-a3b-nvfp4.tachyon"
MODEL_ID="Ornith-1.5-35B-A3B-NVFP4"
MAX_CONCURRENCY="1"
"#;

    #[test]
    fn extracts_common_sh_values() {
        assert_eq!(
            extract_sh_value(COMMON, "MODEL_ID").as_deref(),
            Some("Ornith-1.5-35B-A3B-NVFP4")
        );
        assert_eq!(
            extract_sh_value(COMMON, "TOTAL_BYTES").as_deref(),
            Some("22892664832")
        );
        assert_eq!(
            extract_sh_value(COMMON, "ENGINE_BIN_SHA256").as_deref(),
            Some("0f8a4a7cec88304b7dbd8d8e46c2c70e938b77c31ae7185060ab770b52343c35")
        );
        assert!(extract_sh_value(COMMON, "MISSING").is_none());
    }

    #[test]
    fn parses_deploy_env() {
        let d = parse_deploy_env(DEPLOY).unwrap();
        assert_eq!(d.port, 18080);
        assert_eq!(d.host, "127.0.0.1");
        assert_eq!(d.max_concurrency, 1);
        assert!(d.engine_dir.ends_with("0f8a4a7c"));
    }

    #[test]
    fn scan_finds_only_tachyon_cards() {
        let dir = std::env::temp_dir().join(format!("cmsd-cards-test-{}", std::process::id()));
        let tachyon = dir.join("ornith15-35b-a3b-nvfp4");
        let docker = dir.join("some-docker-card");
        std::fs::create_dir_all(&tachyon).unwrap();
        std::fs::create_dir_all(&docker).unwrap();
        std::fs::write(tachyon.join("common.sh"), COMMON).unwrap();
        std::fs::write(tachyon.join(".deploy-env"), DEPLOY).unwrap();
        // docker 卡没有 ENGINE_BIN_SHA256
        std::fs::write(docker.join("common.sh"), "MODEL_ID=\"x\"\nARTIFACT=\"y\"\n").unwrap();
        let cards = scan_cards(&dir);
        assert_eq!(cards.len(), 1);
        assert_eq!(cards[0].name, "ornith15-35b-a3b-nvfp4");
        assert_eq!(cards[0].engine_sha8, "0f8a4a7c");
        assert_eq!(cards[0].total_bytes, 22892664832);
        assert!(cards[0].deploy.is_some());
        std::fs::remove_dir_all(&dir).ok();
    }
}

#[cfg(test)]
mod docker_card_tests {
    use super::*;

    // 真机 qwen38-27b-sglang0519 卡的关键行（裁剪）
    const SGLANG_COMMON: &str = r#"
IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="qwen38-sglang"
export PORT="${PORT:-30001}"
export MODEL_DIR="${LMM_MODELS_DIR:-$HOME/models}/Qwen3.8-27B-NVFP4-unsloth"
LMM_BIG_FILES=(
    "model.safetensors:22568192096"
    "model_mtp.safetensors:849400392"
)
LMM_SMALL_FILES=(
    config.json generation_config.json model.safetensors.index.json
    tokenizer.json tokenizer_config.json vocab.json
    chat_template.jinja preprocessor_config.json video_preprocessor_config.json
)
"#;

    const VENDOR_DEPLOY_ENV: &str = r#"IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="qwen38-sglang"
PORT="30001"
MODEL_DIR="/home/test/.pinvou3/model-store/models/Qwen3.8-27B-NVFP4-unsloth"
GPU_FLAGS="--gpus all"
"#;

    const VENDOR_START: &str = r#"
PORT="30000"
[ -f "$SCRIPT_DIR/.deploy-env" ] && . "$SCRIPT_DIR/.deploy-env"
MEM_FRACTION_STATIC="${MEM_FRACTION_STATIC:-0.60}"
MAX_RUNNING_REQUESTS="${MAX_RUNNING_REQUESTS:-16}"
"#;

    fn write_sglang_card(root: &Path) -> PathBuf {
        let card = root.join("qwen38-27b-sglang0519");
        std::fs::create_dir_all(card.join("vendor")).unwrap();
        std::fs::write(card.join("common.sh"), SGLANG_COMMON).unwrap();
        // deploy-env 的 MODEL_DIR 指到测试临时目录（绝不沾真机权重目录）
        let deploy = VENDOR_DEPLOY_ENV.replace(
            "/home/test/.pinvou3/model-store/models/Qwen3.8-27B-NVFP4-unsloth",
            root.join("models").to_str().unwrap(),
        );
        std::fs::write(card.join("vendor/.deploy-env"), deploy).unwrap();
        std::fs::write(card.join("vendor/start.sh"), VENDOR_START).unwrap();
        card
    }

    #[test]
    fn scan_finds_docker_card() {
        let dir = std::env::temp_dir().join(format!("cmsd-dockercard-test-{}", std::process::id()));
        write_sglang_card(&dir);
        let cards = scan_cards(&dir);
        assert_eq!(cards.len(), 1);
        let c = &cards[0];
        assert_eq!(c.name, "qwen38-27b-sglang0519");
        assert_eq!(c.engine_kind, EngineKind::Container);
        assert_eq!(c.image.as_deref(), Some("lmsysorg/sglang:v0.5.19"));
        assert_eq!(c.container_name.as_deref(), Some("qwen38-sglang"));
        assert_eq!(c.total_bytes, 22568192096 + 849400392);
        assert_eq!(c.big_files.len(), 2);
        assert_eq!(c.small_files.len(), 9);
        let d = c.docker_deploy.as_ref().unwrap();
        assert_eq!(d.port, 30001);
        assert_eq!(d.mem_fraction_static, Some(0.60));
        assert!(d.model_dir.ends_with("models"), "{}", d.model_dir);
        // 权重不在位：完整性 false（而不是 None——清单与部署都在）
        assert_eq!(c.docker_weights_complete(), Some(false));
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn docker_weights_complete_checks_bytes_and_small_files() {
        let dir = std::env::temp_dir().join(format!("cmsd-dockerw-test-{}", std::process::id()));
        let card_dir = write_sglang_card(&dir);
        // deploy-env 已指向 dir/models（helper 写的）；权重文件落那里
        let model_dir = dir.join("models");
        std::fs::create_dir_all(&model_dir).unwrap();
        // 清单字节数改成测试用小数字（大文件 16B、小 MTP 8B）
        let common = SGLANG_COMMON
            .replace("22568192096", "16")
            .replace("849400392", "8");
        std::fs::write(card_dir.join("common.sh"), common).unwrap();
        std::fs::write(model_dir.join("model.safetensors"), vec![0u8; 16]).unwrap();
        std::fs::write(model_dir.join("model_mtp.safetensors"), vec![0u8; 8]).unwrap();
        for f in [
            "config.json",
            "generation_config.json",
            "model.safetensors.index.json",
            "tokenizer.json",
            "tokenizer_config.json",
            "vocab.json",
            "chat_template.jinja",
            "preprocessor_config.json",
        ] {
            std::fs::write(model_dir.join(f), b"{}").unwrap();
        }
        let cards = scan_cards(&dir);
        assert_eq!(cards[0].docker_weights_complete(), Some(false)); // 缺 video_preprocessor_config.json
        std::fs::write(model_dir.join("video_preprocessor_config.json"), b"{}").unwrap();
        let cards = scan_cards(&dir);
        assert_eq!(cards[0].docker_weights_complete(), Some(true));
        std::fs::remove_dir_all(&dir).ok();
    }
}

#[cfg(test)]
mod serve_flags_tests {
    use super::*;

    // 真机 qwen38-27b-nvfp4-tachyon/enable.sh 的启动段形状
    const QWEN_ENABLE: &str = r#"
nohup "$ENGINE_DIR/tachyon-serve" "$MODEL_DIR/$ARTIFACT" \
    --host "$HOST" --port "$PORT" --model-id "${LMM_MODEL_NAME:-$MODEL_ID}" \
    --max-context 32768 --max-concurrency "$MAX_CONCURRENCY" \
    --device-state-slots 16 --host-state-slots 16 \
    --kv-dtype fp8 --spec dflash2 --draft-tokens 7 --lm-head-draft \
    >> "$LOG_FILE" 2>&1 < /dev/null &
echo $! > "$PID_FILE"
"#;

    #[test]
    fn extracts_literal_flags_from_qwen_card() {
        let flags = extract_serve_flags(QWEN_ENABLE).unwrap();
        // 字面量旗标全部收到
        for want in [
            "--kv-dtype fp8",
            "--spec dflash2",
            "--draft-tokens 7",
            "--lm-head-draft",
            "--max-context 32768",
        ] {
            assert!(flags.contains(want), "缺 {want}：{flags}");
        }
        // 变量参数不收（由 env 文件注入）
        assert!(!flags.contains("$"), "{flags}");
        assert!(!flags.contains("--port"), "{flags}");
        assert!(!flags.contains("--model-id"), "{flags}");
    }

    #[test]
    fn no_enable_flags_is_none() {
        assert!(extract_serve_flags("# nothing here\n").is_none());
    }
}

#[cfg(test)]
mod compose_card_tests {
    use super::*;
    use crate::docker::fixtures as fx;

    const SHA: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

    /// 安装前（无 vendor 目录）：脚本兜底只填展示/映射字段，不算「已部署」。
    #[test]
    fn scan_finds_compose_card_pre_install() {
        let root = fx::tmpdir("compose-pre");
        let cards_dir = root.join("cards");
        let vendor = root.join("vendor");
        fx::write_compose_card(
            &cards_dir,
            "qwen38-27b-nvfp4-mtp",
            "fixture-mtp",
            &vendor,
            &root.join("models"),
            30000,
            SHA,
        );

        let cards = scan_cards(&cards_dir);
        assert_eq!(cards.len(), 1);
        let c = &cards[0];
        assert_eq!(c.name, "qwen38-27b-nvfp4-mtp");
        assert_eq!(c.engine_kind, EngineKind::Container);
        assert_eq!(c.image.as_deref(), Some("fixture/sglang:v0.5.16"));
        assert_eq!(c.container_name.as_deref(), Some("fixture-mtp"));
        assert!(c.model_id.is_empty(), "compose 卡真名等 enable 后现场问");
        assert!(
            c.docker_deploy.is_none(),
            "vendor 还没生成 .env：不算已部署"
        );
        // download_model.sh 内嵌清单提取
        assert_eq!(c.weight_files.len(), 4);
        assert!(c
            .weight_files
            .iter()
            .any(|f| f == "model-00001-of-00003.safetensors"));
        assert!(c.weight_files.iter().any(|f| f == "tokenizer.json"));
        assert_eq!(c.weights_manifest_sha.as_deref(), Some(SHA));
        assert_eq!(c.docker_weights_complete(), None, "未部署给 None 不误判");
        std::fs::remove_dir_all(&root).ok();
    }

    /// card.json（MCP 物化随卡写入）：compose 一代卡的 size_bytes 口径来源
    /// （install 进度分母/预算兜底）；没有它维持 0 的保守行为。
    #[test]
    fn compose_card_total_bytes_from_card_json() {
        let root = fx::tmpdir("compose-meta");
        let cards_dir = root.join("cards");
        let vendor = root.join("vendor");
        fx::write_compose_card(
            &cards_dir,
            "mtp",
            "fixture-mtp",
            &vendor,
            &root.join("models"),
            30000,
            SHA,
        );
        let before = scan_cards(&cards_dir);
        assert_eq!(before[0].total_bytes, 0, "无 card.json 时保守为 0");
        std::fs::write(
            cards_dir.join("mtp").join("card.json"),
            r#"{"id":"mtp","size_bytes":21945295265,"engine_name":"sglang"}"#,
        )
        .unwrap();
        let after = scan_cards(&cards_dir);
        assert_eq!(after[0].total_bytes, 21945295265);
        std::fs::remove_dir_all(&root).ok();
    }

    fn deploy_env(vendor: &Path, models: &Path, port: u16) {
        std::fs::create_dir_all(vendor).unwrap();
        std::fs::write(
            vendor.join(".env"),
            format!(
                "IMAGE=fixture/sglang:v0.5.16\nCONTAINER_NAME=fixture-mtp\nMODEL_PATH={}/Qwen3.8-27B-NVFP4\nCACHE_PATH={}/cache\nHOST_BIND=127.0.0.1\nPORT={}\nMEM_FRACTION_STATIC=0.70\n",
                models.display(),
                vendor.display(),
                port
            ),
        )
        .unwrap();
    }

    /// 已部署（vendor/.env 在）：部署字段全提取；host 网络卡的端口来自 .env。
    #[test]
    fn scan_compose_card_deployed() {
        let root = fx::tmpdir("compose-dep");
        let cards_dir = root.join("cards");
        let vendor = root.join("vendor");
        fx::write_compose_card(
            &cards_dir,
            "mtp",
            "fixture-mtp",
            &vendor,
            &root.join("models"),
            30000,
            SHA,
        );
        deploy_env(&vendor, &root.join("models"), 30000);

        let c = scan_cards(&cards_dir).remove(0);
        let d = c.docker_deploy.as_ref().unwrap();
        assert_eq!(d.image, "fixture/sglang:v0.5.16");
        assert_eq!(d.container_name, "fixture-mtp");
        assert_eq!(d.port, 30000);
        assert!(
            d.model_dir.ends_with("models/Qwen3.8-27B-NVFP4"),
            "{}",
            d.model_dir
        );
        assert_eq!(d.mem_fraction_static, Some(0.70));
        assert_eq!(d.host_bind.as_deref(), Some("127.0.0.1"));
        assert_eq!(d.vendor_dir.as_ref().unwrap(), &vendor);
        // 权重目录还没东西：判不完整（不误认）
        assert_eq!(c.docker_weights_complete(), Some(false));
        std::fs::remove_dir_all(&root).ok();
    }

    /// .env.example 兜底（install_engine 跑过但还没 deploy）：也算已部署档。
    #[test]
    fn scan_compose_card_env_example_fallback() {
        let root = fx::tmpdir("compose-ex");
        let cards_dir = root.join("cards");
        let vendor = root.join("vendor");
        fx::write_compose_card(
            &cards_dir,
            "mtp",
            "fixture-mtp",
            &vendor,
            &root.join("models"),
            30000,
            SHA,
        );
        std::fs::create_dir_all(vendor.join("profiles/benchmark")).unwrap();
        // 只生成 .env.example（deploy 还没跑）
        std::fs::write(
            vendor.join(".env.example"),
            "IMAGE=fixture/sglang:v0.5.16\nCONTAINER_NAME=fixture-mtp\nMODEL_PATH=/x/models/Qwen3.8-27B-NVFP4\nHOST_BIND=127.0.0.1\nPORT=30000\nMEM_FRACTION_STATIC=0.70\n",
        )
        .unwrap();
        let c = scan_cards(&cards_dir).remove(0);
        assert!(c.docker_deploy.is_some(), ".env.example 在也算已部署档");
        assert_eq!(c.docker_deploy.as_ref().unwrap().port, 30000);
        std::fs::remove_dir_all(&root).ok();
    }

    /// 快照权重完整性：清单文件 + 校验标记双条件（同名目录误认防线）。
    #[test]
    fn compose_weights_complete_manifest_and_mark() {
        let root = fx::tmpdir("compose-w");
        let cards_dir = root.join("cards");
        let vendor = root.join("vendor");
        let models = root.join("models");
        fx::write_compose_card(
            &cards_dir,
            "mtp",
            "fixture-mtp",
            &vendor,
            &models,
            30000,
            SHA,
        );
        deploy_env(&vendor, &models, 30000);
        let model_dir = models.join("Qwen3.8-27B-NVFP4");
        std::fs::create_dir_all(&model_dir).unwrap();

        let c = || scan_cards(&cards_dir).remove(0);
        // 1) 目录空 → false
        assert_eq!(c().docker_weights_complete(), Some(false));
        // 2) 清单文件齐但没校验标记 → false（宁可判不完整让下载脚本幂等处理）
        for f in [
            "model-00001-of-00003.safetensors",
            "model-00002-of-00003.safetensors",
            "model-00003-of-00003.safetensors",
            "tokenizer.json",
        ] {
            std::fs::write(model_dir.join(f), b"x").unwrap();
        }
        assert_eq!(c().docker_weights_complete(), Some(false));
        // 3) 标记内容对不上 → false
        std::fs::write(model_dir.join(".lmm-weights-verified"), "bbbb").unwrap();
        assert_eq!(c().docker_weights_complete(), Some(false));
        // 4) 标记匹配 → true
        std::fs::write(model_dir.join(".lmm-weights-verified"), SHA).unwrap();
        assert_eq!(c().docker_weights_complete(), Some(true));
        // 5) 缺一个清单文件 → false（同名目录误认场景：别家的快照文件名单对不上）
        std::fs::remove_file(model_dir.join("model-00003-of-00003.safetensors")).unwrap();
        assert_eq!(c().docker_weights_complete(), Some(false));
        std::fs::remove_dir_all(&root).ok();
    }

    /// 无内嵌清单的卡：对齐 vendor model_complete（config.json + ≥1 *.safetensors）。
    #[test]
    fn compose_weights_complete_fallback() {
        let root = fx::tmpdir("compose-fb");
        let cards_dir = root.join("cards");
        let vendor = root.join("vendor");
        let models = root.join("models");
        let dir = fx::write_compose_card(
            &cards_dir,
            "mtp",
            "fixture-mtp",
            &vendor,
            &models,
            30000,
            SHA,
        );
        // 抹掉清单段（保留 MANIFEST_SHA 变量——此时 weight_files 为空走兜底）
        std::fs::write(
            dir.join("download_model.sh"),
            "#!/bin/sh\nLMM_WEIGHTS_MANIFEST_SHA=\"x\"\nexit 0\n",
        )
        .unwrap();
        deploy_env(&vendor, &models, 30000);
        let model_dir = models.join("Qwen3.8-27B-NVFP4");
        std::fs::create_dir_all(&model_dir).unwrap();

        let c = || scan_cards(&cards_dir).remove(0);
        assert_eq!(c().docker_weights_complete(), Some(false));
        std::fs::write(model_dir.join("config.json"), b"{}").unwrap();
        assert_eq!(
            c().docker_weights_complete(),
            Some(false),
            "只有 config 不算"
        );
        std::fs::write(model_dir.join("model.safetensors"), b"x").unwrap();
        assert_eq!(c().docker_weights_complete(), Some(true));
        std::fs::remove_dir_all(&root).ok();
    }

    #[test]
    fn expand_home_and_first_wins() {
        let home = std::env::var("HOME").unwrap();
        assert_eq!(
            expand_home("$HOME/.local/share/x"),
            format!("{home}/.local/share/x")
        );
        assert_eq!(expand_home("${HOME}/x"), format!("{home}/x"));
        assert_eq!(
            expand_home("$TARGET_HOME/.local/share/local-model-manager/vendor/mtp"),
            format!("{home}/.local/share/local-model-manager/vendor/mtp"),
            "compose 一代卡的 TARGET_HOME（cmsd 永不 pkexec，恒为当前用户）"
        );
        assert_eq!(expand_home("/abs/path"), "/abs/path");
        let map = first_wins_kv_lines("PORT=30000\nPORT=30001\nIMAGE=img:1\nlower=skip\n");
        assert_eq!(
            map.get("PORT").map(|s| s.as_str()),
            Some("30000"),
            "首个出现者胜"
        );
        assert_eq!(map.get("IMAGE").map(|s| s.as_str()), Some("img:1"));
        assert!(
            !map.contains_key("lower"),
            "小写键不收（shell 变量行不收进 env 映射）"
        );
    }

    #[test]
    fn deployment_readers_preserve_literal_paths_and_one_quote_pair() {
        let directory = std::env::temp_dir().join(format!(
            "cmsd-literal-deployment-paths-{}",
            std::process::id()
        ));
        std::fs::create_dir_all(directory.join("vendor")).unwrap();
        for path in [
            "/tmp/with spaces",
            "/tmp/$archive",
            "/tmp/`touch should-not-exist`",
            "/tmp/back\\slash",
            "/tmp/single'quote",
            "/tmp/embedded\"quote",
            "/tmp/trailing\"",
            "/tmp/two-trailing\"\"",
        ] {
            let text = format!(
                "IMAGE=\"image\"\nCONTAINER_NAME=\"fixture\"\nPORT=\"30173\"\nMODEL_DIR=\"{path}\"\nGPU_FLAGS=\"--gpus all\"\n"
            );
            assert_eq!(parse_kv_lines(&text)["MODEL_DIR"], path);
            assert_eq!(first_wins_kv_lines(&text)["MODEL_DIR"], path);
            assert_eq!(extract_sh_value(&text, "MODEL_DIR").as_deref(), Some(path));
            std::fs::write(directory.join("vendor/.deploy-env"), &text).unwrap();
            let docker = parse_docker_deploy(&directory).unwrap();
            assert_eq!(docker.model_dir, path);
            assert_eq!(docker.port, 30173);
            let native = parse_deploy_env(&format!(
                "{text}HOST=\"127.0.0.1\"\nENGINE_DIR=\"{path}\"\nARTIFACT=\"weights\"\nMODEL_ID=\"model\"\n"
            ))
            .unwrap();
            assert_eq!(native.model_dir, path);
            assert_eq!(native.engine_dir, path);
        }
        assert_eq!(literal_assignment_value("unquoted"), "unquoted");
        assert_eq!(parse_kv_lines("PORT=30001\n")["PORT"], "30001");
        assert_eq!(
            first_wins_kv_lines("PORT=30001\nPORT=30002\n")["PORT"],
            "30001"
        );
        std::fs::remove_dir_all(directory).unwrap();
    }
}
