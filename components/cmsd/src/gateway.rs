//! 薄网关（T6，D0）：127.0.0.1:8000 固定端点。
//!
//! 转发到活跃模型真实端口（W6：活跃指针指向容器卡时代理到它的 host_port，
//! 机制无差别）；model 字段策略（2026-10-13 用户拍板真名化）：
//! 别名 `megacube-default` 改写成活跃模型真实 id（存量配置/脚本终身有效）。
//! 真名归属路由（2026-10-14 用户拍板「行保留，没服务要诚实报」）：
//! 请求的模型名属于某张登记卡时——在跑的供应卡（活跃卡优先）收流量；
//! 全部供应卡停止 → 网关诚实 503（不再发活跃卡顶替：SGLang 不校验 model
//! 字段，发过去就是静默换模型，违反「不静默换模型」原则）；不属于任何
//! 登记卡的名字 → 维持发活跃卡（由引擎校验，驭光会回诚实 404）。
//! 无活跃模型返回结构化 503。响应用 connection-close 语义直通，SSE 穿透。

use crate::app::{log_line, App};
use crate::http;
use crate::json::Value;
use crate::store::ModelStatus;
use std::io::{BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::Arc;
use std::time::{Duration, Instant};

const RETRY_MS: u64 = 5000;

pub struct Runtime {
    phase: &'static str,
    bound: Option<String>,
    error: Option<&'static str>,
    attempts: u64,
    changed_at: u64,
    retry: bool,
    retry_at: Option<Instant>,
}
impl Default for Runtime {
    fn default() -> Self {
        Self {
            phase: "starting",
            bound: None,
            error: None,
            attempts: 0,
            changed_at: crate::timeutil::now_epoch(),
            retry: false,
            retry_at: None,
        }
    }
}
pub fn status(app: &App) -> Value {
    let runtime = app
        .gateway_runtime
        .lock()
        .unwrap_or_else(|error| error.into_inner());
    let mut value = Value::obj();
    value.set("version", 1u64.into());
    value.set("phase", runtime.phase.into());
    value.set("ready", (runtime.phase == "listening").into());
    value.set(
        "configured_listen",
        app.config.gateway_listen.clone().into(),
    );
    value.set(
        "bound_listen",
        runtime
            .bound
            .clone()
            .map(Value::from)
            .unwrap_or(Value::Null),
    );
    value.set("bind_attempts", runtime.attempts.into());
    value.set("changed_at", runtime.changed_at.into());
    value.set(
        "retry_after_ms",
        runtime
            .retry_at
            .map(|deadline| {
                (deadline
                    .saturating_duration_since(Instant::now())
                    .as_millis() as u64)
                    .into()
            })
            .unwrap_or(Value::Null),
    );
    value.set(
        "retry_interval_ms",
        if runtime.retry {
            RETRY_MS.into()
        } else {
            Value::Null
        },
    );
    value.set("upstream_health_verified", false.into());
    value.set(
        "error",
        runtime
            .error
            .map(|code| {
                let mut error = Value::obj();
                error.set("code", code.into());
                error.set("retryable", runtime.retry.into());
                error.set(
                    "message",
                    if runtime.retry {
                        "推理网关未监听，管理接口继续可用；保持占用方不变，五秒后重试配置地址"
                    } else {
                        "推理网关工作线程已退出，管理接口继续可用；核实日志后从组件入口重启cmsd"
                    }
                    .into(),
                );
                error
            })
            .unwrap_or(Value::Null),
    );
    value.set(
        "note",
        "ready仅表示本进程持有监听，不证明活跃模型、上游健康或推理成功；旧gateway字段仅为配置地址"
            .into(),
    );
    value
}
fn failed(app: &App, code: &'static str, retry: bool) {
    let mut runtime = app
        .gateway_runtime
        .lock()
        .unwrap_or_else(|error| error.into_inner());
    let changed = runtime.error != Some(code);
    runtime.phase = "unavailable";
    runtime.bound = None;
    runtime.error = Some(code);
    runtime.changed_at = crate::timeutil::now_epoch();
    runtime.retry = retry;
    runtime.retry_at = retry.then(|| Instant::now() + Duration::from_millis(RETRY_MS));
    drop(runtime);
    if changed {
        log_line(&format!(
            "推理网关不可用：{code}；管理接口保留，不接管端口占用方"
        ));
    }
}

pub fn run(app: Arc<App>) {
    struct Worker(Arc<App>);
    impl Drop for Worker {
        fn drop(&mut self) {
            failed(&self.0, "gateway_worker_stopped", false);
        }
    }
    let _worker = Worker(app.clone());
    loop {
        {
            let mut runtime = app
                .gateway_runtime
                .lock()
                .unwrap_or_else(|error| error.into_inner());
            runtime.attempts = runtime
                .attempts
                .saturating_add(1)
                .min(crate::lifecycle_revision::MAX_REVISION);
        }
        let listener = match TcpListener::bind(&app.config.gateway_listen) {
            Ok(listener) => listener,
            Err(error) => {
                let code = match error.kind() {
                    std::io::ErrorKind::AddrInUse => "gateway_address_in_use",
                    std::io::ErrorKind::AddrNotAvailable => "gateway_address_not_available",
                    std::io::ErrorKind::PermissionDenied => "gateway_permission_denied",
                    _ => "gateway_bind_failed",
                };
                failed(&app, code, true);
                std::thread::sleep(Duration::from_millis(RETRY_MS));
                continue;
            }
        };
        let bound = match listener.local_addr() {
            Ok(address) => address.to_string(),
            Err(_) => {
                drop(listener);
                failed(&app, "gateway_local_address_unavailable", true);
                std::thread::sleep(Duration::from_millis(RETRY_MS));
                continue;
            }
        };
        {
            let mut runtime = app
                .gateway_runtime
                .lock()
                .unwrap_or_else(|error| error.into_inner());
            runtime.phase = "listening";
            runtime.bound = Some(bound.clone());
            runtime.error = None;
            runtime.changed_at = crate::timeutil::now_epoch();
            runtime.retry = false;
            runtime.retry_at = None;
        }
        log_line(&format!("网关实际监听 {bound}"));
        for stream in listener.incoming() {
            match stream {
                Ok(s) => {
                    let Some(permit) = app.gateway_connections.acquire() else {
                        let mut s = s;
                        let _ = s.set_write_timeout(Some(Duration::from_secs(1)));
                        let _ = http::write_response(&mut s, "503 Service Unavailable", "application/json", b"{\"error\":{\"code\":\"connection_limit\",\"message\":\"inference gateway busy\"}}", &[]);
                        continue;
                    };
                    let app2 = Arc::clone(&app);
                    if let Err(error) = std::thread::Builder::new()
                        .name("cmsd-gateway".into())
                        .spawn(move || {
                            let _permit = permit;
                            if let Err(e) = handle_conn(&app2, s) {
                                log_line(&format!("网关连接处理失败：{e}"));
                            }
                        })
                    {
                        log_line(&format!("网关连接线程未创建：{error}"));
                    }
                }
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                Err(_) => {
                    failed(&app, "gateway_accept_failed", true);
                    break;
                }
            }
        }
        drop(listener);
        std::thread::sleep(Duration::from_millis(RETRY_MS));
    }
}

fn handle_conn(app: &Arc<App>, stream: TcpStream) -> std::io::Result<()> {
    stream.set_read_timeout(Some(Duration::from_secs(3)))?;
    stream.set_write_timeout(Some(Duration::from_secs(60)))?;
    let mut downstream = stream;
    let mut reader = BufReader::new(http::DeadlineReader::new(
        downstream.try_clone()?,
        Duration::from_secs(10),
    ));
    let req = match http::read_request(&mut reader) {
        Ok(Some(req)) => req,
        Ok(None) => return Ok(()),
        Err(error) => return http::write_receive_error(&mut downstream, &error),
    };

    let selection = match crate::store_adoption::selection::cached(app) {
        Ok(value) => value,
        Err(_) => return http::write_response(&mut downstream,"503 Service Unavailable","application/json",
            br#"{"error":{"code":"store_selection_unavailable","message":"Current store deployment cannot be verified"}}"#,&[]),
    };
    let models: Vec<_> = app
        .store
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .state
        .models
        .values()
        .cloned()
        .collect();
    let verified: std::collections::BTreeSet<_> = models
        .into_iter()
        .filter(|model| {
            model.lifecycle_owned
                && model.port > 0
                && matches!(model.status, ModelStatus::Running | ModelStatus::Starting)
                && selection.routable(&model.card)
                && match model.engine_type.as_str() {
                    "container" => crate::container_runtime::routable(app, model),
                    "native" => crate::ops::find_card_raw(app, &model.card).is_ok_and(|card| {
                        crate::ops::native_instance_matches(
                            app,
                            &card,
                            model.port,
                            model.main_pid.unwrap_or(0),
                        )
                    }),
                    _ => false,
                }
        })
        .map(|model| (model.card, model.port, model.main_pid))
        .collect();
    let allowed = |model: &crate::store::ModelState| {
        verified.contains(&(model.card.clone(), model.port, model.main_pid))
    };
    let requested_name = model_name_of(&req.body);
    let (target, default_is_unset, named_claimed) = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        (
            target_for_request_filtered(&guard.state, requested_name.as_deref(), &allowed),
            target_for_request_filtered(&guard.state, None, &allowed).is_none(),
            requested_name.as_ref().is_some_and(|name| {
                guard
                    .state
                    .models
                    .values()
                    .any(|m| m.lifecycle_owned && &m.model_id == name)
            }),
        )
    };
    // Discovery must remain possible without implicitly selecting a default.
    if default_is_unset && req.method == "GET" && req.path == "/v1/models" {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let live = app.live.lock().unwrap_or_else(|e| e.into_inner());
        let mut ids = std::collections::BTreeSet::new();
        let mut data = Vec::new();
        for m in guard.state.models.values() {
            if m.lifecycle_owned
                && allowed(m)
                && m.status == ModelStatus::Running
                && m.port > 0
                && !m.model_id.is_empty()
                && live.health.get(&m.port).is_some_and(|h| h.ok)
                && ids.insert(m.model_id.clone())
            {
                let mut row = Value::obj();
                row.set("id", m.model_id.clone().into());
                row.set("object", "model".into());
                row.set("owned_by", "cmsd".into());
                if let Some(length) = live.health.get(&m.port).and_then(|h| h.model_len) {
                    row.set("max_model_len", length.into());
                }
                data.push(row);
            }
        }
        if !data.is_empty() {
            let mut value = Value::obj();
            value.set("object", "list".into());
            value.set("data", Value::Arr(data));
            return http::write_response(
                &mut downstream,
                "200 OK",
                "application/json",
                value.to_string().as_bytes(),
                &[],
            );
        }
    }
    if target.is_none() && named_claimed {
        return write_model_not_running(&mut downstream, requested_name.as_deref().unwrap());
    }
    let Some((port, real_model_id, card)) = target else {
        let body = Value::parse("{}").map(|mut v| {
            v.set("error", {
                let mut e = Value::obj();
                e.set("code", "no_active_model".into());
                e.set(
                    "message",
                    "当前没有活跃模型。请先通过 cmsd enable 一个模型（换胎期间会短暂出现此状态）"
                        .into(),
                );
                e.set("type", "service_unavailable".into());
                e
            });
            v
        });
        let body = body.map(|v| v.to_string()).unwrap_or_else(|_| {
            "{\"error\":{\"code\":\"no_active_model\",\"message\":\"当前没有活跃模型\"}}"
                .to_string()
        });
        http::write_response(
            &mut downstream,
            "503 Service Unavailable",
            "application/json",
            body.as_bytes(),
            &[],
        )?;
        return Ok(());
    };

    // model 字段改写：megacube-default（或任何别名）→ 真实模型 id
    let is_json = req
        .headers
        .iter()
        .find(|(key, _)| key == "content-type")
        .is_none_or(|(_, value)| {
            value
                .split(';')
                .next()
                .unwrap_or("")
                .trim()
                .eq_ignore_ascii_case("application/json")
        });
    let (body, requested) = if is_json {
        match rewrite_json_body(&req.body,&real_model_id) {
            Ok(value)=>value,
            Err(())=>return http::write_response(&mut downstream,"400 Bad Request","application/json",
                br#"{"error":{"code":"invalid_json","message":"invalid JSON, UTF-8 or excessive structure"}}"#,&[]),
        }
    } else {
        (req.body.clone(), None)
    };

    // 真名归属路由（2026-10-14 用户拍板「行保留，没服务要诚实报」）：
    // 请求的模型名有登记归属且全部供应卡停止 → 网关诚实 503（不静默顶替）；
    // 有在跑供应卡（活跃卡优先）→ 发那张卡；无归属 → 维持发活跃卡由引擎校验。
    let port = match requested.as_deref() {
        Some(name) if name != real_model_id.as_str() => {
            let (claimed, running_owner) = {
                let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
                let mut claimed = false;
                let mut running_owner: Option<u16> = None;
                for m in guard.state.models.values() {
                    if !m.lifecycle_owned || m.model_id != name {
                        continue;
                    }
                    claimed = true;
                    if running_owner.is_none()
                        && allowed(m)
                        && m.port > 0
                        && matches!(m.status, ModelStatus::Running | ModelStatus::Starting)
                    {
                        running_owner = Some(m.port);
                    }
                }
                (claimed, running_owner)
            };
            match route_model(requested.as_deref(), &real_model_id, claimed, running_owner) {
                ModelRoute::Active => port,
                ModelRoute::Owner { port: p, .. } => p,
                ModelRoute::NotRunning => {
                    write_model_not_running(&mut downstream, name)?;
                    return Ok(());
                }
            }
        }
        _ => port,
    };
    // 转发到真实端口
    let upstream = match TcpStream::connect_timeout(
        &format!("127.0.0.1:{port}")
            .parse()
            .map_err(|_| http::bad_input("地址非法"))?,
        Duration::from_secs(3),
    ) {
        Ok(s) => s,
        Err(e) => {
            let body = format!(
                "{{\"error\":{{\"code\":\"engine_unreachable\",\"message\":\"活跃模型 {card} 的引擎（127.0.0.1:{port}）连不上：{e}。可能正在重启，请稍后重试\"}}}}"
            );
            http::write_response(
                &mut downstream,
                "503 Service Unavailable",
                "application/json",
                body.as_bytes(),
                &[],
            )?;
            return Ok(());
        }
    };
    let mut up_read = forward_request(upstream, &req, &body, port, Duration::from_secs(60))?;

    // 响应头直通：上游的分帧方式（Content-Length 或 Transfer-Encoding: chunked）
    // 原样透传——剥掉分帧头会把 chunk 尺寸行泄漏给客户端（V6 流式实测踩过）。
    // 仅把 Connection 改成 close（我们不做连接复用）。
    let head = read_response_head(&mut up_read)?;
    write!(downstream, "{}\r\n", head.status_line)?;
    for (k, v) in &head.headers {
        if k.eq_ignore_ascii_case("connection") {
            continue;
        }
        write!(downstream, "{k}: {v}\r\n")?;
    }
    write!(downstream, "connection: close\r\n\r\n")?;
    downstream.flush()?;
    std::io::copy(&mut up_read, &mut downstream)?;
    downstream.flush()?;
    Ok(())
}

/// Send the complete request on one bounded writer before switching to the
/// independent long-lived inference response. A stalled upstream must release
/// the Request's shared body-buffer permit even while its peer stays open.
fn forward_request(
    mut upstream: TcpStream,
    req: &http::Request,
    body: &[u8],
    port: u16,
    write_timeout: Duration,
) -> std::io::Result<BufReader<TcpStream>> {
    upstream.set_read_timeout(Some(Duration::from_secs(600)))?;
    upstream.set_write_timeout(Some(write_timeout))?;
    // 请求行 + 头（Host/Content-Length/Connection 重写）+ 主体
    let target = match req.query.is_empty() {
        true => req.path.clone(),
        false => format!("{}?{}", req.path, req.query),
    };
    write!(upstream, "{} {} HTTP/1.1\r\n", req.method, target)?;
    write!(upstream, "host: 127.0.0.1:{port}\r\n")?;
    for (k, v) in &req.headers {
        if matches!(
            k.as_str(),
            "host" | "content-length" | "connection" | "transfer-encoding"
        ) {
            continue;
        }
        write!(upstream, "{k}: {v}\r\n")?;
    }
    write!(
        upstream,
        "content-length: {}\r\nconnection: close\r\n\r\n",
        body.len()
    )?;
    upstream.write_all(body)?;
    upstream.flush()?;

    Ok(BufReader::new(upstream))
}

struct ResponseHead {
    status_line: String,
    headers: Vec<(String, String)>,
}

/// 读上游响应头（状态行 + 头块），不解 body。
fn read_response_head<R: Read>(reader: &mut BufReader<R>) -> std::io::Result<ResponseHead> {
    let status_line = http::bounded_line(reader, 8192)?;
    if status_line.is_empty() {
        return Err(http::bad_input("上游响应为空"));
    }
    let status_line = status_line.trim_end().to_string();
    let mut bytes = status_line.len();
    let mut headers = Vec::new();
    loop {
        let line = http::bounded_line(reader, 65536usize.saturating_sub(bytes))?;
        if line.is_empty() {
            return Err(http::bad_input("上游头部截断"));
        }
        bytes += line.len();
        let trimmed = line.trim_end();
        if trimmed.is_empty() {
            break;
        }
        if let Some((k, v)) = trimmed.split_once(':') {
            headers.push((k.trim().to_string(), v.trim().to_string()));
        }
        if bytes > 65536 {
            return Err(http::bad_input("上游头部过大"));
        }
    }
    Ok(ResponseHead {
        status_line,
        headers,
    })
}

/// 供单测：改写 body 里的 model 字段。
/// 策略（2026-10-13 用户拍板「接入用真名」后收敛）：只有别名 `megacube-default`
/// 被改写成活跃模型真实 id；其他名字原样透传——引擎对不认识的名字回诚实的
/// 404（stale 配置浮出水面），不做静默换模型。
fn rewrite_json_body(body: &[u8], real_model_id: &str) -> Result<(Vec<u8>, Option<String>), ()> {
    if body.is_empty() {
        return Ok((Vec::new(), None));
    }
    let text = std::str::from_utf8(body).map_err(|_| ())?;
    let mut value = Value::parse_limited(text, 65_536).map_err(|_| ())?;
    if value.get("model").and_then(Value::as_str) == Some("megacube-default") {
        value.set("model", real_model_id.into());
        let model = Some(real_model_id.to_owned());
        Ok((value.to_string().into_bytes(), model))
    } else {
        let model = value
            .get("model")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .map(str::to_owned);
        Ok((body.to_vec(), model))
    }
}

#[cfg(test)]
pub fn rewrite_model_field(body: &[u8], real_model_id: &str) -> Vec<u8> {
    if body.is_empty() {
        return body.to_vec();
    }
    rewrite_json_body(body, real_model_id)
        .map(|v| v.0)
        .unwrap_or_else(|_| body.to_vec())
}

/// 取改写后 body 的 model 字段（非 JSON/无字段 → None）。
fn model_name_of(body: &[u8]) -> Option<String> {
    if body.is_empty() {
        return None;
    }
    Value::parse_limited(std::str::from_utf8(body).ok()?, 65_536)
        .ok()
        .and_then(|v| {
            v.get("model")
                .and_then(|m| m.as_str())
                .map(|s| s.to_string())
        })
        .filter(|s| !s.is_empty())
}

/// Named requests can address an already managed running model even when no
/// default was selected. An alias or missing/unknown name still needs a default.
#[cfg(test)]
fn target_for_request(
    state: &crate::store::State,
    requested: Option<&str>,
) -> Option<(u16, String, String)> {
    target_for_request_filtered(state, requested, &|_| true)
}

fn target_for_request_filtered(
    state: &crate::store::State,
    requested: Option<&str>,
    allowed: &impl Fn(&crate::store::ModelState) -> bool,
) -> Option<(u16, String, String)> {
    let ready = |m: &&crate::store::ModelState| {
        m.port > 0 && matches!(m.status, ModelStatus::Running | ModelStatus::Starting) && allowed(m)
    };
    let active = state
        .active_model
        .as_ref()
        .and_then(|card| state.models.get(card))
        .filter(ready);
    let selected = active.or_else(|| {
        requested
            .filter(|name| *name != "megacube-default")
            .and_then(|name| {
                state
                    .models
                    .values()
                    .find(|m| m.lifecycle_owned && m.model_id == name && ready(m))
            })
    });
    selected.map(|m| (m.port, m.model_id.clone(), m.card.clone()))
}

/// 真名归属路由的决策表（2026-10-14 用户拍板「行保留，没服务要诚实报」）。
#[derive(Debug, PartialEq, Eq)]
enum ModelRoute {
    /// 发活跃卡（别名改写后 / 无 model 字段 / 真名即活跃卡供应的 id / 无归属真名）。
    Active,
    /// 真名属于某张在跑的非活跃供应卡：发那张卡（注册顺序先到先服务）。
    Owner { port: u16 },
    /// 真名有登记归属但全部供应卡停止：网关诚实 503（不静默顶替）。
    NotRunning,
}

fn route_model(
    requested: Option<&str>,
    active_real_id: &str,
    claimed: bool,
    running_owner: Option<u16>,
) -> ModelRoute {
    match requested {
        None => ModelRoute::Active,
        Some(name) if name == active_real_id => ModelRoute::Active,
        Some(_) => {
            if let Some(port) = running_owner {
                ModelRoute::Owner { port }
            } else if claimed {
                ModelRoute::NotRunning
            } else {
                ModelRoute::Active
            }
        }
    }
}

/// 有归属但全停时的诚实 503（与 no_active_model 同构的结构化错误）。
fn write_model_not_running(downstream: &mut TcpStream, name: &str) -> std::io::Result<()> {
    let mut v = Value::obj();
    v.set("error", {
        let mut e = Value::obj();
        e.set("code", "model_not_running".into());
        e.set(
            "message",
            format!(
                "模型 {name} 的引擎未在运行（登记供应它的卡片全部停止）。请先在模型商店或 MegaCube 面板启用它，或改用当前活跃模型"
            )
            .into(),
        );
        e.set("type", "service_unavailable".into());
        e
    });
    http::write_response(
        downstream,
        "503 Service Unavailable",
        "application/json",
        v.to_string().as_bytes(),
        &[],
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn excessive_structure_and_invalid_utf8_do_not_reach_engine() {
        let flat = format!(
            "{{\"model\":\"megacube-default\",\"padding\":[{}0]}}",
            "0,".repeat(65_536)
        );
        assert!(rewrite_json_body(flat.as_bytes(), "real").is_err());
        assert!(rewrite_json_body(b"{\"input\":\"\xff\"}", "real").is_err());
        assert_eq!(
            rewrite_json_body(br#"{"model":"other","messages":[]}"#, "real")
                .unwrap()
                .1
                .as_deref(),
            Some("other")
        );
    }

    #[test]
    fn a_named_owned_model_does_not_require_or_select_a_default() {
        let json = Value::parse(r#"{"models":{"owned":{"card":"owned","model_id":"Named","engine_sha8":"x","port":18080,"status":"running","lifecycle_owned":true}}}"#).unwrap();
        let mut state = crate::store::State::from_json(&json);
        assert_eq!(target_for_request(&state, Some("Named")).unwrap().0, 18080);
        assert!(state.active_model.is_none());
        assert!(target_for_request(&state, None).is_none());
        assert!(target_for_request(&state, Some("megacube-default")).is_none());
        assert!(target_for_request(&state, Some("Unknown")).is_none());
        state.models.get_mut("owned").unwrap().lifecycle_owned = false;
        assert!(target_for_request(&state, Some("Named")).is_none());
        state.models.get_mut("owned").unwrap().lifecycle_owned = true;
        state.models.get_mut("owned").unwrap().status = ModelStatus::Stopped;
        assert!(target_for_request(&state, Some("Named")).is_none());
    }

    #[test]
    fn rewrites_default_alias() {
        let body = br#"{"model":"megacube-default","messages":[{"role":"user","content":"hi"}],"stream":true}"#;
        let out = rewrite_model_field(body, "Ornith-1.5-35B-A3B-NVFP4");
        let v = Value::parse(&String::from_utf8_lossy(&out)).unwrap();
        assert_eq!(
            v.get("model").and_then(|m| m.as_str()),
            Some("Ornith-1.5-35B-A3B-NVFP4")
        );
        // 其他字段不动
        assert_eq!(v.get("stream"), Some(&Value::Bool(true)));
    }

    #[test]
    fn passes_through_real_names() {
        // 真名（哪怕是 stale 的）不改写：引擎回诚实 404
        let body = br#"{"model":"Ornith-1.5-35B-A3B-NVFP4","messages":[]}"#;
        let out = rewrite_model_field(body, "Qwen3.8-27B-NVFP4");
        assert_eq!(out.as_slice(), body.as_slice());
    }

    #[test]
    fn leaves_body_without_model() {
        let body = br#"{"messages":[]}"#;
        assert_eq!(rewrite_model_field(body, "X"), body);
    }

    #[test]
    fn leaves_non_json() {
        let body = b"not json";
        assert_eq!(rewrite_model_field(body, "X"), body);
    }

    #[test]
    fn leaves_empty() {
        assert_eq!(rewrite_model_field(b"", "X"), b"");
    }

    /// 真名归属路由决策表（2026-10-14 拍板「行保留，没服务要诚实报」）。
    #[test]
    fn model_routing_decision_table() {
        // 别名改写后 / 无字段 / 真名即活跃卡供应的 id → 活跃卡
        assert_eq!(route_model(None, "A", false, None), ModelRoute::Active);
        assert_eq!(route_model(Some("A"), "A", true, None), ModelRoute::Active);
        // 真名属于在跑的非活跃供应卡 → 那张卡（撞名也归它——决策输入已按活跃卡优先排好）
        assert_eq!(
            route_model(Some("X"), "A", true, Some(30001)),
            ModelRoute::Owner { port: 30001 }
        );
        // 真名有归属但全停 → 诚实 503（sglang0519 行在 mtp 活跃时的实测场景）
        assert_eq!(
            route_model(Some("X"), "A", true, None),
            ModelRoute::NotRunning
        );
        // 无归属真名 → 维持发活跃卡（由引擎校验）
        assert_eq!(
            route_model(Some("who"), "A", false, None),
            ModelRoute::Active
        );
    }

    #[test]
    fn model_name_extraction() {
        assert_eq!(
            model_name_of(br#"{"model":"Qwen3.8-27B-NVFP4","messages":[]}"#).as_deref(),
            Some("Qwen3.8-27B-NVFP4")
        );
        assert_eq!(model_name_of(br#"{"messages":[]}"#), None);
        assert_eq!(model_name_of(b"not json"), None);
        assert_eq!(model_name_of(b""), None);
    }
}

#[cfg(test)]
mod debug_chinese_body {
    // 真实 V6 请求形状：中文 body 改写后必须仍是合法 JSON 且内容不变
    #[test]
    fn chinese_body_roundtrip() {
        let body = r#"{"model":"megacube-default","messages":[{"role":"user","content":"用两个字回答：北京的别称"}],"max_tokens":32}"#.as_bytes();
        let out = super::rewrite_model_field(body, "Ornith-1.5-35B-A3B-NVFP4");
        let text = String::from_utf8_lossy(&out).to_string();
        let v = crate::json::Value::parse(&text).unwrap();
        assert_eq!(
            v.get("model").and_then(|m| m.as_str()),
            Some("Ornith-1.5-35B-A3B-NVFP4")
        );
        assert_eq!(
            v.get("messages")
                .and_then(|m| m.as_arr())
                .and_then(|a| a.first())
                .and_then(|x| x.get("content"))
                .and_then(|c| c.as_str()),
            Some("用两个字回答：北京的别称")
        );
    }
}

#[cfg(test)]
mod ownership_route_tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, Ordering};
    #[test]
    fn stopped_owned_model_must_not_route_to_an_observation_only_container() {
        let root = crate::docker::fixtures::tmpdir("r04-observed-gateway");
        let app = crate::docker::fixtures::make_app(&root, 128, 100);
        let external = TcpListener::bind("127.0.0.1:0").unwrap();
        external.set_nonblocking(true).unwrap();
        let external_port = external.local_addr().unwrap().port();
        let sent = Arc::new(AtomicBool::new(false));
        let captured = sent.clone();
        let upstream = std::thread::spawn(move || {
            let until = Instant::now() + Duration::from_secs(2);
            while Instant::now() < until {
                if let Ok((mut socket, _)) = external.accept() {
                    socket
                        .set_read_timeout(Some(Duration::from_secs(1)))
                        .unwrap();
                    let _ = http::read_request(&mut BufReader::new(socket.try_clone().unwrap()));
                    captured.store(true, Ordering::SeqCst);
                    http::write_response(
                        &mut socket,
                        "200 OK",
                        "application/json",
                        b"{\"source\":\"external-observation-only\"}",
                        &[],
                    )
                    .unwrap();
                    return;
                }
                std::thread::sleep(Duration::from_millis(10));
            }
        });
        let raw = serde_json::json!({"models": {
            "active": {"card":"active","model_id":"Owned-A","engine_sha8":"","port":19998,"status":"running","engine_type":"container","lifecycle_owned":true},
            "stopped": {"card":"stopped","model_id":"Owned-B","engine_sha8":"","port":19999,"status":"stopped","engine_type":"container","lifecycle_owned":true},
            "observed": {"card":"observed","model_id":"Owned-B","engine_sha8":"","port":external_port,"status":"running","engine_type":"container","lifecycle_owned":false}
        },"active_model":"active"});
        app.store.lock().unwrap().state =
            crate::store::State::from_json(&Value::parse(&raw.to_string()).unwrap());
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let daemon =
            std::thread::spawn(move || handle_conn(&app, listener.accept().unwrap().0).unwrap());
        let mut client = TcpStream::connect(addr).unwrap();
        client
            .set_read_timeout(Some(Duration::from_secs(3)))
            .unwrap();
        let body = b"{\"model\":\"Owned-B\"}";
        write!(client,"POST /v1/chat/completions HTTP/1.1\r\nHost: local\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n",body.len()).unwrap();
        client.write_all(body).unwrap();
        let mut response = String::new();
        client.read_to_string(&mut response).unwrap();
        daemon.join().unwrap();
        upstream.join().unwrap();
        eprintln!(
            "response={response:?}; reached_observation_only={}",
            sent.load(Ordering::SeqCst)
        );
        assert!(!sent.load(Ordering::SeqCst));
        assert!(response.starts_with("HTTP/1.1 503"));
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[cfg(test)]
mod container_identity_tests {
    use super::*;
    use crate::docker::fixtures as fx;
    #[test]
    fn gateway_requires_matching_image_and_published_loopback_endpoint() {
        for (tag, image, published) in [
            ("foreign-image", "foreign/image:latest", true),
            ("unpublished", "fixture/img:1.0", false),
        ] {
            let root = fx::tmpdir(&format!("gateway-container-{tag}"));
            let app = fx::make_app(&root, 128, 100);
            let upstream = TcpListener::bind("127.0.0.1:0").unwrap();
            let port = upstream.local_addr().unwrap().port();
            fx::write_docker_card(
                &app.config.cards_dir,
                "selected",
                "selected-ctr",
                port,
                Some("0.01"),
            );
            fx::set_container(
                &root,
                "selected-ctr",
                image,
                true,
                "always",
                0,
                false,
                8765,
                if published { port } else { 0 },
                8000,
                "--served-model-name,Declared-Model,--mem-fraction-static,0.01",
            );
            crate::ops::upsert_model_state(&app, "selected", |m| {
                m.lifecycle_owned = true;
                m.status = ModelStatus::Running;
                m.main_pid = Some(4321);
                m.port = port;
                m.model_id = "Declared-Model".into();
            });
            app.store.lock().unwrap().state.active_model = Some("selected".into());
            let sent = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
            let flag = sent.clone();
            upstream.set_nonblocking(true).unwrap();
            let upstream_thread = std::thread::spawn(move || {
                let until = Instant::now() + Duration::from_secs(2);
                while Instant::now() < until {
                    if let Ok((mut socket, _)) = upstream.accept() {
                        let mut reader = BufReader::new(socket.try_clone().unwrap());
                        let request = http::read_request(&mut reader).unwrap().unwrap();
                        flag.store(true, std::sync::atomic::Ordering::SeqCst);
                        eprintln!(
                            "FOREIGN UPSTREAM received={}",
                            String::from_utf8_lossy(&request.body)
                        );
                        http::write_response(
                            &mut socket,
                            "200 OK",
                            "application/json",
                            br#"{"source":"foreign-container-model"}"#,
                            &[],
                        )
                        .unwrap();
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(10));
                }
            });
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let addr = listener.local_addr().unwrap();
            let daemon = std::thread::spawn(move || {
                handle_conn(&app, listener.accept().unwrap().0).unwrap()
            });
            let mut client = TcpStream::connect(addr).unwrap();
            client
                .set_read_timeout(Some(Duration::from_secs(4)))
                .unwrap();
            let body = br#"{"model":"Declared-Model"}"#;
            write!(client,"POST /v1/chat/completions HTTP/1.1\r\nHost: local\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n",body.len()).unwrap();
            client.write_all(body).unwrap();
            let mut response = String::new();
            client.read_to_string(&mut response).unwrap();
            daemon.join().unwrap();
            upstream_thread.join().unwrap();
            eprintln!(
                "GATEWAY response={response:?} reached_foreign={}",
                sent.load(std::sync::atomic::Ordering::SeqCst)
            );
            assert!(response.starts_with("HTTP/1.1 503"));
            assert!(!sent.load(std::sync::atomic::Ordering::SeqCst));
        }
    }
}

#[cfg(test)]
mod upstream_write_tests {
    use super::*;
    use std::io::Cursor;
    use std::net::Shutdown;
    use std::os::fd::AsRawFd;

    fn request(size: usize) -> std::io::Result<http::Request> {
        let mut bytes = format!(
            "POST /v1/chat/completions HTTP/1.1\r\ncontent-type: application/octet-stream\r\ncontent-length: {size}\r\n\r\n"
        ).into_bytes();
        bytes.resize(bytes.len() + size, b'x');
        http::read_request(&mut BufReader::new(Cursor::new(bytes))).map(|value| value.unwrap())
    }

    #[test]
    fn stalled_upstream_write_releases_shared_large_body_budget() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let upstream = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (peer, _) = listener.accept().unwrap();
        drop(listener);
        let size: libc::c_int = 4096;
        for (socket, option) in [(&upstream, libc::SO_SNDBUF), (&peer, libc::SO_RCVBUF)] {
            // SAFETY: live fixture socket fd and valid socket-option buffer.
            assert_eq!(
                unsafe {
                    libc::setsockopt(
                        socket.as_raw_fd(),
                        libc::SOL_SOCKET,
                        option,
                        (&size as *const libc::c_int).cast(),
                        std::mem::size_of_val(&size) as libc::socklen_t,
                    )
                },
                0
            );
        }
        let rescue = upstream.try_clone().unwrap();
        let req = request(16 * 1024 * 1024).unwrap();
        assert_eq!(
            request(65_537).unwrap_err().to_string(),
            "HTTP buffer budget exhausted"
        );
        assert!(request(32).is_ok());
        let (sender, receiver) = std::sync::mpsc::channel();
        let worker = std::thread::spawn(move || {
            let result =
                forward_request(upstream, &req, &req.body, 12345, Duration::from_millis(100))
                    .map(|_| ())
                    .map_err(|error| error.kind());
            drop(req);
            let _ = sender.send(result);
        });
        // Keep the accepted peer open without reading. A regression must fail
        // within this watchdog, and closing it still lets the worker be reaped.
        let result = receiver.recv_timeout(Duration::from_secs(2));
        let admission_restored = result.is_ok() && request(65_537).is_ok();
        rescue.shutdown(Shutdown::Both).unwrap();
        let _ = peer.shutdown(Shutdown::Both);
        drop(peer);
        worker.join().unwrap();
        let error = result
            .expect("upstream write must finish while its peer remains open")
            .expect_err("the frozen upstream must not accept a complete body");
        assert!(matches!(
            error,
            std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
        ));
        assert!(
            admission_restored,
            "timed-out Request must release the shared large-body permit"
        );
    }
}
