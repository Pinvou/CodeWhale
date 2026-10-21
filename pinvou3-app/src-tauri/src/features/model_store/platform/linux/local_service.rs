//! 模型商店本地下载服务：`<store_root>/service.sock` 上的极简 HTTP/1.1 JSON
//! over unix socket。
//!
//! # 它为什么存在
//!
//! 架构决策（用户拍板 2026-10-14）：**商店是唯一的下载执行者**。cmsd（以及
//! 将来的其它本地入口）要下载模型时，不自己实现一套编排，而是经这个 socket
//! 委托商店执行——商店卡与本地卡片、进度、日志、提权、配额、断点续传全是
//! 商店原生那一条链路。
//!
//! # 协议形状
//!
//! 与 cmsd 自己的 socket 协议同款（cube-manage-software src/http.rs）：请求行 +
//! `content-length` + `connection: close` + JSON body，响应带 content-length 后
//! 关闭。客户端实现因此可以做到最简（megacube_admin_server.py 的 `_http_get` /
//! `_http_post` 原样可用）。
//!
//! ```text
//! POST /v1/install              {"card_id":"…"}  → 200 {"run_id","kind":"download","card"}
//! POST /v1/install_local        {"card_id":"…"}  → 200 {"run_id","kind":"install_local","card"}
//! GET  /v1/tasks                             → 200 {"tasks":[{run_id,card_id,kind,status,
//!                                                created_at,progress,last_line,error}]}
//! GET  /v1/tasks/<id>                        → 200 （同上字段 + lines 末尾至多 500 行）
//! POST /v1/tasks/<id>/cancel                 → 200 {"status":"cancelling"}
//! 错误一律 {"error":{"code","message"}}，message 是可直接展示的中文。
//! ```
//!
//! status ∈ running / succeeded / failed / cancelled——取消单列一态，不等同失败
//! （商店 RunLog 本来就有 cancelled 标记）。
//!
//! # 同权客户端，没有额外权限层
//!
//! 这里发起的下载走**同一个** spawn_op / ActiveCardOps / DownloadQueue：socket
//! 和界面是同权客户端，撞同一把卡片互斥锁、排同一条队。这是刻意的——本机语义，
//! 与 cmsd socket 同等级（0600，只本用户可连），不另加 token 或鉴权；多一层
//! 权限只会让「界面看到的状态」和「cmsd 看到的状态」分叉。

use std::io::{BufRead as _, BufReader, Read as _, Write as _};
use std::os::unix::fs::PermissionsExt as _;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use parking_lot::Mutex;
use serde_json::{Value, json};

use super::config;
use super::lifecycle::ModelManager;
use super::service::{self, ModelStoreService, TaskSnapshot};

/// 请求头/主体的上限。这个协议的 body 只有 `{"card_id":"…"}` 量级，
/// 给 1MB 已经是几十倍的余量；头部对齐 cmsd 的 64KB。
const MAX_HEAD_BYTES: usize = 64 * 1024;
const MAX_BODY_BYTES: usize = 1024 * 1024;
/// 单连接的读写超时。请求都是立刻能答的（下发是 spawn 即回、查询是内存快照），
/// 30 秒答不完只能说明对端死了，关掉就是——不能让它占着连接线程不放。
const IO_TIMEOUT: Duration = Duration::from_secs(30);
/// 单任务详情带的日志行数上限（末尾 N 行）。
const TASK_LOG_TAIL: usize = 500;

/// 监听句柄。`shutdown()` 幂等：置标志、用一个空连接唤醒 accept、join 掉
/// 接受线程、删掉 sock 文件；Drop 走同一条路。两条路都跑过一遍之后，
/// 「fd 泄漏 / 僵尸线程」在构造上就不成立：
/// - accept 线程：shutdown 返回时已被 join，必然死透；
/// - 连接线程：每个请求处理完即终（应答写完、流 drop），最坏被 30 秒 IO
///   超时兜底，不持有任何句柄；
/// - sock 文件：启动时清掉上次的残留（活监听者除外，那直接报错），退出时
///   由 shutdown 删除。
pub struct LocalService {
    sock_path: PathBuf,
    shutdown: Arc<AtomicBool>,
    accept_thread: Mutex<Option<std::thread::JoinHandle<()>>>,
    stopped: AtomicBool,
}

impl LocalService {
    /// 绑定并开始服务。`test_mode` 只影响 /v1/install 走哪个商店数据源
    /// （测试商店 = 编译进二进制的卡片，不发网络请求）；生产恒为 false。
    pub(super) fn bind(
        service: Arc<ModelStoreService>,
        sock_path: PathBuf,
        test_mode: bool,
    ) -> std::io::Result<Self> {
        if let Some(parent) = sock_path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        if sock_path.exists() {
            // 上次异常退出留下的旧 sock 文件。能 connect 上说明真有活着的
            // 监听者（比如第二个应用实例）——那不能抢，报错让调用方记日志；
            // 连不上就是死残留，删掉重绑。socket 文件不是数据，删了无损。
            if UnixStream::connect(&sock_path).is_ok() {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::AddrInUse,
                    format!("{} 上已有活着的监听者，不重复绑定", sock_path.display()),
                ));
            }
            let _ = std::fs::remove_file(&sock_path);
        }
        let listener = UnixListener::bind(&sock_path)?;
        // 0600：本机同用户语义，与 cmsd socket 同级。同机其它用户不该能往
        // 这里灌下载任务——socket 一开放，等于把「以本用户身份跑卡片脚本」
        // 的入口交了出去。
        std::fs::set_permissions(&sock_path, std::fs::Permissions::from_mode(0o600))?;

        let shutdown = Arc::new(AtomicBool::new(false));
        let flag = Arc::clone(&shutdown);
        let accept_thread = std::thread::spawn(move || {
            accept_loop(listener, service, test_mode, flag);
        });
        Ok(Self {
            sock_path,
            shutdown,
            accept_thread: Mutex::new(Some(accept_thread)),
            stopped: AtomicBool::new(false),
        })
    }

    /// 停服务并清理 sock 文件。幂等：调用多少次效果都一样。
    pub fn shutdown(&self) {
        if self.stopped.swap(true, Ordering::SeqCst) {
            return;
        }
        self.shutdown.store(true, Ordering::SeqCst);
        // accept 在阻塞等连接：用一个空连接把它唤醒，它看到标志位自然退出。
        // 连不上（比如监听者自己已经没了）也无所谓，join 照样收得住。
        let _ = UnixStream::connect(&self.sock_path);
        if let Some(thread) = self.accept_thread.lock().take() {
            let _ = thread.join();
        }
        let _ = std::fs::remove_file(&self.sock_path);
    }
}

