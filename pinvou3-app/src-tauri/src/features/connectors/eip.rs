//! H3C EIP 员工门户接入 —— 连接生命周期 + 鉴权编排。
//!
//! 路线:A(CLI 连接器)。CLI 二进制内置打包到 `~/.pinvou3/bundle/skills/eip/bin/`,
//! **五端(Windows / Linux x64 / Linux ARM64 / macOS arm64 / macOS x64)统一由仓库内
//! 源码 `linux-src/main.go` 交叉编译**,见同目录 `build.sh`。官方 iClaw 只发
//! Windows/Linux x86_64 且已不可用:SSO 服务端把登录响应 `pollInterval` 从 string
//! 改为 number 后,其 Go struct 反序列化必失败(auth login / AIT 重登录全挂,报文
//! 误导读检查网络);自研实现按 `json.RawMessage` 宽容解析,对字段类型变化天然兼容。
//! 业务命令(考勤/假期/待办…)由模型经 shell 跑 `eip <域> ...`,技能文档教用法
//! (同飞书 lark-cli)。本模块只负责**连接态**:状态查询、SSO 登录(轮询自动收)、
//! 取消、登出。
//!
//! 鉴权(用户零找 key):CLI 自管凭证——首次跑业务命令未认证时输出 SSO URL,
//! 用户浏览器登录后 `auth poll` 自动收凭证;token 过期 CLI 自动用 AIT 刷新。
//! 凭证由 CLI 用 `AGENT_DEVICE_ID` 派生密钥加密存到 `AGENT_CREDENTIALS_DIR`。
//! 见《EIP员工门户接入-开发方案》§3。
//!
//! ⚠️ 协议与路由源自 H3C IT 的 iClaw eip-cli(内部产物);合规事后补内部知会。

use std::path::PathBuf;
use std::process::Command;
use std::time::{Duration, Instant};

use super::flow_gate::{Begin, CANCEL_WAIT_LIMIT, FlowGuard, SsoFlow};

use qrcode::{QrCode, render::svg};
use serde_json::{Value, json};
use tauri::{AppHandle, Emitter, Manager};

/// 标准 base64 编码(避免引新依赖)。
fn b64(data: &[u8]) -> String {
    const A: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(data.len().div_ceil(3) * 4);
    for chunk in data.chunks(3) {
        let b0 = chunk[0] as u32;
        let b1 = *chunk.get(1).unwrap_or(&0) as u32;
        let b2 = *chunk.get(2).unwrap_or(&0) as u32;
        let n = (b0 << 16) | (b1 << 8) | b2;
        out.push(A[((n >> 18) & 63) as usize] as char);
        out.push(A[((n >> 12) & 63) as usize] as char);
        out.push(if chunk.len() > 1 {
            A[((n >> 6) & 63) as usize] as char
        } else {
            '='
        });
        out.push(if chunk.len() > 2 {
            A[(n & 63) as usize] as char
        } else {
            '='
        });
    }
    out
}

/// 把 SSO URL 生成二维码 data URL。用 SVG 避免依赖外部 CLI 的 qrcode 子命令。
fn make_qr(url: &str) -> Option<String> {
    let code = QrCode::new(url.as_bytes()).ok()?;
    let image = code
        .render::<svg::Color<'_>>()
        .min_dimensions(256, 256)
        .dark_color(svg::Color("#000000"))
        .light_color(svg::Color("#ffffff"))
        .build();
    Some(format!(
        "data:image/svg+xml;base64,{}",
        b64(image.as_bytes())
    ))
}

// ──────────────────────────── 路径 / 凭证 ────────────────────────────

/// `~/.pinvou3/eip/` —— EIP 的 device-id 与凭证根目录(与 bundle 里的二进制分开)。
fn eip_home() -> PathBuf {
    crate::platform::paths::pinvou3_home().join("eip")
}

/// 凭证目录,传给 CLI 的 `AGENT_CREDENTIALS_DIR`。CLI 在此加密存 token/ait。
fn credentials_dir() -> PathBuf {
    let d = eip_home().join("credentials");
    let _ = std::fs::create_dir_all(&d);
    d
}

