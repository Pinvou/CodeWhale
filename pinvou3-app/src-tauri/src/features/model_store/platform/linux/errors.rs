//! 把底层库/外部工具抛出的英文错误信息转换成用户能看懂的中文提示。
//!
//! 给用户看的文案必须是干净的中文。`AppError` 的消息文本在构造时就已经是
//! 干净的中文，可以直接展示；别的错误（网络库、脚本原始输出）需要经过
//! 本模块的转换函数，避免把英文原始报错糊给用户看。

use std::fmt;

#[derive(Debug, Clone)]
pub struct AppError(pub String);

impl fmt::Display for AppError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}", self.0)
    }
}

impl std::error::Error for AppError {}

impl AppError {
    pub fn new(msg: impl Into<String>) -> Self {
        Self(msg.into())
    }
}

/// 卡片脚本（不管背后是 ollama/sglang/其它引擎）大多是外部工具/命令行的英文报错，
/// 这里对常见的几类报错做关键词匹配转成中文；匹配不到的情况退而求其次，给一个
/// 中文前缀 + 原始简短提示，而不是整段英文。这个函数是所有卡片脚本失败时的通用
/// 摘要逻辑，不是只给 Ollama 用的，命名/文案不要带 Ollama 字样。
const SCRIPT_ERROR_PATTERNS: &[(&str, &str)] = &[
    // pkexec 的几种结局要分开说，而且必须排在最前面：下面那条笼统的 "unauthorized"
    // 和 "permission denied" 会把它们全盖成「检查凭据配置」，而真正的原因跟凭据毫无
    // 关系，照着那句话排查永远修不好。
    //
    // 这几条文案的措辞是复盘出来的，别随手改回"猜原因"的写法：2026-09-18 现场是
    // 「一直弹密码框、输对了也一直报错」，当时先入为主归因成"品悟是不是 SSH 起的"，
    // 而实测品悟是标准桌面启动、用户在 94 秒里成功认证了 6 次。真正的原因是
    // model_manager 无退避地重发了约 16 次 pkexec，gnome-shell 的 polkit agent
    // 一次只能持有一个 modal grab，被打爆之后开始自动 dismiss，于是"永远密码错"。
    // 所以这里只陈述**观察到的事实**，把"接下来做什么"交给用户，不替他断因。
    (
        "request dismissed",
        "管理员授权被取消（你点了取消，或系统授权框被打断）。安装引擎没有继续，重新点「下载」可以再来一次",
    ),
    (
        "authentication failed",
        "管理员授权失败。请重新点「下载」，并在弹出的系统授权框里输入本机登录密码",
    ),
    (
        "no authentication agent found",
        "系统找不到授权代理，无法弹出管理员密码框。请确认品悟是在设备桌面上打开的（而不是通过 SSH 等远程方式启动）",
    ),
    (
        "error executing command as another user",
        "管理员授权失败。请重新点「下载」，并在弹出的系统授权框里输入本机登录密码",
    ),
    ("no space left on device", "磁盘空间不足"),
    ("model not found", "找不到对应的模型"),
    ("invalid file magic", "文件格式无效，可能不是有效的模型文件"),
    ("invalid gguf", "文件格式无效，可能不是有效的 GGUF 模型文件"),
    (
        "connection refused",
        "无法连接到推理服务，请确认服务正在运行",
    ),
    ("manifest unknown", "在远端找不到该模型/量化版本"),
    ("unauthorized", "没有访问权限，请检查凭据配置"),
    ("timeout", "操作超时，请稍后重试"),
    // ssh 自己的连接失败码就是 255（区别于远端命令的退出码）。双机卡的每一步都要
    // 经 ssh 操作从机，从机断电、直连线松了、网口没起来，都会走到这里。
    // 不给这条的话用户看到的是一整段 ssh 命令行——而那段里还带着 ConnectTimeout，
    // 正是上面那条 timeout 误伤的来源。
    (
        "exit status 255",
        "无法连接到从机，请检查从机是否开机、直连网线和网口是否正常",
    ),
    (
        "permission denied",
        "权限不足，请检查当前用户权限（比如是否需要加入 docker 用户组）",
    ),
];