impl Drop for LocalService {
    fn drop(&mut self) {
        self.shutdown();
    }
}

fn accept_loop(
    listener: UnixListener,
    service: Arc<ModelStoreService>,
    test_mode: bool,
    shutdown: Arc<AtomicBool>,
) {
    for stream in listener.incoming() {
        if shutdown.load(Ordering::SeqCst) {
            break;
        }
        let Ok(stream) = stream else {
            // 临时性接受失败（比如被信号打断）：不是关停就继续听
            if shutdown.load(Ordering::SeqCst) {
                break;
            }
            continue;
        };
        if shutdown.load(Ordering::SeqCst) {
            break;
        }
        let svc = Arc::clone(&service);
        // 一连接一线程：请求都是毫秒级能答完的（下发即 spawn 即回、查询是
        // 内存快照），唯一的长阻塞是 /v1/install 下发前的商店卡片确认
        // （一次 https，有超时兜底）。线程随请求结束而终结，不攒句柄。
        std::thread::spawn(move || handle_connection(stream, svc, test_mode));
    }
}

fn handle_connection(stream: UnixStream, service: Arc<ModelStoreService>, test_mode: bool) {
    let _ = stream.set_read_timeout(Some(IO_TIMEOUT));
    let _ = stream.set_write_timeout(Some(IO_TIMEOUT));
    let Ok(reader_stream) = stream.try_clone() else {
        return;
    };
    let mut reader = BufReader::new(reader_stream);
    let req = match read_request(&mut reader) {
        Ok(None) => return,
        Ok(Some(req)) => req,
        Err(e) => {
            let _ = write_response(
                &mut { stream },
                error_response(400, "bad_request", &e.to_string()),
            );
            return;
        }
    };
    let path = req.path.split('?').next().unwrap_or("");
    let control = req.method == "POST" && path == "/v1/host/retire";
    let health = req.method == "POST" && path == "/v1/ui/health";
    let _request = if control || health {
        None
    } else {
        match service.host_control.request() {
            Ok(guard) => Some(guard),
            Err(error) => {
                let _ = write_response(
                    &mut { stream },
                    error_response(409, "host_draining", &error),
                );
                return;
            }
        }
    };
    let response = route(&req, &service, test_mode);
    let retired = control && response.status == 200;
    let mut writer = stream;
    let _ = write_response(&mut writer, response);
    if retired {
        service.host_control.response_finished();
    }
}

// ---------------------------------------------------------------- 极简 HTTP/1.1
//
// 与 cmsd 的 src/http.rs 同一个子集：请求行 + content-length + connection: close。
// 不实现 chunked——这个协议的客户端（cmsd、megacube_admin_server.py）都只发
// content-length，多实现一种编码就是多一处没人用的攻击面。

struct Request {
    method: String,
    path: String,
    body: Vec<u8>,
}

fn bad_input(msg: &str) -> std::io::Error {
    std::io::Error::new(std::io::ErrorKind::InvalidData, msg.to_string())
}

fn read_request(reader: &mut BufReader<UnixStream>) -> std::io::Result<Option<Request>> {
    let mut head = String::new();
    loop {
        let mut line = String::new();
        let n = reader.read_line(&mut line)?;
        if n == 0 {
            if head.is_empty() {
                return Ok(None);
            }
            return Err(bad_input("头部未结束连接就断了"));
        }
        if head.len() + line.len() > MAX_HEAD_BYTES {
            return Err(bad_input("头部过大"));
        }
        if line == "\r\n" || line == "\n" {
            break;
        }
        head.push_str(&line);
    }
    let mut lines = head.split("\r\n");
    let request_line = lines.next().unwrap_or("");
    let mut parts = request_line.split_whitespace();
    let method = parts.next().unwrap_or("").to_string();
    let target = parts.next().unwrap_or("").to_string();
    if method.is_empty() || target.is_empty() {
        return Err(bad_input("请求行不完整"));
    }
    let mut content_length = 0usize;
    for line in lines {
        if let Some((k, v)) = line.split_once(':') {
            if k.trim().eq_ignore_ascii_case("content-length") {
                content_length = v
                    .trim()
                    .parse()
                    .map_err(|_| bad_input("content-length 非法"))?;
            }
        }
    }
    if content_length > MAX_BODY_BYTES {
        return Err(bad_input("主体过大"));
    }
    let mut body = vec![0u8; content_length];
    reader.read_exact(&mut body)?;
    Ok(Some(Request {
        method,
        path: target,
        body,
    }))
}

struct Response {
    status: u16,
    body: Value,
}

fn ok_response(body: Value) -> Response {
    Response { status: 200, body }
}

