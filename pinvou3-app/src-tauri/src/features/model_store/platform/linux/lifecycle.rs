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
    DeployProfile, DeployTopology, StoreCardDetail, StoreCardSummary, StoreClient, StoreSource,
};

pub type ModelManagerError = AppError;

/// ollama 不是 docker 镜像，没有 LMM_ENGINE_IMAGE 标记可用，删除逻辑必须单独
/// 写死一份：停服务、删二进制、删 systemd unit。这几步都要改系统级文件/服务，
/// 必须走 pkexec 提权，跟 install_engine 是同一套需要 root 的理由。
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
    /// 今天它恒等于 `!purged_weights`，但仍然单独回一个字段：这两件事问的不是
    /// 同一个问题（一个问磁盘、一个问库记录），而调用方拿它决定的是**界面动作**
    /// ——留着这张卡刷新成「已下载」，还是整行移走。让界面去推断"删了权重所以
    /// 记录大概也没了"，等于把一条内部约定钉死在两边。
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
    pub size_bytes: u64,
    pub affected_models: Vec<String>,
}

/// 已下载的模型里，脚本比商店旧的那些。
#[derive(Debug, Clone, serde::Serialize)]
pub struct OutdatedCard {
    pub card_id: String,
    pub card_name: String,
    /// 已部署/运行中——刷新脚本后必须重新部署才会生效，光刷新没用
    pub needs_redeploy: bool,
    pub running: bool,
}

#[derive(Debug, Clone, serde::Serialize)]
pub struct StoreSyncResult {
    /// 商店当前的卡片总数
    pub total: usize,
    /// 已下载但商店已下架的卡片名（本地仍可用，只是不能再重新下载）
    pub delisted: Vec<String>,
    /// 有脚本更新可用的已下载卡片
    pub outdated: Vec<OutdatedCard>,
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
        /// 这些"URL"其实是本地文件路径（内置测试商店：它没有服务器，路径是应用
        /// 自己刚写到 data_dir 下的）。真实商店一律为 false，`fetch_script_content`
        /// 会据此拒绝一切非 https 的地址——见 `script_runner::FetchPolicy`。
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
    /// `local_paths` 来自 `ModelManager::builtin_store`：只有内置测试商店的
    /// scripts/assets 值是本地文件路径，真实商店必须是 https 链接。
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
        std::fs::create_dir_all(&dir)
            .map_err(|e| ModelManagerError::new(format!("创建卡片目录失败：{e}")))?;

        let assets = self.asset_contents(policy)?;
        script_runner::materialize_assets(&dir, &assets)?;

        // 入口脚本也按固定文件名写进包里，这样包内其它脚本也能互相引用
        let entry_content = self.entry_content(script_type, policy)?;
        let mut one = BTreeMap::new();
        one.insert(entry_name.clone(), entry_content);
        script_runner::materialize_assets(&dir, &one)?;

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
    /// 数据源是不是"打包在应用里"的测试商店。决定了固化脚本要不要跟着应用升级
    /// 自动刷新——测试商店没有服务器，应用升级就等于脚本升级。
    builtin_store: bool,
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

impl ModelManager {
    /// 连接**测试模型商店**（卡片和脚本都打包在应用本地，不发网络请求）的
    /// 管理器。业务逻辑与真实商店完全一致，只是换了数据来源。
    pub fn new_test_store() -> Self {
        let mut mgr = Self::new(
            None,
            Some(Box::new(super::store_client::LocalStoreClient::new())),
        );
        mgr.builtin_store = true;
        mgr
    }

