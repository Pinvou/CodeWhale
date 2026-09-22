//! 下载并执行模型商店卡片背后的脚本（install_engine/deploy/download_model/
//! enable/stop/uninstall）。
//!
//! 调用约定：脚本通过环境变量接收参数（而不是命令行参数），常见的有：
//! `LMM_CARD_ID` / `LMM_MODEL_NAME` / `LMM_ENGINE_NAME` / `LMM_SOURCE_REPO_ID` /
//! `LMM_SOURCE_FILENAME` / `LMM_HF_ENDPOINT` / `LMM_HF_TOKEN` / `LMM_STAGING_DIR` /
//! `LMM_OLLAMA_HOST`。具体某个脚本用得到哪些变量、要求哪些必填，由脚本自己声明；
//! 本应用统一把已知的变量都传过去，脚本按需取用即可。

use std::collections::HashMap;
use std::fs::File;
use std::io::{Read, Write};
use std::os::fd::{AsFd, OwnedFd};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use nix::sys::signal::{Signal, kill, killpg};
use nix::unistd::Pid;

use super::errors::{AppError, humanize_script_message};

pub type ScriptError = AppError;

#[derive(Debug, Clone)]
pub struct ScriptProgress {
    pub line: String,
    pub done: bool,
    pub error: Option<String>,
}

impl ScriptProgress {
    fn line(line: impl Into<String>) -> Self {
        Self {
            line: line.into(),
            done: false,
            error: None,
        }
    }
    fn done() -> Self {
        Self {
            line: String::new(),
            done: true,
            error: None,
        }
    }
    fn cancelled() -> Self {
        Self {
            line: String::new(),
            done: false,
            error: Some("cancelled".to_string()),
        }
    }
}

pub type ProgressCallback<'a> = dyn FnMut(ScriptProgress) + 'a;

/// 脚本请求一次用户输入的标记：`LMM_PROMPT=<kind>|<提示语>`。
///
/// 打印这一行之后，脚本应当从 stdin 读一行作为答案。stdout/stdin 接的是同一个
/// pty，所以我们把答案写回 pty 主端，脚本那边的 `read` 就能读到。
///
/// kind 目前认三种：`text`（普通文本）/ `password`（界面上掩码显示，且不落日志）
/// / `confirm`（是否，答案是 `yes`/`no`）。不认识的 kind 一律按 text 处理——
/// 卡片脚本可能比应用新，不该因为多了一种输入类型就整个跑不动。
///
/// `password` 那句"不落日志"由**两道**校验共同兜住，缺一道就只是句空话：
/// spawn 前关掉 pty 回显（`build_command`，关不掉就不跑），以及每次写答案前
/// 重新确认回显还关着（`echo_is_off`，脚本中途 `stty echo` 就在这里被拦下）。
pub const PROMPT_MARKER: &str = "LMM_PROMPT=";

/// 一次问齐多项的表单：`LMM_PROMPT_FORM={"title":…,"fields":[…]}`（JSON 必须
/// 压在一行里）。脚本照样 `read` 一行，拿到的是一行 JSON。
///
/// 为什么要有它：双机部署一次要问从机 IP、用户名、密码、本机密码四项，用
/// 一问一答会连弹四个框——而这个流程的目标恰恰是"填一次就不管了"。
pub const PROMPT_FORM_MARKER: &str = "LMM_PROMPT_FORM=";

/// 这一行是不是在请求用户输入。
///
/// 注意顺序无所谓但前缀不能搞混：`LMM_PROMPT_FORM=` 并不以 `LMM_PROMPT=` 开头
/// （中间是下划线不是等号），两者不会互相误判。
fn is_prompt_line(line: &str) -> bool {
    let t = line.trim();
    t.starts_with(PROMPT_MARKER) || t.starts_with(PROMPT_FORM_MARKER)
}

// Rust 侧到此为止：只需要知道"这一行在要输入"，好在这里阻塞去读 stdin。
// 把 `<kind>|<提示语>` 真正拆开、决定弹什么控件，是**界面**的事——
// LMM 自己的 TaskCenter.jsx 和宿主应用（品悟）的 model-store-logic.js 各有一份
// 解析器和各自的测试。这里不要再放一份"看起来像生产路径"的解析函数。

/// 等用户作答的上限。用户可能正好走开了，不能无限占着这次运行；但也不能用
/// 常规的 idle_timeout（300 秒）——那是用来判"脚本卡死"的，而等输入期间脚本
/// 本来就该一声不吭。
const PROMPT_TIMEOUT: Duration = Duration::from_secs(600);

/// 一次交互输入的应答槽：界面答完写进来，脚本执行循环轮询取走。
///
/// 用轮询而不是阻塞回调，是因为执行循环同时还要盯着取消信号、绝对超时和 pty
/// 输出；一旦在回调里同步等界面，这三样就全停摆了——用户点"取消"会没反应。
#[derive(Default)]
pub struct PromptSlot {
    answer: parking_lot::Mutex<Option<String>>,
    /// 用户在输入框上点了"取消"
    aborted: AtomicBool,
}

impl PromptSlot {
    pub fn answer(&self, text: impl Into<String>) {
        *self.answer.lock() = Some(text.into());
    }
    pub fn abort(&self) {
        self.aborted.store(true, Ordering::SeqCst);
    }
    fn take(&self) -> Option<String> {
        self.answer.lock().take()
    }
    fn is_aborted(&self) -> bool {
        self.aborted.load(Ordering::SeqCst)
    }
}

/// 一次长操作的"外部控制通道"：取消信号 + 交互输入应答槽。
///
/// 合成一个结构而不是并列两个参数：这两样东西生命周期完全一致（都由前端的
/// runId 注册、随任务结束销号），而且要从命令层一路穿过七八层调用才到脚本
/// 执行处——每多一样就得把全部签名再改一遍。
#[derive(Clone, Default)]
pub struct RunControl {
    pub cancel: Option<Arc<AtomicBool>>,
    pub prompt: Option<Arc<PromptSlot>>,
}

impl RunControl {
    pub fn cancelled(&self) -> bool {
        self.cancel
            .as_ref()
            .is_some_and(|f| f.load(Ordering::SeqCst))
    }
}

/// 把一段已经在手的脚本正文落成可执行临时文件。
///
/// 用于"本地固化副本"这条路径：脚本在下载模型时就抓下来存进数据库了，之后
/// 部署/启用/停用/卸载直接用这份副本执行，完全不联商店——商店重启、上架新
/// 卡片、改脚本都不会打断用户正在做的事。
pub fn materialize_script(content: &str) -> Result<PathBuf, ScriptError> {
    write_executable_temp(content)
}

/// 把卡片的附属文件树写进 `dir`（路径 -> 正文），并把 `.sh` 置为可执行。
///
/// **只写、不删。** 卡片包自己会在这个目录里生成运行期状态（config.json、
/// node.json、state/、data/ 之类），每次执行前清空目录会把这些状态一起抹掉，
/// 用户的部署就废了。所以这里只覆盖卡片带来的那些文件，其余一概不碰。
///
/// 路径必须是相对的、且不能跳出 `dir`——卡片来自网络，`../` 这种路径要是照写
/// 就等于让卡片往任意位置写文件。
pub fn materialize_assets(
    dir: &Path,
    files: &std::collections::BTreeMap<String, String>,
) -> Result<(), ScriptError> {
    for (rel, content) in files {
        let safe = sanitize_relative_path(rel)?;
        let target = dir.join(&safe);
        if let Some(parent) = target.parent() {
            std::fs::create_dir_all(parent).map_err(|e| {
                ScriptError::new(format!("创建目录 {} 失败：{e}", parent.display()))
            })?;
        }
        std::fs::write(&target, content)
            .map_err(|e| ScriptError::new(format!("写入 {} 失败：{e}", target.display())))?;
        if safe.extension().and_then(|e| e.to_str()) == Some("sh") {
            let mut perms = std::fs::metadata(&target)
                .map_err(|e| ScriptError::new(format!("读取 {} 权限失败：{e}", target.display())))?
                .permissions();
            perms.set_mode(perms.mode() | 0o111);
            let _ = std::fs::set_permissions(&target, perms);
        }
    }
    Ok(())
}

/// 绝不能出现在提权进程 argv 里的环境变量。
///
/// pkexec 不透传环境变量，我们只能把 env 拼进 argv，而 argv 对本机所有用户可见
/// （`ps` / `/proc/<pid>/cmdline`）。列在这里的键在提权执行前会被剥掉。
const SECRET_ENV_KEYS: [&str; 1] = ["LMM_HF_TOKEN"];

/// 校验并归一化卡片里声明的相对路径。
///
/// 拒绝绝对路径、`..`、以及 Windows 风格的盘符——卡片内容来自网络，路径直接
/// 拿来拼接就等于把「往任意位置写文件」的能力交给了卡片作者（或中间人）。
fn sanitize_relative_path(rel: &str) -> Result<PathBuf, ScriptError> {
    let rel = rel.trim().replace('\\', "/");
    if rel.is_empty() {
        return Err(ScriptError::new("卡片里有一个空的文件路径"));
    }
    let p = Path::new(&rel);
    if p.is_absolute() || rel.contains(':') {
        return Err(ScriptError::new(format!(
            "卡片文件路径不能是绝对路径：{rel}"
        )));
    }
    let mut out = PathBuf::new();
    for comp in p.components() {
        match comp {
            std::path::Component::Normal(c) => out.push(c),
            std::path::Component::CurDir => {}
            _ => {
                return Err(ScriptError::new(format!(
                    "卡片文件路径不允许含有 .. 或根目录：{rel}"
                )));
            }
        }
    }
    if out.as_os_str().is_empty() {
        return Err(ScriptError::new(format!("卡片文件路径无效：{rel}")));
    }
    Ok(out)
}

