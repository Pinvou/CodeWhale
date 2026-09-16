//! H3C 知道知识库接入 —— 连接生命周期 + 鉴权编排。
//!
//! 复用 IT 提供的 `zhidao` CLI。连接流程与 EIP 同源 SSO_NEXT。当前 zhidao CLI
//! 只提供 `login/save/load/exchange`,没有 poll 子命令;但底层凭证格式和 EIP CLI
//! 相同,因此 Pinvou 用 `zhidao login` 拿 SSO URL,再借 EIP CLI 的 `auth poll`
//! 收 token/AIT 到 zhidao 凭证目录,实现和 EIP 一样的扫码自动连接体验。

use std::path::PathBuf;
use std::process::Command;
use std::time::{Duration, Instant};

use super::flow_gate::{Begin, CANCEL_WAIT_LIMIT, FlowGuard, SsoFlow};

use qrcode::{QrCode, render::svg};
use serde_json::{Value, json};
use tauri::{AppHandle, Emitter, Manager};

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

fn zhidao_home() -> PathBuf {
    crate::platform::paths::pinvou3_home().join("zhidao")
}

fn credentials_dir() -> PathBuf {
    let d = zhidao_home().join("credentials");
    let _ = std::fs::create_dir_all(&d);
    d
}

fn device_id() -> String {
    let p = zhidao_home().join("device_id");
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
    let _ = std::fs::create_dir_all(zhidao_home());
    let _ = std::fs::write(&p, &id);
    id
}

fn zhidao_bin_path() -> Result<PathBuf, String> {
    super::platform::zhidao_bin_path()
}

// ──────────────────────────── 子进程封装 ────────────────────────────

/// 构造 知道 CLI 命令并注入运行环境。
fn base_cmd() -> Result<Command, String> {
    let mut c = Command::new(zhidao_bin_path()?);
    c.env("AGENT_DEVICE_ID", device_id());
    c.env("AGENT_CREDENTIALS_DIR", credentials_dir());
    c.env("AGENT_NON_INTERACTIVE", "1");
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

fn zhidao(args: &[&str]) -> Result<Command, String> {
    let mut c = base_cmd()?;
    c.args(args);
    Ok(c)
}

/// 跑命令,收 (success, exit_code, stdout, stderr)。带超时兜底:CLI 真挂死时
/// kill 并收尸,而不是无限阻塞 spawn_blocking 线程(issue #1097:僵尸流程资源
/// 不回收)。
fn run(cmd: Command) -> Result<(bool, i32, String, String), String> {
    super::connector_cli::run_captured_with_timeout(
        cmd,
        "zhidao",
        super::connector_cli::CLI_RUN_TIMEOUT,
    )
}

fn parse_json(s: &str) -> Option<Value> {
    let start = s.find('{')?;
    let end = s.rfind('}')?;
    if end < start {
        return None;
    }
    serde_json::from_str(&s[start..=end]).ok()
}

fn is_authed() -> bool {
    matches!(run(match zhidao(&["load"]) { Ok(c) => c, Err(_) => return false }), Ok((true, _, so, _)) if so.contains("ZHIDAO_TOKEN"))
}

fn connected_flag() -> PathBuf {
    zhidao_home().join("connected.flag")
}

pub fn zhidao_skills_should_show() -> bool {
    connected_flag().is_file()
}

fn set_connected(v: bool) -> bool {
    let previous = zhidao_skills_should_show();
    let p = connected_flag();
    if previous != v {
        if v {
            let _ = std::fs::create_dir_all(zhidao_home());
            let _ = std::fs::write(&p, b"1");
        } else {
            let _ = std::fs::remove_file(&p);
        }
    }
    let skill_visible = crate::features::runtime_bundle::platform::Pinvou3Bundle::paths()
        .skills_dir
        .join("zhidao")
        .join("SKILL.md")
        .is_file();
    let deferred = (previous != v || skill_visible != v)
        && crate::features::connectors::visibility::request(
            crate::features::connectors::visibility::ConnectorKind::Zhidao,
            v,
        );
    if deferred {
        log::info!("[zhidao] skill visibility queued for the next turn connected={v}");
    }
    previous != v
}

#[derive(Default)]
pub struct ZhidaoConn {
    flow: super::flow_gate::SsoFlow,
}

fn emit(app: &AppHandle, event: &str, payload: Value) {
    let _ = app.emit(event, payload);
}
pub async fn zhidao_status() -> Result<Value, String> {
    tokio::task::spawn_blocking(|| {
        let cmd = zhidao(&["load"])?;
        let (ok, _code, so, _se) = run(cmd)?;
        let connected = ok && so.contains("ZHIDAO_TOKEN");
        // 状态查询只更新期望可见性；活跃 turn 自然结束后再落技能文件。
        let _ = set_connected(connected);
        // 只回 connected:load 的 stdout 就是 ZHIDAO_TOKEN 本体,不能进 webview
        Ok::<Value, String>(json!({ "connected": connected }))
    })
    .await
    .map_err(|e| format!("spawn_blocking: {e}"))?
}
pub async fn zhidao_connect_begin(app: AppHandle) -> Result<Value, String> {
    let flow = &app.state::<ZhidaoConn>().flow;
    // 与 eip_connect_begin 同一套单飞语义:进行中复用、取消中等待归还门闩。
    // 等待上限与抢占自愈同样见 flow_gate(CANCEL_WAIT_LIMIT / STALL_LIMIT)。
    // 取消监票同 eip:等待期间用户的"取消"必须能中止本次起步。
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
                    // 不是连接失败:旧流程可能只是这一拍特别慢,稍后大概率自己连上。
                    return Ok(json!({ "started": false, "busy": true }));
                }
                if !announced && wait_start.elapsed() > Duration::from_secs(1) {
                    announced = true;
                    emit(
                        &app,
                        "zhidao:sso_wait",
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
                Some((url, qr)) => {
                    emit(&app, "zhidao:sso", json!({ "url": url, "qr_data_url": qr }))
                }
                // 同 eip:URL 还没拿到时先给个"生成中"等待态,别留无反馈的忙碌按钮。
                None => emit(&app, "zhidao:sso_wait", json!({ "seconds": 0 })),
            }
            Ok(json!({ "started": true, "reused": true }))
        }
        Begin::Busy => unreachable!("Busy 已在上面的等待循环处理"),
    }
}

