//! 业务编排层：模型商店浏览、下载（引擎+模型判重）、模型库管理（部署/启用/
//! 停用/卸载）都在这里串起来。
//!
//! 核心流程是"脚本驱动"：卡片背后绑定六类脚本（install_engine / deploy /
//! download_model / enable / stop / uninstall），具体怎么装引擎、怎么部署、
//! 怎么启停，全部由脚本决定，本层只负责：判断本地是否已有对应引擎/模型
//! （避免重复下载）、按需下载脚本并执行、记录状态。

use std::collections::{BTreeMap, HashMap};
use std::io::Write;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use super::config::{self, Settings};
use super::db;
use super::disk;
use super::errors::AppError;
use super::import_scan;
use super::proc_timeout;
use super::script_runner::{self, RunScriptOptions, ScriptProgress};
use super::store_client::{
    DeployProfile, DeployTopology, HINT_CHECK_INTERNET_ACCESS, HINT_CONNECT_FAILED,
    HINT_CONNECT_TIMEOUT, StoreCardDetail, StoreCardSummary, StoreClient, StoreSource,
};
use crate::features::monitor::current_ram_snapshot;
use crate::platform::process_resources::ProcessResourceSampler;

pub type ModelManagerError = AppError;

/// ollama 不是 docker 镜像，没有 LMM_ENGINE_IMAGE 标记可用，删除逻辑必须单独
/// 写死一份：停服务、删二进制、删 systemd unit、删 `$HOME/.ollama`。这几步都要
/// 改系统级文件/服务，必须走 pkexec 提权，跟 install_engine 是同一套需要 root
/// 的理由。
///
/// **最后那一步会把模型权重一起删掉。** ollama 的 blob 就存在 `~/.ollama/models`
/// 下（常有几十 GB），这条 `rm -rf` 不区分"引擎"和"模型数据"。重装 ollama 二进制
/// **不会**把它们找回来——每一个 ollama 模型都得重新下载。
///
/// 为什么仍然连着删：只删二进制的话，"删除引擎"会留下几十 GB 谁都看不见的数据
/// （ollama 自己拉的模型不在 LMM 的模型库里，任何界面都列不出来），用户点了删除
/// 却一点空间都没回来。两种做法各有代价，这里选的是真把空间还给用户，代价是
/// 后果必须如实说出去——见 `delete_engine` 的 `affected_models` /
/// `purged_model_data`。
const OLLAMA_UNINSTALL_SCRIPT: &str = r#"#!/bin/bash
set -euo pipefail
TARGET_UID="${PKEXEC_UID:-$(id -u)}"
TARGET_HOME="$(getent passwd "$TARGET_UID" | cut -d: -f6)"
systemctl stop ollama 2>/dev/null || true
systemctl disable ollama 2>/dev/null || true
rm -f /usr/local/bin/ollama /usr/bin/ollama
rm -f /etc/systemd/system/ollama.service
systemctl daemon-reload 2>/dev/null || true
if [ -n "$TARGET_HOME" ]; then
    rm -rf "$TARGET_HOME/.ollama"
fi
echo "ollama 已卸载"
"#;

/// 驭光原生引擎的稳定身份键。`engine_label` 才是给用户展示的文案；不能再把
/// 展示名的改动当成一次新引擎安装。版本取二进制 SHA-256 的前 8 位，和引擎
/// 实际落盘目录一一对应。
const NATIVE_TACHYON_ENGINE_NAME: &str = "驭光（H3C自研推理引擎）";

#[derive(Debug, Clone, serde::Serialize)]
pub struct DownloadResult {
    pub model_name: String,
    pub engine_reused: bool,
    pub model_reused: bool,
}

/// `force_remove_record` 的结果。字段的存在本身就是契约的一部分：这条路径
/// 一个字节都没删，调用方必须把残留信息转达给用户，不能只显示"已移除"。
#[derive(Debug, Clone, serde::Serialize)]
pub struct ForcedRemoval {
    pub card_id: String,
    pub model_name: String,
    /// 推理服务真正认的那个模型 id。**跟 `model_name` 不是一回事**，六张卡里没有
    /// 一张两者相等（ornith：`Ornith-1.5-35B-A3B` vs `ornith15-bf16`）。
    ///
    /// 要带出来，是因为前端「在品悟里使用」存的是这个值；清理那几行死数据时
    /// 只按 `model_name` 找必然落空，留下一行指向已删模型的默认模型。
    /// 没部署过的记录拿不到它，所以是 Option。
    pub endpoint_model: Option<String>,
    /// 记录里登记的体积。注意它**不是**"释放了多少空间"——这次什么都没释放。
    /// 名字特意不叫 freed_bytes，免得调用方顺手当成回收量显示给用户。
    pub recorded_bytes: i64,
    /// 已确认此刻仍在磁盘上的目录（可能为空，为空也不等于干净，见 warning）
    pub residual_paths: Vec<String>,
    /// 一句可以直接显示给用户的话
    pub warning: String,
}

/// `uninstall` 的结果。整个结构体只为一件事存在：**如实说出这次到底做了什么**。
///
/// 之前这个方法返回的是 `rec.size_bytes`（卡片登记的体积），界面照着它说
/// "释放约 XX GB"。那是一句猜出来的话：登记体积是卡片作者写在 card.json 里的
/// 标称值，跟磁盘上真实少掉多少字节没有任何关系。而现在用户还能选"保留权重"
/// ——那条路径上一个 GB 都没释放，再报登记体积就不是不准，是撒谎了。
#[derive(Debug, Clone, serde::Serialize)]
pub struct UninstallOutcome {
    pub card_id: String,
    pub model_name: String,
    /// 这次是按"彻底删除（含权重）"跑的吗。原样回给调用方，让界面的措辞跟
    /// 实际行为绑死，而不是靠调用方记着自己刚才传了什么。
    pub purged_weights: bool,
    /// 卡片是不是还在模型库里。
    ///
    /// 删除权重时只有在后验核对确认文件确实都已清掉，才会是 false。若脚本返回
    /// 成功但仍有残留，记录必须保留并变成 `cleanup_required`，让用户能继续处理，
    /// 绝不能把几十 GB 文件变成列表里看不见的孤儿。
    pub record_kept: bool,
    /// **实测**释放量：脚本跑之前和跑完之后，对同一批候选目录各量一次的差值。
    ///
    /// 它是个**下界**——候选目录是按卡片脚本的约定反推的（真正的 MODEL_PATH
    /// 写在脚本自己的 .env 里，LMM 读不到），卡片把东西放在别处就量不到。
    /// 报少了顶多让用户惊喜，报多了是骗人，所以宁可取下界。
    pub freed_bytes: u64,
    /// 卡片登记的体积。留着只作对照（"登记 20GB、实测释放 19.7GB"），
    /// **不是**回收量，名字也刻意跟 freed_bytes 分开。
    pub recorded_bytes: i64,
    /// 卸完之后确认**仍然在磁盘上**的目录。
    ///
    /// 保留权重时这里就是留下来的权重；彻底删除时这里本该是空的，非空就说明
    /// 脚本没删干净——两种情况都必须让用户看见，不能替他抹平。
    pub remaining_paths: Vec<String>,
    /// 一句可以直接显示给用户的话
    pub summary: String,
}

#[derive(Debug, Clone, serde::Serialize)]
pub struct ModelDetail {
    pub record: db::StoreModelRecord,
    pub runtime_lines: Vec<String>,
    pub runtime_note: String,
}

#[derive(Debug, Clone, serde::Serialize)]
pub struct InstalledEngineInfo {
    pub engine_name: String,
    pub engine_version: Option<String>,
    pub docker_image: Option<String>,
    /// docker/ollama 可以精确统计；厂商交付的二进制引擎只在探针验证过存在时列出，
    /// 但未必有安全的通用方式统计其全部附属文件。
    pub size_bytes: Option<u64>,
    pub affected_models: Vec<String>,
    /// 只有当前客户端知道如何安全删除的引擎才允许点删除。
    pub cleanup_supported: bool,
}

/// 已运行模型的实际进程树占用。它是“现在已经占了多少”的观测值，不拿来替代卡片
/// 的峰值预算：模型加载后的 KV cache 会继续增长，二者是互补关系。
#[derive(Debug, Clone, serde::Serialize)]
pub struct RunningModelMemory {
    pub card_id: String,
    pub card_name: String,
    pub rss_bytes: Option<u64>,
}

/// 启用前的统一内存准入快照。所有字节数都用字节交给前端，避免 GiB/GB 换算混乱。
#[derive(Debug, Clone, serde::Serialize)]
pub struct MemoryAdmission {
    pub card_id: String,
    /// None means system sampling failed, never zero bytes of physical RAM.
    pub total_bytes: Option<u64>,
    pub available_bytes: Option<u64>,
    pub safety_reserve_bytes: Option<u64>,
    pub hard_reserve_bytes: Option<u64>,
    pub candidate_peak_bytes: Option<u64>,
    pub projected_available_bytes: Option<u64>,
    /// Conservative reserve for running models. RSS is not a unified-memory
    /// accounting metric, so it must not be subtracted from a unified budget.
    /// None means at least one workload has no usable budget.
    pub running_growth_bytes: Option<u64>,
    pub running_models: Vec<RunningModelMemory>,
    /// green / caution / warning / unknown。unknown 是“卡片尚未给出可信峰值”，不是 0。
    pub level: String,
    /// Stable reason for the translated UI; message remains for older clients.
    pub reason: String,
    pub requires_confirmation: bool,
    /// 直接可显示给用户的一句话，不暴露 docker / /proc 等实现细节。
    pub message: String,
}

/// `delete_engine` 的结果。跟 `UninstallOutcome` 是同一个理由：**如实说出这次
/// 到底删掉了什么**。
///
/// 这里的"什么"两种形态差别极大：docker 镜像删掉还能自动重装回来，ollama 则是
/// 连 `~/.ollama` 整个删掉、几十 GB 权重一起没。调用方不该靠自己记着引擎是哪种
/// 形态去推断后果，所以两件事都摆在返回值里。
#[derive(Debug, Clone, serde::Serialize)]
pub struct EngineRemoval {
    pub engine_name: String,
    pub engine_version: Option<String>,
    /// 释放量。docker 形态是镜像体积，ollama 是二进制加上 `~/.ollama` 的体积
    /// （**含模型权重**）；都是删除前量的，不是登记值。
    pub freed_bytes: u64,
    /// 这次删除有没有把模型数据一起删掉。ollama 恒为 true，docker 镜像恒为
    /// false。界面的措辞必须跟着它走，不能一律说"只删引擎"。
    pub purged_model_data: bool,
    /// 库里还挂在这个引擎上的卡片名。
    ///
    /// `purged_model_data` 为 false 时，它是"下次部署会自动重装一遍引擎"的卡片；
    /// 为 true 时，它是**权重被一起删掉、需要重新下载**的模型。两种含义都得说给
    /// 用户听，所以算出来就要带出来，不能算完扔掉。
    ///
    /// 注意它列的是卡片名（`db::list_model_names_using_engine` 取的是 card_name），
    /// 给人看的，不是 model_name。
    pub affected_models: Vec<String>,
    /// 一句可以直接显示给用户的话
    pub summary: String,
}

#[derive(Debug, Clone, serde::Serialize)]
pub struct DiskSummary {
    pub managed_total_bytes: i64,
    pub free_bytes: u64,
    pub quota_bytes: Option<u64>,
    /// 模型目录里残留的下载临时块（huggingface_hub 的 *.incomplete）合计字节数。
    ///
    /// 正常下载完会自己清掉；这里统计的是异常中断留下的。它们是「可续传的半成品」
    /// ——所以**不能**自动删（删了用户重新下载就得多下这一截），只在界面上告诉
    /// 用户占了多少，让他自己决定要不要清。
    pub stale_cache_bytes: u64,
}

pub fn suggest_model_name(card: &StoreCardDetail) -> String {
    let mut base = card.summary.name.clone();
    if let Some(q) = &card.summary.quantization {
        base = format!("{base}-{q}");
    }
    let lowered = base.to_lowercase();
    // 必须跟 Python 版的 re.sub(r"[^a-z0-9_.-]+", "-", name) 行为完全一致：
    // 每一段连续的非法字符替换成一个 "-"（即使旁边已经有合法的 "-"，也会
    // 出现 "--"）——这个名字是模型的身份标识，老用户数据库里存的就是 Python
    // 算出来的值，这里算出不一样的名字会导致按名字查找对不上。
    let mut name = String::with_capacity(lowered.len());
    let mut in_bad_run = false;
    for c in lowered.chars() {
        if c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_' || c == '.' || c == '-' {
            name.push(c);
            in_bad_run = false;
        } else if !in_bad_run {
            name.push('-');
            in_bad_run = true;
        }
    }
    let name = name.trim_matches('-').to_string();
    if name.is_empty() {
        card.summary.id.clone()
    } else {
        name
    }
}

/// 脚本来源。两种形态：
/// - `Local`：本地固化的脚本正文（下载模型时抓下来存进数据库的）。**执行时
///   完全不联网**——商店重启、上架新卡片、改脚本都打断不了用户正在做的事。
///   这是日常操作（部署/启用/停用/卸载）唯一使用的形态。
/// - `Remote`：商店给的 URL + 哈希，只在"下载模型"和"手动检查卡片更新"这两
///   个明确需要联网的时刻使用。
enum ScriptsSource {
    Local {
        blobs: BTreeMap<String, db::ScriptBlob>,
        assets: BTreeMap<String, db::ScriptBlob>,
        profiles: Vec<DeployProfile>,
    },
    Remote {
        scripts: BTreeMap<String, String>,
        script_sha256: Option<BTreeMap<String, String>>,
        assets: BTreeMap<String, String>,
        asset_sha256: Option<BTreeMap<String, String>>,
        profiles: Vec<DeployProfile>,
        /// 这些"URL"其实是本地文件路径。生产上**恒为 false**——见
        /// `ModelManager::local_script_paths` 的说明，那里讲了为什么它不再是一个
        /// 生产开关。`fetch_script_content` 据此拒绝一切非 https 的地址。
        local_paths: bool,
    },
}

/// 这类脚本有机会以 **root** 身份执行，取它的正文时必须有登记哈希。
///
/// 只有 install_engine：六个入口脚本里只有它会走 pkexec。判定刻意按
/// `script_type` 而不是按调用时那个 `need_root` 参数——同一份 install_engine
/// 在正式执行前还会以普通用户身份跑一次只读探针，而兜底提权恰恰发生在"普通
/// 用户那次失败之后"。只认 need_root 的话，未验签的内容会先从探针那条腿进来，
/// 再由兜底提权原样送上 root；按 script_type 判定，三条腿取到的是同一份
/// 被校验过的内容。
fn may_run_as_root(script_type: &str) -> bool {
    script_type == "install_engine"
}

/// 返回卡片固化的驭光版本；缺失版本时才使用完整 SHA 的前 8 位。
///
/// 这不是通用 Shell 解析器：只接受驭光交付包里那条明确的
/// `ENGINE_BIN_SHA256="..."` 声明，并同时要求脚本写明 tachyon-serve 的受管
/// 目录。任何不满足格式的旧卡都不迁移，宁可保留旧关联，不能把其他厂商的引擎
/// 误归为驭光。
fn native_tachyon_version(record: &db::StoreModelRecord) -> Option<String> {
    let install = &record.scripts_content.get("install_engine")?.content;
    native_tachyon_identity(
        install,
        record
            .assets_content
            .get("common.sh")
            .map(|blob| blob.content.as_str()),
        record.engine_version.as_deref(),
    )
    .map(|(version, _)| version)
}

fn declared_native_sha(script: &str) -> Option<String> {
    let value = script.lines().find_map(|line| {
        let value = line.trim().strip_prefix("ENGINE_BIN_SHA256=")?;
        Some(value.trim().trim_matches(['\"', '\'']))
    })?;
    (value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit()))
        .then(|| value.to_ascii_lowercase())
}

fn native_tachyon_identity(
    install: &str,
    common: Option<&str>,
    version: Option<&str>,
) -> Option<(String, String)> {
    // 厂商卡片把版本与目录声明放在 common.sh；旧单文件卡才内联在入口里。
    let script = if install.contains("$CARD_DIR/common.sh") {
        common?
    } else {
        install
    };
    if !script.contains("tachyon-serve") || !script.contains("local-model-manager/engines") {
        return None;
    }
    let value = declared_native_sha(script)?;
    // 非空的卡片版本可能是研发构建批次（如 7fca766a），并不等于文件 SHA。
    // 保留这类版本，只为历史上遗漏版本的卡片补二进制指纹。
    Some((
        version
            .filter(|v| !v.is_empty())
            .map(str::to_string)
            .unwrap_or_else(|| value[..8].to_string()),
        value,
    ))
}

/// 一张卡片抓下来的完整脚本包：六个入口脚本 + 附属文件。
struct CardPackage {
    scripts: BTreeMap<String, db::ScriptBlob>,
    assets: BTreeMap<String, db::ScriptBlob>,
}

/// 一次脚本执行需要的落地结果：入口脚本路径 + 工作目录。
///
/// 多文件卡片的入口脚本要相对自己的位置去找同级文件（`megacube.sh` 先 cd 到
/// 自己所在目录，再调 `controller.py`），所以必须把工作目录一起交出去。
struct PreparedScript {
    entry: std::path::PathBuf,
    cwd: Option<std::path::PathBuf>,
}

impl ScriptsSource {
    /// `local_paths` 来自 `ModelManager::local_script_paths`，生产上恒为 false。
    fn from_card(card: &StoreCardDetail, local_paths: bool) -> Self {
        Self::Remote {
            scripts: card.scripts.clone(),
            script_sha256: card.script_sha256.clone(),
            assets: card.assets.clone(),
            asset_sha256: card.asset_sha256.clone(),
            profiles: card.profiles.clone(),
            local_paths,
        }
    }

    /// 取这类脚本（以及跟它一起落地的附属文件）正文时的来源策略。
    ///
    /// 附属文件跟入口脚本共用同一份策略是有意的：多文件卡片会把整棵文件树落进
    /// 同一个工作目录，install_engine 以 root 跑起来之后调用的就是它们——对入口
    /// 脚本验签、却放任它下一行要执行的 controller.py 不验，等于没验。
    fn fetch_policy(&self, script_type: &str) -> script_runner::FetchPolicy {
        script_runner::FetchPolicy {
            may_run_as_root: may_run_as_root(script_type),
            allow_local_path: matches!(
                self,
                Self::Remote {
                    local_paths: true,
                    ..
                }
            ),
        }
    }

    fn profiles(&self) -> &[DeployProfile] {
        match self {
            Self::Local { profiles, .. } | Self::Remote { profiles, .. } => profiles,
        }
    }

    /// 某类脚本当前内容的 sha256（用于判断"跟上次跑的那份是不是同一个版本"）。
    fn script_hash(&self, script_type: &str) -> Option<String> {
        match self {
            Self::Local { blobs, .. } => blobs.get(script_type).map(|b| b.sha256.clone()),
            Self::Remote { script_sha256, .. } => script_sha256
                .as_ref()
                .and_then(|h| h.get(script_type))
                .cloned(),
        }
    }

    /// 取一份可随已安装引擎长期保存的可选脚本。`uninstall_engine` 不属于模型
    /// 生命周期必须的六类脚本：老卡片没有它不能妨碍下载；新卡片提供它时，则
    /// 在引擎安装完成的同一事务里固化，之后即使模型记录被删除也还有安全的清理
    /// 契约可用。
    fn optional_script_blob(
        &self,
        script_type: &str,
    ) -> Result<Option<db::ScriptBlob>, ModelManagerError> {
        match self {
            Self::Local { blobs, .. } => Ok(blobs.get(script_type).cloned()),
            Self::Remote { scripts, .. } if !scripts.contains_key(script_type) => Ok(None),
            Self::Remote { .. } => {
                let content = self.entry_content(script_type, self.fetch_policy(script_type))?;
                Ok(Some(db::ScriptBlob {
                    sha256: script_runner::sha256_hex(&content),
                    content,
                }))
            }
        }
    }

    /// 卡片是不是带附属文件（也就是「多文件包」形态）。
    fn has_assets(&self) -> bool {
        match self {
            Self::Local { assets, .. } => !assets.is_empty(),
            Self::Remote { assets, .. } => !assets.is_empty(),
        }
    }

    /// 把某类脚本准备成可执行状态。
    ///
    /// - 没有附属文件的卡片（历史上的所有卡片）：还是落一个临时文件就跑，行为不变。
    /// - 有附属文件的卡片：把整棵文件树落到**卡片自己的持久目录**里，入口脚本也
    ///   写进去，并把这个目录作为工作目录返回。持久是必须的——包会在自己目录里
    ///   生成运行期状态（config.json、node.json、state/），放临时目录的话每次执行
    ///   都是一个全新的空包，部署状态一次都留不住。
    fn prepare(
        &self,
        card_id: &str,
        script_type: &str,
    ) -> Result<PreparedScript, ModelManagerError> {
        let entry_name = format!("{script_type}.sh");
        let policy = self.fetch_policy(script_type);
        if !self.has_assets() {
            return Ok(PreparedScript {
                entry: self.materialize_entry(script_type, policy)?,
                cwd: None,
            });
        }

        // card_id 来自商店响应，直接 join 的话 `../` 或绝对路径会整体逃出 cards 目录。
        let safe_id = config::safe_path_component(card_id)
            .map_err(|e| ModelManagerError::new(format!("卡片 id 不合法：{e}")))?;
        let dir = config::data_dir().join("cards").join(safe_id);
        {
            use std::os::unix::fs::DirBuilderExt;
            std::fs::DirBuilder::new()
                .mode(0o700)
                .recursive(true)
                .create(&dir)
        }
        .map_err(|e| ModelManagerError::new(format!("创建卡片目录失败：{e}")))?;

        let assets = self.asset_contents(policy)?;
        // Native multi-file cards use CARD_DIR/.runtime. Initialize before any
        // entry script (including health logging); existing directories are
        // unchanged and remain subject to the script's identity/mode checks.
        if assets.contains_key("native_engine_process.py") {
            use std::os::unix::fs::DirBuilderExt;
            std::fs::DirBuilder::new()
                .mode(0o700)
                .recursive(true)
                .create(dir.join(".runtime"))
                .map_err(|e| ModelManagerError::new(format!("初始化运行目录失败：{e}")))?;
        }
        script_runner::materialize_assets(&dir, &assets)?;

        // 入口脚本也按固定文件名写进包里，这样包内其它脚本也能互相引用
        let mut entries = BTreeMap::new();
        if let Self::Local { blobs, .. } = self {
            // Sibling scripts (enable -> stop, deploy -> enable) must come from
            // the same DB snapshot as common.sh, not an older materialization.
            for (kind, blob) in blobs {
                if ![
                    "install_engine",
                    "download_model",
                    "deploy",
                    "enable",
                    "stop",
                    "uninstall",
                    "uninstall_engine",
                ]
                .contains(&kind.as_str())
                {
                    return Err(ModelManagerError::new(
                        "本地卡片包含未知脚本入口，请检查卡片更新；未执行任何脚本。",
                    ));
                }
                entries.insert(format!("{kind}.sh"), blob.content.clone());
            }
        }
        entries.insert(entry_name.clone(), self.entry_content(script_type, policy)?);
        script_runner::materialize_assets(&dir, &entries)?;

        Ok(PreparedScript {
            entry: dir.join(&entry_name),
            cwd: Some(dir),
        })
    }

    /// 单文件形态：把入口脚本落成一个可执行临时文件。
    fn materialize_entry(
        &self,
        script_type: &str,
        policy: script_runner::FetchPolicy,
    ) -> Result<std::path::PathBuf, ModelManagerError> {
        script_runner::materialize_script(&self.entry_content(script_type, policy)?)
            .map_err(ModelManagerError::from)
    }

    /// 取入口脚本正文。Local 用本地副本（不联网），Remote 才下载并按 `policy`
    /// 校验来源和哈希。
    fn entry_content(
        &self,
        script_type: &str,
        policy: script_runner::FetchPolicy,
    ) -> Result<String, ModelManagerError> {
        match self {
            Self::Local { blobs, .. } => blobs
                .get(script_type)
                .map(|b| b.content.clone())
                .ok_or_else(|| {
                    ModelManagerError::new(format!(
                        "本地没有该模型的 {script_type} 脚本副本，请在模型卡片上点「检查卡片更新」重新获取"
                    ))
                }),
            Self::Remote {
                scripts,
                script_sha256,
                ..
            } => {
                let url = scripts.get(script_type).ok_or_else(|| {
                    ModelManagerError::new(format!("该模型卡片没有提供 {script_type} 脚本"))
                })?;
                let expected = script_sha256
                    .as_ref()
                    .and_then(|h| h.get(script_type))
                    .map(|s| s.as_str());
                Ok(script_runner::fetch_script_content(url, expected, policy)?)
            }
        }
    }

    /// 取全部附属文件的正文。Remote 会逐个下载并按 `policy` 校验来源和哈希。
    fn asset_contents(
        &self,
        policy: script_runner::FetchPolicy,
    ) -> Result<BTreeMap<String, String>, ModelManagerError> {
        match self {
            Self::Local { assets, .. } => Ok(assets
                .iter()
                .map(|(k, v)| (k.clone(), v.content.clone()))
                .collect()),
            Self::Remote {
                assets,
                asset_sha256,
                ..
            } => {
                let mut out = BTreeMap::new();
                for (path, url) in assets {
                    let expected = asset_sha256
                        .as_ref()
                        .and_then(|h| h.get(path))
                        .map(|s| s.as_str());
                    out.insert(
                        path.clone(),
                        script_runner::fetch_script_content(url, expected, policy)?,
                    );
                }
                Ok(out)
            }
        }
    }
}

pub type ProgressFn<'a> = dyn FnMut(&str) + Send + 'a;

pub struct ModelManager {
    pub settings: Settings,
    store: Box<dyn StoreSource>,
    /// 卡片给的 scripts/assets 值是**本地文件路径**而不是 https 链接。
    ///
    /// **生产上恒为 false，而且没有任何生产代码能把它置真**——唯一的写入口是下面
    /// 那个 `#[cfg(test)]` 的 `with_local_script_paths`。
    ///
    /// 它原来叫 `builtin_store`，是一位真正的生产开关：内置测试商店没有服务器，
    /// 卡片详情里的"URL"就是应用自己刚写到 `data_dir()/test_store_scripts/` 下的
    /// 文件路径。内置测试商店 2026-09-30 整体拿掉之后，这条能力在生产上再无合法
    /// 用途，而它的危险没变——一张被改过的卡片只要把 URL 写成任意一个本地路径，
    /// 就能让应用去读它、再把读到的内容当脚本执行，install_engine 那条还经 pkexec
    /// 提权。
    ///
    /// 那为什么不干脆删掉这一位：下载编排那四条集成测试（`local_script_card` 起
    /// 头的那一组）靠它在不起服务器的前提下把整段真跑一遍，包括"登记哈希对不上
    /// 就中止"这条只在 fetch 路径上才成立的判据。留一位只有测试能置真的字段，比
    /// 让这几条测试消失、或者为它们另造一套 https 假服务器都更诚实。
    local_script_paths: bool,
}

/// 长时间不出声也属正常的脚本，允许多久没有输出。
///
/// 装引擎的实质是几 GB 的 `docker pull`；部署可能要 docker build；启用要等推理
/// 引擎把几十 GB 权重加载进显存——ornith15 的启动脚本自己就写着「首次启动可能
/// 需要 5-10 分钟」，而且它内部的等待上限是 1200 秒。我们的阈值必须高过脚本
/// 自己的上限，否则脚本还没来得及给出一句有意义的错误就被我们杀了（真机上就
/// 这么误杀过：容器其实已经健康起来了，界面却报「判定为卡死」）。
///
/// 脚本那边也补了心跳输出，正常情况下根本不会走到这个阈值；这里是兜底。
/// 绝对上限（6 小时）依然有效，真挂死不会无限等下去。
const LONG_RUNNING_IDLE_TIMEOUT: Duration = Duration::from_secs(1800);

/// 某类脚本允许静默多久。`None` = 用 script_runner 的默认值（300 秒）。
///
/// 走默认值的是 `stop` 和 `download_model`——它们都**一路有输出**，五分钟不出声
/// 确实就是卡住了：`docker compose down` 会逐个打印容器停止的进度，下载则按
/// `LMM_PROGRESS_BYTES=` 持续汇报字节数。其余四类全部放宽，各有各的理由：
///
/// - `install_engine`：几 GB 的 `docker pull`；
/// - `deploy`：可能要 docker build；
/// - `enable`：等推理引擎把几十 GB 权重加载进显存；
/// - `uninstall`：**这一条是补上的，不是顺手加的**。注释以前写着「停用/卸载是
///   快操作」——停用是，卸载不是。卸载的实质是几百 GB 的 `rm -rf`，而且全程
///   零输出：`shutil.rmtree` / `rm -rf` 不打进度，脚本在删完之前没有任何一行
///   可打。deepseek-v4-flash-vision-dual 要 SSH 到两台机器各删约 168 GB（里面
///   还有几十万个 JIT 缓存小文件，元数据操作比字节数更磨时间），
///   qwen38fn-nvfp4-single 是 133 GB，qwen38-27b-sglang0519 还要先
///   `docker stop -t 60` 再删 23 GB。这些没有一个能保证在 300 秒内出声。
///
///   被误杀的后果比"慢"严重得多：删到一半的进程被 kill 之后，磁盘是半清理的，
///   而数据库记录还完好——而厂商 uninstall 脚本自己有一条
///   `[[ -f "${MODEL_PATH}/config.json" ]] || die "拒绝删除：目标不像模型目录"`
///   的安全检查，`rm -rf` 按 readdir 顺序走，config.json 多半早没了，于是重试
///   永远 die，记录永远删不掉，那几十 GB 从任何界面都回收不了。这个超时值直接
///   决定了会不会**制造**出那种状态。
///
/// 绝对上限（DEFAULT_TIMEOUT，6 小时）对所有脚本依然有效，真挂死不会无限等。
fn idle_timeout_for(script_type: &str) -> Option<Duration> {
    match script_type {
        "install_engine" | "deploy" | "enable" | "uninstall" => Some(LONG_RUNNING_IDLE_TIMEOUT),
        _ => None,
    }
}

/// 只读探针的结果
#[derive(Default)]
struct EngineProbe {
    /// 引擎已经装好了（可以整段跳过安装）
    installed: bool,
    /// 这次安装需要管理员权限（脚本打印了 LMM_NEED_ROOT=1）
    need_root: bool,
}

const ENGINE_PROBE_TIMEOUT: Duration = Duration::from_secs(60);
const ENGINE_PROBE_IDLE_TIMEOUT: Duration = Duration::from_secs(20);

/// `docker rmi` 的时限。
///
/// 刻意**不用** `proc_timeout::QUERY_TIMEOUT`（5 秒）：那个值是给"拿一下状态
/// 给界面看"的查询定的，而 rmi 是真删——删一个几十 GB 的镜像要逐层删掉成千
/// 上万个文件，在慢盘上几分钟很正常，5 秒会把正常删除当成卡死。这里要挡的
/// 从来不是"慢"，而是"永远不返回"，所以给得宽一些不影响它的作用。
const DOCKER_RMI_TIMEOUT: Duration = Duration::from_secs(600);

/// 下载权重时自动续传的次数上限。
///
/// 实测目标网络对几十 GB 的大文件传输不稳定，经常传几个 GB 就断一次
/// （2026-09-17 真机：MiniMax-H3 的 40GB 权重在第 375 秒断在
/// `error sending request for url (https://cas-server.xethub.hf.co/…)`）。
/// 每次续传都是脚本自己从断点接着传，已经下好的分片不会浪费，所以这个数字
/// 给得大一点没有成本，纯粹是拿更多次机会换"耐心等它断断续续地传完"。
///
/// 为什么要自动续传：这条曾经被改成"不自动重试，让用户自己决定什么时候接着传"。
/// 那个顾虑本身是对的——用户该对长任务有掌控权——但在这台设备的网络条件下，
/// 它的实际效果是让用户守着屏幕每隔几分钟点一次「下载」，一个 40GB 的模型要点
/// 几十次。掌控权应该体现在**能随时取消**和**每次续传都看得见**，而不是
/// 逼他手动驱动每一段传输。
///
/// 所以现在：非致命失败自动续传，每次都往日志里写一行（用户看得见在干什么），
/// 每次续传前先看取消信号（用户随时能停），次数用尽仍失败才抛给用户。
const DOWNLOAD_RESUME_LIMIT: u32 = 30;

/// 引擎清理契约补取的会话级负缓存（见 `refresh_native_engine_cleanup_contracts`）。
/// ModelManager 每次调用都是新实例，进程级 static 是唯一能跨调用记住失败的层。
static CONTRACT_SYNC_FAILED_CARDS: std::sync::OnceLock<
    std::sync::Mutex<std::collections::HashSet<String>>,
> = std::sync::OnceLock::new();

/// reconcile 端点探测的正结果缓存（r19 评审 P2-7）：card_id -> 最近一次探到
/// 端点活着的时刻。
///
/// `reconcile_running_status` 是 `library()` 的第一步（service.rs 每次 library()
/// 都触发），反向认领要对每张 deployed + OpenAI 端点的未运行卡逐张探
/// `/v1/models`——串行、单张最长蹲满一个超时，K 张卡 = K×超时/刷新，卡多的
/// 设备上商店列表就是K 个超时叠出来的转圈。缓存**只记成功**（端点活着），
/// 60 秒内直接复用；失败不缓存——死讯会挡住刚恢复的服务，宁可下一秒再探。
/// 放行方向本来也只是「不改状态」，60 秒的陈旧度换掉 K 次串行探测是划算的。
///
/// 与 CONTRACT_SYNC_FAILED_CARDS 同款进程级 static：ModelManager 每次调用都
/// 是新实例，只有进程级状态能跨调用记住；键是 card_id，条目数上界就是卡数。
static RECONCILE_ALIVE_ENDPOINTS: std::sync::OnceLock<std::sync::Mutex<HashMap<String, Instant>>> =
    std::sync::OnceLock::new();

/// [`RECONCILE_ALIVE_ENDPOINTS`] 的有效期。
const RECONCILE_PROBE_CACHE_TTL: Duration = Duration::from_secs(60);

/// 存量设备的目录链安全迁移（2026-10-22 根治）。
///
/// 旧客户端用不带 mode 的 `create_dir_all` 在 umask 0002 下把模型商店自有目录
/// 建成了 **775（组可写）**；新版安全脚本（安全卸载 / v3 生命周期）按设计拒绝在
/// 组可写目录下操作——不迁移的话存量设备一更新脚本就撞「删除路径父链不安全」，
/// 且无自助出路（用户设备实锤）。每次启动执行，幂等：
/// - 范围：商店根目录、cards/、logs/、模型权重根目录，以及 cards 与权重根下的
///   **一级子目录**（每张卡一个，数量有限）；不递归更深——卡片/权重内部由脚本
///   自己管。
/// - 只做**收紧**：属主是当前用户且带组/其他写位的目录去掉这些写位
///   （775→755）。不放宽任何目录、不碰属主不是自己的目录、不改文件。
/// - 失败只记日志不拦流程：迁移尽力而为，真正的安全判定仍在卡片脚本侧
///   fail-closed。
fn tighten_store_directory_permissions(settings: &Settings) {
    use std::os::unix::fs::MetadataExt;
    use std::os::unix::fs::PermissionsExt;

    // SAFETY: getuid() 无参数、无失败路径（libc 直接读进程凭证），调用安全。
    let uid = unsafe { libc::getuid() };
    let mut roots = vec![
        config::data_dir(),
        config::data_dir().join("cards"),
        config::data_dir().join("logs"),
        settings.models_path(),
    ];
    // 一级子目录（cards/<card> 与 models/<model>）
    for root in roots.clone() {
        if let Ok(entries) = std::fs::read_dir(&root) {
            for entry in entries.flatten() {
                if entry.file_type().is_ok_and(|t| t.is_dir()) {
                    roots.push(entry.path());
                }
            }
        }
    }
    for dir in roots {
        let Ok(meta) = std::fs::metadata(&dir) else {
            continue;
        };
        if !meta.is_dir() || meta.uid() != uid {
            continue;
        }
        let mode = meta.permissions().mode();
        if mode & 0o022 == 0 {
            continue; // 无组/其他写位，无需收紧
        }
        let tightened =
            std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(mode & !0o022));
        if let Err(error) = tightened {
            eprintln!(
                "[model-store] 目录安全迁移失败（{}）：{error}",
                dir.display()
            );
        }
    }
}

impl ModelManager {
    /// `store` 用于在测试里注入假的商店数据源；不传则连生产商店
    /// （`config::STORE_URL`，固定地址，见那里的说明）。
    ///
    /// 这里原来还有一个 `new_test_store()`——连**内置测试商店**（6 张卡片的脚本
    /// 用 `include_str!` 编进二进制）。2026-09-30 整体拿掉了：内置副本和生产在架
    /// 卡片已经分叉，而改内置脚本又不触发 CI 的 rust-test。生产商店是唯一数据源。
    pub fn new(settings: Option<Settings>, store: Option<Box<dyn StoreSource>>) -> Self {
        let settings = settings.unwrap_or_else(config::load_settings);
        let store = store.unwrap_or_else(|| Box::new(StoreClient::new(config::STORE_URL)));
        let _ = db::init_db();
        tighten_store_directory_permissions(&settings);
        Self {
            settings,
            store,
            local_script_paths: false,
        }
    }

    /// 放行"脚本地址是本地文件路径"。**只有测试能调**，见
    /// [`ModelManager::local_script_paths`]。
    #[cfg(test)]
    fn with_local_script_paths(mut self) -> Self {
        self.local_script_paths = true;
        self
    }

    // ---------------- 模型商店 ----------------

    pub fn list_store_cards(&self) -> Result<Vec<StoreCardSummary>, ModelManagerError> {
        self.store.list_cards()
    }

    /// **目前零调用点**：详情页走的是 `get_model_detail`（读本地库记录），
    /// 只有"看一张还没下载的卡片的完整详情"才需要它，那个界面还没有。
    /// 留待后续分期接线。
    pub fn get_store_card(&self, card_id: &str) -> Result<StoreCardDetail, ModelManagerError> {
        self.store.get_card(card_id)
    }

    // ---------------- 模型库 ----------------

    /// 数据库里的 running 标记只在 LMM 自己走 enable()/stop() 流程时更新；
    /// 容器崩溃、被外部直接 docker stop、设备重启这些场景完全不经过这两个
    /// 方法，标记会一直停留在错误的"运行中"。这里核对一次真实状态：运行中的
    /// 记录，它引擎对应的 docker 镜像是不是真的还有活着的容器，不是就把标记
    /// 纠正回来。每次打开/刷新"本地模型库"都会跑一遍，返回被纠正的卡片名列表。
    ///
    /// docker 不可用（比如探测超时/命令缺失）时不判断容器卡；Ollama 与厂商原生
    /// 引擎仍按各自本机健康端点核对。没有可靠信号就不误纠正，宁可漏判也不能错判。
    pub fn reconcile_running_status(&self) -> Vec<String> {
        let mut corrected = Vec::new();
        // 本批评审 P2-4：核对逻辑只消费 8 个小字段（容器认领/镜像比对/返回值，
        // 列集见 list_store_models_reconcile_columns），此前 list_store_models 把
        // scripts/assets 两个 MB 级正文列整表拉出来又全程不消费——本函数是
        // library() 的第一步，等于每次刷新模型库都白读一遍全库正文。
        let Ok(records) = db::list_store_models_reconcile_columns() else {
            return corrected;
        };
        if records.is_empty() {
            return corrected;
        }
        // 带超时：docker daemon 卡死时 `docker ps` 既不报错也不退出，而这里是
        // `library()` 的第一步——用户看到的会是**整个模型商店永久转圈**，而不是
        // 「拿不到运行状态」。同一文件里 ollama 那条分支早就设了 5 秒超时，
        // 这两处没设，标准不一致本身就是漏了的信号。
        // docker 不可用时不能据此把容器卡判成已停止，但原生引擎和 Ollama 仍各有
        // 自己可靠的健康检查；不能因为 daemon 挂了就跳过整张模型库的状态纠正。
        // 用 `ps -a` 而不是只看运行中的：无 container_name 的 docker 卡要按
        // 「容器存在与否 + 状态」认领（见下方 owned_running 分支）， exited 的
        // 容器也得能看见。
        let docker_rows =
            proc_timeout::query("docker", &["ps", "-a", "--format", DOCKER_PS_A_FORMAT])
                .map(|stdout| parse_docker_ps_a(&stdout));
        let mut live_images: std::collections::HashSet<String> = std::collections::HashSet::new();
        let mut live_names: std::collections::HashSet<String> = std::collections::HashSet::new();
        if let Some(rows) = docker_rows.as_ref() {
            for row in rows {
                if !row.running {
                    continue;
                }
                live_names.insert(row.name.clone());
                if !row.image.is_empty() {
                    live_images.insert(row.image.clone());
                }
            }
        }

        // 镜像不是身份，只能当作没有容器名时的退路。
        let mut image_of: HashMap<String, Option<String>> = HashMap::new();
        for rec in &records {
            let image = db::get_installed_engine(&rec.engine_name, rec.engine_version.as_deref())
                .ok()
                .flatten()
                .and_then(|e| e.docker_image);
            image_of.insert(rec.card_id.clone(), image);
        }

        // card_id -> 是否有活着的自有容器。键取两个都等于 card_id 的身份：
        // compose 项目名（部署时我们的脚本定的，如 ornith15-bf16 的容器
        // ornith15-api）和容器名（z-image-turbo 直接拿 card_id 当容器名）。
        // 之所以需要：没有 container_name 的老记录（迁移前部署的卡）在设备
        // 重启后会被 docker 的 restart 策略自己拉起来（unless-stopped），商店
        // 对此失明的话用户看到的就是「内存被占着、商店却全部离线」（真机
        // 实锤：ornith15-api 重启后照常服务，DB 里全部 running=0），再启用
        // 别的模型时内存预算也把这 76G 漏掉了。
        let mut owned_running: HashMap<String, bool> = HashMap::new();
        if let Some(rows) = docker_rows.as_ref() {
            for row in rows {
                let keys = [row.compose_project.as_deref(), Some(row.name.as_str())];
                for key in keys.into_iter().flatten() {
                    if key.is_empty() {
                        continue;
                    }
                    let slot = owned_running.entry(key.to_string()).or_insert(false);
                    *slot = *slot || row.running;
                }
            }
        }

        for rec in records {
            // 有容器名就只认容器名——那是启用时脚本亲口报上来的，是确定的身份。
            if let Some(name) = rec
                .container_name
                .as_deref()
                .map(str::trim)
                .filter(|n| !n.is_empty())
            {
                // docker daemon 此刻不可查时，状态未知而不是“容器不存在”。
                if docker_rows.is_none() {
                    continue;
                }
                let alive = live_names.contains(name);
                if rec.running && !alive {
                    let _ = db::set_running(&rec.card_id, false);
                    corrected.push(rec.card_name);
                } else if !rec.running && alive && rec.status == "deployed" {
                    // 启用脚本被静默超时误杀、而 `compose up -d` 是分离式的、
                    // 容器照样起来了——这种要认领回来，否则卡片一直显示「启用」
                    // 按钮，那个容器还会被当成外部容器列出来。
                    let _ = db::set_running(&rec.card_id, true);
                    corrected.push(rec.card_name);
                }
                continue;
            }

            // ollama 既不是容器、也没有镜像，上面两条路都不适用。不单独处理的话
            // 它会一路 continue 到底，running 标记**永远不会被纠正**：服务停了、
            // 机器重启过，库里仍写着 true，界面一直显示「停用」按钮。
            //
            // **不能按「查 /api/ps 看这个模型在不在」来纠正**：ollama 默认空闲
            // 5 分钟就把模型卸出显存，但服务照常提供接口、下次请求会自动加载
            // 回来。按那个判的话，用户启用一个模型、五分钟不碰，界面就自己变回
            // 「未启用」了——把正常的省显存行为报成了故障。
            //
            // 可靠的判据只有一个：**ollama 服务本身在不在**。不在 = 这台机器上
            // 任何 ollama 模型都不可能在服务，置 false 一定对；在 = 不动。
            // 跟本函数另外两处一样，只做「说在跑其实没跑」这一个方向。
            if rec.engine_name == "ollama" {
                if rec.running && !ollama_alive(&self.settings.ollama_host) {
                    let _ = db::set_running(&rec.card_id, false);
                    corrected.push(rec.card_name);
                }
                continue;
            }

            // 厂商原生引擎（例如驭光）没有 docker 容器名，过去会直接落到下面的
            // "没有镜像就 continue"，因此设备重启或进程崩溃后仍会永久显示为运行中；
            // 反过来，启用第二张卡时又会拿这条假状态去作内存判断。它们已经由 enable
            // 脚本交回了本机 OpenAI 兼容端点，直接轻量探测 /v1/models 才是可靠判据。
            // 仍然只做“记录说在跑、实际已不在”的纠正，不凭一个端口把外部服务认领为
            // 本模型商店的运行实例。
            // 但这个探测只对 OpenAI 风格的端点成立（路径为空或 /v1 开头）：z-image
            // 这类文生图卡报的是 /generate，把路径改写成 /v1/models 去探必然 404，
            // 会把正在运行的服务误判成已停止——真机上踩过：容器 healthy 而 UI
            // 永远显示未启用。非 OpenAI 端点不做主动探测，落到下面的镜像比对。
            if let Some(endpoint) = rec.endpoint_url.as_deref() {
                if endpoint_path_is_openai_style(endpoint) {
                    // 探测走 60s TTL 缓存（r19 评审 P2-7，见
                    // openai_endpoint_alive_cached）：本函数每次刷新列表都会跑，
                    // 不能让 K 张卡各蹲一次串行超时。
                    if rec.running && !openai_endpoint_alive_cached(&rec.card_id, endpoint) {
                        let _ = db::set_running(&rec.card_id, false);
                        corrected.push(rec.card_name);
                        continue;
                    }
                    // 任务 G（收编）：记录说没在跑、端点实际有服务、且身份核验
                    // 通过（.runtime/serve.identity.json 的 PID 活着）→ 认领为
                    // 运行中。运维常在终端手动跑 enable.sh 恢复模型，商店对此
                    // 失明的话用户再点启用就是双实例（sglang 双负载致整机崩溃
                    // 已实锤）。身份核验确保不把外来服务误认为自己的实例。
                    if !rec.running
                        && rec.status == "deployed"
                        && openai_endpoint_alive_cached(&rec.card_id, endpoint)
                    {
                        let card_dir = config::data_dir()
                            .join("cards")
                            .join(config::safe_path_component(&rec.card_id).unwrap_or_default());
                        if !card_dir.as_os_str().is_empty()
                            && native_engine_identity_alive(&card_dir)
                        {
                            let _ = db::set_running(&rec.card_id, true);
                            corrected.push(rec.card_name.clone());
                        }
                    }
                    continue;
                }
            }

            // 无 container_name、端点也不适用（非 OpenAI 路径或根本没报端点）的
            // docker 卡：按上面的 owned_running 认领，双向都纠正。项目名/容器名
            // == card_id 是我们自己的部署约定，同事用同款镜像跑**别的**模型不会
            // 撞上这两个名——镜像比对那条路当初不敢反向认领的理由在这里不成立。
            // docker 查不动时不认领也不清除（状态未知，保留原值）。
            if docker_rows.is_some() {
                if let Some(alive) = owned_running.get(&rec.card_id) {
                    if *alive && !rec.running && rec.status == "deployed" {
                        let _ = db::set_running(&rec.card_id, true);
                        corrected.push(rec.card_name.clone());
                    } else if !*alive && rec.running {
                        let _ = db::set_running(&rec.card_id, false);
                        corrected.push(rec.card_name);
                    }
                    continue;
                }
            }

            // 没有容器名（这一版之前启用的老记录）：退回按镜像判断，但**只做
            // 「说在跑其实没跑」这一个方向**。反向认领在这里是危险的：别人用
            // 同款镜像起的容器会让我们把自己的卡误标成运行中——真机上就撞见过
            // 同事用同一个 vllm 镜像跑了另一个模型。宁可让用户自己点一下启用。
            if docker_rows.is_none() {
                continue;
            }
            let Some(image) = image_of.get(&rec.card_id).cloned().flatten() else {
                continue;
            };
            if rec.running && !live_images.contains(&image) {
                let _ = db::set_running(&rec.card_id, false);
                corrected.push(rec.card_name);
            }
        }
        corrected
    }

    /// **目前零调用点**：存储用量那一块界面还没接过来（设置页只露出了配额
    /// 设置本身）。留待后续分期接线。
    pub fn disk_summary(&self) -> DiskSummary {
        let managed_total = db::total_managed_bytes().unwrap_or(0);
        let free = disk::disk_free_bytes(&self.settings.staging_path()).unwrap_or(0);
        let quota_bytes = self
            .settings
            .storage_quota_gb
            .map(|q| (q * 1024.0 * 1024.0 * 1024.0) as u64);
        DiskSummary {
            managed_total_bytes: managed_total,
            free_bytes: free,
            quota_bytes,
            stale_cache_bytes: self.scan_download_cache(false),
        }
    }

    /// 扫描（`delete=false`）或清理（`delete=true`）模型目录下残留的下载临时块，
    /// 返回字节数。
    ///
    /// 只进 `<模型存放目录>/<任意子目录>/.cache/huggingface/download/` 这一层找
    /// `*.incomplete`，不碰同目录下的 `.metadata`（huggingface_hub 靠它判断本地
    /// 文件跟远端是否一致，删了会导致下次重新校验甚至重下），也不碰成品文件。
    ///
    /// 注意是遍历磁盘上的子目录，**不是**按数据库里的 model_name 去拼路径：
    /// 权重目录的名字由卡片脚本决定（一般是 HF 仓库名，例如
    /// `Ornith-1.5-35B-A3B`），跟 LMM 自己生成的 model_name
    /// （`ornith-1.5-35b-a3b-bf16`）根本不是一回事。按后者拼路径永远找不到东西
    /// ——第一版就是这么写的，界面上那个「清理下载缓存」按钮因此从来没出现过。
    fn scan_download_cache(&self, delete: bool) -> u64 {
        let root = self.settings.models_path();
        let Ok(entries) = std::fs::read_dir(&root) else {
            return 0;
        };
        let mut total = 0u64;
        for entry in entries.flatten() {
            let cache = entry
                .path()
                .join(".cache")
                .join("huggingface")
                .join("download");
            if cache.is_dir() {
                total += collect_incomplete(&cache, delete);
            }
        }
        total
    }

    /// 清理残留的下载临时块，返回释放的字节数。
    ///
    /// **目前零调用点**：界面上那个「清理下载缓存」按钮跟 `disk_summary` 是
    /// 一套，还没接过来。留待后续分期接线。
    pub fn cleanup_download_cache(&self) -> u64 {
        self.scan_download_cache(true)
    }

    /// 下载前的余量预检查。
    ///
    /// **两个目录都要查，而且以更紧的那个为准。** 权重最终落在 `models_dir`，
    /// 下载过程中的临时块落在 `staging_dir`——这两个现在默认同盘，所以只查一个
    /// 也没炸过；但 `models_dir` 是设置页可以单独改到另一块盘的（「几十上百 GB
    /// 的权重，用户很可能想放到别的盘」正是那个设置存在的理由）。
    ///
    /// 只查 staging 的后果是双向的：外部盘满了却放行，下到一半 ENOSPC；
    /// 或者 staging 盘小、目标盘很空，平白把用户拦住。
    pub fn check_can_download(&self, size_bytes: u64) -> disk::SpaceCheck {
        let staging = self.settings.staging_path();
        let models = self.settings.models_path();
        let mut check = disk::check_space(&staging, size_bytes, self.settings.min_free_space_gb);
        // 判据是**路径字符串**：两个路径一模一样才省掉第二次查询，不做任何挂载点
        // 归一化——要判断两个路径是不是同一个文件系统得读 /proc/mounts 再做前缀
        // 匹配，为这点收益不值当。代价只是同盘异路径（默认就是这样：staging 是
        // models 的兄弟目录）会对同一个文件系统多查一次余量，两次拿到的是同一个
        // free_bytes，重复检查无害。
        if check.ok && models != staging {
            let target = disk::check_space(&models, size_bytes, self.settings.min_free_space_gb);
            if !target.ok {
                check = target;
            }
        }
        if !check.ok {
            return check;
        }
        if let Some(quota) = self.settings.storage_quota_gb {
            let used = db::total_managed_bytes().unwrap_or(0).max(0) as u64;
            let projected = used + size_bytes;
            let quota_bytes = (quota * 1024.0 * 1024.0 * 1024.0) as u64;
            if projected > quota_bytes {
                return disk::SpaceCheck {
                    ok: false,
                    free_bytes: check.free_bytes,
                    required_bytes: size_bytes,
                    reason: format!(
                        "超出存储配额：当前已用 {}，加上本次下载将达到 {}，配额为 {}GB",
                        disk::format_bytes(used as f64),
                        disk::format_bytes(projected as f64),
                        format_quota(quota),
                    ),
                };
            }
        }
        check
    }

    fn env_for_card(&self, card: &StoreCardDetail, model_name: &str) -> HashMap<String, String> {
        let mut env = self.common_env();
        env.insert("LMM_CARD_ID".into(), card.summary.id.clone());
        env.insert("LMM_MODEL_NAME".into(), model_name.to_string());
        env.insert("LMM_ENGINE_NAME".into(), card.summary.engine_name.clone());
        env.insert(
            "LMM_ENGINE_VERSION".into(),
            card.summary.engine_version.clone().unwrap_or_default(),
        );
        env.insert(
            "LMM_SOURCE_REPO_ID".into(),
            card.source_repo_id.clone().unwrap_or_default(),
        );
        env.insert(
            "LMM_SOURCE_FILENAME".into(),
            card.source_filename.clone().unwrap_or_default(),
        );
        // 模型总体积（卡片元数据里的真实值），下载脚本用来算下载进度百分比
        // （磁盘上已有字节数 / 这个总数），不是本地临时估算出来的。
        env.insert(
            "LMM_MODEL_SIZE_BYTES".into(),
            card.summary.size_bytes.to_string(),
        );
        env
    }

    fn env_for_record(&self, rec: &db::StoreModelRecord) -> HashMap<String, String> {
        let mut env = self.common_env();
        env.insert("LMM_CARD_ID".into(), rec.card_id.clone());
        env.insert("LMM_MODEL_NAME".into(), rec.model_name.clone());
        env.insert("LMM_ENGINE_NAME".into(), rec.engine_name.clone());
        env.insert(
            "LMM_ENGINE_VERSION".into(),
            rec.engine_version.clone().unwrap_or_default(),
        );
        env.insert(
            "LMM_SOURCE_REPO_ID".into(),
            rec.source_repo_id.clone().unwrap_or_default(),
        );
        env.insert(
            "LMM_SOURCE_FILENAME".into(),
            rec.source_filename.clone().unwrap_or_default(),
        );
        // 双机卡片（主机 + 从机）要靠 `$LMM_WORKER` 这个命令行工具去操作从机。
        //
        // **一期没有搬从机控制**（LMM 的 `core/worker.rs`，941 行，排在三期）。这里
        // 刻意不注入 `LMM_WORKER`：注入一个指向不存在的程序的路径，脚本只会拿到一句
        // "No such file or directory"，那句话不会告诉任何人发生了什么。不注入的话，
        // 脚本会在真正要动从机那一步以它自己的话失败。
        if rec.deploy_topology == DeployTopology::Dual {
            env.insert(
                "LMM_DUAL_UNSUPPORTED".into(),
                "双机部署尚未在这个版本里实现".into(),
            );
        }
        env
    }

    fn common_env(&self) -> HashMap<String, String> {
        let mut env = HashMap::new();
        env.insert("LMM_HF_ENDPOINT".into(), self.settings.hf_endpoint.clone());
        // 关掉 Xet 传输，强制走经 HF_ENDPOINT 的经典下载路径。
        //
        // huggingface_hub 1.x 只要装了 hf_xet 就默认改走 Xet（内容寻址存储），
        // 而 `HF_ENDPOINT` **只重定向 API 和 resolve 接口，不覆盖 Xet 的
        // cas-server.xethub.hf.co / transfer.xethub.hf.co**——那是两个独立 CDN。
        // 结果就是：镜像站明明通的，下载却在中途断在一个没人镜像的域名上。
        //
        // 2026-09-17 真机实测（GB10，访客 WiFi 出口）：
        //     hf-mirror.com              200，连接 0.09s
        //     huggingface.co             连不上（000）
        //     transfer.xethub.hf.co      403，连接 5.1s
        // MiniMax-H3 的权重就是这么失败的，报错是
        // `error sending request for url (https://cas-server.xethub.hf.co/v2/reconstructions/…)`,
        // 而那一行里既没有"镜像"也没有"Xet"，光看报错很难联想到根因。
        //
        // 这是**部署环境的事实**（这些设备到不了 Xet），不是某一张卡的事，
        // 所以在这里对所有卡片统一注入，而不是让每个卡片作者各写一遍。
        // 代价：放弃 Xet 的分块去重加速。在够不到 Xet 的网络里，这个代价是零。
        //
        // 这里刻意**不加 `LMM_` 前缀**，跟别的变量不一样：`LMM_*` 那套是我们和卡片
        // 作者之间的契约，要卡片自己翻译成工具认的名字；而这一条是直接喂给
        // huggingface_hub 的开关，用原名才能对**所有**卡片生效——包括厂商自带的、
        // 我们不改也改不动的那些下载脚本。
        env.insert("HF_HUB_DISABLE_XET".into(), "1".into());
        // 🔴 把 token 按**确切值**登记给落盘脱敏。
        //
        // 这一条注给**所有卡片的所有操作**，而卡片来自网络、可能被投毒：脚本里一句
        // `set -x` 就会把它原样打进输出。输出在落盘之前是看过就没了，落盘之后同一句
        // `set -x` 就是一个持久的凭证泄漏。按前缀（`hf_`）匹配只是当前约定、不是
        // 规范；而这里恰好知道确切值，按确切值匹配是能给出的最强保证。
        let hf_token = config::get_hf_token().unwrap_or_default();
        super::run_log_store::register_secret(&hf_token);
        env.insert("LMM_HF_TOKEN".into(), hf_token);
        env.insert(
            "LMM_STAGING_DIR".into(),
            self.settings.staging_path().to_string_lossy().to_string(),
        );
        env.insert(
            "LMM_MODELS_DIR".into(),
            self.settings.models_path().to_string_lossy().to_string(),
        );
        env.insert("LMM_OLLAMA_HOST".into(), self.settings.ollama_host.clone());
        // 「去哪找已经在本机的权重/交付包」，一行一个目录。
        //
        // 这一条是给**所有**脚本的，不只是下载：minimax 的 install_engine 找交付包
        // 用的也是同一批位置。在这之前，每张卡都各自硬编码了一份根目录列表——
        // ornith15 找 `~/LMM/model`，minimax 找 `$HOME`，彼此不一样，而且**没有
        // 一张卡包含 `~/models` 或用户配置的权重目录**。2026-09-20 就是这么漏掉
        // 用户已经导进来的 124GB 的：他放在 `~/models`，卡片一个都不看那儿。
        //
        // 卡片侧读法：路径里带空格和中文是常事（`/media/user/我的 移动硬盘`），
        // 所以按行切，不能用 `for dir in $LMM_IMPORT_DIRS` 那种靠 IFS 分词的写法。
        //     echo "${LMM_IMPORT_DIRS:-}" | while IFS= read -r dir; do ... done
        env.insert(
            "LMM_IMPORT_DIRS".into(),
            import_scan::encode_dirs(&import_scan::roots(
                &self.settings.import_scan_dirs,
                &self.settings.models_path(),
                &self.settings.staging_path(),
            )),
        );
        env
    }

    /// 按卡片下载：装引擎、下载模型。
    ///
    /// 注意：这一步只负责"装引擎 + 下载模型"，不做部署——部署是模型库里的
    /// 独立操作。
    ///
    /// install_engine / download_model 这两个脚本本身就要求是幂等、会自己检查
    /// "是不是已经有了"的——本地数据库里的"是否已安装/已下载"记录只用来给界面
    /// 提示用词参考，不能拿来决定"要不要跳过整个脚本不跑"：本地记录说"已安装"，
    /// 不代表用户没有在应用外面手动把引擎、模型删掉。
    /// `wait_for_slot` 是「下模型权重」前的闸门，用于排队限流：并发已满时在这里
    /// 阻塞等待，轮到了返回 Ok，用户在排队期间取消则返回 Err。
    ///
    /// 闸门的位置是这个设计的关键——它在装引擎**之后**。装引擎要 pkexec 弹管理员
    /// 密码框，必须趁用户还在电脑前做掉；等真排到队时用户可能早走开了，那时候弹
    /// 密码框只会干等到超时。下权重不需要 root，正好适合排队慢慢跑。
    pub fn download_from_store(
        &self,
        card_id: &str,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
        mut wait_for_slot: Option<&mut dyn FnMut() -> Result<(), ModelManagerError>>,
    ) -> Result<DownloadResult, ModelManagerError> {
        let card = self.store.get_card(card_id)?;
        let model_name = suggest_model_name(&card);

        let check = self.check_can_download(card.summary.size_bytes);
        if !check.ok {
            return Err(ModelManagerError::new(check.reason));
        }

        let env = self.env_for_card(&card, &model_name);

        // 仅供最后 toast 提示措辞参考，不用来决定要不要跑脚本
        let engine_reused = db::is_engine_installed(
            &card.summary.engine_name,
            card.summary.engine_version.as_deref(),
        )
        .unwrap_or(false);
        let model_reused = db::get_store_model(&card.summary.id)
            .ok()
            .flatten()
            .is_some();
        let existing_status = db::get_store_model(&card.summary.id)
            .ok()
            .flatten()
            .map(|r| r.status);

        eprintln!("[download] 开始：card={card_id} model={model_name}");

        // 一上来就把这张卡的六类脚本（下载/安装引擎/部署/启用/停用/卸载）整套
        // 抓到本地，之后整个下载流程全部跑本地副本，中途不再连商店。
        //
        // 以前是「用到哪个脚本才去商店取哪个」：同一次下载里 install_engine 光
        // 探针加正式执行就要取两趟，download_model 每重试一次再取一趟，末尾固化
        // 副本又取六趟。商店那边任何一次抖动，都会把已经跑到一半的下载整个打断。
        // 真机上就这么炸过——引擎探测完、正准备装 diffusers 时取脚本失败，报
        // 「下载脚本失败：error decoding response body」，前面下的 20GB 白跑。
        on_progress("获取该模型的部署脚本…");
        let (source, pinned_pkg) = match self.fetch_and_store_scripts(&card) {
            Ok(pkg) => (
                ScriptsSource::Local {
                    blobs: pkg.scripts.clone(),
                    assets: pkg.assets.clone(),
                    profiles: card.profiles.clone(),
                },
                Some(pkg),
            ),
            Err(e) => {
                // 整套取不下来就退回老的「按需取」路径。不比改动前更差，至少让
                // 老后台和弱网环境还有机会跑下去。
                eprintln!("[download] 整套脚本抓取失败，退回按需获取：{e}");
                on_progress(
                    "提示：未能一次性取到完整脚本，改为按需获取（商店不稳定时下载可能中断）",
                );
                (
                    ScriptsSource::from_card(&card, self.local_script_paths),
                    None,
                )
            }
        };

        on_progress(&format!(
            "检查 {} 引擎是否已安装…",
            card.summary.engine_name
        ));
        // install_engine 需要 root（pkexec，每次都要输管理员密码）。所以先以普通
        // 用户身份跑一个只读探针（install_engine 读到 LMM_PROBE_ONLY=1 时只做
        // "装没装"检查、不发任何安装动作）：命中就跳过 root 步，避免每次下载都为
        // 已装好的引擎重复输密码；只有探针说"没装/脚本不支持探针"才真正走 root 安装。
        let mut docker_image: Option<String> = None;
        let probe = self.engine_probe(&source, card_id, &env);
        let probe_ok = probe.installed;

        // 光看"引擎装没装"不够。install_engine 除了装引擎，还负责把整个部署包
        // （compose.yaml、scripts/*.sh 等）落到 vendor 目录——用户后续部署/启用
        // 跑的其实是那些落地的脚本。如果只按"引擎在不在"决定跳不跳过，商店那边
        // 修好的脚本就永远送不到已经装过引擎的设备上（真机踩到过：改了下载并发
        // 参数，设备上的部署包脚本还是老版本）。所以再比一次 install_engine 本身
        // 的哈希：跟上次成功执行的那份不一样，就算引擎在也要重跑一遍，把新的
        // 部署包刷下去。
        let current_hash = source.script_hash("install_engine");
        let recorded_hash = db::get_installed_engine(
            &card.summary.engine_name,
            card.summary.engine_version.as_deref(),
        )
        .ok()
        .flatten()
        .and_then(|r| r.install_script_sha256);
        let script_changed = match (&current_hash, &recorded_hash) {
            (Some(cur), Some(old)) => cur != old,
            // 记录里没有哈希（老版本装的）就不强制重跑，避免所有老用户平白
            // 多一次提权安装；等他们下次真正需要装引擎时自然会补上。
            _ => false,
        };
        eprintln!(
            "[download] 引擎探针：已安装={probe_ok}，需要提权={}，install_engine 脚本有更新={script_changed}",
            probe.need_root
        );
        if script_changed && probe_ok {
            on_progress("检测到引擎/部署包脚本有更新，需要重新执行一次安装以刷新部署包…");
        }

        // 闸门位置取决于装引擎要不要用户在场：
        // - 不需要提权（绝大多数情况）：把装引擎也放进队列一起排。全程零密码框，
        //   用户点完就能走开，轮到它时自动装引擎、自动下权重。
        // - 需要提权（设备第一次配 NVIDIA Container Toolkit 这类一次性环境准备）：
        //   密码框必须趁用户还在电脑前弹，所以先装完引擎再去排队等下载名额。
        if !probe.need_root {
            if let Some(gate) = wait_for_slot.take() {
                gate()?;
            }
        }

        if !probe_ok || script_changed {
            if probe.need_root {
                // 会弹系统的管理员授权框（pkexec）。先把话说在前面，否则用户
                // 看到密码框会不知道是哪来的、在等什么。
                on_progress(&format!(
                    "首次配置 {} 运行环境需要管理员权限，请在弹出的授权窗口中输入密码…",
                    card.summary.engine_name
                ));
            } else {
                on_progress(&format!("正在安装 {} 引擎…", card.summary.engine_name));
            }
            let install_lines = self.install_engine(
                &source,
                &env,
                &mut |line| on_progress(&script_runner::prefix_log_line("[安装引擎] ", line)),
                control.clone(),
                probe.need_root,
            )?;
            // install_engine 可以打印 LMM_ENGINE_IMAGE= 这一行告诉 LMM 这个引擎
            // 实际用的 docker 镜像是哪个（不是所有引擎都是 docker 镜像，比如
            // ollama 就没有），供"已安装引擎"的删除功能精确 docker rmi。
            // stdout 是卡片作者可控的输入，先过形式白名单、且不覆盖共享行里
            // 已登记的不同镜像名（r19 评审 P2-1，判据见 engine_image_to_record）。
            docker_image = engine_image_to_record(
                &card.summary.engine_name,
                card.summary.engine_version.as_deref(),
                extract_marker(&install_lines, "LMM_ENGINE_IMAGE="),
            );
        }
        // 独立清理脚本不是下载前提：老卡片没有它、或弱网下一次补取失败，都不能
        // 让已经验证好的模型权重下载功亏一篑。缺失时只是不开放“删除原生引擎”。
        let cleanup_script = match source.optional_script_blob("uninstall_engine") {
            Ok(script) => script,
            Err(error) => {
                eprintln!(
                    "[download] 未能固化原生引擎清理脚本（{}）：{error}",
                    card.summary.id
                );
                None
            }
        };
        db::mark_engine_installed(
            &card.summary.engine_name,
            card.summary.engine_version.as_deref(),
            docker_image.as_deref(),
            // 记下这次跑的 install_engine 是哪个版本；下次比对哈希决定要不要
            // 为了刷新部署包再跑一遍
            current_hash.as_deref(),
            cleanup_script.as_ref(),
        )
        .map_err(|e| ModelManagerError::new(format!("记录引擎安装状态失败：{e}")))?;

        // 需要提权的那条路径：管理员授权已经做完了，现在才去排队等下载名额。
        // （不需要提权时闸门在上面就已经过了，这里的 take() 拿不到东西。）
        if let Some(gate) = wait_for_slot.take() {
            gate()?;
        }

        eprintln!("[download] 引擎就绪，开始下载模型权重");

        // 下载前先看看本机已经有什么权重了。
        //
        // 结果有两个去处。一是**写进运行日志**：用户点下载时最想知道的就是"我
        // 导进来的那份到底算不算数"，在这之前他无从得知——2026-09-20 那次，
        // 124GB 明明在 `~/models` 躺着，界面上只有一条一直在涨的下载进度。
        // 二是通过 `LMM_IMPORT_CANDIDATES` 交给下载脚本：哪一个是这个模型、
        // 全不全，只有脚本自己答得了（见 import_scan 模块文档）。
        //
        // 扫描本身有时间预算，扫不完、扫不到都不影响下载——最坏就是照常走网络。
        let candidates = import_scan::scan(&import_scan::roots(
            &self.settings.import_scan_dirs,
            &self.settings.models_path(),
            &self.settings.staging_path(),
        ));
        let mut env = env;
        if candidates.is_empty() {
            on_progress("本机未找到已有的权重目录，将从网络下载");
        } else {
            // 措辞**两个方向的绝对断言都不能下**：既不能说「会先尝试复用」，
            // 也不能说「暂不自动复用，仍会联网下载」。
            //
            // 宿主这一层的职责只有三件：扫描、把结果通过 LMM_IMPORT_DIRS /
            // LMM_IMPORT_CANDIDATES 交给卡片脚本、把找到了什么写进日志。
            // **复用与否完全取决于卡片脚本自己**，而卡片脚本不归这个仓库管：
            // 2026-09-21 起在架的 7 张全部消费 LMM_IMPORT_DIRS，拿不到才回落到
            // 脚本自带的硬编码清单——但这是**当时在架那一批**的情况，不是保证。
            //
            // 这里原来写的是"当前没有任何一张卡这么做"——那句话是**在本仓库里
            // grep 之后得出的**，而这正是它错的原因：正式卡片的脚本源在商店
            // 服务器的 seed 目录里，不随任何一个 git 仓库走，仓库内 grep 必然
            // 零命中。下一个人照样 grep 一遍还会得出同一个错误结论，所以这件事
            // 值得写在这儿。要核实卡片到底消费不消费，只能去看商店服务器上的
            // 脚本，或者从设备上按 `scripts/verify_store_card.py` 的路子拉一份下来。
            on_progress(&format!(
                "本机已有 {} 处权重目录，已交给卡片脚本（是否复用由脚本自己决定）：",
                candidates.len()
            ));
            // 只列前几条。扫到十几个目录是常见的（`~/models` 下每个模型一个），
            // 全列出来会把真正的下载日志挤出可视范围。
            for c in candidates.iter().take(8) {
                on_progress(&format!(
                    "  {}（{}{}）",
                    c.path.display(),
                    disk::format_bytes(c.bytes as f64),
                    if c.removable {
                        "，可移动介质"
                    } else {
                        ""
                    }
                ));
            }
            if candidates.len() > 8 {
                on_progress(&format!("  …另有 {} 处", candidates.len() - 8));
            }
        }
        env.insert(
            "LMM_IMPORT_CANDIDATES".into(),
            import_scan::encode_dirs(
                &candidates
                    .iter()
                    .map(|c| c.path.clone())
                    .collect::<Vec<_>>(),
            ),
        );

        on_progress("检查模型权重…");
        self.run_card_script_with_retry(
            &source,
            "download_model",
            &env,
            &mut |line| on_progress(&script_runner::prefix_log_line("[下载模型] ", line)),
            control,
            false,
        )?;

        // 重新下载时只保留真实的“已部署”状态；“权重不完整”必须在本次成功后
        // 恢复成已下载。若原样保留 incomplete，用户即使补齐了文件，界面仍只能
        // 看见“继续下载”，又会造出另一种假状态。
        let status = match existing_status.as_deref() {
            Some("deployed") => "deployed".to_string(),
            _ => "downloaded".to_string(),
        };
        db::add_store_model(db::AddStoreModelParams {
            card_id: &card.summary.id,
            model_name: &model_name,
            card_name: &card.summary.name,
            org: card.summary.org.as_deref(),
            description: card.summary.description.as_deref(),
            engine_name: &card.summary.engine_name,
            engine_version: card.summary.engine_version.as_deref(),
            size_bytes: card.summary.size_bytes as i64,
            quantization: card.summary.quantization.as_deref(),
            source_repo_id: card.source_repo_id.as_deref(),
            source_filename: card.source_filename.as_deref(),
            scripts: &card.scripts,
            status: &status,
            profiles: &card.profiles,
            deploy_topology: card.summary.deploy_topology,
        })
        .map_err(|e| ModelManagerError::new(format!("记录模型下载状态失败：{e}")))?;
        // 内存档位随这次成功下载的卡片一起固化；启用时不再依赖商店网络，也不会把
        // 权重文件大小误当作内存预算。空结构的语义是“当前卡片没有给可信预算”。
        // 这个档位只影响后续的启用前提醒，不能把已经完成的下载误报成失败。
        // 写入失败时保守地回退到“需要用户确认”的未知档位即可。
        if let Err(e) =
            db::set_resource_requirements(&card.summary.id, &card.summary.resource_requirements)
        {
            eprintln!("[model-store] 保存模型内存档位失败（将使用保守提示）：{e}");
        }

        // 把六类脚本的正文固化到本地：从这一刻起，这个模型的部署/启用/停用/
        // 卸载全部用本地副本执行，不再依赖商店可达——商店重启、上架、改脚本
        // 都不会影响用户手上的流程。
        //
        // 正常情况下开头已经整套抓下来了（pinned_blobs），这里直接写库，不再多
        // 跑一趟 HTTP。只有开头那次抓取失败、退回按需获取时才在这兜底补一次；
        // 补不到也不让整个下载失败（模型已经下好了），只提示一句，后续操作走
        // 按 URL 取的兼容路径。
        let pkg = match pinned_pkg {
            Some(pkg) => Some(pkg),
            None => match self.fetch_and_store_scripts(&card) {
                Ok(pkg) => Some(pkg),
                Err(e) => {
                    eprintln!("[download] 抓取脚本副本失败（不影响本次下载）：{e}");
                    on_progress("提示：脚本本地副本未能保存，后续操作仍需连接商店");
                    None
                }
            },
        };
        if let Some(pkg) = pkg {
            if let Err(e) = db::set_scripts_content(&card.summary.id, &pkg.scripts, &pkg.assets) {
                eprintln!("[download] 固化脚本副本失败（不影响本次下载）：{e}");
            }
        }

        Ok(DownloadResult {
            model_name,
            engine_reused,
            model_reused,
        })
    }

    /// 以普通用户身份只读探测（绝不弹密码框）。回答两个问题：引擎装了没有、
    /// 这次安装需不需要管理员权限。
    ///
    /// `need_root` 靠脚本打印约定行 `LMM_NEED_ROOT=1` 来表达。绝大多数情况下
    /// 装引擎根本不需要 root——docker pull 靠 docker 组权限就行，部署包写的是
    /// 用户自己的家目录；只有「NVIDIA Container Toolkit 还没注册成 docker
    /// runtime」这类要改系统配置的一次性环境准备才真的需要。老脚本不打印这一
    /// 行，探针就报 need_root=false，安装会先以普通用户身份尝试，失败了再自动
    /// 提权重跑一次（见 install_engine），所以老卡片不会因此坏掉。
    ///
    /// 输出用 on_progress 收集而不是取返回值：引擎没装时探针以非 0 退出，
    /// run_script 返回的是 Err，那样就拿不到 LMM_NEED_ROOT 这行了。
    fn engine_probe(
        &self,
        source: &ScriptsSource,
        card_id: &str,
        env: &HashMap<String, String>,
    ) -> EngineProbe {
        let Ok(prepared) = source.prepare(card_id, "install_engine") else {
            return EngineProbe::default();
        };
        let mut probe_env = env.clone();
        probe_env.insert("LMM_PROBE_ONLY".into(), "1".into());
        let mut lines: Vec<String> = Vec::new();
        let mut collect = |p: ScriptProgress| {
            if !p.line.is_empty() {
                lines.push(p.line);
            }
        };
        let installed = script_runner::run_script(
            &prepared.entry,
            &probe_env,
            RunScriptOptions {
                on_progress: Some(&mut collect),
                timeout: Some(ENGINE_PROBE_TIMEOUT),
                idle_timeout: Some(ENGINE_PROBE_IDLE_TIMEOUT),
                cwd: prepared.cwd.clone(),
                ..Default::default()
            },
        )
        .is_ok();
        EngineProbe {
            installed,
            need_root: lines.iter().any(|l| l.trim() == "LMM_NEED_ROOT=1"),
        }
    }

    /// 装引擎。默认以**普通用户**身份跑，不打扰用户。
    ///
    /// `need_root=true`（探针明确报了要提权）时直接走 pkexec。否则先按普通用户
    /// 跑；万一失败——比如某张老卡片的脚本确实需要 root 却没实现探针约定——再
    /// 自动提权重跑一次。install_engine 本来就要求幂等（用户可能在应用外手动
    /// 删过东西），重跑一遍是安全的。
    ///
    /// **兜底提权只对"内容可信"的脚本开放。** 这条兜底本身是合理的产品设计，
    /// 但它是整条 root 链的最后一段：脚本没验签也能借这条路拿到 root。所以
    /// 两道一起上——`script_runner::FetchPolicy` 让这种内容根本取不回来
    /// （install_engine 强制要求 sha256、强制 https），而 `is_fatal_failure`
    /// 又把"来源不可信/完整性校验失败"全部列为致命，校验不过绝不自动提权。
    fn install_engine(
        &self,
        source: &ScriptsSource,
        env: &HashMap<String, String>,
        on_line: &mut dyn FnMut(&str),
        control: script_runner::RunControl,
        need_root: bool,
    ) -> Result<Vec<String>, ModelManagerError> {
        // 提权这条路**只跑一次，不进重试循环**。
        //
        // pkexec 的授权是 ONE-SHOT：重试 N 次 = 弹 N 个系统密码框。而桌面的 polkit
        // 代理一次只能持有一个 modal grab，连发会把它打到开始自动 dismiss，之后每次
        // 都秒失败、再触发下一次重试——用户看到的是「密码永远是错的」。
        // 详见 is_fatal_failure 上面那段复盘。
        //
        // 代价是提权路径失去了断点续传的自动重试。可以接受：install_engine 拉的是
        // 容器镜像，docker 自己就有分层重试和断点续传；真正需要 30 次续传的是
        // download_model 那条几百 GB 的路，它不提权。
        //
        // 「拉的是容器镜像」这个假设在 2026-10-29 被 cube-5059 打破：驭光
        // （tachyon-serve）的 install_engine 是 curl 拉 230MB 的 engine.tar.gz。
        // 设备换 IP 后出网走客户自装的代理（Clash TUN）路径劣化，普通用户身份
        // curl 92（HTTP/2 流被 CANCEL）→ 落进兜底提权整段重跑 → root 身份再
        // curl 18（partial file）→ 终态失败，全程没有一次续传。所以**普通用户
        // 身份**这条路上的传输中断改为自动断点续传（见下面那个分支）；提权那条
        // 仍单次——pkexec 授权是一次性的，续传循环弹 N 个密码框比失败本身更糟。
        if need_root {
            return self.run_card_script(source, "install_engine", env, on_line, control, true);
        }
        match self.run_card_script(
            source,
            "install_engine",
            env,
            on_line,
            control.clone(),
            false,
        ) {
            Ok(lines) => Ok(lines),
            // 用户点了取消、磁盘满这类错误，提权重跑一遍也是一样的结果，
            // 别拿一个多余的密码框去骚扰用户。
            Err(e) if Self::is_fatal_failure(&e.to_string()) => Err(e),
            // 网络类失败同样不提权（.48 真机案例，见 network_unreachable_failure
            // 的注释）——密码框解决不了路由不通，只会把明确失败拖成授权挂死。
            // 这里把"为什么不再重试/用户该做什么"说在错误里，而不是让用户
            // 对着一个没出现的弹窗等半小时。
            Err(e) if Self::network_unreachable_failure(&e.to_string()) => {
                on_line("失败原因是网络不可达，管理员权限解决不了，不再弹授权窗口。");
                Err(ModelManagerError::new(format!(
                    "{e}\n提示：设备当前无法访问外网（或镜像源不可达）。请把设备接入可出网的网络后重试；\
                     或在有外网的机器上 docker save 导出引擎镜像，用 U 盘拷到本机 docker load 导入后再试。"
                )))
            }
            // 传输中断（curl 18/92/连接被重置）：路通着但断流，断点就在磁盘上。
            // 既不是权限问题（提权解决不了，别弹框），也不该终止（重跑即续传），
            // 按 download_model 同款策略自动续传。见函数头 2026-10-29 复盘。
            Err(e) if Self::transfer_interrupted_failure(&e.to_string()) => {
                on_line("失败原因是传输中断（不是权限问题），不弹授权窗口，自动从断点续传…");
                self.install_engine_resume_after_interrupt(source, env, on_line, control, e)
            }
            Err(e) => {
                eprintln!("[download] 普通用户身份安装引擎失败，改用管理员权限重试：{e}");
                on_line(
                    "以当前用户身份安装未成功，改用管理员权限重试，请在弹出的授权窗口中输入密码…",
                );
                // 同样只跑一次：这是**兜底提权**，不是重试循环。理由同上面那段。
                self.run_card_script(source, "install_engine", env, on_line, control, true)
            }
        }
    }

    /// install_engine 专用的断点续传循环：只在错误**仍是传输中断**时接着跑。
    ///
    /// 为什么不直接复用 [`Self::run_card_script_with_retry`]：那个循环对一切
    /// 非致命错误都续传，而这里非传输类失败（权限、磁盘、脚本自身 bug）要立刻
    /// 上抛，让下一次用户操作重新走一遍分类——该提权提权、该报错报错，不能拿
    /// 传输中断的耐心硬扛。循环骨架（上限、退避、取消响应、每轮日志）与那边
    /// 一致，改动时两边同步。
    fn install_engine_resume_after_interrupt(
        &self,
        source: &ScriptsSource,
        env: &HashMap<String, String>,
        on_line: &mut dyn FnMut(&str),
        control: script_runner::RunControl,
        first_error: ModelManagerError,
    ) -> Result<Vec<String>, ModelManagerError> {
        let mut attempt = 0u32;
        let mut error = first_error;
        loop {
            // 致命错误与用户取消按原样上抛，一次都不多跑。首个错误在进循环前
            // 已被调用方分过类（致命/网络不可达都到不了这里），这道检查防的是
            // 续传中途换出的新致命错误。
            if Self::is_fatal_failure(&error.to_string()) || control.cancelled() {
                return Err(error);
            }
            attempt += 1;
            if attempt > DOWNLOAD_RESUME_LIMIT {
                return Err(ModelManagerError::new(format!(
                    "{error}\n\n已自动续传 {DOWNLOAD_RESUME_LIMIT} 次仍未完成。\
                     已下载的部分都在本地，重新点击「下载」会从断点继续，不会重头开始。"
                )));
            }
            on_line(&format!(
                "传输中断（{}），第 {attempt}/{DOWNLOAD_RESUME_LIMIT} 次从断点续传…",
                error.to_string().lines().next().unwrap_or("原因未知")
            ));
            // 指数退避封顶 16 秒，取消信号分片可中断，同 run_card_script_with_retry
            let backoff = Duration::from_secs(2u64.saturating_pow(attempt.min(4)));
            let deadline = Instant::now() + backoff;
            while Instant::now() < deadline {
                if control.cancelled() {
                    return Err(error);
                }
                std::thread::sleep(Duration::from_millis(200));
            }
            match self.run_card_script(
                source,
                "install_engine",
                env,
                on_line,
                control.clone(),
                false,
            ) {
                Ok(lines) => return Ok(lines),
                // 仍是传输中断 → 记下错误进下一轮；换了个不相干的新错误 →
                // 按原样上抛，交回上层分类处理
                Err(e) => {
                    if Self::transfer_interrupted_failure(&e.to_string()) {
                        error = e;
                    } else {
                        return Err(e);
                    }
                }
            }
        }
    }

    fn run_card_script(
        &self,
        source: &ScriptsSource,
        script_type: &str,
        env: &HashMap<String, String>,
        on_line: &mut dyn FnMut(&str),
        control: script_runner::RunControl,
        need_root: bool,
    ) -> Result<Vec<String>, ModelManagerError> {
        // Local 来源直接落盘本地副本执行，全程不联网；Remote 才会下载+校验哈希。
        // 多文件卡片会把整棵文件树落到卡片目录，并以该目录为工作目录执行。
        let card_id = env
            .get("LMM_CARD_ID")
            .map(|s| s.as_str())
            .unwrap_or("unknown");
        let prepared = source.prepare(card_id, script_type)?;
        let label = script_run_label(script_type, env);
        let mut relay = |p: ScriptProgress| {
            if !p.line.is_empty() {
                on_line(&p.line);
            }
        };
        script_runner::run_script(
            &prepared.entry,
            env,
            RunScriptOptions {
                on_progress: Some(&mut relay),
                control,
                need_root,
                label: Some(label),
                cwd: prepared.cwd,
                idle_timeout: idle_timeout_for(script_type),
                ..Default::default()
            },
        )
    }

    /// 这类失败重试多少次都不会变好，遇到就立刻抛给用户，别白白耗 30 轮：
    /// 磁盘满了、模型/镜像在远端根本不存在、文件格式不对、权限不足、用户自己点的
    /// 取消，以及**所有跟管理员授权有关的失败**。
    ///
    /// 授权类为什么必须是致命的（2026-09-18 现场复盘）：pkexec 的授权是 ONE-SHOT，
    /// 重试一次就是**再弹一个系统密码框**。当时这里漏了这一类，于是 94 秒内连发了
    /// 约 16 次 pkexec；而 gnome-shell 的 polkit agent 一次只能持有一个 modal grab，
    /// 被这样轰炸之后它开始报 `Failed to show modal dialog` 并自动 dismiss 后续请求
    /// ——从那一刻起密码框根本不再弹出，每次都瞬间"认证失败"，再触发下一次重试。
    /// 用户看到的就是「一直弹密码、输对了也一直报错」。这是自我强化的，而且把桌面
    /// 的授权代理一起拖垮，重试次数越多越糟。
    ///
    /// 这份清单还承担第二个职责，而且是安全性质的：`install_engine` 在普通用户
    /// 身份失败后会**自动提权重跑一次**（见 `install_engine`），凡是不在这份
    /// 清单里的失败都会走那条路。所以"内容信不过"这一类必须在这里登记——
    /// 否则校验没过的脚本反而借兜底提权拿到了 root。纵深防御的第二道：第一道是
    /// `script_runner::FetchPolicy` 压根不让这种内容被取回来。
    pub(crate) fn is_fatal_failure(msg: &str) -> bool {
        const FATAL: &[&str] = &[
            "已取消",
            "磁盘空间不足",
            "找不到对应的模型",
            "在远端找不到该模型",
            "文件格式无效",
            "权限不足",
            "没有访问权限",
            // 内容信不过的三类。写成引用常量而不是抄一遍字面量：这份清单靠文字
            // 片段匹配，抄字面量的话 script_runner 那边改一个字，这里就会静默
            // 失效——而失效的表现恰好是"未验签内容自动拿到 root"。
            "完整性校验失败",
            script_runner::UNTRUSTED_SOURCE_PREFIX,
            script_runner::ECHO_REOPENED_ABORT,
            // 下面这几句是 errors.rs 对 pkexec 各种结局的中文化结果，必须与那边保持
            // 一致。用中文片段而不是英文原文来匹配，是因为脚本错误在到达这里之前
            // 已经过了 humanize_script_message。
            "管理员授权被取消",
            "管理员授权失败",
            "系统找不到授权代理",
        ];
        FATAL.iter().any(|k| msg.contains(k))
    }

    /// 网络类失败：**提权解决不了**，兜底 pkexec 重跑前必须拦下。
    ///
    /// 192.168.10.48 真机实锤（2026-10-27）：设备无默认路由（出不了外网），
    /// digest 钉死的引擎镜像不在本地 → docker pull 超时失败 → 这里没把网络类
    /// 失败归为"别提权"→ 弹 pkexec 重试 → 现场没人输密码 → 1800 秒无输出被
    /// idle 判死。一个明确的两分钟失败被拖成半小时挂死，且死因文案（"判定为
    /// 卡死"）与真相（在等授权）完全脱节。
    ///
    /// 注意与 [`Self::is_fatal_failure`] 的分工：网络失败**可以**走断点续传重试
    /// （网络抖一下就该重试），所以不进 FATAL 清单；它只是**不配**触发提权——
    /// 权限给得再高，路由也不通。
    pub(crate) fn network_unreachable_failure(msg: &str) -> bool {
        // 小写比对：docker/DNS 的报错在句首会大写（"Temporary failure resolving"），
        // 中文标记不受影响。
        const MARKERS: &[&str] = &[
            // docker / registry 原文（docker pull、dial 系列）
            "i/o timeout",
            "dial tcp",
            "no route to host",
            "network is unreachable",
            "failed to resolve reference",
            "connection refused",
            "context deadline exceeded",
            // DNS 原文
            "could not resolve host",
            "temporary failure resolving",
            // 客户端已 humanize 的中文形态（errors.rs / store_client.rs）。
            // 引用 store_client 的常量而不是在这里抄字面量：文案与判据分家后，
            // 那边改一个字、这边还在匹配旧文本，守卫就静默失效（r19 评审 P2-2）。
            HINT_CONNECT_TIMEOUT,
            HINT_CONNECT_FAILED,
            HINT_CHECK_INTERNET_ACCESS,
        ];
        let lowered = msg.to_lowercase();
        MARKERS.iter().any(|k| lowered.contains(k))
    }

    /// 传输中断类失败：下载**已经在传**、中途被掐断（curl 18/92、连接被重置）。
    ///
    /// 与 [`Self::network_unreachable_failure`] 的分工：那边是"路不通"（连不上、
    /// 解析不了），重试无意义，治法是让用户换网络；这边是"路通着但断流"，断点
    /// 就在磁盘上，重跑脚本即从断点续传。两类都不配触发提权。
    ///
    /// 2026-10-29 cube-5059 实锤：换 IP 后出网走客户自装的代理（Clash TUN），
    /// 路径劣化成 14KB/s，驭光引擎包（curl 拉的 230MB tarball，不是 docker
    /// 镜像）先 curl 92（HTTP/2 流被 CANCEL）、兜底提权整段重跑又 curl 18
    /// （partial file）后终态失败——三次全是传输中断，全都有断点可续。
    pub(crate) fn transfer_interrupted_failure(msg: &str) -> bool {
        // 小写比对：curl 原文的 "HTTP/2"、"Transferred" 都在句中大写。
        const MARKERS: &[&str] = &[
            // 卡片脚本 humanize 后的中文形态（驭光 install_engine 自己打印的）。
            // r21 P2-7：按「退出码 N」收而不是「curl 退出码 N」——通用包装器
            // `脚本执行失败（退出码 18）：{summary}`（script_runner 的失败路径）
            // 没有 curl 前缀，卡片未透出 curl 原文时此前必 miss。误收面只有
            // 「别的工具恰好也用 18/92 当退出码」，代价是多几次断点续传重试，
            // 不涉及安全。
            "退出码 18",
            "退出码 92",
            // curl 原文（卡片没 humanize 时直接透出）
            "transferred a partial file",
            "http/2 stream",
            // 同族中断：连接被对端重置（curl 56 / docker read reset）
            "connection reset by peer",
            // r21 P2-7：curl/docker 断流的三个原文形态，此前全部 miss、落
            // pkexec 整段重跑，正是 cube-5059 要消灭的路径——整段被掐
            // （curl 18 原文 `transfer closed with N bytes remaining`）、
            // 连接重置的无 by peer 变体（`Connection was reset`）、对端半途
            // 关流（`unexpected EOF`）。
            "transfer closed with",
            "connection was reset",
            "unexpected eof",
        ];
        let lowered = msg.to_lowercase();
        MARKERS.iter().any(|k| lowered.contains(k))
    }

    /// 跑卡片脚本；网络类失败自动从断点续传，最多 [`DOWNLOAD_RESUME_LIMIT`] 次。
    ///
    /// 为什么可以放心重跑：install_engine / download_model 都是**幂等 + 断点续传**的
    /// ——已经下好的分片留在磁盘上，重跑会先校验完整性、跳过已完成的部分，从断点
    /// 接着传，不会重头来。所以"重试"的成本只有一次进程启动，不会浪费已传的字节。
    ///
    /// 三条保证，缺一条这个自动化就会变成用户讨厌的东西：
    /// 1. **看得见**：每次续传往日志里写一行，用户知道应用在干什么，而不是对着一个
    ///    不动的进度条猜。
    /// 2. **停得下**：每次续传前先看取消信号，用户随时能中断，不用等 30 次跑完。
    /// 3. **不白转**：致命错误（磁盘满、远端没有这个模型、校验失败、用户取消）
    ///    立刻抛出，绝不在上面耗次数。
    fn run_card_script_with_retry(
        &self,
        source: &ScriptsSource,
        script_type: &str,
        env: &HashMap<String, String>,
        on_line: &mut dyn FnMut(&str),
        control: script_runner::RunControl,
        need_root: bool,
    ) -> Result<Vec<String>, ModelManagerError> {
        let mut attempt = 0u32;
        loop {
            let result = self.run_card_script(
                source,
                script_type,
                env,
                on_line,
                control.clone(),
                need_root,
            );
            let error = match result {
                Ok(lines) => return Ok(lines),
                Err(e) => e,
            };
            let msg = error.to_string();

            // 用户主动取消、以及重试也没用的致命错误，按原样报出去
            if Self::is_fatal_failure(&msg) {
                return Err(error);
            }
            // 取消信号可能在脚本被杀之后才落地，这里再看一次：用户点了取消就不该
            // 还往下续传。
            if control.cancelled() {
                return Err(error);
            }

            attempt += 1;
            if attempt > DOWNLOAD_RESUME_LIMIT {
                return Err(ModelManagerError::new(format!(
                    "{msg}\n\n已自动续传 {DOWNLOAD_RESUME_LIMIT} 次仍未完成。\
                     已下载的部分都在本地，重新点击「下载」会从断点继续，不会重头开始。"
                )));
            }
            // 这一行必须打出来。用户看到的是任务卡片上的日志，不写的话界面会安静
            // 几分钟然后突然又开始动，看上去像卡死后自己好了。
            on_line(&format!(
                "传输中断（{}），第 {attempt}/{DOWNLOAD_RESUME_LIMIT} 次从断点续传…",
                msg.lines().next().unwrap_or("原因未知")
            ));

            // 退避。原来是**一失败就立刻重跑**，脚本秒失败时就变成 1-3 秒一发、
            // 94 秒打出十几轮——既刷屏，也会把下游（远端服务、系统授权代理这类
            // 一次只服务一个请求的东西）打得更惨。
            //
            // 指数退避，实际封顶 16 秒（2^4）：真正的网络抖动几秒就恢复，封顶是为了
            // 别让一次长下载在末尾干等几分钟。取消信号在退避期间要能立刻生效，
            // 所以分片睡。
            //
            // 这里原来写的是「封顶 30 秒」并跟着一个 `.min(30)`——而指数部分已经被
            // `attempt.min(4)` 钳在 2^4=16，那个 `.min(30)` 永远不生效。注释说 30、
            // 代码给 16，两边对不上；以代码的实际行为为准，把死代码去掉。
            let backoff = Duration::from_secs(2u64.saturating_pow(attempt.min(4)));
            let deadline = Instant::now() + backoff;
            while Instant::now() < deadline {
                if control.cancelled() {
                    return Err(error);
                }
                std::thread::sleep(Duration::from_millis(200));
            }
        }
    }

    // ---------------- 部署 / 启用 / 停用 / 卸载 ----------------

    /// 日常操作（部署/启用/停用/卸载）用的脚本来源：**只用本地固化副本，绝不
    /// 联商店**。
    ///
    /// 这是刻意的解耦——用户可能随时在用应用，商店那边的优化、重启、上架新卡
    /// 片都不该打断他手上的流程；而且"每次操作偷偷去拉最新脚本"还有个隐患：
    /// 部署时用的是 A 版脚本，启用时商店恰好更新了，就变成用 B 版脚本去启用
    /// A 版部署出来的东西。脚本更新改由用户主动点「检查卡片更新」触发。
    ///
    /// 老记录（本次改动之前下载的）没有固化副本，退回按 URL 去取——这是一次性
    /// 的兼容路径，用户点一次「检查卡片更新」之后就转为完全离线。
    ///
    /// 这条兼容路径拿不到哈希（`script_sha256: None`，记录里只存了 URL）。它
    /// 服务的是部署/启用/停用/卸载，**没有一个走 root**，所以
    /// `may_run_as_root` 判定不到它头上；真要按 root 那档要求哈希，用户手上那
    /// 些老模型会连卸载都卸不掉，几百 GB 从任何界面都回收不了。
    fn local_scripts_source(&self, rec: &db::StoreModelRecord) -> ScriptsSource {
        if !rec.scripts_content.is_empty() {
            return ScriptsSource::Local {
                blobs: rec.scripts_content.clone(),
                assets: rec.assets_content.clone(),
                profiles: rec.profiles.clone(),
            };
        }
        ScriptsSource::Remote {
            scripts: rec
                .scripts
                .iter()
                .filter_map(|(k, v)| v.clone().map(|url| (k.clone(), url)))
                .collect(),
            script_sha256: None,
            assets: BTreeMap::new(),
            asset_sha256: None,
            profiles: rec.profiles.clone(),
            local_paths: self.local_script_paths,
        }
    }

    /// 抓取卡片六类脚本的正文并固化到本地。只在两个时刻调用：下载模型成功后、
    /// 用户主动点「检查卡片更新」时。抓取过程中任何一个脚本失败都整体放弃，
    /// 不留下半套新半套旧的副本。
    fn fetch_and_store_scripts(
        &self,
        card: &StoreCardDetail,
    ) -> Result<CardPackage, ModelManagerError> {
        // 来源校验必须在**抓取的这一刻**做：抓完就固化成 Local 副本，之后
        // install_engine 直接跑副本、不再有任何 fetch 可校验。副本里的 sha256
        // 是按内容自算的，证明不了"内容是对的"，只能证明"内容没再变过"。
        let local_paths = self.local_script_paths;
        let fetch = |url: &str,
                     expected: Option<&str>,
                     root_capable: bool|
         -> Result<db::ScriptBlob, ModelManagerError> {
            let policy = script_runner::FetchPolicy {
                may_run_as_root: root_capable,
                allow_local_path: local_paths,
            };
            let content = script_runner::fetch_script_content(url, expected, policy)?;
            let sha256 = script_runner::sha256_hex(&content);
            Ok(db::ScriptBlob { content, sha256 })
        };

        let mut scripts = BTreeMap::new();
        for (script_type, url) in &card.scripts {
            let expected = card
                .script_sha256
                .as_ref()
                .and_then(|h| h.get(script_type))
                .map(|s| s.as_str());
            scripts.insert(
                script_type.clone(),
                fetch(url, expected, may_run_as_root(script_type))?,
            );
        }

        // 附属文件跟入口脚本一起抓：只抓到一半的包既不是旧版也不是新版，
        // 跑出来的结果没人能解释，所以任何一个失败就整体放弃。
        //
        // 附属文件按"会以 root 执行"那一档要求哈希：整包卡片的文件树跟
        // install_engine 落在同一个工作目录，它以 root 跑起来之后调的就是这些
        // 文件（controller.py、compose.yaml…）。只对入口脚本验签，等于没验。
        let assets_may_run_as_root = card.scripts.keys().any(|t| may_run_as_root(t));
        let mut assets = BTreeMap::new();
        for (path, url) in &card.assets {
            let expected = card
                .asset_sha256
                .as_ref()
                .and_then(|h| h.get(path))
                .map(|s| s.as_str());
            assets.insert(path.clone(), fetch(url, expected, assets_may_run_as_root)?);
        }
        Ok(CardPackage { scripts, assets })
    }

    /// 跟商店同步一次目录状态：哪些卡片是新上架的、哪些已下架、以及**已下载
    /// 的模型有没有更新的脚本**。
    ///
    /// Catalog-wide update notifications are not wired yet. Explicit updates
    /// are available in model details through `change_card_scripts`; merely
    /// opening the store never replaces a cached script package.
    ///
    /// 设计意图保持不变：只读，不改任何本地副本——检测到有更新只是提醒用户，
    /// 换不换由用户点「检查卡片更新」决定，免得他正用着的部署脚本在背后被悄悄
    pub fn change_card_scripts(
        &self,
        card_id: &str,
        control: &script_runner::RunControl,
    ) -> Result<bool, ModelManagerError> {
        let rec = self.get_record(card_id)?;
        self.require_stopped_for_script_update(&rec)?;
        if control.cancelled() {
            return Err(ModelManagerError::new("已取消，原脚本未修改。"));
        }
        let pkg = {
            let card = self.store.get_card(card_id)?;
            let empty = BTreeMap::new();
            super::card_update::validate_inventory(
                &card.scripts,
                card.script_sha256.as_ref().unwrap_or(&empty),
                &card.assets,
                card.asset_sha256.as_ref().unwrap_or(&empty),
            )
            .map_err(ModelManagerError::new)?;
            // Updating scripts is not an implicit migration to different weights,
            // engines or profiles. Those require a separate deployment workflow.
            if card.summary.id != rec.card_id
                || card.source_repo_id != rec.source_repo_id
                || card.source_filename != rec.source_filename
                || card.summary.size_bytes != rec.size_bytes.max(0) as u64
                || card.summary.deploy_topology != rec.deploy_topology
                || serde_json::to_value(&card.profiles).ok()
                    != serde_json::to_value(&rec.profiles).ok()
            {
                return Err(ModelManagerError::new(
                    "卡片的权重或部署方案已改变，不能仅更新脚本。已保留本地资源，请联系维护者提供兼容升级。",
                ));
            }
            let pkg = self.fetch_and_store_scripts(&card)?;
            if rec.assets_content.get("weights.sha256").is_some_and(|old| {
                pkg.assets.get("weights.sha256").map(|new| &new.sha256) != Some(&old.sha256)
            }) {
                return Err(ModelManagerError::new(
                    "卡片权重校验清单已改变，不能仅更新脚本。已保留原版本和权重，请联系维护者提供兼容升级。",
                ));
            }
            // 引擎版本变化不拦（含已部署状态，2026-10-22 根治）：部署流程会按
            // 本地缓存脚本探测并补装正确引擎（prepare_deployment_engine），「先换
            // 脚本→重部署」是安全路径。此前在此拦截迫使更新链先卸载，而卸载一步
            // 又可能被危险旧脚本拦截器/旧运行时结构/组可写目录卡住，形成设备无
            // 自助出路的死锁（用户设备实锤三连）。更新脚本会重置
            // deployed_script_sha256，界面据此提示重新部署。
            pkg
        };
        let urls = |files: &BTreeMap<String, db::ScriptBlob>| {
            files
                .keys()
                .map(|key| (key.clone(), "https://local.invalid/validated-cache".into()))
                .collect()
        };
        let hashes = |files: &BTreeMap<String, db::ScriptBlob>| {
            files
                .iter()
                .map(|(key, blob)| (key.clone(), blob.sha256.clone()))
                .collect()
        };
        super::card_update::validate_inventory(
            &urls(&pkg.scripts),
            &hashes(&pkg.scripts),
            &urls(&pkg.assets),
            &hashes(&pkg.assets),
        )
        .map_err(ModelManagerError::new)?;
        for blob in pkg.scripts.values().chain(pkg.assets.values()) {
            if script_runner::sha256_hex(&blob.content) != blob.sha256 {
                return Err(ModelManagerError::new("脚本包校验失败，已保留原版本。"));
            }
        }
        if control.cancelled() {
            return Err(ModelManagerError::new("已取消，原脚本未修改。"));
        }
        self.require_stopped_for_script_update(&rec)?;
        db::replace_card_package(&rec, &pkg.scripts, &pkg.assets).map_err(ModelManagerError::new)
    }

    fn require_stopped_for_script_update(
        &self,
        rec: &db::StoreModelRecord,
    ) -> Result<(), ModelManagerError> {
        self.require_actually_stopped(rec, "更新脚本")
    }

    /// 权重不完整归位前的等价停验（本批评审 P2-1）。
    ///
    /// deploy/enable 撞上「权重不完整」时走 [`db::mark_weights_incomplete`] 归位，
    /// 而它会连 running/container_name/endpoint_url/endpoint_model 一起抹掉。
    /// reconcile 的两条反向认领臂（容器名认领、端点+身份文件认领）都要求
    /// status=="deployed" 且这些身份字段还在——容器其实活着却被抹掉身份的卡，
    /// 从此进程照旧占着内存/端口，应用内却再也没有停止入口。所以抹身份前先做
    /// 与卸载预检同款的真实验验（复用 [`Self::require_actually_stopped`]：容器
    /// inspect / 原生进程指纹 / 端口存活三重核验）：真不在才抹；仍在或查不动，
    /// 就保留原记录，把「权重不完整但实例仍在」如实报给用户并指引自助恢复。
    /// 认领臂的 status 条件不动——那会放宽认领面，停验才是保守的那一侧。
    fn require_instance_gone_for_weights_reset(
        &self,
        card_id: &str,
    ) -> Result<(), ModelManagerError> {
        // 重新读一遍记录：deploy/enable 脚本可能已经跑了很久，落库前按最新
        // 状态判定，而不是拿进入函数时的旧快照。
        let rec = self.get_record(card_id)?;
        if let Err(alive) = self.require_actually_stopped(&rec, "继续下载") {
            return Err(ModelManagerError::new(format!(
                "模型文件未完整下载，但该模型仍有实例在运行或无法确认已停止，\
                 已保留原有运行记录不做改动。请先停用该模型，再点击「继续下载」\
                 补齐文件后重新部署。（停验未通过：{alive}）"
            )));
        }
        Ok(())
    }

    /// 三重真实核验（容器 inspect / 原生进程指纹 / 端口存活）+ DB 标志，
    /// `action` 只用于错误文案（"更新脚本" / "卸载"）。
    ///
    /// 为什么不能只信 `rec.running`：stop 半成功（`stop.sh || echo 继续` 模式）时
    /// 标志已是 false 而进程还活——只看标志会在活进程脚下更新脚本/卸载（评审 P2：
    /// 智能更新流 stop→uninstall→card_scripts 串行正踩这条路径）。
    pub fn require_actually_stopped(
        &self,
        rec: &db::StoreModelRecord,
        action: &str,
    ) -> Result<(), ModelManagerError> {
        if rec.running {
            return Err(ModelManagerError::new(format!(
                "请先停用此模型，再{action}；不会自动停止模型。"
            )));
        }
        if let Some(name) = rec.container_name.as_deref() {
            match proc_timeout::query(
                "docker",
                &["inspect", "--format", "{{.State.Running}}", name],
            ) {
                Some(state) if state.trim() == "false" => return Ok(()),
                // 容器不存在 = 已停止（2026-10-22 用户验证实锤：sglang0519 的
                // container_name 留在库里而容器早已删掉，inspect 报 No such object，
                // 此前落入"无法确认"分支把更新链整个拒掉）。用 ps -a 精确名字过滤
                // 区分「不存在」与「真的查不动」。
                //
                // r18 P2-1：`ps -a` 自己也返回 None（daemon 卡死/宕机）时是「查不动」，
                // 不是「不存在」——此前 is_some_and 把两种 None 混为一谈，live-restore
                // 下活容器会被宣告已停。与 reconcile_running_status 的 None=未知口径
                // 对齐：查不动就拒绝，宁可让用户重试。
                _ => {
                    let Some(ps_stdout) = proc_timeout::query(
                        "docker",
                        &[
                            "ps",
                            "-a",
                            "--filter",
                            &format!("name=^/{name}$"),
                            "--format",
                            "{{.Names}}",
                        ],
                    ) else {
                        return Err(ModelManagerError::new(format!(
                            "暂时查不到 Docker 状态（守护进程无响应），无法确认容器已停止，请稍后再试或先检查 Docker，再{action}。"
                        )));
                    };
                    let exists = ps_stdout.lines().any(|l| l.trim() == name);
                    if !exists {
                        return Ok(());
                    }
                    return Err(ModelManagerError::new(format!(
                        "模型容器仍在运行或无法确认已停止，请先检查并停用，再{action}。"
                    )));
                }
            }
        }
        if let Some((_, fingerprint)) =
            rec.scripts_content
                .get("install_engine")
                .and_then(|install| {
                    native_tachyon_identity(
                        &install.content,
                        rec.assets_content
                            .get("common.sh")
                            .map(|b| b.content.as_str()),
                        rec.engine_version.as_deref(),
                    )
                })
        {
            // r21 P2-2：先看卡片自己的身份文件。mark_weights_incomplete /
            // mark_cleanup_required 会把 endpoint_model/container_name 一并抹掉，
            // 下面那套「进程 --model-id ↔ endpoint_model（model_name 兜底）」的
            // 比对从此失去目标（model_name 兜底六卡实测无一命中）——本 PR 创造的
            // incomplete/cleanup_required 两态上被重新拉起的实例认不出，purge
            // 预检放行，r19 P1-1 修的洞换个状态重现。身份文件
            // （.runtime/serve.identity.json）在卡片目录里、不随 DB 状态复位消失，
            // 且带 start_ticks/uid 防 PID 复用——与 reconcile 的收编臂同源同判据。
            let card_dir = config::data_dir()
                .join("cards")
                .join(config::safe_path_component(&rec.card_id).unwrap_or_default());
            if !card_dir.as_os_str().is_empty() && native_engine_identity_alive(&card_dir) {
                return Err(ModelManagerError::new(format!(
                    "此原生引擎仍有运行实例或状态无法确认，请先停用使用该引擎的模型再{action}。"
                )));
            }
            let snapshot = super::native_process::discover();
            if !snapshot.complete
                || snapshot.processes.iter().any(|process| {
                    let fingerprint_match = process
                        .executable
                        .parent()
                        .and_then(std::path::Path::file_name)
                        .and_then(|p| p.to_str())
                        == fingerprint.get(..8);
                    if !fingerprint_match {
                        return false;
                    }
                    // 同一引擎二进制被多张卡共用（两张驭光卡同指纹）：只有**属于
                    // 本卡**的运行实例才拦（2026-10-22 用户实锤：Ornith 在跑时
                    // 更新 Qwen 卡被误拦——纯脚本更新/卸载不碰引擎，跨引擎风险
                    // 跨引擎场景由部署时的引擎补装把关）。归属无法确认（进程无
                    // --model-id）的保守起见仍然拦。
                    native_process_owned_by_card(
                        process.model_id.as_deref(),
                        rec.endpoint_model.as_deref(),
                        &rec.model_name,
                    )
                })
            {
                return Err(ModelManagerError::new(format!(
                    "此原生引擎仍有运行实例或状态无法确认，请先停用使用该引擎的模型再{action}。"
                )));
            }
        } else if rec.status == "deployed" {
            // Unknown deployment must not be treated as safely stopped just
            // because a stale database flag says false.
            //
            // r19 P1-2：ollama 卡不走下面的 docker 判据——它既没有
            // container_name 也没有 docker 镜像，docker_owned_container_state
            // 只会得到 Absent，又因 ollama 引擎登记不出 docker_image 兜不回
            // 来，必落 _ 臂拒绝；而这个分支提前 return，下面的端口探测不可达。
            // 停用后 status 仍是 deployed。判据与 reconcile_running_status 的
            // ollama 分支同口径：ollama 服务不在 = 这台机器上任何 ollama 模型
            // 都不可能在服务 = 已停；服务还在时不能就此放行，也不该一刀切拒绝
            // （r21 P2-5：daemon 是系统服务常态存活，一刀切让稳态更新/卸载全部
            // 确定性失败）——继续按 /api/ps 查这个模型是否仍被加载。
            if matches!(
                deployed_stopped_evidence(&rec.engine_name),
                DeployedStoppedEvidence::OllamaService
            ) {
                if !ollama_alive(&self.settings.ollama_host) {
                    return Ok(());
                }
                // r21 P2-5：服务在 ≠ 这个模型在服务。ollama 卡的 deployed 是永态
                // （stop 只清 running 标志），daemon 是系统服务常态存活——只按
                // 服务判停，稳态下更新脚本/卸载全部确定性拒绝，正文原先宣称的
                // 「不再锁死」只在 daemon 已死时成立。对破坏性操作真正危险的
                // 是**这个模型此刻被加载在显存里**；判据改查 /api/ps 该模型
                // 是否加载（与 ollama_runtime_lines 同口径按 name 匹配）：
                // 未加载 → 放行；已加载或查不动 → 保守拒绝。
                return match ollama_model_loaded(&self.settings.ollama_host, &rec.model_name) {
                    Some(false) => Ok(()),
                    Some(true) => Err(ModelManagerError::new(format!(
                        "该模型仍加载在 Ollama 服务中，请先停用再{action}。"
                    ))),
                    None => Err(ModelManagerError::new(format!(
                        "Ollama 服务仍在运行且无法确认该模型是否已卸载，请先停用再{action}。"
                    ))),
                };
            }
            // 例外（2026-10-27 真机实锤）：无 container_name 的 docker 卡——
            // z-image-turbo 停用五天后点「更新脚本」，一路落到这里被「无法确认」
            // 拦死，设备从此没有自助出路。docker 卡的运行形态只有容器：按
            // compose 项目名/容器名 == card_id 找得到容器（任何状态）就按容器
            // 状态下结论；找不到而引擎登记过 docker 镜像，同样没有别的运行
            // 形态，视为已停。其余（docker 查不动、又无镜像记录）保留原护栏。
            return match docker_owned_container_state(&rec.card_id) {
                DockerOwnedContainerState::Running => Err(ModelManagerError::new(format!(
                    "模型容器仍在运行，请先停用再{action}。"
                ))),
                DockerOwnedContainerState::Stopped => Ok(()),
                DockerOwnedContainerState::Absent
                    if db::get_installed_engine(
                        &rec.engine_name,
                        rec.engine_version.as_deref(),
                    )
                    .ok()
                    .flatten()
                    .and_then(|e| e.docker_image)
                    .is_some() =>
                {
                    Ok(())
                }
                _ => Err(ModelManagerError::new(format!(
                    "无法可靠确认此部署已停止，请先停用后再{action}。"
                ))),
            };
        }
        // 端口存活只对**无容器名**的记录有意义（原生引擎）：docker 卡的存活性由
        // 上面的容器分支负责，而且同端口可能被别的服务占着——z-image 默认 8000
        // 与品悟内置 vLLM 同端口，拿 endpoint_url 探测会把停用已久的卡误判成
        // 「端口仍有服务」（2026-10-22 用户验证实锤）。
        if rec.container_name.is_none()
            && rec
                .endpoint_url
                .as_deref()
                .is_some_and(openai_compatible_endpoint_alive)
        {
            return Err(ModelManagerError::new(format!(
                "模型端口仍有服务或状态无法确认，请先停用后再{action}。"
            )));
        }
        Ok(())
    }

    fn get_record(&self, card_id: &str) -> Result<db::StoreModelRecord, ModelManagerError> {
        db::get_store_model(card_id)
            .map_err(|e| ModelManagerError::new(format!("读取模型记录失败：{e}")))?
            .ok_or_else(|| ModelManagerError::new("该模型尚未下载"))
    }

    /// A cached package may require a new engine after an explicit script update.
    /// Probe that package rather than trusting the historical installed-engine row.
    /// Reuse existing weights and never fetch a newer package during deployment.
    fn prepare_deployment_engine(
        &self,
        rec: &db::StoreModelRecord,
        source: &ScriptsSource,
        env: &HashMap<String, String>,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<(), ModelManagerError> {
        if control.cancelled() {
            return Err(ModelManagerError::new("已取消部署。"));
        }
        let probe = self.engine_probe(source, &rec.card_id, env);
        if probe.installed {
            return Ok(());
        }
        if rec.running {
            return Err(ModelManagerError::new(
                "模型仍在运行且无法确认引擎依赖，请先停用模型再重新部署。",
            ));
        }
        on_progress("检查发现当前脚本所需引擎尚未就绪，先安装引擎；已有模型权重将保留。");
        if probe.need_root {
            on_progress("安装此引擎需要管理员权限，请在系统授权窗口中确认。");
        }
        let lines = self.install_engine(
            source,
            env,
            &mut |line| on_progress(&script_runner::prefix_log_line("[安装引擎] ", line)),
            control.clone(),
            probe.need_root,
        )?;
        if control.cancelled() {
            return Err(ModelManagerError::new("已取消部署，未继续执行部署脚本。"));
        }
        // Native cards implement an exact binary checksum probe. A zero installer
        // exit alone must not turn a missing/corrupt engine into a successful deploy.
        if native_tachyon_version(rec).is_some()
            && !self.engine_probe(source, &rec.card_id, env).installed
        {
            return Err(ModelManagerError::new(
                "引擎安装后仍未通过完整性检查，已停止部署；请查看安装引擎日志。",
            ));
        }
        let cleanup_script = source.optional_script_blob("uninstall_engine")?;
        // 同 download 路径：自报镜像名先过白名单、不覆盖已登记的不同值
        // （r19 评审 P2-1）。
        let docker_image = engine_image_to_record(
            &rec.engine_name,
            rec.engine_version.as_deref(),
            extract_marker(&lines, "LMM_ENGINE_IMAGE="),
        );
        db::mark_engine_installed(
            &rec.engine_name,
            rec.engine_version.as_deref(),
            docker_image.as_deref(),
            source.script_hash("install_engine").as_deref(),
            cleanup_script.as_ref(),
        )
        .map_err(|error| ModelManagerError::new(format!("记录引擎安装状态失败：{error}")))?;
        Ok(())
    }

    /// profile_id 不传时：已经部署过就沿用上次用的方案（重新部署/修复场景），
    /// 从没部署过就用商店给的第一个方案——没有多方案概念的卡片 profiles 是空
    /// 列表，解析出来的就是 None，行为跟没有 profile 概念时完全一样。
    pub fn deploy(
        &self,
        card_id: &str,
        profile_id: Option<&str>,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<(), ModelManagerError> {
        let rec = self.get_record(card_id)?;
        // 状态门（r21 P1-1，对照 enable 的 status 门）。此前 deploy 对任何状态都
        // 直接跑脚本：cleanup_required 的卡在弹窗被误点「部署模型」（mark_cleanup_
        // required 不清 weights_ready_at，前端矩阵落在 enabled 部署格）后，脚本
        // 一报权重残缺，错误路径的 mark_weights_incomplete 会把 cleanup_required
        // **静默改写成 incomplete**——正是 db.rs 注释禁止的「用继续下载掩盖清理态」。
        // incomplete / 缺 weights_ready_at（升级前的老库）同理：权重未就绪时部署
        // 必然失败，门在跑脚本之前把指引给对（先继续下载补齐）。
        if rec.status == "cleanup_required" {
            return Err(ModelManagerError::new(
                "该模型上一次删除没有清理完，请先点「继续清理」完成清理（或明确选择仅从列表移除），再部署。",
            ));
        }
        if rec.status == "incomplete" || rec.weights_ready_at.is_none() {
            return Err(ModelManagerError::new(
                "模型权重尚未就绪，请先点「继续下载」补齐文件后再部署。",
            ));
        }
        let source = self.local_scripts_source(&rec);
        let profiles = source.profiles();
        let mut resolved_profile_id = profile_id
            .map(|s| s.to_string())
            .or_else(|| rec.active_profile_id.clone());
        if resolved_profile_id.is_none() {
            if let Some(first) = profiles.first() {
                resolved_profile_id = Some(first.id.clone());
            }
        }
        if let Some(pid) = &resolved_profile_id {
            if !profiles.is_empty() && !profiles.iter().any(|p| &p.id == pid) {
                return Err(ModelManagerError::new(format!("未知的部署方案：{pid}")));
            }
        }

        let mut env = self.env_for_record(&rec);
        if let Some(pid) = &resolved_profile_id {
            env.insert("LMM_DEPLOY_PROFILE".into(), pid.clone());
        }

        self.prepare_deployment_engine(&rec, &source, &env, on_progress, control.clone())?;

        match self.run_card_script(
            &source,
            "deploy",
            &env,
            &mut |line| on_progress(line),
            control,
            false,
        ) {
            Ok(_) => {}
            Err(error) if model_weights_are_incomplete(&error.to_string()) => {
                // DB 的 downloaded 只是一份历史投影；用户可以在应用外清目录，旧版也
                // 曾在下载中断后留下错误记录。部署脚本已经给出了“文件不完整”的
                // 物理事实，立刻把状态归正，下一次界面刷新就必须给“继续下载”。
                //
                // 绝不删模型目录：下载脚本负责断点续传，清掉分片反而会浪费用户已经
                // 下完的几十 GB。
                //
                // 本批评审 P2-1：归位会连运行身份一起抹掉，抹前先做与卸载预检
                // 同款的真实验验；实例仍在就保留记录、如实报错。
                self.require_instance_gone_for_weights_reset(card_id)?;
                db::mark_weights_incomplete(card_id).map_err(|db_error| {
                    ModelManagerError::new(format!("修复模型下载状态失败：{db_error}"))
                })?;
                return Err(ModelManagerError::new(
                    "模型文件未完整下载，已切换为“继续下载”。已下载的部分会保留；请点击“继续下载”补齐文件后再部署。",
                ));
            }
            Err(error) => return Err(error),
        }
        // 记下这次用的 deploy 脚本版本：以后脚本一更新，界面就能准确判断
        // 「这个模型需要重新部署」，而不是对所有已部署模型都常驻一个按钮。
        db::update_store_model_status(card_id, "deployed", source.script_hash("deploy").as_deref())
            .map_err(|e| ModelManagerError::new(format!("记录部署状态失败：{e}")))?;
        if let Some(pid) = &resolved_profile_id {
            let _ = db::set_active_profile(card_id, Some(pid));
        }
        Ok(())
    }

    pub fn enable(
        &self,
        card_id: &str,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<(), ModelManagerError> {
        let rec = self.get_record(card_id)?;
        if rec.status != "deployed" {
            return Err(ModelManagerError::new("该模型尚未部署，请先部署再启用"));
        }
        // 本批评审 P2-8：启用前的廉价在跑守卫。卸载侧有三重停验，启用此前只有
        // 内存准入——一张被遗留脚本 fail-open 启过的卡（老版 enable.sh 半途失败
        // 仍报成功、或运维照卡片文档手工跑过脚本），库里 running=0 而服务真在跑，
        // 再点「启用」就是第二个实例（sglang 双负载拖垮整机已实锤）。两条判据：
        // 1) DB 已说在跑 → 拒绝，别在活实例上再叠一个；
        // 2) 本卡报过 OpenAI 风格端点且探针有响应 → 同样拒绝。口径与
        //    reconcile 一致：非 OpenAI 路径（z-image 的 /generate）改写探测必
        //    404，不探；ollama 例外——服务级探针分不清哪张卡在跑，只认 DB 标志。
        // 这里选择**拒绝**而不是静默按已启用返回成功：enable 没有幂等成功的
        // 先例，报错并指引自助恢复（先停用再启用）与全库 fail-closed 的风格
        // 一致，也不会把「探针看走眼」粉饰成成功。
        if rec.running {
            return Err(ModelManagerError::new(
                "该模型已在运行，请勿重复启用；如需重启，请先停用再启用。",
            ));
        }
        if rec.engine_name != "ollama" {
            if let Some(endpoint) = rec.endpoint_url.as_deref() {
                if endpoint_path_is_openai_style(endpoint)
                    && openai_compatible_endpoint_alive(endpoint)
                {
                    return Err(ModelManagerError::new(
                        "该模型的服务端口仍有响应（可能是旧版脚本或手工操作启动的遗留\
                         实例），已取消本次启用以避免出现双实例。请先停用该模型，\
                         再重新启用；若确认没有实例在跑，请稍后重试。",
                    ));
                }
            }
        }
        let env = self.env_for_record(&rec);
        let source = self.local_scripts_source(&rec);

        let output_lines = match self.run_card_script(
            &source,
            "enable",
            &env,
            &mut |line| on_progress(line),
            control,
            false,
        ) {
            Ok(lines) => lines,
            Err(error) if model_weights_are_incomplete(&error.to_string()) => {
                // 已部署模型同样可能在应用外被清走权重。不能让它一直顶着
                // “启用”按钮反复失败；跟 deploy 路径一样，归正为可续传的下载状态。
                //
                // 本批评审 P2-1：enable 撞权重不完整时这张卡多半刚部署过、
                // 容器还活着的概率不低，抹身份前的等价停验尤其不能省。
                self.require_instance_gone_for_weights_reset(card_id)?;
                db::mark_weights_incomplete(card_id).map_err(|db_error| {
                    ModelManagerError::new(format!("修复模型下载状态失败：{db_error}"))
                })?;
                return Err(ModelManagerError::new(
                    "模型文件未完整下载，已切换为“继续下载”。已下载的部分会保留；请点击“继续下载”补齐文件后再部署。",
                ));
            }
            Err(error) if deployment_receipt_is_missing(&error.to_string()) => {
                // “已部署”只是历史数据库状态；卡片自己的部署配置可能被手工清理、
                // 被旧版卸载遗漏，或因中途失败而从未真正写完。继续保留这个假状态，
                // 界面就会一直给用户“启用”按钮，而每次都必然失败。
                //
                // 退回“已下载”只清部署痕迹，不会删除几十 GB 权重或任何引擎。下次
                // 点击“部署”会用已保存的脚本与上次选择的 profile 重新生成配置。
                db::mark_deployment_missing(card_id).map_err(|db_error| {
                    ModelManagerError::new(format!("修复失效部署状态失败：{db_error}"))
                })?;
                return Err(ModelManagerError::new(
                    "该模型的部署配置已丢失，已恢复为“已下载”。请先点击“部署”，部署完成后再启用。",
                ));
            }
            Err(error) if engine_startup_failed(&error.to_string()) => {
                // Other running models are context, not proof of an OOM. Keep
                // the actual error and qualify the resource-pressure suggestion.
                let running = db::list_store_models()
                    .map_err(|db_error| {
                        ModelManagerError::new(format!("读取运行模型失败：{db_error}"))
                    })?
                    .into_iter()
                    .filter(|model| model.running && model.card_id != card_id)
                    .map(|model| model.card_name)
                    .collect::<Vec<_>>();
                if running.is_empty() {
                    return Err(error);
                }
                let names = running
                    .iter()
                    .map(|name| format!("「{name}」"))
                    .collect::<Vec<_>>()
                    .join("、");
                return Err(ModelManagerError::new(format!(
                    "无法启用「{}」：{}。同时检测到{}正在运行，可能增加运行资源压力，但尚不能据此确定失败原因。请检查诊断日志；如内存紧张，可先停用其他模型后重试，不要直接重新下载权重。",
                    rec.card_name, error, names
                )));
            }
            Err(error) => return Err(error),
        };
        db::set_running(card_id, true)
            .map_err(|e| ModelManagerError::new(format!("记录运行状态失败：{e}")))?;

        // 记下这次真正起来的容器名。这是判断"某个容器是不是本应用起的"唯一
        // 可靠依据——镜像不是身份：真机上遇到过同事用同一个 vllm 镜像手工起了
        // 另一个模型，按镜像判定的话 LMM 既会把它藏进自己名下，又会反过来把
        // 自己那张卡误标成"运行中"。
        if let Some(name) = extract_marker(&output_lines, "LMM_CONTAINER_NAME=") {
            let _ = db::set_container_name(card_id, Some(name.trim_start_matches('/')));
        }

        // enable 脚本可以在输出里打印 LMM_ENDPOINT_URL= / LMM_ENDPOINT_MODEL=
        // 这两行，告诉 LMM 这个模型对外怎么调用（给 API 网关转发用）。
        let mut endpoint_url = extract_marker(&output_lines, "LMM_ENDPOINT_URL=");
        let mut endpoint_model = extract_marker(&output_lines, "LMM_ENDPOINT_MODEL=");

        // 标记行是「脚本 → 宿主」的约定通道，但**内容完全由卡片决定**，跟卡片正文
        // 一样不可信。落库前必须断言它指向本机，因为这一行会被「在品悟里使用」原样
        // 写成模型的 base_url：一张被篡改（或上游被投毒）的卡片只要打印一句
        // `LMM_ENDPOINT_URL=http://<远程>/v1`，用户此后**每一句对话**都发去那台机器，
        // 而界面上只会显示「已接入本地模型」。
        //
        // 更糟的是没有退路：清理侧（pinvou-model-link.js 的 findLinkedRows）只认回环
        // base_url，非回环的那行卸载时根本匹配不到——卡片删了它还留着，且接入时会被
        // 设为当前模型，所以留下的很可能正是默认模型。
        //
        // 非法就**丢掉标记、不打断启用**：服务这会儿已经真的起来了（上面 set_running
        // 刚落库），为了一行标记把整个 enable 报成失败，用户只会去反复重启服务，一个
        // 问题都解决不了。丢掉的代价是详情页少一行调用地址、「在品悟里使用」用不了，
        // 都在安全的那一侧；而这句 warning 会进运行日志，排查时看得到。
        if let Some(bad) = endpoint_url.take_if(|url| !endpoint_url_is_loopback(url.as_str())) {
            on_progress(&format!(
                "已忽略卡片报上来的对外地址（{bad}）：它不是本机回环地址。\
                 模型商店只接受 127.0.0.0/8、localhost、::1；换成别的地址意味着\
                 把对话内容发往那台机器，不会被接受。"
            ));
        }

        if endpoint_url.is_none() && rec.engine_name == "ollama" {
            // 没有约定标记（或者标记刚被上面那条回环断言丢掉）：走 Ollama 的默认
            // 约定（OpenAI 兼容接口，模型名就是 ollama create 时用的名字）。
            //
            // 这一支**刻意不受那条断言管**：`ollama_host` 来自本机 config.json
            // （默认 127.0.0.1:11434，全仓没有任何界面/命令能改它），是部署这台
            // 机器的人自己写的，跟「卡片脚本打印了什么」是两个信任级别——把运维
            // 有意指向的地址也一并拒掉，属于拿安全检查去否决用户自己的配置。
            // 真出现非回环的 ollama_host 时，接入侧（pinvou-model-link.js 的
            // checkLinkable）仍然会拦住「在品悟里使用」，对话流量出不了本机。
            endpoint_url = Some(format!(
                "{}/v1/chat/completions",
                self.settings.ollama_host.trim_end_matches('/')
            ));
            endpoint_model = Some(rec.model_name.clone());
        }
        let _ = db::set_endpoint_info(card_id, endpoint_url.as_deref(), endpoint_model.as_deref());
        Ok(())
    }

    pub fn stop(
        &self,
        card_id: &str,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<(), ModelManagerError> {
        let rec = self.get_record(card_id)?;
        let env = self.env_for_record(&rec);
        let source = self.local_scripts_source(&rec);
        self.run_card_script(
            &source,
            "stop",
            &env,
            &mut |line| on_progress(line),
            control,
            false,
        )?;
        db::set_running(card_id, false)
            .map_err(|e| ModelManagerError::new(format!("记录运行状态失败：{e}")))?;
        Ok(())
    }

    /// 卸载一张卡。`purge_weights` 就是用户在界面上按的那个按钮，它同时决定了
    /// 磁盘和库记录两件事：
    ///
    /// - `false`（**卸载**）：停服务、删容器、删部署配置和卡片自己的运行时目录，
    ///   **已下载的权重留在磁盘上**，**卡片留在模型库里退回「已下载」**。
    ///   下次要用点「部署」即可，不用再下几十上百 GB。
    /// - `true`（**删除**）：连权重一起删，库记录一并移除，不可撤销。
    ///
    /// 两件事必须同进同退：权重留着而记录没了，那几十 GB 就变成没有任何界面
    /// 管得到的孤儿；记录留着而权重没了，库里就多一条指向空气的死数据。
    ///
    /// 这个意图通过 `LMM_PURGE_WEIGHTS=1|0` 传给卡片脚本（见《模型卡片脚本
    /// 规范》4.6 与第 5 节）。两个值都**显式**设，不留"不设=让脚本猜"的空档；
    /// 脚本侧约定的缺省（变量没有时按 `0` 处理）是给手工执行脚本的人兜底的。
    ///
    /// 为什么参数没有默认值、要调用方明说
    /// -------------------------------
    /// 删几十上百 GB 的权重是不可逆的，而"要不要删"是用户的判断不是我们的。
    /// 方法签名上留一个必填的 bool，谁调用都得先回答这个问题；真正的默认值只
    /// 在最外层的接口上定义（见 admin.rs 的 `LifecycleBody`：字段缺席时按
    /// **保留权重**处理——不可逆的那一侧绝不能靠"没写"命中）。
    pub fn uninstall(
        &self,
        card_id: &str,
        purge_weights: bool,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<UninstallOutcome, ModelManagerError> {
        let rec = self.get_record(card_id)?;
        let source = self.local_scripts_source(&rec);
        let uninstall = source.entry_content("uninstall", source.fetch_policy("uninstall"))?;
        // Exact published-body backstop for a confirmed destructive version.
        // Hash the bytes, not cached metadata or the card name. This is NOT a
        // safety certification for arbitrary other/new card scripts.
        if script_runner::sha256_hex(&uninstall)
            == "184de35d3e0844a4a43c8ffc6ab481aeeb65a06843d68410c18fe0f869ec349c"
        {
            return Err(ModelManagerError::new(
                "已拦截存在已知删除风险的旧版卸载脚本：该版本忽略保留权重选项，停用失败后仍会继续删除。\
                 本次未执行脚本、未停止模型或删除文件。请先获取经过修复的模型卡片包，再继续更新或卸载；无需重新下载现有权重。",
            ));
        }
        // Old URL-only records have no sibling assets. Pin the checked entry
        // in memory so a second HTTP response cannot replace it after preflight.
        let source = match source {
            ScriptsSource::Remote { profiles, .. } => ScriptsSource::Local {
                blobs: BTreeMap::from([(
                    "uninstall".into(),
                    db::ScriptBlob {
                        sha256: script_runner::sha256_hex(&uninstall),
                        content: uninstall,
                    },
                )]),
                assets: BTreeMap::new(),
                profiles,
            },
            local => local,
        };
        let mut env = self.env_for_record(&rec);
        env.insert(
            "LMM_PURGE_WEIGHTS".into(),
            if purge_weights { "1" } else { "0" }.into(),
        );

        // 先量一次基线。**必须在跑脚本之前量**：脚本一跑完目录就没了，事后再
        // 想知道"刚才那儿有多少字节"已经无处可查，只剩下 card.json 里那个标称
        // 值可以拿来充数——而那正是要杜绝的事。
        //
        // 清单 = 猜的候选 + `.deploy-env` 记录的实际部署目录：猜错方向的代价
        // 见 `deployed_weights_hint`（量不到真权重 → "实测释放"虚报为 0）。
        let candidates = self.owned_candidate_paths(&rec);
        let before = total_size(&candidates);

        self.run_card_script(
            &source,
            "uninstall",
            &env,
            &mut |line| on_progress(line),
            control,
            false,
        )?;

        // `cards/<card-id>` 由 LMM 自己创建，里面只有这张卡下载的脚本、厂商附属包
        // 与标记文件。用户选择“删除模型数据”后，这些文件也在删除范围内；不能让
        // 卡片脚本各自记得删，否则脚本升级/回滚一次就会留下看不见的部署包。
        //
        // 注意：只在卡片脚本已经成功结束后处理，且路径由安全的 card id 生成。
        // 不接收脚本输出的任意路径，也不碰共享的 engines/ 目录。
        if purge_weights {
            if let Ok(safe_id) = config::safe_path_component(&rec.card_id) {
                let card_dir = config::data_dir().join("cards").join(safe_id);
                if card_dir.exists() {
                    on_progress("正在清理此模型的本地脚本与部署文件…");
                    if let Err(error) = std::fs::remove_dir_all(&card_dir) {
                        // 不提前返回：下面必须先重新扫描并把残留状态落库。否则一旦
                        // 这里权限不足，卡片又会从列表消失，用户无从再点“继续清理”。
                        on_progress(&format!("本地脚本与部署文件尚未清理：{error}"));
                    }
                }
            }
        }

        let after = total_size(&candidates);
        let freed_bytes = before.saturating_sub(after);
        // 残留判定前先摘掉**其他在库卡片**拥有的路径：猜的权重目录可能跟别的卡
        // 碰名（repo 末段相同）或共用同一个 blob 根（ollama 全家）。不摘的话，
        // 本卡删得再干净也会被判"尚未全部删除"，而那个目录恰恰是别的卡的活
        // 权重，重试一万次也删不掉——用户最后只能走强制移除，还被提示去手动
        // 清理那个目录，一下就把保留卡的权重删了（2026-11-03 真机剧本）。
        let other_cards_paths = self.paths_owned_by_other_cards(card_id);
        let remaining_paths: Vec<String> = candidates
            .iter()
            .filter(|p| p.exists() && !other_cards_paths.contains(p))
            .map(|p| p.to_string_lossy().to_string())
            .collect();

        // 删除是否完成必须由**删除后的磁盘事实**决定，不能根据脚本退出码猜。
        // 先落状态再决定是否移除记录：任何残留都会进入“需要继续清理”，卡片仍在
        // 模型库中，用户可重试、打开日志或明确选择“仅从列表移除”。
        if purge_weights {
            if remaining_paths.is_empty() {
                db::delete_store_model(card_id)
                    .map_err(|e| ModelManagerError::new(format!("删除模型记录失败：{e}")))?;
            } else {
                db::mark_cleanup_required(card_id)
                    .map_err(|e| ModelManagerError::new(format!("保留待清理模型记录失败：{e}")))?;
                return Err(ModelManagerError::new(format!(
                    "模型数据尚未全部删除。已保留在模型库中，请点击“继续清理”重试；如仍失败，可打开日志目录并反馈。仍需处理：{}",
                    remaining_paths.join("、")
                )));
            }
        } else {
            db::mark_uninstalled(card_id)
                .map_err(|e| ModelManagerError::new(format!("更新模型状态失败：{e}")))?;
        }

        let freed_text = disk::format_bytes(freed_bytes as f64);
        let summary = if purge_weights {
            if remaining_paths.is_empty() {
                format!(
                    "已卸载「{}」并删除模型权重，实测释放 {}。",
                    rec.card_name, freed_text
                )
            } else {
                // 脚本返回了 0 却还有东西剩着：可能是权重放在别的分区、也可能是
                // 某一步 rm 被静默跳过。不替用户抹平——他是冲着"把盘收回来"点的。
                format!(
                    "已卸载「{}」，实测释放 {}；但以下目录仍在磁盘上，需要时请手动清理：{}",
                    rec.card_name,
                    freed_text,
                    remaining_paths.join("、")
                )
            }
        } else if remaining_paths.is_empty() {
            // 用户选了保留，扫下来却什么都没剩：可能权重本来就不在常见位置，
            // 也可能脚本根本没认 LMM_PURGE_WEIGHTS。不能顺口说成"权重已保留"。
            format!(
                "已卸载「{}」：服务和部署配置已移除，实测释放 {}。\
                 卡片仍在模型库里（已下载），但在常见位置上没有找到保留下来的权重目录，\
                 请在重新部署前确认一下。",
                rec.card_name, freed_text
            )
        } else {
            format!(
                "已卸载「{}」：服务和部署配置已移除，模型权重保留在磁盘上（{}）。\
                 卡片仍在模型库里，随时可以直接重新部署，不用再下一遍权重。本次实测释放 {}。",
                rec.card_name,
                remaining_paths.join("、"),
                freed_text
            )
        };

        Ok(UninstallOutcome {
            card_id: rec.card_id,
            model_name: rec.model_name,
            purged_weights: purge_weights,
            record_kept: !purge_weights,
            freed_bytes,
            recorded_bytes: rec.size_bytes,
            remaining_paths,
            summary,
        })
    }

    /// 逃生出口：**只删数据库记录，一个字节都不删**。
    ///
    /// 什么时候需要它
    /// -------------
    /// `uninstall()` 只有在脚本返回 Ok 时才删记录，这是对的——脚本没删干净就把
    /// 记录抹掉，等于把磁盘上的几十 GB 变成谁也管不着的孤儿。但它留下了一个死
    /// 结：脚本的删除顺序是 `compose down` → `rm -rf runtime` → `rm -rf
    /// ${MODEL_PATH}`，中途任何一次中断（用户取消、静默超时被杀、磁盘 I/O 错误）
    /// 都会停在「磁盘半清理 + 记录完好」的状态。而厂商脚本开头有一条安全检查：
    ///
    /// ```text
    /// [[ -f "${MODEL_PATH}/config.json" ]] || die "拒绝删除：目标不像模型目录"
    /// ```
    ///
    /// `rm -rf` 是按 readdir 顺序删的，config.json 多半已经跟着没了。于是重试
    /// 第二次、第三次、第一百次都是同一句 die、同一个 exit 1、记录同样删不掉。
    /// 用户界面上只看到「拒绝删除：目标不像模型目录」，卡片永远赖在库里。
    ///
    /// 为什么不去改那条厂商检查
    /// ----------------------
    /// 它是对的。`MODEL_PATH` 来自 .env，写错一行就可能指到用户的家目录，那条
    /// 检查挡的正是「配置写错时 rm -rf 掉不该删的东西」。把它放宽，等于用一个
    /// 真正危险的 bug 去换一个恢复路径。正确的做法是在 LMM 这一侧提供「我承认
    /// 删不干净，先把记录清掉」的能力，把脚本的安全边界留在原地。
    ///
    /// 契约：不假装干净
    /// --------------
    /// 返回值里必须带着残留信息。这条路径没有做任何清理，调用方（以及最终的
    /// 用户）必须知道磁盘上可能还留着几十上百 GB，得自己去删——所以
    /// `residual_paths` 装的是**此刻确认还在磁盘上**的目录，`warning` 是一句能
    /// 直接贴到界面上的话。绝不能让用户以为点完就干净了。
    ///
    /// 这是破坏性且不可逆的操作（连这张卡的 API Key 一并删掉），调用方必须显式
    /// 走这条方法/这个端点，普通卸载不会拐到这里来。
    pub fn force_remove_record(&self, card_id: &str) -> Result<ForcedRemoval, ModelManagerError> {
        let rec = self.get_record(card_id)?;
        let residual_paths = self.residual_paths(&rec);

        db::delete_store_model(card_id)
            .map_err(|e| ModelManagerError::new(format!("删除模型记录失败：{e}")))?;

        let warning = if residual_paths.is_empty() {
            "已从模型库移除这条记录。本操作只删记录、没有删除任何文件；\
             常见位置上没有发现残留目录，但卡片可能把权重放在别处，磁盘空间请自行核实。"
                .to_string()
        } else {
            format!(
                "已从模型库移除这条记录，但**磁盘上的文件没有被删除**。\
                 以下目录仍然存在，需要手动清理：{}",
                residual_paths.join("、")
            )
        };
        // 留一条 stderr：这是不可逆操作，日志里要能查到是谁、什么时候干的
        eprintln!("[force_remove] 强制移除记录 {card_id}，残留：{residual_paths:?}");

        Ok(ForcedRemoval {
            card_id: rec.card_id,
            model_name: rec.model_name,
            endpoint_model: rec.endpoint_model,
            recorded_bytes: rec.size_bytes,
            residual_paths,
            warning,
        })
    }

    /// 猜「这张卡的东西可能还留在哪」，并且只报**此刻真的还在**的目录。
    ///
    /// 路径是按卡片脚本的约定反推的，不是 LMM 自己记下来的（脚本里的
    /// `MODEL_PATH` 写在它自己的 .env 里，LMM 读不到），所以这里只能是"候选"。
    /// 正因为是猜的，才必须用 `exists()` 过滤一遍：报一个根本不存在的路径，用户
    /// 去 `ls` 发现没有，下次就不信这条提示了。反过来，漏报也比误报安全——
    /// warning 里那句"请自行核实"就是为漏报兜底的。
    fn residual_paths(&self, rec: &db::StoreModelRecord) -> Vec<String> {
        // 与 uninstall 的收尾判定同一套口径：实际部署目录入列、他卡拥有的路径
        // 摘掉。强制移除的"残留点名"会引导用户去手动 rm——把保留卡的活权重
        // 列进去等于教用户删错东西（2026-11-03 真机：删除下线卡收尾被
        // `models_dir/Qwen3.8-27B-NVFP4` 挡住，那其实是保留卡的 21G 权重）。
        let other_cards_paths = self.paths_owned_by_other_cards(&rec.card_id);
        let mut out: Vec<String> = Vec::new();
        for p in self.owned_candidate_paths(rec) {
            let s = p.to_string_lossy().to_string();
            if p.exists() && !other_cards_paths.contains(&p) && !out.contains(&s) {
                out.push(s);
            }
        }
        out
    }

    /// 「这张卡的东西可能落在哪」的候选清单（去重，不判断存不存在）。
    ///
    /// 两个用处：`residual_paths` 拿它过滤出还在的，`uninstall` 拿它在跑脚本
    /// 前后各量一次算实测释放量。两边必须用**同一份**清单——否则"释放了多少"
    /// 和"还剩什么"会各说各话。
    fn candidate_paths(&self, rec: &db::StoreModelRecord) -> Vec<PathBuf> {
        let models_dir = self.settings.models_path();
        let mut candidates: Vec<PathBuf> = Vec::new();
        // 权重目录：卡片脚本统一放在 ${LMM_MODELS_DIR}/<HF 仓库名> 下。
        //
        // 末段同样要过 safe_path_component——本函数另外两个分支都过了，这一支漏了。
        // `source_repo_id` 来自商店响应，`"x/.."` 的末段就是 `..`，join 之后指向
        // models_dir 的**父目录**（默认就是用户主目录）：两次全树遍历能把卸载收尾
        // 卡住，`freed_bytes` 因父子重复计数虚增，还会把用户主目录当成"残留"报给他。
        // 这个函数只做测量和报告、不做删除，但同样违反本文件「宁可漏不能错」的原则。
        if let Some(repo) = rec.source_repo_id.as_deref() {
            if let Some(base) = repo.rsplit('/').next() {
                if let Ok(safe) = config::safe_path_component(base) {
                    candidates.push(models_dir.join(safe));
                }
            }
        }
        // 有些卡按用户起的模型名建目录，一并看一眼。同样要过一遍校验：
        // model_name 由 suggest_model_name 从卡片名推导，理论上能推出 ".."。
        if let Ok(safe_name) = config::safe_path_component(&rec.model_name) {
            candidates.push(models_dir.join(safe_name));
        }
        // 卡片目录：入口脚本、厂商原件（vendor/）、部署包（compose/scripts/
        // runtime/cache）都在这下面，install_engine 落的。
        //
        // 注意是 `cards/<id>`，**不是** `vendor/<id>`。卡片脚本里写的是
        // `VENDOR_DIR="$CARD_DIR/vendor"`，而 `CARD_DIR` 就是这里的 `cards/<id>`
        // （见 `PreparedScript::prepare`）——vendor 是卡片目录的子目录，不是
        // data_dir 下的平级目录。原先按 `vendor/<id>` 猜，那个路径在磁盘上根本
        // 不存在：`residual_paths` 的 `exists()` 会把它整条滤掉，于是强制移除
        // 永远报"没有残留"，而实际上几百 MB 的部署包还躺在 cards/<id> 里；
        // `uninstall` 的前后差值也因此漏算了这部分，"释放了多少"报小。
        //
        // 这里在算"要删哪些目录"，逃出去的路径意味着删错地方，比落错地方更糟——
        // 校验不过就整个不加，宁可漏删也不能乱删。
        if let Ok(safe_id) = config::safe_path_component(&rec.card_id) {
            candidates.push(config::data_dir().join("cards").join(safe_id));
        }
        // ollama 卡的权重不落 models_dir，blob 全在 `~/.ollama/models` 下（常有
        // 几十 GB）。漏掉它的话：`residual_paths` 对强制移除报"没有残留"，
        // `uninstall` 前后差值也量不到真实释放——用户听到的"已清干净/释放 0 字节"
        // 都是假的（r19 评审 P2-4）。目录解析跟 `ollama_size` 同一份逻辑
        // （`ollama_home_dir`），不能各写各的 HOME 拼法。
        if rec.engine_name == "ollama" {
            if let Some(ollama_dir) = ollama_home_dir() {
                candidates.push(ollama_dir);
            }
        }

        let mut out: Vec<PathBuf> = Vec::new();
        for p in candidates {
            if !out.contains(&p) {
                out.push(p);
            }
        }
        out
    }

    /// 这张卡**实际部署时**用的权重目录，从部署脚本落下的 `.deploy-env` 里读。
    ///
    /// 为什么需要它：[`Self::candidate_paths`] 是按卡片脚本约定**猜**的（repo 末段
    /// /模型名），而猜有两个已知的错方向——
    /// - 同名异源：`unsloth/Qwen3.8-27B-NVFP4` 与 `RadixArk/Qwen3.8-27B-NVFP4`
    ///   猜出来是**同一条** `models_dir/Qwen3.8-27B-NVFP4`；
    /// - 消歧后缀：厂商部署脚本会自己在末段上加后缀（真机 2026-11-03：
    ///   实际部署在 `Qwen3.8-27B-NVFP4-unsloth`，猜的路径两头都对不上）。
    ///
    /// 后一种让"实测释放"量不到真权重；前一种见 `paths_owned_by_other_cards`。
    ///
    /// 只信一种形态：`MODEL_DIR="<models_dir>/<单个安全段>"`（多行取最后一行，
    /// 与卸载脚本自己的读法一致）。`.deploy-env` 是卡片脚本写的文件，内容不可
    /// 信——逃出 models_dir、带路径分隔符或校验不过的一律当没有。这里的结果
    /// 只用于测量与残留报告，不做删除，但校验仍然先行。
    fn deployed_weights_hint(&self, rec: &db::StoreModelRecord) -> Option<PathBuf> {
        let safe_id = config::safe_path_component(&rec.card_id).ok()?;
        let env_file = config::data_dir()
            .join("cards")
            .join(safe_id)
            .join("vendor")
            .join(".deploy-env");
        let content = std::fs::read_to_string(env_file).ok()?;
        let last = content.lines().rev().find_map(|line| {
            line.strip_prefix("MODEL_DIR=\"")
                .and_then(|rest| rest.strip_suffix('"'))
        })?;
        let models_dir = self.settings.models_path();
        let candidate = PathBuf::from(last);
        let leaf = candidate.file_name()?.to_str()?;
        if candidate.parent() != Some(models_dir.as_path()) {
            return None;
        }
        // safe_path_component 是校验不是改写：过不了就当没有。
        if config::safe_path_component(leaf).ok()? != leaf {
            return None;
        }
        Some(candidate)
    }

    /// 本卡候选 + 实际部署目录，去重——`uninstall` 的前后计量与残留判定、
    /// `residual_paths` 的残留点名必须用**同一份**清单，否则"释放了多少"和
    /// "还剩什么"各说各话。
    fn owned_candidate_paths(&self, rec: &db::StoreModelRecord) -> Vec<PathBuf> {
        let mut candidates = self.candidate_paths(rec);
        if let Some(actual) = self.deployed_weights_hint(rec) {
            if !candidates.contains(&actual) {
                candidates.push(actual);
            }
        }
        candidates
    }

    /// 其他在库卡片的候选路径：这些目录不属于本卡，即便"存在"也不能算本卡的
    /// 删除残留。两类现实场景（都是把用户逼到「强制移除」逃生口的真机事故）：
    ///
    /// - repo 末段碰撞（2026-11-03 真机，cube-9fde）：删 A 卡时它猜的权重目录
    ///   恰是 B 卡的**活权重**——A 自己的东西全删干净了，收尾判定却永远看见
    ///   B 的 21G"仍在磁盘上"，卡进「需要继续清理」死循环，重试不可能成功；
    /// - ollama 家族：所有 ollama 卡的 blob 同住 `~/.ollama`，多卡互为 purge
    ///   残留，谁也删不干净（r21 登记的 P3 死循环）。
    ///
    /// 归属以「其他记录按同一套约定会猜到它」为准——宁可少报残留也不把别的卡
    /// 的活权重说成本卡欠的账；漏报由 force_remove 的"请自行核实"兜底。
    fn paths_owned_by_other_cards(&self, card_id: &str) -> Vec<PathBuf> {
        let mut out: Vec<PathBuf> = Vec::new();
        let Ok(rows) = db::list_store_models_light() else {
            return out;
        };
        for row in rows {
            if row.record.card_id == card_id {
                continue;
            }
            for p in self.candidate_paths(&row.record) {
                if !out.contains(&p) {
                    out.push(p);
                }
            }
        }
        out
    }

    // ---------------- 引擎管理 ----------------
    // 引擎（docker 镜像/ollama 等）可能被多张卡片共用，跟"模型"是两个独立的
    // 东西：卸载模型只删模型权重和部署配置，引擎本身默认保留。这里单独给用户
    // 一个入口去清理引擎本身占用的空间。
    //
    // "引擎和模型是两个独立的东西"这句话对 docker 形态成立，**对 ollama 不成立**
    // ——它的引擎和模型数据同住 `~/.ollama`，删引擎就是删模型。见 delete_engine。
    //
    // 推理引擎页调用这一节时，数据库只作为候选来源，展示前必须验明实体仍在。
    // 否则用户会看到“0 B、删不掉”的幽灵记录；删除入口也只会开放给已经解除
    // 全部模型绑定、且客户端有明确清理契约的引擎。

    pub fn list_installed_engines(&self) -> Vec<InstalledEngineInfo> {
        // 先把历史上“展示名误作身份键”的驭光记录迁到二进制真实版本。迁移只
        // 改数据库关联，不移动、更不删除任何引擎目录；随后仍以探针确认实体。
        self.migrate_native_engine_identities();
        // 存量用户不应为取得一份几 KB 的清理契约而重新下载几十 GB 模型权重。
        // 这里仅在本机已有对应原生引擎、且记录尚未有契约时，从商店按哈希补取。
        // 网络不可用就维持“不可自动删除”，绝不退化成猜路径删除。
        self.refresh_native_engine_cleanup_contracts();
        let Ok(records) = db::list_installed_engines() else {
            return Vec::new();
        };
        let models = db::list_store_models().unwrap_or_default();
        records
            .into_iter()
            .filter_map(|rec| {
                // 数据库只说明“过去装过”，不代表实体仍在。先验实际 docker 镜像、
                // ollama 目录或卡片的只读探针；旧记录/残缺目录绝不能在用户眼前伪装
                // 成一个 0 B 的可管理引擎。
                let (size, cleanup_supported) = if let Some(image) = &rec.docker_image {
                    let size = docker_image_size(image);
                    if size == 0 {
                        return None;
                    }
                    (Some(size), true)
                } else if rec.engine_name == "ollama" {
                    let size = ollama_size();
                    if size == 0 {
                        return None;
                    }
                    (Some(size), true)
                } else if let Some(cleanup_script) = rec.cleanup_script.as_ref() {
                    // 新版原生引擎必须用随引擎固化的清理契约探测。它能在模型
                    // 记录已经删掉后依然验明实体、给出真实大小，不能再依赖一张
                    // 恰好仍留在数据库里的模型卡片。
                    match native_engine_cleanup_probe(cleanup_script) {
                        Ok((false, _)) => return None,
                        Ok((true, size)) => (size, true),
                        Err(error) => {
                            // 探测失败不等于不存在：保留一条不可删除的待核实记录。
                            eprintln!(
                                "[model-store] 无法探测原生引擎 {}：{error}",
                                rec.engine_name
                            );
                            (None, false)
                        }
                    }
                } else {
                    let Some(model) = models.iter().find(|model| {
                        model.engine_name == rec.engine_name
                            && model.engine_version == rec.engine_version
                            && !model.scripts_content.is_empty()
                    }) else {
                        return None;
                    };
                    let source = self.local_scripts_source(model);
                    if !self
                        .engine_probe(&source, &model.card_id, &self.env_for_record(model))
                        .installed
                    {
                        return None;
                    }
                    // 老版原生引擎尚未下发独立清理契约；显示真实存在的实体，但
                    // 绝不猜路径 `rm -rf`。用户重新下载/更新到含清理契约的卡片
                    // 后，新的安装记录会接管这一项。
                    (None, false)
                };
                let affected = db::list_model_names_using_engine(
                    &rec.engine_name,
                    rec.engine_version.as_deref(),
                )
                .unwrap_or_default();
                Some(InstalledEngineInfo {
                    engine_name: rec.engine_name,
                    engine_version: rec.engine_version,
                    docker_image: rec.docker_image,
                    size_bytes: size,
                    affected_models: affected,
                    cleanup_supported,
                })
            })
            .collect()
    }

    /// 从已固化的 install_engine 脚本读取驭光二进制的内容指纹，并将历史记录
    /// 迁到稳定的 (engine_name, engine_version) 键。不能按卡片名称、目录名或
    /// 仅 8 位前缀猜测：只有卡片自己锁定的完整 SHA-256 才足以说明两者是同一
    /// 个原生二进制。
    fn migrate_native_engine_identities(&self) {
        // r19 评审 P2-6：这里只做指纹提取，改走只取 install_engine/common.sh
        // 两段的轻量查询，不再为整张表把六类脚本+附属文件正文拉出库。
        let Ok(models) = db::list_store_models_install_columns() else {
            return;
        };
        for model in models {
            let Some(install) = model.install_engine.as_deref() else {
                continue;
            };
            let Some(version) = native_tachyon_identity(
                install,
                model.common_sh.as_deref(),
                model.engine_version.as_deref(),
            )
            .map(|(version, _)| version) else {
                continue;
            };
            if let Err(error) = db::migrate_engine_identity(
                &model.card_id,
                NATIVE_TACHYON_ENGINE_NAME,
                Some(&version),
            ) {
                // 迁移只是兼容修复；列表不能因为某一条旧数据写不进去而整体不可用。
                eprintln!(
                    "[model-store] 迁移原生引擎身份失败（{}）：{error}",
                    model.card_id
                );
            }
        }
    }

    /// 为旧安装记录补取原生引擎的独立清理契约。
    ///
    /// 卡片的六类日常脚本下载成功时会固化进模型记录；`uninstall_engine` 是后加的
    /// 可选契约，老记录自然没有。只同步它本身（并强制校验商店下发的哈希），不
    /// 重跑安装、不会碰权重、不会改变部署状态。
    fn refresh_native_engine_cleanup_contracts(&self) {
        // r19 评审 P2-6：同 migrate_native_engine_identities——指纹比对只要
        // install_engine/common.sh 两段，走轻量查询。
        let Ok(models) = db::list_store_models_install_columns() else {
            return;
        };
        for model in models {
            let Some(install) = model.install_engine.as_deref() else {
                continue;
            };
            // 指纹（显示版本 + 完整 SHA-256）一次算出：开头判要不要补契约、
            // 后面跟商店脚本比对用的都是这一份，不再对同一段正文算两遍。
            let Some((version, local_sha)) = native_tachyon_identity(
                install,
                model.common_sh.as_deref(),
                model.engine_version.as_deref(),
            ) else {
                continue;
            };
            let already_present =
                db::get_installed_engine(NATIVE_TACHYON_ENGINE_NAME, Some(&version))
                    .ok()
                    .flatten()
                    .is_some_and(|engine| engine.cleanup_script.is_some());
            if already_present {
                continue;
            }
            // 会话级负缓存（评审 P2）：商店不可达时 get_card 的 3 次重试+退避
            // ≈18s/卡，而引擎管理页每次打开都无条件走一遍——离线存量设备每进
            // 一次页就再撞一轮。失败的卡本进程内不再重试；重启即清。
            let failed: &std::sync::Mutex<std::collections::HashSet<String>> =
                CONTRACT_SYNC_FAILED_CARDS.get_or_init(Default::default);
            if failed.lock().is_ok_and(|set| set.contains(&model.card_id)) {
                continue;
            }
            let card = match self.store.get_card(&model.card_id) {
                Ok(card) => card,
                Err(error) => {
                    eprintln!(
                        "[model-store] 无法同步原生引擎清理契约（{}）：{error}",
                        model.card_id
                    );
                    if let Ok(mut set) = failed.lock() {
                        set.insert(model.card_id.clone());
                    }
                    continue;
                }
            };
            // 商店若不是同一套二进制，绝不能把它的清理脚本安到当前引擎记录上。
            if card.summary.engine_name != NATIVE_TACHYON_ENGINE_NAME
                || card.summary.engine_version.as_deref() != Some(version.as_str())
            {
                continue;
            }
            let source = ScriptsSource::from_card(&card, self.local_script_paths);
            let cleanup_script = match source.optional_script_blob("uninstall_engine") {
                Ok(Some(script)) => script,
                Ok(None) => continue,
                Err(error) => {
                    eprintln!(
                        "[model-store] 原生引擎清理契约校验失败（{}）：{error}",
                        model.card_id
                    );
                    continue;
                }
            };
            // 显示版本可能被服务器复用；必须再比对完整二进制指纹，不能仅凭
            // 相同的卡片名/版本给历史引擎装上另一套二进制的删除脚本。
            // （指纹 local_sha 已在循环开头与 version 一次算出。）
            if declared_native_sha(&cleanup_script.content) != Some(local_sha) {
                eprintln!(
                    "[model-store] 原生引擎清理契约与本地二进制指纹不符（{}），保留现有记录",
                    model.card_id
                );
                continue;
            }
            if let Err(error) = db::set_engine_cleanup_script(
                NATIVE_TACHYON_ENGINE_NAME,
                Some(&version),
                &cleanup_script,
            ) {
                eprintln!(
                    "[model-store] 保存原生引擎清理契约失败（{}）：{error}",
                    model.card_id
                );
            }
        }
    }

    /// 启用前读取一次真实系统内存与当前受本模型商店管理的运行模型。
    ///
    /// 首个模型不因缺少峰值元数据被拦截；系统已处于低内存或已知预算超出余量时
    /// 仍提示风险并允许用户确认继续。已有模型但预算不全时必须确认，不用权重
    /// 体积冒充运行内存，也不拿进程树 RSS 抵扣统一内存预算。
    pub fn memory_admission(&self, card_id: &str) -> Result<MemoryAdmission, ModelManagerError> {
        let candidate = db::get_store_model(card_id)
            .map_err(|e| ModelManagerError::new(format!("读取模型记录失败：{e}")))?
            .ok_or_else(|| ModelManagerError::new("模型不存在，请先下载并部署"))?;
        let ram =
            current_ram_snapshot().filter(|ram| ram.total_kib > 0 && ram.used_kib <= ram.total_kib);
        let total_bytes = ram.as_ref().map(|ram| ram.total_kib.saturating_mul(1024));
        let available_bytes = ram.as_ref().map(|ram| {
            ram.total_kib
                .saturating_sub(ram.used_kib)
                .saturating_mul(1024)
        });
        const GIB: u64 = 1024 * 1024 * 1024;
        let safety_reserve_bytes = total_bytes.map(|total| (total / 100 * 15).max(12 * GIB));
        let hard_reserve_bytes = total_bytes.map(|total| (total / 100 * 10).max(6 * GIB));

        // Reconcile store ownership, then sample container trees and native
        // engines by executable fingerprint/model ID, never by a stale PID file.
        self.reconcile_running_status();
        // 本批评审 P2-4：这里连同上面的 reconcile 此前是连续两遍 list_store_models
        // 全量正文读。真正要的是运行中卡片的小字段 + install_engine/common.sh 两段
        // （算原生进程指纹）+ 内存预算，改走库内 json_extract 的窄投影，且只取
        // 运行中的卡（running 过滤已推进到 SQL 里）。
        let running_records = db::list_running_store_models_admission_columns()
            .map_err(|e| ModelManagerError::new(format!("读取运行模型失败：{e}")))?
            .into_iter()
            .filter(|model| model.card_id != candidate.card_id)
            .collect::<Vec<_>>();
        let native_snapshot = super::native_process::discover();
        let native_processes = &native_snapshot.processes;
        let roots = running_records
            .iter()
            .filter_map(|model| {
                let pid = if let Some(name) = model.container_name.as_deref() {
                    docker_container_pid(name)
                } else {
                    // 指纹两段正文来自窄投影的库内提取（见上面的查询），
                    // 语义与整表解析后取 install_engine/common.sh 相同。
                    let (_, fingerprint) = native_tachyon_identity(
                        model.install_engine.as_deref()?,
                        model.common_sh.as_deref(),
                        model.engine_version.as_deref(),
                    )?;
                    super::native_process::matching_pid(
                        native_processes,
                        &fingerprint,
                        model.endpoint_model.as_deref(),
                        model
                            .endpoint_url
                            .as_deref()
                            .and_then(|url| reqwest::Url::parse(url).ok())
                            .and_then(|url| url.port_or_known_default()),
                    )
                };
                pid.map(|pid| (model.card_id.clone(), pid))
            })
            .collect::<Vec<_>>();
        let mut sampler = ProcessResourceSampler::default();
        let mut sample_roots = roots.iter().map(|(_, pid)| *pid).collect::<Vec<_>>();
        sample_roots.extend(native_processes.iter().map(|process| process.pid));
        sample_roots.sort_unstable();
        sample_roots.dedup();
        let sampled = sampler.sample_trees(&sample_roots);
        let mut running_models = running_records
            .iter()
            .map(|model| {
                let pid = roots
                    .iter()
                    .find(|(card, _)| card == &model.card_id)
                    .map(|(_, pid)| *pid);
                RunningModelMemory {
                    card_id: model.card_id.clone(),
                    card_name: model.card_name.clone(),
                    rss_bytes: pid.and_then(|pid| {
                        sampled
                            .get(&pid)
                            .filter(|usage| usage.process_count > 0 && usage.rss_bytes > 0)
                            .map(|usage| usage.rss_bytes)
                    }),
                }
            })
            .collect::<Vec<_>>();
        // External native inference is still a competing workload, not a reason
        // to report "first model". Do not adopt it into the model-store database.
        for process in native_processes {
            if roots.iter().any(|(_, pid)| *pid == process.pid) {
                continue;
            }
            // A matching model name alone is NOT identity: another instance
            // may use the same weights/name on a different port. Include every
            // unmatched process conservatively instead of hiding its load.
            running_models.push(RunningModelMemory {
                card_id: format!("native-{}-{}", process.pid, process.start_ticks),
                card_name: process
                    .model_id
                    .clone()
                    .unwrap_or_else(|| "tachyon-serve".into()),
                rss_bytes: sampled
                    .get(&process.pid)
                    .filter(|usage| usage.process_count > 0 && usage.rss_bytes > 0)
                    .map(|usage| usage.rss_bytes),
            });
        }
        let candidate_peak_bytes = candidate
            .resource_requirements
            .admission_peak_mib()
            .map(|mib| mib.saturating_mul(1024 * 1024));
        let running_growth_bytes = running_records
            .iter()
            .try_fold(0u64, |sum, record| {
                let peak = record
                    .resource_requirements
                    .admission_peak_mib()?
                    .checked_mul(1024 * 1024)?;
                // RSS can double-count shared pages and omit GPU allocations.
                // Until a comparable, isolated accounting source is available,
                // reserve the whole budget rather than underestimating growth.
                sum.checked_add(peak)
            })
            .filter(|_| native_snapshot.complete && running_models.len() == running_records.len());
        let decision = match (available_bytes, safety_reserve_bytes, hard_reserve_bytes) {
            (Some(available), Some(safety), Some(hard)) => super::memory_policy::evaluate(
                available,
                safety,
                hard,
                candidate_peak_bytes,
                running_models.len(),
                running_growth_bytes,
            ),
            _ => super::memory_policy::Decision::sampling_unavailable(),
        };
        let message = match decision.reason {
            "measurement_unavailable" => "暂时无法读取系统内存，不能评估本次启动的风险。建议稍后重试；如仍要继续，请明确确认。",
            "insufficient" => "当前或预计内存低于安全下限，可能导致设备卡顿或服务退出。建议先停用其他模型。",
            "limited" => "启用后内存余量较小，已为正在运行的模型预留后续增长空间。建议先停用其他模型。",
            "sufficient" => "按当前模型配置和内存预算，设备余量充足。",
            "first_unverified" => "允许尝试启用首个模型；尚无可信峰值预算，不能保证所有负载下均有足够内存。",
            _ => "模型峰值预算或当前占用数据不完整，无法可靠预测多模型能否同时运行。建议先停用其他模型。",
        }.to_string();

        Ok(MemoryAdmission {
            card_id: candidate.card_id,
            total_bytes,
            available_bytes,
            safety_reserve_bytes,
            hard_reserve_bytes,
            candidate_peak_bytes,
            projected_available_bytes: decision.projected_available,
            running_growth_bytes,
            running_models,
            level: decision.level.into(),
            reason: decision.reason.into(),
            requires_confirmation: decision.confirmation,
            message,
        })
    }

    /// 删掉引擎本身。**后果按引擎形态分两种，措辞绝不能混用**：
    ///
    /// - **docker 镜像形态**：只 `docker rmi` 掉镜像，不动任何模型权重、部署
    ///   配置。还在用它的卡片下次下载/部署会自动重新走一遍 install_engine——
    ///   探针（LMM_PROBE_ONLY）发现引擎不在了就触发真正安装，这条探针优先的
    ///   安装流程本身是自愈的。
    /// - **ollama：连它的全部模型数据一起删。** 卸载脚本里有
    ///   `rm -rf "$TARGET_HOME/.ollama"`（见 `OLLAMA_UNINSTALL_SCRIPT`），而
    ///   ollama 的权重 blob 就在这个目录下（`~/.ollama/models`，常有几十 GB）。
    ///   这一半**不自愈**：重装 ollama 二进制不会把权重带回来，用户得把每个
    ///   ollama 模型重新下一遍。
    ///
    /// 所以返回的是 [`EngineRemoval`] 而不是一个字节数：`purged_model_data` 和
    /// `affected_models` 是这次操作的后果本身，调用方（接线那天的确认框）必须先
    /// 把"哪些模型受影响、权重会不会跟着没"摆给用户看，不能只说一句"删除引擎"。
    /// 本层再次检查模型绑定，不能只依赖界面禁用按钮。
    ///
    /// docker 镜像形态的引擎不需要 root（docker rmi 本来就是普通用户能跑的
    /// 命令）；ollama 是系统级二进制 + systemd 服务，删除必须走 pkexec 提权。
    pub fn delete_engine(
        &self,
        engine_name: &str,
        engine_version: Option<&str>,
    ) -> Result<EngineRemoval, ModelManagerError> {
        // 必须按「名字 + 版本」定位。同名不同版本是两个不同的镜像，只按名字取
        // 会拿到另一个版本的记录，然后 rmi 掉别的模型正在用的镜像。
        let rec = db::get_installed_engine(engine_name, engine_version)
            .map_err(|e| ModelManagerError::new(format!("读取引擎记录失败：{e}")))?
            .ok_or_else(|| {
                ModelManagerError::new(format!(
                    "未找到已安装的引擎记录：{engine_name} {}",
                    engine_version.unwrap_or("")
                ))
            })?;

        // 在分叉之前就把名单取出来：两条分支对它的解读不同（要重装一遍引擎 /
        // 权重被一起删掉），但两条都得把它带给调用方。`list_installed_engines`
        // 早就在算同一份名单了，删除这一侧不能算完就扔。
        //
        // 查不到绑定关系就停止，不能把数据库读取失败当成“没有模型使用”。
        let affected =
            db::list_model_names_using_engine(engine_name, engine_version).map_err(|e| {
                ModelManagerError::new(format!("无法确认引擎关联的模型，未执行删除：{e}"))
            })?;
        if !affected.is_empty() {
            return Err(ModelManagerError::new(format!(
                "该引擎仍被以下模型绑定，不能删除：{}。请先处理这些模型。",
                affected.join("、")
            )));
        }

        if let Some(image) = &rec.docker_image {
            // 本批评审 P2-5：rmi 前按镜像字符串反查引用。docker_image 的来源是
            // install_engine 输出的 LMM_ENGINE_IMAGE= 自报标记——一张被投毒的卡片
            // 可以让自己的**新**引擎行自报受害者正在用的真实镜像（形式合法就过
            // 白名单，「不覆盖」只保护同 (name,version) 已有不同值的行）。上面的
            // affected 名单只查本引擎行的绑定，对这种冒名视而不见；真放行 rmi，
            // 受害卡下次部署就得重新拉几十 GB 镜像，界面上还看不出任何异常。
            // 查不到引用名单（库读失败）同样停手：不能把读取失败当成「没人用」。
            let sharing =
                db::find_docker_image_users(image, engine_name, engine_version).map_err(|e| {
                    ModelManagerError::new(format!("无法核对镜像引用，未执行删除：{e}"))
                })?;
            if !sharing.is_empty() {
                let described = sharing
                    .iter()
                    .map(|r| {
                        let engine = match r.engine_version.as_deref() {
                            Some(v) if !v.is_empty() => format!("{} {}", r.engine_name, v),
                            _ => r.engine_name.clone(),
                        };
                        if r.model_names.is_empty() {
                            engine
                        } else {
                            format!("{engine}（模型：{}）", r.model_names.join("、"))
                        }
                    })
                    .collect::<Vec<_>>()
                    .join("；");
                return Err(ModelManagerError::new(format!(
                    "镜像 {image} 同时被以下引擎/模型引用，不能删除：{described}。\
                     请先处理这些模型，或联系卡片提供方修正引擎的镜像声明。"
                )));
            }
            let freed = docker_image_size(image);
            // 带时限。这里原来是裸 `Command::output()`，而 `output()` 是无限期
            // 阻塞的：docker daemon 卡死时 `docker rmi` 跟 `docker ps` 一样，
            // 既不报错也不退出（proc_timeout 的模块注释复盘过）。同一个函数里
            // ollama 那条分支早就设了超时，它的注释也已经预告过「接线的那天
            // 就会踩」——眼下 delete_engine 还没注册成命令，正好现在补上。
            //
            // 代价：proc_timeout 只回 Option，拿不到 docker 的 stderr 原文，
            // 失败时只能给一句概括。可以接受——「镜像还被容器占用」这类真实
            // 原因用户在 docker 侧查得到，而「永远卡住」是连查的机会都没有。
            if proc_timeout::output_within("docker", &["rmi", image], DOCKER_RMI_TIMEOUT).is_none()
            {
                return Err(ModelManagerError::new(format!(
                    "删除镜像失败：{image}（docker rmi 没有成功退出，可能仍有容器在用这个\
                     镜像，或者 docker 服务没有响应）"
                )));
            }
            db::delete_installed_engine(engine_name, engine_version)
                .map_err(|e| ModelManagerError::new(format!("删除引擎记录失败：{e}")))?;
            let follow_up = if affected.is_empty() {
                "。".to_string()
            } else {
                format!(
                    "；以下卡片下次部署会自动重新安装一遍引擎：{}。",
                    affected.join("、")
                )
            };
            return Ok(EngineRemoval {
                engine_name: engine_name.to_string(),
                engine_version: engine_version.map(str::to_string),
                freed_bytes: freed,
                purged_model_data: false,
                affected_models: affected,
                summary: format!(
                    "已删除引擎镜像 {image}，释放约 {}。模型权重和部署配置没有动{follow_up}",
                    disk::format_bytes(freed as f64),
                ),
            });
        }

        if engine_name == "ollama" {
            let freed = ollama_size();
            let script_path = write_temp_script(OLLAMA_UNINSTALL_SCRIPT)?;
            let result = script_runner::run_script(
                &script_path,
                &HashMap::new(),
                RunScriptOptions {
                    need_root: true,
                    // 跟 uninstall 同一档（LONG_RUNNING_IDLE_TIMEOUT，1800 秒）。
                    //
                    // 这个脚本是 `rm -rf ~/.ollama`：常有几十 GB，而且**全程零输出**。
                    // 原来是 60 秒绝对 + 30 秒静默——静默 30 秒就 SIGKILL，磁盘被删到
                    // 一半，而 `result?` 让引擎记录留在库里；引擎删除又没有 force_remove
                    // 那样的逃生出口，用户从此卡死。
                    //
                    // 这正是 idle_timeout_for 把 uninstall 单独提到 1800 秒的那条教训
                    // （本文件 :398 的注释里复盘过），同一个坑不该在隔壁再踩一遍。
                    // 眼下 delete_engine 还没注册成命令、用户够不着，但代码已经在库里，
                    // 接线的那天就会踩。
                    timeout: None,
                    idle_timeout: Some(LONG_RUNNING_IDLE_TIMEOUT),
                    ..Default::default()
                },
            );
            let _ = std::fs::remove_file(&script_path);
            result?;
            db::delete_installed_engine(engine_name, engine_version)
                .map_err(|e| ModelManagerError::new(format!("删除引擎记录失败：{e}")))?;
            // 这句话必须把"权重也没了"说在明处。脚本删的是整个 ~/.ollama，
            // 模型 blob 就在里面——沿用 docker 分支那套"只删引擎、下次自动装
            // 回来"的措辞，用户会以为重装一下就好，实际是要重下几十 GB。
            let follow_up = if affected.is_empty() {
                String::new()
            } else {
                format!("以下模型需要重新下载才能再用：{}。", affected.join("、"))
            };
            return Ok(EngineRemoval {
                engine_name: engine_name.to_string(),
                engine_version: engine_version.map(str::to_string),
                freed_bytes: freed,
                purged_model_data: true,
                affected_models: affected,
                summary: format!(
                    "已卸载 ollama，并删除了 ~/.ollama（释放约 {}）。\
                     该目录下的模型权重被一并删除，重装 ollama 也不会恢复。{follow_up}",
                    disk::format_bytes(freed as f64),
                ),
            });
        }

        if let Some(cleanup_script) = rec.cleanup_script.as_ref() {
            let freed = remove_native_engine(cleanup_script)?;
            db::delete_installed_engine(engine_name, engine_version)
                .map_err(|e| ModelManagerError::new(format!("删除引擎记录失败：{e}")))?;
            return Ok(EngineRemoval {
                engine_name: engine_name.to_string(),
                engine_version: engine_version.map(str::to_string),
                freed_bytes: freed,
                purged_model_data: false,
                affected_models: affected,
                summary: format!(
                    "已删除推理引擎，释放约 {}。模型权重和部署配置没有动；下次下载需要它的模型时会自动重新安装。",
                    disk::format_bytes(freed as f64),
                ),
            });
        }

        Err(ModelManagerError::new(format!(
            "该引擎尚未提供安全的独立清理方式：{engine_name}。请更新模型卡片后再试，不能通过猜测目录自动删除。"
        )))
    }

    // ---------------- 详情（基础信息 + 运行时信息） ----------------

    pub fn get_model_detail(&self, card_id: &str) -> Result<ModelDetail, ModelManagerError> {
        let rec = db::get_store_model(card_id)
            .map_err(|e| ModelManagerError::new(format!("读取模型记录失败：{e}")))?
            .ok_or_else(|| ModelManagerError::new("模型不存在"))?;
        if !rec.running {
            return Ok(ModelDetail {
                record: rec,
                runtime_lines: Vec::new(),
                runtime_note: "未启用，没有运行时数据".into(),
            });
        }
        if rec.engine_name == "ollama" {
            let lines = self.ollama_runtime_lines(&rec);
            return Ok(ModelDetail {
                record: rec,
                runtime_lines: lines,
                runtime_note: String::new(),
            });
        }
        let (runtime_lines, runtime_note) = model_runtime_lines(&rec);
        Ok(ModelDetail {
            record: rec,
            runtime_lines,
            runtime_note,
        })
    }

    fn ollama_runtime_lines(&self, rec: &db::StoreModelRecord) -> Vec<String> {
        let url = format!("{}/api/ps", self.settings.ollama_host.trim_end_matches('/'));
        let resp = reqwest::blocking::Client::builder()
            .timeout(Duration::from_secs(5))
            .build()
            .and_then(|c| c.get(&url).send());
        let Ok(resp) = resp else {
            return vec!["无法连接 Ollama，拿不到运行时信息".into()];
        };
        let Ok(data) = resp.json::<serde_json::Value>() else {
            return vec!["无法连接 Ollama，拿不到运行时信息".into()];
        };
        for m in data
            .get("models")
            .and_then(|v| v.as_array())
            .into_iter()
            .flatten()
        {
            if m.get("name").and_then(|v| v.as_str()) == Some(rec.model_name.as_str()) {
                let size_vram = m.get("size_vram").and_then(|v| v.as_u64()).unwrap_or(0);
                let mut lines = vec![format!(
                    "显存占用：{}",
                    disk::format_bytes(size_vram as f64)
                )];
                if let Some(expires) = m.get("expires_at").and_then(|v| v.as_str()) {
                    lines.push(format!("到期自动卸载时间：{expires}"));
                }
                return lines;
            }
        }
        vec!["Ollama 里没查到这个模型在跑（可能刚被切换掉了）".into()]
    }
}

/// 给一次脚本运行起个人话名字，只用于「上次没退干净、这次启动清理掉了什么」
/// 的提示文案。脚本参数里 LMM_MODEL_NAME 是用户看得懂的模型名，没有时退回
/// 卡片 id。
fn script_run_label(script_type: &str, env: &HashMap<String, String>) -> String {
    let who = env
        .get("LMM_MODEL_NAME")
        .or_else(|| env.get("LMM_CARD_ID"))
        .map(|s| s.as_str())
        .unwrap_or("模型");
    match script_type {
        "download_model" => format!("下载 {who}"),
        "install_engine" => format!("安装 {who} 需要的推理引擎"),
        "deploy" => format!("部署 {who}"),
        "enable" => format!("启用 {who}"),
        "stop" => format!("停用 {who}"),
        "uninstall" => format!("卸载 {who}"),
        other => format!("{other}（{who}）"),
    }
}

/// 递归统计（或删除）目录下的 `*.incomplete`。
fn collect_incomplete(dir: &Path, delete: bool) -> u64 {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return 0;
    };
    let mut total = 0u64;
    for entry in entries.flatten() {
        let path = entry.path();
        let Ok(meta) = entry.metadata() else { continue };
        if meta.is_dir() {
            total += collect_incomplete(&path, delete);
        } else if path.extension().and_then(|e| e.to_str()) == Some("incomplete") {
            total += meta.len();
            if delete {
                let _ = std::fs::remove_file(&path);
            }
        }
    }
    total
}

fn format_quota(q: f64) -> String {
    if q.fract() == 0.0 {
        format!("{}", q as i64)
    } else {
        format!("{q}")
    }
}

/// 一批目录此刻合计占多少字节（不存在的算 0）。
///
/// 卸载前后各调一次，差值就是**实测**释放量。为什么不用 `statvfs` 去看整盘可用
/// 空间的变化：那是全机共享的数字，卸载那几分钟里别的进程（另一张卡在下权重、
/// 容器在写日志）随时会把它搅浑，算出来可能是负数，也可能把别人释放的空间算到
/// 这次卸载头上。只量这张卡自己的目录，宁可漏不肯多。
fn total_size(paths: &[PathBuf]) -> u64 {
    paths.iter().map(|p| disk::dir_size_bytes(p)).sum()
}

/// 从脚本输出里提取 `PREFIX=value` 形式的约定标记行。
fn extract_marker(output_lines: &[String], prefix: &str) -> Option<String> {
    let mut found = None;
    for line in output_lines {
        if let Some(rest) = line.strip_prefix(prefix) {
            let v = rest.trim();
            if !v.is_empty() {
                found = Some(v.to_string());
            }
        }
    }
    found
}

/// `LMM_ENGINE_IMAGE=` marker 的长度上限（本批评审 P2-5）。真实镜像引用最长
/// 也就 `registry/repo:tag@sha256:<64 hex>` 一两百字节的量级，512 已是数倍
/// 宽裕；没有上限的话，脚本不小心 echo 出的一整行日志也会被当成候选镜像
/// 走一遍白名单与查库比对。
const ENGINE_IMAGE_MARKER_MAX_BYTES: usize = 512;

/// `LMM_ENGINE_IMAGE=` 自报值的形式判据（r19 评审 P2-1）。
///
/// 这个值最终会写进 installed_engines 里 (name,version) 相同的共享行，并由
/// `delete_engine` 拿去拼 `docker rmi <image>`——而 install_engine 的 stdout 是
/// 卡片作者可控的输入，一行伪造的 marker 就能把别的卡正在用的镜像名覆盖掉，
/// 之后的「删除引擎」会 rmi 掉被冒名的镜像。白名单收口：
///
/// - 首字符必须是字母数字：`-` 开头会被 docker 当成选项吞掉（`docker rmi -f`
///   就是这么拼出来的），`/`、`:`、`@`、`.` 开头说明没有 repo 名；
/// - 字符集只放行镜像引用（`registry/repo:tag@sha256:…`）会用到的
///   `[A-Za-z0-9._:/@-]`：空白、引号、`$`、`;`、`|` 一律出局——镜像引用是
///   一个 token，混进这些字符它就不是镜像名而是别的什么东西了。
/// - 长度不得超过 [`ENGINE_IMAGE_MARKER_MAX_BYTES`]（本批评审 P2-5）。
///
/// 刻意停在「形式像不像」这一层，不去联网验证镜像真的存在：那要花几十秒，
/// 不该挡在下载主流程上。形式不合法的值在这里挡掉；「合法但编造」的值有两道
/// 后手——同 (name,version) 行已有不同镜像时由 [`engine_image_to_record`] 的
/// 不覆盖规则保住旧值，**新**引擎行自报别人正在用的镜像则由 `delete_engine`
/// 删除前的镜像引用反查（`db::find_docker_image_users`，本批评审 P2-5）拦下
/// rmi。此前注释宣称「由不覆盖规则兜住」，那句话不成立：不覆盖只保护同
/// (name,version) 已有不同值的行，冒名者开新行就绕开了它。
fn engine_image_marker_is_wellformed(value: &str) -> bool {
    if !value.starts_with(|c: char| c.is_ascii_alphanumeric()) {
        return false;
    }
    if value.len() > ENGINE_IMAGE_MARKER_MAX_BYTES {
        return false;
    }
    value
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-' | '/' | ':' | '@'))
}

/// 算出这次要登记进 installed_engines 的镜像名（r19 评审 P2-1）。
///
/// 两道保守防线：
/// 1. 形式不合法的 marker 直接丢弃并留日志，绝不让它靠近 docker rmi；
/// 2. 引擎行已有**不同**的镜像名时不覆盖、保留旧值——两个名字都是卡片脚本
///    自报的、没有第三方权威，但旧值至少跟着这行记录被用过，新值可能只是
///    另一张卡的脚本在抢写共享行。返回 None 即可：`mark_engine_installed`
///    的 COALESCE 会保留库里的旧值。这比弹框让用户裁决侵入性小，先做保守版。
///    注意这一道只保护同 (name,version) 且已有不同值的行——**新**引擎行自报
///    别的卡正在用的镜像拦不住，那一路由 `delete_engine` 删除前的镜像引用
///    反查兜底（本批评审 P2-5，见 [`db::find_docker_image_users`]）。
///
/// 空值（ollama、厂商二进制引擎都不报镜像）维持原行为，同样交给 COALESCE。
fn engine_image_to_record(
    engine_name: &str,
    engine_version: Option<&str>,
    marker: Option<String>,
) -> Option<String> {
    let candidate = marker?;
    if !engine_image_marker_is_wellformed(&candidate) {
        eprintln!(
            "[engine] 丢弃形式不合法的 LMM_ENGINE_IMAGE 标记（{engine_name}）：{candidate:?}"
        );
        return None;
    }
    if let Ok(Some(existing)) = db::get_installed_engine(engine_name, engine_version) {
        if let Some(old) = existing.docker_image {
            if old != candidate {
                eprintln!(
                    "[engine] 引擎 {engine_name} 已登记镜像 {old}，忽略脚本自报的不同镜像 \
                     {candidate}，保留原记录"
                );
                return None;
            }
        }
    }
    Some(candidate)
}

/// 用厂商随引擎提供的独立清理脚本做只读探测。
///
/// 约定：`LMM_PROBE_ONLY=1` 时，输出 `LMM_ENGINE_PRESENT=0/1` 并退出 0，
/// 存在时可选输出 `LMM_ENGINE_SIZE_BYTES=<bytes>`；非 0 表示探测失败而非不存在。这样“模型已经删了、
/// 引擎仍在”的场景不必借一张模型卡片来猜目录，也不会扫描/删除用户任意路径。
fn native_engine_cleanup_probe(
    script: &db::ScriptBlob,
) -> Result<(bool, Option<u64>), ModelManagerError> {
    if script_runner::sha256_hex(&script.content) != script.sha256 {
        return Err(ModelManagerError::new(
            "引擎清理脚本校验失败，请刷新卡片后重试。",
        ));
    }
    let script_path = write_temp_script(&script.content)?;
    let mut env = HashMap::new();
    env.insert("LMM_PROBE_ONLY".to_string(), "1".to_string());
    let result = script_runner::run_script(
        &script_path,
        &env,
        RunScriptOptions {
            label: Some("检查本机推理引擎".to_string()),
            timeout: Some(Duration::from_secs(30)),
            idle_timeout: Some(Duration::from_secs(10)),
            ..Default::default()
        },
    );
    let _ = std::fs::remove_file(&script_path);
    let lines = result?;
    let present = match extract_marker(&lines, "LMM_ENGINE_PRESENT=").as_deref() {
        Some("1") => true,
        Some("0") => false,
        _ => {
            return Err(ModelManagerError::new(
                "无法确认推理引擎是否存在，已保留管理记录。",
            ));
        }
    };
    Ok((
        present,
        extract_marker(&lines, "LMM_ENGINE_SIZE_BYTES=")
            .and_then(|value| value.parse::<u64>().ok()),
    ))
}

/// 执行已经固化的原生引擎清理契约，并以同一份脚本后验确认实体确实消失。
///
/// 不能在此处根据 `$HOME` / 目录名自行拼 `rm -rf`：唯一获准删除的路径必须由
/// 随该引擎版本签发并校验过的脚本决定。
fn remove_native_engine(script: &db::ScriptBlob) -> Result<u64, ModelManagerError> {
    let (present, _) = native_engine_cleanup_probe(script)?;
    if !present {
        return Ok(0);
    }
    let script_path = write_temp_script(&script.content)?;
    let result = script_runner::run_script(
        &script_path,
        &HashMap::new(),
        RunScriptOptions {
            label: Some("删除本机推理引擎".to_string()),
            timeout: Some(LONG_RUNNING_IDLE_TIMEOUT),
            idle_timeout: Some(LONG_RUNNING_IDLE_TIMEOUT),
            ..Default::default()
        },
    );
    let _ = std::fs::remove_file(&script_path);
    let lines = result?;
    if native_engine_cleanup_probe(script)?.0 {
        return Err(ModelManagerError::new(
            "推理引擎清理脚本已经结束，但后验检查发现引擎文件仍在；为避免留下无法管理的残留，未移除引擎记录。",
        ));
    }
    Ok(extract_marker(&lines, "LMM_ENGINE_FREED_BYTES=")
        .and_then(|value| value.parse::<u64>().ok())
        .unwrap_or(0))
}

/// 卡片的 enable 脚本明确说部署配置缺失时，数据库中的“已部署”就是陈旧状态。
///
/// 这里不能把任意 enable 失败都降级：显存不足、端口冲突、镜像故障都可能在一次
/// 重试或排障后恢复，清掉部署状态反而会误导用户。只有脚本同时提到部署凭据及
/// "missing/not found/缺少" 这类缺失事实时才自愈。
fn deployment_receipt_is_missing(message: &str) -> bool {
    let lower = message.to_ascii_lowercase();
    let mentions_receipt = lower.contains(".deploy-env")
        || lower.contains("deployment configuration")
        || lower.contains("deployment config");
    let says_missing = lower.contains("missing")
        || lower.contains("not found")
        || message.contains('缺')
        || message.contains("缺少")
        || message.contains("不存在");
    mentions_receipt && says_missing
}

/// deploy 脚本已经确认权重不完整时，数据库中的“已下载”就是陈旧状态。
///
/// 只识别“模型/权重 + 不完整/缺失”这一组强信号；不能把网络、显存、端口等普通
/// 部署故障降级成下载问题，否则用户会被错误引导去重下几十 GB。
fn model_weights_are_incomplete(message: &str) -> bool {
    let lower = message.to_ascii_lowercase();
    let explicit_english = [
        "weights incomplete",
        "weight files incomplete",
        "model weights are incomplete",
        "model directory is incomplete",
        "model files are incomplete",
        "model weights missing",
        "missing model weights",
    ];
    explicit_english.iter().any(|needle| lower.contains(needle))
        || [
            "模型权重不完整",
            "权重不完整",
            "模型文件不完整",
            "权重缺失",
            "请先下载模型",
        ]
        .iter()
        .any(|needle| message.contains(needle))
}

/// enable 脚本把引擎的原始末尾日志一并带回来了。对于“权重已加载、服务在最后
/// 初始化阶段退出”的情况，既可能是引擎自己的 `startup failed`，也可能已被
/// `errors::humanize_script_message` 归一成中文；两种形态都要识别到。
fn engine_startup_failed(message: &str) -> bool {
    let lower = message.to_ascii_lowercase();
    lower.contains("server failed during startup")
        || lower.contains("startup failed")
        || message.contains("推理服务启动失败")
        || message.contains("推理服务启动未完成")
}

/// 卡片报上来的对外调用地址是不是指向**本机**。
///
/// 这把尺子在三个地方必须一模一样：入库（本函数）、接入（前端
/// `pinvou-model-link.js` 的 `isLocalGatewayUrl`，「在品悟里使用」按钮）、
/// 卸载清理（同文件的 `findLinkedRows`）。尺子不一致本身就是漏洞——宽的那一侧
/// 放进来、窄的那一侧清不掉，正好凑出一行「存得进、接得上、删不掉」的死数据，
/// 而它又恰好是会被设成当前模型的那一行。
///
/// 实现刻意跟 `features/assistant/platform/bridge.rs` 的 `base_url_uses_loopback`
/// 逐字对齐（品悟判「这个端点算不算本机」用的就是它）。**没有直接 use 它**：
/// model_store → assistant 会新增一条跨 feature 依赖边，而
/// `scripts/architecture-guard.py` 盯着那张图（rust_cyclic_feature_dependencies），
/// 何况那个函数是 assistant 的 `pub(crate)` 内部件。仓库既有做法就是「各自一份 +
/// 注释互指」——`features/settings/model-catalog.js` 里 `baseUrlUsesLoopback` 开头
/// 那句「对齐 Rust bridge.rs」是同一个写法。改这里请把那几处一起改。
///
/// 判据走 `IpAddr::is_loopback()` 而不是列举字面量：回环不止 `127.0.0.1`，整个
/// `127.0.0.0/8` 都是（真机上 sglang/vllm 确实有绑到 `127.0.0.2` 的用法，只认
/// 那一个字面量会把合法卡片一起拒掉）。反过来也别放宽到 RFC1918 私网段——
/// 那已经是「另一台机器」，对话内容就出本机了。
fn endpoint_url_is_loopback(url: &str) -> bool {
    reqwest::Url::parse(url)
        .ok()
        .and_then(|parsed| parsed.host_str().map(str::to_string))
        .is_some_and(|host| {
            // IPv6 的 host_str 带方括号（`[::1]`），不是裸的 `::1`，直接 parse 会失败。
            let host = host.trim_start_matches('[').trim_end_matches(']');
            // 末尾那个点是 FQDN 的合法写法（`localhost.`），不剥掉就绕过去了。
            //
            // **只剥一个**：合法的 FQDN 最多带一个结尾点，`localhost..` 不是合法写法。
            // 这里原先用 `trim_end_matches('.')` 剥光全部结尾点，而 JS 侧
            // `pinvou-model-link.js` 的 `isLocalGatewayUrl` 只剥一个——于是
            // `http://localhost../v1` 会被 Rust 判 true（存得进库）、被 JS 判 false
            // （接不上，也走不到清理那条路）。方向虽在安全侧，但两把尺子不一样这件事
            // 本身迟早要咬人，统一成严格的那一把。
            let host = host.strip_suffix('.').unwrap_or(host);
            host.eq_ignore_ascii_case("localhost")
                || host
                    .parse::<std::net::IpAddr>()
                    .is_ok_and(|address| address.is_loopback())
        })
}

/// ollama 服务在不在。
///
/// 只问「服务活着吗」，不问「哪个模型加载着」——后者会随空闲卸载来回变，
/// 拿它纠正 running 会把正常的省显存行为报成故障（见 reconcile_running_status
/// 里那段注释）。
///
/// 超时跟同文件其余查询一致（5 秒）。失败分两类下结论，**不是一律当活着**：
///
/// - **确定性失败**（连接被拒 / 被重置）：那个端口后面根本没有在监听的进程，
///   这台机器上的 ollama 模型一个都不可能在服务，判「死」。
/// - **不确定失败**（超时、DNS 查不到、建不出客户端……）：判「活着」，不动
///   running。置错的代价是用户明明启用着却被告知没启用，「问不出来」离「确定
///   没在跑」还差得远。
///
/// 这个分界是补回来的，原来是 `Err(_) => true` 一刀切。一刀切的问题在于
/// **连不上恰恰是「ollama 已经停了」的主场景**：服务停掉、机器重启过，探测拿到的
/// 就是 ConnectionRefused，于是 `reconcile_running_status` 的 ollama 分支永远不
/// 生效，库里的 running 永远停在 true。连带受害的是 service.rs 里「运行中不许
/// 卸载」那条护栏——它读的就是这个 running，用户会被一直挡在「请先停用再卸载」
/// 上，而那个服务早就没在跑了；点「停用」还得先把一个已经不存在的服务停一遍。
///
/// 判据本身抽成了 [`probe_failure_means_nothing_listening`]，可以直接喂错误链
/// 单测，不用真发请求。
fn ollama_alive(host: &str) -> bool {
    let url = format!("{}/api/tags", host.trim_end_matches('/'));
    let ok = reqwest::blocking::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        // 这一探测必须直连。Cargo.toml 给 reqwest 开了 `system-proxy`，默认会读
        // 系统代理设置，而 ollama_host 默认就是 127.0.0.1——本机地址走代理没有
        // 任何意义，代理没起来时还会把「代理连不上」误报成「ollama 死了」，正好
        // 撞在上面新加的确定性判据上。
        .no_proxy()
        .build()
        .and_then(|c| c.get(&url).send())
        .map(|r| r.status().is_success());
    match ok {
        Ok(alive) => alive,
        Err(e) => !probe_failure_means_nothing_listening(&e),
    }
}

/// `/api/ps` 里这个模型当前是否仍被加载（r21 P2-5）。
///
/// 三态：`Some(true)` 在跑；`Some(false)` 服务在但这个模型未加载；`None` 查不动
/// （请求失败/应答不是合法 JSON——保守侧）。与 reconcile 的 ollama 分支刻意不是
/// 同一把尺子：那边纠 running 标志不能用 /api/ps（ollama 空闲 5 分钟自动把模型
/// 卸出显存，按它纠会把正常待机报成故障，见 reconcile 里的注释）；这边是破坏性
/// 操作（删权重/换脚本）前的停验，问的就是「此刻在不在显存里」——正是 /api/ps
/// 回答的问题，与 [`ModelManager::ollama_runtime_lines`] 的匹配口径一致（按
/// `name` 全等比对 `model_name`）。
fn ollama_model_loaded(host: &str, model_name: &str) -> Option<bool> {
    let url = format!("{}/api/ps", host.trim_end_matches('/'));
    let response = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(5))
        .no_proxy()
        .build()
        .and_then(|client| client.get(&url).send())
        .ok()?;
    let data = response.json::<serde_json::Value>().ok()?;
    let models = data.get("models")?.as_array()?;
    Some(
        models
            .iter()
            .any(|m| m.get("name").and_then(|v| v.as_str()) == Some(model_name)),
    )
}

/// 原生引擎给模型商店的 endpoint 是 OpenAI 兼容的 chat-completions 地址。
/// 运行状态探测不能向它发一次聊天请求（那会触发真正推理），只访问轻量的
/// `/v1/models`。无法确定的网络错误一律保留原状态，避免一时负载过高就把
/// 正在运行的服务错标成已停止。
fn endpoint_path_is_openai_style(endpoint_url: &str) -> bool {
    let Ok(url) = reqwest::Url::parse(endpoint_url) else {
        // 解析不了的地址不探测，保留原状态。
        return false;
    };
    let path = url.path();
    path.is_empty() || path == "/" || path.starts_with("/v1")
}

fn openai_compatible_endpoint_alive(endpoint_url: &str) -> bool {
    let Ok(mut url) = reqwest::Url::parse(endpoint_url) else {
        return true;
    };
    url.set_path("/v1/models");
    url.set_query(None);
    let ok = reqwest::blocking::Client::builder()
        // 2 秒（r19 评审 P2-7，由 5 秒收敛）：这个探测串行发生在 reconcile
        // （library() 的第一步）里，K 张卡就是 K×超时。/v1/models 是本机或
        // 局域网推理端点的轻量请求，正常路径毫秒级返回，2 秒已经足够覆盖
        // 「确实没响应」之外的一切慢。而超时属于"不确定"失败，
        // probe_failure_means_nothing_listening 判它为活着、保留原状态——
        // 收紧超时不会把重负载下响应慢的端点误纠正成已停止。
        .timeout(Duration::from_secs(2))
        .no_proxy()
        .build()
        .and_then(|client| client.get(url).send())
        .map(|response| response.status().is_success());
    match ok {
        Ok(alive) => alive,
        Err(error) => !probe_failure_means_nothing_listening(&error),
    }
}

/// [`openai_compatible_endpoint_alive`] 的带缓存包装（r19 评审 P2-7）。
///
/// 命中缓存（60 秒内探到过端点活着）直接返回 true；**只有探测成功才写
/// 缓存**，失败不缓存——一条过期的死讯会把刚恢复的服务挡到 TTL 到期，
/// 而多探一次的成本只是一次毫秒级的 /v1/models。
fn openai_endpoint_alive_cached(card_id: &str, endpoint_url: &str) -> bool {
    let cache = RECONCILE_ALIVE_ENDPOINTS.get_or_init(Default::default);
    if let Ok(cached) = cache.lock() {
        if let Some(seen_at) = cached.get(card_id) {
            if seen_at.elapsed() < RECONCILE_PROBE_CACHE_TTL {
                return true;
            }
        }
    }
    let alive = openai_compatible_endpoint_alive(endpoint_url);
    if alive {
        if let Ok(mut cached) = cache.lock() {
            cached.insert(card_id.to_string(), Instant::now());
        }
    }
    alive
}

/// 任务 G（收编）：读卡片 `.runtime/serve.identity.json` 核验进程身份。
///
/// 端点活着只能说明「这个端口上有服务」，不能说明是我们的实例——身份核验
/// 靠 PID 存活检查。`.runtime` 目录由 enable.sh 写入（卡片脚本自管），格式
/// `{"version":1, "process":{"pid":N, "start_ticks":N, "uid":N}, ...}`。
/// 文件缺失/损坏/PID 不活 → false（不认领，可能是外来服务或残留端口占用）。
fn native_engine_identity_alive(card_dir: &std::path::Path) -> bool {
    let Some(identity) = native_identity_process(card_dir) else {
        return false;
    };
    identity_process_alive(&identity)
}

/// /proc/{pid}/stat 存在 = 进程活着；不需要解析内容。
fn pid_alive(pid: i64) -> bool {
    std::path::Path::new(&format!("/proc/{pid}/stat")).exists()
}

/// identity 文件里记录的进程身份。start_ticks/uid 老文件可能缺——缺了就
/// 只能退回「PID 在不在」，带了就比对（见 [`identity_process_alive`]）。
struct NativeIdentity {
    pid: i64,
    start_ticks: Option<u64>,
    uid: Option<u32>,
}

/// 从 `.runtime/serve.identity.json` 读出记录的进程身份；文件缺失/损坏/没有
/// 合法 PID 都返回 None（调用方据此判定「无身份」，不认领、不编数据）。
fn native_identity_process(card_dir: &std::path::Path) -> Option<NativeIdentity> {
    let identity_path = card_dir.join(".runtime").join("serve.identity.json");
    let content = std::fs::read_to_string(&identity_path).ok()?;
    let value = serde_json::from_str::<serde_json::Value>(&content).ok()?;
    let pid = value["process"]["pid"].as_i64()?;
    if pid <= 0 {
        return None;
    }
    Some(NativeIdentity {
        pid,
        start_ticks: value["process"]["start_ticks"].as_u64(),
        uid: value["process"]["uid"].as_u64().map(|u| u as u32),
    })
}

/// PID 存在**不等于**还是那个进程——PID 会被内核复用（r18 P2-2：仅凭 PID
/// 文件判活违反 native_process 自己立的规矩，`matching_pid` 在 60 行外就有
/// 现成机制）。identity 带了 start_ticks/uid 就比对：start_ticks 换了 =
/// 同号新进程；uid 换了 = 别的用户起的。老 identity 缺字段时保守退回仅
/// PID 存在（比误杀好——这条只影响「认领」方向，正向纠正另有端点判据）。
fn identity_process_alive(identity: &NativeIdentity) -> bool {
    if !pid_alive(identity.pid) {
        return false;
    }
    let stat = match std::fs::read_to_string(format!("/proc/{pid}/stat", pid = identity.pid)) {
        Ok(stat) => stat,
        Err(_) => return false,
    };
    if let Some(expected) = identity.start_ticks {
        if super::native_process::start_ticks(&stat) != Some(expected) {
            return false;
        }
    }
    if let Some(expected) = identity.uid {
        match std::fs::metadata(format!("/proc/{pid}", pid = identity.pid)) {
            Ok(meta) => {
                use std::os::unix::fs::MetadataExt;
                if meta.uid() != expected {
                    return false;
                }
            }
            Err(_) => return false,
        }
    }
    true
}

/// `docker ps -a --format '{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Label ...}}'`
/// 的一行。`running` 只认 `state == "running"`（restarted/paused 都不算在服务）；
/// compose 项目名是 `docker run` 起的容器和外来的容器没有的标签，用 None 表示。
struct DockerPsRow {
    name: String,
    image: String,
    running: bool,
    compose_project: Option<String>,
}

fn parse_docker_ps_a(stdout: &str) -> Vec<DockerPsRow> {
    stdout
        .lines()
        .filter(|line| !line.trim().is_empty())
        .map(|line| {
            let mut parts = line.splitn(4, '\t');
            let name = parts.next().unwrap_or_default().trim().to_string();
            let image = parts.next().unwrap_or_default().trim().to_string();
            let state = parts.next().unwrap_or_default().trim();
            let project = parts
                .next()
                .map(|p| p.trim().to_string())
                .filter(|p| !p.is_empty());
            DockerPsRow {
                name,
                image,
                running: state == "running",
                compose_project: project,
            }
        })
        .collect()
}

/// `docker ps -a` 喂给 [`parse_docker_ps_a`] 的四字段格式（名字、镜像、状态、
/// compose 项目名）——解析端按这个顺序 splitn(4)。
///
/// 抽成常量给所有查询共用是 r19 P1-3 的教训：此前「自有容器判停」带镜像
/// 字段、「详情页容器名」没带，后者的三个字段被 splitn(4) 整体左移解析
/// （state 吃到项目标签、compose_project 恒 None），compose 卡（容器名 ≠
/// card_id）在详情页拿不到自己的容器名。共用一份，格式再漂移单测就能抓住。
const DOCKER_PS_A_FORMAT: &str =
    "{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Label \"com.docker.compose.project\"}}";

/// `require_actually_stopped` 对「无 container_name 的 docker 卡」的结论。
enum DockerOwnedContainerState {
    /// 找到本卡容器且有运行中的。
    Running,
    /// 找到本卡容器但全部处于退出/暂停等非运行状态。
    Stopped,
    /// 没有任何容器以本卡 card_id 为 compose 项目名或容器名。
    Absent,
    /// docker 查不动（超时/未装），下不了结论。
    Unknown,
}

fn docker_owned_container_state(card_id: &str) -> DockerOwnedContainerState {
    // 四字段格式与 parse_docker_ps_a 的解析顺序对齐，见 DOCKER_PS_A_FORMAT。
    let Some(stdout) = proc_timeout::query("docker", &["ps", "-a", "--format", DOCKER_PS_A_FORMAT])
    else {
        return DockerOwnedContainerState::Unknown;
    };
    let rows = parse_docker_ps_a(&stdout);
    owned_container_state_from_rows(&rows, card_id)
}

fn owned_container_state_from_rows(
    rows: &[DockerPsRow],
    card_id: &str,
) -> DockerOwnedContainerState {
    let mut found = false;
    let mut any_running = false;
    for row in rows {
        let owned = row.compose_project.as_deref() == Some(card_id) || row.name == card_id;
        if !owned {
            continue;
        }
        found = true;
        any_running = any_running || row.running;
    }
    if !found {
        DockerOwnedContainerState::Absent
    } else if any_running {
        DockerOwnedContainerState::Running
    } else {
        DockerOwnedContainerState::Stopped
    }
}

/// 原生引擎进程是否属于本卡（r19 P1-1）。
///
/// 进程命令行里的 `--model-id` 是推理服务真正认的模型 id——对应记录里的
/// `endpoint_model`，**不是**展示名 `model_name`（两者六张卡里没有一张相等，
/// 见 `ForcedRemoval::endpoint_model` 的注释；内存准入喂给 matching_pid 的
/// 也是 endpoint_model）。此前拿 model_name 比对，驭光卡 stop 半成功（DB 已
/// running=0、进程还活）时活实例被判「非本卡」放行，purge 会在活进程脚下
/// 删权重。两者任一命中都算本卡：老记录可能没有 endpoint_model，留展示名
/// 兜底。
fn native_process_owned_by_card(
    process_model_id: Option<&str>,
    endpoint_model: Option<&str>,
    model_name: &str,
) -> bool {
    let Some(id) = process_model_id else {
        // 归属无法确认（进程无 --model-id）：保守起见仍然拦。
        return true;
    };
    Some(id) == endpoint_model || Some(id) == Some(model_name)
}

/// deployed 态、无 container_name 的卡该用哪条判停依据（r19 P1-2）。
///
/// ollama 卡既不是容器也没有 docker 镜像：走 docker 自有容器判据只会得到
/// Absent，再因 ollama 引擎登记不出 docker_image 兜不回来，恒被「无法确认」
/// 拒绝，停用后更新/卸载全部必拒。抽成纯函数是为了钉住「ollama 不落 docker
/// 分支」这条路径——ollama_alive 要真发 HTTP，单测里 mock 不了。
enum DeployedStoppedEvidence {
    /// ollama 卡：判据是 ollama 服务本身在不在（与 reconcile_running_status
    /// 的 ollama 分支同口径：服务不在 = 任何 ollama 模型都不可能在服务）。
    OllamaService,
    /// 其余（docker 形态）卡：判据是 compose 项目名/容器名 == card_id 的
    /// 容器状态（[`docker_owned_container_state`]）。
    OwnedContainer,
}

fn deployed_stopped_evidence(engine_name: &str) -> DeployedStoppedEvidence {
    if engine_name == "ollama" {
        DeployedStoppedEvidence::OllamaService
    } else {
        DeployedStoppedEvidence::OwnedContainer
    }
}

/// 一次 HTTP 探测的失败，是否**确定**说明「那个地址后面没有服务」。
///
/// 只认底层 IO 错误的 `ErrorKind`，不看错误文案：文案会随 reqwest/hyper 版本变，
/// `ErrorKind` 是 std 的稳定契约。必须**沿着错误链往下挖**——reqwest 0.13 把
/// `std::io::Error` 包在 hyper 的错误里，顶层那个 `reqwest::Error` 自己没有
/// kind，只 downcast 顶层的话一条都命中不了（那等于悄悄退回成「永远返回活着」，
/// 而且编译和类型都拦不住这种退化，所以单测里专门钉了一条多层嵌套的用例）。
///
/// 只收两种：
/// - `ConnectionRefused`：内核直接回的 RST，那个端口上没有监听者，没有第二种解释。
/// - `ConnectionReset`：覆盖中间隔着端口转发的情形（docker-proxy、systemd socket
///   activation），转发层会先把 TCP 接下来、发现后端没人再断开，报上来的不是
///   refused 而是 reset。
///
/// 刻意**不**收 `TimedOut` / `HostUnreachable` / DNS 失败：那些是「问不出来」，
/// 网络抖一下、机器负载高一点都会出现，据此把 running 置 false 就是误纠。
fn probe_failure_means_nothing_listening(err: &(dyn std::error::Error + 'static)) -> bool {
    let mut cursor = Some(err);
    while let Some(current) = cursor {
        if let Some(io) = current.downcast_ref::<std::io::Error>() {
            return matches!(
                io.kind(),
                std::io::ErrorKind::ConnectionRefused | std::io::ErrorKind::ConnectionReset
            );
        }
        cursor = current.source();
    }
    false
}

fn docker_image_size(image: &str) -> u64 {
    // 同样带超时。这个函数用在「已安装引擎」列表上，一个镜像一次调用——
    // daemon 挂了的话，不设时限就是列表有几行就卡几次。
    proc_timeout::query(
        "docker",
        &["image", "inspect", image, "--format", "{{.Size}}"],
    )
    .and_then(|s| s.trim().parse::<u64>().ok())
    .unwrap_or(0)
}

/// 只读取由本模型商店登记的容器 PID，供内存准入采样进程树使用。
///
/// 失败时返回 None：容器可能恰好在停止、docker 也可能短暂不可用；这种情况下前端
/// 会显示“当前占用暂不可读取”，但仍按系统 `MemAvailable` 做保守判断。
fn docker_container_pid(container_name: &str) -> Option<u32> {
    proc_timeout::query(
        "docker",
        &["inspect", "--format", "{{.State.Pid}}", container_name],
    )
    .and_then(|pid| pid.trim().parse::<u32>().ok())
    .filter(|pid| *pid > 0)
}

/// ollama 的引擎与全部模型数据（权重 blob 在 `models/` 子目录）都放在
/// `$HOME/.ollama` 下。统计大小（`ollama_size`）和残留测量
/// （`candidate_paths`）都要落到这一个目录上，解析逻辑只此一份。
fn ollama_home_dir() -> Option<PathBuf> {
    std::env::var("HOME")
        .ok()
        .map(|home| PathBuf::from(home).join(".ollama"))
}

fn ollama_size() -> u64 {
    let mut total = 0u64;
    for p in ["/usr/local/bin/ollama", "/usr/bin/ollama"] {
        if let Ok(meta) = std::fs::metadata(p) {
            total += meta.len();
        }
    }
    if let Some(ollama_dir) = ollama_home_dir() {
        if ollama_dir.is_dir() {
            total += disk::dir_size_bytes(&ollama_dir);
        }
    }
    total
}

fn gpu_runtime_lines() -> Vec<String> {
    // nvidia-smi 在驱动异常时会挂住不返回（真机上驱动重装期间遇到过）。
    // 这条是详情弹窗里的「运行时信息」，拿不到就是少几行字，绝不该把弹窗卡死。
    let Some(stdout) = proc_timeout::query(
        "nvidia-smi",
        &[
            "--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw",
            "--format=csv,noheader",
        ],
    ) else {
        return vec!["无法读取 GPU 状态（nvidia-smi 不可用）".into()];
    };
    let line = stdout.trim();
    if line.is_empty() {
        return vec!["无法读取 GPU 状态".into()];
    }
    let parts: Vec<&str> = line.split(',').map(|p| p.trim()).collect();
    let labels = ["GPU 型号", "利用率", "显存已用", "显存总量", "温度", "功耗"];
    labels
        .iter()
        .zip(parts.iter())
        // GB10 这类统一内存设备上显存两行是 [N/A]——留着就是两行空数据
        // （2026-10-27 用户反馈），设备不支持的指标直接不出。
        .filter(|(_, value)| !gpu_value_unsupported(value))
        .map(|(label, value)| format!("{label}：{value}"))
        .collect()
}

/// nvidia-smi 的这个字段值是否为「设备不支持此指标」的占位。
fn gpu_value_unsupported(value: &str) -> bool {
    let v = value.trim().to_ascii_lowercase();
    matches!(
        v.as_str(),
        "" | "n/a" | "[n/a]" | "[not supported]" | "not supported"
    )
}

/// 运行中模型的真实时画像（2026-10-27 用户反馈重做）：以前这里只有设备级
/// GPU 六行，GB10 这类统一内存设备上「显存已用/总量」是 [N/A]，用户看到的
/// 就是两行空数据。现在按优先级拼三段真实数据，全部即时实测：
///
/// 1. **本模型的进程**——docker 卡经 `docker inspect` 拿容器 PID/健康状态、
///    `docker stats` 拿容器内存；原生引擎经 `.runtime/serve.identity.json`
///    拿 PID、/proc 拿主进程 RSS。没有身份可查（如刚认领、无 identity 的
///    老卡）就整段跳过，不编数据。
/// 2. **端点实测**——OpenAI 风格端点发一次 /v1/models，报告响应耗时和
///    端点里的模型清单。/generate 这类非 OpenAI 路径改写成 /v1/models 必
///    404，探了只会误导，跳过。
/// 3. **GPU 整体**——nvidia-smi，过滤设备不支持的行（[N/A]）。
fn model_runtime_lines(rec: &db::StoreModelRecord) -> (Vec<String>, String) {
    let mut lines = Vec::new();

    if let Some(container) = owned_container_name(rec) {
        lines.push(format!("运行方式：Docker 容器 {container}"));
        if let Some((pid, state, health)) = docker_container_runtime(&container) {
            let mut state_desc = state;
            if let Some(h) = health {
                state_desc = format!("{state_desc} · {h}");
            }
            lines.push(format!("容器状态：{state_desc}"));
            if pid > 0 {
                lines.push(format!("进程 PID：{pid}"));
                push_uptime_and_rss(&mut lines, pid);
            }
        }
        if let Some(mem) = docker_container_memory(&container) {
            lines.push(format!("内存占用：{mem}（容器合计，实时）"));
        }
    } else if let Some(pid) = native_runtime_pid(rec).filter(|pid| pid_alive(*pid)) {
        lines.push("运行方式：原生引擎进程".into());
        if pid > 0 {
            lines.push(format!("进程 PID：{pid}"));
            push_uptime_and_rss(&mut lines, pid);
        }
    }

    if let Some(endpoint) = rec.endpoint_url.as_deref() {
        if endpoint_path_is_openai_style(endpoint) {
            match probe_openai_endpoint(endpoint) {
                OpenAiProbe::Ok { latency_ms, models } => {
                    lines.push(format!("服务状态：正常（/v1/models 响应 {latency_ms}ms）"));
                    if !models.is_empty() {
                        lines.push(format!("端点模型：{}", models.join("、")));
                    }
                }
                // 连接被拒是确定性的「没在服务」，照实说；超时/DNS 之类「问不出来」
                // 的一行都不加——加了就是把不确定当结论。
                OpenAiProbe::Refused => lines.push("服务状态：端点无响应（连接被拒）".into()),
                OpenAiProbe::Unknown => {}
            }
        }
    }

    let gpu = gpu_runtime_lines();
    let has_gpu = !gpu.is_empty();
    lines.extend(gpu);
    let note = if has_gpu {
        "进程/内存/端点为本模型实时实测；GPU 行是设备整体状态，仅供参考。".into()
    } else {
        String::new()
    };
    (lines, note)
}

/// 本卡自有的 docker 容器名：优先用 DB 里启用脚本报上来的 container_name，
/// 没有的话按 compose 项目名/容器名 == card_id 找（与 reconcile 同一套身份）。
fn owned_container_name(rec: &db::StoreModelRecord) -> Option<String> {
    if let Some(name) = rec
        .container_name
        .as_deref()
        .map(str::trim)
        .filter(|n| !n.is_empty())
    {
        return Some(name.to_string());
    }
    // r19 P1-3：这里的查询此前只有 Names/State/Label 三个 tab 字段，喂给
    // splitn(4) 的 parse_docker_ps_a 后字段整体左移一位——state 吃到项目标签
    // （永远不是 "running"）、compose_project 恒 None，find 判据只剩
    // name == card_id 一条路，compose 卡（容器名 ≠ card_id）在详情页拿不到
    // 容器名。补上 {{.Image}} 对齐 docker_owned_container_state 的四字段。
    let stdout = proc_timeout::query("docker", &["ps", "-a", "--format", DOCKER_PS_A_FORMAT])?;
    let rows = parse_docker_ps_a(&stdout);
    owned_container_name_from_rows(&rows, &rec.card_id)
}

/// 与 [`owned_container_state_from_rows`] 同一套身份判据（compose 项目名/
/// 容器名 == card_id），返回命中的容器名。
fn owned_container_name_from_rows(rows: &[DockerPsRow], card_id: &str) -> Option<String> {
    rows.iter()
        .find(|row| row.compose_project.as_deref() == Some(card_id) || row.name == card_id)
        .map(|row| row.name.clone())
}

/// `docker inspect` 出 (容器主进程 PID, 状态, 健康检查结论)。
fn docker_container_runtime(container: &str) -> Option<(i64, String, Option<String>)> {
    let stdout = proc_timeout::query(
        "docker",
        &[
            "inspect",
            "--format",
            "{{.State.Pid}}\t{{.State.Status}}\t{{if .State.Health}}{{.State.Health.Status}}{{end}}",
            container,
        ],
    )?;
    let mut parts = stdout.trim().splitn(3, '\t');
    let pid = parts.next()?.trim().parse::<i64>().ok()?;
    let state = parts.next().unwrap_or_default().trim().to_string();
    let health = parts
        .next()
        .map(|h| h.trim().to_string())
        .filter(|h| !h.is_empty());
    Some((pid, state, health))
}

/// `docker stats --no-stream` 的容器内存用量（如 `7.9GiB`，docker 自己的口径）。
/// 这个命令要等一个采样周期（约 2 秒），靠 proc_timeout 兜底防卡死。
fn docker_container_memory(container: &str) -> Option<String> {
    let stdout = proc_timeout::query(
        "docker",
        &[
            "stats",
            "--no-stream",
            "--format",
            "{{.MemUsage}}",
            container,
        ],
    )?;
    stdout
        .trim()
        .split_whitespace()
        .next()
        .map(|first| first.trim().to_string())
        .filter(|mem| !mem.is_empty())
}

/// 原生引擎在 `.runtime/serve.identity.json` 里记的 PID（含 start_ticks/uid
/// 核验——与 reconcile 的身份核验同一判据，PID 复用后老 PID 的假活不展示）。
fn native_runtime_pid(rec: &db::StoreModelRecord) -> Option<i64> {
    let card_dir = config::data_dir()
        .join("cards")
        .join(config::safe_path_component(&rec.card_id).unwrap_or_default());
    if card_dir.as_os_str().is_empty() {
        return None;
    }
    let identity = native_identity_process(&card_dir)?;
    identity_process_alive(&identity).then_some(identity.pid)
}

/// uptime（ps etime）+ 主进程 RSS（/proc VmRSS）。两项各自尽力：拿不到就少一行，
/// 不编数。RSS 是主进程而非整个进程树，容器场景另有 docker stats 的容器合计，
/// 这里标明「主进程」避免两个数对不上时像是测错了。
fn push_uptime_and_rss(lines: &mut Vec<String>, pid: i64) {
    if let Some(etime) = proc_timeout::query("ps", &["-o", "etime=", &pid.to_string()]) {
        let etime = etime.trim();
        if !etime.is_empty() {
            lines.push(format!("已运行：{etime}"));
        }
    }
    if let Ok(status) = std::fs::read_to_string(format!("/proc/{pid}/status")) {
        let rss = status.lines().find_map(|line| {
            let rest = line.strip_prefix("VmRSS:")?.trim();
            rest.split_whitespace()
                .next()
                .and_then(|kb| kb.parse::<u64>().ok())
                .map(|kb| disk::format_bytes((kb * 1024) as f64))
        });
        if let Some(rss) = rss {
            lines.push(format!("内存占用：{rss}（主进程）"));
        }
    }
}

enum OpenAiProbe {
    Ok {
        latency_ms: u64,
        models: Vec<String>,
    },
    Refused,
    Unknown,
}

fn probe_openai_endpoint(endpoint_url: &str) -> OpenAiProbe {
    let Ok(mut url) = reqwest::Url::parse(endpoint_url) else {
        return OpenAiProbe::Unknown;
    };
    url.set_path("/v1/models");
    url.set_query(None);
    let started = std::time::Instant::now();
    let result = reqwest::blocking::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .no_proxy()
        .build()
        .and_then(|client| client.get(url).send());
    match result {
        Ok(response) if response.status().is_success() => {
            let latency_ms = started.elapsed().as_millis() as u64;
            let models = response
                .json::<serde_json::Value>()
                .ok()
                .and_then(|body| {
                    body["data"].as_array().map(|items| {
                        items
                            .iter()
                            .filter_map(|item| item["id"].as_str().map(str::to_string))
                            .collect::<Vec<_>>()
                    })
                })
                .unwrap_or_default();
            OpenAiProbe::Ok { latency_ms, models }
        }
        // HTTP 有响应但非 2xx：服务在、路由不对，与「没服务」分开说。
        Ok(_) => OpenAiProbe::Refused,
        Err(error) => {
            if probe_failure_means_nothing_listening(&error) {
                OpenAiProbe::Refused
            } else {
                OpenAiProbe::Unknown
            }
        }
    }
}

fn write_temp_script(content: &str) -> Result<PathBuf, ModelManagerError> {
    let mut f = tempfile::Builder::new()
        .prefix("lmm-engine-uninstall-")
        .suffix(".sh")
        .tempfile()
        .map_err(|e| ModelManagerError::new(format!("创建临时脚本失败：{e}")))?;
    f.write_all(content.as_bytes())
        .map_err(|e| ModelManagerError::new(format!("写入临时脚本失败：{e}")))?;
    let path = f
        .into_temp_path()
        .keep()
        .map_err(|e| ModelManagerError::new(format!("保留临时脚本失败：{e}")))?;
    let mut perms = std::fs::metadata(&path)
        .map_err(|e| ModelManagerError::new(format!("读取临时脚本权限失败：{e}")))?
        .permissions();
    perms.set_mode(perms.mode() | 0o111);
    std::fs::set_permissions(&path, perms)
        .map_err(|e| ModelManagerError::new(format!("设置临时脚本权限失败：{e}")))?;
    Ok(path)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_identity_supports_common_assets_without_overwriting_build_versions() {
        let sha = "a".repeat(64);
        let common =
            format!("ENGINE_BIN_SHA256=\"{sha}\"\n# local-model-manager/engines/tachyon-serve");
        let install = "source \"$CARD_DIR/common.sh\"";
        assert_eq!(
            native_tachyon_identity(install, Some(&common), None),
            Some(("aaaaaaaa".into(), sha.clone()))
        );
        assert_eq!(
            native_tachyon_identity(install, Some(&common), Some("build-42")),
            Some(("build-42".into(), sha.clone()))
        );
        assert_eq!(
            native_tachyon_identity(&common, None, Some("")),
            Some(("aaaaaaaa".into(), sha))
        );
        assert!(native_tachyon_identity(install, None, None).is_none());
        assert!(declared_native_sha("ENGINE_BIN_SHA256=short").is_none());
        assert!(native_tachyon_identity("ENGINE_BIN_SHA256=bad", None, None).is_none());
    }

    /// 真机（2026-10-27 cube-9fde）`docker ps -a` 四字段的实际形态：compose 卡
    /// 带项目标签，`docker run` 起的容器没有（None），状态字段区分 running/exited。
    #[test]
    fn parse_docker_ps_a_handles_missing_label_and_states() {
        let stdout = "ornith15-api\tvllm/vllm-openai:v0.27.1\trunning\tornith15-bf16\n\
                      z-image-turbo\tz-image-turbo:diffusers-0.40.0\texited\tz-image-turbo\n\
                      megacube-qwen38fn-single\ta551e05307cd\texited\t\n";
        let rows = parse_docker_ps_a(stdout);
        assert_eq!(rows.len(), 3);
        assert_eq!(rows[0].name, "ornith15-api");
        assert!(rows[0].running);
        assert_eq!(rows[0].compose_project.as_deref(), Some("ornith15-bf16"));
        assert!(!rows[1].running);
        assert_eq!(rows[1].compose_project.as_deref(), Some("z-image-turbo"));
        assert_eq!(rows[2].compose_project, None);
        // 空行不算容器
        assert!(parse_docker_ps_a("\n  \n").is_empty());
    }

    /// 无 container_name 的 docker 卡判停/认领依据：compose 项目名或容器名等于
    /// card_id 即视为本卡部署；两者都对不上时绝不把别人的容器当自己的。
    #[test]
    fn owned_container_state_matches_project_or_name() {
        let rows = parse_docker_ps_a(
            "ornith15-api\tvllm:v0.27.1\trunning\tornith15-bf16\n\
             z-image-turbo\tz-image:diffusers\texited\tz-image-turbo\n\
             cowork-vllm\tvllm:v0.27.1\trunning\t\n",
        );
        // 项目名 == card_id（ornith15-bf16 的容器叫 ornith15-api，名字对不上）
        assert!(matches!(
            owned_container_state_from_rows(&rows, "ornith15-bf16"),
            DockerOwnedContainerState::Running
        ));
        // 容器名 == card_id，且已退出 → Stopped（z-image「更新脚本」被拦的场景）
        assert!(matches!(
            owned_container_state_from_rows(&rows, "z-image-turbo"),
            DockerOwnedContainerState::Stopped
        ));
        // 同款镜像的外来容器：项目名、容器名都对不上 → Absent，不认领
        assert!(matches!(
            owned_container_state_from_rows(&rows, "ornith15-35b-a3b-nvfp4"),
            DockerOwnedContainerState::Absent
        ));
    }

    /// r19 P1-1：原生进程归属判定。进程 `--model-id` 是推理服务真正认的模型
    /// id（= endpoint_model），不是展示名 model_name——此前拿 model_name 比对，
    /// 六张卡没有一个能命中，驭光卡 stop 半成功（DB running=0、进程还活）时
    /// 活实例被判「非本卡」放行，purge 在活进程脚下删权重。
    #[test]
    fn native_process_ownership_matches_endpoint_model_first() {
        let endpoint_model = Some("ornith15-bf16");
        let model_name = "Ornith-1.5-35B-A3B";
        // 进程报的正是推理服务认的 id → 本卡（修复前这里恒 false）
        assert!(native_process_owned_by_card(
            Some("ornith15-bf16"),
            endpoint_model,
            model_name
        ));
        // 老记录没有 endpoint_model：展示名兜底命中，不算「非本卡」
        assert!(native_process_owned_by_card(
            Some(model_name),
            None,
            model_name
        ));
        // 别的卡的实例（两个 id 都对不上）→ 非本卡，放行跨卡更新
        assert!(!native_process_owned_by_card(
            Some("qwen38fn"),
            endpoint_model,
            model_name
        ));
        // 归属无法确认（进程无 --model-id）→ 保守起见视为本卡（拦）
        assert!(native_process_owned_by_card(
            None,
            endpoint_model,
            model_name
        ));
    }

    /// r19 P1-2：deployed 态无 container_name 的卡，ollama 不许落 docker 判据
    /// ——它既无容器也无镜像，docker 分支只会得到 Absent 再被恒拒。ollama_alive
    /// 要真发 HTTP、mock 不了，这里钉住路径选择这一层。
    #[test]
    fn deployed_stopped_evidence_routes_ollama_away_from_docker() {
        assert!(matches!(
            deployed_stopped_evidence("ollama"),
            DeployedStoppedEvidence::OllamaService
        ));
        // docker 形态的引擎照旧走自有容器判据
        assert!(matches!(
            deployed_stopped_evidence("vllm"),
            DeployedStoppedEvidence::OwnedContainer
        ));
    }

    /// r19 P1-3：详情页容器名按 compose 项目名命中（compose 卡容器名 ≠
    /// card_id），并钉住查询格式与 splitn(4) 解析器的字段对齐。
    #[test]
    fn owned_container_name_matches_compose_project() {
        let rows = parse_docker_ps_a(
            "ornith15-api\tvllm:v0.27.1\trunning\tornith15-bf16\n\
             z-image-turbo\tz-image:diffusers\texited\tz-image-turbo\n",
        );
        // compose 卡：容器名 ornith15-api ≠ card_id，靠项目名命中
        assert_eq!(
            owned_container_name_from_rows(&rows, "ornith15-bf16"),
            Some("ornith15-api".into())
        );
        // docker run 形态：容器名 == card_id
        assert_eq!(
            owned_container_name_from_rows(&rows, "z-image-turbo"),
            Some("z-image-turbo".into())
        );
        // 谁的容器都不是
        assert_eq!(owned_container_name_from_rows(&rows, "other-card"), None);
        // 查询格式必须与解析器对齐：四个 tab 分隔字段，顺序名字/镜像/状态/项目
        let fields = DOCKER_PS_A_FORMAT.split('\t').collect::<Vec<_>>();
        assert_eq!(fields.len(), 4, "字段数漂移会让 parse_docker_ps_a 错位");
        assert!(fields[0].contains("Names"));
        assert!(fields[1].contains("Image"));
        assert!(fields[2].contains("State"));
        assert!(fields[3].contains("com.docker.compose.project"));
        // 错位实况（钉住为什么必须四字段）：三字段行的项目标签落进 state、
        // compose_project 恒 None，compose 卡从此只能靠容器名碰运气。
        let misaligned = parse_docker_ps_a("ornith15-api\trunning\tornith15-bf16");
        assert_eq!(misaligned[0].compose_project, None);
        assert!(!misaligned[0].running);
    }

    /// GB10 统一内存设备上 nvidia-smi 的显存两行是 [N/A]，过滤后不再出现空数据。
    #[test]
    fn gpu_value_unsupported_covers_na_placeholders() {
        assert!(gpu_value_unsupported("[N/A]"));
        assert!(gpu_value_unsupported(" N/A "));
        assert!(gpu_value_unsupported("[Not Supported]"));
        assert!(gpu_value_unsupported(""));
        assert!(!gpu_value_unsupported("11.44 W"));
        assert!(!gpu_value_unsupported("45"));
    }

    /// r18 P2-2：identity 判活必须比对 start_ticks/uid——PID 会被内核复用，
    /// 仅凭「/proc/{pid}/stat 存在」会把同号新进程当成自己的实例认领。
    /// 用本测试进程自己的 /proc 做真实比对（非伪造）。
    #[test]
    fn identity_pid_reuse_is_caught_by_start_ticks_and_uid() {
        let stat = std::fs::read_to_string("/proc/self/stat").unwrap();
        let ticks = super::super::native_process::start_ticks(&stat)
            .expect("本进程 stat 应可解析出 starttime");
        use std::os::unix::fs::MetadataExt;
        let uid = std::fs::metadata("/proc/self").unwrap().uid();
        let self_pid = std::process::id() as i64;

        let ok = NativeIdentity {
            pid: self_pid,
            start_ticks: Some(ticks),
            uid: Some(uid),
        };
        assert!(identity_process_alive(&ok), "字段全对必须判活");

        let reused = NativeIdentity {
            pid: self_pid,
            start_ticks: Some(ticks + 1),
            uid: Some(uid),
        };
        assert!(
            !identity_process_alive(&reused),
            "start_ticks 换了 = 同号新进程（PID 复用），不得判活"
        );

        let foreign_uid = NativeIdentity {
            pid: self_pid,
            start_ticks: Some(ticks),
            uid: Some(uid + 1),
        };
        assert!(
            !identity_process_alive(&foreign_uid),
            "uid 对不上 = 别的用户起的进程，不得判活"
        );

        let legacy = NativeIdentity {
            pid: self_pid,
            start_ticks: None,
            uid: None,
        };
        assert!(
            identity_process_alive(&legacy),
            "老 identity 缺 start_ticks/uid 时退回仅 PID 存在"
        );
    }

    #[test]
    fn native_probe_distinguishes_absence_from_failure_and_checks_integrity() {
        let blob = |content: &str| db::ScriptBlob {
            content: content.into(),
            sha256: script_runner::sha256_hex(content),
        };
        assert_eq!(
            native_engine_cleanup_probe(&blob("#!/bin/bash\necho LMM_ENGINE_PRESENT=0\n")).unwrap(),
            (false, None)
        );
        assert_eq!(
            native_engine_cleanup_probe(&blob(
                "#!/bin/bash\necho LMM_ENGINE_PRESENT=1\necho LMM_ENGINE_SIZE_BYTES=42\n"
            ))
            .unwrap(),
            (true, Some(42))
        );
        assert!(native_engine_cleanup_probe(&blob("#!/bin/bash\nexit 1\n")).is_err());
        assert!(native_engine_cleanup_probe(&blob("echo unknown\n")).is_err());
        let mut altered = blob("#!/bin/bash\necho LMM_ENGINE_PRESENT=0\n");
        altered.content.push_str("echo changed\n");
        assert!(native_engine_cleanup_probe(&altered).is_err());
    }

    fn make_card(name: &str, quant: Option<&str>) -> StoreCardDetail {
        StoreCardDetail {
            assets: Default::default(),
            asset_sha256: None,
            summary: StoreCardSummary {
                resource_requirements: Default::default(),
                deploy_topology: Default::default(),
                id: "card-x".into(),
                name: name.into(),
                org: None,
                description: None,
                size_bytes: 0,
                quantization: quant.map(|s| s.to_string()),
                engine_name: "sglang".into(),
                engine_version: None,
                script_sha256: Default::default(),
                // 这几个测试只看模型名/量化怎么被清洗，档位取什么都行；
                // 用 Default 是为了跟「没声明 = 没校验」的缺省保持一致。
                weight_verification: Default::default(),
            },
            source_repo_id: None,
            source_filename: None,
            scripts: Default::default(),
            script_sha256: None,
            profiles: Vec::new(),
        }
    }

    #[test]
    fn suggests_safe_model_names() {
        // 期望值是用 Python 版逐字符对拍出来的（")" 被换成 "-" 后跟原有的 "-"
        // 形成 "--"，Python 的 re.sub 就是这个行为，必须保持一致）
        assert_eq!(
            suggest_model_name(&make_card(
                "Qwen3.8-27B (NVFP4 + Native MTP)",
                Some("NVFP4")
            )),
            "qwen3.8-27b-nvfp4-native-mtp--nvfp4"
        );
        assert_eq!(suggest_model_name(&make_card("Simple", None)), "simple");
    }

    #[test]
    fn network_failures_get_resume_hint_but_fatal_ones_dont() {
        // 用户主动取消、以及重试也没用的错误：按原样报出去，不要加"可续传"
        // 那句话——磁盘满了还说"重新点下载会继续"是误导
        for fatal in [
            "脚本执行失败（退出码 1）：磁盘空间不足",
            "脚本执行失败（退出码 1）：在远端找不到该模型/量化版本",
            "已取消",
            "脚本完整性校验失败：实际内容与商店登记的不一致",
        ] {
            assert!(
                ModelManager::is_fatal_failure(fatal),
                "应判定为致命：{fatal}"
            );
        }
        // 网络类失败：不自动重试（用户要自己掌控），但要明确告诉用户进度保留了
        for resumable in [
            "脚本执行超时：download_model.sh（连续 300 秒没有任何新输出，判定为卡死）",
            "脚本执行失败（退出码 1）：网络连接失败，请检查网络是否可用",
            "脚本执行失败（退出码 56）：脚本返回错误（curl: (56) Recv failure）",
        ] {
            assert!(
                !ModelManager::is_fatal_failure(resumable),
                "应带续传提示：{resumable}"
            );
        }
    }

    /// 兜底提权是 root 链的最后一段：凡是**不在**致命清单里的失败，
    /// install_engine 都会自动提权把同一份内容以 root 重跑一次。所以
    /// "内容信不过"这一类必须登记在案——否则校验没过的脚本反倒借这条路拿到了
    /// root，正好是这道校验要防的事。
    ///
    /// 钉的是 script_runner 那两个常量本身而不是抄一遍字面量：抄字面量的话，
    /// 那边改一个字这里照样绿，而失效的表现恰恰是"未验签内容自动拿到 root"。
    #[test]
    fn untrusted_content_never_escalates_to_root() {
        for msg in [
            format!(
                "{}：这一步要以管理员权限执行，但商店没有下发该脚本的 sha256",
                script_runner::UNTRUSTED_SOURCE_PREFIX
            ),
            format!(
                "{}（脚本在请求输入前改动了终端设置）",
                script_runner::ECHO_REOPENED_ABORT
            ),
            "脚本完整性校验失败：实际内容与商店登记的不一致".to_string(),
        ] {
            assert!(
                ModelManager::is_fatal_failure(&msg),
                "这类失败绝不能触发自动提权重跑：{msg}"
            );
        }
    }

    /// 网络类失败不得触发兜底提权（.48 真机案例，2026-10-27）：无外网设备上
    /// docker pull 超时 → 自动弹 pkexec 重试 → 没人输密码 → 1800 秒 idle 判死。
    /// 样本取自该真机的实际失败文案（英文原文 + humanize 中文形态各一）。
    #[test]
    fn network_failures_never_escalate_to_root() {
        for msg in [
            // .48 run log 原文：docker pull 的 registry 请求超时
            "脚本执行失败（退出码 1）：failed to do request: Head \"https://registry-1.docker.io/v2/\": dial tcp: lookup registry-1.docker.io: i/o timeout",
            "脚本执行失败（退出码 1）：Error response from daemon: failed to resolve reference vllm/vllm-openai: no route to host",
            "脚本执行失败（退出码 1）：Temporary failure resolving 'hf-mirror.com'",
            // 客户端 humanize 形态。这条**刻意保留 .48 真机原文的字面量**、不
            // 引用 store_client 的 HINT_* 常量：守卫（MARKERS）那侧已经引用
            // 常量了，这里从真机文案那一侧把两边一起钉住——常量若被改得与
            // 真实文案分家，本用例会先红（r19 评审 P2-2 的双侧钉）。
            "脚本执行失败（退出码 1）：连接超时，请检查网络（或尝试在设置里切换镜像地址）（请检查设备能不能访问外网）",
        ] {
            assert!(
                ModelManager::network_unreachable_failure(msg),
                "网络类失败必须拦下兜底提权：{msg}"
            );
            assert!(
                !ModelManager::is_fatal_failure(msg),
                "网络类失败应保留断点续传重试（不进致命清单）：{msg}"
            );
        }
        // 反例：与网络无关的普通失败不受影响（仍走既有兜底提权路径）
        assert!(!ModelManager::network_unreachable_failure(
            "脚本执行失败（退出码 1）：Error response from daemon: No such image: vllm/x"
        ));
        assert!(!ModelManager::network_unreachable_failure(
            "脚本执行失败（退出码 2）：some card-specific error"
        ));
    }

    /// 传输中断（cube-5059 真机案例，2026-10-29）：换 IP 后出网走代理劣化，
    /// 驭光引擎包 curl 92 → 兜底提权重跑 → curl 18 → 终态失败。三类判定必须
    /// 同时成立：是传输中断、不致命、也不是"网络不可达"（治法不同——这边
    /// 该静默续传，那边该提示换网络）。样本取自该真机 run log 原文。
    #[test]
    fn transfer_interrupts_resume_instead_of_escalate_or_die() {
        for msg in [
            // .59 run log 原文：卡片脚本 humanize 后的 curl 18
            "脚本执行失败（退出码 1）：引擎包还没下完整（curl 退出码 18）。已下好的部分保留在 /root/.local/share/local-model-manager/engines/tachyon-serve/engine.tar.gz.ea7605.part，再点一次「安装引擎」会从断点接着传。",
            // curl 原文（卡片没 humanize 时直接透出）
            "脚本执行失败（退出码 18）：curl: (18) Transferred a partial file",
            "脚本执行失败（退出码 92）：curl: (92) HTTP/2 stream 1 was not closed cleanly: CANCEL (err 8)",
            // 同族：连接被对端重置（curl 56）
            "脚本执行失败（退出码 56）：脚本返回错误（curl: (56) Recv failure: Connection reset by peer）",
            // r21 P2-7：通用包装器形态——卡片没透出 curl 原文、摘要里也没有
            // 「curl」字样时，只剩退出码本身可认。
            "脚本执行失败（退出码 18）：下载未完成",
            "脚本执行失败（退出码 92）：流被中途取消",
            // r21 P2-7：curl/docker 断流的三个原文形态
            "脚本执行失败（退出码 18）：curl: (18) transfer closed with 297 bytes remaining",
            "脚本执行失败（退出码 56）：Recv failure: Connection was reset",
            "脚本执行失败（退出码 1）：unexpected EOF during chunked transfer",
        ] {
            assert!(
                ModelManager::transfer_interrupted_failure(msg),
                "应判为传输中断（自动续传）：{msg}"
            );
            assert!(!ModelManager::is_fatal_failure(msg), "不该判致命：{msg}");
            assert!(
                !ModelManager::network_unreachable_failure(msg),
                "不该判网络不可达（治法是续传不是提示换网）：{msg}"
            );
        }
        // 反例：远端真没有（HTTP 404）、权限不足、超时判死——都不是传输中断，
        // 不许进续传循环
        for msg in [
            "脚本执行失败（退出码 22）：脚本返回错误（curl: (22) The requested URL returned error: 404 Not Found）",
            "脚本执行失败（退出码 1）：权限不足",
            "脚本执行超时：install_engine.sh（连续 300 秒没有任何新输出，判定为卡死）",
        ] {
            assert!(
                !ModelManager::transfer_interrupted_failure(msg),
                "不该判传输中断：{msg}"
            );
        }
    }

    /// install_engine 是六个入口脚本里唯一会走 pkexec 的，所以只有它按
    /// 「会以 root 执行」那一档强制要求哈希。这份判定写错的代价是不对称的：
    /// 漏了它 = 未验签内容能拿到 root；多算一个 = 一张正常卡片突然装不动。
    #[test]
    fn only_install_engine_is_treated_as_root_capable() {
        assert!(may_run_as_root("install_engine"));
        for t in ["download_model", "deploy", "enable", "stop", "uninstall"] {
            assert!(!may_run_as_root(t), "{t} 不该按提权那一档要求哈希");
        }
    }

    /// **生产上没有任何一条路能把"脚本地址是本地文件路径"放行。**
    ///
    /// 这一位原来是内置测试商店的生产开关（`builtin_store`）。测试商店 2026-09-30
    /// 拿掉之后它只剩测试一个用处，所以这条测试从"谁可以用"改成盯"生产拿不到它"
    /// ——`ModelManager::new` 出来的管理器必须是关着的，而唯一的开法是那个
    /// `#[cfg(test)]` 的 `with_local_script_paths`。它一旦松掉，一张被改过的卡片
    /// 就能指定"读本机哪个文件再当脚本跑"，install_engine 那条还经 pkexec。
    #[test]
    fn nothing_in_production_may_turn_on_local_script_paths() {
        let card = make_card("x", None);
        let remote = ScriptsSource::from_card(&card, false);

        assert!(
            !remote.fetch_policy("install_engine").allow_local_path,
            "商店卡片不能读本地路径"
        );
        assert!(
            remote.fetch_policy("install_engine").may_run_as_root,
            "install_engine 必须按提权那一档取（强制要求哈希）"
        );
        assert!(!remote.fetch_policy("deploy").may_run_as_root);

        // Local 来源（已固化的本地副本）压根不 fetch，策略也必须是最严的一档，
        // 免得哪天有人把它接进 fetch 路径时意外放行。
        let local = ScriptsSource::Local {
            blobs: Default::default(),
            assets: Default::default(),
            profiles: Vec::new(),
        };
        assert!(!local.fetch_policy("install_engine").allow_local_path);

        // 生产构造出来的管理器：这一位必须是关着的。
        let _ctx = setup_temp_db();
        assert!(
            !ModelManager::new(None, None).local_script_paths,
            "生产构造函数把本地路径放行了"
        );
    }

    /// 引擎没装时探针以非 0 退出码结束，run_script 返回的是 Err。这个测试盯的
    /// 就是「那种情况下还能不能读到 LMM_NEED_ROOT」——如果实现改回用返回值判断
    /// 而不是用 on_progress 收集输出，这里就会挂。
    #[test]
    fn probe_reads_need_root_even_when_engine_is_missing() {
        let _ctx = setup_temp_db();
        let mgr = ModelManager::new(None, None);
        let source = ScriptsSource::Local {
            assets: Default::default(),
            blobs: [(
                "install_engine".to_string(),
                db::ScriptBlob {
                    content: "#!/bin/sh
echo LMM_NEED_ROOT=1
echo 'probe: engine missing' >&2
exit 1
"
                    .to_string(),
                    sha256: String::new(),
                },
            )]
            .into_iter()
            .collect(),
            profiles: Vec::new(),
        };
        let probe = mgr.engine_probe(&source, "card-x", &HashMap::new());
        assert!(!probe.installed, "退出码非 0 应判定为「未安装」");
        assert!(probe.need_root, "非 0 退出时也必须能读到 LMM_NEED_ROOT=1");
    }

    /// 老卡片的脚本不打印 LMM_NEED_ROOT，探针必须报 need_root=false —— 这样
    /// 安装会先以普通用户身份尝试（失败再自动提权），而不是无条件弹密码框。
    #[test]
    fn probe_defaults_to_no_root_for_scripts_without_the_marker() {
        let _ctx = setup_temp_db();
        let mgr = ModelManager::new(None, None);
        let source = ScriptsSource::Local {
            assets: Default::default(),
            blobs: [(
                "install_engine".to_string(),
                db::ScriptBlob {
                    content: "#!/bin/sh
echo 'probe: engine present'
exit 0
"
                    .to_string(),
                    sha256: String::new(),
                },
            )]
            .into_iter()
            .collect(),
            profiles: Vec::new(),
        };
        let probe = mgr.engine_probe(&source, "card-x", &HashMap::new());
        assert!(probe.installed);
        assert!(!probe.need_root, "没有约定标记就不该擅自提权");
    }

    // 这里原来有一条 `bundled_cards_implement_the_probe_convention`：遍历内置测试
    // 商店那 6 张卡，断言每张卡的 install_engine 都实现了只读探针约定
    // （`LMM_PROBE_ONLY`）。内置卡片 2026-09-30 整体从仓库里拿掉之后，它没有可遍历
    // 的对象了。
    //
    // **这条约定并没有跟着消失，只是守它的位置变了**：卡片脚本现在只存在于生产
    // 商店，仓库里没有任何副本，所以"每张在架卡片都实现了探针约定"只能在上架流程
    // 里查（见 pinvou3-ops 的上架检查单），代码这边查不了也不该假装能查。宿主侧的
    // 那一半仍然有测试守着：探针怎么跑、读不到标记时按 need_root=false 处理，见上面
    // 两条 `probe_*`。

    #[test]
    fn extracts_markers() {
        let lines = vec![
            "some output".to_string(),
            "LMM_ENDPOINT_URL=http://127.0.0.1:30000/v1/chat/completions".to_string(),
            "LMM_ENDPOINT_MODEL=/models/target".to_string(),
        ];
        assert_eq!(
            extract_marker(&lines, "LMM_ENDPOINT_URL=").as_deref(),
            Some("http://127.0.0.1:30000/v1/chat/completions")
        );
        assert_eq!(
            extract_marker(&lines, "LMM_ENDPOINT_MODEL=").as_deref(),
            Some("/models/target")
        );
        assert_eq!(extract_marker(&lines, "LMM_ENGINE_IMAGE="), None);
    }

    /// r19 评审 P2-1：install_engine 的 stdout 是卡片作者可控的输入，伪造的
    /// `LMM_ENGINE_IMAGE=` 经 mark_engine_installed 的 COALESCE 能覆盖共享引擎
    /// 行、再借 delete_engine 驱动 `docker rmi`。形式白名单先挡掉注入形状。
    #[test]
    fn engine_image_marker_whitelist_rejects_injection_shapes() {
        // 合法形态：裸 repo、repo:tag、带 registry 端口、摘要引用
        for ok in [
            "example/img",
            "vllm/vllm-openai:v0.8.5",
            "registry.internal:5000/team/app:latest",
            "registry.internal:5000/team/app@sha256:0f2a9c11d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9",
            "ghcr.io/org/repo",
        ] {
            assert!(engine_image_marker_is_wellformed(ok), "应放行：{ok}");
        }
        // `-` 开头（docker 选项）、空白/引号/元字符（不是一个 token）、没有
        // repo 名的纯 tag/纯摘要、空串——全部出局
        for bad in [
            "",
            "-f",
            "--",
            "a b",
            "'quoted'",
            "$evIL",
            "example/img;rm -rf /",
            "a||b",
            "a\nb",
            ":just-a-tag",
            "/repo",
            "@sha256:0f2a9c",
        ] {
            assert!(!engine_image_marker_is_wellformed(bad), "应拒绝：{bad:?}");
        }
    }

    /// r19 评审 P2-1：同名同版本的引擎行是共享的。脚本自报一个**不同**的镜像
    /// 名时必须保留旧值（返回 None 让 COALESCE 生效），形式不合法的 marker
    /// 直接丢弃；新引擎行没有旧值时照常登记。
    #[test]
    fn differing_engine_image_marker_never_overwrites_existing_row() {
        let _home = setup_temp_db();
        db::mark_engine_installed("vllm", Some("v1"), Some("old/img:1"), None, None).unwrap();

        // 不同镜像名：不覆盖
        assert_eq!(
            engine_image_to_record("vllm", Some("v1"), Some("new/img:2".into())),
            None,
            "共享行已有镜像名时，自报的不同值不该覆盖旧值"
        );
        // 相同镜像名：等值登记，无害
        assert_eq!(
            engine_image_to_record("vllm", Some("v1"), Some("old/img:1".into())),
            Some("old/img:1".into())
        );
        // 形式不合法：丢弃
        assert_eq!(
            engine_image_to_record("vllm", Some("v1"), Some("-f".into())),
            None
        );
        // 新引擎行（库里没有旧值）：正常登记
        assert_eq!(
            engine_image_to_record("sglang", None, Some("s/img:9".into())),
            Some("s/img:9".into())
        );

        // 走完 mark_engine_installed 全链路，库里仍是旧镜像名（COALESCE 那条腿）
        let recorded = engine_image_to_record("vllm", Some("v1"), Some("new/img:2".into()));
        db::mark_engine_installed("vllm", Some("v1"), recorded.as_deref(), None, None).unwrap();
        assert_eq!(
            db::get_installed_engine("vllm", Some("v1"))
                .unwrap()
                .unwrap()
                .docker_image
                .as_deref(),
            Some("old/img:1"),
            "覆盖被拒后落库的必须是旧镜像名"
        );
    }

    #[test]
    fn identifies_only_explicit_missing_deployment_receipts() {
        assert!(deployment_receipt_is_missing(
            "ERROR: 模型尚未部署，缺少 .deploy-env，请先部署。"
        ));
        assert!(deployment_receipt_is_missing(
            "ERROR: 还没有部署过（缺 .deploy-env），请先完成「部署」"
        ));
        assert!(deployment_receipt_is_missing(
            "deployment configuration missing; deploy the model first"
        ));

        // 不能把普通的启用故障误降级为“未部署”。这些问题都应该保留原状态，
        // 让用户修复后直接重试，而不是被迫重新部署。
        assert!(!deployment_receipt_is_missing("启动服务失败：显存不足"));
        assert!(!deployment_receipt_is_missing("找不到模型权重"));
        assert!(!deployment_receipt_is_missing(
            "deployment configuration is valid, but port 30000 is already in use"
        ));
    }

    #[test]
    fn identifies_explicit_incomplete_weight_errors_without_misclassifying_runtime_errors() {
        assert!(model_weights_are_incomplete(
            "ERROR: download finished but the model directory is incomplete"
        ));
        assert!(model_weights_are_incomplete("模型权重不完整，请先下载模型"));
        assert!(model_weights_are_incomplete("missing model weights"));
        assert!(!model_weights_are_incomplete("网络连接失败，请稍后重试"));
        assert!(!model_weights_are_incomplete("启动服务失败：显存不足"));
        assert!(!model_weights_are_incomplete(
            "port 30000 is already in use"
        ));
    }

    #[test]
    fn recognizes_engine_startup_failures_without_calling_them_download_failures() {
        assert!(engine_startup_failed(
            "2026-10-13 FATAL server failed during startup"
        ));
        assert!(engine_startup_failed(
            "ERROR startup failed [finalizing target]"
        ));
        assert!(engine_startup_failed(
            "推理服务启动失败：模型权重已加载完成，但服务初始化未完成"
        ));
        assert!(!engine_startup_failed("模型权重不完整，请先下载模型"));
    }

    /// 防的是「几百 GB 的 rm -rf 被 300 秒静默超时误杀」。
    ///
    /// 卸载脚本删权重时一行输出都没有（`rm -rf` / `shutil.rmtree` 不打进度），
    /// 双机卡要在两台机器上各删约 168 GB。用默认的 300 秒阈值，它必然在删到
    /// 一半时被判成卡死并杀掉——而那正是"磁盘半清理 + 记录还在"这个死结的
    /// 制造现场（厂商脚本的 config.json 安全检查会让之后每一次重试都 die）。
    /// 所以这里钉的不只是一个常量，是"不要自己造出不可恢复状态"。
    #[test]
    fn uninstall_is_not_held_to_the_five_minute_silence_limit() {
        assert_eq!(
            idle_timeout_for("uninstall"),
            Some(LONG_RUNNING_IDLE_TIMEOUT),
            "卸载是无输出的大体积删除，不能用默认的 300 秒静默阈值"
        );
        for long in ["install_engine", "deploy", "enable"] {
            assert_eq!(idle_timeout_for(long), Some(LONG_RUNNING_IDLE_TIMEOUT));
        }
        // stop 不放宽：compose down 一路都在打印容器停止进度，真静默五分钟就是
        // 卡住了。全都放宽等于这个阈值形同虚设。
        assert_eq!(
            idle_timeout_for("stop"),
            None,
            "停用是快操作，要保留默认阈值"
        );
        assert_eq!(idle_timeout_for("download_model"), None);
    }

    /// db 测试同款：环境变量是进程全局的，而测试默认多线程并行，必须拿
    /// `config::ENV_TEST_LOCK` 串行化，否则两个测试会互相把数据目录切走。
    fn setup_temp_db() -> config::TempHome {
        // TempHome 自带那把跨模块的锁，并在析构时把 HOME 还原回去。
        let home = config::TempHome::new();
        db::init_db().unwrap();
        home
    }

    fn add_stuck_record(card_id: &str, repo: &str, size: i64) {
        db::add_store_model(db::AddStoreModelParams {
            card_id,
            model_name: "ornith15-bf16",
            card_name: "Ornith 1.5 35B",
            org: None,
            description: None,
            engine_name: "vllm",
            engine_version: Some("v0.27.1-aarch64"),
            size_bytes: size,
            quantization: None,
            source_repo_id: Some(repo),
            source_filename: None,
            scripts: &Default::default(),
            status: "deployed",
            profiles: &[],
            deploy_topology: DeployTopology::Single,
        })
        .unwrap();
    }

    fn manager_with_models_dir(dir: &std::path::Path) -> ModelManager {
        let settings = Settings {
            models_dir: dir.to_string_lossy().to_string(),
            staging_dir: dir.join("staging").to_string_lossy().to_string(),
            ..Default::default()
        };
        ModelManager::new(Some(settings), None)
    }

    /// 防的是「卸载删到一半被打断，卡片永远赖在模型库里」。
    ///
    /// 真实剧本：uninstall 跑到 `rm -rf ${MODEL_PATH}` 时被杀，config.json 已经
    /// 没了，于是厂商脚本的安全检查让之后每一次重试都 die → exit 1 → 记录删不
    /// 掉。这条逃生路径必须真的把记录清掉，**并且**把还在磁盘上的目录报出来
    /// ——只清记录不报残留，那几十 GB 就变成谁也不知道的孤儿。
    #[test]
    fn force_remove_clears_the_record_and_names_what_is_still_on_disk() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        // 半删现场：目录还在、config.json 已经被删掉了（所以重试必然 die）
        let weights = models_dir.join("Ornith-1.5-35B-A3B");
        std::fs::create_dir_all(weights.join("blobs")).unwrap();

        add_stuck_record("card-stuck", "Org/Ornith-1.5-35B-A3B", 168_000_000_000);
        let mgr = manager_with_models_dir(&models_dir);

        let removal = mgr
            .force_remove_record("card-stuck")
            .expect("应当能强制移除");
        assert!(
            db::get_store_model("card-stuck").unwrap().is_none(),
            "记录必须真的从库里消失，否则卡片还是赖着"
        );
        assert_eq!(removal.recorded_bytes, 168_000_000_000);
        assert!(
            removal
                .residual_paths
                .iter()
                .any(|p| p.ends_with("Ornith-1.5-35B-A3B")),
            "还在磁盘上的权重目录必须被点名：{:?}",
            removal.residual_paths
        );
        assert!(
            removal.warning.contains("没有被删除"),
            "提示必须明说文件还在，不能让用户以为点完就干净了：{}",
            removal.warning
        );

        // 记录没了之后再点一次，要给明确错误，不能假装又成功了一次
        assert!(
            mgr.force_remove_record("card-stuck").is_err(),
            "记录已不存在时应当报错"
        );
    }

    /// 防的是「常见位置没找到残留，就对用户说已经清干净了」。
    ///
    /// 权重目录是按卡片脚本的约定反推的（MODEL_PATH 写在脚本自己的 .env 里，
    /// LMM 读不到），卡片完全可能把几十 GB 放在别处。没扫到 ≠ 干净，这条路径
    /// 一个字节都没删，任何情况下都不能给出"已回收空间"的暗示。
    #[test]
    fn force_remove_never_claims_the_disk_is_clean() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        add_stuck_record(
            "card-elsewhere",
            "Org/Weights-Somewhere-Else",
            133_000_000_000,
        );

        let removal = manager_with_models_dir(&models_dir)
            .force_remove_record("card-elsewhere")
            .expect("应当能强制移除");
        assert!(removal.residual_paths.is_empty(), "常见位置本来就没有东西");
        assert!(
            removal.warning.contains("只删记录") && removal.warning.contains("自行核实"),
            "扫不到残留时也必须说清楚「没删文件、请自己核实」：{}",
            removal.warning
        );
    }

    /// r19 评审 P2-4：ollama 卡的权重 blob 在 `~/.ollama/models`，不在 models_dir。
    /// candidate_paths 漏掉它的话，强制移除一张 ollama 卡会报"没有残留"——
    /// 几十 GB 权重从此变成谁也不知道的孤儿。
    #[test]
    fn ollama_card_residue_measurement_includes_dot_ollama() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        // TempHome 已把 HOME 指到临时目录：在这里造出 ollama 的真实布局
        let ollama_models = tmp.path().join(".ollama").join("models").join("blobs");
        std::fs::create_dir_all(&ollama_models).unwrap();
        std::fs::write(ollama_models.join("sha256-weights"), [0u8; 64]).unwrap();

        db::add_store_model(db::AddStoreModelParams {
            card_id: "card-ollama",
            model_name: "qwen3:32b",
            card_name: "Qwen3 32B",
            org: None,
            description: None,
            engine_name: "ollama",
            engine_version: None,
            size_bytes: 20_000_000_000,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &Default::default(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: DeployTopology::Single,
        })
        .unwrap();

        let removal = manager_with_models_dir(&models_dir)
            .force_remove_record("card-ollama")
            .expect("应当能强制移除");
        assert!(
            removal
                .residual_paths
                .iter()
                .any(|p| p.ends_with(".ollama")),
            "ollama 的权重目录必须被点名，否则几十 GB 就成了谁也看不见的孤儿：{:?}",
            removal.residual_paths
        );
    }

    // ---------------------------------------------- 卸载：保留权重 / 彻底删除

    #[test]
    fn known_destructive_qwen_uninstall_is_rejected_before_materialization() {
        let original =
            include_str!("../../../../../../tests/fixtures/model-store-unsafe-qwen-uninstall.sh");
        assert_eq!(
            script_runner::sha256_hex(original),
            "184de35d3e0844a4a43c8ffc6ab481aeeb65a06843d68410c18fe0f869ec349c"
        );
        for purge in [false, true] {
            let tmp = setup_temp_db();
            let models = tmp.path().join("models");
            let weights = models.join("Qwen3.8-27B-NVFP4-Tachyon");
            make_fake_weights(&weights, 4096);
            add_stuck_record("unsafe-qwen", "Org/Qwen3.8-27B-NVFP4-Tachyon", 4098);
            let scripts = BTreeMap::from([
                (
                    "uninstall".into(),
                    db::ScriptBlob {
                        content: original.into(),
                        sha256: "forged-metadata".into(),
                    },
                ),
                (
                    "stop".into(),
                    db::ScriptBlob {
                        content:
                            "#!/bin/bash\necho stop-ran > \"$LMM_MODELS_DIR/stop-ran\"\nexit 70\n"
                                .into(),
                        sha256: String::new(),
                    },
                ),
            ]);
            let assets = BTreeMap::from([("common.sh".into(), db::ScriptBlob {
                content: "MODEL_DIR=\"$LMM_MODELS_DIR/Qwen3.8-27B-NVFP4-Tachyon\"\nRUNTIME_DIR=\"$LMM_MODELS_DIR/runtime-fixture\"\nDEPLOY_ENV=\"$LMM_MODELS_DIR/env-fixture\"\nENGINE_DIR=\"$LMM_MODELS_DIR/engine-fixture\"\nENGINE_ROOT=\"$LMM_MODELS_DIR/engines-fixture\"\n".into(),
                sha256: String::new(),
            })]);
            db::set_scripts_content("unsafe-qwen", &scripts, &assets).unwrap();
            db::set_running("unsafe-qwen", true).unwrap();
            let error = manager_with_models_dir(&models)
                .uninstall("unsafe-qwen", purge, &mut |_| {}, Default::default())
                .unwrap_err();
            assert!(
                error
                    .to_string()
                    .contains("已拦截存在已知删除风险的旧版卸载脚本"),
                "{error}"
            );
            assert!(weights.join("model.safetensors").exists());
            assert!(!models.join("stop-ran").exists());
            assert!(!config::data_dir().join("cards/unsafe-qwen").exists());
            let record = db::get_store_model("unsafe-qwen").unwrap().unwrap();
            assert!(record.running);
            assert_eq!(record.status, "deployed");
        }
    }

    /// 装一条带真脚本的记录，脚本按 `LMM_PURGE_WEIGHTS` 决定删不删权重
    /// ——和六张内置卡的入口脚本用的是同一套约定（规范 4.6）。
    fn add_record_with_uninstall_script(card_id: &str, repo: &str, size: i64) {
        add_stuck_record(card_id, repo, size);
        let leaf = repo.rsplit('/').next().unwrap().to_string();
        let script = format!(
            "#!/bin/bash\n\
             set -euo pipefail\n\
             if [ \"${{LMM_PURGE_WEIGHTS:-0}}\" = \"1\" ]; then\n\
             \x20   rm -rf -- \"$LMM_MODELS_DIR/{leaf}\"\n\
             \x20   echo '权重已删除'\n\
             else\n\
             \x20   echo '按选择保留权重'\n\
             fi\n"
        );
        let blobs: BTreeMap<String, db::ScriptBlob> = [(
            "uninstall".to_string(),
            db::ScriptBlob {
                content: script,
                sha256: String::new(),
            },
        )]
        .into_iter()
        .collect();
        db::set_scripts_content(card_id, &blobs, &Default::default()).unwrap();
    }

    /// 写一个体积已知的假权重目录，返回写进去的字节数。
    fn make_fake_weights(dir: &std::path::Path, bytes: usize) -> u64 {
        std::fs::create_dir_all(dir).unwrap();
        std::fs::write(dir.join("config.json"), "{}").unwrap();
        std::fs::write(dir.join("model.safetensors"), vec![7u8; bytes]).unwrap();
        2 + bytes as u64
    }

    /// 防的是「用户选了『仅卸载（保留权重）』，权重还是被删了」。
    ///
    /// 2026-09-17 用户提的正是这个：有时候只想清掉部署、把权重留着，重装时不用
    /// 再下几十 GB。这条测试钉住三件事：
    ///   ① 意图确实以 `LMM_PURGE_WEIGHTS=0` 传到了脚本（脚本据此不删）；
    ///   ② 权重目录卸载完还在；
    ///   ③ **返回值不许撒谎**——这次一个 GB 都没释放，`freed_bytes` 就得接近 0，
    ///      绝不能像改之前那样把 card.json 里登记的 67GB 当成回收量报上去。
    #[test]
    fn keeping_the_weights_leaves_them_on_disk_and_reports_no_space_reclaimed() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        let weights = models_dir.join("Ornith-1.5-35B-A3B");
        make_fake_weights(&weights, 4096);

        add_record_with_uninstall_script("card-keep", "Org/Ornith-1.5-35B-A3B", 67_000_000_000);
        // 造出一个"正在运行的已部署模型"，卸载要清的就是这几列。只把 status 摆成
        // deployed 是不够的：漏清 running / container_name / endpoint_url 的话，
        // 界面会把一个已经不存在的服务显示成运行中，而测试照样是绿的。
        db::set_running("card-keep", true).unwrap();
        db::set_container_name("card-keep", Some("ornith15-vllm")).unwrap();
        db::set_endpoint_info(
            "card-keep",
            Some("http://127.0.0.1:30000/v1"),
            Some("ornith15-bf16"),
        )
        .unwrap();
        let mgr = manager_with_models_dir(&models_dir);
        let outcome = mgr
            .uninstall("card-keep", false, &mut |_| {}, Default::default())
            .expect("卸载应当成功");

        assert!(weights.exists(), "用户选了保留，权重目录必须还在");
        assert!(
            weights.join("model.safetensors").exists(),
            "保留的必须是完整的权重，不能只剩一个空壳目录"
        );
        assert!(!outcome.purged_weights);
        assert_eq!(outcome.freed_bytes, 0, "什么都没删，就不能报出释放量");
        assert_eq!(
            outcome.recorded_bytes, 67_000_000_000,
            "登记体积照旧回报，但它在另一个字段里，不会被当成回收量"
        );
        assert!(
            outcome
                .remaining_paths
                .iter()
                .any(|p| p.ends_with("Ornith-1.5-35B-A3B")),
            "保留下来的权重目录要点名报出来：{:?}",
            outcome.remaining_paths
        );
        assert!(
            outcome.summary.contains("保留") && !outcome.summary.contains("已删除"),
            "这句话会直接显示给用户，不能把「保留权重」说成「已清理」：{}",
            outcome.summary
        );
        // 卡片必须**留在模型库里**并退回「已下载」。
        //
        // 改之前这里断言的是记录被删掉（"卸载就是卸载"）。那条规则和"保留权重"
        // 放在一起就自相矛盾：权重还在盘上、卡片却从库里消失，那几十 GB 就没有
        // 任何界面管得到了——用户 2026-09-18 在真机上踩的正是这个。
        let rec = db::get_store_model("card-keep")
            .unwrap()
            .expect("仅卸载不许把卡片从模型库里抹掉");
        assert_eq!(rec.status, "downloaded", "卸载后应当退回「已下载」");
        assert!(!rec.running, "服务都卸了，不能还挂着运行中");
        assert!(
            rec.deployed_at.is_none(),
            "部署痕迹要一起清干净，否则界面会把一个不存在的服务显示成已部署"
        );
        assert!(rec.container_name.is_none(), "容器已删，容器名不能留");
        assert!(
            rec.endpoint_url.is_none(),
            "服务地址已失效，不能留给网关继续转发"
        );
        assert!(
            outcome.record_kept,
            "record_kept 要如实告诉调用方卡片还在库里"
        );
        assert!(
            outcome.summary.contains("仍在模型库"),
            "这句话要让用户知道卡片还在、可以直接重新部署：{}",
            outcome.summary
        );
    }

    /// 防的是「用户选了『彻底删除』，权重却没删干净，界面还报了个释放量」。
    ///
    /// 真机上出现过：卡片卸载后权重还在盘上白占几十上百 GB，而界面照着
    /// `rec.size_bytes` 说"已释放 XX GB"，用户完全没机会发现。所以这里既钉
    /// "真的删掉了"，也钉"报的是实测差值而不是登记体积"——两个数字刻意造得
    /// 差好几个数量级，一旦有人把 freed_bytes 换回登记值，这条会当场红。
    #[test]
    fn purging_the_weights_really_deletes_them_and_reports_measured_bytes() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        let weights = models_dir.join("Ornith-1.5-35B-A3B");
        let real_bytes = make_fake_weights(&weights, 4096);

        add_record_with_uninstall_script("card-purge", "Org/Ornith-1.5-35B-A3B", 67_000_000_000);
        let mgr = manager_with_models_dir(&models_dir);
        let outcome = mgr
            .uninstall("card-purge", true, &mut |_| {}, Default::default())
            .expect("卸载应当成功");

        assert!(!weights.exists(), "用户选了彻底删除，权重目录必须真的没了");
        assert!(outcome.purged_weights);
        assert_eq!(
            outcome.freed_bytes, real_bytes,
            "释放量必须是实测的字节数，不是 card.json 登记的标称体积"
        );
        assert_ne!(
            outcome.freed_bytes as i64, outcome.recorded_bytes,
            "登记 67GB、实删 4KB，两个数字混用就是在骗人"
        );
        assert!(
            outcome.remaining_paths.is_empty(),
            "删干净了就不该报残留：{:?}",
            outcome.remaining_paths
        );
        assert!(
            outcome.summary.contains("删除模型权重") && !outcome.summary.contains("保留"),
            "这句话会直接显示给用户，得说清权重真的没了：{}",
            outcome.summary
        );
        // 权重没了，记录就必须一起走：留着就是一条指向空气的死数据，用户点"部署"
        // 才会发现权重根本不在。这是「删除」和「卸载」的分界线。
        assert!(
            db::get_store_model("card-purge").unwrap().is_none(),
            "彻底删除必须把卡片从模型库里移除"
        );
        assert!(!outcome.record_kept);
    }

    /// 同 [`add_record_with_uninstall_script`]，但脚本删的目录名可以指定——
    /// 复现厂商部署脚本「实际权重落在带消歧后缀的目录」的形态（真机：
    /// `Qwen3.8-27B-NVFP4-unsloth`，与 repo 末段猜出的目录名不同）。
    fn add_record_with_uninstall_script_for_dir(card_id: &str, repo: &str, dir_leaf: &str) {
        add_stuck_record(card_id, repo, 0);
        let script = format!(
            "#!/bin/bash\n\
             set -euo pipefail\n\
             if [ \"${{LMM_PURGE_WEIGHTS:-0}}\" = \"1\" ]; then\n\
             \x20   rm -rf -- \"$LMM_MODELS_DIR/{dir_leaf}\"\n\
             \x20   echo '权重已删除'\n\
             else\n\
             \x20   echo '按选择保留权重'\n\
             fi\n"
        );
        let blobs: BTreeMap<String, db::ScriptBlob> = [(
            "uninstall".to_string(),
            db::ScriptBlob {
                content: script,
                sha256: String::new(),
            },
        )]
        .into_iter()
        .collect();
        db::set_scripts_content(card_id, &blobs, &Default::default()).unwrap();
    }

    /// 2026-11-03 真机事故（cube-9fde）：下线卡 `unsloth/Qwen3.8-27B-NVFP4` 与
    /// 保留卡 `RadixArk/Qwen3.8-27B-NVFP4` 的 repo 末段相同，猜出的权重目录是
    /// 同一条。用户删下线卡：脚本把自己真正的权重（`-unsloth` 后缀目录）删得
    /// 干干净净，收尾判定却看见保留卡的 21G 活权重"仍在磁盘上"→ 卡进
    /// 「需要继续清理」，重试永远失败，唯一出路是强制移除——还被提示去手动
    /// 清理那个目录。他卡拥有的路径不得算本卡的删除残留。
    #[test]
    fn purge_completes_when_a_sibling_card_shares_the_guessed_weights_dir() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        // 保留卡的活权重：整个卸载动作从头到尾不许碰它
        let sibling_weights = models_dir.join("Qwen3.8-27B-NVFP4");
        make_fake_weights(&sibling_weights, 4096);
        // 下线卡自己的权重在带消歧后缀的目录里，脚本按约定删掉它
        let own_weights = models_dir.join("Qwen3.8-27B-NVFP4-unsloth");
        let own_bytes = make_fake_weights(&own_weights, 4096);

        add_record_with_uninstall_script_for_dir(
            "card-retired",
            "unsloth/Qwen3.8-27B-NVFP4",
            "Qwen3.8-27B-NVFP4-unsloth",
        );
        // 部署过的卡都会留下 .deploy-env，记着真实权重位置（带消歧后缀）
        let vendor = config::data_dir()
            .join("cards")
            .join("card-retired")
            .join("vendor");
        std::fs::create_dir_all(&vendor).unwrap();
        std::fs::write(
            vendor.join(".deploy-env"),
            format!("MODEL_DIR=\"{}\"\n", own_weights.display()),
        )
        .unwrap();
        add_stuck_record("card-kept", "RadixArk/Qwen3.8-27B-NVFP4", 21_000_000_000);

        let outcome = manager_with_models_dir(&models_dir)
            .uninstall("card-retired", true, &mut |_| {}, Default::default())
            .expect("自己的产物删干净了就该判完成，而不是卡进死循环");

        assert!(!own_weights.exists(), "本卡自己的权重必须真的删掉");
        assert!(
            sibling_weights.join("model.safetensors").exists(),
            "保留卡的活权重一个字节都不能少"
        );
        assert!(
            outcome.remaining_paths.is_empty(),
            "他卡的目录不得算本卡残留：{:?}",
            outcome.remaining_paths
        );
        // 等号钉不死（purge 收尾还会删 cards/<id> 里的部署文件），钉下界：
        // 量不到自己目录的字节就说明 hint/清单又退回纯猜了。
        assert!(
            outcome.freed_bytes >= own_bytes,
            "量到的必须含自己目录的真实字节：{} < {own_bytes}",
            outcome.freed_bytes
        );
        assert!(
            db::get_store_model("card-retired").unwrap().is_none(),
            "彻底删除必须移除记录"
        );
        let kept = db::get_store_model("card-kept")
            .unwrap()
            .expect("保留卡的记录不许被顺手删掉");
        assert_eq!(kept.status, "deployed");
    }

    /// 卡片的真实权重落在带消歧后缀的目录（`.deploy-env` 记着），猜路径猜不到
    /// 它。修复前 23G 权重删了、"实测释放"却报 0——数字是从猜的清单上量出来
    /// 的。hint 入列后要量到真字节。
    #[test]
    fn deploy_env_hint_counts_the_real_weights_dir_in_freed_bytes() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        // 猜的目录（repo 末段）在磁盘上不存在——本卡的实际权重在别处
        let actual = models_dir.join("Qwen3.8-27B-NVFP4-unsloth");
        let real_bytes = make_fake_weights(&actual, 4096);

        add_record_with_uninstall_script_for_dir(
            "card-hint",
            "unsloth/Qwen3.8-27B-NVFP4",
            "Qwen3.8-27B-NVFP4-unsloth",
        );
        let vendor = config::data_dir()
            .join("cards")
            .join("card-hint")
            .join("vendor");
        std::fs::create_dir_all(&vendor).unwrap();
        std::fs::write(
            vendor.join(".deploy-env"),
            format!("MODEL_DIR=\"{}\"\n", actual.display()),
        )
        .unwrap();

        let outcome = manager_with_models_dir(&models_dir)
            .uninstall("card-hint", true, &mut |_| {}, Default::default())
            .expect("卸载应当成功");
        assert!(!actual.exists());
        // 等号钉不死：purge 收尾还会删掉 cards/<id> 里的部署文件（含这份
        // .deploy-env 本身）。钉下界就够了——修复前 hint 不入列，量到的只有
        // 几十字节的部署文件，远够不到真权重的字节数。
        assert!(
            outcome.freed_bytes >= real_bytes,
            "实测释放必须量到 .deploy-env 指向的真权重目录：{} < {real_bytes}",
            outcome.freed_bytes
        );
        assert!(
            outcome.remaining_paths.is_empty(),
            "删干净了就不该报残留：{:?}",
            outcome.remaining_paths
        );
    }

    /// `.deploy-env` 是卡片脚本写的文件，内容不可信：指到 models_dir 外、
    /// 指到多层子目录的一律当没有。这里只做测量与报告，但校验必须先行——
    /// 防的是未来有人拿 hint 去做删除。
    #[test]
    fn deploy_env_hint_rejects_paths_outside_the_models_dir() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        std::fs::create_dir_all(&models_dir).unwrap();
        add_stuck_record("card-hint-guard", "unsloth/Qwen3.8-27B-NVFP4", 1);
        let vendor = config::data_dir()
            .join("cards")
            .join("card-hint-guard")
            .join("vendor");
        std::fs::create_dir_all(&vendor).unwrap();
        let mgr = manager_with_models_dir(&models_dir);
        let rec = mgr.get_record("card-hint-guard").unwrap();

        for bad in [
            format!("MODEL_DIR=\"/etc\"\n"),
            format!("MODEL_DIR=\"{}\"\n", models_dir.join("sub/inner").display()),
            format!("MODEL_DIR=\"{}\"\n", models_dir.parent().unwrap().display()),
            "PORT=30000\n".to_string(),
        ] {
            std::fs::write(vendor.join(".deploy-env"), bad).unwrap();
            assert!(
                mgr.deployed_weights_hint(&rec).is_none(),
                "逃出 models_dir 或非 MODEL_DIR 行必须当没有"
            );
        }
        let good = models_dir.join("Qwen3.8-27B-NVFP4-unsloth");
        std::fs::write(
            vendor.join(".deploy-env"),
            format!("MODEL_DIR=\"{}\"\n", good.display()),
        )
        .unwrap();
        assert_eq!(mgr.deployed_weights_hint(&rec), Some(good));
        std::fs::remove_file(vendor.join(".deploy-env")).unwrap();
        assert_eq!(mgr.deployed_weights_hint(&rec), None);
    }

    /// r21 登记 P3「多 ollama 卡互为 purge 残留死循环」：所有 ollama 卡的 blob
    /// 同住 `~/.ollama`。删 A 卡时该目录因 B 卡仍在而"存在"→ A 永远
    /// 「需要继续清理」。它是 B 卡的候选路径，不算 A 的残留。
    #[test]
    fn purging_one_ollama_card_does_not_stall_on_the_shared_blob_dir() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        // TempHome 已把 HOME 指到临时目录：在这里造出 ollama 的 blob 布局
        let ollama_blobs = tmp.path().join(".ollama").join("models").join("blobs");
        std::fs::create_dir_all(&ollama_blobs).unwrap();
        std::fs::write(ollama_blobs.join("sha256-weights"), [0u8; 64]).unwrap();

        for card_id in ["card-ollama-a", "card-ollama-b"] {
            db::add_store_model(db::AddStoreModelParams {
                card_id,
                model_name: "qwen3:32b",
                card_name: "Qwen3 32B",
                org: None,
                description: None,
                engine_name: "ollama",
                engine_version: None,
                size_bytes: 20_000_000_000,
                quantization: None,
                source_repo_id: None,
                source_filename: None,
                scripts: &Default::default(),
                status: "deployed",
                profiles: &[],
                deploy_topology: DeployTopology::Single,
            })
            .unwrap();
        }
        // A 卡的卸载脚本：blob 由 ollama 服务自己管（ollama rm），这里只复现
        // "脚本成功结束、共享 blob 根仍在"的收尾现场。
        let blobs: BTreeMap<String, db::ScriptBlob> = [(
            "uninstall".to_string(),
            db::ScriptBlob {
                content: "#!/bin/bash\nexit 0\n".into(),
                sha256: String::new(),
            },
        )]
        .into_iter()
        .collect();
        db::set_scripts_content("card-ollama-a", &blobs, &Default::default()).unwrap();

        manager_with_models_dir(&models_dir)
            .uninstall("card-ollama-a", true, &mut |_| {}, Default::default())
            .expect("共享 blob 根是 B 卡的，不该把 A 卡卡进「需要继续清理」");
        assert!(
            ollama_blobs.join("sha256-weights").exists(),
            "B 卡还在用，blob 根必须原样保留"
        );
        assert!(db::get_store_model("card-ollama-a").unwrap().is_none());
        assert!(
            db::get_store_model("card-ollama-b").unwrap().is_some(),
            "B 卡记录不许被顺手删掉"
        );
    }

    /// 防的是「脚本没认 LMM_PURGE_WEIGHTS，界面却照样宣布权重已删除」。
    ///
    /// 老卡片（按规范 1.0 写的）根本不知道有这个变量，收到 `=1` 也只会保留权重。
    /// 这时候如果 LMM 顺着"用户点的是彻底删除"去报一句"已释放 67GB"，用户就会
    /// 带着错误的认知去清理磁盘。结论必须来自**卸完之后磁盘的实际样子**。
    #[test]
    fn a_card_that_ignores_the_flag_cannot_make_us_claim_the_weights_are_gone() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        let weights = models_dir.join("Ornith-1.5-35B-A3B");
        make_fake_weights(&weights, 4096);

        add_stuck_record("card-legacy", "Org/Ornith-1.5-35B-A3B", 67_000_000_000);
        // 老脚本：完全不看 LMM_PURGE_WEIGHTS，权重一律保留
        let blobs: BTreeMap<String, db::ScriptBlob> = [(
            "uninstall".to_string(),
            db::ScriptBlob {
                content: "#!/bin/bash\necho '服务已卸载'\n".to_string(),
                sha256: String::new(),
            },
        )]
        .into_iter()
        .collect();
        db::set_scripts_content("card-legacy", &blobs, &Default::default()).unwrap();

        let error = manager_with_models_dir(&models_dir)
            .uninstall("card-legacy", true, &mut |_| {}, Default::default())
            .expect_err("删不干净不能伪装成成功");

        assert!(
            error.to_string().contains("尚未全部删除")
                && error.to_string().contains("Ornith-1.5-35B-A3B"),
            "失败要明确告诉用户还有什么没清：{error}"
        );
        let rec = db::get_store_model("card-legacy")
            .unwrap()
            .expect("残留权重时记录必须留下，不能把文件变成看不见的孤儿");
        assert_eq!(rec.status, "cleanup_required");
        assert!(!rec.running);
        assert!(weights.exists(), "脚本没删的权重仍在，应由后续继续清理接管");
    }

    // ------------------------------------------------ 下载：整段编排的端到端

    // StoreError 是 store_client 里 AppError 的别名，跟 ModelManagerError 同一个
    // 类型；这里写 trait 声明用的那个名字，读的人不用再去推一遍。
    use super::super::store_client::StoreError;

    /// 只认一张卡的假商店。`download_from_store` 第一步就是 `store.get_card`，
    /// 没有它这段编排一行都跑不起来。
    struct OneCardStore {
        card: StoreCardDetail,
    }

    impl StoreSource for OneCardStore {
        fn list_cards(&self) -> Result<Vec<StoreCardSummary>, StoreError> {
            Ok(vec![self.card.summary.clone()])
        }

        fn get_card(&self, card_id: &str) -> Result<StoreCardDetail, StoreError> {
            if card_id == self.card.summary.id {
                Ok(self.card.clone())
            } else {
                Err(StoreError::new(format!("商店里没有这张卡：{card_id}")))
            }
        }
    }

    /// 探针约定齐全的 install_engine：装过了探针就退 0（整段跳过安装），没装过
    /// 才真装。真装时往脚印文件里追一行，测试据此分辨"到底跑了几次"——只看
    /// 返回值分不出"跳过了"和"又装了一遍"。
    const FAKE_INSTALL_ENGINE: &str = r#"#!/bin/sh
if [ "${LMM_PROBE_ONLY:-0}" = "1" ]; then
    if [ -f "$LMM_MODELS_DIR/engine-installed" ]; then
        exit 0
    fi
    echo 'probe: engine missing' >&2
    exit 1
fi
mkdir -p "$LMM_MODELS_DIR"
echo ran >> "$LMM_MODELS_DIR/engine-installed"
echo LMM_ENGINE_IMAGE=example/vllm:test
"#;

    /// 下载脚本，同样留脚印。
    const FAKE_DOWNLOAD_MODEL: &str = r#"#!/bin/sh
mkdir -p "$LMM_MODELS_DIR"
echo "$LMM_MODEL_NAME" >> "$LMM_MODELS_DIR/download-ran"
echo '权重就绪'
"#;

    const FAKE_UNINSTALL: &str = "#!/bin/sh\necho '已卸载'\n";

    /// 脚印文件里有几行 = 那个脚本真正跑了几次。文件不在就是 0 次。
    fn ran_times(marker: &std::path::Path) -> usize {
        std::fs::read_to_string(marker)
            .map(|s| s.lines().count())
            .unwrap_or(0)
    }

    /// 造一张「脚本正文是本地文件」的卡片：scripts 的值是本地路径而不是 https URL，
    /// 于是整段下载编排能在不起服务器的前提下真跑一遍——包括"登记哈希对不上就中止"
    /// 这条只在 fetch 路径上才成立的判据。
    ///
    /// 放行这种地址的那一位（`ModelManager::local_script_paths`）**只有测试能置真**，
    /// 生产构造函数出来的管理器一律是关着的（那里有为什么，
    /// `nothing_in_production_may_turn_on_local_script_paths` 钉着）。它以前是内置
    /// 测试商店的生产开关，测试商店拿掉之后就只剩这一个用处了。
    ///
    /// `tampered` 指定哪个脚本的**登记哈希**故意写错：内容没动，动的是商店说
    /// 它应该是什么——正是"传输被篡改/后台数据被改"到达客户端时的样子。
    fn local_script_card(dir: &std::path::Path, tampered: Option<&str>) -> StoreCardDetail {
        std::fs::create_dir_all(dir).unwrap();
        let mut urls = BTreeMap::new();
        let mut hashes = BTreeMap::new();
        for (script_type, body) in [
            ("install_engine", FAKE_INSTALL_ENGINE),
            ("download_model", FAKE_DOWNLOAD_MODEL),
            ("uninstall", FAKE_UNINSTALL),
        ] {
            let path = dir.join(format!("{script_type}.sh"));
            std::fs::write(&path, body).unwrap();
            urls.insert(script_type.to_string(), path.to_string_lossy().to_string());
            hashes.insert(
                script_type.to_string(),
                if tampered == Some(script_type) {
                    script_runner::sha256_hex("完全是另外一份内容")
                } else {
                    script_runner::sha256_hex(body)
                },
            );
        }
        let mut card = make_card("Ornith 1.5 35B", None);
        card.summary.id = "card-dl".into();
        card.summary.engine_name = "vllm".into();
        card.summary.engine_version = Some("v0.27.1".into());
        card.summary.size_bytes = 4096;
        card.source_repo_id = Some("Org/Ornith-1.5-35B-A3B".into());
        card.scripts = urls;
        card.script_sha256 = Some(hashes);
        card
    }

    fn download_settings(models_dir: &std::path::Path) -> Settings {
        Settings {
            models_dir: models_dir.to_string_lossy().to_string(),
            staging_dir: models_dir.join("staging").to_string_lossy().to_string(),
            // 默认要留 2GB 安全余量。这几条盯的是编排本身，不该跟着跑测试那台
            // 机器还剩多少盘走，所以清零；需要"空间不够"的那条自己调上去。
            min_free_space_gb: 0.0,
            ..Default::default()
        }
    }

    fn manager_with_card(settings: Settings, card: StoreCardDetail) -> ModelManager {
        ModelManager::new(Some(settings), Some(Box::new(OneCardStore { card })))
            .with_local_script_paths()
    }

    fn updated_engine_deployment(
        replacement_install: Option<&str>,
        common: Option<&str>,
    ) -> (config::TempHome, ModelManager, PathBuf) {
        let home = setup_temp_db();
        let models = home.path().join("models");
        let manager = manager_with_card(
            download_settings(&models),
            local_script_card(&home.path().join("scripts"), None),
        );
        manager
            .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .unwrap();
        let rec = db::get_store_model("card-dl").unwrap().unwrap();
        let mut scripts = rec.scripts_content.clone();
        let blob = |text: &str| db::ScriptBlob {
            content: text.into(),
            sha256: script_runner::sha256_hex(text),
        };
        scripts.insert(
            "install_engine".into(),
            blob(&replacement_install.map(str::to_string).unwrap_or_else(|| {
                FAKE_INSTALL_ENGINE.replace("engine-installed", "engine-updated")
            })),
        );
        scripts.insert("deploy".into(), blob(
            "#!/bin/sh\n[ -f \"$LMM_MODELS_DIR/engine-updated\" ] || { echo 'ERROR: 引擎未就绪' >&2; exit 1; }\necho ran >> \"$LMM_MODELS_DIR/deploy-ran\"\n",
        ));
        let mut assets = rec.assets_content.clone();
        if let Some(common) = common {
            assets.insert("common.sh".into(), blob(common));
        }
        db::replace_card_package(&rec, &scripts, &assets).unwrap();
        (home, manager, models)
    }

    #[test]
    fn deploy_after_script_update_installs_missing_engine_without_downloading_weights() {
        let (_home, manager, models) = updated_engine_deployment(None, None);
        assert!(
            db::get_installed_engine("vllm", Some("v0.27.1"))
                .unwrap()
                .is_some()
        );
        assert!(!models.join("engine-updated").exists());
        manager
            .deploy("card-dl", None, &mut |_| {}, Default::default())
            .unwrap();
        assert_eq!(ran_times(&models.join("engine-updated")), 1);
        assert_eq!(ran_times(&models.join("deploy-ran")), 1);
        assert_eq!(ran_times(&models.join("download-ran")), 1);
        assert!(
            models.join("engine-installed").exists(),
            "旧引擎不能被更新流程删除"
        );
        assert_eq!(
            db::get_store_model("card-dl").unwrap().unwrap().status,
            "deployed"
        );
        manager
            .deploy("card-dl", None, &mut |_| {}, Default::default())
            .unwrap();
        assert_eq!(
            ran_times(&models.join("engine-updated")),
            1,
            "已就绪的依赖应复用"
        );
        assert_eq!(ran_times(&models.join("deploy-ran")), 2);
    }

    #[test]
    fn deploy_engine_install_failure_keeps_weights_and_never_executes_deploy() {
        let failed = "#!/bin/sh\nif [ \"${LMM_PROBE_ONLY:-0}\" = 1 ]; then exit 1; fi\necho 'ERROR: 磁盘空间不足' >&2\nexit 1\n";
        let (_home, manager, models) = updated_engine_deployment(Some(failed), None);
        let before = db::get_installed_engine("vllm", Some("v0.27.1"))
            .unwrap()
            .unwrap();
        let error = manager
            .deploy("card-dl", None, &mut |_| {}, Default::default())
            .unwrap_err();
        assert!(error.to_string().contains("磁盘空间不足"), "{error}");
        assert_eq!(ran_times(&models.join("deploy-ran")), 0);
        assert_eq!(ran_times(&models.join("download-ran")), 1);
        let after = db::get_installed_engine("vllm", Some("v0.27.1"))
            .unwrap()
            .unwrap();
        assert_eq!(before.install_script_sha256, after.install_script_sha256);
        assert_eq!(
            db::get_store_model("card-dl").unwrap().unwrap().status,
            "downloaded"
        );
    }

    /// r21 P1-1：deploy 的状态门。cleanup_required 的卡被误点部署时，必须在跑
    /// 任何脚本之前拒绝，且不得把状态改写成 incomplete（「用继续下载掩盖清理态」
    /// 正是 db.rs 注释禁止的）；incomplete / 缺 weights_ready_at（升级前老库）
    /// 同样先拒、指「继续下载」。
    #[test]
    fn deploy_status_gate_rejects_cleanup_required_and_incomplete_records() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        let mgr = manager_with_card(
            download_settings(&models_dir),
            local_script_card(&tmp.path().join("scripts"), None),
        );
        mgr.download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .unwrap();

        db::mark_cleanup_required("card-dl").unwrap();
        let error = mgr
            .deploy("card-dl", None, &mut |_| {}, Default::default())
            .unwrap_err();
        assert!(
            error.to_string().contains("继续清理"),
            "cleanup_required 要指引继续清理：{error}"
        );
        let rec = db::get_store_model("card-dl").unwrap().unwrap();
        assert_eq!(rec.status, "cleanup_required", "拒绝路径不得改写状态");
        assert!(
            rec.weights_ready_at.is_some(),
            "拒绝路径不得顺手抹掉下载凭据"
        );

        db::mark_weights_incomplete("card-dl").unwrap();
        let error = mgr
            .deploy("card-dl", None, &mut |_| {}, Default::default())
            .unwrap_err();
        assert!(
            error.to_string().contains("继续下载"),
            "incomplete 要指引继续下载：{error}"
        );
        let rec = db::get_store_model("card-dl").unwrap().unwrap();
        assert_eq!(rec.status, "incomplete");
    }

    /// r20 P2-5 的守卫接线（r21 补测试）：deploy 撞「权重不完整」要走
    /// 抹身份前的等价停验——实例仍在（running=1）时保留记录原样报错；
    /// 真停了才归位成 incomplete。此前这条守卫零接线测试。
    #[test]
    fn weights_incomplete_reset_keeps_records_whose_instance_is_still_alive() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        let mgr = manager_with_card(
            download_settings(&models_dir),
            local_script_card(&tmp.path().join("scripts"), None),
        );
        mgr.download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .unwrap();
        // 换一个「报权重不完整」的 deploy 脚本（模型权重不完整是
        // model_weights_are_incomplete 的强信号标记）。
        let rec = db::get_store_model("card-dl").unwrap().unwrap();
        let mut scripts = rec.scripts_content.clone();
        let deploy_body = "#!/bin/sh\necho '模型权重不完整，请先下载模型' >&2\nexit 1\n";
        scripts.insert(
            "deploy".into(),
            db::ScriptBlob {
                content: deploy_body.into(),
                sha256: script_runner::sha256_hex(deploy_body),
            },
        );
        db::replace_card_package(&rec, &scripts, &rec.assets_content).unwrap();

        // 实例仍在：停验必须拒绝归位，记录一字不动。
        db::set_running("card-dl", true).unwrap();
        let error = mgr
            .deploy("card-dl", None, &mut |_| {}, Default::default())
            .unwrap_err();
        assert!(
            error.to_string().contains("仍有实例在运行"),
            "活实例脚下不得归位：{error}"
        );
        let rec = db::get_store_model("card-dl").unwrap().unwrap();
        assert_eq!(rec.status, "downloaded", "拒绝路径不得改写状态");
        assert!(rec.weights_ready_at.is_some());
        assert!(rec.running, "拒绝路径不得清 running 标志");

        // 真停了：归位照常发生（这正是守卫要保护的正确路径）。
        db::set_running("card-dl", false).unwrap();
        let error = mgr
            .deploy("card-dl", None, &mut |_| {}, Default::default())
            .unwrap_err();
        assert!(
            error.to_string().contains("继续下载"),
            "停验通过后应归位为继续下载：{error}"
        );
        let rec = db::get_store_model("card-dl").unwrap().unwrap();
        assert_eq!(rec.status, "incomplete");
        assert!(rec.weights_ready_at.is_none());
    }

    /// r20 P2-8 的启用预检接线（r21 补测试）：running=1 直接拒；端点仍有响应
    /// （遗留实例）也拒；端点确定没人听时放行（后续失败是脚本层的事，预检
    /// 不背锅）。此前这条守卫零接线测试。
    #[test]
    fn enable_precheck_rejects_running_records_and_live_endpoints() {
        use std::io::{Read as _, Write as _};

        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        let mgr = manager_with_card(
            download_settings(&models_dir),
            local_script_card(&tmp.path().join("scripts"), None),
        );
        mgr.download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .unwrap();
        db::update_store_model_status("card-dl", "deployed", None).unwrap();

        // ① DB 已说在跑 → 拒绝，别在活实例上再叠一个。
        db::set_running("card-dl", true).unwrap();
        let error = mgr
            .enable("card-dl", &mut |_| {}, Default::default())
            .unwrap_err();
        assert!(
            error.to_string().contains("请勿重复启用"),
            "running=1 必须拒绝：{error}"
        );

        // ② 端点仍有响应（遗留实例）→ 拒绝并指引自助恢复。
        db::set_running("card-dl", false).unwrap();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        std::thread::spawn(move || {
            while let Ok((mut socket, _)) = listener.accept() {
                let mut buf = [0_u8; 1024];
                let _ = socket.read(&mut buf);
                let _ = socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n");
                let _ = socket.flush();
            }
        });
        db::set_endpoint_info(
            "card-dl",
            Some(&format!("http://127.0.0.1:{port}/v1")),
            Some("model-id"),
        )
        .unwrap();
        let error = mgr
            .enable("card-dl", &mut |_| {}, Default::default())
            .unwrap_err();
        assert!(
            error.to_string().contains("双实例"),
            "端点活着的遗留实例必须拒绝：{error}"
        );

        // ③ 端点确定没人听 → 预检放行（失败原因是夹具没有 enable 脚本，
        // 而不是预检拦的）。
        let closed = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let closed_port = closed.local_addr().unwrap().port();
        drop(closed);
        db::set_endpoint_info(
            "card-dl",
            Some(&format!("http://127.0.0.1:{closed_port}/v1")),
            Some("model-id"),
        )
        .unwrap();
        let error = mgr
            .enable("card-dl", &mut |_| {}, Default::default())
            .unwrap_err();
        assert!(
            !error.to_string().contains("双实例"),
            "端口确定没人听时预检必须放行：{error}"
        );
        assert!(
            error.to_string().contains("enable 脚本"),
            "放行后失败应来自脚本层：{error}"
        );
    }

    /// r21 P2-5 的判停新判据：/api/ps 能区分「服务在但模型未加载」（放行）与
    /// 「模型仍在显存」（拒绝）；查不动是三态里的保守侧。
    #[test]
    fn ollama_model_loaded_distinguishes_loaded_from_idle() {
        use std::io::{Read as _, Write as _};

        let body = r#"{"models":[{"name":"qwen3:8b","size_vram":8589934592}]}"#;
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        std::thread::spawn(move || {
            for _ in 0..2 {
                let Ok((mut socket, _)) = listener.accept() else {
                    return;
                };
                let mut buf = [0_u8; 1024];
                let _ = socket.read(&mut buf);
                let response = format!(
                    "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                    body.len(),
                    body
                );
                let _ = socket.write_all(response.as_bytes());
                let _ = socket.flush();
            }
        });
        let host = format!("http://127.0.0.1:{port}");
        assert_eq!(
            ollama_model_loaded(&host, "qwen3:8b"),
            Some(true),
            "api/ps 里列着的就是在跑"
        );
        assert_eq!(
            ollama_model_loaded(&host, "llama3:70b"),
            Some(false),
            "服务在、模型未加载 = 卸载/更新可放行的稳态"
        );

        // 查不动（端口上没有服务）→ None（保守侧，调用方拒绝）。
        let closed = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let closed_port = closed.local_addr().unwrap().port();
        drop(closed);
        assert_eq!(
            ollama_model_loaded(&format!("http://127.0.0.1:{closed_port}"), "qwen3:8b"),
            None
        );
    }

    #[test]
    fn deploy_cancelled_before_engine_install_has_no_install_or_deploy_side_effects() {
        let (_home, manager, models) = updated_engine_deployment(None, None);
        let control = script_runner::RunControl {
            cancel: Some(std::sync::Arc::new(std::sync::atomic::AtomicBool::new(
                true,
            ))),
            ..Default::default()
        };
        let error = manager
            .deploy("card-dl", None, &mut |_| {}, control)
            .unwrap_err();
        assert!(error.to_string().contains("已取消"));
        assert!(!models.join("engine-updated").exists());
        assert!(!models.join("deploy-ran").exists());
    }

    #[test]
    fn deploy_rejects_native_installer_success_without_a_ready_engine() {
        let install = "#!/bin/sh\nCARD_DIR=\"$(dirname \"$0\")\"\n. \"$CARD_DIR/common.sh\"\nif [ \"${LMM_PROBE_ONLY:-0}\" = 1 ]; then exit 1; fi\necho 'claimed installed'\n";
        let common = format!(
            "ENGINE_BIN_SHA256=\"{}\"\n# local-model-manager/engines/tachyon-serve\n",
            "a".repeat(64)
        );
        let (_home, manager, models) = updated_engine_deployment(Some(install), Some(&common));
        let error = manager
            .deploy("card-dl", None, &mut |_| {}, Default::default())
            .unwrap_err();
        assert!(error.to_string().contains("完整性检查"), "{error}");
        assert!(!models.join("deploy-ran").exists());
        assert_eq!(ran_times(&models.join("download-ran")), 1);
    }

    /// 防的是「下载中途失败，库里却留下一条说它已经好了的记录」。
    ///
    /// 失败点挑的是哈希对不上，因为它落在编排的中段：整套脚本已经取过一轮
    /// （失败后退回按需获取）、引擎探针也跑过了，再往下才炸。这一段里有两处
    /// 写库（`mark_engine_installed` 和 `add_store_model`），失败之后两处都必须
    /// 是干净的：
    ///   ① 全新卡片：一条记录都不能留——留下就是一张点开才发现没有权重的死卡；
    ///   ② 已部署过的卡片：原状态必须原封不动——被退回「已下载」或者被删掉，
    ///      用户那边就是一个正跑着的模型忽然从库里消失。
    /// 引擎同样不能被标成"已安装"：那会让下一次下载整段跳过装引擎。
    ///
    /// 断言里先钉住失败原因，再钉库里的状态：只断言"返回了 Err"证明不了这条
    /// 测试盯的是哪个阶段——真在取卡片那一步就失败了，后面几条照样全绿。
    #[test]
    fn a_failed_download_leaves_no_record_claiming_the_model_is_downloaded() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        let mgr = manager_with_card(
            download_settings(&models_dir),
            local_script_card(&tmp.path().join("scripts"), Some("install_engine")),
        );

        // ① 全新卡片
        let err = mgr
            .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .expect_err("登记哈希对不上，这次下载不该成功");
        assert!(
            err.to_string().contains("完整性校验失败"),
            "失败原因必须是校验没过，否则这条测试盯的不是它以为的那个阶段：{err}"
        );
        assert!(
            db::get_store_model("card-dl").unwrap().is_none(),
            "下载没成功，模型库里不能留下记录"
        );
        assert!(
            !db::is_engine_installed("vllm", Some("v0.27.1")).unwrap(),
            "引擎压根没装成，不能登记成已安装"
        );
        assert_eq!(
            ran_times(&models_dir.join("engine-installed")),
            0,
            "校验没过就不该真去装引擎"
        );
        assert_eq!(
            ran_times(&models_dir.join("download-ran")),
            0,
            "更不该跑到下载权重那一步"
        );

        // ② 已经部署好的卡片，重新下载时中途失败
        add_stuck_record("card-dl", "Org/Ornith-1.5-35B-A3B", 168_000_000_000);
        let err = mgr
            .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .expect_err("同样不该成功");
        assert!(err.to_string().contains("完整性校验失败"), "{err}");
        let rec = db::get_store_model("card-dl")
            .unwrap()
            .expect("失败的下载不能把已有记录删掉");
        assert_eq!(
            rec.status, "deployed",
            "下载失败不能动已部署卡片的状态，它那个服务还好好跑着"
        );
    }

    /// 防的是「盘明明放不下，还是照样开跑」。
    ///
    /// 空间预检查是整段编排的第一道闸。它一失效，用户付出的代价是几个小时的
    /// 下载最后死在 ENOSPC，而且中途已经往盘上写了一堆半截文件。两条路径都要
    /// 钉：物理余量不够（判据在 disk 层），以及用户自己设的存储配额被撑破
    /// （判据就在本文件的 `check_can_download` 里）。
    ///
    /// 光断言返回 Err 是不够的——闸门要是挪到下载之后，返回值照样是 Err，但盘
    /// 已经被写过一遍了。所以一并钉住"两个脚本一次都没跑"。
    #[test]
    fn a_download_that_does_not_fit_never_starts() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        let scripts_dir = tmp.path().join("scripts");

        // 物理余量：要求留出一百万 GB，任何机器都满足不了
        let mut tight = download_settings(&models_dir);
        tight.min_free_space_gb = 1_000_000.0;
        let err = manager_with_card(tight, local_script_card(&scripts_dir, None))
            .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .expect_err("余量不够就不该放行");
        assert!(
            err.to_string().contains("可用空间不足"),
            "拦下来的理由必须是余量不够：{err}"
        );

        // 存储配额：盘上放得下，但用户自己设的上限已经被现有模型占满了
        let mut quota = download_settings(&models_dir);
        quota.storage_quota_gb = Some(1.0);
        add_stuck_record("card-other", "Org/Other-Weights", 10_000_000_000);
        let err = manager_with_card(quota, local_script_card(&scripts_dir, None))
            .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .expect_err("超出配额就不该放行");
        assert!(
            err.to_string().contains("超出存储配额"),
            "拦下来的理由必须是配额：{err}"
        );

        assert!(
            db::get_store_model("card-dl").unwrap().is_none(),
            "根本没开跑，不该有记录"
        );
        assert_eq!(
            ran_times(&models_dir.join("engine-installed")),
            0,
            "空间不够却已经把引擎装上了"
        );
        assert_eq!(
            ran_times(&models_dir.join("download-ran")),
            0,
            "空间不够却已经开始下载了"
        );
    }

    /// 防的是「下载跑完了，库里的状态没跟上」——以及反过来，「重下一遍把用户
    /// 已经部署好的状态给退回去」。
    ///
    /// 成功路径上有四处落库，缺一处都是真问题：引擎登记（含这次跑的
    /// install_engine 哈希，下次靠它判断要不要为刷新部署包重跑一遍）、模型记录、
    /// 脚本正文固化（之后部署/启用/卸载全用这份本地副本，缺了它商店一挂用户
    /// 连卸载都卸不掉）、以及状态本身。
    ///
    /// 第二遍下载钉的是另外三条现成约定：探针命中就整段跳过装引擎；
    /// download_model **照跑不误**（库里写着"已下载"不代表权重还在磁盘上，见
    /// `download_from_store` 的方法注释）；已部署的状态不因为重下一遍而回退。
    #[test]
    fn a_successful_download_records_the_engine_the_scripts_and_the_status() {
        let tmp = setup_temp_db();
        let models_dir = tmp.path().join("models");
        let mgr = manager_with_card(
            download_settings(&models_dir),
            local_script_card(&tmp.path().join("scripts"), None),
        );

        let result = mgr
            .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .expect("下载应当成功");
        assert_eq!(result.model_name, "ornith-1.5-35b");
        assert!(!result.engine_reused, "第一次下载，引擎是刚装的");
        assert!(!result.model_reused);
        assert_eq!(
            ran_times(&models_dir.join("engine-installed")),
            1,
            "引擎该实打实装一次"
        );
        assert_eq!(
            ran_times(&models_dir.join("download-ran")),
            1,
            "权重该实打实下一次"
        );

        let engine = db::get_installed_engine("vllm", Some("v0.27.1"))
            .unwrap()
            .expect("引擎必须登记进库，否则下次下载还要再装一遍");
        assert_eq!(
            engine.docker_image.as_deref(),
            Some("example/vllm:test"),
            "install_engine 报上来的镜像要记下来，删除引擎时靠它精确 rmi"
        );
        assert!(
            engine.install_script_sha256.is_some(),
            "要记下这次跑的是哪一版 install_engine，否则商店修好的部署包永远刷不下去"
        );

        let rec = db::get_store_model("card-dl")
            .unwrap()
            .expect("下载完必须落库，否则模型库里根本看不到它");
        assert_eq!(
            rec.status, "downloaded",
            "刚下完就该是「已下载」，不能停在中间态"
        );
        assert_eq!(rec.model_name, result.model_name);
        assert!(
            rec.scripts_content.contains_key("uninstall"),
            "六类脚本的正文要固化到本地，否则商店一挂用户连卸载都卸不掉：{:?}",
            rec.scripts_content.keys().collect::<Vec<_>>()
        );

        // 用户把它部署起来了，之后又点了一次下载
        db::update_store_model_status("card-dl", "deployed", None).unwrap();
        let again = mgr
            .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .expect("重新下载应当成功");
        assert!(again.engine_reused, "引擎已在库里，这次是复用");
        assert!(again.model_reused);
        assert_eq!(
            ran_times(&models_dir.join("engine-installed")),
            1,
            "探针命中就该整段跳过装引擎——真机上多装一遍就是多一个密码框"
        );
        assert_eq!(
            ran_times(&models_dir.join("download-ran")),
            2,
            "download_model 必须照跑：库里写着「已下载」不代表权重还在磁盘上"
        );
        let rec = db::get_store_model("card-dl").unwrap().unwrap();
        assert_eq!(
            rec.status, "deployed",
            "重下一遍不能把已部署的卡片退回「已下载」"
        );
    }

    /// 套一层的错误，专门用来验证「判据要沿错误链往下挖」。
    ///
    /// reqwest 0.13 的 `reqwest::Error` 自己没有 `ErrorKind`，真正的
    /// `std::io::Error` 埋在 hyper 那一层下面。只 downcast 顶层的实现能通过
    /// 「裸 io::Error」那几条用例，却在真机上一条都命中不了——等于悄悄退回成
    /// 原来那个 `Err(_) => true`。所以嵌套这条用例必须单独钉。
    #[derive(Debug)]
    struct NestedError(Box<dyn std::error::Error + Send + Sync + 'static>);

    impl std::fmt::Display for NestedError {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "nested: {}", self.0)
        }
    }

    impl std::error::Error for NestedError {
        fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
            Some(self.0.as_ref())
        }
    }

    fn io_err(kind: std::io::ErrorKind) -> std::io::Error {
        std::io::Error::new(kind, "boom")
    }

    #[test]
    fn only_refused_or_reset_connections_prove_ollama_is_gone() {
        use std::io::ErrorKind;

        // 确定性失败：端口上没有监听者。这是「ollama 已经停了」的主场景，
        // 判不出来的话 reconcile 的 ollama 分支等于不存在。
        for kind in [ErrorKind::ConnectionRefused, ErrorKind::ConnectionReset] {
            assert!(
                probe_failure_means_nothing_listening(&io_err(kind)),
                "{kind:?} 说明那个端口后面没有进程，必须判「服务不在」"
            );
        }

        // 不确定失败：一律保守当活着。超时尤其不能判死——ollama 加载大模型时
        // 整个进程就是会卡住不回，那时候它活得好好的。
        for kind in [
            ErrorKind::TimedOut,
            ErrorKind::NotFound,
            ErrorKind::PermissionDenied,
            ErrorKind::Other,
        ] {
            assert!(
                !probe_failure_means_nothing_listening(&io_err(kind)),
                "{kind:?} 只说明「问不出来」，据此把 running 置 false 就是误纠"
            );
        }

        // 埋在错误链第二层/第三层，照样要挖出来——真机上的 reqwest 错误就长这样。
        let one_deep = NestedError(Box::new(io_err(ErrorKind::ConnectionRefused)));
        assert!(
            probe_failure_means_nothing_listening(&one_deep),
            "只 downcast 顶层的话，真机上的 reqwest 错误一条都命中不了"
        );
        let two_deep = NestedError(Box::new(NestedError(Box::new(io_err(
            ErrorKind::ConnectionRefused,
        )))));
        assert!(probe_failure_means_nothing_listening(&two_deep));
        assert!(!probe_failure_means_nothing_listening(&NestedError(
            Box::new(io_err(ErrorKind::TimedOut))
        )));

        // 整条链里没有 io::Error（建不出客户端那类）：问不出来，当活着。
        assert!(!probe_failure_means_nothing_listening(&AppError::new(
            "建不出 HTTP 客户端"
        )));
    }

    /// 端到端钉住「reqwest 的错误链里真的挖得到 io::Error」这件事。
    ///
    /// 上面那条纯函数用例证明不了这一点：它喂的是我们自己造的错误链，而真正会
    /// 退化的地方恰恰是 reqwest/hyper 哪天换了包装方式。这里绑一个端口、拿到
    /// 端口号之后立刻放掉，于是那个地址上确定没有监听者——不依赖外网，也不需要
    /// 真的装了 ollama。
    #[test]
    fn ollama_alive_reports_dead_when_nothing_listens() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("本机总能绑上");
        let port = listener.local_addr().unwrap().port();
        drop(listener);
        assert!(
            !ollama_alive(&format!("http://127.0.0.1:{port}")),
            "端口上没有任何进程，这正是「ollama 已经停了」的样子，必须判死"
        );
    }

    /// 反向那一半：有人在监听并且好好回话时，绝不能判死。
    ///
    /// 只钉前一半的话，「无脑返回 false」也能让上面那条用例通过，而那正是最坏的
    /// 退化——用户启用着的模型会被一路纠正成「未启用」。
    #[test]
    fn ollama_alive_reports_live_when_something_answers() {
        use std::io::{Read as _, Write as _};

        let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("本机总能绑上");
        let port = listener.local_addr().unwrap().port();
        let server = std::thread::spawn(move || {
            let Ok((mut socket, _)) = listener.accept() else {
                return;
            };
            // 请求头读到哪算哪：我们只需要把连接接下来、回一个 200。
            let mut buf = [0_u8; 1024];
            let _ = socket.read(&mut buf);
            let _ = socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n");
            let _ = socket.flush();
        });

        let alive = ollama_alive(&format!("http://127.0.0.1:{port}"));
        let _ = server.join();
        assert!(alive, "对面好端端地回了 200，不能判成服务不在");
    }

    /// r19 评审 P2-7：成功的端点探测必须进 60s TTL 缓存——同一张卡在同一
    /// 刷新窗口内被 reconcile 反复过问时（library() 每次都触发），第二次
    /// 起直接复用，不再各蹲一次串行超时。服务端只应看到**一次**请求。
    #[test]
    fn successful_endpoint_probe_is_cached_within_ttl() {
        use std::io::{Read as _, Write as _};
        use std::sync::atomic::{AtomicUsize, Ordering};

        let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("本机总能绑上");
        let port = listener.local_addr().unwrap().port();
        let hits = std::sync::Arc::new(AtomicUsize::new(0));
        let counter = hits.clone();
        std::thread::spawn(move || {
            while let Ok((mut socket, _)) = listener.accept() {
                let mut buf = [0_u8; 1024];
                let _ = socket.read(&mut buf);
                counter.fetch_add(1, Ordering::SeqCst);
                let _ = socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n");
                let _ = socket.flush();
            }
        });

        // card_id 用端口号唯一化，避免撞上其他测试（缓存是进程级 static）留在
        // 缓存里的同名卡。
        let card_id = format!("probe-cache-test-{port}");
        let endpoint = format!("http://127.0.0.1:{port}/v1/chat/completions");
        assert!(
            openai_endpoint_alive_cached(&card_id, &endpoint),
            "对面回了 200，探测应判活"
        );
        assert!(
            openai_endpoint_alive_cached(&card_id, &endpoint),
            "60 秒内的重复探测应命中缓存并同样判活"
        );
        assert_eq!(
            hits.load(Ordering::SeqCst),
            1,
            "第二次探测必须命中缓存，服务端只能看到一个请求"
        );
    }

    /// 卡片打印的 `LMM_ENDPOINT_URL=` 内容完全不可信，而它会被原样写成品悟里
    /// 那一行模型的 base_url。放进一个远程地址，用户此后每一句对话都发去那台
    /// 机器，且卸载清理（findLinkedRows 只认回环）永远匹配不到那一行。
    ///
    /// 这把尺子跟前端 `pinvou-model-link.js` 的 `isLocalGatewayUrl` 必须一致，
    /// 两边的用例也是照着对的。
    #[test]
    fn only_loopback_endpoints_are_accepted() {
        for good in [
            "http://127.0.0.1:30001/v1",
            "http://127.0.0.1:30001/v1/chat/completions",
            // 整个 127/8 都是回环，真机上 sglang/vllm 有绑到这些地址的用法。
            "http://127.0.0.2:8000/v1",
            "http://127.255.255.254:8000/v1",
            "http://localhost:11434/v1",
            // 大小写和 FQDN 末尾的点都是合法写法，不能靠它们绕过去。
            // 注意只有**一个**点算合法；两个点的那条在下面的「该拒」组里。
            "http://LocalHost:11434/v1",
            "http://localhost.:11434/v1",
            "http://[::1]:8000/v1",
            "http://[0:0:0:0:0:0:0:1]:8000/v1",
            "https://127.0.0.1:8443/v1",
        ] {
            assert!(endpoint_url_is_loopback(good), "{good} 是本机地址，该放行");
        }

        for bad in [
            // 数据外泄面本体。
            "http://203.0.113.7/v1",
            "https://evil.example.com/v1",
            // 私网/内网也不行：那已经是另一台机器，对话内容就出本机了。
            "http://192.168.1.10:8000/v1",
            "http://10.0.0.5:8000/v1",
            // 子域名骗局：以 localhost / 127.0.0.1 开头，主机却是别人的。
            "http://localhost.evil.example.com/v1",
            "http://127.0.0.1.evil.example.com/v1",
            // 0.0.0.0 不是回环（std 的 is_loopback 对它也是 false），它是
            // 「监听所有网卡」，当成本机地址会把判据悄悄放宽一圈。
            "http://0.0.0.0:8000/v1",
            // IPv4-mapped IPv6 不是回环，别被里面那几个 127 骗了。
            "http://[::ffff:127.0.0.1]:8000/v1",
            // 两个结尾点不是合法的 FQDN 写法。这条曾经 Rust 判 true、JS 判 false
            // （Rust 剥光全部结尾点，JS 只剥一个），两把尺子对不上——现在都只剥一个。
            "http://localhost../v1",
            // 压根不是 URL / 空串：解析不出主机就按不安全处理。
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
    fn openai_style_detection_separates_v1_endpoints_from_generate() {
        // 原生引擎报上来的都是这两种形态：/v1 或 /v1/chat/completions。
        assert!(endpoint_path_is_openai_style("http://127.0.0.1:8000/v1"));
        assert!(endpoint_path_is_openai_style(
            "http://127.0.0.1:8000/v1/chat/completions"
        ));
        // 没写路径（reqwest 归一成 "/"）也算：改写成 /v1/models 是安全的。
        assert!(endpoint_path_is_openai_style("http://127.0.0.1:8000/"));
        assert!(endpoint_path_is_openai_style("http://127.0.0.1:8000"));

        // z-image 的文生图端点：改写路径探测必 404，绝不能按 OpenAI 探测，
        // 否则容器明明 healthy 也会被误标成已停止（真机踩过）。
        assert!(!endpoint_path_is_openai_style(
            "http://127.0.0.1:8000/generate"
        ));
        assert!(!endpoint_path_is_openai_style(
            "http://127.0.0.1:8000/generate?size=1024"
        ));
        // 解析失败按「不可探测」处理，保留原状态而不是清 running。
        assert!(!endpoint_path_is_openai_style("not a url"));
    }
}

/// r7 P1-5：启动时自动收紧存量 775 目录链的行为锁。
/// 构造模拟目录（0775），验证收紧后变 0755；非属主目录不动。
#[test]
fn tighten_directory_permissions_fixes_wide_modes_and_skips_foreign() {
    use std::os::unix::fs::PermissionsExt;
    let _home = config::TempHome::new();
    let data = config::data_dir();
    let cards = data.join("cards");
    let logs = data.join("logs");
    let card_a = cards.join("test-card-a");
    for dir in [&data, &cards, &logs, &card_a] {
        std::fs::create_dir_all(dir).unwrap();
        std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o775)).unwrap();
    }
    // 已正确的目录不动
    let already_tight = cards.join("already-755");
    std::fs::create_dir(&already_tight).unwrap();
    std::fs::set_permissions(&already_tight, std::fs::Permissions::from_mode(0o755)).unwrap();

    // r20 P2-11（r21 补齐）：名字里的 skips_foreign 此前没有对应用例。构造
    // 非属主目录需要 chown 权限——只有 root 有（CI 常规腿以非 root 跑，这里
    // 拿不到就跳过该子断言并说明，不做假断言）；root 环境下真正钉住「属主
    // 不是自己的目录一个字节都不动」。
    let foreign = cards.join("foreign-owned");
    std::fs::create_dir(&foreign).unwrap();
    std::fs::set_permissions(&foreign, std::fs::Permissions::from_mode(0o777)).unwrap();
    let foreign_chowned = {
        use std::os::unix::ffi::OsStrExt as _;
        let c_path = std::ffi::CString::new(foreign.as_os_str().as_bytes()).unwrap();
        // 65534 = nobody/4294967294 沙箱惯用的非特权 uid。
        (unsafe { libc::chown(c_path.as_ptr(), 65534, 65534) } == 0)
    };

    let settings = config::Settings::default();
    tighten_store_directory_permissions(&settings);

    let mode = |p: &std::path::Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode(&data), 0o755, "data 目录应收紧到 755");
    assert_eq!(mode(&cards), 0o755, "cards 目录应收紧到 755");
    assert_eq!(mode(&logs), 0o755, "logs 目录应收紧到 755");
    assert_eq!(mode(&card_a), 0o755, "一级子目录应收紧到 755");
    assert_eq!(mode(&already_tight), 0o755, "已正确的目录不变");
    if foreign_chowned {
        assert_eq!(
            mode(&foreign),
            0o777,
            "属主不是当前用户的目录不许动（哪怕它 777）"
        );
    } else {
        eprintln!("[test] 非 root 环境无法构造非属主目录，跳过 skips_foreign 子断言");
    }
}
