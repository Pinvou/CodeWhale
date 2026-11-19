//! 共享应用状态：配置、状态库、systemd 后端、实况缓存。

use crate::config::Config;
use crate::engine::{Health, ProcInfo};
use crate::store::{StateStore, TaskRecord, TaskStatus};
use crate::systemd::{Systemd, UnitState};
use crate::timeutil::now_epoch;
use std::collections::{BTreeMap, BTreeSet};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

#[derive(Debug, Clone, Default)]
pub struct LiveState {
    /// 最近一次 /proc 扫描到的驭光进程。
    pub procs: Vec<ProcInfo>,
    /// 端口 → 健康判定结果。
    pub health: BTreeMap<u16, Health>,
    /// Native health belongs to one process generation, never only a port.
    pub health_instances: BTreeMap<u16, (u32, u64)>,
    /// 单元名 → systemd 状态。
    pub units: BTreeMap<String, UnitState>,
    /// W6：容器名 → docker 现实（30s 级扫描缓存；scan 失败保旧值、不做状态收敛）。
    pub containers: BTreeMap<String, crate::docker::ContainerInfo>,
    /// 最近一次容器扫描成功的时刻（0 = 从未成功，对账据此跳过容器判定）。
    pub containers_scanned_at: u64,
    /// W6：显存使用（宿主 pid → MiB，nvidia-smi query-compute-apps）。
    pub gpu_procs: Vec<(u32, u64)>,
    /// W6：容器名 → 容器内进程的宿主 PID 列表（docker top，60s 级刷新）。
    pub container_pids: BTreeMap<String, Vec<u32>>,
    /// W6 手动停判定的驻留计时：卡 → 首次见到「异常退出+自启策略在」的时刻。
    /// 干净退出码（0/143）直接判显式停不需要驻留；非零退出要等 docker 的崩溃
    /// 重拉窗口过去（120s）仍 exited 才改判手动停，避免把崩溃循环误判成手动停。
    pub container_stop_candidates: BTreeMap<String, u64>,
}

impl LiveState {
    /// 某容器的显存占用（MiB）：docker top 的宿主 PID ∩ nvidia-smi 清单求和。
    pub fn gpu_mib_for_container(&self, name: &str) -> u64 {
        let Some(pids) = self.container_pids.get(name) else {
            return 0;
        };
        self.gpu_procs
            .iter()
            .filter(|(pid, _)| pids.contains(pid))
            .map(|(_, mib)| mib)
            .sum()
    }

    /// 某宿主 PID 的显存占用（MiB，原生引擎 main_pid 归属用）。
    pub fn gpu_mib_for_pid(&self, pid: u32) -> u64 {
        self.gpu_procs
            .iter()
            .filter(|(p, _)| *p == pid)
            .map(|(_, mib)| mib)
            .sum()
    }
}

pub struct App {
    pub config: Config,
    pub store: Mutex<StateStore>,
    pub systemd: Systemd,
    pub live: Mutex<LiveState>,
    pub network: crate::network::Network,
    pub started_at: u64,
    task_counter: AtomicU64,
    pub lifecycle_epoch: AtomicU64,
    pub lifecycle_revision_error: Mutex<Option<String>>,
    /// 取消请求登记：cancel 端点写入，install 工作线程每步自查。
    pub cancel_requests: Mutex<BTreeSet<String>>,
    /// W6：docker 后端（CLI 经 config 注入，测试用 fixture 顶替）。
    pub docker: crate::docker::Docker,
    pub connections: std::sync::Arc<crate::limits::Slots>,
    pub event_connections: std::sync::Arc<crate::limits::Slots>,
    pub gateway_connections: std::sync::Arc<crate::limits::Slots>,
    pub gateway_runtime: Mutex<crate::gateway::Runtime>,
    pub gateway_proofs: crate::gateway::ProofCache,
    pub selection_cache: crate::store_adoption::selection::Cache,
    pub store_adoption_requested: std::sync::atomic::AtomicBool,
    pub store_adoption_status: Mutex<crate::json::Value>,
}

