//! 模型商店的配置管理：路径、设置持久化、HF Token 安全存储。
//!
//! # 数据放在哪，以及为什么不沿用 LMM 的老路径
//!
//! 这份代码搬自 LMM，那边用的是 `~/.config/local-model-manager/` 和
//! `~/.local/share/local-model-manager/`——因为 LMM 是一个独立应用。
//!
//! 现在模型商店是品悟的一个模块，数据跟着品悟走：`~/.pinvou3/model-store/`。
//! 卸载品悟时一目了然，不会在用户的 XDG 目录里留下一个他不认识的应用名。
//!
//! **刻意不去读 LMM 的老目录**（2026-09-18 定的「另起炉灶」）。理由不是怕冲突，
//! 而是怕**分不清**：老目录里有一份 `models.db` 和上百 GB 权重，自动接管的话，
//! 新实现一打开就是满的，谁也说不清界面上看到的是新代码的行为还是老数据的残影。
//! 设备上那份老数据原样留着，等新实现验证通过再统一处理。
//!
//! 权重目录（`models_dir`）单独可配：几十上百 GB 的东西，用户很可能想放到别的盘。

use std::fs;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};

use codewhale_secrets::{DefaultKeyringStore, KeyringStore as _};
use serde::{Deserialize, Serialize};

/// 密钥环里的服务名。
///
/// # 不要"顺手统一成 pinvou3- 前缀"
///
/// 这个取值是从 LMM 时期沿用下来的，**用户真机上已经按这个名字存着 token**。
/// 密钥环条目是 `(service, username)` 定位的，改掉 service 名不是重命名，是
/// **换了一条新条目**：老条目还躺在 Secret Service 里没人读，新条目是空的。
/// 用户看到的症状是私有模型权重突然下不动，而他从来没动过设置——最难查的那种。
///
/// 全仓其余凭证的 service 名都是 `pinvou3-*` 前缀（见 platform/credential_store.rs），
/// 这里是唯一的例外，是历史数据决定的，不是疏漏。真要统一得连迁移一起写：
/// 先按老名字读出来、按新名字写回去、确认成功再删老条目。
const KEYRING_SERVICE: &str = "local-model-manager";
const KEYRING_USERNAME: &str = "hf_token";

/// 模型商店在品悟数据目录下的落点。
///
/// 配置和数据放同一个根：模型商店的"配置"（商店地址、权重目录、并发数）本来就只有
/// 一个 json，为它单独占一个 XDG 配置目录没有意义，反而让"卸载时要清哪些"变成两处。
///
/// 根走 `platform::paths::pinvou3_home()`，不再自己解析 `$HOME`：那个函数认
/// `PINVOU3_HOME`（把整棵 `~/.pinvou3/` 树整体重定位）。自己解析等于设了这个变量时
/// 模型商店的库、权重、配置全部留在重定位树外面——本文件开头定下的"卸载时要清哪些
/// 只能有一处答案"又变回两处。
///
/// **没设 `PINVOU3_HOME` 时解析结果逐字节不变**，仍然是 `$HOME/.pinvou3/model-store`：
/// 两边都是 `PathBuf::from($HOME).join(".pinvou3")`（platform 那边见
/// `platform/os/linux/linux_path.rs` 的 `user_home_dir`）。这一点必须逐字节成立才敢
/// 换——设备上已经有权重、models.db、config.json 落在这个路径下，路径差一个字节就是
/// 用户数据全部失联（2026-09-20 改默认权重目录让 124GB 一夜失效，就是这么来的）。
///
/// 两处刻意的差别，都只在原来那条路走不到的情况下才出现：
///
/// 1. `$HOME` 缺失时的兜底，原来是 `/`，platform 层是 `/tmp`。`/` 对普通用户根本
///    不可写，那条路上不可能存在既有数据，统一过去没有迁移代价。
/// 2. 设了 `PINVOU3_HOME` 的机器上，数据根跟着变成 `$PINVOU3_HOME/model-store`
///    ——这正是本次要的语义（INSTALL.md 把它登记为"可重定位用户数据根目录"）。
///    代价要说清楚：**已经**把模型商店数据落在 `~/.pinvou3/model-store`、事后又设上
///    这个变量的机器，得手工把这个目录搬进新根，否则库和权重在界面上表现为"没有"。
///    没设过该变量的机器（出厂默认、开发机、CI）完全不受影响。
fn store_root() -> PathBuf {
    let d = crate::platform::paths::pinvou3_home().join("model-store");
    let _ = {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new()
            .mode(0o700)
            .recursive(true)
            .create(&d)
    };
    d
}

pub fn config_dir() -> PathBuf {
    store_root()
}

pub fn data_dir() -> PathBuf {
    store_root()
}

fn config_file() -> PathBuf {
    config_dir().join("config.json")
}