pub fn humanize_script_message(raw: &str) -> String {
    let raw = raw.trim();
    if raw.is_empty() {
        return "未知错误".to_string();
    }

    // 有些脚本出错是 {"error": "..."} 这样的 JSON（比如调用 Ollama HTTP API 的
    // 脚本），先把纯文本提出来
    let text: String = match serde_json::from_str::<serde_json::Value>(raw) {
        Ok(serde_json::Value::Object(map)) => map
            .get("error")
            .map(|v| v.as_str().unwrap_or_default().trim().to_string())
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| raw.to_string()),
        _ => raw.to_string(),
    };

    let lowered = text.to_lowercase();
    for (keyword, translation) in SCRIPT_ERROR_PATTERNS {
        if contains_as_word(&lowered, keyword) {
            return translation.to_string();
        }
    }

    // 卡片脚本被要求「失败要说人话」（规范第 4 节通用要求），做到了的脚本会打出
    // 一句面向用户的中文错误。这种句子再套一层「脚本返回错误（…）」纯属噪声：
    // 界面上会显示成「脚本执行失败（退出码 1）：脚本返回错误（ERROR: 没有检测到
    // 处于 ACTIVE 状态的直连接口…）」——三层前缀套下来，真正有用的那句被挤到后面
    // 看不见了。所以已经是中文的，扒掉 ERROR: 前缀后原样呈现。
    let text = strip_error_prefix(&text);
    let short: String = if text.chars().count() <= 120 {
        text.clone()
    } else {
        let truncated: String = text.chars().take(120).collect();
        format!("{truncated}…")
    };
    if short.chars().any(is_cjk) {
        return short;
    }
    // 纯英文的（多半是底层工具直接吐出来的）才加中文说明，不让用户面对一整段英文
    format!("脚本返回错误（{short}）")
}

/// 一眼就是"错误本身"的通用词。命中一条就说明这一行在陈述**发生了什么坏事**。
///
/// 跟 [`SCRIPT_ERROR_PATTERNS`] 分开：那张表是"能翻成人话的已知原因"，命中它的行
/// 价值更高（下面给的分更高）；这张表管的是"翻不出人话、但确实是报错"的行。
const ERROR_TOKENS: &[&str] = &[
    "error",
    "fatal",
    "failed",
    "failure",
    "cannot",
    "can't",
    "unable",
    "no such",
    "not found",
    "denied",
    "refused",
    "timed out",
    "timeout",
    "traceback",
    "panic",
    "aborted",
    "exceeded",
    "unreachable",
    "错误",
    "失败",
    "无法",
    "不能",
    "超时",
    "不足",
    "拒绝",
    "找不到",
    "不存在",
    "异常",
];

/// 这一行是**建议**，不是原因。
///
/// # 为什么必须有这一档
///
/// 日本客户的截图实锤过：他看到的"失败原因"是
/// 「也可以在别的机器上拉好后用 docker save/load 拷进来」——那是脚本在报错之后好心
/// 附上的一句变通办法，而真正的原因（DNS 超时）被埋在它上面几行。摘要原来取的是
/// **最后一条非空行**，于是越是"失败要说人话"写得认真的脚本（报完错还给建议），
/// 摘要就越没用。用户照着那句建议排查，永远修不到 DNS。
///
/// # 🔴 判据看的是**位置**，不是"含不含"
///
/// 这张表里的 `请检查` / `请确认` / `please` 在真正的报错句子**尾部**极其常见：
/// 「拉取引擎镜像失败，请检查网络或配置镜像加速器」是一句合格的失败原因，只是顺手
/// 带了下一步。按"含不含"判会把这一整类好句子扣成负分，摘要反而退回去挑那些什么都
/// 没说的中性行。所以判据是 **marker 落在行首那一小段里**（见 [`ADVICE_LEAD_CHARS`]）：
/// 通篇是建议的行才从建议词开头。
const ADVICE_MARKERS: &[&str] = &[
    "也可以",
    "你可以",
    "您可以",
    "可以尝试",
    "可尝试",
    "请尝试",
    "试试",
    "建议",
    "提示：",
    "提示:",
    "参考",
    "或者",
    "解决办法",
    "解决方法",
    "处理办法",
    "如需",
    "若需",
    "请先",
    "请检查",
    "请确认",
    "workaround",
    "hint:",
    "note:",
    "tip:",
    "see also",
    "you can",
    "try ",
    "please ",
];

