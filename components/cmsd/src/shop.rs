//! 商店服务客户端（2026-10-14 架构拍板：品悟模型商店是唯一下载执行者，
//! cmsd 的 install 改为委托门面）。
//!
//! 协议：`<PINVOU3_HOME>/model-store/service.sock`（缺省 ~/.pinvou3/model-store/）
//! 上的极简 HTTP/1.1 JSON over unix socket（与 cmsd 同款形态）：
//!
//! - POST /v1/install        {"card_id"} → {"run_id","kind":"download","card"}
//! - POST /v1/install_local  {"card_id"} → 同上（cards_dir 已物化的本地卡）
//! - GET  /v1/tasks          → {"tasks":[{run_id,card_id,kind,status,created_at,created_at_epoch,progress:{done,total}|null,last_line,error}]}
//! - GET  /v1/tasks/<id>     → 单任务 + lines（末尾日志行）
//! - POST /v1/tasks/<id>/cancel → {"status":"cancelling"}
//!
//! Pinvou 的 created_at 为 RFC3339，created_at_epoch 为配套 epoch 秒；
//! 旧整数 created_at 仍可读取，缺失/非法时间保留未知，不以查询时间补造。
//!
//! 错误形状：{"error":{"code","message"}}；卡片不存在 404、互斥 409（透传中文）。
//! 商店服务端位于 pinvou3 仓；本模块遵循上述本机 API 合同。

use crate::http;
use crate::json::Value;
use std::io::{BufReader, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::time::Duration;

struct DeadlineStream {
    stream: UnixStream,
    deadline: std::time::Instant,
}
impl DeadlineStream {
    fn remaining(&self) -> std::io::Result<Duration> {
        self.deadline
            .checked_duration_since(std::time::Instant::now())
            .filter(|v| !v.is_zero())
            .ok_or_else(|| {
                std::io::Error::new(
                    std::io::ErrorKind::TimedOut,
                    "store request deadline exceeded",
                )
            })
    }
}
impl Read for DeadlineStream {
    fn read(&mut self, buffer: &mut [u8]) -> std::io::Result<usize> {
        self.stream.set_read_timeout(Some(self.remaining()?))?;
        self.stream.read(buffer)
    }
}
impl Write for DeadlineStream {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        self.stream.set_write_timeout(Some(self.remaining()?))?;
        self.stream.write(bytes)
    }
    fn flush(&mut self) -> std::io::Result<()> {
        self.stream.flush()
    }
}

#[derive(Debug, Clone)]
pub struct StoreClient {
    sock: PathBuf,
}

/// 商店侧任务（/v1/tasks 列表项与 /v1/tasks/<id> 共用的核心字段）。
#[derive(Debug, Clone)]
pub struct StoreTask {
    pub run_id: String,
    pub card_id: String,
    pub kind: String,
    pub status: String,
    pub created_at: u64,
    pub progress: Option<(u64, u64)>,
    pub last_line: String,
    pub error: String,
}

/// 单任务详情（列表字段 + 末尾日志行）。
#[derive(Debug, Clone)]
pub struct StoreTaskDetail {
    pub line_offset: usize,
    pub task: StoreTask,
    pub lines: Vec<String>,
}

#[derive(Debug)]
pub struct StoreReceipt {
    pub run_id: String,
    pub kind: String,
    pub card: String,
}

#[derive(Debug)]
pub enum StoreErr {
    /// Connection failed before any request bytes were sent.
    NotSent(String),
    /// socket 缺失/连接失败：品悟未在运行。
    Offline(String),
    /// 商店给了结构化错误（404/409 等）：code/message 原样透传。
    Http(u16, String, String),
    /// 应答不是预期的协议形状（对端可能不是商店服务）。
    Protocol(String),
}

impl StoreClient {
    pub fn new(sock: &Path) -> StoreClient {
        StoreClient {
            sock: sock.to_path_buf(),
        }
    }

    /// 委托商店下载本地已物化卡（MCP 造卡/完整六脚本在 cards_dir）。
    #[cfg(test)]
    pub fn install_local(&self, card_id: &str) -> Result<String, StoreErr> {
        self.post_run("/v1/install_local", card_id)
    }