/// 稳定的 `AGENT_DEVICE_ID`(CLI 用它派生凭证加密密钥)。**一次生成、持久化**——
/// 换了 device-id 会解不开旧凭证(方案 §7 风险)。首次用 sha256(时间+pid) 取 16 字节 hex。
fn device_id() -> String {
    let p = eip_home().join("device_id");
    if let Ok(s) = std::fs::read_to_string(&p) {
        let s = s.trim().to_string();
        if !s.is_empty() {
            return s;
        }
    }
    use sha2::{Digest, Sha256};
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    let mut h = Sha256::new();
    h.update(format!("{}-{}", nanos, std::process::id()).as_bytes());
    let id: String = h
        .finalize()
        .iter()
        .take(16)
        .map(|b| format!("{:02x}", b))
        .collect();
    let _ = std::fs::create_dir_all(eip_home());
    let _ = std::fs::write(&p, &id);
    id
}

/// 定位 EIP CLI 二进制(按平台选 `.exe` / ELF)。Linux 下确保有执行权限。
fn eip_bin_path() -> Result<PathBuf, String> {
    super::platform::eip_bin_path()
}

// ──────────────────────────── 子进程封装 ────────────────────────────

/// 构造 EIP CLI 命令并注入运行环境。
fn base_cmd() -> Result<Command, String> {
    let bin = eip_bin_path()?;
    let mut c = Command::new(&bin);
    c.env("AGENT_DEVICE_ID", device_id());
    c.env("AGENT_CREDENTIALS_DIR", credentials_dir());
    // 非交互:CLI 不弹本地交互提示,未认证时直接输出 SSO URL 让我们编排。
    c.env("AGENT_NON_INTERACTIVE", "1");
    // EIP/SSO 是内网服务,不能继承 Clash/VPN 代理环境；否则可能打到外部出口后被网关拒绝。
    for k in [
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "http_proxy",
        "https_proxy",
        "all_proxy",
    ] {
        c.env_remove(k);
    }
    c.env("NO_PROXY", "*");
    c.env("no_proxy", "*");
    crate::platform::process::hide_std_console(&mut c);
    Ok(c)
}

/// `eip-cli <args>`(已注入环境)。
fn eip(args: &[&str]) -> Result<Command, String> {
    let mut c = base_cmd()?;
    c.args(args);
    Ok(c)
}

/// 跑命令,收 (success, exit_code, stdout, stderr)。`exit_code==3` = 需登录。
fn run(mut cmd: Command) -> Result<(bool, i32, String, String), String> {
    let out = cmd
        .output()
        .map_err(|e| format!("启动 eip-cli 失败: {e}"))?;
    Ok((
        out.status.success(),
        out.status.code().unwrap_or(-1),
        String::from_utf8_lossy(&out.stdout).into_owned(),
        String::from_utf8_lossy(&out.stderr).into_owned(),
    ))
}

/// 从一段输出里抓第一个 JSON 对象(CLI 可能夹带提示行)。
fn parse_json(s: &str) -> Option<Value> {
    let start = s.find('{')?;
    let end = s.rfind('}')?;
    if end < start {
        return None;
    }
    serde_json::from_str(&s[start..=end]).ok()
}

/// `auth status` 是否已有有效 token(已认证)。
fn is_authed() -> bool {
    if let Ok(cmd) = eip(&["auth", "status", "--output", "json"]) {
        if let Ok((_ok, _code, so, se)) = run(cmd) {
            let p = parse_json(&so).or_else(|| parse_json(&se));
            return p
                .as_ref()
                .and_then(|v| v.get("hasToken"))
                .and_then(|v| v.as_bool())
                .unwrap_or(false);
        }
    }
    false
}

// ──────────────────────────── 连接标记 / 技能门控 ────────────────────────────

/// 连接态标记文件:连上写、登出删。用作**技能门控信号**——非 spawn `auth status`,
/// 故启动门控判定零子进程,且非 EIP 用户(无凭证/从不连接)天然为 false。
fn connected_flag() -> PathBuf {
    eip_home().join("connected.flag")
}

