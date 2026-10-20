//! 模型商店的**专属运行日志落盘**。
//!
//! # 在这之前：日志一个字都不落盘
//!
//! 一次长操作的输出走的是 pty → `strip_ansi` → `script_runner` 的 `output_lines`
//! → `service::RunLog::lines` → 长轮询。全程只在内存里，而且：
//!
//! - `RUN_RETENTION` 是 600 秒，`sweep_old_runs` 到点把整条运行从 HashMap 里删掉，
//!   日志**永久消失**；应用重启同样清零。
//! - 于是「昨天那次下载为什么失败」这个问题在设备上**无法回答**。后端报错只带最后
//!   20 行 tail，前端只留 500 行，真正的根因往往在那之前几千行。
//!
//! # 为什么不是 `~/.pinvou3/logs/model-store/`
//!
//! 落点是 **`~/.pinvou3/model-store/logs/`**，跟着 `config.rs` 已经定下的决策走：
//! 那个文件开头写着「模型商店数据跟着品悟走 `~/.pinvou3/model-store/`，卸载品悟时
//! 一目了然」，`store_root` / `config_dir` / `data_dir` 和 hf_token 的回退文件全挂在
//! 这下面，理由是「卸载时一起走」。日志换到 `~/.pinvou3/logs/` 下就让「卸载模型商店
//! 要清哪些」这件事重新变成两处答案——而 `config.rs` 花了一整段说明它只能有一处。
//!
//! # 品悟没有统一日志模块，所以这里是第五套手写诊断
//!
//! `tauri-plugin-log` 只在 `cfg!(debug_assertions)` 下注册（`lib.rs`），release 包里
//! 四百多处 `log::*` 全部丢弃。真正落盘的是四套各自手写的诊断
//! （`platform/startup.rs`、`features/sessions/diagnostics.rs`、
//! `features/codex_acp/diagnostics.rs`、`features/memory/llm_review.rs`），**没有**
//! `logs_dir()` / `write_log()` 之类的抽象可以复用。
//!
//! 本模块因此照 `codex_acp/diagnostics.rs` 的形状写（体积上限 + 脱敏 + 0600），
//! 刻意**不**顺手去抽一个全局日志层：那要动 `platform/`，是跨功能的地基改动，
//! 跟这次「模型商店的运行日志」不是一件事，塞进同一个改动里谁也审不动。
//!
//! # 🔴 脱敏是这个模块存在的前提
//!
//! 宿主对**所有卡片的所有操作**统一注入 `LMM_HF_TOKEN`（`lifecycle::common_env`），
//! 脚本侧到处 re-export 成 `HF_TOKEN` 再 `-e HF_TOKEN` 传进 docker。而卡片**来自
//! 网络、可能被投毒**：脚本里一句 `set -x` 就会把 token 原样打进输出，而输出在这之前
//! 是不落盘的、看过就没了。一旦落盘，同一句 `set -x` 就变成一个**持久**的凭证泄漏。
//!
//! 所以这里有两道，分工明确：
//!
//! - [`sanitize_for_disk`]：落盘用。**定向**脱敏——登记过的字面量机密、`hf_*` /
//!   `sk-*` / `ghp_*` 这类前缀、`Bearer <x>`、URL 里的 userinfo 和 query 参数、
//!   `--token=<x>` 这类命令行取值。
//! - [`sanitize_for_export`]：导出/上报用。在上面之外**再过一道**
//!   `credential_store::redact_secret`（全仓通用的那把尺子）以及内网 IP / 主机名。
//!
//! 为什么分两道，而不是落盘时就上 `redact_secret`：它的通用判据是「长度 ≥ 24 且
//! 同时含字母和数字」，而 `/home/l28756/.pinvou3/model-store/models` 正好命中——
//! 落盘时就上这把尺子，日志里**每一条路径**都会变成 `[REDACTED]`，一份读不懂的日志
//! 比没有日志更糟（守卫见本文件测试 `broad_redaction_would_eat_ordinary_paths`）。
//! 真正会离开这台机器的是"导出"，那一步该按最严的尺子来，而它也确实按最严的来了。
//!
//! # 绝不新增的东西
//!
//! **不记录我们写回 pty 的内容。** 双机卡会问本机密码和从机密码，答案是直接写进 pty
//! 主端的；不被回显是靠两道 fail-closed 校验兜住的（`script_runner::build_command`
//! spawn 前关 ECHO、关不掉就拒绝执行；写每个答案前再确认一次 ECHO 仍关着，被打开就
//! SIGKILL）。这个模块只挂在 `RunLog::push` 之后——也就是「已经 `strip_ansi` 过、
//! 已经切好行」的那条既有出口上，一个新的读取点都不开。