/// 把一个来自商店的标识（card_id、模型名）收成**单一、安全的路径分量**。
///
/// # 为什么需要
///
/// `card_id` 来自商店响应，`Path::join` 遇到绝对路径会**整体替换前缀**、遇到
/// `..` 会往上跳——于是 `cards/<card_id>` 可以落到 cards 目录之外的任何地方，
/// 而那正是资产落地和卸载时扫描残留的位置。
///
/// `script_runner::sanitize_relative_path` 早就为"卡片内容来自网络"这个威胁模型
/// 写了防线，但它只护住了**资产的相对路径**，没护住**存放它们的那个目录名**，
/// 同一个威胁模型下自相矛盾。
///
/// 卡片脚本本身就是任意代码、执行时本来就有同等能力，所以这不是提权边界；
/// 但既然防线已经建了，就不该留一个绕过它的口子。
///
/// 判据刻意收得很紧——只放行 `[A-Za-z0-9._-]`，且不允许 `.` / `..` 整体。
/// 真实的 card_id 都是 `qwen38fn-nvfp4-single` 这种形状，收紧没有代价。
pub fn safe_path_component(raw: &str) -> Result<String, String> {
    let s = raw.trim();
    if s.is_empty() {
        return Err("标识为空".to_string());
    }
    if s == "." || s == ".." {
        return Err(format!("标识不能是 {s}"));
    }
    if !s
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || c == '.' || c == '_' || c == '-')
    {
        return Err(format!("标识含有不能用于路径的字符：{s}"));
    }
    Ok(s.to_string())
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct Settings {
    pub staging_dir: String,
    pub models_dir: String,
    pub ollama_host: String,
    pub min_free_space_gb: f64,
    pub storage_quota_gb: Option<f64>,
    pub hf_endpoint: String,
    pub update_url: Option<String>,
    /// Independent of manual checks, telemetry and application updates.
    pub startup_update_check_enabled: bool,
    pub gateway_enabled: bool,
    pub gateway_port: u16,
    /// 同时下载几个模型。多个下载并行不会更快——它们抢的是同一条带宽，
    /// 真机上实测并行两个时单个速度掉到不足三分之一，总吞吐还更低。所以默认 1，
    /// 允许调到 3 给网络特别好的场景。取值范围由 max_concurrent_downloads() 兜底。
    pub max_concurrent_downloads: u8,
    /// 下载前去哪找「用户已经放在本机的权重」，除内置位置之外用户自己加的目录。
    ///
    /// 内置位置（当前权重目录、`~/models`、下载/桌面、以及所有挂着的 U 盘）由
    /// `import_scan::roots()` 给出，不需要写在这里；这一项是补充，比如用户把模型
    /// 统一放在 `/data/llm-weights` 这种我们猜不到的地方。
    ///
    /// 为什么必须可配：权重几十上百 GB，用户往往放在另一块盘上，而这些设备的
    /// 出口带宽只有几百 KB/s——找不到本地副本就意味着几十个小时的重下。
    #[serde(default)]
    pub import_scan_dirs: Vec<String>,

    /// 历史上用过的权重目录（`set_storage_settings` 换目录时把旧值记进来）。
    ///
    /// 为什么必须留痕：改权重目录**不搬运已下载的权重**（见
    /// `set_storage_settings` 的注释），老目录下的模型在商店库里仍然记着——
    /// 它们依旧是商店管理的权重，只是换目录那一刻起不再位于当前 `models_dir`。
    /// 设备管理的外部模型移除守卫按"商店权重根"拒绝删除（boundary.rs），不把
    /// 历史目录算进去的话，改过目录的机器上旧根就处于无保护状态。只留最近
    /// [`MAX_MODELS_DIR_HISTORY`] 个，足够覆盖正常使用（换盘不频繁），又不让
    /// 手改配置的脏数据无限堆积。
    ///
    /// 已知边界（第七轮评审 P3 披露）：留痕只发生在 `set_storage_settings`。
    /// 绕过设置页、直接手改本配置文件的 `models_dir` 不会触发留痕，旧根在
    /// 下次经设置页保存前不进保护集合——此时在册模型仍受名字臂保护，只是
    /// 路径臂不覆盖旧根。
    #[serde(default)]
    pub models_dir_history: Vec<String>,

    /// 历史上挂过、后来从设置里移除的补充扫描目录。
    ///
    /// 为什么必须留痕：下载脚本会在扫描目录里**就地复用**已存在的权重（复用
    /// 不搬家，商店在册权重可能就落在里面），而权重的实际落点由卡片脚本自己
    /// 决定、宿主无从枚举「哪些文件仍被已安装卡片引用」——保护只能取保守
    /// 超集。扫描目录一旦挂过，目录里的既有权重就可能被某张在册卡片以原路径
    /// 引用；把它从设置里移除的那一刻起，若不留痕，目录立即脱出商店权重根
    /// 集合（boundary.rs），设备管理「名称不同的外部模型记录 + 删除权重」
    /// 就能把仍在使用的唯一副本连目录删掉（2026-11-04 评审 P2：评审者以隔离
    /// 探针实证守卫在设置变更前拦截、变更后放行）。与 `models_dir_history`
    /// 同一先例：移除不等于脱管，曾经扫过的目录永久留在保护集合里。
    ///
    /// 已知边界（与上方 `models_dir_history` 同一条披露）：留痕只发生在
    /// `set_storage_settings`；手改配置文件移除扫描目录不触发留痕。另见
    /// 容量取舍：超出 [`MAX_IMPORT_SCAN_DIRS_HISTORY`] 的最老根被挤出后
    /// 脱出保护集合，在册模型只剩名字臂。
    #[serde(default)]
    pub import_scan_dirs_history: Vec<String>,
}

impl Default for Settings {
    fn default() -> Self {
        Self {
            staging_dir: String::new(),
            models_dir: String::new(),
            ollama_host: "http://127.0.0.1:11434".to_string(),
            min_free_space_gb: 2.0,
            storage_quota_gb: None,
            // 国内网络访问 huggingface.co 经常不通，默认用镜像站 hf-mirror.com
            hf_endpoint: "https://hf-mirror.com".to_string(),
            update_url: Some("https://pinvou.com/model-cube-update".to_string()),
            startup_update_check_enabled: true,
            gateway_enabled: false,
            gateway_port: 8090,
            max_concurrent_downloads: 1,
            import_scan_dirs: Vec::new(),
            import_scan_dirs_history: Vec::new(),
            models_dir_history: Vec::new(),
        }
    }
}

/// 模型商店的地址。**固定的，不可配**。
///
/// account-platform 反向隧道的公网入口：2026-09-10 起商店所在的那台服务器改成了
/// 只收隧道流量的私有源站，直连 IP 一律 403。
///
/// 为什么钉成常量而不是留一个设置项（2026-09-30）：「指向自定义商店地址」这个
/// 能力从来没有界面入口，只有手改 `config.json` 一条路，而它换来的是两件坏事
/// ——排障时得先确认用户到底连的是哪个后台；以及一张被改过的卡片配合一个被改过
/// 的地址就是一条「任意内容以 root 执行」的路（脚本经 pkexec 跑）。钉死之后
/// 「以生产为准」成为唯一口径。
///
/// 顺带把旧地址的迁移问题一次解决掉：以前 `load_settings` 要认出配置文件里那个
/// 已经 403 的历史默认值 `http://47.120.8.237/model-store` 并换成新地址，否则
/// 老用户的商店永远"连不上"。现在这个值根本不再被读取，**任何**存量取值（历史
/// 默认值、用户手填的、将来又换一次域名后遗留的）都自动失效，比逐个列进迁移
/// 名单更彻底。配置文件里残留的那个 `store_url` 键会在下一次保存设置时被丢掉。
pub const STORE_URL: &str = "https://account.pinvou.com/model-store";

pub const HF_ENDPOINT_PRESETS: &[&str] = &["https://huggingface.co", "https://hf-mirror.com"];

/// `models_dir_history` 的保留上限。换权重目录不是高频操作，16 个足够覆盖
/// "换盘—换回—再换"的正常弧线；超出后丢最老的。
pub const MAX_MODELS_DIR_HISTORY: usize = 16;

/// `import_scan_dirs_history` 的保留上限。与 [`MAX_MODELS_DIR_HISTORY`] 同一
/// 量级：增删扫描目录同样是低频设置操作，16 个足够覆盖正常弧线；超出后丢
/// 最老的（被挤出的根脱出权重保护集合，属既定容量取舍，见字段注释）。
pub const MAX_IMPORT_SCAN_DIRS_HISTORY: usize = 16;

impl Settings {
    /// 夹到 1..=3。配置文件可能被手改成 0（那样谁都下不了）或者 99，不能信。
    pub fn max_concurrent_downloads(&self) -> usize {
        (self.max_concurrent_downloads as usize).clamp(1, 3)
    }

    pub fn staging_path(&self) -> PathBuf {
        let p = PathBuf::from(&self.staging_dir);
        let _ = {
            use std::os::unix::fs::DirBuilderExt;
            std::fs::DirBuilder::new()
                .mode(0o700)
                .recursive(true)
                .create(&p)
        };
        p
    }

    pub fn models_path(&self) -> PathBuf {
        let p = PathBuf::from(&self.models_dir);
        let _ = {
            use std::os::unix::fs::DirBuilderExt;
            std::fs::DirBuilder::new()
                .mode(0o700)
                .recursive(true)
                .create(&p)
        };
        p
    }
}

/// 隔离留存的落点：`config.json` → `config.json.bad`。读取端据此区分"全新安装"
/// 和"上一拍刚隔离改名"——两者在原文件上都表现为 NotFound。
fn quarantine_bad_path(cfg_file: &Path) -> PathBuf {
    cfg_file.with_extension("json.bad")
}