/// EIP 技能此刻该不该对模型可见:本机存在连接标记(已连接过且未登出)。
/// `bundle.rs::ensure_extracted` 启动时用它决定放 / 删 `eip/SKILL.md`。
pub fn eip_skills_should_show() -> bool {
    connected_flag().is_file()
}

/// 置连接标记并请求技能可见性变更。空闲时立即写/删 `eip/SKILL.md`；
/// 有活跃 turn 时延迟到边界应用，保证配置只影响下一个 turn。
fn set_connected(v: bool) -> bool {
    let previous = eip_skills_should_show();
    let p = connected_flag();
    if previous != v {
        if v {
            let _ = std::fs::create_dir_all(eip_home());
            let _ = std::fs::write(&p, b"1");
        } else {
            let _ = std::fs::remove_file(&p);
        }
    }
    let skill_visible = crate::features::runtime_bundle::platform::Pinvou3Bundle::paths()
        .skills_dir
        .join("eip")
        .join("SKILL.md")
        .is_file();
    let deferred = (previous != v || skill_visible != v)
        && crate::features::connectors::visibility::request(
            crate::features::connectors::visibility::ConnectorKind::Eip,
            v,
        );
    if deferred {
        log::info!("[eip] skill visibility queued for the next turn connected={v}");
    }
    previous != v
}

// ──────────────────────────── 连接编排状态 ────────────────────────────

/// 连接编排共享状态:SSO 登录流程的单飞门闩(见 flow_gate 模块注释——快速重开
/// 连接界面时复用进行中流程,避免第二次 `auth login` 撞服务端 429 限流)。
/// `lib.rs` 用 `.manage(EipConn::default())` 注册。
#[derive(Default)]
pub struct EipConn {
    flow: super::flow_gate::SsoFlow,
}

fn emit(app: &AppHandle, event: &str, payload: Value) {
    let _ = app.emit(event, payload);
}

// ───────────────────────────── Tauri commands ─────────────────────────────

/// 查询 EIP 连接状态:`auth status --output json`。connected = 有有效 token。
/// 只读 CLI 凭证态,但会把结果对账进持久连接标记(自愈后端标记漂移;技能目录
/// 与 Engine 回收不动——可见性变更经 connector_visibility 应用:空闲时立即,
/// 有活跃 turn 时延至回合边界)。
pub async fn eip_status() -> Result<Value, String> {
    tokio::task::spawn_blocking(|| {
        let cmd = eip(&["auth", "status", "--output", "json"])?;
        let (_ok, _code, so, se) = run(cmd)?;
        let p = parse_json(&so).or_else(|| parse_json(&se));
        let connected = p
            .as_ref()
            .and_then(|v| v.get("hasToken"))
            .and_then(|v| v.as_bool())
            .unwrap_or(false);
        // Reconcile durable visibility without rebuilding or interrupting an
        // active Engine. connector_visibility applies it at the turn boundary.
        let _ = set_connected(connected);
        // 只回 connected:auth status 的 raw/stderr 可能含 token,不进 webview
        Ok::<Value, String>(json!({ "connected": connected }))
    })
    .await
    .map_err(|e| format!("spawn_blocking: {e}"))?
}