/// 回收子进程，**有界**——绝不在取消/超时路径上无限等。
///
/// 原来这里是 `try_wait()`（结果丢掉）之后无条件 `wait()`，而 `wait()` 是无界阻塞。
/// 在 `need_root` 的取消路径上这是致命的：pkexec 认证之后是**原位 exec 替换**，
/// 子进程已经变成 root 进程，非 root 身份发 SIGTERM 得到 EPERM——杀不掉。于是
/// `wait()` 一直挂到那个 root 脚本自然结束（install_engine 要 docker pull 几十 GB，
/// 可达小时级）。
///
/// 后果是整条链路僵住，而且**用户只能重启应用**：工作线程不返回 →
/// `OpCleanup::drop` 不执行 → 这张卡在 `ActiveCardOps` 里永不释放（此后任何操作
/// 都被拒为「正在…请等待」）→ `RunLog::finish` 永不调用 → 前端长轮询永远等不到
/// 结局。取消本是为了让用户脱身，结果反而把卡片彻底焊死，跟本文件
/// 「取消必须中断调用链」的自订契约正好相反。
///
/// 做法：先轮询一小段（正常路径上进程刚被 SIGKILL，几十毫秒内就退出），到点还
/// 没走就把那个阻塞的 `wait()` 丢给一个分离线程。这样既不挂住调用方，也不留下
/// 僵尸进程——两者都要，只做前者会把回收问题换成句柄泄漏。
///
/// 真正遗留下来的进程靠 orphans 的登记兜底，但**兜得住什么必须说清楚**（这里
/// 原来写的是"下次启动会被收掉"，与事实相反）：
/// - 非 root 的残留进程：登记在案，下次启动 `cleanup_on_startup` 会按进程组
///   真的杀掉；
/// - `need_root` 的残留进程：那是 root 进程，非 root 的应用即使是它父进程也
///   没权限发信号——下次启动**只能在 `skipped_root` 里报一笔**给用户看，杀不掉。
///
/// 而且这份登记默认在 `RunGuard` 析构（= run_script 一返回）时就销号了。所以
/// run_script 在取消/超时返回前必须对"还活着的 root 进程"显式调用
/// `RunGuard::keep`，否则连那一笔上报都没有：用户点完取消，root 的 docker pull
/// 继续拉几十 GB，重启应用零痕迹。
fn reap_child(mut child: std::process::Child) {
    let deadline = Instant::now() + Duration::from_millis(500);
    loop {
        match child.try_wait() {
            // 已退出，或者连状态都拿不到（那也没什么可做的）
            Ok(Some(_)) | Err(_) => return,
            Ok(None) => {}
        }
        if Instant::now() >= deadline {
            break;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    std::thread::spawn(move || {
        let _ = child.wait();
    });
}

fn write_executable_temp(content: &str) -> Result<PathBuf, ScriptError> {
    let mut tmp = tempfile::Builder::new()
        .prefix("lmm-script-")
        .suffix(".sh")
        .tempfile()
        .map_err(|e| ScriptError::new(format!("创建临时脚本文件失败：{e}")))?;
    tmp.write_all(content.as_bytes())
        .map_err(|e| ScriptError::new(format!("写入临时脚本文件失败：{e}")))?;
    let path = tmp.into_temp_path();
    let mut perms = std::fs::metadata(&path)
        .map_err(|e| ScriptError::new(format!("读取临时脚本文件权限失败：{e}")))?
        .permissions();
    perms.set_mode(perms.mode() | 0o111);
    std::fs::set_permissions(&path, perms)
        .map_err(|e| ScriptError::new(format!("设置临时脚本文件权限失败：{e}")))?;
    // 保留这个路径不被自动删除（tempfile 默认 drop 时清理），生命周期交给
    // run_script 在收尾阶段统一清理。
    path.keep()
        .map_err(|e| ScriptError::new(format!("保留临时脚本文件失败：{e}")))
}

/// 计算脚本正文的 sha256（十六进制），用于跟商店登记的哈希比对。
pub fn sha256_hex(content: &str) -> String {
    use sha2::{Digest, Sha256};
    crate::platform::encoding::hex_lower(&Sha256::digest(content.as_bytes()))
}

/// 取脚本正文要跨过的网络环境很不友好：设备很可能正在全速下模型权重，把带宽
/// 占得死死的，一个几 KB 的脚本请求也可能要十几秒才读得完。所以超时给宽、并且
/// 允许重试几次——真机上就因为总超时只有 15 秒且不重试，报出过
/// 「下载脚本失败：error decoding response body」，把跑到一半的下载整个打断。
const SCRIPT_FETCH_ATTEMPTS: u32 = 3;

fn http_get_text(url: &str) -> Result<String, ScriptError> {
    let client = reqwest::blocking::Client::builder()
        .connect_timeout(Duration::from_secs(10))
        .timeout(Duration::from_secs(60))
        .build()
        .map_err(|e| ScriptError::new(format!("创建 HTTP 客户端失败：{e}")))?;

    let mut last_err = String::new();
    for attempt in 1..=SCRIPT_FETCH_ATTEMPTS {
        match client
            .get(url)
            .send()
            .and_then(|r| r.error_for_status())
            .and_then(|r| r.text())
        {
            Ok(text) => return Ok(text),
            Err(e) => {
                // 4xx 是商店真的没有这个脚本，重试多少次结果都一样，立刻报错。
                if e.status().map(|s| s.is_client_error()).unwrap_or(false) {
                    return Err(ScriptError::new(format!("下载脚本失败：{e}")));
                }
                last_err = e.to_string();
                if attempt < SCRIPT_FETCH_ATTEMPTS {
                    std::thread::sleep(Duration::from_secs(2 * attempt as u64));
                }
            }
        }
    }
    Err(ScriptError::new(format!(
        "下载脚本失败（已重试 {SCRIPT_FETCH_ATTEMPTS} 次）：{last_err}"
    )))
}

/// 「来源不可信」这一类拒绝的统一前缀。
///
/// 三种拒绝（明文 http、该验签却没有哈希、真实商店给了本地路径）共用一个前缀，
/// 是为了让 `lifecycle::ModelManager::is_fatal_failure` 只登记一个词就能把它们
/// 一起挡在**自动提权**之外——那份清单是按文字片段匹配的，共用前缀之后将来再
/// 加一条来源策略也不会漏登记。
pub const UNTRUSTED_SOURCE_PREFIX: &str = "脚本来源不可信";

/// 脚本中途把 pty 回显打开时的中止语（见 run_script 写答案前的重校验）。
/// 同样要进致命清单：这条路径上刚发生的是"脚本试图骗出明文密码"，
/// 绝不能再自动提权把同一份内容以 root 重跑一遍。
pub const ECHO_REOPENED_ABORT: &str = "脚本把终端回显打开了";

/// 取脚本/附属文件正文时对**来源**的约束。
///
/// 为什么做成显式参数、而不是在函数里看着 url 自己猜：这里取回来的内容最终会
/// 被当成 shell 脚本执行，install_engine 那条还会经 pkexec 变成 root。判断
/// 「这份内容值不值得信」要的三件事——信道加不加密、该不该有登记哈希、允不允许
/// 读本地文件——没有一件能从 url 字符串本身推出来，只有调用方知道。
///
/// 默认值是**最严**的一档（两个 false），所以新调用方漏填的后果是被拒绝，
/// 而不是悄悄放行。
#[derive(Debug, Clone, Copy, Default)]
pub struct FetchPolicy {
    /// 这份内容有机会以 root 身份执行。为真时**必须**有登记哈希，没有就拒绝。
    ///
    /// 要说清这道校验的边界：哈希跟内容同信道（同一个商店后台、同一张卡片
    /// 详情），所以它**只防传输损坏和后台数据被改，不防商店本身被伪造**——
    /// 真正的签名机制这个版本还没有。它之所以仍然值钱，是因为下面那条 https
    /// 硬要求：TLS 保证了信道另一头确实是设置里那个商店，哈希保证了这一头拿到
    /// 的就是那张卡片详情里登记的那份内容。少了任何一半，另一半都不成立。
    pub may_run_as_root: bool,
    /// 允许把 `url` 当本地文件路径读。
    ///
    /// 只有内置测试商店会这样：它没有服务器，卡片详情里的"URL"就是应用自己
    /// 刚写到 `data_dir()/test_store_scripts/` 下的那个文件路径。真实商店一律
    /// 不给这个开关——否则一张被改过的卡片只要把 URL 写成任意一个本地路径，
    /// 就能让应用去读它、再把读到的内容当脚本执行。
    pub allow_local_path: bool,
}

/// 从 URL（或测试商店的本地路径）取正文，先按 `policy` 校验来源、再校验哈希。
/// 调用方拿到正文可以直接固化进数据库，不用再重复下载。
///
/// 这三道来源校验不是各自独立的洁癖，是一条链上的三个环节：任何一环放行，
/// 「未验签的网络内容被 root 执行」这条路就重新通了（install_engine 走 pkexec，
/// 而且非 root 安装失败时 lifecycle 还有一次自动提权重跑）。
pub fn fetch_script_content(
    url: &str,
    expected_sha256: Option<&str>,
    policy: FetchPolicy,
) -> Result<String, ScriptError> {
    // ① 要以 root 跑的脚本，没有登记哈希就不跑。
    //
    // 这个参数以前是纯 Option：商店不下发就跳过校验、"保持兼容"。兼容的是
    // 一个并不存在的老后台——生产商店的 card_scripts 表每一行都带 sha256，
    // 内置测试商店的哈希是应用自己算的，两边都给。而代价是：真有一天哪张卡片
    // 少了这个字段（或者有人把它删了），install_engine 会**不声不响**地以 root
    // 执行一份没人校验过的内容。
    if policy.may_run_as_root && expected_sha256.is_none() {
        return Err(ScriptError::new(format!(
            "{UNTRUSTED_SOURCE_PREFIX}：这一步要以管理员权限执行，但商店没有下发该脚本的 \
             sha256，无法确认内容没被改动，已中止执行"
        )));
    }

    let content = if url.starts_with("https://") {
        http_get_text(url)?
    } else if url.starts_with("http://") {
        // ② 明文 HTTP 一律拒绝。
        //
        // 在明文链路上，内容和用来校验它的哈希是同一条路下来的——中间人把两样
        // 一起改掉就行，校验等于没做。以前这里跟 https 一视同仁地放行，而
        // install_engine 紧接着就是 pkexec。
        return Err(ScriptError::new(format!(
            "{UNTRUSTED_SOURCE_PREFIX}：脚本地址不是 https（{url}）。明文链路上内容和\
             校验值会被一起篡改，而这些脚本要在本机执行，已中止；请把设置里的模型商店\
             地址改成 https 开头的地址"
        )));
    } else if policy.allow_local_path {
        std::fs::read_to_string(url)
            .map_err(|e| ScriptError::new(format!("读取本地脚本失败：{e}")))?
    } else {
        // ③ 真实商店给的地址只能是 https 链接。
        //
        // 以前这里是"不是 http(s) 就按本地路径读"，也就是卡片里写什么路径就读
        // 什么路径。卡片来自网络，这等于把"读本机任意文件、再把它当脚本执行"
        // 的能力交了出去。
        return Err(ScriptError::new(format!(
            "{UNTRUSTED_SOURCE_PREFIX}：商店给的脚本地址不是 https 链接（{url}）。\
             只有打包在应用里的测试商店才允许用本地文件路径，已中止"
        )));
    };

    if let Some(expected) = expected_sha256 {
        let actual = sha256_hex(&content);
        if !actual.eq_ignore_ascii_case(expected) {
            let expected_prefix: String = expected.chars().take(12).collect();
            let actual_prefix: String = actual.chars().take(12).collect();
            return Err(ScriptError::new(format!(
                "脚本完整性校验失败：实际内容与商店登记的不一致（期望 sha256={expected_prefix}…，\
                 实际 {actual_prefix}…），可能是传输被篡改或后台数据被改动，已中止执行"
            )));
        }
    }
    Ok(content)
}

/// 取脚本并落成可执行临时文件（= fetch_script_content + materialize_script）。
///
/// **目前零调用点**：lifecycle 走的是 `ScriptsSource::prepare`——它还要处理多
/// 文件卡片的整棵文件树和持久工作目录，不能只落一个临时文件。
///
/// `url` 是商店服务器下发的 https 地址；测试模型商店没有服务器，直接给的是本地
/// 脚本文件路径——哪一种算合法由 `policy` 决定，见 [`FetchPolicy`]。
///
/// `expected_sha256` 是商店卡片详情里登记的脚本哈希，下发了就必须匹配；
/// `policy.may_run_as_root` 为真时它还是**必填**的。
pub fn download_script(
    url: &str,
    expected_sha256: Option<&str>,
    policy: FetchPolicy,
) -> Result<PathBuf, ScriptError> {
    let content = fetch_script_content(url, expected_sha256, policy)?;
    materialize_script(&content)
}

/// 从 buf 里把已经凑成一整行的部分挑出来喂给 on_progress，返回剩下还没凑成
/// 一行的残余内容。进度条常见的写法是只用 `\r`（不用 `\n`）反复重绘同一行，
/// 所以这里把 `\r` 和 `\n` 都当作行结束符；连续的 `\r\n` 当一个结束符处理，
/// 避免每行都被拆成"内容行 + 一条空行"。
/// 去掉终端控制序列（ANSI escape）。
///
/// 子进程接的是 pty，`docker pull` / `hf download` 这类工具会认为自己在真终端
/// 里，于是用「光标上移 + 清行」的方式重绘进度（`[1A[2K`…）。这些
/// 字节原样塞进日志框会显示成一串乱码方块，把真正有用的信息淹掉——真机上
/// docker pull 的日志就是这样没法看的。
fn strip_ansi(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    let mut chars = s.chars().peekable();
    while let Some(c) = chars.next() {
        if c != '' {
            out.push(c);
            continue;
        }
        match chars.peek() {
            // CSI：ESC [ 参数… 终止字符(0x40..=0x7e)
            Some('[') => {
                chars.next();
                for c in chars.by_ref() {
                    if ('@'..='~').contains(&c) {
                        break;
                    }
                }
            }
            // OSC：ESC ] …… 以 BEL 或 ESC \ 结束
            Some(']') => {
                chars.next();
                while let Some(c) = chars.next() {
                    if c == '' {
                        break;
                    }
                    if c == '' {
                        chars.next();
                        break;
                    }
                }
            }
            // 其它双字符转义（ESC M 之类）：连同下一个字符一起丢掉
            Some(_) => {
                chars.next();
            }
            None => {}
        }
    }
    out
}

/// 按流解 UTF-8：**不完整的尾巴留到下一块**，不要当场判成坏字节。
///
/// 从 pty 读回来的是字节流，一次 read 切在哪里跟字符边界毫无关系。一个中文字
/// 占 3 字节，4096 边界正好落在它中间的概率是 3/4096——按每 GB 日志算，一次
/// 下载就能撞上几百次。原先每块单独 `from_utf8_lossy`，撞上就把那个字**永久**
/// 变成两个替换符（前半截一个、后半截一个），而且是进度条这类高频刷新的行最
/// 容易中招。留住尾巴之后，同一个字的两半会在下一块拼回去。
///
/// 真正的坏字节（`error_len()` 有值）还是要吐一个替换符顶掉——否则它会永远卡
/// 在 pending 里，后面**所有**输出都跟着解不出来，日志直接哑掉。
fn decode_utf8_stream(pending: &mut Vec<u8>, chunk: &[u8]) -> String {
    pending.extend_from_slice(chunk);
    let mut out = String::new();
    loop {
        match std::str::from_utf8(pending) {
            Ok(s) => {
                out.push_str(s);
                pending.clear();
                return out;
            }
            Err(e) => {
                let valid = e.valid_up_to();
                if let Ok(s) = std::str::from_utf8(&pending[..valid]) {
                    out.push_str(s);
                }
                match e.error_len() {
                    // 尾部只是还没读完。UTF-8 一个字符最多 4 字节，所以留下来的
                    // 永远不超过 3 字节，pending 不会无限涨。
                    None => {
                        pending.drain(..valid);
                        return out;
                    }
                    Some(bad) => {
                        out.push(char::REPLACEMENT_CHARACTER);
                        pending.drain(..valid + bad);
                    }
                }
            }
        }
    }
}

fn drain_lines(
    buf: String,
    output_lines: &mut Vec<String>,
    on_progress: &mut Option<&mut ProgressCallback<'_>>,
) -> String {
    let mut rest = buf;
    loop {
        let idx_n = rest.find('\n');
        let idx_r = rest.find('\r');
        let (idx, skip) = match (idx_n, idx_r) {
            (None, None) => return rest,
            // 回车在换行之前（或者压根没有换行）就按回车切。用 is_none_or 而不是
            // `is_none() || … unwrap()`：后者要读的人自己去确认两个分支互斥。
            (idx_n, Some(idx_r)) if idx_n.is_none_or(|n| idx_r < n) => {
                let skip = if rest[idx_r..].starts_with("\r\n") {
                    2
                } else {
                    1
                };
                (idx_r, skip)
            }
            (Some(idx_n), _) => (idx_n, 1),
            _ => unreachable!(),
        };
        let line = strip_ansi(&rest[..idx]);
        rest = rest[idx + skip..].to_string();
        if !line.trim().is_empty() {
            output_lines.push(line.clone());
            if let Some(cb) = on_progress.as_deref_mut() {
                cb(ScriptProgress::line(line));
            }
        }
    }
}

#[derive(Default)]
pub struct RunScriptOptions<'a> {
    pub on_progress: Option<&'a mut ProgressCallback<'a>>,
    /// 取消信号 + 交互输入应答槽
    pub control: RunControl,
    pub need_root: bool,
    pub timeout: Option<Duration>,
    pub idle_timeout: Option<Duration>,
    /// 给这次运行起一个人话名字（比如「下载 Qwen3-8B」）。只用于应用异常退出后，
    /// 下次启动清理残留进程时告诉用户刚才收掉的是什么，不影响执行本身。
    pub label: Option<String>,
    /// 脚本的工作目录。多文件卡片必须设成卡片包所在目录——包里的入口脚本会
    /// 相对自己的位置去找同级文件（例如 `megacube.sh` 要 cd 到自己目录再调
    /// `controller.py`），不设的话它们一个都找不到。
    pub cwd: Option<PathBuf>,
}

const DEFAULT_TIMEOUT: Duration = Duration::from_secs(21600);
const DEFAULT_IDLE_TIMEOUT: Duration = Duration::from_secs(300);

/// 执行脚本，实时把每行输出喂给 on_progress；非 0 退出码返回 Err。成功时
/// 返回完整输出行列表，供调用方在需要时解析脚本输出里的约定标记（比如
/// enable 脚本用 `LMM_ENDPOINT_URL=` 这行告诉 LMM 对外调用地址）。
///
/// `need_root=true` 时用 pkexec 提权执行（会弹图形密码框），仅用于真正需要
/// root 的操作（目前只有 install_engine）；其它脚本以当前用户身份运行。
///
/// 注意：pkexec 出于安全考虑，不会把调用方进程的环境变量透传给提权后执行的
/// 目标程序，所以 `LMM_*` 这些自定义变量必须显式通过 `pkexec env VAR=val ...
/// 脚本` 的写法传进去，否则脚本里读到的全是空值。
///
/// 子进程的 stdout/stderr 接到一个伪终端（pty）上，而不是普通管道：像
/// huggingface_hub CLI、docker pull 这类工具，检测到自己接的不是真终端时，
/// 经常会整段关闭/攒批进度条输出。接 pty 能让子进程以为自己接的就是真终端，
/// 进度条按其正常的实时刷新行为工作。
///
/// 取消/超时的处理分两种情况：
/// - `need_root=false`：脚本以当前用户身份跑，对其发信号没有权限问题，但脚本
///   自己可能又 fork 了孙子进程——用新建进程组的方式让整棵进程树自成一组，
///   取消时对整个组发信号（killpg）。
/// - `need_root=true`：脚本认证通过后是以 root 身份跑的，非 root 的应用进程
///   即使是它的"父进程"也没有权限对它发 kill 信号——这不是能绕过的 bug，是
///   操作系统的权限边界。真正能生效的办法是让 root 身份运行的脚本自己配合：
///   给它传一个 `LMM_CANCEL_FILE` 路径，脚本内部长时间操作改成后台跑 + 轮询
///   这个文件是否出现，出现了就自己把子进程杀掉、退出。这里只负责按需生成
///   这个标记文件并在用户点取消时创建它；脚本那边要不要配合轮询，看脚本自己
///   有没有实现。
///
/// 超时判定用两个独立的计时器，而不是单一的固定墙钟上限：
/// - `idle_timeout`：连续这么久完全没有任何新输出才判定为真的卡死。下载引擎
///   镜像/模型权重这种操作，总耗时取决于文件大小和网络带宽，只要进度条还在
///   刷新就不该被杀掉。
/// - `timeout`：一个很宽松的绝对安全上限，纯粹防止极端情况无限跑下去。
pub fn run_script(
    script_path: &Path,
    env_vars: &HashMap<String, String>,
    mut opts: RunScriptOptions<'_>,
) -> Result<Vec<String>, ScriptError> {
    let need_root = opts.need_root;
    let timeout = opts.timeout.unwrap_or(DEFAULT_TIMEOUT);
    let idle_timeout = opts.idle_timeout.unwrap_or(DEFAULT_IDLE_TIMEOUT);

    let cancel_file: Option<PathBuf> = if need_root {
        let path = tempfile::Builder::new()
            .prefix("lmm-cancel-")
            .tempfile()
            .map_err(|e| ScriptError::new(format!("创建取消标记文件失败：{e}")))?
            .into_temp_path();
        // 只是占个路径；出现即代表"请取消"，先确保不存在。
        let path: PathBuf = path.to_path_buf();
        let _ = std::fs::remove_file(&path);
        Some(path)
    } else {
        None
    };

    let (master, mut cmd) = build_command(
        script_path,
        env_vars,
        need_root,
        cancel_file.as_deref(),
        opts.cwd.as_deref(),
    )?;

    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            if need_root {
                return Err(ScriptError::new(
                    "未找到 pkexec，无法以管理员权限执行该脚本，请确认系统已安装 \
                     policykit（大多数 Ubuntu 桌面版默认自带）",
                ));
            }
            return Err(ScriptError::new(format!("无法执行脚本：{e}")));
        }
        Err(e) => return Err(ScriptError::new(format!("无法执行脚本：{e}"))),
    };

    let mut master_file = File::from(master);
    let child_pid = child.id() as i32;

    // 登记这次派生的进程。应用要是被强杀/覆盖安装、没走到下面的销号逻辑，
    // 下次启动时 core::orphans 会照着这份登记把残留进程收掉——否则它会一直
    // 占着 huggingface_hub 的文件锁，把用户下一次点的下载堵死在那儿。
    let mut run_guard = super::orphans::RunGuard::register(
        child_pid,
        need_root,
        opts.label.as_deref().unwrap_or("后台脚本"),
    );

    let stop_process_tree = |sig: Signal| {
        if need_root {
            // 大概率没权限，能生效算捡到，失败也别让取消操作本身崩掉。
            let _ = kill(Pid::from_raw(child_pid), sig);
        } else {
            let _ = killpg(Pid::from_raw(child_pid), sig);
        }
    };

    // need_root=true 时大概率没权限强杀，只能指望脚本自己配合轮询取消文件，
    // 所以宽限得给久一点；need_root=false 时对整个进程组有完整权限，SIGTERM
    // 不管用就直接 SIGKILL，没必要死等太久让用户觉得"点了取消没反应"。
    let cancel_grace = if need_root {
        Duration::from_secs(20)
    } else {
        Duration::from_secs(3)
    };

    let mut output_lines: Vec<String> = Vec::new();
    let mut buf = String::new();
    // 跨 read 的 UTF-8 残尾（见 decode_utf8_stream）。
    let mut pending: Vec<u8> = Vec::new();
    let deadline = Instant::now() + timeout;
    let mut idle_deadline = Instant::now() + idle_timeout;
    let mut cancel_requested = false;
    let mut cancel_deadline = Instant::now();
    let mut cancel_escalated = false;
    let mut timed_out = false;
    let mut timeout_reason = String::new();
    // 既不是取消也不是超时的主动中止（目前只有"脚本把回显打开了"这一种）。
    // 不复用 timed_out 是因为那条路径会把原因包进"脚本执行超时：…"，而这里
    // 发生的事跟超时毫无关系，那样说会把排查引向完全错误的方向。
    let mut aborted_reason: Option<String> = None;
    // 等用户输入期间：脚本阻塞在 read 上，一声不吭是正常的，得把 idle 计时器
    // 停掉，否则用户还在打字脚本就被当成卡死杀了。
    let mut prompt_deadline: Option<Instant> = None;
    let mut scanned_lines = 0usize;

    'outer: loop {
        if !cancel_requested && opts.control.cancelled() {
            cancel_requested = true;
            cancel_deadline = Instant::now() + cancel_grace;
            if let Some(cf) = &cancel_file {
                let _ = File::create(cf);
            }
            stop_process_tree(Signal::SIGTERM);
        }
        if cancel_requested && !cancel_escalated && Instant::now() >= cancel_deadline {
            if need_root {
                // 大概率是脚本没配合轮询取消文件（没有权限强杀它），等了一段
                // 宽限时间还没退出就不再死等——把"已取消"报给界面，脚本可能
                // 仍在后台跑完，这是权限边界下能做到的最好结果。
                break;
            }
            cancel_escalated = true;
            cancel_deadline = Instant::now() + Duration::from_secs(3);
            stop_process_tree(Signal::SIGKILL);
        } else if cancel_escalated && Instant::now() >= cancel_deadline {
            break;
        }

        // 脚本请求了输入：等界面把答案送回来，期间不判 idle 超时。
        if let Some(until) = prompt_deadline {
            // 有待答提示就一定注册过应答槽——prompt_deadline 只在解析到提示行时
            // 设置，而那一步要求 control.prompt 存在。这里仍然不用 expect：
            // 脚本执行线程 panic 的后果是卡片被锁死到超时（spawn_op 的
            // catch_unwind 兜得住线程，兜不住用户那张卡）。槽真没了就当没人能
            // 回答，撤掉待答状态让 idle 超时按正常路径收场。
            let Some(slot) = opts.control.prompt.as_ref() else {
                prompt_deadline = None;
                continue;
            };
            if slot.is_aborted() {
                cancel_requested = true;
                cancel_deadline = Instant::now() + cancel_grace;
                if let Some(cf) = &cancel_file {
                    let _ = File::create(cf);
                }
                stop_process_tree(Signal::SIGTERM);
                prompt_deadline = None;
            } else if let Some(answer) = slot.take() {
                // **写答案之前重新确认回显确实还关着。**
                //
                // build_command 里的 fail-closed 只守住了 spawn 那一刻，而卡片
                // 脚本来自网络：中途一句 `stty echo` 就能把回显打开，此后我们
                // 写回 pty 的每一个字符都会被原样回显回来，顺着 output_lines →
                // RunLog → 界面日志一路流出去，还会进错误摘要的 tail 20 行和
                // humanize 的 120 字符窗口，并且持久化落盘。本文件开头
                // 「password……且不落日志」那句承诺，不重校验就只在 spawn 时刻成立。
                //
                // 发现不对就**不作答**并中止：继续写等于把密码送出去；喂个空串
                // 也不行（见 aborting_a_prompt_cancels_the_run… 那条测试的理由，
                // 空的从机 IP 会让脚本拿着它继续往下跑）。
                if !echo_is_off(&master_file) {
                    aborted_reason = Some(format!(
                        "{ECHO_REOPENED_ABORT}（脚本在请求输入前改动了终端设置）：\
                         再作答会把密码原样回显进日志，已拒绝作答并中止执行"
                    ));
                    stop_process_tree(Signal::SIGKILL);
                    break 'outer;
                }
                // 写回 pty 主端 = 送进脚本的 stdin。答案里的换行要去掉，否则
                // 脚本的一次 read 只拿到前半截，剩下的会被当成下一次输入。
                let one_line = answer.replace(['\n', '\r'], "");
                let _ = master_file.write_all(format!("{one_line}\n").as_bytes());
                let _ = master_file.flush();
                prompt_deadline = None;
                idle_deadline = Instant::now() + idle_timeout;
            } else if Instant::now() >= until {
                timed_out = true;
                timeout_reason = format!(
                    "等待用户输入超过 {} 秒（脚本请求了交互输入但一直没有作答）",
                    PROMPT_TIMEOUT.as_secs()
                );
                stop_process_tree(Signal::SIGKILL);
                break;
            }
        }

        if prompt_deadline.is_none() && Instant::now() >= idle_deadline {
            timed_out = true;
            timeout_reason = format!(
                "连续 {} 秒没有任何新输出，判定为卡死",
                idle_timeout.as_secs()
            );
            stop_process_tree(Signal::SIGKILL);
            break;
        }
        if Instant::now() >= deadline {
            timed_out = true;
            timeout_reason = format!("运行时间超过安全上限 {} 秒", timeout.as_secs());
            stop_process_tree(Signal::SIGKILL);
            break;
        }

        let readable = poll_readable(&master_file, Duration::from_millis(300));
        if readable {
            let mut chunk = [0u8; 4096];
            match master_file.read(&mut chunk) {
                Ok(0) => {
                    if try_wait_exited(&mut child) {
                        break 'outer;
                    }
                    continue;
                }
                Ok(n) => {
                    idle_deadline = Instant::now() + idle_timeout;
                    buf.push_str(&decode_utf8_stream(&mut pending, &chunk[..n]));
                    buf = drain_lines(buf, &mut output_lines, &mut opts.on_progress);
                    // pty 是回显的：我们写回去的答案会原样再读回来一遍，所以
                    // 扫描只看新增的行，不会把自己写的东西当成新的提示。
                    while scanned_lines < output_lines.len() {
                        let line = output_lines[scanned_lines].clone();
                        scanned_lines += 1;
                        if !is_prompt_line(&line) {
                            continue;
                        }
                        if opts.control.prompt.is_none() {
                            // 没有应答槽却要交互，等下去只会挂到超时。直接说
                            // 清楚是哪一步不支持，比让用户盯十分钟进度条强。
                            timed_out = true;
                            timeout_reason =
                                "脚本请求了交互输入，但这次调用没有开启输入通道".to_string();
                            stop_process_tree(Signal::SIGKILL);
                            break 'outer;
                        }
                        prompt_deadline = Some(Instant::now() + PROMPT_TIMEOUT);
                    }
                }
                Err(_) => {
                    // 子进程关闭了 slave 端（Linux pty 读到底会报 EIO）。
                    if try_wait_exited(&mut child) {
                        break 'outer;
                    }
                }
            }
        } else if try_wait_exited(&mut child) {
            break 'outer;
        }
    }

    if cancel_requested || timed_out || aborted_reason.is_some() {
        // 观察到的"进程已退出"只是顶层那一个 PID；如果是被信号强行打断的
        // （而不是自己跑完退出），进程组里可能还有孙子进程正在收尾（比如脚本
        // 自己 trap 里的清理逻辑）。这里给一点点宽限时间继续从 pty 读，避免
        // 刚检测到顶层进程消失就立刻结束，把这些收尾输出硬生生截断。
        let mut grace_until = Instant::now() + Duration::from_secs(2);
        while Instant::now() < grace_until {
            if !poll_readable(&master_file, Duration::from_millis(200)) {
                continue;
            }
            let mut chunk = [0u8; 4096];
            match master_file.read(&mut chunk) {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    buf.push_str(&decode_utf8_stream(&mut pending, &chunk[..n]));
                    buf = drain_lines(buf, &mut output_lines, &mut opts.on_progress);
                    grace_until = Instant::now() + Duration::from_secs(2);
                }
            }
        }
    }

    // 收尾：pending 里还剩的残尾只能是被截断的半个字符（脚本被强杀时常见）。
    // 到这里没有"下一块"了，按损坏处理吐出去，总比整段丢掉强。
    if !pending.is_empty() {
        buf.push_str(&String::from_utf8_lossy(&pending));
        buf = drain_lines(buf, &mut output_lines, &mut opts.on_progress);
    }

    if !buf.trim().is_empty() {
        output_lines.push(buf.clone());
        if let Some(cb) = opts.on_progress.as_deref_mut() {
            cb(ScriptProgress::line(buf));
        }
    }

    // 走到这里还活着的 root 进程，登记必须留下来。
    //
    // need_root 的脚本经 pkexec 之后是 root 进程，上面那些 SIGTERM/SIGKILL 对它
    // 多半是 EPERM（见 run_script 文档里那段「操作系统的权限边界」），取消/超时
    // 返回时它大概率还在后台把 docker pull 跑完。而 `run_guard` 是本函数的**栈上
    // 局部变量**：函数一返回就析构、把登记抹掉，下次启动的 cleanup_on_startup 连
    // "有这么个东西"都不知道，连它唯一能做的那件事（在 skipped_root 里报一笔）
    // 都做不了。
    // 用户点完取消，几十 GB 的 docker pull 继续在后台拉，重启应用零痕迹。
    //
    // 留着记录不会造成误杀：need_root 的记录在 cleanup_on_startup 里只上报不动
    // 手，而且上报前还要再比一次 starttime——进程真的已经结束、或者那个 pid 被
    // 系统回收给别人了，那条记录会被自然丢弃，不会误报也不会越留越多。
    if need_root && !try_wait_exited(&mut child) {
        run_guard.keep();
    }

    if cancel_requested {
        if let Some(cb) = opts.on_progress.as_deref_mut() {
            cb(ScriptProgress::cancelled());
        }
        reap_child(child);
        cleanup_temp_files(&cancel_file, script_path);
        // 取消必须跟真正的失败一样中断调用链，不能让后续步骤（写数据库状态）
        // 继续跑下去——这是 v1.0.6 修过的一个大 bug：取消后误标记"已完成"。
        return Err(ScriptError::new("已取消"));
    }
    if let Some(reason) = aborted_reason {
        reap_child(child);
        cleanup_temp_files(&cancel_file, script_path);
        // 刻意不附带输出尾巴：这条路径上刚发生的事就是"脚本想把输入骗进日志"，
        // 再把它打印的那些行拼进错误信息，等于替它把话带出去。
        return Err(ScriptError::new(reason));
    }
    if timed_out {
        reap_child(child);
        cleanup_temp_files(&cancel_file, script_path);
        return Err(ScriptError::new(format!(
            "脚本执行超时：{}（{timeout_reason}）",
            script_path
                .file_name()
                .and_then(|s| s.to_str())
                .unwrap_or("")
        )));
    }

    let status = child
        .wait()
        .map_err(|e| ScriptError::new(format!("等待脚本退出失败：{e}")))?;
    cleanup_temp_files(&cancel_file, script_path);

    let code = status.code().unwrap_or(-1);
    if code != 0 {
        let last_line = output_lines.iter().rev().find(|l| !l.trim().is_empty());
        let summary = last_line
            .map(|l| humanize_script_message(l))
            .unwrap_or_else(|| "未知错误".to_string());
        let tail_start = output_lines.len().saturating_sub(20);
        let tail = output_lines[tail_start..].join("\n");
        return Err(ScriptError::new(format!(
            "脚本执行失败（退出码 {code}）：{summary}\n{tail}"
        )));
    }

    if let Some(cb) = opts.on_progress.as_deref_mut() {
        cb(ScriptProgress::done());
    }
    Ok(output_lines)
}