/// 错误应答：`{"error":{"code","message"}}`，message 是可直接展示的中文。
/// 形状与 cmsd 的错误应答一致，客户端一套解析代码两边通用。
fn error_response(status: u16, code: &str, message: &str) -> Response {
    Response {
        status,
        body: json!({"error": {"code": code, "message": message}}),
    }
}

fn status_text(status: u16) -> &'static str {
    match status {
        200 => "200 OK",
        400 => "400 Bad Request",
        404 => "404 Not Found",
        409 => "409 Conflict",
        502 => "502 Bad Gateway",
        _ => "500 Internal Server Error",
    }
}

fn write_response(w: &mut UnixStream, resp: Response) -> std::io::Result<()> {
    let body = serde_json::to_vec(&resp.body).unwrap_or_else(|_| b"{}".to_vec());
    write!(
        w,
        "HTTP/1.1 {}\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
        status_text(resp.status),
        body.len()
    )?;
    w.write_all(&body)?;
    w.flush()
}

// ---------------------------------------------------------------- 路由

fn route(req: &Request, service: &Arc<ModelStoreService>, test_mode: bool) -> Response {
    // 这个服务没有查询串；有也忽略（路径部分照常匹配）
    let path = req.path.split('?').next().unwrap_or("");
    if req.method == "POST" && path == "/v1/host/retire" {
        let expected = match serde_json::from_slice::<super::host_control::Identity>(&req.body) {
            Ok(expected) => expected,
            Err(_) => return error_response(400, "bad_request", "invalid host identity"),
        };
        return match service.retire_host(&expected) {
            Ok(()) => ok_response(json!({"retiring":true,"host":expected})),
            Err(message) => error_response(409, "host_not_idle", &message),
        };
    }
    // Only these stable task controls may cross a software-version handoff.
    if req.method == "POST" && path == "/v1/host/task" {
        #[derive(serde::Deserialize)]
        #[serde(deny_unknown_fields)]
        struct TaskControl {
            host: super::host_control::Identity,
            method: String,
            args: Value,
        }
        let request = match serde_json::from_slice::<TaskControl>(&req.body) {
            Ok(request) => request,
            Err(_) => return error_response(400, "bad_request", "invalid host task request"),
        };
        if request.host != service.host_control.identity
            || !["active", "log", "cancel", "answer"].contains(&request.method.as_str())
        {
            return error_response(
                409,
                "host_generation_changed",
                "host or task control mismatch",
            );
        }
        return match super::background::dispatch(service, &request.method, request.args) {
            Ok(result) => ok_response(json!({"result":result})),
            Err(message) => error_response(409, "store_error", &message),
        };
    }

    if req.method == "POST" {
        if let Some(method) = path.strip_prefix("/v1/ui/") {
            let args = match serde_json::from_slice(&req.body) {
                Ok(value) => value,
                Err(e) => return error_response(400, "bad_request", &e.to_string()),
            };
            return match super::background::dispatch(service, method, args) {
                Ok(result) => ok_response(json!({"result": result})),
                Err(message) => error_response(409, "store_error", &message),
            };
        }
    }
    match (req.method.as_str(), path) {
        ("POST", "/v1/install") => handle_install(req, service, test_mode, false),
        ("POST", "/v1/install_local") => handle_install(req, service, test_mode, true),
        ("POST", "/v1/retry_install") => handle_retry_install(req, service),
        ("GET", "/v1/tasks") => handle_tasks(service),
        ("POST", p) if p.starts_with("/v1/tasks/") && p.ends_with("/cancel") => {
            let id = &p["/v1/tasks/".len()..p.len() - "/cancel".len()];
            handle_cancel(service, id)
        }
        ("GET", p) if p.starts_with("/v1/tasks/") => {
            handle_task_detail(service, &p["/v1/tasks/".len()..])
        }
        _ => error_response(
            404,
            "unknown_endpoint",
            &format!("没有这个接口：{} {path}", req.method),
        ),
    }
}

/// 下发的卡片 id 先过这把尺：它同时要进商店 URL 路径段和 `cards/<id>` 目录名，
/// 跟商店客户端（store_client::get_card）用的是同一个谓词。
fn parse_card_id(req: &Request) -> Result<String, Response> {
    let body: Value = match serde_json::from_slice(&req.body) {
        Ok(v) => v,
        Err(e) => {
            return Err(error_response(
                400,
                "bad_request",
                &format!("请求体不是合法 JSON：{e}"),
            ));
        }
    };
    let Some(card_id) = body
        .get("card_id")
        .and_then(|v| v.as_str())
        .map(str::trim)
        .filter(|s| !s.is_empty())
    else {
        return Err(error_response(
            400,
            "bad_request",
            "请求体缺少 card_id（期望 {\"card_id\":\"…\"}）",
        ));
    };
    if let Err(e) = config::safe_path_component(card_id) {
        return Err(error_response(
            400,
            "bad_request",
            &format!("卡片 id 不合法：{e}"),
        ));
    }
    Ok(card_id.to_string())
}

fn handle_retry_install(req: &Request, service: &Arc<ModelStoreService>) -> Response {
    #[derive(serde::Deserialize)]
    #[serde(deny_unknown_fields)]
    struct RetryBody {
        original_run_id: String,
        expected_preview_sha256: String,
        run_id: String,
    }
    let body: RetryBody = match serde_json::from_slice(&req.body) {
        Ok(body) => body,
        Err(_) => return error_response(400, "bad_request", "invalid retry installation request"),
    };
    let suffix = body.run_id.strip_prefix("cmsd-").unwrap_or("");
    if suffix.is_empty()
        || suffix.len() > 100
        || !suffix
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"_-".contains(&byte))
    {
        return error_response(400, "bad_request", "invalid delegated run_id");
    }
    match service::retry_incomplete_with_id(
        service,
        body.original_run_id,
        body.expected_preview_sha256,
        Some(body.run_id),
    ) {
        Ok(value) => ok_response(
            json!({"run_id":value["run_id"],"kind":value["kind"],"card":value["card_id"]}),
        ),
        Err(message) => error_response(409, "retry_install_rejected", &message),
    }
}