/// 解析不了的配置文件改名留存成 `config.json.bad`，再回退默认值。
///
/// **不能静默**，理由跟 `write_token_fallback` 那条一样，后果还更重：
/// `load_settings` 回退到全默认值之后，界面上看不出任何异常，而下一次**任意**保存
/// ——哪怕只是在「存储与导入」里动一下权重目录（service.rs 里每个 `set_*` 都是
/// "先读再改再整体写回"）——就会把这堆默认值原样写回盘上。用户自配的权重目录、
/// 导入扫描目录到这一刻才真正**不可恢复**。而 `Settings` 是整体反序列化的，任一字段类型不符
/// （手改坏、旧版本遗留、磁盘损坏）就足以让整个文件作废，门槛并不高。
///
/// 所以坏文件先改名留在原地：至少还能手工把值抄回来，排查的人在日志里也看得见。
/// 只留最近一份（`.bad` 已存在时 rename 直接覆盖）——这种文件不该攒成一堆，
/// 真要看的也是刚坏掉的那一份。
///
/// 留存文件同时也是 [`try_load_settings`] 的隔离标记：原文件改名后，后续读取在
/// 文件系统里看到的就是"缺失"，破坏性消费方必须凭这份残留继续 fail-closed，
/// 而不是把保护范围未知的半拍当成全新安装（十六轮评审 P1）。写端同一口径：
/// [`update_settings`] 以严格读取为闸门，读不过去的态（缺失+留存、在但不可读、
/// 解析坏）一律拒绝普通保存，防止宽松读取的默认值被固化成合法配置、把读端的
/// fail-closed 洗掉（二十六/二十七轮评审 P1）。
fn quarantine_broken_config(cfg_file: &Path, reason: &str) {
    let bad = quarantine_bad_path(cfg_file);
    match fs::rename(cfg_file, &bad) {
        Ok(()) => eprintln!(
            "[model-store] 配置文件{reason}，已改名留存为 {}，本次回退默认设置",
            bad.display()
        ),
        Err(e) => eprintln!(
            "[model-store] 配置文件 {} {reason}，改名留存也失败（{e}），本次回退默认设置",
            cfg_file.display()
        ),
    }
}

/// All configuration mutation, including quarantine, shares one private file lock.
/// Hold it only for local reads/patches/atomic writes, never for network work.
fn settings_lock() -> std::io::Result<fs::File> {
    let lock =
        crate::platform::filesystem::open_private_lock_file(&config_dir().join("config.lock"))?;
    lock.lock()?;
    Ok(lock)
}

fn missing_settings_default() -> Settings {
    let mut settings = Settings::default();
    // A quarantined configuration is evidence of an interrupted recovery, not
    // a fresh install. Unknown prior consent cannot authorize automatic traffic.
    settings.startup_update_check_enabled = matches!(
        fs::symlink_metadata(config_file().with_extension("json.bad")),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound
    );
    settings
}

fn normalize_settings(mut settings: Settings) -> Settings {
    if settings.staging_dir.trim().is_empty() {
        settings.staging_dir = data_dir().join("staging").to_string_lossy().to_string();
    }
    if settings.models_dir.trim().is_empty() {
        settings.models_dir = data_dir().join("models").to_string_lossy().to_string();
    }
    settings
}

/// Strict fresh read while the caller holds settings_lock. Writers must not
/// turn read/parse failures into a replacement configuration.
fn read_settings_snapshot_unlocked() -> std::io::Result<(Settings, bool)> {
    let (settings, missing) = match read_config_text(&config_file()) {
        Ok(text) => (
            serde_json::from_str::<Settings>(&text)
                .map_err(|error| std::io::Error::new(std::io::ErrorKind::InvalidData, error))?,
            false,
        ),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            (missing_settings_default(), true)
        }
        Err(error) => return Err(error),
    };
    Ok((normalize_settings(settings), missing))
}

fn read_settings_unlocked() -> std::io::Result<Settings> {
    read_settings_snapshot_unlocked().map(|(settings, _)| settings)
}

pub fn load_settings() -> Settings {
    let result = (|| {
        let _lock = settings_lock()?;
        match read_settings_unlocked() {
            Ok(settings) => Ok(settings),
            Err(error) => {
                if error.kind() == std::io::ErrorKind::InvalidData {
                    // This rename is inside the same lock as all writers. A
                    // stale reader can never quarantine a newly saved file.
                    quarantine_broken_config(&config_file(), "解析失败");
                }
                Err(error)
            }
        }
    })();
    result.unwrap_or_else(|error: std::io::Error| {
        eprintln!("[model-store] 配置读取失败（{error}），本次回退默认设置并关闭自动检查");
        let mut settings = Settings::default();
        settings.startup_update_check_enabled = false;
        normalize_settings(settings)
    })
}

/// Automatic access fails closed on errors or quarantined configuration. A
/// valid old configuration without the field keeps its original enabled default.
pub fn startup_update_check_enabled() -> Result<bool, String> {
    read_settings().map(|settings| settings.startup_update_check_enabled)
}

/// A coherent settings snapshot for controls that must not save fallback data
/// after a failed read. Missing quarantined files carry a disabled policy.
pub fn read_settings() -> Result<Settings, String> {
    let _lock = settings_lock().map_err(|error| format!("读取模型商店设置失败：{error}"))?;
    read_settings_unlocked().map_err(|error| format!("读取模型商店设置失败：{error}"))
}

/// 配置文本的唯一读取口。生产上就是 [`fs::read_to_string`]。
///
/// 抽出来只有一个理由：测试要能注入**瞬态**读取失败（见下方
/// `arm_transient_read_failure`）。权限位（mode 000）构造不出「同一拍里首读
/// 失败、保存端重读已恢复」的时序——权限失败到重读时仍在；而 fd 耗尽（EMFILE）、
/// 一拍 EIO 这类真实瞬态故障恰恰是"过一下自己就好了"。二十八轮评审 P1 的回归
/// 需要能受控地打出这个时序。
fn read_config_text(cfg_file: &Path) -> std::io::Result<String> {
    #[cfg(test)]
    {
        let left = TEST_TRANSIENT_READ_FAILURES.load(std::sync::atomic::Ordering::Relaxed);
        if left > 0 {
            TEST_TRANSIENT_READ_FAILURES.store(left - 1, std::sync::atomic::Ordering::Relaxed);
            // 错误类别只要不是 NotFound 就会走 `try_load_settings` 的"读取失败"
            // 分支（报错、不隔离）——正是瞬态 I/O 故障的形态。
            return Err(std::io::Error::other("注入的瞬态读取失败（测试）"));
        }
    }
    fs::read_to_string(cfg_file)
}

/// 注入接下来 `times` 次配置读取的瞬态失败。仅测试存在（`#[cfg(test)]`），
/// 由 `TempHome` 的全局测试锁串行化，计数器无并发竞争。
#[cfg(test)]
pub fn arm_transient_read_failure(times: usize) {
    TEST_TRANSIENT_READ_FAILURES.store(times, std::sync::atomic::Ordering::Relaxed);
}

#[cfg(test)]
static TEST_TRANSIENT_READ_FAILURES: std::sync::atomic::AtomicUsize =
    std::sync::atomic::AtomicUsize::new(0);