/// 开始连接 EIP(SSO 轮询自动收):
/// ① `auth login --no-poll --output json` 拿 SSO URL → emit `eip:sso`(前端引导浏览器登录)。
/// ② 循环 `auth poll` 直到认证成功 / 超时(5min)/ 取消 → emit `eip:connected` / `eip:error`。
/// 单飞:已有未取消的流程在跑时重放缓存的 SSO 载荷(不二次 `auth login`,防 429);
/// 旧流程取消中尚未退出时等它归还门闩。返回 `{started:true}`,前端 listen 事件驱动 UI。
pub async fn eip_connect_begin(app: AppHandle) -> Result<Value, String> {
    let flow = &app.state::<EipConn>().flow;
    // 旧流程取消中:等它归还门闩再起步,避免并发双流程。等待上限按最坏取消节拍
    // 推导(见 flow_gate::CANCEL_WAIT_LIMIT);真卡死的流程由 begin() 的超时抢占自愈。
    //
    // 等待期间用户仍可能点"取消",而那时旧流程已是 Cancelling(再取消是 no-op)、
    // 新流程又还没起步(无人可取消)——只有取消监票能记下这个意图。不查它的话,
    // 旧流程一归还门闩,等待循环就会照常起步,把用户刚取消的弹窗重新弹出来。
    let wait_start = Instant::now();
    let ticket = flow.cancel_ticket();
    let mut announced = false;
    let decision = loop {
        if flow.cancelled_since(ticket) {
            return Ok(json!({ "started": false, "cancelled": true }));
        }
        match flow.begin() {
            Begin::Busy => {
                if wait_start.elapsed() > CANCEL_WAIT_LIMIT {
                    // **不是连接失败**:旧流程可能只是这一拍特别慢(`auth poll` 最坏
                    // 90s),稍后大概率自己连上。回结构化 busy 让前端提示"稍后重试",
                    // 别报连接失败。
                    return Ok(json!({ "started": false, "busy": true }));
                }
                // 真等起来了才提示(旧流程通常一拍内退出,避免闪一下)。
                if !announced && wait_start.elapsed() > Duration::from_secs(1) {
                    announced = true;
                    emit(
                        &app,
                        "eip:sso_wait",
                        json!({ "seconds": 0, "previous_flow": true }),
                    );
                }
                tokio::time::sleep(Duration::from_millis(250)).await;
            }
            d => break d,
        }
    };
    match decision {
        Begin::Start { epoch } => {
            let app2 = app.clone();
            tokio::task::spawn_blocking(move || run_login_flow(&app2, epoch));
            Ok(json!({ "started": true }))
        }
        Begin::Reuse { payload } => {
            match payload {
                Some((url, qr)) => emit(&app, "eip:sso", json!({ "url": url, "qr_data_url": qr })),
                // 流程在跑但 URL 还没拿到(可能正卡在 65s 节流窗口里):在途的 eip:sso
                // 随后会送达,**但**这次重连若晚于那个事件发出,用户会面对一个毫无
                // 反馈的忙碌按钮。给一个"生成中"的等待态(seconds=0 → 前端显示
                // generating),让弹窗(连同取消按钮)先出来。
                None => emit(&app, "eip:sso_wait", json!({ "seconds": 0 })),
            }
            Ok(json!({ "started": true, "reused": true }))
        }
        Begin::Busy => unreachable!("Busy 已在上面的等待循环处理"),
    }
}

/// 登录轮询总预算:用户扫码的时间窗。
const FLOW_TIMEOUT: Duration = Duration::from_secs(300);

/// 轮询节拍间隔。
const POLL_TICK: Duration = Duration::from_secs(3);