    /// `store` 用于注入自定义的商店数据源（比如测试模型商店），不传则按设置里
    /// 的 store_url 连接真正的商店服务器，业务逻辑完全一致。
    pub fn new(settings: Option<Settings>, store: Option<Box<dyn StoreSource>>) -> Self {
        let settings = settings.unwrap_or_else(config::load_settings);
        let store = store.unwrap_or_else(|| Box::new(StoreClient::new(&settings.store_url)));
        let _ = db::init_db();
        Self {
            settings,
            store,
            builtin_store: false,
        }
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
    /// docker 不可用（比如探测超时/命令缺失）时直接跳过，不做任何判断——
    /// 没有可靠信号就不误纠正，宁可漏判也不能错判。
    pub fn reconcile_running_status(&self) -> Vec<String> {
        let mut corrected = Vec::new();
        let Ok(records) = db::list_store_models() else {
            return corrected;
        };
        if records.is_empty() {
            return corrected;
        }
        // 带超时：docker daemon 卡死时 `docker ps` 既不报错也不退出，而这里是
        // `library()` 的第一步——用户看到的会是**整个模型商店永久转圈**，而不是
        // 「拿不到运行状态」。同一文件里 ollama 那条分支早就设了 5 秒超时，
        // 这两处没设，标准不一致本身就是漏了的信号。
        let Some(stdout) =
            proc_timeout::query("docker", &["ps", "--format", "{{.Names}}\t{{.Image}}"])
        else {
            return corrected;
        };
        let mut live_images: std::collections::HashSet<String> = std::collections::HashSet::new();
        let mut live_names: std::collections::HashSet<String> = std::collections::HashSet::new();
        for line in stdout.lines() {
            let mut parts = line.splitn(2, '\t');
            if let Some(n) = parts.next() {
                live_names.insert(n.trim().to_string());
            }
            if let Some(i) = parts.next() {
                live_images.insert(i.trim().to_string());
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

        for rec in records {
            // 有容器名就只认容器名——那是启用时脚本亲口报上来的，是确定的身份。
            if let Some(name) = rec
                .container_name
                .as_deref()
                .map(str::trim)
                .filter(|n| !n.is_empty())
            {
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

            // 没有容器名（这一版之前启用的老记录）：退回按镜像判断，但**只做
            // 「说在跑其实没跑」这一个方向**。反向认领在这里是危险的：别人用
            // 同款镜像起的容器会让我们把自己的卡误标成运行中——真机上就撞见过
            // 同事用同一个 vllm 镜像跑了另一个模型。宁可让用户自己点一下启用。
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
        // 同一块盘就不必查第二遍（绝大多数情况）。不比较挂载点而比较余量：
        // 拿到同一个 free_bytes 基本就是同一个文件系统，而判断挂载点要读
        // /proc/mounts 再做前缀匹配，为这点收益不值当。
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
        env.insert(
            "LMM_HF_TOKEN".into(),
            config::get_hf_token().unwrap_or_default(),
        );
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
                (ScriptsSource::from_card(&card, self.builtin_store), None)
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
                &mut |line| on_progress(&format!("[安装引擎] {line}")),
                control.clone(),
                probe.need_root,
            )?;
            // install_engine 可以打印 LMM_ENGINE_IMAGE= 这一行告诉 LMM 这个引擎
            // 实际用的 docker 镜像是哪个（不是所有引擎都是 docker 镜像，比如
            // ollama 就没有），供"已安装引擎"的删除功能精确 docker rmi。
            docker_image = extract_marker(&install_lines, "LMM_ENGINE_IMAGE=");
        }
        db::mark_engine_installed(
            &card.summary.engine_name,
            card.summary.engine_version.as_deref(),
            docker_image.as_deref(),
            // 记下这次跑的 install_engine 是哪个版本；下次比对哈希决定要不要
            // 为了刷新部署包再跑一遍
            current_hash.as_deref(),
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
            // - 正式商店的远端卡片会复用（2026-09-21 起在架的 7 张全部消费
            //   LMM_IMPORT_DIRS，拿不到才回落到脚本自带的硬编码清单）；
            // - 内置测试商店那批卡片没接（`data/test-store/` 下零命中）。
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
            &mut |line| on_progress(&format!("[下载模型] {line}")),
            control,
            false,
        )?;

        // 已经在库里的话保留原有状态（比如已部署过），不要因为重新走了一遍
        // 下载就把状态退回"仅下载"；只有全新记录才用 "downloaded" 起步。
        let status = existing_status.unwrap_or_else(|| "downloaded".to_string());
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
            card_source: if self.builtin_store {
                "builtin"
            } else {
                "store"
            },
        })
        .map_err(|e| ModelManagerError::new(format!("记录模型下载状态失败：{e}")))?;

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
            local_paths: self.builtin_store,
        }
    }