/// API 错误：(HTTP 状态码, 错误码, 人话信息)。
pub type ApiErr = (u16, String, String);

pub fn api_err(status: u16, code: &str, message: &str) -> ApiErr {
    (status, code.to_string(), message.to_string())
}

impl App {
    pub fn new(config: Config, store: StateStore, systemd: Systemd) -> App {
        let docker = crate::docker::Docker::new(&config.docker_cli, &config.nvidia_smi_cli);
        let epoch = store.state.lifecycle_revision.unwrap_or(0);
        App {
            config,
            store: Mutex::new(store),
            systemd,
            live: Mutex::new(LiveState::default()),
            network: crate::network::Network::new(),
            started_at: now_epoch(),
            task_counter: AtomicU64::new(0),
            lifecycle_epoch: AtomicU64::new(epoch),
            lifecycle_revision_error: Mutex::new(None),
            cancel_requests: Mutex::new(BTreeSet::new()),
            docker,
            connections: crate::limits::Slots::new(64),
            event_connections: crate::limits::Slots::new(16),
            gateway_connections: crate::limits::Slots::new(64),
            gateway_runtime: Mutex::new(crate::gateway::Runtime::default()),
            gateway_proofs: Default::default(),
            selection_cache: Default::default(),
            store_adoption_requested: std::sync::atomic::AtomicBool::new(false),
            store_adoption_status: Mutex::new(crate::json::Value::Null),
        }
    }

    pub fn uptime_s(&self) -> u64 {
        now_epoch().saturating_sub(self.started_at)
    }

    fn make_task_record(&self, kind: &str, card: &str) -> (String, TaskRecord) {
        let n = self.task_counter.fetch_add(1, Ordering::Relaxed);
        let stamp = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        let id = format!("t-{stamp}-{n}");
        let record = TaskRecord {
            id: id.clone(),
            kind: kind.to_string(),
            card: card.to_string(),
            status: TaskStatus::Running,
            created_at: now_epoch(),
            finished_at: None,
            error: None,
            result: None,
            events: Vec::new(),
            progress_done_bytes: None,
            progress_total_bytes: None,
            store_run_id: None,
            store_context: None,
            selection: None,
        };
        (id, record)
    }

    /// 建任务，返回任务 id。
    #[cfg(test)]
    pub fn task_create(&self, kind: &str, card: &str) -> String {
        let (id, record) = self.make_task_record(kind, card);
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        guard.state.tasks.insert(id.clone(), record);
        guard.mark_dirty();
        let _ = guard.save_if_dirty();
        id
    }

    /// 建 install 任务（P2 互斥纪律：全局同时只允许一个安装——对齐下载并发 1 的
    /// 实测结论；同卡有其它任务在跑也拒）。持锁检查+落记录，并发请求不会都放行。
    /// Atomic admission for lifecycle mutations. Serial startup prevents two
    /// accepted tasks from spending the same MemAvailable before either loads.
    pub fn task_create_exclusive(&self, kind: &str, card: &str) -> Result<String, ApiErr> {
        self.task_create_exclusive_with_maintenance(kind, card, None)
    }

    pub fn task_create_exclusive_with_maintenance(
        &self,
        kind: &str,
        card: &str,
        owner: Option<&str>,
    ) -> Result<String, ApiErr> {
        self.task_create_guarded(kind, card, owner, None, None)
    }

    pub fn task_create_switch(
        &self,
        card: &str,
        expected_previous: Option<&str>,
    ) -> Result<String, ApiErr> {
        self.task_create_guarded("switch", card, None, Some(expected_previous), None)
    }

    pub(crate) fn task_create_maintenance(
        &self,
        kind: &str,
        card: &str,
        owner: &str,
        observation: &crate::model_maintenance::Observation,
    ) -> Result<String, ApiErr> {
        self.task_create_guarded(kind, card, Some(owner), None, Some(observation))
    }