/// 登录流程体。`epoch` 是 [`eip_connect_begin`] 从门闩拿到的持有权凭证。
fn run_login_flow(app: &AppHandle, epoch: u64) {
    let state = app.state::<EipConn>();
    let flow = &state.flow;
    // 门闩守卫:正常退出、提前 return、panic 都会归还门闩。流程体的 JoinHandle
    // 在上面的 spawn_blocking 里被丢弃,没有别的收尾者——靠流程自己逐条 return
    // 前 `finish()` 迟早会漏,漏一次门闩就永久压在 Running。
    let _guard = FlowGuard::new(flow, epoch);

    let url = match get_sso_url(flow, epoch, app) {
        Ok(Some(url)) => url,
        Ok(None) => return, // 节流等待中被取消/抢占:静默退出
        Err(e) => {
            // 已被取消/抢占就静默退场:僵尸流程给新流程的 UI 发 error 会误导用户
            // (见 flow_gate 模块注释"僵尸不再发任何事件")。
            if !flow.is_active(epoch) {
                return;
            }
            emit(
                app,
                "eip:error",
                json!({ "code": "sso_start_failed", "message": e }),
            );
            return;
        }
    };
    let qr = make_qr(&url);
    // 取消/抢占可能正落在 begin 与出码之间(节流窗口把这段从 ~2s 拉到 ~65s),
    // 那时不能再把用户刚收起的弹窗重新打开。
    if !flow.is_active(epoch) {
        return;
    }
    flow.arm_payload(epoch, url.clone(), qr.clone());
    emit(app, "eip:sso", json!({ "url": url, "qr_data_url": qr }));

    let start = Instant::now();
    loop {
        // 每拍自检 + 心跳:取消、被抢占(epoch 过期 = 门闩已被转交新流程)都从这里
        // 静默退场,不再发任何事件。
        if !flow.is_active(epoch) {
            return;
        }
        if start.elapsed() > FLOW_TIMEOUT {
            // 同源守卫:检查与 emit 之间若被取消/抢占,同样静默退场。
            if !flow.is_active(epoch) {
                return;
            }
            emit(
                app,
                "eip:error",
                json!({
                    "code": "sso_timeout",
                    "message": "登录超时(5 分钟内未完成)"
                }),
            );
            return;
        }
        std::thread::sleep(POLL_TICK);
        // 心跳贴着阻塞调用放:这样"卡死"只需由**单次**调用的最坏耗时界定
        // (HTTP 客户端 60s),而不是整拍。
        if !flow.is_active(epoch) {
            return;
        }
        // auth poll 一轮(自带 30s 轮询窗口);成功与否统一靠 auth status 判定。
        if let Ok(cmd) = eip(&["auth", "poll", "--output", "json"]) {
            let _ = run(cmd);
        }
        if !flow.is_active(epoch) {
            return;
        }
        if is_authed() {
            if set_connected(true) {
                log::info!(
                    "[eip] connected state changed; visibility will apply without evicting active turns"
                );
            }
            emit(app, "eip:connected", json!({ "ok": true }));
            return;
        }
    }
}

/// `auth login --no-poll --output json` → 取 SSO 登录地址(只出 URL 不阻塞)。
/// 返回 `Ok(None)` = 节流等待期间被取消/抢占,本次登录作废。
fn get_sso_url(flow: &SsoFlow, epoch: u64, app: &AppHandle) -> Result<Option<String>, String> {
    let cmd = eip(&["auth", "login", "--no-poll", "--output", "json"])?;
    // 同源网关限流:与知道的 login 至少间隔 65s。等待期间给前端出进度,可被取消。
    let proceed = super::flow_gate::throttle_sso_login(
        |wait| {
            emit(
                app,
                "eip:sso_wait",
                json!({ "seconds": wait.as_secs() + 1 }),
            )
        },
        || !flow.is_active(epoch),
    );
    if !proceed {
        return Ok(None);
    }
    let (_ok, _code, so, se) = run(cmd)?;
    let p = parse_json(&so)
        .or_else(|| parse_json(&se))
        .unwrap_or(Value::Null);
    ["ssoLoginUrl", "ssoUrl", "url"]
        .iter()
        .find_map(|k| p.get(*k).and_then(|v| v.as_str()))
        .map(String::from)
        .map(Some)
        .ok_or_else(|| {
            let message = p
                .get("message")
                .and_then(|v| v.as_str())
                .or_else(|| p.get("hint").and_then(|v| v.as_str()))
                .unwrap_or("auth login 未返回 SSO 登录地址");
            format!("auth login 未返回 SSO 登录地址: {message}")
        })
}

/// 取消连接:请求门闩取消(登录轮询下一拍自停并归还门闩)。
pub async fn eip_cancel(app: AppHandle) -> Result<Value, String> {
    app.state::<EipConn>().flow.request_cancel();
    Ok(json!({ "ok": true }))
}