/// 纯装饰/纯噪声行：分隔线、进度条残渣、协议行。
fn is_noise_line(lowered: &str) -> bool {
    let t = lowered.trim();
    if t.is_empty() {
        return true;
    }
    // `====`、`----`、`****` 这类分隔线：没有一个字是信息
    if t.chars()
        .all(|c| matches!(c, '=' | '-' | '*' | '#' | '~' | '_' | ' ' | '.'))
    {
        return true;
    }
    // docker/pip 的逐层进度残渣
    t.contains("pulling fs layer")
        || t.contains("verifying checksum")
        || t.contains("already exists")
        || t.contains("download complete")
        || t.contains("pull complete")
        || t.contains("waiting")
        || t.contains("extracting")
}

/// 扒掉 `[安装引擎] ` / `[下载模型] ` 这类阶段前缀。
///
/// 手写而不是上正则：这个 crate 没有 `regex` 依赖，而判据本身只有三条（行首是 `[`、
/// 有配对的 `]`、方括号里不超过 12 个字符）。上限是为了不把日志里真正以 `[` 开头的
/// 长句子（比如 `[====>] 1.2GB/4.1GB`）当成前缀切掉半截。
fn strip_stage_prefix(line: &str) -> &str {
    let t = line.trim_start();
    if !t.starts_with('[') {
        return t;
    }
    match t.find(']') {
        Some(close) if t[1..close].chars().count() <= 12 => t[close + 1..].trim_start(),
        _ => t,
    }
}

/// 判"这一整行是建议"时只看行首这么多个字符。
///
/// 8 个字的依据：中文建议句的引导词都很短（`也可以` / `建议` / `提示：` / `请先`），
/// 而合格的报错句在把话说到 `请检查` 之前一定先说了发生什么
/// （`拉取引擎镜像失败，` 就已经 9 个字）。窗口再宽就会开始吃掉好句子。
const ADVICE_LEAD_CHARS: usize = 8;

/// 这一行是不是**通篇建议**。判据看位置，理由见 [`ADVICE_MARKERS`]。
///
/// `has_error_signal` 是"这行含错误词吗"。两条判据：
/// 1. 建议词落在行首那一小段里 —— 整句话从建议开头，它不是原因；
/// 2. 整行一个错误词都没有、却有建议词 —— 那也只是一句好心的下一步。
fn is_advice_line(lowered: &str, has_error_signal: bool) -> bool {
    // 先扒掉 `[安装引擎] ` 这类阶段前缀和 `ERROR:` / `错误：` 这类错误前缀，否则
    // 「[安装引擎] 也可以……」的建议词会被前缀顶出行首窗口，判据静默失效。
    //
    // 现在的唯一调用点（`script_runner` 里脚本非零退出那一支）拿到的是**没加前缀**的
    // 原始行——前缀是 `lifecycle` 在 `on_progress` 回调里才加的。这里仍然剥一道，是
    // 因为"输入没有前缀"这个前提写在另一个文件里，而它一旦变了，失效是静默的。
    let stripped = strip_error_prefix(&strip_stage_prefix(lowered));
    let lead: String = stripped.chars().take(ADVICE_LEAD_CHARS).collect();
    if ADVICE_MARKERS.iter().any(|marker| lead.contains(marker)) {
        return true;
    }
    !has_error_signal
        && ADVICE_MARKERS
            .iter()
            .any(|marker| stripped.contains(marker))
}