use std::io::{Read, Write};
use std::path::{Path, PathBuf};

use parking_lot::Mutex;

use super::config;

/// 单次运行的日志文件上限。
///
/// 一次 168GB 的下载能打出几万行。**必须有上限**：这个目录跟用户的权重同盘，
/// 日志把盘写满的后果是下载失败，而且是以一种完全看不懂的方式失败。
const MAX_RUN_LOG_BYTES: u64 = 4 * 1024 * 1024;

// A final UTF-8 line and truncation notice can slightly exceed the write cap.
const MAX_EXPORT_LOG_BYTES: u64 = MAX_RUN_LOG_BYTES + (MAX_LINE_CHARS as u64 * 4) + 1024;

/// 单行上限（字符）。脚本可以打出一行几 MB 的 base64。
const MAX_LINE_CHARS: usize = 4_000;

/// 目录里最多留几次运行的日志。够覆盖「昨天那次为什么失败」，又不会无限涨。
const MAX_RUN_FILES: usize = 20;

/// 登记的字面量机密最多留几条。宿主目前只会登记一条（HF Token）；给点余量，
/// 但必须有上限——这张表是全进程常驻的。
const MAX_LITERAL_SECRETS: usize = 16;

/// 太短的字面量不登记：三四个字符的"机密"会把正常文本打成筛子，
/// 而一份被打成筛子的日志等于没有日志。
const MIN_LITERAL_SECRET_LEN: usize = 8;

const REDACTED: &str = "[REDACTED]";

/// 已知的**字面量**机密。写进这里的值会在落盘前被逐字替换掉。
///
/// 为什么要有字面量这一档，而不是只靠前缀匹配：HF Token 的形状会变（`hf_` 前缀是
/// 现在的约定，不是规范），而宿主**恰好知道自己注进去的那个值是什么**。知道确切
/// 值时按确切值匹配，是这里能给出的最强保证。
static LITERAL_SECRETS: Mutex<Vec<String>> = Mutex::new(Vec::new());

/// 日志目录：`~/.pinvou3/model-store/logs/`。
pub fn logs_dir() -> PathBuf {
    config::data_dir().join("logs")
}

/// 登记一个字面量机密。宿主在注入凭证时调一次即可（重复调用是幂等的）。
pub fn register_secret(value: &str) {
    let value = value.trim();
    if value.len() < MIN_LITERAL_SECRET_LEN {
        return;
    }
    let mut list = LITERAL_SECRETS.lock();
    if list.iter().any(|known| known == value) || list.len() >= MAX_LITERAL_SECRETS {
        return;
    }
    list.push(value.to_string());
}

/// 把登记过的字面量机密逐字换掉。
fn mask_literal_secrets(line: &str) -> String {
    let list = LITERAL_SECRETS.lock();
    let mut out = line.to_string();
    for secret in list.iter() {
        if out.contains(secret.as_str()) {
            out = out.replace(secret.as_str(), REDACTED);
        }
    }
    out
}

/// 一眼就是凭证的前缀。命中就把整个词换掉。
const SECRET_PREFIXES: [&str; 8] = [
    "hf_",
    "sk-",
    "ghp_",
    "gho_",
    "github_pat_",
    "xoxb-",
    "AKIA",
    "bce-v3/",
];

/// URL 的 query 里叫这些名字的参数，取值一律抹掉。
///
/// `credential_store::redact_secret` 按**空白**分词，所以
/// `https://host/f?token=abc123` 对它是一个整词、而这个整词不长得像机密——
/// 嵌在 query 里的 token 它一个都抓不到。这一档就是补这个洞。
const SECRET_QUERY_KEYS: [&str; 10] = [
    "token",
    "access_token",
    "accesstoken",
    "password",
    "passwd",
    "pwd",
    "secret",
    "apikey",
    "api_key",
    "signature",
];

/// 命令行上叫这些名字的开关，紧跟的取值一律抹掉（`--token abc` 和 `--token=abc`）。
///
/// **刻意不收 `-p`**（`mysql -p<密码>` 那个）：它跟 `mkdir -p` 撞名，收进来会把脚本
/// 里每一个 `mkdir -p <目录>` 的目录名抹掉。单字母开关的歧义没法靠这张表消除，而
/// 抹掉路径的代价（日志读不懂）比这一档能挡住的风险大。
const SECRET_FLAGS: [&str; 7] = [
    "--token",
    "--password",
    "--passwd",
    "--secret",
    "--api-key",
    "--apikey",
    "--header",
];

/// 这些环境变量名后面跟的取值一律抹掉（脚本 `set -x` / `env` 时会整行打出来）。
const SECRET_ENV_KEYS: [&str; 7] = [
    "HF_TOKEN",
    "LMM_HF_TOKEN",
    "HUGGINGFACE_TOKEN",
    "HUGGING_FACE_HUB_TOKEN",
    "PASSWORD",
    "LMM_ANSWER",
    "API_KEY",
];