    pub fn install_request(
        &self,
        card_id: &str,
        request_id: &str,
        local: bool,
    ) -> Result<String, StoreErr> {
        let path = if local {
            "/v1/install_local"
        } else {
            "/v1/install"
        };
        let mut request = Value::obj();
        request.set("card_id", card_id.into());
        request.set("request_id", request_id.into());
        let (status, text) = self.request("POST", path, Some(&request.to_string()))?;
        let value = self.parse_json(status, &text)?;
        value
            .get("run_id")
            .and_then(Value::as_str)
            .map(str::to_string)
            .ok_or_else(|| StoreErr::Protocol("install response missing run_id".into()))
    }

    /// Exact durable replay only; this endpoint never falls back to install.
    pub fn retry_install(
        &self,
        retry: &crate::store::StoreRetry,
        run_id: &str,
    ) -> Result<StoreReceipt, StoreErr> {
        let mut body = Value::obj();
        body.set("original_run_id", retry.original_run_id.clone().into());
        body.set(
            "expected_preview_sha256",
            retry.expected_preview_sha256.clone().into(),
        );
        body.set("run_id", run_id.into());
        let (status, text) = self.request("POST", "/v1/retry_install", Some(&body.to_string()))?;
        let value = self.parse_json(status, &text)?;
        let field = |key| {
            value
                .get(key)
                .and_then(Value::as_str)
                .map(str::to_string)
                .ok_or_else(|| StoreErr::Protocol(format!("retry receipt missing {key}")))
        };
        Ok(StoreReceipt {
            run_id: field("run_id")?,
            kind: field("kind")?,
            card: field("card")?,
        })
    }

    #[cfg(test)]
    fn post_run(&self, path: &str, card_id: &str) -> Result<String, StoreErr> {
        let body = format!("{{\"card_id\":\"{card_id}\"}}");
        let (status, text) = self.request("POST", path, Some(&body))?;
        let v = self.parse_json(status, &text)?;
        v.get("run_id")
            .and_then(|x| x.as_str())
            .map(|s| s.to_string())
            .ok_or_else(|| StoreErr::Protocol(format!("{path} 应答缺 run_id：{text}")))
    }

    /// Invalid records fail the query; a partial list cannot prove absence.
    pub fn tasks(&self) -> Result<Vec<StoreTask>, StoreErr> {
        let (status, text) = self.request("GET", "/v1/tasks", None)?;
        let v = self.parse_json(status, &text)?;
        let arr = v
            .get("tasks")
            .and_then(|x| x.as_arr())
            .ok_or_else(|| StoreErr::Protocol(format!("/v1/tasks 应答缺 tasks 数组：{text}")))?;
        arr.iter()
            .map(|value| {
                parse_store_task(value)
                    .ok_or_else(|| StoreErr::Protocol("invalid store task in list".into()))
            })
            .collect()
    }

    /// 单任务详情（镜像 worker 轮询用）。
    pub fn task(&self, run_id: &str) -> Result<StoreTaskDetail, StoreErr> {
        if !crate::store::safe_store_id(run_id) {
            return Err(StoreErr::Http(
                400,
                "invalid_task_id".into(),
                "任务标识不合法".into(),
            ));
        }
        let path = format!("/v1/tasks/{run_id}");
        let (status, text) = self.request("GET", &path, None)?;
        let v = self.parse_json(status, &text)?;
        let task = parse_store_task(&v)
            .ok_or_else(|| StoreErr::Protocol(format!("单任务应答缺字段：{text}")))?;
        let lines = v
            .get("lines")
            .and_then(|x| x.as_arr())
            .map(|a| {
                a.iter()
                    .filter_map(|l| l.as_str().map(|s| s.to_string()))
                    .collect()
            })
            .unwrap_or_default();
        let line_offset = v.get("line_offset").and_then(Value::as_u64).unwrap_or(0) as usize;
        Ok(StoreTaskDetail {
            task,
            lines,
            line_offset,
        })
    }

    /// 取消商店侧任务。
    pub fn cancel(&self, run_id: &str) -> Result<(), StoreErr> {
        if !crate::store::safe_store_id(run_id) {
            return Err(StoreErr::Http(
                400,
                "invalid_task_id".into(),
                "任务标识不合法".into(),
            ));
        }
        let path = format!("/v1/tasks/{run_id}/cancel");
        let (status, text) = self.request("POST", &path, None)?;
        let _ = self.parse_json(status, &text)?;
        Ok(())
    }