/// 给一行打分：越大越像"失败的真正原因"。
fn failure_line_score(line: &str) -> i32 {
    let lowered = line.to_lowercase();
    if is_noise_line(&lowered) {
        return -12;
    }
    let mut score = 0;
    // 能翻成人话的已知原因，价值最高
    let known_cause = SCRIPT_ERROR_PATTERNS
        .iter()
        .any(|(keyword, _)| contains_as_word(&lowered, keyword));
    if known_cause {
        score += 6;
    }
    let has_error_token = ERROR_TOKENS
        .iter()
        .any(|token| contains_as_word(&lowered, token));
    if has_error_token {
        score += 3;
    }
    // 建议不是原因。**减得比上面两档加起来还多**：一句建议里完全可能把错误词重复一遍
    // （「下载失败也可以在别的机器上拉好」），不减过头就还是会被选中。
    if is_advice_line(&lowered, known_cause || has_error_token) {
        score -= 12;
    }
    score
}

/// 从失败运行的输出尾部挑出**最像失败原因**的那一行。
///
/// # 选行规则
///
/// 按 [`failure_line_score`] 取最高分；同分取**更靠后**的那一行（越晚打出来的越接近
/// 退出点）。全都同分（典型情况：一行都不含错误词，比如脚本静默 `exit 1`）时，
/// 这条规则自然退化成"最后一条非空行"，也就是原来的行为——**不会比原来更差**。
///
/// # 为什么不是"最后一条非空行"
///
/// 见 [`ADVICE_MARKERS`]：那正是日本客户那次误导的来源。
pub fn pick_failure_line<'a>(lines: &'a [String]) -> Option<&'a str> {
    let mut best: Option<(i32, &str)> = None;
    for line in lines {
        let text = line.trim();
        if text.is_empty() {
            continue;
        }
        let score = failure_line_score(text);
        // `>=` 而不是 `>`：同分时让更靠后的行赢。
        if best.is_none_or(|(best_score, _)| score >= best_score) {
            best = Some((score, text));
        }
    }
    best.map(|(_, text)| text)
}

/// 关键词必须作为**独立的词**出现，不能是别的单词的一部分。
///
/// 真实踩过的坑：双机卡卸载失败时脚本打出的是
/// `Worker: Command ['ssh', '-o', 'ConnectTimeout=10', …] returned non-zero exit status 255`。
/// 这里面 `ConnectTimeout` 含有 `timeout`，于是整句被替换成「操作超时，请稍后重试」——
/// 而真正的原因是 SSH 连不上从机（255 是 ssh 自己的连接失败码），跟超时毫无关系。
/// 用户照着这句提示「稍后重试」，重试多少次都一样。
///
/// 判据只有一条：关键词前后一个字节不是 ASCII 字母数字。于是 `connecttimeout`
/// 不算命中，`timeout:` 和 `(timeout)` 算；keyword 本身含空格（如
/// `no space left on device`）时同样适用。
///
/// **修掉的只有"字母数字粘连"这一类**——`ConnectTimeout`、`StrictHostKeyChecking`、
/// `timeouts`、`xtimeout`。这条注释原来还写着它能挡住"路径里带 unauthorized 的模型名、
/// URL 里带 timeout 的查询参数"，那是夸大：`-`、`=`、`/`、`?`、`.` 在这个判据下**都算
/// 边界**，所以 `models/unauthorized-model`（d 后面是 `-`）和 `?timeout=30`
/// （`?` 在前、`=` 在后）照样命中，照样被翻成「没有访问权限」/「操作超时」。
///
/// 要连这一类一起挡住，就不是收紧边界字符集能了的（`-` 和 `/` 在真实的错误句子里
/// 本来就是合法的词间分隔），得动"拿整段命令行/URL 当输入去猜原因"这个前提本身。
/// 那是行为变更，另案处理；在此之前别把这里读成"误伤已经没有了"。
fn contains_as_word(haystack: &str, keyword: &str) -> bool {
    let bytes = haystack.as_bytes();
    let mut from = 0;
    while let Some(offset) = haystack[from..].find(keyword) {
        let start = from + offset;
        let end = start + keyword.len();
        let before_ok = start == 0 || !bytes[start - 1].is_ascii_alphanumeric();
        let after_ok = end >= bytes.len() || !bytes[end].is_ascii_alphanumeric();
        if before_ok && after_ok {
            return true;
        }
        // 没命中就从这次匹配的下一个字节继续找，别漏掉重叠的情况
        from = start + 1;
        if from >= haystack.len() {
            break;
        }
    }
    false
}