fn cleanup_temp_files(cancel_file: &Option<PathBuf>, script_path: &Path) {
    if let Some(cf) = cancel_file {
        let _ = std::fs::remove_file(cf);
    }
    if script_path.exists() && script_path.parent() == Some(std::env::temp_dir().as_path()) {
        let _ = std::fs::remove_file(script_path);
    }
}

fn try_wait_exited(child: &mut std::process::Child) -> bool {
    matches!(child.try_wait(), Ok(Some(_)))
}

fn poll_readable(file: &File, timeout: Duration) -> bool {
    use nix::poll::{PollFd, PollFlags, PollTimeout, poll};
    let mut fds = [PollFd::new(file.as_fd(), PollFlags::POLLIN)];
    let timeout_ms: u16 = timeout.as_millis().min(u16::MAX as u128) as u16;
    match poll(&mut fds, PollTimeout::from(timeout_ms)) {
        Ok(n) if n > 0 => fds[0]
            .revents()
            .map(|r| r.contains(PollFlags::POLLIN))
            .unwrap_or(false),
        _ => false,
    }
}

/// 伪终端此刻的回显是不是确实还关着。
///
/// 读的是**主端**的句柄：Linux 对 pty 主端做 TCGETS/TCSETS 时，内核会转去操作
/// 从端的 termios（`tty_mode_ioctl` 见到 `PTY_TYPE_MASTER` 就换成 `tty->link`），
/// 所以这里拿到的就是脚本那一侧的设置。非这样不可——从端的句柄在 spawn 时已经
/// 交给子进程了，我们手上只剩主端。
///
/// 读不到就当成"不安全"。跟 build_command 里"关不掉就不跑"是同一个取向：
/// 防护措施 fail-open 比没有防护更糟，没有防护时大家至少知道日志里可能有密码。
fn echo_is_off(tty: &File) -> bool {
    use nix::sys::termios::LocalFlags;
    let Ok(attrs) = nix::sys::termios::tcgetattr(tty) else {
        return false;
    };
    !attrs.local_flags.contains(LocalFlags::ECHO) && !attrs.local_flags.contains(LocalFlags::ECHONL)
}