/// 登录流程体。`epoch` 是 [`zhidao_connect_begin`] 从门闩拿到的持有权凭证。
fn run_login_flow(app: &AppHandle, epoch: u64) {
    let state = app.state::<ZhidaoConn>();
    let flow = &state.flow;
    // 门闩守卫:正常退出、提前 return、panic 都会归还门闩(同一根因见 eip.rs)。
    let _guard = FlowGuard::new(flow, epoch);

    let login = match get_sso_login(flow, epoch, app) {
        Ok(Some(login)) => login,
        Ok(None) => return, // 节流等待中被取消/抢占:静默退出
        Err(e) => {
            // 已被取消/抢占就静默退场(同 eip:僵尸不发事件)。
            if !flow.is_active(epoch) {
                return;
            }
            emit(
                app,
                "zhidao:error",
                json!({ "code": "sso_start_failed", "message": e }),
            );
            return;
        }
    };
    let qr = make_qr(&login.url);
    // 取消/抢占可能正落在 begin 与出码之间,那时不能再把刚收起的弹窗重新打开。
    if !flow.is_active(epoch) {
        return;
    }
    flow.arm_payload(epoch, login.url.clone(), qr.clone());
    emit(
        app,
        "zhidao:sso",
        json!({ "url": login.url, "qr_data_url": qr }),
    );

    let deadline = Instant::now() + Duration::from_secs(300);
    loop {
        // 每拍自检 + 心跳:取消、被抢占都从这里静默退场,不再发任何事件。
        if !flow.is_active(epoch) {
            return;
        }
        if Instant::now() >= deadline {
            break;
        }
        // 心跳贴着阻塞调用放:"卡死"只需由单次调用的最坏耗时界定。
        if !flow.is_active(epoch) {
            return;
        }
        // 用 zhidao-cli 自己的 poll(复用 H3C session/poll 端点)收 token/AIT 到 zhidao 凭证目录。
        // 成功即由本 CLI 用同一套加密落盘,`load` 随后能读到 —— 不再依赖 eip-cli 的凭证格式。
        if let Ok(cmd) = zhidao(&["poll", "--session-id", &login.session_id, "--timeout", "5"]) {
            let _ = run(cmd);
        }
        if !flow.is_active(epoch) {
            return;
        }
        if is_authed() {
            if set_connected(true) {
                log::info!(
                    "[zhidao] connected state changed; visibility will apply without evicting active turns"
                );
            }
            emit(app, "zhidao:connected", json!({ "ok": true }));
            return;
        }
    }
    // 同源守卫:检查与 emit 之间若被取消/抢占,静默退场。
    if !flow.is_active(epoch) {
        return;
    }
    emit(
        app,
        "zhidao:error",
        json!({
            "code": "sso_timeout",
            "message": "知道 SSO 认证超时，请重新连接"
        }),
    );
}

struct SsoLogin {
    url: String,
    session_id: String,
}