    /// 抓取卡片六类脚本的正文并固化到本地。只在两个时刻调用：下载模型成功后、
    /// 用户主动点「检查卡片更新」时。抓取过程中任何一个脚本失败都整体放弃，
    /// 不留下半套新半套旧的副本。
    /// 把内置（测试商店）卡片的固化脚本刷新成应用当前版本，返回刷新过的卡片名。
    ///
    /// 应用启动时跑一次。测试商店的脚本打包在二进制里，应用升级就等于脚本升级，
    /// 不该还要用户手动点一次「更新脚本」——真机上就因为这个白查过一轮：推了新
    /// 应用、脚本改动一个都没生效，卡片跑的还是下载那天固化的旧脚本。
    ///
    /// 只碰 `card_source = 'builtin'` 的记录（判断在 db 层），失败不影响启动。
    pub fn refresh_builtin_cards() -> Vec<String> {
        let mut refreshed = Vec::new();
        let store = super::store_client::LocalStoreClient::new();
        for card in super::test_store_data::test_cards() {
            let Ok(detail) = store.get_card(&card.summary.id) else {
                continue;
            };
            // 内置卡的 scripts/assets 值是本地文件路径，fetch 就是读文件，不联网
            let mgr = Self::new_test_store();
            let Ok(pkg) = mgr.fetch_and_store_scripts(&detail) else {
                continue;
            };
            // 只在"确实刷新了"时记一笔；Ok(false)（没变化）和 Err（这张卡刷不动）
            // 都不该打断其余卡片。
            if let Ok(true) =
                db::refresh_builtin_card_scripts(&card.summary.id, &pkg.scripts, &pkg.assets)
            {
                refreshed.push(card.summary.name.clone());
            }
        }
        refreshed
    }