/// 这个词是不是该整体抹掉。
fn word_is_secret(word: &str) -> bool {
    let trimmed =
        word.trim_matches(|c: char| matches!(c, '"' | '\'' | ',' | ';' | ')' | '(' | '`'));
    if trimmed.len() < MIN_LITERAL_SECRET_LEN {
        return false;
    }
    SECRET_PREFIXES
        .iter()
        .any(|prefix| trimmed.starts_with(prefix))
}

/// `KEY=VALUE` 里的 KEY 是不是机密名。大小写不敏感：脚本里两种写法都见过。
fn key_is_secret(key: &str) -> bool {
    let key = key.trim_matches(|c: char| matches!(c, '"' | '\'' | '$' | '{' | '}'));
    SECRET_ENV_KEYS
        .iter()
        .any(|known| key.eq_ignore_ascii_case(known))
}

/// 把 URL 里的 userinfo（`https://user:pass@host`）和机密 query 参数抹掉。
///
/// 只动**取值**，保留参数名和主机路径——`?token=[REDACTED]` 比整条 URL 变成
/// `[REDACTED]` 有用得多：排查的第一个问题往往是"它到底在连哪儿"。
fn redact_url_secrets(word: &str) -> String {
    if !word.contains("://") {
        return word.to_string();
    }
    let mut out = String::with_capacity(word.len());
    // userinfo：`scheme://` 之后、第一个 `@` 之前的 `user:pass`。密码才是机密，
    // 但用户名也可能是账号，一起抹掉更省心。
    let (scheme, rest) = match word.split_once("://") {
        Some(pair) => pair,
        None => return word.to_string(),
    };
    out.push_str(scheme);
    out.push_str("://");
    let authority_end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let (authority, tail) = rest.split_at(authority_end);
    match authority.rsplit_once('@') {
        Some((_, host)) => {
            out.push_str(REDACTED);
            out.push('@');
            out.push_str(host);
        }
        None => out.push_str(authority),
    }
    // query：逐个参数看名字。
    match tail.split_once('?') {
        None => out.push_str(tail),
        Some((path, query)) => {
            out.push_str(path);
            out.push('?');
            let mut first = true;
            for pair in query.split('&') {
                if !first {
                    out.push('&');
                }
                first = false;
                match pair.split_once('=') {
                    Some((key, _))
                        if SECRET_QUERY_KEYS
                            .iter()
                            .any(|known| key.eq_ignore_ascii_case(known)) =>
                    {
                        out.push_str(key);
                        out.push('=');
                        out.push_str(REDACTED);
                    }
                    _ => out.push_str(pair),
                }
            }
        }
    }
    out
}

/// 定向脱敏：落盘走这一道。
///
/// 按空白分词逐词处理，然后把词拼回去。**刻意不做全量启发式**，理由见模块文档。
///
/// # 行首缩进要留着
///
/// 分词重拼会把所有连续空白压成一个空格。词**内部**的空白压掉无所谓，但**行首缩进**
/// 不行：Python traceback 是这份日志里最有价值的形状，而它全靠缩进才读得出调用层级——
///
/// ```text
///   File "/opt/x.py", line 3, in <module>
///     raise ValueError(...)
/// ```
///
/// 压平之后这两行看起来是同一层，一份读不顺的日志会让人放弃往下看。所以行首那段
/// 原样搬过去。
pub fn sanitize_for_disk(line: &str) -> String {
    let masked = mask_literal_secrets(line);
    let mut out = String::with_capacity(masked.len());
    let indent: String = masked
        .chars()
        .take_while(|c| c.is_whitespace())
        // 制表符在 `clamp_line` 里会变成空格，这里先按同样的宽度铺开，免得缩进
        // 在两道处理之间对不上
        .map(|c| if c == '\t' { ' ' } else { c })
        .take(MAX_LINE_CHARS / 8)
        .collect();
    out.push_str(&indent);
    // 上一个词要求把下一个词抹掉（`--token abc` / `Bearer abc` 这种分开写的）。
    let mut redact_next = false;
    let mut first = true;
    for word in masked.split_whitespace() {
        if !first {
            out.push(' ');
        }
        first = false;
        if redact_next {
            out.push_str(REDACTED);
            redact_next = false;
            continue;
        }
        if word_is_secret(word) {
            out.push_str(REDACTED);
        } else if let Some((key, _)) = word.split_once('=') {
            // `KEY=VALUE`：只看 KEY。命令行开关（`--token=abc`）和环境变量
            // （`HF_TOKEN=abc`）共用这一支。
            let flag_secret = SECRET_FLAGS
                .iter()
                .any(|known| key.eq_ignore_ascii_case(known));
            if flag_secret || key_is_secret(key) {
                out.push_str(key);
                out.push('=');
                out.push_str(REDACTED);
            } else {
                out.push_str(&redact_url_secrets(word));
            }
        } else {
            out.push_str(&redact_url_secrets(word));
        }
        let bare = word.trim_matches(|c: char| matches!(c, '"' | '\'' | ',' | ';' | ':'));
        redact_next = bare.eq_ignore_ascii_case("bearer")
            || SECRET_FLAGS.contains(&bare)
            || SECRET_ENV_KEYS.contains(&bare);
    }
    clamp_line(&out)
}