fn handle_install(
    req: &Request,
    service: &Arc<ModelStoreService>,
    test_mode: bool,
    local: bool,
) -> Response {
    let card_id = match parse_card_id(req) {
        Ok(id) => id,
        Err(resp) => return resp,
    };
    let args: Value = serde_json::from_slice(&req.body).unwrap_or(Value::Null);
    let requested_run_id = match args.get("request_id").and_then(Value::as_str) {
        Some(id) => {
            if id.len() > 100
                || id.is_empty()
                || !id
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b))
            {
                return error_response(400, "bad_request", "request_id 不合法");
            }
            Some(format!("cmsd-{id}"))
        }
        None => None,
    };
    let (kind, result) = if local {
        // 本地卡片：目录在不在一眼可查，不在当场 404。更细的校验（六脚本齐不齐、
        // 形态可不可辨）在任务线程里做——失败进任务错误，跟 spawn_op 的语义一致，
        // 客户端经 /v1/tasks/<id> 拿得到那句中文。
        let dir = config::data_dir().join("cards").join(&card_id);
        if !dir.is_dir() {
            return error_response(
                404,
                "card_not_found",
                &format!("本地卡片目录不存在：{}", dir.display()),
            );
        }
        (
            "install_local",
            service::start_install_local_with_id(service, card_id.clone(), requested_run_id),
        )
    } else {
        // 商店卡片：先确认商店里真有这张卡——不存在要当场 404，而不是等任务
        // 线程跑起来再失败。商店够不着时不放行：排一个注定失败的下载没有意思。
        let mgr = if test_mode {
            ModelManager::new_test_store()
        } else {
            ModelManager::new(None, None)
        };
        if let Err(e) = mgr.get_store_card(&card_id) {
            let msg = e.to_string();
            // 真实商店（「未找到对应的商店内容」）与测试商店（「未找到测试商店
            // 卡片」）的 404 文案共用的片段就是「未找到」。
            if msg.contains("未找到") {
                return error_response(
                    404,
                    "card_not_found",
                    &format!("商店里没有这张卡片：{card_id}"),
                );
            }
            return error_response(
                502,
                "store_unreachable",
                &format!("无法确认卡片是否存在（模型商店访问失败）：{msg}"),
            );
        }
        (
            "download",
            service::start_download_with_id(service, card_id.clone(), test_mode, requested_run_id),
        )
    };
    match result {
        Ok(v) => {
            let run_id = v
                .get("run_id")
                .and_then(|r| r.as_str())
                .unwrap_or_default()
                .to_string();
            ok_response(json!({"run_id": run_id, "kind": kind, "card": card_id}))
        }
        Err(msg) if msg.starts_with("registered_model_requires_store_workflow") => {
            error_response(409, "store_install_required", &msg)
        }
        Err(msg) if msg.starts_with("archived_model_version") => {
            error_response(409, "archived_model_version", &msg)
        }
        Err(msg) => error_response(409, "card_busy", &msg),
    }
}

fn task_json(t: &TaskSnapshot) -> Value {
    json!({
        "run_id": t.run_id,
        "card_id": t.card_id,
        "kind": t.kind,
        "status": t.status,
        "created_at": t.created_at,
        "created_at_epoch": chrono::DateTime::parse_from_rfc3339(&t.created_at).ok().map(|t| t.timestamp().max(0) as u64),
        "progress": t.progress.map(|(done, total)| json!({"done": done, "total": total})),
        "last_line": t.last_line,
        "error": t.error,
        "error_truncated": t.error_truncated,
        "last_line_truncated": t.last_line_truncated,
    })
}

fn handle_tasks(service: &Arc<ModelStoreService>) -> Response {
    let tasks: Vec<Value> = service.task_snapshots().iter().map(task_json).collect();
    ok_response(json!({ "tasks": tasks }))
}

fn handle_task_detail(service: &Arc<ModelStoreService>, run_id: &str) -> Response {
    if run_id.is_empty() {
        return error_response(404, "task_not_found", "任务 id 为空");
    }
    match service.task_snapshot(run_id, TASK_LOG_TAIL) {
        Some(t) => {
            let mut v = task_json(&t);
            v["line_offset"] = json!(t.line_offset);
            v["lines"] = json!(t.lines);
            v["lines_truncated"] = json!(t.lines_truncated);
            ok_response(v)
        }
        None => error_response(
            404,
            "task_not_found",
            &format!("没有这个任务：{run_id}（可能已过期被清扫）"),
        ),
    }
}

fn handle_cancel(service: &Arc<ModelStoreService>, run_id: &str) -> Response {
    if run_id.is_empty() {
        return error_response(404, "task_not_found", "任务 id 为空");
    }
    if service.task_snapshot(run_id, 0).is_none() {
        return error_response(404, "task_not_found", &format!("没有这个任务：{run_id}"));
    }
    // 与 model_store_cancel_run 同语义：置取消标记（正卡在等输入的一起中断）。
    // 取消是异步收尾的——脚本进程要杀、互斥要放、日志要收尾——所以这里只能
    // 如实说「取消中」，终态去 /v1/tasks/<id> 看。
    let _ = service::cancel_run(service, run_id.to_string());
    ok_response(json!({"status": "cancelling"}))
}

