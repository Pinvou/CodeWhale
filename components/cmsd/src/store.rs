//! 状态库（T1）：模型记录、引擎注册表、端口分配、活跃指针、任务记录。
//!
//! JSON 文件 + 原子写（tmp + rename），崩溃不丢上一版。启动对账不信任它，
//! 只当线索——现实（systemd 单元、进程、健康探测）以 reconcile 为准。

use crate::json::Value;
use crate::timeutil::now_epoch;
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

/// Keep persistent histories under the state file bound, including escaping.
pub fn bounded_text(text: &str, maximum: usize) -> String {
    let mut out = String::new();
    for ch in text
        .chars()
        .filter(|c| !c.is_control() || *c == '\n' || *c == '\t')
    {
        if out.len() + ch.len_utf8() > maximum {
            while out.len() + '…'.len_utf8() > maximum {
                if out.pop().is_none() {
                    break;
                }
            }
            if maximum >= '…'.len_utf8() {
                out.push('…');
            }
            break;
        }
        out.push(ch);
    }
    out
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ModelStatus {
    Stopped,
    Starting,
    Running,
    Degraded,
}

impl ModelStatus {
    pub fn as_str(&self) -> &'static str {
        match self {
            ModelStatus::Stopped => "stopped",
            ModelStatus::Starting => "starting",
            ModelStatus::Running => "running",
            ModelStatus::Degraded => "degraded",
        }
    }

    fn from_str(s: &str) -> ModelStatus {
        match s {
            "starting" => ModelStatus::Starting,
            "running" => ModelStatus::Running,
            "degraded" => ModelStatus::Degraded,
            _ => ModelStatus::Stopped,
        }
    }
}

#[derive(Debug, Clone)]
pub struct ModelState {
    /// Discovery is observation only; explicit enable grants local lifecycle ownership.
    pub lifecycle_owned: bool,
    pub card: String,
    pub model_id: String,
    pub engine_sha8: String,
    pub port: u16,
    pub status: ModelStatus,
    pub degraded_reason: Option<String>,
    pub restart_count: u64,
    pub last_restart_at: Option<u64>,
    pub last_ready_at: Option<u64>,
    pub enabled_at: Option<u64>,
    pub weights_bytes: u64,
    /// V2 实测标定：就绪前后系统可用内存差（GiB）。
    pub measured_footprint_gib: Option<f64>,
    /// 引擎进程 VmRSS（GiB，统一内存平台仅供参考——驱动分配不完全计入）。
    pub engine_rss_gib: Option<f64>,
    pub main_pid: Option<u32>,
    /// W6：引擎形态（"native" / "container"）。旧记录无此字段按 native 读。
    pub engine_type: String,
    /// 容器卡：容器名（docker 生命周期操作与容器→卡映射的锚点）。
    pub container_name: Option<String>,
    /// 容器卡：镜像名。
    pub image: Option<String>,
}

#[cfg(test)]
pub fn unit_name_for(card: &str) -> String {
    format!("megacube-model@{card}.service")
}

#[cfg(test)]
pub fn card_from_unit(unit: &str) -> Option<String> {
    unit.strip_prefix("megacube-model@")
        .and_then(|s| s.strip_suffix(".service"))
        .map(|s| s.to_string())
}

/// 已安装引擎（内容寻址目录）登记表项。
#[derive(Debug, Clone)]
pub struct EngineInfo {
    pub sha8: String,
    pub engine_type: String, // "native"（容器引擎 P2 才纳入）
    pub path: String,
    pub first_seen_at: u64,
    pub last_seen_at: u64,
}

/// 野生（非 cmsd 拉起）引擎进程登记（D2 只读收编）。
#[derive(Debug, Clone)]
pub struct ExternalSighting {
    pub sha8: String,
    pub pid: u32,
    /// /proc/<pid>/stat 启动时刻：同 pid 但 start_time 变了 = pid 复用，按新进程轮转。
    pub start_time: u64,
    pub port: Option<u16>,
    pub model_id: Option<String>,
    pub first_seen_at: u64,
    pub last_seen_at: u64,
}

impl ExternalSighting {
    pub fn key(sha8: &str, pid: u32) -> String {
        format!("{sha8}:{pid}")
    }
}

#[derive(Debug, Clone)]
pub struct TaskEvent {
    pub ts: u64,
    pub stage: String,
    pub message: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TaskStatus {
    Running,
    Succeeded,
    Failed,
    /// P2 install 取消终态：与失败分列（error 留给真失败，取消的续传提示写 result）。
    Cancelled,
}

impl TaskStatus {
    pub fn as_str(&self) -> &'static str {
        match self {
            TaskStatus::Running => "running",
            TaskStatus::Succeeded => "succeeded",
            TaskStatus::Failed => "failed",
            TaskStatus::Cancelled => "cancelled",
        }
    }

