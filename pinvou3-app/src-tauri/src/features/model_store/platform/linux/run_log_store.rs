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
//!   `credential_store::redact_secret`（全仓通用的那把尺子）以及 IP 地址
//!   （IPv4/IPv6，含 `IP:端口`、URL、参数、中文标点包裹等内嵌形态——按标准库
//!   逐候选验证，不区分公网私网）。**主机名不识别也不猜测**：没有可靠的
//!   「这是主机名而非普通单词」判据，猜错的代价是把整份日志变得读不懂；
//!   这是保守取舍，不是疏漏。
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
/// 用户导出件的独立上界（r6 P2-5）：不吃运行日志预算，但源日志轮转掉之后
/// 旧导出件就是孤儿，只增不减。
const MAX_EXPORT_FILES: usize = 10;

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

/// query 参数名带这些**前缀**的，取值也一律抹掉。SigV4 签名全家
/// （`X-Amz-Signature` / `X-Amz-Credential` / `X-Amz-Security-Token`……）逐名
/// 枚举永远追不全（评审 P2：整名精确匹配漏掉大小写/新参数），按前缀收——
/// `X-Amz-Algorithm`、`X-Amz-Date` 不是机密，但一起抹掉只是少一个非必要信息，
/// 漏一个签名值却是签名本身持久化落盘。
const SECRET_QUERY_PREFIXES: [&str; 1] = ["x-amz-"];

/// query 参数名是否命中机密名单（整名或前缀，均大小写不敏感）。
fn query_key_is_secret(key: &str) -> bool {
    SECRET_QUERY_KEYS
        .iter()
        .any(|known| key.eq_ignore_ascii_case(known))
        || SECRET_QUERY_PREFIXES
            .iter()
            .any(|prefix| key.to_ascii_lowercase().starts_with(prefix))
}

/// 命令行上叫这些名字的开关，紧跟的取值一律抹掉（`--token abc` 和 `--token=abc`）。
///
/// **刻意不收 `-p`**（`mysql -p<密码>` 那个）：它跟 `mkdir -p` 撞名，收进来会把脚本
/// 里每一个 `mkdir -p <目录>` 的目录名抹掉。单字母开关的歧义没法靠这张表消除，而
/// 抹掉路径的代价（日志读不懂）比这一档能挡住的风险大。
///
/// `--header` / `-H` 是**两段式**取值（`名字: 值` 可能拆成两个词），它们不走
/// `redact_next` 的"整词抹掉"，走 [`HeaderExpect`] 的名字保留/取值抹掉。
const SECRET_FLAGS: [&str; 8] = [
    "--token",
    "--password",
    "--passwd",
    "--secret",
    "--api-key",
    "--apikey",
    "--hf-token",
    "--header",
];

/// `--header` / `-H` 之后处于等名字还是等取值。
enum HeaderExpect {
    Name,
    Value,
}

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
    if SECRET_PREFIXES
        .iter()
        .any(|prefix| trimmed.starts_with(prefix))
    {
        return true;
    }
    // 紧凑 JSON（r5 评审 P2-3 实测泄漏）："api_key":"sk-xxx" 整体是一个词，
    // `sk-` 不在词首——把常见值定界符（冒号/逗号/引号）后的段也按前缀查一遍。
    // 只切这几个 ASCII 定界符，不切 CJK（前缀表全是 ASCII 开头，切中文白费）。
    for segment in trimmed.split([':', ',', '"']) {
        let segment = segment.trim_matches(['\'', '{', '}', '[', ']']);
        if segment.len() >= MIN_LITERAL_SECRET_LEN
            && SECRET_PREFIXES
                .iter()
                .any(|prefix| segment.starts_with(prefix))
        {
            return true;
        }
    }
    false
}