    fn fetch_and_store_scripts(
        &self,
        card: &StoreCardDetail,
    ) -> Result<CardPackage, ModelManagerError> {
        // 来源校验必须在**抓取的这一刻**做：抓完就固化成 Local 副本，之后
        // install_engine 直接跑副本、不再有任何 fetch 可校验。副本里的 sha256
        // 是按内容自算的，证明不了"内容是对的"，只能证明"内容没再变过"。
        let local_paths = self.builtin_store;
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
    /// **目前没有任何调用点**（命令未注册、界面上也没有入口），留待后续分期
    /// 接线。这里原来写的是"在打开应用和打开模型商店时自动调用"——那句话从来
    /// 没有成立过，而它会让读代码的人以为这个能力已经在跑。
    ///
    /// 因此要如实交代当前的实际后果：**远端商店卡片的固化脚本副本永远不会
    /// 过期刷新**。内置（测试商店）卡片有 `refresh_builtin_cards` 在启动时兜底
    /// （lib.rs 里的 `run_startup_refresh` 真的调它），远端卡片没有对应物——
    /// 商店那边修好的脚本只有在用户重新下载这个模型时才会下来。连带地，
    /// `OutdatedCard`、`StoreSyncResult` 以及 `refresh_card_scripts` 目前都到
    /// 不了；`needs_redeploy`（service.rs）虽然活着，但对远端卡片实际上恒为
    /// false，因为它比的那份 `scripts_content` 不会变。
    ///
    /// 设计意图保持不变：只读，不改任何本地副本——检测到有更新只是提醒用户，
    /// 换不换由用户点「检查卡片更新」决定，免得他正用着的部署脚本在背后被悄悄
    /// 换掉。只发一个请求（列表接口已带 script_sha256），不用为每张卡再查一次
    /// 详情。
    pub fn sync_store_catalog(&self) -> Result<StoreSyncResult, ModelManagerError> {
        let cards = self.store.list_cards()?;
        let local = db::list_store_models().unwrap_or_default();

        let store_ids: std::collections::HashSet<&str> =
            cards.iter().map(|c| c.id.as_str()).collect();

        // 已下载但商店里已经没有的卡片：本地仍可正常使用（脚本都固化在本地了），
        // 只是不能再重新下载，值得让用户知道。
        let delisted: Vec<String> = local
            .iter()
            .filter(|m| !store_ids.contains(m.card_id.as_str()))
            .map(|m| m.card_name.clone())
            .collect();

        // 已下载、且商店的脚本哈希跟本地固化副本对不上的 —— 有更新可用
        let mut outdated = Vec::new();
        for m in &local {
            if m.scripts_content.is_empty() {
                continue; // 老记录还没固化过副本，不参与比对
            }
            let Some(card) = cards.iter().find(|c| c.id == m.card_id) else {
                continue;
            };
            if card.script_sha256.is_empty() {
                continue; // 商店没给哈希（老后台），无从比较
            }
            let changed = card
                .script_sha256
                .iter()
                .any(|(t, h)| m.scripts_content.get(t).map(|b| &b.sha256) != Some(h));
            if changed {
                outdated.push(OutdatedCard {
                    card_id: m.card_id.clone(),
                    card_name: m.card_name.clone(),
                    // 运行中的模型即使刷新了脚本也不会自动生效——脚本只是文件，
                    // 已经起来的容器还是按旧参数在跑。提醒文案要说清楚这点。
                    needs_redeploy: m.status == "deployed" || m.running,
                    running: m.running,
                });
            }
        }

        Ok(StoreSyncResult {
            total: cards.len(),
            delisted,
            outdated,
        })
    }

    /// 用户主动点「检查卡片更新」：去商店比对脚本哈希，有变化就把新脚本固化
    /// 到本地。返回是否真的更新了。
    ///
    /// **目前没有任何调用点**：命令未注册，界面上也没有「检查卡片更新」这个
    /// 按钮和对应的 i18n 文案，留待后续分期接线。本文件别处（`entry_content`
    /// 的报错、`local_scripts_source` 的注释）已经在让用户"点一下检查卡片
    /// 更新"，那句话现在还无处可点——接线时要一起把入口补上。
    pub fn refresh_card_scripts(&self, card_id: &str) -> Result<bool, ModelManagerError> {
        let rec = self.get_record(card_id)?;
        let card = self.store.get_card(card_id)?;
        let pkg = self.fetch_and_store_scripts(&card)?;
        let changed =
            pkg.scripts.len() != rec.scripts_content.len()
                || pkg.assets.len() != rec.assets_content.len()
                || pkg.scripts.iter().any(|(k, v)| {
                    rec.scripts_content.get(k).map(|old| &old.sha256) != Some(&v.sha256)
                })
                || pkg.assets.iter().any(|(k, v)| {
                    rec.assets_content.get(k).map(|old| &old.sha256) != Some(&v.sha256)
                });
        if changed {
            db::set_scripts_content(card_id, &pkg.scripts, &pkg.assets)
                .map_err(|e| ModelManagerError::new(format!("保存脚本副本失败：{e}")))?;
        }
        Ok(changed)
    }

    fn get_record(&self, card_id: &str) -> Result<db::StoreModelRecord, ModelManagerError> {
        db::get_store_model(card_id)
            .map_err(|e| ModelManagerError::new(format!("读取模型记录失败：{e}")))?
            .ok_or_else(|| ModelManagerError::new("该模型尚未下载"))
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

        self.run_card_script(
            &source,
            "deploy",
            &env,
            &mut |line| on_progress(line),
            control,
            false,
        )?;
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
        let env = self.env_for_record(&rec);
        let source = self.local_scripts_source(&rec);

        let output_lines = self.run_card_script(
            &source,
            "enable",
            &env,
            &mut |line| on_progress(line),
            control,
            false,
        )?;
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
        if endpoint_url.is_none() && rec.engine_name == "ollama" {
            // 没有约定标记：走 Ollama 的默认约定（OpenAI 兼容接口，模型名就是
            // ollama create 时用的名字）。
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
        let mut env = self.env_for_record(&rec);
        env.insert(
            "LMM_PURGE_WEIGHTS".into(),
            if purge_weights { "1" } else { "0" }.into(),
        );
        let source = self.local_scripts_source(&rec);

        // 先量一次基线。**必须在跑脚本之前量**：脚本一跑完目录就没了，事后再
        // 想知道"刚才那儿有多少字节"已经无处可查，只剩下 card.json 里那个标称
        // 值可以拿来充数——而那正是要杜绝的事。
        let candidates = self.candidate_paths(&rec);
        let before = total_size(&candidates);

        self.run_card_script(
            &source,
            "uninstall",
            &env,
            &mut |line| on_progress(line),
            control,
            false,
        )?;
        // 记录留还是删，取决于用户按的是哪个按钮——这是两个动作**唯一**的语义
        // 差别，也是它们在界面上叫不同名字的原因：
        //
        //   仅卸载：只是把模型从引擎上撤下来。权重还在盘上，卡片就该还在库里，
        //           退回「已下载」，按钮变回「部署」。
        //   彻底删除：权重都没了，留着记录就是一条指向空气的死数据。
        //
        // 以前两条路径都走 delete_store_model，于是「仅卸载」会把卡片从库里抹掉，
        // 盘上几十 GB 权重从此没有任何界面管得到——用户要么手工 rm，要么重下一遍
        // 才能重新接管。那不是"保留"，是把东西弄丢在原地。
        if purge_weights {
            db::delete_store_model(card_id)
                .map_err(|e| ModelManagerError::new(format!("删除模型记录失败：{e}")))?;
        } else {
            db::mark_uninstalled(card_id)
                .map_err(|e| ModelManagerError::new(format!("更新模型状态失败：{e}")))?;
        }

        let after = total_size(&candidates);
        let freed_bytes = before.saturating_sub(after);
        let remaining_paths: Vec<String> = candidates
            .iter()
            .filter(|p| p.exists())
            .map(|p| p.to_string_lossy().to_string())
            .collect();

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
        let mut out: Vec<String> = Vec::new();
        for p in self.candidate_paths(rec) {
            let s = p.to_string_lossy().to_string();
            if p.exists() && !out.contains(&s) {
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

        let mut out: Vec<PathBuf> = Vec::new();
        for p in candidates {
            if !out.contains(&p) {
                out.push(p);
            }
        }
        out
    }

    // ---------------- 引擎管理 ----------------
    // 引擎（docker 镜像/ollama 等）可能被多张卡片共用，跟"模型"是两个独立的
    // 东西：卸载模型只删模型权重和部署配置，引擎本身默认保留。这里单独给用户
    // 一个入口去清理引擎本身占用的空间。
    //
    // **这一节（list_installed_engines / delete_engine）目前零调用点**：命令
    // 未注册，界面上也没有"已安装引擎"这一块，留待后续分期接线。注意
    // delete_engine 虽然用户够不着，它里面的超时/幂等问题仍然照修——接线那天
    // 才发现，代价是真机上删到一半的镜像和删不掉的引擎记录。

    pub fn list_installed_engines(&self) -> Vec<InstalledEngineInfo> {
        let Ok(records) = db::list_installed_engines() else {
            return Vec::new();
        };
        records
            .into_iter()
            .map(|rec| {
                let size = if let Some(image) = &rec.docker_image {
                    docker_image_size(image)
                } else if rec.engine_name == "ollama" {
                    ollama_size()
                } else {
                    0
                };
                let affected = db::list_model_names_using_engine(
                    &rec.engine_name,
                    rec.engine_version.as_deref(),
                )
                .unwrap_or_default();
                InstalledEngineInfo {
                    engine_name: rec.engine_name,
                    engine_version: rec.engine_version,
                    docker_image: rec.docker_image,
                    size_bytes: size,
                    affected_models: affected,
                }
            })
            .collect()
    }

    /// 只删引擎本身占用的空间，不动任何模型权重、部署配置。如果还有卡片在用
    /// 这个引擎，删完之后那些卡片下次下载/部署会自动重新走一遍 install_engine
    /// ——探针（LMM_PROBE_ONLY）会发现引擎不在了从而触发真正安装，这条探针
    /// 优先的安装流程本身就是自愈的。
    ///
    /// docker 镜像形态的引擎不需要 root（docker rmi 本来就是普通用户能跑的
    /// 命令）；ollama 是系统级二进制 + systemd 服务，删除必须走 pkexec 提权。
    pub fn delete_engine(
        &self,
        engine_name: &str,
        engine_version: Option<&str>,
    ) -> Result<u64, ModelManagerError> {
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

        if let Some(image) = &rec.docker_image {
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
            return Ok(freed);
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
            return Ok(freed);
        }

        Err(ModelManagerError::new(format!(
            "暂不支持自动清理该类型的引擎：{engine_name}"
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
        Ok(ModelDetail {
            record: rec,
            runtime_lines: gpu_runtime_lines(),
            runtime_note: "以下是设备 GPU 整体状态，不是精确到这一个模型的占用\
                           （同一时间通常只跑一个大模型，仅供参考）"
                .into(),
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

/// ollama 服务在不在。
///
/// 只问「服务活着吗」，不问「哪个模型加载着」——后者会随空闲卸载来回变，
/// 拿它纠正 running 会把正常的省显存行为报成故障（见 reconcile_running_status
/// 里那段注释）。
///
/// 超时跟同文件其余查询一致（5 秒）：拿不到就当**活着**，不是当死了。
/// 这个函数的唯一用途是把 running 置成 false，而置错的代价是用户明明启用着
/// 却被告知没启用；「问不出来」离「确定没在跑」还差得远，不该据此下结论。
fn ollama_alive(host: &str) -> bool {
    let url = format!("{}/api/tags", host.trim_end_matches('/'));
    let ok = reqwest::blocking::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()
        .and_then(|c| c.get(&url).send())
        .map(|r| r.status().is_success());
    match ok {
        Ok(alive) => alive,
        // 建不出客户端、连不上、超时：一律当活着，不动 running。
        Err(_) => true,
    }
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

fn ollama_size() -> u64 {
    let mut total = 0u64;
    for p in ["/usr/local/bin/ollama", "/usr/bin/ollama"] {
        if let Ok(meta) = std::fs::metadata(p) {
            total += meta.len();
        }
    }
    if let Ok(home) = std::env::var("HOME") {
        let ollama_dir = PathBuf::from(home).join(".ollama");
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
        .map(|(label, value)| format!("{label}：{value}"))
        .collect()
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

    fn make_card(name: &str, quant: Option<&str>) -> StoreCardDetail {
        StoreCardDetail {
            assets: Default::default(),
            asset_sha256: None,
            summary: StoreCardSummary {
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

    /// 只有打包在应用里的测试商店才允许把"URL"写成本地文件路径。真实商店的
    /// 卡片来自网络，放开这个开关就等于让卡片指定"读本机哪个文件再当脚本跑"。
    #[test]
    fn only_the_bundled_store_may_use_local_paths() {
        let card = make_card("x", None);
        let remote = ScriptsSource::from_card(&card, false);
        let bundled = ScriptsSource::from_card(&card, true);

        assert!(
            !remote.fetch_policy("install_engine").allow_local_path,
            "真实商店不能读本地路径"
        );
        assert!(
            remote.fetch_policy("install_engine").may_run_as_root,
            "install_engine 必须按提权那一档取（强制要求哈希）"
        );
        assert!(!remote.fetch_policy("deploy").may_run_as_root);
        assert!(bundled.fetch_policy("install_engine").allow_local_path);

        // Local 来源（已固化的本地副本）压根不 fetch，策略也必须是最严的一档，
        // 免得哪天有人把它接进 fetch 路径时意外放行。
        let local = ScriptsSource::Local {
            blobs: Default::default(),
            assets: Default::default(),
            profiles: Vec::new(),
        };
        assert!(!local.fetch_policy("install_engine").allow_local_path);
    }

    /// 引擎没装时探针以非 0 退出码结束，run_script 返回的是 Err。这个测试盯的
    /// 就是「那种情况下还能不能读到 LMM_NEED_ROOT」——如果实现改回用返回值判断
    /// 而不是用 on_progress 收集输出，这里就会挂。
    #[test]
    fn probe_reads_need_root_even_when_engine_is_missing() {
        let mgr = ModelManager::new_test_store();
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
        let mgr = ModelManager::new_test_store();
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

    /// 内置测试商店的每张卡都要实现 LMM_NEED_ROOT 约定，否则新加的卡片会悄悄
    /// 退回「先普通用户跑一次、失败再提权」的兜底路径，用户平白多等一轮。
    #[test]
    fn bundled_cards_implement_the_probe_convention() {
        // 每张卡的 install_engine 都必须实现只读探针：LMM 在每次下载前先跑一次，
        // 命中就整段跳过安装。没有它，用户每下一个模型都要重装一遍引擎。
        //
        // 但**不是每张卡都要声明 LMM_NEED_ROOT**：整包交付的那两张卡，包的
        // preflight 明确 `require(os.geteuid() != 0)`，整个脚本以 root 跑会被
        // 当场拒绝。它们需要提权的动作（注册 nvidia runtime、配直连网卡）由
        // 脚本自己在内部处理——所以这里只钉住探针约定，不强求 root 声明。
        for card in super::super::test_store_data::test_cards() {
            let script = super::super::test_store_data::script_content(&card, "install_engine")
                .expect("每张卡都该有 install_engine");
            assert!(
                script.contains("LMM_PROBE_ONLY"),
                "卡片 {} 的 install_engine 没有实现只读探针约定",
                card.summary.id
            );
        }
    }

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
            card_source: "builtin",
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

    // ---------------------------------------------- 卸载：保留权重 / 彻底删除

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

        let outcome = manager_with_models_dir(&models_dir)
            .uninstall("card-legacy", true, &mut |_| {}, Default::default())
            .expect("卸载应当成功");

        assert_eq!(outcome.freed_bytes, 0, "脚本什么都没删，就不能报释放量");
        assert!(
            outcome
                .remaining_paths
                .iter()
                .any(|p| p.ends_with("Ornith-1.5-35B-A3B")),
            "还在盘上的权重必须被点名，否则这几十 GB 就成了没人知道的孤儿：{:?}",
            outcome.remaining_paths
        );
        assert!(
            outcome.summary.contains("仍在磁盘上"),
            "用户点的是彻底删除却没删掉，这句话必须说出来：{}",
            outcome.summary
        );
    }
}