fn build_command(
    script_path: &Path,
    env_vars: &HashMap<String, String>,
    need_root: bool,
    cancel_file: Option<&Path>,
    cwd: Option<&Path>,
) -> Result<(OwnedFd, Command), ScriptError> {
    let pty = nix::pty::openpty(None, None)
        .map_err(|e| ScriptError::new(format!("创建伪终端失败：{e}")))?;
    let (master, slave) = (pty.master, pty.slave);

    // 关掉 pty 的输入回显。
    //
    // 平时没人往脚本的 stdin 里写东西，回显与否无所谓；但脚本用 LMM_PROMPT
    // 请求输入时，我们写回去的答案会被 pty 原样回显成一行输出，跟日志一起送
    // 到界面上——密码就这么漏进日志了。脚本需要 pty 是为了让 docker pull 这类
    // 工具认为自己接的是终端（进度条才实时刷新），跟输入回显没有关系。
    // **关不掉就不跑。** 这里原来是 `if let Ok` + `let _`，两头都把失败悄悄咽掉——
    // 而失败的后果正是这段代码存在的全部理由：本次运行带着回显跑，用户填进表单的
    // 密码（从机 root 密码、HF token）原样进任务日志，界面上还没有任何提示。
    //
    // 防护措施 fail-open 比没有防护更糟：没有防护时大家知道日志里可能有密码，
    // fail-open 会让所有人以为已经挡住了。
    //
    // 这里只守得住**这一刻**。脚本跑起来之后自己一句 `stty echo` 就能把回显打
    // 回去，所以 run_script 在每次写答案前还要再 `tcgetattr` 确认一次
    // （`echo_is_off`）——两道一起才算真的挡住。
    //
    // 顺带清掉 ECHONL：它单独控制「换行也回显」，ECHO 关了而它开着时，答案末尾
    // 那个换行仍会回显——不泄密，但会在日志里留下空行，让人怀疑是不是没关干净。
    let mut attrs = nix::sys::termios::tcgetattr(&slave).map_err(|e| {
        ScriptError::new(format!(
            "无法读取伪终端属性，拒绝在可能回显的终端上执行脚本：{e}"
        ))
    })?;
    attrs
        .local_flags
        .remove(nix::sys::termios::LocalFlags::ECHO);
    attrs
        .local_flags
        .remove(nix::sys::termios::LocalFlags::ECHONL);
    nix::sys::termios::tcsetattr(&slave, nix::sys::termios::SetArg::TCSANOW, &attrs).map_err(
        |e| {
            ScriptError::new(format!(
                "无法关闭伪终端回显，拒绝执行脚本（否则输入的密码会进日志）：{e}"
            ))
        },
    )?;

    let mut cmd = if need_root {
        let mut env_vars = env_vars.clone();
        if let Some(cf) = cancel_file {
            env_vars.insert(
                "LMM_CANCEL_FILE".to_string(),
                cf.to_string_lossy().to_string(),
            );
        }
        // **提权这条路上不许带秘密。**
        //
        // pkexec 不透传环境变量，所以下面把整份 env 拼进 argv —— 而 argv 是公开的：
        // 提权脚本跑着的那几分钟到几小时里（install_engine 要 docker pull 几十 GB），
        // 本机**任何**用户都能从 `ps` 或 `/proc/<pid>/cmdline` 读到每一个值。
        //
        // 剥掉它是零代价的：需要 root 的只有 install_engine（配 NVIDIA runtime），
        // 而用 token 的只有 download_model（下私有权重），两边从不重合——六张卡
        // 逐个核对过。真哪天有卡片两样都要，也该用 fd 或 root-only 文件传，
        // 而不是把它摆进 argv。
        for key in SECRET_ENV_KEYS {
            env_vars.remove(key);
        }
        let mut c = Command::new("pkexec");
        c.arg("--keep-cwd").arg("env");
        for (k, v) in &env_vars {
            c.arg(format!("{k}={v}"));
        }
        c.arg(script_path);
        c
    } else {
        let mut c = Command::new(script_path);
        c.envs(env_vars.iter().map(|(k, v)| (k.clone(), v.clone())));
        // 脚本自己可能又 fork 出孙子进程（比如调 curl/hf 下载）——新建一个
        // 进程组（pgid = 子进程自身 pid），取消时对整个组发信号才能带走它们。
        c.process_group(0);
        c
    };

    if let Some(dir) = cwd {
        cmd.current_dir(dir);
    }

    let stdin_fd: OwnedFd = slave
        .try_clone()
        .map_err(|e| ScriptError::new(format!("复制伪终端句柄失败：{e}")))?;
    let stdout_fd: OwnedFd = slave
        .try_clone()
        .map_err(|e| ScriptError::new(format!("复制伪终端句柄失败：{e}")))?;
    cmd.stdin(Stdio::from(stdin_fd));
    cmd.stdout(Stdio::from(stdout_fd));
    cmd.stderr(Stdio::from(slave));

    Ok((master, cmd))
}