    fn task_create_guarded(
        &self,
        kind: &str,
        card: &str,
        owner: Option<&str>,
        expected_previous: Option<Option<&str>>,
        observation: Option<&crate::model_maintenance::Observation>,
    ) -> Result<String, ApiErr> {
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(observation) = observation {
            observation.verify(self, &guard.state, card)?;
        }
        if expected_previous.is_some_and(|expected| guard.state.active_model.as_deref() != expected)
        {
            return Err(api_err(
                409,
                "precondition_changed",
                "默认模型在预检期间变化，请刷新后重新发起切换",
            ));
        }
        crate::component::check_admission(self)?;
        crate::model_maintenance::check_admission(self, kind, card, owner)?;
        if kind == "switch" {
            if let Some(previous) = &guard.state.active_model {
                crate::model_maintenance::check_admission(self, kind, previous, None)?;
            }
        }
        if let Some(task) = guard.state.tasks.values().find(|task| {
            !task.status.is_terminal() && (task.card == card || task.kind != "install")
        }) {
            return Err(api_err(
                409,
                "task_in_progress",
                &format!("设备变更任务 {} 仍在进行，请先查看进度", task.id),
            ));
        }
        crate::lifecycle_revision::reserve(self, &mut guard)?;
        if kind == "recover" {
            crate::recovery::reserve_attempt(self, card)?;
        }
        if ["disable", "purge"].contains(&kind) {
            crate::recovery::disarm(self, card)?;
        }
        let (id, record) = self.make_task_record(kind, card);
        if let Some(owner) = owner {
            crate::model_maintenance::record_task(self, card, owner, &id, kind)?;
        }
        guard.state.tasks.insert(id.clone(), record);
        guard.mark_dirty();
        if let Err(e) = guard.save_if_dirty() {
            guard.state.tasks.remove(&id);
            guard.mark_dirty();
            if owner.is_some() {
                let _ = crate::model_maintenance::finish_task(self, card, &id, "failed", true);
            }
            return Err(api_err(500, "task_persist_failed", &e.to_string()));
        }
        Ok(id)
    }

    #[cfg(test)]
    pub fn task_create_install(&self, card: &str) -> Result<String, ApiErr> {
        self.task_create_install_context(card, None)
    }

    pub fn task_create_install_context(
        &self,
        card: &str,
        context: Option<crate::store::StoreContext>,
    ) -> Result<String, ApiErr> {
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        crate::component::check_admission(self)?;
        crate::model_maintenance::check_admission(self, "install", card, None)?;
        if let Some(t) = guard
            .state
            .tasks
            .values()
            .find(|t| t.kind == "install" && t.status == TaskStatus::Running)
        {
            return Err(api_err(
                409,
                "install_in_progress",
                &format!("当前有安装任务进行中（{}），请等它结束", t.card),
            ));
        }
        if let Some(t) = guard
            .state
            .tasks
            .values()
            .find(|t| t.card == card && t.status == TaskStatus::Running)
        {
            return Err(api_err(
                409,
                "task_in_progress",
                &format!("卡片 {card} 已有进行中的任务 {}，请等它结束", t.id),
            ));
        }
        crate::lifecycle_revision::reserve(self, &mut guard)?;
        let (id, mut record) = self.make_task_record("install", card);
        if let Some(context) = context {
            record.store_run_id = Some(format!("cmsd-{id}"));
            record.store_context = Some(context);
        }
        guard.state.tasks.insert(id.clone(), record);
        guard.mark_dirty();
        if let Err(e) = guard.save_if_dirty() {
            guard.state.tasks.remove(&id);
            guard.mark_dirty();
            return Err(api_err(500, "task_persist_failed", &e.to_string()));
        }
        Ok(id)
    }

    pub fn task_event(&self, task_id: &str, stage: &str, message: &str) {
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        guard.state.task_event(task_id, stage, message);
        guard.mark_dirty();
        // 事件不落盘也行，但落盘让 SSE 断线重连能补全；存盘失败不阻塞任务
        let _ = guard.save_if_dirty();
    }