/// Destructive device-boundary queries and every production settings patch
/// require known settings roots. Missing config plus a quarantine marker is not
/// a fresh install, even when the patch explicitly enables automatic checks.
/// A valid restored config takes precedence over the retained backup. Removing
/// the backup is an explicit manual abandonment, never a settings-save effect.
/// The unlocked helper's caller already owns the single configuration lock.
fn try_load_settings_unlocked() -> Result<Settings, String> {
    let cfg_file = config_file();
    match read_settings_snapshot_unlocked() {
        Ok((settings, missing)) => {
            if missing {
                let bad = quarantine_bad_path(&cfg_file);
                match fs::symlink_metadata(&bad) {
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => (),
                    _ => {
                        return Err(format!(
                            "配置文件缺失，但此前读取失败的隔离留存 {} 仍在或无法确认：自定义设置（如权重目录）的保护范围未知。请恢复配置文件，或确认放弃后删除该留存文件",
                            bad.display()
                        ));
                    }
                }
            }
            Ok(settings)
        }
        Err(error) => {
            if error.kind() == std::io::ErrorKind::InvalidData {
                quarantine_broken_config(&cfg_file, "解析失败");
            }
            Err(format!("配置文件读取失败：{error}"))
        }
    }
}

pub fn try_load_settings() -> Result<Settings, String> {
    let _lock = settings_lock().map_err(|error| format!("读取模型商店设置失败：{error}"))?;
    try_load_settings_unlocked()
}

/// One domain transaction for all production settings patches. Waiting writers
/// reread after acquiring the cross-process lock; None fields retain that value.
/// Failed reads never run the patch or publish defaults. A parse failure may
/// quarantine the original bytes under this same lock, preserving main's
/// recovery contract without a second settings read before writing.
pub fn update_settings(
    patch: impl FnOnce(&mut Settings) -> Result<(), String>,
) -> Result<Settings, String> {
    let _lock = settings_lock().map_err(|error| format!("锁定模型商店设置失败：{error}"))?;
    let mut settings = try_load_settings_unlocked()
        .map_err(|error| format!("读取当前配置失败，未做任何修改：{error}"))?;
    patch(&mut settings)?;
    write_settings_unlocked(&settings).map_err(|error| format!("保存设置失败：{error}"))?;
    Ok(settings)
}

/// 写配置。**先写临时文件再 rename**，并且一开始就以 0600 创建。
///
/// 两件事都不是讲究：
///
/// 1. 直接 `fs::write` 覆盖不是原子的。保存那一瞬断电或被杀，留下的是半截 JSON，
///    而 `load_settings` 解析失败就回退到全部默认值——用户自己配的权重目录、
///    导入扫描目录、HF 镜像地址一起消失，界面上没有任何异常，他只会觉得
///    "设置怎么自己变回去了"。坏文件现在至少会被 `quarantine_broken_config` 留成
///    `config.json.bad` 并打一行日志，但那只是事后能抄回来，**不是**可以不做原子
///    写的理由。同模块 orphans.rs 的 `store_unlocked` 早就是这么写的，照搬即可。
/// 2. 先写后 `set_permissions` 之间有一个 0644 的窗口，而这个文件里有权重目录、
///    导入扫描目录这类不该给同机其他用户看的东西。用 `OpenOptions::mode` 直接按
///    0600 建，窗口就不存在了。
///
/// 写端闸门与读端 fail-closed 同口径（二十六/二十七轮评审 P1）：动笔前先按
/// [`try_load_settings`] 严格读一遍，读不过去的状态一律拒绝普通保存。普通
/// setter（切测试商店开关等）先经宽松 [`load_settings`] 拿到默认值再整体写回，
/// 三种"保护范围未知"的态若放行，都会把默认值固化成合法 `config.json`，此后
/// 严格读取走"合法 JSON"分支接受它，未知的自定义 `models_dir` 等保护整个失效
/// （严格读端的 fail-closed 被写端洗掉）：
///
/// * 配置缺失而隔离留存还在——这正是 [`try_load_settings`] 持续 `Err` 的态，
///   一次与删除无关的开关切换就会让留存里未知的保护范围整个失效；
/// * 配置在但读不出来（如 mode 000、磁盘错误）——严格读取报错且**不**隔离，
///   留存不存在；但父目录可写就能建 0600 临时文件再 rename 覆盖原文件
///   （rename(2) 不要求读被替换的文件），下一拍严格读取接受合法默认 JSON，
///   原配置里未知的保护范围就此消失（二十七轮评审 P1 补的正是这一分支）；
/// * 配置在但解析坏——宽松读取那一拍已先隔离改名，到这里表现为缺失+留存。
///
/// 拒绝的报错带上严格读取的原始原因，与读端同口径给出出路：恢复有效配置，或
/// 确认放弃后按提示清理残留。配置可读可解析就放行——包括用户已显式恢复、
/// 历史留存还在的态，不能让留存永久阻断已恢复的有效配置；全新安装（缺失且
/// 无留存）同样放行。
///
/// 闸门有一处自己罩不住的盲区（二十八轮评审 P1 的剩余分支）：它重读成功时
/// **只判 Err/Ok，读到的真实 `Settings` 被丢弃**，落盘的仍是调用方传进来的
/// 对象。若调用方的首读走宽松 [`load_settings`] 赶上瞬态失败拿到默认值、到
/// 这一拍瞬态已恢复，闸门放行的就是那份默认快照——真实 config.json 被洗成
/// 默认。所以这个盲区不在写端修（那需要把两次读取缝成一次事务），而是在
/// 上游入口把首读改成 [`try_load_settings`]：读不到就不改、不保存（service.rs
/// 的两个普通设置入口，回归见 `transient_first_read_failure_must_not_persist_the_default_snapshot`）。
pub fn save_settings(settings: &Settings) -> std::io::Result<()> {
    let _lock = settings_lock()?;
    try_load_settings_unlocked().map_err(|reason| std::io::Error::other(format!(
        "普通保存被拒绝：当前配置严格读取失败（{reason}）——请先恢复有效配置，或确认放弃后清理隔离留存再重试",
    )))?;
    write_settings_unlocked(settings)
}

fn write_settings_unlocked(settings: &Settings) -> std::io::Result<()> {
    use std::io::Write as _;

    let cfg_file = config_file();
    let json = serde_json::to_string_pretty(settings)?;

    let tmp = cfg_file.with_extension("json.tmp");
    {
        let mut f = fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(&tmp)?;
        f.write_all(json.as_bytes())?;
        // 不 sync 就 rename 的话，掉电后可能 rename 生效而内容还在页缓存里没落盘，
        // 结果是"文件在、内容是空的"——比半截 JSON 更难查。
        f.sync_all()?;
    }
    fs::rename(&tmp, &cfg_file)
}

