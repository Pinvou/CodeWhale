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
use std::path::PathBuf;

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
fn store_root() -> PathBuf {
    let d = dirs_home().join(".pinvou3").join("model-store");
    let _ = fs::create_dir_all(&d);
    d
}

pub fn config_dir() -> PathBuf {
    store_root()
}

pub fn data_dir() -> PathBuf {
    store_root()
}

fn dirs_home() -> PathBuf {
    std::env::var("HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from("/"))
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
    pub store_url: String,
    pub update_url: Option<String>,
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

    /// 测试模型商店是否开着。
    ///
    /// 入口是隐藏的：设置页「关于」里连点版本号若干次再输口令才能开（见
    /// app::commands::settings::test_store_unlock）。这不是安全边界，只是防止
    /// 普通用户误入——口令哈希编译在二进制里，逆向拿得到。真要挡住有心人，
    /// 得让测试卡片根本不进发布包。
    pub test_mode_enabled: bool,
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
            // 商店入口：account-platform 反向隧道的公网入口。2026-09-10 起商店所在的
            // 那台服务器改成了只收隧道流量的私有源站，直连 IP 一律 403。
            store_url: "https://account.pinvou.com/model-store".to_string(),
            update_url: Some("https://pinvou.com/model-cube-update".to_string()),
            gateway_enabled: false,
            gateway_port: 8090,
            max_concurrent_downloads: 1,
            import_scan_dirs: Vec::new(),
            test_mode_enabled: false,
        }
    }
}

pub const HF_ENDPOINT_PRESETS: &[&str] = &["https://huggingface.co", "https://hf-mirror.com"];

impl Settings {
    /// 夹到 1..=3。配置文件可能被手改成 0（那样谁都下不了）或者 99，不能信。
    pub fn max_concurrent_downloads(&self) -> usize {
        (self.max_concurrent_downloads as usize).clamp(1, 3)
    }

    pub fn staging_path(&self) -> PathBuf {
        let p = PathBuf::from(&self.staging_dir);
        let _ = fs::create_dir_all(&p);
        p
    }

    pub fn models_path(&self) -> PathBuf {
        let p = PathBuf::from(&self.models_dir);
        let _ = fs::create_dir_all(&p);
        p
    }
}

/// 出现在配置文件里、又恰好等于某个**历史默认值**的商店地址——说明用户从没改过
/// 它，是当年写配置时顺手存进去的，跟着新默认走。用户自己填的别的地址一律不动。
///
/// 为什么要迁：配置文件把 store_url 原样存下来了，只改代码里的默认值，老用户读到
/// 的永远是旧值。旧地址 http://47.120.8.237/model-store 自 2026-09-10 起一律 403
/// （那台服务器改成了只收反向隧道流量的私有源站），不迁的话老用户的模型商店就
/// 一直"连不上"——而且报错里看不出任何线索。
const LEGACY_DEFAULT_STORE_URLS: &[&str] = &["http://47.120.8.237/model-store"];

pub fn load_settings() -> Settings {
    let mut settings = Settings::default();
    let cfg_file = config_file();
    if let Ok(text) = fs::read_to_string(&cfg_file) {
        if let Ok(value) = serde_json::from_str::<serde_json::Value>(&text) {
            // 逐字段合并，而不是整体反序列化覆盖：老配置文件缺字段时要保留
            // 上面 Default 给的默认值，不能让 serde 把它们清成各类型的零值。
            if let Ok(partial) = serde_json::from_value::<Settings>(value) {
                settings = partial;
            }
        }
    }
    if settings.staging_dir.trim().is_empty() {
        settings.staging_dir = data_dir().join("staging").to_string_lossy().to_string();
    }
    if settings.models_dir.trim().is_empty() {
        // 默认跟其余数据放在一起。LMM 那边默认的是 `~/models`——那是它作为独立
        // 应用时的取值，在品悟里会在用户主目录下凭空多一个没人认识的目录。
        // 这个值可配：几十上百 GB 的权重，用户很可能想放到别的盘。
        settings.models_dir = data_dir().join("models").to_string_lossy().to_string();
    }
    if LEGACY_DEFAULT_STORE_URLS.contains(&settings.store_url.trim().trim_end_matches('/')) {
        settings.store_url = Settings::default().store_url;
    }
    settings
}

/// 写配置。**先写临时文件再 rename**，并且一开始就以 0600 创建。
///
/// 两件事都不是讲究：
///
/// 1. 直接 `fs::write` 覆盖不是原子的。保存那一瞬断电或被杀，留下的是半截 JSON，
///    而 `load_settings` 解析失败会**静默回退到全部默认值**——用户自己配的权重
///    目录、商店地址、导入扫描目录、测试商店开关一起无声消失，界面上没有任何
///    异常，他只会觉得"设置怎么自己变回去了"。同模块 orphans.rs 的 `store_unlocked`
///    早就是这么写的，照搬即可。
/// 2. 先写后 `set_permissions` 之间有一个 0644 的窗口，而这个文件里有商店地址
///    这类不该给同机其他用户看的东西。用 `OpenOptions::mode` 直接按 0600 建，
///    窗口就不存在了。
pub fn save_settings(settings: &Settings) -> std::io::Result<()> {
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
#[cfg(test)]
pub struct TempHome {
    dir: tempfile::TempDir,
    previous: Option<std::ffi::OsString>,
    // 声明在最后 => 最后析构：HOME 已经还原、临时目录也删掉之后才放锁。
    _lock: std::sync::MutexGuard<'static, ()>,
}

#[cfg(test)]
impl TempHome {
    pub fn new() -> Self {
        let lock = ENV_TEST_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let dir = tempfile::tempdir().unwrap();
        let previous = std::env::var_os("HOME");
        // SAFETY: edition 2024 起 set_var 是 unsafe——并发改环境变量是 UB。
        // 这里全程持有 ENV_TEST_LOCK，同一时刻只有一个测试在动 HOME。
        unsafe { std::env::set_var("HOME", dir.path()) };
        Self {
            dir,
            previous,
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
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 配置文件里存着旧默认商店地址的，读出来要换成新地址；用户自己改过的不动。
    #[test]
    fn legacy_default_store_url_is_migrated_but_custom_one_is_kept() {
        // 把整个数据目录切到临时目录：store_root() 是 $HOME/.pinvou3/model-store，
        // 只认 HOME。这里**不能**改成设 XDG_*——那是 LMM 独立应用时期的路径，
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

        // 带不带结尾斜杠都算旧默认值
        std::fs::write(
            &path,
            r#"{"store_url": "http://47.120.8.237/model-store/"}"#,
        )
        .unwrap();
        assert_eq!(load_settings().store_url, Settings::default().store_url);

        // 192.0.2.x 是 RFC5737 的文档保留段（TEST-NET-1），跟任何真实环境无关。
        // 这条断言只关心"不在 LEGACY_DEFAULT_STORE_URLS 名单里的地址一律不动"，
        // 与地址是公网还是私网无关，所以换成文档段不影响语义。
        std::fs::write(&path, r#"{"store_url": "http://192.0.2.10:8088"}"#).unwrap();
        assert_eq!(
            load_settings().store_url,
            "http://192.0.2.10:8088",
            "用户自定义的地址被改掉了"
        );
    }

    #[test]
    fn default_settings_have_expected_values() {
        let s = Settings::default();
        assert_eq!(s.gateway_port, 8090);
        assert_eq!(s.min_free_space_gb, 2.0);
        assert!(!s.gateway_enabled);
        assert_eq!(s.max_concurrent_downloads(), 1, "默认只允许一个下载");
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