// ---------------------------------------------------------------- 测试

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::Path;

    /// 环境变量隔离：模型商店的路径全部由 HOME / PINVOU3_HOME 推导，而它们是
    /// 进程全局。锁的顺序全仓统一：先 platform::paths::tests::ENV_LOCK，
    /// TempHome 自己再握 config::ENV_TEST_LOCK（改 HOME 并保证还原）。
    fn temp_home() -> (
        std::sync::MutexGuard<'static, ()>,
        super::super::config::TempHome,
    ) {
        let guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let home = config::TempHome::new();
        (guard, home)
    }

    /// 极简客户端：与 megacube_admin_server.py 的 `_http_get`/`_http_post` 同款
    /// 形状（请求行 + content-length + connection: close）。这就是「客户端实现
    /// 最简单」那条约束的实证——解析器只有十几行。
    fn request(sock: &Path, method: &str, path: &str, body: Option<&str>) -> (u16, Value) {
        let mut stream = UnixStream::connect(sock).expect("连得上服务 socket");
        stream
            .set_read_timeout(Some(Duration::from_secs(10)))
            .unwrap();
        let body = body.unwrap_or("");
        let req = format!(
            "{method} {path} HTTP/1.1\r\nhost: localhost\r\ncontent-type: application/json\r\n\
             content-length: {}\r\nconnection: close\r\n\r\n{body}",
            body.len()
        );
        stream.write_all(req.as_bytes()).unwrap();
        let mut raw = Vec::new();
        stream.read_to_end(&mut raw).unwrap();
        let text = String::from_utf8(raw).expect("应答是 UTF-8");
        let (head, body) = text.split_once("\r\n\r\n").expect("应答有头部终止");
        let status: u16 = head
            .split_whitespace()
            .nth(1)
            .and_then(|s| s.parse().ok())
            .expect("状态行可解析");
        (
            status,
            serde_json::from_str(body.trim()).expect("应答体是 JSON"),
        )
    }

    /// stub 本地卡片：探针即过、download_model 按调用方给的脚本体执行。
    /// 探针过 = 引擎「已就绪」，装引擎整步跳过，全程没有任何真实副作用。
    fn write_fixture_card(cards_dir: &Path, card_id: &str, download_sh: &str) {
        let dir = cards_dir.join(card_id);
        std::fs::create_dir_all(&dir).unwrap();
        for (name, body) in [
            (
                "install_engine.sh",
                "#!/bin/sh\nif [ \"${LMM_PROBE_ONLY:-0}\" = \"1\" ]; then exit 0; fi\nexit 0\n",
            ),
            ("download_model.sh", download_sh),
            (
                "deploy.sh",
                "#!/bin/sh\necho 'PORT=\"18080\"' > .deploy-env\necho '部署完成'\n",
            ),
            ("enable.sh", "#!/bin/sh\nexit 0\n"),
            ("stop.sh", "#!/bin/sh\nexit 0\n"),
            ("uninstall.sh", "#!/bin/sh\nexit 0\n"),
        ] {
            std::fs::write(dir.join(name), body).unwrap();
        }
        std::fs::write(
            dir.join("common.sh"),
            "#!/bin/sh\n# 本卡由 megacube-admin 按 Test/Fixture 的 artifact-manifest 生成\nMODEL_ID=\"fixture-model\"\n",
        )
        .unwrap();
        std::fs::write(
            dir.join("card.json"),
            format!(
                "{{\"id\":\"{card_id}\",\"name\":\"Fixture Model\",\"size_bytes\":16,\
                 \"engine_name\":\"fixture-engine\",\"source_repo_id\":\"Test/Fixture\"}}"
            ),
        )
        .unwrap();
    }

    /// 挂在下载段的卡：打出一条进度行之后睡 30 秒。任务在取消前一直
    /// 「运行中」，互斥/列表/进度/取消四条断言都靠这段窗口。
    const SLOW_DOWNLOAD_SH: &str =
        "#!/bin/sh\necho '开始下载'\necho 'LMM_PROGRESS_BYTES=10/100'\nsleep 30\n";

    /// 磁盘余量闸清零：测试盯的是服务面行为，不该跟着跑测试那台机器还剩多少
    /// 盘走。任务的 job 线程走 ModelManager::new(None, None)——设置从盘上读，
    /// 所以要写进临时 HOME 的 config.json 而不是改全局。
    fn hermetic_disk_gate() {
        config::save_settings(&config::Settings {
            min_free_space_gb: 0.0,
            ..Default::default()
        })
        .unwrap();
    }

    fn wait_task(sock: &Path, run_id: &str, want: &str, timeout: Duration) -> Value {
        let deadline = std::time::Instant::now() + timeout;
        loop {
            let (status, body) = request(sock, "GET", &format!("/v1/tasks/{run_id}"), None);
            assert_eq!(status, 200, "任务详情应该拿得到：{body}");
            if body["status"].as_str().unwrap_or("") == want {
                return body;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "等 {want} 超时，任务停在：{body}"
            );
            std::thread::sleep(Duration::from_millis(100));
        }
    }

    fn cancel_and_drain(sock: &Path, run_id: &str) -> Value {
        let (status, body) = request(
            sock,
            "POST",
            &format!("/v1/tasks/{run_id}/cancel"),
            Some("{}"),
        );
        assert_eq!(status, 200, "取消应答应成功：{body}");
        assert_eq!(body["status"], "cancelling");
        for _ in 0..150 {
            let (_, detail) = request(sock, "GET", &format!("/v1/tasks/{run_id}"), None);
            if detail["status"].as_str().unwrap_or("running") != "running" {
                return detail;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        panic!("取消之后任务一直没有到达终态");
    }

    /// 服务面端到端：真监听 + 极简客户端，覆盖下发形状、任务列表/详情、
    /// 进度解析、互斥 409、卡片不存在 404、取消到 cancelled 终态。
    #[test]
    fn install_local_dispatch_tasks_and_cancel_over_a_real_socket() {
        let (_guard, home) = temp_home();
        hermetic_disk_gate();
        let cards_dir = home.path().join(".pinvou3/model-store/cards");
        write_fixture_card(&cards_dir, "sock-slow", SLOW_DOWNLOAD_SH);

        let sock = home.path().join("svc/test.sock");
        let service = Arc::new(ModelStoreService::new());
        let svc =
            LocalService::bind(Arc::clone(&service), sock.clone(), true).expect("绑定临时 socket");

        // ① 下发：形状必须是 {"run_id","kind","card"}
        let (status, body) = request(
            &sock,
            "POST",
            "/v1/install_local",
            Some("{\"card_id\":\"sock-slow\"}"),
        );
        assert_eq!(status, 200, "下发应答应成功：{body}");
        let run_id = body["run_id"].as_str().expect("带 run_id").to_string();
        assert!(run_id.starts_with("run-"), "run_id 形状：{run_id}");
        assert_eq!(body["kind"], "install_local");
        assert_eq!(body["card"], "sock-slow");

        // ② 互斥：同一张卡再来一发，当场 409 中文错误
        let (status, body) = request(
            &sock,
            "POST",
            "/v1/install_local",
            Some("{\"card_id\":\"sock-slow\"}"),
        );
        assert_eq!(status, 409, "互斥必须当场拦下：{body}");
        assert_eq!(body["error"]["code"], "card_busy");
        assert!(
            body["error"]["message"].as_str().unwrap().contains("正在"),
            "409 要说是正在进行的什么操作：{body}"
        );

        // ③ 卡片不存在：install_local 404
        let (status, body) = request(
            &sock,
            "POST",
            "/v1/install_local",
            Some("{\"card_id\":\"no-such-card\"}"),
        );
        assert_eq!(status, 404);
        assert_eq!(body["error"]["code"], "card_not_found");
        assert!(
            body["error"]["message"]
                .as_str()
                .unwrap()
                .contains("不存在"),
            "404 文案要是人话：{body}"
        );

        // ④ 任务列表：这条任务在，且状态/卡片/种类/进度都对
        let detail = wait_task(&sock, &run_id, "running", Duration::from_secs(15));
        let _ = detail;
        let (status, body) = request(&sock, "GET", "/v1/tasks", None);
        assert_eq!(status, 200);
        let tasks = body["tasks"].as_array().unwrap();
        let task = tasks
            .iter()
            .find(|t| t["run_id"] == run_id)
            .expect("任务列表里找得到刚下发的任务");
        assert_eq!(task["card_id"], "sock-slow");
        assert_eq!(task["kind"], "install_local");
        assert_eq!(task["status"], "running");
        assert!(!task["created_at"].as_str().unwrap().is_empty());
        // 进度行打出来之后，列表里要解析得出 {done,total}
        let deadline = std::time::Instant::now() + Duration::from_secs(15);
        loop {
            let (_, body) = request(&sock, "GET", "/v1/tasks", None);
            let task = body["tasks"]
                .as_array()
                .unwrap()
                .iter()
                .find(|t| t["run_id"] == run_id)
                .unwrap()
                .clone();
            if task["progress"].is_object() {
                assert_eq!(task["progress"], json!({"done": 10, "total": 100}));
                break;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "进度一直没出现：{task}"
            );
            std::thread::sleep(Duration::from_millis(100));
        }

        // ⑤ 单任务详情：带 lines（末尾日志），last_line 有内容
        let (_, detail) = request(&sock, "GET", &format!("/v1/tasks/{run_id}"), None);
        let lines = detail["lines"].as_array().expect("详情带 lines");
        assert!(
            lines
                .iter()
                .any(|l| l.as_str().unwrap_or("").contains("开始下载")),
            "日志里要有下载段的输出：{detail}"
        );
        assert!(detail["last_line"].as_str().is_some());

        // ⑥ 取消：先回 cancelling，终态必须是 cancelled——不是 failed
        let terminal = cancel_and_drain(&sock, &run_id);
        assert_eq!(
            terminal["status"], "cancelled",
            "取消要单列成态，不能记成失败：{terminal}"
        );
        assert!(
            terminal["error"].as_str().unwrap_or("").contains("已取消"),
            "取消的原因要带出来：{terminal}"
        );

        // ⑦ 任务不存在：详情和取消都是 404
        let (status, body) = request(&sock, "GET", "/v1/tasks/run-nope", None);
        assert_eq!(status, 404);
        assert_eq!(body["error"]["code"], "task_not_found");
        let (status, body) = request(&sock, "POST", "/v1/tasks/run-nope/cancel", Some("{}"));
        assert_eq!(status, 404);
        assert_eq!(body["error"]["code"], "task_not_found");

        svc.shutdown();
    }

    #[test]
    fn incomplete_install_retry_preserves_files_and_delegated_identity_over_a_real_socket() {
        let (_guard, home) = temp_home();
        hermetic_disk_gate();
        let card = "sock-incomplete";
        let cards = config::data_dir().join("cards");
        write_fixture_card(
            &cards,
            card,
            "#!/bin/sh\nset -e\nmkdir -p \"$LMM_MODELS_DIR/retry-fixture\" .runtime\nfile=\"$LMM_MODELS_DIR/retry-fixture/partial.bin\"\n[ -f \"$file\" ] || printf partial > \"$file\"\necho invocation >> .runtime/calls\necho LMM_PROGRESS_BYTES=7/11\n[ -f .runtime/complete ] || sleep 30\nprintf done >> \"$file\"\n",
        );
        let sock = home.path().join("svc/test.sock");
        let service = Arc::new(ModelStoreService::new());
        let svc = LocalService::bind(Arc::clone(&service), sock.clone(), true).unwrap();
        let (_, accepted) = request(
            &sock,
            "POST",
            "/v1/install_local",
            Some(&json!({"card_id":card}).to_string()),
        );
        let original = accepted["run_id"].as_str().unwrap().to_string();
        let deadline = std::time::Instant::now() + Duration::from_secs(15);
        loop {
            let (_, task) = request(&sock, "GET", &format!("/v1/tasks/{original}"), None);
            if !task["progress"].is_null() {
                break;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "fixture did not reach download"
            );
            std::thread::sleep(Duration::from_millis(50));
        }
        assert_eq!(cancel_and_drain(&sock, &original)["status"], "cancelled");
        assert!(super::super::db::get_store_model(card).unwrap().is_none());
        let partial = config::data_dir().join("models/retry-fixture/partial.bin");
        assert_eq!(std::fs::read(&partial).unwrap(), b"partial");
        let (status, preview) = request(
            &sock,
            "POST",
            "/v1/ui/incomplete_preview",
            Some(&json!({"run_id":original}).to_string()),
        );
        assert_eq!(status, 200, "{preview}");
        let digest = preview["result"]["previewSha256"].as_str().unwrap();
        std::fs::write(cards.join(card).join(".runtime/complete"), "go").unwrap();
        let body = json!({"original_run_id":original,"expected_preview_sha256":digest,"run_id":"cmsd-retry-fixture"});
        let (status, first) = request(&sock, "POST", "/v1/retry_install", Some(&body.to_string()));
        assert_eq!(status, 200, "{first}");
        assert_eq!(
            first,
            json!({"run_id":"cmsd-retry-fixture","kind":"install_local","card":card})
        );
        assert_eq!(
            request(&sock, "POST", "/v1/retry_install", Some(&body.to_string())),
            (200, first.clone())
        );
        let mut conflicting = body.clone();
        conflicting["expected_preview_sha256"] = json!("0".repeat(64));
        assert_eq!(
            request(
                &sock,
                "POST",
                "/v1/retry_install",
                Some(&conflicting.to_string())
            )
            .0,
            409
        );
        conflicting = body.clone();
        conflicting["original_run_id"] = json!("another-original-run");
        assert_eq!(
            request(
                &sock,
                "POST",
                "/v1/retry_install",
                Some(&conflicting.to_string())
            )
            .0,
            409
        );
        let mut extra = body.clone();
        extra["confirmed"] = json!(true);
        assert_eq!(
            request(&sock, "POST", "/v1/retry_install", Some(&extra.to_string())).0,
            400
        );
        let duplicate = format!(
            "{{\"original_run_id\":\"{original}\",\"original_run_id\":\"{original}\",\"expected_preview_sha256\":\"{digest}\",\"run_id\":\"cmsd-retry-fixture\"}}"
        );
        assert_eq!(
            request(&sock, "POST", "/v1/retry_install", Some(&duplicate)).0,
            400
        );
        wait_task(
            &sock,
            "cmsd-retry-fixture",
            "succeeded",
            Duration::from_secs(20),
        );
        assert_eq!(std::fs::read(&partial).unwrap(), b"partialdone");
        assert_eq!(
            super::super::db::get_store_model(card)
                .unwrap()
                .unwrap()
                .status,
            "deployed"
        );
        let calls = std::fs::read(cards.join(card).join(".runtime/calls")).unwrap();
        assert_eq!(calls, b"invocation\ninvocation\n");
        svc.shutdown();
        let restored = Arc::new(ModelStoreService::new());
        restored.restore_runs().unwrap();
        let svc2 = LocalService::bind(restored, sock.clone(), true).unwrap();
        assert_eq!(
            request(&sock, "POST", "/v1/retry_install", Some(&body.to_string())),
            (200, first)
        );
        assert_eq!(
            std::fs::read(cards.join(card).join(".runtime/calls")).unwrap(),
            calls
        );
        svc2.shutdown();
    }

    /// /v1/install（商店卡）那条路：下发形状、404、互斥 409。
    ///
    /// 走 test_mode（内置测试商店，全离线）：卡片确认与下发本身都不发网络
    /// 请求；任务线程内部的脚本执行会失败或被杀，那不是这条测试关心的——
    /// 这里钉的是**服务面**的行为。
    #[test]
    fn store_install_dispatch_404_and_conflict() {
        let (_guard, home) = temp_home();
        hermetic_disk_gate();
        let cards_dir = home.path().join(".pinvou3/model-store/cards");
        // 与内置测试商店的一张卡同名：互斥断言需要「先占住这张卡」，
        // 而 /v1/install 的卡片确认又要求商店里真有它。
        write_fixture_card(&cards_dir, "ornith15-bf16", SLOW_DOWNLOAD_SH);

        let sock = home.path().join("svc/install.sock");
        let service = Arc::new(ModelStoreService::new());
        let svc =
            LocalService::bind(Arc::clone(&service), sock.clone(), true).expect("绑定临时 socket");

        // 先占住这张卡
        let (status, body) = request(
            &sock,
            "POST",
            "/v1/install_local",
            Some("{\"card_id\":\"ornith15-bf16\"}"),
        );
        assert_eq!(status, 200, "{body}");
        let local_run = body["run_id"].as_str().unwrap().to_string();

        // 商店里没有的卡：404
        let (status, body) = request(
            &sock,
            "POST",
            "/v1/install",
            Some("{\"card_id\":\"no-such-store-card\"}"),
        );
        assert_eq!(status, 404, "商店里没有的卡要当场 404：{body}");
        assert_eq!(body["error"]["code"], "card_not_found");

        // 商店里有、但被占着的卡：409（预检过、互斥拦）
        let (status, body) = request(
            &sock,
            "POST",
            "/v1/install",
            Some("{\"card_id\":\"ornith15-bf16\"}"),
        );
        assert_eq!(status, 409, "互斥必须当场拦下：{body}");
        assert_eq!(body["error"]["code"], "card_busy");

        // 坏请求也要说人话：缺 card_id 与非法 id
        let (status, body) = request(&sock, "POST", "/v1/install", Some("{}"));
        assert_eq!(status, 400);
        assert_eq!(body["error"]["code"], "bad_request");
        let (status, body) = request(
            &sock,
            "POST",
            "/v1/install",
            Some("{\"card_id\":\"../escape\"}"),
        );
        assert_eq!(status, 400, "路径逃逸形状的 id 必须拒：{body}");

        // 放开互斥，再发商店下载：200 + kind=download
        let terminal = cancel_and_drain(&sock, &local_run);
        assert_eq!(terminal["status"], "cancelled");
        let (status, body) = request(
            &sock,
            "POST",
            "/v1/install",
            Some("{\"card_id\":\"ornith15-bf16\"}"),
        );
        assert_eq!(status, 200, "互斥放开后下发要成功：{body}");
        let run_id = body["run_id"].as_str().expect("带 run_id").to_string();
        assert!(run_id.starts_with("run-"));
        assert_eq!(body["kind"], "download");
        assert_eq!(body["card"], "ornith15-bf16");

        // 立刻取消：任务最终要停（终态是 cancelled 还是 failed 取决于取消
        // 落在预检前还是后，这里只钉「取消被接受且任务不再跑」）。
        let terminal = cancel_and_drain(&sock, &run_id);
        assert!(
            terminal["status"] == "cancelled" || terminal["status"] == "failed",
            "取消后任务必须到达终态：{terminal}"
        );

        svc.shutdown();
    }

    /// sock 文件的生命周期：0600 创建、退出清理、残留重绑。
    ///
    /// 这条同时是「fd 泄漏与僵尸线程」的实证半边：shutdown 返回时 accept
    /// 线程已被 join（构造上保证），sock 文件已删、监听 fd 已放——所以同
    /// 一路径能立刻再绑。另半边（连接线程随请求终结）由每个请求应答后
    /// 客户端读到 EOF 来佐证。
    #[test]
    fn socket_file_lifecycle_create_0600_cleanup_and_rebind() {
        let (_guard, home) = temp_home();
        let sock = home.path().join("svc/life.sock");
        let service = Arc::new(ModelStoreService::new());

        // 造一个上次的残留：普通文件占着路径，bind 要清掉它再绑
        std::fs::create_dir_all(sock.parent().unwrap()).unwrap();
        std::fs::write(&sock, b"stale").unwrap();
        let svc = LocalService::bind(Arc::clone(&service), sock.clone(), true)
            .expect("残留文件不该挡住绑定");

        use std::os::unix::fs::PermissionsExt as _;
        let mode = std::fs::metadata(&sock).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "sock 文件权限是 {mode:o}，同机其它用户连得上");

        // 服务能用
        let (status, body) = request(&sock, "GET", "/v1/tasks", None);
        assert_eq!(status, 200);
        assert_eq!(body, json!({"tasks": []}));

        // 关停：sock 文件删掉、监听释放（同址立刻能再绑）
        svc.shutdown();
        assert!(!sock.exists(), "退出后 sock 文件必须清掉");
        assert!(
            UnixStream::connect(&sock).is_err(),
            "关停之后还能连上，说明监听没有放掉"
        );
        let svc2 = LocalService::bind(Arc::clone(&service), sock.clone(), true)
            .expect("同一路径应当能立刻重新绑定");
        svc2.shutdown();

        // 活监听者不抢：第三个 bind 在 svc3 活着时必须报错
        let svc3 = LocalService::bind(Arc::clone(&service), sock.clone(), true).unwrap();
        assert!(
            LocalService::bind(Arc::clone(&service), sock.clone(), true).is_err(),
            "已有活监听者时重复绑定必须失败，不能抢地址"
        );
        svc3.shutdown();
    }

    /// 请求解析：连请求行都不完整的输入回 400 中文错误，而不是把连接晾着。
    #[test]
    fn malformed_requests_get_a_chinese_400() {
        let (_guard, home) = temp_home();
        let sock = home.path().join("svc/bad.sock");
        let service = Arc::new(ModelStoreService::new());
        let svc = LocalService::bind(Arc::clone(&service), sock.clone(), true).unwrap();

        let mut stream = UnixStream::connect(&sock).unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        stream.write_all(b"garbage\r\n\r\n").unwrap();
        let mut raw = Vec::new();
        stream.read_to_end(&mut raw).unwrap();
        let text = String::from_utf8(raw).unwrap();
        assert!(text.starts_with("HTTP/1.1 400"), "应答：{text}");
        assert!(text.contains("bad_request"), "应答：{text}");

        // 未知路由：404 + 中文
        let (status, body) = request(&sock, "GET", "/v1/nope", None);
        assert_eq!(status, 404);
        assert_eq!(body["error"]["code"], "unknown_endpoint");
        svc.shutdown();
    }
}