/// `zhidao login` → SSO 登录地址 + sessionId。
/// 返回 `Ok(None)` = 节流等待期间被取消/抢占,本次登录作废。
fn get_sso_login(flow: &SsoFlow, epoch: u64, app: &AppHandle) -> Result<Option<SsoLogin>, String> {
    let cmd = zhidao(&["login"])?;
    // 同源网关限流:与 EIP 的 auth login 至少间隔 65s。等待期间出**可递减**的
    // 倒计时(整秒续报),可被取消。
    let permit = super::flow_gate::throttle_sso_login(
        |wait| {
            emit(
                app,
                "zhidao:sso_wait",
                json!({ "seconds": wait.as_secs() + 1 }),
            )
        },
        || !flow.is_active(epoch),
    );
    let Some(permit) = permit else {
        return Ok(None);
    };
    // 放行 ≠ 请求已发出:Err(spawn 失败或 120s 兜底击杀)丢弃凭证自动回滚,
    // 不白占 65s 窗口;Ok 时无论退出码如何,confirm 保留窗口(同 eip.rs,含
    // 超时回滚的取舍说明)。
    let out = run(cmd);
    if out.is_ok() {
        permit.confirm();
    }
    let (_ok, _code, so, se) = out?;
    let p = parse_json(&so)
        .or_else(|| parse_json(&se))
        .unwrap_or(Value::Null);
    let url = ["ssoLoginUrl", "ssoUrl", "url"]
        .iter()
        .find_map(|k| p.get(*k).and_then(|v| v.as_str()))
        .or_else(|| {
            p.get("data").and_then(|d| {
                ["ssoLoginUrl", "ssoUrl", "url"]
                    .iter()
                    .find_map(|k| d.get(*k).and_then(|v| v.as_str()))
            })
        })
        .map(String::from)
        .ok_or_else(|| {
            let message = p
                .get("message")
                .and_then(|v| v.as_str())
                .or_else(|| p.get("msg").and_then(|v| v.as_str()))
                .unwrap_or("zhidao login 未返回 SSO 登录地址");
            format!("zhidao login 未返回 SSO 登录地址: {message}")
        })?;
    let session_id = ["sessionId", "session_id", "state"]
        .iter()
        .find_map(|k| p.get(*k).and_then(|v| v.as_str()))
        .or_else(|| {
            p.get("data").and_then(|d| {
                ["sessionId", "session_id", "state"]
                    .iter()
                    .find_map(|k| d.get(*k).and_then(|v| v.as_str()))
            })
        })
        .map(str::to_string)
        .or_else(|| {
            url.split("state=")
                .nth(1)
                .and_then(|s| s.split('&').next())
                .map(str::to_string)
        })
        .ok_or_else(|| "zhidao login 未返回 sessionId/state,无法自动轮询".to_string())?;
    Ok(Some(SsoLogin {
        url: url.to_string(),
        session_id,
    }))
}
pub async fn zhidao_cancel(app: AppHandle) -> Result<Value, String> {
    app.state::<ZhidaoConn>().flow.request_cancel();
    Ok(json!({ "ok": true }))
}
pub async fn zhidao_logout() -> Result<Value, String> {
    tokio::task::spawn_blocking(|| {
        let cmd = zhidao(&["clear"])?;
        let (ok, _code, so, se) = run(cmd)?;
        if ok {
            let _ = set_connected(false);
        }
        Ok::<Value, String>(json!({ "ok": ok, "stdout": so, "stderr": se }))
    })
    .await
    .map_err(|e| format!("spawn_blocking: {e}"))?
}

#[cfg(test)]
mod tests {
    use super::make_qr;

    #[test]
    fn make_qr_returns_svg_data_url() {
        let qr = make_qr("https://sso.h3c.com/login?sessionId=test-session").unwrap();
        assert!(qr.starts_with("data:image/svg+xml;base64,"));
        assert!(qr.len() > 512);
    }

    #[test]
    fn skill_documented_subcommands_exist_in_win_src_routes() {
        // SKILL.md「调用方式」公开 qa/search/download 三个业务子命令；darwin 与
        // linux-arm64 实际执行 win-src fallback 构建（同 PR #1057 对 EIP 的评审
        // 发现）：公开子命令必须存在于 fallback 路由，防文档与能力漂移。
        let src = std::fs::read_to_string(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/resources/common/bundle/zhidao/win-src/main.go"
        ))
        .unwrap();
        for cmd in ["qa", "search", "download"] {
            assert!(
                src.contains(&format!("case \"{cmd}\":")),
                "win-src fallback 路由缺 SKILL.md 公开子命令 {cmd}"
            );
        }
    }
}