/// `KEY=VALUE` 里的 KEY 是不是机密名。大小写不敏感：脚本里两种写法都见过。
/// SigV4 全家（`Credential=AKIA…/…`、`Signature=…`、`X-Amz-Credential=` 等）按
/// 前缀+整名收——签名材料持久化落盘等于把凭证落盘（r5 评审 P2-3 实测）。
fn key_is_secret(key: &str) -> bool {
    let key = key.trim_matches(|c: char| matches!(c, '"' | '\'' | '$' | '{' | '}'));
    let lowered = key.to_ascii_lowercase();
    SECRET_ENV_KEYS
        .iter()
        .any(|known| key.eq_ignore_ascii_case(known))
        || lowered.starts_with("x-amz-")
        || matches!(
            lowered.as_str(),
            "credential" | "signature" | "security-token"
        )
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
                    Some((key, _)) if query_key_is_secret(key) => {
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
    // `--header` / `-H` 的两段式取值：先等 `名字:`、再等值。名字要留着（排查靠
    // 它认出是哪个头），值才是机密——整词抹掉会把"哪个头"一起丢掉。
    let mut header_expect: Option<HeaderExpect> = None;
    let mut first = true;
    for word in masked.split_whitespace() {
        if !first {
            out.push(' ');
        }
        first = false;
        if redact_next {
            // r7 P1-4：`password => 'secret'` 的 `=>` 是操作符不是值——把它当值
            // 消耗后真正的凭据词反而存活。操作符词（纯符号+引号+冒号+等号）不
            // 抹零、继续等下一个真正的值词。
            let is_operator = word
                .chars()
                .all(|c| matches!(c, '=' | '>' | '<' | '|' | '&' | ':' | ',' | ';' | '"'));
            if is_operator {
                out.push_str(word);
                // 保持 redact_next = true，等下一个真正的值
            } else {
                out.push_str(REDACTED);
                redact_next = false;
            }
            continue;
        }
        if let Some(HeaderExpect::Value) = header_expect {
            // r6 P1-5：`Authorization: Basic <凭据>`——scheme 词（Basic/Bearer）
            // 本身不是机密，真正的凭据是它**后面**那个词。只抹 scheme 词会把
            // 凭据词原样留下（实测泄漏）。scheme 词照抹（无诊断价值），并连带
            // 标记紧随的凭据词。
            let scheme_word = word.trim_matches(|c: char| matches!(c, '"' | '\'' | ',' | ';'));
            out.push_str(REDACTED);
            header_expect = None;
            if scheme_word.eq_ignore_ascii_case("basic")
                || scheme_word.eq_ignore_ascii_case("bearer")
            {
                redact_next = true;
            }
            continue;
        }
        if let Some(HeaderExpect::Name) = header_expect.take() {
            let trimmed = word.trim_end_matches(',');
            match trimmed.split_once(':') {
                Some((name, value)) if !value.is_empty() => {
                    out.push_str(name);
                    out.push(':');
                    out.push_str(REDACTED);
                }
                Some((name, _)) => {
                    out.push_str(name);
                    out.push(':');
                    header_expect = Some(HeaderExpect::Value);
                }
                None => match trimmed.split_once('=') {
                    // `-H X-Amz-Signature=值`：KEY=VALUE 形态的 header（r5 P2-3）。
                    // 机密键的值抹掉；普通键原样，且不再等下一个词当值。
                    Some((name, value)) if !value.is_empty() && key_is_secret(name) => {
                        out.push_str(name);
                        out.push('=');
                        out.push_str(REDACTED);
                    }
                    _ => {
                        out.push_str(trimmed);
                        header_expect = Some(HeaderExpect::Value);
                    }
                },
            }
            continue;
        }
        if word_is_secret(word) {
            out.push_str(REDACTED);
        } else if let Some((key, rest)) = word.split_once('=') {
            // `KEY=VALUE`：只看 KEY。命令行开关（`--token=abc`）和环境变量
            // （`HF_TOKEN=abc`）共用这一支。
            let flag_secret = SECRET_FLAGS
                .iter()
                .any(|known| key.eq_ignore_ascii_case(known));
            // r18 P1-3：bare 键分支（下方）r7 已并入 query 名单，`=` 分支漏了
            // 同一次并入——`+ export token=sk-xxx` / `passwd=` / `apikey=` 整值
            // 落盘+导出。两个分支必须同一张名单，分开维护就会再来一轮。
            if flag_secret || key_is_secret(key) || query_key_is_secret(key) {
                out.push_str(key);
                out.push('=');
                // `--header=X-Api-Key:value`：`= ` 形式里 header 的名字同样要留。
                if key.eq_ignore_ascii_case("--header") {
                    match rest.split_once(':') {
                        Some((name, value)) if !value.is_empty() => {
                            out.push_str(name);
                            out.push(':');
                            out.push_str(REDACTED);
                        }
                        _ => {
                            out.push_str(rest);
                            header_expect = Some(HeaderExpect::Value);
                        }
                    }
                    // 状态已在上面落好，不能再走底部的 bare 检查（会把它覆写回等名字）
                    continue;
                }
                out.push_str(REDACTED);
            } else {
                out.push_str(&redact_url_secrets(word));
            }
        } else {
            out.push_str(&redact_url_secrets(word));
        }
        let bare = word.trim_matches(|c: char| matches!(c, '"' | '\'' | ',' | ';' | ':'));
        // `--TOKEN x`（空格形态）此前大小写敏感漏网——`=` 形态早已不敏感，两处对齐。
        // r6 P1-5 + r7 P1-4：bare 键检查大小写不敏感，且并入 SECRET_QUERY_KEYS
        // （token / x-api-key / access_token 等 URL query 表已有的名字，此前
        // 裸键漏检：`token: ab12cd34` / `> x-api-key: abcdef012` 落盘+导出）。
        if bare.eq_ignore_ascii_case("bearer")
            || SECRET_ENV_KEYS
                .iter()
                .any(|known| bare.eq_ignore_ascii_case(known))
            || SECRET_QUERY_KEYS
                .iter()
                .any(|known| bare.eq_ignore_ascii_case(known))
            || query_key_is_secret(bare)
            || SECRET_FLAGS
                .iter()
                .any(|known| bare.eq_ignore_ascii_case(known))
        {
            if bare.eq_ignore_ascii_case("--header") {
                header_expect = Some(HeaderExpect::Name);
            } else {
                redact_next = true;
            }
        } else if bare == "-H" {
            // curl 的 `-H`（大写）与 `--header` 同义；`-h` 是 help，刻意不收。
            header_expect = Some(HeaderExpect::Name);
        }
    }
    clamp_line(&out)
}

/// 导出/上报走这一道：定向脱敏 + 全仓通用尺子 + 内网地址。
///
/// `AGENTS.md` 的禁写清单里内网 IP 和主机名跟凭证并列。落盘时不抹它们是刻意的
/// （「这个模型在哪台从机上起不来」全靠它），但日志一旦要离开这台机器，那条理由
/// 就不成立了。
///
/// 行首时间戳先摘出去再脱敏：`append` 盖的 RFC3339 毫秒章恰好 24 字符且含字母
/// 数字，credential_store 的粗尺（≥24 含字母数字）100% 命中——不摘的话导出件的
/// 时间列整列变 `[REDACTED]`（评审 P2 实测时间戳与正文"双灭"）。时间戳不含机密。
pub fn sanitize_for_export(line: &str) -> String {
    let (stamp, rest) = split_leading_timestamp(line);
    let targeted = sanitize_for_disk(rest);
    let broad = crate::platform::credential_store::redact_secret(&targeted);
    let masked = redact_private_hosts(&broad);
    if stamp.is_empty() {
        masked
    } else {
        format!("{stamp}{masked}")
    }
}

/// 把行首的 `2026-10-21T06:42:39.191Z ` 时间戳前缀（含尾随空格）摘出来。
/// 形状不合（非本模块盖的章）就原样返回，脱敏继续从行首做。
fn split_leading_timestamp(line: &str) -> (&str, &str) {
    let bytes = line.as_bytes();
    let shape_ok = bytes.len() > 25
        && bytes[4] == b'-'
        && bytes[7] == b'-'
        && bytes[10] == b'T'
        && bytes[13] == b':'
        && bytes[16] == b':'
        && bytes[19] == b'.'
        && bytes[23] == b'Z'
        && bytes[24] == b' '
        && bytes[..23]
            .iter()
            .all(|b| b.is_ascii_digit() || matches!(b, b'-' | b'T' | b':' | b'.' | b'Z'));
    if shape_ok {
        (&line[..25], &line[25..])
    } else {
        ("", line)
    }
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
    /// 进度行的"日志省略"说明只留一次。
    progress_notice_emitted: bool,
}

impl RunLogFile {
    /// 为一次运行开一个日志文件。**开不出来就返回 `None`，绝不让操作本身失败**：
    /// 日志是诊断手段，不是功能；为了记日志而让用户下不了模型是本末倒置。
    pub fn open(run_id: &str, label: &str, active_run_ids: &[String]) -> Option<Self> {
        let dir = logs_dir();
        if {
            use std::os::unix::fs::DirBuilderExt;
            std::fs::DirBuilder::new()
                .mode(0o700)
                .recursive(true)
                .create(&dir)
        }
        .is_err()
        {
            return None;
        }
        prune_old_logs(&dir, active_run_ids);
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
                progress_notice_emitted: false,
            }),
        })
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    /// 落一行。入参是**已经 `strip_ansi` 过、已经切好行**的那条既有出口上的行
    /// （`service::RunLog::push`）。
    pub fn append(&self, line: &str) {
        // 进度行不落盘（2026-10-20 评审 P2）：`LMM_PROGRESS_BYTES` 是高频单调计数器
        // ——168GB 下载按 1MB 粒度 ≈ 16.8 万行，会把 4MiB 预算在下载中途吃光，之后
        // 真正的报错不再落盘。界面实时显示进度；这里只留一次说明。
        if line
            .trim_start()
            .starts_with(super::script_runner::PROGRESS_MARKER)
        {
            let mut state = self.state.lock();
            if state.progress_notice_emitted {
                return;
            }
            state.progress_notice_emitted = true;
            drop(state);
            self.append("…… 下载进度已在界面实时显示，日志省略重复的进度行 ……");
            return;
        }
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
///
/// 不能删的例外（2026-10-20 评审 P2）：
/// - **活跃运行的日志**（`active_run_ids`）：脚本静默期（等输入最长 600s）里它的
///   mtime 会变旧，下一次别的运行 open 触发轮转时它正落在删除线上——fd 还开着，
///   删掉后写入全进孤儿 inode，「导出日志」从此读不到这次运行；
///
/// **`.export.log` 是用户手动导出的产物**：不吃运行日志的 20 份预算，但也不能
/// 无限累积（r6 P2-5：源日志轮转掉之后，旧导出件成了没人引用的孤儿，只增不减）。
/// 独立上界 `MAX_EXPORT_FILES`，同一次轮转里清更旧的。
fn prune_old_logs(dir: &Path, active_run_ids: &[String]) {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    let active: std::collections::HashSet<String> =
        active_run_ids.iter().map(|id| safe_stem(id)).collect();
    let mut runs: Vec<(std::time::SystemTime, PathBuf)> = Vec::new();
    let mut exports: Vec<(std::time::SystemTime, PathBuf)> = Vec::new();
    for entry in entries.flatten() {
        let path = entry.path();
        if !path.extension().is_some_and(|extension| extension == "log") {
            continue;
        }
        let modified = entry
            .metadata()
            .and_then(|m| m.modified())
            .unwrap_or(std::time::SystemTime::UNIX_EPOCH);
        if path
            .file_stem()
            .is_some_and(|stem| stem.to_string_lossy().ends_with(".export"))
        {
            exports.push((modified, path));
        } else {
            runs.push((modified, path));
        }
    }
    if runs.len() >= MAX_RUN_FILES {
        runs.sort_by(|a, b| b.0.cmp(&a.0));
        for (_, path) in runs.into_iter().skip(MAX_RUN_FILES - 1) {
            // 活跃运行的日志宁可让预算临时超一份，也不能删。
            if path
                .file_stem()
                .is_some_and(|stem| active.contains(stem.to_string_lossy().as_ref()))
            {
                continue;
            }
            let _ = std::fs::remove_file(path);
        }
    }
    if exports.len() > MAX_EXPORT_FILES {
        exports.sort_by(|a, b| b.0.cmp(&a.0));
        for (_, path) in exports.into_iter().skip(MAX_EXPORT_FILES) {
            let _ = std::fs::remove_file(path);
        }
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

    /// r5 评审 P2-3 对抗性盲区：--hf-token / 紧凑 JSON / SigV4 头与裸键值。
    #[test]
    fn sanitiser_covers_r5_adversarial_gaps() {
        for line in [
            "hf download --hf-token shortsecret123 repo",
            "hf download --HF-TOKEN shortsecret123 repo",
        ] {
            let cleaned = sanitize_for_disk(line);
            assert!(!cleaned.contains("shortsecret123"), "{line} -> {cleaned}");
        }
        let cleaned = sanitize_for_disk("cfg = {\"api_key\":\"sk-shortabc123xyz\"}");
        assert!(
            !cleaned.contains("sk-shortabc123xyz"),
            "紧凑 JSON 值泄漏：{cleaned}"
        );
        for line in [
            "Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20261022/s3/aws4_request",
            "curl -H X-Amz-Signature=abcdef0123456789abcdef0123456789abcdef01 https://s3",
        ] {
            let cleaned = sanitize_for_disk(line);
            assert!(
                !cleaned.contains("AKIAIOSFODNN7EXAMPLE")
                    && !cleaned.contains("abcdef0123456789abcdef0123456789abcdef01"),
                "{line} -> {cleaned}"
            );
        }
    }

    /// 2026-10-21 评审 P2 脱敏长尾四形态：
    /// ① `--TOKEN <值>` 空格形态大小写敏感漏网；
    /// ② `--header`/`-H` 的 `名字: 值` 两段式——名字留着（排查靠它认头），值抹掉；
    /// ③ SigV4 的 `X-Amz-Signature` 整名匹配不上（前缀收）；
    /// ④ 导出时行首时间戳被 ≥24 粗尺整列抹掉。
    #[test]
    fn sanitiser_covers_the_long_tails_the_review_found() {
        // ① 空格形态 + 大小写
        for line in [
            "hf download --Token shortsecret123 Qwen/Qwen3-8B",
            "hf download --TOKEN shortsecret123 Qwen/Qwen3-8B",
        ] {
            let cleaned = sanitize_for_disk(line);
            assert!(!cleaned.contains("shortsecret123"), "{line} -> {cleaned}");
        }
        // ② 两段式 header：`名字:` 留、值抹；一段式 `名字:值` 同样只抹值
        for (line, name) in [
            (
                "curl --header X-Api-Key: shortsecret123 https://x",
                "X-Api-Key",
            ),
            ("curl -H X-Api-Key: shortsecret123 https://x", "X-Api-Key"),
            ("curl -H X-Api-Key:shortsecret123 https://x", "X-Api-Key"),
            (
                "curl --header=X-Api-Key: shortsecret123 https://x",
                "X-Api-Key",
            ),
        ] {
            let cleaned = sanitize_for_disk(line);
            assert!(!cleaned.contains("shortsecret123"), "{line} -> {cleaned}");
            assert!(
                cleaned.contains(name),
                "header 名字不该一起抹掉：{line} -> {cleaned}"
            );
        }
        // ③ SigV4 签名参数：前缀匹配（大小写不敏感）
        for line in [
            "GET https://s3.example.com/?X-Amz-Signature=abcdef0123456789abcdef&X=1",
            "GET https://s3.example.com/?x-amz-credential=AKIAIOSFODNN7EXAMPLE/20261021&X=1",
        ] {
            let cleaned = sanitize_for_disk(line);
            assert!(
                !cleaned.contains("abcdef0123456789abcdef")
                    && !cleaned.contains("AKIAIOSFODNN7EXAMPLE/20261021"),
                "{line} -> {cleaned}"
            );
            assert!(
                cleaned.contains("s3.example.com"),
                "主机路径留着才有诊断价值：{cleaned}"
            );
        }
        // ④ 导出的时间戳列必须原样保留（粗尺会把 24 字符时间戳打成 [REDACTED]）
        let exported = sanitize_for_export(
            "2026-10-21T06:42:39.191Z curl --header X-Api-Key: shortsecret123 https://x",
        );
        assert!(
            exported.starts_with("2026-10-21T06:42:39.191Z "),
            "时间戳被抹掉了：{exported}"
        );
        assert!(!exported.contains("shortsecret123"), "{exported}");
    }

    /// r18 P1-3：`KEY=VALUE` 形态此前没并入 query 名单（bare 分支 r7 已并入、
    /// `=` 分支漏了），`set -x` 痕迹 `+ export token=sk-xxx` 整值落盘+导出。
    /// 两个分支共用同一张名单的行为由这里钉住。
    #[test]
    fn key_value_form_covers_the_query_key_list_too() {
        for line in [
            "+ export token=sk-live-abc123def456ghi789",
            "+ ACCESS_TOKEN=ghp_16C7e42F292c6912E7710c838347Ae178B4a",
            "+ accesstoken=shortsecret123",
            "+ passwd=s3cr3tValue2026",
            "+ pwd=shortsecret123",
            "+ secret=shortsecret123",
            "+ apikey=shortsecret123",
            "+ api_key=shortsecret123",
            "+ X-Amz-Security-Token=FwoGZXIvYXdzEB",
        ] {
            let cleaned = sanitize_for_disk(line);
            for secret in [
                "sk-live-abc123def456ghi789",
                "ghp_16C7e42F292c6912E7710c838347Ae178B4a",
                "shortsecret123",
                "s3cr3tValue2026",
                "FwoGZXIvYXdzEB",
            ] {
                assert!(!cleaned.contains(secret), "{line} -> {cleaned}");
            }
            let exported = sanitize_for_export(line);
            for secret in [
                "sk-live-abc123def456ghi789",
                "ghp_16C7e42F292c6912E7710c838347Ae178B4a",
                "shortsecret123",
                "s3cr3tValue2026",
                "FwoGZXIvYXdzEB",
            ] {
                assert!(!exported.contains(secret), "{line} -> {exported}");
            }
        }
        // 名字留着的诊断价值：键名本身不是机密。
        assert!(
            sanitize_for_disk("+ export token=sk-live-abc123def456ghi789").contains("token="),
            "键名被一起抹掉了"
        );
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

    /// r6 P1-5：pretty-JSON 的小写机密键。`  "api_key": "secret"` 分词后
    /// 键词 bare=`api_key`——裸键检查此前是精确匹配 `API_KEY`，小写全漏
    /// （值再短也没有长度下限兜底，导出侧的 ≥24 粗尺同样放过短值）。
    /// 键命中即 redact-next，导出件同样先过这道定向层。
    #[test]
    fn pretty_json_secret_keys_redact_next_word_case_insensitively() {
        for line in [
            "  \"api_key\": \"short-secret\"",
            "  \"Api_Key\": \"another-one\"",
            "  \"password\": \"hunter2\"",
        ] {
            let disk = sanitize_for_disk(line);
            assert!(!disk.contains("short-secret"), "{line} -> {disk}");
            assert!(!disk.contains("another-one"), "{line} -> {disk}");
            assert!(!disk.contains("hunter2"), "{line} -> {disk}");
            assert!(disk.contains("[REDACTED]"), "{line} -> {disk}");
            // 导出件先过同一道定向层再上通用尺子
            let export = sanitize_for_export(line);
            assert!(!export.contains("short-secret"), "{line} -> {export}");
        }
        // 对照：非机密键的普通 JSON 不受影响
        assert_eq!(
            sanitize_for_disk("  \"model_name\": \"qwen3.8-27b\""),
            "  \"model_name\": \"qwen3.8-27b\""
        );
    }

    /// r6 P1-5：`Authorization: Basic <凭据>` 的凭据词存活——scheme 词被抹了，
    /// 它后面的 base64 凭据原样落盘。scheme 词连带标记下一个词。
    #[test]
    fn authorization_basic_scheme_redacts_following_credential() {
        for line in [
            "curl -s -H 'Authorization: Basic dXNlcjpwYXNzOndyb25n' https://example.internal/v1",
            "curl -H \"Authorization: Basic dXNlcjpwYXNzOndyb25n\" http://10.0.0.8/api",
            "curl --header=Authorization: Bearer eyJhbGciOi.very.long.token https://x",
        ] {
            let disk = sanitize_for_disk(line);
            assert!(!disk.contains("dXNlcjpwYXNz"), "{line} -> {disk}");
            assert!(!disk.contains("eyJhbGciOi"), "{line} -> {disk}");
        }
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
        let file =
            RunLogFile::open("run-TeStId_1", "下载 hf_HeaderTokenAbCdEf0123456789", &[]).unwrap();
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
        let file = RunLogFile::open("run-cap", "封顶测试", &[]).unwrap();
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

    /// 进度行不吃落盘预算（2026-10-20 评审 P2）：16.8 万行进度若逐行落盘，
    /// 4MiB 预算在下载中途耗尽，之后真正的报错不再落盘。
    #[test]
    fn progress_lines_do_not_consume_the_disk_budget() {
        let _home = config::TempHome::new();
        let file = RunLogFile::open("run-progress", "进度测试", &[]).unwrap();
        for i in 0..2_000 {
            file.append(&format!("LMM_PROGRESS_BYTES={i}/999999"));
        }
        file.append("真实错误：磁盘空间不足");
        file.finish("失败");

        let content = std::fs::read_to_string(file.path()).unwrap();
        assert!(
            !content.contains("LMM_PROGRESS_BYTES"),
            "进度行不该落盘：\n{}",
            &content[..content.len().min(400)]
        );
        assert_eq!(
            content.matches("省略重复的进度行").count(),
            1,
            "说明只留一次"
        );
        assert!(content.contains("真实错误"), "报错必须落盘：\n{content}");
    }

    /// 导出那份比落盘那份更严，而且是**重新生成**的快照。
    #[test]
    fn export_rewrites_the_log_through_the_stricter_sanitizer() {
        let _home = config::TempHome::new();
        let file = RunLogFile::open("run-export", "导出测试", &[]).unwrap();
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
        let file = RunLogFile::open("run-oversize", "测试", &[]).unwrap();
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
            let file = RunLogFile::open(&format!("run-{index:03}"), "轮转测试", &[]).unwrap();
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

    fn set_mtime(path: &std::path::Path, when: std::time::SystemTime) {
        let file = std::fs::File::options().write(true).open(path).unwrap();
        file.set_times(
            std::fs::FileTimes::new()
                .set_accessed(when)
                .set_modified(when),
        )
        .unwrap();
    }

    /// 活跃运行的日志不能被轮转删掉（2026-10-20 评审 P2）：等输入的静默期
    /// （最长 600s）里它的 mtime 会变旧——下一次别的运行 open 触发轮转时，
    /// 它恰好落在删除线上；fd 还开着，删掉后写入全进孤儿 inode，
    /// 「导出日志」从此读不到这次运行。
    #[test]
    fn an_active_run_log_is_never_pruned_even_when_oldest() {
        let _home = config::TempHome::new();
        let dir = logs_dir();
        std::fs::create_dir_all(&dir).unwrap();
        let ancient = std::time::SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(1_000_000);
        for index in 0..(MAX_RUN_FILES + 3) {
            let path = dir.join(format!("stale-{index:03}.log"));
            std::fs::write(&path, "stale").unwrap();
            set_mtime(
                &path,
                ancient + std::time::Duration::from_secs(1 + index as u64),
            );
        }
        // 活跃那份的 mtime 最旧（等输入静默期的形态），没有保护必被删
        let active_path = dir.join("run-waiting-input.log");
        std::fs::write(&active_path, "waiting for input").unwrap();
        set_mtime(&active_path, ancient);
        RunLogFile::open("run-new", "新运行", &["run-waiting-input".to_string()]).unwrap();
        assert!(active_path.exists(), "活跃运行的日志被轮转删掉了");
        let count = std::fs::read_dir(&dir)
            .unwrap()
            .flatten()
            .filter(|entry| {
                let path = entry.path();
                path.extension().is_some_and(|e| e == "log")
                    && !path
                        .file_stem()
                        .is_some_and(|stem| stem.to_string_lossy().ends_with(".export"))
            })
            .count();
        assert!(
            count <= MAX_RUN_FILES + 1,
            "其余陈旧日志照常轮转（活跃那份允许临时超一份），目录里有 {count} 份"
        );
    }

    /// `.export.log` 是用户手动导出的产物：不吃 20 份预算，也不被静默删除。
    #[test]
    fn user_export_files_are_not_part_of_the_run_log_budget() {
        let _home = config::TempHome::new();
        let dir = logs_dir();
        std::fs::create_dir_all(&dir).unwrap();
        let export = dir.join("run-old.export.log");
        std::fs::write(&export, "exported snapshot").unwrap();
        set_mtime(&export, std::time::SystemTime::UNIX_EPOCH);
        for index in 0..(MAX_RUN_FILES + 5) {
            let file = RunLogFile::open(&format!("run-e{index:03}"), "导出预算测试", &[]).unwrap();
            file.append("一行");
            file.finish("成功");
        }
        assert!(export.exists(), "用户导出的文件被轮转删掉了");
    }

    /// r6 P2-5：导出件不吃运行预算，但要有独立上界——源日志轮转掉之后，
    /// 旧导出件是没人引用的孤儿，只增不减。超过 `MAX_EXPORT_FILES` 份时
    /// 清最旧的，且不影响运行日志的预算。
    #[test]
    fn export_files_have_their_own_bounded_retention() {
        let _home = config::TempHome::new();
        let dir = logs_dir();
        std::fs::create_dir_all(&dir).unwrap();
        for index in 0..(MAX_EXPORT_FILES + 7) {
            let path = dir.join(format!("run-x{index:03}.export.log"));
            std::fs::write(&path, "exported snapshot").unwrap();
            // mtime 递增：编号大的新
            set_mtime(
                &path,
                std::time::SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(index as u64),
            );
        }
        // 一份运行日志触发轮转（prune 在 open 时执行）
        let file = RunLogFile::open("run-x-trigger", "导出上界测试", &[]).unwrap();
        file.append("一行");
        file.finish("成功");
        let remaining: Vec<String> = std::fs::read_dir(&dir)
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|name| name.ends_with(".export.log"))
            .collect();
        assert_eq!(
            remaining.len(),
            MAX_EXPORT_FILES,
            "导出件应收敛到独立上界，实际剩 {remaining:?}"
        );
        assert!(
            remaining.contains(&"run-x016.export.log".to_string()),
            "最新的一份必须保留：{remaining:?}"
        );
        assert!(
            !remaining.contains(&"run-x000.export.log".to_string()),
            "最旧的一份应被清掉：{remaining:?}"
        );
    }

    #[test]
    fn run_id_is_reduced_to_a_single_safe_path_component() {
        assert_eq!(safe_stem("../../etc/passwd"), "etcpasswd");
        assert_eq!(safe_stem("run-AbC_1-2"), "run-AbC_1-2");
        assert_eq!(safe_stem("///"), "unknown");
    }
}