    fn parse_json(&self, status: u16, text: &str) -> Result<Value, StoreErr> {
        let v =
            Value::parse(text).map_err(|e| StoreErr::Protocol(format!("应答不是 JSON：{e}")))?;
        if (200..300).contains(&status) {
            return Ok(v);
        }
        // 结构化错误：{"error":{"code","message"}}
        let code = v
            .get("error")
            .and_then(|e| e.get("code"))
            .and_then(|x| x.as_str())
            .unwrap_or("store_error")
            .to_string();
        let message = v
            .get("error")
            .and_then(|e| e.get("message"))
            .and_then(|x| x.as_str())
            .unwrap_or(text)
            .to_string();
        Err(StoreErr::Http(status, code, message))
    }

    fn request(
        &self,
        method: &str,
        path: &str,
        body: Option<&str>,
    ) -> Result<(u16, String), StoreErr> {
        let budget = Duration::from_secs(5);
        let started = std::time::Instant::now();
        let stream = http::connect_unix(&self.sock, budget)
            .map_err(|e| StoreErr::NotSent(format!("{}：{e}", self.sock.display())))?;
        let mut stream = DeadlineStream {
            stream,
            deadline: started + budget,
        };
        let mut req = format!("{method} {path} HTTP/1.1\r\nhost: store\r\nconnection: close\r\n");
        if let Some(b) = body {
            req.push_str(&format!(
                "content-type: application/json\r\ncontent-length: {}\r\n",
                b.len()
            ));
        }
        req.push_str("\r\n");
        if let Some(b) = body {
            req.push_str(b);
        }
        stream
            .write_all(req.as_bytes())
            .and_then(|_| stream.flush())
            .map_err(|e| StoreErr::Offline(format!("写商店 socket 失败：{e}")))?;
        let mut reader = BufReader::new(stream);
        let resp = http::read_response(&mut reader)
            .map_err(|e| StoreErr::Offline(format!("应答读取失败：{e}")))?;
        let text = String::from_utf8(resp.body)
            .map_err(|_| StoreErr::Protocol("store response UTF-8 invalid".into()))?;
        Ok((resp.status, text))
    }
}

fn parse_store_task(v: &Value) -> Option<StoreTask> {
    let run_id = v.get("run_id")?.as_str()?;
    let card_id = v.get("card_id")?.as_str()?;
    let kind = v.get("kind")?.as_str()?;
    let status = v.get("status")?.as_str()?;
    if !crate::store::safe_store_id(run_id)
        || !crate::store::safe_store_id(card_id)
        || kind.is_empty()
        || kind.len() > 128
        || !["running", "succeeded", "failed", "cancelled"].contains(&status)
    {
        return None;
    }
    let progress = v.get("progress").and_then(|p| {
        let done = p.get("done").and_then(|x| x.as_u64())?;
        let total = p.get("total").and_then(|x| x.as_u64())?;
        Some((done, total))
    });
    Some(StoreTask {
        run_id: run_id.into(),
        card_id: card_id.into(),
        kind: kind.into(),
        status: status.into(),
        // Pinvou keeps its RFC3339 UI field and supplies the matching epoch
        // separately. Older integer producers remain readable; 0 is unknown.
        created_at: v
            .get("created_at_epoch")
            .and_then(Value::as_u64)
            .or_else(|| v.get("created_at").and_then(Value::as_u64))
            .unwrap_or(0),
        progress,
        last_line: v
            .get("last_line")
            .and_then(|x| x.as_str())
            .unwrap_or_default()
            .to_string(),
        error: v
            .get("error")
            .and_then(|x| x.as_str())
            .unwrap_or_default()
            .to_string(),
    })
}

#[cfg(test)]
mod store_task_time_tests {
    use super::*;

    #[test]
    fn pinvou_production_timestamp_preserves_epoch_and_task_facts() {
        // Generated by Pinvou 36a0b4537's production local_service::task_json, whose
        // real-socket contract test also checks both timestamp representations.
        let value = Value::parse(include_str!(
            "../tests/fixtures/pinvou-store-task-rfc3339.json"
        ))
        .unwrap();
        let task = parse_store_task(&value).unwrap();
        assert_eq!(task.created_at, 1_792_569_600);
        assert_eq!(task.run_id, "run-surface-new");
        assert_eq!(task.card_id, "surface-card");
        assert_eq!(task.kind, "download");
        assert_eq!(task.status, "running");
        assert_eq!(task.progress, Some((1024, 4096)));
    }