    fn from_str(s: &str) -> TaskStatus {
        match s {
            "succeeded" => TaskStatus::Succeeded,
            "failed" => TaskStatus::Failed,
            "cancelled" => TaskStatus::Cancelled,
            _ => TaskStatus::Running,
        }
    }

    pub fn is_terminal(&self) -> bool {
        !matches!(self, TaskStatus::Running)
    }
}

#[derive(Debug, Clone)]
pub struct TaskRecord {
    pub id: String,
    pub kind: String,
    pub card: String,
    pub status: TaskStatus,
    pub created_at: u64,
    pub finished_at: Option<u64>,
    pub error: Option<String>,
    pub events: Vec<TaskEvent>,
    /// 结果摘要（如 purge 的释放字节数）。
    pub result: Option<String>,
    /// P2 install 进度：已下载字节数（来自脚本 LMM_PROGRESS_BYTES 行）。
    pub progress_done_bytes: Option<u64>,
    /// P2 install 进度：总量字节数（进度条分母）。
    pub progress_total_bytes: Option<u64>,
    /// 委托门面的商店侧任务 id（2026-10-14 起 install 委托品悟商店执行；
    /// None = 非委托任务或委托没落上）。
    pub store_run_id: Option<String>,
    /// Durable source semantics; a retry may replay only this exact correlation.
    pub store_context: Option<StoreContext>,
    /// Immutable admitted logical/current deployment, retained across restarts.
    pub selection: Option<Value>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StoreContext {
    pub kind: String,
    pub retry: Option<StoreRetry>,
    pub cancel_requested: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StoreRetry {
    pub original_run_id: String,
    pub expected_preview_sha256: String,
}

pub fn safe_store_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_.:".contains(&b))
}

impl StoreContext {
    pub fn to_json(&self) -> Value {
        let mut value = Value::obj();
        value.set("kind", self.kind.clone().into());
        value.set("cancel_requested", self.cancel_requested.into());
        if let Some(retry) = &self.retry {
            value.set("original_run_id", retry.original_run_id.clone().into());
            value.set(
                "expected_preview_sha256",
                retry.expected_preview_sha256.clone().into(),
            );
        }
        value
    }
    fn from_json(value: &Value) -> Option<Self> {
        let object = value.as_obj()?;
        if object.keys().any(|k| {
            ![
                "kind",
                "cancel_requested",
                "original_run_id",
                "expected_preview_sha256",
            ]
            .contains(&k.as_str())
        }) {
            return None;
        }
        let kind = value.get("kind")?.as_str()?;
        if !["download", "install_local"].contains(&kind) {
            return None;
        }
        let cancel_requested = match value.get("cancel_requested")? {
            Value::Bool(v) => *v,
            _ => return None,
        };
        let retry = match (
            value.get("original_run_id"),
            value.get("expected_preview_sha256"),
        ) {
            (None, None) => None,
            (Some(id), Some(hash)) => {
                let id = id.as_str()?;
                let hash = hash.as_str()?;
                if !safe_store_id(id) || !valid_sha256(hash) {
                    return None;
                }
                Some(StoreRetry {
                    original_run_id: id.into(),
                    expected_preview_sha256: hash.into(),
                })
            }
            _ => return None,
        };
        Some(Self {
            kind: kind.into(),
            retry,
            cancel_requested,
        })
    }
}

pub fn valid_sha256(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

const MAX_TASKS: usize = 100;
const MAX_EVENTS_PER_TASK: usize = 500;

#[derive(Debug, Default)]
pub struct State {
    pub version: u32,
    /// None identifies legacy writers which do not preserve control versions.
    pub lifecycle_revision: Option<u64>,
    pub active_model: Option<String>,
    pub models: BTreeMap<String, ModelState>,
    pub engines: BTreeMap<String, EngineInfo>,
    pub externals: BTreeMap<String, ExternalSighting>,
    /// 端口 → 占用卡片（原生池分配记录，D5）。
    pub ports: BTreeMap<u16, String>,
    pub tasks: BTreeMap<String, TaskRecord>,
}

impl State {
    pub fn to_json(&self) -> Value {
        let mut v = Value::obj();
        v.set("version", (self.version as u64).into());
        if let Some(epoch) = self.lifecycle_revision {
            v.set("lifecycle_revision", epoch.into());
        }
        match &self.active_model {
            Some(m) => v.set("active_model", m.clone().into()),
            None => v.set("active_model", Value::Null),
        }
        v.set(
            "models",
            Value::Obj(
                self.models
                    .iter()
                    .map(|(k, m)| (k.clone(), model_to_json(m)))
                    .collect(),
            ),
        );
        v.set(
            "engines",
            Value::Obj(
                self.engines
                    .iter()
                    .map(|(k, e)| (k.clone(), engine_to_json(e)))
                    .collect(),
            ),
        );
        v.set(
            "externals",
            Value::Obj(
                self.externals
                    .iter()
                    .map(|(k, e)| (k.clone(), external_to_json(e)))
                    .collect(),
            ),
        );
        v.set(
            "ports",
            Value::Obj(
                self.ports
                    .iter()
                    .map(|(p, c)| (p.to_string(), c.clone().into()))
                    .collect(),
            ),
        );
        v.set(
            "tasks",
            Value::Obj(
                self.tasks
                    .iter()
                    .map(|(k, t)| (k.clone(), task_to_json(t)))
                    .collect(),
            ),
        );
        v
    }

    pub fn from_json(v: &Value) -> State {
        let mut st = State {
            version: v.get("version").and_then(|x| x.as_u64()).unwrap_or(1) as u32,
            lifecycle_revision: v.get("lifecycle_revision").and_then(Value::as_u64),
            active_model: v
                .get("active_model")
                .and_then(|x| x.as_str())
                .map(|s| s.to_string()),
            ..State::default()
        };
        if let Some(m) = v.get("models").and_then(|x| x.as_obj()) {
            for (k, mv) in m {
                if let Some(model) = model_from_json(mv) {
                    st.models.insert(k.clone(), model);
                }
            }
        }
        if let Some(m) = v.get("engines").and_then(|x| x.as_obj()) {
            for (k, ev) in m {
                if let Some(e) = engine_from_json(ev) {
                    st.engines.insert(k.clone(), e);
                }
            }
        }
        if let Some(m) = v.get("externals").and_then(|x| x.as_obj()) {
            for (k, ev) in m {
                if let Some(e) = external_from_json(ev) {
                    st.externals.insert(k.clone(), e);
                }
            }
        }
        if let Some(m) = v.get("ports").and_then(|x| x.as_obj()) {
            for (k, cv) in m {
                if let (Ok(port), Some(card)) = (k.parse::<u16>(), cv.as_str()) {
                    st.ports.insert(port, card.to_string());
                }
            }
        }
        if let Some(m) = v.get("tasks").and_then(|x| x.as_obj()) {
            for (k, tv) in m {
                if let Some(t) = task_from_json(tv) {
                    st.tasks.insert(k.clone(), t);
                }
            }
        }
        st
    }

    /// 追加任务事件（带界：单任务 500 条，总量 100 个任务）。
    pub fn task_event(&mut self, task_id: &str, stage: &str, message: &str) {
        if let Some(t) = self.tasks.get_mut(task_id) {
            if t.events.len() < MAX_EVENTS_PER_TASK {
                t.events.push(TaskEvent {
                    ts: now_epoch(),
                    stage: stage.to_string(),
                    message: bounded_text(message, 512),
                });
            }
        }
    }

    pub fn prune_tasks(&mut self) {
        while self.tasks.len() > MAX_TASKS {
            // 删最老的已终结任务
            let victim = self
                .tasks
                .iter()
                .filter(|(_, t)| t.status.is_terminal())
                .min_by_key(|(_, t)| t.created_at)
                .map(|(k, _)| k.clone());
            match victim {
                Some(k) => {
                    self.tasks.remove(&k);
                }
                None => break,
            }
        }
    }
}

fn opt_u64(v: &Value, key: &str) -> Option<u64> {
    v.get(key).and_then(|x| x.as_u64())
}

fn opt_str(v: &Value, key: &str) -> Option<String> {
    v.get(key).and_then(|x| x.as_str()).map(|s| s.to_string())
}

fn opt_f64(v: &Value, key: &str) -> Option<f64> {
    v.get(key).and_then(|x| x.as_f64())
}

fn set_opt_u64(v: &mut Value, key: &str, val: Option<u64>) {
    match val {
        Some(n) => v.set(key, n.into()),
        None => v.set(key, Value::Null),
    }
}

fn set_opt_str(v: &mut Value, key: &str, val: &Option<String>) {
    match val {
        Some(s) => v.set(key, s.clone().into()),
        None => v.set(key, Value::Null),
    }
}

fn set_opt_f64(v: &mut Value, key: &str, val: Option<f64>) {
    match val {
        Some(n) => v.set(key, n.into()),
        None => v.set(key, Value::Null),
    }
}

fn model_to_json(m: &ModelState) -> Value {
    let mut v = Value::obj();
    v.set("card", m.card.clone().into());
    v.set("lifecycle_owned", m.lifecycle_owned.into());
    v.set("model_id", m.model_id.clone().into());
    v.set("engine_sha8", m.engine_sha8.clone().into());
    v.set("port", (m.port as u64).into());
    v.set("status", m.status.as_str().into());
    set_opt_str(&mut v, "degraded_reason", &m.degraded_reason);
    v.set("restart_count", m.restart_count.into());
    set_opt_u64(&mut v, "last_restart_at", m.last_restart_at);
    set_opt_u64(&mut v, "last_ready_at", m.last_ready_at);
    set_opt_u64(&mut v, "enabled_at", m.enabled_at);
    v.set("weights_bytes", m.weights_bytes.into());
    set_opt_f64(&mut v, "measured_footprint_gib", m.measured_footprint_gib);
    set_opt_f64(&mut v, "engine_rss_gib", m.engine_rss_gib);
    match m.main_pid {
        Some(p) => v.set("main_pid", (p as u64).into()),
        None => v.set("main_pid", Value::Null),
    }
    v.set("engine_type", m.engine_type.clone().into());
    set_opt_str(&mut v, "container_name", &m.container_name);
    set_opt_str(&mut v, "image", &m.image);
    v
}

fn model_from_json(v: &Value) -> Option<ModelState> {
    Some(ModelState {
        // Legacy observations had no ownership bit. Preserve explicit-enable
        // evidence, never turn a discovery-only record into write authority.
        lifecycle_owned: match v.get("lifecycle_owned") {
            Some(value) => value.as_bool().unwrap_or(false),
            None => opt_u64(v, "enabled_at").is_some(),
        },
        card: opt_str(v, "card")?,
        model_id: opt_str(v, "model_id")?,
        engine_sha8: opt_str(v, "engine_sha8")?,
        port: opt_u64(v, "port")? as u16,
        status: ModelStatus::from_str(
            v.get("status")
                .and_then(|x| x.as_str())
                .unwrap_or("stopped"),
        ),
        degraded_reason: opt_str(v, "degraded_reason"),
        restart_count: opt_u64(v, "restart_count").unwrap_or(0),
        last_restart_at: opt_u64(v, "last_restart_at"),
        last_ready_at: opt_u64(v, "last_ready_at"),
        enabled_at: opt_u64(v, "enabled_at"),
        weights_bytes: opt_u64(v, "weights_bytes").unwrap_or(0),
        measured_footprint_gib: opt_f64(v, "measured_footprint_gib"),
        engine_rss_gib: opt_f64(v, "engine_rss_gib"),
        main_pid: opt_u64(v, "main_pid").map(|p| p as u32),
        // 旧记录没有容器字段：按 native 缺省容错
        engine_type: opt_str(v, "engine_type").unwrap_or_else(|| "native".to_string()),
        container_name: opt_str(v, "container_name"),
        image: opt_str(v, "image"),
    })
}

fn engine_to_json(e: &EngineInfo) -> Value {
    let mut v = Value::obj();
    v.set("sha8", e.sha8.clone().into());
    v.set("engine_type", e.engine_type.clone().into());
    v.set("path", e.path.clone().into());
    v.set("first_seen_at", e.first_seen_at.into());
    v.set("last_seen_at", e.last_seen_at.into());
    v
}

fn engine_from_json(v: &Value) -> Option<EngineInfo> {
    Some(EngineInfo {
        sha8: opt_str(v, "sha8")?,
        engine_type: opt_str(v, "engine_type").unwrap_or_else(|| "native".to_string()),
        path: opt_str(v, "path")?,
        first_seen_at: opt_u64(v, "first_seen_at").unwrap_or(0),
        last_seen_at: opt_u64(v, "last_seen_at").unwrap_or(0),
    })
}

fn external_to_json(e: &ExternalSighting) -> Value {
    let mut v = Value::obj();
    v.set("sha8", e.sha8.clone().into());
    v.set("pid", (e.pid as u64).into());
    v.set("start_time", e.start_time.into());
    match e.port {
        Some(p) => v.set("port", (p as u64).into()),
        None => v.set("port", Value::Null),
    }
    set_opt_str(&mut v, "model_id", &e.model_id);
    v.set("first_seen_at", e.first_seen_at.into());
    v.set("last_seen_at", e.last_seen_at.into());
    v
}

fn external_from_json(v: &Value) -> Option<ExternalSighting> {
    Some(ExternalSighting {
        sha8: opt_str(v, "sha8")?,
        pid: opt_u64(v, "pid")? as u32,
        start_time: opt_u64(v, "start_time").unwrap_or(0),
        port: opt_u64(v, "port").map(|p| p as u16),
        model_id: opt_str(v, "model_id"),
        first_seen_at: opt_u64(v, "first_seen_at").unwrap_or(0),
        last_seen_at: opt_u64(v, "last_seen_at").unwrap_or(0),
    })
}

fn task_to_json(t: &TaskRecord) -> Value {
    let mut v = Value::obj();
    v.set("id", t.id.clone().into());
    v.set("kind", t.kind.clone().into());
    v.set("card", t.card.clone().into());
    v.set("selection", t.selection.clone().unwrap_or(Value::Null));
    v.set("status", t.status.as_str().into());
    v.set("created_at", t.created_at.into());
    set_opt_u64(&mut v, "finished_at", t.finished_at);
    set_opt_str(&mut v, "error", &t.error);
    set_opt_str(&mut v, "result", &t.result);
    set_opt_u64(&mut v, "progress_done_bytes", t.progress_done_bytes);
    set_opt_u64(&mut v, "progress_total_bytes", t.progress_total_bytes);
    set_opt_str(&mut v, "store_run_id", &t.store_run_id);
    v.set(
        "store_context",
        t.store_context
            .as_ref()
            .map(StoreContext::to_json)
            .unwrap_or(Value::Null),
    );
    v.set(
        "events",
        Value::Arr(
            t.events
                .iter()
                .map(|e| {
                    let mut ev = Value::obj();
                    ev.set("ts", e.ts.into());
                    ev.set("stage", e.stage.clone().into());
                    ev.set("message", e.message.clone().into());
                    ev
                })
                .collect(),
        ),
    );
    v
}

fn task_from_json(v: &Value) -> Option<TaskRecord> {
    Some(TaskRecord {
        id: opt_str(v, "id")?,
        kind: opt_str(v, "kind")?,
        card: opt_str(v, "card").unwrap_or_default(),
        selection: v
            .get("selection")
            .filter(|value| value.as_obj().is_some())
            .cloned(),
        status: TaskStatus::from_str(
            v.get("status")
                .and_then(|x| x.as_str())
                .unwrap_or("running"),
        ),
        created_at: opt_u64(v, "created_at").unwrap_or(0),
        finished_at: opt_u64(v, "finished_at"),
        error: opt_str(v, "error"),
        result: opt_str(v, "result"),
        // 旧版 state.json 没有进度字段：缺省 None，向前兼容
        progress_done_bytes: opt_u64(v, "progress_done_bytes"),
        progress_total_bytes: opt_u64(v, "progress_total_bytes"),
        store_run_id: opt_str(v, "store_run_id"),
        store_context: match v.get("store_context") {
            None | Some(Value::Null) => None,
            Some(context) => Some(StoreContext::from_json(context)?),
        },
        events: v
            .get("events")
            .and_then(|x| x.as_arr())
            .map(|arr| {
                arr.iter()
                    .filter_map(|e| {
                        Some(TaskEvent {
                            ts: opt_u64(e, "ts")?,
                            stage: opt_str(e, "stage")?,
                            message: opt_str(e, "message")?,
                        })
                    })
                    .collect()
            })
            .unwrap_or_default(),
    })
}

/// 状态库：内存态 + 落盘（原子写）。
pub struct StateStore {
    path: PathBuf,
    pub state: State,
    dirty: bool,
}

impl StateStore {
    #[cfg(test)]
    pub fn load(path: &Path) -> StateStore {
        Self::try_load(path).expect("valid fixture state")
    }

    /// Missing is an initial installation. Unreadable/corrupt is never an empty
    /// device: retaining the file is essential for ownership and recovery.
    pub fn try_load(path: &Path) -> std::io::Result<StateStore> {
        use std::io::{Error, ErrorKind, Read};
        let state = match std::fs::symlink_metadata(path) {
            Err(e) if e.kind() == ErrorKind::NotFound => State {
                version: 1,
                ..State::default()
            },
            Err(e) => return Err(e),
            Ok(meta) => {
                const MAX_STATE: u64 = 64 * 1024 * 1024;
                if !meta.is_file() || meta.file_type().is_symlink() || meta.len() > MAX_STATE {
                    return Err(Error::new(
                        ErrorKind::InvalidData,
                        "state must be a regular file under 64 MiB",
                    ));
                }
                let mut text = String::new();
                std::fs::File::open(path)?
                    .take(MAX_STATE + 1)
                    .read_to_string(&mut text)?;
                if text.len() as u64 > MAX_STATE {
                    return Err(Error::new(ErrorKind::InvalidData, "state too large"));
                }
                let value =
                    Value::parse(&text).map_err(|e| Error::new(ErrorKind::InvalidData, e))?;
                if value.get("version").and_then(Value::as_u64) != Some(1)
                    || value.get("models").and_then(Value::as_obj).is_none()
                    || value.get("tasks").and_then(Value::as_obj).is_none()
                {
                    return Err(Error::new(
                        ErrorKind::InvalidData,
                        "unsupported or incomplete state schema",
                    ));
                }
                if let Some(epoch) = value.get("lifecycle_revision") {
                    if !matches!(epoch, Value::Num(n) if n.is_finite() && *n >= 0.0 && n.fract() == 0.0 && *n <= crate::lifecycle_revision::MAX_REVISION as f64)
                    {
                        return Err(Error::new(ErrorKind::InvalidData, "invalid control epoch"));
                    }
                }
                // Dropping a malformed running task would silently release its
                // lifecycle exclusion. Reject the state instead of filter_map.
                for (id, task) in value.get("tasks").and_then(Value::as_obj).unwrap() {
                    let parsed = task_from_json(task)
                        .filter(|task| task.id == *id)
                        .ok_or_else(|| {
                            Error::new(
                                ErrorKind::InvalidData,
                                "invalid durable task or store correlation",
                            )
                        })?;
                    if parsed.store_context.is_some()
                        && (parsed.kind != "install"
                            || !parsed.store_run_id.as_deref().is_some_and(safe_store_id))
                    {
                        return Err(Error::new(
                            ErrorKind::InvalidData,
                            "store context missing installation correlation",
                        ));
                    }
                    if parsed
                        .store_context
                        .as_ref()
                        .is_some_and(|c| c.retry.is_some())
                        && parsed.store_run_id.as_deref()
                            != Some(format!("cmsd-{}", parsed.id).as_str())
                    {
                        return Err(Error::new(
                            ErrorKind::InvalidData,
                            "retry run must bind its durable cmsd task",
                        ));
                    }
                }
                State::from_json(&value)
            }
        };
        Ok(StateStore {
            path: path.to_path_buf(),
            state,
            dirty: false,
        })
    }

    pub fn mark_dirty(&mut self) {
        self.dirty = true;
    }

    /// 有改动才写。tmp + rename 原子替换。
    pub fn save_if_dirty(&mut self) -> std::io::Result<()> {
        if !self.dirty {
            return Ok(());
        }
        self.save()
    }

    pub fn save(&mut self) -> std::io::Result<()> {
        use std::io::Write;
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
        if let Some(dir) = self.path.parent() {
            std::fs::create_dir_all(dir)?;
            if std::fs::symlink_metadata(dir)?.file_type().is_symlink() {
                return Err(std::io::Error::other("state directory cannot be a symlink"));
            }
            std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
        }
        if std::fs::symlink_metadata(&self.path)
            .is_ok_and(|m| !m.is_file() || m.file_type().is_symlink())
        {
            return Err(std::io::Error::other("state path must be a regular file"));
        }
        self.state.prune_tasks();
        let text = self.state.to_json().to_string();
        if text.len() > 64 * 1024 * 1024 {
            return Err(std::io::Error::other(
                "state exceeds 64 MiB; previous durable state retained",
            ));
        }
        let stamp = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        let tmp = self.path.with_extension(format!("json.{stamp}.tmp"));
        let result = (|| -> std::io::Result<()> {
            let mut file = std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(&tmp)?;
            file.write_all(text.as_bytes())?;
            file.sync_all()?;
            std::fs::rename(&tmp, &self.path)?;
            std::fs::File::open(self.path.parent().unwrap_or_else(|| Path::new(".")))?.sync_all()
        })();
        if result.is_err() {
            let _ = std::fs::remove_file(&tmp);
        }
        result?;
        self.dirty = false;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn legacy_discovery_does_not_gain_lifecycle_authority() {
        let model = sample_state().models.values().next().unwrap().clone();
        let mut json = model_to_json(&model);
        if let Value::Obj(values) = &mut json {
            values.remove("lifecycle_owned");
        }
        assert!(
            model_from_json(&json).unwrap().lifecycle_owned,
            "explicit enable evidence is compatible"
        );
        json.set("enabled_at", Value::Null);
        assert!(
            !model_from_json(&json).unwrap().lifecycle_owned,
            "legacy discovery stays read-only"
        );
        json.set("lifecycle_owned", Value::Str("invalid".into()));
        assert!(!model_from_json(&json).unwrap().lifecycle_owned);
    }

    fn sample_state() -> State {
        let mut st = State {
            version: 1,
            active_model: Some("ornith15-35b-a3b-nvfp4".to_string()),
            ..State::default()
        };
        st.models.insert(
            "ornith15-35b-a3b-nvfp4".to_string(),
            ModelState {
                lifecycle_owned: true,
                card: "ornith15-35b-a3b-nvfp4".to_string(),
                model_id: "Ornith-1.5-35B-A3B-NVFP4".to_string(),
                engine_sha8: "0f8a4a7c".to_string(),
                port: 18080,
                status: ModelStatus::Running,
                degraded_reason: None,
                restart_count: 2,
                last_restart_at: Some(1791763200),
                last_ready_at: Some(1791763300),
                enabled_at: Some(1791760000),
                weights_bytes: 22892664832,
                measured_footprint_gib: Some(24.6),
                engine_rss_gib: Some(3.1),
                main_pid: Some(4242),
                engine_type: "native".to_string(),
                container_name: None,
                image: None,
            },
        );
        st.engines.insert(
            "0f8a4a7c".to_string(),
            EngineInfo {
                sha8: "0f8a4a7c".to_string(),
                engine_type: "native".to_string(),
                path: "/home/test/.local/share/engines/tachyon-serve/0f8a4a7c".to_string(),
                first_seen_at: 1791760000,
                last_seen_at: 1791763200,
            },
        );
        st.externals.insert(
            "ec14a422:9999".to_string(),
            ExternalSighting {
                sha8: "ec14a422".to_string(),
                pid: 9999,
                start_time: 123456789,
                port: Some(18099),
                model_id: Some("Qwen3.8-27B-NVFP4".to_string()),
                first_seen_at: 1791760000,
                last_seen_at: 1791763200,
            },
        );
        st.ports.insert(18080, "ornith15-35b-a3b-nvfp4".to_string());
        st.tasks.insert(
            "t1".to_string(),
            TaskRecord {
                id: "t1".to_string(),
                kind: "enable".to_string(),
                card: "ornith15-35b-a3b-nvfp4".to_string(),
                status: TaskStatus::Succeeded,
                created_at: 1791760000,
                finished_at: Some(1791760300),
                error: None,
                result: Some("ok".to_string()),
                progress_done_bytes: None,
                progress_total_bytes: None,
                store_run_id: None,
                store_context: None,
                selection: None,
                events: vec![TaskEvent {
                    ts: 1791760001,
                    stage: "precheck".to_string(),
                    message: "检查通过".to_string(),
                }],
            },
        );
        st
    }

    #[test]
    fn state_json_roundtrip() {
        let st = sample_state();
        let text = st.to_json().to_string();
        let parsed = State::from_json(&Value::parse(&text).unwrap());
        assert_eq!(
            parsed.active_model.as_deref(),
            Some("ornith15-35b-a3b-nvfp4")
        );
        let m = parsed.models.get("ornith15-35b-a3b-nvfp4").unwrap();
        assert_eq!(m.port, 18080);
        assert_eq!(m.status, ModelStatus::Running);
        assert_eq!(m.restart_count, 2);
        assert_eq!(m.weights_bytes, 22892664832);
        assert_eq!(m.measured_footprint_gib, Some(24.6));
        assert_eq!(m.main_pid, Some(4242));
        let e = parsed.externals.get("ec14a422:9999").unwrap();
        assert_eq!(e.port, Some(18099));
        assert_eq!(parsed.ports.get(&18080).unwrap(), "ornith15-35b-a3b-nvfp4");
        let t = parsed.tasks.get("t1").unwrap();
        assert_eq!(t.status, TaskStatus::Succeeded);
        assert_eq!(t.events.len(), 1);
    }

    #[test]
    fn corrupt_state_is_preserved_and_refused() {
        let dir = std::env::temp_dir().join(format!("cmsd-store-test-{}", std::process::id()));
        let path = dir.join("state.json");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(&path, "{not json").unwrap();
        assert!(StateStore::try_load(&path).is_err());
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "{not json");
        for invalid in ["{}", "null", r#"{"version":2,"models":{},"tasks":{}}"#] {
            std::fs::write(&path, invalid).unwrap();
            assert!(StateStore::try_load(&path).is_err());
        }
        for invalid in ["null", "-1", "1.5", "9007199254740992", "\"1\""] {
            let text = format!(
                r#"{{"version":1,"models":{{}},"tasks":{{}},"lifecycle_revision":{invalid}}}"#
            );
            std::fs::write(&path, &text).unwrap();
            assert!(StateStore::try_load(&path).is_err());
            assert_eq!(std::fs::read_to_string(&path).unwrap(), text);
        }
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn atomic_save_and_reload() {
        let dir = std::env::temp_dir().join(format!("cmsd-store-test2-{}", std::process::id()));
        let path = dir.join("state.json");
        {
            let mut store = StateStore::load(&path);
            store.state = sample_state();
            store.mark_dirty();
            store.save_if_dirty().unwrap();
        }
        let store = StateStore::load(&path);
        assert_eq!(store.state.models.len(), 1);
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn task_pruning_keeps_bound() {
        let mut st = State::default();
        for i in 0..(MAX_TASKS + 20) {
            st.tasks.insert(
                format!("t{i:03}"),
                TaskRecord {
                    id: format!("t{i:03}"),
                    kind: "enable".to_string(),
                    card: "c".to_string(),
                    status: TaskStatus::Succeeded,
                    created_at: i as u64,
                    finished_at: Some(i as u64 + 1),
                    error: None,
                    result: None,
                    progress_done_bytes: None,
                    progress_total_bytes: None,
                    store_run_id: None,
                    store_context: None,
                    selection: None,
                    events: vec![],
                },
            );
        }
        st.prune_tasks();
        assert_eq!(st.tasks.len(), MAX_TASKS);
        // 最老的被裁掉
        assert!(!st.tasks.contains_key("t000"));
    }

    #[test]
    fn unit_name_roundtrip() {
        let u = unit_name_for("ornith15-35b-a3b-nvfp4");
        assert_eq!(u, "megacube-model@ornith15-35b-a3b-nvfp4.service");
        assert_eq!(
            card_from_unit(&u).as_deref(),
            Some("ornith15-35b-a3b-nvfp4")
        );
        assert_eq!(card_from_unit("other.service"), None);
    }

    #[test]
    fn task_progress_and_cancelled_roundtrip() {
        let mut st = State::default();
        st.tasks.insert(
            "t-install".to_string(),
            TaskRecord {
                id: "t-install".to_string(),
                kind: "install".to_string(),
                card: "qwen38-27b-nvfp4-tachyon".to_string(),
                status: TaskStatus::Cancelled,
                created_at: 1791760000,
                finished_at: Some(1791760300),
                error: None,
                result: Some("已取消，已下载部分保留，重新安装会从断点继续".to_string()),
                progress_done_bytes: Some(1073741824),
                progress_total_bytes: Some(21944459776),
                store_run_id: Some("r-9".to_string()),
                store_context: None,
                selection: None,
                events: vec![],
            },
        );
        let parsed = State::from_json(&Value::parse(&st.to_json().to_string()).unwrap());
        let t = parsed.tasks.get("t-install").unwrap();
        assert_eq!(t.status, TaskStatus::Cancelled);
        assert!(t.status.is_terminal());
        assert_eq!(t.progress_done_bytes, Some(1073741824));
        assert_eq!(t.progress_total_bytes, Some(21944459776));
    }

    #[test]
    fn old_task_json_without_progress_parses() {
        // 旧版 state.json 没有进度字段：缺省容错为 None，不得读不进
        let v = Value::parse(
            r#"{"id":"t1","kind":"enable","card":"c","status":"succeeded","created_at":1}"#,
        )
        .unwrap();
        let t = task_from_json(&v).unwrap();
        assert_eq!(t.status, TaskStatus::Succeeded);
        assert!(t.progress_done_bytes.is_none());
        assert!(t.progress_total_bytes.is_none());
    }

    #[test]
    fn container_model_fields_roundtrip() {
        // W6：容器模型的引擎字段落盘/读回；旧记录（无 engine_type）按 native 容错
        let mut st = State::default();
        st.models.insert(
            "qwen38-27b-sglang0519".to_string(),
            ModelState {
                lifecycle_owned: true,
                card: "qwen38-27b-sglang0519".to_string(),
                model_id: "Qwen3.8-27B-NVFP4".to_string(),
                engine_sha8: String::new(),
                port: 30001,
                status: ModelStatus::Running,
                degraded_reason: None,
                restart_count: 0,
                last_restart_at: None,
                last_ready_at: None,
                enabled_at: None,
                weights_bytes: 23417592488,
                measured_footprint_gib: Some(73.3),
                engine_rss_gib: None,
                main_pid: Some(1365029),
                engine_type: "container".to_string(),
                container_name: Some("qwen38-sglang".to_string()),
                image: Some("lmsysorg/sglang:v0.5.19".to_string()),
            },
        );
        let parsed = State::from_json(&Value::parse(&st.to_json().to_string()).unwrap());
        let m = parsed.models.get("qwen38-27b-sglang0519").unwrap();
        assert_eq!(m.engine_type, "container");
        assert_eq!(m.container_name.as_deref(), Some("qwen38-sglang"));
        assert_eq!(m.image.as_deref(), Some("lmsysorg/sglang:v0.5.19"));
        // 旧格式（手工构造无新字段的 JSON）按 native 读
        let legacy = Value::parse(
            r#"{"card":"c","model_id":"m","engine_sha8":"x","port":1,"status":"stopped"}"#,
        )
        .unwrap();
        let m2 = model_from_json(&legacy).unwrap();
        assert_eq!(m2.engine_type, "native");
        assert!(m2.container_name.is_none());
    }
}