// ---- HF Token：优先使用系统密钥环（GNOME Keyring / Secret Service），
// ---- 不可用时退化为受限权限的本地文件。 ----
//
// # 走底座 codewhale-secrets，但不走 `platform/credential_store.rs`
//
// 密钥环访问经 `codewhale_secrets::DefaultKeyringStore`，**不直接依赖 `keyring`**
// ——这跟 Cargo.toml 里「凭证走底座 codewhale-secrets」那条决策是一致的，
// app 层不再有第二处 keyring 直接依赖。
//
// `DefaultKeyringStore::{get,set,delete}` 底下就是 `keyring::Entry::new(service, key)`
// 加 `get_password` / `set_password` / `delete_credential`，跟这里原来手写的两行
// 逐字等价，所以真机上已经存着的那条 `(local-model-manager, hf_token)` 读写行为不变。
// 一处细微差别：条目不存在时，原来的 `delete_credential()` 返回 `Err(NoEntry)`，
// 底座把它归一成 `Ok(())`（"目标状态已达成"算成功）。净效果相同——两条路最后都会
// 去清一次回退文件，只是少绕一圈。
//
// 那为什么不干脆用 `SystemCredentialStore` + `CredentialReference::for_*`
// （全仓其余十余种凭证都走那条）？三条理由，都不是偷懒：
//
// 1. **service 名换不得。** 条目是 LMM 时期沿用的 `(local-model-manager, hf_token)`，
//    真机上已经存着。`SystemCredentialStore` 按 `(reference.service, reference.account)`
//    定位，要读到老值就得把 service 定成 `local-model-manager`——而那张注册表里
//    十余个作用域清一色 `pinvou3-*` 前缀，塞个外来名字进去比分开放更难解释；
//    改用 `pinvou3-hf-token` 之类的新名字则让已存 token 一夜失踪。详见
//    `KEYRING_SERVICE` 的说明。
//
// 2. **回退落盘位置不一样，而且这里的位置是刻意选的。** `SystemCredentialStore`
//    在 keyring probe 失败时回退到 `Secrets::file_backed()`，也就是底座共享的
//    `~/.codewhale/secrets/secrets.json`。模型商店的回退文件在
//    `~/.pinvou3/model-store/hf_token`，跟它的库、配置、权重同根——「卸载品悟时
//    要清哪些」这件事在本文件开头就定过只能有一处答案。换过去意味着已经落在
//    老位置的 token 读不出来，还得再写一段迁移，为了分层反而多欠一笔债。
//
// 3. **没有测试接缝。** `credential_store` 给自己留的假后端注入口
//    （`inject_fake_secrets`）是它 `mod tests` 里的私有函数，外部模块用不到。
//    模型商店这边一旦改走过去，凡是碰 token 的测试都会打到开发机/CI 上真实的
//    Secret Service——这正是本模块刚花力气用 `TempHome` 消灭的那类污染。
//
// # musl 构建下会降级到文件回退
//
// 底座给 `keyring` 的 Linux 依赖声明带着
// `cfg(all(target_os = "linux", not(target_env = "ohos"), not(target_env = "musl")))`，
// `DefaultKeyringStore` 的实现也按同一组 cfg 分支——**musl 目标下 get/set/delete
// 一律返回 Err(unsupported)**，于是这里每次都会落到 0600 回退文件那条路。
// 当前出包是 glibc aarch64，不受影响；将来真要出 musl 包，得先想清楚 token
// 只能落明文文件这件事可不可接受（至少是 0600，但不再有密钥环加持）。
// 原先直接依赖 `keyring` 时 musl 下也能编出 secret-service 后端，这是这次改动
// 唯一一处行为面的收窄，如实记在这里。

fn token_fallback_file() -> PathBuf {
    config_dir().join("hf_token")
}

/// 密钥环后端。每次现构造——它只存一个 service 名字符串，没有连接或句柄，
/// 构造是纯内存操作，没必要缓存。
fn keyring_store() -> DefaultKeyringStore {
    DefaultKeyringStore::new(KEYRING_SERVICE)
}