    #[test]
    fn store_timestamps_use_known_epoch_or_legacy_integer_without_guessing() {
        let original = Value::parse(include_str!(
            "../tests/fixtures/pinvou-store-task-rfc3339.json"
        ))
        .unwrap();
        let mut both = original.clone();
        both.set("created_at", 123_u64.into());
        assert_eq!(parse_store_task(&both).unwrap().created_at, 1_792_569_600);
        let mut legacy = original.clone();
        if let Value::Obj(fields) = &mut legacy {
            fields.remove("created_at_epoch");
        }
        legacy.set("created_at", 123_u64.into());
        assert_eq!(parse_store_task(&legacy).unwrap().created_at, 123);
        for unknown in [Value::Null, "not-a-timestamp".into(), (-1_i64).into()] {
            let mut value = original.clone();
            value.set("created_at_epoch", unknown.clone());
            value.set("created_at", unknown);
            assert_eq!(parse_store_task(&value).unwrap().created_at, 0);
        }
        if let Value::Obj(fields) = &mut legacy {
            fields.remove("created_at");
        }
        assert_eq!(parse_store_task(&legacy).unwrap().created_at, 0);
    }
}

#[cfg(test)]
mod deadline_tests {
    use super::*;
    #[test]
    fn trickle_reads_share_one_absolute_deadline() {
        let (reader, mut writer) = UnixStream::pair().unwrap();
        let worker = std::thread::spawn(move || {
            for _ in 0..40 {
                if writer.write_all(b"x").is_err() {
                    break;
                }
                std::thread::sleep(Duration::from_millis(5));
            }
        });
        let started = std::time::Instant::now();
        let mut reader = DeadlineStream {
            stream: reader,
            deadline: started + Duration::from_millis(45),
        };
        let error = reader.read_to_end(&mut Vec::new()).unwrap_err();
        assert!(matches!(
            error.kind(),
            std::io::ErrorKind::TimedOut | std::io::ErrorKind::WouldBlock
        ));
        assert!(started.elapsed() < Duration::from_millis(180));
        drop(reader);
        worker.join().unwrap();
    }
    #[test]
    fn incomplete_or_unknown_task_is_not_a_download() {
        for raw in [
            r#"{"run_id":"run-a","card_id":"a","status":"succeeded"}"#,
            r#"{"run_id":"../other","card_id":"a","kind":"download","status":"running"}"#,
            r#"{"run_id":"run-a","card_id":"a","kind":"download","status":"unknown"}"#,
        ] {
            assert!(parse_store_task(&Value::parse(raw).unwrap()).is_none());
        }
    }
}

#[cfg(test)]
pub(crate) mod fixtures {
    //! fixture 商店服务：内存态驱动的 fake，给 install/install_local/tasks/
    //! tasks/<id>/cancel 五个端点。任务进度用「每次轮询推一步」的确定脚本
    //! （不依赖时间，测试不抖）。
    use super::*;
    use crate::http;
    use std::collections::BTreeMap;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::{Arc, Mutex};

    static SEQ: AtomicU64 = AtomicU64::new(0);