/// 导出/上报走这一道：定向脱敏 + 全仓通用尺子 + 内网地址。
///
/// `AGENTS.md` 的禁写清单里内网 IP 和主机名跟凭证并列。落盘时不抹它们是刻意的
/// （「这个模型在哪台从机上起不来」全靠它），但日志一旦要离开这台机器，那条理由
/// 就不成立了。
pub fn sanitize_for_export(line: &str) -> String {
    let targeted = sanitize_for_disk(line);
    let broad = crate::platform::credential_store::redact_secret(&targeted);
    redact_private_hosts(&broad)
}

/// 隐藏 IPv4 / IPv6。**不区分公网私网**：公网 IP 一样是可定位信息，而区分
/// 需要一张会过期的网段表——那张表漏一条就是一次泄漏。
fn redact_private_hosts(line: &str) -> String {
    let mut out = String::with_capacity(line.len());
    let mut first = true;
    for word in line.split_whitespace() {
        if !first {
            out.push(' ');
        }
        first = false;
        if contains_ip_address(word) {
            out.push_str("[HOST]");
        } else {
            out.push_str(word);
        }
    }
    out
}

/// 地址可能出现在 URL、端口、参数或中文标点中，不能只修剪整词两端。
/// 找出 ASCII 地址候选后用标准库验证；含地址的整个词按导出的保守策略隐藏。
/// IPv6 zone 在 `%` 处结束，地址部分仍可识别。不识别/猜测主机名。
fn contains_ip_address(word: &str) -> bool {
    let bytes = word.as_bytes();
    for ipv6 in [false, true] {
        let candidate_byte = |byte: u8| {
            if ipv6 {
                byte.is_ascii_hexdigit() || matches!(byte, b':' | b'.')
            } else {
                byte.is_ascii_digit() || byte == b'.'
            }
        };
        let mut cursor = 0;
        while cursor < bytes.len() {
            if !candidate_byte(bytes[cursor]) {
                cursor += 1;
                continue;
            }
            let start = cursor;
            while cursor < bytes.len() && candidate_byte(bytes[cursor]) {
                cursor += 1;
            }
            // ASCII spans always begin/end at UTF-8 boundaries, including when
            // non-ASCII log text surrounds them. A final full stop is prose.
            let candidate = word[start..cursor].trim_end_matches('.');
            if (!ipv6 && candidate.parse::<std::net::Ipv4Addr>().is_ok())
                || (ipv6 && candidate.parse::<std::net::Ipv6Addr>().is_ok())
            {
                return true;
            }
        }
    }
    false
}

/// 单行截断 + 去掉换行/回车/制表（一行日志必须是一行）。
fn clamp_line(line: &str) -> String {
    let mut out: String = line
        .chars()
        .map(|c| {
            if matches!(c, '\r' | '\n' | '\t') {
                ' '
            } else {
                c
            }
        })
        .take(MAX_LINE_CHARS)
        .collect();
    if line.chars().count() > MAX_LINE_CHARS {
        out.push_str(" …（本行已截断）");
    }
    out
}

/// 把 run_id 收成单一、安全的文件名分量。
///
/// run_id 是服务端自己发的 base64url，本来就安全；仍然收一道是因为**这是一个会
/// 拼进路径的外来标识**，而「安全来自调用方」这种约定迟早会被下一个调用方打破。
fn safe_stem(run_id: &str) -> String {
    let stem: String = run_id
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_'))
        .take(64)
        .collect();
    if stem.is_empty() {
        "unknown".to_string()
    } else {
        stem
    }
}

/// 一次运行的日志文件。
pub struct RunLogFile {
    path: PathBuf,
    state: Mutex<FileState>,
}

struct FileState {
    /// 写到上限之后置成 `None`，此后这条运行不再落盘。
    file: Option<std::fs::File>,
    written: u64,
}