#[cfg(test)]
mod tests {
    use super::*;

    const BSLASH: char = '\\';
    use std::sync::atomic::AtomicBool;
    use std::thread;

    /// 所有会真的起脚本的测试都要串行跑，并且各自指向独立的数据目录。
    ///
    /// 起一个脚本 = 往 orphans 的 running-scripts.json 里登记一笔，而那个文件的
    /// 位置来自进程级的 HOME 环境变量。测试是多线程并发跑的，不串起来
    /// 就会出现"这个测试起的脚本把记录写进了另一个测试刚设好的临时目录"——
    /// orphans 那边断言登记表内容的用例就会莫名其妙多出一条「后台脚本」。
    /// 用的是跟 orphans 测试同一把锁，两边才真的互斥。
    fn with_isolated_data_dir<T>(f: impl FnOnce() -> T) -> T {
        // TempHome 自带那把跨模块的锁，并在析构时把 HOME 还原回去——
        // 不还原会让 tempdir 删除后 HOME 悬空，污染同进程里读 HOME 的其他测试。
        let _home = super::super::config::TempHome::new();
        f()
    }

    fn write_script(body: &str) -> PathBuf {
        let mut f = tempfile::Builder::new()
            .prefix("lmm-test-script-")
            .suffix(".sh")
            .tempfile()
            .unwrap();
        f.write_all(body.as_bytes()).unwrap();
        let path = f.into_temp_path().keep().unwrap();
        let mut perms = std::fs::metadata(&path).unwrap().permissions();
        perms.set_mode(perms.mode() | 0o111);
        std::fs::set_permissions(&path, perms).unwrap();
        path
    }