    /// 一个任务的推进剧本：每次 GET /v1/tasks/<id> 弹一步。
    #[derive(Debug, Clone)]
    pub enum Step {
        Line(&'static str),
        Progress(u64, u64),
        Succeed,
        Fail(&'static str),
    }

    struct FakeTask {
        run_id: String,
        card_id: String,
        kind: String,
        status: String,
        created_at: u64,
        done: u64,
        total: u64,
        lines: Vec<String>,
        error: String,
        steps: Vec<Step>,
    }

    impl FakeTask {
        fn advance(&mut self) {
            let Some(step) = self.steps.first().cloned() else {
                return;
            };
            self.steps.remove(0);
            match step {
                Step::Line(l) => self.lines.push(l.to_string()),
                Step::Progress(d, t) => {
                    self.done = d;
                    self.total = t;
                }
                Step::Succeed => self.status = "succeeded".to_string(),
                Step::Fail(e) => {
                    self.status = "failed".to_string();
                    self.error = e.to_string();
                }
            }
        }

        fn json(&self, with_lines: bool) -> Value {
            let mut v = Value::obj();
            v.set("run_id", self.run_id.clone().into());
            v.set("card_id", self.card_id.clone().into());
            v.set("kind", self.kind.clone().into());
            v.set("status", self.status.clone().into());
            v.set("created_at", self.created_at.into());
            if self.total > 0 || self.done > 0 {
                let mut p = Value::obj();
                p.set("done", self.done.into());
                p.set("total", self.total.into());
                v.set("progress", p);
            } else {
                v.set("progress", Value::Null);
            }
            v.set(
                "last_line",
                self.lines.last().cloned().unwrap_or_default().into(),
            );
            v.set("error", self.error.clone().into());
            if with_lines {
                v.set(
                    "lines",
                    Value::Arr(self.lines.iter().map(|l| l.clone().into()).collect()),
                );
            }
            v
        }
    }

    struct FakeState {
        tasks: BTreeMap<String, FakeTask>,
        /// 商店目录里有这些卡（install 可下发）；其余 → 404 unknown_card
        catalog: Vec<String>,
        /// 本地已物化卡（install_local 可下发）；其余 → 404 unknown_local_card
        locals: Vec<String>,
        counter: u64,
    }

    pub struct FakeStore {
        state: Arc<Mutex<FakeState>>,
    }

    impl FakeStore {
        /// 起服务（后台线程每连接）。catalog/locals 决定两个 install 端点的 404 面。
        pub fn start(dir: &Path, catalog: &[&str], locals: &[&str]) -> FakeStore {
            let sock = dir.join("store.sock");
            let _ = std::fs::remove_file(&sock);
            let listener = std::os::unix::net::UnixListener::bind(&sock).unwrap();
            let state = Arc::new(Mutex::new(FakeState {
                tasks: BTreeMap::new(),
                catalog: catalog.iter().map(|s| s.to_string()).collect(),
                locals: locals.iter().map(|s| s.to_string()).collect(),
                counter: 0,
            }));
            let state2 = Arc::clone(&state);
            std::thread::spawn(move || {
                for stream in listener.incoming() {
                    let Ok(s) = stream else { continue };
                    let st = Arc::clone(&state2);
                    std::thread::spawn(move || handle_conn(s, st));
                }
            });
            FakeStore { state }
        }

        /// 给某个卡预置推进剧本（下次 install 下发时按它跑）。
        pub fn script_for(&self, card: &str, steps: Vec<Step>) {
            let mut st = self.state.lock().unwrap();
            st.tasks.insert(
                format!("__script__{card}"),
                FakeTask {
                    run_id: String::new(),
                    card_id: card.to_string(),
                    kind: String::new(),
                    status: String::new(),
                    created_at: 0,
                    done: 0,
                    total: 0,
                    lines: Vec::new(),
                    error: String::new(),
                    steps,
                },
            );
        }

        /// 当前跑着的任务数（断言商店侧互斥用）。
        pub fn running_count(&self) -> usize {
            let st = self.state.lock().unwrap();
            st.tasks
                .values()
                .filter(|t| t.status == "running" && !t.run_id.starts_with("__script__"))
                .count()
        }

        /// 某卡是否被下发过（install/install_local 命中记录）。
        pub fn dispatched(&self, card: &str) -> Vec<String> {
            let st = self.state.lock().unwrap();
            st.tasks
                .values()
                .filter(|t| t.card_id == card && !t.run_id.starts_with("__script__"))
                .map(|t| t.kind.clone())
                .collect()
        }
    }

    fn write_json(stream: &mut UnixStream, status: &str, v: Value) {
        let _ = http::write_response(
            stream,
            status,
            "application/json",
            v.to_string().as_bytes(),
            &[],
        );
    }

    fn err_json(stream: &mut UnixStream, status: &str, code: &str, message: &str) {
        let mut v = Value::obj();
        let mut e = Value::obj();
        e.set("code", code.into());
        e.set("message", message.into());
        v.set("error", e);
        write_json(stream, status, v);
    }

    fn handle_conn(mut stream: UnixStream, state: Arc<Mutex<FakeState>>) {
        let cloned = match stream.try_clone() {
            Ok(s) => s,
            Err(_) => return,
        };
        let mut reader = BufReader::new(cloned);
        let req = match http::read_request(&mut reader) {
            Ok(Some(r)) => r,
            _ => return,
        };
        let segments: Vec<&str> = req.path.split('/').filter(|s| !s.is_empty()).collect();
        match (req.method.as_str(), segments.as_slice()) {
            ("POST", ["v1", kind @ ("install" | "install_local")]) => {
                let body = String::from_utf8_lossy(&req.body).to_string();
                let card = Value::parse(&body)
                    .ok()
                    .and_then(|v| {
                        v.get("card_id")
                            .and_then(|x| x.as_str())
                            .map(|s| s.to_string())
                    })
                    .unwrap_or_default();
                let mut st = state.lock().unwrap();
                let known = if *kind == "install" {
                    st.catalog.contains(&card)
                } else {
                    st.locals.contains(&card)
                };
                if !known {
                    let code = if *kind == "install" {
                        "unknown_card"
                    } else {
                        "unknown_local_card"
                    };
                    return err_json(
                        &mut stream,
                        "404 Not Found",
                        code,
                        &format!("商店目录里没有卡片 {card}"),
                    );
                }
                if let Some(t) = st
                    .tasks
                    .values()
                    .find(|t| t.status == "running" && !t.run_id.starts_with("__script__"))
                {
                    let card = t.card_id.clone();
                    return err_json(
                        &mut stream,
                        "409 Conflict",
                        "busy",
                        &format!("当前有下载任务进行中（{card}），请等它结束"),
                    );
                }
                st.counter += 1;
                let run_id = format!("r-{}", st.counter);
                let steps = st
                    .tasks
                    .remove(&format!("__script__{card}"))
                    .map(|s| s.steps)
                    .unwrap_or_default();
                let task = FakeTask {
                    run_id: run_id.clone(),
                    card_id: card.clone(),
                    kind: if *kind == "install_local" {
                        "install_local"
                    } else {
                        "download"
                    }
                    .to_string(),
                    status: "running".to_string(),
                    created_at: crate::timeutil::now_epoch(),
                    done: 0,
                    total: 0,
                    lines: vec!["商店已接单，开始下载".to_string()],
                    error: String::new(),
                    steps,
                };
                st.tasks.insert(run_id.clone(), task);
                let mut v = Value::obj();
                v.set("run_id", run_id.into());
                v.set("kind", "download".into());
                v.set("card", card.into());
                write_json(&mut stream, "200 OK", v);
            }
            ("GET", ["v1", "tasks"]) => {
                let st = state.lock().unwrap();
                let arr: Vec<Value> = st
                    .tasks
                    .values()
                    .filter(|t| !t.run_id.starts_with("__script__"))
                    .map(|t| t.json(false))
                    .collect();
                let mut v = Value::obj();
                v.set("tasks", Value::Arr(arr));
                write_json(&mut stream, "200 OK", v);
            }
            ("GET", ["v1", "tasks", id]) => {
                let mut st = state.lock().unwrap();
                let Some(t) = st.tasks.get_mut(*id) else {
                    return err_json(
                        &mut stream,
                        "404 Not Found",
                        "unknown_task",
                        &format!("没有任务 {id}"),
                    );
                };
                // 每次轮询推进一步（确定剧本）
                if t.status == "running" {
                    t.advance();
                }
                let v = t.json(true);
                write_json(&mut stream, "200 OK", v);
            }
            ("POST", ["v1", "tasks", id, "cancel"]) => {
                let mut st = state.lock().unwrap();
                let Some(t) = st.tasks.get_mut(*id) else {
                    return err_json(
                        &mut stream,
                        "404 Not Found",
                        "unknown_task",
                        &format!("没有任务 {id}"),
                    );
                };
                if t.status == "running" {
                    t.status = "cancelled".to_string();
                }
                let mut v = Value::obj();
                v.set("status", "cancelling".into());
                write_json(&mut stream, "200 OK", v);
            }
            _ => err_json(
                &mut stream,
                "404 Not Found",
                "unknown_endpoint",
                "不认识的路径",
            ),
        }
    }

    pub fn tmpdir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "cmsd-store-{tag}-{}-{}",
            std::process::id(),
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }
}