fn is_cjk(c: char) -> bool {
    matches!(c, '\u{4e00}'..='\u{9fff}' | '\u{3400}'..='\u{4dbf}')
}

/// 扒掉脚本习惯性加的错误前缀。留着它只会让界面上出现两三层「错误」字样。
fn strip_error_prefix(text: &str) -> String {
    let t = text.trim();
    for p in [
        "ERROR:",
        "Error:",
        "error:",
        "ERROR -",
        "错误：",
        "错误:",
        "失败：",
        "失败:",
    ] {
        if let Some(rest) = t.strip_prefix(p) {
            return rest.trim().to_string();
        }
    }
    t.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn matches_known_keywords() {
        assert_eq!(
            humanize_script_message("Error: no space left on device"),
            "磁盘空间不足"
        );
        assert_eq!(
            humanize_script_message(r#"{"error": "model not found: xyz"}"#),
            "找不到对应的模型"
        );
    }

    /// 关键词不能在别的单词内部命中。
    ///
    /// 这条是真机踩出来的：用户卸载双机卡，从机连不上，脚本打出的是
    /// `失败: Worker: Command ['ssh', '-o', 'ConnectTimeout=10', …] returned
    /// non-zero exit status 255.`——`ConnectTimeout` 里的 `timeout` 命中了
    /// 子串匹配，界面上于是显示「操作超时，请稍后重试」。用户照着这句"稍后重试"，
    /// 重试多少次都一样，因为真正的原因是从机压根没通。
    ///
    /// 错误提示把人引向错误的方向，比不给提示更糟。
    #[test]
    fn a_keyword_inside_another_word_must_not_hijack_the_message() {
        let real = "失败: Worker: Command ['ssh', '-o', 'ConnectTimeout=10', '-o', \
                    'ServerAliveInterval=15', '-o', 'BatchMode=yes', 'user@192.0.2.20', \
                    'python3 /home/user/megacube-deepseek/controller.py _node uninstall e30='] \
                    returned non-zero exit status 255.";
        let got = humanize_script_message(real);
        assert_ne!(got, "操作超时，请稍后重试", "ConnectTimeout 不是 timeout");
        // ssh 的 255 是连接失败，要说成人话而不是甩一整段命令行
        assert_eq!(
            got,
            "无法连接到从机，请检查从机是否开机、直连网线和网口是否正常"
        );

        // 真正独立成词的 timeout 仍然要命中
        assert_eq!(
            humanize_script_message("request timeout after 30s"),
            "操作超时，请稍后重试"
        );
        assert_eq!(
            humanize_script_message("Error: timeout"),
            "操作超时，请稍后重试"
        );
    }

    /// pkexec 的失败必须按原因分开说，不能被后面那条笼统的 "unauthorized" 盖掉。
    ///
    /// 现场症状是「一直提示输入密码、一直报错」：没有桌面会话时 pkexec 找不到图形
    /// 授权代理，退化成在伪终端里打印 `Password:` 等输入，而那个伪终端接的是任务
    /// 日志面板，用户没有地方可以输。原文全是英文，不翻成人话用户根本不知道该干嘛。
    #[test]
    fn pkexec_failures_say_what_the_user_should_actually_do() {
        // 授权被取消 —— 不是故障，别吓唬他，也别去猜是谁取消的（现场那次是 polkit
        // 代理被打爆后自动 dismiss 的，根本不是用户点的）
        assert_eq!(
            humanize_script_message("Error executing command as another user: Request dismissed"),
            "管理员授权被取消（你点了取消，或系统授权框被打断）。安装引擎没有继续，重新点「下载」可以再来一次"
        );

        // 认证失败：两种原文措辞都要命中，而且都要把用户引回那个系统授权框
        for raw in [
            "==== AUTHENTICATION FAILED ====",
            "Error executing command as another user: Not authorized",
        ] {
            let got = humanize_script_message(raw);
            assert_eq!(
                got,
                "管理员授权失败。请重新点「下载」，并在弹出的系统授权框里输入本机登录密码"
            );
        }

        // 真的没有授权代理（比如从 SSH 里跑）才提"要在设备桌面上打开"。
        // 这句话只能出现在这一条上：把它挂到 authentication failed 上会把排查方向
        // 带偏 —— 现场那次品悟就是标准桌面启动的。
        let got = humanize_script_message(
            "Error executing command as another user: No authentication agent found.",
        );
        assert!(got.contains("桌面"), "实际：{got}");

        // 以上任何一条都绝不能退化成那两句笼统的权限提示 —— 跟凭据、用户组毫无
        // 关系，照它们排查永远修不好
        for raw in [
            "Error executing command as another user: Request dismissed",
            "==== AUTHENTICATION FAILED ====",
            "Error executing command as another user: No authentication agent found.",
        ] {
            let got = humanize_script_message(raw);
            assert_ne!(got, "没有访问权限，请检查凭据配置");
            assert_ne!(
                got,
                "权限不足，请检查当前用户权限（比如是否需要加入 docker 用户组）"
            );
        }
    }

    /// 授权类失败必须被判为致命 —— 这是「一直弹密码框」那个故障的真正修复点。
    #[test]
    fn authorization_failures_must_stop_the_retry_loop() {
        use super::super::lifecycle::ModelManager;
        for raw in [
            "Error executing command as another user: Request dismissed",
            "==== AUTHENTICATION FAILED ====",
            "Error executing command as another user: No authentication agent found.",
        ] {
            let humanized = humanize_script_message(raw);
            assert!(
                ModelManager::is_fatal_failure(&humanized),
                "授权类失败必须致命，否则每重试一次就多弹一个系统密码框，\
                 最终把 polkit 代理打到自动 dismiss（现场实测 94 秒 16 次）。实际：{humanized}"
            );
        }
    }

    /// 词边界判定本身的边界情况。
    #[test]
    fn word_boundary_matching_handles_punctuation_and_multiword_keywords() {
        // 标点、括号算边界
        assert!(contains_as_word(
            "connection refused (retry)",
            "connection refused"
        ));
        assert!(contains_as_word("timeout:", "timeout"));
        assert!(contains_as_word("[timeout]", "timeout"));
        // 粘在字母数字上的不算
        assert!(!contains_as_word("connecttimeout=10", "timeout"));
        assert!(!contains_as_word("timeouts", "timeout"));
        assert!(!contains_as_word("xtimeout", "timeout"));
        // 同一个词出现多次，只要有一次是独立的就算命中
        assert!(contains_as_word(
            "connecttimeout=10 and a real timeout",
            "timeout"
        ));
        // 带空格的多词关键词
        assert!(contains_as_word(
            "error: no space left on device",
            "no space left on device"
        ));
    }

    #[test]
    fn falls_back_with_prefix() {
        assert_eq!(
            humanize_script_message("some weird error"),
            "脚本返回错误（some weird error）"
        );
    }

    #[test]
    fn handles_empty() {
        assert_eq!(humanize_script_message("   "), "未知错误");
    }

    /// 脚本按规范写了一句面向用户的中文错误时，原样呈现，不要再套壳。
    ///
    /// 真机上的反面例子：双机卡片在设备没有直连网卡时会打出
    /// `ERROR: 没有检测到处于 ACTIVE 状态的直连接口，请检查 QSFP 线缆…`，
    /// 界面上却显示成「脚本执行失败（退出码 1）：脚本返回错误（ERROR: 没有检测
    /// 到…）」——三层"错误"前缀把真正有用的那句挤到了看不见的地方。
    #[test]
    fn chinese_script_errors_are_shown_as_is() {
        assert_eq!(
            humanize_script_message(
                "ERROR: 没有检测到处于 ACTIVE 状态的直连接口，请检查 QSFP 线缆是否插好"
            ),
            "没有检测到处于 ACTIVE 状态的直连接口，请检查 QSFP 线缆是否插好"
        );
        // 中文前缀同样扒掉
        assert_eq!(
            humanize_script_message("错误：模型目录不可写"),
            "模型目录不可写"
        );
        // 没有前缀的中文原样返回
        assert_eq!(humanize_script_message("从机未开机"), "从机未开机");
    }

    /// 纯英文的仍然要加中文说明——那多半是底层工具直接吐出来的，
    /// 不加壳用户面对的就是一整段英文。
    #[test]
    fn english_errors_still_get_a_chinese_wrapper() {
        assert_eq!(
            humanize_script_message("ERROR: something odd happened"),
            "脚本返回错误（something odd happened）"
        );
    }

    /// 🔴 日本客户那张截图的回归测试。
    ///
    /// 他看到的"失败原因"是最后那句**建议**（docker save/load 变通办法），真正的原因
    /// （DNS 解析超时）在它上面。摘要原来取"最后一条非空行"，所以越是报完错还附带
    /// 变通办法的脚本，摘要越没用。
    #[test]
    fn the_summary_is_the_cause_not_the_workaround() {
        let lines: Vec<String> = [
            "[安装引擎] + docker pull vllm/vllm-openai:v0.8.5",
            "[安装引擎] Error response from daemon: Get \"https://registry-1.docker.io/v2/\": dial tcp: lookup registry-1.docker.io: i/o timeout",
            "[安装引擎] ERROR: 拉取引擎镜像失败，请检查网络或配置镜像加速器",
            "[安装引擎] 也可以在别的机器上拉好后用 docker save/load 拷进来",
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();

        let picked = pick_failure_line(&lines).expect("有非空行就该挑出一行");
        assert!(
            !picked.contains("docker save/load"),
            "又挑中了那句建议：{picked}"
        );
        assert!(
            picked.contains("timeout") || picked.contains("拉取引擎镜像失败"),
            "挑出来的不是原因：{picked}"
        );
    }

    /// 一行都不含错误词时，退化成"最后一条非空行"——也就是原来的行为，不会更差。
    #[test]
    fn picking_falls_back_to_the_last_nonempty_line() {
        let lines: Vec<String> = ["第一行", "", "最后一行", "   "]
            .iter()
            .map(|s| s.to_string())
            .collect();
        assert_eq!(pick_failure_line(&lines), Some("最后一行"));
        assert_eq!(pick_failure_line(&[]), None);
    }

    /// docker 的逐层刷屏绝不能当成失败原因：它往往正好是退出前的最后几行。
    #[test]
    fn layer_noise_never_wins_over_a_real_error() {
        let lines: Vec<String> = [
            "no space left on device",
            "7e1f8fb7d4ba: Verifying Checksum",
            "7e1f8fb7d4ba: Download complete",
            "==========================",
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();
        assert_eq!(pick_failure_line(&lines), Some("no space left on device"));
    }

    /// 同分时取更靠后的那一行：越晚打出来的越接近退出点。
    ///
    /// 这里挑的两行必须是**真的同分**。`invalid gguf` 和 `manifest unknown` 都只命中
    /// [`SCRIPT_ERROR_PATTERNS`]（各 +6），谁都不含 [`ERROR_TOKENS`] 里的通用错误词，
    /// 所以 6 分平手、靠位置分胜负。
    ///
    /// 这条测试原来拿的是 `connection refused` 和 `manifest unknown`，而那两行**并不
    /// 同分**：`refused` 同时在 `ERROR_TOKENS` 里，前者是 9 分、后者 6 分，选前者是
    /// 对的。挑错样例的测试会反过来逼着实现去迎合一个错的预期——所以下面把"信号更强
    /// 的行赢过更靠后的行"单独钉一条，两条一起才说清了优先级：**先比分，再比位置**。
    #[test]
    fn later_lines_win_on_a_tie() {
        let lines: Vec<String> = ["invalid gguf", "manifest unknown"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        assert_eq!(pick_failure_line(&lines), Some("manifest unknown"));
    }

    /// 位置只是**平手时**的裁判，压不过更强的错误信号。
    ///
    /// `connection refused` 既是已知原因（+6）又含通用错误词 `refused`（+3），
    /// 而它后面那行只有 +6。更靠后的行输掉了——这正是这套打分想要的：先比分，再比位置。
    #[test]
    fn a_stronger_signal_beats_a_later_position() {
        let lines: Vec<String> = ["connection refused", "manifest unknown"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        assert_eq!(pick_failure_line(&lines), Some("connection refused"));
    }

    /// 🔴 报错句尾巴上的「请检查…」不算建议。
    ///
    /// 这条钉的是 [`is_advice_line`] 看**位置**而不是"含不含"。
    /// 「拉取引擎镜像失败，请检查网络或配置镜像加速器」是一句合格的失败原因，只是顺手
    /// 带了下一步；按"含不含建议词"判会把这一整类好句子扣成负分，摘要反而退回去挑那些
    /// 什么都没说的中性行（`+ docker pull …`）——那比原来的缺陷更糟。
    #[test]
    fn a_next_step_at_the_end_of_a_real_error_is_not_advice() {
        let lines: Vec<String> = [
            "+ docker pull vllm/vllm-openai:v0.8.5",
            "ERROR: 拉取引擎镜像失败，请检查网络或配置镜像加速器",
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();
        assert_eq!(
            pick_failure_line(&lines),
            Some("ERROR: 拉取引擎镜像失败，请检查网络或配置镜像加速器")
        );
    }

    /// 阶段前缀不能让"通篇是建议"的判据失效。
    ///
    /// 当前调用点拿到的是没加前缀的原始行（前缀是 `lifecycle` 在 `on_progress` 里才加
    /// 的），但那个前提写在另一个文件里；它一旦变了，失效是静默的。
    #[test]
    fn a_stage_prefix_does_not_hide_the_advice_marker() {
        assert_eq!(
            strip_stage_prefix("[安装引擎] 也可以换个镜像源"),
            "也可以换个镜像源"
        );
        // 方括号里太长的不当前缀切——日志里真以 `[` 开头的长句子不该被砍半截
        let long = "[这是一段很长的不是阶段名的东西] 正文";
        assert_eq!(strip_stage_prefix(long), long);

        let lines: Vec<String> = [
            "[安装引擎] connection refused",
            "[安装引擎] 也可以在别的机器上拉好后用 docker save/load 拷进来",
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();
        assert_eq!(
            pick_failure_line(&lines),
            Some("[安装引擎] connection refused")
        );
    }

    /// 挑完还要过 `humanize_script_message`：用户看到的是那句中文。
    #[test]
    fn the_picked_line_still_goes_through_humanize() {
        let lines: Vec<String> = [
            "Error response from daemon: no space left on device",
            "请检查磁盘剩余空间后重试",
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();
        let picked = pick_failure_line(&lines).unwrap();
        assert_eq!(humanize_script_message(picked), "磁盘空间不足");
    }
}