    #[test]
    fn runs_successful_script_and_captures_output() {
        let path = write_script("#!/bin/sh\necho hello\necho world\nexit 0\n");
        let lines = with_isolated_data_dir(|| {
            run_script(&path, &HashMap::new(), RunScriptOptions::default())
        })
        .unwrap();
        assert_eq!(lines, vec!["hello".to_string(), "world".to_string()]);
    }

    #[test]
    fn nonzero_exit_is_an_error() {
        let path = write_script("#!/bin/sh\necho failing\nexit 7\n");
        let err = with_isolated_data_dir(|| {
            run_script(&path, &HashMap::new(), RunScriptOptions::default())
        })
        .unwrap_err();
        assert!(err.to_string().contains("退出码 7"));
    }

    #[test]
    fn env_vars_are_passed_through() {
        let path = write_script("#!/bin/sh\necho \"$LMM_MODEL_NAME\"\n");
        let mut env = HashMap::new();
        env.insert("LMM_MODEL_NAME".to_string(), "qwen38".to_string());
        let lines = with_isolated_data_dir(|| run_script(&path, &env, RunScriptOptions::default()))
            .unwrap();
        assert_eq!(lines, vec!["qwen38".to_string()]);
    }

    /// 提权路径上必须剥掉秘密——pkexec 不透传 env，只能拼进 argv，而 argv
    /// 对本机所有用户可见。
    ///
    /// 这里钉的是**清单本身**而不是去跑一次 pkexec：真跑要弹授权框，在 CI 和
    /// 开发机上都跑不了。清单是那段剥离逻辑的唯一输入，钉住它就钉住了行为；
    /// 将来谁往 common_env 里加了新的凭据，这条会提醒他也加进来。
    #[test]
    fn secrets_are_stripped_before_privileged_exec() {
        assert!(
            SECRET_ENV_KEYS.contains(&"LMM_HF_TOKEN"),
            "HF token 必须在提权前被剥掉：install_engine 以 root 跑 docker pull 的几小时里，\
             argv 对本机任何用户可见"
        );
    }