impl RunLogFile {
    /// 为一次运行开一个日志文件。**开不出来就返回 `None`，绝不让操作本身失败**：
    /// 日志是诊断手段，不是功能；为了记日志而让用户下不了模型是本末倒置。
    pub fn open(run_id: &str, label: &str) -> Option<Self> {
        let dir = logs_dir();
        if std::fs::create_dir_all(&dir).is_err() {
            return None;
        }
        prune_old_logs(&dir);
        let path = dir.join(format!("{}.log", safe_stem(run_id)));
        // 0600，且每次打开都重新收紧权限——早先版本或外部工具留下的松权限文件
        // 会一直松着（见 `filesystem::open_private_append_file` 的说明）。
        let mut file = crate::platform::filesystem::open_private_append_file(&path).ok()?;
        let written = file.metadata().map(|m| m.len()).unwrap_or(0);
        let header = format!(
            "==== {} | {} | run={} | pid={} ====\n",
            chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true),
            clamp_line(&sanitize_for_disk(label)),
            safe_stem(run_id),
            std::process::id(),
        );
        let _ = file.write_all(header.as_bytes());
        Some(Self {
            path,
            state: Mutex::new(FileState {
                file: Some(file),
                written: written + header.len() as u64,
            }),
        })
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    /// 落一行。入参是**已经 `strip_ansi` 过、已经切好行**的那条既有出口上的行
    /// （`service::RunLog::push`）。
    pub fn append(&self, line: &str) {
        let text = sanitize_for_disk(line);
        if text.trim().is_empty() {
            return;
        }
        let stamped = format!(
            "{} {}\n",
            chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true),
            text
        );
        let mut state = self.state.lock();
        if state.written >= MAX_RUN_LOG_BYTES {
            // 上限到了：留一句话说明为什么突然没了，然后彻底闭嘴。
            if let Some(mut file) = state.file.take() {
                let _ = file.write_all(
                    format!(
                        "…… 本次运行的日志已达 {} MiB 上限，后续行不再记录 ……\n",
                        MAX_RUN_LOG_BYTES / 1024 / 1024
                    )
                    .as_bytes(),
                );
                let _ = file.flush();
            }
            return;
        }
        let Some(file) = state.file.as_mut() else {
            return;
        };
        // 先把这一行写完、刷完，再动计数器：`state.written` 和 `state.file` 是同一个
        // `FileState` 的两个字段，借用检查器不接受「文件的可变借用还活着的时候改计数」。
        let wrote = file.write_all(stamped.as_bytes()).is_ok();
        let _ = file.flush();
        if wrote {
            state.written += stamped.len() as u64;
        }
    }

    /// 写结局那一行并收尾。
    pub fn finish(&self, outcome: &str) {
        self.append(&format!("==== 结束：{outcome} ===="));
        let mut state = self.state.lock();
        if let Some(mut file) = state.file.take() {
            let _ = file.flush();
        }
    }
}

/// 只留最近 `MAX_RUN_FILES` 份。按 mtime 排，拿不到 mtime 的排最后（宁可留着
/// 也不要因为读不到元数据就删掉一份可能有用的日志）。
fn prune_old_logs(dir: &Path) {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    let mut files: Vec<(std::time::SystemTime, PathBuf)> = entries
        .flatten()
        .filter(|entry| {
            entry
                .path()
                .extension()
                .is_some_and(|extension| extension == "log")
        })
        .map(|entry| {
            let modified = entry
                .metadata()
                .and_then(|m| m.modified())
                .unwrap_or(std::time::SystemTime::UNIX_EPOCH);
            (modified, entry.path())
        })
        .collect();
    if files.len() < MAX_RUN_FILES {
        return;
    }
    files.sort_by(|a, b| b.0.cmp(&a.0));
    for (_, path) in files.into_iter().skip(MAX_RUN_FILES - 1) {
        let _ = std::fs::remove_file(path);
    }
}