    pub fn task_finish(&self, task_id: &str, outcome: Result<String, String>) {
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        let mut receipt = None;
        if let Some(t) = guard.state.tasks.get_mut(task_id) {
            t.status = match &outcome {
                Ok(_) => TaskStatus::Succeeded,
                Err(_) => TaskStatus::Failed,
            };
            t.finished_at = Some(now_epoch());
            match outcome {
                Ok(msg) => t.result = Some(crate::store::bounded_text(&msg, 4096)),
                Err(msg) => t.error = Some(crate::store::bounded_text(&msg, 4096)),
            }
            receipt = Some((t.card.clone(), t.status.as_str()));
        }
        if let Some((card, status)) = receipt {
            if let Err((_, code, message)) = crate::model_maintenance::finish_task(
                self,
                &card,
                task_id,
                status,
                status == "failed",
            ) {
                log_line(&format!("维护回执持久化失败：{code}: {message}"));
            }
        }
        guard.mark_dirty();
        let _ = guard.save_if_dirty();
    }

    /// 同卡是否有进行中的任务（并发护栏）。
    pub fn task_running_for(&self, card: &str) -> Option<String> {
        let guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .tasks
            .values()
            .find(|t| t.card == card && t.status == TaskStatus::Running)
            .map(|t| t.id.clone())
    }

    /// 进度字段更新（P2 install）：只改内存标脏，落盘由同一行的任务事件顺带
    /// 完成——下载 2s 一行进度，每次都写盘太浪费，崩溃丢几秒进度无所谓。
    pub fn task_progress(&self, task_id: &str, done: u64, total: u64) {
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(t) = guard.state.tasks.get_mut(task_id) {
            t.progress_done_bytes = Some(done);
            t.progress_total_bytes = Some(total);
            guard.mark_dirty();
        }
    }

    /// 委托门面：登记商店侧 run_id（install 委托品悟商店执行的关联键）。
    pub fn task_set_store_run(&self, task_id: &str, run_id: &str) -> Result<(), ApiErr> {
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(t) = guard.state.tasks.get_mut(task_id) {
            t.store_run_id = Some(run_id.to_string());
            guard.mark_dirty();
        }
        guard
            .save_if_dirty()
            .map_err(|e| api_err(500, "task_persist_failed", &e.to_string()))?;
        Ok(())
    }

    pub fn task_request_store_cancel(&self, task_id: &str) -> Result<(), ApiErr> {
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(context) = guard
            .state
            .tasks
            .get_mut(task_id)
            .and_then(|t| t.store_context.as_mut())
        {
            context.cancel_requested = true;
            guard.mark_dirty();
        }
        guard
            .save_if_dirty()
            .map_err(|e| api_err(500, "task_persist_failed", &e.to_string()))
    }

    /// 取消终态（P2 install）：status=cancelled + result 续传提示（error 留给真失败）。
    pub fn task_cancel(&self, task_id: &str, message: &str) {
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(t) = guard.state.tasks.get_mut(task_id) {
            t.status = TaskStatus::Cancelled;
            t.finished_at = Some(now_epoch());
            t.result = Some(message.to_string());
        }
        guard.mark_dirty();
        let _ = guard.save_if_dirty();
    }

    pub fn cancel_mark(&self, task_id: &str) {
        let mut set = self
            .cancel_requests
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        set.insert(task_id.to_string());
    }

    pub fn cancel_requested(&self, task_id: &str) -> bool {
        let set = self
            .cancel_requests
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        set.contains(task_id)
    }

    pub fn save_store(&self) {
        let mut guard = self.store.lock().unwrap_or_else(|e| e.into_inner());
        let _ = guard.save_if_dirty();
    }
}

pub fn log_line(msg: &str) {
    eprintln!("[{}] {msg}", crate::timeutil::now_iso8601());
}