    #[test]
    fn cancellation_raises_instead_of_returning_ok() {
        // 关键回归测试：取消必须报错，不能让调用方误以为脚本成功跑完
        // （v1.0.6 修过的那个"取消后误标记已完成"的 bug，Rust 版不能重蹈覆辙）。
        let path =
            write_script("#!/bin/sh\nfor i in $(seq 1 100); do echo tick $i; sleep 0.1; done\n");
        let flag = Arc::new(AtomicBool::new(false));
        let flag_clone = flag.clone();
        thread::spawn(move || {
            thread::sleep(Duration::from_millis(300));
            flag_clone.store(true, Ordering::SeqCst);
        });
        let opts = RunScriptOptions {
            control: RunControl {
                cancel: Some(flag),
                ..Default::default()
            },
            ..Default::default()
        };
        let err = with_isolated_data_dir(|| run_script(&path, &HashMap::new(), opts)).unwrap_err();
        assert_eq!(err.to_string(), "已取消");
    }

    /// 脚本请求一次输入 -> 界面作答 -> 答案经 pty 写回脚本的 stdin。
    ///
    /// 这条链路有三处都可能悄悄断掉且不报错：标记行没被认出来、答案没写进
    /// pty、脚本读到的是带换行的半截。所以直接用真脚本跑一遍端到端。
    #[test]
    fn prompt_marker_collects_an_answer_and_feeds_it_to_stdin() {
        let path = write_script(
            "#!/bin/sh\necho 'LMM_PROMPT=text|请输入从机 IP'\nread ip\necho \"got=[$ip]\"\n",
        );
        let slot = Arc::new(PromptSlot::default());
        let slot_ui = slot.clone();
        thread::spawn(move || {
            thread::sleep(Duration::from_millis(400));
            slot_ui.answer("192.0.2.30");
        });
        let opts = RunScriptOptions {
            control: RunControl {
                prompt: Some(slot),
                ..Default::default()
            },
            ..Default::default()
        };
        let lines = with_isolated_data_dir(|| run_script(&path, &HashMap::new(), opts)).unwrap();
        assert!(
            lines.iter().any(|l| l.contains("got=[192.0.2.30]")),
            "脚本没收到答案，实际输出：{lines:?}"
        );
    }

    /// 用户在输入框上点取消 = 放弃这次操作。绝不能拿空串喂给脚本让它继续跑
    /// ——双机部署里那就是拿一个空的从机 IP 去连。
    #[test]
    fn aborting_a_prompt_cancels_the_run_instead_of_answering_empty() {
        let path = write_script(
            "#!/bin/sh\necho 'LMM_PROMPT=text|请输入从机 IP'\nread ip\necho \"got=[$ip]\"\n",
        );
        let slot = Arc::new(PromptSlot::default());
        let slot_ui = slot.clone();
        thread::spawn(move || {
            thread::sleep(Duration::from_millis(400));
            slot_ui.abort();
        });
        let opts = RunScriptOptions {
            control: RunControl {
                prompt: Some(slot),
                ..Default::default()
            },
            ..Default::default()
        };
        let err = with_isolated_data_dir(|| run_script(&path, &HashMap::new(), opts)).unwrap_err();
        assert_eq!(err.to_string(), "已取消");
    }

    /// 脚本要交互、调用方却没开输入通道时，必须立刻说清楚，而不是让脚本阻塞
    /// 在 read 上一直挂到 idle 超时（默认 300 秒），界面上只看到一个转圈。
    #[test]
    fn prompt_without_a_channel_fails_fast_with_a_clear_reason() {
        let path = write_script("#!/bin/sh\necho 'LMM_PROMPT=text|要点什么'\nread x\necho done\n");
        let err = with_isolated_data_dir(|| {
            run_script(&path, &HashMap::new(), RunScriptOptions::default())
        })
        .unwrap_err();
        assert!(
            err.to_string().contains("没有开启输入通道"),
            "错误信息没说清原因：{err}"
        );
    }

    /// 密码类输入绝不能出现在日志里。
    ///
    /// pty 默认是回显的：我们把答案写回主端，它会原样再冒出来一行，跟普通输出
    /// 一样流进任务日志和 output_lines。所以关掉了 ECHO——这条测试就是钉住它。
    #[test]
    fn answers_are_not_echoed_back_into_the_log() {
        let path = write_script(
            "#!/bin/sh\necho 'LMM_PROMPT=password|输入密码'\nread pw\necho '收到，已处理'\n",
        );
        let slot = Arc::new(PromptSlot::default());
        let slot_ui = slot.clone();
        thread::spawn(move || {
            thread::sleep(Duration::from_millis(400));
            slot_ui.answer("hunter2-super-secret");
        });
        let opts = RunScriptOptions {
            control: RunControl {
                prompt: Some(slot),
                ..Default::default()
            },
            ..Default::default()
        };
        let lines = with_isolated_data_dir(|| run_script(&path, &HashMap::new(), opts)).unwrap();
        assert!(
            !lines.iter().any(|l| l.contains("hunter2")),
            "密码被 pty 回显进了日志：{lines:?}"
        );
        assert!(
            lines.iter().any(|l| l.contains("收到，已处理")),
            "脚本没跑完：{lines:?}"
        );
    }