/// 导出一次运行的日志：逐行过 [`sanitize_for_export`]，写到 `<run>.export.log`。
///
/// 为什么不直接把落盘那份给出去：落盘那份刻意保留了路径和内网地址（不保留就没法
/// 诊断），而导出意味着这份内容会离开这台机器。
pub fn export_run_log(run_id: &str) -> Result<(PathBuf, String), String> {
    let source = logs_dir().join(format!("{}.log", safe_stem(run_id)));
    let mut content = String::new();
    std::fs::File::open(&source)
        .and_then(|file| {
            file.take(MAX_EXPORT_LOG_BYTES + 1)
                .read_to_string(&mut content)
        })
        .map_err(|e| format!("读不到这次运行的日志（{}）：{e}", source.display()))?;
    if content.len() as u64 > MAX_EXPORT_LOG_BYTES {
        return Err("运行日志超过安全导出上限，未导出或复制；请在本机检查日志".into());
    }
    let mut snapshot = String::with_capacity(content.len());
    for line in content.lines() {
        snapshot.push_str(&sanitize_for_export(line));
        snapshot.push('\n');
    }
    let target = logs_dir().join(format!("{}.export.log", safe_stem(run_id)));
    // 导出文件同样 0600：它比原始日志更敏感（更容易被顺手发出去），不该更宽松。
    // 先清空再追加——导出是"这一刻的快照"，追加到上一次的尾巴上只会让人拿错。
    let _ = std::fs::remove_file(&target);
    let mut file = crate::platform::filesystem::open_private_append_file(&target)
        .map_err(|e| format!("创建导出文件失败：{e}"))?;
    file.write_all(snapshot.as_bytes())
        .map_err(|e| format!("写导出文件失败：{e}"))?;
    file.flush().map_err(|e| format!("写导出文件失败：{e}"))?;
    // Clipboard consumers receive these exact sanitized bytes, never a reread
    // of the export path and never the less-redacted live frontend buffer.
    Ok((target, snapshot))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 落盘路径上四种形状的机密都不许留下原值。
    ///
    /// 这条对齐 `codex_acp/diagnostics.rs` 和 `sessions/diagnostics.rs` 里同类断言的
    /// 风格：**断言落盘内容不含原值**，而不是断言"调用了脱敏函数"。
    #[test]
    fn disk_sanitizer_removes_every_secret_shape() {
        let cases = [
            "+ export HF_TOKEN=hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345",
            "curl -H 'Authorization: Bearer hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345' https://hf-mirror.com/api",
            "sk-proj-0123456789abcdefghijklmnop",
            "hf download --token hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345 Qwen/Qwen3-8B",
            "GET https://hf-mirror.com/file?token=hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345&x=1",
            "https://someone:s3cr3tpassword@internal.example.com/pkg.tar",
            "LMM_HF_TOKEN=hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345",
        ];
        for case in cases {
            let cleaned = sanitize_for_disk(case);
            assert!(
                !cleaned.contains("hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345"),
                "HF Token 漏了：{cleaned}"
            );
            assert!(
                !cleaned.contains("sk-proj-0123456789abcdefghijklmnop"),
                "sk- 形状漏了：{cleaned}"
            );
            assert!(
                !cleaned.contains("s3cr3tpassword"),
                "URL userinfo 里的密码漏了：{cleaned}"
            );
            assert!(cleaned.contains(REDACTED), "整行一处都没脱敏：{cleaned}");
        }
    }

    /// 登记过的字面量机密按**确切值**抹掉——哪怕它一点也不像凭证。
    ///
    /// HF Token 的 `hf_` 前缀是当前约定不是规范，而宿主恰好知道自己注进去的是什么。
    #[test]
    fn registered_literal_secrets_are_masked_even_without_a_telltale_prefix() {
        register_secret("plain-looking-value-98765");
        let cleaned = sanitize_for_disk("echo plain-looking-value-98765 > /tmp/x");
        assert!(!cleaned.contains("plain-looking-value-98765"), "{cleaned}");
        assert!(cleaned.contains(REDACTED), "{cleaned}");
        // 太短的不登记：会把正常文本打成筛子。
        register_secret("abc");
        assert_eq!(sanitize_for_disk("abc def"), "abc def");
    }

    /// 落盘时**不能**上全量启发式。
    ///
    /// 这条钉的是本模块分两道脱敏的理由：`credential_store::redact_secret` 的通用
    /// 判据是「≥24 字符且同时含字母和数字」，普通路径正好命中。落盘就用它的话，
    /// 日志里每一条路径都变成 `[REDACTED]`——一份读不懂的日志比没有日志更糟。
    #[test]
    fn broad_redaction_would_eat_ordinary_paths() {
        let path = "/home/l28756/.pinvou3/model-store/models/Qwen3-8B";
        // 先证明"全量那把尺子确实会吃掉它"，这条理由才立得住。
        assert!(
            crate::platform::credential_store::redact_secret(path).contains(REDACTED),
            "前提变了：全量脱敏不再吃普通路径，这个模块该合并成一道"
        );
        // 落盘这道原样保留。
        assert_eq!(sanitize_for_disk(path), path);
        // 导出那道按最严的来——路径被抹掉是**可以接受**的代价，因为那份内容要离开本机。
        assert!(sanitize_for_export(path).contains(REDACTED));
    }

    /// 导出还要抹掉地址。`AGENTS.md` 的禁写清单里内网 IP / 主机名跟凭证并列。
    #[test]
    fn export_sanitizer_also_removes_addresses() {
        let exported = sanitize_for_export("ssh 192.168.31.42 失败：连接超时");
        assert!(!exported.contains("192.168.31.42"), "{exported}");
        assert!(exported.contains("[HOST]"), "{exported}");
        // 落盘那份要留着——「这个模型在哪台从机上起不来」全靠它。
        assert!(sanitize_for_disk("ssh 192.168.31.42 失败").contains("192.168.31.42"));
        // 版本号不能误伤
        assert!(sanitize_for_export("vllm/vllm-openai:v0.8.5").contains("v0.8.5"));
    }

    #[test]
    fn export_host_filter_covers_urls_ports_flags_and_ipv6() {
        for address in [
            "192.168.31.42:18080",
            "http://192.168.31.42:18080/v1/models",
            "--worker=192.168.31.42",
            "连接192.168.31.42失败",
            "[2001:db8::42]:18080",
            "http://[2001:db8::42]:18080/v1",
            "--host=fe80::42%eth0",
            "::1",
            "::ffff:192.168.31.42",
        ] {
            assert_eq!(redact_private_hosts(address), "[HOST]", "{address}");
            assert!(
                !contains_ip_address(&sanitize_for_export(address)),
                "{address}"
            );
        }
    }

    #[test]
    fn export_host_filter_keeps_non_addresses() {
        for text in [
            "v0.8.5",
            "256.168.31.42",
            "dead:beef",
            "12:34:56",
            "中文正文",
        ] {
            assert_eq!(redact_private_hosts(text), text);
        }
        assert_eq!(
            redact_private_hosts("worker=192.168.31.42:80 连接失败 code=70"),
            "[HOST] 连接失败 code=70"
        );
    }

    /// `mkdir -p <目录>` 的目录名不许被抹掉。
    ///
    /// 这条钉的是 `SECRET_FLAGS` 里**没有** `-p`。单字母开关歧义太大，收进来的代价是
    /// 日志里每一个 `mkdir -p` 后面的路径变成 `[REDACTED]`。
    #[test]
    fn single_letter_flags_do_not_eat_the_next_word() {
        assert_eq!(
            sanitize_for_disk("mkdir -p /home/user/.pinvou3/model-store/staging"),
            "mkdir -p /home/user/.pinvou3/model-store/staging"
        );
    }

    /// 行首缩进必须留着。
    ///
    /// 脱敏是按空白分词重拼的，默认会把缩进一起压平。而 Python traceback 是这份日志里
    /// 最有价值的形状，它全靠缩进才读得出调用层级；压平之后两层看起来一样深，
    /// 一份读不顺的日志会让人放弃往下看。
    #[test]
    fn leading_indentation_survives_the_sanitizer() {
        assert_eq!(
            sanitize_for_disk("    raise ValueError(\"weights checksum mismatch\")"),
            "    raise ValueError(\"weights checksum mismatch\")"
        );
        assert_eq!(
            sanitize_for_disk("  File \"/opt/x.py\", line 3, in <module>"),
            "  File \"/opt/x.py\", line 3, in <module>"
        );
        // 缩进里的机密照样要抹掉——留缩进不等于放过内容
        assert!(
            !sanitize_for_disk("    HF_TOKEN=hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345")
                .contains("hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345")
        );
    }

    #[test]
    fn long_lines_are_clamped_and_single_line() {
        let cleaned =
            sanitize_for_disk(&format!("head\r\ntail {}", "x".repeat(MAX_LINE_CHARS * 2)));
        assert!(!cleaned.contains('\n'));
        assert!(!cleaned.contains('\r'));
        assert!(cleaned.contains("已截断"));
    }

    /// **文件级**断言：落到磁盘上的那份不含任何一个机密的原值。
    ///
    /// 这是这个模块允许存在的前提条件。断言的对象刻意是**文件内容**而不是脱敏函数的
    /// 返回值——中间任何一环（忘了调 sanitize、header 里漏了 label、结局行拼错）
    /// 都会被这条抓住，而只断函数返回值的测试全都抓不到。
    #[test]
    fn nothing_secret_ever_reaches_the_file_on_disk() {
        let _home = config::TempHome::new();
        register_secret("literal-secret-value-42");
        let file = RunLogFile::open("run-TeStId_1", "下载 hf_HeaderTokenAbCdEf0123456789").unwrap();
        file.append("+ export HF_TOKEN=hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345");
        file.append("curl -H 'Authorization: Bearer hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345' https://hf-mirror.com");
        file.append("openai key sk-proj-0123456789abcdefghijklmnop");
        file.append(
            "wget https://mirror.example.com/x.tar?token=hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345",
        );
        file.append("echo literal-secret-value-42");
        file.finish("失败：磁盘空间不足");

        let content = std::fs::read_to_string(file.path()).unwrap();
        for secret in [
            "hf_AbCdEfGhIjKlMnOpQrStUvWxYz012345",
            "hf_HeaderTokenAbCdEf0123456789",
            "sk-proj-0123456789abcdefghijklmnop",
            "literal-secret-value-42",
        ] {
            assert!(!content.contains(secret), "{secret} 落盘了：\n{content}");
        }
        // 反面：正常内容必须还在，否则这份日志没有诊断价值
        assert!(content.contains("hf-mirror.com"), "{content}");
        assert!(content.contains("磁盘空间不足"), "{content}");
        assert!(content.contains(REDACTED), "{content}");

        // 落点必须是 `~/.pinvou3/model-store/logs/`（跟着 config.rs 的既有决策走）
        let expected = config::data_dir().join("logs").join("run-TeStId_1.log");
        assert_eq!(file.path(), expected.as_path());

        // 0600：这份文件里有主机名、路径、有时还有用户名
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(file.path()).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode, 0o600, "日志文件权限不是 0600");
        }
    }

    /// 单次运行的体积上限。日志目录跟用户的权重同盘，写满盘的后果是下载失败，
    /// 而且是以一种完全看不懂的方式失败。
    #[test]
    fn a_single_run_log_stops_growing_at_the_cap() {
        let _home = config::TempHome::new();
        let file = RunLogFile::open("run-cap", "封顶测试").unwrap();
        let line = "x".repeat(MAX_LINE_CHARS - 1);
        // 略多于上限所需的行数，确保踩过界
        for _ in 0..(MAX_RUN_LOG_BYTES as usize / MAX_LINE_CHARS + 4) {
            file.append(&line);
        }
        let size = std::fs::metadata(file.path()).unwrap().len();
        // 允许超出一行加一句说明的量：判定发生在写之前，不是写之中
        assert!(
            size < MAX_RUN_LOG_BYTES + 2 * MAX_LINE_CHARS as u64,
            "封顶没生效，写了 {size} 字节"
        );
        let content = std::fs::read_to_string(file.path()).unwrap();
        assert!(
            content.contains("上限"),
            "截断没留下说明，日志会像是凭空断掉"
        );
    }

    /// 导出那份比落盘那份更严，而且是**重新生成**的快照。
    #[test]
    fn export_rewrites_the_log_through_the_stricter_sanitizer() {
        let _home = config::TempHome::new();
        let file = RunLogFile::open("run-export", "导出测试").unwrap();
        file.append("ssh 192.168.31.42 连接超时，权重在 /home/l28756/.pinvou3/model-store/models");
        file.finish("失败");

        let (exported, snapshot) = export_run_log("run-export").unwrap();
        let content = std::fs::read_to_string(&exported).unwrap();
        assert_eq!(snapshot, content);
        assert!(!content.contains("192.168.31.42"), "{content}");
        assert!(content.contains("[HOST]"), "{content}");
        // 连跑两次不许把内容叠起来——导出是"这一刻的快照"，叠加只会让人拿错
        let (again, snapshot) = export_run_log("run-export").unwrap();
        assert_eq!(snapshot, std::fs::read_to_string(&again).unwrap());
        assert_eq!(
            std::fs::read_to_string(&again).unwrap().len(),
            content.len()
        );
        // 没有这次运行时给一句人话，而不是 panic
        assert!(export_run_log("run-nope").is_err());
    }

    #[test]
    fn oversized_logs_never_return_an_export_or_clipboard_snapshot() {
        let _home = config::TempHome::new();
        let file = RunLogFile::open("run-oversize", "测试").unwrap();
        let source = std::fs::OpenOptions::new()
            .write(true)
            .open(file.path())
            .unwrap();
        source.set_len(MAX_EXPORT_LOG_BYTES + 1).unwrap();
        assert!(export_run_log("run-oversize").is_err());
        assert!(!logs_dir().join("run-oversize.export.log").exists());
    }

    /// 只留最近若干份，别让日志目录无限涨。
    #[test]
    fn old_run_logs_get_pruned() {
        let _home = config::TempHome::new();
        for index in 0..(MAX_RUN_FILES + 5) {
            let file = RunLogFile::open(&format!("run-{index:03}"), "轮转测试").unwrap();
            file.append("一行");
            file.finish("成功");
        }
        let count = std::fs::read_dir(logs_dir())
            .unwrap()
            .flatten()
            .filter(|entry| entry.path().extension().is_some_and(|e| e == "log"))
            .count();
        assert!(count <= MAX_RUN_FILES, "轮转没生效，目录里有 {count} 份");
    }

    #[test]
    fn run_id_is_reduced_to_a_single_safe_path_component() {
        assert_eq!(safe_stem("../../etc/passwd"), "etcpasswd");
        assert_eq!(safe_stem("run-AbC_1-2"), "run-AbC_1-2");
        assert_eq!(safe_stem("///"), "unknown");
    }
}