/// 断开 EIP:`auth logout`(清凭证)。
pub async fn eip_logout() -> Result<Value, String> {
    tokio::task::spawn_blocking(|| {
        let cmd = eip(&["auth", "logout"])?;
        let (ok, _code, so, se) = run(cmd)?;
        if ok {
            let _ = set_connected(false); // 下一个 turn 生效，不驱逐活跃 Engine
        }
        Ok::<Value, String>(json!({ "ok": ok, "stdout": so, "stderr": se }))
    })
    .await
    .map_err(|e| format!("spawn_blocking: {e}"))?
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;
    use std::path::Path;

    use super::make_qr;

    #[test]
    fn make_qr_returns_svg_data_url() {
        let qr = make_qr("https://sso.h3c.com/login?sessionId=test-session").unwrap();
        assert!(qr.starts_with("data:image/svg+xml;base64,"));
        assert!(qr.len() > 512);
    }

    /// 从 SKILL.md 命令速查表提取公开命令键（与 linux-src routes 同一口径：
    /// `eip attendance get-summary` → "attendance get-summary"，`eip news` → "news"；
    /// 同格续写命令如 `get-sick-balance` 继承本行首个 eip 命令的域）。
    fn collect_skill_commands(skill: &str) -> BTreeSet<String> {
        let mut out = BTreeSet::new();
        for line in skill.lines() {
            if !line.trim_start().starts_with('|') {
                continue;
            }
            let mut domain = String::new();
            for span in extract_code_spans(line) {
                if let Some(rest) = span.strip_prefix("eip ") {
                    let tokens: Vec<&str> = rest.split_whitespace().collect();
                    if tokens.is_empty() {
                        continue;
                    }
                    let key = if tokens.len() > 1
                        && tokens[1].starts_with(|c: char| c.is_ascii_lowercase())
                    {
                        format!("{} {}", tokens[0], tokens[1])
                    } else {
                        tokens[0].trim_end_matches('[').to_string()
                    };
                    domain = key.split(' ').next().unwrap_or("").to_string();
                    out.insert(key);
                } else if !domain.is_empty()
                    && !span.is_empty()
                    && span
                        .chars()
                        .all(|c| c.is_ascii_lowercase() || c == '-' || c == '[')
                {
                    out.insert(format!("{domain} {}", span.trim_end_matches('[')));
                }
            }
        }
        out
    }

    /// 提取一行内所有反引号代码段。
    fn extract_code_spans(line: &str) -> Vec<String> {
        let mut spans = Vec::new();
        let mut rest = line;
        while let Some(start) = rest.find('`') {
            let after = &rest[start + 1..];
            if let Some(end) = after.find('`') {
                spans.push(after[..end].to_string());
                rest = &after[end + 1..];
            } else {
                break;
            }
        }
        spans
    }

    /// 从 linux-src/main.go 的 routes 表提取命令键（形如 `"key": {"METHOD", ...}`）。
    fn collect_fallback_routes(source: &str) -> BTreeSet<String> {
        source
            .lines()
            .filter_map(|line| {
                let t = line.trim_start();
                let rest = t.strip_prefix('"')?;
                let end = rest.find('"')?;
                let key = &rest[..end];
                let after = rest[end + 1..].trim_start().strip_prefix(':')?.trim_start();
                if after.starts_with("{\"")
                    && key
                        .chars()
                        .all(|c| c.is_ascii_lowercase() || c == ' ' || c == '-')
                {
                    Some(key.to_string())
                } else {
                    None
                }
            })
            .collect()
    }

    #[test]
    fn skill_documented_commands_exist_in_fallback_routes() {
        // SKILL.md 命令速查表是模型在全部平台调用 eip 的契约，darwin/linux-arm64
        // 实际执行 linux-src fallback 构建：路由缺命令时公开命令稳定失败
        // （#1057 评审发现 news/announcement/course/meeting/community-hot 缺失）。
        // 契约：文档公开命令必须存在于 fallback 路由，防文档与能力再漂移。
        let base = Path::new(env!("CARGO_MANIFEST_DIR")).join("resources/common/bundle/eip");
        let skill = std::fs::read_to_string(base.join("SKILL.md")).unwrap();
        let source = std::fs::read_to_string(base.join("linux-src/main.go")).unwrap();
        let routes = collect_fallback_routes(&source);
        assert!(
            routes.contains("attendance get-summary") && routes.contains("news"),
            "fallback 路由解析异常，得到 {} 条",
            routes.len()
        );
        let documented = collect_skill_commands(&skill);
        assert!(
            documented.contains("news") && documented.contains("attendance get-summary"),
            "SKILL.md 命令解析异常，得到 {documented:?}"
        );
        let missing: Vec<String> = documented.difference(&routes).cloned().collect();
        assert!(
            missing.is_empty(),
            "SKILL.md 公开命令在 fallback 路由缺失: {missing:?}"
        );
    }
}