    /// 脚本中途把回显打开（`stty echo`）之后再要密码：必须拒绝作答并中止。
    ///
    /// 关回显只发生在 spawn 那一刻，而卡片脚本来自网络。只要中途一句 `stty
    /// echo`，我们写回 pty 的答案就会被原样回显回来，流进 output_lines → 任务
    /// 日志 → 错误摘要 → 落盘。上面那条 answers_are_not_echoed_back… 钉的是
    /// spawn 时刻，这条钉的是**写答案的那一刻**，缺哪条都留着完整的泄密路径。
    #[test]
    fn reopened_echo_refuses_to_answer_instead_of_leaking_the_password() {
        let path = write_script(
            "#!/bin/sh\nstty echo\necho 'LMM_PROMPT=password|输入密码'\nread pw\necho \"got=[$pw]\"\n",
        );
        let slot = Arc::new(PromptSlot::default());
        let slot_ui = slot.clone();
        thread::spawn(move || {
            thread::sleep(Duration::from_millis(400));
            slot_ui.answer("hunter2-super-secret");
        });
        let opts = RunScriptOptions {
            control: RunControl {
                prompt: Some(slot),
                ..Default::default()
            },
            ..Default::default()
        };
        let err = with_isolated_data_dir(|| run_script(&path, &HashMap::new(), opts)).unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains(ECHO_REOPENED_ABORT),
            "中止原因没说清是回显被打开：{msg}"
        );
        assert!(!msg.contains("hunter2"), "密码进了错误信息：{msg}");
        assert!(
            !msg.contains("超时"),
            "这不是超时，报成超时会把排查引到完全错误的方向：{msg}"
        );
    }

    /// 表单式输入：一次问齐多项，脚本读回一行 JSON。
    ///
    /// 双机部署要问从机 IP、用户名、密码、本机密码——一问一答会连弹四个框，
    /// 而那个流程的目标恰恰是"填一次就不用管了"。
    #[test]
    fn form_prompt_collects_several_fields_in_one_answer() {
        let path = write_script(
            "#!/bin/sh\necho 'LMM_PROMPT_FORM={\"title\":\"双机部署\",\"fields\":[{\"id\":\"ip\"},{\"id\":\"pw\"}]}'\nread ans\necho \"got=$ans\"\n",
        );
        let slot = Arc::new(PromptSlot::default());
        let slot_ui = slot.clone();
        thread::spawn(move || {
            thread::sleep(Duration::from_millis(400));
            slot_ui.answer("{\"ip\":\"192.0.2.42\",\"pw\":\"fake-pw\"}");
        });
        let opts = RunScriptOptions {
            control: RunControl {
                prompt: Some(slot),
                ..Default::default()
            },
            ..Default::default()
        };
        let lines = with_isolated_data_dir(|| run_script(&path, &HashMap::new(), opts)).unwrap();
        assert!(
            lines
                .iter()
                .any(|l| l.contains("192.0.2.42") && l.contains("fake-pw")),
            "表单答案没完整送到脚本：{lines:?}"
        );
    }

    /// 两个前缀不能互相误判：LMM_PROMPT_FORM= 中间是下划线不是等号，
    /// 不以 LMM_PROMPT= 开头。
    #[test]
    fn both_prompt_markers_are_recognised_and_distinct() {
        assert!(is_prompt_line("LMM_PROMPT=text|要点什么"));
        assert!(is_prompt_line("LMM_PROMPT_FORM={}"));
        assert!(!is_prompt_line("LMM_PROMPTX=1"));
        assert!(!is_prompt_line("普通日志"));
    }

    /// 一个永远不会等于任何真实内容的哈希。
    ///
    /// 下面三条 fetch 测试全部走本地路径分支，不联网也不碰真实商店——
    /// `fetch_script_content` 接受本地文件输入，这让来源策略的三条腿都能直测。
    const ZERO_SHA: &str = "0000000000000000000000000000000000000000000000000000000000000000";

    /// 哈希对不上必须**中止**。
    ///
    /// 这条失败路径以前一个测试都没有（20 个 script_runner 测试没一个碰 fetch），
    /// 而它是"脚本内容被改过"唯一的拦截点——真出问题时，它是最后一道关。
    #[test]
    fn hash_mismatch_aborts_the_fetch() {
        let path = write_script("#!/bin/sh\necho hello\n");
        let url = path.to_string_lossy().to_string();
        let policy = FetchPolicy {
            allow_local_path: true,
            ..Default::default()
        };
        let real = sha256_hex(&std::fs::read_to_string(&path).unwrap());

        // 先证明哈希正确时这条路径确实走得通，否则下面的断言可能只是"根本没走到"
        assert!(fetch_script_content(&url, Some(&real), policy).is_ok());
        // 商店用大写十六进制登记也算匹配（eq_ignore_ascii_case）
        assert!(fetch_script_content(&url, Some(&real.to_uppercase()), policy).is_ok());

        let err = fetch_script_content(&url, Some(ZERO_SHA), policy).unwrap_err();
        assert!(
            err.to_string().contains("完整性校验失败"),
            "哈希不匹配没被拦下：{err}"
        );
        let _ = std::fs::remove_file(&path);
    }

    /// 要以 root 执行的脚本，没有登记哈希就不许跑。
    ///
    /// 这条以前是"没下发就跳过校验，保持兼容"——兼容的是一个并不存在的老后台，
    /// 换来的是 install_engine 可能不声不响地以 root 跑一份没人校验过的内容。
    #[test]
    fn root_scripts_refuse_to_run_without_a_registered_hash() {
        let path = write_script("#!/bin/sh\necho hi\n");
        let url = path.to_string_lossy().to_string();
        let root = FetchPolicy {
            may_run_as_root: true,
            allow_local_path: true,
        };
        let err = fetch_script_content(&url, None, root).unwrap_err();
        assert!(
            err.to_string().contains(UNTRUSTED_SOURCE_PREFIX),
            "缺哈希的提权脚本被放行了：{err}"
        );
        // 带上哈希就该照常放行，别把真实设备上的安装路径改死
        let real = sha256_hex(&std::fs::read_to_string(&path).unwrap());
        assert!(fetch_script_content(&url, Some(&real), root).is_ok());

        // 不提权的脚本维持原状：没哈希仍然可以取（老记录只有 URL 没有哈希，
        // 卸载这类操作要靠它才回收得掉磁盘）
        let lenient = FetchPolicy {
            allow_local_path: true,
            ..Default::default()
        };
        assert!(fetch_script_content(&url, None, lenient).is_ok());
        let _ = std::fs::remove_file(&path);
    }

    /// 明文 http 和"真实商店给了本地路径"都必须当场拒绝。
    ///
    /// 两条都在发请求/读文件**之前**判掉，所以这个测试既不联网也碰不到 /etc。
    #[test]
    fn plain_http_and_unexpected_local_paths_are_refused() {
        let strict = FetchPolicy::default();
        for bad in [
            // 明文链路上内容和校验值会被一起改掉，校验等于没做
            "http://example.invalid/install_engine.sh",
            // 卡片来自网络：URL 能写成任意本地路径 = 读本机任意文件再当脚本跑
            "/etc/passwd",
            "../../../etc/shadow",
            "file:///etc/passwd",
        ] {
            let err = fetch_script_content(bad, Some(ZERO_SHA), strict).unwrap_err();
            assert!(
                err.to_string().contains(UNTRUSTED_SOURCE_PREFIX),
                "不可信来源被放行了：{bad}（{err}）"
            );
        }
    }

    /// 卡片内容来自网络。路径直接拿来拼接，就等于
    /// 把「往任意位置写文件」的能力交给了卡片作者或中间人，所以这里必须拒绝。
    #[test]
    fn rejects_paths_that_escape_the_card_dir() {
        for bad in [
            "/etc/passwd",
            "../../.ssh/authorized_keys",
            "a/../../b",
            "..",
            "",
            "   ",
            "C:/Windows/system32/x.dll",
        ] {
            assert!(
                sanitize_relative_path(bad).is_err(),
                "危险路径没被拦下：{bad:?}"
            );
        }
    }

    #[test]
    fn accepts_normal_relative_paths() {
        for (input, want) in [
            ("megacube.sh", "megacube.sh"),
            ("runtime/patch/ple_layer.py", "runtime/patch/ple_layer.py"),
            ("./controller.py", "controller.py"),
        ] {
            let got = sanitize_relative_path(input).unwrap();
            assert_eq!(got, std::path::PathBuf::from(want), "输入 {input:?}");
        }
        // 反斜杠按分隔符处理，交付方在 Windows 上打包也不会出问题
        assert_eq!(
            sanitize_relative_path(
                r"runtime@patch@x.py"
                    .replace('@', &BSLASH.to_string())
                    .as_str()
            )
            .unwrap(),
            std::path::PathBuf::from("runtime/patch/x.py")
        );
    }

    #[test]
    fn materialize_assets_writes_tree_and_keeps_other_files() {
        let dir = tempfile::tempdir().unwrap();
        // 卡片包自己产生的运行期状态，绝不能被覆盖式落地抹掉
        std::fs::write(dir.path().join("config.json"), "user-state").unwrap();

        let mut files = std::collections::BTreeMap::new();
        files.insert(
            "megacube.sh".to_string(),
            "#!/bin/sh
echo hi
"
            .to_string(),
        );
        files.insert("runtime/patch/a.py".to_string(), "print(1)".to_string());
        materialize_assets(dir.path(), &files).unwrap();

        assert_eq!(
            std::fs::read_to_string(dir.path().join("runtime/patch/a.py")).unwrap(),
            "print(1)"
        );
        assert_eq!(
            std::fs::read_to_string(dir.path().join("config.json")).unwrap(),
            "user-state",
            "落地附属文件时把用户的运行期状态抹掉了"
        );
        // .sh 要可执行，否则入口脚本调不起来
        let mode = std::fs::metadata(dir.path().join("megacube.sh"))
            .unwrap()
            .permissions()
            .mode();
        assert_ne!(mode & 0o111, 0, "megacube.sh 没有可执行位");
    }

    #[test]
    fn strips_terminal_control_sequences_from_log_lines() {
        // docker pull 在 pty 下的典型输出：光标上移 + 清行 + 真正的内容。
        // 转义字符直接以字节形式写进脚本，不走 shell 的  解析——各家 sh
        // 对八进制转义的支持并不一致，那样测的就成了 shell 而不是我们的代码。
        let path = write_script(
            "#!/bin/sh
printf '[1A[2K7e1f8fb7d4ba: Pulling fs layer
'
",
        );
        let lines = with_isolated_data_dir(|| {
            run_script(&path, &HashMap::new(), RunScriptOptions::default())
        })
        .unwrap();
        assert_eq!(lines, vec!["7e1f8fb7d4ba: Pulling fs layer".to_string()]);
    }

    #[test]
    fn strip_ansi_keeps_normal_text_untouched() {
        assert_eq!(
            strip_ansi("LMM_PROGRESS_BYTES=123/456"),
            "LMM_PROGRESS_BYTES=123/456"
        );
        assert_eq!(strip_ansi("下载中 42%"), "下载中 42%");
        // 半截的转义序列（读到一半被切断）不能把后面的正文一起吃掉
        assert_eq!(strip_ansi("a[31mred[0m b"), "ared b");
    }

    #[test]
    fn idle_timeout_kills_hung_script() {
        let path = write_script("#!/bin/sh\necho start\nsleep 30\necho done\n");
        let opts = RunScriptOptions {
            idle_timeout: Some(Duration::from_millis(500)),
            ..Default::default()
        };
        let err = with_isolated_data_dir(|| run_script(&path, &HashMap::new(), opts)).unwrap_err();
        assert!(err.to_string().contains("判定为卡死"));
    }

    /// 4096 边界切在一个中文字中间：两半必须拼回同一个字，而不是两个替换符。
    #[test]
    fn utf8_split_across_reads_is_reassembled() {
        let text = "下载进度：完成";
        let bytes = text.as_bytes();
        // 在第一个中文字的三个字节中间切一刀。
        let cut = 1;
        let mut pending = Vec::new();
        let mut out = String::new();
        out.push_str(&decode_utf8_stream(&mut pending, &bytes[..cut]));
        // 第一块只有半个字符，一个字都不该吐出来。
        assert_eq!(out, "");
        out.push_str(&decode_utf8_stream(&mut pending, &bytes[cut..]));
        assert_eq!(out, text);
        assert!(pending.is_empty());
    }

    /// 逐字节喂：每一步最多留 3 字节残尾，全部喂完必须一字不差。
    #[test]
    fn utf8_byte_by_byte_feed_loses_nothing() {
        let text = "检查 vLLM 引擎是否已安装…（32.3GB）";
        let mut pending = Vec::new();
        let mut out = String::new();
        for b in text.as_bytes() {
            out.push_str(&decode_utf8_stream(&mut pending, &[*b]));
            assert!(pending.len() <= 3, "残尾不该超过一个字符的长度");
        }
        assert_eq!(out, text);
        assert!(pending.is_empty());
    }

    /// 真的坏字节要立刻顶掉。留在 pending 里的话，它后面所有输出都解不出来。
    #[test]
    fn utf8_real_bad_bytes_do_not_block_the_rest() {
        let mut pending = Vec::new();
        let mut chunk = vec![0xffu8, 0xfe];
        chunk.extend_from_slice("正常".as_bytes());
        let out = decode_utf8_stream(&mut pending, &chunk);
        assert!(
            out.ends_with("正常"),
            "坏字节后面的内容必须照常出来：{out:?}"
        );
        assert!(pending.is_empty());
    }
}