/// 读回退文件里的 token。空文件和只有空白的文件都按「没有」处理。
fn read_token_fallback() -> Option<String> {
    fs::read_to_string(token_fallback_file())
        .ok()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

/// 把 token 写进回退文件。
///
/// 用 `OpenOptions::mode(0o600)` 直接按 0600 创建，而不是「先 write 再 chmod」。
/// 后者按 umask 先落成 0644，中间有一个同机其他用户读得到的窗口，而这个文件里
/// 装的是 HF token。同文件的 `save_settings` 已经是这么写的——那里存的只是商店
/// 地址，token 比它敏感得多，没有理由反而更松。
fn write_token_fallback(token: &str) {
    use std::io::Write as _;

    let written = fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(token_fallback_file())
        .and_then(|mut file| file.write_all(token.as_bytes()));
    if let Err(e) = written {
        // 不能静默：写不进去意味着下次下载私有权重会以"没有 token"失败，
        // 而用户明明刚填过。至少让排查的人在日志里看见。
        eprintln!("[model-store] HF token 回退文件写入失败：{e}");
    }
}

/// 删掉回退文件。不存在就什么也不做。
fn clear_token_fallback() {
    let _ = fs::remove_file(token_fallback_file());
}

pub fn get_hf_token() -> Option<String> {
    // 条目不存在时底座返回 Ok(None)，够不着密钥环才返回 Err——两种情况都落回退文件。
    if let Ok(Some(token)) = keyring_store().get(KEYRING_USERNAME) {
        if !token.is_empty() {
            return Some(token);
        }
    }
    read_token_fallback()
}

/// 写入（`Some`）或清除（`None`）HF token。这是 token 的**唯一写入口**。
///
/// **目前零调用点**：没有对应的命令、前端入口和三语文案，所以全新用户的 token
/// 恒为空——需要 HF 凭据的 gated / 私有模型下不了（在架的公开模型不受影响：
/// `get_hf_token` 读不到时 lifecycle 注入的 `LMM_HF_TOKEN` 是空串，脚本按匿名
/// 下载走）。留待后续分期接线，那是「命令 + 设置里的输入框 + i18n 三语」一整套，
/// 不在本期范围内。
///
/// 读那一侧（`get_hf_token`）是接着的（lifecycle 给每次脚本执行注入
/// `LMM_HF_TOKEN`），所以真机上早年由 LMM 存进密钥环的那条 token 仍然在用——
/// 只是用户现在没有办法在品悟里改它或删它。
pub fn set_hf_token(token: Option<&str>) {
    let store = keyring_store();
    let stored_in_keyring = match token {
        Some(t) => store.set(KEYRING_USERNAME, t).is_ok(),
        // 条目本来就不存在时底座也返回 Ok(())——"要它没了"这个目标已经达成，
        // 算成功。原先手写 keyring 时这里是 Err(NoEntry)，会多绕一圈落到下面
        // 那个 match 的 None 分支，最后同样是清回退文件，净效果一致。
        None => store.delete(KEYRING_USERNAME).is_ok(),
    };
    if stored_in_keyring {
        // 密钥环这条路走通了，就顺手把回退文件清掉——**这一步不能省**。
        //
        // 回退是有状态的：用户某次设 token 时密钥环恰好不可用（刚装完系统还没
        // 解锁 keyring、跑在没有 D-Bus 的会话里），token 就以明文落在
        // `~/.pinvou3/model-store/hf_token`。之后密钥环恢复正常，他再改一次或者
        // 删掉 token，走的是上面这条成功分支，那份**旧明文一直留在盘上**；而
        // `get_hf_token` 优先读密钥环，界面上看什么都对，用户完全无从察觉自己
        // 主目录里还躺着一个早就作废的 HF token。
        //
        // 「密钥环里有一份」和「明文文件里有一份」不能同时成立：密钥环是唯一
        // 真相源，回退文件只在密钥环够不着的时候临时顶一下。
        clear_token_fallback();
        return;
    }
    match token {
        Some(t) => write_token_fallback(t),
        None => clear_token_fallback(),
    }
}

/// 测试专用的全局锁。
///
/// 多个模块的测试都靠改 HOME 把自己切到独立的临时
/// 目录上，但环境变量是**整个进程**共享的，而 Rust 测试默认同进程多线程并行。
/// 只在自己模块内加锁是不够的——db 的测试会把 orphans 测试的数据目录切走，
/// 反过来也一样（症状是莫名其妙的 "no such table"、刚写进去的记录凭空消失）。
/// 所以这把锁必须是跨模块唯一的一把。
#[cfg(test)]
pub static ENV_TEST_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// 把 `HOME` 临时切到一个空目录，**离开作用域时一定还原**。
///
/// # 为什么必须还原
///
/// 这里原来是裸 `set_var("HOME", tmp.path())`，从不还原；测试结束 tempdir 析构
/// 被删，`HOME` 就悬空指向一个已经不存在的路径。`cargo test --lib` 是**单进程
/// 多线程**，于是污染的不只是自己：CI 上 `remote_control::file_access`（本模块
/// 一行都没碰过）按 HOME 解析默认目录时读到那个已删路径，直接失败——
/// 2420 passed / 1 failed，而失败信息里的 tempdir 路径只有模型商店的新测试会创建。
///
/// `ENV_TEST_LOCK` 保护不了它：那把锁只在模型商店自己的测试之间串行化，
/// 读 `HOME` 的既有测试根本不知道有这把锁。所以**还原**才是真正的修复，
/// 加锁只是让并发改动不至于互相撕裂。
///
/// 锁一并握在这个守卫里：这样「改 HOME」和「还原 HOME」之间全程持锁，
/// 不会出现某个测试改完、还没还原就被另一个测试读到的窗口。
///
/// # 为什么连 `PINVOU3_HOME` 一起清
///
/// `store_root()` 现在走 `platform::paths::pinvou3_home()`，而它**先看
/// `PINVOU3_HOME`**：开发机上真设了这个变量的话，只切 HOME 是隔离不住的——
/// 测试会直接读写那棵被重定位的**真实**数据树（models.db、权重、config.json）。
/// 所以这里把它一并移除，离开作用域再还原成原值。
#[cfg(test)]
pub struct TempHome {
    dir: tempfile::TempDir,
    previous: Option<std::ffi::OsString>,
    previous_pinvou3_home: Option<std::ffi::OsString>,
    // 声明在最后 => 最后析构：HOME 已经还原、临时目录也删掉之后才放锁。
    _lock: std::sync::MutexGuard<'static, ()>,
}

#[cfg(test)]
impl TempHome {
    pub fn new() -> Self {
        let lock = ENV_TEST_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let dir = tempfile::tempdir().unwrap();
        let previous = std::env::var_os("HOME");
        let previous_pinvou3_home = std::env::var_os("PINVOU3_HOME");
        // SAFETY: edition 2024 起 set_var 是 unsafe——并发改环境变量是 UB。
        // 这里全程持有 ENV_TEST_LOCK，同一时刻只有一个测试在动这两个变量。
        unsafe {
            std::env::set_var("HOME", dir.path());
            // 移除而不是设成临时目录：设了的话解析结果就变成
            // `<tmp>/model-store`，跟生产路径 `$HOME/.pinvou3/model-store` 差一层，
            // 测试也就不再覆盖真实布局了。
            std::env::remove_var("PINVOU3_HOME");
        }
        Self {
            dir,
            previous,
            previous_pinvou3_home,
            _lock: lock,
        }
    }

    pub fn path(&self) -> &std::path::Path {
        self.dir.path()
    }
}

#[cfg(test)]
impl Drop for TempHome {
    fn drop(&mut self) {
        // SAFETY: 同上，仍在锁内。
        unsafe {
            match &self.previous {
                Some(v) => std::env::set_var("HOME", v),
                // 原来就没有 HOME 的环境（CI 容器里有过）：移除而不是设成空串，
                // 空串会让 `$HOME/...` 拼成绝对路径 `/...`，比没有更糟。
                None => std::env::remove_var("HOME"),
            }
            match &self.previous_pinvou3_home {
                Some(v) => std::env::set_var("PINVOU3_HOME", v),
                // 原来就没有的，离开时也得是没有：不能只"什么都不做"——作用域内
                // 万一有别的代码设上了它，就会漏到后面的测试里，而那正是 TempHome
                // 这个类型存在的理由。
                None => std::env::remove_var("PINVOU3_HOME"),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 商店地址是钉死的常量，配置文件里写什么都不生效。
    ///
    /// 这条盯的是 2026-09-30 那次改动的**全部价值**：只要还有任何一条路能让磁盘上
    /// 的一个值决定商店连去哪，「以生产为准」就不成立，而那条路上跑的是经 pkexec
    /// 以 root 执行的卡片脚本。用历史默认值（已 403）和一个自定义地址各试一遍——
    /// 前者代表存量配置，后者代表有人手改过。
    ///
    /// 顺带也是"旧地址迁移"那件事的归宿：以前靠 `LEGACY_DEFAULT_STORE_URLS` 逐个
    /// 认；现在整个字段不再被读，存量取值自动全部失效。
    #[test]
    fn the_store_url_is_a_constant_that_no_config_file_can_override() {
        // 把整个数据目录切到临时目录：store_root() 是 $HOME/.pinvou3/model-store，
        // 只认 HOME 和 PINVOU3_HOME（TempHome 两个都管了）。
        // 这里**不能**改成设 XDG_*——那是 LMM 独立应用时期的路径，
        // 原生化之后没有任何生产代码再读它，设了等于没隔离，测试会直接读写开发机
        // 上真实的 ~/.pinvou3/model-store。
        //
        // 必须用 TempHome 而不是裸 set_var：后者结束时不还原，tempdir 一析构
        // HOME 就悬空指向一个已删路径，同进程后续跑的**别的模块**的测试会按这个
        // HOME 解析默认目录然后失败（CI 上真出过，见 TempHome 的说明）。
        // ENV_TEST_LOCK 只防并发互撕，防不了这种污染——TempHome 把锁和还原一起管了。
        let _home = TempHome::new();
        let path = config_file();
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();

        // 192.0.2.x 是 RFC5737 的文档保留段（TEST-NET-1），跟任何真实环境无关。
        for leftover in [
            r#"{"store_url": "http://47.120.8.237/model-store/"}"#,
            r#"{"store_url": "http://192.0.2.10:8088"}"#,
            r#"{"store_url": "https://192.0.2.10/model-store"}"#,
        ] {
            std::fs::write(&path, leftover).unwrap();
            // r21 P2-11：断言必须在 load_settings **之后**——隔离行为要锁定的
            // 正是「读取看到不认识的键时不作废整份配置」：既不把文件改名
            // quarantine（坏文件才走那条路，文件会被移走、此处 exists 即假），
            // 读取也照常完成。断言放在读取之前只证明了「我们刚写过」，
            // 什么行为都没钉住。
            let _settings = load_settings();
            assert!(
                path.exists(),
                "多一个不认识的键不该让整份配置作废：{leftover}"
            );
        }

        assert_eq!(
            STORE_URL, "https://account.pinvou.com/model-store",
            "生产商店地址被改动了。改它意味着所有设备下一次升级后连去另一个后台，\
             不能顺手改"
        );
        assert!(
            STORE_URL.starts_with("https://"),
            "商店地址必须是 https：卡片 JSON 就是脚本哈希的载体，明文链路上校验挡不住伪造"
        );
    }

    /// 数据根必须逐字节等于 `$HOME/.pinvou3/model-store`。
    ///
    /// 换走 `platform::paths::pinvou3_home()` 那次改动，安全性全压在这一条上：
    /// 设备上的权重、models.db、config.json 都躺在这个路径下，差一个分量就是用户
    /// 数据全部失联。TempHome 保证 `PINVOU3_HOME` 是没设的，也就是出厂状态。
    #[test]
    fn store_root_is_byte_for_byte_the_old_path_when_pinvou3_home_is_unset() {
        let home = TempHome::new();
        let expected = home.path().join(".pinvou3").join("model-store");
        assert_eq!(config_dir(), expected);
        assert_eq!(data_dir(), expected);
        assert_eq!(config_file(), expected.join("config.json"));
    }

    /// 坏掉的配置文件不能无声重置：回退默认值之前要先把它改名留存。
    ///
    /// 不留存的话，下一次任意保存（在「存储与导入」里动一下权重目录就够）会把重置
    /// 后的默认值整体写回盘上，用户自配的权重目录/扫描目录从此不可恢复。
    #[test]
    fn a_broken_config_is_kept_aside_instead_of_being_silently_reset() {
        let _home = TempHome::new();
        let path = config_file();

        // 合法 JSON，但 gateway_port 的类型不对——整体反序列化时这一个字段
        // 就足以让整个文件作废，正是"门槛并不高"的那种坏法。
        std::fs::write(
            &path,
            r#"{"models_dir": "/mnt/192.0.2.10", "gateway_port": "不是数字"}"#,
        )
        .unwrap();

        assert_eq!(
            load_settings().gateway_port,
            Settings::default().gateway_port,
            "坏文件应当回退到默认值"
        );
        assert!(
            !path.exists(),
            "坏文件必须让开，否则下一次保存会把它原地覆盖掉"
        );

        let bad = path.with_extension("json.bad");
        let kept = std::fs::read_to_string(&bad).expect("坏文件没有留存");
        assert!(
            kept.contains("192.0.2.10"),
            "留存的必须是原样内容，用户还指着它把值抄回来：{kept}"
        );
    }

    /// 隔离态下普通保存必须被拒绝（二十六轮评审 P1）：普通 setter 走宽松读取
    /// 拿到默认值再整体写回——放行的话，一次与删除无关的开关切换就把默认值
    /// 固化成合法 `config.json`，此后严格读取走"合法 JSON"分支接受它，隔离留存
    /// 里未知的自定义 `models_dir` 保护整个失效（写端把读端的 fail-closed 洗掉）。
    /// 回归场景即评审点名的三拍：隔离 → 普通 toggle/save → 严格查询仍拒绝。
    #[test]
    fn quarantined_config_blocks_plain_save_until_resolved() {
        let _home = TempHome::new();
        let path = config_file();

        // 第一拍：带自定义权重目录的合法配置损坏被隔离（与现有隔离测试同一坏法）。
        std::fs::write(
            &path,
            r#"{"store_url": "http://192.0.2.10:8088", "models_dir": "/home/u/ai/shared/weights", "gateway_port": "不是数字"}"#,
        )
        .unwrap();
        assert_eq!(
            load_settings().gateway_port,
            Settings::default().gateway_port,
            "坏文件应当回退到默认值"
        );
        let bad = path.with_extension("json.bad");
        assert!(bad.exists(), "坏文件应已隔离留存");
        assert!(!path.exists(), "原文件应已改名让位");

        // 第二拍：写端闸门（纵深防御）。生产 setter 的首读现在就走严格读取、
        // 还没到保存就会报错（二十八轮评审 P1）；本拍打的是首读之后、落盘之前
        // 配置才进入本态的窗口——`save_settings` 自己的严格重读必须兜住，不能
        // 把宽松口径的默认值固化成合法配置。
        let mut toggled = load_settings();
        toggled.min_free_space_gb = 3.0;
        let error = save_settings(&toggled).expect_err("隔离态普通保存必须被拒绝");
        assert!(
            error.to_string().contains("隔离留存"),
            "报错应指明隔离留存与出路：{error}"
        );
        assert!(
            !path.exists(),
            "被拒的保存不得留下半截产物（合法 config.json 会洗掉 fail-closed）"
        );
        let kept = std::fs::read_to_string(&bad).expect("隔离留存必须原样保留");
        assert!(
            kept.contains("/home/u/ai/shared/weights"),
            "留存内容不得被改动：{kept}"
        );

        // 第三拍：严格查询仍拒绝——写端被拒后，保护范围未知的判定没有变化。
        assert!(
            try_load_settings().is_err(),
            "隔离态下严格读取必须继续 fail-closed"
        );
        let roots = crate::features::model_store::store_weights_roots_checked();
        assert!(roots.is_err(), "破坏性消费方的权重根查询同样必须继续拒绝");

        // 对照流程 1：显式恢复（用户把留存修好放回原位）→ 保存放行、严格读取
        // 接受；历史留存不永久阻断已恢复的有效配置。
        std::fs::write(
            &path,
            r#"{"store_url": "http://192.0.2.10:8088", "models_dir": "/home/u/ai/shared/weights"}"#,
        )
        .unwrap();
        assert_eq!(
            try_load_settings()
                .expect("显式恢复后严格读取应接受")
                .models_dir,
            "/home/u/ai/shared/weights"
        );
        save_settings(&toggled).expect("显式恢复后普通保存放行");

        // 对照流程 2：显式放弃（确认不要旧配置，删掉新配置与历史留存）→ 回到
        // "配置缺失、无留存"的全新安装态 → 保存放行并建立新配置，严格读取从此
        // 接受。
        std::fs::remove_file(&path).expect("remove config");
        std::fs::remove_file(&bad).expect("abandon quarantine leftover");
        let mut fresh = load_settings();
        fresh.min_free_space_gb = 3.0;
        save_settings(&fresh).expect("显式放弃残留后普通保存放行");
        assert!(
            try_load_settings().is_ok(),
            "显式放弃后严格读取不应再被阻断"
        );
    }

    /// 不可读的配置同样挡住普通保存（二十七轮评审 P1）：config.json 在但读不出
    /// 来（如 mode 000）时，严格读取走"读取失败"分支报错且**不**隔离——留存不
    /// 存在，"缺失+留存"的旧判断为 false；宽松 setter 拿到默认值后整体写回，
    /// 父目录可写就能 rename 覆盖掉读不出来的原文件（rename(2) 不要求读被替换
    /// 文件），下一拍严格读取接受合法默认 JSON，原配置里未知的保护范围就此被
    /// 洗掉。回归场景即评审点名的三拍：不可读 → 普通 toggle/save → 严格保护
    /// 仍拒绝——不能只覆盖成功隔离后的 missing+.bad 分支。
    #[test]
    fn unreadable_config_blocks_plain_save_until_resolved() {
        let _home = TempHome::new();
        let path = config_file();

        // 第一拍：带自定义权重目录的合法配置落盘，随后对属主收走读权限——文件
        // 还在、内容未知（比如被同机其他用户或备份工具误伤）。
        std::fs::write(
            &path,
            r#"{"store_url": "http://192.0.2.10:8088", "models_dir": "/home/u/ai/shared/weights"}"#,
        )
        .unwrap();
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mut perm = fs::metadata(&path).unwrap().permissions();
            perm.set_mode(0o000);
            fs::set_permissions(&path, perm).unwrap();
        }
        assert!(try_load_settings().is_err(), "不可读必须让严格读取拒绝");
        assert!(path.exists(), "读取失败分支不得改名留存");
        assert!(
            !quarantine_bad_path(&path).exists(),
            "不可读分支不产生隔离留存"
        );

        // 第二拍：写端闸门（纵深防御，框架同隔离测试的第二拍）：生产 setter 的
        // 首读走严格读取在这里之前就报错；本拍钉住 `save_settings` 自己的严格
        // 重读兜住首读之后才变坏的窗口。宽松读取拿到默认值，保存必须被拒，
        // 而不是 rename 覆盖掉读不出来的原文件。
        let mut toggled = load_settings();
        toggled.min_free_space_gb = 3.0;
        let error = save_settings(&toggled).expect_err("不可读配置的普通保存必须被拒绝");
        assert!(
            error.to_string().contains("严格读取失败"),
            "报错应说明写端闸门来自严格读取：{error}"
        );
        assert!(
            path.exists(),
            "被拒的保存不得动原文件：文件被覆盖就是不可恢复的重置"
        );

        // 第三拍：严格保护仍拒绝——写端被拒后，保护范围未知的判定没有变化。
        assert!(try_load_settings().is_err());
        let roots = crate::features::model_store::store_weights_roots_checked();
        assert!(roots.is_err(), "破坏性消费方的权重根查询同样必须继续拒绝");

        // 对照：用户修复权限（显式恢复可读）→ 严格读取接受原配置；普通 setter
        // 从此按真实设置正常工作，写回的不丢自定义 models_dir。
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mut perm = fs::metadata(&path).unwrap().permissions();
            perm.set_mode(0o600);
            fs::set_permissions(&path, perm).unwrap();
        }
        assert_eq!(
            try_load_settings()
                .expect("权限修复后严格读取应接受")
                .models_dir,
            "/home/u/ai/shared/weights"
        );
        let mut recovered = load_settings();
        recovered.min_free_space_gb = 3.0;
        save_settings(&recovered).expect("权限修复后普通保存放行");
        let restored = try_load_settings().expect("写回后仍可读");
        assert_eq!(
            restored.models_dir, "/home/u/ai/shared/weights",
            "放行的保存写回的是真实设置，不是默认值"
        );
        assert_eq!(restored.min_free_space_gb, 3.0, "setter 的改动要落进新配置");
    }

    #[test]
    fn default_settings_have_expected_values() {
        let s = Settings::default();
        assert_eq!(s.gateway_port, 8090);
        assert_eq!(s.min_free_space_gb, 2.0);
        assert!(!s.gateway_enabled);
        assert!(s.startup_update_check_enabled);
        assert_eq!(s.max_concurrent_downloads(), 1, "默认只允许一个下载");
    }

    #[test]
    fn startup_update_policy_preserves_old_defaults_and_persisted_opt_out() {
        let _home = TempHome::new();
        assert!(startup_update_check_enabled().unwrap());
        fs::write(config_file(), "{}").unwrap();
        assert!(startup_update_check_enabled().unwrap());
        let mut settings = load_settings();
        settings.startup_update_check_enabled = false;
        save_settings(&settings).unwrap();
        assert!(!startup_update_check_enabled().unwrap());
        assert!(!load_settings().startup_update_check_enabled);
        let mut settings = load_settings();
        settings.import_scan_dirs = vec!["/extra/models".into()];
        save_settings(&settings).unwrap();
        assert!(!startup_update_check_enabled().unwrap());
        assert_eq!(
            load_settings().import_scan_dirs,
            vec!["/extra/models".to_string()],
            "显式保存的其他字段要落盘"
        );
    }

    #[test]
    fn startup_update_policy_does_not_default_on_unreadable_or_malformed_settings() {
        let _home = TempHome::new();
        for text in ["{broken", r#"{"startup_update_check_enabled":"false"}"#] {
            fs::write(config_file(), text).unwrap();
            assert!(startup_update_check_enabled().is_err());
            assert_eq!(fs::read_to_string(config_file()).unwrap(), text);
        }
        fs::remove_file(config_file()).unwrap();
        fs::create_dir(config_file()).unwrap();
        assert!(startup_update_check_enabled().is_err());
    }

    #[test]
    fn concurrent_downloads_is_clamped() {
        // 配置文件被手改坏了也不能让下载彻底卡死或者放任无限并发
        let mut s = Settings {
            max_concurrent_downloads: 0,
            ..Default::default()
        };
        assert_eq!(s.max_concurrent_downloads(), 1);
        s.max_concurrent_downloads = 99;
        assert_eq!(s.max_concurrent_downloads(), 3);
    }

    /// 回退文件必须按 0600 落盘，并且读得回来（空白会被 trim 掉）。
    ///
    /// 这几个测试只碰回退文件那一半，**不调 `set_hf_token` / `get_hf_token`**：
    /// 那两个会先去够系统密钥环，在开发机上就是往真实的 Secret Service 里写东西。
    #[test]
    fn token_fallback_round_trips_at_0600() {
        let _home = TempHome::new();
        assert_eq!(read_token_fallback(), None, "干净环境下不该读出 token");

        write_token_fallback("hf_abc123\n");
        assert_eq!(read_token_fallback().as_deref(), Some("hf_abc123"));

        use std::os::unix::fs::PermissionsExt as _;
        let mode = fs::metadata(token_fallback_file())
            .unwrap()
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(mode, 0o600, "回退文件权限是 {mode:o}，同机其他用户读得到");
    }

    /// 密钥环恢复之后，早先那次降级留下的明文必须被清掉。
    ///
    /// 场景：某次设 token 时密钥环不可用 → 明文落回退文件；后来密钥环好了，
    /// 用户再改或再删 token 走的是密钥环成功分支。那条分支要是不清一次回退文件，
    /// 这份作废的 token 就永远躺在用户主目录里，而 `get_hf_token` 优先读密钥环，
    /// 界面上一切正常，用户根本无从察觉。
    #[test]
    fn clearing_the_fallback_removes_an_earlier_plaintext_token() {
        let _home = TempHome::new();
        write_token_fallback("hf_stale_from_a_degraded_run");
        assert!(token_fallback_file().exists());

        clear_token_fallback();
        assert!(
            !token_fallback_file().exists(),
            "降级时期留下的明文 token 文件没被清掉"
        );
        assert_eq!(read_token_fallback(), None);

        // 再清一次不能出事：`set_hf_token` 每次走通密钥环都会调它，
        // 而绝大多数机器上这个文件根本就不存在。
        clear_token_fallback();
    }
}

#[cfg(test)]
mod transaction_tests;
